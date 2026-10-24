//go:build cgo && !windows

package gitutil

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// treeScanFoldFixture adds to the edge fixture the characters Unicode case
// folding ties to ASCII letters or that a search's Unicode variants use: the
// Kelvin sign, the long s, the dotted and dotless i, and Latin-1 bytes.
func treeScanFoldFixture(t *testing.T) string {
	t.Helper()
	repo := treeScanEdgeFixture(t)
	files := map[string]string{
		"kelvin.txt":     "temperature in Kelvin\n",
		"long-s.txt":     "poſition paper\n",
		"dotted-i.txt":   "İndex of terms\n",
		"dotless-i.txt":  "ındex here\n",
		"latin1.txt":     "caf\xe9 SESSION\n",
		"mixed.txt":      "plain session but café\n",
		"ascii-only.txt": "Session Token VALIDATE\n",
		"specials.txt":   "c++ and a.b and [x] and $y\n",
		"nothing.txt":    "no match at all é\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "folds")
	return repo
}

// The path scan answers exactly what git grep -I -i -F -l answers, in a
// UTF-8 locale and in C, for ASCII patterns, for the Unicode variants a
// search adds, for git's special bytes, and from remembered answers.
func TestScanTreePathsIsGitGrepPaths(t *testing.T) {
	repo := treeScanFoldFixture(t)
	patternSets := map[string][]string{
		"ascii":    {"foo", "session", "kelvin", "position", "index", "VALIDATE"},
		"variants": {"Kelvin", "keKin", "İndex", "posİtion", "kelvin"},
		"specials": {"c++", "a.b", "[x]", "$y", "dup"},
		"none":     {"zzzq"},
		"empty":    {"", "foo"},
		"repeats":  {"foo", "foo", "FOO"},
		// Lists only git answers exactly: a pattern git's \Q...\E quoting
		// ends early, and a variant without its ASCII form.
		"quote end":     {`foo\Ebar`, "foo"},
		"variant alone": {"\u212aelvin", "\u0130ndex"},
	}
	for _, locale := range []string{"en_US.UTF-8", "C", "en_US.ISO8859-1"} {
		t.Setenv("LC_ALL", locale)
		store := OpenTreeScanAnswers(t.TempDir())
		for name, patterns := range patternSets {
			want, err := GrepTreePaths(t.Context(), repo, "HEAD", patterns)
			if err != nil {
				t.Fatal(err)
			}
			for round := 0; round < 2; round++ {
				got, stats, err := ScanTreePaths(t.Context(), repo, "HEAD", patterns, TreeScanOptions{Answers: store})
				if err != nil {
					t.Fatal(err)
				}
				if len(got) == 0 && len(want) == 0 {
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s, %s, round %d (%s, %d deferred):\n got %q\nwant %q", locale, name, round, stats.Route, stats.Deferred, got, want)
				}
				if round == 1 && stats.Route == TreeScanRoutePaths && stats.BlobsRead != 0 {
					t.Fatalf("%s, %s: a warm store still read %d blobs", locale, name, stats.BlobsRead)
				}
			}
		}
	}
}

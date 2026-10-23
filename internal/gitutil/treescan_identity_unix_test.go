//go:build cgo && !windows

package gitutil

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// treeScanEdgeFixture commits the cases where Graph's one-pass scan could
// part from git grep: line endings, empty and final lines, NUL bytes before
// and after git's 8000-byte binary probe, every attribute route to "binary",
// one blob at two paths with different attributes, symlinks, invalid UTF-8,
// non-ASCII letters under POSIX classes, and git's fixed-string patterns.
func treeScanEdgeFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	git(t, repo, "config", "diff.bindrv.binary", "true")
	git(t, repo, "config", "diff.autodrv.binary", "auto")
	git(t, repo, "config", "diff.textdrv.binary", "false")
	files := map[string]string{
		".gitattributes":   "attr-binary.txt binary\nattr-text.bin diff\n*.dat diff=bindrv\n*.cfg diff=autodrv\n*.raw diff=textdrv\ndir/dup2.txt -diff\n",
		"crlf.txt":         "x\r\nfoo bar\r\n",
		"nonl.txt":         "zz\nfoo",
		"empty.txt":        "",
		"blank.txt":        "\n\n\nfoo\n",
		"late-nul.txt":     strings.Repeat("a", 9000) + "\x00 foo\n",
		"early-nul.bin":    "\x00foo\n",
		"attr-binary.txt":  "foo\n",
		"attr-text.bin":    "\x00foo\n",
		"driver.dat":       "foo\n",
		"driver-auto.cfg":  "\x00foo\n",
		"driver-text.raw":  "\x00foo\n",
		"dup1.txt":         "foo dup\n",
		"dir/dup2.txt":     "foo dup\n",
		"invalid.txt":      "\xff foo\n\xc3bar\nfoo ok\n",
		"invalid-only.txt": "\xfffoo\n",
		"accents.txt":      "éfoo\nÉBAR\ncafé bar\nKelvin\n",
		"digits.txt":       "a 2026 b\n",
		"case.txt":         "FOO\nfOo\n",
		"word.txt":         "foobar\nx foo y\n",
		"repeat.txt":       "fooo\nxxx\nabc\n",
		"span.txt":         "a\nzz\n",
		"tail-invalid.txt": "ok\n\xff\n",
		"late.txt":         strings.Repeat("filler line\n", 500) + "the foo at last\n",
	}
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("foo-target", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	return repo
}

var treeScanEdgePatternSets = map[string][]string{
	"literal and class": {
		"[fF][oO][oO]",
		"(^|[^[:alnum:]])bar($|[^[:alnum:]])",
		"[[:upper:]]BAR",
		"2026",
		"(k|K|K)elvin",
	},
	"alias boundaries": {
		"(^|[^[:alnum:]])([fF][oO][oO])($|[^[:alnum:]])",
		"[[:lower:][:digit:]]([fF])oo",
		"([fF][oO][oO])[[:upper:]]",
	},
	"engine syntax": {
		"\\<foo\\>",
		"fo+",
		"x{2,}",
		"a.c",
		"[^a]foo",
		"ok$",
	},
	"fixed only":         {"foo", "dup", "2026"},
	"matches every line": {"()", "zz[q]"},
	// An expression matching the empty string lets git's look-ahead report
	// the empty tail after a file's final LF when no line matched.
	"matches the empty tail": {"^$", "zz[q]"},
	"empty or spanning":      {"^x?$", "a[[:space:]]+zz", "[^[:print:]]"},
	// git reads `-f -` line by line: a pattern holding LF is two, a trailing
	// CR is dropped and an empty line is ignored.
	"pattern file lines": {"zz[q]\r", "", "dup\nFOO", "[^a]foo\r\n2026"},
}

// treeScanDriverFixture covers every route git's userdiff takes to a binary
// verdict that the edge fixture leaves out: the "default" driver's own
// setting (for unattributed paths and for names no driver has), built-in and
// configured drivers without a binary setting, attribute values spelled like
// the states ("set", "unset"), and git's integer booleans.
func treeScanDriverFixture(t *testing.T, defaultBinary string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	if defaultBinary != "" {
		git(t, repo, "config", "diff.default.binary", defaultBinary)
	}
	git(t, repo, "config", "diff.cfgdrv.funcname", "^func")
	git(t, repo, "config", "diff.sfx.binary", "1k")
	git(t, repo, "config", "diff.zero.binary", "0")
	git(t, repo, "config", "diff.unspecified.binary", "true")
	files := map[string]string{
		".gitattributes": "*.x diff=nosuch\n*.go diff=golang\n*.cfgd diff=cfgdrv\n*.ls diff=set\n*.lu diff=unset\n*.sfx diff=sfx\n*.zero diff=zero\n*.forced diff\n*.un diff=unspecified\n",
		"plain.txt":      "foo\n",
		"nul.txt":        "\x00foo\n",
		"other.x":        "foo\n",
		"nul.x":          "\x00foo\n",
		"code.go":        "foo\n",
		"nul.go":         "\x00foo\n",
		"conf.cfgd":      "foo\n",
		"literal.ls":     "\x00foo\n",
		"text.ls":        "foo\n",
		"literal.lu":     "foo\n",
		"suffix.sfx":     "foo\n",
		"zero.zero":      "\x00foo\n",
		"nul.forced":     "\x00foo\n",
		"literal.un":     "foo\n",
		"plain.un.txt":   "foo\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "drivers")
	return repo
}

// The one-pass scan must answer exactly what one multi-pattern
// `git grep -m 1 -f -` answers (grepPatternLines is that command), in every
// locale a git child may run under, and so must the per-pattern Git route.
func TestScanTreePatternLinesIsGitGrepByteForByte(t *testing.T) {
	drivers := map[string][]string{"drivers": {"foo", "fo+"}}
	fixtures := map[string]struct {
		repo     string
		patterns map[string][]string
	}{
		"edges":          {treeScanEdgeFixture(t), treeScanEdgePatternSets},
		"drivers":        {treeScanDriverFixture(t, ""), drivers},
		"default binary": {treeScanDriverFixture(t, "true"), drivers},
		"default text":   {treeScanDriverFixture(t, "false"), drivers},
		"default auto":   {treeScanDriverFixture(t, "auto"), drivers},
	}
	for _, locale := range []string{"C", "en_US.UTF-8", "C.UTF-8"} {
		t.Run(locale, func(t *testing.T) {
			t.Setenv("LC_ALL", locale)
			for fixtureName, fixture := range fixtures {
				repo := fixture.repo
				for setName, patterns := range fixture.patterns {
					t.Run(fixtureName+"/"+setName, func(t *testing.T) {
						var reference []GrepMatch
						if err := grepPatternLines(t.Context(), repo, "HEAD", patterns, 1, func(match GrepMatch) error {
							reference = append(reference, match)
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						var got []GrepMatch
						stats, err := ScanTreePatternLines(t.Context(), repo, "HEAD", patterns, func(match GrepMatch) error {
							got = append(got, match)
							return nil
						})
						if err != nil {
							t.Fatal(err)
						}
						if stats.Route != TreeScanRouteOnePass {
							t.Fatalf("route = %q", stats.Route)
						}
						if !reflect.DeepEqual(got, reference) {
							t.Fatalf("one-pass scan differs from git grep:\n got %q\nwant %q", got, reference)
						}
						var viaGit []GrepMatch
						if err := grepTreeFirstPatternLinesGit(t.Context(), repo, "HEAD", patterns, func(match GrepMatch) error {
							viaGit = append(viaGit, match)
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(viaGit, reference) {
							t.Fatalf("Git route differs from git grep:\n got %q\nwant %q", viaGit, reference)
						}
					})
				}
			}
		})
	}
}

// git refuses a pattern file with no pattern in it, and so does the scan; no
// patterns at all start no git grep and match nothing.
func TestScanTreePatternLinesEmptyPatternLists(t *testing.T) {
	repo := treeFirstLineFixture(t)
	if _, err := ScanTreePatternLines(t.Context(), repo, "HEAD", []string{"", "\r"}, func(GrepMatch) error { return nil }); err == nil {
		t.Fatal("a pattern list of empty lines was accepted; git grep refuses it")
	}
	calls := 0
	if _, err := ScanTreePatternLines(t.Context(), repo, "HEAD", nil, func(GrepMatch) error { calls++; return nil }); err != nil || calls != 0 {
		t.Fatalf("no patterns = %v after %d visits, want nil after 0", err, calls)
	}
}

// Each distinct blob is read once, however many patterns and paths share it,
// and a blob every path of which is binary by attribute is not read at all.
func TestScanTreePatternLinesReadsEachBlobOnce(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	patterns := treeScanEdgePatternSets["literal and class"]
	stats, err := ScanTreePatternLines(t.Context(), repo, "HEAD", append(patterns, patterns...), func(GrepMatch) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// 24 regular files hold 19 distinct blobs. "foo\n" is held only by
	// attr-binary.txt and driver.dat, both binary by attribute, so 18 are read.
	if stats.FilesExamined != 24 || stats.BlobsRead != 18 || stats.Patterns != len(patterns) {
		t.Fatalf("stats = %+v, want 24 files examined, 18 blobs read, %d patterns", stats, len(patterns))
	}
}

// A pattern git refuses is an error before any blob is read, as git grep's
// own compile failure is.
func TestScanTreePatternLinesRefusesWhatGitRefuses(t *testing.T) {
	repo := treeFirstLineFixture(t)
	_, err := ScanTreePatternLines(t.Context(), repo, "HEAD", []string{"[fF]oo", "(unclosed"}, func(GrepMatch) error { return nil })
	if err == nil {
		t.Fatal("an expression git grep cannot compile was accepted")
	}
}

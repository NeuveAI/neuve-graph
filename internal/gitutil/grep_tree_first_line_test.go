package gitutil

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// treeFirstLineFixture commits files that separate "first line matching ANY
// pattern" from "first line of the first pattern": patterns hit at different
// lines, two patterns share a line, one file matches none, one is binary, and
// paths whose byte order differs from a per-directory walk ("a-b" < "a/").
func treeFirstLineFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	files := map[string]string{
		"a.txt":      "nothing here\nbeta second\nalpha third\n",
		"a-b.txt":    "alpha first\nbeta second\n",
		"a/b.txt":    "x\ny\ngamma third\n",
		"a/c d.txt":  "alpha and beta on one line\n",
		"none.txt":   "no pattern at all\n",
		"tail.txt":   "first\nlast line beta",
		"binary.dat": "beta\x00alpha\n",
		"many.txt":   strings.Repeat("alpha again\n", 300) + "beta late\n" + strings.Repeat("filler\n", 300),
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
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture")
	return repo
}

var treeFirstLinePatterns = []string{
	"[aA]lpha",
	"[bB]eta",
	"zz[q]",
	"[bB]eta",
	"(^|[^[:alnum:]])gam+a($|[^[:alnum:]])",
}

// The per-pattern route must answer exactly what one multi-pattern
// `git grep -m 1 -f -` answers (grepPatternLines is that original command),
// in the same order.
func TestGrepTreePatternLinesMatchesOneMultiPatternScan(t *testing.T) {
	repo := treeFirstLineFixture(t)
	var reference []GrepMatch
	if err := grepPatternLines(t.Context(), repo, "HEAD", treeFirstLinePatterns, 1, func(match GrepMatch) error {
		reference = append(reference, match)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var got []GrepMatch
	if err := GrepTreePatternLines(t.Context(), repo, "HEAD", treeFirstLinePatterns, func(match GrepMatch) error {
		got = append(got, match)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []GrepMatch{
		{Path: "a-b.txt", Text: "alpha first"},
		{Path: "a.txt", Text: "beta second"},
		{Path: "a/b.txt", Text: "gamma third"},
		{Path: "a/c d.txt", Text: "alpha and beta on one line"},
		{Path: "many.txt", Text: "alpha again"},
		{Path: "tail.txt", Text: "last line beta"},
	}
	if !reflect.DeepEqual(reference, want) {
		t.Fatalf("fixture no longer exercises the cases it names; one multi-pattern scan = %#v", reference)
	}
	if !reflect.DeepEqual(got, reference) {
		t.Fatalf("per-pattern merge differs from one multi-pattern scan:\n got %#v\nwant %#v", got, reference)
	}
}

func TestGrepTreePatternLinesNoMatchAndVisitError(t *testing.T) {
	repo := treeFirstLineFixture(t)
	calls := 0
	if err := GrepTreePatternLines(t.Context(), repo, "HEAD", []string{"zz[q]", "yy[q]"}, func(GrepMatch) error {
		calls++
		return nil
	}); err != nil || calls != 0 {
		t.Fatalf("no-match scan = %v after %d visits, want nil after 0", err, calls)
	}
	stop := os.ErrClosed
	calls = 0
	err := GrepTreePatternLines(t.Context(), repo, "HEAD", treeFirstLinePatterns, func(GrepMatch) error {
		calls++
		return stop
	})
	if err != stop || calls != 1 {
		t.Fatalf("visit error = %v after %d visits, want the visitor's error after 1", err, calls)
	}
}

// The captured stall: every line of a file hits one pattern while the others
// never match. One multi-pattern `git grep -m 1` re-scans the absent patterns
// to the end of the file after every hit, so its time grows with the square of
// the file (measured 0.4 s, 1.7 s, 6.9 s at 2,000, 4,000 and 8,000 lines). The
// per-pattern route is linear: a few hundredths of a second here.
func TestGrepTreePatternLinesStaysLinearWhenOnePatternHitsEveryLine(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	var content strings.Builder
	for line := 0; line < 8000; line++ {
		content.WriteString("alpha line with some filler text\n")
	}
	if err := os.WriteFile(filepath.Join(repo, "hits.txt"), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "hits")
	started := time.Now()
	var got []GrepMatch
	err := GrepTreePatternLines(t.Context(), repo, "HEAD", []string{"[aA]lpha", "zz[q]", "yy[q]", "xx[q]"}, func(match GrepMatch) error {
		got = append(got, match)
		return nil
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (GrepMatch{Path: "hits.txt", Text: "alpha line with some filler text"}) {
		t.Fatalf("matches = %#v", got)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("first-line scan took %s; the per-pattern route is linear and needs well under a second", elapsed)
	}
}

// Whatever the budget, and so whatever order the scans finish in, the merged
// answer is the one multi-pattern scan's.
func TestGrepTreePatternLinesIsIdenticalAtEveryBudget(t *testing.T) {
	repo := treeFirstLineFixture(t)
	var reference []GrepMatch
	if err := grepPatternLines(t.Context(), repo, "HEAD", treeFirstLinePatterns, 1, func(match GrepMatch) error {
		reference = append(reference, match)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []struct{ cpu, threads string }{{"1", "1"}, {"2", "1"}, {"4", "2"}, {"6", "1"}, {"8", "4"}} {
		t.Setenv(treePatternGrepCPUEnv, budget.cpu)
		t.Setenv(treePatternGrepThreadsEnv, budget.threads)
		var got []GrepMatch
		if err := GrepTreePatternLines(t.Context(), repo, "HEAD", treeFirstLinePatterns, func(match GrepMatch) error {
			got = append(got, match)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, reference) {
			t.Fatalf("budget cpu=%s threads=%s: got %#v, want %#v", budget.cpu, budget.threads, got, reference)
		}
	}
}

func TestResolveTreePatternGrepBudget(t *testing.T) {
	for _, test := range []struct {
		name                 string
		cpu, threads         string
		performance, maxProc int
		want                 TreePatternGrepBudget
		workers              int
	}{
		{"half the performance cores", "", "", 8, 12, TreePatternGrepBudget{CPU: 4, Threads: 1}, 4},
		{"GOMAXPROCS caps the default", "", "", 8, 2, TreePatternGrepBudget{CPU: 2, Threads: 1}, 2},
		{"never below one", "", "", 1, 1, TreePatternGrepBudget{CPU: 1, Threads: 1}, 1},
		{"settings override", "6", "2", 8, 2, TreePatternGrepBudget{CPU: 6, Threads: 2}, 3},
		{"threads never exceed the budget", "2", "4", 8, 12, TreePatternGrepBudget{CPU: 2, Threads: 2}, 1},
		{"invalid settings are ignored", "zero", "-3", 8, 12, TreePatternGrepBudget{CPU: 4, Threads: 1}, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := resolveTreePatternGrepBudget(test.cpu, test.threads, test.performance, test.maxProc)
			if got != test.want || got.workers() != test.workers {
				t.Fatalf("got %+v with %d workers, want %+v with %d", got, got.workers(), test.want, test.workers)
			}
		})
	}
}

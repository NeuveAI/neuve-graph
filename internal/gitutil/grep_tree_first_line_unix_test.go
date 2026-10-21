//go:build !windows

package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// installGitShim puts a `git` first on PATH that runs body for `git grep`
// invocations and hands every other command to the real Git.
func installGitShim(t *testing.T, body string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	dir := t.TempDir()
	logDir := t.TempDir()
	script := "#!/bin/sh\nLOG=" + logDir + "\nREAL=" + realGit + "\n" +
		"if [ \"$1\" = grep ]; then\n" + body + "\nfi\nexec \"$REAL\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logDir
}

// One `git grep -m 1` with several patterns is quadratic in Git (see
// grepTreeFirstPatternLines), so a committed-tree first-line scan must give
// each Git process exactly one pattern.
func TestGrepTreePatternLinesRunsOnePatternPerGitProcess(t *testing.T) {
	repo := treeFirstLineFixture(t)
	logDir := installGitShim(t, `tee "$LOG/$$.patterns" | "$REAL" "$@"; exit $?`)
	if err := GrepTreePatternLines(t.Context(), repo, "HEAD", treeFirstLinePatterns, func(GrepMatch) error { return nil }); err != nil {
		t.Fatal(err)
	}
	logs, err := filepath.Glob(filepath.Join(logDir, "*.patterns"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 4 {
		t.Fatalf("ran %d git grep processes, want one per distinct pattern (4)", len(logs))
	}
	for _, log := range logs {
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Count(string(data), "\n"); lines != 1 {
			t.Fatalf("one git grep received %d patterns, want 1:\n%s", lines, data)
		}
	}
}

// The per-pattern scans share one CPU budget: no more than budget/threads Git
// processes at once, each told its thread cap, and more than one at once when
// the budget allows it (never strictly one after another).
func TestGrepTreePatternLinesStaysWithinItsBudget(t *testing.T) {
	repo := treeFirstLineFixture(t)
	t.Setenv(treePatternGrepCPUEnv, "6")
	t.Setenv(treePatternGrepThreadsEnv, "2")
	// Each scan registers itself, records how many scans were running, holds
	// long enough to overlap, then leaves. mkdir is the atomic step.
	logDir := installGitShim(t, `mkdir "$LOG/run.$$"; ls -d "$LOG"/run.* | wc -l | tr -d ' ' > "$LOG/count.$$"; printf '%s\n' "$*" > "$LOG/args.$$"; sleep 0.3; "$REAL" "$@"; rc=$?; rmdir "$LOG/run.$$"; exit $rc`)
	patterns := []string{"[aA]lpha", "[bB]eta", "zz[q]", "yy[q]", "xx[q]", "ww[q]", "vv[q]", "uu[q]", "tt[q]"}
	if err := GrepTreePatternLines(t.Context(), repo, "HEAD", patterns, func(GrepMatch) error { return nil }); err != nil {
		t.Fatal(err)
	}
	counts, err := filepath.Glob(filepath.Join(logDir, "count.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != len(patterns) {
		t.Fatalf("ran %d scans, want %d", len(counts), len(patterns))
	}
	peak := 0
	for _, path := range counts {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		running, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		peak = max(peak, running)
	}
	if peak > 3 {
		t.Fatalf("%d scans ran at once; a budget of 6 at 2 threads allows 3", peak)
	}
	if peak < 2 {
		t.Fatalf("scans never overlapped (peak %d); the pool ran them one after another", peak)
	}
	args, err := filepath.Glob(filepath.Join(logDir, "args.*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range args {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(" "+string(data), " --threads 2 ") {
			t.Fatalf("a scan was not capped at 2 Git threads: %s", data)
		}
	}
}

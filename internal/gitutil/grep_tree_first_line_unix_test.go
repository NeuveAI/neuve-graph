//go:build !windows

package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
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

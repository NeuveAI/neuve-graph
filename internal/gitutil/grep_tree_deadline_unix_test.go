//go:build !windows

package gitutil

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A scan that outlives TreePatternGrepDeadline is killed and reported as a
// typed deadline error rather than waited for.
func TestGrepTreePatternLinesStopsAtDeadline(t *testing.T) {
	repo := treeFirstLineFixture(t)
	logDir := installGitShim(t, `echo $$ > "$LOG/pid"; exec sleep 60`)
	previous := TreePatternGrepDeadline
	TreePatternGrepDeadline = 300 * time.Millisecond
	t.Cleanup(func() { TreePatternGrepDeadline = previous })

	err := GrepTreePatternLines(t.Context(), repo, "HEAD", treeFirstLinePatterns, func(GrepMatch) error { return nil })
	var deadline *GrepDeadlineError
	if !errors.As(err, &deadline) || !errors.Is(err, ErrGrepDeadline) || deadline.Limit != TreePatternGrepDeadline {
		t.Fatalf("err = %v, want a *GrepDeadlineError for %s", err, TreePatternGrepDeadline)
	}
	data, readErr := os.ReadFile(filepath.Join(logDir, "pid"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil {
		t.Fatal(convErr)
	}
	if processAlive(pid) {
		t.Fatalf("git grep process %d still running after its deadline", pid)
	}
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

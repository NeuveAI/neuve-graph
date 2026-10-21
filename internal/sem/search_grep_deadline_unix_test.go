//go:build !windows

package sem

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-graph/internal/gitutil"
)

// A committed-tree query whose alias grep is stopped at its deadline must still
// answer, and must say that its file selection took the fallback route.
func TestPreparedSearchDisclosesStoppedAliasGrep(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth/session.go", "package auth\n// auth session token\nfunc NewAuthSession() int { return 1 }\n")
	for index := 0; index < 20; index++ {
		write(t, repo, fmt.Sprintf("noise/file_%02d.go", index), fmt.Sprintf("package noise\nfunc Noise%d() int { return %d }\n", index, index))
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	cacheDir := t.TempDir()
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cacheDir); err != nil {
		t.Fatal(err)
	}
	const queryText = "authentication session"
	if len(buildSearchQuery(queryText).inferredAbbreviations) == 0 {
		t.Fatal("query no longer infers an abbreviation, so it would not reach the alias grep")
	}
	search := func() SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", queryText, SearchOptions{
			Profile: ProfileSyntaxOnly, TopK: 10, MaxIndexedFiles: 1, CacheDir: cacheDir, PreparedOnly: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	hasDeadlineWarning := func(response SearchResponse) bool {
		for _, warning := range response.Warnings {
			if warning.Code == searchGitGrepDeadlineCode {
				return true
			}
		}
		return false
	}

	completed := search()
	if hasDeadlineWarning(completed) || completed.Stats.PreselectionBackend != "git-tree-grep" {
		t.Fatalf("a completed scan was disclosed as stopped or left the Git route: %#v %#v", completed.Warnings, completed.Stats)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	shim := t.TempDir()
	// Only the alias scan (-E -m 1 -f -) hangs; every other Git command is real.
	script := "#!/bin/sh\nif [ \"$1\" = grep ]; then case \" $* \" in *\" -E -m 1 -f - \"*) exec sleep 60;; esac; fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous := gitutil.TreePatternGrepDeadline
	gitutil.TreePatternGrepDeadline = 300 * time.Millisecond
	t.Cleanup(func() { gitutil.TreePatternGrepDeadline = previous })

	started := time.Now()
	stopped := search()
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("stopped scan still took %s", elapsed)
	}
	if !hasDeadlineWarning(stopped) {
		t.Fatalf("stopped alias grep was not disclosed: %#v", stopped.Warnings)
	}
	if stopped.Stats.PreselectionBackend == "git-tree-grep" {
		t.Fatalf("stopped scan still reported the Git route: %#v", stopped.Stats)
	}
}

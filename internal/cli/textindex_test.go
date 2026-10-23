//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The commit-time build indexes a commit's blobs once, reports the tree's
// index bytes in a versioned JSON receipt, indexes nothing the second time,
// and refuses anything but an exact commit.
func TestTextIndexBuildsOnceAndReportsTheTreesBytes(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth.go", "package auth\n\nfunc ValidateToken(token string) bool { return token != \"\" }\n")
	write(t, repo, "README.md", "Authentication helpers.\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(head))
	cacheDir := t.TempDir()
	run := func(args ...string) (textIndexResponse, error) {
		t.Helper()
		var output bytes.Buffer
		err := Run(t.Context(), Options{Version: "test-version", Env: EntireEnv{RepoRoot: repo}, Stdout: &output}, append([]string{"text-index"}, args...))
		var response textIndexResponse
		if err == nil {
			if decodeErr := json.Unmarshal(output.Bytes(), &response); decodeErr != nil {
				t.Fatalf("decode text-index receipt %q: %v", output.String(), decodeErr)
			}
		}
		return response, err
	}
	first, err := run("--repo", repo, "--commit", commit, "--cache-dir", cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if first.FormatVersion != 1 || first.Commit != commit || first.TextIndex.BlobsIndexed != 2 || first.TextIndex.TreeIndexBytes == 0 || first.TextIndex.TreeBlobs != 2 {
		t.Fatalf("first receipt = %+v", first)
	}
	second, err := run("--repo", repo, "--commit", commit, "--cache-dir", cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if second.TextIndex.BlobsIndexed != 0 || second.TextIndex.TreeIndexBytes != first.TextIndex.TreeIndexBytes {
		t.Fatalf("second receipt = %+v, want nothing indexed again", second)
	}
	if _, err := run("--repo", repo, "--commit", "HEAD", "--cache-dir", cacheDir); err == nil {
		t.Fatal("a moving revision was accepted")
	}
}

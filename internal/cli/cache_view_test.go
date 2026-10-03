package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCachedNeighborsAndQueryReadFreshViewAfterOldViewRemoved(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "view")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Cache Test")
	git(t, repo, "config", "user.email", "cache@example.com")
	write(t, repo, "calls.go", "package sample\n\nfunc Callee() {}\nfunc Caller() {\n Callee()\n}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "source")
	old := filepath.Join(t.TempDir(), "view")
	git(t, repo, "worktree", "add", "--detach", old, "HEAD")
	cache := t.TempDir()
	run := func(args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := Run(t.Context(), Options{Version: "dev", Stdout: &out}, args); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	run("index", "--repo", old, "--head", "--profile", "full", "--cache-dir", cache, "--format", "json")
	git(t, repo, "worktree", "remove", old)
	fresh := filepath.Join(t.TempDir(), "view")
	git(t, repo, "worktree", "add", "--detach", fresh, "HEAD")
	t.Cleanup(func() { git(t, repo, "worktree", "remove", "--force", fresh) })
	neighborJSON := run("neighbors", "--repo", fresh, "--head", "--profile", "full", "--cache-dir", cache,
		"--symbol", "Callee", "--file", "calls.go", "--direction", "in", "--selection", "exact", "--format", "json")
	var neighbors neighborResponse
	if err := json.Unmarshal(neighborJSON, &neighbors); err != nil {
		t.Fatal(err)
	}
	if !neighbors.IndexCacheHit || neighbors.RepoRoot != fresh || len(neighbors.Matches) != 1 {
		t.Fatalf("fresh view neighbors=%+v", neighbors)
	}
	incoming := neighbors.Matches[0].Incoming
	if len(incoming) != 1 || incoming[0].Endpoint.Name != "Caller" || incoming[0].CallSite == nil || incoming[0].CallSite.Line != 5 || incoming[0].CallSite.FilePath != "calls.go" {
		t.Fatalf("warm incoming call=%+v", incoming)
	}
	queryJSON := run("query", "--repo", fresh, "--head", "--profile", "full", "--cache-dir", cache,
		"--query", "Caller", "--format", "json", "--top-k", "1")
	var query struct {
		RepoRoot string `json:"repo_root"`
		Stats    struct {
			IndexCacheHit bool `json:"index_cache_hit"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(queryJSON, &query); err != nil {
		t.Fatal(err)
	}
	if !query.Stats.IndexCacheHit || query.RepoRoot != fresh || !bytes.Contains(queryJSON, []byte("Callee()")) {
		t.Fatalf("warm query=%s", queryJSON)
	}
	if strings.Contains(string(neighborJSON), old) || strings.Contains(string(queryJSON), old) {
		t.Fatal("retired view remains in a public response")
	}
}

func TestCachedBareIndexPreservesCommittedSourceAndCalls(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Cache Test")
	git(t, repo, "config", "user.email", "cache@example.com")
	write(t, repo, "bare.go", "package sample\n\nfunc BareLeaf() {}\nfunc BareCaller() {\n BareLeaf()\n}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "bare source")
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, repo, "clone", "--bare", "--no-local", repo, bare)
	cache := t.TempDir()
	run := func(args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := Run(t.Context(), Options{Version: "dev", Stdout: &out}, args); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	for attempt := 0; attempt < 2; attempt++ {
		payload := run("index", "--repo", bare, "--head", "--profile", "full", "--cache-dir", cache, "--format", "json")
		var index indexResponse
		if err := json.Unmarshal(payload, &index); err != nil {
			t.Fatal(err)
		}
		if index.IndexCacheHit != (attempt == 1) || index.RepoRoot != bare || index.Commit != rev(t, repo, "HEAD") || index.Tree != rev(t, repo, "HEAD^{tree}") || index.Counts.Symbols != 2 {
			t.Fatalf("bare index attempt=%d receipt=%s", attempt, payload)
		}
	}
	neighborJSON := run("neighbors", "--repo", bare, "--head", "--profile", "full", "--cache-dir", cache,
		"--symbol", "BareLeaf", "--file", "bare.go", "--direction", "in", "--selection", "exact", "--format", "json")
	var neighbors neighborResponse
	if err := json.Unmarshal(neighborJSON, &neighbors); err != nil {
		t.Fatal(err)
	}
	if !neighbors.IndexCacheHit || len(neighbors.Matches) != 1 {
		t.Fatalf("bare neighbors=%s", neighborJSON)
	}
	incoming := neighbors.Matches[0].Incoming
	if len(incoming) != 1 || incoming[0].Endpoint.Name != "BareCaller" || incoming[0].Relation != "CALLS" || incoming[0].CallSite == nil || incoming[0].CallSite.Line != 5 || incoming[0].CallSite.FilePath != "bare.go" || len(incoming[0].Evidence) == 0 {
		t.Fatalf("bare call and evidence=%+v", incoming)
	}
	queryJSON := run("query", "--repo", bare, "--head", "--profile", "full", "--cache-dir", cache,
		"--query", "BareCaller", "--format", "json", "--top-k", "1")
	var query struct {
		RepoRoot string `json:"repo_root"`
		Stats    struct {
			IndexCacheHit bool `json:"index_cache_hit"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(queryJSON, &query); err != nil {
		t.Fatal(err)
	}
	if !query.Stats.IndexCacheHit || query.RepoRoot != bare || !bytes.Contains(queryJSON, []byte("BareLeaf()")) {
		t.Fatalf("bare source query=%s", queryJSON)
	}
}

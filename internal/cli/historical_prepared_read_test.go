package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func historicalPreparedCommand(t *testing.T, repo, cache, commit, profile, command string) ([]byte, error) {
	t.Helper()
	args := []string{command, "--repo", repo, "--cache-dir", cache, "--prepared-only", "--commit", commit, "--profile", profile, "--format", "json"}
	if command == "query" {
		args = append(args, "--query", "dispatchReview")
	} else {
		args = append(args, "--symbol", "Desk", "--relation", "CONTAINS", "--direction", "out")
	}
	var out bytes.Buffer
	err := Run(t.Context(), Options{Version: "test", Stdout: &out}, args)
	return out.Bytes(), err
}

func TestHistoricalPreparedCommandsWithoutCheckout(t *testing.T) {
	repo, cache := incrementalCommandFixture(t)
	old := incrementalCommandCommit(t, repo)
	oldIndex := incrementalIndexCommand(t, repo, cache, old)
	fullCache := t.TempDir()
	if _, _, err := sem.PreindexProviderSnapshot(t.Context(), repo, "test", sem.ProviderSnapshotOptions{Profile: sem.ProfileFull}, fullCache); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "review.ts", "export class Desk { next() { return 'new'; } }\nexport function dispatchReview() { return 'new'; }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "new HEAD source")
	head := incrementalCommandCommit(t, repo)
	common, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	beforeWorktrees, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	before := sem.StructuralWorkSnapshot()
	buildID, err := sem.ExecutableBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"syntax-only", "full"} {
		directory := cache
		if profile == "full" {
			directory = fullCache
		}
		queryBytes, err := historicalPreparedCommand(t, repo, directory, old, profile, "query")
		if err != nil {
			t.Fatal(err)
		}
		var query sem.SearchResponse
		if err := json.Unmarshal(queryBytes, &query); err != nil {
			t.Fatal(err)
		}
		if query.Commit != old || query.Profile != profile || query.Stats.AnalysisScope != "complete-prepared" || !query.Stats.IndexCacheHit || len(query.Results) == 0 || !strings.Contains(query.Results[0].Snippet, "'old'") || strings.Contains(query.Results[0].Snippet, "'new'") {
			t.Fatalf("mixed old source/query=%s", queryBytes)
		}
		if query.PreparedRead == nil || query.PreparedRead.ProducerBuildID != buildID || len(query.PreparedRead.PolicyID) != 64 || strings.Trim(query.PreparedRead.PolicyID, "0123456789abcdef") != "" || query.PreparedRead.Source.Scope != "" || query.PreparedRead.Source.CommonDir != common || query.PreparedRead.ExternalPolicyOrigin != "serving-directory-captured" || query.PreparedRead.GitignoreOrigin != "target-commit" {
			t.Fatalf("unqualified read=%s", queryBytes)
		}
		if profile == "syntax-only" && (query.PreparedRead.ArtifactFamily != "syntax-generation" || query.PreparedRead.GenerationID != oldIndex.IncrementalPreparation.GenerationID) {
			t.Fatal("read lost generation identity")
		}
		if profile == "full" && (query.PreparedRead.ArtifactFamily != "search-snapshot" || query.PreparedRead.GenerationID != "") {
			t.Fatal("legacy full read invented generation identity")
		}
		var stream bytes.Buffer
		if err := Run(t.Context(), Options{Version: "test", Stdout: &stream}, []string{"query", "--repo", repo, "--cache-dir", directory, "--prepared-only", "--commit", old, "--profile", profile, "--query", "dispatchReview", "--format", "ndjson"}); err != nil {
			t.Fatal(err)
		}
		var header struct {
			PreparedRead *sem.PreparedReadReceipt `json:"prepared_read"`
			Commit       string                   `json:"commit"`
		}
		if err := json.NewDecoder(&stream).Decode(&header); err != nil || header.Commit != old || !reflect.DeepEqual(header.PreparedRead, query.PreparedRead) {
			t.Fatal("NDJSON omitted or changed exact read qualification")
		}
		neighborBytes, err := historicalPreparedCommand(t, repo, directory, old, profile, "neighbors")
		if err != nil {
			t.Fatal(err)
		}
		var neighbors neighborResponse
		if err := json.Unmarshal(neighborBytes, &neighbors); err != nil {
			t.Fatal(err)
		}
		if neighbors.Commit != old || neighbors.Tree != query.Tree || neighbors.Profile != profile || neighbors.AnalysisScope != "complete-prepared" || !reflect.DeepEqual(neighbors.PreparedRead, query.PreparedRead) || len(neighbors.Matches) != 1 || len(neighbors.Matches[0].Outgoing) == 0 {
			t.Fatalf("old neighbors=%s", neighborBytes)
		}
		for _, edge := range neighbors.Matches[0].Outgoing {
			if edge.Endpoint.Name == "next" {
				t.Fatal("neighbors used current HEAD")
			}
		}
	}
	if sem.StructuralWorkSnapshot() != before {
		t.Fatal("historical read performed structural work")
	}
	afterWorktrees, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil || !bytes.Equal(beforeWorktrees, afterWorktrees) || incrementalCommandCommit(t, repo) != head {
		t.Fatal("historical read changed HEAD/worktree inventory")
	}
	for _, profile := range []string{"syntax-only", "full"} {
		output, err := historicalPreparedCommand(t, repo, cache, head, profile, "query")
		var failure *sem.PreparedSnapshotError
		if !errors.As(err, &failure) || len(output) != 0 {
			t.Fatalf("missing preparation emitted success: %s %v", output, err)
		}
	}
	if sem.StructuralWorkSnapshot() != before {
		t.Fatal("missing historical preparation rebuilt")
	}
}

func TestHistoricalPreparedSelectorsRejectAmbiguity(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for _, selectors := range [][]string{
		{"--commit", oid}, {"--commit", "HEAD", "--prepared-only"}, {"--commit", "", "--prepared-only"}, {"--commit", strings.Repeat("A", 40), "--prepared-only"},
		{"--commit", oid, "--prepared-only", "--head"}, {"--head", "--prepared-only", "--commit", oid},
		{"--worktree", "--commit", oid, "--prepared-only"}, {"--commit", oid, "--prepared-only", "--worktree", "--head"},
		{"--commit", oid, "--prepared-only", "--no-cache"}, {"--commit", oid, "--commit", oid, "--prepared-only"},
	} {
		for _, command := range []string{"query", "neighbors"} {
			args := append([]string{command, "--repo", t.TempDir()}, selectors...)
			if command == "query" {
				args = append(args, "--query", "dispatchReview")
			} else {
				args = append(args, "--symbol", "Desk")
			}
			before := sem.StructuralWorkSnapshot()
			var out bytes.Buffer
			if err := Run(t.Context(), Options{Version: "test", Stdout: &out}, args); err == nil || out.Len() != 0 || sem.StructuralWorkSnapshot() != before {
				t.Fatalf("ambiguous selector accepted: %v %v", args, err)
			}
		}
	}
	repo, cache := incrementalCommandFixture(t)
	commit := incrementalCommandCommit(t, repo)
	incrementalIndexCommand(t, repo, cache, commit)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	if err := Run(ctx, Options{Version: "test", Stdout: &out}, []string{"query", "--repo", repo, "--cache-dir", cache, "--prepared-only", "--commit", commit, "--profile", "syntax-only", "--query", "dispatchReview"}); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancelled exact read=%v output=%s", err, &out)
	}
	if !sem.Capabilities().OptionalLocalOnlyFeatures["exact_commit_prepared_read"] {
		t.Fatal("exact read capability absent")
	}
}

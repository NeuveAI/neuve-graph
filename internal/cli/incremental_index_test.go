package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/sem"
)

func incrementalCommandFixture(t *testing.T) (string, string) {
	t.Helper()
	repo, cache := t.TempDir(), t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Incremental Command")
	git(t, repo, "config", "user.email", "incremental@example.invalid")
	write(t, repo, "review.ts", "export class Desk { review(value: string) { return value; } }\nexport function dispatchReview() { return 'old'; }\n")
	write(t, repo, "helper.go", "package review\nfunc helper() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial committed source")
	return repo, cache
}

func incrementalCommandCommit(t *testing.T, repo string) string {
	t.Helper()
	commit, err := gitutil.RevParse(t.Context(), repo, "HEAD^{commit}")
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func incrementalIndexCommand(t *testing.T, repo, cache, commit string) indexResponse {
	t.Helper()
	var out bytes.Buffer
	if err := Run(t.Context(), Options{Version: "test", Stdout: &out}, []string{"index", "--repo", repo, "--cache-dir", cache, "--incremental", "--commit", commit, "--profile", "syntax-only", "--format", "json"}); err != nil {
		t.Fatal(err)
	}
	var response indexResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func syntaxPreparedQueryCommand(t *testing.T, repo, cache, question string) sem.SearchResponse {
	t.Helper()
	var out bytes.Buffer
	if err := Run(t.Context(), Options{Version: "test", Stdout: &out}, []string{"query", "--repo", repo, "--cache-dir", cache, "--head", "--prepared-only", "--profile", "syntax-only", "--query", question, "--format", "json"}); err != nil {
		t.Fatal(err)
	}
	var response sem.SearchResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestIncrementalIndexCommandAndPreparedSyntaxQueries(t *testing.T) {
	repo, cache := incrementalCommandFixture(t)
	initialCommit := incrementalCommandCommit(t, repo)
	started := time.Now()
	initial := incrementalIndexCommand(t, repo, cache, initialCommit)
	coldElapsed := time.Since(started)
	if initial.Profile != "syntax-only" || initial.Commit != initialCommit || initial.IndexCacheHit || initial.IncrementalPreparation == nil || initial.IncrementalPreparation.ExtractedFiles != 2 || initial.IncrementalPreparation.RelationshipResolution != "none" || !reflect.DeepEqual(initial.IncrementalPreparation.SupportedRelationTypes, []string{"CONTAINS", "DEFINES"}) {
		t.Fatalf("unqualified preparation receipt: %#v", initial)
	}
	write(t, repo, "review.ts", "export class Desk { review(value: string) { return value; } }\nexport function dispatchReview() { return 'new'; }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "change TypeScript blob")
	commit := incrementalCommandCommit(t, repo)
	before := sem.StructuralWorkSnapshot()
	started = time.Now()
	delta := incrementalIndexCommand(t, repo, cache, commit)
	deltaElapsed := time.Since(started)
	after := sem.StructuralWorkSnapshot()
	if after.Parses-before.Parses != 1 || after.Extractions-before.Extractions != 1 || after.Resolutions != before.Resolutions || delta.IncrementalPreparation.ExtractedFiles != 1 || delta.IncrementalPreparation.ReusedFiles != 1 {
		t.Fatalf("delta structural work=%v -> %v, receipt=%#v", before, after, delta.IncrementalPreparation)
	}
	before = sem.StructuralWorkSnapshot()
	started = time.Now()
	warm := incrementalIndexCommand(t, repo, cache, commit)
	warmElapsed := time.Since(started)
	if !warm.IndexCacheHit || warm.IncrementalPreparation.GenerationID != delta.IncrementalPreparation.GenerationID || sem.StructuralWorkSnapshot() != before {
		t.Fatal("exact generation retry did structural work or changed identity")
	}
	freshCache := t.TempDir()
	if _, _, err := sem.PreindexProviderSnapshot(t.Context(), repo, "test", sem.ProviderSnapshotOptions{Profile: sem.ProfileSyntaxOnly}, freshCache); err != nil {
		t.Fatal(err)
	}
	before = sem.StructuralWorkSnapshot()
	for _, question := range []string{"dispatch review", "Desk review", "unknown source qzxjkv"} {
		actual := syntaxPreparedQueryCommand(t, repo, cache, question)
		fresh := syntaxPreparedQueryCommand(t, repo, freshCache, question)
		if actual.Query != question || actual.Commit != commit || actual.Profile != "syntax-only" || actual.Stats.AnalysisScope != "complete-prepared" || !actual.Stats.IndexCacheHit || !reflect.DeepEqual(actual.Results, fresh.Results) {
			t.Fatalf("generation query differs from independently prepared fresh syntax facts: %q", question)
		}
	}
	if sem.StructuralWorkSnapshot() != before {
		t.Fatal("prepared natural queries did structural work")
	}
	var neighbors []neighborResponse
	for _, directory := range []string{cache, freshCache} {
		var out bytes.Buffer
		if err := Run(t.Context(), Options{Version: "test", Stdout: &out}, []string{"neighbors", "--repo", repo, "--cache-dir", directory, "--head", "--prepared-only", "--profile", "syntax-only", "--symbol", "Desk", "--relation", "CONTAINS", "--direction", "out", "--format", "json"}); err != nil {
			t.Fatal(err)
		}
		var response neighborResponse
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Profile != "syntax-only" || response.Commit != commit || response.AnalysisScope != "complete-prepared" || len(response.Matches) != 1 || len(response.Matches[0].Outgoing) != 1 || response.Matches[0].Outgoing[0].Endpoint.Name != "review" {
			t.Fatalf("syntax containment frontier missing/incorrect: %s", &out)
		}
		response.IndexLatencyMS, response.QueryLatencyMS, response.TotalLatencyMS = 0, 0, 0
		if response.PreparedRead == nil || (directory == cache && (response.PreparedRead.ArtifactFamily != "syntax-generation" || response.PreparedRead.GenerationID != delta.IncrementalPreparation.GenerationID)) || (directory == freshCache && (response.PreparedRead.ArtifactFamily != "search-snapshot" || response.PreparedRead.GenerationID != "")) {
			t.Fatal("syntax neighbors lost the actual artifact-family qualification")
		}
		response.PreparedRead = nil
		neighbors = append(neighbors, response)
	}
	if !reflect.DeepEqual(neighbors[0], neighbors[1]) || sem.StructuralWorkSnapshot() != before {
		t.Fatal("prepared syntax neighbors differ or performed structural work")
	}
	t.Logf("two-language two-path fixture: cold=%s changed-TS=%s retained=%s; changed parse=1 unchanged parse=0 retained structural work=0", coldElapsed, deltaElapsed, warmElapsed)
}

func TestIncrementalIndexCapabilitiesDeclareSyntaxProfileOnly(t *testing.T) {
	var out bytes.Buffer
	if err := Run(t.Context(), Options{Stdout: &out}, []string{"capabilities", "--json"}); err != nil {
		t.Fatal(err)
	}
	var capabilities sem.CapabilityReport
	if err := json.Unmarshal(out.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if !capabilities.OptionalLocalOnlyFeatures["incremental_syntax_preparation"] || !reflect.DeepEqual(capabilities.IncrementalPreparationProfiles, []string{"syntax-only"}) || !reflect.DeepEqual(capabilities.RelationSupportByProfile["syntax-only"], []string{"CONTAINS", "DEFINES"}) {
		t.Fatalf("incremental capability failed to qualify syntax limits: %s", &out)
	}
}

func TestIncrementalIndexCommandPinsOldCommitAndRequiresExplicitAdmission(t *testing.T) {
	repo, cache := incrementalCommandFixture(t)
	old := incrementalCommandCommit(t, repo)
	git(t, repo, "commit", "--allow-empty", "-m", "advance HEAD without source change")
	prepared := incrementalIndexCommand(t, repo, cache, old)
	if prepared.Commit != old {
		t.Fatal("queued explicit target silently prepared newer HEAD")
	}
	before := sem.StructuralWorkSnapshot()
	var out bytes.Buffer
	err := Run(t.Context(), Options{Version: "test", Stdout: &out}, []string{"query", "--repo", repo, "--cache-dir", cache, "--head", "--prepared-only", "--profile", "syntax-only", "--query", "review", "--format", "json"})
	var failure *sem.PreparedSnapshotError
	if !errors.As(err, &failure) || failure.Reason != "missing" || out.Len() != 0 {
		t.Fatalf("wrong revision yielded success: %v %q", err, &out)
	}
	for _, args := range [][]string{
		{"index", "--incremental", "--commit", old},
		{"index", "--incremental", "--profile", "syntax-only"},
		{"index", "--incremental", "--profile", "syntax-only", "--commit", old, "--force"},
		{"index", "--incremental", "--profile", "syntax-only", "--commit", old, "--worktree"},
		{"index", "--profile", "syntax-only", "--commit", old},
		{"health", "--incremental", "--profile", "syntax-only", "--commit", old},
		{"index", "--incremental", "--profile", "syntax-only", "--commit", "HEAD"},
	} {
		out.Reset()
		args = append(args, "--repo", repo, "--cache-dir", cache, "--format", "json")
		if err := Run(t.Context(), Options{Version: "test", Stdout: &out}, args); err == nil || out.Len() != 0 {
			t.Fatalf("unsupported preparation admitted: %v %v %q", args, err, &out)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out.Reset()
	err = Run(ctx, Options{Version: "test", Stdout: &out}, []string{"index", "--repo", repo, "--cache-dir", cache, "--incremental", "--commit", old, "--profile", "syntax-only", "--format", "json"})
	if !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancelled preparation emitted receipt: %v %q", err, &out)
	}
	if sem.StructuralWorkSnapshot() != before {
		t.Fatal("wrong revision or invalid admission performed structural work")
	}
	if matches, _ := filepath.Glob(filepath.Join(cache, "search", "*", "*.json.gz")); len(matches) != 0 {
		t.Fatal("incremental preparation masqueraded as legacy complete search cache")
	}
}

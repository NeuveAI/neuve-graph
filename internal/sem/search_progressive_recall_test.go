package sem

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const progressiveRecallQuery = "How does ExecuteDeterministicRequest bypass queue observation for reproducible execution while preserving watcher semantics in normal mode, and which production caller and parity test establish both branches?"

func progressiveRecallRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	write(t, repo, "pipeline/execute.go", `package pipeline

// ExecuteDeterministicRequest bypasses queue observation for reproducible
// execution and preserves watcher semantics in normal mode.
func ExecuteDeterministicRequest(deterministic bool) string {
	_ = ReadMonotonicClock()
	if deterministic {
		return "direct"
	}
	return ObserveCommandQueue()
}
`)
	write(t, repo, "pipeline/watcher.go", `package pipeline

// ObserveCommandQueue preserves queue watcher semantics in normal mode.
func ObserveCommandQueue() string { return "watched" }
`)
	write(t, repo, "pipeline/agent.go", `package pipeline

// RunProductionAgentLoop is the production caller for both execution branches.
func RunProductionAgentLoop() string { return ExecuteDeterministicRequest(false) }
`)
	write(t, repo, "pipeline/clock.go", `package pipeline

func ReadMonotonicClock() int64 { return 0 }
`)
	write(t, repo, "pipeline/execute_test.go", `package pipeline

import "testing"

func TestExecuteDeterministicRequestParity(t *testing.T) {
	if ExecuteDeterministicRequest(true) == ExecuteDeterministicRequest(false) {
		t.Fatal("execution branches unexpectedly match")
	}
}
`)

	for index := 0; index < 120; index++ {
		write(t, repo, fmt.Sprintf("internal/watcher/scenario%03d.go", index), fmt.Sprintf(`package watcher

// Scenario%03d is a production scenario.
func Scenario%03d() string { return "production" }
`, index, index))
	}
	for index := 0; index < 500; index++ {
		write(t, repo, fmt.Sprintf(".github/ISSUE_TEMPLATE/report%03d.md", index), fmt.Sprintf(`# Operational report %03d

Bypass queue observation for reproducible execution while preserving watcher
semantics in normal mode; production caller parity test establishes both branches.
`, index))
	}
	return repo
}

func TestDefaultProgressiveSearchKeepsImmediateStructuralSupport(t *testing.T) {
	repo := progressiveRecallRepo(t)
	response, err := SearchRepository(t.Context(), repo, "test", progressiveRecallQuery, SearchOptions{
		Worktree:        true,
		Profile:         ProfileFull,
		TopK:            8,
		MaxContextBytes: 16 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(response.Results))
	for _, result := range response.Results {
		if result.SymbolName != "" {
			got = append(got, result.SymbolName)
		}
	}
	want := []string{
		"ExecuteDeterministicRequest",
		"RunProductionAgentLoop",
		"TestExecuteDeterministicRequestParity",
		"ObserveCommandQueue",
	}
	if len(got) < len(want) || !reflect.DeepEqual(got[:len(want)], want) {
		t.Fatalf("ranked symbols = %#v, want the four structural roles in order %#v; stats=%#v", got, want, response.Stats)
	}
	if strings.Contains(strings.Join(got, "\n"), "ReadMonotonicClock") {
		t.Fatalf("unrelated zero-overlap callee entered the result: %#v", got)
	}
}

func TestProgressiveSearchCensusPreservesBaseGraphAtEveryWidth(t *testing.T) {
	repo := progressiveRecallRepo(t)
	q := buildSearchQuery(progressiveRecallQuery)
	sparse := buildSparseSearchQueryExpanded(progressiveRecallQuery, q)
	snapshotOptions := ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull}

	legacy, err := preselectSearchFiles(t.Context(), repo, q, sparse,
		SearchOptions{Worktree: true, Profile: ProfileFull, MaxIndexedFiles: 96},
		snapshotOptions, ProviderSnapshot{}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertProgressiveRecallGraph(t, repo, "legacy base", legacy.files)

	progressive, err := preselectSearchFiles(t.Context(), repo, q, sparse,
		SearchOptions{Worktree: true, Profile: ProfileFull, MaxIndexedFiles: 96, progressivePreselection: true},
		snapshotOptions, ProviderSnapshot{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, width := range []int{96, 192, 384} {
		if len(progressive.files) < width {
			t.Fatalf("progressive selection has %d files, want the %d-file census stage", len(progressive.files), width)
		}
		assertProgressiveRecallGraph(t, repo, fmt.Sprintf("fused width %d", width), progressive.files[:width])
	}
}

func TestOmittedLimitKeepsStructuralSupportContinuousAcrossTopK32And33(t *testing.T) {
	repo := progressiveRecallRepo(t)
	search := func(topK int) SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", progressiveRecallQuery, SearchOptions{
			Worktree:        true,
			Profile:         ProfileFull,
			TopK:            topK,
			MaxContextBytes: 0,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	want := []string{
		"ExecuteDeterministicRequest",
		"RunProductionAgentLoop",
		"TestExecuteDeterministicRequestParity",
		"ObserveCommandQueue",
	}
	for _, response := range []SearchResponse{search(32), search(33)} {
		got := make([]string, 0, len(response.Results))
		for _, result := range response.Results {
			if result.SymbolName != "" {
				got = append(got, result.SymbolName)
			}
		}
		if len(got) < len(want) || !reflect.DeepEqual(got[:len(want)], want) {
			t.Fatalf("top-k structural prefix = %#v, want %#v; stats=%#v", got, want, response.Stats)
		}
		if response.Stats.FilesIndexed > searchPreselectionMaxSelectedFiles {
			t.Fatalf("top-k indexed %d files, absolute ceiling is %d", response.Stats.FilesIndexed, searchPreselectionMaxSelectedFiles)
		}
	}
}

func assertProgressiveRecallGraph(t *testing.T, repo, stage string, files []string) {
	t.Helper()
	snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{
		Worktree:  true,
		Profile:   ProfileFull,
		OnlyFiles: files,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSymbols := []string{
		"ExecuteDeterministicRequest",
		"ObserveCommandQueue",
		"RunProductionAgentLoop",
		"TestExecuteDeterministicRequestParity",
	}
	for _, name := range wantSymbols {
		if !hasSymbolNamed(snapshot.Symbols, name) {
			t.Fatalf("%s omitted %s: files=%d symbols=%d relations=%d", stage, name, len(files), len(snapshot.Symbols), len(snapshot.Relations))
		}
	}
	wantCalls := [][2]string{
		{"ExecuteDeterministicRequest", "ObserveCommandQueue"},
		{"RunProductionAgentLoop", "ExecuteDeterministicRequest"},
		{"TestExecuteDeterministicRequestParity", "ExecuteDeterministicRequest"},
	}
	for _, call := range wantCalls {
		if !hasRelationBySymbolName(snapshot, "CALLS", call[0], call[1]) {
			t.Fatalf("%s omitted CALLS %s -> %s: files=%d symbols=%d relations=%d", stage, call[0], call[1], len(files), len(snapshot.Symbols), len(snapshot.Relations))
		}
	}
}

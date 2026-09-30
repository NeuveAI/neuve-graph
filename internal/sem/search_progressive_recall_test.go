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

func stableMixedCorpusRankingRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	var execute strings.Builder
	execute.WriteString(`package pipeline

// ExecuteDeterministicRequest bypasses queue observation for reproducible
// execution while preserving watcher semantics in normal mode.
func ExecuteDeterministicRequest(deterministic bool) string {
	if deterministic {
		return "direct"
	}
	return ObserveCommandQueue()
}

// DriveRequestCycle deliberately carries no query vocabulary. Its two distant
// calls are independently useful evidence for the same production callable.
func DriveRequestCycle(deterministic bool) string {
	first := ExecuteDeterministicRequest(deterministic)
	value := first
`)
	for range 180 {
		execute.WriteString("\tvalue += first\n")
	}
	execute.WriteString(`
	second := ExecuteDeterministicRequest(!deterministic)
	return value + second
}
`)
	write(t, repo, "pipeline/execute.go", execute.String())
	write(t, repo, "pipeline/watcher.go", `package pipeline

// ObserveCommandQueue preserves watcher semantics in normal mode.
func ObserveCommandQueue() string { return "watched" }
`)
	for index := 0; index < 4; index++ {
		write(t, repo, fmt.Sprintf("decoys/variant%d/execute.go", index), fmt.Sprintf(`package variant%d

// ExecuteDeterministicRequest repeats the request vocabulary but has no
// call or usage relationship to the execution path under inspection.
func ExecuteDeterministicRequest() string {
	return "bypass queue observation reproducible execution watcher semantics normal mode production caller parity check branches"
}
`, index))
	}
	write(t, repo, "pipeline/execute_test.go", `package pipeline

import "testing"

func TestBranchAgreement(t *testing.T) {
	if ExecuteDeterministicRequest(true) == ExecuteDeterministicRequest(false) {
		t.Fatal("execution branches unexpectedly match")
	}
}
`)
	for index := 0; index < 170; index++ {
		write(t, repo, fmt.Sprintf("internal/scenario/scenario%03d.go", index), fmt.Sprintf(`package scenario

// Scenario%03d is an unrelated production operation.
func Scenario%03d() string { return "ordinary" }
`, index, index))
	}
	for index := 0; index < 330; index++ {
		write(t, repo, fmt.Sprintf("docs/scenarios/note%03d.md", index), fmt.Sprintf(`# Execution note %03d

Deterministic requests bypass queue observation for reproducible execution while
normal mode preserves watcher semantics. A production caller and parity check
establish both branches.
`, index))
	}
	return repo
}

func searchSemanticIdentity(result SearchResult) string {
	return fmt.Sprintf("%s|%s|%s|%d|%d",
		result.FilePath, result.Kind, result.SymbolName, result.StartLine, result.EndLine)
}

func primarySearchSemanticIdentities(results []SearchResult) []string {
	var identities []string
	for _, result := range results {
		if result.Section == searchSectionRelated || result.Section == searchSectionCoveringTest {
			continue
		}
		identities = append(identities, searchSemanticIdentity(result))
	}
	return identities
}

func assertSearchSupplementContract(t *testing.T, response SearchResponse, topK int) {
	t.Helper()
	primary, related, covering := 0, 0, 0
	supplementsStarted := false
	coveringStarted := false
	for _, result := range response.Results {
		switch result.Section {
		case searchSectionRelated:
			if coveringStarted {
				t.Fatal("related supplement followed covering-test supplement")
			}
			supplementsStarted = true
			related++
		case searchSectionCoveringTest:
			supplementsStarted = true
			coveringStarted = true
			covering++
		default:
			if supplementsStarted {
				t.Fatalf("primary result %s followed a supplement", result.FilePath)
			}
			primary++
		}
	}
	if primary > topK {
		t.Fatalf("primary results = %d, exceed TopK %d", primary, topK)
	}
	if related > searchRelatedSiteLimit || covering > searchCoveringTestLimit {
		t.Fatalf("supplements = %d related / %d covering, bounds are %d / %d",
			related, covering, searchRelatedSiteLimit, searchCoveringTestLimit)
	}
}

func TestQueryTopKProjectsOneStableMixedCorpusRanking(t *testing.T) {
	repo := stableMixedCorpusRankingRepo(t)
	const query = "How does ExecuteDeterministicRequest bypass queue observation for reproducible execution while preserving watcher semantics in normal mode, and which production caller and parity check establish both branches?"
	search := func(topK int) SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", query, SearchOptions{
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

	responses := map[int]SearchResponse{}
	for _, topK := range []int{1, 2, 8, 16, 64} {
		responses[topK] = search(topK)
		assertSearchSupplementContract(t, responses[topK], topK)
	}
	at8, at64 := responses[8], responses[64]
	for _, required := range []struct {
		path   string
		symbol string
	}{
		{path: "pipeline/execute.go", symbol: "ExecuteDeterministicRequest"},
		{path: "pipeline/watcher.go", symbol: "ObserveCommandQueue"},
		{path: "pipeline/execute_test.go", symbol: "TestBranchAgreement"},
	} {
		found := false
		for _, result := range at8.Results {
			if result.FilePath == required.path && result.SymbolName == required.symbol {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("top-k 8 omitted %s in %s: %#v", required.symbol, required.path, at8.Results)
		}
	}
	callerLocations := map[string]bool{}
	for _, result := range at8.Results {
		if result.Section == searchSectionPrimary && result.FilePath == "pipeline/execute.go" &&
			result.SymbolName == "DriveRequestCycle" {
			callerLocations[fmt.Sprintf("%d:%d", result.StartLine, result.EndLine)] = true
		}
	}
	if len(callerLocations) != 2 {
		t.Fatalf("top-k 8 production caller regions = %#v, want two distinct locations: %#v", callerLocations, at8.Results)
	}
	for _, topK := range []int{1, 2, 8, 16} {
		response := responses[topK]
		got := primarySearchSemanticIdentities(response.Results)
		deep := primarySearchSemanticIdentities(at64.Results)
		if len(got) == 0 || len(deep) < len(got) {
			t.Fatalf("top-k %d primary result counts = %d/%d, want a non-empty prefix",
				topK, len(got), len(deep))
		}
		if !reflect.DeepEqual(got, deep[:len(got)]) {
			t.Fatalf("top-k %d changed primary semantic prefix: %#v -> %#v", topK, got, deep[:len(got)])
		}
	}

	bounded := func(topK int) SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", query, SearchOptions{
			Worktree: true, Profile: ProfileFull, TopK: topK, MaxContextBytes: 16 * 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	bounded8, bounded64 := bounded(8), bounded(64)
	primaryResults := func(results []SearchResult) []SearchResult {
		var primary []SearchResult
		for _, result := range results {
			if result.Section != searchSectionRelated && result.Section != searchSectionCoveringTest {
				primary = append(primary, result)
			}
		}
		return primary
	}
	boundedHead, boundedDeep := primaryResults(bounded8.Results), primaryResults(bounded64.Results)
	if len(boundedHead) == 0 || len(boundedDeep) < len(boundedHead) {
		t.Fatalf("finite budget primary counts = %d/%d, want a non-empty prefix", len(boundedHead), len(boundedDeep))
	}
	for index, head := range boundedHead {
		deep := boundedDeep[index]
		// File plus focus is the location identity that survives every rendering diet. When both
		// projections carry a parser-owned symbol ID it must agree too. One side may gain an ID
		// when the snippet allocator can afford the complete enclosing body; that is additive
		// rendering metadata, not a different ranked location.
		if head.FilePath != deep.FilePath || head.FocusLine != deep.FocusLine ||
			(head.SymbolID != "" && deep.SymbolID != "" && head.SymbolID != deep.SymbolID) {
			t.Fatalf("finite budget reordered primary rank %d: %#v -> %#v", index+1, head, deep)
		}
	}
	if bounded8.Results[0].SymbolName != "ExecuteDeterministicRequest" {
		t.Fatalf("finite budget lost required head evidence: %#v", bounded8.Results)
	}
	parity := false
	for _, result := range bounded8.Results {
		parity = parity || (result.Section != searchSectionCoveringTest &&
			result.FilePath == "pipeline/execute_test.go" && result.SymbolName == "TestBranchAgreement")
	}
	if !parity {
		t.Fatalf("finite budget lost primary parity evidence: %#v", bounded8.Results)
	}
}

func TestMixedProseSourceSelectionProjectsOneStableRanking(t *testing.T) {
	q := buildSearchQuery("durable evidence explicit ownership")
	var candidates []searchCandidate
	for index := 0; index < 10; index++ {
		candidate := projectionCandidate(
			fmt.Sprintf("docs/note%02d.md", index), fmt.Sprintf("Note%02d", index), 1, float64(100-index),
		)
		candidate.result.Kind = "section"
		candidate.result.Snippet = "durable evidence explicit ownership"
		candidate.termCounts = map[string]int{"durable": 1, "evidence": 1, "explicit": 1, "ownership": 1}
		candidates = append(candidates, candidate)
		if index == 7 {
			source := projectionCandidate("src/worker.go", "RunProductionCycle", 20, 92.5)
			source.result.Snippet = "func RunProductionCycle() { recordEvidence() }"
			source.termCounts = map[string]int{"evidence": 1}
			candidates = append(candidates, source)
		}
	}

	ordinary8 := selectDiverseCandidates(candidates, 8, 3)
	ordinaryAll := selectDiverseCandidates(candidates, 64, 3)
	sourceKey := searchCandidateLocationKey(candidates[8])
	for _, candidate := range ordinary8 {
		if searchCandidateLocationKey(candidate) == sourceKey {
			t.Fatalf("fixture source unexpectedly entered the shallow ordinary baseline: %#v", candidateIdentityPrefix(ordinary8))
		}
	}
	sourceRank := 0
	for index, candidate := range ordinaryAll {
		if searchCandidateLocationKey(candidate) == sourceKey {
			sourceRank = index + 1
			break
		}
	}
	if sourceRank != 9 {
		t.Fatalf("fixture source ordinary rank = %d, want 9: %#v", sourceRank, candidateIdentityPrefix(ordinaryAll))
	}

	at8 := selectSearchCandidates(candidates, q, 8, 3, true)
	at64 := selectSearchCandidates(candidates, q, 64, 3, true)
	if len(at8) != 8 || len(at64) < len(at8) ||
		!reflect.DeepEqual(candidateIdentityPrefix(at8), candidateIdentityPrefix(at64[:len(at8)])) {
		t.Fatalf("mixed prose/source prefix changed with TopK: %#v -> %#v",
			candidateIdentityPrefix(at8), candidateIdentityPrefix(at64))
	}
	foundSource := false
	for _, candidate := range at8 {
		foundSource = foundSource || searchCandidateLocationKey(candidate) == sourceKey
	}
	if !foundSource {
		t.Fatalf("top-k 8 omitted weak but legitimate source below the shallow ordinary cohort: %#v",
			candidateIdentityPrefix(at8))
	}
}

func TestSearchRepositoryMixedProseSourceProjectsOneStableRanking(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "examples/worker.go", `package examples

// ProcessCycle runs the operational path.
func ProcessCycle() string {
	value := "ready"
`+strings.Repeat("\tvalue += \".\"\n", 500)+`	return value
}
`)
	for index := 0; index < 10; index++ {
		write(t, repo, fmt.Sprintf("docs/note%02d.md", index), fmt.Sprintf(`# Documentation note %02d

documentation durable explicit ownership repeatable verification handoff run operational
`, index))
	}
	const query = "documentation durable explicit ownership repeatable verification handoff run operational"
	search := func(topK int) SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", query, SearchOptions{
			Worktree: true, Profile: ProfileFull, TopK: topK, MaxContextBytes: 0,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	at8, at64 := search(8), search(64)
	head, deep := primarySearchSemanticIdentities(at8.Results), primarySearchSemanticIdentities(at64.Results)
	deepSource := false
	for _, result := range at64.Results {
		deepSource = deepSource || result.FilePath == "examples/worker.go"
	}
	if !deepSource {
		t.Fatalf("deeper public ranking lost the weak source the fixture is meant to project: %#v", at64.Results)
	}
	if len(head) != 8 || len(deep) < len(head) || !reflect.DeepEqual(head, deep[:len(head)]) {
		t.Fatalf("public mixed prose/source prefix changed with TopK: %#v -> %#v", head, deep)
	}
	foundSource := false
	for _, result := range at8.Results {
		foundSource = foundSource || result.FilePath == "examples/worker.go"
	}
	if !foundSource {
		t.Fatalf("top-k 8 omitted weak but legitimate source below the shallow ordinary cohort: %#v", at8.Results)
	}
}

func deepTopKProjectionRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for file := 0; file < 24; file++ {
		var body strings.Builder
		fmt.Fprintf(&body, "package corpus\n\n// ReconcileLedger%03d synchronizes the delta ledger retry window.\n", file)
		fmt.Fprintf(&body, "func ReconcileLedger%03d() string {\n", file)
		for line := 0; line < 150; line++ {
			if line == 10 || line == 80 || line == 140 {
				fmt.Fprintf(&body, "\t// synchronize delta ledger retry window checkpoint %d\n", line)
			}
			fmt.Fprintf(&body, "\tvalue%d := %d\n\t_ = value%d\n", line, line, line)
		}
		body.WriteString("\treturn \"done\"\n}\n")
		write(t, repo, fmt.Sprintf("corpus/ledger%03d.go", file), body.String())
	}
	for file := 0; file < 96; file++ {
		write(t, repo, fmt.Sprintf("other/filler%03d.go", file), fmt.Sprintf(
			"package other\n\nfunc Filler%03d() int { return %d }\n", file, file))
	}
	return repo
}

func TestSearchRepositoryDeepProjectsOneStableMixedRanking(t *testing.T) {
	repo := deepTopKProjectionRepo(t)
	const query = "synchronize delta ledger retry window"
	for _, limit := range []struct {
		name string
		max  int
	}{{name: "omitted"}, {name: "explicit", max: 96}} {
		t.Run(limit.name, func(t *testing.T) {
			search := func(topK int) SearchResponse {
				response, err := SearchRepository(t.Context(), repo, "test", query, SearchOptions{
					Worktree: true, Profile: ProfileFull, Deep: true, TopK: topK,
					MaxIndexedFiles: limit.max, MaxContextBytes: 0,
				})
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			at8, at64 := search(8), search(64)
			head, deep := primarySearchSemanticIdentities(at8.Results), primarySearchSemanticIdentities(at64.Results)
			if len(head) != 8 || len(deep) < 64 || !reflect.DeepEqual(head, deep[:8]) {
				t.Fatalf("deep primary projection changed: %d/%d %#v -> %#v", len(head), len(deep), head, deep[:minInt(8, len(deep))])
			}
			if at8.Stats.SparseCandidates == 0 || at64.Stats.SparseCandidates == 0 {
				t.Fatalf("deep fixture did not exercise sparse overlap: %#v / %#v", at8.Stats, at64.Stats)
			}
			files := map[string]int{}
			for _, result := range at64.Results {
				if result.Section != searchSectionRelated && result.Section != searchSectionCoveringTest {
					files[result.FilePath]++
				}
			}
			repeated := false
			for _, count := range files {
				repeated = repeated || count > 1
			}
			if !repeated {
				t.Fatalf("deep ranking never exercised repeated-file replacement: %#v", files)
			}
			if limit.max > 0 && (at64.Stats.FilesIndexed == 0 || at64.Stats.FilesIndexed > limit.max) {
				t.Fatalf("explicit indexed files = %d, want a non-empty cohort capped at %d", at64.Stats.FilesIndexed, limit.max)
			}
		})
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

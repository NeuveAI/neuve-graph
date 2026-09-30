package sem

import (
	"reflect"
	"strings"
	"testing"
)

func rankedTestContractResults(focus int) []SearchResult {
	results := contractPayload()[:searchEnclosureHeadRanks]
	results = append(results, SearchResult{
		Rank:             searchEnclosureHeadRanks + 1,
		Score:            42,
		FilePath:         "tests/contract_test.go",
		StartLine:        focus,
		EndLine:          focus + 1,
		FocusLine:        focus,
		SnippetStartLine: focus,
		SnippetEndLine:   focus + 1,
		Language:         "Go",
		Signals:          []string{"body"},
		Snippet:          "ranked evidence\ncontinued",
	})
	return results
}

func rankedTestContractLines(count int) []string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = "contract line " + itoaCover(index+1)
	}
	return lines
}

// A ranked test region below the protected fix-site head is already relevance evidence. When the
// parser cannot name its surrounding test, contract context must promote that region instead of
// requiring a symbol the repository does not expose.
func TestPlanSearchRankedTestContractPromotesSymbolLessTail(t *testing.T) {
	t.Parallel()
	results := rankedTestContractResults(31)
	lines := rankedTestContractLines(39)
	read := func(path string) (string, bool) {
		if path != "tests/contract_test.go" {
			return "", false
		}
		return strings.Join(lines, "\n"), true
	}
	plan, ok := planSearchRankedTestContract(
		results, results, buildSearchQuery("contract subject behaviour"), read, 16384,
	)
	if !ok {
		t.Fatal("symbol-less ranked test tail was not promoted")
	}
	entry := plan.entry
	if entry.Section != searchSectionCoveringTest {
		t.Fatalf("section = %q, want %q", entry.Section, searchSectionCoveringTest)
	}
	if entry.StartLine != 1 || entry.EndLine != 39 ||
		entry.SnippetStartLine != 1 || entry.SnippetEndLine != 39 {
		t.Fatalf("small artifact was not shown whole: %#v", entry)
	}
	if entry.Language != "Go" {
		t.Fatalf("language = %q, want ranked result language", entry.Language)
	}
}

// Oversized test artifacts are contract WINDOWS, not pretend symbols or complete units. The
// window remains centred on the ranked focus and carries the same honest label used by ordinary
// bounded read windows.
func TestPlanSearchRankedTestContractBoundsLargeArtifactHonestly(t *testing.T) {
	t.Parallel()
	results := rankedTestContractResults(100)
	lines := rankedTestContractLines(140)
	read := func(path string) (string, bool) {
		return strings.Join(lines, "\n"), path == "tests/contract_test.go"
	}
	plan, ok := planSearchRankedTestContract(
		results, results, buildSearchQuery("contract subject behaviour"), read, 16384,
	)
	if !ok {
		t.Fatal("large symbol-less ranked test tail was not promoted")
	}
	entry := plan.entry
	if entry.StartLine != 70 || entry.EndLine != 129 {
		t.Fatalf("contract window = %d-%d, want centred 70-129: %#v",
			entry.StartLine, entry.EndLine, entry)
	}
	if !containsString(entry.Signals, searchHeadWindowSignal) {
		t.Fatalf("bounded contract lacks %q: %v", searchHeadWindowSignal, entry.Signals)
	}
	for _, forbidden := range []string{searchCompleteSymbolSignal, searchFullUnitSignal} {
		if containsString(entry.Signals, forbidden) {
			t.Fatalf("bounded contract falsely claims %q: %v", forbidden, entry.Signals)
		}
	}
	if entry.SnippetStartLine < entry.StartLine || entry.SnippetEndLine > entry.EndLine {
		t.Fatalf("snippet %d-%d escapes contract window %d-%d",
			entry.SnippetStartLine, entry.SnippetEndLine, entry.StartLine, entry.EndLine)
	}
	if want := strings.Join(lines[entry.SnippetStartLine-1:entry.SnippetEndLine], "\n"); entry.Snippet != want {
		t.Fatalf("snippet is not the verbatim reported slice:\n got %q\nwant %q", entry.Snippet, want)
	}
}

func TestPlanSearchRankedTestContractDoesNotPromoteProtectedHeadOrExplicitTestRequest(t *testing.T) {
	t.Parallel()
	read := func(string) (string, bool) { return strings.Join(rankedTestContractLines(39), "\n"), true }

	head := rankedTestContractResults(20)
	head[0], head[searchEnclosureHeadRanks] = head[searchEnclosureHeadRanks], head[0]
	for index := range head {
		head[index].Rank = index + 1
	}
	if _, ok := planSearchRankedTestContract(
		head, head, buildSearchQuery("subject behaviour"), read, 16384,
	); ok {
		t.Fatal("protected-head test was promoted")
	}

	tail := rankedTestContractResults(20)
	if _, ok := planSearchRankedTestContract(
		tail, tail, buildSearchQuery("tests for subject behaviour"), read, 16384,
	); ok {
		t.Fatal("explicit test request was reclassified")
	}
}

func TestPlanSearchRankedTestContractRequiresContentRelevanceAndChoosesHighestRank(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"tests/first_test.go":  strings.Join(rankedTestContractLines(20), "\n"),
		"tests/second_test.go": strings.Join(rankedTestContractLines(20), "\n"),
	}
	read := func(path string) (string, bool) { content, ok := files[path]; return content, ok }
	results := rankedTestContractResults(10)
	results[searchEnclosureHeadRanks].FilePath = "tests/first_test.go"
	second := results[searchEnclosureHeadRanks]
	second.Rank++
	second.FilePath = "tests/second_test.go"
	results = append(results, second)
	plan, ok := planSearchRankedTestContract(
		results, results, buildSearchQuery("subject behaviour"), read, 16384,
	)
	if !ok || plan.entry.FilePath != "tests/first_test.go" {
		t.Fatalf("highest-ranked relevant test was not selected: %#v", plan.entry)
	}

	pathOnly := rankedTestContractResults(10)
	pathOnly[searchEnclosureHeadRanks].FilePath = "tests/first_test.go"
	pathOnly[searchEnclosureHeadRanks].Signals = []string{"path"}
	pathOnly[searchEnclosureHeadRanks].Snippet = ""
	if _, ok := planSearchRankedTestContract(
		pathOnly, pathOnly, buildSearchQuery("subject behaviour"), read, 16384,
	); ok {
		t.Fatal("path-only test was promoted without content relevance")
	}
}

func TestApplySearchRankedTestContractReclassifiesOnlyTheSelectedTail(t *testing.T) {
	t.Parallel()
	results := rankedTestContractResults(31)
	results = append(results,
		SearchResult{Rank: 7, FilePath: "src/head.go", StartLine: 900, EndLine: 900,
			FocusLine: 900, SnippetStartLine: 900, SnippetEndLine: 900, Signals: []string{}},
		SearchResult{Rank: 8, FilePath: "src/only.go", StartLine: 12, EndLine: 12,
			FocusLine: 12, SnippetStartLine: 12, SnippetEndLine: 12, Signals: []string{}},
	)
	original := append([]SearchResult(nil), results...)
	entry, ok := searchRankedTestContractResult(
		results[searchEnclosureHeadRanks], rankedTestContractLines(39),
	)
	if !ok {
		t.Fatal("ranked test contract was not renderable")
	}
	plan := searchRankedTestContractPlan{
		entry: entry, ranked: results[searchEnclosureHeadRanks],
		delta: serializedSearchResultBytes([]SearchResult{entry}) -
			serializedSearchResultBytes([]SearchResult{results[searchEnclosureHeadRanks]}),
	}
	merged, ok := applySearchRankedTestContract(results, plan, serializedSearchResultBytes(results)+2048)
	if !ok {
		t.Fatal("funded ranked test contract was not applied")
	}
	mentions := 0
	for _, result := range merged {
		if result.FilePath == "tests/contract_test.go" {
			mentions++
			if result.Section != searchSectionCoveringTest {
				t.Fatalf("promoted test retained primary classification: %#v", result)
			}
		}
	}
	if mentions != 1 {
		t.Fatalf("promoted test appears %d times, want once", mentions)
	}
	for _, want := range original {
		if want.FilePath == "tests/contract_test.go" {
			continue
		}
		found := false
		for _, got := range merged {
			if got.FilePath == want.FilePath && got.StartLine == want.StartLine {
				found = true
				copy := got
				copy.Rank = want.Rank
				if !reflect.DeepEqual(copy, want) {
					t.Fatalf("non-test tail changed:\n got %#v\nwant %#v", got, want)
				}
			}
		}
		if !found {
			t.Fatalf("non-test tail was removed: %#v", want)
		}
	}
	if growth := serializedSearchResultBytes(merged) - serializedSearchResultBytes(results); growth > 2048 {
		t.Fatalf("promotion grew payload by %d bytes, over one 2048-byte unit", growth)
	}
	if _, ok := applySearchRankedTestContract(results, plan, serializedSearchResultBytes(merged)-1); ok {
		t.Fatal("promotion displaced evidence instead of yielding under a tight ceiling")
	}
	changed := append([]SearchResult(nil), results...)
	changed[searchEnclosureHeadRanks].Snippet = "changed during enrichment"
	if _, ok := applySearchRankedTestContract(changed, plan, 1<<20); ok {
		t.Fatal("promotion matched a changed test artifact")
	}
}

func TestSearchRankedTestReservationAcceptsOnlyHeadSubsetsAndIdenticalTails(t *testing.T) {
	t.Parallel()
	ordinary := rankedTestContractResults(31)
	ordinary[0].StartLine, ordinary[0].EndLine = 1, 4
	ordinary[0].SnippetStartLine, ordinary[0].SnippetEndLine = 1, 4
	ordinary[0].Snippet = "one\ntwo\nthree\nfour"
	reserved := append([]SearchResult(nil), ordinary...)
	reserved[0].StartLine, reserved[0].EndLine = 2, 3
	reserved[0].SnippetStartLine, reserved[0].SnippetEndLine = 2, 3
	reserved[0].Snippet = "two\nthree"
	budget := serializedSearchResultBytes(reserved)
	if !searchRankedTestReservationIsSafe(ordinary, reserved, budget) {
		t.Fatal("truthful protected-head subset was rejected")
	}

	disjoint := append([]SearchResult(nil), reserved...)
	disjoint[0].StartLine, disjoint[0].EndLine = 5, 5
	disjoint[0].SnippetStartLine, disjoint[0].SnippetEndLine = 5, 5
	disjoint[0].Snippet = "five"
	if searchRankedTestReservationIsSafe(ordinary, disjoint, 1<<20) {
		t.Fatal("shifted/disjoint protected-head fallback was accepted")
	}

	mutatedTail := append([]SearchResult(nil), reserved...)
	mutatedTail[searchEnclosureHeadRanks].Snippet = "mutated tail"
	if searchRankedTestReservationIsSafe(ordinary, mutatedTail, 1<<20) {
		t.Fatal("tail mutation was accepted")
	}

	invented := append([]SearchResult(nil), reserved...)
	invented[0].Signals = append(append([]string(nil), invented[0].Signals...), searchCompleteSymbolSignal)
	if searchRankedTestReservationIsSafe(ordinary, invented, 1<<20) {
		t.Fatal("reservation invented complete-symbol")
	}

	repeated := append([]SearchResult(nil), ordinary...)
	repeated[0].StartLine, repeated[0].EndLine = 1, 3
	repeated[0].SnippetStartLine, repeated[0].SnippetEndLine = 1, 3
	repeated[0].Snippet = "same\nmiddle\nsame"
	wrongOffset := append([]SearchResult(nil), repeated...)
	wrongOffset[0].StartLine, wrongOffset[0].EndLine = 2, 2
	wrongOffset[0].SnippetStartLine, wrongOffset[0].SnippetEndLine = 2, 2
	wrongOffset[0].Snippet = "same"
	if searchRankedTestReservationIsSafe(repeated, wrongOffset, 1<<20) {
		t.Fatal("same text from the wrong reported line was accepted")
	}

	complete := append([]SearchResult(nil), ordinary...)
	complete[0].Signals = append(append([]string(nil), complete[0].Signals...), searchCompleteSymbolSignal)
	staleComplete := append([]SearchResult(nil), complete...)
	staleComplete[0].StartLine, staleComplete[0].EndLine = 2, 3
	staleComplete[0].SnippetStartLine, staleComplete[0].SnippetEndLine = 2, 3
	staleComplete[0].Snippet = "two\nthree"
	if searchRankedTestReservationIsSafe(complete, staleComplete, 1<<20) {
		t.Fatal("narrowed head retained stale complete-symbol")
	}
}

func TestPlanSearchRankedTestContractSupportsUnboundedOutput(t *testing.T) {
	t.Parallel()
	results := rankedTestContractResults(31)
	read := func(string) (string, bool) {
		return strings.Join(rankedTestContractLines(39), "\n"), true
	}
	plan, ok := planSearchRankedTestContract(
		results, results, buildSearchQuery("subject behaviour"), read, 0,
	)
	if !ok {
		t.Fatal("unbounded output disabled ranked contract planning")
	}
	if _, ok := applySearchRankedTestContract(results, plan, 0); !ok {
		t.Fatal("unbounded output disabled ranked contract application")
	}
}

func TestSearchRankedTestContractDoesNotCallClippedSmallArtifactWhole(t *testing.T) {
	t.Parallel()
	result := rankedTestContractResults(1)[searchEnclosureHeadRanks]
	result.FocusLine = 2
	lines := []string{strings.Repeat("a", 3000), "focus"}
	entry, ok := searchRankedTestContractResult(result, lines)
	if !ok {
		t.Fatal("bounded line from byte-heavy small artifact was not rendered")
	}
	if entry.SnippetStartLine != entry.StartLine || entry.SnippetEndLine != entry.EndLine ||
		!containsString(entry.Signals, searchHeadWindowSignal) {
		t.Fatalf("clipped small artifact masqueraded as whole: %#v", entry)
	}
}

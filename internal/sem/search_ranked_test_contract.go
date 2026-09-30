package sem

import (
	"reflect"
	"slices"
	"strings"
)

const searchRankedTestContractGrowthBytes = searchEnclosureGrowthBytes / searchEnclosureHeadRanks

type searchRankedTestContractPlan struct {
	entry  SearchResult
	ranked SearchResult
	delta  int
}

// selectSearchRankedTestContract promotes one test artifact the ranking already established as
// relevant when the parser cannot name its surrounding test callback. Only the unprotected primary
// tail is eligible: candidate fix sites keep their positions, and a caller explicitly searching for
// tests keeps the ordinary ranking it asked for.
func selectSearchRankedTestContract(
	results []SearchResult,
	q searchQuery,
	cache *searchRelatedFileCache,
) (SearchResult, SearchResult, bool) {
	if searchQuerySupplied(q, searchTestIntentWords...) {
		return SearchResult{}, SearchResult{}, false
	}
	for index := searchEnclosureHeadRanks; index < len(results); index++ {
		result := results[index]
		if !searchRankedTestContractCandidate(result) {
			continue
		}
		lines, ok := cache.get(result.FilePath)
		if !ok {
			continue
		}
		entry, ok := searchRankedTestContractResult(result, lines)
		if ok {
			return entry, result, true
		}
	}
	return SearchResult{}, SearchResult{}, false
}

func planSearchRankedTestContract(
	ordinary, floor []SearchResult,
	q searchQuery,
	read contentReader,
	hardBudget int,
) (searchRankedTestContractPlan, bool) {
	entry, ranked, ok := selectSearchRankedTestContract(
		ordinary, q, newSearchRelatedFileCache(read, 1),
	)
	if !ok {
		return searchRankedTestContractPlan{}, false
	}
	delta := serializedSearchResultBytes([]SearchResult{entry}) -
		serializedSearchResultBytes([]SearchResult{ranked})
	if delta < 0 {
		delta = 0
	}
	if delta > searchRankedTestContractGrowthBytes ||
		(hardBudget > 0 && serializedSearchResultBytes(floor)+delta > hardBudget) {
		return searchRankedTestContractPlan{}, false
	}
	return searchRankedTestContractPlan{entry: entry, ranked: ranked, delta: delta}, true
}

// searchRankedTestReservationIsSafe admits the lower-budget allocation only when every tail is
// exactly the ordinary allocation and every protected-head excerpt is a contiguous subset of it.
func searchRankedTestReservationIsSafe(ordinary, reserved []SearchResult, budget int) bool {
	if len(ordinary) != len(reserved) ||
		(budget > 0 && serializedSearchResultBytes(reserved) > budget) {
		return false
	}
	for index := range ordinary {
		before, after := ordinary[index], reserved[index]
		if index >= searchEnclosureHeadRanks {
			if !reflect.DeepEqual(before, after) {
				return false
			}
			continue
		}
		if before.Rank != after.Rank || before.FilePath != after.FilePath ||
			before.Score != after.Score || before.FocusLine != after.FocusLine ||
			before.SymbolID != after.SymbolID || before.SymbolName != after.SymbolName ||
			before.QualifiedName != after.QualifiedName || before.Kind != after.Kind ||
			before.Language != after.Language || before.Section != after.Section ||
			after.SnippetStartLine < before.SnippetStartLine ||
			after.SnippetEndLine > before.SnippetEndLine ||
			!searchRankedTestSnippetIsExactSubset(before, after) {
			return false
		}
		narrowed := after.SnippetStartLine > before.SnippetStartLine ||
			after.SnippetEndLine < before.SnippetEndLine
		if narrowed && (slices.Contains(after.Signals, searchCompleteSymbolSignal) ||
			slices.Contains(after.Signals, searchFullUnitSignal)) {
			return false
		}
		beforeShape, afterShape := before, after
		beforeShape.StartLine, beforeShape.EndLine = 0, 0
		beforeShape.SnippetStartLine, beforeShape.SnippetEndLine = 0, 0
		beforeShape.Snippet, beforeShape.Signals = "", nil
		afterShape.StartLine, afterShape.EndLine = 0, 0
		afterShape.SnippetStartLine, afterShape.SnippetEndLine = 0, 0
		afterShape.Snippet, afterShape.Signals = "", nil
		if !reflect.DeepEqual(beforeShape, afterShape) ||
			!searchRankedTestAllocationSignalsAreSafe(before.Signals, after.Signals) {
			return false
		}
	}
	return true
}

func searchRankedTestSnippetIsExactSubset(ordinary, reserved SearchResult) bool {
	if ordinary.Snippet == "" || reserved.Snippet == "" {
		return ordinary.Snippet == reserved.Snippet &&
			ordinary.SnippetStartLine == reserved.SnippetStartLine &&
			ordinary.SnippetEndLine == reserved.SnippetEndLine
	}
	lines := strings.Split(ordinary.Snippet, "\n")
	if len(lines) != ordinary.SnippetEndLine-ordinary.SnippetStartLine+1 {
		return ordinary.Snippet == reserved.Snippet &&
			ordinary.SnippetStartLine == reserved.SnippetStartLine &&
			ordinary.SnippetEndLine == reserved.SnippetEndLine
	}
	start := reserved.SnippetStartLine - ordinary.SnippetStartLine
	end := reserved.SnippetEndLine - ordinary.SnippetStartLine + 1
	if start < 0 || end > len(lines) || start >= end {
		return false
	}
	return reserved.Snippet == strings.Join(lines[start:end], "\n")
}

func searchRankedTestAllocationSignalsAreSafe(ordinary, reserved []string) bool {
	allocation := map[string]bool{
		searchCompleteSymbolSignal: true, searchFullUnitSignal: true,
		searchFullUnitElidedSignal: true, searchHeadWindowSignal: true,
	}
	strip := func(signals []string) []string {
		out := make([]string, 0, len(signals))
		for _, signal := range signals {
			if !allocation[signal] {
				out = append(out, signal)
			}
		}
		return out
	}
	return reflect.DeepEqual(strip(ordinary), strip(reserved)) &&
		(!slices.Contains(reserved, searchCompleteSymbolSignal) ||
			slices.Contains(ordinary, searchCompleteSymbolSignal)) &&
		(!slices.Contains(reserved, searchFullUnitSignal) ||
			slices.Contains(ordinary, searchFullUnitSignal)) &&
		(!slices.Contains(reserved, searchFullUnitElidedSignal) ||
			slices.Contains(reserved, searchFullUnitSignal))
}

func applySearchRankedTestContract(
	results []SearchResult,
	plan searchRankedTestContractPlan,
	hardBudget int,
) ([]SearchResult, bool) {
	match := -1
	want := plan.ranked
	want.Rank = 0
	for index, result := range results {
		candidate := result
		candidate.Rank = 0
		if reflect.DeepEqual(candidate, want) {
			match = index
			break
		}
	}
	if match < 0 {
		return results, false
	}
	out := make([]SearchResult, 0, len(results))
	out = append(out, results[:match]...)
	out = append(out, results[match+1:]...)
	out = append(out, plan.entry)
	for index := range out {
		out[index].Rank = index + 1
	}
	if hardBudget > 0 && serializedSearchResultBytes(out) > hardBudget {
		return results, false
	}
	return out, true
}

func searchRankedTestContractCandidate(result SearchResult) bool {
	if result.Section != searchSectionPrimary || result.Score <= 0 ||
		!searchTestArtifactPath(searchLowerPath(result.FilePath)) {
		return false
	}
	if result.SymbolID != "" || result.SymbolName != "" || result.QualifiedName != "" {
		return false
	}
	return searchRankedTestHasContentEvidence(result)
}

func searchRankedTestHasContentEvidence(result SearchResult) bool {
	if result.Snippet == "" {
		return false
	}
	for _, signal := range result.Signals {
		if signal != "" && signal != "path" {
			return true
		}
	}
	return false
}

// searchRankedTestContractResult renders a whole artifact up to the established 60-line read
// bound, or an honestly labelled focus window when line or byte bounds require clipping.
func searchRankedTestContractResult(result SearchResult, lines []string) (SearchResult, bool) {
	if len(lines) == 0 || result.FilePath == "" {
		return SearchResult{}, false
	}
	focus := result.FocusLine
	if focus <= 0 {
		focus = result.SnippetStartLine
	}
	if focus <= 0 || focus > len(lines) {
		return SearchResult{}, false
	}
	start, end := 1, len(lines)
	windowed := end-start+1 > searchHeadWindowLines
	if windowed {
		start, end = clipSearchUnitToCap(start, end, focus, searchHeadWindowLines)
	}
	build := func(from, to int, clipped bool) SearchResult {
		signals := []string{searchRelatedSignalPrefix + searchCoveringTestSignal}
		if clipped {
			signals = append(signals, searchHeadWindowSignal)
		}
		return SearchResult{
			Rank: result.Rank, Score: result.Score, FilePath: result.FilePath,
			StartLine: from, EndLine: to, FocusLine: focus,
			SnippetStartLine: from, SnippetEndLine: to,
			Language: result.Language, Signals: signals, Section: searchSectionCoveringTest,
			Snippet: strings.Join(lines[from-1:to], "\n"),
		}
	}
	budget := serializedSearchResultBytes([]SearchResult{result}) + searchRankedTestContractGrowthBytes
	for span := end - start + 1; span >= 1; span-- {
		from, to := focusedSnippetRegion(start, end, focus, span)
		candidate := build(from, to, windowed || from != start || to != end)
		if serializedSearchResultBytes([]SearchResult{candidate}) <= budget {
			return candidate, true
		}
	}
	return SearchResult{}, false
}

package sem

import (
	"fmt"
	"sort"
	"strings"
)

const (
	searchContainedCallablesComplete  = "complete"
	searchContainedCallablesTruncated = "truncated"
)

type searchContainedCallablePlan struct {
	owner int
	value SearchContainedCallable
}

func searchContainedCallableResultBudget(contextBudget, fundedBlockBytes int) int {
	if contextBudget <= 0 {
		return 0
	}
	return maxInt(1, contextBudget-fundedBlockBytes)
}

// attachSearchContainedCallables adds typed identity for complete callable declarations whose
// source is already present in a result's primary snippet. Passages are intentionally excluded:
// they are prose context rather than the ranked source enclosure owned by this manifest. This runs
// after every result-shaping pass. The allocator only admits additive metadata that fits the
// remaining hard result-byte ceiling; it never changes, shrinks, reorders, or displaces primary
// evidence.
func attachSearchContainedCallables(
	results []SearchResult,
	symbolsByFile map[string][]SymbolRecord,
	hardBudget int,
) ([]SearchResult, string, int, int, []SearchContainedCallableDrop) {
	plans := planSearchContainedCallables(results, symbolsByFile)
	if len(plans) == 0 {
		return results, searchContainedCallablesComplete, 0, 0, nil
	}
	for emitted := len(plans); emitted >= 0; emitted-- {
		candidate, droppedByResult := applySearchContainedCallablePrefix(results, plans, emitted)
		if hardBudget <= 0 || serializedSearchResultBytes(candidate) <= hardBudget {
			dropped := len(plans) - emitted
			status := searchContainedCallablesComplete
			if dropped > 0 {
				status = searchContainedCallablesTruncated
			}
			return candidate, status, emitted, dropped, droppedByResult
		}
	}
	panic("zero-item contained callable manifest must preserve the baseline result bytes")
}

func planSearchContainedCallables(
	results []SearchResult,
	symbolsByFile map[string][]SymbolRecord,
) []searchContainedCallablePlan {
	topLevel := make(map[string]bool, len(results))
	for _, result := range results {
		if result.SymbolID != "" {
			topLevel[result.SymbolID] = true
		}
	}
	owners := make([]int, len(results))
	for index := range results {
		owners[index] = index
	}
	sort.SliceStable(owners, func(i, j int) bool {
		left, right := results[owners[i]], results[owners[j]]
		if left.Rank != right.Rank {
			return left.Rank < right.Rank
		}
		return owners[i] < owners[j]
	})
	seen := make(map[string]bool)
	var plans []searchContainedCallablePlan
	for _, owner := range owners {
		result := results[owner]
		if !searchResultHasRenderedContiguousSnippet(result) {
			continue
		}
		symbols := append([]SymbolRecord(nil), symbolsByFile[result.FilePath]...)
		sort.SliceStable(symbols, func(i, j int) bool {
			if symbols[i].StartLine != symbols[j].StartLine {
				return symbols[i].StartLine < symbols[j].StartLine
			}
			if symbols[i].EndLine != symbols[j].EndLine {
				return symbols[i].EndLine < symbols[j].EndLine
			}
			return symbols[i].ID < symbols[j].ID
		})
		for _, symbol := range symbols {
			if symbol.ID == "" || symbol.Name == "" || symbol.QualifiedName == "" || symbol.Kind == "" ||
				symbol.FilePath != result.FilePath || topLevel[symbol.ID] || seen[symbol.ID] ||
				!searchEnclosableSymbolKind(symbol.Kind) || symbol.StartLine <= 0 ||
				symbol.EndLine < symbol.StartLine || symbol.StartLine < result.SnippetStartLine ||
				symbol.EndLine > result.SnippetEndLine {
				continue
			}
			seen[symbol.ID] = true
			plans = append(plans, searchContainedCallablePlan{
				owner: owner,
				value: SearchContainedCallable{
					SymbolID: symbol.ID, SymbolName: symbol.Name, QualifiedName: symbol.QualifiedName,
					Kind: symbol.Kind, Language: symbol.Language,
					StartLine: symbol.StartLine, EndLine: symbol.EndLine,
				},
			})
		}
	}
	return plans
}

func searchResultHasRenderedContiguousSnippet(result SearchResult) bool {
	if result.Snippet == "" || strings.TrimSpace(result.Snippet) == "... truncated ..." ||
		result.SnippetStartLine <= 0 || result.SnippetEndLine < result.SnippetStartLine {
		return false
	}
	return len(strings.Split(result.Snippet, "\n")) == result.SnippetEndLine-result.SnippetStartLine+1
}

func applySearchContainedCallablePrefix(
	results []SearchResult,
	plans []searchContainedCallablePlan,
	emitted int,
) ([]SearchResult, []SearchContainedCallableDrop) {
	out := append([]SearchResult(nil), results...)
	for index := range out {
		out[index].ContainedCallables = nil
	}
	droppedByOwner := make(map[int]int)
	for index, plan := range plans {
		if index < emitted {
			out[plan.owner].ContainedCallables = append(out[plan.owner].ContainedCallables, plan.value)
			continue
		}
		droppedByOwner[plan.owner]++
	}
	ledger := make([]SearchContainedCallableDrop, 0, len(droppedByOwner))
	for owner, result := range out {
		if dropped := droppedByOwner[owner]; dropped > 0 {
			ledger = append(ledger, SearchContainedCallableDrop{Rank: result.Rank, Dropped: dropped})
		}
	}
	sort.SliceStable(ledger, func(i, j int) bool { return ledger[i].Rank < ledger[j].Rank })
	return out, ledger
}

func validateSearchContainedCallables(response SearchResponse) error {
	topLevel := make(map[string]bool, len(response.Results))
	for _, result := range response.Results {
		if result.SymbolID != "" {
			topLevel[result.SymbolID] = true
		}
	}
	emitted := 0
	seen := make(map[string]bool)
	for _, result := range response.Results {
		var prior *SearchContainedCallable
		for index := range result.ContainedCallables {
			callable := result.ContainedCallables[index]
			if !searchResultHasRenderedContiguousSnippet(result) {
				return fmt.Errorf("contained callable at rank %d has no exact rendered snippet", result.Rank)
			}
			if callable.SymbolID == "" || callable.SymbolName == "" || callable.QualifiedName == "" ||
				callable.Kind == "" || !searchEnclosableSymbolKind(callable.Kind) || callable.StartLine <= 0 ||
				callable.EndLine < callable.StartLine || callable.StartLine < result.SnippetStartLine ||
				callable.EndLine > result.SnippetEndLine {
				return fmt.Errorf("invalid contained callable at rank %d index %d", result.Rank, index)
			}
			if topLevel[callable.SymbolID] {
				return fmt.Errorf("contained callable %q duplicates a top-level result identity", callable.SymbolID)
			}
			if seen[callable.SymbolID] {
				return fmt.Errorf("duplicate contained callable identity %q", callable.SymbolID)
			}
			if prior != nil && (callable.StartLine < prior.StartLine ||
				(callable.StartLine == prior.StartLine && callable.EndLine < prior.EndLine) ||
				(callable.StartLine == prior.StartLine && callable.EndLine == prior.EndLine && callable.SymbolID <= prior.SymbolID)) {
				return fmt.Errorf("contained callables at rank %d are not in stable range and identity order", result.Rank)
			}
			seen[callable.SymbolID] = true
			prior = &result.ContainedCallables[index]
			emitted++
		}
	}
	if response.Stats.ContainedCallablesEmitted != emitted {
		return fmt.Errorf("contained callable emitted count mismatch: %d != %d", response.Stats.ContainedCallablesEmitted, emitted)
	}
	dropped, priorRank := 0, 0
	seenRanks := make(map[int]bool, len(response.Stats.ContainedCallablesDroppedByResult))
	for index, entry := range response.Stats.ContainedCallablesDroppedByResult {
		if entry.Rank <= priorRank || entry.Rank < 1 || entry.Rank > len(response.Results) ||
			entry.Dropped <= 0 || seenRanks[entry.Rank] {
			return fmt.Errorf("invalid contained callable omission ledger entry at index %d", index)
		}
		seenRanks[entry.Rank] = true
		priorRank = entry.Rank
		dropped += entry.Dropped
	}
	if response.Stats.ContainedCallablesDroppedByBudget != dropped {
		return fmt.Errorf("contained callable dropped count mismatch: %d != %d", response.Stats.ContainedCallablesDroppedByBudget, dropped)
	}
	switch response.Stats.ContainedCallablesStatus {
	case searchContainedCallablesComplete:
		if dropped != 0 || len(response.Stats.ContainedCallablesDroppedByResult) != 0 {
			return fmt.Errorf("complete contained callable manifest reports %d dropped", dropped)
		}
	case searchContainedCallablesTruncated:
		if dropped == 0 || len(response.Stats.ContainedCallablesDroppedByResult) == 0 {
			return fmt.Errorf("truncated contained callable manifest has no attributed omissions")
		}
	default:
		return fmt.Errorf("unknown contained callable status %q", response.Stats.ContainedCallablesStatus)
	}
	return nil
}

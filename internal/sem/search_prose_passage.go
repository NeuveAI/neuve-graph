package sem

import (
	"fmt"
	"sort"
)

// planProseParentPassages keeps distant same-session regions that the one-result-per-parent
// ranking would otherwise discard. Query-term marginal coverage wins first; the existing
// candidate score and deterministic candidate ordering break ties.
func planProseParentPassages(
	parent *proseParent,
	primary searchCandidate,
	q searchQuery,
	limit int,
) []SearchPassage {
	return planProsePassages(parent, &primary, q, limit)
}

func planCompleteProseDocumentPassages(
	parent *proseParent,
	q searchQuery,
	limit int,
) []SearchPassage {
	return planProsePassages(parent, nil, q, limit)
}

func planProsePassages(
	parent *proseParent,
	primary *searchCandidate,
	q searchQuery,
	limit int,
) []SearchPassage {
	if parent == nil || limit <= 0 || len(parent.candidates) == 0 {
		return nil
	}
	terms := proseParentQueryTerms(q)
	covered := map[string]bool{}
	var primaryPassage SearchPassage
	if primary != nil {
		covered = proseCandidateTermCoverage(*primary, terms)
		var ok bool
		primaryPassage, ok = searchCandidatePassage(*primary)
		if !ok {
			return nil
		}
	}
	type option struct {
		candidate *searchCandidate
		passage   SearchPassage
		coverage  map[string]bool
	}
	candidateLimit := minInt(len(parent.candidates), limit)
	options := make([]option, 0, candidateLimit)
	// parent.candidates follows the stable global merit order. Only the bounded
	// consumable prefix participates in the quadratic marginal-coverage planner;
	// scanning an unbounded remainder cannot add a passage the plan is allowed to
	// return.
	for _, candidate := range parent.candidates[:candidateLimit] {
		if candidate == nil {
			continue
		}
		passage, valid := searchCandidatePassage(*candidate)
		if !valid || (primary != nil && passagesOverlap(primaryPassage, passage)) {
			continue
		}
		options = append(options, option{
			candidate: candidate,
			passage:   passage,
			coverage:  proseCandidateTermCoverage(*candidate, terms),
		})
	}
	sort.SliceStable(options, func(left, right int) bool {
		return searchCandidateScoreLess(*options[left].candidate, *options[right].candidate)
	})
	planned := make([]SearchPassage, 0, minInt(limit, len(options)))
	selectedPassages := make([]SearchPassage, 0, minInt(limit, len(options)))
	selectedOptions := make([]bool, len(options))
	// Marginal coverage can improve only while one of the query's bounded terms
	// remains unseen. That limits full rescoring to at most len(terms) rounds;
	// after coverage is exhausted, the remaining order is ordinary candidate
	// merit and can be consumed in one linear pass.
	for len(planned) < limit && len(covered) < len(terms) {
		best, bestMarginal := -1, 0
		for index, candidate := range options {
			if selectedOptions[index] || prosePassageOverlapsSorted(selectedPassages, candidate.passage) {
				continue
			}
			marginal := 0
			for term := range candidate.coverage {
				if !covered[term] {
					marginal++
				}
			}
			if marginal > bestMarginal || (marginal > 0 && marginal == bestMarginal &&
				(best < 0 || searchCandidateScoreLess(*candidate.candidate, *options[best].candidate))) {
				best, bestMarginal = index, marginal
			}
		}
		if best < 0 || bestMarginal == 0 {
			break
		}
		selectedOptions[best] = true
		planned = append(planned, options[best].passage)
		selectedPassages = insertProsePassageSorted(selectedPassages, options[best].passage)
		for term := range options[best].coverage {
			covered[term] = true
		}
	}
	for index, candidate := range options {
		if len(planned) == limit {
			break
		}
		if selectedOptions[index] || prosePassageOverlapsSorted(selectedPassages, candidate.passage) {
			continue
		}
		planned = append(planned, candidate.passage)
		selectedPassages = insertProsePassageSorted(selectedPassages, candidate.passage)
	}
	return planned
}

func prosePassageOverlapsSorted(passages []SearchPassage, candidate SearchPassage) bool {
	index := sort.Search(len(passages), func(index int) bool {
		return passages[index].StartLine >= candidate.StartLine
	})
	return (index > 0 && passagesOverlap(passages[index-1], candidate)) ||
		(index < len(passages) && passagesOverlap(passages[index], candidate))
}

func insertProsePassageSorted(passages []SearchPassage, passage SearchPassage) []SearchPassage {
	index := sort.Search(len(passages), func(index int) bool {
		return passages[index].StartLine >= passage.StartLine
	})
	passages = append(passages, SearchPassage{})
	copy(passages[index+1:], passages[index:])
	passages[index] = passage
	return passages
}

func searchCandidatePassage(candidate searchCandidate) (SearchPassage, bool) {
	result := candidate.result
	start, end := result.SnippetStartLine, result.SnippetEndLine
	if start <= 0 || end < start || result.Snippet == "" {
		return SearchPassage{}, false
	}
	focus := result.FocusLine
	if focus < start || focus > end {
		focus = start
	}
	return SearchPassage{
		StartLine: start,
		EndLine:   end,
		FocusLine: focus,
		Snippet:   result.Snippet,
	}, true
}

func proseCandidateTermCoverage(candidate searchCandidate, terms []string) map[string]bool {
	coverage := make(map[string]bool, len(terms))
	for _, term := range terms {
		if candidate.termCounts[term] > 0 {
			coverage[term] = true
		}
	}
	if len(coverage) == len(terms) {
		return coverage
	}
	for _, token := range proseASCIITokens(candidate.result.Snippet + "\n" + candidate.result.Signature) {
		for _, term := range terms {
			if !coverage[term] && safeProseInflectionMatch(term, token) {
				coverage[term] = true
			}
		}
	}
	return coverage
}

func passagesOverlap(left, right SearchPassage) bool {
	return left.StartLine <= right.EndLine && right.StartLine <= left.EndLine
}

func primarySearchPassage(result SearchResult) SearchPassage {
	return SearchPassage{
		StartLine: result.SnippetStartLine,
		EndLine:   result.SnippetEndLine,
		FocusLine: result.FocusLine,
		Snippet:   result.Snippet,
	}
}

// allocateProseParentPassages spends remaining result bytes round-robin by passage depth and
// parent rank. A hard caller ceiling is exact; without one, growth stays at the existing
// conservative snippet allowance.
func allocateProseParentPassages(
	results []SearchResult,
	plans map[int][]SearchPassage,
	hardBudget int,
) ([]SearchResult, int, int) {
	if len(results) == 0 || len(plans) == 0 {
		return results, 0, 0
	}
	allocated := append([]SearchResult(nil), results...)
	for index := range allocated {
		allocated[index].Passages = append([]SearchPassage(nil), results[index].Passages...)
	}
	baseline := serializedSearchResultBytes(allocated)
	budget := baseline + searchEnclosureGrowthBytes
	if hardBudget > 0 {
		budget = hardBudget
	}
	if baseline > budget {
		return results, 0, 0
	}
	count, passageBytes := 0, 0
	for depth := 0; ; depth++ {
		sawDepth := false
		for index := range allocated {
			candidates := plans[allocated[index].Rank]
			if depth >= len(candidates) {
				continue
			}
			sawDepth = true
			passage := candidates[depth]
			if passage.StartLine <= 0 || passage.EndLine < passage.StartLine || passage.Snippet == "" ||
				passagesOverlap(primarySearchPassage(allocated[index]), passage) {
				continue
			}
			overlaps := false
			for _, existing := range allocated[index].Passages {
				if passagesOverlap(existing, passage) {
					overlaps = true
					break
				}
			}
			if overlaps {
				continue
			}
			trial := append([]SearchResult(nil), allocated...)
			trial[index].Passages = append(append([]SearchPassage(nil), allocated[index].Passages...), passage)
			sort.Slice(trial[index].Passages, func(left, right int) bool {
				return trial[index].Passages[left].StartLine < trial[index].Passages[right].StartLine
			})
			if serializedSearchResultBytes(trial) > budget {
				continue
			}
			allocated = trial
			count++
			passageBytes += serializedSearchResultBytes(passage)
		}
		if !sawDepth {
			break
		}
	}
	return allocated, count, passageBytes
}

func validateSearchResultPassages(result SearchResult) error {
	primary := primarySearchPassage(result)
	for index, passage := range result.Passages {
		if passage.StartLine <= 0 || passage.EndLine < passage.StartLine ||
			passage.FocusLine < passage.StartLine || passage.FocusLine > passage.EndLine ||
			passage.Snippet == "" {
			return fmt.Errorf("invalid prose passage %d", index)
		}
		if passagesOverlap(primary, passage) {
			return fmt.Errorf("prose passage %d overlaps primary snippet", index)
		}
		if index > 0 {
			previous := result.Passages[index-1]
			if passage.StartLine <= previous.StartLine {
				return fmt.Errorf("prose passages are not source ordered")
			}
			if passagesOverlap(previous, passage) {
				return fmt.Errorf("prose passages overlap")
			}
		}
	}
	return nil
}

func searchPassageStats(results []SearchResult) (int, int) {
	count, bytes := 0, 0
	for _, result := range results {
		for _, passage := range result.Passages {
			count++
			bytes += serializedSearchResultBytes(passage)
		}
	}
	return count, bytes
}

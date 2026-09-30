package sem

// MULTI-RESOLUTION RETRIEVAL
//
// Ranking can finish with fewer base primary regions than TopK after unit dedupe, file diversity,
// containment removal, and byte fitting. For prose, one markdown file is one document — a design
// note, a meeting log, a chat session — and the answer to a question about it is routinely spread
// across several distant regions of the SAME file. For those files the ranker already computes
// extra regions
// and attaches them to their parent as `passages` (search_prose_passage.go), but a consumer that
// reads `results[]` and nothing else never sees them: the parent result carries one span, and the
// rest of the evidence sits in a field it did not ask about.
//
// So when the base ranking and any explicitly requested primary enrichment leave capacity below
// TopK, the leftover slots are spent promoting those already-computed regions to first-class
// results. Two
// resolutions of the same document are then returned side by side: the parent's own span, which
// keeps the surrounding context, and the finer spans, which are the precise evidence.
//
// The rule that keeps this from becoming `--deep` (which traded breadth for depth and measured
// worse) is that promotion is STRICTLY ADDITIVE: it runs after selection, only into slots the
// pre-supplement primary block left empty, and only within the byte ceiling the caller set. It can
// never displace a file. TopK bounds the base ranking rather than supplements or requested primary
// enrichment; promotions only spend capacity that remains after those primary rows. When the
// primary block already fills that capacity, the output is byte-identical to single-resolution.
//
// Ordinary code search is untouched: no code result carries passages, so there is nothing to
// promote.

const proseResolutionSignal = "retrieval_mode=prose-passage"

// prosePromotion identifies one passage of one parent result.
type prosePromotion struct {
	parent  int
	passage int
}

// planProseResolutionPromotions constructs the complete promotion order before
// TopK projects it. Round-robin depth prevents one long document from spending
// every spare slot. A passage whose text the payload already prints is skipped.
func planProseResolutionPromotions(results []SearchResult) []prosePromotion {
	var order []prosePromotion
	deepest := 0
	for index := range results {
		deepest = maxInt(deepest, len(results[index].Passages))
	}
	for depth := 0; depth < deepest; depth++ {
		for index := range results {
			if depth >= len(results[index].Passages) {
				continue
			}
			if prosePassageAlreadyPrinted(results, results[index].FilePath, results[index].Passages[depth]) {
				continue
			}
			order = append(order, prosePromotion{parent: index, passage: depth})
		}
	}
	return order
}

// prosePassageAlreadyPrinted reports whether the passage's span lies entirely inside the PRINTED
// span of some result from the same file, so promoting it would return its text twice.
//
// This is the promotion-side counterpart of dropContainedProseResults, and it exists for the same
// reason: dropSelectedProsePassages keeps passages disjoint from the ranked spans as SELECTED, and
// allocateProseParentPassages re-checks a passage only against its OWN parent's grown window — a
// window grown on a DIFFERENT result of the same file is checked by neither, so a passage planned
// for a distant section can sit inside the head result's grown window. The passage stays attached
// to its parent (that field's bytes were already fitted); it just cannot spend a result slot on
// lines the payload already shows.
func prosePassageAlreadyPrinted(results []SearchResult, path string, passage SearchPassage) bool {
	for index := range results {
		result := results[index]
		if result.FilePath != path || result.SnippetStartLine <= 0 || result.SnippetEndLine < result.SnippetStartLine {
			continue
		}
		if result.SnippetStartLine <= passage.StartLine && passage.EndLine <= result.SnippetEndLine {
			return true
		}
	}
	return false
}

// applyProseResolutionPromotions materializes the first `count` planned promotions. The promoted
// passages leave their parent, so no region is returned twice, and the parents keep the passages
// that were not promoted (a suffix of a source-ordered, non-overlapping list stays source-ordered
// and non-overlapping, which is what SearchResponse.Validate requires).
func applyProseResolutionPromotions(results []SearchResult, order []prosePromotion, count int) []SearchResult {
	promotedFrom := make([]map[int]bool, len(results))
	for _, promotion := range order[:count] {
		if promotedFrom[promotion.parent] == nil {
			promotedFrom[promotion.parent] = map[int]bool{}
		}
		promotedFrom[promotion.parent][promotion.passage] = true
	}
	expanded := make([]SearchResult, 0, len(results)+count)
	for index := range results {
		parent := results[index]
		if promotedFrom[index] != nil {
			kept := make([]SearchPassage, 0, len(parent.Passages))
			for passageIndex, passage := range parent.Passages {
				if !promotedFrom[index][passageIndex] {
					kept = append(kept, passage)
				}
			}
			if len(kept) == 0 {
				kept = nil
			}
			parent.Passages = kept
		}
		expanded = append(expanded, parent)
	}
	for _, promotion := range order[:count] {
		promoted := proseResolutionResult(results[promotion.parent], results[promotion.parent].Passages[promotion.passage])
		expanded = append(expanded, promoted)
	}
	// Rank is the policy order: retain the selected primaries byte-for-byte, then append the
	// independently planned promotion suffix. Score remains each region's underlying retrieval
	// relevance and is deliberately not a second final-order key.
	for index := range expanded {
		expanded[index].Rank = index + 1
	}
	return expanded
}

func proseResolutionPrimaryPrefix(results []SearchResult) ([]SearchResult, []SearchResult) {
	for index, result := range results {
		if result.Section == searchSectionRelated || result.Section == searchSectionCoveringTest {
			return results[:index], results[index:]
		}
	}
	return results, nil
}

func appendProseResolutionSupplements(primaries, supplements []SearchResult) []SearchResult {
	if len(supplements) == 0 {
		return primaries
	}
	combined := make([]SearchResult, 0, len(primaries)+len(supplements))
	combined = append(combined, primaries...)
	combined = append(combined, supplements...)
	for index := range combined {
		combined[index].Rank = index + 1
	}
	return combined
}

// proseResolutionResult turns one passage into a result. It inherits its parent's file, language
// and score — it is the same document and the same relevance evidence at a finer resolution, and a
// score invented for it here would be a number no ranking produced. Symbol identity is dropped:
// the passage is a region of the document, not a definition of the parent's symbol, and claiming
// otherwise would make two results advertise the same symbol at different lines.
func proseResolutionResult(parent SearchResult, passage SearchPassage) SearchResult {
	signals := make([]string, 0, len(parent.Signals)+1)
	signals = append(signals, parent.Signals...)
	return SearchResult{
		Score:            parent.Score,
		FilePath:         parent.FilePath,
		StartLine:        passage.StartLine,
		EndLine:          passage.EndLine,
		FocusLine:        passage.FocusLine,
		SnippetStartLine: passage.StartLine,
		SnippetEndLine:   passage.EndLine,
		Language:         parent.Language,
		Signals:          appendUnique(signals, proseResolutionSignal),
		Section:          parent.Section,
		Snippet:          passage.Snippet,
	}
}

// expandProseResolution spends the TopK capacity the pre-supplement primary block left empty on
// the finer regions it already computed, without breaching the caller's byte ceiling. Promotions
// are inserted before the related/covering supplement suffix.
//
// `reserved` is the bytes the reference blocks funded from that SAME ceiling have already spent —
// the declaration card and the signature-type block. Both live OUTSIDE `results`, and
// SearchResponse.Validate checks their SUM against the ceiling
// (result_bytes + type_card_bytes + signature_type_bytes <= context_budget_bytes). Measuring
// promotion against the whole ceiling therefore let the combined payload overshoot it, and the
// response was then rejected by its own validator: with either block on and a tight
// --max-context-bytes the search command failed outright instead of returning results, which is
// the worst possible outcome for a caller who asked for a SMALLER answer.
func expandProseResolution(results []SearchResult, topK, budget, reserved int) []SearchResult {
	primaries, supplements := proseResolutionPrimaryPrefix(results)
	if topK <= 0 || len(primaries) >= topK {
		return results
	}
	order := planProseResolutionPromotions(primaries)
	if len(order) == 0 {
		return results
	}
	order = order[:minInt(len(order), topK-len(primaries))]
	apply := func(count int) []SearchResult {
		expanded := applyProseResolutionPromotions(primaries, order, count)
		return appendProseResolutionSupplements(expanded, supplements)
	}
	if budget <= 0 {
		return apply(len(order))
	}
	// `budget <= 0` above means "no ceiling at all"; an available allowance of zero or less means
	// the opposite — the blocks already spent the ceiling, so nothing may be promoted.
	available := budget - maxInt(0, reserved)
	if available <= 0 {
		return results
	}
	// Serialized size is strictly increasing in count: a promotion always moves a passage out of a
	// `passages` entry and into a full result record, which is strictly larger. The largest fitting
	// count is therefore found by bisection in ~log2(n) serializations rather than by materializing
	// and marshalling the whole result set once per candidate count.
	low, high := 0, len(order)
	for low < high {
		mid := (low + high + 1) / 2
		if serializedSearchResultBytes(apply(mid)) <= available {
			low = mid
		} else {
			high = mid - 1
		}
	}
	if low == 0 {
		return results
	}
	return apply(low)
}

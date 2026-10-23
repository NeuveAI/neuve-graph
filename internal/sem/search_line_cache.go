package sem

import "strings"

// searchLineCache remembers, for one file and one query, what each line
// contributes to ranking: its term counts and length, its focus weight, and
// whether it matches the query. candidatesForFile scores overlapping regions
// (the file, its types, their members, regions around hits), so without it
// the same lines are tokenized and matched again for every region holding
// them, which on a broad search is most of the ranking time.
//
// It answers exactly what the uncached helpers answer, because each of them
// is a function of single lines: searchSourceWordPattern never matches a
// line break, so the words of a region's text (its lines joined by LF) are
// the words of its lines in order, and searchTermCounts over the region is
// the sum of its lines' counts and lengths; searchFocusLine and
// textMatchesSearchQuery look at one line at a time.
type searchLineCache struct {
	q       searchQuery
	lines   []string
	counts  []map[string]int
	lengths []int
	weights []float64
	matches []bool
	done    []uint8
}

const (
	searchLineCounted uint8 = 1 << iota
	searchLineWeighted
	searchLineMatched
)

func newSearchLineCache(q searchQuery, lines []string) *searchLineCache {
	return &searchLineCache{
		q:       q,
		lines:   lines,
		counts:  make([]map[string]int, len(lines)),
		lengths: make([]int, len(lines)),
		weights: make([]float64, len(lines)),
		matches: make([]bool, len(lines)),
		done:    make([]uint8, len(lines)),
	}
}

// termCounts is searchTermCounts(strings.Join(lines[start-1:end], "\n"), q).
func (c *searchLineCache) termCounts(start, end int) (map[string]int, int) {
	counts := map[string]int{}
	length := 0
	for index := start - 1; index < end; index++ {
		if c.done[index]&searchLineCounted == 0 {
			c.counts[index], c.lengths[index] = searchTermCounts(c.lines[index], c.q)
			c.done[index] |= searchLineCounted
		}
		for term, n := range c.counts[index] {
			counts[term] += n
		}
		length += c.lengths[index]
	}
	return counts, length
}

// focusLine is searchFocusLine(q, lines, start, end).
func (c *searchLineCache) focusLine(start, end int) int {
	bestLine := start
	bestMatches := 0.0
	for line := start; line <= end; line++ {
		index := line - 1
		if c.done[index]&searchLineWeighted == 0 {
			c.weights[index] = searchLineFocusWeight(c.q, c.lines[index])
			c.done[index] |= searchLineWeighted
		}
		if c.weights[index] > bestMatches {
			bestLine, bestMatches = line, c.weights[index]
		}
	}
	return bestLine
}

// matchesLine is textMatchesSearchQuery(q, lines[line-1]).
func (c *searchLineCache) matchesLine(line int) bool {
	index := line - 1
	if c.done[index]&searchLineMatched == 0 {
		c.matches[index] = textMatchesSearchQuery(c.q, c.lines[index])
		c.done[index] |= searchLineMatched
	}
	return c.matches[index]
}

// searchLineFocusWeight is the weight searchFocusLine gives one line: the
// sum, in query order, of the weights of the terms it matches.
//
// searchQueryTermMatches answers an inferred abbreviation by splitting every
// word of the line and asking whether one of its parts is that alias; the
// aliases the line's words reach are the same for every term, so they are
// gathered once and each abbreviation term is a lookup.
func searchLineFocusWeight(q searchQuery, line string) float64 {
	lower := strings.ToLower(line)
	var aliases map[string]bool
	matches := 0.0
	for _, term := range q.terms {
		var matched bool
		if q.inferredAbbreviations[term] {
			if aliases == nil {
				aliases = searchLineAliases(q, line)
			}
			matched = aliases[term]
		} else {
			matched = strings.Contains(lower, term)
		}
		if matched {
			matches += q.weights[term]
		}
	}
	return matches
}

// searchLineAliases is every alias the words of line reach through
// q.aliasTokens, the set searchQueryTermMatches tests one term against.
func searchLineAliases(q searchQuery, line string) map[string]bool {
	aliases := map[string]bool{}
	for _, raw := range searchSourceWords(line) {
		for _, token := range searchTokenVariants(raw) {
			for _, alias := range q.aliasTokens[token] {
				aliases[alias] = true
			}
		}
	}
	return aliases
}

package sem

import (
	"slices"
	"strings"
	"unsafe"
)

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
	q     searchQuery
	lines []string
	// tokens[line] is that line's constraint tokens, split when first asked
	// for (constraintTokens).
	tokens []*searchLineTokens
	// parts holds the constraint tokens of a candidate's other parts (its
	// symbol's name, qualified name and signature, its aliases), by the
	// string's bytes in memory: one symbol's signature, which can be a whole
	// declaration, is shared by every candidate of that symbol.
	parts   map[searchPartKey]*searchLineTokens
	counts  []map[string]int
	lengths []int
	weights []float64
	matches []bool
	done    []uint8
	// entityWanted maps each normalized entity of entityKey's constraints to
	// its indexes (entityHits).
	entityKey    searchEntityKey
	entityWanted map[string][]int
}

// searchEntityKey names a constraints' entity slice: the same slice, not
// merely an equal one.
type searchEntityKey struct {
	first *searchEntityConstraint
	count int
}

func searchEntityKeyOf(entities []searchEntityConstraint) searchEntityKey {
	if len(entities) == 0 {
		return searchEntityKey{}
	}
	return searchEntityKey{first: &entities[0], count: len(entities)}
}

// entityHits is the indexes of entities whose normalized form is one of
// piece's words or tokens, found once per piece and entity slice.
func (c *searchLineCache) entityHits(piece *searchLineTokens, entities []searchEntityConstraint) []int {
	key := searchEntityKeyOf(entities)
	if piece.entitiesDone && piece.entityKey == key {
		return piece.entityHits
	}
	if c.entityWanted == nil || c.entityKey != key {
		c.entityKey, c.entityWanted = key, make(map[string][]int, len(entities))
		for index, entity := range entities {
			c.entityWanted[entity.Normalized] = append(c.entityWanted[entity.Normalized], index)
		}
	}
	var hits []int
	for _, tokens := range [][]string{piece.words, piece.raw} {
		for _, token := range tokens {
			for _, index := range c.entityWanted[token] {
				if !slices.Contains(hits, index) {
					hits = append(hits, index)
				}
			}
		}
	}
	piece.entityKey, piece.entityHits, piece.entitiesDone = key, hits, true
	return hits
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

// searchLineTokens is one line as a candidate's constraint check reads it:
// its words and raw tokens (appendSearchConstraintTokens) and its lowered
// text.
type searchLineTokens struct {
	words []string
	raw   []string
	lower string
	// The entities of entityKey's constraints this piece holds (entityHits).
	entityKey    searchEntityKey
	entityHits   []int
	entitiesDone bool
}

// constraintTokens is a line's searchLineTokens, split once per file however
// many overlapping candidates hold the line. line is 1-based.
func (c *searchLineCache) constraintTokens(line int) *searchLineTokens {
	if c.tokens == nil {
		c.tokens = make([]*searchLineTokens, len(c.lines))
	}
	index := line - 1
	if c.tokens[index] == nil {
		c.tokens[index] = splitSearchLineTokens(c.lines[index])
	}
	return c.tokens[index]
}

// splitSearchLineTokens is a text's searchLineTokens. For ASCII text, which
// most source lines are, every word and token is a piece of the lowered text
// itself, so none is copied: appendSearchConstraintTokens lowers ASCII
// letters and digits and the class bytes byte for byte, and words and tokens
// are runs of them.
func splitSearchLineTokens(text string) *searchLineTokens {
	tokens := &searchLineTokens{lower: strings.ToLower(text)}
	for index := 0; index < len(text); index++ {
		if text[index] >= 0x80 {
			tokens.words, tokens.raw = appendSearchConstraintTokens(text, nil, nil)
			return tokens
		}
	}
	lower := tokens.lower
	run := func(in func(byte) bool) []string {
		var out []string
		for start := 0; start < len(lower); {
			if !in(lower[start]) {
				start++
				continue
			}
			end := start + 1
			for end < len(lower) && in(lower[end]) {
				end++
			}
			out = append(out, lower[start:end])
			start = end
		}
		return out
	}
	tokens.words = run(func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' })
	tokens.raw = run(searchWordByte)
	return tokens
}

// searchPartKey names a string by its bytes in memory: strings are
// immutable, so two strings at the same place and length are equal.
type searchPartKey struct {
	data   *byte
	length int
}

// partTokens is a candidate part's searchLineTokens, split once per file for
// every candidate holding that very string.
func (c *searchLineCache) partTokens(part string) *searchLineTokens {
	if part == "" {
		return &searchLineTokens{}
	}
	key := searchPartKey{data: unsafe.StringData(part), length: len(part)}
	if tokens, ok := c.parts[key]; ok {
		return tokens
	}
	if c.parts == nil {
		c.parts = map[searchPartKey]*searchLineTokens{}
	}
	tokens := splitSearchLineTokens(part)
	c.parts[key] = tokens
	return tokens
}

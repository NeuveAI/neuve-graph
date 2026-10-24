package sem

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// searchTermMatcher is a compact ASCII case-insensitive Aho-Corasick matcher.
// Issue and source identifiers are overwhelmingly ASCII; uncommon non-ASCII
// terms use a separate Unicode-aware fallback.
type searchTermMatcher struct {
	nodes []searchMatcherNode
	// table is the automaton with its failure links followed ahead of time:
	// table[state<<7|c] is the state after ASCII byte c, either case, so a
	// scan takes one lookup per byte.
	table       []int32
	fallback    []searchFallbackTerm
	termCount   int
	aliasTerms  map[int]string
	aliasTokens map[string][]int
}

type searchMatcherNode struct {
	next    map[byte]int
	failure int
	outputs []int
}

type searchFallbackTerm struct {
	index int
	term  string
}

// Query aliases carry stricter boundaries than the caller's explicit substrings.
func newSearchQueryTermMatcher(q searchQuery) searchTermMatcher {
	matcher := newSearchTermMatcher(q.terms)
	matcher.aliasTerms = map[int]string{}
	matcher.aliasTokens = map[string][]int{}
	termIndexes := map[string]int{}
	for index, term := range q.terms {
		termIndexes[term] = index
		if q.inferredAbbreviations[term] {
			matcher.aliasTerms[index] = term
		}
	}
	for token, aliases := range q.aliasTokens {
		for _, alias := range aliases {
			matcher.aliasTokens[token] = append(matcher.aliasTokens[token], termIndexes[alias])
		}
	}
	return matcher
}

func newSearchTermMatcher(terms []string) searchTermMatcher {
	matcher := searchTermMatcher{
		nodes:     []searchMatcherNode{{next: map[byte]int{}}},
		termCount: len(terms),
	}
	for index, term := range terms {
		if term == "" || !asciiString(term) {
			if term != "" {
				matcher.fallback = append(matcher.fallback, searchFallbackTerm{index: index, term: term})
			}
			continue
		}
		state := 0
		for offset := 0; offset < len(term); offset++ {
			character := lowerASCII(term[offset])
			next, ok := matcher.nodes[state].next[character]
			if !ok {
				next = len(matcher.nodes)
				matcher.nodes[state].next[character] = next
				matcher.nodes = append(matcher.nodes, searchMatcherNode{next: map[byte]int{}})
			}
			state = next
		}
		matcher.nodes[state].outputs = append(matcher.nodes[state].outputs, index)
	}
	matcher.buildFailures()
	matcher.buildTable()
	return matcher
}

// buildTable follows every state's failure links ahead of time. A state's
// failure is shallower, so in breadth-first order its row is complete when
// the state's is filled.
func (matcher *searchTermMatcher) buildTable() {
	matcher.table = make([]int32, len(matcher.nodes)<<7)
	queue := []int{0}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		row := matcher.table[state<<7 : (state+1)<<7]
		for character := 0; character < 128; character++ {
			lower := lowerASCII(byte(character))
			if next, ok := matcher.nodes[state].next[lower]; ok {
				row[character] = int32(next)
			} else if state != 0 {
				row[character] = matcher.table[matcher.nodes[state].failure<<7|character]
			}
		}
		for character := 0; character < 128; character++ {
			if next, ok := matcher.nodes[state].next[byte(character)]; ok {
				queue = append(queue, next)
			}
		}
	}
}

func (matcher *searchTermMatcher) buildFailures() {
	queue := make([]int, 0, len(matcher.nodes))
	for _, state := range matcher.nodes[0].next {
		queue = append(queue, state)
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		for character, next := range matcher.nodes[state].next {
			queue = append(queue, next)
			failure := matcher.nodes[state].failure
			for failure != 0 {
				if target, ok := matcher.nodes[failure].next[character]; ok {
					failure = target
					break
				}
				failure = matcher.nodes[failure].failure
			}
			if failure == 0 {
				if target, ok := matcher.nodes[0].next[character]; ok && target != next {
					failure = target
				}
			}
			matcher.nodes[next].failure = failure
			matcher.nodes[next].outputs = append(matcher.nodes[next].outputs, matcher.nodes[failure].outputs...)
		}
	}
}

func (matcher searchTermMatcher) match(text string) []bool {
	found := make([]bool, matcher.termCount)
	matcher.matchTerms(text, found)
	if len(matcher.aliasTerms) > 0 {
		for index := range matcher.aliasTerms {
			found[index] = false
		}
		for _, raw := range searchSourceWords(text) {
			for _, token := range searchTokenVariants(raw) {
				for _, index := range matcher.aliasTokens[token] {
					found[index] = true
				}
			}
		}
	}
	return found
}

// matchTerms sets found[index] for each term, ASCII or not, that the
// lowered text contains: strings.Contains(strings.ToLower(text), term) for a
// non-empty term. found holds a slot per term and starts false.
func (matcher searchTermMatcher) matchTerms(text string, found []bool) {
	remaining := matcher.termCount - len(matcher.fallback)
	state := int32(0)
	table := matcher.table
	step := func(character byte) {
		state = table[state<<7|int32(character)]
		for _, index := range matcher.nodes[state].outputs {
			if !found[index] {
				found[index] = true
				remaining--
			}
		}
	}
	// The lowered text, a rune at a time without building it: strings.ToLower
	// maps each rune by unicode.ToLower and an invalid byte to U+FFFD. A
	// non-ASCII rune that lowers to ASCII (the Kelvin sign to k) is that byte;
	// any other lowers to bytes of 128 or above, which no term in the table
	// holds, so the scan starts over after it.
	for offset := 0; offset < len(text) && remaining > 0; {
		if character := text[offset]; character < utf8.RuneSelf {
			step(character)
			offset++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[offset:])
		offset += size
		if lower := unicode.ToLower(r); lower < utf8.RuneSelf {
			step(byte(lower))
		} else {
			state = 0
		}
	}
	if len(matcher.fallback) > 0 {
		lower := strings.ToLower(text)
		for _, fallback := range matcher.fallback {
			if !found[fallback.index] && strings.Contains(lower, fallback.term) {
				found[fallback.index] = true
			}
		}
	}
}

func asciiString(value string) bool {
	for offset := 0; offset < len(value); offset++ {
		if value[offset] >= 128 {
			return false
		}
	}
	return true
}

func lowerASCII(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}

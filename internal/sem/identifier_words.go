package sem

import (
	"strings"
	"unicode"
)

// canonicalIdentifierWords splits a source identifier into stable lowercase
// words across qualified-name separators, snake case, and camel case. Provider
// identity heuristics use this tokenizer instead of retrieval-oriented query
// variants so their entity sets cannot drift with search ranking behavior.
func canonicalIdentifierWords(value string) []string {
	var words []string
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		words = append(words, strings.ToLower(string(current)))
		current = nil
	}
	runes := []rune(value)
	for index, character := range runes {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			flush()
			continue
		}
		if len(current) > 0 && unicode.IsUpper(character) {
			previous := runes[index-1]
			nextLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || unicode.IsUpper(previous) && nextLower {
				flush()
			}
		}
		current = append(current, character)
	}
	flush()
	return words
}

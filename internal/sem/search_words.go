package sem

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// searchSourceWords returns what searchSourceWordPattern.FindAllString(text,
// -1) returns, without the regular-expression engine: the maximal runs of
// letters, numbers and `_./:+#-`. The engine reads text rune by rune, an
// invalid byte as U+FFFD, which is in neither class, and its leftmost-first
// greedy `+` over one character class yields exactly the maximal runs.
func searchSourceWords(text string) []string {
	var words []string
	start := -1
	for index := 0; index < len(text); {
		r, size := utf8.DecodeRuneInString(text[index:])
		if searchSourceWordRune(r) {
			if start < 0 {
				start = index
			}
		} else if start >= 0 {
			words = append(words, text[start:index])
			start = -1
		}
		index += size
	}
	if start >= 0 {
		words = append(words, text[start:])
	}
	return words
}

func searchSourceWordRune(r rune) bool {
	switch r {
	case '_', '.', '/', ':', '+', '#', '-':
		return true
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

// searchTokenVariantsMemo remembers searchTokenVariants per word. The split
// is a pure function of the word and ranking asks it for the same words
// across every file of a search. A word and its parts are copied before they
// are kept, because they are slices of a file's content and would otherwise
// keep the whole file alive; a remembered result is clipped to its length,
// so a caller appending to it copies rather than writing into it. Words
// longer than maxRememberedSearchWord (minified or encoded runs) are split
// every time, and the memory is cleared once it holds
// maxSearchTokenVariantsMemoBytes of word text (about two and a half times
// that in heap, with the map's own overhead), which keeps a long-lived
// process flat.
var searchTokenVariantsMemo = struct {
	sync.RWMutex
	words map[string][]string
	bytes int
}{words: map[string][]string{}}

const (
	maxRememberedSearchWord         = 64
	maxSearchTokenVariantsMemoBytes = 16 << 20
)

func searchTokenVariants(raw string) []string {
	if len(raw) > maxRememberedSearchWord {
		return computeSearchTokenVariants(raw)
	}
	searchTokenVariantsMemo.RLock()
	variants, ok := searchTokenVariantsMemo.words[raw]
	searchTokenVariantsMemo.RUnlock()
	if ok {
		return variants
	}
	computed := computeSearchTokenVariants(raw)
	if computed != nil {
		variants = make([]string, len(computed))
	}
	size := len(raw)
	for i, variant := range computed {
		variants[i] = strings.Clone(variant)
		size += len(variant) + 16 // the string header
	}
	searchTokenVariantsMemo.Lock()
	if searchTokenVariantsMemo.bytes+size > maxSearchTokenVariantsMemoBytes {
		clear(searchTokenVariantsMemo.words)
		searchTokenVariantsMemo.bytes = 0
	}
	if _, raced := searchTokenVariantsMemo.words[raw]; !raced {
		searchTokenVariantsMemo.words[strings.Clone(raw)] = variants
		searchTokenVariantsMemo.bytes += size
	}
	searchTokenVariantsMemo.Unlock()
	return variants
}

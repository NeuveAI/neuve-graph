package sem

import "strings"

// asciiFoldContainsAny reports whether text holds any of needles, which are
// lower-case ASCII, ignoring ASCII case. ascii is false when text holds a
// non-ASCII byte, and found is then not computed: a case-insensitive regexp
// folds Unicode too (the long s with s, the Kelvin sign with k), so only on
// ASCII text is this exactly where such a regexp can find a needle.
func asciiFoldContainsAny(text string, needles ...string) (found, ascii bool) {
	if !asciiString(text) {
		return false, false
	}
	lower := strings.ToLower(text)
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true, true
		}
	}
	return false, true
}

// asciiFoldContainsAll is asciiFoldContainsAny, true only when text holds
// every needle.
func asciiFoldContainsAll(text string, needles ...string) (found, ascii bool) {
	if !asciiString(text) {
		return false, false
	}
	lower := strings.ToLower(text)
	for _, needle := range needles {
		if !strings.Contains(lower, needle) {
			return false, true
		}
	}
	return true, true
}

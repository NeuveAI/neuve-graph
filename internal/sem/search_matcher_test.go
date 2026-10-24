package sem

import (
	"math/rand"
	"strings"
	"testing"
)

// The matcher's table answers as strings.Contains on the lowered text does,
// for every lower-case term: overlapping terms, terms inside one another,
// upper-case and non-ASCII text (the Kelvin sign lowers to an ASCII k),
// invalid UTF-8, and non-ASCII terms.
func TestSearchTermMatcherIsContainsOnTheLoweredText(t *testing.T) {
	terms := []string{"he", "she", "his", "hers", "k", "kelvin", "ab", "abab", "b", "bab", "é", "straße", "x_y", "a.b", "hers", "zzz"}
	alphabet := []string{"a", "b", "h", "e", "s", "r", "i", "K", "H", "E", "K", "é", "É", "ß", "x", "_", "y", ".", " ", "\xff", "elvin", "STRASSE"}
	random := rand.New(rand.NewSource(7))
	matcher := newSearchTermMatcher(terms)
	for round := 0; round < 4000; round++ {
		var text strings.Builder
		for length := random.Intn(24); length > 0; length-- {
			text.WriteString(alphabet[random.Intn(len(alphabet))])
		}
		found := make([]bool, len(terms))
		matcher.matchTerms(text.String(), found)
		lower := strings.ToLower(text.String())
		for index, term := range terms {
			if want := strings.Contains(lower, term); found[index] != want {
				t.Fatalf("text %q term %q: matcher %v, Contains %v", text.String(), term, found[index], want)
			}
		}
	}
}

// Symbol scoring scores and signals alike whether the query's signature
// matcher answers or each term is asked on its own, abbreviations, an empty
// term and an upper-case one included.
func TestSymbolSearchScoreSignatureTermsAgree(t *testing.T) {
	queries := []string{
		"allocate the address buffer for the asynchronous administrator",
		"fix parseConfig when the YAML file has a BOM",
		"update market sizes in the HTTP handler",
	}
	signatures := []string{
		"func allocAddr(buf []byte) error",
		"func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)",
		"def parse_config(path: str) -> Config:",
		"class MarketSizes extends Base { update(): void }",
		"func asyncAdminAddressBuffer() Kelvin",
		"",
	}
	for _, raw := range queries {
		q := buildSearchQuery(raw)
		q.terms = append(append([]string(nil), q.terms...), "", "Http")
		for _, signature := range signatures {
			symbol := SymbolRecord{Name: "handler", Kind: "function", Signature: signature}
			plain := q
			plain.signatureTerms = nil
			wantScore, wantSignals := symbolSearchScore(plain, symbol)
			matched := q
			matched.signatureTerms = newSearchSignatureTerms(matched.terms)
			gotScore, gotSignals := symbolSearchScore(matched, symbol)
			if gotScore != wantScore || strings.Join(gotSignals, ",") != strings.Join(wantSignals, ",") {
				t.Fatalf("query %q signature %q: %v %v with the matcher, %v %v without", raw, signature, gotScore, gotSignals, wantScore, wantSignals)
			}
			stale := q
			stale.signatureTerms = newSearchSignatureTerms(append([]string(nil), q.terms...))
			if score, _ := symbolSearchScore(stale, symbol); score != wantScore {
				t.Fatalf("a matcher built for other terms answered: %v, want %v", score, wantScore)
			}
		}
	}
}

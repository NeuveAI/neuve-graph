package sem

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// oldAgreement is what searchCandidateAgreementCached computed before the
// line cache: the may-match check over the joined text, then the surface.
func oldAgreement(candidate searchCandidate, constraints searchConstraints) (float64, []string, bool) {
	if !searchCandidateConstraintMayMatch(candidate, constraints) {
		return 0, nil, false
	}
	bonus, signals := searchCandidateAgreementSurface(buildSearchCandidateConstraintSurface(candidate), constraints)
	return bonus, signals, true
}

// A candidate's agreement read from its file's line cache is the one its own
// text gives: on this package's source and on mixed-script text, for random
// regions, symbols and aliases, under constraints taken from queries and from
// the files' own words and tokens, phrases explicit or not.
func TestAgreementFromLinesIsTheSurfaces(t *testing.T) {
	paths, _ := filepath.Glob("search*.go")
	var files [][]string
	for _, path := range paths[:min(len(paths), 6)] {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, strings.Split(string(data), "\n"))
	}
	files = append(files, []string{
		"// Avery recommended the book to Morgan.", "func Recommend(book string) {}", "",
		"café Größe ﬁle İstanbul \xff\xfe Kelvin\u212a", "the reviewed archive transactions", "c++ a/b#c -x_y book.go:12",
	})
	queries := []string{
		`What book did Avery recommend?`,
		`Which archive transaction was reviewed after "book.go:12"?`,
		`how does "func Recommend" handle the book`,
		`Größe of the café file`,
	}
	rng := rand.New(rand.NewSource(5))
	checked, matched := 0, 0
	for _, lines := range files {
		var vocabulary []string
		for _, line := range lines {
			words, raw := appendSearchConstraintTokens(line, nil, nil)
			vocabulary = append(vocabulary, words...)
			vocabulary = append(vocabulary, raw...)
		}
		pick := func() string {
			if len(vocabulary) == 0 {
				return "book"
			}
			return vocabulary[rng.Intn(len(vocabulary))]
		}
		var constraintSets []searchConstraints
		for _, query := range queries {
			constraintSets = append(constraintSets, parseSearchConstraints(query))
		}
		for range 12 {
			c := searchConstraints{}
			for range 1 + rng.Intn(3) {
				c.Entities = append(c.Entities, searchEntityConstraint{Normalized: pick()})
			}
			for range rng.Intn(3) {
				phrase := searchPhraseConstraint{Explicit: rng.Intn(2) == 0}
				for range 1 + rng.Intn(3) {
					phrase.Words = append(phrase.Words, pick())
				}
				c.Phrases = append(c.Phrases, phrase)
			}
			if rng.Intn(4) == 0 {
				c.Entities = nil
			}
			constraintSets = append(constraintSets, c)
		}
		cache := newSearchLineCache(buildSearchQuery("book"), lines)
		for range 80 {
			start := 1 + rng.Intn(len(lines))
			end := min(len(lines), start+rng.Intn(60))
			candidate := searchCandidate{
				result: SearchResult{
					SymbolName:       []string{"", "Recommend", "Größe", "book"}[rng.Intn(4)],
					QualifiedName:    []string{"", "notes.Recommend", "pkg.Archive"}[rng.Intn(3)],
					Signature:        []string{"", "func Recommend(book string)", "func F()"}[rng.Intn(3)],
					SnippetStartLine: start,
					SnippetEndLine:   end,
					Snippet:          strings.Join(lines[start-1:end], "\n"),
				},
				aliases:   [][]string{nil, {"avery", "Book"}, {"café"}}[rng.Intn(3)],
				lineCache: cache,
			}
			for _, constraints := range constraintSets {
				if !searchConstraintsRequireAgreement(constraints) {
					continue
				}
				bonus, signals, mayMatch, ok := searchCandidateAgreementFromLines(&candidate, constraints)
				if !ok {
					t.Fatalf("the line path declined a joined snippet")
				}
				wantBonus, wantSignals, wantMay := oldAgreement(candidate, constraints)
				if bonus != wantBonus || !reflect.DeepEqual(signals, wantSignals) || mayMatch != wantMay {
					t.Fatalf("lines %d-%d, constraints %+v:\n got %v %v %v\nwant %v %v %v", start, end, constraints, bonus, signals, mayMatch, wantBonus, wantSignals, wantMay)
				}
				checked++
				if bonus > 0 {
					matched++
				}
			}
		}
	}
	if checked == 0 || matched == 0 {
		t.Fatalf("compared %d agreements, %d of them nonzero", checked, matched)
	}
	// A snippet that is not its lines joined is left to the old path.
	candidate := searchCandidate{result: SearchResult{Snippet: "edited", SnippetStartLine: 1, SnippetEndLine: 1}, lineCache: newSearchLineCache(buildSearchQuery("x"), []string{"original"})}
	if _, _, _, ok := searchCandidateAgreementFromLines(&candidate, parseSearchConstraints(`What book did Avery recommend?`)); ok {
		t.Fatal("an edited snippet was read from the line cache")
	}
}

// A text's line tokens are what appendSearchConstraintTokens splits, and its
// lowered text is strings.ToLower's, for ASCII and for anything else.
func TestSplitSearchLineTokensIsTheTokenizer(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	alphabet := []string{"a", "Z", "9", "_", ".", "/", ":", "+", "#", "-", " ", "\t", "(", "É", "é", "日", "\xff", "İ", "K", "ﬁ"}
	texts := []string{"", " ", "Func NewAuthSession(token string) *Session {", "a.b/c:d+e#f-g_h", "CAFÉ café", "\xff\xfe"}
	for range 2000 {
		var b strings.Builder
		for range rng.Intn(40) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		texts = append(texts, b.String())
	}
	for _, text := range texts {
		got := splitSearchLineTokens(text)
		words, raw := appendSearchConstraintTokens(text, nil, nil)
		if got.lower != strings.ToLower(text) || !reflect.DeepEqual(got.words, words) || !reflect.DeepEqual(got.raw, raw) {
			t.Fatalf("%q:\n got %q %q %q\nwant %q %q %q", text, got.lower, got.words, got.raw, strings.ToLower(text), words, raw)
		}
	}
}

package sem

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// entitiesFromMap is what searchFileConstraintEvidence computed before:
// membership in the map of every whole token.
func entitiesFromMap(content string, entities []searchEntityConstraint) []bool {
	whole := searchConstraintWholeTokens(content, searchQueryWordSequence(strings.ToLower(content)))
	found := make([]bool, len(entities))
	for index, entity := range entities {
		found[index] = whole[entity.Normalized]
	}
	return found
}

func checkEntitiesAsWholeTokens(t *testing.T, content string, entities []searchEntityConstraint) {
	t.Helper()
	got := searchEntitiesAsWholeTokens(content, searchQueryWordSequence(strings.ToLower(content)), entities)
	if want := entitiesFromMap(content, entities); !reflect.DeepEqual(got, want) {
		t.Fatalf("content %q, entities %v: got %v, want %v", content, entities, got, want)
	}
}

// Whether each entity is a whole token is what the map of every token said:
// on this package's own source, on text mixing ASCII, other scripts, symbols
// and invalid UTF-8, with entities taken from the text and from elsewhere.
func TestEntitiesAsWholeTokensMatchTheTokenMap(t *testing.T) {
	paths, _ := filepath.Glob("search*.go")
	var texts []string
	for _, path := range paths[:min(len(paths), 8)] {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(data))
	}
	texts = append(texts,
		"Avery's book.go:12 c++ a/b#c -x_y café Größe 日本語 \xff\xfebroken\xc3 tail",
		"", "....", "ID id Id iD", "ﬁle ǅungla İstanbul ıi KelvinK",
	)
	pieces := []string{"avery", "book.go", "c++", "a/b#c", "-x_y", "café", "größe", "id", "日本語", "istanbul", "missing", "go:12", "tail", "broken", "x"}
	rng := rand.New(rand.NewSource(11))
	alphabet := []string{"a", "B", "1", "_", ".", "/", ":", "+", "#", "-", " ", "\n", "é", "日", "\xff", "\xc3", "İ", "ı", "K"}
	for round := 0; round < 400; round++ {
		var b strings.Builder
		for range 1 + rng.Intn(60) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		texts = append(texts, b.String())
	}
	for index, text := range texts {
		var entities []searchEntityConstraint
		for range 1 + rng.Intn(6) {
			entities = append(entities, searchEntityConstraint{Normalized: pieces[rng.Intn(len(pieces))]})
		}
		// And tokens of the text itself, lowered as the map lowers them.
		for _, token := range searchWordPattern.FindAllString(text, 4) {
			entities = append(entities, searchEntityConstraint{Normalized: strings.ToLower(token)})
		}
		if index%3 == 0 {
			for _, word := range searchQueryWordSequence(strings.ToLower(text)) {
				entities = append(entities, searchEntityConstraint{Normalized: word})
				break
			}
		}
		checkEntitiesAsWholeTokens(t, text, entities)
	}
}

func FuzzEntitiesAsWholeTokens(f *testing.F) {
	f.Add("Avery's book.go:12 \xff c++", "book.go:12")
	f.Add("ID id", "id")
	f.Fuzz(func(t *testing.T, content, entity string) {
		checkEntitiesAsWholeTokens(t, content, []searchEntityConstraint{{Normalized: entity}, {Normalized: strings.ToLower(entity)}})
	})
}

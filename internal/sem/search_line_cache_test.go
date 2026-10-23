package sem

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The line cache answers what the uncached ranking helpers answer, for any
// region of a file: term counts and length of the region's text, its focus
// line, and each line's match. Real source (this package's own files, plus
// lines with non-ASCII words, CRLF and empty lines) and queries with
// inferred abbreviations, phrases and non-ASCII terms.
func TestSearchLineCacheMatchesTheUncachedHelpers(t *testing.T) {
	paths, err := filepath.Glob("search*.go")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no source files to read: %v", err)
	}
	var files [][]string
	for _, path := range paths[:min(len(paths), 6)] {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, strings.Split(string(data), "\n"))
	}
	files = append(files, []string{
		"café naïve résumé", "", "func AuthSession() {}\r", "  ctx := context.Background() // auth cfg",
		"Größe der Datei", "repo.Repository{}", "", "session_token, sessionToken, SESSIONTOKEN", "日本語のコメント auth",
	})
	queries := []string{
		"authentication session token",
		"repository configuration context",
		"search query term counts",
		"café résumé size",
		"validate the login token for a user session",
		"makeSearchCandidate focus line",
	}
	rng := rand.New(rand.NewSource(7))
	checked := 0
	for _, text := range queries {
		q := buildSearchQuery(text)
		for _, lines := range files {
			cache := newSearchLineCache(q, lines)
			for trial := 0; trial < 60; trial++ {
				start := 1 + rng.Intn(len(lines))
				end := min(len(lines), start+rng.Intn(80))
				wantCounts, wantLength := searchTermCounts(strings.Join(lines[start-1:end], "\n"), q)
				gotCounts, gotLength := cache.termCounts(start, end)
				if !reflect.DeepEqual(gotCounts, wantCounts) || gotLength != wantLength {
					t.Fatalf("query %q lines %d-%d: counts %v/%d, want %v/%d", text, start, end, gotCounts, gotLength, wantCounts, wantLength)
				}
				if got, want := cache.focusLine(start, end), searchFocusLine(q, lines, start, end); got != want {
					t.Fatalf("query %q lines %d-%d: focus %d, want %d", text, start, end, got, want)
				}
				line := start + rng.Intn(end-start+1)
				// The focus weight, gathered once per line, is the original
				// per-term sum.
				original, lower := 0.0, strings.ToLower(lines[line-1])
				for _, term := range q.terms {
					if searchQueryTermMatches(q, lines[line-1], lower, term) {
						original += q.weights[term]
					}
				}
				if got := searchLineFocusWeight(q, lines[line-1]); got != original {
					t.Fatalf("query %q line %d: focus weight %v, want %v", text, line, got, original)
				}
				if got, want := cache.matchesLine(line), textMatchesSearchQuery(q, lines[line-1]); got != want {
					t.Fatalf("query %q line %d: match %v, want %v", text, line, got, want)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("nothing was compared")
	}
}

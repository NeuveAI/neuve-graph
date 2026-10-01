package sem

import (
	"reflect"
	"strings"
	"testing"
)

func containedCallableSymbol(id, name, path, language string, start, end int) SymbolRecord {
	return SymbolRecord{
		RecordType: "symbol", ID: id, Name: name, QualifiedName: name, Kind: "function",
		Language: language, FilePath: path, StartLine: start, EndLine: end,
	}
}

func renderedSnippet(lines int) string {
	return strings.TrimSuffix(strings.Repeat("rendered source\n", lines), "\n")
}

func withoutContainedCallableManifest(results []SearchResult) []SearchResult {
	copyResults := append([]SearchResult(nil), results...)
	for index := range copyResults {
		copyResults[index].ContainedCallables = nil
	}
	return copyResults
}

func TestAttachSearchContainedCallablesPreservesGenericProviderIdentity(t *testing.T) {
	t.Parallel()
	path := "src/session.misleading"
	owner := containedCallableSymbol("owner", "resolve", path, "ProviderOwner", 20, 40)
	first := containedCallableSymbol("first", "build", path, "ArbitraryOne", 2, 5)
	second := containedCallableSymbol("second", "check", path, "ArbitraryTwo", 7, 10)
	alreadyTopLevel := containedCallableSymbol("top-level", "other", path, "ArbitraryThree", 12, 14)
	partial := containedCallableSymbol("partial", "partial", path, "ArbitraryFour", 39, 44)
	malformed := containedCallableSymbol("malformed", "malformed", path, "ArbitraryFive", 15, 17)
	malformed.QualifiedName = ""
	container := SymbolRecord{ID: "container", Name: "Container", QualifiedName: "Container", Kind: "class", FilePath: path, StartLine: 1, EndLine: 18}
	results := []SearchResult{
		{Rank: 1, FilePath: path, SymbolID: owner.ID, SnippetStartLine: 1, SnippetEndLine: 40, Snippet: renderedSnippet(40)},
		{Rank: 2, FilePath: path, SymbolID: alreadyTopLevel.ID, SnippetStartLine: 1, SnippetEndLine: 18, Snippet: renderedSnippet(18)},
	}
	before := append([]SearchResult(nil), results...)
	// Deliberately shuffled: output order is range then opaque provider ID, not provider slice order.
	symbolsByFile := map[string][]SymbolRecord{path: {second, partial, owner, container, malformed, first, alreadyTopLevel}}

	got, status, emitted, dropped, ledger := attachSearchContainedCallables(results, symbolsByFile, 0)
	if status != searchContainedCallablesComplete || emitted != 2 || dropped != 0 {
		t.Fatalf("manifest = status %q emitted %d dropped %d, want complete 2 0", status, emitted, dropped)
	}
	if len(ledger) != 0 {
		t.Fatalf("complete manifest ledger = %#v, want empty", ledger)
	}
	want := []SearchContainedCallable{
		{SymbolID: first.ID, SymbolName: first.Name, QualifiedName: first.QualifiedName, Kind: first.Kind, Language: first.Language, StartLine: 2, EndLine: 5},
		{SymbolID: second.ID, SymbolName: second.Name, QualifiedName: second.QualifiedName, Kind: second.Kind, Language: second.Language, StartLine: 7, EndLine: 10},
	}
	if !reflect.DeepEqual(got[0].ContainedCallables, want) {
		t.Fatalf("rank-1 contained callables = %#v, want %#v", got[0].ContainedCallables, want)
	}
	if len(got[1].ContainedCallables) != 0 {
		t.Fatalf("later owner duplicated or admitted excluded symbols: %#v", got[1].ContainedCallables)
	}
	if !reflect.DeepEqual(withoutContainedCallableManifest(got), before) {
		t.Fatalf("primary evidence changed:\n got: %#v\nwant: %#v", withoutContainedCallableManifest(got), before)
	}
}

func TestAttachSearchContainedCallablesTruncatesManifestWithoutChangingEvidence(t *testing.T) {
	t.Parallel()
	path := "src/session.any"
	first := containedCallableSymbol("first", "first", path, "Declared", 2, 5)
	second := containedCallableSymbol("second", "second", path, "Declared", 7, 10)
	results := []SearchResult{{
		Rank: 1, FilePath: path, SnippetStartLine: 1, SnippetEndLine: 12,
		Snippet: renderedSnippet(12), Signals: []string{"body"},
	}}
	symbolsByFile := map[string][]SymbolRecord{path: {first, second}}
	expected := append([]SearchResult(nil), results...)
	expected[0].ContainedCallables = []SearchContainedCallable{{
		SymbolID: first.ID, SymbolName: first.Name, QualifiedName: first.QualifiedName,
		Kind: first.Kind, Language: first.Language, StartLine: first.StartLine, EndLine: first.EndLine,
	}}
	budget := serializedSearchResultBytes(expected)

	got, status, emitted, dropped, ledger := attachSearchContainedCallables(results, symbolsByFile, budget)
	if status != searchContainedCallablesTruncated || emitted != 1 || dropped != 1 {
		t.Fatalf("manifest = status %q emitted %d dropped %d, want truncated 1 1", status, emitted, dropped)
	}
	if want := []SearchContainedCallableDrop{{Rank: 1, Dropped: 1}}; !reflect.DeepEqual(ledger, want) {
		t.Fatalf("omission ledger = %#v, want %#v", ledger, want)
	}
	if serializedSearchResultBytes(got) > budget {
		t.Fatalf("manifest exceeded hard result budget: %d > %d", serializedSearchResultBytes(got), budget)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("budgeted manifest = %#v, want %#v", got, expected)
	}
	if !reflect.DeepEqual(withoutContainedCallableManifest(got), results) {
		t.Fatal("manifest allocation changed primary evidence")
	}
}

func TestAttachSearchContainedCallablesReportsZeroHeadroomOmissionsOutsideResultBytes(t *testing.T) {
	t.Parallel()
	firstPath := "src/first.any"
	secondPath := "src/second.any"
	results := []SearchResult{
		{Rank: 1, FilePath: firstPath, SnippetStartLine: 1, SnippetEndLine: 10, Snippet: renderedSnippet(10)},
		{Rank: 2, FilePath: secondPath, SnippetStartLine: 1, SnippetEndLine: 10, Snippet: renderedSnippet(10)},
	}
	symbolsByFile := map[string][]SymbolRecord{
		firstPath: {
			containedCallableSymbol("first-a", "firstA", firstPath, "Declared", 2, 3),
			containedCallableSymbol("first-b", "firstB", firstPath, "Declared", 5, 6),
		},
		secondPath: {
			containedCallableSymbol("second-a", "secondA", secondPath, "Declared", 2, 3),
		},
	}
	budget := serializedSearchResultBytes(results)

	got, status, emitted, dropped, ledger := attachSearchContainedCallables(results, symbolsByFile, budget)
	if status != searchContainedCallablesTruncated || emitted != 0 || dropped != 3 {
		t.Fatalf("manifest = status %q emitted %d dropped %d, want truncated 0 3", status, emitted, dropped)
	}
	wantLedger := []SearchContainedCallableDrop{{Rank: 1, Dropped: 2}, {Rank: 2, Dropped: 1}}
	if !reflect.DeepEqual(ledger, wantLedger) {
		t.Fatalf("omission ledger = %#v, want %#v", ledger, wantLedger)
	}
	if !reflect.DeepEqual(got, results) {
		t.Fatalf("zero-headroom result changed:\n got: %#v\nwant: %#v", got, results)
	}
	if serializedSearchResultBytes(got) != budget {
		t.Fatalf("result bytes = %d, want unchanged %d", serializedSearchResultBytes(got), budget)
	}
}

func TestAttachSearchContainedCallablesRequiresExactRenderedSnippetBytes(t *testing.T) {
	t.Parallel()
	path := "src/session.any"
	symbols := map[string][]SymbolRecord{
		path: {containedCallableSymbol("nested", "nested", path, "Declared", 2, 3)},
	}
	for _, testCase := range []struct {
		name    string
		snippet string
	}{
		{name: "empty locator", snippet: ""},
		{name: "elision placeholder", snippet: "... truncated ..."},
		{name: "claimed span exceeds rendered lines", snippet: "one line"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			results := []SearchResult{{
				Rank: 1, FilePath: path, SnippetStartLine: 1, SnippetEndLine: 4,
				Snippet: testCase.snippet,
			}}
			got, status, emitted, dropped, ledger := attachSearchContainedCallables(results, symbols, 0)
			if status != searchContainedCallablesComplete || emitted != 0 || dropped != 0 || len(ledger) != 0 {
				t.Fatalf("manifest = %q/%d/%d %#v, want complete absence", status, emitted, dropped, ledger)
			}
			if !reflect.DeepEqual(got, results) {
				t.Fatalf("ineligible locator changed: %#v", got)
			}
		})
	}
}

func validContainedCallableResponse() SearchResponse {
	result := SearchResult{
		Rank: 1, FilePath: "src/session.any", SymbolID: "owner", StartLine: 1, EndLine: 5,
		FocusLine: 1, SnippetStartLine: 1, SnippetEndLine: 5, Snippet: renderedSnippet(5),
		ContainedCallables: []SearchContainedCallable{
			{SymbolID: "first", SymbolName: "first", QualifiedName: "first", Kind: "function", StartLine: 2, EndLine: 2},
			{SymbolID: "second", SymbolName: "second", QualifiedName: "second", Kind: "method", StartLine: 4, EndLine: 5},
		},
	}
	return SearchResponse{
		Results: []SearchResult{result},
		Stats: SearchStats{
			ResultBytes:               serializedSearchResultBytes([]SearchResult{result}),
			ContainedCallablesStatus:  searchContainedCallablesComplete,
			ContainedCallablesEmitted: 2,
		},
	}
}

func cloneContainedCallableResponse(response SearchResponse) SearchResponse {
	clone := response
	clone.Results = append([]SearchResult(nil), response.Results...)
	for index := range clone.Results {
		clone.Results[index].ContainedCallables = append(
			[]SearchContainedCallable(nil), response.Results[index].ContainedCallables...,
		)
	}
	clone.Stats.ContainedCallablesDroppedByResult = append(
		[]SearchContainedCallableDrop(nil), response.Stats.ContainedCallablesDroppedByResult...,
	)
	return clone
}

func TestSearchResponseValidateEnforcesContainedCallableContract(t *testing.T) {
	t.Parallel()
	valid := validContainedCallableResponse()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid contained callable response rejected: %v", err)
	}
	mutations := []struct {
		name   string
		mutate func(*SearchResponse)
	}{
		{name: "missing status", mutate: func(response *SearchResponse) { response.Stats.ContainedCallablesStatus = "" }},
		{name: "emitted mismatch", mutate: func(response *SearchResponse) { response.Stats.ContainedCallablesEmitted = 1 }},
		{name: "complete with drops", mutate: func(response *SearchResponse) {
			response.Stats.ContainedCallablesDroppedByBudget = 1
			response.Stats.ContainedCallablesDroppedByResult = []SearchContainedCallableDrop{{Rank: 1, Dropped: 1}}
		}},
		{name: "truncated without drops", mutate: func(response *SearchResponse) {
			response.Stats.ContainedCallablesStatus = searchContainedCallablesTruncated
		}},
		{name: "ledger outside results", mutate: func(response *SearchResponse) {
			response.Stats.ContainedCallablesStatus = searchContainedCallablesTruncated
			response.Stats.ContainedCallablesDroppedByBudget = 1
			response.Stats.ContainedCallablesDroppedByResult = []SearchContainedCallableDrop{{Rank: 2, Dropped: 1}}
		}},
		{name: "range outside rendered snippet", mutate: func(response *SearchResponse) { response.Results[0].ContainedCallables[0].StartLine = 0 }},
		{name: "non callable kind", mutate: func(response *SearchResponse) { response.Results[0].ContainedCallables[0].Kind = "class" }},
		{name: "duplicate identity", mutate: func(response *SearchResponse) { response.Results[0].ContainedCallables[1].SymbolID = "first" }},
		{name: "top level identity", mutate: func(response *SearchResponse) { response.Results[0].ContainedCallables[0].SymbolID = "owner" }},
		{name: "unstable order", mutate: func(response *SearchResponse) {
			response.Results[0].ContainedCallables[0], response.Results[0].ContainedCallables[1] =
				response.Results[0].ContainedCallables[1], response.Results[0].ContainedCallables[0]
		}},
		{name: "empty identity", mutate: func(response *SearchResponse) { response.Results[0].ContainedCallables[0].SymbolName = "" }},
		{name: "empty locator", mutate: func(response *SearchResponse) { response.Results[0].Snippet = "" }},
		{name: "elision placeholder", mutate: func(response *SearchResponse) {
			response.Results[0].Snippet = "... truncated ..."
			response.Results[0].SnippetEndLine = response.Results[0].SnippetStartLine
		}},
	}
	for _, testCase := range mutations {
		t.Run(testCase.name, func(t *testing.T) {
			response := cloneContainedCallableResponse(valid)
			testCase.mutate(&response)
			response.Stats.ResultBytes = serializedSearchResultBytes(response.Results)
			if err := response.Validate(); err == nil {
				t.Fatal("mutated contained callable response passed validation")
			}
		})
	}
}

func TestAttachSearchContainedCallablesCannotRespendFundedBlockBytes(t *testing.T) {
	t.Parallel()
	path := "src/session.any"
	base := []SearchResult{{
		Rank: 1, FilePath: path, SymbolID: "owner", StartLine: 1, EndLine: 5,
		FocusLine: 1, SnippetStartLine: 1, SnippetEndLine: 5, Snippet: renderedSnippet(5),
	}}
	symbols := map[string][]SymbolRecord{
		path: {containedCallableSymbol("nested", "nested", path, "Declared", 2, 3)},
	}
	card := []TypeCardEntry{{Name: "Dependency", FilePath: path, Line: 9, Decl: "type Dependency struct{}"}}
	signature := []SearchSignatureType{{Name: "Input", Kind: "struct", FilePath: path, StartLine: 12}}
	for _, testCase := range []struct {
		name      string
		card      []TypeCardEntry
		signature []SearchSignatureType
	}{
		{name: "type card", card: card},
		{name: "type card and signature types", card: card, signature: signature},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cardBytes := serializedSearchResultBytes(testCase.card)
			signatureBytes := 0
			if len(testCase.signature) > 0 {
				signatureBytes = serializedSearchResultBytes(testCase.signature)
			}
			baseBytes := serializedSearchResultBytes(base)
			contextBudget := baseBytes + cardBytes + signatureBytes
			resultBudget := searchContainedCallableResultBudget(contextBudget, cardBytes+signatureBytes)
			results, status, emitted, dropped, ledger := attachSearchContainedCallables(base, symbols, resultBudget)
			if status != searchContainedCallablesTruncated || emitted != 0 || dropped != 1 {
				t.Fatalf("manifest = %q/%d/%d, want truncated 0/1", status, emitted, dropped)
			}
			response := SearchResponse{
				Results: results, TypeCard: testCase.card, SignatureTypes: testCase.signature,
				Stats: SearchStats{
					ResultBytes: baseBytes, ContextBudgetBytes: contextBudget,
					TypeCardBytes: cardBytes, SignatureTypeBytes: signatureBytes,
					ContextBlockBytes:        cardBytes + signatureBytes,
					ContainedCallablesStatus: status, ContainedCallablesEmitted: emitted,
					ContainedCallablesDroppedByBudget: dropped,
					ContainedCallablesDroppedByResult: ledger,
				},
			}
			if err := response.Validate(); err != nil {
				t.Fatalf("funded-block response rejected: %v", err)
			}
		})
	}
}

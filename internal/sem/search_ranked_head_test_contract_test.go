package sem

import (
	"reflect"
	"strings"
	"testing"
)

func rankedHeadTestContractFixture(count int) ([]SearchResult, contentReader) {
	lines := rankedTestContractLines(count)
	results := contractPayload()[:searchEnclosureHeadRanks]
	results[2] = SearchResult{Rank: 3, Score: 42, FilePath: "tests/contract_test.go", StartLine: 17, EndLine: 21,
		FocusLine: 19, SnippetStartLine: 17, SnippetEndLine: 21, Language: "Go", Signals: []string{"body", "query_terms=subject"},
		Snippet: strings.Join(lines[16:21], "\n")}
	results[3] = results[2]
	results[3].Rank = 4
	return results, func(path string) (string, bool) {
		return strings.Join(lines, "\n"), path == "tests/contract_test.go"
	}
}

func TestRankedHeadTestContractKeepsRankIdentitySignalsAndOtherResults(t *testing.T) {
	t.Parallel()
	before, read := rankedHeadTestContractFixture(48)
	after := expandSearchRankedHeadTestContract(before, buildSearchQuery("subject behaviour"), read, 16384)
	unbounded := expandSearchRankedHeadTestContract(before, buildSearchQuery("subject behaviour"), read, 0)
	if !reflect.DeepEqual(after, unbounded) {
		t.Fatal("bounded and unbounded head eligibility differed despite sufficient budget")
	}
	if len(after) != len(before) {
		t.Fatalf("TopK changed: %d -> %d", len(before), len(after))
	}
	for index := range before {
		if index != 2 && !reflect.DeepEqual(before[index], after[index]) {
			t.Fatalf("unselected result %d changed", index)
		}
	}
	entry := after[2]
	if entry.Rank != 3 || entry.Score != 42 || entry.FocusLine != 19 || entry.Section != searchSectionPrimary ||
		entry.SymbolID != "" || entry.StartLine != 1 || entry.EndLine != 48 ||
		!searchRankedTestSnippetIsExactSubset(entry, before[2]) ||
		!reflect.DeepEqual(entry.Signals, []string{"body", "query_terms=subject", searchHeadWindowSignal}) {
		t.Fatalf("head window lost ranking or evidence: %#v", entry)
	}
}

func TestRankedHeadTestContractRefusesIneligibleAndUnfundedWindows(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"named", "path-only", "explicit-test", "funded-blocks", "source-mismatch"} {
		t.Run(name, func(t *testing.T) {
			before, read := rankedHeadTestContractFixture(48)
			// Remove the duplicate as an alternative eligible candidate.
			before[3].SymbolID = "named-duplicate"
			query := buildSearchQuery("subject behaviour")
			budget := 16384
			switch name {
			case "named":
				before[2].SymbolID = "real-named-callable"
			case "path-only":
				before[2].Signals = []string{"path"}
			case "explicit-test":
				query = buildSearchQuery("tests for subject behaviour")
			case "funded-blocks":
				// Caller has already charged type-card and signature-type blocks.
				budget = searchContainedCallableResultBudget(serializedSearchResultBytes(before)+1000, 1000)
			case "source-mismatch":
				before[2].Snippet = "not the captured source"
			}
			after := expandSearchRankedHeadTestContract(before, query, read, budget)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("%s changed protected results", name)
			}
		})
	}
}

func TestRankedHeadTestContractLargeFileRemainsBoundedAndHonest(t *testing.T) {
	t.Parallel()
	before, read := rankedHeadTestContractFixture(140)
	after := expandSearchRankedHeadTestContract(before, buildSearchQuery("subject behaviour"), read, 16384)
	entry := after[2]
	if entry.EndLine-entry.StartLine+1 > searchHeadWindowLines || entry.EndLine == 140 ||
		!containsString(entry.Signals, searchHeadWindowSignal) ||
		containsString(entry.Signals, searchCompleteSymbolSignal) || containsString(entry.Signals, searchFullUnitSignal) ||
		!searchRankedTestSnippetIsExactSubset(entry, before[2]) ||
		serializedSearchResultBytes([]SearchResult{entry})-serializedSearchResultBytes([]SearchResult{before[2]}) > searchRankedTestContractGrowthBytes {
		t.Fatalf("unbounded or untruthful head window: %#v", entry)
	}
}

func TestRankedHeadTestContractPublicQueryKeepsTopKProjection(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "service.js", `export function resolveTerminalStop() { return "already_terminating"; }
export const api = { stopSession: () => resolveTerminalStop() };
`)
	writeFile(t, repo, "tests/terminal.integration.test.js", `import { resolveTerminalStop } from "../service.js";
describe("terminal lifecycle", () => {
  it("returns already_terminating for a terminal phase", () => {
    expect(resolveTerminalStop()).toBe("already_terminating");
  });
});
`)
	query := "How does resolveTerminalStop return already_terminating for a terminal phase and which production caller establishes this?"
	search := func(topK int) SearchResponse {
		response, err := SearchRepository(t.Context(), repo, "test", query, SearchOptions{
			Worktree: true, Profile: ProfileFull, CacheDir: t.TempDir(), TopK: topK, MaxContextBytes: 16384,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	small, large := search(8), search(16)
	var window SearchResult
	for _, result := range small.Results {
		if result.FilePath == "tests/terminal.integration.test.js" && result.SymbolID == "" &&
			containsString(result.Signals, searchHeadWindowSignal) {
			window = result
			break
		}
	}
	if window.FilePath == "" || window.Rank > searchEnclosureHeadRanks ||
		window.SnippetStartLine != 1 || window.SnippetEndLine != 6 {
		t.Fatalf("public query failed to render ranked symbol-less contract: %#v", small.Results)
	}
	if len(large.Results) < len(small.Results) || !reflect.DeepEqual(small.Results, large.Results[:len(small.Results)]) {
		t.Fatalf("TopK changed protected projection:\nsmall: %#v\nlarge: %#v", small.Results, large.Results)
	}
	if len(small.Results) != 3 || small.Results[0].QualifiedName != "resolveTerminalStop" ||
		small.Results[2].QualifiedName != "api.stopSession" || window.Rank != 2 ||
		containsString(small.Results[0].Signals, "retrieval_mode=prose-parent") {
		t.Fatalf("fixture did not exercise callable-enabled ordinary mode: %#v", small.Results)
	}
}

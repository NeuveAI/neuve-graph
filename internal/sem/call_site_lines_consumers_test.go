package sem

import (
	"strings"
	"testing"
)

const consumersSource = `export function helper(): number {
  return 1;
}

export function double(value: number): number {
  return value * 2;
}

export function caller(xs: number[]): number {
  const a = 1;
  const b = 2;
  const mapped = xs.map(double); // reference-line
  return helper() + a + b + mapped.length; // call-line
}
`

const consumersTest = `import { helper } from "./a";

describe("suite", () => {
  it("works", () => {
    const z = 1;
    helper(); // test-call-line
  });
});
`

func consumersSnapshot(t *testing.T) (ProviderSnapshot, map[string]string) {
	t.Helper()
	files := map[string]string{"a.ts": consumersSource, "a.test.ts": consumersTest}
	return buildCallLineFixture(t, files), files
}

// Search's related-caller block and callee hop read the line of a call from
// the entry: they now land on the call, not on the caller's declaration.
// Reason: point 6 -- both printed the caller's first line before.
func TestCallSiteLinesReachSearchSites(t *testing.T) {
	snapshot, _ := consumersSnapshot(t)
	relation, _ := callEntry(t, snapshot, "CALLS", "a.ts", "caller", ":helper")
	var caller SymbolRecord
	for _, symbol := range snapshot.Symbols {
		if symbol.ID == relation.FromID {
			caller = symbol
		}
	}
	want := markerLine(t, consumersSource, "call-line")
	if got := searchRelatedCallSiteLine(relation, caller); got != want {
		t.Fatalf("related caller site line %d, want the call's %d (caller declared at %d)", got, want, caller.StartLine)
	}
	if got := searchCalleeCallLine(relation, caller); got != want {
		t.Fatalf("callee hop call line %d, want %d", got, want)
	}
}

// The compact form carries the call's line and the reference form, and a
// file-level entry's line, which it carried as zeros before. Reason: point 5.
func TestCallSiteLinesSurviveTheCompactForm(t *testing.T) {
	snapshot, _ := consumersSnapshot(t)
	records := []any{snapshot.Header}
	for _, file := range snapshot.Files {
		records = append(records, file)
	}
	for _, symbol := range snapshot.Symbols {
		records = append(records, symbol)
	}
	for _, relation := range snapshot.Relations {
		records = append(records, relation)
	}
	records = append(records, SnapshotSummary{RecordType: "summary", Warnings: []ProviderWarning{}, PartialFailures: []PartialFailure{}})
	data, _ := encodeCompactFixture(t, records)
	decoded := map[string]Evidence{}
	for _, record := range decodedCompactRecords(t, data) {
		if relation, ok := record.(RelationRecord); ok && relation.Type == "CALLS" && len(relation.Evidence) == 1 {
			decoded[relation.FromID+"->"+relation.ToID] = relation.Evidence[0]
		}
	}
	checked := 0
	for _, relation := range snapshot.Relations {
		if relation.Type != "CALLS" {
			continue
		}
		got, ok := decoded[relation.FromID+"->"+relation.ToID]
		if !ok {
			t.Fatalf("compact form lost %s -> %s", relation.FromID, relation.ToID)
		}
		if got != relation.Evidence[0] {
			t.Fatalf("compact entry %+v, native %+v", got, relation.Evidence[0])
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("checked %d call entries, want the reference, the call and the file-level call", checked)
	}
	_, reference := callEntry(t, snapshot, "CALLS", "a.ts", "caller", ":double")
	if reference.Form != evidenceFormReference || reference.StartLine != markerLine(t, consumersSource, "reference-line") {
		t.Fatalf("reference entry %+v", reference)
	}
	_, fileLevel := callEntry(t, snapshot, "CALLS", "a.test.ts", "", ":helper")
	if fileLevel.StartLine != markerLine(t, consumersTest, "test-call-line") {
		t.Fatalf("file-level entry %+v", fileLevel)
	}
}

// SCIP turns each call entry with a line into a reference occurrence: it is
// now one line at the call, where it covered the caller's whole span, and a
// file-level call, which had no line and was counted missing, is now an
// occurrence. Reason: point 6.
func TestCallSiteLinesReachSCIPOccurrences(t *testing.T) {
	snapshot, _ := consumersSnapshot(t)
	records := []any{snapshot.Header}
	for _, file := range snapshot.Files {
		records = append(records, file)
	}
	for _, symbol := range snapshot.Symbols {
		records = append(records, symbol)
	}
	for _, relation := range snapshot.Relations {
		records = append(records, relation)
	}
	records = append(records, SnapshotSummary{RecordType: "summary", Warnings: []ProviderWarning{}, PartialFailures: []PartialFailure{}})
	index, note := encodeSCIP(t, records...)
	want := map[string]int{
		"a.ts":      markerLine(t, consumersSource, "call-line"),
		"a.test.ts": markerLine(t, consumersTest, "test-call-line"),
	}
	for _, document := range index.GetDocuments() {
		line, ok := want[document.GetRelativePath()]
		if !ok {
			continue
		}
		for _, occurrence := range document.GetOccurrences() {
			span, ok := occurrence.SourceRange()
			if ok && strings.Contains(occurrence.GetSymbol(), "/helper(") && occurrence.GetSymbolRoles() == 0 &&
				int(span.Start.Line) == line-1 && int(span.End.Line) == line {
				delete(want, document.GetRelativePath())
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("no one-line reference occurrence to helper at the call in %v (note %+v)", want, note)
	}
}

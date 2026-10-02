package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func frontierSnapshot(ids ...string) sem.ProviderSnapshot {
	snapshot := sem.ProviderSnapshot{Header: sem.SnapshotHeader{
		LanguageTiers: map[string]string{"fixture-language": "semantic", "notes": "inventory-only"},
		Completeness:  sem.CompletenessReport{Health: sem.GraphHealth{Status: "ok"}},
	}}
	for _, id := range ids {
		snapshot.Symbols = append(snapshot.Symbols, sem.SymbolRecord{
			ID: id, Name: id, QualifiedName: id, Kind: "function", Language: "fixture-language",
			FilePath: id + ".fixture", StartLine: 1, EndLine: 1,
		})
	}
	return snapshot
}

func frontierFlags() neighborFlags {
	return neighborFlags{Symbol: "A", Relation: "CALLS", Selection: "exact", Direction: "out", Depth: 1, Limit: 20, Summary: "frontier", Format: "json"}
}

func assertFrontierJSON(t *testing.T, response neighborResponse, want string) {
	t.Helper()
	encoded, err := json.Marshal(response.Summary)
	if err != nil {
		t.Fatal(err)
	}
	var gotJSON, wantJSON any
	if err := json.Unmarshal(encoded, &gotJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("summary\ngot  %s\nwant %s", encoded, want)
	}
}

func TestNeighborFrontierLiteralChainAndNoOption(t *testing.T) {
	snapshot := frontierSnapshot("A", "B", "C", "D")
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS", Resolution: "resolved"},
		{FromID: "B", ToID: "C", Type: "CALLS", Resolution: "resolved"},
		{FromID: "C", ToID: "D", Type: "CALLS", Resolution: "resolved"},
	}
	flags := frontierFlags()
	response := buildNeighborResponseFromReader(snapshot, flags, nil)
	assertFrontierJSON(t, response, `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":1,"emitted_edges":1,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":"resolved","edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0}]}`)
	if len(response.Matches) != 1 || len(response.Matches[0].Outgoing) != 1 || response.Matches[0].Outgoing[0].Endpoint.ID != "B" {
		t.Fatalf("chain direct observations changed: %#v", response.Matches)
	}
	flags.Summary = ""
	ordinary := buildNeighborResponseFromReader(snapshot, flags, nil)
	before, err := json.Marshal(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte(`"summary"`)) {
		t.Fatalf("ordinary response has a summary: %s", before)
	}
	response.Summary = nil
	after, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("enrichment changed ordinary fields\nbefore %s\nafter %s", before, after)
	}
	flags.Symbol = "B"
	continued := buildNeighborResponseFromReader(snapshot, flags, nil)
	if len(continued.Matches[0].Outgoing) != 1 || continued.Matches[0].Outgoing[0].Endpoint.ID != "C" {
		t.Fatalf("explicit follow-up failed: %#v", continued.Matches)
	}
}

func TestNeighborFrontierLiteralFanoutAndDiamond(t *testing.T) {
	snapshot := frontierSnapshot("A", "B", "C", "D")
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS"},
		{FromID: "A", ToID: "C", Type: "CALLS"},
		{FromID: "A", ToID: "D", Type: "CALLS"},
		{FromID: "B", ToID: "D", Type: "CALLS"},
		{FromID: "C", ToID: "D", Type: "CALLS"},
	}
	flags := frontierFlags()
	flags.Limit = 2
	response := buildNeighborResponseFromReader(snapshot, flags, nil)
	assertFrontierJSON(t, response, `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":3,"emitted_edges":2,"omitted_edges":1,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":null,"edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0},{"kind":"indexed","endpoint_id":"C","counts":[{"relation":"CALLS","resolution":null,"edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0}]}`)
	if !response.Truncated || response.Matches[0].Outgoing[0].Endpoint.ID != "B" || response.Matches[0].Outgoing[1].Endpoint.ID != "C" {
		t.Fatalf("capped existing order changed: %#v", response)
	}
}

func TestNeighborFrontierLiteralCycleAndSelfLoop(t *testing.T) {
	snapshot := frontierSnapshot("A", "B")
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS"},
		{FromID: "B", ToID: "A", Type: "CALLS"},
		{FromID: "A", ToID: "A", Type: "CALLS"},
	}
	response := buildNeighborResponseFromReader(snapshot, frontierFlags(), nil)
	assertFrontierJSON(t, response, `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":2,"emitted_edges":2,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"A","counts":[{"relation":"CALLS","resolution":null,"edges":2}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":1,"self_edges":1},{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":null,"edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":1,"self_edges":0}]}`)
}

func TestNeighborFrontierLiteralRecordMultiplicity(t *testing.T) {
	snapshot := frontierSnapshot("A", "B", "C")
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS", Evidence: []sem.Evidence{{Kind: "call", StartLine: 1}}},
		{FromID: "A", ToID: "B", Type: "CALLS", Evidence: []sem.Evidence{{Kind: "call", StartLine: 2}}},
		{FromID: "B", ToID: "C", Type: "CALLS", Evidence: []sem.Evidence{{Kind: "call", StartLine: 3}}},
		{FromID: "B", ToID: "C", Type: "CALLS", Evidence: []sem.Evidence{{Kind: "call", StartLine: 4}}},
	}
	response := buildNeighborResponseFromReader(snapshot, frontierFlags(), nil)
	assertFrontierJSON(t, response, `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":2,"emitted_edges":2,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":null,"edges":2}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0}]}`)
	if len(response.Matches[0].Outgoing) != 2 {
		t.Fatalf("duplicate relation records were deduplicated: %#v", response.Matches)
	}
}

func TestNeighborFrontierLiteralSelectionAndResolution(t *testing.T) {
	snapshot := frontierSnapshot("A", "B", "C", "D", "E")
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS"},
		{FromID: "A", ToID: "C", Type: "CONSTRUCTS"},
		{FromID: "B", ToID: "D", Type: "CALLS", Resolution: "resolved"},
		{FromID: "B", ToID: "E", Type: "CONSTRUCTS", Resolution: "provider-future-label"},
		{FromID: "B", ToID: "A", Type: "ASYNC_CALLS"},
	}
	flags := frontierFlags()
	assertFrontierJSON(t, buildNeighborResponseFromReader(snapshot, flags, nil), `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":1,"emitted_edges":1,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":"resolved","edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0}]}`)
	flags.Selection = "family"
	assertFrontierJSON(t, buildNeighborResponseFromReader(snapshot, flags, nil), `{"mode":"frontier","kind":"observed","selection":"family","selected_relations":["CALLS","CONSTRUCTS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":2,"emitted_edges":2,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":"resolved","edges":1},{"relation":"CONSTRUCTS","resolution":"provider-future-label","edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0},{"kind":"indexed","endpoint_id":"C","counts":[],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0}]}`)
}

func TestNeighborFrontierLiteralDirection(t *testing.T) {
	snapshot := frontierSnapshot("A", "B", "X", "Y")
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "X", ToID: "A", Type: "CALLS"},
		{FromID: "A", ToID: "B", Type: "CALLS"},
		{FromID: "Y", ToID: "X", Type: "CALLS"},
	}
	flags := frontierFlags()
	assertFrontierJSON(t, buildNeighborResponseFromReader(snapshot, flags, nil), `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":1,"emitted_edges":1,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0}]}`)
	flags.Direction = "in"
	assertFrontierJSON(t, buildNeighborResponseFromReader(snapshot, flags, nil), `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"in","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":1,"emitted_edges":1,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"X","counts":[{"relation":"CALLS","resolution":null,"edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0}]}`)
}

func TestNeighborFrontierLiteralResolutionOrderAndEmptyView(t *testing.T) {
	snapshot := frontierSnapshot("A", "B", "C")
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS"},
		{FromID: "B", ToID: "C", Type: "CALLS", Resolution: "z_future"},
		{FromID: "B", ToID: "C", Type: "CALLS", Resolution: "a_future"},
		{FromID: "B", ToID: "C", Type: "CALLS"},
	}
	assertFrontierJSON(t, buildNeighborResponseFromReader(snapshot, frontierFlags(), nil), `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":1,"emitted_edges":1,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":null,"edges":1},{"relation":"CALLS","resolution":"a_future","edges":1},{"relation":"CALLS","resolution":"z_future","edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0}]}`)
	snapshot = frontierSnapshot("A")
	snapshot.Header.Completeness.Health.Status = ""
	assertFrontierJSON(t, buildNeighborResponseFromReader(snapshot, frontierFlags(), nil), `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"unknown","direct":{"eligible_edges":0,"emitted_edges":0,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[]}`)
}

func TestNeighborFrontierLiteralMissingExternalAndPartial(t *testing.T) {
	snapshot := frontierSnapshot("A", "B")
	snapshot.Externals = []sem.ExternalRecord{{ID: "external", Kind: "unresolved", Value: "missing", External: true}}
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS"},
		{FromID: "A", ToID: "external", Type: "CALLS", Resolution: "unresolved"},
		{FromID: "A", ToID: "absent", Type: "CALLS"},
		{FromID: "B", ToID: "external", Type: "CALLS", Resolution: "unresolved"},
		{FromID: "B", ToID: "absent", Type: "CALLS"},
	}
	snapshot.Header.Completeness.Health.Status = "degraded"
	snapshot.Header.PartialFailures = []sem.PartialFailure{{Code: "E_PARSE", FilePath: "broken.fixture"}}
	snapshot.Header.Warnings = []sem.ProviderWarning{{Code: "W_FILE_LIMIT", Detail: "fixture cap"}}
	response := buildNeighborResponseFromReader(snapshot, frontierFlags(), nil)
	assertFrontierJSON(t, response, `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"degraded","direct":{"eligible_edges":2,"emitted_edges":2,"omitted_edges":0,"missing_endpoint_edges":1},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":"unresolved","edges":1}],"external_edges":1,"missing_endpoint_edges":1,"returns_to_focus":0,"self_edges":0},{"kind":"unavailable","endpoint_id":"external","reason":"external_endpoint"}]}`)
	if !reflect.DeepEqual(response.PartialFailures, snapshot.Header.PartialFailures) || !reflect.DeepEqual(response.Warnings, snapshot.Header.Warnings) || !reflect.DeepEqual(response.Completeness, snapshot.Header.Completeness) {
		t.Fatal("summary changed extraction diagnostics")
	}
}

func TestNeighborFrontierLiteralNonCodeAndFilters(t *testing.T) {
	snapshot := frontierSnapshot("A", "B", "test", "doc", "unknown")
	snapshot.Symbols[2].FilePath = "tests/check.fixture"
	snapshot.Symbols[3].Language = "notes"
	snapshot.Symbols[3].Kind = "document"
	snapshot.Symbols[4].Language = ""
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS"},
		{FromID: "A", ToID: "doc", Type: "CALLS", Resolution: "name_only"},
		{FromID: "A", ToID: "test", Type: "CALLS"},
		{FromID: "A", ToID: "unknown", Type: "CALLS"},
		{FromID: "A", ToID: "B", Type: "FILE_CHANGES_WITH"},
		{FromID: "B", ToID: "test", Type: "CALLS"},
		{FromID: "test", ToID: "B", Type: "CALLS"},
	}
	flags := frontierFlags()
	response := buildNeighborResponseFromReader(snapshot, flags, nil)
	assertFrontierJSON(t, response, `{"mode":"frontier","kind":"observed","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok","direct":{"eligible_edges":4,"emitted_edges":4,"omitted_edges":0,"missing_endpoint_edges":0},"frontier":[{"kind":"indexed","endpoint_id":"B","counts":[{"relation":"CALLS","resolution":null,"edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0},{"kind":"unavailable","endpoint_id":"doc","reason":"inventory_only_endpoint"},{"kind":"indexed","endpoint_id":"test","counts":[{"relation":"CALLS","resolution":null,"edges":1}],"external_edges":0,"missing_endpoint_edges":0,"returns_to_focus":0,"self_edges":0},{"kind":"unavailable","endpoint_id":"unknown","reason":"unclassified_endpoint"}]}`)
	flags.ExcludeTests = true
	response = buildNeighborResponseFromReader(snapshot, flags, nil)
	if response.Summary.Direct.EligibleEdges != 3 || len(response.Summary.Frontier) != 3 || len(response.Summary.Frontier[0].Counts) != 0 {
		t.Fatalf("test endpoint filtering diverged: %#v", response.Summary)
	}
	snapshot.Externals = []sem.ExternalRecord{{ID: "external", Value: "unresolved", External: true}}
	snapshot.Relations = []sem.RelationRecord{
		{FromID: "A", ToID: "B", Type: "CALLS"},
		{FromID: "A", ToID: "external", Type: "CALLS"},
		{FromID: "B", ToID: "external", Type: "CALLS"},
	}
	flags.InternalOnly = true
	response = buildNeighborResponseFromReader(snapshot, flags, nil)
	if response.Summary.Direct.EligibleEdges != 1 || len(response.Summary.Frontier) != 1 || len(response.Summary.Frontier[0].Counts) != 0 {
		t.Fatalf("external endpoint filtering diverged: %#v", response.Summary)
	}
}

func TestNeighborFrontierUnresolvedFocus(t *testing.T) {
	flags := frontierFlags()
	assertFrontierJSON(t, buildNeighborResponseFromReader(frontierSnapshot(), flags, nil), `{"mode":"frontier","kind":"unresolved_focus","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok"}`)
	snapshot := frontierSnapshot("A", "other")
	snapshot.Symbols[0].Name = "shared"
	snapshot.Symbols[1].Name = "shared"
	flags.Symbol = "shared"
	assertFrontierJSON(t, buildNeighborResponseFromReader(snapshot, flags, nil), `{"mode":"frontier","kind":"unresolved_focus","selection":"exact","selected_relations":["CALLS"],"direction":"out","depth":1,"indexed_coverage":"ok"}`)
}

func TestNeighborFrontierFlagsCapabilityAndCommand(t *testing.T) {
	for _, suffix := range [][]string{
		{"--summary", "unknown"}, {"--summary", ""}, {"--summary"},
		{"--summary", "frontier", "--depth", "2"},
		{"--summary", "frontier", "--direction", "both"},
		{"--summary", "frontier", "--format", "text"},
		{"--summary", "frontier", "--format", "agent"},
	} {
		if _, err := parseNeighborFlags(append([]string{"--symbol", "A", "--direction", "out"}, suffix...)); err == nil {
			t.Fatalf("invalid summary flags accepted: %v", suffix)
		}
	}
	if !sem.Capabilities().OptionalLocalOnlyFeatures["neighbor_frontier_summary"] {
		t.Fatal("summary capability absent")
	}
	repo := t.TempDir()
	write(t, repo, "chain.ts", "function A() { B(); }\nfunction B() { C(); }\nfunction C() {}\n")
	var out bytes.Buffer
	err := Run(t.Context(), Options{Version: "test", Stdout: &out}, []string{"neighbors", "--repo", repo, "--symbol", "A", "--direction", "out", "--selection", "exact", "--summary", "frontier", "--no-cache"})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Summary struct {
			Kind     string `json:"kind"`
			Frontier []struct {
				Kind   string                  `json:"kind"`
				Counts []neighborFrontierCount `json:"counts"`
			} `json:"frontier"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Summary.Kind != "observed" || len(response.Summary.Frontier) != 1 || response.Summary.Frontier[0].Kind != "indexed" || len(response.Summary.Frontier[0].Counts) != 1 || response.Summary.Frontier[0].Counts[0].Edges != 1 {
		t.Fatalf("command did not return the literal chain boundary: %s", out.String())
	}
	if !strings.Contains(out.String(), `"direction":"out"`) {
		t.Fatalf("missing scope: %s", out.String())
	}
}

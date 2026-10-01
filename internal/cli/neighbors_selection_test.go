package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func TestNeighborsRelationSelectionCommand(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, "calls.ts", "class Built {}\nfunction helper() {}\nfunction focus() { helper(); new Built(); }\nfunction caller() { focus(); }\n")
	for _, tc := range []struct {
		mode      string
		relations []string
	}{
		{"", []string{"CALLS", "CONSTRUCTS"}}, {"family", []string{"CALLS", "CONSTRUCTS"}}, {"exact", []string{"CALLS"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			args := []string{"neighbors", "--repo", repo, "--symbol", "focus", "--direction", "both", "--depth", "2", "--no-cache"}
			if tc.mode != "" {
				args = append(args, "--selection", tc.mode)
			}
			var out bytes.Buffer
			if err := Run(t.Context(), Options{Version: "test", Stdout: &out}, args); err != nil {
				t.Fatal(err)
			}
			var response neighborResponse
			if err := json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			mode := tc.mode
			if mode == "" {
				mode = "family"
			}
			if response.Selection != mode || !reflect.DeepEqual(response.SelectedRelations, tc.relations) {
				t.Fatalf("selection envelope: %#v", response)
			}
			if len(response.Matches) != 1 {
				t.Fatalf("matches=%#v", response.Matches)
			}
			match := response.Matches[0]
			if len(match.Incoming) != 1 || match.Incoming[0].Endpoint.Name != "caller" || match.Incoming[0].Relation != "CALLS" {
				t.Fatalf("incoming=%#v", match.Incoming)
			}
			got := []string{}
			for _, edge := range match.Outgoing {
				got = append(got, edge.Relation)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.relations) {
				t.Fatalf("outgoing=%v want %v", got, tc.relations)
			}
			if len(match.Paths) != len(tc.relations) {
				t.Fatalf("paths=%#v", match.Paths)
			}
			if mode == "exact" && match.Paths[0].Callee.Name != "helper" {
				t.Fatalf("exact path includes constructor: %#v", match.Paths)
			}
		})
	}
}

func TestNeighborsSelectionSingletonAndDiagnostics(t *testing.T) {
	snapshot := sem.ProviderSnapshot{Symbols: []sem.SymbolRecord{{ID: "focus", Name: "focus", Kind: "function"}, {ID: "target", Name: "target", Kind: "function"}}, Relations: []sem.RelationRecord{{FromID: "focus", ToID: "target", Type: "IMPORTS"}, {FromID: "focus", ToID: "target", Type: "CALLS"}}, Header: sem.SnapshotHeader{PartialFailures: []sem.PartialFailure{{Code: "E_FIXTURE", FilePath: "broken.fixture"}}}}
	for _, mode := range []string{"exact", "family"} {
		response := buildNeighborResponseFromReader(snapshot, neighborFlags{Symbol: "focus", Relation: "IMPORTS", Selection: mode, Direction: "out", Depth: 1, Limit: 20}, nil)
		if !reflect.DeepEqual(response.SelectedRelations, []string{"IMPORTS"}) || len(response.Matches[0].Outgoing) != 1 || response.Matches[0].Outgoing[0].Relation != "IMPORTS" || len(response.PartialFailures) != 1 {
			t.Fatalf("singleton selection=%#v", response)
		}
	}
	if _, err := parseNeighborFlags([]string{"--symbol", "focus", "--selection", "unknown"}); err == nil {
		t.Fatal("invalid mode accepted")
	}
	capability := sem.Capabilities()
	if !reflect.DeepEqual(capability.RelationSelectionModes, []string{"exact", "family"}) || !reflect.DeepEqual(capability.RelationFamilies["CALLS"], []string{"CALLS", "CONSTRUCTS"}) {
		t.Fatalf("capabilities=%#v", capability)
	}
}

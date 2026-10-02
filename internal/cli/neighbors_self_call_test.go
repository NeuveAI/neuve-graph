package cli

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNeighborsNamedSelfCallSiteAndFrontier(t *testing.T) {
	for _, extension := range []string{"js", "ts"} {
		for _, tc := range []struct {
			name, source string
			line         int
		}{
			{"multiline", "export function self() {\n  return self();\n}\n", 2},
			{"same line", "export function self() { return self(); }\n", 1},
		} {
			t.Run(extension+"/"+tc.name, func(t *testing.T) {
				repo := t.TempDir()
				write(t, repo, "focus."+extension, tc.source)
				for _, direction := range []string{"out", "in"} {
					var out bytes.Buffer
					if err := Run(t.Context(), Options{Version: "test", Stdout: &out}, []string{"neighbors", "--repo", repo, "--symbol", "self", "--direction", direction, "--relation", "CALLS", "--selection", "exact", "--summary", "frontier", "--no-cache", "--format", "json"}); err != nil {
						t.Fatal(err)
					}
					var response struct {
						Matches []neighborFocus `json:"matches"`
						Summary *struct {
							Direct   neighborFrontierDirect `json:"direct"`
							Frontier []struct {
								SelfEdges      int `json:"self_edges"`
								ReturnsToFocus int `json:"returns_to_focus"`
							} `json:"frontier"`
						} `json:"summary"`
					}
					if err := json.Unmarshal(out.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if len(response.Matches) != 1 || response.Matches[0].Symbol.Name != "self" {
						t.Fatalf("source focus missing: %s", out.String())
					}
					focus := response.Matches[0]
					edges := focus.Outgoing
					if direction == "in" {
						edges = focus.Incoming
					}
					if len(edges) != 1 || edges[0].Relation != "CALLS" || edges[0].Endpoint.ID != focus.Symbol.ID || edges[0].Resolution != "exact" {
						t.Fatalf("%s self record = %#v", direction, edges)
					}
					if len(edges[0].Evidence) != 1 || edges[0].Evidence[0].StartLine != tc.line || edges[0].Evidence[0].EndLine != tc.line {
						t.Fatalf("body call-site evidence = %#v", edges[0].Evidence)
					}
					if direction == "in" && (edges[0].CallSite == nil || edges[0].CallSite.Line != tc.line) {
						t.Fatalf("incoming call site = %#v, want line %d", edges[0].CallSite, tc.line)
					}
					if response.Summary == nil || response.Summary.Direct.EligibleEdges != 1 || response.Summary.Direct.EmittedEdges != 1 || len(response.Summary.Frontier) != 1 || response.Summary.Frontier[0].SelfEdges != 1 || response.Summary.Frontier[0].ReturnsToFocus != 1 {
						t.Fatalf("self frontier = %#v", response.Summary)
					}
				}
			})
		}
	}
}

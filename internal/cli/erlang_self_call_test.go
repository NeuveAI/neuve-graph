package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

const erlangCLIProofID = "local/erlang-proof:Erlang:recursion.erl:function:self_call"

func TestErlangNamedSelfCallNeighbors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{{"multiline expression", "    self_call(\n        N-1\n    ).\n", 1}, {"anonymous only", "    fun(X) -> self_call(X-1) end.\n", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "erlang-proof")
			write(t, repo, "recursion.erl", "-module(recursion).\n-export([self_call/1]).\nself_call(N) ->\n"+tc.body)
			for _, direction := range []string{"in", "out"} {
				var out bytes.Buffer
				if err := Run(t.Context(), Options{Version: "dev", Stdout: &out}, []string{"neighbors", "--repo", repo, "--symbol", erlangCLIProofID, "--direction", direction, "--relation", "CALLS", "--selection", "exact", "--summary", "frontier", "--profile", "full", "--no-cache", "--format", "json"}); err != nil {
					t.Fatal(err)
				}
				var response struct {
					Matches []neighborFocus `json:"matches"`
				}
				if err := json.Unmarshal(out.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Matches) != 1 || response.Matches[0].Symbol.ID != erlangCLIProofID {
					t.Fatalf("literal source missing: %s", out.Bytes())
				}
				focus := response.Matches[0]
				edges := focus.Outgoing
				if direction == "in" {
					edges = focus.Incoming
				}
				if len(edges) != tc.want {
					t.Fatalf("%s edges=%#v want %d", direction, edges, tc.want)
				}
				if tc.want == 1 {
					e := edges[0]
					if e.Endpoint.ID != erlangCLIProofID || e.Resolution != "exact" || e.Confidence != .92 || len(e.Evidence) != 1 || e.Evidence[0].StartLine != 4 || e.Evidence[0].EndLine != 6 || e.Evidence[0].Kind != "call_site" || e.Evidence[0].Detail != "self_call" {
						t.Fatalf("native witness=%#v", e)
					}
					if direction == "in" && (e.CallSite == nil || e.CallSite.Line != 4) {
						t.Fatalf("incoming callsite=%#v", e.CallSite)
					}
				}
				var summary struct {
					Summary struct {
						Direct   neighborFrontierDirect `json:"direct"`
						Frontier []struct {
							SelfEdges      int `json:"self_edges"`
							ReturnsToFocus int `json:"returns_to_focus"`
						} `json:"frontier"`
					} `json:"summary"`
				}
				if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
					t.Fatal(err)
				}
				if summary.Summary.Direct.EligibleEdges != tc.want || summary.Summary.Direct.EmittedEdges != tc.want {
					t.Fatalf("native direct frontier=%#v", summary)
				}
				if tc.want == 1 && (len(summary.Summary.Frontier) != 1 || summary.Summary.Frontier[0].SelfEdges != 1 || summary.Summary.Frontier[0].ReturnsToFocus != 1) {
					t.Fatalf("native self frontier=%#v", summary)
				}
			}
		})
	}
}

func TestErlangSelfCallNativeCaches(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "erlang-proof")
	write(t, repo, "recursion.erl", "-module(recursion).\n-export([self_call/1]).\nself_call(N) ->\n    self_call(N-1).\n")
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "Erlang native cache control")
	beforeBinary := os.Getenv("GRAPH_SELF_CALL_BEFORE_BINARY")
	for _, family := range []string{"search", "records"} {
		t.Run(family, func(t *testing.T) {
			cacheDir := t.TempDir()
			args := []string{"snapshot", "--repo", repo, "--cache-dir", cacheDir, "--profile", "full", "--format", "ndjson"}
			if family == "search" {
				args = []string{"neighbors", "--repo", repo, "--head", "--symbol", erlangCLIProofID, "--direction", "out", "--selection", "exact", "--profile", "full", "--cache-dir", cacheDir, "--format", "json"}
			}
			var beforeTree string
			if beforeBinary != "" {
				command := exec.CommandContext(t.Context(), beforeBinary, args...)
				var stderr bytes.Buffer
				command.Stderr = &stderr
				before, err := command.Output()
				if err != nil {
					t.Fatalf("baseline: %v %s", err, stderr.Bytes())
				}
				beforeTree = assertErlangNativeCacheOutput(t, family, before, 0)
				entries, err := os.ReadDir(filepath.Join(cacheDir, family))
				if err != nil || len(entries) != 1 {
					t.Fatalf("baseline actual namespace: %v %#v", err, entries)
				}
				t.Logf("baseline %s: tree=%s ID=%s self=0 actual_namespace=%s", family, beforeTree, erlangCLIProofID, entries[0].Name())
			} else {
				t.Log("historical baseline unavailable; historical proof not claimed")
			}
			var cold []byte
			for attempt := 0; attempt < 2; attempt++ {
				var out bytes.Buffer
				if err := Run(t.Context(), Options{Version: "dev", Stdout: &out}, args); err != nil {
					t.Fatal(err)
				}
				tree := assertErlangNativeCacheOutput(t, family, out.Bytes(), 1)
				if beforeTree != "" && tree != beforeTree {
					t.Fatal("candidate changed baseline tree")
				}
				if family == "search" {
					var response neighborResponse
					if err := json.Unmarshal(out.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if response.IndexCacheHit != (attempt == 1) {
						t.Fatalf("candidate attempt=%d hit=%t", attempt, response.IndexCacheHit)
					}
				}
				if attempt == 0 {
					cold = bytes.Clone(out.Bytes())
				} else if family == "records" && !bytes.Equal(cold, out.Bytes()) {
					t.Fatal("opaque warm changed finished bytes")
				}
			}
			if beforeBinary != "" {
				entries, err := os.ReadDir(filepath.Join(cacheDir, family))
				if err != nil || len(entries) != 2 {
					t.Fatalf("old namespace lost or reused: %v %#v", err, entries)
				}
				t.Logf("candidate %s: unchanged source/tree, self=1 cold/warm, both namespaces retained", family)
			}
		})
	}
}

func assertErlangNativeCacheOutput(t *testing.T, family string, output []byte, want int) string {
	t.Helper()
	if bytes.Contains(output, []byte("erlang_self_call")) {
		t.Fatal("private proof leaked to native public output")
	}
	if family == "search" {
		var response neighborResponse
		if err := json.Unmarshal(output, &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Matches) != 1 || response.Matches[0].Symbol.ID != erlangCLIProofID || response.Matches[0].Symbol.StartLine != 3 || response.Matches[0].Symbol.EndLine != 4 {
			t.Fatalf("native source owner changed: %s", output)
		}
		edges := response.Matches[0].Outgoing
		if len(edges) != want {
			t.Fatalf("native search self=%d want %d", len(edges), want)
		}
		for _, e := range edges {
			if e.Endpoint.ID != erlangCLIProofID || e.Resolution != "exact" || e.Confidence != .92 || len(e.Evidence) != 1 || e.Evidence[0].StartLine != 4 || e.Evidence[0].EndLine != 4 {
				t.Fatalf("cached native witness=%#v", e)
			}
		}
		return response.Tree
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var header sem.SnapshotHeader
	var symbols []sem.SymbolRecord
	var relations []sem.RelationRecord
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var kind struct {
			RecordType      string `json:"record_type"`
			ProviderVersion string `json:"provider_version"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			t.Fatal(err)
		}
		switch kind.RecordType {
		case "":
			if kind.ProviderVersion == "" {
				continue
			}
			if err := json.Unmarshal(raw, &header); err != nil {
				t.Fatal(err)
			}
		case "symbol":
			var s sem.SymbolRecord
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			symbols = append(symbols, s)
		case "relation":
			var r sem.RelationRecord
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			relations = append(relations, r)
		}
	}
	found := false
	for _, s := range symbols {
		if s.ID == erlangCLIProofID {
			found = true
			if s.StartLine != 3 || s.EndLine != 4 || s.Signature != "self_call(N)" {
				t.Fatalf("cached owner=%#v", s)
			}
		}
	}
	if !found || header.Tree == "" || header.ProviderVersion != "dev" {
		t.Fatalf("record source/header missing: %s", output)
	}
	count := 0
	for _, r := range relations {
		if r.Type == "CALLS" && r.FromID == erlangCLIProofID && r.ToID == erlangCLIProofID {
			count++
			if r.Resolution != "exact" || r.Confidence != .92 || r.RelationScope != "file" || r.TargetKind != "symbol" || len(r.Evidence) != 1 || r.Evidence[0].StartLine != 4 || r.Evidence[0].EndLine != 4 || r.Evidence[0].Detail != "self_call" {
				t.Fatalf("opaque final witness=%#v", r)
			}
		}
	}
	if count != want {
		t.Fatalf("native record self=%d want %d", count, want)
	}
	return header.Tree
}

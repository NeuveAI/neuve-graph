package sem

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const erlangProofID = "local/erlang-proof:Erlang:recursion.erl:function:self_call"
const erlangProofPrefix = "-module(recursion).\n-export([self_call/1]).\n"

func TestErlangNamedSelfCallSources(t *testing.T) {
	p := erlangProofPrefix
	both := "-module(recursion).\n-export([self_call/1, self_call/2]).\n"
	for _, tc := range []struct {
		name, source, function                   string
		ownerStart, ownerEnd, siteStart, siteEnd int
		parseError                               bool
	}{
		{"same line", p + "self_call(N) -> self_call(N-1).\n", "self_call", 3, 3, 3, 3, false},
		{"multiline", p + "self_call(N) ->\n    self_call(N-1).\n", "self_call", 3, 4, 4, 4, false},
		{"column zero", p + "self_call(N) ->\nself_call(N-1).\n", "self_call", 3, 4, 4, 4, false},
		{"zero arity", "-module(recursion).\n-export([self_call/0]).\nself_call() -> self_call().\n", "self_call", 3, 3, 3, 3, false},
		{"multiline expression", p + "self_call(N) ->\n    self_call(\n        N-1\n    ).\n", "self_call", 3, 6, 4, 6, false},
		{"later folded clause", p + "self_call(0) -> ok;\nself_call(N) ->\n    self_call(N-1).\n", "self_call", 3, 5, 5, 5, false},
		{"repeated", p + "self_call(N) ->\n    self_call(N-1),\n    self_call(N-2).\n", "self_call", 3, 5, 4, 4, false},
		{"anonymous first", p + "self_call(N) ->\n    F = fun(X) -> self_call(X-1) end,\n    F(N),\n    self_call(N-1).\n", "self_call", 3, 6, 6, 6, false},
		{"nested args", "-module(recursion).\n-export([self_call/2]).\nself_call(N, Acc) -> self_call({N, [N]}, (Acc-1)).\n", "self_call", 3, 3, 3, 3, false},
		{"comment args", p + "self_call(N) ->\n    self_call(\n        % only one argument\n        N-1\n    ).\n", "self_call", 3, 7, 4, 7, false},
		{"header", p + "self_call(N) -> N.\n", "self_call", 3, 3, 0, 0, false},
		{"indented header", p + "    self_call(N) -> N.\n", "self_call", 3, 3, 0, 0, false},
		{"anonymous only", p + "self_call(N) ->\n    fun(X) -> self_call(X-1) end.\n", "self_call", 3, 4, 0, 0, false},
		{"variable", p + "self_call(N) ->\n    F = fun(X) -> X end,\n    F(N).\n", "self_call", 3, 5, 0, 0, false},
		{"reference", p + "self_call(N) ->\n    {N, fun self_call/1}.\n", "self_call", 3, 4, 0, 0, false},
		{"other remote", p + "self_call(N) ->\n    other:self_call(N-1).\n", "self_call", 3, 4, 0, 0, false},
		{"same remote", p + "self_call(N) ->\n    recursion:self_call(N-1).\n", "self_call", 3, 4, 0, 0, false},
		{"wrong arity", p + "self_call(N) -> self_call(N, 0).\n", "self_call", 3, 3, 0, 0, false},
		{"text", p + "self_call(N) ->\n    % self_call(N-1)\n    {N, \"self_call(N-1)\", 'self_call(N-1)'}.\n", "self_call", 3, 5, 0, 0, false},
		{"arity change", both + "self_call(N) -> self_call(N, 0).\nself_call(N, Acc) -> Acc.\n", "self_call", 3, 3, 0, 0, false},
		{"sibling precision debt", both + "self_call(N) -> self_call(N).\nself_call(N, Acc) -> Acc.\n", "self_call", 3, 3, 3, 3, false},
		{"macro call", p + "self_call(N) -> ?SELF(N).\n", "self_call", 3, 3, 0, 0, false},
		{"macro args", "-module(recursion).\n-define(ARGS, N, N).\nself_call(N) -> self_call(?ARGS).\n", "self_call", 3, 3, 0, 0, false},
		{"conditional", "-module(recursion).\n-ifdef(DEBUG).\nself_call(N) -> self_call(N).\n-else.\nself_call(N) -> self_call(N).\n-endif.\n", "self_call", 3, 3, 0, 0, false},
		{"duplicate", "-module(recursion).\nself_call(N) -> self_call(N).\nbarrier() -> ok.\nself_call(N) -> self_call(N).\n", "self_call", 2, 2, 0, 0, false},
		{"patterns fold", p + "self_call(0) -> ok;\nself_call(N) -> self_call(N-1).\n", "self_call", 3, 4, 4, 4, false},
		{"malformed", p + "self_call(N) -> self_call(N-1.\n", "self_call", 3, 3, 0, 0, true},
		{"different name", "-module(recursion).\n-export([walk_down/1]).\nwalk_down(N) ->\n    walk_down(N-1).\n", "walk_down", 3, 4, 4, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "erlang-proof")
			writeFile(t, repo, "recursion.erl", tc.source)
			snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull})
			if err != nil {
				t.Fatal(err)
			}
			var owner SymbolRecord
			for _, s := range snapshot.Symbols {
				if s.Name == tc.function && s.Kind == "function" && s.StartLine == tc.ownerStart {
					owner = s
				}
			}
			if owner.ID == "" || owner.EndLine != tc.ownerEnd {
				t.Fatalf("source owner missing or changed: %#v", snapshot.Symbols)
			}
			id := "local/erlang-proof:Erlang:recursion.erl:function:" + tc.function
			if tc.name == "arity change" || tc.name == "sibling precision debt" || tc.name == "conditional" || tc.name == "duplicate" {
				id += "#sig:a1671487f7a14c50"
			}
			if owner.ID != id {
				t.Errorf("owner ID=%q want %q", owner.ID, id)
			}
			if tc.parseError != (len(snapshot.Header.PartialFailures) > 0) {
				t.Errorf("partial failures=%#v", snapshot.Header.PartialFailures)
			}
			count := 0
			for _, r := range snapshot.Relations {
				if r.Type != "CALLS" || r.FromID != owner.ID || r.ToID != owner.ID {
					continue
				}
				count++
				want := []Evidence{{Kind: "call_site", FilePath: "recursion.erl", StartLine: tc.siteStart, EndLine: tc.siteEnd, Detail: tc.function}}
				if r.Resolution != "exact" || r.Confidence != .92 || r.RelationScope != "file" || r.TargetKind != "symbol" || !reflect.DeepEqual(r.Evidence, want) || len(r.WarningCodes) != 0 {
					t.Errorf("self edge=%#v want evidence=%#v", r, want)
				}
			}
			want := 0
			if tc.siteStart > 0 {
				want = 1
			}
			if count != want {
				t.Errorf("self CALLS=%d want %d; source=%q", count, want, tc.source)
			}
			if tc.name == "sibling precision debt" {
				for _, r := range snapshot.Relations {
					if r.Type == "CALLS" && r.FromID == owner.ID && strings.HasSuffix(r.ToID, "#sig:b4f668b6005a1bf6") {
						t.Log("RETAINED NONSELF DEBT: self_call/1 falsely targets sibling self_call/2")
					}
				}
			}
		})
	}
}

func TestErlangSelfCallFinalization(t *testing.T) {
	for _, status := range []ParseStatus{{}, {ParseError: true}, {Partial: true}, {DepthExceeded: true}, {ParseError: true, DepthExceeded: true, Code: "E_PARSE_ERROR"}, {ParseError: true, Code: "E_PARSE_TIMEOUT"}} {
		t.Run(status.Code+"/"+strings.TrimSpace(strings.Join([]string{boolLabel(status.ParseError), boolLabel(status.Partial), boolLabel(status.DepthExceeded)}, " ")), func(t *testing.T) {
			entities := []Entity{{Kind: "function", Name: "self_call", Signature: "self_call(N)", StartLine: 3, EndLine: 5, sourceStartByte: 44, sourceEndByte: 63, erlangSelfCall: erlangSelfCallWitness{5, 5}}}
			before := entities[0]
			finalizeErlangSelfCalls(entities, status)
			want := before
			if status.ParseError || status.Partial || status.DepthExceeded {
				want.erlangSelfCall = erlangSelfCallWitness{}
			}
			if !reflect.DeepEqual(entities[0], want) {
				t.Fatalf("finalized=%#v want %#v", entities[0], want)
			}
		})
	}
}

func boolLabel(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestErlangNamedSelfCallProfiles(t *testing.T) {
	for _, positive := range []bool{true, false} {
		for _, profile := range []Profile{ProfileFull, ProfileFast, ProfileSyntaxOnly} {
			t.Run(string(profile)+"/"+boolLabel(positive), func(t *testing.T) {
				repo := filepath.Join(t.TempDir(), "erlang-proof")
				source := erlangProofPrefix + "self_call(N) -> N.\n"
				if positive {
					source = erlangProofPrefix + "self_call(N) ->\n    self_call(N-1).\n"
				}
				writeFile(t, repo, "recursion.erl", source)
				snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: profile})
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.Header.Profile != string(profile) || len(snapshot.Header.PartialFailures) != 0 {
					t.Fatalf("profile/health=%#v", snapshot.Header)
				}
				count := 0
				for _, r := range snapshot.Relations {
					if r.Type == "CALLS" && r.FromID == erlangProofID && r.ToID == erlangProofID {
						count++
						if r.Resolution != "exact" || r.Confidence != .92 {
							t.Fatalf("profile edge=%#v", r)
						}
						if profile == ProfileFull {
							if len(r.Evidence) != 1 || r.Evidence[0].StartLine != 4 || r.Evidence[0].EndLine != 4 {
								t.Fatalf("full witness=%#v", r)
							}
						} else if len(r.Evidence) != 0 {
							t.Fatalf("fast evidence=%#v", r)
						}
					}
				}
				want := 0
				if positive && profile != ProfileSyntaxOnly {
					want = 1
				}
				if count != want {
					t.Fatalf("self count=%d want %d", count, want)
				}
			})
		}
	}
}

func TestErlangSelfCallUnknownOutsideArity(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "erlang-proof")
	writeFile(t, repo, "recursion.erl", "-module(recursion).\n-define(ARGS, N, N).\nself_call(N) -> self_call(N).\nbarrier() -> ok.\nself_call(?ARGS) -> ok.\n")
	snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Header.PartialFailures) != 0 {
		t.Fatalf("literal must parse cleanly: %#v", snapshot.Header)
	}
	owners := 0
	for _, s := range snapshot.Symbols {
		if s.Name == "self_call" && s.Kind == "function" {
			owners++
		}
	}
	if owners != 2 {
		t.Fatalf("source inventory=%#v", snapshot.Symbols)
	}
	for _, r := range snapshot.Relations {
		if r.Type == "CALLS" && r.FromID == r.ToID {
			t.Fatalf("unknown outside arity admitted self=%#v", r)
		}
	}
}

func TestErlangSelfCallDepthStatus(t *testing.T) {
	source := erlangProofPrefix + "self_call(N) ->\n" + strings.Repeat("[\n", 5002) + "self_call(N-1)\n" + strings.Repeat("]\n", 5002) + ".\n"
	entities, language, status := (TreeSitterParser{}).ParseWithStatus("recursion.erl", source)
	if language != "Erlang" || !status.ParseError || !status.Partial || !status.DepthExceeded || status.Code != "E_PARSE_DEPTH_EXCEEDED" {
		t.Fatalf("actual depth status=%#v language=%s", status, language)
	}
	owners := 0
	for _, e := range entities {
		if e.Name == "self_call" {
			owners++
			if e.StartLine != 3 || e.EndLine != 10009 || e.erlangSelfCall.StartLine != 0 {
				t.Fatalf("depth owner=%#v", e)
			}
		}
	}
	if owners != 1 {
		t.Fatalf("depth source retained=%#v", entities)
	}
}

// Supplementary evidence added after the original red/green source oracle.
func TestErlangSelfCallAllOwnersAndDiagnosticSupplement(t *testing.T) {
	type owner struct {
		ID, Name, Signature string
		StartLine, EndLine  int
	}
	const first = erlangProofID + "#sig:a1671487f7a14c50"
	const second = erlangProofID + "#sig:b4f668b6005a1bf6"
	const malformedDetail = "tree-sitter syntax error nodes present: missing ) at line 3 column 30 near \"self_call(N) -> self_call(N-1.\""
	for _, tc := range []struct {
		name, source string
		owners       []owner
		selfCounts   map[string]int
		malformed    bool
	}{
		{"conditional all owners", "-module(recursion).\n-ifdef(DEBUG).\nself_call(N) -> self_call(N).\n-else.\nself_call(N) -> self_call(N).\n-endif.\n", []owner{
			{first, "self_call", "self_call(N)", 3, 3},
			{first + "#2", "self_call", "self_call(N)", 5, 5},
		}, map[string]int{first: 0, first + "#2": 0}, false},
		{"separated duplicate all owners", "-module(recursion).\nself_call(N) -> self_call(N).\nbarrier() -> ok.\nself_call(N) -> self_call(N).\n", []owner{
			{first, "self_call", "self_call(N)", 2, 2},
			{"local/erlang-proof:Erlang:recursion.erl:function:barrier", "barrier", "barrier()", 3, 3},
			{first + "#2", "self_call", "self_call(N)", 4, 4},
		}, map[string]int{first: 0, "local/erlang-proof:Erlang:recursion.erl:function:barrier": 0, first + "#2": 0}, false},
		{"sibling other arity", "-module(recursion).\n-export([self_call/1, self_call/2]).\nself_call(N) -> self_call(N).\nself_call(N, Acc) -> Acc.\n", []owner{
			{first, "self_call", "self_call(N)", 3, 3},
			{second, "self_call", "self_call(N, Acc)", 4, 4},
		}, map[string]int{first: 1, second: 0}, false},
		{"malformed exact diagnostic", erlangProofPrefix + "self_call(N) -> self_call(N-1.\n", []owner{
			{erlangProofID, "self_call", "self_call(N)", 3, 3},
		}, map[string]int{erlangProofID: 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "erlang-proof")
			writeFile(t, repo, "recursion.erl", tc.source)
			snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Worktree: true, Profile: ProfileFull})
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Files) != 1 || snapshot.Files[0].ID != "local/erlang-proof:file:recursion.erl" || snapshot.Files[0].Path != "recursion.erl" || snapshot.Files[0].Language != "Erlang" || snapshot.Files[0].Bytes != len(tc.source) {
				t.Fatalf("literal file not retained: %#v", snapshot.Files)
			}
			observed := []owner{}
			for _, s := range snapshot.Symbols {
				if s.Kind == "function" {
					if s.FilePath != "recursion.erl" || s.Language != "Erlang" {
						t.Errorf("owner attribution=%#v", s)
					}
					observed = append(observed, owner{s.ID, s.Name, s.Signature, s.StartLine, s.EndLine})
				}
			}
			if !reflect.DeepEqual(observed, tc.owners) {
				t.Fatalf("all owner inventory=%#v want %#v", observed, tc.owners)
			}
			counts := map[string]int{}
			for _, r := range snapshot.Relations {
				if r.Type != "CALLS" {
					continue
				}
				if r.FromID == r.ToID {
					counts[r.FromID]++
				}
				if tc.name == "sibling other arity" && r.FromID == first && r.ToID == second {
					t.Logf("RETAINED NONSELF DEBT, not repaired or accepted: %#v", r)
				}
			}
			for _, o := range tc.owners {
				if counts[o.ID] != tc.selfCounts[o.ID] {
					t.Errorf("owner %s self CALLS=%d want %d", o.ID, counts[o.ID], tc.selfCounts[o.ID])
				}
			}
			if tc.malformed {
				want := []PartialFailure{{Code: "E_PARSE_ERROR", Severity: "warning", FilePath: "recursion.erl", EffectOnCompleteness: "file parsed with syntax errors; semantic facts may be incomplete", Detail: malformedDetail}}
				if !reflect.DeepEqual(snapshot.Header.PartialFailures, want) {
					t.Fatalf("exact malformed diagnostic=%#v want %#v", snapshot.Header.PartialFailures, want)
				}
				t.Logf("SUPPLEMENTAL EXACT DIAGNOSTIC: %#v", snapshot.Header.PartialFailures[0])
			} else if len(snapshot.Header.PartialFailures) != 0 {
				t.Fatalf("clean literal diagnosed incomplete: %#v", snapshot.Header.PartialFailures)
			}
		})
	}
}

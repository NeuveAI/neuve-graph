package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func TestIDMatches(t *testing.T) {
	const id = "gh/apache/druid:Bash:distribution/docker/peon.sh:function:getConfPath"
	cases := []struct {
		sel  string
		want bool
	}{
		{id, true},                                     // exact full ID
		{"getConfPath", true},                          // trailing name segment
		{"function:getConfPath", true},                 // kind:name tail
		{"getConf", false},                             // partial word must not match
		{"peon.sh:function:getConfPath", false},        // not a ':'-aligned suffix (path uses '/')
		{"docker/peon.sh:function:getConfPath", false}, // path fragment, still not ':'-aligned
		{"otherName", false},
	}
	for _, tc := range cases {
		if got := idMatches(id, tc.sel); got != tc.want {
			t.Errorf("idMatches(id, %q) = %v, want %v", tc.sel, got, tc.want)
		}
	}
}

func TestRelationMatches(t *testing.T) {
	rel := sem.RelationRecord{
		FromID: "gh/o/r:Go:a.go:function:Caller",
		ToID:   "gh/o/r:Go:b.go:function:Callee",
		Type:   "CALLS",
	}
	mk := func(to, from string, relTypes ...string) providerFlags {
		return providerFlags{To: to, From: from, Relation: relTypes}
	}
	if !relationMatches(rel, mk("Callee", "", "CALLS")) {
		t.Error("expected match on to=Callee relation=CALLS")
	}
	if !relationMatches(rel, mk("Callee", "", "REFERENCES", "CALLS")) {
		t.Error("expected match when CALLS is one of several relation types")
	}
	if relationMatches(rel, mk("Callee", "", "REFERENCES")) {
		t.Error("must not match when relation type differs")
	}
	if relationMatches(rel, mk("Nope", "", "CALLS")) {
		t.Error("must not match when --to selector misses")
	}
	if !relationMatches(rel, mk("", "Caller")) {
		t.Error("expected match on from=Caller with no relation constraint")
	}
	if !relationMatches(rel, providerFlags{}) {
		t.Error("empty filter should match any edge")
	}
}

func TestWarnIfPartial(t *testing.T) {
	ok := &sem.SnapshotSummary{Stats: sem.ProviderStats{CompletenessLevel: "ok", Files: 10, ParsedFiles: 10}}
	var b strings.Builder
	warnIfPartial(&b, false, ok)
	if b.Len() != 0 {
		t.Errorf("clean run must be silent, got: %q", b.String())
	}

	// The subdir bug: 1 stray file, no --worktree → loud warning naming --worktree.
	subdir := &sem.SnapshotSummary{Stats: sem.ProviderStats{CompletenessLevel: "degraded", Files: 1, ParsedFiles: 1, Symbols: 0}}
	b.Reset()
	warnIfPartial(&b, false, subdir)
	out := b.String()
	if !strings.Contains(out, "DEGRADED") || !strings.Contains(out, "--worktree") {
		t.Errorf("subdir partial parse must warn loudly + suggest --worktree, got: %q", out)
	}

	// nil summary (stream produced none) must not panic.
	b.Reset()
	warnIfPartial(&b, false, nil)
}

// TestFilteredEdgesHonourExactCommit: the filtered path returns before any
// cache code, so it has its own branch to get the target from. Reason: a
// `--relation CALLS` read for an old commit must report that commit's calls,
// not HEAD's.
func TestFilteredEdgesHonourExactCommit(t *testing.T) {
	repo, old, _ := exactCommitStreamFixture(t)
	stream, err := runExactStream(t, "edges", "--repo", repo, "--commit", old, "--relation", "CALLS")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(stream), []byte("\n"))
	var header sem.SnapshotHeader
	if err := json.Unmarshal(lines[0], &header); err != nil || header.Commit != old {
		t.Fatalf("header commit=%q err=%v", header.Commit, err)
	}
	calls := 0
	for _, line := range lines[1:] {
		var record sem.RelationRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.RecordType != "relation" {
			continue
		}
		if record.Type != "CALLS" {
			t.Fatalf("filter leaked %s", record.Type)
		}
		if strings.HasSuffix(record.FromID, ":caller") && strings.HasSuffix(record.ToID, ":legacyOnly") {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("caller -> legacyOnly CALLS edges = %d in %s", calls, stream)
	}
}

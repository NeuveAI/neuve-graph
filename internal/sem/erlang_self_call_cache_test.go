package sem

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestErlangSelfCallRetainedConsumers(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "erlang-proof")
	writeFile(t, repo, "recursion.erl", erlangProofPrefix+"self_call(0) -> ok;\nself_call(N) ->\n    self_call(N-1).\n")
	writeFile(t, repo, "refusal.erl", "-module(refusal).\nself_call(N) ->\n    fun(X) -> self_call(X-1) end.\n")
	writeFile(t, repo, "decoy.erl", "-module(decoy).\nself_call(N) -> N.\n")
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "Erlang retained consumers")
	cacheDir := t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileFull}
	var cold ProviderSnapshot
	for attempt := 0; attempt < 2; attempt++ {
		snapshot, hit, err := PreindexProviderSnapshot(t.Context(), repo, "test", options, cacheDir)
		if err != nil {
			t.Fatal(err)
		}
		if hit != (attempt == 1) {
			t.Fatalf("complete attempt=%d hit=%t", attempt, hit)
		}
		assertErlangRetainedSelf(t, snapshot, erlangProofID, 1, 5)
		if len(snapshot.Files) != 3 || len(snapshot.Header.PartialFailures) != 0 {
			t.Fatalf("complete population/health=%#v", snapshot)
		}
		if attempt == 0 {
			cold = snapshot
		} else if !reflect.DeepEqual(cold.Relations, snapshot.Relations) {
			t.Fatal("exact persisted hit changed finished relations")
		}
	}
	for _, tc := range []struct {
		path, id    string
		count, line int
	}{{"recursion.erl", erlangProofID, 1, 5}, {"refusal.erl", "local/erlang-proof:Erlang:refusal.erl:function:self_call", 0, 0}} {
		t.Run("persisted selective/"+tc.path, func(t *testing.T) {
			selectiveOptions := options
			selectiveOptions.OnlyFiles = []string{tc.path}
			for attempt := 0; attempt < 2; attempt++ {
				snapshot, hit, err := loadOrBuildSearchSnapshot(t.Context(), repo, "test", selectiveOptions, cacheDir, false, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !hit {
					t.Fatal("persisted full-to-selective consumer missed complete/derived cache")
				}
				if len(snapshot.Files) != 1 || snapshot.Files[0].Path != tc.path || snapshotHasSymbolNamed(snapshot, "decoy") {
					t.Fatalf("selective population leaked decoy: %#v", snapshot)
				}
				assertErlangRetainedSelf(t, snapshot, tc.id, tc.count, tc.line)
			}
		})
	}
	// The opaque transaction retains finished public records, not parser proof.
	transaction, err := BeginProviderRecordsCache(t.Context(), repo, "test", cold.Header.Commit, cold.Header.Tree, "snapshot:ndjson", t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, hit := transaction.Load(); hit {
		t.Fatal("opaque cold unexpectedly hit")
	}
	var out bytes.Buffer
	if err := WriteSnapshotNDJSON(&out, cold); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("erlang_self_call")) {
		t.Fatal("private witness leaked into public NDJSON")
	}
	if err := transaction.Store(out.Bytes(), nil, cold.Header); err != nil {
		t.Fatal(err)
	}
	loaded, _, hit := transaction.Load()
	if !hit || !bytes.Equal(loaded, out.Bytes()) {
		t.Fatal("opaque Load did not preserve final projection")
	}
	decoder := json.NewDecoder(bytes.NewReader(loaded))
	var projected ProviderSnapshot
	for {
		var record json.RawMessage
		err := decoder.Decode(&record)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var kind struct {
			RecordType string `json:"record_type"`
		}
		if err := json.Unmarshal(record, &kind); err != nil {
			t.Fatal(err)
		}
		switch kind.RecordType {
		case "symbol":
			var s SymbolRecord
			if err := json.Unmarshal(record, &s); err != nil {
				t.Fatal(err)
			}
			projected.Symbols = append(projected.Symbols, s)
		case "relation":
			var r RelationRecord
			if err := json.Unmarshal(record, &r); err != nil {
				t.Fatal(err)
			}
			projected.Relations = append(projected.Relations, r)
		}
	}
	assertErlangRetainedSelf(t, projected, erlangProofID, 1, 5)
	// Old semantic envelopes remain invalid even if their other inputs agree.
	old := newCachedSearchSnapshot("test", cold.Header.Commit, cold.Header.Tree, options, cold)
	if !validCachedSearchSnapshot(old, cold.Header.RepoKey, "test", cold.Header.Tree, options) {
		t.Fatal("current structured semantic envelope refused")
	}
	for _, version := range []string{"search-snapshot-v18-", "search-snapshot-v19-"} {
		old.CacheVersion = version + IdentityRevision
		if validCachedSearchSnapshot(old, cold.Header.RepoKey, "test", cold.Header.Tree, options) {
			t.Fatalf("old structured semantic namespace admitted: %s", version)
		}
	}
	oldRecords, err := readProviderRecords(transaction.entry)
	if err != nil {
		t.Fatal(err)
	}
	if !validCachedProviderRecords(oldRecords, "test", cold.Header.Commit, cold.Header.Tree, "snapshot:ndjson", options) {
		t.Fatal("current opaque semantic envelope refused")
	}
	oldRecords.CacheVersion = "provider-records-v12-" + IdentityRevision
	if validCachedProviderRecords(oldRecords, "test", cold.Header.Commit, cold.Header.Tree, "snapshot:ndjson", options) {
		t.Fatal("old opaque semantic namespace admitted")
	}
}

func assertErlangRetainedSelf(t *testing.T, snapshot ProviderSnapshot, id string, want, line int) {
	t.Helper()
	found := false
	for _, s := range snapshot.Symbols {
		if s.ID == id {
			found = true
			if s.Language != "Erlang" || s.Kind != "function" {
				t.Fatalf("retained owner=%#v", s)
			}
		}
	}
	if !found {
		t.Fatalf("source ID missing: %s", id)
	}
	count := 0
	for _, r := range snapshot.Relations {
		if r.Type == "CALLS" && r.FromID == id && r.ToID == id {
			count++
			if r.Resolution != "exact" || r.Confidence != .92 || r.RelationScope != "file" || r.TargetKind != "symbol" || len(r.Evidence) != 1 || r.Evidence[0].Kind != "call_site" || r.Evidence[0].FilePath != "recursion.erl" || r.Evidence[0].StartLine != line || r.Evidence[0].EndLine != line || r.Evidence[0].Detail != "self_call" || len(r.WarningCodes) != 0 {
				t.Fatalf("retained edge=%#v", r)
			}
		}
	}
	if count != want {
		t.Fatalf("retained self count=%d want %d", count, want)
	}
}

func TestErlangSelfCallCacheWitnessValidation(t *testing.T) {
	entities, language, _ := (TreeSitterParser{}).ParseWithStatus("recursion.erl", erlangProofPrefix+"self_call(N) ->\n    self_call(N-1).\n")
	symbols := entitySymbols("local/erlang-proof", "recursion.erl", language, entities)
	snapshot := ProviderSnapshot{Symbols: symbols}
	cache := newCachedSearchSnapshot("test", "commit", "tree", ProviderSnapshotOptions{Profile: ProfileFull}, snapshot)
	if len(cache.ErlangSelfCalls) != 1 {
		t.Fatalf("positive-only sidecar=%#v", cache.ErlangSelfCalls)
	}
	encoded, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	var persisted cachedSearchSnapshot
	if err := json.Unmarshal(encoded, &persisted); err != nil {
		t.Fatal(err)
	}
	persisted.ErlangSelfCalls[erlangProofID] = erlangSelfCallWitness{1, 100}
	restoreCachedSearchInternals(&persisted)
	for _, s := range persisted.Snapshot.Symbols {
		if s.erlangSelfCall.StartLine != 0 {
			t.Fatalf("out-of-owner cached witness admitted: %#v", s)
		}
	}
	public, err := json.Marshal(symbols)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "erlang_self") {
		t.Fatal("private symbol metadata became public")
	}
}

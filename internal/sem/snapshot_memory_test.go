package sem

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

// memoryFingerprints hashes each remembered snapshot, every field included,
// as it now stands.
func memoryFingerprints(t *testing.T, memory *snapshotMemory) map[string][32]byte {
	t.Helper()
	memory.mu.Lock()
	defer memory.mu.Unlock()
	out := map[string][32]byte{}
	for key, entry := range memory.entries {
		// %#v prints unexported fields too, so an edit to a hidden field the
		// cache restores (or to a shared nested value) changes the hash.
		out[key] = sha256.Sum256([]byte(fmt.Sprintf("%#v", entry.cached)))
	}
	return out
}

func withoutLatencies(response SearchResponse) SearchResponse {
	response.Stats.IndexLatencyMS = 0
	response.Stats.QueryLatencyMS = 0
	response.Stats.TotalLatencyMS = 0
	response.Stats.SearchLatencyMS = 0
	response.Stats.PreselectLatencyMS = 0
	return response
}

// A long-lived process that remembers decoded snapshots (the Graph service)
// answers every search exactly as a fresh process does, and no search
// changes the snapshot it shares with the next: the remembered snapshot is
// byte for byte what was decoded, after every search of a varied battery.
func TestSnapshotMemoryAnswersLikeAFreshProcess(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth/session.go", "package auth\n\n// NewAuthSession opens a session for a token.\nfunc NewAuthSession(token string) *Session { return &Session{Token: token} }\n\ntype Session struct{ Token string }\n\nfunc (s *Session) Close() error { return nil }\n")
	write(t, repo, "auth/session_test.go", "package auth\n\nimport \"testing\"\n\nfunc TestNewAuthSession(t *testing.T) { _ = NewAuthSession(\"x\").Close() }\n")
	write(t, repo, "web/handler.ts", "export function handleLogin(req: Request) { return validateToken(req.headers.get('auth')) }\nexport function validateToken(token: string | null) { return token !== null }\n")
	write(t, repo, "web/routes.py", "def register(app):\n    app.route('/login')(login)\n\ndef login():\n    return 'ok'\n")
	write(t, repo, "README.md", "# Auth\n\nSessions and tokens for the login flow.\n")
	for index := 0; index < 8; index++ {
		write(t, repo, fmt.Sprintf("noise/file_%02d.go", index), fmt.Sprintf("package noise\n\nfunc Noise%d() int { return %d }\n", index, index))
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	cacheDir := t.TempDir()
	for _, profile := range []Profile{ProfileFull, ProfileFast} {
		if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: profile}, cacheDir); err != nil {
			t.Fatal(err)
		}
	}
	type search struct {
		query   string
		options SearchOptions
	}
	battery := []search{
		{"authentication session", SearchOptions{Profile: ProfileFull, TopK: 10, CacheDir: cacheDir, PreparedOnly: true}},
		{"validate the login token", SearchOptions{Profile: ProfileFull, TopK: 5, CacheDir: cacheDir, PreparedOnly: true}},
		{"close the session", SearchOptions{Profile: ProfileFast, TopK: 10, CacheDir: cacheDir, PreparedOnly: true}},
		{"register routes for login", SearchOptions{Profile: ProfileFull, TopK: 3, MaxIndexedFiles: 2, CacheDir: cacheDir, PreparedOnly: true}},
		{"NewAuthSession", SearchOptions{Profile: ProfileFull, TopK: 10, CacheDir: cacheDir, PreparedOnly: true, Deep: true}},
		{"noise", SearchOptions{Profile: ProfileFast, TopK: 20, CacheDir: cacheDir, PreparedOnly: true, MaxContextBytes: 512}},
	}
	run := func(s search) SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", s.query, s.options)
		if err != nil {
			t.Fatal(err)
		}
		return withoutLatencies(response)
	}
	// The baseline is a second pass, so the cache directory's remembered scan
	// answers are as warm for it as for the passes with memory.
	for _, s := range battery {
		run(s)
	}
	var fresh []SearchResponse
	for _, s := range battery {
		fresh = append(fresh, run(s))
	}
	EnableSnapshotMemory(2)
	t.Cleanup(func() { EnableSnapshotMemory(0) })
	memory := currentSnapshotMemory()
	seen := map[string][32]byte{}
	// Twice through, so every search runs on a snapshot others have used.
	for round := 0; round < 2; round++ {
		for index, s := range battery {
			got := run(s)
			if !reflect.DeepEqual(got, fresh[index]) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(fresh[index])
				at := 0
				for at < len(gotJSON) && at < len(wantJSON) && gotJSON[at] == wantJSON[at] {
					at++
				}
				from := max(0, at-120)
				t.Fatalf("round %d, search %q differs with snapshot memory at byte %d:\n got …%.300s\nwant …%.300s", round, s.query, at, gotJSON[from:], wantJSON[from:])
			}
			for key, hash := range memoryFingerprints(t, memory) {
				if first, ok := seen[key]; ok && first != hash {
					t.Fatalf("round %d, search %q changed a remembered snapshot", round, s.query)
				}
				seen[key] = hash
			}
		}
	}
	if len(memory.entries) == 0 || memory.hits < len(battery) {
		t.Fatalf("the battery barely used the memory: %d snapshots, %d hits", len(memory.entries), memory.hits)
	}
}

// A search that edits a record of its snapshot edits its own copy: the next
// search reads the snapshot as it was decoded.
func TestSnapshotMemoryHandsOutSeparateRecords(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, _ := file.Stat()
	decoded := cachedSearchSnapshot{Snapshot: ProviderSnapshot{
		Files:     []FileRecord{{ID: "f1", Path: "a.go"}},
		Symbols:   []SymbolRecord{{ID: "s1", Name: "Original"}},
		Relations: []RelationRecord{{FromID: "r1"}},
		Externals: []ExternalRecord{{ID: "e1"}},
	}}
	memory := &snapshotMemory{max: 2, entries: map[string]*snapshotMemoryEntry{}}
	decode := func() (cachedSearchSnapshot, error) { return decoded, nil }
	first, _, _ := memory.read(file.Name(), info, decode)
	first.Snapshot.Symbols[0].Name = "Changed"
	first.Snapshot.Files[0].Path = "changed.go"
	first.Snapshot.Relations[0].FromID = "changed"
	first.Snapshot.Externals[0].ID = "changed"
	second, hit, _ := memory.read(file.Name(), info, decode)
	if !hit || second.Snapshot.Symbols[0].Name != "Original" || second.Snapshot.Files[0].Path != "a.go" || second.Snapshot.Relations[0].FromID != "r1" || second.Snapshot.Externals[0].ID != "e1" {
		t.Fatalf("an edit reached the remembered snapshot: %+v (hit %v)", second.Snapshot, hit)
	}
}

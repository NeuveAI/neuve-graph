package cli

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func revParse(t *testing.T, repo, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", rev).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// An index reports its cache directory's size; a declared base survives a
// prune to a cap nothing fits in, an undeclared one does not, and a prepared
// read of the kept base still answers.
func TestCacheDeclarePruneAndUsage(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth.go", "package auth\n\nfunc ValidateToken(token string) bool { return token != \"\" }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "first")
	first := revParse(t, repo, "HEAD")
	write(t, repo, "session.go", "package auth\n\nfunc OpenSession() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "second")
	second := revParse(t, repo, "HEAD")
	cache := t.TempDir()
	run := func(args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := Run(t.Context(), Options{Version: "test", Stdout: &out, Env: EntireEnv{}}, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.Bytes()
	}
	type receipt struct {
		Cache struct {
			Bytes          int64            `json:"bytes"`
			CapBytes       int64            `json:"cap_bytes"`
			DeclaredBytes  int64            `json:"declared_bytes"`
			EvictedEntries int              `json:"evicted_entries"`
			Families       map[string]int64 `json:"families"`
		} `json:"cache"`
		Declared *struct {
			Commits []string `json:"commits"`
			Trees   []string `json:"trees"`
		} `json:"declared"`
	}
	decode := func(data []byte) receipt {
		t.Helper()
		var value receipt
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		return value
	}
	// The second commit is indexed first, then the first, so the second is
	// the least recently used; declaring it keeps it all the same.
	indexed := decode(run("index", "--repo", repo, "--head", "--profile", "full", "--cache-dir", cache, "--format", "json"))
	if indexed.Cache.Bytes == 0 || indexed.Cache.CapBytes != 4<<30 || indexed.Cache.Families["search"] == 0 {
		t.Fatalf("index reported cache %+v", indexed.Cache)
	}
	git(t, repo, "checkout", "-q", first)
	run("index", "--repo", repo, "--head", "--profile", "full", "--cache-dir", cache, "--format", "json")
	declared := decode(run("cache", "declare", "--cache-dir", cache, "--repo", repo, "--commit", second))
	if declared.Declared == nil || len(declared.Declared.Trees) != 1 || declared.Declared.Trees[0] != revParse(t, repo, second+"^{tree}") || declared.Cache.DeclaredBytes == 0 {
		t.Fatalf("declare reported %+v", declared)
	}
	pruned := decode(run("cache", "prune", "--cache-dir", cache, "--max-bytes", "1"))
	if pruned.Cache.EvictedEntries == 0 || pruned.Cache.DeclaredBytes == 0 || pruned.Cache.Families["search"] != pruned.Cache.DeclaredBytes {
		t.Fatalf("prune to one byte left %+v; want only the declared base's snapshot", pruned.Cache)
	}
	git(t, repo, "checkout", "-q", second)
	out := run("query", "--repo", repo, "--head", "--prepared-only", "--profile", "full", "--cache-dir", cache, "--format", "json", "--query", "open session")
	if !bytes.Contains(out, []byte("OpenSession")) {
		t.Fatalf("the declared base no longer answers: %.300s", out)
	}
	text := run("cache", "usage", "--cache-dir", cache, "--format", "text")
	if !bytes.Contains(text, []byte("declared")) || !bytes.Contains(text, []byte(`"search"`)) {
		t.Fatalf("usage text: %s", text)
	}
	for _, bad := range [][]string{
		{"cache"},
		{"cache", "declare", "--cache-dir", cache, "--commit", "HEAD"},
		{"cache", "prune", "--cache-dir", cache, "--commit", second},
		{"cache", "usage", "--cache-dir", cache, "--max-bytes", "0"},
	} {
		if err := Run(t.Context(), Options{Version: "test", Stdout: &bytes.Buffer{}, Env: EntireEnv{}}, bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

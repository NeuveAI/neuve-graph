//go:build cgo && !windows

package textindex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/gitutil"
)

func gitRun(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// indexFixture holds files the index can rule out, files it must still name
// (case variants, non-ASCII neighbours) and files it cannot hold at all (NUL
// after git's binary probe, invalid UTF-8, over the 1 MiB document limit).
func indexFixture(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init")
	gitRun(t, repo, "config", "user.name", "Entire Graph Test")
	gitRun(t, repo, "config", "user.email", "graph@example.com")
	files := map[string]string{
		"auth/session.go":  "package auth\n// AUTH Session\nfunc NewAuthSession() {}\n",
		"auth/token.go":    "package auth\nfunc Token() {}\n",
		"accents.txt":      "éSESSION\nsessioné\n",
		"late-nul.txt":     strings.Repeat("a", 9000) + "\x00 session\n",
		"invalid.txt":      "\xff session\n",
		"huge.txt":         strings.Repeat("filler line\n", 100000) + "authentication at the end\n",
		"unrelated/a.go":   "package unrelated\nfunc A() {}\n",
		"unrelated/b.md":   "nothing to see\n",
		"mixed/Case.java":  "class SeSsIoN {}\n",
		"mixed/kelvin.txt": "Kelvin and session\n",
	}
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "fixture")
	return repo, gitRun(t, repo, "rev-parse", "HEAD")
}

func reader(repo string) BlobReader {
	return func(ctx context.Context, oids []string, visit func(string, []byte) error) error {
		return gitutil.ReadBlobs(ctx, repo, oids, visit)
	}
}

func candidates(store *Store) gitutil.TreeScanCandidates { return store.ScanCandidates }

func grepReference(t *testing.T, repo, commit string, patterns []string) []gitutil.GrepMatch {
	t.Helper()
	var out []gitutil.GrepMatch
	if err := gitutil.GrepTreePatternLinesGit(t.Context(), repo, commit, patterns, func(m gitutil.GrepMatch) error {
		out = append(out, m)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A scan through the index answers exactly what git grep answers, while the
// index rules out the files that hold none of the patterns' literals; files
// the index cannot hold are scanned.
func TestScanThroughTheIndexIsGitGrep(t *testing.T) {
	repo, commit := indexFixture(t)
	oids, err := gitutil.ListTreeBlobs(t.Context(), repo, commit)
	if err != nil {
		t.Fatal(err)
	}
	store := Open(t.TempDir(), 0)
	receipt, err := store.Build(t.Context(), oids, reader(repo))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.TreeBlobs != len(oids) || receipt.BlobsSkipped != 3 || receipt.TreeUnindexedBlobs != 3 || receipt.TreeIndexBytes == 0 {
		t.Fatalf("receipt = %+v, want every blob considered and the three it cannot hold left unindexed", receipt)
	}
	for _, locale := range []string{"C", "en_US.UTF-8"} {
		t.Setenv("LC_ALL", locale)
		for name, patterns := range map[string][]string{
			"selective": {"[sS][eE][sS][sS][iI][oO][nN]", "[aA][uU][tT][hH][eE][nN][tT][iI][cC][aA][tT][iI][oO][nN]"},
			"kelvin":    {"(k|K|K)elvin"},
			"alias":     {"(^|[^[:alnum:]])([aA][uU][tT][hH])($|[^[:alnum:]])", "[[:lower:][:digit:]]([sS])ession"},
		} {
			want := grepReference(t, repo, commit, patterns)
			var got []gitutil.GrepMatch
			stats, err := gitutil.ScanTreePatternLinesWith(t.Context(), repo, commit, patterns, gitutil.TreeScanOptions{Candidates: candidates(store)}, func(m gitutil.GrepMatch) error {
				got = append(got, m)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s/%s: scan through the index differs from git grep:\n got %q\nwant %q", locale, name, got, want)
			}
			if name == "selective" && (stats.Route != gitutil.TreeScanRouteTextIndex || stats.TextIndexExcluded == 0 || stats.BlobsRead >= len(oids)) {
				t.Fatalf("%s/%s: the index ruled nothing out: %+v", locale, name, stats)
			}
		}
	}
}

// A pattern with no literal of three ASCII bytes can match lines the index
// knows nothing about, so the scan reads every blob and says why.
func TestScanWithoutUsableLiteralsReadsEverything(t *testing.T) {
	repo, commit := indexFixture(t)
	oids, _ := gitutil.ListTreeBlobs(t.Context(), repo, commit)
	store := Open(t.TempDir(), 0)
	if _, err := store.Build(t.Context(), oids, reader(repo)); err != nil {
		t.Fatal(err)
	}
	patterns := []string{"[sS][eE][sS][sS][iI][oO][nN]", "[gG][oO]"}
	var got []gitutil.GrepMatch
	stats, err := gitutil.ScanTreePatternLinesWith(t.Context(), repo, commit, patterns, gitutil.TreeScanOptions{Candidates: candidates(store)}, func(m gitutil.GrepMatch) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, grepReference(t, repo, commit, patterns)) || stats.FallbackReason != "no-trigram" || stats.TextIndexLookups != 0 {
		t.Fatalf("scan = %q %+v, want git's answer, no lookup, reason no-trigram", got, stats)
	}
}

// Each commit adds a shard only for the blobs it changed, and past its bound
// the index evicts the least recently used shard no longer needed, never
// those of the tree just built.
func TestIndexEvictsOldestShardsAndKeepsTheNewTree(t *testing.T) {
	repo, _ := indexFixture(t)
	dir := t.TempDir()
	build := func(maxBytes int64) BuildReceipt {
		t.Helper()
		oids, err := gitutil.ListTreeBlobs(t.Context(), repo, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := Open(dir, maxBytes).Build(t.Context(), oids, reader(repo))
		if err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	commitToken := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "auth", "token.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, repo, "commit", "-am", "token")
	}
	base := build(0)
	commitToken("package auth\nfunc Token2() {}\n")
	second := build(0)
	if second.BlobsIndexed != 1 || second.Shards != 2 {
		t.Fatalf("second build = %+v, want one blob in one new shard", second)
	}
	obsolete := second.IndexBytesOnDisk - base.IndexBytesOnDisk
	commitToken("package auth\nfunc Token3() {}\n")
	// Room for the base and the new shard, not for the second commit's.
	third := build(second.IndexBytesOnDisk + 1)
	if third.Shards != 2 || third.EvictedBytes != obsolete {
		t.Fatalf("third build = %+v, want the second commit's shard (%d bytes) evicted and two kept", third, obsolete)
	}
	oids, _ := gitutil.ListTreeBlobs(t.Context(), repo, "HEAD")
	_, lookup, err := Open(dir, 0).Candidates(t.Context(), oids, []string{"zzqqzz"})
	if err != nil {
		t.Fatal(err)
	}
	if lookup.Uncovered != third.TreeUnindexedBlobs || third.TreeUnindexedBlobs != 3 {
		t.Fatalf("the new tree lost coverage: %+v after %+v", lookup, third)
	}
}

func scanThrough(t *testing.T, repo, commit string, patterns []string, store *Store) ([]gitutil.GrepMatch, gitutil.TreeScanStats) {
	t.Helper()
	var got []gitutil.GrepMatch
	stats, err := gitutil.ScanTreePatternLinesWith(t.Context(), repo, commit, patterns, gitutil.TreeScanOptions{Candidates: store.ScanCandidates}, func(m gitutil.GrepMatch) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, stats
}

// A damaged shard or manifest never rules a matching file out and never
// crashes the search: it fails its checksum, its blobs are scanned, and the
// scan answers what git grep answers.
func TestDamagedIndexIsNeverAnAnswer(t *testing.T) {
	repo, commit := indexFixture(t)
	oids, _ := gitutil.ListTreeBlobs(t.Context(), repo, commit)
	patterns := []string{"[sS][eE][sS][sS][iI][oO][nN]", "[aA][uU][tT][hH][eE][nN][tT][iI][cC][aA][tT][iI][oO][nN]"}
	want := grepReference(t, repo, commit, patterns)
	dir := t.TempDir()
	if _, err := Open(dir, 0).Build(t.Context(), oids, reader(repo)); err != nil {
		t.Fatal(err)
	}
	shards, _ := filepath.Glob(filepath.Join(dir, "text-index", layoutVersion, "*.zoekt"))
	manifests, _ := filepath.Glob(filepath.Join(dir, "text-index", layoutVersion, "*.blobs"))
	if len(shards) != 1 || len(manifests) != 1 {
		t.Fatalf("want one shard and one manifest, got %d and %d", len(shards), len(manifests))
	}
	pristine, err := os.ReadFile(shards[0])
	if err != nil {
		t.Fatal(err)
	}
	for step := 1; step < 40; step++ {
		damaged := append([]byte(nil), pristine...)
		for k := 0; k < 48; k++ {
			damaged[(len(damaged)*step/40+k)%len(damaged)] ^= 0x5a
		}
		if err := os.WriteFile(shards[0], damaged, 0o644); err != nil {
			t.Fatal(err)
		}
		got, stats := scanThrough(t, repo, commit, patterns, Open(dir, 0))
		if !reflect.DeepEqual(got, want) || stats.TextIndexExcluded != 0 || stats.FallbackReason != "index-error" {
			t.Fatalf("damage at step %d: %q %+v", step, got, stats)
		}
	}
	if err := os.WriteFile(shards[0], pristine, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, _ := os.ReadFile(manifests[0])
	at := strings.Index(string(manifest), "i ") + 2
	manifest[at] ^= 0x01 // another blob id, still hex
	if err := os.WriteFile(manifests[0], manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, stats := scanThrough(t, repo, commit, patterns, Open(dir, 0)); !reflect.DeepEqual(got, want) || stats.TextIndexExcluded != 0 {
		t.Fatalf("a damaged manifest was trusted: %q %+v", got, stats)
	}
}

// Zoekt answers a cancelled search with what it found so far; the lookup
// reports the cancellation instead of a short list.
func TestCancelledLookupIsAnError(t *testing.T) {
	repo, commit := indexFixture(t)
	oids, _ := gitutil.ListTreeBlobs(t.Context(), repo, commit)
	store := Open(t.TempDir(), 0)
	if _, err := store.Build(t.Context(), oids, reader(repo)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if hits, _, err := store.Candidates(ctx, oids, []string{"session"}); err == nil {
		t.Fatalf("a cancelled lookup answered %d candidates without an error", len(hits))
	}
}

// With Zoekt's tenant enforcement set, its searches skip documents, so the
// index declines and the scan reads every blob.
func TestTenantEnforcementDisablesTheIndex(t *testing.T) {
	repo, commit := indexFixture(t)
	oids, _ := gitutil.ListTreeBlobs(t.Context(), repo, commit)
	store := Open(t.TempDir(), 0)
	if _, err := store.Build(t.Context(), oids, reader(repo)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(tenantEnforcementEnv, "strict")
	patterns := []string{"[sS][eE][sS][sS][iI][oO][nN]"}
	got, stats := scanThrough(t, repo, commit, patterns, store)
	if !reflect.DeepEqual(got, grepReference(t, repo, commit, patterns)) || stats.FallbackReason != "index-error" || stats.TextIndexExcluded != 0 {
		t.Fatalf("scan with tenant enforcement = %q %+v", got, stats)
	}
}

// What a killed build leaves (a temporary file, a shard whose manifest was
// never written, a manifest that fails its checksum) is removed by the next
// build, so it never sits outside the bound.
func TestBuildRemovesWhatAKilledBuildLeft(t *testing.T) {
	repo, commit := indexFixture(t)
	oids, _ := gitutil.ListTreeBlobs(t.Context(), repo, commit)
	cache := t.TempDir()
	dir := filepath.Join(cache, "text-index", layoutVersion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	leftovers := map[string]string{
		".tmp-123":                               strings.Repeat("x", 4096),
		"0123456789abcdef0123456789abcdef.zoekt": strings.Repeat("y", 2048),
		"fedcba9876543210fedcba9876543210.zoekt": "z",
		"fedcba9876543210fedcba9876543210.blobs": "i 0000000000000000000000000000000000000000\nmanifest-sha256 0\n",
	}
	for name, content := range leftovers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := Open(cache, 0).Build(t.Context(), oids, reader(repo))
	if err != nil {
		t.Fatal(err)
	}
	for name := range leftovers {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s survived the next build (%+v)", name, receipt)
		}
	}
	if receipt.EvictedBytes < 4096+2048 {
		t.Fatalf("receipt = %+v, want the leftovers counted as evicted", receipt)
	}
}

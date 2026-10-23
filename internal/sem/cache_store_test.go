package sem

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCacheArtifact writes a gzipped JSON artifact below the cache directory,
// padded to about size bytes, last modified at modTime.
func writeCacheArtifact(t *testing.T, cacheDir, relative string, header map[string]any, size int, modTime time.Time) {
	t.Helper()
	header["padding"] = strings.Repeat("x", size)
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&buf, gzip.NoCompression)
	writer.Write(data)
	writer.Close()
	path := filepath.Join(cacheDir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func cacheFileExists(cacheDir, relative string) bool {
	_, err := os.Lstat(filepath.Join(cacheDir, relative))
	return err == nil
}

func walkedCacheBytes(t *testing.T, cacheDir string) int64 {
	t.Helper()
	var total int64
	filepath.WalkDir(cacheDir, func(_ string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			info, _ := entry.Info()
			total += info.Size()
		}
		return nil
	})
	return total
}

func oid(c byte) string { return strings.Repeat(string(c), 40) }
func key(c byte) string { return strings.Repeat(string(c), 64) }

// A prune removes other engine versions' artifacts first, then the least
// recently used, with the artifacts derived from each; it keeps generation
// markers,
// declared bases (by commit or by tree), an artifact a read holds open, and
// a writer's fresh temporary, and stops once within the cap. Its usage report
// matches the directory as it is left.
func TestPruneCacheRemovesInOrderAndKeepsWhatMustStay(t *testing.T) {
	cacheDir := t.TempDir()
	search := filepath.Join("search", searchSnapshotCacheVersion)
	old := time.Now().Add(-48 * time.Hour)
	base := time.Now().Add(-10 * time.Hour)
	at := func(hours int) time.Time { return base.Add(time.Duration(hours) * time.Hour) }
	const size = 10_000
	// Another engine version's snapshot, used most recently of all: removed first.
	writeCacheArtifact(t, cacheDir, filepath.Join("search", "search-snapshot-v1-0", key('0')+".json.gz"), map[string]any{"commit": oid('0'), "tree": oid('0')}, size, at(9))
	// Current snapshots: a (oldest, with a marker and a derived artifact), b
	// (declared by commit), c (declared by tree), d (held by a read), e (newest).
	writeCacheArtifact(t, cacheDir, filepath.Join(search, key('a')+".json.gz"), map[string]any{"commit": oid('a'), "tree": oid('1')}, size, at(1))
	writeCacheArtifact(t, cacheDir, filepath.Join(search, key('a')+".generation.json.gz"), map[string]any{"generation": "g"}, 10, at(1))
	writeCacheArtifact(t, cacheDir, filepath.Join(search, key('a'), key('f')+".json.gz"), map[string]any{"commit": oid('a'), "tree": oid('1')}, size, at(1))
	writeCacheArtifact(t, cacheDir, filepath.Join(search, key('b')+".json.gz"), map[string]any{"commit": oid('b'), "tree": oid('2')}, size, at(0))
	writeCacheArtifact(t, cacheDir, filepath.Join(search, key('c')+".json.gz"), map[string]any{"commit": oid('c'), "tree": oid('3')}, size, at(0))
	writeCacheArtifact(t, cacheDir, filepath.Join(search, key('d')+".json.gz"), map[string]any{"commit": oid('d'), "tree": oid('4')}, size, at(2))
	writeCacheArtifact(t, cacheDir, filepath.Join(search, key('e')+".json.gz"), map[string]any{"commit": oid('e'), "tree": oid('5')}, size, at(8))
	// A syntax generation keeps its base inside its snapshot; a source object
	// that names a commit of its own is not the base.
	writeCacheArtifact(t, cacheDir, filepath.Join("generations", preparedGenerationVersion, key('g')+".json.gz"), map[string]any{"id": "x", "snapshot": map[string]any{"source": map[string]any{"commit": oid('9')}, "commit": oid('b'), "tree": oid('2')}}, size, at(0))
	// An extraction, used recently.
	writeCacheArtifact(t, cacheDir, filepath.Join("extractions", extractionCacheVersion, key('x')+".json.gz"), map[string]any{}, size, at(7))
	// Temporaries: a crashed writer's, and one being written.
	writeCacheArtifact(t, cacheDir, filepath.Join(search, ".snapshot-stale.json.gz"), map[string]any{}, size, old)
	writeCacheArtifact(t, cacheDir, filepath.Join(search, ".snapshot-fresh.json.gz"), map[string]any{}, size, time.Now())
	// The text index is counted, not removed by this store.
	writeCacheArtifact(t, cacheDir, filepath.Join("text-index", "v1", "shard"), map[string]any{}, size, old)

	if err := DeclareCacheBases(cacheDir, []string{oid('b')}, []string{oid('3')}); err != nil {
		t.Fatal(err)
	}
	// e was read an hour ago although its file is older: the recorded use
	// counts, not the modification time.
	appendCacheUses(cacheDir, map[string]int64{filepath.ToSlash(filepath.Join(search, key('e')+".json.gz")): time.Now().Add(-time.Hour).UnixNano()})
	// d is being read.
	entry, err := newCacheEntry(cacheDir, "search", searchSnapshotCacheVersion, key('d'))
	if err != nil {
		t.Fatal(err)
	}
	held, err := entry.open()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	before, err := MeasureCache(cacheDir, CacheLimits{MaxBytes: 1 << 40})
	if err != nil {
		t.Fatal(err)
	}
	if before.DeclaredBytes < 3*size {
		t.Fatalf("declared bytes %d, want b, c and the generation of b", before.DeclaredBytes)
	}
	// A cap that needs one more removal after the other version, the stale
	// temporary and a: d is next but held, so the extraction (used before e)
	// goes instead.
	limit := before.Bytes - 5*size + size/2
	usage, err := PruneCache(cacheDir, CacheLimits{MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	removed := []string{
		filepath.Join("search", "search-snapshot-v1-0", key('0')+".json.gz"),
		filepath.Join(search, ".snapshot-stale.json.gz"),
		filepath.Join(search, key('a')+".json.gz"),
		filepath.Join(search, key('a'), key('f')+".json.gz"),
		filepath.Join("extractions", extractionCacheVersion, key('x')+".json.gz"),
	}
	for _, name := range removed {
		if cacheFileExists(cacheDir, name) {
			t.Errorf("%s was kept", name)
		}
	}
	kept := []string{
		filepath.Join(search, key('b')+".json.gz"),
		filepath.Join(search, key('c')+".json.gz"),
		filepath.Join(search, key('d')+".json.gz"),
		filepath.Join(search, key('e')+".json.gz"),
		filepath.Join("generations", preparedGenerationVersion, key('g')+".json.gz"),
		filepath.Join(search, ".snapshot-fresh.json.gz"),
		// A generation marker stays, or a view a rebuild retired could read
		// as current again.
		filepath.Join(search, key('a')+".generation.json.gz"),
		filepath.Join("text-index", "v1", "shard"),
	}
	for _, name := range kept {
		if !cacheFileExists(cacheDir, name) {
			t.Errorf("%s was removed", name)
		}
	}
	if cacheFileExists(cacheDir, filepath.Join(search, key('a'))) {
		t.Error("the derived directory of a was left behind")
	}
	if usage.Bytes > limit || usage.EvictedEntries != 4 || usage.PinnedBytes == 0 {
		t.Fatalf("usage %+v, want within %d after removing four artifacts, with d held", usage, limit)
	}
	if got := walkedCacheBytes(t, cacheDir); got != usage.Bytes {
		// The store rewrote its own small files after measuring; allow for them.
		if diff := got - usage.Bytes; diff < -4096 || diff > 4096 {
			t.Fatalf("usage reports %d bytes, the directory holds %d", usage.Bytes, got)
		}
	}
	// With the read done, a tighter cap removes d before e (e's use is newer).
	held.Close()
	if _, err := PruneCache(cacheDir, CacheLimits{MaxBytes: usage.Bytes - size/2}); err != nil {
		t.Fatal(err)
	}
	if cacheFileExists(cacheDir, filepath.Join(search, key('d')+".json.gz")) || !cacheFileExists(cacheDir, filepath.Join(search, key('e')+".json.gz")) {
		t.Fatal("the next prune did not remove the least recently used artifact, d, first")
	}
	last, err := LastCacheUsage(cacheDir, CacheLimits{MaxBytes: 7})
	if err != nil || last.Bytes == 0 || last.CapBytes != 7 {
		t.Fatalf("last usage %+v, %v", last, err)
	}
}

// The store reads the commit and tree of a real prepared snapshot, and a
// read through the cache records its use.
func TestCacheStoreReadsRealSnapshots(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth.go", "package auth\n\nfunc ValidateToken(token string) bool { return token != \"\" }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	commit := gitInput(t, repo, "", "rev-parse", "HEAD")
	tree := gitInput(t, repo, "", "rev-parse", "HEAD^{tree}")
	cacheDir := t.TempDir()
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileFull}, cacheDir); err != nil {
		t.Fatal(err)
	}
	if err := DeclareCacheBases(cacheDir, nil, []string{tree}); err != nil {
		t.Fatal(err)
	}
	usage, err := MeasureCache(cacheDir, CacheLimits{MaxBytes: 1 << 40})
	if err != nil {
		t.Fatal(err)
	}
	if usage.DeclaredBytes == 0 || usage.DeclaredBytes != usage.Families["search"] {
		t.Fatalf("declaring the tree kept %d of the search snapshots' %d bytes", usage.DeclaredBytes, usage.Families["search"])
	}
	if err := DeclareCacheBases(cacheDir, []string{commit}, nil); err != nil {
		t.Fatal(err)
	}
	if usage, _ = MeasureCache(cacheDir, CacheLimits{MaxBytes: 1 << 40}); usage.DeclaredBytes != usage.Families["search"] {
		t.Fatalf("declaring the commit kept %d of %d bytes", usage.DeclaredBytes, usage.Families["search"])
	}
	FlushCacheUse()
	os.Remove(filepath.Join(cacheDir, cacheStoreDir, cacheStoreUses))
	if _, err := SearchRepository(t.Context(), repo, "test", "validate token", SearchOptions{Profile: ProfileFull, CacheDir: cacheDir, PreparedOnly: true}); err != nil {
		t.Fatal(err)
	}
	FlushCacheUse()
	root, err := os.OpenRoot(filepath.Join(cacheDir, cacheStoreDir))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var used bool
	for name := range readCacheUses(root) {
		used = used || strings.HasPrefix(name, "search/"+searchSnapshotCacheVersion+"/")
	}
	if !used {
		t.Fatal("a prepared search left no recorded use of its snapshot")
	}
}

// A read's bookkeeping never creates a cache directory.
func TestCacheBookkeepingNeverCreatesTheDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	noteCacheUse(missing, "search/x.json.gz")
	FlushCacheUse()
	if _, err := LastCacheUsage(missing, CacheLimitsFromEnv()); err == nil {
		t.Fatal("usage of a missing cache directory was reported")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("bookkeeping created the cache directory")
	}
}

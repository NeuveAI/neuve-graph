//go:build !windows

package sem

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The store's own directory is never a link: a cache directory inside a
// checkout must not let a committed link send the store's writes elsewhere.
func TestCacheStoreRefusesALinkedStoreDirectory(t *testing.T) {
	cacheDir := t.TempDir()
	target := filepath.Join(cacheDir, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(cacheDir, cacheStoreDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := MeasureCache(cacheDir, CacheLimitsFromEnv()); err == nil {
		t.Fatal("a linked store directory was used")
	}
	if err := DeclareCacheBases(cacheDir, []string{oid('a')}, nil); err == nil {
		t.Fatal("a declaration went through a linked store directory")
	}
	noteCacheUse(cacheDir, "search/x.json.gz")
	FlushCacheUse()
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("the store wrote through the link: %v", entries)
	}
}

// A read that opens an artifact a writer replaces before the read holds it
// reads the new artifact.
func TestCacheReadsFollowReplacement(t *testing.T) {
	cacheDir := t.TempDir()
	entry, err := newCacheEntry(cacheDir, "search", searchSnapshotCacheVersion, key('c'))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cacheDir, entry.relative)
	writeCacheArtifact(t, cacheDir, entry.relative, map[string]any{"commit": "old"}, 10, time.Now())
	root, err := os.OpenRoot(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	stale, err := openRootBoundedRegularFile(root, entry.relative)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	writeCacheArtifact(t, cacheDir, filepath.Join("search", searchSnapshotCacheVersion, ".fresh.json.gz"), map[string]any{"commit": "new"}, 10, time.Now())
	if err := os.Rename(filepath.Join(cacheDir, "search", searchSnapshotCacheVersion, ".fresh.json.gz"), path); err != nil {
		t.Fatal(err)
	}
	if holdArtifactForRead(root, entry.relative, stale) {
		t.Fatal("a replaced artifact was held as current")
	}
	file, err := entry.open()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	commit, _ := sniffCacheBase(root, entry.relative)
	if commit != "new" {
		t.Fatalf("read %q, want the replacement", commit)
	}
}

// A copy a running Graph process holds is never removed, however long ago it
// was last recorded as used.
func TestPruneCacheKeepsAHeldEngineCopy(t *testing.T) {
	cacheDir := t.TempDir()
	name := filepath.Join("executables", key('d'), "entire-graph")
	path := filepath.Join(cacheDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("engine"), 0o500); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	running, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	holdExecutableLock(running)
	if _, err := PruneCache(cacheDir, CacheLimits{MaxBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if !cacheFileExists(cacheDir, name) {
		t.Fatal("a held engine copy was removed")
	}
	running.Close()
	if _, err := PruneCache(cacheDir, CacheLimits{MaxBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if cacheFileExists(cacheDir, name) {
		t.Fatal("an unheld engine copy unused for a day was kept")
	}
}

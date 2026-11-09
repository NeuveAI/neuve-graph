package sem

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// The byte-bounded cache stays within its bound, drops the least recently
// used first, and counts hits.
func TestByteLRUBoundsAndOrder(t *testing.T) {
	cache := newByteLRU[string, int](100)
	cache.put("a", 1, 40)
	cache.put("b", 2, 40)
	if _, ok := cache.get("a"); !ok { // a is now the most recently used
		t.Fatal("a was dropped")
	}
	cache.put("c", 3, 40) // over 100: b goes, not a
	if _, ok := cache.get("b"); ok {
		t.Fatal("the least recently used survived")
	}
	if _, ok := cache.get("a"); !ok {
		t.Fatal("a recently used entry was dropped")
	}
	cache.put("d", 4, 30) // 110: c, now the least recently used, goes
	if _, ok := cache.get("c"); ok {
		t.Fatal("the least recently used survived")
	}
	stats := cache.stats()
	if stats.Bytes != 70 || stats.Hits != 2 || stats.Misses != 2 {
		t.Fatalf("stats %+v, want 70 bytes, 2 hits, 2 misses", stats)
	}
	cache.put("huge", 4, 101) // over the whole bound: never kept
	if _, ok := cache.get("huge"); ok {
		t.Fatal("an entry larger than the bound was kept")
	}
}

// The shared content reader returns what the reader returns, reading each
// commit's tree path once at a given ceiling; it keeps reads apart by commit,
// by ceiling and by the repository's directory within the tree, and never
// keeps a read that did not finish.
func TestSharedContentReaderReadsOnce(t *testing.T) {
	EnableSharedCaches(true)
	t.Cleanup(func() { EnableSharedCaches(false) })
	reads := map[string]int{}
	var mu sync.Mutex
	failing := true
	reader := func(commit, prefix string) committedRead {
		return func(path string) (string, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			reads[commit+":"+prefix+path]++
			switch {
			case strings.HasPrefix(path, "missing"):
				return "", false, nil
			case strings.HasPrefix(path, "flaky") && failing:
				return "", false, errors.New("git cat-file: signal: killed")
			}
			return commit + " " + prefix + path, true, nil
		}
	}
	first := sharedContentReader(context.Background(), reader("aa", ""), "aa", "", 1000)
	second := sharedContentReader(context.Background(), reader("aa", ""), "aa", "", 1000)
	otherCommit := sharedContentReader(context.Background(), reader("bb", ""), "bb", "", 1000)
	otherCeiling := sharedContentReader(context.Background(), reader("aa", ""), "aa", "", 2000)
	subdirectory := sharedContentReader(context.Background(), reader("aa", "pkg/"), "aa", "pkg/", 1000)
	for _, read := range []contentReader{first, second} {
		if content, ok := read("x.go"); !ok || content != "aa x.go" {
			t.Fatalf("read %q %v", content, ok)
		}
		if _, ok := read("missing.go"); ok {
			t.Fatal("a missing file read as present")
		}
		if _, ok := read("flaky.go"); ok {
			t.Fatal("a failed read read as present")
		}
	}
	otherCommit("x.go")
	otherCeiling("x.go")
	if content, ok := subdirectory("x.go"); !ok || content != "aa pkg/x.go" {
		t.Fatalf("a subdirectory's x.go read %q %v, want its own file", content, ok)
	}
	if reads["aa:x.go"] != 2 || reads["bb:x.go"] != 1 || reads["aa:missing.go"] != 1 || reads["aa:pkg/x.go"] != 1 {
		t.Fatalf("reads %v: want x.go read once per ceiling at aa, once at bb, once under pkg/", reads)
	}
	if reads["aa:flaky.go"] != 2 {
		t.Fatalf("a failed read was kept: read %d times, want 2", reads["aa:flaky.go"])
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	if content, ok := first("flaky.go"); !ok || content != "aa flaky.go" {
		t.Fatalf("after a failed read, read %q %v, want the file", content, ok)
	}
	plain := sharedContentReader(context.Background(), reader("cc", ""), "", "", 1000)
	plain("x.go")
	plain("x.go")
	if reads["cc:x.go"] != 2 {
		t.Fatal("a read with no exact commit was kept")
	}
}

// A long signature's digest is remembered by its text, from its own copy,
// and is the digest normalize gives.
func TestSharedDigestIsTheDigest(t *testing.T) {
	EnableSharedCaches(true)
	t.Cleanup(func() { EnableSharedCaches(false) })
	long := strings.Repeat("func  Big( x int )\n", 40)
	for range 3 {
		// Each time the same text at another address.
		if got, want := sharedNormalizedDigest(strings.Clone(long)), normalizedDigest(long); got != want {
			t.Fatalf("digest %s, want %s", got, want)
		}
	}
	if state := SharedCachesState()["digests"]; state.Hits != 2 || state.Bytes == 0 {
		t.Fatalf("digests %+v, want two hits", state)
	}
	if got := sharedNormalizedDigest("short sig"); got != normalizedDigest("short sig") {
		t.Fatal("a short signature's digest changed")
	}
}

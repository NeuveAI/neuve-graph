package sem

import (
	"container/list"
	"context"

	"github.com/entireio/entire-graph/internal/sharedwork"
	"strings"
	"sync"
)

// The shared caches keep, in a long-lived process that answers many searches
// (the Graph service), what searches at one base read and work out alike,
// whatever their query: file contents and the digests of symbol signatures.
// Each is bounded in bytes and drops the least recently used first; each key
// names its value exactly, so nothing stale can be served:
//
//   - contents by the exact commit, the path within the commit's tree and the
//     read's byte ceiling: a commit's file never changes, and a moved branch
//     resolves to another commit;
//   - signature digests by the signature's text.
//
// Off (nil) in a process answering one search.
type sharedCaches struct {
	contents    *byteLRU[contentKey, contentValue]
	digests     *byteLRU[string, string]
	contentWork sharedwork.Group[contentKey, contentValue]
	digestWork  sharedwork.Group[string, string]
}

type contentKey struct {
	commit, path string
	maxBytes     int
}

type contentValue struct {
	content string
	ok      bool
}

// Default bounds, in bytes.
const (
	sharedContentsMaxBytes = 256 << 20
	sharedDigestsMaxBytes  = 32 << 20
	// A file larger than this is not kept: one would crowd out many.
	sharedContentMaxFileBytes = 8 << 20
)

var (
	sharedCachesMu     sync.Mutex
	activeSharedCaches *sharedCaches
)

// EnableSharedCaches turns on, for the rest of this process, the caches
// searches at one base share; false turns them off.
func EnableSharedCaches(on bool) {
	if !on {
		enableSharedCachesSized(0, 0)
		return
	}
	enableSharedCachesSized(sharedContentsMaxBytes, sharedDigestsMaxBytes)
}

// enableSharedCachesSized turns the caches on with these bounds; all zero
// turns them off.
func enableSharedCachesSized(contents, digests int64) {
	sharedCachesMu.Lock()
	defer sharedCachesMu.Unlock()
	if contents == 0 && digests == 0 {
		activeSharedCaches = nil
		return
	}
	activeSharedCaches = &sharedCaches{
		contents: newByteLRU[contentKey, contentValue](contents),
		digests:  newByteLRU[string, string](digests),
	}
}

func currentSharedCaches() *sharedCaches {
	sharedCachesMu.Lock()
	defer sharedCachesMu.Unlock()
	return activeSharedCaches
}

// SharedCacheStats is one shared cache's size and use since the process
// started.
type SharedCacheStats struct {
	Bytes  int64 `json:"bytes"`
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`
}

// SharedCachesState reports each shared cache, by name; nil when they are
// off.
func SharedCachesState() map[string]SharedCacheStats {
	caches := currentSharedCaches()
	if caches == nil {
		return nil
	}
	return map[string]SharedCacheStats{
		"contents": caches.contents.stats(),
		"digests":  caches.digests.stats(),
	}
}

// committedRead is one read at a commit: the content and whether the file
// is shown, or err when the read did not finish (a cancelled search, a dead
// git process), which says nothing about the file.
type committedRead func(path string) (content string, ok bool, err error)

// sharedContentReader is read with the shared contents cache: the same
// commit, tree path (prefix, the repository's directory within its tree,
// then path) and ceiling read the same bytes. Only finished reads are kept.
func sharedContentReader(ctx context.Context, read committedRead, commit, prefix string, maxBytes int) contentReader {
	caches := currentSharedCaches()
	if caches == nil || commit == "" {
		return func(path string) (string, bool) {
			content, ok, err := read(path)
			return content, ok && err == nil
		}
	}
	return func(path string) (string, bool) {
		key := contentKey{commit: commit, path: prefix + path, maxBytes: maxBytes}
		if value, ok := caches.contents.get(key); ok {
			return value.content, value.ok
		}
		value, err := caches.contentWork.Do(ctx, key, func(context.Context) (contentValue, error) {
			if value, ok := caches.contents.peek(key); ok {
				return value, nil
			}
			content, ok, err := read(path)
			if err != nil {
				return contentValue{content: content}, sharedwork.Abandon(err)
			}
			value := contentValue{content: content, ok: ok}
			if len(content) <= sharedContentMaxFileBytes {
				caches.contents.put(key, value, int64(len(content)+len(key.path)+64))
			}
			return value, nil
		})
		return value.content, value.ok && err == nil
	}
}

// sharedNormalizedDigest is normalizedDigest(value), remembered by value's
// text, which the cache keeps its own copy of.
func sharedNormalizedDigest(value string) string {
	caches := currentSharedCaches()
	if caches == nil || len(value) < 256 {
		return normalizedDigest(value)
	}
	if digest, ok := caches.digests.get(value); ok {
		return digest
	}
	digest, _ := caches.digestWork.Do(context.Background(), value, func(context.Context) (string, error) {
		if digest, ok := caches.digests.peek(value); ok {
			return digest, nil
		}
		digest := normalizedDigest(value)
		// A copy keeps a slice's larger backing text out of the cache.
		caches.digests.put(strings.Clone(value), digest, int64(len(value)+len(digest)+64))
		return digest, nil
	})
	return digest
}

// byteLRU is a map bounded by the bytes its values are given, dropping the
// least recently used first.
type byteLRU[K comparable, V any] struct {
	mu     sync.Mutex
	max    int64
	bytes  int64
	order  *list.List
	items  map[K]*list.Element
	hits   int64
	misses int64
}

type byteLRUItem[K comparable, V any] struct {
	key   K
	value V
	size  int64
}

func newByteLRU[K comparable, V any](max int64) *byteLRU[K, V] {
	return &byteLRU[K, V]{max: max, order: list.New(), items: map[K]*list.Element{}}
}

func (c *byteLRU[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		c.order.MoveToFront(element)
		c.hits++
		return element.Value.(*byteLRUItem[K, V]).value, true
	}
	c.misses++
	var zero V
	return zero, false
}

// peek rechecks publication after claiming without counting a second cache request.
func (c *byteLRU[K, V]) peek(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		return element.Value.(*byteLRUItem[K, V]).value, true
	}
	var zero V
	return zero, false
}

func (c *byteLRU[K, V]) put(key K, value V, size int64) {
	if size > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		item := element.Value.(*byteLRUItem[K, V])
		c.bytes += size - item.size
		item.value, item.size = value, size
		c.order.MoveToFront(element)
	} else {
		c.items[key] = c.order.PushFront(&byteLRUItem[K, V]{key: key, value: value, size: size})
		c.bytes += size
	}
	c.evictLocked()
}

func (c *byteLRU[K, V]) evictLocked() {
	for c.bytes > c.max && c.order.Len() > 0 {
		oldest := c.order.Back()
		item := oldest.Value.(*byteLRUItem[K, V])
		c.order.Remove(oldest)
		delete(c.items, item.key)
		c.bytes -= item.size
	}
}

func (c *byteLRU[K, V]) stats() SharedCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return SharedCacheStats{Bytes: c.bytes, Hits: c.hits, Misses: c.misses}
}

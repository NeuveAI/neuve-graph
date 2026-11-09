package sem

import (
	"os"
	"slices"
	"sync"
)

// snapshotMemory keeps decoded search snapshots in a long-lived process (the
// Graph service), so a search there does not decode its base's snapshot
// again. Decoding a Neuve-sized base takes about half a second; a memory hit
// costs a copy of the snapshot's top-level arrays.
//
// A cache artifact is published by rename and never written in place, so
// the opened file's identity (device, inode, size, modification time) names
// its content exactly; a republished artifact is a new key. Every hit hands
// out fresh top-level arrays, so a search that changes a record changes its
// own copy; the records' nested values are shared and read-only.
type snapshotMemory struct {
	mu       sync.Mutex
	max      int
	entries  map[string]*snapshotMemoryEntry
	sequence uint64
	reads    int
	hits     int
}

type snapshotMemoryEntry struct {
	once     sync.Once
	cached   cachedSearchSnapshot
	err      error
	lastUsed uint64
}

var (
	snapshotMemoryMu     sync.Mutex
	activeSnapshotMemory *snapshotMemory
)

// EnableSnapshotMemory turns on, for the rest of this process, the memory of
// up to max decoded search snapshots, least recently used evicted first. Only
// a process that serves many searches turns it on.
func EnableSnapshotMemory(max int) {
	snapshotMemoryMu.Lock()
	defer snapshotMemoryMu.Unlock()
	if max <= 0 {
		activeSnapshotMemory = nil
		return
	}
	activeSnapshotMemory = &snapshotMemory{max: max, entries: map[string]*snapshotMemoryEntry{}}
}

func currentSnapshotMemory() *snapshotMemory {
	snapshotMemoryMu.Lock()
	defer snapshotMemoryMu.Unlock()
	return activeSnapshotMemory
}

// SnapshotMemoryState reports the decoded snapshots this process holds, how
// many it may hold, and how many snapshot reads it has served and how many of
// them came from memory; all zero when the memory is off.
func SnapshotMemoryState() (held, limit, reads, hits int) {
	memory := currentSnapshotMemory()
	if memory == nil {
		return 0, 0, 0, 0
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	return len(memory.entries), memory.max, memory.reads, memory.hits
}

// read returns the snapshot of the opened artifact, decoding it at most once
// per identity; concurrent readers of one identity share one decode.
func (m *snapshotMemory) read(path string, file *os.File, info os.FileInfo, decode func() (cachedSearchSnapshot, error)) (cachedSearchSnapshot, bool, error) {
	identity, ok := cacheFileIdentity(file, info)
	if !ok {
		m.mu.Lock()
		m.reads++
		m.mu.Unlock()
		cached, err := decode()
		return cached, false, err
	}
	key := path + "\x00" + identity
	m.mu.Lock()
	entry, hit := m.entries[key]
	m.reads++
	if !hit {
		entry = &snapshotMemoryEntry{}
		m.entries[key] = entry
	} else {
		m.hits++
	}
	m.sequence++
	entry.lastUsed = m.sequence
	m.evictLocked(key)
	m.mu.Unlock()
	entry.once.Do(func() { entry.cached, entry.err = decode() })
	if entry.err != nil {
		m.mu.Lock()
		if m.entries[key] == entry {
			delete(m.entries, key) // a failed read is never remembered
		}
		m.mu.Unlock()
		return cachedSearchSnapshot{}, false, entry.err
	}
	return cloneCachedSearchSnapshot(entry.cached), hit, nil
}

func (m *snapshotMemory) evictLocked(keep string) {
	for len(m.entries) > m.max {
		oldest, oldestUse := "", ^uint64(0)
		for key, entry := range m.entries {
			if key != keep && entry.lastUsed < oldestUse {
				oldest, oldestUse = key, entry.lastUsed
			}
		}
		if oldest == "" {
			return
		}
		delete(m.entries, oldest)
	}
}

// cloneCachedSearchSnapshot gives a search its own top-level record arrays.
func cloneCachedSearchSnapshot(cached cachedSearchSnapshot) cachedSearchSnapshot {
	cached.Snapshot.Files = slices.Clone(cached.Snapshot.Files)
	cached.Snapshot.Externals = slices.Clone(cached.Snapshot.Externals)
	cached.Snapshot.Symbols = slices.Clone(cached.Snapshot.Symbols)
	cached.Snapshot.Relations = slices.Clone(cached.Snapshot.Relations)
	return cached
}

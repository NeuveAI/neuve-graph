package sem

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The cache store keeps one cache directory within a cap and above a floor of
// free disk space. Everything Graph writes there counts towards the cap; what
// it may remove are the prepared artifacts (search snapshots, syntax
// generations, provider records) and the per-file extractions. The remembered
// tree-scan answers and the text index keep their own bounds inside it.
//
// It removes, in order: artifacts of other engine versions, which this build
// never reads; then artifacts of no declared base, least recently used first.
// It never removes an artifact a read holds open (each read holds a shared lock
// on the file it reads) or one of a declared base (`cache declare`: the bases
// reviews start from, declared from Records).
//
// Last use comes from a log of uses each command appends to when it ends, not
// from file access times, which mounts and sandboxes do not keep.
const (
	DefaultCacheMaxBytes     int64 = 4 << 30
	DefaultCacheMinFreeBytes int64 = 10 << 30

	envCacheMaxBytes     = "ENTIRE_GRAPH_CACHE_MAX_BYTES"
	envCacheMinFreeBytes = "ENTIRE_GRAPH_CACHE_MIN_FREE_BYTES"

	// The store's own files, in cacheStoreDir.
	cacheStoreDir      = "cache-store"
	cacheStoreUses     = "uses-v1.log"
	cacheStoreDeclared = "declared-v1.json"
	cacheStoreHeaders  = "headers-v1.json"
	cacheStoreUsage    = "usage-v1.json"
	cacheStoreLogLock  = "uses.lock"
	cacheStorePrune    = "prune.lock"

	// staleCacheTemporary is how old a writer's temporary file must be before
	// the store treats it as a crashed writer's leftover.
	staleCacheTemporary = 24 * time.Hour
)

// cacheFamilyVersions names the removable artifact families and the version
// directory of each this build reads.
var cacheFamilyVersions = map[string]string{
	"search":      searchSnapshotCacheVersion,
	"generations": preparedGenerationVersion,
	"records":     providerRecordsCacheVersion,
	"extractions": extractionCacheVersion,
}

// cacheFamiliesOfBases are the families whose artifacts belong to one commit
// and tree, so a declared base keeps them. Extractions are per file content,
// shared by every commit.
var cacheFamiliesOfBases = map[string]bool{"search": true, "generations": true, "records": true}

// CacheLimits bounds a cache directory.
type CacheLimits struct {
	MaxBytes     int64
	MinFreeBytes int64
}

// CacheLimitsFromEnv is the default bound, overridden by
// ENTIRE_GRAPH_CACHE_MAX_BYTES and ENTIRE_GRAPH_CACHE_MIN_FREE_BYTES.
func CacheLimitsFromEnv() CacheLimits {
	limits := CacheLimits{MaxBytes: DefaultCacheMaxBytes, MinFreeBytes: DefaultCacheMinFreeBytes}
	if value, err := strconv.ParseInt(os.Getenv(envCacheMaxBytes), 10, 64); err == nil && value > 0 {
		limits.MaxBytes = value
	}
	if value, err := strconv.ParseInt(os.Getenv(envCacheMinFreeBytes), 10, 64); err == nil && value >= 0 {
		limits.MinFreeBytes = value
	}
	return limits
}

// CacheUsage is a cache directory's size as last measured, and what the
// measuring prune removed.
type CacheUsage struct {
	Bytes         int64 `json:"bytes"`
	CapBytes      int64 `json:"cap_bytes"`
	MinFreeBytes  int64 `json:"min_free_bytes"`
	FreeBytes     int64 `json:"free_bytes"`
	DeclaredBytes int64 `json:"declared_bytes"`
	PinnedBytes   int64 `json:"pinned_bytes"`
	// Families holds the bytes of each top-level directory: the artifact
	// families, tree-scan-answers, text-index and the store's own files.
	Families       map[string]int64 `json:"families"`
	EvictedBytes   int64            `json:"evicted_bytes"`
	EvictedEntries int              `json:"evicted_entries"`
	MeasuredAt     string           `json:"measured_at"`
}

// cacheDeclared is the set of bases a cache keeps whatever their last use.
type cacheDeclared struct {
	Commits    []string `json:"commits"`
	Trees      []string `json:"trees"`
	DeclaredAt string   `json:"declared_at"`
}

// DeclareCacheBases replaces the cache directory's declared bases: their
// prepared artifacts are never removed while declared. Trees name the same
// bases by content, for artifacts prepared at another commit of the same tree.
func DeclareCacheBases(cacheDir string, commits, trees []string) error {
	for _, oid := range append(append([]string(nil), commits...), trees...) {
		if !validObjectID(oid) {
			return fmt.Errorf("declared base %q is not an exact lowercase object ID", oid)
		}
	}
	store, err := openCacheStore(cacheDir, true)
	if err != nil {
		return err
	}
	defer store.Close()
	declared := cacheDeclared{Commits: sortedUnique(commits), Trees: sortedUnique(trees), DeclaredAt: time.Now().UTC().Format(time.RFC3339)}
	return writeCacheStoreJSON(store.files, cacheStoreDeclared, declared)
}

func validObjectID(oid string) bool {
	return (len(oid) == 40 || len(oid) == 64) && strings.Trim(oid, "0123456789abcdef") == ""
}

func sortedUnique(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

// cacheStore is a cache directory opened as a root no path below it can
// escape, with the store's own directory opened the same way.
type cacheStore struct {
	cache *os.Root
	files *os.Root
}

func (store *cacheStore) Close() {
	store.files.Close()
	store.cache.Close()
}

// openCacheStore opens a cache directory and the store's own directory in it,
// creating that directory, and the cache directory itself when create is set
// (never for a read's bookkeeping). The store's directory must be a real
// directory, never a link: a cache directory inside a checkout would
// otherwise let a committed link point the store's writes at .git.
func openCacheStore(cacheDir string, create bool) (*cacheStore, error) {
	if cacheDir == "" {
		return nil, errors.New("cache directory is empty")
	}
	if create {
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			return nil, err
		}
	}
	cache, err := os.OpenRoot(cacheDir)
	if err != nil {
		return nil, err
	}
	if err := cache.Mkdir(cacheStoreDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		cache.Close()
		return nil, err
	}
	named, err := cache.Lstat(cacheStoreDir)
	if err != nil {
		cache.Close()
		return nil, err
	}
	if !named.IsDir() {
		cache.Close()
		return nil, fmt.Errorf("cache store %q is not a directory", cacheStoreDir)
	}
	files, err := cache.OpenRoot(cacheStoreDir)
	if err != nil {
		cache.Close()
		return nil, err
	}
	if opened, err := files.Stat("."); err != nil || !os.SameFile(named, opened) {
		files.Close()
		cache.Close()
		return nil, fmt.Errorf("cache store %q changed while it was opened", cacheStoreDir)
	}
	return &cacheStore{cache: cache, files: files}, nil
}

// writeCacheStoreJSON publishes one of the store's files by rename, so a
// reader sees the old content or the new, never a part.
func writeCacheStoreJSON(root *os.Root, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := fmt.Sprintf("%s.tmp-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	if err := root.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := root.Rename(temporary, name); err != nil {
		root.Remove(temporary)
		return err
	}
	return nil
}

func readCacheStoreJSON(root *os.Root, name string, value any) bool {
	data, err := root.ReadFile(name)
	return err == nil && json.Unmarshal(data, value) == nil
}

// cacheUses holds the artifacts this process read, by cache directory, until
// FlushCacheUse appends them to each directory's use log.
var cacheUses = struct {
	sync.Mutex
	byRoot map[string]map[string]int64
}{byRoot: map[string]map[string]int64{}}

// noteCacheUse records that an artifact was read now.
func noteCacheUse(cacheDir, relative string) {
	cacheUses.Lock()
	defer cacheUses.Unlock()
	uses := cacheUses.byRoot[cacheDir]
	if uses == nil {
		uses = map[string]int64{}
		cacheUses.byRoot[cacheDir] = uses
	}
	uses[filepath2slash(relative)] = time.Now().UnixNano()
}

func filepath2slash(relative string) string {
	return strings.ReplaceAll(relative, string(os.PathSeparator), "/")
}

// DiscardCacheUse forgets the artifacts read since the last flush, for a
// command that must write nothing to the cache. Only a command that runs
// alone in its process calls it.
func DiscardCacheUse() {
	cacheUses.Lock()
	cacheUses.byRoot = map[string]map[string]int64{}
	cacheUses.Unlock()
}

// FlushCacheUse appends the artifacts read since the last flush to their
// cache directories' use logs. A command calls it when it ends.
func FlushCacheUse() {
	cacheUses.Lock()
	pending := cacheUses.byRoot
	cacheUses.byRoot = map[string]map[string]int64{}
	cacheUses.Unlock()
	for cacheDir, uses := range pending {
		appendCacheUses(cacheDir, uses)
	}
}

func appendCacheUses(cacheDir string, uses map[string]int64) {
	store, err := openCacheStore(cacheDir, false)
	if err != nil {
		return
	}
	defer store.Close()
	unlock, err := lockCacheStoreFile(store.files, cacheStoreLogLock, true)
	if err != nil {
		return
	}
	defer unlock()
	file, err := store.files.OpenFile(cacheStoreUses, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	var buf bytes.Buffer
	for relative, at := range uses {
		fmt.Fprintf(&buf, "%d %s\n", at, relative)
	}
	_, _ = file.Write(buf.Bytes())
}

// readCacheUses is each artifact's last recorded use.
func readCacheUses(files *os.Root) map[string]int64 {
	uses := map[string]int64{}
	file, err := files.Open(cacheStoreUses)
	if err != nil {
		return uses
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		at, relative, ok := strings.Cut(scanner.Text(), " ")
		if !ok {
			continue
		}
		when, err := strconv.ParseInt(at, 10, 64)
		if err != nil {
			continue
		}
		if when > uses[relative] {
			uses[relative] = when
		}
	}
	return uses
}

// cacheUnit is what the store removes as one: an artifact with its
// generation marker and the artifacts derived from it.
type cacheUnit struct {
	family   string
	main     string   // the artifact's path below the cache directory
	paths    []string // every path removed with it, main first
	bytes    int64
	expired  bool // of another engine version
	lastUse  int64
	declared bool
}

// cacheHeader is the base an artifact was prepared for, read once from its
// head and remembered by path, size and modification time.
type cacheHeader struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	Commit  string `json:"commit"`
	Tree    string `json:"tree"`
}

// cacheScan is one walk of a cache directory.
type cacheScan struct {
	families map[string]int64
	total    int64
	units    []*cacheUnit
}

func scanCache(root *os.Root) (cacheScan, error) {
	scan := cacheScan{families: map[string]int64{}}
	units := map[string]*cacheUnit{}
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while walking
			}
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		parts := strings.Split(name, "/")
		scan.families[parts[0]] += info.Size()
		scan.total += info.Size()
		current, removable := cacheFamilyVersions[parts[0]]
		if !removable || len(parts) < 3 || !strings.HasSuffix(name, ".json.gz") {
			return nil
		}
		if strings.HasPrefix(path.Base(name), ".") {
			// A writer's temporary: removable once no writer can still be
			// filling it, as a crashed one's leftover.
			if time.Since(info.ModTime()) > staleCacheTemporary {
				scan.units = append(scan.units, &cacheUnit{family: parts[0], main: name, paths: []string{name}, bytes: info.Size(), expired: true})
			}
			return nil
		}
		// family/version/key.json.gz, or family/version/group/key.json.gz
		// (derived from group). A generation marker,
		// family/version/key.generation.json.gz, is never removed: without
		// it the entry's generation reads as the one every view derived
		// before the first rebuild carries, and a view a rebuild retired
		// could become valid again.
		if len(parts) == 3 && strings.HasSuffix(parts[2], ".generation.json.gz") {
			return nil
		}
		key := strings.TrimSuffix(parts[2], ".json.gz")
		unitName := path.Join(parts[0], parts[1], key+".json.gz")
		unit := units[unitName]
		if unit == nil {
			unit = &cacheUnit{family: parts[0], main: unitName, expired: parts[1] != current}
			units[unitName] = unit
		}
		unit.bytes += info.Size()
		if name == unitName {
			unit.paths = append([]string{name}, unit.paths...)
		} else {
			unit.paths = append(unit.paths, name)
		}
		return nil
	})
	for _, unit := range units {
		if len(unit.paths) > 0 {
			scan.units = append(scan.units, unit)
		}
	}
	sort.Slice(scan.units, func(i, j int) bool { return scan.units[i].main < scan.units[j].main })
	return scan, err
}

// markDeclared reads the base of each artifact of a base family and marks
// those of a declared commit or tree.
func markDeclared(store *cacheStore, units []*cacheUnit, declared cacheDeclared) {
	root := store.cache
	var known map[string]cacheHeader
	readCacheStoreJSON(store.files, cacheStoreHeaders, &known)
	headers := map[string]cacheHeader{}
	commits, trees := map[string]bool{}, map[string]bool{}
	for _, oid := range declared.Commits {
		commits[oid] = true
	}
	for _, oid := range declared.Trees {
		trees[oid] = true
	}
	for _, unit := range units {
		if unit.expired || !cacheFamiliesOfBases[unit.family] {
			continue
		}
		// The artifact itself, or for views whose artifact is gone, the first
		// view: each records the base it was prepared for.
		named := unit.paths[0]
		info, err := root.Lstat(named)
		if err != nil {
			continue
		}
		header, ok := known[named]
		if !ok || header.Size != info.Size() || header.ModTime != info.ModTime().UnixNano() {
			header = cacheHeader{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
			header.Commit, header.Tree = sniffCacheBase(root, named)
		}
		headers[named] = header
		unit.declared = (header.Commit != "" && commits[header.Commit]) || (header.Tree != "" && trees[header.Tree])
	}
	_ = writeCacheStoreJSON(store.files, cacheStoreHeaders, headers)
}

// sniffCacheBase reads the commit and tree an artifact was prepared for from
// the head of its stream: top-level fields, or those of its top-level
// snapshot (a syntax generation). It stops as soon as both are read.
func sniffCacheBase(root *os.Root, name string) (commit, tree string) {
	file, err := openRootBoundedRegularFile(root, name)
	if err != nil {
		return "", ""
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return "", ""
	}
	defer reader.Close()
	return sniffJSONBase(io.LimitReader(reader, 64<<20))
}

func sniffJSONBase(reader io.Reader) (commit, tree string) {
	type level struct {
		object  bool
		key     string
		wantKey bool
	}
	decoder := json.NewDecoder(reader)
	var stack []level
	valueDone := func() {
		if top := len(stack) - 1; top >= 0 && stack[top].object {
			stack[top].wantKey = true
		}
	}
	for tokens := 0; tokens < 1_000_000; tokens++ {
		token, err := decoder.Token()
		if err != nil {
			return commit, tree
		}
		if top := len(stack) - 1; top >= 0 && stack[top].object && stack[top].wantKey {
			if delim, ok := token.(json.Delim); ok && delim == '}' {
				stack = stack[:top]
				valueDone()
				continue
			}
			key, _ := token.(string)
			stack[top].key, stack[top].wantKey = key, false
			continue
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				stack = append(stack, level{object: true, wantKey: true})
			case '[':
				stack = append(stack, level{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		case string:
			var at []string
			for _, frame := range stack {
				at = append(at, frame.key)
			}
			switch strings.Join(at, "/") {
			case "commit", "snapshot/commit":
				if commit == "" {
					commit = value
				}
			case "tree", "snapshot/tree":
				if tree == "" {
					tree = value
				}
			}
		}
		valueDone()
		if commit != "" && tree != "" {
			return commit, tree
		}
	}
	return commit, tree
}

// PruneCache removes artifacts until the cache directory is within its cap
// and the disk keeps its floor of free space, or nothing removable is left,
// then records and returns its usage. Artifacts of keepTrees are kept as
// declared ones are (an index keeps the tree it just prepared). A prune
// already running elsewhere makes this one only measure.
func PruneCache(cacheDir string, limits CacheLimits, keepTrees ...string) (CacheUsage, error) {
	// When the machine-wide budget host exists, eviction asks it first and
	// does not run when it cannot be reached (architecture record 0013,
	// section 3); this is where that check goes.
	store, err := openCacheStore(cacheDir, true)
	if err != nil {
		return CacheUsage{}, err
	}
	defer store.Close()
	unlock, err := lockCacheStoreFile(store.files, cacheStorePrune, false)
	if err != nil {
		return measureCache(store, limits, false, keepTrees)
	}
	defer unlock()
	return measureCache(store, limits, true, keepTrees)
}

// MeasureCache records and returns a cache directory's usage without
// removing anything.
func MeasureCache(cacheDir string, limits CacheLimits) (CacheUsage, error) {
	store, err := openCacheStore(cacheDir, true)
	if err != nil {
		return CacheUsage{}, err
	}
	defer store.Close()
	return measureCache(store, limits, false, nil)
}

// LastCacheUsage is the usage the last prune or measure recorded, measuring
// now when none was.
func LastCacheUsage(cacheDir string, limits CacheLimits) (CacheUsage, error) {
	store, err := openCacheStore(cacheDir, false)
	if err != nil {
		return CacheUsage{}, err
	}
	defer store.Close()
	var usage CacheUsage
	if readCacheStoreJSON(store.files, cacheStoreUsage, &usage) {
		usage.CapBytes, usage.MinFreeBytes = limits.MaxBytes, limits.MinFreeBytes
		return usage, nil
	}
	return measureCache(store, limits, false, nil)
}

func measureCache(store *cacheStore, limits CacheLimits, evict bool, keepTrees []string) (CacheUsage, error) {
	root := store.cache
	scan, err := scanCache(root)
	if err != nil {
		return CacheUsage{}, err
	}
	var declared cacheDeclared
	readCacheStoreJSON(store.files, cacheStoreDeclared, &declared)
	declared.Trees = append(declared.Trees, keepTrees...)
	markDeclared(store, scan.units, declared)
	uses := readCacheUses(store.files)
	usage := CacheUsage{CapBytes: limits.MaxBytes, MinFreeBytes: limits.MinFreeBytes, FreeBytes: freeDiskBytes(root.Name()), Families: scan.families}
	for _, unit := range scan.units {
		for _, name := range unit.paths {
			unit.lastUse = max(unit.lastUse, uses[name])
		}
		if unit.lastUse == 0 {
			if info, err := root.Lstat(unit.paths[0]); err == nil {
				unit.lastUse = info.ModTime().UnixNano()
			}
		}
		if unit.declared {
			usage.DeclaredBytes += unit.bytes
		}
	}
	total := scan.total
	if evict {
		// Other versions first, then the least recently used.
		candidates := slices.DeleteFunc(slices.Clone(scan.units), func(unit *cacheUnit) bool { return unit.declared })
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].expired != candidates[j].expired {
				return candidates[i].expired
			}
			return candidates[i].lastUse < candidates[j].lastUse
		})
		for _, unit := range candidates {
			if total <= limits.MaxBytes && (usage.FreeBytes < 0 || usage.FreeBytes >= limits.MinFreeBytes) {
				break
			}
			removed, ok := removeCacheUnit(root, unit)
			if !ok {
				usage.PinnedBytes += unit.bytes
				continue
			}
			total -= removed
			scan.families[unit.family] -= removed
			usage.EvictedBytes += removed
			usage.EvictedEntries++
			if usage.FreeBytes >= 0 {
				usage.FreeBytes += removed
			}
		}
		compactCacheUses(store, uses)
	}
	usage.Bytes = total
	usage.MeasuredAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeCacheStoreJSON(store.files, cacheStoreUsage, usage)
	return usage, nil
}

// removeCacheUnit removes an artifact with its derived artifacts, unless a
// read holds one of them open or a writer has just replaced one. It returns
// the bytes removed.
func removeCacheUnit(root *os.Root, unit *cacheUnit) (int64, bool) {
	var unlocks []func()
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}()
	for _, name := range unit.paths {
		unlock, held := lockArtifactForRemoval(root, name)
		if !held {
			return 0, false
		}
		unlocks = append(unlocks, unlock)
	}
	var removed int64
	for _, name := range unit.paths {
		info, err := root.Lstat(name)
		if err != nil {
			continue
		}
		if err := root.Remove(name); err == nil {
			removed += info.Size()
		}
	}
	// The derived directory, now empty unless written since.
	_ = root.Remove(strings.TrimSuffix(unit.main, ".json.gz"))
	return removed, true
}

// compactCacheUses rewrites the use log to one line per artifact still
// present.
func compactCacheUses(store *cacheStore, uses map[string]int64) {
	unlock, err := lockCacheStoreFile(store.files, cacheStoreLogLock, true)
	if err != nil {
		return
	}
	defer unlock()
	for relative, at := range readCacheUses(store.files) {
		uses[relative] = max(uses[relative], at)
	}
	var buf bytes.Buffer
	names := make([]string, 0, len(uses))
	for relative := range uses {
		names = append(names, relative)
	}
	sort.Strings(names)
	for _, relative := range names {
		if _, err := store.cache.Lstat(relative); err == nil {
			fmt.Fprintf(&buf, "%d %s\n", uses[relative], relative)
		}
	}
	temporary := fmt.Sprintf("%s.tmp-%d", cacheStoreUses, os.Getpid())
	if err := store.files.WriteFile(temporary, buf.Bytes(), 0o600); err != nil {
		return
	}
	if err := store.files.Rename(temporary, cacheStoreUses); err != nil {
		store.files.Remove(temporary)
	}
}

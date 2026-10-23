//go:build !windows

// Package textindex is Graph's text index: Zoekt shards of blob contents,
// built per changed blob at commit time and assembled per tree, used only to
// narrow the files a tree scan reads. It never answers a search itself: every
// file it names is still decided by the scan, and every file it cannot vouch
// for is scanned, so a search's answers do not depend on it.
//
// Layout under <cache>/text-index/v1/:
//
//	<id>.zoekt     an immutable Zoekt shard; each document is one blob, named
//	               by its object id, holding its content
//	<id>.blobs     the shard's manifest: every blob the build considered, with
//	               whether its content is in the shard; written after the shard,
//	               so a shard without a manifest is never read
//	build.lock     held while a process builds, so two processes never index
//	               the same blobs
//
// A blob is indexed once, whatever trees or repositories hold it, so a tree's
// index is the union of the shards that hold its blobs: a base shard from the
// first build plus a small shard per later commit for the blobs it changed.
// Shards are content files named by id, ready to live in an object store.
package textindex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

const (
	// DefaultMaxBytes bounds the index on disk: one or two bases of a
	// repository the size of Neuve (a Neuve base takes 200 to 370 MB); past
	// it the least recently used shards are evicted, never those of the tree
	// just built.
	DefaultMaxBytes = 512 << 20
	// maxDocBytes keeps very large blobs out of the index (they are always
	// scanned), as Zoekt's own file limit does.
	maxDocBytes = 1 << 20
	// maxShardContent bounds one shard's content, well under Zoekt's 32-bit
	// offsets, and the memory a build holds while assembling it.
	maxShardContent = 64 << 20
	layoutVersion   = "v1"
)

// MaxBytesEnv overrides DefaultMaxBytes.
const MaxBytesEnv = "ENTIRE_GRAPH_TEXT_INDEX_MAX_BYTES"

// Store is one cache directory's text index.
type Store struct {
	dir      string
	maxBytes int64
}

// Open returns the store under cacheDir, or nil for an empty cacheDir.
func Open(cacheDir string, maxBytes int64) *Store {
	if cacheDir == "" {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
		if value, err := strconv.ParseInt(os.Getenv(MaxBytesEnv), 10, 64); err == nil && value > 0 {
			maxBytes = value
		}
	}
	return &Store{dir: filepath.Join(cacheDir, "text-index", layoutVersion), maxBytes: maxBytes}
}

// manifest is what one shard's build considered: indexed blobs are in the
// shard; skipped ones (NUL bytes, invalid UTF-8, too large or too small for
// Zoekt) are not, and are always candidates.
type manifest struct {
	id       string
	indexed  map[string]bool
	skipped  map[string]bool
	shardCRC uint32 // CRC-32C of the shard file, checked before each search
	hasCRC   bool
}

func (s *Store) manifests() ([]manifest, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []manifest
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".blobs")
		if !ok {
			continue
		}
		m, err := readManifest(filepath.Join(s.dir, entry.Name()), id)
		if err != nil {
			continue // as if absent, so its blobs are scanned
		}
		if _, err := os.Stat(filepath.Join(s.dir, id+".zoekt")); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// errDamagedManifest marks a manifest that was read and fails its checks,
// as opposed to one that could not be read.
var errDamagedManifest = errors.New("damaged text index manifest")

func readManifest(path, id string) (m manifest, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", errDamagedManifest, err)
		}
	}()
	// The last line is the SHA-256 of everything before it, so a damaged
	// manifest is never taken to cover a blob its shard does not hold.
	body, trailer, ok := cutLastLine(data)
	if !ok {
		return manifest{}, fmt.Errorf("malformed manifest %s", path)
	}
	if sum := sha256.Sum256(body); trailer != "manifest-sha256 "+hex.EncodeToString(sum[:]) {
		return manifest{}, fmt.Errorf("manifest %s fails its checksum", path)
	}
	m = manifest{id: id, indexed: map[string]bool{}, skipped: map[string]bool{}}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		kind, value, ok := strings.Cut(scanner.Text(), " ")
		if !ok {
			return manifest{}, fmt.Errorf("malformed manifest %s", path)
		}
		switch kind {
		case "i", "s":
			if !isObjectID(value) {
				return manifest{}, fmt.Errorf("malformed manifest %s", path)
			}
			if kind == "i" {
				m.indexed[value] = true
			} else {
				m.skipped[value] = true
			}
		case "shard-crc32c":
			parsed, err := strconv.ParseUint(value, 16, 32)
			if err != nil {
				return manifest{}, fmt.Errorf("malformed manifest %s", path)
			}
			m.shardCRC, m.hasCRC = uint32(parsed), true
		default:
			return manifest{}, fmt.Errorf("malformed manifest %s", path)
		}
	}
	if !m.hasCRC {
		return manifest{}, fmt.Errorf("manifest %s names no shard checksum", path)
	}
	return m, scanner.Err()
}

// cutLastLine splits data, which must end in LF, into what precedes its last
// line and that line.
func cutLastLine(data []byte) ([]byte, string, bool) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, "", false
	}
	trimmed := data[:len(data)-1]
	cut := bytes.LastIndexByte(trimmed, '\n')
	return trimmed[:cut+1], string(trimmed[cut+1:]), true
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func isObjectID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil && strings.ToLower(oid) == oid
}

// BuildReceipt reports one build.
type BuildReceipt struct {
	// The tree built for: its blobs, the bytes of the shards that hold them
	// (the index's cost for this base) and the blobs the index cannot vouch
	// for, which every search scans.
	TreeBlobs          int   `json:"tree_blobs"`
	TreeIndexBytes     int64 `json:"tree_index_bytes"`
	TreeUnindexedBlobs int   `json:"tree_unindexed_blobs"`

	BlobsIndexed     int   `json:"blobs_indexed"`
	BlobsSkipped     int   `json:"blobs_skipped"`
	BytesIndexed     int64 `json:"bytes_indexed"`
	BuildMS          int64 `json:"build_ms"`
	IndexBytesOnDisk int64 `json:"index_bytes_on_disk"`
	EvictedBytes     int64 `json:"evicted_bytes"`
	CapBytes         int64 `json:"cap_bytes"`
	Shards           int   `json:"shards"`
}

// BlobReader streams the content of blobs by object id, in order.
type BlobReader func(ctx context.Context, oids []string, visit func(oid string, data []byte) error) error

// Build indexes every blob of oids that no shard holds yet into one new
// shard (or a few, past maxShardContent).
func (s *Store) Build(ctx context.Context, oids []string, read BlobReader) (BuildReceipt, error) {
	started := time.Now()
	receipt := BuildReceipt{CapBytes: s.maxBytes}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return receipt, err
	}
	lock, err := os.OpenFile(filepath.Join(s.dir, "build.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return receipt, err
	}
	defer lock.Close()
	if err := lockBuild(ctx, lock); err != nil {
		return receipt, err
	}
	known, err := s.manifests()
	if err != nil {
		return receipt, err
	}
	have := map[string]bool{}
	for _, m := range known {
		for oid := range m.indexed {
			have[oid] = true
		}
		for oid := range m.skipped {
			have[oid] = true
		}
	}
	var missing []string
	seen := map[string]bool{}
	for _, oid := range oids {
		if !have[oid] && !seen[oid] {
			seen[oid] = true
			missing = append(missing, oid)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		if err := s.buildShards(ctx, missing, read, &receipt); err != nil {
			return receipt, err
		}
	}
	protect := s.shardsHolding(oids)
	receipt.EvictedBytes, receipt.IndexBytesOnDisk, receipt.Shards = s.evict(protect)
	receipt.TreeBlobs = len(seenAll(oids))
	for id := range protect {
		receipt.TreeIndexBytes += s.shardBytes(id)
	}
	receipt.TreeUnindexedBlobs = receipt.TreeBlobs - s.indexedCount(oids)
	receipt.BuildMS = time.Since(started).Milliseconds()
	return receipt, nil
}

func (s *Store) buildShards(ctx context.Context, oids []string, read BlobReader, receipt *BuildReceipt) error {
	var builder *index.ShardBuilder
	var lines []string
	content := 0
	flush := func() error {
		if builder == nil && len(lines) == 0 {
			return nil
		}
		if builder == nil {
			var err error
			if builder, err = newBuilder(); err != nil {
				return err
			}
		}
		if err := s.writeShard(builder, lines); err != nil {
			return err
		}
		builder, lines, content = nil, nil, 0
		return nil
	}
	err := read(ctx, oids, func(oid string, data []byte) error {
		if !indexable(data) {
			lines = append(lines, "s "+oid)
			receipt.BlobsSkipped++
			return nil
		}
		if builder != nil && content+len(data) > maxShardContent {
			if err := flush(); err != nil {
				return err
			}
		}
		if builder == nil {
			var err error
			if builder, err = newBuilder(); err != nil {
				return err
			}
		}
		if err := builder.Add(index.Document{Name: oid, Content: data, Language: "Text"}); err != nil {
			return err
		}
		lines = append(lines, "i "+oid)
		content += len(data)
		receipt.BlobsIndexed++
		receipt.BytesIndexed += int64(len(data))
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func newBuilder() (*index.ShardBuilder, error) {
	return index.NewShardBuilder(&zoekt.Repository{Name: "graph-blobs"})
}

// indexable keeps out of the index what Zoekt would mark as skipped, and what
// its rune-based matching could read differently from the scan's bytes.
func indexable(data []byte) bool {
	if len(data) < 3 || len(data) > maxDocBytes {
		return false
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return false
	}
	var checker index.DocChecker
	return checker.Check(data, 1<<30, true) == index.SkipReasonNone
}

// writeShard streams the shard to a temporary file, names it by the SHA-256
// of its content and its manifest, and renames both into place, the shard
// first: a shard without its manifest is never read.
func (s *Store) writeShard(builder *index.ShardBuilder, lines []string) error {
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	sum, crc := sha256.New(), crc32.New(castagnoli)
	werr := builder.Write(io.MultiWriter(tmp, sum, crc))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		return errors.Join(werr, cerr)
	}
	body := strings.Join(append(lines, fmt.Sprintf("shard-crc32c %08x", crc.Sum32())), "\n") + "\n"
	bodySum := sha256.Sum256([]byte(body))
	manifestData := []byte(body + "manifest-sha256 " + hex.EncodeToString(bodySum[:]) + "\n")
	sum.Write(manifestData)
	id := hex.EncodeToString(sum.Sum(nil))[:32]
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, id+".zoekt")); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, id+".blobs"), manifestData)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func seenAll(oids []string) map[string]bool {
	set := make(map[string]bool, len(oids))
	for _, oid := range oids {
		set[oid] = true
	}
	return set
}

// shardsHolding names the shards that hold any of oids as indexed content.
func (s *Store) shardsHolding(oids []string) map[string]bool {
	wanted := seenAll(oids)
	held := map[string]bool{}
	ms, _ := s.manifests()
	for _, m := range ms {
		for oid := range m.indexed {
			if wanted[oid] {
				held[m.id] = true
				break
			}
		}
	}
	return held
}

func (s *Store) indexedCount(oids []string) int {
	wanted := seenAll(oids)
	indexed := map[string]bool{}
	ms, _ := s.manifests()
	for _, m := range ms {
		for oid := range m.indexed {
			if wanted[oid] {
				indexed[oid] = true
			}
		}
	}
	return len(indexed)
}

func (s *Store) shardBytes(id string) int64 {
	var size int64
	for _, suffix := range []string{".zoekt", ".blobs"} {
		if info, err := os.Stat(filepath.Join(s.dir, id+suffix)); err == nil {
			size += info.Size()
		}
	}
	return size
}

// evict keeps the store under its cap, least recently used shards first
// (a search touches the manifests it reads).
//
// It runs only inside Build, under the build lock, so no other build is
// writing: a temporary file, a shard without a valid manifest or a manifest
// without its shard is what a killed build left behind, and goes first.
func (s *Store) evict(protect map[string]bool) (removed, total int64, shards int) {
	removed += s.sweepLeftovers()
	ms, _ := s.manifests()
	type shardFile struct {
		id    string
		size  int64
		mtime time.Time
	}
	var files []shardFile
	for _, m := range ms {
		var size int64
		var mtime time.Time
		for _, suffix := range []string{".zoekt", ".blobs"} {
			if info, err := os.Stat(filepath.Join(s.dir, m.id+suffix)); err == nil {
				size += info.Size()
				if suffix == ".blobs" {
					mtime = info.ModTime()
				}
			}
		}
		files = append(files, shardFile{id: m.id, size: size, mtime: mtime})
		total += size
	}
	if total > s.maxBytes {
		sort.Slice(files, func(a, b int) bool { return files[a].mtime.Before(files[b].mtime) })
		target := s.maxBytes * 8 / 10
		var kept []shardFile
		for len(files) > 0 && total > target {
			f := files[0]
			files = files[1:]
			if protect[f.id] {
				kept = append(kept, f)
				continue
			}
			// The manifest goes first: without it the shard is never read.
			if err := os.Remove(filepath.Join(s.dir, f.id+".blobs")); err != nil {
				continue
			}
			_ = os.Remove(filepath.Join(s.dir, f.id+".zoekt"))
			total -= f.size
			removed += f.size
		}
		files = append(files, kept...)
	}
	return removed, total, len(files)
}

// LookupStats reports one candidate lookup.
type LookupStats struct {
	Lookups    int // shard searches that answered in full
	Failed     int // shards that could not answer; their blobs stay uncovered
	Candidates int // blobs named, including those the index cannot vouch for
	Uncovered  int // blobs no shard vouched for, always candidates
}

// tenantEnforcementEnv is Zoekt's own setting that makes its searches skip
// documents without a tenant; with it set, no answer can be trusted.
const tenantEnforcementEnv = "SRC_TENANT_ENFORCEMENT_MODE"

// Candidates returns every blob of oids that may contain one of literals
// (ASCII, folded to lower case, at least three bytes): the blobs whose
// indexed content contains one, case-insensitively, and every blob the index
// does not hold.
//
// A shard counts only when it answers in full: its file matches the checksum
// its manifest recorded, the search neither panics nor stops early, and
// Zoekt reports nothing skipped. Any other shard's blobs are candidates, and
// cancellation is an error, never a short answer.
func (s *Store) Candidates(ctx context.Context, oids []string, literals []string) (map[string]bool, LookupStats, error) {
	var stats LookupStats
	if os.Getenv(tenantEnforcementEnv) != "" {
		return nil, stats, fmt.Errorf("the text index is unavailable while %s is set", tenantEnforcementEnv)
	}
	candidates := map[string]bool{}
	ms, err := s.manifests()
	if err != nil {
		return nil, stats, err
	}
	wanted := map[string]bool{}
	for _, oid := range oids {
		wanted[oid] = true
	}
	covered := map[string]bool{}
	var substrings []query.Q
	for _, literal := range literals {
		substrings = append(substrings, &query.Substring{Pattern: literal, Content: true})
	}
	for _, m := range ms {
		set := map[string]struct{}{}
		for oid := range m.indexed {
			if wanted[oid] && !covered[oid] {
				set[oid] = struct{}{}
			}
		}
		if len(set) == 0 {
			continue
		}
		hits, err := s.search(ctx, m, set, substrings)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, stats, ctxErr
		}
		if err != nil {
			stats.Failed++
			continue // its blobs stay uncovered and are scanned
		}
		stats.Lookups++
		for oid := range set {
			covered[oid] = true
		}
		for oid := range hits {
			candidates[oid] = true
		}
		now := time.Now()
		_ = os.Chtimes(filepath.Join(s.dir, m.id+".blobs"), now, now)
	}
	for oid := range wanted {
		if !covered[oid] {
			candidates[oid] = true
			stats.Uncovered++
		}
	}
	stats.Candidates = len(candidates)
	return candidates, stats, nil
}

func (s *Store) search(ctx context.Context, m manifest, set map[string]struct{}, substrings []query.Q) (hits map[string]bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			hits, err = nil, fmt.Errorf("text index shard %s: %v", m.id, recovered)
		}
	}()
	file, err := os.Open(filepath.Join(s.dir, m.id+".zoekt"))
	if err != nil {
		return nil, err
	}
	crc := crc32.New(castagnoli)
	if _, err := io.Copy(crc, file); err != nil {
		file.Close()
		return nil, err
	}
	if crc.Sum32() != m.shardCRC {
		file.Close()
		return nil, fmt.Errorf("text index shard %s fails its checksum", m.id)
	}
	indexFile, err := index.NewIndexFile(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	searcher, err := index.NewSearcher(indexFile)
	if err != nil {
		indexFile.Close()
		return nil, err
	}
	defer searcher.Close()
	q := &query.Type{Type: query.TypeFileName, Child: query.NewAnd(&query.FileNameSet{Set: set}, query.NewOr(substrings...))}
	result, err := searcher.Search(ctx, q, &zoekt.SearchOptions{ShardMaxMatchCount: 1 << 30, TotalMaxMatchCount: 1 << 30})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err // Zoekt returns what it found so far when cancelled
	}
	// A shard "skipped by filter" holds none of the query's trigrams: a
	// complete "no match". Skips from limits, time or crashes are not.
	if st := result.Stats; st.Crashes > 0 || st.FilesSkipped > 0 || st.ShardsSkipped > 0 {
		return nil, fmt.Errorf("text index shard %s did not search in full: %+v", m.id, st)
	}
	hits = map[string]bool{}
	for _, file := range result.Files {
		hits[file.FileName] = true
	}
	return hits, nil
}

// sweepLeftovers removes what a killed build left: temporary files, shards
// whose manifest is missing, and manifests that were read and fail their
// checks, with their shards. A file that merely could not be read is kept,
// so a transient error never costs the index a valid shard.
func (s *Store) sweepLeftovers() (removed int64) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	present := map[string]bool{}
	for _, entry := range entries {
		present[entry.Name()] = true
	}
	remove := func(name string) {
		if info, err := os.Lstat(filepath.Join(s.dir, name)); err == nil && os.Remove(filepath.Join(s.dir, name)) == nil {
			removed += info.Size()
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, ".tmp-"):
			remove(name)
		case strings.HasSuffix(name, ".zoekt"):
			if !present[strings.TrimSuffix(name, ".zoekt")+".blobs"] {
				remove(name)
			}
		case strings.HasSuffix(name, ".blobs"):
			id := strings.TrimSuffix(name, ".blobs")
			_, err := readManifest(filepath.Join(s.dir, name), id)
			if errors.Is(err, errDamagedManifest) || (err == nil && !present[id+".zoekt"]) {
				remove(name)
				remove(id + ".zoekt")
			}
		}
	}
	return removed
}

// ScanCandidates adapts the store to a tree scan's candidate hook: the hits
// and how many lookups answered, or an error when no shard could answer at
// all although some hold the blobs.
func (s *Store) ScanCandidates(ctx context.Context, oids, literals []string) (map[string]bool, int, error) {
	hits, stats, err := s.Candidates(ctx, oids, literals)
	if err == nil && stats.Lookups == 0 && stats.Failed > 0 {
		err = fmt.Errorf("no text index shard could answer (%d failed)", stats.Failed)
	}
	return hits, stats.Lookups, err
}

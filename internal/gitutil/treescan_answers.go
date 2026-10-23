package gitutil

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TreeScanAnswers remembers each blob's answer to each pattern: the first
// matching line, or none. A blob's content never changes, so an answer keyed
// by blob id, pattern and the scan's engine identity (scanner version,
// platform regex engine, locale) never goes stale; it is invalidated only by
// a change of key, never by time. Answers are shared across hunks, reviews,
// trees and processes through files under one cache directory.
//
// Layout, under <cache>/tree-scan-answers/<engine>/:
//
//	blobsets/<id>          the blobs one scan read, each with its NUL-in-head
//	                       flag; <id> is the SHA-256 of the file's content
//	patterns/<p>/<id>      for pattern p (SHA-256 of its text), the first
//	                       matching line of every blob of blobset <id> that
//	                       matches, ending in the SHA-256 of what precedes
//	                       it; every other blob of the set matches nothing
//	claims/<p>             a lock held while a process computes pattern p
//
// Every file is written whole under a temporary name and renamed into place,
// so a reader sees a complete file or none, and every file is verified when
// read: a record whose blobset is gone, that fails its checksum or that names
// a blob outside its blobset is a miss, never an answer. The store is only an
// accelerator: any failure to read, claim or write it means remembering
// nothing, and the scan computes its answers as if there were no store.
type TreeScanAnswers struct {
	root     string // <cache>/tree-scan-answers, shared by every engine
	dir      string // this engine's directory under root
	maxBytes int64
}

// DefaultTreeScanAnswersMaxBytes bounds the disk use of the whole store,
// every engine's answers together; past it the least recently used patterns
// are evicted.
const DefaultTreeScanAnswersMaxBytes = 256 << 20

const treeScanAnswersMaxBytesEnv = "ENTIRE_GRAPH_SCAN_ANSWERS_MAX_BYTES"

// treeScanEngineVersion changes whenever the scanner could answer
// differently or the store's layout changes, so older answers are never read.
const treeScanEngineVersion = "tree-scan-2"

// maxStoredLineBytes bounds the line text a record keeps. A longer matched
// line is remembered by its place in the blob and read back from the blob
// only when a search reports it.
const maxStoredLineBytes = 1024

// OpenTreeScanAnswers opens the store under cacheDir; an empty cacheDir gives
// a nil store, which remembers nothing.
func OpenTreeScanAnswers(cacheDir string) *TreeScanAnswers {
	if cacheDir == "" {
		return nil
	}
	maxBytes := int64(DefaultTreeScanAnswersMaxBytes)
	if value, err := parsePositiveInt64(os.Getenv(treeScanAnswersMaxBytesEnv)); err == nil {
		maxBytes = value
	}
	sum := sha256.Sum256([]byte(treeScanEngineIdentity()))
	root := filepath.Join(cacheDir, "tree-scan-answers")
	return &TreeScanAnswers{root: root, dir: filepath.Join(root, hex.EncodeToString(sum[:12])), maxBytes: maxBytes}
}

func parsePositiveInt64(text string) (int64, error) {
	var value int64
	if _, err := fmt.Sscan(strings.TrimSpace(text), &value); err != nil || value <= 0 {
		return 0, errors.New("not a positive integer")
	}
	return value, nil
}

func patternKey(pattern string) string {
	sum := sha256.Sum256([]byte(pattern))
	return hex.EncodeToString(sum[:])
}

// knownAnswers is what the store holds for some blobs of a scan: per blob,
// its NUL flag (when any blobset holds it) and its answer to each pattern it
// has one for.
type knownAnswers struct {
	nul     map[string]bool
	answers map[string]map[int]blobLine // oid -> pattern index -> line (number 0: none)
}

// lookup returns every remembered answer for oids and patterns.
func (s *TreeScanAnswers) lookup(patterns []string, oids []string) knownAnswers {
	known := knownAnswers{nul: map[string]bool{}, answers: map[string]map[int]blobLine{}}
	if s == nil {
		return known
	}
	wanted := make(map[string]bool, len(oids))
	for _, oid := range oids {
		wanted[oid] = true
	}
	sets := map[string]map[string]bool{} // blobset id -> oid -> NUL flag, nil when unreadable
	for index, pattern := range patterns {
		dir := filepath.Join(s.dir, "patterns", patternKey(pattern))
		records, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		used := false
		for _, record := range records {
			id := record.Name()
			if !isHexID(id) {
				continue
			}
			set, ok := sets[id]
			if !ok {
				set = s.readBlobset(id)
				sets[id] = set
			}
			if set == nil {
				continue
			}
			lines, ok := readAnswerRecord(filepath.Join(dir, id), id, set)
			if !ok {
				continue
			}
			used = true
			for oid, nul := range set {
				if !wanted[oid] {
					continue
				}
				known.nul[oid] = nul
				byPattern := known.answers[oid]
				if byPattern == nil {
					byPattern = map[int]blobLine{}
					known.answers[oid] = byPattern
				}
				byPattern[index] = lines[oid]
			}
		}
		if used {
			now := time.Now()
			_ = os.Chtimes(dir, now, now) // recency for eviction
		}
	}
	return known
}

// record stores the answers of one scan over blobs, for every pattern. A
// failure only means those answers are not remembered.
func (s *TreeScanAnswers) record(patterns []string, scanned map[string]blobAnswer) {
	if s == nil || len(scanned) == 0 {
		return
	}
	oids := make([]string, 0, len(scanned))
	for oid := range scanned {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	var set bytes.Buffer
	set.WriteString(blobsetMagic)
	writeUint32(&set, uint32(len(oids)))
	for _, oid := range oids {
		if err := writeOID(&set, oid); err != nil {
			return
		}
		flag := byte(0)
		if scanned[oid].nulInHead {
			flag = 1
		}
		set.WriteByte(flag)
	}
	sum := sha256.Sum256(set.Bytes())
	id := hex.EncodeToString(sum[:])
	setPath := filepath.Join(s.dir, "blobsets", id)
	if s.readBlobset(id) == nil {
		if err := writeFileAtomic(setPath, set.Bytes()); err != nil {
			return
		}
	}
	for index, pattern := range patterns {
		var rec bytes.Buffer
		rec.WriteString(recordMagic)
		rec.WriteString(id)
		var hits []string
		for _, oid := range oids {
			if scanned[oid].lines[index].number > 0 {
				hits = append(hits, oid)
			}
		}
		writeUint32(&rec, uint32(len(hits)))
		for _, oid := range hits {
			line := scanned[oid].lines[index]
			if !line.stored || line.size != len(line.text) {
				return // a line this scan did not read in full cannot be recorded
			}
			_ = writeOID(&rec, oid)
			writeUint32(&rec, uint32(line.number))
			writeUint64(&rec, uint64(line.offset))
			writeUint32(&rec, uint32(line.size))
			if line.size <= maxStoredLineBytes {
				rec.WriteByte(1)
				rec.WriteString(line.text)
			} else {
				rec.WriteByte(0)
			}
		}
		sum := sha256.Sum256(rec.Bytes())
		rec.Write(sum[:])
		dir := filepath.Join(s.dir, "patterns", patternKey(pattern))
		if err := writeFileAtomic(filepath.Join(dir, id), rec.Bytes()); err != nil {
			continue
		}
	}
}

const (
	blobsetMagic = "GTSB2\n"
	recordMagic  = "GTSR2\n"
)

func (s *TreeScanAnswers) readBlobset(id string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(s.dir, "blobsets", id))
	if err != nil {
		return nil
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != id {
		return nil
	}
	r := answerReader{data: data}
	if !r.magic(blobsetMagic) {
		return nil
	}
	count, ok := r.uint32()
	if !ok {
		return nil
	}
	set := make(map[string]bool, count)
	for range count {
		oid, ok := r.oid()
		if !ok {
			return nil
		}
		flag, ok := r.byte()
		if !ok || flag > 1 {
			return nil
		}
		set[oid] = flag == 1
	}
	if !r.done() {
		return nil
	}
	return set
}

// readAnswerRecord reads one pattern's record over blobset id, whose blobs
// are set; a record that fails its checksum or names a blob outside the set
// is rejected whole.
func readAnswerRecord(path, id string, set map[string]bool) (map[string]blobLine, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < sha256.Size {
		return nil, false
	}
	body, trailer := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], trailer) {
		return nil, false
	}
	r := answerReader{data: body}
	if !r.magic(recordMagic) || !r.magic(id) {
		return nil, false
	}
	count, ok := r.uint32()
	if !ok {
		return nil, false
	}
	lines := make(map[string]blobLine, count)
	for range count {
		oid, ok := r.oid()
		if !ok {
			return nil, false
		}
		if _, member := set[oid]; !member {
			return nil, false
		}
		number, ok := r.uint32()
		if !ok || number == 0 {
			return nil, false
		}
		offset, ok := r.uint64()
		if !ok {
			return nil, false
		}
		size, ok := r.uint32()
		if !ok {
			return nil, false
		}
		stored, ok := r.byte()
		if !ok || stored > 1 || (stored == 1) != (size <= maxStoredLineBytes) {
			return nil, false
		}
		line := blobLine{number: int(number), offset: int64(offset), size: int(size)}
		if stored == 1 {
			text, ok := r.bytes(int(size))
			if !ok {
				return nil, false
			}
			line.text, line.stored = string(text), true
		}
		lines[oid] = line
	}
	return lines, r.done()
}

type answerReader struct {
	data []byte
	pos  int
}

func (r *answerReader) magic(text string) bool {
	if !bytes.HasPrefix(r.data[r.pos:], []byte(text)) {
		return false
	}
	r.pos += len(text)
	return true
}

func (r *answerReader) uint32() (uint32, bool) {
	if len(r.data)-r.pos < 4 {
		return 0, false
	}
	value := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return value, true
}

func (r *answerReader) uint64() (uint64, bool) {
	if len(r.data)-r.pos < 8 {
		return 0, false
	}
	value := binary.LittleEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	return value, value <= 1<<62
}

func (r *answerReader) byte() (byte, bool) {
	if r.pos >= len(r.data) {
		return 0, false
	}
	r.pos++
	return r.data[r.pos-1], true
}

func (r *answerReader) bytes(n int) ([]byte, bool) {
	if n < 0 || len(r.data)-r.pos < n {
		return nil, false
	}
	r.pos += n
	return r.data[r.pos-n : r.pos], true
}

// oid reads a length-prefixed raw object id (20 bytes SHA-1, 32 SHA-256).
func (r *answerReader) oid() (string, bool) {
	size, ok := r.byte()
	if !ok || (size != 20 && size != 32) {
		return "", false
	}
	raw, ok := r.bytes(int(size))
	if !ok {
		return "", false
	}
	return hex.EncodeToString(raw), true
}

func (r *answerReader) done() bool { return r.pos == len(r.data) }

func writeUint32(b *bytes.Buffer, value uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	b.Write(buf[:])
}

func writeUint64(b *bytes.Buffer, value uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], value)
	b.Write(buf[:])
}

func writeOID(b *bytes.Buffer, oid string) error {
	raw, err := hex.DecodeString(oid)
	if err != nil || (len(raw) != 20 && len(raw) != 32) || hex.EncodeToString(raw) != oid {
		return fmt.Errorf("object id %q is not a lower-case SHA-1 or SHA-256", oid)
	}
	b.WriteByte(byte(len(raw)))
	b.Write(raw)
	return nil
}

func isHexID(name string) bool {
	if len(name) != 64 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// writeFileAtomic replaces path with data through a rename, so a reader sees
// the old file or the new one, never part of either, and a damaged file is
// healed by the next write.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
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

// evict keeps the whole store, every engine's directory together, under its
// bound: the least recently used patterns go first, with their claim files,
// then blobsets no remaining record names, then engine directories left
// empty. It reports the bytes removed and the bytes left.
func (s *TreeScanAnswers) evict() (removed, left int64) {
	if s == nil {
		return 0, 0
	}
	type patternDir struct {
		engine, path string
		size         int64
		mtime        time.Time
		sets         []string
	}
	var dirs []patternDir
	var total int64
	type setFile struct {
		engine, name string
		size         int64
	}
	var sets []setFile
	engines, _ := os.ReadDir(s.root)
	for _, engine := range engines {
		if !engine.IsDir() {
			continue
		}
		base := filepath.Join(s.root, engine.Name())
		patterns, _ := os.ReadDir(filepath.Join(base, "patterns"))
		for _, entry := range patterns {
			path := filepath.Join(base, "patterns", entry.Name())
			info, err := entry.Info()
			if err != nil {
				continue
			}
			d := patternDir{engine: base, path: path, mtime: info.ModTime()}
			records, _ := os.ReadDir(path)
			for _, record := range records {
				if ri, err := record.Info(); err == nil {
					d.size += ri.Size()
				}
				if isHexID(record.Name()) {
					d.sets = append(d.sets, record.Name())
				}
			}
			total += d.size
			dirs = append(dirs, d)
		}
		files, _ := os.ReadDir(filepath.Join(base, "blobsets"))
		for _, entry := range files {
			if info, err := entry.Info(); err == nil {
				sets = append(sets, setFile{engine: base, name: entry.Name(), size: info.Size()})
				total += info.Size()
			}
		}
	}
	if total <= s.maxBytes {
		return 0, total
	}
	target := s.maxBytes * 8 / 10
	sort.Slice(dirs, func(a, b int) bool { return dirs[a].mtime.Before(dirs[b].mtime) })
	cut := 0
	for cut < len(dirs) && total > target {
		d := dirs[cut]
		if err := os.RemoveAll(d.path); err == nil {
			total -= d.size
			removed += d.size
			_ = os.Remove(filepath.Join(d.engine, "claims", filepath.Base(d.path)))
		}
		cut++
	}
	kept := map[string]bool{}
	for _, d := range dirs[cut:] {
		for _, id := range d.sets {
			kept[filepath.Join(d.engine, id)] = true
		}
	}
	for _, set := range sets {
		if kept[filepath.Join(set.engine, set.name)] || strings.HasPrefix(set.name, ".tmp-") {
			continue
		}
		if err := os.Remove(filepath.Join(set.engine, "blobsets", set.name)); err == nil {
			total -= set.size
			removed += set.size
		}
	}
	for _, engine := range engines {
		base := filepath.Join(s.root, engine.Name())
		if base == s.dir {
			continue
		}
		if patterns, err := os.ReadDir(filepath.Join(base, "patterns")); err == nil && len(patterns) == 0 {
			_ = os.RemoveAll(base) // an older engine with nothing left to answer
		}
	}
	return removed, total
}

// blobLine is a pattern's first matching line in a blob: its 1-based number,
// where it starts and how long it is, and its text without the LF. A line
// remembered without its text (longer than maxStoredLineBytes) has stored
// false until resolveLineTexts reads it back.
type blobLine struct {
	number int
	offset int64
	size   int
	text   string
	stored bool
}

// blobAnswer is one blob's result for every pattern of a plan.
type blobAnswer struct {
	nulInHead bool
	// lines[pattern] is that pattern's first matching line; number 0 is none.
	lines []blobLine
	// phantom is the line number git reports for the empty tail of a file no
	// line of which matched (phantomLine), or 0. It belongs to the whole
	// plan, not to one pattern.
	phantom int
}

func (a blobAnswer) first() (blobLine, bool) {
	best := blobLine{}
	for _, line := range a.lines {
		if line.number > 0 && (best.number == 0 || line.number < best.number) {
			best = line
		}
	}
	if best.number == 0 && a.phantom > 0 {
		return blobLine{number: a.phantom, stored: true}, true
	}
	return best, best.number > 0
}

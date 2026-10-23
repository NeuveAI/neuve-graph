//go:build cgo && !windows

package gitutil

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const sha256Size = sha256.Size

func gitGrepReference(t *testing.T, repo, treeish string, patterns []string) []GrepMatch {
	t.Helper()
	var reference []GrepMatch
	if err := grepPatternLines(t.Context(), repo, treeish, patterns, 1, func(match GrepMatch) error {
		reference = append(reference, match)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return reference
}

func scanWith(t *testing.T, repo, treeish string, patterns []string, store *TreeScanAnswers) ([]GrepMatch, TreeScanStats) {
	t.Helper()
	var got []GrepMatch
	stats, err := ScanTreePatternLinesWith(t.Context(), repo, treeish, patterns, TreeScanOptions{Answers: store}, func(match GrepMatch) error {
		got = append(got, match)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, stats
}

// A remembered answer must be the answer: a warm store answers exactly what
// git grep answers without reading a blob, and a new commit reads only the
// blobs it changed, while still answering what git grep answers there.
func TestRememberedAnswersAreTheScansAnswers(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	store := OpenTreeScanAnswers(t.TempDir())
	patterns := treeScanEdgePatternSets["literal and class"]
	want := gitGrepReference(t, repo, "HEAD", patterns)

	cold, coldStats := scanWith(t, repo, "HEAD", patterns, store)
	if !reflect.DeepEqual(cold, want) || coldStats.AnswerHits != 0 || coldStats.BlobsRead == 0 {
		t.Fatalf("cold scan = %q %+v, want git's answer computed", cold, coldStats)
	}
	warm, warmStats := scanWith(t, repo, "HEAD", patterns, store)
	if !reflect.DeepEqual(warm, want) {
		t.Fatalf("warm scan differs from git grep:\n got %q\nwant %q", warm, want)
	}
	// late-nul.txt's first matching line is over 9,000 bytes, longer than a
	// record keeps, so only that blob is read back for its text.
	if warmStats.BlobsRead != 1 || warmStats.AnswerMisses != 0 || warmStats.AnswerHits != coldStats.AnswerMisses {
		t.Fatalf("warm stats = %+v, want every answer remembered and only late-nul.txt read (cold %+v)", warmStats, coldStats)
	}

	// A subset of the patterns is answered from the same records.
	subset := patterns[:2]
	if got, stats := scanWith(t, repo, "HEAD", subset, store); !reflect.DeepEqual(got, gitGrepReference(t, repo, "HEAD", subset)) || stats.BlobsRead != 1 {
		t.Fatalf("subset scan = %q %+v", got, stats)
	}

	if err := os.WriteFile(filepath.Join(repo, "case.txt"), []byte("no match now\nFoo again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "added.txt"), []byte("ÉBAR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "change two files")
	next, nextStats := scanWith(t, repo, "HEAD", patterns, store)
	if want := gitGrepReference(t, repo, "HEAD", patterns); !reflect.DeepEqual(next, want) {
		t.Fatalf("scan after a commit differs from git grep:\n got %q\nwant %q", next, want)
	}
	if nextStats.BlobsRead != 3 {
		t.Fatalf("scan after a commit read %d blobs, want the 2 it changed and late-nul.txt's long line (%+v)", nextStats.BlobsRead, nextStats)
	}
}

// The locale changes what an expression matches, so answers from one locale
// are never read under another.
func TestRememberedAnswersAreKeyedByLocale(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	dir := t.TempDir()
	patterns := treeScanEdgePatternSets["literal and class"]
	t.Setenv("LC_ALL", "C")
	scanWith(t, repo, "HEAD", patterns, OpenTreeScanAnswers(dir))
	t.Setenv("LC_ALL", "en_US.UTF-8")
	got, stats := scanWith(t, repo, "HEAD", patterns, OpenTreeScanAnswers(dir))
	if stats.AnswerHits != 0 || !reflect.DeepEqual(got, gitGrepReference(t, repo, "HEAD", patterns)) {
		t.Fatalf("UTF-8 scan reused C answers: %+v", stats)
	}
}

// A damaged or half-evicted store is a miss, never an answer.
func TestRememberedAnswersIgnoreDamagedFiles(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	dir := t.TempDir()
	store := OpenTreeScanAnswers(dir)
	patterns := treeScanEdgePatternSets["literal and class"]
	scanWith(t, repo, "HEAD", patterns, store)
	records, err := filepath.Glob(filepath.Join(store.dir, "patterns", "*", "*"))
	if err != nil || len(records) == 0 {
		t.Fatalf("no records written: %v", err)
	}
	damage := func(flip func(data []byte, hits uint32) []byte) {
		t.Helper()
		for _, record := range records {
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			hits := binary.LittleEndian.Uint32(data[len(recordMagic)+64:])
			if err := os.WriteFile(record, flip(data, hits), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Every damage below still parses: a flipped byte of a line's text, or of
	// a blob id. Only the checksum (and the blob's membership) can tell.
	damage(func(data []byte, hits uint32) []byte {
		if hits == 0 {
			return data[:len(data)-1] // truncated
		}
		data[len(data)-sha256Size-1] ^= 0x20 // the last line's last text byte
		return data
	})
	got, stats := scanWith(t, repo, "HEAD", patterns, store)
	if !reflect.DeepEqual(got, gitGrepReference(t, repo, "HEAD", patterns)) || stats.AnswerHits != 0 {
		t.Fatalf("damaged records were read: %q %+v", got, stats)
	}
	scanWith(t, repo, "HEAD", patterns, OpenTreeScanAnswers(t.TempDir()))
	store = OpenTreeScanAnswers(t.TempDir())
	scanWith(t, repo, "HEAD", patterns, store)
	records, _ = filepath.Glob(filepath.Join(store.dir, "patterns", "*", "*"))
	damage(func(data []byte, hits uint32) []byte {
		if hits > 0 {
			data[len(recordMagic)+64+4+1] ^= 0x01 // the first hit's blob id
		}
		return data
	})
	if got, stats := scanWith(t, repo, "HEAD", patterns, store); !reflect.DeepEqual(got, gitGrepReference(t, repo, "HEAD", patterns)) {
		t.Fatalf("records with a damaged blob id were read: %q %+v", got, stats)
	}
	sets, _ := filepath.Glob(filepath.Join(store.dir, "blobsets", "*"))
	for _, set := range sets {
		_ = os.Remove(set)
	}
	if got, stats := scanWith(t, repo, "HEAD", patterns, store); !reflect.DeepEqual(got, gitGrepReference(t, repo, "HEAD", patterns)) || stats.AnswerHits != 0 {
		t.Fatalf("records without their blobset were read: %+v", stats)
	}
}

// A pattern another process is computing is waited for, not computed beside
// it; when the holder leaves without answering, the waiter computes it.
func TestRememberedAnswersWaitForAClaimHolder(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	dir := t.TempDir()
	patterns := []string{"[fF][oO][oO]", "2026"}
	// Every blob already has its answer to the second pattern, so the blobs
	// lack only the first, which the "other process" holds.
	scanWith(t, repo, "HEAD", patterns[1:], OpenTreeScanAnswers(dir))
	holder := OpenTreeScanAnswers(dir)
	claim, held := holder.tryClaim(patterns[0])
	if claim == nil || held {
		t.Fatal("could not take the claim")
	}
	type outcome struct {
		got   []GrepMatch
		stats TreeScanStats
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		var got []GrepMatch
		stats, err := ScanTreePatternLinesWith(t.Context(), repo, "HEAD", patterns, TreeScanOptions{Answers: OpenTreeScanAnswers(dir)}, func(match GrepMatch) error {
			got = append(got, match)
			return nil
		})
		done <- outcome{got, stats, err}
	}()
	select {
	case <-done:
		t.Fatal("the scan did not wait for the claim holder")
	case <-time.After(300 * time.Millisecond):
	}
	claim.release()
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !reflect.DeepEqual(result.got, gitGrepReference(t, repo, "HEAD", patterns)) {
		t.Fatalf("waiting scan differs from git grep: %q", result.got)
	}
	if result.stats.AnswerWaits != 1 || result.stats.BlobsRead == 0 || result.stats.AnswerMisses == 0 {
		t.Fatalf("stats = %+v, want one wait and its own scan after the holder left nothing", result.stats)
	}
}

// The store stays under its bound, least recently used patterns first, and a
// scan after eviction still answers what git grep answers.
func TestRememberedAnswersStayUnderTheirBound(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	t.Setenv(treeScanAnswersMaxBytesEnv, "4096")
	store := OpenTreeScanAnswers(t.TempDir())
	for name, patterns := range treeScanEdgePatternSets {
		if got, _ := scanWith(t, repo, "HEAD", patterns, store); !reflect.DeepEqual(got, gitGrepReference(t, repo, "HEAD", gitPatternFileLines(patterns))) {
			t.Fatalf("%s: scan differs from git grep", name)
		}
	}
	if _, total := store.evict(); total > 4096 {
		t.Fatalf("store holds %d bytes over its 4096-byte bound", total)
	}
}

// git's empty-tail line belongs to a whole pattern list, so a list that can
// produce it is answered by scanning and never remembered.
func TestRememberedAnswersSkipEmptyTailLists(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	store := OpenTreeScanAnswers(t.TempDir())
	patterns := treeScanEdgePatternSets["matches the empty tail"]
	for range 2 {
		got, stats := scanWith(t, repo, "HEAD", patterns, store)
		if !reflect.DeepEqual(got, gitGrepReference(t, repo, "HEAD", patterns)) || stats.AnswerHits != 0 {
			t.Fatalf("empty-tail list = %q %+v", got, stats)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(store.dir, "patterns")); len(entries) != 0 {
		t.Fatalf("an empty-tail list was remembered: %d pattern directories", len(entries))
	}
}

// Every remembered answer, per blob and pattern, is the one a fresh scan
// computes: line number, text, and "none". Merged results alone would hide a
// uniform error in line numbers.
func TestRememberedAnswersRoundTripPerPattern(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	store := OpenTreeScanAnswers(t.TempDir())
	patterns := append(treeScanEdgePatternSets["literal and class"], treeScanEdgePatternSets["alias boundaries"]...)
	scanWith(t, repo, "HEAD", patterns, store)
	plan, err := newTreeScanPlan(gitPatternFileLines(patterns))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := listTreeRegularFiles(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var oids []string
	seen := map[string]bool{}
	for _, entry := range entries {
		if !seen[entry.oid] {
			seen[entry.oid] = true
			oids = append(oids, entry.oid)
		}
	}
	fresh, _, err := scanBlobs(t.Context(), repo, oids, plan)
	if err != nil {
		t.Fatal(err)
	}
	known := store.lookup(plan.sources(), oids)
	checked := 0
	for oid, answer := range fresh {
		remembered, ok := known.answers[oid]
		if !ok {
			continue // binary by attribute everywhere: never scanned, never stored
		}
		if known.nul[oid] != answer.nulInHead {
			t.Fatalf("blob %s: remembered NUL flag %v, scanned %v", oid, known.nul[oid], answer.nulInHead)
		}
		for index, line := range answer.lines {
			got, ok := remembered[index]
			if ok && !got.stored && line.size > maxStoredLineBytes {
				got.text, got.stored = line.text, true // kept by place only
			}
			if !ok || got != line {
				t.Fatalf("blob %s pattern %q: remembered %+v (%v), scanned %+v", oid, plan.patterns[index].source, got, ok, line)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("nothing was compared")
	}
}

// The store only accelerates: when its directory cannot be used at all, the
// scan answers exactly what git grep answers, by the one-pass route, and
// reports nothing remembered.
func TestRememberedAnswersStoreFailureIsAMiss(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	cache := t.TempDir()
	// A plain file where the store's directory should be.
	if err := os.WriteFile(filepath.Join(cache, "tree-scan-answers"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	patterns := treeScanEdgePatternSets["literal and class"]
	for range 2 {
		got, stats := scanWith(t, repo, "HEAD", patterns, OpenTreeScanAnswers(cache))
		if !reflect.DeepEqual(got, gitGrepReference(t, repo, "HEAD", patterns)) || stats.Route != TreeScanRouteOnePass || stats.AnswerHits != 0 {
			t.Fatalf("scan over an unusable store = %q %+v", got, stats)
		}
	}
}

// The bound covers the whole store: answers left by another engine (another
// locale here; an OS update or a new scanner version alike) are evicted too.
func TestRememberedAnswersBoundCoversEveryEngine(t *testing.T) {
	repo := treeScanEdgeFixture(t)
	cache := t.TempDir()
	t.Setenv(treeScanAnswersMaxBytesEnv, "3000")
	patterns := treeScanEdgePatternSets["literal and class"]
	for _, locale := range []string{"C", "en_US.UTF-8", "C.UTF-8", "en_GB.UTF-8", "fr_FR.UTF-8", "de_DE.UTF-8"} {
		t.Setenv("LC_ALL", locale)
		scanWith(t, repo, "HEAD", patterns, OpenTreeScanAnswers(cache))
	}
	var total int64
	err := filepath.Walk(filepath.Join(cache, "tree-scan-answers"), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total > 3000 {
		t.Fatalf("store holds %d bytes across engines, over its 3000-byte bound", total)
	}
}

// A matched line longer than a record keeps is remembered by its place and
// read back from its blob when a search reports it.
func TestRememberedAnswersReadLongLinesBack(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	long := "x foo " + strings.Repeat("minified;", 400)
	files := map[string]string{
		"long.js":  "first\n" + long + "\nfoo short\n",
		"short.go": "foo\n",
		"none.txt": "nothing\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "long")
	store := OpenTreeScanAnswers(t.TempDir())
	patterns := []string{"[fF][oO][oO]", "zz[q]"}
	want := gitGrepReference(t, repo, "HEAD", patterns)
	scanWith(t, repo, "HEAD", patterns, store)
	got, stats := scanWith(t, repo, "HEAD", patterns, store)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("warm scan with a long line differs from git grep:\n got %q\nwant %q", got, want)
	}
	if stats.AnswerMisses != 0 || stats.BlobsRead != 1 {
		t.Fatalf("stats = %+v, want every answer remembered and only long.js read back", stats)
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

func historicalPreparedCommand(t *testing.T, repo, cache, commit, profile, command string) ([]byte, error) {
	t.Helper()
	args := []string{command, "--repo", repo, "--cache-dir", cache, "--prepared-only", "--commit", commit, "--profile", profile, "--format", "json"}
	if command == "query" {
		args = append(args, "--query", "dispatchReview")
	} else {
		args = append(args, "--symbol", "Desk", "--relation", "CONTAINS", "--direction", "out")
	}
	var out bytes.Buffer
	err := Run(t.Context(), Options{Version: "test", Stdout: &out}, args)
	return out.Bytes(), err
}

func TestHistoricalPreparedCommandsWithoutCheckout(t *testing.T) {
	repo, cache := incrementalCommandFixture(t)
	old := incrementalCommandCommit(t, repo)
	oldIndex := incrementalIndexCommand(t, repo, cache, old)
	fullCache := t.TempDir()
	if _, _, err := sem.PreindexProviderSnapshot(t.Context(), repo, "test", sem.ProviderSnapshotOptions{Profile: sem.ProfileFull}, fullCache); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "review.ts", "export class Desk { next() { return 'new'; } }\nexport function dispatchReview() { return 'new'; }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "new HEAD source")
	head := incrementalCommandCommit(t, repo)
	common, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	beforeWorktrees, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	before := sem.StructuralWorkSnapshot()
	buildID, err := sem.ExecutableBuildIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"syntax-only", "full"} {
		directory := cache
		if profile == "full" {
			directory = fullCache
		}
		queryBytes, err := historicalPreparedCommand(t, repo, directory, old, profile, "query")
		if err != nil {
			t.Fatal(err)
		}
		var query sem.SearchResponse
		if err := json.Unmarshal(queryBytes, &query); err != nil {
			t.Fatal(err)
		}
		if query.Commit != old || query.Profile != profile || query.Stats.AnalysisScope != "complete-prepared" || !query.Stats.IndexCacheHit || len(query.Results) == 0 || !strings.Contains(query.Results[0].Snippet, "'old'") || strings.Contains(query.Results[0].Snippet, "'new'") {
			t.Fatalf("mixed old source/query=%s", queryBytes)
		}
		if query.PreparedRead == nil || query.PreparedRead.ProducerBuildID != buildID || len(query.PreparedRead.PolicyID) != 64 || strings.Trim(query.PreparedRead.PolicyID, "0123456789abcdef") != "" || query.PreparedRead.Source.Scope != "" || query.PreparedRead.Source.CommonDir != common || query.PreparedRead.ExternalPolicyOrigin != "serving-directory-captured" || query.PreparedRead.GitignoreOrigin != "target-commit" {
			t.Fatalf("unqualified read=%s", queryBytes)
		}
		if profile == "syntax-only" && (query.PreparedRead.ArtifactFamily != "syntax-generation" || query.PreparedRead.GenerationID != oldIndex.IncrementalPreparation.GenerationID) {
			t.Fatal("read lost generation identity")
		}
		if profile == "full" && (query.PreparedRead.ArtifactFamily != "search-snapshot" || query.PreparedRead.GenerationID != "") {
			t.Fatal("legacy full read invented generation identity")
		}
		var stream bytes.Buffer
		if err := Run(t.Context(), Options{Version: "test", Stdout: &stream}, []string{"query", "--repo", repo, "--cache-dir", directory, "--prepared-only", "--commit", old, "--profile", profile, "--query", "dispatchReview", "--format", "ndjson"}); err != nil {
			t.Fatal(err)
		}
		var header struct {
			PreparedRead *sem.PreparedReadReceipt `json:"prepared_read"`
			Commit       string                   `json:"commit"`
		}
		if err := json.NewDecoder(&stream).Decode(&header); err != nil || header.Commit != old || !reflect.DeepEqual(header.PreparedRead, query.PreparedRead) {
			t.Fatal("NDJSON omitted or changed exact read qualification")
		}
		neighborBytes, err := historicalPreparedCommand(t, repo, directory, old, profile, "neighbors")
		if err != nil {
			t.Fatal(err)
		}
		var neighbors neighborResponse
		if err := json.Unmarshal(neighborBytes, &neighbors); err != nil {
			t.Fatal(err)
		}
		if neighbors.Commit != old || neighbors.Tree != query.Tree || neighbors.Profile != profile || neighbors.AnalysisScope != "complete-prepared" || !reflect.DeepEqual(neighbors.PreparedRead, query.PreparedRead) || len(neighbors.Matches) != 1 || len(neighbors.Matches[0].Outgoing) == 0 {
			t.Fatalf("old neighbors=%s", neighborBytes)
		}
		for _, edge := range neighbors.Matches[0].Outgoing {
			if edge.Endpoint.Name == "next" {
				t.Fatal("neighbors used current HEAD")
			}
		}
	}
	if sem.StructuralWorkSnapshot() != before {
		t.Fatal("historical read performed structural work")
	}
	afterWorktrees, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil || !bytes.Equal(beforeWorktrees, afterWorktrees) || incrementalCommandCommit(t, repo) != head {
		t.Fatal("historical read changed HEAD/worktree inventory")
	}
	for _, profile := range []string{"syntax-only", "full"} {
		output, err := historicalPreparedCommand(t, repo, cache, head, profile, "query")
		var failure *sem.PreparedSnapshotError
		if !errors.As(err, &failure) || len(output) != 0 {
			t.Fatalf("missing preparation emitted success: %s %v", output, err)
		}
	}
	if sem.StructuralWorkSnapshot() != before {
		t.Fatal("missing historical preparation rebuilt")
	}
}

func TestHistoricalPreparedSelectorsRejectAmbiguity(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for _, selectors := range [][]string{
		{"--commit", oid}, {"--commit", "HEAD", "--prepared-only"}, {"--commit", "", "--prepared-only"}, {"--commit", strings.Repeat("A", 40), "--prepared-only"},
		{"--commit", oid, "--prepared-only", "--head"}, {"--head", "--prepared-only", "--commit", oid},
		{"--worktree", "--commit", oid, "--prepared-only"}, {"--commit", oid, "--prepared-only", "--worktree", "--head"},
		{"--commit", oid, "--prepared-only", "--no-cache"}, {"--commit", oid, "--commit", oid, "--prepared-only"},
	} {
		for _, command := range []string{"query", "neighbors"} {
			args := append([]string{command, "--repo", t.TempDir()}, selectors...)
			if command == "query" {
				args = append(args, "--query", "dispatchReview")
			} else {
				args = append(args, "--symbol", "Desk")
			}
			before := sem.StructuralWorkSnapshot()
			var out bytes.Buffer
			if err := Run(t.Context(), Options{Version: "test", Stdout: &out}, args); err == nil || out.Len() != 0 || sem.StructuralWorkSnapshot() != before {
				t.Fatalf("ambiguous selector accepted: %v %v", args, err)
			}
		}
	}
	repo, cache := incrementalCommandFixture(t)
	commit := incrementalCommandCommit(t, repo)
	incrementalIndexCommand(t, repo, cache, commit)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	if err := Run(ctx, Options{Version: "test", Stdout: &out}, []string{"query", "--repo", repo, "--cache-dir", cache, "--prepared-only", "--commit", commit, "--profile", "syntax-only", "--query", "dispatchReview"}); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancelled exact read=%v output=%s", err, &out)
	}
	if !sem.Capabilities().OptionalLocalOnlyFeatures["exact_commit_prepared_read"] {
		t.Fatal("exact read capability absent")
	}
}

// exactCommitStreamFixture is a repository whose HEAD is not the commit under
// test: legacyOnly exists only at old, and the working tree carries an
// untracked edit that an exact-commit read must neither see nor touch.
func exactCommitStreamFixture(t *testing.T) (repo, old, head string) {
	t.Helper()
	repo = t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Exact Commit Stream")
	git(t, repo, "config", "user.email", "exact@example.invalid")
	write(t, repo, "review.ts", "export function legacyOnly() { return 1; }\nexport function caller() { return legacyOnly(); }\n")
	write(t, repo, "helper.go", "package review\n\nfunc helper() {}\n")
	write(t, repo, "generate.go", "//go:build ignore\n\npackage main\n\nfunc main() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "old source")
	old = rev(t, repo, "HEAD^{commit}")
	write(t, repo, "review.ts", "export function current() { return 2; }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "new HEAD source")
	head = rev(t, repo, "HEAD^{commit}")
	write(t, repo, "draft.ts", "export function draftOnly() { return 3; }\n")
	return repo, old, head
}

func runExactStream(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(t.Context(), Options{Version: "exact-stream-test", Stdout: &out, Stderr: io.Discard}, args)
	return out.Bytes(), err
}

// exactStreamHeader decodes the first NDJSON line and the names of every symbol
// record and every relation target in the stream.
func exactStreamHeader(t *testing.T, stream []byte) (sem.SnapshotHeader, map[string]bool, map[string]bool) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(stream), []byte("\n"))
	var header sem.SnapshotHeader
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("header: %v", err)
	}
	symbols, targets := map[string]bool{}, map[string]bool{}
	for _, line := range lines[1:] {
		var record struct {
			RecordType string `json:"record_type"`
			Name       string `json:"name"`
			ToID       string `json:"to_id"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("record: %v", err)
		}
		switch record.RecordType {
		case "symbol":
			symbols[record.Name] = true
		case "relation":
			targets[record.ToID[strings.LastIndex(record.ToID, ":")+1:]] = true
		}
	}
	return header, symbols, targets
}

// TestBulkStreamsBuildExactCommitWithoutCheckout is the whole feature: the
// three bulk streams build a commit that is not HEAD from Git objects, report
// the target's commit and tree, and leave HEAD, the worktree list and the
// working tree exactly as they were.
func TestBulkStreamsBuildExactCommitWithoutCheckout(t *testing.T) {
	repo, old, head := exactCommitStreamFixture(t)
	oldTree := rev(t, repo, old+"^{tree}")
	status := func() string {
		out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "status", "--porcelain=v1", "--untracked-files=all").Output()
		if err != nil {
			t.Fatal(err)
		}
		worktrees, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "list", "--porcelain").Output()
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(repo, "review.ts"))
		if err != nil {
			t.Fatal(err)
		}
		return string(out) + string(worktrees) + string(content) + rev(t, repo, "HEAD")
	}
	before := status()
	for _, mode := range []string{"snapshot", "symbols", "edges"} {
		stream, err := runExactStream(t, mode, "--repo", repo, "--no-cache", "--commit", old)
		if err != nil {
			t.Fatalf("%s --commit: %v", mode, err)
		}
		header, symbols, targets := exactStreamHeader(t, stream)
		if header.Commit != old || header.Tree != oldTree {
			t.Fatalf("%s header commit=%s tree=%s, want %s %s", mode, header.Commit, header.Tree, old, oldTree)
		}
		if mode != "edges" && (!symbols["legacyOnly"] || symbols["current"] || symbols["draftOnly"]) {
			t.Fatalf("%s read the wrong source: %v", mode, symbols)
		}
		if mode == "edges" && !targets["legacyOnly"] {
			t.Fatalf("edges lost the old commit's relations: %v", targets)
		}
		// The bulk commands turn on the source omission warnings.
		if !bytes.Contains(stream, []byte(`"code":"W_GO_BUILD_EXCLUDED","severity":"info","file_path":"generate.go"`)) {
			t.Fatalf("%s summary does not name the build-excluded Go file", mode)
		}
	}
	// The harness reads the compact form for a commit.
	compact, err := runExactStream(t, "snapshot", "--repo", repo, "--no-cache", "--format", "compact-ndjson", "--commit", old)
	if err != nil {
		t.Fatal(err)
	}
	index, err := sem.LoadCompactSnapshot(bytes.NewReader(compact))
	if err != nil {
		t.Fatal(err)
	}
	if index.Snapshot.Header.Commit != old || index.Snapshot.Header.Tree != oldTree {
		t.Fatalf("compact header commit=%s tree=%s", index.Snapshot.Header.Commit, index.Snapshot.Header.Tree)
	}
	found := false
	for _, symbol := range index.Snapshot.Symbols {
		found = found || symbol.Name == "legacyOnly"
	}
	if !found {
		t.Fatal("compact --commit stream lost legacyOnly")
	}
	// SCIP is built from the same stream; its protobuf carries symbol names raw.
	scip, err := runExactStream(t, "snapshot", "--repo", repo, "--format", "scip", "--commit", old)
	if err != nil || !bytes.Contains(scip, []byte("legacyOnly")) || bytes.Contains(scip, []byte("current")) {
		t.Fatalf("scip --commit read the wrong source: %v", err)
	}
	if after := status(); after != before || rev(t, repo, "HEAD") != head {
		t.Fatalf("exact-commit stream changed the repository:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestBulkStreamExactCommitRefusals: a wrong target must never fall back to
// HEAD or the working tree. Malformed selectors are refused before any Git
// process (the repository does not even exist); well-formed ones that are not
// commits are refused with nothing on stdout.
func TestBulkStreamExactCommitRefusals(t *testing.T) {
	oid := strings.Repeat("a", 40)
	missing := filepath.Join(t.TempDir(), "absent")
	for _, selectors := range [][]string{
		{"--commit", "abc1234"}, {"--commit", strings.Repeat("A", 40)}, {"--commit", "HEAD"}, {"--commit", "main"}, {"--commit", ""},
		{"--commit", oid, "--worktree"}, {"--worktree", "--commit", oid}, {"--commit", oid, "--commit", oid}, {"--commit"},
	} {
		for _, mode := range []string{"snapshot", "symbols", "edges"} {
			args := append([]string{mode, "--repo", missing}, selectors...)
			stream, err := runExactStream(t, args...)
			if err == nil || len(stream) != 0 || !strings.Contains(err.Error(), "--commit") {
				t.Fatalf("%v accepted or failed late: %v stdout=%q", args, err, stream)
			}
		}
	}
	repo, old, _ := exactCommitStreamFixture(t)
	git(t, repo, "tag", "-a", "-m", "annotated", "v-old", old)
	tagObject := rev(t, repo, "v-old")
	if tagObject == old {
		t.Fatal("fixture tag is not an annotated tag object")
	}
	blob := rev(t, repo, old+":review.ts")
	for _, target := range []string{tagObject, blob, strings.Repeat("0", 40), strings.Repeat("b", 64)} {
		for _, mode := range []string{"snapshot", "symbols", "edges"} {
			for _, extra := range [][]string{nil, {"--no-cache"}, {"--relation", "CALLS"}} {
				if mode == "symbols" && len(extra) == 2 {
					continue
				}
				args := append([]string{mode, "--repo", repo, "--commit", target}, extra...)
				stream, err := runExactStream(t, args...)
				if err == nil || len(stream) != 0 {
					t.Fatalf("%v read something: %v stdout=%q", args, err, stream)
				}
			}
		}
	}
	// This path builds; the prepared-only read contract does not apply here.
	if stream, err := runExactStream(t, "snapshot", "--repo", repo, "--prepared-only", "--commit", old); err == nil || len(stream) != 0 {
		t.Fatalf("--prepared-only accepted on a bulk stream: %v", err)
	}
	if !sem.Capabilities().OptionalLocalOnlyFeatures["exact_commit_bulk_streams"] || !sem.Capabilities().OptionalLocalOnlyFeatures["source_omission_warnings"] {
		t.Fatal("exact bulk stream capabilities absent")
	}
}

// TestBulkStreamExactCommitCacheKey: the records cache is keyed on the target,
// not on HEAD. A second read of one commit is a hit wherever HEAD has moved, a
// different commit is a miss, and HEAD and --commit <HEAD> share one entry
// because their bytes are identical.
func TestBulkStreamExactCommitCacheKey(t *testing.T) {
	repo, old, head := exactCommitStreamFixture(t)
	cache := t.TempDir()
	stream := func(extra ...string) ([]byte, sem.StructuralWorkStats) {
		t.Helper()
		out, err := runExactStream(t, append([]string{"snapshot", "--repo", repo, "--cache-dir", cache}, extra...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out, sem.StructuralWorkSnapshot()
	}
	parsed := func(before, after sem.StructuralWorkStats) bool { return before != after }
	start := sem.StructuralWorkSnapshot()
	cold, afterCold := stream("--commit", old)
	if !parsed(start, afterCold) {
		t.Fatal("first read of the commit did no work")
	}
	write(t, repo, "later.ts", "export function later() { return 4; }\n")
	git(t, repo, "add", "later.ts")
	git(t, repo, "commit", "-m", "HEAD moves on")
	warm, afterWarm := stream("--commit", old)
	if parsed(afterCold, afterWarm) || !bytes.Equal(cold, warm) {
		t.Fatal("second read of the same commit missed the cache after HEAD moved")
	}
	_, afterOther := stream("--commit", head)
	if !parsed(afterWarm, afterOther) {
		t.Fatal("a different commit hit the old commit's entry")
	}
	current := rev(t, repo, "HEAD^{commit}")
	plain, err := runExactStream(t, "snapshot", "--repo", repo, "--no-cache")
	if err != nil {
		t.Fatal(err)
	}
	exact, err := runExactStream(t, "snapshot", "--repo", repo, "--no-cache", "--commit", current)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, exact) {
		t.Fatal("HEAD and --commit <HEAD> differ, so they must not share a cache entry")
	}
	_, afterExact := stream("--commit", current)
	shared, afterPlain := stream()
	if parsed(afterExact, afterPlain) || !bytes.Equal(shared, plain) {
		t.Fatal("HEAD did not reuse the identical --commit <HEAD> entry")
	}
}

// TestBulkStreamExactCommitWithoutAnyCheckout: the same read from a bare clone
// and a --no-checkout clone, where there is no working tree to fall back to.
// Reason: neighbors --commit works there, and the bulk streams promise the same.
func TestBulkStreamExactCommitWithoutAnyCheckout(t *testing.T) {
	source, old, _ := exactCommitStreamFixture(t)
	parent := t.TempDir()
	git(t, parent, "clone", "--quiet", "--bare", source, "bare.git")
	git(t, parent, "clone", "--quiet", "--no-checkout", source, "nocheckout")
	for _, clone := range []string{"bare.git", "nocheckout"} {
		stream, err := runExactStream(t, "symbols", "--repo", filepath.Join(parent, clone), "--no-cache", "--commit", old)
		if err != nil {
			t.Fatalf("%s: %v", clone, err)
		}
		header, symbols, _ := exactStreamHeader(t, stream)
		if header.Commit != old || !symbols["legacyOnly"] || symbols["current"] {
			t.Fatalf("%s read commit=%s symbols=%v", clone, header.Commit, symbols)
		}
	}
}

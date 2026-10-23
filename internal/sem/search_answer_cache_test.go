package sem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/textindex"
)

// The search wires the tree scan to the cache directory's remembered answers:
// a repeat of a prepared alias search reads no blob for its preselection,
// reports its hits, and returns the same results.
func TestPreparedAliasSearchRemembersScanAnswers(t *testing.T) {
	if gitutil.TreeScanRoute != gitutil.TreeScanRouteOnePass {
		t.Skip("this build scans through Git, which remembers nothing")
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth/session.go", "package auth\n// auth session token\nfunc NewAuthSession() int { return 1 }\n")
	for index := 0; index < 10; index++ {
		write(t, repo, fmt.Sprintf("noise/file_%02d.go", index), fmt.Sprintf("package noise\nfunc Noise%d() int { return %d }\n", index, index))
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	cacheDir := t.TempDir()
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cacheDir); err != nil {
		t.Fatal(err)
	}
	const queryText = "authentication session"
	search := func() SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", queryText, SearchOptions{
			Profile: ProfileSyntaxOnly, TopK: 10, MaxIndexedFiles: 1, CacheDir: cacheDir, PreparedOnly: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := search()
	if first.Stats.PreselectionBackend != gitutil.TreeScanRouteOnePass || first.Stats.AnswerCacheMisses == 0 || first.Stats.AnswerCacheHits != 0 {
		t.Fatalf("first search did not compute and remember: %+v", first.Stats)
	}
	second := search()
	if second.Stats.AnswerCacheHits != first.Stats.AnswerCacheMisses || second.Stats.AnswerCacheMisses != 0 || second.Stats.PreselectionBlobsRead != 0 {
		t.Fatalf("repeat search did not answer from memory: %+v", second.Stats)
	}
	if !reflect.DeepEqual(first.Results, second.Results) {
		t.Fatal("remembered answers changed the results")
	}
}

// The search wires the tree scan to the cache directory's text index: once
// the base is indexed, a prepared alias search whose patterns all have
// literals reports the index's route and lookups, and returns the results it
// returned before the index existed.
func TestPreparedAliasSearchUsesTheTextIndex(t *testing.T) {
	if gitutil.TreeScanRoute != gitutil.TreeScanRouteOnePass {
		t.Skip("this build scans through Git")
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
	write(t, repo, "auth/session.go", "package auth\n// auth session token\nfunc NewAuthSession() int { return 1 }\n")
	for index := 0; index < 10; index++ {
		write(t, repo, fmt.Sprintf("noise/file_%02d.go", index), fmt.Sprintf("package noise\nfunc Noise%d() int { return %d }\n", index, index))
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	cacheDir := t.TempDir()
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cacheDir); err != nil {
		t.Fatal(err)
	}
	search := func() SearchResponse {
		t.Helper()
		response, err := SearchRepository(t.Context(), repo, "test", "authentication session", SearchOptions{
			Profile: ProfileSyntaxOnly, TopK: 10, MaxIndexedFiles: 1, CacheDir: cacheDir, PreparedOnly: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	before := search()
	if before.Stats.PreselectionFallbackReason != "index-absent" {
		t.Fatalf("search before indexing = %+v, want the index reported absent", before.Stats)
	}
	if err := os.RemoveAll(filepath.Join(cacheDir, "tree-scan-answers")); err != nil {
		t.Fatal(err) // so the next search asks the index rather than its own answers
	}
	oids, err := gitutil.ListTreeBlobs(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := textindex.Open(cacheDir, 0).Build(t.Context(), oids, func(ctx context.Context, oids []string, visit func(string, []byte) error) error {
		return gitutil.ReadBlobs(ctx, repo, oids, visit)
	}); err != nil {
		t.Fatal(err)
	}
	after := search()
	if after.Stats.PreselectionBackend != gitutil.TreeScanRouteTextIndex || after.Stats.TextIndexLookups == 0 || after.Stats.TextIndexCandidates == 0 {
		t.Fatalf("search after indexing = %+v, want the text-index route", after.Stats)
	}
	if !reflect.DeepEqual(before.Results, after.Results) {
		t.Fatal("the text index changed the results")
	}
}

package sem

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/entireio/entire-graph/internal/gitutil"
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

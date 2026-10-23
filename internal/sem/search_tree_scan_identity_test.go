package sem

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-graph/internal/gitutil"
)

// The one-pass tree scan replaced the per-pattern Git route under answers the
// retrieval benchmark pinned, so it must answer the benchmark's own alias
// searches identically on their real base trees. Those trees are not part of
// this repository, so the check runs only when pointed at them:
//
//	ENTIRE_GRAPH_SCAN_IDENTITY_QUERIES=queries.dev.json,queries.heldout.json
//	ENTIRE_GRAPH_SCAN_IDENTITY_REPOS=neuve=/path/to/neuve,neuve_effect=/path/to/neuve_effect
//
// Each query file is the benchmark's {"queries":[{"repository","base","text"}]}.
func TestTreeScanMatchesGitRouteOnBenchmarkSearches(t *testing.T) {
	queryFiles := os.Getenv("ENTIRE_GRAPH_SCAN_IDENTITY_QUERIES")
	repoSetting := os.Getenv("ENTIRE_GRAPH_SCAN_IDENTITY_REPOS")
	if queryFiles == "" || repoSetting == "" {
		t.Skip("set ENTIRE_GRAPH_SCAN_IDENTITY_QUERIES and ENTIRE_GRAPH_SCAN_IDENTITY_REPOS to check real trees")
	}
	repos := map[string]string{}
	for _, entry := range strings.Split(repoSetting, ",") {
		name, path, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed repository entry %q", entry)
		}
		repos[name] = path
	}
	type query struct {
		Repository string `json:"repository"`
		Base       string `json:"base"`
		Text       string `json:"text"`
	}
	type search struct {
		repo, base string
		patterns   []string
	}
	var searches []search
	seen := map[string]bool{}
	total, aliased := 0, 0
	for _, file := range strings.Split(queryFiles, ",") {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Queries []query `json:"queries"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatal(err)
		}
		for _, entry := range parsed.Queries {
			repo, ok := repos[entry.Repository]
			if !ok {
				t.Fatalf("no path for repository %q", entry.Repository)
			}
			total++
			q := buildSearchQuery(entry.Text)
			if len(q.inferredAbbreviations) == 0 {
				continue // this search never reaches the first-line tree scan
			}
			patterns := searchGitAliasFirstLinePatterns(q, q.terms)
			if patterns == nil {
				continue
			}
			aliased++
			key := entry.Repository + "\x00" + entry.Base + "\x00" + strings.Join(patterns, "\n")
			if seen[key] {
				continue
			}
			seen[key] = true
			searches = append(searches, search{repo: repo, base: entry.Base, patterns: patterns})
		}
	}
	t.Logf("%d queries, %d reach the alias first-line scan, %d distinct by base and patterns", total, aliased, len(searches))
	sort.SliceStable(searches, func(a, b int) bool { return searches[a].base < searches[b].base })
	// ENTIRE_GRAPH_SCAN_IDENTITY_EVERY=n checks every n-th search only.
	if every, err := strconv.Atoi(os.Getenv("ENTIRE_GRAPH_SCAN_IDENTITY_EVERY")); err == nil && every > 1 {
		var sample []search
		for index := 0; index < len(searches); index += every {
			sample = append(sample, searches[index])
		}
		searches = sample
	}
	collect := func(run func(visit func(gitutil.GrepMatch) error) error) ([]gitutil.GrepMatch, time.Duration, error) {
		var out []gitutil.GrepMatch
		started := time.Now()
		err := run(func(match gitutil.GrepMatch) error {
			out = append(out, match)
			return nil
		})
		return out, time.Since(started), err
	}
	answers := gitutil.OpenTreeScanAnswers(t.TempDir())
	for index, s := range searches {
		reference, gitTime, err := collect(func(visit func(gitutil.GrepMatch) error) error {
			return gitutil.GrepTreePatternLinesGit(t.Context(), s.repo, s.base, s.patterns, visit)
		})
		if err != nil {
			t.Fatalf("search %d at %s: Git route: %v", index, s.base, err)
		}
		var stats gitutil.TreeScanStats
		got, scanTime, err := collect(func(visit func(gitutil.GrepMatch) error) error {
			var scanErr error
			stats, scanErr = gitutil.ScanTreePatternLines(t.Context(), s.repo, s.base, s.patterns, visit)
			return scanErr
		})
		if err != nil {
			t.Fatalf("search %d at %s: tree scan: %v", index, s.base, err)
		}
		if !reflect.DeepEqual(got, reference) {
			t.Fatalf("search %d at %s (%d patterns) differs: %d matches against the Git route's %d", index, s.base, len(s.patterns), len(got), len(reference))
		}
		// The same search through one store shared by every search of the
		// run, as hunks at one base share it in a review.
		var remembered gitutil.TreeScanStats
		viaStore, storeTime, err := collect(func(visit func(gitutil.GrepMatch) error) error {
			var scanErr error
			remembered, scanErr = gitutil.ScanTreePatternLinesWith(t.Context(), s.repo, s.base, s.patterns, gitutil.TreeScanOptions{Answers: answers}, visit)
			return scanErr
		})
		if err != nil {
			t.Fatalf("search %d at %s: tree scan with remembered answers: %v", index, s.base, err)
		}
		if !reflect.DeepEqual(viaStore, reference) {
			t.Fatalf("search %d at %s differs with remembered answers: %d matches against the Git route's %d", index, s.base, len(viaStore), len(reference))
		}
		t.Logf("search %d at %.12s: %d patterns, %d files matched, identical (route %s, %d blobs, %d bytes; Git route %s, tree scan %s; with remembered answers %s, %d hits, %d misses, %d blobs read)",
			index, s.base, len(s.patterns), len(got), stats.Route, stats.BlobsRead, stats.BytesRead, gitTime.Round(time.Millisecond), scanTime.Round(time.Millisecond),
			storeTime.Round(time.Millisecond), remembered.AnswerHits, remembered.AnswerMisses, remembered.BlobsRead)
	}
	t.Logf("%d distinct alias searches identical", len(searches))
}

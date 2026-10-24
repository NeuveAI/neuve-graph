//go:build !cgo || windows

package gitutil

import "context"

// TreeScanRoute is the route ScanTreePatternLines takes in this build.
const TreeScanRoute = TreeScanRouteGitGrep

// The Git route remembers nothing; the identity only keeps the store's
// layout the same on every build.
func treeScanEngineIdentity() string { return treeScanEngineVersion + "\x00git-route" }

// Without the platform's POSIX regex engine Graph cannot decide lines the way
// git grep does, so Git scans once per pattern.
func treePaths(ctx context.Context, repo, treeish string, patterns []string, _ TreeScanOptions) ([]string, TreeScanStats, error) {
	paths, err := grepTreePaths(ctx, repo, treeish, patterns, true, false, nil)
	return paths, TreeScanStats{}, err // git's own route, named by the caller
}

func treeFirstPatternLines(ctx context.Context, repo, treeish string, patterns []string, _ TreeScanOptions, visit func(GrepMatch) error) (TreeScanStats, error) {
	distinct := map[string]bool{}
	for _, pattern := range patterns {
		distinct[pattern] = true
	}
	stats := TreeScanStats{Route: TreeScanRouteGitGrep, Patterns: len(distinct)}
	return stats, grepTreeFirstPatternLinesGit(ctx, repo, treeish, patterns, visit)
}

//go:build !cgo || windows

package gitutil

import "context"

// TreeScanRoute is the route ScanTreePatternLines takes in this build.
const TreeScanRoute = TreeScanRouteGitGrep

// Without the platform's POSIX regex engine Graph cannot decide lines the way
// git grep does, so Git scans once per pattern.
func treeFirstPatternLines(ctx context.Context, repo, treeish string, patterns []string, visit func(GrepMatch) error) (TreeScanStats, error) {
	distinct := map[string]bool{}
	for _, pattern := range patterns {
		distinct[pattern] = true
	}
	stats := TreeScanStats{Route: TreeScanRouteGitGrep, Patterns: len(distinct)}
	return stats, grepTreeFirstPatternLinesGit(ctx, repo, treeish, patterns, visit)
}

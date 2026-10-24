//go:build cgo && !windows

package gitutil

import "context"

// TreeScanRoute is the route ScanTreePatternLines takes in this build.
const TreeScanRoute = TreeScanRouteOnePass

func treePaths(ctx context.Context, repo, treeish string, patterns []string, options TreeScanOptions) ([]string, TreeScanStats, error) {
	return scanTreePaths(ctx, repo, treeish, patterns, options)
}

func treeFirstPatternLines(ctx context.Context, repo, treeish string, patterns []string, options TreeScanOptions, visit func(GrepMatch) error) (TreeScanStats, error) {
	return scanTreeFirstPatternLines(ctx, repo, treeish, patterns, options, visit)
}

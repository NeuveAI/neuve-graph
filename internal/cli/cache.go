package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/termsafe"
)

// cacheResponse is the `cache` command's JSON receipt.
type cacheResponse struct {
	FormatVersion int             `json:"format_version"`
	Action        string          `json:"action"`
	Declared      *cacheDeclaredV `json:"declared,omitempty"`
	Cache         sem.CacheUsage  `json:"cache"`
}

type cacheDeclaredV struct {
	Commits []string `json:"commits"`
	Trees   []string `json:"trees"`
}

// runCache keeps a cache directory within its bounds: `declare` replaces the
// bases it always keeps, `prune` removes what it may until within its cap and
// free-space floor, and `usage` reports its size.
func runCache(ctx context.Context, opts Options, args []string) error {
	if len(args) == 0 || (args[0] != "declare" && args[0] != "prune" && args[0] != "usage") {
		return errors.New("cache requires an action: declare, prune or usage")
	}
	action := args[0]
	var repoFlag, cacheFlag, format string
	var commits []string
	limits := sem.CacheLimitsFromEnv()
	var rest []string
	for index := 1; index < len(args); index++ {
		switch args[index] {
		case "--repo", "--commit", "--cache-dir", "--format", "--max-bytes", "--min-free-bytes":
			value, next, err := searchFlagValue(args, index)
			if err != nil {
				return err
			}
			switch args[index] {
			case "--repo":
				repoFlag = value
			case "--commit":
				commits = append(commits, value)
			case "--cache-dir":
				cacheFlag = value
			case "--format":
				format = value
			case "--max-bytes", "--min-free-bytes":
				parsed, err := strconv.ParseInt(value, 10, 64)
				if err != nil || parsed < 0 || (args[index] == "--max-bytes" && parsed == 0) {
					return fmt.Errorf("cache %s must be a byte count", args[index])
				}
				if args[index] == "--max-bytes" {
					limits.MaxBytes = parsed
				} else {
					limits.MinFreeBytes = parsed
				}
			}
			index = next
		default:
			rest = append(rest, args[index])
		}
	}
	if len(rest) != 0 {
		return unexpectedArgumentsError("cache", opts.Version, rest)
	}
	if format != "" && format != "json" && format != "text" {
		return fmt.Errorf("cache --format must be json or text, got %q", format)
	}
	if action != "declare" && (len(commits) != 0 || repoFlag != "") {
		return errors.New("cache --commit and --repo belong to cache declare")
	}
	cacheDir := resolveCacheDir(cacheFlag, opts.Env.PluginDataDir)
	if cacheDir == "" {
		return errors.New("cache requires --cache-dir or ENTIRE_PLUGIN_DATA_DIR: this platform has no resolvable per-user cache directory")
	}
	response := cacheResponse{FormatVersion: 1, Action: action}
	var err error
	switch action {
	case "declare":
		var declared cacheDeclaredV
		declared.Commits, declared.Trees, err = declaredBases(ctx, opts, repoFlag, commits)
		if err != nil {
			return err
		}
		if err := sem.DeclareCacheBases(cacheDir, declared.Commits, declared.Trees); err != nil {
			return err
		}
		response.Declared = &declared
		response.Cache, err = sem.MeasureCache(cacheDir, limits)
	case "prune":
		response.Cache, err = sem.PruneCache(cacheDir, limits)
	case "usage":
		response.Cache, err = sem.MeasureCache(cacheDir, limits)
	}
	if err != nil {
		return err
	}
	if format == "text" {
		return writeCacheText(opts.Stdout, response)
	}
	encoder := json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout))
	encoder.SetEscapeHTML(false)
	return encoder.Encode(response)
}

// declaredBases checks the declared commits and, with --repo, adds the tree
// of each, so a base keeps the artifacts prepared at another commit of the
// same tree.
func declaredBases(ctx context.Context, opts Options, repoFlag string, commits []string) ([]string, []string, error) {
	for _, commit := range commits {
		if (len(commit) != 40 && len(commit) != 64) || !isLowerHex(commit) {
			return nil, nil, fmt.Errorf("cache declare --commit %q is not an exact lowercase commit OID", commit)
		}
	}
	if repoFlag == "" {
		return commits, nil, nil
	}
	repo, err := resolveRepo(ctx, opts.Env, repoFlag)
	if err != nil {
		return nil, nil, err
	}
	trees := make([]string, 0, len(commits))
	for _, commit := range commits {
		tree, err := gitutil.RevParse(ctx, repo, commit+"^{tree}")
		if err != nil {
			return nil, nil, fmt.Errorf("cache declare: the tree of %s: %w", commit, err)
		}
		trees = append(trees, tree)
	}
	return commits, trees, nil
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func writeCacheText(out io.Writer, response cacheResponse) error {
	usage := response.Cache
	free := "unknown"
	if usage.FreeBytes >= 0 {
		free = formatBytes(usage.FreeBytes)
	}
	if _, err := fmt.Fprintf(out, "Cache %s of %s cap; %s free (floor %s); %s declared\n", formatBytes(usage.Bytes), formatBytes(usage.CapBytes), free, formatBytes(usage.MinFreeBytes), formatBytes(usage.DeclaredBytes)); err != nil {
		return err
	}
	if response.Action == "prune" {
		if _, err := fmt.Fprintf(out, "Removed %d artifacts, %s; kept %s held by reads\n", usage.EvictedEntries, formatBytes(usage.EvictedBytes), formatBytes(usage.PinnedBytes)); err != nil {
			return err
		}
	}
	families := make([]string, 0, len(usage.Families))
	for family := range usage.Families {
		families = append(families, family)
	}
	sort.Strings(families)
	for _, family := range families {
		if _, err := fmt.Fprintf(out, "  %-20q %s\n", family, formatBytes(usage.Families[family])); err != nil {
			return err
		}
	}
	return nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

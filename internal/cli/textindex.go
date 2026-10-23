package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/termsafe"
	"github.com/entireio/entire-graph/internal/textindex"
)

// textIndexResponse is the text-index command's JSON receipt.
type textIndexResponse struct {
	FormatVersion int                    `json:"format_version"`
	Commit        string                 `json:"commit"`
	TextIndex     textindex.BuildReceipt `json:"text_index"`
}

// runTextIndex indexes the blobs of one exact commit's tree that no shard in
// the cache directory holds yet, as commit-time background work, so later
// tree scans over that tree read only the files the index cannot rule out.
func runTextIndex(ctx context.Context, opts Options, args []string) error {
	var repoFlag, commit, cacheFlag, format string
	var maxBytes int64
	var rest []string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--repo", "--commit", "--cache-dir", "--format", "--max-bytes":
			value, next, err := searchFlagValue(args, index)
			if err != nil {
				return err
			}
			switch args[index] {
			case "--repo":
				repoFlag = value
			case "--commit":
				commit = value
			case "--cache-dir":
				cacheFlag = value
			case "--format":
				format = value
			case "--max-bytes":
				parsed, err := strconv.ParseInt(value, 10, 64)
				if err != nil || parsed <= 0 {
					return errors.New("text-index --max-bytes must be a positive byte count")
				}
				maxBytes = parsed
			}
			index = next
		default:
			rest = append(rest, args[index])
		}
	}
	if len(rest) != 0 {
		return unexpectedArgumentsError("text-index", opts.Version, rest)
	}
	if format != "" && format != "json" && format != "text" {
		return fmt.Errorf("text-index --format must be json or text, got %q", format)
	}
	if (len(commit) != 40 && len(commit) != 64) || strings.Trim(commit, "0123456789abcdef") != "" {
		return errors.New("text-index requires --commit with an exact lowercase commit OID")
	}
	repo, err := resolveRepo(ctx, opts.Env, repoFlag)
	if err != nil {
		return err
	}
	cacheDir := resolveCacheDir(cacheFlag, opts.Env.PluginDataDir)
	if cacheDir == "" {
		return errors.New("text-index requires --cache-dir or ENTIRE_PLUGIN_DATA_DIR: this platform has no resolvable per-user cache directory")
	}
	oids, err := gitutil.ListTreeBlobs(ctx, repo, commit)
	if err != nil {
		return err
	}
	receipt, err := textindex.Open(cacheDir, maxBytes).Build(ctx, oids, func(ctx context.Context, oids []string, visit func(string, []byte) error) error {
		return gitutil.ReadBlobs(ctx, repo, oids, visit)
	})
	if err != nil {
		return err
	}
	if format == "text" {
		_, err := fmt.Fprintf(opts.Stdout, "Text index for %s: %d blobs indexed (%d bytes), %d not indexable, %d bytes for this tree, %d bytes on disk of %d, %d evicted\n",
			commit, receipt.BlobsIndexed, receipt.BytesIndexed, receipt.BlobsSkipped, receipt.TreeIndexBytes, receipt.IndexBytesOnDisk, receipt.CapBytes, receipt.EvictedBytes)
		return err
	}
	encoder := json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout))
	encoder.SetEscapeHTML(false)
	return encoder.Encode(textIndexResponse{FormatVersion: 1, Commit: commit, TextIndex: receipt})
}

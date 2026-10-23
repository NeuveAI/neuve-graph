//go:build windows

// Package textindex is Graph's text index. Zoekt does not build for Windows,
// so there the index is never available: builds report it, and searches
// keep scanning every blob.
package textindex

import (
	"context"
	"errors"
)

const DefaultMaxBytes = 512 << 20

const MaxBytesEnv = "ENTIRE_GRAPH_TEXT_INDEX_MAX_BYTES"

// ErrUnavailable reports that this build has no text index.
var ErrUnavailable = errors.New("the text index is not available on Windows")

type Store struct{}

func Open(string, int64) *Store { return nil }

type BuildReceipt struct {
	TreeBlobs          int   `json:"tree_blobs"`
	TreeIndexBytes     int64 `json:"tree_index_bytes"`
	TreeUnindexedBlobs int   `json:"tree_unindexed_blobs"`
	BlobsIndexed       int   `json:"blobs_indexed"`
	BlobsSkipped       int   `json:"blobs_skipped"`
	BytesIndexed       int64 `json:"bytes_indexed"`
	BuildMS            int64 `json:"build_ms"`
	IndexBytesOnDisk   int64 `json:"index_bytes_on_disk"`
	EvictedBytes       int64 `json:"evicted_bytes"`
	CapBytes           int64 `json:"cap_bytes"`
	Shards             int   `json:"shards"`
}

type BlobReader func(ctx context.Context, oids []string, visit func(oid string, data []byte) error) error

type LookupStats struct {
	Lookups    int
	Failed     int
	Candidates int
	Uncovered  int
}

func (s *Store) Build(context.Context, []string, BlobReader) (BuildReceipt, error) {
	return BuildReceipt{}, ErrUnavailable
}

func (s *Store) Candidates(context.Context, []string, []string) (map[string]bool, LookupStats, error) {
	return nil, LookupStats{}, ErrUnavailable
}

func (s *Store) ScanCandidates(context.Context, []string, []string) (map[string]bool, int, error) {
	return nil, 0, ErrUnavailable
}

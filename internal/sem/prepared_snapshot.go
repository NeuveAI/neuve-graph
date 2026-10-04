package sem

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync/atomic"
)

// Process counters let native tests observe the actual structural entry points
// across a prepared read. They are not per-query usage or approval evidence.
var structuralWorkCounts struct {
	extractions atomic.Uint64
	parses      atomic.Uint64
	builds      atomic.Uint64
	resolutions atomic.Uint64
}

type StructuralWorkStats struct{ Extractions, Parses, Builds, Resolutions uint64 }

// StructuralWorkSnapshot observes cumulative process work. Tests and native
// benchmarks must isolate their interval; concurrent work is not attributed.
func StructuralWorkSnapshot() StructuralWorkStats {
	return StructuralWorkStats{structuralWorkCounts.extractions.Load(), structuralWorkCounts.parses.Load(), structuralWorkCounts.builds.Load(), structuralWorkCounts.resolutions.Load()}
}

// PreparedSnapshotError identifies a retained-read failure. None of these
// outcomes permits parsing, relation resolution, or a replacement build.
type PreparedSnapshotFailureReason string

type PreparedSnapshotError struct {
	Reason PreparedSnapshotFailureReason
	Cause  error
}

func (e *PreparedSnapshotError) Error() string {
	return "prepared snapshot unavailable: " + string(e.Reason)
}

func (e *PreparedSnapshotError) Unwrap() error { return e.Cause }

// LoadPreparedProviderSnapshot reads the existing complete artifact for the
// exact committed source and producer configuration. Unlike optional cache
// loaders it never degrades a missing or invalid artifact into a build.
func LoadPreparedProviderSnapshot(ctx context.Context, repo, providerVersion string, options ProviderSnapshotOptions, cacheDir string) (ProviderSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return ProviderSnapshot{}, err
	}
	fail := func(reason PreparedSnapshotFailureReason, cause error) (ProviderSnapshot, error) {
		return ProviderSnapshot{}, &PreparedSnapshotError{Reason: reason, Cause: cause}
	}
	if options.Worktree || options.ForceRebuild || len(options.OnlyFiles) > 0 || cacheDir == "" {
		return fail("incompatible-read-options", nil)
	}
	if options.Profile == "" {
		options.Profile = ProfileFull
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return fail("source-unavailable", err)
	}
	options, err = ensureProviderCachePolicy(absRepo, options)
	if err != nil {
		return fail("policy-unavailable", err)
	}
	options, err = captureSearchCacheSource(ctx, absRepo, options)
	if err != nil {
		return fail("source-unavailable", err)
	}
	commit, tree, err := resolveCommittedHEAD(ctx, absRepo)
	if err != nil {
		if ctx.Err() != nil {
			return ProviderSnapshot{}, ctx.Err()
		}
		return fail("source-unavailable", err)
	}
	repositoryKey := repoKey(ctx, absRepo)
	key, err := searchSnapshotKey(absRepo, repositoryKey, providerVersion, tree, options)
	if err != nil {
		return fail("policy-unavailable", err)
	}
	entry, err := newCacheEntry(cacheDir, "search", searchSnapshotCacheVersion, key)
	if err != nil {
		return fail("artifact-unavailable", err)
	}
	cached, err := readSearchSnapshot(entry)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fail("missing", err)
		}
		return fail("invalid-artifact", err)
	}
	if !validCachedSearchSnapshot(cached, repositoryKey, providerVersion, tree, options) || !validCachedSearchSource(cached, options) || cached.DerivedFrom != "" {
		return fail("incompatible-artifact", nil)
	}
	// Complete full-profile artifacts contain history-derived co-change facts.
	// A tree match alone cannot establish freshness after a same-tree rebase.
	// Stage A requires the preparation commit exactly, including fast profiles,
	// until tree-derived and history-derived artifacts have separate identities.
	if cached.Commit != commit || cached.Snapshot.Header.Commit != commit {
		return fail("revision-not-prepared", nil)
	}
	if err := ctx.Err(); err != nil {
		return ProviderSnapshot{}, err
	}
	cached = rebindCachedSearchSnapshot(cached, absRepo, commit)
	if cached.Snapshot.Header.Tree != tree {
		return fail("incompatible-artifact", fmt.Errorf("prepared tree mismatch"))
	}
	return cached.Snapshot, nil
}

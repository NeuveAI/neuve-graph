package sem

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
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

type preparedReadTarget struct{ commit, tree string }

// PreparedReadReceipt qualifies a read under the provider's existing artifact
// family. Policy IDs are opaque; they do not recover historical invocation policy.
type PreparedReadReceipt struct {
	ArtifactFamily       string      `json:"artifact_family"`
	GenerationID         string      `json:"generation_id,omitempty"`
	PolicyID             string      `json:"policy_id"`
	ProducerBuildID      string      `json:"producer_build_id"`
	Source               cacheSource `json:"source"`
	ExternalPolicyOrigin string      `json:"external_policy_origin"`
	GitignoreOrigin      string      `json:"gitignore_origin"`
}

type PreparedProviderRead struct {
	Snapshot ProviderSnapshot
	Receipt  PreparedReadReceipt
	options  ProviderSnapshotOptions
}

// LoadPreparedProviderSnapshot reads the existing complete artifact for the
// exact committed source and producer configuration. Unlike optional cache
// loaders it never degrades a missing or invalid artifact into a build.
func LoadPreparedProviderSnapshot(ctx context.Context, repo, providerVersion string, options ProviderSnapshotOptions, cacheDir string) (ProviderSnapshot, error) {
	read, err := ReadPreparedProviderSnapshot(ctx, repo, providerVersion, options, cacheDir)
	return read.Snapshot, err
}

// ReadPreparedProviderSnapshot returns facts and their qualified retained-read
// receipt without indexing, resolving, repairing or changing the source view.
func ReadPreparedProviderSnapshot(ctx context.Context, repo, providerVersion string, options ProviderSnapshotOptions, cacheDir string) (PreparedProviderRead, error) {
	if err := ctx.Err(); err != nil {
		return PreparedProviderRead{}, err
	}
	fail := func(reason PreparedSnapshotFailureReason, cause error) (PreparedProviderRead, error) {
		return PreparedProviderRead{}, &PreparedSnapshotError{Reason: reason, Cause: cause}
	}
	if options.Worktree || options.ForceRebuild || len(options.OnlyFiles) > 0 || cacheDir == "" {
		return fail("incompatible-read-options", nil)
	}
	if target := options.PreparedCommit; target != "" && ((len(target) != 40 && len(target) != 64) || strings.Trim(target, "0123456789abcdef") != "") {
		return fail("invalid-target", nil)
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
	// One walk of the repository's Git metadata for this read; each later
	// check before a Git command takes its answer only while what it looked
	// at stands as it was, and walks again otherwise.
	ctx = withReusableGitMetadataValidation(ctx, absRepo)
	options, err = captureSearchCacheSource(ctx, absRepo, options)
	if err != nil {
		return fail("source-unavailable", err)
	}
	var commit, tree string
	if options.PreparedCommit != "" {
		commit, tree, err = resolvePreparedTarget(ctx, absRepo, options.PreparedCommit)
		options.preparedReadTarget = &preparedReadTarget{commit: commit, tree: tree}
	} else {
		commit, tree, err = resolveCommittedHEAD(ctx, absRepo)
	}
	if err != nil {
		if ctx.Err() != nil {
			return PreparedProviderRead{}, ctx.Err()
		}
		return fail("source-unavailable", err)
	}
	repositoryKey := repoKey(ctx, absRepo)
	build, err := ExecutableBuildIdentity()
	if err != nil {
		return fail("source-unavailable", err)
	}
	receipt := PreparedReadReceipt{ProducerBuildID: build, Source: *options.cacheSource, ExternalPolicyOrigin: "serving-directory-captured", GitignoreOrigin: "target-commit"}
	if options.Profile == ProfileSyntaxOnly {
		binding, err := bindPreparedGeneration(absRepo, repositoryKey, providerVersion, commit, tree, options, cacheDir)
		if err != nil {
			return fail("policy-unavailable", err)
		}
		generation, err := binding.read(ctx)
		if err == nil {
			receipt.ArtifactFamily, receipt.GenerationID, receipt.PolicyID = "syntax-generation", generation.ID, binding.policy
			return PreparedProviderRead{Snapshot: generation.Snapshot, Receipt: receipt, options: options}, nil
		}
		if ctx.Err() != nil {
			return PreparedProviderRead{}, ctx.Err()
		}
		// Only absence permits reading the compatible legacy syntax cache.
		// A present invalid generation cannot be masked or repaired by a query.
		if !errors.Is(err, fs.ErrNotExist) {
			return fail("invalid-artifact", err)
		}
	}
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
		return PreparedProviderRead{}, err
	}
	cached = rebindCachedSearchSnapshot(cached, absRepo, commit)
	if cached.Snapshot.Header.Tree != tree {
		return fail("incompatible-artifact", fmt.Errorf("prepared tree mismatch"))
	}
	receipt.ArtifactFamily = "search-snapshot"
	receipt.PolicyID, err = searchSnapshotKey(absRepo, repositoryKey, providerVersion, "", options)
	if err != nil {
		return fail("policy-unavailable", err)
	}
	return PreparedProviderRead{Snapshot: cached.Snapshot, Receipt: receipt, options: options}, nil
}

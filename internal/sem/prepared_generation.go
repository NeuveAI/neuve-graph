package sem

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/build"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-graph/internal/gitutil"
)

const preparedGenerationVersion = "prepared-generation-v1-" + IdentityRevision

type IncrementalPreparationError struct {
	Reason string
	Cause  error
}

func (err *IncrementalPreparationError) Error() string {
	return "incremental preparation unavailable: " + err.Reason
}
func (err *IncrementalPreparationError) Unwrap() error { return err.Cause }

type PreparedGeneration struct {
	ID               string
	Commit           string
	Tree             string
	Profile          Profile
	Snapshot         ProviderSnapshot
	Extraction       ExtractionReuseStats
	AlreadyPublished bool
}

type cachedPreparedGeneration struct {
	ID       string               `json:"id"`
	Policy   string               `json:"policy"`
	Digest   string               `json:"sha256"`
	Snapshot cachedSearchSnapshot `json:"snapshot"`
}

type cacheContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader cacheContextReader) Read(bytes []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(bytes)
}

func decodeCompressedJSON(ctx context.Context, reader io.Reader, value any) error {
	compressed, err := gzip.NewReader(cacheContextReader{ctx: ctx, reader: reader})
	if err != nil {
		return err
	}
	defer compressed.Close()
	decoder := json.NewDecoder(cacheContextReader{ctx: ctx, reader: compressed})
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("artifact has trailing data")
	}
	return nil
}

func resolvePreparedTarget(ctx context.Context, repo, target string) (string, string, error) {
	if (len(target) != 40 && len(target) != 64) || strings.Trim(target, "0123456789abcdef") != "" {
		return "", "", &IncrementalPreparationError{Reason: "invalid-target"}
	}
	if !gitMetadataSafeForSubprocessContext(ctx, repo) {
		return "", "", gitMetadataRefusalError(repo)
	}
	commit, err := gitutil.RevParse(ctx, repo, target+"^{commit}")
	if err != nil || commit != target {
		return "", "", &IncrementalPreparationError{Reason: "source-unavailable", Cause: err}
	}
	tree, err := gitutil.RevParse(ctx, repo, commit+"^{tree}")
	return commit, tree, err
}

func preparationPolicy(repo, repositoryKey, providerVersion string, options ProviderSnapshotOptions) (string, error) {
	base, err := searchSnapshotKey(repo, repositoryKey, providerVersion, "", options)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	writeCacheKeyString(hash, "version", preparedGenerationVersion)
	writeCacheKeyString(hash, "snapshot-policy", base)
	// Go's default-build eligibility is producer configuration, not an assumed
	// property of the blob. Changing it must not revive an old exclusion.
	configuration, err := json.Marshal(struct {
		GOOS, GOARCH, Compiler      string
		Cgo                         bool
		Tags, ToolTags, ReleaseTags []string
	}{build.Default.GOOS, build.Default.GOARCH, build.Default.Compiler, build.Default.CgoEnabled, build.Default.BuildTags, build.Default.ToolTags, build.Default.ReleaseTags})
	if err != nil {
		return "", err
	}
	writeCacheKeyField(hash, "host-build", configuration)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func preparedGenerationKey(policy, commit, tree string) string {
	hash := sha256.New()
	writeCacheKeyString(hash, "policy", policy)
	writeCacheKeyString(hash, "commit", commit)
	writeCacheKeyString(hash, "tree", tree)
	return hex.EncodeToString(hash.Sum(nil))
}

func generationDigest(snapshot cachedSearchSnapshot) (string, error) {
	bytes, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(bytes)
	return hex.EncodeToString(digest[:]), nil
}

// PrepareIncrementalProviderSnapshot is the internal Stage B1 extraction
// operation. It admits every provider language at syntax-only depth. Full
// relation/binding generations remain unsupported; no CLI advertises this
// operation as production readiness. The target must be an exact Git commit,
// so a queued job never silently prepares the newer HEAD.
func PrepareIncrementalProviderSnapshot(ctx context.Context, repo, target, providerVersion string, options ProviderSnapshotOptions, cacheDir string) (PreparedGeneration, error) {
	fail := func(reason string, cause error) (PreparedGeneration, error) {
		if err := ctx.Err(); err != nil {
			reason, cause = "cancelled", err
		}
		return PreparedGeneration{}, &IncrementalPreparationError{Reason: reason, Cause: cause}
	}
	if err := ctx.Err(); err != nil {
		return fail("cancelled", err)
	}
	if options.Profile != ProfileSyntaxOnly || options.Worktree || options.ForceRebuild || len(options.OnlyFiles) != 0 || cacheDir == "" {
		return fail("unsupported-preparation", nil)
	}
	if (len(target) != 40 && len(target) != 64) || strings.Trim(target, "0123456789abcdef") != "" {
		return fail("invalid-target", nil)
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return fail("source-unavailable", err)
	}
	options, err = CaptureProviderCachePolicy(absRepo, options)
	if err != nil {
		return fail("policy-unavailable", err)
	}
	options, err = captureSearchCacheSource(ctx, absRepo, options)
	if err != nil {
		return fail("source-unavailable", err)
	}
	commit, tree, err := resolvePreparedTarget(ctx, absRepo, target)
	if err != nil {
		return fail("source-unavailable", err)
	}
	repositoryKey := repoKey(ctx, absRepo)
	policy, err := preparationPolicy(absRepo, repositoryKey, providerVersion, options)
	if err != nil {
		return fail("policy-unavailable", err)
	}
	id := preparedGenerationKey(policy, commit, tree)
	entry, err := newCacheEntry(cacheDir, "generations", preparedGenerationVersion, id)
	if err != nil {
		return fail("artifact-unavailable", err)
	}
	readRetained := func() (PreparedGeneration, error) {
		file, err := entry.open()
		if err != nil {
			return PreparedGeneration{}, err
		}
		var cached cachedPreparedGeneration
		err = decodeCompressedJSON(ctx, file, &cached)
		_ = file.Close()
		digest, digestErr := generationDigest(cached.Snapshot)
		if err != nil || digestErr != nil || cached.ID != id || cached.Policy != policy || cached.Digest != digest || cached.Snapshot.Commit != commit || cached.Snapshot.Snapshot.Header.Commit != commit || !validCachedSearchSnapshot(cached.Snapshot, repositoryKey, providerVersion, tree, options) || !validCachedSearchSource(cached.Snapshot, options) {
			return fail("invalid-generation", err)
		}
		restoreCachedSearchInternals(&cached.Snapshot)
		if err := ctx.Err(); err != nil {
			return fail("cancelled", err)
		}
		return PreparedGeneration{ID: id, Commit: commit, Tree: tree, Profile: options.Profile, Snapshot: rebindCachedSearchSnapshot(cached.Snapshot, absRepo, commit).Snapshot, AlreadyPublished: true}, nil
	}
	retained, readErr := readRetained()
	if readErr == nil {
		return retained, nil
	}
	if !errors.Is(readErr, fs.ErrNotExist) {
		var invalid *IncrementalPreparationError
		if errors.As(readErr, &invalid) {
			return PreparedGeneration{}, readErr
		}
		return fail("artifact-unavailable", readErr)
	}
	cache := newProviderExtractionCache(cacheDir, policy)
	options.committedTarget = commit
	options.extractionReuse = cache
	snapshot, err := BuildProviderSnapshotWithOptions(ctx, absRepo, providerVersion, options)
	if err != nil {
		return fail("preparation-failed", err)
	}
	if snapshot.Header.Commit != commit || snapshot.Header.Tree != tree || snapshot.Header.RepoKey != repositoryKey {
		return fail("source-changed", nil)
	}
	if err := validateBuiltSearchSnapshot(snapshot, repositoryKey, providerVersion, tree, options); err != nil {
		return fail("invalid-generation", err)
	}
	currentSource, err := resolveCacheSource(ctx, absRepo)
	if err != nil || options.cacheSource == nil || currentSource != *options.cacheSource {
		return fail("source-changed", err)
	}
	for _, failure := range snapshot.Header.PartialFailures {
		if failure.Code == "E_FILE_READ" || failure.Code == "E_PARSE_TIMEOUT" {
			return fail("incomplete-extraction", nil)
		}
	}
	cached := newCachedSearchSnapshot(providerVersion, commit, tree, options, snapshot)
	digest, err := generationDigest(cached)
	if err != nil {
		return fail("artifact-unavailable", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("cancelled", err)
	}
	// One atomic artifact is the ready record and complete retained generation.
	// Reusable file facts written before it are never themselves a ready graph.
	if err := entry.writeImmutable(ctx, "generation", cachedPreparedGeneration{ID: id, Policy: policy, Digest: digest, Snapshot: cached}); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// A concurrent publisher won. Read and validate its immutable value;
			// this retry cannot replace it with a later writer's output.
			retained, err := readRetained()
			if err != nil {
				return fail("publication-unconfirmed", err)
			}
			return retained, nil
		}
		return fail("publication-failed", err)
	}
	return PreparedGeneration{ID: id, Commit: commit, Tree: tree, Profile: options.Profile, Snapshot: snapshot, Extraction: cache.stats()}, nil
}

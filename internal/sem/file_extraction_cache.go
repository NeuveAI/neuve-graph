package sem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"sync"
)

const extractionCacheVersion = "file-extraction-v1-" + IdentityRevision

type cachedFileExtraction struct {
	Key                   string                  `json:"key"`
	Path                  string                  `json:"path"`
	Blob                  string                  `json:"blob"`
	File                  *FileRecord             `json:"file,omitempty"`
	Symbols               []SymbolRecord          `json:"symbols"`
	Internals             cachedSnapshotInternals `json:"internals"`
	Language              string                  `json:"language"`
	PrecomputedImports    []string                `json:"imports,omitempty"`
	HasPrecomputedImports bool                    `json:"imports_known"`
	Parsed                bool                    `json:"parsed"`
	Failures              []PartialFailure        `json:"failures,omitempty"`
}

type sealedFileExtraction struct {
	Digest string               `json:"sha256"`
	Facts  cachedFileExtraction `json:"facts"`
}

// These are preparation stages, not query work or evidence of semantic review.
type ExtractionReuseStats struct {
	ReusedPaths      []string
	ExtractedPaths   []string
	UncacheablePaths []string
}

type providerExtractionCache struct {
	directory, policy string
	mu                sync.Mutex
	reused, extracted map[string]bool
	uncacheable       map[string]bool
}

func newProviderExtractionCache(directory, policy string) *providerExtractionCache {
	return &providerExtractionCache{directory: directory, policy: policy, reused: map[string]bool{}, extracted: map[string]bool{}, uncacheable: map[string]bool{}}
}

func (cache *providerExtractionCache) stats() ExtractionReuseStats {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	keys := func(values map[string]bool) []string {
		out := make([]string, 0, len(values))
		for path := range values {
			out = append(out, path)
		}
		sort.Strings(out)
		return out
	}
	return ExtractionReuseStats{keys(cache.reused), keys(cache.extracted), keys(cache.uncacheable)}
}

func extractionDigest(value cachedFileExtraction) (string, error) {
	bytes, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(bytes)
	return hex.EncodeToString(digest[:]), nil
}

func (cache *providerExtractionCache) process(ctx context.Context, sc sourceContext, spec profileSpec, maxParseBytes, index int, path string) providerFileResult {
	fail := func(reason string, cause error) providerFileResult {
		if err := ctx.Err(); err != nil {
			reason, cause = "cancelled", err
		}
		return providerFileResult{index: index, path: path, err: &IncrementalPreparationError{Reason: reason, Cause: cause}}
	}
	if err := ctx.Err(); err != nil {
		return fail("cancelled", err)
	}
	content, readable := sc.read(path)
	blob := ""
	if readable {
		blob = contentHash([]byte(content))
	} else if oversized, ok := sc.oversizeAt(path); ok {
		blob = oversized.Hash
	}
	if blob == "" {
		cache.mu.Lock()
		cache.uncacheable[path] = true
		cache.mu.Unlock()
		return processProviderFileUncached(ctx, sc, spec, maxParseBytes, index, path)
	}
	hash := sha256.New()
	writeCacheKeyString(hash, "version", extractionCacheVersion)
	writeCacheKeyString(hash, "policy", cache.policy)
	writeCacheKeyString(hash, "path", path)
	writeCacheKeyString(hash, "blob-content", blob)
	key := hex.EncodeToString(hash.Sum(nil))
	entry, err := newCacheEntry(cache.directory, "extractions", extractionCacheVersion, key)
	if err != nil {
		return fail("artifact-unavailable", err)
	}
	file, err := entry.open()
	if err == nil {
		var sealed sealedFileExtraction
		err = decodeCompressedJSON(ctx, file, &sealed)
		_ = file.Close()
		if err != nil {
			return fail("invalid-extraction", err)
		}
		digest, digestErr := extractionDigest(sealed.Facts)
		facts := sealed.Facts
		if digestErr != nil || digest != sealed.Digest || facts.Key != key || facts.Path != path || facts.Blob != blob {
			return fail("invalid-extraction", digestErr)
		}
		snapshot := ProviderSnapshot{Symbols: facts.Symbols}
		if facts.File != nil {
			snapshot.Files = []FileRecord{*facts.File}
		}
		restoreCachedSnapshotInternals(&snapshot, facts.Internals)
		cache.mu.Lock()
		if !cache.extracted[path] {
			cache.reused[path] = true
		}
		cache.mu.Unlock()
		result := providerFileResult{index: index, path: path, symbols: snapshot.Symbols, language: facts.Language, precomputedImports: facts.PrecomputedImports, hasPrecomputedImports: facts.HasPrecomputedImports, parsed: facts.Parsed, failures: facts.Failures}
		if len(snapshot.Files) != 0 {
			result.file = &snapshot.Files[0]
		}
		return result
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fail("artifact-unavailable", err)
	}
	result := processProviderFileUncached(ctx, sc, spec, maxParseBytes, index, path)
	if err := ctx.Err(); err != nil {
		return fail("cancelled", err)
	}
	for _, failure := range result.failures {
		if failure.Code == "E_PARSE_TIMEOUT" || failure.Code == "E_FILE_READ" {
			return fail("incomplete-extraction", nil)
		}
	}
	snapshot := ProviderSnapshot{Symbols: result.symbols}
	if result.file != nil {
		snapshot.Files = []FileRecord{*result.file}
	}
	facts := cachedFileExtraction{Key: key, Path: path, Blob: blob, File: result.file, Symbols: result.symbols, Internals: captureCachedSnapshotInternals(snapshot), Language: result.language, PrecomputedImports: result.precomputedImports, HasPrecomputedImports: result.hasPrecomputedImports, Parsed: result.parsed, Failures: result.failures}
	digest, err := extractionDigest(facts)
	if err != nil {
		return fail("artifact-unavailable", err)
	}
	if err := entry.writeImmutable(ctx, "extraction", sealedFileExtraction{Digest: digest, Facts: facts}); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fail("artifact-unavailable", err)
		}
		retained, readErr := entry.open()
		if readErr != nil {
			return fail("artifact-unavailable", readErr)
		}
		var existing sealedFileExtraction
		readErr = decodeCompressedJSON(ctx, retained, &existing)
		_ = retained.Close()
		existingDigest, digestErr := extractionDigest(existing.Facts)
		if readErr != nil || digestErr != nil || existing.Digest != existingDigest || existingDigest != digest {
			return fail("conflicting-extraction", readErr)
		}
	}
	cache.mu.Lock()
	cache.extracted[path] = true
	delete(cache.reused, path)
	cache.mu.Unlock()
	return result
}

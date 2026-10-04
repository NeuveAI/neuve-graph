package sem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPreparedSyntaxGenerationReadsExactFactsWithoutStructuralWork(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	prepared := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	before := StructuralWorkSnapshot()
	actual, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	if err != nil || !reflect.DeepEqual(actual, prepared.Snapshot) {
		t.Fatalf("retained syntax facts mismatch: %v", err)
	}
	for _, question := range []string{"serve", "context", "start", "unmatched question qzxjkv"} {
		response, err := SearchRepository(t.Context(), repo, "test", question, SearchOptions{PreparedOnly: true, Profile: ProfileSyntaxOnly, CacheDir: cache, TopK: 8, MaxContextBytes: 16000})
		if err != nil {
			t.Fatal(err)
		}
		if response.Query != question || response.Commit != prepared.Commit || response.Profile != "syntax-only" || response.Stats.AnalysisScope != "complete-prepared" || !response.Stats.IndexCacheHit {
			t.Fatalf("unqualified syntax read: %#v", response)
		}
		if question == "serve" || question == "context" {
			found := false
			for _, result := range response.Results {
				if result.SymbolName == question {
					found = true
				}
			}
			if !found {
				t.Fatalf("required syntax callable %s missing", question)
			}
		}
	}
	if StructuralWorkSnapshot() != before {
		t.Fatal("syntax generation read rebuilt, parsed or resolved facts")
	}
	for _, relation := range actual.Relations {
		if relation.Type != "DEFINES" && relation.Type != "CONTAINS" {
			t.Fatalf("syntax generation claimed unsupported relation %s", relation.Type)
		}
	}
}

func TestPreparedSyntaxGenerationAdmissionNeverMasksInvalidPresentArtifact(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	opts := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	// A compatible legacy preparation remains usable when no generation exists.
	legacy, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", opts, cache)
	if err != nil {
		t.Fatal(err)
	}
	before := StructuralWorkSnapshot()
	actual, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", opts, cache)
	if err != nil || !reflect.DeepEqual(actual, legacy) || StructuralWorkSnapshot() != before {
		t.Fatalf("legacy syntax read failed: %v", err)
	}
	generation := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	entry, err := newCacheEntry(cache, "generations", preparedGenerationVersion, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, entry.relative), []byte("corrupt retained generation"), 0600); err != nil {
		t.Fatal(err)
	}
	before = StructuralWorkSnapshot()
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", opts, cache)
	var refusal *PreparedSnapshotError
	if !errors.As(err, &refusal) || refusal.Reason != "invalid-artifact" {
		t.Fatalf("present invalid generation masked by legacy cache: %v", err)
	}
	if StructuralWorkSnapshot() != before {
		t.Fatal("invalid generation triggered replacement structural work")
	}
}

func TestPreparedSyntaxGenerationDanglingLinkCannotUseLegacyCache(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", options, cache); err != nil {
		t.Fatal(err)
	}
	generation := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	entry, err := newCacheEntry(cache, "generations", preparedGenerationVersion, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, entry.relative)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-retained-generation.json.gz", path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("present dangling link positive control=%v %v", info, err)
	}
	before := StructuralWorkSnapshot()
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", options, cache)
	requirePreparedFailure(t, err, "invalid-artifact")
	if StructuralWorkSnapshot() != before {
		t.Fatal("dangling generation performed fallback structural work")
	}
	// Genuine entry absence still admits the compatible legacy artifact.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", options, cache); err != nil {
		t.Fatalf("genuine absence rejected compatible legacy preparation: %v", err)
	}
	if StructuralWorkSnapshot() != before {
		t.Fatal("legacy compatibility read performed structural work")
	}
}

func TestPreparedSyntaxGenerationRejectsWrongRevisionProfileAndPolicy(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	before := StructuralWorkSnapshot()
	foreign := filepath.Join(t.TempDir(), "foreign-clone")
	git(t, repo, "clone", "--no-hardlinks", repo, foreign)
	_, foreignErr := LoadPreparedProviderSnapshot(t.Context(), foreign, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	requirePreparedFailure(t, foreignErr, "missing")
	for _, profile := range []Profile{ProfileFast, ProfileFull} {
		_, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: profile}, cache)
		requirePreparedFailure(t, err, "missing")
	}
	_, err := LoadPreparedProviderSnapshot(t.Context(), repo, "other-producer", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	requirePreparedFailure(t, err, "missing")
	write(t, repo, ".graphignore", "lib/state.py\n")
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	requirePreparedFailure(t, err, "missing")
	if err := os.Remove(filepath.Join(repo, ".graphignore")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "--allow-empty", "-m", "new commit same tree")
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	requirePreparedFailure(t, err, "missing")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = LoadPreparedProviderSnapshot(ctx, repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation cause: %v", err)
	}
	if StructuralWorkSnapshot() != before {
		t.Fatal("generation admission failure caused structural work")
	}
}

func TestPreparedSyntaxGenerationCancellationAfterReadNeverBuilds(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	before := StructuralWorkSnapshot()
	_, err := SearchRepository(ctx, repo, "test", "context", SearchOptions{PreparedOnly: true, Profile: ProfileSyntaxOnly, CacheDir: cache, afterPreindexLoad: cancel})
	if !errors.Is(err, context.Canceled) || StructuralWorkSnapshot() != before {
		t.Fatalf("cancelled retained read performed work or lost cancellation cause: %v", err)
	}
}

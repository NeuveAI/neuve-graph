package sem

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHistoricalPreparedReadCapturesSourcePolicyAndRevision(t *testing.T) {
	repo, cache := preparedFixture(t), t.TempDir()
	write(t, repo, ".gitignore", "node_modules/*\n!node_modules/pkg/\n!node_modules/pkg/keep.py\n")
	write(t, repo, "node_modules/pkg/keep.py", "def historical_vendor():\n    return 'old'\n")
	git(t, repo, "add", "-f", ".")
	git(t, repo, "commit", "-m", "historical ignore exception")
	old := incrementalHead(t, repo)
	generation := prepareIncremental(t, repo, old, cache)
	write(t, repo, ".gitignore", "")
	write(t, repo, "node_modules/pkg/keep.py", "def current_vendor():\n    return 'new'\n")
	git(t, repo, "add", "-f", ".")
	git(t, repo, "commit", "-m", "new source and policy lineage")
	head := incrementalHead(t, repo)
	before := StructuralWorkSnapshot()
	options := SearchOptions{PreparedOnly: true, PreparedCommit: old, CacheDir: cache, Profile: ProfileSyntaxOnly}
	options.afterPreindexLoad = func() { git(t, repo, "commit", "--allow-empty", "-m", "HEAD moves during exact read") }
	response, err := SearchRepository(t.Context(), repo, "test", "historical_vendor", options)
	if err != nil {
		t.Fatal(err)
	}
	if response.Commit != old || response.Tree != generation.Tree || response.Profile != "syntax-only" || response.PreparedRead == nil || response.PreparedRead.GenerationID != generation.ID || len(response.Results) == 0 || response.Results[0].FilePath != "node_modules/pkg/keep.py" || !strings.Contains(response.Results[0].Snippet, "historical_vendor") || strings.Contains(response.Results[0].Snippet, "current_vendor") {
		t.Fatalf("mixed historical policy/source=%#v", response)
	}
	if incrementalHead(t, repo) == head {
		t.Fatal("moving HEAD control did not fire")
	}
	options.afterPreindexLoad = nil
	// External policy remains current serving-directory capture; it is not
	// recovered from the old commit. Changing it selects another preparation.
	write(t, repo, ".graphignore", "src/**\n")
	_, err = SearchRepository(t.Context(), repo, "test", "historical_vendor", options)
	requirePreparedFailure(t, err, "missing")
	if StructuralWorkSnapshot() != before {
		t.Fatal("historical policy read/refusal performed structural work")
	}
}

func TestHistoricalPreparedSameTreeAndLegacyFamily(t *testing.T) {
	repo, cache := preparedFixture(t), t.TempDir()
	old := incrementalHead(t, repo)
	first := prepareIncremental(t, repo, old, cache)
	legacyCache := t.TempDir()
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, legacyCache); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "--allow-empty", "-m", "same tree distinct history")
	next := incrementalHead(t, repo)
	before := StructuralWorkSnapshot()
	_, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{PreparedCommit: next, Profile: ProfileSyntaxOnly}, cache)
	requirePreparedFailure(t, err, "missing")
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{PreparedCommit: next, Profile: ProfileSyntaxOnly}, legacyCache)
	requirePreparedFailure(t, err, "revision-not-prepared")
	read, err := ReadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{PreparedCommit: old, Profile: ProfileSyntaxOnly}, legacyCache)
	if err != nil || read.Receipt.ArtifactFamily != "search-snapshot" || read.Receipt.GenerationID != "" || read.Snapshot.Header.Commit != old {
		t.Fatalf("legacy exact read=%#v err=%v", read.Receipt, err)
	}
	if StructuralWorkSnapshot() != before {
		t.Fatal("same-tree missing revision/legacy read did structural work")
	}
	second := prepareIncremental(t, repo, next, cache)
	if first.ID == second.ID || first.Tree != second.Tree {
		t.Fatal("same-tree commit generations collapsed")
	}
	before = StructuralWorkSnapshot()
	for _, target := range []string{old, next} {
		read, err := ReadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{PreparedCommit: target, Profile: ProfileSyntaxOnly}, cache)
		if err != nil || read.Snapshot.Header.Commit != target || read.Snapshot.Header.Tree != first.Tree || read.Receipt.ArtifactFamily != "syntax-generation" {
			t.Fatalf("same-tree exact read target=%s err=%v", target, err)
		}
	}
	if StructuralWorkSnapshot() != before {
		t.Fatal("ready same-tree exact reads did structural work")
	}
}

func TestHistoricalPreparedSourceProducerAndCancellationRefusals(t *testing.T) {
	repo, cache := preparedFixture(t), t.TempDir()
	old := incrementalHead(t, repo)
	prepareIncremental(t, repo, old, cache)
	before := StructuralWorkSnapshot()
	options := ProviderSnapshotOptions{PreparedCommit: old, Profile: ProfileSyntaxOnly}
	for _, target := range []string{"HEAD", strings.Repeat("A", 40)} {
		invalid := options
		invalid.PreparedCommit = target
		_, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", invalid, cache)
		requirePreparedFailure(t, err, "invalid-target")
	}
	_, err := LoadPreparedProviderSnapshot(t.Context(), repo, "other-producer", options, cache)
	requirePreparedFailure(t, err, "missing")
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{PreparedCommit: old, Profile: ProfileFull}, cache)
	requirePreparedFailure(t, err, "missing")
	_, err = LoadPreparedProviderSnapshot(t.Context(), filepath.Join(repo, "src"), "test", options, cache)
	requirePreparedFailure(t, err, "missing")
	clone := filepath.Join(t.TempDir(), filepath.Base(repo))
	git(t, filepath.Dir(clone), "clone", "--quiet", "--local", repo, clone)
	_, err = LoadPreparedProviderSnapshot(t.Context(), clone, "test", options, cache)
	requirePreparedFailure(t, err, "missing")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = LoadPreparedProviderSnapshot(ctx, repo, "test", options, cache)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled exact load=%v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	queryOptions := SearchOptions{PreparedOnly: true, PreparedCommit: old, Profile: ProfileSyntaxOnly, CacheDir: cache, afterPreindexLoad: cancel}
	_, err = SearchRepository(ctx, repo, "test", "persistDecision", queryOptions)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled exact search=%v", err)
	}
	if StructuralWorkSnapshot() != before {
		t.Fatal("refused exact reads did structural work")
	}
	// The public selector cannot turn an ordinary build/search into preparation.
	_, err = SearchRepository(t.Context(), repo, "test", "persistDecision", SearchOptions{PreparedCommit: old, Profile: ProfileSyntaxOnly, CacheDir: cache})
	requirePreparedFailure(t, err, "incompatible-read-options")
	_, err = BuildProviderSnapshotWithOptions(t.Context(), repo, "test", options)
	requirePreparedFailure(t, err, "incompatible-read-options")
	_, _, err = PreindexProviderSnapshot(t.Context(), repo, "test", options, cache)
	requirePreparedFailure(t, err, "incompatible-read-options")
	_, _, err = LoadOrBuildProviderSnapshot(t.Context(), repo, "test", options, cache, false)
	requirePreparedFailure(t, err, "incompatible-read-options")
	if StructuralWorkSnapshot() != before {
		t.Fatal("incompatible read selector reached a build/extraction entry point")
	}
	if !reflect.DeepEqual(options, ProviderSnapshotOptions{PreparedCommit: old, Profile: ProfileSyntaxOnly}) {
		t.Fatal("read mutated caller options")
	}
}

package sem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func preparedFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Prepared Test")
	git(t, repo, "config", "user.email", "prepared@example.invalid")
	write(t, repo, "src/target.ts", "export function persistDecision() { return 'recorded'; }\n")
	write(t, repo, "src/caller.ts", "import { persistDecision } from './target';\nexport function reviewDecision() { return persistDecision(); }\n")
	write(t, repo, "src/shadow.ts", "import { persistDecision } from './target';\nexport function shadowDecision(persistDecision: () => string) { return persistDecision(); }\n")
	write(t, repo, "src/unresolved.ts", "export function unresolvedDecision() { return missingDecision(); }\n")
	write(t, repo, "src/handlers.ts", "export function dispatchReview() { return reviewDecision(); }\n")
	write(t, repo, "commands/review-cycle.json", `{"function":"dispatchReview"}`)
	write(t, repo, "src/ambiguous-a.ts", "export function duplicateDecision() { return 1; }\n")
	write(t, repo, "src/ambiguous-b.ts", "export function duplicateDecision() { return 2; }\n")
	write(t, repo, "src/ambiguous-caller.ts", "export function ambiguousDecision() { return duplicateDecision(); }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "prepared fixture")
	return repo
}

func requirePreparedFailure(t *testing.T, err error, reason string) {
	t.Helper()
	var failure *PreparedSnapshotError
	if !errors.As(err, &failure) || string(failure.Reason) != reason {
		t.Fatalf("failure = %v, want %s", err, reason)
	}
}

func TestPreparedSnapshotReadsDoNoStructuralWork(t *testing.T) {
	repo, cache := preparedFixture(t), t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileFull}
	before := StructuralWorkSnapshot()
	fresh, hit, err := PreindexProviderSnapshot(t.Context(), repo, "test", options, cache)
	if err != nil || hit {
		t.Fatalf("prepare: hit=%v err=%v", hit, err)
	}
	afterPrepare := StructuralWorkSnapshot()
	if afterPrepare.Extractions <= before.Extractions || afterPrepare.Parses <= before.Parses || afterPrepare.Builds <= before.Builds || afterPrepare.Resolutions <= before.Resolutions {
		t.Fatalf("counter positive control failed: %v -> %v", before, afterPrepare)
	}
	loaded, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", options, cache)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, fresh) {
		t.Fatal("prepared artifact differs from complete fresh snapshot, including private binding metadata")
	}
	var target, caller, shadow, ambiguous string
	duplicates := map[string]bool{}
	for _, symbol := range loaded.Symbols {
		switch symbol.Name {
		case "persistDecision":
			if symbol.FilePath == "src/target.ts" && symbol.Kind == "function" {
				target = symbol.ID
			}
		case "reviewDecision":
			caller = symbol.ID
		case "shadowDecision":
			shadow = symbol.ID
		case "ambiguousDecision":
			ambiguous = symbol.ID
		case "duplicateDecision":
			duplicates[symbol.ID] = true
		}
	}
	if target == "" || caller == "" || shadow == "" || ambiguous == "" || len(duplicates) != 2 {
		t.Fatal("binding fixture definitions missing")
	}
	resolvedCalls, shadowCalls, ambiguousCalls := 0, 0, 0
	for _, relation := range loaded.Relations {
		if relation.Type != "CALLS" {
			continue
		}
		if relation.FromID == caller && relation.ToID == target && relation.Resolution == "import_resolved" {
			resolvedCalls++
		}
		if relation.FromID == shadow && relation.ToID == target {
			shadowCalls++
		}
		if relation.FromID == ambiguous && duplicates[relation.ToID] && relation.Resolution == "exact" {
			ambiguousCalls++
		}
	}
	if resolvedCalls != 1 || shadowCalls != 0 || ambiguousCalls != 0 {
		t.Fatalf("literal binding obligations: imported=%d want1 shadow=%d want0 ambiguous_exact=%d want0", resolvedCalls, shadowCalls, ambiguousCalls)
	}
	for _, question := range []string{"persist decision", "review decision caller", "review-cycle", "unresolved decision", "duplicate decision", "words without any matching source qzxjkv"} {
		response, err := SearchRepository(t.Context(), repo, "test", question, SearchOptions{
			PreparedOnly: true, Profile: ProfileFull, CacheDir: cache, MaxIndexedFiles: 1, TopK: 8, MaxContextBytes: 16000,
		})
		if err != nil {
			t.Fatalf("query %q: %v", question, err)
		}
		if response.Query != question || response.Commit != fresh.Header.Commit || response.Stats.AnalysisScope != "complete-prepared" || !response.Stats.IndexCacheHit {
			t.Fatalf("prepared source/read mode missing: %#v", response)
		}
		if question == "persist decision" || question == "review-cycle" {
			wanted := "persistDecision"
			if question == "review-cycle" {
				wanted = "dispatchReview"
			}
			found := false
			for _, result := range response.Results {
				if result.SymbolName == wanted {
					found = true
				}
			}
			if !found {
				t.Fatalf("prepared %q omitted required callable %s: %#v", question, wanted, response.Results)
			}
		}
	}
	if work := StructuralWorkSnapshot(); work != afterPrepare {
		t.Fatalf("prepared reads performed structural work: %v -> %v", afterPrepare, work)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := SearchRepository(ctx, repo, "test", "review decision", SearchOptions{PreparedOnly: true, CacheDir: cache, Profile: ProfileFull}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestPreparedSearchEqualsCompleteFreshRanking(t *testing.T) {
	repo, cache := preparedFixture(t), t.TempDir()
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileFull}, cache); err != nil {
		t.Fatal(err)
	}
	for _, deep := range []bool{false, true} {
		for _, question := range []string{"persist decision", "review decision caller", "review-cycle", "unresolved decision", "duplicate decision"} {
			options := SearchOptions{Profile: ProfileFull, IndexAllFiles: true, TopK: 8, MaxContextBytes: 16000, Deep: deep}
			fresh := options
			fresh.CacheDir = t.TempDir()
			if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileFull}, fresh.CacheDir); err != nil {
				t.Fatal(err)
			}
			expected, err := SearchRepository(t.Context(), repo, "test", question, fresh)
			if err != nil {
				t.Fatal(err)
			}
			options.CacheDir, options.PreparedOnly = cache, true
			before := StructuralWorkSnapshot()
			actual, err := SearchRepository(t.Context(), repo, "test", question, options)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual.Results, expected.Results) || !reflect.DeepEqual(actual.PartialFailures, expected.PartialFailures) || !reflect.DeepEqual(actual.Completeness, expected.Completeness) {
				t.Fatalf("prepared ranking differs from independently rebuilt complete warm read for %q:\nactual=%#v\nexpected=%#v", question, actual.Results, expected.Results)
			}
			if StructuralWorkSnapshot() != before {
				t.Fatal("prepared equality path performed structural work")
			}
		}
	}
}

func TestPreparedSnapshotFailuresNeverBuild(t *testing.T) {
	repo, cache := preparedFixture(t), t.TempDir()
	options := ProviderSnapshotOptions{Profile: ProfileFull}
	before := StructuralWorkSnapshot()
	_, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", options, cache)
	requirePreparedFailure(t, err, "missing")
	_, err = SearchRepository(t.Context(), repo, "test", "review cycle", SearchOptions{PreparedOnly: true, CacheDir: cache, Profile: ProfileFull})
	requirePreparedFailure(t, err, "missing")
	if StructuralWorkSnapshot() != before {
		t.Fatal("missing prepared read built source")
	}
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", options, cache); err != nil {
		t.Fatal(err)
	}
	before = StructuralWorkSnapshot()
	for _, change := range []ProviderSnapshotOptions{{Profile: ProfileFast}, {Profile: ProfileFull, Worktree: true}, {Profile: ProfileFull, OnlyFiles: []string{"src/caller.ts"}}} {
		if _, err := LoadPreparedProviderSnapshot(t.Context(), repo, "test", change, cache); err == nil {
			t.Fatalf("invalid read options accepted: %#v", change)
		}
	}
	git(t, repo, "commit", "--allow-empty", "-m", "same tree different history")
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", options, cache)
	requirePreparedFailure(t, err, "revision-not-prepared")
	if StructuralWorkSnapshot() != before {
		t.Fatal("incompatible prepared read performed structural work")
	}
	if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileFull, ForceRebuild: true}, cache); err != nil {
		t.Fatal(err)
	}
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", options, cache)
	if err != nil {
		t.Fatalf("explicit preparation did not repair exact revision: %v", err)
	}
	var artifact string
	if err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".json.gz") && !strings.HasSuffix(path, ".generation.json.gz") {
			artifact = path
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if artifact == "" {
		t.Fatal("missing prepared artifact")
	}
	if err := os.WriteFile(artifact, []byte("corrupt fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = StructuralWorkSnapshot()
	_, err = LoadPreparedProviderSnapshot(t.Context(), repo, "test", options, cache)
	requirePreparedFailure(t, err, "invalid-artifact")
	if StructuralWorkSnapshot() != before {
		t.Fatal("corrupt prepared read rebuilt source")
	}
}

func TestPreparedSearchRejectsChangesDuringRead(t *testing.T) {
	for _, change := range []string{"same-tree-revision", "cancellation"} {
		t.Run(change, func(t *testing.T) {
			repo, cache := preparedFixture(t), t.TempDir()
			if _, _, err := PreindexProviderSnapshot(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileFull}, cache); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			before := StructuralWorkSnapshot()
			_, err := SearchRepository(ctx, repo, "test", "persist decision", SearchOptions{
				PreparedOnly: true, CacheDir: cache, Profile: ProfileFull,
				afterPreindexLoad: func() {
					if change == "cancellation" {
						cancel()
					} else {
						git(t, repo, "commit", "--allow-empty", "-m", "revision changed during prepared read")
					}
				},
			})
			if change == "cancellation" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation = %v", err)
				}
			} else {
				requirePreparedFailure(t, err, "source-changed-during-read")
			}
			if StructuralWorkSnapshot() != before {
				t.Fatal("failed prepared admission performed structural work")
			}
		})
	}
}

package sem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func incrementalFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Incremental Test")
	git(t, repo, "config", "user.email", "incremental@example.invalid")
	write(t, repo, "api/handler.go", "package api\nfunc serve() string { return \"one\" }\n")
	write(t, repo, "ui/context.ts", "export function context(value: string): string;\nexport function context(value: string) { return value; }\nexport function shadow(context: () => string) { return context(); }\n")
	write(t, repo, "lib/state.py", "def outer(value):\n    def inner():\n        return value\n    return inner()\n")
	write(t, repo, "lib/state.erl", "-module(state).\n-export([walk/1]).\nwalk(0) -> 0;\nwalk(N) -> walk(N - 1).\n")
	write(t, repo, "native/state.cpp", "extern \"C\" { void state(); }\nnamespace review { struct State { void advance(); }; void State::advance() {} }\n")
	write(t, repo, "commands/start.json", `{"function":"serve"}`)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "incremental fixture")
	return repo
}

func incrementalHead(t *testing.T, repo string) string {
	t.Helper()
	commit, _, err := resolveCommittedHEAD(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func prepareIncremental(t *testing.T, repo, commit, cache string) PreparedGeneration {
	t.Helper()
	generation, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, commit, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func requireIncrementalFreshEquality(t *testing.T, repo string, generation PreparedGeneration) {
	t.Helper()
	fresh, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly, committedTarget: generation.Commit})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generation.Snapshot, fresh) {
		for i, symbol := range generation.Snapshot.Symbols {
			if i < len(fresh.Symbols) && !reflect.DeepEqual(symbol, fresh.Symbols[i]) {
				t.Errorf("symbol %s differs including private metadata: %#v / %#v", symbol.ID, symbol, fresh.Symbols[i])
			}
		}
		t.Fatalf("incremental generation does not equal independent uncached snapshot at target commit %s", generation.Commit)
	}
}

func requireIncrementalFailure(t *testing.T, err error, reason string) {
	t.Helper()
	var failure *IncrementalPreparationError
	if !errors.As(err, &failure) || failure.Reason != reason {
		t.Fatalf("failure=%v, want %s", err, reason)
	}
}

func TestIncrementalExtractionChangedBlobOnlyAndFreshParity(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	started := time.Now()
	initial := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	bootstrapElapsed := time.Since(started)
	if len(initial.Extraction.ExtractedPaths) != 6 {
		t.Fatalf("bootstrap extraction=%v", initial.Extraction)
	}
	requireIncrementalFreshEquality(t, repo, initial)
	write(t, repo, "api/handler.go", "package api\nfunc serve() string { return \"two\" }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "change one blob")
	before := StructuralWorkSnapshot()
	started = time.Now()
	delta := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	deltaElapsed := time.Since(started)
	after := StructuralWorkSnapshot()
	if after.Extractions-before.Extractions != 1 || after.Parses-before.Parses != 1 || after.Resolutions != before.Resolutions {
		t.Fatalf("delta must extract/parse only changed blob and do no content relation resolution: %v -> %v", before, after)
	}
	if !reflect.DeepEqual(delta.Extraction.ExtractedPaths, []string{"api/handler.go"}) || len(delta.Extraction.ReusedPaths) != 5 {
		t.Fatalf("delta reuse=%v", delta.Extraction)
	}
	requireIncrementalFreshEquality(t, repo, delta)
	t.Logf("isolated six-path fixture: bootstrap=%s delta=%s; changed extraction=1 reused=5 unchanged parser invocations=0; independent uncached equality passed", bootstrapElapsed, deltaElapsed)
	before = StructuralWorkSnapshot()
	retry := prepareIncremental(t, repo, delta.Commit, cache)
	if !retry.AlreadyPublished || retry.ID != delta.ID || !reflect.DeepEqual(retry.Snapshot, delta.Snapshot) || StructuralWorkSnapshot() != before {
		t.Fatal("published retry must return exact retained generation with zero structural work")
	}
	for _, symbol := range delta.Snapshot.Symbols {
		if symbol.Name == "serve" && !slices.Contains(symbol.Aliases, "start") {
			t.Fatal("reused registration table lost unique handler alias")
		}
	}
}

func TestIncrementalConcurrentPreparationPublishesOneImmutableGeneration(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	commit := incrementalHead(t, repo)
	type result struct {
		generation PreparedGeneration
		err        error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			generation, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, commit, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
			results <- result{generation, err}
		}()
	}
	close(start)
	left, right := <-results, <-results
	if left.err != nil || right.err != nil {
		t.Fatalf("concurrent preparation errors: %v / %v", left.err, right.err)
	}
	if left.generation.ID != right.generation.ID || !reflect.DeepEqual(left.generation.Snapshot, right.generation.Snapshot) {
		t.Fatal("concurrent preparation disagreed on the immutable generation")
	}
	before := StructuralWorkSnapshot()
	retained := prepareIncremental(t, repo, commit, cache)
	if !retained.AlreadyPublished || StructuralWorkSnapshot() != before {
		t.Fatal("concurrent publication did not retain ready generation without reprocessing")
	}
	files, err := filepath.Glob(filepath.Join(cache, "generations", preparedGenerationVersion, "*.json.gz"))
	if err != nil || len(files) != 1 {
		t.Fatalf("ready generation count=%d err=%v", len(files), err)
	}
	requireIncrementalFreshEquality(t, repo, retained)
}

func TestIncrementalConcurrentPublicationRetainsInvocationWork(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	commit := incrementalHead(t, repo)
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releasePreparation := func() { releaseOnce.Do(func() { close(release) }) }
	defer releasePreparation()
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly, Progress: func(ProgressEvent) {
		enterOnce.Do(func() { close(entered); <-release })
	}}
	type result struct {
		generation PreparedGeneration
		err        error
	}
	finished := make(chan result, 1)
	go func() {
		generation, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache)
		finished <- result{generation, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first publisher did not reach preparation after observing absence")
	}
	winner := prepareIncremental(t, repo, commit, cache)
	if len(winner.Extraction.ExtractedPaths) != 6 {
		t.Fatalf("winner extraction positive control=%v", winner.Extraction)
	}
	before := StructuralWorkSnapshot()
	releasePreparation()
	var loser result
	select {
	case loser = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared publisher did not finish after release")
	}
	if loser.err != nil {
		t.Fatal(loser.err)
	}
	if !loser.generation.AlreadyPublished || loser.generation.ID != winner.ID || !reflect.DeepEqual(loser.generation.Snapshot, winner.Snapshot) {
		t.Fatal("publication loser did not validate and retain the immutable winner")
	}
	if len(loser.generation.Extraction.ReusedPaths) != 6 || len(loser.generation.Extraction.ExtractedPaths) != 0 || len(loser.generation.Extraction.UncacheablePaths) != 0 {
		t.Fatalf("publication loser discarded completed path work: %v", loser.generation.Extraction)
	}
	after := StructuralWorkSnapshot()
	if after.Parses != before.Parses || after.Extractions != before.Extractions || after.Builds != before.Builds || after.Resolutions != before.Resolutions {
		t.Fatalf("loser work interval does not establish reuse: %v -> %v", before, after)
	}
	before = after
	retry := prepareIncremental(t, repo, commit, cache)
	if !retry.AlreadyPublished || len(retry.Extraction.ReusedPaths) != 0 || len(retry.Extraction.ExtractedPaths) != 0 || StructuralWorkSnapshot() != before {
		t.Fatal("immediate-ready retry acquired someone else's invocation work")
	}
}

func TestIncrementalCrashHelperProcess(t *testing.T) {
	if os.Getenv("NEUVE_INCREMENTAL_NATIVE_CRASH") != "1" {
		return
	}
	_, err := PrepareIncrementalProviderSnapshot(t.Context(), os.Getenv("NEUVE_INCREMENTAL_NATIVE_REPO"), os.Getenv("NEUVE_INCREMENTAL_NATIVE_COMMIT"), "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly, Progress: func(event ProgressEvent) {
		if event.Phase == BuildPhaseFinalize {
			os.Exit(23)
		}
	}}, os.Getenv("NEUVE_INCREMENTAL_NATIVE_CACHE"))
	t.Fatalf("helper did not crash before publication: %v", err)
}

func TestIncrementalAbruptProcessCrashBeforePublication(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	commit := incrementalHead(t, repo)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestIncrementalCrashHelperProcess$")
	child.Env = append(os.Environ(), "NEUVE_INCREMENTAL_NATIVE_CRASH=1", "NEUVE_INCREMENTAL_NATIVE_REPO="+repo, "NEUVE_INCREMENTAL_NATIVE_COMMIT="+commit, "NEUVE_INCREMENTAL_NATIVE_CACHE="+cache)
	output, err := child.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 23 {
		t.Fatalf("helper exit=%v output=%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(cache, "generations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crashed process published a generation: %v", err)
	}
	before := StructuralWorkSnapshot()
	retried := prepareIncremental(t, repo, commit, cache)
	if retried.AlreadyPublished || StructuralWorkSnapshot().Parses != before.Parses || len(retried.Extraction.ReusedPaths) != 6 {
		t.Fatal("restart did not reuse retained facts while creating first complete ready artifact")
	}
	requireIncrementalFreshEquality(t, repo, retried)
}

func TestIncrementalPublicationRejectsRedirectedGenerationDirectory(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	target := filepath.Join(cache, "redirect-target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("redirect-target", filepath.Join(cache, "generations")); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, incrementalHead(t, repo), "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	requireIncrementalFailure(t, err, "publication-failed")
	files, err := os.ReadDir(target)
	if err != nil || len(files) != 0 {
		t.Fatalf("redirected target received files: count=%d err=%v", len(files), err)
	}
	if err := os.Remove(filepath.Join(cache, "generations")); err != nil {
		t.Fatal(err)
	}
	before := StructuralWorkSnapshot()
	retried := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	if retried.AlreadyPublished || StructuralWorkSnapshot().Parses != before.Parses {
		t.Fatal("publication retry must reuse completed extraction without claiming earlier readiness")
	}
}

func TestIncrementalAliasLookupAmbiguityAndRemoval(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	write(t, repo, "other/handler.go", "package other\nfunc serve() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "new same-name declaration")
	before := StructuralWorkSnapshot()
	ambiguous := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	if StructuralWorkSnapshot().Parses-before.Parses != 1 {
		t.Fatal("ambiguity lookup must not parse unchanged handler")
	}
	for _, symbol := range ambiguous.Snapshot.Symbols {
		if symbol.Name == "serve" && slices.Contains(symbol.Aliases, "start") {
			t.Fatal("new same-name declaration must invalidate unique alias attribution")
		}
	}
	requireIncrementalFreshEquality(t, repo, ambiguous)
	git(t, repo, "rm", "other/handler.go")
	git(t, repo, "commit", "-m", "remove ambiguity")
	before = StructuralWorkSnapshot()
	unambiguous := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	if StructuralWorkSnapshot().Parses != before.Parses {
		t.Fatal("removing lookup rival must not parse unchanged blobs")
	}
	found := false
	for _, symbol := range unambiguous.Snapshot.Symbols {
		if symbol.Name == "serve" && slices.Contains(symbol.Aliases, "start") {
			found = true
		}
	}
	if !found {
		t.Fatal("removing same-name declaration must restore unique alias attribution")
	}
	requireIncrementalFreshEquality(t, repo, unambiguous)
}

func TestIncrementalExactCommitPinAndSameTreeHistoryIdentity(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	old := incrementalHead(t, repo)
	git(t, repo, "commit", "--allow-empty", "-m", "same tree different history")
	current := incrementalHead(t, repo)
	oldGeneration := prepareIncremental(t, repo, old, cache)
	if oldGeneration.Commit != old || oldGeneration.Snapshot.Header.Commit != old {
		t.Fatal("queued exact target was replaced by newer HEAD")
	}
	before := StructuralWorkSnapshot()
	newGeneration := prepareIncremental(t, repo, current, cache)
	if oldGeneration.ID == newGeneration.ID || oldGeneration.Tree != newGeneration.Tree || StructuralWorkSnapshot().Parses != before.Parses || len(newGeneration.Extraction.ExtractedPaths) != 0 {
		t.Fatal("same-tree history must have distinct exact generation identity with all extraction reused")
	}
	requireIncrementalFreshEquality(t, repo, oldGeneration)
	requireIncrementalFreshEquality(t, repo, newGeneration)
}

func TestIncrementalRenameRebindsSourceOwnership(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	initial := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	git(t, repo, "mv", "api/handler.go", "api/renamed.go")
	git(t, repo, "commit", "-m", "rename file")
	before := StructuralWorkSnapshot()
	renamed := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	if StructuralWorkSnapshot().Parses-before.Parses != 1 || !reflect.DeepEqual(renamed.Extraction.ExtractedPaths, []string{"api/renamed.go"}) {
		t.Fatal("rename must rebind through extraction, not reuse stale path IDs")
	}
	for _, file := range renamed.Snapshot.Files {
		if file.Path == "api/handler.go" {
			t.Fatal("deleted file survived generation")
		}
	}
	for _, symbol := range renamed.Snapshot.Symbols {
		if symbol.FilePath == "api/handler.go" {
			t.Fatal("renamed symbol retained stale ownership")
		}
	}
	if len(initial.Snapshot.Files) != len(renamed.Snapshot.Files) {
		t.Fatal("rename dropped source inventory")
	}
	requireIncrementalFreshEquality(t, repo, renamed)
}

func TestIncrementalPolicyAndProducerInvalidation(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	commit := incrementalHead(t, repo)
	initial := prepareIncremental(t, repo, commit, cache)
	write(t, repo, ".graphignore", "lib/state.py\n")
	changed := prepareIncremental(t, repo, commit, cache)
	if changed.ID == initial.ID || len(changed.Extraction.ReusedPaths) != 0 {
		t.Fatal("policy change must select a separate qualified extraction namespace")
	}
	for _, file := range changed.Snapshot.Files {
		if file.Path == "lib/state.py" {
			t.Fatal("ignored source revived from prior generation")
		}
	}
	requireIncrementalFreshEquality(t, repo, changed)
	producer, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, commit, "different-producer", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if producer.ID == changed.ID || len(producer.Extraction.ReusedPaths) != 0 {
		t.Fatal("producer change reused incompatible extraction products")
	}
}

func TestIncrementalCancellationLeavesOnlyReusableFacts(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	commit := incrementalHead(t, repo)
	ctx, cancel := context.WithCancel(t.Context())
	_, err := PrepareIncrementalProviderSnapshot(ctx, repo, commit, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly, Progress: func(event ProgressEvent) {
		if event.Phase == BuildPhaseFinalize {
			cancel()
		}
	}}, cache)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
	before := StructuralWorkSnapshot()
	retried := prepareIncremental(t, repo, commit, cache)
	if retried.AlreadyPublished || StructuralWorkSnapshot().Parses != before.Parses || len(retried.Extraction.ReusedPaths) != 6 {
		t.Fatal("cancelled prepublication must expose no ready generation; retry reuses completed facts without parsing")
	}
	requireIncrementalFreshEquality(t, repo, retried)
}

func TestIncrementalRejectsUnsupportedPreparationBeforeStructuralWork(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	for _, options := range []ProviderSnapshotOptions{{Profile: ProfileFull}, {Profile: ProfileFast}, {Profile: ProfileSyntaxOnly, Worktree: true}, {Profile: ProfileSyntaxOnly, ForceRebuild: true}, {Profile: ProfileSyntaxOnly, OnlyFiles: []string{"api/handler.go"}}} {
		before := StructuralWorkSnapshot()
		_, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, incrementalHead(t, repo), "test", options, cache)
		requireIncrementalFailure(t, err, "unsupported-preparation")
		if StructuralWorkSnapshot() != before {
			t.Fatal("unsupported contract performed structural work")
		}
	}
}

func TestIncrementalCorruptGenerationNeverFallsBackToBuild(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	generation := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	entry, err := newCacheEntry(cache, "generations", preparedGenerationVersion, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, entry.relative), []byte("truncated generation"), 0600); err != nil {
		t.Fatal(err)
	}
	before := StructuralWorkSnapshot()
	_, err = PrepareIncrementalProviderSnapshot(t.Context(), repo, generation.Commit, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	requireIncrementalFailure(t, err, "invalid-generation")
	if StructuralWorkSnapshot() != before {
		t.Fatal("corrupt ready artifact triggered fallback preparation")
	}
}

func TestIncrementalCorruptExtractionRefusesPublication(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	initial := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	files, err := filepath.Glob(filepath.Join(cache, "extractions", extractionCacheVersion, "*.json.gz"))
	if err != nil || len(files) != 6 {
		t.Fatalf("extraction artifact count=%d err=%v", len(files), err)
	}
	if err := os.WriteFile(files[0], []byte("incomplete extraction artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "--allow-empty", "-m", "new generation same facts")
	before := StructuralWorkSnapshot()
	_, err = PrepareIncrementalProviderSnapshot(t.Context(), repo, incrementalHead(t, repo), "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	var failure *IncrementalPreparationError
	if !errors.As(err, &failure) || failure.Reason != "preparation-failed" {
		t.Fatalf("corrupt extraction failure=%v", err)
	}
	if StructuralWorkSnapshot().Parses != before.Parses {
		t.Fatal("corrupt immutable extraction triggered silent replacement parsing")
	}
	retained, err := filepath.Glob(filepath.Join(cache, "generations", preparedGenerationVersion, "*.json.gz"))
	if err != nil || len(retained) != 1 {
		t.Fatalf("incomplete candidate published generation: count=%d err=%v", len(retained), err)
	}
	if initial.ID == "" {
		t.Fatal("positive control generation absent")
	}
}

func TestIncrementalInvalidTargetDoesNotInspectWorkingTree(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	for _, target := range []string{"HEAD", "--all", "ABCDEF", "", string(make([]byte, 40))} {
		before := StructuralWorkSnapshot()
		_, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, target, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
		requireIncrementalFailure(t, err, "invalid-target")
		if StructuralWorkSnapshot() != before {
			t.Fatal("invalid exact target triggered structural work")
		}
	}
	before := StructuralWorkSnapshot()
	_, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
	requireIncrementalFailure(t, err, "source-unavailable")
	if StructuralWorkSnapshot() != before {
		t.Fatal("missing exact commit fell back to working-tree extraction")
	}
}

func TestIncrementalImmutablePublicationCannotReplaceReadyBytes(t *testing.T) {
	cache := t.TempDir()
	entry, err := newCacheEntry(cache, "generations", preparedGenerationVersion, contentHash([]byte("immutable-native-fixture")))
	if err != nil {
		t.Fatal(err)
	}
	if err := entry.writeImmutable(t.Context(), "fixture", map[string]string{"value": "first"}); err != nil {
		t.Fatal(err)
	}
	if err := entry.writeImmutable(t.Context(), "fixture", map[string]string{"value": "replacement"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("replacement error=%v", err)
	}
	file, err := entry.open()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var retained map[string]string
	if err := decodeCompressedJSON(t.Context(), file, &retained); err != nil {
		t.Fatal(err)
	}
	if retained["value"] != "first" {
		t.Fatal("immutable ready name was overwritten")
	}
	info, err := file.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private artifact mode=%v err=%v", info, err)
	}
}

func TestIncrementalTypeScriptDeltaAndDeletedFileDoNotRetainStaleSymbols(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	write(t, repo, "ui/context.ts", "export class Review { readonly phase = 'prepared'; }\nexport function revised(value: number) { return value + 1; }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "replace TypeScript declarations")
	before := StructuralWorkSnapshot()
	delta := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	after := StructuralWorkSnapshot()
	if after.Parses-before.Parses != 1 || after.Extractions-before.Extractions != 1 || after.Resolutions != before.Resolutions {
		t.Fatalf("TypeScript delta work=%v -> %v", before, after)
	}
	if !reflect.DeepEqual(delta.Extraction.ExtractedPaths, []string{"ui/context.ts"}) || len(delta.Extraction.ReusedPaths) != 5 {
		t.Fatalf("TypeScript delta reuse=%v", delta.Extraction)
	}
	for _, symbol := range delta.Snapshot.Symbols {
		if symbol.FilePath == "ui/context.ts" && (symbol.Name == "context" || symbol.Name == "shadow") {
			t.Fatal("changed blob retained obsolete TypeScript declarations")
		}
	}
	requireIncrementalFreshEquality(t, repo, delta)
	git(t, repo, "rm", "ui/context.ts")
	git(t, repo, "commit", "-m", "remove TypeScript source")
	before = StructuralWorkSnapshot()
	deleted := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
	if StructuralWorkSnapshot().Parses != before.Parses || len(deleted.Extraction.ExtractedPaths) != 0 || len(deleted.Extraction.ReusedPaths) != 5 {
		t.Fatalf("file deletion reparsed unchanged corpus: %v", deleted.Extraction)
	}
	for _, symbol := range deleted.Snapshot.Symbols {
		if symbol.FilePath == "ui/context.ts" {
			t.Fatal("deleted path retained symbols")
		}
	}
	requireIncrementalFreshEquality(t, repo, deleted)
}

func TestIncrementalRetainedPrivateMetadataCorruptionRejectsWithoutParsing(t *testing.T) {
	for _, family := range []string{"generation", "extraction"} {
		t.Run(family, func(t *testing.T) {
			repo, cache := incrementalFixture(t), t.TempDir()
			initial := prepareIncremental(t, repo, incrementalHead(t, repo), cache)
			var entry cacheEntry
			var value any
			if family == "generation" {
				var err error
				entry, err = newCacheEntry(cache, "generations", preparedGenerationVersion, initial.ID)
				if err != nil {
					t.Fatal(err)
				}
				file, err := entry.open()
				if err != nil {
					t.Fatal(err)
				}
				var retained cachedPreparedGeneration
				err = decodeCompressedJSON(t.Context(), file, &retained)
				_ = file.Close()
				if err != nil || len(retained.Snapshot.SymbolParameterNames) == 0 {
					t.Fatalf("private metadata positive control absent: %v", err)
				}
				for id, names := range retained.Snapshot.SymbolParameterNames {
					retained.Snapshot.SymbolParameterNames[id] = append(names, "corrupted-parameter")
				}
				value = retained // deliberately retain the old checksum
			} else {
				paths, err := filepath.Glob(filepath.Join(cache, "extractions", extractionCacheVersion, "*.json.gz"))
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					file, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					var retained sealedFileExtraction
					err = decodeCompressedJSON(t.Context(), file, &retained)
					_ = file.Close()
					if err != nil {
						t.Fatal(err)
					}
					if retained.Facts.Path != "ui/context.ts" {
						continue
					}
					if len(retained.Facts.Internals.SymbolParameterNames) == 0 {
						t.Fatal("extraction private metadata positive control absent")
					}
					for id, names := range retained.Facts.Internals.SymbolParameterNames {
						retained.Facts.Internals.SymbolParameterNames[id] = append(names, "corrupted-parameter")
					}
					entry, err = newCacheEntry(cache, "extractions", extractionCacheVersion, retained.Facts.Key)
					if err != nil {
						t.Fatal(err)
					}
					value = retained // deliberately retain the old checksum
					break
				}
				if value == nil {
					t.Fatal("TypeScript extraction positive control absent")
				}
			}
			if err := entry.write("native-corruption-fixture", value); err != nil {
				t.Fatal(err)
			}
			if family == "extraction" {
				git(t, repo, "commit", "--allow-empty", "-m", "require reusable file facts")
			}
			before := StructuralWorkSnapshot()
			_, err := PrepareIncrementalProviderSnapshot(t.Context(), repo, incrementalHead(t, repo), "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache)
			if family == "generation" {
				requireIncrementalFailure(t, err, "invalid-generation")
			} else {
				requireIncrementalFailure(t, err, "preparation-failed")
			}
			if StructuralWorkSnapshot().Parses != before.Parses {
				t.Fatal("private metadata corruption silently triggered reparsing")
			}
		})
	}
}

func TestSearchSnapshotCacheRetainsActualCDeclarationMetadata(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init")
	write(t, repo, "engine.h", "extern \"C\" {\nvoid invoke();\n}\nnamespace engine {\nclass State {\npublic:\nvoid advance();\n};\n}\n")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=Native Test", "-c", "user.email=native@example.invalid", "commit", "-m", "C declaration metadata")
	fresh, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test", ProviderSnapshotOptions{Profile: ProfileFull})
	if err != nil {
		t.Fatal(err)
	}
	cached := newCachedSearchSnapshot("test", fresh.Header.Commit, fresh.Header.Tree, ProviderSnapshotOptions{Profile: ProfileFull}, fresh)
	if len(cached.CDeclarationMetadata) == 0 {
		t.Fatal("actual full C header extraction produced no private declaration metadata")
	}
	bytes, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	var restored cachedSearchSnapshot
	if err := json.Unmarshal(bytes, &restored); err != nil {
		t.Fatal(err)
	}
	restoreCachedSearchInternals(&restored)
	if !reflect.DeepEqual(fresh.Symbols, restored.Snapshot.Symbols) {
		t.Fatal("persistent snapshot lost actual C/C++ declaration ownership or linkage metadata")
	}
}

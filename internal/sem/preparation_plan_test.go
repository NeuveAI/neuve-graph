package sem

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPreparationPlanReadsOnlyValidatedGeneration(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	commit := incrementalHead(t, repo)
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	before := StructuralWorkSnapshot()
	plan, err := PlanIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, "")
	if err != nil || plan.Availability != "absent" || StructuralWorkSnapshot() != before {
		t.Fatalf("absent plan=%#v err=%v", plan, err)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatal("absent plan published files")
	}
	selected := plan.Receipt.GenerationID
	generation, err := PrepareExpectedIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, selected)
	if err != nil || generation.ID != selected || !reflect.DeepEqual(generation.Receipt, plan.Receipt) {
		t.Fatalf("guarded prepare=%#v err=%v", generation, err)
	}
	before = StructuralWorkSnapshot()
	ready, err := PlanIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, selected)
	if err != nil || ready.Availability != "ready" || !reflect.DeepEqual(ready.Receipt, plan.Receipt) || StructuralWorkSnapshot() != before {
		t.Fatalf("ready plan=%#v err=%v", ready, err)
	}
	entry, err := newCacheEntry(cache, "generations", preparedGenerationVersion, selected)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, entry.relative)
	retained, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PlanIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, strings.Repeat("0", 64))
	requireIncrementalFailure(t, err, "generation-mismatch")
	bytes, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(bytes, retained) || StructuralWorkSnapshot() != before {
		t.Fatal("mismatch altered retained artifact or performed work")
	}
	if err = os.WriteFile(path, []byte("corrupt retained artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = PlanIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, selected)
	requireIncrementalFailure(t, err, "invalid-generation")
	if StructuralWorkSnapshot() != before {
		t.Fatal("corrupt plan rebuilt")
	}
}

func TestPreparationGuardRefusesCapturedPolicyProducerAndSourceDrift(t *testing.T) {
	repo, cache := incrementalFixture(t), t.TempDir()
	commit := incrementalHead(t, repo)
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	plan, err := PlanIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, "")
	if err != nil {
		t.Fatal(err)
	}
	before := StructuralWorkSnapshot()
	guard := func(repo, version string, opts ProviderSnapshotOptions) {
		t.Helper()
		_, err := PrepareExpectedIncrementalProviderSnapshot(t.Context(), repo, commit, version, opts, cache, plan.Receipt.GenerationID)
		requireIncrementalFailure(t, err, "generation-mismatch")
		if StructuralWorkSnapshot() != before {
			t.Fatal("changed selection performed structural work")
		}
		if files, err := os.ReadDir(cache); err != nil || len(files) != 0 {
			t.Fatal("changed selection published files")
		}
	}
	write(t, repo, ".graphignore", "lib/state.py\n")
	guard(repo, "test", options)
	if err = os.Remove(filepath.Join(repo, ".graphignore")); err != nil {
		t.Fatal(err)
	}
	guard(repo, "different-provider-version", options)
	guard(repo, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly, MaxFiles: 1})
	clone := filepath.Join(t.TempDir(), "clone")
	if output, err := exec.Command("git", "clone", "--quiet", repo, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone fixture: %v %s", err, output)
	}
	guard(clone, "test", options)
	git(t, repo, "remote", "add", "origin", "https://github.com/literal/preparation-owner.git")
	guard(repo, "test", options)
	git(t, repo, "remote", "remove", "origin")
	selected, err := PrepareExpectedIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, plan.Receipt.GenerationID)
	if err != nil || selected.ID != plan.Receipt.GenerationID {
		t.Fatalf("unchanged positive control=%v", err)
	}
}

func TestPreparationPlanInvalidPresenceAndAbsentRoot(t *testing.T) {
	repo := incrementalFixture(t)
	commit := incrementalHead(t, repo)
	options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}
	absentRoot := filepath.Join(t.TempDir(), "not-created")
	plan, err := PlanIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, absentRoot, "")
	if err != nil || plan.Availability != "absent" {
		t.Fatalf("absent root=%#v %v", plan, err)
	}
	if _, err = os.Lstat(absentRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("plan created cache root")
	}
	for _, kind := range []string{"directory", "dangling-link"} {
		t.Run(kind, func(t *testing.T) {
			cache := t.TempDir()
			before := StructuralWorkSnapshot()
			plan, err := PlanIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, "")
			if err != nil {
				t.Fatal(err)
			}
			entry, err := newCacheEntry(cache, "generations", preparedGenerationVersion, plan.Receipt.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cache, entry.relative)
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "directory" {
				err = os.Mkdir(path, 0700)
			} else {
				err = os.Symlink("missing", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = PlanIncrementalProviderSnapshot(t.Context(), repo, commit, "test", options, cache, plan.Receipt.GenerationID)
			requireIncrementalFailure(t, err, "artifact-unavailable")
			if StructuralWorkSnapshot() != before {
				t.Fatal("invalid presence did structural work")
			}
		})
	}
}

func TestPreparationGuardLateOwnerChangeAndCancellationNeverReady(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner-drift", true: "cancelled"}[cancelled], func(t *testing.T) {
			repo, cache := incrementalFixture(t), t.TempDir()
			commit := incrementalHead(t, repo)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			plan, err := PlanIncrementalProviderSnapshot(ctx, repo, commit, "test", ProviderSnapshotOptions{Profile: ProfileSyntaxOnly}, cache, "")
			if err != nil {
				t.Fatal(err)
			}
			options := ProviderSnapshotOptions{Profile: ProfileSyntaxOnly, Progress: func(event ProgressEvent) {
				if event.Phase == BuildPhaseFinalize {
					if cancelled {
						cancel()
					} else {
						git(t, repo, "remote", "add", "origin", "https://github.com/literal/changed-after-selection.git")
					}
				}
			}}
			_, err = PrepareExpectedIncrementalProviderSnapshot(ctx, repo, commit, "test", options, cache, plan.Receipt.GenerationID)
			if cancelled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation cause=%v", err)
				}
			} else {
				requireIncrementalFailure(t, err, "source-changed")
			}
			if files, _ := filepath.Glob(filepath.Join(cache, "generations", preparedGenerationVersion, "*.json.gz")); len(files) != 0 {
				t.Fatal("late failure published ready artifact")
			}
		})
	}
}

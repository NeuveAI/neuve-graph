//go:build darwin || linux

package sem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// vanishingGitMetadataFixture is a linked worktree whose common directory
// also holds a second worktree's administrative directory, the one another
// process's `git worktree remove` deletes while this walk runs.
func vanishingGitMetadataFixture(t *testing.T) (repo, gitDir, common, other string) {
	t.Helper()
	repo, gitDir, common = linkedGitMetadataTreeFixture(t)
	other = filepath.Join(common, "worktrees", "other")
	writeFile(t, other, "HEAD", strings.Repeat("b", 40)+"\n")
	writeFile(t, other, "commondir", "../..\n")
	return repo, gitDir, common, other
}

// beforeGitMetadataOpen runs change once, when the walk is about to open
// target: after target was listed and queued, before it is opened. It reports
// whether that moment came, so a test cannot pass without its change.
func beforeGitMetadataOpen(t *testing.T, target string, change func()) *atomic.Bool {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	var fired atomic.Bool
	previous := gitMetadataTreeBeforeOpen
	gitMetadataTreeBeforeOpen = func(path string) {
		if path == resolved && fired.CompareAndSwap(false, true) {
			change()
		}
	}
	t.Cleanup(func() { gitMetadataTreeBeforeOpen = previous })
	return &fired
}

func requireGitMetadataChangeRan(t *testing.T, fired *atomic.Bool) {
	t.Helper()
	if !fired.Load() {
		t.Fatal("the walk never reached the directory the test changes")
	}
}

// A worktree removed by another process between the walk listing it and
// opening it is absent when Git starts, so the run may proceed. Before this
// rule the whole query was refused (graph round 23, finding 1).
func TestGitMetadataTreeGuardTreatsADirectoryRemovedAfterListingAsAbsent(t *testing.T) {
	repo, _, _, other := vanishingGitMetadataFixture(t)
	fired := beforeGitMetadataOpen(t, other, func() {
		if err := os.RemoveAll(other); err != nil {
			t.Error(err)
		}
	})
	if !gitMetadataSafeForSubprocess(repo) {
		t.Fatal("a worktree removed between listing and opening refused the walk")
	}
	requireGitMetadataChangeRan(t, fired)
}

// Only "gone" is absent. Whatever stands at the listed path instead, or
// blocks the walk from looking, is still refused.
func TestGitMetadataTreeGuardStillRefusesADirectorySwappedAfterListing(t *testing.T) {
	cases := map[string]func(t *testing.T, common, other string) error{
		// A file where a directory was listed: the walk never classified it.
		"regular file": func(_ *testing.T, _, other string) error {
			if err := os.RemoveAll(other); err != nil {
				return err
			}
			return os.WriteFile(other, []byte("x\n"), 0o644)
		},
		// The parent replaced by a file answers ENOTDIR, not not-exist.
		"parent replaced by a file": func(_ *testing.T, common, _ string) error {
			worktrees := filepath.Join(common, "worktrees")
			if err := os.RemoveAll(worktrees); err != nil {
				return err
			}
			return os.WriteFile(worktrees, []byte("x\n"), 0o644)
		},
		"permission denied": func(t *testing.T, common, _ string) error {
			if os.Geteuid() == 0 {
				t.Skip("permissions do not bind the super-user")
			}
			worktrees := filepath.Join(common, "worktrees")
			t.Cleanup(func() { _ = os.Chmod(worktrees, 0o755) })
			return os.Chmod(worktrees, 0o000)
		},
	}
	for name, swap := range cases {
		t.Run(name, func(t *testing.T) {
			repo, _, common, other := vanishingGitMetadataFixture(t)
			fired := beforeGitMetadataOpen(t, other, func() {
				if err := swap(t, common, other); err != nil {
					t.Error(err)
				}
			})
			if gitMetadataSafeForSubprocess(repo) {
				t.Fatalf("a listed directory swapped for %s passed the walk", name)
			}
			requireGitMetadataChangeRan(t, fired)
		})
	}
}

// A listed directory swapped for a link off the volume is refused as
// off-volume, never skipped as missing: the resolver's volume and mount checks
// run before it looks the target up, including a target that does not exist.
func TestGitMetadataTreeGuardRefusesADirectorySwappedForAnOffVolumeLink(t *testing.T) {
	repoInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	devInfo, err := os.Stat("/dev")
	if err != nil {
		t.Fatal(err)
	}
	repoDevice, repoOK := fileSystemDevice(repoInfo)
	devDevice, devOK := fileSystemDevice(devInfo)
	if !repoOK || !devOK {
		t.Fatal("platform did not expose filesystem device identities")
	}
	if repoDevice == devDevice {
		t.Skip("/dev is not a distinct filesystem on this host")
	}
	for name, target := range map[string]string{
		"existing target": "/dev",
		"missing target":  "/dev/neuve-graph-missing/worktree",
	} {
		t.Run(name, func(t *testing.T) {
			repo, _, _, other := vanishingGitMetadataFixture(t)
			fired := beforeGitMetadataOpen(t, other, func() {
				if err := os.RemoveAll(other); err != nil {
					t.Error(err)
					return
				}
				if err := os.Symlink(target, other); err != nil {
					t.Error(err)
				}
			})
			if gitMetadataSafeForSubprocess(repo) {
				t.Fatalf("a listed directory swapped for a link to %s passed the walk", target)
			}
			requireGitMetadataChangeRan(t, fired)
			if _, _, err := openSameVolumePath(repo, other); !errors.Is(err, errSymlinkChainOffVolume) {
				t.Fatalf("opening the swapped link: error = %v, want %v", err, errSymlinkChainOffVolume)
			}
		})
	}
}

// The walk's roots are what Git is pointed at, so a root gone before its open
// still refuses; only directories found inside them may be absent.
func TestGitMetadataTreeGuardRefusesAWalkRootRemovedBeforeOpening(t *testing.T) {
	for _, root := range []string{"git directory", "common directory"} {
		t.Run(root, func(t *testing.T) {
			repo, gitDir, common, _ := vanishingGitMetadataFixture(t)
			target := gitDir
			if root == "common directory" {
				target = common
			}
			fired := beforeGitMetadataOpen(t, target, func() {
				if err := os.RemoveAll(target); err != nil {
					t.Error(err)
				}
			})
			if gitMetadataSafeForSubprocess(repo) {
				t.Fatalf("the walk accepted a repository whose %s vanished before it was opened", root)
			}
			requireGitMetadataChangeRan(t, fired)
		})
	}
}

// A walk that skipped a vanished directory leaves nothing a later check could
// reuse, so the directory coming back with unsafe content is walked and
// refused, not answered from the earlier walk.
func TestReusedGitMetadataWalkIsNotTakenAfterAListedDirectoryVanished(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	repo, _, _, other := vanishingGitMetadataFixture(t)
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	if safe, footprint := recordedGitMetadataValidation(absRepo); !safe || footprint == nil {
		t.Fatal("fixture error: the undisturbed walk left no reusable footprint")
	}
	fired := beforeGitMetadataOpen(t, other, func() {
		if err := os.RemoveAll(other); err != nil {
			t.Error(err)
		}
	})
	ctx := withReusableGitMetadataValidation(context.Background(), repo)
	requireGitMetadataChangeRan(t, fired)
	if ctx.Value(gitMetadataValidationContextKey{}) != nil {
		t.Fatal("a walk that skipped a vanished directory left a reusable receipt")
	}
	writeGitMetadataFIFO(t, filepath.Join(other, "HEAD"))
	requireRefusedAsToday(t, ctx, repo, walks, "the vanished worktree came back holding a FIFO")
}

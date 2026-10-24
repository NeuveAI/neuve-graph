//go:build darwin || linux

package sem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countGitMetadataWalks counts full walks for the rest of the test, and lets
// the walk's answer be reused however recently the fixture was written.
func countGitMetadataWalks(t *testing.T, window time.Duration) *atomic.Int64 {
	t.Helper()
	var walks atomic.Int64
	previousObserved, previousWindow := gitMetadataWalkObserved, gitMetadataRacyWindow
	gitMetadataWalkObserved = func() { walks.Add(1) }
	gitMetadataRacyWindow = window
	t.Cleanup(func() { gitMetadataWalkObserved, gitMetadataRacyWindow = previousObserved, previousWindow })
	return &walks
}

// reusableValidation is a read's reusable walk of a safe repository, checked
// to be reused by a later check that walks nothing.
func reusableValidation(t *testing.T, repo string, walks *atomic.Int64) context.Context {
	t.Helper()
	ctx := withReusableGitMetadataValidation(context.Background(), repo)
	before := walks.Load()
	if !gitMetadataSafeForSubprocessContext(ctx, repo) {
		t.Fatal("a safe repository was refused")
	}
	if walks.Load() != before {
		t.Fatal("an unchanged repository was walked again instead of reusing the read's walk")
	}
	return ctx
}

// requireRefusedAsToday checks that, after a change, a later check of the
// read walks again and refuses, as a fresh walk does.
func requireRefusedAsToday(t *testing.T, ctx context.Context, repo string, walks *atomic.Int64, change string) {
	t.Helper()
	if gitMetadataSafeForSubprocess(repo) {
		t.Fatalf("fixture error: a fresh walk accepts the repository after %s", change)
	}
	before := walks.Load()
	if gitMetadataSafeForSubprocessContext(ctx, repo) {
		t.Fatalf("after %s, a later check reused the read's walk and accepted the repository", change)
	}
	if walks.Load() == before {
		t.Fatalf("after %s, a later check answered without walking again", change)
	}
	if _, safe := refreshGitMetadataValidation(ctx, repo); safe {
		t.Fatalf("after %s, the source check reused the read's walk", change)
	}
}

func TestReusedGitMetadataWalkRefusesASymlinkSwappedInForADirectory(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	repo, gitDir := rootGitMetadataTreeFixture(t)
	writeFile(t, gitDir, "HEAD", "ref: refs/heads/main\n")
	ctx := reusableValidation(t, repo, walks)
	// refs becomes a symlink to a tree whose HEAD branch is a FIFO.
	target := filepath.Join(repo, "ref-target")
	writeGitMetadataFIFO(t, filepath.Join(target, "heads", "main"))
	if err := os.RemoveAll(filepath.Join(gitDir, "refs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(gitDir, "refs")); err != nil {
		t.Fatal(err)
	}
	requireRefusedAsToday(t, ctx, repo, walks, "refs was swapped for a symlink")
}

func TestReusedGitMetadataWalkRefusesASwapTwoLevelsDown(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	repo, gitDir := rootGitMetadataTreeFixture(t)
	if err := os.MkdirAll(filepath.Join(gitDir, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := reusableValidation(t, repo, walks)
	// objects/pack, below the object store's root, becomes a redirect to a
	// FIFO-holding directory: only objects/pack's own entry changes.
	target := filepath.Join(repo, "pack-target")
	writeGitMetadataFIFO(t, filepath.Join(target, "pack-"+strings.Repeat("b", 40)+".idx"))
	if err := os.Remove(filepath.Join(gitDir, "objects", "pack")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(gitDir, "objects", "pack")); err != nil {
		t.Fatal(err)
	}
	requireRefusedAsToday(t, ctx, repo, walks, "objects/pack was swapped for a symlink")
}

func TestReusedGitMetadataWalkRefusesAnEntryAddedToAWalkedDirectory(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	repo, gitDir := rootGitMetadataTreeFixture(t)
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "tags"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := reusableValidation(t, repo, walks)
	writeGitMetadataFIFO(t, filepath.Join(gitDir, "refs", "tags", "release"))
	requireRefusedAsToday(t, ctx, repo, walks, "a FIFO was added under refs/tags")
}

func TestReusedGitMetadataWalkRefusesAPermissionChange(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind the super-user")
	}
	walks := countGitMetadataWalks(t, 0)
	repo, gitDir := rootGitMetadataTreeFixture(t)
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := reusableValidation(t, repo, walks)
	refs := filepath.Join(gitDir, "refs")
	if err := os.Chmod(refs, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(refs, 0o755) })
	requireRefusedAsToday(t, ctx, repo, walks, "refs lost its permissions")
}

func TestReusedGitMetadataWalkRefusesAnInPlaceConfigChange(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	repo, gitDir := rootGitMetadataTreeFixture(t)
	writeFile(t, gitDir, "config", "[core]\n\tbare = false\n")
	ctx := reusableValidation(t, repo, walks)
	// Rewritten in place, same inode: only its content and times change.
	config, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.WriteString("[include]\n\tpath = /x\n"); err != nil {
		t.Fatal(err)
	}
	if err := config.Close(); err != nil {
		t.Fatal(err)
	}
	requireRefusedAsToday(t, ctx, repo, walks, "config was rewritten in place with an include")
}

func TestReusedGitMetadataWalkRefusesTheRepositoryReplaced(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	safeRepo, _ := rootGitMetadataTreeFixture(t)
	if err := os.Rename(safeRepo, repo); err != nil {
		t.Fatal(err)
	}
	ctx := reusableValidation(t, repo, walks)
	// The same path now names another repository, whose metadata is unsafe.
	unsafeRepo, unsafeGitDir := rootGitMetadataTreeFixture(t)
	writeGitMetadataFIFO(t, filepath.Join(unsafeGitDir, "shallow"))
	if err := os.Rename(repo, filepath.Join(parent, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(unsafeRepo, repo); err != nil {
		t.Fatal(err)
	}
	requireRefusedAsToday(t, ctx, repo, walks, "the repository was replaced")
}

func TestReusedGitMetadataWalkRefusesTheRepositoryPathRepointed(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	parent := t.TempDir()
	safeRepo, _ := rootGitMetadataTreeFixture(t)
	link := filepath.Join(parent, "repo")
	if err := os.Symlink(safeRepo, link); err != nil {
		t.Fatal(err)
	}
	ctx := reusableValidation(t, link, walks)
	unsafeRepo, unsafeGitDir := rootGitMetadataTreeFixture(t)
	writeGitMetadataFIFO(t, filepath.Join(unsafeGitDir, "shallow"))
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unsafeRepo, link); err != nil {
		t.Fatal(err)
	}
	requireRefusedAsToday(t, ctx, link, walks, "the repository's path was repointed")
}

func TestReusedGitMetadataWalkRefusesAGitDirectoryAppearingWhereNoneWas(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	repo, _ := rootGitMetadataTreeFixture(t)
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Discovery from sub finds no .git there and walks up to repo's.
	ctx := reusableValidation(t, sub, walks)
	unsafeRepo, unsafeGitDir := rootGitMetadataTreeFixture(t)
	writeGitMetadataFIFO(t, filepath.Join(unsafeGitDir, "shallow"))
	writeFile(t, sub, ".git", "gitdir: "+filepath.Join(unsafeRepo, ".git")+"\n")
	requireRefusedAsToday(t, ctx, sub, walks, "a .git pointer appeared where discovery had found none")
}

// A walk's answer is reused only well clear of the metadata's last change,
// and only for the repository it walked.
func TestReusedGitMetadataWalkStaysClearOfRecentChangesAndOtherRepositories(t *testing.T) {
	walks := countGitMetadataWalks(t, time.Hour)
	repo, _ := rootGitMetadataTreeFixture(t)
	ctx := withReusableGitMetadataValidation(context.Background(), repo)
	before := walks.Load()
	if !gitMetadataSafeForSubprocessContext(ctx, repo) || walks.Load() == before {
		t.Fatal("metadata changed within the racy window was not walked again")
	}

	gitMetadataRacyWindow = 0
	ctx = reusableValidation(t, repo, walks)
	other, otherGitDir := rootGitMetadataTreeFixture(t)
	writeGitMetadataFIFO(t, filepath.Join(otherGitDir, "shallow"))
	if gitMetadataSafeForSubprocessContext(ctx, other) {
		t.Fatal("a read's walk was reused for another repository")
	}
}

// The directory reader's stat of an entry is the same identity a stat of its
// path gives, which a later check compares against.
func TestGitMetadataFootprintDirectoryEntriesMatchTheirStat(t *testing.T) {
	repo, gitDir := rootGitMetadataTreeFixture(t)
	writeFile(t, gitDir, "config", "[core]\n")
	footprint := newGitMetadataFootprint(repo)
	if !gitMetadataSafeForSubprocessRecording(repo, footprint) {
		t.Fatal("a safe repository was refused")
	}
	resolvedGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config", "HEAD", "objects", "refs"} {
		path := filepath.Join(resolvedGitDir, name)
		entry, ok := footprint.entries[path]
		if !ok {
			t.Fatalf("the walk did not record %s; recorded %d paths", path, len(footprint.entries))
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := statIdentity(info)
		if entry.traversal || entry.id != id {
			t.Fatalf("%s recorded %+v (traversal %v), stat gives %+v", name, entry.id, entry.traversal, id)
		}
	}
	if footprint.broken {
		t.Fatal("a plain walk could not be recorded")
	}
}

// A walk on a file system that is not local is never reused, and the
// fixtures' own is.
func TestReusedGitMetadataWalkStaysOnLocalFileSystems(t *testing.T) {
	walks := countGitMetadataWalks(t, 0)
	repo, _ := rootGitMetadataTreeFixture(t)
	if !localFileSystem(repo) {
		t.Fatalf("the test's temporary directory %s does not count as local", repo)
	}
	previous := gitMetadataLocalFileSystem
	gitMetadataLocalFileSystem = func(string) bool { return false }
	t.Cleanup(func() { gitMetadataLocalFileSystem = previous })
	ctx := withReusableGitMetadataValidation(context.Background(), repo)
	before := walks.Load()
	if !gitMetadataSafeForSubprocessContext(ctx, repo) || walks.Load() == before {
		t.Fatal("a walk on a file system that is not local was reused")
	}
}

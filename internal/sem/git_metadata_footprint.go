package sem

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// gitMetadataFootprint is everything one walk of gitMetadataSafeForSubprocess
// looked at: each path it stat'ed, opened or listed, and each it found
// missing, with what it found. A later check in the same read may take the
// walk's answer instead of walking again, but only when every one of those
// paths still stats as the walk saw it (intact): the walk's answer is a
// function of what it looked at, so the same observations give the same
// answer.
//
// A directory the walk only passed through on the way to another path is
// held to its identity, type, permissions and owner, not its change time:
// its other entries were never the walk's to judge, and the next path along
// is recorded on its own. Everything else the walk examined is held to its
// change time and size too, which any write, rename into, removal from,
// permission or owner change moves; a directory's change time moves when an
// entry is added to or removed from it.
//
// A change time is trusted only well clear of the walk: the walk's answer is
// not reused when any examined path changed less than gitMetadataRacyWindow
// before the walk began (a change within the same clock tick could leave it
// unmoved), or when no recorded change time has a sub-second part (a
// file system whose ticks are whole seconds or coarser), or when any path it
// looked at is on a file system that is not local (localFileSystem).
type gitMetadataFootprint struct {
	repo     string
	resolved string
	started  time.Time
	entries  map[string]gitMetadataFootprintEntry
	// broken is set by an observation that could not be recorded exactly; a
	// broken footprint is never reused.
	broken  bool
	precise bool
	trusted bool
}

type gitMetadataFootprintState uint8

const (
	gitMetadataFootprintPresent gitMetadataFootprintState = iota
	gitMetadataFootprintNotExist
	gitMetadataFootprintNotDirectory
)

type gitMetadataFootprintEntry struct {
	state     gitMetadataFootprintState
	traversal bool
	id        fileStatIdentity
}

// fileStatIdentity is what a stat says of a path that the walk's answer can
// depend on.
type fileStatIdentity struct {
	device, inode       uint64
	mode, uid, gid      uint32
	size                int64
	ctimeSec, ctimeNsec int64
}

// The file-type bits of st_mode, as POSIX numbers them: a footprint's mode
// is only ever a darwin or Linux st_mode (statIdentity).
const (
	statTypeMask      = 0o170000
	statTypeDirectory = 0o040000
	statTypeRegular   = 0o100000
)

func (id fileStatIdentity) sameTraversal(other fileStatIdentity) bool {
	return id.device == other.device && id.inode == other.inode && id.mode == other.mode && id.uid == other.uid && id.gid == other.gid
}

// gitMetadataRacyWindow is how long before a walk an examined path's last
// change must be for the walk's answer to be reused.
var gitMetadataRacyWindow = 2 * time.Second

// gitMetadataLocalFileSystem is localFileSystem, replaceable by tests.
var gitMetadataLocalFileSystem = localFileSystem

// gitMetadataWalkObserved, when set (by tests), is called at each full walk.
var gitMetadataWalkObserved func()

func newGitMetadataFootprint(repo string) *gitMetadataFootprint {
	footprint := &gitMetadataFootprint{repo: repo, started: time.Now(), entries: map[string]gitMetadataFootprintEntry{}}
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		footprint.broken = true
	}
	footprint.resolved = resolved
	return footprint
}

func (f *gitMetadataFootprint) recordInfo(path string, info os.FileInfo, traversal bool) {
	if f == nil {
		return
	}
	id, ok := statIdentity(info)
	if !ok {
		f.broken = true
		return
	}
	f.record(path, gitMetadataFootprintEntry{state: gitMetadataFootprintPresent, traversal: traversal, id: id})
}

func (f *gitMetadataFootprint) recordIdentity(path string, id fileStatIdentity) {
	if f == nil {
		return
	}
	f.record(path, gitMetadataFootprintEntry{state: gitMetadataFootprintPresent, id: id})
}

// recordLookupError records a lookup of path that failed with err: missing
// (not there, or a parent not a directory) as such, anything else as an
// observation the footprint cannot hold.
func (f *gitMetadataFootprint) recordLookupError(path string, err error) {
	if f == nil {
		return
	}
	switch {
	case errors.Is(err, syscall.ENOTDIR):
		f.record(path, gitMetadataFootprintEntry{state: gitMetadataFootprintNotDirectory})
	case errors.Is(err, fs.ErrNotExist):
		f.record(path, gitMetadataFootprintEntry{state: gitMetadataFootprintNotExist})
	default:
		f.broken = true
	}
}

// recordUnrecordable marks an observation the footprint cannot hold.
func (f *gitMetadataFootprint) recordUnrecordable() {
	if f != nil {
		f.broken = true
	}
}

func (f *gitMetadataFootprint) record(path string, entry gitMetadataFootprintEntry) {
	path = filepath.Clean(path)
	existing, seen := f.entries[path]
	if !seen {
		f.entries[path] = entry
		return
	}
	// The same path seen twice in one walk must have stood still between.
	if existing.state != entry.state {
		f.broken = true
		return
	}
	if existing.state != gitMetadataFootprintPresent {
		return
	}
	switch {
	case existing.traversal && entry.traversal:
		if !existing.id.sameTraversal(entry.id) {
			f.broken = true
		}
	case existing.traversal || entry.traversal:
		if !existing.id.sameTraversal(entry.id) {
			f.broken = true
			return
		}
		if existing.traversal {
			f.entries[path] = gitMetadataFootprintEntry{state: gitMetadataFootprintPresent, id: entry.id}
		}
	default:
		if existing.id != entry.id {
			f.broken = true
		}
	}
}

// finish settles, once the walk has answered, whether its footprint can be
// trusted.
func (f *gitMetadataFootprint) finish() bool {
	if f.broken {
		return false
	}
	cutoff := f.started.Add(-gitMetadataRacyWindow)
	// A stat on a network or user-space file system may be answered from a
	// client cache that opening a path would revalidate, so a re-stat there
	// is no fresh observation: only a footprint wholly on local file systems
	// is trusted.
	devices := map[uint64]bool{}
	for _, entry := range f.entries {
		if entry.state == gitMetadataFootprintPresent {
			devices[entry.id.device] = false
		}
	}
	for path, entry := range f.entries {
		// The file system is asked of a directory or regular file that still
		// stats on the recorded device, never of a link that statfs would
		// follow elsewhere.
		kind := entry.id.mode & statTypeMask
		if entry.state != gitMetadataFootprintPresent || devices[entry.id.device] || kind != statTypeDirectory && kind != statTypeRegular {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return false
		}
		if id, ok := statIdentity(info); !ok || id.device != entry.id.device || !gitMetadataLocalFileSystem(path) {
			return false
		}
		devices[entry.id.device] = true
	}
	for _, local := range devices {
		if !local {
			return false
		}
	}
	for _, entry := range f.entries {
		if entry.state != gitMetadataFootprintPresent || entry.traversal {
			continue
		}
		if entry.id.ctimeNsec != 0 {
			f.precise = true
		}
		if !time.Unix(entry.id.ctimeSec, entry.id.ctimeNsec).Before(cutoff) {
			return false
		}
	}
	f.trusted = f.precise
	return f.trusted
}

// intact reports whether every path the walk looked at still stats as the
// walk saw it, and the repository still resolves where it did.
func (f *gitMetadataFootprint) intact() bool {
	if f == nil || !f.trusted {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(f.repo); err != nil || resolved != f.resolved {
		return false
	}
	for path, entry := range f.entries {
		info, err := os.Lstat(path)
		switch entry.state {
		case gitMetadataFootprintNotExist:
			if err == nil || errors.Is(err, syscall.ENOTDIR) || !errors.Is(err, fs.ErrNotExist) {
				return false
			}
		case gitMetadataFootprintNotDirectory:
			if err == nil || !errors.Is(err, syscall.ENOTDIR) {
				return false
			}
		default:
			if err != nil {
				return false
			}
			id, ok := statIdentity(info)
			if !ok {
				return false
			}
			if entry.traversal {
				if !entry.id.sameTraversal(id) {
					return false
				}
			} else if entry.id != id {
				return false
			}
		}
	}
	return true
}

// recordedGitMetadataValidation is gitMetadataSafeForSubprocess's answer for
// an absolute repository, and, when that answer is safe and the walk's
// footprint can be trusted, the footprint.
func recordedGitMetadataValidation(absRepo string) (bool, *gitMetadataFootprint) {
	footprint := newGitMetadataFootprint(absRepo)
	safe := gitMetadataSafeForSubprocessRecording(absRepo, footprint)
	if !safe || !footprint.finish() {
		return safe, nil
	}
	return true, footprint
}

// withReusableGitMetadataValidation walks repo's metadata once for the read
// that ctx belongs to and returns a context whose later checks of the same
// repository take that walk's answer while its footprint is intact
// (gitMetadataSafeForSubprocessContext), walking again whenever it is not. It
// adds nothing when the walk refuses or its footprint cannot be trusted:
// each later check then walks as it would have. The receipt lives in ctx
// only, so it never outlasts the read or reaches another.
func withReusableGitMetadataValidation(ctx context.Context, repo string) context.Context {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return ctx
	}
	absRepo = filepath.Clean(absRepo)
	if receipt, ok := ctx.Value(gitMetadataValidationContextKey{}).(gitMetadataValidationReceipt); ok && receipt.repo == absRepo && receipt.footprint != nil && receipt.footprint.intact() {
		return ctx
	}
	safe, footprint := recordedGitMetadataValidation(absRepo)
	if !safe || footprint == nil {
		return ctx
	}
	return context.WithValue(ctx, gitMetadataValidationContextKey{}, gitMetadataValidationReceipt{repo: absRepo, safe: true, footprint: footprint})
}

// refreshGitMetadataValidation is newGitMetadataValidation, except that this
// read's own reusable walk answers while its footprint is intact.
func refreshGitMetadataValidation(ctx context.Context, repo string) (context.Context, bool) {
	if absRepo, err := filepath.Abs(repo); err == nil {
		if receipt, ok := ctx.Value(gitMetadataValidationContextKey{}).(gitMetadataValidationReceipt); ok && receipt.repo == filepath.Clean(absRepo) && receipt.footprint != nil && receipt.footprint.intact() {
			return ctx, true
		}
	}
	return newGitMetadataValidation(ctx, repo)
}

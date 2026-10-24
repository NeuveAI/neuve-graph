//go:build cgo && !windows

package sem

/*
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <sys/stat.h>
#include <sys/types.h>

enum {
	entire_graph_metadata_special = 0,
	entire_graph_metadata_regular = 1,
	entire_graph_metadata_directory = 2,
	entire_graph_metadata_redirect = 3,
	entire_graph_metadata_socket = 4
};

static int entire_graph_fstatat(int fd, const char *name, struct stat *result) {
	if (fstatat(fd, name, result, AT_SYMLINK_NOFOLLOW) == 0) {
		return 0;
	}
	return errno;
}

static int entire_graph_metadata_kind(const struct stat *info) {
	if (S_ISREG(info->st_mode)) return entire_graph_metadata_regular;
	if (S_ISDIR(info->st_mode)) return entire_graph_metadata_directory;
	if (S_ISLNK(info->st_mode)) return entire_graph_metadata_redirect;
	if (S_ISSOCK(info->st_mode)) return entire_graph_metadata_socket;
	return entire_graph_metadata_special;
}

static uint64_t entire_graph_metadata_device(const struct stat *info) {
	return (uint64_t)info->st_dev;
}

static uint64_t entire_graph_metadata_inode(const struct stat *info) { return (uint64_t)info->st_ino; }
static uint32_t entire_graph_metadata_mode(const struct stat *info) { return (uint32_t)info->st_mode; }
static uint32_t entire_graph_metadata_uid(const struct stat *info) { return (uint32_t)info->st_uid; }
static uint32_t entire_graph_metadata_gid(const struct stat *info) { return (uint32_t)info->st_gid; }
static int64_t entire_graph_metadata_size(const struct stat *info) { return (int64_t)info->st_size; }

// The change time, where a walk's footprint can be trusted (darwin and
// Linux, as statIdentity); zero elsewhere, where none ever is.
#if defined(__APPLE__)
static int64_t entire_graph_metadata_ctime_sec(const struct stat *info) { return (int64_t)info->st_ctimespec.tv_sec; }
static int64_t entire_graph_metadata_ctime_nsec(const struct stat *info) { return (int64_t)info->st_ctimespec.tv_nsec; }
#elif defined(__linux__)
static int64_t entire_graph_metadata_ctime_sec(const struct stat *info) { return (int64_t)info->st_ctim.tv_sec; }
static int64_t entire_graph_metadata_ctime_nsec(const struct stat *info) { return (int64_t)info->st_ctim.tv_nsec; }
#else
static int64_t entire_graph_metadata_ctime_sec(const struct stat *info) { (void)info; return 0; }
static int64_t entire_graph_metadata_ctime_nsec(const struct stat *info) { (void)info; return 0; }
#endif
*/
import "C"

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// readGitMetadataDirectory enumerates names from the held directory descriptor,
// runs the mount preflight, then uses fstatat(AT_SYMLINK_NOFOLLOW) against that
// same descriptor. It therefore performs no repository-controlled pathname
// lookup before the mount decision and never follows a child redirect merely to
// learn its type.
func readGitMetadataDirectory(
	directory *os.File,
	resolved string,
	resolver *sameVolumePathResolver,
	count int,
	admit func(string) bool,
) ([]gitMetadataDirectoryEntry, error) {
	names, readErr := directory.Readdirnames(count)
	entries := make([]gitMetadataDirectoryEntry, 0, len(names))
	for _, name := range names {
		child := filepath.Join(resolved, name)
		if !admit(child) {
			return nil, errGitMetadataTreeBound
		}
		if err := resolver.beforeLookup(child); err != nil {
			return nil, err
		}
		encoded := append([]byte(name), 0)
		var info C.struct_stat
		errno := C.entire_graph_fstatat(
			C.int(directory.Fd()),
			(*C.char)(unsafe.Pointer(&encoded[0])),
			&info,
		)
		runtime.KeepAlive(directory)
		if errno != 0 {
			err := syscall.Errno(errno)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, &os.PathError{Op: "fstatat", Path: child, Err: err}
		}
		if uint64(C.entire_graph_metadata_device(&info)) != resolver.anchor.device {
			return nil, errSymlinkChainOffVolume
		}
		resolver.footprint.recordIdentity(child, fileStatIdentity{
			device:    uint64(C.entire_graph_metadata_device(&info)),
			inode:     uint64(C.entire_graph_metadata_inode(&info)),
			mode:      uint32(C.entire_graph_metadata_mode(&info)),
			uid:       uint32(C.entire_graph_metadata_uid(&info)),
			gid:       uint32(C.entire_graph_metadata_gid(&info)),
			size:      int64(C.entire_graph_metadata_size(&info)),
			ctimeSec:  int64(C.entire_graph_metadata_ctime_sec(&info)),
			ctimeNsec: int64(C.entire_graph_metadata_ctime_nsec(&info)),
		})
		entries = append(entries, gitMetadataDirectoryEntry{
			name: name,
			kind: gitMetadataEntryKind(C.entire_graph_metadata_kind(&info)),
		})
	}
	return entries, readErr
}

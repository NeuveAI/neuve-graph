//go:build darwin

package sem

import (
	"os"
	"syscall"
)

func statIdentity(info os.FileInfo) (fileStatIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStatIdentity{}, false
	}
	return fileStatIdentity{
		device: uint64(stat.Dev), inode: stat.Ino,
		mode: uint32(stat.Mode), uid: stat.Uid, gid: stat.Gid,
		size:     stat.Size,
		ctimeSec: stat.Ctimespec.Sec, ctimeNsec: stat.Ctimespec.Nsec,
	}, true
}

// mntLocal is MNT_LOCAL: the file system is stored locally.
const mntLocal = 0x00001000

// localFileSystemTypes are the darwin file systems whose stats the kernel
// answers itself: a user-space file system can claim MNT_LOCAL and still
// serve stats from its own cache.
var localFileSystemTypes = map[string]bool{"apfs": true, "hfs": true}

// localFileSystem reports whether path's file system is stored locally and
// is one whose stats this machine's kernel answers rather than a cache.
func localFileSystem(path string) bool {
	var stat syscall.Statfs_t
	if syscall.Statfs(path, &stat) != nil || stat.Flags&mntLocal == 0 {
		return false
	}
	name := make([]byte, 0, len(stat.Fstypename))
	for _, c := range stat.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return localFileSystemTypes[string(name)]
}

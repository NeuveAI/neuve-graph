//go:build linux

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
		device: uint64(stat.Dev), inode: uint64(stat.Ino),
		mode: uint32(stat.Mode), uid: stat.Uid, gid: stat.Gid,
		size:     stat.Size,
		ctimeSec: int64(stat.Ctim.Sec), ctimeNsec: int64(stat.Ctim.Nsec),
	}, true
}

// localFileSystemTypes are the statfs magic numbers of Linux file systems
// whose stats the kernel answers from the file system itself: no network or
// user-space file system (NFS, SMB, FUSE, 9p) is among them.
// (Nor overlayfs, whose layers may themselves be on one.)
var localFileSystemTypes = map[uint32]bool{
	0xEF53:     true, // ext2, ext3, ext4
	0x58465342: true, // xfs
	0x9123683E: true, // btrfs
	0x01021994: true, // tmpfs
	0xF2F52010: true, // f2fs
	0x2FC12FC1: true, // zfs
}

// localFileSystem reports whether path's file system is one whose stats the
// kernel answers itself rather than from a client cache.
func localFileSystem(path string) bool {
	var stat syscall.Statfs_t
	return syscall.Statfs(path, &stat) == nil && localFileSystemTypes[uint32(stat.Type)]
}

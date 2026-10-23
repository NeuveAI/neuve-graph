//go:build !windows

package sem

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockCacheStoreFile takes one of the store's locks, waiting for it or not.
func lockCacheStoreFile(root *os.Root, name string, wait bool) (func(), error) {
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		file.Close()
		return nil, err
	}
	return func() { file.Close() }, nil
}

// lockArtifactForRemoval takes an artifact for removal unless a read holds
// it (reads hold a shared lock on the file they read: holdArtifactForRead) or
// a writer published another file under its name since the prune looked. An
// artifact that cannot be opened is held by no read.
func lockArtifactForRemoval(root *os.Root, name string) (func(), bool) {
	file, err := openRootBoundedRegularFile(root, name)
	if err != nil {
		return func() {}, true
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, false
	}
	if !stillNamed(root, name, file) {
		file.Close()
		return nil, false
	}
	return func() { file.Close() }, true
}

// holdArtifactForRead keeps the store from removing an artifact while it is
// read; closing the file lets go. It reports whether the file opened is still
// the one the name holds: a prune may have removed it, or a writer replaced
// it, between the open and the lock. A prune holds its lock only to remove a
// few files; should one stall, the read goes on without the lock rather than
// wait, since a removed file stays readable through its open handle.
func holdArtifactForRead(root *os.Root, name string, file *os.File) bool {
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			break
		}
	}
	return stillNamed(root, name, file)
}

func identityOf(info os.FileInfo) FileIdentity {
	identity := FileIdentity{Present: true, Size: info.Size(), ModTime: info.ModTime().UnixNano()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity.Device, identity.Inode = uint64(stat.Dev), uint64(stat.Ino)
	}
	return identity
}

func stillNamed(root *os.Root, name string, file *os.File) bool {
	opened, err := file.Stat()
	if err != nil {
		return false
	}
	// Stat, not Lstat: the name may be a link a read follows.
	named, err := root.Stat(name)
	return err == nil && os.SameFile(opened, named)
}

// holdExecutableLock holds a shared lock on the running executable for the
// process's life, which keeps a prune from removing it (lockArtifactForRemoval
// takes an exclusive one).
func holdExecutableLock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
}

// freeDiskBytes is the space left to this user on dir's file system, -1 when
// unknown.
func freeDiskBytes(dir string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return -1
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}

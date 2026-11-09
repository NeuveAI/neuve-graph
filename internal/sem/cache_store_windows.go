//go:build windows

package sem

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func lockCacheStoreFile(root *os.Root, name string, wait bool) (func(), error) {
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &windows.Overlapped{}); err != nil {
		file.Close()
		return nil, err
	}
	return func() { file.Close() }, nil
}

func lockArtifactForRemoval(root *os.Root, name string) (func(), bool) {
	file, err := openRootBoundedRegularFile(root, name)
	if err != nil {
		return func() {}, true
	}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		file.Close()
		return nil, false
	}
	if !stillNamed(root, name, file) {
		file.Close()
		return nil, false
	}
	return func() { file.Close() }, true
}

func holdArtifactForRead(root *os.Root, name string, file *os.File) bool {
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		// Shared byte-range locks permit reads through this and other handles.
		err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
		if err == nil {
			return stillNamed(root, name, file)
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) || time.Now().After(deadline) {
			// Unlike flock, an unacquired Windows lock cannot be ignored:
			// the remover's mandatory exclusive lock would deny this read.
			return false
		}
	}
}

func stillNamed(root *os.Root, name string, file *os.File) bool {
	opened, err := file.Stat()
	if err != nil {
		return false
	}
	named, err := root.Stat(name)
	return err == nil && os.SameFile(opened, named)
}

func removeLockedArtifact(root *os.Root, name string, unlock func()) error {
	// Retire the name while exclusively locked, so no new reader is admitted
	// between releasing the mandatory lock and deleting the file. A crash
	// leaves a dot-prefixed temporary that the store's stale-file scan collects.
	suffix := ".json.gz"
	if strings.HasPrefix(filepath.ToSlash(name), cacheExecutables+"/") {
		suffix = ".partial"
	}
	temporary := filepath.Join(filepath.Dir(name), ".prune-"+cacheTempNameSuffix(rand.Uint64)+suffix)
	if err := root.Rename(name, temporary); err != nil {
		return err
	}
	unlock()
	return root.Remove(temporary)
}

func holdExecutableLock(file *os.File) {
	_ = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func freeDiskBytes(string) int64 { return -1 }

func identityOf(file *os.File, info os.FileInfo) FileIdentity {
	var data windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &data); err != nil {
		return FileIdentity{}
	}
	return FileIdentity{
		Present: true,
		Device:  uint64(data.VolumeSerialNumber),
		Inode:   uint64(data.FileIndexHigh)<<32 | uint64(data.FileIndexLow),
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
	}
}

func identifyFile(path string) FileIdentity {
	file, err := os.Open(path)
	if err != nil {
		return FileIdentity{}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return FileIdentity{}
	}
	return identityOf(file, info)
}

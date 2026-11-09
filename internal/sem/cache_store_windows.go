//go:build windows

package sem

import (
	"errors"
	"os"
	"path/filepath"
	"time"
	"unsafe"

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

func lockArtifactForRemoval(root *os.Root, name string) (*cacheArtifactRemoval, bool) {
	file, err := openCacheArtifactForRemoval(root, name)
	if err != nil {
		if os.IsNotExist(err) {
			return &cacheArtifactRemoval{unlock: func() {}, remove: func() (int64, error) { return 0, nil }}, true
		}
		return nil, false
	}
	handle := windows.Handle(file.Fd())
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		file.Close()
		return nil, false
	}
	if !stillNamed(root, name, file) {
		file.Close()
		return nil, false
	}
	return &cacheArtifactRemoval{
		unlock: func() { file.Close() },
		remove: func() (int64, error) {
			info, err := file.Stat()
			if err != nil {
				return 0, err
			}
			// Retirement is bound to the locked object even if publication
			// replaced its name while the other unit locks were acquired.
			if err := retireCacheWindowsHandle(handle); err != nil {
				return 0, err
			}
			// POSIX disposition unlinks on this handle's close. Classic
			// disposition stays delete-pending until all handles close.
			return info.Size(), file.Close()
		},
	}, true
}

func openCacheArtifactForRemoval(root *os.Root, name string) (*os.File, error) {
	parent, err := root.OpenRoot(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	directory, err := parent.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	leaf, err := windows.NewNTUnicodeString(filepath.Base(name))
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(directory.Fd()),
		ObjectName:    leaf,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ|windows.DELETE|windows.FILE_WRITE_ATTRIBUTES, &attributes,
		&windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		if status, ok := err.(windows.NTStatus); ok {
			err = status.Errno()
		}
		return nil, &os.PathError{Op: "open artifact for removal", Path: name, Err: err}
	}
	// The parent is root-confined and the leaf cannot traverse a reparse
	// point. DELETE access and read access support disposition and locking.
	return os.NewFile(uintptr(handle), name), nil
}

// cacheWindowsStandardInfo matches Microsoft's FILE_STANDARD_INFO ABI.
type cacheWindowsStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  byte
	Directory      byte
}

func cacheWindowsStandardInformation(handle windows.Handle) (cacheWindowsStandardInfo, error) {
	var info cacheWindowsStandardInfo
	err := windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return info, err
}

func retireCacheWindowsHandle(handle windows.Handle) error {
	info, err := cacheWindowsStandardInformation(handle)
	if err != nil {
		return err
	}
	if info.NumberOfLinks == 0 || info.DeletePending != 0 {
		// Replacing a name can already have unlinked this object. Closing
		// the locked handle completes retirement without touching the new name.
		return nil
	}
	var data windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &data); err != nil {
		return err
	}
	if data.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		// Preserve os.Root.Remove's ability to retire read-only engine copies.
		// This is FILE_BASIC_INFO, with zero timestamps meaning no change.
		var basic struct {
			CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
			FileAttributes                                          uint32
		}
		basic.FileAttributes = data.FileAttributes &^ windows.FILE_ATTRIBUTE_READONLY
		if basic.FileAttributes == 0 {
			basic.FileAttributes = windows.FILE_ATTRIBUTE_NORMAL
		}
		if err := windows.SetFileInformationByHandle(handle, windows.FileBasicInfo,
			(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
			return err
		}
	}
	// Microsoft's FILE_DISPOSITION_INFORMATION_EX documentation specifies
	// DELETE access and POSIX unlink semantics. Set disposition on the locked
	// handle, never on a name that a writer can replace in the meantime.
	flags := uint32(0x1 | 0x2) // FILE_DISPOSITION_FLAG_DELETE | FILE_DISPOSITION_FLAG_POSIX_SEMANTICS
	err = windows.SetFileInformationByHandle(handle, windows.FileDispositionInfoEx,
		(*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_INVALID_LEVEL) {
		// Older Windows/filesystems can only mark the object delete-pending.
		// New opens fail; existing readers recheck DeletePending below.
		pending := byte(1)
		return windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &pending, 1)
	}
	return err
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
	// A reader may have opened just before classic delete disposition. It
	// must not admit that object after the remover closes its locked handle.
	standard, err := cacheWindowsStandardInformation(windows.Handle(file.Fd()))
	if err != nil || standard.DeletePending != 0 {
		return false
	}
	opened, err := file.Stat()
	if err != nil {
		return false
	}
	named, err := root.Stat(name)
	return err == nil && os.SameFile(opened, named)
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

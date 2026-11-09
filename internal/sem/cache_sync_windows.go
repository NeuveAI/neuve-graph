//go:build windows

package sem

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var cacheFlushFileBuffers = windows.FlushFileBuffers

func syncCacheDirectory(directory *os.Root) error {
	file, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	admitted := windows.Handle(file.Fd())
	path, err := cacheWindowsHandlePath(admitted)
	if err != nil {
		// Some filesystems cannot supply a final DOS path. Fall back to the
		// root's original name, still requiring the identity check below.
		path = directory.Name()
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// Microsoft's FlushFileBuffers documentation requires GENERIC_WRITE;
	// NtFlushBuffersFileEx documents directory flushes. Use the CreateFile
	// directory open used by etcd, with FILE_FLAG_BACKUP_SEMANTICS.
	// CI's directory opened by os.OpenRoot cannot be renamed: that path-based
	// open omits FILE_SHARE_DELETE. Root-relative child opens do share delete,
	// so the identity check still matters, even with a handle-derived path.
	// It also protects the fallback to directory.Name().
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &os.PathError{Op: "open directory for sync", Path: path, Err: err}
	}
	defer windows.CloseHandle(handle)
	if err := verifyCacheWindowsObject(admitted, handle); err != nil {
		return &os.PathError{Op: "verify directory for sync", Path: path, Err: err}
	}
	if err := cacheFlushFileBuffers(handle); err != nil {
		return &os.PathError{Op: "sync", Path: path, Err: err}
	}
	return nil
}

func cacheWindowsHandlePath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 260)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:n]), nil
		}
		buffer = make([]uint16, n+1)
	}
}

func verifyCacheWindowsObject(admitted, opened windows.Handle) error {
	var before, after windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(admitted, &before); err != nil {
		return err
	}
	if err := windows.GetFileInformationByHandle(opened, &after); err != nil {
		return err
	}
	if before.VolumeSerialNumber != after.VolumeSerialNumber ||
		before.FileIndexHigh != after.FileIndexHigh || before.FileIndexLow != after.FileIndexLow {
		return fmt.Errorf("opened object differs from admitted object")
	}
	return nil
}

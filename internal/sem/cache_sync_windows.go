//go:build windows

package sem

import (
	"os"

	"golang.org/x/sys/windows"
)

var cacheReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

func syncCacheDirectory(directory *os.Root) error {
	file, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	// Microsoft documents GENERIC_WRITE for FlushFileBuffers and directory
	// flushes in NtFlushBuffersFileEx. ReOpenFile with BACKUP_SEMANTICS keeps
	// this flush on the admitted directory object, even if it was renamed.
	// x/sys/windows does not expose ReOpenFile; this matches its Win32 ABI.
	handle, _, callErr := cacheReOpenFile.Call(file.Fd(), uintptr(windows.GENERIC_WRITE),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE),
		uintptr(windows.FILE_FLAG_BACKUP_SEMANTICS))
	if windows.Handle(handle) == windows.InvalidHandle {
		return &os.PathError{Op: "ReOpenFile", Path: file.Name(), Err: callErr}
	}
	defer windows.CloseHandle(windows.Handle(handle))
	if err := windows.FlushFileBuffers(windows.Handle(handle)); err != nil {
		return &os.PathError{Op: "sync", Path: file.Name(), Err: err}
	}
	return nil
}

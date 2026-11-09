//go:build !windows

package sem

import (
	"fmt"
	"os"
	"syscall"
)

func cacheFileIdentity(_ *os.File, info os.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d:%d", stat.Dev, stat.Ino, info.Size(), info.ModTime().UnixNano()), true
}

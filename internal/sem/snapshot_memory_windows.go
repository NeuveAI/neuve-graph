//go:build windows

package sem

import (
	"fmt"
	"os"
)

func cacheFileIdentity(file *os.File, info os.FileInfo) (string, bool) {
	identity := identityOf(file, info)
	if !identity.Present {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d:%d", identity.Device, identity.Inode, identity.Size, identity.ModTime), true
}

//go:build windows

package sem

import "os"

// Windows has no Graph service, so snapshots are never remembered there.
func cacheFileIdentity(os.FileInfo) (string, bool) { return "", false }

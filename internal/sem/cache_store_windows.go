//go:build windows

package sem

import "os"

// Windows has no flock: reads do not pin what they read, and the store's
// locks do not exclude one another. A file a read holds open can still be
// removed (Go opens files with FILE_SHARE_DELETE), and the read completes
// through its handle; concurrent use-log writes can lose a use, which only
// affects the least-recently-used order.
func lockCacheStoreFile(*os.Root, string, bool) (func(), error) { return func() {}, nil }

func lockArtifactForRemoval(*os.Root, string) (func(), bool) { return func() {}, true }

func holdArtifactForRead(*os.Root, string, *os.File) bool { return true }

func holdExecutableLock(*os.File) {}

func freeDiskBytes(string) int64 { return -1 }

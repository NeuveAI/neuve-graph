//go:build !darwin && !linux

package sem

import "os"

// statIdentity is unavailable here, so no walk's footprint is ever trusted
// and every check walks.
func statIdentity(os.FileInfo) (fileStatIdentity, bool) { return fileStatIdentity{}, false }

func localFileSystem(string) bool { return false }

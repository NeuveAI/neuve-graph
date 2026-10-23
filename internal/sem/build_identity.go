package sem

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
)

// ExecutableBuildIdentity identifies the native executable's contents. A process
// hashes its executable once, including when the reported version is "dev".
func ExecutableBuildIdentity() (string, error) {
	return executableBuildIdentity()
}

var executableBuildIdentity = sync.OnceValues(func() (string, error) {
	hash := sha256.New()
	if held := heldExecutableFile(); held != nil {
		// The file this process holds: its bytes even if the name it was
		// started from has since been removed.
		info, err := held.Stat()
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(hash, io.NewSectionReader(held, 0, info.Size())); err != nil {
			return "", err
		}
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
})

var heldExecutable struct {
	once sync.Once
	file *os.File
}

// HoldExecutable opens this process's executable for the rest of its life and,
// where flock exists, holds a shared lock on it: the cache store never
// removes an engine copy a running Graph process holds (cache_store.go), and
// the build identity is read from the file held, not from a name that may
// since have gone. Cheap: an open and a lock, no read.
func HoldExecutable() {
	heldExecutable.once.Do(func() {
		path, err := os.Executable()
		if err != nil {
			return
		}
		file, err := os.Open(path)
		if err != nil {
			return
		}
		holdExecutableLock(file)
		heldExecutable.file = file
	})
}

func heldExecutableFile() *os.File {
	heldExecutable.once.Do(func() {}) // a hold not taken by now is not taken
	return heldExecutable.file
}

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
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
})

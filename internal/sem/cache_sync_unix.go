//go:build !windows

package sem

import "os"

func syncCacheDirectory(directory *os.Root) error {
	file, err := directory.Open(".")
	if err != nil {
		return err
	}
	err = file.Sync()
	_ = file.Close()
	return err
}

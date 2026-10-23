//go:build !windows

package textindex

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// lockBuild waits for the store's build lock, an advisory lock the kernel
// drops if its holder dies, so two processes never index the same blobs and
// a dead builder never strands the next.
func lockBuild(ctx context.Context, file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

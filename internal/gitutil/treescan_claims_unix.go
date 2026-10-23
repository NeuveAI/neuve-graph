//go:build !windows

package gitutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// A claim is an advisory lock on claims/<pattern> that ends when its file is
// closed, which the kernel also does when the holding process dies: no holder
// can strand its waiters and no lock is timed. It is how one process computes
// a pattern's answers while another that needs them waits and reads them
// (the computed-once rule), until the machine's budget host holds such claims.
type answerClaim struct{ file *os.File }

// tryClaim takes the claim on pattern without waiting. held reports that
// another process holds it; any failure to claim (no store, an unwritable
// directory, a filesystem without locks) leaves the pattern to this process.
func (s *TreeScanAnswers) tryClaim(pattern string) (claim *answerClaim, held bool) {
	if s == nil {
		return nil, false
	}
	file, err := s.openClaim(pattern)
	if err != nil {
		return nil, false
	}
	if err := flockRetry(file, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.Is(err, syscall.EWOULDBLOCK)
	}
	return &answerClaim{file: file}, false
}

// awaitClaim returns once no process holds the claim on pattern, reporting
// whether it had to wait. Only cancellation is an error: a claim that cannot
// be observed is treated as released, and the caller computes what is left.
func (s *TreeScanAnswers) awaitClaim(ctx context.Context, pattern string) (bool, error) {
	if s == nil {
		return false, nil
	}
	file, err := s.openClaim(pattern)
	if err != nil {
		return false, nil
	}
	defer file.Close()
	for waited := false; ; waited = true {
		err := flockRetry(file, syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) {
			return waited, nil
		}
		select {
		case <-ctx.Done():
			return waited, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func flockRetry(file *os.File, how int) error {
	for {
		err := syscall.Flock(int(file.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func (c *answerClaim) release() {
	if c != nil && c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
}

func (s *TreeScanAnswers) openClaim(pattern string) (*os.File, error) {
	path := filepath.Join(s.dir, "claims", patternKey(pattern))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}

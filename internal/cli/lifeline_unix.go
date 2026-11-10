//go:build !windows

package cli

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openLifeline takes ownership of the opt-in caller connection. Remove its
// environment entry before any subprocess can start, even when it is invalid.
func openLifeline() (*os.File, error) {
	const name = "ENTIRE_GRAPH_LIFELINE_FD"
	raw, present := os.LookupEnv(name)
	_ = os.Unsetenv(name)
	if !present {
		return nil, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 0 {
		return nil, fmt.Errorf("%s must name an open non-negative file descriptor, got %q", name, raw)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return nil, fmt.Errorf("%s=%s: %w", name, raw, err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return nil, fmt.Errorf("%s=%s: %w", name, raw, err)
	}
	// NewFile registers a nonblocking pipe with Go's poller, so Close also
	// releases the reader when an ordinary command finishes before EOF.
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, fmt.Errorf("%s=%s: %w", name, raw, err)
	}
	return os.NewFile(uintptr(fd), "Graph caller lifeline"), nil
}

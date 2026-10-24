//go:build windows

package cli

import (
	"context"
	"errors"
	"time"
)

// Windows has no shared Graph service: every command runs in its own process.
func forwardToService(context.Context, []string) (bool, string, error) { return false, "", nil }

func processCPU() (user, system time.Duration) { return 0, 0 }

func processSelfCPU() (user, system time.Duration) { return 0, 0 }

func runService(context.Context, Options, []string) error {
	return errors.New("the Graph service is not available on Windows")
}

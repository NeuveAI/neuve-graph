//go:build windows

package cli

import (
	"context"
	"errors"
)

// Windows has no shared Graph service: every command runs in its own process.
func forwardToService(context.Context, []string) (bool, error) { return false, nil }

func runService(context.Context, Options, []string) error {
	return errors.New("the Graph service is not available on Windows")
}

//go:build windows

package cli

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/windows"
)

func openLifeline() (*os.File, error) {
	const name = "ENTIRE_GRAPH_LIFELINE_FD"
	raw, present := os.LookupEnv(name)
	_ = os.Unsetenv(name)
	if !present {
		return nil, nil
	}
	descriptor, err := strconv.Atoi(raw)
	if err != nil || descriptor < 0 {
		return nil, fmt.Errorf("%s must name an open non-negative file descriptor, got %q", name, raw)
	}
	// Go exposes Windows handles rather than CRT descriptors. Standard input
	// is still descriptor 0 in the caller's contract; other inherited handles
	// can be named by their native value.
	handle := uintptr(descriptor)
	switch descriptor {
	case 0:
		handle = os.Stdin.Fd()
	case 1:
		handle = os.Stdout.Fd()
	case 2:
		handle = os.Stderr.Fd()
	}
	if err := windows.SetHandleInformation(windows.Handle(handle), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, fmt.Errorf("%s=%s: %w", name, raw, err)
	}
	file := os.NewFile(handle, "Graph caller lifeline")
	if file == nil {
		return nil, fmt.Errorf("%s=%s: invalid handle", name, raw)
	}
	return file, nil
}

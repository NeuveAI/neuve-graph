//go:build !darwin

package gitutil

import "runtime"

// performanceCores reports every logical CPU where the platform does not
// distinguish performance cores.
func performanceCores() int {
	return runtime.NumCPU()
}

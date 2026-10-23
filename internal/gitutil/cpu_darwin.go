//go:build darwin

package gitutil

import (
	"runtime"
	"syscall"
)

// performanceCores counts the performance cores on Apple silicon, where the
// efficiency cores are much slower and are left to the person and interactive
// work. Machines without performance levels report every logical CPU.
func performanceCores() int {
	if cores, err := syscall.SysctlUint32("hw.perflevel0.physicalcpu"); err == nil && cores > 0 {
		return int(cores)
	}
	return runtime.NumCPU()
}

// osBuild is the macOS build, which also fixes the C library's regex engine.
func osBuild() string {
	build, _ := syscall.Sysctl("kern.osversion")
	return build
}

package sem

import (
	"os"
	"strconv"
	"strings"
)

// residentBytes is the resident size now; Linux keeps no peak beside it.
func residentBytes() (now, peak uint64) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0
	}
	return pages * uint64(os.Getpagesize()), 0
}

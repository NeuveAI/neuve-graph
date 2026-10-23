//go:build !darwin && !linux

package sem

func maxRSSBytes() uint64 {
	return 0
}

func residentBytes() (now, peak uint64) {
	return 0, 0
}

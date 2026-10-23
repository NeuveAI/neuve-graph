package sem

// ResidentMemory is this process's resident memory now and at its peak, as
// the operating system counts it, the peak never below the current size;
// zero where it reports none.
func ResidentMemory() (now, peak uint64) {
	now, peak = residentBytes()
	return now, max(peak, maxRSSBytes(), now)
}

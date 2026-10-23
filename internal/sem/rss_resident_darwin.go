package sem

/*
#include <mach/mach.h>

static void graph_resident_bytes(unsigned long long *now, unsigned long long *peak) {
	struct mach_task_basic_info info;
	mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
	*now = 0;
	*peak = 0;
	if (task_info(mach_task_self(), MACH_TASK_BASIC_INFO, (task_info_t)&info, &count) != KERN_SUCCESS) {
		return;
	}
	*now = info.resident_size;
	*peak = info.resident_size_max;
}
*/
import "C"

// residentBytes is the resident size now and at its peak, from one reading,
// so the peak is never below the current size.
func residentBytes() (now, peak uint64) {
	var cNow, cPeak C.ulonglong
	C.graph_resident_bytes(&cNow, &cPeak)
	return uint64(cNow), uint64(cPeak)
}

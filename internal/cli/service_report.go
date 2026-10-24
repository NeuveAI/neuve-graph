package cli

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/sem"
)

// prepareLane is the service's lane for preparations (index): one at a time,
// never in a read's slot.
const prepareLane = "prepare"

// ExitServiceDeadline is the exit status of a call whose deadline
// (ENTIRE_GRAPH_DEADLINE_MS) passed while it waited for the shared Graph
// service, with nothing on stdout: EX_TEMPFAIL, "try again later". A search
// the service had already taken goes on there, and an exact-commit read keeps
// its answer for the next identical call.
const ExitServiceDeadline = 75

// ErrServiceDeadline is the error of such a call.
var ErrServiceDeadline = errors.New("deadline passed while waiting for the Graph service")

// ExitCode is the process exit status for a command's error.
func ExitCode(err error) int {
	if errors.Is(err, ErrServiceDeadline) {
		return ExitServiceDeadline
	}
	var exit exitStatusError
	if errors.As(err, &exit) {
		return exit.code
	}
	return 1
}

// exitStatusError is the exit status of a command the service ran in a child
// process, which printed its own error: the caller exits with that status
// and prints nothing more.
type exitStatusError struct{ code int }

func (e exitStatusError) Error() string { return "" }

// serviceReport measures one prepared read for the `service` block of its
// JSON, whenever the shared Graph service setting is on: in the service, the
// read it answers; in a process that runs the read itself, that process.
type serviceReport struct {
	answered  bool
	reason    string
	cpuUser   time.Duration
	cpuSystem time.Duration
	reads     int
	hits      int
	queueWait time.Duration
	// stats is the block last handed out, for the service's log, with the
	// process's own CPU over the read's window, which its share comes from.
	stats                   *sem.ServiceStats
	processUser, processSys time.Duration
	// For a read the service answers: the process's running reads, as
	// read-seconds, when it began, when it began, and whether another read
	// overlapped it.
	activeAtStart float64
	startedAt     time.Time
	overlapped    bool
	running       bool
	// The process's own CPU, its children's apart, when the read began, and
	// the read's own Git processes, accounted exactly.
	selfUser, selfSys time.Duration
	children          *gitutil.ChildCPU
}

// serviceReads accounts the reads a service process runs at once, so a
// read's CPU, measured for the whole process, can be given as its share.
var serviceReads = struct {
	sync.Mutex
	active   map[*serviceReport]bool
	integral float64 // running reads integrated over time, in read-seconds
	last     time.Time
}{active: map[*serviceReport]bool{}}

// advanceServiceReads brings the integral up to now. serviceReads is held.
func advanceServiceReads(now time.Time) {
	if !serviceReads.last.IsZero() {
		serviceReads.integral += float64(len(serviceReads.active)) * now.Sub(serviceReads.last).Seconds()
	}
	serviceReads.last = now
}

// beginServiceRead counts a read the service starts running.
func beginServiceRead(report *serviceReport) {
	serviceReads.Lock()
	defer serviceReads.Unlock()
	now := time.Now()
	advanceServiceReads(now)
	report.selfUser, report.selfSys = processSelfCPU()
	if len(serviceReads.active) > 0 {
		report.overlapped = true
		for other := range serviceReads.active {
			other.overlapped = true
		}
	}
	serviceReads.active[report] = true
	report.activeAtStart, report.startedAt, report.running = serviceReads.integral, now, true
}

// endServiceRead stops counting a read.
func endServiceRead(report *serviceReport) {
	serviceReads.Lock()
	defer serviceReads.Unlock()
	advanceServiceReads(time.Now())
	delete(serviceReads.active, report)
	report.running = false
}

// readConcurrency is the average number of reads running over a read's
// window so far, and whether another read overlapped it.
func readConcurrency(report *serviceReport) (float64, bool) {
	serviceReads.Lock()
	defer serviceReads.Unlock()
	now := time.Now()
	advanceServiceReads(now)
	wall := now.Sub(report.startedAt).Seconds()
	if !report.running || wall <= 0 {
		return 1, report.overlapped
	}
	return max(1, (serviceReads.integral-report.activeAtStart)/wall), report.overlapped
}

type serviceReportKey struct{}

func newServiceReport(answered bool, reason string) *serviceReport {
	report := &serviceReport{answered: answered, reason: reason}
	report.cpuUser, report.cpuSystem = processCPU()
	_, _, report.reads, report.hits = sem.SnapshotMemoryState()
	return report
}

func withServiceReport(ctx context.Context, report *serviceReport) context.Context {
	return context.WithValue(ctx, serviceReportKey{}, report)
}

// serviceStats is the `service` block for the read ctx carries, measured
// now, with the size of its cache directory as last measured; nil when the
// setting is off.
func serviceStats(ctx context.Context, cacheDir string) *sem.ServiceStats {
	report, _ := ctx.Value(serviceReportKey{}).(*serviceReport)
	if report == nil {
		return nil
	}
	user, system := processCPU()
	report.processUser, report.processSys = user-report.cpuUser, system-report.cpuSystem
	cpuUser, cpuSys := report.processUser, report.processSys
	// While other reads overlap it, a read's CPU is its share of the
	// process's: the process's CPU over its window divided by the average
	// number of reads running then. An estimate, and flagged as one.
	concurrency, shared := 1.0, false
	if report.answered && report.running {
		concurrency, shared = readConcurrency(report)
	}
	if report.answered && report.running {
		// The process's own CPU, shared among the reads running with it;
		// the read's Git processes are its own, counted exactly. Not every
		// child of the process: a preparation's child is not the read's.
		selfUser, selfSys := processSelfCPU()
		cpuUser = time.Duration(float64(selfUser-report.selfUser) / concurrency)
		cpuSys = time.Duration(float64(selfSys-report.selfSys) / concurrency)
		if report.children != nil {
			childUser, childSys := report.children.Total()
			cpuUser, cpuSys = cpuUser+childUser, cpuSys+childSys
		}
	}
	held, limit, reads, hits := sem.SnapshotMemoryState()
	rss, maxRSS := sem.ResidentMemory()
	stats := &sem.ServiceStats{
		Answered:       report.answered,
		Reason:         report.reason,
		RSSBytes:       rss,
		MaxRSSBytes:    maxRSS,
		SnapshotsHeld:  held,
		SnapshotsLimit: limit,
		CPUUserMS:      cpuUser.Milliseconds(),
		CPUSystemMS:    cpuSys.Milliseconds(),
		CPUShared:      shared,
		QueueWaitMS:    report.queueWait.Milliseconds(),
	}
	if reads > report.reads {
		hit := hits-report.hits == reads-report.reads
		stats.SnapshotHit = &hit
	}
	if cacheDir != "" {
		limits := sem.CacheLimitsFromEnv()
		stats.CacheCapBytes = limits.MaxBytes
		if usage, err := sem.LastCacheUsage(cacheDir, limits); err == nil {
			stats.CacheBytes = usage.Bytes
		}
	}
	report.stats = stats
	return stats
}

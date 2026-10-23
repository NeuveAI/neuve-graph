package cli

import (
	"context"
	"errors"
	"time"

	"github.com/entireio/entire-graph/internal/sem"
)

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
	return 1
}

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
	// stats is the block last handed out, for the service's log.
	stats *sem.ServiceStats
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
	held, limit, reads, hits := sem.SnapshotMemoryState()
	rss, maxRSS := sem.ResidentMemory()
	stats := &sem.ServiceStats{
		Answered:       report.answered,
		Reason:         report.reason,
		RSSBytes:       rss,
		MaxRSSBytes:    maxRSS,
		SnapshotsHeld:  held,
		SnapshotsLimit: limit,
		SnapshotHit:    reads > report.reads && hits-report.hits == reads-report.reads,
		CPUUserMS:      (user - report.cpuUser).Milliseconds(),
		CPUSystemMS:    (system - report.cpuSystem).Milliseconds(),
		QueueWaitMS:    report.queueWait.Milliseconds(),
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

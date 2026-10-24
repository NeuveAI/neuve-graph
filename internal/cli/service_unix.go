//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/entireio/entire-graph/internal/gitutil"
	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/service"
)

// Settings of the shared Graph service. They are part of the environment that
// names a service, so changing one starts another.
const (
	envService            = "ENTIRE_GRAPH_SERVICE"             // "auto" forwards eligible commands; "require" never falls back
	envServiceIdle        = "ENTIRE_GRAPH_SERVICE_IDLE"        // seconds before an idle service exits (60)
	envServiceConcurrency = "ENTIRE_GRAPH_SERVICE_CONCURRENCY" // reads run at once (2)
	envServiceSnapshots   = "ENTIRE_GRAPH_SERVICE_SNAPSHOTS"   // decoded snapshots kept (2)

	// A call's class and deadline, set per call; they order the service's
	// queue and never change an answer, so they name no service.
	envClass      = "ENTIRE_GRAPH_CLASS"       // "interactive" or "background" (default)
	envDeadlineMS = "ENTIRE_GRAPH_DEADLINE_MS" // milliseconds from now the call waits for the service at most
)

// callTiming reads the call's class and deadline from its environment.
func callTiming() (class string, deadline time.Time, err error) {
	switch class = os.Getenv(envClass); class {
	case "":
		class = service.ClassBackground
	case service.ClassInteractive, service.ClassBackground:
	default:
		return "", time.Time{}, fmt.Errorf("%s must be %q or %q, got %q", envClass, service.ClassInteractive, service.ClassBackground, class)
	}
	if raw := os.Getenv(envDeadlineMS); raw != "" {
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ms <= 0 {
			return "", time.Time{}, fmt.Errorf("%s must be a positive number of milliseconds, got %q", envDeadlineMS, raw)
		}
		deadline = time.Now().Add(time.Duration(ms) * time.Millisecond)
	}
	return class, deadline, nil
}

// serviceCommands are the read-only prepared reads a service answers: the
// commands whose cost is mostly loading a prepared snapshot.
var serviceCommands = map[string]bool{"query": true, "search": true, "neighbors": true}

// serviceEligible reports whether a command line can be answered by the
// service exactly as this process would answer it: opted in, a prepared read
// (parsed by the command's own flag parser) whose repository, cache and
// policy files are absolute paths, because the service's working directory
// is not the client's; output that is not a terminal, whose formatting
// depends on the process; and session files named absolutely.
func serviceEligible(args []string, env EntireEnv) bool {
	if !serviceSettingOn(args) {
		return false
	}
	var prepared bool
	var paths []string
	switch args[0] {
	case "query", "search":
		flags, rest, err := parseSearchFlags(args[1:])
		if err != nil || len(rest) != 0 {
			return false
		}
		prepared = flags.PreparedOnly
		paths = append(append([]string{flags.Repo, flags.CacheDir}, flags.IgnoreFiles...), flags.IncludeFiles...)
	case "neighbors":
		flags, err := parseNeighborFlags(args[1:])
		if err != nil {
			return false
		}
		prepared = flags.PreparedOnly
		paths = append(append([]string{flags.Repo, flags.CacheDir}, flags.IgnoreFile...), flags.IncludeFile...)
	}
	if !prepared {
		return false
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return false
		}
	}
	if isTerminal(os.Stdout) || isTerminal(os.Stderr) {
		return false
	}
	for _, path := range []string{env.PresearchPath, env.SearchSession} {
		if path != "" && !filepath.IsAbs(path) {
			return false
		}
	}
	return true
}

// serviceSettingOn reports whether the service setting is on for a command
// the service serves, so its read reports a `service` block.
func serviceSettingOn(args []string) bool {
	mode := os.Getenv(envService)
	return (mode == "auto" || mode == "require") && len(args) > 0 && serviceCommands[args[0]]
}

// forwardToService answers an eligible command through the shared service,
// starting it if none runs. handled is false when the command should run in
// this process: not eligible, or no service could answer. reason then says
// why, for the read's `service` block ("off", "busy" or "unavailable"), and
// is empty when the setting is off.
func forwardToService(ctx context.Context, args []string) (handled bool, reason string, err error) {
	if !serviceSettingOn(args) {
		return false, "", nil
	}
	class, deadline, err := callTiming()
	if err != nil {
		return true, "", err
	}
	if !serviceEligible(args, EnvFromOS()) {
		return false, "off", nil
	}
	// The resolved path: an executable reached through a link is the same
	// build, so it shares one service.
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err == nil {
		var socket string
		if socket, err = service.SocketPath(executable, os.Environ()); err == nil {
			return callService(ctx, executable, socket, args, class, deadline)
		}
	}
	if os.Getenv(envService) == "require" {
		return true, "", fmt.Errorf("the Graph service cannot be reached: %v", err)
	}
	return false, "unavailable", nil
}

func callService(ctx context.Context, executable, socket string, args []string, class string, deadline time.Time) (handled bool, reason string, err error) {
	build, err := runningBuild()
	if err != nil {
		if os.Getenv(envService) == "require" {
			return true, "", fmt.Errorf("the Graph service cannot be reached: %v", err)
		}
		return false, "unavailable", nil
	}
	start := func() (*os.Process, error) {
		return service.Start(executable, socket, []string{"service", "--socket", socket})
	}
	callCtx := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	response, err := service.CallStarting(callCtx, socket, start, service.Request{Args: args, Build: build, Class: class, Deadline: deadline})
	if err != nil && ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return true, "", ErrServiceDeadline
	}
	if errors.Is(err, service.ErrUnreachable) || (err == nil && (response.Retry || response.Mismatch)) {
		if os.Getenv(envService) == "require" {
			switch {
			case err != nil:
				return true, "", fmt.Errorf("the Graph service did not answer: %v", err)
			case response.Mismatch:
				return true, "", errors.New("the Graph service runs another build or protocol")
			}
			return true, "", errors.New("the Graph service was busy")
		}
		switch {
		case err != nil:
			return false, "unavailable", nil
		case response.Mismatch:
			return false, "mismatch", nil
		}
		return false, "busy", nil
	}
	if err != nil {
		return true, "", err
	}
	if _, err := os.Stdout.Write(response.Stdout); err != nil {
		return true, "", err
	}
	if _, err := os.Stderr.Write(response.Stderr); err != nil {
		return true, "", err
	}
	if response.Failed {
		return true, "", errors.New(response.Error)
	}
	return true, "", nil
}

// runService runs the shared Graph service on --socket until it is idle. It
// is started by forwardToService, not by people.
func runService(ctx context.Context, opts Options, args []string) error {
	var socket string
	for index := 0; index < len(args); index++ {
		if args[index] == "--socket" && index+1 < len(args) {
			socket = args[index+1]
			index++
			continue
		}
		return unexpectedArgumentsError("service", opts.Version, args[index:])
	}
	if socket == "" || !filepath.IsAbs(socket) {
		return errors.New("service requires --socket with an absolute path")
	}
	build, err := runningBuild()
	if err != nil {
		return fmt.Errorf("the Graph service cannot name its own build: %w", err)
	}
	sem.EnableSnapshotMemory(positiveSetting(envServiceSnapshots, 2))
	// The cache directories the service read from since it was last idle; it
	// keeps each within its bounds when idle.
	var cacheDirsMu sync.Mutex
	cacheDirs := map[string]bool{}
	pruneIdle := func() {
		cacheDirsMu.Lock()
		dirs := cacheDirs
		cacheDirs = map[string]bool{}
		cacheDirsMu.Unlock()
		for dir := range dirs {
			_, _ = sem.PruneCache(dir, sem.CacheLimitsFromEnv())
		}
	}
	serveOptions := service.ServeOptions{
		// Two reads at once: measured on the Neuve repository, 1.55 times
		// the throughput of one for about 3.7 cores in all, where four gave
		// 1.9 times for 5 cores and doubled the slowest search.
		Concurrency: positiveSetting(envServiceConcurrency, 2),
		Idle:        time.Duration(positiveSetting(envServiceIdle, 60)) * time.Second,
		Build:       build,
		Deliver:     deliverServiceResponse,
		Keepable:    keepableServiceRead,
		OnIdle:      pruneIdle,
		OnExit: func(idle time.Duration) {
			appendServiceLog(socket, map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "event": "exit", "idle_ms": idle.Milliseconds()})
		},
	}
	return service.Serve(ctx, socket, serveOptions, func(requestCtx context.Context, request service.Request) (response service.Response) {
		if len(request.Args) == 0 || !serviceCommands[request.Args[0]] {
			return service.Response{Failed: true, Error: fmt.Sprintf("the Graph service does not run %q", request.Args)}
		}
		if dir := serviceCacheDir(request.Args); dir != "" {
			cacheDirsMu.Lock()
			cacheDirs[dir] = true
			cacheDirsMu.Unlock()
		}
		report := newServiceReport(true, "")
		requestCtx, report.children = gitutil.WithChildCPU(requestCtx)
		beginServiceRead(report)
		defer endServiceRead(report)
		started := time.Now()
		panicked := false
		// Deferred first so it runs last, after a panic became a Retry.
		defer func() {
			logServiceRead(socket, request, report, started, response, panicked, requestCtx.Err() != nil)
		}()
		defer func() {
			if recover() != nil {
				panicked = true
				response = service.Response{Retry: true}
			}
		}()
		// A read whose answer may be kept records what it opened, so the
		// kept answer is served only while all of that is unchanged.
		var opened *sem.CacheOpenRecord
		var policy map[string]sem.FileIdentity
		keepable := keepableServiceRead(request)
		if keepable {
			policy = identifyFiles(servingPolicyFiles(requestCtx, request.Args))
			opened = sem.RecordCacheOpens()
		}
		var stdout, stderr bytes.Buffer
		err := Run(withServiceReport(requestCtx, report), Options{Version: opts.Version, Env: EnvFromOS(), Stdout: &stdout, Stderr: &stderr, Stdin: bytes.NewReader(nil)}, request.Args)
		response = service.Response{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
		if err != nil {
			response.Failed, response.Error = true, err.Error()
		}
		if keepable {
			inputs := opened.Stop()
			for path, identity := range policy {
				inputs[path] = identity
			}
			response.StillValid = inputsUnchanged(inputs)
		}
		return response
	})
}

// serviceCacheDir is the cache directory a read the service runs names.
func serviceCacheDir(args []string) string {
	switch args[0] {
	case "query", "search":
		if flags, _, err := parseSearchFlags(args[1:]); err == nil {
			return flags.CacheDir
		}
	case "neighbors":
		if flags, err := parseNeighborFlags(args[1:]); err == nil {
			return flags.CacheDir
		}
	}
	return ""
}

// serviceLogLimit bounds the service's log: past it, the log is kept as
// <socket>.log.1 and a new one begun.
const serviceLogLimit = 1 << 20

// serviceLogMu keeps reads that end together from rotating the log twice.
var serviceLogMu sync.Mutex

// logServiceRead appends one JSON line for a read the service ran to
// <socket>.log: its command, outcome (failed, a panic handed back as Retry,
// or cancelled because its callers left), duration and `service` block
// (measured at its end when the read wrote none, as a text read does).
func logServiceRead(socket string, request service.Request, report *serviceReport, started time.Time, response service.Response, panicked, cancelled bool) {
	stats := report.stats
	if stats == nil {
		stats = serviceStats(withServiceReport(context.Background(), report), "")
	}
	appendServiceLog(socket, struct {
		Time      string `json:"time"`
		Command   string `json:"command"`
		Class     string `json:"class"`
		Failed    bool   `json:"failed,omitempty"`
		Panicked  bool   `json:"panicked,omitempty"`
		Cancelled bool   `json:"cancelled,omitempty"`
		TotalMS   int64  `json:"total_ms"`
		// IdleBeforeMS is how long the service had been idle when this
		// read's first caller arrived: the gaps the idle exit is set from.
		IdleBeforeMS int64 `json:"idle_before_ms"`
		KeptBytes    int64 `json:"kept_bytes"`
		// The process's own CPU over this read's window, which a shared
		// read's CPU fields are its share of.
		ProcessCPUUserMS   int64             `json:"process_cpu_user_ms"`
		ProcessCPUSystemMS int64             `json:"process_cpu_system_ms"`
		Service            *sem.ServiceStats `json:"service"`
	}{time.Now().UTC().Format(time.RFC3339Nano), request.Args[0], request.Class, response.Failed, panicked, cancelled, time.Since(started).Milliseconds(), request.IdleBefore.Milliseconds(), service.KeptBytes(), report.processUser.Milliseconds(), report.processSys.Milliseconds(), stats})
}

// appendServiceLog appends one JSON line to <socket>.log.
func appendServiceLog(socket string, value any) {
	line, err := json.Marshal(value)
	if err != nil {
		return
	}
	serviceLogMu.Lock()
	defer serviceLogMu.Unlock()
	path := socket + ".log"
	if info, err := os.Stat(path); err == nil && info.Size() > serviceLogLimit {
		_ = os.Rename(path, path+".1")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}

// joinedServiceBlock starts the `service` block of a read the service
// answered. Outside strings JSON never holds this byte sequence except as that
// key, and the block holds no nested object, so it ends at the next brace.
var joinedServiceBlock = []byte(`"service":{"answered":true`)

// keepableServiceRead reports whether a read's task runs on when its callers
// leave, with its answer kept: a prepared read at an exact commit, whose
// inputs cannot move under it. A --head read follows HEAD, so it never is.
func keepableServiceRead(request service.Request) bool {
	args := request.Args
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "query", "search":
		flags, _, err := parseSearchFlags(args[1:])
		return err == nil && flags.PreparedOnly && isExactOID(flags.PreparedCommit)
	case "neighbors":
		flags, err := parseNeighborFlags(args[1:])
		return err == nil && flags.PreparedOnly && isExactOID(flags.PreparedCommit)
	}
	return false
}

// servingPolicyFiles are the files besides the cache an exact-commit prepared
// read's answer depends on: the serving directory's external policy (ADR
// 0008), the repository's .graphignore and the ignore and include files the
// read names, and the repository's git config.
func servingPolicyFiles(ctx context.Context, args []string) []string {
	var repo string
	var files []string
	switch args[0] {
	case "query", "search":
		flags, _, err := parseSearchFlags(args[1:])
		if err != nil {
			return nil
		}
		repo, files = flags.Repo, append(append(files, flags.IgnoreFiles...), flags.IncludeFiles...)
	case "neighbors":
		flags, err := parseNeighborFlags(args[1:])
		if err != nil {
			return nil
		}
		repo, files = flags.Repo, append(append(files, flags.IgnoreFile...), flags.IncludeFile...)
	}
	files = append(files, filepath.Join(repo, ".graphignore"))
	if top, err := resolveRepo(ctx, EnvFromOS(), repo); err == nil && top != repo {
		files = append(files, filepath.Join(top, ".graphignore"))
	}
	// The repository's config holds the remote its preparations are keyed by.
	if common, err := gitutil.CommonDir(ctx, repo); err == nil {
		files = append(files, filepath.Join(common, "config"))
	}
	return files
}

func identifyFiles(paths []string) map[string]sem.FileIdentity {
	identities := make(map[string]sem.FileIdentity, len(paths))
	for _, path := range paths {
		identities[path] = sem.IdentifyFile(path)
	}
	return identities
}

// inputsUnchanged returns a check that each path still names what it named
// when recorded: the same file (device and inode), size and modification
// time, or still nothing.
func inputsUnchanged(recorded map[string]sem.FileIdentity) func() bool {
	return func() bool {
		for path, identity := range recorded {
			if sem.IdentifyFile(path) != identity {
				return false
			}
		}
		return true
	}
}

func isExactOID(oid string) bool {
	return (len(oid) == 40 || len(oid) == 64) && isLowerHex(oid)
}

// deliverServiceResponse is one caller's answer: the shared bytes, with the
// `service` block saying how this caller got it. A caller that joined a task
// another started, or was answered from a kept answer, reports no CPU of its
// own, so the CPU of one run is counted once however many callers shared it.
func deliverServiceResponse(response service.Response, delivery service.Delivery) service.Response {
	return rewriteServiceBlock(response, func(stats *sem.ServiceStats) {
		if delivery.Joined || delivery.Kept {
			stats.CPUUserMS, stats.CPUSystemMS, stats.CPUShared = 0, 0, false
		}
		stats.Class = delivery.Class
		stats.KeptAnswer = delivery.Kept
		stats.MovedUp = delivery.MovedUp
		stats.QueueWaitMS = delivery.QueueWait.Milliseconds()
		if delivery.Kept {
			stats.QueueWaitMS = 0
		}
	})
}

// joinedServiceResponse is the answer handed to a caller that joined a task
// another caller started.
func joinedServiceResponse(response service.Response) service.Response {
	return deliverServiceResponse(response, service.Delivery{Joined: true})
}

// rewriteServiceBlock changes the `service` block of an answer the service
// gave, leaving every other byte as it was; an answer with none (a text
// format) is returned unchanged.
func rewriteServiceBlock(response service.Response, change func(*sem.ServiceStats)) service.Response {
	at := bytes.Index(response.Stdout, joinedServiceBlock)
	if at < 0 {
		return response
	}
	start := at + len(`"service":`)
	end := bytes.IndexByte(response.Stdout[start:], '}')
	if end < 0 {
		return response
	}
	end += start + 1
	var stats sem.ServiceStats
	if err := json.Unmarshal(response.Stdout[start:end], &stats); err != nil {
		return response
	}
	change(&stats)
	block, err := json.Marshal(stats)
	if err != nil {
		return response
	}
	stdout := make([]byte, 0, len(response.Stdout)-(end-start)+len(block))
	stdout = append(append(append(stdout, response.Stdout[:start]...), block...), response.Stdout[end:]...)
	response.Stdout = stdout
	return response
}

// runningBuild is the stamp of the executable file this process runs, the
// one it holds open from its start (sem.HoldExecutable).
func runningBuild() (service.BuildStamp, error) {
	held := sem.HeldExecutable()
	if held == nil {
		return service.BuildStamp{}, errors.New("this process does not hold its executable")
	}
	return service.StampOf(held)
}

// processSelfCPU is the CPU this process itself has used, its children's
// apart.
func processSelfCPU() (user, system time.Duration) {
	var self syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &self)
	return time.Duration(self.Utime.Nano()), time.Duration(self.Stime.Nano())
}

// processCPU is the CPU this process and the children it has waited for (the
// engine's git processes) have used.
func processCPU() (user, system time.Duration) {
	var self, children syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &self)
	_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children)
	return time.Duration(self.Utime.Nano() + children.Utime.Nano()), time.Duration(self.Stime.Nano() + children.Stime.Nano())
}

func positiveSetting(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}

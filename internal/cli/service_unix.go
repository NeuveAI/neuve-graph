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

	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/service"
)

// Settings of the shared Graph service. They are part of the environment that
// names a service, so changing one starts another.
const (
	envService            = "ENTIRE_GRAPH_SERVICE"             // "auto" forwards eligible commands; "require" never falls back
	envServiceIdle        = "ENTIRE_GRAPH_SERVICE_IDLE"        // seconds before an idle service exits (60)
	envServiceConcurrency = "ENTIRE_GRAPH_SERVICE_CONCURRENCY" // commands run at once (1)
	envServiceSnapshots   = "ENTIRE_GRAPH_SERVICE_SNAPSHOTS"   // decoded snapshots kept (2)
)

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
			return callService(ctx, executable, socket, args)
		}
	}
	if os.Getenv(envService) == "require" {
		return true, "", fmt.Errorf("the Graph service cannot be reached: %v", err)
	}
	return false, "unavailable", nil
}

func callService(ctx context.Context, executable, socket string, args []string) (handled bool, reason string, err error) {
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
	response, err := service.CallStarting(ctx, socket, start, service.Request{Args: args, Build: build})
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
		Concurrency: positiveSetting(envServiceConcurrency, 1),
		Idle:        time.Duration(positiveSetting(envServiceIdle, 60)) * time.Second,
		Build:       build,
		Joined:      joinedServiceResponse,
		OnIdle:      pruneIdle,
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
		started := time.Now()
		panicked := false
		// Deferred first so it runs last, after a panic became a Retry.
		defer func() {
			logServiceRead(socket, request.Args[0], report, started, response, panicked, requestCtx.Err() != nil)
		}()
		defer func() {
			if recover() != nil {
				panicked = true
				response = service.Response{Retry: true}
			}
		}()
		var stdout, stderr bytes.Buffer
		err := Run(withServiceReport(requestCtx, report), Options{Version: opts.Version, Env: EnvFromOS(), Stdout: &stdout, Stderr: &stderr, Stdin: bytes.NewReader(nil)}, request.Args)
		response = service.Response{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
		if err != nil {
			response.Failed, response.Error = true, err.Error()
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
func logServiceRead(socket, command string, report *serviceReport, started time.Time, response service.Response, panicked, cancelled bool) {
	stats := report.stats
	if stats == nil {
		stats = serviceStats(withServiceReport(context.Background(), report), "")
	}
	line, err := json.Marshal(struct {
		Time      string            `json:"time"`
		Command   string            `json:"command"`
		Failed    bool              `json:"failed,omitempty"`
		Panicked  bool              `json:"panicked,omitempty"`
		Cancelled bool              `json:"cancelled,omitempty"`
		TotalMS   int64             `json:"total_ms"`
		Service   *sem.ServiceStats `json:"service"`
	}{time.Now().UTC().Format(time.RFC3339Nano), command, response.Failed, panicked, cancelled, time.Since(started).Milliseconds(), stats})
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

// joinedServiceResponse is the answer handed to a caller whose read joined an
// identical one already running: the same bytes, except that its `service`
// block reports no CPU of its own, so the CPU of one run is counted once
// however many callers shared it.
func joinedServiceResponse(response service.Response) service.Response {
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
	stats.CPUUserMS, stats.CPUSystemMS = 0, 0
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

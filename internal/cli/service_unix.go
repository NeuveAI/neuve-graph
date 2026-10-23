//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	if mode := os.Getenv(envService); (mode != "auto" && mode != "require") || len(args) == 0 || !serviceCommands[args[0]] {
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

// forwardToService answers an eligible command through the shared service,
// starting it if none runs. handled is false when the command should run in
// this process: not eligible, or no service could answer.
func forwardToService(ctx context.Context, args []string) (handled bool, err error) {
	if !serviceEligible(args, EnvFromOS()) {
		return false, nil
	}
	executable, err := os.Executable()
	if err == nil {
		var socket string
		if socket, err = service.SocketPath(executable, os.Environ()); err == nil {
			return callService(ctx, executable, socket, args)
		}
	}
	if os.Getenv(envService) == "require" {
		return true, fmt.Errorf("the Graph service cannot be reached: %v", err)
	}
	return false, nil
}

func callService(ctx context.Context, executable, socket string, args []string) (handled bool, err error) {
	start := func() (*os.Process, error) {
		return service.Start(executable, socket, []string{"service", "--socket", socket})
	}
	response, err := service.CallStarting(ctx, socket, start, service.Request{Args: args})
	if errors.Is(err, service.ErrUnreachable) || (err == nil && response.Retry) {
		if os.Getenv(envService) == "require" {
			return true, fmt.Errorf("the Graph service did not answer: %v", err)
		}
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if _, err := os.Stdout.Write(response.Stdout); err != nil {
		return true, err
	}
	if _, err := os.Stderr.Write(response.Stderr); err != nil {
		return true, err
	}
	if response.Failed {
		return true, errors.New(response.Error)
	}
	return true, nil
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
	sem.EnableSnapshotMemory(positiveSetting(envServiceSnapshots, 2))
	serveOptions := service.ServeOptions{
		Concurrency: positiveSetting(envServiceConcurrency, 1),
		Idle:        time.Duration(positiveSetting(envServiceIdle, 60)) * time.Second,
	}
	return service.Serve(ctx, socket, serveOptions, func(requestCtx context.Context, request service.Request) (response service.Response) {
		if len(request.Args) == 0 || !serviceCommands[request.Args[0]] {
			return service.Response{Failed: true, Error: fmt.Sprintf("the Graph service does not run %q", request.Args)}
		}
		defer func() {
			if recover() != nil {
				response = service.Response{Retry: true}
			}
		}()
		var stdout, stderr bytes.Buffer
		err := Run(requestCtx, Options{Version: opts.Version, Env: EnvFromOS(), Stdout: &stdout, Stderr: &stderr, Stdin: bytes.NewReader(nil)}, request.Args)
		response = service.Response{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
		if err != nil {
			response.Failed, response.Error = true, err.Error()
		}
		return response
	})
}

func positiveSetting(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}

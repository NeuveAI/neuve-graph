//go:build !windows

// Package service runs Graph as one long-lived process per user and
// environment, so searches stop paying for a process start and a snapshot
// decode each. It is started on demand by the first search that wants it and
// exits when idle; nothing runs while nobody searches.
//
// A service is bound to one executable and one environment: its socket's name
// is a digest of both, so a client always reaches a service whose process
// sees exactly what the client's own process would, and a rebuilt binary or a
// changed setting starts a new service. Requests carry only arguments.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProtocolVersion is the service's wire protocol. A caller and a service
// of different protocols never talk: the service refuses the request and the
// caller runs the command itself.
const ProtocolVersion = 2

// BuildStamp names the executable file a process runs: the file itself
// (device and inode) and its size. A service answers only a caller running
// the very same file, so a build never answers for another, even one
// installed at the same path (an install writes a new file and renames it
// into place). Not the modification time: Neuve touches an engine copy it
// has checked, so the cache store keeps it, and that must not part a caller
// from its service. While a service runs it holds its file open, so the
// file's inode cannot be reused by another.
type BuildStamp struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Size   int64  `json:"size"`
}

// StampOf is the build stamp of an open executable file.
func StampOf(file *os.File) (BuildStamp, error) {
	info, err := file.Stat()
	if err != nil {
		return BuildStamp{}, err
	}
	return stampOf(info)
}

func stampOf(info os.FileInfo) (BuildStamp, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return BuildStamp{}, errors.New("no file identity on this platform")
	}
	return BuildStamp{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Size: info.Size()}, nil
}

// Request is one command line for the service to run, from a caller running
// the build Build, of class Class (interactive or background) that waits until
// Deadline (zero: no deadline). Class and deadline order the queue; they never
// change the answer.
type Request struct {
	Args     []string
	Build    BuildStamp
	Class    string
	Deadline time.Time
	// IdleBefore is how long the service had been idle when this request
	// arrived, set by the service for the request that starts a task.
	IdleBefore time.Duration
}

// Response is what running it printed and returned.
type Response struct {
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
	Failed bool   `json:"failed"`
	Error  string `json:"error,omitempty"`
	// Retry asks the client to run the command itself: the service could not
	// answer it the way the client's own process would (it panicked).
	Retry bool `json:"retry,omitempty"`
	// Mismatch reports that the service runs another build or protocol than
	// the caller, so it did not run the command; the caller runs it itself.
	Mismatch bool `json:"mismatch,omitempty"`
	// StillValid, set by the service's handler for an answer it may keep,
	// reports whether every input the answer came from is as it was; a kept
	// answer is served only while it is. Never sent.
	StillValid func() bool `json:"-"`
}

const maxFrameBytes = 1 << 30

// wireRequest and wireResponse carry text as bytes, so arguments, output and
// errors that are not valid UTF-8 arrive exactly as they were sent.
type wireRequest struct {
	Protocol       int        `json:"protocol"`
	Build          BuildStamp `json:"build"`
	Args           [][]byte   `json:"args"`
	Class          string     `json:"class,omitempty"`
	DeadlineUnixMS int64      `json:"deadline_unix_ms,omitempty"`
}

type wireResponse struct {
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	Failed   bool   `json:"failed"`
	Error    []byte `json:"error,omitempty"`
	Retry    bool   `json:"retry,omitempty"`
	Mismatch bool   `json:"mismatch,omitempty"`
}

func (r Request) wire() wireRequest {
	args := make([][]byte, len(r.Args))
	for i, arg := range r.Args {
		args[i] = []byte(arg)
	}
	wire := wireRequest{Protocol: ProtocolVersion, Build: r.Build, Args: args, Class: r.Class}
	if !r.Deadline.IsZero() {
		wire.DeadlineUnixMS = r.Deadline.UnixMilli()
	}
	return wire
}

func (w wireRequest) request() Request {
	args := make([]string, len(w.Args))
	for i, arg := range w.Args {
		args[i] = string(arg)
	}
	request := Request{Args: args, Build: w.Build, Class: w.Class}
	if request.Class == "" {
		request.Class = ClassBackground
	}
	if w.DeadlineUnixMS > 0 {
		request.Deadline = time.UnixMilli(w.DeadlineUnixMS)
	}
	return request
}

func (r Response) wire() wireResponse {
	return wireResponse{Stdout: r.Stdout, Stderr: r.Stderr, Failed: r.Failed, Error: []byte(r.Error), Retry: r.Retry, Mismatch: r.Mismatch}
}

func (w wireResponse) response() Response {
	return Response{Stdout: w.Stdout, Stderr: w.Stderr, Failed: w.Failed, Error: string(w.Error), Retry: w.Retry, Mismatch: w.Mismatch}
}

// maxSocketPath is the longest unix socket path every supported platform
// binds (macOS allows 103 bytes and a NUL).
const maxSocketPath = 103

// volatileEnvironment never changes what Graph computes and changes between
// calls from one caller, so it does not choose the service.
var volatileEnvironment = map[string]bool{
	"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true,
	// A call's class and deadline: they order the queue, never the answer.
	"ENTIRE_GRAPH_CLASS": true, "ENTIRE_GRAPH_DEADLINE_MS": true,
}

// SocketPath names the socket of the service for this executable file (its
// resolved path, device, inode and size; see BuildStamp) and
// environment, in a directory only this user can enter. The executable is
// named by its resolved path, so one reached through a link shares the
// service.
func SocketPath(executable string, environ []string) (string, error) {
	executable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(executable)
	if err != nil {
		return "", err
	}
	stamp, err := stampOf(info)
	if err != nil {
		return "", err
	}
	var keys []string
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if !volatileEnvironment[key] {
			keys = append(keys, entry)
		}
	}
	sort.Strings(keys)
	sum := sha256.New()
	fmt.Fprintf(sum, "graph-service-%d\x00%s\x00%d\x00%d\x00%d\x00", ProtocolVersion, executable, stamp.Device, stamp.Inode, stamp.Size)
	for _, entry := range keys {
		sum.Write([]byte(entry))
		sum.Write([]byte{0})
	}
	dir, err := socketDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "graph-"+hex.EncodeToString(sum.Sum(nil))[:16]+".sock"), nil
}

// EnvDirectory overrides the service's directory, for tests and diagnosis;
// like every setting it is part of the environment that names a service.
const EnvDirectory = "ENTIRE_GRAPH_SERVICE_DIR"

// socketDirectory is the user-level directory Neuve's sockets share,
// /tmp/neuve-<uid> (the budget socket's and the worker socket's fallback):
// one place every process of the user finds, whatever its TMPDIR, which
// sandboxes vary. Only its owner may enter it, and it is refused if anyone
// else could have prepared it or if it is a link.
func socketDirectory() (string, error) {
	dir := fmt.Sprintf("/tmp/neuve-%d", os.Getuid())
	if override := os.Getenv(EnvDirectory); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("%s must be an absolute path", EnvDirectory)
		}
		dir = override
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("graph service directory %s is not private to this user", dir)
	}
	return dir, nil
}

// staleServiceAge is how long a service's files must have been left alone
// before a starting service removes them, when no service holds them.
const staleServiceAge = 24 * time.Hour

// sweepStaleServices removes the files of services that are gone: each set
// beside a socket graph-<hash>.sock in dir (.sock, .lock, .start, .failed,
// .log, .log.1), when none was touched since before and no service holds its
// lock. Only Graph's own sets: the directory is shared with Neuve's other
// sockets. The lock goes last, while held, so a service starting for that
// set takes a fresh one.
func sweepStaleServices(dir, self string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	newest := map[string]time.Time{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "graph-") {
			continue
		}
		base, _, ok := strings.Cut(name, ".sock")
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		set := filepath.Join(dir, base+".sock")
		if info.ModTime().After(newest[set]) {
			newest[set] = info.ModTime()
		}
	}
	for set, touched := range newest {
		if set == self || touched.After(before) {
			continue
		}
		lock, err := os.OpenFile(set+".lock", os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			continue
		}
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			lock.Close() // a live service
			continue
		}
		for _, suffix := range []string{"", ".start", ".failed", ".log", ".log.1"} {
			_ = os.Remove(set + suffix)
		}
		_ = os.Remove(set + ".lock")
		lock.Close()
	}
}

func writeFrame(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readFrame(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxFrameBytes {
		return fmt.Errorf("graph service frame of %d bytes is too large", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, value)
}

// ErrUnreachable reports that no service answered; the caller runs the
// command itself.
var ErrUnreachable = errors.New("graph service unreachable")

// Call sends one request and waits for its response. Cancelling ctx closes
// the connection, which cancels the request in the service.
func Call(ctx context.Context, socket string, request Request) (Response, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := writeFrame(conn, request.wire()); err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	var response wireResponse
	if err := readFrame(conn, &response); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return response.response(), nil
}

// startFailureMemory is how long a failed start keeps later calls from
// trying again: where no service can run (a sandbox that forbids binding a
// socket), each call then costs one failed dial, not a start.
const startFailureMemory = time.Minute

// startReadyWait bounds how long a call waits for a service it started.
const startReadyWait = 5 * time.Second

// CallStarting calls the service, starting it first when none answers. Only
// one client starts a given service at a time; the others wait for it. It
// returns ErrUnreachable at once when the socket path cannot be bound or a
// start failed within the last minute, as soon as a service it started dies,
// and after startReadyWait if the service never listens.
func CallStarting(ctx context.Context, socket string, start func() (*os.Process, error), request Request) (Response, error) {
	if len(socket) > maxSocketPath {
		return Response{}, fmt.Errorf("%w: socket path is too long", ErrUnreachable)
	}
	response, err := Call(ctx, socket, request)
	if !errors.Is(err, ErrUnreachable) || startFailedRecently(socket) {
		return response, err
	}
	lock, err := os.OpenFile(socket+".start", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer lock.Close()
	if err := flockWait(ctx, lock, syscall.LOCK_EX); err != nil {
		return Response{}, err
	}
	// A service that exits at once with success found another still holding
	// the socket (one exiting idle); a couple of attempts outlast it.
	for attempt := 0; attempt < 3; attempt++ {
		// A service another caller started meanwhile: let the next caller in
		// before waiting for this answer.
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			lock.Close()
			return Call(ctx, socket, request)
		}
		if startFailedRecently(socket) {
			break
		}
		process, err := start()
		if err != nil {
			recordStartFailure(socket)
			break
		}
		exited := make(chan int, 1)
		go func() {
			state, err := process.Wait()
			code := -1
			if err == nil {
				code = state.ExitCode()
			}
			exited <- code
		}()
		switch waitForService(ctx, socket, exited) {
		case serviceListening:
			// Let the next caller in before waiting for this answer: the start
			// lock covers starting the service, not the search.
			lock.Close()
			return Call(ctx, socket, request)
		case serviceStepAside:
			time.Sleep(50 * time.Millisecond)
			continue
		case serviceCancelled:
			return Response{}, ctx.Err()
		default:
			recordStartFailure(socket)
		}
		break
	}
	return Response{}, fmt.Errorf("%w: the service could not start", ErrUnreachable)
}

type startOutcome int

const (
	serviceListening startOutcome = iota
	serviceStepAside              // exited with success: another service owns the socket
	serviceFailed
	serviceCancelled
)

func waitForService(ctx context.Context, socket string, exited <-chan int) startOutcome {
	deadline := time.After(startReadyWait)
	for {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			return serviceListening
		}
		select {
		case code := <-exited:
			if conn, err := net.Dial("unix", socket); err == nil {
				conn.Close()
				return serviceListening
			}
			if code == 0 {
				return serviceStepAside
			}
			return serviceFailed
		case <-deadline:
			return serviceFailed
		case <-ctx.Done():
			return serviceCancelled
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func startFailedRecently(socket string) bool {
	info, err := os.Stat(socket + ".failed")
	return err == nil && time.Since(info.ModTime()) < startFailureMemory
}

func recordStartFailure(socket string) {
	if file, err := os.Create(socket + ".failed"); err == nil {
		file.Close()
	}
}

// Start launches the service for socket as a detached process of its own
// session, with this process's environment. The returned process is the
// caller's to wait for while it waits for the service to listen.
func Start(executable, socket string, args []string) (*os.Process, error) {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	cmd := exec.Command(executable, args...)
	cmd.Env = os.Environ()
	cmd.Dir = filepath.Dir(socket)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// ServeOptions bounds a service.
type ServeOptions struct {
	Concurrency int           // requests run at once
	Idle        time.Duration // exit after this long with no request
	// Build is the stamp of the executable the service runs; a request from a
	// caller running another, or speaking another protocol, is refused.
	Build BuildStamp
	// Deliver, when set, is applied to each caller's answer with how that
	// caller got it (joined, kept, moved up, its class and queue wait).
	Deliver func(Response, Delivery) Response
	// Keepable reports whether a request's task runs on when its callers
	// leave, and its answer is kept for repeat requests: only one whose
	// inputs cannot move under it.
	Keepable func(Request) bool
	// OnExit, when set, runs as the idle service exits, with the idle time
	// that ended it.
	OnExit func(idle time.Duration)
	// OnIdle, when set, runs once each time the service has had no request
	// for IdleWorkAfter (default 5 s, at most half of Idle) after serving
	// some; the service does not exit while it runs.
	OnIdle        func()
	IdleWorkAfter time.Duration
}

// Serve runs the service on socket until ctx ends or it has been idle for
// opts.Idle. A second service for the same socket returns at once: a lock held
// for the service's life, dropped by the kernel if it dies, names the owner.
// Identical requests in flight together run once and share the response.
func Serve(ctx context.Context, socket string, opts ServeOptions, handle func(context.Context, Request) Response) error {
	lock, err := os.OpenFile(socket+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil // another service owns this socket
		}
		return err
	}
	// A lock a sweep removed after this service opened it names no one: step
	// aside, and the starter tries again with the fresh one.
	if locked, err := lock.Stat(); err != nil {
		return err
	} else if named, err := os.Stat(socket + ".lock"); err != nil || !os.SameFile(locked, named) {
		return nil
	}
	_ = os.Remove(socket) // a dead service's socket
	// Sweep only Neuve's own socket directory, never one a hand-run service
	// was pointed at.
	if dir, err := socketDirectory(); err == nil && dir == filepath.Dir(socket) {
		go sweepStaleServices(dir, socket, time.Now().Add(-staleServiceAge))
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer os.Remove(socket)
	defer listener.Close()
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.Idle <= 0 {
		opts.Idle = time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		active   int
		idleDone = true // nothing to do while idle until a request ran
		lastSeen = time.Now()
		wg       sync.WaitGroup
	)
	touch := func(delta int) {
		mu.Lock()
		active += delta
		idleDone = false
		lastSeen = time.Now()
		mu.Unlock()
	}
	// The idle timer counts from the end of the last task, queued or running,
	// as well as from the last connection.
	tasks := newQueue(ctx, opts.Concurrency, handle, opts.Keepable, func() { touch(0) })
	if opts.IdleWorkAfter <= 0 {
		opts.IdleWorkAfter = min(5*time.Second, opts.Idle/2)
	}
	go func() {
		ticker := time.NewTicker(min(opts.Idle/4, time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				listener.Close()
				return
			case <-ticker.C:
				busy := tasks.busy()
				mu.Lock()
				quiet := time.Since(lastSeen)
				idleWork := active == 0 && !busy && !idleDone && quiet >= opts.IdleWorkAfter
				if idleWork {
					idleDone = true
				}
				idle := active == 0 && !busy && quiet >= opts.Idle
				mu.Unlock()
				if idleWork && opts.OnIdle != nil {
					opts.OnIdle()
					continue // measure the idle period again after the work
				}
				if idle {
					if opts.OnExit != nil {
						opts.OnExit(quiet)
					}
					cancel()
				}
			}
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		mu.Lock()
		idleBefore := time.Duration(0)
		if active == 0 && !tasks.busy() {
			idleBefore = time.Since(lastSeen)
		}
		mu.Unlock()
		arrival := time.Now()
		touch(+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer touch(-1)
			defer conn.Close()
			var wire wireRequest
			if err := readFrame(conn, &wire); err != nil {
				return
			}
			// Only a caller of this protocol running this very executable.
			if wire.Protocol != ProtocolVersion || wire.Build != opts.Build {
				_ = writeFrame(conn, Response{Mismatch: true}.wire())
				return
			}
			request := wire.request()
			request.IdleBefore = idleBefore
			// The client sends nothing more; a read that returns means it
			// closed the connection, which cancels its request.
			requestCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			go func() {
				var probe [1]byte
				_, _ = conn.Read(probe[:])
				cancelRequest()
			}()
			// A run that was cancelled answers nothing; the client then runs
			// the command itself.
			if response, delivery, ok := tasks.ask(requestCtx, request, arrival); ok && requestCtx.Err() == nil {
				if opts.Deliver != nil {
					response = opts.Deliver(response, delivery)
				}
				_ = writeFrame(conn, response.wire())
			}
		}()
	}
}

func flockWait(ctx context.Context, file *os.File, how int) error {
	for {
		err := syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

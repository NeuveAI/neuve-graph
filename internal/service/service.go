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

// Request is one command line for the service to run.
type Request struct {
	Args []string `json:"args"`
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
}

const maxFrameBytes = 1 << 30

// wireRequest and wireResponse carry text as bytes, so arguments, output and
// errors that are not valid UTF-8 arrive exactly as they were sent.
type wireRequest struct {
	Args [][]byte `json:"args"`
}

type wireResponse struct {
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
	Failed bool   `json:"failed"`
	Error  []byte `json:"error,omitempty"`
	Retry  bool   `json:"retry,omitempty"`
}

func (r Request) wire() wireRequest {
	args := make([][]byte, len(r.Args))
	for i, arg := range r.Args {
		args[i] = []byte(arg)
	}
	return wireRequest{Args: args}
}

func (w wireRequest) request() Request {
	args := make([]string, len(w.Args))
	for i, arg := range w.Args {
		args[i] = string(arg)
	}
	return Request{Args: args}
}

func (r Response) wire() wireResponse {
	return wireResponse{Stdout: r.Stdout, Stderr: r.Stderr, Failed: r.Failed, Error: []byte(r.Error), Retry: r.Retry}
}

func (w wireResponse) response() Response {
	return Response{Stdout: w.Stdout, Stderr: w.Stderr, Failed: w.Failed, Error: string(w.Error), Retry: w.Retry}
}

// maxSocketPath is the longest unix socket path every supported platform
// binds (macOS allows 103 bytes and a NUL).
const maxSocketPath = 103

// volatileEnvironment never changes what Graph computes and changes between
// calls from one caller, so it does not choose the service.
var volatileEnvironment = map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true}

// SocketPath names the socket of the service for this executable and
// environment, in a directory only this user can enter.
func SocketPath(executable string, environ []string) (string, error) {
	info, err := os.Stat(executable)
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
	fmt.Fprintf(sum, "graph-service-1\x00%s\x00%d\x00%d\x00", executable, info.Size(), info.ModTime().UnixNano())
	for _, entry := range keys {
		sum.Write([]byte(entry))
		sum.Write([]byte{0})
	}
	dir, err := socketDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hex.EncodeToString(sum.Sum(nil))[:16]+".sock"), nil
}

// socketDirectory is a per-user directory under the temporary directory that
// only its owner can enter, refused if anyone else could have prepared it.
func socketDirectory() (string, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("entire-graph-%d", os.Getuid()))
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
		if response, err := Call(ctx, socket, request); !errors.Is(err, ErrUnreachable) {
			return response, err
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
	_ = os.Remove(socket) // a dead service's socket
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
		lastSeen = time.Now()
		inFlight = map[string]*sharedCall{}
		wg       sync.WaitGroup
		slots    = make(chan struct{}, opts.Concurrency)
	)
	touch := func(delta int) {
		mu.Lock()
		active += delta
		lastSeen = time.Now()
		mu.Unlock()
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
				mu.Lock()
				idle := active == 0 && time.Since(lastSeen) >= opts.Idle
				mu.Unlock()
				if idle {
					cancel()
				}
			}
		}
	}()
	// run joins an identical request already in flight or starts one. The
	// shared run lasts while any of its callers still waits, so one caller
	// leaving never cancels another's answer.
	run := func(requestCtx context.Context, request Request) (Response, bool) {
		key, _ := json.Marshal(request.wire())
		mu.Lock()
		call, joined := inFlight[string(key)]
		if !joined {
			callCtx, cancelCall := context.WithCancel(ctx)
			call = &sharedCall{done: make(chan struct{}), cancel: cancelCall}
			inFlight[string(key)] = call
			go func() {
				// A busy service does not queue a different request: the
				// caller runs it in its own process, as it would without a
				// service, rather than wait behind another search.
				select {
				case slots <- struct{}{}:
					call.response = handle(callCtx, request)
					<-slots
				default:
					call.response = Response{Retry: true}
				}
				if callCtx.Err() != nil {
					call.cancelled = true
				}
				mu.Lock()
				delete(inFlight, string(key))
				mu.Unlock()
				cancelCall()
				close(call.done)
			}()
		}
		call.waiters++
		mu.Unlock()
		select {
		case <-call.done:
			return call.response, !call.cancelled
		case <-requestCtx.Done():
			mu.Lock()
			call.waiters--
			if call.waiters == 0 {
				call.cancel()
			}
			mu.Unlock()
			return Response{}, false
		}
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
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
			request := wire.request()
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
			if response, ok := run(requestCtx, request); ok && requestCtx.Err() == nil {
				_ = writeFrame(conn, response.wire())
			}
		}()
	}
}

type sharedCall struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	response  Response
	cancelled bool
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

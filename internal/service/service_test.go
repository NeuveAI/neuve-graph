//go:build !windows

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shortSocket keeps the socket path under the 104 bytes macOS allows.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// serve runs a service until the test ends; done closes when Serve returns,
// with its error in *result.
func serve(t *testing.T, socket string, opts ServeOptions, handle func(context.Context, Request) Response) (done <-chan struct{}, result *error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var err error
	go func() {
		err = Serve(ctx, socket, opts, handle)
		close(finished)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the service never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { cancel(); <-finished })
	return finished, &err
}

// Identical requests in flight together run once and share the answer;
// different ones run separately.
func TestIdenticalRequestsRunOnce(t *testing.T) {
	socket := shortSocket(t)
	var runs atomic.Int32
	release := make(chan struct{})
	serve(t, socket, ServeOptions{Concurrency: 2}, func(ctx context.Context, request Request) Response {
		runs.Add(1)
		<-release
		return Response{Stdout: []byte(request.Args[0])}
	})
	var wg sync.WaitGroup
	results := make([]Response, 3)
	for i, arg := range []string{"same", "same", "other"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := Call(t.Context(), socket, Request{Args: []string{arg}})
			if err != nil {
				t.Error(err)
			}
			results[i] = response
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs.Load() != 2 || string(results[0].Stdout) != "same" || string(results[1].Stdout) != "same" || string(results[2].Stdout) != "other" {
		t.Fatalf("runs = %d, results = %+v", runs.Load(), results)
	}
}

// A client that goes away cancels its run, so a killed search stops working
// in the service too; one that stays keeps a shared run alive.
func TestLeavingClientCancelsItsRun(t *testing.T) {
	socket := shortSocket(t)
	cancelled := make(chan struct{})
	serve(t, socket, ServeOptions{}, func(ctx context.Context, request Request) Response {
		<-ctx.Done()
		close(cancelled)
		return Response{}
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := Call(ctx, socket, Request{Args: []string{"x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call = %v, want cancellation", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the run kept going after its client left")
	}
}

// A second service for the same socket steps aside at once.
func TestOneServicePerSocket(t *testing.T) {
	socket := shortSocket(t)
	serve(t, socket, ServeOptions{}, func(context.Context, Request) Response { return Response{} })
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), socket, ServeOptions{}, func(context.Context, Request) Response { return Response{} })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second service took over a live socket")
	}
}

// An idle service exits and removes its socket; a busy one does not.
func TestIdleServiceExits(t *testing.T) {
	socket := shortSocket(t)
	done, result := serve(t, socket, ServeOptions{Idle: 300 * time.Millisecond}, func(context.Context, Request) Response {
		time.Sleep(600 * time.Millisecond) // busy past the idle bound
		return Response{Stdout: []byte("ok")}
	})
	if response, err := Call(t.Context(), socket, Request{Args: []string{"x"}}); err != nil || string(response.Stdout) != "ok" {
		t.Fatalf("a busy service did not answer: %+v %v", response, err)
	}
	select {
	case <-done:
		if *result != nil {
			t.Fatal(*result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the idle service did not exit")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
	if _, err := Call(t.Context(), socket, Request{Args: []string{"x"}}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Call after exit = %v, want unreachable", err)
	}
}

// The socket directory must be private to the user, and the socket's name
// follows the executable and the environment but not the volatile variables.
func TestSocketPathIsPrivateAndFollowsTheEnvironment(t *testing.T) {
	tmp, err := os.MkdirTemp("", "gp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	t.Setenv("TMPDIR", tmp)
	executable, _ := os.Executable()
	a, err := SocketPath(executable, []string{"A=1", "PWD=/x"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := SocketPath(executable, []string{"PWD=/y", "A=1"})
	c, _ := SocketPath(executable, []string{"A=2"})
	if a != b || a == c {
		t.Fatalf("socket names: %s %s %s", a, b, c)
	}
	if err := os.Chmod(filepath.Dir(a), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketPath(executable, []string{"A=1"}); err == nil {
		t.Fatal("a socket directory others can enter was accepted")
	}
}

// A busy service never queues a different request: it tells the caller to
// run the command itself, as it would have without a service.
func TestBusyServiceTellsTheCallerToRunItself(t *testing.T) {
	socket := shortSocket(t)
	release := make(chan struct{})
	serve(t, socket, ServeOptions{Concurrency: 1}, func(ctx context.Context, request Request) Response {
		<-release
		return Response{Stdout: []byte("done")}
	})
	first := make(chan Response, 1)
	go func() {
		response, _ := Call(t.Context(), socket, Request{Args: []string{"long"}})
		first <- response
	}()
	time.Sleep(150 * time.Millisecond)
	started := time.Now()
	response, err := Call(t.Context(), socket, Request{Args: []string{"other"}})
	if err != nil || !response.Retry || time.Since(started) > time.Second {
		t.Fatalf("second request = %+v %v after %s, want an immediate retry", response, err, time.Since(started))
	}
	close(release)
	if got := <-first; string(got.Stdout) != "done" {
		t.Fatalf("first request = %+v", got)
	}
}

// Arguments, output and errors travel as bytes: text that is not valid UTF-8
// arrives as it was sent.
func TestInvalidUTF8TravelsUnchanged(t *testing.T) {
	socket := shortSocket(t)
	arg := "zzqq\xffnothere"
	serve(t, socket, ServeOptions{}, func(ctx context.Context, request Request) Response {
		return Response{Stdout: []byte(request.Args[0]), Failed: true, Error: "no symbol " + request.Args[0]}
	})
	response, err := Call(t.Context(), socket, Request{Args: []string{arg}})
	if err != nil || string(response.Stdout) != arg || response.Error != "no symbol "+arg {
		t.Fatalf("response = %q %q %v", response.Stdout, response.Error, err)
	}
}

// A service that dies at start is noticed at once, and later calls do not
// try again for a while; one that steps aside for another is retried.
func TestStartFailuresAreQuickAndRemembered(t *testing.T) {
	socket := shortSocket(t)
	starts := 0
	failing := func() (*os.Process, error) {
		starts++
		return Start("/bin/sh", socket, []string{"-c", "exit 3"})
	}
	began := time.Now()
	if _, err := CallStarting(t.Context(), socket, failing, Request{Args: []string{"x"}}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("CallStarting = %v, want unreachable", err)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second || starts != 1 {
		t.Fatalf("a failed start took %s and %d starts", elapsed, starts)
	}
	began = time.Now()
	if _, err := CallStarting(t.Context(), socket, failing, Request{Args: []string{"x"}}); !errors.Is(err, ErrUnreachable) || starts != 1 || time.Since(began) > 500*time.Millisecond {
		t.Fatalf("a recent failure was retried: %v, %d starts, %s", err, starts, time.Since(began))
	}
	other := shortSocket(t)
	stepAside := 0
	if _, err := CallStarting(t.Context(), other, func() (*os.Process, error) {
		stepAside++
		return Start("/bin/sh", other, []string{"-c", "exit 0"})
	}, Request{Args: []string{"x"}}); !errors.Is(err, ErrUnreachable) || stepAside != 3 {
		t.Fatalf("stepping aside: %v after %d starts, want three tries", err, stepAside)
	}
	long := filepath.Join(os.TempDir(), strings.Repeat("d", 120), "s.sock")
	if _, err := CallStarting(t.Context(), long, failing, Request{}); !errors.Is(err, ErrUnreachable) || starts != 1 {
		t.Fatalf("an unbindable socket path was tried: %v", err)
	}
}

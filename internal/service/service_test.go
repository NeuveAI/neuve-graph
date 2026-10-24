//go:build !windows

package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	deliver := func(response Response, delivery Delivery) Response {
		if delivery.Joined {
			response.Stdout = append([]byte("joined "), response.Stdout...)
		}
		return response
	}
	serve(t, socket, ServeOptions{Concurrency: 2, Deliver: deliver}, func(ctx context.Context, request Request) Response {
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
	// The caller that joined the running "same" gets the Joined form, the
	// one that started it the response as it was.
	same := []string{string(results[0].Stdout), string(results[1].Stdout)}
	if same[0] > same[1] {
		same[0], same[1] = same[1], same[0]
	}
	if runs.Load() != 2 || same[0] != "joined same" || same[1] != "same" || string(results[2].Stdout) != "other" {
		t.Fatalf("runs = %d, results = %q %q %q", runs.Load(), results[0].Stdout, results[1].Stdout, results[2].Stdout)
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
	t.Setenv(EnvDirectory, filepath.Join(tmp, "services"))
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
	// The same executable through a link is the same service.
	link := filepath.Join(tmp, "linked-executable")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	if linked, err := SocketPath(link, []string{"A=1", "PWD=/x"}); err != nil || linked != a {
		t.Fatalf("through a link: %s, %v; want %s", linked, err, a)
	}
	if err := os.Chmod(filepath.Dir(a), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketPath(executable, []string{"A=1"}); err == nil {
		t.Fatal("a socket directory others can enter was accepted")
	}
}

// Touching an executable keeps its service; a new file installed at the
// same path, as an install renames one into place, names another.
func TestSocketPathFollowsTheFileNotItsTimes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(EnvDirectory, filepath.Join(tmp, "services"))
	executable := filepath.Join(tmp, "entire-graph")
	if err := os.WriteFile(executable, []byte("build one"), 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := SocketPath(executable, nil)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(executable, later, later); err != nil {
		t.Fatal(err)
	}
	if touched, _ := SocketPath(executable, nil); touched != before {
		t.Fatal("touching the executable named another service")
	}
	// The old file stays open, as a running service holds it, so the new one
	// cannot take its inode.
	held, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.WriteFile(executable+".new", []byte("build one"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(executable+".new", executable); err != nil {
		t.Fatal(err)
	}
	if installed, _ := SocketPath(executable, nil); installed == before {
		t.Fatal("a new file at the same path shared the old one's service")
	}
}

// A starting service removes the files of services gone for a day, never
// those of a live service, recent ones, its own, or anyone else's sockets.
func TestStartingServiceSweepsStaleSets(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	place := func(name string, modTime time.Time) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{".lock", ".start", ".log", ".failed"} {
		place("graph-aaaa.sock"+suffix, old) // gone a day
		place("graph-bbbb.sock"+suffix, old) // held below
	}
	place("graph-cccc.sock.lock", old)
	place("graph-cccc.sock.log", time.Now()) // used today
	place("budget.sock", old)                // Neuve's, not Graph's
	place("0123456789abcdef0123456789abcdef.sock", old)
	held, err := os.OpenFile(filepath.Join(dir, "graph-bbbb.sock.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	place("graph-self.sock.lock", old)
	sweepStaleServices(dir, filepath.Join(dir, "graph-self.sock"), time.Now().Add(-staleServiceAge))
	for name, want := range map[string]bool{
		"graph-aaaa.sock.lock": false, "graph-aaaa.sock.start": false, "graph-aaaa.sock.log": false, "graph-aaaa.sock.failed": false,
		"graph-bbbb.sock.lock": true, "graph-bbbb.sock.log": true,
		"graph-cccc.sock.lock": true, "graph-cccc.sock.log": true,
		"graph-self.sock.lock": true, "budget.sock": true, "0123456789abcdef0123456789abcdef.sock": true,
	} {
		if _, err := os.Lstat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", name, err == nil, want)
		}
	}
}

// A service answers only a caller of its protocol running its very
// executable; any other is told so and runs the command itself.
func TestServiceRefusesAnotherBuildOrProtocol(t *testing.T) {
	socket := shortSocket(t)
	var runs atomic.Int32
	serve(t, socket, ServeOptions{Build: BuildStamp{Device: 1, Inode: 7, Size: 10}}, func(ctx context.Context, request Request) Response {
		runs.Add(1)
		return Response{Stdout: []byte("ran")}
	})
	same, err := Call(t.Context(), socket, Request{Args: []string{"x"}, Build: BuildStamp{Device: 1, Inode: 7, Size: 10}})
	if err != nil || string(same.Stdout) != "ran" || same.Mismatch {
		t.Fatalf("the same build: %+v, %v", same, err)
	}
	other, err := Call(t.Context(), socket, Request{Args: []string{"x"}, Build: BuildStamp{Device: 1, Inode: 8, Size: 10}})
	if err != nil || !other.Mismatch || len(other.Stdout) != 0 {
		t.Fatalf("another build: %+v, %v", other, err)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	old := wireRequest{Protocol: ProtocolVersion - 1, Build: BuildStamp{Device: 1, Inode: 7, Size: 10}, Args: [][]byte{[]byte("x")}}
	if err := writeFrame(conn, old); err != nil {
		t.Fatal(err)
	}
	var answer wireResponse
	if err := readFrame(conn, &answer); err != nil || !answer.Mismatch {
		t.Fatalf("another protocol: %+v, %v", answer, err)
	}
	if runs.Load() != 1 {
		t.Fatalf("%d runs, want only the matching caller's", runs.Load())
	}
}

// A busy service never queues a different request: it tells the caller to
// A busy service queues a request instead of turning it away, and serves the
// queue interactive first, then by earliest deadline, then by arrival; a
// caller that needs a queued task sooner moves it up, and each caller learns
// how long it waited and at which class its task ran.
func TestBusyServiceQueuesByClassDeadlineAndArrival(t *testing.T) {
	socket := shortSocket(t)
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	deliveries := map[string]Delivery{}
	var deliveriesMu sync.Mutex
	serve(t, socket, ServeOptions{Concurrency: 1, Deliver: func(response Response, delivery Delivery) Response {
		deliveriesMu.Lock()
		deliveries[string(response.Stdout)+fmt.Sprint(delivery.Joined)] = delivery
		deliveriesMu.Unlock()
		return response
	}}, func(ctx context.Context, request Request) Response {
		if request.Args[0] == "blocker" {
			<-release
		}
		mu.Lock()
		order = append(order, request.Args[0])
		mu.Unlock()
		return Response{Stdout: []byte(request.Args[0])}
	})
	var wg sync.WaitGroup
	call := func(request Request) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if response, err := Call(t.Context(), socket, request); err != nil || string(response.Stdout) != request.Args[0] {
				t.Errorf("%v: %+v %v", request.Args, response, err)
			}
		}()
		time.Sleep(60 * time.Millisecond)
	}
	soon := time.Now().Add(time.Minute)
	call(Request{Args: []string{"blocker"}, Class: ClassBackground})
	call(Request{Args: []string{"background-first"}, Class: ClassBackground})
	call(Request{Args: []string{"background-deadline"}, Class: ClassBackground, Deadline: soon})
	call(Request{Args: []string{"interactive"}, Class: ClassInteractive})
	call(Request{Args: []string{"moved"}, Class: ClassBackground})
	// An interactive caller of "moved" moves it up, ahead of everything
	// queued after the interactive one already there.
	call(Request{Args: []string{"moved"}, Class: ClassInteractive})
	close(release)
	wg.Wait()
	want := []string{"blocker", "interactive", "moved", "background-deadline", "background-first"}
	if !slices.Equal(order, want) {
		t.Fatalf("served %v, want %v", order, want)
	}
	if d := deliveries["movedtrue"]; !d.MovedUp || d.Class != ClassInteractive || d.QueueWait <= 0 {
		t.Fatalf("the caller that moved the task up got %+v", d)
	}
	if d := deliveries["background-firstfalse"]; d.MovedUp || d.Class != ClassBackground || d.QueueWait <= 0 {
		t.Fatalf("a queued background caller got %+v", d)
	}
}

// A keepable task outlives its callers: it runs to the end and its answer
// is kept, so the next identical request is answered at once without a run.
// Another task, whose inputs could move, is cancelled when its callers leave.
func TestKeptTaskOutlivesItsCallers(t *testing.T) {
	socket := shortSocket(t)
	var runs atomic.Int32
	var inputsMoved atomic.Bool
	release := make(chan struct{})
	serve(t, socket, ServeOptions{Keepable: func(request Request) bool { return request.Args[0] == "exact" }}, func(ctx context.Context, request Request) Response {
		runs.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return Response{}
		}
		return Response{Stdout: []byte("answer " + request.Args[0]), StillValid: func() bool { return !inputsMoved.Load() }}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err := Call(ctx, socket, Request{Args: []string{"exact"}}); err == nil {
		t.Fatal("the first caller was answered before the task could end")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for KeptBytes() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the task's answer was never kept")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response, err := Call(t.Context(), socket, Request{Args: []string{"exact"}})
	if err != nil || string(response.Stdout) != "answer exact" || runs.Load() != 1 {
		t.Fatalf("repeat = %+v %v after %d runs, want the kept answer", response, err, runs.Load())
	}
	// Once an input it came from moved, the kept answer is not served: the
	// request runs again.
	inputsMoved.Store(true)
	if response, err := Call(t.Context(), socket, Request{Args: []string{"exact"}}); err != nil || string(response.Stdout) != "answer exact" || runs.Load() != 2 {
		t.Fatalf("after its inputs moved: %+v %v after %d runs, want a new run", response, err, runs.Load())
	}
}

// An idle service exits only once its last task, queued or running, has
// ended, though no caller waits for it.
func TestIdleWaitsForTheLastTask(t *testing.T) {
	socket := shortSocket(t)
	release := make(chan struct{})
	finished := make(chan struct{})
	done, _ := serve(t, socket, ServeOptions{Idle: 200 * time.Millisecond, Keepable: func(Request) bool { return true }}, func(ctx context.Context, request Request) Response {
		<-release
		close(finished)
		return Response{Stdout: []byte("late")}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	Call(ctx, socket, Request{Args: []string{"x"}})
	select {
	case <-done:
		t.Fatal("the service exited with a task running")
	case <-time.After(time.Second):
	}
	close(release)
	<-finished
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle service did not exit after its last task")
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

// Callers that find no service and start one together do not wait for each
// other's searches: the start lock covers the start, not the answer.
func TestStartingCallersDoNotWaitForEachOther(t *testing.T) {
	socket := shortSocket(t)
	release := make(chan struct{})
	var started sync.Once
	serving := make(chan struct{})
	startService := func() (*os.Process, error) {
		started.Do(func() {
			go func() {
				// Up only once the second caller waits on the start lock.
				time.Sleep(400 * time.Millisecond)
				_ = Serve(t.Context(), socket, ServeOptions{Concurrency: 2}, func(ctx context.Context, request Request) Response {
					if request.Args[0] == "slow" {
						<-release
					}
					return Response{Stdout: []byte(request.Args[0])}
				})
			}()
			close(serving)
		})
		// A process that stays up, standing in for the service's own.
		standIn := exec.Command("sleep", "30")
		if err := standIn.Start(); err != nil {
			return nil, err
		}
		t.Cleanup(func() { standIn.Process.Kill() })
		return standIn.Process, nil
	}
	slow := make(chan error, 1)
	go func() {
		_, err := CallStarting(t.Context(), socket, startService, Request{Args: []string{"slow"}})
		slow <- err
	}()
	<-serving
	time.Sleep(100 * time.Millisecond)
	quick, err := CallStarting(t.Context(), socket, startService, Request{Args: []string{"quick"}})
	if err != nil || string(quick.Stdout) != "quick" {
		t.Fatalf("the second caller: %+v %v", quick, err)
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
}

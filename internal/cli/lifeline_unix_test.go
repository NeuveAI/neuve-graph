//go:build !windows

package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"github.com/entireio/entire-graph/internal/service"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLifelineHelperProcess(t *testing.T) {
	mode := os.Getenv("GRAPH_LIFELINE_HELPER")
	if mode == "" {
		t.Skip("helper process only")
	}
	if mode == "forward" {
		if err := Execute("test", []string{"query", "--repo", "/unused-repo", "--head", "--prepared-only", "--cache-dir", "/unused-cache", "--profile", "full", "--format", "json", "--query", "held"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	if mode == "validate" {
		if err := Execute("test", []string{"version"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	ctx, stop, err := interruptibleContext(context.Background(), os.Exit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer stop()
	if mode == "environment" {
		child := exec.CommandContext(ctx, "sh", "-c", `printf '%s' "${ENTIRE_GRAPH_LIFELINE_FD-unset}"; if test -e /dev/fd/3; then printf ' inherited-fd'; fi`)
		child.Stdout = os.Stdout
		if err := child.Run(); err != nil {
			os.Exit(2)
		}
		return
	}
	child := exec.CommandContext(ctx, "sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Println(child.Process.Pid)
	_ = child.Wait()
	// Exercise the forced-exit path as well as context cancellation.
	select {}
}

func lifelineHelper(t *testing.T, mode, value string, files ...*os.File) *exec.Cmd {
	t.Helper()
	helper := exec.Command(os.Args[0], "-test.run=^TestLifelineHelperProcess$")
	helper.Env = append(os.Environ(), "GRAPH_LIFELINE_HELPER="+mode, "ENTIRE_GRAPH_LIFELINE_FD="+value)
	helper.ExtraFiles = files
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	t.Cleanup(func() {
		if helper.Process != nil {
			_ = syscall.Kill(-helper.Process.Pid, syscall.SIGKILL)
		}
	})
	return helper
}

// Why: SIGKILL runs no caller cleanup; EOF must cancel Git children and bound an uncooperative engine.
func TestLifelineEOFStopsEngineAndChild(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	helper := lifelineHelper(t, "wait", "3", read)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- helper.Wait() }()
	select {
	case <-done:
		if helper.ProcessState.ExitCode() != 143 {
			t.Fatalf("exit = %d, want 143", helper.ProcessState.ExitCode())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop within 5s of EOF")
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("child %d outlived the engine", pid)
	}
}

// Why: service daemons, preparation children and Git must never adopt their caller's lifeline.
func TestLifelineIsNotInherited(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	helper := lifelineHelper(t, "environment", "3", read)
	out, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if string(out) != "unsetPASS\n" {
		t.Fatalf("child environment/fd = %q", out)
	}
}

// Why: silently ignoring a broken lifeline would leave the original orphan gap open.
func TestLifelineInvalidDescriptorIsRefused(t *testing.T) {
	for _, value := range []string{"", "no", "-1", "999999999999999999999", "999999"} {
		t.Run(value, func(t *testing.T) {
			helper := lifelineHelper(t, "validate", value)
			var stderr bytes.Buffer
			helper.Stderr = &stderr
			if err := helper.Run(); err == nil || !strings.Contains(stderr.String(), "ENTIRE_GRAPH_LIFELINE_FD") {
				t.Fatalf("invalid lifeline %q: err=%v stderr=%q", value, err, stderr.String())
			}
		})
	}
}

// Why: the production Execute path must stop an index blocked in Git when the caller disappears.
func TestLifelineStopsRealIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	binary := filepath.Join(t.TempDir(), "entire-graph")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/entire-graph").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Lifeline test")
	git(t, repo, "config", "user.email", "lifeline@example.test")
	write(t, repo, "source.go", "package source\nfunc Source() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "source")
	shimDir := t.TempDir()
	pidFile := filepath.Join(shimDir, "git.pid")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = ls-tree ]; then\n echo $$ > " + shellQuote(pidFile) + "\n exec sleep 60\n fi\ndone\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	read, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer writeEnd.Close()
	cmd := exec.Command(binary, "index", "--repo", repo, "--head", "--profile", "full", "--cache-dir", t.TempDir(), "--format", "json")
	cmd.Env = append(os.Environ(), "ENTIRE_GRAPH_SERVICE=off", "ENTIRE_GRAPH_LIFELINE_FD=3", "PATH="+shimDir+":"+os.Getenv("PATH"))
	cmd.ExtraFiles = []*os.File{read}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 1 {
		t.Fatal("index did not reach the held Git command")
	}
	_ = writeEnd.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("real index did not stop within 5s of EOF")
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("Git child %d outlived index", pid)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// Why: EOF must close a forwarding client's connection, so the service drops its waiter.
func TestLifelineForwardingClientLeavesService(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gl-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	t.Setenv(service.EnvDirectory, dir)
	t.Setenv(envService, "require")
	helper := lifelineHelper(t, "forward", "3", read)
	socket, err := service.SocketPath(os.Args[0], helper.Env)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stamp, err := service.StampOf(file)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, left := make(chan struct{}), make(chan struct{})
	serving := make(chan error, 1)
	go func() {
		serving <- service.Serve(ctx, socket, service.ServeOptions{Build: stamp, Concurrency: 1, Idle: time.Minute, Keepable: keepableServiceRead}, func(ctx context.Context, _ service.Request) service.Response {
			close(started)
			<-ctx.Done()
			close(left)
			return service.Response{}
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		select {
		case err := <-serving:
			t.Fatalf("service listen: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() { cancel(); <-serving }()
	var diagnostic, output bytes.Buffer
	helper.Stderr, helper.Stdout = &diagnostic, &output
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- helper.Wait() }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("forwarding client exited early: %v: %s", err, diagnostic.String())
	case <-time.After(5 * time.Second):
		t.Fatal("forwarding client did not reach service")
	}
	_ = write.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarding client did not stop within 5s of EOF")
	}
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("service did not drop the departed client")
	}
}

// delayedLifelineClose holds normal teardown after its read has been closed,
// making the read-error/stop race deterministic instead of relying on scheduling.
type delayedLifelineClose struct {
	io.ReadCloser
	closed  chan struct{}
	release chan struct{}
}

func (f *delayedLifelineClose) Close() error {
	err := f.ReadCloser.Close()
	close(f.closed)
	<-f.release
	return err
}

// Why: normal teardown closes an open lifeline; its read error must never synthesize SIGTERM or exit 143.
func TestLifelineNormalStopDoesNotExit(t *testing.T) {
	read, write := io.Pipe()
	defer write.Close()
	signals := make(chan os.Signal, 1)
	exits := make(chan int, 1)
	ctx, stopInterrupts := watchInterrupts(context.Background(), signals, func() {}, 40*time.Millisecond, func(code int) { exits <- code })
	lifeline := &delayedLifelineClose{ReadCloser: read, closed: make(chan struct{}), release: make(chan struct{})}
	stopLifeline := watchLifeline(ctx, lifeline, signals)
	stopped := make(chan struct{})
	go func() { stopLifeline(); close(stopped) }()
	defer func() { close(lifeline.release); <-stopped; stopInterrupts() }()
	<-lifeline.closed
	select {
	case code := <-exits:
		t.Fatalf("normal lifeline stop forced exit %d", code)
	case <-time.After(150 * time.Millisecond):
	}
	if ctx.Err() != nil {
		t.Fatal("normal lifeline stop cancelled the command")
	}
}

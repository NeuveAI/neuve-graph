//go:build !windows

package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const interruptHelperEnv = "ENTIRE_GRAPH_INTERRUPT_HELPER"

// TestInterruptHelperProcess is the child half of
// TestInterruptKillsChildProcesses: it starts a long child the way every
// production subprocess is started (exec.CommandContext on the command's
// context), reports the child's pid, and waits.
func TestInterruptHelperProcess(t *testing.T) {
	if os.Getenv(interruptHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	ctx, stop, err := interruptibleContext(context.Background(), os.Exit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer stop()
	child := exec.CommandContext(ctx, "sleep", "60")
	if err := child.Start(); err != nil {
		fmt.Println("start:", err)
		os.Exit(2)
	}
	fmt.Println(child.Process.Pid)
	_ = child.Wait()
	os.Exit(0)
}

// SIGTERM to the engine alone (not its process group) must not leave its
// children running: before the handler, the engine died at once and a long
// `git grep` child kept scanning for minutes as an orphan.
func TestInterruptKillsChildProcesses(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			helper := exec.Command(os.Args[0], "-test.run=^TestInterruptHelperProcess$")
			helper.Env = append(os.Environ(), interruptHelperEnv+"=1")
			stdout, err := helper.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil {
				_ = helper.Process.Kill()
				t.Fatalf("helper did not report its child: %v", err)
			}
			childPID, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				_ = helper.Process.Kill()
				t.Fatalf("helper reported %q", line)
			}
			t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
			if err := helper.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- helper.Wait() }()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				_ = helper.Process.Kill()
				t.Fatal("helper did not exit after the signal")
			}
			deadline := time.Now().Add(5 * time.Second)
			for syscall.Kill(childPID, 0) == nil {
				if time.Now().After(deadline) {
					t.Fatalf("child %d still running after the engine exited on %s", childPID, sig)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestWatchInterruptsCancelsThenForcesExit(t *testing.T) {
	signals := make(chan os.Signal, 1)
	exitCode := make(chan int, 1)
	released := false
	ctx, stop := watchInterrupts(context.Background(), signals, func() { released = true }, 50*time.Millisecond, func(code int) { exitCode <- code })
	defer stop()
	signals <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("signal did not cancel the context")
	}
	select {
	case code := <-exitCode:
		if code != 128+int(syscall.SIGTERM) {
			t.Fatalf("exit code %d, want %d", code, 128+int(syscall.SIGTERM))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a command that ignored cancellation was not forced to exit")
	}
	if !released {
		t.Fatal("the handler was not released, so a second signal could not take the default action")
	}
}

func TestWatchInterruptsStopWithoutSignal(t *testing.T) {
	signals := make(chan os.Signal, 1)
	ctx, stop := watchInterrupts(context.Background(), signals, func() {}, time.Millisecond, func(int) { t.Error("exited without a signal") })
	stop()
	if ctx.Err() == nil {
		t.Fatal("stop did not release the context")
	}
}

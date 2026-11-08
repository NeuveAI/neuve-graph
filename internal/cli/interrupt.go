package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// interruptSignals end a command early. The default action for each would kill
// this process at once and leave its Git children running: a long `git grep`
// does not watch its stdin and writes nothing while it scans, so it can outlive
// the process that asked for it by minutes.
var interruptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// interruptGrace is how long a cancelled command may take to unwind before the
// process exits anyway. Cancelling the root context is what stops the children
// (os/exec kills every command started with it); the grace only bounds code
// that does not observe the context.
const interruptGrace = 2 * time.Second

// interruptibleContext returns a context cancelled by the first interrupt
// signal or lifeline EOF. Git children stay in this process's group, so a
// caller that kills the whole group still reaches them even when this process
// is SIGKILLed; this
// handler covers callers that signal only this process. A second signal takes
// the default action. stop releases the handler.
func interruptibleContext(parent context.Context, exit func(int)) (context.Context, func(), error) {
	lifeline, err := openLifeline()
	if err != nil {
		return nil, nil, err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, interruptSignals...)
	ctx, stop := watchInterrupts(parent, signals, func() { signal.Stop(signals) }, interruptGrace, exit)
	stopLifeline := func() {}
	if lifeline != nil {
		stopLifeline = watchLifeline(ctx, lifeline, signals)
	}
	return ctx, func() {
		stopLifeline()
		signal.Stop(signals)
		stop()
	}, nil
}

func watchLifeline(ctx context.Context, lifeline io.ReadCloser, signals chan<- os.Signal) func() {
	stopping := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, lifeline)
		// Check before selecting a ready send: normal Close is not a lost caller.
		select {
		case <-stopping:
			return
		default:
		}
		select {
		case signals <- syscall.SIGTERM:
		case <-stopping:
		case <-ctx.Done():
		}
	}()
	return func() {
		close(stopping)
		_ = lifeline.Close()
	}
}

func watchInterrupts(parent context.Context, signals <-chan os.Signal, release func(), grace time.Duration, exit func(int)) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case sig := <-signals:
			release()
			cancel()
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case <-timer.C:
				exit(interruptExitCode(sig))
			case <-done:
			}
		case <-done:
		}
	}()
	return ctx, func() {
		close(done)
		<-finished
		cancel()
	}
}

func interruptExitCode(sig os.Signal) int {
	if number, ok := sig.(syscall.Signal); ok {
		return 128 + int(number)
	}
	return 1
}

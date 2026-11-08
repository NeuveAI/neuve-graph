package sharedwork

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// Done is evaluated after subscription, giving the test an event barrier without sleeps.
type subscribedContext struct {
	context.Context
	ready chan struct{}
	once  sync.Once
}

func (c *subscribedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}
func subscribe(g *Group[string, int], ctx context.Context, key string, run func(context.Context) (int, error)) <-chan result[int] {
	c := &subscribedContext{Context: ctx, ready: make(chan struct{})}
	done := make(chan result[int], 1)
	go func() { v, e := g.Do(c, key, run); done <- result[int]{v, e} }()
	<-c.ready
	return done
}
func empty(t *testing.T, g *Group[string, int]) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.entries) != 0 {
		t.Fatalf("retained %d settled entries", len(g.entries))
	}
}

// Count the real run boundary and check delivery, key separation and retry after failure.
func TestConcurrentOutcomes(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			var g Group[string, int]
			var calls atomic.Int32
			release := make(chan struct{})
			failure := errors.New("read failed")
			run := func(context.Context) (int, error) {
				calls.Add(1)
				<-release
				if fail {
					return 0, failure
				}
				return 42, nil
			}
			first := subscribe(&g, context.Background(), "a", run)
			second := subscribe(&g, context.Background(), "a", run)
			other := subscribe(&g, context.Background(), "b", run)
			close(release)
			for _, ch := range []<-chan result[int]{first, second, other} {
				r := <-ch
				if fail {
					if r.err != failure {
						t.Fatal(r)
					}
				} else if r.err != nil || r.value != 42 {
					t.Fatal(r)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("runs=%d", calls.Load())
			}
			empty(t, &g)
			v, e := g.Do(context.Background(), "a", func(context.Context) (int, error) { calls.Add(1); return 7, nil })
			if v != 7 || e != nil || calls.Load() != 3 {
				t.Fatal(v, e, calls.Load())
			}
			empty(t, &g)
		})
	}
}

// A departing holder cannot cancel another subscriber; zero remaining demand cancels.
func TestSubscriberCancellation(t *testing.T) {
	var g Group[string, int]
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	started := make(chan context.Context, 1)
	stopped := make(chan struct{})
	first := subscribe(&g, firstCtx, "a", func(ctx context.Context) (int, error) {
		started <- ctx
		<-ctx.Done()
		close(stopped)
		return 0, ctx.Err()
	})
	second := subscribe(&g, secondCtx, "a", func(context.Context) (int, error) { t.Error("duplicate run"); return 0, nil })
	runCtx := <-started
	cancelFirst()
	if r := <-first; r.err != context.Canceled {
		t.Fatal(r)
	}
	if runCtx.Err() != nil {
		t.Fatal("one subscriber cancelled shared run")
	}
	cancelSecond()
	if r := <-second; r.err != context.Canceled {
		t.Fatal(r)
	}
	<-stopped
	// A new request waits for cancellation cleanup, then can claim again.
	v, e := g.Do(context.Background(), "a", func(context.Context) (int, error) { return 9, nil })
	if v != 9 || e != nil {
		t.Fatal(v, e)
	}
	empty(t, &g)
}

// The waiter's own reader takes over after abandonment, at most once per caller.
func TestTakeover(t *testing.T) {
	var g Group[string, int]
	release := make(chan struct{})
	lost := errors.New("reader closed")
	var calls atomic.Int32
	first := subscribe(&g, context.Background(), "a", func(context.Context) (int, error) { calls.Add(1); <-release; return 0, Abandon(lost) })
	run := func(context.Context) (int, error) { calls.Add(1); return 42, nil }
	second := subscribe(&g, context.Background(), "a", run)
	third := subscribe(&g, context.Background(), "a", run)
	close(release)
	if r := <-first; r.err == nil {
		t.Fatal(r)
	}
	for _, ch := range []<-chan result[int]{second, third} {
		if r := <-ch; r.value != 42 || r.err != nil {
			t.Fatal(r)
		}
	}
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
	empty(t, &g)
	// All callers abandoning terminates rather than retrying the same resources.
	release = make(chan struct{})
	fail := func(context.Context) (int, error) { <-release; return 0, Abandon(lost) }
	first = subscribe(&g, context.Background(), "a", fail)
	second = subscribe(&g, context.Background(), "a", fail)
	close(release)
	<-first
	<-second
	empty(t, &g)
}

// Panics are outcomes for every subscriber, and release the key.
func TestPanic(t *testing.T) {
	var g Group[string, int]
	release := make(chan struct{})
	run := func(context.Context) (int, error) { <-release; panic("broken reader") }
	first := subscribe(&g, context.Background(), "a", run)
	second := subscribe(&g, context.Background(), "a", run)
	close(release)
	var prior error
	for _, ch := range []<-chan result[int]{first, second} {
		r := <-ch
		var p *PanicError
		if !errors.As(r.err, &p) || p.Value != "broken reader" {
			t.Fatal(r)
		}
		if prior != nil && prior != r.err {
			t.Fatal("different panic outcomes")
		}
		prior = r.err
	}
	empty(t, &g)
}

// Cancellation must retain the old key until its run actually stops. A fresh
// caller waits for that event, rather than overlapping with cancellation cleanup.
func TestCancelledRunRetainsClaimUntilSettled(t *testing.T) {
	var g Group[string, int]
	ctx, cancel := context.WithCancel(context.Background())
	stopping := make(chan struct{})
	release := make(chan struct{})
	first := subscribe(&g, ctx, "a", func(ctx context.Context) (int, error) { <-ctx.Done(); close(stopping); <-release; return 0, ctx.Err() })
	cancel()
	if r := <-first; r.err != context.Canceled {
		t.Fatal(r)
	}
	<-stopping
	started := make(chan struct{})
	second := subscribe(&g, context.Background(), "a", func(context.Context) (int, error) { close(started); return 9, nil })
	select {
	case <-started:
		t.Fatal("overlapped a stopping run")
	default:
	}
	close(release)
	if r := <-second; r.value != 9 || r.err != nil {
		t.Fatal(r)
	}
	empty(t, &g)
}

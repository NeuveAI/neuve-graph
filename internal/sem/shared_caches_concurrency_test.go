package sem

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type contentSubscriberContext struct {
	context.Context
	ready chan struct{}
	once  sync.Once
}

func (c *contentSubscriberContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

// A cancelled joining search must return before the holder's Git read settles,
// without reading again or interrupting the search that still wants the result.
func TestSharedContentJoiningSearchCancellation(t *testing.T) {
	EnableSharedCaches(true)
	t.Cleanup(func() { EnableSharedCaches(false) })
	started := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	holder := sharedContentReader(context.Background(), func(string) (string, bool, error) {
		close(started)
		<-release
		return "contents", true, nil
	}, "commit", "", 1000)
	holderResult := make(chan contentValue, 1)
	go func() {
		content, ok := holder("cold")
		holderResult <- contentValue{content: content, ok: ok}
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joiningCtx := &contentSubscriberContext{Context: ctx, ready: make(chan struct{})}
	var joiningReads atomic.Int32
	joiner := sharedContentReader(joiningCtx, func(string) (string, bool, error) {
		joiningReads.Add(1)
		return "unexpected", true, nil
	}, "commit", "", 1000)
	joiningResult := make(chan contentValue, 1)
	go func() {
		content, ok := joiner("cold")
		joiningResult <- contentValue{content: content, ok: ok}
	}()
	// Done is evaluated after subscription, so cancellation exercises a joiner.
	select {
	case <-joiningCtx.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("joining search did not subscribe with its context")
	}
	cancel()
	select {
	case got := <-joiningResult:
		if got != (contentValue{}) {
			t.Fatalf("cancelled search returned %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled joining search waited for the holder's Git read")
	}
	select {
	case got := <-holderResult:
		t.Fatalf("holder returned before its read settled: %+v", got)
	default:
	}
	unblock()
	if got := <-holderResult; got != (contentValue{content: "contents", ok: true}) {
		t.Fatalf("holder returned %+v", got)
	}
	if joiningReads.Load() != 0 {
		t.Fatalf("joining search read %d times, want 0", joiningReads.Load())
	}
	if content, ok := holder("cold"); !ok || content != "contents" {
		t.Fatalf("completed cache returned %q, %v", content, ok)
	}
}

// Concurrent search readers use one Git read per cold key.
func TestSharedContentConcurrentMisses(t *testing.T) {
	EnableSharedCaches(true)
	t.Cleanup(func() { EnableSharedCaches(false) })
	var calls atomic.Int32
	start := make(chan struct{})
	release := make(chan struct{})
	read := func(path string) (string, bool, error) {
		if calls.Add(1) == 1 {
			close(start)
		}
		<-release
		return "contents of " + path, true, nil
	}
	first := sharedContentReader(context.Background(), read, "commit", "", 1000)
	second := sharedContentReader(context.Background(), read, "commit", "", 1000)
	var entered sync.WaitGroup
	entered.Add(32)
	var finished sync.WaitGroup
	finished.Add(32)
	for i := range 32 {
		go func() {
			defer finished.Done()
			entered.Done()
			entered.Wait()
			reader := first
			if i%2 == 1 {
				reader = second
			}
			if content, ok := reader("cold"); !ok || content != "contents of cold" {
				t.Errorf("read %q %v", content, ok)
			}
		}()
	}
	<-start
	entered.Wait()
	close(release)
	finished.Wait()
	if calls.Load() != 1 {
		t.Fatalf("Git reads=%d, want 1", calls.Load())
	}
}

// A dead reader must not publish a missing-file fact. Every search either gets
// a completed read from its peer or tries with its own resources at most once.
func TestSharedContentReaderAbandonment(t *testing.T) {
	EnableSharedCaches(true)
	t.Cleanup(func() { EnableSharedCaches(false) })
	var deadCalls, liveCalls atomic.Int32
	start := make(chan struct{})
	release := make(chan struct{})
	dead := sharedContentReader(context.Background(), func(string) (string, bool, error) {
		deadCalls.Add(1)
		close(start)
		<-release
		return "", false, errors.New("Git reader closed")
	}, "commit", "", 1000)
	live := sharedContentReader(context.Background(), func(string) (string, bool, error) { liveCalls.Add(1); return "survived", true, nil }, "commit", "", 1000)
	failed := make(chan bool, 1)
	go func() { _, ok := dead("cold"); failed <- ok }()
	<-start
	result := make(chan string, 1)
	go func() {
		content, ok := live("cold")
		if !ok {
			result <- "missing"
		} else {
			result <- content
		}
	}()
	close(release)
	if <-failed {
		t.Fatal("dead reader returned a file")
	}
	if value := <-result; value != "survived" {
		t.Fatal(value)
	}
	if content, ok := live("cold"); !ok || content != "survived" {
		t.Fatal(content, ok)
	}
	if deadCalls.Load() != 1 || liveCalls.Load() != 1 {
		t.Fatal(deadCalls.Load(), liveCalls.Load())
	}
}

// The shared digest path must produce the same normalized value concurrently
// and preserve the small-input bypass and the existing cache statistics.
func TestSharedDigestConcurrentMisses(t *testing.T) {
	EnableSharedCaches(true)
	t.Cleanup(func() { EnableSharedCaches(false) })
	text := strings.Repeat("func  Big( x int )\n", 400)
	want := normalizedDigest(text)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if got := sharedNormalizedDigest(strings.Clone(text)); got != want {
				t.Errorf("digest %q want %q", got, want)
			}
		})
	}
	wg.Wait()
	if stats := SharedCachesState()["digests"]; stats.Bytes == 0 {
		t.Fatal(stats)
	}
	if sharedNormalizedDigest("short") != normalizedDigest("short") {
		t.Fatal("short digest changed")
	}
}

// Package sharedwork shares unfinished work within one process. Completed values
// belong in the caller's cache, stored inside run before it returns.
package sharedwork

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type abandoned struct{ error }

func (e *abandoned) Unwrap() error { return e.error }

// Abandon reports that this caller's resources went away. Live subscribers take
// over with their own run function; each subscriber can run at most once.
func Abandon(err error) error {
	if err == nil {
		err = errors.New("shared work abandoned")
	}
	return &abandoned{err}
}

// PanicError delivers a run's panic to all subscribers instead of stranding them.
type PanicError struct{ Value any }

func (e *PanicError) Error() string { return fmt.Sprintf("shared work panicked: %v", e.Value) }

type result[V any] struct {
	value V
	err   error
}
type subscriber[V any] struct {
	ctx    context.Context
	run    func(context.Context) (V, error)
	result chan result[V]
	ran    bool
}
type entry[V any] struct {
	subscribers map[*subscriber[V]]struct{}
	cancel      context.CancelFunc
	done        chan struct{}
	stopping    bool
}

// Group's zero value is ready to use. A Group must not be copied after use.
// It retains keys only until the run settles, including cancellation cleanup.
type Group[K comparable, V any] struct {
	mu      sync.Mutex
	entries map[K]*entry[V]
}

// Do subscribes to key's outcome. Only the last subscriber leaving cancels the
// run. Cancellation returns the subscriber's own context error immediately.
func (g *Group[K, V]) Do(ctx context.Context, key K, run func(context.Context) (V, error)) (V, error) {
	var zero V
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		g.mu.Lock()
		e := g.entries[key]
		if e != nil && e.stopping {
			g.mu.Unlock()
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-e.done:
				continue
			}
		}
		fresh := e == nil
		if fresh {
			if g.entries == nil {
				g.entries = make(map[K]*entry[V])
			}
			e = &entry[V]{subscribers: make(map[*subscriber[V]]struct{}), done: make(chan struct{})}
			g.entries[key] = e
		}
		s := &subscriber[V]{ctx: ctx, run: run, result: make(chan result[V], 1)}
		e.subscribers[s] = struct{}{}
		if fresh {
			g.start(key, e, s)
		}
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			delete(e.subscribers, s)
			if len(e.subscribers) == 0 && g.entries[key] == e {
				e.stopping = true
				e.cancel()
			}
			g.mu.Unlock()
			return zero, ctx.Err()
		case r := <-s.result:
			if err := ctx.Err(); err != nil {
				return zero, err
			}
			return r.value, r.err
		}
	}
}

// start runs under g.mu. Runs themselves never hold the mutex.
func (g *Group[K, V]) start(key K, e *entry[V], s *subscriber[V]) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.ctx))
	e.cancel = cancel
	s.ran = true
	go func() {
		value, err := invoke(ctx, s.run)
		cancel()
		g.mu.Lock()
		defer g.mu.Unlock()
		var gone *abandoned
		takeover := errors.As(err, &gone)
		var next *subscriber[V]
		for waiter := range e.subscribers {
			if takeover && !waiter.ran && waiter.ctx.Err() == nil {
				if next == nil {
					next = waiter
				}
				continue
			}
			waiter.result <- result[V]{value, err}
			delete(e.subscribers, waiter)
		}
		if next != nil {
			g.start(key, e, next)
			return
		}
		delete(g.entries, key)
		close(e.done)
	}()
}

func invoke[V any](ctx context.Context, run func(context.Context) (V, error)) (value V, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &PanicError{Value: v}
		}
	}()
	return run(ctx)
}

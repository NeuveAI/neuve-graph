//go:build !windows

package service

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// The classes of a request, in the order the queue serves them.
const (
	ClassInteractive = "interactive"
	ClassBackground  = "background"
)

func classRank(class string) int {
	if class == ClassInteractive {
		return 0
	}
	return 1
}

// Kept answers are bounded in bytes and in age; they live as long as the
// service does. At most maxOrphanTasks tasks run on, queued or running, once
// all their callers left; past that, a task whose callers leave is cancelled.
const (
	keptAnswersMaxBytes = 64 << 20
	keptAnswerAge       = 30 * time.Minute
	maxOrphanTasks      = 32
)

// keptBytes is the size of the kept answers of this process's service.
var keptBytes atomic.Int64

// KeptBytes is the size of the answers the service keeps for repeat requests.
func KeptBytes() int64 { return keptBytes.Load() }

// Delivery is how one caller got its answer.
type Delivery struct {
	Joined    bool          // it joined a task another caller started
	Kept      bool          // the answer was kept from a task that ended before it asked
	MovedUp   bool          // it moved the queued task it joined up
	Class     string        // the task's class when it ran
	QueueWait time.Duration // how long this caller waited for the task to start
}

// task is one request's run, shared by every caller of an identical request.
type task struct {
	key      string
	request  Request
	class    string
	deadline time.Time
	seq      uint64
	waiters  int
	// keep: the task runs to the end when its callers leave, and its answer
	// is kept (Keepable).
	keep      bool
	orphan    bool // it runs on although all its callers left
	queued    bool
	started   time.Time
	runClass  string
	response  Response
	cancelled bool
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
}

// before reports whether the queue serves a before b: the higher class, then
// the earlier deadline (none is latest), then the earlier arrival.
func (a *task) before(b *task) bool {
	if ra, rb := classRank(a.class), classRank(b.class); ra != rb {
		return ra < rb
	}
	if !a.deadline.Equal(b.deadline) {
		if a.deadline.IsZero() || b.deadline.IsZero() {
			return b.deadline.IsZero()
		}
		return a.deadline.Before(b.deadline)
	}
	return a.seq < b.seq
}

type keptAnswer struct {
	response Response
	class    string
	at       time.Time
	bytes    int64
}

// queue runs tasks in at most slots at once, the best queued task first, and
// keeps the answers of tasks that may outlive their callers.
type queue struct {
	mu        sync.Mutex
	ctx       context.Context
	slots     int
	running   int
	waiting   []*task
	tasks     map[string]*task // queued or running
	kept      map[string]*keptAnswer
	keptOrder []string
	keptTotal int64
	orphans   *atomic.Int32 // tasks running on with no caller, in every lane
	seq       uint64
	handle    func(context.Context, Request) Response
	keepable  func(Request) bool
	// finished is told each task's end, and whether no caller was left.
	finished func(request Request, orphan bool)
}

func newQueue(ctx context.Context, slots int, handle func(context.Context, Request) Response, keepable func(Request) bool, orphans *atomic.Int32, finished func(Request, bool)) *queue {
	if keepable == nil {
		keepable = func(Request) bool { return false }
	}
	return &queue{ctx: ctx, slots: slots, tasks: map[string]*task{}, kept: map[string]*keptAnswer{}, handle: handle, keepable: keepable, orphans: orphans, finished: finished}
}

// takeOrphan counts one more task running on with no caller, if the bound
// across every lane allows it.
func (q *queue) takeOrphan() bool {
	for {
		count := q.orphans.Load()
		if count >= maxOrphanTasks {
			return false
		}
		if q.orphans.CompareAndSwap(count, count+1) {
			return true
		}
	}
}

// requestKey names identical requests: the same protocol, build and command
// line. Class and deadline are the caller's, not the request's.
func requestKey(request Request) string {
	wire := request.wire()
	key, _ := json.Marshal(struct {
		Protocol int        `json:"protocol"`
		Build    BuildStamp `json:"build"`
		Args     [][]byte   `json:"args"`
	}{wire.Protocol, wire.Build, wire.Args})
	return string(key)
}

// busy reports whether any task is queued or running.
func (q *queue) busy() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks) > 0
}

// ask answers request for one caller that arrived at arrival: from a kept
// answer, by joining the identical task queued or running, or by queueing a
// new one. ok is false when the task was cancelled or the caller left first.
func (q *queue) ask(callerCtx context.Context, request Request, arrival time.Time) (response Response, delivery Delivery, ok bool) {
	key := requestKey(request)
	q.mu.Lock()
	q.dropExpiredKept(time.Now())
	if kept := q.kept[key]; kept != nil {
		// Served only while every input it came from is as it was: the
		// cache artifacts the run opened and the serving directory's policy
		// files (a re-preparation, eviction or policy edit since).
		if kept.response.StillValid != nil && kept.response.StillValid() {
			q.mu.Unlock()
			return kept.response, Delivery{Kept: true, Class: kept.class}, true
		}
		q.dropKept(key)
	}
	t, joined := q.tasks[key]
	if !joined {
		taskCtx, cancel := context.WithCancel(q.ctx)
		q.seq++
		t = &task{key: key, request: request, class: request.Class, deadline: request.Deadline, seq: q.seq, keep: q.keepable(request), queued: true, done: make(chan struct{}), ctx: taskCtx, cancel: cancel}
		q.tasks[key] = t
		q.waiting = append(q.waiting, t)
		q.dispatch()
	} else if t.queued {
		// A caller that needs the queued task sooner moves it up.
		if classRank(request.Class) < classRank(t.class) {
			t.class, delivery.MovedUp = request.Class, true
		}
		if !request.Deadline.IsZero() && (t.deadline.IsZero() || request.Deadline.Before(t.deadline)) {
			t.deadline, delivery.MovedUp = request.Deadline, true
		}
	}
	if t.orphan {
		t.orphan = false
		q.orphans.Add(-1)
	}
	t.waiters++
	q.mu.Unlock()
	select {
	case <-t.done:
		if t.cancelled {
			return Response{}, Delivery{}, false
		}
		delivery.Joined, delivery.Class = joined, t.runClass
		if t.started.After(arrival) {
			delivery.QueueWait = t.started.Sub(arrival)
		}
		return t.response, delivery, true
	case <-callerCtx.Done():
		q.mu.Lock()
		t.waiters--
		ended := false
		if t.waiters == 0 && q.tasks[t.key] == t {
			if t.keep && q.takeOrphan() {
				t.orphan = true
			} else {
				ended = q.abandon(t)
			}
		}
		q.mu.Unlock()
		// Outside q.mu: finished takes the service's lock, which is held
		// while asking whether the queue is busy.
		if ended && q.finished != nil {
			q.finished(t.request, false)
		}
		return Response{}, Delivery{}, false
	}
}

// abandon cancels a task no caller wants any more: a queued one never runs,
// and ends now (reported true); a running one ends when its run returns.
// q.mu is held.
func (q *queue) abandon(t *task) bool {
	if !t.queued {
		t.cancel()
		return false
	}
	for index, waiting := range q.waiting {
		if waiting == t {
			q.waiting = append(q.waiting[:index], q.waiting[index+1:]...)
			break
		}
	}
	delete(q.tasks, t.key)
	t.queued, t.cancelled = false, true
	t.cancel()
	close(t.done)
	return true
}

// dispatch starts the best queued tasks while slots are free. q.mu is held.
func (q *queue) dispatch() {
	for q.running < q.slots && len(q.waiting) > 0 {
		best := 0
		for index := range q.waiting {
			if q.waiting[index].before(q.waiting[best]) {
				best = index
			}
		}
		t := q.waiting[best]
		q.waiting = append(q.waiting[:best], q.waiting[best+1:]...)
		t.queued, t.started, t.runClass = false, time.Now(), t.class
		q.running++
		go q.execute(t)
	}
}

func (q *queue) execute(t *task) {
	response := q.handle(t.ctx, t.request)
	q.mu.Lock()
	t.response = response
	t.cancelled = t.ctx.Err() != nil
	delete(q.tasks, t.key)
	q.running--
	orphan := t.orphan
	if t.orphan {
		t.orphan = false
		q.orphans.Add(-1)
	}
	if !t.cancelled && t.keep && response.StillValid != nil && !response.Failed && !response.Retry && !response.Mismatch {
		q.keepAnswer(t.key, response, t.runClass)
	}
	q.dispatch()
	q.mu.Unlock()
	t.cancel()
	close(t.done)
	if q.finished != nil {
		q.finished(t.request, orphan)
	}
}

// keepAnswer keeps a finished task's answer for repeat requests, the oldest
// dropped first past the bound. q.mu is held.
func (q *queue) keepAnswer(key string, response Response, class string) {
	size := int64(len(response.Stdout) + len(response.Stderr))
	if size > keptAnswersMaxBytes {
		return
	}
	if old := q.kept[key]; old != nil {
		q.keptTotal -= old.bytes
		delete(q.kept, key)
		for index, kept := range q.keptOrder {
			if kept == key {
				q.keptOrder = append(q.keptOrder[:index], q.keptOrder[index+1:]...)
				break
			}
		}
	}
	q.kept[key] = &keptAnswer{response: response, class: class, at: time.Now(), bytes: size}
	q.keptOrder = append(q.keptOrder, key)
	q.keptTotal += size
	for q.keptTotal > keptAnswersMaxBytes && len(q.keptOrder) > 0 {
		q.dropOldestKept()
	}
	keptBytes.Store(q.keptTotal)
}

// dropExpiredKept drops answers kept longer than keptAnswerAge. q.mu is held.
func (q *queue) dropExpiredKept(now time.Time) {
	for len(q.keptOrder) > 0 {
		oldest := q.kept[q.keptOrder[0]]
		if oldest != nil && now.Sub(oldest.at) < keptAnswerAge {
			break
		}
		q.dropOldestKept()
	}
	keptBytes.Store(q.keptTotal)
}

func (q *queue) dropOldestKept() {
	key := q.keptOrder[0]
	q.keptOrder = q.keptOrder[1:]
	if kept := q.kept[key]; kept != nil {
		q.keptTotal -= kept.bytes
		delete(q.kept, key)
	}
}

// dropKept drops one kept answer. q.mu is held.
func (q *queue) dropKept(key string) {
	kept := q.kept[key]
	if kept == nil {
		return
	}
	q.keptTotal -= kept.bytes
	delete(q.kept, key)
	for index, name := range q.keptOrder {
		if name == key {
			q.keptOrder = append(q.keptOrder[:index], q.keptOrder[index+1:]...)
			break
		}
	}
	keptBytes.Store(q.keptTotal)
}

// lanes are a service's queues: the reads' and any other a request names.
type lanes struct {
	mu     sync.Mutex
	make   func(slots int) *queue
	reads  *queue
	others map[string]*queue
	lane   func(Request) (string, int)
}

func newLanes(make func(int) *queue, readSlots int, lane func(Request) (string, int)) *lanes {
	return &lanes{make: make, reads: make(readSlots), others: map[string]*queue{}, lane: lane}
}

func (l *lanes) queueFor(request Request) *queue {
	if l.lane == nil {
		return l.reads
	}
	name, slots := l.lane(request)
	if name == "" {
		return l.reads
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	q, ok := l.others[name]
	if !ok {
		q = l.make(max(1, slots))
		l.others[name] = q
	}
	return q
}

func (l *lanes) ask(callerCtx context.Context, request Request, arrival time.Time) (Response, Delivery, bool) {
	return l.queueFor(request).ask(callerCtx, request, arrival)
}

// busy reports whether any lane has a task queued or running.
func (l *lanes) busy() bool {
	if l.reads.busy() {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, q := range l.others {
		if q.busy() {
			return true
		}
	}
	return false
}

package node

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moejs/engine"
)

// The memory limit's measurements walk the JavaScript heap. The callback
// of a callback operation in flight, and the promise of a promise
// operation, with the async functions awaiting it, are held only by Go
// until the operation settles, so an installation lists them under a
// limit and reports them through one Runtime.AddLiveRoots registration.
// What a worker reads is Go memory until it is handed over, and charged
// then.

// inflightOp is an operation in flight: what JavaScript waits on.
type inflightOp struct{ v engine.Value }

// track lists v, a callback or a promise, while its operation runs, under
// a memory limit; it returns nil without one.
func (e *env) track(v engine.Value) *inflightOp {
	if e.r.MemoryLimit() <= 0 {
		return nil
	}
	if e.liveReg == nil {
		g := &liveReg{}
		g.e.Store(e)
		g.remove = e.rt.AddLiveRoots(g.roots)
		e.liveReg = g
		e.inflight = map[*inflightOp]struct{}{}
	}
	op := &inflightOp{v: v}
	e.inflight[op] = struct{}{}
	return op
}

// untrack ends the listing of op.
func (e *env) untrack(op *inflightOp) {
	if op != nil {
		delete(e.inflight, op)
	}
}

// liveReg is the registration of an installation with the runtime: Close
// drops the installation from it, and it removes itself at the next
// measurement.
type liveReg struct {
	e      atomic.Pointer[env]
	remove func()
}

// roots adds what the operations in flight keep to a measurement, which
// runs on the loop's goroutine, as track and untrack do.
func (g *liveReg) roots(add func(engine.Value)) {
	e := g.e.Load()
	if e == nil {
		g.remove()
		return
	}
	for op := range e.inflight {
		add(op.v)
	}
}

// budget bounds what the workers hold for JavaScript and have not handed
// over yet, under a memory limit: the files they read and the listings
// they make, at most the limit at once. Without it each worker could read
// a whole limit's worth before the loop charges any of it.
//
// An operation takes what it is about to hold (a file's size, before it
// reads it; a listing's growth, as it grows). Once its work is done, what
// it holds is ready: the loop releases it when it settles the operation,
// having charged what it hands over to JavaScript. An operation whose
// growth does not fit waits, as backpressure, for what will be released
// without it: the ready results, which the loop settles, and the
// operations that began before it. So none waits on itself, and no two on
// each other: an operation that holds nothing yet may wait for any, and
// one that holds some waits until the ready results and the older
// operations have released what they held; the oldest of those that hold
// some, with no ready result left, never waits on a younger one, but goes
// over the budget instead. Only the oldest goes over, so the budget is
// exceeded by at most that one operation's growth. An operation that
// alone needs more than the limit fails, as that operation, with a
// RangeError the program can catch. A wait ends at the runtime's
// interrupt and at Close, and Close, a dropped result and every error
// release what was held.
type budget struct {
	mu      sync.Mutex
	limit   int64
	used    int64                 // what the holders hold, ready included
	ready   int64                 // what the ready results hold
	holders map[*holding]struct{} // those still working that hold some
	readys  map[*holding]struct{} // those whose results are ready
	seq     uint64
	closed  bool
	wake    chan struct{} // closed, and replaced, by each release and by close
}

func newBudget(limit int64) *budget {
	if limit <= 0 {
		return nil
	}
	return &budget{limit: limit, holders: map[*holding]struct{}{}, readys: map[*holding]struct{}{}, wake: make(chan struct{})}
}

// take takes n bytes of the budget for h, waiting as the type describes.
func (b *budget) take(h *holding, n int64, halt func() error) error {
	if b == nil || n <= 0 {
		return nil
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return closedError()
		}
		if h.n+n > b.limit {
			b.mu.Unlock()
			return &tooLargeAlone{what: h.what, limit: b.limit}
		}
		grant := b.used == 0 || b.used+n <= b.limit
		if !grant && h.n > 0 && b.ready == 0 {
			// What will be released without h, the ready results and
			// what the operations older than h hold, is waited for
			// until it is. Once none is left, the rest is held by
			// younger operations, which may wait on h: h goes over.
			grant = true
			for x := range b.holders {
				if x != h && x.seq < h.seq && x.n > 0 {
					grant = false
					break
				}
			}
		}
		if grant {
			if h.n == 0 {
				b.seq++
				h.seq = b.seq
				b.holders[h] = struct{}{}
			}
			h.n += n
			b.used += n
			b.mu.Unlock()
			return nil
		}
		ch := b.wake
		b.mu.Unlock()
		// An interrupt has no channel: it is polled.
		if timer == nil {
			timer = time.NewTimer(20 * time.Millisecond)
		} else {
			timer.Reset(20 * time.Millisecond)
		}
		select {
		case <-ch:
		case <-timer.C:
		}
		if err := halt(); err != nil {
			return err
		}
	}
}

// finished marks what h holds ready: its work is done, and the loop is to
// settle it.
func (b *budget) finished(h *holding) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if _, ok := b.holders[h]; ok && !b.closed {
		delete(b.holders, h)
		b.readys[h] = struct{}{}
		b.ready += h.n
		h.ready = true
	}
	b.mu.Unlock()
}

// release gives back what h holds.
func (b *budget) release(h *holding) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if h.n > 0 && !b.closed {
		b.used -= h.n
		if h.ready {
			b.ready -= h.n
			delete(b.readys, h)
		} else {
			delete(b.holders, h)
		}
		close(b.wake)
		b.wake = make(chan struct{})
	}
	h.n, h.ready = 0, false
	b.mu.Unlock()
}

// dropReady gives back what every ready result holds, once the loop can
// settle none of them: it refused a task, as after Terminate.
func (b *budget) dropReady() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if len(b.readys) > 0 && !b.closed {
		for h := range b.readys {
			b.used -= h.n
			h.n, h.ready = 0, false
		}
		clear(b.readys)
		b.ready = 0
		close(b.wake)
		b.wake = make(chan struct{})
	}
	b.mu.Unlock()
}

// close wakes the operations that wait, which fail, and forgets what is
// held.
func (b *budget) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		b.used, b.ready = 0, 0
		clear(b.holders)
		clear(b.readys)
		close(b.wake)
	}
	b.mu.Unlock()
}

// tooLargeAlone is the error of an operation that alone needs more than
// the memory limit: a RangeError, as webapi's for a body larger than the
// limit.
type tooLargeAlone struct {
	what  string
	limit int64
}

func (t *tooLargeAlone) Error() string {
	return "The " + t.what + " is larger than the memory limit (" + itoa(t.limit) + ")"
}

// holding is what one operation holds of the budget, or charged, for the
// file or listing it hands over: on a worker it takes of the budget, on
// the loop (the synchronous form) it charges at once. Its fields but e,
// sync and what are guarded by the budget's mutex.
type holding struct {
	e     *env
	sync  bool
	what  string // "listing", "file": what its errors name
	n     int64
	seq   uint64
	ready bool
}

// grow takes n more bytes. A growth past the memory limit fails with
// tooLargeAlone in either form: the synchronous form charges, and its
// charge stops the program only when the program keeps too much.
func (h *holding) grow(n int) error {
	if n <= 0 {
		return nil
	}
	if err := h.e.halt(); err != nil {
		return err
	}
	if h.sync {
		limit := h.e.r.MemoryLimit()
		if limit > 0 && h.n+int64(n) > limit {
			return &tooLargeAlone{what: h.what, limit: limit}
		}
		h.n += int64(n)
		return h.e.r.ChargeMemory(n)
	}
	return h.e.budget.take(h, int64(n), h.e.halt)
}

// finished marks the work done; what it holds waits for the loop.
func (h *holding) finished() {
	if h != nil && !h.sync {
		h.e.budget.finished(h)
	}
}

// done releases what it holds; later calls release nothing.
func (h *holding) done() {
	if h == nil || h.sync {
		return
	}
	h.e.budget.release(h)
}

// Package eventloop runs a moejs Runtime on an event loop: the Node.js
// timers (setTimeout, setInterval, setImmediate and their clear functions)
// and host work whose result comes back from another goroutine.
//
// A bare Runtime has no timers, and the settle functions of
// Runtime.NewPromise may only be called from the goroutine that uses the
// runtime. A Loop owns its runtime while it runs, and is the one goroutine
// that uses it then:
//
//	rt := moejs.NewRuntime(moejs.Options{})
//	loop, err := eventloop.New(rt, eventloop.Options{})
//	err = loop.Run(func(rt *moejs.Runtime) error {
//		_, err := rt.RunScript(script) // may call setTimeout
//		return err
//	})
//
// Run returns once nothing keeps the loop alive: no timer or immediate that
// is ref'd, no Hold not yet done, no promise of NewPromise not yet settled
// and no task not yet run. Other goroutines hand work to the loop with
// RunOnLoop, and a host function whose result comes from another goroutine
// returns a promise of NewPromise. Between runs the runtime is the host's
// again.
//
// # Phases
//
// One iteration of the loop has three phases, as Node's timers, poll and
// check phases: the timers due when the phase starts, in order of expiry,
// then of scheduling; the tasks posted before the phase started (RunOnLoop,
// Post, the done function of Hold, the settle function of NewPromise); the
// immediates set before the phase started. The promise jobs and
// queueMicrotask callbacks a callback or task queues run when it returns,
// before the next one. With nothing to run, the loop sleeps until the next
// timer is due or a task is posted.
//
// # Timers
//
// The timer functions follow Node.js:
//   - setTimeout and setInterval return a Timeout object with the methods
//     ref, unref, hasRef, refresh, close and Symbol.toPrimitive, which
//     returns the timer's numeric id; setImmediate returns an Immediate
//     with ref, unref and hasRef.
//   - The callback gets the extra arguments, and the Timeout or Immediate
//     as this. A callback that is not a function is a TypeError with the
//     code ERR_INVALID_ARG_TYPE; a string is not evaluated.
//   - The delay goes through ToNumber and is truncated to whole
//     milliseconds. NaN, a delay below 1 and one above 2147483647 are 1 ms,
//     so a timer set by a callback runs in a later iteration.
//   - An interval is rescheduled for its delay after the time its callback
//     started. refresh restarts a timer's delay from now, and runs a
//     timeout that already fired again; a cleared timer stays cleared.
//   - clearTimeout and clearInterval take a Timeout, or the id its
//     Symbol.toPrimitive returned, as a number or a string, once that was
//     read; clearImmediate takes an Immediate. They ignore anything else,
//     and work inside the timer's own callback.
//   - An unref'd timer or immediate does not keep Run alive, and runs only
//     while something else does.
//
// Differences from Node: the Timeout and Immediate objects have no own
// properties (JSON.stringify gives {}, and constructor.name is "Object"),
// their methods throw a TypeError when called on another object, a Proxy of
// one included, and a Timeout has no Symbol.dispose method. Timers expire
// in the order of their exact due times, where Node compares whole
// milliseconds. timers/promises, the AbortSignal options, util.promisify
// and Node's warning for a delay above 2147483647 are not provided.
//
// # Errors and interrupts
//
// A callback's exception stops the loop, as an uncaught exception ends a
// Node.js process, unless Options.OnError takes it. An interrupt always
// stops it, before it takes the next timer, task or immediate off its
// queues, which keep them. A loop is interrupted through Loop.Interrupt:
// Runtime.Interrupt alone stops the JavaScript running but does not wake a
// loop that sleeps. A rejection no handler catches is the host's business,
// through Runtime.SetPromiseRejectionTracker.
//
// # Memory limit
//
// Options.MemoryLimit of the runtime bounds what the program keeps plus
// what it allocates, over the whole run: Run, or Start until Stop, starts
// a new budget (Runtime.ResetAllocation, which keeps the last measurement
// of what the program retains), and before each timer or immediate
// callback and each task the loop rebases it instead of resetting it
// (Runtime.RebaseAllocation): once enough was allocated since the last
// measurement, it measures what the runtime's JavaScript retains and counts
// that in place of what was allocated. Where an allocation would pass the
// limit before then, the runtime measures first (for an allocation a
// builtin makes, at the latest before the next callback), and stops the
// program only when it keeps too much. The measurement reaches what the loop holds for JavaScript, the
// callbacks and arguments of the pending timers and immediates and the
// promises of NewPromise not yet settled, and through them the async
// functions awaiting those promises: the loop registers them with
// Runtime.AddLiveRoots once the runtime has a limit, and Terminate removes
// them, so that a runtime that gets a new Loop does not keep the old one.
// It does not see values
// only the host's Go code holds, such as the captures of a task posted
// with RunOnLoop, unless the host registers them too. A program that keeps
// what it allocates, across however many callbacks and runs, stops near
// the limit; one that keeps little runs on, however much it allocates in
// all. The loop holds its mutex only while it adds its roots, not while
// the measurement walks the heap, so goroutines that post tasks or settle
// promises do not wait for it. Without a limit the loop does no work for
// it.
//
// A callback that passes the limit stops the loop with its
// *moejs.InterruptedError, as any interrupt does, and so does a
// measurement that finds the program keeps more than the limit: the loop
// interrupts the runtime with the *moejs.MemoryLimitError before it takes
// the next timer, task or immediate off its queues. Either way errors.Is
// finds moejs.ErrMemoryLimit in what Run (or Stop) returns, and the
// interrupt stays pending (Runtime.ClearInterrupt).
//
// Options.ResetMemoryPerTask restores the budget of earlier versions: a
// new one before each callback and task, with the promise jobs it queues.
// The limit then bounds what one macrotask allocates, not what the program
// keeps alive between them.
package eventloop

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moejs"
)

var (
	// ErrTerminated is Run's and Start's error once Terminate ended the
	// loop, and the value Terminate interrupts the runtime with.
	ErrTerminated = errors.New("eventloop: loop terminated")
	// ErrRunning is Run's and Start's error while the loop already runs,
	// started by Run or Start.
	ErrRunning = errors.New("eventloop: loop is already running")
)

// Options configures New.
type Options struct {
	// OnError receives the errors of the callbacks the loop runs: what a
	// timer or immediate callback throws, the first exception the jobs it
	// queued throw (a queueMicrotask callback's), a Go panic as an
	// *moejs.InternalError, and what the settle function of NewPromise
	// fails with. It runs on the loop, which keeps running afterwards.
	// When OnError is nil, such an error stops the loop: Run returns it,
	// or Stop for a loop started with Start, as an uncaught exception ends
	// a Node.js process. An interrupt always stops the loop and is never
	// passed to OnError.
	OnError func(err error)
	// ResetMemoryPerTask resets the budget of the runtime's MemoryLimit
	// before each callback and task (Runtime.ResetAllocation), as before:
	// the limit then bounds what one macrotask allocates, not what the
	// program keeps. By default the budget spans the run and the limit
	// bounds what the program keeps plus what it allocates (see Memory
	// limit in the package documentation).
	ResetMemoryPerTask bool
}

// Loop is an event loop that owns one Runtime while it runs. Its methods
// may be called from any goroutine, except NewPromise, which a host
// function calls on the loop, and Stop and Terminate, which must not be
// called on the loop: they wait for it. On the loop means from Run's fn, a
// callback, a task, a host function JavaScript calls there, or OnError. A
// runtime has at most one Loop.
type Loop struct {
	// The fields the loop reads for every callback and task, never written
	// after New, come first; the mutex, which goroutines posting tasks
	// write all the time, starts at least a cache line after them (pad),
	// so that it does not share one with them wherever the Loop lands.
	rt      *moejs.Runtime
	onError func(error)
	// resetPerTask is Options.ResetMemoryPerTask.
	resetPerTask bool
	epoch        time.Time
	// wake is signalled when a task is posted or a stop requested.
	wake chan struct{}
	// quit is set, with l.mu held, when Stop or StopNoWait asks the running
	// loop to end or Terminate ends it, and cleared by enter: the tasks
	// phase reads it without l.mu (haltedTask).
	quit atomic.Bool

	// err is the error that stops the running loop. Only the goroutine
	// running the loop reads and writes it, after enter reset it.
	err error

	// reg is the loop's registration of its roots with the runtime, made
	// once the runtime has a memory limit (register) and removed once
	// Terminate ended the loop. Only the goroutine using the runtime writes
	// it, under mu, so that Terminate may read it there.
	reg *liveReg
	// The prototypes of the Timeout and Immediate objects, created on
	// first use. Only the goroutine using the runtime touches them.
	timeoutProto, immediateProto *moejs.Object

	_ [64]byte // pad

	mu sync.Mutex
	// cur is the run in progress, nil while the loop does not run.
	cur        *run
	background bool  // started by Start: alive while idle
	stop       bool  // Stop or StopNoWait asked the running loop to end
	terminated bool  // Terminate ended the loop for good
	lastErr    error // what ended the last run (Stop returns it)

	timers     timerHeap
	ids        map[int64]*timer // timers whose id was read, while active
	immediates []*immediate     // set and not yet run, in order
	tasks      []func(*moejs.Runtime) error
	refs       int // active timers and pending immediates that are ref'd
	holds      int // Hold and NewPromise not yet done or settled
	seq        uint64
	lastID     int64
	// batch is the immediates phase's last batch: the immediates still
	// pending in it are taken off l.immediates, but not settled.
	batch []*immediate
	// spare is the tasks phase's last batch, emptied, which the next
	// phase gives l.tasks so that posting reuses it; only runTasks uses it.
	spare []func(*moejs.Runtime) error
	// pending lists the promises of NewPromise of a loop with a memory
	// limit, which only their settle functions keep otherwise, for the
	// measurements (roots); a settled one stays until the list is
	// compacted, at the next measurement or once it reached compactAt.
	pending   []*pending
	compactAt int
}

// run is one run of the loop, by Run or Start.
type run struct {
	done chan struct{} // closed when the run ended
	err  error         // what ended it, set before done is closed
}

// New creates a loop for rt and installs setTimeout, clearTimeout,
// setInterval, clearInterval, setImmediate and clearImmediate as globals
// of rt, as the package documentation describes. The error is SetGlobal's.
func New(rt *moejs.Runtime, opts Options) (*Loop, error) {
	l := &Loop{rt: rt, onError: opts.OnError, resetPerTask: opts.ResetMemoryPerTask, epoch: time.Now(), wake: make(chan struct{}, 1)}
	if err := l.install(); err != nil {
		return nil, err
	}
	l.register()
	return l, nil
}

// Runtime returns the loop's runtime.
func (l *Loop) Runtime() *moejs.Runtime { return l.rt }

// Run calls fn with the runtime on the calling goroutine and then runs the
// loop there until nothing keeps it alive, Stop or StopNoWait ends it, or
// an error or an interrupt stops it (Options.OnError, Interrupt). It
// returns fn's error at once when fn fails, the error that stopped the
// loop (an *moejs.InterruptedError for an interrupt), ErrTerminated once
// Terminate ended it, or nil. fn may be nil. fn runs on the loop: it must
// not call Stop or Terminate, which would wait for it.
//
// What is still pending when Run returns, an unref'd timer or immediate, or
// anything after a stop, an error or an interrupt, stays pending: the next
// Run or Start continues it, and JavaScript the host runs on the runtime
// between them (Call) can set more. The runtime is the host's again between
// them. Run ends by releasing what would keep the values the callbacks and
// tasks made and dropped reachable for Go's collector until the runtime
// runs code again: the engine's scratch (engine Realm.ReleaseScratch), the
// registers and frame records they used, and the loop's own last batch of
// immediates and settled promises. A loop started with Start does the same
// when it ends.
//
// A panic in fn or in a host task (the functions of RunOnLoop, of done and
// of settle, and OnError) is not recovered: it unwinds through Run, or
// ends the program for a loop started with Start. The loop is marked
// stopped first, and the tasks and immediates its phase had not run yet
// stay queued; the task that panicked is gone, and a promise whose settle
// function panicked stays pending.
func (l *Loop) Run(fn func(*moejs.Runtime) error) (err error) {
	if err := l.enter(false); err != nil {
		return err
	}
	defer func() { l.exit(err) }()
	l.register()
	if fn != nil {
		err = fn(l.rt)
	}
	if err == nil {
		err = l.loop()
	}
	l.rt.Realm().ReleaseScratch()
	l.mu.Lock()
	if l.terminated {
		err = ErrTerminated
	}
	l.mu.Unlock()
	return err
}

// Start runs the loop on a new goroutine until Stop, StopNoWait or
// Terminate ends it, or an error stops it (Options.OnError): idle, it
// waits for tasks (RunOnLoop) and timers. While it runs, the host uses the
// runtime only through RunOnLoop. It returns ErrRunning when the loop
// already runs and ErrTerminated once Terminate ended it.
func (l *Loop) Start() error {
	if err := l.enter(true); err != nil {
		return err
	}
	l.register()
	go func() {
		var err error
		defer func() { l.exit(err) }()
		err = l.loop()
		l.rt.Realm().ReleaseScratch()
	}()
	return nil
}

// Stop ends the running loop, started by Run or Start, after the callback
// or task it is running returns (or Run's fn), and waits until it has
// ended. Timers, immediates, holds and posted tasks stay pending for the
// next Run or Start. Stop returns what ended the run it waited for, or the
// last run when none was running, as Run returns it: nil, the error that
// stopped it (Options.OnError), the *moejs.InterruptedError of an
// interrupt or ErrTerminated; for a loop started with Start it is how the
// host learns of that error. It must not be called on the loop, where it
// would wait for itself; StopNoWait may be.
func (l *Loop) Stop() error {
	l.mu.Lock()
	r := l.cur
	if r == nil {
		defer l.mu.Unlock()
		return l.lastErr
	}
	l.stop = true
	l.quit.Store(true)
	l.mu.Unlock()
	l.signal()
	<-r.done
	return r.err
}

// StopNoWait is Stop without the wait: the loop ends after the callback or
// task it is running returns. It may be called on the loop.
func (l *Loop) StopNoWait() {
	l.mu.Lock()
	if l.cur != nil {
		l.stop = true
		l.quit.Store(true)
	}
	l.mu.Unlock()
	l.signal()
}

// Terminate ends the loop for good: it interrupts the runtime
// (Runtime.Interrupt with ErrTerminated), which stops the JavaScript the
// loop runs, drops every pending timer, immediate and task, and waits until
// a running loop has ended. Run and Start then return ErrTerminated,
// RunOnLoop returns false, the done function of Hold and the settle
// function of NewPromise do nothing, and setTimeout, setInterval and
// setImmediate throw. The interrupt is left pending also when no
// JavaScript was running, so the host calls Runtime.ClearInterrupt before
// it uses the runtime again; a new Loop may be created for it then. It
// must not be called on the loop, where it would wait for itself.
func (l *Loop) Terminate() {
	l.mu.Lock()
	if !l.terminated {
		l.terminated = true
		l.quit.Store(true)
		// Before the loop can see terminated, so that the interrupt never
		// lands after a ClearInterrupt the host makes once Run returned.
		l.rt.Interrupt(ErrTerminated)
		l.drop()
		if l.reg != nil {
			// The runtime lets go of the loop now, and of the registration at
			// its next measurement or once a run ends (exit): this may run
			// on another goroutine than the runtime's.
			l.reg.loop.Store(nil)
		}
	}
	r := l.cur
	l.mu.Unlock()
	l.signal()
	if r != nil {
		<-r.done
	}
}

// Interrupt interrupts the runtime (Runtime.Interrupt with v) and wakes the
// loop, which Runtime.Interrupt alone leaves asleep until its next timer
// or task. The JavaScript running stops, and the loop stops before it takes
// another timer, task or immediate off its queues, which keep them: Run
// returns the *moejs.InterruptedError, and Stop for a loop started with
// Start. As after Runtime.Interrupt, the interrupt stays pending, so the
// host calls Runtime.ClearInterrupt before it runs the loop or uses the
// runtime again. Interrupt may be called from any goroutine, the loop
// included.
func (l *Loop) Interrupt(v any) {
	l.rt.Interrupt(v)
	l.signal()
}

// RunOnLoop posts fn to the loop, which calls it with the runtime in its
// next tasks phase, and reports true. It may be called from any goroutine,
// the loop included. When the loop does not run, fn waits for the next Run
// or Start; a posted task keeps Run alive until it ran. RunOnLoop returns
// false once Terminate ended the loop, and fn is dropped.
func (l *Loop) RunOnLoop(fn func(*moejs.Runtime)) bool {
	if fn == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		return !l.terminated
	}
	return l.post(func(rt *moejs.Runtime) error {
		fn(rt)
		return nil
	})
}

// Hold keeps the loop alive for host work in progress, until done is
// called. done may be called from any goroutine: it posts fn to the loop as
// RunOnLoop does (fn may be nil) and releases the hold. The first call
// counts and later calls do nothing; after Terminate, done drops fn. Hold
// may be called from any goroutine, but only a hold taken on the loop (in a
// host function or a callback) keeps a running Run from returning: one
// taken on another goroutine races with a Run that has nothing else left,
// and almost always loses it, so that it only keeps the next Run alive.
func (l *Loop) Hold() (done func(fn func(*moejs.Runtime))) {
	release := l.hold()
	return func(fn func(*moejs.Runtime)) {
		if fn == nil {
			release(nil)
			return
		}
		release(func(rt *moejs.Runtime) error {
			fn(rt)
			return nil
		})
	}
}

// NewPromise creates a pending promise for a host function to return, and
// keeps the loop alive until settle is called. It is called on the loop.
// settle may be called from any goroutine: the first call posts f to the
// loop, which settles the promise with what f returns there, and later
// calls do nothing. A nil error fulfills it with the value, or adopts the
// value when it is a thenable; any other error rejects it with the value
// JavaScript would catch from a host function that returned the error
// (Runtime.ThrownValue). An *moejs.InterruptedError leaves it pending and
// stops the loop. A nil f fulfills it with undefined. The jobs settling
// queues run before the loop goes on; what they throw, and what settling
// fails with (moejs.ErrForeign), goes to Options.OnError.
func (l *Loop) NewPromise() (p moejs.Value, settle func(f func(*moejs.Runtime) (moejs.Value, error))) {
	p, resolve, reject := l.rt.NewPromise()
	pd := &pending{l: l, resolve: resolve, reject: reject}
	l.mu.Lock()
	l.holds++
	if l.reg != nil {
		// A loop with a memory limit lists its promises (register).
		pd.p = p
		if len(l.pending) >= l.compactAt {
			l.compact()
		}
		l.pending = append(l.pending, pd)
	}
	l.mu.Unlock()
	return p, pd.settle
}

// pending is a promise of NewPromise and its resolving functions: a hold
// on the loop, as Hold takes one (holds), until its settle function is
// first called, and, in a loop with a memory limit, a root of the
// measurements (Loop.pending) until its settle task settled it: the async
// functions awaiting it are reachable only through it. released is
// guarded by l.mu; p and settled are written and read only on the
// goroutine using the runtime.
type pending struct {
	l               *Loop
	p               moejs.Value // zero once settled, or when no limit lists it
	resolve, reject func(moejs.Value) error
	released        bool
	settled         bool
}

// settle is the settle function of NewPromise: the first call posts the
// settle task (run).
func (pd *pending) settle(f func(*moejs.Runtime) (moejs.Value, error)) {
	pd.release(func(rt *moejs.Runtime) error { return pd.run(rt, f) })
}

// run is the settle task: it settles the promise with what f returns and
// then stops listing it. An interrupt f returns leaves it pending.
func (pd *pending) run(rt *moejs.Runtime, f func(*moejs.Runtime) (moejs.Value, error)) error {
	v := moejs.Undefined()
	if f != nil {
		var err error
		if v, err = f(rt); err != nil {
			reason, ok := rt.ThrownValue(err)
			if !ok {
				return err
			}
			err = pd.reject(reason)
			pd.settled, pd.p = true, moejs.Value{}
			return err
		}
	}
	err := pd.resolve(v)
	pd.settled, pd.p = true, moejs.Value{}
	return err
}

// release releases the hold once, posting task when it is not nil.
func (pd *pending) release(task func(*moejs.Runtime) error) {
	l := pd.l
	l.mu.Lock()
	if pd.released {
		l.mu.Unlock()
		return
	}
	pd.released = true
	l.holds--
	if task != nil && !l.terminated {
		l.tasks = append(l.tasks, task)
	}
	l.mu.Unlock()
	l.signal()
}

// compact drops the settled promises from l.pending and sets the length
// at which NewPromise compacts again: twice what is left, so that it costs
// a constant per promise. It runs on the goroutine using the runtime, with
// l.mu held.
func (l *Loop) compact() {
	kept := l.pending[:0]
	for _, pd := range l.pending {
		if !pd.settled {
			kept = append(kept, pd)
		}
	}
	clear(l.pending[len(kept):])
	l.pending, l.compactAt = kept, 2*len(kept)+64
}

// hold takes a hold and returns the function that releases it once,
// posting task when it is not nil.
func (l *Loop) hold() func(task func(*moejs.Runtime) error) {
	l.mu.Lock()
	l.holds++
	l.mu.Unlock()
	released := false
	return func(task func(*moejs.Runtime) error) {
		l.mu.Lock()
		if released {
			l.mu.Unlock()
			return
		}
		released = true
		l.holds--
		if task != nil && !l.terminated {
			l.tasks = append(l.tasks, task)
		}
		l.mu.Unlock()
		l.signal()
	}
}

// post queues a task, reporting false once the loop is terminated.
func (l *Loop) post(task func(*moejs.Runtime) error) bool {
	l.mu.Lock()
	if l.terminated {
		l.mu.Unlock()
		return false
	}
	l.tasks = append(l.tasks, task)
	l.mu.Unlock()
	l.signal()
	return true
}

// signal wakes a sleeping loop.
func (l *Loop) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// enter marks the loop running, started by Start when background.
func (l *Loop) enter(background bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.terminated:
		return ErrTerminated
	case l.cur != nil:
		return ErrRunning
	}
	l.cur = &run{done: make(chan struct{})}
	l.background, l.stop, l.lastErr, l.err = background, false, nil, nil
	l.quit.Store(false)
	return nil
}

// exit marks the loop not running, after a run that ended with err.
func (l *Loop) exit(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminated {
		// What the run had in hand when Terminate dropped the rest, and the
		// promises, which only this goroutine touches.
		l.drop()
		l.pending = nil
		if l.reg != nil {
			l.reg.loop.Store(nil)
			l.reg.remove()
			l.reg = nil
		}
		err = ErrTerminated
	} else {
		// What the run's last batch of immediates and its settled
		// promises still keep: a settled immediate its Immediate object,
		// which JavaScript may have given properties, and a settled
		// promise's entry, until the list is compacted, its resolving
		// functions and so its result. The next run needs neither.
		l.batch = nil
		if len(l.pending) != 0 {
			l.compact()
		}
	}
	l.cur.err = err
	close(l.cur.done)
	l.cur, l.background, l.stop, l.lastErr = nil, false, false, err
	l.quit.Store(l.terminated)
}

// halted reports whether the running loop must end now. A pending
// interrupt becomes the error that stops it here, before the caller takes
// anything off a queue, so that what the interrupt stops stays pending; it
// is the run's result also when a stop was asked for too. l.mu is held.
func (l *Loop) halted() bool {
	if l.err == nil {
		l.err = l.rt.Realm().CheckInterrupt()
	}
	return l.err != nil || l.stop || l.terminated
}

// haltedTask is halted for the tasks phase, which takes l.mu only when a
// stop or Terminate was asked for (quit): a pending interrupt is the
// realm's atomic flag, and only this goroutine writes l.err. A stop that
// comes meanwhile ends the loop at the next task, as one that comes while
// a task runs does.
func (l *Loop) haltedTask() bool {
	if l.quit.Load() {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.halted()
	}
	if l.err == nil {
		l.err = l.rt.Realm().CheckInterrupt()
	}
	return l.err != nil
}

// drop discards every pending timer, immediate and task. l.mu is held.
func (l *Loop) drop() {
	for _, t := range l.timers {
		t.index, t.active = -1, false
	}
	for _, im := range l.immediates {
		im.pending, im.ref = false, false
	}
	clear(l.timers)
	l.timers = l.timers[:0]
	clear(l.ids)
	clear(l.immediates)
	l.immediates = nil
	clear(l.tasks)
	l.tasks = nil
	l.batch = nil
	l.refs = 0
}

// now is the loop's clock: the monotonic time since New.
func (l *Loop) now() int64 { return int64(time.Since(l.epoch)) }

// loop runs iterations until the loop must end, and returns the error that
// stopped it.
func (l *Loop) loop() error {
	sleep := time.NewTimer(time.Hour)
	sleep.Stop()
	defer sleep.Stop()
	// The run's budget of the memory limit (budget), which keeps the last
	// measurement of what the program retains.
	l.rt.ResetAllocation()
	for l.wait(sleep) && !l.runTimers() && !l.runTasks() && !l.runImmediates() {
	}
	return l.err
}

// fail handles the error of a callback or task on the loop and reports
// whether it stops the loop.
func (l *Loop) fail(err error) bool {
	if err == nil {
		return false
	}
	// Only an interrupt itself, as the engine sees one: a host function's
	// error that wraps one is an Error JavaScript can catch.
	if _, ok := err.(*moejs.InterruptedError); ok || l.onError == nil {
		l.err = err
		return true
	}
	l.onError(err)
	return false
}

// runTimers is the timers phase: it runs the timers due when it starts, in
// order of expiry, then of scheduling, and reports whether the loop must
// end. A timer set or refreshed meanwhile is due at least 1 ms after the
// phase started, so it waits for a later iteration.
func (l *Loop) runTimers() bool {
	now := l.now()
	budgeted := false
	for {
		l.mu.Lock()
		if l.halted() {
			l.mu.Unlock()
			return true
		}
		if len(l.timers) == 0 || l.timers[0].when > now {
			l.mu.Unlock()
			return false
		}
		if !budgeted && l.budgetDue() {
			// The budget may measure, which it does without l.mu, the
			// due timer still queued among the roots.
			l.mu.Unlock()
			if l.rebase() {
				return true
			}
			budgeted = true
			continue
		}
		budgeted = false
		t := l.timers.pop()
		fn, this, args := t.fn, t.this, t.args
		// Node reschedules an interval from the time its callback starts.
		start := l.now()
		l.mu.Unlock()
		err := l.call(fn, this, args)
		l.mu.Lock()
		switch {
		case !t.active || l.terminated:
			// Cleared by its callback, or dropped by Terminate.
		case t.repeat:
			if t.index >= 0 {
				// Refreshed by its callback: the interval wins, as in Node.
				l.timers.remove(t)
			}
			t.when = start + t.delay
			l.push(t)
		case t.index < 0:
			// A timeout its callback did not refresh. As in Node, it was
			// active, and clearTimeout found it by its id, until now.
			l.deactivate(t)
		}
		l.mu.Unlock()
		if l.fail(err) {
			return true
		}
	}
}

// runTasks is the tasks phase: it runs the tasks posted before it started
// and reports whether the loop must end.
func (l *Loop) runTasks() bool {
	l.mu.Lock()
	batch := l.tasks
	l.tasks, l.spare = l.spare, nil
	l.mu.Unlock()
	i := 0
	// What a stop, an error or a panic of a task leaves of the batch goes
	// back in front of the queue; the panic goes on.
	defer func() {
		if i < len(batch) {
			l.mu.Lock()
			l.requeueTasks(batch[i:])
			l.mu.Unlock()
		}
	}()
	for i < len(batch) {
		if l.haltedTask() || l.budget() {
			return true
		}
		task := batch[i]
		batch[i] = nil
		i++
		if l.fail(task(l.rt)) {
			return true
		}
	}
	if cap(batch) <= 64 {
		l.spare = batch[:0]
	}
	return false
}

// requeueTasks puts the tasks a run left unrun back in front of the
// queue. l.mu is held.
func (l *Loop) requeueTasks(rest []func(*moejs.Runtime) error) {
	if len(rest) != 0 && !l.terminated {
		l.tasks = append(rest[:len(rest):len(rest)], l.tasks...)
	}
}

// runImmediates is the check phase: it runs the immediates set before it
// started, in order, and reports whether the loop must end.
func (l *Loop) runImmediates() bool {
	l.mu.Lock()
	batch := l.immediates
	l.immediates = nil
	// Settled or requeued as the batch runs; the next batch replaces it,
	// and the end of the run drops it (exit).
	l.batch = batch
	l.mu.Unlock()
	i := 0
	// As in runTasks: OnError may panic.
	defer func() {
		if i < len(batch) {
			l.mu.Lock()
			l.requeueImmediates(batch[i:])
			l.mu.Unlock()
		}
	}()
	for i < len(batch) {
		im := batch[i]
		if l.budget() {
			return true
		}
		l.mu.Lock()
		if l.halted() {
			l.mu.Unlock()
			return true
		}
		i++
		if !im.pending {
			l.mu.Unlock()
			continue
		}
		fn, this, args := im.fn, im.this, im.args
		l.settleImmediate(im)
		l.mu.Unlock()
		if l.fail(l.call(fn, this, args)) {
			return true
		}
	}
	return false
}

// call calls a callback.
func (l *Loop) call(fn, this moejs.Value, args []moejs.Value) error {
	_, err := l.rt.CallFunction(fn, this, args...)
	return err
}

// budget prepares the budget of the runtime's memory limit for the next
// callback or task, before the loop takes it off its queue: nothing
// without a limit, a new one with Options.ResetMemoryPerTask, else a
// rebase on what the program keeps (Runtime.RebaseAllocation), which
// measures only now and then, with the roots the loop registered with the
// runtime (roots); when no measurement is due, its check (StartRebase) is
// a few loads. A measurement past the limit interrupts the runtime
// with its *moejs.MemoryLimitError, as the hit of a callback does, and
// budget reports that the loop must end (halted). It runs on the loop
// without l.mu: the measurement walks the heap while other goroutines post
// tasks and settle promises.
func (l *Loop) budget() bool {
	r := l.rt.Realm()
	if l.reg != nil {
		if !r.StartRebase() {
			return false
		}
	} else if r.MemoryLimit() <= 0 {
		return false
	}
	return l.rebase()
}

// budgetDue reports whether budget has work to do: a memory limit with
// Options.ResetMemoryPerTask, the first rebase, which registers the
// loop's roots, or a measurement RebaseAllocation would make
// (Realm.StartRebase, which starts its interval). It measures nothing,
// so it may run with l.mu held.
func (l *Loop) budgetDue() bool {
	r := l.rt.Realm()
	if l.reg != nil {
		return r.StartRebase()
	}
	return r.MemoryLimit() > 0
}

// rebase is the work of budget once budgetDue reported some.
func (l *Loop) rebase() bool {
	if l.resetPerTask {
		l.rt.ResetAllocation()
		return false
	}
	l.register()
	if err := l.rt.RebaseAllocation(nil); err != nil {
		l.rt.Interrupt(err)
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.halted()
	}
	return false
}

// register registers the loop's roots with the runtime (roots), once it
// has a memory limit, unless Options.ResetMemoryPerTask leaves nothing to
// measure: New, Run, Start and the first rebase under a limit set later
// call it, so that the promises of NewPromise are listed from the first.
// It runs on the goroutine using the runtime.
func (l *Loop) register() {
	if l.reg != nil || l.resetPerTask || l.rt.Realm().MemoryLimit() <= 0 {
		return
	}
	g := &liveReg{}
	g.loop.Store(l)
	g.remove = l.rt.AddLiveRoots(g.roots)
	l.mu.Lock()
	l.reg = g
	l.mu.Unlock()
}

// liveReg is a loop's registration of its roots with the runtime
// (Runtime.AddLiveRoots), which keeps it as long as the runtime lives: it
// holds the loop only until Terminate ends the loop, so that a runtime
// that gets a new Loop does not keep the old ones, and removes itself
// from the runtime at the next measurement after that, when exit did not
// (Terminate may run on another goroutine than the runtime's, while the
// loop does not run).
type liveReg struct {
	loop   atomic.Pointer[Loop]
	remove func()
}

// roots adds the loop's roots to a measurement (Loop.roots), or removes
// the registration once the loop was terminated.
func (g *liveReg) roots(add func(moejs.Value)) {
	if l := g.loop.Load(); l != nil {
		l.roots(add)
	} else {
		g.remove()
	}
}

// roots adds to a measurement of the runtime's heap what the loop holds for
// JavaScript: the callbacks, this and arguments of the pending timers and
// immediates, and the promises of NewPromise not yet settled. It holds l.mu
// while it adds them, which only queues them for the measurement's walk:
// the walk runs after it returns. Only the goroutine using the runtime
// changes the JavaScript heap, and nothing allocates for JavaScript with
// l.mu held, so the measurement an allocation makes on the loop never
// waits for it.
func (l *Loop) roots(add func(moejs.Value)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminated {
		return
	}
	call := func(fn, this moejs.Value, args []moejs.Value) {
		add(fn)
		add(this)
		for _, v := range args {
			add(v)
		}
	}
	for _, t := range l.timers {
		call(t.fn, t.this, t.args)
	}
	for _, ims := range [2][]*immediate{l.immediates, l.batch} {
		for _, im := range ims {
			if im.pending {
				call(im.fn, im.this, im.args)
			}
		}
	}
	l.compact()
	for _, pd := range l.pending {
		add(pd.p)
	}
}

// requeueImmediates puts the immediates a run left unrun back in front of
// the queue. l.mu is held.
func (l *Loop) requeueImmediates(rest []*immediate) {
	if len(rest) != 0 && !l.terminated {
		l.immediates = append(rest[:len(rest):len(rest)], l.immediates...)
	}
}

// wait starts an iteration. It reports false when the loop must end: it
// was asked to, an error stopped it, or nothing keeps a loop started by Run
// alive, so that an unref'd timer or immediate does not run then.
// Otherwise it returns at once when tasks or immediates are pending, else
// when the next timer is due or a task is posted.
func (l *Loop) wait(sleep *time.Timer) bool {
	l.mu.Lock()
	if l.halted() || !(l.background || l.refs > 0 || l.holds > 0 || len(l.tasks) > 0) {
		l.mu.Unlock()
		return false
	}
	if len(l.tasks) > 0 || len(l.immediates) > 0 {
		l.mu.Unlock()
		return true
	}
	d := time.Duration(-1)
	if len(l.timers) > 0 {
		d = max(time.Duration(l.timers[0].when-l.now()), 0)
	}
	l.mu.Unlock()
	switch {
	case d == 0:
	case d < 0:
		<-l.wake
	default:
		sleep.Reset(d)
		select {
		case <-l.wake:
			sleep.Stop()
		case <-sleep.C:
		}
	}
	return true
}

// Post posts task to the loop l as l.RunOnLoop does, for a task that can
// fail: the loop handles its error as a callback's. An interrupt stops the
// loop; any other error goes to Options.OnError, or stops the loop when
// OnError is nil. A task that calls JavaScript or settles promises itself
// returns what that fails with, so that an exception of the jobs it runs
// or the error of a memory limit is not lost. Post returns false once
// Terminate ended the loop, and task is dropped. It is a function rather
// than a method so that a program that does not call it carries none of
// it.
func Post(l *Loop, task func(*moejs.Runtime) error) bool {
	if task == nil {
		return l.RunOnLoop(nil)
	}
	return l.post(task)
}

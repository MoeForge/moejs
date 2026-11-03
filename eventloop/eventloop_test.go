package eventloop_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// env is a runtime with a loop and a global log(...) that records its
// arguments joined by spaces.
type env struct {
	rt   *moejs.Runtime
	loop *eventloop.Loop
	mu   sync.Mutex
	logs []string
}

func newEnv(t *testing.T, opts eventloop.Options) *env {
	t.Helper()
	e := &env{rt: moejs.NewRuntime(moejs.Options{})}
	var err error
	e.loop, err = eventloop.New(e.rt, opts)
	require.NoError(t, err)
	require.NoError(t, e.rt.SetGlobal("log", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = a.String()
		}
		e.add(strings.Join(parts, " "))
		return moejs.Undefined(), nil
	})))
	return e
}

func (e *env) add(s string) {
	e.mu.Lock()
	e.logs = append(e.logs, s)
	e.mu.Unlock()
}

func (e *env) log() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.logs...)
}

// script returns a Run function that runs src as a classic script.
func script(t *testing.T, src string) func(*moejs.Runtime) error {
	t.Helper()
	s, err := moejs.CompileScript("test.js", src)
	require.NoError(t, err)
	return func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(s)
		return err
	}
}

// run runs src in the loop and requires Run to succeed.
func (e *env) run(t *testing.T, src string) []string {
	t.Helper()
	require.NoError(t, e.loop.Run(script(t, src)))
	return e.log()
}

// TestTimerOrder checks the order of timers against promise jobs and
// queueMicrotask: the jobs run after the script and after each callback,
// due timers run by expiry, equal delays in creation order.
func TestTimerOrder(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{"sync", "p", "m", "a", "a.p", "a.m", "b", "0", "1", "2", "3", "late"}, e.run(t, `
log("sync");
setTimeout(() => log("late"), 50);
setTimeout(() => {
	log("a");
	Promise.resolve().then(() => log("a.p"));
	queueMicrotask(() => log("a.m"));
}, 1);
setTimeout(() => log("b"), 1);
for (let i = 0; i < 4; i++) setTimeout(() => log(String(i)), 2);
Promise.resolve().then(() => log("p"));
queueMicrotask(() => log("m"));
`))
}

// TestPhaseOrder checks one iteration's phases: due timers, then the tasks
// posted before the tasks phase, then the immediates set before the check
// phase; what a phase adds waits for a later iteration.
func TestPhaseOrder(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	post := func(name string) moejs.NativeFunc {
		return func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			e.loop.RunOnLoop(func(*moejs.Runtime) { e.add(name) })
			return moejs.Undefined(), nil
		}
	}
	require.NoError(t, e.rt.SetGlobal("post", post("task")))
	require.NoError(t, e.rt.SetGlobal("post2", post("task2")))

	// A timer's task and immediates run in its iteration, its timer of 0
	// ms in a later one; an immediate's immediate waits for the next check
	// phase, after the tasks the first one posted.
	got := e.run(t, `
setTimeout(() => {
	log("t");
	setTimeout(() => log("t0"), 0);
	setImmediate(() => { log("i1"); post2(); setImmediate(() => log("i3")); });
	setImmediate(() => log("i2"));
	post();
}, 1);
`)
	require.Len(t, got, 7)
	assert.Equal(t, []string{"t", "task", "i1", "i2"}, got[:4])
	assert.ElementsMatch(t, []string{"task2", "i3", "t0"}, got[4:])
	assert.Less(t, indexOf(got, "task2"), indexOf(got, "i3"))
}

// TestTimerFromTimer checks that a timer of 0 ms set by a timer callback
// runs after the timers due with it, in a later timers phase.
func TestTimerFromTimer(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	got := e.run(t, `
setTimeout(() => { log("t1"); setTimeout(() => log("t3"), 0); setImmediate(() => log("imm")); }, 1);
setTimeout(() => log("t2"), 1);
`)
	require.ElementsMatch(t, []string{"t1", "t2", "t3", "imm"}, got)
	assert.Equal(t, "t1", got[0])
	assert.Less(t, indexOf(got, "imm"), indexOf(got, "t3"), "t3 is not due in t1's timers phase")
	assert.Less(t, indexOf(got, "t2"), indexOf(got, "t3"))
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// TestTasksPhaseSnapshot checks that a task posted by a task waits for the
// next iteration, after the immediates of the first.
func TestTasksPhaseSnapshot(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	e.loop.RunOnLoop(func(rt *moejs.Runtime) {
		e.add("task1")
		e.loop.RunOnLoop(func(*moejs.Runtime) { e.add("task2") })
		_, err := rt.RunScript(mustScript(t, `setImmediate(() => log("imm"))`))
		require.NoError(t, err)
	})
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"task1", "imm", "task2"}, e.log())
}

func mustScript(t *testing.T, src string) *moejs.Script {
	t.Helper()
	s, err := moejs.CompileScript("test.js", src)
	require.NoError(t, err)
	return s
}

// TestImmediates checks setImmediate's order, arguments, this, the
// microtasks after each one, and clearImmediate, also of an immediate of
// the same phase.
func TestImmediates(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{"a", "a.m", "c true x 1"}, e.run(t, `
setImmediate(() => { log("a"); queueMicrotask(() => log("a.m")); clearImmediate(b); });
const b = setImmediate(() => log("b"));
const c = setImmediate(function (s, n) { log("c", String(this === c), s, String(n)); }, "x", 1);
const d = setImmediate(() => log("d"));
clearImmediate(d);
clearImmediate(d);
clearImmediate(undefined); clearImmediate({}); clearImmediate(1); clearImmediate(setTimeout(() => {}, 1));
`))
}

// TestClear checks clearTimeout and clearInterval: inside the timer's own
// callback, of a later timer, by the numeric or string id
// Symbol.toPrimitive returns, by close, and that anything else is ignored.
func TestClear(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{"iv 0", "iv 1", "iv 2"}, e.run(t, `
let n = 0;
const iv = setInterval(() => { log("iv " + n); if (++n === 3) clearInterval(iv); }, 1);
`))
	e.logs = nil
	assert.Equal(t, []string{"this 1", "this 2"}, e.run(t, `
let m = 0;
setInterval(function () { log("this " + ++m); if (m === 2) clearTimeout(this); }, 1);
`))
	e.logs = nil
	assert.Equal(t, []string{"a"}, e.run(t, `
const later = setTimeout(() => log("later"), 30);
setTimeout(() => { log("a"); clearTimeout(later); }, 5);
`))
	e.logs = nil
	assert.Equal(t, []string{"same", "immediate", "kept"}, e.run(t, `
const byNumber = setTimeout(() => log("byNumber"), 1);
clearTimeout(+byNumber);
const byString = setTimeout(() => log("byString"), 1);
clearInterval(String(byString));
const closed = setTimeout(() => log("closed"), 1);
log(closed.close() === closed ? "same" : "other");
const kept = setTimeout(() => log("kept"), 5);
const immediate = setImmediate(() => log("immediate"));
clearTimeout(undefined); clearTimeout(null); clearTimeout({}); clearTimeout("x"); clearTimeout(1.5);
clearTimeout(String(+kept) + " "); clearTimeout(immediate); clearImmediate(kept);
`))
}

// TestIntervalOrder checks that an interval runs once per timers phase and
// is rescheduled after the timers due with it.
func TestIntervalOrder(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	got := e.run(t, `
let n = 0;
const iv = setInterval(() => { log("iv"); if (++n === 3) clearInterval(iv); setImmediate(() => log("imm")); }, 1);
`)
	assert.Equal(t, []string{"iv", "imm", "iv", "imm", "iv", "imm"}, got)
}

// TestRefresh checks refresh: a timeout that fired runs again, a cleared
// one does not, and a pending one moves after the timers due before its
// new expiry.
func TestRefresh(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{"true", "f0", "f1", "f2"}, e.run(t, `
let n = 0;
const f = setTimeout(() => { log("f" + n); if (++n < 3) f.refresh(); }, 1);
log(String(f.refresh() === f));
const c = setTimeout(() => log("cleared"), 1);
clearTimeout(c);
c.refresh();
`))
	e.logs = nil
	assert.Equal(t, []string{"mid", "moved"}, e.run(t, `
const moved = setTimeout(() => log("moved"), 30);
setTimeout(() => moved.refresh(), 10);
setTimeout(() => log("mid"), 35);
`))
}

// TestRef checks ref, unref and hasRef: an unref'd timer or immediate does
// not keep Run alive, and still runs while something else does.
func TestRef(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{"true false true", "true false"}, e.run(t, `
const t = setTimeout(() => log("unref'd timer"), 1);
const i = setImmediate(() => log("unref'd immediate"));
log(String(t.hasRef()), String(t.unref().hasRef()), String(t.ref().hasRef()));
t.unref();
log(String(i.hasRef()), String(i.unref().hasRef()));
`), "Run returns: nothing is ref'd")

	e.logs = nil
	got := e.run(t, `
const r = setTimeout(() => { log("ref'd"); log(String(i.hasRef()), String(r.hasRef())); }, 30);
`)
	require.Len(t, got, 4)
	assert.ElementsMatch(t, []string{"unref'd timer", "unref'd immediate"}, got[:2],
		"the unref'd timer and immediate of the first run run while another timer keeps the loop alive")
	assert.Equal(t, []string{"ref'd", "false true"}, got[2:])

	e.logs = nil
	assert.Equal(t, []string{"kept"}, e.run(t, `
const k = setTimeout(() => log("kept"), 1);
k.unref();
k.ref();
`))
}

// TestDelayAndArguments checks the delay's conversion (ToNumber, which may
// run valueOf or throw) and the callback's arguments and this.
func TestDelayAndArguments(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{"valueOf", "TypeError", "TypeError", "true 5", "nan", "neg", "big", "str"}, e.run(t, `
const t = setTimeout(function (a, b) { log(String(this === t), String(a + b)); }, 1, 2, 3);
setTimeout(() => log("nan"), NaN);
setTimeout(() => log("neg"), -5);
setTimeout(() => log("big"), 2 ** 31);
setTimeout(() => log("str"), "3");
setTimeout(() => {}, { valueOf() { log("valueOf"); return 1; } });
for (const d of [Symbol("x"), 1n]) {
	try { setTimeout(() => log("no"), d); } catch (e) { log(e.name); }
}
`))
}

// TestCallbackTypeError checks Node's TypeError for a callback that is not a
// function; the string form is not evaluated.
func TestCallbackTypeError(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{
		`TypeError ERR_INVALID_ARG_TYPE The "callback" argument must be of type function. Received type string ('log("evaluated")')`,
		`TypeError ERR_INVALID_ARG_TYPE The "callback" argument must be of type function. Received undefined`,
		`TypeError ERR_INVALID_ARG_TYPE The "callback" argument must be of type function. Received an instance of Object`,
	}, e.run(t, `
for (const [f, cb] of [[setTimeout, 'log("evaluated")'], [setInterval, undefined], [setImmediate, {}]]) {
	try { f(cb, 1); } catch (e) { log(e.name, e.code, e.message); }
}
`))
}

// TestHold checks Hold: Run waits for done, called from another goroutine,
// which runs its function on the loop once.
func TestHold(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	require.NoError(t, e.rt.SetGlobal("work", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		cb := moejs.Arg(args, 0)
		done := e.loop.Hold()
		go func() {
			time.Sleep(10 * time.Millisecond)
			for range 2 {
				done(func(rt *moejs.Runtime) {
					_, err := rt.CallFunction(cb, moejs.Undefined(), moejs.String("result"))
					assert.NoError(t, err)
				})
			}
		}()
		return moejs.Undefined(), nil
	})))
	assert.Equal(t, []string{"started", "done result"}, e.run(t, `work(v => log("done " + v)); log("started");`))

	// A done with no function only releases the hold.
	done := e.loop.Hold()
	go done(nil)
	require.NoError(t, e.loop.Run(nil))
}

// TestNewPromise checks NewPromise settled from another goroutine:
// fulfilled with a value, rejected with an Error built from a Go error,
// which unwraps to it when thrown back to the host, and rejected with the
// value of an *Exception.
func TestNewPromise(t *testing.T) {
	errBad := errors.New("bad input")
	e := newEnv(t, eventloop.Options{})
	require.NoError(t, e.rt.SetGlobal("fetch", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		arg := moejs.Arg(args, 0).String()
		p, settle := e.loop.NewPromise()
		go func() {
			settle(func(rt *moejs.Runtime) (moejs.Value, error) {
				switch arg {
				case "bad":
					return moejs.Undefined(), errBad
				case "throw":
					return moejs.Undefined(), &moejs.Exception{Value: moejs.String("thrown")}
				case "nil":
					return moejs.Undefined(), nil
				}
				return rt.FromGo(map[string]any{"v": arg})
			})
			settle(func(*moejs.Runtime) (moejs.Value, error) { panic("settled twice") })
		}()
		return p, nil
	})))
	got := e.run(t, `
fetch("ok").then(o => log("ok " + o.v));
fetch("bad").catch(e => log("bad", String(e instanceof Error), e.message));
fetch("throw").catch(e => log("throw " + e));
fetch("nil").then(v => log("nil " + v));
`)
	assert.ElementsMatch(t, []string{"ok ok", "bad true bad input", "throw thrown", "nil undefined"}, got)

	// A rejection a callback rethrows from a microtask reaches the host as
	// the error, unwrapping to the Go error.
	err := e.loop.Run(script(t, `fetch("bad").catch(e => queueMicrotask(() => { throw e; }));`))
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.ErrorIs(t, err, errBad)
}

// TestErrors checks what a callback's error does: without OnError it stops
// the loop and Run returns it, the timers still pending running in the
// next Run; with OnError the loop goes on. The errors include an exception
// of a queueMicrotask callback, which the job drain after a callback
// returns.
func TestErrors(t *testing.T) {
	src := `
setTimeout(() => { throw new Error("boom"); }, 1);
setTimeout(() => {
	queueMicrotask(() => { throw new TypeError("job"); });
	setImmediate(() => { throw new RangeError("immediate"); });
}, 5);
setTimeout(() => log("after"), 20);
`
	e := newEnv(t, eventloop.Options{})
	var exc *moejs.Exception
	err := e.loop.Run(script(t, src))
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "Error: boom", exc.Error())
	for _, want := range []string{"TypeError: job", "RangeError: immediate"} {
		err = e.loop.Run(nil)
		require.ErrorAs(t, err, &exc, "the next Run goes on")
		assert.Equal(t, want, exc.Error())
	}
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"after"}, e.log())

	var errs []string
	e = newEnv(t, eventloop.Options{OnError: func(err error) { errs = append(errs, err.Error()) }})
	assert.Equal(t, []string{"after"}, e.run(t, src))
	assert.Equal(t, []string{"Error: boom", "TypeError: job", "RangeError: immediate"}, errs)

	// fn's own error ends Run at once.
	errFn := errors.New("fn failed")
	e = newEnv(t, eventloop.Options{})
	assert.Equal(t, errFn, e.loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(mustScript(t, `setTimeout(() => log("pending"), 1)`))
		require.NoError(t, err)
		return errFn
	}))
	assert.Empty(t, e.log())
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"pending"}, e.log())
}

// TestPost checks the errors of Post's tasks: an exception of the jobs a
// task runs and a task's own error go to OnError, or stop the loop without
// it, and the tasks after it run in the next Run.
func TestPost(t *testing.T) {
	errTask := errors.New("task failed")
	post := func(e *env) {
		require.True(t, eventloop.Post(e.loop, func(rt *moejs.Runtime) error {
			_, err := rt.RunScript(mustScript(t, `queueMicrotask(() => { throw new TypeError("job"); }); log("first")`))
			return err
		}))
		require.True(t, eventloop.Post(e.loop, func(*moejs.Runtime) error { return errTask }))
		require.True(t, eventloop.Post(e.loop, func(rt *moejs.Runtime) error {
			_, err := rt.RunScript(mustScript(t, `log("last")`))
			return err
		}))
	}

	var errs []string
	e := newEnv(t, eventloop.Options{OnError: func(err error) { errs = append(errs, err.Error()) }})
	post(e)
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"first", "last"}, e.log())
	assert.Equal(t, []string{"TypeError: job", errTask.Error()}, errs)

	e = newEnv(t, eventloop.Options{})
	post(e)
	var exc *moejs.Exception
	require.ErrorAs(t, e.loop.Run(nil), &exc)
	assert.Equal(t, "TypeError: job", exc.Error())
	assert.Equal(t, errTask, e.loop.Run(nil))
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"first", "last"}, e.log())

	// After Terminate, Post drops the task.
	e.loop.Terminate()
	assert.False(t, eventloop.Post(e.loop, func(*moejs.Runtime) error { t.Error("ran"); return nil }))
}

// TestPanicInCallback checks that a Go panic in a host function a timer
// callback calls is an *InternalError the loop reports.
func TestPanicInCallback(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	require.NoError(t, e.rt.SetGlobal("boom", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		panic("host bug")
	})))
	err := e.loop.Run(script(t, `setTimeout(() => boom(), 1); setTimeout(() => log("after"), 10);`))
	var ie *moejs.InternalError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "host bug", ie.Value)
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"after"}, e.log())
}

// TestTerminate checks Terminate: it interrupts a callback in an endless
// loop, Run returns ErrTerminated, RunOnLoop returns false, the timer
// functions throw, the interrupt stays pending until ClearInterrupt, and a
// new Loop works on the runtime.
func TestTerminate(t *testing.T) {
	e := newEnv(t, eventloop.Options{OnError: func(err error) { t.Errorf("OnError(%v): an interrupt is not a callback error", err) }})
	started := make(chan struct{})
	require.NoError(t, e.rt.SetGlobal("started", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		close(started)
		return moejs.Undefined(), nil
	})))
	go func() {
		<-started
		e.loop.Terminate()
	}()
	err := e.loop.Run(script(t, `setTimeout(() => { started(); for (;;) {} }, 1); setTimeout(() => log("never"), 1000);`))
	assert.ErrorIs(t, err, eventloop.ErrTerminated)
	assert.False(t, e.loop.RunOnLoop(func(*moejs.Runtime) {}))
	assert.ErrorIs(t, e.loop.Run(nil), eventloop.ErrTerminated)
	assert.ErrorIs(t, e.loop.Start(), eventloop.ErrTerminated)
	e.loop.Terminate()
	assert.ErrorIs(t, e.loop.Stop(), eventloop.ErrTerminated, "what ended the last run")

	_, err = e.rt.RunScript(mustScript(t, `1`))
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie, "the interrupt stays pending")
	assert.Equal(t, eventloop.ErrTerminated, ie.Value)
	e.rt.ClearInterrupt()
	res, err := e.rt.RunScript(mustScript(t, `try { setTimeout(() => {}, 1); "no throw" } catch (e) { e.message }`))
	require.NoError(t, err)
	assert.Equal(t, "setTimeout: eventloop: loop terminated", res.String())

	loop, err := eventloop.New(e.rt, eventloop.Options{})
	require.NoError(t, err)
	require.NoError(t, loop.Run(script(t, `setTimeout(() => log("new loop"), 1)`)))
	assert.Equal(t, []string{"new loop"}, e.log())
}

// TestTerminateRunFn checks Terminate during Run's fn, in an endless loop
// of a script, and on a loop that does not run.
func TestTerminateRunFn(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	started := make(chan struct{})
	require.NoError(t, e.rt.SetGlobal("started", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		close(started)
		return moejs.Undefined(), nil
	})))
	go func() {
		<-started
		e.loop.Terminate()
	}()
	assert.ErrorIs(t, e.loop.Run(script(t, `started(); for (;;) {}`)), eventloop.ErrTerminated)

	e = newEnv(t, eventloop.Options{})
	_, err := e.rt.RunScript(mustScript(t, `setTimeout(() => log("dropped"), 1)`))
	require.NoError(t, err)
	done := e.loop.Hold()
	e.loop.Terminate()
	done(func(*moejs.Runtime) { t.Error("done after Terminate ran its function") })
	e.rt.ClearInterrupt()
	assert.ErrorIs(t, e.loop.Run(nil), eventloop.ErrTerminated)
	assert.Empty(t, e.log())
}

// TestTerminateResumedAsync checks Terminate while an async function call a
// job resumed runs: the call's promise stays pending, and async function
// calls whose this is that promise, with or without an await, settle their
// own promises once the runtime is used again.
func TestTerminateResumedAsync(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	started := make(chan struct{})
	require.NoError(t, e.rt.SetGlobal("started", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		close(started)
		return moejs.Undefined(), nil
	})))
	go func() {
		<-started
		e.loop.Terminate()
	}()
	err := e.loop.Run(script(t, `globalThis.held = (async () => { await null; started(); for (;;) {} })();`))
	assert.ErrorIs(t, err, eventloop.ErrTerminated)
	e.rt.ClearInterrupt()

	loop, err := eventloop.New(e.rt, eventloop.Options{})
	require.NoError(t, err)
	require.NoError(t, loop.Run(script(t, `
held.then(() => log("held fulfilled"), () => log("held rejected"));
async function probe() { return "probe"; }
async function probeAwait() { await null; return "probeAwait"; }
const a = probe.call(held), b = probeAwait.call(held);
Promise.all([a, b]).then((v) => log(v.join(" "), a !== held, b !== held));
`)))
	assert.Equal(t, []string{"probe probeAwait true true"}, e.log())
}

// TestStartStop checks a loop started with Start: it runs posted tasks
// while idle, Stop waits for the running task, pending timers survive a
// stop, and Run and Start refuse a running loop.
func TestStartStop(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.NoError(t, e.loop.Stop(), "Stop of a loop that does not run")
	require.NoError(t, e.loop.Start())
	assert.ErrorIs(t, e.loop.Start(), eventloop.ErrRunning)
	assert.ErrorIs(t, e.loop.Run(nil), eventloop.ErrRunning)

	results := make(chan string, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			require.True(t, e.loop.RunOnLoop(func(rt *moejs.Runtime) {
				res, err := rt.RunScript(mustScript(t, fmt.Sprintf(`"task %d"`, i)))
				require.NoError(t, err)
				results <- res.String()
			}))
		})
	}
	wg.Wait()
	got := make([]string, 0, 4)
	for range 4 {
		got = append(got, <-results)
	}
	assert.ElementsMatch(t, []string{"task 0", "task 1", "task 2", "task 3"}, got)

	// Stop waits for the task running, and the loop stops after it, so
	// the timer it set waits for the next Start.
	fired := make(chan struct{})
	require.NoError(t, e.rt.SetGlobal("fired", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		close(fired)
		return moejs.Undefined(), nil
	})))
	inTask := make(chan struct{})
	finished := false
	e.loop.RunOnLoop(func(rt *moejs.Runtime) {
		_, err := rt.RunScript(mustScript(t, `setTimeout(() => { log("timer"); fired(); }, 1)`))
		require.NoError(t, err)
		close(inTask)
		time.Sleep(10 * time.Millisecond)
		finished = true
	})
	<-inTask
	require.NoError(t, e.loop.Stop())
	assert.True(t, finished, "Stop waited for the task")
	assert.Empty(t, e.log(), "the timer waits for the next Start")
	require.NoError(t, e.loop.Start())
	<-fired
	require.NoError(t, e.loop.Stop())
	assert.Equal(t, []string{"timer"}, e.log())

	// StopNoWait from the loop: Run returns once fn does.
	e.logs = nil
	require.NoError(t, e.loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(mustScript(t, `setTimeout(() => log("next run"), 1)`))
		e.loop.StopNoWait()
		return err
	}))
	assert.Empty(t, e.log())
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"next run"}, e.log())
}

// TestStartError checks that, without OnError, an error stops a loop
// started with Start and Stop returns it.
func TestStartError(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	thrown := make(chan struct{})
	require.NoError(t, e.rt.SetGlobal("thrown", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		close(thrown)
		return moejs.Undefined(), nil
	})))
	require.NoError(t, e.loop.Start())
	e.loop.RunOnLoop(func(rt *moejs.Runtime) {
		_, err := rt.RunScript(mustScript(t, `setTimeout(() => { thrown(); throw new Error("background"); }, 1)`))
		require.NoError(t, err)
	})
	<-thrown
	err := e.loop.Stop()
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "Error: background", exc.Error())
}

// TestTopLevelAwait checks a module whose top level awaits a timer, loaded
// inside Run: Load returns ErrModulePending, and the loop runs the timer
// that resumes the module.
func TestTopLevelAwait(t *testing.T) {
	mod, err := moejs.Compile("tla.js", `export let state = "start";
await new Promise(r => setTimeout(r, 10));
state = "done";`)
	require.NoError(t, err)
	e := newEnv(t, eventloop.Options{})
	var loadErr error
	require.NoError(t, e.loop.Run(func(rt *moejs.Runtime) error {
		loadErr = rt.Load(mod)
		if errors.Is(loadErr, moejs.ErrModulePending) {
			return nil
		}
		return loadErr
	}))
	assert.ErrorIs(t, loadErr, moejs.ErrModulePending)
	v, ok := e.rt.Export("state")
	require.True(t, ok)
	assert.Equal(t, "done", v.String())
}

// TestReuseBetweenRuns checks that the runtime is the host's between runs:
// JavaScript a Call runs then can set a timer the next Run runs.
func TestReuseBetweenRuns(t *testing.T) {
	mod, err := moejs.Compile("m.js", `export function later(s) { setTimeout(() => log(s), 1); return "set"; }`)
	require.NoError(t, err)
	e := newEnv(t, eventloop.Options{})
	require.NoError(t, e.rt.Load(mod))
	hook, err := mod.Hook("later")
	require.NoError(t, err)
	res, err := e.rt.Call(hook, moejs.String("from Call"))
	require.NoError(t, err)
	assert.Equal(t, "set", res.String())
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"from Call"}, e.log())
}

// TestConcurrentPosts posts tasks and settles promises from many goroutines
// while timers run, for the race detector.
func TestConcurrentPosts(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	const n = 50
	count := 0
	require.NoError(t, e.rt.SetGlobal("spawn", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		p, settle := e.loop.NewPromise()
		done := e.loop.Hold()
		go func() {
			e.loop.RunOnLoop(func(*moejs.Runtime) { count++ })
			go settle(func(*moejs.Runtime) (moejs.Value, error) { return moejs.Int(1), nil })
			done(func(*moejs.Runtime) { count++ })
		}()
		return p, nil
	})))
	e.run(t, fmt.Sprintf(`
let settled = 0;
for (let i = 0; i < %d; i++) setTimeout(() => spawn().then(v => { settled += v; if (settled === %d) log("all"); }), i %% 5);
`, n, n))
	assert.Equal(t, []string{"all"}, e.log())
	assert.Equal(t, 2*n, count)
}

// setInterrupt installs interrupt(), which interrupts through the loop.
func (e *env) setInterrupt(t *testing.T) {
	t.Helper()
	require.NoError(t, e.rt.SetGlobal("interrupt", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		e.loop.Interrupt("stop")
		return moejs.Undefined(), nil
	})))
}

// TestInterruptKeepsQueues checks that an interrupt pending between two
// callbacks stops the loop before it takes the next timer or immediate off
// its queue: Run returns the *InterruptedError, and after ClearInterrupt
// the next Run runs the rest, each once.
func TestInterruptKeepsQueues(t *testing.T) {
	for _, c := range []struct{ src, first, rest string }{
		{`setTimeout(() => { log("t1"); interrupt(); }, 1); setTimeout(() => log("t2"), 1); setTimeout(() => log("t3"), 1);`, "t1", "t2 t3"},
		{`setImmediate(() => { log("i1"); interrupt(); }); setImmediate(() => log("i2")); setImmediate(() => log("i3"));`, "i1", "i2 i3"},
	} {
		e := newEnv(t, eventloop.Options{})
		e.setInterrupt(t)
		err := e.loop.Run(script(t, c.src))
		var ie *moejs.InterruptedError
		require.ErrorAs(t, err, &ie, c.first)
		assert.Equal(t, "stop", ie.Value)
		assert.Equal(t, []string{c.first}, e.log())
		assert.ErrorAs(t, e.loop.Run(nil), &ie, "the interrupt stays pending, and the queues with it")
		assert.Equal(t, []string{c.first}, e.log())
		e.rt.ClearInterrupt()
		require.NoError(t, e.loop.Run(nil))
		assert.Equal(t, append([]string{c.first}, strings.Fields(c.rest)...), e.log())
	}
}

// TestInterruptKeepsTasks checks that a task posted after the one that
// interrupts does not run with the interrupt pending, where the jobs of the
// promise it settles would be dropped, but in the next Run.
func TestInterruptKeepsTasks(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	var settle func(func(*moejs.Runtime) (moejs.Value, error))
	require.NoError(t, e.rt.SetGlobal("pending", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		var p moejs.Value
		p, settle = e.loop.NewPromise()
		return p, nil
	})))
	run := script(t, `pending().then(v => log("then " + v));`)
	err := e.loop.Run(func(rt *moejs.Runtime) error {
		if err := run(rt); err != nil {
			return err
		}
		e.loop.RunOnLoop(func(*moejs.Runtime) {
			e.add("task")
			e.loop.Interrupt("stop")
		})
		settle(func(*moejs.Runtime) (moejs.Value, error) { return moejs.Int(1), nil })
		return nil
	})
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, []string{"task"}, e.log())
	e.rt.ClearInterrupt()
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"task", "then 1"}, e.log())
}

// TestInterruptWakesLoop checks that Interrupt wakes a loop that sleeps: a
// Run kept alive by a Hold returns the *InterruptedError, and a loop
// started with Start ends by itself, Stop then returning the interrupt.
func TestInterruptWakesLoop(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	done := e.loop.Hold()
	result := make(chan error, 1)
	go func() { result <- e.loop.Run(nil) }()
	time.Sleep(10 * time.Millisecond)
	e.loop.Interrupt("timeout")
	select {
	case err := <-result:
		var ie *moejs.InterruptedError
		require.ErrorAs(t, err, &ie)
		assert.Equal(t, "timeout", ie.Value)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return: Interrupt did not wake the loop")
	}
	e.rt.ClearInterrupt()
	done(func(*moejs.Runtime) { e.add("done") })
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"done"}, e.log())

	require.NoError(t, e.loop.Start())
	e.loop.Interrupt("background")
	deadline := time.Now().Add(10 * time.Second)
	for e.loop.Start() == eventloop.ErrRunning {
		require.True(t, time.Now().Before(deadline), "the loop started with Start did not end")
		time.Sleep(time.Millisecond)
	}
	err := e.loop.Stop()
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "background", ie.Value)
	e.rt.ClearInterrupt()
}

// TestPanicKeepsBatch checks that a panic in a host task, or in OnError,
// leaves the tasks and immediates of its phase that had not run queued for
// the next Run: a promise settled after the panicking task still settles.
func TestPanicKeepsBatch(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	var settle func(func(*moejs.Runtime) (moejs.Value, error))
	require.NoError(t, e.rt.SetGlobal("pending", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		var p moejs.Value
		p, settle = e.loop.NewPromise()
		return p, nil
	})))
	run := script(t, `pending().then(v => log("then " + v));`)
	assert.PanicsWithValue(t, "host task bug", func() {
		_ = e.loop.Run(func(rt *moejs.Runtime) error {
			if err := run(rt); err != nil {
				return err
			}
			e.loop.RunOnLoop(func(*moejs.Runtime) { panic("host task bug") })
			settle(func(*moejs.Runtime) (moejs.Value, error) { return moejs.Int(1), nil })
			e.loop.RunOnLoop(func(*moejs.Runtime) { e.add("task") })
			return nil
		})
	})
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"then 1", "task"}, e.log())

	panicNext := true
	e = newEnv(t, eventloop.Options{OnError: func(error) {
		if panicNext {
			panicNext = false
			panic("OnError bug")
		}
	}})
	assert.PanicsWithValue(t, "OnError bug", func() {
		_ = e.loop.Run(script(t, `setImmediate(() => { throw new Error("x"); }); setImmediate(() => log("i2"));`))
	})
	require.NoError(t, e.loop.Run(nil))
	assert.Equal(t, []string{"i2"}, e.log())
}

// TestWrappedInterrupt checks that a host function's error wrapping an
// *InterruptedError is an Error JavaScript catches and, uncaught, a
// callback error OnError receives, not an interrupt that stops the loop.
func TestWrappedInterrupt(t *testing.T) {
	var errs []error
	e := newEnv(t, eventloop.Options{OnError: func(err error) { errs = append(errs, err) }})
	require.NoError(t, e.rt.SetGlobal("wrapped", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Undefined(), fmt.Errorf("context: %w", &moejs.InterruptedError{Value: "inner"})
	})))
	assert.Equal(t, []string{"caught context: interrupted: inner", "after"}, e.run(t, `
setTimeout(() => { try { wrapped(); } catch (e) { log("caught " + e.message); } wrapped(); }, 1);
setTimeout(() => log("after"), 5);
`))
	require.Len(t, errs, 1)
	var exc *moejs.Exception
	require.ErrorAs(t, errs[0], &exc)
	var ie *moejs.InterruptedError
	assert.ErrorAs(t, errs[0], &ie, "the Exception still unwraps to the host's error")
}

// TestTimeoutIdDuringCallback checks that, as in Node, a timeout's id
// finds it until its callback returns: clearTimeout by id inside the
// callback clears it, so the refresh after it does nothing.
func TestTimeoutIdDuringCallback(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{"ran"}, e.run(t, `
let n = 0;
const t = setTimeout(() => { log("ran"); clearTimeout(id); if (++n < 3) t.refresh(); }, 1);
const id = +t;
`))
}

// TestDelayTruncation checks that a delay is truncated to whole
// milliseconds, as in Node: 1.9 ms and 1 ms run in creation order.
func TestDelayTruncation(t *testing.T) {
	e := newEnv(t, eventloop.Options{})
	assert.Equal(t, []string{"a", "b"}, e.run(t, `setTimeout(() => log("a"), 1.9); setTimeout(() => log("b"), 1);`))
}

// TestMemoryLimit: callbacks that each allocate less than the runtime's
// memory limit and keep nothing run, more in all than the limit, and a
// callback that passes it stops the loop without running its finally.
func TestMemoryLimit(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 4 << 20})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	var finally bool
	require.NoError(t, rt.SetGlobal("ranFinally", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		finally = true
		return moejs.Undefined(), nil
	})))
	err = loop.Run(script(t, `
		var ticks = 0;
		const iv = setInterval(() => {
			const keep = [];
			for (let i = 0; i < 20000; i++) keep.push({i});
			if (++ticks === 10) {
				clearInterval(iv);
				setTimeout(() => {
					const keep = [];
					try { for (;;) keep.push({n: keep.length}); } finally { ranFinally(); }
				});
			}
		});
	`))
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	var interrupted *moejs.InterruptedError
	require.ErrorAs(t, err, &interrupted)
	assert.False(t, finally)
	st := rt.Stats()
	assert.Equal(t, int64(1), st.MemoryLimitHits)
	assert.Greater(t, st.AllocatedBytes, int64(8<<20), "the intervals together allocated more than the limit")
	v, ok := rt.Realm().Global.GetOwnDataValue(rt.Realm().KeyFromGoString("ticks"))
	require.True(t, ok)
	assert.Equal(t, 10.0, v.AsNumber())
}

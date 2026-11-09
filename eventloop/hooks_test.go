package eventloop_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookLog is the host of OnIdle and OnWake: it records their calls and the
// work the loop runs (mark), and fails the test when the calls of a run do
// not alternate, OnIdle first, or work runs while the host counts the loop
// idle: after OnIdle, until an OnWake that returns nil.
type hookLog struct {
	t       *testing.T
	mu      sync.Mutex
	events  []string
	awake   bool
	wakeErr error           // what OnWake returns
	idles   []chan struct{} // closed at the next OnIdle
	wakes   []chan struct{} // closed at the next OnWake
}

// newHookEnv returns an env whose loop has the hooks of a hookLog, and
// the global mark(s), which records s as work.
func newHookEnv(t *testing.T) (*env, *hookLog) {
	t.Helper()
	h := &hookLog{t: t}
	e := newEnv(t, eventloop.Options{OnIdle: h.onIdle, OnWake: h.onWake})
	require.NoError(t, e.rt.SetGlobal("mark", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		h.ran(moejs.Arg(args, 0).String())
		return moejs.Undefined(), nil
	})))
	return e, h
}

// begin marks the host awake, as before a run it took its slot for.
func (h *hookLog) begin() {
	h.mu.Lock()
	h.awake = true
	h.mu.Unlock()
}

func (h *hookLog) onIdle() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.awake {
		h.t.Error("OnIdle while idle")
	}
	h.awake = false
	h.events = append(h.events, "idle")
	for _, c := range h.idles {
		close(c)
	}
	h.idles = nil
}

func (h *hookLog) onWake() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.awake {
		h.t.Error("OnWake while awake")
	}
	h.awake = h.wakeErr == nil
	h.events = append(h.events, "wake")
	for _, c := range h.wakes {
		close(c)
	}
	h.wakes = nil
	return h.wakeErr
}

// ran records work the loop runs, which must not run while it is idle.
func (h *hookLog) ran(what string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.awake {
		h.t.Errorf("%s ran while the loop was idle", what)
	}
	h.events = append(h.events, what)
}

// nextIdle returns a channel closed at the next OnIdle.
func (h *hookLog) nextIdle() <-chan struct{} {
	c := make(chan struct{})
	h.mu.Lock()
	h.idles = append(h.idles, c)
	h.mu.Unlock()
	return c
}

// nextWake returns a channel closed at the next OnWake.
func (h *hookLog) nextWake() <-chan struct{} {
	c := make(chan struct{})
	h.mu.Lock()
	h.wakes = append(h.wakes, c)
	h.mu.Unlock()
	return c
}

func (h *hookLog) setWakeErr(err error) {
	h.mu.Lock()
	h.wakeErr = err
	h.mu.Unlock()
}

func (h *hookLog) calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

// runAsync runs the loop on another goroutine and returns Run's result.
func runAsync(e *env, fn func(*moejs.Runtime) error) <-chan error {
	result := make(chan error, 1)
	go func() { result <- e.loop.Run(fn) }()
	return result
}

// await returns what c delivers, failing the test after 10 seconds.
func await[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// TestHooksAlternate checks the calls of OnIdle and OnWake over a run that
// waits for a timer, for the done function of a Hold and for the settle
// function of a promise of NewPromise, both called from other goroutines:
// one pair for each wait, OnIdle first, and none for the immediates, which
// run without a wait.
func TestHooksAlternate(t *testing.T) {
	e, h := newHookEnv(t)
	require.NoError(t, e.rt.SetGlobal("hold", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		cb := moejs.Arg(args, 0)
		idle := h.nextIdle()
		done := e.loop.Hold()
		go func() {
			<-idle
			done(func(rt *moejs.Runtime) {
				h.ran("done")
				_, err := rt.CallFunction(cb, moejs.Undefined())
				assert.NoError(t, err)
			})
		}()
		return moejs.Undefined(), nil
	})))
	require.NoError(t, e.rt.SetGlobal("later", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		idle := h.nextIdle()
		p, settle := e.loop.NewPromise()
		go func() {
			<-idle
			settle(func(*moejs.Runtime) (moejs.Value, error) {
				h.ran("settle")
				return moejs.String("value"), nil
			})
		}()
		return p, nil
	})))
	h.begin()
	require.NoError(t, e.loop.Run(script(t, `
setImmediate(() => mark("immediate"));
setTimeout(() => {
	mark("timer");
	hold(() => {
		mark("hold callback");
		later().then((v) => {
			mark("then " + v);
			setImmediate(() => mark("immediate 2"));
		});
	});
}, 100);
mark("script");
`)))
	assert.Equal(t, []string{
		"script", "immediate",
		"idle", "wake", "timer",
		"idle", "wake", "done", "hold callback",
		"idle", "wake", "settle", "then value", "immediate 2",
	}, h.calls())
}

// TestHooksNoJavaScriptWhileIdle runs intervals, timers, immediates,
// promises settled from other goroutines and tasks posted from another
// one, and checks in each that the loop is not idle (hookLog.ran).
func TestHooksNoJavaScriptWhileIdle(t *testing.T) {
	e, h := newHookEnv(t)
	require.NoError(t, e.rt.SetGlobal("later", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		p, settle := e.loop.NewPromise()
		go func() {
			time.Sleep(time.Millisecond)
			settle(func(*moejs.Runtime) (moejs.Value, error) {
				h.ran("settle")
				return moejs.Undefined(), nil
			})
		}()
		return p, nil
	})))
	run := script(t, `
let n = 0;
const iv = setInterval(() => { mark("interval"); if (++n === 10) clearInterval(iv); }, 1);
for (let i = 0; i < 10; i++) setTimeout(() => {
	mark("timer");
	setImmediate(() => mark("immediate"));
	later().then(() => mark("then"));
}, i);
`)
	h.begin()
	require.NoError(t, e.loop.Run(func(rt *moejs.Runtime) error {
		done := e.loop.Hold()
		go func() {
			for range 20 {
				time.Sleep(500 * time.Microsecond)
				e.loop.RunOnLoop(func(*moejs.Runtime) { h.ran("task") })
			}
			done(func(*moejs.Runtime) { h.ran("done") })
		}()
		return run(rt)
	}))
	calls := h.calls()
	for _, want := range []struct {
		event string
		n     int
	}{{"interval", 10}, {"timer", 10}, {"immediate", 10}, {"settle", 10}, {"then", 10}, {"task", 20}, {"done", 1}} {
		assert.Equal(t, want.n, count(calls, want.event), want.event)
	}
	assert.NotZero(t, count(calls, "idle"), "the loop waited")
}

func count(events []string, event string) int {
	n := 0
	for _, s := range events {
		if s == event {
			n++
		}
	}
	return n
}

// TestHooksNoWait checks that a loop that never has to wait calls neither
// hook: tasks, chained immediates and promises settled before the loop
// looks always leave work, and a timer that is due when the loop first
// looks runs at once.
func TestHooksNoWait(t *testing.T) {
	e, h := newHookEnv(t)
	require.NoError(t, e.rt.SetGlobal("settled", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		p, settle := e.loop.NewPromise()
		settle(nil)
		return p, nil
	})))
	for range 3 {
		e.loop.RunOnLoop(func(*moejs.Runtime) { h.ran("task") })
	}
	h.begin()
	require.NoError(t, e.loop.Run(script(t, `
let n = 0;
setImmediate(function step() { mark("immediate"); if (++n < 5) setImmediate(step); });
(async () => { for (let i = 0; i < 5; i++) await settled(); mark("settled"); })();
`)))
	calls := h.calls()
	assert.NotContains(t, calls, "idle")
	assert.NotContains(t, calls, "wake")
	assert.Equal(t, 3, count(calls, "task"))
	assert.Equal(t, 5, count(calls, "immediate"))
	assert.Equal(t, 1, count(calls, "settled"))

	e, h = newHookEnv(t)
	run := script(t, `setTimeout(() => mark("timer"), 1);`)
	h.begin()
	require.NoError(t, e.loop.Run(func(rt *moejs.Runtime) error {
		err := run(rt)
		time.Sleep(10 * time.Millisecond) // the timer is due when the loop looks
		return err
	}))
	assert.Equal(t, []string{"timer"}, h.calls())
}

// TestHooksEndIdle checks a loop that ends while it waits, by Stop and
// StopNoWait from another goroutine, Terminate, Interrupt, an interrupt of
// Runtime.Interrupt found when a timer wakes the loop, and the release of
// its last hold: OnWake is not called, and Run returns what it returns
// without the hooks.
func TestHooksEndIdle(t *testing.T) {
	interrupted := func(t *testing.T, err error) {
		var ie *moejs.InterruptedError
		require.ErrorAs(t, err, &ie)
		assert.Equal(t, "timeout", ie.Value)
	}
	for _, c := range []struct {
		name  string
		src   string
		end   func(t *testing.T, e *env, done func(func(*moejs.Runtime)))
		check func(t *testing.T, err error)
	}{
		{"Stop", ``, func(t *testing.T, e *env, _ func(func(*moejs.Runtime))) {
			assert.NoError(t, e.loop.Stop())
		}, func(t *testing.T, err error) { assert.NoError(t, err) }},
		{"StopNoWait", ``, func(_ *testing.T, e *env, _ func(func(*moejs.Runtime))) {
			e.loop.StopNoWait()
		}, func(t *testing.T, err error) { assert.NoError(t, err) }},
		{"Terminate", ``, func(_ *testing.T, e *env, _ func(func(*moejs.Runtime))) {
			e.loop.Terminate()
		}, func(t *testing.T, err error) { assert.ErrorIs(t, err, eventloop.ErrTerminated) }},
		{"Interrupt", ``, func(_ *testing.T, e *env, _ func(func(*moejs.Runtime))) {
			e.loop.Interrupt("timeout")
		}, interrupted},
		{"Runtime.Interrupt and a timer", `setTimeout(() => mark("timer"), 100);`, func(_ *testing.T, e *env, _ func(func(*moejs.Runtime))) {
			e.rt.Interrupt("timeout")
		}, interrupted},
		{"last hold released", ``, func(_ *testing.T, _ *env, done func(func(*moejs.Runtime))) {
			done(nil)
		}, func(t *testing.T, err error) { assert.NoError(t, err) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, h := newHookEnv(t)
			done := e.loop.Hold()
			idle := h.nextIdle()
			h.begin()
			result := runAsync(e, script(t, c.src))
			await(t, idle, "OnIdle")
			c.end(t, e, done)
			c.check(t, await(t, result, "Run"))
			assert.Equal(t, []string{"idle"}, h.calls())
			e.rt.ClearInterrupt()
		})
	}
}

// TestHooksSpuriousWake checks that a wake with nothing to run calls no
// hook: the signal of a task the loop ran before it waited, and the
// release of a hold while another is still held.
func TestHooksSpuriousWake(t *testing.T) {
	e, h := newHookEnv(t)
	first, second := e.loop.Hold(), e.loop.Hold()
	idle := h.nextIdle()
	h.begin()
	result := runAsync(e, func(*moejs.Runtime) error {
		// Posted before the loop starts: its signal is still there when
		// the loop first waits, after it ran the task.
		e.loop.RunOnLoop(func(*moejs.Runtime) { h.ran("task") })
		return nil
	})
	await(t, idle, "OnIdle")
	first(nil)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, []string{"task", "idle"}, h.calls())
	second(func(*moejs.Runtime) { h.ran("done") })
	require.NoError(t, await(t, result, "Run"))
	assert.Equal(t, []string{"task", "idle", "wake", "done"}, h.calls())
}

// TestOnWakeError checks that an error of OnWake stops the loop before it
// takes anything off its queues: Run returns the error itself, the
// runtime is not interrupted, and the next Run runs the queued task and
// timer.
func TestOnWakeError(t *testing.T) {
	errSlot := errors.New("no slot")
	e, h := newHookEnv(t)
	h.setWakeErr(errSlot)
	idle := h.nextIdle()
	h.begin()
	result := runAsync(e, script(t, `setTimeout(() => mark("timer"), 20);`))
	await(t, idle, "OnIdle")
	require.True(t, e.loop.RunOnLoop(func(*moejs.Runtime) { h.ran("task") }))
	err := await(t, result, "Run")
	assert.True(t, err == errSlot, "Run returned %v, not OnWake's error", err)
	assert.Equal(t, []string{"idle", "wake"}, h.calls())
	_, err = e.rt.RunScript(mustScript(t, `1`))
	require.NoError(t, err, "no interrupt is pending")

	h.setWakeErr(nil)
	h.begin()
	require.NoError(t, e.loop.Run(nil))
	work := slices.DeleteFunc(h.calls()[2:], func(s string) bool { return s == "idle" || s == "wake" })
	assert.ElementsMatch(t, []string{"task", "timer"}, work)
}

// TestOnWakeBlocks checks an OnWake that blocks: Stop waits for it to
// return, and the stop that came meanwhile ends the run before the task
// that woke the loop runs; Terminate waits for an OnWake that waits on a
// context, until the host cancels it.
func TestOnWakeBlocks(t *testing.T) {
	t.Run("Stop", func(t *testing.T) {
		idle, inWake, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var idles, wakes, returned atomic.Int32
		e := newEnv(t, eventloop.Options{
			OnIdle: func() {
				if idles.Add(1) == 1 {
					close(idle)
				}
			},
			OnWake: func() error {
				if wakes.Add(1) == 1 {
					close(inWake)
				}
				<-release
				returned.Add(1)
				return nil
			},
		})
		done := e.loop.Hold()
		result := runAsync(e, nil)
		await(t, idle, "OnIdle")
		require.True(t, e.loop.RunOnLoop(func(*moejs.Runtime) { e.add("task") }))
		await(t, inWake, "OnWake")
		// The stop is asked for before OnWake returns (StopNoWait does it
		// at once); Stop waits for the loop, so for OnWake.
		e.loop.StopNoWait()
		stopped := make(chan error, 1)
		go func() { stopped <- e.loop.Stop() }()
		select {
		case <-stopped:
			t.Fatal("Stop returned while OnWake blocked")
		case <-time.After(20 * time.Millisecond):
		}
		close(release)
		require.NoError(t, await(t, stopped, "Stop"))
		assert.Equal(t, int32(1), returned.Load(), "Stop returned before OnWake")
		require.NoError(t, await(t, result, "Run"))
		assert.Empty(t, e.log(), "the stop ended the run before the task")
		assert.Equal(t, int32(1), idles.Load(), "the run ended awake")
		done(nil)
		require.NoError(t, e.loop.Run(nil))
		assert.Equal(t, []string{"task"}, e.log())
	})
	t.Run("context and Terminate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		idle, inWake := make(chan struct{}), make(chan struct{})
		slot := make(chan struct{}) // never free
		e := newEnv(t, eventloop.Options{
			OnIdle: func() { close(idle) },
			OnWake: func() error {
				close(inWake)
				select {
				case slot <- struct{}{}:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
		e.loop.Hold()
		result := runAsync(e, nil)
		await(t, idle, "OnIdle")
		require.True(t, e.loop.RunOnLoop(func(*moejs.Runtime) { e.add("task") }))
		await(t, inWake, "OnWake")
		terminated := make(chan struct{})
		go func() {
			e.loop.Terminate()
			close(terminated)
		}()
		for e.loop.RunOnLoop(nil) {
			time.Sleep(time.Millisecond) // until Terminate marked the loop
		}
		select {
		case <-terminated:
			t.Fatal("Terminate returned while OnWake blocked")
		case <-time.After(20 * time.Millisecond):
		}
		cancel()
		await(t, terminated, "Terminate")
		assert.ErrorIs(t, await(t, result, "Run"), eventloop.ErrTerminated)
		assert.Empty(t, e.log())
		e.rt.ClearInterrupt()
	})
}

// TestHooksStart checks the hooks of a loop started with Start: the same
// pairing, and Stop returns the error of OnWake that ended the loop. The
// task that woke it runs in the next Start, before the loop waits again.
func TestHooksStart(t *testing.T) {
	errSlot := errors.New("no slot")
	e, h := newHookEnv(t)
	idle := h.nextIdle()
	h.begin()
	require.NoError(t, e.loop.Start())
	await(t, idle, "OnIdle")
	idle = h.nextIdle()
	require.True(t, e.loop.RunOnLoop(func(*moejs.Runtime) { h.ran("task 1") }))
	await(t, idle, "OnIdle")
	h.setWakeErr(errSlot)
	wake := h.nextWake()
	require.True(t, e.loop.RunOnLoop(func(*moejs.Runtime) { h.ran("task 2") }))
	await(t, wake, "OnWake")
	err := e.loop.Stop()
	assert.True(t, err == errSlot, "Stop returned %v, not OnWake's error", err)
	assert.Equal(t, []string{"idle", "wake", "task 1", "idle", "wake"}, h.calls())

	h.setWakeErr(nil)
	idle = h.nextIdle()
	h.begin()
	require.NoError(t, e.loop.Start())
	await(t, idle, "OnIdle")
	require.NoError(t, e.loop.Stop())
	assert.Equal(t, []string{"idle", "wake", "task 1", "idle", "wake", "task 2", "idle"}, h.calls())
}

// TestHooksOneSet checks a loop with only one of the hooks.
func TestHooksOneSet(t *testing.T) {
	var idles, wakes atomic.Int32
	e := newEnv(t, eventloop.Options{OnIdle: func() { idles.Add(1) }})
	assert.Equal(t, []string{"a"}, e.run(t, `setTimeout(() => log("a"), 20);`))
	assert.Equal(t, int32(1), idles.Load())

	e = newEnv(t, eventloop.Options{OnWake: func() error { wakes.Add(1); return nil }})
	assert.Equal(t, []string{"a"}, e.run(t, `setTimeout(() => log("a"), 20);`))
	assert.Equal(t, int32(1), wakes.Load())
}

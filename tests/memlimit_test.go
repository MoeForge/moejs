package tests

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const memSource = `
export function loop() {
	const keep = [];
	try {
		for (;;) keep.push({n: keep.length, s: "item"});
	} catch (e) {
		mark("catch");
	} finally {
		mark("finally");
	}
}
export function gen() {
	function* items() {
		const keep = [];
		try {
			for (;;) { keep.push([1, 2, 3]); yield keep.length; }
		} finally {
			mark("finally");
		}
	}
	let n = 0;
	for (const x of items()) n += x;
	return n;
}
export async function asyncFn() {
	const keep = [];
	try {
		for (;;) { keep.push({a: 1}); await null; }
	} finally {
		mark("finally");
	}
}
export function job() {
	Promise.resolve().then(() => {
		const keep = [];
		try {
			for (;;) keep.push({a: 1});
		} finally {
			mark("finally");
		}
	});
	return "queued";
}
export function small(n) {
	const out = [];
	for (let i = 0; i < n; i++) out.push({i});
	return out.length;
}
export function repeat(n) {
	try {
		return "x".repeat(n).length;
	} catch (e) {
		return e instanceof RangeError ? "RangeError: " + e.message : "other";
	}
}
export function garbage(n) {
	let sum = 0;
	for (let i = 0; i < n; i++) {
		const o = {i, s: "k" + (i % 10)};
		const a = [o, o];
		sum += a.length + o.s.length;
	}
	let s = "";
	for (let i = 0; i < n; i++) s += "x";
	let p = "";
	for (let i = 0; i < 10000; i++) p = "y" + p;
	return sum + s.length + p.charCodeAt(0);
}
export function stats() {
	const m = new Map();
	for (let i = 0; i < 100; i++) m.set("k" + i, {i});
	return m.size;
}
`

type memMarks struct{ seen []string }

func (m *memMarks) fn(r *moejs.Realm, this moejs.Value, args []moejs.Value) (moejs.Value, error) {
	m.seen = append(m.seen, moejs.Arg(args, 0).String())
	return moejs.Undefined(), nil
}

func newMemRuntime(t *testing.T, limit int64) (*moejs.Runtime, *moejs.Module, *memMarks) {
	t.Helper()
	mod, err := moejs.Compile("mem.js", memSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	marks := &memMarks{}
	require.NoError(t, rt.SetGlobal("mark", moejs.NativeFunc(marks.fn)))
	require.NoError(t, rt.Load(mod))
	require.Zero(t, rt.Stats().RequestAllocatedBytes, "Load starts a new budget")
	return rt, mod, marks
}

// requireMemoryHit checks that err is a memory limit hit of limit.
func requireMemoryHit(t *testing.T, err error, limit int64) *moejs.MemoryLimitError {
	t.Helper()
	var interrupted *moejs.InterruptedError
	require.ErrorAs(t, err, &interrupted)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	var mle *moejs.MemoryLimitError
	require.ErrorAs(t, err, &mle)
	assert.Equal(t, limit, mle.Limit)
	assert.Greater(t, mle.Live+mle.Allocated, limit)
	assert.Contains(t, err.Error(), "memory limit exceeded")
	return mle
}

// TestMemoryLimitHit: a hit stops a plain loop, a generator, an async
// function and a promise job without running a catch or a finally, and the
// runtime runs the next request after ClearInterrupt and ReleaseCallData.
func TestMemoryLimitHit(t *testing.T) {
	const limit = 1 << 20
	for _, hook := range []string{"loop", "gen", "asyncFn", "job"} {
		t.Run(hook, func(t *testing.T) {
			rt, mod, marks := newMemRuntime(t, limit)
			_, err := rt.Call(mustHook(t, mod, hook))
			mle := requireMemoryHit(t, err, limit)
			assert.Empty(t, marks.seen, "no catch or finally runs")
			assert.Contains(t, mle.Stack, "mem.js:")
			st := rt.Stats()
			assert.Equal(t, int64(1), st.MemoryLimitHits)
			assert.Same(t, mle, st.LastMemoryLimitError)
			assert.Equal(t, 0, st.PendingJobs, "the interrupt dropped the jobs")

			// Still interrupted: the next call is refused.
			_, err = rt.Call(mustHook(t, mod, "small"), moejs.Int(10))
			require.ErrorIs(t, err, moejs.ErrMemoryLimit)

			rt.ClearInterrupt()
			rt.ReleaseCallData()
			assert.Zero(t, rt.Stats().RequestAllocatedBytes)
			assert.Nil(t, rt.Stats().LastMemoryLimitError)
			v, err := rt.Call(mustHook(t, mod, "small"), moejs.Int(1000))
			require.NoError(t, err)
			assert.Equal(t, int64(1000), int64(v.AsNumber()))
			assert.Equal(t, int64(1), rt.Stats().MemoryLimitHits)
		})
	}
}

// TestMemoryLimitStack: the error carries the JavaScript stack at the
// allocation that passed the limit.
func TestMemoryLimitStack(t *testing.T) {
	rt, mod, _ := newMemRuntime(t, 1<<20)
	_, err := rt.Call(mustHook(t, mod, "loop"))
	mle := requireMemoryHit(t, err, 1<<20)
	assert.Contains(t, mle.Stack, "    at loop (mem.js:5:", mle.Stack)
}

// TestMemoryLimitClearWithoutReset: clearing the interrupt without a reset
// leaves the budget spent, so the next allocation hits again.
func TestMemoryLimitClearWithoutReset(t *testing.T) {
	rt, mod, _ := newMemRuntime(t, 1<<20)
	_, err := rt.Call(mustHook(t, mod, "loop"))
	requireMemoryHit(t, err, 1<<20)
	rt.ClearInterrupt()
	_, err = rt.Call(mustHook(t, mod, "small"), moejs.Int(10))
	requireMemoryHit(t, err, 1<<20)
	assert.Equal(t, int64(2), rt.Stats().MemoryLimitHits)
	rt.ClearInterrupt()
	rt.ResetAllocation()
	_, err = rt.Call(mustHook(t, mod, "small"), moejs.Int(10))
	require.NoError(t, err)
}

// TestMemoryLimitRangeError: a producer that knows its size before it
// allocates throws its RangeError for a size past the budget, which the
// script catches; the runtime is not interrupted.
func TestMemoryLimitRangeError(t *testing.T) {
	rt, mod, _ := newMemRuntime(t, 1<<20)
	v, err := rt.Call(mustHook(t, mod, "repeat"), moejs.Int(1<<21))
	require.NoError(t, err)
	assert.Equal(t, "RangeError: Invalid string length", v.String())
	v, err = rt.Call(mustHook(t, mod, "repeat"), moejs.Int(1<<10))
	require.NoError(t, err)
	assert.Equal(t, int64(1<<10), int64(v.AsNumber()))
	assert.Zero(t, rt.Stats().MemoryLimitHits)
}

// TestMemoryLimitGarbage: a loop that allocates and drops objects, appends
// to a string and prepends to one stays under a generous limit: the
// account counts what is allocated, and a rope chain only the flattening
// it causes.
func TestMemoryLimitGarbage(t *testing.T) {
	rt, mod, _ := newMemRuntime(t, 64<<20)
	_, err := rt.Call(mustHook(t, mod, "garbage"), moejs.Int(100000))
	require.NoError(t, err)
	st := rt.Stats()
	assert.Zero(t, st.MemoryLimitHits)
	assert.Greater(t, st.RequestAllocatedBytes, int64(5<<20))
	assert.Less(t, st.RequestAllocatedBytes, int64(40<<20))
}

// TestMemoryLimitStats: the counters move as JavaScript allocates; without
// a limit the allocation counters stay zero.
func TestMemoryLimitStats(t *testing.T) {
	rt, mod, _ := newMemRuntime(t, 1<<40)
	before := rt.Stats()
	v, err := rt.Call(mustHook(t, mod, "stats"))
	require.NoError(t, err)
	assert.Equal(t, int64(100), int64(v.AsNumber()))
	st := rt.Stats()
	assert.Greater(t, st.Objects, before.Objects+100)
	assert.GreaterOrEqual(t, st.Strings, before.Strings+100)
	assert.Greater(t, st.AllocatedBytes, before.AllocatedBytes+100*48)
	assert.Equal(t, st.AllocatedBytes-before.AllocatedBytes, st.RequestAllocatedBytes-before.RequestAllocatedBytes)
	assert.Positive(t, st.ICEntries)
	assert.Positive(t, st.RegisterStackBytes)
	assert.Zero(t, st.Interrupts)

	rt.ReleaseCallData()
	st2 := rt.Stats()
	assert.Zero(t, st2.RequestAllocatedBytes)
	assert.Equal(t, st.AllocatedBytes, st2.AllocatedBytes)

	// FromGo's wrappers and ParseJSON's copy are counted.
	arg, err := rt.ParseJSON([]byte(`{"a": [1, 2, {"b": "c"}], "d": "e"}`))
	require.NoError(t, err)
	_, err = rt.FromGo(map[string]any{"list": []any{map[string]any{"x": 1}}})
	require.NoError(t, err)
	assert.Greater(t, rt.Stats().RequestAllocatedBytes, int64(100))
	_ = arg

	rt.Interrupt("stop")
	rt.ClearInterrupt()
	rt.Interrupt("again")
	assert.Equal(t, int64(2), rt.Stats().Interrupts)
	rt.ClearInterrupt()
	assert.Equal(t, int64(2), rt.Stats().Interrupts)

	plain := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, plain.SetGlobal("mark", moejs.NativeFunc((&memMarks{}).fn)))
	require.NoError(t, plain.Load(mod))
	_, err = plain.Call(mustHook(t, mod, "stats"))
	require.NoError(t, err)
	st = plain.Stats()
	assert.Zero(t, st.AllocatedBytes)
	assert.Zero(t, st.Objects)
	assert.Positive(t, st.ICEntries)
}

// TestMemoryLimitStatsFromHostFunction: a host function reads the
// counters while its call runs.
func TestMemoryLimitStatsFromHostFunction(t *testing.T) {
	mod, err := moejs.Compile("usage.js", `export function f() { const a = []; for (let i = 0; i < 1000; i++) a.push({i}); return usage(); }`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 30})
	require.NoError(t, rt.SetGlobal("usage", moejs.NativeFunc(func(r *moejs.Realm, this moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return moejs.Int(r.Stats().RequestAllocatedBytes), nil
	})))
	require.NoError(t, rt.Load(mod))
	v, err := rt.Call(mustHook(t, mod, "f"))
	require.NoError(t, err)
	assert.Greater(t, int64(v.AsNumber()), int64(1000*64))
}

// TestMemoryLimitHostTimeout: a host's timeout and a memory hit each
// surface as themselves, and when both race the error is one of them, a
// hit is counted with its error kept in Stats, and the runtime is
// reusable after ClearInterrupt and ReleaseCallData.
func TestMemoryLimitHostTimeout(t *testing.T) {
	timeout := func(rt *moejs.Runtime, d time.Duration) func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		stop := context.AfterFunc(ctx, func() { rt.Interrupt(context.Cause(ctx)) })
		return func() bool { stopped := stop(); cancel(); return stopped }
	}

	// The timeout first: a limit the loop does not reach in time.
	mod, err := moejs.Compile("spin.js", `export function spin() { let x = 0; for (;;) { x = {x}; } }`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 50})
	require.NoError(t, rt.Load(mod))
	done := timeout(rt, 20*time.Millisecond)
	_, err = rt.Call(mustHook(t, mod, "spin"))
	done()
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorIs(t, err, moejs.ErrMemoryLimit)
	assert.Zero(t, rt.Stats().MemoryLimitHits)
	rt.ClearInterrupt()
	rt.ReleaseCallData()

	// The hit first: a timeout that does not fire.
	rt2, mod2, _ := newMemRuntime(t, 1<<20)
	done = timeout(rt2, time.Minute)
	_, err = rt2.Call(mustHook(t, mod2, "loop"))
	assert.True(t, done(), "the timeout did not fire")
	requireMemoryHit(t, err, 1<<20)

	// Both at once.
	for i := range 20 {
		rt, mod, _ := newMemRuntime(t, 4<<20)
		var fired atomic.Bool
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%5)*time.Millisecond)
		stop := context.AfterFunc(ctx, func() { fired.Store(true); rt.Interrupt(context.Cause(ctx)) })
		_, err := rt.Call(mustHook(t, mod, "loop"))
		stop()
		cancel()
		var interrupted *moejs.InterruptedError
		require.ErrorAs(t, err, &interrupted)
		hit, late := errors.Is(err, moejs.ErrMemoryLimit), errors.Is(err, context.DeadlineExceeded)
		assert.True(t, hit != late, "one cause: %v", err)
		st := rt.Stats()
		if hit {
			assert.Equal(t, int64(1), st.MemoryLimitHits)
		}
		assert.Equal(t, st.MemoryLimitHits != 0, st.LastMemoryLimitError != nil)
		rt.ClearInterrupt()
		rt.ReleaseCallData()
		// A timeout that fires late interrupts nothing that runs; clear it.
		time.Sleep(time.Millisecond)
		rt.ClearInterrupt()
		v, err := rt.Call(mustHook(t, mod, "small"), moejs.Int(100))
		require.NoError(t, err)
		assert.Equal(t, int64(100), int64(v.AsNumber()))
	}
}

// TestMemoryLimitSetup: Load and SetGlobal are setup, which ends with a new
// budget when it succeeds outside any call: a top level that allocates
// most of the limit leaves the first request all of it. A SetGlobal from a
// host function does not reset, and a failed Load keeps the count and the
// hit's error.
func TestMemoryLimitSetup(t *testing.T) {
	// Values are smaller on 32-bit systems: the rows take most of half
	// the limit there.
	const limit = 1 << 20 * strconv.IntSize / 64
	const big = `export const table = []; for (let i = 0; i < 3000; i++) table.push({i, s: "row" + i});
export function grow(n) { const a = []; for (let i = 0; i < n; i++) a.push({i, s: "row" + i}); return a.length; }
export function setInside() { const a = []; for (let i = 0; i < 1000; i++) a.push({i}); return setGlobal(); }`
	mod, err := moejs.Compile("setup.js", big)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	var before, after int64
	require.NoError(t, rt.SetGlobal("setGlobal", moejs.NativeFunc(func(r *moejs.Realm, this moejs.Value, args []moejs.Value) (moejs.Value, error) {
		before = r.Stats().RequestAllocatedBytes
		if err := rt.SetGlobal("fromHost", 1); err != nil {
			return moejs.Undefined(), err
		}
		after = r.Stats().RequestAllocatedBytes
		return moejs.Undefined(), nil
	})))
	assert.Zero(t, rt.Stats().RequestAllocatedBytes, "SetGlobal starts a new budget")
	require.NoError(t, rt.Load(mod))
	st := rt.Stats()
	assert.Zero(t, st.RequestAllocatedBytes, "Load starts a new budget")
	assert.Greater(t, st.AllocatedBytes, int64(limit/2), "the top level used most of the limit")
	// Without the reset after Load this request would pass the limit.
	v, err := rt.Call(mustHook(t, mod, "grow"), moejs.Int(3000))
	require.NoError(t, err)
	assert.Equal(t, int64(3000), int64(v.AsNumber()))
	assert.Greater(t, rt.Stats().RequestAllocatedBytes, int64(limit/2))

	// SetGlobal outside a call resets; from a host function it does not.
	require.NoError(t, rt.SetGlobal("between", "requests"))
	assert.Zero(t, rt.Stats().RequestAllocatedBytes)
	_, err = rt.Call(mustHook(t, mod, "setInside"))
	require.NoError(t, err)
	assert.Positive(t, before)
	assert.GreaterOrEqual(t, after, before, "a SetGlobal inside a call keeps the budget")

	// A failed Load keeps the count and the hit's error.
	failing, err := moejs.Compile("failing.js", `const a = []; for (;;) a.push({n: a.length});`)
	require.NoError(t, err)
	rt2 := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	err = rt2.Load(failing)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	st = rt2.Stats()
	assert.Greater(t, st.LiveBytes+st.RequestAllocatedBytes, int64(limit))
	require.NotNil(t, st.LastMemoryLimitError)
	assert.Equal(t, int64(limit), st.LastMemoryLimitError.Limit)

	// A linked graph: the modules it imports count as setup too.
	dep, err := moejs.Compile("dep.js", `export const rows = []; for (let i = 0; i < 2000; i++) rows.push({i});`)
	require.NoError(t, err)
	entry, err := moejs.Compile("entry.js", `import { rows } from "./dep.js"; export function count() { return rows.length; }`)
	require.NoError(t, err)
	linked, err := moejs.Link(entry, func(_ moejs.Referrer, specifier string) (*moejs.Module, error) { return dep, nil })
	require.NoError(t, err)
	rt3 := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	require.NoError(t, rt3.Load(linked))
	assert.Zero(t, rt3.Stats().RequestAllocatedBytes)
	assert.Greater(t, rt3.Stats().AllocatedBytes, int64(100<<10))

	// A top level still awaiting: ErrModulePending ends the setup too.
	pending, err := moejs.Compile("pending.js", `export const rows = []; for (let i = 0; i < 1000; i++) rows.push({i}); await new Promise(() => {});`)
	require.NoError(t, err)
	rt4 := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	require.ErrorIs(t, rt4.Load(pending), moejs.ErrModulePending)
	assert.Zero(t, rt4.Stats().RequestAllocatedBytes)
}

// TestMemoryLimitLazyModuleBody: JSON.parse in a module body charges the
// strings it makes as decoded, and the hit becomes the load error.
func TestMemoryLimitLazyModuleBody(t *testing.T) {
	dep, err := moejs.Compile("dep.js", `export const y = 1;`)
	require.NoError(t, err)
	mod, err := moejs.Compile("entry.js", `import "dep"; JSON.parse(input);`)
	require.NoError(t, err)
	mod, err = moejs.Link(mod, func(moejs.Referrer, string) (*moejs.Module, error) { return dep, nil })
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 64 << 10})
	require.NoError(t, rt.SetGlobal("input", `"`+strings.Repeat("中", 40000)+`"`))
	requireMemoryHit(t, rt.Load(mod), 64<<10)
	assert.Equal(t, int64(1), rt.Stats().MemoryLimitHits)
}

// TestMemoryLimitOff: SetMemoryLimit(0) removes the limit and its counters.
func TestMemoryLimitOff(t *testing.T) {
	rt, mod, _ := newMemRuntime(t, 1<<20)
	rt.Realm().SetMemoryLimit(0)
	v, err := rt.Call(mustHook(t, mod, "small"), moejs.Int(100000))
	require.NoError(t, err)
	assert.Equal(t, int64(100000), int64(v.AsNumber()))
	assert.Zero(t, rt.Stats().AllocatedBytes)
	assert.Zero(t, rt.Realm().MemoryLimit())
}

// ExampleRuntime_Stats exposes the runtime's counters to JavaScript through
// a host function and reports a request that passed the memory limit.
func ExampleRuntime_Stats() {
	mod, _ := moejs.Compile("plugin.js", `
export function handle() {
	const before = memoryUsage();
	const rows = [];
	for (let i = 0; i < 100; i++) rows.push({i});
	return memoryUsage() > before;
}
export function runaway() { const a = []; for (;;) a.push([a.length]); }`)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 8 << 20})
	_ = rt.SetGlobal("memoryUsage", moejs.NativeFunc(func(r *moejs.Realm, this moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return moejs.Int(r.Stats().RequestAllocatedBytes), nil
	}))
	_ = rt.Load(mod) // setup: Load ends with a new budget

	handle, _ := mod.Hook("handle")
	v, _ := rt.Call(handle)
	fmt.Println(v.String())

	runaway, _ := mod.Hook("runaway")
	_, err := rt.Call(runaway)
	var mle *moejs.MemoryLimitError
	fmt.Println(errors.Is(err, moejs.ErrMemoryLimit), errors.As(err, &mle) && mle.Limit == 8<<20)
	// The request is over: drop the interrupt and start a new budget.
	rt.ClearInterrupt()
	rt.ReleaseCallData()
	v, _ = rt.Call(handle)
	fmt.Println(v.String(), rt.Stats().MemoryLimitHits)
	// Output:
	// true
	// true true
	// true 1
}

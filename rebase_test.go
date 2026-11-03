package moejs_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const keepSource = `
const keep = [];
const base = "x".repeat(65536 - 8);
export function grow(n) {
	for (let i = 0; i < n; i++) keep.push(base + keep.length);
	return keep.length;
}
export function make(n) {
	return "y".repeat(n) + "z";
}
`

// newKeepRuntime loads keepSource, whose module bindings keep what grow
// adds, in a runtime with the memory limit limit.
func newKeepRuntime(t *testing.T, limit int64) (*moejs.Runtime, *moejs.Module) {
	t.Helper()
	mod, err := moejs.Compile("keep.js", keepSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	require.NoError(t, rt.Load(mod))
	return rt, mod
}

// requireRebasedHit checks that err is a memory limit hit of limit after
// a measurement: what was measured and allocated since passed it.
func requireRebasedHit(t *testing.T, err error, limit int64) *moejs.MemoryLimitError {
	t.Helper()
	var interrupted *moejs.InterruptedError
	require.ErrorAs(t, err, &interrupted)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	var mle *moejs.MemoryLimitError
	require.ErrorAs(t, err, &mle)
	assert.Equal(t, limit, mle.Limit)
	assert.Positive(t, mle.Live)
	assert.Greater(t, mle.Live+mle.Allocated, limit)
	return mle
}

// TestRebaseAllocation: between calls, RebaseAllocation measures what the
// module keeps once the allocation since the last measurement reached the
// larger of half of what the limit leaves and half of what it keeps, and
// the limit then stops a module that keeps growing near the limit, however
// small each call.
func TestRebaseAllocation(t *testing.T) {
	const limit = 8 << 20
	rt, mod := newKeepRuntime(t, limit)
	grow := mustHook(t, mod, "grow")
	measured, kept := 0, int64(0)
	for i := 0; ; i++ {
		require.Less(t, i, 1000, "the limit stops the module")
		before := rt.Stats()
		// The pace counts what Load allocated too, before its reset: up to
		// a megabyte more than RequestAllocatedBytes before the first
		// measurement.
		threshold := max((limit-before.LiveBytes)/2, before.LiveBytes/2)
		called := false
		require.NoError(t, rt.RebaseAllocation(func(func(moejs.Value)) { called = true }))
		after := rt.Stats()
		if before.RequestAllocatedBytes >= threshold {
			require.True(t, called, "a measurement when due, at %d", i)
		}
		if called {
			require.GreaterOrEqual(t, before.RequestAllocatedBytes+1<<20, threshold, "a measurement only when due, at %d", i)
			measured++
			assert.Zero(t, after.RequestAllocatedBytes)
			if kept > 0 {
				// The module's bindings, which Load evaluated alone.
				assert.InDelta(t, float64(kept), float64(after.LiveBytes), float64(kept)/2, "what the module keeps")
			}
		} else {
			assert.Equal(t, before, after, "nothing until due")
		}
		_, err := rt.Call(grow, moejs.Int(1))
		if err != nil {
			// The allocation that would pass the limit measured once more.
			mle := requireRebasedHit(t, err, limit)
			assert.GreaterOrEqual(t, mle.Live, after.LiveBytes)
			assert.Greater(t, kept, int64(limit/2), "stopped near the limit")
			assert.Less(t, kept, int64(2*limit))
			break
		}
		kept += 65536
	}
	assert.GreaterOrEqual(t, measured, 2)
	assert.Less(t, measured, 30, "paced")
}

// TestRebaseAllocationHit: a measurement past the limit returns the
// *MemoryLimitError and counts a hit without interrupting the runtime,
// whose next allocation then passes the limit; a reset keeps the
// measurement, so that the next allocation measures again and hits again
// while the module keeps more than the limit.
func TestRebaseAllocationHit(t *testing.T) {
	rt, mod := newKeepRuntime(t, 8<<20)
	_, err := rt.Call(mustHook(t, mod, "grow"), moejs.Int(64))
	require.NoError(t, err)
	rt.Realm().SetMemoryLimit(2 << 20)
	err = rt.RebaseAllocation(nil)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	var mle *moejs.MemoryLimitError
	require.ErrorAs(t, err, &mle)
	assert.Equal(t, int64(2<<20), mle.Limit)
	assert.Zero(t, mle.Allocated)
	assert.InDelta(t, float64(4<<20), float64(mle.Live), float64(2<<20))
	assert.Contains(t, mle.Error(), "memory limit exceeded")
	st := rt.Stats()
	assert.Equal(t, mle.Live, st.LiveBytes)
	assert.Same(t, mle, st.LastMemoryLimitError)
	assert.Equal(t, int64(1), st.MemoryLimitHits)
	assert.Zero(t, st.Interrupts, "no interrupt")

	_, err = rt.Call(mustHook(t, mod, "grow"), moejs.Int(1))
	hit := requireRebasedHit(t, err, 2<<20)
	assert.InDelta(t, float64(mle.Live), float64(hit.Live), float64(64<<10), "the allocation measured again")

	rt.ClearInterrupt()
	rt.ResetAllocation()
	assert.Equal(t, mle.Live, rt.Stats().LiveBytes, "a reset keeps the measurement")
	_, err = rt.Call(mustHook(t, mod, "grow"), moejs.Int(1))
	hit = requireRebasedHit(t, err, 2<<20)
	assert.InDelta(t, float64(mle.Live), float64(hit.Live), float64(1<<20), "measured again")
	assert.Equal(t, int64(3), rt.Stats().MemoryLimitHits)

	rt.ClearInterrupt()
	rt.Realm().SetMemoryLimit(8 << 20)
	_, err = rt.Call(mustHook(t, mod, "make"), moejs.Int(10))
	require.NoError(t, err)
}

// TestRebaseAllocationRoots: what only Go code holds counts only through
// roots, and without a limit RebaseAllocation does nothing.
func TestRebaseAllocationRoots(t *testing.T) {
	const limit = 8 << 20 // 6 MiB allocated: due, not a hit
	for _, root := range []bool{false, true} {
		rt, mod := newKeepRuntime(t, limit)
		v, err := rt.Call(mustHook(t, mod, "make"), moejs.Int(3<<20))
		require.NoError(t, err)
		require.NoError(t, rt.RebaseAllocation(func(add func(moejs.Value)) {
			if root {
				add(v)
			}
		}))
		if root {
			assert.InDelta(t, float64(3<<20), float64(rt.Stats().LiveBytes), float64(3<<19))
		} else {
			assert.Less(t, rt.Stats().LiveBytes, int64(1<<20), "a result the module does not keep")
		}
	}

	rt := moejs.NewRuntime(moejs.Options{})
	called := false
	require.NoError(t, rt.RebaseAllocation(func(func(moejs.Value)) { called = true }))
	assert.False(t, called, "no limit, no measurement")
	assert.Equal(t, moejs.Stats{ICEntries: rt.Stats().ICEntries, RegisterStackBytes: rt.Stats().RegisterStackBytes}, rt.Stats())
	assert.Less(t, rt.Realm().MeasureLive(nil), int64(16<<10), "an empty runtime")
}

// TestRebaseAllocationAllocs: RebaseAllocation allocates nothing when no
// measurement is due.
func TestRebaseAllocationAllocs(t *testing.T) {
	rt, _ := newKeepRuntime(t, 64<<20)
	roots := func(add func(moejs.Value)) { t.Fatal("measured") }
	assert.Zero(t, testing.AllocsPerRun(100, func() {
		if err := rt.RebaseAllocation(roots); err != nil {
			t.Fatal(err)
		}
	}))
}

// churnSource keeps what keep adds, in MiB, in a module binding, and
// makes garbage through one kind of allocation per round of churn.
const churnSource = `
const kept = [];
export function keep(mib) { for (let i = 0; i < mib; i++) kept.push("r".repeat(1048576 - 16) + i); }
const kinds = {
	arrays: () => { const a = []; for (let i = 0; i < 4096; i++) a.push(i, {i}); return a.length; },
	objects: () => { const o = {}; for (let i = 0; i < 300; i++) o["p" + i] = {i, s: "v" + i}; return Object.keys(o).length; },
	maps: () => { const m = new Map(); for (let i = 0; i < 3000; i++) m.set(i, [i]); return m.size; },
	sets: () => { const s = new Set(); for (let i = 0; i < 3000; i++) s.add("s" + i); return s.size; },
	ropes: () => { let s = ""; for (let i = 0; i < 2000; i++) s += "abcdefgh" + i; return s.length + s.charCodeAt(5); },
	json: () => JSON.parse(JSON.stringify({a: Array.from({length: 2000}, (_, i) => ({i, s: "x" + i}))})).a.length,
	jsonReviver: () => JSON.parse(JSON.stringify(Array.from({length: 1500}, (_, i) => [i, "y" + i])), (k, v) => v).length,
	classes: () => { class P { #x; y; constructor(i) { this.#x = [i]; this.y = {i}; } get x() { return this.#x; } } let n = 0; for (let i = 0; i < 3000; i++) n += new P(i).x[0]; return n; },
	generators: () => { function* g() { for (let i = 0; i < 3000; i++) yield {i}; } let n = 0; for (const v of g()) n += v.i; return n; },
	closures: () => { const fs = []; for (let i = 0; i < 3000; i++) fs.push(() => i); return fs.length; },
	arrayFrom: () => Array.from({length: 5000}, (_, i) => [i]).map(x => ({x})).filter(o => o.x[0] % 2).length,
	replace: () => "abc ".repeat(3000).replace(/b/g, (m) => m + "[" + m + "]").length,
	split: () => "q,".repeat(6000).split(",").map(s => s + "!").length,
	proxies: () => { const p = new Proxy({}, {get: (t, k) => [k]}); let n = 0; for (let i = 0; i < 3000; i++) n += p["k" + i].length; return n; },
	typed: () => { let n = 0; for (let i = 0; i < 200; i++) n += new Float64Array(512).length + new ArrayBuffer(1024).byteLength; return n; },
	sort: () => Array.from({length: 3000}, (_, i) => ({v: (i * 7919) % 3001})).sort((a, b) => [a.v][0] - [b.v][0]).length,
	entries: () => Object.fromEntries(Object.entries(Object.fromEntries(Array.from({length: 2000}, (_, i) => ["e" + i, [i]])))).e5[0],
	bigint: () => { let b = 1n; for (let i = 0; i < 1500; i++) b = b * 3n + BigInt(i); return Number(b % 1000n); },
	bind: () => { let n = 0; for (let i = 0; i < 3000; i++) n += (function (a, b) { return a + b; }).bind(null, i)(1); return n; },
	errors: () => { let n = 0; for (let i = 0; i < 800; i++) n += new Error("e" + i).stack.length > 0; return n; },
	tagged: () => { const t = (s, ...v) => s.raw.join("") + v.join(); let n = 0; for (let i = 0; i < 3000; i++) n += t` + "`a${i}b${[i]}`" + `.length; return n; },
	structuredClone: () => typeof structuredClone === "function" ? structuredClone({a: Array.from({length: 2000}, (_, i) => ({i}))}).a.length : 0,
	weak: () => { const w = new WeakMap(), keys = []; for (let i = 0; i < 3000; i++) { const k = {}; keys.push(k); w.set(k, [i]); } return keys.length; },
};
export async function churn(kind, rounds) {
	let n = 0;
	for (let r = 0; r < rounds; r++) {
		n += kinds[kind]();
		if (r % 16 === 0) n += await Promise.resolve(1);
	}
	return n;
}
export const kindNames = Object.keys(kinds).join(",");
`

// keepMeasured has the churn module keep mib MiB, in steps between which
// RebaseAllocation measures, so that a measurement is the account's base.
func keepMeasured(t *testing.T, rt *moejs.Runtime, mod *moejs.Module, mib int64) {
	t.Helper()
	for i := int64(0); i < mib; i += 2 {
		_, err := rt.Call(mustHook(t, mod, "keep"), moejs.Int(2))
		require.NoError(t, err)
		require.NoError(t, rt.RebaseAllocation(nil))
	}
	require.Positive(t, rt.Stats().LiveBytes, "measured")
}

// churnRounds is how many rounds of churn of kind allocate about bytes.
func churnRounds(t *testing.T, rt *moejs.Runtime, mod *moejs.Module, kind string, bytes int64) int64 {
	t.Helper()
	before := rt.Stats().AllocatedBytes
	_, err := rt.Call(mustHook(t, mod, "churn"), moejs.String(kind), moejs.Int(1))
	require.NoError(t, err, kind)
	return bytes / max(rt.Stats().AllocatedBytes-before, 1)
}

// TestRemeasureMidJSPaced: a program that keeps 12 MiB under a 32 MiB
// limit and makes 2.5 times what the limit leaves of garbage per call runs
// on, whatever the allocation that passes the limit: the measurement an
// allocation past the limit makes in the middle of JavaScript is made again
// each time the allocation since the last one reaches what paces
// RebaseAllocation. A program that keeps growing still stops.
func TestRemeasureMidJSPaced(t *testing.T) {
	mod, err := moejs.Compile("churn.js", churnSource)
	require.NoError(t, err)
	rt0 := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt0.Load(mod))
	names, ok := rt0.Export("kindNames")
	require.True(t, ok)
	kinds := strings.Split(names.String(), ",")
	if raceEnabled {
		kinds = []string{"arrays", "closures", "arrayFrom", "json"}
	}
	const limit = 32 << 20
	for _, kind := range kinds {
		for _, mutable := range []bool{false, true} {
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit, MutableIntrinsics: mutable})
			require.NoError(t, rt.Load(mod))
			keepMeasured(t, rt, mod, 12)
			room := limit - rt.Stats().LiveBytes
			rounds := churnRounds(t, rt, mod, kind, 5*room/2)
			if rounds < 3 {
				t.Logf("%s: %d rounds, skipped", kind, rounds)
				continue
			}
			for call := range 4 {
				require.NoError(t, rt.RebaseAllocation(nil), kind)
				before := rt.Stats().AllocatedBytes
				_, err := rt.Call(mustHook(t, mod, "churn"), moejs.String(kind), moejs.Int(rounds))
				require.NoError(t, err, "%s mutable=%v call %d, %d KiB allocated", kind, mutable, call, (rt.Stats().AllocatedBytes-before)>>10)
			}
			assert.Zero(t, rt.Stats().MemoryLimitHits, kind)
		}
	}
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	require.NoError(t, rt.Load(mod))
	for i := 0; ; i++ {
		require.Less(t, i, 64, "stops")
		require.NoError(t, rt.RebaseAllocation(nil))
		if _, err = rt.Call(mustHook(t, mod, "keep"), moejs.Int(2)); err != nil {
			break
		}
	}
	assert.ErrorIs(t, err, moejs.ErrMemoryLimit)
}

// TestRemeasureSeesLoadedModule: every measurement counts what a module
// Load loaded keeps, alone or through its graph, not only RebaseAllocation's:
// Realm().MeasureLive, and the measurement an allocation past the limit
// makes in the middle of JavaScript, so that a script that keeps 24 MiB
// more next to a module keeping 20 MiB under a 32 MiB limit stops within
// the call, past a measurement that counted the module.
func TestRemeasureSeesLoadedModule(t *testing.T) {
	const keep = `const kept = []; export function keep(mib) { for (let i = 0; i < mib; i++) kept.push("m".repeat(1048576 - 16) + i); }`
	for _, src := range []string{keep, keep + ` export const url = import.meta.url;`} {
		mod, err := moejs.Compile("keep.js", src)
		require.NoError(t, err)
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 32 << 20})
		require.NoError(t, rt.Load(mod))
		for range 2 {
			_, err = rt.Call(mustHook(t, mod, "keep"), moejs.Int(10))
			require.NoError(t, err)
			require.NoError(t, rt.RebaseAllocation(nil))
		}
		require.GreaterOrEqual(t, rt.Stats().LiveBytes, int64(20<<20))
		assert.GreaterOrEqual(t, rt.Realm().MeasureLive(nil), int64(20<<20), "Realm().MeasureLive")
		s, err := moejs.CompileScript("grow.js", `globalThis.g = []; for (let i = 0; i < 24 * 8192; i++) g.push({i, s: "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz" + i}); g.length`)
		require.NoError(t, err)
		_, err = rt.RunScript(s)
		require.ErrorIs(t, err, moejs.ErrMemoryLimit, "kept past the limit within one call")
		var mle *moejs.MemoryLimitError
		require.ErrorAs(t, err, &mle)
		assert.GreaterOrEqual(t, mle.Live, int64(20<<20), "the measurement before the hit counted the module")
	}
}

// TestMeasureAfterRootsPanic: a root provider that panics fails the
// measurement that called it, MeasureLive, RebaseAllocation or the call
// that measured in the middle of JavaScript, and leaves nothing behind:
// once it is removed, a call that passes the limit measures again in the
// same interval.
func TestMeasureAfterRootsPanic(t *testing.T) {
	mod, err := moejs.Compile("churn.js", churnSource)
	require.NoError(t, err)
	const limit = 32 << 20
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	require.NoError(t, rt.Load(mod))
	keepMeasured(t, rt, mod, 12)
	rounds := churnRounds(t, rt, mod, "ropes", 13*(limit-rt.Stats().LiveBytes)/10)
	require.GreaterOrEqual(t, rounds, int64(3))
	armed := true
	remove := rt.AddLiveRoots(func(func(moejs.Value)) {
		if armed {
			panic("roots")
		}
	})
	assert.PanicsWithValue(t, "roots", func() { rt.Realm().MeasureLive(nil) })
	for range 2 {
		// The call passes the limit and measures, and the root's panic
		// fails it.
		rt.ResetAllocation()
		_, err = rt.Call(mustHook(t, mod, "churn"), moejs.String("ropes"), moejs.Int(rounds))
		var ie *moejs.InternalError
		require.ErrorAs(t, err, &ie)
		rt.ClearInterrupt()
	}
	armed = false
	remove()
	// Calls of 1.3 times what the limit leaves pass only through the
	// measurement in the middle of JavaScript.
	for i := range 3 {
		rt.ResetAllocation()
		_, err = rt.Call(mustHook(t, mod, "churn"), moejs.String("ropes"), moejs.Int(rounds))
		assert.NoError(t, err, "call %d after the root provider was removed", i)
		rt.ClearInterrupt()
	}
	// A RebaseAllocation that measures (the limit is below what the module
	// keeps) and panics.
	armed = true
	remove = rt.AddLiveRoots(func(func(moejs.Value)) {
		if armed {
			panic("roots")
		}
	})
	rt.Realm().SetMemoryLimit(1 << 20)
	assert.PanicsWithValue(t, "roots", func() { _ = rt.RebaseAllocation(nil) })
	rt.Realm().SetMemoryLimit(limit)
	remove()
	rt.ResetAllocation()
	_, err = rt.Call(mustHook(t, mod, "churn"), moejs.String("ropes"), moejs.Int(rounds))
	assert.NoError(t, err, "a call after the RebaseAllocation that panicked")
}

// garbageSource defines churn(rounds, keep), whose rounds allocate objects,
// arrays, strings, JSON, a Map and a closure, in JavaScript and through
// builtins, and keep them only when keep is true.
const garbageSource = `
function churn(rounds, keep) {
	const kept = [];
	let n = 0;
	for (let r = 0; r < rounds; r++) {
		const o = {r, s: "v" + r, a: [r, r + 1, r + 2]};
		const t = JSON.stringify({o, list: Array.from({length: 64}, (_, i) => i * r)});
		n += JSON.parse(t).list.length + t.length;
		n += ("x".repeat(256) + r).length;
		const m = new Map([[r, o]]);
		const f = () => m.size + o.r;
		n += f();
		if (keep) kept.push(o, t);
	}
	return n + kept.length;
}
`

// garbageRounds is how many rounds of churn allocate about bytes.
func garbageRounds(t *testing.T, bytes int64) int64 {
	t.Helper()
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 40})
	s, err := moejs.CompileScript("garbage.js", garbageSource+"churn(100, false);")
	require.NoError(t, err)
	before := rt.Stats().AllocatedBytes
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	return bytes * 100 / (rt.Stats().AllocatedBytes - before)
}

// TestMemoryLimitMeasuresGarbage: a script that allocates ten times the
// limit and keeps little runs to its end, with or without a measurement
// before (the first charge past the limit measures, and the next ones as
// they are due), and the same script keeping what it allocates stops near
// the limit, after a measurement. Without a limit nothing measures.
func TestMemoryLimitMeasuresGarbage(t *testing.T) {
	const limit = 16 << 20
	rounds := garbageRounds(t, 10*limit)
	for _, keep := range []bool{false, true} {
		for _, mutable := range []bool{false, true} {
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit, MutableIntrinsics: mutable})
			s, err := moejs.CompileScript("garbage.js", garbageSource+fmt.Sprintf("churn(%d, %v);", rounds, keep))
			require.NoError(t, err)
			_, err = rt.RunScript(s)
			st := rt.Stats()
			t.Logf("keep=%v mutable=%v: %v; allocated %d MiB, last measurement %d KiB", keep, mutable, err, st.AllocatedBytes>>20, st.LiveBytes>>10)
			if !keep {
				require.NoError(t, err)
				assert.GreaterOrEqual(t, st.AllocatedBytes, int64(9*limit))
				assert.Positive(t, st.LiveBytes, "measured")
				assert.Less(t, st.LiveBytes, int64(limit/2))
				continue
			}
			var ex *moejs.Exception
			if errors.As(err, &ex) {
				// String.prototype.repeat refused what the limit left with its
				// RangeError, which the script did not catch.
				assert.Contains(t, err.Error(), "RangeError", "keep=%v mutable=%v", keep, mutable)
				continue
			}
			mle := requireRebasedHit(t, err, limit)
			assert.Greater(t, mle.Live, int64(limit/4), "the measurement before the hit counted what the script keeps")
		}
	}

	rt := moejs.NewRuntime(moejs.Options{})
	rt.AddLiveRoots(func(func(moejs.Value)) { t.Fatal("measured without a limit") })
	s, err := moejs.CompileScript("garbage.js", garbageSource+fmt.Sprintf("churn(%d, false);", rounds))
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	assert.Zero(t, rt.Stats().LiveBytes)
}

// TestMemoryLimitBuiltinResults: what a builtin builds in Go before it
// returns it (the result of map or Array.from, the tree of JSON.parse, a
// rest element's array) counts: a builtin that builds more than the limit
// stops within half of what the limit leaves past it, though the
// measurements a charge past the limit makes cannot reach what it built,
// with or without a measurement before.
func TestMemoryLimitBuiltinResults(t *testing.T) {
	const limit = 32 << 20
	var b strings.Builder
	b.WriteString("[")
	for i := range 400000 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"a":1,"b":"xyz"}`)
	}
	b.WriteString("]")
	text := b.String()
	setup, err := moejs.CompileScript("setup.js", `globalThis.ints = []; for (let i = 0; i < 500000; i++) ints.push(i);
function* gen() { for (const i of ints) yield {i, s: "v" + i}; }`)
	require.NoError(t, err)
	for _, c := range []struct{ name, src string }{
		{"map", `ints.map(i => ({i, s: "v" + i})).length`},
		{"Array.from", `Array.from(ints, i => ({i, s: "v" + i})).length`},
		{"JSON.parse", `JSON.parse(text).length`},
		{"rest", `{ const [first, ...rest] = gen(); rest.length; }`},
		{"spread", `[...gen()].length`},
	} {
		for _, measured := range []bool{false, true} {
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 30})
			_, err := rt.RunScript(setup)
			require.NoError(t, err)
			require.NoError(t, rt.SetGlobal("text", text))
			if measured {
				rt.Realm().SetMemoryLimit(1 << 20) // below what it keeps: RebaseAllocation measures
				_ = rt.RebaseAllocation(nil)
				require.Positive(t, rt.Stats().LiveBytes)
			}
			rt.Realm().SetMemoryLimit(limit)
			rt.ResetAllocation()
			s, err := moejs.CompileScript("build.js", c.src)
			require.NoError(t, err)
			before := rt.Stats().AllocatedBytes
			_, err = rt.RunScript(s)
			allocated := rt.Stats().AllocatedBytes - before
			t.Logf("%s measured=%v: %v after %d MiB", c.name, measured, err, allocated>>20)
			require.ErrorIs(t, err, moejs.ErrMemoryLimit, "%s measured=%v", c.name, measured)
			assert.Less(t, allocated, int64(2*limit), "%s measured=%v: stopped within the slack", c.name, measured)
		}
	}
}

// callbackGarbageSource keeps data, and makes garbage in callbacks a
// builtin calls, or in plain JavaScript, through one kind of loop.
const callbackGarbageSource = `
const kept = [];
export function keep(mib) { for (let i = 0; i < mib * 16; i++) kept.push("k".repeat(65536 - 16) + i); }
const N = 200000;
const idx = Array.from({length: N}, (_, i) => i);
function work(i) { const o = {i, s: "abcdefghijklmnop" + i, a: [i, i, i, i]}; return o.s.length; }
const kinds = {
	forLoop: () => { let n = 0; for (let i = 0; i < N; i++) n += work(i); return n; },
	forOfGenerator: () => { function* g() { for (let i = 0; i < N; i++) yield i; } let n = 0; for (const i of g()) n += work(i); return n; },
	generatorNext: () => { function* g() { for (let i = 0; i < N; i++) yield work(i); } const it = g(); let n = 0, r; while (!(r = it.next()).done) n += r.value; return n; },
	forEach: () => { let n = 0; idx.forEach(i => { n += work(i); }); return n; },
	map: () => idx.map(i => work(i) & 1).length,
	filter: () => idx.filter(i => work(i) & 1).length,
	flatMap: () => idx.flatMap(i => [work(i) & 1]).length,
	reduce: () => idx.reduce((n, i) => n + work(i), 0),
	sort: () => idx.slice(0, 30000).sort((a, b) => work(a) * 0 + work(b) * 0 + (b - a)).length,
	replace: () => "ab".repeat(N / 2).replace(/b/g, (m, off) => String(work(off) & 1)).length,
	replaceString: () => "ab".repeat(N / 2).replaceAll("b", (m, off) => String(work(off) & 1)).length,
	arrayFrom: () => Array.from({length: N}, (_, i) => work(i) & 1).length,
	reviver: () => JSON.parse(JSON.stringify(idx.slice(0, 100000)), (k, v) => (work(1), work(2), v)).length,
	callbackHost: () => { let n = 0; hostEach(N, (i) => { n += work(i); }); return n; },
};
export function run(kind, calls) { let n = 0; for (let c = 0; c < calls; c++) n += kinds[kind](); return n; }
export const kindNames = Object.keys(kinds).join(",");
`

// hostEach calls args[1] with each index below args[0].
func hostEach(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	n := int(args[0].AsNumber())
	for i := range n {
		if _, err := r.Call(args[1], moejs.Undefined(), []moejs.Value{moejs.Int(int64(i))}); err != nil {
			return moejs.Undefined(), err
		}
	}
	return moejs.Undefined(), nil
}

// TestMemoryLimitGarbageInBuiltinCallbacks: a call that keeps little and
// makes garbage, about six times what the limit leaves, in callbacks of
// builtins (forEach, map, filter, flatMap, reduce, sort, replace with a
// regular expression or a string, Array.from, a JSON.parse reviver, a
// generator resumed with next, a host function's callbacks) runs as the
// same garbage made in a plain loop does: the builtins keep what they
// build where the measurements reach it.
func TestMemoryLimitGarbageInBuiltinCallbacks(t *testing.T) {
	mod, err := moejs.Compile("garbage.js", callbackGarbageSource)
	require.NoError(t, err)
	rt0 := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt0.SetGlobal("hostEach", moejs.NativeFunc(hostEach)))
	require.NoError(t, rt0.Load(mod))
	names, _ := rt0.Export("kindNames")
	const limit = 32 << 20
	kinds := strings.Split(names.String(), ",")
	if raceEnabled {
		kinds = []string{"forEach", "map", "replace", "callbackHost"}
	}
	for _, kind := range kinds {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		require.NoError(t, rt.SetGlobal("hostEach", moejs.NativeFunc(hostEach)))
		require.NoError(t, rt.Load(mod))
		for range 4 {
			_, err := rt.Call(mustHook(t, mod, "keep"), moejs.Int(1))
			require.NoError(t, err)
			require.NoError(t, rt.RebaseAllocation(nil))
		}
		before := rt.Stats().AllocatedBytes
		require.NoError(t, rt.RebaseAllocation(nil))
		_, err := rt.Call(mustHook(t, mod, "run"), moejs.String(kind), moejs.Int(3))
		st := rt.Stats()
		t.Logf("%-15s %v: %d MiB allocated, live %d MiB", kind, err, (st.AllocatedBytes-before)>>20, st.LiveBytes>>20)
		assert.NoError(t, err, "%s: garbage made in callbacks", kind)
		assert.Greater(t, st.AllocatedBytes-before, int64(2*limit), kind)
	}
}

// buildObjects makes n plain objects from Go and returns them in an array.
func buildObjects(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	n := int(args[0].AsNumber())
	key := r.KeyFromGoString("s")
	vals := make([]moejs.Value, n)
	for i := range vals {
		o := r.NewObject()
		if _, err := o.Set(r, key, moejs.String(fmt.Sprintf("value number %d", i)), engine.ObjectValue(o)); err != nil {
			return moejs.Undefined(), err
		}
		vals[i] = engine.ObjectValue(o)
	}
	return engine.ObjectValue(r.NewArrayFromSlice(vals)), nil
}

// TestMemoryLimitBuildersPastLimit: what a builtin builds and the program
// keeps past the limit stops the call: JSON.parse, Array.from, map and the
// spread of a generator, each about 40-60 MiB under a 32 MiB limit. The
// account passes the limit by at most half of what it left, and the one
// allocation that crosses that (an array's storage as it grows, up to
// 14 MiB for these). A host function, or a host accessor, that builds the
// value from Go returns it, and the next RebaseAllocation finds the program
// past the limit (see the guide).
func TestMemoryLimitBuildersPastLimit(t *testing.T) {
	const limit, crossing = 32 << 20, 14 << 20
	text := "[" + strings.Repeat(`{"a":1,"s":"some string value here"},`, 700000) + `{"a":1}]`
	for _, c := range []struct {
		name, src string
		host      bool
	}{
		{"JSON.parse", `globalThis.g = JSON.parse(text);`, false},
		{"Array.from", `globalThis.g = Array.from({length: 700000}, (_, i) => ({i, s: "x" + i}));`, false},
		{"map", `globalThis.g = Array.from({length: 700000}, (_, i) => i).map(i => ({i, s: "x" + i}));`, false},
		{"spread of a generator", `function* gen() { for (let i = 0; i < 700000; i++) yield {i, s: "x" + i}; } globalThis.g = [...gen()];`, false},
		{"host function", `globalThis.g = build(700000);`, true},
		{"host accessor", `Object.defineProperty(globalThis, "built", {get: () => build(700000)}); globalThis.g = built;`, true},
	} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		if c.name == "JSON.parse" {
			require.NoError(t, rt.SetGlobal("text", text))
		}
		require.NoError(t, rt.SetGlobal("build", moejs.NativeFunc(buildObjects)))
		s, err := moejs.CompileScript("build.js", c.src)
		require.NoError(t, err)
		_, err = rt.RunScript(s)
		if c.host {
			// The value comes back past the limit; what the program keeps
			// then fails the next rebase.
			t.Logf("%s: %v", c.name, err)
			rt.ClearInterrupt()
			var mle *moejs.MemoryLimitError
			if rerr := rt.RebaseAllocation(nil); assert.ErrorAs(t, rerr, &mle, c.name) {
				assert.Greater(t, mle.Live, int64(limit), c.name)
			}
			continue
		}
		var mle *moejs.MemoryLimitError
		if !assert.ErrorAs(t, err, &mle, "%s: kept past the limit", c.name) {
			continue
		}
		total := mle.Live + mle.Allocated
		t.Logf("%s: %v; %d MiB past the limit", c.name, err, (total-limit)>>20)
		assert.LessOrEqual(t, total, int64(limit+limit/2+crossing), "%s: past the limit by more than half of it and one allocation", c.name)
	}
}

// TestMemoryLimitHostParsePastLimit: what the host's own ParseJSONString
// or ParseJSON builds past the limit, outside any call, fails the parse,
// and so does JSON.parse the host calls with no frame below it: the tree
// is in the parser's Go variables, which a measurement made mid-parse
// cannot reach, so none is made there, also after an earlier measurement.
func TestMemoryLimitHostParsePastLimit(t *testing.T) {
	const limit = 32 << 20
	text := "[" + strings.Repeat(`{"a":1,"s":"some string value here"},`, 700000) + `{"a":1}]`
	for _, api := range []string{"ParseJSONString", "ParseJSON", "JSON.parse"} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		s, err := moejs.CompileScript("base.js", `globalThis.k = []; for (let i = 0; i < 100000; i++) k.push({i}); k = null; for (let i = 0; i < 200000; i++) ({i}); JSON.parse`)
		require.NoError(t, err)
		parse, err := rt.RunScript(s)
		require.NoError(t, err)
		require.NoError(t, rt.RebaseAllocation(nil))
		switch api {
		case "ParseJSON":
			_, err = rt.ParseJSON([]byte(text))
		case "ParseJSONString":
			_, err = rt.ParseJSONString(text)
		default:
			_, err = rt.CallFunction(parse, moejs.Undefined(), moejs.String(text))
		}
		t.Logf("%s: %v", api, err)
		assert.ErrorIs(t, err, moejs.ErrMemoryLimit, "%s: a parse that builds ~150 MiB under a %d MiB limit", api, limit>>20)
		rt.ClearInterrupt()
	}
}

// TestMemoryLimitHostChargeBetweenCalls: the host's ChargeMemory outside
// any call measures as RebaseAllocation does: after a script that left 12
// MiB of garbage under a 16 MiB limit, a charge of 8 MiB stands, and one
// of more than the limit fails.
func TestMemoryLimitHostChargeBetweenCalls(t *testing.T) {
	const limit = 16 << 20
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	s, err := moejs.CompileScript("garbage.js", `let n = 0; for (let i = 0; i < 12; i++) n += "g".repeat(1 << 20).length; n`)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	require.Zero(t, rt.Stats().LiveBytes, "measured during the script")
	require.NoError(t, rt.Realm().ChargeMemory(8<<20))
	assert.Positive(t, rt.Stats().LiveBytes, "the charge measured")
	assert.ErrorIs(t, rt.Realm().ChargeMemory(limit+1), moejs.ErrMemoryLimit)
	rt.ClearInterrupt()
}

// TestMemoryLimitBetweenPromiseJobs: a task that drains many promise jobs,
// each of which allocates only inside a host function (no JavaScript
// allocation the measurement could run at), measures between the jobs:
// garbage of ten times the limit runs on, and what the jobs keep stops it.
func TestMemoryLimitBetweenPromiseJobs(t *testing.T) {
	const limit = 16 << 20
	for _, keep := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		require.NoError(t, rt.SetGlobal("chunk", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			if err := r.ChargeMemory(1 << 20); err != nil {
				return moejs.Undefined(), err
			}
			return moejs.String(strings.Repeat("c", 1<<20)), nil
		})))
		src := `globalThis.kept = []; for (let i = 0; i < 160; i++) queueMicrotask(() => chunk());`
		if keep {
			src = `globalThis.kept = []; for (let i = 0; i < 160; i++) queueMicrotask(() => kept.push(chunk()));`
		}
		s, err := moejs.CompileScript("jobs.js", src)
		require.NoError(t, err)
		_, err = rt.RunScript(s)
		st := rt.Stats()
		t.Logf("keep=%v: %v; allocated %d MiB, last measurement %d MiB", keep, err, st.AllocatedBytes>>20, st.LiveBytes>>20)
		if keep {
			assert.ErrorIs(t, err, moejs.ErrMemoryLimit)
		} else {
			assert.NoError(t, err)
			assert.GreaterOrEqual(t, st.AllocatedBytes, int64(9*limit))
		}
	}
}

// TestMemoryLimitJobsMeasureOnlyPastLimit: the drain between promise jobs
// measures only once what was allocated since the last measurement passed
// what the limit leaves, as an allocation past it does, not because one
// would be due: a host that resets the budget per task (ResetAllocation)
// gets no measurement, and no hit, from jobs that keep more than the limit
// over three tasks while each task allocates less than it.
func TestMemoryLimitJobsMeasureOnlyPastLimit(t *testing.T) {
	const limit = 16 << 20
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	s, err := moejs.CompileScript("task.js", `globalThis.kept ??= []; for (let i = 0; i < 6; i++) queueMicrotask(() => kept.push("k".repeat(1 << 20)));`)
	require.NoError(t, err)
	for task := range 3 {
		rt.ResetAllocation()
		_, err := rt.RunScript(s)
		require.NoError(t, err, "task %d", task)
	}
	st := rt.Stats()
	t.Logf("18 MiB kept over three tasks: allocated %d MiB, %d measured", st.AllocatedBytes>>20, st.LiveBytes)
	assert.Zero(t, st.LiveBytes, "a measurement between the jobs")
	assert.Zero(t, st.MemoryLimitHits)
}

// TestMemoryLimitBuiltinLoops: a loop whose allocation all happens inside
// builtin calls measures at its next iteration once a call passed the
// limit: garbage of ten times the limit runs on, and the same loop keeping
// what it makes stops past the limit by at most one call's allocation.
func TestMemoryLimitBuiltinLoops(t *testing.T) {
	const limit = 32 << 20
	for _, keep := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		src := `const keep = []; let n = 0; for (let i = 0; i < 320; i++) n += "x".repeat(1 << 20).length; n`
		if keep {
			src = `const keep = []; for (let i = 0; i < 320; i++) keep.push("x".repeat(1 << 20)); keep.length`
		}
		s, err := moejs.CompileScript("loop.js", src)
		require.NoError(t, err)
		_, err = rt.RunScript(s)
		st := rt.Stats()
		t.Logf("keep=%v: %v; allocated %d MiB, last measurement %d MiB", keep, err, st.AllocatedBytes>>20, st.LiveBytes>>20)
		if !keep {
			assert.NoError(t, err)
			continue
		}
		var mle *moejs.MemoryLimitError
		if assert.ErrorAs(t, err, &mle) {
			assert.LessOrEqual(t, mle.Live+mle.Allocated, int64(limit+2<<20), "stopped within one call of the limit")
		}
	}
}

// TestStartRebaseFollowsLimit: StartRebase's precomputed threshold follows
// the limit and resets: a lower limit than what the program keeps
// measures at once and fails, a higher one waits for the pacing again, and
// resets between small allocations still advance the pacing.
func TestStartRebaseFollowsLimit(t *testing.T) {
	mod, err := moejs.Compile("garbage.js", callbackGarbageSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 64 << 20})
	require.NoError(t, rt.SetGlobal("hostEach", moejs.NativeFunc(hostEach)))
	require.NoError(t, rt.Load(mod))
	r := rt.Realm()
	for range 20 {
		_, err := rt.Call(mustHook(t, mod, "keep"), moejs.Int(1))
		require.NoError(t, err)
		require.NoError(t, rt.RebaseAllocation(nil))
	}
	live := rt.Stats().LiveBytes
	require.Greater(t, live, int64(8<<20))
	assert.False(t, r.StartRebase(), "just measured")
	r.SetMemoryLimit(live / 2)
	assert.True(t, r.StartRebase(), "a limit below what is kept measures at once")
	var mle *moejs.MemoryLimitError
	assert.ErrorAs(t, rt.RebaseAllocation(nil), &mle)
	r.SetMemoryLimit(256 << 20)
	assert.False(t, r.StartRebase(), "a higher limit waits for the pacing")
	measured := false
	s, err := moejs.CompileScript("g.js", `{ const g = []; for (let i = 0; i < 20000; i++) g.push({i}); }`)
	require.NoError(t, err)
	for i := 0; i < 400 && !measured; i++ {
		_, err = rt.RunScript(s)
		require.NoError(t, err)
		rt.ResetAllocation()
		measured = r.StartRebase()
	}
	assert.True(t, measured, "resets do not hold the pacing back")
}

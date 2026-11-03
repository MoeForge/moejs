package moejs_test

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mib = 1 << 20

// measureKept measures a fresh runtime that ran setup, which keeps about
// n MiB through one structure, against one that ran nothing.
func measureKept(t *testing.T, opts moejs.Options, setup string) (kept int64) {
	t.Helper()
	empty := moejs.NewRuntime(opts)
	base := empty.Realm().MeasureLive(nil)
	rt := moejs.NewRuntime(opts)
	s, err := moejs.CompileScript("setup.js", "function big(i) { return ('x'.repeat(1048576 - 8) + (1000000 + i)).slice(0); }\n"+setup)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	return rt.Realm().MeasureLive(nil) - base
}

// TestMeasureLiveKeptByStructure: a structure that keeps 4 MiB (four
// strings of 1 MiB, each reachable only through it) measures at least
// 4 MiB and not much more, in both kinds of realm; one that keeps them
// only weakly does not count them.
func TestMeasureLiveKeptByStructure(t *testing.T) {
	strong := []struct{ name, src string }{
		{"closure", "globalThis.f = (() => { const s = [big(0), big(1), big(2), big(3)]; return () => s; })();"},
		{"nested closures", "globalThis.f = (() => { const a = big(0); return () => { const b = big(1); return () => { const c = big(2), d = big(3); return () => a + b + c + d; }; }; })()()();"},
		{"generator", "function* g() { const s = [big(0), big(1), big(2), big(3)]; yield 1; return s; } globalThis.it = g(); it.next();"},
		{"async awaiting a global promise", "globalThis.p = new Promise(() => {}); (async () => { const s = [big(0), big(1), big(2), big(3)]; await p; return s; })();"},
		{"async generator queue", "async function* g() { await new Promise(() => {}); } globalThis.it = g(); for (let i = 0; i < 4; i++) it.next(big(i));"},
		{"then callback", "globalThis.p = new Promise(() => {}); const s = [big(0), big(1), big(2), big(3)]; p.then(() => s);"},
		{"Promise.all values", "globalThis.p = new Promise(() => {}); Promise.all([p, big(0), big(1), big(2), big(3)]);"},
		{"finally", "globalThis.p = new Promise(() => {}); const s = [big(0), big(1), big(2), big(3)]; p.finally(() => s);"},
		{"Map values", "globalThis.m = new Map([[1, big(0)], [2, big(1)], [3, big(2)], [4, big(3)]]);"},
		{"Map keys", "globalThis.m = new Map([[big(0), 1], [big(1), 2], [big(2), 3], [big(3), 4]]);"},
		{"Set", "globalThis.m = new Set([big(0), big(1), big(2), big(3)]);"},
		{"Map iterator", "globalThis.it = new Map([[1, big(0)], [2, big(1)], [3, big(2)], [4, big(3)]]).values();"},
		{"WeakMap with a live key", "globalThis.k = {}; globalThis.w = new WeakMap([[k, [big(0), big(1), big(2), big(3)]]]);"},
		{"WeakMap, key reached first", "globalThis.a = {k: {}}; globalThis.z = new WeakMap(); z.set(a.k, [big(0), big(1), big(2), big(3)]);"},
		{"ArrayBuffer", "globalThis.b = new ArrayBuffer(4 * 1048576);"},
		{"typed array views", "const b = new ArrayBuffer(4 * 1048576); globalThis.v = [new Uint8Array(b), new DataView(b), new Uint32Array(b, 8)];"},
		{"rope", "let s = big(0); for (let i = 1; i < 4; i++) s = s + big(i); globalThis.s = s;"},
		{"bound function", "globalThis.f = function () {}.bind(null, big(0), big(1), big(2), big(3));"},
		{"proxy", "globalThis.p = new Proxy({a: big(0), b: big(1)}, {c: big(2), d: big(3)});"},
		{"Error message", "globalThis.e = [new Error(big(0)), new TypeError(big(1)), new RangeError(big(2)), new AggregateError([], big(3))];"},
		{"private fields", "class C { #a = big(0); #b = big(1); static #c = big(2); #d = big(3); } globalThis.c = new C();"},
		{"symbol description", "globalThis.s = [Symbol(big(0)), Symbol(big(1)), Symbol(big(2)), Symbol(big(3))];"},
		{"arguments", "function f(a, b, c, d) { return () => arguments; } globalThis.g = f(big(0), big(1), big(2), big(3));"},
		{"array iterator", "globalThis.it = [big(0), big(1), big(2), big(3)].values();"},
		{"global lexical", "let kept = [big(0), big(1), big(2), big(3)];"},
		{"global var", "var kept = [big(0), big(1), big(2), big(3)];"},
		{"dictionary object", "const o = {}; for (let i = 0; i < 100; i++) o['k' + i] = i; for (let i = 0; i < 90; i++) delete o['k' + i]; o.a = big(0); o.b = big(1); o.c = big(2); o.d = big(3); globalThis.o = o;"},
		{"sparse array", "const a = []; a[1e6] = big(0); a[2e6] = big(1); a[3e6] = big(2); a[4e6] = big(3); globalThis.a = a;"},
		{"accessor closure", "const s = [big(0), big(1), big(2), big(3)]; Object.defineProperty(globalThis, 'g', {get() { return s; }});"},
		{"property keys of a realm's own shapes", "globalThis.o = {}; for (let i = 0; i < 40; i++) o['k' + i] = 1; for (let i = 0; i < 4; i++) o[big(i)] = 1;"},
		{"class static block", "class C { static { C.s = [big(0), big(1), big(2), big(3)]; } } globalThis.C = C;"},
		{"regexp source", "globalThis.r = [0, 1, 2, 3].map(i => new RegExp(big(i).slice(0, 1048576 - 16) + i));"},
		{"string object", "globalThis.s = [new String(big(0)), new String(big(1)), new String(big(2)), new String(big(3))];"},
		{"queued job", "const s = [big(0), big(1), big(2), big(3)]; Promise.resolve().then(() => s);"},
	}
	for _, mutable := range []bool{false, true} {
		opts := moejs.Options{}
		if mutable {
			opts.MutableIntrinsics = true
		}
		for _, c := range strong {
			kept := measureKept(t, opts, c.src)
			assert.GreaterOrEqual(t, kept, int64(4*mib), "mutable=%v %s: measured %d", mutable, c.name, kept)
			assert.Less(t, kept, int64(5*mib), "mutable=%v %s: measured %d", mutable, c.name, kept)
		}
	}
	kept := measureKept(t, moejs.Options{MutableIntrinsics: true}, "Array.prototype.kept = [big(0), big(1), big(2), big(3)];")
	assert.GreaterOrEqual(t, kept, int64(4*mib), "on an intrinsic of a mutable realm")
	weak := []struct{ name, src string }{
		{"WeakMap with a dead key", "globalThis.w = new WeakMap([[{}, [big(0), big(1), big(2), big(3)]]]);"},
		{"WeakRef", "globalThis.r = new WeakRef([big(0), big(1), big(2), big(3)]);"},
		{"WeakSet", "globalThis.w = new WeakSet([[big(0), big(1), big(2), big(3)]]);"},
		{"garbage", "for (let i = 0; i < 4; i++) big(i);"},
	}
	for _, c := range weak {
		kept := measureKept(t, moejs.Options{}, c.src)
		assert.Less(t, kept, int64(mib/2), "%s: measured %d", c.name, kept)
	}
}

// TestMeasureLiveSkipsSharedConstants: a string constant of the compiled
// code, which every runtime that runs it shares and no charge point
// charges, does not count as what a runtime that keeps it retains, as the
// shared shapes do not.
func TestMeasureLiveSkipsSharedConstants(t *testing.T) {
	lit := strings.Repeat("y", 4*mib)
	s, err := moejs.CompileScript("lit.js", "globalThis.kept = '"+lit+"';")
	require.NoError(t, err)
	base := moejs.NewRuntime(moejs.Options{}).Realm().MeasureLive(nil)
	for i := range 3 {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 64 * mib})
		_, err := rt.RunScript(s)
		require.NoError(t, err)
		kept := rt.Realm().MeasureLive(nil) - base
		charged := rt.Stats().AllocatedBytes
		t.Logf("runtime %d: measured %d bytes retained, %d charged in all", i, kept, charged)
		assert.Less(t, kept, int64(mib), "runtime %d: the shared 4 MiB literal counted (%d charged)", i, charged)
	}
}

// TestMeasureLiveCountsPropertyNames: what an object keeps through its
// property names counts. With the shared intrinsics (the default), a name
// that only data supplies and that is longer than 64 bytes stays in the
// realm's own shapes, which the walk counts, rather than in the
// process-wide shape tree, which it skips.
func TestMeasureLiveCountsPropertyNames(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"computed keys", "globalThis.o = {}; for (let i = 0; i < 4; i++) o[big(i)] = 1;"},
		{"JSON.parse keys", "globalThis.o = JSON.parse('{' + [0, 1, 2, 3].map(i => JSON.stringify(big(i)) + ':' + i).join(',') + '}');"},
		{"one key per object", "globalThis.a = [0, 1, 2, 3].map(i => ({[big(i)]: i}));"},
	} {
		for _, mutable := range []bool{false, true} {
			kept := measureKept(t, moejs.Options{MutableIntrinsics: mutable}, c.src)
			assert.GreaterOrEqual(t, kept, int64(4*mib), "mutable=%v %s: measured %d", mutable, c.name, kept)
		}
	}
}

// TestMeasureLiveDeepStructures: deep and cyclic structures measure without a
// Go stack overflow, in time linear in their size; a measurement reuses the
// storage an earlier one left, so that a second one allocates next to
// nothing (but under the race detector, whose sync.Pool drops a quarter of
// what is put back).
func TestMeasureLiveDeepStructures(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"linked list", "let l = null; for (let i = 0; i < 1000000; i++) l = {next: l}; globalThis.l = l;"},
		{"nested arrays", "let a = []; for (let i = 0; i < 1000000; i++) a = [a]; globalThis.a = a;"},
		{"rope", "let s = 'a'; for (let i = 0; i < 1000000; i++) s = s + 'b'; globalThis.s = s;"},
		{"left rope", "let s = 'a'; for (let i = 0; i < 1000000; i++) s = 'b' + s; globalThis.s = s;"},
		{"proto chain", "let o = {}; for (let i = 0; i < 300000; i++) o = Object.create(o); globalThis.o = o;"},
		{"bound chain", "let f = function () {}; for (let i = 0; i < 300000; i++) f = f.bind(null); globalThis.f = f;"},
		{"proxy chain", "let p = {}; for (let i = 0; i < 300000; i++) p = new Proxy(p, {}); globalThis.p = p;"},
		{"promise chain", "let p = new Promise(() => {}); globalThis.root = p; for (let i = 0; i < 300000; i++) p = p.then(() => 1);"},
		{"closure chain", "let f = () => 0; for (let i = 0; i < 300000; i++) { const g = f; f = () => g; } globalThis.f = f;"},
		{"map chain", "let m = new Map(); for (let i = 0; i < 300000; i++) m = new Map([[1, m]]); globalThis.m = m;"},
		{"weakmap chain", "const w = new WeakMap(); let k = {}; globalThis.k0 = k; for (let i = 0; i < 300000; i++) { const n = {}; w.set(k, n); k = n; } globalThis.w = w;"},
		{"cycles", "const a = []; for (let i = 0; i < 100000; i++) { const o = {i}; o.self = o; o.all = a; a.push(o); } globalThis.a = a;"},
		{"shape chain", "const o = {}; for (let i = 0; i < 100000; i++) o['p' + i] = i; globalThis.o = o;"},
	} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 40})
		s, err := moejs.CompileScript("deep.js", c.src)
		require.NoError(t, err, c.name)
		_, err = rt.RunScript(s)
		require.NoError(t, err, c.name)
		// The storage is the process's: another goroutine's measurement may
		// take it, and two collections drop it. The collector waits, and
		// the later walk is the best of three.
		gc := debug.SetGCPercent(-1)
		var alloc [2]uint64
		var live [2]int64
		var took time.Duration
		for i := range 4 {
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			start := time.Now()
			l := rt.Realm().MeasureLive(nil)
			took = time.Since(start)
			runtime.ReadMemStats(&m1)
			a := m1.TotalAlloc - m0.TotalAlloc
			switch {
			case i == 0:
				live[0], alloc[0] = l, a
			case i == 1 || a < alloc[1]:
				live[1], alloc[1] = l, a
			}
		}
		debug.SetGCPercent(gc)
		t.Logf("%s: %d bytes in %v; the walks allocated %d and %d KiB of Go memory", c.name, live[1], took, alloc[0]>>10, alloc[1]>>10)
		assert.Positive(t, live[1], c.name)
		assert.Equal(t, live[0], live[1], c.name)
		assert.Less(t, took, 5*time.Second, c.name)
		if c.name != "weakmap chain" && !raceEnabled { // keys reached before their map wait in a map of their own
			// TotalAlloc counts what the process's other goroutines allocate meanwhile.
			assert.Less(t, alloc[1], uint64(mib), "%s: the second walk allocated", c.name)
		}
	}
}

// TestMeasureLiveSharedIntrinsics: runtimes of the shared template
// do not count the intrinsics, also after a program read and used many.
func TestMeasureLiveSharedIntrinsics(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	s, err := moejs.CompileScript("use.js", `
globalThis.refs = [Object, Array.prototype.map, Promise, Map, Set, WeakMap, Proxy, Reflect, JSON, Math, globalThis.Intl ?? 0, Symbol.iterator,
	Object.getPrototypeOf(function* () {}), Object.getPrototypeOf(async function () {}), RegExp.prototype.exec, Date, ArrayBuffer, Uint8Array,
	String.prototype.split, Error.prototype, globalThis];`)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	live := rt.Realm().MeasureLive(nil)
	t.Logf("shared: %d bytes", live)
	assert.Less(t, live, int64(64<<10))
}

// TestRebaseAllocationAcrossSetGlobal: a host that rebases between
// requests, as RebaseAllocation's documentation says, and sets a global
// per request still bounds what the program keeps: SetGlobal resets the
// budget but keeps the measurement, and the resets do not delay the next
// one.
func TestRebaseAllocationAcrossSetGlobal(t *testing.T) {
	const limit = 16 << 20
	mod, err := moejs.Compile("keep.js", `const kept = []; export function handle() { kept.push("x".repeat(1048576 - 8) + kept.length + String(globalThis.req)); return kept.length; }`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	require.NoError(t, rt.Load(mod))
	h := mustHook(t, mod, "handle")
	requests := 0
	for ; requests < 100; requests++ {
		if err = rt.RebaseAllocation(nil); err != nil {
			break
		}
		require.NoError(t, rt.SetGlobal("req", requests))
		if _, err = rt.Call(h); err != nil {
			break
		}
	}
	t.Logf("%d requests of 1 MiB kept each under a %d MiB limit: %v", requests, limit>>20, err)
	assert.ErrorIs(t, err, moejs.ErrMemoryLimit)
	assert.Less(t, requests, 40)
}

// TestMeasureLiveCountsSharedBytes: what a program keeps through a short
// string that shares the bytes of a long one (a slice, a match, a split
// piece, a value JSON.parse read out of a long text) counts as what it
// keeps alive, within a small factor: the Go heap that remains after the
// long strings are dropped. JSON values count their text when the realm
// has a memory limit.
func TestMeasureLiveCountsSharedBytes(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"slice", "globalThis.kept = []; for (let i = 0; i < 64; i++) kept.push(big(i).slice(0, 16));"},
		{"substring", "globalThis.kept = []; for (let i = 0; i < 64; i++) kept.push(big(i).substring(8, 24));"},
		{"slice of a slice", "globalThis.kept = []; for (let i = 0; i < 64; i++) kept.push(big(i).slice(1).slice(0, 16));"},
		{"UTF-16 slice", "globalThis.kept = []; for (let i = 0; i < 64; i++) kept.push(('\\u00e9'.repeat(524280) + i).slice(0, 16));"},
		{"JSON.parse value", "globalThis.kept = []; for (let i = 0; i < 64; i++) kept.push(JSON.parse('[\"' + 'k'.repeat(16) + i + '\", \"' + big(i) + '\"]')[0]);"},
		{"split", "globalThis.kept = []; for (let i = 0; i < 64; i++) kept.push(('k'.repeat(16) + i + ',' + big(i)).split(',')[0]);"},
		{"match", "globalThis.kept = []; for (let i = 0; i < 64; i++) kept.push(/k+\\d+/.exec('k'.repeat(16) + i + big(i))[0]);"},
	} {
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 40})
		base := rt.Realm().MeasureLive(nil)
		s, err := moejs.CompileScript("setup.js", "function big(i) { return ('x'.repeat(1048576 - 8) + (1000000 + i)).slice(0); }\n"+c.src)
		require.NoError(t, err)
		_, err = rt.RunScript(s)
		require.NoError(t, err)
		measured := rt.Realm().MeasureLive(nil) - base
		runtime.GC()
		runtime.ReadMemStats(&m1)
		retained := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
		t.Logf("%s: measured %d KiB, Go heap retained %d KiB", c.name, measured>>10, retained>>10)
		assert.Greater(t, measured, retained/2, "%s: measured %d KiB of %d KiB retained", c.name, measured>>10, retained>>10)
		assert.Less(t, measured, 2*retained+mib, "%s: measured %d KiB of %d KiB retained", c.name, measured>>10, retained>>10)
		runtime.KeepAlive(rt)
	}
}

// spreadBall is Go data allocated between a realm's objects (spread), in
// the size class of a small JavaScript object.
type spreadBall struct {
	p   *spreadBall
	pad [80]byte
}

// spread has each runtime of rts keep objs small objects, made in turn
// with balls of Go data each, so that the realms' nodes are spread thin
// over the heap, as in a pool of runtimes serving requests. It returns the
// Go data.
func spread(t testing.TB, rts []*moejs.Runtime, objs, balls int) [][]*spreadBall {
	mod, err := moejs.Compile("push.js", `globalThis.kept = []; export function push(n) { for (let i = 0; i < n; i++) kept.push({i}); return kept.length; }`)
	require.NoError(t, err)
	for _, rt := range rts {
		require.NoError(t, rt.Load(mod))
	}
	var data [][]*spreadBall
	for range objs {
		for _, rt := range rts {
			_, err := rt.Call(mustHook(t, mod, "push"), moejs.Int(1))
			require.NoError(t, err)
			b := make([]*spreadBall, 0, balls)
			for range balls {
				b = append(b, &spreadBall{})
			}
			data = append(data, b)
		}
	}
	return data
}

// TestMeasureLiveKeepsNoVisitedSet: a realm keeps none of a measurement's
// storage once it ended, and a walk over nodes spread thin over the heap
// allocates little: 16 realms of a pool each keeping 2,000 small objects,
// made in turn with other data, each measured once.
func TestMeasureLiveKeepsNoVisitedSet(t *testing.T) {
	rts := make([]*moejs.Runtime, 16)
	for i := range rts {
		rts[i] = moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 30})
	}
	data := spread(t, rts, 2000, 256)
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	var live int64
	for _, rt := range rts {
		live += rt.Realm().MeasureLive(nil)
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	kept, walked := int64(m1.HeapAlloc)-int64(m0.HeapAlloc), int64(m1.TotalAlloc-m0.TotalAlloc)
	t.Logf("%d realms measuring %d KiB in all keep %d KiB (%d KiB allocated by the walks) among %d MiB of other data",
		len(rts), live>>10, kept>>10, walked>>10, int64(len(data))*256*96>>20)
	assert.Less(t, kept, live/4, "what the realms keep after their measurements")
	if !raceEnabled { // its sync.Pool drops a quarter of what is put back
		assert.Less(t, walked, live/2, "what the walks allocated")
	}
	runtime.KeepAlive(data)
	runtime.KeepAlive(rts)
}

// BenchmarkMeasureLive: a measurement of a realm with a memory limit that
// keeps a linked list of a million objects (dense in the heap), 100,000
// small objects in an array, or 4,096 objects spread thin among other data.
func BenchmarkMeasureLive(b *testing.B) {
	for _, c := range []struct{ name, src string }{
		{"list", "let l = null; for (let i = 0; i < 1000000; i++) l = {next: l}; globalThis.l = l;"},
		{"array", "globalThis.a = []; for (let i = 0; i < 100000; i++) a.push({a: i, b: 'v' + i});"},
		{"spread", ""},
	} {
		b.Run(c.name, func(b *testing.B) {
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 40})
			var data [][]*spreadBall
			if c.src == "" {
				data = spread(b, []*moejs.Runtime{rt}, 4096, 256)
			} else {
				s, err := moejs.CompileScript("setup.js", c.src)
				require.NoError(b, err)
				_, err = rt.RunScript(s)
				require.NoError(b, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				rt.Realm().MeasureLive(nil)
			}
			runtime.KeepAlive(data)
		})
	}
}

// liveState is the Go state of a host object that keeps a JavaScript value
// and Go bytes, and reports them (moejs.LiveReporter).
type liveState struct {
	v     moejs.Value
	bytes int
	calls int
}

func (s *liveState) ReportLive(add func(moejs.Value), addBytes func(n int)) {
	s.calls++
	add(s.v)
	addBytes(s.bytes)
}

// TestLiveReporter: what the Go state of a host object, or the payload of a
// data function, reports counts while JavaScript reaches the object, once
// however many paths lead to it, and stops counting at the next
// measurement once JavaScript drops it, with no collection: Go still holds
// the state. In both kinds of realm.
func TestLiveReporter(t *testing.T) {
	for _, mutable := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable})
		r := rt.Realm()
		run := func(src string) moejs.Value {
			t.Helper()
			s, err := moejs.CompileScript("live.js", src)
			require.NoError(t, err)
			v, err := rt.RunScript(s)
			require.NoError(t, err)
			return v
		}
		measure := func(s *liveState) int64 {
			s.calls = 0
			return r.MeasureLive(nil)
		}
		state := &liveState{v: run(`"s".repeat(1 << 20) + "!"`), bytes: 3 << 20}
		o := r.NewObject()
		o.SetInternal(state)
		fstate := &liveState{v: run(`"f".repeat(1 << 19) + "!"`), bytes: 1 << 20}
		f := r.NewNativeDataFunction(engine.FromGoString("f"), 0, func(*engine.Realm, *engine.FunctionData, moejs.Value, []moejs.Value) (moejs.Value, error) {
			return moejs.Undefined(), nil
		}, fstate)
		base := measure(state)
		assert.Zero(t, state.calls, "mutable=%v: not reached", mutable)

		require.NoError(t, rt.SetGlobal("h", engine.ObjectValue(o)))
		require.NoError(t, rt.SetGlobal("f", engine.ObjectValue(f)))
		one := measure(state) - base
		assert.Equal(t, 1, state.calls, "mutable=%v", mutable)
		assert.InDelta(t, float64(1<<20+3<<20+1<<19+1<<20), float64(one), 64<<10, "mutable=%v: the values and bytes reported", mutable)

		run(`globalThis.h2 = h; globalThis.both = [h, h, f, f];`)
		two := measure(state) - base
		assert.Equal(t, 1, state.calls, "mutable=%v: one report per measurement", mutable)
		assert.InDelta(t, float64(one), float64(two), 4<<10, "mutable=%v: counted once", mutable)

		run(`delete globalThis.h; delete globalThis.h2; delete globalThis.f; delete globalThis.both;`)
		dropped := measure(state) - base
		assert.Zero(t, state.calls, "mutable=%v: dropped", mutable)
		assert.Less(t, dropped, int64(64<<10), "mutable=%v: no longer counted", mutable)
		runtime.KeepAlive(o)
		runtime.KeepAlive(f)
	}
}

// TestLiveReporterLimit: under a memory limit, a script that keeps host
// objects whose Go state reports 1 MiB each stops near the limit though
// it allocates little itself, and one that drops them runs on.
func TestLiveReporterLimit(t *testing.T) {
	const limit = 16 << 20
	for _, keep := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		require.NoError(t, rt.SetGlobal("mk", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			o := r.NewObject()
			o.SetInternal(&liveState{v: moejs.Undefined(), bytes: 1 << 20})
			return engine.ObjectValue(o), nil
		})))
		s, err := moejs.CompileScript("mk.js", fmt.Sprintf(`const kept = [];
for (let i = 0; i < 200000; i++) { const o = mk(); const pad = {i, s: "p" + i, a: [i, i]}; if (%v) kept.push(o); }
kept.length`, keep))
		require.NoError(t, err)
		_, err = rt.RunScript(s)
		st := rt.Stats()
		t.Logf("keep=%v: %v; allocated %d MiB, last measurement %d MiB", keep, err, st.AllocatedBytes>>20, st.LiveBytes>>20)
		if !keep {
			require.NoError(t, err)
			assert.Greater(t, st.AllocatedBytes, int64(2*limit))
			continue
		}
		requireRebasedHit(t, err, limit)
	}
}

// reportState is a host payload that reports a value and bytes, and
// misbehaves on request.
type reportState struct {
	v        moejs.Value
	bytes    int
	calls    int
	panics   bool
	measure  *moejs.Realm // MeasureLive from within ReportLive
	allocate *moejs.Realm // allocates from within ReportLive
}

func (s *reportState) ReportLive(add func(moejs.Value), addBytes func(int)) {
	s.calls++
	if s.panics {
		panic("report boom")
	}
	if s.measure != nil {
		s.measure.MeasureLive(nil)
	}
	if s.allocate != nil {
		s.allocate.NewObject()
	}
	add(s.v)
	addBytes(s.bytes)
}

// TestLiveReporterOncePerObject: ReportLive runs once per object reached
// and per measurement; the values it adds that the walk reached or is
// visiting count once; a payload two objects share counts for each; a
// panic fails that measurement and leaves the realm able to measure; an
// allocation inside it does not break the realm.
func TestLiveReporterOncePerObject(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 30})
	r := rt.Realm()
	base := r.MeasureLive(nil)
	mk := func(st *reportState) moejs.Value {
		o := r.NewObject()
		o.SetInternal(st)
		return engine.ObjectValue(o)
	}
	st := &reportState{bytes: 1 << 20}
	obj := mk(st)
	holder := r.NewObject()
	_, _ = holder.Set(r, r.KeyFromGoString("a"), obj, engine.ObjectValue(holder))
	_, _ = holder.Set(r, r.KeyFromGoString("b"), obj, engine.ObjectValue(holder))
	st.v = engine.ObjectValue(holder)
	require.NoError(t, rt.SetGlobal("h", engine.ObjectValue(holder)))
	got := r.MeasureLive(nil) - base
	assert.Equal(t, 1, st.calls, "reached twice, adding its holder")
	assert.GreaterOrEqual(t, got, int64(1<<20))
	assert.Less(t, got, int64(1<<20+64<<10), "the bytes once")

	shared := &reportState{bytes: 1 << 20}
	require.NoError(t, rt.SetGlobal("s1", mk(shared)))
	require.NoError(t, rt.SetGlobal("s2", mk(shared)))
	shared.calls = 0
	before := r.MeasureLive(nil)
	assert.Equal(t, 2, shared.calls, "a payload two objects share")

	bad := &reportState{panics: true}
	require.NoError(t, rt.SetGlobal("bad", mk(bad)))
	assert.PanicsWithValue(t, "report boom", func() { r.MeasureLive(nil) })
	bad.panics = false
	after := r.MeasureLive(nil)
	assert.GreaterOrEqual(t, after, before, "measures again after the panic")
	assert.Equal(t, after, r.MeasureLive(nil))

	alloc := &reportState{allocate: r}
	require.NoError(t, rt.SetGlobal("alloc", mk(alloc)))
	assert.Equal(t, r.MeasureLive(nil), r.MeasureLive(nil))
	require.NoError(t, rt.RebaseAllocation(nil))
	s, err := moejs.CompileScript("after.js", `globalThis.after = Array.from({length: 1000}, (_, i) => ({i})); after.length`)
	require.NoError(t, err)
	v, err := rt.RunScript(s)
	require.NoError(t, err)
	assert.Equal(t, "1000", v.String())
}

// TestMeasureLiveNestedRefused: a root or a ReportLive that measures the
// realm again gets 0 at once, with or without a memory limit, rather than
// recursing into a Go stack overflow, which ends the process: run in a
// child process.
func TestMeasureLiveNestedRefused(t *testing.T) {
	if os.Getenv("MOEJS_NESTED_MEASURE") == "1" {
		for _, limit := range []int64{0, 1 << 30} {
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
			r := rt.Realm()
			o := r.NewObject()
			o.SetInternal(&reportState{measure: r})
			require.NoError(t, rt.SetGlobal("nested", engine.ObjectValue(o)))
			assert.Positive(t, r.MeasureLive(nil))
			assert.Positive(t, r.MeasureLive(func(func(moejs.Value)) { r.MeasureLive(nil) }))
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMeasureLiveNestedRefused$", "-test.count=1")
	cmd.Env = append(os.Environ(), "MOEJS_NESTED_MEASURE=1")
	out, err := cmd.CombinedOutput()
	first := string(out)
	if i := strings.Index(first, "\n\n"); i > 0 {
		first = first[:i]
	}
	assert.NoError(t, err, "a nested measurement crashed the process:\n%s", first)
}

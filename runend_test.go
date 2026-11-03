package moejs_test

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/stretchr/testify/require"
)

// What finished runs made and dropped is garbage for Go's collector once
// the host released the runtime's call data (ReleaseCallData): the engine
// keeps no reference to it in the registers, frame records, job queue or
// caches the runs used, so a pooled runtime does not carry one request's
// large values into the next. probe() returns an ArrayBuffer over a Go
// buffer whose collection the test sees (runtime.AddCleanup).

const probeSize = 1 << 20

// probes counts the buffers probe() made and those the collector freed.
type probes struct{ made, freed atomic.Int64 }

func installProbe(t *testing.T, rt *moejs.Runtime) *probes {
	t.Helper()
	p := &probes{}
	require.NoError(t, rt.SetGlobal("probe", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		b := make([]byte, probeSize)
		p.made.Add(1)
		runtime.AddCleanup(&b[0], func(n *atomic.Int64) { n.Add(1) }, &p.freed)
		ab, err := r.NewArrayBuffer(b)
		if err != nil {
			return moejs.Undefined(), err
		}
		return engine.ObjectValue(ab), nil
	})))
	return p
}

// collected fails unless the collector frees every buffer made so far.
func (p *probes) collected(t *testing.T) {
	t.Helper()
	made := p.made.Load()
	require.NotZero(t, made, "the script made no buffer")
	deadline := time.Now().Add(5 * time.Second)
	for p.freed.Load() < made {
		if time.Now().After(deadline) {
			t.Fatalf("%d of the %d buffers the runs dropped are still alive", made-p.freed.Load(), made)
		}
		runtime.GC()
		time.Sleep(2 * time.Millisecond)
	}
}

// loadProbed returns a runtime with probe() that loaded src.
func loadProbed(t *testing.T, src string) (*moejs.Runtime, *moejs.Module, *probes) {
	t.Helper()
	m, err := moejs.Compile("run.js", src)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	p := installProbe(t, rt)
	require.NoError(t, rt.Load(m))
	return rt, m, p
}

func TestFinishedRunFreesScript(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	p := installProbe(t, rt)
	s, err := moejs.CompileScript("run.js", `
function work() { const a = probe(), b = probe(); return a.byteLength + b.byteLength; }
work();
probe().byteLength;
{ let c = probe(); c.byteLength; }
0`)
	require.NoError(t, err)
	res, err := rt.RunScript(s)
	require.NoError(t, err)
	require.Equal(t, int64(0), int64(res.AsNumber()))
	rt.ReleaseCallData()
	p.collected(t)
	runtime.KeepAlive(rt)
}

func TestFinishedRunFreesCall(t *testing.T) {
	rt, m, p := loadProbed(t, `
export function hook(n) {
	const keep = [];
	for (let i = 0; i < n; i++) keep.push(probe());
	return nest(keep.length);
}
function nest(n) { const x = probe(); return n + inner(); }
function inner() { return probe().byteLength > 0 ? 1 : 0; }
export function closure() { return outer()(); }
function outer() { const a = probe(); return () => a.byteLength; }
`)
	res, err := rt.Call(mustHook(t, m, "hook"), moejs.Int(6))
	require.NoError(t, err)
	require.Equal(t, int64(7), int64(res.AsNumber()))
	rt.ReleaseCallData()
	p.collected(t)
	// The arrow outer returned ran last at depth 1: its record keeps it,
	// and its scope the buffer, unless the release clears the records.
	res, err = rt.Call(mustHook(t, m, "closure"))
	require.NoError(t, err)
	require.Equal(t, int64(probeSize), int64(res.AsNumber()))
	rt.ReleaseCallData()
	p.collected(t)
	runtime.KeepAlive(rt)
}

func TestFinishedRunFreesCallFunction(t *testing.T) {
	rt, _, p := loadProbed(t, `export const fn = function (n) { const a = probe(), b = probe(); return n + 1; };`)
	fn, ok := rt.Export("fn")
	require.True(t, ok)
	res, err := rt.CallFunction(fn, moejs.Undefined(), moejs.Int(1))
	require.NoError(t, err)
	require.Equal(t, int64(2), int64(res.AsNumber()))
	rt.ReleaseCallData()
	p.collected(t)
	runtime.KeepAlive(rt)
}

func TestFinishedRunFreesLoad(t *testing.T) {
	rt, _, p := loadProbed(t, `
{ const a = probe(); a.byteLength; }
(function () { const b = probe(), c = probe(); return b.byteLength + c.byteLength; })();
export const n = 1;
`)
	rt.ReleaseCallData()
	p.collected(t)
	runtime.KeepAlive(rt)
}

func TestFinishedRunFreesThrow(t *testing.T) {
	rt, m, p := loadProbed(t, `
export function hook() { const a = probe(); inner(); }
function inner() { const b = probe(); null.x; }
export function caught() {
	try { throw probe(); } catch (e) {}
	try { const c = probe(); throw new Error("dropped"); } catch (e) {}
	return 1;
}
`)
	_, err := rt.Call(mustHook(t, m, "hook"))
	require.Error(t, err)
	rt.ReleaseCallData()
	p.collected(t)
	_, err = rt.Call(mustHook(t, m, "caught"))
	require.NoError(t, err)
	rt.ReleaseCallData()
	p.collected(t)
	runtime.KeepAlive(rt)
}

func TestFinishedRunFreesGenerator(t *testing.T) {
	rt, m, p := loadProbed(t, `
export function hook() {
	function* g() { const a = probe(); yield 1; yield 2; }
	const it = g();
	return it.next().value;
}
let held;
export function start() {
	held = (function* () { const a = probe(); yield a.byteLength; yield 2; })();
	return held.next().value;
}
export function drop() { held = undefined; return 0; }
`)
	_, err := rt.Call(mustHook(t, m, "hook"))
	require.NoError(t, err)
	rt.ReleaseCallData()
	p.collected(t)
	// A generator suspended in one run and dropped in the next.
	_, err = rt.Call(mustHook(t, m, "start"))
	require.NoError(t, err)
	_, err = rt.Call(mustHook(t, m, "drop"))
	require.NoError(t, err)
	rt.ReleaseCallData()
	p.collected(t)
	runtime.KeepAlive(rt)
}

func TestFinishedRunFreesJobs(t *testing.T) {
	rt, m, p := loadProbed(t, `
export function hook() {
	Promise.resolve().then(() => { const a = probe(); return a.byteLength; });
	queueMicrotask(() => { const b = probe(); });
	(async () => { const c = probe(); await null; const d = probe(); return 1; })();
	new Promise((resolve) => resolve(probe())).then((v) => v.byteLength);
	return 1;
}
`)
	_, err := rt.Call(mustHook(t, m, "hook"))
	require.NoError(t, err)
	rt.ReleaseCallData()
	p.collected(t)
	runtime.KeepAlive(rt)
}

package eventloop_test

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/stretchr/testify/require"
)

// What the loop's tasks and callbacks made and dropped is garbage for Go's
// collector once Run returned: neither the engine nor the loop keeps a
// reference to it (the registers and frame records of the callbacks,
// which Run releases at its end, the batch of immediates the check phase
// ran, a settled promise of NewPromise). probe() returns an ArrayBuffer
// over a Go buffer whose collection the test sees (runtime.AddCleanup).

const probeSize = 1 << 20

type probes struct{ made, freed atomic.Int64 }

func installProbe(t *testing.T, rt *moejs.Runtime) *probes {
	t.Helper()
	p := &probes{}
	require.NoError(t, rt.SetGlobal("probe", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		return p.value(r)
	})))
	return p
}

// value returns a new probed buffer.
func (p *probes) value(r *moejs.Realm) (moejs.Value, error) {
	b := make([]byte, probeSize)
	p.made.Add(1)
	runtime.AddCleanup(&b[0], func(n *atomic.Int64) { n.Add(1) }, &p.freed)
	ab, err := r.NewArrayBuffer(b)
	if err != nil {
		return moejs.Undefined(), err
	}
	return engine.ObjectValue(ab), nil
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

func TestFinishedTasksFree(t *testing.T) {
	cases := []struct{ name, src string }{
		{"timer", `setTimeout(() => { const a = probe(), b = probe(); a.byteLength + b.byteLength; }, 0);`},
		{"interval", `let n = 0; const t = setInterval(() => { const a = probe(); if (++n === 3) clearInterval(t); }, 0);`},
		{"immediate", `setImmediate((x) => { const a = probe(); }, 1);`},
		{"property of an immediate", `(() => { const im = setImmediate(() => {}); im.held = probe(); })();`},
		{"promise job", `Promise.resolve().then(() => { const a = probe(); return a.byteLength; });`},
		{"promise job of a timer", `setTimeout(() => Promise.resolve(probe()).then((v) => { const a = probe(); return v.byteLength; }), 0);`},
		{"async function", `(async () => { const a = probe(); await new Promise((r) => setTimeout(r, 0)); const b = probe(); return a.byteLength; })();`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := moejs.NewRuntime(moejs.Options{})
			p := installProbe(t, rt)
			loop, err := eventloop.New(rt, eventloop.Options{})
			require.NoError(t, err)
			require.NoError(t, loop.Run(script(t, tc.src)))
			p.collected(t)
			runtime.KeepAlive(loop)
		})
	}
}

func TestFinishedPostedTaskFrees(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	p := installProbe(t, rt)
	m, err := moejs.Compile("task.js", `export function task() { const a = probe(), b = probe(); return a.byteLength; }`)
	require.NoError(t, err)
	require.NoError(t, rt.Load(m))
	h, err := m.Hook("task")
	require.NoError(t, err)
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	loop.RunOnLoop(func(rt *moejs.Runtime) {
		_, err := rt.Call(h)
		require.NoError(t, err)
	})
	require.NoError(t, loop.Run(nil))
	p.collected(t)
	runtime.KeepAlive(loop)
}

// A promise of NewPromise a loop with a memory limit lists keeps nothing
// once settled and awaited: its entry stays listed until the list is
// compacted, without the promise or its result.
func TestFinishedPromiseFrees(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 64 << 20})
	p := installProbe(t, rt)
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	require.NoError(t, rt.SetGlobal("tool", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		pr, settle := loop.NewPromise()
		go settle(func(rt *moejs.Runtime) (moejs.Value, error) { return p.value(rt.Realm()) })
		return pr, nil
	})))
	require.NoError(t, loop.Run(script(t, `(async () => { for (let i = 0; i < 3; i++) { const v = await tool(); v.byteLength; } })();`)))
	p.collected(t)
	runtime.KeepAlive(loop)
}

// A loop started with Start releases the engine's scratch when it ends.
func TestFinishedStartFrees(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	p := installProbe(t, rt)
	s, err := moejs.CompileScript("task.js", `(function () { const a = probe(), b = probe(); return a.byteLength; })();`)
	require.NoError(t, err)
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	require.NoError(t, loop.Start())
	ran := make(chan error, 1)
	loop.RunOnLoop(func(rt *moejs.Runtime) {
		_, err := rt.RunScript(s)
		ran <- err
	})
	require.NoError(t, <-ran)
	require.NoError(t, loop.Stop())
	p.collected(t)
	runtime.KeepAlive(loop)
}

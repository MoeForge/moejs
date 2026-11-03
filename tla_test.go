package moejs_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadTopLevelAwait checks Load of a module with top-level await: the
// evaluation completes in Load's drain, a rejection is Load's error and a
// pending evaluation is ErrModulePending.
func TestLoadTopLevelAwait(t *testing.T) {
	load := func(src string) (*moejs.Runtime, error) {
		mod, err := moejs.Compile("tla.js", src)
		require.NoError(t, err)
		rt := moejs.NewRuntime(moejs.Options{})
		return rt, rt.Load(mod)
	}
	rt, err := load(`export const config = await Promise.resolve({ key: "k" }); export function key() { return config.key; }`)
	require.NoError(t, err)
	v, ok := rt.Export("config")
	require.True(t, ok)
	assert.True(t, v.IsObject())
	res, err := rt.Call(mustHook(t, rt.Module(), "key"))
	require.NoError(t, err)
	assert.Equal(t, "k", res.String())

	_, err = load(`await 0; throw new TypeError("after await");`)
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "TypeError: after await", exc.Error())

	rt, err = load(`export const a = 1; await new Promise(() => {});`)
	assert.ErrorIs(t, err, moejs.ErrModulePending)
	v, ok = rt.Export("a")
	require.True(t, ok, "bindings initialized before the await stay readable")
	assert.Equal(t, "1", v.String())
}

// TestLoadTopLevelAwaitJobs checks Load's job hold with top-level await:
// the top level resumed from a job finds the module's bindings and hooks, a
// job error after a fulfilled evaluation is Load's error, the evaluation's
// rejection wins over one and is told to the tracker once, an interrupt wins
// over the rejection, also over a throw before the first await, whose hooks
// then return the throw, and an interrupt of a pending evaluation leaves the
// runtime reusable.
func TestLoadTopLevelAwaitJobs(t *testing.T) {
	type load struct {
		rt   *moejs.Runtime
		err  error
		seen []string
		ops  []moejs.PromiseRejectionOperation
	}
	run := func(src string, interrupt bool) *load {
		mod, err := moejs.Compile("tla.js", src)
		require.NoError(t, err)
		l := &load{rt: moejs.NewRuntime(moejs.Options{})}
		l.rt.SetPromiseRejectionTracker(func(_ moejs.Value, op moejs.PromiseRejectionOperation) { l.ops = append(l.ops, op) })
		require.NoError(t, l.rt.SetGlobal("host", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			x, ok := l.rt.Export("x")
			v, err := l.rt.Call(mustHook(t, mod, "get"))
			l.seen = append(l.seen, fmt.Sprint(ok, " ", x.String(), " ", v.String(), " ", err, " ", l.rt.Load(mod) != nil))
			if interrupt {
				time.AfterFunc(20*time.Millisecond, func() { l.rt.Interrupt("stop") })
			}
			return moejs.Undefined(), nil
		})))
		l.err = l.rt.Load(mod)
		return l
	}
	const top = "export let x = 1; export function get() { return x; }\n"

	l := run(top+`host(); await null; x = 2; host();`, false)
	require.NoError(t, l.err)
	assert.Equal(t, []string{
		"false undefined undefined " + moejs.ErrHookNotFound.Error() + " true",
		"true 2 2 <nil> true",
	}, l.seen)

	var exc *moejs.Exception
	l = run(top+`await null; queueMicrotask(() => { throw new Error("job") });`, false)
	require.ErrorAs(t, l.err, &exc)
	assert.Equal(t, "Error: job", exc.Error())
	assert.Empty(t, l.ops)

	l = run(top+`queueMicrotask(() => { throw new Error("job") }); await null; throw new Error("tla");`, false)
	require.ErrorAs(t, l.err, &exc)
	assert.Equal(t, "Error: tla", exc.Error())
	assert.Equal(t, []moejs.PromiseRejectionOperation{moejs.PromiseRejectionReject}, l.ops)

	l = run(top+`await null; host(); queueMicrotask(() => { for (;;) {} }); throw new Error("tla");`, true)
	var ie *moejs.InterruptedError
	require.ErrorAs(t, l.err, &ie)
	assert.Equal(t, []moejs.PromiseRejectionOperation{moejs.PromiseRejectionReject}, l.ops)
	l.rt.ClearInterrupt()

	l = run(top+`host(); queueMicrotask(() => { for (;;) {} }); throw new Error("tla"); await 0;`, true)
	require.ErrorAs(t, l.err, &ie)
	assert.Equal(t, []moejs.PromiseRejectionOperation{moejs.PromiseRejectionReject}, l.ops)
	l.rt.ClearInterrupt()
	_, err := l.rt.Call(mustHook(t, l.rt.Module(), "get"))
	require.ErrorAs(t, err, &exc, "the hooks of a top level that threw before its first await")
	assert.Equal(t, "Error: tla", exc.Error())

	l = run(top+`await null; host(); x++; for (;;) {}`, true)
	require.ErrorAs(t, l.err, &ie)
	assert.Empty(t, l.ops)
	l.rt.ClearInterrupt()
	v, err := l.rt.Call(mustHook(t, l.rt.Module(), "get"))
	require.NoError(t, err)
	assert.Equal(t, "2", v.String())
}

// TestLoadTopLevelAwaitPanic checks a Go panic in a job that Load's drain
// runs after the top level awaited: Load returns it as an *InternalError,
// not the top level's rejection, which the tracker is still told once; the
// call bookkeeping is restored and the jobs the panic left are dropped.
func TestLoadTopLevelAwaitPanic(t *testing.T) {
	mod, err := moejs.Compile("tla.js", `export let n = 0;
export function get() { return n; }
await null;
queueMicrotask(() => bug());
queueMicrotask(() => { n = 2; });
n = 1;
throw new Error("tla");`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	var ops []moejs.PromiseRejectionOperation
	rt.SetPromiseRejectionTracker(func(_ moejs.Value, op moejs.PromiseRejectionOperation) { ops = append(ops, op) })
	require.NoError(t, rt.SetGlobal("bug", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		var m map[string]int
		m["x"] = 1
		return moejs.Undefined(), nil
	})))
	depth := rt.Realm().CallDepth()
	err = rt.Load(mod)
	var ie *moejs.InternalError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "moejs: internal error: assignment to entry in nil map", ie.Error())
	assert.NotEmpty(t, ie.Stack)
	assert.Equal(t, depth, rt.Realm().CallDepth())
	assert.Equal(t, []moejs.PromiseRejectionOperation{moejs.PromiseRejectionReject}, ops)
	v, ok := rt.Export("n")
	require.True(t, ok)
	assert.Equal(t, "1", v.String())
	res, err := rt.Call(mustHook(t, mod, "get"))
	require.NoError(t, err)
	assert.Equal(t, "1", res.String())
	v, _ = rt.Export("n")
	assert.Equal(t, "1", v.String(), "the job after the panic does not run in the next call")
}

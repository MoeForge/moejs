package moejs_test

import (
	"errors"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// asyncUnwindSource: victim awaits, and the job that resumes it calls
// step, which a test makes panic, interrupt or fail; probe and probeAwait
// are async functions a test calls with victim's promise as this.
const asyncUnwindSource = `
export let held;
export async function victim() {
	await null;
	step();
	const keep = [];
	for (;;) keep.push({a: 1});
}
export function start() { held = victim(); return "started"; }
export async function probe() { return "probe"; }
export async function probeAwait() { await null; return "probeAwait"; }
export function probeIn() { return probe.call(held); }
export function probeAwaitIn() { return probeAwait.call(held); }
`

// newAsyncUnwind loads asyncUnwindSource with step as the host function
// the resumed victim calls.
func newAsyncUnwind(t *testing.T, opts moejs.Options, step func(rt *moejs.Runtime) error) (*moejs.Runtime, *moejs.Module) {
	t.Helper()
	mod, err := moejs.Compile("unwind.js", asyncUnwindSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(opts)
	require.NoError(t, rt.SetGlobal("step", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Undefined(), step(rt)
	})))
	require.NoError(t, rt.Load(mod))
	return rt, mod
}

// exportOf returns the value of mod's export name.
func exportOf(t *testing.T, rt *moejs.Runtime, name string) moejs.Value {
	t.Helper()
	v, ok := rt.Export(name)
	require.True(t, ok, name)
	return v
}

// requireUntouched checks that the async function calls made with held
// as this, from Go and from JavaScript, without and with an await, settle
// their own promises and leave held in the state it had.
func requireUntouched(t *testing.T, rt *moejs.Runtime, mod *moejs.Module) {
	t.Helper()
	held := exportOf(t, rt, "held")
	state, result, ok := moejs.PromiseResult(held)
	require.True(t, ok)
	check := func(what string, p moejs.Value, err error, want string) {
		t.Helper()
		require.NoError(t, err, what)
		st, v, ok := moejs.PromiseResult(p)
		require.True(t, ok, what)
		assert.Equal(t, moejs.PromiseFulfilled, st, what)
		assert.Equal(t, want, v.String(), what)
		st, v, _ = moejs.PromiseResult(held)
		assert.Equal(t, state, st, what+": held's state")
		assert.True(t, v == result, what+": held's result")
	}
	for _, c := range []struct{ fn, want string }{{"probe", "probe"}, {"probeAwait", "probeAwait"}} {
		p, err := rt.CallFunction(exportOf(t, rt, c.fn), held)
		check(c.fn+" from Go", p, err, c.want)
		p, err = rt.Call(mustHook(t, mod, c.fn+"In"))
		check(c.fn+" from JavaScript", p, err, c.want)
	}
}

// TestAsyncResumePanic: a Go panic in a host function the job resuming an
// async function call calls leaves the call's promise pending, and async
// function calls whose this is that promise, with or without an await,
// are new calls that settle their own promises.
func TestAsyncResumePanic(t *testing.T) {
	rt, mod := newAsyncUnwind(t, moejs.Options{}, func(*moejs.Runtime) error { panic("host bug") })
	_, err := rt.Call(mustHook(t, mod, "start"))
	var ie *moejs.InternalError
	require.ErrorAs(t, err, &ie)
	st, _, ok := moejs.PromiseResult(exportOf(t, rt, "held"))
	require.True(t, ok)
	require.Equal(t, moejs.PromisePending, st)
	requireUntouched(t, rt, mod)
}

// TestAsyncResumeUnwind: an interrupt, a memory limit stop or an exception
// out of an async function call a job resumed leaves async function calls
// whose this is the call's promise new calls.
func TestAsyncResumeUnwind(t *testing.T) {
	errStop := errors.New("stop")
	errStep := errors.New("step failed")
	for _, c := range []struct {
		name  string
		opts  moejs.Options
		step  func(rt *moejs.Runtime) error
		check func(t *testing.T, rt *moejs.Runtime, err error, held moejs.Value)
	}{
		{"interrupt", moejs.Options{}, func(rt *moejs.Runtime) error { rt.Interrupt(errStop); return nil },
			func(t *testing.T, rt *moejs.Runtime, err error, held moejs.Value) {
				require.ErrorIs(t, err, errStop)
				st, _, _ := moejs.PromiseResult(held)
				assert.Equal(t, moejs.PromisePending, st)
			}},
		{"memory limit", moejs.Options{MemoryLimit: 1 << 20}, func(*moejs.Runtime) error { return nil },
			func(t *testing.T, rt *moejs.Runtime, err error, held moejs.Value) {
				requireMemoryHit(t, err, 1<<20)
				st, _, _ := moejs.PromiseResult(held)
				assert.Equal(t, moejs.PromisePending, st)
			}},
		{"exception", moejs.Options{}, func(*moejs.Runtime) error { return errStep },
			func(t *testing.T, rt *moejs.Runtime, err error, held moejs.Value) {
				require.NoError(t, err)
				st, v, _ := moejs.PromiseResult(held)
				require.Equal(t, moejs.PromiseRejected, st)
				msg, err := rt.Get(v, "message")
				require.NoError(t, err)
				assert.Equal(t, errStep.Error(), msg.String())
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt, mod := newAsyncUnwind(t, c.opts, c.step)
			_, err := rt.Call(mustHook(t, mod, "start"))
			c.check(t, rt, err, exportOf(t, rt, "held"))
			rt.ClearInterrupt()
			rt.ReleaseCallData()
			requireUntouched(t, rt, mod)
		})
	}
}

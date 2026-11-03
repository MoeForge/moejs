package moejs_test

import (
	"errors"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCallFunction checks CallFunction on a callback a host function kept:
// this and the arguments reach it, the jobs it queues run before
// CallFunction returns, and a value that is not a function is
// ErrNotCallable.
func TestCallFunction(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	var kept moejs.Value
	require.NoError(t, rt.SetGlobal("keep", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		kept = moejs.Arg(args, 0)
		return moejs.Undefined(), nil
	})))
	mod, err := moejs.Compile("cb.js", `export let log = "";
keep(function (a, b) { queueMicrotask(() => { log += "job;"; }); log += "call;"; return this.name + (a + b); });`)
	require.NoError(t, err)
	require.NoError(t, rt.Load(mod))
	this, err := rt.ParseJSON([]byte(`{"name":"sum="}`))
	require.NoError(t, err)

	res, err := rt.CallFunction(kept, this, moejs.Int(2), moejs.Int(3))
	require.NoError(t, err)
	assert.Equal(t, "sum=5", res.String())
	log, _ := rt.Export("log")
	assert.Equal(t, "call;job;", log.String(), "the job ran before CallFunction returned")

	for _, v := range []moejs.Value{moejs.Undefined(), moejs.Int(1), moejs.String("f"), this} {
		_, err = rt.CallFunction(v, moejs.Undefined())
		assert.ErrorIs(t, err, moejs.ErrNotCallable, v.String())
	}

	thrower, err := rt.RunScript(mustCompileScript(t, `(function () { throw new TypeError("thrown"); })`))
	require.NoError(t, err)
	_, err = rt.CallFunction(thrower, moejs.Undefined())
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "TypeError: thrown", exc.Error())

	jobThrower, err := rt.RunScript(mustCompileScript(t, `(function () { queueMicrotask(() => { throw new RangeError("job"); }); return 1; })`))
	require.NoError(t, err)
	res, err = rt.CallFunction(jobThrower, moejs.Undefined())
	require.ErrorAs(t, err, &exc, "the first exception of the jobs is the error")
	assert.Equal(t, "RangeError: job", exc.Error())
	assert.Equal(t, "1", res.String())
}

func mustCompileScript(t *testing.T, src string) *moejs.Script {
	t.Helper()
	s, err := moejs.CompileScript("s.js", src)
	require.NoError(t, err)
	return s
}

// TestCallFunctionNested checks CallFunction from inside a host function:
// the jobs it queues wait for the end of the outermost call.
func TestCallFunctionNested(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("callBack", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return rt.CallFunction(moejs.Arg(args, 0), moejs.Undefined(), moejs.String("x"))
	})))
	res, err := rt.RunScript(mustCompileScript(t, `
let log = "";
const r = callBack(v => { queueMicrotask(() => { log += "job;"; }); log += "cb " + v + ";"; return "ret"; });
log += "after " + r + ";";
Promise.resolve().then(() => log)`))
	require.NoError(t, err)
	state, v, ok := moejs.PromiseResult(res)
	require.True(t, ok)
	assert.Equal(t, moejs.PromiseFulfilled, state)
	assert.Equal(t, "cb x;after ret;job;", v.String())
}

// TestCallFunctionGuards checks CallFunction's guards: an interrupt stops
// it before a host function runs, a Go panic is an *InternalError that
// leaves the runtime usable, and a function of another runtime as fn,
// this or an argument is ErrForeign.
func TestCallFunctionGuards(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	ran := 0
	native := rt.Function("native", 0, func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		ran++
		return moejs.Undefined(), nil
	})
	rt.Interrupt("stop")
	_, err := rt.CallFunction(native, moejs.Undefined())
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	assert.Zero(t, ran)
	rt.ClearInterrupt()
	_, err = rt.CallFunction(native, moejs.Undefined())
	require.NoError(t, err)
	assert.Equal(t, 1, ran)

	depth := rt.Realm().CallDepth()
	panicky := rt.Function("panicky", 0, func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		panic("host bug")
	})
	wrapper, err := rt.RunScript(mustCompileScript(t, `(f => [1].map(() => f()))`))
	require.NoError(t, err)
	_, err = rt.CallFunction(wrapper, moejs.Undefined(), panicky)
	var internal *moejs.InternalError
	require.ErrorAs(t, err, &internal)
	assert.Equal(t, "host bug", internal.Value)
	assert.Equal(t, depth, rt.Realm().CallDepth())
	res, err := rt.CallFunction(wrapper, moejs.Undefined(), native)
	require.NoError(t, err)
	assert.True(t, res.IsObject())

	other, _ := loadRuntime(t, `export function f() { return 1; }`)
	foreign, ok := other.Export("f")
	require.True(t, ok)
	own, err := rt.RunScript(mustCompileScript(t, `(function () { return typeof this; })`))
	require.NoError(t, err)
	_, err = rt.CallFunction(foreign, moejs.Undefined())
	assert.ErrorIs(t, err, moejs.ErrForeign)
	_, err = rt.CallFunction(own, foreign)
	assert.ErrorIs(t, err, moejs.ErrForeign)
	_, err = rt.CallFunction(own, moejs.Undefined(), moejs.Int(1), foreign)
	assert.ErrorIs(t, err, moejs.ErrForeign)
	res, err = rt.CallFunction(own, moejs.Undefined())
	require.NoError(t, err)
	assert.Equal(t, "object", res.String(), "a sloppy function's undefined this is the global object")
}

// TestThrownValue checks ThrownValue: a Go error becomes the Error a host
// function's error throws, which unwraps to it when it comes back thrown,
// an *Exception gives its value, and nil and an interrupt give none.
func TestThrownValue(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	errHost := errors.New("host failure")
	v, ok := rt.ThrownValue(errHost)
	require.True(t, ok)
	rethrow, err := rt.RunScript(mustCompileScript(t, `(e => { if (!(e instanceof Error) || e.message !== "host failure") return "wrong"; throw e; })`))
	require.NoError(t, err)
	_, err = rt.CallFunction(rethrow, moejs.Undefined(), v)
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.ErrorIs(t, err, errHost)

	p, _, reject := rt.NewPromise()
	require.NoError(t, reject(v))
	_, reason, _ := moejs.PromiseResult(p)
	assert.Equal(t, v, reason)

	v, ok = rt.ThrownValue(&moejs.Exception{Value: moejs.Int(7)})
	require.True(t, ok)
	assert.Equal(t, "7", v.String())
	_, ok = rt.ThrownValue(nil)
	assert.False(t, ok)
	_, ok = rt.ThrownValue(&moejs.InterruptedError{Value: "x"})
	assert.False(t, ok)
}

// TestCallFunctionNestedPanic checks what CallFunction's comment says of a
// Go panic below a CallFunction made inside a host function: the host
// function gets the *InternalError, and when it returns it, JavaScript
// catches it as an Error, or the outermost call returns an *Exception that
// unwraps to it.
func TestCallFunctionNestedPanic(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	panicky := rt.Function("panicky", 0, func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		panic("host bug")
	})
	var nested error
	require.NoError(t, rt.SetGlobal("callIt", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		_, nested = rt.CallFunction(panicky, moejs.Undefined())
		return moejs.Undefined(), nested
	})))
	res, err := rt.RunScript(mustCompileScript(t, `try { callIt(); "no throw" } catch (e) { e instanceof Error ? e.message : "other" }`))
	require.NoError(t, err)
	var ie *moejs.InternalError
	require.ErrorAs(t, nested, &ie)
	assert.Equal(t, "host bug", ie.Value)
	assert.Equal(t, "moejs: internal error: host bug", res.String())

	_, err = rt.RunScript(mustCompileScript(t, `callIt()`))
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.ErrorAs(t, err, &ie)
}

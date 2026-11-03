package moejs_test

import (
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foreignSource makes the values another runtime must not run: its
// functions' inline caches are bound in its own table.
const foreignSource = `export function big(o) { return o.a + o.b + o.c; }
export function mk() { return {a: 1, b: 2, c: 3}; }
export function* gen(o) { yield o.a; yield o.b + o.c; }
export async function* agen(o) { yield o.a; }
export async function af(o) { return o.a; }
export const bound = big.bind(null);
export const started = gen(mk());
started.next();
export const dyn = new Function("o", "return o.a + o.b + o.c");
export const parse = JSON.parse;`

func loadRuntime(t *testing.T, src string) (*moejs.Runtime, *moejs.Module) {
	t.Helper()
	rt := moejs.NewRuntime(moejs.Options{})
	m, err := moejs.Compile("m.js", src)
	require.NoError(t, err)
	require.NoError(t, rt.Load(m))
	return rt, m
}

// TestForeignFunctions checks the realm-affinity contract at the root
// boundary: a function or generator of another runtime passed to Call,
// SetGlobal, FromGo or a settler of NewPromise is ErrForeign, not a Go panic or a wrong result from
// inline caches indexed in the wrong table. Dynamic functions and shared
// intrinsics run anywhere.
func TestForeignFunctions(t *testing.T) {
	rtB, mB := loadRuntime(t, foreignSource)
	o, err := rtB.Call(mustHook(t, mB, "mk"))
	require.NoError(t, err)
	res, err := rtB.Call(mustHook(t, mB, "big"), o)
	require.NoError(t, err)
	require.Equal(t, "6", res.String())

	rtA, mA := loadRuntime(t, `export function h(o) { return o.c + o.c + o.c; }
export function mk() { return {a: 1, b: 2, c: 3}; }
export function callIt(f, o) { return f(o); }
export function nextOf(it) { return it.next().value; }
export function own() { return h; }`)
	// A's caches for h sit where big's are in B's table.
	own, err := rtA.Call(mustHook(t, mA, "mk"))
	require.NoError(t, err)
	res, err = rtA.Call(mustHook(t, mA, "h"), own)
	require.NoError(t, err)
	require.Equal(t, "9", res.String())

	callIt := mustHook(t, mA, "callIt")
	for _, name := range []string{"big", "bound", "af"} {
		f, ok := rtB.Export(name)
		require.True(t, ok)
		_, err = rtA.Call(callIt, f, o)
		assert.ErrorIs(t, err, moejs.ErrForeign, name)
		_, err = rtA.FromGo(f)
		assert.ErrorIs(t, err, moejs.ErrForeign, name)
		assert.ErrorIs(t, rtA.SetGlobal("f", f), moejs.ErrForeign, name)
		_, resolve, reject := rtA.NewPromise()
		assert.ErrorIs(t, resolve(f), moejs.ErrForeign, name)
		assert.ErrorIs(t, reject(f), moejs.ErrForeign, name)
	}
	gen, _ := rtB.Export("gen")
	it, err := rtB.Call(mustHook(t, mB, "gen"), o)
	require.NoError(t, err)
	agen, err := rtB.Call(mustHook(t, mB, "agen"), o)
	require.NoError(t, err)
	started, _ := rtB.Export("started")
	for _, it := range []moejs.Value{it, agen, started} {
		_, err = rtA.Call(mustHook(t, mA, "nextOf"), it)
		assert.ErrorIs(t, err, moejs.ErrForeign)
	}
	_, err = rtA.Call(callIt, gen, o)
	assert.ErrorIs(t, err, moejs.ErrForeign)
	// A refused settle leaves the promise pending.
	p, resolve, _ := rtA.NewPromise()
	assert.ErrorIs(t, resolve(it), moejs.ErrForeign)
	st, _, _ := moejs.PromiseResult(p)
	assert.Equal(t, moejs.PromisePending, st)
	require.NoError(t, resolve(moejs.Int(5)))
	st, v, _ := moejs.PromiseResult(p)
	assert.Equal(t, moejs.PromiseFulfilled, st)
	assert.Equal(t, "5", v.String())

	// The runtimes stay usable, with their own results.
	res, err = rtA.Call(mustHook(t, mA, "h"), own)
	require.NoError(t, err)
	assert.Equal(t, "9", res.String())
	res, err = rtB.Call(mustHook(t, mB, "big"), o)
	require.NoError(t, err)
	assert.Equal(t, "6", res.String())
	res, err = rtB.Call(mustHook(t, mB, "big"), o)
	require.NoError(t, err)
	assert.Equal(t, "6", res.String())

	// Dynamic code binds its caches in the calling realm; a shared
	// intrinsic belongs to no realm; a function of the runtime itself is
	// not foreign.
	dyn, _ := rtB.Export("dyn")
	res, err = rtA.Call(callIt, dyn, o)
	require.NoError(t, err)
	assert.Equal(t, "6", res.String())
	parse, _ := rtB.Export("parse")
	res, err = rtA.Call(callIt, parse, moejs.String("7"))
	require.NoError(t, err)
	assert.Equal(t, "7", res.String())
	h, err := rtA.Call(mustHook(t, mA, "own"))
	require.NoError(t, err)
	res, err = rtA.Call(callIt, h, o)
	require.NoError(t, err)
	assert.Equal(t, "9", res.String())
	_, err = rtA.FromGo(h)
	require.NoError(t, err)
	require.NoError(t, rtA.SetGlobal("h2", h))
}

// TestForeignFunctionSameLayout is TestForeignFunctions with two runtimes
// whose tables line up: without the check, big would run on h's caches in
// A's table and return 9.
func TestForeignFunctionSameLayout(t *testing.T) {
	rtB, mB := loadRuntime(t, `export function big(o) { return o.a + o.b + o.c; }
export function mk() { return {a: 1, b: 2, c: 3}; }`)
	o, err := rtB.Call(mustHook(t, mB, "mk"))
	require.NoError(t, err)
	res, err := rtB.Call(mustHook(t, mB, "big"), o)
	require.NoError(t, err)
	require.Equal(t, "6", res.String())
	rtA, mA := loadRuntime(t, `export function h(o) { return o.c + o.c + o.c; }
export function mk() { return {a: 1, b: 2, c: 3}; }
export function callIt(f, o) { return f(o); }`)
	_, err = rtA.Call(mustHook(t, mA, "mk"))
	require.NoError(t, err)
	res, err = rtA.Call(mustHook(t, mA, "h"), o)
	require.NoError(t, err)
	require.Equal(t, "9", res.String())
	big, _ := rtB.Export("big")
	res, err = rtA.Call(mustHook(t, mA, "callIt"), big, o)
	require.ErrorIs(t, err, moejs.ErrForeign, "result %v", res)
}

// TestForeignProxies is TestForeignFunctions for the proxy forms: a proxy
// runs its target's code, so a proxy of another runtime's function or
// generator, and a bound function of one, are ErrForeign; a callable proxy
// another runtime created runs that runtime's traps, so it is ErrForeign
// whatever its target, revoked or not. The forms reached through an object
// (an object proxy's traps, an accessor, a method, an array element, a
// promise or thenable) are not checked: only Go values and JSON cross
// between runtimes.
func TestForeignProxies(t *testing.T) {
	rtB, mB := loadRuntime(t, `export function big(o) { return o.a + o.b + o.c; }
export function mk() { return {a: 1, b: 2, c: 3}; }
export function* gen(o) { yield o.a; yield o.b + o.c; }
export const prox = new Proxy(big, {});
export const proxProx = new Proxy(prox, {});
export const boundProx = prox.bind(null);
export const proxBound = new Proxy(big.bind(null), {});
export const genProx = new Proxy(gen(mk()), {});
const dyn = new Function("return function (o) { return o.a + o.b + o.c; }")();
export const dynProx = new Proxy(dyn, {});
const r = Proxy.revocable(big, {});
r.revoke();
export const revoked = r.proxy;
export const trapNative = new Proxy(Math.max, { apply(t, th, args) { return big(args[0]); } });
export const trapDyn = new Proxy(new Function("return 1"), { apply(t, th, args) { return big(args[0]); } });
export const trapCtor = new Proxy(Object, { construct(t, args) { return { v: big(args[0]) }; } });`)
	o, err := rtB.Call(mustHook(t, mB, "mk"))
	require.NoError(t, err)
	res, err := rtB.Call(mustHook(t, mB, "big"), o)
	require.NoError(t, err)
	require.Equal(t, "6", res.String())

	rtA, mA := loadRuntime(t, `export function h(o) { return o.c + o.c + o.c; }
export function mk() { return {a: 1, b: 2, c: 3}; }
export function callIt(f, o) { return f(o); }
export function newIt(f, o) { return new f(o).v; }
export function nextOf(it) { return it.next().value; }`)
	own, err := rtA.Call(mustHook(t, mA, "mk"))
	require.NoError(t, err)
	res, err = rtA.Call(mustHook(t, mA, "h"), own)
	require.NoError(t, err)
	require.Equal(t, "9", res.String())

	exp := func(name string) moejs.Value {
		v, ok := rtB.Export(name)
		require.True(t, ok, name)
		return v
	}
	// A callable proxy of B runs B's traps whatever its target, so the
	// proxies of dynamic code and of intrinsics with traps that call big
	// are foreign too, and so is a revoked one.
	callIt, newIt := mustHook(t, mA, "callIt"), mustHook(t, mA, "newIt")
	for _, name := range []string{"prox", "proxProx", "boundProx", "proxBound", "dynProx", "revoked", "trapNative", "trapDyn", "trapCtor"} {
		f := exp(name)
		hook := callIt
		if name == "trapCtor" {
			hook = newIt
		}
		_, err = rtA.Call(hook, f, o)
		assert.ErrorIs(t, err, moejs.ErrForeign, name)
		_, err = rtA.FromGo(f)
		assert.ErrorIs(t, err, moejs.ErrForeign, name)
		assert.ErrorIs(t, rtA.SetGlobal("f", f), moejs.ErrForeign, name)
		_, resolve, reject := rtA.NewPromise()
		assert.ErrorIs(t, resolve(f), moejs.ErrForeign, name)
		assert.ErrorIs(t, reject(f), moejs.ErrForeign, name)
	}
	_, err = rtA.Call(mustHook(t, mA, "nextOf"), exp("genProx"))
	assert.ErrorIs(t, err, moejs.ErrForeign)

	// The runtimes stay usable, with their own results.
	res, err = rtA.Call(mustHook(t, mA, "h"), own)
	require.NoError(t, err)
	assert.Equal(t, "9", res.String())
	res, err = rtB.Call(mustHook(t, mB, "big"), o)
	require.NoError(t, err)
	assert.Equal(t, "6", res.String())
}

// TestFromGoForeignNested checks the Go containers FromGo converts: a
// function of another runtime (a function, a bound function, a callable
// proxy) at the root as an *Object is ErrForeign, as it is as a Value, and
// inside a map or slice, at any depth, reading its member throws a
// TypeError with ErrForeign's text and runs nothing; SetGlobal of such a map
// behaves the same. A function of the runtime itself still converts and
// runs.
func TestFromGoForeignNested(t *testing.T) {
	rtB, mB := loadRuntime(t, `let n = 0;
export function f(x) { n += x; return n; }
export const bound = f.bind(null);
export const prox = new Proxy(f, {});
export function count() { return n; }`)
	rtA, mA := loadRuntime(t, `function attempt(get) {
  try { return "ran " + get()(1); } catch (e) { return e.name + ": " + e.message; }
}
export function viaMap(m) { return attempt(() => m.f); }
export function viaList(l) { return attempt(() => l[0]); }
export function viaMaps(l) { return attempt(() => l[0].f); }
export function viaNested(m) { return attempt(() => m.inner.list[1]); }
export function viaGlobal() { return attempt(() => g.f); }
export function own(x) { return x * 2; }
export function ownFn() { return own; }`)
	const thrown = "TypeError: " + "moejs: function or generator of another runtime"
	require.Equal(t, thrown, "TypeError: "+moejs.ErrForeign.Error())
	for _, name := range []string{"f", "bound", "prox"} {
		fn, ok := rtB.Export(name)
		require.True(t, ok)
		_, err := rtA.FromGo(fn)
		assert.ErrorIs(t, err, moejs.ErrForeign, name)
		_, err = rtA.FromGo(fn.AsObject())
		assert.ErrorIs(t, err, moejs.ErrForeign, name)
		for _, c := range []struct {
			hook string
			v    any
		}{
			{"viaMap", map[string]any{"f": fn}},
			{"viaMap", map[string]any{"f": fn.AsObject()}},
			{"viaList", []any{fn}},
			{"viaMaps", []map[string]any{{"f": fn}}},
			{"viaNested", map[string]any{"inner": map[string]any{"list": []any{1, fn}}}},
		} {
			arg, err := rtA.FromGo(c.v)
			require.NoError(t, err, name)
			res, err := rtA.Call(mustHook(t, mA, c.hook), arg)
			require.NoError(t, err, name)
			assert.Equal(t, thrown, res.String(), "%s %s", name, c.hook)
		}
		require.NoError(t, rtA.SetGlobal("g", map[string]any{"f": fn}))
		res, err := rtA.Call(mustHook(t, mA, "viaGlobal"))
		require.NoError(t, err)
		assert.Equal(t, thrown, res.String(), name)
	}
	n, err := rtB.Call(mustHook(t, mB, "count"))
	require.NoError(t, err)
	assert.Equal(t, "0", n.String(), "nothing of B ran")

	own, err := rtA.Call(mustHook(t, mA, "ownFn"))
	require.NoError(t, err)
	for _, v := range []any{own, own.AsObject()} {
		arg, err := rtA.FromGo(map[string]any{"f": v})
		require.NoError(t, err)
		res, err := rtA.Call(mustHook(t, mA, "viaMap"), arg)
		require.NoError(t, err)
		assert.Equal(t, "ran 2", res.String())
	}
}

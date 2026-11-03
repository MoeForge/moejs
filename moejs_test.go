package moejs_test

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hostSource = `
let calls = 0;
export const meta = { key: "smoke", version: "1.0.0", models: ["a", "b"] };
export function decode(ctx) {
  calls++;
  const req = ctx.body;
  if (!req || typeof req !== "object") throw new TypeError("body must be an object");
  return { model: req.model, n: req.n ?? 1, prompt: String(req.prompt).trim(), auth: utils.auth(ctx.token), calls };
}
export const protocols = {
  "openai.images": { decodeRequest(ctx) { return { kind: "image", size: ctx.size }; } },
  broken: { decodeRequest: 42 },
  get lazy() { return { decodeRequest() { return "lazy"; } }; },
  get throwing() { throw new Error("getter failed"); },
};
export const native = null;
export function fail(kind) {
  switch (kind) {
  case "string": throw "raw text";
  case "object": throw { toString() { return "custom"; } };
  case "accessor": { const e = new Error("x"); Object.defineProperty(e, "message", { get() { throw new Error("nested"); } }); throw e; }
  case "host": return utils.fail();
  case "caught": try { utils.fail(); } catch (e) { return e.message + "|" + e.name; }
  case "loop": for (;;) {}
  case "overflow": { const f = () => f(); return f(); }
  }
  throw new RangeError("kind " + kind);
}
export function echo(...args) { return args; }
export let later = 1;
export function replace() { later = function () { return "replaced"; }; }
`

var errHost = errors.New("host failure")

func newHostRuntime(t testing.TB, mod *moejs.Module) *moejs.Runtime {
	t.Helper()
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("utils", map[string]any{
		"auth": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			return moejs.Bool(moejs.Arg(args, 0).IsString()), nil
		}),
		"fail": moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			return moejs.Undefined(), fmt.Errorf("sign request: %w", errHost)
		}),
	}))
	require.NoError(t, rt.Load(mod))
	return rt
}

func mustHook(t testing.TB, mod *moejs.Module, export string, members ...string) moejs.Hook {
	t.Helper()
	h, err := mod.Hook(export, members...)
	require.NoError(t, err)
	return h
}

func TestCallRoundTrip(t *testing.T) {
	mod, err := moejs.Compile("plugin.js", hostSource)
	require.NoError(t, err)
	assert.Equal(t, "plugin.js", mod.Name())
	assert.Equal(t, []string{"decode", "echo", "fail", "later", "meta", "native", "protocols", "replace"}, mod.Exports())
	rt := newHostRuntime(t, mod)
	decode := mustHook(t, mod, "decode")

	arg, err := rt.FromGo(map[string]any{"token": "sk", "body": map[string]any{"model": "a", "prompt": "  hi  "}})
	require.NoError(t, err)
	res, err := rt.Call(decode, arg)
	require.NoError(t, err)
	out, err := rt.ToGo(res)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"model": "a", "n": int64(1), "prompt": "hi", "auth": true, "calls": int64(1)}, out)

	arg, err = rt.ParseJSON([]byte(`{"body":{"model":"b","n":2,"prompt":"déjà vu"}}`))
	require.NoError(t, err)
	res, err = rt.Call(decode, arg)
	require.NoError(t, err)
	b, err := rt.AppendJSON([]byte("x="), res)
	require.NoError(t, err)
	assert.Equal(t, `x={"model":"b","n":2,"prompt":"déjà vu","auth":false,"calls":2}`, string(b))

	model, err := rt.Get(res, "model")
	require.NoError(t, err)
	assert.Equal(t, "b", model.String())
	missing, err := rt.Get(moejs.Undefined(), "model")
	require.NoError(t, err)
	assert.True(t, missing.IsUndefined())

	echo := mustHook(t, mod, "echo")
	res, err = rt.Call(echo, moejs.String("a"), moejs.Int(1))
	require.NoError(t, err)
	b, err = rt.AppendJSON(nil, res)
	require.NoError(t, err)
	assert.Equal(t, `["a",1]`, string(b))
	b, err = rt.AppendJSON(nil, moejs.Undefined())
	require.NoError(t, err)
	assert.Equal(t, "null", string(b))

	meta, ok := rt.Export("meta")
	require.True(t, ok)
	out, err = rt.ToGo(meta)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"key": "smoke", "version": "1.0.0", "models": []any{"a", "b"}}, out)
	_, ok = rt.Export("nope")
	assert.False(t, ok)
}

func TestHookResolution(t *testing.T) {
	mod, err := moejs.Compile("plugin.js", hostSource)
	require.NoError(t, err)
	rt := newHostRuntime(t, mod)

	_, err = mod.Hook("undeclared")
	require.ErrorIs(t, err, moejs.ErrHookNotFound)

	images := mustHook(t, mod, "protocols", "openai.images", "decodeRequest")
	assert.Equal(t, "protocols.openai.images.decodeRequest", images.Name())
	arg, err := rt.FromGo(map[string]any{"size": "1024x1024"})
	require.NoError(t, err)
	res, err := rt.Call(images, arg)
	require.NoError(t, err)
	b, err := rt.AppendJSON(nil, res)
	require.NoError(t, err)
	assert.Equal(t, `{"kind":"image","size":"1024x1024"}`, string(b))

	res, err = rt.Call(mustHook(t, mod, "protocols", "lazy", "decodeRequest"))
	require.NoError(t, err, "own getters on the path run")
	assert.Equal(t, "lazy", res.String())

	for _, tc := range []struct {
		hook moejs.Hook
		want error
	}{
		{mustHook(t, mod, "protocols", "broken", "decodeRequest"), moejs.ErrNotCallable},
		{mustHook(t, mod, "meta"), moejs.ErrNotCallable},
		{mustHook(t, mod, "protocols", "missing", "decodeRequest"), moejs.ErrHookNotFound},
		{mustHook(t, mod, "protocols", "toString"), moejs.ErrHookNotFound}, // inherited, not own
		{mustHook(t, mod, "native", "error"), moejs.ErrHookNotFound},
		{mustHook(t, mod, "meta", "key", "length"), moejs.ErrHookNotFound},
		{moejs.Hook{}, moejs.ErrHookNotFound},
	} {
		_, err := rt.Call(tc.hook)
		assert.ErrorIs(t, err, tc.want, tc.hook.Name())
		ok, err := rt.Has(tc.hook)
		assert.NoError(t, err, tc.hook.Name())
		assert.False(t, ok, tc.hook.Name())
	}
	ok, err := rt.Has(images)
	require.NoError(t, err)
	assert.True(t, ok)

	throwing := mustHook(t, mod, "protocols", "throwing", "decodeRequest")
	_, err = rt.Has(throwing)
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "getter failed", exc.Message())

	later := mustHook(t, mod, "later")
	ok, err = rt.Has(later)
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = rt.Call(mustHook(t, mod, "replace"))
	require.NoError(t, err)
	res, err = rt.Call(later)
	require.NoError(t, err, "bindings are live")
	assert.Equal(t, "replaced", res.String())

	other, err := moejs.Compile("other.js", hostSource)
	require.NoError(t, err)
	_, err = rt.Call(mustHook(t, other, "decode"))
	assert.ErrorIs(t, err, moejs.ErrHookNotFound, "a hook of another module")
}

func TestErrors(t *testing.T) {
	mod, err := moejs.Compile("plugin.js", hostSource)
	require.NoError(t, err)
	rt := newHostRuntime(t, mod)
	fail := mustHook(t, mod, "fail")

	call := func(kind string) error {
		_, err := rt.Call(fail, moejs.String(kind))
		return err
	}
	for _, tc := range []struct {
		kind, name, message, error string
	}{
		{"string", "", "raw text", "raw text"},
		{"object", "", "", "[object Object]"},
		{"accessor", "Error", "", "Error"},
		{"host", "Error", "sign request: host failure", "Error: sign request: host failure"},
		{"overflow", "RangeError", "Maximum call stack size exceeded", "RangeError: Maximum call stack size exceeded"},
		{"other", "RangeError", "kind other", "RangeError: kind other"},
	} {
		err := call(tc.kind)
		var exc *moejs.Exception
		require.ErrorAs(t, err, &exc, tc.kind)
		assert.Equal(t, tc.name, exc.Name(), tc.kind)
		assert.Equal(t, tc.message, exc.Message(), tc.kind)
		assert.Equal(t, tc.error, exc.Error(), tc.kind)
	}
	assert.ErrorIs(t, call("host"), errHost)
	assert.NotErrorIs(t, call("other"), errHost)

	res, err := rt.Call(fail, moejs.String("caught"))
	require.NoError(t, err)
	assert.Equal(t, "sign request: host failure|Error", res.String())

	stack := rt.StackTrace(func() *moejs.Exception {
		var exc *moejs.Exception
		require.ErrorAs(t, call("other"), &exc)
		return exc
	}())
	assert.True(t, strings.HasPrefix(stack, "RangeError: kind other\n    at fail (plugin.js:"), stack)

	rt.Interrupt("deadline")
	var interrupted *moejs.InterruptedError
	require.ErrorAs(t, call("loop"), &interrupted)
	assert.Equal(t, "deadline", interrupted.Value)
	rt.ClearInterrupt()
	require.ErrorAs(t, call("other"), new(*moejs.Exception), "usable after an interrupt")

	// An interrupt that arrives while nothing runs stops the next Call even
	// when it names a native, which never enters the interpreter.
	hostMod, err := moejs.Compile("host.js", "export const fail = utils.fail;")
	require.NoError(t, err)
	hrt := newHostRuntime(t, hostMod)
	hrt.Interrupt("idle")
	_, err = hrt.Call(mustHook(t, hostMod, "fail"))
	require.ErrorAs(t, err, &interrupted)
	assert.Equal(t, "idle", interrupted.Value)
	hrt.ClearInterrupt()
	_, err = hrt.Call(mustHook(t, hostMod, "fail"))
	assert.ErrorIs(t, err, errHost)
	hrt = moejs.NewRuntime(moejs.Options{})
	hrt.Interrupt("idle")
	loadErr := hrt.Load(mod)
	require.ErrorAs(t, loadErr, &interrupted, "and the next Load")
	hrt.ClearInterrupt()
	for _, name := range []string{"meta", "decode", "later"} {
		_, ok := hrt.Export(name)
		assert.False(t, ok, "%s: the top level never ran", name)
	}
	_, err = hrt.Call(mustHook(t, mod, "decode"))
	assert.Same(t, loadErr, err)
	ok, err := hrt.Has(mustHook(t, mod, "decode"))
	assert.False(t, ok)
	assert.Same(t, loadErr, err)
	// So does one that arrives while Load starts: no export reads as an
	// initialized undefined.
	raceMod, err := moejs.Compile("race.js", "export function f() { return 1; } export const x = 1;")
	require.NoError(t, err)
	for range 300 {
		rrt := moejs.NewRuntime(moejs.Options{})
		var stop atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		started := make(chan struct{})
		go func() {
			defer wg.Done()
			close(started)
			for !stop.Load() {
				rrt.Interrupt("race")
				rrt.ClearInterrupt()
			}
		}()
		<-started
		loadErr = rrt.Load(raceMod)
		stop.Store(true)
		wg.Wait()
		rrt.ClearInterrupt()
		for _, name := range []string{"f", "x"} {
			v, ok := rrt.Export(name)
			require.False(t, ok && v.IsUndefined(), "%s after %v", name, loadErr)
		}
	}

	_, err = moejs.Compile("bad.js", "export const x = ;")
	var se *moejs.SyntaxError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, moejs.SyntaxError{File: "bad.js", Line: 1, Column: 18, Message: se.Message}, *se)
	assert.Equal(t, "bad.js:1:18: SyntaxError: "+se.Message, se.Error())
	imp, err := moejs.Compile("import.js", `import x from "y";`)
	require.NoError(t, err)
	assert.EqualError(t, moejs.NewRuntime(moejs.Options{}).Load(imp), `moejs: module "import.js" imports "y": link it with moejs.Link and a resolver`)

	thrower, err := moejs.Compile("thrower.js", `export const a = 1; throw new Error("top level");`)
	require.NoError(t, err)
	trt := moejs.NewRuntime(moejs.Options{})
	err = trt.Load(thrower)
	require.ErrorAs(t, err, new(*moejs.Exception))
	a, ok := trt.Export("a")
	require.True(t, ok, "bindings initialized before the throw stay readable")
	assert.Equal(t, "1", a.String())
	require.Error(t, trt.Load(thrower))
}

func TestBigIntValues(t *testing.T) {
	mod, err := moejs.Compile("bigint.js", `
export function twice(x) { return [typeof x, x * 2n]; }
export function wrapped() { return Object(7n); }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))

	huge := new(big.Int).Lsh(big.NewInt(1), 100)
	in, err := rt.FromGo(huge)
	require.NoError(t, err)
	res, err := rt.Call(mustHook(t, mod, "twice"), in)
	require.NoError(t, err)
	out, err := rt.ToGo(res)
	require.NoError(t, err)
	assert.Equal(t, []any{"bigint", new(big.Int).Lsh(big.NewInt(1), 101)}, out)
	assert.Equal(t, 0, huge.Cmp(new(big.Int).Lsh(big.NewInt(1), 100)), "FromGo copies")

	res, err = rt.Call(mustHook(t, mod, "wrapped"))
	require.NoError(t, err)
	out, err = rt.ToGo(res)
	require.NoError(t, err)
	assert.Equal(t, big.NewInt(7), out, "a BigInt wrapper exports its primitive")

	null, err := rt.FromGo((*big.Int)(nil))
	require.NoError(t, err)
	assert.True(t, null.IsNull())
	_, err = rt.FromGo(new(big.Int).Lsh(big.NewInt(1), 1<<21))
	require.ErrorAs(t, err, new(*moejs.Exception), "a *big.Int over the size limit")

	_, err = rt.AppendJSON(nil, in)
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "TypeError: Do not know how to serialize a BigInt", exc.Error())
}

func TestByteValues(t *testing.T) {
	mod, err := moejs.Compile("bytes.js", `
export function upper(b) { const u = new Uint8Array(b); for (let i = 0; i < u.length; i++) u[i] &= ~32; return [b instanceof ArrayBuffer, new Uint16Array(b, 0, 1), new DataView(b, 1)]; }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))

	body := []byte("abc")
	in, err := rt.FromGo(body)
	require.NoError(t, err)
	res, err := rt.Call(mustHook(t, mod, "upper"), in)
	require.NoError(t, err)
	assert.Equal(t, "ABC", string(body), "the buffer is the host's bytes")
	out, err := rt.ToGo(res)
	require.NoError(t, err)
	assert.Equal(t, []any{true, []byte("AB"), []byte("BC")}, out)
	out, err = rt.ToGo(in)
	require.NoError(t, err)
	out.([]byte)[0] = 'x'
	assert.Equal(t, "ABC", string(body), "ToGo copies")
}

func TestProxyValues(t *testing.T) {
	mod, err := moejs.Compile("proxy.js", `
export const log = [];
const logged = (target, extra) => new Proxy(target, {
	ownKeys(t) { log.push('ownKeys'); return Reflect.ownKeys(t) },
	get(t, k, r) { log.push('get ' + String(k)); return Reflect.get(t, k, r) },
	getOwnPropertyDescriptor(t, k) { log.push('gopd ' + String(k)); return Reflect.getOwnPropertyDescriptor(t, k) },
	...extra,
});
export const api = logged({ping(x) { return 'pong ' + x }, nested: logged({deep() { return 'deep' }})});
export const hidden = logged({f() {}}, {getOwnPropertyDescriptor() { log.push('hide'); return undefined }});
export const throwing = new Proxy({}, {getOwnPropertyDescriptor() { throw new TypeError('gopd boom') }});
export const revocable = Proxy.revocable({}, {});
export function objects() { return [logged({a: 1, b: [1, 2]}), logged([1, 2, 3], {get(t, k) { return k === 'length' ? 2 : t[k] }})]; }
export function fn() { return new Proxy(function () {}, {}); }
export function drain() { return log.splice(0).join(); }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	drain := mustHook(t, mod, "drain")
	logOf := func() string {
		t.Helper()
		v, err := rt.Call(drain)
		require.NoError(t, err)
		return v.String()
	}

	// A hook path goes through the proxies' getOwnPropertyDescriptor and get.
	res, err := rt.Call(mustHook(t, mod, "api", "ping"), moejs.String("x"))
	require.NoError(t, err)
	assert.Equal(t, "pong x", res.String())
	assert.Equal(t, "gopd ping,get ping", logOf())
	res, err = rt.Call(mustHook(t, mod, "api", "nested", "deep"))
	require.NoError(t, err)
	assert.Equal(t, "deep", res.String())
	assert.Equal(t, "gopd nested,get nested,gopd deep,get deep", logOf())
	_, err = rt.Call(mustHook(t, mod, "hidden", "f"))
	require.ErrorIs(t, err, moejs.ErrHookNotFound)
	assert.Equal(t, "hide", logOf())
	_, err = rt.Call(mustHook(t, mod, "throwing", "f"))
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "TypeError: gopd boom", exc.Error())
	rv, err := rt.Call(mustHook(t, mod, "revocable", "revoke"))
	require.NoError(t, err)
	assert.True(t, rv.IsUndefined())
	_, err = rt.Call(mustHook(t, mod, "revocable", "proxy", "f"))
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "TypeError: Cannot perform 'getOwnPropertyDescriptor' on a proxy that has been revoked", exc.Error())

	// ToGo and AppendJSON run the traps.
	res, err = rt.Call(mustHook(t, mod, "objects"))
	require.NoError(t, err)
	out, err := rt.ToGo(res)
	require.NoError(t, err)
	assert.Equal(t, []any{map[string]any{"a": int64(1), "b": []any{int64(1), int64(2)}}, []any{int64(1), int64(2)}}, out)
	assert.Equal(t, "ownKeys,gopd a,gopd b,get a,get b", logOf())
	b, err := rt.AppendJSON(nil, res)
	require.NoError(t, err)
	assert.Equal(t, `[{"a":1,"b":[1,2]},[1,2]]`, string(b))
	assert.Equal(t, "get toJSON,ownKeys,gopd a,gopd b,get a,get b", logOf())
	res, err = rt.Call(mustHook(t, mod, "fn"))
	require.NoError(t, err)
	out, err = rt.ToGo(res)
	require.NoError(t, err)
	assert.IsType(t, (*moejs.Object)(nil), out, "a callable proxy exports as itself")
	b, err = rt.AppendJSON(nil, res)
	require.NoError(t, err)
	assert.Equal(t, "null", string(b))
}

// Each host entry that runs JavaScript ends as an outermost call does: the
// jobs its getters and proxy traps queued run before it returns, the first
// exception they throw is its error, and an interrupt drops them.
func TestHostEntriesRunTheirJobs(t *testing.T) {
	mod, err := moejs.Compile("entries.js", `
export const log = [];
const job = s => queueMicrotask(() => log.push(s));
const traps = v => new Proxy({}, {
	get(t, k) { job('get ' + String(k)); return v },
	getOwnPropertyDescriptor(t, k) { job('gopd ' + k); if (k !== 'missing') return {value: v, configurable: true, enumerable: true} },
	ownKeys() { job('ownKeys'); return ['a'] },
});
export const fns = traps(() => 1), data = traps(1);
export const outer = {inner: fns, get g() { job('getter'); return {f: () => 2} }};
export const later = new Proxy({}, {get() { return Promise.resolve(1).then(v => v + 1) }});
export const throws = new Proxy({}, {get() { queueMicrotask(() => { throw new Error('trap job') }); return 1 }});
export const loops = new Proxy({}, {get() { job('stale'); stop(); for (;;) {} }});
Object.setPrototypeOf(globalThis, new Proxy(Object.getPrototypeOf(globalThis), {set(t, k, v, r) { job('set ' + k); return Reflect.set(t, k, v, r) }}));
export function drain() { return log.splice(0).join(); }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		rt.Interrupt("stop")
		return moejs.Undefined(), nil
	})))
	require.NoError(t, rt.Load(mod))
	drain := mustHook(t, mod, "drain")
	logOf := func() string {
		t.Helper()
		v, err := rt.Call(drain)
		require.NoError(t, err)
		return v.String()
	}
	export := func(name string) moejs.Value {
		v, ok := rt.Export(name)
		require.True(t, ok)
		return v
	}

	v, err := rt.Get(export("later"), "x")
	require.NoError(t, err)
	state, res, _ := moejs.PromiseResult(v)
	assert.Equal(t, moejs.PromiseFulfilled, state)
	assert.Equal(t, "2", res.String())
	ok, err := rt.Has(mustHook(t, mod, "fns", "f"))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "gopd f,get f", logOf())
	_, err = rt.Call(mustHook(t, mod, "data", "a"))
	require.ErrorIs(t, err, moejs.ErrNotCallable)
	assert.Equal(t, "gopd a,get a", logOf())
	_, err = rt.Call(mustHook(t, mod, "fns", "missing"))
	require.ErrorIs(t, err, moejs.ErrHookNotFound)
	assert.Equal(t, "gopd missing", logOf())
	v, err = rt.Call(mustHook(t, mod, "outer", "inner", "f"))
	require.NoError(t, err)
	assert.Equal(t, "1", v.String())
	assert.Equal(t, "gopd f,get f", logOf(), "a proxy after a data step")
	v, err = rt.Call(mustHook(t, mod, "outer", "g", "f"))
	require.NoError(t, err)
	assert.Equal(t, "2", v.String())
	assert.Equal(t, "getter", logOf())
	_, err = rt.ToGo(export("data"))
	require.NoError(t, err)
	assert.Equal(t, "ownKeys,gopd a,get a", logOf())
	_, err = rt.AppendJSON(nil, export("data"))
	require.NoError(t, err)
	assert.Equal(t, "get toJSON,ownKeys,gopd a,get a", logOf())
	require.NoError(t, rt.SetGlobal("fresh", 1))
	assert.Equal(t, "set fresh", logOf())

	_, err = rt.Get(export("throws"), "x")
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "trap job", exc.Message())
	_, err = rt.Get(export("loops"), "x")
	require.ErrorAs(t, err, new(*moejs.InterruptedError))
	rt.ClearInterrupt()
	assert.Equal(t, "", logOf())
	assert.Equal(t, "", logOf(), "no stale job ran after the first")
}

func TestPromises(t *testing.T) {
	mod, err := moejs.Compile("promise.js", `
export function double(p) { return p.then(v => v * 2); }
export function reject() { Promise.reject("lost"); return Promise.reject("later"); }
export function handle(p) { p.catch(() => {}); }
export function throwLater(p) { p.then(() => queueMicrotask(() => { throw new Error("tick"); })); }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	var ops []string
	var tracked []*moejs.Object
	rt.SetPromiseRejectionTracker(func(p moejs.Value, op moejs.PromiseRejectionOperation) {
		_, reason, _ := moejs.PromiseResult(p)
		ops = append(ops, fmt.Sprint(op, " ", reason.String()))
		tracked = append(tracked, p.AsObject())
	})
	require.NoError(t, rt.Load(mod))

	// A host promise settled after the Call returned runs its reactions
	// before resolve returns.
	p, resolve, reject := rt.NewPromise()
	res, err := rt.Call(mustHook(t, mod, "double"), p)
	require.NoError(t, err)
	state, _, ok := moejs.PromiseResult(res)
	require.True(t, ok)
	assert.Equal(t, moejs.PromisePending, state)
	require.NoError(t, resolve(moejs.Int(21)))
	require.NoError(t, reject(moejs.Int(0)), "the first settlement decides")
	state, v, _ := moejs.PromiseResult(res)
	assert.Equal(t, moejs.PromiseFulfilled, state)
	assert.Equal(t, "42", v.String())
	state, v, _ = moejs.PromiseResult(p)
	assert.Equal(t, moejs.PromiseFulfilled, state)
	assert.Equal(t, "21", v.String())
	_, _, ok = moejs.PromiseResult(moejs.Int(1))
	assert.False(t, ok)

	res, err = rt.Call(mustHook(t, mod, "reject"))
	require.NoError(t, err)
	_, err = rt.Call(mustHook(t, mod, "handle"), res)
	require.NoError(t, err)
	assert.Equal(t, []string{"0 lost", "0 later", "1 later"}, ops)
	assert.Same(t, res.AsObject(), tracked[1])
	assert.Same(t, tracked[1], tracked[2], "Reject and Handle name the same promise")

	// The first exception a job throws is the error of the settlement that
	// ran it.
	p, resolve, _ = rt.NewPromise()
	_, err = rt.Call(mustHook(t, mod, "throwLater"), p)
	require.NoError(t, err)
	var exc *moejs.Exception
	require.ErrorAs(t, resolve(moejs.Undefined()), &exc)
	assert.Equal(t, "tick", exc.Message())

	rt.SetPromiseRejectionTracker(nil)
	_, err = rt.Call(mustHook(t, mod, "reject"))
	require.NoError(t, err)
	assert.Len(t, ops, 3)
}

// The jobs a module's top level queued run after Load recorded the module:
// they find its exports and hooks, and a Load from them or from the top
// level is refused instead of running the top level again.
func TestLoadRunsJobsAfterTheModule(t *testing.T) {
	mod, err := moejs.Compile("jobs.js", `
export let x = 1;
export function get() { return x; }
host();
Promise.resolve().then(() => host());
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	var seen []string
	require.NoError(t, rt.SetGlobal("host", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		x, ok := rt.Export("x")
		v, err := rt.Call(mustHook(t, mod, "get"))
		seen = append(seen, fmt.Sprint(ok, " ", x.String(), " ", v.String(), " ", err, " ", rt.Load(mod) != nil))
		return moejs.Undefined(), nil
	})))
	require.NoError(t, rt.Load(mod))
	assert.Equal(t, []string{
		"false undefined undefined " + moejs.ErrHookNotFound.Error() + " true",
		"true 1 1 <nil> true",
	}, seen)
}

// TestHooksOfAFailedModule checks the exported functions of a module whose
// top level did not finish: Call and Has refuse them with Load's error once
// the top level threw or was interrupted, and so does Export, which still
// reads the other bindings initialized before the failure; while it awaits
// they throw a ReferenceError for the bindings it has not initialized yet,
// which Export does not find either.
func TestHooksOfAFailedModule(t *testing.T) {
	for _, src := range []string{
		`export const y = 2; export function f() { return px.a; } queueMicrotask(host); throw new Error("top"); const px = { a: 1 }; export const z = 3;`,
		`export const y = 2; export function f() { return px.a; } queueMicrotask(host); throw new Error("top"); const px = { a: 1 }; await 0; export const z = 3;`,
		`export const y = 2; export function f() { return px.a; } queueMicrotask(host); stop(); for (;;); const px = { a: 1 }; export const z = 3;`,
	} {
		mod, err := moejs.Compile("failed.js", src)
		require.NoError(t, err)
		rt := moejs.NewRuntime(moejs.Options{})
		f := mustHook(t, mod, "f")
		var inJob error
		require.NoError(t, rt.SetGlobal("host", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			_, inJob = rt.Call(f)
			return moejs.Undefined(), nil
		})))
		require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			rt.Interrupt("stop")
			return moejs.Undefined(), nil
		})))
		loadErr := rt.Load(mod)
		require.Error(t, loadErr, src)
		rt.ClearInterrupt()
		_, err = rt.Call(f)
		assert.Same(t, loadErr, err, src)
		ok, err := rt.Has(f)
		assert.False(t, ok, src)
		assert.Same(t, loadErr, err, src)
		if _, isExc := loadErr.(*moejs.Exception); isExc {
			assert.Same(t, loadErr, inJob, src)
		}
		_, ok = rt.Export("f")
		assert.False(t, ok, "its functions could read px uninitialized")
		y, ok := rt.Export("y")
		assert.True(t, ok, "its other bindings stay readable")
		assert.Equal(t, 2.0, y.AsNumber(), src)
		_, ok = rt.Export("z")
		assert.False(t, ok, "but not those left uninitialized")
	}

	mod, err := moejs.Compile("tla.js", `
export function readLate() { return late; }
export function w() { late = 5; return late; }
export function mk() { return new K(); }
export function wc() { limit = 5; }
export const early = 1;
await hostPromise();
export let late = 1;
class K {}
const limit = 1;
export const cfg = { key: "k" };
export class C {}
export default 42;
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	p, resolve, _ := rt.NewPromise()
	require.NoError(t, rt.SetGlobal("hostPromise", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return p, nil
	})))
	require.ErrorIs(t, rt.Load(mod), moejs.ErrModulePending)
	for _, name := range []string{"readLate", "w", "mk", "wc"} {
		_, err := rt.Call(mustHook(t, mod, name))
		var exc *moejs.Exception
		require.ErrorAs(t, err, &exc, name)
		assert.Equal(t, "ReferenceError", exc.Name(), name)
	}
	exported := func(name string) bool { _, ok := rt.Export(name); return ok }
	assert.True(t, exported("early"))
	for _, name := range []string{"late", "cfg", "C", "default"} {
		assert.False(t, exported(name), "%s is not initialized yet", name)
	}
	require.NoError(t, resolve(moejs.Undefined()))
	res, err := rt.Call(mustHook(t, mod, "readLate"))
	require.NoError(t, err)
	assert.Equal(t, "1", res.String())
	for _, name := range []string{"late", "cfg", "C", "default"} {
		assert.True(t, exported(name), name)
	}
	_, err = rt.Call(mustHook(t, mod, "wc"))
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "TypeError", exc.Name(), "a const is assignable to nothing once initialized")
}

func TestPanicBecomesInternalError(t *testing.T) {
	mod, err := moejs.Compile("p.js", `export function f(x) { return [1, 2].map(() => boom(x)).length; }
export let count = 0;
export function g() { queueMicrotask(() => { count++; }); boom("x"); }`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("boom", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if moejs.Arg(args, 0).IsString() {
			panic("host bug")
		}
		return moejs.Undefined(), nil
	})))
	require.NoError(t, rt.Load(mod))
	f := mustHook(t, mod, "f")
	depth := rt.Realm().CallDepth()

	_, err = rt.Call(f, moejs.String("x"))
	var ie *moejs.InternalError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "host bug", ie.Value)
	assert.Equal(t, depth, rt.Realm().CallDepth())
	res, err := rt.Call(f, moejs.Int(1))
	require.NoError(t, err)
	assert.Equal(t, "2", res.String())

	// The panic drops the job g queued before it: it does not run at the
	// end of the next call.
	_, err = rt.Call(mustHook(t, mod, "g"))
	require.ErrorAs(t, err, &ie)
	_, err = rt.Call(f, moejs.Int(1))
	require.NoError(t, err)
	count, _ := rt.Export("count")
	assert.Equal(t, "0", count.String(), "no stale job ran")
}

// TestPanicDropsJobs checks the jobs left by a Go panic out of a job: the
// jobs after it are dropped, not run in the next call, while a panic
// recovered by a nested Call leaves the outer call's jobs to its end, and a
// module top level that a dropped job would have resumed does not resume.
func TestPanicDropsJobs(t *testing.T) {
	mod, err := moejs.Compile("p.js", `export let log = "";
export function arm() { queueMicrotask(() => { log += "1"; boom(); }); queueMicrotask(() => { log += "2"; }); queueMicrotask(() => { log += "3"; }); }
export function next() { log += "n"; }
export function outer() { queueMicrotask(() => { log += "o"; }); nested(); log += "a"; }`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("boom", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		panic("job bug")
	})))
	var nestedErr error
	require.NoError(t, rt.SetGlobal("nested", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		_, nestedErr = rt.Call(mustHook(t, mod, "arm"))
		return moejs.Undefined(), nil
	})))
	require.NoError(t, rt.Load(mod))
	log := func() string { v, _ := rt.Export("log"); return v.String() }

	_, err = rt.Call(mustHook(t, mod, "arm"))
	var ie *moejs.InternalError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "1", log())
	_, err = rt.Call(mustHook(t, mod, "next"))
	require.NoError(t, err)
	assert.Equal(t, "1n", log(), "the jobs after the panic do not run in the next call")

	// The nested Call queues its jobs in the outer call, whose end runs
	// them: the panic is the outer call's error, the jobs after it still
	// queued are dropped.
	_, err = rt.Call(mustHook(t, mod, "outer"))
	require.NoError(t, nestedErr)
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "1nao1", log())
	_, err = rt.Call(mustHook(t, mod, "next"))
	require.NoError(t, err)
	assert.Equal(t, "1nao1n", log())

	// A top level awaiting past a job that panics does not resume in a
	// later call, alone or through its graph (import.meta).
	for _, src := range []string{"", "import.meta;\n"} {
		tla, err := moejs.Compile("tla.js", src+`export let n = 0;
export function get() { return n; }
queueMicrotask(() => boom());
await null;
n = 1;`)
		require.NoError(t, err)
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.SetGlobal("boom", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			panic("tla bug")
		})))
		err = rt.Load(tla)
		require.ErrorAs(t, err, &ie, src)
		assert.Equal(t, "tla bug", ie.Value, src)
		for range 2 {
			res, err := rt.Call(mustHook(t, tla, "get"))
			require.NoError(t, err, src)
			assert.Equal(t, "0", res.String(), src)
		}
		v, _ := rt.Export("n")
		assert.Equal(t, "0", v.String(), "%sthe top level does not resume in a later call", src)
	}
}

// nilMapWrite is a host function with a Go bug: it writes to a nil map.
func nilMapWrite(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
	var m map[string]int
	m["x"] = 1
	return moejs.Undefined(), nil
}

// TestInternalErrorDetails checks what an *InternalError carries: the panic
// value in Error, the value as the error Unwrap returns when it is one, and
// the goroutine stack; and that a JavaScript try statement neither catches
// the panic nor keeps it from reaching the host, also when the host
// function panics from inside a nested call.
func TestInternalErrorDetails(t *testing.T) {
	mod, err := moejs.Compile("ie.js", `
function deep(n) { return n === 0 ? bug() : deep(n - 1); }
export function direct() { return deep(20); }
export function caught() { try { return deep(20); } catch (e) { return "caught " + e; } }
export function nested() { try { return callback(() => deep(5)); } catch (e) { return "caught " + e; } }
export function ok() { return "ok"; }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("bug", moejs.NativeFunc(nilMapWrite)))
	require.NoError(t, rt.SetGlobal("callback", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		v, err := r.Call(moejs.Arg(args, 0), moejs.Undefined(), nil)
		if err != nil {
			panic(err)
		}
		return v, nil
	})))
	require.NoError(t, rt.Load(mod))
	depth := rt.Realm().CallDepth()
	for _, name := range []string{"direct", "caught", "nested"} {
		res, err := rt.Call(mustHook(t, mod, name))
		var ie *moejs.InternalError
		require.ErrorAs(t, err, &ie, "%s returned %v", name, res)
		var re runtime.Error
		require.ErrorAs(t, err, &re, name)
		assert.Equal(t, "assignment to entry in nil map", re.Error(), name)
		assert.Equal(t, "moejs: internal error: assignment to entry in nil map", ie.Error(), name)
		assert.NotEmpty(t, ie.Stack, name)
		var exc *moejs.Exception
		assert.False(t, errors.As(err, &exc), name)
		assert.Equal(t, depth, rt.Realm().CallDepth(), name)
	}
	res, err := rt.Call(mustHook(t, mod, "ok"))
	require.NoError(t, err)
	assert.Equal(t, "ok", res.String())
	assert.Nil(t, (&moejs.InternalError{Value: "text"}).Unwrap(), "a panic value that is not an error unwraps to nil")
}

// TestCallArgStackReentrancy checks the arguments Call stacks for the
// engine under rest parameters and reentrant calls: a rest array a function
// keeps is not overwritten by a later call's arguments, and a host function
// that calls again while an outer call's arguments are on the stack leaves
// them intact.
func TestCallArgStackReentrancy(t *testing.T) {
	mod, err := moejs.Compile("args.js", `
export const saved = [];
export function keep(...r) { saved.push(r); return r; }
export function nested(f, ...r) { const before = r.slice(); f(); return [before, r]; }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	keep := mustHook(t, mod, "keep")
	toGo := func(v moejs.Value) any {
		t.Helper()
		out, err := rt.ToGo(v)
		require.NoError(t, err)
		return out
	}
	first, err := rt.Call(keep, moejs.Int(1), moejs.Int(2))
	require.NoError(t, err)
	_, err = rt.Call(keep, moejs.String("a"), moejs.String("b"), moejs.String("c"))
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1), int64(2)}, toGo(first))
	saved, _ := rt.Export("saved")
	assert.Equal(t, []any{[]any{int64(1), int64(2)}, []any{"a", "b", "c"}}, toGo(saved))

	f := rt.Function("f", 0, func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		for i := range 100 {
			n := moejs.Int(int64(i))
			if _, err := rt.Call(keep, n, n, n, n, n); err != nil {
				return moejs.Undefined(), err
			}
		}
		return moejs.Undefined(), nil
	})
	res, err := rt.Call(mustHook(t, mod, "nested"), f, moejs.String("x"), moejs.String("y"))
	require.NoError(t, err)
	assert.Equal(t, []any{[]any{"x", "y"}, []any{"x", "y"}}, toGo(res))
}

// TestNativeHookArgsAfterReentry checks that a native function exported as a
// hook still reads its own arguments after it re-enters Call with more
// arguments than it received.
func TestNativeHookArgsAfterReentry(t *testing.T) {
	mod, err := moejs.Compile("native.js", `export function keep(...r) { return r; }
export const h = hostH;`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	var keep moejs.Hook
	require.NoError(t, rt.SetGlobal("hostH", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		for i := range 10 {
			n := moejs.Int(int64(i))
			if _, err := rt.Call(keep, n, n, n, n, n); err != nil {
				return moejs.Undefined(), err
			}
		}
		return moejs.String(moejs.Arg(args, 0).String() + moejs.Arg(args, 1).String()), nil
	})))
	require.NoError(t, rt.Load(mod))
	keep = mustHook(t, mod, "keep")
	big := make([]moejs.Value, 16)
	for i := range big {
		big[i] = moejs.Int(int64(i))
	}
	_, err = rt.Call(keep, big...)
	require.NoError(t, err)
	res, err := rt.Call(mustHook(t, mod, "h"), moejs.String("x"), moejs.String("y"))
	require.NoError(t, err)
	assert.Equal(t, "xy", res.String())
}

// TestFunctionNameLength checks the name and length a host function shows
// JavaScript, a negative length being 0.
func TestFunctionNameLength(t *testing.T) {
	mod, err := moejs.Compile("fn.js", `export function show(f) { return f.name + "/" + f.length; }`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	noop := func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) { return moejs.Undefined(), nil }
	for _, c := range []struct {
		length int
		want   string
	}{{2, "sign/2"}, {0, "sign/0"}, {-3, "sign/0"}} {
		res, err := rt.Call(mustHook(t, mod, "show"), rt.Function("sign", c.length, noop))
		require.NoError(t, err)
		assert.Equal(t, c.want, res.String(), "length %d", c.length)
	}
}

// TestCompileIgnoresSourceMappingURL checks that a sourceMappingURL comment
// is inert: Compile reads no file for it and the module loads and runs.
func TestCompileIgnoresSourceMappingURL(t *testing.T) {
	for _, src := range []string{
		"export function run() { return 1; }\n//# sourceMappingURL=/etc/passwd",
		"export function run() { return 1; }\n//# sourceMappingURL=/nonexistent/leak-probe.map\n",
		"export function run() { return 1; }\n//@ sourceMappingURL=data:application/json;base64,e30=\n",
	} {
		mod, err := moejs.Compile("smap.js", src)
		require.NoError(t, err, src)
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.Load(mod))
		res, err := rt.Call(mustHook(t, mod, "run"))
		require.NoError(t, err)
		assert.Equal(t, "1", res.String())
	}
}

func TestModuleSharedAcrossRuntimes(t *testing.T) {
	mod, err := moejs.Compile("plugin.js", hostSource)
	require.NoError(t, err)
	decode := mustHook(t, mod, "decode")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			rt := newHostRuntime(t, mod)
			for range 50 {
				arg, err := rt.ParseJSON([]byte(`{"body":{"model":"a","prompt":"x"}}`))
				if err != nil {
					errs[i] = err
					return
				}
				if _, err := rt.Call(decode, arg); err != nil {
					errs[i] = err
					return
				}
			}
			res, err := rt.Call(decode, moejs.String("not an object"))
			if res.IsUndefined() && err != nil && strings.Contains(err.Error(), "TypeError") {
				return
			}
			errs[i] = fmt.Errorf("want a TypeError, got %v", err)
		})
	}
	wg.Wait()
	for _, err := range errs {
		assert.NoError(t, err)
	}
}

// TestModuleSharedAcrossRuntimesInterrupted runs one compiled module in 16
// runtimes at once, each calling a hook 100 times while other goroutines
// interrupt some of the calls: an interrupted call returns an
// *InterruptedError with the interrupt's value, a hook that throws returns
// its exception, and every other call returns its own result. Run with
// -race.
func TestModuleSharedAcrossRuntimesInterrupted(t *testing.T) {
	mod, err := moejs.Compile("conc.js", `
const cache = {};
export function hook(ctx) {
	const parts = [];
	for (const k of Object.keys(ctx.headers).sort()) parts.push(k + "=" + ctx.headers[k]);
	const sig = sign(parts.join("&"), ctx.secret);
	cache[ctx.id] = sig;
	const n = ctx.items.reduce((s, x) => s + x.v, 0);
	if (ctx.fail) throw new Error("hook failed " + ctx.id);
	return { id: ctx.id, sig, n, json: JSON.parse(JSON.stringify(ctx.nested)), cached: Object.keys(cache).length };
}`)
	require.NoError(t, err)
	hook := mustHook(t, mod, "hook")
	const runtimes, calls = 16, 100
	errs := make([]error, runtimes)
	var interrupted atomic.Int64
	var wg sync.WaitGroup
	for g := range runtimes {
		wg.Go(func() {
			rt := moejs.NewRuntime(moejs.Options{})
			sign := moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
				return moejs.String(moejs.Arg(args, 1).String() + "(" + moejs.Arg(args, 0).String() + ")"), nil
			})
			if errs[g] = rt.SetGlobal("sign", sign); errs[g] != nil {
				return
			}
			if errs[g] = rt.Load(mod); errs[g] != nil {
				return
			}
			for i := range calls {
				id := fmt.Sprintf("g%d-%d", g, i)
				arg, err := rt.FromGo(map[string]any{
					"id": id, "secret": "s", "fail": i%25 == 24,
					"headers": map[string]any{"b": "2", "a": "1"},
					"items":   []any{map[string]any{"v": 1}, map[string]any{"v": 2}, map[string]any{"v": i}},
					"nested":  map[string]any{"k": []any{1, "x", nil, true, 1.5}},
				})
				if err != nil {
					errs[g] = err
					return
				}
				var watcher chan struct{}
				if g%4 == 0 && i%10 == 5 {
					watcher = make(chan struct{})
					go func() { rt.Interrupt("watch"); close(watcher) }()
				}
				res, err := rt.Call(hook, arg)
				if watcher != nil {
					<-watcher
					rt.ClearInterrupt()
				}
				var ie *moejs.InterruptedError
				var exc *moejs.Exception
				switch {
				case errors.As(err, &ie):
					interrupted.Add(1)
					if ie.Value != "watch" {
						errs[g] = fmt.Errorf("%s: interrupt value %v", id, ie.Value)
						return
					}
					continue
				case i%25 == 24:
					if !errors.As(err, &exc) || exc.Message() != "hook failed "+id {
						errs[g] = fmt.Errorf("%s: want the hook's failure, got %v", id, err)
						return
					}
					continue
				case err != nil:
					errs[g] = fmt.Errorf("%s: %w", id, err)
					return
				}
				out, err := rt.ToGo(res)
				if err != nil {
					errs[g] = err
					return
				}
				m, _ := out.(map[string]any)
				if c, ok := m["cached"].(int64); !ok || c < 1 || c > int64(i+1) {
					errs[g] = fmt.Errorf("%s: cached %v", id, m["cached"])
					return
				}
				delete(m, "cached")
				want := map[string]any{"id": id, "sig": "s(a=1&b=2)", "n": int64(3 + i),
					"json": map[string]any{"k": []any{int64(1), "x", nil, true, 1.5}}}
				if !assert.ObjectsAreEqual(want, m) {
					errs[g] = fmt.Errorf("%s: got %#v", id, m)
					return
				}
			}
		})
	}
	wg.Wait()
	for g, err := range errs {
		assert.NoError(t, err, "runtime %d", g)
	}
	t.Logf("interrupted calls: %d", interrupted.Load())
}

func BenchmarkCall(b *testing.B) {
	mod, err := moejs.Compile("plugin.js", hostSource)
	require.NoError(b, err)
	rt := newHostRuntime(b, mod)
	images := mustHook(b, mod, "protocols", "openai.images", "decodeRequest")
	arg, err := rt.FromGo(map[string]any{"size": "1024x1024"})
	require.NoError(b, err)
	var buf []byte
	b.ReportAllocs()
	for b.Loop() {
		res, err := rt.Call(images, arg)
		if err != nil {
			b.Fatal(err)
		}
		if buf, err = rt.AppendJSON(buf[:0], res); err != nil {
			b.Fatal(err)
		}
	}
}

// TestAppendJSONDstDuringCode checks that a host function JavaScript calls
// during AppendJSON (from a toJSON here) may append to the slice passed as
// dst, or run another AppendJSON with it: the output moves out of dst's
// spare capacity before any code runs, and is appended to dst at the end.
func TestAppendJSONDstDuringCode(t *testing.T) {
	mod, err := moejs.Compile("a.js", `
export function logs() { return {x: 1, y: {toJSON() { host.log("hello"); return "Y"; }}, z: [1, 2, 3]}; }
export function nests() { return {x: 1, y: {toJSON() { host.nested({q: "nested"}); return "Y"; }}, z: [1, 2, 3]}; }`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	var dst []byte
	var logged string
	require.NoError(t, rt.SetGlobal("host", map[string]any{
		"log": moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			logged = string(append(append(dst, "LOG:"...), args[0].String()...))
			return moejs.Undefined(), nil
		}),
		"nested": moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			out, err := rt.AppendJSON(dst, args[0])
			logged = string(out)
			return moejs.Undefined(), err
		}),
	}))
	require.NoError(t, rt.Load(mod))
	for hook, want := range map[string]string{"logs": "LOG:hello", "nests": `{"q":"nested"}`} {
		res, err := rt.Call(mustHook(t, mod, hook))
		require.NoError(t, err)
		for _, pre := range []string{"", "pre:"} {
			dst = append(make([]byte, 0, 4096), pre...)
			out, err := rt.AppendJSON(dst, res)
			require.NoError(t, err)
			require.Equal(t, pre+`{"x":1,"y":"Y","z":[1,2,3]}`, string(out), hook)
			require.Equal(t, pre+want, logged, hook)
		}
	}
}

// TestAppendJSONPanicReturnsDst checks a host function that panics during
// AppendJSON, called by a toJSON inside the engine's call or by a job at
// the release: the error is the InternalError and the slice returned is
// dst, not nil or the output the job could have overwritten; the runtime
// then serializes the next value.
func TestAppendJSONPanicReturnsDst(t *testing.T) {
	mod, err := moejs.Compile("p.js", `
export function inCall() { return {s: {toJSON() { return host.boom(); }}, n: 2}; }
export function inJob() { return {s: {toJSON() { queueMicrotask(() => host.boom()); return "S"; }}, n: 2}; }
export function plain() { return {n: 2}; }`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("host", map[string]any{
		"boom": moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) { panic("host panic") }),
	}))
	require.NoError(t, rt.Load(mod))
	for _, hook := range []string{"inCall", "inJob"} {
		res, err := rt.Call(mustHook(t, mod, hook))
		require.NoError(t, err)
		dst := append(make([]byte, 0, 4096), "data: "...)
		out, err := rt.AppendJSON(dst, res)
		var ie *moejs.InternalError
		require.ErrorAs(t, err, &ie, hook)
		require.Equal(t, "data: ", string(out), hook)
		res, err = rt.Call(mustHook(t, mod, "plain"))
		require.NoError(t, err, hook)
		out, err = rt.AppendJSON(dst, res)
		require.NoError(t, err, hook)
		require.Equal(t, `data: {"n":2}`, string(out), hook)
	}
}

// TestAppendJSONJobsUseDst checks the jobs JavaScript queues during
// AppendJSON: they run before AppendJSON returns, after the output was
// written, and one that appends to dst (a host's event stream buffer, its
// prefix kept) leaves the output intact. A toJSON and a getter queue it.
func TestAppendJSONJobsUseDst(t *testing.T) {
	mod, err := moejs.Compile("j.js", `
export function viaToJSON() {
  return {s: {toJSON() { Promise.resolve().then(() => host.late()); return "S"; }}, n: 2, tail: "end"};
}
export function viaGetter() {
  return {get s() { Promise.resolve().then(() => host.late()); return "S"; }, n: 2, tail: "end"};
}`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	var dst []byte
	var wrote string
	require.NoError(t, rt.SetGlobal("host", map[string]any{
		"late": moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			ev, err := rt.FromGo(map[string]any{"event": "late"})
			if err != nil {
				return moejs.Undefined(), err
			}
			out, err := rt.AppendJSON(dst, ev)
			wrote = string(out)
			return moejs.Undefined(), err
		}),
	}))
	require.NoError(t, rt.Load(mod))
	for _, hook := range []string{"viaToJSON", "viaGetter"} {
		res, err := rt.Call(mustHook(t, mod, hook))
		require.NoError(t, err)
		wrote = ""
		dst = append(make([]byte, 0, 4096), "data: "...)
		out, err := rt.AppendJSON(dst, res)
		require.NoError(t, err)
		require.Equal(t, `data: {"s":"S","n":2,"tail":"end"}`, string(out), hook)
		require.Equal(t, `data: {"event":"late"}`, wrote, hook)
	}
}

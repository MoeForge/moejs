package moejs_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const strictJSONSource = `
export function value(name) {
	switch (name) {
	case "lossy": return {a: undefined, b: () => 1, c: Symbol("c"), d: [undefined, NaN, Infinity, -Infinity, () => 1, Symbol()], e: -0};
	case "messages": return {messages: [{content: "x"}, {content: "y"}, {}, {content: undefined}]};
	case "element": return {a: [1, () => 1]};
	case "symbol": return {s: Symbol("s")};
	case "nan": return {n: NaN};
	case "infinity": return [1, Infinity];
	case "-infinity": return {deep: {list: [[-Infinity]]}};
	case "undefined": return undefined;
	case "function": return function f() {};
	case "symbol itself": return Symbol();
	case "NaN itself": return NaN;
	case "quoted": return {"a b": {"x-y": undefined}};
	case "index key": return {1: {2: () => 1}};
	case "toJSON": return {d: {toJSON() { return undefined; }}};
	case "toJSON value": return {d: {toJSON() { return NaN; }}};
	case "proxy": return new Proxy({a: 1, b: undefined}, {});
	case "callable proxy": return [new Proxy(function () {}, {})];
	case "getter": return {get g() { return Symbol(); }};
	case "zero": return {z: -0, list: [-0]};
	case "fine": return {a: [1, "two", null, true, {b: 2.5}], c: ""};
	}
}
`

// TestAppendJSONStrict: AppendJSONWith's Strict fails on every value
// AppendJSON writes otherwise or drops, naming its path, and leaves dst as
// it was; without it the output is AppendJSON's.
func TestAppendJSONStrict(t *testing.T) {
	mod, err := moejs.Compile("strict.js", strictJSONSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	value := func(name string) moejs.Value {
		t.Helper()
		v, err := rt.Call(mustHook(t, mod, "value"), moejs.String(name))
		require.NoError(t, err)
		return v
	}
	strict := moejs.JSONOptions{Strict: true}

	lossy := value("lossy")
	out, err := rt.AppendJSON(nil, lossy)
	require.NoError(t, err)
	assert.Equal(t, `{"d":[null,null,null,null,null,null],"e":0}`, string(out))
	out, err = rt.AppendJSONWith(nil, lossy, moejs.JSONOptions{})
	require.NoError(t, err)
	assert.Equal(t, `{"d":[null,null,null,null,null,null],"e":0}`, string(out), "Strict unset is AppendJSON")

	for _, c := range []struct {
		name, path, value string
		omitted           bool
	}{
		{"lossy", "$.a", "undefined", true},
		{"messages", "$.messages[3].content", "undefined", true},
		{"element", "$.a[1]", "a function", false},
		{"symbol", "$.s", "a symbol", true},
		{"nan", "$.n", "NaN", false},
		{"infinity", "$[1]", "Infinity", false},
		{"-infinity", "$.deep.list[0][0]", "-Infinity", false},
		{"undefined", "$", "undefined", false},
		{"function", "$", "a function", false},
		{"symbol itself", "$", "a symbol", false},
		{"NaN itself", "$", "NaN", false},
		{"quoted", `$["a b"]["x-y"]`, "undefined", true},
		{"index key", `$["1"]["2"]`, "a function", true},
		{"toJSON", "$.d", "undefined", true},
		{"toJSON value", "$.d", "NaN", false},
		{"proxy", "$.b", "undefined", true},
		{"callable proxy", "$[0]", "a function", false},
		{"getter", "$.g", "a symbol", true},
	} {
		dst := []byte("kept")
		out, err := rt.AppendJSONWith(dst, value(c.name), strict)
		var le *moejs.LossyJSONError
		require.True(t, errors.As(err, &le), "%s: %v", c.name, err)
		assert.Equal(t, moejs.LossyJSONError{Path: c.path, Value: c.value, Omitted: c.omitted}, *le, c.name)
		assert.Equal(t, "kept", string(out), c.name)
	}
	_, err = rt.AppendJSONWith(nil, value("messages"), strict)
	assert.EqualError(t, err, "moejs: $.messages[3].content is undefined, which JSON leaves out")
	_, err = rt.AppendJSONWith(nil, value("infinity"), strict)
	assert.EqualError(t, err, "moejs: $[1] is Infinity, which JSON writes as null")

	for _, name := range []string{"zero", "fine"} {
		want, err := rt.AppendJSON(nil, value(name))
		require.NoError(t, err)
		got, err := rt.AppendJSONWith([]byte("x"), value(name), strict)
		require.NoError(t, err, name)
		assert.Equal(t, "x"+string(want), string(got), name)
	}

	// A Go map or slice FromGo converted is checked as JavaScript sees it.
	host, err := rt.FromGo(map[string]any{"ok": []any{1.5, "s"}, "bad": []any{map[string]any{"x": math.Inf(1)}}})
	require.NoError(t, err)
	out, err = rt.AppendJSON(nil, host)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":[1.5,"s"],"bad":[{"x":null}]}`, string(out))
	_, err = rt.AppendJSONWith(nil, host, strict)
	assert.EqualError(t, err, "moejs: $.bad[0].x is Infinity, which JSON writes as null")
	good, err := rt.FromGo(map[string]any{"ok": []any{1.5, "s", nil}})
	require.NoError(t, err)
	out, err = rt.AppendJSONWith(nil, good, strict)
	require.NoError(t, err)
	assert.Equal(t, `{"ok":[1.5,"s",null]}`, string(out))
}

const strictPathsSource = `
export function value(name) {
	const p = new Proxy({a: 1, b: 2}, {get(t, k) { return k === "b" ? undefined : t[k]; }});
	switch (name) {
	case "nested": return {messages: [{content: [1, {x: [0, 0, {"y z": [NaN]}]}]}]};
	case "quote key": return {'a"b': {"\\u00e9\n": undefined}};
	case "unicode key": return {"héllo": {"日本": () => 1}};
	case "empty key": return {"": undefined};
	case "dollar key": return {$a_1: {_: undefined}};
	case "hole": return [1, , 3];
	case "proxy get": return {p};
	case "proxy array": return new Proxy([1, undefined], {});
	case "toJSON root": return {toJSON() { return undefined; }};
	case "toJSON nested": return {a: {toJSON() { return {b: [undefined]}; }}};
	case "toJSON key": return {a: {toJSON(k) { return k === "a" ? Symbol() : 1; }}};
	case "getter throws": return {a: 1, get b() { throw new TypeError("boom"); }};
	case "cycle": { const o = {a: {}}; o.a.o = o; return o; }
	case "cycle after loss": { const o = {u: undefined}; o.self = o; return o; }
	case "map": return {m: new Map([[1, 2]]), s: new Set([1])};
	case "set": return [new Set([1])];
	case "weak": return {w: [new WeakMap(), new WeakSet(), new WeakRef({})]};
	case "promise": return {p: Promise.resolve(1)};
	case "regexp": return {r: /a+/g};
	case "error": return {e: new TypeError("x")};
	case "error subclass": { class E extends Error {} return {list: [1, new E("x")]}; }
	case "buffers": return {b: new ArrayBuffer(8)};
	case "shared buffer": return {b: new SharedArrayBuffer(8)};
	case "data view": return {v: new DataView(new ArrayBuffer(8))};
	case "opaque root": return new Map();
	case "opaque with own data": { const m = new Map(); m.size2 = 1; return {m}; }
	case "opaque with toJSON": { const m = new Map([[1, 2]]); m.toJSON = function () { return [...this]; }; return {m}; }
	case "typed array": return {t: new Uint8Array([1, 2])};
	case "boxed nan": return {n: new Number(NaN)};
	case "boxed inf": return [Object(Infinity)];
	case "date": return {d: new Date(NaN)};
	case "bigint": return {b: 1n};
	case "neg zero": return {z: -0, a: [-0], n: new Number(-0)};
	case "deep loss": { let v = [undefined]; for (let i = 0; i < 9000; i++) v = [v]; return v; }
	case "fine": return {a: [1, "two", null, true, {b: 2.5, c: [[], {}]}], "x y": "", "\u2028": 1e21, n: -1.5e-7};
	}
}
export function stringify(v) { return JSON.stringify(v); }
`

// TestAppendJSONStrictPaths: paths, values and the errors that are
// not losses, and the objects whose data JSON does not see: a Map, Set,
// WeakMap, WeakSet, WeakRef, Promise, RegExp, Error, ArrayBuffer,
// SharedArrayBuffer or DataView fails unless a toJSON method turns it
// into data.
func TestAppendJSONStrictPaths(t *testing.T) {
	mod, err := moejs.Compile("strict.js", strictPathsSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	value := func(name string) moejs.Value {
		v, err := rt.Call(mustHook(t, mod, "value"), moejs.String(name))
		require.NoError(t, err)
		return v
	}
	loss := func(name, path, what string, omitted bool) {
		t.Helper()
		dst := []byte("keep")
		out, err := rt.AppendJSONWith(dst, value(name), moejs.JSONOptions{Strict: true})
		var le *moejs.LossyJSONError
		if assert.ErrorAs(t, err, &le, name) {
			assert.Equal(t, moejs.LossyJSONError{Path: path, Value: what, Omitted: omitted}, *le, name)
		}
		assert.Equal(t, "keep", string(out), name)
	}
	loss("nested", "$.messages[0].content[1].x[2][\"y z\"][0]", "NaN", false)
	loss("quote key", `$["a\"b"]["\\u00e9\n"]`, "undefined", true) // the JS source has a backslash
	loss("unicode key", `$["héllo"]["日本"]`, "a function", true)
	loss("empty key", `$[""]`, "undefined", true)
	loss("dollar key", "$.$a_1._", "undefined", true)
	loss("hole", "$[1]", "undefined", false)
	loss("proxy get", "$.p.b", "undefined", true)
	loss("proxy array", "$[1]", "undefined", false)
	loss("toJSON root", "$", "undefined", false)
	loss("toJSON nested", "$.a.b[0]", "undefined", false)
	loss("toJSON key", "$.a", "a symbol", true)
	loss("boxed nan", "$.n", "NaN", false)
	loss("boxed inf", "$[0]", "Infinity", false)
	loss("cycle after loss", "$.u", "undefined", true)
	loss("deep loss", "$"+strings.Repeat("[0]", 9001), "undefined", false)

	opaque := func(name, path, what string) {
		t.Helper()
		out, err := rt.AppendJSONWith([]byte("keep"), value(name), moejs.JSONOptions{Strict: true})
		var le *moejs.LossyJSONError
		if assert.ErrorAs(t, err, &le, name) {
			assert.Equal(t, moejs.LossyJSONError{Path: path, Value: what, Object: true}, *le, name)
			assert.Equal(t, "moejs: "+path+" is "+what+", whose contents JSON does not write", le.Error())
		}
		assert.Equal(t, "keep", string(out), name)
	}
	opaque("map", "$.m", "a Map")
	opaque("set", "$[0]", "a Set")
	opaque("weak", "$.w[0]", "a WeakMap")
	opaque("promise", "$.p", "a Promise")
	opaque("regexp", "$.r", "a RegExp")
	opaque("error", "$.e", "an Error")
	opaque("error subclass", "$.list[1]", "an Error")
	opaque("buffers", "$.b", "an ArrayBuffer")
	opaque("shared buffer", "$.b", "a SharedArrayBuffer")
	opaque("data view", "$.v", "a DataView")
	opaque("opaque root", "$", "a Map")
	opaque("opaque with own data", "$.m", "a Map")

	notLoss := func(name, msg string) {
		t.Helper()
		_, err := rt.AppendJSONWith(nil, value(name), moejs.JSONOptions{Strict: true})
		var le *moejs.LossyJSONError
		assert.False(t, errors.As(err, &le), name)
		assert.ErrorContains(t, err, msg, name)
	}
	notLoss("getter throws", "boom")
	notLoss("cycle", "circular")
	notLoss("bigint", "BigInt")

	ok := func(name, want string) {
		t.Helper()
		out, err := rt.AppendJSONWith(nil, value(name), moejs.JSONOptions{Strict: true})
		require.NoError(t, err, name)
		assert.Equal(t, want, string(out), name)
	}
	ok("opaque with toJSON", `{"m":[[1,2]]}`)
	ok("typed array", `{"t":{"0":1,"1":2}}`)
	ok("date", `{"d":null}`) // Date#toJSON returns null for an invalid date: no loss
	ok("neg zero", `{"z":0,"a":[0],"n":0}`)
	ok("fine", "{\"a\":[1,\"two\",null,true,{\"b\":2.5,\"c\":[[],{}]}],\"x y\":\"\",\"\u2028\":1e+21,\"n\":-1.5e-7}")
}

// TestAppendJSONDefaultMatchesStringify: without Strict, AppendJSON and
// AppendJSONWith write what JSON.stringify writes, byte for byte, for the
// lossy values too; the hash of the outputs is logged to compare trees.
func TestAppendJSONDefaultMatchesStringify(t *testing.T) {
	mod, err := moejs.Compile("strict.js", strictPathsSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	h := sha256.New()
	for _, name := range []string{"nested", "quote key", "unicode key", "empty key", "dollar key", "hole", "proxy get", "proxy array",
		"toJSON root", "toJSON nested", "toJSON key", "map", "set", "weak", "promise", "regexp", "error",
		"error subclass", "buffers", "shared buffer", "data view", "opaque root", "opaque with own data", "opaque with toJSON", "typed array", "boxed nan", "boxed inf", "date", "neg zero", "deep loss", "fine", "cycle after loss"} {
		if name == "cycle after loss" {
			continue
		}
		v, err := rt.Call(mustHook(t, mod, "value"), moejs.String(name))
		require.NoError(t, err)
		js, err := rt.Call(mustHook(t, mod, "stringify"), v)
		require.NoError(t, err)
		a, err := rt.AppendJSON(nil, v)
		require.NoError(t, err)
		b, err := rt.AppendJSONWith(nil, v, moejs.JSONOptions{})
		require.NoError(t, err)
		want := js.String()
		if js.IsUndefined() {
			want = "null"
		}
		assert.Equal(t, want, string(a), name)
		assert.Equal(t, string(a), string(b), name)
		h.Write(a)
	}
	for _, x := range []any{math.NaN(), math.Inf(1), math.Copysign(0, -1), map[string]any{"a": math.NaN(), "b": []any{nil, 1.5}}} {
		v, err := rt.FromGo(x)
		require.NoError(t, err)
		a, err := rt.AppendJSON(nil, v)
		require.NoError(t, err)
		h.Write(a)
	}
	t.Logf("AppendJSON output hash %s", hex.EncodeToString(h.Sum(nil)))
}

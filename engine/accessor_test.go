package engine

import (
	"errors"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accEval runs body (a function body) in a fresh realm, with shared
// intrinsics when shared is set, and returns JSON.stringify of the result
// or "throws Name: message".
func accEval(t *testing.T, shared bool, body string) string {
	t.Helper()
	src := `export function f() {
  try { return String(JSON.stringify((function () { ` + body + ` })())); }
  catch (e) { return "throws " + e.name + ": " + e.message; }
}`
	m, err := syntax.ParseModule("acc.js", src, syntax.Options{})
	require.NoError(t, err, "parse: %s", body)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err, "compile: %s", body)
	r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	fn, _ := env.GetBindingValue("f")
	v, err := r.Call(fn, Undefined(), nil)
	if err != nil {
		var exc *Exception
		if errors.As(err, &exc) {
			return "hosterr " + errorDisplayString(exc.Value)
		}
		return "hosterr " + err.Error()
	}
	return v.AsString().GoString()
}

// accTable checks body -> want in both realm kinds.
func accTable(t *testing.T, cases [][2]string) {
	t.Helper()
	for _, c := range cases {
		for _, shared := range []bool{false, true} {
			assert.Equal(t, c[1], accEval(t, shared, c[0]), "shared=%v body: %s", shared, c[0])
		}
	}
}

func TestObjectGetOwnPropertyDescriptor(t *testing.T) {
	accTable(t, [][2]string{
		{`return Object.getOwnPropertyDescriptor({a: 1}, "a");`, `{"value":1,"writable":true,"enumerable":true,"configurable":true}`},
		{`var o = {}; Object.defineProperty(o, "x", {get: function () { return 1; }});
		  var d = Object.getOwnPropertyDescriptor(o, "x");
		  return [Object.keys(d), typeof d.get, d.set === undefined, "set" in d, d.enumerable, d.configurable];`,
			`[["get","set","enumerable","configurable"],"function",true,true,false,false]`},
		{`return Object.getOwnPropertyDescriptor({}, "nope") === undefined;`, `true`},
		{`return Object.getOwnPropertyDescriptor("ab", 1);`, `{"value":"b","writable":false,"enumerable":true,"configurable":false}`},
		{`return Object.getOwnPropertyDescriptor("ab", "length");`, `{"value":2,"writable":false,"enumerable":false,"configurable":false}`},
		{`return Object.getOwnPropertyDescriptor([1, 2], "length");`, `{"value":2,"writable":true,"enumerable":false,"configurable":false}`},
		{`var a = [1]; Object.freeze(a); return [Object.getOwnPropertyDescriptor(a, "length"), Object.getOwnPropertyDescriptor(a, 0)];`,
			`[{"value":1,"writable":false,"enumerable":false,"configurable":false},{"value":1,"writable":false,"enumerable":true,"configurable":false}]`},
		{`var d = Object.getOwnPropertyDescriptor(function f(a, b) {}, "prototype");
		  return [typeof d.value, d.writable, d.enumerable, d.configurable];`, `["object",true,false,false]`},
		{`return Object.getOwnPropertyDescriptor(Math, "PI");`, `{"value":3.141592653589793,"writable":false,"enumerable":false,"configurable":false}`},
		{`return Object.getOwnPropertyDescriptor(1, "x") === undefined;`, `true`},
		{`return Object.getOwnPropertyDescriptor(null, "x");`, `throws TypeError: Cannot convert undefined or null to object`},
		// ToObject precedes ToPropertyKey.
		{`var log = []; try { Object.getOwnPropertyDescriptor(null, {toString: function () { log.push("key"); return "k"; }}); } catch (e) { log.push(e.name); } return log;`,
			`["TypeError"]`},
		{`return Object.getOwnPropertyDescriptor({1: "one"}, {toString: function () { return "1"; }}).value;`, `"one"`},
		// Descriptor objects are ordinary, extensible, and inherit from
		// Object.prototype.
		{`var d = Object.getOwnPropertyDescriptor({a: 1}, "a"); d.extra = 2; delete d.value;
		  return [Object.getPrototypeOf(d) === Object.prototype, Object.isExtensible(d), Object.keys(d)];`,
			`[true,true,["writable","enumerable","configurable","extra"]]`},
	})
}

func TestFromPropertyDescriptorAllocs(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		getter := r.NewNativeFunction(AtomEmpty, 0, nil)
		data := DataDescriptor(IntValue(1), attrDefault)
		acc := AccessorDescriptor(getter, nil, attrConfigurable)
		r.FromPropertyDescriptor(data)
		r.FromPropertyDescriptor(acc)
		assert.Equal(t, 1.0, testing.AllocsPerRun(100, func() { r.FromPropertyDescriptor(data) }), "shared=%v", shared)
		assert.Equal(t, 1.0, testing.AllocsPerRun(100, func() { r.FromPropertyDescriptor(acc) }), "shared=%v", shared)
		d1, d2 := r.FromPropertyDescriptor(data).AsObject(), r.FromPropertyDescriptor(data).AsObject()
		assert.Same(t, d1.Shape(), d2.Shape())
		assert.NotSame(t, d1.Shape(), r.FromPropertyDescriptor(acc).AsObject().Shape())
		assert.Same(t, r.ObjectPrototype, d1.Proto())
	}
	// Shared realms use the template's shapes: no per-realm shape nodes.
	r1, r2 := NewRealmWith(RealmOptions{SharedIntrinsics: true}), NewRealmWith(RealmOptions{SharedIntrinsics: true})
	d := DataDescriptor(IntValue(1), attrDefault)
	s1 := r1.FromPropertyDescriptor(d).AsObject().Shape()
	assert.Same(t, s1, r2.FromPropertyDescriptor(d).AsObject().Shape())
	assert.True(t, s1.IsShared())
}

func TestObjectGetOwnPropertyDescriptors(t *testing.T) {
	accTable(t, [][2]string{
		{`var o = {b: 1, 2: "two"}; Object.defineProperty(o, "a", {get: function () { return 0; }, enumerable: true});
		  var ds = Object.getOwnPropertyDescriptors(o);
		  return [Object.keys(ds), ds[2].value, ds.b.writable, typeof ds.a.get, ds.a.enumerable, ds.a.configurable];`,
			`[["2","b","a"],"two",true,"function",true,false]`},
		{`return Object.keys(Object.getOwnPropertyDescriptors([7]));`, `["0","length"]`},
		{`return Object.getOwnPropertyDescriptors("x");`,
			`{"0":{"value":"x","writable":false,"enumerable":true,"configurable":false},"length":{"value":1,"writable":false,"enumerable":false,"configurable":false}}`},
		{`return Object.getOwnPropertyDescriptors(undefined);`, `throws TypeError: Cannot convert undefined or null to object`},
		// Round trip through defineProperties keeps accessors.
		{`var src = {}; var n = 0; Object.defineProperty(src, "x", {get: function () { return ++n; }, enumerable: true, configurable: true});
		  var dst = Object.defineProperties({}, Object.getOwnPropertyDescriptors(src));
		  return [dst.x, dst.x, Object.getOwnPropertyDescriptor(dst, "x").configurable];`, `[1,2,true]`},
	})
}

func TestObjectDefinePropertiesStatic(t *testing.T) {
	accTable(t, [][2]string{
		{`var o = {}; var r = Object.defineProperties(o, {a: {value: 1, enumerable: true}, b: {get: function () { return 2; }}});
		  return [r === o, o.a, o.b, Object.keys(o)];`, `[true,1,2,["a"]]`},
		{`return Object.defineProperties(1, {});`, `throws TypeError: Object.defineProperties called on non-object`},
		{`return Object.defineProperties({}, null);`, `throws TypeError: Cannot convert undefined or null to object`},
		// Every descriptor is validated before the first definition.
		{`var o = {}; try { Object.defineProperties(o, {a: {value: 1}, b: 5}); } catch (e) { return [e.message, "a" in o]; }`,
			`["Property description must be an object: 5",false]`},
		// Non-enumerable entries of the properties object are skipped.
		{`var props = {}; Object.defineProperty(props, "hidden", {value: {value: 1}}); props.shown = {value: 2};
		  var o = Object.defineProperties({}, props); return ["hidden" in o, o.shown];`, `[false,2]`},
		// Descriptor fields are read with getters.
		{`var d = {}; Object.defineProperty(d, "value", {get: function () { return "via getter"; }});
		  return Object.defineProperties({}, {k: d}).k;`, `"via getter"`},
		{`return Object.defineProperties({}, {x: {get: 1}});`, `throws TypeError: Getter must be a function: 1`},
		{`return Object.defineProperties({}, {x: {get: function () {}, writable: true}});`,
			`throws TypeError: Invalid property descriptor. Cannot both specify accessors and a value or writable attribute`},
	})
}

func TestObjectSealPreventExtensions(t *testing.T) {
	accTable(t, [][2]string{
		{`var o = {a: 1}; var r = Object.seal(o); o.a = 2;
		  return [r === o, o.a, Object.isSealed(o), Object.isFrozen(o), Object.isExtensible(o), Object.getOwnPropertyDescriptor(o, "a").configurable];`,
			`[true,2,true,false,false,false]`},
		{`var o = Object.seal({a: 1}); o.b = 1;`, `throws TypeError: Cannot assign to read only property 'b' of object`},
		{`var o = Object.seal({a: 1}); delete o.a;`, `throws TypeError: Cannot delete property 'a' of [object Object]`},
		{`var o = {}; Object.defineProperty(o, "x", {get: function () { return 1; }, configurable: true}); Object.seal(o);
		  var d = Object.getOwnPropertyDescriptor(o, "x"); return [typeof d.get, d.configurable, o.x, Object.isFrozen(o)];`,
			`["function",false,1,true]`},
		{`var a = Object.seal([1, 2]); a[0] = 9; return [a[0], Object.isSealed(a), Object.getOwnPropertyDescriptor(a, 1).configurable];`, `[9,true,false]`},
		{`var a = Object.seal([1, 2]); a.push(3);`, `throws TypeError: Cannot assign to read only property '2' of object`},
		{`return [Object.seal(1), Object.isSealed(1), Object.isSealed("s"), Object.isSealed({}), Object.isSealed(Object.preventExtensions({}))];`,
			`[1,true,true,false,true]`},
		{`var o = {a: 1}; var r = Object.preventExtensions(o); o.a = 2; delete o.a;
		  return [r === o, Object.isExtensible(o), "a" in o, Object.isSealed(o), Object.isFrozen(o)];`, `[true,false,false,true,true]`},
		{`var o = Object.preventExtensions({}); o.x = 1;`, `throws TypeError: Cannot assign to read only property 'x' of object`},
		{`var o = Object.preventExtensions({}); Object.defineProperty(o, "x", {value: 1});`, `throws TypeError: Cannot redefine property: x`},
		{`return [Object.preventExtensions(1), Object.preventExtensions(undefined) === undefined, Object.isExtensible(1), Object.isExtensible({}), Object.isExtensible(Object.freeze({}))];`,
			`[1,true,false,true,false]`},
		// A non-extensible object keeps its prototype.
		{`var o = Object.preventExtensions({}); Object.setPrototypeOf(o, null);`, `throws TypeError: [object Object] is not extensible`},
		// Sealing converges on one shape and does not break later reads.
		{`function mk() { return Object.seal({p: 1, q: 2}); } var a = mk(), b = mk(); a.p = 3;
		  return [a.p + a.q, b.p + b.q, Object.keys(a)];`, `[5,3,["p","q"]]`},
	})
	// Intrinsics: already frozen in shared realms, freely sealable in
	// mutable ones.
	assert.Equal(t, `[true,true,false,true]`, accEval(t, true, `var p = Object.preventExtensions(Object.prototype); Object.seal(Math);
		return [p === Object.prototype, Object.isSealed(Object.prototype), Object.isExtensible(Math), Object.isFrozen(Math)];`))
	assert.Equal(t, `[true,false,true,false]`, accEval(t, false, `Object.seal(Math);
		return [Object.isSealed(Math), Object.isExtensible(Math), Object.getOwnPropertyDescriptor(Math, "max").writable, Object.getOwnPropertyDescriptor(Math, "max").configurable];`))
}

func TestObjectLiteralAccessors(t *testing.T) {
	accTable(t, [][2]string{
		{`var o = { get x() { return 1; }, set x(v) { this.y = v; } }; o.x = 5;
		  var d = Object.getOwnPropertyDescriptor(o, "x");
		  return [o.x, o.y, Object.keys(o), typeof d.get, typeof d.set, d.enumerable, d.configurable];`,
			`[1,5,["x","y"],"function","function",true,true]`},
		// Function names and lengths.
		{`var k = "c";
		  var o = { get x() {}, set x(v) {}, get 1() {}, get 1.5() {}, get "s p"() {}, get ""() {}, set [k + 1](v) {}, get [""]() {} };
		  function g(key) { return Object.getOwnPropertyDescriptor(o, key); }
		  return [g("x").get.name, g("x").set.name, g(1).get.name, g("1.5").get.name, g("s p").get.name, g("").get.name,
		    g("c1").set.name, g("x").get.length, g("x").set.length, String(g("x").get)];`,
			`["get x","set x","get 1","get 1.5","get s p","get ","set c1",0,1,"get x() {}"]`},
		// Getters and setters are not constructors and have no prototype.
		{`var g = Object.getOwnPropertyDescriptor({ get x() {} }, "x").get; return ["prototype" in g, Object.getPrototypeOf(g) === Function.prototype];`,
			`[false,true]`},
		{`var g = Object.getOwnPropertyDescriptor({ get x() {} }, "x").get; try { new g(); } catch (e) { return e.name; }`, `"TypeError"`},
		// Merging and replacement follow definition order.
		{`var o = { get a() { return 1; }, b: 2, set a(v) {} }; var d = Object.getOwnPropertyDescriptor(o, "a");
		  return [Object.keys(o), typeof d.get, typeof d.set];`, `[["a","b"],"function","function"]`},
		{`var o = { a: 1, get a() { return 2; } }; var d = Object.getOwnPropertyDescriptor(o, "a");
		  return [o.a, "value" in d, d.set === undefined];`, `[2,false,true]`},
		{`var o = { get a() { return 2; }, a: 1 }; return Object.getOwnPropertyDescriptor(o, "a");`,
			`{"value":1,"writable":true,"enumerable":true,"configurable":true}`},
		{`var o = { set a(v) {}, get a() { return 1; }, get a() { return 2; } }; var d = Object.getOwnPropertyDescriptor(o, "a");
		  return [o.a, typeof d.set];`, `[2,"function"]`},
		{`var o = { get a() { return 1; }, ...{ b: 2 }, set a(v) {} }; return [Object.keys(o), typeof Object.getOwnPropertyDescriptor(o, "a").get];`,
			`[["a","b"],"function"]`},
		// Keys: indices, __proto__, and computed keys converted exactly once, in order.
		{`var o = { b: 0, get 0() { return "a"; }, 1: "b" }; return [o[0], o["0"], Object.keys(o)];`, `["a","a",["0","1","b"]]`},
		{`var o = { get __proto__() { return 5; } }; return [o.__proto__, Object.getPrototypeOf(o) === Object.prototype, Object.keys(o)];`,
			`[5,true,["__proto__"]]`},
		{`var n = 0; var k = { toString: function () { n++; return "z"; } };
		  var o = { get [k]() { return 1; } }; return [n, o.z, Object.getOwnPropertyDescriptor(o, "z").get.name];`, `[1,1,"get z"]`},
		{`var log = []; var o = { [(log.push("k1"), "a")]: log.push("v1"), get [(log.push("k2"), "b")]() {}, c: log.push("v3") }; return log;`,
			`["k1","v1","k2","v3"]`},
		{`var o = { get [{ toString: function () { throw new EvalError("key"); } }]() {} };`, `throws EvalError: key`},
		// A computed data key is converted before its value is evaluated,
		// and names an anonymous class value.
		{`var log = []; var k = { toString: function () { log.push("key"); return "p"; } };
		  var o = { [k]: (log.push("value"), 1), [k]: class extends (log.push("heritage"), Object) {}, [k]: 3 };
		  return [o.p, log];`, `[3,["key","value","key","heritage","key"]]`},
		{`var v = "bad"; var o = { [{ toString: function () { v = "ok"; return "p"; } }]: v }; return o.p;`, `"ok"`},
		{`var s = Symbol("d"), a = Symbol(); var o = { [s]: class {}, [a]: class {}, ["c" + 1]: class {}, [s + "x"]: 1 };`,
			`throws TypeError: Cannot convert a Symbol value to a string`},
		{`var s = Symbol("d"), a = Symbol(); var o = { [s]: class {}, [a]: class {}, ["c" + 1]: class {}, [1]: class { static name = "own"; }, [2]: class C {} };
		  return [o[s].name, o[a].name, o.c1.name, o[1].name, o[2].name];`, `["[d]","","c1","own","C"]`},
		// Receivers.
		{`var p = { get me() { return this; } }; var o = Object.create(p); return [p.me === p, o.me === o];`, `[true,true]`},
		{`var p = { set x(v) { this._x = v; } }; var o = Object.create(p); o.x = 3; return [o._x, p._x, o.hasOwnProperty("x")];`,
			`[3,null,false]`},
		{`var o = { get x() { throw new RangeError("boom"); } }; return o.x;`, `throws RangeError: boom`},
		{`var o = { get x() { return this.x; } }; return o.x;`, `throws RangeError: Maximum call stack size exceeded`},
	})
}

func TestKeyFunctionName(t *testing.T) {
	r := NewRealm()
	for _, c := range []struct {
		key    PropertyKey
		prefix string
		want   string
	}{
		{r.KeyFromGoString("x"), "", "x"},
		{r.KeyFromGoString("x"), "get ", "get x"},
		{IndexKey(7), "set ", "set 7"},
		{SymbolKey(SymIterator), "", "[Symbol.iterator]"},
		{SymbolKey(SymIterator), "get ", "get [Symbol.iterator]"},
		{SymbolKey(NewSymbol(nil)), "", ""},
		{SymbolKey(NewSymbol(nil)), "set ", "set "},
		{SymbolKey(NewSymbol(EmptyString())), "", "[]"},
	} {
		got, err := r.keyFunctionName(c.key, c.prefix)
		require.NoError(t, err)
		assert.Equal(t, c.want, got.GoString())
	}
}

// TestAccessorSemantics audits the paths that read or write properties
// generically against accessor properties.
func TestAccessorSemantics(t *testing.T) {
	accTable(t, [][2]string{
		// Enumeration snapshots the keys, then reads each still-present
		// property through [[Get]].
		{`var o = { get a() { delete this.b; return 1; }, b: 2 }; return Object.values(o);`, `[1]`},
		{`var o = { get a() { delete this.b; return 1; }, b: 2 }; return Object.entries(o);`, `[["a",1]]`},
		{`var o = { get a() { Object.defineProperty(this, "b", { enumerable: false }); return 1; }, b: 2 }; return Object.values(o);`, `[1]`},
		{`var o = { get a() { this.c = 3; return 1; }, b: 2 }; return [Object.entries(o), Object.keys(o)];`, `[[["a",1],["b",2]],["a","b","c"]]`},
		{`return { ...{ get a() { delete this.b; return 1; }, b: 2 } };`, `{"a":1}`},
		{`var t = {}; Object.assign(t, { get a() { delete this.b; return 1; }, b: 2 }); return t;`, `{"a":1}`},
		{`var t = {}; try { Object.assign(t, { a: 1, get b() { throw new Error("x"); }, c: 3 }); } catch (e) {} return t;`, `{"a":1}`},
		{`var r = []; for (var k in { get a() { delete this.b; return 1; }, b: 2 }) r.push(k); return r;`, `["a","b"]`},
		{`var r = []; var o = { get a() { delete this.b; return 1; }, b: 2 }; for (var k in o) r.push(o[k]); return r;`, `[1]`},
		// JSON.stringify takes the key list first: a key made non-enumerable
		// by an earlier getter is still serialized, a deleted one is not.
		{`return JSON.stringify({ get a() { delete this.b; return 1; }, b: 2 });`, `"{\"a\":1}"`},
		{`return JSON.stringify({ get a() { Object.defineProperty(this, "b", { enumerable: false }); return 1; }, b: 2 });`, `"{\"a\":1,\"b\":2}"`},
		{`return JSON.stringify({ get toJSON() { return function () { return "tj"; }; } });`, `"\"tj\""`},
		{`return JSON.stringify({ get x() { throw new TypeError("json"); } });`, `throws TypeError: json`},
		// Key queries, in, delete and hasOwnProperty never call getters.
		{`var n = 0; var o = { get x() { n++; return 1; } }; Object.keys(o); Object.getOwnPropertyNames(o); o.hasOwnProperty("x"); var a = "x" in o;
		  var b = delete o.x; return [n, a, b, "x" in o];`, `[0,true,true,false]`},
		{`var o = {}; Object.defineProperty(o, "x", { get: function () {} }); delete o.x;`, `throws TypeError: Cannot delete property 'x' of [object Object]`},
		// Target setters: Object.assign uses [[Set]], spread defines.
		{`var log = ""; var t = { set x(v) { log += v; } }; Object.assign(t, { x: 1 }); return [log, "value" in Object.getOwnPropertyDescriptor(t, "x")];`, `["1",false]`},
		{`var log = ""; var o = { set x(v) { log += v; }, ...{ x: 1 } }; return [log, o.x];`, `["",1]`},
		// Destructuring reads through getters with the source as receiver.
		{`var { a, b } = { get a() { return this; }, b: 1 }; return [typeof a, b];`, `["object",1]`},
		{`var n = 0; var { x = n++ } = { get x() { return undefined; } }; return n;`, `1`},
		{`var o = Object.create({ get p() { return 1; } }); var { p, ...rest } = o; return [p, rest];`, `[1,{}]`},
		{`var a = [1, 2]; Object.defineProperty(a, 0, { get: function () { return "g"; } }); var [x, y] = a; var s = [];
		  for (var v of a) s.push(v); return [x, y, [...a], s, Math.max.apply(null, [5].concat(a.length))];`, `["g",2,["g",2],["g",2],5]`},
		// A getter-only property rejects assignment in strict code, own or
		// inherited, and the receiver gets no own property.
		{`var o = { get x() { return 1; } }; o.x = 2;`, `throws TypeError: Cannot assign to read only property 'x' of object`},
		{`var o = Object.create({ get x() { return 1; } }); try { o.x = 2; } catch (e) { return [e.name, o.hasOwnProperty("x")]; }`, `["TypeError",false]`},
		{`var o = { get x() { return 1; } }; o["x"] = 2;`, `throws TypeError: Cannot assign to read only property 'x' of object`},
		// Frozen objects keep calling their setters; accessors need no
		// writable bit to be frozen.
		{`var log = ""; var o = Object.freeze({ set x(v) { log += v; }, get x() { return log; } }); o.x = "a"; o.x = "b"; return [o.x, Object.isFrozen(o)];`, `["ab",true]`},
		{`var o = { get x() {} }; Object.preventExtensions(o); var a = Object.isFrozen(o); Object.defineProperty(o, "x", { configurable: false });
		  return [a, Object.isFrozen(o), Object.isSealed(o)];`, `[false,true,true]`},
		{`var o = Object.freeze({ get x() { return 1; } }); Object.defineProperty(o, "x", { get: function () { return 2; } });`, `throws TypeError: Cannot redefine property: x`},
		// Array index accessors move the array off its dense paths.
		{`var a = [1, 2, 3]; Object.defineProperty(a, 1, { get: function () { return 20; }, configurable: true });
		  return [a[1], a.slice(), a.map(function (x) { return x; }), a.indexOf(20), a.join(), JSON.stringify(a), a.length, Object.keys(a)];`,
			`[20,[1,20,3],[1,20,3],1,"1,20,3","[1,20,3]",3,["0","1","2"]]`},
		{`var log = ""; var a = [1, 2]; Object.defineProperty(a, 0, { set: function (v) { log += v; } }); a[0] = 5; a.fill(7); return [log, a[0], a[1]];`, `["57",null,7]`},
		{`var a = [1, 2, 3]; var log = []; Object.defineProperty(a, 0, { get: function () { log.push("g"); return 1; }, set: function (v) { log.push("s" + v); } });
		  a.reverse(); return [log, a[2]];`, `[["g","s3"],1]`},
		{`var log = []; var a = [3, 1, 2]; Object.defineProperty(a, 0, { get: function () { return 3; }, set: function (v) { log.push(v); } }); a.sort(); return [log, a[1], a[2]];`,
			`[[1],2,3]`},
		{`var a = [1]; Object.defineProperty(a, 0, { get: function () { return 1; } }); a.push(2); a[0] = 5;`, `throws TypeError: Cannot assign to read only property '0' of object`},
		// A non-writable length.
		{`var a = [1, 2]; Object.defineProperty(a, "length", { writable: false }); try { a.push(3); } catch (e) { return [e.name, a.length, 2 in a]; }`, `["TypeError",2,false]`},
		{`var a = [1, 2]; Object.defineProperty(a, "length", { writable: false }); a[5] = 1;`, `throws TypeError: Cannot assign to read only property '5' of object`},
		{`var a = [1, 2]; Object.defineProperty(a, "length", { writable: false }); a[0] = 9; return [a[0], a.length, a.slice(), a.map(function (x) { return x + 1; })];`, `[9,2,[9,2],[10,3]]`},
		// Inline caches drop entries when a property turns into an accessor
		// or an accessor appears on the prototype chain.
		{`function rd(o) { return o.x; } var o = { x: 1 }; var r = [rd(o), rd(o)]; Object.defineProperty(o, "x", { get: function () { return 2; } }); return r.concat(rd(o), rd(o));`, `[1,1,2,2]`},
		{`function rd(o) { return o.x; } var p = { x: 1 }; var o = Object.create(p); rd(o); rd(o); Object.defineProperty(p, "x", { get: function () { return 3; } }); return rd(o);`, `3`},
		{`function rd(o) { return o.x; } var o = { get x() { return 1; } }; var r = [rd(o), rd(o)]; Object.defineProperty(o, "x", { value: 5 }); return r.concat(rd(o));`, `[1,1,5]`},
		{`function wr(o, v) { o.y = v; } var log = ""; var P = {}; var a = Object.create(P); wr(a, 1); wr(Object.create(P), 0); var b = Object.create(P);
		  Object.defineProperty(P, "y", { set: function (v) { log += v; } }); wr(b, 2); return [log, a.y, b.hasOwnProperty("y")];`, `["2",1,false]`},
		{`function wr(o, v) { o.y = v; } var o = { y: 0 }; wr(o, 1); wr(o, 2); var log = ""; Object.defineProperty(o, "y", { set: function (v) { log += v; } }); wr(o, 3); return log;`, `"3"`},
		{`var n = 0; var o = { get x() { return ++n; } }; var s = 0; for (var i = 0; i < 10; i++) s += o.x; return [s, n];`, `[55,10]`},
		// Global accessors.
		{`Object.defineProperty(globalThis, "gv", { get: function () { return 42; }, configurable: true }); return [gv, typeof gv, globalThis.gv];`, `[42,"number",42]`},
		{`var log = ""; Object.defineProperty(globalThis, "gs", { set: function (v) { log += v; }, get: function () { return log; }, configurable: true }); gs = "a"; gs = "b"; return gs;`, `"ab"`},
		{`Object.defineProperty(globalThis, "go", { get: function () { return 1; }, configurable: true }); go = 2;`, `throws TypeError: Cannot assign to read only property 'go' of object`},
		// Redefinition (ValidateAndApplyPropertyDescriptor).
		{`var o = {}; var g = function () { return 1; }; Object.defineProperty(o, "x", { get: g }); Object.defineProperty(o, "x", { get: g });
		  Object.defineProperty(o, "x", { get: g, set: undefined, enumerable: false, configurable: false }); return o.x;`, `1`},
		{`var o = {}; Object.defineProperty(o, "x", { get: function () {} }); Object.defineProperty(o, "x", { get: function () {} });`, `throws TypeError: Cannot redefine property: x`},
		{`var o = {}; Object.defineProperty(o, "x", { get: function () {} }); Object.defineProperty(o, "x", { set: function (v) {} });`, `throws TypeError: Cannot redefine property: x`},
		{`var o = {}; Object.defineProperty(o, "x", { get: function () {} }); Object.defineProperty(o, "x", { value: 1 });`, `throws TypeError: Cannot redefine property: x`},
		{`var o = {}; Object.defineProperty(o, "x", { get: function () {} }); Object.defineProperty(o, "x", { enumerable: true });`, `throws TypeError: Cannot redefine property: x`},
		{`var o = {}; Object.defineProperty(o, "x", { value: 1 }); Object.defineProperty(o, "x", { get: function () {} });`, `throws TypeError: Cannot redefine property: x`},
		{`var o = {}; Object.defineProperty(o, "x", { get: function () {}, configurable: true, enumerable: true }); Object.defineProperty(o, "x", { value: 1 });
		  return Object.getOwnPropertyDescriptor(o, "x");`, `{"value":1,"writable":false,"enumerable":true,"configurable":true}`},
		{`var o = { x: 1 }; Object.defineProperty(o, "x", { get: function () { return 2; } }); var d = Object.getOwnPropertyDescriptor(o, "x");
		  return [o.x, d.enumerable, d.configurable, "set" in d, d.set === undefined];`, `[2,true,true,true,true]`},
		{`var o = {}; var g = function () { return 1; }; Object.defineProperty(o, "x", { get: g, configurable: true }); Object.defineProperty(o, "x", { set: function (v) {} });
		  var d = Object.getOwnPropertyDescriptor(o, "x"); return [d.get === g, typeof d.set];`, `[true,"function"]`},
		// Accessor descriptors reject value and writable, and non-callable halves.
		{`Object.defineProperty({}, "x", { get: function () {}, writable: false });`,
			`throws TypeError: Invalid property descriptor. Cannot both specify accessors and a value or writable attribute`},
		{`Object.defineProperty({}, "x", { set: function (v) {}, value: 1 });`,
			`throws TypeError: Invalid property descriptor. Cannot both specify accessors and a value or writable attribute`},
		{`Object.defineProperty({}, "x", { get: undefined, value: 1 });`,
			`throws TypeError: Invalid property descriptor. Cannot both specify accessors and a value or writable attribute`},
		{`Object.defineProperty({}, "x", { get: null });`, `throws TypeError: Getter must be a function: null`},
		{`Object.defineProperty({}, "x", { set: {} });`, `throws TypeError: Setter must be a function: [object Object]`},
		{`var o = {}; Object.defineProperty(o, "x", { get: undefined, set: undefined }); o.x = 1; `, `throws TypeError: Cannot assign to read only property 'x' of object`},
	})
}

// TestAccessorRealms covers accessors on intrinsics: mutable realms accept
// them (and call them with primitive receivers unboxed in strict code),
// shared realms reject every change to the frozen intrinsics.
func TestAccessorRealms(t *testing.T) {
	for _, c := range [][3]string{
		{`Object.defineProperty(Number.prototype, "me", { get: function () { return typeof this; }, configurable: true }); return [(5).me, (5)["me"]];`,
			`["number","number"]`, `throws TypeError: Cannot modify property 'me' of shared intrinsic [object Number]`},
		{`var log = ""; Object.defineProperty(String.prototype, "sx", { set: function (v) { log = typeof this + v; } }); "s".sx = 1; return log;`,
			`"string1"`, `throws TypeError: Cannot modify property 'sx' of shared intrinsic [object String]`},
		{`Object.defineProperty(Object.prototype, "q", { get: function () { return this === undefined; } }); return [({}).q, Object.keys({})];`,
			`[false,[]]`, `throws TypeError: Cannot modify property 'q' of shared intrinsic [object Object]`},
		// Array.prototype.push assigns through inherited index setters.
		{`var log = ""; Object.defineProperty(Array.prototype, 0, { set: function (v) { log += v; }, configurable: true });
		  var a = [], b = []; a.push(7); b.push(8, 9); return [log, a.length, a.hasOwnProperty(0), b.length, b.hasOwnProperty(0), b[1]];`,
			`["78",1,false,2,false,9]`, `throws TypeError: Cannot modify property '0' of shared intrinsic [object Array]`},
		// Inherited index getters fill holes.
		{`Object.defineProperty(Array.prototype, 1, { get: function () { return "inh"; }, configurable: true }); var a = [0, , 2];
		  return [a[1], a.join(), a.indexOf("inh"), a.slice(), a.map(function (x) { return x; }), [...a]];`,
			`["inh","0,inh,2",1,[0,"inh",2],[0,"inh",2],[0,"inh",2]]`, `throws TypeError: Cannot modify property '1' of shared intrinsic [object Array]`},
		// An Object.prototype setter intercepts adding assignments cached earlier.
		{`function mk() { var o = {}; o.z = 1; return o; } mk(); mk(); var log = "";
		  Object.defineProperty(Object.prototype, "z", { set: function (v) { log += v; }, configurable: true }); var o = mk(); return [log, o.hasOwnProperty("z")];`,
			`["1",false]`, `throws TypeError: Cannot modify property 'z' of shared intrinsic [object Object]`},
	} {
		assert.Equal(t, c[1], accEval(t, false, c[0]), "mutable: %s", c[0])
		assert.Equal(t, c[2], accEval(t, true, c[0]), "shared: %s", c[0])
	}
}

// TestAccessorIC exercises accessor inline-cache entries: each case runs
// the access in a loop so later iterations hit the entry filled by the
// first.
func TestAccessorIC(t *testing.T) {
	accTable(t, [][2]string{
		// Receivers: same-shaped objects share the entry but each is this.
		{`function mk(v) { return { v: v, get x() { return this.v; }, set x(n) { this.v = n * 10; } }; }
		  var os = [mk(1), mk(2), mk(3)]; var r = []; for (var i = 0; i < 6; i++) { var o = os[i % 3]; o.x = i; r.push(o.x); } return r;`, `[0,10,20,30,40,50]`},
		{`var p = { get x() { return this.id; }, set x(v) { this.seen = v; } }; var a = Object.create(p), b = Object.create(p); a.id = "a"; b.id = "b";
		  var r = []; for (var i = 0; i < 4; i++) { var o = i % 2 ? b : a; o.x = i; r.push(o.x, o.seen); } return [r, p.seen, a.hasOwnProperty("x")];`,
			`[["a",0,"b",1,"a",2,"b",3],null,false]`},
		{`var p = { get x() { return 1; } }; var q = Object.create(p); var o = Object.create(q); var r = [];
		  for (var i = 0; i < 3; i++) r.push(o.x); Object.defineProperty(q, "x", { value: 2 }); r.push(o.x); return r;`, `[1,1,1,2]`},
		// Swapping the getter or setter keeps the shape; hits read the slot.
		{`var o = {}; Object.defineProperty(o, "x", { get: function () { return 1; }, set: function (v) {}, configurable: true }); var r = [];
		  for (var i = 0; i < 4; i++) { r.push(o.x); if (i == 1) Object.defineProperty(o, "x", { get: function () { return 2; } }); } return r;`, `[1,1,2,2]`},
		{`var log = ""; var o = {}; Object.defineProperty(o, "x", { get: function () {}, set: function (v) { log += v; }, configurable: true });
		  try { for (var i = 0; i < 4; i++) { o.x = i; if (i == 1) Object.defineProperty(o, "x", { set: undefined }); } } catch (e) { return [log, e.message]; }`,
			`["01","Cannot assign to read only property 'x' of object"]`},
		{`var o = {}; Object.defineProperty(o, "x", { get: function () { return 1; }, configurable: true }); var r = [];
		  for (var i = 0; i < 4; i++) { r.push(o.x); if (i == 1) Object.defineProperty(o, "x", { get: undefined }); } return r;`, `[1,1,null,null]`},
		// A getter that reshapes its receiver, and one that throws once.
		{`var o = { n: 0, get x() { this["k" + this.n] = 1; return ++this.n; } }; var r = []; for (var i = 0; i < 3; i++) r.push(o.x); return [r, Object.keys(o)];`,
			`[[1,2,3],["n","x","k0","k1","k2"]]`},
		{`var n = 0; var o = { get x() { if (++n == 2) throw new Error("second"); return n; } }; var r = [];
		  for (var i = 0; i < 3; i++) { try { r.push(o.x); } catch (e) { r.push(e.message); } } return r;`, `[1,"second",3]`},
		// Setter arguments: rest, nested calls with many locals,
		// bound and native setters.
		{`var o = {}; Object.defineProperty(o, "x", { set: function (...a) { this.a = a; } }); for (var i = 0; i < 3; i++) o.x = i; return o.a;`, `[2]`},
		{`function deep(a, b, c, d) { var e = a + b, f = c + d; return [e, f, a, b, c, d].join(""); }
		  var o = { set x(v) { this.r = deep("a", "b", "c", "d") + v + deep(v, v, v, v); } }; for (var i = 0; i < 3; i++) o.x = i; return o.r;`, `"abcdabcd2442222"`},
		{`var t = {}; var o = {}; Object.defineProperty(o, "x", { set: function (a, v) { this.last = a + v; }.bind(t, "b") }); for (var i = 0; i < 3; i++) o.x = i; return t.last;`, `"b2"`},
		{`var o = {}; Object.defineProperty(o, "x", { set: Array.prototype.push }); for (var i = 0; i < 3; i++) o.x = i; return [o.length, o[0], o[2]];`, `[3,0,2]`},
		{`var o = { set x(v) { this.y = o.x = 0; } };`, `undefined`},
		// Globals.
		{`var n = 0; Object.defineProperty(globalThis, "gx", { get: function () { return ++n; }, set: function (v) { n = v * 100; }, configurable: true });
		  var r = []; for (var i = 0; i < 3; i++) r.push(gx); gx = 2; r.push(gx); delete globalThis.gx; r.push(typeof gx); return r;`, `[1,2,3,201,"undefined"]`},
		{`Object.defineProperty(globalThis, "gy", { get: function () { return 1; }, configurable: true }); var r = [];
		  for (var i = 0; i < 3; i++) { try { gy = i; r.push("set"); } catch (e) { r.push(e.name); } } return r;`, `["TypeError","TypeError","TypeError"]`},
	})
	// Getters on String.prototype see the primitive receiver (strict mode).
	assert.Equal(t, `["string:ab","string:cd","string:ab"]`, accEval(t, false, `Object.defineProperty(String.prototype, "tag", { get: function () { return typeof this + ":" + this; } });
		var r = []; var s = ["ab", "cd", "ab"]; for (var i = 0; i < 3; i++) r.push(s[i].tag); return r;`))
}

// TestAccessorICAllocs checks that cached getters and setters cost no
// allocations beyond the accessor's own work.
func TestAccessorICAllocs(t *testing.T) {
	skipInRegcheck(t)
	for _, src := range []string{
		`const o = { v: 1, get x() { return this.v; } }; export function run(n) { let s = 0; for (let i = 0; i < n; i++) s += o.x; return s; }`,
		`const o = Object.create({ get x() { return 1; } }); export function run(n) { let s = 0; for (let i = 0; i < n; i++) s += o.x; return s; }`,
		`const o = { v: 0, set x(v) { this.v = v; } }; export function run(n) { for (let i = 0; i < n; i++) o.x = i; return o.v; }`,
		`const o = Object.create({ set x(v) { this.v = v; } }); o.v = 0; export function run(n) { for (let i = 0; i < n; i++) o.x = i; return o.v; }`,
	} {
		m, err := syntax.ParseModule("ic.js", src, syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileModule(m)
		require.NoError(t, err)
		r := NewRealm()
		env, err := r.EvaluateModule(code)
		require.NoError(t, err)
		fn, _ := env.GetBindingValue("run")
		args := []Value{IntValue(100)}
		_, err = r.Call(fn, Undefined(), args)
		require.NoError(t, err)
		assert.Equal(t, 0.0, testing.AllocsPerRun(20, func() { _, _ = r.Call(fn, Undefined(), args) }), src)
	}
}

func TestAccessorICEntry(t *testing.T) {
	r := NewRealm()
	proto := r.NewObject()
	getter := r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) { return IntValue(7), nil })
	ok, err := proto.DefineOwnProperty(r, key(r, "g"), AccessorDescriptor(getter, nil, attrConfigurable))
	require.True(t, ok)
	require.NoError(t, err)
	o := r.NewObjectWithProto(proto)
	var e ICEntry
	r.fillGetIC(&e, o, key(r, "g"))
	// The entry is tagged: the data check fails, the accessor check passes.
	assert.Equal(t, uint32(0), r.ProtoEpoch()%2)
	assert.Equal(t, r.ProtoEpoch()|1, e.Epoch)
	assert.False(t, e.Valid(r, o))
	assert.Same(t, proto, e.Holder(o))
	a := r.accessorIC(&e, o)
	require.NotNil(t, a)
	assert.Same(t, getter, a.Get)
	// Data entries never pass the accessor check.
	mustSet(t, r, proto, "d", IntValue(1))
	var d ICEntry
	r.fillGetIC(&d, o, key(r, "d"))
	assert.True(t, d.Valid(r, o))
	assert.Nil(t, r.accessorIC(&d, o))
	// Prototype changes keep the epoch even and invalidate both kinds.
	assert.Nil(t, r.accessorIC(&e, o))
	assert.Equal(t, uint32(0), r.ProtoEpoch()%2)
	r.fillGetIC(&e, o, key(r, "g"))
	proto.Delete(r, key(r, "d"))
	assert.Nil(t, r.accessorIC(&e, o))
	assert.False(t, d.Valid(r, o))
}

// TestBuiltinAccessors covers the spec accessors on existing builtins:
// Object.prototype.__proto__ (Annex B) and the %ThrowTypeError% caller and
// arguments properties of Function.prototype.
func TestBuiltinAccessors(t *testing.T) {
	accTable(t, [][2]string{
		{`var p = { a: 1 }; var o = {}; o.__proto__ = p;
		  return [o.a, Object.getPrototypeOf(o) === p, o.__proto__ === p, ({}).__proto__ === Object.prototype, o.hasOwnProperty("__proto__")];`,
			`[1,true,true,true,false]`},
		{`return [(1).__proto__ === Number.prototype, "s".__proto__ === String.prototype, true.__proto__ === Boolean.prototype, [].__proto__ === Array.prototype];`,
			`[true,true,true,true]`},
		{`var o = {}; o.__proto__ = null; return [Object.getPrototypeOf(o) === null, o.__proto__ === undefined, "__proto__" in o];`,
			`[true,true,false]`},
		// Values that are neither objects nor null, and primitive receivers, are ignored.
		{`var o = {}; o.__proto__ = 5; o.__proto__ = "x"; o.__proto__ = undefined;
		  var d = Object.getOwnPropertyDescriptor(Object.prototype, "__proto__"); d.set.call(1, {});
		  return [Object.getPrototypeOf(o) === Object.prototype, d.set.call("s", null)];`,
			`[true,null]`},
		{`var d = Object.getOwnPropertyDescriptor(Object.prototype, "__proto__");
		  return [typeof d.get, typeof d.set, d.get.name, d.set.name, d.get.length, d.set.length, d.enumerable, Object.keys(Object.prototype)];`,
			`["function","function","get __proto__","set __proto__",0,1,false,[]]`},
		{`var d = Object.getOwnPropertyDescriptor(Object.prototype, "__proto__"); var r = [];
		  try { d.set.call(undefined, {}); } catch (e) { r.push(e.name, e.message); }
		  try { d.get.call(null); } catch (e) { r.push(e.name); }
		  var a = {}, b = Object.create(a); try { a.__proto__ = b; } catch (e) { r.push(e.message); }
		  var f = Object.preventExtensions({}); try { f.__proto__ = {}; } catch (e) { r.push(e.message); }
		  f.__proto__ = Object.prototype; return r;`,
			`["TypeError","Object.prototype.__proto__ called on null or undefined","TypeError","Cyclic __proto__ value","[object Object] is not extensible"]`},
		// The literal form and CreateDataProperty paths never call the setter.
		{`var p = {}; var o = { __proto__: p }; var q = { ["__proto__"]: p }; var s = { ...q };
		  return [Object.getPrototypeOf(o) === p, Object.getPrototypeOf(q) === Object.prototype, Object.keys(q), Object.getPrototypeOf(s) === Object.prototype, Object.keys(s)];`,
			`[true,true,["__proto__"],true,["__proto__"]]`},
		{`var o = JSON.parse('{"__proto__": {"x": 1}}'); var d = {}; Object.defineProperty(d, "__proto__", { value: 1, enumerable: true });
		  return [o.x, Object.keys(o), Object.getPrototypeOf(o) === Object.prototype, d.__proto__, Object.getPrototypeOf(d) === Object.prototype];`,
			`[null,["__proto__"],true,1,true]`},
		// Object.assign uses [[Set]], so it reaches the setter.
		{`var p = { y: 2 }; var t = Object.assign({}, { ["__proto__"]: p }); return [t.y, t.hasOwnProperty("__proto__")];`,
			`[2,false]`},
		{`var p = { v: 1 }, q = { v: 2 }; var os = [{}, {}]; var r = [];
		  for (var i = 0; i < 6; i++) { var o = os[i % 2]; o.__proto__ = i % 3 ? p : q; r.push(o.__proto__.v, o.v); } return r;`,
			`[2,2,1,1,1,1,2,2,1,1,1,1]`},
		// Function.prototype.caller/arguments are %ThrowTypeError% accessors.
		{`function f() {} var r = [];
		  try { f.caller; } catch (e) { r.push(e.name + ": " + e.message); }
		  try { f.arguments = 1; } catch (e) { r.push(e.name); }
		  try { (() => 1).caller; } catch (e) { r.push(e.name); }
		  return r;`,
			`["TypeError: 'caller', 'callee', and 'arguments' properties may not be accessed on strict mode functions or the arguments objects for calls to them","TypeError","TypeError"]`},
		{`function f() {} var d = Object.getOwnPropertyDescriptor(Function.prototype, "caller"), a = Object.getOwnPropertyDescriptor(Function.prototype, "arguments");
		  return [d.get === d.set, d.get === a.get, a.get === a.set, d.get.name, d.get.length, Object.isFrozen(d.get), d.enumerable,
		    "caller" in f, f.hasOwnProperty("caller"), Object.getOwnPropertyDescriptor(d.get, "name").configurable];`,
			`[true,true,true,"",0,true,false,true,false,false]`},
	})
	// Shared intrinsics are frozen, and writes to them throw before any setter runs.
	for _, c := range [][3]string{
		{`return [Object.getOwnPropertyDescriptor(Object.prototype, "__proto__").configurable,
		    Object.getOwnPropertyDescriptor(Function.prototype, "caller").configurable, Object.getOwnPropertyDescriptor(Function.prototype, "arguments").configurable];`,
			`[true,true,true]`, `[false,false,false]`},
		// Object.prototype is an immutable prototype exotic object.
		{`var r = []; try { Object.setPrototypeOf(Object.prototype, Object.create(null)); } catch (e) { r.push(e.message); }
		  try { Object.prototype.__proto__ = Object.create(null); } catch (e) { r.push(e.message); }
		  Object.setPrototypeOf(Object.prototype, null); r.push(Object.getPrototypeOf(Object.prototype)); return r;`,
			`["Immutable prototype object 'Object.prototype' cannot have their prototype set","Immutable prototype object 'Object.prototype' cannot have their prototype set",null]`,
			`["Immutable prototype object 'Object.prototype' cannot have their prototype set","Cannot modify property '__proto__' of shared intrinsic [object Object]",null]`},
		{`var p = Object.create(null); Array.prototype.__proto__ = p; return [Object.getPrototypeOf(Array.prototype) === p, [].hasOwnProperty];`,
			`[true,null]`, `throws TypeError: Cannot modify property '__proto__' of shared intrinsic [object Array]`},
	} {
		assert.Equal(t, c[1], accEval(t, false, c[0]), "mutable: %s", c[0])
		assert.Equal(t, c[2], accEval(t, true, c[0]), "shared: %s", c[0])
	}
}

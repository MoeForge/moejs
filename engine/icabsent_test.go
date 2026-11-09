package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reads of properties no object on the chain has, through the absent
// entries of the get inline cache (fillGetIC, icAbsent). Each case reads
// through one site several times, so that the later reads hit the entry
// the first one filled, then changes something the entry must not
// survive: each change that advances the prototype epoch, a receiver an
// absent entry must not answer, and sites that see several shapes.
func TestICAbsent(t *testing.T) {
	accTable(t, [][2]string{
		// An add on the prototype.
		{`function get(o) { return o.k; }
		  var P = {}, a = [];
		  for (var i = 0; i < 3; i++) a.push(get(Object.create(P)));
		  P.k = 1; a.push(get(Object.create(P)));
		  return a;`, `[null,null,null,1]`},
		// An add further up, then a change of its attributes: data to accessor.
		{`class A {} class B extends A {}
		  function get(o) { return o.k; }
		  var b = new B(), a = [get(b), get(b), get(b)];
		  A.prototype.k = 2; a.push(get(b));
		  Object.defineProperty(A.prototype, "k", {get() { return 3; }}); a.push(get(b));
		  return a;`, `[null,null,null,2,3]`},
		// An accessor added, deleted, a data property added.
		{`function get(o) { return o.k; }
		  var P = {}, o = Object.create(P), a = [get(o), get(o)];
		  Object.defineProperty(P, "k", {get() { return 4; }, configurable: true}); a.push(get(o));
		  delete P.k; a.push(get(o), get(o));
		  P.k = 5; a.push(get(o));
		  return a;`, `[null,null,4,null,null,5]`},
		// A deletion: an inherited property cached, then absent.
		{`function get(o) { return o.k; }
		  var P = {k: 1, x: 0}, o = Object.create(P), a = [get(o), get(o)];
		  delete P.k; a.push(get(o), get(o));
		  delete P.x; a.push(get(o));
		  return a;`, `[1,1,null,null,null]`},
		// A change of attributes: accessor to data, and of another key.
		{`function get(o) { return o.k; }
		  function getY(o) { return o.y; }
		  var P = {get k() { return 6; }, x: 1}, o = Object.create(P), a = [get(o), get(o), getY(o), getY(o)];
		  Object.defineProperty(P, "k", {value: 7}); a.push(get(o));
		  Object.defineProperty(P, "x", {enumerable: false}); a.push(getY(o));
		  Object.freeze(P); a.push(get(o), getY(o), Object.isFrozen(P));
		  return a;`, `[6,6,null,null,7,null,7,null,true]`},
		// A new prototype of the receiver.
		{`function get(o) { return o.k; }
		  var o = {}, a = [get(o), get(o)];
		  Object.setPrototypeOf(o, {k: 8}); a.push(get(o));
		  Object.setPrototypeOf(o, null); a.push(get(o), get(o));
		  return a;`, `[null,null,8,null,null]`},
		// A new prototype of a prototype.
		{`function get(o) { return o.k; }
		  var P = {}, o = Object.create(P), a = [get(o), get(o)];
		  Object.setPrototypeOf(P, {k: 9}); a.push(get(o));
		  Object.setPrototypeOf(P, Object.prototype); a.push(get(o), get(o));
		  return a;`, `[null,null,9,null,null]`},
		// A global added, with the global object on the chain.
		{`function get(o) { return o.absentGlobal; }
		  var o = Object.create(globalThis), a = [get(o), get(o)];
		  globalThis.absentGlobal = 10; a.push(get(o));
		  delete globalThis.absentGlobal; a.push(get(o), get(o));
		  return a;`, `[null,null,10,null,null]`},
		// A proxy as the receiver answers with its trap.
		{`function get(o) { return o.k; }
		  var a = [get({}), get({})];
		  var p = new Proxy({}, {get(t, k) { return "trap " + String(k); }});
		  a.push(get(p), get(p));
		  return a;`, `[null,null,"trap k","trap k"]`},
		// A proxy ending the chain answers every time.
		{`function get(o) { return o.k; }
		  var n = 0, o = Object.create(new Proxy({}, {get() { n++; return 11; }}));
		  return [get(o), get(o), get(o), n];`, `[11,11,11,3]`},
		// A proxy that becomes a prototype later.
		{`function get(o) { return o.k; }
		  var P = {}, o = Object.create(P), a = [get(o), get(o)];
		  Object.setPrototypeOf(P, new Proxy({}, {get() { return 12; }})); a.push(get(o), get(o));
		  return a;`, `[null,null,12,12]`},
		// length is never cached as absent: an array or a String object can
		// have the shape of an ordinary object.
		{`function len(o) { return o.length; }
		  var a = [len({}), len({})];
		  var arr = [1, 2, 3]; Object.setPrototypeOf(arr, Object.prototype); a.push(len(arr));
		  var n = Object.create(null); a.push(len(n), len(n));
		  var arr2 = [4]; Object.setPrototypeOf(arr2, null); a.push(len(arr2));
		  var s = new String("abcd"); Object.setPrototypeOf(s, null); a.push(len(s));
		  return a;`, `[null,null,3,null,null,1,4]`},
		// Functions, and a builtin prototype on the chain.
		{`function get(o) { return o.defaultProps; }
		  function C() {} function D() {}
		  var a = [get(C), get(D), get(C)];
		  D.defaultProps = 1; a.push(get(C), get(D));
		  var m = new Map(); a.push(get(m), get(m), m.size);
		  return a;`, `[null,null,null,null,1,null,null,0]`},
		// A site that sees several shapes: data, inherited, absent and
		// accessor entries.
		{`function get(o) { return o.k; }
		  var P = {k: "p"}, Q = {};
		  var objs = [{k: 1}, {a: 0, k: 2}, Object.create(P), Object.create(Q), {b: 0}, {get k() { return 3; }}, {c: 0, d: 0, k: 4}];
		  var out = [];
		  for (var r = 0; r < 3; r++) out.push(objs.map(get).join());
		  Q.k = "q"; delete P.k;
		  out.push(objs.map(get).join());
		  return out;`, `["1,2,p,,,3,4","1,2,p,,,3,4","1,2,p,,,3,4","1,2,,q,,3,4"]`},
		// A typed array answers its indices outside its shape, which it
		// shares with an ordinary object of its prototype.
		{`function get(o) { return o["1"]; }
		  function getK(o) { return o.k; }
		  var plain = Object.create(Uint8Array.prototype), ta = new Uint8Array([7, 8]);
		  return [get(plain), get(plain), get(ta), get(ta), getK(plain), getK(plain), getK(ta), getK(ta)];`, `[null,null,8,8,null,null,null,null]`},
		// Arguments objects, unmapped and mapped.
		{`function get(o) { return o.k; }
		  function args() { "use strict"; return arguments; }
		  function mapped(x) { return arguments; }
		  var A = args(1, 2), a = [get(A), get(A), A.length];
		  A.k = 3; a.push(get(A));
		  var M = mapped(4); a.push(get(M), get(M), M[0]);
		  M.k = 5; a.push(get(M));
		  return a;`, `[null,null,2,3,null,null,4,5]`},
		// Dictionary-mode receivers and prototypes take no absent entry.
		{`function get(o) { return o.k; }
		  function dict() { var o = {}; for (var i = 0; i < 100; i++) o["p" + i] = i; for (var i = 0; i < 90; i++) delete o["p" + i]; return o; }
		  var o = dict(), a = [get(o), get(o)];
		  o.k = 1; a.push(get(o));
		  var P = dict(), c = Object.create(P); a.push(get(c), get(c));
		  P.k = 2; a.push(get(c));
		  return a;`, `[null,null,1,null,null,2]`},
		// A bound function takes the prototype a proxy's trap gives.
		{`var P = {};
		  var bf = Function.prototype.bind.call(new Proxy(function () {}, {getPrototypeOf() { return P; }}), null);
		  function f(o) { return o.foo; }
		  var a = [f(bf), f(bf)];
		  P.foo = 44; a.push(f(bf));
		  delete P.foo; a.push(f(bf));
		  Object.setPrototypeOf(P, {foo: 45}); a.push(f(bf));
		  return a;`, `[null,null,44,null,45]`},
		// A string primitive's absent property.
		{`function get(s) { return s.nope; }
		  return [get("a"), get("b"), get("c")];`, `[null,null,null]`},
	})
}

// TestICAbsentMutable changes builtin prototypes, which a realm with
// shared intrinsics does not allow: a string primitive's property, an
// object whose prototype installs its properties on first use, and Error,
// the prototype of the other error constructors.
func TestICAbsentMutable(t *testing.T) {
	assert.Equal(t, ",,42,,45", runScripts(t, `function f(o) { return o.foo; }
var a = [f(TypeError), f(AggregateError)];
Error.foo = 42; a.push(f(TypeError));
delete Error.foo; a.push(f(AggregateError));
Object.setPrototypeOf(Error, {foo: 45}); a.push(f(RangeError)); a.join()`))
	assert.Equal(t, ",,12", runScripts(t, `function get(s) { return s.nope; }
var a = [get("a"), get("b")];
String.prototype.nope = 12; a.push(get("c")); a.join()`))
	assert.Equal(t, ",,function,13", runScripts(t, `function get(o) { return o.nope; }
var o = Object.create(Map.prototype), a = [get(o), get(o), typeof o.get];
Map.prototype.nope = 13; a.push(get(o)); a.join()`))
}

// TestICAbsentLazy: a host registers a lazy property on a prototype, on
// Object.prototype and on an object's own, after a site found its key
// absent there.
func TestICAbsentLazy(t *testing.T) {
	r := NewRealm()
	assert.Equal(t, ",,,", runScriptsIn(t, r, `var P = {}, o = Object.create(P);
function f(x) { return x.zz; }
function g(x) { return x.yy; }
[f({}), f({}), g(o), g(o)].join()`))
	r.ObjectPrototype.DefineLazyProperty(r, r.KeyFromGoString("zz"), func(*Realm) Value { return IntValue(11) })
	jsGlobal(t, r, "P").AsObject().DefineLazyProperties(r, []PropertyKey{r.KeyFromGoString("yy")},
		func(*Realm, int) Value { return IntValue(12) })
	assert.Equal(t, "11,12", runScriptsIn(t, r, `[f({}), g(o)].join()`))
}

// TestICAbsentNamespace: a module namespace object answers its exports
// outside its shape, as a receiver and as a prototype.
func TestICAbsentNamespace(t *testing.T) {
	r, _, err := evalGraph(t, map[string]string{
		"main": `import * as ns from "m";
function get(o) { return o.a; }
function getK(o) { return o.k; }
var o = Object.create(ns), out = [get(o), get(o), getK(o), getK(o), getK(ns), getK(ns)];
ns.setA(5); out.push(get(o), get(ns));
globalThis.out = out.join();`,
		"m": `export let a = 2; export function setA(v) { a = v; }`,
	}, "main")
	require.NoError(t, err)
	out, err := r.Global.GetProp(r, r.KeyFromGoString("out"))
	require.NoError(t, err)
	assert.Equal(t, "2,2,,,,,5,5", out.String())
}

// TestICAbsentEntry checks the entry an absent read fills, and that a host
// object on the chain, which defines its properties on first use, gets
// none until then.
func TestICAbsentEntry(t *testing.T) {
	r := NewRealm()
	assert.Equal(t, "true", runScriptsIn(t, r, `function get(o) { return o.k; }
var o = {x: 1}; get(o); get(o) === undefined`))
	o, err := r.Global.GetProp(r, r.KeyFromGoString("o"))
	require.NoError(t, err)
	absent := 0
	for _, e := range r.ic {
		if e.loc == icAbsent {
			absent++
			assert.Same(t, o.AsObject().shape, e.Shape)
			assert.Equal(t, r.protoEpoch|1, e.Epoch)
		}
	}
	assert.Equal(t, 1, absent)

	// An absent read does not replace the entry of a shape that has the
	// key, from the first read on or after it; a read of the key replaces
	// an absent entry.
	assert.Equal(t, "3,,3,,3|,3,,3", runScriptsIn(t, r, `function getK(o) { return o.k; }
function getK2(o) { return o.k; }
var has = {k: 3}, lacks = {y: 1};
[getK(has), getK(lacks), getK(has), getK(lacks), getK(has)].join() + "|" + [getK2(lacks), getK2(has), getK2(lacks), getK2(has)].join()`))
	shapeOf := func(name string) *Shape {
		v, err := r.Global.GetProp(r, r.KeyFromGoString(name))
		require.NoError(t, err)
		return v.AsObject().shape
	}
	hasShape, lacksShape := shapeOf("has"), shapeOf("lacks")
	hasEntries := 0
	for _, e := range r.ic {
		switch {
		case e.loc == icAbsent:
			assert.NotSame(t, lacksShape, e.Shape, "no absent entry for {y: 1}")
		case e.Shape == hasShape && e.HolderDepth() != icAdd: // not the literal's add entry
			hasEntries++
		}
	}
	assert.GreaterOrEqual(t, hasEntries, 2, "getK and getK2 keep the entry of {k: 3}")

	h, err := r.FromGo(map[string]any{"a": 1})
	require.NoError(t, err)
	require.NotZero(t, h.AsObject().flags&flagHasLazy)
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("host"), h))
	assert.Equal(t, ",,1,,14", runScriptsIn(t, r, `var p = Object.create(host), a = [get(p), get(p), p.a, get(p)];
host.k = 14; a.push(get(p)); a.join()`))
}

// TestGetFillIC reads objects of many kinds through one site, so that
// each read misses the entry the one before filled and takes getFillIC,
// or the generic path where that declines: own, inherited and absent data
// properties, getters own and inherited (one that changes its receiver),
// a holder in dictionary mode, a proxy as receiver and on the chain, the
// length of an array, a String object and a function, and built-in
// methods on their first read in the realm.
func TestGetFillIC(t *testing.T) {
	accTable(t, [][2]string{
		{`function get(o) { return o.p; }
		  var G = {get p() { return "g" + this.k; }}, D = {};
		  for (var i = 0; i < 70; i++) D["d" + i] = i;
		  D.p = "dict";
		  var objs = [{p: 1}, {q: 0, p: 2}, Object.create({p: 3}), Object.create(G, {k: {value: 4}}), {},
		    Object.create(null), Object.create(D), new Proxy({p: 5}, {}), Object.create(new Proxy({p: 6}, {})),
		    {get p() { this.z = 1; return 7; }}, Object.create(Object.create({p: 8}))];
		  var a = [];
		  for (var r = 0; r < 2; r++) for (var o of objs) a.push(get(o));
		  return a;`, `[1,2,3,"g4",null,null,"dict",5,6,7,8,1,2,3,"g4",null,null,"dict",5,6,7,8]`},
		{`function len(o) { return o.length; }
		  return [len([1, 2]), len(new String("abc")), len({length: 4}), len(function (a, b, c) {}), len([1, 2]), len(new String("abc"))];`,
			`[2,3,4,3,2,3]`},
		{`function m(o) { return typeof o.map + typeof o.toFixed + typeof o.at + typeof o.description; }
		  return [m([]), m(new Number(1)), m(Object(Symbol("s"))), m(new Map()), m([])];`,
			`["functionundefinedfunctionundefined","undefinedfunctionundefinedundefined","undefinedundefinedundefinedstring","undefinedundefinedundefinedundefined","functionundefinedfunctionundefined"]`},
	})
}

// TestGetFillICLazyInit: a lazy property's init that changes the chain
// the site reads through (the receiver's prototype, a property on an
// object below the holder) leaves no entry for the chain before it: the
// first read gets the property defined, the next ones what the chain
// holds now.
func TestGetFillICLazyInit(t *testing.T) {
	for _, c := range []struct {
		name, p, want string
		below         bool
	}{
		{"prototype past the holder's slots", "{p0: 0, p1: 1, p2: 2, p3: 3, p4: 4, p5: 5, p6: 6, p7: 7}", "77,Q,Q", false},
		{"prototype within them", "{p0: 0, p1: 1}", "77,Q,Q", false},
		{"property below the holder", "{p0: 0, p1: 1}", "2,1,1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := NewRealm()
			runScriptsIn(t, r, `var P = `+c.p+`, Q = {q0: "q0", q1: "q1", q2: "q2", k: "Q"};
var P1 = Object.create(P), o = Object.create(`+map[bool]string{false: "P", true: "P1"}[c.below]+`);
function rd(x) { return x.k; }`)
			o, p1, q := jsGlobal(t, r, "o").AsObject(), jsGlobal(t, r, "P1").AsObject(), jsGlobal(t, r, "Q").AsObject()
			init := func(r *Realm) Value {
				if c.below {
					_, err := p1.Set(r, InternKey("k"), IntValue(1), ObjectValue(p1))
					require.NoError(t, err)
					return IntValue(2)
				}
				o.SetPrototypeOf(r, q)
				return IntValue(77)
			}
			jsGlobal(t, r, "P").AsObject().DefineLazyProperty(r, InternKey("k"), init)
			assert.Equal(t, c.want, runScriptsIn(t, r, `[rd(o), rd(o), rd(o)].join()`))
		})
	}
}

package engine

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

// Assignments that add a property: the add entry of the set inline cache
// (setNamedAdd), the direct add at the end of OrdinarySet (createAbsent) and
// the slot room of constructed objects (constructThis). Each case runs the
// assignment site several times so that the later runs hit the entry the
// first one filled, then changes something the entry must not survive.
func TestSetAddStrict(t *testing.T) {
	accTable(t, [][2]string{
		// Plain adds through one site, several shapes.
		{`function P(x, y) { this.x = x; this.y = y; }
		  var a = []; for (var i = 0; i < 4; i++) { var p = new P(i, -i); a.push(p.x, p.y, Object.keys(p).join()); }
		  return a;`, `[0,0,"x,y",1,-1,"x,y",2,-2,"x,y",3,-3,"x,y"]`},
		{`function set(o) { o.k = 1; return o; }
		  return [set({}), set({a: 1}), set({}), set(Object.create(null)), set({a: 2})].map(o => Object.keys(o).join() + "=" + o.k);`,
			`["k=1","a,k=1","k=1","k=1","a,k=1"]`},
		// A setter defined on the prototype after the entry was filled.
		{`function P() { this.x = 1; }
		  var log = [];
		  new P(); new P();
		  Object.defineProperty(P.prototype, "x", {set(v) { log.push(v); }, configurable: true});
		  var p = new P();
		  return [log, Object.keys(p), p.x];`, `[[1],[],null]`},
		// A setter further up the chain, on a prototype added later.
		{`function P() { this.x = 1; }
		  new P(); new P();
		  var log = [];
		  var mid = {set x(v) { log.push("mid " + v); }};
		  Object.setPrototypeOf(P.prototype, mid);
		  var p = new P();
		  return [log, Object.keys(p)];`, `[["mid 1"],[]]`},
		// A read-only data property on the prototype rejects the assignment.
		{`function P() { this.x = 1; }
		  new P(); new P();
		  Object.defineProperty(P.prototype, "x", {value: 0, writable: false});
		  try { new P(); return "no throw"; } catch (e) { return e instanceof TypeError; }`, `true`},
		// A getter without setter rejects it.
		{`function P() { this.x = 1; }
		  new P(); new P();
		  Object.defineProperty(P.prototype, "x", {get() { return 7; }});
		  try { new P(); return "no throw"; } catch (e) { return e instanceof TypeError; }`, `true`},
		// A setter that throws: the exception propagates, nothing is added.
		{`function set(o) { o.x = 1; }
		  var p = {}; set(Object.create(p)); set(Object.create(p));
		  Object.defineProperty(p, "x", {set(v) { throw new RangeError("no " + v); }});
		  var o = Object.create(p);
		  try { set(o); return "no throw"; } catch (e) { return [e.name, e.message, Object.keys(o).length]; }`, `["RangeError","no 1",0]`},
		// A proxy on the chain runs its set trap.
		{`function set(o) { o.x = 1; }
		  var p = {}; set(Object.create(p)); set(Object.create(p));
		  var log = [];
		  Object.setPrototypeOf(p, new Proxy({}, {set(t, k, v, r) { log.push(k, v); return true; }}));
		  var o = Object.create(p); set(o);
		  return [log, Object.keys(o)];`, `[["x",1],[]]`},
		// An object made a proxy target's receiver: Reflect.set's receiver.
		{`function set(o) { o.x = 1; }
		  set({}); set({});
		  var log = [];
		  var px = new Proxy({}, {defineProperty(t, k, d) { log.push(k, d.value); return Reflect.defineProperty(t, k, d); }});
		  return [Reflect.set({}, "x", 2, px), log];`, `[true,["x",2]]`},
		// Non-extensible, sealed and frozen receivers with the filled shape.
		{`function set(o) { o.x = 1; }
		  set({}); set({});
		  var r = [];
		  for (var f of [Object.preventExtensions, Object.seal, Object.freeze]) {
		    var o = f({});
		    try { set(o); r.push("added"); } catch (e) { r.push(e instanceof TypeError, Object.keys(o).length); }
		  }
		  return r;`, `[true,0,true,0,true,0]`},
		// A prototype as the receiver: an inline cache below it sees the add.
		{`function set(o) { o.x = 1; }
		  set({}); set({});
		  var p = {}; var c = Object.create(p);
		  function get() { return c.x; }
		  var before = [get(), get()];
		  set(p);
		  return [before, get()];`, `[[null,null],1]`},
		// Delete and add again through the same site.
		{`function set(o, v) { o.x = v; }
		  var o = {a: 1}; set(o, 1); delete o.x; set(o, 2); delete o.x; set(o, 3);
		  return [o.x, Object.keys(o)];`, `[3,["a","x"]]`},
		// An array and a plain object with the same prototype: an array's
		// length stays the exotic one.
		{`function set(o, v) { o.length = v; }
		  var P = {}; var o1 = Object.create(P), o2 = Object.create(P);
		  set(o1, 1); set(o2, 2);
		  var a = Object.setPrototypeOf([1, 2, 3], P);
		  set(a, 1);
		  return [o1.length, o2.length, a.length, a[1], Object.keys(a), Array.isArray(a)];`, `[1,2,1,null,["0"],true]`},
		// A typed array and a plain object with the same prototype: numeric
		// string keys stay the typed array's.
		{`function set(o) { o["1.5"] = 9; o["-0"] = 9; }
		  var o1 = Object.create(Uint8Array.prototype), o2 = Object.create(Uint8Array.prototype);
		  set(o1); set(o2);
		  var ta = new Uint8Array(2); set(ta);
		  return [o1["1.5"], o2["-0"], ta["1.5"], ta["-0"], Object.keys(ta)];`, `[9,9,null,null,["0","1"]]`},
		// A typed array on the chain answers numeric string keys.
		{`function set(o) { o["1.5"] = 9; }
		  set({}); set({});
		  var o = Object.create(new Uint8Array(2)); set(o);
		  return [Object.keys(o), o["1.5"]];`, `[[],null]`},
		// The override of a frozen intrinsic's property makes an own one.
		{`class E extends Error { constructor(m) { super(m); this.name = "E"; } }
		  var a = [new E("1"), new E("2"), new E("3")];
		  return a.map(e => [e.name, e.message, Object.keys(e).join(), String(e)].join("|"));`,
			`["E|1|name|E: 1","E|2|name|E: 2","E|3|name|E: 3"]`},
		// Reflect.set with a different receiver: the receiver gets the property,
		// unless it is not extensible or a typed array with a numeric key.
		{`var t = {}, r1 = {}, r2 = Object.preventExtensions({}), ta = new Int8Array(1);
		  return [Reflect.set(t, "x", 1, r1), r1.x, Object.keys(t).length, Reflect.set(t, "x", 1, r2), Reflect.set(t, "-0", 1, ta), Reflect.set(t, "x", 1, ta), ta.x];`,
			`[true,1,0,false,false,true,1]`},
		// Indices: an ordinary object, an array, a non-writable length, a
		// non-extensible array.
		{`var o = {}; o[3] = 1; o[0] = 2;
		  var a = [1]; a[4] = 5;
		  var b = [1]; Object.defineProperty(b, "length", {writable: false});
		  var r = [Object.keys(o), a.length, a[4]];
		  try { b[3] = 1; r.push("no throw"); } catch (e) { r.push(e instanceof TypeError, b.length); }
		  b[0] = 7; r.push(b[0]);
		  var c = Object.preventExtensions([1, 2]);
		  try { c[2] = 1; r.push("no throw"); } catch (e) { r.push(e instanceof TypeError, c.length); }
		  return r;`, `[["0","3"],5,5,true,1,7,true,2]`},
		// Many properties through one constructor: past the co-allocated
		// room and into dictionary mode.
		{`function P(n) { for (var i = 0; i < n; i++) this["p" + i] = i; this.last = n; }
		  var r = [];
		  for (var n of [0, 1, 3, 9, 2, 70, 5]) { var p = new P(n); r.push(Object.keys(p).length, p.last, p["p" + (n - 1)]); }
		  return r;`, `[1,0,null,2,1,0,4,3,2,10,9,8,3,2,1,71,70,69,6,5,4]`},
		// Class fields, derived classes, Reflect.construct and a constructor
		// returning another object.
		{`class A { a = 1; constructor() { this.b = 2; } }
		  class B extends A { c = 3; constructor() { super(); this.d = 4; } }
		  function F() { this.f = 1; }
		  function G() { this.g = 1; return {other: true}; }
		  var r = [];
		  for (var i = 0; i < 3; i++) {
		    r.push(Object.keys(new A()).join(), Object.keys(new B()).join(), Object.keys(Reflect.construct(F, [], B)).join(), Object.keys(new G()).join());
		  }
		  return r;`, `["a,b","a,b,c,d","f","other","a,b","a,b,c,d","f","other","a,b","a,b,c,d","f","other"]`},
	})
}

// TestSetAddSloppy covers the sloppy-mode set path (setNamedSloppy): a
// rejected add is ignored, a setter added later runs.
func TestSetAddSloppy(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		got := runScriptsIn(t, r, `
			function P() { this.x = 1; }
			function set(o) { o.y = 2; return o; }
			new P(); new P(); set({}); set({});
			var log = [];
			Object.defineProperty(P.prototype, "x", {set: function (v) { log.push(v); }});
			var p = new P();
			var frozen = set(Object.freeze({}));
			var ne = set(Object.preventExtensions({}));
			try { Object.defineProperty(Object.prototype, "ro", {value: 0, writable: false, configurable: true}); } catch (e) {}
			function setRo(o) { o.ro = 1; return o; }
			var q = setRo({});
			delete Object.prototype.ro;
			var q2 = setRo({});
			globalThis.newGlobal = 5;
			[log, Object.keys(p), Object.keys(frozen), Object.keys(ne), Object.keys(q), q2.ro, newGlobal].join(";")`)
		if shared {
			// Object.prototype is a frozen intrinsic: the definition
			// throws, so ro never exists.
			assert.Equal(t, "1;;;;ro;1;5", got)
		} else {
			assert.Equal(t, "1;;;;;1;5", got)
		}
	}
}

// TestSetAddFills checks that the cases above take the paths they are
// about: an assignment that adds fills an add entry, and the second object
// a constructor makes has room for the properties of the first.
func TestSetAddFills(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	got := runScriptsIn(t, r, `"use strict";
		function P(x) { this.x = x; this.y = x; this.z = x; }
		var a = new P(1), b = new P(2);
		b.x + b.y + b.z`)
	assert.Equal(t, "6", got)
	adds := 0
	for i := range r.ic {
		if e := &r.ic[i]; e.Shape != nil && e.HolderDepth() == icAdd && e.Epoch == r.protoEpoch {
			adds++
		}
	}
	assert.Equal(t, 3, adds, "add entries")
	assert.Equal(t, 4, cap(jsGlobal(t, r, "b").AsObject().slots), "the second one has room for three in an object4")
	assert.Equal(t, uint8(3), jsGlobal(t, r, "P").AsObject().FunctionData().ctorSlots&ctorSlotsMask)
}

// TestSetAbsentIndex covers writes of indices an object lacks
// (setAbsentIndex) and Array.prototype.fill growing an array's storage: an
// index on a prototype (data, setter, read-only, a proxy, a typed array, a
// String wrapper) keeps the generic path, as do non-extensible, sealed and
// frozen arrays, a non-writable length and sparse arrays.
func TestSetAbsentIndex(t *testing.T) {
	accTable(t, [][2]string{
		// Holes inside the storage, the index right after it, a gap.
		{`var a = [1, , 3]; a[1] = 2; a[3] = 4; a[10] = 11;
		  var b = new Array(5000); for (var i = 0; i < 5000; i++) b[i] = i;
		  return [a, a.length, 1 in a, 7 in a, b.length, b[4999], Object.keys(b).length];`,
			`[[1,2,3,4,null,null,null,null,null,null,11],11,true,false,5000,4999,5000]`},
		{`var o = {}; o[0] = "a"; o[1] = "b"; o[5] = "f"; var p = {0: 1}; delete p[0]; p[0] = 2;
		  return [o, Object.keys(o), p[0]];`, `[{"0":"a","1":"b","5":"f"},["0","1","5"],2]`},
		// -0, NaN, Infinity and huge values as stored values and as keys.
		{`var a = []; a[0] = -0; a[1] = NaN; a[2] = Infinity; a[3] = 1e308; a[-0] = 7;
		  a["4294967294"] = 1; a["4294967295"] = 2;
		  return [Object.is(a[0], 7), a.length, Object.keys(a).slice(0, 5), a["4294967295"]];`,
			`[true,4294967295,["0","1","2","3","4294967294"],2]`},
		// An index on a user prototype: data, setter, read-only, proxy,
		// typed array, String wrapper.
		{`function make(p) { var a = [0, , 2]; Object.setPrototypeOf(a, p); return a; }
		  var log = [];
		  var a1 = make(Object.setPrototypeOf({1: "inherited"}, Array.prototype)); a1[1] = 5;
		  var a2 = make(Object.setPrototypeOf({set 1(v) { log.push("set " + v); }}, Array.prototype)); a2[1] = 6;
		  var ro = Object.setPrototypeOf({}, Array.prototype); Object.defineProperty(ro, 1, {value: "ro"});
		  var a3 = make(ro); var threw = false; try { a3[1] = 7; } catch (e) { threw = e instanceof TypeError; }
		  var a4 = make(new Proxy({}, {set(t, k, v) { log.push("trap " + k + "=" + v); return true; }})); a4[1] = 8;
		  var a5 = make(new Uint8Array(4)); a5[1] = 9; a5[3] = 10;
		  var a6 = make(new String("xyz")); var threw6 = false; try { a6[1] = 11; } catch (e) { threw6 = e instanceof TypeError; }
		  return [a1[1], Object.keys(a1), log, threw, a3[1], Object.keys(a4), Object.keys(a5), a5[1], threw6, a6[1]];`,
			`[5,["0","1","2"],["set 6","trap 1=8"],true,"ro",["0","2"],["0","1","2","3"],9,true,"y"]`},
		// Non-extensible, sealed, frozen arrays; a non-writable length.
		{`var r = [];
		  for (var f of [Object.preventExtensions, Object.seal, Object.freeze]) {
		    var a = f([0, , 2]);
		    try { a[1] = 1; r.push("hole written"); } catch (e) { r.push(e instanceof TypeError, 1 in a); }
		    try { a[3] = 1; r.push("appended"); } catch (e) { r.push(e instanceof TypeError, a.length); }
		  }
		  var b = [0, , 2]; Object.defineProperty(b, "length", {writable: false});
		  b[1] = 1; r.push(b[1]);
		  try { b[3] = 1; r.push("appended"); } catch (e) { r.push(e instanceof TypeError, b.length); }
		  return r;`, `[true,false,true,3,true,false,true,3,true,false,true,3,1,true,3]`},
		// A sparse array: an index whose element was redefined lives in the
		// sparse map, with the hole in the storage.
		{`var a = [0, 1, 2]; Object.defineProperty(a, 1, {value: 1, writable: false});
		  var threw = false; try { a[1] = 5; } catch (e) { threw = e instanceof TypeError; }
		  a[3] = 3;
		  return [threw, a, Object.getOwnPropertyDescriptor(a, 1).writable];`, `[true,[0,1,2,3],false]`},
		// A prototype array gaining an index: reads through it see it.
		{`var p = []; var c = Object.create(p);
		  function get() { return c[0]; }
		  var before = get(); p[0] = "now"; return [before, get()];`, `[null,"now"]`},
		// fill over holes and past the storage of new Array(n).
		{`var a = new Array(3000).fill(true); var b = new Array(5).fill(0, 2); var c = new Array(3000).fill(1, 10, 20);
		  var d = [1, , 3, , 5]; d.length = 8; d.fill(0, 1);
		  return [a.length, a[2999], Object.keys(a).length, b, Object.keys(c).length, c[10], c[19], 9 in c, d];`,
			`[3000,true,3000,[null,null,0,0,0],10,1,1,false,[1,0,0,0,0,0,0,0]]`},
		{`var log = [];
		  var p = Object.setPrototypeOf({set 2500(v) { log.push(v); }}, Array.prototype);
		  var a = new Array(3000); Object.setPrototypeOf(a, p); a.fill(7);
		  var f = Object.freeze(new Array(3000)); var threw = false; try { f.fill(1); } catch (e) { threw = e instanceof TypeError; }
		  var s = Object.seal(new Array(3000)); var threws = false; try { s.fill(1); } catch (e) { threws = e instanceof TypeError; }
		  return [log, a.hasOwnProperty(2500), a[2499], threw, threws, 0 in f];`, `[[7],false,7,true,true,false]`},
		// A valueOf of start or end that shrinks the array.
		{`var a = new Array(3000);
		  a.fill(1, {valueOf() { a.length = 10; return 0; }}, 3000);
		  return [a.length, a[9], a[2999]];`, `[3000,1,1]`},
	})
}

// TestSetAbsentIndexArrayPrototype defines an index setter on
// Array.prototype, which only a mutable realm allows.
func TestSetAbsentIndexArrayPrototype(t *testing.T) {
	body := `var log = "";
	  try { Object.defineProperty(Array.prototype, 1, {set(v) { log += v; }, configurable: true}); } catch (e) { log = e.name; }
	  var a = [0, , 2]; a[1] = 9; var b = []; b[0] = 0; b[1] = 8;
	  var c = new Array(3000).fill(5);
	  try { delete Array.prototype[1]; } catch (e) {}
	  var d = [0, , 2]; d[1] = 7;
	  return [log, a[1], b.length, c.hasOwnProperty(1), c[2], d[1]];`
	assert.Equal(t, `["985",null,1,false,5,7]`, accEval(t, false, body))
	assert.Equal(t, `["TypeError",9,2,true,5,7]`, accEval(t, true, body))
}

// TestCtorRoomPastConstruction: the objects of a constructor that most of
// them outgrow right after it returns, as a tree's nodes take their
// children, are given the room the ones before took from the ctorProbe-th
// on, in their own allocation, and keep it as they take those properties.
// A constructor whose objects stay as constructed, or of which only a few
// go on, gives no more than their construction takes.
func TestCtorRoomPastConstruction(t *testing.T) {
	r := NewRealm()
	got := runScriptsIn(t, r, `
		function N(k, v) { this.key = k; this.value = v; }
		N.prototype.left = null; N.prototype.right = null;
		var prev = null, late;
		for (var i = 0; i < 40; i++) {
			var n = new N(i, i);
			if (i & 1) { n.left = prev; n.right = null; } else { n.right = prev; n.left = null; }
			prev = n;
			if (i == 31) late = n;
		}
		function P(x) { this.x = x; this.y = x; }
		var ps = []; for (var i = 0; i < 40; i++) ps.push(new P(i));
		function Q(x) { this.x = x; this.y = x; }
		var qs = []; for (var i = 0; i < 40; i++) { var q = new Q(i); if (i % 4 == 0) q.z = i; qs.push(q); }
		[Object.keys(late).join(), late.left.key, late.right, qs[8].z, Object.keys(qs[9]).join()].join(";")`)
	assert.Equal(t, "key,value,left,right;30;;8;x,y", got)
	room := func(name string) uint8 {
		return jsGlobal(t, r, name).AsObject().FunctionData().ctorSlots & ctorSlotsMask
	}
	assert.Equal(t, uint8(4), room("N"), "nodes take their children")
	assert.Equal(t, uint8(2), room("P"), "objects that stay as constructed")
	assert.Equal(t, uint8(2), room("Q"), "a quarter go on")
	late := jsGlobal(t, r, "late").AsObject()
	assert.Equal(t, 4, len(late.slots))
	assert.Equal(t, uintptr(unsafe.Pointer(late))+unsafe.Sizeof(Object{}), uintptr(unsafe.Pointer(&late.slots[0])), "slots in the object's own allocation")
}

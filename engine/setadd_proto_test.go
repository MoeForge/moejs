package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSetAddThroughPrototype warms a write site that adds a property to
// fresh objects whose prototype chain has it as a writable data property
// (setNamedAdd caches the add, protoChainAdds), then changes the chain and
// writes once more: the write must follow OrdinarySet on the changed chain,
// in sloppy and in strict code. The expected results are Node 24's.
func TestSetAddThroughPrototype(t *testing.T) {
	type addCase struct {
		name, setup, change, key string
		sloppy, strict           string
	}
	cases := []addCase{
		{name: "receiver setPrototypeOf",
			setup:  `var Base = {x: 1}, P = Object.create(Base), Q = Object.defineProperty({}, 'x', {value: 2, writable: false});`,
			change: `var o = mk(); Object.setPrototypeOf(o, Q);`,
			sloppy: "ok own=false v=2 log=", strict: "TypeError own=false v=2 log="},
		{name: "chain setPrototypeOf",
			setup:  `var Base = {x: 1}, P = Object.create(Base), Q = Object.defineProperty({}, 'x', {value: 2, writable: false});`,
			change: `Object.setPrototypeOf(P, Q);`,
			sloppy: "ok own=false v=2 log=", strict: "TypeError own=false v=2 log="},
		{name: "non-writable",
			setup:  `var Base = {x: 1}, P = Object.create(Base);`,
			change: `Object.defineProperty(Base, 'x', {writable: false});`,
			sloppy: "ok own=false v=1 log=", strict: "TypeError own=false v=1 log="},
		{name: "non-writable in a dictionary",
			setup:  `var Base = {a: 1, x: 1, b: 2, c: 3}, P = Object.create(Base); delete Base.a;`,
			change: `Object.defineProperty(Base, 'x', {writable: false});`,
			sloppy: "ok own=false v=1 log=", strict: "TypeError own=false v=1 log="},
		{name: "accessor",
			setup:  `var Base = {x: 1}, P = Object.create(Base);`,
			change: `Object.defineProperty(Base, 'x', {get: function () { return 'got'; }, set: function (v) { log.push('set ' + v + ' ' + (this === o)); }});`,
			sloppy: "ok own=false v=got log=set 99 true", strict: "ok own=false v=got log=set 99 true"},
		{name: "freeze",
			setup:  `var Base = {x: 1}, P = Object.create(Base);`,
			change: `Object.freeze(Base);`,
			sloppy: "ok own=false v=1 log=", strict: "TypeError own=false v=1 log="},
		{name: "delete",
			setup: `var Top = Object.defineProperty({}, 'x', {get: function () { return 'top'; }, set: function (v) { log.push('top ' + v + ' ' + (this === o)); }}),
				Base = Object.defineProperty(Object.create(Top), 'x', {value: 1, writable: true, enumerable: true, configurable: true}), P = Object.create(Base);`,
			change: `delete Base.x;`,
			sloppy: "ok own=false v=top log=top 99 true", strict: "ok own=false v=top log=top 99 true"},
		{name: "proxy",
			setup:  `var Base = {x: 1}, P = Object.create(Base);`,
			change: `Object.setPrototypeOf(P, new Proxy(Base, {set: function (t, k, v, recv) { log.push('trap ' + String(k) + '=' + v + ' ' + (recv === o)); return true; }}));`,
			sloppy: "ok own=false v=1 log=trap x=99 true", strict: "ok own=false v=1 log=trap x=99 true"},
		{name: "array",
			setup:  `var Base = {x: 1}, P = Object.create(Base), A = []; Object.defineProperty(A, 'x', {value: 7, writable: false});`,
			change: `Object.setPrototypeOf(P, A);`,
			sloppy: "ok own=false v=7 log=", strict: "TypeError own=false v=7 log="},
		{name: "array length", key: "length",
			setup:  `var Base = [1, 2], P = Object.create(Base);`,
			change: `Object.defineProperty(Base, 'length', {writable: false});`,
			sloppy: "ok own=false v=2 log=", strict: "TypeError own=false v=2 log="},
		{name: "typed array", key: "Infinity",
			setup:  `var Base = {Infinity: 1}, P = Object.create(Base);`,
			change: `Object.setPrototypeOf(P, new Uint8Array(4));`,
			sloppy: "ok own=false v=undefined log=", strict: "ok own=false v=undefined log="},
		{name: "global", key: "gx",
			setup:  `var gx = 1; var Base = globalThis, P = Object.create(Base);`,
			change: `Object.defineProperty(globalThis, 'gx', {writable: false});`,
			sloppy: "ok own=false v=1 log=", strict: "TypeError own=false v=1 log="},
	}
	for _, tc := range cases {
		key := tc.key
		if key == "" {
			key = "x"
		}
		for _, strict := range []bool{false, true} {
			name, want, prologue := tc.name+" sloppy", tc.sloppy, ""
			if strict {
				name, want, prologue = tc.name+" strict", tc.strict, "'use strict';\n"
			}
			src := prologue + `var key = '` + key + `', log = [], o;
` + tc.setup + `
function mk() { return Object.create(P); }
function w(o, v) { o.` + key + ` = v; }
for (var i = 0; i < 20; i++) {
  o = mk(); w(o, i);
  if (!Object.prototype.hasOwnProperty.call(o, key) || o[key] !== i) throw new Error('warm ' + i);
}
o = mk();
` + tc.change + `
var r; try { w(o, 99); r = 'ok'; } catch (e) { r = e.constructor.name; }
r + ' own=' + Object.prototype.hasOwnProperty.call(o, key) + ' v=' + String(o[key]) + ' log=' + log.join(',');`
			t.Run(name, func(t *testing.T) {
				assert.Equal(t, want, runScripts(t, src))
			})
		}
	}
}

// TestSloppyThisAfterObjectThis calls a sloppy function that reads this
// with an object until run's CoerceThis case is warm, then with each
// primitive, undefined and null: they still get the global object or a
// wrapper, and a strict function's this stays as it is.
func TestSloppyThisAfterObjectThis(t *testing.T) {
	src := `
function f() { return this; }
function s() { 'use strict'; return this; }
var obj = {}, out = [];
for (var i = 0; i < 10; i++) if (f.call(obj) !== obj) throw new Error('object this');
out.push(f() === globalThis, f.call(undefined) === globalThis, f.call(null) === globalThis);
var n = f.call(5), b = f.call(true), str = f.call('ab'), sym = Symbol('q'), y = f.call(sym), big = f.call(7n);
out.push(typeof n, n instanceof Number, n.valueOf(), typeof b, b.valueOf(), typeof str, str.length);
out.push(typeof y, y.valueOf() === sym, typeof big, big.valueOf() === 7n);
out.push(s.call(5), String(s.call(undefined)), String(s.call(null)));
var m = {g: f}; out.push(m.g() === m);
out.join(' ');`
	assert.Equal(t, "true true true object true 5 object true object 2 object true object true 5 undefined null true", runScripts(t, src))
}

// TestSloppyElementWrites runs sloppy computed writes that run's SetElem
// case shares with strict code: past the fast paths, a rejected write is
// ignored instead of throwing, and a typed array converts the value.
func TestSloppyElementWrites(t *testing.T) {
	src := `
var a = [1, 2, 3], out = [];
function w(o, k, v) { o[k] = v; }
for (var i = 0; i < 3; i++) w(a, i, i * 10);
out.push(a.join());
var fz = Object.freeze([1, 2]); w(fz, 0, 9); w(fz, 5, 9); out.push(fz.join(), fz.length);
var ro = Object.defineProperty({}, 'k', {value: 1, writable: false}); w(ro, 'k', 2); out.push(ro.k);
var ta = new Int8Array(2); w(ta, 0, 300); w(ta, 1, '7'); w(ta, 5, 1); out.push(ta.join(), String(ta[5]));
var h = [, 1]; w(h, 0, 'x'); w(h, 4, 'y'); out.push(h.join(), h.length);
try { w(undefined, 0, 1); } catch (e) { out.push(e.constructor.name); }
out.join(' ');`
	assert.Equal(t, "0,10,20 1,2 2 1 44,7 undefined x,1,,,y 5 TypeError", runScripts(t, src))
}

// TestSloppyGlobalWrites warms global writes of sloppy and strict
// functions, which are both SetGlobal, then makes each write slow: a
// sloppy one creates the name or ignores a rejected write, a strict one
// throws, and a setter or a function called from code of the other kind
// keeps its own. The expected results are Node 24's.
func TestSloppyGlobalWrites(t *testing.T) {
	src := `
var out = [];
function w(v) { g1 = v; }
function ws(v) { 'use strict'; g1 = v; }
for (var i = 0; i < 10; i++) w(i);
out.push(g1, Object.getOwnPropertyDescriptor(globalThis, 'g1').enumerable);
Object.defineProperty(globalThis, 'g1', {writable: false});
w(50); out.push(g1);
try { ws(51); out.push('no'); } catch (e) { out.push(e.constructor.name); }
Object.defineProperty(globalThis, 'g2', {get: function () { return 'got'; }, configurable: true});
function w2(v) { g2 = v; }
function w2s(v) { 'use strict'; g2 = v; }
for (var i = 0; i < 5; i++) w2(i);
try { w2s(1); out.push('no'); } catch (e) { out.push(e.constructor.name); }
Object.defineProperty(globalThis, 'g2', {set: function (v) { out.push('set ' + v + ' ' + (this === globalThis)); }, configurable: true});
w2(7); w2s(8);
function w3(v) { g3 = v; }
function w3s(v) { 'use strict'; g3 = v; }
for (var i = 0; i < 5; i++) w3(i);
delete globalThis.g3;
try { w3s(1); out.push('no'); } catch (e) { out.push(e.constructor.name); }
w3(9); out.push(g3);
var o = {set p(v) { 'use strict'; w(v); g4 = v; }};
try { o.p = 3; out.push('no'); } catch (e) { out.push(e.constructor.name, typeof g4); }
var s = {set p(v) { g5 = v; }};
(function () { 'use strict'; s.p = 4; })(); out.push(g5);
Object.preventExtensions(globalThis);
w(1); function w6(v) { g6 = v; } w6(2); out.push(typeof g6);
try { (function () { 'use strict'; g7 = 1; })(); } catch (e) { out.push(e.constructor.name); }
out.join(' ');`
	assert.Equal(t, "9 true 9 TypeError TypeError set 7 true set 8 true ReferenceError 9 ReferenceError undefined 4 undefined ReferenceError", runScripts(t, src))
	src = `
let gl = 1; const gc = 2;
var out = [];
function wl(v) { gl = v; }
function wc(v) { gc = v; }
for (var i = 0; i < 5; i++) wl(i);
out.push(gl);
try { wc(1); out.push('no'); } catch (e) { out.push(e.constructor.name); }
x9 = 1; out.push(Object.getOwnPropertyDescriptor(globalThis, 'x9').configurable);
out.join(' ');`
	assert.Equal(t, "4 TypeError true", runScripts(t, src))
}

// TestInstanceOfPlain warms instanceof against ordinary functions, whose
// prototype instanceOfPlain reads from the slot, then changes what it
// depends on: the prototype is read each time, an own or inherited
// @@hasInstance is called, a bound function, a proxy on the chain, a chain
// past the walk's bound, a frozen function and the lazy prototype go
// through InstanceofOperator. The expected results are Node 24's.
func TestInstanceOfPlain(t *testing.T) {
	src := `
var out = [], log = [];
function F() {} function G() {}
var o = new F(), q = {};
function t(v, C) { return v instanceof C; }
for (var i = 0; i < 20; i++) if (!t(o, F) || t(q, F)) throw new Error('warm');
out.push(t(5, F), t('s', F), t(null, F), t(undefined, F));
var P2 = {}; F.prototype = P2; out.push(t(o, F), t(Object.create(P2), F));
F.prototype = 5; try { t(o, F); out.push('no'); } catch (e) { out.push(e.constructor.name + ': ' + e.message); }
F.prototype = Object.create(null); out.push(t(Object.create(F.prototype), F), t(o, F));
out.push(t({}, G), typeof G.prototype, t(new G(), G));
function H() {} var h = new H();
Object.defineProperty(H, Symbol.hasInstance, {value: function (v) { log.push('own'); return v === 1; }});
out.push(t(1, H), t(h, H));
function K() {} var k = new K();
Object.setPrototypeOf(K, Object.create(Function.prototype, {[Symbol.hasInstance]: {value: function () { log.push('proto'); return 'yes'; }}}));
out.push(t(k, K), t({}, K));
function M() {} var m = new M(), B = M.bind(null);
out.push(t(m, B), t({}, B));
function N() {} var tr = new Proxy(N.prototype, {getPrototypeOf: function (x) { log.push('trap'); return Reflect.getPrototypeOf(x); }});
var pv = Object.create(Object.create(tr));
out.push(t(pv, N), t(Object.create(tr), Object));
var pr = new Proxy({}, {getPrototypeOf: function () { log.push('self'); return N.prototype; }});
out.push(t(pr, N));
var c = N.prototype; for (var j = 0; j < 100; j++) c = Object.create(c);
out.push(t(c, N), t(c, F));
Object.freeze(N); out.push(t(c, N), t({}, N));
out.push(t(o, Object), t(o, Function), t(function () {}, Function));
try { t(o, function () {}.bind()); out.push('bound ok'); } catch (e) { out.push(e.constructor.name); }
try { t(o, () => 1); out.push('no'); } catch (e) { out.push(e.constructor.name + ': ' + e.message); }
try { t(o, {}); out.push('no'); } catch (e) { out.push(e.constructor.name + ': ' + e.message); }
class C {} out.push(t(new C(), C), t({}, C));
function* gen() {} out.push(t(gen(), gen));
out.push(log.join());
out.join(' ');`
	assert.Equal(t, "false false false false false true TypeError: Function has non-object prototype '5' in instanceof check true false false object true true false true true true false false true true true false true false true false true bound ok TypeError: Function has non-object prototype 'undefined' in instanceof check TypeError: Right-hand side of 'instanceof' is not callable true false true own,own,proto,proto,trap,trap,self", runScripts(t, src))
}

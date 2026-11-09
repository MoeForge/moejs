package engine

import (
	"strings"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/stretchr/testify/assert"
)

// TestPolyGet reads a property at sites that see several shapes, which
// turn polymorphic (icpoly.go): own and inherited data properties, a
// getter, an absent property, a dictionary object, a proxy, string
// primitives among objects, methods through prototypes, length and index
// keys, then changes the prototypes behind the cached entries. The
// expected results are Node 24's.
func TestPolyGet(t *testing.T) {
	src := `var out = [];
function A() { this.x = 1; this.y = 2; }
function B() { this.y = 3; this.x = 4; }
function C() { this.z = 0; this.x = 5; }
function D() {} D.prototype.x = 6;
function E() { this.w = 1; } E.prototype = Object.create(D.prototype);
var gets = 0;
var F = {get x() { gets++; return 'g' + gets; }};
var G = {other: 1};
var objs = [new A(), new B(), new C(), new D(), new E(), F, G];
function rx(o) { return o.x; }
for (var i = 0; i < 50; i++) for (var j = 0; j < objs.length; j++) rx(objs[j]);
out.push(objs.map(rx).join());
D.prototype.x = 'd2'; out.push(objs.map(rx).join());
Object.defineProperty(D.prototype, 'x', {get: function () { return 'dg'; }, configurable: true});
out.push(objs.map(rx).join());
Object.prototype.x = 'op'; out.push(objs.map(rx).join());
delete Object.prototype.x; delete D.prototype.x; out.push(objs.map(rx).join());
var d = {a: 1, x: 'dict'}; delete d.a; out.push(rx(d), objs.map(rx).join());
var p = new Proxy({}, {get: function (t, k) { return 'proxy ' + String(k); }}); out.push(rx(p), objs.map(rx).join());
String.prototype.x = 'sp';
out.push(rx('abc'), rx(objs[0]), rx('d'), rx(new String('q')), rx(objs[3]), rx(5));
for (var i = 0; i < 20; i++) out.push(rx(i % 2 ? 'abc' : objs[i % objs.length]));
delete String.prototype.x; out.push(rx('abc'), rx(objs[1]));
function m(o) { return o.f(); }
var k1 = {f: function () { return 'k1'; }}, k2 = {a: 0, f: function () { return 'k2'; }};
function K() {} K.prototype.f = function () { return 'K'; };
var k3 = new K(), k4 = Object.create(k3);
for (var i = 0; i < 30; i++) out.push(m([k1, k2, k3, k4][i % 4]));
K.prototype.f = function () { return 'K2'; }; out.push(m(k3), m(k4), m(k1));
var lens = ['ab', 'cde', [1, 2, 3, 4], {length: 9}, new String('xyz'), Object.assign([], {length: 2})];
function ln(o) { return o['len' + 'gth']; }
out.push(lens.map(ln).join());
function g0(o) { return o[0]; }
out.push(['ab', [7], {0: 'z'}, new Uint8Array([5])].map(g0).join());
var sv = 'a';
function idx(o) { return o['1']; }
out.push(['ab', [7, 8], {1: 'z'}, new Uint8Array([5, 6])].map(idx).join());
function Q() {} Q.prototype.x = 'Qp'; String.prototype.x = 'S';
var q = [{x: 'q1'}, {a: 0, x: 'q2'}, new Q(), 'str'];
function r2(o) { return o.x; }
for (var i = 0; i < 20; i++) r2(q[i % 4]);
out.push(q.map(r2).join());
Object.defineProperty(Q.prototype, 'x', {get: function () { return 'Qg'; }, configurable: true});
out.push(q.map(r2).join(), q.map(r2).join());
Object.defineProperty(String.prototype, 'x', {get: function () { return 'Sg' + this.length; }, configurable: true});
out.push(q.map(r2).join(), q.map(r2).join());
delete String.prototype.x; out.push(q.map(r2).join());
out.join(' ');`
	assert.Equal(t, "1,4,5,6,6,g51, 1,4,5,d2,d2,g52, 1,4,5,dg,dg,g53, 1,4,5,dg,dg,g54,op 1,4,5,,,g55, dict 1,4,5,,,g56, proxy x 1,4,5,,,g57, sp 1 sp sp   1 sp 5 sp  sp  sp 4 sp  sp g58 sp 1 sp 5 sp  sp  4 k1 k2 K K k1 k2 K K k1 k2 K K k1 k2 K K k1 k2 K K k1 k2 K K k1 k2 K K k1 k2 K2 K2 k1 2,3,4,9,3,2 a,7,z,5 b,8,z,6 q1,q2,Qp,S q1,q2,Qg,S q1,q2,Qg,S q1,q2,Qg,Sg3 q1,q2,Qg,Sg3 q1,q2,Qg,", runScriptsIn(t, polyRealm(), src))
}

// TestPolySet writes a property at sites that see several shapes: own data
// properties, an inherited setter, adds to objects of two prototypes, then
// a setter and a read-only property on one of the prototypes, frozen and
// non-extensible objects, an array, a proxy, a dictionary object and a
// setter on Object.prototype, in sloppy and strict functions. The expected
// results are Node 24's.
func TestPolySet(t *testing.T) {
	src := `var out = [], log = [];
function w(o, v) { o.x = v; }
function ws(o, v) { 'use strict'; o.x = v; }
function P1() {} function P2() {} P2.prototype.q = 1;
function A() { this.x = 0; } function B() { this.a = 0; this.x = 0; }
var S = {set x(v) { log.push('set ' + v + ' ' + (this === s1)); }};
var s1 = Object.create(S);
var objs = [new A(), new B(), {x: 0, b: 1}, {c: 2, x: 0}, s1];
for (var i = 0; i < 40; i++) { w(objs[i % 5], i); ws(objs[(i + 1) % 5], -i); }
out.push(objs.slice(0, 4).map(function (o) { return o.x; }).join(), log.length, log[log.length - 1]);
function mk(P) { var o = new P(); w(o, 7); return o; }
for (var i = 0; i < 30; i++) { var o = mk(i % 2 ? P1 : P2); if (o.x !== 7 || !o.hasOwnProperty('x')) throw new Error('add ' + i); }
out.push(Object.keys(mk(P1)).join(), Object.keys(mk(P2)).join());
Object.defineProperty(P2.prototype, 'x', {set: function (v) { log.push('p2 ' + v); }, configurable: true});
var o2 = mk(P2); out.push(o2.hasOwnProperty('x'), log[log.length - 1]);
var o1 = mk(P1); out.push(o1.hasOwnProperty('x'), o1.x);
Object.defineProperty(P1.prototype, 'x', {value: 'ro', writable: false, configurable: true});
var o3 = mk(P1); out.push(o3.hasOwnProperty('x'), o3.x);
try { var o4 = new P1(); ws(o4, 1); out.push('no'); } catch (e) { out.push(e.constructor.name); }
delete P1.prototype.x; delete P2.prototype.x;
var fz = Object.freeze({x: 1}), ne = Object.preventExtensions({y: 1});
for (var i = 0; i < 10; i++) { w(objs[i % 4], i); w(fz, 9); w(ne, 9); }
out.push(fz.x, ne.x, ne.hasOwnProperty('x'));
try { ws(fz, 2); out.push('no'); } catch (e) { out.push(e.constructor.name); }
try { ws(ne, 2); out.push('no'); } catch (e) { out.push(e.constructor.name); }
var arr = [1, 2]; w(arr, 'ax'); out.push(arr.x);
var px = new Proxy({}, {set: function (t, k, v) { log.push('trap ' + String(k) + v); t[k] = v; return true; }}); w(px, 5); out.push(log[log.length - 1], px.x);
var d = {a: 1, x: 1}; delete d.a; w(d, 'dd'); out.push(d.x);
for (var i = 0; i < 20; i++) w(objs[i % 5], 'r' + i);
out.push(objs.slice(0, 4).map(function (o) { return o.x; }).join(), log[log.length - 1]);
Object.defineProperty(Object.prototype, 'x', {set: function (v) { log.push('op ' + v); }, configurable: true});
var nobj = {n: 1}; w(nobj, 'z'); out.push(nobj.hasOwnProperty('x'), log[log.length - 1]);
w(objs[0], 'own'); out.push(objs[0].x);
delete Object.prototype.x;
function ww(o, v) { o.x = v; }
function T() {} var t3 = new T(); t3.x = 0;
var tt = [{x: 0}, {a: 0, x: 0}, t3];
for (var i = 0; i < 20; i++) ww(tt[i % 3], i);
out.push(tt.map(function (o) { return o.x; }).join());
Object.defineProperty(T.prototype, 'y', {value: 1, configurable: true});
for (var i = 0; i < 6; i++) ww(tt[i % 3], 'e' + i);
out.push(tt.map(function (o) { return o.x; }).join());
var tw = {set x(v) { log.push('tw ' + v); }}; tt.push(tw);
for (var i = 0; i < 8; i++) ww(tt[i % 4], 'f' + i);
Object.defineProperty(tw, 'x', {set: function (v) { log.push('tw2 ' + v); }});
for (var i = 0; i < 4; i++) ww(tt[i % 4], 'g' + i);
out.push(tt.slice(0, 3).map(function (o) { return o.x; }).join(), log.slice(-3).join());
out.join(' ');`
	assert.Equal(t, "-39,36,37,38 16 set 39 true x x false p2 7 true 7 false ro TypeError 1  false TypeError TypeError ax trap x5 5 dd r15,r16,r17,r18 set r19 true false op z own 18,19,17 e3,e4,e5 g0,g1,g2 tw f3,tw f7,tw2 g3", runScriptsIn(t, polyRealm(), src))
}

// TestPolyCollide reads and writes x at 80 sites of three shapes each in a
// polyTable whose even slots other sites hold, so that the polyICs of the
// sites whose first slot is even are past it, and getPropSlow and
// setPropSlow take them through findNext.
func TestPolyCollide(t *testing.T) {
	r := NewRealm()
	pt := &polyTable{slots: make([]polySlot, 256)}
	for j := 0; j < len(pt.slots); j += 2 {
		pt.slots[j].site = uint32(j) | 1<<24
	}
	r.lazyState().poly = pt
	src := `var objs = [{x: 0}, {a: 0, x: 0}, {b: 0, c: 0, x: 0}], fs = [], bad = 0;
` + strings.Repeat("fs.push([function (o) { return o.x; }, function (o, v) { o.x = v; }]);\n", 40) + `
for (var i = 0; i < 50; i++) for (var n = 0; n < fs.length; n++) {
  var o = objs[(i + n) % 3]; fs[n][1](o, i * 100 + n);
  if (fs[n][0](o) !== i * 100 + n) bad++;
}
bad + ' ' + objs[0].x + ',' + objs[1].x + ',' + objs[2].x;`
	assert.Equal(t, "0 4938,4939,4937", runScriptsIn(t, r, src))
	assert.Len(t, pt.ways, 80)
	mask, moved := uint32(len(pt.slots)-1), 0
	for j, s := range pt.slots {
		if s.site < 1<<24 && s.site != 0 && s.site&mask != uint32(j) {
			moved++
		}
	}
	assert.Greater(t, moved, 20, "polyICs past their first slot")
	for i := range pt.ways {
		assert.False(t, pt.ways[i].mega)
		assert.Equal(t, uint32(47), pt.ways[i].hits)
	}
}

// polyRealm returns a realm whose sites get a polyIC at their first
// polymorphic miss.
func polyRealm() *Realm {
	r := NewRealm()
	r.lazyState().poly = &polyTable{slots: make([]polySlot, 16)}
	return r
}

// TestPolySites checks that only a site that sees several shapes allocates
// its entries, after polyAfter such misses of its code's sites, that it
// turns polymorphic once, that dynamic code stays monomorphic and still
// reads and writes right, that sites past what their entries hold turn
// megamorphic, and that a getter may run the site and grow the realm's
// table of entries.
func TestPolySites(t *testing.T) {
	sites := func(r *Realm) int {
		if r.lazy == nil || r.lazy.poly == nil {
			return 0
		}
		return len(r.lazy.poly.ways)
	}
	r := NewRealm()
	runScriptsIn(t, r, `var a = {x: 1}, b = {x: 2}, s = 0;
function rx(o) { return o.x; }
for (var i = 0; i < 100; i++) s += rx(a) + rx(b); s`)
	assert.Zero(t, sites(r), "a site that sees one shape")
	runScriptsIn(t, r, `for (var i = 0; i < 10; i++) s += rx(a) + rx({w: 0, x: 1}); s`)
	assert.True(t, r.lazy == nil || r.lazy.poly == nil, "fewer misses than polyAfter")
	runScriptsIn(t, r, `var c = {y: 0, x: 3}, d = {z: 0, x: 4}, e = Object.create(a);
for (var i = 0; i < 100; i++) s += rx(a) + rx(c) + rx(d) + rx(e) + rx(Object.create(c)); s`)
	assert.Equal(t, 1, sites(r))
	SetCompiler(compiler.Hook{})
	t.Cleanup(func() { SetCompiler(nil) })
	assert.Equal(t, "1,0,2,1,3,2,4,3,1,4,2,5,3,6,4,7,1,8,2,9,3,10,4,11,1,12,2,13,3,14,4,15,1,16,2,17,3,18,4,19,1,20,2,21,3,22,4,23,1,24,2,25,3,26,4,27,1,28,2,29,3,30,4,31,1,32,2,33,3,34,4,35,1,36,2,37,3,38,4,39", runScriptsIn(t, r, `var out = [];
var f = new Function('o', 'return o.x;');
var g = eval('(function (o, v) { o.y = v; return o.y; })');
var objs = [{x: 1, y: 0}, {a: 0, x: 2, y: 0}, {b: 0, c: 0, x: 3, y: 0}, Object.create({x: 4})];
for (var i = 0; i < 40; i++) out.push(f(objs[i % 4]), g(objs[(i + 1) % 4], i));
out.join();`))
	assert.Equal(t, 1, sites(r), "dynamic code")

	// Sites that cycle through more shapes than their entries hold turn
	// megamorphic and still read and write right.
	r = NewRealm()
	assert.Equal(t, "79800 392,393,394,395,396,397,398,399", runScriptsIn(t, r, `var objs = [];
for (var k = 0; k < 8; k++) { var o = {}; o['p' + k] = k; o.x = k; objs.push(o); }
function mx(o) { return o.x; }
function mw(o, v) { o.x = v; }
var s = 0;
for (var i = 0; i < 400; i++) { mw(objs[i % 8], i); s += mx(objs[i % 8]); }
s + ' ' + objs.map(mx).join();`))
	mega := 0
	for _, p := range r.lazy.poly.ways {
		if p.mega {
			mega++
		}
	}
	assert.Equal(t, 2, mega, "megamorphic sites")

	// A getter at a polymorphic site reads the site again, with other
	// shapes, and compiles code, which grows the table of entries.
	src := `var R = [], calls = 0;
function rv(o) { return o.v; }
for (var k = 0; k < 6; k++) (function (k) {
  var o = {}; o['p' + k] = k;
  if (k % 2) Object.defineProperty(o, 'v', {get: function () {
    calls++;
    var f = new Function('o', 'return o.p' + (k - 1) + ' + ' + k);
    return f(R[k - 1]) + (calls < 40 ? rv(R[(k + 2) % 6]) : 0);
  }});
  else o.v = k;
  R.push(o);
})(k);
var acc = [];
for (var i = 0; i < 30; i++) acc.push(rv(R[i % 6]));
acc.join() + ' ' + calls;`
	assert.Equal(t, "0,196,2,5,4,9,0,1,2,5,4,9,0,1,2,5,4,9,0,1,2,5,4,9,0,1,2,5,4,9 54", runScriptsIn(t, polyRealm(), src))
}

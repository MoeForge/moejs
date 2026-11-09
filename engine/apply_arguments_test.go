package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestApplyArguments runs f.apply(x, arguments) in functions whose
// arguments object only such calls read (bytecode.Function.ForwardArguments:
// the frame keeps its arguments, and ApplyArguments creates the object only
// for a callee other than %Function.prototype.apply%), next to the lists
// apply builds from other array-likes (denseList) and constructors that read
// their prototype (constructThis). The expected results are Node 24's.
func TestApplyArguments(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"forwarded values", []string{`function g() { return [this === o, arguments.length, Array.prototype.join.call(arguments, ',')].join(' '); }
var o = {};
function h(a, b) { return g.apply(o, arguments); }
[h(), h(1), h(1, 2), h(1, 2, 3, 4)].join(' | ')`},
			"true 0  | true 1 1 | true 2 1,2 | true 4 1,2,3,4"},
		{"strict keeps the arguments", []string{`'use strict';
function g(a, b) { return a + ',' + b + ',' + arguments.length; }
function h(a, b) { a = 10; b = 20; return g.apply(this, arguments); }
h(1, 2) + ' ' + h(1)`},
			"1,2,2 1,undefined,1"},
		{"constructor", []string{`var Class = { create: function () { return function () { this.initialize.apply(this, arguments); }; } };
var P = Class.create();
P.prototype = { initialize: function (x, y) { this.x = x; this.y = y; } };
var out = [];
for (var i = 0; i < 20; i++) { var p = new P(i, i * 2); out.push(p.x + p.y); }
out.join(',') + ' ' + (new P(1) instanceof P) + ' ' + String(new P().x)`},
			"0,3,6,9,12,15,18,21,24,27,30,33,36,39,42,45,48,51,54,57 true undefined"},
		{"another apply gets a mapped object", []string{`var seen = [];
var o = { apply: function (t, args) { seen.push(typeof args, Object.prototype.toString.call(args), args.length, args[0], args.callee === h); args[0] = 9; return t; } };
function h(a) { var t = o.apply(this, arguments); return [a, t === globalThis].join(); }
h(1) + ' ' + seen.join(',')`},
			"9,true object,[object Arguments],1,1,true"},
		{"another apply gets an unmapped object", []string{`'use strict';
var seen = [];
var o = { apply: function (t, args) { seen.push(Object.prototype.toString.call(args), args.length, args[0]); args[0] = 9; try { args.callee; } catch (e) { seen.push(e.constructor.name); } return t; } };
function h(a) { var t = o.apply(this, arguments); return [a, t === undefined].join(); }
h(1) + ' ' + seen.join(',')`},
			"1,true [object Arguments],1,1,TypeError"},
		{"one object for every site", []string{`var keep = [];
var o = { apply: function (t, a) { keep.push(a); } };
function g() { return arguments.length; }
function h() { o.apply(this, arguments); o.apply(this, arguments); return g.apply(this, arguments); }
h(1, 2) + ' ' + (keep[0] === keep[1]) + ' ' + keep[0].length`},
			"2 true 2"},
		{"replaced Function.prototype.apply", []string{`var orig = Function.prototype.apply, log = [];
function g(a, b) { return a + b; }
function h(a, b) { return g.apply(null, arguments); }
var r1 = h(1, 2);
Function.prototype.apply = function (t, args) { log.push(args.length); return orig.call(this, t, args) * 10; };
var r2 = h(1, 2);
Function.prototype.apply = orig;
var r3 = h(1, 2);
[r1, r2, r3, log.join()].join(' ')`},
			"3 30 3 2"},
		{"Reflect.apply", []string{`function f() { return 'f'; }
function h() { try { return Reflect.apply(f, arguments); } catch (e) { return e.constructor.name; } }
h(1)`},
			"TypeError"},
		{"not callable", []string{`function h() { try { return ({}).apply(this, arguments); } catch (e) { return e.constructor.name; } }
function k() { var o = {apply: Function.prototype.apply}; try { return o.apply(this, arguments); } catch (e) { return e.constructor.name; } }
h(1) + ' ' + k(1)`},
			"TypeError TypeError"},
		{"spread calls keep the arguments", []string{`function sum() { var s = 0; for (var i = 0; i < arguments.length; i++) s += arguments[i]; return s; }
function h(a, b, c) { var x = sum(...[100, 200, 300, 400, 500]); var y = new Array(...[7, 8, 9]).length; return x + y + sum.apply(this, arguments); }
h(1, 2, 3) + ' ' + h(4)`},
			"1509 1507"},
		{"derived constructor", []string{`class B { constructor(...xs) { this.xs = xs; } }
class D extends B { constructor() { super(...[1, 2]); this.r = D.f.apply(this, arguments); } static f() { return Array.prototype.slice.call(arguments).join('-'); } }
new D('a', 'b', 'c').r + ' ' + new D('a', 'b', 'c').xs.join()`},
			"a-b-c 1,2"},
		{"deep recursion", []string{`function f(n) { return n === 0 ? [].slice.call(arguments, 1).join() : g.apply(null, arguments); }
function g(n, a, b, c) { return f(n - 1, a + 1, b, c, 'x'); }
f(1000, 0, 'b', 'c')`},
			"1000,b,c,x"},
		{"recursion limit", []string{`function r() { return r.apply(this, arguments); }
var m; try { r(1, 2); } catch (e) { m = e.constructor.name + ': ' + e.message; } m`},
			"RangeError: Maximum call stack size exceeded"},
		{"mapped in a block scope", []string{`var o = { apply: function (t, args) { args[1] = 'B'; return args; } };
function h(a, b) { { let k = 1; var f = function () { return k; }; var r = o.apply(this, arguments); } return [a, b, r.length, f()].join(); }
h('a', 'b', 'c') + ' ' + h('x')`},
			"a,B,3,1 x,,1,1"},
		{"arguments objects", []string{`function g() { return Array.prototype.join.call(arguments, ',') + '/' + arguments.length; }
function h() { arguments.length = 2; return g.apply(null, arguments); }
function k() { arguments.length = '1'; return g.apply(null, arguments); }
function m() { delete arguments[0]; return g.apply(null, arguments); }
function n() { Object.defineProperty(arguments, 1, {get: function () { return 'G'; }}); return g.apply(null, arguments); }
function p() { arguments.length = 1.5; return g.apply(null, arguments); }
function q() { arguments.length = -1; return g.apply(null, arguments); }
function s(a, b) { 'use strict'; arguments[0] = 'S'; return g.apply(null, arguments); }
function t(a, b) { a = 'T'; return g.apply(null, arguments); }
[h(1, 2, 3), k(1, 2, 3), m(1, 2, 3), n(1, 2, 3), p(1, 2, 3), q(1, 2), s(1, 2), t(1, 2)].join(' | ')`},
			"1,2/2 | 1/1 | ,2,3/3 | 1,G,3/3 | 1/1 | /0 | S,2/2 | T,2/2"},
		{"array lists", []string{`function g() { return Array.prototype.join.call(arguments, ',') + '/' + arguments.length; }
Array.prototype[1] = 'P';
var r1 = g.apply(null, [1, , 3]);
delete Array.prototype[1];
var b = [1, 2]; b.length = 4;
var r2 = g.apply(null, b);
var r3 = g.apply(null, Object.freeze([5, 6]));
var r4 = g.apply(null, new Proxy([7, 8], {get: function (t, k) { return k === 'length' ? 1 : t[k]; }}));
var r5 = g.apply(null, new Int8Array([1, 2]));
var r6 = g.apply(null, {length: 2, 0: 'x', 1: 'y'});
var big = []; for (var i = 0; i < 70000; i++) big.push(i);
var r7 = Math.max.apply(null, big);
var r8 = g.apply(null, []);
[r1, r2, r3, r4, r5, r6, r7, r8].join(' | ')`},
			"1,P,3/3 | 1,2,,/4 | 5,6/2 | 7/1 | 1,2/2 | x,y/2 | 69999 | /0"},
		{"constructor prototypes", []string{`function F() { this.v = 1; }
var out = [];
for (var i = 0; i < 5; i++) out.push(Object.getPrototypeOf(new F()) === F.prototype);
var p1 = F.prototype; F.prototype = {tag: 'new'}; out.push(new F().tag);
F.prototype = 7; out.push(Object.getPrototypeOf(new F()) === Object.prototype);
F.prototype = p1; out.push(Object.getPrototypeOf(new F()) === p1);
Object.defineProperty(F, 'prototype', {writable: false}); out.push(Object.getPrototypeOf(new F()) === p1);
function G() { return {own: 1}; } out.push(new G().own);
var BF = F.bind(null); out.push(Object.getPrototypeOf(new BF()) === p1);
class C { x = 2; } out.push(new C().x);
function H() {} var r = Reflect.construct(H, [], F); out.push(Object.getPrototypeOf(r) === p1);
function M() {} out.push(Object.getPrototypeOf(new M()) === M.prototype);
var N = function () {}; N.foo = 1; out.push(Object.getPrototypeOf(new N()) === N.prototype);
function Q() {} delete Q.name; out.push(Object.getPrototypeOf(new Q()) === Q.prototype);
function S() {} var sp = new Proxy(S, {}); out.push(Object.getPrototypeOf(new sp()) === S.prototype);
out.join()`},
			"true,true,true,true,true,new,true,true,true,1,true,2,true,true,true,true,true"},
	})
}

// TestApplyArgumentsStack checks the stack traces and error messages of
// errors through f.apply(x, arguments) of functions with ForwardArguments:
// as for any call of %Function.prototype.apply%, its own frame does not
// show, and a callee that is no function is described as the call's.
func TestApplyArgumentsStack(t *testing.T) {
	src := `function thrower() { throw new Error('boom'); }
var obj = { initialize: function () { thrower(); } };
function Ctor() { this.initialize.apply(this, arguments); }
Ctor.prototype = obj;
function viaApply() { return thrower.apply(this, arguments); }
function viaCall() { return thrower.call(this, 1); }
function notFn() { var o = {}; return o.apply(this, arguments); }
function notFn2() { var q = {apply: 5}; return q.apply(this, arguments); }
function notCallable() { var o = {apply: Function.prototype.apply, x: 1}; return o.apply(this, arguments); }
function deep() { return deep.apply(this, arguments); }
var out = [];
[function () { new Ctor(1, 2); }, function () { viaApply(1); }, function () { viaCall(); }, function () { notFn(1); },
 function () { notFn2(1); }, function () { notCallable(1); }, function () { deep(1); }].forEach(function (f) {
  try { f(); } catch (e) { out.push(e.constructor.name + ': ' + e.message + '\n' + String(e.stack).split('\n').slice(0, 6).join('\n')); }
});
out.join('\n---\n')`
	assert.Equal(t, applyStackWant, runScripts(t, src))
}

const applyStackWant = `Error: boom
Error: boom
    at thrower (script.js:1:28)
    at Object.initialize (script.js:2:39)
    at new Ctor (script.js:3:35)
    at script.js:12:16
    at script.js:14:9
---
Error: boom
Error: boom
    at thrower (script.js:1:28)
    at viaApply (script.js:5:38)
    at script.js:12:49
    at script.js:14:9
    at Array.forEach (<anonymous>)
---
Error: boom
Error: boom
    at thrower (script.js:1:28)
    at viaCall (script.js:6:37)
    at script.js:12:79
    at script.js:14:9
    at Array.forEach (<anonymous>)
---
TypeError: o.apply is not a function
TypeError: o.apply is not a function
    at notFn (script.js:7:41)
    at script.js:12:107
    at script.js:14:9
    at Array.forEach (<anonymous>)
    at script.js:13:89
---
TypeError: q.apply is not a function
TypeError: q.apply is not a function
    at notFn2 (script.js:8:50)
    at script.js:13:16
    at script.js:14:9
    at Array.forEach (<anonymous>)
    at script.js:13:89
---
TypeError: Function.prototype.apply was called on [object Object], which is not a function
TypeError: Function.prototype.apply was called on [object Object], which is not a function
    at notCallable (script.js:9:84)
    at script.js:13:44
    at script.js:14:9
    at Array.forEach (<anonymous>)
    at script.js:13:89
---
RangeError: Maximum call stack size exceeded
RangeError: Maximum call stack size exceeded
    at deep (script.js:10:31)
    at deep (script.js:10:31)
    at deep (script.js:10:31)
    at deep (script.js:10:31)
    at deep (script.js:10:31)`

// TestArgumentsReads reads arguments.length and arguments[k], directly and
// through a top-level var a = arguments, in functions whose arguments object
// nothing else needs (ArgsLen, ArgsGet, whose fast paths are run's): an
// index below the count reads the kept value, another key the object's
// property as before, and a function whose arguments object exists by then
// (another apply got it, a default parameter read it) reads the object. The
// functions of the last case keep the object. The expected results are
// Node 24's.
func TestArgumentsReads(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"length and elements", []string{`function len() { return arguments.length; }
function at(i) { return arguments[i]; }
function last() { return arguments[arguments.length - 1]; }
function sum() { var s = 0; for (var i = 0; i < arguments.length; i++) s += arguments[i]; return s; }
function list() { var res = null; var a = arguments; for (var i = a.length - 1; i >= 0; i--) res = [a[i], res]; return JSON.stringify(res); }
function fact() { var n = arguments[0]; return n <= 1 ? 1 : n * fact(n - 1); }
var big = []; for (var i = 0; i < 300; i++) big.push(i);
[len(), len(1, 2, 3), at(1, 'x'), String(at(5, 1)), last(1, 2, 3), String(last()), sum(1, 2, 3, 4), list(1, 2, 3), list(), fact(10), sum.apply(null, big), len.apply(null, big)].join(' ')`},
			"0 3 x undefined 3 undefined 10 [1,[2,[3,null]]] null 3628800 44850 300"},
		{"keys past the kept values", []string{`function k(key) { return arguments[key]; }
Object.prototype[7] = 'proto7';
var r = [k('0'), String(k(-1)), String(k(1.5)), k(7), k('length'), k('callee') === k, typeof k(Symbol.iterator), k(Symbol.iterator) === Array.prototype.values, k({toString: function () { return '0'; }}) === '[object Object]'];
delete Object.prototype[7];
Object.defineProperty(Object.prototype, '99', {get: function () { return Object.prototype.toString.call(this) + this.length; }, configurable: true});
function gg() { return arguments[99]; }
var g = gg(1, 2); delete Object.prototype[99]; r.push(g);
function twice(i) { var x = arguments['x' + i], y = arguments[0]; return String(x) + y + arguments.length; }
r.push(twice(0), twice(3));
r.join(' ')`},
			"0 undefined undefined proto7 1 true function true false [object Arguments]2 undefined01 undefined31"},
		{"strict", []string{`'use strict';
function k(key) { return arguments[key]; }
var out = [k('length'), k(0), String(k(3))];
try { k('callee'); out.push('no'); } catch (e) { out.push(e.constructor.name); }
function n(a, b) { a = 5; return [a, arguments[0], arguments.length].join(); }
out.push(n(1, 2));
out.join(' ')`},
			"1 0 undefined TypeError 5,1,2"},
		{"materialized later", []string{`var o = { apply: function (t, args) { args[0] = 'chg'; args.length = 5; return t; } };
function h(a, b) { var r = [arguments.length, arguments[0]]; o.apply(this, arguments); r.push(arguments.length, arguments[0], a, String(arguments[4])); return r.join(); }
function hs(a, b) { 'use strict'; var r = [arguments.length, arguments[0]]; o.apply(this, arguments); r.push(arguments.length, arguments[0], a); return r.join(); }
function d(x = 1, y) { var a = arguments; return [a.length, a[0], a[1], x].join(); }
function p(a, b = arguments.length) { return b; }
function m(a) { var r = arguments[0]; arguments.length; return [r, a].join(); }
[h(1, 2), hs(1, 2), d(), d(5), d(undefined, 2), p(1), p(1, 2, 3), m(3), m()].join(' ')`},
			"2,1,5,chg,chg,undefined 2,1,5,chg,1 0,,,1 1,5,,5 2,,2,1 1 2 3,3 ,"},
		{"edge keys", []string{`function k(key) { return arguments[key]; }
function k2(a, b) { return String(arguments[b]); }
[k(-0), String(k(NaN)), String(k(Infinity)), String(k(4294967295)), String(k(2147483648)), k2(5, 0), k2(5, 1), k2(5, -0), k2(5, 2)].join(' ')`},
			"0 undefined undefined undefined undefined 5 1 5 undefined"},
		{"reads that need the object", []string{`var out = [];
function before() { var r = typeof a; var a = arguments; return r + a.length; }
function reassigned() { var a = arguments; a = [9]; return a[0] + ',' + arguments[0]; }
function twoDecl() { var a = arguments; var a; return a.length; }
function captured() { var a = arguments; return function () { return a[0]; }(); }
function callee() { return arguments[0](); }
function writes(x) { arguments[0] = 2; return x + ',' + arguments[0]; }
function deletes(x) { delete arguments[0]; return String(arguments[0]) + arguments.length; }
function update(x) { arguments[0]++; return x + ',' + arguments[0]; }
function forin(x) { for (arguments[0] in {k: 1}); return x + ',' + arguments[0]; }
function tagged() { return arguments[0]` + "`" + `t` + "`" + `; }
function opt() { return arguments?.[0]; }
function aliasCallee() { var a = arguments; return a.callee === aliasCallee; }
function aliasLater() { var a = arguments; var r = a.length; a.push = 1; return r; }
out.push(before(1, 2), reassigned(1), twoDecl(1, 2), captured(4), callee(function () { return 'c' + (this === undefined); }));
out.push(writes(1), deletes(1), update(1), forin(1), tagged(function (s) { return s[0]; }), opt(5), aliasCallee(), aliasLater(1, 2));
out.join(' ')`},
			"undefined2 9,1 2 4 cfalse 2,2 undefined1 2,2 k,k t 5 true 2"},
	})
}

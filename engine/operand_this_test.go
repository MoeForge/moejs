package engine

import (
	"strings"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/stretchr/testify/assert"
)

// TestOperandCopies runs operands that are register locals while the rest
// of their expression assigns: the compiler reads a local's register in
// place only when nothing evaluated before the operator can assign that
// local, and a prefix update's value is its local's register. A local that
// a closure, a direct eval or a mapped arguments object can write lives in
// an environment instead, and with statements keep their names out of
// registers; destructuring copies a source local it assigns.
func TestOperandCopies(t *testing.T) {
	SetCompiler(compiler.Hook{}) // direct eval
	t.Cleanup(func() { SetCompiler(nil) })
	runScriptCases(t, []scriptCase{
		{"other local assigned", []string{`(function (a, b) { return a + (b = 5) + b; })(1, 2)`}, "11"},
		{"same local assigned", []string{`(function (a) { return a + (a = 5) + a; })(1)`}, "11"},
		{"compound", []string{`(function (a, b) { var r = a; r += (b = 3); var s = 1; s += (s = 10); var t = 2; t *= (t++, t); return r + "," + s + "," + b + "," + t; })(1, 2)`}, "4,11,3,6"},
		{"captured by a closure", []string{`(function () { var a = 1; function g() { a = 10; return 0; } return a + g() + a; })()`}, "11"},
		{"direct eval", []string{`(function () { var a = 1; return a + eval("a = 5, 0") + a; })()`}, "6"},
		{"direct eval in an enclosing function", []string{`(function () { var a = 1; eval(""); return (function () { return a + (a = 4) + a; })() + "," + a; })()`}, "9,4"},
		{"eval elsewhere, own register local", []string{`(function () { eval(""); return (function (a) { return a + (a = 5) + a; })(1); })()`}, "11"},
		{"with", []string{`(function () { var a = 1, o = {}; with (o) { var r = a + (a = 5, 0) + a; } return r + "," + a; })()`}, "6,5"},
		{"with property", []string{`(function () { var a = 1, o = {a: 2}; with (o) { var r = a + (a = 5, 0) + a; } return r + "," + a + "," + o.a; })()`}, "7,1,5"},
		{"mapped arguments", []string{`(function (a) { return a + (arguments[0] = 5) + a; })(1)`}, "11"},
		{"mapped arguments, compound", []string{`(function (a, b) { var s = a; s += (arguments[0] = 7); return s + "," + a + (arguments[1] = 3) + b; })(1, 2)`}, "8,733"},
		{"strict arguments", []string{`(function (a) { "use strict"; return a + (arguments[0] = 5) + a; })(1)`}, "7"},
		{"unmapped arguments", []string{`(function (a, b = 0) { return a + (arguments[0] = 5) + a; })(1)`}, "7"},
		{"prefix updates", []string{`(function () { var i = 1, x = [10, 20, 30, 40, 50, 60]; return [x[++i], x[i++], i, ++i + i, i + ++i, x[--i] + i, (x[++i] = i), x.join()].join(); })()`}, "30,30,3,8,9,54,5,10,20,30,40,50,5"},
		{"prefix update then assignment", []string{`(function () { var i = 1, x = [0, 0, 0, 0]; x[++i] = i++; var j = 1; var r = ++j * (j = 10); return x.join() + "," + i + "," + r + "," + j; })()`}, "0,0,2,0,3,20,10"},
		{"const update", []string{`(function () { const c = 1; try { return [0][++c]; } catch (e) { return e.name + c; } })()`}, "TypeError1"},
		{"function name update", []string{`(function f() { return [++f, typeof f].join(); })()`}, "NaN,function"},
		{"destructuring its own source", []string{`(function () { var i = {a: 1, b: 2}; var {a: i, b} = i; return b; })()`}, "2"},
		{"destructuring default reads the target", []string{`(function () { var i = {a: {x: 1}, b: undefined}; var {a: i, b = i} = i; return JSON.stringify(b); })()`}, "{\"x\":1}"},
		{"destructuring an update", []string{`(function () { var i = 1; var {x: i = i} = ++i; var j = 1; var [j, k = j] = [++j]; return i + "," + j + "," + k; })()`}, "2,2,2"},
		{"let destructuring", []string{`(function () { var o = {p: 1, q: 2}; let {p, q = p} = o; return p + q; })()`}, "3"},
	})
}

// TestLogicalAssignReadsTarget runs logical and compound assignments to a
// register local whose value reads the local: a template, an array or
// object literal, a call, a conditional or a logical expression that
// reads it reads the old value, as a plain assignment's does. The
// expected results are Node 24's.
func TestLogicalAssignReadsTarget(t *testing.T) {
	src := strings.Join([]string{
		"var out = [];",
		"function t(name, f) { try { out.push(name + \"=\" + f()); } catch (e) { out.push(name + \"=\" + e.constructor.name); } }",
		"t(\"assign tmpl\", function () { var c = 3, a = 1; c = `${a}-${c}`; return c; });",
		"t(\"and-assign tmpl\", function () { var c = 3, a = 1; c &&= `${a}-${c}`; return c; });",
		"t(\"or-assign tmpl\", function () { var c = 0, a = 1; c ||= `${a}-${c}`; return c; });",
		"t(\"nullish tmpl\", function () { var c = null, a = 1; c ??= `${a}-${c}`; return c; });",
		"t(\"add-assign tmpl\", function () { var c = 3, a = 1; c += `${a}-${c}`; return c; });",
		"t(\"assign tmpl2\", function () { var c = 3, a = 1; c = `x${c}y${a}z${c}`; return c; });",
		"t(\"var tmpl\", function () { var a = 1, c = 3; var c = `${a}-${c}`; return c; });",
		"t(\"assign arr\", function () { var c = 3, a = 1; c = [a, c].join(); return c; });",
		"t(\"and-assign arr\", function () { var c = 3, a = 1; c &&= [a, c].join(); return c; });",
		"t(\"assign obj\", function () { var c = 3, a = 1; c = {k: a, j: c}.j; return c; });",
		"t(\"and-assign obj\", function () { var c = 3, a = 1; c &&= ({k: a, j: c}).j; return c; });",
		"t(\"and-assign obj2\", function () { var c = 3, a = 1; c &&= {k: a, j: c}; return c.j; });",
		"t(\"assign obj2\", function () { var c = 3, a = 1; c = {k: a, j: c}; return c.j; });",
		"t(\"assign arr2\", function () { var c = 3, a = 1; c = [a, c]; return c[1]; });",
		"t(\"and-assign arr2\", function () { var c = 3, a = 1; c &&= [a, c]; return c[1]; });",
		"t(\"and-assign cond\", function () { var c = 3, a = 1; c &&= a ? c + 1 : 0; return c; });",
		"t(\"and-assign bin\", function () { var c = 3, a = 1; c &&= a - c; return c; });",
		"t(\"and-assign neg\", function () { var c = 3, a = 1; c &&= -c; return c; });",
		"t(\"and-assign typeof\", function () { var c = 3; c &&= typeof c; return c; });",
		"t(\"and-assign call\", function () { var c = 3; c &&= String(c); return c; });",
		"t(\"and-assign seq\", function () { var c = 3, a = 1; c &&= (a, c); return c; });",
		"t(\"and-assign logical\", function () { var c = 3, a = 0; c &&= a || c; return c; });",
		"t(\"let tmpl\", function () { let a = 1; let c = 3; c = `${a}-${c}`; return c; });",
		"t(\"strict tmpl\", function () { \"use strict\"; var c = 3, a = 1; c = `${a}-${c}`; return c; });",
		"t(\"param tmpl\", function (c, a) { c = `${a}-${c}`; return c; }.bind(null, 3, 1));",
		"t(\"param and tmpl\", function (c, a) { c &&= `${a}-${c}`; return c; }.bind(null, 3, 1));",
		"t(\"tagged\", function () { var c = 3, a = 1; c = String.raw`${a}-${c}`; return c; });",
		"t(\"cond\", function () { var c = 3, a = 1; c = a ? `${a}-${c}` : 0; return c; });",
		"t(\"cond and\", function () { var c = 3, a = 1; c &&= a ? `${a}-${c}` : 0; return c; });",
		"out.join(\"\\n\");",
	}, "\n")
	want := strings.Join([]string{
		"assign tmpl=1-3",
		"and-assign tmpl=1-3",
		"or-assign tmpl=1-0",
		"nullish tmpl=1-null",
		"add-assign tmpl=31-3",
		"assign tmpl2=x3y1z3",
		"var tmpl=1-3",
		"assign arr=1,3",
		"and-assign arr=1,3",
		"assign obj=3",
		"and-assign obj=3",
		"and-assign obj2=3",
		"assign obj2=3",
		"assign arr2=3",
		"and-assign arr2=3",
		"and-assign cond=4",
		"and-assign bin=-2",
		"and-assign neg=-3",
		"and-assign typeof=number",
		"and-assign call=3",
		"and-assign seq=3",
		"and-assign logical=3",
		"let tmpl=1-3",
		"strict tmpl=1-3",
		"param tmpl=1-3",
		"param and tmpl=1-3",
		"tagged=1-3",
		"cond=1-3",
		"cond and=1-3",
	}, "\n")
	assert.Equal(t, want, runScripts(t, src))
}

// TestThisRegister runs functions whose this the compiler keeps in a
// register loaded once at entry, after a sloppy function's coercion,
// against derived constructors (whose this is a binding super()
// initializes), arrows, generators and async functions across yield and
// await, direct eval, class fields and static blocks, and super property
// references.
func TestThisRegister(t *testing.T) {
	SetCompiler(compiler.Hook{}) // direct eval
	t.Cleanup(func() { SetCompiler(nil) })
	runScriptCases(t, []scriptCase{
		{"derived constructor TDZ", []string{`class A {} class B extends A { constructor() { var r; try { this.x = 1; } catch (e) { r = e.name; } super(); this.y = 2; this.r = r; } } var b = new B(); b.r + "," + b.y + "," + ("x" in b)`}, "ReferenceError,2,false"},
		{"super() called twice", []string{`class A { constructor() { this.n = (this.n || 0) + 1; } } class C extends A { constructor() { super(); try { super(); } catch (e) { this.err = e.name; } } } var c = new C(); c.err + "," + c.n`}, "ReferenceError,1"},
		{"arrows in methods and constructors", []string{`class P { constructor() { this.v = 1; const f = () => this.v + 1; this.w = f(); } m() { return [1, 2].map(x => this.v + x).join(); } } var p = new P(); p.w + "," + p.m() + "," + P.prototype.m.call({v: 10})`}, "2,2,3,11,12"},
		{"arrow in a derived constructor", []string{`class A {} class D extends A { constructor() { const f = () => this; let r; try { f(); } catch (e) { r = e.name; } super(); this.r = r; this.same = f() === this; } } var d = new D(); d.r + "," + d.same`}, "ReferenceError,true"},
		{"generator", []string{`var o = { a: 5, *g() { yield this.a; this.a++; yield this.a + 1; return this === o; } }; [...o.g()].join() + "," + Array.from(o.g.call({a: 1})).join() + "," + o.g().next().value`}, "5,7,1,3,6"},
		{"generator this across next", []string{`var o = { a: 1, *g() { var x = yield this.a; this.a += x; yield this.a; } }; var it = o.g(); it.next(); it.next(5).value + "," + o.a`}, "6,6"},
		{"direct eval reads this", []string{`var o = { a: 3, m() { return eval("this.a") + this.a; } }; [o.m(), (function () { return eval("typeof this") + typeof this; }).call(5), (function () { "use strict"; return eval("typeof this") + typeof this; }).call(5)].join()`}, "6,objectobject,numbernumber"},
		{"class field initializers", []string{`class F { a = this.constructor.name; b = () => this.a; c = this; static s = this.name; static t = () => this.s; } var f = new F(); [f.b(), f.c === f, F.s, F.t()].join()`}, "F,true,F,F"},
		{"super property reads and calls", []string{`class S { m() { return "S" + this.t; } get g() { return this.t; } } class T extends S { constructor() { super(); this.t = 1; } m() { return super.m() + super.g + (() => super.m())(); } n() { return super.m.call({t: 9}); } } var t = new T(); t.m() + "," + t.n()`}, "S11S1,S9"},
		{"super property in an object literal", []string{`var base = { hi() { return "hi " + this.name; } }; var o = { name: "o", __proto__: base, hi() { return super.hi() + "!"; } }; o.hi() + "," + o.hi.call({name: "x"})`}, "hi o!,hi x!"},
		{"sloppy this coerced once", []string{`function f() { var a = this; return typeof this + (this === globalThis) + (a === this); } [f.call(undefined), f.call(null), f.call(5), f()].join()`}, "objecttruetrue,objecttruetrue,objectfalsetrue,objecttruetrue"},
		{"this in a loop", []string{`var o = { n: 0, k: 2, run(m) { for (var i = 0; i < m; i++) this.n += this.k; return this.n; } }; o.run(5) + "," + o.run.call({n: 1, k: 3}, 2)`}, "10,7"},
		{"base constructor with fields", []string{`class Q { x = 1; constructor(v) { this.y = this.x + v; } } var q = new Q(4); q.x + "," + q.y`}, "1,5"},
		{"static block", []string{`class R { static v = 2; static { this.w = this.v * 3; } } R.w`}, "6"},
		{"async", []string{`var log = []; var o = { a: 3, async m() { const x = this.a; await null; this.a++; await 0; log.push(x + this.a, this === o); } }; o.m(); (async function () { await null; log.push(typeof this); }).call(7); "started"`, `log.join()`}, "object,7,true"},
		{"async arrow", []string{`var log = []; var o = { a: 2, m() { return (async () => { await null; return this.a * 10; })(); } }; o.m().then(v => log.push(v)); "started"`, `log.join()`}, "20"},
	})
}

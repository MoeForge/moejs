package engine

import (
	"testing"

	"github.com/MoeForge/moejs/compiler"
)

// TestPlainCallThis covers calls whose this is undefined by the Call's
// operand (bytecode.CallUndefThis) instead of a LoadUndef into the call
// block's this register: that register keeps whatever an earlier call left
// in it, so neither the callee nor a stack trace may read it.
func TestPlainCallThis(t *testing.T) {
	SetCompiler(compiler.Hook{}) // for direct eval
	t.Cleanup(func() { SetCompiler(nil) })
	// Each case first makes a method call whose receiver is left in the
	// registers the plain call after it reuses.
	const recv = `var o = { m() { return this; } }; `
	runScriptCases(t, []scriptCase{
		{"sloppy callee", []string{recv + `function f() { return this; } o.m(); f() === globalThis`}, "true"},
		{"strict callee", []string{recv + `function f() { "use strict"; return this; } o.m(); f() === undefined`}, "true"},
		{"in a function", []string{recv + `function f() { "use strict"; return this; } function g() { o.m(); return f(); } g() === undefined`}, "true"},
		{"arguments", []string{recv + `function f(a, b) { "use strict"; return [this, a, b]; } o.m(); JSON.stringify(f(1, 2)) + typeof f(3)[0]`}, "[null,1,2]undefined"},
		{"many arguments", []string{recv + `function f() { "use strict"; return this === undefined && arguments.length; } o.m(); f(1, 2, 3, 4, 5, 6, 7, 8, 9)`}, "9"},
		{"nested", []string{recv + `function f(x) { "use strict"; return this === undefined ? x : "this"; } o.m(); f(f(f("ok")))`}, "ok"},
		{"call result as callee", []string{recv + `function mk() { return function () { "use strict"; return this; }; } o.m(); mk()() === undefined`}, "true"},
		{"optional call", []string{recv + `function f() { "use strict"; return this; } var n = null; o.m(); f?.() === undefined && n?.() === undefined`}, "true"},
		{"optional chain callee", []string{recv + `var mk = () => function () { "use strict"; return this; }; o.m(); (mk?.())() === undefined`}, "true"},
		{"member callee keeps this", []string{recv + `o.m(); o.m() === o && (o?.m)() === o`}, "true"},
		{"spread", []string{recv + `function f() { "use strict"; return this; } o.m(); f(...[1, 2]) === undefined`}, "true"},
		{"tagged template", []string{recv + `function tag(s) { "use strict"; return this; } o.m(); tag` + "`x${1}`" + ` === undefined`}, "true"},
		{"member tag", []string{recv + `var t = { tag(s) { return this; } }; o.m(); t.tag` + "`x`" + ` === t`}, "true"},
		{"native callee", []string{recv + `var p = parseInt; o.m(); p("12")`}, "12"},
		{"bound callee", []string{recv + `var b = function () { return this; }.bind(o); o.m(); b() === o`}, "true"},
		{"arrow callee", []string{recv + `var a = () => this; o.m(); a() === globalThis`}, "true"},
		{"Function.prototype.call as a plain callee", []string{recv + `var c = Function.prototype.call; o.m(); var r; try { c(); } catch (e) { r = e.name; } r`}, "TypeError"},
		{"generator across a yield", []string{recv + `function f(x) { "use strict"; return this === undefined ? x : "this"; }
			function* g() { o.m(); yield f(yield 1); } var it = g(); it.next(); it.next("v").value`}, "v"},
		{"async across an await", []string{recv + `var out; function f(x) { "use strict"; return this === undefined ? x : "this"; }
			async function a() { o.m(); out = f(await "v"); } a();`, `out`}, "v"},
		{"class static block", []string{`class C { static { this.x = this === C; } } C.x`}, "true"},
		{"not a function", []string{recv + `var nf = 1; o.m(); var r; try { nf(); } catch (e) { r = e.message; } r`}, "nf is not a function"},
		// The receiver of the frame a stack trace prints: a plain call
		// shows the bare name, a method call the receiver's class.
		{"stack trace frame", []string{recv + `function thrower() { return new Error("e").stack.split("\n")[1].trim(); }
			function g() { o.m(); return thrower(); } var s = g(); s.slice(0, s.indexOf(" ("))`}, "at thrower"},
		{"stack trace of a method", []string{`var p = { thrower() { return new Error("e").stack.split("\n")[1].trim(); } }; var s = p.thrower(); s.slice(0, s.indexOf(" ("))`}, "at Object.thrower"},
		// A call in with finds its callee on the with object (this is the
		// object) or past it (this is undefined).
		{"with, hit", []string{recv + `var w = { f() { return this; } }; var r; o.m(); with (w) { r = f() === w; } r`}, "true"},
		{"with, miss", []string{recv + `function f() { "use strict"; return this; } var r; o.m(); with ({}) { r = f() === undefined; } r`}, "true"},
		{"with in a function, hit and miss", []string{recv + `function g() { "use strict"; return this; } function t(w) { with (w) { o.m(); return g(); } }
			var w = { g() { return this; } }; [t(w) === w, t({}) === undefined].join()`}, "true,true"},
		{"callee declared by sloppy direct eval", []string{recv + `function t() { eval("function h() { 'use strict'; return this; } function s() { return this; }"); o.m(); return [h() === undefined, s() === globalThis].join(); } t()`}, "true,true"},
		{"super method call", []string{recv + `class A { m() { return this; } } class B extends A { m() { o.m(); return super.m(); } } var b = new B(); b.m() === b`}, "true"},
		{"super method call in an object literal", []string{recv + `var base = { m() { return this; } }; var obj = { __proto__: base, m() { o.m(); return super.m(); } }; obj.m() === obj`}, "true"},
		{"Proxy apply trap", []string{recv + `var p = new Proxy(function () {}, { apply(t, thisArg, args) { return thisArg; } }); var q = { p }; o.m(); [p(1) === undefined, q.p() === q].join()`}, "true,true"},
		{"comma-expression callee", []string{recv + `var c = { m() { "use strict"; return this; } }; o.m(); [(0, c.m)() === undefined, (c.m)() === c, (o.m, c.m)() === undefined].join()`}, "true,true,true"},
	})
}

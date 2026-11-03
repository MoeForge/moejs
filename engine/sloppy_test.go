package engine

import (
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runScripts runs srcs as scripts, in order, in one realm and returns the
// completion value of the last one as a string, or the message of the first
// error.
func runScripts(t *testing.T, srcs ...string) string {
	t.Helper()
	return runScriptsIn(t, NewRealm(), srcs...)
}

// runScriptsIn is runScripts in the realm r.
func runScriptsIn(t *testing.T, r *Realm, srcs ...string) string {
	t.Helper()
	var v Value
	for _, src := range srcs {
		s, err := syntax.ParseScript("script.js", src, syntax.Options{})
		if err != nil {
			return "SyntaxError: " + err.Error()
		}
		code, err := compiler.CompileScript(s)
		require.NoError(t, err, src)
		if v, err = r.RunScript(code); err != nil {
			return errorString(err)
		}
	}
	return v.String()
}

// errorString renders a thrown error as "Name: message".
func errorString(err error) string {
	if ex, ok := err.(*Exception); ok {
		return errorDisplayString(ex.Value)
	}
	return err.Error()
}

type scriptCase struct {
	name string
	srcs []string
	want string
}

func runScriptCases(t *testing.T, cases []scriptCase) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runScripts(t, tc.srcs...))
		})
	}
}

func TestSloppyThis(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"top level", []string{`this === globalThis`}, "true"},
		{"undefined receiver", []string{`function f() { return this; } f() === globalThis`}, "true"},
		{"null receiver", []string{`function f() { return this; } f.call(null) === globalThis`}, "true"},
		{"primitive boxed", []string{`function f() { return typeof this; } f.call(1) + f.call("s") + f.call(true)`}, "objectobjectobject"},
		{"object kept", []string{`var o = {}; function f() { return this; } f.call(o) === o`}, "true"},
		{"strict function", []string{`function f() { "use strict"; return this; } f() === undefined && f.call(1) === 1`}, "true"},
		{"strict script", []string{`"use strict"; function f() { return this; } f() === undefined`}, "true"},
		{"arrow captures coerced", []string{`function f() { return (() => this)(); } f.call(2) instanceof Number`}, "true"},
		{"generator", []string{`function* g() { yield this; } g.call(undefined).next().value === globalThis`}, "true"},
		{"async", []string{`var r; async function a() { r = this; } a.call(3); r instanceof Number`}, "true"},
		{"class body strict", []string{`class C { m() { return this; } } new C().m.call(undefined) === undefined`}, "true"},
		// CoerceThis reruns the frame from the next instruction (sloppy.go).
		{"handler after coercion", []string{`function f() { try { throw this; } catch (e) { return e === this && e instanceof Number; } } f.call(4)`}, "true"},
		{"parameter scope", []string{`function f(a = 1) { var v = 2; return [this instanceof Number, (() => a)(), (() => v)()].join(); } f.call(5)`}, "true,1,2"},
		{"loop after coercion", []string{`function f(n) { var s = 0; for (var i = 0; i < n; i++) s += this.k; return s; } f.call({ k: 2 }, 10)`}, "20"},
		{"deep recursion", []string{`function f(n) { return n === 0 ? this === globalThis : f(n - 1); } f(500)`}, "true"},
	})
}

// TestScriptCompletion checks a script's completion value, which only
// expression statements set; the statements whose completion is
// UpdateEmpty(C, undefined) yield undefined when their body has no value,
// and break and continue carry the value of the statements before them.
func TestScriptCompletion(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"expression", []string{`1; 2 + 3`}, "5"},
		{"declaration empty", []string{`1; var x;`}, "1"},
		{"directive", []string{`"use strict"; var x;`}, "use strict"},
		{"if value", []string{`1; if (true) { 2; }`}, "2"},
		{"if empty body", []string{`1; if (true) {}`}, "undefined"},
		{"if false", []string{`1; if (false) 2;`}, "undefined"},
		{"else value", []string{`1; if (false) 2; else { 3; var z; }`}, "3"},
		{"if then declaration", []string{`1; if (true) {} var y;`}, "undefined"},
		{"if function", []string{`1; if (true) function g() {}`}, "undefined"},
		{"strict if", []string{`"use strict"; 1; if (true) {}`}, "undefined"},
		{"while false", []string{`1; while (false);`}, "undefined"},
		{"while false body", []string{`1; while (false) { 2; }`}, "undefined"},
		{"while value", []string{`1; var i = 0; while (i < 2) { i++; }`}, "1"},
		{"for false", []string{`1; for (;false;);`}, "undefined"},
		{"do while", []string{`1; do ; while (false)`}, "undefined"},
		{"do while value", []string{`1; do { 2; } while (false)`}, "2"},
		{"for-in null", []string{`1; for (var k in null);`}, "undefined"},
		{"for-in value", []string{`1; for (var k in {a: 1}) { k; }`}, "a"},
		{"for-of empty", []string{`1; for (var k of []);`}, "undefined"},
		{"break value", []string{`1; while (true) { 2; break; }`}, "2"},
		{"break empty", []string{`1; while (true) break;`}, "undefined"},
		{"continue after if", []string{`1; for (var i = 0; i < 2; i++) { if (i) continue; 3; }`}, "undefined"},
		{"continue value", []string{`1; for (var i = 0; i < 2; i++) { 3; continue; }`}, "3"},
		{"continue outer from inner loop", []string{`1; outer: for (var i = 0; i < 2; i++) { 3; for (;;) continue outer; }`}, "undefined"},
		{"continue outer value", []string{`1; outer: for (var i = 0; i < 2; i++) { for (;;) { 4; continue outer; } }`}, "4"},
		{"break outer from inner loop", []string{`1; outer: for (var i = 0; i < 2; i++) { 5; for (;;) break outer; }`}, "undefined"},
		{"labelled block break", []string{`1; L: { 2; break L; }`}, "2"},
		{"labelled break empty", []string{`1; L: break L;`}, "1"},
		{"labelled if break", []string{`1; L: if (true) break L;`}, "undefined"},
		{"labelled block if break", []string{`1; L: { 2; if (true) break L; }`}, "undefined"},
		{"switch empty", []string{`1; switch (1) {}`}, "undefined"},
		{"switch no match", []string{`1; switch (2) { case 1: 2; }`}, "undefined"},
		{"switch declaration", []string{`1; switch (1) { case 1: var s; }`}, "undefined"},
		{"switch break", []string{`1; switch (1) { case 1: break; }`}, "undefined"},
		{"switch fallthrough", []string{`1; switch (1) { case 1: 2; case 2: 3; break; }`}, "3"},
		{"try finally empty", []string{`1; try {} finally {}`}, "undefined"},
		{"finally value discarded", []string{`1; try { 2; } finally { 3; }`}, "2"},
		{"finally declaration", []string{`1; try { 2; } finally { var q = 3; }`}, "2"},
		{"try catch value", []string{`1; try { 2; } catch (e) { 3; }`}, "2"},
		{"catch discards try", []string{`1; try { 2; throw 0; } catch (e) {}`}, "undefined"},
		{"catch finally", []string{`1; try { 2; throw 0; } catch (e) { 3; } finally { 4; }`}, "3"},
		{"break in finally", []string{`1; L: try { 2; } finally { 3; break L; }`}, "3"},
		{"break in finally empty", []string{`1; L: try { 2; } finally { break L; }`}, "undefined"},
		{"break through finally", []string{`1; L: try { 2; break L; } finally { 3; }`}, "2"},
		{"break through finally empty", []string{`1; L: try { break L; } finally { 3; }`}, "undefined"},
		{"continue through finally", []string{`1; for (var i = 0; i < 2; i++) { try { 6; continue; } finally { 7; } }`}, "6"},
		{"with value", []string{`1; with ({}) { 2; }`}, "2"},
		{"with empty", []string{`1; with ({}) {}`}, "undefined"},
	})
}

// TestSloppyTypeofGlobalTDZ checks that typeof of a global lexical binding
// left uninitialized by a failed script throws in a later script.
func TestSloppyTypeofGlobalTDZ(t *testing.T) {
	r := NewRealm()
	require.Equal(t, "Error: x", runScriptsIn(t, r, `let tm = (() => { throw new Error("x"); })();`))
	require.Equal(t, "ReferenceError: Cannot access 'tm' before initialization", runScriptsIn(t, r, `typeof tm`))
	require.Equal(t, "ReferenceError: Cannot access 'tm' before initialization", runScriptsIn(t, r, `var tv; typeof tm`))
}

func TestSloppyGlobals(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"implicit global", []string{`x = 1; Object.getOwnPropertyDescriptor(globalThis, "x").configurable`}, "true"},
		{"implicit global in function", []string{`function f() { y = 2; } f(); y`}, "2"},
		{"strict unresolvable", []string{`"use strict"; z = 1`}, "ReferenceError: z is not defined"},
		{"var property", []string{`var v = 1; var d = Object.getOwnPropertyDescriptor(globalThis, "v"); [d.value, d.writable, d.enumerable, d.configurable].join()`}, "1,true,true,false"},
		{"function property", []string{`function g() {} typeof globalThis.g + Object.getOwnPropertyDescriptor(globalThis, "g").configurable`}, "functionfalse"},
		{"function hoisted", []string{`var early = typeof h; function h() {} early`}, "function"},
		{"last function wins", []string{`function k() { return 1; } function k() { return 2; } k()`}, "2"},
		{"let not a property", []string{`let l = 1; "l" in globalThis`}, "false"},
		{"let across scripts", []string{`let a = 1; const b = 2; class C {}`, `a + b + typeof C`}, "3function"},
		{"let shadows property", []string{`globalThis.s = 1;`, `let s = 2;`, `s + globalThis.s`}, "3"},
		{"let tdz across scripts", []string{`let q = f(); function f() { return q; }`}, "ReferenceError: Cannot access 'q' before initialization"},
		{"let tdz later script", []string{`try { let w = (() => { throw 1; })(); } catch (e) {} function rw() { return w; }`, `typeof rw`}, "function"},
		{"const assign", []string{`const c = 1;`, `c = 2`}, "TypeError: Assignment to constant variable."},
		{"redeclare let", []string{`let r = 1;`, `var r;`}, "SyntaxError: Identifier 'r' has already been declared"},
		{"redeclare let let", []string{`let r2 = 1;`, `let r2 = 2;`}, "SyntaxError: Identifier 'r2' has already been declared"},
		{"restricted global", []string{`let undefined;`}, "SyntaxError: Identifier 'undefined' has already been declared"},
		// ES2025 16.1.7 has no [[VarNames]] (proposal-redeclarable-global-eval-vars):
		// a let may shadow a var whose property is configurable.
		{"let over configurable var", []string{`globalThis.vp = 1;`, `var vp;`, `let vp = 3; vp + globalThis.vp`}, "4"},
		{"let over deleted var", []string{`globalThis.vd = 1;`, `var vd; delete globalThis.vd;`, `let vd = 5; vd`}, "5"},
		{"let over annex B var", []string{`{ function bf() {} }`, `let bf;`}, "SyntaxError: Identifier 'bf' has already been declared"},
		{"let over function", []string{`function lf() {}`, `let lf;`}, "SyntaxError: Identifier 'lf' has already been declared"},
		{"failed gdi creates nothing", []string{`let u1 = 1;`, `var nv; let u1;`, `typeof nv`}, "SyntaxError: Identifier 'u1' has already been declared"},
		{"frozen assign silent", []string{`var o = Object.freeze({p: 1}); o.p = 2; o.q = 3; o.p + "" + o.q`}, "1undefined"},
		{"frozen assign strict", []string{`"use strict"; var o = Object.freeze({p: 1}); o.p = 2`}, "TypeError: Cannot assign to read only property 'p' of object"},
		{"primitive property silent", []string{`var s = "x"; s.p = 1; s.p`}, "undefined"},
		{"getter only silent", []string{`var o = { get g() { return 1; } }; o.g = 2; o.g`}, "1"},
		{"elem silent", []string{`var a = Object.freeze([1]); a[0] = 2; a[5] = 1; a.join()`}, "1"},
		{"null base throws", []string{`var n = null; n.p = 1`}, "TypeError: Cannot set property 'p' of null"},
		{"super set silent", []string{`var o = { m() { super.x = 1; Object.freeze(o); super.y = 2; super["z"] = 3; return Object.keys(o).join(); } }; o.m()`}, "m,x"},
		{"super set strict", []string{`var o = { m() { "use strict"; Object.freeze(o); super.y = 2; } }; o.m()`}, "TypeError: Cannot assign to read only property 'y' of object"},
		{"delete non-configurable", []string{`var o = Object.freeze({p: 1}); delete o.p`}, "false"},
		{"delete elem", []string{`delete Object.freeze([1])[0]`}, "false"},
		{"delete global var", []string{`var dv; delete dv`}, "false"},
		{"typeof skipped block function", []string{`Object.preventExtensions(globalThis);`, `{ function tf() {} } typeof tf`}, "undefined"},
		{"typeof deleted global var", []string{`globalThis.tv = 1;`, `var tv; delete globalThis.tv; typeof tv`}, "undefined"},
		{"typeof deleted global function", []string{`globalThis.tg = 1;`, `function tg() {}`, `delete globalThis.tg; typeof tg`}, "function"},
		{"typeof global var", []string{`var tw = 1; typeof tw`}, "number"},
		{"typeof global let tdz", []string{`typeof tl; let tl;`}, "ReferenceError: Cannot access 'tl' before initialization"},
		{"typeof global let", []string{`const tc = 1;`, `typeof tc`}, "number"},
		{"delete implicit global", []string{`ig = 1; [delete ig, typeof ig].join()`}, "true,undefined"},
		{"delete local", []string{`(function () { var l; return delete l; })()`}, "false"},
		{"delete unresolvable", []string{`delete nothingHere`}, "true"},
		{"delete global lexical", []string{`let dl = 1; delete dl`}, "false"},
		{"named function expression", []string{`var f = function g() { g = 1; return typeof g; }; f()`}, "function"},
		{"named function expression update", []string{`var f = function g() { g++; return typeof g; }; f()`}, "function"},
		{"named function expression strict", []string{`var f = function g() { "use strict"; g = 1; }; f()`}, "TypeError: Assignment to constant variable."},
	})
}

// TestOffTableDispatch covers the ops asyncOp's default case routes by
// opcode range: async iteration to asyncIterOp, ImportCall to moduleOp, the
// sloppy ops to sloppyOp, and an opcode past the last to the unknown-opcode
// error.
func TestOffTableDispatch(t *testing.T) {
	r := NewRealm()
	got := runScriptsIn(t, r, `var out = [], o = {x: 2};
		async function* ag() { yield 1; yield 2; }
		(async function () { for await (var v of ag()) out.push(v); })();
		out.push(import("m") instanceof Promise);
		g = 3; with (o) { out.push(x + g); } function f() { return typeof this; } out.push(f());`, `out.join()`)
	assert.Equal(t, "true,5,object,1,2", got)
	bad := bytecode.Function{Name: "bad", Strict: true, Kind: bytecode.KindNormal,
		Code: []uint32{bytecode.EncodeABC(bytecode.Op(bytecode.OpCount), 0, 0, 0)}}
	f, _ := testClosure(r, &bad)
	_, err := r.CallObject(f, Undefined(), nil)
	assert.Equal(t, "TypeError: internal: unknown opcode", errorString(err))
}

// TestSloppySharedIntrinsics covers writes to the frozen intrinsics a
// shared realm uses: sloppy code ignores them as for any frozen object,
// strict code throws the shared-intrinsic TypeError, and no realm sees them.
func TestSloppySharedIntrinsics(t *testing.T) {
	for _, tc := range []scriptCase{
		{"new property", []string{`Array.prototype.polluted = 1; typeof Array.prototype.polluted`}, "undefined"},
		{"polyfill idiom", []string{`String.prototype.trim = String.prototype.trim || function () {}; " a ".trim()`}, "a"},
		{"replace method", []string{`String.prototype.trim = function () { return 1; }; " a ".trim()`}, "a"},
		{"namespace constant", []string{`Math.PI = 3; Math.PI`}, "3.141592653589793"},
		{"Object.prototype", []string{`Object.prototype.x = 1; typeof ({}).x`}, "undefined"},
		{"index", []string{`Array.prototype[0] = 1; [,][0] === undefined && !Array.prototype.hasOwnProperty(0)`}, "true"},
		{"computed key", []string{`var k = "p"; Math[k] = 1; typeof Math.p`}, "undefined"},
		{"array length", []string{`Array.prototype.length = 5; Array.prototype.length`}, "0"},
		{"global prototype", []string{`globalThis.__proto__.y = 1; Object.getPrototypeOf(globalThis) === Object.prototype && typeof y`}, "undefined"},
		{"setter not run", []string{`Error.prototype.stack = "s"; Error.prototype.name = "X"; new Error("m").name`}, "Error"},
		// Rejected before any setter on the chain runs (DESIGN §8.1), even
		// the __proto__ and %ThrowTypeError% setters, which throw in a
		// mutable realm (NOTES, "Wave 9 sloppy notes").
		{"intrinsic setters not run", []string{`Object.prototype.__proto__ = {}; Math.__proto__ = {}; Function.prototype.caller = 1; Object.getPrototypeOf(Math) === Object.prototype`}, "true"},
		{"compound", []string{`Math.PI += 1; Math.E++; [Math.PI, Math.E].join()`}, "3.141592653589793,2.718281828459045"},
		{"with", []string{`with (Array.prototype) { push = 1; } with (Math) { PI = 3; } [typeof Array.prototype.push, Math.PI].join()`}, "function,3.141592653589793"},
		{"super receiver", []string{`var o = { m() { super.q = 1; } }; o.m.call(Array.prototype); typeof Array.prototype.q`}, "undefined"},
		{"this receiver", []string{`function f() { this.z = 1; } f.call(Math); typeof Math.z`}, "undefined"},
		{"delete", []string{`[delete Array.prototype.push, delete Math.PI, typeof Array.prototype.push].join()`}, "false,false,function"},
		{"Reflect.set", []string{`[Reflect.set(Math, "PI", 3), Reflect.set(Array.prototype, "q", 1)].join()`}, "false,false"},
		{"proxy without set trap", []string{`var p = new Proxy(Math, {}); p.PI = 3; p.fresh = 1; var q = new Proxy(new Proxy(Array.prototype, {}), {}); q[0] = 1; [p.PI, typeof Math.fresh, Array.prototype.hasOwnProperty(0), Reflect.set(p, "PI", 3)].join()`}, "3.141592653589793,undefined,false,false"},
		{"proxy strict", []string{`"use strict"; new Proxy(Math, {}).PI = 3`}, "TypeError: 'set' on proxy: trap returned falsish for property 'PI'"},
		{"Reflect.defineProperty", []string{`[Reflect.defineProperty(Math, "PI", {value: 3}), Reflect.defineProperty(Array.prototype, "q", {value: 1}), typeof Array.prototype.q, Reflect.defineProperty(new Proxy(Math, {}), "e", {value: 1})].join()`}, "false,false,undefined,false"},
		{"proxy defineProperty", []string{`try { Object.defineProperty(new Proxy(Math, {}), "PI", {value: 3}); } catch (e) { e.name }`}, "TypeError"},
		// A descriptor that changes nothing succeeds, as on any frozen
		// object (ValidateAndApplyPropertyDescriptor).
		{"compatible descriptor", []string{`var d = Object.getOwnPropertyDescriptor(Array.prototype, "push");
			[Reflect.defineProperty(Array.prototype, "push", d), Reflect.defineProperty(new Proxy(Array.prototype, {}), "push", d),
			 Reflect.defineProperty(Math, "PI", {}), Reflect.defineProperty(Math, "PI", {value: Math.PI, writable: false, enumerable: false}),
			 Object.defineProperty(Math, "PI", {value: Math.PI}) === Math, Object.defineProperty(new Proxy(Math, {}), "PI", {configurable: false}) !== Math,
			 Object.defineProperties(Array.prototype, {push: d, length: {value: 0}}) === Array.prototype, Reflect.defineProperty(Array.prototype, "length", {value: 0}),
			 Reflect.defineProperty(Array.prototype, "length", {value: 1}), Reflect.defineProperty(Math, "PI", {configurable: true}),
			 Reflect.defineProperty(Math, "PI", {get: Math.max}), Reflect.defineProperty(Math, "fresh", {}), Array.prototype.push === d.value].join()`},
			"true,true,true,true,true,true,true,true,false,false,false,false,true"},
		{"incompatible descriptor", []string{`try { Object.defineProperty(Array.prototype, "push", {value: Array.prototype.pop}); } catch (e) { e.message }`},
			"Cannot modify property 'push' of shared intrinsic [object Array]"},
		{"strict", []string{`"use strict"; Array.prototype.polluted = 1`}, "TypeError: Cannot modify property 'polluted' of shared intrinsic [object Array]"},
		{"strict index", []string{`"use strict"; Array.prototype[0] = 1`}, "TypeError: Cannot modify property '0' of shared intrinsic [object Array]"},
		{"strict function", []string{`function f() { "use strict"; Math.PI = 3; } f()`}, "TypeError: Cannot modify property 'PI' of shared intrinsic [object Object]"},
		{"strict super receiver", []string{`var o = { m() { "use strict"; super.q = 1; } }; o.m.call(Array.prototype)`}, "TypeError: Cannot modify property 'q' of shared intrinsic [object Array]"},
		{"strict delete", []string{`"use strict"; delete Math.PI`}, "TypeError: Cannot delete property 'PI' of [object Object]"},
		{"Object.assign", []string{`Object.assign(Math, { PI: 3 })`}, "TypeError: Cannot modify property 'PI' of shared intrinsic [object Object]"},
		{"push", []string{`Array.prototype.push.call(Array.prototype, 1)`}, "TypeError: Cannot modify property '0' of shared intrinsic [object Array]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runScriptsIn(t, NewRealmWith(RealmOptions{SharedIntrinsics: true}), tc.srcs...))
		})
	}
	// The template is untouched: a fresh shared realm sees none of it.
	assert.Equal(t, "undefined,undefined,undefined,3.141592653589793,function,0",
		runScriptsIn(t, NewRealmWith(RealmOptions{SharedIntrinsics: true}),
			`[typeof Array.prototype.polluted, typeof ({}).x, typeof Array.prototype.q, Math.PI, typeof Array.prototype.push, Array.prototype.length].join()`))
}

func TestSloppyWith(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"read", []string{`var o = {a: 1}; with (o) a`}, "1"},
		{"write", []string{`var o = {a: 1}; with (o) { a = 2; } o.a`}, "2"},
		{"miss falls through", []string{`var b = 3; with ({}) b`}, "3"},
		{"var init targets object", []string{`var o = {v: 0}; with (o) { var v = 5; } [o.v, v].join()`}, "5,"},
		{"nested", []string{`with ({a: 1}) with ({b: 2}) a + b`}, "3"},
		{"inner shadows", []string{`with ({a: 1}) with ({a: 2}) a`}, "2"},
		{"call this", []string{`var o = { f() { return this; } }; with (o) f() === o`}, "true"},
		{"unscopables", []string{`var a = "outer"; var o = {a: "inner", [Symbol.unscopables]: {a: true}}; with (o) a`}, "outer"},
		{"array unscopables", []string{`var keys = "outer"; with ([]) typeof keys`}, "string"},
		{"typeof missing", []string{`with ({}) typeof nothing`}, "undefined"},
		{"compound", []string{`var o = {n: 1}; with (o) { n += 2; n++; } o.n`}, "4"},
		{"logical", []string{`var o = {n: 0}; with (o) { n ||= 7; } o.n`}, "7"},
		{"function local", []string{`function f(o) { var x = 1; with (o) { return x; } } f({x: 2}) + f({})`}, "3"},
		{"closure", []string{`var o = {x: 1}; var g; with (o) { g = function () { return x; }; } o.x = 5; g()`}, "5"},
		{"arrow closure", []string{`var o = {x: 1}; var g; with (o) g = () => x; o.x = 6; g()`}, "6"},
		{"delete", []string{`var o = {p: 1}; with (o) { delete p; } "p" in o`}, "false"},
		{"primitive object", []string{`with ("abc") length`}, "3"},
		{"null object", []string{`with (null) {}`}, "TypeError: Cannot convert undefined or null to object"},
		{"let inside", []string{`with ({x: 1}) { let x = 2; x; }`}, "2"},
		{"undefined not folded", []string{`with ({undefined: 1}) undefined`}, "1"},
		{"binding removed before get", []string{`var o = {p: 1}; var r; with (o) { r = (delete o.p, p); } r`}, "ReferenceError: p is not defined"},
		{"completion empty", []string{`1; with ({}) {}`}, "undefined"},
		{"const outside", []string{`const c = 1; var o = {c: 0}; with (o) { c = 2; } o.c`}, "2"},
		{"tagged template", []string{`var o = { t() { return this === o; } }; with (o) t` + "``"}, "true"},
		{"pattern target resolved first", []string{`var log = []; var env = new Proxy({}, { has(t, k) { log.push(String(k)); return false; } });
var src = { get p() { log.push("get"); return undefined; } }; var t, d = 1;
with (env) { var { p: t = d } = src; ({ p: t = d } = src); [t = d] = [src.p]; }
log.join()`}, "src,t,get,d,src,t,get,d,src,get,t,d"},
		{"pattern default named", []string{`var o = {}; with (o) { var { f = function () {} } = {}; } f.name`}, "f"},
		{"proxy has", []string{`var log = []; var p = new Proxy({}, { has(t, k) { log.push(String(k)); return false; } }); var z = 1; with (p) z; log.join()`}, "z"},
	})
}

func TestMappedArguments(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"param to arguments", []string{`function f(a) { a = 2; return arguments[0]; } f(1)`}, "2"},
		{"arguments to param", []string{`function f(a) { arguments[0] = 3; return a; } f(1)`}, "3"},
		{"unpassed not mapped", []string{`function f(a) { a = 2; return arguments[0]; } typeof f()`}, "undefined"},
		{"delete unmaps", []string{`function f(a) { delete arguments[0]; arguments[0] = 5; return a; } f(1)`}, "1"},
		{"define unmaps", []string{`function f(a) { Object.defineProperty(arguments, "0", {value: 7, writable: false}); a = 8; return arguments[0]; } f(1)`}, "7"},
		{"callee", []string{`function f() { return arguments.callee === f; } f()`}, "true"},
		{"strict unmapped", []string{`function f(a) { "use strict"; a = 2; return arguments[0]; } f(1)`}, "1"},
		{"defaults unmapped", []string{`function f(a = 0) { a = 2; return arguments[0]; } f(1)`}, "1"},
		{"closure mapped", []string{`function f(a) { var g = () => a; arguments[0] = 9; return g(); } f(1)`}, "9"},
		{"duplicate params", []string{`function f(a, a) { return a; } f(1, 2)`}, "2"},
		{"toString tag", []string{`function f() { return Object.prototype.toString.call(arguments); } f()`}, "[object Arguments]"},
	})
}

func TestAnnexBFunctions(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"block function var", []string{`{ function f() { return 1; } } f()`}, "1"},
		{"before block undefined", []string{`var t = typeof f; { function f() {} } t`}, "undefined"},
		{"if function", []string{`if (true) function g() { return 2; } g()`}, "2"},
		{"in function", []string{`function o() { { function i() { return 3; } } return i(); } o()`}, "3"},
		{"blocked by let", []string{`let f = 1; { function f() {} } f`}, "1"},
		{"catch param redeclare", []string{`try { throw 1; } catch (e) { var e = 2; } typeof e`}, "undefined"},
		{"labelled", []string{`l: function lf() { return 4; } lf()`}, "4"},
		{"strict no annex b", []string{`"use strict"; { function f() {} } typeof f`}, "undefined"},
	})
}

func TestAnnexBForInInitializer(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"no keys", []string{`for (var x = 1 in {}) ; x`}, "1"},
		{"overwritten by keys", []string{`var r = []; for (var x = 1 in {a: 1}) r.push(x); r.join() + x`}, "aa"},
		{"before the object", []string{`var s; for (var a = 0 in s = a, {}); s`}, "0"},
		{"evaluated once", []string{`var e = 0, n = 0, s; for (var a = (++e, -1) in s = a, {a: 0, b: 1, c: 2}) ++n; [s, e, n].join()`}, "-1,1,3"},
		{"named", []string{`for (var f = function () {} in {}); f.name`}, "f"},
		{"arrow", []string{`var b = {k: 1}, a = 7, r; for (var f = () => a in {}) r = f(); for (var g = (x) => x in b) ; [r, f(), f.name, g].join()`}, ",7,f,k"},
		{"through with", []string{`var o = {x: 0}; with (o) { for (var x = 3 in {}); } [o.x, x].join()`}, "3,"},
		{"in function", []string{`(function () { for (var v = 2 in null); return v; })()`}, "2"},
	})
}

func TestAnnexBCallAssignmentTargets(t *testing.T) {
	// The call runs, then a ReferenceError is thrown before the value is
	// evaluated or the call's result converted.
	const f = `var calls = 0, rhs = 0; function f() { calls++; return { valueOf() { throw "converted"; } }; } `
	const report = ` } catch (e) { [e.name, calls, rhs].join(); }`
	runScriptCases(t, []scriptCase{
		{"assignment", []string{f + `try { f() = rhs++;` + report}, "ReferenceError,1,0"},
		{"message", []string{f + `try { f() = 1; } catch (e) { e.message }`}, "Invalid left-hand side in assignment"},
		{"compound", []string{f + `try { f() *= rhs++;` + report}, "ReferenceError,1,0"},
		{"postfix", []string{f + `var x; try { x = f()++;` + report}, "ReferenceError,1,0"},
		{"prefix", []string{f + `try { --f();` + report}, "ReferenceError,1,0"},
		{"parenthesized", []string{f + `try { (f()) = rhs++;` + report}, "ReferenceError,1,0"},
		{"for-in", []string{f + `try { for (f() in {a: 1}) rhs++;` + report}, "ReferenceError,1,0"},
		{"for-in no keys", []string{f + `for (f() in {}) ; calls`}, "0"},
		{"for-of closes", []string{f + `var closed = 0; var it = { [Symbol.iterator]() { return { next() { return {done: false}; }, return() { closed++; return {}; } }; } };
try { for (f() of it) rhs++; } catch (e) { [e.name, calls, closed].join(); }`}, "ReferenceError,1,1"},
		{"call named async", []string{`function async() {} try { async() = 1; } catch (e) { e.name }`}, "ReferenceError"},
		{"strict", []string{`"use strict"; f() = 1;`}, "SyntaxError: script.js:1:15: SyntaxError: Invalid left-hand side in assignment"},
	})
}

// TestDestructuringKeyOrder checks that a computed key is converted before
// a member target is evaluated (KeyedDestructuringAssignmentEvaluation).
func TestDestructuringKeyOrder(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"member target", []string{`"use strict"; var log = [];
var key = { toString() { log.push("key"); return "p"; } };
var target = { set q(v) { log.push("set"); } };
function obj() { log.push("target"); return target; }
({ [key]: obj().q } = { get p() { log.push("get"); } });
log.join()`}, "key,target,get,set"},
		{"identifier target", []string{`"use strict"; var log = [], x;
({ [{ toString() { log.push("key"); return "p"; } }]: x } = { p: 1 });
log.join() + x`}, "key1"},
	})
}

func TestSloppyCompileGuards(t *testing.T) {
	// A sloppy function declaration named like a global lexical stays local.
	assert.Equal(t, "number", runScripts(t, `let n = 1;`, `{ function n() {} } typeof n`))
	// with is a syntax error in strict code.
	assert.Contains(t, runScripts(t, `"use strict"; with ({}) {}`), "SyntaxError")
}

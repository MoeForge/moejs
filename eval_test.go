package moejs_test

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runScript runs src in a fresh runtime and returns its completion value as
// a Go value.
func runScript(t *testing.T, src string) any {
	t.Helper()
	rt := moejs.NewRuntime(moejs.Options{})
	v, err := rt.RunScript(mustScript(t, src))
	require.NoError(t, err)
	got, err := rt.ToGo(v)
	require.NoError(t, err)
	return got
}

func TestDirectEval(t *testing.T) {
	// big declares enough variables that eval code is resolved against the
	// names it uses rather than all its caller's (syntax's resolveEval).
	const big = "var v0, v1, v2, v3, v4, v5, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15, v16;"
	tests := []struct {
		name, src string
		want      any
	}{
		{"completion value", `eval("1; 2 + 3")`, int64(5)},
		{"non-string argument", `eval(42)`, int64(42)},
		{"no argument", `eval()`, nil},
		{"caller's bindings", `function f(a) { let l = 2; return eval("a * l"); } f(3)`, int64(6)},
		{"global lexical", `let gl = 3; eval("gl + 1")`, int64(4)},
		{"sloppy var goes to the caller", `function f() { eval("var v = 1"); return typeof v; } f()`, "number"},
		{"var of an existing name", `function f() { var s = 1; eval("var s = 2"); return s; } f()`, int64(2)},
		{"evals share vars", `function f() { eval("var e1 = 1"); eval("var e2 = e1 + 1"); return e2; } f()`, int64(2)},
		{"strict caller", `function f() { "use strict"; eval("var v = 1"); return typeof v; } f()`, "undefined"},
		{"strict eval code", `function f() { eval("'use strict'; var v = 1"); return typeof v; } f()`, "undefined"},
		{"let stays in the eval", `eval("let l = 1"); typeof l`, "undefined"},
		{"global var is configurable", `eval("var g = 1"); delete globalThis.g`, true},
		{"global function", `eval("function gf() { return 'gf'; }"); gf()`, "gf"},
		{"local function", `function f() { eval("function inner() { return 'in'; }"); return inner(); } f()`, "in"},
		{"eval var is deletable", `function f() { eval("var d = 1"); return [delete d, typeof d]; } f()`, []any{true, "undefined"}},
		{"Annex B block function", `eval("if (true) { function blk() { return 'b'; } }"); blk()`, "b"},
		{"implicit global", `function f() { eval("ig = 5"); return ig; } f()`, int64(5)},
		{"parameter expressions", `function f(a = eval("var pv = 2"), b = pv) { return b; } f()`, int64(2)},
		{"arrow this", `var ar = () => eval("this === globalThis"); ar()`, true},
		{"class field", `class F { x = eval("this.constructor.name"); } new F().x`, "F"},
		{"arguments", `function f() { return eval("arguments.length"); } f(1, 2, 3)`, int64(3)},
		{"new.target", `function F() { this.nt = eval("new.target === F"); } new F().nt`, true},
		{"super property", `class B { m() { return "bm"; } } class C extends B { m() { return eval("super.m()"); } } new C().m()`, "bm"},
		{"super call", `class D extends Object { x = 2; constructor() { eval("super()"); this.v = this.x + 1; } } new D().v`, int64(3)},
		{"with", `var o = {p: 1}; with (o) { eval("p + 1"); }`, int64(2)},
		{"var through with", `var o = {w: 1}; function f() { with (o) { eval("var w = 5"); } return [o.w, typeof w]; } f()`, []any{int64(5), "undefined"}},
		{"closure over an eval var", `function f() { eval("var c = 1"); return () => c++; } var g = f(); g(); g()`, int64(2)},
		{"nested eval", `function f() { var n = 1; return eval("eval('n + 1')"); } f()`, int64(2)},
		{"nested eval, large environment", `function f() { var n = 1; ` + big + ` return eval("eval('n + 1')"); } f()`, int64(2)},
		{"large environment", `class B { m() { return 2; } }
class C extends B { #x = 3; m(a) { ` + big + ` return eval("[a, super.m(), this.#x, arguments.length, new.target]"); } }
new C().m(4, 5)`, []any{int64(4), int64(2), int64(3), int64(2), nil}},
		{"with, large environment", `var o = {p: 1}; function f() { ` + big + ` with (o) { return eval("p + 1"); } } f()`, int64(2)},
		{"var conflict, large environment", `function f() { let q = 1; ` + big + ` try { eval("var q = 2"); } catch (e) { return e.name; } } f()`, "SyntaxError"},
		{"a local named eval", `function f(eval) { return eval("1"); } f(s => s + "!")`, "1!"},
		{"thrown errors", `try { eval("throw new TypeError('boom')"); } catch (e) { e.message }`, "boom"},
		{"syntax error", `try { eval("a b"); } catch (e) { e instanceof SyntaxError }`, true},
		{"in a loop", `var n = 0; for (var i = 0; i < 100; i++) n += eval("i"); n`, int64(4950)},
		{"template objects per evaluation", "function tag(t) { return t; } var ts = []; for (var i = 0; i < 2; i++) ts.push(eval('[tag`x`, tag`x`, (() => tag`y`)()]'));" +
			"[ts[0][0] === ts[1][0], ts[0][0] === ts[0][1], ts[0][2] === ts[1][2], eval('var s = []; for (var j = 0; j < 2; j++) s.push(tag`z`); s[0] === s[1]')]",
			[]any{false, false, false, true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runScript(t, tc.src))
		})
	}
}

func TestEvalEarlyErrors(t *testing.T) {
	tests := []struct{ name, src string }{
		{"new.target outside a function", `eval("new.target")`},
		{"super outside a method", `eval("super.x")`},
		{"super call outside a derived constructor", `class A { m() { eval("super()"); } } new A().m()`},
		{"arguments in a field initializer", `class G { x = eval("arguments"); } new G()`},
		{"redeclaring a caller's let", `function f() { let l; eval("var l"); } f()`},
		{"global var over a global let", `let gl2; eval("var gl2")`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, "SyntaxError", runScript(t, `try { `+tc.src+`; "none"; } catch (e) { e.name }`))
		})
	}
}

func TestIndirectEval(t *testing.T) {
	tests := []struct {
		name, src string
		want      any
	}{
		{"global scope", `var x = "global"; function f() { var x = "local"; return (0, eval)("x"); } f()`, "global"},
		{"declares globals", `function f() { (0, eval)("var ig2 = 9"); } f(); ig2`, int64(9)},
		{"global this", `(function () { "use strict"; return (0, eval)("this === globalThis"); })()`, true},
		{"sloppy in strict code", `(function () { "use strict"; return (0, eval)("var sv = 1; typeof sv"); })() + typeof sv`, "numbernumber"},
		{"through a variable", `var e = eval; function f() { var loc = 1; return e("typeof loc"); } f()`, "undefined"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runScript(t, tc.src))
		})
	}
}

// TestEvalIdentityAcrossRealms: a call is a direct eval when its callee is
// %eval% (SameValue, ES2025 13.3.6.1), whether or not the realm has read
// its eval global. Shared realms have one %eval%; a mutable realm's is its
// own, so another realm's eval is an ordinary call there.
func TestEvalIdentityAcrossRealms(t *testing.T) {
	const call = `function f(eval) { var loc = 1; return eval("typeof loc"); } f(ev)`
	for _, tc := range []struct {
		name    string
		mutable bool
		want    string
	}{
		{"shared", false, "number"},
		{"mutable", true, "undefined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := moejs.NewRuntime(moejs.Options{MutableIntrinsics: tc.mutable})
			b := moejs.NewRuntime(moejs.Options{MutableIntrinsics: tc.mutable}) // created before a reads eval
			ev, err := a.RunScript(mustScript(t, `eval`))
			require.NoError(t, err)
			c := moejs.NewRuntime(moejs.Options{MutableIntrinsics: tc.mutable}) // created after
			for _, rt := range []*moejs.Runtime{b, c} {
				require.NoError(t, rt.SetGlobal("ev", ev))
				v, err := rt.RunScript(mustScript(t, call))
				require.NoError(t, err)
				assert.Equal(t, tc.want, v.String(), "before the realm reads its eval global")
				same, err := rt.RunScript(mustScript(t, `ev === eval`))
				require.NoError(t, err)
				assert.Equal(t, !tc.mutable, same.AsBool())
				v, err = rt.RunScript(mustScript(t, call))
				require.NoError(t, err)
				assert.Equal(t, tc.want, v.String(), "after")
			}
			// The realm's own eval, under another name, before it reads the
			// global: a direct eval in every realm.
			d := moejs.NewRuntime(moejs.Options{MutableIntrinsics: tc.mutable})
			own, err := d.RunScript(mustScript(t, `function g(eval) { var loc = 1; return eval("typeof loc"); } g(globalThis["ev" + "al"])`))
			require.NoError(t, err)
			assert.Equal(t, "number", own.String())
		})
	}
}

func TestDynamicFunction(t *testing.T) {
	tests := []struct {
		name, src string
		want      any
	}{
		{"call", `new Function("a", "b", "return a + b")(2, 3)`, int64(5)},
		{"without new", `Function("a, b", "return a * b")(2, 3)`, int64(6)},
		{"toString", `Function("a, b", "return a + b").toString()`, "function anonymous(a, b\n) {\nreturn a + b\n}"},
		{"no parameters", `String(Function())`, "function anonymous(\n) {\n\n}"},
		{"name", `Function().name`, "anonymous"},
		{"anonymous is not bound", `new Function("return typeof anonymous")()`, "undefined"},
		{"global scope", `function f() { var loc = 1; return new Function("return typeof loc")(); } f()`, "undefined"},
		{"sloppy", `new Function("return this")() === globalThis`, true},
		{"strict body", `new Function("'use strict'; return this")()`, nil},
		{"generator", `Object.getPrototypeOf(function* () {}).constructor("a", "yield a * 2")(4).next().value`, int64(8)},
		{"generator toString", `String(Object.getPrototypeOf(function* () {}).constructor("yield 1"))`, "function* anonymous(\n) {\nyield 1\n}"},
		{"async", `Object.getPrototypeOf(async function () {}).constructor("return 1")() instanceof Promise`, true},
		{"async generator", `typeof Object.getPrototypeOf(async function* () {}).constructor("yield 1")()[Symbol.asyncIterator]`, "function"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runScript(t, tc.src))
		})
	}

	// The parameters and the body are each checked to be what they are.
	errs := []struct {
		name, ctor, params, body, msg string
	}{
		{"parameters close the list", "Function", `"}"`, `""`, ""},
		{"body closes the function", "Function", `""`, `"}{"`, ""},
		{"parameters end early", "Function", `"/*"`, `"*/){"`, "Arg string terminates parameters early"},
		{"body ends early", "Function", `""`, `"}; function x() {"`, "Function body ends the function early"},
		{"yield outside a generator", "(async function () {}).constructor", `""`, `"yield 1"`, ""},
		{"await in generator parameters", "(async function* () {}).constructor", `"a = await 1"`, `""`, ""},
	}
	for _, tc := range errs {
		t.Run(tc.name, func(t *testing.T) {
			got := runScript(t, `try { `+tc.ctor+`(`+tc.params+`, `+tc.body+`); "none"; } catch (e) { [e.name, e.message] }`)
			require.IsType(t, []any{}, got)
			assert.Equal(t, "SyntaxError", got.([]any)[0])
			if tc.msg != "" {
				assert.Equal(t, tc.msg, got.([]any)[1])
			}
		})
	}
}

// TestEvalReferrer: eval code and dynamic functions belong to the script
// that evaluates them (their stack frames name it), also when two scripts
// evaluate the same string. Code a job evaluates with no code of a script
// running is anonymous.
func TestEvalReferrer(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	s, err := moejs.CompileScript("a.js", `var stacks = []; Promise.resolve("new Error().stack").then(eval).then(s => stacks.push(s));
Promise.resolve("return new Error().stack").then(Function).then(f => stacks.push(f()));`)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	got := runIn(t, rt, `stacks`)
	require.Len(t, got, 2)
	for i, stack := range got.([]any) {
		frames := strings.Split(stack.(string), "\n")
		require.Greater(t, len(frames), 1)
		assert.Contains(t, frames[1], "<anonymous>", "string %d", i)
		assert.NotContains(t, frames[1], "a.js", "string %d", i)
	}
	for _, name := range []string{"b.js", "c.js"} {
		s, err := moejs.CompileScript(name, `[eval("new Error().stack"), (0, eval)("new Error().stack"), new Function("return new Error().stack")()]`)
		require.NoError(t, err)
		v, err := rt.RunScript(s)
		require.NoError(t, err)
		got, err := rt.ToGo(v)
		require.NoError(t, err)
		for i, stack := range got.([]any) {
			frames := strings.Split(stack.(string), "\n")
			require.Greater(t, len(frames), 1)
			assert.Contains(t, frames[1], name, "string %d", i)
		}
	}
}

func TestModuleEval(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `
import { count, inc } from "./counter.js";
inc();
export const seen = eval("count + typeof inc");
export const strict = eval("var leak = 1; typeof leak") + "," + typeof leak;
export const indirect = (0, eval)("typeof count");
const name = f => { try { f(); } catch (e) { return e.name; } };
export const meta = name(() => eval("import.meta")) + "," + name(() => Function("import.meta"));
`,
		"counter.js": `export let count = 0; export function inc() { count++; }`,
	})
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(h.mustLink("main.js")))
	for name, want := range map[string]string{
		"seen":     "1function", // an import read through the module's Env
		"strict":   "number,undefined",
		"indirect": "undefined",
		"meta":     "SyntaxError,SyntaxError",
	} {
		v, ok := rt.Export(name)
		require.True(t, ok, name)
		assert.Equal(t, want, v.String(), name)
	}
}

// TestEvalImportReferrer: import() in eval code and in the code of a
// Function constructor passes the referrer of the script or module that
// evaluates the code.
func TestEvalImportReferrer(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"dep.js":   `export const v = 1;`,
		"main.js":  `import { v } from "./dep.js"; eval("import('./dep.js')");`,
		"outer.js": `import "./inner.js";`,
		"inner.js": `eval("import('./dep.js')");`,
		"alone.js": `eval("import('./dep.js')");`,
		"hookless.js": `export let err;
eval("import('./dep.js')").catch(e => { err = e.name; });`,
	})
	var got []moejs.Referrer
	imp := &moejs.Importer{Resolve: func(referrer moejs.Referrer, _ string) (*moejs.Module, error) {
		got = append(got, referrer)
		return h.module("dep.js"), nil
	}}

	// A script records its referrer when its code uses import() or holds
	// a direct eval: the indirect evals and constructors of the others
	// import with none.
	scripts := []struct {
		name, src string
		self      bool // the referrer is the script, else nil
	}{
		{"direct eval", `eval("import('./dep.js')")`, true},
		{"nested eval", `eval("eval('import(\"./dep.js\")')")`, true},
		{"eval in a function", `function f() { return eval("import('./dep.js')"); } f()`, true},
		{"function of eval code", `eval("(function () { return import('./dep.js'); })")()`, true},
		{"function of reclaimed eval code", `var f = eval("(function () { return import('./dep.js'); })"); for (var i = 0; i < 300; i++) eval("({a: 1}).a + " + i); f()`, true},
		{"indirect eval", `(0, eval)("import('./dep.js')"); if (false) import("./dep.js");`, true},
		{"indirect eval in a direct one", `eval("(0, eval)('import(\"./dep.js\")')")`, true},
		{"Function constructor", `Function("return import('./dep.js')")(); if (false) import("./dep.js");`, true},
		{"async function constructor", `(async function () {}).constructor("await import('./dep.js')")(); eval("")`, true},
		{"indirect eval without a record", `(0, eval)("import('./dep.js')")`, false},
		{"Function constructor without a record", `Function("return import('./dep.js')")()`, false},
		// A job calling eval or a constructor runs no code of the script.
		{"eval called by a job", `Promise.resolve("import('./dep.js')").then(eval); eval("")`, false},
		{"Function constructor called by a job", `Promise.resolve("return import('./dep.js')").then(Function).then(f => f()); eval("")`, false},
	}
	for _, tc := range scripts {
		t.Run(tc.name, func(t *testing.T) {
			got = nil
			s, err := moejs.CompileScript("s.js", tc.src)
			require.NoError(t, err)
			_, err = moejs.NewRuntime(moejs.Options{Importer: imp}).RunScript(s)
			require.NoError(t, err)
			require.Len(t, got, 1)
			if tc.self {
				assert.Same(t, s, got[0])
			} else {
				assert.Nil(t, got[0])
			}
		})
	}

	t.Run("one string in two scripts", func(t *testing.T) {
		got = nil
		rt := moejs.NewRuntime(moejs.Options{Importer: imp})
		var want []moejs.Referrer
		for _, name := range []string{"b.js", "c.js"} {
			s, err := moejs.CompileScript(name, `(0, eval)("import('./dep.js')"); if (false) import("./dep.js");`)
			require.NoError(t, err)
			_, err = rt.RunScript(s)
			require.NoError(t, err)
			want = append(want, s)
		}
		require.Len(t, got, 2)
		assert.Same(t, want[0], got[0])
		assert.Same(t, want[1], got[1])
	})

	modules := []struct {
		name string
		load func() *moejs.Module
		want string // the module whose code holds the eval
	}{
		{"entry with imports", func() *moejs.Module { return h.mustLink("main.js") }, "main.js"},
		{"imported module", func() *moejs.Module { return h.mustLink("outer.js") }, "inner.js"},
		{"module alone", func() *moejs.Module { return h.module("alone.js") }, "alone.js"},
	}
	for _, tc := range modules {
		t.Run(tc.name, func(t *testing.T) {
			got = nil
			require.NoError(t, moejs.NewRuntime(moejs.Options{Importer: imp}).Load(tc.load()))
			require.Len(t, got, 1)
			assert.Same(t, h.module(tc.want), got[0])
		})
	}

	// A module whose only use of a record is a direct eval evaluates without
	// a graph when no Importer asks for one.
	t.Run("module alone without an Importer", func(t *testing.T) {
		m := h.module("hookless.js")
		linked, err := moejs.Link(m, nil)
		require.NoError(t, err)
		assert.Same(t, m, linked)
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.Load(m))
		v, ok := rt.Export("err")
		require.True(t, ok)
		assert.Equal(t, "TypeError", v.String())
	})
}

func TestEvalScript(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("evalScript", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return r.EvalScript("inner.js", args[0].String())
	})))
	v, err := rt.RunScript(mustScript(t, `var r = evalScript("var q = 3; let ql = 4; q + ql"); [r, q, typeof ql, globalThis.q]`))
	require.NoError(t, err)
	got, err := rt.ToGo(v)
	require.NoError(t, err)
	assert.Equal(t, []any{int64(7), int64(3), "number", int64(3)}, got)

	v, err = rt.RunScript(mustScript(t, `try { evalScript("a b"); } catch (e) { e.name }`))
	require.NoError(t, err)
	assert.Equal(t, "SyntaxError", v.String())
}

// TestEvalErrorUnwinding: a direct eval's error unwinds the calling frame
// like any other: an exception reaches the caller's handler, an interrupt
// passes it.
func TestEvalErrorUnwinding(t *testing.T) {
	assert.Equal(t, []any{"caught boom", int64(2)}, runScript(t, `function f() { var n = 1; try { eval("n++; throw 'boom'"); } catch (e) { return ["caught " + e, n]; } } f()`))

	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		rt.Interrupt("stop")
		return moejs.Undefined(), nil
	})))
	_, err := rt.RunScript(mustScript(t, `function f() { try { eval("stop(); for (;;) {}"); } catch (e) { return "caught"; } } f()`))
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	rt.ClearInterrupt()
}

// TestEvalGrowsStack: a direct eval that grows the register stack and the
// frame records leaves the calling frame's registers intact (the frame
// reruns on fresh ones).
func TestEvalGrowsStack(t *testing.T) {
	assert.Equal(t, []any{int64(3), int64(400), int64(3), int64(510)}, runScript(t, `
function deep(n) { return n ? deep(n - 1) + 1 : 0; }
function f(n) { var a = 1, b = 2, r = eval("deep(n)"); return [a + b, r, eval("a + b")]; }
var s = 0;
for (var i = 1; i < 9; i++) s += eval("deep(1 << i)");
f(400).concat(s)`))
}

// TestDynamicCompileTime: a program that compiles distinct sources pays the
// same per compile at the ten-thousandth as at the first, so nothing per
// compile grows with the code compiled before it (growIC used to sum the
// sites of every earlier tree, and the table grew to the exact size each
// time: the audit's p-icgrowth.js). The inline-cache table grows
// geometrically, and 200,000 compiles take seconds.
func TestDynamicCompileTime(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	caps := map[int]bool{}
	require.NoError(t, rt.SetGlobal("icCap", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		caps[cap(r.ICSlots())] = true
		return moejs.Undefined(), nil
	})))
	_, err := rt.RunScript(mustScript(t, `var k = 0`))
	require.NoError(t, err)
	batch := mustScript(t, `for (var i = 0; i < 500; i++, k++) { Function("o", "return o.a + o.b + " + k)({a: 1, b: 2}); icCap(); }`)
	run := func() time.Duration {
		t0 := time.Now()
		_, err := rt.RunScript(batch)
		require.NoError(t, err)
		return time.Since(t0)
	}
	best := func() time.Duration {
		return min(run(), run(), run())
	}
	run()
	first := best()
	for range 14 {
		run()
	}
	last := best()
	require.Less(t, last, 3*first+20*time.Millisecond, "a batch of 500 compiles took %v after 8,500 compiles, %v at first", last, first)
	require.Less(t, len(caps), 200, "the inline-cache table was reallocated %d times over 10,000 compiles", len(caps))
	if raceEnabled {
		return
	}
	t0 := time.Now()
	_, err = rt.RunScript(mustScript(t, `for (var i = 0; i < 200000; i++, k++) Function("o", "return o.a + o.b + " + k)({a: 1, b: 2});`))
	require.NoError(t, err)
	assert.Less(t, time.Since(t0), 60*time.Second)
}

// heapAfterGC returns the live heap after a full collection.
func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// TestDynamicCodeRetained: a runtime keeps a bounded amount of state for
// code compiled from strings that the program no longer runs, however much
// it compiles (the audit's TestAuditRetainedGrowth): the heap it retains
// after 20,000 iterations is within a small factor of that after 2,000,
// and its inline-cache table stops growing.
func TestDynamicCodeRetained(t *testing.T) {
	cases := []struct{ name, src string }{
		{"distinct eval strings", `for (var i = 0; i < N; i++) eval("({a:1}).a + " + i);`},
		{"65 eval strings round robin", `var srcs = []; for (var j = 0; j < 65; j++) srcs.push("({a:1}).a + " + j); for (var i = 0; i < N; i++) eval(srcs[i % 65]);`},
		{"tagged template eval in a loop", "function tag(t) { return t; } for (var i = 0; i < N; i++) eval('tag`x`.length');"},
		{"distinct direct eval strings", `function f(o, i) { return eval("o.a + " + i); } for (var i = 0; i < N; i++) f({a: 1}, i);`},
		{"distinct Function sources", `for (var i = 0; i < N; i++) Function("o", "return o.a + o.b + " + i)({a: 1, b: 2});`},
		{"65 Function sources round robin", `for (var i = 0; i < N; i++) Function("o", "return o.a + o.b + " + i % 65)({a: 1, b: 2});`},
		{"one Function source", `eval(""); for (var i = 0; i < N; i++) Function("o", "return o.a + o.b")({a: 1, b: 2});`},
		{"tagged template Function in a loop", "function tag(t) { return t; } for (var i = 0; i < N; i++) Function('return tag`x`.length')();"},
		{"distinct GeneratorFunction sources", `var G = Object.getPrototypeOf(function* () {}).constructor; for (var i = 0; i < N; i++) G("o", "yield o.a + " + i)({a: 1}).next();`},
		{"distinct async function sources", `var A = Object.getPrototypeOf(async function () {}).constructor; for (var i = 0; i < N; i++) A("o", "return await o.a + " + i)({a: 1});`},
		{"distinct EvalScript sources", `for (var i = 0; i < N; i++) evalScript("({a:1}).a + " + i);`},
		{"closures of running eval code", `eval("for (var i = 0; i < N; i++) (function (o) { return o.a; })({a: 1});");`},
	}
	measure := func(t *testing.T, src string, n int) (retained uint64, ic int) {
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.SetGlobal("N", n))
		require.NoError(t, rt.SetGlobal("evalScript", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			return r.EvalScript("inner.js", args[0].String())
		})))
		s := mustScript(t, src)
		before := heapAfterGC()
		_, err := rt.RunScript(s)
		require.NoError(t, err)
		after := heapAfterGC()
		ic = len(rt.Realm().ICSlots())
		runtime.KeepAlive(rt)
		if after < before {
			return 0, ic
		}
		return after - before, ic
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			small, icSmall := measure(t, tc.src, 2000)
			large, icLarge := measure(t, tc.src, 20000)
			t.Logf("retained %.1f KiB after 2,000, %.1f KiB after 20,000; %d and %d inline caches", float64(small)/1024, float64(large)/1024, icSmall, icLarge)
			assert.Less(t, large, 2*small+256<<10, "the retained heap grows with the code compiled")
			assert.LessOrEqual(t, icLarge, 2*icSmall+64, "the inline-cache table grows with the code compiled")
		})
	}
}

// TestDynamicCodeReclaimed: a closure of dynamic code whose inline caches
// the realm reclaimed while the program compiled other code keeps working,
// with its tagged templates' objects: it binds new caches on its next call.
func TestDynamicCodeReclaimed(t *testing.T) {
	const thrash = `function thrash() { for (var i = 0; i < 1000; i++) eval("({a: 1, b: 2}).b + " + i); }`
	tests := []struct {
		name, src string
		want      any
	}{
		{"eval closure", `var f = eval("(function (o) { return o.a + 1; })"); var r = [f({a: 1})]; thrash(); r.push(f({a: 2}), f({a: 3})); r`,
			[]any{int64(2), int64(3), int64(4)}},
		{"nested factory", `var mk = Function("return function (n) { return function () { return ({v: n}).v; }; }")(); var g = mk(1); g(); thrash(); [g(), mk(2)(), g()]`,
			[]any{int64(1), int64(2), int64(1)}},
		{"generator suspended across a reclaim", `var gen = eval("(function* () { var o = {a: 1}; yield o.a; for (var j = 0; j < 3; j++) yield o.a + j; })")(); var r = [gen.next().value]; thrash(); for (var x of gen) r.push(x); r`,
			[]any{int64(1), int64(1), int64(2), int64(3)}},
		{"reclaim inside a running frame", `var f = Function("o", "var s = o.a; thrash(); return s + o.a + ({c: 3}).c"); [f({a: 1}), f({a: 2})]`,
			[]any{int64(5), int64(7)}},
		{"sloppy writes after a reclaim in the frame", `var f = Function("o", "thrash(); gx = o.a; thrash(); o.b = gx + 1; with (o) { b += a; } return o.b"); [f({a: 1}), f({a: 2}), gx]`,
			[]any{int64(3), int64(5), int64(2)}},
		{"reclaim in a recursion", `var r = Function("n", "if (n === 0) { thrash(); return ({v: 1}).v; } return r(n - 1) + ({v: 1}).v"); [r(20), r(3)]`,
			[]any{int64(21), int64(4)}},
		{"template identity", "var f = eval('(function () { return (t => t)`x${1}y`; })'); var a = f(); thrash(); var b = f(); [a === b, a === f(), b.raw.join()]",
			[]any{true, true, "x,y"}},
		{"distinct evaluations have distinct templates", "var ts = []; for (var i = 0; i < 2; i++) ts.push(eval('(t => t)`x`')); ts[0] !== ts[1]",
			true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runScript(t, thrash+"\n"+tc.src))
		})
	}

	t.Run("async function across a reclaim", func(t *testing.T) {
		rt := moejs.NewRuntime(moejs.Options{})
		_, err := rt.RunScript(mustScript(t, thrash+`
var out = [];
var f = eval("(async function (o) { out.push(o.a); await null; out.push(o.a + 1); await null; out.push(({z: 9}).z); })");
var p = f({a: 1}); thrash(); p.then(() => { thrash(); return f({a: 5}); }).then(() => out.push("done"));`))
		require.NoError(t, err)
		v, err := rt.RunScript(mustScript(t, `out.join()`))
		require.NoError(t, err)
		assert.Equal(t, "1,2,9,5,6,9,done", v.String())
	})
}

// countingCompiler is the root package's compiler counting the dynamic
// functions it compiles.
type countingCompiler struct {
	compiler.Hook
	funcs int
}

func (c *countingCompiler) CompileFunction(name, params, body string, generator, async bool, stop func() error) (*bytecode.Function, error) {
	c.funcs++
	return c.Hook.CompileFunction(name, params, body, generator, async, stop)
}

// TestDynamicFunctionCache: a runtime compiles the source of a dynamic
// function once for a kind, parameter list, body and script, and each call
// of a constructor still creates a new function; a body with a tagged
// template compiles every time, as its template objects differ.
func TestDynamicFunctionCache(t *testing.T) {
	cc := &countingCompiler{}
	engine.SetCompiler(cc)
	t.Cleanup(func() { engine.SetCompiler(compiler.Hook{}) })
	const prelude = `var G = Object.getPrototypeOf(function* () {}).constructor, A = Object.getPrototypeOf(async function () {}).constructor;
function tag(t) { return t; }
`
	tests := []struct {
		name, src string
		want      any
		compiles  int
	}{
		{"one source in a loop", `var fs = []; for (var i = 0; i < 100; i++) fs.push(new Function("o", "return o.a + 1")); fs[0].x = 1; [fs[99]({a: 1}), fs[0] !== fs[1], fs[0].prototype !== fs[1].prototype, fs[1].x]`,
			[]any{int64(2), true, true, nil}, 1},
		{"called and constructed", `[Function("return 1")(), new Function("return 1")()]`, []any{int64(1), int64(1)}, 1},
		{"a parameter list split differently", `[Function("a,b", "return a + b")(1, 2), Function("a", "b", "return a + b")(3, 4)]`, []any{int64(3), int64(7)}, 1},
		{"different parameters", `[Function("a", "return typeof a")(1), Function("b", "return typeof a")(1)]`, []any{"number", "undefined"}, 2},
		{"different kinds", `[typeof Function("return 1")(), typeof G("return 1")().next, A("return 1")() instanceof Promise]`, []any{"number", "function", true}, 3},
		{"subclass prototype", `class F extends Function {} var f = new F("return 1"), g = new F("return 1"), h = Function("return 1"); [f instanceof F, g instanceof F, h instanceof F, f() + g()]`,
			[]any{true, true, false, int64(2)}, 1},
		{"tagged template", "var ts = []; for (var i = 0; i < 3; i++) ts.push(Function('return tag`x`')()); [ts[0] !== ts[1], ts[1] !== ts[2]]",
			[]any{true, true}, 3},
		{"a template called twice", "var f = Function('return tag`x`'); f() === f()", true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cc.funcs = 0
			assert.Equal(t, tc.want, runScript(t, prelude+tc.src))
			assert.Equal(t, tc.compiles, cc.funcs, "compiles")
		})
	}

	// Two scripts compile the same source each: the function belongs to the
	// script creating it.
	t.Run("per script", func(t *testing.T) {
		cc.funcs = 0
		rt := moejs.NewRuntime(moejs.Options{})
		for _, name := range []string{"b.js", "c.js", "b.js"} {
			s, err := moejs.CompileScript(name, `var st = []; for (var i = 0; i < 3; i++) st.push(Function("return new Error().stack")()); st`)
			require.NoError(t, err)
			v, err := rt.RunScript(s)
			require.NoError(t, err)
			got, err := rt.ToGo(v)
			require.NoError(t, err)
			for _, stack := range got.([]any) {
				assert.Contains(t, strings.Split(stack.(string), "\n")[1], name)
			}
		}
		assert.Equal(t, 3, cc.funcs, "compiles")
	})
}

// TestDynamicSourceLimit: source text longer than the runtime's
// MaxDynamicSource throws a RangeError before it is parsed, from eval, the
// four Function constructors (the parameters and the body together, once
// all are strings) and Realm.EvalScript (in bytes); text as long as the
// limit compiles. The limit is 1 MiB unless the host sets it, and a
// negative one is none. Code the host compiles has no limit.
// TestDynamicSourceLoneSurrogates: source text compiled from a String keeps
// its lone surrogates in the literals that hold them.
func TestDynamicSourceLoneSurrogates(t *testing.T) {
	const prelude = `var hi = String.fromCharCode(0xd800), lo = String.fromCharCode(0xdc00);
function syntaxError(f) { try { f(); return false; } catch (e) { return e instanceof SyntaxError; } }
`
	tests := []struct{ name, src string }{
		{"regexp source", `eval("/" + hi + "/").source === hi && eval("/a\\" + lo + "/").source === "a\\" + lo`},
		{"regexp matches", `var re = eval("/^[" + hi + "]" + lo + "$/"); re.test(hi + lo) && !re.test("\ufffd" + lo)`},
		{"regexp early error", `syntaxError(() => eval("() => /[" + lo + "-" + hi + "]/")) && !syntaxError(() => eval("/[" + hi + "-" + lo + "]/"))`},
		{"string", `eval("'" + hi + "'") === hi && eval("'" + lo + hi + "'") === lo + hi`},
		{"template", "eval('`' + lo + '`') === lo && eval('String.raw`' + hi + '`') === hi"},
		{"comment", `eval("1 // " + hi) === 1 && eval("/* " + lo + " */ 2") === 2`},
		{"identifier", `syntaxError(() => eval("var a" + hi))`},
		{"property", `Object.keys(eval("({'" + hi + "': 1})"))[0] === hi && eval("({'" + lo + "'() {}})")[lo].name === lo`},
		{"pair", `var p = String.fromCharCode(0xd83d, 0xde00); eval("'" + p + "'") === p`},
		{"distinct evals", `eval("'" + hi + "'") !== eval("'" + String.fromCharCode(0xd801) + "'")`},
		{"Function", `Function("return '" + hi + "'")() === hi && String(Function("'" + lo + "'")).includes("'" + lo + "'")`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, true, runScript(t, prelude+tc.src))
		})
	}
}

func TestDynamicSourceLimit(t *testing.T) {
	const prelude = `var G = Object.getPrototypeOf(function* () {}).constructor,
	A = Object.getPrototypeOf(async function () {}).constructor,
	AG = Object.getPrototypeOf(async function* () {}).constructor;
function pad(n, s) { return " ".repeat(n - s.length) + s; }
function check(f) { try { f(); return "ok"; } catch (e) { return e.name + ": " + e.message; } }
`
	tests := []struct {
		name string
		src  string // at: a string of n bytes, compiled at run time
	}{
		{"direct eval", `function at(n) { var one = 1; return eval(pad(n, "one")); }`},
		{"indirect eval", `function at(n) { return (0, eval)(pad(n, "1")); }`},
		{"Function", `function at(n) { return Function("a", "b", pad(n - 3, "return 1"))(); }`},
		{"new Function", `function at(n) { return new Function(pad(n, "return 1"))(); }`},
		{"GeneratorFunction", `function at(n) { return G("a", pad(n - 1, "yield 1"))().next().value; }`},
		{"AsyncFunction", `function at(n) { return A(pad(n, "return 1"))() instanceof Promise ? 1 : 0; }`},
		{"AsyncGeneratorFunction", `function at(n) { return typeof AG(pad(n, "yield 1"))().next === "function" ? 1 : 0; }`},
		{"EvalScript", `function at(n) { return evalScript(pad(n, "1")); }`},
	}
	for _, limit := range []int{0, 100, -1} {
		n := limit
		if limit == 0 {
			n = engine.DefaultMaxDynamicSource
		}
		for _, tc := range tests {
			t.Run(tc.name+"/"+strconv.Itoa(limit), func(t *testing.T) {
				rt := moejs.NewRuntime(moejs.Options{MaxDynamicSource: limit})
				assert.Equal(t, n, rt.Realm().MaxDynamicSource())
				require.NoError(t, rt.SetGlobal("evalScript", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
					return r.EvalScript("inner.js", args[0].String())
				})))
				_, err := rt.RunScript(mustScript(t, prelude+tc.src))
				require.NoError(t, err)
				if limit < 0 {
					assert.Equal(t, int64(1), runIn(t, rt, `at(2 << 20)`), "no limit")
					return
				}
				assert.Equal(t, int64(1), runIn(t, rt, `at(`+strconv.Itoa(n)+`)`), "as long as the limit")
				// A String of more code units than the limit is refused by
				// that count; EvalScript's Go string is counted in bytes.
				over := strconv.Itoa(n+1) + " code units is longer than the " + strconv.Itoa(n) + " bytes"
				if tc.name == "EvalScript" {
					over = strconv.Itoa(n+1) + " bytes is longer than the " + strconv.Itoa(n)
				}
				assert.Equal(t, "RangeError: Source text of "+over+" this realm compiles at run time (MaxDynamicSource)",
					runIn(t, rt, `check(() => at(`+strconv.Itoa(n+1)+`))`))
			})
		}
	}

	rt := moejs.NewRuntime(moejs.Options{MaxDynamicSource: 100})
	_, err := rt.RunScript(mustScript(t, strings.Repeat(" ", 200)+"var ok = 1"))
	require.NoError(t, err, "a host compile has no limit")
	// Every entry point counts the bytes of the UTF-8 the compiler parses: é
	// is two, a surrogate pair four and a lone surrogate, in WTF-8, three. A
	// string of more code units than the limit is refused by that count
	// before it is converted, and a rope before it is flattened.
	require.NoError(t, rt.SetGlobal("evalScript", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return r.EvalScript("inner.js", args[0].String())
	})))
	tooLong := func(n int) string {
		return "RangeError: Source text of " + strconv.Itoa(n) + " bytes is longer than the 100 this realm compiles at run time (MaxDynamicSource)"
	}
	tooManyUnits := func(n int) string {
		return "RangeError: Source text of " + strconv.Itoa(n) + " code units is longer than the 100 bytes this realm compiles at run time (MaxDynamicSource)"
	}
	assert.Equal(t, []any{int64(49), tooLong(102), tooManyUnits(101), tooLong(102), nil, tooLong(104), nil, tooLong(103), "function", tooLong(101)},
		runIn(t, rt, `function check(f) { try { return f(); } catch (e) { return e.name + ": " + e.message; } }
[eval("'" + "é".repeat(49) + "'").length, check(() => eval("'" + "é".repeat(50) + "'")), check(() => eval("é".repeat(101))),
check(() => evalScript("'" + "é".repeat(50) + "'")),
eval("/*" + "😀".repeat(24) + "*/"), check(() => eval("/*" + "😀".repeat(25) + "*/")),
eval("/*" + "\ud800".repeat(32) + "*/"), check(() => eval("/*" + "\ud800".repeat(33) + "*/")),
typeof Function("a", "/*" + "é".repeat(47) + "*/"), check(() => Function("a", "/*" + "é".repeat(48) + "*/"))]`))
	// A rope is refused by its length too, before it is flattened.
	assert.Equal(t, []any{tooManyUnits(1 << 26), tooManyUnits(1<<26 + 1)}, runIn(t, rt, `var s = "x"; while (s.length < 1 << 26) s += s;
[check(() => eval(s)), check(() => Function("a", s))]`))
	// The parameters of a Function count with the commas that join them,
	// one fewer than they are (the fixed text around them does not): 50 of
	// one byte are 99 bytes and 51 are 101, while 34 of two bytes in UTF-8
	// are 67 code units and 101 bytes. A million empty ones were once none.
	assert.Equal(t, []any{"function", tooManyUnits(101), "function", tooLong(101), tooManyUnits(999_999)},
		runIn(t, rt, `[typeof Function(...Array(50).fill("a"), ""), check(() => Function(...Array(51).fill("a"), "")),
typeof Function(...Array(33).fill("é"), ""), check(() => Function(...Array(34).fill("é"), "")),
check(() => Function(...Array(1e6).fill(""), ""))]`))
	// The limit applies once every argument is a string: their conversions
	// run first, and one that throws throws.
	assert.Equal(t, []any{"a", "b", "RangeError", "a", "boom"}, runIn(t, rt, `var log = [];
var a = {toString() { log.push("a"); return "a"; }}, b = {toString() { log.push("b"); return " ".repeat(100); }};
try { Function(a, b); } catch (e) { log.push(e.name); }
try { Function(a, {toString() { throw "boom"; }}); } catch (e) { log.push(e); }
log`))
	rt.Realm().SetMaxDynamicSource(0)
	assert.Equal(t, engine.DefaultMaxDynamicSource, rt.Realm().MaxDynamicSource())
	assert.Equal(t, int64(1), runIn(t, rt, `eval(" ".repeat(200) + "1")`), "the default restored")
}

// runIn runs src in rt and returns its completion value as a Go value.
func runIn(t *testing.T, rt *moejs.Runtime, src string) any {
	t.Helper()
	v, err := rt.RunScript(mustScript(t, src))
	require.NoError(t, err)
	got, err := rt.ToGo(v)
	require.NoError(t, err)
	return got
}

// stopCompiler is the root package's compiler interrupting rt at the at-th
// call of the stop functions the engine passes it (zero: never).
type stopCompiler struct {
	compiler.Hook
	rt        *moejs.Runtime
	at, calls int
}

func (c *stopCompiler) wrap(stop func() error) func() error {
	return func() error {
		if c.calls++; c.calls == c.at {
			c.rt.Interrupt("stop")
		}
		return stop()
	}
}

func (c *stopCompiler) CompileScript(name, src string, stop func() error) (*bytecode.Function, error) {
	return c.Hook.CompileScript(name, src, c.wrap(stop))
}

func (c *stopCompiler) CompileEval(name, src string, scope *bytecode.EvalScope, stop func() error) (*bytecode.Function, error) {
	return c.Hook.CompileEval(name, src, scope, c.wrap(stop))
}

func (c *stopCompiler) CompileFunction(name, params, body string, generator, async bool, stop func() error) (*bytecode.Function, error) {
	return c.Hook.CompileFunction(name, params, body, generator, async, c.wrap(stop))
}

// TestDynamicCompileInterrupt: an interrupt stops a compile of code from a
// string in progress, in the parser, the resolver or the compiler (the
// audit's p-bigcompile.js and p-interrupt3.js ran on for seconds): the
// compile ends at the engine's stop function it calls next, before the
// first and every 1024th statement of a list, with the *InterruptedError,
// which passes the program's handlers. An interrupt at the first call is
// one pending when the compile starts. After ClearInterrupt the runtime
// runs the same compile.
func TestDynamicCompileInterrupt(t *testing.T) {
	sc := &stopCompiler{}
	engine.SetCompiler(sc)
	t.Cleanup(func() { engine.SetCompiler(compiler.Hook{}) })
	const prelude = `var src = "var x = 0;" + "x = x + 1;".repeat(3000);
function run() { try { return at(); } catch (e) { return "caught " + e; } }
`
	tests := []struct {
		name, src string
		calls     int // three for each pass of one list of 3,001 statements
	}{
		{"direct eval", `function at() { var x = 1; return eval(src); }`, 9},
		{"indirect eval", `function at() { return (0, eval)(src); }`, 9},
		{"Function", `function at() { return Function("a", src + "return x")(); }`, 11},
		{"EvalScript", `function at() { return evalScript(src); }`, 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newRuntime := func() *moejs.Runtime {
				rt := moejs.NewRuntime(moejs.Options{})
				require.NoError(t, rt.SetGlobal("evalScript", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
					return r.EvalScript("inner.js", args[0].String())
				})))
				_, err := rt.RunScript(mustScript(t, prelude+tc.src))
				require.NoError(t, err)
				return rt
			}
			sc.rt, sc.at, sc.calls = newRuntime(), 0, 0
			assert.Equal(t, int64(3000), runIn(t, sc.rt, `run()`))
			require.Equal(t, tc.calls, sc.calls, "calls of the stop function")
			for at := 1; at <= tc.calls; at++ {
				rt := newRuntime()
				sc.rt, sc.at, sc.calls = rt, at, 0
				_, err := rt.RunScript(mustScript(t, `run()`))
				var ie *moejs.InterruptedError
				require.ErrorAs(t, err, &ie, "an interrupt at call %d", at)
				assert.Equal(t, "stop", ie.Value)
				assert.Equal(t, at, sc.calls, "the compile ends at the call of stop that finds the interrupt")
				rt.ClearInterrupt()
				sc.at = 0
				assert.Equal(t, int64(3000), runIn(t, rt, `run()`), "the runtime after an interrupt at call %d", at)
			}
		})
	}
}

// TestDisableDynamicCode: a runtime with DisableDynamicCode compiles no code
// from strings. eval of a string, the four Function constructors and
// Realm.EvalScript throw an EvalError where MaxDynamicSource would throw its
// RangeError, once the arguments are strings, code compiled before
// included; eval of another value returns it, and the host's compiles run.
func TestDisableDynamicCode(t *testing.T) {
	const disabled = "EvalError: code generation from strings is disabled for this realm (DisableDynamicCode)"
	rt := moejs.NewRuntime(moejs.Options{DisableDynamicCode: true})
	assert.True(t, rt.Realm().DynamicCodeDisabled())
	assert.Equal(t, engine.DefaultMaxDynamicSource, rt.Realm().MaxDynamicSource(), "the limit keeps its default")
	require.NoError(t, rt.SetGlobal("evalScript", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return r.EvalScript("inner.js", args[0].String())
	})))
	assert.Equal(t, []any{disabled, disabled, disabled, disabled, disabled, disabled, disabled, disabled, disabled, int64(1), true, "a,b"},
		runIn(t, rt, `var G = Object.getPrototypeOf(function* () {}).constructor,
	A = Object.getPrototypeOf(async function () {}).constructor,
	AG = Object.getPrototypeOf(async function* () {}).constructor;
function check(f) { try { f(); return "ok"; } catch (e) { return e.name + ": " + e.message; } }
var log = [], a = {toString() { log.push("a"); return "a"; }}, b = {toString() { log.push("b"); return ""; }};
[check(() => eval("1")), check(() => { var v = 1; return eval("v"); }), check(() => (0, eval)("1")),
check(() => Function(a, b)), check(() => new Function("")), check(() => G("")), check(() => A("")), check(() => AG("")),
check(() => evalScript("1")), eval(1), (0, eval)(null) === null, log.join()]`))

	rt = moejs.NewRuntime(moejs.Options{})
	assert.False(t, rt.Realm().DynamicCodeDisabled())
	src := `function f() { var v = 1; return eval("v + 1"); }
function g() { return Function("return 2")(); }
function check(f) { try { return f(); } catch (e) { return e.name + ": " + e.message; } }
[check(f), check(g)]`
	assert.Equal(t, []any{int64(2), int64(2)}, runIn(t, rt, src))
	rt.Realm().SetDynamicCodeDisabled(true)
	assert.Equal(t, []any{disabled, disabled}, runIn(t, rt, src), "cached code is refused too")
	rt.Realm().SetDynamicCodeDisabled(false)
	assert.Equal(t, []any{int64(2), int64(2)}, runIn(t, rt, src))
}

// evalSiteSource returns a function with vars variables and depth nested
// blocks, each with a let and a call, and in the innermost scope sites
// groups of calls: one in the scope, one in a block of its own and one in
// an arrow. The calls are direct evals or, for control, calls of evil but
// for one eval in the innermost scope, which captures the same bindings.
func evalSiteSource(vars, depth, sites int, control bool) string {
	call := "eval"
	if control {
		call = "evil"
	}
	var b strings.Builder
	b.WriteString("function f(x) {\n")
	if vars > 0 {
		b.WriteString("var ")
		for i := range vars {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("a" + strconv.Itoa(i))
		}
		b.WriteString(";\n")
	}
	for range depth {
		b.WriteString("{ let a; " + call + "(x);")
	}
	b.WriteString("eval(x);\n")
	for range sites {
		b.WriteString(call + "(x); { let b; " + call + "(x); } () => " + call + "(x);\n")
	}
	b.WriteString(strings.Repeat("}", depth) + "}")
	return b.String()
}

// TestEvalSiteCost: a direct eval call costs its compile little however
// many bindings it sees, as the calls of a compilation share the levels of
// their environments (the compiler's evalMemo) and the resolver marks each
// scope once. A function of many variables (wide) or nested blocks (deep)
// and many calls compiles within a small multiple of its source with one
// call, and of its size: sources of 190 and 180 KB allocated 1.1 GB and
// 430 MB, 86 and 37 times as much as with one call.
func TestEvalSiteCost(t *testing.T) {
	alloc := func(src string) uint64 {
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		_, err := moejs.CompileScript("m.js", src)
		runtime.ReadMemStats(&m1)
		require.NoError(t, err)
		return m1.TotalAlloc - m0.TotalAlloc
	}
	for _, tc := range []struct {
		name               string
		vars, depth, sites int
	}{
		{"wide", 16000, 0, 2000},
		{"deep", 0, 250, 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := evalSiteSource(tc.vars, tc.depth, tc.sites, false)
			one := alloc(evalSiteSource(tc.vars, tc.depth, tc.sites, true))
			all := alloc(src)
			assert.Less(t, all, 3*one, "%d bytes allocated for %d bytes of source, %d with one call", all, len(src), one)
			assert.Less(t, all, uint64(256*len(src)), "%d bytes allocated for %d bytes of source", all, len(src))
		})
	}
}

// TestEvalSiteInterrupt: an interrupt stops a compile of code with many
// direct eval calls promptly, as none of its steps, the marking of what the
// calls see included, runs long between calls of the stop function: one of
// 60,000 variables and as many calls ran on for seconds.
func TestEvalSiteInterrupt(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles sources of 900 KB for a while")
	}
	var b strings.Builder
	b.WriteString("var ")
	for i := range 60000 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("a" + strconv.Itoa(i))
	}
	b.WriteString(";" + strings.Repeat("eval(x);", 60000))
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("body", b.String()))
	s := mustScript(t, `for (var i = 0; i < 1000; i++) Function("x", body + ";" + i);`)
	const after = 200 * time.Millisecond
	slack := time.Second
	if raceEnabled {
		slack = 10 * time.Second
	}
	t0 := time.Now()
	timer := time.AfterFunc(after, func() { rt.Interrupt("stop") })
	defer timer.Stop()
	_, err := rt.RunScript(s)
	d := time.Since(t0)
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Less(t, d, after+slack, "interrupted after %v", after)
}

// TestEvalInLargeEnvironment: eval code looks up the bindings it names in
// its caller's environments rather than copying every one, so small eval
// code in a function of 20,000 variables costs about what it costs in a
// function of one (a thousand times as much).
func TestEvalInLargeEnvironment(t *testing.T) {
	cost := func(vars int) uint64 {
		var b strings.Builder
		b.WriteString("function f(k, n) { var ")
		for i := range vars {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("a" + strconv.Itoa(i))
		}
		b.WriteString(`; a0 = 1; var s = 0; for (var i = k; i < k + n; i++) s += eval("a0 + " + i); return s; }`)
		rt := moejs.NewRuntime(moejs.Options{})
		_, err := rt.RunScript(mustScript(t, b.String()))
		require.NoError(t, err)
		// The first eval in the large environment indexes its names.
		assert.Equal(t, int64(2), runIn(t, rt, `f(1, 1)`))
		s := mustScript(t, `f(10, 200)`)
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		v, err := rt.RunScript(s)
		runtime.ReadMemStats(&m1)
		require.NoError(t, err)
		got, err := rt.ToGo(v)
		require.NoError(t, err)
		assert.Equal(t, int64(200+(10+209)*100), got)
		return m1.TotalAlloc - m0.TotalAlloc
	}
	small, large := cost(1), cost(20000)
	// The large function's own environment is 20,000 slots.
	assert.Less(t, large, 2*small+1<<20, "200 evals allocated %d bytes in a function of 20,000 variables, %d in one of one", large, small)
}

// BenchmarkDynamicCompile measures code compiled from strings: small direct
// eval code, distinct each time (compiled) or not (cached), in a function
// of a few variables or of 2,000, a Function constructor call, and the host
// compile of a function with many direct eval calls.
func BenchmarkDynamicCompile(b *testing.B) {
	var vars strings.Builder
	for i := range 2000 {
		vars.WriteString(",v" + strconv.Itoa(i))
	}
	for _, tc := range []struct{ name, setup, call string }{
		{"eval", `function f(a, i) { return eval("a + " + i); }`, `f(1, i)`},
		{"eval-cached", `function f(a, i) { return eval("a + 1"); }`, `f(1, i)`},
		{"eval-large-env", `function f(a, i) { var v` + vars.String() + `; return eval("a + " + i); }`, `f(1, i)`},
		{"Function", ``, `Function("a", "return a + " + i)`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			rt := moejs.NewRuntime(moejs.Options{})
			s, err := moejs.CompileScript("m.js", tc.setup+"\nvar i = 0; function run() { "+tc.call+"; i++; }\nrun")
			require.NoError(b, err)
			run, err := rt.RunScript(s)
			require.NoError(b, err)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := rt.Realm().Call(run, moejs.Undefined(), nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("CompileScript-eval-sites", func(b *testing.B) {
		src := evalSiteSource(2000, 20, 200, false)
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := moejs.CompileScript("m.js", src); err != nil {
				b.Fatal(err)
			}
		}
	})
}

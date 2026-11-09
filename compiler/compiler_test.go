package compiler

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func compileModule(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	m, err := syntax.ParseModule("t.js", src, syntax.Options{})
	require.NoError(t, err)
	fn, err := CompileModule(m)
	require.NoError(t, err)
	return fn
}

// child returns the child template named name.
func child(t *testing.T, fn *bytecode.Function, name string) *bytecode.Function {
	t.Helper()
	for _, c := range fn.Children {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no child %q", name)
	return nil
}

func ops(fn *bytecode.Function) []bytecode.Op {
	var out []bytecode.Op
	for pc := 0; pc < len(fn.Code); {
		op := bytecode.DecodeOp(fn.Code[pc])
		out = append(out, op)
		pc += 1 + op.ExtraWords()
	}
	return out
}

func TestDisassembleGolden(t *testing.T) {
	fn := compileModule(t, `export function clamp(v, lo, hi) {
  const n = +v;
  if (typeof v !== "number") return lo;
  if (n < lo) return lo;
  return n > hi ? hi : n;
}`)
	want := `function clamp (normal) params=3 regs=5 env=0 ics=0
  0000 Plus r3 r0
  0001 JmpTypeofIs r0 -> 0004 "number"
  0003 Ret r1
  0004 JmpNLt r3 -> 0007 r1
  0006 Ret r1
  0007 JmpNGt r3 -> 0010 r2
  0009 Ret r2
  0010 Ret r3
  0011 RetUndef
`
	assert.Equal(t, want, bytecode.Disassemble(child(t, fn, "clamp")))
}

// TestConditionalReturn checks that `return c ? a : b` returns from each
// branch, and joins them where a return routes through a finally or closes
// an iterator.
func TestConditionalReturn(t *testing.T) {
	m := compileModule(t, `export function plain(c, a, b) { return c ? a : b; }
export function fin(c, a, b) { try { return c ? a : b; } finally { a = 0; } }
export function iter(c, xs) { for (const x of xs) return c ? x : 0; }`)
	assert.Equal(t, []bytecode.Op{bytecode.JmpF, bytecode.Ret, bytecode.Ret, bytecode.RetUndef}, ops(child(t, m, "plain")))
	// One return completion (kind 2) stored for the finally.
	fin := child(t, m, "fin")
	rk := fin.Handlers[0].Reg
	stores := 0
	for pc := 0; pc < len(fin.Code); {
		w := fin.Code[pc]
		op := bytecode.DecodeOp(w)
		if op == bytecode.LoadInt && uint16(bytecode.DecodeA(w)) == rk && bytecode.DecodeSBx(w) == 2 {
			stores++
		}
		pc += 1 + op.ExtraWords()
	}
	assert.Equal(t, 1, stores, bytecode.Disassemble(fin))
	// One close of the iterator, one Ret.
	iter := ops(child(t, m, "iter"))
	assert.Equal(t, 1, countOp(iter, bytecode.IterClose))
	assert.Equal(t, 1, countOp(iter, bytecode.Ret))
}

func countOp(list []bytecode.Op, op bytecode.Op) int {
	n := 0
	for _, o := range list {
		if o == op {
			n++
		}
	}
	return n
}

func TestDisassembleModuleAndHandlers(t *testing.T) {
	fn := compileModule(t, `export let n = 0;
export function guard(f) { try { return f(); } catch (e) { n++; return e.message; } }`)
	got := bytecode.Disassemble(fn)
	assert.True(t, strings.HasPrefix(got, "function <anonymous> (module) params=0 regs=1 env=2 ics=0\n"), got)
	assert.Contains(t, got, "  F0:\n  function guard (normal) params=1 regs=")
	assert.Contains(t, got, `K0 = "message" (key)`)
	assert.Contains(t, got, "GetProp r")
	assert.Contains(t, got, " catch r")
	require.Len(t, fn.Module.Exports, 2)
	assert.Equal(t, "guard", fn.Module.Exports[0].Name, "sorted by name")
	assert.NotEqual(t, fn.Module.Exports[0].Slot, fn.Module.Exports[1].Slot, "each export has its own module Env slot")
	assert.Nil(t, fn.Module.Links, "no module requests")
	guard := child(t, fn, "guard")
	require.Len(t, guard.Handlers, 1)
	assert.Equal(t, bytecode.HandlerCatch, guard.Handlers[0].Kind)
	assert.Equal(t, uint32(0), guard.Handlers[0].Start)
	assert.Less(t, guard.Handlers[0].End, guard.Handlers[0].Handler)
}

// TestIterCloseRows checks the handler rows around the iterator close of a
// return leaving a for-of loop: the rows of a try statement inside the loop
// skip it, and the loop's own row covers it whole (the record is closed
// before its return method runs, so the row only rethrows).
func TestIterCloseRows(t *testing.T) {
	fn := compileModule(t, `export function plain(a) { for (const x of a) if (x) return x; }
export function nested(a) { for (const x of a) { try { if (x) return x; } catch (e) {} } }`)
	covering := func(f *bytecode.Function) []bytecode.Handler {
		pc := slices.IndexFunc(f.Code, func(w uint32) bool { return w == bytecode.EncodeAsBx(bytecode.IterClose, uint8(w>>8), 0) })
		require.GreaterOrEqual(t, pc, 0, "no IterClose")
		var rows []bytecode.Handler
		for _, h := range f.Handlers {
			if h.Start <= uint32(pc) && uint32(pc) < h.End {
				rows = append(rows, h)
			}
		}
		return rows
	}
	plain := child(t, fn, "plain")
	assert.Len(t, plain.Handlers, 1, "the loop's row is not split")
	assert.Len(t, covering(plain), 1)
	nested := child(t, fn, "nested")
	assert.Len(t, nested.Handlers, 3, "the try's row is split around the close, the loop's is not")
	assert.Len(t, covering(nested), 1, "only the loop's row covers the close")
}

func TestConstantFolding(t *testing.T) {
	fn := compileModule(t, `
export const a = 1 + 2 * 3;
export const b = "x" + "y" + "z";
export const c = !0;
export const d = -(2 ** 3);
export const e = typeof "s";
export const f = 1 < 2 ? "yes" : "no";
export const g = null ?? undefined ?? 5;
export const h = (1, 2) | 4;
export const i = 1 / 0;
`)
	names := map[float64]bool{}
	strs := map[string]bool{}
	for _, k := range fn.Consts {
		switch k.Kind {
		case bytecode.ConstNumber:
			names[k.Num] = true
		case bytecode.ConstString:
			strs[k.Str] = true
		}
	}
	assert.True(t, strs["xyz"], "string concatenation folded")
	assert.False(t, strs["x"], "no intermediate string constants")
	assert.True(t, strs["yes"])
	assert.False(t, strs["no"], "dead conditional branch dropped")
	assert.True(t, strs["string"], "typeof folded")
	for _, op := range ops(fn) {
		assert.NotContains(t, []bytecode.Op{bytecode.Add, bytecode.Mul, bytecode.Exp, bytecode.Not, bytecode.Typeof, bytecode.BitOr, bytecode.Div, bytecode.JmpT, bytecode.JmpF, bytecode.JmpNotNullish}, op, "operator %s should have been folded", op)
	}
	// Small integers load as immediates, others through the constant pool.
	seenLoadInt := false
	for _, op := range ops(fn) {
		if op == bytecode.LoadInt {
			seenLoadInt = true
		}
	}
	assert.True(t, seenLoadInt)
	assert.True(t, names[1/zero()], "Infinity is a pool constant")
}

func zero() float64 { return 0 }

// TestFoldStringChains covers the concatenations built across the links of
// one chain, through logical operators that do and do not short-circuit.
func TestFoldStringChains(t *testing.T) {
	const no = "\x00" // the chain does not fold
	for _, tt := range []struct{ src, want string }{
		{`"a" + "b" + "c" + "d"`, "abcd"},
		{`("a" + "b" || 0) + "c"`, "abc"},
		{`("a" + "b" ?? 0) + "c" + "d"`, "abcd"},
		{`("" + "" || "x") + "y"`, "xy"},
		{`("a" + "b" && "q") + "r"`, "qr"},
		{`("a" + "b" && "") + "c"`, "c"},
		{`(("a" + "b" && "p" + "q") || 0) + "r"`, "pqr"},
		{`(typeof 1 + "!" || 0) + "?"`, "number!?"},
		{`"" + "" + ""`, ""},
		{`"a" + "b" + 1`, no},
		{`("a" + "b" || 0) + x`, no},
		{`("a" + "b" - 1) + "c"`, no},
	} {
		fn := compileModule(t, "export const x = 1; export const y = "+tt.src+";")
		strs := []string{""} // the empty string loads without a constant
		for _, k := range fn.Consts {
			if k.Kind == bytecode.ConstString {
				strs = append(strs, k.Str)
			}
		}
		adds := slices.ContainsFunc(ops(fn), func(op bytecode.Op) bool { return op == bytecode.Add || op == bytecode.AddImm })
		if tt.want == no {
			assert.True(t, adds, "%s does not fold", tt.src)
			continue
		}
		assert.False(t, adds, "%s folds", tt.src)
		assert.Contains(t, strs, tt.want, "%s", tt.src)
	}
}

func TestDeadCodeAfterReturn(t *testing.T) {
	fn := compileModule(t, `export function f(x) { return x; x = 2; f(); throw 1; }
export function g(x) { if (x) { return 1; g(); } return 2; }
export function h() { throw new Error("e"); return 3; }`)
	assert.Equal(t, []bytecode.Op{bytecode.Ret, bytecode.RetUndef}, ops(child(t, fn, "f")))
	assert.NotContains(t, ops(child(t, fn, "g")), bytecode.Call)
	hops := ops(child(t, fn, "h"))
	assert.Equal(t, bytecode.Throw, hops[len(hops)-2])
	assert.Equal(t, bytecode.RetUndef, hops[len(hops)-1])
}

func TestRegisterAndEnvAllocation(t *testing.T) {
	fn := compileModule(t, `
export function plain(a, b) { let c = a + b; const d = c * 2; return d; }
export function captured(a) { let n = a; return () => n++; }
export function block() { let out; { let x = 1; out = () => x; } return out; }
export function loopEnv() { const fs = []; for (let i = 0; i < 2; i++) fs[i] = () => i; return fs; }
`)
	plain := child(t, fn, "plain")
	assert.Empty(t, plain.CaptureLayout)
	assert.NotContains(t, ops(plain), bytecode.PushEnv)
	assert.NotContains(t, ops(plain), bytecode.GetEnv)
	assert.NotContains(t, ops(plain), bytecode.GetEnv0)
	assert.NotContains(t, ops(plain), bytecode.CheckTDZ)

	cap := child(t, fn, "captured")
	assert.Equal(t, bytecode.CaptureLayout{bytecode.NoRegister}, cap.CaptureLayout, "only n is boxed, not the parameter")
	assert.Contains(t, ops(cap), bytecode.SetEnv)
	inner := cap.Children[0]
	assert.Equal(t, bytecode.KindArrow, inner.Kind)
	assert.Contains(t, ops(inner), bytecode.GetEnv0)

	blk := child(t, fn, "block")
	assert.Empty(t, blk.CaptureLayout, "block scope environments are pushed, not part of the function env")
	assert.Contains(t, ops(blk), bytecode.PushEnv)
	assert.Contains(t, ops(blk), bytecode.PopEnv)

	loop := child(t, fn, "loopEnv")
	assert.Contains(t, ops(loop), bytecode.CopyEnv)
	assert.False(t, loop.HasArguments)
}

// TestGetEnvDepth checks that a read of the function's own environment, of
// the enclosing one and of the one around it is GetEnv0, GetEnv1 and
// GetEnv2, with B 0, 1 and 2, and a read of a deeper one GetEnv.
func TestGetEnvDepth(t *testing.T) {
	fn := compileModule(t, `
export function f0(a) {
	return function f1(b) {
		return function f2(c) {
			return function f3(d) { return () => a + b + c + d; };
		};
	};
}`)
	arrow := child(t, fn, "f0").Children[0].Children[0].Children[0].Children[0]
	require.Equal(t, bytecode.KindArrow, arrow.Kind)
	depths := map[bytecode.Op][]int{}
	for _, w := range words(arrow) {
		if op := bytecode.DecodeOp(w[0]); op == bytecode.GetEnv || op == bytecode.GetEnv0 || op == bytecode.GetEnv1 || op == bytecode.GetEnv2 {
			depths[op] = append(depths[op], int(bytecode.DecodeB(w[0])))
		}
	}
	assert.Equal(t, map[bytecode.Op][]int{bytecode.GetEnv: {3}, bytecode.GetEnv2: {2}, bytecode.GetEnv1: {1}, bytecode.GetEnv0: {0}}, depths)
}

// TestArgumentsRegister checks that a function referencing `arguments` (in
// its body or a nested arrow) gets HasArguments and a register after its
// parameters and rest array, boxed into its environment when an arrow reads
// it; arrows never get their own.
func TestArgumentsRegister(t *testing.T) {
	fn := compileModule(t, `
export function direct(a, b) { return arguments[0]; }
export function rest(a, ...r) { return arguments.length; }
export function viaArrow(a) { return () => arguments; }
`)
	direct := child(t, fn, "direct")
	assert.True(t, direct.HasArguments)
	assert.Empty(t, direct.CaptureLayout)
	assert.GreaterOrEqual(t, int(direct.NumRegs), 3)

	rest := child(t, fn, "rest")
	assert.True(t, rest.HasArguments)
	assert.True(t, rest.HasRest)
	assert.GreaterOrEqual(t, int(rest.NumRegs), 3)

	via := child(t, fn, "viaArrow")
	assert.True(t, via.HasArguments)
	assert.Equal(t, bytecode.CaptureLayout{1}, via.CaptureLayout, "the arguments register (after a) is boxed")
	arrow := via.Children[0]
	assert.False(t, arrow.HasArguments)
	assert.Contains(t, ops(arrow), bytecode.GetEnv0)
}

// TestForwardArguments checks which functions leave their arguments object
// to ApplyArguments (ForwardArguments): those whose every use of it is
// f.apply(x, arguments), with nothing else that could reach it (reads of
// its length and elements, TestArgumentsReadCompile, may join them). Their
// prologue maps no object, and each such call is an ApplyArguments of the
// arguments register, mapped in sloppy code with simple parameters.
func TestForwardArguments(t *testing.T) {
	fn := compileScript(t, `
function plain() { return g.apply(this, arguments); }
function params(a, b) { o.f.apply(a, arguments); return o.g.apply(b, arguments); }
function strictWrites(a) { 'use strict'; a = 1; return g.apply(this, arguments); }
function inCtor() { this.initialize.apply(this, arguments); }
function writes(a) { a = 1; return g.apply(this, arguments); }
function arrow() { return () => g.apply(this, arguments); }
function evals() { eval(''); return g.apply(this, arguments); }
function withs(o) { with (o) { return g.apply(this, arguments); } }
function* gen() { yield g.apply(this, arguments); }
async function asyncFn() { return g.apply(this, arguments); }
function computed() { return g['apply'](this, arguments); }
function three() { return g.apply(this, arguments, 1); }
function optional() { return g?.apply(this, arguments); }
function spread() { return g.apply(...arguments); }
function call() { return g.call(this, arguments); }
function typeofArgs() { return typeof arguments + g.apply(this, arguments); }
`)
	for _, name := range []string{"plain", "params", "strictWrites", "inCtor"} {
		f := child(t, fn, name)
		assert.True(t, f.ForwardArguments, name)
		assert.NotContains(t, ops(f), bytecode.MapArguments, name)
		args := int(f.NumParams)
		mapped := uint8(1)
		if name == "strictWrites" {
			mapped = 0
		}
		n := 0
		for pc := 0; pc < len(f.Code); pc += 1 + bytecode.DecodeOp(f.Code[pc]).ExtraWords() {
			if w := f.Code[pc]; bytecode.DecodeOp(w) == bytecode.ApplyArguments {
				n++
				assert.Equal(t, args, int(bytecode.DecodeB(w)), name)
				assert.Equal(t, mapped, bytecode.DecodeC(w), name)
			}
		}
		want := 1
		if name == "params" {
			want = 2
		}
		assert.Equal(t, want, n, name)
		require.NoError(t, bytecode.VerifyRegisters(f), name)
	}
	for _, name := range []string{"writes", "arrow", "evals", "withs", "gen", "asyncFn", "computed", "three", "optional", "spread", "call", "typeofArgs"} {
		f := child(t, fn, name)
		assert.True(t, f.HasArguments, name)
		assert.False(t, f.ForwardArguments, name)
		assert.NotContains(t, ops(f), bytecode.ApplyArguments, name)
	}
}

// TestArgumentsReadCompile checks the functions whose arguments object
// waits while they only read arguments.length and arguments[k], directly or
// through a top-level var a = arguments (ArgsLen, ArgsGet of the arguments
// register, X = 1 when mapped), and those whose other uses need the object.
func TestArgumentsReadCompile(t *testing.T) {
	fn := compileScript(t, `
function length() { return arguments.length; }
function index(i) { return arguments[i] + arguments[0]; }
function both() { return g.apply(this, arguments) + arguments[arguments.length - 1]; }
function strictIndex(a) { 'use strict'; a = 1; return arguments[0]; }
function defaults(a = 1) { return arguments[0]; }
function alias() { var res = null, a = arguments; for (var i = a.length - 1; i >= 0; i--) res = [a[i], res]; return res; }
function callee() { return arguments[0](); }
function method() { return arguments.callee; }
function writes() { arguments[0] = 1; }
function updates() { arguments[0]++; }
function deletes() { delete arguments[0]; }
function forIn() { for (arguments[0] in o); }
function tag() { return arguments[0]`+"`t`"+`; }
function optional() { return arguments?.[0]; }
function bare() { return arguments; }
function aliasBefore() { a[0]; var a = arguments; return a[0]; }
function aliasWritten() { var a = arguments; a = []; return a[0]; }
function aliasCaptured() { var a = arguments; return () => a[0]; }
function aliasNested() { if (x) { var a = arguments; } return a[0]; }
function aliasMember() { var a = arguments; a.x = 1; return a[0]; }
`)
	for _, name := range []string{"length", "index", "both", "strictIndex", "defaults", "alias"} {
		f := child(t, fn, name)
		assert.True(t, f.ForwardArguments, name)
		assert.NotContains(t, ops(f), bytecode.MapArguments, name)
		args := uint8(f.NumParams)
		mapped := uint32(1)
		if name == "strictIndex" || name == "defaults" {
			mapped = 0
		}
		n := 0
		for pc := 0; pc < len(f.Code); pc += 1 + bytecode.DecodeOp(f.Code[pc]).ExtraWords() {
			switch w := f.Code[pc]; bytecode.DecodeOp(w) {
			case bytecode.ArgsLen:
				n++
				assert.Equal(t, args, bytecode.DecodeB(w), name)
			case bytecode.ArgsGet:
				n++
				assert.Equal(t, args, bytecode.DecodeB(w), name)
				assert.Equal(t, mapped, f.Code[pc+1], name)
			}
		}
		want := map[string]int{"length": 1, "index": 2, "both": 2, "strictIndex": 1, "defaults": 1, "alias": 2}[name]
		assert.Equal(t, want, n, name)
		require.NoError(t, bytecode.VerifyRegisters(f), name)
	}
	for _, name := range []string{"callee", "method", "writes", "updates", "deletes", "forIn", "tag", "optional", "bare",
		"aliasBefore", "aliasWritten", "aliasCaptured", "aliasNested", "aliasMember"} {
		f := child(t, fn, name)
		assert.True(t, f.HasArguments, name)
		assert.False(t, f.ForwardArguments, name)
		assert.NotContains(t, ops(f), bytecode.ArgsGet, name)
		assert.NotContains(t, ops(f), bytecode.ArgsLen, name)
	}
}

// TestTaggedTemplateCompile checks the tagged-template lowering: one
// ConstTemplate and one inline-cache slot per site, the tag's object as
// this, and the substitutions as the arguments after the template object.
func TestTaggedTemplateCompile(t *testing.T) {
	fn := compileModule(t, "export function f(o, x) { return o.t`a${x}\\01`; }\nexport function g(x) { return `a${x}`; }")
	f := child(t, fn, "f")
	assert.Equal(t, []bytecode.Op{bytecode.Move, bytecode.GetProp, bytecode.GetTemplate, bytecode.Move, bytecode.Call, bytecode.Ret, bytecode.RetUndef}, ops(f))
	var tmpl *bytecode.Const
	for i := range f.Consts {
		if f.Consts[i].Kind == bytecode.ConstTemplate {
			require.Nil(t, tmpl, "one template constant")
			tmpl = &f.Consts[i]
		}
	}
	require.NotNil(t, tmpl)
	assert.Equal(t, []string{"a", bytecode.UndefinedCooked}, tmpl.Cooked)
	assert.Equal(t, []string{"a", `\01`}, tmpl.Raw)
	assert.Equal(t, uint32(2), f.ICCount, "GetProp and GetTemplate")
	g := child(t, fn, "g")
	assert.NotContains(t, ops(g), bytecode.GetTemplate)
	assert.Zero(t, g.ICCount, "untagged templates use no cache slot")
}

func TestModuleExportsAndTopLevel(t *testing.T) {
	fn := compileModule(t, `
let a = 1;
export { a as b, a as c };
export default 42;
export function f() {}
`)
	assert.Equal(t, bytecode.KindModule, fn.Kind)
	assert.Equal(t, 3, len(fn.CaptureLayout), "a, *default*, f")
	b, _ := fn.Module.Export("b")
	c, _ := fn.Module.Export("c")
	assert.Equal(t, b, c)
	_, hasDefault := fn.Module.Export("default")
	assert.True(t, hasDefault)
	assert.NotNil(t, fn.Source)
	assert.Equal(t, "t.js", fn.Source.Name)
	assert.Equal(t, "function f() {}", child(t, fn, "f").SourceText())
}

func TestModuleTopLevelAwait(t *testing.T) {
	fn := compileModule(t, `export const x = await 1;`)
	assert.True(t, fn.Async, "a module with top-level await is an async body")
	o := ops(fn)
	assert.Contains(t, o, bytecode.Await)
	assert.Contains(t, o, bytecode.AsyncReturn)
	assert.Contains(t, o, bytecode.AsyncThrow)
	fn = compileModule(t, `export const x = 1;`)
	assert.False(t, fn.Async)
	o = ops(fn)
	assert.NotContains(t, o, bytecode.AsyncReturn)
	assert.Equal(t, bytecode.RetUndef, o[len(o)-1])
}

// TestScriptOrModuleFlag checks that the root of code that uses import()
// or import.meta, or holds a direct eval, is flagged, from any depth, that
// a module flagged for the first two has Links without requests, and that
// other code compiles as before.
func TestScriptOrModuleFlag(t *testing.T) {
	tests := []struct {
		name, src string
		flagged   bool
		eval      bool // flagged for a direct eval alone: no Links
		requests  int
		op        bytecode.Op // 0: none checked
	}{
		{"none", `export const x = 1;`, false, false, 0, 0},
		{"import-call", `export const p = import("./a.js");`, true, false, 0, bytecode.ImportCall},
		{"import-meta", `export const m = import.meta;`, true, false, 0, bytecode.ImportMeta},
		{"nested", `export function f() { return () => import("./a.js"); }`, true, false, 0, 0},
		{"with-requests", `import "./b.js"; export const m = import.meta;`, true, false, 1, bytecode.ImportMeta},
		{"requests-only", `import "./b.js";`, false, false, 1, 0},
		{"direct-eval", `export const e = eval("1");`, true, true, 0, bytecode.CallEval},
		{"nested-direct-eval", `export function f() { return () => eval("1"); }`, true, true, 0, 0},
		{"direct-eval-with-requests", `import "./b.js"; eval("1");`, true, true, 1, bytecode.CallEval},
		{"indirect-eval", `export const e = (0, eval)("1");`, false, false, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn := compileModule(t, tt.src)
			assert.Equal(t, tt.flagged, fn.ScriptOrModule)
			switch {
			case tt.flagged && !tt.eval || tt.requests > 0:
				require.NotNil(t, fn.Module.Links)
				assert.Len(t, fn.Module.Links.Requests, tt.requests)
			default:
				assert.Nil(t, fn.Module.Links)
			}
			for _, c := range fn.Children {
				assert.False(t, c.ScriptOrModule, "only the root is flagged")
			}
			if tt.op != 0 {
				assert.Contains(t, ops(fn), tt.op)
			}
		})
	}
	s, err := syntax.ParseScript("s.js", `function f() { return import(x); }`, syntax.Options{})
	require.NoError(t, err)
	fn, err := CompileScript(s)
	require.NoError(t, err)
	assert.True(t, fn.ScriptOrModule)
	assert.Contains(t, ops(child(t, fn, "f")), bytecode.ImportCall)
	for src, flagged := range map[string]bool{
		`var x = 1;`:                         false,
		`function f() { return eval("1"); }`: true,
		`(0, eval)("1");`:                    false,
	} {
		s, err = syntax.ParseScript("s.js", src, syntax.Options{})
		require.NoError(t, err)
		fn, err = CompileScript(s)
		require.NoError(t, err)
		assert.Equal(t, flagged, fn.ScriptOrModule, src)
	}
}

func TestCompileScript(t *testing.T) {
	s, err := syntax.ParseScript("s.js", `var x = 1; x + 1;`, syntax.Options{})
	require.NoError(t, err)
	fn, err := CompileScript(s)
	require.NoError(t, err)
	assert.Equal(t, bytecode.KindScript, fn.Kind)
	o := ops(fn)
	assert.Equal(t, bytecode.Ret, o[len(o)-1], "scripts return their completion value")
}

// TestExportNotModuleBinding covers an export entry whose binding the module
// does not declare, which the scope pass never produces.
func TestExportNotModuleBinding(t *testing.T) {
	m, err := syntax.ParseModule("t.js", "const a = 1;\nexport { a as b };", syntax.Options{})
	require.NoError(t, err)
	m.Exports[0].Binding = &syntax.Binding{Name: "a"}
	_, err = CompileModule(m)
	var ce *Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "t.js:2:10: SyntaxError: export 'b' is not a module binding", err.Error())
}

func TestCompileLimits(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("export function f() {\n")
	for i := range 300 {
		sb.WriteString("let v")
		sb.WriteString(strings.Repeat("x", i%5))
		sb.WriteString(itoa(i))
		sb.WriteString(" = ")
		sb.WriteString(itoa(i))
		sb.WriteString(";\n")
	}
	sb.WriteString("return v0;\n}")
	m, err := syntax.ParseModule("t.js", sb.String(), syntax.Options{})
	require.NoError(t, err)
	fn, err := CompileModule(m)
	require.NoError(t, err, "300 let bindings spill and compile")
	assert.Less(t, int(fn.Children[0].NumRegs), 256, "spilled bindings live in the environment")
	assert.Greater(t, len(fn.Children[0].CaptureLayout), 100, "spilled bindings are environment slots")
}

// TestCompileParameterLimits checks that parameters count against the
// 256 registers: a function of 255 or 256 parameters whose body needs no
// temporary compiles to that many registers, and 257 or more, 256 with a
// rest parameter or the arguments object after them, or 256 and one
// temporary, are the SyntaxError of a function needing more than 256
// registers, as the engine's verifier refuses a wider frame.
func TestCompileParameterLimits(t *testing.T) {
	params := func(n int) string {
		ps := make([]string, n)
		for i := range ps {
			ps[i] = "p" + itoa(i)
		}
		return strings.Join(ps, ", ")
	}
	compile := func(src string) (*bytecode.Function, error) {
		s, err := syntax.ParseScript("t.js", src, syntax.Options{})
		require.NoError(t, err)
		return CompileScript(s)
	}
	for _, c := range []struct {
		n          int
		rest, body string
		regs       int // 0: the SyntaxError
	}{
		{255, "", "return p0;", 255},
		{256, "", "return p255;", 256},
		{256, "", "", 256},
		{255, ", ...r", "return r;", 256},
		{257, "", "return p0;", 0},
		{300, "", "", 0},
		{256, ", ...r", "return r;", 0},
		{256, "", "return arguments;", 0},
		{256, "", "return [p0];", 0},
	} {
		code, err := compile("function f(" + params(c.n) + c.rest + ") { " + c.body + " }")
		if c.regs == 0 {
			assert.ErrorContains(t, err, "SyntaxError: function needs more than 256 registers", "%d parameters%s, %q", c.n, c.rest, c.body)
			continue
		}
		require.NoError(t, err, "%d parameters%s, %q", c.n, c.rest, c.body)
		assert.Equal(t, c.regs, int(code.Children[0].NumRegs), "%d parameters%s, %q", c.n, c.rest, c.body)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestLineTable(t *testing.T) {
	fn := compileModule(t, "export function f(a) {\n  const b = a + 1;\n  return b.x;\n}")
	f := child(t, fn, "f")
	require.NotEmpty(t, f.LineTable)
	// The property read on line 3 maps back to line 3.
	for pc := 0; pc < len(f.Code); {
		op := bytecode.DecodeOp(f.Code[pc])
		if op == bytecode.GetProp {
			line, col := f.Position(uint32(pc))
			assert.Equal(t, 3, line)
			assert.Equal(t, 10, col)
		}
		pc += 1 + op.ExtraWords()
	}
	// The body's first instruction is the const's: nothing clears b, which
	// it writes before anything reads it.
	assert.Equal(t, uint32(0), f.LineTable[0].PC)
	assert.Equal(t, int32(2), f.LineTable[0].Line)
}

// TestLineTablePositions: the words emitted at one position share an entry,
// and a position revisited after another one gets a new entry.
func TestLineTablePositions(t *testing.T) {
	s, err := syntax.ParseScript("t.js", "a;\nbc;", syntax.Options{})
	require.NoError(t, err)
	f := (&compiler{file: s.File}).newFuncState(nil, nil, bytecode.KindScript, nil)
	for _, pos := range []int{0, 0, 3, 3, 4, 0, -1, 0, 4} {
		f.setPos(pos)
		f.emitWord(0)
	}
	assert.Equal(t, []bytecode.LineEntry{
		{PC: 0, Line: 1, Col: 1}, {PC: 2, Line: 2, Col: 1}, {PC: 4, Line: 2, Col: 2},
		{PC: 5, Line: 1, Col: 1}, {PC: 8, Line: 2, Col: 2},
	}, f.lines)
}

func TestTypeofIsFusion(t *testing.T) {
	fn := compileModule(t, `export function f(x) { return [typeof x === "string", "object" == typeof x, typeof x !== "function", typeof x === "nope", typeof x === y]; }`)
	f := child(t, fn, "f")
	count := 0
	for _, op := range ops(f) {
		if op == bytecode.TypeofIs {
			count++
		}
	}
	assert.Equal(t, 3, count, "three fusable comparisons; unknown type names and non-literals stay generic")
	assert.Contains(t, ops(f), bytecode.Typeof)
}

// TestTestJumps checks the conditions condJump compiles as the jump's own
// test: a typeof test, a template as the type name too, is JmpTypeofNot or
// JmpTypeofIs; a loose comparison with null or undefined is JmpNullish or
// JmpNotNullish; a strict one with undefined is JmpNotUndef where that
// jump is taken when the value is not undefined, else JmpStrictEqK. A
// template key names a property as a string does.
func TestTestJumps(t *testing.T) {
	fn := compileModule(t, "export function ty(x) { if (typeof x == `object`) return 1; if (typeof x != `function`) return 2; }\n"+
		"export function nu(x, y) { if (x == null) return 1; if (void 0 != y) return 2; }\n"+
		"export function un(x, y) { if (x === undefined) return 1; if (y !== void 0) return 2; }\n"+
		"export function key(o) { return o[`k`]; }\n"+
		"export function val(x) { return x == null; }")
	assert.Equal(t, []bytecode.Op{bytecode.JmpTypeofNot, bytecode.LoadInt, bytecode.Ret,
		bytecode.JmpTypeofIs, bytecode.LoadInt, bytecode.Ret, bytecode.RetUndef}, ops(child(t, fn, "ty")))
	assert.Equal(t, []bytecode.Op{bytecode.JmpNotNullish, bytecode.LoadInt, bytecode.Ret,
		bytecode.JmpNullish, bytecode.LoadInt, bytecode.Ret, bytecode.RetUndef}, ops(child(t, fn, "nu")))
	assert.Equal(t, []bytecode.Op{bytecode.JmpNotUndef, bytecode.LoadInt, bytecode.Ret,
		bytecode.JmpStrictEqK, bytecode.LoadInt, bytecode.Ret, bytecode.RetUndef}, ops(child(t, fn, "un")))
	assert.Equal(t, []bytecode.Op{bytecode.GetProp, bytecode.Ret, bytecode.RetUndef}, ops(child(t, fn, "key")))
	assert.Equal(t, []bytecode.Op{bytecode.LoadNull, bytecode.Eq, bytecode.Ret, bytecode.RetUndef}, ops(child(t, fn, "val")))
}

func TestNamedEvaluation(t *testing.T) {
	fn := compileModule(t, `
export const arrow = () => 1;
export const fexpr = function () {};
export const named = function real() {};
export default () => 2;
const o = { m: () => 3, meth() {}, ["comp" + "uted"]: () => 4 };
`)
	names := map[string]bool{}
	for _, c := range fn.Children {
		names[c.Name] = true
	}
	assert.True(t, names["arrow"])
	assert.True(t, names["fexpr"])
	assert.True(t, names["real"])
	assert.False(t, names["named"])
	assert.True(t, names["default"])
	assert.True(t, names["m"])
	assert.True(t, names["meth"])
	assert.True(t, names[""], "computed-key arrow stays anonymous")
}

func TestAccessorLiteral(t *testing.T) {
	fn := compileModule(t, `export function f(k) { return { get a() { return 1; }, set [k](v) {}, set 2(v) {} }; }`)
	want := `function f (normal) params=1 regs=4 env=0 ics=0
  K0 = "a"
  K1 = "2"
  0000 NewObject r1 3
  0001 LoadConst r2 K0
  0002 Closure r3 F0
  0003 DefineAccessor r1 r2 r3 2
  0005 Move r2 r0
  0006 Closure r3 F1
  0007 DefineAccessor r1 r2 r3 7
  0009 LoadConst r2 K1
  0010 Closure r3 F2
  0011 DefineAccessor r1 r2 r3 3
  0013 Ret r1
  0014 RetUndef
`
	f := child(t, fn, "f")
	dis := bytecode.Disassemble(f)
	assert.Equal(t, want, dis[:strings.Index(dis, "  F0:")])
	require.Len(t, f.Children, 3)
	for i, name := range []string{"get a", "", "set 2"} {
		assert.Equal(t, name, f.Children[i].Name)
		assert.Equal(t, bytecode.KindMethod, f.Children[i].Kind)
	}
	assert.Equal(t, 0, int(f.Children[0].Length))
	assert.Equal(t, 1, int(f.Children[1].Length))
}

// nestedSource builds a module nested n levels deep in the given shape.
func nestedSource(kind string, n int) string {
	rep := strings.Repeat
	switch kind {
	case "paren":
		return "const x = " + rep("(", n) + "1" + rep(")", n) + ";"
	case "array":
		return "const x = " + rep("[", n) + rep("]", n) + ";"
	case "object":
		return "const x = " + rep("{a:", n) + "1" + rep("}", n) + ";"
	case "block":
		return rep("{", n) + rep("}", n)
	case "if":
		return rep("if(1)", n) + ";"
	case "not":
		return "const x = " + rep("!", n) + "1;"
	case "func":
		return rep("function f(){", n) + rep("}", n)
	case "arrow":
		return "const x = " + rep("()=>", n) + "1;"
	case "call":
		return "const f = x => x; const x = " + rep("f(", n) + "1" + rep(")", n) + ";"
	case "ternary":
		return "const c = 1; const x = " + rep("c?", n) + "1" + rep(":0", n) + ";"
	case "template":
		return "const x = " + rep("`${", n) + "1" + rep("}`", n) + ";"
	case "new":
		return "const f = x => x; const x = " + rep("new ", n) + "f;"
	case "pattern":
		return "const " + rep("[", n) + "a" + rep("]", n) + " = [];"
	}
	panic("unknown kind " + kind)
}

// TestCompileAtNestingLimit compiles every nesting shape at the deepest
// level the parser accepts (syntax.MaxNestingDepth bounds its recursion):
// the scope pass and the compiler recurse over the same tree and must
// either compile it or fail with a positioned error, never overflow the Go
// stack (a fatal error the host cannot recover from).
func TestCompileAtNestingLimit(t *testing.T) {
	for _, kind := range []string{"paren", "array", "object", "block", "if", "not", "func", "arrow", "call", "ternary", "template", "new", "pattern"} {
		t.Run(kind, func(t *testing.T) {
			lo, hi := 1, syntax.MaxNestingDepth+1
			for hi-lo > 1 {
				mid := (lo + hi) / 2
				if _, err := syntax.ParseModule("deep.js", nestedSource(kind, mid), syntax.Options{}); err == nil {
					lo = mid
				} else {
					hi = mid
				}
			}
			m, err := syntax.ParseModule("deep.js", nestedSource(kind, lo), syntax.Options{})
			require.NoError(t, err)
			_, err = CompileModule(m)
			if err != nil {
				var ce *Error
				require.ErrorAs(t, err, &ce, "%s at depth %d: %v", kind, lo, err)
				t.Logf("%s at depth %d: %v", kind, lo, err)
				return
			}
			t.Logf("%s at depth %d: compiled", kind, lo)
		})
	}
}

// TestFoldChainLinear pins the linear compile time of long left-nested
// chains: compiling a chain folds every level's left operand on the way
// down, and before foldChain remembered the unfoldable spine this was
// quadratic (a 100,000-term `a+a+...` took 55 s, 20,000 terms of `&&` 2 s).
// One chain of 16n terms must cost about as much as sixteen chains of n
// terms (deep recursion allows up to 6x); a quadratic compiler is 16x
// slower. The trials count CPU time (cpuTime): with more threads running
// than CPUs, the long chain's wall time grew up to 7x the short chains'.
// No collection runs during them (but at a memory limit, which only a
// regression's quadratic garbage reaches), so the stack the first trial
// grows for the long chain (32 MB for "mixed") is not shrunk before the
// next two: growing it was most of the long logical chains' cost. The
// minimum of three trials keeps the ratio robust under -race too.
func TestFoldChainLinear(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(1 << 30)) // what a quadratic regression allocates
	chain := func(prefix, term, op, suffix string) func(n int) string {
		return func(n int) string {
			return "const a = 1; " + prefix + strings.TrimSuffix(strings.Repeat(term+op, n), op) + suffix
		}
	}
	for _, c := range []struct {
		name string
		n    int
		src  func(n int) string
	}{
		{"plus", 2000, chain("export const x = ", "a", "+", ";")},
		{"plus-const", 2000, chain("export const x = a+", "1", "+", ";")},
		{"concat", 2000, chain("export const x = ", `"abcdefgh"`, "+", ";")},
		{"concat-or", 300, func(n int) string {
			return "export const x = " + strings.Repeat("(", n) + `""` + strings.Repeat(` + "abcdefgh" || 0)`, n) + ";"
		}},
		{"lt", 2000, chain("export const x = ", "a", "<", ";")},
		{"and", 2000, chain("export const x = ", "a", "&&", ";")},
		{"or", 2000, chain("export const x = ", "a", "||", ";")},
		{"nullish", 2000, chain("export const x = ", "a", "??", ";")},
		{"mixed", 2000, chain("export const x = ", "a*a+a", "&&", ";")},
		{"if-and", 500, chain("if (", "a", "&&", ") {}")},
		{"lt-or", 2000, chain("export const x = ", "a<1", "||", ";")},
		{"and-lt", 2000, func(n int) string { return "export const x = a" + strings.Repeat(" && a<1", n) + ";" }},
		{"return-lt-or", 2000, func(n int) string { return "export function f(a) { return a<0" + strings.Repeat(" || a<1", n) + "; }" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			runtime.GC() // the garbage of the cases before
			parse := func(n int) *syntax.Module {
				m, err := syntax.ParseModule("chain.js", c.src(n), syntax.Options{})
				require.NoError(t, err)
				return m
			}
			compile := func(ms ...*syntax.Module) time.Duration {
				var err error
				d := cpuTime(func() {
					for _, m := range ms {
						if _, err = CompileModule(m); err != nil {
							return
						}
					}
				})
				require.NoError(t, err)
				return d
			}
			const k = 16
			one, many := time.Duration(1<<62), time.Duration(1<<62)
			for range 3 {
				small := make([]*syntax.Module, k)
				for i := range small {
					small[i] = parse(c.n)
				}
				many = min(many, compile(small...))
				one = min(one, compile(parse(k*c.n)))
			}
			t.Logf("%s: 1 x %d terms %v, %d x %d terms %v", c.name, k*c.n, one, k, c.n, many)
			assert.Less(t, one, 6*many+2*time.Millisecond, "compile time grows quadratically with the chain length")
		})
	}
}

// TestAssignmentIntoRegister: an assignment builds its value in the
// variable's register, except inside a try statement, where a handler must
// not see a partial value (the root package's
// TestAssignmentThrowKeepsVariable), unless the value cannot throw once it
// is written.
func TestAssignmentIntoRegister(t *testing.T) {
	fn := compileModule(t, `
export function out(g) { var a = 1; a = g(); return a; }
export function inTry(g) { var a = 1; try { a = g(); } catch (e) {} return a; }
export function simple(g) { var a = 1; try { a = 2; a = g; a = function () {}; } catch (e) {} return a; }`)
	assert.Equal(t, []bytecode.Op{bytecode.LoadInt, bytecode.Move, bytecode.Call, bytecode.Ret, bytecode.RetUndef},
		ops(child(t, fn, "out")), "the callee and the result share the variable's register")
	assert.Equal(t, []bytecode.Op{bytecode.LoadInt, bytecode.Move, bytecode.Call, bytecode.Move, bytecode.Jmp, bytecode.Move, bytecode.Ret, bytecode.RetUndef},
		ops(child(t, fn, "inTry")), "the result is moved to the variable once the call returns")
	assert.Equal(t, []bytecode.Op{bytecode.LoadInt, bytecode.LoadInt, bytecode.Move, bytecode.Closure, bytecode.Jmp, bytecode.Move, bytecode.Ret, bytecode.RetUndef},
		ops(child(t, fn, "simple")))
}

// TestStoreIntoRegister: x = o[k] = v computes v in x's register, from
// which the store reads it, except inside a try statement, where a handler
// would find x written before a store that throws, and when o or k reads x.
func TestStoreIntoRegister(t *testing.T) {
	fn := compileModule(t, `
export function chain(o, k, s) { for (;;) s = o[k] = s * 2; }
export function prop(o, s) { for (;;) s = o.p = s * 2; }
export function inTry(o, k, s) { try { s = o[k] = s * 2; } catch (e) {} return s; }
export function keyReads(o, s) { s = o[s] = s * 2; return s; }
export function objReads(s) { s = s.p = s.q; return s; }`)
	assert.NotContains(t, ops(child(t, fn, "chain")), bytecode.Move)
	assert.NotContains(t, ops(child(t, fn, "prop")), bytecode.Move)
	for _, name := range []string{"inTry", "keyReads", "objReads"} {
		assert.Contains(t, ops(child(t, fn, name)), bytecode.Move, name)
	}
}

// TestCompareEnvOperand: a compare-and-branch reads a binding closures
// share in the innermost environment itself (bytecode.EnvOperand), without
// a GetEnv before it; one in its TDZ, one further out and a strict
// equality keep their read.
func TestCompareEnvOperand(t *testing.T) {
	fn := compileModule(t, `
export function bound(a) { var n = a, k = () => n; for (var i = 0; i < n; i++) a++; return a; }
export function tdz(a) { for (var i = 0; i < n; i++) a++; let n = a, k = () => n; return a; }
export function outer(a) { var n = a; return function () { var m = 1, g = () => m; for (var i = 0; i < n; i++) a++; return a + g(); }; }
export function strict(a) { var n = a, k = () => n; if (a === n) a++; return a; }`)
	envReads := func(f *bytecode.Function) (n int) {
		for _, w := range words(f) {
			if op := bytecode.DecodeOp(w[0]); op >= bytecode.JmpLt && op <= bytecode.JmpNGe && w[1] >= bytecode.EnvOperand {
				n++
			}
		}
		return n
	}
	bound := child(t, fn, "bound")
	assert.Equal(t, 1, envReads(bound))
	assert.NotContains(t, ops(bound), bytecode.GetEnv0)
	tdz := child(t, fn, "tdz")
	assert.Equal(t, 0, envReads(tdz))
	assert.Contains(t, ops(tdz), bytecode.GetEnvChk)
	inner := child(t, fn, "outer").Children[0]
	assert.Equal(t, 0, envReads(inner))
	assert.Contains(t, ops(inner), bytecode.GetEnv1)
	strict := child(t, fn, "strict")
	assert.Equal(t, 0, envReads(strict))
	assert.Contains(t, ops(strict), bytecode.GetEnv0)
}

// TestValueFromItsRegister: the property of an object literal that adds
// its properties one at a time and a member assignment whose value is not
// used store a local from its own register, without a copy; an assignment
// whose value is used still builds it in the result's register.
func TestValueFromItsRegister(t *testing.T) {
	fn := compileModule(t, `
export function lit(a, b) { return {a, b: b, c: 1, ...a}; }
export function set(o, v) { o.p = v; o[0] = v; }
export function used(o, v) { return o.p = v; }`)
	assert.Equal(t, []bytecode.Op{bytecode.NewObject, bytecode.DefineField, bytecode.DefineField, bytecode.LoadInt, bytecode.DefineField, bytecode.CopyDataProps, bytecode.Ret, bytecode.RetUndef},
		ops(child(t, fn, "lit")))
	assert.Equal(t, []bytecode.Op{bytecode.SetProp, bytecode.LoadInt, bytecode.SetElem, bytecode.RetUndef},
		ops(child(t, fn, "set")))
	assert.Equal(t, []bytecode.Op{bytecode.Move, bytecode.SetProp, bytecode.Ret, bytecode.RetUndef},
		ops(child(t, fn, "used")))
}

// TestGetElemRefEmission checks that only a compound assignment or update
// of a computed member whose key may be an object reads it with
// GetElemRef, which converts the key into a temporary when the key is a
// variable.
func TestGetElemRefEmission(t *testing.T) {
	fn := compileModule(t, `export function conv(o, k) { o[k] += 1; o[k]++; o[k] ??= 1; o[k.x] -= 1; }
export function prim(o, k) { o[0] += 1; o["x"] += 1; o[k + 1]++; o[-k] *= 2; o[`+"`${k}`"+`] |= 1; o.x++; }
export function plain(o, k) { o[k] = 1; o[k]; delete o[k]; }
class C { #x = 0; m(k) { this.#x += 1; this.#x++; } }`)
	count := func(name string) int {
		n := 0
		for _, op := range ops(child(t, fn, name)) {
			if op == bytecode.GetElemRef {
				n++
			}
		}
		return n
	}
	assert.Equal(t, 4, count("conv"))
	assert.Equal(t, 0, count("prim"))
	assert.Equal(t, 0, count("plain"))
	// The write uses the converted key, in a temporary for the variable k
	// and in place for the temporary k.x.
	code := child(t, fn, "conv").Code
	var keys []uint8
	for pc := 0; pc < len(code); pc += 1 + bytecode.DecodeOp(code[pc]).ExtraWords() {
		switch w := code[pc]; bytecode.DecodeOp(w) {
		case bytecode.GetElemRef:
			c, x := bytecode.DecodeC(w), uint8(code[pc+1])
			if x == 1 {
				assert.NotEqual(t, uint8(1), c, "a variable key must keep its value")
			} else {
				assert.Equal(t, x, c, "a temporary key is converted in place")
			}
			keys = append(keys, c)
		case bytecode.SetElem:
			require.NotEmpty(t, keys)
			assert.Equal(t, keys[len(keys)-1], bytecode.DecodeB(w), "the write reuses the converted key")
		}
	}
	assert.Len(t, keys, 4)
	assert.Equal(t, 0, count("m"), "a private name is never converted")
}

// TestResolveGlobalEmission checks that only a strict assignment of an
// undeclared name from a value that may run code resolves the name first.
func TestResolveGlobalEmission(t *testing.T) {
	for src, want := range map[string][]bytecode.Op{
		`"use strict"; x = f();`:                  {bytecode.ResolveGlobal, bytecode.SetGlobalRef},
		`"use strict"; x = y;`:                    {bytecode.ResolveGlobal, bytecode.SetGlobalRef},
		`"use strict"; x = 1; x = "s"; x = null;`: {bytecode.SetGlobal},
		`(function (v) { "use strict"; x = v; })`: {bytecode.SetGlobal},
		`"use strict"; var x; x = f();`:           {bytecode.SetGlobal},
		`"use strict"; x += f(); x++; x ??= f();`: {bytecode.SetGlobal},
		`x = f();`: {bytecode.SetGlobal},
		`(function () { "use strict"; x = f(); })`: {bytecode.ResolveGlobal, bytecode.SetGlobalRef},
	} {
		s, err := syntax.ParseScript("s.js", src, syntax.Options{})
		require.NoError(t, err)
		fn, err := CompileScript(s)
		require.NoError(t, err)
		var got []bytecode.Op
		var walk func(*bytecode.Function)
		walk = func(fn *bytecode.Function) {
			for _, op := range ops(fn) {
				switch op {
				case bytecode.ResolveGlobal, bytecode.SetGlobalRef, bytecode.SetGlobal, bytecode.SetGlobalSloppy:
					if !slices.Contains(got, op) {
						got = append(got, op)
					}
				}
			}
			for _, c := range fn.Children {
				walk(c)
			}
		}
		walk(fn)
		assert.Equal(t, want, got, src)
	}
}

// TestUndefRangeLeavesOutWrittenVars: the UndefRange of a function's
// prologue clears the registers of its vars that may be read before they
// are written, and leaves out those every path writes before it reads
// them, its lexical bindings and its function declarations.
func TestUndefRangeLeavesOutWrittenVars(t *testing.T) {
	src := `
function first(g) { var x = g(); var y = x; return y; }
function readBefore(g) { g(x); var x = 1; return x; }
function inBranch(g) { if (g) { var x = 1; } return x; }
function inTry(g) { try { var x = g(); } finally {} return x; }
function inLabelledBlock() { l: { var x = 1; break l; } return x; }
function forHead() { var r; for (var i = 0; i < 2; i++) { var y = i; r = y; } return r; }
function labelledFor() { l: for (var i = 0; i < 2; i++) { continue l; } return i; }
function selfInit() { var x = x; return x; }
function order() { var a = b, b = 1; return a + b; }
function noInit() { var x; return x; }
function lexical() { let x; const y = 1; class C {} return [x, y, C]; }
function decl() { return h(); function h() { return 1; } }
function annexB() { var r = k; { function k() {} } return r; }
function inWith(o) { with (o) { var x = 1; } return x; }
function withEval(s) { var x = 1; return eval(s) + x; }
function split(a, b = () => a) { var a = 2; var x = 1; var y = y; return [a, x, y]; }
function* gen() { var x = 1; yield x; }
async function async() { var x = 1; await x; }
function strict() { "use strict"; var x = 1; { function k() {} } return x; }
function inCase(t) { switch (t) { case 1: var x = 1; return x; case 2: var y = 2; break; } return y; }
function caseFallthrough(t) { switch (t) { case 1: var x = 1; case 2: return x; } }
function ifElse(c) { if (c) var x = 1; else x = 2; return x; }
function returnBranch(c) { if (c) { return 0; } else { var x = 1; } return x; }
function loopBody(a) { for (var i = 0; i < a.length; i++) { var v = a[i]; if (v) return v; } return 0; }
function afterLoop(a) { for (var k in a) var v = k; return v; }
function whileBody(n) { while (n) { var t = n; n = t - 1; } return n; }
function doWhile(a) { do { var x = a(); } while (x); return 0; }
function shortCircuit(c) { c && (x = 1); return x; var x; }
function logicalAssign(c) { var r = c; r ||= (x = 1); return x; var x; }
function bothArms(c) { var x = c ? (y = 1) : (y = 2); return x + y; var y; }
function optional(o) { o?.f(x = 1); return x; var x; }
function tryCatch(g) { try { var x = g(); } catch (e) { x = 0; } return x; }
function catchReads(g) { try { var x = g(); } catch (e) { return x; } return x; }
function finallyWrites(g) { try { g(); } finally { var x = 1; } return x; }
function member(o) { o[x = 1] = x; return x; var x; }
function pattern(o) { var {p} = o; return p; }
`
	fn := compileScript(t, src)
	// cleared returns the number of registers the UndefRange at the start
	// of f clears.
	cleared := func(name string) int {
		f := child(t, fn, name)
		n := 0
		for pc := 0; pc < len(f.Code); {
			w := f.Code[pc]
			op := bytecode.DecodeOp(w)
			if op == bytecode.UndefRange {
				n += int(uint8(w >> 16))
			}
			pc += 1 + op.ExtraWords()
		}
		return n
	}
	for _, c := range []struct {
		name string
		want int
	}{
		{"first", 0},
		{"readBefore", 1},
		{"inBranch", 1},
		{"inTry", 0}, // a throw leaves the function
		{"inLabelledBlock", 1},
		{"forHead", 1}, // r; the head writes i, the body y before it reads it
		{"labelledFor", 0},
		{"selfInit", 1},
		{"order", 1}, // b
		{"noInit", 1},
		{"lexical", 0},
		{"decl", 0},
		{"annexB", 1}, // the var k
		{"inWith", 1},
		{"withEval", 0}, // x lives in the environment
		{"split", 1},    // the body's y; its a starts with the parameter's value
		{"gen", 1},
		{"async", 1},
		{"strict", 0},          // k is the block's own
		{"inCase", 1},          // y, read after the switch
		{"caseFallthrough", 1}, // x, which case 2 reads when entered by its test
		{"ifElse", 0},
		{"returnBranch", 0},
		{"loopBody", 0},
		{"afterLoop", 1}, // v: the body may not run
		{"whileBody", 0},
		{"doWhile", 1}, // the test is checked as if the body had not run
		{"shortCircuit", 1},
		{"logicalAssign", 1},
		{"bothArms", 0},
		{"optional", 1},
		{"tryCatch", 0},
		{"catchReads", 1},
		{"finallyWrites", 0},
		{"member", 0}, // the key's assignment runs before the value
		{"pattern", 1},
	} {
		assert.Equal(t, c.want, cleared(c.name), c.name)
	}

	// A function whose vars take all 256 registers clears them in two
	// runs: one UndefRange would encode the length 256 as 0.
	names := make([]string, 256)
	for i := range names {
		names[i] = "a" + strconv.Itoa(i)
	}
	fn = compileScript(t, "function wide() { var "+strings.Join(names, ", ")+"; return a0; }")
	assert.Equal(t, 256, cleared("wide"))
}

// TestEnvOrderByReferences: a function with more captured bindings than
// GetEnv reaches gives the most referenced the first slots after its
// parameters, so that a closure reads the hot one with GetEnv and a cold
// one with GetEnvW.
func TestEnvOrderByReferences(t *testing.T) {
	var b strings.Builder
	b.WriteString("function outer(a, b) {\n")
	for i := range 300 {
		fmt.Fprintf(&b, "var v%d = %d; function f%d() { return v%d; }\n", i, i, i, i)
	}
	b.WriteString("function hot() { return v299 + v299 + v299 + v299 + a + b; }\n")
	b.WriteString("function cold() { return v0; }\n")
	b.WriteString("return hot; }")
	fn := compileScript(t, b.String())
	outer := child(t, fn, "outer")
	require.Greater(t, len(outer.CaptureLayout), bytecode.MaxRegister+1)
	assert.Equal(t, uint16(0), outer.CaptureLayout[0], "a keeps slot 0")
	assert.Equal(t, uint16(1), outer.CaptureLayout[1], "b keeps slot 1")
	hot := child(t, outer, "hot")
	assert.NotContains(t, ops(hot), bytecode.GetEnvW)
	assert.Contains(t, ops(hot), bytecode.GetEnv0)
	// v0 has one reference as each of v1..v298 has, the function
	// declarations too: past the first 256 slots some of them read wide.
	wide := 0
	for i := range 300 {
		if slices.Contains(ops(child(t, outer, fmt.Sprintf("f%d", i))), bytecode.GetEnvW) {
			wide++
		}
	}
	assert.Greater(t, wide, 0)
	assert.Less(t, wide, 300)
}

// TestArrayPushK checks which elements of an array literal ArrayPushK
// pushes: runs of two or more constants (numbers, strings, null and
// undefined as fold finds them), at most arrayPushKMax at a time and while
// the constant pool stays within arrayPushKPool; any other element is
// pushed on its own. A literal that is one such run is a NewArrayK.
func TestArrayPushK(t *testing.T) {
	seq := func(n int, f func(i int) string) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(f(i))
		}
		return "[" + b.String() + "]"
	}
	num := func(i int) string { return strconv.Itoa(i) }
	for _, tc := range []struct {
		src  string
		runs []int // the n of each ArrayPushK, in order
		push int   // ArrayPush count
		newK int   // the n of the literal's NewArrayK, 0 for none
	}{
		{`[1, 2, 3]`, nil, 0, 3},
		{`[1, 2]`, nil, 0, 2},
		{`[1]`, nil, 1, 0},
		{`[1, x, 2, 3]`, []int{2}, 2, 0},
		{`[, 1, 2, , 3]`, []int{2}, 1, 0},
		{`[...a, 1, 2, ...b]`, []int{2}, 0, 0},
		{`[-0, 'é', null, undefined, NaN, Infinity, -1, 1 / 2, 'a' + 'b', void 0, ` + "`t`" + `]`, nil, 0, 11},
		{`[-0, 'é', null, undefined, NaN, x]`, []int{5}, 1, 0},
		{`(function (undefined) { return [undefined, 1]; })`, nil, 2, 0},
		{`with (o) [undefined, NaN];`, nil, 2, 0},
		{`[x, y]`, nil, 2, 0},
		{`[true, false, {}]`, nil, 3, 0},
		{seq(256, num), nil, 0, 256},
		{seq(257, num), []int{256}, 1, 0},
		{seq(600, num), []int{256, 256, 88}, 0, 0},
		{seq(5000, num), slices.Repeat([]int{256}, 16), 5000 - 16*256, 0},
	} {
		fn := compileScript(t, tc.src)
		var runs []int
		var walk func(fn *bytecode.Function)
		push, newK := 0, 0
		walk = func(fn *bytecode.Function) {
			for pc := 0; pc < len(fn.Code); {
				w := fn.Code[pc]
				op := bytecode.DecodeOp(w)
				switch op {
				case bytecode.ArrayPushK:
					x := fn.Code[pc+1]
					runs = append(runs, int(bytecode.ExtraHi(x)))
					assert.LessOrEqual(t, int(bytecode.ExtraLo(x))+int(bytecode.ExtraHi(x)), len(fn.Consts))
				case bytecode.ArrayPush:
					push++
				case bytecode.NewArrayK:
					x := fn.Code[pc+1]
					newK = int(bytecode.ExtraHi(x))
					assert.LessOrEqual(t, int(bytecode.ExtraLo(x))+int(bytecode.ExtraHi(x)), len(fn.Consts))
				}
				pc += 1 + op.ExtraWords()
			}
			for _, c := range fn.Children {
				walk(c)
			}
		}
		walk(fn)
		name := tc.src[:min(len(tc.src), 40)]
		assert.Equal(t, tc.runs, runs, name)
		assert.Equal(t, tc.push, push, name)
		assert.Equal(t, tc.newK, newK, name)
		assert.LessOrEqual(t, len(fn.Consts), arrayPushKPool+16, name)
	}
}

// TestObjectLitK: a literal of two or more plain data properties under
// static keys is one NewObjectLit of its values, evaluated into registers in a row, a
// call's result where the call leaves it; its keys are a run of constants,
// which the next literal of the same keys shares. Every other literal makes
// its object first and adds its properties one at a time.
func TestObjectLitK(t *testing.T) {
	fn := compileModule(t, `
function g() {}
let k;
export function rows(x) { const p = {a: x, "b c": 1}; const q = {a: g(), "b c": 2}; return [p, q]; }
export function calls() { return {a: g(), b: g(), c: {d: g()}}; }
export function* gen() { return {a: yield 1, 1.5: 2, "4294967295": 3}; }
export function sixteen(a) { return {a, b: a, c: a, d: a, e: a, f: a, g: a, h: a, i: a, j: a, k: a, l: a, m: a, n: a, o: a, p: a}; }
export function seventeen(a) { return {a, b: a, c: a, d: a, e: a, f: a, g: a, h: a, i: a, j: a, k: a, l: a, m: a, n: a, o: a, p: a, q: a}; }
export function empty() { return {}; }
export function single(a) { return {a}; }
export function locals(a, b) { return {a, b: b}; }
export function twice() { return {a: 1, a: 2}; }
export function proto() { return {__proto__: null, a: 1}; }
export function protoString() { return {"__proto__": 1}; }
export function protoShorthand(__proto__) { return {__proto__}; }
export function index() { return {a: 1, 0: 2}; }
export function indexString() { return {"1": 1}; }
export function computed() { return {[k]: 1}; }
export function spread() { return {a: 1, ...k}; }
export function method() { return {a: 1, m() {}}; }
export function getter() { return {a: 1, get b() { return 1; }}; }
`)
	type lit struct{ b, c, lo, ic int }
	lits := func(f *bytecode.Function) []lit {
		var out []lit
		for pc := 0; pc < len(f.Code); pc += 1 + bytecode.DecodeOp(f.Code[pc]).ExtraWords() {
			if w := f.Code[pc]; bytecode.DecodeOp(w) == bytecode.NewObjectLit {
				x := f.Code[pc+1]
				out = append(out, lit{int(bytecode.DecodeB(w)), int(bytecode.DecodeC(w)), int(bytecode.ExtraLo(x)), int(bytecode.ExtraHi(x))})
			}
		}
		return out
	}
	keys := func(f *bytecode.Function, l lit) []string {
		var out []string
		for _, k := range f.Consts[l.lo : l.lo+l.c] {
			assert.True(t, k.Kind == bytecode.ConstString && k.Key, "%v", k)
			out = append(out, k.Str)
		}
		return out
	}

	rows := child(t, fn, "rows")
	assert.NotContains(t, ops(rows), bytecode.NewObject)
	assert.NotContains(t, ops(rows), bytecode.DefineField)
	if l := lits(rows); assert.Len(t, l, 2) {
		assert.Equal(t, []string{"a", "b c"}, keys(rows, l[0]))
		assert.Equal(t, l[0].lo, l[1].lo, "the second literal's keys are the first's")
		assert.NotEqual(t, l[0].ic, l[1].ic)
	}

	calls := child(t, fn, "calls")
	assert.NotContains(t, ops(calls), bytecode.Move, "each call's result is in its value's register")
	if l := lits(calls); assert.Len(t, l, 1) {
		assert.Equal(t, []string{"a", "b", "c"}, keys(calls, l[0]))
	}

	gen := child(t, fn, "gen")
	if l := lits(gen); assert.Len(t, l, 1) {
		assert.Equal(t, []string{"a", "1.5", "4294967295"}, keys(gen, l[0]))
	}
	if l := lits(child(t, fn, "sixteen")); assert.Len(t, l, 1) {
		assert.Equal(t, 16, l[0].c)
	}

	for _, name := range []string{"seventeen", "empty", "single", "locals", "twice", "proto", "protoString", "protoShorthand", "index", "indexString", "computed", "spread", "method", "getter"} {
		f := child(t, fn, name)
		assert.Contains(t, ops(f), bytecode.NewObject, name)
		assert.NotContains(t, ops(f), bytecode.NewObjectLit, name)
	}
}

// TestObjectLitKeyRuns checks the key runs of a function's literals: one
// run for each set of keys, however far apart its literals are; past
// objectLitKeyBudget, a literal of keys without a run makes its object one
// property at a time, under a constant per name; and a function whose runs
// would take its constants past the limit compiles again without them.
func TestObjectLitKeyRuns(t *testing.T) {
	type lit struct{ lo, c int }
	lits := func(f *bytecode.Function) []lit {
		var out []lit
		for pc := 0; pc < len(f.Code); pc += 1 + bytecode.DecodeOp(f.Code[pc]).ExtraWords() {
			if w := f.Code[pc]; bytecode.DecodeOp(w) == bytecode.NewObjectLit {
				out = append(out, lit{int(bytecode.ExtraLo(f.Code[pc+1])), int(bytecode.DecodeC(w))})
			}
		}
		return out
	}
	keyConsts := func(f *bytecode.Function) int {
		n := 0
		for _, k := range f.Consts {
			if k.Kind == bytecode.ConstString && k.Key {
				n++
			}
		}
		return n
	}
	count := func(f *bytecode.Function, op bytecode.Op) int {
		n := 0
		for _, o := range ops(f) {
			if o == op {
				n++
			}
		}
		return n
	}

	// Six sets of keys, their literals interleaved, the first six in the
	// order a b, id name tags, id name, x y z, b a, a c: 12 constants, as
	// {id, name} takes the start of {id, name, tags}'s run, whose id is the
	// function's property name id.
	sets := []string{"a: 1, b: 2", "a: 1, c: 2", "b: 1, a: 2", "x: 1, y: 2, z: 3", "id: 1, name: 2", "id: 1, name: 2, tags: 3"}
	var src strings.Builder
	src.WriteString("export function rows() { return [")
	for i := range 60 {
		fmt.Fprintf(&src, "{%s}, ", sets[(i*5)%len(sets)])
	}
	src.WriteString("]; }")
	rows := child(t, compileModule(t, src.String()), "rows")
	l := lits(rows)
	require.Len(t, l, 60)
	runs := map[int]bool{}
	for i, x := range l {
		runs[x.lo] = true
		assert.Equal(t, l[i%len(sets)].lo, x.lo, "literal %d", i)
	}
	assert.Len(t, runs, len(sets)-1)
	assert.Equal(t, l[1].lo, l[2].lo)
	assert.Equal(t, 12, keyConsts(rows))

	// Sets of four keys fill the budget; a literal of a new set past it
	// makes its object one property at a time, its names a, b and c the
	// constants of the first run, and one of a set with a run still takes
	// the run.
	src.Reset()
	src.WriteString("export function many() { return [")
	for i := range objectLitKeyBudget / 4 {
		fmt.Fprintf(&src, "{a: 1, b: 2, c: 3, k%d: 4}, ", i)
	}
	src.WriteString("{a: 1, b: 2, c: 3, past: 4}, {a: 1, b: 2, c: 3, k7: 4}]; }")
	many := child(t, compileModule(t, src.String()), "many")
	l = lits(many)
	require.Len(t, l, objectLitKeyBudget/4+1)
	assert.Equal(t, l[7].lo, l[len(l)-1].lo)
	assert.Equal(t, 1, count(many, bytecode.NewObject))
	assert.Equal(t, 4, count(many, bytecode.DefineField))
	assert.Equal(t, objectLitKeyBudget+1, keyConsts(many)) // and past, through nameConst

	// 65,000 strings leave room for the names of the literals, a constant
	// each, not for their runs.
	src.Reset()
	src.WriteString("export function full() { var s; ")
	for i := range 65000 {
		fmt.Fprintf(&src, "s = 's%d'; ", i)
	}
	for i := range 200 {
		fmt.Fprintf(&src, "s = {a: 1, b: 2, k%d: 3}; ", i)
	}
	src.WriteString("return s; }")
	full := child(t, compileModule(t, src.String()), "full")
	assert.Empty(t, lits(full))
	assert.Equal(t, 200, count(full, bytecode.NewObject))
	assert.Equal(t, 202, keyConsts(full))
	assert.Len(t, full.Consts, 65000+202)
}

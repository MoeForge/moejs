package engine

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleFixture is a parsed, compiled and evaluated module in a fresh realm.
type moduleFixture struct {
	t   *testing.T
	r   *Realm
	env *ModuleEnv
}

// evalModule parses, compiles and evaluates src as a module in a fresh realm.
func evalModule(t *testing.T, src string) *moduleFixture {
	t.Helper()
	m, err := syntax.ParseModule("t.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	return &moduleFixture{t: t, r: r, env: env}
}

// export returns ToGo of the named export.
func (f *moduleFixture) export(name string) any {
	f.t.Helper()
	v, ok := f.env.GetBindingValue(name)
	require.True(f.t, ok, "export %q", name)
	return f.r.ToGo(v)
}

// callErr calls the exported function with FromGo-converted arguments.
func (f *moduleFixture) callErr(name string, args ...any) (any, error) {
	f.t.Helper()
	fn, ok := f.env.GetBindingValue(name)
	require.True(f.t, ok, "export %q", name)
	argv := make([]Value, len(args))
	for i, a := range args {
		v, err := f.r.FromGo(a)
		require.NoError(f.t, err)
		argv[i] = v
	}
	res, err := f.r.Call(fn, Undefined(), argv)
	if err != nil {
		return nil, err
	}
	return f.r.ToGo(res), nil
}

// call is callErr that fails the test on error.
func (f *moduleFixture) call(name string, args ...any) any {
	f.t.Helper()
	res, err := f.callErr(name, args...)
	require.NoError(f.t, err)
	return res
}

// evalExpr evaluates a single expression through an exported function.
func evalExpr(t *testing.T, expr string) any {
	t.Helper()
	return evalModule(t, "export function f() { return ("+expr+"); }").call("f")
}

// errMessage returns "Name: message" of a thrown JS error.
func errMessage(t *testing.T, err error) string {
	t.Helper()
	var exc *Exception
	require.True(t, errors.As(err, &exc), "want *Exception, got %T: %v", err, err)
	return errorDisplayString(exc.Value)
}

func TestInterpExpressions(t *testing.T) {
	tests := []struct {
		expr string
		want any
	}{
		{"1 + 2", int64(3)},
		{"0.1 + 0.2", 0.30000000000000004},
		{"7 / 2", 3.5},
		{"7 % 3", int64(1)},
		{"-7 % 3", int64(-1)},
		{"2 ** 10", int64(1024)},
		{"(-8) ** (1/3)", nil}, // NaN exports as float NaN; checked separately
		{"'a' + 1", "a1"},
		{"1 + '2'", "12"},
		{"'3' * '4'", int64(12)},
		{"'3' - 1", int64(2)},
		{"'3' - '1'", int64(2)},
		{"'3' + 1", "31"},
		{"null - 1", int64(-1)},
		{"1 - 1000", int64(-999)},
		{"'s' - 1000", nil},
		{"true + 1", int64(2)},
		{"null + 1", int64(1)},
		{"+'  42  '", int64(42)},
		{"+'0x1f'", int64(31)},
		{"-'5'", int64(-5)},
		{"1 < 2", true},
		{"2 <= 2", true},
		{"'a' < 'b'", true},
		{"'10' < '9'", true},
		{"10 < '9'", false},
		{"null >= 0", true},
		{"1 == '1'", true},
		{"1 === '1'", false},
		{"null == undefined", true},
		{"null === undefined", false},
		{"0 == false", true},
		{"'' == 0", true},
		{"NaN == NaN", false},
		{"NaN !== NaN", true},
		{"5 & 3", int64(1)},
		{"5 | 3", int64(7)},
		{"5 ^ 3", int64(6)},
		{"~5", int64(-6)},
		{"1 << 31", int64(-2147483648)},
		{"-1 >>> 0", int64(4294967295)},
		{"-16 >> 2", int64(-4)},
		{"2 ** 32 | 0", int64(0)},
		{"!0", true},
		{"!'x'", false},
		{"!!null", false},
		{"void 1", nil},
		{"typeof 1", "number"},
		{"typeof 's'", "string"},
		{"typeof true", "boolean"},
		{"typeof undefined", "undefined"},
		{"typeof null", "object"},
		{"typeof {}", "object"},
		{"typeof (() => 1)", "function"},
		{"typeof 1n", "bigint"},
		{"typeof notDeclaredAnywhere", "undefined"},
		{"typeof 1 === 'number'", true},
		{"'x' === typeof 1", false},
		{"typeof 1 !== 'string'", true},
		{"1n === 1n", true},
		{"String(12345678901234567890n)", "12345678901234567890"},
		{"1 ? 'a' : 'b'", "a"},
		{"0 ? 'a' : 'b'", "b"},
		{"(1, 2, 3)", int64(3)},
		{"0x10 + 0o10 + 0b10", int64(26)},
		{"1_000_000", int64(1000000)},
		{"'\\u{1F600}'.length", int64(2)},
		{"'abc'[1]", "b"},
		{"'abc'.length", int64(3)},
		{"[1,2,3][1]", int64(2)},
		{"[1,2,3].length", int64(3)},
		{"[,1].length", int64(2)},
		{"[,1][0]", nil},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got := evalExpr(t, tt.expr)
			if tt.expr == "(-8) ** (1/3)" || tt.expr == "'s' - 1000" {
				assert.NotEqual(t, got, got) // NaN
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestInterpArithmeticErrors(t *testing.T) {
	f := evalModule(t, `
export function big() { return 1n / 0n; }
export function mix() { return 1n + 1; }
export function sym() { return -Symbol(); }
export function notFn() { const o = {}; return o.missing(); }
export function nullProp() { const o = null; return o.x; }
export function undefProp() { let o; return o.x; }
export function undefElem() { let o; return o[1]; }
export function setNull() { const o = null; o.x = 1; }
export function undef() { return notDefined; }
export function newNonCtor() { return new (() => 1)(); }
`)
	for _, tt := range []struct{ fn, want string }{
		{"big", "RangeError: Division by zero"},
		{"mix", "TypeError: Cannot mix BigInt and other types, use explicit conversions"},
		{"notFn", "TypeError: o.missing is not a function"},
		{"nullProp", "TypeError: Cannot read properties of null (reading 'x')"},
		{"undefProp", "TypeError: Cannot read properties of undefined (reading 'x')"},
		{"undefElem", "TypeError: Cannot read properties of undefined (reading '1')"},
		{"setNull", "TypeError: Cannot set property 'x' of null"},
		{"undef", "ReferenceError: notDefined is not defined"},
		{"newNonCtor", "TypeError: (intermediate value) is not a constructor"},
	} {
		_, err := f.callErr(tt.fn)
		require.Error(t, err, tt.fn)
		assert.Equal(t, tt.want, errMessage(t, err), tt.fn)
	}
	_, err := f.callErr("sym")
	require.Error(t, err)
}

// TestInterpCalleeNames checks that a call or new of a value that is not
// callable names the callee as written, and names the value when the
// template has no source to describe it from.
func TestInterpCalleeNames(t *testing.T) {
	const src = `
export function member() { const o = {}; return o.missing(); }
export function chained() { const g = () => 1; return g()(); }
export function spread() { const o = {x: 1}; return o.x(...[1]); }
export function ctor() { const o = {}; return new o.C(); }
export function arrowCtor() { const A = () => 1; return new A(); }
export function staticField() { class X { static y = X.z(); } }
export function staticBlock() { class X { static { X.z(); } } }
export function field() { class X { f = this.z(); } new X(); }
export function ctorBody() { class X extends Object { constructor() { super(); this.q(); } } new X(); }
`
	f := evalModule(t, src)
	for _, tt := range []struct{ fn, want string }{
		{"member", "TypeError: o.missing is not a function"},
		{"member", "TypeError: o.missing is not a function"}, // cached
		{"chained", "TypeError: g(...) is not a function"},
		{"spread", "TypeError: o.x is not a function"},
		{"ctor", "TypeError: o.C is not a constructor"},
		{"arrowCtor", "TypeError: A is not a constructor"},
		{"staticField", "TypeError: X.z is not a function"},
		{"staticBlock", "TypeError: X.z is not a function"},
		{"field", "TypeError: this.z is not a function"},
		{"ctorBody", "TypeError: this.q is not a function"},
	} {
		_, err := f.callErr(tt.fn)
		require.Error(t, err, tt.fn)
		assert.Equal(t, tt.want, errMessage(t, err), tt.fn)
	}

	m, err := syntax.ParseModule("t.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	for _, c := range code.Children {
		c.Source = nil
	}
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	g := &moduleFixture{t: t, r: r, env: env}
	for _, tt := range []struct{ fn, want string }{
		{"member", "TypeError: undefined is not a function"},
		{"spread", "TypeError: 1 is not a function"},
		{"arrowCtor", "TypeError: function A() { [native code] } is not a constructor"},
	} {
		_, err := g.callErr(tt.fn)
		require.Error(t, err, tt.fn)
		assert.Equal(t, tt.want, errMessage(t, err), tt.fn)
	}
}

func TestInterpLogicalAndOptional(t *testing.T) {
	f := evalModule(t, `
export function or(a, b) { return a || b; }
export function and(a, b) { return a && b; }
export function nullish(a, b) { return a ?? b; }
export function chain(o) { return o?.a?.b; }
export function chainCall(o) { return o?.f?.(); }
export function chainElem(o, k) { return o?.[k]; }
export function chainMethod(o) { return o.inner?.m(); }
export function chainDelete(o) { return delete o?.a; }
export function chainLong(o) { return o?.a.b.c; }
export function count() { let n = 0; const f = () => { n++; return null; }; f()?.x; return n; }
export function shortCircuitArgs() { let n = 0; const o = null; o?.f(n++); return n; }
export function nested(o) { return (o?.a).b; }
`)
	assert.Equal(t, int64(2), f.call("or", 0, 2))
	assert.Equal(t, int64(1), f.call("or", 1, 2))
	assert.Equal(t, int64(0), f.call("and", 0, 2))
	assert.Equal(t, int64(2), f.call("and", 1, 2))
	assert.Equal(t, int64(0), f.call("nullish", 0, 2))
	assert.Equal(t, int64(2), f.call("nullish", nil, 2))
	assert.Equal(t, nil, f.call("chain", nil))
	assert.Equal(t, nil, f.call("chain", map[string]any{}))
	assert.Equal(t, int64(7), f.call("chain", map[string]any{"a": map[string]any{"b": 7}}))
	assert.Equal(t, nil, f.call("chainCall", nil))
	assert.Equal(t, nil, f.call("chainCall", map[string]any{}))
	assert.Equal(t, nil, f.call("chainElem", nil, "k"))
	assert.Equal(t, int64(3), f.call("chainElem", map[string]any{"k": 3}, "k"))
	assert.Equal(t, nil, f.call("chainMethod", map[string]any{}))
	assert.Equal(t, true, f.call("chainDelete", nil))
	assert.Equal(t, true, f.call("chainDelete", map[string]any{"a": 1}))
	assert.Equal(t, nil, f.call("chainLong", nil))
	assert.Equal(t, int64(1), f.call("count"))
	assert.Equal(t, int64(0), f.call("shortCircuitArgs"))
	_, err := f.callErr("nested", nil)
	assert.Equal(t, "TypeError: Cannot read properties of undefined (reading 'b')", errMessage(t, err))
	_, err = f.callErr("chainLong", map[string]any{"a": map[string]any{}})
	assert.Equal(t, "TypeError: Cannot read properties of undefined (reading 'c')", errMessage(t, err))
}

func TestInterpOperatorsOnObjects(t *testing.T) {
	f := evalModule(t, `
export function del() { const o = { a: 1, b: 2 }; const r1 = delete o.a; const r2 = delete o["b"]; const r3 = delete o.zz; return [r1, r2, r3, "a" in o, "b" in o]; }
export function delArr() { const a = [1, 2, 3]; delete a[1]; return [a.length, a[1], 1 in a]; }
export function delNonConfig() { const a = [1]; try { delete a.length; return "no"; } catch (e) { return e.constructor === TypeError; } }
export function inOp() { return ["length" in [], "toString" in {}, 0 in [1], 1 in [1], "x" in { x: undefined }]; }
export function inErr() { try { return "a" in "abc"; } catch (e) { return e.constructor === TypeError; } }
export function inst() { function F() {} const f = new F(); return [f instanceof F, f instanceof Object, [] instanceof Array, {} instanceof Array, null instanceof Object]; }
export function instErr() { try { return 1 instanceof 2; } catch (e) { return e.constructor === TypeError; } }
export function typeofs() { function g() {} return [typeof g, typeof new F(), typeof F]; function F() {} }
`)
	assert.Equal(t, []any{true, true, true, false, false}, f.call("del"))
	assert.Equal(t, []any{int64(3), nil, false}, f.call("delArr"))
	assert.Equal(t, true, f.call("delNonConfig"))
	assert.Equal(t, []any{true, true, true, false, true}, f.call("inOp"))
	assert.Equal(t, true, f.call("inErr"))
	assert.Equal(t, []any{true, true, true, false, false}, f.call("inst"))
	assert.Equal(t, true, f.call("instErr"))
	assert.Equal(t, []any{"function", "object", "function"}, f.call("typeofs"))
}

func TestInterpClosures(t *testing.T) {
	f := evalModule(t, `
export function counter() { let n = 0; return { inc: () => ++n, get: () => n }; }
export function run() { const c = counter(); c.inc(); c.inc(); return c.get(); }
export function shared() { let x = 1; const set = v => { x = v; }; const get = () => x; set(5); return get(); }
export function loopLet() { const fs = []; for (let i = 0; i < 3; i++) { fs[fs.length] = () => i; } return [fs[0](), fs[1](), fs[2]()]; }
export function loopVar() { const fs = []; for (var i = 0; i < 3; i++) { fs[fs.length] = () => i; } return [fs[0](), fs[1](), fs[2]()]; }
export function loopOf() { const fs = []; for (const v of [10, 20]) { fs[fs.length] = () => v; } return [fs[0](), fs[1]()]; }
export function loopClosureUpdate() { const fs = []; for (let i = 0; i < 2; i++) { fs[fs.length] = () => i; i += 0; } return [fs[0](), fs[1]()]; }
export function nestedDeep() { let a = 1; return (() => { let b = 2; return (() => { let c = 3; return () => a + b + c; })(); })()(); }
export function blockScope() { let x = 1; { let x = 2; } return x; }
export function blockCapture() { const fs = []; { let y = 5; fs[0] = () => y; y = 6; } return fs[0](); }
export function params(a, b) { return () => a + b; }
export function paramMut(a) { const g = () => a; a = 9; return g(); }
export function selfRef() { const f = function fact(n) { return n <= 1 ? 1 : n * fact(n - 1); }; return f(5); }
export function hoisted() { return inner(); function inner() { return 42; } }
export function hoistedBlock() { { function h() { return 7; } return h(); } }
export function recursiveClosure() { function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); } return fib(15); }
export function argsMissing(a, b) { return [a, b, typeof b]; }
export function extraArgs(a) { return a; }
export function restArgs(a, ...rest) { return [a, rest.length, rest]; }
export function restCaptured(...rest) { return () => rest.length; }
export function fnLength() { return [((a, b) => 0).length, ((a, b = 1, c) => 0).length, ((...r) => 0).length]; }
export function fnName() { function named() {} const anon = () => {}; const o = { m() {}, ["c" + "k"]: () => {} }; return [named.name, anon.name, o.m.name, o.ck.name]; }
export function protoProp() { function F() {} return [typeof F.prototype, F.prototype.constructor === F, (() => {}).prototype]; }
`)
	assert.Equal(t, int64(2), f.call("run"))
	assert.Equal(t, int64(5), f.call("shared"))
	assert.Equal(t, []any{int64(0), int64(1), int64(2)}, f.call("loopLet"))
	assert.Equal(t, []any{int64(3), int64(3), int64(3)}, f.call("loopVar"))
	assert.Equal(t, []any{int64(10), int64(20)}, f.call("loopOf"))
	assert.Equal(t, []any{int64(0), int64(1)}, f.call("loopClosureUpdate"))
	assert.Equal(t, int64(6), f.call("nestedDeep"))
	assert.Equal(t, int64(1), f.call("blockScope"))
	assert.Equal(t, int64(6), f.call("blockCapture"))
	assert.Equal(t, int64(9), f.call("paramMut"))
	assert.Equal(t, int64(120), f.call("selfRef"))
	assert.Equal(t, int64(42), f.call("hoisted"))
	assert.Equal(t, int64(7), f.call("hoistedBlock"))
	assert.Equal(t, int64(610), f.call("recursiveClosure"))
	assert.Equal(t, []any{int64(1), nil, "undefined"}, f.call("argsMissing", 1))
	assert.Equal(t, int64(1), f.call("extraArgs", 1, 2, 3))
	assert.Equal(t, []any{int64(1), int64(2), []any{int64(2), int64(3)}}, f.call("restArgs", 1, 2, 3))
	assert.Equal(t, []any{int64(1), int64(0), []any{}}, f.call("restArgs", 1))
	assert.Equal(t, []any{int64(2), int64(1), int64(0)}, f.call("fnLength"))
	assert.Equal(t, []any{"named", "anon", "m", "ck"}, f.call("fnName"))
	assert.Equal(t, []any{"object", true, nil}, f.call("protoProp"))

	adder, err := f.callErr("params", 2, 3)
	require.NoError(t, err)
	res, err := f.r.Call(ObjectValue(adder.(*Object)), Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, int64(5), f.r.ToGo(res))
	lenFn, err := f.callErr("restCaptured", 1, 2, 3, 4)
	require.NoError(t, err)
	res, err = f.r.Call(ObjectValue(lenFn.(*Object)), Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, int64(4), f.r.ToGo(res))
}

func TestInterpTDZAndConst(t *testing.T) {
	f := evalModule(t, `
export function tdzRead() { try { return x; } catch (e) { return e.constructor === ReferenceError ? e.message : "wrong"; } let x = 1; }
export function tdzClosure() { const g = () => y; try { return g(); } catch (e) { return e.message; } let y = 1; }
export function tdzAfter() { const g = () => y; let y = 1; return g(); }
export function tdzTypeof() { try { return typeof z; } catch (e) { return e.constructor === ReferenceError; } let z = 1; }
export function tdzSwitch(v) { switch (v) { case 1: let s = 5; return s; case 2: try { return s; } catch (e) { return e.message; } } }
export function tdzForOf() { try { for (const q of q) {} } catch (e) { return e.message; } }
export function tdzAssign() { try { w = 2; } catch (e) { return e.message; } let w = 1; }
export function tdzConstAssign() { try { c = 2; } catch (e) { return e.constructor === ReferenceError ? e.message : "wrong"; } const c = 1; }
export function constAssign() { const c = 1; try { c = 2; } catch (e) { return e.message; } return c; }
export function constCompound() { const c = 1; try { c += 2; } catch (e) { return e.message; } }
export function constUpdate() { const c = 1; try { c++; } catch (e) { return e.message; } }
export function constDestructure() { const c = 1; try { [c] = [2]; } catch (e) { return e.message; } }
export function fnNameAssign() { const f = function g() { try { g = 1; } catch (e) { return e.message; } }; return f(); }
export function letUndef() { let u; return typeof u; }
export function constForOf() { let n = 0; for (const v of [1, 2]) { n += v; } return n; }
export function tdzModule() { return early; }
export const early = tdzModuleProbe();
function tdzModuleProbe() { try { return late; } catch (e) { return e.message; } }
export const late = 1;
`)
	assert.Equal(t, "Cannot access 'x' before initialization", f.call("tdzRead"))
	assert.Equal(t, "Cannot access 'y' before initialization", f.call("tdzClosure"))
	assert.Equal(t, int64(1), f.call("tdzAfter"))
	assert.Equal(t, true, f.call("tdzTypeof"))
	assert.Equal(t, int64(5), f.call("tdzSwitch", 1))
	assert.Equal(t, "Cannot access 's' before initialization", f.call("tdzSwitch", 2))
	assert.Equal(t, "Cannot access 'q' before initialization", f.call("tdzForOf"))
	assert.Equal(t, "Cannot access 'w' before initialization", f.call("tdzAssign"))
	assert.Equal(t, "Cannot access 'c' before initialization", f.call("tdzConstAssign"))
	assert.Equal(t, "Assignment to constant variable.", f.call("constAssign"))
	assert.Equal(t, "Assignment to constant variable.", f.call("constCompound"))
	assert.Equal(t, "Assignment to constant variable.", f.call("constUpdate"))
	assert.Equal(t, "Assignment to constant variable.", f.call("constDestructure"))
	assert.Equal(t, "Assignment to constant variable.", f.call("fnNameAssign"))
	assert.Equal(t, "undefined", f.call("letUndef"))
	assert.Equal(t, int64(3), f.call("constForOf"))
	assert.Equal(t, "Cannot access 'late' before initialization", f.call("tdzModule"))
}

func TestInterpParamScope(t *testing.T) {
	f := evalModule(t, `
const x = "outside";
function msg(g) { try { g(); return "no error"; } catch (e) { return e.constructor === ReferenceError ? e.message : "wrong"; } }
export function refLater() { return msg(function () { (function (a = b, b) {})(); }); }
export function refSelf() { return msg(function () { ((a = a) => a)(); }); }
export function refLaterClosure() { return msg(function () { (function (g = () => b, b = g()) {})(); }); }
export function refLaterComputed() { return msg(function () { (function ({ [b]: a }, b) {})({}); }); }
export function refRest() { return msg(function () { (function (a = r, ...r) {})(); }); }
export function refMethod() { return msg(function () { ({ m(a = b, b) {} }).m(); }); }
export function assignLater() { return msg(function () { (function (a = (b = 1), b) {})(); }); }
export function refPrior(a, b = a, c = () => b) { return [a, b, c()]; }
export function laterClosure() { return (function (g = () => b, b) { return g(); })(undefined, 5); }
export function varOpen() {
  let probeParams, probeBody;
  (function (_ = probeParams = function () { return x; }) { var x = "inside"; probeBody = function () { return x; }; })();
  return [probeParams(), probeBody()];
}
export function varCopy(a, g = () => a) { var a; const first = a; a = 2; return [first, a, g()]; }
export function varDefault(a = 1) { var a; return a; }
export function funcShadow(x = 1) { function x() {} return typeof x; }
export function bodyFuncHidden() { return msg(function () { (function (a = g) { function g() {} })(); }); }
export function selfName() { return (function f(a = f) { var f = 1; return [typeof a, f]; })(); }
export function bodyLexical(a = 1, b = () => a) { let c = 5; const d = () => c; return [b(), d(), arguments.length]; }
export function sharedParam(a, b = () => a) { a = 9; return b(); }
`)
	for _, name := range []string{"refLater", "refLaterClosure", "refLaterComputed", "assignLater", "refMethod"} {
		assert.Equal(t, "Cannot access 'b' before initialization", f.call(name), name)
	}
	assert.Equal(t, "Cannot access 'a' before initialization", f.call("refSelf"))
	assert.Equal(t, "Cannot access 'r' before initialization", f.call("refRest"))
	assert.Equal(t, []any{int64(1), int64(1), int64(1)}, f.call("refPrior", 1))
	assert.Equal(t, int64(5), f.call("laterClosure"))
	assert.Equal(t, []any{"outside", "inside"}, f.call("varOpen"))
	assert.Equal(t, []any{int64(4), int64(2), int64(4)}, f.call("varCopy", 4))
	assert.Equal(t, int64(1), f.call("varDefault"))
	assert.Equal(t, "function", f.call("funcShadow"))
	assert.Equal(t, "g is not defined", f.call("bodyFuncHidden"))
	assert.Equal(t, []any{"function", int64(1)}, f.call("selfName"))
	assert.Equal(t, []any{int64(1), int64(5), int64(0)}, f.call("bodyLexical"))
	assert.Equal(t, int64(9), f.call("sharedParam", 1))
}

func TestInterpDestructuringAndSpread(t *testing.T) {
	f := evalModule(t, `
export function obj({ a, b: bb = 2, ...rest }) { return [a, bb, rest]; }
export function arr([x, , y = 9, ...rest]) { return [x, y, rest]; }
export function arrUndef() { return arr([1, 2, undefined, 4, 5]); }
export function nested({ p: { q: [, r] } }) { return r; }
export function computed(k) { const { [k]: v } = { dyn: 1 }; return v; }
export function assignPattern() { let a, b; [a, b] = [1, 2]; ({ a, b } = { a: 3, b: 4 }); return [a, b]; }
export function swap() { let a = 1, b = 2; [a, b] = [b, a]; return [a, b]; }
export function memberTarget() { const o = {}; [o.x, o["y"]] = [1, 2]; ({ z: o.z } = { z: 3 }); return o; }
export function defaultsEval() { let n = 0; const [a = n++, b = n++] = [1]; return [a, b, n]; }
export function stringPattern() { const [a, b] = "hi"; return a + b; }
export function paramsDefault(a, b = a + 1, { c } = { c: a + b }) { return [a, b, c]; }
export function undefinedDefault(a = 5) { return a; }
export function nullNoDefault(a = 5) { return a; }
export function spreadCall() { function sum(...n) { let s = 0; for (const x of n) s += x; return s; } return sum(...[1, 2], 3, ...[4]); }
export function spreadArray() { const a = [1, 2]; return [0, ...a, ...'ab', 3]; }
export function spreadObj() { const base = { a: 1, b: 2 }; return { ...base, b: 3, ...null, ...undefined, c: 4 }; }
export function spreadNew() { function P(a, b) { this.s = a + b; } return new P(...[1, 2]).s; }
export function holes() { const a = [1, , 3]; return [a.length, 1 in a, a[1]]; }
export function restObjExcludes() { const { a, ...r } = { a: 1, b: 2, c: 3 }; return r; }
export function iterNonIterable() { try { const [x] = 1; } catch (e) { return e.message; } }
export function destructNull() { try { const { x } = null; } catch (e) { return e.constructor === TypeError; } }
export function restParamPattern(...[a, b]) { return a + b; }
export function forOfPattern() { let out = ""; for (const [k, v] of [["a", 1], ["b", 2]]) out += k + v; return out; }
export function forOfObjPattern() { let out = 0; for (const { n } of [{ n: 1 }, { n: 2 }]) out += n; return out; }
export function catchPattern() { try { throw { code: 7 }; } catch ({ code }) { return code; } }
`)
	assert.Equal(t, []any{int64(1), int64(2), map[string]any{"c": int64(3)}}, f.call("obj", map[string]any{"a": 1, "c": 3}))
	assert.Equal(t, []any{int64(1), int64(9), []any{int64(4), int64(5)}}, f.call("arrUndef"))
	assert.Equal(t, []any{int64(1), nil, []any{int64(4), int64(5)}}, f.call("arr", []any{1, 2, nil, 4, 5}), "null does not trigger defaults")
	assert.Equal(t, int64(2), f.call("nested", map[string]any{"p": map[string]any{"q": []any{1, 2}}}))
	assert.Equal(t, int64(1), f.call("computed", "dyn"))
	assert.Equal(t, []any{int64(3), int64(4)}, f.call("assignPattern"))
	assert.Equal(t, []any{int64(2), int64(1)}, f.call("swap"))
	assert.Equal(t, map[string]any{"x": int64(1), "y": int64(2), "z": int64(3)}, f.call("memberTarget"))
	assert.Equal(t, []any{int64(1), int64(0), int64(1)}, f.call("defaultsEval"))
	assert.Equal(t, "hi", f.call("stringPattern"))
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, f.call("paramsDefault", 1))
	assert.Equal(t, int64(5), f.call("undefinedDefault"))
	assert.Equal(t, nil, f.call("nullNoDefault", nil))
	assert.Equal(t, int64(10), f.call("spreadCall"))
	assert.Equal(t, []any{int64(0), int64(1), int64(2), "a", "b", int64(3)}, f.call("spreadArray"))
	assert.Equal(t, map[string]any{"a": int64(1), "b": int64(3), "c": int64(4)}, f.call("spreadObj"))
	assert.Equal(t, int64(3), f.call("spreadNew"))
	assert.Equal(t, []any{int64(3), false, nil}, f.call("holes"))
	assert.Equal(t, map[string]any{"b": int64(2), "c": int64(3)}, f.call("restObjExcludes"))
	assert.Equal(t, "1 is not iterable", f.call("iterNonIterable"))
	assert.Equal(t, true, f.call("destructNull"))
	assert.Equal(t, int64(3), f.call("restParamPattern", 1, 2))
	assert.Equal(t, "a1b2", f.call("forOfPattern"))
	assert.Equal(t, int64(3), f.call("forOfObjPattern"))
	assert.Equal(t, int64(7), f.call("catchPattern"))
}

func TestInterpLoops(t *testing.T) {
	f := evalModule(t, `
export function forLoop() { let s = 0; for (let i = 0; i < 5; i++) s += i; return s; }
export function forNoInit() { let i = 0, s = 0; for (; i < 3;) { s += i; i++; } return s; }
export function forEmpty() { let n = 0; for (;;) { if (++n > 4) break; } return n; }
export function forMulti() { let out = []; for (let i = 0, j = 10; i < j; i += 3, j -= 3) out[out.length] = i + ":" + j; return out; }
export function whileLoop() { let n = 0; while (n < 10) n += 3; return n; }
export function doWhile() { let n = 0; do { n++; } while (n < 0); return n; }
export function doWhileContinue() { let n = 0, s = 0; do { n++; if (n % 2) continue; s += n; } while (n < 6); return s; }
export function forOf() { let s = ""; for (const x of ["a", "b", "c"]) s += x; return s; }
export function forOfGrow() { const a = [1, 2]; let n = 0; for (const x of a) { n++; if (a.length < 5) a[a.length] = 0; } return n; }
export function forOfShrink() { const a = [1, 2, 3, 4]; let n = 0; for (const x of a) { n++; a.length = 2; } return n; }
export function forOfHoles() { let out = []; for (const x of [1, , 3]) out[out.length] = x; return out; }
export function forOfString() { let out = []; for (const c of "a😀b") out[out.length] = c; return out; }
export function forOfBreak() { let n = 0; for (const x of [1, 2, 3, 4]) { if (x === 3) break; n += x; } return n; }
export function forOfVar() { for (var v of [1, 2]) {} return v; }
export function forOfAssign() { let v; for (v of [7]) {} return v; }
export function forOfMember() { const o = {}; for (o.k of [1, 2]) {} return o.k; }
export function forOfEmpty() { for (const x of []) return "no"; return "yes"; }
export function forIn() { const o = { b: 1, a: 2, 1: 3 }; let ks = []; for (const k in o) ks[ks.length] = k; return ks; }
export function forInProto() { const p = { inherited: 1 }; const o = Object.create ? { own: 1 } : null; return "skip"; }
export function forInDelete() { const o = { a: 1, b: 2, c: 3 }; let ks = []; for (const k in o) { ks[ks.length] = k; delete o.c; } return ks; }
export function forInNull() { let n = 0; for (const k in null) n++; for (const k in undefined) n++; return n; }
export function forInArray() { let ks = []; for (const k in [5, 6]) ks[ks.length] = k; return ks; }
export function forInString() { let ks = []; for (const k in "ab") ks[ks.length] = k; return ks; }
export function forInVar() { for (var k in { z: 1 }) {} return k; }
export function labelled() { let n = 0; outer: for (let i = 0; i < 3; i++) { for (let j = 0; j < 3; j++) { if (j === 1) continue outer; if (i === 2) break outer; n++; } } return n; }
export function labelledBlock() { let n = 0; blk: { n = 1; if (n) break blk; n = 2; } return n; }
export function labelledWhile() { let n = 0; a: while (true) { b: while (true) { n++; if (n > 2) break a; continue b; } } return n; }
export function nestedBreak() { let out = []; for (let i = 0; i < 3; i++) { for (let j = 0; j < 3; j++) { if (j === 2) break; out[out.length] = i * 10 + j; } } return out; }
export function switchBasic(v) { switch (v) { case 1: return "one"; case "1": return "str"; default: return "d"; } }
export function switchFall(v) { let out = ""; switch (v) { case 1: out += "a"; case 2: out += "b"; break; case 3: out += "c"; default: out += "d"; } return out; }
export function switchDefaultMiddle(v) { let out = ""; switch (v) { case 1: out += "a"; break; default: out += "d"; case 2: out += "b"; } return out; }
export function switchNoMatch(v) { switch (v) { case 1: return 1; } return "none"; }
export function switchLexical(v) { switch (v) { case 1: { let x = "in"; return x; } case 2: let y = "y"; return y; } }
export function switchInLoop() { let n = 0; for (let i = 0; i < 5; i++) { switch (i) { case 2: continue; case 4: break; default: n++; } } return n; }
export function switchEval() { let n = 0; const f = () => { n++; return 2; }; switch (2) { case f(): case f(): n += 10; } return n; }
`)
	assert.Equal(t, int64(10), f.call("forLoop"))
	assert.Equal(t, int64(3), f.call("forNoInit"))
	assert.Equal(t, int64(5), f.call("forEmpty"))
	assert.Equal(t, []any{"0:10", "3:7"}, f.call("forMulti"))
	assert.Equal(t, int64(12), f.call("whileLoop"))
	assert.Equal(t, int64(1), f.call("doWhile"))
	assert.Equal(t, int64(12), f.call("doWhileContinue"))
	assert.Equal(t, "abc", f.call("forOf"))
	assert.Equal(t, int64(5), f.call("forOfGrow"))
	assert.Equal(t, int64(2), f.call("forOfShrink"))
	assert.Equal(t, []any{int64(1), nil, int64(3)}, f.call("forOfHoles"))
	assert.Equal(t, []any{"a", "😀", "b"}, f.call("forOfString"))
	assert.Equal(t, int64(3), f.call("forOfBreak"))
	assert.Equal(t, int64(2), f.call("forOfVar"))
	assert.Equal(t, int64(7), f.call("forOfAssign"))
	assert.Equal(t, int64(2), f.call("forOfMember"))
	assert.Equal(t, "yes", f.call("forOfEmpty"))
	assert.Equal(t, []any{"1", "b", "a"}, f.call("forIn"))
	assert.Equal(t, []any{"a", "b"}, f.call("forInDelete"))
	assert.Equal(t, int64(0), f.call("forInNull"))
	assert.Equal(t, []any{"0", "1"}, f.call("forInArray"))
	assert.Equal(t, []any{"0", "1"}, f.call("forInString"))
	assert.Equal(t, "z", f.call("forInVar"))
	assert.Equal(t, int64(2), f.call("labelled"))
	assert.Equal(t, int64(1), f.call("labelledBlock"))
	assert.Equal(t, int64(3), f.call("labelledWhile"))
	assert.Equal(t, []any{int64(0), int64(1), int64(10), int64(11), int64(20), int64(21)}, f.call("nestedBreak"))
	assert.Equal(t, "one", f.call("switchBasic", 1))
	assert.Equal(t, "str", f.call("switchBasic", "1"))
	assert.Equal(t, "d", f.call("switchBasic", 2))
	assert.Equal(t, "ab", f.call("switchFall", 1))
	assert.Equal(t, "b", f.call("switchFall", 2))
	assert.Equal(t, "cd", f.call("switchFall", 3))
	assert.Equal(t, "d", f.call("switchFall", 9))
	assert.Equal(t, "a", f.call("switchDefaultMiddle", 1))
	assert.Equal(t, "b", f.call("switchDefaultMiddle", 2))
	assert.Equal(t, "db", f.call("switchDefaultMiddle", 3))
	assert.Equal(t, "none", f.call("switchNoMatch", 2))
	assert.Equal(t, "in", f.call("switchLexical", 1))
	assert.Equal(t, "y", f.call("switchLexical", 2))
	assert.Equal(t, int64(3), f.call("switchInLoop"))
	assert.Equal(t, int64(11), f.call("switchEval"))
}

func TestInterpTryCatchFinally(t *testing.T) {
	f := evalModule(t, `
let log;
export function getLog() { return log; }
function reset() { log = []; }
function push(x) { log[log.length] = x; }
export function catchOnly() { try { throw new Error("e1"); } catch (e) { return e.message; } }
export function catchNoParam() { try { throw 1; } catch { return "caught"; } }
export function finallyNormal() { reset(); try { push("try"); } finally { push("fin"); } push("after"); return log; }
export function finallyReturnOverride() { try { return "try"; } finally { return "fin"; } }
export function finallyThrowOverride() { try { throw new Error("try"); } finally { throw new Error("fin"); } }
export function finallyKeepsReturn() { reset(); try { return "try"; } finally { push("fin"); } }
export function finallyKeepsThrow() { reset(); try { try { throw new Error("t"); } finally { push("fin"); } } catch (e) { push(e.message); } return log; }
export function catchThenFinally() { reset(); try { throw new Error("t"); } catch (e) { push("c:" + e.message); } finally { push("f"); } return log; }
export function throwInCatch() { reset(); try { try { throw new Error("a"); } catch (e) { throw new Error("b"); } finally { push("f"); } } catch (e) { push(e.message); } return log; }
export function returnInCatchFinallyRuns() { reset(); try { throw 1; } catch (e) { return log[log.length] = "c"; } finally { push("f"); } }
export function breakThroughFinally() { reset(); for (let i = 0; i < 3; i++) { try { if (i === 1) break; push(i); } finally { push("f" + i); } } return log; }
export function continueThroughFinally() { reset(); for (let i = 0; i < 3; i++) { try { if (i === 1) continue; push(i); } finally { push("f" + i); } } return log; }
export function labelledBreakThroughNested() { reset(); out: for (let i = 0; i < 2; i++) { try { for (let j = 0; j < 2; j++) { try { if (j === 1) break out; push(j); } finally { push("inner" + j); } } } finally { push("outer" + i); } } return log; }
export function nestedFinallyReturn() { reset(); try { try { return 1; } finally { push("a"); } } finally { push("b"); } }
export function finallyOverridesBreak() { let n = 0; for (;;) { try { break; } finally { n++; if (n < 3) continue; } } return n; }
export function rethrow() { try { try { throw new TypeError("x"); } catch (e) { throw e; } } catch (e) { return e.constructor === TypeError && e.message === "x"; } }
export function throwString() { try { throw "str"; } catch (e) { return [typeof e, e]; } }
export function throwNumber() { try { throw 42; } catch (e) { return e; } }
export function throwObject() { try { throw { code: 1 }; } catch (e) { return e.code; } }
export function throwNull() { try { throw null; } catch (e) { return e === null; } }
export function throwUndefined() { try { throw undefined; } catch (e) { return e === undefined; } }
export function catchScope() { let e = "outer"; try { throw "inner"; } catch (e) { } return e; }
export function catchClosure() { try { throw 5; } catch (e) { return (() => e)(); } }
export function nativeErrorCaught() { try { null.x; } catch (e) { return e.constructor === TypeError; } }
export function deepUnwind() { function a() { b(); } function b() { c(); } function c() { throw new Error("deep"); } try { a(); } catch (e) { return e.message; } }
export function finallyInLoopEnv() { reset(); for (let i = 0; i < 2; i++) { const f = () => i; try { push(f()); } finally { push("f"); } } return log; }
export function uncaught() { throw new RangeError("out"); }
export function uncaughtValue() { throw "plain"; }
export function finallyValueWithoutReturn() { try { 1; } finally { 2; } }
`)
	assert.Equal(t, "e1", f.call("catchOnly"))
	assert.Equal(t, "caught", f.call("catchNoParam"))
	assert.Equal(t, []any{"try", "fin", "after"}, f.call("finallyNormal"))
	assert.Equal(t, "fin", f.call("finallyReturnOverride"))
	_, err := f.callErr("finallyThrowOverride")
	assert.Equal(t, "Error: fin", errMessage(t, err))
	assert.Equal(t, "try", f.call("finallyKeepsReturn"))
	assert.Equal(t, []any{"fin", "t"}, f.call("finallyKeepsThrow"))
	assert.Equal(t, []any{"c:t", "f"}, f.call("catchThenFinally"))
	assert.Equal(t, []any{"f", "b"}, f.call("throwInCatch"))
	assert.Equal(t, "c", f.call("returnInCatchFinallyRuns"))
	assert.Equal(t, []any{"c", "f"}, f.call("getLog"))
	assert.Equal(t, []any{int64(0), "f0", "f1"}, f.call("breakThroughFinally"))
	assert.Equal(t, []any{int64(0), "f0", "f1", int64(2), "f2"}, f.call("continueThroughFinally"))
	assert.Equal(t, []any{int64(0), "inner0", "inner1", "outer0"}, f.call("labelledBreakThroughNested"))
	assert.Equal(t, int64(1), f.call("nestedFinallyReturn"))
	assert.Equal(t, []any{"a", "b"}, f.call("getLog"))
	assert.Equal(t, int64(3), f.call("finallyOverridesBreak"))
	assert.Equal(t, true, f.call("rethrow"))
	assert.Equal(t, []any{"string", "str"}, f.call("throwString"))
	assert.Equal(t, int64(42), f.call("throwNumber"))
	assert.Equal(t, int64(1), f.call("throwObject"))
	assert.Equal(t, true, f.call("throwNull"))
	assert.Equal(t, true, f.call("throwUndefined"))
	assert.Equal(t, "outer", f.call("catchScope"))
	assert.Equal(t, int64(5), f.call("catchClosure"))
	assert.Equal(t, true, f.call("nativeErrorCaught"))
	assert.Equal(t, "deep", f.call("deepUnwind"))
	assert.Equal(t, []any{int64(0), "f", int64(1), "f"}, f.call("finallyInLoopEnv"))
	assert.Equal(t, nil, f.call("finallyValueWithoutReturn"))

	_, err = f.callErr("uncaught")
	assert.Equal(t, "RangeError: out", errMessage(t, err))
	_, err = f.callErr("uncaughtValue")
	var exc *Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "plain", f.r.ToGo(exc.Value))
}

func TestInterpTemplatesAndStrings(t *testing.T) {
	f := evalModule(t, `
export function plain() { return `+"`plain`"+`; }
export function subst(a, b) { return `+"`${a} + ${b} = ${a + b}`"+`; }
export function nonASCII(n) { return `+"`héllo ${n} 😀 ${'ü'}`"+`; }
export function leadingExpr(x) { return `+"`${x}!`"+`; }
export function empty() { return `+"``"+`; }
export function nestedTpl(x) { return `+"`a${`b${x}c`}d`"+`; }
export function objToString() { return `+"`${{ toString() { return 'T'; } }}`"+`; }
export function long() { let s = ""; for (let i = 0; i < 100; i++) s += "xy"; return s.length; }
export function lone() { const s = "\uD800x"; return [s.length, s.charCodeAt ? "has" : "no"]; }
export function concatMixed() { return "a" + 1 + 2 + "b" + (1 + 2); }
export function escapes() { return "tab\tnl\nq\"A\x42\u{1F600}"; }
export function unicodeLiteral() { return "日本語"; }
export function surrogatePair() { return "😀".length; }
export function indexNonASCII() { return "日本語"[1]; }
`)
	assert.Equal(t, "plain", f.call("plain"))
	assert.Equal(t, "1 + 2 = 3", f.call("subst", 1, 2))
	assert.Equal(t, "héllo 5 😀 ü", f.call("nonASCII", 5))
	assert.Equal(t, "x!", f.call("leadingExpr", "x"))
	assert.Equal(t, "", f.call("empty"))
	assert.Equal(t, "abXcd", f.call("nestedTpl", "X"))
	assert.Equal(t, "T", f.call("objToString"))
	assert.Equal(t, int64(200), f.call("long"))
	assert.Equal(t, []any{int64(2), "has"}, f.call("lone"))
	assert.Equal(t, "a12b3", f.call("concatMixed"))
	assert.Equal(t, "tab\tnl\nq\"AB😀", f.call("escapes"))
	assert.Equal(t, "日本語", f.call("unicodeLiteral"))
	assert.Equal(t, int64(2), f.call("surrogatePair"))
	assert.Equal(t, "本", f.call("indexNonASCII"))
}

func TestInterpObjectsAndThis(t *testing.T) {
	f := evalModule(t, `
export const topThis = this;
export const arrowTopThis = (() => this)();
export function fnThis() { return this; }
export function methodThis() { const o = { v: 7, m() { return this.v; }, arrow: () => this }; return [o.m(), o.arrow()]; }
export function arrowInMethod() { const o = { v: 8, m() { return (() => this.v)(); } }; return o.m(); }
export function callWithThis() { function g() { return this === undefined ? "undef" : this.tag; } const o = { tag: "o", g }; return [g(), o.g()]; }
export function computedKeys() { const k = "a" + "b"; const o = { [k]: 1, ["x" + 1]: 2, [3 + 4]: 3, 10: 4, "s p": 5, 1.5: 6 }; return [o.ab, o.x1, o[7], o["10"], o["s p"], o["1.5"]]; }
export function protoLiteral() { const p = { inherited: 1 }; const o = { __proto__: p, own: 2 }; return [o.inherited, o.own, Object.getPrototypeOf ? "skip" : "skip", "__proto__" in o && !o.hasOwnProperty ? "n/a" : "ok"]; }
export function protoLiteralNull() { const o = { __proto__: null, a: 1 }; return [o.a, "toString" in o]; }
export function protoLiteralNonObject() { const o = { __proto__: 5, a: 1 }; return "toString" in o; }
export function protoComputedIsOwn() { const o = { ["__proto__"]: 5 }; return typeof o.toString; }
export function shorthand() { const a = 1, b = 2; return { a, b }; }
export function methodShorthand() { const o = { add(x, y) { return x + y; }, "quoted"() { return 1; }, 42() { return 2; } }; return [o.add(1, 2), o.quoted(), o[42]()]; }
export function methodNotCtor() { const o = { m() {} }; try { new o.m(); } catch (e) { return e.constructor === TypeError; } }
export function arrowNotCtor() { const a = () => {}; try { new a(); } catch (e) { return e.constructor === TypeError; } }
export function ctor() { function P(x) { this.x = x; } P.prototype.get = function () { return this.x; }; const p = new P(3); return [p.get(), p instanceof P, p.constructor === P]; }
export function ctorReturnObject() { function P() { return { alt: true }; } return new P().alt; }
export function ctorReturnPrimitive() { function P() { this.a = 1; return 5; } return new P().a; }
export function newNoArgs() { function P() { this.n = arguments0(); } function arguments0() { return 0; } return new P().n; }
export function duplicateKeys() { const o = { a: 1, a: 2 }; return o.a; }
export function numericKeyOrder() { const o = { b: 1, 2: 1, a: 1, 1: 1 }; let ks = ""; for (const k in o) ks += k; return ks; }
export function nestedLiteral() { return { a: { b: [1, { c: "d" }] } }; }
export function assignChain() { let a, b; a = b = 5; return [a, b]; }
export function assignValue() { const o = {}; const v = (o.x = 3); return [v, o.x]; }
export function compoundMember() { const o = { n: 1, s: "a" }; o.n += 2; o.s += "b"; o["n"] *= 10; o.n **= 2; return [o.n, o.s]; }
export function logicalAssign() { const o = { a: 0, b: null, c: 1 }; o.a ||= 5; o.b ??= 6; o.c &&= 7; o.d ??= 8; let x = 0; x ||= 9; let y = 1; y &&= 2; let z = null; z ??= 3; return [o.a, o.b, o.c, o.d, x, y, z]; }
export function logicalAssignShortCircuit() { let n = 0; let a = 1; a ||= n++; let b = 0; b &&= n++; let c = 1; c ??= n++; return [n, a, b, c]; }
export function updateOps() { let i = 1; const a = i++; const b = ++i; const c = i--; const d = --i; return [i, a, b, c, d]; }
export function updateMember() { const o = { n: 5 }; const a = o.n++; const b = ++o.n; const arr = [1]; arr[0]++; return [a, b, o.n, arr[0]]; }
export function updateString() { let s = "5"; s++; let t = "x"; t++; return [s, t !== t]; }
export function seqAssign() { let a = 1; const r = (a = 2, a + 1); return [a, r]; }
export function arrayLength() { const a = [1, 2, 3]; a.length = 1; a[3] = 4; return [a.length, a[1], a[3]]; }
export function frozenWrite() { const o = Object.freeze ? "skip" : "skip"; return o; }
export function globalRead() { return typeof Object; }
export function globalWrite() { try { undeclaredGlobal = 1; } catch (e) { return e.constructor === ReferenceError; } }
export function globalThisProp() { return globalThis.Array === Array; }
export function stringWrapperMethod() { return "abc".toString(); }
export function numberMethod() { return (255).toString(16); }
export function elemStringKeys() { const o = {}; o["k"] = 1; o[1] = 2; o[1.5] = 3; o[true] = 4; o[null] = 5; return [o.k, o["1"], o["1.5"], o["true"], o["null"]]; }
export function elemSymbolLike() { const a = []; a["length"] = 2; return a.length; }
export function negZeroKey() { const o = {}; o[-0] = 1; const p = { [-0]: 2 }; return [o[0], p["0"], Object.keys(o).length]; }
export function enumeratedIndexKeys() { const a = [10, 20], o = { 1: "x" }, r = []; for (const k in a) r.push(a[k]); for (const k in o) r.push(o[k]); for (const k of Object.keys(a)) r.push(a[k]); return r; }
export function protoNotNamed() { const o = { __proto__: function () {} }, p = { __proto__() {} }; return [Object.getPrototypeOf(o).name, p.__proto__.name]; }
export function parenOptionalCallThis() { const o = { v: 7, m() { return this === o ? this.v : "bad"; }, i: { m() { return this; } } }; return [(o?.m)(), (o.i?.m)() === o.i, (o?.i.m)() === o.i, (o?.["m"])()]; }
export function computedKeyNames() { const k = "x"; const o = { [k]: function () {}, ["y"]: () => {}, [1]: function () {}, ["z"]: function named() {}, ["w"]: (0, function () {}) }; return [o.x.name, o.y.name, o[1].name, o.z.name, o.w.name, Object.keys(o).join()]; }
export function parenAssignNotNamed() { let a, b, c = null, d; (a) = function () {}; ((b)) = () => {}; (c) ??= function () {}; d = function () {}; return [a.name, b.name, c.name, d.name]; }
export function parenOptionalCallShort() { const o = null; try { (o?.m)(); } catch (e) { return e.constructor === TypeError; } }
`)
	assert.Equal(t, nil, f.export("topThis"))
	assert.Equal(t, nil, f.export("arrowTopThis"))
	assert.Equal(t, nil, f.call("fnThis"))
	assert.Equal(t, []any{int64(7), nil}, f.call("methodThis"))
	assert.Equal(t, int64(8), f.call("arrowInMethod"))
	assert.Equal(t, []any{"undef", "o"}, f.call("callWithThis"))
	assert.Equal(t, []any{int64(1), int64(2), int64(3), int64(4), int64(5), int64(6)}, f.call("computedKeys"))
	assert.Equal(t, []any{int64(1), int64(2), "skip", "ok"}, f.call("protoLiteral"))
	assert.Equal(t, []any{int64(1), false}, f.call("protoLiteralNull"))
	assert.Equal(t, true, f.call("protoLiteralNonObject"))
	assert.Equal(t, "function", f.call("protoComputedIsOwn"))
	assert.Equal(t, map[string]any{"a": int64(1), "b": int64(2)}, f.call("shorthand"))
	assert.Equal(t, []any{int64(3), int64(1), int64(2)}, f.call("methodShorthand"))
	assert.Equal(t, true, f.call("methodNotCtor"))
	assert.Equal(t, true, f.call("arrowNotCtor"))
	assert.Equal(t, []any{int64(3), true, true}, f.call("ctor"))
	assert.Equal(t, true, f.call("ctorReturnObject"))
	assert.Equal(t, int64(1), f.call("ctorReturnPrimitive"))
	assert.Equal(t, int64(0), f.call("newNoArgs"))
	assert.Equal(t, int64(2), f.call("duplicateKeys"))
	assert.Equal(t, "12ba", f.call("numericKeyOrder"))
	assert.Equal(t, map[string]any{"a": map[string]any{"b": []any{int64(1), map[string]any{"c": "d"}}}}, f.call("nestedLiteral"))
	assert.Equal(t, []any{int64(5), int64(5)}, f.call("assignChain"))
	assert.Equal(t, []any{int64(3), int64(3)}, f.call("assignValue"))
	assert.Equal(t, []any{int64(900), "ab"}, f.call("compoundMember"))
	assert.Equal(t, []any{int64(5), int64(6), int64(7), int64(8), int64(9), int64(2), int64(3)}, f.call("logicalAssign"))
	assert.Equal(t, []any{int64(0), int64(1), int64(0), int64(1)}, f.call("logicalAssignShortCircuit"))
	assert.Equal(t, []any{int64(1), int64(1), int64(3), int64(3), int64(1)}, f.call("updateOps"))
	assert.Equal(t, []any{int64(5), int64(7), int64(7), int64(2)}, f.call("updateMember"))
	assert.Equal(t, []any{int64(6), true}, f.call("updateString"))
	assert.Equal(t, []any{int64(2), int64(3)}, f.call("seqAssign"))
	assert.Equal(t, []any{int64(4), nil, int64(4)}, f.call("arrayLength"))
	assert.Equal(t, "function", f.call("globalRead"))
	assert.Equal(t, true, f.call("globalWrite"))
	assert.Equal(t, true, f.call("globalThisProp"))
	assert.Equal(t, "abc", f.call("stringWrapperMethod"))
	assert.Equal(t, "ff", f.call("numberMethod"))
	assert.Equal(t, []any{int64(1), int64(2), int64(3), int64(4), int64(5)}, f.call("elemStringKeys"))
	assert.Equal(t, int64(2), f.call("elemSymbolLike"))
	assert.Equal(t, []any{int64(1), int64(2), int64(1)}, f.call("negZeroKey"))
	assert.Equal(t, []any{int64(10), int64(20), "x", int64(10), int64(20)}, f.call("enumeratedIndexKeys"))
	assert.Equal(t, []any{"", "__proto__"}, f.call("protoNotNamed"))
	assert.Equal(t, []any{int64(7), true, true, int64(7)}, f.call("parenOptionalCallThis"))
	assert.Equal(t, true, f.call("parenOptionalCallShort"))
	assert.Equal(t, []any{"x", "y", "1", "named", "", "1,x,y,z,w"}, f.call("computedKeyNames"))
	assert.Equal(t, []any{"", "", "", "d"}, f.call("parenAssignNotNamed"))
}

func TestInterpModuleBindings(t *testing.T) {
	f := evalModule(t, `
export let counter = 0;
export const fixed = 1;
export var v = "v";
export function bump() { counter++; v += "!"; return counter; }
let hidden = 10;
export function getHidden() { return hidden; }
export { hidden as renamed, hidden as alias2 };
export default function () { return "dflt"; }
export const order = [];
order[0] = typeof bump;
order[1] = typeof later;
function later() {}
`)
	assert.Equal(t, int64(0), f.export("counter"))
	assert.Equal(t, int64(1), f.call("bump"))
	assert.Equal(t, int64(2), f.call("bump"))
	assert.Equal(t, int64(2), f.export("counter"), "live binding reflects mutation after evaluation")
	assert.Equal(t, "v!!", f.export("v"))
	assert.Equal(t, int64(1), f.export("fixed"))
	assert.Equal(t, int64(10), f.export("renamed"))
	assert.Equal(t, int64(10), f.export("alias2"))
	assert.Equal(t, []any{"function", "function"}, f.export("order"))
	def, ok := f.env.GetBindingValue("default")
	require.True(t, ok)
	res, err := f.r.Call(def, Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, "dflt", f.r.ToGo(res))
	_, ok = f.env.GetBindingValue("hidden")
	assert.False(t, ok)

	// A module whose top level throws surfaces the error and stays usable.
	m, err := syntax.ParseModule("bad.js", `export const a = 1; throw new Error("top"); export const b = 2;`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.Error(t, err)
	assert.Equal(t, "Error: top", errMessage(t, err))
	a, _ := env.GetBindingValue("a")
	assert.Equal(t, int64(1), r.ToGo(a))
	b, _ := env.GetBindingValue("b")
	assert.True(t, b.IsHole(), "initializer never ran: b is in its TDZ")

	// export default expression.
	f2 := evalModule(t, `const x = 5; export default x * 2;`)
	assert.Equal(t, int64(10), f2.export("default"))
}

func TestInterpSharedTemplateAcrossRealms(t *testing.T) {
	m, err := syntax.ParseModule("s.js", `
export let n = 0;
export function inc(o) { n++; return o.a + o.b; }
`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	envs := make([]*ModuleEnv, 3)
	realms := make([]*Realm, 3)
	for i := range realms {
		realms[i] = NewRealmWith(RealmOptions{SharedIntrinsics: i%2 == 0})
		envs[i], err = realms[i].EvaluateModule(code)
		require.NoError(t, err)
	}
	for i, r := range realms {
		inc, _ := envs[i].GetBindingValue("inc")
		arg, _ := r.FromGo(map[string]any{"a": i, "b": 1})
		for range 3 {
			res, err := r.Call(inc, Undefined(), []Value{arg})
			require.NoError(t, err)
			assert.Equal(t, int64(i+1), r.ToGo(res))
		}
		n, _ := envs[i].GetBindingValue("n")
		assert.Equal(t, int64(3), r.ToGo(n))
	}
}

// TestInterpLazyInlineCaches: a function's inline caches are reserved on its
// first call in the realm, closures of one template share them, and the
// table never outgrows the sites of the program.
func TestInterpLazyInlineCaches(t *testing.T) {
	m, err := syntax.ParseModule("ic.js", `
export function get(o) { return o.a + o.b; }
export function unused(o) { return o.x + o.y + o.z; }
export function make() { return o => o.a * 10; }
`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	total := 0
	var walk func(f *bytecode.Function)
	walk = func(f *bytecode.Function) {
		total += int(f.ICCount)
		for _, c := range f.Children {
			walk(c)
		}
	}
	walk(code)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	assert.Len(t, r.ICSlots(), int(code.ICCount), "only the module body ran")

	export := func(name string) Value {
		v, ok := env.GetBindingValue(name)
		require.True(t, ok)
		return v
	}
	sites := func(fn Value) int { return int(fn.AsObject().FunctionData().Code().ICCount) }
	arg, err := r.FromGo(map[string]any{"a": 2, "b": 3})
	require.NoError(t, err)
	get := export("get")
	for range 2 {
		res, err := r.Call(get, Undefined(), []Value{arg})
		require.NoError(t, err)
		assert.Equal(t, IntValue(5), res)
		assert.Len(t, r.ICSlots(), int(code.ICCount)+sites(get))
	}
	before := len(r.ICSlots())
	var arrows []Value
	for range 2 {
		fn, err := r.Call(export("make"), Undefined(), nil)
		require.NoError(t, err)
		arrows = append(arrows, fn)
	}
	for _, fn := range arrows {
		res, err := r.Call(fn, Undefined(), []Value{arg})
		require.NoError(t, err)
		assert.Equal(t, IntValue(20), res)
	}
	assert.Len(t, r.ICSlots(), before+sites(export("make"))+sites(arrows[0]), "closures of one template share their caches")
	assert.Less(t, len(r.ICSlots()), total, "unused never ran")
	assert.LessOrEqual(t, cap(r.ICSlots()), total)
}

func TestInterpScript(t *testing.T) {
	run := func(src string) (any, error) {
		s, err := syntax.ParseScript("s.js", src, syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileScript(s)
		require.NoError(t, err)
		r := NewRealm()
		v, err := r.RunScript(code)
		if err != nil {
			return nil, err
		}
		return r.ToGo(v), nil
	}
	v, err := run(`let a = 2; function sq(x) { return x * x; } sq(a) + 1;`)
	require.NoError(t, err)
	assert.Equal(t, int64(5), v)
	v, err = run(`var s = ""; for (const c of "ab") s += c; s;`)
	require.NoError(t, err)
	assert.Equal(t, "ab", v)
	v, err = run(`this === globalThis;`)
	require.NoError(t, err)
	assert.Equal(t, true, v)
	v, err = run(`1; if (false) 2;`) // UpdateEmpty(C, undefined)
	require.NoError(t, err)
	assert.Nil(t, v)
	_, err = run(`throw new TypeError("script");`)
	assert.Equal(t, "TypeError: script", errMessage(t, err))
	// A lexical declaration may not shadow a non-configurable global
	// (HasRestrictedGlobalProperty); the body does not run.
	_, err = run(`globalThis.ran = 1; let undefined;`)
	assert.Equal(t, "SyntaxError: Identifier 'undefined' has already been declared", errMessage(t, err))
	_, err = run(`const NaN = 1;`)
	assert.Equal(t, "SyntaxError: Identifier 'NaN' has already been declared", errMessage(t, err))
	v, err = run(`let Object = 1, JSON = 2; Object + JSON;`)
	require.NoError(t, err, "configurable globals may be shadowed")
	assert.Equal(t, int64(3), v)
	// A function may not replace a non-configurable global that is not a
	// writable, enumerable data property, and a name the global object
	// lacks needs it extensible (CanDeclareGlobalFunction,
	// CanDeclareGlobalVar); the body does not run.
	_, err = run(`globalThis.ran = 1; function NaN() {}`)
	assert.Equal(t, "TypeError: Cannot redefine global function 'NaN'", errMessage(t, err))
	r := NewRealm()
	for _, c := range [][2]string{
		{`var NaN, JSON; function isNaN() {} Object.preventExtensions(globalThis);`, ""},
		{`var ne;`, "TypeError: Cannot define global variable 'ne', global object is not extensible"},
		{`function fe() {}`, "TypeError: Cannot redefine global function 'fe'"},
	} {
		s, err := syntax.ParseScript("s.js", c[0], syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileScript(s)
		require.NoError(t, err)
		if _, err = r.RunScript(code); c[1] == "" {
			require.NoError(t, err)
		} else {
			assert.Equal(t, c[1], errMessage(t, err))
		}
	}
}

func TestInterpCallDepth(t *testing.T) {
	f := evalModule(t, `
export function recurse() { return recurse(); }
export function depth() { let d = 0; function r() { d++; r(); } try { r(); } catch (e) { return [e.constructor === RangeError, e.message, d]; } }
export function afterOverflow() { return 1; }
`)
	_, err := f.callErr("recurse")
	assert.Equal(t, "RangeError: Maximum call stack size exceeded", errMessage(t, err))
	res := f.call("depth").([]any)
	assert.Equal(t, true, res[0])
	assert.Equal(t, "Maximum call stack size exceeded", res[1])
	assert.Equal(t, int64(MaxCallDepth-1), res[2], "the test call and depth() take one frame each; the last r() fails to enter")
	assert.Equal(t, int64(1), f.call("afterOverflow"))
	assert.Equal(t, 0, f.r.CallDepth())
	assert.Equal(t, 0, f.r.interp.nframes)
	assert.Equal(t, 0, f.r.interp.sp)
}

func TestInterpInterrupt(t *testing.T) {
	f := evalModule(t, `
export function spin(tick) { let n = 0; while (true) { n++; tick(); } }
export function spinFor(tick) { for (;;) { tick(); } }
export function ok() { return "ok"; }
export function nativeCheck(fn) { return fn(); }
`)
	r := f.r
	for _, name := range []string{"spin", "spinFor"} {
		started := make(chan struct{}, 1)
		first := true
		tick := r.NewNativeFunction(AtomEmpty, 0, func(*Realm, Value, []Value) (Value, error) {
			if first {
				first = false
				started <- struct{}{}
			}
			return Undefined(), nil
		})
		done := make(chan error, 1)
		go func() {
			_, err := f.callErr(name, tick)
			done <- err
		}()
		<-started
		r.Interrupt("stop-" + name)
		err := <-done
		var ie *InterruptedError
		require.ErrorAs(t, err, &ie)
		assert.Equal(t, "stop-"+name, ie.Value)
		assert.Equal(t, 0, r.CallDepth())
		assert.Equal(t, 0, r.interp.nframes)
		assert.Equal(t, 0, r.interp.sp)

		// While the flag is set every entry is refused; after clearing, the
		// realm works again.
		_, err = f.callErr("ok")
		require.ErrorAs(t, err, &ie)
		r.ClearInterrupt()
		assert.Equal(t, "ok", f.call("ok"))
	}

	// A native that observes the interrupt returns it unchanged.
	check := r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		r.Interrupt(7)
		return Undefined(), r.CheckInterrupt()
	})
	_, err := f.callErr("nativeCheck", check)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, 7, ie.Value)
	r.ClearInterrupt()
	assert.Equal(t, "ok", f.call("ok"))
}

// TestInterpInterruptDoWhile is a FuzzRun finding: the back-edge of a
// do-while loop was a conditional jump, which does not check for an
// interrupt, so the loop could not be stopped.
func TestInterpInterruptDoWhile(t *testing.T) {
	f := evalModule(t, `export function spin(tick) { let n = 0; do { n++; tick(); } while (n > 0); }`)
	started := make(chan struct{})
	var once sync.Once
	tick := f.r.NewNativeFunction(AtomEmpty, 0, func(*Realm, Value, []Value) (Value, error) {
		once.Do(func() { close(started) })
		return Undefined(), nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.callErr("spin", tick)
		done <- err
	}()
	<-started
	f.r.Interrupt("stop")
	select {
	case err := <-done:
		var ie *InterruptedError
		require.ErrorAs(t, err, &ie)
		assert.Equal(t, 0, f.r.CallDepth())
	case <-time.After(5 * time.Second):
		t.Fatal("the do-while loop ignores the interrupt")
	}
}

// TestInterpInterruptSkipsCatchAndFinally checks that an interrupt unwinds
// through try statements: no catch clause sees it and no finally block runs,
// also in the callers' try statements and in a try with no catch clause.
func TestInterpInterruptSkipsCatchAndFinally(t *testing.T) {
	f := evalModule(t, `
export let ticks = 0;
export let log = [];
export function spin(tick) { try { for (;;) { ticks++; tick(); } } catch (e) { log.push("caught"); return "caught"; } finally { log.push("finally"); ticks = -1; } }
export function outer(tick) { try { return spin(tick); } catch (e) { log.push("outer caught"); } finally { log.push("outer finally"); } }
function loop(tick) { for (;;) { ticks++; tick(); } }
export function bare(tick) { try { loop(tick); } finally { log.push("bare finally"); ticks = -1; } }
export function reset() { ticks = 0; }
`)
	tick := f.r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		r.Interrupt("stop")
		return Undefined(), nil
	})
	for _, name := range []string{"spin", "outer", "bare"} {
		_, err := f.callErr(name, tick)
		var ie *InterruptedError
		require.ErrorAs(t, err, &ie, name)
		assert.Equal(t, "stop", ie.Value)
		f.r.ClearInterrupt()
		assert.Equal(t, []any{}, f.export("log"), "%s: a catch or finally ran", name)
		assert.Equal(t, int64(1), f.export("ticks"), "%s: finally must not run", name)
		assert.Equal(t, 0, f.r.CallDepth())
		assert.Equal(t, 0, f.r.interp.nframes)
		assert.Equal(t, 0, f.r.interp.sp)
		f.call("reset")
	}
}

// TestInterpInterruptRace is a review finding: a check that saw the
// interrupt flag set loaded it again to build the error, so a ClearInterrupt
// in between made a call return undefined, a loop leave its function with
// undefined, or a generator's next return undefined. Every result is now the
// value or an *InterruptedError.
func TestInterpInterruptRace(t *testing.T) {
	f := evalModule(t, `function one() { return 1; }
function* gen() { yield 1; }
export function call() { return one(); }
export function loop() { let n = 0; for (let i = 0; i < 20; i++) n = one(); return n; }
export function resume() { return gen().next().value; }`)
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			f.r.Interrupt("race")
			f.r.ClearInterrupt()
		}
	}()
	defer func() { stop.Store(true); <-done }()
	for i := 0; i < 3000; i++ {
		for _, name := range []string{"call", "loop", "resume"} {
			got, err := f.callErr(name)
			if err != nil {
				var ie *InterruptedError
				require.ErrorAs(t, err, &ie, name)
				continue
			}
			require.EqualValues(t, 1, got, name)
		}
	}
}

func TestInterpErrorStack(t *testing.T) {
	src := `export function outer(x) {
  return inner(x);
}
function inner(x) {
  const e = new Error("bad " + x);
  return e.stack;
}
export function anon() { return (() => new Error("arrow").stack)(); }
export function thrown() { try { throw new TypeError("t"); } catch (e) { return e.stack; } }
export function native() { try { null.x; } catch (e) { return e.stack; } }
export const top = new Error("top").stack;
export function custom() { const e = new Error("c"); e.stack = "mine"; return e.stack; }
`
	f := evalModule(t, src)
	at := func(fn, needle string) string { return "    at " + fn + " (t.js:" + lineCol(src, needle) + ")" }
	assert.Equal(t, "Error: bad 1\n"+at("inner", `new Error("bad "`)+"\n"+at("outer", "inner(x)"), f.call("outer", 1))
	loc := func(needle string) string { return "    at t.js:" + lineCol(src, needle) }
	assert.Equal(t, "Error: arrow\n"+loc(`new Error("arrow")`)+"\n"+at("anon", "() => new"), f.call("anon"))
	assert.Equal(t, "TypeError: t\n"+at("thrown", `new TypeError("t")`), f.call("thrown"))
	assert.Equal(t, "TypeError: Cannot read properties of null (reading 'x')\n"+at("native", "null.x"), f.call("native"))
	assert.Equal(t, "Error: top\n"+loc(`new Error("top")`), f.export("top"))
	assert.Equal(t, "mine", f.call("custom"))

	// Frames captured from Go through the hook API.
	var frames []StackFrame
	probe := f.r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		frames = captureStack(r)
		return Undefined(), nil
	})
	f2 := evalModule(t, "export function a(p) { b(p); }\nfunction b(p) { p(); }")
	_, err := f2.callErr("a", probe)
	require.NoError(t, err)
	require.Len(t, frames, 3)
	assert.Same(t, probe, frames[0].Fn)
	assert.Nil(t, frames[0].Code())
	assert.Equal(t, "b", frames[1].Code().Name)
	assert.Equal(t, "a", frames[2].Code().Name)
	assert.Equal(t, "    at <anonymous>\n    at b (t.js:2:17)\n    at a (t.js:1:24)", FormatStack(f2.r, frames))
	assert.Equal(t, []int{2, 1}, []int{lineOf("export function a(p) { b(p); }\nfunction b(p) { p(); }", "p()"), lineOf("export function a(p) { b(p); }\nfunction b(p) { p(); }", "b(p)")})
	assert.Equal(t, "", FormatStack(f2.r, nil))
	assert.Equal(t, "    at <anonymous>", FormatStack(f2.r, []StackFrame{{}}))
}

// lineCol returns "line:col" (1-based) of the first occurrence of needle.
func lineCol(src, needle string) string {
	i := strings.Index(src, needle)
	if i < 0 {
		panic("needle not found: " + needle)
	}
	line := 1 + strings.Count(src[:i], "\n")
	col := i - strings.LastIndex(src[:i], "\n")
	return strconv.Itoa(line) + ":" + strconv.Itoa(col)
}

func lineOf(src, needle string) int {
	return 1 + strings.Count(src[:strings.Index(src, needle)], "\n")
}

func TestInterpIteratorObjects(t *testing.T) {
	r := NewRealm()
	arr := r.NewArray(IntValue(1), IntValue(2), IntValue(3))
	mk := func(kind IterKind) *Object { return r.newArrayIterator(arr, kind) }
	it, err := r.getIterator(ObjectValue(mk(IterKeys)))
	require.NoError(t, err)
	var got []any
	for {
		v, done, err := it.step(r)
		require.NoError(t, err)
		if done {
			break
		}
		got = append(got, r.ToGo(v))
	}
	assert.Equal(t, []any{int64(0), int64(1), int64(2)}, got)

	it, err = r.getIterator(ObjectValue(mk(IterEntries)))
	require.NoError(t, err)
	v, done, err := it.step(r)
	require.NoError(t, err)
	require.False(t, done)
	assert.Equal(t, []any{int64(0), int64(1)}, r.ToGo(v))

	// Exhausted iterators stay exhausted; for-of over an iterator object
	// consumes it.
	m, err := syntax.ParseModule("i.js", `export function drain(it) { let out = []; for (const v of it) out[out.length] = v; for (const v of it) out[out.length] = "again"; return out; }`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	drain, _ := env.GetBindingValue("drain")
	res, err := r.Call(drain, Undefined(), []Value{ObjectValue(mk(IterValues))})
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, r.ToGo(res))

	_, err = r.getIterator(ObjectValue(r.NewObject()))
	assert.Equal(t, "TypeError: [object Object] is not iterable", errMessage(t, err))
	_, err = r.getIterator(IntValue(1))
	assert.Equal(t, "TypeError: 1 is not iterable", errMessage(t, err))

	// Array-like target through an iterator object.
	like := r.NewObject()
	like.DefineOwnDataFast(r, lengthKey, IntValue(2), attrDefault)
	like.DefineOwnDataFast(r, IndexKey(0), StringValue(asciiString("x")), attrDefault)
	it, err = r.getIterator(ObjectValue(r.newArrayIterator(like, IterValues)))
	require.NoError(t, err)
	v, _, _ = it.step(r)
	assert.Equal(t, "x", r.ToGo(v))
	v, _, _ = it.step(r)
	assert.Nil(t, r.ToGo(v))
	_, done, _ = it.step(r)
	assert.True(t, done)
}

func TestInterpUnsupportedSurfaces(t *testing.T) {
	r := NewRealm()
	_, err := r.EvaluateModule(&bytecodeStubFunction)
	require.Error(t, err)
	_, err = r.RunScript(&bytecodeStubFunction)
	require.Error(t, err)
}

// TestInterpCallEntry covers the entries of a JavaScript call: the frame
// callValue sets up itself (every parameter passed, nothing else to bind)
// and enterFrame's (missing parameters, rest parameter, arguments object,
// captured bindings, the first call), the register stack and frame table
// growing under them, and the state a throwing, overflowing or interrupted
// callee leaves behind.
func TestInterpCallEntry(t *testing.T) {
	f := evalModule(t, `
function two(a, b) { return [a, b]; }
function count() { return arguments.length + ":" + Array.prototype.join.call(arguments, ","); }
function extra(a) { return arguments.length + ":" + arguments[1]; }
function rest(a, ...r) { return a + "|" + r.join(","); }
function capture(a) { return () => a; }
function self() { return this; }
const lexical = () => typeof this;
function wide(n) {
  const a = n + 1, b = a + 1, c = b + 1, d = c + 1, e = d + 1, g = e + 1, h = g + 1, i = h + 1;
  return n === 0 ? a - b + c - d + e - g + h - i : 1 + wide(n - 1);
}
function thrower(n) { if (n === 0) throw new Error("bottom"); return thrower(n - 1); }
export function entries() {
  return [
    two(1, 2).join(), two(1).join(), String(two(1)[1]), two(1, 2, 3).join(),
    count(), count(1, 2, 3), extra(1), extra(1, 2),
    rest(1), rest(1, 2, 3), capture(7)(), capture(8)(),
    String(self()), self.call(5), typeof self.call("s"), lexical(),
    ({ v: 4, m() { return this.v; } }).m(),
  ].join(";");
}
export function deep(n) { return wide(n); }
export function throwThenCall() {
  try { thrower(100); } catch (e) { return e.message + ":" + two(3, 4).join(); }
}
export function overflow() {
  function f(x) { return f(x + 1) + 1; }
  try { f(0); } catch (e) { return e.name + ":" + two(5, 6).join() + ":" + wide(200); }
}
export function noNew() { class C {} try { C(); return "called"; } catch (e) { return e.name; } }
export function loop(n) { let s = 0; for (let i = 0; i < n; i++) s += two(i, 0)[0]; return s; }
export function callsWhile(stop) { function leaf(x) { return x; } let s = leaf(1); stop(); s += leaf(2); return s; }
export function deepWhile(stop, n) { return n === 0 ? stop() : deepWhile(stop, n - 1); }
`)
	r := f.r
	assert.Equal(t, "1,2;1,;undefined;1,2;0:;3:1,2,3;1:undefined;2:2;1|;1|2,3;7;8;undefined;5;string;undefined;4", f.call("entries"))
	// 400 frames of a dozen registers grow the register stack (64 at
	// first) and the frame table (16) several times, on callValue's path.
	assert.Equal(t, int64(400-4), f.call("deep", 400))
	assert.Equal(t, "bottom:3,4", f.call("throwThenCall"))
	assert.Equal(t, "RangeError:5,6:196", f.call("overflow"))
	assert.Equal(t, "TypeError", f.call("noNew"))
	assert.Equal(t, int64(4950), f.call("loop", 100))
	st := &r.interp
	assert.Zero(t, st.sp, "register stack released")
	assert.Zero(t, st.nframes, "frames released")
	assert.Zero(t, r.callDepth, "call depth released")

	// An interrupt raised by a native is seen by the next JavaScript call,
	// which callValue sets up itself.
	stop := nativeFn(r, func(Value, []Value) (Value, error) { r.Interrupt("stop"); return Undefined(), nil })
	_, err := f.callErr("callsWhile", stop)
	var ie *InterruptedError
	require.True(t, errors.As(err, &ie), "the call after the interrupt ran: %v", err)
	r.ClearInterrupt()
	_, err = f.callErr("deepWhile", stop, 50)
	require.NoError(t, err, "stop() is the last call: nothing is left to see the interrupt")
	r.ClearInterrupt()
	assert.Zero(t, st.sp)
	assert.Zero(t, st.nframes)
	assert.Zero(t, r.callDepth)
	assert.Equal(t, int64(4950), f.call("loop", 100), "the realm runs again")
}

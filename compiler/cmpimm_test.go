package compiler

import (
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
)

// TestCompareImmediateEmission checks which comparisons that decide a jump
// take their integer right operand in the X word (JmpLtI .. JmpStrictNeI):
// int32 literals but -0, not a literal a loop nest holds in a register, and
// not a comparison whose value is used. A string is a constant's (JmpStrictNeK).
func TestCompareImmediateEmission(t *testing.T) {
	fn := compileModule(t, `
export function imm(n) { if (n < 2) return n; if (n === -7) return 0; return n >= 2147483647 ? 1 : n <= -2147483648 ? 2 : 3; }
export function reg(n) { return n < 1.5 || n > -0 || n < 2147483648 || n === "2" ? 1 : 0; }
export function value(n) { return n < 2; }
export function loop(n) { var s = 0; for (var i = 0; i < 1000; i++) s += n; return s; }`)
	imms := func(name string) (got []int32, regs int) {
		for _, w := range words(child(t, fn, name)) {
			switch op := bytecode.DecodeOp(w[0]); {
			case op >= bytecode.JmpLtI && op <= bytecode.JmpStrictNeI:
				got = append(got, int32(w[1]))
			case op >= bytecode.JmpLt && op <= bytecode.JmpStrictNe:
				regs++
			case op == bytecode.JmpStrictEqK || op == bytecode.JmpStrictNeK:
				got = append(got, -1)
			}
		}
		return got, regs
	}
	got, regs := imms("imm")
	assert.Equal(t, []int32{2, -7, 2147483647, -2147483648}, got)
	assert.Zero(t, regs)
	got, regs = imms("reg")
	assert.Equal(t, []int32{-1}, got)
	assert.Equal(t, 3, regs)
	got, regs = imms("value")
	assert.Empty(t, got)
	assert.Zero(t, regs)
	assert.Contains(t, ops(child(t, fn, "value")), bytecode.Lt)
	got, regs = imms("loop")
	assert.Empty(t, got, "the loop's bound is hoisted into a register")
	assert.Equal(t, 1, regs)
}

// TestCompareConstantEmission checks the strict comparisons that decide a
// jump against a string, null or undefined, which take the constant in the
// X word (JmpStrictEqK, JmpStrictNeK): either operand order, a template
// without substitutions, a switch's cases; not a string a loop nest holds
// in a register, a local named undefined, a number or a comparison whose
// value is used.
func TestCompareConstantEmission(t *testing.T) {
	fn := compileModule(t, `
export function k(x, y) { if (x === null) return 1; if (null !== y) return 2; if (x === "s") return 3;
	if (`+"`t`"+` !== y) return 4; if (x === undefined || y !== void 0) return 5; return 0; }
export function sw(x) { switch (x) { case "a": return 1; case null: return 2; case 3: return 3; case x: return 4; } }
export function loop(a) { var n = 0; for (var i = 0; i < a.length; i++) if (a[i] === "x") n++; return n; }
export function local(x) { var undefined = 0; if (x === undefined) return 1; if (x === 1.5) return 2; return x === null; }`)
	consts := func(name string) (got []string, regs int) {
		f := child(t, fn, name)
		for _, w := range words(f) {
			switch op := bytecode.DecodeOp(w[0]); op {
			case bytecode.JmpStrictEqK, bytecode.JmpStrictNeK:
				k := f.Consts[w[1]]
				switch k.Kind {
				case bytecode.ConstString:
					got = append(got, op.String()+" "+k.Str)
				case bytecode.ConstNull:
					got = append(got, op.String()+" null")
				case bytecode.ConstUndefined:
					got = append(got, op.String()+" undefined")
				}
			case bytecode.JmpStrictEq, bytecode.JmpStrictNe:
				regs++
			}
		}
		return got, regs
	}
	got, regs := consts("k")
	assert.Equal(t, []string{"JmpStrictNeK null", "JmpStrictEqK null", "JmpStrictNeK s", "JmpStrictEqK t",
		"JmpStrictEqK undefined", "JmpStrictEqK undefined"}, got)
	assert.Zero(t, regs)
	got, regs = consts("sw")
	assert.Equal(t, []string{"JmpStrictEqK a", "JmpStrictEqK null"}, got)
	assert.Equal(t, 2, regs, "a number and a non-literal")
	got, regs = consts("loop")
	assert.Empty(t, got, "the loop's string is hoisted into a register")
	assert.Equal(t, 1, regs)
	got, regs = consts("local")
	assert.Empty(t, got)
	assert.Equal(t, 2, regs, "a local named undefined, and 1.5")
	assert.Contains(t, ops(child(t, fn, "local")), bytecode.StrictEq)
	assert.NoError(t, bytecode.VerifyRegisters(child(t, fn, "k")))
}

// TestLogicalBoolEmission: && and || whose left operand is a boolean jump
// on it to the constant they yield; as a returned value each side returns
// on its own. A left operand that is not a boolean keeps its register and
// the jump on it.
func TestLogicalBoolEmission(t *testing.T) {
	fn := compileModule(t, `
export function ret(e) { return e !== null && e < -2; }
export function any(e) { return e === 43 || e === 45 || e === 46 || g(e); }
export function val(e) { var r = e === 1 || e === 2; return [r]; }
export function not(e) { var r = !e || g(e); return [r]; }
export function obj(e) { var r = e.p && g(e); return [r]; }
function g(x) { return x; }`)
	count := func(name string) map[bytecode.Op]int {
		n := map[bytecode.Op]int{}
		for _, op := range ops(child(t, fn, name)) {
			n[op]++
		}
		return n
	}
	ret := count("ret")
	assert.Equal(t, 1, ret[bytecode.JmpStrictEqK], "ret")
	assert.Equal(t, 1, ret[bytecode.LoadFalse], "ret")
	assert.Equal(t, 2, ret[bytecode.Ret], "ret")
	assert.Zero(t, ret[bytecode.StrictNe]+ret[bytecode.JmpF]+ret[bytecode.LoadNull], "ret")
	anyOps := count("any")
	assert.Equal(t, 3, anyOps[bytecode.JmpStrictEqI], "any")
	assert.Equal(t, 1, anyOps[bytecode.LoadTrue], "any")
	assert.Zero(t, anyOps[bytecode.StrictEq]+anyOps[bytecode.JmpT]+anyOps[bytecode.LoadInt], "any")
	val := count("val")
	assert.Equal(t, 1, val[bytecode.JmpStrictEqI], "val")
	assert.Equal(t, 1, val[bytecode.StrictEq], "val: the right operand yields its own value")
	assert.Equal(t, 1, val[bytecode.LoadTrue], "val")
	assert.Equal(t, 1, val[bytecode.Jmp], "val")
	not := count("not")
	assert.Zero(t, not[bytecode.Not], "not")
	assert.Equal(t, 1, not[bytecode.LoadTrue], "not")
	obj := count("obj")
	assert.Equal(t, 1, obj[bytecode.JmpF], "obj")
	assert.Zero(t, obj[bytecode.LoadFalse], "obj")
}

// TestOperandsInPlaceEmission: a switch on a local and a for-in or for-of
// loop over one read its register without a Move; a test that assigns
// the local makes the switch copy it first.
func TestOperandsInPlaceEmission(t *testing.T) {
	fn := compileModule(t, `
export function sw(x) { switch (x) { case "a": return 1; case "b": return 2; case g(x): return 3; } return 0; }
export function swa(x) { switch (x) { case "a": return 1; case (x = "b"): return 2; } return 0; }
export function fin(o) { var n = 0; for (var k in o) n++; return n; }
export function fof(a) { var n = 0; for (var v of a) n += v; return n; }
function g(x) { return x; }`)
	moves := func(name string) int {
		n := 0
		for _, op := range ops(child(t, fn, name)) {
			if op == bytecode.Move {
				n++
			}
		}
		return n
	}
	assert.Equal(t, 1, moves("sw"), "sw: g's argument only")
	assert.Equal(t, 2, moves("swa"), "swa: the switch's copy and the assignment's value")
	assert.Equal(t, 1, moves("fin"), "fin: the key")
	assert.Equal(t, 1, moves("fof"), "fof: the value")
}

// TestCaseRuns: the first of caseRunMin or more cases on constants in a row
// is JmpCaseK, a default between them included; a test that is not a
// constant ends a run; a shorter run is JmpStrictEqK each.
func TestCaseRuns(t *testing.T) {
	fn := compileModule(t, `
export function long(x) { switch (x) { case "a": return 1; case "b": return 2; default: return 0; case null: return 3; case "d": return 4; case "e": return 5; } }
export function short(x) { switch (x) { case "a": return 1; case "b": return 2; case "c": return 3; } return 0; }
export function split(x) { switch (x) { case "a": case "b": case "c": case "d": return 1; case g(x): return 2; case "e": case "f": case "g": case "h": return 3; case "i": return 4; } return 0; }
function g(x) { return x; }`)
	count := func(name string) (heads, rest int) {
		for _, op := range ops(child(t, fn, name)) {
			switch op {
			case bytecode.JmpCaseK:
				heads++
			case bytecode.JmpStrictEqK:
				rest++
			}
		}
		return
	}
	h, r := count("long")
	assert.Equal(t, [2]int{1, 4}, [2]int{h, r}, "long")
	h, r = count("short")
	assert.Equal(t, [2]int{0, 3}, [2]int{h, r}, "short")
	h, r = count("split")
	assert.Equal(t, [2]int{2, 7}, [2]int{h, r}, "split")
}

// TestSeqConditionJumps: a comma expression as a condition jumps on its
// last expression's own test, with no comparison into a register and no
// JmpF.
func TestSeqConditionJumps(t *testing.T) {
	fn := compileModule(t, "export function sq(o, a) { if (a = o.k, a !== null) return 1; while (a++, a < 3) ; if (o.n = 1, o.m === 3) return 2; }")
	got := ops(child(t, fn, "sq"))
	for _, op := range []bytecode.Op{bytecode.StrictNe, bytecode.StrictEq, bytecode.Lt, bytecode.JmpF, bytecode.JmpT, bytecode.LoadNull} {
		assert.NotContains(t, got, op)
	}
	assert.Contains(t, got, bytecode.JmpStrictEqK)
	assert.Contains(t, got, bytecode.JmpStrictNeI)
}

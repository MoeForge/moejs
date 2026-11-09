package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTestJumps covers the conditions compiled as the jump's own test
// (compiler testJump) and the templates without substitutions the
// compiler reads as string literals: typeof tests in both senses and
// either order, with a template as the type name too and an undeclared
// name; loose comparisons with null, undefined and void 0 in either order
// and both senses, against values that convert and one whose valueOf
// throws; strict ones with undefined; names that are not the constant (a
// local and a with object's property named undefined, void of an
// expression with an effect); operands read once; template keys and
// constants in loops.
func TestTestJumps(t *testing.T) {
	const ty = "function ty(x) { var s = ''; if (typeof x == `object`) s += 'o'; if (typeof x != `function`) s += '!f';" +
		" if (`string` === typeof x) s += 's'; if (!(typeof x !== `undefined`)) s += 'u'; return s; } "
	const nu = `function nu(x) { var s = "";
		if (x == null) s += "n"; if (x != null) s += "!n"; if (void 0 == x) s += "v"; if (!(undefined != x)) s += "u";
		if (x === undefined) s += "U"; if (x !== void 0) s += "!U"; if (!(x === void 0)) s += "!!U"; if (void 0 !== x) s += "~U";
		return s; } `
	runScriptCases(t, []scriptCase{
		{"typeof", []string{ty + "[ty({}), ty(null), ty(f => 1), ty('a'), ty(undefined), ty(1n)].join(' ')"}, "o!f o!f  !fs !fu !f"},
		{"typeof of more values", []string{ty + "[ty(Symbol()), ty(new Proxy(function () {}, {})), ty(class {}), ty(new String('a')), ty(0)].join(' ')"}, "!f   o!f !f"},
		{"typeof an undeclared name", []string{"function g() { if (typeof nosuch == `undefined`) return 1; return 2; } g()"}, "1"},
		{"typeof with an escaped name", []string{"function es(x) { return typeof x == `\\u0073tring` ? 's' : 'n'; } es('a') + es(1)"}, "sn"},
		{"null and undefined", []string{nu + `[nu(null), nu(undefined)].join(" ")`}, "nvu!U!!U~U nvuU"},
		{"other values", []string{nu + `[nu(0), nu(""), nu(false), nu(NaN), nu({}), nu(0n)].join(" ")`},
			"!n!U!!U~U !n!U!!U~U !n!U!!U~U !n!U!!U~U !n!U!!U~U !n!U!!U~U"},
		{"no conversion", []string{nu + `nu({ valueOf() { throw 1; }, toString() { throw 2; } })`}, "!n!U!!U~U"},
		{"a local named undefined", []string{`function lu(x) { var undefined = 0; if (x == undefined) return "eq"; return "ne"; } [lu(0), lu(null)].join()`}, "eq,ne"},
		{"a with property named undefined", []string{`function wu(x) { with ({undefined: 0}) { if (x == undefined) return "eq"; return "ne"; } } [wu(0), wu(null)].join()`}, "eq,ne"},
		{"void with an effect", []string{`function vs(x) { var n = 0; if (x == void n++) return n; return -n; } vs(null)`}, "1"},
		{"operand read once", []string{`function once() { var n = 0, g = { get p() { n++; return null; } };
			if (g.p == null) n += 10; if (g.p !== void 0) n += 100; if (typeof g.p != "object") n += 1000; return n; } once()`}, "113"},
		{"template keys", []string{"function k(o) { return o[`a`] + o[`0`] + o[`b c`]; } k({a: 1, 0: 2, 'b c': 3})"}, "6"},
		{"templates fold", []string{"function fo() { if (``) return 'empty'; return `a` + `b` + `${'c'}`; } fo()"}, "abc"},
		{"templates in a loop", []string{"function lp(a) { var n = 0; for (var i = 0; i < a.length; i++) if (a[i] === `x` || typeof a[i] == `number`) n++; return n; } lp(['x', 'y', 'x', 1])"}, "3"},
	})
}

// TestLogicalBoolValues: && and || whose left operand is a boolean test
// that operand with a jump to the constant they yield (compiler
// boolValued), as a value and as a returned value (returnExpr), also in a
// generator, inside try/finally and as an argument. Each expression gives
// for each input what it gives with every comparison behind a call (B),
// which compiles it the other way, and evaluates the same operands in the
// same order (the log).
func TestLogicalBoolValues(t *testing.T) {
	exprs := []string{
		"B(x === 1) || B(x === 2)", "B(x !== null) && B(x < -2)", "B(!x) || f(x)", "B(x < 3) && B(x > 0) || f(x)",
		"B(x == null) || B(x === 0) && f(x)", "B(typeof x == 'string') && B(x.length > 1)", "B('a' in o) && B(o.a === x)",
		"B(o instanceof Object) || f(x)", "B(x >= 1) && (B(x <= 2) || f(x))", "B(true) && x", "B(false) || f(x)",
		"B(x === 1) || B(x === 2) || B(x === 3) || f(x)", "B(x !== 0) && B(x !== '') && B(!!x) && f(x)",
		"(B(x < 0) || B(x > 2)) && (B(x != 3) || f(x))", "B(v(x) < v(2)) || f(x)", "B(f(x) === null) && B(f(1) === 1)", "B(x != null) || B(x.p === 1)",
	}
	const inputs = "[0, 1, 2, 3, -3, NaN, null, undefined, 'ab', '', {}, [], {valueOf() { log.push('vo'); return 2; }}]"
	var src strings.Builder
	src.WriteString(`var log = [], o = {a: 1};
function f(v) { log.push(typeof v === "object" && v !== null ? "obj" : String(v)); return v; }
function v(x) { log.push("v"); return x; }
function show(r) { return typeof r === "object" && r !== null ? "obj" : String(r); }
function id(r) { return r; }
var out = [];
`)
	for i, e := range exprs {
		fmt.Fprintf(&src, `function r%[1]d(x) { return %[2]s; }
function e%[1]d(x) { var r = %[2]s; return r; }
function* g%[1]d(x) { return %[2]s; }
function t%[1]d(x) { try { return %[2]s; } finally { log.push("fin"); } }
function a%[1]d(x) { return id(%[2]s); }
for (var x of %[3]s) for (var fn of [r%[1]d, e%[1]d, x => g%[1]d(x).next().value, t%[1]d, a%[1]d]) {
	var res;
	try { res = show(fn(x)); } catch (err) { res = err.name; }
	out.push(res + ":" + log.join(" ")); log = [];
}
`, i, e, inputs)
	}
	src.WriteString(`out.join("|")`)
	fast := strings.ReplaceAll(src.String(), "B(", "(")
	slow := "function B(v) { return v; }\n" + src.String()
	got, want := runScripts(t, fast), runScripts(t, slow)
	assert.Equal(t, want, got)
	assert.Equal(t, 17*13*5, strings.Count(want, "|")+1)
	assert.Contains(t, want, "TypeError", "x.p of null and undefined throws")
}

// TestOperandsInPlace: a switch compares a local's own register with its
// cases' tests and a for-in or for-of loop starts from it (compiler
// operand). The statement keeps the value it read when a test or the body
// changes the local: through mapped arguments, an assignment or an update
// in a test, an assignment in the body, the loop's own variable.
func TestOperandsInPlace(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"mapped arguments in a test", []string{`function f(x) { var a = arguments; function h() { a[0] = 2; return 1; }
			switch (x) { case h(): return "one"; case 2: return "two"; default: return "d"; } } f(1)`}, "one"},
		{"assignment in a test", []string{`function s(x) { switch (x) { case (x = 2, 1): return "one"; case 2: return "two"; } return "d"; } s(1)`}, "one"},
		{"update in a test", []string{`function u(x) { switch (x) { case 0: return "zero"; case x++: return "first:" + x; } return "d"; } u(1)`}, "first:2"},
		{"default between cases", []string{`function d(x) { switch (x) { case 1: return "1"; default: return "d"; case 2: return "2"; } } [d(1), d(2), d(3)].join()`}, "1,2,d"},
		{"for-in body assigns the object", []string{`function fi(o) { var ks = []; for (var k in o) { ks.push(k); o = {z: 1}; } return ks.join(); } fi({a: 1, b: 2})`}, "a,b"},
		{"for-of body assigns the array", []string{`function fo(a) { var s = 0; for (var v of a) { s += v; a = [100]; } return s; } fo([1, 2, 3])`}, "6"},
		{"the loop's own variable", []string{`function same(a) { var r = []; for (a of a) r.push(a); return r.join(); } same([1, 2])`}, "1,2"},
		{"for-in over mapped arguments", []string{`function g(o) { var a = arguments; var n = 0; for (var k in o) { if (n++ == 0) a[0] = {}; } return n; } g({p: 1, q: 2})`}, "2"},
	})
}

// TestCaseChains: a switch's cases on constants that test its register in
// a row start with JmpCaseK, which the engine runs in a loop (caseChain).
// Each switch gives for each input what it gives with every case test
// behind a call (B), which compiles to JmpStrictEq: duplicates, null and
// undefined, strings of one length, a non-ASCII one, the empty string,
// strings made at run time and ropes, values of other types, a default
// between cases, runs a call breaks, a run of three (JmpStrictEqK each),
// two switches in a row and fallthrough.
func TestCaseChains(t *testing.T) {
	const src = `var log = [];
function f(v) { log.push(v); return v; }
function sw(x) { switch (x) { case B("alpha"): return 1; case B("beta"): return 2; case B("al"): return 3; case B(null): return 4;
	case B("alpha"): return 5; case B(undefined): return 6; case B("gamma"): return 7; case B(""): return 8; case B("é"): return 9;
	default: return 0; case B("delta"): return 10; case B("alphb"): return 11; } }
function mix(x) { switch (x) { case B("a"): return 1; case B("b"): return 2; case B("e"): return 12; case B("g"): return 13;
	case f("c"): return 3; case B("c"): return 4; case B("d"): return 5; case B("h"): return 14; case B("i"): return 15; } return 0; }
function three(x) { switch (x) { case B("a"): return 1; case B("b"): return 2; case B("c"): return 3; } return 0; }
function two(x, y) { switch (x) { case B("a"): return 1; case B("b"): return 2; case B("c"): return 5; case B("d"): return 6; }
	switch (y) { case B("a"): return 3; case B("b"): return 4; case B("c"): return 7; case B("d"): return 8; } return 0; }
function fall(x) { var s = ""; switch (x) { case B("a"): s += "a"; case B("b"): s += "b"; break; case B("c"): s += "c"; default: s += "d"; case B("e"): s += "e"; } return s; }
var rope = "alpha".repeat(1), big = "x".repeat(20);
var inputs = ["alpha", "beta", "al", null, undefined, "gamma", "", "é", "delta", "alphb", "x", 0, 1, NaN, {}, true,
	"alp" + "ha".repeat(1), ["be", "ta"].join(""), String.fromCharCode(233), "gam".concat("ma"), (big + "delta").slice(20),
	rope, "a", "b", "c", "d", "e", "g", "h", "i", "a".repeat(1)];
var out = [];
for (var x of inputs) {
	out.push(sw(x), mix(x), three(x), two(x, x), two("z", x), fall(x), log.join());
	log = [];
}
out.join("|")`
	fast := strings.ReplaceAll(src, "B(", "(")
	slow := "function B(v) { return v; }\n" + src
	got, want := runScripts(t, fast), runScripts(t, slow)
	assert.Equal(t, want, got)
	assert.Contains(t, want, "|10|", "delta, made at run time too")
}

// TestSeqConditions: a condition that is a comma expression runs its
// leading expressions for their effects and jumps on its last one's test
// (compiler condJump). Each condition, in an if, a loop, a conditional
// expression, behind ! and in && and ||, gives what it gives behind a call
// (B), which takes its value instead, with the same effects in the same
// order (the log).
func TestSeqConditions(t *testing.T) {
	const src = `var log = [];
function f(v) { log.push(String(v)); return v; }
function c1(x) { var a; if (B((a = f(x), a !== null))) return "nn:" + a; return "null"; }
function c2(o) { if (B((f(1), o.k === 3))) return "three"; if (B((f(2), f(3), typeof o.k == "string"))) return "str"; return "other"; }
function c3(x) { var n = 0; while (B((n++, n < x))) f(n); return n; }
function c4(x) { var s = ""; for (var i = 0; B((s += i, i < x)); i++) f(i); return s; }
function c5(x) { return B((f("a"), x == null)) ? "nullish" : "set"; }
function c6(x) { if (!B((f("n"), x === "k"))) return "notk"; return "k"; }
function c7(x, y) { if (B((f("l"), x > 1)) && B((f("r"), y !== void 0))) return "both"; return B((f("o"), x)) || B((f("p"), y)) ? "or" : "neither"; }
function c8(x) { var a; do { f(x); } while (B((a = x, x = x - 1, a > 1))); return x; }
function c9(x) { if (B((0, x))) return "t"; if (B((1, 0, x === 0))) return "z"; return "f"; }
var out = [];
for (var x of [null, undefined, 0, 1, 3, -1, "k", "s", {k: 3}, {k: "x"}, {k: null}, NaN]) {
	for (var fn of [c1, c3, c4, c5, c6, c8, c9]) {
		var r; try { r = String(fn(x)); } catch (e) { r = e.name; }
		out.push(r + ":" + log.join()); log = [];
	}
	out.push(String(c2(x !== null && typeof x == "object" ? x : {k: x})) + ":" + log.join()); log = [];
	out.push(c7(x, x) + ":" + log.join(), c7(2, x) + ":" + log.join()); log = [];
}
out.join("|")`
	fast := strings.ReplaceAll(src, "B(", "(")
	slow := "function B(v) { return v; }\n" + src
	got, want := runScripts(t, fast), runScripts(t, slow)
	assert.Equal(t, want, got)
	assert.Contains(t, want, "nn:1:1|")
}

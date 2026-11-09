package engine

import "testing"

// TestCompareImmediate covers the compare-and-branch ops against an
// integer in their X word (JmpLtI .. JmpStrictNeI): each relation in both
// jump senses, operands that are not numbers (strings, BigInts, objects
// with valueOf, undefined, null, booleans), NaN, -0, the int32 bounds, and
// the literals that stay in a register (non-integers, -0, past int32, and
// constants a loop nest hoists).
func TestCompareImmediate(t *testing.T) {
	// cmp runs every relation against the literal c in an if (the N forms)
	// and an if-not (the plain forms), and the strict ones too.
	const cmp = `function cmp(x) { var s = "";
		if (x < 2) s += "<"; if (!(x < 2)) s += "!<";
		if (x <= 2) s += "<="; if (!(x <= 2)) s += "!<=";
		if (x > 2) s += ">"; if (!(x > 2)) s += "!>";
		if (x >= 2) s += ">="; if (!(x >= 2)) s += "!>=";
		if (x === 2) s += "==="; if (x !== 2) s += "!==";
		return s; } `
	runScriptCases(t, []scriptCase{
		{"numbers", []string{cmp + `[1, 2, 3, 2.5, -1].map(cmp).join(" ")`}, "<<=!>!>=!== !<<=!>>==== !<!<=>>=!== !<!<=>>=!== <<=!>!>=!=="},
		{"NaN", []string{cmp + `cmp(NaN)`}, "!<!<=!>!>=!=="},
		{"strings", []string{cmp + `[cmp("2"), cmp("10"), cmp("abc"), cmp("")].join(" ")`}, "!<<=!>>=!== !<!<=>>=!== !<!<=!>!>=!== <<=!>!>=!=="},
		{"BigInt", []string{cmp + `[cmp(2n), cmp(1n), cmp(3n)].join(" ")`}, "!<<=!>>=!== <<=!>!>=!== !<!<=>>=!=="},
		{"other primitives", []string{cmp + `[cmp(undefined), cmp(null), cmp(true), cmp(Symbol.iterator === 0)].join(" ")`}, "!<!<=!>!>=!== <<=!>!>=!== <<=!>!>=!== <<=!>!>=!=="},
		{"valueOf once per comparison", []string{cmp + `var n = 0; var v = { valueOf() { n++; return 2; } }; cmp(v) + " " + n`}, "!<<=!>>=!== 8"},
		{"valueOf throws", []string{`function f(x) { try { if (x < 2) return "lt"; return "ge"; } catch (e) { return e; } }
			f({ valueOf() { throw "boom"; } })`}, "boom"},
		{"symbol throws", []string{`function f(x) { try { if (x < 2) return "lt"; return "ge"; } catch (e) { return e.name; } } f(Symbol())`}, "TypeError"},
		{"strict equality", []string{`function z(x) { return x === 0 ? "eq" : "ne"; } function nz(x) { if (x !== 0) return "ne"; return "eq"; }
			[0, -0, "0", 0n, false, null, NaN, 1].map(x => z(x) + nz(x)).join()`}, "eqeq,eqeq,nene,nene,nene,nene,nene,nene"},
		{"negative and bounds", []string{`function f(x) { return [x < -5, x > 2147483647, x < -2147483648, x === 2147483647, x === -2147483648].join(); }
			[f(-6), f(2147483648), f(-2147483649), f(2147483647), f(-2147483648)].join(" ")`},
			"true,false,false,false,false false,true,false,false,false true,false,true,false,false false,false,false,true,false true,false,false,false,true"},
		{"past int32", []string{`function f(x) { return [x < 2147483648, x > -2147483649, x === 4294967296].join(); } f(2147483647) + " " + f(4294967296)`}, "true,true,false false,true,true"},
		{"non-integer and -0 literals", []string{`function f(x) { return [x < 1.5, x > -0, x === -0, x <= 0.5].join(); } f(1) + " " + f(-0) + " " + f(0.25)`}, "true,true,false,false true,false,true,true true,true,false,true"},
		{"constant on the left", []string{`function f(x) { return [2 < x, 2 === x, 2 >= x].join(); } f(1) + " " + f(2) + " " + f(3)`}, "false,false,true false,true,true true,false,false"},
		{"logical conditions", []string{`function f(x, y) { return x > 0 && y < 10 ? "in" : x === 0 || y === 0 ? "zero" : "out"; } [f(1, 2), f(0, 50), f(5, 0), f(-1, 2)].join()`}, "in,zero,in,out"},
		{"loops", []string{`function f(n) { var s = 0; for (var i = 0; i < 10; i++) { if (i % 3 === 0) continue; s += i; } while (n > 3) n--; do { s++; } while (s < 50); return s + ":" + n; } f(9)`}, "50:3"},
		{"in a loop past the hoisted constants", []string{`function f(x) { var s = 0; for (var i = 0; i < 3; i++) { s += (x < 1 ? 1 : 0) + (x < 2 ? 1 : 0) + (x < 3 ? 1 : 0) + (x < 4 ? 1 : 0) + (x < 5 ? 1 : 0) + (x < 6 ? 1 : 0) + (x < 7 ? 1 : 0) + (x < 8 ? 1 : 0) + (x < 9 ? 1 : 0) + (x < 10 ? 1 : 0); } return s; } f(4)`}, "18"},
		{"fib", []string{`function fib(n) { if (n < 2) return n; return fib(n - 1) + fib(n - 2); } fib(20)`}, "6765"},
		{"generator", []string{`function* g() { var x = yield; if (x >= 3) yield "big"; else yield "small"; } var it = g(); it.next(); it.next(5).value`}, "big"},
		{"Symbol.toPrimitive hint", []string{cmp + `var hints = []; var x = { [Symbol.toPrimitive](h) { hints.push(h); return 1; } };
			cmp(x) + ":" + hints.length + ":" + [...new Set(hints)].join()`}, "<<=!>!>=!==:8:number"},
		{"a -0 register", []string{`function c0(x) { var s = "";
			if (x < 0) s += "<"; if (!(x < 0)) s += "!<"; if (x <= 0) s += "<="; if (!(x <= 0)) s += "!<=";
			if (x > 0) s += ">"; if (!(x > 0)) s += "!>"; if (x >= 0) s += ">="; if (!(x >= 0)) s += "!>=";
			if (x === 0) s += "==="; if (x !== 0) s += "!=="; if (x < 1) s += "<1"; if (x > -1) s += ">-1"; return s; }
			c0(-0) + " " + c0(0)`}, "!<<=!>>====<1>-1 !<<=!>>====<1>-1"},
	})
}

// TestCompareConstant covers the strict comparisons that branch against a
// string, null or undefined in their X word (JmpStrictEqK, JmpStrictNeK):
// both senses and operand orders, strings built at run time and atoms,
// non-ASCII and empty strings, a String object, numbers (the smallest
// denormal has the bits of a string's tag), the other primitives, and a
// switch's cases.
func TestCompareConstant(t *testing.T) {
	const k = `function k(x) { var s = "";
		if (x === "ab") s += "s"; if ("ab" !== x) s += "!s"; if (x === "é") s += "e"; if (x === ` + "``" + `) s += "0";
		if (x === null) s += "n"; if (null !== x) s += "!n"; if (x === undefined) s += "u"; if (void 0 !== x) s += "!u";
		return s; } `
	runScriptCases(t, []scriptCase{
		{"strings", []string{k + `["ab", "a" + "b".repeat(1), "xab".slice(1), JSON.parse('"ab"'), String.fromCharCode(233), "", "abc"].map(k).join(" ")`},
			"s!n!u s!n!u s!n!u s!n!u !se!n!u !s0!n!u !s!n!u"},
		{"not strings", []string{k + `[new String("ab"), 5e-324, 0, NaN, false, true, {}, 1n, Symbol("ab")].map(k).join(" ")`},
			"!s!n!u !s!n!u !s!n!u !s!n!u !s!n!u !s!n!u !s!n!u !s!n!u !s!n!u"},
		{"null and undefined", []string{k + `[k(null), k(undefined), k()].join(" ")`}, "!sn!u !s!nu !s!nu"},
		{"switch", []string{`function sw(x) { switch (x) { case "ab": return "s"; case null: return "n"; case undefined: return "u"; case 1: return "1"; default: return "d"; } }
			["ab", "a".concat("b"), null, undefined, 1, "1", 5e-324, 0].map(sw).join()`}, "s,s,n,u,1,d,d,d"},
		{"in a loop", []string{`function lp(a) { var n = 0; for (var i = 0; i < a.length; i++) { if (a[i] === null) n += 10; else if (a[i] !== "x") n++; } return n; }
			lp(["x", null, "y", "x".repeat(1), undefined])`}, "12"},
	})
}

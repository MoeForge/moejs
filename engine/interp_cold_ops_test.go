package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestColdArithmetic runs the arithmetic and bitwise operators, ++, --, a
// postfix update whose value is used and a read x[++k] on operands that
// are not numbers, which run sends to coldOp: objects, strings, null,
// booleans, undefined and BigInts, and a mix of a BigInt and a number. The
// expected result is Node 24's.
func TestColdArithmetic(t *testing.T) {
	src := `
var o = {valueOf() { return 6; }}, out = [];
out.push(o * 2, o - 1, o / 4, o % 4, o ** 2, "3" * "4", null - 1, true / 2);
out.push(o & 3, o | 1, o ^ 3, o << 2, o >> 1, -o >>> 28, "7" & "3");
var b = 7n; out.push(String(b * 3n), String(b - 1n), String(b / 2n), String(b % 4n), String(b ** 2n));
out.push(String(b & 3n), String(b | 8n), String(b ^ 1n), String(b << 2n), String(b >> 1n));
var i = 1n; i++; ++i; var j = 5n; j--; --j; out.push(String(i), String(j));
var k = "5"; k++; var m = "5"; --m; var n = {valueOf() { return 1; }}; var nn = n++; out.push(k, m, n, nn);
var u; u++; out.push(u);
(function () {
	var q = "5", qq = q--, bq = 3n, bqq = bq++; out.push(q, qq, typeof qq, String(bq), String(bqq));
	var ar = [4, 5, 6], ks = "0", kb = 0n, ko = {valueOf() { return 1; }};
	out.push(ar[++ks], ks, ar[++kb], String(kb), ar[++ko], ko, "ab"[++ks]);
})();
try { 1n * 1; } catch (e) { out.push(e.constructor.name); }
out.join(" ");`
	assert.Equal(t, "12 5 1.5 2 36 12 -1 0.5 2 7 5 24 3 15 3 21 6 3 3 49 3 15 6 28 3 3 3 6 4 2 1 NaN 4 5 number 4 3 5 1 5 1 6 2  TypeError", runScripts(t, src))
}

// TestTailOps runs the ops numbered last, whose cases run lays out after
// every other, with their fast paths and the slow ones they leave to
// coldOp: a function's vars cleared at its start (UndefRange), a postfix
// decrement whose value is used (PostDec), a template's conversions and
// concatenations (ToStr, Concat), and for-in over keys deleted and added
// during the loop (ForInNext); and the reads and writes of closure
// variables that run walks the environments out to (GetEnv of depth 3 and
// more, SetEnv of depth 1 and more). The expected result is Node 24's.
func TestTailOps(t *testing.T) {
	src := `var out = [];
(function () {
	var a, b, c, d, e, f, g, h, i, j, k;
	out.push([a, b, c, d, e, f, g, h, i, j, k].every(x => x === undefined));
	var n = 3, m = n--; out.push(n, m);
	var s = "5", t = s--; out.push(s, t, typeof t);
	var x = 7.5, y = x--; out.push(x, y);
	var big = 2n, bb = big--; out.push(String(big), String(bb));
	var u, uu = u--; out.push(u, uu);
	var obj = {toString() { return "O"; }};
	out.push(` + "`" + `<${1}|${"s"}|${obj}|${null}|${2n}>` + "`" + `, ` + "`" + `${"a"}${"b"}` + "`" + `);
	try { ` + "`" + `${Symbol()}` + "`" + `; } catch (err) { out.push(err.constructor.name); }
	var o = {p: 1, q: 2, r: 3}, keys = [];
	for (var key in o) { keys.push(key); delete o.r; o.z = 9; }
	out.push(keys.join());
	var proto = {inh: 1}, child = Object.create(proto); child.own = 2;
	var ks = []; for (var kk in child) ks.push(kk); out.push(ks.join());
	var cnt = 0; for (var e2 in null) cnt++; for (var e3 in "ab") cnt++; out.push(cnt);
})();
(function () {
	var v0 = "a";
	(function () {
		var v1 = "b";
		(function () {
			var v2 = "c";
			(function () {
				var v3 = "d";
				(function () {
					out.push(v0 + v1 + v2 + v3);
					v3 = "D"; v2 = "C"; v1 = "B"; v0 = "A";
					out.push(v0 + v1 + v2 + v3);
				})();
				out.push(v3);
			})();
			out.push(v2);
		})();
		out.push(v1);
	})();
	out.push(v0);
})();
out.join(" ");`
	assert.Equal(t, "true 2 3 4 5 number 6.5 7.5 1 2 NaN NaN <1|s|O|null|2> ab TypeError p,q own,inh 2 abcd ABCD D C B A", runScripts(t, src))
}

// TestColdSlowPaths runs what run's property, conversion and comparison
// cases send to coldOp: reads and writes of primitives and their errors, a
// template's and an addition's conversions, == of values of other types,
// relational comparisons and compare-and-branch loops of strings and
// objects, and the positions of their errors. The expected result is Node
// 24's but for the message of a write to a property of undefined and the
// columns of stack frames, which are the engine's own.
func TestColdSlowPaths(t *testing.T) {
	src := `var out = [];
(function () {
	"use strict";
	var s = "abc", n = 5, u, nul = null, t = true, o = {valueOf() { return 2; }, toString() { return "O"; }};
	out.push(s.length, s.toUpperCase(), n.toFixed(1), t.toString(), (1n).toString(), typeof n.length);
	try { u.x; } catch (e) { out.push(e.message); }
	try { nul.length; } catch (e) { out.push(e.message); }
	try { s.x = 1; } catch (e) { out.push(e.constructor.name); }
	try { u.x = 1; } catch (e) { out.push(e.message); }
	out.push(o + 1, o - 1, "a" + 1, s - 1, ` + "`" + `${n}|${o}|${u}|${nul}|${t}|${1n}` + "`" + `);
	try { ` + "`" + `${Symbol()}` + "`" + `; } catch (e) { out.push(e.constructor.name); }
	out.push(1 == "1", null == undefined, 0 == false, o == 2, o == "O", NaN == NaN, "a" == "a", s != "abc", u == 0, 1n == 1, o != o);
	out.push("a" < "b", "b" <= "a", "10" > "9", 10 > "9", o >= 2, u < 1, nul <= 0, 1n < 2);
	var r = 0;
	for (var x = "a"; x < "e"; x = String.fromCharCode(x.charCodeAt(0) + 1)) r++;
	for (var y = "0"; y < 3; y = String(+y + 1)) r++;
	var z = o; while (z <= 3) { r++; z = z + 1; }
	if (s >= "abd") r += 100; if (!(s < "abd")) r += 1000;
	out.push(r);
})();
(function () {
	var s = "q";
	s.x = 1;
	out.push(s.x, (5).y = 3);
})();
function line() { try { null.p; } catch (e) { return at(e); } }
function line2() { try { var a = "x"; a < Symbol(); } catch (e) { return at(e); } }
function at(e) { return e.stack.split("\n")[1].trim().replace(/\(.*:(\d+):(\d+)\)/, "$1:$2"); }
out.push(line(), line2());
out.join(" ");`
	assert.Equal(t, "3 ABC 5.0 true 1 undefined Cannot read properties of undefined (reading 'x') Cannot read properties of null (reading 'length') TypeError Cannot set property 'x' of undefined 3 1 a1 NaN 5|O|undefined|null|true|1 TypeError true true true true false false true false false true false true false false true true false true true 9  3 at line 26:25 at line2 27:43", runScripts(t, src))
}

// TestScalarOperands runs the operators whose fast paths compute on any
// two scalars and check the result for a NaN (scalarFloat), and the element
// reads and writes whose keys they read so, on numbers and the other
// scalars: each result must equal the same operator's on the operand boxed
// in an object, which run sends to coldOp's conversions. A NaN result is the
// canonical NaN.
func TestScalarOperands(t *testing.T) {
	src := `var bad = [];
function box(v) { return {valueOf() { return v; }, toString() { return v; }}; }
function check(what, x, y, got, want) {
	if (!Object.is(got, want)) bad.push(what + "(" + String(x) + ", " + String(y) + ") = " + String(got) + ", want " + String(want));
}
function jumps(x, y) {
	var c = 0;
	if (x < y) c |= 1; if (x <= y) c |= 2; if (x > y) c |= 4; if (x >= y) c |= 8;
	if (!(x < y)) c |= 16; if (!(x <= y)) c |= 32; if (!(x > y)) c |= 64; if (!(x >= y)) c |= 128;
	return c;
}
function jumpsI(x) {
	var c = 0;
	if (x < 1) c |= 1; if (x <= 0) c |= 2; if (x > 0) c |= 4; if (x >= 1) c |= 8;
	if (!(x < 1)) c |= 16; if (!(x <= 0)) c |= 32; if (!(x > 0)) c |= 64; if (!(x >= 1)) c |= 128;
	return c;
}
(function () {
	var vs = [undefined, null, true, false, 1, 0, -0, 0.5, -1, 2147483648, Infinity, -Infinity, NaN];
	for (var i = 0; i < vs.length; i++) {
		var x = vs[i], bx = box(x);
		for (var j = 0; j < vs.length; j++) {
			var y = vs[j];
			check("+", x, y, x + y, bx + y); check("-", x, y, x - y, bx - y);
			check("*", x, y, x * y, bx * y); check("/", x, y, x / y, bx / y);
			check("%", x, y, x % y, bx % y); check("**", x, y, x ** y, bx ** y);
			check("&", x, y, x & y, bx & y); check("|", x, y, x | y, bx | y);
			check("^", x, y, x ^ y, bx ^ y); check("<<", x, y, x << y, bx << y);
			check(">>", x, y, x >> y, bx >> y); check(">>>", x, y, x >>> y, bx >>> y);
			check("<", x, y, x < y, bx < y); check("<=", x, y, x <= y, bx <= y);
			check(">", x, y, x > y, bx > y); check(">=", x, y, x >= y, bx >= y);
			check("jumps", x, y, jumps(x, y), jumps(bx, y));
		}
		check("jumpsI", x, 0, jumpsI(x), jumpsI(bx));
		check("+1", x, 1, x + 1, bx + 1); check("-1", x, 1, x - 1, bx - 1);
		check("+5", x, 5, x + 5, bx + 5); check("-5", x, 5, x - 5, bx - 5);
		var p = x, q = p++, bp = bx, bq = bp++;
		check("x++", x, 0, q, bq); check("x++ after", x, 0, p, bp);
		p = x; q = p--; bp = bx; bq = bp--;
		check("x--", x, 0, q, bq); check("x-- after", x, 0, p, bp);
		p = x; p++; bp = bx; bp++; check("++", x, 0, p, bp);
		p = x; p--; bp = bx; bp--; check("--", x, 0, p, bp);
		var arr = [10, 20, 30];
		arr["true"] = "T"; arr["null"] = "N"; arr["undefined"] = "U"; arr["NaN"] = "NA"; arr["false"] = "F"; arr["Infinity"] = "I";
		check("get", x, 0, arr[x], arr[box(x)]);
		var k = x, bk = bx; check("get ++k", x, 0, arr[++k], arr[++bk]); check("++k", x, 0, k, bk);
		var w1 = [], w2 = []; w1[x] = 1; w2[box(x)] = 1;
		check("set", x, 0, Object.keys(w1).join(), Object.keys(w2).join());
		var ta = new Float64Array(2); ta[0] = 1.5; ta["true"] = 9; ta["null"] = 8;
		check("typed get", x, 0, ta[x], ta[box(x)]);
		var t1 = new Float64Array(2), t2 = new Float64Array(2); t1[x] = 3; t2[box(x)] = 3;
		check("typed set", x, 0, t1.join() + Object.keys(t1).join(), t2.join() + Object.keys(t2).join());
	}
})();
bad.length + (bad.length ? ": " + bad.slice(0, 10).join("; ") : "");`
	assert.Equal(t, "0", runScripts(t, src))
	r := NewRealm()
	for _, e := range []string{"a - a", "a + -a", "a * 0", "0 * a", "a / a", "0 / z", "z % z", "(a - a) + 1", "a - a < 1 ? 1 : a - a"} {
		v := evalValue(t, r, "(function (a, z) { return "+e+"; })(Infinity, 0)")
		require.True(t, v.IsNumber(), e)
		assert.Equal(t, canonicalNaN, v.Bits(), e)
	}
}

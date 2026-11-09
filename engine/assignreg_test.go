package engine

import "testing"

// TestAssignIntoRegister covers assignments to a variable in a register
// whose value is computed straight into it (compiler/expr.go
// lastOpWrites): every read of the old value must come before the write,
// and a variable the assignment cannot write in place (a closure's, one
// behind a with statement, a parameter the arguments object aliases, one
// in its TDZ) must be written where it lives. A handler that catches a
// throw from the middle of the value must find the old value.
func TestAssignIntoRegister(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"read after a call", []string{`function f(s) { function g(x) { return x * 10; } s = g(s) + s; return s; } f(3)`}, "33"},
		{"read after a call, right first", []string{`function f(s) { function g(x) { return x * 10; } s = s - g(s); return s; } f(3)`}, "-27"},
		{"assignment in the right operand", []string{`function f(s) { s = s - (s = 1); return s; } f(10)`}, "9"},
		{"assignment in the left operand", []string{`function f(s) { s = (s = 5) + s * 3; return s; } f(10)`}, "20"},
		{"assignment in a chain", []string{`function f(s) { s = s * 2 + (s = 100) - s; return s; } f(10)`}, "20"},
		{"nested assignment written in place", []string{`function f(s, t) { s = (t = s * 2) + (s = t + 1); return s + ":" + t; } f(3, 0)`}, "13:6"},
		{"postfix update", []string{`function f(s) { s = s++; return s; } f(4)`}, "4"},
		{"postfix update in a sum", []string{`function f(s) { s = s++ + s; return s; } f(4)`}, "9"},
		{"prefix update", []string{`function f(s) { s = ++s * 2 + s; return s; } f(4)`}, "15"},
		{"logical or", []string{`function f(s, t) { s = t || s; return s; } f(4, 0)`}, "4"},
		{"logical and", []string{`function f(s, t) { s = t && s; return s; } f(4, 1)`}, "4"},
		{"nullish", []string{`function f(s, t) { s = t ?? s; return s; } f(4, null)`}, "4"},
		{"conditional", []string{`function f(s, c) { s = c ? s + 1 : s - 1; return s; } f(4, 0) + ":" + f(4, 1)`}, "3:5"},
		{"call result register", []string{`function f(s) { s = String(s); return s; } f(5)`}, "5"},
		{"call result register, two calls", []string{`function f(s) { s = Math.max(s, 2) + Math.min(s, 2); return s; } f(5)`}, "7"},
		{"array literal", []string{`function f(s) { s = [s, s + 1]; return s.join(); } f(5)`}, "5,6"},
		{"object literal", []string{`function f(s) { s = {a: s, b: s * 2}; return s.a + s.b; } f(5)`}, "15"},
		{"template", []string{`function f(s, t) { s = ` + "`${t}${s}`" + `; return s; } f(5, "x")`}, "x5"},
		{"comma", []string{`function f(s) { s = (s++, s * 10); return s; } f(5)`}, "60"},
		{"property read", []string{`function f(s) { let n = 0; while (s) { n += s.v; s = s.next; } return n; } f({v: 1, next: {v: 2, next: {v: 3, next: null}}})`}, "6"},
		{"property chain", []string{`function f(s) { s = s.a.b; return s; } f({a: {b: 7}})`}, "7"},
		{"computed property", []string{`function f(s, a) { s = a[s]; return s; } f(1, [10, 20])`}, "20"},
		{"unary", []string{`function f(s) { s = -s; s = ~s; s = !s; return s; } f(5)`}, "false"},
		{"typeof", []string{`function f(s) { s = typeof s; return s; } f(5)`}, "number"},
		{"typeof test", []string{`function f(s) { s = typeof s !== "number"; return s; } f(5)`}, "false"},
		{"destructuring in the value", []string{`function f(s) { s = ([s] = [5, 6]); return s.join(); } f(1)`}, "5,6"},
		{"destructuring swap", []string{`function f(s, t) { [s, t] = [t, s + t]; return s + ":" + t; } f(1, 2)`}, "2:3"},
		{"destructuring in an operand", []string{`function f(s) { s = s + ([s] = [5])[0]; return s; } f(1)`}, "6"},
		{"compound with an assignment", []string{`function f(s) { s += (s = 5); return s; } f(10)`}, "15"},
		{"compound with an update", []string{`function f(s) { s -= s++; return s; } f(10)`}, "0"},
		{"compound read after the value", []string{`function f(s) { s *= (s = 2) + s; return s; } f(10)`}, "40"},
		{"logical assignment", []string{`function f(s) { s ||= (s = 3, s + 1); return s; } f(0)`}, "4"},
		{"closure sees the write", []string{`function f(s) { const g = () => s; s = s + 1; s = s * 10; return g(); } f(1)`}, "20"},
		{"closure written in the value", []string{`function f(s) { const g = () => s = 100; s = s + g(); return s; } f(1)`}, "101"},
		{"with object", []string{`function f() { var s = 1, o = {s: 100}; with (o) { s = s + 1; } return s + ":" + o.s; } f()`}, "1:101"},
		{"with object without the name", []string{`function f() { var s = 1, o = {}; with (o) { s = s + 1; } return s + ":" + ("s" in o); } f()`}, "2:false"},
		{"sloppy arguments alias", []string{`function f(a) { a = a + 1; return arguments[0] + ":" + a; } f(1)`}, "2:2"},
		{"sloppy arguments written", []string{`function f(a) { arguments[0] = 10; a = a * 2; return arguments[0] + ":" + a; } f(1)`}, "20:20"},
		{"strict arguments", []string{`function f(a) { "use strict"; a = a + 1; return arguments[0] + ":" + a; } f(1)`}, "1:2"},
		{"TDZ, the value reads", []string{`function f() { try { s = s + 1; } catch (e) { return e.name; } let s = 0; } f()`}, "ReferenceError"},
		{"TDZ, the value does not read", []string{`function f() { try { s = !delete s; } catch (e) { return e.name; } let s = 0; } f()`}, "ReferenceError"},
		{"TDZ in a loop", []string{`function f() { const out = []; for (let i = 0; i < 2; i++) { try { s = typeof i; } catch (e) { out.push(e.name); } } let s; return out.join(); } f()`}, "ReferenceError,ReferenceError"},
		{"const", []string{`function f() { const s = 1; try { s = s + 1; } catch (e) { return e.name + s; } } f()`}, "TypeError1"},
		{"throw from the right operand", []string{`function f() { let s = 5; try { s = s * 2 + g(); } catch (e) { return s; } function g() { throw 0; } } f()`}, "5"},
		{"throw from the last operator", []string{`function f() { let s = 5; try { s = s + {valueOf() { throw 0; }}; } catch (e) { return s; } } f()`}, "5"},
		{"throw from a BigInt mix", []string{`function f() { let s = 5n; try { s = s * 2n + 1; } catch (e) { return String(s); } } f()`}, "5"},
		{"throw from a property read", []string{`function f() { let s = 5; try { s = s.missing.x; } catch (e) { return s; } } f()`}, "5"},
		{"throw from a getter", []string{`function f(o) { let s = 5; try { s = o.p; } catch (e) { return s; } } f({get p() { throw 0; }})`}, "5"},
		{"throw from a unary operator", []string{`function f() { let s = 5; try { s = -Symbol(); } catch (e) { return s; } } f()`}, "5"},
		{"throw while building an array", []string{`function f() { let s = 5; try { s = [s, g()]; } catch (e) { return s; } function g() { throw 0; } } f()`}, "5"},
		{"throw in a template", []string{`function f(t) { let s = 5; try { s = ` + "`${t}${Symbol()}`" + `; } catch (e) { return typeof s + s; } } f("x")`}, "number5"},
		{"throw in a call's arguments", []string{`function f() { let s = 5; try { s = String(s, g()); } catch (e) { return s; } function g() { throw 0; } } f()`}, "5"},
		{"finally sees the old value", []string{`function f() { let s = 5, r; try { try { s = s + {valueOf() { throw 0; }}; } finally { r = s; } } catch (e) {} return r; } f()`}, "5"},
		{"generator resumes", []string{`function* g() { let s = 1; s = s + (yield s); yield s; } var it = g(); it.next().value + ":" + it.next(10).value`}, "1:11"},
		{"generator returns at the yield", []string{`var r; function* g() { let s = 1; try { s = s + (yield s); } finally { r = s; } } var it = g(); it.next(); it.return(7); r`}, "1"},
		{"value of the assignment", []string{`function f(s) { const t = (s = s * 3 + 1); return s + ":" + t; } f(2)`}, "7:7"},
		{"store in a chain", []string{`function f(o, s) { for (var i = 0; i < 3; i++) s = o[i] = s * 2 + 1; return s + ":" + o.join(); } f([], 1)`}, "15:3,7,15"},
		{"store in a chain to a property", []string{`function f(o, s) { s = o.p = s + o.q; return s + ":" + o.p; } f({q: 2}, 1)`}, "3:3"},
		{"store in a chain, its value used", []string{`function f(o, s) { const t = (s = o[0] = s * 3); return s + ":" + t + ":" + o[0]; } f([], 2)`}, "6:6:6"},
		{"store in a chain, the key reads", []string{`function f(o, s) { s = o[s] = s + 1; return s + ":" + o[1]; } f({}, 1)`}, "2:2"},
		{"store in a chain, the object reads", []string{`function f(s) { s = s.p = s.q + 1; return s; } f({q: 1})`}, "2"},
		{"store in a chain, a setter sees the value", []string{`function f(s) { var seen; const o = {set p(v) { seen = v; }}; s = o.p = s * 2; return s + ":" + seen; } f(4)`}, "8:8"},
		{"store in a chain throws", []string{`function f(o) { let s = 5; try { s = o.p = s + 1; } catch (e) { return s; } } f({set p(v) { throw 0; }})`}, "5"},
		{"store in a chain throws, strict", []string{`function f(o) { "use strict"; let s = 5; try { s = o[0] = s + 1; } catch (e) { return s; } } f(Object.freeze([1]))`}, "5"},
		{"store in a chain throws, finally", []string{`function f(o) { let s = 5, r; try { try { s = o.p = s + 1; } finally { r = s; } } catch (e) {} return r; } f({set p(v) { throw 0; }})`}, "5"},
		{"script completion", []string{`var x = 2; function f(s) { return s = s * 3; } f(x)`}, "6"},
	})
}

// TestAssignIntoRegisterThrows throws from each op that lastOpWrites lets
// write the variable's register (the cases of interp_run.go's arithmetic,
// comparison and property sections and InstanceOf, coldOp's In and
// InstanceOf, classOp's
// GetPrivate and InPrivate): the catch must find the old value, which
// holds only while the op writes R[A] once it has a result. Not, Typeof,
// TypeofIs, StrictEq and StrictNe cannot throw.
func TestAssignIntoRegisterThrows(t *testing.T) {
	const bad = `{valueOf() { throw 0; }, toString() { throw 0; }}`
	ops := []struct{ name, init, value string }{
		{"Add", "5", "s + " + bad},
		{"Add string", `"a"`, "s + " + bad},
		{"Add BigInt mix", "5", "1n + s"},
		{"Sub", "5", "s - " + bad},
		{"Mul", "5", "s * " + bad},
		{"Mul BigInt mix", "5", "s * 2n"},
		{"Div", "5", "s / " + bad},
		{"Mod BigInt mix", "5", "s % 1n"},
		{"Exp BigInt mix", "5", "s ** 2n"},
		{"AddImm", bad, "s + 1"},
		{"SubImm", bad, "s - 1"},
		{"Inc", bad, "++s"},
		{"Dec", bad, "--s"},
		{"PostInc", bad, "s++"},
		{"PostDec", bad, "s--"},
		{"BitAnd", "5", "s & 1n"},
		{"BitOr", "5", "s | " + bad},
		{"BitXor", "5", "s ^ 1n"},
		{"Shl", "5", "s << 1n"},
		{"Shr", "5", "s >> " + bad},
		{"UShr", "5", "s >>> 0n"},
		{"Eq", "5", "s == " + bad},
		{"Ne", "5", "s != " + bad},
		{"Lt", "5", "s < " + bad},
		{"Le", "5", "s <= " + bad},
		{"Gt", "5", "s > " + bad},
		{"Ge", "5", "s >= " + bad},
		{"In trap", `"k"`, "s in new Proxy({}, {has() { throw 0; }})"},
		{"In primitive", `"k"`, "s in 5"},
		{"InstanceOf hasInstance", "5", "s instanceof {[Symbol.hasInstance]() { throw 0; }}"},
		{"InstanceOf primitive", "5", "s instanceof 5"},
		{"Neg", "5", "-Symbol()"},
		{"Plus", "5", "+Symbol()"},
		{"BitNot", "5", "~Symbol()"},
		{"GetProp trap", "new Proxy({}, {get() { throw 0; }})", "s.x"},
		{"GetProp getter", "{get x() { throw 0; }}", "s.x"},
		{"GetProp null", "null", "s.x"},
		{"GetLen null", "null", "s.length"},
		{"GetLen getter", "{get length() { throw 0; }}", "s.length"},
		{"GetElem key", "{}", "s[" + bad + "]"},
		{"GetElem null", "null", "s[0]"},
		{"chain link", "{a: null}", "s.a.b.c"},
		{"comma head", "5", "(null.x, s + 1)"},
	}
	var cases []scriptCase
	for _, op := range ops {
		src := `function f() { let s = ` + op.init + `; const old = s; try { s = ` + op.value + `; } catch (e) { return s === old; } return "no throw"; } f()`
		cases = append(cases, scriptCase{op.name, []string{src}, "true"})
	}
	cases = append(cases,
		scriptCase{"GetPrivate", []string{`class C { #x = 1; static f() { let s = {}; const old = s; try { s = s.#x; } catch (e) { return s === old; } } } C.f()`}, "true"},
		scriptCase{"InPrivate", []string{`class C { #x = 1; static f() { let s = 5; try { s = #x in s; } catch (e) { return s; } } } C.f()`}, "5"},
		// GetElemInc increments its key before the read that throws.
		scriptCase{"GetElemInc null", []string{`function f() { let s = 5, k = 0; try { s = null[++k]; } catch (e) { return s + ":" + k; } } f()`}, "5:1"},
		scriptCase{"GetElemInc key", []string{`function f() { const bad = ` + bad + `; let s = 5, k = bad; try { s = [1][++k]; } catch (e) { return s + ":" + (k === bad); } } f()`}, "5:true"},
		scriptCase{"GetElemInc getter", []string{`function f() { let s = 5, k = 0; try { s = {get 1() { throw 0; }}[++k]; } catch (e) { return s + ":" + k; } } f()`}, "5:1"},
		// Suspensions inside the value.
		scriptCase{"throw at a yield", []string{`function* g() { let s = 1; try { s = s + (yield); } catch (e) { s = s * 10; } yield s; } var it = g(); it.next(); it.throw(0).value`}, "10"},
		scriptCase{"yield as the left operand", []string{`function* g() { let s = 1; s = (yield) + s; yield s; } var it = g(); it.next(); it.next(5).value`}, "6"},
		scriptCase{"yield between two reads", []string{`function* g() { let s = 1; s = s + (yield) + s; yield s; } var it = g(); it.next(); it.next(5).value`}, "7"},
		scriptCase{"rejected await", []string{`var out; async function f() { let s = 1; try { s = s + await Promise.reject(2); } catch (e) { out = s; } } f();`, `out`}, "1"},
		scriptCase{"fulfilled await", []string{`var out; async function f() { let s = 1; s = s + await 2; out = s; } f();`, `out`}, "3"},
		// Aliases.
		scriptCase{"arrow writes arguments", []string{`function f(a) { const g = () => arguments[0] = 5; a = a + g(); return a + ":" + arguments[0]; } f(1)`}, "6:6"},
		scriptCase{"catch parameter", []string{`function f() { try { throw 1; } catch (s) { s = s + 1; return s; } } f()`}, "2"},
		scriptCase{"string append keeps the old string", []string{`function f() { let s = "x".repeat(100), t = s; s = s + "y"; s = s + "z"; return t.length + ":" + s.length + ":" + (t + "q").slice(-2); } f()`}, "100:102:xq"},
	)
	runScriptCases(t, cases)
}

// TestValueFromItsRegister: an object literal's property and a member
// assignment whose value is not used read a local from its own register
// (compiler localReg): the property or the store gets the local's value at
// that point, a setter or a definition that throws leaves it as it was, and
// sloppy code's stores to primitives and to frozen objects stay silent.
func TestValueFromItsRegister(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"shorthand and named", []string{`function f(a, b) { var o = {a, b: b, c: a}; return o.a + o.b + o.c; } f(1, 2)`}, "4"},
		{"value read before later properties", []string{`function f(a) { var o = {x: a, y: (a = 5), z: a}; return [o.x, o.y, o.z].join(); } f(1)`}, "1,5,5"},
		{"shorthand __proto__ is an own property", []string{`function f(__proto__) { var o = {__proto__}; return Object.getPrototypeOf(o) === Object.prototype && o.__proto__ === 7; } f(7)`}, "true"},
		{"index key", []string{`function f(a) { var o = {0: a, "1": a}; return o[0] + o[1]; } f(3)`}, "6"},
		{"setter sees the value", []string{`function f(v) { var seen, o = {set p(x) { seen = x; }}; o.p = v; return seen; } f(9)`}, "9"},
		{"element store", []string{`function f(v, k) { var o = {}; o[k] = v; o.q = v; return o.z + o.q; } f(4, "z")`}, "8"},
		{"throwing setter keeps the local", []string{`function f(v) { var o = {set p(x) { throw x; }}; try { o.p = v; } catch (e) { return e + ":" + v; } } f(2)`}, "2:2"},
		{"sloppy primitive target", []string{`function f(v) { var s = "str"; s.p = v; return typeof s.p; } f(1)`}, "undefined"},
		{"sloppy frozen target", []string{`function f(v) { var o = Object.freeze({p: 1}); o.p = v; return o.p; } f(5)`}, "1"},
		{"strict frozen target throws", []string{`function f(v) { "use strict"; var o = Object.freeze({p: 1}); try { o.p = v; } catch (e) { return e instanceof TypeError; } } f(5)`}, "true"},
		{"value of the assignment", []string{`function f(o, v) { var r = (o.p = v); return r + o.p; } f({}, 3)`}, "6"},
	})
}

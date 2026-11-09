package engine

import (
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// TestVarsReadBeforeWritten: the prologue leaves out of its UndefRange the
// registers of the vars every path writes before it reads them (compiler
// allocVarRegs), so each case first runs fill, which leaves objects in the
// registers a frame takes, and every var read before its first write must
// still read undefined.
func TestVarsReadBeforeWritten(t *testing.T) {
	const fill = `function fill() { var a = {}, b = {}, c = {}, d = {}, e = {}, f = {}, g = {}, h = {}; return [a, b, c, d, e, f, g, h].length; }
function show(v) { return v === undefined ? "u" : typeof v === "object" ? "o" : String(v); }
`
	runScriptCases(t, []scriptCase{
		{"read before the declarator", []string{fill + `function t() { var r = show(x); var x = 1; return r + show(x); } fill(); t()`}, "u1"},
		{"in a branch", []string{fill + `function t(c) { if (c) { var x = 1; } return show(x); } fill(); t(false)`}, "u"},
		{"in a loop body", []string{fill + `function t() { var r = ""; for (var i = 0; i < 2; i++) { r += show(y); var y = i; } return r; } fill(); t()`}, "u0"},
		{"in a labelled block", []string{fill + `function t() { l: { break l; var x = 1; } return show(x); } fill(); t()`}, "u"},
		{"in try, thrown before", []string{fill + `function t() { try { throw 0; var x = 1; } catch (e) {} return show(x); } fill(); t()`}, "u"},
		{"in finally", []string{fill + `function t() { try { return show(x); } finally { var x = 1; } } fill(); t()`}, "u"},
		{"self-initialized", []string{fill + `function t() { var x = x; return show(x); } fill(); t()`}, "u"},
		{"later declarator", []string{fill + `function t() { var a = show(b), b = 2; return a + show(b); } fill(); t()`}, "u2"},
		{"no initializer", []string{fill + `function t() { var x; return show(x); } fill(); t()`}, "u"},
		{"for-in head", []string{fill + `function t() { var r = ""; for (var k in {}) {} r += show(k); return r; } fill(); t()`}, "u"},
		{"for-of head", []string{fill + `function t() { for (var k of []) {} return show(k); } fill(); t()`}, "u"},
		{"destructuring", []string{fill + `function t() { var r = show(p); var {p} = {p: 1}; return r + show(p); } fill(); t()`}, "u1"},
		{"annex B", []string{fill + `function t() { var r = show(k); { function k() {} } return r + typeof k; } fill(); t()`}, "ufunction"},
		{"generator", []string{fill + `function* g() { var r = show(x); yield r; var x = 1; yield show(x); } function t() { var it = g(); fill(); var a = it.next().value; fill(); return a + it.next().value; } t()`}, "u1"},
		{"generator resumed", []string{fill + `function* g() { yield 0; var r = show(x); var x = 1; yield r + show(x); } function t() { var it = g(); it.next(); fill(); return it.next().value; } t()`}, "u1"},
		{"parameter expressions", []string{fill + `function t(a, f = () => a) { var r = show(x); var x = 1; var a; return r + show(x) + show(a) + show(f()); } fill(); t(5)`}, "u155"},
		{"declarator after a read in a closure", []string{fill + `function t() { var f = () => x; var r = show(f()); var x = 1; return r + show(f()); } fill(); t()`}, "u1"},
		{"written then read", []string{fill + `function t() { var x = {}; var y = x; return show(y); } fill(); t()`}, "o"},
		{"in a case", []string{fill + `function t(k) { switch (k) { case 1: var x = 1; break; case 2: return show(x); } return show(x); } fill(); var a = t(2); fill(); var b = t(3); a + b + t(1)`}, "uu1"},
		{"case fallthrough", []string{fill + `function t(k) { switch (k) { case 1: var x = 1; case 2: return show(x); } } fill(); var a = t(2); a + t(1)`}, "u1"},
		{"short circuit", []string{fill + `function t(c) { c && (x = 1); return show(x); var x; } fill(); var a = t(false); a + t(true)`}, "u1"},
		{"logical assignment", []string{fill + `function t(c) { var r = c; r ||= (x = 1); return show(x); var x; } fill(); var a = t(true); a + t(false)`}, "u1"},
		{"one arm", []string{fill + `function t(c) { var r = c ? (x = 1) : 0; return show(x); var x; } fill(); var a = t(false); a + t(true)`}, "u1"},
		{"optional chain", []string{fill + `function t(o) { o?.f(x = 1); return show(x); var x; } fill(); var a = t(null); a + t({f() {}})`}, "u1"},
		{"after a loop", []string{fill + `function t(o) { for (var k in o) var v = k; return show(v); } fill(); var a = t({}); a + t({p: 1})`}, "up"},
		{"after a while", []string{fill + `function t(n) { while (n-- > 0) var x = n; return show(x); } fill(); var a = t(0); a + t(1)`}, "u0"},
		{"do-while test", []string{fill + `function t(n) { do { var x = n--; } while (x > 0); return show(x); } fill(); t(1)`}, "0"},
		{"catch block", []string{fill + `function g() { throw 0; } function t() { try { var x = g(); } catch (e) { return show(x); } } fill(); t()`}, "u"},
		{"with body", []string{fill + `function t(o) { with (o) { var x = 1; } return show(x) + o.x; } fill(); t({x: 0})`}, "u1"},
		{"both branches", []string{fill + `function t(c) { if (c) var x = 1; else x = 2; return show(x); } fill(); var a = t(true); fill(); a + t(false)`}, "12"},
		{"returning branch", []string{fill + `function t(c) { if (c) { return "r"; } else { var x = {}; } return show(x); } fill(); var a = t(true); fill(); a + t(false)`}, "ro"},
		{"loop body", []string{fill + `function t(a) { var r = ""; for (var i = 0; i < a.length; i++) { var v = a[i]; r += show(v); } return r; } fill(); t([1, 2])`}, "12"},
		{"try then catch", []string{fill + `function t(f) { try { var x = f(); } catch (e) { x = "c"; } return show(x); } fill(); var a = t(() => { throw 0; }); fill(); a + t(() => 1)`}, "c1"},
		{"finally", []string{fill + `function t() { try {} finally { var x = 1; } return show(x); } fill(); t()`}, "1"},
	})
}

// TestVarsInAllRegisters: a function whose vars take all 256 registers
// reads each of them undefined after fill256 has left numbers there.
func TestVarsInAllRegisters(t *testing.T) {
	vars := func(init string) string {
		names := make([]string, 256)
		for i := range names {
			names[i] = "a" + strconv.Itoa(i) + init
		}
		return strings.Join(names, ", ")
	}
	src := `function show(v) { return v === undefined ? "u" : String(v); }
function fill256() { var ` + vars(" = 1") + `; }
function first() { var ` + vars("") + `; return a0; }
function last() { var ` + vars("") + `; return a255; }
`
	runScriptCases(t, []scriptCase{
		{"first", []string{src + `fill256(); show(first())`}, "u"},
		{"last", []string{src + `fill256(); show(last())`}, "u"},
	})
}

// TestUndefRangeUnderGC: UndefRange runs of every length about the one
// where run stops clearing registers one at a time and copies undefined
// values over them (undefRegs), up to several copies of its block, clear
// registers in which fill has just left objects, while the collector runs all the
// time (GOGC=1). A clear that hid an overwritten pointer from the
// collector's barrier could free an object fill also keeps.
func TestUndefRangeUnderGC(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	names := func(n int, init string) string {
		s := make([]string, n)
		for i := range s {
			s[i] = "a" + strconv.Itoa(i) + init
		}
		return strings.Join(s, ", ")
	}
	var src strings.Builder
	src.WriteString("var keep = [];\n")
	src.WriteString("function fill(k) { var " + names(200, " = {k: k}") + "; keep.push(a7, a100, a199); if (keep.length > 300) keep.splice(0, 3); return 0; }\n")
	lens := []int{1, 2, undefLoopMax - 1, undefLoopMax, undefLoopMax + 1, 16, 31, 32, 33, 64, 65, 100, 150, 200}
	for _, n := range lens {
		src.WriteString("function v" + strconv.Itoa(n) + "() { var " + names(n, "") + "; return [" + names(n, "") + "].filter(x => x !== undefined).length; }\n")
	}
	// Twice the rounds on 32-bit systems, whose values are smaller, for as
	// many collections.
	src.WriteString("var bad = 0;\nfor (var i = 0; i < " + strconv.Itoa(200*64/strconv.IntSize) + "; i++) {\n")
	for _, n := range lens {
		src.WriteString("  fill(i); bad += v" + strconv.Itoa(n) + "();\n")
	}
	src.WriteString("}\nbad + \",\" + keep.every(o => typeof o.k === \"number\")")
	gcs := func() uint32 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.NumGC
	}
	before := gcs()
	runScriptCases(t, []scriptCase{{"clears", []string{src.String()}, "0,true"}})
	if n := gcs() - before; n < 20 {
		t.Errorf("the collector ran %d times during the clears, too few to test them", n)
	}
}

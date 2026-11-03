package engine

import (
	"errors"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRotatedLoops covers for, while and do-while loops, those that end in
// their condition's jump (a relational comparison, compiler/stmt.go
// loopBack) and those that do not: conditions evaluated once per test,
// with side effects, && and ||, break and continue (labelled too),
// per-iteration let bindings, constant conditions, and yields and awaits
// in the condition.
func TestRotatedLoops(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"no iteration", []string{`var n = 0; for (var i = 0; i < 0; i++) n++; while (n > 0) n--; n + ":" + i`}, "0:0"},
		{"tests per loop", []string{`var tests = 0; function c(v) { tests++; return v; } var i = 0, j = 0, k = 0;
			for (i = 0; c(i < 3); i++); while (c(j < 4)) j++; do k++; while (c(k < 2)); i + ":" + j + ":" + k + ":" + tests`}, "3:4:2:11"},
		{"compound conditions", []string{`var log = []; function t(s, v) { log.push(s); return v; } var i = 0;
			while (t("a", i < 2) && t("b", true)) i++; for (var j = 0; t("c", j > 5) || t("d", j < 1); j++);
			log.join("")`}, "ababacdcd"},
		{"continue and break", []string{`var s = []; for (var i = 0; i < 6; i++) { if (i === 1) continue; if (i === 4) break; s.push(i); }
			var j = 0; while (j < 5) { j++; if (j % 2) continue; s.push("w" + j); }
			var k = 0; do { k++; if (k === 2) continue; if (k === 4) break; s.push("d" + k); } while (k < 10); s.join()`}, "0,2,3,w2,w4,d1,d3"},
		{"labels", []string{`var s = []; outer: for (var i = 0; i < 3; i++) { inner: for (var j = 0; j < 3; j++) { if (j === 1) continue outer; if (i === 2) break outer; s.push(i + "" + j); } } s.join()`}, "00,10"},
		{"per-iteration bindings", []string{`var fs = []; for (let i = 0; i < 3; i++) fs.push(() => i); var gs = []; for (let i = 0; i < 3; i++) { gs.push(() => i); i++; }
			fs.map(f => f()).join() + ":" + gs.map(f => f()).join()`}, "0,1,2:1,3"},
		{"condition reads the next iteration's binding", []string{`var fs = []; for (let i = 0; fs.push(() => i), i < 2; i++); fs.map(f => f()).join()`}, "0,1,2"},
		{"constant conditions", []string{`var n = 0; while (true) { if (++n === 3) break; } for (;;) { if (++n === 5) break; } do { n++; } while (false); while (false) n = -1; for (; false;) n = -1; n`}, "6"},
		{"no update", []string{`var i = 0; for (; i < 3;) i++; i`}, "3"},
		{"condition throws", []string{`var i = 0; function c() { if (i === 2) throw "stop"; return true; } var r; try { while (c()) i++; } catch (e) { r = e; } r + ":" + i`}, "stop:2"},
		{"strict and immediate conditions", []string{`var a = 0, b = 5, c = 0, d = 0; while (a !== 3) a++; while (b > -2) b--; do c++; while (c !== d + 4); for (var x = 1.5; x < 5; x++) d++; a + ":" + b + ":" + c + ":" + d`}, "3:-2:4:4"},
		{"yield in the condition", []string{`function* g() { var n = 0; while (yield n) n++; return n; } var it = g(); it.next(); it.next(1); it.next(1); it.next(0).value`}, "2"},
		{"await in the condition", []string{`var out; async function f() { var n = 0; while (await (n < 3)) n++; out = n; } f();`, `out`}, "3"},
		{"generator yields in the body", []string{`function* g() { for (var i = 0; i < 3; i++) yield i; } [...g()].join()`}, "0,1,2"},
		{"nested", []string{`var s = 0; for (var i = 0; i < 10; i++) { var j = 0; while (j < i) { j++; do { s++; } while (s % 7); } } s`}, "315"},
		{"continue and break through finally", []string{`var log = []; for (var i = 0; i < 5; i++) { try { if (i === 1) continue; if (i === 3) break; log.push("b" + i); } finally { log.push("f" + i); } }
			var j = 0; while (j < 4) { try { j++; if (j === 2) continue; log.push("w" + j); } finally { log.push("g" + j); } }
			log.join() + ":" + i + ":" + j`}, "b0,f0,f1,b2,f2,f3,w1,g1,g2,w3,g3,w4,g4:3:4"},
		{"labelled continue from a for-of", []string{`var s = [], closed = 0; var it = { [Symbol.iterator]() { var n = 0; return { next() { return { value: n++, done: n > 3 }; }, return() { closed++; return {}; } }; } };
			outer: for (var i = 0; i < 3; i++) { for (const x of it) { if (x === 1) continue outer; s.push(i + ":" + x); } s.push("never"); } s.join() + " " + i + " " + closed`}, "0:0,1:0,2:0 3 3"},
		{"yield in a relational condition", []string{`function* g() { var i = 0; while (i < (yield i)) i++; for (var j = 0; (yield "j" + j) > j; j++); return i + ":" + j; }
			var it = g(), out = []; out.push(it.next().value); out.push(it.next(3).value, it.next(3).value, it.next(3).value);
			out.push(it.next(1).value, it.next(5).value, it.next(0).value); out.join()`}, "0,1,2,3,j0,j1,3:1"},
		{"await in a relational condition", []string{`var out; async function f() { var n = 0, lim = () => Promise.resolve(3); while (n < await lim()) n++;
			for (var k = 0; await k < 2; k++); out = n + ":" + k; } f();`, `out`}, "3:2"},
		{"switch with continue", []string{`var s = []; for (var i = 0; i < 5; i++) { switch (i) { case 1: continue; case 3: break; default: s.push("d" + i); } s.push(i); } s.join()`}, "d0,0,d2,2,3,d4,4"},
	})
}

// TestRotatedLoopInterrupts interrupts loops with every kind of condition,
// at an iteration that goes on and at the last one: rotated loops end in a
// relational compare-and-branch, which checks the flag whichever way it
// goes, as the backward Jmp that closed a for or while loop did; loops
// whose condition is a truthiness or equality test still end in a Jmp. (A
// do-while loop exited at its condition before its Jmp, so an interrupt in
// its last iteration went unseen there.) cmpHoisted's bound is a constant
// the loop nest holds in a register (compiler/hoist.go), so it closes with
// the register form too; TestRotatedLoopInterruptsImmediate covers the
// immediate forms.
func TestRotatedLoopInterrupts(t *testing.T) {
	src := `
var x = 1, n = 1000000000, s = "go";
export function jmpT() { while (x) tick(); }
export function jmpF() { do tick(); while (!(x === 0 && s === "stop")); }
export function cmpReg() { for (var i = 0; i < n; i++) tick(); }
export function cmpHoisted() { for (var i = 0; i < 1000000000; i++) tick(); }
export function whileReg() { var i = 0; while (i < n) { tick(); i++; } }
export function strictReg() { var i = 0; while (i !== n) { tick(); i++; } }
export function strictImm() { var i = 0; do { tick(); i++; } while (i !== 2000000000); }
export function last() { var k = 0; for (var i = 0; i < 3; i++) { tick(); k++; } return k; }
export function lastDo() { var k = 0; do { tick(); k++; } while (k < 3); return k; }
`
	f := evalModule(t, src)
	r := f.r
	ti := &tickInterrupter{}
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("tick"), ObjectValue(r.NewNativeFunction(AtomEmpty, 0, ti.native))))
	for _, name := range []string{"jmpT", "jmpF", "cmpReg", "cmpHoisted", "whileReg", "strictReg", "strictImm", "last", "lastDo"} {
		for _, target := range []int{1, 3} {
			ti.n, ti.target = 0, target
			_, err := f.callErr(name)
			var ie *InterruptedError
			assert.True(t, errors.As(err, &ie), "%s at tick %d: got %v, want *InterruptedError", name, target, err)
			assert.Equal(t, target, ti.n, "%s: the loop stops at the first back edge after the interrupt", name)
			assert.Equal(t, 0, r.interp.sp, name)
			assert.Zero(t, r.callDepth, name)
			r.ClearInterrupt()
		}
	}
	ti.n, ti.target = 0, -1
	assert.Equal(t, int64(3), f.call("last"))
	assert.Equal(t, int64(3), f.call("lastDo"))
}

// TestRotatedLoopInterruptsImmediate interrupts rotated loops that close
// with the immediate compare-and-branch ops (JmpLtI .. JmpNGeI), at an
// iteration that goes on and at the last one. A loop nest holds at most
// eight constants in registers (compiler/hoist.go), and each body here uses
// nine others twice, so the bound stays an immediate; the back edges are
// checked first, so that the test cannot quietly fall back to the register
// forms.
func TestRotatedLoopInterruptsImmediate(t *testing.T) {
	const body = `s = s * 10 * 10 + s * 11 * 11 + s * 12 * 12 + s * 13 * 13 + s * 14 * 14 + s * 15 * 15 + s * 16 * 16 + s * 17 * 17 + s * 18 * 18;`
	src := `
var s = 0;
export function forLt() { for (var i = 0; i < 1000000000; i++) { tick(); ` + body + ` } }
export function forGe() { for (var i = 1000000000; i >= 1; i--) { tick(); ` + body + ` } }
export function whileLe() { var i = 0; while (i <= 1000000000) { tick(); i++; ` + body + ` } }
export function doGt() { var i = 1000000000; do { tick(); i--; ` + body + ` } while (i > 0); }
export function lastImm() { var k = 0; for (var i = 0; i < 3; i++) { tick(); k++; ` + body + ` } return k; }
export function lastDoImm() { var k = 0; do { tick(); k++; ` + body + ` } while (k < 3); return k; }
`
	m, err := syntax.ParseModule("t.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	want := map[string]bytecode.Op{"forLt": bytecode.JmpLtI, "forGe": bytecode.JmpGeI, "whileLe": bytecode.JmpLeI,
		"doGt": bytecode.JmpGtI, "lastImm": bytecode.JmpLtI, "lastDoImm": bytecode.JmpLtI}
	for _, c := range code.Children {
		op, ok := want[c.Name]
		if !ok {
			continue
		}
		var back []bytecode.Op
		for pc := 0; pc < len(c.Code); pc += 1 + bytecode.DecodeOp(c.Code[pc]).ExtraWords() {
			o := bytecode.DecodeOp(c.Code[pc])
			if f := o.Format(); (f == bytecode.FmtSBx || f == bytecode.FmtAsBx && o != bytecode.LoadInt) && bytecode.DecodeSBx(c.Code[pc]) < 0 {
				back = append(back, o)
			}
		}
		assert.Equal(t, []bytecode.Op{op}, back, "%s closes with the immediate form", c.Name)
		delete(want, c.Name)
	}
	require.Empty(t, want, "functions not found")

	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	f := &moduleFixture{t: t, r: r, env: env}
	ti := &tickInterrupter{}
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("tick"), ObjectValue(r.NewNativeFunction(AtomEmpty, 0, ti.native))))
	for _, name := range []string{"forLt", "forGe", "whileLe", "doGt", "lastImm", "lastDoImm"} {
		for _, target := range []int{1, 3} {
			ti.n, ti.target = 0, target
			_, err := f.callErr(name)
			var ie *InterruptedError
			assert.True(t, errors.As(err, &ie), "%s at tick %d: got %v, want *InterruptedError", name, target, err)
			assert.Equal(t, target, ti.n, "%s: the loop stops at the first back edge after the interrupt", name)
			assert.Equal(t, 0, r.interp.sp, name)
			assert.Zero(t, r.callDepth, name)
			r.ClearInterrupt()
		}
	}
	ti.n, ti.target = 0, -1
	assert.Equal(t, int64(3), f.call("lastImm"))
	assert.Equal(t, int64(3), f.call("lastDoImm"))
}

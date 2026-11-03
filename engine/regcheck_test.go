//go:build moejsregcheck

package engine

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// regcheckBuild reports whether the tests run in the checked build.
const regcheckBuild = true

// skipInRegcheck skips a test of the fast build's allocations: the
// checked run allocates its register checks.
func skipInRegcheck(t *testing.T) {
	t.Helper()
	t.Skip("the checked build of run allocates its register checks")
}

// regcheckRun runs the script src in a fresh realm and returns the
// message of the panic it ends with, or "".
func regcheckRun(t *testing.T, src string) (msg string) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			msg = fmt.Sprint(p)
		}
	}()
	runScripts(t, src)
	return ""
}

// breakRow returns the register layouts with op's row changed by narrow,
// which gets the row's runs one at a time and passes on what it keeps.
func breakRow(op bytecode.Op, narrow func(w uint32, r, k int, span func(r, k int))) func(bytecode.Op, uint32, uint32, func(r, k int)) bool {
	return func(o bytecode.Op, w, x uint32, span func(r, k int)) bool {
		if o != op {
			return bytecode.RegisterSpans(o, w, x, span)
		}
		return bytecode.RegisterSpans(o, w, x, func(r, k int) { narrow(w, r, k, span) })
	}
}

// TestRegCheckBrokenRows breaks one row of the register layouts at a time
// and runs a program that executes the opcode: the checked run must fail
// on the register access the broken row leaves out, and an opcode without
// a row must fail when it runs. With the layouts as they are, each program
// runs.
func TestRegCheckBrokenRows(t *testing.T) {
	t.Cleanup(func() { regSpans = bytecode.RegisterSpans })
	a := func(w uint32) int { return int(bytecode.DecodeA(w)) }
	cases := []struct {
		name, src, want string
		spans           func(bytecode.Op, uint32, uint32, func(r, k int)) bool
	}{
		{"Move without its source", `function f() { var x = {}; var y = x; return y === x; } f()`, "Move",
			breakRow(bytecode.Move, func(w uint32, r, k int, span func(r, k int)) {
				if r == a(w) {
					span(r, k)
				}
			})},
		{"a method call without this", `var o = {m() { return this; }}; o.m() === o`, "Call",
			breakRow(bytecode.Call, func(w uint32, r, k int, span func(r, k int)) { span(r, 1) })},
		{"a compare-and-branch without its X register", `function f(n) { var s = 0; for (var i = 0; i < n; i++) s++; return s; } f(3)`, "JmpLt",
			breakRow(bytecode.JmpLt, func(w uint32, r, k int, span func(r, k int)) {
				if r == a(w) {
					span(r, k)
				}
			})},
		{"an iterator record without its value", `var s = 0; for (var v of [1, 2]) s += v; s`, "IterNext",
			breakRow(bytecode.IterNext, func(w uint32, r, k int, span func(r, k int)) { span(r, k-1) })},
		{"a for-in record without its key", `var ks = ""; for (var k in {a: 1}) ks += k; ks`, "ForInNext",
			breakRow(bytecode.ForInNext, func(w uint32, r, k int, span func(r, k int)) { span(r, k-1) })},
		{"UndefRange one short", `function f() { var p, q; return [p, q].length; } f()`, "UndefRange",
			breakRow(bytecode.UndefRange, func(w uint32, r, k int, span func(r, k int)) { span(r, k-1) })},
		{"an operator without its second operand", `function f(x, y) { return x * y; } f(2, 3)`, "Mul",
			breakRow(bytecode.Mul, func(w uint32, r, k int, span func(r, k int)) {
				if r != int(bytecode.DecodeC(w)) || r == a(w) || r == int(bytecode.DecodeB(w)) {
					span(r, k)
				}
			})},
		{"no row for Add", `function f(x, y) { return x + y; } f(2, 3)`, "no register layout for Add",
			func(o bytecode.Op, w, x uint32, span func(r, k int)) bool {
				return o != bytecode.Add && bytecode.RegisterSpans(o, w, x, span)
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			regSpans = bytecode.RegisterSpans
			require.Empty(t, regcheckRun(t, c.src), "the program runs with the layouts as they are")
			regSpans = c.spans
			msg := regcheckRun(t, c.src)
			regSpans = bytecode.RegisterSpans
			require.NotEmpty(t, msg, "the broken row went unnoticed")
			assert.Contains(t, msg, "engine: regcheck: ")
			assert.Contains(t, msg, c.want)
		})
	}
}

// TestRegCheckWindow checks the window checks themselves: a frame whose
// registers are past the stack, a window kept across a move of the stack,
// a register past NumRegs and a register outside the instruction's layout.
func TestRegCheckWindow(t *testing.T) {
	st := &interpState{stack: make([]Value, 4, 8)}
	move := []uint32{bytecode.EncodeABC(bytecode.Move, 0, 2, 0)}
	code := &bytecode.Function{Name: "f", NumRegs: 3, Code: move}
	panics := func(want string, f func()) {
		t.Helper()
		defer func() {
			p := recover()
			require.NotNil(t, p, want)
			assert.Contains(t, fmt.Sprint(p), want)
		}()
		f()
	}
	panics("past the stack", func() { newCheckedRegs(st, 2, code) })
	c := newCheckedRegs(st, 1, code)
	c.dispatch(move, 0)
	*c.at(0) = *c.at(2)
	panics("leaves out", func() { c.at(1) })
	panics("register 3 of 3", func() { c.at(3) })
	// The stack moves (growStack) and run would go on with its old window.
	st.stack = append(st.stack[:4:4], Value{})
	panics("before the stack moved", func() { c.at(0) })
	c.refresh()
	assert.Same(t, &st.stack[1], c.at(0), "the window taken again is the frame's")
	c.handler(&bytecode.Handler{Kind: bytecode.HandlerFinally, Reg: 1})
	c.at(1)
	c.at(2)
	panics("a handler", func() { c.at(0) })
}

// unverifiedClosure is a function object of code without the verifier:
// a bare tree for hand-made code with no constants, children or inline
// caches.
func unverifiedClosure(r *Realm, code *bytecode.Function) *Object {
	m := &funcMeta{name: AtomEmpty, nfuncs: 1}
	m.root = m
	fn, _ := r.newClosure(code, m, nil, Undefined())
	return fn
}

// TestRegCheckStarts runs functions the verifier rejects, whose jump or
// handler lands on the X word of an immediate compare, which decodes as
// Move R[200], R[0]: the checked run must stop at the dispatch of that
// word, before its register access.
func TestRegCheckStarts(t *testing.T) {
	ret := bytecode.EncodeABC(bytecode.RetUndef, 0, 0, 0)
	x := bytecode.EncodeABC(bytecode.Move, 200, 0, 0)
	jump := &bytecode.Function{Name: "jump", NumRegs: 1, Strict: true, Code: []uint32{
		bytecode.EncodeAsBx(bytecode.Jmp, 0, 1), bytecode.EncodeAsBx(bytecode.JmpLtI, 0, 1), x, ret}}
	catch := &bytecode.Function{Name: "catch", NumRegs: 1, Strict: true, Code: []uint32{
		bytecode.EncodeABC(bytecode.Throw, 0, 0, 0), bytecode.EncodeAsBx(bytecode.JmpLtI, 0, 1), x, ret},
		Handlers: []bytecode.Handler{{Start: 0, End: 1, Handler: 2, Kind: bytecode.HandlerCatch, Reg: 0}}}
	for _, code := range []*bytecode.Function{jump, catch} {
		require.Error(t, bytecode.VerifyRegisters(code), code.Name)
		r := NewRealm()
		fn := unverifiedClosure(r, code)
		msg := func() (msg string) {
			defer func() { msg = fmt.Sprint(recover()) }()
			r.CallObject(fn, Undefined(), nil)
			return
		}()
		assert.Equal(t, fmt.Sprintf("engine: regcheck: %q dispatches pc 2, which starts no instruction", code.Name), msg)
	}
	// With the jump and the handler on the return, both run.
	jump.Code[0] = bytecode.EncodeAsBx(bytecode.Jmp, 0, 2)
	catch.Handlers[0].Handler = 3
	for _, code := range []*bytecode.Function{jump, catch} {
		require.NoError(t, bytecode.VerifyRegisters(code), code.Name)
		r := NewRealm()
		_, err := r.CallObject(unverifiedClosure(r, code), Undefined(), nil)
		assert.NoError(t, err, code.Name)
	}
}

// TestRegCheckKindOperands runs the operands run reads as a kind without a
// check (bytecode.KindOperands) on values of another kind: the verifier
// refuses each program, and run without it, the checked build's payload
// accessors stop each before it reads one kind as another.
func TestRegCheckKindOperands(t *testing.T) {
	abc := bytecode.EncodeABC
	str := abc(bytecode.Typeof, 0, 1, 0) // R[0] = "undefined"
	ret := abc(bytecode.RetUndef, 0, 0, 0)
	for _, c := range []struct {
		name string
		code []uint32
	}{
		{"DefineField on a string", []uint32{str, abc(bytecode.DefineField, 0, 1, 0), 0, ret}},
		{"ArrayPush on a string", []uint32{str, abc(bytecode.ArrayPush, 0, 1, 0), ret}},
		{"ForInNext with string keys", []uint32{
			str, bytecode.EncodeAsBx(bytecode.LoadInt, 1, 0), bytecode.EncodeABx(bytecode.NewObject, 2, 0), bytecode.EncodeAsBx(bytecode.ForInNext, 0, 0), ret}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fn := &bytecode.Function{Name: "f", NumRegs: 8, Strict: true, Code: c.code}
			require.ErrorContains(t, bytecode.VerifyRegisters(fn), "reads register 0 as an")
			r := NewRealm()
			f := unverifiedClosure(r, fn)
			msg := func() (msg string) {
				defer func() { msg = fmt.Sprint(recover()) }()
				r.CallObject(f, Undefined(), nil)
				return
			}()
			assert.Equal(t, "engine: payload: AsObject of a value of another kind", msg)
		})
	}
}

// TestRegCheckWrites breaks one row of bytecode.RegisterWrites at a time
// and runs a program that executes the opcode: the checked run must fail
// on the instruction that writes a register its row leaves out, or that
// leaves another kind than its row gives, a cold op's (IterClose's) too.
// With the rows as they are, each program runs.
func TestRegCheckWrites(t *testing.T) {
	t.Cleanup(func() { regWrites = bytecode.RegisterWrites })
	breakRow := func(op bytecode.Op, row func(w uint32, write func(r, k int, kind bytecode.RegKind))) func(bytecode.Op, uint32, uint32, func(r, k int, kind bytecode.RegKind)) bool {
		return func(o bytecode.Op, w, x uint32, write func(r, k int, kind bytecode.RegKind)) bool {
			if o != op {
				return bytecode.RegisterWrites(o, w, x, write)
			}
			row(w, write)
			return true
		}
	}
	a := func(w uint32) int { return int(bytecode.DecodeA(w)) }
	cases := []struct {
		name, src, want string
		writes          func(bytecode.Op, uint32, uint32, func(r, k int, kind bytecode.RegKind)) bool
	}{
		{"Move writing nothing", `function f() { var x = {}; var y = x; return y === x; } f()`, "Move at",
			breakRow(bytecode.Move, func(uint32, func(r, k int, kind bytecode.RegKind)) {})},
		{"IterClose keeping its record", `for (var v of [1, 2]) break;`, "IterClose at",
			breakRow(bytecode.IterClose, func(uint32, func(r, k int, kind bytecode.RegKind)) {})},
		{"NewObject making an array", `({a: 1}).a`, "a value that is not an array",
			breakRow(bytecode.NewObject, func(w uint32, write func(r, k int, kind bytecode.RegKind)) { write(a(w), 1, bytecode.RegArray) })},
		{"LoadInt making an object", `var n = 1; n`, "a value that is not an object",
			breakRow(bytecode.LoadInt, func(w uint32, write func(r, k int, kind bytecode.RegKind)) { write(a(w), 1, bytecode.RegObject) })},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			regWrites = bytecode.RegisterWrites
			require.Empty(t, regcheckRun(t, c.src), "the program runs with the rows as they are")
			regWrites = c.writes
			msg := regcheckRun(t, c.src)
			regWrites = bytecode.RegisterWrites
			require.NotEmpty(t, msg, "the broken row went unnoticed")
			assert.Contains(t, msg, "engine: regcheck: ")
			assert.Contains(t, msg, c.want)
		})
	}
}

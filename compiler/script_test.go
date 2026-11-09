package compiler

import (
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func compileScript(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	s, err := syntax.ParseScript("s.js", src, syntax.Options{})
	require.NoError(t, err)
	fn, err := CompileScript(s)
	require.NoError(t, err)
	return fn
}

func TestScriptGlobalNames(t *testing.T) {
	fn := compileScript(t, `var v, f; let l; const c = 1; class K {} function f() {} function g() {} function f() {} { function b() {} }`)
	require.NotNil(t, fn.Extra)
	require.NotNil(t, fn.Extra.Globals)
	g := fn.Extra.Globals
	assert.Equal(t, []string{"l", "c", "K"}, g.Lexical)
	assert.Equal(t, []string{"g", "f"}, g.Function, "in the order of the last declarations")
	assert.Equal(t, []string{"v"}, g.Var)
	assert.Equal(t, []string{"b"}, g.AnnexB)
	assert.Zero(t, count(fn, bytecode.GetEnv)+count(fn, bytecode.GetEnv0)+count(fn, bytecode.GetEnv1)+count(fn, bytecode.GetEnv2)+count(fn, bytecode.SetEnv), "top-level declarations are global, not environment slots")

	assert.Nil(t, compileScript(t, `1 + 1`).Extra, "a script without declarations has no GlobalNames")
}

// countDeep counts op in fn and the functions nested in it.
func countDeep(fn *bytecode.Function, op bytecode.Op) int {
	n := count(fn, op)
	for _, c := range fn.Children {
		n += countDeep(c, op)
	}
	return n
}

// TestScriptOps checks which ops sloppy and strict code select.
func TestScriptOps(t *testing.T) {
	tests := []struct {
		name, src string
		has       []bytecode.Op
		not       []bytecode.Op
	}{
		{"sloppy assignment", `x = 1; o.p = 1; o[k] = 1;`,
			[]bytecode.Op{bytecode.SetGlobal, bytecode.SetProp, bytecode.SetElem},
			[]bytecode.Op{bytecode.SetGlobalSloppy, bytecode.SetPropSloppy, bytecode.SetElemSloppy}},
		{"strict assignment", `"use strict"; x = 1; o.p = 1; o[k] = 1;`,
			[]bytecode.Op{bytecode.SetGlobal, bytecode.SetProp, bytecode.SetElem},
			[]bytecode.Op{bytecode.SetGlobalSloppy, bytecode.SetPropSloppy, bytecode.SetElemSloppy}},
		{"sloppy delete", `delete o.p; delete o[k]; delete x;`,
			[]bytecode.Op{bytecode.DelPropSloppy, bytecode.DelElemSloppy, bytecode.DelGlobal},
			[]bytecode.Op{bytecode.DelProp, bytecode.DelElem}},
		{"let initialization", `let a = 1; const b = 2;`,
			[]bytecode.Op{bytecode.InitGlobal}, []bytecode.Op{bytecode.SetGlobal, bytecode.SetGlobalSloppy}},
		{"with", `with (o) a = b;`,
			[]bytecode.Op{bytecode.ToObject, bytecode.JmpWith, bytecode.WithGet, bytecode.WithSet}, nil},
		{"sloppy super", `({ m() { super.p = 1; [super.q] = [1]; } });`,
			[]bytecode.Op{bytecode.SetSuperSloppy}, []bytecode.Op{bytecode.SetSuper}},
		{"annex b copy", `{ function f() {} }`,
			[]bytecode.Op{bytecode.SetGlobalVar}, nil},
		{"no with ops outside with", `a = b; typeof c; c();`,
			nil, []bytecode.Op{bytecode.JmpWith, bytecode.WithGet, bytecode.WithSet}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fn := compileScript(t, tc.src)
			for _, op := range tc.has {
				assert.Positive(t, countDeep(fn, op), "missing %v", op)
			}
			for _, op := range tc.not {
				assert.Zero(t, countDeep(fn, op), "unexpected %v", op)
			}
		})
	}
	// A named write is SetProp in both, with SetPropSloppyFlag in sloppy
	// code.
	for src, want := range map[string]uint8{`o.p = 1;`: bytecode.SetPropSloppyFlag, `"use strict"; o.p = 1;`: 0} {
		var flags []uint8
		code := compileScript(t, src).Code
		for pc := 0; pc < len(code); pc += 1 + bytecode.DecodeOp(code[pc]).ExtraWords() {
			if w := code[pc]; bytecode.DecodeOp(w) == bytecode.SetProp {
				flags = append(flags, bytecode.DecodeC(w))
			}
		}
		assert.Equal(t, []uint8{want}, flags, src)
	}
}

// TestSloppyFunctionPrologue checks that only sloppy functions that use
// this or arguments get CoerceThis and MapArguments, and that CoerceThis
// loads this into its register, where LoadThis does in strict code.
func TestSloppyFunctionPrologue(t *testing.T) {
	tests := []struct {
		name, src            string
		coerce, mapped, load int
	}{
		{"plain", `function f(a) { return a; }`, 0, 0, 0},
		{"this", `function f() { return this; }`, 1, 0, 0},
		{"this twice", `function f() { return [this, this.x]; }`, 1, 0, 0},
		{"arguments", `function f(a) { return arguments; }`, 0, 1, 0},
		{"this and arguments", `function f(a) { return [this, arguments]; }`, 1, 1, 0},
		{"strict", `function f(a) { "use strict"; return [this, arguments]; }`, 0, 0, 1},
		{"non-simple params", `function f(a = 1) { return arguments; }`, 0, 0, 0},
		{"arrow this", `function f() { return () => this; }`, 1, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := child(t, compileScript(t, tc.src), "f")
			assert.Equal(t, tc.coerce, count(f, bytecode.CoerceThis))
			assert.Equal(t, tc.mapped, count(f, bytecode.MapArguments))
			assert.Equal(t, tc.load, count(f, bytecode.LoadThis))
		})
	}
}

// TestLazyMappedArguments checks MapArguments' B operand: 1 (alias lazily)
// only when no code can write a parameter and parameter i is Env slot i.
func TestLazyMappedArguments(t *testing.T) {
	tests := []struct {
		name, src string
		lazy      bool
	}{
		{"read only", `function f(a, b) { return [arguments, arguments[0] + a + b]; }`, true},
		{"no params", `function f() { return [arguments, arguments.length]; }`, true},
		{"captured read", `function f(a) { return () => a + arguments[0]; }`, true},
		{"assign", `function f(a) { a = 1; return arguments; }`, false},
		{"compound", `function f(a) { a += 1; return arguments; }`, false},
		{"update", `function f(a) { a++; return arguments; }`, false},
		{"closure", `function f(a) { (() => { a = 1; })(); return arguments; }`, false},
		{"nested function", `function f(a) { function g() { a = 1; } return arguments; }`, false},
		{"destructuring", `function f(a) { [a] = [1]; return arguments; }`, false},
		{"object destructuring", `function f(a) { ({a} = {a: 1}); return arguments; }`, false},
		{"for-in", `function f(a) { for (a in {x: 1}); return arguments; }`, false},
		{"for-of", `function f(a) { for (a of [1]); return arguments; }`, false},
		{"for var-in", `function f(a) { for (var a in {x: 1}); return arguments; }`, false},
		{"var init", `function f(a) { var a = 1; return arguments; }`, false},
		{"var no init", `function f(a) { var a; return arguments; }`, true},
		{"function declaration", `function f(a) { function a() {} return arguments; }`, false},
		{"with", `function f(a) { with ({}) a = 1; return arguments; }`, false},
		{"direct eval", `function f(a) { eval(""); return arguments; }`, false},
		{"eval in closure", `function f(a) { return () => eval("a = 1") && arguments; }`, false},
		{"duplicate params", `function f(a, a) { return arguments; }`, false},
		{"other binding", `function f(a) { var b = 1; b = 2; return arguments; }`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := child(t, compileScript(t, tc.src), "f")
			n := 0
			for pc := 0; pc < len(f.Code); pc += 1 + bytecode.DecodeOp(f.Code[pc]).ExtraWords() {
				if w := f.Code[pc]; bytecode.DecodeOp(w) == bytecode.MapArguments {
					n++
					assert.Equal(t, tc.lazy, bytecode.DecodeB(w) == 1)
				}
			}
			assert.Equal(t, 1, n)
		})
	}
}

// TestModulesPayNothingForSloppy checks that module code never selects a
// sloppy-mode op.
func TestModulesPayNothingForSloppy(t *testing.T) {
	fn := compileModule(t, `let x; export function f(o, k) { x = 1; o.p = 1; o[k] = 1; delete o.p; delete o[k]; ({ m() { super.p = 1; } }); return [this, arguments]; }`)
	for _, op := range []bytecode.Op{
		bytecode.SetGlobalSloppy, bytecode.SetPropSloppy, bytecode.SetElemSloppy, bytecode.DelPropSloppy,
		bytecode.DelElemSloppy, bytecode.DelGlobal, bytecode.InitGlobal, bytecode.CoerceThis, bytecode.MapArguments,
		bytecode.SetSuperSloppy,
	} {
		assert.Zero(t, countDeep(fn, op), "unexpected %v", op)
	}
	assert.Nil(t, fn.Extra)
}

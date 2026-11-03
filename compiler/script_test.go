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
	assert.Zero(t, count(fn, bytecode.GetEnv)+count(fn, bytecode.SetEnv), "top-level declarations are global, not environment slots")

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
			[]bytecode.Op{bytecode.SetGlobalSloppy, bytecode.SetPropSloppy, bytecode.SetElemSloppy},
			[]bytecode.Op{bytecode.SetGlobal, bytecode.SetProp, bytecode.SetElem}},
		{"strict assignment", `"use strict"; x = 1; o.p = 1; o[k] = 1;`,
			[]bytecode.Op{bytecode.SetGlobal, bytecode.SetProp, bytecode.SetElem},
			[]bytecode.Op{bytecode.SetGlobalSloppy, bytecode.SetPropSloppy, bytecode.SetElemSloppy}},
		{"sloppy delete", `delete o.p; delete o[k]; delete x;`,
			[]bytecode.Op{bytecode.DelPropSloppy, bytecode.DelElemSloppy, bytecode.DelGlobal},
			[]bytecode.Op{bytecode.DelProp, bytecode.DelElem}},
		{"let initialization", `let a = 1; const b = 2;`,
			[]bytecode.Op{bytecode.InitGlobal}, []bytecode.Op{bytecode.SetGlobalSloppy}},
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
}

// TestSloppyFunctionPrologue checks that only sloppy functions that use
// this or arguments get CoerceThis and MapArguments.
func TestSloppyFunctionPrologue(t *testing.T) {
	tests := []struct {
		name, src      string
		coerce, mapped int
	}{
		{"plain", `function f(a) { return a; }`, 0, 0},
		{"this", `function f() { return this; }`, 1, 0},
		{"arguments", `function f(a) { return arguments; }`, 0, 1},
		{"strict", `function f(a) { "use strict"; return [this, arguments]; }`, 0, 0},
		{"non-simple params", `function f(a = 1) { return arguments; }`, 0, 0},
		{"arrow this", `function f() { return () => this; }`, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := child(t, compileScript(t, tc.src), "f")
			assert.Equal(t, tc.coerce, count(f, bytecode.CoerceThis))
			assert.Equal(t, tc.mapped, count(f, bytecode.MapArguments))
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

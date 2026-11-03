package compiler

import (
	"math"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/engine"
	"github.com/stretchr/testify/assert"
)

func count(fn *bytecode.Function, op bytecode.Op) int {
	n := 0
	for _, o := range ops(fn) {
		if o == op {
			n++
		}
	}
	return n
}

func TestClassTemplates(t *testing.T) {
	fn := compileModule(t, `
export class A { x = 1; #p; m() { return this.#p; } static s = 2; }
export class B extends A { constructor() { if (this) return; super(); try { return 1; } finally {} } }
export class C extends A {}
`)
	a := child(t, fn, "A")
	assert.Equal(t, bytecode.KindClassCtor, a.Kind)
	assert.True(t, a.NewTarget)
	assert.Equal(t, bytecode.CtorEntry, ops(a)[0], "a call without new throws before any JavaScript runs")
	assert.Contains(t, ops(fn), bytecode.CreateClass)
	assert.Contains(t, ops(fn), bytecode.NewPrivateName)
	fields := child(t, fn, "<instance_members_initializer>")
	assert.Equal(t, bytecode.KindMethod, fields.Kind)
	assert.Contains(t, ops(fields), bytecode.DefPrivate)
	assert.Equal(t, bytecode.KindMethod, child(t, fn, "<static_initializer>").Kind)

	b := child(t, fn, "B")
	assert.Equal(t, bytecode.KindDerivedCtor, b.Kind)
	assert.Equal(t, 1, count(b, bytecode.DerivedResult), "every return goes through the shared epilogue")
	assert.Equal(t, 1, count(b, bytecode.Ret))
	assert.Contains(t, ops(b), bytecode.SuperCall)
	assert.Contains(t, ops(b), bytecode.CheckSuper)

	c := child(t, fn, "C")
	assert.Equal(t, bytecode.KindDerivedCtor, c.Kind)
	assert.True(t, c.HasRest, "the default derived constructor forwards a rest array")
	assert.Contains(t, ops(c), bytecode.SuperCallSpread)

	fn = compileModule(t, `export class D { constructor(x) { this.x = x; } }`)
	assert.Equal(t, []bytecode.Op{bytecode.CtorEntry, bytecode.LoadThis, bytecode.Move, bytecode.SetProp, bytecode.RetUndef}, ops(child(t, fn, "D")),
		"a class without instance fields calls no initializer")
}

// TestClassFreeCodePaysNothing checks that functions that use none of
// new.target, super or private names compile as before.
func TestClassFreeCodePaysNothing(t *testing.T) {
	fn := compileModule(t, `
export function plain(a) { return this.x + a; }
export function nt() { return new.target; }
export const o = { m() { return 1; }, n() { return super.n; } };
`)
	plain := child(t, fn, "plain")
	assert.False(t, plain.NewTarget)
	for _, op := range ops(plain) {
		assert.Less(t, op, bytecode.CtorEntry, "no class op in %v", op)
	}
	nt := child(t, fn, "nt")
	assert.True(t, nt.NewTarget)
	assert.Contains(t, ops(nt), bytecode.LoadNewTarget)
	assert.Equal(t, 1, count(fn, bytecode.SetHome), "only the method that reads super gets a home object")
	assert.Contains(t, ops(child(t, fn, "n")), bytecode.LoadHome)
	assert.NotContains(t, ops(child(t, fn, "m")), bytecode.LoadHome)
}

// TestNumberKeyString checks that numeric literal keys (and the names of
// methods and accessors keyed by them) are spelled as Number::toString
// spells them.
func TestNumberKeyString(t *testing.T) {
	for _, v := range []float64{
		0, 1, -1, 7, 1.5, -1.5, 0.1, 0.5, 1e-6, 1e-7, 1.25e-7, 123e-20,
		1e20, 1e21, 1.5e21, 123456789012345680000, 1 << 53, 1<<53 + 2, 1e300, 5e-324,
		math.MaxFloat64, 0.000001234, 100, 1e15, 1e16, 4294967295, 4294967296,
		math.Inf(1), math.Inf(-1), math.NaN(),
	} {
		assert.Equal(t, engine.NumberToGoString(v), numberKeyString(v), "%v", v)
	}
	fn := compileModule(t, `export class C { get 0.0000001() {} static 1e21() {} }`)
	assert.Equal(t, "get 1e-7", child(t, fn, "get 1e-7").Name)
	assert.Equal(t, "1e+21", child(t, fn, "1e+21").Name)
}

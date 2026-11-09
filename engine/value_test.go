package engine

import (
	"math"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// is64 reports a 64-bit system: the sizes and size classes that tests pin
// are those of its layouts.
const is64 = unsafe.Sizeof(uintptr(0)) == 8

func TestValueSize(t *testing.T) {
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(16), unsafe.Sizeof(Value{}))
	}
}

func TestValuePredicates(t *testing.T) {
	r := NewRealm()
	s := FromGoString("abc")
	o := r.NewObject()
	b, ok := NewBigIntFromDecimal("42")
	require.True(t, ok)
	cases := []struct {
		name string
		v    Value
		typ  Type
	}{
		{"number", NumberValue(1.5), TypeNumber},
		{"zero", Value{}, TypeNumber},
		{"negzero", NumberValue(math.Copysign(0, -1)), TypeNumber},
		{"nan", NaN(), TypeNumber},
		{"inf", NumberValue(math.Inf(1)), TypeNumber},
		{"undefined", Undefined(), TypeUndefined},
		{"null", Null(), TypeNull},
		{"true", True(), TypeBoolean},
		{"false", False(), TypeBoolean},
		{"hole", Hole(), TypeHole},
		{"string", StringValue(s), TypeString},
		{"object", ObjectValue(o), TypeObject},
		{"symbol", SymbolValue(SymIterator), TypeSymbol},
		{"bigint", BigIntValue(b), TypeBigInt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.typ, c.v.Type())
			assert.Equal(t, c.typ == TypeNumber, c.v.IsNumber())
			assert.Equal(t, c.typ == TypeString, c.v.IsString())
			assert.Equal(t, c.typ == TypeObject, c.v.IsObject())
			assert.Equal(t, c.typ == TypeUndefined, c.v.IsUndefined())
			assert.Equal(t, c.typ == TypeNull, c.v.IsNull())
			assert.Equal(t, c.typ == TypeBoolean, c.v.IsBool())
			assert.Equal(t, c.typ == TypeHole, c.v.IsHole())
			assert.Equal(t, c.typ == TypeSymbol, c.v.IsSymbol())
			assert.Equal(t, c.typ == TypeBigInt, c.v.IsBigInt())
			assert.Equal(t, c.typ != TypeObject, c.v.IsPrimitive())
		})
	}
	assert.Same(t, s, StringValue(s).AsString())
	assert.Same(t, o, ObjectValue(o).AsObject())
	assert.True(t, True().AsBool())
	assert.False(t, False().AsBool())
}

func TestNaNCanonicalization(t *testing.T) {
	// A NaN whose payload lands in the reserved space must be folded.
	evil := math.Float64frombits(reservedPrefix | 2) // would alias undefined
	v := NumberValue(evil)
	assert.True(t, v.IsNumber())
	assert.False(t, v.IsUndefined())
	assert.True(t, math.IsNaN(v.AsNumber()))
	assert.Equal(t, canonicalNaN, v.Bits())
	assert.Equal(t, canonicalNaN, NumberValue(math.NaN()).Bits())
	assert.Equal(t, canonicalNaN, NaN().Bits())

	// Negative zero survives.
	nz := NumberValue(math.Copysign(0, -1))
	assert.True(t, math.Signbit(nz.AsNumber()))
	assert.NotEqual(t, nz.Bits(), NumberValue(0).Bits())
}

// TestScalarFloatNaN checks what run's arithmetic relies on (scalarFloat):
// every scalar that is not a number reads as a NaN, and so does an
// operator's result on it, whatever the other operand.
func TestScalarFloatNaN(t *testing.T) {
	others := []Value{Undefined(), Null(), True(), False(), Hole()}
	nums := []float64{0, math.Copysign(0, -1), 1, -1, 0.5, 1 << 53, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1), math.NaN()}
	for _, v := range others {
		require.True(t, v.isScalar())
		f := v.scalarFloat()
		require.True(t, math.IsNaN(f), "%v", v)
		for _, n := range nums {
			for _, r := range []float64{f + n, n + f, f - n, n - f, f * n, n * f, f / n, n / f, f + 1, f - 1} {
				assert.True(t, math.IsNaN(r), "%v and %v", v, n)
			}
			assert.False(t, f < n || f <= n || f > n || f >= n || n < f || n <= f || n > f || n >= f, "%v and %v", v, n)
		}
		i, i32 := int(f), int32(f)
		assert.False(t, float64(i) == f || float64(i32) == f, "%v", v)
	}
	for _, n := range nums {
		v := NumberValue(n)
		assert.True(t, v.isScalar())
		assert.Equal(t, v.Bits(), math.Float64bits(v.scalarFloat()))
	}
	for _, v := range []Value{StringValue(FromGoString("s")), ObjectValue(NewRealm().NewObject())} {
		assert.False(t, v.isScalar())
	}
}

func TestValueIntHelpers(t *testing.T) {
	i, ok := NumberValue(42).IsInt32()
	assert.True(t, ok)
	assert.Equal(t, int32(42), i)
	_, ok = NumberValue(1.5).IsInt32()
	assert.False(t, ok)
	_, ok = NumberValue(math.Copysign(0, -1)).IsInt32()
	assert.False(t, ok)
	_, ok = NumberValue(1 << 40).IsInt32()
	assert.False(t, ok)

	idx, ok := NumberValue(4294967294).IsArrayIndex()
	assert.True(t, ok)
	assert.Equal(t, uint32(4294967294), idx)
	_, ok = NumberValue(4294967295).IsArrayIndex()
	assert.False(t, ok)
	_, ok = NumberValue(-1).IsArrayIndex()
	assert.False(t, ok)
	assert.Equal(t, Undefined(), Arg(nil, 3))
	assert.Equal(t, IntValue(7), Arg([]Value{IntValue(7)}, 0))
}

func TestValueDebugString(t *testing.T) {
	r := NewRealm()
	assert.Equal(t, "undefined", Undefined().String())
	assert.Equal(t, "null", Null().String())
	assert.Equal(t, "1.5", NumberValue(1.5).String())
	assert.Equal(t, "abc", StringValue(FromGoString("abc")).String())
	assert.Equal(t, "[object Object]", ObjectValue(r.NewObject()).String())
	assert.Equal(t, "[object Array]", ObjectValue(r.NewArray()).String())
	assert.Equal(t, "function Object() { [native code] }", ObjectValue(r.ObjectCtor).String())
	b, _ := NewBigIntFromDecimal("12")
	assert.Equal(t, "12n", BigIntValue(b).String())
	assert.Equal(t, "Symbol(Symbol.iterator)", SymbolValue(SymIterator).String())
}

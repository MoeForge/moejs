package engine

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNumberConstants(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.NumberCtor)
	assertNativeShape(t, r, ctor, "Number", 1)
	for _, c := range []struct {
		name string
		want float64
	}{
		{"EPSILON", 2.220446049250313e-16},
		{"MAX_SAFE_INTEGER", 9007199254740991},
		{"MIN_SAFE_INTEGER", -9007199254740991},
		{"MAX_VALUE", 1.7976931348623157e308},
		{"MIN_VALUE", 5e-324},
		{"NaN", math.NaN()},
		{"POSITIVE_INFINITY", posInf},
		{"NEGATIVE_INFINITY", negInf},
	} {
		assertOwnAttrs(t, r, r.NumberCtor, c.name, false, false, false)
		got := jsNumber(t, jsGet(t, r, ctor, c.name))
		assert.True(t, sameNumber(c.want, got), "%s = %v", c.name, got)
	}
	assert.Equal(t, 1.0, jsNumber(t, jsGet(t, r, ctor, "EPSILON"))+1-jsNumber(t, jsGet(t, r, ctor, "EPSILON")))
	assert.Same(t, r.NumberPrototype, jsGet(t, r, ctor, "prototype").AsObject())
	assert.Same(t, r.NumberCtor, jsGet(t, r, ObjectValue(r.NumberPrototype), "constructor").AsObject())
	for _, c := range []struct {
		name   string
		length int
	}{{"isFinite", 1}, {"isInteger", 1}, {"isNaN", 1}, {"isSafeInteger", 1}, {"parseFloat", 1}, {"parseInt", 2}} {
		assertNativeShape(t, r, jsGet(t, r, ctor, c.name), c.name, c.length)
		assertOwnAttrs(t, r, r.NumberCtor, c.name, true, false, true)
	}
	for _, c := range []struct {
		name   string
		length int
	}{{"toExponential", 1}, {"toFixed", 1}, {"toLocaleString", 0}, {"toPrecision", 1}, {"toString", 1}, {"valueOf", 0}} {
		assertNativeShape(t, r, jsGet(t, r, ObjectValue(r.NumberPrototype), c.name), c.name, c.length)
	}
}

func TestNumberStaticPredicates(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.NumberCtor)
	cases := []struct {
		in                                        Value
		isFinite, isInteger, isSafeInteger, isNaN bool
	}{
		{IntValue(0), true, true, true, false},
		{negZero(), true, true, true, false},
		{IntValue(-7), true, true, true, false},
		{NumberValue(1.5), true, false, false, false},
		{NumberValue(9007199254740991), true, true, true, false},
		{NumberValue(9007199254740992), true, true, false, false},
		{NumberValue(-9007199254740992), true, true, false, false},
		{NumberValue(1e300), true, true, false, false},
		{NumberValue(5e-324), true, false, false, false},
		{NaN(), false, false, false, true},
		{NumberValue(posInf), false, false, false, false},
		{NumberValue(negInf), false, false, false, false},
		{str("5"), false, false, false, false},
		{str("NaN"), false, false, false, false},
		{True(), false, false, false, false},
		{Null(), false, false, false, false},
		{Undefined(), false, false, false, false},
		{ObjectValue(r.NewObject()), false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.in.String(), func(t *testing.T) {
			assert.Equal(t, Bool(c.isFinite), jsCall(t, r, ctor, "isFinite", c.in), "isFinite")
			assert.Equal(t, Bool(c.isInteger), jsCall(t, r, ctor, "isInteger", c.in), "isInteger")
			assert.Equal(t, Bool(c.isSafeInteger), jsCall(t, r, ctor, "isSafeInteger", c.in), "isSafeInteger")
			assert.Equal(t, Bool(c.isNaN), jsCall(t, r, ctor, "isNaN", c.in), "isNaN")
		})
	}
	// No coercion: an object with valueOf is never a number.
	var log []string
	o := objWithValueOf(r, IntValue(1), "v", &log)
	assert.Equal(t, False(), jsCall(t, r, ctor, "isInteger", o))
	assert.Empty(t, log)
	assert.Equal(t, False(), jsCall(t, r, ctor, "isNaN"))
}

func TestNumberConstructor(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.NumberCtor)
	call := func(args ...Value) Value {
		v, err := r.Call(ctor, Undefined(), args)
		require.NoError(t, err)
		return v
	}
	assert.Equal(t, IntValue(0), call())
	assert.Equal(t, IntValue(12), call(str(" 12 ")))
	assert.Equal(t, IntValue(31), call(str("0x1f")))
	assert.True(t, math.IsNaN(jsNumber(t, call(str("12px")))))
	assert.Equal(t, IntValue(1), call(True()))
	assert.Equal(t, IntValue(0), call(Null()))
	assert.True(t, math.IsNaN(jsNumber(t, call(Undefined()))))
	b, _ := NewBigIntFromDecimal("12345678901234567890")
	assert.Equal(t, 1.2345678901234567e19, jsNumber(t, call(BigIntValue(b))))
	_, err := r.Call(ctor, Undefined(), []Value{SymbolValue(SymIterator)})
	assert.ErrorContains(t, err, "Symbol")
	var log []string
	assert.Equal(t, IntValue(9), call(objWithValueOf(r, IntValue(9), "v", &log)))
	assert.Equal(t, []string{"v"}, log)

	w, err := r.Construct(ctor, []Value{str("2.5")}, nil)
	require.NoError(t, err)
	assert.Equal(t, ClassNumber, w.AsObject().Class())
	assert.Same(t, r.NumberPrototype, w.AsObject().Proto())
	pv, _ := w.AsObject().PrimitiveValue()
	assert.Equal(t, NumberValue(2.5), pv)
	assert.Equal(t, NumberValue(2.5), jsCall(t, r, w, "valueOf"))
	assert.Equal(t, "2.5", jsString(t, jsCall(t, r, w, "toString")))
	assert.Equal(t, "Number", w.AsObject().ClassName())
	// Receiver checks.
	for _, m := range []string{"toString", "valueOf", "toFixed", "toPrecision", "toExponential", "toLocaleString"} {
		fn := jsGet(t, r, ObjectValue(r.NumberPrototype), m)
		_, err := r.Call(fn, str("1"), nil)
		assert.EqualError(t, err, "TypeError: Number.prototype."+m+" requires that 'this' be a Number", m)
		_, err = r.Call(fn, ObjectValue(r.NewObject()), nil)
		assert.Error(t, err, m)
	}
	assert.Equal(t, "0", jsString(t, jsCall(t, r, ObjectValue(r.NumberPrototype), "toString")), "Number.prototype is a Number wrapper of 0")
}

func TestNumberToFixed(t *testing.T) {
	r := NewRealm()
	cases := []struct {
		x    float64
		d    Value
		want string
	}{
		{0, Undefined(), "0"},
		{0, IntValue(2), "0.00"},
		{math.Copysign(0, -1), IntValue(2), "0.00"},
		{1, IntValue(0), "1"},
		{1.005, IntValue(2), "1.00"}, // 1.005 is below the tie in binary
		{1.255, IntValue(2), "1.25"},
		{0.5, IntValue(0), "1"}, // exact ties round to the larger value
		{1.5, IntValue(0), "2"},
		{2.5, IntValue(0), "3"},
		{-2.5, IntValue(0), "-3"},
		{-1.5, IntValue(0), "-2"},
		{0.125, IntValue(2), "0.13"},
		{0.45, IntValue(1), "0.5"},
		{1.45, IntValue(1), "1.4"},
		{2.345, IntValue(2), "2.35"},
		{8.345, IntValue(2), "8.35"},
		{123.456, IntValue(1), "123.5"},
		{1234.5678, IntValue(2), "1234.57"},
		{0.1, IntValue(20), "0.10000000000000000555"},
		{pointOne + pointTwo, IntValue(17), "0.30000000000000004"},
		{pointOne + pointTwo, IntValue(2), "0.30"},
		{0.000001, IntValue(7), "0.0000010"},
		{1e-10, IntValue(3), "0.000"},
		{-1e-10, IntValue(3), "-0.000"},
		{1e20, IntValue(0), "100000000000000000000"},
		{1e20, IntValue(2), "100000000000000000000.00"},
		{1e21, IntValue(2), "1e+21"},
		{-1e21, IntValue(2), "-1e+21"},
		{1.5e300, IntValue(0), "1.5e+300"},
		{123, IntValue(100), "123." + string(make([]byte, 0)) + "0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"},
		{9007199254740993, IntValue(0), "9007199254740992"},
		{4.35, IntValue(1), "4.3"},
		{-4.35, IntValue(1), "-4.3"},
		{99.995, IntValue(2), "100.00"}, // 99.995 is above the tie in binary
		{99.5, IntValue(0), "100"},
		{9.99, IntValue(1), "10.0"},
		{0.96, IntValue(1), "1.0"},
		{999.999, IntValue(2), "1000.00"},
		{0.995, IntValue(2), "0.99"}, // shortest form ends in 5: exact path, and the binary value is below the tie
		{1.015, IntValue(2), "1.01"},
		{1.025, IntValue(2), "1.02"},
		{1.035, IntValue(2), "1.03"},
		{1.045, IntValue(2), "1.04"},
		{1.055, IntValue(2), "1.05"}, // 1.055 is below the tie in binary
		{8.005, IntValue(2), "8.01"},
		{1e15 + 0.3, IntValue(2), "1000000000000000.25"},
		{123456789.987654321, IntValue(3), "123456789.988"},
		{0.000001234, IntValue(8), "0.00000123"},
		{5e-7, IntValue(6), "0.000000"}, // 5e-7 is below the tie in binary
		{4.5e-7, IntValue(6), "0.000000"},
		{1.5e-7, IntValue(7), "0.0000001"},
		{0.0000001, NumberValue(2.9), "0.00"},
		{1.5, str("1"), "1.5"},
		{1.5, Null(), "2"},
		{1.5, NaN(), "2"},
		{math.NaN(), IntValue(2), "NaN"},
		{posInf, IntValue(2), "Infinity"},
		{negInf, IntValue(2), "-Infinity"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, jsString(t, jsCall(t, r, NumberValue(c.x), "toFixed", c.d)))
		})
	}
	for _, bad := range []Value{IntValue(101), IntValue(-1), NumberValue(posInf), NumberValue(negInf)} {
		err := jsCallErr(t, r, IntValue(1), "toFixed", bad)
		assert.EqualError(t, err, "RangeError: toFixed() digits argument must be between 0 and 100", bad.String())
		// The digits check precedes the finite check.
		err = jsCallErr(t, r, NaN(), "toFixed", bad)
		assert.ErrorContains(t, err, "RangeError", bad.String())
	}
	assert.ErrorContains(t, jsCallErr(t, r, IntValue(1), "toFixed", SymbolValue(SymIterator)), "Symbol")
	w, _ := r.Construct(ObjectValue(r.NumberCtor), []Value{NumberValue(2.5)}, nil)
	assert.Equal(t, "2.50", jsString(t, jsCall(t, r, w, "toFixed", IntValue(2))))
}

func TestNumberToExponential(t *testing.T) {
	r := NewRealm()
	cases := []struct {
		x    float64
		d    Value
		want string
	}{
		{0, Undefined(), "0e+0"},
		{0, IntValue(2), "0.00e+0"},
		{math.Copysign(0, -1), IntValue(1), "0.0e+0"},
		{1, Undefined(), "1e+0"},
		{1, IntValue(0), "1e+0"},
		{1, IntValue(3), "1.000e+0"},
		{123456, Undefined(), "1.23456e+5"},
		{123456, IntValue(2), "1.23e+5"},
		{123456, IntValue(0), "1e+5"},
		{0.000123, Undefined(), "1.23e-4"},
		{0.000123, IntValue(1), "1.2e-4"},
		{1.5, IntValue(0), "2e+0"},
		{2.5, IntValue(0), "3e+0"},
		{-2.5, IntValue(0), "-3e+0"},
		{0.125, IntValue(1), "1.3e-1"},
		{9.995, IntValue(2), "9.99e+0"}, // below the tie in binary
		{99.5, IntValue(1), "1.0e+2"},   // exact tie carries into a new digit
		{999.5, IntValue(2), "1.00e+3"},
		{-1.5e-7, IntValue(3), "-1.500e-7"},
		{1e21, IntValue(3), "1.000e+21"},
		{1e21, Undefined(), "1e+21"},
		{pointOne + pointTwo, Undefined(), "3.0000000000000004e-1"},
		{pointOne + pointTwo, IntValue(20), "3.00000000000000044409e-1"},
		{1.7976931348623157e308, IntValue(2), "1.80e+308"},
		{5e-324, IntValue(2), "4.94e-324"},
		{5e-324, Undefined(), "5e-324"},
		{12345, IntValue(100), "1.2345" + string(make([]byte, 0)) + "000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000e+4"},
		{123, NumberValue(1.9), "1.2e+2"},
		{123, Null(), "1e+2"},
		{math.NaN(), IntValue(2), "NaN"},
		{math.NaN(), IntValue(200), "NaN"}, // finite check precedes the range check
		{posInf, IntValue(-5), "Infinity"},
		{negInf, Undefined(), "-Infinity"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, jsString(t, jsCall(t, r, NumberValue(c.x), "toExponential", c.d)))
		})
	}
	for _, bad := range []Value{IntValue(101), IntValue(-1), NumberValue(posInf)} {
		err := jsCallErr(t, r, IntValue(1), "toExponential", bad)
		assert.EqualError(t, err, "RangeError: toExponential() argument must be between 0 and 100", bad.String())
	}
}

func TestNumberToPrecision(t *testing.T) {
	r := NewRealm()
	cases := []struct {
		x    float64
		p    Value
		want string
	}{
		{0, IntValue(1), "0"},
		{0, IntValue(3), "0.00"},
		{math.Copysign(0, -1), IntValue(2), "0.0"},
		{123.456, IntValue(4), "123.5"},
		{123.456, IntValue(3), "123"},
		{123.456, IntValue(6), "123.456"},
		{123.456, IntValue(2), "1.2e+2"},
		{123.456, IntValue(1), "1e+2"},
		{123456, IntValue(2), "1.2e+5"},
		{0.000123, IntValue(2), "0.00012"},
		{0.00000123, IntValue(2), "0.0000012"},
		{0.000000123, IntValue(2), "1.2e-7"},
		{1.5, IntValue(1), "2"},
		{2.5, IntValue(1), "3"},
		{25, IntValue(1), "3e+1"},
		{-25, IntValue(1), "-3e+1"},
		{99.99, IntValue(2), "1.0e+2"},
		{0.5, IntValue(1), "0.5"},
		{1e21, IntValue(3), "1.00e+21"},
		{1e21, IntValue(22), "1000000000000000000000"},
		{1e-7, IntValue(1), "1e-7"},
		{1e-6, IntValue(1), "0.000001"},
		{5e-324, IntValue(3), "4.94e-324"},
		{1.7976931348623157e308, IntValue(5), "1.7977e+308"},
		{pointOne + pointTwo, IntValue(17), "0.30000000000000004"},
		{pointOne + pointTwo, IntValue(21), "0.300000000000000044409"},
		{1, IntValue(100), "1." + string(make([]byte, 0)) + "000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"},
		{123.456, Undefined(), "123.456"},
		{123.456, NumberValue(4.7), "123.5"},
		{123.456, str("2"), "1.2e+2"},
		{math.NaN(), IntValue(2), "NaN"},
		{math.NaN(), IntValue(0), "NaN"}, // finite check precedes the range check
		{posInf, IntValue(200), "Infinity"},
		{negInf, Undefined(), "-Infinity"},
		{math.NaN(), Undefined(), "NaN"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, jsString(t, jsCall(t, r, NumberValue(c.x), "toPrecision", c.p)))
		})
	}
	for _, bad := range []Value{IntValue(0), IntValue(101), IntValue(-1), NumberValue(posInf), Null()} {
		err := jsCallErr(t, r, IntValue(1), "toPrecision", bad)
		assert.EqualError(t, err, "RangeError: toPrecision() argument must be between 1 and 100", bad.String())
	}
}

func TestNumberToStringRadixMethod(t *testing.T) {
	r := NewRealm()
	cases := []struct {
		x     float64
		radix Value
		want  string
	}{
		{255, Undefined(), "255"},
		{255, IntValue(10), "255"},
		{255, IntValue(16), "ff"},
		{255, IntValue(2), "11111111"},
		{-255, IntValue(16), "-ff"},
		{255, NumberValue(16.9), "ff"},
		{255, str("16"), "ff"},
		{35, IntValue(36), "z"},
		{0.5, IntValue(2), "0.1"},
		{0.1, IntValue(16), "0.1999999999999a"},
		{3.75, IntValue(2), "11.11"},
		{1e21, Undefined(), "1e+21"},
		{1e21, IntValue(16), "3635c9adc5dea00000"},
		{pointOne + pointTwo, Undefined(), "0.30000000000000004"},
		{math.Copysign(0, -1), IntValue(2), "0"},
		{math.NaN(), IntValue(16), "NaN"},
		{posInf, IntValue(2), "Infinity"},
		{negInf, IntValue(36), "-Infinity"},
		{1e-7, Undefined(), "1e-7"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, jsString(t, jsCall(t, r, NumberValue(c.x), "toString", c.radix)))
		})
	}
	for _, bad := range []Value{IntValue(1), IntValue(37), IntValue(0), NaN(), Null(), NumberValue(posInf)} {
		err := jsCallErr(t, r, IntValue(1), "toString", bad)
		assert.EqualError(t, err, "RangeError: toString() radix must be between 2 and 36", bad.String())
	}
	assert.Equal(t, "1,234.5", jsString(t, jsCall(t, r, NumberValue(1234.5), "toLocaleString")))
	assert.Equal(t, "1.234,5", jsString(t, jsCall(t, r, NumberValue(1234.5), "toLocaleString", str("de-DE"))))
	w, _ := r.Construct(ObjectValue(r.NumberCtor), []Value{IntValue(10)}, nil)
	assert.Equal(t, "1010", jsString(t, jsCall(t, r, w, "toString", IntValue(2))))
	s, err := r.ToString(w)
	require.NoError(t, err)
	assert.Equal(t, "10", s.GoString())
}

// TestToFixedOneAlloc: toFixed builds its digits on the stack and allocates
// only the result.
func TestToFixedOneAlloc(t *testing.T) {
	r := NewRealm()
	for _, c := range []struct {
		x    float64
		d    int
		want string
	}{{1.2345, 2, "1.23"}, {12, 2, "12.00"}, {-0.005, 3, "-0.005"}, {0.000123, 2, "0.00"}, {9.995, 1, "10.0"}, {1.006, 2, "1.01"}, {0.7, 0, "1"}} {
		x, args := NumberValue(c.x), []Value{NumberValue(float64(c.d))}
		v, err := numberProtoToFixed(r, x, args)
		require.NoError(t, err)
		assert.Equal(t, c.want, v.AsString().GoString())
		assert.Equal(t, 1.0, testing.AllocsPerRun(20, func() { _, _ = numberProtoToFixed(r, x, args) }), "%v.toFixed(%d)", c.x, c.d)
	}
}

// TestToFixedExact checks toFixed against the exact reference (fuzzToFixed)
// where the engine computes the digits from x's bits (appendFixedExact):
// values with fraction bits at every position, so many ties of the binary
// value, subnormals, products near 2^64, and values drawn from all
// magnitudes, for every digit count up to 21.
func TestToFixedExact(t *testing.T) {
	r := NewRealm()
	rng := rand.New(rand.NewPCG(1, 2))
	var xs []float64
	for j := 1; j <= 70; j++ {
		for _, n := range []float64{1, 3, 5, 7, 99, 12345, 1<<52 - 1} {
			xs = append(xs, math.Ldexp(n, -j)) // n / 2^j, a tie at j-1 binary digits
		}
	}
	xs = append(xs, 5e-324, 2.2250738585072014e-308, 1e-20, 0.5e-19, 1.5e-19, 18446744073.709551615, 1844674407.3709551615,
		1e15+0.3, 9007199254740991.5/2, 4503599627370495.5, 0.1, 0.7, 1.005, 9.995)
	for range 3000 {
		xs = append(xs, math.Float64frombits(rng.Uint64()&^(1<<63)), rng.Float64()*math.Pow10(rng.IntN(25)-8))
	}
	for _, x := range xs {
		for _, s := range []float64{x, -x} {
			for d := 0; d <= 21; d++ {
				want, ok := fuzzToFixed(s, d)
				got, gotOK := fuzzCallNumber(t, r, numberProtoToFixed, s, []Value{IntValue(d)})
				if got != want || gotOK != ok {
					t.Fatalf("(%v).toFixed(%d) = %q, want %q", s, d, got, want)
				}
			}
		}
	}
}

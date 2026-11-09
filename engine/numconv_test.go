package engine

import (
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStringToNumberGrammar(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"   ", 0},
		{"\t\n\v\f\r \u00a0\ufeff\u2028\u2029\u3000", 0},
		{"42", 42},
		{"  42  ", 42},
		{"\ufeff42\u2028", 42},
		{"-42", -42},
		{"+42", 42},
		{"4.5", 4.5},
		{".5", 0.5},
		{"5.", 5},
		{"-.5", -0.5},
		{"1e3", 1000},
		{"1E-3", 0.001},
		{"1.5e+2", 150},
		{"Infinity", math.Inf(1)},
		{"+Infinity", math.Inf(1)},
		{"-Infinity", math.Inf(-1)},
		{"  Infinity  ", math.Inf(1)},
		{"infinity", nan},
		{"inf", nan},
		{"0x1f", 31},
		{"0X1F", 31},
		{"0o17", 15},
		{"0b101", 5},
		{"0x", nan},
		{"-0x1f", nan},
		{"0x1g", nan},
		{"1_000", nan},
		{"12abc", nan},
		{"1e", nan},
		{"1e+", nan},
		{".", nan},
		{"+", nan},
		{"-", nan},
		{"e5", nan},
		{"1 2", nan},
		{"NaN", nan},
		{"undefined", nan},
		{"null", nan},
		{"true", nan},
		{"1e400", math.Inf(1)},
		{"-1e400", math.Inf(-1)},
		{"1e-400", 0},
		{"0.1", 0.1},
		{"123456789012345678901234567890", 1.2345678901234568e29},
		{"0xFFFFFFFFFFFFFFFFF", 295147905179352825856},
		{"0x20000000000001", 9007199254740992}, // rounds to even
		{"0x20000000000003", 9007199254740996},
		{"héllo", nan},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := StringToNumber(FromGoString(c.in))
			if math.IsNaN(c.want) {
				assert.True(t, math.IsNaN(got), "got %v", got)
				return
			}
			assert.Equal(t, c.want, got)
		})
	}
	// "-0" yields negative zero.
	nz := StringToNumber(FromGoString("-0"))
	assert.True(t, nz == 0 && math.Signbit(nz))
}

// Runtime variables so the sum is not folded exactly by the Go compiler.
var pointOne, pointTwo = 0.1, 0.2

func TestNumberToString(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{1, "1"},
		{-1, "-1"},
		{123, "123"},
		{1.5, "1.5"},
		{-1.5, "-1.5"},
		{0.1, "0.1"},
		{0.000001, "0.000001"},
		{0.0000001, "1e-7"},
		{1.5e-7, "1.5e-7"},
		{-1.5e-7, "-1.5e-7"},
		{1e21, "1e+21"},
		{1e20, "100000000000000000000"},
		{123456789012345680000, "123456789012345680000"},
		{1.2345e21, "1.2345e+21"},
		{9007199254740992, "9007199254740992"},
		{9007199254740993, "9007199254740992"},
		{1e15, "1000000000000000"},
		{1e16, "10000000000000000"},
		{123.456, "123.456"},
		{0.5, "0.5"},
		{100, "100"},
		{1.7976931348623157e308, "1.7976931348623157e+308"},
		{5e-324, "5e-324"},
		{2.5e-8, "2.5e-8"},
		{1e-6, "0.000001"},
		{1.1e-6, "0.0000011"},
		{4294967295, "4294967295"},
		{4294967296, "4294967296"},
		{-2147483648, "-2147483648"},
		{pointOne + pointTwo, "0.30000000000000004"},
		{1 / 3.0, "0.3333333333333333"},
		{123e-20, "1.23e-18"},
		{1e100, "1e+100"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, NumberToString(c.in).GoString())
			assert.Equal(t, c.want, NumberToGoString(c.in))
		})
	}
}

// TestIntStrings checks that the strings of the integers intStrings holds
// are made once, with their hash, and shared: converting one again, in
// another realm or as a property key, allocates nothing.
func TestIntStrings(t *testing.T) {
	for _, i := range []int{intStringMin - 1, intStringMin, -3, -1, 0, 9, 10, 24, 255, intStringMax - 1, intStringMax} {
		s := NumberToString(float64(i))
		assert.Equal(t, strconv.Itoa(i), s.GoString())
		held := i >= intStringMin && i < intStringMax
		assert.Equal(t, held, s == NumberToString(float64(i)), "%d", i)
		if held {
			assert.NotZero(t, s.hash, "%d", i)
			assert.Equal(t, s.contentHash(), s.hash, "%d", i)
			assert.NotZero(t, s.flags&strConst, "%d", i)
		}
	}
	assert.Same(t, NumberToString(0), NumberToString(math.Copysign(0, -1)))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = NumberToString(24) }))
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		k, err := r.ToPropertyKey(NumberValue(-3))
		require.NoError(t, err)
		assert.Equal(t, "-3", k.String().GoString())
		assert.Zero(t, testing.AllocsPerRun(100, func() { _, _ = r.ToPropertyKey(NumberValue(-3)) }), "shared %v", shared)
		assert.Zero(t, testing.AllocsPerRun(100, func() { _, _ = r.ToString(NumberValue(1000)) }), "shared %v", shared)
		runScriptsIn(t, r, `globalThis.ks = [String(24), 24 + "", `+"`${-1}`"+`, (1023).toString(), (1024).toString(), String(-0)].join();`)
		assert.Equal(t, "24,24,-1,1023,1024,0", jsGlobal(t, r, "ks").String())
	}
}

func TestNumberToStringRadix(t *testing.T) {
	cases := []struct {
		in    float64
		radix int
		want  string
	}{
		{255, 16, "ff"},
		{255, 2, "11111111"},
		{-255, 16, "-ff"},
		{35, 36, "z"},
		{36, 36, "10"},
		{0, 2, "0"},
		{0.5, 2, "0.1"},
		{0.1, 2, "0.0001100110011001100110011001100110011001100110011001101"},
		{0.5, 16, "0.8"},
		{3.75, 2, "11.11"},
		{1e21, 16, "3635c9adc5dea00000"},
		{math.NaN(), 16, "NaN"},
		{math.Inf(1), 2, "Infinity"},
		{math.Inf(-1), 2, "-Infinity"},
		{1 / 3.0, 3, "0.1"},
		{0.1, 16, "0.1999999999999a"},
		{123.456, 36, "3f.gez4w97ry"},
		// V8 prints 5v1j4f4ds7c000, which reads back as 1e21+131072; of
		// the two 11-digit strings that read back as 1e21, a is closer.
		{1e21, 36, "5v1j4f4ds7a000"},
		{9007199254740993, 2, "100000000000000000000000000000000000000000000000000000"},
		{1<<53 + 2, 2, "100000000000000000000000000000000000000000000000000010"},
		{1 << 60, 16, "1000000000000000"},
		{1<<60 + 1<<8, 16, "1000000000000100"},
		{math.MaxFloat64, 36, "1a1e4vngail" + strings.Repeat("0", 188)},
		{5e-324, 2, "0." + strings.Repeat("0", 1073) + "1"},
		{5e-324, 36, "0." + strings.Repeat("0", 207) + "3"},
		{1e-7, 36, "0.000061oezo085tj"},
		// 34^-211 reads back as 5e-324 too, but 23*34^-212 is closer.
		{5e-324, 34, "0." + strings.Repeat("0", 211) + "n"},
		// Equally close candidates: the even s (Note 2).
		{0.5, 35, "0.hhhhhhhhhhi"},
		{1.5, 35, "1.hhhhhhhhhhh"},
		{-0.5, 2, "-0.1"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, NumberToStringRadix(c.in, c.radix))
		})
	}
}

func TestToInt32Uint32(t *testing.T) {
	cases := []struct {
		in  float64
		i32 int32
		u32 uint32
	}{
		{0, 0, 0},
		{-0.0, 0, 0},
		{1.9, 1, 1},
		{-1.9, -1, 4294967295},
		{2147483647, 2147483647, 2147483647},
		{2147483648, -2147483648, 2147483648},
		{4294967295, -1, 4294967295},
		{4294967296, 0, 0},
		{4294967297, 1, 1},
		{-4294967297, -1, 4294967295},
		{math.NaN(), 0, 0},
		{math.Inf(1), 0, 0},
		{math.Inf(-1), 0, 0},
		{1e20, 1661992960, 1661992960},
		{-1e20, -1661992960, 2632974336},
		{1 << 62, 0, 0},
		{1<<62 + 1<<31, -2147483648, 2147483648},
		{-(1<<62 + 1<<31), -2147483648, 2147483648},
		{1<<63 - 1024, -1024, 4294966272},
		{-(1<<63 - 1024), 1024, 1024},
		{1 << 63, 0, 0},
		{-(1 << 63), 0, 0},
		{1<<63 + 2048, 2048, 2048},
		{9007199254740993, 0, 0},
		{-9007199254740994.0, -2, 4294967294},
	}
	for _, c := range cases {
		assert.Equal(t, c.i32, ToInt32Float(c.in), "int32 %v", c.in)
		assert.Equal(t, c.u32, ToUint32Float(c.in), "uint32 %v", c.in)
	}
	// The int64 conversion below 2^63 agrees with the spec's modulo.
	ref := func(f float64) uint32 {
		if f != f || math.IsInf(f, 0) {
			return 0
		}
		m := math.Mod(math.Trunc(f), 4294967296)
		if m < 0 {
			m += 4294967296
		}
		return uint32(m)
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 100000 {
		f := math.Ldexp(rng.Float64()+0.5, rng.IntN(70))
		if rng.IntN(2) == 0 {
			f = -f
		}
		assert.Equal(t, ref(f), ToUint32Float(f), "uint32 %v", f)
		assert.Equal(t, int32(ref(f)), ToInt32Float(f), "int32 %v", f)
	}
}

func TestToIntegerOrInfinity(t *testing.T) {
	assert.Equal(t, 0.0, ToIntegerOrInfinityFloat(math.NaN()))
	assert.Equal(t, 0.0, ToIntegerOrInfinityFloat(-0.5))
	assert.False(t, math.Signbit(ToIntegerOrInfinityFloat(-0.5)))
	assert.Equal(t, 3.0, ToIntegerOrInfinityFloat(3.9))
	assert.Equal(t, -3.0, ToIntegerOrInfinityFloat(-3.9))
	assert.Equal(t, math.Inf(1), ToIntegerOrInfinityFloat(math.Inf(1)))
}

func TestCanonicalNumericIndexString(t *testing.T) {
	n, ok := CanonicalNumericIndexString(FromGoString("-0"))
	assert.True(t, ok)
	assert.True(t, math.Signbit(n))
	n, ok = CanonicalNumericIndexString(FromGoString("12"))
	assert.True(t, ok)
	assert.Equal(t, 12.0, n)
	_, ok = CanonicalNumericIndexString(FromGoString("012"))
	assert.False(t, ok)
	_, ok = CanonicalNumericIndexString(FromGoString("1.50"))
	assert.False(t, ok)
	n, ok = CanonicalNumericIndexString(FromGoString("1.5"))
	assert.True(t, ok)
	assert.Equal(t, 1.5, n)
	_, ok = CanonicalNumericIndexString(FromGoString("abc"))
	assert.False(t, ok)
	n, ok = CanonicalNumericIndexString(FromGoString("Infinity"))
	assert.True(t, ok)
	assert.True(t, math.IsInf(n, 1))
}

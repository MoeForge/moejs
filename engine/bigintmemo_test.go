package engine

import (
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"math/rand/v2"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MoeForge/moejs/bytecode"
)

// TestBigIntMemo computes BigInts of at most 128 bits, as an Intl
// polyfill's decimal arithmetic does: int64 results, products of two int64s
// past one, and quotients and remainders of one- and two-word dividends by a
// word, both signs. Each is checked against math/big, and one the memo takes
// comes from it the second time, the same BigInt.
func TestBigIntMemo(t *testing.T) {
	r := NewRealm()
	warmMemos(r)
	rng := rand.New(rand.NewPCG(7, 8))
	box := func(x *big.Int) Value {
		b, ok := NewBigIntFromBig(x)
		require.True(t, ok)
		return BigIntValue(b)
	}
	var vals []*big.Int
	for _, v := range []int64{17, 100, 3037000500, 1 << 40, 1e16, math.MaxInt64, math.MinInt64} {
		vals = append(vals, big.NewInt(v), big.NewInt(-v))
	}
	for range 40 {
		x := new(big.Int).SetUint64(rng.Uint64() >> rng.IntN(64))
		if rng.IntN(2) == 0 {
			x.Lsh(x, 64).Or(x, new(big.Int).SetUint64(rng.Uint64()))
		}
		if rng.IntN(2) == 0 {
			x.Neg(x)
		}
		vals = append(vals, x)
	}
	divs := []*big.Int{big.NewInt(10), big.NewInt(-10), big.NewInt(7), big.NewInt(-1), big.NewInt(1 << 62), new(big.Int).SetUint64(math.MaxUint64)}
	ops := []struct {
		op bytecode.Op
		fn func(z, x, y *big.Int) *big.Int
	}{{bytecode.Mul, (*big.Int).Mul}, {bytecode.Div, (*big.Int).Quo}, {bytecode.Mod, (*big.Int).Rem}, {bytecode.Add, (*big.Int).Add}}
	memoized := 0
	for _, x := range vals {
		for _, y := range append(divs, vals[:6]...) {
			for _, o := range ops {
				got, err := r.bigintBinary(o.op, box(x), box(y))
				require.NoError(t, err)
				want := o.fn(new(big.Int), x, y)
				g := got.AsBigInt()
				require.Zero(t, want.Cmp(&g.v), "%v %v %v", x, o.op, y)
				require.Equal(t, want.Sign(), g.v.Sign())
				// The results the memo takes: one of two int64s that fits
				// an int64, or their product, and a quotient of at most two
				// words or a remainder whose magnitude fits an int64 by one
				// word (-2^63 is math/big's).
				word := len(y.Bits()) == 1
				in := x.IsInt64() && y.IsInt64() && (want.IsInt64() || o.op != bytecode.Add) ||
					word && o.op == bytecode.Div && len(x.Bits()) <= 2 ||
					word && o.op == bytecode.Mod && new(big.Int).Abs(want).IsInt64()
				if !in {
					continue
				}
				memoized++
				again, _ := r.bigintBinary(o.op, box(x), box(y))
				require.Same(t, g, again.AsBigInt(), "%v %v %v", x, o.op, y)
			}
		}
	}
	assert.Greater(t, memoized, 1000)
	assert.Zero(t, r.lazy.memos.bigint.skip, "half the lookups hit")
	// A result in the memo allocates nothing.
	x, ten := BigIntValue(r.bigintFromInt64(123456789012345)), BigIntValue(r.bigintFromInt64(10))
	q, _ := r.bigintBinary(bytecode.Div, x, ten)
	q2, _ := r.bigintBinary(bytecode.Div, x, ten)
	assert.Same(t, q.AsBigInt(), q2.AsBigInt())
	assert.Equal(t, 0.0, testing.AllocsPerRun(50, func() { r.bigintBinary(bytecode.Div, x, ten) }))
	// Equal magnitudes of either sign are two BigInts.
	n := r.bigintOf(true, 1, 5)
	p := r.bigintOf(false, 1, 5)
	assert.NotSame(t, n, p)
	assert.Equal(t, "-18446744073709551621", n.v.String())
	assert.Equal(t, "18446744073709551621", p.v.String())
	assert.Same(t, n, r.bigintOf(true, 1, 5))
	// Through scripts, as a polyfill divides.
	assert.Equal(t, "12345678901234,5,12345678901234,true,-100000000000000000000,1606938044258990275541962092341162602522202993782792835301376",
		runScriptsIn(t, r, `var m = 123456789012345n, a = m / 10n, b = m / 10n;
[a, m % 10n, b, a === b, -10000000000n * 10000000000n, (2n ** 100n) * (2n ** 100n)].join()`))
}

// TestBigIntMemoBound: the memo holds at most bigintMemoSets*bigintMemoWays
// BigInts, each of at most 128 bits, at the set its value picks; a larger
// result and one of -16n to 16n never go in.
func TestBigIntMemoBound(t *testing.T) {
	r := NewRealm()
	warmMemos(r)
	for i := range memoWindow {
		r.bigintOf(i%3 == 0, uint64(i%5), uint64(1000+i*7919))
	}
	m := r.lazy.memos.bigints
	require.NotNil(t, m)
	held := 0
	for i := range m {
		set := &m[i]
		for j, b := range &set.b {
			if b == nil {
				continue
			}
			held++
			hi, lo := words128(b.v.Bits())
			assert.Equal(t, set.lo[j], lo)
			assert.Equal(t, set.hi[j], hi)
			assert.LessOrEqual(t, b.v.BitLen(), 128)
			assert.LessOrEqual(t, cap(b.v.Bits()), 128/bits.UintSize)
			h := (lo ^ hi*0x9e3779b97f4a7c15) * 0xbf58476d1ce4e5b9
			if b.v.Sign() < 0 {
				h = ^h
			}
			assert.Equal(t, i, int(h>>(64-bigintMemoBits)), "the set of %v", &b.v)
		}
	}
	assert.LessOrEqual(t, held, bigintMemoSets*bigintMemoWays)
	assert.Greater(t, held, bigintMemoSets*bigintMemoWays/2)
	assert.Same(t, smallBigInts[smallBigIntMax-16], r.bigintOf(true, 0, 16))
	assert.Same(t, smallBigInts[smallBigIntMax], r.bigintOf(true, 0, 0), "no -0n")
}

// TestBigIntMemoLazy: a realm creates its memos after memoAfter results
// they would take, and only with lazy state; the memory account is charged
// for the memos and the BigInt memo, and the live measurement counts them.
func TestBigIntMemoLazy(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	r.bigintFromInt64(1000)
	assert.True(t, r.lazy == nil || r.lazy.memos == nil)
	r.SetMemoryLimit(1 << 30)
	before := r.MeasureLive(nil)
	for i := range memoAfter {
		r.bigintFromInt64(int64(1000 + i))
	}
	require.Nil(t, r.lazy.memos)
	charged := r.Stats().AllocatedBytes
	b := r.bigintFromInt64(5000)
	require.NotNil(t, r.lazy.memos)
	require.NotNil(t, r.lazy.memos.bigints)
	tables := int64(unsafe.Sizeof(bigintMemo{}) + unsafe.Sizeof(realmMemos{}))
	assert.GreaterOrEqual(t, r.Stats().AllocatedBytes-charged, tables)
	assert.GreaterOrEqual(t, r.MeasureLive(nil)-before, tables+int64(unsafe.Sizeof(BigInt{})))
	assert.Same(t, b, r.bigintFromInt64(5000))
	r.ReleaseCallData()
	assert.Same(t, b, r.bigintFromInt64(5000), "ReleaseCallData leaves the memo")
}

// TestBigIntMemoSkip: a BigInt counter, whose results never repeat, skips
// the memo after a window, and counts right meanwhile.
func TestBigIntMemoSkip(t *testing.T) {
	r := NewRealm()
	warmMemos(r)
	assert.Equal(t, "8662667,true",
		runScriptsIn(t, r, `var s = 0n; for (let i = 1000n; i < 3000n; i++) s += i * i; [s / 1000n, s > 0n].join()`))
	g := &r.lazy.memos.bigint
	assert.NotZero(t, g.skip)
	v := r.bigintFromInt64(77777)
	assert.NotSame(t, v, r.bigintFromInt64(77777), "skipped")
	assert.Equal(t, "77777", v.v.String())
}

// TestBigIntUnaryInt64 checks -x, ++x, --x and ~x on int64 operands, which
// compute in an int64 when the result fits, against math/big.
func TestBigIntUnaryInt64(t *testing.T) {
	for _, warm := range []bool{false, true} {
		r := NewRealm()
		if warm {
			warmMemos(r)
		}
		for _, v := range []int64{0, 1, -1, 16, 17, -17, 1 << 40, math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1} {
			x := r.bigintFromInt64(v)
			for _, c := range []struct {
				d    int64
				not  bool
				want *big.Int
			}{
				{0, false, new(big.Int).Neg(big.NewInt(v))},
				{1, false, new(big.Int).Add(big.NewInt(v), big.NewInt(1))},
				{-1, false, new(big.Int).Sub(big.NewInt(v), big.NewInt(1))},
				{0, true, new(big.Int).Not(big.NewInt(v))},
			} {
				got, err := r.bigintUnary(x, c.d, c.not)
				require.NoError(t, err)
				require.Zero(t, c.want.Cmp(&got.AsBigInt().v), "%d d=%d not=%v", v, c.d, c.not)
				require.Equal(t, c.want.Sign(), got.AsBigInt().v.Sign())
			}
		}
	}
}

// TestShortDecimal checks the strings BigInt() reads without math/big
// against parseBigInt, and the ones it leaves to parseBigInt.
func TestShortDecimal(t *testing.T) {
	for _, s := range []string{"0", "-0", "+0", "7", "-7", "007", "123456789012345678", "-999999999999999999", "+16"} {
		v, ok := shortDecimal(s)
		require.True(t, ok, s)
		b, _, ok := parseBigInt(FromGoString(s))
		require.True(t, ok)
		assert.Equal(t, b.v.String(), fmt.Sprint(v), s)
	}
	for _, s := range []string{"", "-", "+", "1234567890123456789", "9223372036854775807", " 1", "1 ", "0x10", "1e3", "1.0", "--1", "1_0", "٣"} {
		_, ok := shortDecimal(s)
		assert.False(t, ok, "%q", s)
	}
	r := NewRealm()
	assert.Equal(t, "42,-42,0,0,1,123456789012345678901234567890,true",
		runScriptsIn(t, r, `[BigInt("42"), BigInt("-42"), BigInt(""), BigInt(false), BigInt(true), BigInt(" 123456789012345678901234567890 "), BigInt("16") === 16n].join()`))
}

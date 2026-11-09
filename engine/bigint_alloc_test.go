package engine

import (
	"math"
	"math/big"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MoeForge/moejs/bytecode"
)

// TestBigIntOperatorsCoAllocated checks the operators whose results get
// co-allocated words (newBigIntCap) against math/big, over operands of 0 to
// 80 words (past the largest bucket), both signs, and values next to a word
// boundary, where a carry or a borrow changes the word count.
func TestBigIntOperatorsCoAllocated(t *testing.T) {
	r := NewRealm()
	rng := rand.New(rand.NewPCG(1, 2))
	var vals []*big.Int
	one := big.NewInt(1)
	for _, words := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 11, 12, 13, 16, 17, 23, 24, 25, 31, 32, 33, 47, 48, 49, 63, 64, 65, 80} {
		edge := new(big.Int).Lsh(one, uint(64*words)) // 2^(64*words)
		vals = append(vals, edge, new(big.Int).Sub(edge, one))
		x := new(big.Int)
		for range words {
			x.Lsh(x, 64).Or(x, new(big.Int).SetUint64(rng.Uint64()))
		}
		vals = append(vals, x)
	}
	for _, v := range vals[:len(vals):len(vals)] {
		vals = append(vals, new(big.Int).Neg(v))
	}
	box := func(x *big.Int) Value {
		b, ok := NewBigIntFromBig(x)
		require.True(t, ok)
		return BigIntValue(b)
	}
	ops := []struct {
		op bytecode.Op
		fn func(z, x, y *big.Int) *big.Int
	}{
		{bytecode.Add, (*big.Int).Add},
		{bytecode.Sub, (*big.Int).Sub},
		{bytecode.Mul, (*big.Int).Mul},
		{bytecode.BitAnd, (*big.Int).And},
		{bytecode.BitOr, (*big.Int).Or},
		{bytecode.BitXor, (*big.Int).Xor},
	}
	for _, x := range vals {
		for _, y := range vals {
			for _, o := range ops {
				got, err := r.bigintBinary(o.op, box(x), box(y))
				require.NoError(t, err)
				want := o.fn(new(big.Int), x, y)
				require.Zero(t, want.Cmp(&got.AsBigInt().v), "%v %v %v", x, o.op, y)
				// A zero result is not negative (no -0n).
				require.Equal(t, want.Sign(), got.AsBigInt().v.Sign())
			}
		}
		for _, u := range []struct {
			d    int64
			not  bool
			want *big.Int
		}{
			{0, false, new(big.Int).Neg(x)},
			{1, false, new(big.Int).Add(x, one)},
			{-1, false, new(big.Int).Sub(x, one)},
			{0, true, new(big.Int).Not(x)},
		} {
			got, err := r.bigintUnary(box(x).AsBigInt(), u.d, u.not)
			require.NoError(t, err)
			require.Zero(t, u.want.Cmp(&got.AsBigInt().v), "%v d=%d not=%v", x, u.d, u.not)
		}
	}
	// A result past the size limit still throws.
	big1 := new(big.Int).Lsh(one, maxBigIntBits-2)
	_, err := r.bigintBinary(bytecode.Add, box(big1), box(big1))
	require.NoError(t, err)
	_, err = r.bigintBinary(bytecode.Mul, box(big1), box(big.NewInt(8)))
	require.Error(t, err)
	_, err = r.bigintBinary(bytecode.Add, box(new(big.Int).Sub(new(big.Int).Lsh(one, maxBigIntBits), one)), box(one))
	require.Error(t, err)
}

// TestBigIntAddAllocs checks that a small sum is one allocation.
func TestBigIntAddAllocs(t *testing.T) {
	r := NewRealm()
	x, _ := NewBigIntFromBig(new(big.Int).Lsh(big.NewInt(1), 1000))
	v := BigIntValue(x)
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := r.bigintBinary(bytecode.Add, v, v); err != nil {
			t.Fatal(err)
		}
	})
	require.Equal(t, 1.0, allocs)
}

// TestBigIntOperatorsInt64 checks every operator on operands that fit an
// int64 (bigintBinaryInt64) against math/big: edge pairs around 0, 2^31,
// 2^32, 2^62 and 2^63, where a result first fails to fit, and a seeded
// random sample of small, word-sized and power-of-two values. The math/big
// path itself (bigintBinaryBig) gives the expected value and error.
func TestBigIntOperatorsInt64(t *testing.T) {
	r := NewRealm()
	rng := rand.New(rand.NewPCG(3, 4))
	vals := []int64{0, 1, 2, 3, 7, 10, 16, 17, 31, 32, 62, 63, 64, 65, 100, 3037000499, 3037000500, math.MaxInt64, math.MinInt64}
	for _, e := range []uint{31, 32, 62} {
		vals = append(vals, 1<<e-1, 1<<e, 1<<e+1)
	}
	vals = append(vals, math.MaxInt64-1, math.MinInt64+1)
	for range 40 {
		vals = append(vals, rng.Int64N(1000), rng.Int64(), int64(rng.Uint64())>>rng.UintN(64))
	}
	for _, v := range vals[:len(vals):len(vals)] {
		if v != math.MinInt64 {
			vals = append(vals, -v)
		}
	}
	ops := []bytecode.Op{bytecode.Add, bytecode.Sub, bytecode.Mul, bytecode.Div, bytecode.Mod, bytecode.Exp, bytecode.BitAnd, bytecode.BitOr, bytecode.BitXor, bytecode.Shl, bytecode.Shr, bytecode.UShr}
	errText := func(err error) string {
		var ex *Exception
		require.ErrorAs(t, err, &ex)
		return errorDisplayString(ex.Value)
	}
	fast := 0
	for _, x := range vals {
		for _, y := range vals {
			bx, by := BigIntValue(NewBigIntFromInt64(x)), BigIntValue(NewBigIntFromInt64(y))
			for _, op := range ops {
				if op == bytecode.Exp && y > 64 && (x > 1 || x < -1) {
					// At least 2^65: math/big's power of up to 2^20 bits
					// would only be compared with itself.
					_, ok := bigintBinaryInt64(op, x, y)
					require.False(t, ok, "%d ** %d", x, y)
					continue
				}
				got, gerr := r.bigintBinary(op, bx, by)
				want, werr := r.bigintBinaryBig(op, big.NewInt(x), big.NewInt(y))
				if werr != nil {
					require.Error(t, gerr, "%d %v %d", x, op, y)
					require.Equal(t, errText(werr), errText(gerr), "%d %v %d", x, op, y)
					continue
				}
				require.NoError(t, gerr, "%d %v %d", x, op, y)
				g := &got.AsBigInt().v
				require.Zero(t, want.AsBigInt().v.Cmp(g), "%d %v %d: %v, want %v", x, op, y, g, &want.AsBigInt().v)
				// A zero result is not negative (no -0n), and the words
				// are normalized: no leading zero word.
				require.Equal(t, want.AsBigInt().v.Sign(), g.Sign())
				if w := g.Bits(); len(w) > 0 {
					require.NotZero(t, w[len(w)-1])
				}
				if _, ok := bigintBinaryInt64(op, x, y); ok {
					fast++
				}
			}
		}
	}
	// Most pairs take the int64 path; the rest are the overflows above.
	require.Greater(t, fast, len(vals)*len(vals)*len(ops)/2)
}

// TestBigIntOperatorsInt64Edges checks the int64 path at its boundaries,
// with the values Node.js 24 prints: overflow on + - * ** and <<, the
// quotient and remainder of -2^63 by -1, the signs of / and %, and shifts
// by negative, past-the-width and huge counts in both directions.
func TestBigIntOperatorsInt64Edges(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"overflow", `const X = 2n ** 63n - 1n, N = -(2n ** 63n); return [X + 1n, N - 1n, N + X, X - N, N / -1n, N % -1n, N % 1n, N / 1n, N * -1n, X * -1n, N * 1n, -(2n ** 32n) * (2n ** 31n), (2n ** 32n) * (2n ** 31n), (2n ** 32n) * (2n ** 32n), 3037000499n * 3037000499n, 3037000500n * 3037000500n].join()`,
			"9223372036854775808,-9223372036854775809,-1,18446744073709551615,9223372036854775808,0,0,-9223372036854775808,9223372036854775808,-9223372036854775807,-9223372036854775808,-9223372036854775808,9223372036854775808,18446744073709551616,9223372030926249001,9223372037000250000"},
		{"power", `const X = 2n ** 63n - 1n; return [(-2n) ** 63n, 2n ** 63n, 2n ** 62n, (-3n) ** 39n, 3n ** 40n, 10n ** 18n, 10n ** 19n, (-1n) ** X, 0n ** X, 1n ** X].join()`,
			"-9223372036854775808,9223372036854775808,4611686018427387904,-4052555153018976267,12157665459056928801,1000000000000000000,10000000000000000000,-1,0,1"},
		{"signs", `const X = 2n ** 63n - 1n, N = -(2n ** 63n); return [-7n / 2n, 7n / -2n, -7n % 2n, 7n % -2n, N & X, N | 0n, N ^ -1n, -(2n ** 31n) & (2n ** 32n - 1n)].join()`,
			"-3,-3,-1,1,0,-9223372036854775808,9223372036854775807,2147483648"},
		{"shifts", `const M = 2n ** 63n; return [1n << 62n, 1n << 63n, -1n << 63n, -1n << 64n, 3n << 61n, 3n << 62n, -(2n ** 62n) << 1n, (M - 1n) >> 62n, (M - 1n) >> 63n, (-M) >> 62n, (-M) >> 63n, (-M) >> 64n, (-M) >> (M - 1n), -5n >> 1n, -5n >> 100n, 5n >> -2n, -1n << -1n, -1n << -64n, 3n << -M, -3n << -M, -3n >> (M - 1n), 0n << (M - 1n), 0n >> -M, 1n >> -63n, -1n >> -63n, (M - 1n) << 0n, (-M) >> 0n, 7n >> -0n].join()`,
			"4611686018427387904,9223372036854775808,-9223372036854775808,-18446744073709551616,6917529027641081856,13835058055282163712,-9223372036854775808,1,0,-2,-1,-1,-1,-3,-1,20,-1,-1,0,-1,-1,0,0,9223372036854775808,-9223372036854775808,9223372036854775807,-9223372036854775808,7"},
		{"shift too large", `return 1n << (2n ** 63n - 1n)`, "!RangeError: Maximum BigInt size exceeded"},
		{"right shift too large", `return -1n >> -(2n ** 63n)`, "!RangeError: Maximum BigInt size exceeded"},
		{"power too large", `return 2n ** (2n ** 63n - 1n)`, "!RangeError: Maximum BigInt size exceeded"},
		{"divide by zero", `return (2n ** 63n - 1n) / 0n`, "!RangeError: Division by zero"},
		{"modulo zero", `return -(2n ** 63n) % 0n`, "!RangeError: Division by zero"},
		{"zero by zero", `return 0n / 0n`, "!RangeError: Division by zero"},
		{"negative exponent", `return 0n ** -1n`, "!RangeError: Exponent must be non-negative"},
		{"negative exponent of one", `return 1n ** -(2n ** 63n)`, "!RangeError: Exponent must be non-negative"},
		{"small results", `const a = []; for (let i = -20n; i <= 20n; i++) a.push(i * 1n, i % 10n, i / 1n, -i + 0n); return [a.length, a.join() === a.map(String).join(), (5n * 3n) === 15n, typeof (3n % 10n), new Set(a).size].join()`, "164,true,true,bigint,41"},
	})
}

// TestBigIntDivWord checks / and % by a one-word divisor (bigintDivWord)
// against math/big over dividends of 1 to 80 words, both signs, divisors
// around 10, 2^32, 2^63 and 2^64, both signs, and that a quotient takes one
// allocation and a small remainder none.
func TestBigIntDivWord(t *testing.T) {
	r := NewRealm()
	rng := rand.New(rand.NewPCG(5, 6))
	one := big.NewInt(1)
	box := func(x *big.Int) Value {
		b, ok := NewBigIntFromBig(x)
		require.True(t, ok)
		return BigIntValue(b)
	}
	var xs []*big.Int
	for _, words := range []int{1, 2, 3, 4, 7, 8, 9, 64, 65, 80} {
		edge := new(big.Int).Lsh(one, uint(64*words))
		x := new(big.Int)
		for range words {
			x.Lsh(x, 64).Or(x, new(big.Int).SetUint64(rng.Uint64()))
		}
		xs = append(xs, new(big.Int).Sub(edge, one), x, new(big.Int).Lsh(one, uint(64*words-1)))
	}
	xs = append(xs, new(big.Int).Lsh(one, 63), big.NewInt(123456789), new(big.Int).Mul(big.NewInt(1e18), big.NewInt(1e18)))
	for _, x := range xs[:len(xs):len(xs)] {
		xs = append(xs, new(big.Int).Neg(x))
	}
	var ds []*big.Int
	for _, d := range []uint64{1, 2, 3, 10, 1e15, 1<<32 - 1, 1 << 32, 1<<63 - 1, 1 << 63, 1<<63 + 1, 1<<64 - 1} {
		x := new(big.Int).SetUint64(d)
		ds = append(ds, x, new(big.Int).Neg(x))
	}
	for _, x := range xs {
		for _, d := range ds {
			for _, c := range []struct {
				op bytecode.Op
				fn func(z, x, y *big.Int) *big.Int
			}{{bytecode.Div, (*big.Int).Quo}, {bytecode.Mod, (*big.Int).Rem}} {
				got, err := r.bigintBinary(c.op, box(x), box(d))
				require.NoError(t, err)
				want := c.fn(new(big.Int), x, d)
				require.Zero(t, want.Cmp(&got.AsBigInt().v), "%v %v %v", x, c.op, d)
				require.Equal(t, want.Sign(), got.AsBigInt().v.Sign())
			}
		}
	}
	x, ten := box(new(big.Int).Lsh(one, 200)), box(big.NewInt(10))
	for _, c := range []struct {
		op     bytecode.Op
		allocs float64
	}{{bytecode.Div, 1}, {bytecode.Mod, 0}} {
		allocs := testing.AllocsPerRun(100, func() {
			if _, err := r.bigintBinary(c.op, x, ten); err != nil {
				t.Fatal(err)
			}
		})
		require.Equal(t, c.allocs, allocs, "%v", c.op)
	}
}

// TestBigIntInt64Allocs checks that a result that fits an int64 is one
// allocation, and one of -16n to 16n none.
func TestBigIntInt64Allocs(t *testing.T) {
	r := NewRealm()
	x, y, ten := BigIntValue(NewBigIntFromInt64(123456789012345)), BigIntValue(NewBigIntFromInt64(987654321)), BigIntValue(NewBigIntFromInt64(10))
	for _, c := range []struct {
		op     bytecode.Op
		x, y   Value
		allocs float64
	}{
		{bytecode.Div, x, ten, 1},
		{bytecode.Mod, x, ten, 0},
		{bytecode.Mul, y, ten, 1},
		{bytecode.Add, x, y, 1},
		{bytecode.Shr, x, ten, 1},
		{bytecode.Exp, ten, ten, 1},
		{bytecode.Div, ten, y, 0},
	} {
		allocs := testing.AllocsPerRun(100, func() {
			if _, err := r.bigintBinary(c.op, c.x, c.y); err != nil {
				t.Fatal(err)
			}
		})
		require.Equal(t, c.allocs, allocs, "%v", c.op)
	}
}

// TestBigIntInt64Charge checks that the memory limit charges a shared
// small result as a new one of the same size.
func TestBigIntInt64Charge(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 30})
	x, ten := BigIntValue(NewBigIntFromInt64(123456789012345)), BigIntValue(NewBigIntFromInt64(10))
	charged := func(op bytecode.Op) int64 {
		before := r.Stats().RequestAllocatedBytes
		_, err := r.bigintBinary(op, x, ten)
		require.NoError(t, err)
		return r.Stats().RequestAllocatedBytes - before
	}
	div, mod := charged(bytecode.Div), charged(bytecode.Mod)
	require.Positive(t, div)
	if is64 { // 32-bit systems give a shared result the words of its value, a new one those of an int64
		require.Equal(t, div, mod)
	}
}

package engine

import (
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

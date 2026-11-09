package engine

import (
	"math"
	"math/big"
	"math/bits"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// The BigInt operators. The interpreter's number fast paths never reach
// them: the operator slow paths (interp_ops.go, Realm.Add) call them once
// ToNumeric has produced a bigint operand.

// bigintBinary finishes a binary operator whose ToNumeric operands include
// a bigint: two bigints compute, a bigint and a number is a TypeError. Two
// bigints that fit an int64 compute in it when the result fits too
// (bigintBinaryInt64), without math/big: on decimal arithmetic such as an
// Intl polyfill's (x % 10n, x / 10n, x * 10n), about half the operations.
func (r *Realm) bigintBinary(op bytecode.Op, x, y Value) (Value, error) {
	if !x.IsBigInt() || !y.IsBigInt() {
		return Undefined(), r.TypeError("Cannot mix BigInt and other types, use explicit conversions")
	}
	a, b := &x.AsBigInt().v, &y.AsBigInt().v
	if a.IsInt64() && b.IsInt64() {
		p, q := a.Int64(), b.Int64()
		if z, ok := bigintBinaryInt64(op, p, q); ok {
			// bigintResult charges a shared result too, as the BigInt it
			// stands for.
			return r.bigintResult(r.bigintFromInt64(z))
		}
		if op == bytecode.Mul {
			// Past an int64, the product of two is at most 2^126.
			hi, lo := bits.Mul64(magnitude64(p), magnitude64(q))
			return r.bigintResult(r.bigintOf(p < 0 != (q < 0), hi, lo))
		}
	}
	return r.bigintBinaryBig(op, a, b)
}

// bigintBinaryBig computes a binary operator on two bigints with math/big.
func (r *Realm) bigintBinaryBig(op bytecode.Op, a, b *big.Int) (Value, error) {
	// The result of +, -, * and the bitwise operators has at most these
	// many words; math/big writes it into the room newBigIntCap made.
	n := max(len(a.Bits()), len(b.Bits())) + 1
	var z *BigInt
	switch op {
	case bytecode.Add:
		z = newBigIntCap(n)
		z.v.Add(a, b)
	case bytecode.Sub:
		z = newBigIntCap(n)
		z.v.Sub(a, b)
	case bytecode.Mul:
		if a.BitLen()+b.BitLen() > maxBigIntBits+1 {
			return Undefined(), errBigIntTooBig(r)
		}
		z = newBigIntCap(len(a.Bits()) + len(b.Bits()))
		z.v.Mul(a, b)
	case bytecode.Div, bytecode.Mod:
		if b.Sign() == 0 {
			return Undefined(), r.RangeError("Division by zero")
		}
		if d := b.Bits(); len(d) == 1 {
			if v, ok := r.bigintDivWord(op, a, b.Sign() < 0, d[0]); ok {
				return v, nil
			}
		}
		z = &BigInt{}
		if op == bytecode.Div {
			z.v.Quo(a, b) // truncates toward zero
		} else {
			z.v.Rem(a, b) // takes the sign of the dividend
		}
	case bytecode.Exp:
		return r.bigintExp(a, b)
	case bytecode.BitAnd:
		z = newBigIntCap(n)
		z.v.And(a, b) // math/big's bitwise operators are two's complement
	case bytecode.BitOr:
		z = newBigIntCap(n)
		z.v.Or(a, b)
	case bytecode.BitXor:
		z = newBigIntCap(n)
		z.v.Xor(a, b)
	case bytecode.Shl:
		return r.bigintShift(a, b, false)
	case bytecode.Shr:
		return r.bigintShift(a, b, true)
	default: // UShr
		return Undefined(), r.TypeError("BigInts have no unsigned right shift, use >> instead")
	}
	return r.bigintResult(z)
}

// bigintDivWord computes a / b (op Div) or a % b (Mod) for a divisor of one
// word d, negative when neg, by short division: the quotient goes into
// words co-allocated with its BigInt and a remainder that fits an int64
// into none, where math/big's Quo allocates the quotient apart from the
// BigInt and Rem allocates a quotient it drops and the remainder apart. The
// decimal arithmetic of an Intl polyfill (x / 10n, x % 10n on mantissas
// past an int64) divides by one word nearly always. ok is false for a
// remainder that does not fit an int64, which math/big then computes.
func (r *Realm) bigintDivWord(op bytecode.Op, a *big.Int, neg bool, d big.Word) (Value, bool) {
	w := a.Bits()
	if op == bytecode.Mod {
		var rem uint
		for i := len(w) - 1; i >= 0; i-- {
			_, rem = bits.Div(rem, uint(w[i]), uint(d))
		}
		if uint64(rem) > math.MaxInt64 {
			return Undefined(), false
		}
		z := int64(rem)
		if a.Sign() < 0 { // takes the sign of the dividend
			z = -z
		}
		v, _ := r.bigintResult(r.bigintFromInt64(z))
		return v, true
	}
	if len(w) <= 128/bits.UintSize {
		// A quotient of at most 128 bits is computed in place, then is
		// the BigInt the memo holds for it, if any (bigintOf).
		var qb [128 / bits.UintSize]big.Word
		var rem uint
		for i := len(w) - 1; i >= 0; i-- {
			var qi uint
			qi, rem = bits.Div(rem, uint(w[i]), uint(d))
			qb[i] = big.Word(qi)
		}
		hi, lo := words128(qb[:len(w)])
		v, _ := r.bigintResult(r.bigintOf(a.Sign() < 0 != neg, hi, lo))
		return v, true
	}
	z := newBigIntCap(len(w))
	q := z.v.Bits()
	if cap(q) < len(w) { // past the largest bucket
		q = make([]big.Word, len(w))
	}
	q = q[:len(w)]
	var rem uint
	for i := len(w) - 1; i >= 0; i-- {
		var qi uint
		qi, rem = bits.Div(rem, uint(w[i]), uint(d))
		q[i] = big.Word(qi)
	}
	// SetBits drops the leading zero words. The quotient truncates toward
	// zero, and Neg leaves 0 without a sign.
	z.v.SetBits(q)
	if a.Sign() < 0 != neg {
		z.v.Neg(&z.v)
	}
	v, _ := r.bigintResult(z)
	return v, true
}

// bigintBinaryInt64 computes a binary operator on two bigints that fit an
// int64, exactly: ok is false when the result does not fit one, and for
// what the math/big path throws on (a zero divisor, a negative exponent,
// >>>), which then runs it.
func bigintBinaryInt64(op bytecode.Op, x, y int64) (z int64, ok bool) {
	switch op {
	case bytecode.Add:
		// A sum overflows when its sign differs from both operands'.
		z = x + y
		return z, (x^z)&(y^z) >= 0
	case bytecode.Sub:
		z = x - y
		return z, (x^y)&(x^z) >= 0
	case bytecode.Mul:
		return mulInt64(x, y)
	case bytecode.Div:
		if y == 0 || x == math.MinInt64 && y == -1 { // -2^63 / -1n is 2^63
			return 0, false
		}
		return x / y, true // truncates toward zero
	case bytecode.Mod:
		if y == 0 {
			return 0, false
		}
		return x % y, true // takes the sign of the dividend; -2^63 % -1 is 0
	case bytecode.Exp:
		return expInt64(x, y)
	case bytecode.BitAnd:
		return x & y, true // two's complement, as math/big's operators
	case bytecode.BitOr:
		return x | y, true
	case bytecode.BitXor:
		return x ^ y, true
	case bytecode.Shl:
		return shiftInt64(x, y, false)
	case bytecode.Shr:
		return shiftInt64(x, y, true)
	}
	return 0, false
}

// mulInt64 is x * y, ok when the product fits an int64.
func mulInt64(x, y int64) (int64, bool) {
	hi, lo := bits.Mul64(magnitude64(x), magnitude64(y))
	if (x < 0) != (y < 0) {
		return int64(-lo), hi == 0 && lo <= 1<<63
	}
	return int64(lo), hi == 0 && lo < 1<<63
}

// magnitude64 is |x|, which for -2^63 only a uint64 holds.
func magnitude64(x int64) uint64 {
	if x < 0 {
		return -uint64(x)
	}
	return uint64(x)
}

// expInt64 is x ** y, ok when y is not negative and the power fits an
// int64.
func expInt64(x, y int64) (int64, bool) {
	switch {
	case y < 0:
		return 0, false
	case y == 0 || x == 1:
		return 1, true
	case x == 0:
		return 0, true
	case x == -1:
		if y&1 != 0 {
			return -1, true
		}
		return 1, true
	case y >= 64: // |x| >= 2, so the power is at least 2^64
		return 0, false
	}
	// Square and multiply. Every product is checked, so a power that fits
	// is exact; the first one that overflows hands the power to math/big.
	z, ok := int64(1), true
	for {
		if y&1 != 0 {
			if z, ok = mulInt64(z, x); !ok {
				return 0, false
			}
		}
		if y >>= 1; y == 0 {
			return z, true
		}
		if x, ok = mulInt64(x, x); !ok {
			return 0, false
		}
	}
}

// shiftInt64 is x << y, or x >> y when right is set (a left shift by -y),
// ok when the result fits an int64. A right shift rounds toward negative
// infinity, as Go's >> on a signed integer.
func shiftInt64(x, y int64, right bool) (int64, bool) {
	n := magnitude64(y)
	if y < 0 {
		right = !right
	}
	if right {
		return x >> n, true // past the last bit only the sign is left
	}
	if n < 64 {
		if z := x << n; z>>n == x {
			return z, true
		}
	}
	return 0, x == 0
}

// bigintResult boxes z, or throws when it exceeds the size limit. It
// charges z to the memory limit.
func (r *Realm) bigintResult(z *BigInt) (Value, error) {
	if z.v.BitLen() > maxBigIntBits {
		return Undefined(), errBigIntTooBig(r)
	}
	r.charge(int(unsafe.Sizeof(BigInt{})) + cap(z.v.Bits())*bits.UintSize/8)
	return BigIntValue(z), nil
}

// bigintExp implements BigInt::exponentiate.
func (r *Realm) bigintExp(a, b *big.Int) (Value, error) {
	if b.Sign() < 0 {
		return Undefined(), r.RangeError("Exponent must be non-negative")
	}
	z := &BigInt{}
	switch {
	case b.Sign() == 0 || a.CmpAbs(big.NewInt(1)) == 0:
		// x ** 0n is 1n; ±1n ** y alternates its sign with y's parity.
		z.v.SetInt64(1)
		if a.Sign() < 0 && b.Bit(0) == 1 {
			z.v.Neg(&z.v)
		}
		return BigIntValue(z), nil
	case a.Sign() == 0:
		return BigIntValue(z), nil
	}
	// |a| >= 2^(bitlen(a)-1), so the result has more than (bitlen(a)-1)*b
	// bits. The product can reach 2^40: it is an int64, not an int, which
	// is 32 bits on some platforms.
	if b.Cmp(big.NewInt(maxBigIntBits)) > 0 || int64(a.BitLen()-1)*b.Int64() >= maxBigIntBits {
		return Undefined(), errBigIntTooBig(r)
	}
	z.v.Exp(a, b, nil)
	return r.bigintResult(z)
}

// bigintShift implements BigInt::leftShift, or BigInt::signedRightShift when
// right is set (a left shift by -b). A right shift rounds toward negative
// infinity.
func (r *Realm) bigintShift(a, b *big.Int, right bool) (Value, error) {
	z := &BigInt{}
	if a.Sign() == 0 || b.Sign() == 0 {
		z.v.Set(a)
		return BigIntValue(z), nil
	}
	if right == (b.Sign() > 0) {
		// A right shift by |b|: past the last bit only the sign is left.
		if b.CmpAbs(big.NewInt(int64(a.BitLen()))) >= 0 {
			if a.Sign() < 0 {
				z.v.SetInt64(-1)
			}
			return BigIntValue(z), nil
		}
		z.v.Rsh(a, uint(absInt64(b.Int64())))
		return BigIntValue(z), nil
	}
	if b.CmpAbs(big.NewInt(maxBigIntBits)) > 0 || a.BitLen()+int(absInt64(b.Int64())) > maxBigIntBits {
		return Undefined(), errBigIntTooBig(r)
	}
	z.v.Lsh(a, uint(absInt64(b.Int64())))
	return r.bigintResult(z)
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// bigintUnary applies unary minus (d == 0), ++ (d == 1), -- (d == -1) or
// ~ (not) to a bigint.
func (r *Realm) bigintUnary(x *BigInt, d int64, not bool) (Value, error) {
	if x.v.IsInt64() {
		v := x.v.Int64()
		switch {
		case not:
			return r.bigintResult(r.bigintFromInt64(^v))
		case d == 0 && v != math.MinInt64:
			return r.bigintResult(r.bigintFromInt64(-v))
		case d != 0:
			if z := v + d; (z > v) == (d > 0) {
				return r.bigintResult(r.bigintFromInt64(z))
			}
		}
	}
	z := newBigIntCap(len(x.v.Bits()) + 1)
	switch {
	case not:
		z.v.Not(&x.v)
	case d == 0:
		z.v.Neg(&x.v)
	default:
		z.v.Add(&x.v, big.NewInt(d))
	}
	return r.bigintResult(z)
}

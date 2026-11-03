package engine

import (
	"math/big"
	"math/bits"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// The BigInt operators. The interpreter's number fast paths never reach
// them: the operator slow paths (interp_ops.go, Realm.Add) call them once
// ToNumeric has produced a bigint operand.

// bigintBinary finishes a binary operator whose ToNumeric operands include
// a bigint: two bigints compute, a bigint and a number is a TypeError.
func (r *Realm) bigintBinary(op bytecode.Op, x, y Value) (Value, error) {
	if !x.IsBigInt() || !y.IsBigInt() {
		return Undefined(), r.TypeError("Cannot mix BigInt and other types, use explicit conversions")
	}
	a, b := &x.AsBigInt().v, &y.AsBigInt().v
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

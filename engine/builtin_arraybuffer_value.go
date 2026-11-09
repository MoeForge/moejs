package engine

import "math"

// The element types of ECMA-262 Table 71 and the raw-bytes conversions of
// GetValueFromBuffer and SetValueInBuffer (RawBytesToNumeric,
// NumericToRawBytes), shared by the DataView methods and the typed arrays.

// elemType is an element type: how one value is laid out in a buffer.
type elemType uint8

const (
	elemInt8 elemType = iota
	elemUint8
	elemUint8Clamped
	elemInt16
	elemUint16
	elemInt32
	elemUint32
	elemFloat16
	elemFloat32
	elemFloat64
	elemBigInt64
	elemBigUint64
)

// elemSize is the Element Size of each elemType.
var elemSize = [...]int{1, 1, 1, 2, 2, 4, 4, 2, 4, 8, 8, 8}

// isBigInt reports whether t holds BigInt values (IsBigIntElementType).
func (t elemType) isBigInt() bool { return t >= elemBigInt64 }

// loadRaw reads the n bytes of b as an unsigned integer, least significant
// byte first when little is set.
func loadRaw(b []byte, n int, little bool) uint64 {
	var u uint64
	if little {
		for i := n - 1; i >= 0; i-- {
			u = u<<8 | uint64(b[i])
		}
	} else {
		for i := range n {
			u = u<<8 | uint64(b[i])
		}
	}
	return u
}

// storeRaw writes the low n bytes of u to b in the byte order of loadRaw.
func storeRaw(b []byte, n int, u uint64, little bool) {
	if little {
		for i := range n {
			b[i] = byte(u)
			u >>= 8
		}
	} else {
		for i := n - 1; i >= 0; i-- {
			b[i] = byte(u)
			u >>= 8
		}
	}
}

// rawToValue implements RawBytesToNumeric on the bits u of an element of
// type t.
func rawToValue(t elemType, u uint64) Value {
	switch t {
	case elemInt8:
		return IntValue(int(int8(u)))
	case elemUint8, elemUint8Clamped:
		return IntValue(int(uint8(u)))
	case elemInt16:
		return IntValue(int(int16(u)))
	case elemUint16:
		return IntValue(int(uint16(u)))
	case elemInt32:
		return IntValue(int(int32(u)))
	case elemUint32:
		return Int64Value(int64(uint32(u))) // a uint32 past 2**31 is no int on 32-bit systems
	case elemFloat16:
		return NumberValue(float16ToFloat64(uint16(u)))
	case elemFloat32:
		return NumberValue(float64(math.Float32frombits(uint32(u))))
	case elemFloat64:
		return NumberValue(math.Float64frombits(u))
	case elemBigInt64:
		return BigIntValue(NewBigIntFromInt64(int64(u)))
	}
	return BigIntValue(NewBigIntFromUint64(u))
}

// numberToRaw implements NumericToRawBytes for the Number f and a type t
// that does not hold BigInts: the integer types keep the low bits of
// ToUint32 (ToInt8 ... ToUint32 agree on them), Uint8Clamped rounds half
// to even within 0..255 and the float types round to nearest, ties to even.
func numberToRaw(t elemType, f float64) uint64 {
	switch t {
	case elemUint8Clamped:
		switch {
		case !(f > 0):
			return 0 // also NaN
		case f >= 255:
			return 255
		}
		return uint64(math.RoundToEven(f))
	case elemFloat16:
		return uint64(float64ToFloat16(f))
	case elemFloat32:
		return uint64(math.Float32bits(float32(f)))
	case elemFloat64:
		return math.Float64bits(f)
	}
	return uint64(ToUint32Float(f))
}

// toRaw converts v for an element of type t (ToBigInt or ToNumber) and
// returns its bits (NumericToRawBytes).
func (r *Realm) toRaw(t elemType, v Value) (uint64, error) {
	if t.isBigInt() {
		return r.ToBigUint64(v) // the bits ToBigInt64 stores too
	}
	f, err := r.ToNumber(v)
	if err != nil {
		return 0, err
	}
	return numberToRaw(t, f), nil
}

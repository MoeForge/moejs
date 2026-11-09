package engine

// Type is the coarse JavaScript type of a Value, for switch dispatch.
type Type uint8

const (
	TypeUndefined Type = iota
	TypeNull
	TypeBoolean
	TypeNumber
	TypeString
	TypeSymbol
	TypeBigInt
	TypeObject
	// TypeHole is the internal hole marker; it never reaches user code.
	TypeHole
)

// Type returns the coarse type of v.
func (v Value) Type() Type {
	if v.ptr == nil {
		if v.bits>>48 != reservedPrefix>>48 {
			return TypeNumber
		}
		switch v.bits {
		case bitsUndefined:
			return TypeUndefined
		case bitsNull:
			return TypeNull
		case bitsTrue, bitsFalse:
			return TypeBoolean
		}
		return TypeHole
	}
	switch v.bits {
	case tagString:
		return TypeString
	case tagObject:
		return TypeObject
	case tagSymbol:
		return TypeSymbol
	case tagBigInt:
		return TypeBigInt
	}
	return TypeHole
}

// Arg returns args[i] or undefined when i is out of range.
func Arg(args []Value, i int) Value {
	if i < len(args) {
		return args[i]
	}
	return Undefined()
}

// IsInt32 reports whether v is a number that is exactly representable as an
// int32 (excluding -0), and returns that integer.
func (v Value) IsInt32() (int32, bool) {
	if !v.IsNumber() {
		return 0, false
	}
	f := v.AsNumber()
	i := int32(f)
	if float64(i) != f || (f == 0 && v.bits != 0) {
		return 0, false
	}
	return i, true
}

// IsArrayIndex reports whether v is a number that is a canonical array index
// (an integer in [0, 2^32-2]).
func (v Value) IsArrayIndex() (uint32, bool) {
	if !v.IsNumber() {
		return 0, false
	}
	f := v.AsNumber()
	if f < 0 || f >= maxArrayIndex+1 {
		return 0, false
	}
	u := uint32(f)
	if float64(u) != f || (f == 0 && v.bits != 0) {
		return 0, false
	}
	return u, true
}

// String returns a debugging representation. It never throws and never
// invokes user code; use Realm.ToString for JavaScript semantics. A BigInt
// of more than 6400 bits reads "<a very large BigInt>" (see
// bigintDisplayBits).
func (v Value) String() string {
	switch v.Type() {
	case TypeUndefined:
		return "undefined"
	case TypeNull:
		return "null"
	case TypeBoolean:
		if v.AsBool() {
			return "true"
		}
		return "false"
	case TypeNumber:
		return NumberToGoString(v.AsNumber())
	case TypeString:
		return v.AsString().GoString()
	case TypeSymbol:
		return v.AsSymbol().descriptiveString()
	case TypeBigInt:
		return v.AsBigInt().displayString()
	case TypeObject:
		return v.AsObject().debugString()
	}
	return "<hole>"
}

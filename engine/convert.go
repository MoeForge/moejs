package engine

import (
	"errors"
	"math"
)

// errHole is the internal error for a hole marker reaching a conversion.
// Holes never leave the engine; should a bug let one through,
// the conversion must fail as an internal error instead of recursing through
// ToPrimitive until the Go stack overflows and takes the process down.
var errHole = errors.New("engine: internal hole marker reached a conversion")

// Hint is the ToPrimitive preferred type.
type Hint uint8

const (
	HintDefault Hint = iota
	HintNumber
	HintString
)

var (
	posInf       = math.Inf(1)
	negInf       = math.Inf(-1)
	negativeZero = math.Copysign(0, -1)
)

// ToPrimitive implements ToPrimitive, including the @@toPrimitive lookup.
func (r *Realm) ToPrimitive(v Value, hint Hint) (Value, error) {
	if !v.IsObject() {
		return v, nil
	}
	o := v.AsObject()
	if !r.lacksWellKnown(o, SymbolKey(SymToPrimitive)) {
		exotic, err := r.GetMethod(v, SymbolKey(SymToPrimitive))
		if err != nil {
			return Undefined(), err
		}
		if !exotic.IsUndefined() {
			res, err := r.CallObject(exotic.AsObject(), v, []Value{StringValue(hintAtoms[hint])})
			if err != nil {
				return Undefined(), err
			}
			if res.IsObject() {
				return Undefined(), r.TypeError("Cannot convert object to primitive value")
			}
			return res, nil
		}
	}
	if hint == HintDefault {
		// Date.prototype[@@toPrimitive] treats "default" as "string"; every
		// other object treats it as "number". The class check covers a Date
		// prototype without the method (it is installed with Date's text
		// support).
		if o.class == ClassDate {
			hint = HintString
		} else {
			hint = HintNumber
		}
	}
	return r.OrdinaryToPrimitive(o, hint)
}

// hintAtoms are the ToPrimitive hint strings passed to @@toPrimitive.
var hintAtoms = [...]*String{HintDefault: AtomDefault, HintNumber: AtomTypeNumber, HintString: AtomTypeString}

// OrdinaryToPrimitive implements OrdinaryToPrimitive.
func (r *Realm) OrdinaryToPrimitive(o *Object, hint Hint) (Value, error) {
	first, second := AtomValueOf, AtomToString
	if hint == HintString {
		first, second = AtomToString, AtomValueOf
	}
	for _, name := range [...]*String{first, second} {
		m, err := o.GetProp(r, StringKey(name))
		if err != nil {
			return Undefined(), err
		}
		if !IsCallable(m) {
			continue
		}
		res, err := r.CallObject(m.AsObject(), ObjectValue(o), nil)
		if err != nil {
			return Undefined(), err
		}
		if !res.IsObject() {
			return res, nil
		}
	}
	return Undefined(), r.TypeError("Cannot convert object to primitive value")
}

// ToBoolean implements ToBoolean.
func ToBoolean(v Value) bool {
	switch v.Type() {
	case TypeBoolean:
		return v.AsBool()
	case TypeNumber:
		f := v.AsNumber()
		return f != 0 && f == f
	case TypeString:
		return v.AsString().Len() != 0
	case TypeUndefined, TypeNull, TypeHole:
		return false
	case TypeBigInt:
		return !v.AsBigInt().IsZero()
	}
	return true
}

// ToNumber implements ToNumber.
func (r *Realm) ToNumber(v Value) (float64, error) {
	switch v.Type() {
	case TypeNumber:
		return v.AsNumber(), nil
	case TypeUndefined:
		return math.NaN(), nil
	case TypeNull:
		return 0, nil
	case TypeBoolean:
		if v.AsBool() {
			return 1, nil
		}
		return 0, nil
	case TypeString:
		return StringToNumber(v.AsString()), nil
	case TypeSymbol:
		return 0, r.TypeError("Cannot convert a Symbol value to a number")
	case TypeBigInt:
		return 0, r.TypeError("Cannot convert a BigInt value to a number")
	case TypeHole:
		return 0, errHole
	}
	p, err := r.ToPrimitive(v, HintNumber)
	if err != nil {
		return 0, err
	}
	return r.ToNumber(p)
}

// ToNumeric implements ToNumeric: the result is a number or a bigint Value.
func (r *Realm) ToNumeric(v Value) (Value, error) {
	p, err := r.ToPrimitive(v, HintNumber)
	if err != nil {
		return Undefined(), err
	}
	if p.IsBigInt() {
		return p, nil
	}
	f, err := r.ToNumber(p)
	if err != nil {
		return Undefined(), err
	}
	return NumberValue(f), nil
}

// ToString implements ToString.
func (r *Realm) ToString(v Value) (*String, error) {
	switch v.Type() {
	case TypeString:
		return v.AsString(), nil
	case TypeNumber:
		return NumberToString(v.AsNumber()), nil
	case TypeUndefined:
		return AtomUndefined, nil
	case TypeNull:
		return AtomNull, nil
	case TypeBoolean:
		if v.AsBool() {
			return AtomTrue, nil
		}
		return AtomFalse, nil
	case TypeSymbol:
		return nil, r.TypeError("Cannot convert a Symbol value to a string")
	case TypeBigInt:
		if err := r.checkBigIntFormat(v.AsBigInt()); err != nil {
			return nil, err
		}
		return asciiString(v.AsBigInt().ToString()), nil
	case TypeHole:
		return nil, errHole
	}
	p, err := r.ToPrimitive(v, HintString)
	if err != nil {
		return nil, err
	}
	return r.ToString(p)
}

// ToObject implements ToObject.
func (r *Realm) ToObject(v Value) (*Object, error) {
	switch v.Type() {
	case TypeObject:
		return v.AsObject(), nil
	case TypeString:
		return r.NewStringObject(v.AsString()), nil
	case TypeNumber:
		o := r.newObject(ClassNumber, r.rootShapeFor(r.NumberPrototype))
		setExtData(o, kindPrimitive, &primitiveWrapper{value: v})
		return o, nil
	case TypeBoolean:
		o := r.newObject(ClassBoolean, r.rootShapeFor(r.BooleanPrototype))
		setExtData(o, kindPrimitive, &primitiveWrapper{value: v})
		return o, nil
	case TypeSymbol:
		o := r.newObject(ClassSymbol, r.rootShapeFor(r.SymbolPrototype))
		setExtData(o, kindSymbol, v.AsSymbol())
		return o, nil
	case TypeBigInt:
		r.lateAt(lateBigInt)
		o := r.newObject(ClassBigInt, r.rootShapeFor(r.BigIntPrototype))
		setExtData(o, kindBigInt, v.AsBigInt())
		return o, nil
	}
	return nil, r.TypeError("Cannot convert undefined or null to object")
}

// NewStringObject creates a String exotic wrapper.
func (r *Realm) NewStringObject(s *String) *Object {
	o := r.newObject(ClassString, r.rootShapeFor(r.StringPrototype))
	setExtData(o, kindString, s)
	return o
}

// ToIntegerOrInfinityFloat implements ToIntegerOrInfinity on a number.
func ToIntegerOrInfinityFloat(f float64) float64 {
	if f != f {
		return 0
	}
	if math.IsInf(f, 0) {
		return f
	}
	t := math.Trunc(f)
	if t == 0 {
		return 0 // fold -0
	}
	return t
}

// ToIntegerOrInfinity implements ToIntegerOrInfinity.
func (r *Realm) ToIntegerOrInfinity(v Value) (float64, error) {
	f, err := r.ToNumber(v)
	if err != nil {
		return 0, err
	}
	return ToIntegerOrInfinityFloat(f), nil
}

// ToInt32Float implements ToInt32 on a number.
func ToInt32Float(f float64) int32 {
	if f >= -2147483648 && f <= 2147483647 {
		return int32(f) // truncates toward zero; handles -0
	}
	if f > -1<<63 && f < 1<<63 {
		return int32(int64(f)) // the low 32 bits of the truncated value
	}
	if f != f || math.IsInf(f, 0) {
		return 0
	}
	f = math.Mod(math.Trunc(f), 4294967296)
	return int32(uint32(int64(f)))
}

// ToUint32Float implements ToUint32 on a number.
func ToUint32Float(f float64) uint32 {
	if f >= 0 && f <= 4294967295 {
		return uint32(f)
	}
	if f > -1<<63 && f < 1<<63 {
		return uint32(int64(f))
	}
	if f != f || math.IsInf(f, 0) {
		return 0
	}
	f = math.Mod(math.Trunc(f), 4294967296)
	return uint32(int64(f))
}

// ToInt32 implements ToInt32.
func (r *Realm) ToInt32(v Value) (int32, error) {
	if v.IsNumber() {
		return ToInt32Float(v.AsNumber()), nil
	}
	f, err := r.ToNumber(v)
	if err != nil {
		return 0, err
	}
	return ToInt32Float(f), nil
}

// ToUint32 implements ToUint32.
func (r *Realm) ToUint32(v Value) (uint32, error) {
	if v.IsNumber() {
		return ToUint32Float(v.AsNumber()), nil
	}
	f, err := r.ToNumber(v)
	if err != nil {
		return 0, err
	}
	return ToUint32Float(f), nil
}

// maxSafeInteger is 2^53 - 1.
const maxSafeInteger = 9007199254740991

// ToLength implements ToLength.
func (r *Realm) ToLength(v Value) (int64, error) {
	f, err := r.ToIntegerOrInfinity(v)
	if err != nil {
		return 0, err
	}
	if f <= 0 {
		return 0, nil
	}
	if f >= maxSafeInteger {
		return maxSafeInteger, nil
	}
	return int64(f), nil
}

// ToIndex implements ToIndex.
func (r *Realm) ToIndex(v Value) (int64, error) {
	if v.IsUndefined() {
		return 0, nil
	}
	f, err := r.ToIntegerOrInfinity(v)
	if err != nil {
		return 0, err
	}
	if f < 0 || f > maxSafeInteger {
		return 0, r.RangeError("Invalid index")
	}
	return int64(f), nil
}

// GetV implements GetV: property lookup on any value, using the primitive's
// prototype without allocating a wrapper.
func (r *Realm) GetV(v Value, key PropertyKey) (Value, error) {
	switch v.Type() {
	case TypeObject:
		return v.AsObject().Get(r, key, v)
	case TypeString:
		s := v.AsString()
		if key.IsIndex() {
			if i := key.Index(); int64(i) < int64(s.Len()) {
				return StringValue(s.Substring(int(i), int(i)+1)), nil
			}
		} else if key == lengthKey {
			return IntValue(s.Len()), nil
		}
		return r.StringPrototype.Get(r, key, v)
	case TypeNumber:
		return r.NumberPrototype.Get(r, key, v)
	case TypeBoolean:
		return r.BooleanPrototype.Get(r, key, v)
	case TypeBigInt:
		r.lateAt(lateBigInt)
		return r.BigIntPrototype.Get(r, key, v)
	case TypeSymbol:
		return r.SymbolPrototype.Get(r, key, v)
	}
	return Undefined(), r.TypeError("Cannot read property '%s' of %s", key.GoString(), v.String())
}

// GetMethod implements GetMethod.
func (r *Realm) GetMethod(v Value, key PropertyKey) (Value, error) {
	f, err := r.GetV(v, key)
	if err != nil {
		return Undefined(), err
	}
	if f.IsNullish() {
		return Undefined(), nil
	}
	if !IsCallable(f) {
		return Undefined(), r.TypeError("%s is not a function", r.DisplayString(f))
	}
	return f, nil
}

// SetV implements strict-mode PutValue on any base value.
func (r *Realm) SetV(target Value, key PropertyKey, v Value) error {
	switch target.Type() {
	case TypeObject:
		return target.AsObject().SetProp(r, key, v)
	case TypeUndefined, TypeNull:
		return r.TypeError("Cannot set property '%s' of %s", key.GoString(), target.String())
	}
	proto, err := r.protoForPrimitive(target)
	if err != nil {
		return err
	}
	ok, err := proto.Set(r, key, v, target)
	if err != nil {
		return err
	}
	if !ok {
		return r.TypeError("Cannot create property '%s' on %s", key.GoString(), r.DisplayString(target))
	}
	return nil
}

func (r *Realm) protoForPrimitive(v Value) (*Object, error) {
	switch v.Type() {
	case TypeString:
		return r.StringPrototype, nil
	case TypeNumber:
		return r.NumberPrototype, nil
	case TypeBoolean:
		return r.BooleanPrototype, nil
	case TypeSymbol:
		return r.SymbolPrototype, nil
	case TypeBigInt:
		r.lateAt(lateBigInt)
		return r.BigIntPrototype, nil
	}
	return nil, r.TypeError("Cannot convert undefined or null to object")
}

// LengthOfArrayLike implements LengthOfArrayLike.
func (r *Realm) LengthOfArrayLike(o *Object) (int64, error) {
	if o.class == ClassArray {
		return int64(o.arrayLen()), nil
	}
	v, err := o.GetProp(r, lengthKey)
	if err != nil {
		return 0, err
	}
	return r.ToLength(v)
}

// CreateListFromArrayLike implements CreateListFromArrayLike (all element
// types allowed).
func (r *Realm) CreateListFromArrayLike(v Value) ([]Value, error) {
	if !v.IsObject() {
		return nil, r.TypeError("CreateListFromArrayLike called on non-object")
	}
	o := v.AsObject()
	if list, ok := r.denseList(o); ok {
		return list, nil
	}
	n, err := r.LengthOfArrayLike(o)
	if err != nil {
		return nil, err
	}
	if n > 1<<24 {
		return nil, r.RangeError("Too many arguments in function call (only %d allowed)", 1<<24)
	}
	list := make([]Value, n)
	for i := range list {
		if list[i], err = o.GetIndex(r, uint32(i)); err != nil {
			return nil, err
		}
		if err := interruptEvery(r, int64(i)); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// denseList is CreateListFromArrayLike of an array or an arguments object
// whose length is at most denseListMax and its first length elements all
// in the dense storage, where no Get of one runs code: an ordinary array, or
// an arguments object whose properties are still the ones it was created
// with (its length an integer, as written to its slot). Else ok is false.
func (r *Realm) denseList(o *Object) (_ []Value, ok bool) {
	var n int
	switch o.class {
	case ClassArray:
		if o.flags&(flagDict|flagHasLazy) != 0 {
			return nil, false
		}
		n = int(o.arrayLen())
	case ClassArguments:
		l := r.shapeState()
		if o.shape != l.arguments && o.shape != l.mapped {
			return nil, false
		}
		k, isInt := o.slots[0].IsInt32()
		if !isInt || k < 0 {
			return nil, false
		}
		n = int(k)
	default:
		return nil, false
	}
	e := o.elems()
	if n > len(e) || n > denseListMax {
		return nil, false
	}
	for _, v := range e[:n] {
		if v.IsHole() {
			return nil, false
		}
	}
	list := make([]Value, n)
	copy(list, e)
	return list, true
}

// denseListMax bounds the lists denseList copies with no interrupt check.
const denseListMax = 1 << 16

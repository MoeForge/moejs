package engine

import (
	"math"
	"math/big"

	"github.com/MoeForge/moejs/bytecode"
)

// SameValue implements SameValue (NaN equals NaN, +0 differs from -0).
func SameValue(a, b Value) bool {
	if a.IsNumber() && b.IsNumber() {
		return a.bits == b.bits // canonical NaN makes NaN == NaN; ±0 differ
	}
	return sameNonNumber(a, b)
}

// SameValueZero implements SameValueZero (NaN equals NaN, +0 equals -0).
func SameValueZero(a, b Value) bool {
	if a.IsNumber() && b.IsNumber() {
		x, y := a.AsNumber(), b.AsNumber()
		return x == y || (x != x && y != y)
	}
	return sameNonNumber(a, b)
}

// StrictEquals implements IsStrictlyEqual (===).
func StrictEquals(a, b Value) bool {
	if a.IsNumber() && b.IsNumber() {
		return a.AsNumber() == b.AsNumber()
	}
	return sameNonNumber(a, b)
}

func sameNonNumber(a, b Value) bool {
	if a.bits != b.bits {
		return false
	}
	if a.ptr == b.ptr {
		return true
	}
	if a.ptr == nil || b.ptr == nil {
		return false
	}
	switch a.bits {
	case tagString:
		return a.AsString().Equals(b.AsString())
	case tagBigInt:
		return a.AsBigInt().StrictEquals(b.AsBigInt())
	}
	return false
}

// LooseEquals implements IsLooselyEqual (==).
func (r *Realm) LooseEquals(a, b Value) (bool, error) {
	ta, tb := a.Type(), b.Type()
	if ta == tb {
		return StrictEquals(a, b), nil
	}
	if a.IsNullish() && b.IsNullish() {
		return true, nil
	}
	switch {
	case ta == TypeNumber && tb == TypeString:
		return a.AsNumber() == StringToNumber(b.AsString()), nil
	case ta == TypeString && tb == TypeNumber:
		return StringToNumber(a.AsString()) == b.AsNumber(), nil
	case ta == TypeBigInt && tb == TypeString:
		if err := r.checkBigIntParse(b.AsString()); err != nil {
			return false, err
		}
		n, ok := StringToBigInt(b.AsString())
		return ok && a.AsBigInt().StrictEquals(n), nil
	case ta == TypeString && tb == TypeBigInt:
		return r.LooseEquals(b, a)
	case ta == TypeBoolean:
		return r.LooseEquals(NumberValue(boolToFloat(a.AsBool())), b)
	case tb == TypeBoolean:
		return r.LooseEquals(a, NumberValue(boolToFloat(b.AsBool())))
	case tb == TypeObject && (ta == TypeString || ta == TypeNumber || ta == TypeBigInt || ta == TypeSymbol):
		p, err := r.ToPrimitive(b, HintDefault)
		if err != nil {
			return false, err
		}
		return r.LooseEquals(a, p)
	case ta == TypeObject && (tb == TypeString || tb == TypeNumber || tb == TypeBigInt || tb == TypeSymbol):
		p, err := r.ToPrimitive(a, HintDefault)
		if err != nil {
			return false, err
		}
		return r.LooseEquals(p, b)
	case ta == TypeBigInt && tb == TypeNumber:
		return bigIntEqualsNumber(a.AsBigInt(), b.AsNumber()), nil
	case ta == TypeNumber && tb == TypeBigInt:
		return bigIntEqualsNumber(b.AsBigInt(), a.AsNumber()), nil
	}
	return false, nil
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func bigIntEqualsNumber(b *BigInt, f float64) bool {
	if f != f || math.IsInf(f, 0) || f != math.Trunc(f) {
		return false
	}
	bf := new(big.Float).SetInt(&b.v)
	return bf.Cmp(big.NewFloat(f)) == 0
}

// Comparison results of IsLessThan.
const (
	CompareFalse     int8 = 0
	CompareTrue      int8 = 1
	CompareUndefined int8 = -1
)

// IsLessThan implements IsLessThan(x, y, leftFirst).
func (r *Realm) IsLessThan(x, y Value, leftFirst bool) (int8, error) {
	if x.IsNumber() && y.IsNumber() {
		return lessNumbers(x.AsNumber(), y.AsNumber()), nil
	}
	var px, py Value
	var err error
	if leftFirst {
		if px, err = r.ToPrimitive(x, HintNumber); err != nil {
			return CompareFalse, err
		}
		if py, err = r.ToPrimitive(y, HintNumber); err != nil {
			return CompareFalse, err
		}
	} else {
		if py, err = r.ToPrimitive(y, HintNumber); err != nil {
			return CompareFalse, err
		}
		if px, err = r.ToPrimitive(x, HintNumber); err != nil {
			return CompareFalse, err
		}
	}
	if px.IsString() && py.IsString() {
		if px.AsString().Compare(py.AsString()) < 0 {
			return CompareTrue, nil
		}
		return CompareFalse, nil
	}
	if px.IsBigInt() && py.IsString() {
		if err := r.checkBigIntParse(py.AsString()); err != nil {
			return CompareFalse, err
		}
		ny, over, ok := parseBigInt(py.AsString())
		switch {
		case !ok:
			return CompareUndefined, nil
		case over != 0: // a literal past the size limit exceeds every bigint
			return boolCompare(over > 0), nil
		}
		return boolCompare(px.AsBigInt().v.Cmp(&ny.v) < 0), nil
	}
	if px.IsString() && py.IsBigInt() {
		if err := r.checkBigIntParse(px.AsString()); err != nil {
			return CompareFalse, err
		}
		nx, over, ok := parseBigInt(px.AsString())
		switch {
		case !ok:
			return CompareUndefined, nil
		case over != 0:
			return boolCompare(over < 0), nil
		}
		return boolCompare(nx.v.Cmp(&py.AsBigInt().v) < 0), nil
	}
	nx, err := r.ToNumeric(px)
	if err != nil {
		return CompareFalse, err
	}
	ny, err := r.ToNumeric(py)
	if err != nil {
		return CompareFalse, err
	}
	switch {
	case nx.IsNumber() && ny.IsNumber():
		return lessNumbers(nx.AsNumber(), ny.AsNumber()), nil
	case nx.IsBigInt() && ny.IsBigInt():
		return boolCompare(nx.AsBigInt().v.Cmp(&ny.AsBigInt().v) < 0), nil
	case nx.IsBigInt():
		return lessBigIntNumber(nx.AsBigInt(), ny.AsNumber(), false), nil
	}
	return lessBigIntNumber(ny.AsBigInt(), nx.AsNumber(), true), nil
}

func lessNumbers(x, y float64) int8 {
	if x != x || y != y {
		return CompareUndefined
	}
	return boolCompare(x < y)
}

func boolCompare(b bool) int8 {
	if b {
		return CompareTrue
	}
	return CompareFalse
}

// lessBigIntNumber compares b < f (or f < b when swapped).
func lessBigIntNumber(b *BigInt, f float64, swapped bool) int8 {
	if f != f {
		return CompareUndefined
	}
	if math.IsInf(f, 1) {
		return boolCompare(!swapped)
	}
	if math.IsInf(f, -1) {
		return boolCompare(swapped)
	}
	c := new(big.Float).SetInt(&b.v).Cmp(big.NewFloat(f))
	if swapped {
		return boolCompare(c > 0)
	}
	return boolCompare(c < 0)
}

// LessThan implements x < y.
func (r *Realm) LessThan(x, y Value) (bool, error) {
	c, err := r.IsLessThan(x, y, true)
	return c == CompareTrue, err
}

// GreaterThan implements x > y.
func (r *Realm) GreaterThan(x, y Value) (bool, error) {
	c, err := r.IsLessThan(y, x, false)
	return c == CompareTrue, err
}

// LessThanOrEqual implements x <= y.
func (r *Realm) LessThanOrEqual(x, y Value) (bool, error) {
	c, err := r.IsLessThan(y, x, false)
	return c == CompareFalse, err
}

// GreaterThanOrEqual implements x >= y.
func (r *Realm) GreaterThanOrEqual(x, y Value) (bool, error) {
	c, err := r.IsLessThan(x, y, true)
	return c == CompareFalse, err
}

// concatValues is Concat as a Value result (the string case of `+`).
func (r *Realm) concatValues(a, b *String) (Value, error) {
	s, err := r.Concat(a, b)
	if err != nil {
		return Undefined(), err
	}
	return StringValue(s), nil
}

// concatNumber is a + b for a string and a number, either way round, when
// the string is ASCII and the result short enough to be flat: the digits go
// straight into the result instead of into the string ToString of the
// number would make first. ok is false otherwise.
func concatNumber(a, b Value) (Value, bool) {
	var (
		s     *String
		f     float64
		first bool
	)
	switch {
	case a.IsString() && b.IsNumber():
		s, f = a.AsString(), b.AsNumber()
	case a.IsNumber() && b.IsString():
		s, f, first = b.AsString(), a.AsNumber(), true
	default:
		return Value{}, false
	}
	if s.kind != strASCII || s.n == 0 {
		return Value{}, false
	}
	var buf [32]byte
	num := AppendNumber(buf[:0], f)
	n := int(s.n) + len(num)
	if n >= ropeThreshold {
		return Value{}, false
	}
	z, dst := newASCIIBuf(n)
	if first {
		copy(dst[copy(dst, num):], s.s)
	} else {
		copy(dst[copy(dst, s.s):], num)
	}
	return StringValue(z), true
}

// Add implements the `+` operator.
func (r *Realm) Add(a, b Value) (Value, error) {
	if a.IsNumber() && b.IsNumber() {
		return NumberValue(a.AsNumber() + b.AsNumber()), nil
	}
	if a.IsString() && b.IsString() {
		return r.concatValues(a.AsString(), b.AsString())
	}
	if v, ok := concatNumber(a, b); ok {
		r.chargeString(v.AsString().Len())
		return v, nil
	}
	pa, err := r.ToPrimitive(a, HintDefault)
	if err != nil {
		return Undefined(), err
	}
	pb, err := r.ToPrimitive(b, HintDefault)
	if err != nil {
		return Undefined(), err
	}
	if pa.IsString() || pb.IsString() {
		sa, err := r.ToString(pa)
		if err != nil {
			return Undefined(), err
		}
		sb, err := r.ToString(pb)
		if err != nil {
			return Undefined(), err
		}
		return r.concatValues(sa, sb)
	}
	na, err := r.ToNumeric(pa)
	if err != nil {
		return Undefined(), err
	}
	nb, err := r.ToNumeric(pb)
	if err != nil {
		return Undefined(), err
	}
	if na.IsBigInt() || nb.IsBigInt() {
		return r.bigintBinary(bytecode.Add, na, nb)
	}
	return NumberValue(na.AsNumber() + nb.AsNumber()), nil
}

// TypeOf implements the typeof operator, returning an atom.
func TypeOf(v Value) *String {
	switch v.Type() {
	case TypeUndefined:
		return AtomUndefined
	case TypeNull:
		return AtomTypeObject
	case TypeBoolean:
		return AtomTypeBoolean
	case TypeNumber:
		return AtomTypeNumber
	case TypeString:
		return AtomTypeString
	case TypeSymbol:
		return AtomTypeSymbol
	case TypeBigInt:
		return AtomTypeBigint
	case TypeObject:
		if v.AsObject().IsCallable() {
			return AtomTypeFunction
		}
		return AtomTypeObject
	}
	return AtomUndefined
}

// InstanceOf implements InstanceofOperator. A lookup of @@hasInstance that
// finds the original Function.prototype[@@hasInstance] skips the call.
func (r *Realm) InstanceOf(v, target Value) (bool, error) {
	if !target.IsObject() {
		return false, r.TypeError("Right-hand side of 'instanceof' is not an object")
	}
	if h, err := r.hasInstanceMethod(target.AsObject()); err != nil || h != nil {
		if err != nil {
			return false, err
		}
		res, err := r.CallObject(h, target, []Value{v})
		if err != nil {
			return false, err
		}
		return ToBoolean(res), nil
	}
	if !IsCallable(target) {
		return false, r.TypeError("Right-hand side of 'instanceof' is not callable")
	}
	return r.OrdinaryHasInstance(target, v)
}

// OrdinaryHasInstance implements OrdinaryHasInstance(C, O).
func (r *Realm) OrdinaryHasInstance(c, o Value) (bool, error) {
	if !IsCallable(c) {
		return false, nil
	}
	if fd := c.AsObject().FunctionData(); fd != nil && fd.kind == FuncBound {
		return r.InstanceOf(o, ObjectValue(fd.bound().target))
	}
	if !o.IsObject() {
		return false, nil
	}
	pv, err := c.AsObject().GetProp(r, StringKey(AtomPrototype))
	if err != nil {
		return false, err
	}
	if !pv.IsObject() {
		return false, r.TypeError("Function has non-object prototype '%s' in instanceof check", pv.String())
	}
	p := pv.AsObject()
	obj := o.AsObject()
	for i := int64(0); ; i++ {
		if obj, err = r.getPrototypeOf(obj); err != nil || obj == nil {
			return false, err
		}
		if obj == p {
			return true, nil
		}
		if err := interruptEvery(r, i); err != nil {
			return false, err
		}
	}
}

// HasPropertyIn implements the `in` operator: key in target.
func (r *Realm) HasPropertyIn(key, target Value) (bool, error) {
	if !target.IsObject() {
		return false, r.TypeError("Cannot use 'in' operator to search for '%s' in %s", key.String(), r.DisplayString(target))
	}
	k, err := r.ToPropertyKey(key)
	if err != nil {
		return false, err
	}
	return r.hasProperty(target.AsObject(), k)
}

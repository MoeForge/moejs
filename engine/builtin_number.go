package engine

import (
	"math"
	"math/big"
	"math/bits"
	"strconv"
)

// installNumber fills the Number constructor and Number.prototype.
// Number.parseInt and Number.parseFloat are the very function objects bound
// on the global object, so installGlobal must have run already.
func installNumber(r *Realm) {
	r.NumberCtor.ReserveSlots(r, len(numberConstants)+2+len(numberStaticMethods))
	r.installValues(r.NumberCtor, numberConstants)
	parseInt, _ := r.Global.GetOwnDataValue(StringKey(AtomParseInt))
	parseFloat, _ := r.Global.GetOwnDataValue(StringKey(AtomParseFloat))
	r.installValues(r.NumberCtor, []valueDef{
		{AtomParseInt, parseInt, attrHidden},
		{AtomParseFloat, parseFloat, attrHidden},
	})
	r.installBuiltins(r.NumberCtor, numberStaticMethods)
	r.installBuiltins(r.NumberPrototype, numberPrototypeMethods)
}

var numberConstants = []valueDef{
	{AtomEPSILON, NumberValue(math.Nextafter(1, 2) - 1), attrFrozen},
	{AtomMaxSafeInteger, NumberValue(maxSafeInteger), attrFrozen},
	{AtomMaxValue, NumberValue(math.MaxFloat64), attrFrozen},
	{AtomMinSafeInteger, NumberValue(-maxSafeInteger), attrFrozen},
	{AtomMinValue, NumberValue(math.SmallestNonzeroFloat64), attrFrozen},
	{AtomNaN, NaN(), attrFrozen},
	{AtomNegativeInfinity, NumberValue(negInf), attrFrozen},
	{AtomPositiveInfinity, NumberValue(posInf), attrFrozen},
}

var numberStaticMethods = []builtinDef{
	{AtomIsFinite, numberIsFinite, 1},
	{AtomIsInteger, numberIsInteger, 1},
	{AtomIsNaN, numberIsNaN, 1},
	{AtomIsSafeInteger, numberIsSafeInteger, 1},
}

var numberPrototypeMethods = []builtinDef{
	{AtomToExponential, numberProtoToExponential, 1},
	{AtomToFixed, numberProtoToFixed, 1},
	{AtomToLocaleString, numberProtoToLocaleString, 0},
	{AtomToPrecision, numberProtoToPrecision, 1},
	{AtomToString, numberProtoToString, 1},
	{AtomValueOf, numberProtoValueOf, 0},
}

// --- constructor ---------------------------------------------------------------

// numberCall implements Number(value).
func numberCall(r *Realm, this Value, args []Value) (Value, error) {
	return numberValueOfArgs(r, args)
}

// numberValueOfArgs performs the ToNumeric step shared by Number(value) and
// new Number(value); a BigInt converts to the nearest number (spec 21.1.1.1).
func numberValueOfArgs(r *Realm, args []Value) (Value, error) {
	if len(args) == 0 {
		return IntValue(0), nil
	}
	n, err := r.ToNumeric(args[0])
	if err != nil {
		return Undefined(), err
	}
	if n.IsBigInt() {
		return NumberValue(n.AsBigInt().Float64()), nil
	}
	return n, nil
}

// numberConstruct implements new Number(value).
func numberConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	n, err := numberValueOfArgs(r, args)
	if err != nil {
		return Undefined(), err
	}
	o, err := r.OrdinaryCreateFromConstructor(newTarget, r.NumberPrototype, ClassNumber)
	if err != nil {
		return Undefined(), err
	}
	setExtData(o, kindPrimitive, &primitiveWrapper{value: n})
	return ObjectValue(o), nil
}

// --- static methods -----------------------------------------------------------------

func numberIsFinite(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	if !v.IsNumber() {
		return False(), nil
	}
	f := v.AsNumber()
	return Bool(!math.IsNaN(f) && !math.IsInf(f, 0)), nil
}

func numberIsInteger(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	return Bool(v.IsNumber() && isIntegralNumber(v.AsNumber())), nil
}

func numberIsNaN(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	return Bool(v.IsNumber() && v.AsNumber() != v.AsNumber()), nil
}

func numberIsSafeInteger(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	if !v.IsNumber() {
		return False(), nil
	}
	f := v.AsNumber()
	return Bool(isIntegralNumber(f) && math.Abs(f) <= maxSafeInteger), nil
}

// isIntegralNumber implements IsIntegralNumber on a number.
func isIntegralNumber(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
}

// --- prototype methods --------------------------------------------------------------

// thisNumberValue implements ThisNumberValue.
func thisNumberValue(r *Realm, this Value, method string) (float64, error) {
	if this.IsNumber() {
		return this.AsNumber(), nil
	}
	if this.IsObject() && this.AsObject().class == ClassNumber {
		return extDataOf(this.AsObject(), kindPrimitive).value.AsNumber(), nil
	}
	return 0, r.TypeError("Number.prototype.%s requires that 'this' be a Number", method)
}

func numberProtoValueOf(r *Realm, this Value, args []Value) (Value, error) {
	f, err := thisNumberValue(r, this, "valueOf")
	if err != nil {
		return Undefined(), err
	}
	return NumberValue(f), nil
}

// numberProtoToString implements Number.prototype.toString(radix).
func numberProtoToString(r *Realm, this Value, args []Value) (Value, error) {
	f, err := thisNumberValue(r, this, "toString")
	if err != nil {
		return Undefined(), err
	}
	radix := 10.0
	if rv := Arg(args, 0); !rv.IsUndefined() {
		if radix, err = r.ToIntegerOrInfinity(rv); err != nil {
			return Undefined(), err
		}
	}
	if radix < 2 || radix > 36 {
		return Undefined(), r.RangeError("toString() radix must be between 2 and 36")
	}
	if radix == 10 {
		return StringValue(NumberToString(f)), nil
	}
	return StringValue(asciiString(NumberToStringRadix(f, int(radix)))), nil
}

// numberProtoToFixed implements Number.prototype.toFixed (ES2023 21.1.3.3)
// with exact decimal rounding (ties go to the larger value).
func numberProtoToFixed(r *Realm, this Value, args []Value) (Value, error) {
	x, err := thisNumberValue(r, this, "toFixed")
	if err != nil {
		return Undefined(), err
	}
	f, err := r.ToIntegerOrInfinity(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if math.IsInf(f, 0) || f < 0 || f > 100 {
		return Undefined(), r.RangeError("toFixed() digits argument must be between 0 and 100")
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return StringValue(NumberToString(x)), nil
	}
	digits := int(f)
	// The digits are built in a buffer on the stack, and the result is one
	// allocation, its header and bytes together when short. A result past
	// 64 bytes, (1e20).toFixed(100), grows the buffer on the heap first.
	var buf [64]byte
	out := buf[:0]
	if x < 0 {
		out = append(out, '-')
		x = -x
	}
	if x >= 1e21 {
		out = append(out, NumberToGoString(x)...)
		return StringValue(asciiStringCopy(out)), nil
	}
	// m is x * 10^digits as decimal digits, appended to out at k.
	k := len(out)
	switch {
	case x == 0:
		out = append(out, '0')
	case x == math.Trunc(x) && x < 1<<53:
		// x * 10^digits is an integer: the digits of x followed by zeros.
		out = appendZeros(strconv.AppendInt(out, int64(x), 10), digits)
	default:
		var ok bool
		if out, ok = appendFixedExact(out, x, digits); !ok {
			out = append(out[:k], decimalScale(x, digits, true).String()...)
		}
	}
	if digits != 0 {
		if m := len(out) - k; m <= digits {
			// 0.0...0m: the zeros go before m.
			z := digits + 1 - m
			out = appendZeros(out, z)
			copy(out[k+z:], out[k:k+m])
			for i := k; i < k+z; i++ {
				out[i] = '0'
			}
		}
		// The point goes before the last digits digits.
		at := len(out) - digits
		out = append(out, 0)
		copy(out[at+1:], out[at:])
		out[at] = '.'
	}
	return StringValue(asciiStringCopy(out)), nil
}

// numberProtoToExponential implements Number.prototype.toExponential
// (ES2023 21.1.3.2).
func numberProtoToExponential(r *Realm, this Value, args []Value) (Value, error) {
	x, err := thisNumberValue(r, this, "toExponential")
	if err != nil {
		return Undefined(), err
	}
	fv := Arg(args, 0)
	f, err := r.ToIntegerOrInfinity(fv)
	if err != nil {
		return Undefined(), err
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return StringValue(NumberToString(x)), nil
	}
	if math.IsInf(f, 0) || f < 0 || f > 100 {
		return Undefined(), r.RangeError("toExponential() argument must be between 0 and 100")
	}
	var out []byte
	if x < 0 {
		out = append(out, '-')
		x = -x
	}
	var digits string
	var e int
	switch {
	case x == 0:
		digits = zeroDigits(int(f) + 1)
	case fv.IsUndefined():
		digits, e = shortestDigits(x)
	default:
		digits, e = roundedDigits(x, int(f)+1)
	}
	out = append(out, digits[0])
	if len(digits) > 1 {
		out = append(out, '.')
		out = append(out, digits[1:]...)
	}
	out = appendExponent(out, e)
	return StringValue(asciiString(string(out))), nil
}

// numberProtoToPrecision implements Number.prototype.toPrecision
// (ES2023 21.1.3.5).
func numberProtoToPrecision(r *Realm, this Value, args []Value) (Value, error) {
	x, err := thisNumberValue(r, this, "toPrecision")
	if err != nil {
		return Undefined(), err
	}
	pv := Arg(args, 0)
	if pv.IsUndefined() {
		return StringValue(NumberToString(x)), nil
	}
	p, err := r.ToIntegerOrInfinity(pv)
	if err != nil {
		return Undefined(), err
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return StringValue(NumberToString(x)), nil
	}
	if math.IsInf(p, 0) || p < 1 || p > 100 {
		return Undefined(), r.RangeError("toPrecision() argument must be between 1 and 100")
	}
	prec := int(p)
	var out []byte
	if x < 0 {
		out = append(out, '-')
		x = -x
	}
	var digits string
	var e int
	if x == 0 {
		digits = zeroDigits(prec)
	} else {
		digits, e = roundedDigits(x, prec)
	}
	switch {
	case e < -6 || e >= prec:
		out = append(out, digits[0])
		if prec != 1 {
			out = append(out, '.')
			out = append(out, digits[1:]...)
		}
		out = appendExponent(out, e)
	case e == prec-1:
		out = append(out, digits...)
	case e >= 0:
		out = append(out, digits[:e+1]...)
		out = append(out, '.')
		out = append(out, digits[e+1:]...)
	default:
		out = append(out, '0', '.')
		out = append(out, zeroDigits(-(e + 1))...)
		out = append(out, digits...)
	}
	return StringValue(asciiString(string(out))), nil
}

// pow10u64 holds 10^i for the i whose power fits in 64 bits.
var pow10u64 = func() (t [20]uint64) {
	t[0] = 1
	for i := 1; i < len(t); i++ {
		t[i] = t[i-1] * 10
	}
	return
}()

// appendFixedExact appends to dst the toFixed digit string of x > 0, not
// an integer, with f digits: x * 10^f rounded half up, as decimal digits
// without the point. x is m / 2^k, so x * 10^f is the 128-bit product
// m * 10^f shifted right by k, and the bit shifted out first tells whether
// it rounds up. It returns dst unchanged and false when 10^f or the result
// does not fit in 64 bits.
func appendFixedExact(dst []byte, x float64, f int) ([]byte, bool) {
	b := math.Float64bits(x)
	m, e := b&(1<<52-1), int(b>>52&0x7FF)
	if e == 0 {
		e = 1 // subnormal
	} else {
		m |= 1 << 52
	}
	k := 1075 - e
	if f >= len(pow10u64) || k <= 0 {
		return dst, false
	}
	hi, lo := bits.Mul64(m, pow10u64[f])
	var n uint64
	up := false
	switch {
	case k < 64:
		if hi>>k != 0 {
			return dst, false
		}
		n, up = hi<<(64-k)|lo>>k, lo>>(k-1)&1 != 0
	case k == 64:
		n, up = hi, lo>>63 != 0
	case k < 128:
		n, up = hi>>(k-64), hi>>(k-65)&1 != 0
	}
	// Past 127 the product, below 2^117, rounds to 0.
	if up {
		if n++; n == 0 {
			return dst, false
		}
	}
	return strconv.AppendUint(dst, n, 10), true
}

// --- exact decimal helpers -----------------------------------------------------------

var (
	bigOne = big.NewInt(1)
	bigTen = big.NewInt(10)
)

// pow10Big returns 10^n as a big integer (n >= 0).
func pow10Big(n int) *big.Int {
	return new(big.Int).Exp(bigTen, big.NewInt(int64(n)), nil)
}

// decimalScale returns x * 10^p as an integer for finite x > 0, either
// truncated (round == false) or rounded half up (round == true), computed
// exactly from the binary representation of x. p may be negative.
func decimalScale(x float64, p int, round bool) *big.Int {
	frac, exp := math.Frexp(x) // x = frac * 2^exp, frac in [0.5, 1)
	num := new(big.Int).SetInt64(int64(frac * (1 << 53)))
	den := big.NewInt(1)
	if e2 := exp - 53; e2 >= 0 {
		num.Lsh(num, uint(e2))
	} else {
		den.Lsh(den, uint(-e2))
	}
	if p >= 0 {
		num.Mul(num, pow10Big(p))
	} else {
		den.Mul(den, pow10Big(-p))
	}
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if round && rem.Lsh(rem, 1).Cmp(den) >= 0 {
		q.Add(q, bigOne)
	}
	return q
}

// decimalExponent returns floor(log10(x)) exactly for finite x > 0.
func decimalExponent(x float64) int {
	e := int(math.Floor(math.Log10(x)))
	for {
		q := decimalScale(x, -e, false)
		switch {
		case q.Sign() == 0:
			e--
		case q.Cmp(bigTen) >= 0:
			e++
		default:
			return e
		}
	}
}

// roundedDigits returns the n significant decimal digits of finite x > 0
// rounded half up, and the decimal exponent e such that the value is
// d.ddd × 10^e.
func roundedDigits(x float64, n int) (string, int) {
	e := decimalExponent(x)
	digits := decimalScale(x, n-1-e, true).String()
	if len(digits) > n {
		// Rounding carried into a new leading digit (999.5 -> 1000):
		// the digits are 1 followed by zeros, one too many.
		digits = digits[:n]
		e++
	}
	return digits, e
}

// shortestDigits returns the shortest round-trip decimal digits of finite
// x > 0 (Number::toString digits) and the decimal exponent.
func shortestDigits(x float64) (string, int) {
	var buf [32]byte
	s := strconv.AppendFloat(buf[:0], x, 'e', -1, 64) // "d.ddde±XX"
	i := 0
	for s[i] != 'e' {
		i++
	}
	mant := s[:i]
	e, _ := strconv.Atoi(string(s[i+1:]))
	if len(mant) == 1 {
		return string(mant), e
	}
	return string(mant[:1]) + string(mant[2:]), e
}

// appendExponent appends "e+dd" / "e-dd" as toExponential/toPrecision format it.
func appendExponent(out []byte, e int) []byte {
	out = append(out, 'e')
	if e < 0 {
		out = append(out, '-')
		e = -e
	} else {
		out = append(out, '+')
	}
	return strconv.AppendInt(out, int64(e), 10)
}

// zeroDigits returns n '0' characters.
func zeroDigits(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return bytesToString(b)
}

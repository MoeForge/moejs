package engine

import (
	"bytes"
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"sync"
)

// Exact decimal arithmetic of Intl.NumberFormat and Intl.PluralRules
// (ECMA-402 §16.5, §17.5): the mathematical value of a Number, a BigInt or
// a decimal string, rounded as FormatNumericToString does with every
// rounding mode, increment and rounding priority of NumberFormat v3. The
// operands and rules of CLDR's plural categories are in intl_plural.go.
//
// Nothing here goes through float64 but setFloat's digit generation: the
// rounding works on the decimal digits, so 9.995 with two fraction digits
// is 10.00 and "0.1000000000000000000001" keeps every digit.

// intlDecimal kinds.
const (
	intlFinite uint8 = iota
	intlNaN
	intlInfinity
)

// intlDecimal is an exact decimal: (-1)^neg × 0.d₁d₂…dₙ × 10^exp, with the
// ASCII digits d₁…dₙ in digits, without leading or trailing zeros (none for
// zero), or NaN or an infinity (kind). neg is also set for negative zero
// and -Infinity. The methods reuse digits' capacity, so a caller that
// points digits at a stack array formats a Number without allocating.
type intlDecimal struct {
	digits []byte
	exp    int
	neg    bool
	kind   uint8
}

// setFloat sets d to a Number: the digits of its shortest round-trip form
// (those Number::toString prints), as ICU formats a double, rather than
// the exact expansion of the binary value; negative zero keeps its sign.
func (d *intlDecimal) setFloat(f float64) {
	d.kind, d.neg, d.exp, d.digits = intlFinite, math.Signbit(f), 0, d.digits[:0]
	switch {
	case f != f:
		d.kind, d.neg = intlNaN, false
		return
	case math.IsInf(f, 0):
		d.kind = intlInfinity
		return
	case f == 0:
		return
	}
	// The digits are written into digits' array, which holds any of them.
	d.reserve(32)
	a := math.Abs(f)
	if a < 1<<53 && a == math.Trunc(a) {
		// An integer below 2^53 is its own shortest form.
		n := len(strconv.AppendUint(d.digits[:0], uint64(a), 10))
		d.digits, d.exp = d.digits[:n], n
		d.trim()
		return
	}
	// "d.ddde±xx" or "de±xx": the digits and the exponent of d₁.
	b := strconv.AppendFloat(d.digits[:0], a, 'e', -1, 64)
	i := len(b) - 1
	e, unit := 0, 1
	for ; b[i] != '+' && b[i] != '-'; i-- {
		e += int(b[i]-'0') * unit
		unit *= 10
	}
	if b[i] == '-' {
		e = -e
	}
	n := i - 1 // the 'e'
	if n > 1 {
		copy(b[1:], b[2:n])
		n--
	}
	d.digits, d.exp = d.digits[:n], e+1
	d.trim()
}

// The digits of a word: intlWordPow is 10^intlWordDigits, the greatest
// power of ten in a uint.
const (
	intlWordDigits      = 9 + 10*(bits.UintSize/64)
	intlWordPow    uint = 1e9 * (1 + (1e10-1)*(bits.UintSize/64))
)

// setBigInt sets d to a BigInt's exact value.
func (d *intlDecimal) setBigInt(b *BigInt) {
	d.kind, d.neg, d.exp, d.digits = intlFinite, b.v.Sign() < 0, 0, d.digits[:0]
	w := b.v.Bits()
	if len(w) > 8 {
		t := b.v.Text(10)
		if d.neg {
			t = t[1:]
		}
		d.reserve(len(t))
		d.digits = d.digits[:copy(d.digits[:cap(d.digits)], t)]
		d.exp = len(d.digits)
		d.trim()
		return
	}
	// Up to 8 words, divided by intlWordPow on the stack: its remainders
	// are the digits from the last.
	var q [8]big.Word
	var buf [8 * 20]byte
	n, pos := copy(q[:], w), len(buf)
	for n > 0 {
		var r uint
		for j := n - 1; j >= 0; j-- {
			var x uint
			x, r = bits.Div(r, uint(q[j]), intlWordPow)
			q[j] = big.Word(x)
		}
		for n > 0 && q[n-1] == 0 {
			n--
		}
		for k := 0; k < intlWordDigits && (n > 0 || r > 0); k++ {
			pos--
			buf[pos] = byte('0' + r%10)
			r /= 10
		}
	}
	d.reserve(len(buf) - pos)
	d.digits = d.digits[:copy(d.digits[:cap(d.digits)], buf[pos:])]
	d.exp = len(d.digits)
	d.trim()
}

// reserve makes room for n digits in d.digits, keeping them. The digits
// are written in place: a slice of digits' array stored back into it would
// make a caller's stack array escape.
func (d *intlDecimal) reserve(n int) {
	if cap(d.digits) < n {
		b := make([]byte, len(d.digits), n+8)
		copy(b, d.digits)
		d.digits = b
	}
}

// setString implements ToIntlMathematicalValue for a String (ECMA-402
// §16.5.15): the exact value of a StringNumericLiteral (whitespace,
// "Infinity", 0x/0o/0b, "-0" being negative zero), NaN for anything else.
// As the specification's RoundMVResult step says, a value whose Number
// would be an infinity (at least 2^1024 - 2^970) is that infinity and one
// whose Number would be a zero (at most 2^-1075) is that zero.
func (d *intlDecimal) setString(s *String) {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		t := f.s
		for len(t) > 0 && isJSWhitespace(uint16(t[0])) {
			t = t[1:]
		}
		for len(t) > 0 && isJSWhitespace(uint16(t[len(t)-1])) {
			t = t[:len(t)-1]
		}
		d.setLiteral(t)
		return
	}
	u := f.units()
	for len(u) > 0 && isJSWhitespace(u[0]) {
		u = u[1:]
	}
	for len(u) > 0 && isJSWhitespace(u[len(u)-1]) {
		u = u[:len(u)-1]
	}
	b := make([]byte, len(u))
	for i, c := range u {
		if c >= 0x80 {
			d.setNaN()
			return
		}
		b[i] = byte(c)
	}
	d.setLiteral(string(b))
}

func (d *intlDecimal) setNaN() {
	d.kind, d.neg, d.exp, d.digits = intlNaN, false, 0, d.digits[:0]
}

// setLiteral sets d to the StringIntlMV of s, a StringNumericLiteral
// without its surrounding white space, or NaN.
func (d *intlDecimal) setLiteral(s string) {
	d.kind, d.neg, d.exp, d.digits = intlFinite, false, 0, d.digits[:0]
	switch s {
	case "":
		return
	case "Infinity", "+Infinity":
		d.kind = intlInfinity
		return
	case "-Infinity":
		d.kind, d.neg = intlInfinity, true
		return
	}
	if len(s) > 2 && s[0] == '0' {
		switch s[1] | 0x20 {
		case 'x':
			d.setRadix(s[2:], 16, 4)
			return
		case 'o':
			d.setRadix(s[2:], 8, 3)
			return
		case 'b':
			d.setRadix(s[2:], 2, 1)
			return
		}
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		d.neg = s[0] == '-'
		i++
	}
	// The digits from the first significant one go to b, digits' array;
	// exp counts the integer digits among them, or minus the fraction's
	// zeros before them.
	d.reserve(len(s))
	b := d.digits[:len(s)]
	n, exp, any := 0, 0, false
	for ; i < len(s) && intlIsDigit(s[i]); i++ {
		any = true
		if n > 0 || s[i] != '0' {
			b[n] = s[i]
			n++
			exp++
		}
	}
	if i < len(s) && s[i] == '.' {
		for i++; i < len(s) && intlIsDigit(s[i]); i++ {
			any = true
			if n > 0 || s[i] != '0' {
				b[n] = s[i]
				n++
			} else {
				exp--
			}
		}
	}
	d.digits = d.digits[:n]
	if !any {
		d.setNaN()
		return
	}
	// The exponent saturates far beyond any finite value's: digits come
	// from a String, fewer than 2^31.
	var e int64
	if i < len(s) && s[i]|0x20 == 'e' {
		i++
		eneg := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			eneg = s[i] == '-'
			i++
		}
		if i == len(s) {
			d.setNaN()
			return
		}
		for ; i < len(s) && intlIsDigit(s[i]); i++ {
			if e < 1e15 {
				e = e*10 + int64(s[i]-'0')
			}
		}
		if eneg {
			e = -e
		}
	}
	if i != len(s) {
		d.setNaN()
		return
	}
	d.trim()
	if len(d.digits) == 0 {
		return
	}
	d.exp = int(min(max(int64(exp)+e, -400), 400))
	d.clampRange()
}

func intlIsDigit(c byte) bool { return c-'0' < 10 }

// setRadix sets d to the unsigned integer of the digits s in base 2, 8 or
// 16 (bits per digit), or NaN when s has another character.
func (d *intlDecimal) setRadix(s string, base, bits int) {
	for i := 0; i < len(s); i++ {
		if v := digitValue(s[i]); v < 0 || v >= base {
			d.setNaN()
			return
		}
	}
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	var t []byte
	switch {
	case len(s)*bits <= 64:
		var buf [20]byte
		if u, _ := strconv.ParseUint(s, base, 64); u != 0 {
			t = strconv.AppendUint(buf[:0], u, 10)
		}
	case (len(s)-1)*bits >= 1024:
		// At least base^(len-1) ≥ 2^1024.
		d.kind = intlInfinity
		return
	default:
		var v big.Int
		v.SetString(s, base)
		t = v.Append(nil, 10)
	}
	d.reserve(len(t))
	d.digits = d.digits[:copy(d.digits[:cap(d.digits)], t)]
	d.exp = len(d.digits)
	d.trim()
	d.clampRange()
}

// intlMVBounds returns the digits of 2^1024 - 2^970 (exponent 309), the
// least value whose Number is +∞, and of 2^-1075 (exponent -323), the
// greatest value whose Number is +0 (a tie, which rounds to even).
var intlMVBounds = sync.OnceValues(func() (string, string) {
	var hi, lo, t big.Int
	hi.Lsh(big.NewInt(1), 1024)
	t.Lsh(big.NewInt(1), 970)
	hi.Sub(&hi, &t)
	lo.Exp(big.NewInt(5), big.NewInt(1075), nil)
	return hi.String(), lo.String()
})

// clampRange applies RoundMVResult's limits to a finite d other than zero:
// an infinity from 2^1024 - 2^970 on, a zero up to 2^-1075.
func (d *intlDecimal) clampRange() {
	switch {
	case d.exp > 309:
		d.kind, d.digits, d.exp = intlInfinity, d.digits[:0], 0
	case d.exp < -323:
		d.digits, d.exp = d.digits[:0], 0
	case d.exp == 309:
		if hi, _ := intlMVBounds(); string(d.digits) >= hi {
			d.kind, d.digits, d.exp = intlInfinity, d.digits[:0], 0
		}
	case d.exp == -323:
		if _, lo := intlMVBounds(); string(d.digits) <= lo {
			d.digits, d.exp = d.digits[:0], 0
		}
	}
}

// trim removes the trailing zeros of d's digits; a zero gets exponent 0.
func (d *intlDecimal) trim() {
	n := len(d.digits)
	for n > 0 && d.digits[n-1] == '0' {
		n--
	}
	d.digits = d.digits[:n]
	if n == 0 {
		d.exp = 0
	}
}

// isZero reports whether d is a zero of either sign.
func (d *intlDecimal) isZero() bool { return d.kind == intlFinite && len(d.digits) == 0 }

// magnitude returns floor(log10(|d|)) for a finite d other than zero.
func (d *intlDecimal) magnitude() int { return d.exp - 1 }

// scale multiplies d by 10^n.
func (d *intlDecimal) scale(n int) {
	if d.kind == intlFinite && len(d.digits) > 0 {
		d.exp += n
	}
}

// cmp compares two decimals that are not NaN: -1, 0 or 1 (the zeros are
// equal).
func (d *intlDecimal) cmp(e *intlDecimal) int {
	ds, es := d.sign(), e.sign()
	switch {
	case ds != es:
		if ds < es {
			return -1
		}
		return 1
	case ds == 0:
		return 0
	}
	var c int
	switch di, ei := d.kind == intlInfinity, e.kind == intlInfinity; {
	case di || ei:
		if di == ei {
			return 0
		}
		c = -1
		if di {
			c = 1
		}
	case d.exp != e.exp:
		c = -1
		if d.exp > e.exp {
			c = 1
		}
	default:
		c = bytes.Compare(d.digits, e.digits)
	}
	return c * ds
}

// sign returns -1, 0 or 1 for a decimal that is not NaN.
func (d *intlDecimal) sign() int {
	if d.kind == intlFinite && len(d.digits) == 0 {
		return 0
	}
	if d.neg {
		return -1
	}
	return 1
}

// intlRoundingType is [[RoundingType]].
type intlRoundingType uint8

const (
	intlFractionDigits intlRoundingType = iota
	intlSignificantDigits
	intlMorePrecision
	intlLessPrecision
)

// intlRoundingMode is [[RoundingMode]], in the order of the specification's
// table (ECMA-402 Table 15).
type intlRoundingMode uint8

const (
	intlRoundCeil intlRoundingMode = iota
	intlRoundFloor
	intlRoundExpand
	intlRoundTrunc
	intlRoundHalfCeil
	intlRoundHalfFloor
	intlRoundHalfExpand
	intlRoundHalfTrunc
	intlRoundHalfEven
)

// The unsigned rounding modes (ECMA-402 Table 31).
const (
	intlUnsignedZero uint8 = iota
	intlUnsignedInfinity
	intlUnsignedHalfZero
	intlUnsignedHalfInfinity
	intlUnsignedHalfEven
)

// intlUnsignedRounding is GetUnsignedRoundingMode: the unsigned mode of
// each rounding mode for a positive and a negative sign.
var intlUnsignedRounding = [...][2]uint8{
	intlRoundCeil:       {intlUnsignedInfinity, intlUnsignedZero},
	intlRoundFloor:      {intlUnsignedZero, intlUnsignedInfinity},
	intlRoundExpand:     {intlUnsignedInfinity, intlUnsignedInfinity},
	intlRoundTrunc:      {intlUnsignedZero, intlUnsignedZero},
	intlRoundHalfCeil:   {intlUnsignedHalfInfinity, intlUnsignedHalfZero},
	intlRoundHalfFloor:  {intlUnsignedHalfZero, intlUnsignedHalfInfinity},
	intlRoundHalfExpand: {intlUnsignedHalfInfinity, intlUnsignedHalfInfinity},
	intlRoundHalfTrunc:  {intlUnsignedHalfZero, intlUnsignedHalfZero},
	intlRoundHalfEven:   {intlUnsignedHalfEven, intlUnsignedHalfEven},
}

// intlDigitOptions are the digit options of a NumberFormat or a
// PluralRules (the slots SetNumberFormatDigitOptions sets).
type intlDigitOptions struct {
	minInt           int
	minFrac, maxFrac int
	minSig, maxSig   int
	roundingType     intlRoundingType
	mode             intlRoundingMode
	increment        int  // [[RoundingIncrement]]: 1, 2, 5, 10, 20, 25, 50, 100, ..., 5000
	stripIfInteger   bool // [[TrailingZeroDisplay]] is "stripIfInteger"
}

// formatNumericToString implements FormatNumericToString (ECMA-402
// §16.5.2) for a finite d: it rounds d in place to [[RoundedNumber]]
// (keeping its sign, negative zero included) and appends
// [[FormattedString]], the unsigned ASCII digits with a '.' before any
// fraction digits ("1234.50", "0.0012", "007"), to buf.
func (o *intlDigitOptions) formatNumericToString(d *intlDecimal, buf []byte) []byte {
	neg := 0
	if d.neg {
		neg = 1
	}
	um := intlUnsignedRounding[o.mode][neg]
	typ := o.roundingType
	if typ >= intlMorePrecision {
		// ToRawFixed's [[RoundingMagnitude]] is -maxFrac; ToRawPrecision's
		// is that of its result's last digit.
		fixedIsMorePrecise := -o.maxFrac < d.precisionMagnitude(o.maxSig, um)
		if (typ == intlMorePrecision) == fixedIsMorePrecise {
			typ = intlFractionDigits
		} else {
			typ = intlSignificantDigits
		}
	}
	// fd is the number of fraction digits the result shows.
	var fd int
	if typ == intlSignificantDigits {
		// ToRawPrecision: maxSig digits, trailing zeros cut down to minSig.
		if len(d.digits) == 0 {
			fd = o.minSig - 1
		} else {
			if len(d.digits) > o.maxSig {
				d.round(o.maxSig, 1, um)
			}
			fd = max(len(d.digits), o.minSig) - d.exp
		}
	} else {
		// ToRawFixed: maxFrac digits in multiples of the increment,
		// trailing zeros cut down to minFrac.
		if len(d.digits) > 0 {
			d.round(d.exp+o.maxFrac, max(o.increment, 1), um)
		}
		fd = o.minFrac
		if len(d.digits) > 0 {
			fd = max(fd, len(d.digits)-d.exp)
		}
	}
	if fd < 0 || o.stripIfInteger && len(d.digits) <= d.exp {
		fd = 0
	}
	// The integer digits, at least one, and minInt's zeros before them.
	n, e := len(d.digits), d.exp
	for range o.minInt - max(e, 1) {
		buf = append(buf, '0')
	}
	switch {
	case e <= 0:
		buf = append(buf, '0')
	case n >= e:
		buf = append(buf, d.digits[:e]...)
	default:
		buf = append(buf, d.digits...)
		buf = appendZeros(buf, e-n)
	}
	if fd == 0 {
		return buf
	}
	// The fraction digits: those of digits after the point, zeros before
	// them (a number below 0.1) and after them (minimum digits).
	buf = append(buf, '.')
	z := 0
	if e < 0 {
		z = min(-e, fd)
		buf = appendZeros(buf, z)
	}
	if lo, hi := max(e, 0), min(n, e+fd); lo < hi {
		buf = append(buf, d.digits[lo:hi]...)
		z += hi - lo
	}
	return appendZeros(buf, fd-z)
}

func appendZeros(buf []byte, n int) []byte {
	for range n {
		buf = append(buf, '0')
	}
	return buf
}

// precisionMagnitude returns the [[RoundingMagnitude]] of ToRawPrecision
// with p digits: the exponent of the p-th digit of the rounded value,
// whose leading digit moves up when the rounding carries out (99.9 → 100).
func (d *intlDecimal) precisionMagnitude(p int, um uint8) int {
	if len(d.digits) == 0 {
		return 1 - p
	}
	e := d.exp - 1
	if len(d.digits) > p {
		if _, _, up := intlRoundDecide(d.digits, p, 1, um); up &&
			len(bytes.TrimLeft(d.digits[:p], "9")) == 0 {
			e++
		}
	}
	return e - p + 1
}

// intlRoundWindow is the number of digits rounding changes in place:
// every increment and its double divide 10^intlRoundWindow.
const (
	intlRoundWindow = 5
	intlRoundBase   = 100000
)

// round rounds d, finite and not zero, to its first k digits (none when k
// is zero or negative) in multiples of inc units of the k-th digit (inc
// divides 10^4), choosing between the multiples below and above as
// ApplyUnsignedRoundingMode does with the unsigned rounding mode um. The
// result may be zero.
func (d *intlDecimal) round(k, inc int, um uint8) {
	n := len(d.digits)
	if k >= n && (inc == 1 || k-n >= 4) {
		return // a multiple already
	}
	low, q, up := intlRoundDecide(d.digits, k, inc, um)
	if k >= n && q == 0 {
		return
	}
	v := low - q
	if up {
		v += inc
	}
	if k <= intlRoundWindow {
		// The result is v units of 10^(exp-k), v at most 10^5.
		place := d.exp - k
		var buf [8]byte
		t := strconv.AppendUint(buf[:0], uint64(v), 10)
		if v == 0 {
			t = t[:0]
		}
		d.reserve(len(t))
		d.digits = d.digits[:copy(d.digits[:cap(d.digits)], t)]
		d.exp = place + len(d.digits)
		d.trim()
		return
	}
	// v replaces the last five of the k digits (zeros past n); v is 10^5
	// at most (a multiple of inc), which carries into the digits before.
	d.reserve(k)
	d.digits = d.digits[:k]
	b := d.digits
	for j := n; j < k; j++ {
		b[j] = '0'
	}
	carry := v >= intlRoundBase
	if carry {
		v -= intlRoundBase
	}
	for j := k - 1; j >= k-intlRoundWindow; j-- {
		b[j] = byte('0' + v%10)
		v /= 10
	}
	if carry {
		j := k - intlRoundWindow - 1
		for ; j >= 0 && b[j] == '9'; j-- {
			b[j] = '0'
		}
		if j >= 0 {
			b[j]++
		} else {
			b[0] = '1'
			d.exp++
		}
	}
	d.trim()
}

// intlRoundDecide decides how the digits d₁…dₙ round to their first k:
// low is the value of the last (up to) five of those k digits (dₙ₊₁… being
// zeros), q is low modulo inc, so that the integer of the k digits less q
// is the multiple of inc below the value (r1), and up reports whether the
// rounding goes to the next multiple (r2). digits has no trailing zeros:
// the value is r1 exactly when q is zero and no digit is dropped.
func intlRoundDecide(digits []byte, k, inc int, um uint8) (low, q int, up bool) {
	n := len(digits)
	for j := max(k-intlRoundWindow, 0); j < k; j++ {
		low *= 10
		if j < n {
			low += int(digits[j] - '0')
		}
	}
	q = low % inc
	dropped := k < n
	if !dropped && q == 0 {
		return
	}
	switch um {
	case intlUnsignedZero:
		return
	case intlUnsignedInfinity:
		return low, q, true
	}
	// c compares x - r1 = (q + f) units with r2 - x = (inc - q - f) units,
	// f in [0, 1) being the dropped digits' fraction of a unit: the sign
	// of 2q + 2f - inc.
	var c int
	switch {
	case !dropped:
		c = 2*q - inc
	case 2*q >= inc:
		c = 1
	case 2*q+1 < inc:
		c = -1
	case k < 0:
		c = -1 // f < 0.1
	default: // f against one half
		c = int(digits[k]) - '5'
		if c == 0 && n > k+1 {
			c = 1
		}
	}
	switch {
	case c > 0:
		up = true
	case c < 0:
	case um == intlUnsignedHalfInfinity:
		up = true
	case um == intlUnsignedHalfEven:
		// r1/(r2 - r1) is odd: 2·inc divides 10^5, so the last five
		// digits decide.
		up = (low-q)/inc%2 == 1
	}
	return
}

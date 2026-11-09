package engine

import (
	"math"
	"math/big"
	"strconv"
	"sync/atomic"
)

// isJSWhitespace reports whether c is a WhiteSpace or LineTerminator code
// unit as defined by ECMAScript (used by ToNumber and String.prototype.trim).
func isJSWhitespace(c uint16) bool {
	switch c {
	case 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x20, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return c >= 0x2000 && c <= 0x200A
}

// TrimJSWhitespace returns s without leading/trailing JS whitespace.
func TrimJSWhitespace(s *String) *String {
	n := s.Len()
	start, end := 0, n
	for start < end && isJSWhitespace(s.At(start)) {
		start++
	}
	for end > start && isJSWhitespace(s.At(end-1)) {
		end--
	}
	return s.Substring(start, end)
}

// StringToNumber implements StringToNumber (the StringNumericLiteral grammar).
func StringToNumber(s *String) float64 {
	t := TrimJSWhitespace(s)
	if t.Len() == 0 {
		return 0
	}
	a, ok := t.ASCII()
	if !ok {
		return math.NaN()
	}
	return parseNumericLiteral(a)
}

func parseNumericLiteral(s string) float64 {
	switch s {
	case "Infinity", "+Infinity":
		return posInf
	case "-Infinity":
		return negInf
	}
	if len(s) > 2 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			return parseRadixInteger(s[2:], 16)
		case 'o', 'O':
			return parseRadixInteger(s[2:], 8)
		case 'b', 'B':
			return parseRadixInteger(s[2:], 2)
		}
	}
	if !isStrDecimalLiteral(s) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Only ErrRange is possible after validation: ±Inf or 0 is right.
		return f
	}
	return f
}

// isStrDecimalLiteral validates: [+-]? (digits ('.' digits?)? | '.' digits) ([eE] [+-]? digits)?
func isStrDecimalLiteral(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	intDigits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		intDigits++
	}
	fracDigits := 0
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			fracDigits++
		}
	}
	if intDigits == 0 && fracDigits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		expDigits := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			expDigits++
		}
		if expDigits == 0 {
			return false
		}
	}
	return i == len(s)
}

// parseRadixInteger parses digits in radix 2, 8 or 16 with correct rounding.
func parseRadixInteger(digits string, radix int) float64 {
	if len(digits) == 0 {
		return math.NaN()
	}
	var v uint64
	for i := range len(digits) {
		d := digitValue(digits[i])
		if d < 0 || d >= radix {
			return math.NaN()
		}
		if i < 13 {
			v = v*uint64(radix) + uint64(d)
		}
	}
	if len(digits) <= 13 {
		return float64(v)
	}
	return radixToFloat(digits, radix)
}

// maxFloatDigits bounds the significant digits radixToFloat converts: more,
// in any radix, make a value of at least 2**1024, which overflows.
const maxFloatDigits = 1024

// radixToFloat converts digits, all valid for radix, to the nearest number,
// through arbitrary precision. Past maxFloatDigits significant digits it
// returns Infinity without converting them: math/big's conversion is
// quadratic outside radixes 2, 4 and 16, and one call of parseInt would
// check no interrupt for seconds.
func radixToFloat(digits string, radix int) float64 {
	i := 0
	for i < len(digits)-1 && digits[i] == '0' {
		i++
	}
	digits = digits[i:]
	if len(digits) > maxFloatDigits {
		return posInf
	}
	if radix == 10 {
		f, _ := strconv.ParseFloat(digits, 64) // ErrRange yields ±Inf, which is right
		return f
	}
	b, ok := new(big.Int).SetString(digits, radix)
	if !ok {
		return math.NaN()
	}
	f, _ := new(big.Float).SetInt(b).Float64()
	return f
}

func digitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	}
	return -1
}

// NumberToString implements Number::toString(10).
func NumberToString(f float64) *String {
	if f == math.Trunc(f) && f >= intStringMin && f < intStringMax {
		return intString(int(f)) // -0 too: its string is "0"
	}
	var buf [32]byte
	return asciiStringCopy(AppendNumber(buf[:0], f))
}

// intStrings holds the strings of the integers from intStringMin up to
// intStringMax, each made on its first use and shared by every realm of
// the process: a server-side render writes the same counts, indices and
// sizes, and reads the same negative keys, at every render, each into a
// new string. Each is made with its hash, which Hash would otherwise store
// from every goroutine that hashes it, and marked strConst, so the live
// measurement does not count it, as it skips the constants of shared code
// (codePointStrings does the same).
var intStrings [intStringMax - intStringMin]atomic.Pointer[String]

const (
	intStringMin = -128
	intStringMax = 1024
)

// intString returns the string of i, intStringMin <= i < intStringMax.
func intString(i int) *String {
	if s := intStrings[i-intStringMin].Load(); s != nil {
		return s
	}
	return newIntString(i)
}

// newIntString makes and publishes the string of i; of two goroutines
// that make one at once, both return the one published first.
//
//go:noinline
func newIntString(i int) *String {
	g := strconv.Itoa(i)
	s := &String{s: g, n: int32(len(g)), kind: strASCII, flags: strConst, json: jsonPlain}
	s.hash = s.contentHash()
	if p := &intStrings[i-intStringMin]; !p.CompareAndSwap(nil, s) {
		return p.Load()
	}
	return s
}

// NumberToGoString implements Number::toString(10) into a Go string.
func NumberToGoString(f float64) string {
	switch {
	case f != f:
		return "NaN"
	case f == 0:
		return "0"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	if f == math.Trunc(f) && f > -9007199254740992 && f < 9007199254740992 {
		return strconv.FormatInt(int64(f), 10)
	}
	return string(AppendNumber(make([]byte, 0, 32), f))
}

// AppendNumber appends Number::toString(10) of f to dst (used by builders
// that must not allocate an intermediate string, such as JSON.stringify).
func AppendNumber(dst []byte, f float64) []byte {
	switch {
	case f != f:
		return append(dst, "NaN"...)
	case f == 0:
		return append(dst, '0')
	case math.IsInf(f, 1):
		return append(dst, "Infinity"...)
	case math.IsInf(f, -1):
		return append(dst, "-Infinity"...)
	}
	if f == math.Trunc(f) && f > -9007199254740992 && f < 9007199254740992 {
		return strconv.AppendInt(dst, int64(f), 10)
	}
	var buf [32]byte
	e := strconv.AppendFloat(buf[:0], f, 'e', -1, 64)
	// e looks like "-d.ddddde+XX" or "de-XX".
	i := 0
	neg := false
	if e[0] == '-' {
		neg = true
		i = 1
	}
	var digits [24]byte
	k := 0
	for ; i < len(e) && e[i] != 'e'; i++ {
		if e[i] != '.' {
			digits[k] = e[i]
			k++
		}
	}
	exp, _ := strconv.Atoi(string(e[i+1:]))
	n := exp + 1 // decimal point position relative to digits
	out := dst
	if neg {
		out = append(out, '-')
	}
	switch {
	case k <= n && n <= 21:
		out = append(out, digits[:k]...)
		for range n - k {
			out = append(out, '0')
		}
	case 0 < n && n <= 21:
		out = append(out, digits[:n]...)
		out = append(out, '.')
		out = append(out, digits[n:k]...)
	case -6 < n && n <= 0:
		out = append(out, '0', '.')
		for range -n {
			out = append(out, '0')
		}
		out = append(out, digits[:k]...)
	default:
		out = append(out, digits[0])
		if k > 1 {
			out = append(out, '.')
			out = append(out, digits[1:k]...)
		}
		out = append(out, 'e')
		if n-1 >= 0 {
			out = append(out, '+')
		}
		out = strconv.AppendInt(out, int64(n-1), 10)
	}
	return out
}

const radixDigits = "0123456789abcdefghijklmnopqrstuvwxyz"

// NumberToStringRadix implements Number::toString(x, radix) for radix !=
// 10: the fewest digits that read back as x, in positional notation. Of two equally short candidates it picks the one
// closer to x, as the spec's Note 2 recommends. Integers below 2^53 are
// their exact digits; other values go through shortestRadixDigits.
func NumberToStringRadix(value float64, radix int) string {
	switch {
	case value != value:
		return "NaN"
	case value == 0:
		return "0"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}
	if radix == 10 {
		return NumberToGoString(value)
	}
	neg := value < 0
	if neg {
		value = -value
	}
	if value == math.Trunc(value) && value < 1<<53 {
		s := strconv.FormatInt(int64(value), radix)
		if neg {
			return "-" + s
		}
		return s
	}
	digits, n := shortestRadixDigits(value, radix)
	k := len(digits)
	out := make([]byte, 0, k+max(n-k, 0)+max(-n, 0)+3)
	if neg {
		out = append(out, '-')
	}
	switch {
	case n >= k:
		out = append(out, digits...)
		for range n - k {
			out = append(out, '0')
		}
	case n > 0:
		out = append(out, digits[:n]...)
		out = append(out, '.')
		out = append(out, digits[n:]...)
	default:
		out = append(out, '0', '.')
		for range -n {
			out = append(out, '0')
		}
		out = append(out, digits...)
	}
	return string(out)
}

// shortestRadixDigits returns the digits of s and the exponent n of
// Number::toString step 5 for a finite positive v: v reads back from
// 0.digits × radix^n, and no shorter digit string does. It is the
// free-format algorithm of Burger and Dybvig ("Printing Floating-Point
// Numbers Quickly and Accurately", 1996) on exact integers.
func shortestRadixDigits(v float64, radix int) ([]byte, int) {
	bits := math.Float64bits(v)
	f := bits & (1<<52 - 1)
	be := int(bits>>52) & 0x7FF
	e := be - 1075
	if be == 0 {
		e = -1074
	} else {
		f |= 1 << 52
	}
	// The numbers that round to v lie strictly between (r-mMinus)/s and
	// (r+mPlus)/s, v = r/s; the bounds round to v too when f is even (ties
	// to even). Below a power of two the next double down is half as far.
	even := f&1 == 0
	lowerCloser := f == 1<<52 && be > 1
	var r, s, mPlus, mMinus, t big.Int
	r.SetUint64(f)
	s.SetUint64(1)
	mMinus.SetUint64(1)
	if e >= 0 {
		r.Lsh(&r, uint(e))
		mMinus.Lsh(&mMinus, uint(e))
	} else {
		s.Lsh(&s, uint(-e))
	}
	mPlus.Set(&mMinus)
	if lowerCloser {
		mPlus.Lsh(&mPlus, 1)
		r.Lsh(&r, 2)
		s.Lsh(&s, 2)
	} else {
		r.Lsh(&r, 1)
		s.Lsh(&s, 1)
	}

	// Scale by radix^k for the estimate k = ceil(log_radix v), then correct
	// it: k is the least integer with (r+mPlus)/s below 1 (at most 1 when
	// even).
	b := big.NewInt(int64(radix))
	k := int(math.Ceil(math.Log(v)/math.Log(float64(radix)) - 1e-10))
	t.Exp(b, big.NewInt(int64(max(k, -k))), nil)
	if k >= 0 {
		s.Mul(&s, &t)
	} else {
		r.Mul(&r, &t)
		mPlus.Mul(&mPlus, &t)
		mMinus.Mul(&mMinus, &t)
	}
	high := func() int { return t.Add(&r, &mPlus).Cmp(&s) }
	for c := high(); c > 0 || even && c == 0; c = high() {
		s.Mul(&s, b)
		k++
	}
	for {
		t.Add(&r, &mPlus)
		t.Mul(&t, b)
		if c := t.Cmp(&s); c > 0 || even && c == 0 {
			break
		}
		r.Mul(&r, b)
		mPlus.Mul(&mPlus, b)
		mMinus.Mul(&mMinus, b)
		k--
	}

	var digits []byte
	var q big.Int
	parity := 0 // of the digits so far, read as an integer
	for {
		r.Mul(&r, b)
		mPlus.Mul(&mPlus, b)
		mMinus.Mul(&mMinus, b)
		q.QuoRem(&r, &s, &r)
		d := int(q.Int64())
		if d == 0 && digits == nil {
			// radix^(k-1) lies above v and reads back as v, so one digit
			// suffices; the candidates one place lower may be closer (only
			// subnormals are coarse enough for this). Choose among those,
			// where d+1 = radix is radix^(k-1) itself.
			k--
			digits = []byte{}
			continue
		}
		c := r.Cmp(&mMinus)
		low := c < 0 || even && c == 0
		c = high()
		up := c > 0 || even && c == 0
		if !low && !up {
			digits = append(digits, radixDigits[d])
			parity = (parity*radix + d) & 1
			continue
		}
		if low && up {
			// Both d and d+1 read back as v: the closer, and of two equally
			// close (0.5 in radix 35 is 0.hhh... exactly) the even s.
			c = t.Lsh(&r, 1).Cmp(&s)
			up = c > 0 || c == 0 && (parity*radix+d)&1 != 0
		}
		if up {
			d++
		}
		if d == radix {
			return []byte{'1'}, k + 1
		}
		return append(digits, radixDigits[d]), k
	}
}

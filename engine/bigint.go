package engine

import (
	"math"
	"math/big"
	"math/bits"
	"unsafe"
)

// BigInt is a JavaScript BigInt value: an immutable arbitrary-precision
// integer. The operators live in bigint_ops.go and the BigInt builtins in
// builtin_bigint.go; math/big runs only when one of them does.
type BigInt struct {
	v big.Int
}

// The BigInt results whose words fit one of these buckets are allocated
// with them (newBigIntCap): one allocation instead of the header and
// math/big's slice. On 64-bit platforms each bucket up to 48 words fills a
// size class exactly.
type (
	bigInt2 struct {
		BigInt
		w [2]big.Word
	}
	bigInt4 struct {
		BigInt
		w [4]big.Word
	}
	bigInt6 struct {
		BigInt
		w [6]big.Word
	}
	bigInt8 struct {
		BigInt
		w [8]big.Word
	}
	bigInt12 struct {
		BigInt
		w [12]big.Word
	}
	bigInt16 struct {
		BigInt
		w [16]big.Word
	}
	bigInt24 struct {
		BigInt
		w [24]big.Word
	}
	bigInt32 struct {
		BigInt
		w [32]big.Word
	}
	bigInt48 struct {
		BigInt
		w [48]big.Word
	}
	bigInt64 struct {
		BigInt
		w [64]big.Word
	}
)

// newBigIntCap returns a zero BigInt with room for n words, co-allocated up
// to 64: math/big reuses the room when the result needs at most n words
// (nat.make). A larger n gets a plain BigInt, whose words math/big
// allocates.
func newBigIntCap(n int) *BigInt {
	var (
		z *BigInt
		w []big.Word
	)
	switch {
	case n <= 2:
		x := &bigInt2{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 4:
		x := &bigInt4{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 6:
		x := &bigInt6{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 8:
		x := &bigInt8{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 12:
		x := &bigInt12{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 16:
		x := &bigInt16{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 24:
		x := &bigInt24{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 32:
		x := &bigInt32{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 48:
		x := &bigInt48{}
		z, w = &x.BigInt, x.w[:0]
	case n <= 64:
		x := &bigInt64{}
		z, w = &x.BigInt, x.w[:0]
	default:
		return &BigInt{}
	}
	z.v.SetBits(w)
	return z
}

// smallBigInts are the BigInts -16n to 16n, shared by all realms: half the
// results of decimal arithmetic such as an Intl polyfill's are the digits
// of x % 10n. A BigInt is immutable, so goroutines share one safely, and no
// operator tells two equal BigInts apart. Each is allocated as a result
// that does not fit is (newBigIntInt64), so the memory limit charges the
// two alike.
const smallBigIntMax = 16

var smallBigInts = func() [2*smallBigIntMax + 1]*BigInt {
	var a [2*smallBigIntMax + 1]*BigInt
	for i := range a {
		a[i] = newBigIntInt64(int64(i - smallBigIntMax))
	}
	return a
}()

// bigintFromInt64 returns z as a BigInt (bigintOf).
func (r *Realm) bigintFromInt64(z int64) *BigInt {
	return r.bigintOf(z < 0, 0, magnitude64(z))
}

// bigintOf returns the BigInt of magnitude hi*2^64+lo, negative when neg:
// a shared one from smallBigInts, the one the realm's BigInt memo holds,
// or a new one.
func (r *Realm) bigintOf(neg bool, hi, lo uint64) *BigInt {
	if hi == 0 && lo <= smallBigIntMax {
		if neg {
			return smallBigInts[smallBigIntMax-lo]
		}
		return smallBigInts[smallBigIntMax+lo]
	}
	if lz := r.lazy; lz != nil {
		if m := lz.memos; m == nil {
			if r.newMemos() != nil {
				return r.bigintMemoized(neg, hi, lo)
			}
		} else if m.bigint.skip == 0 {
			return r.bigintMemoized(neg, hi, lo)
		} else {
			m.bigint.skip--
		}
	}
	return newBigIntWords(neg, hi, lo)
}

// bigintMemo maps BigInt values of at most 128 bits to a BigInt made for
// them: the decimal arithmetic of an Intl polyfill computes the same few
// thousand mantissas, quotients and powers of ten at every render (x / 10n,
// x % 10n, x * 10n ** k), each into a new BigInt. A result of at most 128
// bits that is not one of smallBigInts looks in one set of bigintMemoWays,
// chosen by a hash of its value, for one of its value, and returns it. A
// new result goes in the way the count of misses picks. A realm whose
// results seldom repeat, a counter's, skips the memo (memoGate). BigInts
// are immutable and no operator tells two equal ones apart, so a memo
// result serves every operation of its value.
//
// It holds only results the operators made, each of 48 bytes: at most
// bigintMemoSets*bigintMemoWays of them, 48 KiB, besides its own 24 KiB
// (memo.go). On two such workloads 1024 results serve 82 % of the BigInts
// of at most 128 bits their renders made.
type bigintMemo [bigintMemoSets]bigintMemoSet

type bigintMemoSet struct {
	lo, hi [bigintMemoWays]uint64
	b      [bigintMemoWays]*BigInt
}

const (
	bigintMemoBits = 8
	bigintMemoSets = 1 << bigintMemoBits
	bigintMemoWays = 4
)

// bigintMemoized is bigintOf when the realm's BigInt memo is in use.
func (r *Realm) bigintMemoized(neg bool, hi, lo uint64) *BigInt {
	ms := r.lazy.memos
	m := ms.bigints
	if m == nil {
		m = new(bigintMemo)
		ms.bigints = m
		r.charge(int(unsafe.Sizeof(bigintMemo{})))
	}
	h := (lo ^ hi*0x9e3779b97f4a7c15) * 0xbf58476d1ce4e5b9
	if neg {
		h = ^h
	}
	set := &m[h>>(64-bigintMemoBits)]
	for i, b := range &set.b {
		if set.lo[i] == lo && set.hi[i] == hi && b != nil && (b.v.Sign() < 0) == neg {
			ms.bigint.hits++
			return b
		}
	}
	b := newBigIntWords(neg, hi, lo)
	i := ms.bigint.misses % bigintMemoWays
	set.lo[i], set.hi[i], set.b[i] = lo, hi, b
	ms.bigint.miss()
	return b
}

// newBigIntWords returns a new BigInt of magnitude hi*2^64+lo, negative
// when neg, co-allocated with its words.
func newBigIntWords(neg bool, hi, lo uint64) *BigInt {
	z := newBigIntCap(128 / bits.UintSize)
	w := z.v.Bits()[:128/bits.UintSize]
	for i := range w {
		if s := uint(i) * bits.UintSize; s < 64 {
			w[i] = big.Word(lo >> s)
		} else {
			w[i] = big.Word(hi >> (s - 64))
		}
	}
	z.v.SetBits(w) // drops the leading zero words
	if neg {
		z.v.Neg(&z.v)
	}
	return z
}

// words128 returns the magnitude of the words w, at most 128 bits of
// them, as hi*2^64+lo.
func words128(w []big.Word) (hi, lo uint64) {
	for i, x := range w {
		if s := uint(i) * bits.UintSize; s < 64 {
			lo |= uint64(x) << s
		} else {
			hi |= uint64(x) << (s - 64)
		}
	}
	return hi, lo
}

// newBigIntInt64 returns z as a BigInt co-allocated with its words.
func newBigIntInt64(z int64) *BigInt {
	b := newBigIntCap(64 / bits.UintSize)
	b.v.SetInt64(z)
	return b
}

// smallBigInt returns 0n or 1n.
func smallBigInt(one bool) *BigInt {
	if one {
		return smallBigInts[smallBigIntMax+1]
	}
	return smallBigInts[smallBigIntMax]
}

// maxBigIntBits bounds the magnitude of every BigInt (SpiderMonkey's limit,
// 2^20 bits): an operation whose result would be larger throws a RangeError,
// so no single math/big call runs long enough to need an interrupt check.
// The lexer rejects a larger literal (syntax/lexer.go keeps the same bound).
const maxBigIntBits = 1 << 20

// Converting a BigInt to a string or back takes microseconds past these
// sizes and tens of milliseconds at the size limit (decimal parsing is
// quadratic), while a native loop such as forEach checks the interrupt only
// every interruptStride calls of its callback: conversions of a BigInt of
// more than bigintSlowWords words, or of a string of more than
// bigintSlowChars characters, check it first.
const (
	bigintSlowWords = 4096 / bits.UintSize
	bigintSlowChars = 1024
)

// bigintDisplayBits bounds the BigInts Value.String shows, and so the error
// messages the engine builds, which check no interrupt: a larger one reads
// "<a very large BigInt>", as V8's messages show a BigInt of more than 100
// 64-bit digits (NoSideEffectsToString). A native loop such as
// arr.map(Promise.all, Promise) builds one message per callback.
const bigintDisplayBits = 100 * 64

// checkBigIntFormat checks the interrupt before b is converted to a string
// when b is large.
func (r *Realm) checkBigIntFormat(b *BigInt) error {
	if len(b.v.Bits()) <= bigintSlowWords {
		return nil
	}
	return r.CheckInterrupt()
}

// checkBigIntParse checks the interrupt before s is parsed as a BigInt when
// s is long.
func (r *Realm) checkBigIntParse(s *String) error {
	if s.Len() <= bigintSlowChars {
		return nil
	}
	return r.CheckInterrupt()
}

// NewBigIntFromDecimal parses decimal text such as the digits of a `123n`
// literal (no sign, no suffix). ok is false on malformed input.
func NewBigIntFromDecimal(text string) (*BigInt, bool) {
	b := &BigInt{}
	if _, ok := b.v.SetString(text, 10); !ok {
		return nil, false
	}
	return b, true
}

// newBigIntConst parses the text of a BigInt constant: decimal digits, or
// hex digits after "0x", which a large 0x, 0o or 0b literal keeps so that it
// converts in linear time.
func newBigIntConst(text string) (*BigInt, bool) {
	if len(text) > 2 && text[:2] == "0x" {
		b := &BigInt{}
		if _, ok := b.v.SetString(text[2:], 16); !ok {
			return nil, false
		}
		return b, true
	}
	return NewBigIntFromDecimal(text)
}

// NewBigIntFromInt64 wraps an int64.
func NewBigIntFromInt64(i int64) *BigInt {
	b := &BigInt{}
	b.v.SetInt64(i)
	return b
}

// NewBigIntFromUint64 wraps a uint64.
func NewBigIntFromUint64(u uint64) *BigInt {
	b := &BigInt{}
	b.v.SetUint64(u)
	return b
}

// NewBigIntFromBig copies x (for host import). ok is false when x exceeds
// the BigInt size limit (2^20 bits).
func NewBigIntFromBig(x *big.Int) (*BigInt, bool) {
	if x.BitLen() > maxBigIntBits {
		return nil, false
	}
	b := &BigInt{}
	b.v.Set(x)
	return b, true
}

// ToString returns the decimal representation without the `n` suffix.
func (b *BigInt) ToString() string { return b.v.String() }

// displayString is b as Value.String shows it: the decimal digits and the
// `n` suffix, or "<a very large BigInt>" past bigintDisplayBits.
func (b *BigInt) displayString() string {
	if b.v.BitLen() > bigintDisplayBits {
		return "<a very large BigInt>"
	}
	return b.v.String() + "n"
}

// StrictEquals reports mathematical equality.
func (b *BigInt) StrictEquals(o *BigInt) bool { return b == o || b.v.Cmp(&o.v) == 0 }

// IsZero reports whether the value is 0n (used by ToBoolean).
func (b *BigInt) IsZero() bool { return b.v.Sign() == 0 }

// Big returns a copy of the underlying math/big value (for host export).
func (b *BigInt) Big() *big.Int { return new(big.Int).Set(&b.v) }

// Uint64 returns b modulo 2^64, the value ToBigUint64 stores.
func (b *BigInt) Uint64() uint64 {
	var u uint64
	w := b.v.Bits() // the magnitude, least significant word first
	if bits.UintSize == 64 {
		if len(w) > 0 {
			u = uint64(w[0])
		}
	} else {
		for i := range min(len(w), 2) {
			u |= uint64(w[i]) << (32 * i)
		}
	}
	if b.v.Sign() < 0 {
		u = -u
	}
	return u
}

// Int64 returns b modulo 2^64 as a two's complement int64, the value
// ToBigInt64 stores.
func (b *BigInt) Int64() int64 { return int64(b.Uint64()) }

// Float64 returns the number nearest b (Number(bigint)).
func (b *BigInt) Float64() float64 {
	f, _ := b.v.Float64()
	return f
}

// ToBigInt implements ToBigInt: a bigint is returned as is, booleans and
// strings convert, and undefined, null, numbers and symbols throw a
// TypeError. A string that is not an integer literal is a SyntaxError.
func (r *Realm) ToBigInt(v Value) (*BigInt, error) {
	p, err := r.ToPrimitive(v, HintNumber)
	if err != nil {
		return nil, err
	}
	switch p.Type() {
	case TypeBigInt:
		return p.AsBigInt(), nil
	case TypeBoolean:
		return smallBigInt(p.AsBool()), nil
	case TypeString:
		if s := p.AsString(); s.Len() <= shortDecimalMax {
			if a, ok := s.ASCII(); ok {
				if v, ok := shortDecimal(a); ok {
					return r.bigintFromInt64(v), nil
				}
			}
		}
		if err := r.checkBigIntParse(p.AsString()); err != nil {
			return nil, err
		}
		b, over, ok := parseBigInt(p.AsString())
		if !ok {
			return nil, r.SyntaxError("Cannot convert %s to a BigInt", p.String())
		}
		if over != 0 {
			return nil, errBigIntTooBig(r)
		}
		return b, nil
	}
	return nil, r.TypeError("Cannot convert %s to a BigInt", p.String())
}

// ToBigInt64 implements ToBigInt64: ToBigInt modulo 2^64, as a signed value.
func (r *Realm) ToBigInt64(v Value) (int64, error) {
	b, err := r.ToBigInt(v)
	if err != nil {
		return 0, err
	}
	return b.Int64(), nil
}

// ToBigUint64 implements ToBigUint64: ToBigInt modulo 2^64.
func (r *Realm) ToBigUint64(v Value) (uint64, error) {
	b, err := r.ToBigInt(v)
	if err != nil {
		return 0, err
	}
	return b.Uint64(), nil
}

// numberToBigInt implements NumberToBigInt: a non-integral number is a
// RangeError.
func (r *Realm) numberToBigInt(f float64) (*BigInt, error) {
	if f != math.Trunc(f) || math.IsInf(f, 0) {
		return nil, r.RangeError("The number %s cannot be converted to a BigInt because it is not an integer", NumberToGoString(f))
	}
	if math.Abs(f) < 1<<63 {
		return r.bigintFromInt64(int64(f)), nil
	}
	b := &BigInt{}
	big.NewFloat(f).Int(&b.v)
	return b, nil
}

func errBigIntTooBig(r *Realm) error { return r.RangeError("Maximum BigInt size exceeded") }

// StringToBigInt implements StringToBigInt (decimal, hex, octal and binary
// literals; empty string is 0n). ok is false when the string is not a valid
// StringIntegerLiteral or its value exceeds the BigInt size limit.
func StringToBigInt(s *String) (*BigInt, bool) {
	b, over, ok := parseBigInt(s)
	return b, ok && over == 0
}

// shortDecimalMax is the length of the longest string shortDecimal reads,
// a sign and 18 digits. ToBigInt tries it on no longer string, so that a
// longer rope is flattened once, by parseBigInt.
const shortDecimalMax = 1 + 18

// shortDecimal returns the value of a when it is an optional sign and 1
// to 18 decimal digits, the StringIntegerLiterals that always fit an
// int64; ok is false for any other string, which parseBigInt reads.
func shortDecimal(a string) (v int64, ok bool) {
	neg := false
	if a != "" && (a[0] == '-' || a[0] == '+') {
		neg, a = a[0] == '-', a[1:]
	}
	if a == "" || len(a) > shortDecimalMax-1 {
		return 0, false
	}
	for i := range len(a) {
		d := a[i] - '0'
		if d > 9 {
			return 0, false
		}
		v = v*10 + int64(d)
	}
	if neg {
		v = -v
	}
	return v, true
}

// parseBigInt is StringToBigInt with the size limit apart: a valid literal
// whose value has more than 2^20 bits gives b == nil and over, its sign. Such
// a literal is larger in magnitude than every BigInt, which is all a
// comparison needs, and parsing it would take time proportional to the
// square of its length.
func parseBigInt(s *String) (b *BigInt, over int, ok bool) {
	t := TrimJSWhitespace(s)
	a, ok := t.ASCII()
	if !ok {
		return nil, 0, false
	}
	b = &BigInt{}
	if a == "" {
		return b, 0, true
	}
	base, digits, sign := 10, a, 1
	if len(a) > 2 && a[0] == '0' {
		switch a[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
	}
	if base != 10 {
		digits = a[2:]
	} else if a[0] == '+' || a[0] == '-' {
		if a[0] == '-' {
			sign = -1
		}
		digits = a[1:]
	}
	if digits == "" {
		return nil, 0, false
	}
	for i := range len(digits) {
		if d := digitValue(digits[i]); d < 0 || d >= base {
			return nil, 0, false
		}
	}
	// A literal of n significant digits has more than (n-1)*log2(base) bits.
	n := len(digits)
	for n > 1 && digits[len(digits)-n] == '0' {
		n--
	}
	if float64(n-1)*math.Log2(float64(base)) >= maxBigIntBits {
		return nil, sign, true
	}
	if _, ok := b.v.SetString(digits, base); !ok {
		return nil, 0, false
	}
	if b.v.BitLen() > maxBigIntBits {
		return nil, sign, true
	}
	if sign < 0 {
		b.v.Neg(&b.v)
	}
	return b, 0, true
}

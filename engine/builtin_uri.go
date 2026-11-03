package engine

import "unicode/utf8"

// URI functions: encodeURIComponent, decodeURIComponent,
// encodeURI and decodeURI with the spec's reserved sets. Malformed input
// (lone surrogates, bad percent sequences, invalid UTF-8) throws URIError.

// uriSet is a 128-bit membership table for ASCII characters.
type uriSet [2]uint64

func makeURISet(chars string) uriSet {
	var s uriSet
	for i := range len(chars) {
		c := chars[i]
		s[c>>6] |= 1 << (c & 63)
	}
	return s
}

func (s *uriSet) has(c byte) bool { return c < 0x80 && s[c>>6]&(1<<(c&63)) != 0 }

const (
	uriAlnum      = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	uriMark       = "-_.!~*'()"
	uriReservedCh = ";/?:@&=+$,#"
)

var (
	uriComponentUnescaped = makeURISet(uriAlnum + uriMark)
	uriUnescaped          = makeURISet(uriAlnum + uriMark + uriReservedCh)
	uriReserved           = makeURISet(uriReservedCh)
	uriNone               uriSet
)

// uriFunctions also holds the other string-coding globals: the Annex B
// escape and unescape and the HTML standard's atob and btoa.
var uriFunctions = []builtinDef{
	{AtomEncodeURIComponent, encodeURIComponent, 1},
	{AtomDecodeURIComponent, decodeURIComponent, 1},
	{AtomEncodeURI, encodeURI, 1},
	{AtomDecodeURI, decodeURI, 1},
	{AtomAtob, globalAtob, 1},
	{AtomBtoa, globalBtoa, 1},
	{AtomEscape, globalEscape, 1},
	{AtomUnescape, globalUnescape, 1},
}

func installURI(r *Realm) {
	// Read-only in the shared template like every intrinsic binding
	// (lockdown model, see initGlobal).
	attrs := attrHidden
	if r.buildingShared {
		attrs = attrFrozen
	}
	r.installBuiltinsAttrs(r.Global, uriFunctions, attrs)
}

// URIError returns a thrown URIError.
func (r *Realm) URIError(format string, args ...any) error {
	return &Exception{Value: ObjectValue(r.NewError(KindURIError, format, args...))}
}

func encodeURIComponent(r *Realm, this Value, args []Value) (Value, error) {
	return uriEncode(r, Arg(args, 0), &uriComponentUnescaped)
}

func encodeURI(r *Realm, this Value, args []Value) (Value, error) {
	return uriEncode(r, Arg(args, 0), &uriUnescaped)
}

func decodeURIComponent(r *Realm, this Value, args []Value) (Value, error) {
	return uriDecode(r, Arg(args, 0), &uriNone)
}

func decodeURI(r *Realm, this Value, args []Value) (Value, error) {
	return uriDecode(r, Arg(args, 0), &uriReserved)
}

const upperHex = "0123456789ABCDEF"

// uriEncode implements Encode(string, unescapedSet).
func uriEncode(r *Realm, v Value, unescaped *uriSet) (Value, error) {
	s, err := r.ToString(v)
	if err != nil {
		return Undefined(), err
	}
	f := s // s is returned, f never: a view stays on the stack
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		str := f.s
		extra := 0
		for i := range len(str) {
			if err := interruptEvery(r, int64(i)); err != nil {
				return Undefined(), err
			}
			if !unescaped.has(str[i]) {
				extra += 2
			}
		}
		if extra == 0 {
			return StringValue(s), nil
		}
		if len(str)+extra > maxStringLength || r.overBudget(len(str)+extra) {
			return Undefined(), r.invalidStringLength()
		}
		r.chargeString(len(str) + extra)
		b := make([]byte, 0, len(str)+extra)
		for i := range len(str) {
			if err := interruptEvery(r, int64(i)); err != nil {
				return Undefined(), err
			}
			c := str[i]
			if unescaped.has(c) {
				b = append(b, c)
				continue
			}
			b = append(b, '%', upperHex[c>>4], upperHex[c&15])
		}
		return StringValue(asciiString(bytesToString(b))), nil
	}
	u := f.units()
	b := make([]byte, 0, len(u)*3)
	var tmp [4]byte
	for i := 0; i < len(u); i++ {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		c := u[i]
		if c < 0x80 {
			if unescaped.has(byte(c)) {
				b = append(b, byte(c))
				continue
			}
			b = append(b, '%', upperHex[c>>4], upperHex[c&15])
			continue
		}
		cp := rune(c)
		if c >= 0xD800 && c < 0xE000 {
			if c >= 0xDC00 || i+1 >= len(u) || u[i+1] < 0xDC00 || u[i+1] >= 0xE000 {
				return Undefined(), r.URIError("URI malformed")
			}
			cp = (rune(c)-0xD800)<<10 + (rune(u[i+1]) - 0xDC00) + 0x10000
			i++
		}
		n := utf8.EncodeRune(tmp[:], cp)
		for _, octet := range tmp[:n] {
			b = append(b, '%', upperHex[octet>>4], upperHex[octet&15])
		}
	}
	if len(b) > maxStringLength || r.overBudget(cap(b)) {
		return Undefined(), r.invalidStringLength()
	}
	r.chargeString(cap(b))
	return StringValue(asciiString(bytesToString(b))), nil
}

// uriDecode implements Decode(string, reservedSet).
func uriDecode(r *Realm, v Value, reserved *uriSet) (Value, error) {
	s, err := r.ToString(v)
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	first := -1
	for i := range n {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		if s.At(i) == '%' {
			first = i
			break
		}
	}
	if first < 0 {
		return StringValue(s), nil
	}
	var sb StringBuilder
	sb.Grow(n)
	sb.WriteString(s.Substring(0, first))
	for k := first; k < n; k++ {
		if err := interruptEvery(r, int64(k)); err != nil {
			return Undefined(), err
		}
		c := s.At(k)
		if c != '%' {
			sb.WriteUnit(c)
			continue
		}
		start := k
		b, ok := hexOctetAt(s, k)
		if !ok {
			return Undefined(), r.URIError("URI malformed")
		}
		k += 2
		if b < 0x80 {
			if reserved.has(b) {
				sb.WriteString(s.Substring(start, k+1))
			} else {
				sb.WriteASCII(b)
			}
			continue
		}
		// Multi-byte UTF-8 sequence: the leading byte gives the length.
		var need int
		switch {
		case b&0xE0 == 0xC0:
			need = 1
		case b&0xF0 == 0xE0:
			need = 2
		case b&0xF8 == 0xF0:
			need = 3
		default:
			return Undefined(), r.URIError("URI malformed")
		}
		if k+3*need >= n {
			return Undefined(), r.URIError("URI malformed")
		}
		var octets [4]byte
		octets[0] = b
		for j := 1; j <= need; j++ {
			k++
			if s.At(k) != '%' {
				return Undefined(), r.URIError("URI malformed")
			}
			o, ok := hexOctetAt(s, k)
			if !ok || o&0xC0 != 0x80 {
				return Undefined(), r.URIError("URI malformed")
			}
			octets[j] = o
			k += 2
		}
		// An invalid sequence (overlong, surrogate, above U+10FFFF) decodes
		// as (RuneError, 1); the encoding of U+FFFD itself decodes with its
		// full size and is a valid code point.
		cp, size := utf8.DecodeRune(octets[:need+1])
		if size != need+1 {
			return Undefined(), r.URIError("URI malformed")
		}
		sb.WriteRune(cp)
	}
	return StringValue(r.builtString(&sb)), nil
}

// hexOctetAt parses the two hex digits after the '%' at s[k].
func hexOctetAt(s *String, k int) (byte, bool) {
	if k+2 >= s.Len() {
		return 0, false
	}
	hi, lo := digitValueUnit(s.At(k+1)), digitValueUnit(s.At(k+2))
	if hi < 0 || hi >= 16 || lo < 0 || lo >= 16 {
		return 0, false
	}
	return byte(hi<<4 | lo), true
}

// --- escape, unescape (Annex B.2.1) ----------------------------------------------

// escapeUnescaped is the set of characters escape leaves as they are.
var escapeUnescaped = makeURISet(uriAlnum + "@*_+-./")

// globalEscape implements escape(string) (B.2.1.1): other code units become
// %XX below U+0100 and %uXXXX above.
func globalEscape(r *Realm, this Value, args []Value) (Value, error) {
	s, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	size := 0
	for i := range n {
		switch c := s.At(i); {
		case escapeUnescaped.has(byte(c)) && c < 0x80:
			size++
		case c < 0x100:
			size += 3
		default:
			size += 6
		}
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
	}
	if size == n {
		return StringValue(s), nil
	}
	if size > maxStringLength || r.overBudget(size) {
		return Undefined(), r.invalidStringLength()
	}
	r.chargeString(size)
	b := make([]byte, 0, size)
	for i := range n {
		switch c := s.At(i); {
		case escapeUnescaped.has(byte(c)) && c < 0x80:
			b = append(b, byte(c))
		case c < 0x100:
			b = append(b, '%', upperHex[c>>4], upperHex[c&15])
		default:
			b = append(b, '%', 'u', upperHex[c>>12], upperHex[c>>8&15], upperHex[c>>4&15], upperHex[c&15])
		}
	}
	return StringValue(asciiString(bytesToString(b))), nil
}

// globalUnescape implements unescape(string) (B.2.1.2): %uXXXX and %XX
// become the code unit they spell; any other % is kept.
func globalUnescape(r *Realm, this Value, args []Value) (Value, error) {
	s, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	first := -1
	for i := range n {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		if s.At(i) == '%' {
			first = i
			break
		}
	}
	if first < 0 {
		return StringValue(s), nil
	}
	var sb StringBuilder
	sb.Grow(n)
	sb.WriteString(s.Substring(0, first))
	for k := first; k < n; k++ {
		if err := interruptEvery(r, int64(k)); err != nil {
			return Undefined(), err
		}
		c := s.At(k)
		if c == '%' {
			if k+6 <= n && s.At(k+1) == 'u' {
				if v, ok := hexUnitsAt(s, k+2, 4); ok {
					c = v
					k += 5
				}
			} else if v, ok := hexUnitsAt(s, k+1, 2); ok {
				c = v
				k += 2
			}
		}
		sb.WriteUnit(c)
	}
	return StringValue(r.builtString(&sb)), nil
}

// hexUnitsAt parses the n hex digits at s[k:], which may run past the end.
func hexUnitsAt(s *String, k, n int) (uint16, bool) {
	if k+n > s.Len() {
		return 0, false
	}
	var v uint16
	for i := k; i < k+n; i++ {
		d := digitValueUnit(s.At(i))
		if d < 0 || d >= 16 {
			return 0, false
		}
		v = v<<4 | uint16(d)
	}
	return v, true
}

// --- atob, btoa (HTML §8.3) ------------------------------------------------------

const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// base64Values maps an ASCII character to its sextet plus one, 0 when it is
// not in base64Alphabet.
var base64Values = func() (t [128]uint8) {
	for i := range len(base64Alphabet) {
		t[base64Alphabet[i]] = uint8(i + 1)
	}
	return t
}()

// invalidCharacterError is what atob and btoa throw where the HTML standard
// throws an "InvalidCharacterError" DOMException. There is no DOMException
// type, so it is an Error with the DOMException's name and code as own
// properties: e.name, e.code, e.message and e instanceof Error read as they
// do in browsers and Node.
func (r *Realm) invalidCharacterError(message string) error {
	e := r.NewError(KindError, "%s", message)
	e.DefineOwnDataFast(r, StringKey(AtomName), StringValue(AtomInvalidCharacterError), attrHidden)
	e.DefineOwnDataFast(r, StringKey(AtomCode), IntValue(5), attrHidden)
	return &Exception{Value: ObjectValue(e)}
}

// globalAtob implements atob(data) with forgiving-base64 decode: ASCII
// whitespace is skipped, the padding is optional, and each decoded byte
// becomes one code unit.
func globalAtob(r *Realm, this Value, args []Value) (Value, error) {
	if len(args) == 0 {
		return Undefined(), r.TypeError("atob requires 1 argument")
	}
	s, err := r.ToString(args[0])
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	data := make([]byte, 0, n)
	for i := range n {
		switch c := s.At(i); {
		case c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' ':
		case c < 0x80 && (base64Values[c] != 0 || c == '='):
			data = append(data, byte(c))
		default:
			return Undefined(), r.invalidCharacterError("The string to be decoded is not correctly encoded.")
		}
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
	}
	if len(data)%4 == 0 && len(data) > 0 && data[len(data)-1] == '=' {
		data = data[:len(data)-1]
		if data[len(data)-1] == '=' {
			data = data[:len(data)-1]
		}
	}
	if len(data)%4 == 1 {
		return Undefined(), r.invalidCharacterError("The string to be decoded is not correctly encoded.")
	}
	out := make([]uint16, 0, len(data)*3/4)
	ascii := true
	var acc uint32
	for i, c := range data {
		v := base64Values[c]
		if v == 0 { // a '=' before the end
			return Undefined(), r.invalidCharacterError("The string to be decoded is not correctly encoded.")
		}
		acc = acc<<6 | uint32(v-1)
		if i%4 == 3 {
			out = append(out, uint16(acc>>16), uint16(acc>>8&0xFF), uint16(acc&0xFF))
			ascii = ascii && acc&0x808080 == 0
			acc = 0
		}
	}
	// Two or three trailing sextets hold one or two bytes; the rest of their
	// bits are discarded.
	switch len(data) % 4 {
	case 2:
		out = append(out, uint16(acc>>4))
		ascii = ascii && acc>>4 < 0x80
	case 3:
		out = append(out, uint16(acc>>10), uint16(acc>>2&0xFF))
		ascii = ascii && acc>>2&0x8080 == 0
	}
	if ascii {
		b := make([]byte, len(out))
		for i, u := range out {
			b[i] = byte(u)
		}
		return StringValue(asciiString(bytesToString(b))), nil
	}
	return StringValue(FromUTF16(out)), nil
}

// globalBtoa implements btoa(data): the code units, which must all be below
// U+0100, are taken as bytes and base64 encoded with padding.
func globalBtoa(r *Realm, this Value, args []Value) (Value, error) {
	if len(args) == 0 {
		return Undefined(), r.TypeError("btoa requires 1 argument")
	}
	s, err := r.ToString(args[0])
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	size := (n + 2) / 3 * 4
	if size > maxStringLength || r.overBudget(size) {
		return Undefined(), r.invalidStringLength()
	}
	r.chargeString(size)
	out := make([]byte, 0, size)
	for i := 0; i < n; i += 3 {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		var acc uint32
		k := min(3, n-i)
		for j := range k {
			c := s.At(i + j)
			if c > 0xFF {
				return Undefined(), r.invalidCharacterError("Invalid character")
			}
			acc |= uint32(c) << (16 - 8*j)
		}
		out = append(out, base64Alphabet[acc>>18], base64Alphabet[acc>>12&63], '=', '=')
		if k > 1 {
			out[len(out)-2] = base64Alphabet[acc>>6&63]
		}
		if k > 2 {
			out[len(out)-1] = base64Alphabet[acc&63]
		}
	}
	return StringValue(asciiString(bytesToString(out))), nil
}

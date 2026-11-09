package engine

// Uint8Array to and from base64 and hex: Uint8Array.fromBase64 and fromHex,
// and Uint8Array.prototype.toBase64, toHex, setFromBase64 and setFromHex.
// The decoders read the string's code units in place, its bytes when it is
// ASCII: a unit that is not ASCII is in no alphabet and is not whitespace,
// so they stop at it. They run no user code, so setFromBase64 and
// setFromHex decode straight into the target's bytes, which is what writing
// the decoded bytes afterwards would leave there.

import (
	"encoding/hex"

	"github.com/MoeForge/moego/base64"
)

// base64Handling is the lastChunkHandling option of the base64 decoder.
type base64Handling uint8

const (
	base64Loose base64Handling = iota
	base64Strict
	base64StopBeforePartial
)

// The values of the alphabet and lastChunkHandling options, in the order
// of their meanings (url false then true, then base64Handling).
var (
	base64AlphabetNames = []string{"base64", "base64url"}
	base64HandlingNames = []string{"loose", "strict", "stop-before-partial"}
)

// base64URLValues is base64Values for the base64url alphabet.
var base64URLValues = func() (t [128]uint8) {
	t = base64Values
	t['+'], t['/'] = 0, 0
	t['-'], t['_'] = 63, 64
	return t
}()

// hexValues maps an ASCII character to its hex digit plus one, 0 when it is
// not one.
var hexValues = func() (t [128]uint8) {
	for i, c := range "0123456789abcdef" {
		t[c] = uint8(i + 1)
	}
	for i, c := range "ABCDEF" {
		t[c] = uint8(i + 11)
	}
	return t
}()

// uint8ArrayStatics and uint8ArrayMethods are the members Uint8Array and
// Uint8Array.prototype have beyond BYTES_PER_ELEMENT.
func uint8ArrayStatics(a *binaryAtoms) []builtinDef {
	return []builtinDef{{a.fromBase64, uint8ArrayFromBase64, 1}, {a.fromHex, uint8ArrayFromHex, 1}}
}

func uint8ArrayMethods(a *binaryAtoms) []builtinDef {
	return []builtinDef{
		{a.toBase64, uint8ArrayToBase64, 0},
		{a.setFromBase64, uint8ArraySetFromBase64, 1},
		{a.toHex, uint8ArrayToHex, 0},
		{a.setFromHex, uint8ArraySetFromHex, 1},
	}
}

// thisUint8Array implements ValidateUint8Array(this).
func thisUint8Array(r *Realm, this Value, method string) (*typedArray, error) {
	ta, err := thisTypedArray(r, this, method)
	if err == nil && ta.kind != elemUint8 {
		err = r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(this))
	}
	return ta, err
}

// uint8ArrayBytes implements GetUint8ArrayBytes(ta), without the copy: the
// bytes ta views, a TypeError when it is out of bounds.
func uint8ArrayBytes(r *Realm, ta *typedArray, method string) ([]byte, error) {
	if ta.length() < 0 {
		return nil, outOfBoundsTypedArray(r, method)
	}
	return ta.viewed(), nil
}

// stringArg returns the argument i, which must be a String (no conversion).
func stringArg(r *Realm, args []Value, i int, method string) (*String, error) {
	if v := Arg(args, i); v.IsString() {
		return v.AsString(), nil
	}
	return nil, r.TypeError("%s: the argument is not a string", method)
}

// optionsObject implements GetOptionsObject(v): nil, which reads as an
// empty object, for undefined.
func (r *Realm) optionsObject(v Value, method string) (*Object, error) {
	switch {
	case v.IsUndefined():
		return nil, nil
	case v.IsObject():
		return v.AsObject(), nil
	}
	return nil, r.TypeError("%s: the options are not an object", method)
}

// stringOption reads the option key of opts, which must be undefined (the
// first of names) or one of the strings names, compared without conversion,
// and returns its index in names.
func (r *Realm) stringOption(opts *Object, key *String, names []string, method string) (int, error) {
	if opts == nil {
		return 0, nil
	}
	v, err := opts.GetProp(r, StringKey(key))
	if err != nil || v.IsUndefined() {
		return 0, err
	}
	if v.IsString() {
		for i, name := range names {
			if v.AsString().EqualsGoString(name) {
				return i, nil
			}
		}
	}
	return 0, r.TypeError("%s: invalid %s option %s", method, key.GoString(), r.DisplayString(v))
}

// base64DecodeOptions reads the options of fromBase64 and setFromBase64:
// the alphabet, then lastChunkHandling.
func (r *Realm) base64DecodeOptions(options Value, method string) (url bool, h base64Handling, err error) {
	opts, err := r.optionsObject(options, method)
	if err != nil {
		return false, 0, err
	}
	a := binaryNames()
	alphabet, err := r.stringOption(opts, a.alphabet, base64AlphabetNames, method)
	if err != nil {
		return false, 0, err
	}
	handling, err := r.stringOption(opts, a.lastChunkHandling, base64HandlingNames, method)
	return alphabet == 1, base64Handling(handling), err
}

// readWritten returns the {read, written} object of encodeInto,
// setFromBase64 and setFromHex.
func (r *Realm) readWritten(read, written int) Value {
	a := binaryNames()
	o := r.NewObject()
	o.DefineOwnDataFast(r, StringKey(a.read), IntValue(read), attrDefault)
	o.DefineOwnDataFast(r, StringKey(a.written), IntValue(written), attrDefault)
	return ObjectValue(o)
}

// newUint8Array returns a Uint8Array over data, a data block from newBytes
// with no spare capacity worth keeping.
func (r *Realm) newUint8Array(data []byte) (Value, error) {
	b := r.binaryIntr()
	if err := r.checkTypedArrayLength(elemUint8, int64(len(data))); err != nil {
		return Undefined(), err
	}
	buf := r.newBufferObject(b.ArrayBufferPrototype, ClassArrayBuffer, data, -1)
	return ObjectValue(r.newTypedArrayObject(b.typedArrayPrototypes[elemUint8], elemUint8, buf, 0, len(data))), nil
}

// trimBytes returns data[:n] as a data block of its own when the rest of
// data is worth freeing.
func (r *Realm) trimBytes(data []byte, n int) ([]byte, error) {
	if cap(data)-n <= n/4+64 {
		return data[:n:n], nil
	}
	out := r.newBytes(n, n)
	return out, r.copyBytes(out, data[:n])
}

// --- decoding ---------------------------------------------------------------------

// codeUnit is the type of a string's code units as the decoders read them:
// the bytes of an ASCII string, else its UTF-16 units.
type codeUnit interface{ byte | uint16 }

// isBase64Space reports whether c is ASCII whitespace (SkipAsciiWhitespace).
func isBase64Space[C codeUnit](c C) bool {
	return c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

// skipBase64Space implements SkipAsciiWhitespace(s, i), honouring the
// interrupt flag on long runs.
func skipBase64Space[C codeUnit](r *Realm, s []C, i int) (int, error) {
	for {
		stop := min(len(s), i+copyBytesChunk)
		for i < stop && isBase64Space(s[i]) {
			i++
		}
		if i < stop || i == len(s) {
			return i, nil
		}
		if err := r.CheckInterrupt(); err != nil {
			return 0, err
		}
	}
}

// fromBase64 implements FromBase64(s, alphabet, h, len(dst)), decoding into
// dst: it returns the code units read, the bytes written and false where
// the spec's result has a SyntaxError. It honours the interrupt flag on
// long inputs.
func fromBase64[C codeUnit](r *Realm, dst []byte, s []C, url bool, h base64Handling) (read, written int, ok bool, err error) {
	if len(dst) == 0 {
		return 0, 0, true, nil
	}
	values := &base64Values
	if url {
		values = &base64URLValues
	}
	var acc uint32 // the sextets of the chunk
	n := 0         // their count, chunkLength
	next := copyBytesChunk
	for i := 0; ; {
		if i < len(s) && isBase64Space(s[i]) {
			if i, err = skipBase64Space(r, s, i); err != nil {
				return 0, 0, false, err
			}
		}
		if i >= next {
			if err := r.CheckInterrupt(); err != nil {
				return 0, 0, false, err
			}
			next = i + copyBytesChunk
		}
		if i == len(s) {
			if n > 0 {
				if h == base64StopBeforePartial {
					return read, written, true, nil
				}
				if h == base64Strict || n == 1 {
					return read, written, false, nil
				}
				written += finalBase64Chunk(dst[written:], acc, n)
			}
			return len(s), written, true, nil
		}
		c := s[i]
		i++
		if c == '=' {
			if n < 2 {
				return read, written, false, nil
			}
			if i, err = skipBase64Space(r, s, i); err != nil {
				return 0, 0, false, err
			}
			if n == 2 {
				if i == len(s) {
					return read, written, h == base64StopBeforePartial, nil
				}
				if s[i] == '=' {
					if i, err = skipBase64Space(r, s, i+1); err != nil {
						return 0, 0, false, err
					}
				}
			}
			// The extra bits are the low 4 of a chunk of 2, 2 of one of 3.
			if i < len(s) || h == base64Strict && acc&(1<<(8-2*n)-1) != 0 {
				return read, written, false, nil
			}
			return len(s), written + finalBase64Chunk(dst[written:], acc, n), true, nil
		}
		var v uint8
		if c < 0x80 {
			v = values[byte(c)]
		}
		if v == 0 {
			return read, written, false, nil
		}
		if rest := len(dst) - written; rest == 1 && n == 2 || rest == 2 && n == 3 {
			return read, written, true, nil
		}
		acc = acc<<6 | uint32(v-1)
		if n++; n == 4 {
			dst[written], dst[written+1], dst[written+2] = byte(acc>>16), byte(acc>>8), byte(acc)
			written += 3
			acc, n, read = 0, 0, i
			if written == len(dst) {
				return read, written, true, nil
			}
		}
	}
}

// finalBase64Chunk writes the bytes of a last chunk of n (2 or 3) sextets,
// acc, to dst and returns how many it wrote.
func finalBase64Chunk(dst []byte, acc uint32, n int) int {
	if n == 2 {
		dst[0] = byte(acc >> 4)
		return 1
	}
	dst[0], dst[1] = byte(acc>>10), byte(acc>>2)
	return 2
}

// fromHex implements FromHex(s, len(dst)) after its check of the length,
// decoding into dst: it returns the code units read, the bytes written and
// false where the spec's result has a SyntaxError. It honours the
// interrupt flag on long inputs.
func fromHex[C codeUnit](r *Realm, dst []byte, s []C) (read, written int, ok bool, err error) {
	for read+1 < len(s) && written < len(dst) {
		stop := min(len(dst), written+copyBytesChunk)
		for ; read+1 < len(s) && written < stop; read, written = read+2, written+1 {
			var hi, lo uint8
			if c := s[read]; c < 0x80 {
				hi = hexValues[byte(c)]
			}
			if c := s[read+1]; c < 0x80 {
				lo = hexValues[byte(c)]
			}
			if hi == 0 || lo == 0 {
				return read, written, false, nil
			}
			dst[written] = (hi-1)<<4 | (lo - 1)
		}
		if err := r.CheckInterrupt(); err != nil {
			return 0, 0, false, err
		}
	}
	return read, written, true, nil
}

// decodeBase64 runs fromBase64 over the code units of s. The whole chunks
// of alphabet characters an ASCII string starts with go to moego first
// (decodeBase64Bulk), and fromBase64 goes on from the chunk boundary where
// that stops, which is where it would be after decoding them itself.
// shared says dst is a typed array's bytes, which must not change past
// what is decoded.
func (r *Realm) decodeBase64(dst []byte, s *String, url bool, h base64Handling, shared bool) (read, written int, ok bool, err error) {
	if a, ok := s.ASCII(); ok {
		src := asciiBytes(a)
		read, written, err = r.decodeBase64Bulk(dst, src, url, shared)
		if err != nil {
			return 0, 0, false, err
		}
		n, m, ok, err := fromBase64(r, dst[written:], src[read:], url, h)
		return read + n, written + m, ok, err
	}
	return fromBase64(r, dst, s.UTF16(), url, h)
}

// decodeBase64Bulk decodes whole 4-character chunks of src with moego, in
// blocks that start at 4 KiB of input and double up to copyBytesChunk, as
// long as dst has room for them, and returns the characters read and the
// bytes written. It stops before the block that holds anything but
// alphabet characters (whitespace, padding, a character of neither), which
// the unpadded decoder either refuses or, for CR and LF, skips and so
// writes fewer bytes than the block's chunks, and before the last chunk,
// which may be partial or padded. Every option decodes these chunks
// alike. A shared dst gets each block through buf, as the decoder may
// write past the bytes it decodes. It honours the interrupt flag between
// blocks.
func (r *Realm) decodeBase64Bulk(dst, src []byte, url, shared bool) (read, written int, err error) {
	enc := base64.RawStdEncoding
	if url {
		enc = base64.RawURLEncoding
	}
	var buf [3 << 10]byte
	end := (len(src) - 1) &^ 3
	block := 4 << 10
	next := copyBytesChunk
	for {
		k := min(end-read, block, (len(dst)-written)/3*4)
		if shared {
			k = min(k, len(buf)/3*4)
		}
		if k < 4 {
			return read, written, nil
		}
		out := dst[written : written+k/4*3]
		if shared {
			out = buf[:len(out)]
		}
		if n, err := enc.Decode(out, src[read:read+k]); err != nil || n != len(out) {
			return read, written, nil
		}
		if shared {
			copy(dst[written:], out)
		}
		read, written = read+k, written+len(out)
		if read >= next {
			if err := r.CheckInterrupt(); err != nil {
				return 0, 0, err
			}
			next = read + copyBytesChunk
		}
		block = min(2*block, copyBytesChunk)
	}
}

// decodeHex implements FromHex(s, len(dst)) over the code units of s.
func (r *Realm) decodeHex(dst []byte, s *String) (read, written int, ok bool, err error) {
	if s.Len()%2 != 0 {
		return 0, 0, false, nil
	}
	if a, ok := s.ASCII(); ok {
		return fromHex(r, dst, asciiBytes(a))
	}
	return fromHex(r, dst, s.UTF16())
}

// uint8ArrayFromBase64 implements Uint8Array.fromBase64(string, options).
func uint8ArrayFromBase64(r *Realm, this Value, args []Value) (Value, error) {
	const method = "Uint8Array.fromBase64"
	s, err := stringArg(r, args, 0, method)
	if err != nil {
		return Undefined(), err
	}
	url, h, err := r.base64DecodeOptions(Arg(args, 1), method)
	if err != nil {
		return Undefined(), err
	}
	// With no limit, the decoder reads to the end, and a unit that is not
	// ASCII is an error wherever it is.
	if !s.IsASCII() {
		return Undefined(), r.SyntaxError("%s: the string is not valid base64", method)
	}
	// Room for all the bytes the input could decode to, so the decoder's
	// limit never stops it.
	size := (s.Len() + 3) / 4 * 3
	dst := r.newBytes(size, size)
	_, written, ok, err := r.decodeBase64(dst, s, url, h, false)
	if err != nil {
		return Undefined(), err
	}
	if !ok {
		return Undefined(), r.SyntaxError("%s: the string is not valid base64", method)
	}
	if dst, err = r.trimBytes(dst, written); err != nil {
		return Undefined(), err
	}
	return r.newUint8Array(dst)
}

// uint8ArrayFromHex implements Uint8Array.fromHex(string).
func uint8ArrayFromHex(r *Realm, this Value, args []Value) (Value, error) {
	const method = "Uint8Array.fromHex"
	s, err := stringArg(r, args, 0, method)
	if err != nil {
		return Undefined(), err
	}
	if !s.IsASCII() {
		return Undefined(), r.SyntaxError("%s: the string is not valid hex", method)
	}
	dst := r.newBytes(s.Len()/2, s.Len()/2)
	_, _, ok, err := r.decodeHex(dst, s)
	if err != nil {
		return Undefined(), err
	}
	if !ok {
		return Undefined(), r.SyntaxError("%s: the string is not valid hex", method)
	}
	return r.newUint8Array(dst)
}

// uint8ArraySetFromBase64 implements
// Uint8Array.prototype.setFromBase64(string, options).
func uint8ArraySetFromBase64(r *Realm, this Value, args []Value) (Value, error) {
	const method = "Uint8Array.prototype.setFromBase64"
	ta, err := thisUint8Array(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	s, err := stringArg(r, args, 0, method)
	if err != nil {
		return Undefined(), err
	}
	url, h, err := r.base64DecodeOptions(Arg(args, 1), method)
	if err != nil {
		return Undefined(), err
	}
	dst, err := uint8ArrayBytes(r, ta, method)
	if err != nil {
		return Undefined(), err
	}
	read, written, ok, err := r.decodeBase64(dst, s, url, h, true)
	if err != nil {
		return Undefined(), err
	}
	if !ok {
		return Undefined(), r.SyntaxError("%s: the string is not valid base64", method)
	}
	return r.readWritten(read, written), nil
}

// uint8ArraySetFromHex implements Uint8Array.prototype.setFromHex(string).
func uint8ArraySetFromHex(r *Realm, this Value, args []Value) (Value, error) {
	const method = "Uint8Array.prototype.setFromHex"
	ta, err := thisUint8Array(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	s, err := stringArg(r, args, 0, method)
	if err != nil {
		return Undefined(), err
	}
	dst, err := uint8ArrayBytes(r, ta, method)
	if err != nil {
		return Undefined(), err
	}
	read, written, ok, err := r.decodeHex(dst, s)
	if err != nil {
		return Undefined(), err
	}
	if !ok {
		return Undefined(), r.SyntaxError("%s: the string is not valid hex", method)
	}
	return r.readWritten(read, written), nil
}

// --- encoding ---------------------------------------------------------------------

// base64Encodings are the encodings of toBase64 by alphabet, then without
// padding.
var base64Encodings = [2][2]*base64.Encoding{
	{base64.StdEncoding, base64.RawStdEncoding},
	{base64.URLEncoding, base64.RawURLEncoding},
}

// encodeChunk is how many bytes the encoders take between interrupt checks,
// a multiple of 3 so that base64 chunks need no padding.
const encodeChunk = copyBytesChunk / 3 * 3

// uint8ArrayToBase64 implements Uint8Array.prototype.toBase64(options).
func uint8ArrayToBase64(r *Realm, this Value, args []Value) (Value, error) {
	const method = "Uint8Array.prototype.toBase64"
	ta, err := thisUint8Array(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	opts, err := r.optionsObject(Arg(args, 0), method)
	if err != nil {
		return Undefined(), err
	}
	a := binaryNames()
	alphabet, err := r.stringOption(opts, a.alphabet, base64AlphabetNames, method)
	if err != nil {
		return Undefined(), err
	}
	omitPadding := false
	if opts != nil {
		v, err := opts.GetProp(r, StringKey(a.omitPadding))
		if err != nil {
			return Undefined(), err
		}
		omitPadding = ToBoolean(v)
	}
	src, err := uint8ArrayBytes(r, ta, method)
	if err != nil {
		return Undefined(), err
	}
	pad := 0
	if omitPadding {
		pad = 1
	}
	enc := base64Encodings[alphabet][pad]
	n := enc.EncodedLen(len(src))
	if n > maxStringLength || r.overBudget(n) {
		return Undefined(), r.invalidStringLength()
	}
	r.chargeString(n)
	s, dst := newASCIIBuf(n)
	for len(src) > encodeChunk {
		enc.Encode(dst, src[:encodeChunk])
		dst, src = dst[encodeChunk/3*4:], src[encodeChunk:]
		if err := r.CheckInterrupt(); err != nil {
			return Undefined(), err
		}
	}
	enc.Encode(dst, src)
	return StringValue(s), nil
}

// uint8ArrayToHex implements Uint8Array.prototype.toHex().
func uint8ArrayToHex(r *Realm, this Value, args []Value) (Value, error) {
	const method = "Uint8Array.prototype.toHex"
	ta, err := thisUint8Array(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	src, err := uint8ArrayBytes(r, ta, method)
	if err != nil {
		return Undefined(), err
	}
	if len(src) > maxStringLength/2 || r.overBudget(2*len(src)) {
		return Undefined(), r.invalidStringLength()
	}
	r.chargeString(2 * len(src))
	s, dst := newASCIIBuf(2 * len(src))
	for len(src) > encodeChunk {
		hex.Encode(dst, src[:encodeChunk])
		dst, src = dst[2*encodeChunk:], src[encodeChunk:]
		if err := r.CheckInterrupt(); err != nil {
			return Undefined(), err
		}
	}
	hex.Encode(dst, src)
	return StringValue(s), nil
}

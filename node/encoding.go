package node

import (
	"encoding/binary"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/MoeForge/moejs/engine"
)

// encoding is a Node.js character encoding.
type encoding uint8

const (
	encUTF8 encoding = iota
	encUTF16LE
	encLatin1
	encASCII
	encBase64
	encBase64URL
	encHex
	encBuffer // no encoding: bytes, a Buffer
)

var encodingNames = [...]string{"utf8", "utf16le", "latin1", "ascii", "base64", "base64url", "hex", "buffer"}

func (enc encoding) String() string { return encodingNames[enc] }

// parseEncoding returns the encoding of name, as Node's normalizeEncoding
// reads it: case-insensitively, with its aliases.
func parseEncoding(name string) (encoding, bool) {
	switch name {
	case "utf8", "utf-8":
		return encUTF8, true
	case "hex":
		return encHex, true
	case "base64":
		return encBase64, true
	case "latin1", "binary":
		return encLatin1, true
	}
	switch strings.ToLower(name) {
	case "utf8", "utf-8", "":
		return encUTF8, name != ""
	case "ucs2", "ucs-2", "utf16le", "utf-16le":
		return encUTF16LE, true
	case "latin1", "binary":
		return encLatin1, true
	case "base64":
		return encBase64, true
	case "base64url":
		return encBase64URL, true
	case "hex":
		return encHex, true
	case "ascii":
		return encASCII, true
	}
	return 0, false
}

// --- bytes to strings ---------------------------------------------------------------

// decode returns the string of b in enc. owned says that nothing writes b
// later, so that an ASCII string can share its bytes.
func decode(enc encoding, b []byte, owned bool) *engine.String {
	if len(b) == 0 {
		return engine.EmptyString()
	}
	switch enc {
	case encUTF8:
		return decodeUTF8(b, owned)
	case encLatin1:
		if isASCII(b) {
			return asciiString(b, owned)
		}
		u := make([]uint16, len(b))
		for i, c := range b {
			u[i] = uint16(c)
		}
		return engine.FromUTF16(u)
	case encASCII:
		out := make([]byte, len(b))
		for i, c := range b {
			out[i] = c & 0x7f
		}
		return asciiString(out, true)
	case encUTF16LE:
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		return engine.FromUTF16(u)
	case encBase64:
		return asciiString(encodeBase64(b, &stdPairs, true), true)
	case encBase64URL:
		return asciiString(encodeBase64(b, &urlPairs, false), true)
	case encHex:
		return asciiString(encodeHex(b), true)
	}
	return decodeUTF8(b, owned)
}

// asciiString returns the string of the ASCII bytes b, sharing them when
// they are owned.
func asciiString(b []byte, owned bool) *engine.String {
	if owned {
		return engine.FromGoString(unsafe.String(unsafe.SliceData(b), len(b)))
	}
	return engine.FromGoString(string(b))
}

func isASCII(b []byte) bool {
	// Eight bytes at a time.
	i := 0
	for ; i+8 <= len(b); i += 8 {
		if binary.LittleEndian.Uint64(b[i:])&0x8080808080808080 != 0 {
			return false
		}
	}
	for ; i < len(b); i++ {
		if b[i] >= 0x80 {
			return false
		}
	}
	return true
}

// decodeUTF8 decodes b as the WHATWG decoder does: each maximal subpart
// of an ill-formed sequence becomes one U+FFFD, as Node's
// buffer.toString() and TextDecoder give it.
func decodeUTF8(b []byte, owned bool) *engine.String {
	// The engine converts text that is ASCII, or well-formed UTF-8, as the
	// WHATWG decoder does, with one scan for the common ASCII case.
	var s *engine.String
	if owned {
		s = engine.FromGoString(unsafe.String(unsafe.SliceData(b), len(b)))
	} else {
		s = engine.FromGoString(string(b))
	}
	if s.IsASCII() || utf8.Valid(b) {
		return s
	}
	u := make([]uint16, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			u = append(u, uint16(c))
			i++
			continue
		}
		r, n := decodeRune(b[i:])
		if r < 0x10000 {
			u = append(u, uint16(r))
		} else {
			hi, lo := utf16.EncodeRune(r)
			u = append(u, uint16(hi), uint16(lo))
		}
		i += n
	}
	return engine.FromUTF16(u)
}

// decodeRune decodes the sequence that starts b (b[0] >= 0x80), returning
// U+FFFD and the length of its maximal subpart when it is ill-formed.
func decodeRune(b []byte) (rune, int) {
	c := b[0]
	var need int
	var lo, hi byte = 0x80, 0xbf
	switch {
	case c >= 0xc2 && c <= 0xdf:
		need = 1
	case c == 0xe0:
		need, lo = 2, 0xa0
	case c >= 0xe1 && c <= 0xec, c == 0xee, c == 0xef:
		need = 2
	case c == 0xed:
		need, hi = 2, 0x9f
	case c == 0xf0:
		need, lo = 3, 0x90
	case c >= 0xf1 && c <= 0xf3:
		need = 3
	case c == 0xf4:
		need, hi = 3, 0x8f
	default:
		return utf8.RuneError, 1
	}
	for k := 1; k <= need; k++ {
		if k >= len(b) || b[k] < lo || b[k] > hi {
			return utf8.RuneError, k
		}
		lo, hi = 0x80, 0xbf
	}
	r, _ := utf8.DecodeRune(b[:need+1])
	return r, need + 1
}

// --- strings to bytes ---------------------------------------------------------------

// encode returns the bytes of s in enc, as Buffer.from(s, enc) has them.
func encode(enc encoding, s *engine.String) []byte {
	switch enc {
	case encUTF8, encBuffer:
		return encodeUTF8(nil, s)
	case encLatin1, encASCII:
		if a, ok := s.ASCII(); ok {
			return []byte(a)
		}
		n := s.Len()
		out := make([]byte, n)
		for i := 0; i < n; i++ {
			out[i] = byte(s.At(i))
		}
		return out
	case encUTF16LE:
		n := s.Len()
		out := make([]byte, 2*n)
		for i := 0; i < n; i++ {
			c := s.At(i)
			out[2*i], out[2*i+1] = byte(c), byte(c>>8)
		}
		return out
	case encBase64, encBase64URL:
		return decodeBase64(s)
	case encHex:
		return decodeHex(s)
	}
	return encodeUTF8(nil, s)
}

// encodeUTF8 appends the UTF-8 of s to dst; a lone surrogate becomes
// U+FFFD.
func encodeUTF8(dst []byte, s *engine.String) []byte {
	if a, ok := s.ASCII(); ok {
		return append(dst, a...)
	}
	n := s.Len()
	if dst == nil {
		dst = make([]byte, 0, utf8Len(s))
	}
	for i := 0; i < n; i++ {
		c := rune(s.At(i))
		if utf16.IsSurrogate(c) {
			if c < 0xdc00 && i+1 < n {
				if d := rune(s.At(i + 1)); d >= 0xdc00 && d <= 0xdfff {
					dst = utf8.AppendRune(dst, utf16.DecodeRune(c, d))
					i++
					continue
				}
			}
			c = utf8.RuneError
		}
		dst = utf8.AppendRune(dst, c)
	}
	return dst
}

// utf8Len returns the length of the UTF-8 of s.
func utf8Len(s *engine.String) int {
	if a, ok := s.ASCII(); ok {
		return len(a)
	}
	n, size := s.Len(), 0
	for i := 0; i < n; i++ {
		c := s.At(i)
		switch {
		case c < 0x80:
			size++
		case c < 0x800:
			size += 2
		case c >= 0xd800 && c < 0xdc00 && i+1 < n && s.At(i+1) >= 0xdc00 && s.At(i+1) <= 0xdfff:
			size += 4
			i++
		default:
			size += 3
		}
	}
	return size
}

// stdPairs and urlPairs map 12 bits to the two characters that encode
// them, little-endian, in the standard and the URL alphabet.
var stdPairs, urlPairs = base64Pairs("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"),
	base64Pairs("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")

func base64Pairs(alphabet string) (t [4096]uint16) {
	for i := range t {
		t[i] = uint16(alphabet[i>>6]) | uint16(alphabet[i&63])<<8
	}
	return t
}

// encodeBase64 encodes b, two characters per lookup, with padding when
// pad is set: the standard library's encoder looks up one at a time.
func encodeBase64(b []byte, pairs *[4096]uint16, pad bool) []byte {
	n := len(b) / 3 * 3
	size := len(b) / 3 * 4
	switch rest := len(b) - n; {
	case rest > 0 && pad:
		size += 4
	case rest > 0:
		size += rest + 1
	}
	out := make([]byte, size)
	// Six bytes per step: a 64-bit load, whose top 48 bits are four
	// 12-bit indexes, and a 64-bit store of their eight characters. The
	// loop proves the eight bytes it reads and writes are there, so the
	// loads and stores go through pointers, without bounds checks.
	i, j := 0, 0
	if last := min(len(b)-8, n-6); last >= 0 {
		src, dst := unsafe.Pointer(unsafe.SliceData(b)), unsafe.Pointer(unsafe.SliceData(out))
		for ; i <= last; i, j = i+6, j+8 {
			binary.LittleEndian.PutUint64(bytes8(dst, j), encodeSix(pairs, binary.BigEndian.Uint64(bytes8(src, i))))
		}
	}
	for ; i+3 <= n; i, j = i+3, j+4 {
		v := uint(b[i])<<16 | uint(b[i+1])<<8 | uint(b[i+2])
		binary.LittleEndian.PutUint32(out[j:j+4], uint32(pairs[v>>12])|uint32(pairs[v&0xfff])<<16)
	}
	j = n / 3 * 4
	if rest := len(b) - n; rest > 0 {
		v := uint(b[n]) << 16
		if rest == 2 {
			v |= uint(b[n+1]) << 8
		}
		p := pairs[v>>12]
		out[j], out[j+1] = byte(p), byte(p>>8)
		if rest == 2 {
			out[j+2] = byte(pairs[v&0xfff])
		} else if pad {
			out[j+2] = '='
		}
		if pad {
			out[j+3] = '='
		}
	}
	return out
}

// bytes8 returns the eight bytes at p+off, of an allocation at least
// that long.
func bytes8(p unsafe.Pointer, off int) []byte { return (*[8]byte)(unsafe.Add(p, off))[:] }

// encodeSix returns the eight characters of the top six bytes of x.
func encodeSix(pairs *[4096]uint16, x uint64) uint64 {
	return uint64(pairs[x>>52]) | uint64(pairs[x>>40&0xfff])<<16 | uint64(pairs[x>>28&0xfff])<<32 | uint64(pairs[x>>16&0xfff])<<48
}

// base64Dec are the four lookup tables of the decoder: the value of a
// character shifted to its place in a group of four, and 1<<31 for a
// character of neither alphabet.
var base64Dec = func() (t [4][256]uint32) {
	for k := range 4 {
		for c := range 256 {
			t[k][c] = 1 << 31
		}
		const std = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		shift := uint(18 - 6*k)
		for i := range 64 {
			t[k][std[i]] = uint32(i) << shift
		}
		t[k]['-'], t[k]['_'] = 62<<shift, 63<<shift
	}
	return t
}()

// base64Values maps the characters of both base64 alphabets to their
// values; 0xff marks the others.
var base64Values = func() (t [256]byte) {
	for i := range t {
		t[i] = 0xff
	}
	const std = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < 64; i++ {
		t[std[i]] = byte(i)
	}
	t['-'], t['_'] = 62, 63
	return t
}()

// decodeBase64 decodes s as Node does: either alphabet, the characters of
// neither skipped, up to the first '='; a final lone character is
// dropped.
func decodeBase64(s *engine.String) []byte {
	if a, ok := s.ASCII(); ok {
		if out, ok := decodeBase64Fast(a); ok {
			return out
		}
	}
	return decodeBase64Lenient(s)
}

// decodeBase64Lenient is decodeBase64 one character at a time.
func decodeBase64Lenient(s *engine.String) []byte {
	n := s.Len()
	out := make([]byte, 0, n*3/4+3)
	var acc uint32
	bits := 0
	a, ascii := s.ASCII()
	for i := 0; i < n; i++ {
		var c uint16
		if ascii {
			c = uint16(a[i])
		} else {
			c = s.At(i)
		}
		// Node reads a character above U+00FF by its low byte.
		c &= 0xff
		if c == '=' {
			break
		}
		v := base64Values[c]
		if v == 0xff {
			continue
		}
		acc = acc<<6 | uint32(v)
		if bits += 6; bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	return out
}

// base64Pair16 maps two characters, the first in the low byte, to the
// twelve bits they decode to, and 0x8000 for a pair with a character of
// neither alphabet: a 128 KiB table, built on first use, that decodes
// two characters per lookup.
var base64Pair16 = sync.OnceValue(func() *[65536]uint16 {
	t := new([65536]uint16)
	for i := range t {
		a, b := base64Values[i&0xff], base64Values[i>>8]
		if a == 0xff || b == 0xff {
			t[i] = 0x8000
		} else {
			t[i] = uint16(a)<<6 | uint16(b)
		}
	}
	return t
})

// decodeBase64Fast decodes a, base64 of either alphabet without
// whitespace, four characters per step; ok is false for anything else,
// which the lenient loop decodes.
func decodeBase64Fast(a string) ([]byte, bool) {
	n := len(a)
	if n > 0 && a[n-1] == '=' {
		n--
		if n > 0 && a[n-1] == '=' {
			n--
		}
	}
	if n%4 == 1 {
		return nil, false
	}
	out := make([]byte, n/4*3+max(n%4-1, 0))
	t := &base64Dec
	full := n / 4 * 4
	in := unsafe.Slice(unsafe.StringData(a), full)
	// Eight characters per step: a 64-bit load, two characters per
	// lookup, and a 64-bit store whose last two bytes the next one
	// overwrites; the characters of neither alphabet are looked for once
	// the loop is done. The loop proves the eight bytes it reads and
	// writes are there, so they go through pointers, without bounds
	// checks.
	p := base64Pair16()
	i, j := 0, 0
	if len(out) >= 8 && full >= 8 {
		last := min(full-8, (len(out)-8)/6*8)
		src, dst := unsafe.Pointer(unsafe.StringData(a)), unsafe.Pointer(unsafe.SliceData(out))
		var bad uint16
		for ; i <= last; i, j = i+8, j+6 {
			x := binary.LittleEndian.Uint64(bytes8(src, i))
			a0, a1 := p[uint16(x)], p[uint16(x>>16)]
			hi := uint64(a0)<<12 | uint64(a1)
			a2, a3 := p[uint16(x>>32)], p[x>>48]
			bad |= a0 | a1 | a2 | a3
			binary.BigEndian.PutUint64(bytes8(dst, j), hi<<40|(uint64(a2)<<12|uint64(a3))<<16)
		}
		if bad&0x8000 != 0 {
			return nil, false
		}
	}
	src, dst := in[i:], out[j:]
	for len(src) >= 4 && len(dst) >= 3 {
		v := t[0][src[0]] | t[1][src[1]] | t[2][src[2]] | t[3][src[3]]
		if v&(1<<31) != 0 {
			return nil, false
		}
		dst[0], dst[1], dst[2] = byte(v>>16), byte(v>>8), byte(v)
		src, dst = src[4:], dst[3:]
	}
	j = full / 4 * 3
	if rest := n - full; rest > 0 {
		v := t[0][a[full]] | t[1][a[full+1]]
		if rest == 3 {
			v |= t[2][a[full+2]]
		}
		if v&(1<<31) != 0 {
			return nil, false
		}
		out[j] = byte(v >> 16)
		if rest == 3 {
			out[j+1] = byte(v >> 8)
		}
	}
	return out, true
}

// base64Len is Node's base64ByteLength: the length of the decoding of a
// string of n characters that ends in s's padding.
func base64Len(s *engine.String) int {
	n := s.Len()
	if n > 0 && s.At(n-1) == '=' {
		n--
	}
	if n > 1 && s.At(n-1) == '=' {
		n--
	}
	return n * 3 >> 2
}

// hexPairs maps a byte to its two hex digits, little-endian.
var hexPairs = func() (t [256]uint16) {
	const digits = "0123456789abcdef"
	for i := range t {
		t[i] = uint16(digits[i>>4]) | uint16(digits[i&15])<<8
	}
	return t
}()

// encodeHex returns the hex digits of b, eight bytes per step.
func encodeHex(b []byte) []byte {
	out := make([]byte, 2*len(b))
	src, dst := b, out
	for len(src) >= 8 && len(dst) >= 16 {
		x := binary.LittleEndian.Uint64(src)
		binary.LittleEndian.PutUint64(dst, uint64(hexPairs[byte(x)])|uint64(hexPairs[byte(x>>8)])<<16|
			uint64(hexPairs[byte(x>>16)])<<32|uint64(hexPairs[byte(x>>24)])<<48)
		binary.LittleEndian.PutUint64(dst[8:], uint64(hexPairs[byte(x>>32)])|uint64(hexPairs[byte(x>>40)])<<16|
			uint64(hexPairs[byte(x>>48)])<<32|uint64(hexPairs[byte(x>>56)])<<48)
		src, dst = src[8:], dst[16:]
	}
	for i, c := range src {
		binary.LittleEndian.PutUint16(dst[2*i:], hexPairs[c])
	}
	return out
}

// hexValues maps a character to its value as a hex digit, 0xff for any
// other.
var hexValues = func() (t [256]byte) {
	for i := range t {
		t[i] = 0xff
	}
	for c := '0'; c <= '9'; c++ {
		t[c] = byte(c - '0')
	}
	for c := 'a'; c <= 'f'; c++ {
		t[c] = byte(c-'a') + 10
		t[c-'a'+'A'] = byte(c-'a') + 10
	}
	return t
}()

// decodeHex decodes s's pairs of hex digits up to the first pair that is
// not one, as Node does.
func decodeHex(s *engine.String) []byte {
	n := s.Len() / 2
	out := make([]byte, n)
	if a, ok := s.ASCII(); ok {
		for i := range out {
			hi, lo := hexValues[a[2*i]], hexValues[a[2*i+1]]
			if hi|lo == 0xff || hi > 15 || lo > 15 {
				return out[:i]
			}
			out[i] = hi<<4 | lo
		}
		return out
	}
	for i := range out {
		// Node reads a character above U+00FF by its low byte.
		hi, lo := hexValues[byte(s.At(2*i))], hexValues[byte(s.At(2*i+1))]
		if hi > 15 || lo > 15 {
			return out[:i]
		}
		out[i] = hi<<4 | lo
	}
	return out
}

// byteLength returns Buffer.byteLength(s, enc).
func byteLength(enc encoding, s *engine.String) int {
	switch enc {
	case encUTF8, encBuffer:
		return utf8Len(s)
	case encUTF16LE:
		return 2 * s.Len()
	case encLatin1, encASCII:
		return s.Len()
	case encBase64, encBase64URL:
		return base64Len(s)
	case encHex:
		return s.Len() >> 1
	}
	return utf8Len(s)
}

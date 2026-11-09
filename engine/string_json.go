package engine

import (
	"encoding/binary"
	"math/bits"
	"slices"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// JSON string scanning eight bytes at a time (SWAR). JSON quoting escapes
// control characters (< 0x20), '"' and '\\'; parsing stops at the same bytes
// and, until a string turns out not to be ASCII, at bytes >= 0x80. A run of
// other bytes is copied whole, so a long string without escapes (a base64
// image) costs one scan and one copy.

const (
	swarLSB = 0x0101010101010101
	swarMSB = 0x8080808080808080
)

// jsonSpecial returns the top bit of each byte of x (little-endian) that is a
// control character, '"' or '\\', or >= 0x80 when high is swarMSB (high is 0
// otherwise). A borrow can also flag bytes above a flagged one, so only the
// lowest flag is exact: that is the one jsonScan reads.
func jsonSpecial(x, high uint64) uint64 {
	q := x ^ ('"' * swarLSB)
	b := x ^ ('\\' * swarLSB)
	return ((x-0x20*swarLSB)&^x | (q-swarLSB)&^q | (b-swarLSB)&^b | x&high) & swarMSB
}

// jsonScan returns the index of the first byte of s at or after i that is a
// control character, '"' or '\\', or >= 0x80 when high is swarMSB; len(s)
// when there is none. The parser scans the rest of the text, so the first
// word is tested alone: most strings end in it.
//
// GC safety: as in isASCII, the []byte view of s is read-only and local.
func jsonScan(s string, i int, high uint64) int {
	b := unsafe.Slice(unsafe.StringData(s), len(s))[i:]
	if len(b) >= 8 {
		if m := jsonSpecial(binary.LittleEndian.Uint64(b), high); m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
		b = b[8:]
	}
	for len(b) >= 32 {
		m0 := jsonSpecial(binary.LittleEndian.Uint64(b), high)
		m1 := jsonSpecial(binary.LittleEndian.Uint64(b[8:]), high)
		m2 := jsonSpecial(binary.LittleEndian.Uint64(b[16:]), high)
		m3 := jsonSpecial(binary.LittleEndian.Uint64(b[24:]), high)
		if m0|m1|m2|m3 != 0 {
			at := len(s) - len(b)
			switch {
			case m0 != 0:
				return at + bits.TrailingZeros64(m0)>>3
			case m1 != 0:
				return at + 8 + bits.TrailingZeros64(m1)>>3
			case m2 != 0:
				return at + 16 + bits.TrailingZeros64(m2)>>3
			}
			return at + 24 + bits.TrailingZeros64(m3)>>3
		}
		b = b[32:]
	}
	for len(b) >= 8 {
		if m := jsonSpecial(binary.LittleEndian.Uint64(b), high); m != 0 {
			return len(s) - len(b) + bits.TrailingZeros64(m)>>3
		}
		b = b[8:]
	}
	for j, c := range b {
		if c < 0x20 || c == '"' || c == '\\' || c&byte(high) != 0 {
			return len(s) - len(b) + j
		}
	}
	return len(s)
}

// wtf8Units returns the number of UTF-16 code units of the well-formed
// WTF-8 text s: one per byte that is not a continuation byte (10xxxxxx),
// and two for a four-byte sequence (its lead is 11110xxx). Eight bytes a
// step, the flags in the top bit of each byte summed by a multiplication.
//
// It does not validate multi-byte sequences: its text is well-formed, and
// appendWellFormed decodes it to exactly this many units.
//
// GC safety: as in isASCII, the []byte view of s is read-only and local.
func wtf8Units(s string) int {
	n := len(s)
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	for len(b) >= 8 {
		x := binary.LittleEndian.Uint64(b)
		cont := x &^ (x << 1) & swarMSB
		lead4 := x & (x << 1) & (x << 2) & (x << 3) & swarMSB
		n += int((lead4>>7)*swarLSB>>56) - int((cont>>7)*swarLSB>>56)
		b = b[8:]
	}
	for _, c := range b {
		switch {
		case c&0xC0 == 0x80:
			n--
		case c >= 0xF0:
			n++
		}
	}
	return n
}

// utf8Units reports whether s is valid UTF-8 and, if it is, the number of
// UTF-16 code units it encodes, in one pass: each sequence is checked as
// utf8.ValidString checks it (no overlong form, no surrogate, nothing above
// U+10FFFF), and a four-byte one counts two units. A run of ASCII is taken
// eight bytes a step. The count of invalid text is meaningless.
//
// GC safety: as in isASCII, the []byte view of s is read-only and local.
func utf8Units(s string) (n int, valid bool) {
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80:
			// An ASCII byte may begin a run: take it eight bytes a step.
			n++
			i++
			for i+8 <= len(b) && binary.LittleEndian.Uint64(b[i:])&swarMSB == 0 {
				n += 8
				i += 8
			}
		case c < 0xC2: // a continuation byte, or an overlong two-byte lead
			return n, false
		case c < 0xE0:
			if i+1 >= len(b) || b[i+1]&0xC0 != 0x80 {
				return n, false
			}
			n++
			i += 2
		case c < 0xF0:
			lo, hi := byte(0x80), byte(0xBF)
			switch c {
			case 0xE0:
				lo = 0xA0 // not overlong
			case 0xED:
				hi = 0x9F // not a surrogate
			}
			if i+2 >= len(b) || b[i+1] < lo || b[i+1] > hi || b[i+2]&0xC0 != 0x80 {
				return n, false
			}
			n++
			i += 3
		case c < 0xF5:
			lo, hi := byte(0x80), byte(0xBF)
			switch c {
			case 0xF0:
				lo = 0x90 // not overlong
			case 0xF4:
				hi = 0x8F // not above U+10FFFF
			}
			if i+3 >= len(b) || b[i+1] < lo || b[i+1] > hi || b[i+2]&0xC0 != 0x80 || b[i+3]&0xC0 != 0x80 {
				return n, false
			}
			n += 2
			i += 4
		default:
			return n, false
		}
	}
	return n, true
}

// appendWellFormed appends the UTF-16 code units of s, which must be valid
// UTF-8 or well-formed WTF-8, without checking it: the lead byte of each
// sequence gives its length, and a three-byte one encoding a surrogate
// decodes to the surrogate, as appendWTF8Units decodes it. It writes what
// wtf8Units counts, and decodes faster than appendWTF8Units, which accepts
// any bytes.
func appendWellFormed(u []uint16, s string) []uint16 {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c < 0x80:
			u = append(u, uint16(c))
			i++
		case c < 0xE0:
			u = append(u, uint16(c&0x1F)<<6|uint16(s[i+1]&0x3F))
			i += 2
		case c < 0xF0:
			u = append(u, uint16(c&0x0F)<<12|uint16(s[i+1]&0x3F)<<6|uint16(s[i+2]&0x3F))
			i += 3
		default:
			r := (rune(c&0x07)<<18 | rune(s[i+1]&0x3F)<<12 | rune(s[i+2]&0x3F)<<6 | rune(s[i+3]&0x3F)) - 0x10000
			u = append(u, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			i += 4
		}
	}
	return u
}

// jsonPlainMin is the byte length from which lazy text strings cache whether
// they are ASCII with nothing to escape. Quoting or a JSON size bound first
// records the result; shorter strings use the ordinary quoting path. It is
// also the length from which FromHostString keeps ASCII text lazy.
const jsonPlainMin = 64 << 10

// isASCIILong is isASCII for a long string: 32 bytes a step, tested
// together.
//
// GC safety: as in isASCII, the []byte view of s is read-only and local.
func isASCIILong(s string) bool {
	b := unsafe.Slice(unsafe.StringData(s), len(s))
	for len(b) >= 32 {
		x := binary.LittleEndian.Uint64(b) | binary.LittleEndian.Uint64(b[8:]) |
			binary.LittleEndian.Uint64(b[16:]) | binary.LittleEndian.Uint64(b[24:])
		if x&swarMSB != 0 {
			return false
		}
		b = b[32:]
	}
	return isASCII(s[len(s)-len(b):])
}

// jsonMaybeSpecial sets the top bit of each byte of x (little-endian) that
// is below 0x23, '\\' or >= 0x80, and maybe of the bytes above one, the
// other bits being noise: the bytes jsonSpecial flags with high set, and
// ' ' and '!', in fewer operations. Only whether any byte is flagged is
// exact.
func jsonMaybeSpecial(x uint64) uint64 {
	return (x - 0x23*swarLSB) | x | (x ^ '\\'*swarLSB - swarLSB)
}

// jsonASCII reports whether g is ASCII and whether it has no byte JSON
// quoting escapes: one scan for both. Blocks of 64 bytes are tested whole
// for the bytes jsonMaybeSpecial flags, until one has any (a payload
// without escapes has none); jsonScan finds the first byte from there.
//
// GC safety: as in isASCII, the []byte view of g is read-only and local.
func jsonASCII(g string) (ascii, plain bool) {
	b := unsafe.Slice(unsafe.StringData(g), len(g))
	for len(b) >= 64 {
		m := jsonMaybeSpecial(binary.LittleEndian.Uint64(b)) |
			jsonMaybeSpecial(binary.LittleEndian.Uint64(b[8:])) |
			jsonMaybeSpecial(binary.LittleEndian.Uint64(b[16:])) |
			jsonMaybeSpecial(binary.LittleEndian.Uint64(b[24:])) |
			jsonMaybeSpecial(binary.LittleEndian.Uint64(b[32:])) |
			jsonMaybeSpecial(binary.LittleEndian.Uint64(b[40:])) |
			jsonMaybeSpecial(binary.LittleEndian.Uint64(b[48:])) |
			jsonMaybeSpecial(binary.LittleEndian.Uint64(b[56:]))
		if m&swarMSB != 0 {
			break
		}
		b = b[64:]
	}
	i := jsonScan(g, len(g)-len(b), swarMSB)
	if i == len(g) {
		return true, true
	}
	return g[i] < 0x80 && isASCII(g[i:]), false
}

// jsonEscapes returns the top bit of each byte of x (little-endian) that
// JSON quoting escapes, a control character, '"' or '\\', in all, and of
// each control character in ctl. Unlike jsonSpecial's, every flag is
// exact, as no byte carries into the next (its low seven bits plus at most
// 0x7F), so the flags can be counted.
func jsonEscapes(x uint64) (all, ctl uint64) {
	const low = 0x7F7F7F7F7F7F7F7F
	q := x ^ '"'*swarLSB
	b := x ^ '\\'*swarLSB
	ctl = ^(x&low + 0x60*swarLSB | x) & swarMSB
	all = ctl | ^((q&low+low|q)&(b&low+low|b))&swarMSB
	return all, ctl
}

// jsonQuotedLen returns the length in code units of JSON quoting of the
// flat string f, the quotation marks included, and whether f is ASCII
// with nothing to escape.
//
// GC safety: as in isASCII, the []byte view of f's bytes is read-only and
// local.
func jsonQuotedLen(f *String) (n int64, plain bool) {
	if f.kind != strASCII {
		return jsonQuotedLenUTF16(f.units()), false
	}
	g := f.s
	n = int64(len(g)) + 2
	if f.json == jsonPlain {
		return n, true
	}
	b := unsafe.Slice(unsafe.StringData(g), len(g))
	for ; len(b) >= 8; b = b[8:] {
		all, ctl := jsonEscapes(binary.LittleEndian.Uint64(b))
		n += int64(bits.OnesCount64(all))
		for ; ctl != 0; ctl &= ctl - 1 {
			if jsonEscape[b[bits.TrailingZeros64(ctl)>>3]] == 0 {
				n += 4 // \u00XX
			}
		}
	}
	for _, c := range b {
		if c < 0x20 || c == '"' || c == '\\' {
			n++
			if jsonEscape[c] == 0 {
				n += 4
			}
		}
	}
	return n, n == int64(len(g))+2
}

// jsonQuotedLenUTF16 is jsonQuotedLen of the UTF-16 units u: a surrogate
// pair stays, a lone surrogate takes six units.
func jsonQuotedLenUTF16(u []uint16) int64 {
	n := int64(len(u)) + 2
	for i := 0; i < len(u); i++ {
		switch c := u[i]; {
		case c >= 0x20 && c != '"' && c != '\\' && (c < 0xD800 || c >= 0xE000):
		case c < 0x80:
			n++
			if jsonEscape[c] == 0 {
				n += 4
			}
		case c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000:
			i++
		default:
			n += 5
		}
	}
	return n
}

// jsonShortMax is the longest literal JSON.parse shares between repeats.
const jsonShortMax = 16

// plain returns the String of a literal with no escape and no byte >= 0x80,
// one per content among the recent short ones.
func (p *jsonParser) plain(lit string) *String {
	if len(lit) == 0 {
		return emptyString
	}
	if len(lit) > jsonShortMax {
		if lz := p.r.lazy; lz != nil && lz.mem != nil {
			return p.r.sharedPlain(lit)
		}
		return &String{s: lit, n: int32(len(lit)), kind: strASCII, flags: strSlice, json: jsonPlain}
	}
	slot := strSlot(lit)
	if s := p.short[slot]; s != nil && s.s == lit {
		return s
	}
	var s *String
	if lz := p.r.lazy; lz != nil && lz.mem != nil {
		s = p.r.sharedPlain(lit)
	} else {
		s = &String{s: lit, n: int32(len(lit)), kind: strASCII, flags: strSlice, json: jsonPlain}
	}
	p.short[slot] = s
	return s
}

// sharedPlain is plain's String of a realm with a memory limit: charged,
// and with the text it shares the units of as its share root
// (memAccount.jsonText), which the live measurement counts.
//
//go:noinline
func (r *Realm) sharedPlain(lit string) *String {
	r.chargeSlow(stringHeaderSize, memString)
	return &String{s: lit, n: int32(len(lit)), kind: strASCII, json: jsonPlain, right: r.lazy.mem.jsonText}
}

// quoteASCII writes the quoted body of the ASCII string str. The interrupt
// is checked every interruptStride bytes.
//
// GC safety: as in isASCII, the []byte view of str is read-only and local.
func (js *jsonStringifier) quoteASCII(str string) error {
	sb := &js.sb
	if len(str) < 16 { // most keys and many values: bytes are quicker
		run := 0
		for i := range len(str) {
			if c := str[i]; c < 0x20 || c == '"' || c == '\\' {
				writeASCIIString(sb, str[run:i])
				js.escapeByte(c)
				run = i + 1
			}
		}
		writeASCIIString(sb, str[run:])
		return nil
	}
	sb.Grow(len(str) + 1)
	view := unsafe.Slice(unsafe.StringData(str), len(str))
	run := 0
	for base := 0; base < len(str); base += interruptStride {
		end := min(base+interruptStride, len(str))
		if base > 0 {
			if err := js.r.CheckInterrupt(); err != nil {
				return err
			}
		}
		var x uint64 // the word at run when it holds i, else 0
		for i := jsonScan(str[:end], base, 0); i < end; {
			if n, room := len(sb.b), cap(sb.b)-len(sb.b); room >= i-run+14 {
				// The heap bytes have room: the run and the escape are
				// written in place and only the length is stored
				// (writeASCIIBytes). A run within its word is stored
				// whole, the escape written over the bytes after it.
				d := sb.b[n : n+room]
				k := i - run
				if x != 0 {
					binary.LittleEndian.PutUint64(d, x)
				} else {
					copy(d, str[run:i])
				}
				if e := jsonEscape[str[i]]; e != 0 {
					d[k], d[k+1] = '\\', e
					k += 2
				} else {
					c := str[i]
					d[k], d[k+1], d[k+2], d[k+3], d[k+4], d[k+5] = '\\', 'u', '0', '0', lowerHex[c>>4], lowerHex[c&15]
					k += 6
				}
				sb.b = sb.b[:n+k]
			} else {
				writeASCIIString(sb, str[run:i])
				js.escapeByte(str[i])
			}
			// The next special: escaped text has them close together, so
			// the first word is tested here, as jsonScan would.
			run, x = i+1, 0
			if run+8 > end {
				i = jsonScan(str[:end], run, 0)
			} else if w := binary.LittleEndian.Uint64(view[run:]); jsonSpecial(w, 0) != 0 {
				i, x = run+bits.TrailingZeros64(jsonSpecial(w, 0))>>3, w
			} else {
				i = jsonScan(str[:end], run+8, 0)
			}
		}
	}
	writeASCIIString(sb, str[run:])
	return nil
}

// quoteUTF16 writes the quoted body of the UTF-16 string u: the runs between
// units to escape are appended whole, a surrogate pair stays, a lone
// surrogate is escaped.
func (js *jsonStringifier) quoteUTF16(u []uint16) error {
	sb := &js.sb
	run := 0
	for i := 0; i < len(u); i++ {
		if err := interruptEvery(js.r, int64(i)); err != nil {
			return err
		}
		c := u[i]
		if c >= 0x20 && c != '"' && c != '\\' && (c < 0xD800 || c >= 0xE000) {
			continue
		}
		if c >= 0xD800 && c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000 {
			i++
			continue
		}
		sb.WriteUTF16(u[run:i])
		run = i + 1
		if c < 0x80 {
			js.escapeByte(byte(c))
			continue
		}
		sb.writeASCII2('\\', 'u')
		sb.writeASCII2(lowerHex[c>>12], lowerHex[c>>8&15])
		sb.writeASCII2(lowerHex[c>>4&15], lowerHex[c&15])
	}
	sb.WriteUTF16(u[run:])
	return nil
}

// quoteUTF8 is quoteUTF16 for AppendJSON: it encodes the units as UTF-8
// into sb.b, which a raw stringifier always has (AppendJSON).
func (js *jsonStringifier) quoteUTF8(u []uint16) error {
	sb := &js.sb
	b := slices.Grow(sb.b, len(u)+len(u)/2+1)
	for i := 0; i < len(u); i++ {
		if err := interruptEvery(js.r, int64(i)); err != nil {
			sb.b = b
			return err
		}
		switch c := u[i]; {
		case c < 0x80:
			if c < 0x20 || c == '"' || c == '\\' {
				b = appendJSONEscape(b, byte(c))
				continue
			}
			b = append(b, byte(c))
		case c < 0x800:
			b = append(b, 0xC0|byte(c>>6), 0x80|byte(c)&0x3F)
		case c < 0xD800 || c >= 0xE000:
			b = append(b, 0xE0|byte(c>>12), 0x80|byte(c>>6)&0x3F, 0x80|byte(c)&0x3F)
		case c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000:
			b = utf8.AppendRune(b, utf16.DecodeRune(rune(c), rune(u[i+1])))
			i++
		default:
			b = append(b, '\\', 'u', lowerHex[c>>12], lowerHex[c>>8&15], lowerHex[c>>4&15], lowerHex[c&15])
		}
	}
	sb.b = b
	return nil
}

// serialize writes v as JSONStringify and AppendJSON do, with no replacer
// and no indentation; ok is false when v has no JSON representation or on
// an error, which leaves the call depth as it found it.
func (js *jsonStringifier) serialize(v Value) (ok bool, err error) {
	rv, err := js.resolve(v, StringKey(AtomEmpty), nil)
	if err != nil {
		return false, err
	}
	depth := js.r.callDepth
	if err := js.r.enterJSONUnits(jsonRootUnits); err != nil {
		return false, err
	}
	ok, err = js.write(rv)
	js.r.callDepth = depth // the walk's root units, and the levels an error leaves entered (push)
	if err != nil || !ok {
		if !ok && err == nil && js.strict {
			err = newJSONLoss(rv)
		}
		return false, err
	}
	return true, nil
}

// AppendJSON appends JSONStringify(v) to dst as UTF-8 and reports whether v
// has a JSON representation; dst comes back unchanged when it has none or on
// an error. The output is written into dst's spare capacity, grown once to
// the last output's length plus an eighth (jsonSizeHint; after one very
// large output, a small dst is grown that much again), so a result that fits
// costs no copy; it never goes through UTF-16. Its limit is maxStringLength
// counted in bytes, which UTF-8 of text that is not ASCII reaches before
// JSONStringify's units do. Before any JavaScript runs (a toJSON, a getter,
// a proxy's trap) the output moves to a buffer of its own (own), so a host
// function that code calls may use dst: it is appended to when AppendJSON
// returns, as append(dst, text...) would.
func (r *Realm) AppendJSON(dst []byte, v Value) ([]byte, bool, error) {
	return r.appendJSON(dst, v, false)
}

// AppendJSONWith is AppendJSON with the options of opts. With Strict, a
// value AppendJSON would write otherwise or drop is a *LossyJSONError, and
// dst comes back unchanged.
func (r *Realm) AppendJSONWith(dst []byte, v Value, opts JSONOptions) ([]byte, bool, error) {
	out, ok, err := r.appendJSON(dst, v, opts.Strict)
	if l, isLoss := err.(*jsonLoss); isLoss {
		err = l.export()
	}
	return out, ok, err
}

func (r *Realm) appendJSON(dst []byte, v Value, strict bool) ([]byte, bool, error) {
	js := jsonStringifier{r: r, raw: true, strict: strict}
	out := slices.Grow(dst, max(256, int(r.jsonSizeHint)+int(r.jsonSizeHint)/8))
	js.sb.b = out[len(out):]
	js.inDst = unsafe.SliceData(out) == unsafe.SliceData(dst)
	if ok, err := js.serialize(v); !ok {
		return dst, false, err
	}
	b := js.sb.b
	r.jsonSizeHint = int32(len(b))
	switch {
	case unsafe.SliceData(b) == unsafe.SliceData(out[len(out):]):
		return out[:len(out)+len(b)], true, nil
	case len(dst) == 0 && cap(dst) < len(b):
		return b, true, nil // dst could not hold it: the output's own buffer
	}
	return append(dst, b...), true, nil // into dst, whose capacity the host keeps
}

// JobsPending reports whether jobs are queued for the end of the outermost
// call (or of the hold of HoldJobs): what a call or a conversion that ran
// code left to run, nothing when no code ran. The root AppendJSON asks it
// before it lets its output into dst (its jobs may use dst). It sits here,
// after (*String).flatten and out of line, for layout (realm_calldata.go):
// a symbol of one 32-byte slot in every binary, which with Unmarshal's
// growth keeps (*Runtime).Call on its 64-byte phase.
//
//go:noinline
func (r *Realm) JobsPending() bool { return r.jobsPending }

// own moves AppendJSON's output out of dst's spare capacity, once, before
// code runs that could call the host. The buffer holds twice what is
// written so far (at least 256 bytes) and grows as the rest is written:
// neither dst's spare capacity, which a reused buffer makes as large as the
// largest result it ever held, nor the last output's length (jsonSizeHint),
// which a host alternating large and small results makes just as large,
// says anything about this output.
func (js *jsonStringifier) own() {
	if js.inDst {
		js.inDst = false
		b := make([]byte, len(js.sb.b), ownCap(len(js.sb.b)))
		copy(b, js.sb.b)
		js.sb.b = b
	}
}

// ownCap sizes own's buffer: twice the bytes written so far, at least 256.
// It is out of line, and so is own then, for layout (realm_calldata.go):
// the stringifier's walk, which calls own in four places, is smaller by it,
// and proxy's placement keeps the walk's 64-byte phases. own runs only when
// code runs.
//
//go:noinline
func ownCap(n int) int { return max(256, 2*n) }

// JSONParseGoString is JSONParse of the UTF-8 text, whose invalid bytes read
// as U+FFFD as FromGoString reads them. ASCII and valid UTF-8 text is parsed
// in place, never through UTF-16; the strings of the result may alias text.
// Under a memory limit its allocation waits for it to return to be
// measured, as a builtin's does: until then the tree it builds is held
// only by the parser (parseText).
func (r *Realm) JSONParseGoString(text string) (Value, error) {
	p := jsonParser{r: r}
	if isASCII(text) || utf8.ValidString(text) {
		p.knownUTF8 = true
		return p.parseText(text, nil)
	}
	return p.parse(FromGoString(text))
}

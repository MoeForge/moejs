package engine

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// JSON builtins. parse scans bytes (ASCII text zero-copy; a
// UTF-16 text is converted once to a WTF-8 working copy so lone surrogates
// survive) and builds objects directly into the realm's shape tree, so
// documents with the same key order share shapes with each other and with
// FromGo. stringify streams into a pre-sized StringBuilder.

var jsonMethods = []builtinDef{
	{AtomParse, jsonParse, 2},
	{AtomStringify, jsonStringify, 3},
}

func installJSON(r *Realm) {
	r.installBuiltins(r.JSON, jsonMethods)
}

// jsonMaxDepth bounds nesting in parse, stringify and the reviver walk, as
// deep as ToGo exports: past it each is a RangeError. The parser runs no
// user code, so its recursion, a few hundred bytes of Go stack a level, is
// held by one parse at a time. Stringify and the reviver walk run user code
// that can start another walk, so each jsonCallLevels levels of a walk take
// a walk unit of the call depth (enterJSONUnits), which bounds the Go stack
// all the walks hold together. A walk takes jsonRootUnits at its root, and
// stringify counts levels only past jsonScanDepth, so that a shallow value
// pays one counter update: the walks that fit when a toJSON or a reviver at
// each one's last uncounted level starts the next hold under 5 MB.
const jsonMaxDepth = MaxToGoDepth

// jsonTooDeep is the RangeError of a value nested deeper than jsonMaxDepth
// arrays and objects. It wraps ErrTooDeep.
func (r *Realm) jsonTooDeep() error {
	return r.tooDeep("JSON nesting is too deep (more than %d levels)")
}

// jsonCallLevels is how many levels of a stringify or reviver walk take one
// walk unit.
const jsonCallLevels = 32

// jsonWalkUnits is how many walk units fit in the call depth when nothing
// else is on it. A unit takes jsonUnitCalls calls, so the walks' bound does
// not move with MaxCallDepth; the calls a walk's user code makes take one
// each, as anywhere. A walk's root takes jsonRootUnits, the units of the
// jsonScanDepth levels stringify does not count.
const (
	jsonWalkUnits = 512
	jsonUnitCalls = MaxCallDepth / jsonWalkUnits
	jsonRootUnits = jsonScanDepth / jsonCallLevels
)

// enterJSONUnits takes n walk units of the call depth; exitJSONUnits
// releases them.
func (r *Realm) enterJSONUnits(n int32) error {
	if r.callDepth > MaxCallDepth-n*jsonUnitCalls {
		return r.RangeError("Maximum call stack size exceeded")
	}
	r.callDepth += n * jsonUnitCalls
	return nil
}

func (r *Realm) exitJSONUnits(n int32) { r.callDepth -= n * jsonUnitCalls }

// --- parse -------------------------------------------------------------------------------

// JSONParse parses text with the JSON grammar and no reviver (Go-callable
// entry point). Errors are JavaScript SyntaxErrors, except that nesting
// deeper than MaxToGoDepth arrays and objects is a RangeError that wraps
// ErrTooDeep. Its allocation waits as JSONParseGoString's does.
func (r *Realm) JSONParse(text *String) (Value, error) {
	p := jsonParser{r: r}
	return p.parse(text)
}

func jsonParse(r *Realm, this Value, args []Value) (Value, error) {
	text, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	p := jsonParser{r: r}
	v, err := p.parse(text)
	if err != nil {
		return Undefined(), err
	}
	reviver := Arg(args, 1)
	if !IsCallable(reviver) {
		return v, nil
	}
	root := r.NewObject()
	root.DefineOwnDataFast(r, StringKey(AtomEmpty), v, attrDefault)
	// The root is on the register stack: the measurements made in the
	// reviver reach the tree (FunctionData.measurable).
	pin := r.pushArgs(1)
	defer r.popArgs(1)
	pin[0] = ObjectValue(root)
	return r.internalizeJSONProperty(root, StringKey(AtomEmpty), reviver, 0)
}

// internalizeJSONProperty implements InternalizeJSONProperty; depth counts
// the containers above holder. The parse bounds it by jsonMaxDepth, but the
// reviver can grow the structure as it is walked (a new array or a proxy per
// level), so past that the walk is a RangeError, not a Go stack overflow.
func (r *Realm) internalizeJSONProperty(holder *Object, name PropertyKey, reviver Value, depth int) (Value, error) {
	if depth > jsonMaxDepth {
		return Undefined(), r.jsonTooDeep()
	}
	if depth%jsonCallLevels == 0 {
		n := int32(1)
		if depth == 0 {
			n = jsonRootUnits
		}
		if err := r.enterJSONUnits(n); err != nil {
			return Undefined(), err
		}
		defer r.exitJSONUnits(n)
	}
	val, err := holder.Get(r, name, ObjectValue(holder))
	if err != nil {
		return Undefined(), err
	}
	if val.IsObject() {
		o := val.AsObject()
		isArr, err := r.isArray(val)
		if err != nil {
			return Undefined(), err
		}
		if isArr {
			// The length comes from a proxy's get trap as easily as from an
			// array, so it may be up to 2^53-1: walk it as an int64 with
			// canonical keys past 2^32-2 and stay interruptible.
			n, err := r.LengthOfArrayLike(o)
			if err != nil {
				return Undefined(), err
			}
			for i := int64(0); i < n; i++ {
				if err := r.reviveJSONElement(o, indexKey(r, i), reviver, depth); err != nil {
					return Undefined(), err
				}
				if err := interruptEvery(r, i); err != nil {
					return Undefined(), err
				}
			}
		} else {
			keys, err := r.enumerableOwnKeys(o)
			if err != nil {
				return Undefined(), err
			}
			for _, k := range keys {
				if err := r.reviveJSONElement(o, k, reviver, depth); err != nil {
					return Undefined(), err
				}
			}
		}
	}
	return r.Call(reviver, ObjectValue(holder), []Value{StringValue(name.ToJSString(r)), val})
}

// reviveJSONElement revives o[k] one level below depth and deletes or
// redefines it with the result.
func (r *Realm) reviveJSONElement(o *Object, k PropertyKey, reviver Value, depth int) error {
	nv, err := r.internalizeJSONProperty(o, k, reviver, depth+1)
	if err != nil {
		return err
	}
	if nv.IsUndefined() {
		_, err = r.deleteProperty(o, k)
		return err
	}
	_, err = o.CreateDataProperty(r, k, nv)
	return err
}

type jsonParser struct {
	r *Realm
	// src is valid UTF-8 shared with the caller (knownUTF8), or a WTF-8
	// copy of a JS string's units, which may encode lone surrogates.
	src string
	// short holds recent short literals by their raw contents (strSlot):
	// repeated plain text, Unicode and escapes can share one String.
	short [hostStrSlots]*String
	pos   int
	depth int
	// work counts the values parsed (only its low bits are read). It is an
	// int32 so that quiet shares its word: one word more in the parser
	// costs about a quarter of an instruction per byte of a long text
	// (ParseJSON of the multi-image bodies, 2.7 and 7.5 MiB: +2.4 % and
	// +4.1 % instructions, GOGC=off).
	work int32
	// quiet: observe no interrupt (a parse for FromGo, which may run where
	// no error can be returned, materializing a json.RawMessage).
	quiet bool
	// knownUTF8 applies to the whole src. Unescaped literals inherit this
	// guarantee without another UTF-8 check; false means unknown, not invalid.
	knownUTF8 bool
	// used is the largest length the value stack had when a container
	// closed (closed), saturating; it shares the word of work and quiet.
	used  uint16
	stack []Value
	keys  []PropertyKey
}

// jsonStack is the stack a parse builds containers on: the values and keys
// of the containers still open. A parse runs no JavaScript (a reviver runs
// once it has returned), so the realm keeps one (realmLazy) that every
// parse reuses and leaves cleared, so that what it keeps pins nothing. A Go
// panic out of a parse skips the clearing; DropJobs does it at the host's
// boundary.
type jsonStack struct {
	vals []Value
	keys []PropertyKey
}

// jsonStackMax bounds the entries a kept stack has room for (4 KiB each);
// a parse that needed more grows its own from nil.
const jsonStackMax = 256

func (p *jsonParser) parse(text *String) (Value, error) {
	src, root, knownUTF8 := text.wtf8Root()
	p.knownUTF8 = knownUTF8
	return p.parseText(src, root)
}

// parseText parses src, UTF-8 or WTF-8; the strings of the result may alias
// it. text is the String whose bytes src is (String.wtf8Root), nil for the
// host's text or a working copy.
func (p *jsonParser) parseText(src string, text *String) (Value, error) {
	p.src = src
	l := p.r.lazyState()
	if l.json == nil {
		l.json = &jsonStack{}
	}
	p.stack, p.keys = l.json.vals, l.json.keys
	if m := l.mem; m != nil {
		// The tree is held only by the parser until it returns: a charge
		// made meanwhile waits (holding; a Go panic leaves it to
		// RestoreCallState). The stacks' growth is charged as it goes
		// (every), past the room the kept ones have.
		m.shareText(src, text)
		m.holding++
		m.jsonVals, m.jsonKeys = cap(p.stack), cap(p.keys)
	}
	p.skipWS()
	v, err := p.value()
	if err == nil {
		p.skipWS()
		if p.pos < len(p.src) {
			err = p.unexpected()
		}
	}
	l.json.keep(p.stack, p.keys, int(p.used))
	if m := l.mem; m != nil {
		m.jsonText = nil
		m.holding--
	}
	if err != nil {
		return Undefined(), err
	}
	return v, nil
}

// keep takes back the stack a parse grew, clearing the entries it left: a
// closed container's up to used (closed), and those of the containers a
// failed parse was still in. A key is pushed only with its value, so the
// key stack never got longer than used either.
func (s *jsonStack) keep(vals []Value, keys []PropertyKey, used int) {
	if cap(vals) > jsonStackMax {
		vals = nil
	} else {
		clear(vals[:max(len(vals), used)])
	}
	if cap(keys) > jsonStackMax {
		keys = nil
	} else {
		clear(keys[:max(len(keys), min(used, cap(keys)))])
	}
	s.vals, s.keys = vals[:0], keys[:0]
}

// closed records the value stack's length as a container closes, leaving
// the container's entries where they are: keep clears the stacks once, up
// to the largest length. Clearing them at each container would cost a bulk
// write barrier per container while the collector marks.
// every runs every 4096 values: under a memory limit it charges what the
// value and key stacks grew by since it last looked, the capacity added to
// their high-water mark (memAccount.jsonVals, jsonKeys), so that what is
// charged is what the parse holds, a stack reused once a container closed
// charging nothing again, and a large array or object, whose entries wait
// on the stacks until it closes, stops within the limit's margin; and, but
// in a quiet parse, it checks for an interrupt, the limit's hit included.
// (Charging each growth's whole new array, as Go allocates it, made the
// 1.25x steps of a large stack count five times what it held.)
//
//go:noinline
func (p *jsonParser) every() error {
	if m := p.r.mem(); m != nil {
		if c := cap(p.stack); c > m.jsonVals {
			p.r.chargeSlow((c-m.jsonVals)*valueSize, memBytes)
			m.jsonVals = c
		}
		if c := cap(p.keys); c > m.jsonKeys {
			p.r.chargeSlow((c-m.jsonKeys)*int(unsafe.Sizeof(PropertyKey{})), memBytes)
			m.jsonKeys = c
		}
	}
	if p.quiet {
		return nil
	}
	return p.r.CheckInterrupt()
}

func (p *jsonParser) closed() {
	if n := len(p.stack); n > int(p.used) {
		p.used = uint16(min(n, math.MaxUint16))
	}
}

// wtf8FromUTF16 encodes code units as UTF-8, with lone surrogates encoded as
// generalized (WTF-8) three-byte sequences so they round-trip.
func wtf8FromUTF16(u []uint16) string {
	b := make([]byte, 0, len(u)+len(u)/2)
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c < 0x80:
			b = append(b, byte(c))
		case c >= 0xD800 && c < 0xE000:
			if c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000 {
				b = utf8.AppendRune(b, utf16.DecodeRune(rune(c), rune(u[i+1])))
				i++
				continue
			}
			b = append(b, 0xE0|byte(c>>12), 0x80|byte(c>>6)&0x3F, 0x80|byte(c)&0x3F)
		default:
			b = utf8.AppendRune(b, rune(c))
		}
	}
	return bytesToString(b)
}

func (p *jsonParser) skipWS() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) unexpected() error {
	if p.pos >= len(p.src) {
		return p.r.SyntaxError("Unexpected end of JSON input")
	}
	tok := string(p.src[p.pos])
	if p.src[p.pos] >= 0x80 {
		rn, _ := utf8.DecodeRuneInString(p.src[p.pos:])
		tok = string(rn)
	}
	return p.r.SyntaxError("Unexpected token %s in JSON at position %d", tok, p.pos)
}

func (p *jsonParser) value() (Value, error) {
	p.work++
	if p.work&4095 == 0 {
		if err := p.every(); err != nil {
			return Undefined(), err
		}
	}
	if p.pos >= len(p.src) {
		return Undefined(), p.unexpected()
	}
	switch c := p.src[p.pos]; c {
	case '{':
		return p.object()
	case '[':
		return p.array()
	case '"':
		s, err := p.str()
		if err != nil {
			return Undefined(), err
		}
		return StringValue(s), nil
	case 't':
		return p.literal("true", True())
	case 'f':
		return p.literal("false", False())
	case 'n':
		return p.literal("null", Null())
	default:
		if c == '-' || (c >= '0' && c <= '9') {
			return p.number()
		}
	}
	return Undefined(), p.unexpected()
}

func (p *jsonParser) literal(text string, v Value) (Value, error) {
	if len(p.src)-p.pos >= len(text) && p.src[p.pos:p.pos+len(text)] == text {
		p.pos += len(text)
		return v, nil
	}
	// Report the first mismatching character like V8.
	for i := range len(text) {
		if p.pos+i >= len(p.src) || p.src[p.pos+i] != text[i] {
			p.pos += i
			break
		}
	}
	return Undefined(), p.unexpected()
}

func (p *jsonParser) number() (Value, error) {
	start := p.pos
	neg := p.src[p.pos] == '-'
	if neg {
		p.pos++
	}
	if p.pos >= len(p.src) {
		return Undefined(), p.unexpected()
	}
	switch c := p.src[p.pos]; {
	case c == '0':
		p.pos++
	case c >= '1' && c <= '9':
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
	default:
		return Undefined(), p.unexpected()
	}
	isInt := true
	if p.pos < len(p.src) && p.src[p.pos] == '.' {
		isInt = false
		p.pos++
		if err := p.requireDigits(); err != nil {
			return Undefined(), err
		}
	}
	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		isInt = false
		p.pos++
		if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		if err := p.requireDigits(); err != nil {
			return Undefined(), err
		}
	}
	text := p.src[start:p.pos]
	if isInt && len(text) <= 16 {
		digits := text
		if neg {
			digits = text[1:]
		}
		var v int64
		for i := range len(digits) {
			v = v*10 + int64(digits[i]-'0')
		}
		if neg {
			if v == 0 {
				return NumberValue(negativeZero), nil
			}
			v = -v
		}
		return Int64Value(v), nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// Only ErrRange is possible after validation: ±Inf or 0 is correct.
		return NumberValue(f), nil
	}
	return NumberValue(f), nil
}

func (p *jsonParser) requireDigits() error {
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == start {
		return p.unexpected()
	}
	return nil
}

// str parses a string literal (p.pos at the opening quote). jsonScan stops
// at bytes >= 0x80 until it meets the first.
func (p *jsonParser) str() (*String, error) {
	start := p.pos + 1
	src := p.src
	high := uint64(swarMSB)
	for i := jsonScan(src, start, high); i < len(src); i = jsonScan(src, i+1, high) {
		switch c := src[i]; {
		case c == '"':
			p.pos = i + 1
			if high != 0 {
				return p.plain(src[start:i]), nil
			}
			return p.unescapedString(src[start:i]), nil
		case c == '\\':
			return p.strSlow(start, i)
		case c < 0x20:
			p.pos = i
			return nil, p.r.SyntaxError("Bad control character in string literal in JSON at position %d", i)
		}
		high = 0 // not ASCII
	}
	p.pos = len(src)
	return nil, p.r.SyntaxError("Unterminated string in JSON at position %d", len(src))
}

// strSlow checks a string literal from the first escape at i without decoding
// it. JSON syntax errors are reported during parsing, even if no one reads
// the resulting string; escapedString retains the contents as a lazy string.
func (p *jsonParser) strSlow(start, i int) (*String, error) {
	src := p.src
	// Raw copying requires valid UTF-8 and stringify's exact escape spelling.
	// Slash and Unicode escapes disable copying: stringify may spell them differently.
	canCopyJSON := p.knownUTF8
	// Count what the escapes save while syntax is checked, so that
	// escapedString counts the units without scanning the escapes again.
	saved, wide := 0, false
	for i < len(src) {
		switch c := src[i]; {
		case c == '"':
			p.pos = i + 1
			return p.escapedString(src[start:i], canCopyJSON, saved, wide), nil
		case c < 0x20:
			p.pos = i
			return nil, p.r.SyntaxError("Bad control character in string literal in JSON at position %d", i)
		case c == '\\':
			i++
			if i >= len(src) {
				p.pos = i
				return nil, p.unexpected()
			}
			switch src[i] {
			case '"', '\\', 'b', 'f', 'n', 'r', 't':
				saved++
				i++
			case '/':
				saved++
				canCopyJSON = false
				i++
			case 'u':
				saved += 5
				canCopyJSON = false
				if i+4 >= len(src) {
					p.pos = len(src)
					return nil, p.unexpected()
				}
				for k := 1; k <= 4; k++ {
					d := digitValue(src[i+k])
					if d < 0 || d >= 16 {
						p.pos = i + k
						return nil, p.r.SyntaxError("Bad Unicode escape in JSON at position %d", p.pos)
					}
				}
				wide = wide || wideEscape(src, i+1)
				i += 5
			default:
				p.pos = i
				return nil, p.r.SyntaxError("Bad escaped character in JSON at position %d", i)
			}
		default:
			i++
		}
		i = jsonScan(src, i, 0)
	}
	p.pos = len(src)
	return nil, p.r.SyntaxError("Unterminated string in JSON at position %d", len(src))
}

// shortLazy finds the String this parse made for the short literal text
// in mode: equal raw contents decode alike, so the two share one String and
// its decoding. slot is where a new String for text goes, -1 when text is
// too long to keep (jsonShortMax).
func (p *jsonParser) shortLazy(text string, mode lazyStringMode) (s *String, slot int) {
	if len(text) > jsonShortMax {
		return nil, -1
	}
	slot = strSlot(text)
	if s = p.short[slot]; s != nil && s.isLazy() && s.lazyMode() == mode && s.s == text {
		return s, slot
	}
	return nil, slot
}

// unescapedString is the lazy WTF-8 String of a literal with no escapes and
// a byte >= 0x80, a short repeat's if there is one (shortLazy). Under
// knownUTF8 it records the source as valid UTF-8 without checking it again.
func (p *jsonParser) unescapedString(text string) *String {
	s, slot := p.shortLazy(text, lazyWTF8)
	if s != nil {
		return s
	}
	s = p.r.lazyStringSized(text, lazyWTF8, wtf8Units(text), true)
	if p.knownUTF8 && s.isLazy() {
		s.lazyData().facts.Store(factUTF8Valid)
	}
	if slot >= 0 {
		p.short[slot] = s
	}
	return s
}

// escapedString is the lazy String of the escaped contents text, a short
// repeat's if there is one (shortLazy); saved and wide are what strSlow
// counted of the escapes.
func (p *jsonParser) escapedString(text string, canCopyJSON bool, saved int, wide bool) *String {
	mode := lazyJSONContents
	if canCopyJSON {
		mode = lazyJSONVerbatim
	}
	s, slot := p.shortLazy(text, mode)
	if s != nil {
		return s
	}
	n, wide := jsonDecodedUnits(text, saved, wide)
	s = p.r.lazyStringSized(text, mode, n, wide)
	if slot >= 0 {
		p.short[slot] = s
	}
	return s
}

// decodeJSONContents returns the String the contents src of a lazy string
// decode to, its n units, UTF-16 if wide. strSlow has already checked the
// JSON syntax; src excludes the quotes and may contain WTF-8. This step
// decodes content, including escapes and lone UTF-16 surrogates.
func decodeJSONContents(src string, n int, wide bool) *String {
	// Syntax is already checked, and n and wide, the decoded units and
	// storage kind (jsonDecodedUnits), were counted when the string was
	// made: the storage is allocated once, with no builder growth,
	// upgrade or scratch copy.
	var coallocated *String
	var builder StringBuilder
	sb := &builder
	if wide {
		if n <= utf16BufMax {
			coallocated, sb.u = newUTF16Buf(n)
			sb.u = sb.u[:0]
		} else {
			sb.u = make([]uint16, 0, n)
		}
	} else {
		sb.Grow(n)
	}
	i := jsonNextEscape(src, 0)
	run := 0
	for i < len(src) {
		writeJSONRun(sb, src[run:i])
		i++ // the backslash
		switch src[i] {
		case '"', '\\', '/':
			sb.WriteASCII(src[i])
		case 'b':
			sb.WriteASCII('\b')
		case 'f':
			sb.WriteASCII('\f')
		case 'n':
			sb.WriteASCII('\n')
		case 'r':
			sb.WriteASCII('\r')
		case 't':
			sb.WriteASCII('\t')
		case 'u':
			var v uint16
			for k := 1; k <= 4; k++ {
				v = v<<4 | uint16(digitValue(src[i+k]))
			}
			sb.WriteUnit(v)
			i += 4
		}
		run = i + 1
		i = jsonNextEscape(src, run)
	}
	writeJSONRun(sb, src[run:])
	if coallocated != nil {
		return coallocated
	}
	return sb.String()
}

// jsonNextEscape scans checked contents, where only a backslash needs
// decoding. Quotes and controls were checked by the parser, so the byte
// search can use the standard library's vectorized path.
func jsonNextEscape(src string, start int) int {
	if off := strings.IndexByte(src[start:], '\\'); off >= 0 {
		return start + off
	}
	return len(src)
}

// jsonDecodedUnits counts the code units syntax-checked JSON contents
// decode to, and reports whether one is not ASCII. saved and wide are what
// strSlow counted of the escapes, the bytes they save and whether one is
// wide, which spares scanning them again. A Unicode escape is one unit, a
// lone surrogate included.
func jsonDecodedUnits(src string, saved int, wide bool) (int, bool) {
	n := len(src)
	if !isASCIILong(src) {
		n, wide = wtf8Units(src), true
	}
	return n - saved, wide
}

// wideEscape reports whether the checked \u escape whose hex digits start
// at h decodes to a unit >= 0x80, which ASCII storage cannot hold.
func wideEscape(src string, h int) bool {
	return src[h] != '0' || src[h+1] != '0' || hexDigits[src[h+2]] >= 8
}

// writeJSONRun writes directly into the final, pre-sized representation.
// The contents come from a parse's well-formed source (lazyJSONContents).
func writeJSONRun(sb *StringBuilder, text string) {
	if sb.u != nil {
		sb.u = appendWellFormed(sb.u, text)
	} else {
		sb.writeASCIIBytes(text)
	}
}

// hexDigits maps a hex digit to its value and other bytes to 0xFF.
var hexDigits = func() (t [256]byte) {
	for c := range t {
		t[c] = byte(digitValue(byte(c)))
		if t[c] >= 16 {
			t[c] = 0xFF
		}
	}
	return
}()

// appendWTF8Units decodes a WTF-8 byte run into UTF-16 code units.
func appendWTF8Units(u []uint16, s string) []uint16 {
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			u = append(u, uint16(c))
			i++
			continue
		}
		if c == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			u = append(u, 0xD000|uint16(s[i+1]&0x3F)<<6|uint16(s[i+2]&0x3F))
			i += 3
			continue
		}
		rn, w := utf8.DecodeRuneInString(s[i:])
		i += w
		if rn >= 0x10000 {
			hi, lo := utf16.EncodeRune(rn)
			u = append(u, uint16(hi), uint16(lo))
			continue
		}
		u = append(u, uint16(rn))
	}
	return u
}

// key parses an object key into a PropertyKey without allocating for
// plain ASCII names that are already interned. s is the shape of the
// object's keys before it, or nil: a plain ASCII name that s has a child for
// (Shape.childNamed) comes back with that child, whose key is the name's
// atom, without an intern lookup.
func (p *jsonParser) key(s *Shape) (PropertyKey, *Shape, error) {
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return PropertyKey{}, nil, p.unexpected()
	}
	start := p.pos + 1
	i := start
	src := p.src
	for i < len(src) {
		c := src[i]
		if c == '"' {
			p.pos = i + 1
			if s != nil {
				if child := s.childNamed(src[start:i]); child != nil {
					return child.key, child, nil
				}
			}
			return p.r.KeyFromGoString(src[start:i]), nil, nil
		}
		if c == '\\' || c < 0x20 || c >= 0x80 {
			break
		}
		i++
	}
	name, err := p.str()
	if err != nil {
		return PropertyKey{}, nil, err
	}
	return p.r.KeyFromString(name), nil, nil
}

// jsonKeyScanLimit is the object size above which JSON.parse indexes keys
// for duplicate detection instead of scanning them.
const jsonKeyScanLimit = 64

func (p *jsonParser) enter() error {
	p.depth++
	if p.depth > jsonMaxDepth {
		return p.r.jsonTooDeep()
	}
	return nil
}

func (p *jsonParser) object() (Value, error) {
	if err := p.enter(); err != nil {
		return Undefined(), err
	}
	p.pos++ // '{'
	p.skipWS()
	r := p.r
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		p.depth--
		return ObjectValue(r.NewObject()), nil
	}
	base, kbase := len(p.stack), len(p.keys)
	// shape is the shape of the object's first n keys: while every key is a
	// transition the shape tree already has (key, Shape.child), it follows
	// them, and the object is built from it. Such a key is not among the
	// earlier ones (a shape's keys are distinct: addProperty does not check
	// it, but each of its callers looks the key up first, as addProp,
	// literals through has, host shapes built from map keys and localize's
	// replay do), so it skips the duplicate check. The first key that is
	// not one leaves shape where it is, and it and the keys after it take
	// the duplicate check and get their transitions from buildJSONObject.
	shape, n := r.plainRoot, 0
	// Duplicate keys (last wins, first position kept) are found by scanning
	// the object's keys while it is small and through an index once it has
	// more than jsonKeyScanLimit, so a wide object stays linear.
	var index map[PropertyKey]int
	// The first key is parsed here and the others at the comma before
	// them: an object's first key is looked up at the root, whose children
	// are found differently from the others' (Shape.childNamed), and a call
	// site of its own keeps their outcomes apart for the branch predictor.
	s := shape
	p.skipWS()
	k, child, err := p.key(s)
	for {
		if err != nil {
			return Undefined(), err
		}
		p.skipWS()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return Undefined(), p.unexpected()
		}
		p.pos++
		p.skipWS()
		var v Value
		if v, err = p.value(); err != nil { // err, not a new one: the next key's comes back in it
			return Undefined(), err
		}
		if child == nil && s != nil && !k.IsIndex() {
			child = s.child(k, attrDefault)
		}
		seg := p.keys[kbase:]
		dup := -1
		if child != nil {
			shape, n = child, n+1
		} else if index != nil {
			if i, ok := index[k]; ok {
				dup = i
			}
		} else {
			for i := range seg {
				if seg[i] == k {
					dup = i
					break
				}
			}
		}
		if dup >= 0 {
			p.stack[base+dup] = v
		} else {
			if index != nil {
				index[k] = len(seg)
			} else if len(seg) == jsonKeyScanLimit {
				index = make(map[PropertyKey]int, 2*jsonKeyScanLimit)
				for i, key := range seg {
					index[key] = i
				}
				index[k] = len(seg)
			}
			p.keys = append(p.keys, k)
			p.stack = append(p.stack, v)
		}
		p.skipWS()
		if p.pos >= len(p.src) {
			return Undefined(), p.unexpected()
		}
		if c := p.src[p.pos]; c == ',' {
			p.pos++
			p.skipWS()
			s = nil
			if n == len(p.keys)-kbase && n < maxShapeProps {
				s = shape
			}
			k, child, err = p.key(s)
			continue
		} else if c == '}' {
			p.pos++
			break
		}
		return Undefined(), p.unexpected()
	}
	keys, vals := p.keys[kbase:], p.stack[base:]
	o := r.buildJSONObject(keys, vals, shape, n)
	p.closed()
	p.stack = p.stack[:base]
	p.keys = p.keys[:kbase]
	p.depth--
	return ObjectValue(o), nil
}

// buildJSONObject creates an object with the given own properties in order,
// walking the realm's shape tree so equal key sequences share a Shape. shape
// is the shape of keys[:n], which are strings: r.plainRoot for n == 0.
func (r *Realm) buildJSONObject(keys []PropertyKey, vals []Value, shape *Shape, n int) *Object {
	hasIndex := false
	for _, k := range keys[n:] {
		if k.IsIndex() {
			hasIndex = true
			break
		}
	}
	if hasIndex || len(keys) > maxShapeProps {
		o := r.NewObject()
		for i, k := range keys {
			o.addProp(r, k, propCell{value: vals[i], attrs: attrDefault})
		}
		return o
	}
	for _, k := range keys[n:] {
		shape = shape.addProperty(r, k, attrDefault)
	}
	var o *Object
	var slots []Value
	switch len(vals) {
	case 1:
		x := &object1{}
		o, slots = &x.Object, x.buf[:]
	case 2:
		x := &object2{}
		o, slots = &x.Object, x.buf[:]
	case 3:
		x := &object3{}
		o, slots = &x.Object, x.buf[:]
	case 4:
		x := &object4{}
		o, slots = &x.Object, x.buf[:]
	case 5:
		x := &object5{}
		o, slots = &x.Object, x.buf[:]
	case 6:
		x := &object6{}
		o, slots = &x.Object, x.buf[:]
	case 7:
		x := &object7{}
		o, slots = &x.Object, x.buf[:]
	case 8:
		x := &object8{}
		o, slots = &x.Object, x.buf[:]
	default:
		o, slots = new(Object), make([]Value, len(vals))
	}
	copy(slots, vals)
	o.slots = slots
	r.chargeObject(unsafe.Sizeof(Object{}) + uintptr(len(slots)*valueSize))
	return initObject(o, ClassObject, shape)
}

// A parsed object of up to eight entries is one allocation of the bytes
// its two used to be: the even sizes are the literals' buckets (object.go),
// the odd ones these. A parsed array is one too (newArrayCopy).
type (
	object1 struct {
		Object
		buf [1]Value
	}
	object3 struct {
		Object
		buf [3]Value
	}
	object5 struct {
		Object
		buf [5]Value
	}
	object7 struct {
		Object
		buf [7]Value
	}
)

func (p *jsonParser) array() (Value, error) {
	if err := p.enter(); err != nil {
		return Undefined(), err
	}
	p.pos++ // '['
	p.skipWS()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		p.depth--
		return ObjectValue(p.r.NewArrayFromSlice(nil)), nil
	}
	base := len(p.stack)
	for {
		p.skipWS()
		v, err := p.value()
		if err != nil {
			return Undefined(), err
		}
		p.stack = append(p.stack, v)
		p.skipWS()
		if p.pos >= len(p.src) {
			return Undefined(), p.unexpected()
		}
		if c := p.src[p.pos]; c == ',' {
			p.pos++
			continue
		} else if c == ']' {
			p.pos++
			break
		}
		return Undefined(), p.unexpected()
	}
	a := p.r.newArrayCopy(p.stack[base:])
	p.closed()
	p.stack = p.stack[:base]
	p.depth--
	return ObjectValue(a), nil
}

// --- stringify --------------------------------------------------------------------------------

// JSONStringify serializes v with no replacer and no indentation
// (Go-callable entry point). The result is nil when v is not serializable
// (undefined, functions, symbols). Nesting deeper than MaxToGoDepth arrays
// and objects is a RangeError, as is running out of call depth: the walk's
// root and each 32 levels of it take walk units (jsonWalkUnits), since a
// toJSON method can start another stringify.
func (r *Realm) JSONStringify(v Value) (*String, error) {
	js := jsonStringifier{r: r}
	js.sb.Grow(max(256, int(r.jsonSizeHint)+int(r.jsonSizeHint)/8))
	if ok, err := js.serialize(v); !ok {
		return nil, err
	}
	r.jsonSizeHint = int32(js.sb.Len())
	return r.builtString(&js.sb), nil
}

func jsonStringify(r *Realm, this Value, args []Value) (Value, error) {
	value, replacer, space := Arg(args, 0), Arg(args, 1), Arg(args, 2)
	js := jsonStringifier{r: r}
	if IsCallable(replacer) {
		js.replacerFn = replacer
	} else if isArr, err := r.isArray(replacer); err != nil {
		return Undefined(), err
	} else if isArr {
		if err := js.setPropertyList(replacer.AsObject()); err != nil {
			return Undefined(), err
		}
	}
	if space.IsObject() {
		switch space.AsObject().class {
		case ClassNumber:
			f, err := r.ToNumber(space)
			if err != nil {
				return Undefined(), err
			}
			space = NumberValue(f)
		case ClassString:
			s, err := r.ToString(space)
			if err != nil {
				return Undefined(), err
			}
			space = StringValue(s)
		}
	}
	switch {
	case space.IsNumber():
		n := min(10, ToIntegerOrInfinityFloat(space.AsNumber()))
		if n >= 1 {
			js.gap = asciiString("          "[:int(n)])
		}
	case space.IsString():
		if s := space.AsString(); s.Len() > 0 {
			js.gap = s.Substring(0, min(10, s.Len()))
		}
	}
	// A string's output is sized by its quoting (quoteString), and leaves
	// the size hint to the outputs it is for: a large text's would make
	// the next small call's buffer as large.
	str := value.IsString() && !js.replacerFn.IsObject()
	if str {
		if q, ok, err := r.quoteString(&js.sb, value.AsString()); err != nil {
			return Undefined(), err
		} else if ok {
			return StringValue(q), nil
		}
	} else {
		js.sb.Grow(max(256, int(r.jsonSizeHint)+int(r.jsonSizeHint)/8))
	}
	var root *Object
	if js.replacerFn.IsObject() {
		root = r.NewObject()
		root.DefineOwnDataFast(r, StringKey(AtomEmpty), value, attrDefault)
	}
	rv, err := js.resolve(value, StringKey(AtomEmpty), root)
	if err != nil {
		return Undefined(), err
	}
	depth := r.callDepth
	if err := r.enterJSONUnits(jsonRootUnits); err != nil {
		return Undefined(), err
	}
	ok, err := js.write(rv)
	r.callDepth = depth // the walk's root units, and the levels an error leaves entered (push)
	if err != nil {
		return Undefined(), err
	}
	if !ok {
		return Undefined(), nil
	}
	if err := js.sb.checkLength(r); err != nil {
		return Undefined(), err
	}
	if !str {
		r.jsonSizeHint = int32(js.sb.Len())
	}
	return StringValue(r.builtString(&js.sb)), nil
}

type jsonStringifier struct {
	r            *Realm
	sb           StringBuilder
	replacerFn   Value // callable, or the zero Value
	propertyList []PropertyKey
	hasList      bool
	raw          bool    // AppendJSON: sb.b holds UTF-8 (quoteUTF8) and never upgrades
	inDst        bool    // AppendJSON: sb.b is still dst's spare capacity (own)
	strict       bool    // AppendJSONWith: a value written otherwise or dropped is an error (JSONOptions.Strict)
	plainEpoch   uint32  // protosLackToJSON's answer: protoEpoch+1 when positive
	gap          *String // nil for no indentation
	// depth is the number of containers being serialized, the stack push
	// and pop keep: its first len(shallow) entries are in shallow, the
	// rest in deeper. (A slice over an array of the stringifier would
	// point it at itself and move it to the heap, one allocation per
	// stringify.)
	depth   int
	shallow [16]*Object
	deeper  []*Object
	deep    map[*Object]struct{} // the stack past jsonScanDepth, for the cycle check
	work    int
}

// setPropertyList implements the replacer-array branch of JSON.stringify.
func (js *jsonStringifier) setPropertyList(arr *Object) error {
	r := js.r
	js.hasList = true
	n, err := r.LengthOfArrayLike(arr)
	if err != nil {
		return err
	}
	var set map[PropertyKey]struct{} // the keys of a long list
	for i := int64(0); i < n; i++ {
		v, err := getIndex(r, arr, i)
		if err != nil {
			return err
		}
		// A proxy's length can make the list as long as it likes.
		if err := interruptEvery(r, i); err != nil {
			return err
		}
		var item *String
		switch {
		case v.IsString():
			item = v.AsString()
		case v.IsNumber():
			item = NumberToString(v.AsNumber())
		case v.IsObject() && (v.AsObject().class == ClassString || v.AsObject().class == ClassNumber):
			if item, err = r.ToString(v); err != nil {
				return err
			}
		default:
			continue
		}
		set = js.addListKey(set, r.KeyFromString(item))
	}
	return nil
}

// listSetMin is the property list length from which addListKey keeps a set
// of the keys.
const listSetMin = 32

// addListKey appends k to the property list unless it is there already. A
// short list is scanned; a longer one keeps set, which addListKey creates
// and returns, so that a long replacer array costs linear time.
func (js *jsonStringifier) addListKey(set map[PropertyKey]struct{}, k PropertyKey) map[PropertyKey]struct{} {
	if set != nil {
		if _, ok := set[k]; !ok {
			set[k] = struct{}{}
			js.propertyList = append(js.propertyList, k)
		}
		return set
	}
	for _, e := range js.propertyList {
		if e == k {
			return nil
		}
	}
	js.propertyList = append(js.propertyList, k)
	if len(js.propertyList) == listSetMin {
		set = make(map[PropertyKey]struct{}, 2*listSetMin)
		for _, e := range js.propertyList {
			set[e] = struct{}{}
		}
	}
	return set
}

// resolve performs the value-transforming steps of SerializeJSONProperty:
// toJSON, the replacer function and unwrapping of boxed primitives.
func (js *jsonStringifier) resolve(v Value, key PropertyKey, holder *Object) (Value, error) {
	r := js.r
	if v.IsObject() && r.lacksToJSON(v.AsObject(), &js.plainEpoch) {
		// No toJSON to call.
	} else if v.IsObject() || v.IsBigInt() {
		js.own() // the lookup (a getter, a proxy's trap) and toJSON run code
		toJSON, err := r.GetV(v, StringKey(AtomToJSON))
		if err != nil {
			return Undefined(), err
		}
		if IsCallable(toJSON) {
			if v, err = r.Call(toJSON, v, []Value{StringValue(key.ToJSString(r))}); err != nil {
				return Undefined(), err
			}
		}
	}
	if js.replacerFn.IsObject() {
		var err error
		if v, err = r.Call(js.replacerFn, ObjectValue(holder), []Value{StringValue(key.ToJSString(r)), v}); err != nil {
			return Undefined(), err
		}
	}
	if v.IsObject() {
		o := v.AsObject()
		switch o.class {
		case ClassNumber:
			f, err := r.ToNumber(v)
			if err != nil {
				return Undefined(), err
			}
			return NumberValue(f), nil
		case ClassString:
			s, err := r.ToString(v)
			if err != nil {
				return Undefined(), err
			}
			return StringValue(s), nil
		case ClassBoolean, ClassBigInt:
			pv, _ := o.PrimitiveValue()
			return pv, nil
		}
	}
	return v, nil
}

// write serializes a resolved value; ok is false when the value has no JSON
// representation (undefined, function, symbol).
func (js *jsonStringifier) write(v Value) (bool, error) {
	if err := js.tick(); err != nil {
		return false, err
	}
	switch v.Type() {
	case TypeNull:
		js.sb.writeASCIIBytes("null")
	case TypeBoolean:
		if v.AsBool() {
			js.sb.writeASCIIBytes("true")
		} else {
			js.sb.writeASCIIBytes("false")
		}
	case TypeString:
		if err := js.quote(v.AsString()); err != nil {
			return false, err
		}
	case TypeNumber:
		if f := v.AsNumber(); f != f || math.IsInf(f, 0) {
			if js.strict {
				return false, newJSONLoss(v)
			}
			js.sb.writeASCIIBytes("null")
		} else {
			js.finite(f)
		}
	case TypeBigInt:
		return false, js.r.TypeError("Do not know how to serialize a BigInt")
	case TypeObject:
		o := v.AsObject()
		if o.class != ClassObject {
			if o.class == ClassFunction {
				return false, nil
			}
			if o.class == ClassArray {
				return true, js.array(o)
			}
			if o.class == ClassProxy {
				return js.proxy(o)
			}
			if js.strict {
				if e := jsonOpaque(o); e != nil {
					return false, e
				}
			}
		}
		return true, js.object(o)
	default:
		return false, nil
	}
	// One primitive was appended (a quoted string is the unbounded case).
	return true, js.sb.checkLength(js.r)
}

// tick counts one unit of work, a value written or a property visited and
// skipped, and checks the length and the interrupt every 4096.
func (js *jsonStringifier) tick() error {
	js.work++
	if js.work&4095 == 0 {
		return js.checkpoint()
	}
	return nil
}

// checkpoint checks the interrupt and the length of the output.
func (js *jsonStringifier) checkpoint() error {
	if err := js.sb.checkLength(js.r); err != nil {
		return err
	}
	return js.r.CheckInterrupt()
}

func (js *jsonStringifier) number(f float64) {
	if f != f || math.IsInf(f, 0) {
		js.sb.writeASCIIBytes("null")
		return
	}
	js.finite(f)
}

// finite writes the finite number f.
func (js *jsonStringifier) finite(f float64) {
	var buf [32]byte
	writeASCIIBytes(&js.sb, AppendNumber(buf[:0], f))
}

// writeASCIIBytes appends ASCII bytes to sb without an intermediate string.
func writeASCIIBytes(sb *StringBuilder, b []byte) {
	// bytesToString does not retain b: writeASCIIBytes copies before
	// returning and the caller keeps ownership of the buffer.
	sb.writeASCIIBytes(bytesToString(b))
}

const lowerHex = "0123456789abcdef"

// jsonQuote is the quotation mark quoteString puts around a string, the
// shared one-character string, whose hash is computed: a slice of the
// quoting returns it.
var jsonQuote = singleCharStrings['"']

// quoteString returns JSON.stringify of the string s without copying it,
// '"' + s + '"' as a rope, when s is long enough for a rope (ropeThreshold)
// and ASCII with no unit JSON quoting escapes. Otherwise ok is false and
// sb, empty, is grown to the length of the quoting (jsonQuotedLen), which
// then fills it without another buffer: an output of up to smallASCIIMax
// bytes stays in the builder's own bytes and String makes it one
// allocation, a longer one takes its bytes or units and the header.
//
// A slice of the rope that is s, as a serializer escaping a text into a
// script takes (JSON.stringify(s).slice(1, -1)), is s itself
// (ropeSubstring). A lazy s known plain (plainLazyJSON) is neither scanned
// again nor decoded: the rope holds a view of its source (plainView).
func (r *Realm) quoteString(sb *StringBuilder, s *String) (q *String, ok bool, err error) {
	f := s
	var (
		n     int64
		plain bool
	)
	if s.isLazy() && s.plainLazyJSON() {
		s = s.plainView()
		n, plain = int64(s.n)+2, true
	} else {
		if s.needsFlat() {
			f = s.flat(new(String))
		}
		n, plain = jsonQuotedLen(f)
	}
	switch {
	case plain && s.n >= ropeThreshold:
		if q, err = r.Concat(jsonQuote, s); err == nil {
			q, err = r.Concat(q, jsonQuote)
		}
		return q, true, err
	case n > int64(maxStringLength):
		return nil, false, r.invalidStringLength()
	case f.kind == strUTF16:
		sb.growUnits(int(n))
	case n > smallASCIIMax:
		sb.Grow(int(n))
	}
	return nil, false, nil
}

// quote implements QuoteJSONString (well-formed: lone surrogates as \uXXXX);
// the interrupt flag is checked every interruptStride units of a long string.
func (js *jsonStringifier) quote(s *String) error {
	if s.isLazy() {
		return js.quoteLazy(s)
	}
	if s.kind == strRope {
		return js.quoteRope(s)
	}
	js.sb.WriteASCII('"')
	var err error
	switch {
	case s.json == jsonPlain:
		writeASCIIString(&js.sb, s.s)
	case s.kind == strASCII:
		err = js.quoteASCII(s.s)
	case js.raw:
		err = js.quoteUTF8(s.units())
	default:
		err = js.quoteUTF16(s.units())
	}
	if err != nil {
		return err
	}
	js.sb.WriteASCII('"')
	return nil
}

// quoteLazy writes retained bytes when the output encoding permits it, and
// otherwise quotes the decoded content. Plain ASCII is copied in both
// output modes; AppendJSON's UTF-8 output also copies verbatim JSON and
// quotes valid UTF-8 text in place (quoteASCII), as quoteGo does.
func (js *jsonStringifier) quoteLazy(s *String) error {
	copyJSON := s.plainLazyJSON() || js.raw && s.lazyMode() == lazyJSONVerbatim
	if !copyJSON && !(js.raw && s.validLazyUTF8()) {
		return js.quote(s.resolved())
	}
	if copyJSON && len(s.s) > interruptStride {
		// The copy observes an interrupt where quoteASCII would, past its
		// first interruptStride bytes, and once, before writing: a short
		// value is written whatever is pending, as a call's small value is.
		if err := js.r.CheckInterrupt(); err != nil {
			return err
		}
	}
	js.sb.WriteASCII('"')
	if copyJSON {
		// Raw output is UTF-8: verbatim contents are copied as they are.
		js.sb.writeASCIIBytes(s.s)
	} else if err := js.quoteASCII(s.s); err != nil {
		return err
	}
	js.sb.WriteASCII('"')
	return nil
}

// quoteRope is quote of a rope, out of line so that the view (flat) does
// not grow quote's frame. A view knows nothing of its bytes (json 0).
//
//go:noinline
func (js *jsonStringifier) quoteRope(s *String) error {
	f := s.flat(new(String))
	js.sb.WriteASCII('"')
	var err error
	switch {
	case f.kind == strASCII:
		err = js.quoteASCII(f.s)
	case js.raw:
		err = js.quoteUTF8(f.units())
	default:
		err = js.quoteUTF16(f.units())
	}
	if err != nil {
		return err
	}
	js.sb.WriteASCII('"')
	return nil
}

// jsonScanDepth is the nesting up to which the cycle check scans the stack;
// the containers below it are kept in a set (jsonStringifier.deep).
const jsonScanDepth = 64

// push enters o, a container being serialized, or reports the cycle it
// closes. Past jsonScanDepth every jsonCallLevels-th level takes a walk unit
// (jsonMaxDepth): pop releases it, and on an error the entry point restores
// the depth. A value shallower than jsonScanDepth, the common case, pays
// one compare for the bookkeeping.
func (js *jsonStringifier) push(o *Object) error {
	n := js.depth
	for _, e := range js.shallow[:min(n, len(js.shallow))] {
		if e == o {
			return js.r.TypeError("Converting circular structure to JSON")
		}
	}
	if n > len(js.shallow) {
		for _, e := range js.deeper[:min(n, jsonScanDepth)-len(js.shallow)] {
			if e == o {
				return js.r.TypeError("Converting circular structure to JSON")
			}
		}
	}
	if n >= jsonScanDepth {
		if _, ok := js.deep[o]; ok {
			return js.r.TypeError("Converting circular structure to JSON")
		}
		if n >= jsonMaxDepth {
			return js.r.jsonTooDeep()
		}
		if n%jsonCallLevels == 0 {
			if err := js.r.enterJSONUnits(1); err != nil {
				return err
			}
		}
		if js.deep == nil {
			js.deep = make(map[*Object]struct{})
		}
		js.deep[o] = struct{}{}
	}
	if n < len(js.shallow) {
		js.shallow[n] = o
	} else {
		js.deeper = append(js.deeper, o)
	}
	js.depth = n + 1
	return nil
}

func (js *jsonStringifier) pop() {
	n := js.depth - 1
	if n >= len(js.shallow) {
		i := n - len(js.shallow)
		if n >= jsonScanDepth {
			delete(js.deep, js.deeper[i])
			if n%jsonCallLevels == 0 {
				js.r.exitJSONUnits(1)
			}
		}
		js.deeper = js.deeper[:i]
	}
	js.depth = n
}

// newline starts a line indented by level gaps.
func (js *jsonStringifier) newline(level int) {
	js.sb.WriteASCII('\n')
	for range level {
		js.sb.WriteString(js.gap)
	}
}

// object implements SerializeJSONObject.
func (js *jsonStringifier) object(o *Object) error {
	r := js.r
	if o.flags&flagHasLazy != 0 {
		if ok, err := js.hostNode(o); ok || err != nil {
			return err
		}
	}
	if err := js.push(o); err != nil {
		return err
	}
	level := js.depth // the members' indentation
	js.sb.WriteASCII('{')
	first := true
	if o.flags&flagHasLazy != 0 {
		o.materializeHost()
	}
	if !js.hasList && o.flags&(flagDict|flagHasLazy) == 0 && o.elemLen() == 0 && (o.dictTable() == nil || o.dictTable().sparse == nil) && o.class != ClassString && o.class != ClassProxy && o.class != ClassTypedArray {
		// Fast path: plain shape-mode object without indexed properties.
		// The shape's property list is the spec's key snapshot; slots are
		// read directly while the shape is unchanged and through [[Get]]
		// once user code (toJSON, replacer) has mutated the object.
		shape := o.shape
		props := shape.Props()
		for i, p := range props {
			if p.attrs&attrEnumerable == 0 || !p.key.IsString() {
				if err := js.tick(); err != nil {
					return err
				}
				continue
			}
			var v Value
			if o.shape == shape && p.attrs&attrAccessor == 0 {
				v = o.slots[i]
			} else {
				js.own() // a getter
				var err error
				if v, err = o.Get(r, p.key, ObjectValue(o)); err != nil {
					return err
				}
			}
			var err error
			if v, err = js.resolve(v, p.key, o); err != nil {
				return err
			}
			if v.IsUndefined() || (v.IsObject() && v.AsObject().IsCallable()) || v.IsSymbol() {
				if js.strict {
					return newJSONLoss(v).in(p.key)
				}
				if err := js.tick(); err != nil {
					return err
				}
				continue
			}
			if !first {
				js.sb.WriteASCII(',')
			}
			first = false
			if js.gap != nil {
				js.newline(level)
			}
			if err := js.quote(p.key.String()); err != nil {
				return err
			}
			js.sb.WriteASCII(':')
			if js.gap != nil {
				js.sb.WriteASCII(' ')
			}
			if _, err := js.write(v); err != nil {
				return jsonLossIn(err, p.key)
			}
		}
	} else {
		keys := js.propertyList
		if !js.hasList {
			var err error
			if keys, err = r.countedEnumerableOwnKeys(o, &js.work); err != nil {
				return err
			}
		}
		for _, k := range keys {
			var v Value
			if c, ok := o.getOwnCell(k); ok && c.attrs&attrAccessor == 0 {
				v = c.value
			} else {
				js.own() // a getter, a proxy's trap
				var err error
				if v, err = o.Get(r, k, ObjectValue(o)); err != nil {
					return err
				}
			}
			v, err := js.resolve(v, k, o)
			if err != nil {
				return err
			}
			if v.IsUndefined() || (v.IsObject() && v.AsObject().IsCallable()) || v.IsSymbol() {
				if js.strict {
					return newJSONLoss(v).in(k)
				}
				if err := js.tick(); err != nil {
					return err
				}
				continue
			}
			if !first {
				js.sb.WriteASCII(',')
			}
			first = false
			if js.gap != nil {
				js.newline(level)
			}
			if k.IsString() {
				err = js.quote(k.String())
			} else {
				err = js.quote(k.ToJSString(r))
			}
			if err != nil {
				return err
			}
			js.sb.WriteASCII(':')
			if js.gap != nil {
				js.sb.WriteASCII(' ')
			}
			if _, err := js.write(v); err != nil {
				return jsonLossIn(err, k)
			}
		}
	}
	if !first && js.gap != nil {
		js.newline(level - 1)
	}
	js.sb.WriteASCII('}')
	js.pop()
	return js.sb.checkLength(r)
}

// proxy serializes a proxy: nothing for a callable one, otherwise an array
// or an object as IsArray decides, through its traps. It is placed between
// object and array for layout: its 224 bytes, padded, make up for what
// resolve, object and array shrank by with own out of line, so write,
// quote, push, object, array and escapeByte keep their 64-byte phase.
func (js *jsonStringifier) proxy(o *Object) (bool, error) {
	if o.IsCallable() {
		return false, nil
	}
	isArr, err := js.r.isArray(ObjectValue(o))
	if err != nil {
		return false, err
	}
	if isArr {
		return true, js.array(o)
	}
	return true, js.object(o)
}

// array implements SerializeJSONArray.
func (js *jsonStringifier) array(o *Object) error {
	r := js.r
	if o.flags&flagHasLazy != 0 {
		if ok, err := js.hostNode(o); ok || err != nil {
			return err
		}
		o.materializeHost() // its elements, read below
	}
	if err := js.push(o); err != nil {
		return err
	}
	level := js.depth // the elements' indentation
	n, err := r.LengthOfArrayLike(o)
	if err != nil {
		return err
	}
	// An array's dense elements, read again at each index: a getter or a
	// toJSON may change them. A proxy has none.
	var el *elemStore
	if o.bits&bitInlineElems != 0 {
		el = o.inlineElems()
	}
	js.sb.WriteASCII('[')
	for i := range n {
		if i > 0 {
			js.sb.WriteASCII(',')
		}
		if js.gap != nil {
			js.newline(level)
		}
		var v Value
		if el != nil && i < int64(el.n) && !el.at(int(i)).IsHole() {
			v = *el.at(int(i))
		} else {
			js.own() // a getter, of the element or a prototype's, a proxy's trap
			if v, err = o.Get(r, IndexKey(uint32(i)), ObjectValue(o)); err != nil {
				return err
			}
		}
		v, err = js.resolve(v, IndexKey(uint32(i)), o)
		if err != nil {
			return err
		}
		ok, err := js.write(v)
		if err != nil {
			return jsonLossAt(err, i)
		}
		if !ok {
			if js.strict {
				return newJSONLoss(v).at(i)
			}
			js.sb.writeASCIIBytes("null")
			if err := js.sb.checkLength(r); err != nil { // the indentation
				return err
			}
		}
	}
	if n > 0 && js.gap != nil {
		js.newline(level - 1)
	}
	js.sb.WriteASCII(']')
	js.pop()
	// Repeated references to one object repeat its output: check it.
	return js.sb.checkLength(r)
}

// jsonEscape maps a byte that JSON quoting escapes with a letter to the
// letter, and other bytes to 0.
var jsonEscape = [256]byte{'"': '"', '\\': '\\', '\b': 'b', '\f': 'f', '\n': 'n', '\r': 'r', '\t': 't'}

// appendJSONEscape appends the escape of the byte c, which JSON quoting
// escapes, to b.
func appendJSONEscape(b []byte, c byte) []byte {
	if e := jsonEscape[c]; e != 0 {
		return append(b, '\\', e)
	}
	return append(b, '\\', 'u', '0', '0', lowerHex[c>>4], lowerHex[c&15])
}

// escapeByte writes the escape of the byte c. It is last in the file for
// layout (see realm_calldata.go): there it keeps push, object and array on
// dev's 64-byte phase, which root BenchmarkCall's AppendJSON shows (about
// 25 of its 1,550 cycles when they moved by 32).
func (js *jsonStringifier) escapeByte(c byte) {
	if e := jsonEscape[c]; e != 0 {
		js.sb.writeASCII2('\\', e)
		return
	}
	js.sb.writeASCIIBytes(`\u00`)
	js.sb.writeASCII2(lowerHex[c>>4], lowerHex[c&15])
}

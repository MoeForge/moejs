package engine

import (
	"encoding/binary"
	"hash/maphash"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unsafe"
)

// String builtins. Every method coerces `this` in spec order,
// works on all three string kinds and produces zero-copy slices for
// substrings of ASCII strings.

var stringCtorMethods = []builtinDef{
	{AtomFromCharCode, stringFromCharCode, 1},
	{AtomFromCodePoint, stringFromCodePoint, 1},
	{AtomRaw, stringRaw, 1},
}

var stringProtoMethods = []builtinDef{
	{AtomToString, stringProtoToString, 0},
	{AtomValueOf, stringProtoValueOf, 0},
	{AtomAnchor, stringProtoAnchor, 1},
	{AtomAt, stringProtoAt, 1},
	{AtomBig, stringProtoBig, 0},
	{AtomBlink, stringProtoBlink, 0},
	{AtomBold, stringProtoBold, 0},
	{AtomCharAt, stringProtoCharAt, 1},
	{AtomCharCodeAt, stringProtoCharCodeAt, 1},
	{AtomCodePointAt, stringProtoCodePointAt, 1},
	{AtomConcat, stringProtoConcat, 1},
	{AtomEndsWith, stringProtoEndsWith, 1},
	{AtomFixed, stringProtoFixed, 0},
	{AtomFontcolor, stringProtoFontcolor, 1},
	{AtomFontsize, stringProtoFontsize, 1},
	{AtomIncludes, stringProtoIncludes, 1},
	{AtomIndexOf, stringProtoIndexOf, 1},
	{AtomIsWellFormed, stringProtoIsWellFormed, 0},
	{AtomItalics, stringProtoItalics, 0},
	{AtomLastIndexOf, stringProtoLastIndexOf, 1},
	{AtomLink, stringProtoLink, 1},
	{AtomLocaleCompare, stringProtoLocaleCompare, 1},
	{AtomMatch, stringProtoMatch, 1},
	{AtomMatchAll, stringProtoMatchAll, 1},
	{AtomNormalize, stringProtoNormalize, 0},
	{AtomPadEnd, stringProtoPadEnd, 1},
	{AtomPadStart, stringProtoPadStart, 1},
	{AtomRepeat, stringProtoRepeat, 1},
	{AtomReplace, stringProtoReplace, 2},
	{AtomReplaceAll, stringProtoReplaceAll, 2},
	{AtomSearch, stringProtoSearch, 1},
	{AtomSlice, stringProtoSlice, 2},
	{AtomSmall, stringProtoSmall, 0},
	{AtomSplit, stringProtoSplit, 2},
	{AtomStartsWith, stringProtoStartsWith, 1},
	{AtomStrike, stringProtoStrike, 0},
	{AtomSub, stringProtoSub, 0},
	{AtomSubstr, stringProtoSubstr, 2},
	{AtomSubstring, stringProtoSubstring, 2},
	{AtomSup, stringProtoSup, 0},
	{AtomToLocaleLowerCase, stringProtoToLocaleLowerCase, 0},
	{AtomToLocaleUpperCase, stringProtoToLocaleUpperCase, 0},
	{AtomToLowerCase, stringProtoToLowerCase, 0},
	{AtomToUpperCase, stringProtoToUpperCase, 0},
	{AtomToWellFormed, stringProtoToWellFormed, 0},
	{AtomTrim, stringProtoTrim, 0},
	{AtomTrimEnd, stringProtoTrimEnd, 0},
	{AtomTrimStart, stringProtoTrimStart, 0},
}

func installString(r *Realm) {
	r.installBuiltins(r.StringCtor, stringCtorMethods)
	r.StringPrototype.ReserveSlots(r, len(stringProtoMethods)+stringAliasCount)
	r.installBuiltins(r.StringPrototype, stringProtoMethods)
	installStringAliases(r)
}

// thisStringPrimitive implements the spec's thisStringValue: the receiver is
// a string primitive or a String wrapper, else a TypeError naming method.
func thisStringPrimitive(r *Realm, this Value, method string) (Value, error) {
	if this.IsString() {
		return this, nil
	}
	if this.IsObject() && this.AsObject().class == ClassString {
		return StringValue(extDataOf(this.AsObject(), kindString)), nil
	}
	return Undefined(), r.TypeError("String.prototype.%s requires that 'this' be a String", method)
}

func stringProtoToString(r *Realm, this Value, args []Value) (Value, error) {
	return thisStringPrimitive(r, this, "toString")
}

func stringProtoValueOf(r *Realm, this Value, args []Value) (Value, error) {
	return thisStringPrimitive(r, this, "valueOf")
}

// singleCharStrings are the 128 one-character ASCII strings, shared by all
// realms so charAt/split("")/str[i] on ASCII input allocate nothing per
// character. Their hashes are computed here: Hash would otherwise store
// them from every goroutine that hashes one.
var singleCharStrings = func() [128]*String {
	var a [128]*String
	for i := range a {
		a[i] = asciiString(string(rune(i)))
		a[i].Hash()
	}
	return a
}()

// unitString is a one-unit UTF-16 string allocated with its unit.
type unitString struct {
	String
	u [1]uint16
}

// charString returns the one-unit string for c: a shared one for ASCII, one
// allocation otherwise.
func charString(c uint16) *String {
	if c < 0x80 {
		return singleCharStrings[c]
	}
	x := &unitString{u: [1]uint16{c}}
	x.String = String{p: unitsPtr(x.u[:]), n: 1, kind: strUTF16}
	return &x.String
}

// writeSlice appends s[start:end) to sb without materializing a *String.
func writeSlice(sb *StringBuilder, s *String, start, end int) {
	if start >= end {
		return
	}
	if s.needsFlat() {
		writeRopeSlice(sb, s, start, end)
		return
	}
	if s.kind == strASCII {
		writeASCIIString(sb, s.s[start:end])
		return
	}
	sb.upgrade()
	sb.u = append(sb.u, s.units()[start:end]...)
}

// writeRopeSlice is writeSlice of a rope or lazy string, out of line so
// that the view (flat) does not grow writeSlice's frame.
//
//go:noinline
func writeRopeSlice(sb *StringBuilder, s *String, start, end int) {
	f := s.flat(new(String))
	if f.kind == strASCII {
		writeASCIIString(sb, f.s[start:end])
		return
	}
	sb.upgrade()
	sb.u = append(sb.u, f.units()[start:end]...)
}

// writeASCIIString appends a Go string known to be ASCII.
func writeASCIIString(sb *StringBuilder, s string) { sb.writeASCIIBytes(s) }

// thisStringValue implements RequireObjectCoercible(this) + ToString(this).
func thisStringValue(r *Realm, this Value, method string) (*String, error) {
	if this.IsString() {
		return this.AsString(), nil
	}
	if this.IsNullish() {
		return nil, r.TypeError("String.prototype.%s called on null or undefined", method)
	}
	return r.ToString(this)
}

// argIntegerOrInfinity coerces args[i], returning def when it is undefined.
func argIntegerOrInfinity(r *Realm, args []Value, i int, def float64) (float64, error) {
	v := Arg(args, i)
	if v.IsUndefined() {
		return def, nil
	}
	if v.IsNumber() {
		return ToIntegerOrInfinityFloat(v.AsNumber()), nil
	}
	return r.ToIntegerOrInfinity(v)
}

// clampPos clamps an integer-or-infinity to [0, n].
func clampPos(f float64, n int) int {
	if f <= 0 {
		return 0
	}
	if f >= float64(n) {
		return n
	}
	return int(f)
}

// relativePos resolves a relative index (negative counts from the end) into
// [0, n].
func relativePos(f float64, n int) int {
	if f < 0 {
		if -f >= float64(n) {
			return 0
		}
		return n + int(f)
	}
	if f >= float64(n) {
		return n
	}
	return int(f)
}

// hasSubstringAt reports whether sub occurs in s at unit offset at.
func hasSubstringAt(s, sub *String, at int) bool {
	n := sub.Len()
	if at < 0 || at+n > s.Len() {
		return false
	}
	if s.needsFlat() || sub.needsFlat() {
		return ropeSubstringAt(s, sub, at)
	}
	return substringAt(s, sub, at)
}

// ropeSubstringAt is hasSubstringAt with a rope or lazy string, out of line
// so that the views (flat) do not grow hasSubstringAt's frame. A rope s is
// compared in its leaves (ropeHasAt), or else flattened.
//
//go:noinline
func ropeSubstringAt(s, sub *String, at int) bool {
	a, b := s, sub
	if sub.needsFlat() {
		b = sub.flat(new(String))
	}
	if s.needsFlat() {
		if s.kind == strRope {
			if match, ok := ropeHasAt(s, b, at); ok {
				return match
			}
		}
		a = s.flat(new(String))
	}
	return substringAt(a, b, at)
}

// substringAt is hasSubstringAt of flat strings, sub fitting at at.
func substringAt(s, sub *String, at int) bool {
	n := sub.Len()
	if s.kind == strASCII && sub.kind == strASCII {
		return s.s[at:at+n] == sub.s
	}
	for i := range n {
		if s.at(at+i) != sub.at(i) {
			return false
		}
	}
	return true
}

// lastIndexOfString returns the last index <= start at which sub occurs in s.
func lastIndexOfString(s, sub *String, start int) int {
	n, m := s.Len(), sub.Len()
	start = min(start, n-m)
	if start < 0 {
		return -1
	}
	a := s
	if s.needsFlat() {
		a = s.flat(new(String))
	}
	b := sub
	if sub.needsFlat() {
		b = sub.flat(new(String))
	}
	if a.kind == strASCII && b.kind == strASCII {
		return strings.LastIndex(a.s[:start+m], b.s)
	}
	if a.kind == strASCII {
		return -1 // a UTF-16 sub has a unit no ASCII string has
	}
	if m == 0 {
		return start
	}
	// The direct scan of flatIndexOf, backwards (string_walk.go).
	u, first := a.units(), b.at(0)
	hits := 0
outer:
	for i := start; i >= 0; i-- {
		if u[i] != first {
			continue
		}
		for j := 1; j < m; j++ {
			if u[i+j] != b.at(j) {
				hits += j
				if directScanSwitch(m, hits, start-i, i) {
					return searchFlatUnits(a, b, i-1, true)
				}
				continue outer
			}
		}
		return i
	}
	return -1
}

// --- String constructor methods ---------------------------------------------------------

func stringFromCharCode(r *Realm, this Value, args []Value) (Value, error) {
	if len(args) == 1 {
		u, err := r.ToUint32(args[0])
		if err != nil {
			return Undefined(), err
		}
		return StringValue(charString(uint16(u))), nil
	}
	units := make([]uint16, len(args))
	for i, a := range args {
		u, err := r.ToUint32(a)
		if err != nil {
			return Undefined(), err
		}
		units[i] = uint16(u)
	}
	r.chargeString(2 * len(units))
	return StringValue(FromUTF16(units)), nil
}

// --- positions and code units ---------------------------------------------------------------

func stringProtoAt(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "at")
	if err != nil {
		return Undefined(), err
	}
	f, err := argIntegerOrInfinity(r, args, 0, 0)
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	k := f
	if f < 0 {
		k = float64(n) + f
	}
	if k < 0 || k >= float64(n) {
		return Undefined(), nil
	}
	return StringValue(charString(s.At(int(k)))), nil
}

func stringProtoCharAt(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "charAt")
	if err != nil {
		return Undefined(), err
	}
	f, err := argIntegerOrInfinity(r, args, 0, 0)
	if err != nil {
		return Undefined(), err
	}
	if f < 0 || f >= float64(s.Len()) {
		return StringValue(emptyString), nil
	}
	return StringValue(charString(s.At(int(f)))), nil
}

func stringProtoCharCodeAt(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "charCodeAt")
	if err != nil {
		return Undefined(), err
	}
	f, err := argIntegerOrInfinity(r, args, 0, 0)
	if err != nil {
		return Undefined(), err
	}
	if f < 0 || f >= float64(s.Len()) {
		return NaN(), nil
	}
	return IntValue(int(s.At(int(f)))), nil
}

func stringProtoCodePointAt(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "codePointAt")
	if err != nil {
		return Undefined(), err
	}
	f, err := argIntegerOrInfinity(r, args, 0, 0)
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	if f < 0 || f >= float64(n) {
		return Undefined(), nil
	}
	i := int(f)
	c := s.At(i)
	if c >= 0xD800 && c < 0xDC00 && i+1 < n {
		if lo := s.At(i + 1); lo >= 0xDC00 && lo < 0xE000 {
			return IntValue(int(utf16.DecodeRune(rune(c), rune(lo)))), nil
		}
	}
	return IntValue(int(c)), nil
}

// --- searching -----------------------------------------------------------------------

func stringProtoIndexOf(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "indexOf")
	if err != nil {
		return Undefined(), err
	}
	search, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	pos, err := argIntegerOrInfinity(r, args, 1, 0)
	if err != nil {
		return Undefined(), err
	}
	return IntValue(s.IndexOf(search, clampPos(pos, s.Len()))), nil
}

func stringProtoLastIndexOf(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "lastIndexOf")
	if err != nil {
		return Undefined(), err
	}
	search, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	numPos, err := r.ToNumber(Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	pos := posInf
	if numPos == numPos {
		pos = ToIntegerOrInfinityFloat(numPos)
	}
	return IntValue(lastIndexOfString(s, search, clampPos(pos, s.Len()))), nil
}

// searchStringArg coerces the searchString argument of includes/startsWith/
// endsWith, rejecting RegExp objects.
func searchStringArg(r *Realm, v Value, method string) (*String, error) {
	isRx, err := r.isRegExp(v)
	if err != nil {
		return nil, err
	}
	if isRx {
		return nil, r.TypeError("First argument to String.prototype.%s must not be a regular expression", method)
	}
	return r.ToString(v)
}

func stringProtoIncludes(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "includes")
	if err != nil {
		return Undefined(), err
	}
	search, err := searchStringArg(r, Arg(args, 0), "includes")
	if err != nil {
		return Undefined(), err
	}
	pos, err := argIntegerOrInfinity(r, args, 1, 0)
	if err != nil {
		return Undefined(), err
	}
	return Bool(s.IndexOf(search, clampPos(pos, s.Len())) >= 0), nil
}

func stringProtoStartsWith(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "startsWith")
	if err != nil {
		return Undefined(), err
	}
	search, err := searchStringArg(r, Arg(args, 0), "startsWith")
	if err != nil {
		return Undefined(), err
	}
	pos, err := argIntegerOrInfinity(r, args, 1, 0)
	if err != nil {
		return Undefined(), err
	}
	return Bool(hasSubstringAt(s, search, clampPos(pos, s.Len()))), nil
}

func stringProtoEndsWith(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "endsWith")
	if err != nil {
		return Undefined(), err
	}
	search, err := searchStringArg(r, Arg(args, 0), "endsWith")
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	end, err := argIntegerOrInfinity(r, args, 1, float64(n))
	if err != nil {
		return Undefined(), err
	}
	start := clampPos(end, n) - search.Len()
	if start < 0 {
		return False(), nil
	}
	return Bool(hasSubstringAt(s, search, start)), nil
}

// --- slicing --------------------------------------------------------------------------

func stringProtoSlice(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "slice")
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	start, err := argIntegerOrInfinity(r, args, 0, 0)
	if err != nil {
		return Undefined(), err
	}
	end, err := argIntegerOrInfinity(r, args, 1, float64(n))
	if err != nil {
		return Undefined(), err
	}
	from, to := relativePos(start, n), relativePos(end, n)
	if from >= to {
		return StringValue(emptyString), nil
	}
	return StringValue(r.substring(s, from, to)), nil
}

func stringProtoSubstring(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "substring")
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	start, err := argIntegerOrInfinity(r, args, 0, 0)
	if err != nil {
		return Undefined(), err
	}
	end, err := argIntegerOrInfinity(r, args, 1, float64(n))
	if err != nil {
		return Undefined(), err
	}
	a, b := clampPos(start, n), clampPos(end, n)
	if a > b {
		a, b = b, a
	}
	return StringValue(r.substring(s, a, b)), nil
}

func stringProtoSubstr(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "substr")
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	start, err := argIntegerOrInfinity(r, args, 0, 0)
	if err != nil {
		return Undefined(), err
	}
	from := relativePos(start, n)
	length, err := argIntegerOrInfinity(r, args, 1, float64(n-from))
	if err != nil {
		return Undefined(), err
	}
	to := from + clampPos(length, n-from)
	if from >= to {
		return StringValue(emptyString), nil
	}
	return StringValue(r.substring(s, from, to)), nil
}

// --- case conversion -------------------------------------------------------------------

func stringProtoToLowerCase(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "toLowerCase")
	if err != nil {
		return Undefined(), err
	}
	out, err := stringToLower(r, s)
	return caseConverted(r, out, err)
}

// caseConverted finishes a case conversion: the unconditional special
// casings expand a code point to up to three, so the result is checked
// against the string length limit before it is returned.
func caseConverted(r *Realm, s *String, err error) (Value, error) {
	if err != nil {
		return Undefined(), err
	}
	if s.Len() > maxStringLength {
		return Undefined(), r.invalidStringLength()
	}
	return StringValue(s), nil
}

func stringProtoToUpperCase(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "toUpperCase")
	if err != nil {
		return Undefined(), err
	}
	out, err := stringToUpper(r, s)
	return caseConverted(r, out, err)
}

// stringToLower implements String.prototype.toLowerCase: simple mappings
// from the Unicode tables plus the unconditional special casings (U+0130)
// and the Final_Sigma context rule.
func stringToLower(r *Realm, s *String) (*String, error) {
	f := s // s is returned, f never: a view stays on the stack
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		i := asciiCaseIndex(f.s, 'A')
		if i == len(f.s) {
			return s, nil
		}
		if len(f.s) == 1 {
			c := f.s[0]
			return singleCharStrings[c+'a'-'A'], nil
		}
		var set *caseMemoSet
		if len(f.s) <= caseMemoMax {
			set = r.caseSet(f.s, false)
			if out := set.get(f.s, false); out != nil {
				return out, nil
			}
		}
		out, b := newASCIIBuf(len(f.s))
		r.chargeString(len(b))
		copy(b, f.s)
		asciiToCase(b[i:], 'A')
		r.putCase(set, out)
		return out, nil
	}
	u := f.units()
	var sb StringBuilder
	sb.Grow(len(u))
	for i := 0; i < len(u); i++ {
		if err := interruptEvery(r, int64(i)); err != nil {
			return nil, err
		}
		c := u[i]
		if c < 0x80 {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			sb.WriteASCII(byte(c))
			continue
		}
		rn, w := decodeUnitAt(u, i)
		switch rn {
		case 0x0130: // LATIN CAPITAL LETTER I WITH DOT ABOVE
			sb.WriteUnit('i')
			sb.WriteUnit(0x0307)
		case 0x03A3: // GREEK CAPITAL LETTER SIGMA
			if isFinalSigma(u, i) {
				sb.WriteUnit(0x03C2)
			} else {
				sb.WriteUnit(0x03C3)
			}
		default:
			sb.WriteRune(unicode.ToLower(rn))
		}
		i += w - 1
	}
	return r.builtString(&sb), nil
}

// stringToUpper implements String.prototype.toUpperCase with the
// unconditional SpecialCasing expansions (ß -> SS, ligatures, Greek with
// ypogegrammeni, ...).
func stringToUpper(r *Realm, s *String) (*String, error) {
	f := s // s is returned, f never: a view stays on the stack
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		i := asciiCaseIndex(f.s, 'a')
		if i == len(f.s) {
			return s, nil
		}
		if len(f.s) == 1 {
			c := f.s[0]
			return singleCharStrings[c-('a'-'A')], nil
		}
		var set *caseMemoSet
		if len(f.s) <= caseMemoMax {
			set = r.caseSet(f.s, true)
			if out := set.get(f.s, true); out != nil {
				return out, nil
			}
		}
		out, b := newASCIIBuf(len(f.s))
		r.chargeString(len(b))
		copy(b, f.s)
		asciiToCase(b[i:], 'a')
		r.putCase(set, out)
		return out, nil
	}
	u := f.units()
	var sb StringBuilder
	sb.Grow(len(u) + 4)
	for i := 0; i < len(u); i++ {
		if err := interruptEvery(r, int64(i)); err != nil {
			return nil, err
		}
		c := u[i]
		if c < 0x80 {
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			sb.WriteASCII(byte(c))
			continue
		}
		rn, w := decodeUnitAt(u, i)
		if sp := specialUpper(rn); sp != "" {
			sb.WriteGoString(sp)
		} else {
			sb.WriteRune(unicode.ToUpper(rn))
		}
		i += w - 1
	}
	return r.builtString(&sb), nil
}

// caseMemo maps short ASCII strings to their conversions to lower and to
// upper case, by content: a component library converts the same few
// hundred names at every render (style properties, event names, tags),
// each into a new string. A conversion of 2 to caseMemoMax units that
// changes the string looks in one set of caseMemoWays results, chosen by a
// hash of the units and the direction, for one whose units are those of
// the conversion, which it checks unit by unit, and returns it: a string
// whose conversion is in the set allocates nothing. A new result goes in
// the way the count of the results put in picks (caseNext).
//
// It holds only results, strings the conversions made (newASCIIBuf), each
// keeping nothing alive but its own 80 bytes at most: the memo keeps at
// most caseMemoSets*caseMemoWays of them, 40 KiB, besides its own 4 KiB
// (memo.go).
type caseMemo [caseMemoSets]caseMemoSet

type caseMemoSet [caseMemoWays]*String

// caseMemoSets sets of caseMemoWays results take 4 KiB (memo.go). At 512
// results 96 % of the bytes the case conversions of two server-side
// rendering workloads allocate are saved, at 1024 98 to 99 %.
const (
	caseMemoBits = 6
	caseMemoSets = 1 << caseMemoBits
	caseMemoWays = 8
	caseMemoMax  = 32
)

// caseSeed seeds the memo's hash, so that no input can pick the sets of
// its strings.
var caseSeed = maphash.MakeSeed()

// caseSet returns the set of the realm's case memo that may hold the
// conversion of the ASCII string a, of 2 to caseMemoMax units, to upper
// case or to lower case; nil when the realm has no memo yet, counting the
// conversion then.
func (r *Realm) caseSet(a string, upper bool) *caseMemoSet {
	lz := r.lazy
	if lz == nil {
		return nil
	}
	ms := lz.memos
	if ms == nil {
		if ms = r.newMemos(); ms == nil {
			return nil
		}
	}
	c := ms.cases
	if c == nil {
		c = new(caseMemo)
		ms.cases = c
		r.charge(int(unsafe.Sizeof(caseMemo{})))
	}
	h := maphash.String(caseSeed, a) >> (64 - caseMemoBits)
	if upper {
		h ^= 1 // a string's two conversions are in different sets
	}
	return &c[h]
}

// get returns the conversion of a the set c holds, nil if c is nil or
// holds none.
func (c *caseMemoSet) get(a string, upper bool) *String {
	if c == nil {
		return nil
	}
	for _, out := range c {
		if out != nil && len(out.s) == len(a) && isCaseOf(out.s, a, upper) {
			return out
		}
	}
	return nil
}

// isCaseOf reports whether the ASCII o is the ASCII a, of the same length,
// converted to upper case or to lower case.
func isCaseOf(o, a string, upper bool) bool {
	from := byte('A')
	if upper {
		from = 'a'
	}
	o = o[:len(a)]
	for i := range len(a) {
		c := a[i]
		if c-from < 26 {
			c ^= 'a' - 'A'
		}
		if c != o[i] {
			return false
		}
	}
	return true
}

// putCase puts the conversion out in the set c, unless c is nil, in the
// way the count of the memo's insertions picks.
func (r *Realm) putCase(c *caseMemoSet, out *String) {
	if c == nil {
		return
	}
	ms := r.lazy.memos
	c[ms.caseNext%caseMemoWays] = out
	ms.caseNext++
}

// asciiToCase converts the ASCII bytes b in place to lower case, when from
// is 'A', or to upper case, when from is 'a': a byte from to from+25 has
// bit 0x20 flipped. Eight bytes at a time, with no branch on a byte, whose
// cost would depend on where the assembler pads the loop (caseLetters).
func asciiToCase(b []byte, from byte) {
	i := 0
	for ; i+8 <= len(b); i += 8 {
		x := binary.LittleEndian.Uint64(b[i:])
		binary.LittleEndian.PutUint64(b[i:], x^caseLetters(x, from)>>2)
	}
	for ; i < len(b); i++ {
		if b[i]-from < 26 {
			b[i] ^= 'a' - 'A'
		}
	}
}

// decodeUnitAt returns the code point at u[i] (a lone surrogate is returned
// as is) and the number of units it spans.
func decodeUnitAt(u []uint16, i int) (rune, int) {
	c := u[i]
	if c >= 0xD800 && c < 0xDC00 && i+1 < len(u) {
		if lo := u[i+1]; lo >= 0xDC00 && lo < 0xE000 {
			return utf16.DecodeRune(rune(c), rune(lo)), 2
		}
	}
	return rune(c), 1
}

func isCased(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) }

// isCaseIgnorable approximates the Unicode Case_Ignorable property.
func isCaseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', '^', '`', 0xAD, 0xB7, 0x2018, 0x2019, 0x2024, 0x2027:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

// isFinalSigma implements the Final_Sigma condition for the sigma at u[i]:
// preceded by a cased letter and not followed by one, skipping
// case-ignorable characters.
func isFinalSigma(u []uint16, i int) bool {
	before := false
	for j := i - 1; j >= 0; j-- {
		c := u[j]
		if c >= 0xDC00 && c < 0xE000 && j > 0 && u[j-1] >= 0xD800 && u[j-1] < 0xDC00 {
			j--
		}
		rn, _ := decodeUnitAt(u, j)
		if isCaseIgnorable(rn) {
			continue
		}
		before = isCased(rn)
		break
	}
	if !before {
		return false
	}
	for j := i + 1; j < len(u); {
		rn, w := decodeUnitAt(u, j)
		j += w
		if isCaseIgnorable(rn) {
			continue
		}
		return !isCased(rn)
	}
	return true
}

// specialUpper returns the unconditional full uppercase mapping of r when
// it differs from the simple mapping (SpecialCasing.txt), else "".
func specialUpper(r rune) string {
	switch {
	case r == 0xDF, r == 0x149, r == 0x1F0, r == 0x390, r == 0x3B0, r == 0x587,
		r >= 0x1E96 && r <= 0x1E9A, r >= 0x1F50 && r <= 0x1FFC, r >= 0xFB00 && r <= 0xFB17:
		return specialUpperTable[r]
	}
	return ""
}

var specialUpperTable = map[rune]string{
	0x00DF: "SS", 0x0149: "ʼN", 0x01F0: "J̌", 0x0390: "Ϊ́", 0x03B0: "Ϋ́",
	0x0587: "ԵՒ", 0x1E96: "H̱", 0x1E97: "T̈", 0x1E98: "W̊", 0x1E99: "Y̊", 0x1E9A: "Aʾ",
	0x1F50: "Υ̓", 0x1F52: "Υ̓̀", 0x1F54: "Υ̓́", 0x1F56: "Υ̓͂",
	0x1F80: "ἈΙ", 0x1F81: "ἉΙ", 0x1F82: "ἊΙ", 0x1F83: "ἋΙ",
	0x1F84: "ἌΙ", 0x1F85: "ἍΙ", 0x1F86: "ἎΙ", 0x1F87: "ἏΙ",
	0x1F88: "ἈΙ", 0x1F89: "ἉΙ", 0x1F8A: "ἊΙ", 0x1F8B: "ἋΙ",
	0x1F8C: "ἌΙ", 0x1F8D: "ἍΙ", 0x1F8E: "ἎΙ", 0x1F8F: "ἏΙ",
	0x1F90: "ἨΙ", 0x1F91: "ἩΙ", 0x1F92: "ἪΙ", 0x1F93: "ἫΙ",
	0x1F94: "ἬΙ", 0x1F95: "ἭΙ", 0x1F96: "ἮΙ", 0x1F97: "ἯΙ",
	0x1F98: "ἨΙ", 0x1F99: "ἩΙ", 0x1F9A: "ἪΙ", 0x1F9B: "ἫΙ",
	0x1F9C: "ἬΙ", 0x1F9D: "ἭΙ", 0x1F9E: "ἮΙ", 0x1F9F: "ἯΙ",
	0x1FA0: "ὨΙ", 0x1FA1: "ὩΙ", 0x1FA2: "ὪΙ", 0x1FA3: "ὫΙ",
	0x1FA4: "ὬΙ", 0x1FA5: "ὭΙ", 0x1FA6: "ὮΙ", 0x1FA7: "ὯΙ",
	0x1FA8: "ὨΙ", 0x1FA9: "ὩΙ", 0x1FAA: "ὪΙ", 0x1FAB: "ὫΙ",
	0x1FAC: "ὬΙ", 0x1FAD: "ὭΙ", 0x1FAE: "ὮΙ", 0x1FAF: "ὯΙ",
	0x1FB2: "ᾺΙ", 0x1FB3: "ΑΙ", 0x1FB4: "ΆΙ", 0x1FB6: "Α͂",
	0x1FB7: "Α͂Ι", 0x1FBC: "ΑΙ", 0x1FC2: "ῊΙ", 0x1FC3: "ΗΙ",
	0x1FC4: "ΉΙ", 0x1FC6: "Η͂", 0x1FC7: "Η͂Ι", 0x1FCC: "ΗΙ",
	0x1FD2: "Ϊ̀", 0x1FD3: "Ϊ́", 0x1FD6: "Ι͂", 0x1FD7: "Ϊ͂",
	0x1FE2: "Ϋ̀", 0x1FE3: "Ϋ́", 0x1FE4: "Ρ̓", 0x1FE6: "Υ͂",
	0x1FE7: "Ϋ͂", 0x1FF2: "ῺΙ", 0x1FF3: "ΩΙ", 0x1FF4: "ΏΙ",
	0x1FF6: "Ω͂", 0x1FF7: "Ω͂Ι", 0x1FFC: "ΩΙ",
	0xFB00: "FF", 0xFB01: "FI", 0xFB02: "FL", 0xFB03: "FFI", 0xFB04: "FFL", 0xFB05: "ST", 0xFB06: "ST",
	0xFB13: "ՄՆ", 0xFB14: "ՄԵ", 0xFB15: "ՄԻ", 0xFB16: "ՎՆ", 0xFB17: "ՄԽ",
}

// --- trimming -------------------------------------------------------------------------

// trimString implements the TrimString abstract operation.
func trimString(s *String, start, end bool) *String {
	f := s // s is returned (Substring), f never: a view stays on the stack
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	n := s.Len()
	a, b := 0, n
	if f.kind == strASCII {
		str := f.s
		if start {
			for a < b && isASCIIWhitespace(str[a]) {
				a++
			}
		}
		if end {
			for b > a && isASCIIWhitespace(str[b-1]) {
				b--
			}
		}
	} else {
		u := f.units()
		if start {
			for a < b && isJSWhitespace(u[a]) {
				a++
			}
		}
		if end {
			for b > a && isJSWhitespace(u[b-1]) {
				b--
			}
		}
	}
	return s.Substring(a, b)
}

func isASCIIWhitespace(c byte) bool { return c == ' ' || (c >= 0x09 && c <= 0x0D) }

func stringProtoTrim(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "trim")
	if err != nil {
		return Undefined(), err
	}
	return StringValue(trimString(s, true, true)), nil
}

func stringProtoTrimStart(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "trimStart")
	if err != nil {
		return Undefined(), err
	}
	return StringValue(trimString(s, true, false)), nil
}

func stringProtoTrimEnd(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "trimEnd")
	if err != nil {
		return Undefined(), err
	}
	return StringValue(trimString(s, false, true)), nil
}

// --- building --------------------------------------------------------------------------

func stringProtoConcat(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "concat")
	if err != nil {
		return Undefined(), err
	}
	for _, a := range args {
		t, err := r.ToString(a)
		if err != nil {
			return Undefined(), err
		}
		if s, err = r.Concat(s, t); err != nil {
			return Undefined(), err
		}
	}
	return StringValue(s), nil
}

func stringProtoRepeat(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "repeat")
	if err != nil {
		return Undefined(), err
	}
	n, err := argIntegerOrInfinity(r, args, 0, 0)
	if err != nil {
		return Undefined(), err
	}
	if n < 0 || n == posInf {
		return Undefined(), r.RangeError("Invalid count value: %s", NumberToGoString(n))
	}
	if n == 0 || s.Len() == 0 {
		return StringValue(emptyString), nil
	}
	if float64(s.Len())*n > float64(maxStringLength) || r.overBudget(s.Len()*int(n)) {
		return Undefined(), r.invalidStringLength()
	}
	res, err := repeatString(r, s, int(n))
	if err != nil {
		return Undefined(), err
	}
	return StringValue(res), nil
}

// repeatString returns s repeated count times (count >= 1, s non-empty),
// checking for interrupts between 64 KiB chunks of the output.
func repeatString(r *Realm, s *String, count int) (*String, error) {
	if count == 1 {
		return s, nil
	}
	f := s // s is returned above, f never: a view stays on the stack
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	total := s.Len() * count
	if f.kind == strASCII {
		r.chargeString(total)
	} else {
		r.chargeString(2 * total)
	}
	if total <= 1<<16 {
		if f.kind == strASCII {
			return asciiString(strings.Repeat(f.s, count)), nil
		}
		return utf16String(slices.Repeat(f.units(), count)), nil
	}
	chunk := max(1, (1<<16)/s.Len())
	if f.kind == strASCII {
		b := make([]byte, 0, total)
		for done := 0; done < count; done += chunk {
			for range min(chunk, count-done) {
				b = append(b, f.s...)
			}
			if err := r.CheckInterrupt(); err != nil {
				return nil, err
			}
		}
		return asciiString(bytesToString(b)), nil
	}
	u := make([]uint16, 0, total)
	for done := 0; done < count; done += chunk {
		for range min(chunk, count-done) {
			u = append(u, f.units()...)
		}
		if err := r.CheckInterrupt(); err != nil {
			return nil, err
		}
	}
	return utf16String(u), nil
}

func stringProtoPadStart(r *Realm, this Value, args []Value) (Value, error) {
	return stringPad(r, this, args, true)
}

func stringProtoPadEnd(r *Realm, this Value, args []Value) (Value, error) {
	return stringPad(r, this, args, false)
}

// stringPad implements StringPad for padStart (atStart) and padEnd.
func stringPad(r *Realm, this Value, args []Value, atStart bool) (Value, error) {
	method := "padEnd"
	if atStart {
		method = "padStart"
	}
	s, err := thisStringValue(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	maxLen, err := r.ToLength(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	n := s.Len()
	if maxLen <= int64(n) {
		return StringValue(s), nil
	}
	filler := singleCharStrings[' ']
	if fv := Arg(args, 1); !fv.IsUndefined() {
		if filler, err = r.ToString(fv); err != nil {
			return Undefined(), err
		}
	}
	if filler.Len() == 0 {
		return StringValue(s), nil
	}
	if maxLen > int64(maxStringLength) || r.overBudget(int(maxLen)) {
		return Undefined(), r.invalidStringLength()
	}
	fillLen := int(maxLen) - n
	var sb StringBuilder
	sb.Grow(int(maxLen))
	if !atStart {
		sb.WriteString(s)
	}
	for k := int64(0); fillLen > 0; k++ {
		if err := interruptEvery(r, k); err != nil {
			return Undefined(), err
		}
		if fillLen >= filler.Len() {
			sb.WriteString(filler)
			fillLen -= filler.Len()
			continue
		}
		sb.WriteString(filler.Substring(0, fillLen))
		fillLen = 0
	}
	if atStart {
		sb.WriteString(s)
	}
	return StringValue(r.builtString(&sb)), nil
}

// --- comparison and unsupported ----------------------------------------------------------

func stringProtoLocaleCompare(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisStringValue(r, this, "localeCompare")
	if err != nil {
		return Undefined(), err
	}
	that, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	c, err := collateCompare(r, s, that)
	if err != nil {
		return Undefined(), err
	}
	return IntValue(c), nil
}

// stringProtoMatchAll implements String.prototype.matchAll.
func stringProtoMatchAll(r *Realm, this Value, args []Value) (Value, error) {
	if this.IsNullish() {
		return Undefined(), r.TypeError("String.prototype.matchAll called on null or undefined")
	}
	regexp := Arg(args, 0)
	if regexp.IsObject() {
		o := regexp.AsObject()
		isRx, err := r.isRegExp(regexp)
		if err != nil {
			return Undefined(), err
		}
		if isRx {
			if err := r.regexpRequireGlobal(o, "matchAll"); err != nil {
				return Undefined(), err
			}
		}
		m, builtin, err := r.regexpSymbolMethod(o, rxgMatchAll)
		if err != nil {
			return Undefined(), err
		}
		if builtin {
			return regexpSymMatchAll(r, o, this)
		}
		if !m.IsUndefined() {
			return r.Call(m, regexp, []Value{this})
		}
	}
	s, err := r.ToString(this)
	if err != nil {
		return Undefined(), err
	}
	rx, err := r.regexpCreateFrom(regexp, asciiString("g"))
	if err != nil {
		return Undefined(), err
	}
	m, builtin, err := r.regexpSymbolMethod(rx, rxgMatchAll)
	if err != nil {
		return Undefined(), err
	}
	if builtin {
		return regexpSymMatchAll(r, rx, StringValue(s))
	}
	return r.Call(m, ObjectValue(rx), []Value{StringValue(s)})
}

// --- search -------------------------------------------------------------------------------

// stringProtoSearch implements String.prototype.search.
func stringProtoSearch(r *Realm, this Value, args []Value) (Value, error) {
	if this.IsNullish() {
		return Undefined(), r.TypeError("String.prototype.search called on null or undefined")
	}
	regexp := Arg(args, 0)
	if regexp.IsObject() {
		o := regexp.AsObject()
		m, builtin, err := r.regexpSymbolMethod(o, rxgSearch)
		if err != nil {
			return Undefined(), err
		}
		if builtin {
			return regexpSymSearch(r, o, this)
		}
		if !m.IsUndefined() {
			return r.Call(m, regexp, []Value{this})
		}
	}
	s, err := r.ToString(this)
	if err != nil {
		return Undefined(), err
	}
	rx, err := r.regexpCreateFrom(regexp, AtomEmpty)
	if err != nil {
		return Undefined(), err
	}
	m, builtin, err := r.regexpSymbolMethod(rx, rxgSearch)
	if err != nil {
		return Undefined(), err
	}
	if builtin {
		return regexpSymSearch(r, rx, StringValue(s))
	}
	return r.Call(m, ObjectValue(rx), []Value{StringValue(s)})
}

// --- split ------------------------------------------------------------------------------

func stringProtoSplit(r *Realm, this Value, args []Value) (Value, error) {
	if this.IsNullish() {
		return Undefined(), r.TypeError("String.prototype.split called on null or undefined")
	}
	separator, limit := Arg(args, 0), Arg(args, 1)
	if separator.IsObject() {
		o := separator.AsObject()
		m, builtin, err := r.regexpSymbolMethod(o, rxgSplit)
		if err != nil {
			return Undefined(), err
		}
		if builtin {
			return regexpSymSplit(r, o, this, limit)
		}
		if !m.IsUndefined() {
			return r.Call(m, separator, []Value{this, limit})
		}
	}
	s, err := r.ToString(this)
	if err != nil {
		return Undefined(), err
	}
	lim := uint32(0xFFFFFFFF)
	if !limit.IsUndefined() {
		if lim, err = r.ToUint32(limit); err != nil {
			return Undefined(), err
		}
	}
	sep, err := r.ToString(separator)
	if err != nil {
		return Undefined(), err
	}
	if lim == 0 {
		return ObjectValue(r.NewArrayLen(0)), nil
	}
	if separator.IsUndefined() {
		return ObjectValue(r.NewArray(StringValue(s))), nil
	}
	n := s.Len()
	if n == 0 {
		if sep.Len() != 0 {
			return ObjectValue(r.NewArray(StringValue(s))), nil
		}
		return ObjectValue(r.NewArrayLen(0)), nil
	}
	if sep.Len() == 0 {
		count := int(min(uint64(n), uint64(lim))) // lim may be past an int on 32-bit systems
		if r.overBudget(count * valueSize) {
			return Undefined(), r.RangeError("Invalid array length")
		}
		ao, items := r.newArrayExact(count)
		for i := range items {
			items[i] = StringValue(charString(s.At(i)))
		}
		return ObjectValue(r.initArray(ao, items, uint32(count))), nil
	}
	a, err := splitByString(r, s, sep, lim)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(a), nil
}

// splitByString splits s at every occurrence of the non-empty sep, checking
// the interrupt flag every interruptStride pieces.
func splitByString(r *Realm, s, sep *String, lim uint32) (*Object, error) {
	f := s // s's pieces may be s itself (Substring), f's never: a view stays on the stack
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	fsep := sep // IndexOf takes sep: a view stays on the stack
	if sep.needsFlat() {
		fsep = sep.flat(new(String))
	}
	if f.kind == strASCII && fsep.kind == strASCII {
		str, sp := f.s, fsep.s
		count := strings.Count(str, sp) + 1
		if uint32(count) > lim {
			count = int(lim)
		}
		// The pieces' list and Strings, allocated at once: past the memory
		// limit it throws rather than allocate them.
		if r.overBudget(count * (valueSize + stringHeaderSize)) {
			return nil, r.RangeError("Invalid array length")
		}
		ao, items := r.newArrayExact(count)
		root := s // the string the pieces share (shareRoot)
		if !s.needsFlat() {
			root = s.shareRoot()
		}
		items = items[:count]           // len(items) == count for the loop's bounds checks
		pieces := make([]String, count) // one allocation for every piece
		start := 0
		for i := range count - 1 { // the pieces before the last separator found
			if err := interruptEvery(r, int64(i)); err != nil {
				return nil, err
			}
			end := start + strings.Index(str[start:], sp)
			items[i] = splitPiece(&pieces[i], str, start, end, root)
			start = end + len(sp)
		}
		end := len(str)
		if uint32(count) == lim { // the last piece stops at the next separator
			if j := strings.Index(str[start:], sp); j >= 0 {
				end = start + j
			}
		}
		items[count-1] = splitPiece(&pieces[count-1], str, start, end, root)
		return r.initArray(ao, items, uint32(count)), nil
	}
	var items []Value
	p := 0
	for {
		q := s.IndexOf(sep, p)
		if q < 0 {
			break
		}
		items = r.appendValue(items, StringValue(r.substring(s, p, q)))
		if uint32(len(items)) == lim {
			return r.NewArrayFromSlice(items), nil
		}
		if err := interruptEvery(r, int64(len(items))); err != nil {
			return nil, err
		}
		p = q + sep.Len()
	}
	items = append(items, StringValue(r.substring(s, p, s.Len())))
	return r.NewArrayFromSlice(items), nil
}

// splitPiece is the value of str[start:end], a piece of root kept in p.
func splitPiece(p *String, str string, start, end int, root *String) Value {
	if end == start {
		return StringValue(emptyString)
	}
	*p = String{s: str[start:end], n: int32(end - start), kind: strASCII, right: root}
	return StringValue(p)
}

// --- replace -----------------------------------------------------------------------------

func stringProtoReplace(r *Realm, this Value, args []Value) (Value, error) {
	return stringReplace(r, this, args, false)
}

func stringProtoReplaceAll(r *Realm, this Value, args []Value) (Value, error) {
	return stringReplace(r, this, args, true)
}

// stringReplace implements String.prototype.replace and replaceAll.
func stringReplace(r *Realm, this Value, args []Value, all bool) (Value, error) {
	method := "replace"
	if all {
		method = "replaceAll"
	}
	if this.IsNullish() {
		return Undefined(), r.TypeError("String.prototype.%s called on null or undefined", method)
	}
	searchValue, replaceValue := Arg(args, 0), Arg(args, 1)
	if searchValue.IsObject() {
		o := searchValue.AsObject()
		if all {
			isRx, err := r.isRegExp(searchValue)
			if err != nil {
				return Undefined(), err
			}
			if isRx {
				if err := r.regexpRequireGlobal(o, "replaceAll"); err != nil {
					return Undefined(), err
				}
			}
		}
		m, builtin, err := r.regexpSymbolMethod(o, rxgReplace)
		if err != nil {
			return Undefined(), err
		}
		if builtin {
			return regexpSymReplace(r, o, this, replaceValue)
		}
		if !m.IsUndefined() {
			return r.Call(m, searchValue, []Value{this, replaceValue})
		}
	}
	s, err := r.ToString(this)
	if err != nil {
		return Undefined(), err
	}
	search, err := r.ToString(searchValue)
	if err != nil {
		return Undefined(), err
	}
	functional := IsCallable(replaceValue)
	var tmpl *String
	if !functional {
		if tmpl, err = r.ToString(replaceValue); err != nil {
			return Undefined(), err
		}
	}
	n, m := s.Len(), search.Len()
	first := s.IndexOf(search, 0)
	if first < 0 {
		return StringValue(s), nil
	}
	var sb StringBuilder
	sb.Grow(n + 16)
	next := 0
	pos := first
	held := int64(-1)
	if functional {
		// The measurements made in the callbacks count the builder, until
		// a return reports nothing again (no defer: the function has too
		// many returns for an open-coded one, and every call would pay).
		held = r.heldBase()
	}
	for pos >= 0 {
		writeSlice(&sb, s, next, pos)
		if functional {
			r.holdBytes(held, sb.heldBytes())
			rv, err := r.Call(replaceValue, Undefined(), []Value{StringValue(search), IntValue(pos), StringValue(s)})
			if err != nil {
				r.holdBytes(held, 0)
				return Undefined(), err
			}
			rs, err := r.ToString(rv)
			if err != nil {
				r.holdBytes(held, 0)
				return Undefined(), err
			}
			sb.WriteString(rs)
		} else if err := r.appendSubstitution(&sb, s, pos, pos+m, nil, nil, Undefined(), tmpl); err != nil {
			return Undefined(), err
		}
		if err := sb.checkLength(r); err != nil {
			r.holdBytes(held, 0)
			return Undefined(), err
		}
		next = pos + m
		if !all {
			break
		}
		from := pos + max(m, 1)
		if from > n {
			break
		}
		pos = s.IndexOf(search, from)
	}
	r.holdBytes(held, 0)
	writeSlice(&sb, s, next, n)
	if err := sb.checkLength(r); err != nil {
		return Undefined(), err
	}
	return StringValue(r.builtString(&sb)), nil
}

// --- match ---------------------------------------------------------------------------------

func stringProtoMatch(r *Realm, this Value, args []Value) (Value, error) {
	if this.IsNullish() {
		return Undefined(), r.TypeError("String.prototype.match called on null or undefined")
	}
	regexp := Arg(args, 0)
	if regexp.IsObject() {
		o := regexp.AsObject()
		m, builtin, err := r.regexpSymbolMethod(o, rxgMatch)
		if err != nil {
			return Undefined(), err
		}
		if builtin {
			return regexpSymMatch(r, o, this)
		}
		if !m.IsUndefined() {
			return r.Call(m, regexp, []Value{this})
		}
	}
	s, err := r.ToString(this)
	if err != nil {
		return Undefined(), err
	}
	rx, err := r.regexpCreateFrom(regexp, AtomEmpty)
	if err != nil {
		return Undefined(), err
	}
	m, builtin, err := r.regexpSymbolMethod(rx, rxgMatch)
	if err != nil {
		return Undefined(), err
	}
	if builtin {
		return regexpSymMatch(r, rx, StringValue(s))
	}
	return r.Call(m, ObjectValue(rx), []Value{StringValue(s)})
}

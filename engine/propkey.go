package engine

import (
	"strconv"
	"unsafe"
)

// maxArrayIndex is the largest canonical array index (2^32 - 2).
const maxArrayIndex = 1<<32 - 2

// PropertyKey identifies an own property. It is a Value restricted to an
// interned *String, a *Symbol, a canonical array index (an integer number
// in [0, 2^32-2]) or a class's *PrivateName (private.go). Two keys are equal iff their 16 bytes are equal, so keys
// are usable as Go map keys and never need string comparison.
type PropertyKey struct {
	v Value
}

// IndexKey builds a key for a canonical array index.
func IndexKey(i uint32) PropertyKey { return PropertyKey{Uint32Value(i)} }

// StringKey builds a key from an interned string. The caller must pass an
// atom (Realm.Intern or a static Atom*); it is a programming error otherwise.
func StringKey(s *String) PropertyKey {
	if s.atom == 0 {
		panic("engine: StringKey requires an interned string")
	}
	return PropertyKey{StringValue(s)}
}

// SymbolKey builds a key from a symbol.
func SymbolKey(s *Symbol) PropertyKey { return PropertyKey{SymbolValue(s)} }

// IsIndex reports whether the key is an array index.
func (k PropertyKey) IsIndex() bool { return k.v.ptr == nil }

// Index returns the array index. Only valid when IsIndex.
func (k PropertyKey) Index() uint32 { return uint32(k.v.AsNumber()) }

// IsString reports whether the key is a string.
func (k PropertyKey) IsString() bool { return k.v.IsString() }

// String returns the interned string. Only valid when IsString.
func (k PropertyKey) String() *String { return k.v.AsString() }

// IsSymbol reports whether the key is a symbol.
func (k PropertyKey) IsSymbol() bool { return k.v.IsSymbol() }

// Symbol returns the symbol. Only valid when IsSymbol.
func (k PropertyKey) Symbol() *Symbol { return k.v.AsSymbol() }

// Value returns the key as a Value (index keys become numbers).
func (k PropertyKey) Value() Value { return k.v }

// GoString renders the key for messages and host export (index -> decimal).
func (k PropertyKey) GoString() string {
	switch {
	case k.IsIndex():
		return strconv.FormatUint(uint64(k.Index()), 10)
	case k.IsString():
		return k.String().GoString()
	case k.IsSymbol():
		return k.Symbol().descriptiveString()
	case k.IsPrivate():
		return k.v.asPrivate().desc.GoString()
	}
	return "<root>"
}

// ToJSString returns the key as a JavaScript string (symbols are converted to
// their descriptive form; callers handling symbols must check first).
func (k PropertyKey) ToJSString(r *Realm) *String {
	switch {
	case k.IsString():
		return k.String()
	case k.IsIndex():
		return r.indexString(k.Index())
	case k.IsPrivate():
		return k.v.asPrivate().desc
	}
	return FromGoString(k.Symbol().descriptiveString())
}

// parseArrayIndex parses an ASCII decimal canonical array index: no sign, no
// leading zeros except "0" itself, value <= 2^32-2.
func parseArrayIndex(s string) (uint32, bool) {
	n := len(s)
	if n == 0 || n > 10 {
		return 0, false
	}
	if s[0] == '0' {
		return 0, n == 1
	}
	var v uint64
	for i := range n {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	if v > maxArrayIndex {
		return 0, false
	}
	return uint32(v), true
}

// KeyFromString converts a string to a PropertyKey: canonical array indices
// become index keys, everything else is interned. The key of a string that
// is not an atom comes from the realm's key memo when the string was used
// as a key before (keyMemo).
func (r *Realm) KeyFromString(s *String) PropertyKey {
	if s.atom == 0 {
		if lz := r.lazy; lz != nil && lz.keys != nil {
			set := &lz.keys[keyMemoSlot(s)]
			if set[0].s == s {
				return set[0].k
			}
			if set[1].s == s {
				return set[1].k
			}
		}
		return r.keyFromString(s)
	}
	// indexString interns the small index strings (so for-in and
	// Object.keys hand out atoms like "0"), and the intern table is global,
	// so an atom may still spell a canonical index.
	if s.kind == strASCII && s.s != "" && s.s[0] <= '9' {
		if i, ok := parseArrayIndex(s.s); ok {
			return IndexKey(i)
		}
	}
	return PropertyKey{StringValue(s)}
}

// keyFromString is KeyFromString for a string that is not an atom: an index
// key, or the atom of the string's content, which takes a lookup in the
// static atoms and one in the realm's cache or the process-wide table. It
// remembers the key in the key memo.
func (r *Realm) keyFromString(s *String) PropertyKey {
	f := s // s is interned below, f never: a view stays on the stack
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		if i, ok := parseArrayIndex(f.s); ok {
			return IndexKey(i)
		}
	}
	k := StringKey(r.Intern(s)) // Intern may return what it is given
	r.rememberKey(s, k)
	return k
}

// keyMemo maps strings that are not atoms to their keys, by the String: a
// string constant whose content a realm interned from its data before the
// code first ran keeps its own String (constASCIIAtom), as do the pieces of
// a split, so a bundle that keeps property names in an array of literals
// reads obj[name] with the same String that is not an atom every time, and
// a memo hit costs a load and a comparison where interning costs a lookup
// in the static atoms and one in the realm's cache, each hashing the
// content. It has two ways a set, the set by address, and holds each
// String it maps: the String cannot be collected while its entry stands,
// so another String at its address can never be taken for it, and the atom
// stays the process-wide one for its content (the intern table's entry is
// weak only while nothing holds the atom).
//
// One way a set made two names a loop reads in turn (an Intl polyfill's
// option names, read some 60 000 times a render each) miss every time
// when their Strings fell in one slot, and which did turned on where the
// allocator put them: a render's instructions moved by 4 % from one run
// to the next, and with any change that moved its strings. Two ways keep
// both: a dozen such names put two in one of 64 slots in two layouts of
// three, and three in one of 64 sets in one layout of twenty.
type keyMemo [keyMemoSets][keyMemoWays]keyMemoEntry

type keyMemoEntry struct {
	s *String
	k PropertyKey
}

// The memo's 128 entries take 3 KiB, allocated when a realm with lazy
// state has converted keyMemoAfter strings that the memo takes, as a realm
// creates its intern caches after internCacheAfter names: a runtime that
// converts a few, a plugin's, pays nothing.
const (
	keyMemoBits  = 6
	keyMemoSets  = 1 << keyMemoBits
	keyMemoWays  = 2
	keyMemoAfter = 1024
)

// keyMemoSlot is the memo set of s, by its address (Fibonacci hashing of
// the 16-byte granules, so Strings next to each other in a span or a slab
// spread over the sets).
func keyMemoSlot(s *String) uint32 {
	return uint32(uintptr(unsafe.Pointer(s))>>4) * 0x9e3779b9 >> (32 - keyMemoBits)
}

// rememberKey puts s and its key k first in its set of the realm's key
// memo, where the set's first entry moves second and its second goes. Only
// a string that keeps nothing but its own units alive goes in, so that the
// memo holds at most keyMemoSets*keyMemoWays short strings, which
// the live measurement (memlive.go) does not count, as the intern caches
// keep only short names: a flat string of at most maxSharedDataKey units
// that shares no other string's units (right, shareRoot), does not start an
// append buffer (strInBuf), was not carved from a host conversion's slab
// (strHost) and is not a slice of a JSON text (strSlice). A lazy string,
// whose source may be a slice of a JSON text or a host's Go text, is not
// flat. Until the memo exists it counts such strings instead
// (keyMemoAfter).
func (r *Realm) rememberKey(s *String, k PropertyKey) {
	if s.needsFlat() || s.n > maxSharedDataKey || s.right != nil || s.flags&(strInBuf|strHost|strSlice) != 0 {
		return
	}
	lz := r.lazy
	if lz == nil {
		return
	}
	if lz.keys == nil {
		if lz.keyMisses < keyMemoAfter {
			lz.keyMisses++
			return
		}
		lz.keys = new(keyMemo)
	}
	set := &lz.keys[keyMemoSlot(s)]
	set[1], set[0] = set[0], keyMemoEntry{s, k}
}

// KeyFromGoString converts a Go string to a PropertyKey.
func (r *Realm) KeyFromGoString(g string) PropertyKey {
	if i, ok := parseArrayIndex(g); ok {
		return IndexKey(i)
	}
	return StringKey(r.InternGoString(g))
}

// ToPropertyKey implements the ToPropertyKey abstract operation.
func (r *Realm) ToPropertyKey(v Value) (PropertyKey, error) {
	switch v.Type() {
	case TypeString:
		return r.KeyFromString(v.AsString()), nil
	case TypeNumber:
		if i, ok := v.IsArrayIndex(); ok {
			return IndexKey(i), nil
		}
		if v.AsNumber() == 0 { // -0 names the property "0"
			return IndexKey(0), nil
		}
		return StringKey(r.Intern(NumberToString(v.AsNumber()))), nil
	case TypeSymbol:
		return SymbolKey(v.AsSymbol()), nil
	case TypeObject:
		p, err := r.ToPrimitive(v, HintString)
		if err != nil {
			return PropertyKey{}, err
		}
		return r.ToPropertyKey(p)
	}
	s, err := r.ToString(v)
	if err != nil {
		return PropertyKey{}, err
	}
	return r.KeyFromString(s), nil
}

// CanonicalNumericIndexString implements the spec operation: it returns the
// number n such that ToString(n) == s (or -0 for "-0"), and ok == true.
func CanonicalNumericIndexString(s *String) (float64, bool) {
	if s.EqualsGoString("-0") {
		return negativeZero, true
	}
	n := StringToNumber(s)
	if NumberToString(n).Equals(s) {
		return n, true
	}
	return 0, false
}

// canonicalNumericName reports whether CanonicalNumericIndexString of the
// ASCII name is not undefined. Only a name that starts like a number can be
// one, and it never holds whitespace, so the literal parses as it stands.
func canonicalNumericName(name string) bool {
	if name == "" {
		return false
	}
	switch c := name[0]; {
	case c == 'I', c == 'N', c == '-', c >= '0' && c <= '9':
		return name == "-0" || NumberToGoString(parseNumericLiteral(name)) == name
	}
	return false
}

// smallIndexCached is how many index strings indexString caches per realm.
const smallIndexCached = 128

// indexString returns the decimal string for an array index, caching small
// values per realm.
func (r *Realm) indexString(i uint32) *String {
	if i >= smallIndexCached {
		return asciiString(strconv.FormatUint(uint64(i), 10))
	}
	if r.smallIndexStrings == nil {
		r.smallIndexStrings = new([smallIndexCached]*String)
	}
	if s := r.smallIndexStrings[i]; s != nil {
		return s
	}
	s := r.Intern(asciiString(strconv.FormatUint(uint64(i), 10)))
	r.smallIndexStrings[i] = s
	return s
}

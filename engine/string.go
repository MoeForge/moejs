package engine

import (
	"strings"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// String kinds (String.kind).
const (
	strASCII uint8 = iota // s holds bytes < 0x80; byte index == code unit index
	strUTF16              // u holds code units, at least one >= 0x80
	strRope               // left/right children; flattened lazily, never in place (flatten)
	// strLazy retains immutable source bytes in s (Go text, WTF-8 or JSON
	// contents) and its length in n, counted when it was made; p points to a
	// lazyStringData, which publishes the decoded flat String on demand.
	strLazy
)

// String.flags.
const (
	// strInBuf marks a flat string whose units start an append buffer
	// (string_append.go): Concat may write past them in place.
	strInBuf uint8 = 1 << iota
	// strCharged marks a rope allocated as a ropeNode (memlimit.go): its
	// flattening is charged to the account the node holds.
	strCharged
	// strConst marks a string constant of compiled code that every realm
	// running it shares (funcMeta): the live measurement does not count it,
	// as it does not count the shared shapes (memlive.go).
	strConst
	// strHost marks a string FromGo made for a field of a host's container
	// (hostHeap.str), carved from its slab of Strings or lazy: a string
	// that shares its units does not keep it (shareRoot, unflatRoot),
	// which would keep the slab and its other strings.
	strHost
	// Lazy modes occupy bits 4 and 5; bit 6 says that the content decodes
	// to UTF-16 rather than ASCII.
	lazyModeShift = 4
	strLazyWide   = 1 << 6
	// strLazyRooted marks a lazy string a parse under a memory limit made:
	// its p points at a rootedLazyData, which holds its share root
	// (string_lazy.go).
	strLazyRooted = 1 << 7
)

// jsonPlain marks ASCII bytes with nothing JSON quoting escapes; 0 is unknown.
const jsonPlain uint8 = 1

// ropeThreshold is the combined length at or above which Concat builds a
// rope instead of copying.
const ropeThreshold = 64

// MaxStringLength is the maximum JavaScript string length in code units.
// Every *String satisfies Len() <= MaxStringLength, which is what lets
// Concat add two int32 lengths as an int without overflow on any platform;
// producers whose output can outgrow their inputs check
// maxStringLength before allocating and raise RangeError: Invalid string
// length (Realm.invalidStringLength).
const MaxStringLength = 1<<30 - 24

// maxStringLength is the limit the checks compare against; a variable only
// so tests can lower it and exercise every producer at small sizes.
var maxStringLength = MaxStringLength

// String is an immutable JavaScript string (a UTF-16 code unit sequence).
// Strings are shared freely; the only mutations are the publication of a
// rope's flattened contents, which leaves the fields its readers read as
// they were (flatten), and the caching of hash/atom, both of which preserve
// the logical value. Concat writes past the end of a string in an append
// buffer, never into it. A lazy string publishes its decoded String in
// right, once (resolved); its source, kind, n and flags never change.
type String struct {
	// s is the strASCII payload or the strLazy source; a strRope
	// publishes its flattened contents in it (flatten).
	s string
	// p is the strUTF16 payload, the first of n code units (never all
	// ASCII), a strLazy's lazyStringData, or a strRope's left child; one
	// field for all keeps String in the 48-byte size class. units and
	// children read it.
	p unsafe.Pointer
	// right publishes a strLazy's decoded cache atomically, or is a
	// strRope's right child. A flat string that shares the
	// units of another, a substring or a value JSON.parse read out of a
	// text, keeps that string here (its share root, shareRoot): the units
	// it shares keep the whole of the root's alive, which the live
	// measurement counts once (memlive.go).
	right *String
	n     int32  // length in code units; a lazy string's is counted when it is made
	hash  uint32 // 0 = not computed
	atom  uint32 // 0 = not interned
	kind  uint8
	// flags holds strInBuf, strCharged, strConst and strHost, and a lazy
	// string's mode, strLazyWide and strLazyRooted, set before the String
	// is published and never changed.
	flags uint8
	// numeric marks an atom that is a canonical numeric string other than
	// an array index ("-0", "1.5", "NaN"): the keys a typed array answers
	// itself (typedArrayKey). It is set before the atom is published.
	numeric bool
	// jsonPlain records that parsing found no bytes to escape in this ASCII
	// string, so quote can copy it without scanning; 0 means unknown.
	// Lazy strings keep theirs in lazyStringData's facts instead.
	json uint8
}

// emptyString is the shared "" value.
var emptyString = &String{kind: strASCII}

// EmptyString returns the shared empty string.
func EmptyString() *String { return emptyString }

// FromGoString converts a Go (UTF-8) string. ASCII input is zero-copy;
// anything else is decoded to UTF-16 once, with invalid UTF-8 bytes becoming
// U+FFFD (Go's range semantics). FromHostString defers the decoding.
func FromGoString(s string) *String {
	if len(s) == 0 {
		return emptyString
	}
	if isASCII(s) {
		return &String{s: s, n: int32(len(s)), kind: strASCII}
	}
	return utf16String(appendUTF16(make([]uint16, 0, len(s)), s))
}

// appendUTF16 appends the UTF-16 encoding of the UTF-8 string s to u
// (invalid bytes become U+FFFD, as Go's range does).
func appendUTF16(u []uint16, s string) []uint16 {
	for _, r := range s {
		if r < 0x10000 {
			u = append(u, uint16(r))
			continue
		}
		hi, lo := utf16.EncodeRune(r)
		u = append(u, uint16(hi), uint16(lo))
	}
	return u
}

// FromUTF16 builds a string from code units. The slice is retained when the
// content is not ASCII, so callers must not modify it afterwards.
func FromUTF16(u []uint16) *String {
	if len(u) == 0 {
		return emptyString
	}
	for _, c := range u {
		if c >= 0x80 {
			return utf16String(u)
		}
	}
	b := make([]byte, len(u))
	for i, c := range u {
		b[i] = byte(c)
	}
	return &String{s: bytesString(b), n: int32(len(b)), kind: strASCII}
}

// asciiString wraps a Go string already known to be ASCII.
func asciiString(s string) *String {
	if len(s) == 0 {
		return emptyString
	}
	return &String{s: s, n: int32(len(s)), kind: strASCII}
}

// Len returns the length in UTF-16 code units. A lazy string's is known
// from its creation, so reading it decodes nothing.
func (s *String) Len() int { return int(s.n) }

// IsASCII reports whether the string (after flattening) is ASCII-only.
func (s *String) IsASCII() bool {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	return f.kind == strASCII
}

// IsInterned reports whether s is an atom.
func (s *String) IsInterned() bool { return s.atom != 0 }

// At returns the code unit at index i (0 <= i < Len).
func (s *String) At(i int) uint16 {
	switch s.kind {
	case strASCII:
		return uint16(s.s[i])
	case strUTF16:
		return s.units()[i]
	}
	return s.flat(new(String)).at(i)
}

// at is At of a flat string, for views (flat).
func (s *String) at(i int) uint16 {
	if s.kind == strASCII {
		return uint16(s.s[i])
	}
	return s.units()[i]
}

// ASCII returns the underlying Go string and true when the string is ASCII.
func (s *String) ASCII() (string, bool) {
	f := s
	if s.kind >= strRope { // needsFlat, spelled out to stay inlinable
		f = s.flat(new(String))
	}
	return f.s, f.kind == strASCII // a resolved UTF-16 string's s is ""
}

// UTF16 returns the code units. For ASCII strings a fresh slice is built;
// for UTF-16 strings the internal slice is returned and must not be modified.
func (s *String) UTF16() []uint16 {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strUTF16 {
		return f.units()
	}
	u := make([]uint16, len(f.s))
	for i := range len(f.s) {
		u[i] = uint16(f.s[i])
	}
	return u
}

// GoString exports to a Go string. ASCII, and a lazy string's source that
// is valid UTF-8, are zero-copy; UTF-16 is encoded to UTF-8 with lone
// surrogates replaced by U+FFFD.
func (s *String) GoString() string {
	if g, ok := s.sourceUTF8(); ok {
		return g
	}
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		return f.s
	}
	return utf16ToGoString(f.units())
}

// wtf8 is GoString with lone surrogates encoded as WTF-8 three-byte
// sequences instead of U+FFFD: the source text of dynamic code, whose
// literals keep them (fromWTF8Text). knownUTF8 lets JSON reuse validation;
// false means unknown, not necessarily invalid.
func (s *String) wtf8() (text string, knownUTF8 bool) {
	text, _, knownUTF8 = s.wtf8Root()
	return text, knownUTF8
}

// wtf8Root is wtf8 that also returns the String whose bytes text is, the
// share root of a parse of text (memAccount.shareText): s when text is its
// units, a rope's flattening included, or its UTF-8 source; the decoded
// String of a lazy string whose source is not, when that decoded to ASCII;
// nil for a copy. s's kind decides, not where text's bytes are: a rope's s
// is written by flatten and read only through published, and a parse
// rooted at no String would keep a decoded String's bytes in a String of
// its own, which the live measurement counts again.
func (s *String) wtf8Root() (text string, root *String, knownUTF8 bool) {
	if g, ok := s.sourceUTF8(); ok {
		return g, s, true
	}
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		root = s
		if s.isLazy() {
			root = s.resolved() // decoded already: f is that String
		}
		return f.s, root, true
	}
	return wtf8FromUTF16(f.units()), nil, false
}

func utf16ToGoString(u []uint16) string {
	var b strings.Builder
	b.Grow(len(u) + len(u)/2)
	for i := 0; i < len(u); i++ {
		c := u[i]
		if c < 0x80 {
			b.WriteByte(byte(c))
			continue
		}
		if utf16.IsSurrogate(rune(c)) {
			if c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000 {
				b.WriteRune(utf16.DecodeRune(rune(c), rune(u[i+1])))
				i++
				continue
			}
			b.WriteRune(utf8.RuneError)
			continue
		}
		b.WriteRune(rune(c))
	}
	return b.String()
}

// flat returns a lazy string's decoded String, decoding it on first use, or
// a flat view of a rope in *v. A read that needs a rope's units uses this
// view; flatten leaves kind, n and flags unchanged. Allocated with new in the branch that found the rope, v
// costs nothing for a flat string and stays on the caller's stack
// (TestRopeReadNoAlloc) when the caller keeps it in a variable of its own,
//
//	f := s
//	if s.needsFlat() {
//		f = s.flat(new(String))
//	}
//
// and reads f with methods that have no rope branch (at, contentHash,
// units): flat lets its receiver escape (the atomic operations on a
// rope's words take its address), and so does any method that may call
// it, so f must reach neither, nor be returned or kept.
func (s *String) flat(v *String) *String {
	if s.isLazy() {
		return s.resolved()
	}
	f, ok := s.published()
	if !ok {
		s.flatten()
		f, _ = s.published()
	}
	*v = f
	return v
}

// needsFlat reports a rope or a lazy string, whose units are read through
// flat; their kind never changes.
func (s *String) needsFlat() bool { return s.kind >= strRope }

// flatten copies the leaves of the rope s into one allocation and publishes
// it, unless a flattening did already.
//
// A rope may be read by several goroutines at once (strings are shared like
// values: interned names, frozen intrinsics, strings a host keeps), so
// flattening never changes what a reader may be reading: kind, n and the
// flags stay as Concat made them, and nothing but the words below is
// written after the rope is published. Each flattener makes a copy of its
// own; for ASCII bytes it stores s.n into the length word of s (flatLen)
// and for UTF-16 units leaves it 0; then it publishes the address of its
// copy with a compare-and-swap of the data word of s (flatData) from nil.
// The first swap wins and the losers' copies are dropped. The winner then
// releases the children, storing nil into p and right. Each flattener
// charges what it allocated to the account the rope carries
// (chargeFlatten, before the swap), a loser's copy included.
//
// The data word is the publication point. Go's atomic operations are
// sequentially consistent, and the swap that stores the address
// synchronizes with every load that returns it, so a reader that loads a
// published address also sees what its flattener wrote before the swap:
// the copied units, never written again, and the length word, which every
// flattener of the rope stores with the same value (the contents decide
// it). A walk that loads a released child (nil) finds the data word
// published, as the release follows the swap. These four words of a rope
// are only accessed with atomic operations, which the race detector sees
// as the synchronizing pairs they are, and the fields a reader of a flat
// string loads are written only before the string is shared, so those
// reads stay plain loads.
func (s *String) flatten() {
	if s.kind != strRope || atomic.LoadPointer(s.flatData()) != nil {
		return
	}
	var d unsafe.Pointer
	if s.ropeASCII() {
		b := make([]byte, s.n)
		s.copyBytes(b)
		s.chargeFlatten(len(b))
		d = unsafe.Pointer(unsafe.SliceData(b))
		atomic.StoreUintptr(s.flatLen(), uintptr(s.n))
	} else {
		u := make([]uint16, s.n)
		s.copyUnits(u)
		s.chargeFlatten(2 * len(u))
		d = unitsPtr(u)
	}
	if atomic.CompareAndSwapPointer(s.flatData(), nil, d) {
		atomic.StorePointer(s.leftWord(), nil)
		atomic.StorePointer(s.rightWord(), nil)
	}
}

// children returns the children of the rope s, or nil, nil once a
// flattening published its contents (published), which may have released
// them.
func (s *String) children() (l, r *String) {
	if atomic.LoadPointer(s.flatData()) != nil {
		return nil, nil
	}
	l = (*String)(atomic.LoadPointer(s.leftWord()))
	r = (*String)(atomic.LoadPointer(s.rightWord()))
	if l == nil || r == nil {
		return nil, nil
	}
	return l, r
}

// The rope walks below recurse into the shorter child and loop on the
// longer one, so their depth is at most log2 of the length whatever the
// rope's shape (left-deep from `s += x`, right-deep from `s = x + s`), and
// they need no stack on the heap.

// ropeASCII reports whether every leaf of s is ASCII.
func (s *String) ropeASCII() bool {
	for s.kind == strRope {
		l, r := s.children()
		if l == nil {
			f, _ := s.published()
			return f.kind == strASCII
		}
		if l.n > r.n {
			l, r = r, l
		}
		if !l.ropeASCII() {
			return false
		}
		s = r
	}
	return s.kind == strASCII
}

// copyBytes writes the ASCII string s (a rope of ASCII leaves or a leaf)
// to b, len(b) == s.Len().
func (s *String) copyBytes(b []byte) {
	for s.kind == strRope {
		left, right := s.children()
		if left == nil {
			s.copyPublished(b, nil)
			return
		}
		l := int(left.n)
		if l <= int(right.n) {
			left.copyBytes(b[:l])
			s, b = right, b[l:]
		} else {
			right.copyBytes(b[l:])
			s, b = left, b[:l]
		}
	}
	copy(b, s.s)
}

// copyUnits writes the code units of s to u, len(u) == s.Len().
func (s *String) copyUnits(u []uint16) {
	for s.kind == strRope {
		left, right := s.children()
		if left == nil {
			s.copyPublished(nil, u)
			return
		}
		l := int(left.n)
		if l <= int(right.n) {
			left.copyUnits(u[:l])
			s, u = right, u[l:]
		} else {
			right.copyUnits(u[l:])
			s, u = left, u[:l]
		}
	}
	s.copyFlatUnits(u)
}

// copyPublished copies the contents a flattening of the rope s published
// to b, or to u, out of line so that the walks, which appendString runs on
// flat strings, keep a small frame.
//
//go:noinline
func (s *String) copyPublished(b []byte, u []uint16) {
	f, _ := s.published()
	if b != nil {
		copy(b, f.s)
		return
	}
	f.copyFlatUnits(u)
}

// copyFlatUnits is copyUnits of a flat string.
func (s *String) copyFlatUnits(u []uint16) {
	if s.kind == strUTF16 {
		copy(u, s.units())
		return
	}
	for i := range len(s.s) {
		u[i] = uint16(s.s[i])
	}
}

// Concat returns a + b. A short result is a flat copy; a long one appends
// b to a in an append buffer (appendString) when b is the shorter and a is
// flat, and is a rope otherwise: a prepend, a join of two big pieces, or an
// append to a rope (wrapping `s = "(" + s + ")"` stays constant time a
// step). A result longer than the string length limit is RangeError:
// Invalid string length, raised before anything is allocated (`s = s + s`
// builds ropes without copying, so the limit must be enforced on the length
// arithmetic itself). A lazy operand is decoded first, so a rope's leaves
// are flat strings or ropes, whose units flattening copies as it finds them.
func (r *Realm) Concat(a, b *String) (*String, error) {
	if a.n == 0 {
		return b, nil
	}
	if b.n == 0 {
		return a, nil
	}
	if a.isLazy() {
		a = a.resolved()
	}
	if b.isLazy() {
		b = b.resolved()
	}
	n := int(a.n) + int(b.n) // both <= MaxStringLength: cannot overflow
	if n > maxStringLength {
		return nil, r.invalidStringLength()
	}
	if n >= ropeThreshold {
		if b.n <= a.n && a.kind != strRope && (a.flags&strInBuf != 0 || b.n < appendPieceMax) {
			return r.appendString(a, b, n), nil
		}
		if lz := r.lazy; lz != nil && lz.mem != nil {
			return r.newChargedRope(a, b, n), nil
		}
		return &String{p: unsafe.Pointer(a), right: b, n: int32(n), kind: strRope}, nil
	}
	// Only a rope Concat did not make is this short (a test's).
	fa, fb := a, b // a and b are returned above, fa and fb never
	if a.kind == strRope {
		fa = a.flat(new(String))
	}
	if b.kind == strRope {
		fb = b.flat(new(String))
	}
	if fa.kind == strASCII && fb.kind == strASCII {
		// n < ropeThreshold <= smallASCIIMax: header and bytes share one
		// allocation.
		s, buf := newASCIIBuf(n)
		copy(buf[copy(buf, fa.s):], fb.s)
		r.chargeString(n)
		return s, nil
	}
	u := make([]uint16, 0, n)
	u = appendUnits(u, fa)
	u = appendUnits(u, fb)
	r.chargeString(2 * n)
	return utf16String(u), nil
}

func appendUnits(u []uint16, s *String) []uint16 {
	if s.kind == strASCII {
		for i := range len(s.s) {
			u = append(u, uint16(s.s[i]))
		}
		return u
	}
	return append(u, s.units()...)
}

// Substring returns the code units in [start, end). Bounds must be valid.
func (s *String) Substring(start, end int) *String {
	if start <= 0 && end >= int(s.n) {
		return s
	}
	if start >= end {
		return emptyString
	}
	if s.needsFlat() {
		return s.ropeSubstring(start, end)
	}
	if s.kind == strASCII {
		return &String{s: s.s[start:end], n: int32(end - start), kind: strASCII, right: s.shareRoot()}
	}
	return s.shareUnits(FromUTF16(s.units()[start:end]))
}

// shareRoot returns the string whose units the flat string s shares, s
// itself when it shares none: what a substring of s keeps alive. It
// returns nil for a string shorter than shareMin, whose substrings count
// their own units (at most shareMin more are kept), and for a string
// FromGo carved from its slab (strHost), whose units are the host's Go
// string: a substring keeps those alone, as before, uncounted.
func (s *String) shareRoot() *String {
	if r := s.right; r != nil {
		return r
	}
	if s.n < shareMin || s.flags&strHost != 0 {
		return nil
	}
	return s
}

// shareMin is the length from which a substring keeps its string as its
// share root: below it, keeping the string's header too would cost more
// than the units the measurement does not see.
const shareMin = 64

// shareUnits records in sub, made of units of s, that it shares them
// (FromUTF16 keeps the units of a string that is not all ASCII).
func (s *String) shareUnits(sub *String) *String {
	if sub.kind == strUTF16 {
		if s.needsFlat() { // a rope, or a lazy string: they count what they decode
			sub.right = s.unflatRoot()
		} else {
			sub.right = s.shareRoot()
		}
	}
	return sub
}

// The rope branches of the hot string operations below are out of line,
// so that the views they take (flat) do not grow the frames of the flat
// paths, which stay as they were before ropes were read through views.

//go:noinline
func (s *String) ropeSubstring(start, end int) *String {
	f := s.flat(new(String))
	if f.kind == strASCII {
		return &String{s: f.s[start:end], n: int32(end - start), kind: strASCII, right: s.unflatRoot()}
	}
	return s.shareUnits(FromUTF16(f.units()[start:end]))
}

// unflatRoot is the share root of a substring of the rope or lazy string s:
// s, which counts what it decodes, but none for a field of a host's
// container, as for a string FromGo carved (strHost, shareRoot).
func (s *String) unflatRoot() *String {
	if s.flags&strHost != 0 {
		return nil
	}
	return s
}

// Hash returns the cached content hash (never 0).
func (s *String) Hash() uint32 {
	if s.isLazy() {
		return s.lazyHash()
	}
	if s.hash != 0 {
		return s.hash
	}
	if s.kind == strRope {
		// A rope keeps no hash: no field of it but flatten's is written
		// once it is shared (Equals reads hash).
		return s.flat(new(String)).contentHash()
	}
	h := s.contentHash()
	s.hash = h
	return h
}

func (s *String) cachedHash() uint32 {
	if s.isLazy() {
		return atomic.LoadUint32(&s.hash)
	}
	return s.hash
}

// contentHash computes the hash of the flat string s (never 0).
func (s *String) contentHash() uint32 {
	h := uint32(2166136261)
	if s.kind == strASCII {
		for i := range len(s.s) {
			h = (h ^ uint32(s.s[i])) * 16777619
		}
	} else {
		for _, c := range s.units() {
			h = (h ^ uint32(c)) * 16777619
		}
	}
	if h == 0 {
		h = 1
	}
	return h
}

// Equals reports code-unit equality.
func (s *String) Equals(t *String) bool {
	if s == t {
		return true
	}
	if s.n != t.n {
		return false
	}
	if s.needsFlat() || t.needsFlat() {
		return unflatEquals(s, t)
	}
	if s.hash != 0 && t.hash != 0 && s.hash != t.hash {
		return false
	}
	return flatEquals(s, t)
}

// unflatEquals is Equals of strings of one length where s or t is a rope or
// lazy: two lazy strings of the same source compare without decoding, and
// a lazy string's hash is read atomically (cachedHash).
//
//go:noinline
func unflatEquals(s, t *String) bool {
	if s.isLazy() && t.isLazy() && s.s == t.s && sameLazyContent(s, t) {
		return true
	}
	if sh, th := s.cachedHash(), t.cachedHash(); sh != 0 && th != 0 && sh != th {
		return false
	}
	return ropeEquals(s, t)
}

//go:noinline
func ropeEquals(s, t *String) bool {
	a, b := s, t
	if s.needsFlat() {
		a = s.flat(new(String))
	}
	if t.needsFlat() {
		b = t.flat(new(String))
	}
	return flatEquals(a, b)
}

// flatEquals is Equals of flat strings of one length.
func flatEquals(s, t *String) bool {
	if s.kind != t.kind {
		return false // utf16 strings always contain a unit >= 0x80
	}
	if s.kind == strASCII {
		return s.s == t.s
	}
	tu := t.units()
	for i, c := range s.units() {
		if tu[i] != c {
			return false
		}
	}
	return true
}

// EqualsGoString compares against an ASCII/UTF-8 Go string.
func (s *String) EqualsGoString(g string) bool {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		return f.s == g
	}
	if isASCII(g) {
		return false
	}
	return utf16ToGoString(f.units()) == g && utf8.ValidString(g)
}

// Compare orders by code units: -1, 0 or 1.
func (s *String) Compare(t *String) int {
	if s == t {
		return 0
	}
	x := s
	if s.needsFlat() {
		x = s.flat(new(String))
	}
	y := t
	if t.needsFlat() {
		y = t.flat(new(String))
	}
	if x.kind == strASCII && y.kind == strASCII {
		return strings.Compare(x.s, y.s)
	}
	n := min(int(x.n), int(y.n))
	for i := range n {
		a, b := x.at(i), y.at(i)
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	switch {
	case x.n < y.n:
		return -1
	case x.n > y.n:
		return 1
	}
	return 0
}

// IndexOf returns the first index >= from at which sub occurs, or -1.
func (s *String) IndexOf(sub *String, from int) int {
	if from < 0 {
		from = 0
	}
	if int(sub.n) == 0 {
		return min(from, int(s.n))
	}
	if from+int(sub.n) > int(s.n) {
		return -1
	}
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	fsub := sub
	if sub.needsFlat() {
		fsub = sub.flat(new(String))
	}
	if f.kind == strASCII && fsub.kind == strASCII {
		i := strings.Index(f.s[from:], fsub.s)
		if i < 0 {
			return -1
		}
		return from + i
	}
	if f.kind == strASCII && fsub.kind == strUTF16 {
		return -1
	}
	first := fsub.at(0)
	last := int(f.n) - int(fsub.n)
	u := f.units()
outer:
	for i := from; i <= last; i++ {
		if u[i] != first {
			continue
		}
		for j := 1; j < int(fsub.n); j++ {
			if u[i+j] != fsub.at(j) {
				continue outer
			}
		}
		return i
	}
	return -1
}

// String implements fmt.Stringer for debugging.
func (s *String) String() string { return s.GoString() }

// IsWellFormed reports whether the string has no lone surrogates.
func (s *String) IsWellFormed() bool {
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		return true
	}
	u := f.units()
	for i := 0; i < len(u); i++ {
		c := u[i]
		if c < 0xD800 || c >= 0xE000 {
			continue
		}
		if c >= 0xDC00 || i+1 >= len(u) || u[i+1] < 0xDC00 || u[i+1] >= 0xE000 {
			return false
		}
		i++
	}
	return true
}

package engine

import (
	"encoding/binary"
	"hash/maphash"
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
	// strLazy and the kinds after it retain immutable source bytes in s (Go
	// text, WTF-8 or JSON contents) and their length in n, counted when
	// they were made; p points to a lazyStringData, which publishes the
	// decoded flat String on demand. The kind is strLazy plus the string's
	// lazyStringMode (lazyMode), so that flags keeps its bits for the flags
	// of every kind.
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
	// strSlice marks a JSON.parse literal of a realm without a memory
	// limit, whose units are a slice of the JSON text with no share root
	// (jsonParser.plain): the key memo does not take it, which would keep
	// the whole text alive (rememberKey).
	strSlice
	// strLazyWide marks a lazy string whose content decodes to UTF-16
	// rather than ASCII.
	strLazyWide
	// strLazyRooted marks a lazy string a parse under a memory limit made:
	// its p points at a rootedLazyData, which holds its share root
	// (string_lazy.go).
	strLazyRooted
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
// they were (flatten), and the caching of the hash (Hash), both of which
// preserve the logical value. Concat writes past the end of a string in an
// append buffer, never into it. A lazy string publishes its decoded String
// in right, once (resolved); its source, kind, n and flags never change.
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
	n     int32 // length in code units; a lazy string's is counted when it is made
	// hash is the content hash, 0 until computed. A string made with it
	// (an atom, a constant) has it before it is shared; any other gets it
	// from its first Hash, which may run on several goroutines at once (a
	// string a host keeps and passes to runtimes running in parallel), so
	// Hash and Equals load and store it atomically.
	hash uint32
	atom uint32 // 0 = not interned
	kind uint8
	// flags holds strInBuf, strCharged, strConst, strHost and strSlice, and
	// a lazy string's strLazyWide and strLazyRooted, set before the String
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
	if len(u) < fromUTF16Joint {
		b := make([]byte, len(u))
		for i, c := range u {
			b[i] = byte(c)
		}
		return &String{s: bytesString(b), n: int32(len(b)), kind: strASCII}
	}
	s, b := newASCIIBuf(len(u)) // past smallASCIIMax, two allocations
	for i, c := range u {
		b[i] = byte(c)
	}
	return s
}

// fromUTF16Joint is the length from which FromUTF16 allocates a short ASCII
// result's header and bytes together: one allocation then takes no more
// bytes than the two, whereas below it the bytes alone take Go's tiny
// allocator (under 16) or a size class (17 to 24) the header's would round
// up past.
const fromUTF16Joint = 25

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
		if s.kind == strRope {
			if g, ok := ropeGoString(s); ok {
				return g
			}
		}
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
	n, hi := utf8Len(u, 0)
	b := make([]byte, 0, n+loneLen(hi))
	if b, hi = appendUTF8(b, u, 0); hi != 0 {
		b = utf8.AppendRune(b, utf8.RuneError)
	}
	return bytesString(b)
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
//
// A short concatenation of two flat ASCII strings in a realm that uses the
// concat memo checks the memo's recent results inline (concatRecent) and
// takes the memo (concatMemoized) without the spills of the other short
// paths, which concatShort takes.
func (r *Realm) Concat(a, b *String) (*String, error) {
	n := int(a.n) + int(b.n) // both <= MaxStringLength: cannot overflow
	if n < ropeThreshold {
		if a.kind == strASCII && b.kind == strASCII && a.n != 0 && b.n != 0 {
			if lz := r.lazy; lz != nil {
				if ms := lz.memos; ms != nil && ms.concat.skip == 0 {
					if g := &ms.recentGate; g.skip == 0 {
						if c := &ms.recent[0]; c.a == a && c.b == b {
							g.hits++
							ms.concat.hits++
							return c.s, nil
						}
						if c := &ms.recent[1]; c.a == a && c.b == b {
							g.hits++
							ms.concat.hits++
							return c.s, nil
						}
						if ms.recent[0].pa|ms.recent[1].pa != 0 {
							if s := ms.recentUnits(a, b, n); s != nil {
								return s, nil
							}
						}
						g.miss()
					} else {
						g.skip--
					}
					return r.concatMemoized(a, b, n), nil
				}
			}
		}
		return r.concatShort(a, b, n), nil
	}
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
	if n > maxStringLength {
		return nil, r.invalidStringLength()
	}
	if b.n <= a.n && a.kind != strRope && (a.flags&strInBuf != 0 || b.n < appendPieceMax) {
		return r.appendString(a, b, n), nil
	}
	if lz := r.lazy; lz != nil && lz.mem != nil {
		return r.newChargedRope(a, b, n), nil
	}
	return &String{p: unsafe.Pointer(a), right: b, n: int32(n), kind: strRope}, nil
}

// concatShort is Concat of a and b, n < ropeThreshold units in all, but for
// its path through the concat memo.
func (r *Realm) concatShort(a, b *String, n int) *String {
	if a.n == 0 {
		return b
	}
	if b.n == 0 {
		return a
	}
	if a.kind == strASCII && b.kind == strASCII {
		if lz := r.lazy; lz != nil {
			if m := lz.memos; m == nil {
				if r.newMemos() != nil {
					return r.concatMemoized(a, b, n)
				}
			} else if m.concat.skip == 0 {
				return r.concatMemoized(a, b, n)
			} else {
				m.concat.skip--
			}
		}
		// n < ropeThreshold <= smallASCIIMax: header and bytes share one
		// allocation.
		s, buf := newASCIIBuf(n)
		copy(buf[copy(buf, a.s):], b.s)
		r.chargeString(n)
		return s
	}
	// Only a rope Concat did not make is this short (a test's); a lazy
	// string is read through its decoding.
	fa, fb := a, b // a and b are returned above, fa and fb never
	if a.needsFlat() {
		fa = a.flat(new(String))
	}
	if b.needsFlat() {
		fb = b.flat(new(String))
	}
	if fa.kind == strASCII && fb.kind == strASCII {
		s, buf := newASCIIBuf(n)
		copy(buf[copy(buf, fa.s):], fb.s)
		r.chargeString(n)
		return s
	}
	// Header and units share one allocation.
	s, u := newUTF16Buf(n)
	appendUnits(appendUnits(u[:0], fa), fb)
	r.chargeString(2 * n)
	return s
}

// concatMemoized is Concat of the flat ASCII a and b, n < ropeThreshold
// units in all, when the realm's concat memo is in use, past the check of
// the recent results: the result the memo holds, or a flat copy it then
// holds.
func (r *Realm) concatMemoized(a, b *String, n int) *String {
	ms := r.lazy.memos
	var t [ropeThreshold]byte
	k := t[:n]
	copy(k[copy(k, a.s):], b.s)
	h := concatHash(&t, n)
	set := &ms.concats[h>>(64-concatMemoBits)]
	tag := uint16(h >> 16)
	for i, s := range &set.s {
		if set.tag[i] == tag && s != nil && s.s == string(k) {
			ms.concat.hits++
			if ms.recentGate.skip == 0 {
				ms.keepRecent(a, b, s)
			}
			return s
		}
	}
	s, buf := newASCIIBuf(n)
	copy(buf, k)
	r.chargeString(n)
	i := ms.concat.misses % concatMemoWays
	set.tag[i], set.s[i] = tag, s
	ms.concat.miss()
	if ms.recentGate.skip == 0 {
		ms.keepRecent(a, b, s)
	}
	return s
}

// concatMemo maps the contents of short ASCII concatenations to a result
// made for them: a server-side render builds the same class names, keys,
// attributes and paths from the same pieces at every render, each into a
// new string. A concatenation of fewer than ropeThreshold units looks in
// one set of concatMemoWays results, chosen by a hash of its units, for
// one with those units, and returns it: a concatenation whose result is in
// the set allocates nothing. Each result's tag, 16 more bits of its hash,
// spares a concatenation the comparison of units with the results it
// cannot be. A new result goes in the way the count of misses picks. A
// realm whose concatenations seldom repeat skips the memo (memoGate).
//
// It holds only results, flat strings Concat made (newASCIIBuf) that keep
// nothing alive but their own 112 bytes at most and are never appended to
// in place (strInBuf): the memo keeps at most concatMemoSets*concatMemoWays
// of them, 56 KiB, and the two it returned last with their pieces
// (realmMemos.recent), besides its own 5 KiB (memo.go).
type concatMemo [concatMemoSets]concatMemoSet

// concatRecent is one of the last two results the concat memo returned
// (realmMemos.recent) and the pieces it was made of then. A run of the same
// concatenations, as of a constant and a key that a tree of leaves
// repeats, has the same pieces: a concatenation checks them first, before
// it copies and hashes its own. An entry keeps its pieces when both keep
// nothing else alive (ownsUnits), so that the two entries hold six strings
// of at most 112 bytes each: then the same pieces are the same units, as a
// String's never change, and a hit compares no units. Otherwise it keeps
// the addresses of the pieces' units, as of a host's string or a slice of
// a JSON text, which keep nothing alive: a hit then compares the units
// too, as a new piece may reuse an address (recentUnits). A realm whose
// concatenations seldom repeat their pieces skips the check, and stops
// keeping recent results, as the memo's gate skips the memo
// (realmMemos.recentGate).
type concatRecent struct {
	a, b, s *String
	pa, pb  uintptr // the addresses of the units of a and b, when a and b are nil
}

// keepRecent makes s, the result of the pieces a and b, a recent result,
// in place of the one kept before the last.
func (ms *realmMemos) keepRecent(a, b, s *String) {
	c := &ms.recent[ms.recentNext&1]
	ms.recentNext++
	if ownsUnits(a) && ownsUnits(b) {
		*c = concatRecent{a: a, b: b, s: s}
		return
	}
	*c = concatRecent{s: s, pa: unitsAddr(a), pb: unitsAddr(b)}
}

// recentUnits is Concat's check of the recent results kept with the
// addresses of their pieces' units: the one whose pieces' units were at
// those of a and b, with their units, n in all, counted as the memo's hit,
// or nil. It stays out of line, as only realms that concatenate such
// pieces call it.
//
//go:noinline
func (ms *realmMemos) recentUnits(a, b *String, n int) *String {
	pa, pb := unitsAddr(a), unitsAddr(b)
	for i := range ms.recent {
		c := &ms.recent[i]
		if c.pa != pa || c.pb != pb {
			continue
		}
		if s := c.s; len(s.s) == n && s.s[:len(a.s)] == a.s && s.s[len(a.s):] == b.s {
			ms.recentGate.hits++
			ms.concat.hits++
			return s
		}
	}
	return nil
}

type concatMemoSet struct {
	tag [concatMemoWays]uint16
	s   [concatMemoWays]*String
}

// At 512 results 90 and 70 % of the bytes the short ASCII concatenations
// of two server-side rendering workloads allocate are saved, at 1024 93
// and 75 %. Neither ever skips the memo; a third, which hits 4 % of the
// time, skips it after its first window.
const (
	concatMemoBits = 7
	concatMemoSets = 1 << concatMemoBits
	concatMemoWays = 4
)

// concatHash hashes the n units in t, n < ropeThreshold and the rest zero,
// eight at a time, seeded per process: the top bits pick the memo's set,
// bits 16 to 31 the tag. Strings that collide only make the memo miss, as
// it misses strings it has not seen, so the hash needs to spread a
// render's strings, not to resist an input chosen to collide.
func concatHash(t *[ropeThreshold]byte, n int) uint64 {
	h := concatSeed ^ uint64(n)
	for i := 0; i < n; i += 8 {
		// i&(ropeThreshold-8) is i, which the compiler cannot see.
		h = (h ^ binary.LittleEndian.Uint64(t[i&(ropeThreshold-8):])) * 0x9e3779b97f4a7c15
	}
	return (h ^ h>>29) * 0xbf58476d1ce4e5b9
}

// concatSeed seeds concatHash, per process.
var concatSeed = maphash.String(maphash.MakeSeed(), "")

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

// ropeSubstring is Substring of a rope. A range within one child, down to
// ropeSliceDepth levels, is not flattened for: a range that covers the
// child is the child, and one within a flat child that holds its own units
// is the child's substring, which keeps the child's units alive, no more
// than the rope's flat copy would. A child that shares units it does not
// hold alone, a substring, a JSON.parse value, a host's string or a string
// in an append buffer, would keep all of those for a part of it: that
// range, as one across children or deeper, as in a rope appends made, is
// read from a flat copy of the rope it is in, and the slices after the
// first of it are cheap. A lazy s is read from its decoding.
//
//go:noinline
func (s *String) ropeSubstring(start, end int) *String {
	for d := 0; d < ropeSliceDepth && s.kind == strRope; d++ {
		l, rt := s.children()
		if l == nil {
			break
		}
		c, i, j := l, start, end
		if n := int(l.n); start >= n {
			c, i, j = rt, start-n, end-n
		} else if end > n {
			break
		}
		if i <= 0 && j >= int(c.n) {
			return c
		}
		if c.kind != strRope {
			if c.right != nil || c.flags&(strHost|strInBuf) != 0 {
				break
			}
			return c.Substring(i, j)
		}
		s, start, end = c, i, j
	}
	f := s.flat(new(String))
	if f.kind == strASCII {
		return &String{s: f.s[start:end], n: int32(end - start), kind: strASCII, right: s.unflatRoot()}
	}
	return s.shareUnits(FromUTF16(f.units()[start:end]))
}

// ropeSliceDepth bounds the levels of a rope ropeSubstring descends.
const ropeSliceDepth = 4

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
	if h := atomic.LoadUint32(&s.hash); h != 0 {
		return h
	}
	if s.needsFlat() {
		if s.isLazy() {
			return s.lazyHash()
		}
		// A rope keeps no hash: no field of it but flatten's is written
		// once it is shared.
		return s.flat(new(String)).contentHash()
	}
	h := s.contentHash()
	atomic.StoreUint32(&s.hash, h)
	return h
}

// contentHash computes the hash of the flat string s (never 0).
func (s *String) contentHash() uint32 {
	if s.kind == strASCII {
		return strHash32(strHash64(s.s))
	}
	return strHash32(maphash.Bytes(strSeed, utf16Bytes(s.units())))
}

// Equals reports code-unit equality.
func (s *String) Equals(t *String) bool {
	if s == t {
		return true
	}
	// Interned strings of one content are one String (Intern).
	if s.n != t.n || s.atom != 0 && t.atom != 0 {
		return false
	}
	if hs, ht := atomic.LoadUint32(&s.hash), atomic.LoadUint32(&t.hash); hs != 0 && ht != 0 && hs != ht {
		return false
	}
	if s.needsFlat() || t.needsFlat() {
		return unflatEquals(s, t)
	}
	return flatEquals(s, t)
}

// unflatEquals is Equals of strings of one length where s or t is a rope or
// lazy, past the comparison of hashes: two lazy strings of the same source
// compare without decoding.
//
//go:noinline
func unflatEquals(s, t *String) bool {
	if s.isLazy() && t.isLazy() && s.s == t.s && sameLazyContent(s, t) {
		return true
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

// IndexOf returns the first index >= from at which sub occurs, or -1. A
// rope is searched in its leaves (ropeIndexOf), or else flattened.
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
	if s.kind == strRope {
		if i, ok := ropeIndexOf(s, sub, from); ok {
			return i
		}
	}
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	fsub := sub
	if sub.needsFlat() {
		fsub = sub.flat(new(String))
	}
	return flatIndexOf(f, fsub, from)
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

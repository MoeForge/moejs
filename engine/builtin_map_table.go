package engine

import (
	"math/bits"
	"math/rand/v2"
	"sort"
	"unsafe"
)

// The ordered hash table behind Map and Set (ES2025 §24.1, §24.2).
//
// The spec's [[MapData]]/[[SetData]] is a List in insertion order in which
// deleting an entry leaves an empty slot, and iterators walk it by index, so
// they see entries added during iteration and skip deleted ones. collTable
// is that list (entries, tombstones keyed by the hole marker) plus chained
// hash buckets over it for SameValueZero lookups.
//
// An insertion into a full list without holes grows it in place: the list
// takes a segment as large as itself and only the buckets are made anew, so
// a table filled from empty allocates its entries once, and no entry moves.
// Memory stays proportional to the live entries: an insertion into a full
// list with holes and a deletion that leaves it one-eighth full rebuild it
// compacted (and resized). A rebuild of a table no cursor has seen fills it
// again in place. Otherwise it replaces the table: since a cursor (an
// iterator or a running forEach) may still point into the old one, the old
// table keeps the ascending positions of the tombstones the compaction
// dropped, or records that clear() emptied it, and links to its successor.
// A cursor translates its position lazily on its next step: every dropped
// tombstone before it moves it back one slot. A table no cursor has seen
// records nothing (the common case: build a map, then read it). A cursor
// that has seen the end stays done, as the spec's iterator generators
// complete.
//
// A suspended iterator keeps its table and the successors' tombstone lists
// alive, so memory held by it is proportional to the deletions compacted
// since it last stepped (the same trade-off as V8's OrderedHashTable).

// collEntry is one slot of the insertion-ordered list.
type collEntry struct {
	key, value Value
	hash       uint32
	chain      int32 // next entry of the bucket + 1; 0 ends the chain
}

type collTable struct {
	entries []collEntry // the list's first segment, in insertion order; a deleted entry's key is the hole
	more    *collMore   // the segments the list grew by in place
	buckets []int32     // first entry of each bucket + 1; 0 = empty
	live    int

	// Rebuild bookkeeping for cursors (see above).
	next     *collTable
	removed  []int32 // ascending positions of the tombstones dropped by the rebuild
	cleared  bool
	iterated bool // a cursor has pointed at this table
}

// collMore holds the segments a list grew by in place. Each follows the
// entries before it and is as large as all of them, so that entry i past
// the first segment, whose capacity is a power of two, is at
// i - 1<<(bits.Len(i)-1) in segs[bits.Len(i)-1-log2(cap(entries))].
type collMore struct {
	segs   [][]collEntry
	inline [4][]collEntry // segs until a fifth
}

// collection is the [[MapData]]/[[SetData]] of a Map or Set object.
type collection struct {
	t  *collTable
	t0 collTable // the first table, co-allocated with the object
}

func (c *collection) table() *collTable {
	if c.t == nil {
		c.t = &c.t0
	}
	return c.t
}

// size is the number of live entries.
func (c *collection) size() int {
	if c.t == nil {
		return 0
	}
	return c.t.live
}

// collSeed64 keys the process-wide hash of bigints, numbers and identities;
// strings hash with strSeed (String.Hash): keys from untrusted input cannot
// be chosen to collide.
var collSeed64 = rand.Uint64() | 1

// negZeroBits is the bit pattern of -0.
const negZeroBits = uint64(1) << 63

// canonicalCollKey implements CanonicalizeKeyedCollectionKey: -0 becomes +0.
func canonicalCollKey(v Value) Value {
	if v.IsNumber() && v.bits == negZeroBits {
		return IntValue(0)
	}
	return v
}

// mix64 is the murmur3 finalizer.
func mix64(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb33fe1a85ec9
	x ^= x >> 33
	return x
}

// collHash hashes v consistently with SameValueZero: strings and bigints by
// content, objects and symbols by identity, numbers by value (-0 as +0; NaN
// is canonical).
func collHash(v Value) uint32 {
	var h uint64
	switch {
	case v.IsString():
		// Cached in the String: an atom, a key or a string hashed before
		// costs no hashing.
		return v.AsString().Hash()
	case v.IsBigInt():
		b := &v.AsBigInt().v
		h = uint64(b.Sign() + 2)
		for _, w := range b.Bits() {
			h = mix64(h ^ uint64(w) ^ collSeed64)
		}
	case v.IsObject() || v.IsSymbol():
		h = mix64(v.identityBits() ^ collSeed64)
	default:
		bits := v.bits
		if bits == negZeroBits {
			bits = 0
		}
		h = mix64(bits ^ collSeed64)
	}
	return uint32(h ^ h>>32)
}

// sameCollKey is SameValueZero specialised to a stored key (never -0).
func sameCollKey(stored, key Value) bool {
	if stored.bits == key.bits && stored.ptrEqual(key) {
		return true
	}
	if stored.IsNumber() {
		// Only NaN and ±0 compare equal with different payloads; the key
		// may be -0 in a lookup, the stored key never is, NaN is canonical.
		return key.IsNumber() && key.bits == negZeroBits && stored.bits == 0
	}
	return (stored.IsString() || stored.IsBigInt()) && sameNonNumber(stored, key)
}

// at returns entry i of the list.
func (t *collTable) at(i int) *collEntry {
	if uint(i) < uint(len(t.entries)) {
		return &t.entries[i]
	}
	return t.atMore(i)
}

// atMore returns entry i of the list, past the first segment.
func (t *collTable) atMore(i int) *collEntry {
	l := bits.Len(uint(i))
	s := t.more.segs[l-1-bits.TrailingZeros(uint(cap(t.entries)))]
	return &s[i-1<<(l-1)]
}

// length is the number of the list's entries, holes included.
func (t *collTable) length() int {
	if t.more == nil {
		return len(t.entries)
	}
	segs := t.more.segs
	return cap(t.entries)<<(len(segs)-1) + len(segs[len(segs)-1])
}

// capacity is the number of entries the list has room for.
func (t *collTable) capacity() int {
	if t.more == nil {
		return cap(t.entries)
	}
	return cap(t.entries) << len(t.more.segs)
}

// segments returns the list's segments, the first included, appended to
// buf.
func (t *collTable) segments(buf [][]collEntry) [][]collEntry {
	buf = append(buf, t.entries)
	if t.more != nil {
		buf = append(buf, t.more.segs...)
	}
	return buf
}

// find returns the position of key's entry, or -1.
func (t *collTable) find(key Value, h uint32) int {
	if len(t.buckets) == 0 {
		return -1
	}
	for i := t.buckets[h&uint32(len(t.buckets)-1)]; i != 0; {
		e := t.at(int(i - 1))
		if e.hash == h && sameCollKey(e.key, key) {
			return int(i - 1)
		}
		i = e.chain
	}
	return -1
}

// full reports whether the list has no room for another entry.
func (t *collTable) full() bool {
	if t.more == nil {
		return len(t.entries) == cap(t.entries)
	}
	s := t.more.segs[len(t.more.segs)-1]
	return len(s) == cap(s)
}

// insert appends a new entry; the table has room.
func (t *collTable) insert(key, value Value, h uint32) {
	b := &t.buckets[h&uint32(len(t.buckets)-1)]
	e := collEntry{key: key, value: value, hash: h, chain: *b}
	if t.more == nil {
		t.entries = append(t.entries, e)
		*b = int32(len(t.entries))
	} else {
		segs := t.more.segs
		s := &segs[len(segs)-1]
		*s = append(*s, e)
		*b = int32(cap(t.entries)<<(len(segs)-1) + len(*s))
	}
	t.live++
}

// get returns the value stored under key.
func (c *collection) get(key Value) (Value, bool) {
	if c.t == nil {
		return Undefined(), false
	}
	if i := c.t.find(key, collHash(key)); i >= 0 {
		return c.t.at(i).value, true
	}
	return Undefined(), false
}

// has reports whether key is present.
func (c *collection) has(key Value) bool {
	return c.t != nil && c.t.find(key, collHash(key)) >= 0
}

// set stores value under key (a Map's set; a Set passes Undefined),
// charging a grown table to r's memory limit.
func (c *collection) set(r *Realm, key, value Value) {
	key = canonicalCollKey(key)
	t := c.table()
	h := collHash(key)
	if i := t.find(key, h); i >= 0 {
		t.at(i).value = value
		return
	}
	if t.full() {
		c.grow(r)
		t = c.t
	}
	t.insert(key, value, h)
}

// add inserts key unless present and reports whether it was added, as set
// charging a grown table.
func (c *collection) add(r *Realm, key Value) bool {
	key = canonicalCollKey(key)
	t := c.table()
	h := collHash(key)
	if t.find(key, h) >= 0 {
		return false
	}
	if t.full() {
		c.grow(r)
		t = c.t
	}
	t.insert(key, Undefined(), h)
	return true
}

// delete removes key and reports whether it was present.
func (c *collection) delete(key Value) bool {
	t := c.t
	if t == nil || len(t.buckets) == 0 {
		return false
	}
	h := collHash(key)
	link := &t.buckets[h&uint32(len(t.buckets)-1)]
	for *link != 0 {
		e := t.at(int(*link - 1))
		if e.hash == h && sameCollKey(e.key, key) {
			*link = e.chain
			*e = collEntry{key: Hole(), value: Undefined()}
			t.live--
			if n := t.capacity(); n >= 32 && t.live <= n/8 {
				c.rebuild(nil, collCapFor(t.live))
			}
			return true
		}
		link = &e.chain
	}
	return false
}

// clear removes every entry.
func (c *collection) clear() {
	t := c.t
	if t == nil || len(t.entries) == 0 {
		return
	}
	if !t.iterated {
		*t = collTable{}
		return
	}
	nt := &collTable{iterated: true}
	t.cleared, t.next = true, nt
	t.entries, t.more, t.buckets = nil, nil, nil
	c.t = nt
}

// collTableSize is the size of the storage of a table of capacity n.
func collTableSize(n int) int {
	return n * int(unsafe.Sizeof(collEntry{})+unsafe.Sizeof(int32(0)))
}

// collCapFor returns the capacity of a rebuilt table holding live entries:
// a power of two with room for half as many insertions again.
func collCapFor(live int) int {
	n := 8
	for n < live+live/2+1 {
		n <<= 1
	}
	return n
}

// grow makes room for another entry in a full list, charged to r's memory
// limit unless r is nil: a list without holes takes a segment as large as
// itself, one with holes is rebuilt.
func (c *collection) grow(r *Realm) {
	t := c.t
	n := t.length()
	if n == 0 || t.live < n {
		c.rebuild(r, collCapFor(t.live+1))
		return
	}
	g := n * int(unsafe.Sizeof(collEntry{})) // the segment
	m := t.more
	if m == nil {
		m = &collMore{}
		m.segs = m.inline[:0]
		t.more = m
		g += int(unsafe.Sizeof(collMore{}))
	}
	k := cap(m.segs)
	m.segs = append(m.segs, make([]collEntry, 0, n))
	if cap(m.segs) != k {
		g += cap(m.segs) * int(unsafe.Sizeof([]collEntry(nil)))
	}
	if r != nil {
		r.charge(g + 4*t.capacity())
	}
	t.rehash(make([]int32, t.capacity()))
}

// rehash links every entry of the list into the buckets b, which are
// empty, and makes them the table's.
func (t *collTable) rehash(b []int32) {
	mask := uint32(len(b) - 1)
	i := int32(0)
	var buf [8][]collEntry
	for _, s := range t.segments(buf[:0]) {
		for j := range s {
			if e := &s[j]; !e.key.IsHole() {
				h := &b[e.hash&mask]
				e.chain, *h = *h, i+1
			}
			i++
		}
	}
	t.buckets = b
}

// rebuild fills the table again compacted, at capacity n, charged to r's
// memory limit unless r is nil (a shrink): in place when no cursor has seen
// it, else in a new table the old one links to.
func (c *collection) rebuild(r *Realm, n int) {
	if r != nil {
		r.charge(collTableSize(n))
	}
	old := c.t
	entries := make([]collEntry, 0, n)
	var removed []int32
	var buf [8][]collEntry
	i := int32(0)
	for _, s := range old.segments(buf[:0]) {
		for j := range s {
			if e := &s[j]; !e.key.IsHole() {
				entries = append(entries, *e)
			} else if old.iterated {
				removed = append(removed, i)
			}
			i++
		}
	}
	t := old
	if old.iterated {
		t = &collTable{iterated: true}
		old.removed, old.next = removed, t
		old.entries, old.more, old.buckets = nil, nil, nil
		c.t = t
	}
	t.entries, t.more, t.live = entries, nil, len(entries)
	t.rehash(make([]int32, n))
}

// copyFrom fills an empty collection with src's live entries.
func (c *collection) copyFrom(r *Realm, src *collection) {
	if src.size() == 0 {
		return
	}
	t := c.table()
	r.charge(collTableSize(collCapFor(src.t.live)))
	t.entries = make([]collEntry, 0, collCapFor(src.t.live))
	t.buckets = make([]int32, cap(t.entries))
	var buf [8][]collEntry
	for _, s := range src.t.segments(buf[:0]) {
		for i := range s {
			if e := &s[i]; !e.key.IsHole() {
				t.insert(e.key, e.value, e.hash)
			}
		}
	}
}

// collCursor is a position in a collection's list, stable across rebuilds.
type collCursor struct {
	t *collTable // nil once done
	i int
}

// cursor returns a cursor at the first entry.
func (c *collection) cursor() collCursor {
	t := c.table()
	t.iterated = true
	return collCursor{t: t}
}

// next returns the next live entry, or ok false once the list is exhausted
// (the cursor then stays done).
func (cur *collCursor) next() (key, value Value, ok bool) {
	t := cur.t
	if t == nil {
		return Undefined(), Undefined(), false
	}
	for t.next != nil {
		if t.cleared {
			cur.i = 0
		} else {
			i := cur.i
			cur.i -= sort.Search(len(t.removed), func(k int) bool { return int(t.removed[k]) >= i })
		}
		t = t.next
	}
	for n := t.length(); cur.i < n; {
		e := t.at(cur.i)
		cur.i++
		if !e.key.IsHole() {
			cur.t = t
			return e.key, e.value, true
		}
	}
	cur.t = nil
	return Undefined(), Undefined(), false
}

package engine

import (
	"hash/maphash"
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
// Memory stays proportional to the live entries: an insertion into a full
// table and a deletion that leaves it one-eighth full rebuild it compacted
// (and resized). A rebuild replaces the table; when a cursor (an iterator or
// a running forEach) may still point into the old one, the old table keeps
// the ascending positions of the tombstones the compaction dropped, or
// records that clear() emptied it, and links to its successor. A cursor
// translates its position lazily on its next step: every dropped tombstone
// before it moves it back one slot. A table no cursor has seen records
// nothing (the common case: build a map, then read it). A cursor that has
// seen the end stays done, as the spec's iterator generators complete.
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
	entries []collEntry // insertion order; a deleted entry's key is the hole
	buckets []int32     // first entry of each bucket + 1; 0 = empty
	live    int

	// Rebuild bookkeeping for cursors (see above).
	next     *collTable
	removed  []int32 // ascending positions of the tombstones dropped by the rebuild
	cleared  bool
	iterated bool // a cursor has pointed at this table
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

// collSeed keys the process-wide hash: strings from untrusted input cannot
// be chosen to collide.
var (
	collSeed   = maphash.MakeSeed()
	collSeed64 = rand.Uint64() | 1
)

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
		s := v.AsString()
		f := s
		if s.needsFlat() {
			f = s.flat(new(String))
		}
		if f.kind == strASCII {
			h = maphash.String(collSeed, f.s)
		} else {
			h = maphash.Bytes(collSeed, utf16Bytes(f.units()))
		}
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

// find returns the position of key's entry, or -1.
func (t *collTable) find(key Value, h uint32) int {
	if len(t.buckets) == 0 {
		return -1
	}
	for i := t.buckets[h&uint32(len(t.buckets)-1)]; i != 0; {
		e := &t.entries[i-1]
		if e.hash == h && sameCollKey(e.key, key) {
			return int(i - 1)
		}
		i = e.chain
	}
	return -1
}

// insert appends a new entry; the table has room.
func (t *collTable) insert(key, value Value, h uint32) {
	b := &t.buckets[h&uint32(len(t.buckets)-1)]
	t.entries = append(t.entries, collEntry{key: key, value: value, hash: h, chain: *b})
	*b = int32(len(t.entries))
	t.live++
}

// get returns the value stored under key.
func (c *collection) get(key Value) (Value, bool) {
	if c.t == nil {
		return Undefined(), false
	}
	if i := c.t.find(key, collHash(key)); i >= 0 {
		return c.t.entries[i].value, true
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
		t.entries[i].value = value
		return
	}
	if len(t.entries) == cap(t.entries) {
		c.rebuild(r, collCapFor(t.live+1))
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
	if len(t.entries) == cap(t.entries) {
		c.rebuild(r, collCapFor(t.live+1))
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
		e := &t.entries[*link-1]
		if e.hash == h && sameCollKey(e.key, key) {
			*link = e.chain
			*e = collEntry{key: Hole(), value: Undefined()}
			t.live--
			if n := cap(t.entries); n >= 32 && t.live <= n/8 {
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
	t.entries, t.buckets = nil, nil
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

// rebuild replaces the table with a compacted one of capacity n, charged
// to r's memory limit unless r is nil (a shrink).
func (c *collection) rebuild(r *Realm, n int) {
	if r != nil {
		r.charge(collTableSize(n))
	}
	old := c.t
	t := &collTable{
		entries:  make([]collEntry, 0, n),
		buckets:  make([]int32, n),
		iterated: old.iterated,
	}
	var removed []int32
	for i := range old.entries {
		e := &old.entries[i]
		if e.key.IsHole() {
			if old.iterated {
				removed = append(removed, int32(i))
			}
			continue
		}
		t.insert(e.key, e.value, e.hash)
	}
	if old.iterated {
		old.removed, old.next = removed, t
	}
	old.entries, old.buckets = nil, nil
	c.t = t
}

// copyFrom fills an empty collection with src's live entries.
func (c *collection) copyFrom(r *Realm, src *collection) {
	if src.size() == 0 {
		return
	}
	s := src.t
	t := c.table()
	r.charge(collTableSize(collCapFor(s.live)))
	t.entries = make([]collEntry, 0, collCapFor(s.live))
	t.buckets = make([]int32, cap(t.entries))
	for i := range s.entries {
		if e := &s.entries[i]; !e.key.IsHole() {
			t.insert(e.key, e.value, e.hash)
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
	for cur.i < len(t.entries) {
		e := &t.entries[cur.i]
		cur.i++
		if !e.key.IsHole() {
			cur.t = t
			return e.key, e.value, true
		}
	}
	cur.t = nil
	return Undefined(), Undefined(), false
}

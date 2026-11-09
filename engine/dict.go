package engine

import (
	"math/bits"
	"slices"
	"unsafe"
)

// propCell is a property's storage: a data value or an accessorValue when
// attrs has attrAccessor.
type propCell struct {
	value Value
	attrs uint8
}

// Accessor is the getter/setter pair of an accessor property. Either may be
// nil (undefined).
type Accessor struct {
	Get *Object
	Set *Object
}

// dictEntry is one insertion-ordered named property in dictionary mode. A
// deleted entry is the zero entry: a named key, a string, a symbol or a
// private name, has a pointer, where the zero key is the index 0.
type dictEntry struct {
	key  PropertyKey
	cell propCell
}

// live reports whether the entry holds a property.
func (e *dictEntry) live() bool { return e.key.v.ptr != nil }

// dictProps backs objects in dictionary mode (named properties) and/or with
// sparse indexed properties, holds the weak-collection entries of an object
// used as a WeakMap/WeakSet key, and the root shape of the objects whose
// prototype the object is.
type dictProps struct {
	index   *dictIndex          // named key -> position in entries, past dictLinear entries
	entries []dictEntry         // insertion order with tombstones
	sparse  map[uint32]propCell // indexed properties outside dense storage
	weak    *weakSide           // entries of the weak collections keyed by the object (builtin_weak.go)
}

// dictLinear is the most entries a lookup scans, without an index.
const dictLinear = 8

// dictIndex finds a key's entry among more than dictLinear: an open
// addressing table, probed linearly from the slot the top bits of the key's
// hash select, of entry positions plus one (zero is an empty slot) in the
// low bits of a slot, below len(slots), and the hash's low bits above them,
// which spare a probe the entry's key when they differ. The table has room
// for half as many entries again as the entries' capacity, rounded up to a
// power of two, and is rebuilt when they grow or a compaction shrinks them:
// it is at most two-thirds full.
type dictIndex struct {
	slots []uint32
	shift uint8 // 64 - log2(len(slots))
	dead  int32 // deleted entries left in entries
}

// dictIndexSize is what an index costs besides its slots.
const dictIndexSize = int(unsafe.Sizeof(dictIndex{}))

// dictHash hashes a named key by its identity: the strings of keys are
// atoms. The top bits pick the slot and the low bits tag it.
func dictHash(key PropertyKey) uint64 {
	h := (key.v.identityBits() ^ collSeed64) * 0x9E3779B97F4A7C15
	return h ^ h>>32
}

// find returns the slot holding key's entry, the slot it would go in when it
// has none, and its position in entries, or -1 when it has none.
func (x *dictIndex) find(entries []dictEntry, key PropertyKey) (uint32, int) {
	h := dictHash(key)
	mask := uint32(len(x.slots) - 1)
	tag := uint32(h) &^ mask
	for i := uint32(h >> x.shift); ; i = (i + 1) & mask {
		s := x.slots[i&mask]
		if s == 0 {
			return i, -1
		}
		if s&^mask == tag {
			if p := int(s&mask) - 1; entries[p].key == key {
				return i, p
			}
		}
	}
}

// insert records that entries[p] holds key, which has no slot.
func (x *dictIndex) insert(key PropertyKey, p int) {
	h := dictHash(key)
	mask := uint32(len(x.slots) - 1)
	i := uint32(h >> x.shift)
	for x.slots[i&mask] != 0 {
		i = (i + 1) & mask
	}
	x.slots[i&mask] = uint32(h)&^mask | uint32(p+1)
}

// remove empties slot j, moving back into it the slots after it that probe
// past it, so that no probe stops early (deletion without tombstones).
func (x *dictIndex) remove(entries []dictEntry, j uint32) {
	mask := uint32(len(x.slots) - 1)
	for i := (j + 1) & mask; ; i = (i + 1) & mask {
		s := x.slots[i&mask]
		if s == 0 {
			break
		}
		home := uint32(dictHash(entries[s&mask-1].key) >> x.shift)
		if (i-home)&mask >= (i-j)&mask {
			x.slots[j&mask] = s
			j = i
		}
	}
	x.slots[j&mask] = 0
}

func (d *dictProps) lookup(key PropertyKey) (*propCell, bool) {
	if x := d.index; x != nil {
		if _, p := x.find(d.entries, key); p >= 0 {
			return &d.entries[p].cell, true
		}
		return nil, false
	}
	for i := range d.entries {
		if d.entries[i].key == key {
			return &d.entries[i].cell, true
		}
	}
	return nil, false
}

// add appends key's entry, which d has none of, and returns the bytes it
// allocated: the entries double when full, and their index follows.
func (d *dictProps) add(key PropertyKey, cell propCell) int {
	n := 0
	if len(d.entries) == cap(d.entries) {
		d.entries = slices.Grow(d.entries, max(len(d.entries), 4))
		n = cap(d.entries) * int(unsafe.Sizeof(dictEntry{}))
		if d.index != nil {
			d.entries = append(d.entries, dictEntry{key: key, cell: cell})
			return n + d.reindex()
		}
	}
	d.entries = append(d.entries, dictEntry{key: key, cell: cell})
	if d.index != nil {
		d.index.insert(key, len(d.entries)-1)
	} else if len(d.entries) > dictLinear {
		n += d.reindex()
	}
	return n
}

// remove deletes key's entry and reports whether d had one, and returns the
// bytes it allocated: the entries are compacted when more of them are
// deleted than live, which can move them to a smaller array.
func (d *dictProps) remove(key PropertyKey) (bool, int) {
	p := -1
	if x := d.index; x != nil {
		var j uint32
		if j, p = x.find(d.entries, key); p < 0 {
			return false, 0
		}
		x.remove(d.entries, j)
		x.dead++
		d.entries[p] = dictEntry{}
		if dead := int(x.dead); dead > 8 && dead > len(d.entries)/2 {
			return true, d.compact()
		}
		return true, 0
	}
	for i := range d.entries {
		if d.entries[i].key == key {
			p = i
			break
		}
	}
	if p < 0 {
		return false, 0
	}
	d.entries[p] = dictEntry{}
	return true, 0
}

// reindex builds the index of the entries, past dictLinear of them, for
// their capacity, and returns the bytes it allocated. Where a key has more
// than one entry, as the properties of a host map whose keys decode to one
// atom can (shadowHostKeys), the last is kept.
func (d *dictProps) reindex() int {
	if len(d.entries) <= dictLinear && d.index == nil {
		for i := range d.entries {
			if k := d.entries[i].key; d.entries[i].live() {
				for j := i + 1; j < len(d.entries); j++ {
					if d.entries[j].key == k {
						d.entries[i] = dictEntry{}
						break
					}
				}
			}
		}
		return 0
	}
	n := 0
	x := d.index
	if x == nil {
		x = new(dictIndex)
		d.index = x
		n = dictIndexSize
	}
	size := 16
	if c := cap(d.entries); c+c/2 > size {
		size = 1 << bits.Len(uint(c+c/2-1))
	}
	if len(x.slots) == size {
		clear(x.slots)
	} else {
		x.slots = make([]uint32, size)
		x.shift = uint8(64 - bits.TrailingZeros(uint(size)))
		n += size * 4
	}
	x.dead = 0
	for p := range d.entries {
		e := &d.entries[p]
		if !e.live() {
			x.dead++
			continue
		}
		if j, q := x.find(d.entries, e.key); q >= 0 {
			// A later entry of the key: it takes the slot.
			mask := uint32(len(x.slots) - 1)
			x.slots[j&mask] = x.slots[j&mask]&^mask | uint32(p+1)
			d.entries[q] = dictEntry{}
			x.dead++
			continue
		}
		x.insert(e.key, p)
	}
	return n
}

// dropShadowed removes the entries added under a key that a later entry
// was added under too, after a build of entries without add.
func (d *dictProps) dropShadowed() int {
	n := d.reindex()
	if d.deadCount() != 0 {
		n += d.compact()
	}
	return n
}

// dictShrinkMin is the least room a compaction moves the entries to when
// it shrinks them. Entries of at most twice that (dictRoomMax) never shrink.
const dictShrinkMin = 2 * maxShapeProps

// compact drops the deleted entries, keeping the order of the others, and
// returns the bytes it allocated. Where the live entries fill less than an
// eighth of the entries' capacity, past twice dictShrinkMin, they move to
// an array with room for three times as many, at least dictShrinkMin, and
// the index is rebuilt for it: a compaction of an object that once held
// many more keys costs what the object holds, not what it held, and the
// rest of its memory goes. A compaction runs once more entries are deleted
// than live, at twice the live entries, which room for three times them
// holds: additions and deletions around a size neither grow nor shrink the
// entries again until the live ones pass half as many again, or fall below
// three eighths.
func (d *dictProps) compact() int {
	n := 0
	live := d.entries[:0]
	if c := cap(d.entries); c > 2*dictShrinkMin && c > 8*d.namedCount() {
		live = slices.Grow([]dictEntry(nil), max(3*d.namedCount(), dictShrinkMin))
		n = cap(live) * int(unsafe.Sizeof(dictEntry{}))
	}
	for _, e := range d.entries {
		if e.live() {
			live = append(live, e)
		}
	}
	if n == 0 {
		clear(d.entries[len(live):])
	}
	d.entries = live
	if d.index != nil {
		n += d.reindex()
	}
	return n
}

// deadCount returns the number of deleted entries left in entries.
func (d *dictProps) deadCount() int {
	if d.index != nil {
		return int(d.index.dead)
	}
	n := 0
	for i := range d.entries {
		if !d.entries[i].live() {
			n++
		}
	}
	return n
}

// size returns the bytes of the entries and of their index.
func (d *dictProps) size() int64 {
	n := int64(cap(d.entries)) * int64(unsafe.Sizeof(dictEntry{}))
	if x := d.index; x != nil {
		n += int64(dictIndexSize + 4*len(x.slots))
	}
	return n
}

// namedCount returns the number of live named properties.
func (d *dictProps) namedCount() int { return len(d.entries) - d.deadCount() }

// sparseKeys returns the sparse indices in ascending order.
func (d *dictProps) sparseKeys() []uint32 {
	if len(d.sparse) == 0 {
		return nil
	}
	keys := make([]uint32, 0, len(d.sparse))
	for k := range d.sparse {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

package engine

import "unsafe"

// arrayObject is an array object: the header and its elements
// (inlineObject). Its length is in the header (arrayLen).
type arrayObject struct {
	inlineObject
}

// initArray fills ao as a dense array over items with the realm's array
// shape.
//
// It charges the array and the capacity of items (memlimit.go), which the
// array owns from now on: element storage is charged once, here or where
// it grows.
func (r *Realm) initArray(ao *arrayObject, items []Value, length uint32) *Object {
	r.chargeObject(unsafe.Sizeof(arrayObject{}) + uintptr(cap(items)*valueSize))
	o := ao.initInline()
	o.shape = r.arrayShape()
	o.class = ClassArray
	o.flags = flagExtensible
	o.setElems(items)
	o.setArrayLen(length)
	return o
}

// Small arrays co-allocate their element storage with the object, so an
// array literal, an Object.keys result or a map result of up to
// smallArrayMax elements is one allocation. The buckets land in the 96-,
// 128-, 160- and 192-byte size classes, the same bytes a separate element
// slice would cost.
const smallArrayMax = 8

// Up to coArrayMax elements, the storage of 9, 10, 11, 12, 14 and 16
// elements is co-allocated too: the object then lands in the 208-, 224-,
// 240-, 256-, 288- and 320-byte size classes, again the bytes of the
// object and a separate slice. 13 and 15 would take 16 bytes more and keep
// their slice (coArray).
const coArrayMax = 16

type arrayObject2 struct {
	arrayObject
	buf [2]Value
}

type arrayObject4 struct {
	arrayObject
	buf [4]Value
}

type arrayObject6 struct {
	arrayObject
	buf [6]Value
}

type arrayObject8 struct {
	arrayObject
	buf [smallArrayMax]Value
}

// The odd sizes, for arrays made at their final length (newArrayExact).
type (
	arrayObject1 struct {
		arrayObject
		buf [1]Value
	}
	arrayObject3 struct {
		arrayObject
		buf [3]Value
	}
	arrayObject5 struct {
		arrayObject
		buf [5]Value
	}
	arrayObject7 struct {
		arrayObject
		buf [7]Value
	}
)

// The sizes past smallArrayMax, for both newArrayStorage and newArrayExact.
type (
	arrayObject9 struct {
		arrayObject
		buf [9]Value
	}
	arrayObject10 struct {
		arrayObject
		buf [10]Value
	}
	arrayObject11 struct {
		arrayObject
		buf [11]Value
	}
	arrayObject12 struct {
		arrayObject
		buf [12]Value
	}
	arrayObject14 struct {
		arrayObject
		buf [14]Value
	}
	arrayObject16 struct {
		arrayObject
		buf [coArrayMax]Value
	}
)

// coArray reports whether storage of n elements, n > smallArrayMax, is
// co-allocated with its array (coArrayMax).
func coArray(n int) bool {
	return n <= coArrayMax && n != 13 && n != 15
}

// newArrayStorage returns an array object and element storage of length n
// with capacity for at least c >= n elements, co-allocated when c is small.
func (r *Realm) newArrayStorage(n, c int) (*arrayObject, []Value) {
	switch {
	case c <= 2:
		x := &arrayObject2{}
		return &x.arrayObject, x.buf[:n:2]
	case c <= 4:
		x := &arrayObject4{}
		return &x.arrayObject, x.buf[:n:4]
	case c <= 6:
		x := &arrayObject6{}
		return &x.arrayObject, x.buf[:n:6]
	case c <= smallArrayMax:
		x := &arrayObject8{}
		return &x.arrayObject, x.buf[:n:smallArrayMax]
	case coArray(c):
		ao, buf := newArraySmall(c)
		return ao, buf[:n]
	}
	return &arrayObject{}, make([]Value, n, c)
}

// newArrayExact returns an array object and element storage of length and
// capacity n, co-allocated up to smallArrayMax elements and past it as
// coArray says: the storage of a result whose length is known when it is
// made (a JSON array, a slice, a split, a rest parameter).
func (r *Realm) newArrayExact(n int) (*arrayObject, []Value) {
	if n > coArrayMax {
		return &arrayObject{}, make([]Value, n)
	}
	return newArraySmall(n)
}

// newArraySmall is newArrayExact's co-allocated part, out of line so that
// newArrayExact inlines.
func newArraySmall(n int) (*arrayObject, []Value) {
	switch n {
	case 1:
		x := &arrayObject1{}
		return &x.arrayObject, x.buf[:]
	case 2:
		x := &arrayObject2{}
		return &x.arrayObject, x.buf[:]
	case 3:
		x := &arrayObject3{}
		return &x.arrayObject, x.buf[:]
	case 4:
		x := &arrayObject4{}
		return &x.arrayObject, x.buf[:]
	case 5:
		x := &arrayObject5{}
		return &x.arrayObject, x.buf[:]
	case 6:
		x := &arrayObject6{}
		return &x.arrayObject, x.buf[:]
	case 7:
		x := &arrayObject7{}
		return &x.arrayObject, x.buf[:]
	case 8:
		x := &arrayObject8{}
		return &x.arrayObject, x.buf[:]
	case 9:
		x := &arrayObject9{}
		return &x.arrayObject, x.buf[:]
	case 10:
		x := &arrayObject10{}
		return &x.arrayObject, x.buf[:]
	case 11:
		x := &arrayObject11{}
		return &x.arrayObject, x.buf[:]
	case 12:
		x := &arrayObject12{}
		return &x.arrayObject, x.buf[:]
	case 14:
		x := &arrayObject14{}
		return &x.arrayObject, x.buf[:]
	case 16:
		x := &arrayObject16{}
		return &x.arrayObject, x.buf[:]
	case 13, 15:
		return &arrayObject{}, make([]Value, n)
	}
	return &arrayObject{}, nil
}

// newArrayCopy creates a dense array holding a copy of vals.
func (r *Realm) newArrayCopy(vals []Value) *Object {
	ao, items := r.newArrayExact(len(vals))
	copy(items, vals)
	return r.initArray(ao, items, uint32(len(vals)))
}

// NewArray creates a dense array holding items (the slice is retained).
func (r *Realm) NewArray(items ...Value) *Object {
	return r.NewArrayFromSlice(items)
}

// NewArrayFromSlice creates a dense array that takes ownership of items.
func (r *Realm) NewArrayFromSlice(items []Value) *Object {
	return r.initArray(&arrayObject{}, items, uint32(len(items)))
}

// NewArrayCap creates an empty array with room for n elements (an array
// literal about to be filled by ArrayPush). An empty literal gets room for
// four elements because it is almost always pushed to afterwards.
func (r *Realm) NewArrayCap(n int) *Object {
	if n == 0 {
		n = 4
	}
	ao, items := r.newArrayStorage(0, n)
	return r.initArray(ao, items, 0)
}

// NewArrayLen creates an array of the given length filled with holes; dense
// storage is preallocated only for modest lengths.
func (r *Realm) NewArrayLen(n uint32) *Object {
	if n > denseGrowLimit {
		return r.initArray(&arrayObject{}, nil, n)
	}
	ao, items := r.newArrayStorage(int(n), int(n))
	hole := Hole()
	for i := range items {
		items[i] = hole
	}
	return r.initArray(ao, items, n)
}

// IsArray implements the IsArray abstract operation (there are no proxies yet).
func IsArray(v Value) bool {
	return v.IsObject() && v.AsObject().class == ClassArray
}

// ArrayLength returns the length of an array object (0 for non-arrays).
func (o *Object) ArrayLength() uint32 {
	if o.class != ClassArray {
		return 0
	}
	return o.aux
}

// Elements exposes dense storage for fast iteration; entries may be holes and
// the slice must not be retained across a call into user code. An
// unmaterialized host array (hostlazy.go) is materialized first.
func (o *Object) Elements() []Value {
	o.materializeHost()
	return o.elems()
}

// IsDenseArray reports whether every index < length is present in dense
// storage (no holes, no sparse entries), enabling index-loop fast paths.
func (o *Object) IsDenseArray() bool {
	if o.class != ClassArray {
		return false
	}
	o.materializeHost()
	if int(o.arrayLen()) != o.elemLen() || (o.dictTable() != nil && len(o.dictTable().sparse) != 0) {
		return false
	}
	for _, v := range o.elems() {
		if v.IsHole() {
			return false
		}
	}
	return true
}

// Push appends values to an array as CreateDataProperty does (Array.prototype
// .push semantics when no prototype has indexed properties). It returns
// false when length is not writable or the array is not extensible.
func (o *Object) Push(r *Realm, values ...Value) bool {
	if o.flags&flagHasLazy != 0 {
		o.materializeHost()
	}
	if o.bits&bitLengthReadOnly != 0 || !o.IsExtensible() || o.flags&flagShared != 0 {
		return false
	}
	// Where addIndex would append each value to the dense storage, as it
	// does to an array whose elements all fit in it, the storage grows
	// once for all of them.
	if e := o.elems(); o.class == ClassArray && int(o.aux) == len(e) && len(values) != 0 &&
		(o.bits&bitHasExt == 0 || (*objectExt)(o.ext).sparse == nil) {
		if o.bits&bitInlineElems == 0 {
			o.chargeExt(r)
		}
		if len(e)+len(values) > cap(e) {
			e = r.growValues(e, len(values))
		}
		e = append(e, values...)
		o.setElems(e)
		o.aux = uint32(len(e))
		r.bumpEpoch(o)
		return true
	}
	for _, v := range values {
		o.addIndex(r, o.aux, propCell{value: v, attrs: o.elementAttrs()})
	}
	return true
}

// pushConsts appends the constants of a run of an array literal's elements
// (ArrayPushK) to o, as Push does. Where a constant and the slot it lands
// in hold no pointer, it writes only the value's bits, which takes no
// write barrier: a run of numbers, null and undefined in a literal's fresh
// storage writes no pointer word at all. A string keeps its barrier.
func (o *Object) pushConsts(r *Realm, consts []Value) {
	e := o.elems()
	if o.flags&(flagHasLazy|flagShared) != 0 || o.bits&(bitInlineElems|bitLengthReadOnly|bitHasExt) != bitInlineElems ||
		o.class != ClassArray || int(o.aux) != len(e) || !o.IsExtensible() {
		o.Push(r, consts...)
		return
	}
	if len(e)+len(consts) > cap(e) {
		e = r.growValues(e, len(consts))
	}
	n := len(e)
	e = e[:n+len(consts)]
	copyConsts(e[n:], consts)
	// Storage that did not move only takes its new length.
	if el := o.inlineElems(); el.p == unsafe.SliceData(e) {
		el.n = uint32(len(e))
	} else {
		o.setElems(e)
	}
	o.aux = uint32(len(e))
	r.bumpEpoch(o)
}

// copyConsts copies the constants src over dst as pushConsts writes them:
// only the bits of a value without a pointer over a slot without one.
func copyConsts(dst, src []Value) {
	for i, v := range src {
		if d := &dst[i]; d.ptr == nil && v.ptr == nil {
			d.setScalarBits(v)
		} else {
			*d = v
		}
	}
}

// sharedConstsMin is the length from which an array literal of constants
// shares them (newArrayConsts): its array is then the header alone, where
// a shorter one's copy is one allocation with the header.
const sharedConstsMin = 9

// newArrayConsts creates the array of an array literal whose elements are
// all constants (NewArrayK): ks, a run of its code's constant pool, holds
// numbers, strings, null and undefined. An array shorter than
// sharedConstsMin copies them into storage of its own (copyConsts), with
// the room NewArrayCap gives a literal. A longer one shares them, if share
// is set, (bitSharedElems) until a write, or any path but elemsReadOnly's
// reads, gives it a copy (elems). Either is charged as an array that owns
// its elements, so the copy needs no charge of its own. Dynamic code (eval,
// Function) does not share: a shared array keeps its tree's constant slab,
// whose strings that code charges to the realm, alive after the realm
// drops the tree, and is measured as its own n values only.
func (r *Realm) newArrayConsts(ks []Value, share bool) *Object {
	n := len(ks)
	if n < sharedConstsMin || !share {
		ao, items := r.newArrayStorage(n, n)
		copyConsts(items, ks)
		return r.initArray(ao, items, uint32(n))
	}
	// A capacity equal to the length: no append writes into the constants.
	o := r.initArray(&arrayObject{}, ks[:n:n], uint32(n))
	o.flags |= flagHasLazy
	o.bits |= bitSharedElems
	return o
}

// reserveElems gives the elements of o, an array literal being filled, room
// for n more in one step, where pushing them one at a time would grow the
// storage by doubling it. It reserves only while the elements are the dense
// storage of the whole array, as addIndex then appends to them.
func (o *Object) reserveElems(r *Realm, n int) {
	if e := o.elems(); o.bits&bitInlineElems != 0 && len(e) == int(o.aux) && cap(e)-len(e) < n {
		o.setElems(r.growValues(e, n))
	}
}

// arraySetLength implements ArraySetLength.
func (o *Object) arraySetLength(r *Realm, desc PropertyDescriptor) (bool, error) {
	if !desc.HasValue() {
		cur, _ := o.GetOwnProperty(lengthKey)
		return o.applyLengthAttrs(desc, &cur), nil
	}
	// ArraySetLength converts the value twice (ToUint32, then ToNumber), so a
	// valueOf runs twice and a value that changes between them is rejected.
	newLen, err := r.ToUint32(desc.Value)
	if err != nil {
		return false, err
	}
	numberLen, err := r.ToNumber(desc.Value)
	if err != nil {
		return false, err
	}
	if float64(newLen) != numberLen {
		return false, r.RangeError("Invalid array length")
	}
	// The conversions ran user code, so the old length descriptor is read
	// only now.
	cur, _ := o.GetOwnProperty(lengthKey)
	if newLen >= o.arrayLen() {
		if !o.lengthWritable() && newLen != o.arrayLen() {
			return false, nil
		}
		if !o.applyLengthAttrs(desc, &cur) {
			return false, nil
		}
		o.setArrayLen(newLen)
		return true, nil
	}
	if !o.lengthWritable() {
		return false, nil
	}
	newWritable := !desc.HasWritable() || desc.Writable()
	var check PropertyDescriptor
	check.present = desc.present &^ (descHasValue | descHasWritable)
	check.attrs = desc.attrs
	if !ValidateAndApplyPropertyDescriptor(r, nil, lengthKey, o.IsExtensible(), check, &cur) {
		return false, nil
	}
	ok := o.truncate(r, newLen)
	if !newWritable {
		o.setLengthReadOnly()
	}
	return ok, nil
}

// applyLengthAttrs validates a length descriptor without a value and applies
// a writable -> false change.
func (o *Object) applyLengthAttrs(desc PropertyDescriptor, cur *PropertyDescriptor) bool {
	if !ValidateAndApplyPropertyDescriptor(nil, nil, lengthKey, o.IsExtensible(), desc, cur) {
		return false
	}
	if desc.HasWritable() && !desc.Writable() {
		o.setLengthReadOnly()
	}
	return true
}

// truncate deletes indices >= newLen from the highest down and sets length.
// It stops at the first non-configurable element (setting length just above
// it) and returns false, as ArraySetLength requires.
func (o *Object) truncate(r *Realm, newLen uint32) bool {
	denseConfigurable := o.elementAttrs()&attrConfigurable != 0
	var sparse []uint32
	if o.dictTable() != nil {
		sparse = o.dictTable().sparseKeys()
	}
	for i := len(sparse) - 1; i >= 0; i-- {
		k := sparse[i]
		if k < newLen {
			break
		}
		if stop, ok := o.dropDense(int(k)+1, denseConfigurable); !ok {
			o.setArrayLen(stop)
			r.bumpEpoch(o)
			return false
		}
		if o.dictTable().sparse[k].attrs&attrConfigurable == 0 {
			o.setArrayLen(k + 1)
			r.bumpEpoch(o)
			return false
		}
		delete(o.dictTable().sparse, k)
	}
	if stop, ok := o.dropDense(int(newLen), denseConfigurable); !ok {
		o.setArrayLen(stop)
		r.bumpEpoch(o)
		return false
	}
	o.setArrayLen(newLen)
	r.bumpEpoch(o)
	return true
}

// dropDense removes dense elements at indices >= from. When dense elements
// are not configurable and one is present in that range, only the holes above
// the highest present element are removed and (itsIndex+1, false) is returned.
func (o *Object) dropDense(from int, configurable bool) (uint32, bool) {
	if from >= o.elemLen() {
		return 0, true
	}
	if !configurable {
		for i := o.elemLen() - 1; i >= from; i-- {
			if !o.elems()[i].IsHole() {
				clear(o.elems()[i+1:])
				o.setElems(o.elems()[:i+1])
				return uint32(i) + 1, false
			}
		}
	}
	clear(o.elems()[from:])
	o.setElems(o.elems()[:from])
	return 0, true
}

// SetLength sets an array's length as `a.length = n` would, returning false
// when rejected (strict callers throw).
func (o *Object) SetLength(r *Realm, n uint32) (bool, error) {
	if o.flags&flagShared != 0 {
		return false, r.sharedWriteError(o, lengthKey)
	}
	if o.flags&flagHasLazy != 0 {
		o.materializeHost()
	}
	var d PropertyDescriptor
	d.SetValue(Uint32Value(n))
	return o.arraySetLength(r, d)
}

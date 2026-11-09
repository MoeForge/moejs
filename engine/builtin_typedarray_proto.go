package engine

import "math"

// %TypedArray%.prototype (ECMA-262 §23.2.3): the accessors and the methods
// every typed array inherits. A method validates its receiver first
// (ValidateTypedArray) and then works on the payload directly; an element
// read after user code ran (a callback or a coercion, which can shrink or
// detach the buffer) goes through get, which reads undefined past the
// current length as [[Get]] does. The bulk loops check for an interrupt
// every interruptStride elements or copyBytesChunk bytes.

// typedArrayMethods builds the prototype's method table (binaryTables).
func typedArrayMethods(a *binaryAtoms) []builtinDef {
	return []builtinDef{
		{AtomAt, typedArrayAt, 1},
		{AtomCopyWithin, typedArrayCopyWithin, 2},
		{AtomEntries, typedArrayEntries, 0},
		{AtomEvery, typedArrayEvery, 1},
		{AtomFill, typedArrayFill, 1},
		{AtomFilter, typedArrayFilter, 1},
		{AtomFind, typedArrayFind, 1},
		{AtomFindIndex, typedArrayFindIndex, 1},
		{AtomFindLast, typedArrayFindLast, 1},
		{AtomFindLastIndex, typedArrayFindLastIndex, 1},
		{AtomForEach, typedArrayForEach, 1},
		{AtomIncludes, typedArrayIncludes, 1},
		{AtomIndexOf, typedArrayIndexOf, 1},
		{AtomJoin, typedArrayJoin, 1},
		{AtomKeys, typedArrayKeys, 0},
		{AtomLastIndexOf, typedArrayLastIndexOf, 1},
		{AtomMap, typedArrayMap, 1},
		{AtomReduce, typedArrayReduce, 1},
		{AtomReduceRight, typedArrayReduceRight, 1},
		{AtomReverse, typedArrayReverse, 0},
		{AtomSet, typedArraySetMethod, 1},
		{AtomSlice, typedArraySlice, 2},
		{AtomSome, typedArraySome, 1},
		{AtomSort, typedArraySort, 1},
		{a.subarray, typedArraySubarray, 2},
		{AtomToLocaleString, typedArrayToLocaleString, 0},
		{AtomToReversed, typedArrayToReversed, 0},
		{AtomToSorted, typedArrayToSorted, 1},
		{AtomValues, typedArrayValues, 0},
		{AtomWith, typedArrayWith, 2},
	}
}

// get implements [[Get]] of element i once user code may have run: the
// element, or undefined past the current length.
func (ta *typedArray) get(i int) Value {
	if i < ta.length() {
		return ta.at(i)
	}
	return Undefined()
}

// thisTypedArray implements RequireInternalSlot(this, [[TypedArrayName]]).
func thisTypedArray(r *Realm, this Value, method string) (*typedArray, error) {
	if this.IsObject() {
		if o := this.AsObject(); o.class == ClassTypedArray {
			return dataOf(o, kindTypedArray), nil
		}
	}
	return nil, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(this))
}

// --- accessors --------------------------------------------------------------------

func typedArrayBuffer(r *Realm, this Value, args []Value) (Value, error) {
	ta, err := thisTypedArray(r, this, "get %TypedArray%.prototype.buffer")
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(ta.view.buf), nil
}

// typedArrayByteLength, typedArrayByteOffset and typedArrayLength read 0
// for an out-of-bounds array.
func typedArrayByteLength(r *Realm, this Value, args []Value) (Value, error) {
	ta, err := thisTypedArray(r, this, "get %TypedArray%.prototype.byteLength")
	if err != nil {
		return Undefined(), err
	}
	return IntValue(max(ta.length(), 0) << elemShift[ta.kind]), nil
}

func typedArrayByteOffset(r *Realm, this Value, args []Value) (Value, error) {
	ta, err := thisTypedArray(r, this, "get %TypedArray%.prototype.byteOffset")
	if err != nil {
		return Undefined(), err
	}
	if ta.length() < 0 {
		return IntValue(0), nil
	}
	return IntValue(ta.view.offset), nil
}

func typedArrayLength(r *Realm, this Value, args []Value) (Value, error) {
	ta, err := thisTypedArray(r, this, "get %TypedArray%.prototype.length")
	if err != nil {
		return Undefined(), err
	}
	return IntValue(max(ta.length(), 0)), nil
}

// --- element access ---------------------------------------------------------------

// typedArrayAt implements %TypedArray%.prototype.at(index).
func typedArrayAt(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.at")
	if err != nil {
		return Undefined(), err
	}
	rel, err := r.ToIntegerOrInfinity(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if rel < 0 {
		rel += float64(n)
	}
	if rel < 0 || rel >= float64(n) {
		return Undefined(), nil
	}
	return ta.get(int(rel)), nil
}

// typedArrayWith implements %TypedArray%.prototype.with(index, value).
func typedArrayWith(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.with")
	if err != nil {
		return Undefined(), err
	}
	rel, err := r.ToIntegerOrInfinity(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if rel < 0 {
		rel += float64(n)
	}
	u, err := r.toRaw(ta.kind, Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	cur := ta.length()
	if !(rel >= 0 && rel < float64(cur)) {
		return Undefined(), r.RangeError("Invalid typed array index")
	}
	o, nta, err := r.typedArrayCreateSameType(ta.kind, n)
	if err != nil {
		return Undefined(), err
	}
	// The elements the coercions cut off read as undefined.
	m := min(n, cur)
	if err := r.copyBytes(nta.bytes(0, m), ta.bytes(0, m)); err != nil {
		return Undefined(), err
	}
	if m < n {
		fill, err := r.toRaw(ta.kind, Undefined())
		if err != nil {
			return Undefined(), err
		}
		nta.setRaw(m, fill)
		if err := r.fillPattern(nta.bytes(m, n-m), elemSize[ta.kind]); err != nil {
			return Undefined(), err
		}
	}
	// A length-tracking array can have grown past the index.
	if i := int(rel); i < n {
		nta.setRaw(i, u)
	}
	return ObjectValue(o), nil
}

// --- byte moves -------------------------------------------------------------------

// moveBytes is copy(data[to:to+n], data[from:from+n]), overlapping or not,
// in copyBytesChunk steps with an interrupt check between them.
func (r *Realm) moveBytes(data []byte, to, from, n int) error {
	if to <= from {
		return r.copyBytes(data[to:to+n], data[from:from+n])
	}
	for n > copyBytesChunk {
		n -= copyBytesChunk
		copy(data[to+n:to+n+copyBytesChunk], data[from+n:from+n+copyBytesChunk])
		if err := r.CheckInterrupt(); err != nil {
			return err
		}
	}
	copy(data[to:to+n], data[from:from+n])
	return nil
}

// copyForward copies n bytes from src at from to dst at to one byte after
// the other, first to last, as slice and the ArrayBuffer copies specify:
// within one buffer, a destination starting inside the source repeats the
// bytes before it.
func (r *Realm) copyForward(dst *arrayBuffer, to int, src *arrayBuffer, from, n int) error {
	if dst != src {
		return r.copyBytes(dst.data[to:to+n], src.data[from:from+n])
	}
	if d := to - from; d > 0 && d < n {
		copy(dst.data[to:to+d], src.data[from:to])
		return r.fillPattern(dst.data[to:to+n], d)
	}
	return r.moveBytes(dst.data, to, from, n)
}

// fillPattern repeats the first size bytes of b over all of b, doubling the
// filled prefix up to copyBytesChunk bytes a step, with an interrupt check
// after each full step.
func (r *Realm) fillPattern(b []byte, size int) error {
	for filled := size; filled < len(b); {
		c := min(filled, len(b)-filled, copyBytesChunk)
		copy(b[filled:filled+c], b[:c])
		filled += c
		if c == copyBytesChunk {
			if err := r.CheckInterrupt(); err != nil {
				return err
			}
		}
	}
	return nil
}

// typedArrayCopyWithin implements %TypedArray%.prototype.copyWithin(target,
// start, end): a byte move within the array's current bounds.
func typedArrayCopyWithin(r *Realm, this Value, args []Value) (Value, error) {
	const method = "%TypedArray%.prototype.copyWithin"
	ta, n, err := r.validateTypedArray(this, method)
	if err != nil {
		return Undefined(), err
	}
	to, _, err := r.relativeRange(Arg(args, 0), Undefined(), n)
	if err != nil {
		return Undefined(), err
	}
	from, end, err := r.relativeRange(Arg(args, 1), Arg(args, 2), n)
	if err != nil {
		return Undefined(), err
	}
	count := min(end-from, n-to)
	if count <= 0 {
		return this, nil
	}
	cur := ta.length()
	if cur < 0 {
		return Undefined(), outOfBoundsTypedArray(r, method)
	}
	// Coercing the arguments may have shrunk the array: the longest prefix
	// both ranges still hold is copied.
	if count = min(count, cur-from, cur-to); count > 0 {
		s, off := elemShift[ta.kind], ta.view.offset
		if err := r.moveBytes(ta.view.data.data, off+to<<s, off+from<<s, count<<s); err != nil {
			return Undefined(), err
		}
	}
	return this, nil
}

// typedArrayFill implements %TypedArray%.prototype.fill(value, start, end).
func typedArrayFill(r *Realm, this Value, args []Value) (Value, error) {
	const method = "%TypedArray%.prototype.fill"
	ta, n, err := r.validateTypedArray(this, method)
	if err != nil {
		return Undefined(), err
	}
	u, err := r.toRaw(ta.kind, Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	start, end, err := r.relativeRange(Arg(args, 1), Arg(args, 2), n)
	if err != nil {
		return Undefined(), err
	}
	cur := ta.length()
	if cur < 0 {
		return Undefined(), outOfBoundsTypedArray(r, method)
	}
	if end = min(end, cur); start < end {
		ta.setRaw(start, u)
		if err := r.fillPattern(ta.bytes(start, end-start), elemSize[ta.kind]); err != nil {
			return Undefined(), err
		}
	}
	return this, nil
}

// typedArrayReverse implements %TypedArray%.prototype.reverse.
func typedArrayReverse(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.reverse")
	if err != nil {
		return Undefined(), err
	}
	for lo, hi := 0, n-1; lo < hi; lo, hi = lo+1, hi-1 {
		if err := interruptEvery(r, int64(lo)); err != nil {
			return Undefined(), err
		}
		a, b := ta.raw(lo), ta.raw(hi)
		ta.setRaw(lo, b)
		ta.setRaw(hi, a)
	}
	return this, nil
}

// typedArrayToReversed implements %TypedArray%.prototype.toReversed.
func typedArrayToReversed(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.toReversed")
	if err != nil {
		return Undefined(), err
	}
	o, nta, err := r.typedArrayCreateSameType(ta.kind, n)
	if err != nil {
		return Undefined(), err
	}
	for k := range n {
		if err := interruptEvery(r, int64(k)); err != nil {
			return Undefined(), err
		}
		nta.setRaw(k, ta.raw(n-1-k))
	}
	return ObjectValue(o), nil
}

// --- set, slice and subarray ------------------------------------------------------

// typedArraySetMethod implements %TypedArray%.prototype.set(source, offset).
func typedArraySetMethod(r *Realm, this Value, args []Value) (Value, error) {
	const method = "%TypedArray%.prototype.set"
	ta, err := thisTypedArray(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	off, err := r.ToIntegerOrInfinity(Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	if off < 0 {
		return Undefined(), r.RangeError("offset is out of bounds")
	}
	if src := Arg(args, 0); src.IsObject() && src.AsObject().class == ClassTypedArray {
		return Undefined(), r.setFromTypedArray(ta, off, dataOf(src.AsObject(), kindTypedArray), method)
	}
	return Undefined(), r.setFromArrayLike(ta, off, Arg(args, 0), method)
}

// setFromTypedArray implements SetTypedArrayFromTypedArray. Within one
// buffer the same type moves the bytes and another type converts from a
// copy of the source, as the spec's CloneArrayBuffer does.
func (r *Realm) setFromTypedArray(ta *typedArray, off float64, src *typedArray, method string) error {
	n := ta.length()
	if n < 0 {
		return outOfBoundsTypedArray(r, method)
	}
	sn := src.length()
	if sn < 0 {
		return outOfBoundsTypedArray(r, method)
	}
	if float64(sn)+off > float64(n) {
		return r.RangeError("offset is out of bounds")
	}
	if src.kind.isBigInt() != ta.kind.isBigInt() {
		return mixedContentError(r)
	}
	to := int(off)
	if src.kind == ta.kind {
		s := elemShift[ta.kind]
		if src.view.data == ta.view.data {
			return r.moveBytes(ta.view.data.data, ta.view.offset+to<<s, src.view.offset, sn<<s)
		}
		return r.copyBytes(ta.bytes(to, sn), src.bytes(0, sn))
	}
	if src.view.data == ta.view.data {
		clone := make([]byte, sn<<elemShift[src.kind])
		if err := r.copyBytes(clone, src.bytes(0, sn)); err != nil {
			return err
		}
		src = &typedArray{view: dataView{data: &arrayBuffer{data: clone, max: -1}, length: len(clone)}, kind: src.kind}
	}
	return r.convertElements(ta, to, src, 0, sn)
}

// setFromArrayLike implements SetTypedArrayFromArrayLike.
func (r *Realm) setFromArrayLike(ta *typedArray, off float64, source Value, method string) error {
	n := ta.length()
	if n < 0 {
		return outOfBoundsTypedArray(r, method)
	}
	src, err := r.ToObject(source)
	if err != nil {
		return err
	}
	sn, err := r.LengthOfArrayLike(src)
	if err != nil {
		return err
	}
	if float64(sn)+off > float64(n) {
		return r.RangeError("offset is out of bounds")
	}
	to := int(off)
	for k := int64(0); k < sn; k++ {
		if err := interruptEvery(r, k); err != nil {
			return err
		}
		v, err := getIndex(r, src, k)
		if err != nil {
			return err
		}
		if err := r.typedArraySetElement(ta, to+int(k), v); err != nil {
			return err
		}
	}
	return nil
}

// typedArraySlice implements %TypedArray%.prototype.slice(start, end).
func typedArraySlice(r *Realm, this Value, args []Value) (Value, error) {
	const method = "%TypedArray%.prototype.slice"
	ta, n, err := r.validateTypedArray(this, method)
	if err != nil {
		return Undefined(), err
	}
	start, end, err := r.relativeRange(Arg(args, 0), Arg(args, 1), n)
	if err != nil {
		return Undefined(), err
	}
	count := max(end-start, 0)
	o, nta, err := r.typedArraySpeciesCreateLength(this.AsObject(), count, method)
	if err != nil {
		return Undefined(), err
	}
	if count == 0 {
		return ObjectValue(o), nil
	}
	cur := ta.length()
	if cur < 0 {
		return Undefined(), outOfBoundsTypedArray(r, method)
	}
	if count = max(min(end, cur)-start, 0); count == 0 {
		return ObjectValue(o), nil
	}
	if nta.kind == ta.kind {
		s := elemShift[ta.kind]
		err = r.copyForward(nta.view.data, nta.view.offset, ta.view.data, ta.view.offset+start<<s, count<<s)
	} else {
		err = r.convertElements(nta, 0, ta, start, count)
	}
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

// typedArraySubarray implements %TypedArray%.prototype.subarray(start, end):
// a view of the same buffer from the species constructor, length-tracking
// when this array is and end is undefined.
func typedArraySubarray(r *Realm, this Value, args []Value) (Value, error) {
	const method = "%TypedArray%.prototype.subarray"
	ta, err := thisTypedArray(r, this, method)
	if err != nil {
		return Undefined(), err
	}
	endV := Arg(args, 1)
	begin, end, err := r.relativeRange(Arg(args, 0), endV, max(ta.length(), 0))
	if err != nil {
		return Undefined(), err
	}
	argv := r.pushArgs(3)
	defer r.popArgs(3)
	argv[0] = ObjectValue(ta.view.buf)
	argv[1] = IntValue(ta.view.offset + begin<<elemShift[ta.kind])
	if ta.view.length < 0 && endV.IsUndefined() {
		argv = argv[:2]
	} else {
		argv[2] = IntValue(max(end-begin, 0))
	}
	o, _, err := r.typedArraySpeciesCreate(this.AsObject(), argv, method)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

// --- search -----------------------------------------------------------------------

// searchFrom implements the fromIndex steps of includes and indexOf over a
// length n: the first index to search, n when there is none.
func (r *Realm) searchFrom(v Value, n int) (int, error) {
	f, err := r.ToIntegerOrInfinity(v)
	if err != nil {
		return 0, err
	}
	if f >= float64(n) {
		return n, nil
	}
	if f < 0 {
		f = math.Max(f+float64(n), 0)
	}
	return int(f), nil
}

// typedArraySearch returns the first index in [from, to), or the last when
// back, whose element equals v (SameValueZero when zero, else
// IsStrictlyEqual), or -1. An integer type compares element bits with the
// bits of v when v converts back to itself; a float type compares
// Numbers.
func (r *Realm) typedArraySearch(ta *typedArray, v Value, from, to int, zero, back bool) (int, error) {
	if from >= to {
		return -1, nil
	}
	kind := ta.kind
	var want uint64
	switch {
	case kind.isBigInt():
		if !v.IsBigInt() {
			return -1, nil
		}
		b := v.AsBigInt()
		if kind == elemBigInt64 && !b.v.IsInt64() || kind == elemBigUint64 && !b.v.IsUint64() {
			return -1, nil
		}
		want = b.Uint64()
	case kind >= elemFloat16:
		if !v.IsNumber() {
			return -1, nil
		}
		return r.typedArraySearchFloat(ta, v.AsNumber(), from, to, zero, back)
	default:
		if !v.IsNumber() {
			return -1, nil
		}
		f := v.AsNumber()
		want = numberToRaw(kind, f) & (1<<(8*elemSize[kind]) - 1)
		if rawToFloat(kind, want) != f {
			return -1, nil
		}
	}
	for k := range to - from {
		if err := interruptEvery(r, int64(k)); err != nil {
			return -1, err
		}
		i := from + k
		if back {
			i = to - 1 - k
		}
		if ta.raw(i) == want {
			return i, nil
		}
	}
	return -1, nil
}

// typedArraySearchFloat is typedArraySearch for the Number f in a float
// array.
func (r *Realm) typedArraySearchFloat(ta *typedArray, f float64, from, to int, zero, back bool) (int, error) {
	nan := f != f
	if nan && !zero {
		return -1, nil
	}
	for k := range to - from {
		if err := interruptEvery(r, int64(k)); err != nil {
			return -1, err
		}
		i := from + k
		if back {
			i = to - 1 - k
		}
		if x := rawToFloat(ta.kind, ta.raw(i)); x == f || nan && x != x {
			return i, nil
		}
	}
	return -1, nil
}

// typedArrayIncludes implements %TypedArray%.prototype.includes(searchElement,
// fromIndex). Indices the fromIndex coercion cut off read as undefined.
func typedArrayIncludes(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.includes")
	if err != nil || n == 0 {
		return False(), err
	}
	k, err := r.searchFrom(Arg(args, 1), n)
	if err != nil {
		return Undefined(), err
	}
	v, cur := Arg(args, 0), max(ta.length(), 0)
	if v.IsUndefined() && max(k, cur) < n {
		return True(), nil
	}
	i, err := r.typedArraySearch(ta, v, k, min(n, cur), true, false)
	if err != nil {
		return Undefined(), err
	}
	return Bool(i >= 0), nil
}

// typedArrayIndexOf implements %TypedArray%.prototype.indexOf(searchElement,
// fromIndex).
func typedArrayIndexOf(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.indexOf")
	if err != nil {
		return Undefined(), err
	}
	if n == 0 {
		return IntValue(-1), nil
	}
	k, err := r.searchFrom(Arg(args, 1), n)
	if err != nil {
		return Undefined(), err
	}
	i, err := r.typedArraySearch(ta, Arg(args, 0), k, min(n, max(ta.length(), 0)), false, false)
	if err != nil {
		return Undefined(), err
	}
	return IntValue(i), nil
}

// typedArrayLastIndexOf implements
// %TypedArray%.prototype.lastIndexOf(searchElement, fromIndex).
func typedArrayLastIndexOf(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.lastIndexOf")
	if err != nil {
		return Undefined(), err
	}
	if n == 0 {
		return IntValue(-1), nil
	}
	k := n - 1
	if len(args) > 1 {
		f, err := r.ToIntegerOrInfinity(args[1])
		if err != nil {
			return Undefined(), err
		}
		if f < 0 {
			if f += float64(n); f < 0 {
				return IntValue(-1), nil
			}
		}
		k = int(math.Min(f, float64(n-1)))
	}
	i, err := r.typedArraySearch(ta, Arg(args, 0), 0, min(k+1, max(ta.length(), 0)), false, true)
	if err != nil {
		return Undefined(), err
	}
	return IntValue(i), nil
}

// --- callbacks --------------------------------------------------------------------

// typedArrayCallback validates the receiver and the callback of an
// iteration method.
func (r *Realm) typedArrayCallback(this Value, args []Value, method string) (*typedArray, int, *Object, error) {
	ta, n, err := r.validateTypedArray(this, method)
	if err != nil {
		return nil, 0, nil, err
	}
	fn, err := callbackArg(r, args)
	return ta, n, fn, err
}

// typedArrayScan calls the callback on the elements, last to first when
// back, until its result's truthiness is stop: it returns that element's
// index and value, or -1.
func (r *Realm) typedArrayScan(this Value, args []Value, method string, back, stop bool) (int, Value, error) {
	ta, n, fn, err := r.typedArrayCallback(this, args, method)
	if err != nil {
		return -1, Undefined(), err
	}
	thisArg := Arg(args, 1)
	argv := r.pushArgs(3)
	defer r.popArgs(3)
	argv[2] = this
	for k := range n {
		if err := interruptEvery(r, int64(k)); err != nil {
			return -1, Undefined(), err
		}
		i := k
		if back {
			i = n - 1 - k
		}
		v := ta.get(i)
		argv[0], argv[1] = v, IntValue(i)
		res, err := r.CallObject(fn, thisArg, argv)
		if err != nil {
			return -1, Undefined(), err
		}
		if ToBoolean(res) == stop {
			return i, v, nil
		}
	}
	return -1, Undefined(), nil
}

func typedArrayEvery(r *Realm, this Value, args []Value) (Value, error) {
	i, _, err := r.typedArrayScan(this, args, "%TypedArray%.prototype.every", false, false)
	return Bool(i < 0), err
}

func typedArraySome(r *Realm, this Value, args []Value) (Value, error) {
	i, _, err := r.typedArrayScan(this, args, "%TypedArray%.prototype.some", false, true)
	return Bool(i >= 0), err
}

func typedArrayFind(r *Realm, this Value, args []Value) (Value, error) {
	_, v, err := r.typedArrayScan(this, args, "%TypedArray%.prototype.find", false, true)
	return v, err
}

func typedArrayFindIndex(r *Realm, this Value, args []Value) (Value, error) {
	i, _, err := r.typedArrayScan(this, args, "%TypedArray%.prototype.findIndex", false, true)
	return IntValue(i), err
}

func typedArrayFindLast(r *Realm, this Value, args []Value) (Value, error) {
	_, v, err := r.typedArrayScan(this, args, "%TypedArray%.prototype.findLast", true, true)
	return v, err
}

func typedArrayFindLastIndex(r *Realm, this Value, args []Value) (Value, error) {
	i, _, err := r.typedArrayScan(this, args, "%TypedArray%.prototype.findLastIndex", true, true)
	return IntValue(i), err
}

// typedArrayForEach implements %TypedArray%.prototype.forEach(callbackfn,
// thisArg).
func typedArrayForEach(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, fn, err := r.typedArrayCallback(this, args, "%TypedArray%.prototype.forEach")
	if err != nil {
		return Undefined(), err
	}
	thisArg := Arg(args, 1)
	argv := r.pushArgs(3)
	defer r.popArgs(3)
	argv[2] = this
	for k := range n {
		if err := interruptEvery(r, int64(k)); err != nil {
			return Undefined(), err
		}
		argv[0], argv[1] = ta.get(k), IntValue(k)
		if _, err := r.CallObject(fn, thisArg, argv); err != nil {
			return Undefined(), err
		}
	}
	return Undefined(), nil
}

// typedArrayMap implements %TypedArray%.prototype.map(callbackfn, thisArg).
func typedArrayMap(r *Realm, this Value, args []Value) (Value, error) {
	const method = "%TypedArray%.prototype.map"
	ta, n, fn, err := r.typedArrayCallback(this, args, method)
	if err != nil {
		return Undefined(), err
	}
	o, nta, err := r.typedArraySpeciesCreateLength(this.AsObject(), n, method)
	if err != nil {
		return Undefined(), err
	}
	thisArg := Arg(args, 1)
	argv := r.pushArgs(3)
	defer r.popArgs(3)
	argv[2] = this
	for k := range n {
		if err := interruptEvery(r, int64(k)); err != nil {
			return Undefined(), err
		}
		argv[0], argv[1] = ta.get(k), IntValue(k)
		v, err := r.CallObject(fn, thisArg, argv)
		if err != nil {
			return Undefined(), err
		}
		if err := r.typedArraySetElement(nta, k, v); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(o), nil
}

// typedArrayFilter implements %TypedArray%.prototype.filter(callbackfn,
// thisArg). The selected elements are kept in the buffer encoding, one
// element's bytes each, so the list is no larger than the array; holes
// lists the ones read past the current length, which are undefined.
func typedArrayFilter(r *Realm, this Value, args []Value) (Value, error) {
	const method = "%TypedArray%.prototype.filter"
	ta, n, fn, err := r.typedArrayCallback(this, args, method)
	if err != nil {
		return Undefined(), err
	}
	size := elemSize[ta.kind]
	var kept []byte
	var holes []int
	thisArg := Arg(args, 1)
	argv := r.pushArgs(3)
	defer r.popArgs(3)
	argv[2] = this
	for k := range n {
		if err := interruptEvery(r, int64(k)); err != nil {
			return Undefined(), err
		}
		valid := k < ta.length()
		v, u := Undefined(), uint64(0)
		if valid {
			u = ta.raw(k)
			v = rawToValue(ta.kind, u)
		}
		argv[0], argv[1] = v, IntValue(k)
		res, err := r.CallObject(fn, thisArg, argv)
		if err != nil {
			return Undefined(), err
		}
		if !ToBoolean(res) {
			continue
		}
		if valid { // the bits read before the callback, which may detach
			for b := range size {
				kept = append(kept, byte(u>>(8*b)))
			}
		} else {
			holes = append(holes, len(kept)/size)
			kept = append(kept, make([]byte, size)...)
		}
	}
	count := len(kept) / size
	o, nta, err := r.typedArraySpeciesCreateLength(this.AsObject(), count, method)
	if err != nil {
		return Undefined(), err
	}
	src := &typedArray{view: dataView{data: &arrayBuffer{data: kept, max: -1}, length: len(kept)}, kind: ta.kind}
	copyRun := func(from, to int) error {
		if nta.kind == ta.kind {
			return r.copyBytes(nta.bytes(from, to-from), src.bytes(from, to-from))
		}
		return r.convertElements(nta, from, src, from, to-from)
	}
	prev := 0
	for _, h := range holes {
		if err := copyRun(prev, h); err != nil {
			return Undefined(), err
		}
		if err := r.typedArraySetElement(nta, h, Undefined()); err != nil {
			return Undefined(), err
		}
		prev = h + 1
	}
	if err := copyRun(prev, count); err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

// typedArrayReduceWith implements %TypedArray%.prototype.reduce and, when
// back, reduceRight.
func (r *Realm) typedArrayReduceWith(this Value, args []Value, method string, back bool) (Value, error) {
	ta, n, fn, err := r.typedArrayCallback(this, args, method)
	if err != nil {
		return Undefined(), err
	}
	k, step := 0, 1
	if back {
		k, step = n-1, -1
	}
	var acc Value
	if len(args) >= 2 {
		acc = args[1]
	} else {
		if n == 0 {
			return Undefined(), r.TypeError("Reduce of empty array with no initial value")
		}
		acc = ta.at(k)
		k += step
	}
	argv := r.pushArgs(4)
	defer r.popArgs(4)
	argv[3] = this
	for i := int64(0); k >= 0 && k < n; k, i = k+step, i+1 {
		if err := interruptEvery(r, i); err != nil {
			return Undefined(), err
		}
		argv[0], argv[1], argv[2] = acc, ta.get(k), IntValue(k)
		if acc, err = r.CallObject(fn, Undefined(), argv); err != nil {
			return Undefined(), err
		}
	}
	return acc, nil
}

func typedArrayReduce(r *Realm, this Value, args []Value) (Value, error) {
	return r.typedArrayReduceWith(this, args, "%TypedArray%.prototype.reduce", false)
}

func typedArrayReduceRight(r *Realm, this Value, args []Value) (Value, error) {
	return r.typedArrayReduceWith(this, args, "%TypedArray%.prototype.reduceRight", true)
}

// --- strings and iterators --------------------------------------------------------

// typedArrayJoin implements %TypedArray%.prototype.join(separator).
func typedArrayJoin(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.join")
	if err != nil {
		return Undefined(), err
	}
	sep := commaString
	if sv := Arg(args, 0); !sv.IsUndefined() {
		if sep, err = r.ToString(sv); err != nil {
			return Undefined(), err
		}
	}
	var sb StringBuilder
	for k := range n {
		if k > 0 {
			sb.WriteString(sep)
		}
		if k < ta.length() {
			s, err := r.ToString(ta.at(k))
			if err != nil {
				return Undefined(), err
			}
			sb.WriteString(s)
		}
		if err := sb.checkLength(r); err != nil {
			return Undefined(), err
		}
		if err := interruptEvery(r, int64(k)); err != nil {
			return Undefined(), err
		}
	}
	return StringValue(r.builtString(&sb)), nil
}

// typedArrayToLocaleString implements %TypedArray%.prototype.toLocaleString
// (ECMA-402) as Array.prototype.toLocaleString does
// (arrayProtoToLocaleString).
func typedArrayToLocaleString(r *Realm, this Value, args []Value) (Value, error) {
	ta, n, err := r.validateTypedArray(this, "%TypedArray%.prototype.toLocaleString")
	if err != nil {
		return Undefined(), err
	}
	var sb StringBuilder
	var localeArgs []Value
	for k := range n {
		if k > 0 {
			sb.WriteString(commaString)
		}
		if v := ta.get(k); !v.IsUndefined() {
			fn, err := r.GetV(v, StringKey(AtomToLocaleString))
			if err != nil {
				return Undefined(), err
			}
			if localeArgs == nil {
				localeArgs = []Value{intlArg(args, 0), intlArg(args, 1)}
			}
			sv, err := r.Call(fn, v, localeArgs)
			if err != nil {
				return Undefined(), err
			}
			s, err := r.ToString(sv)
			if err != nil {
				return Undefined(), err
			}
			sb.WriteString(s)
		}
		if err := sb.checkLength(r); err != nil {
			return Undefined(), err
		}
		if err := interruptEvery(r, int64(k)); err != nil {
			return Undefined(), err
		}
	}
	return StringValue(r.builtString(&sb)), nil
}

// typedArrayIterator implements the entries, keys and values methods.
func typedArrayIterator(r *Realm, this Value, kind IterKind, method string) (Value, error) {
	if _, _, err := r.validateTypedArray(this, method); err != nil {
		return Undefined(), err
	}
	return ObjectValue(r.newArrayIterator(this.AsObject(), kind)), nil
}

func typedArrayEntries(r *Realm, this Value, args []Value) (Value, error) {
	return typedArrayIterator(r, this, IterEntries, "%TypedArray%.prototype.entries")
}

func typedArrayKeys(r *Realm, this Value, args []Value) (Value, error) {
	return typedArrayIterator(r, this, IterKeys, "%TypedArray%.prototype.keys")
}

func typedArrayValues(r *Realm, this Value, args []Value) (Value, error) {
	return typedArrayIterator(r, this, IterValues, "%TypedArray%.prototype.values")
}

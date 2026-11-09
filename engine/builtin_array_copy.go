package engine

import "math"

// Array.prototype.copyWithin and the ES2023 change-array-by-copy methods
// (toReversed, toSorted, toSpliced, with). The copying methods read every
// index with Get, so holes become undefined, into a fresh dense array. The
// result is unobservable until it is returned, so it is built as a Go
// slice and wrapped at the end; its length is fixed before any read.

// arrayCreateCheck is ArrayCreate's length check.
func arrayCreateCheck(r *Realm, n int64) error {
	if n > math.MaxUint32 {
		return r.RangeError("Invalid array length")
	}
	return nil
}

// copyStorage returns empty storage for an n-element copy of o: exact (and
// co-allocated when small) unless n far exceeds what o stores, in which
// case it grows as the reads succeed.
func (r *Realm) copyStorage(o *Object, n int64) (*arrayObject, []Value) {
	return r.newArrayStorage(0, int(min(n, int64(o.elemLen())+interruptStride)))
}

// appendDense appends src with holes read as undefined; only valid when o
// is a plainArray, so a hole has nothing behind it on the prototype chain.
func appendDense(dst, src []Value) []Value {
	for _, v := range src {
		if v.IsHole() {
			v = Undefined()
		}
		dst = append(dst, v)
	}
	return dst
}

// appendRead appends Get(O, k) for k in [from, to).
func appendRead(r *Realm, dst []Value, o *Object, from, to int64) ([]Value, error) {
	for k := from; k < to; k++ {
		v, err := getIndex(r, o, k)
		if err != nil {
			return nil, err
		}
		dst = append(dst, v)
		if err := interruptEvery(r, k); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// arrayProtoCopyWithin implements Array.prototype.copyWithin(target, start,
// end).
func arrayProtoCopyWithin(r *Realm, this Value, args []Value) (Value, error) {
	o, n, err := arrayLikeThis(r, this)
	if err != nil {
		return Undefined(), err
	}
	to, err := relativeIndex(r, Arg(args, 0), n, 0)
	if err != nil {
		return Undefined(), err
	}
	from, err := relativeIndex(r, Arg(args, 1), n, 0)
	if err != nil {
		return Undefined(), err
	}
	final, err := relativeIndex(r, Arg(args, 2), n, n)
	if err != nil {
		return Undefined(), err
	}
	count := min(final-from, n-to)
	if count <= 0 {
		return ObjectValue(o), nil
	}
	// The coercions above ran user code that may have resized the array.
	// A hole copies as a hole, which is the spec's DeletePropertyOrThrow.
	if mutableArray(o) && int64(o.elemLen()) == n {
		copy(o.elems()[to:to+count], o.elems()[from:from+count])
		r.bumpEpoch(o)
		return ObjectValue(o), nil
	}
	dir := int64(1)
	if from < to && to < from+count {
		dir = -1
		from += count - 1
		to += count - 1
	}
	for i := int64(0); i < count; i, from, to = i+1, from+dir, to+dir {
		if has, err := hasIndex(r, o, from); err != nil {
			return Undefined(), err
		} else if has {
			v, err := getIndex(r, o, from)
			if err != nil {
				return Undefined(), err
			}
			if err := setIndex(r, o, to, v); err != nil {
				return Undefined(), err
			}
		} else if err := deleteIndex(r, o, to); err != nil {
			return Undefined(), err
		}
		if err := interruptEvery(r, i); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(o), nil
}

// arrayProtoToReversed implements Array.prototype.toReversed.
func arrayProtoToReversed(r *Realm, this Value, args []Value) (Value, error) {
	o, n, err := arrayLikeThis(r, this)
	if err != nil {
		return Undefined(), err
	}
	if err := arrayCreateCheck(r, n); err != nil {
		return Undefined(), err
	}
	ao, items := r.copyStorage(o, n)
	if plainArray(o) {
		for k := n - 1; k >= 0; k-- {
			v := o.elemsReadOnly()[k]
			if v.IsHole() {
				v = Undefined()
			}
			items = append(items, v)
		}
	} else {
		for k := int64(0); k < n; k++ {
			v, err := getIndex(r, o, n-1-k)
			if err != nil {
				return Undefined(), err
			}
			items = append(items, v)
			if err := interruptEvery(r, k); err != nil {
				return Undefined(), err
			}
		}
	}
	return ObjectValue(r.initArray(ao, items, uint32(n))), nil
}

// arrayProtoToSorted implements Array.prototype.toSorted(comparefn): sort's
// ordering over a read-through copy.
func arrayProtoToSorted(r *Realm, this Value, args []Value) (Value, error) {
	cmp := Arg(args, 0)
	if !cmp.IsUndefined() && !IsCallable(cmp) {
		return Undefined(), r.TypeError("The comparison function must be either a function or undefined")
	}
	o, n, err := arrayLikeThis(r, this)
	if err != nil {
		return Undefined(), err
	}
	if err := arrayCreateCheck(r, n); err != nil {
		return Undefined(), err
	}
	ao, items := r.copyStorage(o, n)
	if plainArray(o) {
		items = appendDense(items, o.elemsReadOnly())
	} else if items, err = appendRead(r, items, o, 0, n); err != nil {
		return Undefined(), err
	}
	if err := sortValues(r, items, cmp); err != nil {
		return Undefined(), err
	}
	return ObjectValue(r.initArray(ao, items, uint32(n))), nil
}

// arrayProtoToSpliced implements Array.prototype.toSpliced(start,
// skipCount, ...items).
func arrayProtoToSpliced(r *Realm, this Value, args []Value) (Value, error) {
	o, n, err := arrayLikeThis(r, this)
	if err != nil {
		return Undefined(), err
	}
	start, err := relativeIndex(r, Arg(args, 0), n, 0)
	if err != nil {
		return Undefined(), err
	}
	var skip int64
	var inserted []Value
	switch len(args) {
	case 0:
	case 1:
		skip = n - start
	default:
		sc, err := r.ToIntegerOrInfinity(args[1])
		if err != nil {
			return Undefined(), err
		}
		skip = int64(min(max(sc, 0), float64(n-start)))
		inserted = args[2:]
	}
	newLen := n + int64(len(inserted)) - skip
	if newLen > maxSafeInteger {
		return Undefined(), r.TypeError("Splicing the array-like would surpass 2**53-1 elements")
	}
	if err := arrayCreateCheck(r, newLen); err != nil {
		return Undefined(), err
	}
	ao, items := r.copyStorage(o, newLen)
	// The coercions above may have resized the array; n must still
	// describe the storage.
	if plainArray(o) && int64(o.elemLen()) == n {
		items = appendDense(items, o.elemsReadOnly()[:start])
		items = append(items, inserted...)
		items = appendDense(items, o.elemsReadOnly()[start+skip:])
	} else {
		if items, err = appendRead(r, items, o, 0, start); err != nil {
			return Undefined(), err
		}
		items = append(items, inserted...)
		if items, err = appendRead(r, items, o, start+skip, n); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(r.initArray(ao, items, uint32(newLen))), nil
}

// arrayProtoWith implements Array.prototype.with(index, value).
func arrayProtoWith(r *Realm, this Value, args []Value) (Value, error) {
	o, n, err := arrayLikeThis(r, this)
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
		return Undefined(), r.RangeError("Invalid index : %s", NumberToGoString(rel))
	}
	if err := arrayCreateCheck(r, n); err != nil {
		return Undefined(), err
	}
	idx := int64(rel)
	value := Arg(args, 1)
	ao, items := r.copyStorage(o, n)
	if plainArray(o) && int64(o.elemLen()) == n {
		items = appendDense(items, o.elemsReadOnly())
		items[idx] = value
		return ObjectValue(r.initArray(ao, items, uint32(n))), nil
	}
	if items, err = appendRead(r, items, o, 0, idx); err != nil {
		return Undefined(), err
	}
	items = append(items, value)
	if items, err = appendRead(r, items, o, idx+1, n); err != nil {
		return Undefined(), err
	}
	return ObjectValue(r.initArray(ao, items, uint32(n))), nil
}

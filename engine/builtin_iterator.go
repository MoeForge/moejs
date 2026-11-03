package engine

import "unsafe"

// %IteratorPrototype%, %ArrayIteratorPrototype% and %StringIteratorPrototype%
// (ES2025 §27.1.2, §23.1.5, §22.1.5), Array.prototype[@@iterator] and
// [@@unscopables], and String.prototype[@@iterator].

var (
	AtomArrayIterator  = staticAtom("Array Iterator")
	AtomStringIterator = staticAtom("String Iterator")
)

// arrayUnscopables lists the names of Array.prototype[@@unscopables]
// (ES2025 §23.1.3.41), in spec order.
var arrayUnscopables = [...]*String{
	AtomAt, AtomCopyWithin, AtomEntries, AtomFill, AtomFind, AtomFindIndex, AtomFindLast, AtomFindLastIndex,
	AtomFlat, AtomFlatMap, AtomIncludes, AtomKeys, AtomToReversed, AtomToSorted, AtomToSpliced, AtomValues,
}

// installArrayIterator creates the iterator prototypes and the iteration
// methods of Array.prototype and String.prototype (after installArray: the
// @@iterator of Array.prototype is its `values` function).
func installArrayIterator(r *Realm) {
	r.IteratorPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, 1)
	r.installSymbolMethod(r.IteratorPrototype, SymIterator, atomIteratorFn, iteratorProtoIterator, 0, attrHidden)

	r.ArrayIteratorPrototype = r.newIntrinsic(ClassObject, r.IteratorPrototype, 2)
	r.arrayIterNextFn = r.NewNativeFunction(AtomNext, 0, arrayIteratorNext)
	r.installOrReplace(r.ArrayIteratorPrototype, nextKey, propCell{value: ObjectValue(r.arrayIterNextFn), attrs: attrHidden})
	r.installToStringTag(r.ArrayIteratorPrototype, AtomArrayIterator)

	r.StringIteratorPrototype = r.newIntrinsic(ClassObject, r.IteratorPrototype, 2)
	r.stringIterNextFn = r.NewNativeFunction(AtomNext, 0, stringIteratorNext)
	r.installOrReplace(r.StringIteratorPrototype, nextKey, propCell{value: ObjectValue(r.stringIterNextFn), attrs: attrHidden})
	r.installToStringTag(r.StringIteratorPrototype, AtomStringIterator)

	ap := r.ArrayPrototype
	values := bootstrapOwnValue(ap, StringKey(AtomValues))
	r.arrayValuesFn = values.AsObject()
	r.installOrReplace(ap, iteratorKey, propCell{value: values, attrs: attrHidden})
	unscopables := r.NewObjectWithProto(nil)
	r.installDeferred(unscopables, installArrayUnscopables)
	r.installOrReplace(ap, SymbolKey(SymUnscopables), propCell{value: ObjectValue(unscopables), attrs: attrConfigurable})

	r.stringIterFn = r.installSymbolMethod(r.StringPrototype, SymIterator, atomIteratorFn, stringProtoIterator, 0, attrHidden)

	r.protoGuards = [numProtoGuards]protoGuard{
		guardArrayIter:  {obj: ap, key: iteratorKey, want: values},
		guardArrayNext:  {obj: r.ArrayIteratorPrototype, key: nextKey, want: ObjectValue(r.arrayIterNextFn)},
		guardStringIter: {obj: r.StringPrototype, key: iteratorKey, want: ObjectValue(r.stringIterFn)},
		guardStringNext: {obj: r.StringIteratorPrototype, key: nextKey, want: ObjectValue(r.stringIterNextFn)},
	}
}

// iteratorProtoIterator implements %IteratorPrototype%[@@iterator].
func iteratorProtoIterator(r *Realm, this Value, args []Value) (Value, error) {
	return this, nil
}

// arrayIteratorObject co-allocates an array iterator and its IteratorData.
type arrayIteratorObject struct {
	obj  Object
	data IteratorData
}

// newIteratorObject co-allocates an engine iterator object over target.
func (r *Realm) newIteratorObject(proto *Object, class Class, target Value, kind IterKind) *Object {
	it := &arrayIteratorObject{}
	r.chargeObject(unsafe.Sizeof(arrayIteratorObject{}))
	o := &it.obj
	o.shape = r.rootShapeFor(proto)
	o.proto = proto
	o.class = class
	o.flags = flagExtensible
	it.data = IteratorData{Target: target, Kind: kind}
	o.internal = &it.data
	return o
}

// newArrayIterator implements CreateArrayIterator(array, kind).
func (r *Realm) newArrayIterator(target *Object, kind IterKind) *Object {
	return r.newIteratorObject(r.ArrayIteratorPrototype, ClassArrayIterator, ObjectValue(target), kind)
}

// newStringIterator implements CreateStringIterator (the iterator of
// String.prototype[@@iterator]).
func (r *Realm) newStringIterator(s *String) *Object {
	return r.newIteratorObject(r.StringIteratorPrototype, ClassStringIterator, StringValue(s), IterValues)
}

// iterResultObject co-allocates a {value, done} result with its two slots.
type iterResultObject struct {
	obj   Object
	slots [2]Value
}

// iterResultShape returns the realm's shape for {value, done} objects:
// two cached transitions from the plain root, or the template's
// process-wide shape in a shared realm (the same shared shape, without the
// two lookups).
func (r *Realm) iterResultShape() *Shape {
	if r.sharedIntrinsics {
		return sharedTpl.iterResultShape
	}
	return iterResultShapeFrom(r)
}

// iterResultFields returns the value and done properties of the iterator
// result o when it is an ordinary object of the realm's iterator result
// shape, whose two data properties [[Get]] would read from their slots.
func (r *Realm) iterResultFields(o *Object) (value, done Value, ok bool) {
	if o.flags&(flagDict|flagHasLazy) != 0 || o.class != ClassObject || o.shape != r.iterResultShape() {
		return Undefined(), Undefined(), false
	}
	return o.slots[0], o.slots[1], true
}

// iterResultShapeFrom returns the shape of {value, done} objects in r.
func iterResultShapeFrom(r *Realm) *Shape {
	return r.plainRoot.addProperty(r, StringKey(AtomValue), attrDefault).addProperty(r, StringKey(AtomDone), attrDefault)
}

// createIterResult implements CreateIterResultObject in one allocation.
func (r *Realm) createIterResult(value Value, done bool) Value {
	res := &iterResultObject{}
	r.chargeObject(unsafe.Sizeof(iterResultObject{}))
	o := &res.obj
	o.shape = r.iterResultShape()
	o.proto = r.ObjectPrototype
	o.class = ClassObject
	o.flags = flagExtensible
	res.slots = [2]Value{value, Bool(done)}
	o.slots = res.slots[:]
	return ObjectValue(o)
}

// iteratorNext is the shared body of the engine iterators' next methods:
// it checks the receiver's class and steps its IteratorData.
func iteratorNext(r *Realm, this Value, class Class) (Value, error) {
	if this.IsObject() && this.AsObject().class == class {
		if d, ok := this.AsObject().internal.(*IteratorData); ok {
			v, done, err := d.Next(r)
			if err != nil {
				return Undefined(), err
			}
			return r.createIterResult(v, done), nil
		}
	}
	return Undefined(), r.TypeError("next method called on incompatible receiver %s", r.DisplayString(this))
}

// arrayIteratorNext implements %ArrayIteratorPrototype%.next: the length is
// re-read on every step so growth during iteration is observed, and the
// target is released once exhausted.
func arrayIteratorNext(r *Realm, this Value, args []Value) (Value, error) {
	return iteratorNext(r, this, ClassArrayIterator)
}

// stringIteratorNext implements %StringIteratorPrototype%.next.
func stringIteratorNext(r *Realm, this Value, args []Value) (Value, error) {
	return iteratorNext(r, this, ClassStringIterator)
}

// stringProtoIterator implements String.prototype[@@iterator].
func stringProtoIterator(r *Realm, this Value, args []Value) (Value, error) {
	if this.IsNullish() {
		return Undefined(), r.TypeError("String.prototype[Symbol.iterator] called on null or undefined")
	}
	s, err := r.ToString(this)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(r.newStringIterator(s)), nil
}

func installArrayUnscopables(r *Realm, o *Object) {
	o.ReserveSlots(r, len(arrayUnscopables))
	for _, name := range arrayUnscopables {
		o.DefineOwnDataFast(r, StringKey(name), True(), attrDefault)
	}
}

package engine

import "unsafe"

// Map, %MapIteratorPrototype% and Map.groupBy (ES2025 §24.1, §24.1.5,
// §24.1.2.1). The table is builtin_map_table.go; Set shares it
// (builtin_set.go).

var mapProtoMethods = []builtinDef{
	{AtomClear, mapProtoClear, 0},
	{AtomDelete, mapProtoDelete, 1},
	{AtomEntries, mapProtoEntries, 0},
	{AtomForEach, mapProtoForEach, 1},
	{AtomGet, mapProtoGet, 1},
	{atomCollHas, mapProtoHas, 1},
	{AtomKeys, mapProtoKeys, 0},
	{AtomValues, mapProtoValues, 0},
}

var mapCtorMethods = []builtinDef{{AtomGroupBy, mapGroupBy, 2}}

var mapProtoGetters = []getterDef{newGetterDef(AtomSize, mapProtoSize)}

func installMap(r *Realm) {
	r.MapPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, r.deferredCap(mapProtoProps+1, 1))
	r.MapCtor = r.newConstructor(AtomMapName, 0, mapCall, mapConstruct, r.MapPrototype)
	// @@species first: Map and Set then share that transition of the
	// constructor shape (symbols list after strings in [[OwnPropertyKeys]]
	// whatever the insertion order).
	r.MapCtor.ReserveSlots(r, 2)
	r.installSpeciesGetter(r.MapCtor)
	r.installBuiltins(r.MapCtor, mapCtorMethods)
	r.MapIteratorPrototype = r.newIntrinsic(ClassObject, r.IteratorPrototype, r.deferredCap(2, 0))
	r.installDeferred(r.MapPrototype, installMapPrototype)
	r.bindGlobal(AtomMapName, ObjectValue(r.MapCtor))
}

// mapProtoProps counts the properties installMapPrototype defines on
// Map.prototype.
var mapProtoProps = len(mapProtoMethods) + 4

// installMapPrototype defines Map.prototype and, since only its methods
// create Map iterators, %MapIteratorPrototype% (installDeferred).
func installMapPrototype(r *Realm, p *Object) {
	p.ReserveSlots(r, mapProtoProps)
	r.installBuiltins(p, mapProtoMethods)
	r.mapSetFn = r.NewNativeFunction(AtomSet, 2, mapProtoSet)
	r.installOrReplace(p, StringKey(AtomSet), propCell{value: ObjectValue(r.mapSetFn), attrs: attrHidden})
	r.installGetters(p, mapProtoGetters)
	r.installOrReplace(p, iteratorKey, propCell{value: bootstrapOwnValue(p, StringKey(AtomEntries)), attrs: attrHidden})
	r.installToStringTag(p, AtomMapName)

	ip := r.MapIteratorPrototype
	ip.ReserveSlots(r, 2)
	r.mapIterNextFn = r.NewNativeFunction(AtomNext, 0, mapIteratorNext)
	r.installOrReplace(ip, nextKey, propCell{value: ObjectValue(r.mapIterNextFn), attrs: attrHidden})
	r.installToStringTag(ip, AtomMapIterator)
}

// The [[Call]] of the constructors that throw without new, built once.
var (
	mapCall     = requireNew("Map")
	setCall     = requireNew("Set")
	weakMapCall = requireNew("WeakMap")
	weakSetCall = requireNew("WeakSet")
	weakRefCall = requireNew("WeakRef")
)

// requireNew is the [[Call]] of constructors that throw without new.
func requireNew(name string) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		return Undefined(), r.TypeError("Constructor %s requires 'new'", name)
	}
}

// speciesGetter implements get C[@@species] of the builtin constructors.
func speciesGetter(r *Realm, this Value, args []Value) (Value, error) {
	return this, nil
}

// collObject co-allocates a Map or Set object with its table.
type collObject struct {
	obj  Object
	data collection
}

// newCollectionObject creates an empty Map or Set with the given prototype.
func (r *Realm) newCollectionObject(proto *Object, class Class) (*Object, *collection) {
	r.markPrototype(proto)
	co := &collObject{}
	r.chargeObject(unsafe.Sizeof(collObject{}))
	o := &co.obj
	o.shape = r.rootShapeFor(proto)
	o.class = class
	o.flags = flagExtensible
	setData(o, kindCollection, &co.data)
	return o, &co.data
}

// thisCollection implements RequireInternalSlot(this, [[MapData]] or
// [[SetData]]).
func thisCollection(r *Realm, this Value, class Class, method string) (*collection, error) {
	if this.IsObject() {
		if o := this.AsObject(); o.class == class {
			if c, ok := dataOk(o, kindCollection); ok {
				return c, nil
			}
		}
	}
	return nil, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(this))
}

// mapConstruct implements new Map(iterable).
func mapConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	proto, err := r.GetPrototypeFromConstructor(newTarget, r.MapCtor, r.MapPrototype)
	if err != nil {
		return Undefined(), err
	}
	o, c := r.newCollectionObject(proto, ClassMap)
	return ObjectValue(o), r.fillFromEntries(o, Arg(args, 0), AtomSet, &r.mapSetFn, func(k, v Value) error {
		c.set(r, k, v)
		return nil
	})
}

// fillFromEntries implements the iterable step of the Map and WeakMap
// constructors: Get(target, name) must be callable and receives every entry
// of iterable (AddEntriesFromIterable), stored by direct while it is the
// original method *orig (an error from direct is the method's throw; orig
// is read after the Get, which installs a deferred prototype).
func (r *Realm) fillFromEntries(target *Object, iterable Value, name *String, orig **Object, direct func(k, v Value) error) error {
	if iterable.IsNullish() {
		return nil
	}
	adder, err := r.collectionAdder(target, name)
	if err != nil {
		return err
	}
	if adder == *orig {
		return r.addEntriesFromIterable(iterable, direct)
	}
	argv := r.pushArgs(2)
	defer r.popArgs(2)
	return r.addEntriesFromIterable(iterable, func(k, v Value) error {
		argv[0], argv[1] = k, v
		_, err := r.CallObject(adder, ObjectValue(target), argv)
		return err
	})
}

// fillFromValues implements the iterable step of the Set and WeakSet
// constructors: Get(target, name) must be callable and receives every value
// of iterable, stored by direct while it is the original method *orig (an
// error from direct is the method's throw; orig is read after the Get).
func (r *Realm) fillFromValues(target *Object, iterable Value, name *String, orig **Object, direct func(v Value) error) error {
	if iterable.IsNullish() {
		return nil
	}
	adder, err := r.collectionAdder(target, name)
	if err != nil {
		return err
	}
	ir, err := r.getIterator(iterable)
	if err != nil {
		return err
	}
	isOrig := adder == *orig
	argv := r.pushArgs(1)
	defer r.popArgs(1)
	for k := int64(0); ; k++ {
		if err := interruptEvery(r, k); err != nil {
			return err
		}
		v, done, err := ir.step(r)
		if err != nil || done {
			return err
		}
		if isOrig {
			err = direct(v)
		} else {
			argv[0] = v
			_, err = r.CallObject(adder, ObjectValue(target), argv)
		}
		if err != nil {
			return ir.closeThrow(r, err)
		}
	}
}

// collectionAdder returns Get(target, name), which must be callable.
func (r *Realm) collectionAdder(target *Object, name *String) (*Object, error) {
	adder, err := target.GetProp(r, StringKey(name))
	if err != nil {
		return nil, err
	}
	if !IsCallable(adder) {
		return nil, r.TypeError("'%s' returned for property '%s' of object '#<%s>' is not a function",
			r.DisplayString(adder), name.String(), classNames[target.class])
	}
	return adder.AsObject(), nil
}

func mapProtoClear(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisCollection(r, this, ClassMap, "Map.prototype.clear")
	if err != nil {
		return Undefined(), err
	}
	c.clear()
	return Undefined(), nil
}

func mapProtoDelete(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisCollection(r, this, ClassMap, "Map.prototype.delete")
	if err != nil {
		return Undefined(), err
	}
	return Bool(c.delete(Arg(args, 0))), nil
}

func mapProtoGet(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisCollection(r, this, ClassMap, "Map.prototype.get")
	if err != nil {
		return Undefined(), err
	}
	v, _ := c.get(Arg(args, 0))
	return v, nil
}

func mapProtoHas(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisCollection(r, this, ClassMap, "Map.prototype.has")
	if err != nil {
		return Undefined(), err
	}
	return Bool(c.has(Arg(args, 0))), nil
}

func mapProtoSet(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisCollection(r, this, ClassMap, "Map.prototype.set")
	if err != nil {
		return Undefined(), err
	}
	c.set(r, Arg(args, 0), Arg(args, 1))
	return this, nil
}

func mapProtoSize(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisCollection(r, this, ClassMap, "get Map.prototype.size")
	if err != nil {
		return Undefined(), err
	}
	return IntValue(c.size()), nil
}

// collectionForEach implements Map/Set.prototype.forEach: callback(value,
// key, collection) over the live list (Set passes the element twice).
func collectionForEach(r *Realm, this Value, args []Value, c *collection, isSet bool) (Value, error) {
	fn, err := callbackArg(r, args)
	if err != nil {
		return Undefined(), err
	}
	thisArg := Arg(args, 1)
	argv := r.pushArgs(3)
	defer r.popArgs(3)
	cur := c.cursor()
	for k := int64(0); ; k++ {
		if err := interruptEvery(r, k); err != nil {
			return Undefined(), err
		}
		key, value, ok := cur.next()
		if !ok {
			return Undefined(), nil
		}
		if isSet {
			value = key
		}
		argv[0], argv[1], argv[2] = value, key, this
		if _, err := r.CallObject(fn, thisArg, argv); err != nil {
			return Undefined(), err
		}
	}
}

func mapProtoForEach(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisCollection(r, this, ClassMap, "Map.prototype.forEach")
	if err != nil {
		return Undefined(), err
	}
	return collectionForEach(r, this, args, c, false)
}

func mapProtoEntries(r *Realm, this Value, args []Value) (Value, error) {
	return mapIterator(r, this, IterEntries, "Map.prototype.entries")
}

func mapProtoKeys(r *Realm, this Value, args []Value) (Value, error) {
	return mapIterator(r, this, IterKeys, "Map.prototype.keys")
}

func mapProtoValues(r *Realm, this Value, args []Value) (Value, error) {
	return mapIterator(r, this, IterValues, "Map.prototype.values")
}

// collIterData is the payload of Map and Set iterator objects.
type collIterData struct {
	cur  collCursor
	kind IterKind
}

// step advances the iterator: keys, values or [key, value] entries (a Set's
// value is its element).
func (d *collIterData) step(r *Realm, isSet bool) (Value, bool) {
	key, value, ok := d.cur.next()
	if !ok {
		return Undefined(), true
	}
	switch {
	case d.kind == IterKeys:
		return key, false
	case isSet && d.kind == IterEntries:
		return ObjectValue(r.NewArray(key, key)), false
	case isSet:
		return key, false
	case d.kind == IterEntries:
		return ObjectValue(r.NewArray(key, value)), false
	}
	return value, false
}

// collIterObject co-allocates a Map or Set iterator with its payload.
type collIterObject struct {
	obj  Object
	data collIterData
}

func (r *Realm) newCollIterator(proto *Object, class Class, c *collection, kind IterKind) *Object {
	it := &collIterObject{}
	r.chargeObject(unsafe.Sizeof(collIterObject{}))
	o := &it.obj
	o.shape = r.rootShapeFor(proto)
	o.class = class
	o.flags = flagExtensible
	it.data = collIterData{cur: c.cursor(), kind: kind}
	setData(o, kindCollIter, &it.data)
	return o
}

// mapIterator implements CreateMapIterator(this, kind).
func mapIterator(r *Realm, this Value, kind IterKind, method string) (Value, error) {
	c, err := thisCollection(r, this, ClassMap, method)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(r.newCollIterator(r.MapIteratorPrototype, ClassMapIterator, c, kind)), nil
}

// collIteratorNext is the shared next of the Map and Set iterators.
func collIteratorNext(r *Realm, this Value, class Class) (Value, error) {
	if this.IsObject() && this.AsObject().class == class {
		if d, ok := dataOk(this.AsObject(), kindCollIter); ok {
			v, done := d.step(r, class == ClassSetIterator)
			return r.createIterResult(v, done), nil
		}
	}
	return Undefined(), r.TypeError("next method called on incompatible receiver %s", r.DisplayString(this))
}

// mapIteratorNext implements %MapIteratorPrototype%.next.
func mapIteratorNext(r *Realm, this Value, args []Value) (Value, error) {
	return collIteratorNext(r, this, ClassMapIterator)
}

// mapGroupBy implements Map.groupBy(items, callback).
func mapGroupBy(r *Realm, this Value, args []Value) (Value, error) {
	var groups collection
	var lists [][]Value
	err := r.groupBy(Arg(args, 0), Arg(args, 1), func(k, v Value) error {
		k = canonicalCollKey(k)
		i, ok := groups.get(k)
		if !ok {
			i = IntValue(len(lists))
			groups.set(r, k, i)
			lists = append(lists, nil)
		}
		n := int(i.AsNumber())
		lists[n] = append(lists[n], v)
		return nil
	})
	if err != nil {
		return Undefined(), err
	}
	o, c := r.newCollectionObject(r.MapPrototype, ClassMap)
	cur := groups.cursor()
	for {
		k, i, ok := cur.next()
		if !ok {
			break
		}
		c.set(r, k, ObjectValue(r.NewArrayFromSlice(lists[int(i.AsNumber())])))
	}
	return ObjectValue(o), nil
}

// groupBy implements the iteration of GroupBy(items, callback, keyCoercion):
// add receives each callback result (not yet coerced) and its value; an
// error from add closes the iterator.
func (r *Realm) groupBy(items, callback Value, add func(k, v Value) error) error {
	if items.IsNullish() {
		return r.TypeError("groupBy called on %s", items.String())
	}
	if !IsCallable(callback) {
		return r.TypeError("%s is not a function", r.DisplayString(callback))
	}
	ir, err := r.getIterator(items)
	if err != nil {
		return err
	}
	fn := callback.AsObject()
	argv := r.pushArgs(2)
	defer r.popArgs(2)
	for k := int64(0); ; k++ {
		if err := interruptEvery(r, k); err != nil {
			return err
		}
		v, done, err := ir.step(r)
		if err != nil || done {
			return err
		}
		if k >= maxSafeInteger {
			return ir.closeThrow(r, r.TypeError("groupBy: too many elements"))
		}
		argv[0], argv[1] = v, Int64Value(k)
		key, err := r.CallObject(fn, Undefined(), argv)
		if err == nil {
			err = add(key, v)
		}
		if err != nil {
			return ir.closeThrow(r, err)
		}
	}
}

package engine

import (
	"slices"
	"unsafe"
)

// installObject fills the Object constructor and Object.prototype. It runs
// first in the install list because the abstract operations of every other
// builtin resolve Object.prototype.toString/valueOf.
func installObject(r *Realm) {
	r.installBuiltins(r.ObjectCtor, objectStaticMethods)
	r.ObjectPrototype.ReserveSlots(r, len(objectPrototypeMethods)+1) // and __proto__
	r.installBuiltins(r.ObjectPrototype, objectPrototypeMethods)
	r.installOrReplace(r.ObjectPrototype, StringKey(AtomDunderProto), propCell{
		value: accessorValue(&Accessor{
			Get: r.NewNativeFunction(FromGoString("get __proto__"), 0, objectProtoGetProto),
			Set: r.NewNativeFunction(FromGoString("set __proto__"), 1, objectProtoSetProto),
		}),
		attrs: attrConfigurable | attrAccessor,
	})
}

var objectStaticMethods = []builtinDef{
	{AtomAssign, objectAssign, 2},
	{AtomCreate, objectCreate, 2},
	{AtomDefineProperties, objectDefinePropertiesStatic, 2},
	{AtomDefineProperty, objectDefineProperty, 3},
	{AtomEntries, objectEntries, 1},
	{AtomFreeze, objectFreeze, 1},
	{AtomFromEntries, objectFromEntries, 1},
	{AtomGetOwnPropertyDescriptor, objectGetOwnPropertyDescriptor, 2},
	{AtomGetOwnPropertyDescriptors, objectGetOwnPropertyDescriptors, 1},
	{AtomGetOwnPropertyNames, objectGetOwnPropertyNames, 1},
	{AtomGetOwnPropertySymbols, objectGetOwnPropertySymbols, 1},
	{AtomGetPrototypeOf, objectGetPrototypeOf, 1},
	{AtomGroupBy, objectGroupBy, 2},
	{AtomHasOwn, objectHasOwn, 2},
	{AtomIs, objectIs, 2},
	{AtomIsExtensible, objectIsExtensible, 1},
	{AtomIsFrozen, objectIsFrozen, 1},
	{AtomIsSealed, objectIsSealed, 1},
	{AtomKeys, objectKeys, 1},
	{AtomPreventExtensions, objectPreventExtensions, 1},
	{AtomSeal, objectSeal, 1},
	{AtomSetPrototypeOf, objectSetPrototypeOf, 2},
	{AtomValues, objectValues, 1},
}

var objectPrototypeMethods = []builtinDef{
	{AtomDefineGetter, objectProtoDefineGetter, 2},
	{AtomDefineSetter, objectProtoDefineSetter, 2},
	{AtomLookupGetter, objectProtoLookupGetter, 1},
	{AtomLookupSetter, objectProtoLookupSetter, 1},
	{AtomHasOwnProperty, objectProtoHasOwnProperty, 1},
	{AtomIsPrototypeOf, objectProtoIsPrototypeOf, 1},
	{AtomPropertyIsEnumerable, objectProtoPropertyIsEnumerable, 1},
	{AtomToLocaleString, objectProtoToLocaleString, 0},
	{AtomToString, objectProtoToString, 0},
	{AtomValueOf, objectProtoValueOf, 0},
}

// objectProtoGetProto implements the Object.prototype.__proto__ getter
// (Annex B.2.2.1.1).
func objectProtoGetProto(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(this)
	if err != nil {
		return Undefined(), err
	}
	proto, err := r.getPrototypeOf(o)
	if err != nil || proto == nil {
		return Null(), err
	}
	return ObjectValue(proto), nil
}

// objectProtoSetProto implements the Object.prototype.__proto__ setter
// (Annex B.2.2.1.2): a primitive receiver or a value that is neither an
// object nor null is ignored.
func objectProtoSetProto(r *Realm, this Value, args []Value) (Value, error) {
	if this.IsNullish() {
		return Undefined(), r.TypeError("Object.prototype.__proto__ called on null or undefined")
	}
	v := Arg(args, 0)
	if !this.IsObject() || !v.IsObject() && !v.IsNull() {
		return Undefined(), nil
	}
	var proto *Object
	if v.IsObject() {
		proto = v.AsObject()
	}
	o := this.AsObject()
	if ok, err := r.setPrototypeOf(o, proto); err != nil || !ok {
		return Undefined(), setPrototypeError(r, o, err)
	}
	return Undefined(), nil
}

// setPrototypeError is the TypeError for a failed [[SetPrototypeOf]], or
// err when the operation threw.
func setPrototypeError(r *Realm, o *Object, err error) error {
	switch {
	case err != nil:
		return err
	case o.class == ClassProxy && nsOf(o) == nil:
		return r.TypeError("'setPrototypeOf' on proxy: trap returned falsish")
	case o == r.ObjectPrototype:
		return r.TypeError("Immutable prototype object 'Object.prototype' cannot have their prototype set")
	case !o.IsExtensible():
		return r.TypeError("%s is not extensible", r.DisplayString(ObjectValue(o)))
	}
	return r.TypeError("Cyclic __proto__ value")
}

// --- constructor ------------------------------------------------------------------

// objectCall implements Object(value).
func objectCall(r *Realm, this Value, args []Value) (Value, error) {
	return objectConstruct(r, args, nil)
}

// objectConstruct implements new Object(value): a fresh object for a missing
// or nullish argument, ToObject otherwise, and OrdinaryCreateFromConstructor
// for subclass newTargets.
func objectConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	if newTarget != nil && newTarget != r.ObjectCtor {
		o, err := r.OrdinaryCreateFromConstructor(newTarget, r.ObjectPrototype, ClassObject)
		if err != nil {
			return Undefined(), err
		}
		return ObjectValue(o), nil
	}
	v := Arg(args, 0)
	if v.IsNullish() {
		return ObjectValue(r.NewObject()), nil
	}
	o, err := r.ToObject(v)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

// --- static methods -----------------------------------------------------------------

// hasOnlyShapeProps reports whether every own property of o lives in its
// shape (no elements, sparse or dictionary storage, no lazy entries, no
// exotic string indices), so the shape's property list is the complete
// [[OwnPropertyKeys]]. A host placeholder is materialized first: every caller
// is about to read the properties.
func hasOnlyShapeProps(o *Object) bool {
	if o.flags&flagHasLazy != 0 {
		o.materializeHost()
	}
	return o.class == ClassObject && o.flags&(flagDict|flagHasLazy) == 0 && o.elemLen() == 0 && (o.dictTable() == nil || o.dictTable().sparse == nil)
}

// objectKeys implements Object.keys.
func objectKeys(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if hasOnlyShapeProps(o) {
		props := o.shape.Props()
		n := 0
		for i := range props {
			if props[i].attrs&attrEnumerable != 0 && props[i].key.IsString() {
				n++
			}
		}
		ao, items := r.newArrayStorage(n, n)
		n = 0
		for i := range props {
			if props[i].attrs&attrEnumerable != 0 && props[i].key.IsString() {
				items[n] = props[i].key.Value()
				n++
			}
		}
		return ObjectValue(r.initArray(ao, items, uint32(n))), nil
	}
	if o.class == ClassObject && o.flags&(flagDict|flagHasLazy) == flagDict {
		return r.dictObjectKeys(o)
	}
	keys, err := r.enumerableOwnKeys(o)
	if err != nil {
		return Undefined(), err
	}
	ao, items := r.newArrayStorage(len(keys), len(keys))
	for i, k := range keys {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		items[i] = StringValue(k.ToJSString(r))
	}
	return ObjectValue(r.initArray(ao, items, uint32(len(keys)))), nil
}

// dictObjectKeys is Object.keys of an ordinary object in dictionary mode
// (an Intl polyfill's tables of hundreds of keys): it fills the array from
// the dictionary's entries, after the integer keys, where
// enumerableOwnKeys builds the list of keys first.
func (r *Realm) dictObjectKeys(o *Object) (Value, error) {
	var idx []PropertyKey
	if o.elemLen() != 0 || o.dictTable().sparse != nil {
		idx = o.appendEnumerableIndexKeys(nil)
	}
	d := o.dictTable()
	n := len(idx)
	for i := range d.entries {
		if e := &d.entries[i]; e.live() && e.cell.attrs&attrEnumerable != 0 && e.key.IsString() {
			n++
		}
	}
	ao, items := r.newArrayStorage(n, n)
	for i, k := range idx {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		items[i] = StringValue(k.ToJSString(r))
	}
	n = len(idx)
	for i := range d.entries {
		if e := &d.entries[i]; e.live() && e.cell.attrs&attrEnumerable != 0 && e.key.IsString() {
			items[n] = e.key.Value()
			n++
		}
	}
	return ObjectValue(r.initArray(ao, items, uint32(n))), nil
}

// objectValues implements Object.values.
func objectValues(r *Realm, this Value, args []Value) (Value, error) {
	return enumerableOwnProperties(r, Arg(args, 0), false)
}

// objectEntries implements Object.entries.
func objectEntries(r *Realm, this Value, args []Value) (Value, error) {
	return enumerableOwnProperties(r, Arg(args, 0), true)
}

// enumerableOwnProperties implements EnumerableOwnProperties(O, value) and
// (O, key+value). The keys are o's own string keys when it starts,
// enumerable or not, and each key's property is checked at its turn: a
// getter may have removed a later one or made it enumerable.
func enumerableOwnProperties(r *Realm, v Value, entries bool) (Value, error) {
	o, err := r.ToObject(v)
	if err != nil {
		return Undefined(), err
	}
	proxy := o.class == ClassProxy
	var keys []PropertyKey
	var ps []shapeProp
	var s0 *Shape
	n := 0
	if proxy {
		if keys, err = r.proxyOwnKeys(o); err != nil {
			return Undefined(), err
		}
		n = len(keys)
	} else if hasOnlyShapeProps(o) {
		// The keys are those of o's shape now, read in place. While o keeps
		// that shape their attributes are the shape's; a getter that
		// changes o gives it another shape.
		s0 = o.shape
		ps = s0.Props()
		for i := range ps {
			if ps[i].attrs&attrEnumerable != 0 && ps[i].key.IsString() {
				n++
			}
		}
	} else {
		if o.class == ClassTypedArray {
			if err := r.typedArrayKeysLimit(o); err != nil {
				return Undefined(), err
			}
		}
		keys = o.ownStringKeys()
		n = len(keys)
	}
	// Each entry takes its two elements in its own allocation, and the
	// result its own where that costs no more bytes than a separate slice:
	// an even count up to smallArrayMax and a count past it that coArray
	// takes (newArrayStorage).
	var res *arrayObject
	var items []Value
	if n == 0 || n%2 != 0 && n < smallArrayMax {
		res, items = &arrayObject{}, make([]Value, 0, n)
	} else {
		res, items = r.newArrayStorage(0, n)
	}
	self := ObjectValue(o)
	for i := range len(keys) + len(ps) {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		var k PropertyKey
		if ps == nil {
			k = keys[i]
		} else if k = ps[i].key; !k.IsString() || o.shape == s0 && ps[i].attrs&attrEnumerable == 0 {
			continue
		}
		var val Value
		if proxy {
			// The descriptor and the value of each key are read in turn.
			if k.IsSymbol() {
				continue
			}
			d, ok, err := r.proxyGetOwnProperty(o, k)
			if err != nil {
				return Undefined(), err
			}
			if !ok || !d.Enumerable() {
				continue
			}
			if val, err = r.proxyGet(o, k, self); err != nil {
				return Undefined(), err
			}
		} else {
			c, ok := o.getOwnCell(k)
			if !ok || c.attrs&attrEnumerable == 0 {
				continue
			}
			val = c.value
			if c.attrs&attrAccessor != 0 {
				if val, err = o.Get(r, k, self); err != nil {
					return Undefined(), err
				}
			}
		}
		if !entries {
			items = append(items, val)
			continue
		}
		ao, pair := r.newArrayStorage(2, 2)
		pair[0], pair[1] = StringValue(k.ToJSString(r)), val
		items = append(items, ObjectValue(r.initArray(ao, pair, 2)))
	}
	return ObjectValue(r.initArray(res, items, uint32(len(items)))), nil
}

// objectAssign implements Object.assign(target, ...sources).
func objectAssign(r *Realm, this Value, args []Value) (Value, error) {
	to, err := r.ToObject(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	for i := 1; i < len(args); i++ {
		if args[i].IsNullish() {
			continue
		}
		from, err := r.ToObject(args[i])
		if err != nil {
			return Undefined(), err
		}
		if err := assignProperties(r, to, from); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(to), nil
}

// assignProperties copies the own enumerable properties of from onto to with
// [[Set]] semantics (one source step of Object.assign).
func assignProperties(r *Realm, to, from *Object) error {
	if hasOnlyShapeProps(from) {
		props := from.shape.Props()
		if to.flags&(flagDict|flagShared) == 0 && to.class != ClassProxy {
			to.ReserveSlots(r, len(props))
		}
		// [[OwnPropertyKeys]] order: strings in creation order, then symbols.
		symbols := false
		for i := range props {
			if props[i].key.IsSymbol() {
				symbols = true
				continue
			}
			if err := assignProperty(r, to, from, props[i].key); err != nil {
				return err
			}
		}
		for i := 0; symbols && i < len(props); i++ {
			if !props[i].key.IsSymbol() {
				continue
			}
			if err := assignProperty(r, to, from, props[i].key); err != nil {
				return err
			}
		}
		return nil
	}
	if from.class == ClassProxy {
		return assignProxyProperties(r, to, from)
	}
	keys, err := r.ownPropertyKeys(from)
	if err != nil {
		return err
	}
	for i, k := range keys {
		if err := interruptEvery(r, int64(i)); err != nil {
			return err
		}
		if err := assignProperty(r, to, from, k); err != nil {
			return err
		}
	}
	return nil
}

// assignProxyProperties is assignProperties for a proxy source: the
// ownKeys, getOwnPropertyDescriptor and get traps pick the values.
func assignProxyProperties(r *Realm, to, from *Object) error {
	keys, err := r.proxyOwnKeys(from)
	if err != nil {
		return err
	}
	for _, k := range keys {
		d, ok, err := r.proxyGetOwnProperty(from, k)
		if err != nil {
			return err
		}
		if !ok || !d.Enumerable() {
			continue
		}
		v, err := r.proxyGet(from, k, ObjectValue(from))
		if err != nil {
			return err
		}
		if ok, err := to.Set(r, k, v, ObjectValue(to)); err != nil {
			return err
		} else if !ok {
			return r.readOnlyError(to, k)
		}
	}
	return nil
}

// assignProperty copies one key if it is still an own enumerable property
// of from; a rejected [[Set]] is a TypeError as Object.assign requires.
func assignProperty(r *Realm, to, from *Object, k PropertyKey) error {
	c, ok := from.getOwnCell(k)
	if !ok || c.attrs&attrEnumerable == 0 {
		return nil
	}
	v := c.value
	if c.attrs&attrAccessor != 0 {
		var err error
		if v, err = from.Get(r, k, ObjectValue(from)); err != nil {
			return err
		}
	}
	ok, err := to.Set(r, k, v, ObjectValue(to))
	if err != nil {
		return err
	}
	if !ok {
		return r.readOnlyError(to, k)
	}
	return nil
}

// objectFreeze implements Object.freeze.
func objectFreeze(r *Realm, this Value, args []Value) (Value, error) {
	return objectSetIntegrity(r, Arg(args, 0), true)
}

// objectIsFrozen implements Object.isFrozen (primitives are frozen).
func objectIsFrozen(r *Realm, this Value, args []Value) (Value, error) {
	return objectTestIntegrity(r, Arg(args, 0), true)
}

// objectSeal implements Object.seal.
func objectSeal(r *Realm, this Value, args []Value) (Value, error) {
	return objectSetIntegrity(r, Arg(args, 0), false)
}

// objectIsSealed implements Object.isSealed (primitives are sealed).
func objectIsSealed(r *Realm, this Value, args []Value) (Value, error) {
	return objectTestIntegrity(r, Arg(args, 0), false)
}

// objectSetIntegrity is Object.freeze (frozen) or Object.seal: the object
// methods for an ordinary object, SetIntegrityLevel for a proxy.
func objectSetIntegrity(r *Realm, v Value, frozen bool) (Value, error) {
	if !v.IsObject() {
		return v, nil
	}
	o := v.AsObject()
	switch {
	case o.class == ClassProxy:
		if ok, err := r.setIntegrityLevel(o, frozen); err != nil {
			return Undefined(), err
		} else if !ok {
			return Undefined(), r.TypeError("'preventExtensions' on proxy: trap returned falsish")
		}
	case o.class == ClassTypedArray:
		if err := r.typedArraySetIntegrity(o, frozen); err != nil {
			return Undefined(), err
		}
	case frozen:
		o.Freeze(r)
	default:
		o.Seal(r)
	}
	return v, nil
}

// objectTestIntegrity is Object.isFrozen (frozen) or Object.isSealed.
func objectTestIntegrity(r *Realm, v Value, frozen bool) (Value, error) {
	if !v.IsObject() {
		return True(), nil
	}
	o := v.AsObject()
	switch {
	case o.class == ClassProxy:
		ok, err := r.testIntegrityLevel(o, frozen)
		return Bool(ok), err
	case frozen:
		return Bool(o.IsFrozen()), nil
	}
	return Bool(o.IsSealed()), nil
}

// objectPreventExtensions implements Object.preventExtensions.
func objectPreventExtensions(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	if v.IsObject() {
		if ok, err := r.preventExtensions(v.AsObject()); err != nil {
			return Undefined(), err
		} else if !ok && v.AsObject().class == ClassTypedArray {
			return Undefined(), r.TypeError("Cannot prevent extensions on a length-tracking typed array or one over a resizable buffer")
		} else if !ok {
			return Undefined(), r.TypeError("'preventExtensions' on proxy: trap returned falsish")
		}
	}
	return v, nil
}

// objectIsExtensible implements Object.isExtensible (primitives are not).
func objectIsExtensible(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	if !v.IsObject() {
		return False(), nil
	}
	ok, err := r.isExtensible(v.AsObject())
	return Bool(ok), err
}

// objectGetPrototypeOf implements Object.getPrototypeOf.
func objectGetPrototypeOf(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	proto, err := r.getPrototypeOf(o)
	if err != nil || proto == nil {
		return Null(), err
	}
	return ObjectValue(proto), nil
}

// objectSetPrototypeOf implements Object.setPrototypeOf.
func objectSetPrototypeOf(r *Realm, this Value, args []Value) (Value, error) {
	o := Arg(args, 0)
	if o.IsNullish() {
		return Undefined(), r.TypeError("Object.setPrototypeOf called on null or undefined")
	}
	proto, err := protoArgument(r, Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	if !o.IsObject() {
		return o, nil
	}
	obj := o.AsObject()
	if ok, err := r.setPrototypeOf(obj, proto); err != nil || !ok {
		return Undefined(), setPrototypeError(r, obj, err)
	}
	return o, nil
}

// protoArgument validates an object-or-null prototype argument.
func protoArgument(r *Realm, v Value) (*Object, error) {
	switch {
	case v.IsObject():
		return v.AsObject(), nil
	case v.IsNull():
		return nil, nil
	}
	return nil, r.TypeError("Object prototype may only be an Object or null: %s", r.DisplayString(v))
}

// objectCreate implements Object.create(proto, properties).
func objectCreate(r *Realm, this Value, args []Value) (Value, error) {
	proto, err := protoArgument(r, Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	props := Arg(args, 1)
	n := 0
	if props.IsObject() {
		n = props.AsObject().keyCap() - 1 // its own keys, which it defines on o
		if pd, ok := extDataOk(props.AsObject(), kindPendingDescs); ok {
			n = len(pd.props)
		}
	}
	o := r.createObject(proto, n)
	if !props.IsUndefined() {
		if err := r.objectDefineProperties(o, props); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(o), nil
}

// objectDefineProperties implements ObjectDefineProperties: every
// descriptor is read and validated before any property is defined.
func (r *Realm) objectDefineProperties(o *Object, properties Value) error {
	props, err := r.ToObject(properties)
	if err != nil {
		return err
	}
	if pd, ok := extDataOk(props, kindPendingDescs); ok && pd.definesOn(o, props) {
		return pd.define(o)
	}
	if r.descriptorsInShape(o, props) {
		return r.defineFromShape(o, props)
	}
	keys, err := r.ownPropertyKeys(props)
	if err != nil {
		return err
	}
	type pending struct {
		key  PropertyKey
		desc PropertyDescriptor
	}
	list := make([]pending, 0, len(keys))
	for _, k := range keys {
		if enum, err := r.isOwnEnumerable(props, k); err != nil {
			return err
		} else if !enum {
			continue
		}
		descObj, err := props.GetProp(r, k)
		if err != nil {
			return err
		}
		desc, err := r.toPropertyDescriptor(descObj)
		if err != nil {
			return err
		}
		list = append(list, pending{k, desc})
	}
	for _, p := range list {
		if err := o.DefinePropertyOrThrow(r, p.key, p.desc); err != nil {
			return err
		}
	}
	return nil
}

// descriptorsInShape reports whether objectDefineProperties may define
// props' properties on o as it reads them, without the lists of keys and
// of descriptors it builds first: props is an ordinary object in shape
// mode whose enumerable properties are data properties, each holding a
// descriptor object pureDescriptor reads, and o, an ordinary object or a
// function, is neither props, nor one of them, nor a prototype. Reading
// them runs no code and cannot throw, and defining on o runs none and
// changes neither them nor their prototypes, so each read after a
// definition gives what it gave before.
func (r *Realm) descriptorsInShape(o, props *Object) bool {
	if o.class != ClassObject && o.class != ClassFunction || o.flags&flagIsPrototype != 0 || o == props || !hasOnlyShapeProps(props) {
		return false
	}
	data, acc := r.descriptorShape(false), r.descriptorShape(true)
	ps := props.shape.Props()
	for i := range ps {
		if ps[i].attrs&attrEnumerable == 0 {
			continue
		}
		v := props.slots[i]
		if ps[i].attrs&(attrAccessor|attrLazy) != 0 || !v.IsObject() || v.AsObject() == o {
			return false
		}
		if _, ok := r.pureDescriptor(v.AsObject(), data, acc); !ok {
			return false
		}
	}
	return true
}

// defineFromShape defines on o the properties of props that
// descriptorsInShape accepted, in [[OwnPropertyKeys]] order: the strings
// and then the symbols, in creation order.
func (r *Realm) defineFromShape(o, props *Object) error {
	data, acc := r.descriptorShape(false), r.descriptorShape(true)
	ps := props.shape.Props()
	for _, strings := range [2]bool{true, false} {
		for i := range ps {
			if ps[i].attrs&attrEnumerable == 0 || ps[i].key.IsString() != strings {
				continue
			}
			desc, _ := r.pureDescriptor(props.slots[i].AsObject(), data, acc)
			if err := o.DefinePropertyOrThrow(r, ps[i].key, desc); err != nil {
				return err
			}
		}
	}
	return nil
}

// descFields are the fields of a descriptor object, in the order
// toPropertyDescriptor reads them.
var descFields = [...]*String{AtomEnumerable, AtomConfigurable, AtomValue, AtomWritable, AtomGet, AtomSet}

// pureDescriptor returns what toPropertyDescriptor reads from d when that
// runs no code and cannot throw: d is an ordinary object in shape mode
// whose fields are data properties, whose prototypes hold none of the
// fields it lacks, and whose getter and setter, if any, are callable or
// undefined and come without a value or writable. Of an object of data or
// acc, the shapes of FromPropertyDescriptor's objects, it reads the slots
// in place; of another, it finds the fields in one walk down its shape.
// It reports false otherwise.
func (r *Realm) pureDescriptor(d *Object, data, acc *Shape) (PropertyDescriptor, bool) {
	var desc PropertyDescriptor
	if d.class != ClassObject || d.flags&(flagDict|flagHasLazy) != 0 {
		return desc, false
	}
	var have uint8 // the fields d has, by their index in descFields
	switch d.shape {
	case data:
		desc.SetEnumerable(ToBoolean(d.slots[2]))
		desc.SetConfigurable(ToBoolean(d.slots[3]))
		desc.SetValue(d.slots[0])
		desc.SetWritable(ToBoolean(d.slots[1]))
		have = 0b1111
	case acc:
		desc.SetEnumerable(ToBoolean(d.slots[2]))
		desc.SetConfigurable(ToBoolean(d.slots[3]))
		desc.SetGet(d.slots[0])
		desc.SetSet(d.slots[1])
		have = 0b110011
	default:
		// Down the shape's chain: d's shape may have no list of its
		// properties yet, which Props would allocate.
		for n := d.shape; n.count != 0; n = n.parent {
			f := descField(n.key)
			if f < 0 {
				continue
			}
			if n.attrs&attrAccessor != 0 {
				return desc, false
			}
			have |= 1 << f
			switch v := d.slots[n.slot]; f {
			case 0:
				desc.SetEnumerable(ToBoolean(v))
			case 1:
				desc.SetConfigurable(ToBoolean(v))
			case 2:
				desc.SetValue(v)
			case 3:
				desc.SetWritable(ToBoolean(v))
			case 4:
				desc.SetGet(v)
			case 5:
				desc.SetSet(v)
			}
		}
	}
	if have != 1<<len(descFields)-1 {
		if proto := d.Proto(); !r.lacksDescFields(proto) {
			for f, name := range descFields {
				if have&(1<<f) == 0 && chainMayHave(proto, StringKey(name)) {
					return desc, false
				}
			}
		}
	}
	if desc.present&descHasGet != 0 && !IsCallable(desc.Get) && !desc.Get.IsUndefined() ||
		desc.present&descHasSet != 0 && !IsCallable(desc.Set) && !desc.Set.IsUndefined() ||
		desc.IsAccessorDescriptor() && desc.IsDataDescriptor() {
		return desc, false
	}
	return desc, true
}

// descField returns the index of key in descFields, or -1.
func descField(key PropertyKey) int {
	for f, name := range descFields {
		if key == StringKey(name) {
			return f
		}
	}
	return -1
}

// lacksDescFields reports whether p is the realm's Object.prototype and
// holds none of descFields, so a descriptor object of Object.prototype
// inherits none of them. A positive answer is kept for the prototype epoch
// it was found in: adding a property to Object.prototype bumps it, and
// Object.prototype's own [[Prototype]] is always null.
func (r *Realm) lacksDescFields(p *Object) bool {
	if p != r.ObjectPrototype {
		return false
	}
	lz := r.lazyState()
	if lz.descEpoch == r.protoEpoch+1 {
		return true
	}
	for _, name := range descFields {
		if p.HasOwnProperty(StringKey(name)) {
			return false
		}
	}
	lz.descEpoch = r.protoEpoch + 1
	return true
}

// objectDefinePropertiesStatic implements Object.defineProperties(o, props).
func objectDefinePropertiesStatic(r *Realm, this Value, args []Value) (Value, error) {
	o := Arg(args, 0)
	if !o.IsObject() {
		return Undefined(), r.TypeError("Object.defineProperties called on non-object")
	}
	if err := r.objectDefineProperties(o.AsObject(), Arg(args, 1)); err != nil {
		return Undefined(), err
	}
	return o, nil
}

// objectDefineProperty implements Object.defineProperty(o, key, attributes).
func objectDefineProperty(r *Realm, this Value, args []Value) (Value, error) {
	o := Arg(args, 0)
	if !o.IsObject() {
		return Undefined(), r.TypeError("Object.defineProperty called on non-object")
	}
	key, err := r.ToPropertyKey(Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	desc, err := r.toPropertyDescriptor(Arg(args, 2))
	if err != nil {
		return Undefined(), err
	}
	if err := o.AsObject().DefinePropertyOrThrow(r, key, desc); err != nil {
		return Undefined(), err
	}
	return o, nil
}

// toPropertyDescriptor implements ToPropertyDescriptor, reading the fields
// in spec order (enumerable, configurable, value, writable, get, set).
func (r *Realm) toPropertyDescriptor(v Value) (PropertyDescriptor, error) {
	var d PropertyDescriptor
	if !v.IsObject() {
		return d, r.TypeError("Property description must be an object: %s", r.DisplayString(v))
	}
	o := v.AsObject()
	if d, ok := r.pureDescriptor(o, nil, nil); ok {
		return d, nil
	}
	field := func(name *String) (Value, bool, error) {
		k := StringKey(name)
		if has, err := r.hasProperty(o, k); err != nil || !has {
			return Undefined(), false, err
		}
		val, err := o.GetProp(r, k)
		return val, err == nil, err
	}
	val, ok, err := field(AtomEnumerable)
	if err != nil {
		return d, err
	}
	if ok {
		d.SetEnumerable(ToBoolean(val))
	}
	if val, ok, err = field(AtomConfigurable); err != nil {
		return d, err
	} else if ok {
		d.SetConfigurable(ToBoolean(val))
	}
	if val, ok, err = field(AtomValue); err != nil {
		return d, err
	} else if ok {
		d.SetValue(val)
	}
	if val, ok, err = field(AtomWritable); err != nil {
		return d, err
	} else if ok {
		d.SetWritable(ToBoolean(val))
	}
	if val, ok, err = field(AtomGet); err != nil {
		return d, err
	} else if ok {
		if err := r.CheckAccessorFunction(val, false); err != nil {
			return d, err
		}
		d.SetGet(val)
	}
	if val, ok, err = field(AtomSet); err != nil {
		return d, err
	} else if ok {
		if err := r.CheckAccessorFunction(val, true); err != nil {
			return d, err
		}
		d.SetSet(val)
	}
	if d.IsAccessorDescriptor() && d.IsDataDescriptor() {
		return d, r.TypeError("Invalid property descriptor. Cannot both specify accessors and a value or writable attribute")
	}
	return d, nil
}

// objectGetOwnPropertyDescriptor implements Object.getOwnPropertyDescriptor.
func objectGetOwnPropertyDescriptor(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	key, err := r.ToPropertyKey(Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	d, ok, err := r.getOwnProperty(o, key)
	if err != nil || !ok {
		return Undefined(), err
	}
	return r.FromPropertyDescriptor(d), nil
}

// objectGetOwnPropertyDescriptors implements
// Object.getOwnPropertyDescriptors.
func objectGetOwnPropertyDescriptors(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	if hasOnlyShapeProps(o) {
		// The keys are the shape's, read in place: [[GetOwnProperty]] of an
		// ordinary object runs no code that could change them.
		ps := o.shape.Props()
		if res := r.pendingDescriptors(o, ps); res != nil {
			return ObjectValue(res), nil
		}
		res := r.NewObjectCap(len(ps))
		for _, strings := range [2]bool{true, false} {
			for i := range ps {
				if k := ps[i].key; k.IsString() == strings && !k.IsPrivate() {
					d, _ := o.GetOwnProperty(k)
					res.addProp(r, k, propCell{value: r.FromPropertyDescriptor(d), attrs: attrDefault})
				}
			}
		}
		return ObjectValue(res), nil
	}
	keys, err := r.ownPropertyKeys(o)
	if err != nil {
		return Undefined(), err
	}
	res := r.NewObjectCap(len(keys))
	for _, k := range keys {
		d, ok, err := r.getOwnProperty(o, k)
		if err != nil {
			return Undefined(), err
		}
		if !ok {
			continue
		}
		res.addProp(r, k, propCell{value: r.FromPropertyDescriptor(d), attrs: attrDefault})
	}
	return ObjectValue(res), nil
}

// pendingDescs is the internal of an object Object.getOwnPropertyDescriptors
// made, while the descriptor objects it holds are still to make: props are
// the properties of the shape of the object it read, all data properties
// of the shape (no accessor, lazy slot or private name), with the shape's
// lookup table when it had one, and vals were their values, by slot. It
// keeps the shape's list of keys and attributes, not the shape, which
// would keep the object's prototype and its whole chain (a class's
// methods, what they close over) until the descriptors are made. vals is the object's own slot storage,
// which its slots leave empty meanwhile. The object has the realm's
// pending-descriptors root, which no other object has and on which no
// inline cache is filled (noFillAll), and flagHasLazy. A lookup of one of
// src's keys, a write, redefinition or deletion of any key, its key list,
// a prototype change or preventing extensions make the descriptor objects
// and define them in its storage (resolve); a lookup of any other key
// finds it as it is, without properties. Object.defineProperties and
// Object.create read the descriptors from src and vals instead (define):
// the clone of an object by Object.create(Object.getPrototypeOf(o),
// Object.getOwnPropertyDescriptors(o)) makes none of them.
type pendingDescs struct {
	r     *Realm
	props []shapeProp
	table *shapeIndex
	vals  []Value
}

// pendingDescriptors returns the result of Object.getOwnPropertyDescriptors
// for o, whose properties are those of its shape, ps, with its descriptor
// objects pending, or nil when one of them is an accessor, a lazy slot or
// a private name, or o has fewer than two (where the internal would cost
// as much as the descriptor objects), or the realm is a shared one.
func (r *Realm) pendingDescriptors(o *Object, ps []shapeProp) *Object {
	if r.sharedIntrinsics || len(ps) < 2 {
		return nil
	}
	for i := range ps {
		if ps[i].attrs&(attrAccessor|attrLazy) != 0 || ps[i].key.IsPrivate() {
			return nil
		}
	}
	res := r.NewObjectCap(len(ps))
	vals := res.slots[:len(ps)]
	copy(vals, o.slots)
	res.slots = nil
	r.chargeObject(unsafe.Sizeof(pendingDescs{}))
	setExtData(res, kindPendingDescs, &pendingDescs{r: r, props: ps, table: o.shape.table, vals: vals})
	l := r.shapeState()
	if l.pendingDesc == nil {
		l.pendingDesc = newRootShape(r.ObjectPrototype)
		l.pendingDesc.noFill = noFillAll
	}
	res.shape = l.pendingDesc
	res.flags |= flagHasLazy
	return res
}

// makePendingDescs makes o's descriptor objects when they are pending,
// before a path that adds to o without the lazy hooks (a private element,
// a host's property known to be absent): the add would move o off the
// pending root, on which no inline cache is filled, with its descriptors
// still to make, and a site would cache what o's prototype has for them.
func (o *Object) makePendingDescs() {
	if o.flags&flagHasLazy != 0 {
		if pd, ok := extDataOk(o, kindPendingDescs); ok {
			pd.resolve(o)
		}
	}
}

// has reports whether key is one of the object's pending properties.
func (pd *pendingDescs) has(key PropertyKey) bool {
	if pd.table != nil {
		return pd.table.find(pd.props, key) != 0
	}
	for i := range pd.props {
		if pd.props[i].key == key {
			return true
		}
	}
	return false
}

// resolve makes the descriptor objects of o, whose internal pd is, and
// defines them in o's storage, in [[OwnPropertyKeys]] order: the strings,
// then the symbols, in creation order. Each descriptor takes the slot of
// the value it holds, which it has read, unless a symbol comes before a
// string: then the values are copied first.
func (pd *pendingDescs) resolve(o *Object) {
	r, vals, ps := pd.r, pd.vals, pd.props
	o.clearPayload()
	o.flags &^= flagHasLazy
	sorted := true
	for i := 1; i < len(ps); i++ {
		if ps[i].key.IsString() && !ps[i-1].key.IsString() {
			sorted = false
		}
	}
	if !sorted {
		vals = slices.Clone(vals)
	}
	if o.flags&flagDict == 0 && o.shape.count == 0 {
		o.shape, o.slots = r.plainRoot, pd.vals[:0]
	}
	for _, strings := range [2]bool{true, false} {
		for i := range ps {
			if ps[i].key.IsString() == strings {
				d := r.FromPropertyDescriptor(DataDescriptor(vals[i], ps[i].attrs))
				o.addProp(r, ps[i].key, propCell{value: d, attrs: attrDefault})
			}
		}
	}
}

// definesOn reports whether objectDefineProperties may define on o the
// properties of props, whose internal pd is, from src and vals: o is an
// ordinary object or a function other than props, on which a definition
// runs no code that could resolve props, and Object.prototype, the
// prototype of the descriptor objects, has none of their fields, so that
// each would read as the complete descriptor of the source's property.
func (pd *pendingDescs) definesOn(o, props *Object) bool {
	return (o.class == ClassObject || o.class == ClassFunction) && o.flags&flagHasLazy == 0 && o != props && pd.r.lacksDescFields(pd.r.ObjectPrototype)
}

// define defines on o the properties pd holds the descriptors of, in
// [[OwnPropertyKeys]] order.
func (pd *pendingDescs) define(o *Object) error {
	ps := pd.props
	for _, strings := range [2]bool{true, false} {
		for i := range ps {
			if ps[i].key.IsString() == strings {
				if err := o.DefinePropertyOrThrow(pd.r, ps[i].key, DataDescriptor(pd.vals[i], ps[i].attrs)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// descriptorShapeFrom returns the shape of a complete data (value,
// writable, enumerable, configurable) or accessor (get, set, enumerable,
// configurable) descriptor object below root.
func descriptorShapeFrom(r *Realm, root *Shape, accessor bool) *Shape {
	k0, k1 := AtomValue, AtomWritable
	if accessor {
		k0, k1 = AtomGet, AtomSet
	}
	return root.addProperty(r, StringKey(k0), attrDefault).addProperty(r, StringKey(k1), attrDefault).
		addProperty(r, StringKey(AtomEnumerable), attrDefault).addProperty(r, StringKey(AtomConfigurable), attrDefault)
}

// descriptorShape returns the realm's descriptor object shape: four
// transitions from the plain root, looked up once, or the template's
// process-wide shapes in a shared realm.
func (r *Realm) descriptorShape(accessor bool) *Shape {
	if r.sharedIntrinsics {
		if accessor {
			return sharedTpl.accessorDescShape
		}
		return sharedTpl.dataDescShape
	}
	l := r.shapeState()
	if accessor {
		if l.accDesc == nil {
			l.accDesc = descriptorShapeFrom(r, r.plainRoot, true)
		}
		return l.accDesc
	}
	if l.dataDesc == nil {
		l.dataDesc = descriptorShapeFrom(r, r.plainRoot, false)
	}
	return l.dataDesc
}

// FromPropertyDescriptor implements FromPropertyDescriptor for a complete
// descriptor ([[GetOwnProperty]] results) in one allocation.
func (r *Realm) FromPropertyDescriptor(d PropertyDescriptor) Value {
	x := &object4{}
	r.chargeObject(unsafe.Sizeof(object4{}))
	o := initObject(&x.Object, ClassObject, r.descriptorShape(d.IsAccessorDescriptor()))
	if d.IsAccessorDescriptor() {
		x.buf = [4]Value{d.Get, d.Set, Bool(d.Enumerable()), Bool(d.Configurable())}
	} else {
		x.buf = [4]Value{d.Value, Bool(d.Writable()), Bool(d.Enumerable()), Bool(d.Configurable())}
	}
	o.slots = x.buf[:]
	return ObjectValue(o)
}

// objectGetOwnPropertySymbols implements Object.getOwnPropertySymbols.
func objectGetOwnPropertySymbols(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	keys, err := r.ownPropertyKeys(o)
	if err != nil {
		return Undefined(), err
	}
	var items []Value
	for _, k := range keys {
		if k.IsSymbol() {
			items = append(items, k.Value())
		}
	}
	return ObjectValue(r.NewArrayFromSlice(items)), nil
}

// objectGetOwnPropertyNames implements Object.getOwnPropertyNames.
func objectGetOwnPropertyNames(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	keys, err := r.ownPropertyKeys(o)
	if err != nil {
		return Undefined(), err
	}
	items := make([]Value, 0, len(keys))
	for i, k := range keys {
		if err := interruptEvery(r, int64(i)); err != nil {
			return Undefined(), err
		}
		if k.IsSymbol() {
			continue
		}
		items = append(items, StringValue(k.ToJSString(r)))
	}
	return ObjectValue(r.NewArrayFromSlice(items)), nil
}

// objectFromEntries implements Object.fromEntries.
func objectFromEntries(r *Realm, this Value, args []Value) (Value, error) {
	iterable := Arg(args, 0)
	if iterable.IsNullish() {
		return Undefined(), r.TypeError("Object.fromEntries requires an iterable, got %s", iterable.String())
	}
	obj := r.NewObject()
	n := int64(0)
	err := r.addEntriesFromIterable(iterable, func(k, v Value) error {
		if err := interruptEvery(r, n); err != nil {
			return err
		}
		n++
		key, err := r.ToPropertyKey(k)
		if err != nil {
			return err
		}
		return obj.CreateDataPropertyOrThrow(r, key, v)
	})
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(obj), nil
}

// objectGroupBy implements Object.groupBy(items, callback): a
// null-prototype object whose properties, in order of first appearance, are
// the property keys of the callback results.
func objectGroupBy(r *Realm, this Value, args []Value) (Value, error) {
	var (
		keys  []PropertyKey
		lists [][]Value
		index map[PropertyKey]int
	)
	err := r.groupBy(Arg(args, 0), Arg(args, 1), func(k, v Value) error {
		key, err := r.ToPropertyKey(k)
		if err != nil {
			return err
		}
		i, ok := index[key]
		if !ok {
			if index == nil {
				index = make(map[PropertyKey]int)
			}
			i = len(lists)
			index[key] = i
			keys = append(keys, key)
			lists = append(lists, nil)
		}
		lists[i] = append(lists[i], v)
		return nil
	})
	if err != nil {
		return Undefined(), err
	}
	obj := r.NewObjectWithProto(nil)
	for i, key := range keys {
		if err := obj.CreateDataPropertyOrThrow(r, key, ObjectValue(r.NewArrayFromSlice(lists[i]))); err != nil {
			return Undefined(), err
		}
	}
	return ObjectValue(obj), nil
}

// addEntriesFromIterable implements AddEntriesFromIterable: every value of
// iterable must be an object whose "0" and "1" are passed to adder; a bad
// entry or a throwing adder closes the iterator.
func (r *Realm) addEntriesFromIterable(iterable Value, adder func(k, v Value) error) error {
	ir, err := r.getIterator(iterable)
	if err != nil {
		return err
	}
	for n := int64(0); ; n++ {
		if err := interruptEvery(r, n); err != nil {
			return err
		}
		entry, done, err := ir.step(r)
		if err != nil || done {
			return err
		}
		if err := addEntry(r, entry, adder); err != nil {
			return ir.closeThrow(r, err)
		}
	}
}

func addEntry(r *Realm, entry Value, adder func(k, v Value) error) error {
	if !entry.IsObject() {
		return r.TypeError("Iterator value %s is not an entry object", r.DisplayString(entry))
	}
	e := entry.AsObject()
	k, err := e.GetIndex(r, 0)
	if err != nil {
		return err
	}
	v, err := e.GetIndex(r, 1)
	if err != nil {
		return err
	}
	return adder(k, v)
}

// objectHasOwn implements Object.hasOwn(o, key): ToObject first, then the key.
func objectHasOwn(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	key, err := r.ToPropertyKey(Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	ok, err := r.hasOwnProperty(o, key)
	return Bool(ok), err
}

// objectIs implements Object.is: SameValue.
func objectIs(r *Realm, this Value, args []Value) (Value, error) {
	return Bool(SameValue(Arg(args, 0), Arg(args, 1))), nil
}

// --- prototype methods --------------------------------------------------------------

// objectProtoHasOwnProperty implements Object.prototype.hasOwnProperty: the
// key is coerced before this. Primitive receivers are answered without
// allocating a wrapper (only strings have own properties: indices and
// length).
func objectProtoHasOwnProperty(r *Realm, this Value, args []Value) (Value, error) {
	key, err := r.ToPropertyKey(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	switch this.Type() {
	case TypeObject:
		ok, err := r.hasOwnProperty(this.AsObject(), key)
		return Bool(ok), err
	case TypeString:
		if key.IsIndex() {
			return Bool(int64(key.Index()) < int64(this.AsString().Len())), nil
		}
		return Bool(key == lengthKey), nil
	case TypeUndefined, TypeNull:
		return Undefined(), r.TypeError("Cannot convert undefined or null to object")
	}
	return False(), nil
}

// objectProtoIsPrototypeOf implements Object.prototype.isPrototypeOf.
func objectProtoIsPrototypeOf(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	if !v.IsObject() {
		return False(), nil
	}
	o, err := r.ToObject(this)
	if err != nil {
		return Undefined(), err
	}
	p := v.AsObject()
	for i := int64(0); ; i++ {
		if p, err = r.getPrototypeOf(p); err != nil || p == nil {
			return False(), err
		}
		if p == o {
			return True(), nil
		}
		if err := interruptEvery(r, i); err != nil {
			return Undefined(), err
		}
	}
}

// objectProtoPropertyIsEnumerable implements
// Object.prototype.propertyIsEnumerable (key coerced before this).
func objectProtoPropertyIsEnumerable(r *Realm, this Value, args []Value) (Value, error) {
	key, err := r.ToPropertyKey(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	switch this.Type() {
	case TypeObject:
		ok, err := r.isOwnEnumerable(this.AsObject(), key)
		return Bool(ok), err
	case TypeString:
		return Bool(key.IsIndex() && int64(key.Index()) < int64(this.AsString().Len())), nil
	case TypeUndefined, TypeNull:
		return Undefined(), r.TypeError("Cannot convert undefined or null to object")
	}
	return False(), nil
}

// objectProtoToLocaleString implements Object.prototype.toLocaleString:
// Invoke(this, "toString").
func objectProtoToLocaleString(r *Realm, this Value, args []Value) (Value, error) {
	fn, err := r.GetV(this, StringKey(AtomToString))
	if err != nil {
		return Undefined(), err
	}
	return r.Call(fn, this, nil)
}

// objectProtoToString implements Object.prototype.toString: the builtin tag,
// overridden by a string-valued @@toStringTag.
func objectProtoToString(r *Realm, this Value, args []Value) (Value, error) {
	switch this.Type() {
	case TypeUndefined:
		return StringValue(asciiString("[object Undefined]")), nil
	case TypeNull:
		return StringValue(asciiString("[object Null]")), nil
	}
	o, err := r.ToObject(this)
	if err != nil {
		return Undefined(), err
	}
	tag := "Object"
	switch o.class {
	case ClassArray, ClassFunction, ClassError, ClassBoolean, ClassNumber, ClassString, ClassDate, ClassRegExp, ClassArguments:
		tag = o.class.String()
	case ClassProxy:
		if isArr, err := r.isArray(ObjectValue(o)); err != nil {
			return Undefined(), err
		} else if isArr {
			tag = "Array"
		} else if o.IsCallable() {
			tag = "Function"
		}
	}
	if key := SymbolKey(SymToStringTag); !r.lacksWellKnown(o, key) {
		t, err := o.Get(r, key, ObjectValue(o))
		if err != nil {
			return Undefined(), err
		}
		if t.IsString() {
			var sb StringBuilder
			sb.WriteGoString("[object ")
			sb.WriteString(t.AsString())
			sb.WriteGoString("]")
			return StringValue(sb.String()), nil
		}
	}
	return StringValue(asciiString("[object " + tag + "]")), nil
}

// objectProtoValueOf implements Object.prototype.valueOf.
func objectProtoValueOf(r *Realm, this Value, args []Value) (Value, error) {
	o, err := r.ToObject(this)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

package engine

import "github.com/MoeForge/moejs/bytecode"

// Private names (ECMA-262 §6.2.12) are a property-key kind of their own. A
// PropertyKey holding a *PrivateName never equals a string, symbol or index
// key, and private elements live in the object's ordinary named storage
// (shape or dictionary), so the instances of one class share one shape
// chain for their public and private fields alike. Every key enumeration
// selects string or symbol keys explicitly (OwnPropertyKeys, for-in, JSON,
// Object.assign, host export), so private elements are never observable as
// properties. They carry no attributes JavaScript can see: fields are stored
// writable and non-enumerable and the operations below ignore writability
// and extensibility, so Object.freeze and Object.preventExtensions do not
// affect them.
//
// Private methods and accessors are not stored per instance: the class
// brand (a PrivateName of its own) is, and the method lives in its
// PrivateName. A static private method is checked against the class
// constructor instead of a brand.

// privateKind classifies a private name.
type privateKind uint8

const (
	privateField privateKind = iota // also the class brand
	privateMethod
	privateAccessor
)

// PrivateName is one private name of one class evaluation (a new set is
// created every time a class definition is evaluated).
type PrivateName struct {
	desc  *String // "#x"; for a brand, the class name
	kind  privateKind
	brand *PrivateName // instance methods and accessors: the class brand
	home  *Object      // static methods and accessors: the class constructor
	get   *Object      // the method, or the getter (nil: none)
	set   *Object      // the setter (nil: none)
	// shape and slot cache where a field was last found: shapes are
	// immutable, so a receiver with this shape holds the field at slot.
	shape *Shape
	slot  uint32
}

// newPrivateName creates a field name (NewPrivateName); defining a method
// or accessor under it changes its kind.
func newPrivateName(desc *String) *PrivateName { return &PrivateName{desc: desc} }

// PrivateKey returns the property key of a private name.
func PrivateKey(pn *PrivateName) PropertyKey { return PropertyKey{privateValue(pn)} }

// IsPrivate reports whether the key is a private name.
func (k PropertyKey) IsPrivate() bool { return k.v.isPrivate() }

// definePrivateMethod records fn as the method, getter or setter of pn and
// the brand (owner holds a brand) or, for static elements, the class
// constructor (owner holds the constructor) that receivers must carry;
// flags are the bytecode.Private* flags of SetPrivateMethod.
func definePrivateMethod(pn *PrivateName, fn *Object, owner Value, flags uint32) {
	switch {
	case flags&bytecode.PrivateGetter != 0:
		pn.kind = privateAccessor
		pn.get = fn
	case flags&bytecode.PrivateSetter != 0:
		pn.kind = privateAccessor
		pn.set = fn
	default:
		pn.kind = privateMethod
		pn.get = fn
	}
	if flags&bytecode.PrivateStatic != 0 {
		pn.home = owner.AsObject()
	} else {
		pn.brand = owner.asPrivate()
	}
}

// privateSlot returns the storage of o's private field pn, or nil.
func (o *Object) privateSlot(pn *PrivateName) *Value {
	if o.flags&flagDict == 0 {
		if o.shape == pn.shape {
			return &o.slots[pn.slot]
		}
		slot, _, ok := o.shape.Lookup(PrivateKey(pn))
		if !ok {
			return nil
		}
		pn.shape, pn.slot = o.shape, slot
		return &o.slots[slot]
	}
	if o.dict == nil {
		// A host placeholder (hostlazy.go), which has no private elements:
		// addPrivate materializes it. One that became a prototype or a
		// WeakMap key has a dictProps (its root, its weak entries) without
		// properties, and the lookup below finds nothing either.
		return nil
	}
	c, ok := o.dict.lookup(PrivateKey(pn))
	if !ok {
		return nil
	}
	return &c.value
}

// hasPrivate reports whether v carries pn: the field itself, the brand of an
// instance method or accessor, or for a static one, whether v is the class.
func hasPrivate(v Value, pn *PrivateName) bool {
	if !v.IsObject() {
		return false
	}
	o := v.AsObject()
	switch {
	case pn.kind == privateField:
		return o.privateSlot(pn) != nil
	case pn.home != nil:
		return o == pn.home
	}
	return o.privateSlot(pn.brand) != nil
}

// addPrivate adds a private element known to be absent. Shared intrinsics
// refuse it (a deviation: the spec allows private fields on any object),
// and an object whose shape is shared across realms moves to dictionary
// mode first, so a realm-local name never enters a process-wide transition
// cache.
func (r *Realm) addPrivate(o *Object, pn *PrivateName, v Value, attrs uint8) error {
	if o.flags&flagShared != 0 {
		return r.TypeError("Cannot define private member %s on shared intrinsic %s", pn.desc.GoString(), o.debugString())
	}
	o.materializeHost() // a host placeholder (hostlazy.go) has no storage yet
	if o.flags&flagDict == 0 && o.shape.shared {
		o.toDictionary(r)
	}
	o.addNamed(r, PrivateKey(pn), propCell{value: v, attrs: attrs})
	return nil
}

// privateGet implements PrivateGet (o.#x).
func (r *Realm) privateGet(v Value, pn *PrivateName) (Value, error) {
	if pn.kind == privateField {
		if v.IsObject() {
			if p := v.AsObject().privateSlot(pn); p != nil {
				return *p, nil
			}
		}
		return Undefined(), r.privateReadError(pn)
	}
	if !hasPrivate(v, pn) {
		return Undefined(), r.privateReadError(pn)
	}
	if pn.kind == privateMethod {
		return ObjectValue(pn.get), nil
	}
	if pn.get == nil {
		return Undefined(), r.TypeError("'%s' was defined without a getter", pn.desc.GoString())
	}
	return r.CallObject(pn.get, v, nil)
}

// privateSet implements PrivateSet (o.#x = val).
func (r *Realm) privateSet(v Value, pn *PrivateName, val Value) error {
	if pn.kind == privateField {
		if v.IsObject() {
			if p := v.AsObject().privateSlot(pn); p != nil {
				*p = val
				return nil
			}
		}
		return r.privateWriteError(pn)
	}
	if !hasPrivate(v, pn) {
		return r.privateWriteError(pn)
	}
	if pn.kind == privateMethod {
		return r.TypeError("Private method '%s' is not writable", pn.desc.GoString())
	}
	if pn.set == nil {
		return r.TypeError("'%s' was defined without a setter", pn.desc.GoString())
	}
	_, err := r.CallObject(pn.set, v, []Value{val})
	return err
}

// privateDefine implements PrivateFieldAdd (a field initializer).
func (r *Realm) privateDefine(v Value, pn *PrivateName, val Value) error {
	o := v.AsObject()
	if o.privateSlot(pn) != nil {
		return r.TypeError("Cannot initialize %s twice on the same object", pn.desc.GoString())
	}
	return r.addPrivate(o, pn, val, attrWritable)
}

// privateAddBrand adds the class brand to a new instance (the
// PrivateMethodOrAccessorAdd steps for every instance private method).
func (r *Realm) privateAddBrand(v Value, brand *PrivateName) error {
	o := v.AsObject()
	if o.privateSlot(brand) != nil {
		return r.TypeError("Cannot initialize private methods of class %s twice on the same object", brand.desc.GoString())
	}
	return r.addPrivate(o, brand, Undefined(), 0)
}

// privateIn implements `#x in v`.
func (r *Realm) privateIn(v Value, pn *PrivateName) (bool, error) {
	if !v.IsObject() {
		return false, r.TypeError("Cannot use 'in' operator to search for '%s' in %s", pn.desc.GoString(), v.String())
	}
	return hasPrivate(v, pn), nil
}

func (r *Realm) privateReadError(pn *PrivateName) error {
	return r.TypeError("Cannot read private member %s from an object whose class did not declare it", pn.desc.GoString())
}

func (r *Realm) privateWriteError(pn *PrivateName) error {
	return r.TypeError("Cannot write private member %s to an object whose class did not declare it", pn.desc.GoString())
}

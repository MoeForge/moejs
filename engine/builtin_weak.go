package engine

import (
	"unsafe"
	"weak"
)

// WeakMap, WeakSet and WeakRef (ES2025 §24.3, §24.4, §26.1).
//
// A weak collection must not keep its keys alive, and an entry must not keep
// its key alive through its value either: a value that references its own
// key is collected with it (an ephemeron). The entries therefore live on the
// key — an object carries them in its dictProps (dict.go), a symbol in its
// Symbol struct — each naming its collection by a weak pointer. The key
// reaches the value and the collection reaches neither, so the Go collector
// reclaims key and value together once the key is unreachable.
//
// Keys no realm may write — the frozen objects of shared realms and the
// process-wide well-known symbols — keep their entries in a strong map on
// the collection instead; they outlive every realm anyway.
//
// A lookup scans the key's entries, one per weak collection holding it
// (usually one or two). The entries of a collection that died stay on its
// live keys until the key is next inserted into a weak collection, which
// prunes them.

// weakSide is the list of weak-collection entries carried by one key.
type weakSide struct {
	entries []weakEntry
}

type weakEntry struct {
	owner weak.Pointer[weakColl]
	value Value
}

// prune drops the entries of collections that died.
func (s *weakSide) prune() {
	live := s.entries[:0]
	for _, e := range s.entries {
		if e.owner.Value() != nil {
			live = append(live, e)
		}
	}
	clear(s.entries[len(live):])
	s.entries = live
}

// canBeHeldWeakly implements CanBeHeldWeakly: objects and the symbols that
// are not in the global registry.
func canBeHeldWeakly(v Value) bool {
	return v.IsObject() || v.IsSymbol() && !v.AsSymbol().registered
}

// sideOf returns the entry list carried by key (nil when it has none and
// create is false), or carried false when key cannot carry entries and the
// collection's strong map holds them. key can be held weakly.
func sideOf(key Value, create bool) (side *weakSide, carried bool) {
	var slot **weakSide
	if key.IsSymbol() {
		s := key.AsSymbol()
		if s.wellKnown {
			return nil, false
		}
		slot = &s.weak
	} else {
		o := key.AsObject()
		if o.IsShared() {
			return nil, false
		}
		if o.dictTable() == nil && !create {
			return nil, true
		}
		slot = &o.ensureDict().weak
	}
	if *slot == nil && create {
		*slot = &weakSide{}
	}
	return *slot, true
}

// weakColl is the [[WeakMapData]]/[[WeakSetData]] of a WeakMap or WeakSet
// (a WeakSet stores Undefined values).
type weakColl struct {
	self   weak.Pointer[weakColl]
	strong map[Value]Value // keys that cannot carry entries (see above)
}

// find returns the index of c's entry in side, or -1.
func (c *weakColl) find(side *weakSide) int {
	if side != nil {
		for i := range side.entries {
			if side.entries[i].owner == c.self {
				return i
			}
		}
	}
	return -1
}

// get returns the value stored under key, which can be held weakly.
func (c *weakColl) get(key Value) (Value, bool) {
	side, carried := sideOf(key, false)
	if !carried {
		if v, ok := c.strong[key]; ok {
			return v, true
		}
		return Undefined(), false
	}
	if i := c.find(side); i >= 0 {
		return side.entries[i].value, true
	}
	return Undefined(), false
}

// set stores value under key, which can be held weakly.
func (c *weakColl) set(key, value Value) {
	side, carried := sideOf(key, true)
	if !carried {
		if c.strong == nil {
			c.strong = make(map[Value]Value)
		}
		c.strong[key] = value
		return
	}
	if i := c.find(side); i >= 0 {
		side.entries[i].value = value
		return
	}
	side.prune()
	side.entries = append(side.entries, weakEntry{owner: c.self, value: value})
}

// delete removes key, which can be held weakly, and reports whether it was
// present.
func (c *weakColl) delete(key Value) bool {
	side, carried := sideOf(key, false)
	if !carried {
		_, ok := c.strong[key]
		delete(c.strong, key)
		return ok
	}
	i := c.find(side)
	if i < 0 {
		return false
	}
	last := len(side.entries) - 1
	side.entries[i] = side.entries[last]
	side.entries[last] = weakEntry{}
	side.entries = side.entries[:last]
	return true
}

// weakCollObject co-allocates a WeakMap or WeakSet with its data.
type weakCollObject struct {
	obj  Object
	data weakColl
}

func (r *Realm) newWeakCollection(proto *Object, class Class) (*Object, *weakColl) {
	r.markPrototype(proto)
	co := &weakCollObject{}
	r.chargeObject(unsafe.Sizeof(weakCollObject{}))
	o := &co.obj
	o.shape = r.rootShapeFor(proto)
	o.class = class
	o.flags = flagExtensible
	co.data.self = weak.Make(&co.data)
	setData(o, kindWeakColl, &co.data)
	return o, &co.data
}

// thisWeakColl implements RequireInternalSlot(this, [[WeakMapData]] or
// [[WeakSetData]]).
func thisWeakColl(r *Realm, this Value, class Class, method string) (*weakColl, error) {
	if this.IsObject() {
		if o := this.AsObject(); o.class == class {
			if c, ok := dataOk(o, kindWeakColl); ok {
				return c, nil
			}
		}
	}
	return nil, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(this))
}

// --- WeakMap ------------------------------------------------------------------

var weakMapProtoMethods = []builtinDef{
	{AtomDelete, weakMapProtoDelete, 1},
	{AtomGet, weakMapProtoGet, 1},
	{atomCollHas, weakMapProtoHas, 1},
}

func installWeakMap(r *Realm) {
	r.WeakMapPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, r.deferredCap(len(weakMapProtoMethods)+3, 1))
	r.WeakMapCtor = r.newConstructor(AtomWeakMap, 0, weakMapCall, weakMapConstruct, r.WeakMapPrototype)
	r.installDeferred(r.WeakMapPrototype, installWeakMapPrototype)
	r.bindGlobal(AtomWeakMap, ObjectValue(r.WeakMapCtor))
}

func installWeakMapPrototype(r *Realm, p *Object) {
	p.ReserveSlots(r, len(weakMapProtoMethods)+2)
	r.installBuiltins(p, weakMapProtoMethods)
	r.weakMapSetFn = r.NewNativeFunction(AtomSet, 2, weakMapProtoSet)
	r.installOrReplace(p, StringKey(AtomSet), propCell{value: ObjectValue(r.weakMapSetFn), attrs: attrHidden})
	r.installToStringTag(p, AtomWeakMap)
}

// weakMapConstruct implements new WeakMap(iterable).
func weakMapConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	proto, err := r.GetPrototypeFromConstructor(newTarget, r.WeakMapCtor, r.WeakMapPrototype)
	if err != nil {
		return Undefined(), err
	}
	o, c := r.newWeakCollection(proto, ClassWeakMap)
	return ObjectValue(o), r.fillFromEntries(o, Arg(args, 0), AtomSet, &r.weakMapSetFn, func(k, v Value) error {
		if !canBeHeldWeakly(k) {
			return r.invalidWeakKey(k)
		}
		c.set(k, v)
		return nil
	})
}

func (r *Realm) invalidWeakKey(k Value) error {
	return r.TypeError("Invalid value used as weak map key: %s", r.DisplayString(k))
}

func weakMapProtoDelete(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisWeakColl(r, this, ClassWeakMap, "WeakMap.prototype.delete")
	if err != nil {
		return Undefined(), err
	}
	k := Arg(args, 0)
	return Bool(canBeHeldWeakly(k) && c.delete(k)), nil
}

func weakMapProtoGet(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisWeakColl(r, this, ClassWeakMap, "WeakMap.prototype.get")
	if err != nil {
		return Undefined(), err
	}
	k := Arg(args, 0)
	if !canBeHeldWeakly(k) {
		return Undefined(), nil
	}
	v, _ := c.get(k)
	return v, nil
}

func weakMapProtoHas(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisWeakColl(r, this, ClassWeakMap, "WeakMap.prototype.has")
	if err != nil {
		return Undefined(), err
	}
	k := Arg(args, 0)
	if !canBeHeldWeakly(k) {
		return False(), nil
	}
	_, ok := c.get(k)
	return Bool(ok), nil
}

func weakMapProtoSet(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisWeakColl(r, this, ClassWeakMap, "WeakMap.prototype.set")
	if err != nil {
		return Undefined(), err
	}
	k := Arg(args, 0)
	if !canBeHeldWeakly(k) {
		return Undefined(), r.invalidWeakKey(k)
	}
	c.set(k, Arg(args, 1))
	return this, nil
}

// --- WeakSet ------------------------------------------------------------------

var weakSetProtoMethods = []builtinDef{
	{AtomDelete, weakSetProtoDelete, 1},
	{atomCollHas, weakSetProtoHas, 1},
}

func installWeakSet(r *Realm) {
	r.WeakSetPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, r.deferredCap(len(weakSetProtoMethods)+3, 1))
	r.WeakSetCtor = r.newConstructor(AtomWeakSet, 0, weakSetCall, weakSetConstruct, r.WeakSetPrototype)
	r.installDeferred(r.WeakSetPrototype, installWeakSetPrototype)
	r.bindGlobal(AtomWeakSet, ObjectValue(r.WeakSetCtor))
}

func installWeakSetPrototype(r *Realm, p *Object) {
	p.ReserveSlots(r, len(weakSetProtoMethods)+2)
	r.weakSetAddFn = r.NewNativeFunction(AtomAdd, 1, weakSetProtoAdd)
	r.installOrReplace(p, StringKey(AtomAdd), propCell{value: ObjectValue(r.weakSetAddFn), attrs: attrHidden})
	r.installBuiltins(p, weakSetProtoMethods)
	r.installToStringTag(p, AtomWeakSet)
}

// weakSetConstruct implements new WeakSet(iterable).
func weakSetConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	proto, err := r.GetPrototypeFromConstructor(newTarget, r.WeakSetCtor, r.WeakSetPrototype)
	if err != nil {
		return Undefined(), err
	}
	o, c := r.newWeakCollection(proto, ClassWeakSet)
	return ObjectValue(o), r.fillFromValues(o, Arg(args, 0), AtomAdd, &r.weakSetAddFn, func(v Value) error {
		if !canBeHeldWeakly(v) {
			return r.invalidWeakValue(v)
		}
		c.set(v, Undefined())
		return nil
	})
}

func (r *Realm) invalidWeakValue(v Value) error {
	return r.TypeError("Invalid value used in weak set: %s", r.DisplayString(v))
}

func weakSetProtoAdd(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisWeakColl(r, this, ClassWeakSet, "WeakSet.prototype.add")
	if err != nil {
		return Undefined(), err
	}
	v := Arg(args, 0)
	if !canBeHeldWeakly(v) {
		return Undefined(), r.invalidWeakValue(v)
	}
	c.set(v, Undefined())
	return this, nil
}

func weakSetProtoDelete(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisWeakColl(r, this, ClassWeakSet, "WeakSet.prototype.delete")
	if err != nil {
		return Undefined(), err
	}
	v := Arg(args, 0)
	return Bool(canBeHeldWeakly(v) && c.delete(v)), nil
}

func weakSetProtoHas(r *Realm, this Value, args []Value) (Value, error) {
	c, err := thisWeakColl(r, this, ClassWeakSet, "WeakSet.prototype.has")
	if err != nil {
		return Undefined(), err
	}
	v := Arg(args, 0)
	if !canBeHeldWeakly(v) {
		return False(), nil
	}
	_, ok := c.get(v)
	return Bool(ok), nil
}

// --- WeakRef ------------------------------------------------------------------

// weakRefData is the [[WeakRefTarget]] of a WeakRef: a weak pointer to the
// object or symbol, or the target itself when it cannot die (see above).
type weakRefData struct {
	obj    weak.Pointer[Object]
	sym    weak.Pointer[Symbol]
	strong Value // Undefined when the target is held weakly
	// kept is the kept-list generation (jobState.keptGen, +1) in which the
	// target was last added to the realm's kept list, so repeated derefs within one job add
	// it once.
	kept uint64
}

// target returns the target, or Undefined once it was collected.
func (d *weakRefData) target() Value {
	if !d.strong.IsUndefined() {
		return d.strong
	}
	if o := d.obj.Value(); o != nil {
		return ObjectValue(o)
	}
	if s := d.sym.Value(); s != nil {
		return SymbolValue(s)
	}
	return Undefined()
}

// keepDuringJob implements AddToKeptObjects(d's target v). The
// [[KeptAlive]] list lives in the job state (jobs.go) and is cleared when
// the current job ends.
func (r *Realm) keepDuringJob(d *weakRefData, v Value) {
	if !d.strong.IsUndefined() {
		return
	}
	j := r.jobState()
	if d.kept == j.keptGen+1 {
		return
	}
	d.kept = j.keptGen + 1
	j.kept = append(j.kept, v)
	r.jobsPending = true
}

var weakRefProtoMethods = []builtinDef{{AtomDeref, weakRefProtoDeref, 0}}

func installWeakRef(r *Realm) {
	r.WeakRefPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, r.deferredCap(3, 1))
	r.WeakRefCtor = r.newConstructor(AtomWeakRef, 1, weakRefCall, weakRefConstruct, r.WeakRefPrototype)
	r.installDeferred(r.WeakRefPrototype, installWeakRefPrototype)
	r.bindGlobal(AtomWeakRef, ObjectValue(r.WeakRefCtor))
}

func installWeakRefPrototype(r *Realm, p *Object) {
	p.ReserveSlots(r, 2)
	r.installBuiltins(p, weakRefProtoMethods)
	r.installToStringTag(p, AtomWeakRef)
}

// weakRefObject co-allocates a WeakRef with its data.
type weakRefObject struct {
	obj  Object
	data weakRefData
}

// weakRefConstruct implements new WeakRef(target).
func weakRefConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	t := Arg(args, 0)
	if !canBeHeldWeakly(t) {
		return Undefined(), r.TypeError("WeakRef: invalid target %s", r.DisplayString(t))
	}
	proto, err := r.GetPrototypeFromConstructor(newTarget, r.WeakRefCtor, r.WeakRefPrototype)
	if err != nil {
		return Undefined(), err
	}
	r.markPrototype(proto)
	wo := &weakRefObject{data: weakRefData{strong: Undefined()}}
	r.chargeObject(unsafe.Sizeof(weakRefObject{}))
	o := &wo.obj
	o.shape = r.rootShapeFor(proto)
	o.class = ClassWeakRef
	o.flags = flagExtensible
	setData(o, kindWeakRef, &wo.data)
	d := &wo.data
	switch {
	case t.IsSymbol() && t.AsSymbol().wellKnown, t.IsObject() && t.AsObject().IsShared():
		d.strong = t
	case t.IsSymbol():
		d.sym = weak.Make(t.AsSymbol())
	default:
		d.obj = weak.Make(t.AsObject())
	}
	r.keepDuringJob(d, t)
	return ObjectValue(o), nil
}

// weakRefProtoDeref implements WeakRef.prototype.deref.
func weakRefProtoDeref(r *Realm, this Value, args []Value) (Value, error) {
	if this.IsObject() && this.AsObject().class == ClassWeakRef {
		if d, ok := dataOk(this.AsObject(), kindWeakRef); ok {
			v := d.target()
			if !v.IsUndefined() {
				r.keepDuringJob(d, v)
			}
			return v, nil
		}
	}
	return Undefined(), r.TypeError("Method WeakRef.prototype.deref called on incompatible receiver %s", r.DisplayString(this))
}

package engine

import (
	"slices"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// Slow paths of the interpreter loop: inline-cache misses and fills, generic
// operators, calls, iteration and object-literal helpers. Everything here
// may re-enter the interpreter, so the run loop re-slices its register
// window after calling any of these.

// tdzError builds the ReferenceError for reading a binding in its TDZ; x is
// the name constant index.
func (r *Realm) tdzError(meta *funcMeta, x uint32) error {
	name := "binding"
	if s := meta.constName(x); s != nil {
		name = s.GoString()
	}
	if name == "%this" { // a derived constructor's `this` before super()
		return r.ReferenceError(errSuperNotCalled)
	}
	return r.ReferenceError("Cannot access '%s' before initialization", name)
}

// --- inline caches ----------------------------------------------------------------

// fillGetIC records where key was found starting at o, if the location is
// cacheable: shape-mode objects along the chain, a slot that always holds
// the property (no attrLazy), depth <= 255, no pending lazy definition of
// key (or deferred install) on the holder and none on another object of
// o's shape (noFill, refuseFill). An accessor gets an accessor entry (odd
// Epoch).
//
// When no object on the chain has key, it records that (icAbsent), under
// the same conditions on the whole chain and three more. The key is not
// `length`, which an array or a String object answers outside its shape,
// and either can come to have the shape of an ordinary object
// (setPrototypeOf). Neither o nor a prototype is a proxy (a module
// namespace is one too): a proxy has a shape of its own, but it answers
// every key with its trap and ends a chain, as its prototype is nil. And o
// defines nothing lazily: such an object keeps a shape no other object
// has, and getNamedSlow checks it on every hit as well. A typed array, a
// String object and a mapped arguments object answer only indices and
// numeric strings outside their shapes, which no entry caches; the global
// object's pending globals are lazy definitions, as are a host object's
// properties. The entry stays valid while the realm's protoEpoch does:
// an add, a deletion, a change of attributes or of the prototype, and a
// lazy definition on any prototype advance it (bumpEpoch), and on o they
// change its shape. An absent entry does not replace one that is valid for
// another shape: a site that reads a key some of its receivers have and
// others lack keeps the entry of the ones that have it, as before absent
// entries, and does not refill at each change of shape.
func (r *Realm) fillGetIC(e *ICEntry, o *Object, key PropertyKey) {
	// typedArrayKey covers the indices and the numeric strings: a typed
	// array answers those without its shape, and shares its root shapes with
	// ordinary objects of the same prototype.
	if o.flags&flagDict != 0 || typedArrayKey(key) {
		return
	}
	if o.shape.noFill != 0 && r.refuseFill(o.shape, key) {
		return
	}
	depth := 0
	for holder := o; holder != nil; holder = holder.Proto() {
		if holder.flags&flagDict != 0 || holder.class == ClassProxy || depth > 255 {
			return
		}
		if key == lengthKey && (holder.class == ClassArray || holder.class == ClassString) {
			return
		}
		if holder.flags&flagHasLazy != 0 && holder.lazyPending(key) {
			return
		}
		slot, attrs, ok := holder.shape.Lookup(key)
		if ok {
			if attrs&attrLazy != 0 {
				return // a slot that may hold the lazy marker
			}
			epoch := r.protoEpoch
			if attrs&attrAccessor != 0 {
				epoch |= 1
			}
			*e = newICEntry(o.shape, epoch, slot, uint8(depth))
			return
		}
		depth++
	}
	if key != lengthKey && o.flags&flagHasLazy == 0 && (e.Shape == nil || e.Shape == o.shape || e.Epoch|1 != r.protoEpoch|1) {
		*e = ICEntry{Shape: o.shape, Epoch: r.protoEpoch | 1, loc: icAbsent}
	}
}

// icAbsent is the location of an entry recording that no object on the
// receiver's chain has the key, so that [[Get]] returns undefined. Its
// epoch is odd, as an accessor entry's: it fails the data check on the
// interpreter's hit path, and the slow paths answer it (oddIC). Slot and
// depth are both out of range, so no path can take it for a set entry:
// setNamedAdd wants an even epoch, accessorIC passes over it.
const icAbsent = 0xFFFFFFFF

// accessorIC returns the accessor e caches for receivers shaped like o, or
// nil when e is not a valid accessor entry. The accessor is read from its
// slot on every hit: redefining a getter or setter with the same attributes
// replaces the slot value without changing any shape.
func (r *Realm) accessorIC(e *ICEntry, o *Object) *Accessor {
	if o.shape == e.Shape && r.protoEpoch|1 == e.Epoch && e.loc != icAbsent {
		return e.Holder(o).slots[e.Slot()].asAccessor()
	}
	return nil
}

// oddIC answers a get through e, an entry with an odd epoch that is valid
// for o: an accessor entry calls the getter its slot holds, an absent
// entry gives undefined unless o defines properties lazily. ok is false
// when the caller must take the generic path, as for a slot that holds no
// accessor (accessorIC).
func (r *Realm) oddIC(e *ICEntry, o *Object, receiver Value) (v Value, ok bool, err error) {
	if e.loc != icAbsent {
		if a := e.Holder(o).slots[e.Slot()].asAccessor(); a != nil {
			v, err = r.callGetter(a, receiver)
			return v, true, err
		}
		return Undefined(), false, nil
	}
	return Undefined(), o.flags&flagHasLazy == 0, nil
}

// callGetter calls the getter of a with receiver as this.
func (r *Realm) callGetter(a *Accessor, receiver Value) (Value, error) {
	if a.Get == nil {
		return Undefined(), nil
	}
	return r.CallObject(a.Get, receiver, nil)
}

// callSetter calls the setter of a with receiver as this, rejecting the
// assignment in the strict manner when there is none. The argument is
// passed in the free register stack above the current frame (reserved for
// the call, like CallSpread's list) so the call allocates nothing.
func (r *Realm) callSetter(a *Accessor, receiver Value, key PropertyKey, v Value) error {
	if a.Set == nil {
		return r.TypeError("Cannot assign to read only property '%s' of object", key.GoString())
	}
	st := &r.interp
	sp := st.sp
	if sp+1 > len(st.stack) {
		r.growStack(sp + 1)
	}
	st.stack[sp] = v
	st.sp = sp + 1
	_, err := r.CallObject(a.Set, receiver, st.stack[sp:sp+1:sp+1])
	st.sp = sp
	return err
}

// getNamedSlow is [[Get]] of an interned key on an object with IC fill.
func (r *Realm) getNamedSlow(o *Object, receiver Value, key PropertyKey, e *ICEntry) (Value, error) {
	if o.shape == e.Shape && r.protoEpoch|1 == e.Epoch {
		if v, ok, err := r.oddIC(e, o, receiver); ok {
			return v, err
		}
	}
	if v, ok, err := r.getFillIC(e, o, receiver, key); ok {
		return v, err
	}
	v, err := o.Get(r, key, receiver)
	if err != nil {
		return Undefined(), err
	}
	r.fillGetIC(e, o, key)
	return v, nil
}

// getFillIC is getNamedSlow's [[Get]] on a chain fillGetIC fills an entry
// for: it finds the property once, fills e as fillGetIC does and reads the
// property as a hit on e would, where [[Get]] and then fillGetIC walked the
// chain twice, on every miss of a site that sees several shapes. ok is
// false where fillGetIC might fill no entry, and the caller takes the
// generic path.
func (r *Realm) getFillIC(e *ICEntry, o *Object, receiver Value, key PropertyKey) (v Value, ok bool, err error) {
	if o.flags&flagDict != 0 || o.shape.noFill != 0 || typedArrayKey(key) {
		return Undefined(), false, nil
	}
	depth := 0
	for holder := o; holder != nil; holder = holder.Proto() {
		if holder.flags&flagDict != 0 || holder.class == ClassProxy || depth > 255 ||
			key == lengthKey && (holder.class == ClassArray || holder.class == ClassString) {
			return Undefined(), false, nil
		}
		if holder.flags&flagHasLazy != 0 && holder.lazyPending(key) {
			// A property defined lazily is defined first, and the read goes
			// on at holder, as [[Get]] does. The definition may have changed
			// the chain (a prototype below holder, a property it added on
			// the way), so the entry is filled from the chain as it is now.
			if !holder.resolveLazy(key) {
				return Undefined(), false, nil
			}
			if v, err = holder.Get(r, key, receiver); err == nil {
				r.fillGetIC(e, o, key)
			}
			return v, true, err
		}
		slot, attrs, found := holder.shape.Lookup(key)
		if !found {
			depth++
			continue
		}
		if attrs&attrLazy != 0 {
			return Undefined(), false, nil
		}
		if attrs&attrAccessor == 0 {
			*e = newICEntry(o.shape, r.protoEpoch, slot, uint8(depth))
			return holder.slots[slot], true, nil
		}
		a := holder.slots[slot].asAccessor()
		if a == nil {
			return Undefined(), false, nil
		}
		*e = newICEntry(o.shape, r.protoEpoch|1, slot, uint8(depth))
		v, err = r.callGetter(a, receiver)
		return v, true, err
	}
	if key != lengthKey && o.flags&flagHasLazy == 0 && (e.Shape == nil || e.Shape == o.shape || e.Epoch|1 != r.protoEpoch|1) {
		*e = ICEntry{Shape: o.shape, Epoch: r.protoEpoch | 1, loc: icAbsent}
	}
	return Undefined(), true, nil
}

// getStringProp reads a named property of a string primitive and caches the
// String.prototype lookup. The entry is sound for object receivers too: a
// receiver whose shape equals String.prototype's finds the property at the
// same slot and depth.
func (r *Realm) getStringProp(v Value, key PropertyKey, e *ICEntry) (Value, error) {
	if sp := r.StringPrototype; r.polyMiss(e, sp.shape) {
		p := r.polyTable().find(r.icSite(e))
		if p != nil {
			if p.mega {
				return r.getStringMono(v, key, e)
			}
			for i := range p.w {
				if w := &p.w[i]; w.Shape == sp.shape {
					*e, *w = *w, *e
					p.hits++
					if e.Epoch == r.protoEpoch {
						return e.Holder(sp).slots[e.Slot()], nil
					}
					return r.getStringMono(v, key, e)
				}
			}
		}
		return r.getStringPoly(v, key, e, p)
	}
	return r.getStringMono(v, key, e)
}

// getStringMono is getStringProp on the entry e for String.prototype's
// shape.
func (r *Realm) getStringMono(v Value, key PropertyKey, e *ICEntry) (Value, error) {
	sp := r.StringPrototype
	if sp.shape == e.Shape && r.protoEpoch|1 == e.Epoch {
		if res, ok, err := r.oddIC(e, sp, v); ok {
			return res, err
		}
	}
	res, err := r.GetV(v, key)
	if err != nil {
		return Undefined(), err
	}
	r.fillGetIC(e, sp, key)
	return res, nil
}

// getPrimitiveProp reads a named property of a primitive receiver.
func (r *Realm) getPrimitiveProp(v Value, key PropertyKey) (Value, error) {
	if v.IsNullish() {
		return Undefined(), r.TypeError("Cannot read properties of %s (reading '%s')", v.String(), key.GoString())
	}
	return r.GetV(v, key)
}

// setNamedSlow is strict [[Set]] of an interned key with IC fill for own
// writable data properties, for setters, own or inherited, and for adds.
func (r *Realm) setNamedSlow(o *Object, key PropertyKey, v Value, e *ICEntry) error {
	if a := r.accessorIC(e, o); a != nil {
		return r.callSetter(a, ObjectValue(o), key, v)
	}
	if r.setNamedAdd(o, key, v, e) {
		return nil
	}
	if err := o.SetProp(r, key, v); err != nil {
		return err
	}
	r.fillSetIC(e, o, key)
	return nil
}

// icAdd is the holder depth of a set entry filled by setNamedAdd. Its shape
// is the one after the add, so objects that already have the property hit
// the data path of the run loop like any set entry (which ignores the
// depth); objects with the shape before the add take the transition in
// setNamedAdd. The other set entries have depth 0 or are accessor entries
// (odd epoch), which setNamedAdd never takes.
const icAdd = 0xFF

// setNamedAdd performs OrdinarySet of key on o when it adds key as a data
// property of o, an ordinary extensible shape-mode object that is no
// prototype (addsTo): o has no property key, and the prototype chain
// leaves the write to o (protoChainAdds). It reports whether it did. The
// add from o's shape that e records skips the checks (addIC): the epoch
// proves that no prototype changed since the add e records, and the shape
// that o has the same prototype and lacks key.
func (r *Realm) setNamedAdd(o *Object, key PropertyKey, v Value, e *ICEntry) bool {
	if s := e.Shape; s != nil && s.parent == o.shape && r.addIC(o, key, v, e) {
		return true
	}
	if !addsTo(o) {
		return false
	}
	before := o.shape
	if before.noFill != 0 || before.has(key) || !protoChainAdds(o.Proto(), key) {
		return false
	}
	o.addNamed(r, key, propCell{value: v, attrs: attrDefault})
	// An owned shape is its object's alone: no other object may take it
	// through the cache.
	if s := o.shape; s.parent == before && !s.owned {
		*e = newICEntry(s, r.protoEpoch, s.slot, icAdd)
	}
	return true
}

// addsTo reports whether setNamedAdd may add to o: an ordinary extensible
// shape-mode object that is no prototype.
func addsTo(o *Object) bool {
	return o.flags&(flagExtensible|flagDict|flagHasLazy|flagIsPrototype|flagShared) == flagExtensible &&
		o.class != ClassArray && o.class != ClassString && o.class != ClassTypedArray && o.class != ClassProxy
}

// addIC performs the add of key to o that e records from o's shape
// (setNamedAdd) and reports whether it did. Its callers test that e's
// shape follows o's first, without the call, which a site that sees
// several shapes misses on.
func (r *Realm) addIC(o *Object, key PropertyKey, v Value, e *ICEntry) bool {
	s := e.Shape
	if e.Epoch != r.protoEpoch || e.HolderDepth() != icAdd || s.key != key || !addsTo(o) {
		return false
	}
	o.shape = s
	if len(o.slots) == cap(o.slots) {
		o.growSlots(r)
	}
	o.slots = append(o.slots, v)
	return true
}

// protoChainAdds reports whether OrdinarySet of key on an object whose
// prototype is p reaches the receiver's own definition of key: the first
// object on the chain from p that has key has it as a writable data
// property, or none has it, and none before it could answer the write
// otherwise (a proxy runs its trap, a typed array answers numeric keys, a
// pending lazy definition of key would define it). An array's length is
// never cached: making it read-only sets a bit, which bumps no epoch.
//
// A cached add depends on the chain's own properties of key and their
// attributes, and on the chain itself: every change to them changes a
// prototype's shape, attributes or [[Prototype]], which bumps the realm's
// protoEpoch (bumpEpoch), and the entry's epoch no longer matches.
func protoChainAdds(p *Object, key PropertyKey) bool {
	for ; p != nil; p = p.Proto() {
		if p.class == ClassProxy || p.class == ClassTypedArray || p.flags&flagHasLazy != 0 && p.lazyPending(key) {
			return false
		}
		if key == lengthKey && p.class == ClassArray {
			return false
		}
		if c, ok := p.getOwnCell(key); ok {
			return c.attrs&(attrAccessor|attrWritable) == attrWritable
		}
	}
	return true
}

// fillSetIC records where a successful [[Set]] of key on o wrote, if the
// location is cacheable: an own writable data property of a shape-mode
// object, or a setter, own or inherited. A setter is filled by fillGetIC,
// with its refusals; the shape %RegExp% shares while its statics are
// pending has no writable data property.
func (r *Realm) fillSetIC(e *ICEntry, o *Object, key PropertyKey) {
	if o.flags&(flagDict|flagShared) != 0 || key.IsIndex() || o.flags&flagHasLazy != 0 && o.lazyPending(key) {
		return
	}
	if key == lengthKey && (o.class == ClassArray || o.class == ClassString) {
		return
	}
	slot, attrs, ok := o.shape.Lookup(key)
	if !ok || attrs&attrAccessor != 0 {
		// The assignment went to a setter: cache it like a get would.
		var t ICEntry
		r.fillGetIC(&t, o, key)
		if t.Epoch&1 != 0 && t.loc != icAbsent && t.Holder(o).slots[t.Slot()].asAccessor().Set != nil {
			*e = t
		}
		return
	}
	if attrs&attrWritable == 0 {
		return
	}
	*e = newICEntry(o.shape, r.protoEpoch, slot, 0)
}

// getGlobalSlow resolves an identifier against the global environment: the
// global lexical bindings of scripts, then the global object.
func (r *Realm) getGlobalSlow(key PropertyKey, e *ICEntry, orUndef bool) (Value, error) {
	g := r.Global
	if a := r.accessorIC(e, g); a != nil {
		return r.callGetter(a, ObjectValue(g))
	}
	if l, i := r.globalLexical(key, e); l != nil {
		return l.get(r, key, i)
	}
	if has, err := r.hasProperty(g, key); err != nil {
		return Undefined(), err
	} else if !has {
		if orUndef {
			return Undefined(), nil
		}
		return Undefined(), r.ReferenceError("%s is not defined", key.GoString())
	}
	if err := r.globalStillExists(g, key); err != nil {
		return Undefined(), err
	}
	return r.getNamedSlow(g, ObjectValue(g), key, e)
}

// setGlobalSlow assigns to a declared global (strict mode rejects new ones);
// in a function that is not strict, it is setGlobalSloppy.
func (r *Realm) setGlobalSlow(key PropertyKey, v Value, e *ICEntry) error {
	if !r.frameStrict() {
		return r.setGlobalSloppy(key, v, e)
	}
	g := r.Global
	if a := r.accessorIC(e, g); a != nil {
		return r.callSetter(a, ObjectValue(g), key, v)
	}
	if l, i := r.globalLexical(key, e); l != nil {
		return l.set(r, key, i, v)
	}
	if has, err := r.hasProperty(g, key); err != nil {
		return err
	} else if !has {
		return r.ReferenceError("%s is not defined", key.GoString())
	}
	if err := r.globalStillExists(g, key); err != nil {
		return err
	}
	return r.setNamedSlow(g, key, v, e)
}

// globalStillExists is the HasProperty of GetBindingValue and
// SetMutableBinding (ES2025 9.1.1.2.6 and 9.1.1.2.5), which follows the one
// that resolved the identifier: a ReferenceError when the binding is gone.
// Only a proxy on the global's prototype chain can make the two answers
// differ, or see that there are two. (A compound assignment runs the read's
// and the write's pairs, one call more than the spec's.)
func (r *Realm) globalStillExists(g *Object, key PropertyKey) error {
	for p := g.Proto(); p != nil; p = p.Proto() {
		if p.class != ClassProxy {
			continue
		}
		if has, err := r.hasProperty(g, key); err != nil || has {
			return err
		}
		return r.ReferenceError("%s is not defined", key.GoString())
	}
	return nil
}

// defineFieldSlow is CreateDataPropertyOrThrow for an object literal field,
// caching the shape transition when the object stays in shape mode.
func (r *Realm) defineFieldSlow(o *Object, key PropertyKey, v Value, e *ICEntry) error {
	before := o.shape
	if o.flags&(flagDict|flagHasLazy|flagShared) == 0 && o.flags&flagExtensible != 0 {
		if before.noFill == 0 && !before.has(key) {
			o.addNamed(r, key, propCell{value: v, attrs: attrDefault})
			if o.flags&flagDict == 0 && o.shape.parent == before && !o.shape.owned && o.flags&flagIsPrototype == 0 {
				e.Shape = o.shape
			}
			return nil
		}
	}
	return o.CreateDataPropertyOrThrow(r, key, v)
}

// defineElemSlow is CreateDataPropertyOrThrow with a computed key.
// defineMethodSlow is DefineElem for an object-literal method with a
// computed key: the key is converted once, the anonymous function is named
// after it (SetFunctionName) and the property is created.
func (r *Realm) defineMethodSlow(o *Object, k, fn Value) error {
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return err
	}
	name, err := r.keyFunctionName(key, "")
	if err != nil {
		return err
	}
	r.setFunctionName(fn.AsObject(), name)
	return o.CreateDataPropertyOrThrow(r, key, fn)
}

// defineAccessor implements the DefineAccessor op: fn becomes the getter or
// setter half of property k, merged with an existing accessor's other half
// (ValidateAndApplyPropertyDescriptor), replacing a data property.
func (r *Realm) defineAccessor(o *Object, k Value, fn *Object, flags uint32) error {
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return err
	}
	prefix := "get "
	if flags&bytecode.AccessorSetter != 0 {
		prefix = "set "
	}
	if flags&bytecode.AccessorNameKey != 0 {
		name, err := r.keyFunctionName(key, prefix)
		if err != nil {
			return err
		}
		r.setFunctionName(fn, name)
	}
	var d PropertyDescriptor
	if flags&bytecode.AccessorSetter != 0 {
		d.SetSet(ObjectValue(fn))
	} else {
		d.SetGet(ObjectValue(fn))
	}
	d.SetEnumerable(flags&bytecode.AccessorEnumerable != 0)
	d.SetConfigurable(true)
	return o.DefinePropertyOrThrow(r, key, d)
}

// keyFunctionName is the name SetFunctionName gives a function defined
// under key: a symbol contributes "[description]", and prefix ("get ",
// "set " or "") comes first.
func (r *Realm) keyFunctionName(key PropertyKey, prefix string) (*String, error) {
	if !key.IsSymbol() && prefix == "" {
		return key.ToJSString(r), nil
	}
	var sb StringBuilder
	sb.WriteGoString(prefix)
	if !key.IsSymbol() {
		sb.WriteString(key.ToJSString(r))
	} else if d := key.Symbol().Description(); d != nil {
		sb.WriteASCII('[')
		sb.WriteString(d)
		sb.WriteASCII(']')
	}
	if err := sb.checkLength(r); err != nil {
		return nil, err
	}
	return sb.String(), nil
}

func (r *Realm) defineElemSlow(o *Object, k, v Value) error {
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return err
	}
	return o.CreateDataPropertyOrThrow(r, key, v)
}

// --- elements ---------------------------------------------------------------------

// getElemSlow is the generic computed property read.
func (r *Realm) getElemSlow(o, k Value) (Value, error) {
	if o.IsObject() && k.IsNumber() && o.AsObject().class == ClassTypedArray {
		return typedArrayGetNumber(o.AsObject(), k.AsNumber()), nil
	}
	if o.IsNullish() {
		return Undefined(), r.TypeError("Cannot read properties of %s (reading '%s')", o.String(), k.String())
	}
	if o.IsString() && k.IsNumber() {
		s := o.AsString()
		if i, ok := k.IsArrayIndex(); ok {
			if int64(i) < int64(s.Len()) {
				return StringValue(charString(s.At(int(i)))), nil
			}
			return Undefined(), nil
		}
	}
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return Undefined(), err
	}
	if o.IsObject() {
		return o.AsObject().Get(r, key, o)
	}
	return r.GetV(o, key)
}

// setElemSlow is run's SetElem past its fast paths, the generic computed
// property write: strict, or ignoring a rejected assignment as
// setElemSloppy does when the innermost frame's function is not strict,
// as the compiler emits SetElem in sloppy code too.
func (r *Realm) setElemSlow(o, k, v Value) error {
	if o.IsNullish() {
		return r.TypeError("Cannot set properties of %s (setting '%s')", o.String(), k.String())
	}
	if o.IsObject() && k.IsNumber() && o.AsObject().class == ClassTypedArray {
		return r.typedArraySetNumber(o.AsObject(), k.AsNumber(), v)
	}
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return err
	}
	if o.IsObject() {
		// Strictness matters only once the write is rejected.
		obj := o.AsObject()
		ok, err := obj.Set(r, key, v, o)
		if err == nil && !ok && r.frameStrict() {
			err = r.readOnlyError(obj, key)
		}
		return err
	}
	if !r.frameStrict() {
		return r.setVSloppy(o, key, v)
	}
	return r.SetV(o, key, v)
}

// frameStrict reports whether the innermost frame's function is strict.
func (r *Realm) frameStrict() bool {
	st := &r.interp
	return st.frames[st.nframes-1].fn.funcData().code.Strict
}

// deleteSlow implements the strict `delete` operator for base and key values.
func (r *Realm) deleteSlow(base, k Value) (bool, error) {
	if base.IsNullish() {
		return false, r.TypeError("Cannot convert undefined or null to object")
	}
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return false, err
	}
	if !base.IsObject() {
		o, err := r.ToObject(base)
		if err != nil {
			return false, err
		}
		return true, o.DeletePropertyOrThrow(r, key)
	}
	return true, base.AsObject().DeletePropertyOrThrow(r, key)
}

// --- operators --------------------------------------------------------------------

// arithSlow applies -, *, /, %, ** to non-number operands.
func (r *Realm) arithSlow(op bytecode.Op, x, y Value) (Value, error) {
	nx, err := r.ToNumeric(x)
	if err != nil {
		return Undefined(), err
	}
	ny, err := r.ToNumeric(y)
	if err != nil {
		return Undefined(), err
	}
	if nx.IsBigInt() || ny.IsBigInt() {
		return r.bigintBinary(op, nx, ny)
	}
	return NumberValue(numericOp(op, nx.AsNumber(), ny.AsNumber())), nil
}

// bitwiseSlow applies a bitwise or shift operator to non-number operands.
func (r *Realm) bitwiseSlow(op bytecode.Op, x, y Value) (Value, error) {
	nx, err := r.ToNumeric(x)
	if err != nil {
		return Undefined(), err
	}
	ny, err := r.ToNumeric(y)
	if err != nil {
		return Undefined(), err
	}
	if nx.IsBigInt() || ny.IsBigInt() {
		return r.bigintBinary(op, nx, ny)
	}
	return bitwiseOp(op, nx.AsNumber(), ny.AsNumber()), nil
}

// incSlow is ++/-- on a non-number operand.
func (r *Realm) incSlow(x Value, d float64) (Value, error) {
	n, err := r.ToNumeric(x)
	if err != nil {
		return Undefined(), err
	}
	if n.IsBigInt() {
		return r.bigintUnary(n.AsBigInt(), int64(d), false)
	}
	return NumberValue(n.AsNumber() + d), nil
}

// negSlow is unary minus on a non-number operand.
func (r *Realm) negSlow(x Value) (Value, error) {
	n, err := r.ToNumeric(x)
	if err != nil {
		return Undefined(), err
	}
	if n.IsBigInt() {
		return r.bigintUnary(n.AsBigInt(), 0, false)
	}
	return NumberValue(-n.AsNumber()), nil
}

// bitNotSlow is ~ on a non-number operand.
func (r *Realm) bitNotSlow(x Value) (Value, error) {
	n, err := r.ToNumeric(x)
	if err != nil {
		return Undefined(), err
	}
	if n.IsBigInt() {
		return r.bigintUnary(n.AsBigInt(), 0, true)
	}
	return IntValue(int(^ToInt32Float(n.AsNumber()))), nil
}

// compareSlow evaluates a relational operator on non-number operands.
func (r *Realm) compareSlow(op bytecode.Op, x, y Value) (bool, error) {
	if x.IsString() && y.IsString() {
		c := x.AsString().Compare(y.AsString())
		switch op {
		case bytecode.Lt:
			return c < 0, nil
		case bytecode.Le:
			return c <= 0, nil
		case bytecode.Gt:
			return c > 0, nil
		}
		return c >= 0, nil
	}
	switch op {
	case bytecode.Lt:
		return r.LessThan(x, y)
	case bytecode.Le:
		return r.LessThanOrEqual(x, y)
	case bytecode.Gt:
		return r.GreaterThan(x, y)
	}
	return r.GreaterThanOrEqual(x, y)
}

// --- calls ------------------------------------------------------------------------

// constructValue implements `new fn(...args)`.
func (r *Realm) constructValue(fn Value, args []Value) (Value, error) {
	if !IsConstructor(fn) {
		return Undefined(), r.calleeError(fn, "is not a constructor")
	}
	o := fn.AsObject()
	fd := o.funcData()
	if r.callDepth >= MaxCallDepth {
		return Undefined(), r.RangeError("Maximum call stack size exceeded")
	}
	r.callDepth++
	var (
		res Value
		err error
	)
	switch fd.kind {
	case FuncBytecode:
		res, err = interpConstruct(r, o, fd, args, o)
	case FuncNative:
		res, err = fd.ctor()(r, args, o)
	default:
		r.callDepth--
		return r.Construct(fn, args, nil)
	}
	r.callDepth--
	return res, err
}

// constructAt is constructValue for run's New w, whose arguments are the
// argc = B registers from the stack's hb. One the compiler marked
// bytecode.CallOverlap enters an ordinary function's frame at hb, as
// callAt does a call's (enterAt); the others, and every other kind of
// constructor, take constructValue's paths.
func (r *Realm) constructAt(fn Value, hb int, w uint32) (Value, error) {
	if !IsConstructor(fn) {
		return Undefined(), r.calleeError(fn, "is not a constructor")
	}
	argc := int(uint8(w >> 16))
	args := r.interp.stack[hb : hb+argc : hb+argc]
	o := fn.AsObject()
	fd := o.funcData()
	if r.callDepth >= MaxCallDepth {
		return Undefined(), r.RangeError("Maximum call stack size exceeded")
	}
	r.callDepth++
	var (
		res Value
		err error
	)
	switch fd.kind {
	case FuncBytecode:
		if w>>24&bytecode.CallOverlap == 0 || fd.code.Kind != bytecode.KindNormal || fd.code.NewTarget {
			res, err = interpConstruct(r, o, fd, args, o)
			break
		}
		var obj *Object
		if obj, err = r.constructThis(o); err == nil {
			res, err = r.enterAt(o, fd, ObjectValue(obj), hb, argc, hb)
		}
		if err != nil {
			res = Undefined()
			break
		}
		if !res.IsObject() {
			res = ObjectValue(obj)
		}
		noteCtorSlots(fd, res)
	case FuncNative:
		res, err = fd.ctor()(r, args, o)
	default:
		r.callDepth--
		return r.Construct(fn, args, nil)
	}
	r.callDepth--
	return res, err
}

// calleeError is the TypeError of a call or `new` instruction whose callee
// fn is not callable or constructible. callValue and constructValue run only
// from those instructions, so the innermost frame is executing it and the
// message can name the callee expression as written ("o.missing is not a
// function"); without a description it shows the value.
func (r *Realm) calleeError(fn Value, what string) error {
	name := ""
	if st := &r.interp; st.nframes > 0 && bytecode.DescribeCallee != nil {
		if fr := &st.frames[st.nframes-1]; fr.fn != nil {
			name = bytecode.DescribeCallee(fr.fn.funcData().code, fr.pc)
		}
	}
	if name == "" {
		name = r.DisplayString(fn)
	}
	return r.TypeError("%s %s", name, what)
}

// spreadArgs materializes the elements of the spread array v as an argument
// list in the free region of the register stack above top (no allocation;
// the callee's window overlaps it and copies the values out first).
func (r *Realm) spreadArgs(top int, v Value) ([]Value, error) {
	if !v.IsObject() || v.AsObject().class != ClassArray {
		return nil, r.TypeError("internal: spread arguments are not an array")
	}
	o := v.AsObject()
	n := int(o.arrayLen())
	st := &r.interp
	if top+n > len(st.stack) {
		r.growStack(top + n)
	}
	argv := st.stack[top : top+n : top+n]
	for i := range n {
		v, err := arrayElementAt(r, o, i)
		if err != nil {
			return nil, err
		}
		argv[i] = v
	}
	return argv, nil
}

// --- iteration --------------------------------------------------------------------

// appendSpread appends every value of iterable v to arr (spread never
// closes the iterator: an abrupt completion comes from the iterator itself
// or from the interrupt check).
func (r *Realm) appendSpread(arr *Object, v Value) error {
	it, pos, err := r.iterInit(v)
	if err != nil {
		return err
	}
	if it.IsObject() && pos.IsNumber() {
		src := it.AsObject()
		n := int(src.arrayLen())
		if src.class == ClassArray {
			arr.reserveElems(r, min(n, src.elemLen()))
		}
		for i := 0; i < n; i++ {
			e, err := arrayElementAt(r, src, i)
			if err != nil {
				return err
			}
			arr.Push(r, e)
			n = int(src.arrayLen())
			if err := interruptEvery(r, int64(i)); err != nil {
				return err
			}
		}
		return nil
	}
	if sg := r.segmentIterator(it, pos); sg != nil {
		// iterStep's segment iterator case, checked once.
		for k := int64(0); ; k++ {
			if err := interruptEvery(r, k); err != nil {
				return err
			}
			e, done := sg.step(r)
			if done {
				return nil
			}
			arr.Push(r, e)
		}
	}
	for k := int64(0); ; k++ {
		if err := interruptEvery(r, k); err != nil {
			return err
		}
		e, done, err := r.iterStep(&it, &pos)
		if err != nil || done {
			return err
		}
		arr.Push(r, e)
	}
}

// --- for-in -----------------------------------------------------------------------

// forInInit collects the enumerable string keys of v and its prototype
// chain (deduplicated, shadowed keys skipped) into an array of strings. A
// proxy on the chain contributes the keys of its ownKeys trap that its
// getOwnPropertyDescriptor trap reports enumerable, and its getPrototypeOf
// trap continues the walk.
func (r *Realm) forInInit(v Value) (keys, obj Value, err error) {
	if v.IsNullish() {
		return ObjectValue(r.NewArrayFromSlice(nil)), Undefined(), nil
	}
	o, err := r.ToObject(v)
	if err != nil {
		return Undefined(), Undefined(), err
	}
	if keys := r.forInOwn(o); keys != nil {
		return ObjectValue(keys), ObjectValue(o), nil
	}
	var (
		list  []Value
		seen  map[PropertyKey]struct{}
		first []PropertyKey // the receiver's own keys, enumerable or not: they shadow inherited ones
	)
	for cur, depth := o, 0; cur != nil; depth++ {
		own, err := r.ownPropertyKeys(cur)
		if err != nil {
			return Undefined(), Undefined(), err
		}
		if depth == 0 {
			first = own
		} else if seen == nil && len(own) > 0 {
			seen = make(map[PropertyKey]struct{}, len(first)+len(own))
			for _, k := range first {
				if !k.IsSymbol() {
					seen[k] = struct{}{}
				}
			}
		}
		for _, k := range own {
			if k.IsSymbol() {
				continue
			}
			var attrs uint8
			if cur.class == ClassProxy {
				d, ok, err := r.proxyGetOwnProperty(cur, k)
				if err != nil {
					return Undefined(), Undefined(), err
				}
				if !ok {
					continue
				}
				attrs = d.attrs
			} else {
				c, ok := cur.getOwnCell(k)
				if !ok {
					continue
				}
				attrs = c.attrs
			}
			if depth > 0 {
				if _, dup := seen[k]; dup {
					continue
				}
			}
			if seen != nil {
				seen[k] = struct{}{}
			}
			if attrs&attrEnumerable == 0 {
				continue
			}
			list = append(list, StringValue(k.ToJSString(r)))
		}
		if cur, err = r.getPrototypeOf(cur); err != nil {
			return Undefined(), Undefined(), err
		}
		if err := interruptEvery(r, int64(depth)); err != nil {
			return Undefined(), Undefined(), err
		}
	}
	return ObjectValue(r.NewArrayFromSlice(list)), ObjectValue(o), nil
}

// forInDepth bounds the prototype chain forInOwn checks: a longer one takes
// forInInit's walk, which checks the interrupt at each level.
const forInDepth = 8

// forInOwn is forInInit's key array when no prototype on o's chain has an
// enumerable string key: the keys are then o's own enumerable string keys
// in [[OwnPropertyKeys]] order (indices ascending, then strings in
// insertion order), none shadowed by another, so no set of the keys seen
// is needed. Each loop checks the chain, so a property a script adds to a
// prototype later counts from the next loop on. It returns nil, for the
// walk, when an object of the chain may have keys its storage does not
// list (forInPlain), a prototype has elements or the chain is longer than
// forInDepth. A short list shares the array's allocation
// (newArrayStorage). The keys of an object without elements in shape mode
// are its shape's, and come from the realm's cache once the shape's keys
// were listed (forInCache).
func (r *Realm) forInOwn(o *Object) *Object {
	if !forInPlain(o) {
		return nil
	}
	depth := 0
	for p := o.Proto(); p != nil; p = p.Proto() {
		if depth++; depth > forInDepth || !forInPlain(p) || p.elemLen() != 0 || p.hasEnumerableString() {
			return nil
		}
	}
	var e *forInCacheEntry
	if o.flags&flagDict == 0 && o.elemLen() == 0 && !o.shape.owned {
		if e = r.forInSlot(o.shape); e != nil && e.shape == o.shape {
			return e.keys
		}
	}
	n := 0
	for _, v := range o.elems() {
		if !v.IsHole() {
			n++
		}
	}
	if o.flags&flagDict == 0 {
		for _, p := range o.shape.Props() {
			if p.attrs&attrEnumerable != 0 && p.key.IsString() {
				n++
			}
		}
	} else {
		for i := range o.dictTable().entries {
			if e := &o.dictTable().entries[i]; e.live() && e.cell.attrs&attrEnumerable != 0 && e.key.IsString() {
				n++
			}
		}
	}
	ao, list := r.newArrayStorage(0, n)
	for i, v := range o.elems() {
		if !v.IsHole() {
			list = append(list, StringValue(r.indexString(uint32(i))))
		}
	}
	if o.flags&flagDict == 0 {
		for _, p := range o.shape.Props() {
			if p.attrs&attrEnumerable != 0 && p.key.IsString() {
				list = append(list, StringValue(p.key.String()))
			}
		}
	} else {
		for i := range o.dictTable().entries {
			if e := &o.dictTable().entries[i]; e.live() && e.cell.attrs&attrEnumerable != 0 && e.key.IsString() {
				list = append(list, StringValue(e.key.String()))
			}
		}
	}
	keys := r.initArray(ao, list, uint32(len(list)))
	if e != nil {
		keys.Freeze(r)
		*e = forInCacheEntry{o.shape, keys}
	}
	return keys
}

// forInCache maps shapes to the key arrays of for-in loops over objects
// without elements in shape mode, so that a loop over an object of a shape
// seen before allocates nothing: a template's loop over its props objects,
// a polyfill's over its option bags. Such an object's enumerable string
// keys are a function of its shape, which an object leaves for another
// whenever it adds a property, deletes one, changes its attributes or its
// prototype, or freezes. An owned shape's props grow in place, but an
// object of the shape never looks past its count, so its keys are the
// shape's too; owned shapes are left out all the same, since their objects
// (a global object) can have any number of keys. The chain is checked at
// every loop, the cache or not (forInOwn).
//
// A cached array is frozen: nothing the engine does changes a key array
// (forInNext only reads it), and crafted bytecode that passes the record's
// key list on as a value, or a suspended generator holding it, can only
// read it as well. Loops over objects of one shape share it.
//
// It is direct-mapped, slot by address, and holds each shape it maps, so
// another shape at its address can never be taken for it, as the key memo
// holds its strings (keyMemo). The entries keep their shapes and the
// arrays of their keys, which the live measurement counts (memlive.go);
// they keep nothing else of the loops, so ReleaseCallData leaves them, as
// it leaves the inline caches, which keep shapes too.
type forInCache [forInCacheSize]forInCacheEntry

type forInCacheEntry struct {
	shape *Shape
	keys  *Object
}

// forInCacheSize entries take 4 KiB, allocated when a realm has run
// forInCacheAfter loops that the cache takes, so that a realm that runs a
// few pays nothing. At 256 entries 88 to 100 % of the loops of three
// server-side rendering workloads hit, at 128 86 to 100 %.
const (
	forInCacheBits  = 8
	forInCacheSize  = 1 << forInCacheBits
	forInCacheAfter = 256
)

// forInSlot returns the cache entry for s, by its address (Fibonacci
// hashing of the 16-byte granules, as keyMemoSlot), or nil while the
// realm has no cache, counting the loop then.
func (r *Realm) forInSlot(s *Shape) *forInCacheEntry {
	l := r.shapeState()
	if l.forIn == nil {
		if l.forInMisses < forInCacheAfter {
			l.forInMisses++
			return nil
		}
		l.forIn = new(forInCache)
		r.charge(int(unsafe.Sizeof(forInCache{})))
	}
	return &l.forIn[uint32(uintptr(unsafe.Pointer(s))>>4)*0x9e3779b9>>(32-forInCacheBits)]
}

// forInPlain reports whether o's own keys are those its storage lists: its
// elements, all enumerable, and its named properties. It is false for a
// class with keys of its own making (a proxy or module namespace, a String
// wrapper, a typed array, an arguments object: only plain objects, arrays
// and functions qualify), for keys still pending (lazy or cold properties,
// the Go map or slice of a host node) and for sparse elements, whose
// attributes vary and which are not in index order.
func forInPlain(o *Object) bool {
	switch o.class {
	case ClassObject, ClassArray, ClassFunction:
	default:
		return false
	}
	x := o.extData()
	return o.flags&(flagHasLazy|flagHostNode) == 0 && (x == nil || len(x.sparse) == 0)
}

// hasEnumerableString reports whether o has an enumerable own property
// with a string key among its named properties.
func (o *Object) hasEnumerableString() bool {
	if o.flags&flagDict == 0 {
		for _, p := range o.shape.Props() {
			if p.attrs&attrEnumerable != 0 && p.key.IsString() {
				return true
			}
		}
		return false
	}
	for i := range o.dictTable().entries {
		if e := &o.dictTable().entries[i]; e.live() && e.cell.attrs&attrEnumerable != 0 && e.key.IsString() {
			return true
		}
	}
	return false
}

// forInNext writes to *dst the next key that is still a property of obj,
// and reports false, writing nothing, when there is none. keys and idx are
// the key list and the index of a for-in record, an object and a
// non-negative integer, which the bytecode verifier proves
// (bytecode.KindOperands), as run's ForInNext relies on. A key that is no
// string is skipped: the list is the array of strings ForInInit made, but
// no proof covers what it holds. Bytecode may hand the record another
// object, and a suspended generator's list stays reachable through the
// registers it left on the stack, which a later frame may read before it
// writes them.
func forInNext(dst, keys, idx *Value, obj Value) bool {
	if !obj.IsObject() {
		return false
	}
	arr := keys.AsObject()
	o := obj.AsObject()
	list := arr.elemsReadOnly()
	for i := int(idx.AsNumber()); i < len(list); i++ {
		k := list[i]
		idx.setScalarBits(IntValue(i + 1))
		// Keys deleted during iteration are skipped. An ordinary object's
		// named property needs only its shape, without reading its value.
		if !k.IsString() {
			continue
		}
		key := keyOfString(o, k.AsString())
		own := false
		if o.class == ClassObject && o.flags&(flagDict|flagHasLazy|flagHostNode) == 0 && !key.IsIndex() {
			_, _, own = o.shape.Lookup(key)
		}
		if own || forInHas(o, key) {
			*dst = k
			return true
		}
	}
	return false
}

// forInDone is the pc ForInNext at pc jumps to once forInNext has no key,
// the end of the loop: its sBx read from the code again. Inlined, it costs
// run's size, which must stay below the compiler's big-function size
// (DESIGN §6), only its call.
func forInDone(insns codeWords, pc int) int {
	return pc + 1 + int(int16(insns.at(pc)>>16))
}

// forInHas is the check of forInNext: whether key is still a property of o
// or its chain. A proxy on the chain is taken to still have every key it
// reported (its traps ran in forInInit).
func forInHas(o *Object, key PropertyKey) bool {
	for obj := o; obj != nil; obj = obj.Proto() {
		if obj.class == ClassProxy || obj.HasOwnProperty(key) {
			return true
		}
		if obj.class == ClassTypedArray && typedArrayKey(key) {
			return false
		}
	}
	return false
}

// keyOfString rebuilds the property key of an enumerated key string. The
// strings come from PropertyKey.ToJSString: index keys are canonical decimal
// strings (some of them interned small-index atoms), the rest are atoms.
func keyOfString(o *Object, s *String) PropertyKey {
	if a, ok := s.ASCII(); ok && a != "" && a[0] <= '9' {
		if i, ok := parseArrayIndex(a); ok {
			return IndexKey(i)
		}
	}
	return PropertyKey{StringValue(s)}
}

// --- object spread ----------------------------------------------------------------

// copyDataProps implements CopyDataProperties(target, source, excluded):
// excluded is undefined or an array of key values.
func (r *Realm) copyDataProps(target *Object, source Value, excluded Value) error {
	// ToObject's wrapper of a boolean, number, symbol or bigint has no own
	// property: nothing to build for the false of {...(c && {k: v})}.
	if source.IsNullish() || !source.IsObject() && !source.IsString() {
		return nil
	}
	from, err := r.ToObject(source)
	if err != nil {
		return err
	}
	var excl []PropertyKey
	if excluded.IsObject() {
		// The keys a rest element's pattern names: a component's props
		// destructured before ...rest are often more than a few.
		var buf [32]PropertyKey
		excl = buf[:0]
		ks := excluded.AsObject().elemsReadOnly()
		if len(ks) > len(buf) {
			excl = make([]PropertyKey, 0, len(ks))
		}
		for _, k := range ks {
			pk, err := r.ToPropertyKey(k)
			if err != nil {
				return err
			}
			excl = append(excl, pk)
		}
	}
	if r.spreadFresh(target, from, excl) {
		return nil
	}
	if from.class == ClassProxy {
		return r.copyProxyDataProps(target, from, excl)
	}
	if hasOnlyShapeProps(from) {
		// The keys are those of from's shape now, read in place: a getter
		// that changes from gives it another shape. While from keeps it, a
		// data property's value is the one in its slot.
		s := from.shape
		ps := s.Props()
		for _, strings := range [2]bool{true, false} {
			for i := range ps {
				if k := ps[i].key; k.IsString() == strings && !k.IsPrivate() {
					if len(target.slots) == cap(target.slots) {
						r.spreadRoom(target, ps, i, strings, excl)
					}
					var err error
					if from.shape != s || ps[i].attrs&(attrAccessor|attrLazy) != 0 {
						err = r.copyDataProp(target, from, k, excl)
					} else if ps[i].attrs&attrEnumerable != 0 && !slices.Contains(excl, k) {
						err = r.createDataProp(target, k, from.slots[i])
					}
					if err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	keys, err := r.ownPropertyKeys(from)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := r.copyDataProp(target, from, k, excl); err != nil {
			return err
		}
	}
	return nil
}

// spreadRoom gives target, whose slots are full, room at once for the keys
// of a shape left to copy into it (copyDataProps: ps[i:] of the pass over
// strings or symbols, then the symbols) that it lacks, where the next is
// new to it, in place of a growth for each: a spread into a literal with
// keys before it ({ref, ...props}, {...defaults, ...options}) adds a few
// keys to an object of many, or many to an object of a few, where append's
// doubling leaves twice the room it needs or grows several times. Where
// objects before took the shapes those keys lead to, it gives the room of
// the trunk below the last (Shape.trunkSlots), for the keys the literal
// goes on to add after the spread, even for one key; otherwise that below
// target's shape, and one key is left to growSlots.
func (r *Realm) spreadRoom(target *Object, ps []shapeProp, i int, strings bool, excl []PropertyKey) {
	s := target.shape
	if target.flags&flagDict != 0 || target.class != ClassObject {
		return
	}
	if _, _, ok := s.Lookup(ps[i].key); ok {
		return
	}
	n, end := 0, s
	for pass := range 2 {
		for j := range ps {
			p := ps[j]
			if pass == 0 && (j < i || p.key.IsString() != strings) || pass == 1 && (!strings || p.key.IsString()) ||
				p.attrs&attrEnumerable == 0 || p.key.IsPrivate() || slices.Contains(excl, p.key) {
				continue
			}
			if _, _, ok := s.Lookup(p.key); ok {
				continue // a value for a key target has
			}
			n++
			if end != nil {
				if c := end.child(p.key, attrDefault); c != nil {
					end = c
				} else if _, _, ok := end.Lookup(p.key); !ok {
					end = nil
				}
			}
		}
	}
	l := len(target.slots)
	want := l + n
	if end != nil {
		want = max(want, end.trunkSlots(int(end.count)))
	} else {
		want = max(want, s.trunkSlots(l))
	}
	if want = min(want, maxShapeProps); want > l+1 || end != nil && want > l {
		// As growSlots, but from no capacity: append doubles past full
		// slots, where the keys left may need far less.
		ns := slices.Grow([]Value(nil), want)[:l]
		copy(ns, target.slots)
		target.slots = ns
		r.charge(cap(ns) * valueSize)
	}
}

// spreadPairSource returns the source of the spread into the same object
// that follows the spread w right away in the instructions next, where
// w's own source is read: {...defaults, ...options}, the second source in
// a register or a closure's variable (GetEnv, GetEnv0, GetEnv1, GetEnv2,
// or GetEnvChk, whose extra word, the name constant, comes before the
// spread). It returns undefined for anything else.
func spreadPairSource(w uint32, next []uint32, regs []Value, env *Env) Value {
	if len(next) == 0 {
		return Undefined()
	}
	if n := next[0]; n&0xffff == w&0xffff {
		return regs[uint8(n>>16)]
	} else if op := bytecode.Op(n); op == bytecode.GetEnv || op == bytecode.GetEnv0 || op == bytecode.GetEnv1 || op == bytecode.GetEnv2 || op == bytecode.GetEnvChk {
		if j := 1 + op.ExtraWords(); len(next) > j && next[j]&0xffff == w&0xffff && uint8(next[j]>>16) == uint8(n>>8) {
			return env.up(uint8(n >> 16)).slots()[uint8(n>>24)]
		}
	}
	return Undefined()
}

// spreadPairRoom gives target, a literal's fresh object that the spread of
// from starts and the spread of next continues ({...defaults, ...options},
// copyDataProps twice), room at once for from's keys, those of next that
// from lacks and those the objects before went on to add after them (the
// trunk below, Shape.trunkSlots), where the first spread would give the
// room of from's shape and the second grow it (spreadRoom).
func (r *Realm) spreadPairRoom(target, from *Object, next Value) {
	if !next.IsObject() || target.shape != r.plainRoot || len(target.slots) != 0 {
		return
	}
	nx := next.AsObject()
	for _, o := range [2]*Object{from, nx} {
		if o.class != ClassObject || o.flags&(flagDict|flagHasLazy|flagHostNode) != 0 || o.shape.owned {
			return
		}
	}
	s := from.shape
	n, end := int(s.count), s
	for _, p := range nx.shape.Props() {
		if p.attrs&attrEnumerable == 0 || p.key.IsPrivate() {
			continue
		}
		if _, _, ok := s.Lookup(p.key); ok {
			continue
		}
		if n++; end != nil && p.key.IsString() {
			end = end.child(p.key, attrDefault)
		} else {
			end = nil // a symbol: copied after the strings
		}
	}
	if n == int(s.count) {
		return
	}
	if end != nil {
		n = max(n, end.trunkSlots(int(end.count)))
	}
	if n = min(n, maxShapeProps); cap(target.slots) < n {
		target.slots = r.growValues(target.slots, n)
	}
}

// copyDataProp is one key of copyDataProps.
func (r *Realm) copyDataProp(target, from *Object, k PropertyKey, excl []PropertyKey) error {
	if slices.Contains(excl, k) {
		return nil
	}
	c, ok := from.getOwnCell(k)
	if !ok || c.attrs&attrEnumerable == 0 {
		return nil
	}
	v := c.value // what [[Get]] returns of an ordinary object's data property
	if c.attrs&attrAccessor != 0 || from.class != ClassObject {
		var err error
		if v, err = from.Get(r, k, ObjectValue(from)); err != nil {
			return err
		}
	}
	return r.createDataProp(target, k, v)
}

// createDataProp is target.CreateDataPropertyOrThrow(r, k, v) for
// copyDataProps. An ordinary target in shape mode with nothing lazy takes
// a named key it lacks, or a new value for one whose attributes
// CreateDataProperty leaves as they are, after one lookup, where
// [[DefineOwnProperty]] builds a descriptor of the current property and
// validates the new one against it. Any other takes the general path.
func (r *Realm) createDataProp(target *Object, k PropertyKey, v Value) error {
	if target.class == ClassObject && target.flags&(flagExtensible|flagDict|flagHasLazy|flagShared) == flagExtensible && !k.IsIndex() {
		slot, attrs, ok := target.shape.Lookup(k)
		if !ok {
			target.addNamed(r, k, propCell{value: v, attrs: attrDefault})
			return nil
		}
		if attrs == attrDefault {
			target.slots[slot] = v
			return nil
		}
	}
	return target.CreateDataPropertyOrThrow(r, k, v)
}

// spreadFresh is copyDataProps for a spread ({...o}) or a rest element
// ({a, ...rest} = o) into target, a fresh object of Object.prototype with
// no property yet, from an ordinary object in shape mode whose keys its
// storage lists, all of them data properties. The general path pays a
// [[GetOwnProperty]], a [[Get]] and a [[DefineOwnProperty]] per key, each
// with a lookup; here the keys come from from's shape and the values from
// its slots and elements, which hold what [[Get]] returns. target lists
// them in [[OwnPropertyKeys]] order: the elements ascending, then the
// strings and then the symbols in creation order.
//
// When every property is enumerable, writable and configurable, none is a
// private name or excluded, and from's shape has target's prototype,
// target takes that shape with a copy of the slots, and room for the
// properties the trunk below it adds (Shape.trunkSlots): its keys and
// attributes are those the general path gives target, and their
// [[OwnPropertyKeys]] order is the same, as it lists a shape's strings
// before its symbols wherever they were added. The shape must be one any
// object of r may hold, as an inline cache keyed on it then serves both:
// one of the shared tree or of r's own tree (from r.plainRoot, or from the
// local root a shared realm continues the shared tree on, localize). Never
// an owned shape, which grows in place (Shape.addOwned), nor one of
// another realm's tree, whose transitions that realm writes. Otherwise the
// copied keys are added from target's root through the transitions
// addNamed takes, the strings and then the symbols, so target gets the
// shape the general path gives it, and stays in shape mode as it does
// there: a shape that is not owned has at most maxShapeProps properties.
//
// It reports false, having changed nothing, for the general path: a target
// that is not fresh; a source of another class (a proxy or module
// namespace, an array, a String wrapper, a typed array, an arguments
// object); one with lazy properties pending, a host node over a Go map or
// slice, an internal payload, sparse elements or a dictionary; an accessor
// or a lazy slot; a shape that is owned or bars inline caches (noFill).
func (r *Realm) spreadFresh(target, from *Object, excl []PropertyKey) bool {
	if target.shape != r.plainRoot || target.flags != flagExtensible || target.class != ClassObject ||
		target.elemLen() != 0 || target.dictTable() != nil {
		return false
	}
	if from.class != ClassObject || from.flags&(flagDict|flagHasLazy|flagHostNode) != 0 || from.pkind != 0 ||
		(from.dictTable() != nil && len(from.dictTable().sparse) != 0) {
		return false
	}
	s := from.shape
	if s.owned || s.noFill != 0 || s.count > maxShapeProps {
		return false
	}
	props := s.Props()
	adopt, copied := s.proto == target.Proto(), 0
	for _, p := range props {
		if p.attrs&(attrAccessor|attrLazy) != 0 {
			return false
		}
		if p.attrs&attrEnumerable != 0 && !p.key.IsPrivate() && !slices.Contains(excl, p.key) {
			copied++
		}
		if p.attrs != attrDefault {
			adopt = false
		}
	}
	adopt = adopt && copied == len(props)
	if adopt && !s.shared {
		root := s.root()
		adopt = root == r.plainRoot || root == r.rootShapes[target.Proto()]
	}
	if n := from.elemLen(); n != 0 {
		elems := r.growValues(nil, n)[:n]
		copy(elems, from.elems())
		for _, k := range excl {
			if k.IsIndex() && int64(k.Index()) < int64(n) {
				elems[k.Index()] = Hole()
			}
		}
		for n > 0 && elems[n-1].IsHole() { // an excluded last index
			n--
		}
		target.setElems(elems[:n])
	}
	if adopt {
		if cap(target.slots) < copied {
			// With room for the properties the objects that took s went on
			// to get, as the literal's own after the spread.
			target.slots = r.growValues(target.slots, s.trunkSlots(copied))
		}
		target.slots = target.slots[:copied]
		copy(target.slots, from.slots)
		target.shape = s
		return true
	}
	shape, slots := target.shape, target.slots
	if cap(slots) < copied {
		slots = r.growValues(slots, copied)
	}
	symbols := false
	for i, p := range props {
		if p.attrs&attrEnumerable == 0 || p.key.IsPrivate() || slices.Contains(excl, p.key) {
			continue
		}
		if !p.key.IsString() {
			symbols = true
			continue
		}
		shape = shape.addProperty(r, p.key, attrDefault)
		slots = append(slots, from.slots[i])
	}
	if symbols {
		for i, p := range props {
			if p.attrs&attrEnumerable != 0 && p.key.IsSymbol() && !slices.Contains(excl, p.key) {
				shape = shape.addProperty(r, p.key, attrDefault)
				slots = append(slots, from.slots[i])
			}
		}
	}
	target.shape, target.slots = shape, slots
	return true
}

// caseChain runs the cases of a switch from fd's JmpCaseK at pc: it and
// each JmpStrictEqK after it that tests the same register, whose value is
// x. It returns the target of the first whose constant x equals, or the
// pc of the first instruction past them. A string compares by pointer,
// then by length; two atoms of one length are different strings.
func caseChain(fd *FunctionData, pc int, x Value) int {
	insns, consts := fd.code.Code, fd.meta.consts
	next := uint32(bytecode.JmpStrictEqK) | insns[pc]&0xFF00
	xs := (*String)(nil)
	if x.IsString() {
		xs = x.AsString()
	}
	for {
		w := insns[pc]
		// The verifier's strictOperand: y is a string, null or undefined.
		y := consts[insns[pc+1]]
		if x.bits == y.bits {
			if x.ptr == y.ptr {
				return bytecode.JumpTarget(pc, w)
			}
			if xs != nil && y.ptr != nil {
				ys := y.AsString()
				if xs.n == ys.n && (xs.atom == 0 || ys.atom == 0) && xs.Equals(ys) {
					return bytecode.JumpTarget(pc, w)
				}
			}
		}
		pc += 2
		if insns[pc]&0xFFFF != next {
			return pc
		}
	}
}

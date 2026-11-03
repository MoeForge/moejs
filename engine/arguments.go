package engine

import (
	"github.com/MoeForge/moejs/bytecode"
	"unsafe"
)

// Arguments objects. A function gets one only when its body or a nested
// arrow references `arguments` (the compiler sets
// bytecode.Function.HasArguments and enterFrame calls newArguments). The
// object is unmapped (strict code, or parameters that are not simple); a
// sloppy function with simple parameters turns it into the mapped exotic
// object (MapArguments at its entry, mapArguments), whose indices alias the
// parameters' Env slots.

// argumentsObject co-allocates an arguments object with its three named
// slots (length, @@iterator, callee) and up to four elements.
type argumentsObject struct {
	Object
	slots [3]Value
	elems [4]Value
}

// argumentsShape returns the realm's shape of a fresh arguments object:
// Object.prototype root + length + @@iterator + callee.
func (r *Realm) argumentsShape() *Shape {
	l := r.lazyState()
	if l.argumentsShape == nil {
		l.argumentsShape = r.plainRoot.
			addProperty(r, lengthKey, attrHidden).
			addProperty(r, SymbolKey(SymIterator), attrHidden).
			addProperty(r, StringKey(AtomCallee), attrAccessor)
	}
	return l.argumentsShape
}

// newArguments implements CreateUnmappedArgumentsObject: indexed data
// properties copied from args, a non-enumerable length, @@iterator =
// %Array.prototype.values% and a %ThrowTypeError% callee accessor.
func (r *Realm) newArguments(args []Value) *Object {
	x := &argumentsObject{}
	r.chargeObject(unsafe.Sizeof(argumentsObject{}))
	o := initObject(&x.Object, ClassArguments, r.argumentsShape())
	o.slots = x.slots[:]
	o.slots[0] = IntValue(len(args))
	o.slots[1] = ObjectValue(r.arrayValuesFn)
	o.slots[2] = accessorValue(r.restrictedAccessor)
	if n := len(args); n > 0 {
		elems := x.elems[:0:len(x.elems)]
		if n > len(x.elems) {
			elems = make([]Value, 0, n)
		}
		o.elements = append(elems, args...)
	}
	return o
}

// plainArguments reports whether the arguments object o still has
// %Array.prototype.values% as its own @@iterator data property, so iterating
// it cannot run user code beyond its element getters.
func (r *Realm) plainArguments(o *Object) bool {
	p, attrs, ok := o.lookupNamed(SymbolKey(SymIterator))
	return ok && attrs&attrAccessor == 0 && p.IsObject() && p.AsObject() == r.arrayValuesFn
}

// argumentsMap is the [[ParameterMap]] of a mapped arguments object (the
// object's internal): index i aliases slot slots[i] of env while slots[i]
// is not negative. The aliased indices keep their properties in the
// object's sparse storage, which keeps them off every dense fast path:
// lookupIndex reads their values from env and DefineOwnProperty
// (argumentsDefine) writes through to it.
type argumentsMap struct {
	env   *Env
	slots []int32
}

// mappedShape returns the realm's shape of a mapped arguments object: that
// of an unmapped one with a writable, configurable callee data property.
func (r *Realm) mappedArgumentsShape() *Shape {
	l := r.lazyState()
	if l.mappedShape == nil {
		l.mappedShape = r.plainRoot.
			addProperty(r, lengthKey, attrHidden).
			addProperty(r, SymbolKey(SymIterator), attrHidden).
			addProperty(r, StringKey(AtomCallee), attrHidden)
	}
	return l.mappedShape
}

// mapArguments completes CreateMappedArgumentsObject on the fresh arguments
// object o of a call to callee, the sloppy function of code with simple
// parameters whose own Env is env: callee becomes a data property and each
// index below both the argument count and the parameter count aliases the
// Env slot its parameter's register initialized (CaptureLayout). The
// compiler lays out the last of duplicate parameters, which is the one the
// spec maps.
func (r *Realm) mapArguments(o *Object, code *bytecode.Function, env *Env, callee *Object) {
	o.shape = r.mappedArgumentsShape()
	o.slots[2] = ObjectValue(callee)
	n := min(int(code.NumParams), len(o.elements))
	if n == 0 {
		return
	}
	m := &argumentsMap{env: env, slots: make([]int32, n)}
	for i := range m.slots {
		m.slots[i] = -1
	}
	for slot, reg := range code.CaptureLayout {
		if reg != bytecode.NoRegister && int(reg) < n {
			m.slots[reg] = int32(slot)
		}
	}
	sparse := o.sparseMap()
	for i, s := range m.slots {
		if s >= 0 {
			sparse[uint32(i)] = propCell{value: o.elements[i], attrs: attrDefault}
			o.elements[i] = Hole()
		}
	}
	o.trimTrailingHoles()
	o.internal = m
}

// mappedValue returns the value of the aliased index i of the mapped
// arguments object o, or false when i is not aliased.
func (o *Object) mappedValue(i uint32) (Value, bool) {
	m, ok := o.internal.(*argumentsMap)
	if !ok || int(i) >= len(m.slots) || m.slots[i] < 0 {
		return Value{}, false
	}
	return m.env.slots[m.slots[i]], true
}

// unmapArgument removes index i from the parameter map of the arguments
// object o.
func (o *Object) unmapArgument(i uint32) {
	if m, ok := o.internal.(*argumentsMap); ok && int(i) < len(m.slots) {
		m.slots[i] = -1
	}
}

// argumentsDefine is [[DefineOwnProperty]] of an index of a mapped
// arguments object (ES2025 10.4.4.2).
func (r *Realm) argumentsDefine(o *Object, key PropertyKey, desc PropertyDescriptor) bool {
	i := key.Index()
	mv, mapped := o.mappedValue(i)
	newDesc := desc
	if mapped && desc.IsDataDescriptor() && !desc.HasValue() && desc.HasWritable() && !desc.Writable() {
		newDesc.SetValue(mv)
	}
	var ok bool
	if cur, found := o.GetOwnProperty(key); found {
		ok = ValidateAndApplyPropertyDescriptor(r, o, key, o.IsExtensible(), newDesc, &cur)
	} else {
		ok = ValidateAndApplyPropertyDescriptor(r, o, key, o.IsExtensible(), newDesc, nil)
	}
	if !ok || !mapped {
		return ok
	}
	if desc.IsAccessorDescriptor() {
		o.unmapArgument(i)
		return true
	}
	if desc.HasValue() {
		m := o.internal.(*argumentsMap)
		m.env.slots[m.slots[i]] = desc.Value
	}
	if desc.HasWritable() && !desc.Writable() {
		o.unmapArgument(i)
	}
	return true
}

// freezeArguments prepares a mapped arguments object for Object.freeze,
// whose [[DefineOwnProperty]] calls with writable false unmap every index
// after copying its value into the property.
func (o *Object) freezeArguments() {
	m, ok := o.internal.(*argumentsMap)
	if !ok {
		return
	}
	for i, s := range m.slots {
		if s >= 0 {
			c := o.dict.sparse[uint32(i)]
			c.value = m.env.slots[s]
			o.dict.sparse[uint32(i)] = c
			m.slots[i] = -1
		}
	}
}

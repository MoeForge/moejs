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
// parameters' Env slots. When no code writes a parameter, the object keeps
// its elements and only notes the Env (argumentsEnv): an element then
// changes only through the object, which writes it through to the Env, and
// a define of an aliased index builds the map (mapLazyArguments).

// argumentsObject co-allocates an arguments object with its three named
// slots (length, @@iterator, callee) and up to four elements.
type argumentsObject struct {
	inlineObject
	slots [3]Value
	elems [4]Value
}

// argumentsShape returns the realm's shape of a fresh arguments object:
// Object.prototype root + length + @@iterator + callee.
func (r *Realm) argumentsShape() *Shape {
	l := r.shapeState()
	if l.arguments == nil {
		l.arguments = r.plainRoot.
			addProperty(r, lengthKey, attrHidden).
			addProperty(r, SymbolKey(SymIterator), attrHidden).
			addProperty(r, StringKey(AtomCallee), attrAccessor)
	}
	return l.arguments
}

// newArguments implements CreateUnmappedArgumentsObject: indexed data
// properties copied from args, a non-enumerable length, @@iterator =
// %Array.prototype.values% and a %ThrowTypeError% callee accessor.
func (r *Realm) newArguments(args []Value) *Object {
	x := &argumentsObject{}
	r.chargeObject(unsafe.Sizeof(argumentsObject{}))
	o := initObject(x.initInline(), ClassArguments, r.argumentsShape())
	o.slots = x.slots[:]
	o.slots[0] = IntValue(len(args))
	o.slots[1] = ObjectValue(r.arrayValuesFn)
	o.slots[2] = accessorValue(r.restrictedAccessor)
	if n := len(args); n > 0 {
		elems := x.elems[:0:len(x.elems)]
		if n > len(x.elems) {
			elems = make([]Value, 0, n)
		}
		o.setElems(append(elems, args...))
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
	l := r.shapeState()
	if l.mapped == nil {
		l.mapped = r.plainRoot.
			addProperty(r, lengthKey, attrHidden).
			addProperty(r, SymbolKey(SymIterator), attrHidden).
			addProperty(r, StringKey(AtomCallee), attrHidden)
	}
	return l.mapped
}

// argumentsEnv is the payload of a lazily mapped arguments object: the
// Env of the call whose parameter i is slot i. Index i below the object's
// aux, the aliased count, aliases slot i while the object has it as an
// element; the element and the slot hold the same value, since only a write
// through the object (Set, which writes both) changes either. A deleted
// index is a hole, unaliased for good: an index below aux the object gets
// again is defined, which builds the map first.
type argumentsEnv Env

// mapArguments completes CreateMappedArgumentsObject on the fresh arguments
// object o of a call to callee, the sloppy function of code with simple
// parameters whose own Env is env: callee becomes a data property and each
// index below both the argument count and the parameter count aliases the
// Env slot its parameter's register initialized (CaptureLayout). The
// compiler lays out the last of duplicate parameters, which is the one the
// spec maps. lazy (MapArguments B) defers the map: no code writes a
// parameter and parameter i is slot i (the verifier checks the layout).
func (r *Realm) mapArguments(o *Object, code *bytecode.Function, env *Env, callee *Object, lazy bool) {
	o.shape = r.mappedArgumentsShape()
	o.slots[2] = ObjectValue(callee)
	n := min(int(code.NumParams), o.elemLen())
	if n == 0 {
		return
	}
	if lazy {
		o.aux = uint32(n)
		setExtData(o, kindArgumentsEnv, (*argumentsEnv)(env))
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
			sparse[uint32(i)] = propCell{value: o.elems()[i], attrs: attrDefault}
			o.elems()[i] = Hole()
		}
	}
	o.trimTrailingHoles()
	setExtData(o, kindArgumentsMap, m)
}

// mapLazyArguments builds the parameter map of the lazily mapped arguments
// object o: each index below aux that o still has as an element aliases its
// Env slot and moves to the sparse storage, as mapArguments would have put
// it, with the attributes the elements have now (o may be sealed).
func (r *Realm) mapLazyArguments(o *Object) {
	env := (*Env)(extDataOf(o, kindArgumentsEnv))
	m := &argumentsMap{env: env, slots: make([]int32, o.aux)}
	attrs := o.elementAttrs()
	sparse := o.sparseMap()
	e := o.elems()
	for i := range m.slots {
		m.slots[i] = -1
		if i < len(e) && !e[i].IsHole() {
			m.slots[i] = int32(i)
			sparse[uint32(i)] = propCell{value: e[i], attrs: attrs}
			e[i] = Hole()
		}
	}
	o.trimTrailingHoles()
	o.aux = 0
	setExtData(o, kindArgumentsMap, m)
	r.bumpEpoch(o)
}

// setLazyArgument writes v through to the Env slot that index i of o
// aliases, when o is a lazily mapped arguments object and i is below its
// aliased count; o has just stored v as its element i.
func (o *Object) setLazyArgument(i uint32, v Value) {
	if l, ok := extDataOk(o, kindArgumentsEnv); ok && i < o.aux {
		(*Env)(l).slots()[i] = v
	}
}

// mappedValue returns the value of the aliased index i of the mapped
// arguments object o, or false when i is not aliased.
func (o *Object) mappedValue(i uint32) (Value, bool) {
	m, ok := extDataOk(o, kindArgumentsMap)
	if !ok || int64(i) >= int64(len(m.slots)) || m.slots[i] < 0 {
		return Value{}, false
	}
	return m.env.slots()[m.slots[i]], true
}

// unmapArgument removes index i from the parameter map of the arguments
// object o.
func (o *Object) unmapArgument(i uint32) {
	if m, ok := extDataOk(o, kindArgumentsMap); ok && int64(i) < int64(len(m.slots)) {
		m.slots[i] = -1
	}
}

// argumentsDefine is [[DefineOwnProperty]] of an index of a mapped
// arguments object (ES2025 10.4.4.2).
func (r *Realm) argumentsDefine(o *Object, key PropertyKey, desc PropertyDescriptor) bool {
	i := key.Index()
	if o.pkind == payloadKind(kindArgumentsEnv) && i < o.aux {
		r.mapLazyArguments(o)
	}
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
		m := extDataOf(o, kindArgumentsMap)
		m.env.slots()[m.slots[i]] = desc.Value
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
	if o.pkind == payloadKind(kindArgumentsEnv) {
		// The elements already hold the parameters' values.
		o.clearPayload()
		o.aux = 0
		return
	}
	m, ok := extDataOk(o, kindArgumentsMap)
	if !ok {
		return
	}
	for i, s := range m.slots {
		if s >= 0 {
			c := o.dictTable().sparse[uint32(i)]
			c.value = m.env.slots()[s]
			o.dictTable().sparse[uint32(i)] = c
			m.slots[i] = -1
		}
	}
}

// keepArguments copies the arguments of a call of a function with
// ForwardArguments above the frame's registers, whose top is the stack
// pointer, and raises the stack pointer over them: the frame keeps them
// there for ApplyArguments.
func (r *Realm) keepArguments(args []Value) {
	st := &r.interp
	top := st.sp
	if top+len(args) > len(st.stack) {
		r.growStack(top + len(args))
	}
	copy(st.stack[top:top+len(args)], args)
	st.sp = top + len(args)
}

// applyArguments executes ApplyArguments w, f.apply(x, arguments), of the
// innermost frame, a call of fd whose registers start at base and whose
// innermost environment is env.
func (r *Realm) applyArguments(fd *FunctionData, base int, env *Env, w uint32) (Value, error) {
	st := &r.interp
	code := fd.code
	top := base + int(code.NumRegs)
	regs := st.stack[base:top:top]
	a, b := int(uint8(w>>8)), int(uint8(w>>16))
	if v := regs[b]; !v.IsObject() {
		n, ok := v.IsInt32()
		if !ok || n < 0 || top+int(n) > st.sp {
			return Undefined(), r.wrongKind(w, "the count of the arguments its frame keeps")
		}
		kept := st.stack[top : top+int(n) : top+int(n)]
		if f := regs[a]; f.IsObject() && f.AsObject() == r.funcProtoApply && IsCallable(regs[a+1]) {
			// Function.prototype.apply's call, with the list it would
			// copy from the object, at the depth its own call counts.
			if r.callDepth >= MaxCallDepth {
				return Undefined(), r.RangeError("Maximum call stack size exceeded")
			}
			r.callDepth++
			res, err := r.callValue(regs[a+1], regs[a+2], kept)
			r.callDepth--
			return res, err
		}
		regs[b] = ObjectValue(r.createKeptArguments(fd, env, kept, w>>24 != 0))
	}
	regs[a+3] = regs[b]
	return r.callValue(regs[a], regs[a+1], regs[a+2:a+4:a+4])
}

// createKeptArguments creates the arguments object of the innermost frame,
// a call of fd with ForwardArguments whose innermost environment is env,
// from the arguments it kept: a mapped one when mapped, as MapArguments
// B = 1 makes it.
func (r *Realm) createKeptArguments(fd *FunctionData, env *Env, kept []Value, mapped bool) *Object {
	o := r.newArguments(kept)
	if mapped {
		var own *Env
		if fd.code.NumParams > 0 {
			own = ownEnv(fd, env)
		}
		st := &r.interp
		r.mapArguments(o, fd.code, own, st.frames[st.nframes-1].fn, true)
	}
	return o
}

// keptArguments returns the arguments the innermost frame keeps, a call of
// fd with ForwardArguments whose registers start at base, while its
// arguments register v holds their count, or ok false when v holds no
// count of them.
func (r *Realm) keptArguments(fd *FunctionData, base int, v Value) (kept []Value, ok bool) {
	st := &r.interp
	top := base + int(fd.code.NumRegs)
	n, isInt := v.IsInt32()
	if !isInt || n < 0 || top+int(n) > st.sp {
		return nil, false
	}
	return st.stack[top : top+int(n) : top+int(n)], true
}

// argsLen executes ArgsLen w, arguments.length, of the innermost frame, a
// call of fd whose registers start at base.
func (r *Realm) argsLen(fd *FunctionData, base int, w uint32) (Value, error) {
	v := r.interp.stack[base+int(uint8(w>>16))]
	if v.IsObject() {
		return v.AsObject().Get(r, lengthKey, v)
	}
	if _, ok := r.keptArguments(fd, base, v); !ok {
		return Undefined(), r.wrongKind(w, "the count of the arguments its frame keeps")
	}
	return v, nil
}

// argsGet executes ArgsGet w with its X word x, arguments[k], of the
// innermost frame, a call of fd whose registers start at base and whose
// innermost environment is env. A key other than an index below the count
// creates the object first, and reads the key from it.
func (r *Realm) argsGet(fd *FunctionData, base int, env *Env, w, x uint32) (Value, error) {
	st := &r.interp
	b := base + int(uint8(w>>16))
	v, k := st.stack[b], st.stack[base+int(uint8(w>>24))]
	if !v.IsObject() {
		kept, ok := r.keptArguments(fd, base, v)
		if !ok {
			return Undefined(), r.wrongKind(w, "the count of the arguments its frame keeps")
		}
		if i, ok := k.IsInt32(); ok && uint32(i) < uint32(len(kept)) {
			return kept[i], nil
		}
		v = ObjectValue(r.createKeptArguments(fd, env, kept, x != 0))
		st.stack[b] = v
	}
	return r.getElemSlow(v, k)
}

// ownEnv returns the Env a call of fd created at its entry, from env, the
// innermost environment of its frame: the Envs of block scopes descend from
// it, and its parent is the closure's.
func ownEnv(fd *FunctionData, env *Env) *Env {
	for env != nil && env.parent != fd.env {
		env = env.parent
	}
	return env
}

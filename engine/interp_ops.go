package engine

import (
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
	if int(x) < len(meta.names) && meta.names[x] != nil {
		name = meta.names[x].GoString()
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
	for holder := o; holder != nil; holder = holder.proto {
		if holder.flags&flagDict != 0 || depth > 255 {
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
}

// accessorIC returns the accessor e caches for receivers shaped like o, or
// nil when e is not a valid accessor entry. The accessor is read from its
// slot on every hit: redefining a getter or setter with the same attributes
// replaces the slot value without changing any shape.
func (r *Realm) accessorIC(e *ICEntry, o *Object) *Accessor {
	if o.shape == e.Shape && r.protoEpoch|1 == e.Epoch {
		return e.Holder(o).slots[e.Slot()].asAccessor()
	}
	return nil
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
	if a := r.accessorIC(e, o); a != nil {
		return r.callGetter(a, receiver)
	}
	v, err := o.Get(r, key, receiver)
	if err != nil {
		return Undefined(), err
	}
	r.fillGetIC(e, o, key)
	return v, nil
}

// getStringProp reads a named property of a string primitive and caches the
// String.prototype lookup. The entry is sound for object receivers too: a
// receiver whose shape equals String.prototype's finds the property at the
// same slot and depth.
func (r *Realm) getStringProp(v Value, key PropertyKey, e *ICEntry) (Value, error) {
	if a := r.accessorIC(e, r.StringPrototype); a != nil {
		return r.callGetter(a, v)
	}
	res, err := r.GetV(v, key)
	if err != nil {
		return Undefined(), err
	}
	r.fillGetIC(e, r.StringPrototype, key)
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
// prototype: o has no property key, and nothing on the prototype chain has
// it or could answer it (a proxy, a typed array, a pending lazy property).
// It reports whether it did. The add from o's shape that e records skips the
// checks: the epoch proves that no prototype changed since the add e
// records, and the shape that o has the same prototype and lacks key.
func (r *Realm) setNamedAdd(o *Object, key PropertyKey, v Value, e *ICEntry) bool {
	if o.flags&(flagExtensible|flagDict|flagHasLazy|flagIsPrototype|flagShared) != flagExtensible ||
		o.class == ClassArray || o.class == ClassString || o.class == ClassTypedArray || o.class == ClassProxy {
		return false
	}
	before := o.shape
	if s := e.Shape; s != nil && s.parent == before && e.Epoch == r.protoEpoch && e.HolderDepth() == icAdd && s.key == key {
		o.shape = s
		o.slots = append(o.slots, v)
		return true
	}
	if before.noFill != 0 || before.has(key) || !protoChainLacks(o.proto, key) {
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

// protoChainLacks reports whether no object on the prototype chain from p
// has key or could answer it other than with an own property: a proxy runs
// its trap, a typed array answers numeric keys, a pending lazy definition
// of key would define it.
func protoChainLacks(p *Object, key PropertyKey) bool {
	for ; p != nil; p = p.proto {
		if p.class == ClassProxy || p.class == ClassTypedArray || p.flags&flagHasLazy != 0 && p.lazyPending(key) {
			return false
		}
		if _, ok := p.getOwnCell(key); ok {
			return false
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
		if t.Epoch&1 != 0 && t.Holder(o).slots[t.Slot()].asAccessor().Set != nil {
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

// setGlobalSlow assigns to a declared global (strict mode rejects new ones).
func (r *Realm) setGlobalSlow(key PropertyKey, v Value, e *ICEntry) error {
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
	for p := g.proto; p != nil; p = p.proto {
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
			if int(i) < s.Len() {
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

// setElemSlow is the generic strict computed property write.
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
	return r.SetV(o, key, v)
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

// callValue calls fn with this and args (which may alias the register
// stack). Bytecode callees enter a new frame directly; natives are invoked
// in place. The frame of an ordinary call (every parameter passed, no rest
// parameter, arguments object or captured binding, room in the register
// stack and frame table, its inline caches bound, no interrupt pending) is
// set up here, one call fewer per JavaScript call; enterFrame takes the
// others.
func (r *Realm) callValue(fn Value, this Value, args []Value) (Value, error) {
	if !fn.IsObject() || fn.AsObject().class != ClassFunction {
		if IsCallable(fn) { // a callable proxy
			return r.CallObject(fn.AsObject(), this, args)
		}
		return Undefined(), r.calleeError(fn, "is not a function")
	}
	o := fn.AsObject()
	fd := o.internal.(*FunctionData)
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
		code := fd.code
		st := &r.interp
		base := st.sp
		top := base + int(code.NumRegs)
		np := int(code.NumParams)
		fi := st.nframes
		if !fd.plain || r.interruptFlag.Load() != 0 || fd.icBase == icUnbound || top > len(st.stack) || len(args) < np || fi == len(st.frames) {
			res, err = r.enterFrame(o, fd, this, args)
			break
		}
		regs := st.stack[base:top:top]
		dst, src := regs[:np], args[:np]
		for i := range src {
			dst[i] = src[i]
		}
		if code.Kind == bytecode.KindArrow {
			this = fd.thisValue
		}
		st.frames[fi] = frameInfo{fn: o, base: uint32(base)}
		st.nframes = fi + 1
		st.sp = top
		res, err = r.run(fi, fd, base, fd.env, this, o)
		st.sp = base
		st.nframes = fi
	case FuncNative:
		res, err = fd.native(r, this, args)
		if r.interruptFlag.Load() == flagMeasure && err == nil {
			err = r.measureAfterCall(res)
		}
	default:
		res, err = r.callOther(fd, this, args)
	}
	r.callDepth--
	return res, err
}

// constructValue implements `new fn(...args)`.
func (r *Realm) constructValue(fn Value, args []Value) (Value, error) {
	if !IsConstructor(fn) {
		return Undefined(), r.calleeError(fn, "is not a constructor")
	}
	o := fn.AsObject()
	fd := o.internal.(*FunctionData)
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

// calleeError is the TypeError of a call or `new` instruction whose callee
// fn is not callable or constructible. callValue and constructValue run only
// from those instructions, so the innermost frame is executing it and the
// message can name the callee expression as written ("o.missing is not a
// function"); without a description it shows the value.
func (r *Realm) calleeError(fn Value, what string) error {
	name := ""
	if st := &r.interp; st.nframes > 0 && bytecode.DescribeCallee != nil {
		if fr := &st.frames[st.nframes-1]; fr.fn != nil {
			name = bytecode.DescribeCallee(fr.fn.internal.(*FunctionData).code, fr.pc)
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
	n := int(o.internal.(*ArrayData).length)
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
		n := int(src.internal.(*ArrayData).length)
		for i := 0; i < n; i++ {
			e, err := arrayElementAt(r, src, i)
			if err != nil {
				return err
			}
			arr.Push(r, e)
			n = int(src.internal.(*ArrayData).length)
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

// forInNext yields the next key that is still a property of obj. keys and
// idx are the key list and the index of a for-in record, an object and a
// non-negative integer, which the bytecode verifier proves
// (bytecode.KindOperands), as run's ForInNext relies on. A key that is no
// string is skipped: the list is the array of strings ForInInit made, but
// no proof covers what it holds. Bytecode may hand the record another
// object, and a suspended generator's list stays reachable through the
// registers it left on the stack, which a later frame may read before it
// writes them.
func forInNext(keys, idx *Value, obj Value) (Value, bool) {
	if !obj.IsObject() {
		return Undefined(), false
	}
	arr := keys.AsObject()
	o := obj.AsObject()
	list := arr.elements
	for i := int(idx.AsNumber()); i < len(list); i++ {
		k := list[i]
		*idx = IntValue(i + 1)
		// Keys deleted during iteration are skipped.
		if k.IsString() && forInHas(o, keyOfString(o, k.AsString())) {
			return k, true
		}
	}
	return Undefined(), false
}

// forInHas is the check of forInNext: whether key is still a property of o
// or its chain. A proxy on the chain is taken to still have every key it
// reported (its traps ran in forInInit).
func forInHas(o *Object, key PropertyKey) bool {
	for obj := o; obj != nil; obj = obj.proto {
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
	if a, ok := s.ASCII(); ok {
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
	if source.IsNullish() {
		return nil
	}
	from, err := r.ToObject(source)
	if err != nil {
		return err
	}
	var excl []PropertyKey
	if excluded.IsObject() {
		for _, k := range excluded.AsObject().elements {
			pk, err := r.ToPropertyKey(k)
			if err != nil {
				return err
			}
			excl = append(excl, pk)
		}
	}
	if from.class == ClassProxy {
		return r.copyProxyDataProps(target, from, excl)
	}
	keys, err := r.ownPropertyKeys(from)
	if err != nil {
		return err
	}
	for _, k := range keys {
		skip := false
		for _, x := range excl {
			if x == k {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		c, ok := from.getOwnCell(k)
		if !ok || c.attrs&attrEnumerable == 0 {
			continue
		}
		v, err := from.Get(r, k, ObjectValue(from))
		if err != nil {
			return err
		}
		if err := target.CreateDataPropertyOrThrow(r, k, v); err != nil {
			return err
		}
	}
	return nil
}

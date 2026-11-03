package engine

import "github.com/MoeForge/moejs/bytecode"

// The iteration protocol (ES2025 §7.4.1-7.4.12) shared by the interpreter
// (for-of, spread, array destructuring) and the builtins (Array.from,
// Object.fromEntries, the keyed-collection constructors).
//
// An Iterator Record is two Values, so the interpreter keeps one in two
// consecutive registers and Go code in an iterRecord:
//
//	array fast mode   it = the array,  pos = IntValue(next index)
//	string fast mode  it = the string, pos = IntValue(next code unit)
//	general mode      it = the iterator object, pos = its next method
//	                  (Undefined when that is not callable)
//	done              it = Undefined ([[Done]]: never stepped or closed again)
//
// pos holds a number exactly in the fast modes. A fast mode is chosen only
// while the protocol provably behaves like the index loop (fastIterable):
// the array's @@iterator resolves to the original Array.prototype.values
// and %ArrayIteratorPrototype%.next is the original (likewise for strings),
// so creating the iterator, reading next and the {value, done} results are
// all unobservable. Closing a fast-mode record materializes the iterator
// only when a `return` method is reachable from its prototype.

// IterKind selects what an array iterator yields.
type IterKind uint8

const (
	IterValues IterKind = iota
	IterKeys
	IterEntries
)

// IteratorData is the internal payload (Object.Internal()) of
// ClassArrayIterator and ClassStringIterator objects. Target is the iterated
// array or array-like object, or the string value; Index is the next position
// (element index for arrays, code-unit index for strings); Done is set once
// exhausted (the target is then released) so further next() calls return
// {value: undefined, done: true}.
type IteratorData struct {
	Target Value
	Index  int
	Kind   IterKind
	Done   bool
}

var (
	iteratorKey = SymbolKey(SymIterator)
	nextKey     = StringKey(AtomNext)
	returnKey   = StringKey(AtomReturn)
)

// iterRecord is an Iterator Record held by Go code (see the layout above).
type iterRecord struct{ it, pos Value }

// getIterator implements GetIterator(v, sync).
func (r *Realm) getIterator(v Value) (iterRecord, error) {
	it, pos, err := r.iterInit(v)
	return iterRecord{it, pos}, err
}

// step implements IteratorStepValue.
func (ir *iterRecord) step(r *Realm) (Value, bool, error) { return r.iterStep(&ir.it, &ir.pos) }

// close implements IteratorClose with a normal completion.
func (ir *iterRecord) close(r *Realm) error { return r.iterClose(&ir.it, &ir.pos) }

// closeThrow implements IteratorClose with the throw completion err and
// returns err (or an interrupt raised by the return method).
func (ir *iterRecord) closeThrow(r *Realm, err error) error {
	if ie := r.iterCloseThrow(&ir.it, &ir.pos, err); ie != nil {
		return ie
	}
	return err
}

// fastIterable reports whether iterating v by index is indistinguishable
// from GetIterator(v): a string, or an array with no own named property
// shadowing @@iterator and Array.prototype as its prototype, while the
// original iteration methods are in place. A host slice placeholder is
// materialized first; iterating reads its elements either way.
func (r *Realm) fastIterable(v Value) bool {
	if v.IsString() {
		return r.guardHolds(&r.protoGuards[guardStringIter]) && r.guardHolds(&r.protoGuards[guardStringNext])
	}
	if !v.IsObject() {
		return false
	}
	o := v.AsObject()
	if o.class != ClassArray || o.proto != r.ArrayPrototype {
		return false
	}
	if o.flags&(flagDict|flagHasLazy) != 0 && (!o.materializeHost() || o.flags&(flagDict|flagHasLazy) != 0) {
		return false
	}
	if o.shape.count != 0 {
		if _, _, ok := o.shape.Lookup(iteratorKey); ok {
			return false
		}
	}
	return r.guardHolds(&r.protoGuards[guardArrayIter]) && r.guardHolds(&r.protoGuards[guardArrayNext])
}

// iterInit implements GetIterator(v, sync) into a record.
func (r *Realm) iterInit(v Value) (it, pos Value, err error) {
	if r.fastIterable(v) {
		return v, IntValue(0), nil
	}
	if v.IsNullish() {
		return Undefined(), Undefined(), r.TypeError("%s is not iterable", v.String())
	}
	m, err := r.GetMethod(v, iteratorKey)
	if err != nil {
		return Undefined(), Undefined(), err
	}
	if m.IsUndefined() {
		return Undefined(), Undefined(), r.TypeError("%s is not iterable", r.DisplayString(v))
	}
	return r.iterFromMethod(v, m)
}

// iterFromMethod implements GetIteratorFromMethod(v, m): m is callable.
// The original array and string iterator methods yield a fast-mode record
// while the original next methods are in place.
func (r *Realm) iterFromMethod(v, m Value) (it, pos Value, err error) {
	switch fn := m.AsObject(); {
	case fn == r.arrayValuesFn && v.IsObject() && v.AsObject().class == ClassArray:
		if r.guardHolds(&r.protoGuards[guardArrayNext]) {
			return v, IntValue(0), nil
		}
	case fn == r.stringIterFn && v.IsString():
		if r.guardHolds(&r.protoGuards[guardStringNext]) {
			return v, IntValue(0), nil
		}
	}
	it, err = r.Call(m, v, nil)
	if err != nil {
		return Undefined(), Undefined(), err
	}
	if !it.IsObject() {
		return Undefined(), Undefined(), r.TypeError("Result of the Symbol.iterator method is not an object")
	}
	next, err := it.AsObject().Get(r, nextKey, it)
	if err != nil {
		return Undefined(), Undefined(), err
	}
	if !IsCallable(next) {
		next = Undefined()
	}
	return it, next, nil
}

// iterStep implements IteratorStepValue on the record (*it, *pos). *it
// becomes Undefined once the iterator is exhausted or its next method (or
// reading the result) threw, as the spec sets [[Done]]. Callers inside the
// interpreter pass copies of the registers: next may grow the register
// stack.
func (r *Realm) iterStep(it, pos *Value) (Value, bool, error) {
	v := *it
	if !v.IsObject() {
		if v.IsString() && pos.IsNumber() {
			s := v.AsString()
			i := int(pos.AsNumber())
			if i >= s.Len() {
				*it = Undefined()
				return Undefined(), true, nil
			}
			cp, n := stringCodePointAt(s, i)
			*pos = IntValue(i + n)
			return cp, false, nil
		}
		return Undefined(), true, nil
	}
	o := v.AsObject()
	if pos.IsNumber() {
		i := int(pos.AsNumber())
		if i >= int(o.internal.(*ArrayData).length) {
			*it = Undefined()
			return Undefined(), true, nil
		}
		*pos = IntValue(i + 1)
		e, err := arrayElementAt(r, o, i)
		if err != nil {
			*it = Undefined()
		}
		return e, false, err
	}
	if !pos.IsObject() {
		*it = Undefined()
		return Undefined(), false, r.TypeError("iterator.next is not a function")
	}
	next := pos.AsObject()
	// The original next of the engine's iterators steps their payload
	// directly: the {value, done} object it would return is unobservable.
	switch d := o.internal.(type) {
	case *IteratorData:
		if next == r.arrayIterNextFn && o.class == ClassArrayIterator || next == r.stringIterNextFn && o.class == ClassStringIterator {
			e, done, err := d.Next(r)
			if done || err != nil {
				*it = Undefined()
			}
			return e, done, err
		}
	case *collIterData:
		if next == r.mapIterNextFn && o.class == ClassMapIterator || next == r.setIterNextFn && o.class == ClassSetIterator {
			e, done := d.step(r, o.class == ClassSetIterator)
			if done {
				*it = Undefined()
			}
			return e, done, nil
		}
	case *intlSegments:
		if x := r.intl; x != nil && next == x.segmentIterNextFn && o.class == ClassIntlSegmentIterator {
			e, done := d.step(r)
			if done {
				*it = Undefined()
			}
			return e, done, nil
		}
	}
	res, err := r.CallObject(next, v, nil)
	if err != nil {
		*it = Undefined()
		return Undefined(), false, err
	}
	if !res.IsObject() {
		*it = Undefined()
		return Undefined(), false, r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
	}
	ro := res.AsObject()
	done, err := ro.Get(r, StringKey(AtomDone), res)
	if err != nil {
		*it = Undefined()
		return Undefined(), false, err
	}
	if ToBoolean(done) {
		*it = Undefined()
		return Undefined(), true, nil
	}
	e, err := ro.Get(r, StringKey(AtomValue), res)
	if err != nil {
		*it = Undefined()
	}
	return e, false, err
}

// iterRest collects the remaining values of the record (a rest element).
func (r *Realm) iterRest(it, pos *Value) (Value, error) {
	var items []Value
	if v := *it; v.IsObject() && pos.IsNumber() {
		if n := int(v.AsObject().internal.(*ArrayData).length) - int(pos.AsNumber()); n > 0 {
			items = make([]Value, 0, n)
		}
	}
	for k := int64(0); ; k++ {
		if err := interruptEvery(r, k); err != nil {
			return Undefined(), err
		}
		v, done, err := r.iterStep(it, pos)
		if err != nil {
			return Undefined(), err
		}
		if done {
			return ObjectValue(r.NewArrayFromSlice(items)), nil
		}
		items = append(items, v)
	}
}

// closeTarget returns the iterator object whose `return` method closing the
// record (it, pos) must look up, or nil when there is nothing to call: the
// record is done, or it is a fast-mode record and no `return` is reachable
// from the iterator prototype, the only way the virtual iterator could
// have one (never in a shared realm: that chain is frozen without one).
func (r *Realm) closeTarget(it, pos Value) *Object {
	if !pos.IsNumber() {
		if it.IsObject() {
			return it.AsObject()
		}
		return nil
	}
	if r.sharedIntrinsics {
		return nil
	}
	switch {
	case it.IsObject():
		if r.lacksWellKnown(r.ArrayIteratorPrototype, returnKey) {
			return nil
		}
		o := r.newArrayIterator(it.AsObject(), IterValues)
		o.internal.(*IteratorData).Index = int(pos.AsNumber())
		return o
	case it.IsString():
		if r.lacksWellKnown(r.StringIteratorPrototype, returnKey) {
			return nil
		}
		o := r.newStringIterator(it.AsString())
		o.internal.(*IteratorData).Index = int(pos.AsNumber())
		return o
	}
	return nil
}

// iterClose implements IteratorClose(record, normal completion): the
// return method's errors propagate and a non-object result is a TypeError.
func (r *Realm) iterClose(it, pos *Value) error {
	o := r.closeTarget(*it, *pos)
	*it = Undefined()
	if o == nil {
		return nil
	}
	m, err := r.GetMethod(ObjectValue(o), returnKey)
	if err != nil || m.IsUndefined() {
		return err
	}
	res, err := r.Call(m, ObjectValue(o), nil)
	if err != nil {
		return err
	}
	if !res.IsObject() {
		return r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
	}
	return nil
}

// iterCloseThrow implements IteratorClose(record, throw completion err):
// the return method runs but whatever it throws or returns is dropped, so
// the caller rethrows err. It returns non-nil only for an interrupt, which
// wins over everything and skips the return method when err is one.
func (r *Realm) iterCloseThrow(it, pos *Value, err error) error {
	if ie, ok := err.(*InterruptedError); ok {
		*it = Undefined()
		return ie
	}
	o := r.closeTarget(*it, *pos)
	*it = Undefined()
	if o == nil {
		return nil
	}
	m, merr := r.GetMethod(ObjectValue(o), returnKey)
	if merr == nil && !m.IsUndefined() {
		_, merr = r.Call(m, ObjectValue(o), nil)
	}
	if ie, ok := merr.(*InterruptedError); ok {
		return ie
	}
	return nil
}

// iterOp executes an iteration op of the frame whose registers start at
// base, past the fast paths the interpreter runs inline, and returns the
// next pc (IterNext has its own, iterNext). Keeping these paths out of the
// dispatch loop keeps it below the compiler's big-function size, so its
// small helpers stay inlined. For IterThrow a nil error means the record is
// closed and the caller throws.
func (r *Realm) iterOp(fd *FunctionData, base int, w uint32, pc int) (int, error) {
	st := &r.interp
	top := base + int(fd.code.NumRegs)
	regs := st.stack[base:top:top]
	a := int(uint8(w >> 8))
	// The record registers are copied: next and return may grow the stack.
	it, pos := regs[a], regs[a+1]
	var v Value
	var err error
	switch bytecode.Op(w) {
	case bytecode.IterInit:
		if it, pos, err = r.iterInit(regs[uint8(w>>16)]); err == nil {
			regs = st.stack[base:top:top]
			regs[a], regs[a+1] = it, pos
		}
		return pc, err
	case bytecode.IterValue, bytecode.IterRest:
		if bytecode.Op(w) == bytecode.IterValue {
			v, _, err = r.iterStep(&it, &pos)
		} else {
			v, err = r.iterRest(&it, &pos)
		}
		regs = st.stack[base:top:top]
		regs[a], regs[a+1] = it, pos
		if err == nil {
			regs[uint8(w>>16)] = v
		}
		return pc, err
	case bytecode.IterClose:
		// The record is marked done before `return` runs: a throw from
		// it reaches the handler that would otherwise close it again.
		// A fast-mode record of a shared realm has no return method.
		if !it.IsUndefined() && !(r.sharedIntrinsics && pos.IsNumber()) {
			regs[a] = Undefined()
			if err = r.iterClose(&it, &pos); err != nil {
				return pc, err
			}
		}
		return pc + int(int16(w>>16)), nil
	case bytecode.IterThrow:
		if !it.IsUndefined() {
			regs[a] = Undefined()
			return pc, r.iterCloseThrow(&it, &pos, nil)
		}
	}
	return pc, nil
}

// iterNext is iterOp for IterNext outside an array's fast mode. It steps a
// segment iterator itself: that runs no JavaScript, so the record stays in
// its registers, and the step costs its checks and the segment.
func (r *Realm) iterNext(fd *FunctionData, base int, w uint32, pc int) (int, error) {
	st := &r.interp
	top := base + int(fd.code.NumRegs)
	regs := st.stack[base:top:top]
	a := int(uint8(w >> 8))
	if sg := r.segmentIterator(regs[a], regs[a+1]); sg != nil {
		v, done := sg.step(r)
		if done {
			regs[a] = Undefined()
			return pc + int(int16(w>>16)), nil
		}
		regs[a+2] = v
		return pc, nil
	}
	// The record registers are copied: next may grow the stack.
	it, pos := regs[a], regs[a+1]
	v, done, err := r.iterStep(&it, &pos)
	regs = st.stack[base:top:top]
	regs[a], regs[a+1] = it, pos
	if err != nil {
		return pc, err
	}
	if done {
		return pc + int(int16(w>>16)), nil
	}
	regs[a+2] = v
	return pc, nil
}

// Next advances an engine iterator: arrays yield values, keys or [key,
// value] pairs according to Kind, reading the live length each step and
// holes as undefined; strings yield code points as strings. Array-likes
// (any other object target) are iterated through Get(length) and
// Get(index). A throwing step completes the iterator, as it completes the
// generator of the spec's CreateIteratorFromClosure.
func (d *IteratorData) Next(r *Realm) (Value, bool, error) {
	if d.Done {
		return Undefined(), true, nil
	}
	t := d.Target
	if t.IsString() {
		s := t.AsString()
		i := d.Index
		if i >= s.Len() {
			d.finish()
			return Undefined(), true, nil
		}
		v, n := stringCodePointAt(s, i)
		d.Index = i + n
		return v, false, nil
	}
	o := t.AsObject()
	i := d.Index
	var length int
	switch o.class {
	case ClassArray:
		length = int(o.internal.(*ArrayData).length)
	case ClassTypedArray:
		if length = o.internal.(*typedArray).length(); length < 0 {
			d.finish()
			return Undefined(), false, r.TypeError("Cannot iterate a detached or out-of-bounds TypedArray")
		}
	default:
		n, err := r.LengthOfArrayLike(o)
		if err != nil {
			d.finish()
			return Undefined(), false, err
		}
		length = int(n)
	}
	if i >= length {
		d.finish()
		return Undefined(), true, nil
	}
	d.Index = i + 1
	if d.Kind == IterKeys {
		return IntValue(i), false, nil
	}
	v, err := arrayElementAt(r, o, i)
	if err != nil {
		d.finish()
		return Undefined(), false, err
	}
	if d.Kind == IterEntries {
		return ObjectValue(r.NewArray(IntValue(i), v)), false, nil
	}
	return v, false, nil
}

// finish marks the iterator exhausted and releases its target.
func (d *IteratorData) finish() {
	d.Done = true
	d.Target = Undefined()
}

// arrayElementAt reads element i of o: the dense fast path reads holes as
// undefined, everything else goes through [[Get]].
func arrayElementAt(r *Realm, o *Object, i int) (Value, error) {
	if i < len(o.elements) && o.class != ClassString {
		if v := o.elements[i]; !v.IsHole() {
			return v, nil
		}
		if o.dict == nil || o.dict.sparse == nil {
			if !chainMayHave(o.proto, IndexKey(uint32(i))) {
				return Undefined(), nil
			}
		}
	}
	return o.Get(r, IndexKey(uint32(i)), ObjectValue(o))
}

// stringCodePointAt returns the code point starting at code unit i as a
// one- or two-unit string and the number of units consumed.
func stringCodePointAt(s *String, i int) (Value, int) {
	c := s.At(i)
	if c >= 0xD800 && c < 0xDC00 && i+1 < s.Len() {
		if d := s.At(i + 1); d >= 0xDC00 && d < 0xE000 {
			return StringValue(s.Substring(i, i+2)), 2
		}
	}
	return StringValue(s.Substring(i, i+1)), 1
}

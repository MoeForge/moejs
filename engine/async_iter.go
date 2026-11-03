package engine

import (
	"github.com/MoeForge/moejs/bytecode"
	"unsafe"
)

// Async iteration: for await and an async generator's yield* step their
// iterator with ops that call, then check the awaited result, the await
// between them compiled as an ordinary Await. GetIterator(v, async) wraps a
// sync iterator in an async-from-sync iterator (§27.1.6), whose methods
// return promises of the sync results with their values awaited.

var (
	asyncIteratorKey = SymbolKey(SymAsyncIterator)
	throwKey         = StringKey(AtomThrow)
)

// The phase of an async yield* step, kept in the record's third register
// between the call (AsyncDelegate C=0) and the result check (C=1).
const (
	delegateNext = iota
	delegateThrow
	delegateReturn
	// delegateClose is the return call closing an iterator that has no
	// throw method: its result is checked, then the protocol violation
	// throws.
	delegateClose
)

// asyncIterOp executes one async iteration op (asyncOp's default case) of
// the frame whose registers start at base and returns the next pc.
func (r *Realm) asyncIterOp(fd *FunctionData, base int, w uint32, pc int) (int, error) {
	st := &r.interp
	top := base + int(fd.code.NumRegs)
	regs := st.stack[base:top:top]
	a, b := int(uint8(w>>8)), int(uint8(w>>16))
	switch bytecode.Op(w) {
	case bytecode.AsyncIterInit:
		it, next, err := r.asyncIterator(regs[b])
		regs = st.stack[base:top:top]
		regs[a], regs[a+1] = it, next
		return pc, err
	case bytecode.AsyncIterNext:
		res, err := r.Call(regs[a+1], regs[a], nil)
		regs = st.stack[base:top:top]
		regs[a+2] = res
		return pc, err
	case bytecode.AsyncIterResult:
		res := regs[a+2]
		if !res.IsObject() {
			return pc, r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
		}
		if v, done, ok := r.iterResultFields(res.AsObject()); ok {
			if ToBoolean(done) {
				return pc + int(int16(w>>16)), nil
			}
			regs[a+2] = v
			return pc, nil
		}
		done, err := res.AsObject().Get(r, StringKey(AtomDone), res)
		if err != nil {
			return pc, err
		}
		if ToBoolean(done) {
			return pc + int(int16(w>>16)), nil
		}
		v, err := res.AsObject().Get(r, StringKey(AtomValue), res)
		regs = st.stack[base:top:top]
		regs[a+2] = v
		return pc, err
	case bytecode.AsyncIterClose:
		it := regs[a]
		if it.IsUndefined() {
			return pc + int(int16(w>>16)), nil
		}
		regs[a] = Undefined()
		m, err := r.GetMethod(it, returnKey)
		if err != nil {
			return pc, err
		}
		if m.IsUndefined() {
			return pc + int(int16(w>>16)), nil
		}
		res, err := r.Call(m, it, nil)
		regs = st.stack[base:top:top]
		regs[a+2] = res
		return pc, err
	case bytecode.AsyncIterClosed:
		if res := regs[a+2]; !res.IsObject() {
			return pc, r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
		}
	case bytecode.AsyncDelegate:
		var v, outcome Value
		var err error
		if w>>24 == 0 {
			var phase int
			v, outcome, phase, err = r.asyncDelegateCall(regs[b], regs[b+1], regs[a], regs[a+1])
			regs = st.stack[base:top:top]
			regs[b+2] = IntValue(phase)
		} else {
			v, outcome, err = r.asyncDelegateResult(int(regs[b+2].AsNumber()), regs[a])
			regs = st.stack[base:top:top]
		}
		regs[a], regs[a+1] = v, outcome
		return pc, err
	default:
		return pc, r.TypeError("internal: unknown opcode")
	}
	return pc, nil
}

// asyncIterator implements GetIterator(v, async): the iterator and its next
// method.
func (r *Realm) asyncIterator(v Value) (it, next Value, err error) {
	if v.IsNullish() {
		return Undefined(), Undefined(), r.TypeError("%s is not async iterable", v.String())
	}
	m, err := r.GetMethod(v, asyncIteratorKey)
	if err != nil {
		return Undefined(), Undefined(), err
	}
	if m.IsUndefined() {
		if it, next, err = r.delegateIterator(v); err != nil {
			return Undefined(), Undefined(), err
		}
		return r.newAsyncFromSyncIterator(it, next), ObjectValue(r.asyncIntr().asyncFromSyncNext), nil
	}
	if it, err = r.Call(m, v, nil); err != nil {
		return Undefined(), Undefined(), err
	}
	if !it.IsObject() {
		return Undefined(), Undefined(), r.TypeError("Result of the Symbol.asyncIterator method is not an object")
	}
	next, err = it.AsObject().Get(r, nextKey, it)
	return it, next, err
}

// asyncDelegateCall calls the inner iterator's method for the resumption
// v, mode of an async yield* (§15.5.5 step 7, generatorKind async). It
// returns the result to await with outcome undefined and the phase of the
// step, or, when the iterator has no return method for a return
// resumption, the value to return with outcome false.
func (r *Realm) asyncDelegateCall(it, next, v, mode Value) (Value, Value, int, error) {
	args := []Value{v}
	phase := delegateNext
	var m Value
	var err error
	switch {
	case mode.IsUndefined():
		m = next
	case mode.AsBool():
		phase = delegateThrow
		if m, err = r.GetMethod(it, throwKey); err != nil {
			return Undefined(), Undefined(), phase, err
		}
		if m.IsUndefined() {
			// The protocol is violated: close the iterator
			// (AsyncIteratorClose with a normal completion), then throw.
			if m, err = r.GetMethod(it, returnKey); err != nil {
				return Undefined(), Undefined(), phase, err
			}
			if m.IsUndefined() {
				return Undefined(), Undefined(), phase, r.TypeError("The iterator does not provide a 'throw' method")
			}
			phase, args = delegateClose, nil
		}
	default:
		phase = delegateReturn
		if m, err = r.GetMethod(it, returnKey); err != nil {
			return Undefined(), Undefined(), phase, err
		}
		if m.IsUndefined() {
			return v, False(), phase, nil
		}
	}
	res, err := r.Call(m, it, args)
	return res, Undefined(), phase, err
}

// asyncDelegateResult checks the awaited result res of an async yield*
// step of the given phase and returns its value and the outcome: undefined
// to yield the value, true when the delegation is done with it, false when
// the generator returns it.
func (r *Realm) asyncDelegateResult(phase int, res Value) (Value, Value, error) {
	if !res.IsObject() {
		return Undefined(), Undefined(), r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
	}
	if phase == delegateClose {
		return Undefined(), Undefined(), r.TypeError("The iterator does not provide a 'throw' method")
	}
	ro := res.AsObject()
	done, err := ro.Get(r, StringKey(AtomDone), res)
	if err != nil {
		return Undefined(), Undefined(), err
	}
	v, err := ro.Get(r, StringKey(AtomValue), res)
	if err != nil || !ToBoolean(done) {
		return v, Undefined(), err
	}
	if phase == delegateReturn {
		return v, False(), nil
	}
	return v, True(), nil
}

// --- async-from-sync iterators ------------------------------------------------

// asyncFromSyncIter is the internal of an async-from-sync iterator object:
// its [[SyncIteratorRecord]]. The object is reachable from the iteration
// machinery only.
type asyncFromSyncIter struct {
	obj      Object
	it, next Value
}

// newAsyncFromSyncIterator implements CreateAsyncFromSyncIterator for the
// sync iterator it with next method next.
func (r *Realm) newAsyncFromSyncIterator(it, next Value) Value {
	w := &asyncFromSyncIter{it: it, next: next}
	r.chargeObject(unsafe.Sizeof(asyncFromSyncIter{}))
	proto := r.asyncIntr().AsyncFromSyncIteratorPrototype
	o := initObject(&w.obj, ClassObject, r.rootShapeFor(proto))
	o.internal = w
	return ObjectValue(o)
}

// asyncFromSyncThis returns the record of the async-from-sync iterator
// this. The methods are unreachable from programs the compiler made, but
// bytecode that hands the iterator (an AsyncIterInit's record) to a
// program can call them on another this, which is an internal error.
func (r *Realm) asyncFromSyncThis(this Value) (*asyncFromSyncIter, error) {
	if this.IsObject() {
		if s, ok := this.AsObject().internal.(*asyncFromSyncIter); ok {
			return s, nil
		}
	}
	return nil, r.TypeError("internal: an async-from-sync iterator method called on another object")
}

// asyncFromSyncNext implements %AsyncFromSyncIteratorPrototype%.next(value).
func asyncFromSyncNext(r *Realm, this Value, args []Value) (Value, error) {
	s, err := r.asyncFromSyncThis(this)
	if err != nil {
		return Undefined(), err
	}
	p := r.newPromise()
	res, err := r.Call(s.next, s.it, args[:min(len(args), 1)])
	if err == nil && !res.IsObject() {
		err = r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
	}
	if err != nil {
		return r.rejectWith(p, err)
	}
	return r.asyncFromSyncContinue(s, res, p, true)
}

// asyncFromSyncReturn implements %AsyncFromSyncIteratorPrototype%.return(value).
func asyncFromSyncReturn(r *Realm, this Value, args []Value) (Value, error) {
	s, err := r.asyncFromSyncThis(this)
	if err != nil {
		return Undefined(), err
	}
	p := r.newPromise()
	m, err := r.GetMethod(s.it, returnKey)
	if err != nil {
		return r.rejectWith(p, err)
	}
	if m.IsUndefined() {
		return ObjectValue(p), r.resolvePromiseErr(p, r.createIterResult(Arg(args, 0), true))
	}
	res, err := r.Call(m, s.it, args[:min(len(args), 1)])
	if err == nil && !res.IsObject() {
		err = r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
	}
	if err != nil {
		return r.rejectWith(p, err)
	}
	return r.asyncFromSyncContinue(s, res, p, false)
}

// asyncFromSyncThrow implements %AsyncFromSyncIteratorPrototype%.throw(value).
func asyncFromSyncThrow(r *Realm, this Value, args []Value) (Value, error) {
	s, err := r.asyncFromSyncThis(this)
	if err != nil {
		return Undefined(), err
	}
	p := r.newPromise()
	m, err := r.GetMethod(s.it, throwKey)
	if err != nil {
		return r.rejectWith(p, err)
	}
	if m.IsUndefined() {
		// Close the iterator, then reject for the protocol violation.
		rec := iterRecord{s.it, s.next}
		if err := rec.close(r); err != nil {
			return r.rejectWith(p, err)
		}
		return r.rejectWith(p, r.TypeError("The iterator does not provide a 'throw' method"))
	}
	res, err := r.Call(m, s.it, args[:min(len(args), 1)])
	if err == nil && !res.IsObject() {
		err = r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
	}
	if err != nil {
		return r.rejectWith(p, err)
	}
	return r.asyncFromSyncContinue(s, res, p, true)
}

// asyncFromSyncContinue implements AsyncFromSyncIteratorContinuation: p
// settles with the sync result res, its value awaited. With
// closeOnRejection, the sync iterator is closed when the value is a
// rejected promise (or cannot be resolved to one) and not done.
func (r *Realm) asyncFromSyncContinue(s *asyncFromSyncIter, res Value, p *Object, closeOnRejection bool) (Value, error) {
	ro := res.AsObject()
	dv, err := ro.Get(r, StringKey(AtomDone), res)
	if err != nil {
		return r.rejectWith(p, err)
	}
	done := ToBoolean(dv)
	v, err := ro.Get(r, StringKey(AtomValue), res)
	if err != nil {
		return r.rejectWith(p, err)
	}
	closing := closeOnRejection && !done
	// The value wrapper is PromiseResolve's as an Await's (awaitTarget):
	// none for a value that is no thenable, whose step is queued at once.
	wrapper, err := r.awaitTarget(v)
	if err != nil {
		if closing {
			if ie := r.asyncFromSyncClose(s, err); ie != nil {
				return Undefined(), ie
			}
		}
		return r.rejectWith(p, err)
	}
	x := &asyncFromSyncStep{sync: s, promise: p, done: done, close: closing}
	x.re.reactor = x
	if wrapper != nil {
		r.performPromiseThen(wrapper, &x.re)
	} else {
		r.enqueue(job{reaction: &x.re, arg: v})
	}
	return ObjectValue(p), nil
}

// asyncFromSyncClose closes the sync iterator of s with the throw
// completion err (IteratorClose), dropping what closing throws; only an
// interrupt is returned.
func (r *Realm) asyncFromSyncClose(s *asyncFromSyncIter, err error) error {
	rec := iterRecord{s.it, s.next}
	if ce := rec.closeThrow(r, err); ce != err {
		return ce
	}
	return nil
}

// asyncFromSyncStep reacts to the awaited value of a sync result through
// the reaction it starts with: the promise p of the async-from-sync method
// settles with it.
type asyncFromSyncStep struct {
	re          promiseReaction
	sync        *asyncFromSyncIter
	promise     *Object
	done, close bool
}

func (x *asyncFromSyncStep) promiseSettled(r *Realm, v Value, rejected bool) error {
	if !rejected {
		return r.resolvePromiseErr(x.promise, r.createIterResult(v, x.done))
	}
	if x.close {
		if err := r.asyncFromSyncClose(x.sync, r.Throw(v)); err != nil {
			return err
		}
	}
	r.rejectPromise(x.promise, v)
	return nil
}

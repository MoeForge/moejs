package engine

import "unsafe"

// Async generators (ECMA-262 §27.6) combine the two suspensions: the frame
// suspends at AsyncGenStart and AsyncYield the way a generator's does at
// GenStart and Yield, and at Await the way an async function's does. The
// requests (next, return, throw) queue on the generator with the promise
// each returns. A request resumes a suspended generator; AsyncYield and the
// completion of the body settle the head request (AsyncGeneratorCompleteStep),
// and a yield that finds another request queued continues with it without
// suspending. Once the body has completed, the generator drains the queue,
// awaiting the value of each return request (AsyncGeneratorAwaitReturn) with
// the generator itself as the reactor.
//
// The body has no catch-all handler: what it throws comes out of run and
// rejects the head request. An interrupt completes the generator and drops
// its queue, leaving the queued promises pending, as it leaves an async
// function's promise.

// asyncGenerator is the internal payload of a ClassAsyncGenerator object:
// the frame, the request queue (AsyncGeneratorRequest records, head first)
// and the reaction record of the frame's awaits (async.go).
type asyncGenerator struct {
	obj   Object
	g     genFrame
	queue []asyncGenRequest
	re    promiseReaction
}

// asyncGenRequest is an AsyncGeneratorRequest: the completion (value and
// mode, as a resumption carries them: undefined next, true throw, false
// return) and the promise of the call that queued it.
type asyncGenRequest struct {
	value, mode Value
	promise     *Object
}

// newAsyncGenerator creates the async generator of a call to fn with `this`
// this: OrdinaryCreateFromConstructor(fn, %AsyncGeneratorPrototype%).
func (r *Realm) newAsyncGenerator(fn *Object, fd *FunctionData, this Value) (*Object, error) {
	pv, err := fn.GetProp(r, StringKey(AtomPrototype))
	if err != nil {
		return nil, err
	}
	proto := r.asyncIntr().AsyncGeneratorPrototype
	if pv.IsObject() {
		proto = pv.AsObject()
	}
	r.markPrototype(proto)
	ag := &asyncGenerator{g: genFrame{fn: fn, fd: fd, this: this, regs: make([]Value, fd.code.NumRegs)}}
	ag.re.reactor = ag
	r.chargeObject(unsafe.Sizeof(asyncGenerator{}) + uintptr(len(ag.g.regs)*valueSize))
	o := initObject(&ag.obj, ClassAsyncGenerator, r.rootShapeFor(proto))
	o.internal = ag
	return o, nil
}

// agResume implements AsyncGeneratorResume: the suspended frame continues
// with the completion v, mode.
func (r *Realm) agResume(ag *asyncGenerator, v, mode Value) error {
	res, err := r.resumeFrame(&ag.g, v, mode)
	return r.agResumed(ag, res, err)
}

// agResumed completes a resumption of ag's frame that returned res, err:
// unless the frame suspended again, the body completed (the rest of
// AsyncGeneratorStart's closure), which settles the head request and
// drains the queue.
func (r *Realm) agResumed(ag *asyncGenerator, res Value, err error) error {
	g := &ag.g
	if err == nil && g.state != genExecuting {
		return nil // suspended at a yield or an await
	}
	throw := false
	if err != nil {
		v, ok := r.thrownValue(err)
		if !ok {
			r.agAbort(ag)
			return err
		}
		res, throw = v, true
	}
	g.finish()
	g.state = genDrainingQueue
	if err := r.agCompleteStep(ag, res, throw, true); err != nil {
		r.agAbort(ag)
		return err
	}
	return r.agDrainQueue(ag)
}

// agAbort completes ag after an interrupt, dropping its queue.
func (r *Realm) agAbort(ag *asyncGenerator) {
	ag.g.finish()
	ag.queue = nil
}

// agCompleteStep implements AsyncGeneratorCompleteStep: the head request is
// removed and its promise settled with v, rejected if throw, else fulfilled
// with {value: v, done}. The resolution reads the result's `then`, which
// can run code; only an interrupt is returned.
func (r *Realm) agCompleteStep(ag *asyncGenerator, v Value, throw, done bool) error {
	if len(ag.queue) == 0 {
		return nil
	}
	p := ag.queue[0].promise
	n := copy(ag.queue, ag.queue[1:])
	ag.queue[n] = asyncGenRequest{}
	ag.queue = ag.queue[:n]
	if throw {
		r.rejectPromise(p, v)
		return nil
	}
	return r.resolvePromiseErr(p, r.createIterResult(v, done))
}

// agDrainQueue implements AsyncGeneratorDrainQueue for ag, whose body has
// completed: the requests settle in order, next with {undefined, done: true}
// and throw with its value, until a return request awaits its value
// (AsyncGeneratorAwaitReturn, resumed in promiseSettled).
func (r *Realm) agDrainQueue(ag *asyncGenerator) error {
	for len(ag.queue) > 0 {
		req := &ag.queue[0]
		var err error
		switch {
		case req.mode.IsUndefined():
			err = r.agCompleteStep(ag, Undefined(), false, true)
		case req.mode.AsBool():
			err = r.agCompleteStep(ag, req.value, true, true)
		default:
			var p *Object
			if p, err = r.promiseResolve(req.value); err == nil {
				r.promiseReact(p, ag)
				return nil
			}
			v, ok := r.thrownValue(err)
			if !ok {
				break
			}
			err = r.agCompleteStep(ag, v, true, true)
		}
		if err != nil {
			r.agAbort(ag)
			return err
		}
	}
	ag.g.state = genCompleted
	return nil
}

// promiseSettled resumes ag's frame awaiting the settled promise, or
// completes the return request whose value the draining generator awaited.
func (ag *asyncGenerator) promiseSettled(r *Realm, v Value, rejected bool) error {
	switch ag.g.state {
	case genDrainingQueue:
		if err := r.agCompleteStep(ag, v, rejected, true); err != nil {
			r.agAbort(ag)
			return err
		}
		return r.agDrainQueue(ag)
	case genAwaiting:
		res, err := r.resumeAwait(&ag.g, v, rejected)
		return r.agResumed(ag, res, err)
	}
	return nil
}

// asyncGenEnqueue implements the methods of %AsyncGeneratorPrototype%
// (mode undefined: next, true: throw, false: return) with the value v on
// this: the request is settled at once or queued, and resumes a suspended
// generator.
func (r *Realm) asyncGenEnqueue(this, v, mode Value, method string) (Value, error) {
	p := r.newPromise()
	var ag *asyncGenerator
	if this.IsObject() && this.AsObject().class == ClassAsyncGenerator {
		ag = this.AsObject().internal.(*asyncGenerator)
	}
	if ag == nil {
		return r.rejectWith(p, r.TypeError("AsyncGenerator.prototype.%s called on incompatible receiver %s", method, r.DisplayString(this)))
	}
	state := ag.g.state
	switch {
	case mode.IsUndefined():
		if state == genCompleted {
			return ObjectValue(p), r.resolvePromiseErr(p, r.createIterResult(Undefined(), true))
		}
	case mode.AsBool():
		if state == genSuspendedStart {
			ag.g.finish()
			state = genCompleted
		}
		if state == genCompleted {
			r.rejectPromise(p, v)
			return ObjectValue(p), nil
		}
	}
	ag.queue = append(ag.queue, asyncGenRequest{value: v, mode: mode, promise: p})
	var err error
	switch {
	case state == genSuspendedYield, state == genSuspendedStart && mode.IsUndefined():
		err = r.agResume(ag, v, mode)
	case state == genSuspendedStart, state == genCompleted:
		// A return request: the body never runs again.
		ag.g.finish()
		ag.g.state = genDrainingQueue
		err = r.agDrainQueue(ag)
	}
	return ObjectValue(p), err
}

// rejectWith rejects p with the value err throws and returns p
// (IfAbruptRejectPromise); an interrupt is returned instead.
func (r *Realm) rejectWith(p *Object, err error) (Value, error) {
	v, ok := r.thrownValue(err)
	if !ok {
		return Undefined(), err
	}
	r.rejectPromise(p, v)
	return ObjectValue(p), nil
}

// asyncGeneratorNext implements %AsyncGeneratorPrototype%.next(value).
func asyncGeneratorNext(r *Realm, this Value, args []Value) (Value, error) {
	return r.asyncGenEnqueue(this, Arg(args, 0), Undefined(), "next")
}

// asyncGeneratorReturn implements %AsyncGeneratorPrototype%.return(value).
func asyncGeneratorReturn(r *Realm, this Value, args []Value) (Value, error) {
	return r.asyncGenEnqueue(this, Arg(args, 0), False(), "return")
}

// asyncGeneratorThrow implements %AsyncGeneratorPrototype%.throw(exception).
func asyncGeneratorThrow(r *Realm, this Value, args []Value) (Value, error) {
	return r.asyncGenEnqueue(this, Arg(args, 0), True(), "throw")
}

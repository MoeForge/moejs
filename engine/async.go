package engine

import (
	"github.com/MoeForge/moejs/bytecode"
	"unsafe"
)

// Async functions (ECMA-262 §27.7) suspend and resume the way generators do
// (generator.go): the frame's register window is copied out at an Await and
// back when the awaited promise settles, and no goroutine or Go stack is
// kept per call. A call runs the body from a LoadThis of its this to an
// AsyncReturn that resolves the call's promise or, from a
// catch-all handler, an AsyncThrow that rejects it (asyncEnd); both leave
// the promise for the Ret after them, which the caller receives when the
// body completes without awaiting. Such a call makes no state, and its
// promise only there: no code could reach the promise before. The call's
// first Await makes the promise and the state, which holds the suspended
// frame, and leaves the state in the register that held the call's this
// (asyncFrameValue): a value no JavaScript code holds, by which the Awaits
// and the AsyncReturn after it know the frame for the call's, whatever
// another call's this is.
//
// Await resolves its operand to a promise and reacts to it with the state
// itself (a promiseReactor), through the reaction record the state holds:
// the frame awaits one promise at a time, so the record is free again when
// its job runs, and an await after the first allocates nothing. An operand
// that is no thenable makes no promise either: the promise PromiseResolve
// would create is fulfilled with it at once, so the reaction job is queued
// with the operand, and no code can reach that promise (awaitTarget). The
// frame is saved into the state and run returns the promise to the caller.
// The reaction job resumes the frame with the result in R[A] and the
// outcome in R[A+1] (undefined: fulfilled, true: rejected), and the code
// after Await throws a rejection where the await stands. A resumed frame
// runs with the call depth raised, as a call from the job would.
//
// A frame that the running job resumed continues past an Await without
// suspending when the job of its reaction would be the next to run
// (awaitNext): the drain would resume it with nothing run in between.

// asyncFrame is the state of an async function call that awaited: its
// suspended frame, the reaction record of its awaits and the promise the
// call returned, allocated with the frame's registers (newAsyncFrame). The
// awaited promise's reaction keeps it while the frame waits, and the
// frame's registers while it runs (with the job state while the job that
// resumed the frame runs, jobState.af); the call's promise does not, so a
// held promise keeps no frame.
type asyncFrame struct {
	g       genFrame
	re      promiseReaction
	promise *Object
}

// asyncOp executes one async op (genOp's default case) of the frame whose
// registers start at base and returns the next pc. A *genFrame error
// suspends the frame.
func (r *Realm) asyncOp(fd *FunctionData, base int, w uint32, pc int) (int, error) {
	st := &r.interp
	top := base + int(fd.code.NumRegs)
	regs := st.stack[base:top:top]
	a, b := int(uint8(w>>8)), int(uint8(w>>16))
	switch bytecode.Op(w) {
	case bytecode.AsyncFunc:
		if err := r.asyncFunc(regs[a], w); err != nil {
			return pc, err
		}
	case bytecode.AsyncGenStart:
		o, err := r.newAsyncGenerator(st.frames[st.nframes-1].fn, fd, regs[a])
		if err != nil {
			return pc, err
		}
		regs = st.stack[base:top:top]
		regs[a] = ObjectValue(o)
		g := &o.internal.(*asyncGenerator).g
		g.out = regs[a]
		return pc, g.save(regs, pc)
	case bytecode.AsyncYield:
		o, err := r.objectArg(regs[b], w)
		if err != nil {
			return pc, err
		}
		ag, ok := o.internal.(*asyncGenerator)
		if !ok {
			return pc, r.wrongKind(w, "an async generator")
		}
		if !ag.g.runs(fd) {
			return pc, r.wrongKind(w, "the async generator its frame runs")
		}
		err = r.agCompleteStep(ag, regs[a], false, false)
		regs = st.stack[base:top:top]
		if err != nil {
			return pc, err
		}
		if len(ag.queue) > 0 {
			// The next request is already queued: continue with it.
			regs[a], regs[a+1] = ag.queue[0].value, ag.queue[0].mode
			return pc, nil
		}
		g := &ag.g
		g.recv, g.state = uint8(a), genSuspendedYield
		return pc, g.save(regs, pc)
	default:
		// The module and sloppy ops follow the async iteration ops: one
		// range test sends them on without asyncIterOp's hop.
		if op := bytecode.Op(w); op >= bytecode.ImportCall {
			if op >= bytecode.SetGlobalSloppy {
				return r.sloppyOp(fd, base, w, pc)
			}
			return r.moduleOp(fd, base, w, pc)
		}
		return r.asyncIterOp(fd, base, w, pc)
	}
	return pc, nil
}

// await is Await of the frame whose registers start at base: R[A] is the
// operand, and R[B] the async generator the frame runs or, in an async
// function's frame, the call's this until the first Await leaves the
// call's state there. It returns the frame's state, to save the window
// regs into (coldOp), and the frame suspends until the promise of the
// operand settles; or nil, and the frame continues at once (awaitNext) or
// with the error.
func (r *Realm) await(fd *FunctionData, base int, w uint32) (*genFrame, []Value, error) {
	st := &r.interp
	top := base + int(fd.code.NumRegs)
	regs := st.stack[base:top:top]
	a, b := int(uint8(w>>8)), int(uint8(w>>16))
	v := regs[a]
	p, err := r.awaitTarget(v)
	if err != nil {
		return nil, nil, err
	}
	regs = st.stack[base:top:top]
	var g *genFrame
	var re *promiseReaction
	if fd.code.Async && !fd.code.Generator {
		var af *asyncFrame
		if x := regs[b]; !x.isAsyncFrame() {
			// The call's first Await: its frame runs from its call, and
			// R[B] holds its this.
			af = r.newAsyncFrame(fd, x, len(regs))
			regs[b] = asyncFrameValue(af)
		} else if af = x.asAsyncFrame(); !af.g.runs(fd) {
			return nil, nil, r.wrongKind(w, "the async function or generator its frame runs")
		} else if j := r.resumingJob(af); j != nil && j.head == len(j.queue) {
			if res, mode, ok := r.awaitNext(j, p, v); ok {
				regs[a], regs[a+1] = res, mode
				return nil, nil, nil
			}
		}
		g, re = &af.g, &af.re
		g.out = ObjectValue(af.promise)
	} else {
		o, err := r.objectArg(regs[b], w)
		if err != nil {
			return nil, nil, err
		}
		ag, ok := o.internal.(*asyncGenerator)
		if !ok {
			return nil, nil, r.wrongKind(w, "an async function or generator")
		}
		g, re = &ag.g, &ag.re // run returns undefined to the resumer
		if !g.runs(fd) {
			return nil, nil, r.wrongKind(w, "the async function or generator its frame runs")
		}
	}
	g.recv, g.state = uint8(a), genAwaiting
	if p != nil {
		r.performPromiseThen(p, re)
	} else {
		r.enqueue(job{reaction: re, arg: v})
	}
	return g, regs, nil
}

// newAsyncFrame makes the promise and the state of the async function
// call whose frame, of fd with n registers and the this this, first
// awaits: the frame runs from the call, and its function is its callee.
func (r *Realm) newAsyncFrame(fd *FunctionData, this Value, n int) *asyncFrame {
	af, size := allocAsyncFrame(n)
	r.chargeObject(size)
	st := &r.interp
	af.g.fn, af.g.fd, af.g.this, af.g.state = st.frames[st.nframes-1].fn, fd, this, genExecuting
	af.re.reactor = af
	af.promise = r.newPromise()
	return af
}

// awaitNext lets the frame the running job resumed continue past an Await
// of v, whose promise awaitTarget made p (nil: v is no thenable), when the
// reaction job the Await would queue is the next job to run: the operand is
// no thenable or p is settled, and no job is queued (j.queue is empty, as
// the caller checks first). The frame's
// suspension would then return to the drain (endJob), which ends the job
// and runs that one next, resuming the frame with the result. awaitNext
// does what the drain does in between, ClearKeptObjects, and returns the
// result and the outcome as the resumption would hand them over. The frame
// suspends instead when the drain has more to do between the jobs: an
// interrupt or a measurement of the memory limit is pending. It suspends
// too for a rejected p that no handler has handled, for which the Await
// reports the handling to the rejection tracker.
func (r *Realm) awaitNext(j *jobState, p *Object, v Value) (Value, Value, bool) {
	if r.interruptFlag.Load() != 0 {
		return v, Undefined(), false
	}
	if m := r.lazy.mem; m != nil && m.allocated > m.room {
		return v, Undefined(), false
	}
	mode := Undefined()
	if p != nil {
		d := p.internal.(*promiseData)
		switch d.state {
		case PromisePending:
			return v, Undefined(), false
		case PromiseRejected:
			if !d.handled {
				return v, Undefined(), false
			}
			mode = True()
		}
		v = d.result
	}
	j.clearKept()
	return v, mode, true
}

// awaitTarget is PromiseResolve(%Promise%, v) of an Await: the promise to
// react to, or nil when v is no thenable. Such a v fulfills the promise
// PromiseResolve creates at once, whose reaction job is then queued with v:
// the caller queues that job itself, and no code can reach the promise. A
// thenable is adopted through a fresh promise as PromiseResolve does, and
// a then getter's exception rejects one.
func (r *Realm) awaitTarget(v Value) (*Object, error) {
	if !v.IsObject() {
		return nil, nil
	}
	o := v.AsObject()
	pi := r.promiseIntr()
	if o.class == ClassPromise {
		if r.plainPromise(o, pi) {
			return o, nil
		}
		c, err := o.GetProp(r, constructorKey)
		if err != nil {
			return nil, err
		}
		if c.IsObject() && c.AsObject() == pi.ctor {
			return o, nil
		}
	} else if r.notThenable(o, pi) {
		return nil, nil
	}
	then, err := o.GetProp(r, thenKey)
	if err != nil {
		t, ok := r.thrownValue(err)
		if !ok {
			return nil, err
		}
		p := r.newPromise()
		r.rejectPromise(p, t)
		return p, nil
	}
	if !IsCallable(then) {
		return nil, nil
	}
	p := r.newPromise()
	r.enqueue(job{promise: p, arg: v, fn: then.AsObject()})
	return p, nil
}

// resumingJob returns the job state when the running job resumed the call
// whose state is af, else nil. A frame holding its call's state runs from
// that job, and its suspension returns to the drain (awaitNext).
func (r *Realm) resumingJob(af *asyncFrame) *jobState {
	if lz := r.lazy; lz != nil && lz.jobs != nil && lz.jobs.af == af {
		return lz.jobs
	}
	return nil
}

// promiseSettled resumes the awaiting call with the awaited promise's
// result, recording its state for the measurements of a memory limit until
// the frame's registers, which hold it, are back on the stack
// (jobState.af). The body settles its own promise, so an error here is an
// interrupt.
func (af *asyncFrame) promiseSettled(r *Realm, v Value, rejected bool) error {
	j := r.lazy.jobs
	j.af = af
	_, err := r.resumeAwait(&af.g, v, rejected)
	j.af = nil
	if af.g.state == genExecuting {
		af.g.finish()
	}
	return err
}

// resumeAwait resumes the frame g suspended at an Await from the reaction
// job of the awaited promise, counting the resumption as a call.
func (r *Realm) resumeAwait(g *genFrame, v Value, rejected bool) (Value, error) {
	mode := Undefined()
	if rejected {
		mode = True()
	}
	r.callDepth++
	res, err := r.resumeFrame(g, v, mode)
	r.callDepth--
	return res, err
}

// asyncEnd is AsyncReturn or AsyncThrow of the frame whose registers
// start at base: the call's promise resolves or rejects with R[A], which
// then holds the promise for the Ret after it. R[B] holds the call's state
// once the call awaited (await), and the call's this before: a call that
// never awaited makes its promise here, fulfilled at once with a value
// that is no object.
func (r *Realm) asyncEnd(fd *FunctionData, base int, w uint32) error {
	if !fd.code.Async || fd.code.Generator {
		return r.wrongKind(w, "an async function")
	}
	st := &r.interp
	top := base + int(fd.code.NumRegs)
	regs := st.stack[base:top:top]
	a, b := int(uint8(w>>8)), int(uint8(w>>16))
	v := regs[a]
	ret := bytecode.Op(w) == bytecode.AsyncReturn
	var p *Object
	if x := regs[b]; x.isAsyncFrame() {
		p = x.asAsyncFrame().promise
	} else if ret && !v.IsObject() {
		pi := r.promiseIntr()
		po := &promiseObject{}
		r.chargeObject(unsafe.Sizeof(promiseObject{}))
		regs[a] = ObjectValue(r.initPromise(po, pi.proto, pi.root))
		po.data.state, po.data.result = PromiseFulfilled, v
		return nil
	} else {
		p = r.newPromise()
	}
	var err error
	if ret {
		err = r.resolvePromiseErr(p, v)
	} else {
		r.rejectPromise(p, v)
	}
	regs = st.stack[base:top:top] // a then getter or the rejection tracker may have run code
	regs[a] = ObjectValue(p)
	return err
}

// asyncFunc is AsyncFunc, of a function's code or a module's prologue: v
// is the closure a Closure just made of an async function or async
// generator function, which it checks (Realm.wrongKind) and completes.
func (r *Realm) asyncFunc(v Value, w uint32) error {
	fn, fd, err := r.closureArg(v, w)
	if err != nil {
		return err
	}
	if !fd.code.Async {
		return r.wrongKind(w, "an async function")
	}
	r.makeAsyncFunction(fn)
	return nil
}

// makeAsyncFunction completes the fresh closure o of an async function: its
// [[Prototype]] is %AsyncFunction.prototype%. An async generator function's
// is %AsyncGeneratorFunction.prototype%, and its own `prototype` (writable,
// not enumerable or configurable) an ordinary object inheriting from
// %AsyncGeneratorPrototype%.
func (r *Realm) makeAsyncFunction(o *Object) {
	ai := r.asyncIntr()
	if o.internal.(*FunctionData).code.Generator {
		o.proto = ai.AsyncGeneratorFunctionPrototype
		o.shape = r.rootShapeFor(o.proto).
			addProperty(r, StringKey(AtomLength), attrConfigurable).
			addProperty(r, StringKey(AtomName), attrConfigurable).
			addProperty(r, StringKey(AtomPrototype), attrWritable)
		o.slots = append(o.slots, ObjectValue(r.NewObjectWithProto(ai.AsyncGeneratorPrototype)))
		return
	}
	o.proto = ai.AsyncFunctionPrototype
	o.shape = r.rootShapeFor(o.proto).
		addProperty(r, StringKey(AtomLength), attrConfigurable).
		addProperty(r, StringKey(AtomName), attrConfigurable)
}

// --- intrinsics ----------------------------------------------------------------

// asyncIntr returns the realm's async intrinsics, building them on first
// use: into a mutable realm's own, once per process for the shared realms
// (sharedAsyncIntrinsics).
func (r *Realm) asyncIntr() *asyncIntrinsics {
	ai := r.generatorIntrinsics()
	if ai.AsyncFunctionPrototype == nil {
		if r.sharedIntrinsics {
			r.Intrinsics = sharedAsyncIntrinsics()
			return r.async
		}
		installAsyncIntrinsics(r, ai)
	}
	return ai
}

// sharedAsyncIntrinsics returns the latest shared intrinsics, building the
// async ones into them first if no shared realm of the process has. The
// template leaves them out, as it does the late globals (buildLateGroup),
// so a process that runs no async function or async iteration carries none
// of their objects, which every collection would mark. No group installer
// needs them: its builder would build them into the template's copy.
func sharedAsyncIntrinsics() *Intrinsics {
	if intr := sharedIntr.Load(); intr.async.AsyncFunctionPrototype != nil {
		return intr
	}
	lateMu.Lock()
	defer lateMu.Unlock()
	if intr := sharedIntr.Load(); intr.async.AsyncFunctionPrototype != nil {
		return intr
	}
	b := newGroupBuilder()
	ai := *b.async
	b.async = &ai
	installAsyncIntrinsics(b, &ai)
	b.freezeIntrinsics()
	addSharedRoots(b.rootShapes)
	sharedIntr.Store(b.Intrinsics)
	return b.Intrinsics
}

// installAsyncIntrinsics builds the intrinsics of async functions, async
// generators and async iteration.
func installAsyncIntrinsics(r *Realm, ai *asyncIntrinsics) {
	// %AsyncFunction.prototype%: constructor, @@toStringTag.
	afp := r.newIntrinsic(ClassObject, r.FunctionPrototype, 2)
	af := r.newConstructor(AtomAsyncFunction, 1, asyncFunctionCall, asyncFunctionConstruct, afp)
	af.SetPrototypeOf(r, r.FunctionCtor)
	r.installOrReplace(afp, StringKey(AtomConstructor), propCell{value: ObjectValue(af), attrs: attrConfigurable})
	r.installToStringTag(afp, AtomAsyncFunction)

	// %AsyncIteratorPrototype%: @@asyncIterator.
	aip := r.newIntrinsic(ClassObject, r.ObjectPrototype, 1)
	r.installSymbolMethod(aip, SymAsyncIterator, atomAsyncIteratorFn, iteratorProtoIterator, 0, attrHidden)

	// %AsyncGeneratorFunction.prototype%: constructor, prototype,
	// @@toStringTag; %AsyncGeneratorPrototype%: constructor, next, return,
	// throw, @@toStringTag. The links are read-only, as the generator ones.
	agfp := r.newIntrinsic(ClassObject, r.FunctionPrototype, 3)
	agp := r.newIntrinsic(ClassObject, aip, 5)
	agf := r.newConstructor(AtomAsyncGeneratorFunction, 1, asyncGeneratorFunctionCall, asyncGeneratorFunctionConstruct, agfp)
	agf.SetPrototypeOf(r, r.FunctionCtor)
	r.installOrReplace(agfp, StringKey(AtomConstructor), propCell{value: ObjectValue(agf), attrs: attrConfigurable})
	r.installOrReplace(agfp, StringKey(AtomPrototype), propCell{value: ObjectValue(agp), attrs: attrConfigurable})
	r.installToStringTag(agfp, AtomAsyncGeneratorFunction)
	r.installOrReplace(agp, StringKey(AtomConstructor), propCell{value: ObjectValue(agfp), attrs: attrConfigurable})
	r.installBuiltins(agp, []builtinDef{
		{AtomNext, asyncGeneratorNext, 1},
		{AtomReturn, asyncGeneratorReturn, 1},
		{AtomThrow, asyncGeneratorThrow, 1},
	})
	r.installToStringTag(agp, AtomAsyncGenerator)

	// %AsyncFromSyncIteratorPrototype%: next, return, throw.
	afsp := r.newIntrinsic(ClassObject, aip, 3)
	r.installBuiltins(afsp, []builtinDef{
		{AtomNext, asyncFromSyncNext, 1},
		{AtomReturn, asyncFromSyncReturn, 1},
		{AtomThrow, asyncFromSyncThrow, 1},
	})
	next, _, _ := afsp.shape.Lookup(nextKey)

	ai.AsyncFunction, ai.AsyncFunctionPrototype = af, afp
	ai.AsyncGeneratorFunction, ai.AsyncGeneratorFunctionPrototype, ai.AsyncGeneratorPrototype = agf, agfp, agp
	ai.AsyncIteratorPrototype, ai.AsyncFromSyncIteratorPrototype = aip, afsp
	ai.asyncFromSyncNext = afsp.slots[next].AsObject()
}

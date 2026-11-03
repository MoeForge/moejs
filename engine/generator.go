package engine

import (
	"github.com/MoeForge/moejs/bytecode"
	"unsafe"
)

// Generators (ECMA-262 §27.3, §27.5) run on the interpreter's register stack
// like every bytecode function: no goroutine or Go stack is kept per
// generator. A generator function is a KindMethod closure (not
// constructible) that GenFunc moves onto %GeneratorFunction.prototype% and
// gives its own `prototype`. Its body binds the parameters and declarations,
// then runs GenStart, which creates the generator and suspends: the call
// returns the generator.
//
// Suspending returns from run. The suspending op (GenStart, Yield) copies
// the frame's register window into the generator's genFrame, records the
// resume pc and returns the genFrame as its error; run's unwinding path
// recognizes the type and returns what genFrame.suspend hands it (the
// generator, or the iterator result) after recording the environment and
// its depth. Resuming (genResume) copies the window back to the top of the
// register stack, pushes a frame holding the saved pc and re-enters run,
// which starts at its frame's pc (ordinary calls push 0) with the
// environment depth in the pc's high byte. Only suspending and resuming
// copy; calls and frames are the ones of ordinary functions.
//
// A resumption hands the code after Yield the sent value in R[A] and the
// mode in R[A+1]: undefined for next, true for throw, false for return. The
// compiler dispatches on the mode with ordinary jumps, so throw and return
// run the frame's handlers, finally blocks and iterator closes the way
// `throw` and `return` statements do. yield* loops over Delegate, which
// forwards the mode to the inner iterator, and a Yield of the inner result
// object as is.

// genState is a generator's [[GeneratorState]].
type genState uint8

const (
	genSuspendedStart genState = iota
	genSuspendedYield
	genExecuting
	genCompleted
	// genAwaiting is an async function or generator suspended at an Await,
	// executing as far as the spec is concerned (async.go).
	genAwaiting
	// genDrainingQueue is an async generator whose body completed and
	// that settles its queued requests (async_gen.go).
	genDrainingQueue
)

// genDepthShift places the environment depth of a suspended frame above its
// resume pc in frameInfo.pc. The compiler keeps the depth in range, and
// bytecode.VerifyRegisters the pcs (bytecode.PCBits).
const genDepthShift = bytecode.PCBits

// genFrame is the internal payload of a ClassGenerator object: the suspended
// frame and the generator's state. It is also the error value by which the
// suspending ops signal run.
type genFrame struct {
	fn   *Object
	fd   *FunctionData
	this Value
	env  *Env
	// regs is the saved register window (NumRegs values).
	regs []Value
	// pc is the resume pc, with the environment depth above genDepthShift.
	pc    uint32
	recv  uint8 // the register a resumption writes (the Yield's or Await's A)
	state genState
	// out is what run returns at the suspension: the generator (GenStart)
	// or the iterator result (Yield).
	out Value
}

func (g *genFrame) Error() string { return "internal: generator suspension" }

// runs reports whether g is the state a frame of fd runs: a suspension
// saves the frame's pc and registers into g, and its resumption runs them
// as g's frame, so a frame of another function, or one that suspends into
// a completed generator (which keeps no registers), would resume as code
// it is not: at a pc that starts no instruction of it, with registers its
// verified flow never wrote (bytecode.VerifyRegisters), or past its window.
func (g *genFrame) runs(fd *FunctionData) bool { return g.fd == fd && g.state == genExecuting }

// save copies the frame's window into g and records the resume pc; run's
// unwinding path completes the suspension (suspend).
func (g *genFrame) save(regs []Value, pc int) *genFrame {
	copy(g.regs, regs)
	g.pc = uint32(pc)
	return g
}

// suspend records the environment of the suspended frame and returns the
// value run returns.
func (g *genFrame) suspend(env *Env, depth int) Value {
	g.env = env
	g.pc |= uint32(depth) << genDepthShift
	out := g.out
	g.out = Undefined()
	return out
}

// finish completes the generator and releases its frame.
func (g *genFrame) finish() {
	g.state = genCompleted
	g.regs, g.env, g.this = nil, nil, Undefined()
}

// generatorObject co-allocates a generator with its payload.
type generatorObject struct {
	obj Object
	g   genFrame
}

// genOp executes one generator op (classOp's default case) of the frame
// whose registers start at base and returns the next pc. A *genFrame error
// suspends the frame.
func (r *Realm) genOp(fd *FunctionData, base int, w uint32, pc int) (int, error) {
	st := &r.interp
	top := base + int(fd.code.NumRegs)
	regs := st.stack[base:top:top]
	a, b := int(uint8(w>>8)), int(uint8(w>>16))
	switch bytecode.Op(w) {
	case bytecode.GenFunc:
		if err := r.genFunc(regs[a], w); err != nil {
			return pc, err
		}
	case bytecode.GenStart:
		o, err := r.newGenerator(st.frames[st.nframes-1].fn, fd, regs[a])
		if err != nil {
			return pc, err
		}
		regs = st.stack[base:top:top]
		regs[a] = ObjectValue(o)
		g := o.internal.(*genFrame)
		g.out = regs[a]
		return pc, g.save(regs, pc)
	case bytecode.Yield:
		o, err := r.objectArg(regs[b], w)
		if err != nil {
			return pc, err
		}
		g, ok := o.internal.(*genFrame)
		if !ok {
			return pc, r.wrongKind(w, "a generator")
		}
		if !g.runs(fd) {
			return pc, r.wrongKind(w, "the generator its frame runs")
		}
		out := regs[a]
		if w>>24 == 0 {
			out = r.createIterResult(out, false)
		}
		g.out, g.recv, g.state = out, uint8(a), genSuspendedYield
		return pc, g.save(regs, pc)
	case bytecode.GenIter:
		it, next, err := r.delegateIterator(regs[b])
		regs = st.stack[base:top:top]
		regs[a], regs[a+1] = it, next
		return pc, err
	case bytecode.Delegate:
		v, outcome, err := r.delegateStep(regs[b], regs[b+1], regs[a], regs[a+1])
		regs = st.stack[base:top:top]
		regs[a], regs[a+1] = v, outcome
		return pc, err
	default:
		return r.asyncOp(fd, base, w, pc)
	}
	return pc, nil
}

// genFunc is GenFunc, of a function's code or a module's prologue: v is
// the closure a Closure just made of a generator function, which it
// checks (Realm.wrongKind) and completes.
func (r *Realm) genFunc(v Value, w uint32) error {
	o, fd, err := r.closureArg(v, w)
	if err != nil {
		return err
	}
	if !fd.code.Generator || fd.code.Async {
		return r.wrongKind(w, "a generator function")
	}
	r.makeGeneratorFunction(o)
	return nil
}

// makeGeneratorFunction completes the fresh closure o of a generator
// function: its [[Prototype]] is %GeneratorFunction.prototype% and its own
// `prototype` (writable, not enumerable or configurable) an ordinary object
// inheriting from %GeneratorPrototype%.
func (r *Realm) makeGeneratorFunction(o *Object) {
	gi := r.generatorIntrinsics()
	o.proto = gi.GeneratorFunctionPrototype
	o.shape = r.rootShapeFor(o.proto).
		addProperty(r, StringKey(AtomLength), attrConfigurable).
		addProperty(r, StringKey(AtomName), attrConfigurable).
		addProperty(r, StringKey(AtomPrototype), attrWritable)
	o.slots = append(o.slots, ObjectValue(r.NewObjectWithProto(gi.GeneratorPrototype)))
}

// newGenerator creates the generator of a call to fn with `this` this:
// OrdinaryCreateFromConstructor(fn, %GeneratorPrototype%).
func (r *Realm) newGenerator(fn *Object, fd *FunctionData, this Value) (*Object, error) {
	pv, err := fn.GetProp(r, StringKey(AtomPrototype))
	if err != nil {
		return nil, err
	}
	proto := r.generatorIntrinsics().GeneratorPrototype
	if pv.IsObject() {
		proto = pv.AsObject()
	}
	r.markPrototype(proto)
	gen := &generatorObject{g: genFrame{fn: fn, fd: fd, this: this, regs: make([]Value, fd.code.NumRegs)}}
	r.chargeObject(unsafe.Sizeof(generatorObject{}) + uintptr(len(gen.g.regs)*valueSize))
	o := initObject(&gen.obj, ClassGenerator, r.rootShapeFor(proto))
	o.internal = &gen.g
	return o, nil
}

// genResume implements GeneratorResume (mode undefined) and
// GeneratorResumeAbrupt (true: throw, false: return) with the value v for
// the method of %GeneratorPrototype% called on this.
func (r *Realm) genResume(this, v, mode Value, method string) (Value, error) {
	var g *genFrame
	if this.IsObject() && this.AsObject().class == ClassGenerator {
		g = this.AsObject().internal.(*genFrame)
	}
	if g == nil {
		return Undefined(), r.TypeError("Generator.prototype.%s called on incompatible receiver %s", method, r.DisplayString(this))
	}
	switch g.state {
	case genExecuting:
		return Undefined(), r.TypeError("Generator is already running")
	case genSuspendedStart:
		if !mode.IsUndefined() {
			g.finish()
		}
	}
	if g.state == genCompleted {
		switch {
		case mode.IsUndefined():
			return r.createIterResult(Undefined(), true), nil
		case mode.AsBool():
			return Undefined(), r.Throw(v)
		}
		return r.createIterResult(v, true), nil
	}
	res, err := r.resumeFrame(g, v, mode)
	if g.state != genExecuting {
		return res, err // suspended by a Yield, which made the result, or interrupted before resuming
	}
	g.finish()
	if err != nil {
		return Undefined(), err
	}
	return r.createIterResult(res, true), nil
}

// resumeFrame continues the suspended frame g, handing a resumption after
// a Yield or Await the value v and mode, and returns what run returns. The
// state is genExecuting until the frame suspends again or completes; an
// interrupt raised before resuming leaves the frame suspended.
func (r *Realm) resumeFrame(g *genFrame, v, mode Value) (Value, error) {
	if r.interruptFlag.Load() != 0 {
		if err := r.pendingInterrupt(); err != nil {
			return Undefined(), err
		}
	}
	if g.fd.icBase == icUnbound {
		return r.resumeDynamic(g, v, mode)
	}
	st := &r.interp
	base := st.sp
	top := base + len(g.regs)
	if top > len(st.stack) {
		r.growStack(top)
	}
	regs := st.stack[base:top:top]
	copy(regs, g.regs)
	// The frame's registers are on the stack until it suspends again, which
	// saves them back: the saved ones would keep what the frame drops, past
	// any await it continues in place (awaitNext). Their length sizes the
	// next resumption, and the buffer takes the next suspension.
	clear(g.regs)
	if g.state != genSuspendedStart {
		regs[g.recv], regs[g.recv+1] = v, mode
	}
	fi := st.nframes
	if fi == len(st.frames) {
		nf := make([]frameInfo, max(initialFrames, 2*len(st.frames)))
		copy(nf, st.frames)
		st.frames = nf
	}
	st.frames[fi] = frameInfo{fn: g.fn, pc: g.pc, base: uint32(base)}
	st.nframes = fi + 1
	st.sp = top
	g.state = genExecuting
	res, err := r.run(fi, g.fd, base, g.env, g.this, g.fn)
	st.sp, st.nframes = base, fi
	return res, err
}

// delegateIterator is GetIterator(v, sync) of yield*: the iterator and its
// next method, never a fast-mode record (the inner results are yielded as
// they are).
func (r *Realm) delegateIterator(v Value) (it, next Value, err error) {
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
	if it, err = r.Call(m, v, nil); err != nil {
		return Undefined(), Undefined(), err
	}
	if !it.IsObject() {
		return Undefined(), Undefined(), r.TypeError("Result of the Symbol.iterator method is not an object")
	}
	next, err = it.AsObject().Get(r, nextKey, it)
	return it, next, err
}

// delegateStep runs one iteration of the yield* loop (§15.5.5 step 7) on the
// iterator it with next method next, for the received value v and mode
// (Yield's). It returns the value and the outcome: undefined when the inner
// result res is to be yielded as is (v = res), true when the delegation is
// done with value v, false when the generator returns v.
func (r *Realm) delegateStep(it, next, v, mode Value) (Value, Value, error) {
	args := []Value{v}
	var res Value
	var err error
	switch {
	case mode.IsUndefined():
		res, err = r.Call(next, it, args)
	case mode.AsBool():
		var m Value
		if m, err = r.GetMethod(it, StringKey(AtomThrow)); err != nil {
			return Undefined(), Undefined(), err
		}
		if m.IsUndefined() {
			// The protocol is violated: close the iterator, then throw.
			rec := iterRecord{it, next}
			if err = rec.close(r); err != nil {
				return Undefined(), Undefined(), err
			}
			return Undefined(), Undefined(), r.TypeError("The iterator does not provide a 'throw' method")
		}
		res, err = r.Call(m, it, args)
	default:
		var m Value
		if m, err = r.GetMethod(it, returnKey); err != nil {
			return Undefined(), Undefined(), err
		}
		if m.IsUndefined() {
			return v, False(), nil
		}
		res, err = r.Call(m, it, args)
	}
	if err != nil {
		return Undefined(), Undefined(), err
	}
	if !res.IsObject() {
		return Undefined(), Undefined(), r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
	}
	ro := res.AsObject()
	done, err := ro.Get(r, StringKey(AtomDone), res)
	if err != nil {
		return Undefined(), Undefined(), err
	}
	if !ToBoolean(done) {
		return res, Undefined(), nil
	}
	if v, err = ro.Get(r, StringKey(AtomValue), res); err != nil {
		return Undefined(), Undefined(), err
	}
	if mode.IsUndefined() || mode.AsBool() {
		return v, True(), nil
	}
	return v, False(), nil
}

// --- intrinsics ----------------------------------------------------------------

// asyncIntrinsics holds the intrinsics of generators and async functions.
// None is a global: the shared template builds the generator ones with the
// other intrinsics and the shared realms the async ones once per process
// (sharedAsyncIntrinsics); a mutable realm builds each when it first needs
// one, the generator ones (generatorIntrinsics) apart from the async ones
// (asyncIntr).
type asyncIntrinsics struct {
	GeneratorFunction, GeneratorFunctionPrototype, GeneratorPrototype *Object

	AsyncFunction, AsyncFunctionPrototype *Object

	AsyncGeneratorFunction, AsyncGeneratorFunctionPrototype, AsyncGeneratorPrototype *Object
	AsyncIteratorPrototype, AsyncFromSyncIteratorPrototype                           *Object
	// asyncFromSyncNext is %AsyncFromSyncIteratorPrototype%.next, the next
	// method of every async-from-sync iterator record.
	asyncFromSyncNext *Object
}

// generatorIntrinsics returns the realm's generator intrinsics, building
// them on first use.
func (r *Realm) generatorIntrinsics() *asyncIntrinsics {
	if r.async == nil {
		r.async = newAsyncIntrinsics(r)
	}
	return r.async
}

// installGenerators builds the generator intrinsics into the shared
// template; a mutable realm defers them to their first use.
func installGenerators(r *Realm) {
	if r.buildingShared {
		r.async = newAsyncIntrinsics(r)
	}
}

func newAsyncIntrinsics(r *Realm) *asyncIntrinsics {
	gi := &asyncIntrinsics{}
	// %GeneratorFunction.prototype%: constructor, prototype, @@toStringTag.
	gfp := r.newIntrinsic(ClassObject, r.FunctionPrototype, 3)
	// %GeneratorPrototype%: constructor, next, return, throw, @@toStringTag.
	gp := r.newIntrinsic(ClassObject, r.IteratorPrototype, 5)
	gf := r.newConstructor(AtomGeneratorFunction, 1, generatorFunctionCall, generatorFunctionConstruct, gfp)
	gf.SetPrototypeOf(r, r.FunctionCtor)
	// newConstructor made gfp.constructor writable; both links are
	// read-only here.
	r.installOrReplace(gfp, StringKey(AtomConstructor), propCell{value: ObjectValue(gf), attrs: attrConfigurable})
	r.installOrReplace(gfp, StringKey(AtomPrototype), propCell{value: ObjectValue(gp), attrs: attrConfigurable})
	r.installToStringTag(gfp, AtomGeneratorFunction)
	r.installOrReplace(gp, StringKey(AtomConstructor), propCell{value: ObjectValue(gfp), attrs: attrConfigurable})
	// A table literal here, not a package variable: the methods reach
	// this function through run (an initialization cycle).
	r.installBuiltins(gp, []builtinDef{
		{AtomNext, generatorNext, 1},
		{AtomReturn, generatorReturn, 1},
		{AtomThrow, generatorThrow, 1},
	})
	r.installToStringTag(gp, AtomGenerator)
	gi.GeneratorFunction, gi.GeneratorFunctionPrototype, gi.GeneratorPrototype = gf, gfp, gp
	return gi
}

// generatorNext implements %GeneratorPrototype%.next(value).
func generatorNext(r *Realm, this Value, args []Value) (Value, error) {
	return r.genResume(this, Arg(args, 0), Undefined(), "next")
}

// generatorReturn implements %GeneratorPrototype%.return(value).
func generatorReturn(r *Realm, this Value, args []Value) (Value, error) {
	return r.genResume(this, Arg(args, 0), False(), "return")
}

// generatorThrow implements %GeneratorPrototype%.throw(exception).
func generatorThrow(r *Realm, this Value, args []Value) (Value, error) {
	return r.genResume(this, Arg(args, 0), True(), "throw")
}

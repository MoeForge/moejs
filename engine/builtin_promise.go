package engine

import "unsafe"

// Promise (ES2025 §27.2). A promise is a ClassPromise object whose internal
// is its promiseData: state, result, the pending reactions and
// [[PromiseIsHandled]]. Its jobs run from the realm's job queue (jobs.go).
//
// Resolving functions and the functions the combinators and finally pass to
// then are data natives, so creating one costs a function object and no
// closure. A capability of %Promise% itself whose functions no program can
// reach (then's derived promise, Promise.resolve, Promise.try, allSettled)
// has none: resolving it calls resolvePromise or rejectPromise directly,
// which is unobservable because each is resolved at most once.
//
// HostPromiseRejectionTracker reports to jobState.tracker when set
// (SetPromiseRejectionTracker, builtin_promise_host.go).

func init() { lateGlobal(StringKey(AtomPromise), installPromise) }

// promiseIntrinsics are %Promise%, %Promise.prototype% and its then, and
// the guards of %Promise.prototype%.constructor, %Promise%[@@species] and
// %Promise.prototype%.then (protocol.go), whose original values make
// plainPromise's promises read %Promise% as their constructor and species
// and nativeThen's the original then.
type promiseIntrinsics struct {
	proto, ctor, then *Object
	// root is the root shape of proto, which every new promise of proto
	// takes: the shared tree's for a shared realm, which a lookup in the
	// process-wide map would find (rootShapeFor).
	root                               *Shape
	ctorGuard, speciesGuard, thenGuard protoGuard
	// objectThen and arrayThen watch that %Object.prototype% and
	// %Array.prototype% have no then (notThenable).
	objectThen, arrayThen absentGuard
}

// promiseIntr returns the realm's promise intrinsics, defining Promise if
// the realm has yet to (ensureLate).
func (r *Realm) promiseIntr() *promiseIntrinsics {
	r.lateAt(latePromise)
	return r.promise
}

var promiseProtoMethods = []builtinDef{
	{AtomThen, promiseProtoThen, 2},
	{AtomCatch, promiseProtoCatch, 1},
	{AtomFinally, promiseProtoFinally, 1},
}

var promiseStatics = []builtinDef{
	{AtomAll, promiseAll, 1},
	{AtomAllSettled, promiseAllSettled, 1},
	{AtomAny, promiseAny, 1},
	{AtomRace, promiseRace, 1},
	{AtomReject, promiseReject, 1},
	{AtomResolve, promiseResolveStatic, 1},
	{AtomTry, promiseTry, 1},
	{AtomWithResolvers, promiseWithResolvers, 0},
}

func installPromise(r *Realm) {
	proto := r.newIntrinsic(ClassObject, r.ObjectPrototype, len(promiseProtoMethods)+2)
	ctor := r.newConstructor(AtomPromise, 1, requireNew("Promise"), promiseConstruct, proto)
	// The executor's resolving functions keep the promise it runs for.
	ctor.internal.(*FunctionData).measurable = true
	r.promise = &promiseIntrinsics{proto: proto, ctor: ctor, root: r.rootShapeFor(proto)}
	r.installBuiltins(proto, promiseProtoMethods)
	r.installToStringTag(proto, AtomPromise)
	r.installBuiltins(ctor, promiseStatics)
	r.installSpeciesGetter(ctor)
	species, _ := ctor.getOwnCell(speciesKey)
	then, _ := proto.getOwnCell(thenKey)
	r.promise.then = then.value.AsObject()
	r.promise.ctorGuard = protoGuard{obj: proto, key: constructorKey, want: ObjectValue(ctor)}
	r.promise.speciesGuard = protoGuard{obj: ctor, key: speciesKey, want: species.value}
	r.promise.thenGuard = protoGuard{obj: proto, key: thenKey, want: then.value}
	r.promise.objectThen = absentGuard{obj: r.ObjectPrototype, key: thenKey}
	r.promise.arrayThen = absentGuard{obj: r.ArrayPrototype, key: thenKey}
	r.bindGlobal(AtomPromise, ObjectValue(ctor))
}

// --- promise objects ----------------------------------------------------------

// PromiseState is the [[PromiseState]] of a promise.
type PromiseState uint8

// Promise states.
const (
	PromisePending PromiseState = iota
	PromiseFulfilled
	PromiseRejected
)

// promiseData is the internal of a ClassPromise object. A pending promise's
// [[PromiseFulfillReactions]] and [[PromiseRejectReactions]] are one list:
// every then adds one reaction to both, so each record holds both handlers.
// The list is a ring through last, whose next is the first reaction, so one
// pointer appends in order and promiseObject fits the 128-byte size class.
type promiseData struct {
	result  Value
	last    *promiseReaction
	state   PromiseState
	handled bool // [[PromiseIsHandled]]
	// host marks a promise of NewHostPromise, which SettlePromise settles,
	// and resolved is its [[AlreadyResolved]] (builtin_promise_host.go).
	host, resolved bool
	// derived marks a promise then derived as %Promise%'s, allocated with
	// the reaction that settles it (thenPromise).
	derived bool
}

// promiseObject co-allocates a promise with its data.
type promiseObject struct {
	obj  Object
	data promiseData
}

// thenPromise co-allocates the promise then derives as %Promise%'s with
// the reaction of then that settles it, one allocation for both. The
// reaction job drops the handlers, which the promise would keep otherwise.
type thenPromise struct {
	promiseObject
	re promiseReaction
}

// promiseCapability is a PromiseCapability Record. resolve and reject nil
// mean promise is a %Promise% instance resolved directly (see the file
// comment).
type promiseCapability struct {
	promise, resolve, reject *Object
}

// promiseReaction is a PromiseReaction Record for both outcomes: the
// handlers (nil is empty) and the capability of the derived promise, or a
// reactor that replaces all three (promiseReact).
type promiseReaction struct {
	next                    *promiseReaction
	cap                     promiseCapability
	onFulfilled, onRejected *Object
	reactor                 promiseReactor
}

// promiseReactor is what await and the async iteration machinery react to a
// promise with: promiseSettled runs as the reaction job with the promise's
// result. An error it returns escapes the job (jobs.go).
type promiseReactor interface {
	promiseSettled(r *Realm, v Value, rejected bool) error
}

func isPromise(v Value) bool { return v.IsObject() && v.AsObject().class == ClassPromise }

var thenKey = StringKey(AtomThen)

// nativeThen reports whether o is a promise whose then is the original
// %Promise.prototype%.then without a lookup: one of %Promise.prototype%
// with no own property, while the guard sees the original then.
func (r *Realm) nativeThen(o *Object, pi *promiseIntrinsics) bool {
	return o.class == ClassPromise && o.proto == pi.proto && o.shape.count == 0 && o.flags&(flagDict|flagHasLazy) == 0 && r.guardHolds(&pi.thenGuard)
}

// notThenable reports whether o has no then without a lookup: o has no
// such own property, lazy or in a dictionary, and its prototype is
// %Object.prototype%, or %Array.prototype% on %Object.prototype%, while
// the guards see neither with a then. Such a lookup runs no code, so
// skipping it is unobservable.
func (r *Realm) notThenable(o *Object, pi *promiseIntrinsics) bool {
	if o.flags&(flagDict|flagHasLazy) != 0 {
		return false
	}
	switch o.proto {
	case r.ObjectPrototype:
	case r.ArrayPrototype:
		if !r.sharedIntrinsics && (o.proto.proto != r.ObjectPrototype || !r.absentHolds(&pi.arrayThen)) {
			return false
		}
	default:
		return false
	}
	if o.shape.count != 0 {
		if _, _, ok := o.shape.Lookup(thenKey); ok {
			return false
		}
	}
	return r.absentHolds(&pi.objectThen)
}

// plainPromise reports whether o is a promise whose constructor is
// %Promise% without a lookup: one of %Promise.prototype% with no own
// property, while %Promise.prototype%.constructor is %Promise%. Such a
// lookup runs no code, so skipping it is unobservable.
func (r *Realm) plainPromise(o *Object, pi *promiseIntrinsics) bool {
	return o.class == ClassPromise && o.proto == pi.proto && o.shape.count == 0 && o.flags&(flagDict|flagHasLazy) == 0 && r.guardHolds(&pi.ctorGuard)
}

// promiseSpecies implements SpeciesConstructor(p, %Promise%), with no
// lookups for a plain promise while %Promise%[@@species] is the original
// getter, which returns %Promise%.
func (r *Realm) promiseSpecies(p *Object) (*Object, error) {
	pi := r.promiseIntr()
	if r.plainPromise(p, pi) && r.guardHolds(&pi.speciesGuard) {
		return pi.ctor, nil
	}
	return r.speciesConstructor(p, pi.ctor)
}

// allocPromise creates a pending promise with the given prototype.
func (r *Realm) allocPromise(proto *Object) *Object {
	po := &promiseObject{}
	r.chargeObject(unsafe.Sizeof(promiseObject{}))
	return r.initPromise(po, proto, r.rootShapeFor(proto))
}

// initPromise makes po a pending promise with the given prototype and its
// root shape, and returns it.
func (r *Realm) initPromise(po *promiseObject, proto *Object, root *Shape) *Object {
	r.markPrototype(proto)
	po.data.result = Undefined()
	o := &po.obj
	o.shape = root
	o.proto = proto
	o.class = ClassPromise
	o.flags = flagExtensible
	o.internal = &po.data
	return o
}

// newPromise creates a pending promise with %Promise.prototype%.
func (r *Realm) newPromise() *Object {
	pi := r.promiseIntr()
	po := &promiseObject{}
	r.chargeObject(unsafe.Sizeof(promiseObject{}))
	return r.initPromise(po, pi.proto, pi.root)
}

// resolvePromise runs the steps of p's resolve function for v: a thenable
// is adopted from a job, anything else fulfills p. Like the resolve
// function, it resolves p once; the caller holds p's only resolution. An
// interrupt raised by reading v.then leaves p pending (the interrupt flag
// stays set for the caller's next check).
func (r *Realm) resolvePromise(p *Object, v Value) { _ = r.resolvePromiseErr(p, v) }

// resolvePromiseErr is resolvePromise returning the interrupt.
func (r *Realm) resolvePromiseErr(p *Object, v Value) error {
	if !v.IsObject() {
		r.settlePromise(p, v, false)
		return nil
	}
	if v.AsObject() == p {
		r.rejectPromise(p, ObjectValue(r.NewError(KindTypeError, "Chaining cycle detected for promise #<Promise>")))
		return nil
	}
	if pi := r.promise; pi != nil {
		if r.nativeThen(v.AsObject(), pi) {
			r.enqueue(job{promise: p, arg: v, fn: pi.then})
			return nil
		}
		if r.notThenable(v.AsObject(), pi) {
			r.settlePromise(p, v, false)
			return nil
		}
	}
	then, err := v.AsObject().GetProp(r, thenKey)
	if err != nil {
		t, ok := r.thrownValue(err)
		if !ok {
			return err
		}
		r.rejectPromise(p, t)
		return nil
	}
	if !IsCallable(then) {
		r.settlePromise(p, v, false)
		return nil
	}
	r.enqueue(job{promise: p, arg: v, fn: then.AsObject()})
	return nil
}

// rejectPromise implements RejectPromise; a settled p is left alone.
func (r *Realm) rejectPromise(p *Object, reason Value) { r.settlePromise(p, reason, true) }

// settlePromise implements FulfillPromise and RejectPromise, queueing the
// reactions in the order they were added.
func (r *Realm) settlePromise(p *Object, v Value, rejected bool) {
	d := p.internal.(*promiseData)
	if d.state != PromisePending {
		return
	}
	var re *promiseReaction
	if d.last != nil {
		re, d.last.next = d.last.next, nil
	}
	d.result, d.last = v, nil
	d.state = PromiseFulfilled
	if rejected {
		d.state = PromiseRejected
		if !d.handled {
			r.trackRejection(p, PromiseRejectionReject)
		}
	}
	for re != nil {
		next := re.next
		re.next = nil
		r.enqueue(job{reaction: re, arg: v, rejected: rejected})
		re = next
	}
}

// performPromiseThen implements PerformPromiseThen with the reaction re.
func (r *Realm) performPromiseThen(p *Object, re *promiseReaction) {
	d := p.internal.(*promiseData)
	switch d.state {
	case PromisePending:
		if d.last == nil {
			re.next = re
		} else {
			re.next, d.last.next = d.last.next, re
		}
		d.last = re
	case PromiseFulfilled:
		r.enqueue(job{reaction: re, arg: d.result})
	default:
		if !d.handled {
			r.trackRejection(p, PromiseRejectionHandle)
		}
		r.enqueue(job{reaction: re, arg: d.result, rejected: true})
	}
	d.handled = true
}

// promiseReact implements PerformPromiseThen(p, x, x) with no result
// capability: x.promiseSettled runs as the reaction job. It allocates the
// reaction record only.
func (r *Realm) promiseReact(p *Object, x promiseReactor) {
	r.performPromiseThen(p, &promiseReaction{reactor: x})
}

// promiseResolve implements PromiseResolve(%Promise%, v).
func (r *Realm) promiseResolve(v Value) (*Object, error) {
	pi := r.promiseIntr()
	if isPromise(v) {
		if r.plainPromise(v.AsObject(), pi) {
			return v.AsObject(), nil
		}
		c, err := v.AsObject().GetProp(r, constructorKey)
		if err != nil {
			return nil, err
		}
		if c.IsObject() && c.AsObject() == pi.ctor {
			return v.AsObject(), nil
		}
	}
	p := r.newPromise()
	return p, r.resolvePromiseErr(p, v)
}

// promiseResolveWith implements PromiseResolve(c, v).
func (r *Realm) promiseResolveWith(c *Object, v Value) (Value, error) {
	if c == r.promiseIntr().ctor {
		p, err := r.promiseResolve(v)
		if err != nil {
			return Undefined(), err
		}
		return ObjectValue(p), nil
	}
	if isPromise(v) {
		vc, err := v.AsObject().GetProp(r, constructorKey)
		if err != nil {
			return Undefined(), err
		}
		if vc.IsObject() && vc.AsObject() == c {
			return v, nil
		}
	}
	pc, err := r.newPromiseCapability(ObjectValue(c), false)
	if err != nil {
		return Undefined(), err
	}
	if err := r.capResolve(&pc, v); err != nil {
		return Undefined(), err
	}
	return ObjectValue(pc.promise), nil
}

// trackRejection implements HostPromiseRejectionTracker.
func (r *Realm) trackRejection(p *Object, op PromiseRejectionOperation) {
	if lz := r.lazy; lz != nil && lz.jobs != nil && lz.jobs.tracker != nil {
		lz.jobs.tracker(p, op)
	}
}

// thrownValue returns the value a catch sees for err, or false for an
// interrupt, which no promise catches.
func (r *Realm) thrownValue(err error) (Value, bool) {
	switch e := err.(type) {
	case *InterruptedError:
		return Undefined(), false
	case *Exception:
		return e.Value, true
	}
	return r.hostErrorValue(err), true
}

// callStack calls fn with args placed in the free register stack above the
// current frame, as callSetter does, so the call allocates no argument list.
func (r *Realm) callStack(fn *Object, this Value, args ...Value) (Value, error) {
	st := &r.interp
	sp, n := st.sp, len(args)
	if sp+n > len(st.stack) {
		r.growStack(sp + n)
	}
	copy(st.stack[sp:], args)
	st.sp = sp + n
	res, err := r.CallObject(fn, this, st.stack[sp:sp+n:sp+n])
	st.sp = sp
	return res, err
}

// invokeThen implements Invoke(v, "then", args).
func (r *Realm) invokeThen(v Value, args ...Value) (Value, error) {
	f, err := r.GetV(v, StringKey(AtomThen))
	if err != nil {
		return Undefined(), err
	}
	if !IsCallable(f) {
		return Undefined(), r.TypeError("%s is not a function", r.DisplayString(f))
	}
	return r.callStack(f.AsObject(), v, args...)
}

// --- jobs -----------------------------------------------------------------------

// promiseReactionJob implements NewPromiseReactionJob's job for the
// settled value v.
func (r *Realm) promiseReactionJob(re *promiseReaction, v Value, rejected bool) error {
	if re.reactor != nil {
		return re.reactor.promiseSettled(r, v, rejected)
	}
	h := re.onFulfilled
	if rejected {
		h = re.onRejected
	}
	re.onFulfilled, re.onRejected = nil, nil
	if h != nil {
		res, err := r.callStack(h, Undefined(), v)
		if err == nil {
			v, rejected = res, false
		} else if t, ok := r.thrownValue(err); ok {
			v, rejected = t, true
		} else {
			return err
		}
	}
	if rejected {
		return r.capReject(&re.cap, v)
	}
	return r.capResolve(&re.cap, v)
}

// promiseResolveThenableJob implements NewPromiseResolveThenableJob's job:
// thenable.then(resolve, reject) with fresh resolving functions of p. When
// then is the original %Promise.prototype%.then and thenable a promise
// whose constructor and species it reads as %Promise% without a lookup,
// that call's steps are a reaction on thenable that settles p directly: the
// promise then would derive and the resolving functions are unobservable.
func (r *Realm) promiseResolveThenableJob(p *Object, thenable Value, then *Object) error {
	if pi := r.promise; pi != nil && then == pi.then && r.plainPromise(thenable.AsObject(), pi) && r.guardHolds(&pi.speciesGuard) {
		r.performPromiseThen(thenable.AsObject(), &promiseReaction{cap: promiseCapability{promise: p}})
		return nil
	}
	s, resolve, reject := r.createResolvingFunctions(p)
	_, err := r.callStack(then, thenable, ObjectValue(resolve), ObjectValue(reject))
	if err == nil {
		return nil
	}
	t, ok := r.thrownValue(err)
	if !ok {
		return err
	}
	s.reject(r, t)
	return nil
}

// --- resolving functions and capabilities -----------------------------------------

// resolvingFns is the state a promise's resolve and reject functions share,
// allocated with both functions: either keeps the other, which the memory
// measurement counts with it (liveWalker.function). The resolve function's
// thisValue is the promise until either function is called, and Undefined
// once [[AlreadyResolved]] is set, so that the state has no fields of its own.
type resolvingFns struct {
	resolveFn, rejectFn nativeFuncObject
}

// createResolvingFunctions implements CreateResolvingFunctions(p) in one
// allocation.
func (r *Realm) createResolvingFunctions(p *Object) (s *resolvingFns, resolve, reject *Object) {
	s = new(resolvingFns)
	r.chargeObject(unsafe.Sizeof(resolvingFns{}))
	resolve, reject = r.initResolvingFunction(&s.resolveFn, promiseResolveFunction, s),
		r.initResolvingFunction(&s.rejectFn, promiseRejectFunction, s)
	s.resolveFn.fd.thisValue = ObjectValue(p)
	return s, resolve, reject
}

// initResolvingFunction makes fo the resolving function fn of s, as
// NewNativeDataFunction makes a function.
func (r *Realm) initResolvingFunction(fo *nativeFuncObject, fn NativeDataFunc, s *resolvingFns) *Object {
	r.initFunction(&fo.obj, &fo.fd, fo.slots[:], AtomEmpty, 1, FuncNativeData)
	fo.fd.dataFn = fn
	fo.fd.data = s
	return &fo.obj
}

// take returns the promise of s and sets [[AlreadyResolved]], or nil when
// it is already set.
func (s *resolvingFns) take() *Object {
	p := &s.resolveFn.fd.thisValue
	if !p.IsObject() {
		return nil
	}
	o := p.AsObject()
	*p = Undefined()
	return o
}

func (s *resolvingFns) reject(r *Realm, v Value) {
	if p := s.take(); p != nil {
		r.rejectPromise(p, v)
	}
}

func promiseResolveFunction(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	p := fd.Data().(*resolvingFns).take()
	if p == nil {
		return Undefined(), nil
	}
	return Undefined(), r.resolvePromiseErr(p, Arg(args, 0))
}

func promiseRejectFunction(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	fd.Data().(*resolvingFns).reject(r, Arg(args, 0))
	return Undefined(), nil
}

// newPromiseCapability implements NewPromiseCapability(c). For %Promise%
// it creates the resolving functions only when fns is set: the caller
// hands them to code that can observe them.
func (r *Realm) newPromiseCapability(c Value, fns bool) (promiseCapability, error) {
	if pi := r.promiseIntr(); c.IsObject() && c.AsObject() == pi.ctor {
		p := r.newPromise()
		if !fns {
			return promiseCapability{promise: p}, nil
		}
		_, resolve, reject := r.createResolvingFunctions(p)
		return promiseCapability{p, resolve, reject}, nil
	}
	if !IsConstructor(c) {
		return promiseCapability{}, r.TypeError("%s is not a constructor", r.DisplayString(c))
	}
	s := &capabilityExecutor{resolve: Undefined(), reject: Undefined()}
	executor := r.NewNativeDataFunction(AtomEmpty, 2, promiseCapabilityExecutor, s)
	pv, err := r.Construct(c, []Value{ObjectValue(executor)}, nil)
	if err != nil {
		return promiseCapability{}, err
	}
	if !IsCallable(s.resolve) || !IsCallable(s.reject) {
		return promiseCapability{}, r.TypeError("Promise resolve or reject function is not callable")
	}
	if !pv.IsObject() {
		return promiseCapability{}, r.TypeError("Promise constructor returned a non-object")
	}
	return promiseCapability{pv.AsObject(), s.resolve.AsObject(), s.reject.AsObject()}, nil
}

// capabilityExecutor is the state of GetCapabilitiesExecutor.
type capabilityExecutor struct{ resolve, reject Value }

func promiseCapabilityExecutor(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	s := fd.Data().(*capabilityExecutor)
	if !s.resolve.IsUndefined() || !s.reject.IsUndefined() {
		return Undefined(), r.TypeError("Promise executor has already been invoked with non-undefined arguments")
	}
	s.resolve, s.reject = Arg(args, 0), Arg(args, 1)
	return Undefined(), nil
}

// capResolve calls c.[[Resolve]](v).
func (r *Realm) capResolve(c *promiseCapability, v Value) error {
	if c.resolve == nil {
		return r.resolvePromiseErr(c.promise, v)
	}
	_, err := r.callStack(c.resolve, Undefined(), v)
	return err
}

// capReject calls c.[[Reject]](v).
func (r *Realm) capReject(c *promiseCapability, v Value) error {
	if c.reject == nil {
		r.rejectPromise(c.promise, v)
		return nil
	}
	_, err := r.callStack(c.reject, Undefined(), v)
	return err
}

// capSettle calls c.[[Reject]](v) when reject is set, c.[[Resolve]](v)
// otherwise, and returns what the call returned, for the element functions
// that return it. The built-in functions of an intrinsic capability return
// undefined.
func (r *Realm) capSettle(c *promiseCapability, reject bool, v Value) (Value, error) {
	fn := c.resolve
	if reject {
		fn = c.reject
	}
	switch {
	case fn != nil:
		return r.callStack(fn, Undefined(), v)
	case reject:
		return Undefined(), r.capReject(c, v)
	}
	return Undefined(), r.capResolve(c, v)
}

// rejectAbrupt implements IfAbruptRejectPromise for the error err.
func (r *Realm) rejectAbrupt(c *promiseCapability, err error) (Value, error) {
	t, ok := r.thrownValue(err)
	if !ok {
		return Undefined(), err
	}
	if err := r.capReject(c, t); err != nil {
		return Undefined(), err
	}
	return ObjectValue(c.promise), nil
}

// --- constructor and prototype ----------------------------------------------------

// promiseConstruct implements new Promise(executor).
func promiseConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	executor := Arg(args, 0)
	if !IsCallable(executor) {
		return Undefined(), r.TypeError("Promise resolver %s is not a function", r.DisplayString(executor))
	}
	pi := r.promiseIntr()
	proto, err := r.GetPrototypeFromConstructor(newTarget, pi.ctor, pi.proto)
	if err != nil {
		return Undefined(), err
	}
	p := r.allocPromise(proto)
	s, resolve, reject := r.createResolvingFunctions(p)
	if _, err := r.callStack(executor.AsObject(), Undefined(), ObjectValue(resolve), ObjectValue(reject)); err != nil {
		t, ok := r.thrownValue(err)
		if !ok {
			return Undefined(), err
		}
		s.reject(r, t)
	}
	return ObjectValue(p), nil
}

// callableOrNil is v as a handler: nil (empty) unless callable.
func callableOrNil(v Value) *Object {
	if IsCallable(v) {
		return v.AsObject()
	}
	return nil
}

// promiseProtoThen implements Promise.prototype.then.
func promiseProtoThen(r *Realm, this Value, args []Value) (Value, error) {
	if !isPromise(this) {
		return Undefined(), r.TypeError("Method Promise.prototype.then called on incompatible receiver %s", r.DisplayString(this))
	}
	p := this.AsObject()
	c, err := r.promiseSpecies(p)
	if err != nil {
		return Undefined(), err
	}
	onFulfilled, onRejected := callableOrNil(Arg(args, 0)), callableOrNil(Arg(args, 1))
	if pi := r.promise; c == pi.ctor {
		tp := &thenPromise{}
		r.chargeObject(unsafe.Sizeof(thenPromise{}))
		d := r.initPromise(&tp.promiseObject, pi.proto, pi.root)
		tp.data.derived = true
		tp.re = promiseReaction{cap: promiseCapability{promise: d}, onFulfilled: onFulfilled, onRejected: onRejected}
		r.performPromiseThen(p, &tp.re)
		return ObjectValue(d), nil
	}
	pc, err := r.newPromiseCapability(ObjectValue(c), false)
	if err != nil {
		return Undefined(), err
	}
	r.performPromiseThen(p, &promiseReaction{cap: pc, onFulfilled: onFulfilled, onRejected: onRejected})
	return ObjectValue(pc.promise), nil
}

// promiseProtoCatch implements Promise.prototype.catch.
func promiseProtoCatch(r *Realm, this Value, args []Value) (Value, error) {
	return r.invokeThen(this, Undefined(), Arg(args, 0))
}

// finallyData is the state of the thenFinally and catchFinally functions.
type finallyData struct {
	onFinally, c *Object
}

// promiseProtoFinally implements Promise.prototype.finally.
func promiseProtoFinally(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("Method Promise.prototype.finally called on incompatible receiver %s", r.DisplayString(this))
	}
	c, err := r.promiseSpecies(this.AsObject())
	if err != nil {
		return Undefined(), err
	}
	onFinally := Arg(args, 0)
	if !IsCallable(onFinally) {
		return r.invokeThen(this, onFinally, onFinally)
	}
	d := &finallyData{onFinally: onFinally.AsObject(), c: c}
	thenFinally := r.NewNativeDataFunction(AtomEmpty, 1, promiseThenFinally, d)
	catchFinally := r.NewNativeDataFunction(AtomEmpty, 1, promiseCatchFinally, d)
	return r.invokeThen(this, ObjectValue(thenFinally), ObjectValue(catchFinally))
}

func promiseThenFinally(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	return r.runFinally(fd.Data().(*finallyData), Arg(args, 0), promiseValueThunk)
}

func promiseCatchFinally(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	return r.runFinally(fd.Data().(*finallyData), Arg(args, 0), promiseThrower)
}

// runFinally calls onFinally, then chains a function that returns (thunk)
// or throws (thrower) v onto PromiseResolve(C, its result).
func (r *Realm) runFinally(d *finallyData, v Value, fn NativeDataFunc) (Value, error) {
	res, err := r.CallObject(d.onFinally, Undefined(), nil)
	if err != nil {
		return Undefined(), err
	}
	p, err := r.promiseResolveWith(d.c, res)
	if err != nil {
		return Undefined(), err
	}
	return r.invokeThen(p, ObjectValue(r.NewNativeDataFunction(AtomEmpty, 0, fn, &v)))
}

func promiseValueThunk(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	return *fd.Data().(*Value), nil
}

func promiseThrower(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	return Undefined(), r.Throw(*fd.Data().(*Value))
}

// --- statics ------------------------------------------------------------------------

// promiseResolveStatic implements Promise.resolve.
func promiseResolveStatic(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("Promise.resolve called on non-object")
	}
	return r.promiseResolveWith(this.AsObject(), Arg(args, 0))
}

// promiseReject implements Promise.reject.
func promiseReject(r *Realm, this Value, args []Value) (Value, error) {
	pc, err := r.newPromiseCapability(this, false)
	if err != nil {
		return Undefined(), err
	}
	if err := r.capReject(&pc, Arg(args, 0)); err != nil {
		return Undefined(), err
	}
	return ObjectValue(pc.promise), nil
}

// promiseTry implements Promise.try as the current draft has it: the
// callback runs first, and a promise it returns comes back unwrapped
// through PromiseResolve (ES2025 created the capability first and always
// wrapped; test262 tests the draft).
func promiseTry(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("Promise.try called on non-object")
	}
	var rest []Value
	if len(args) > 1 {
		rest = args[1:]
	}
	res, err := r.Call(Arg(args, 0), Undefined(), rest)
	if err == nil {
		return r.promiseResolveWith(this.AsObject(), res)
	}
	if _, ok := r.thrownValue(err); !ok {
		return Undefined(), err
	}
	pc, cerr := r.newPromiseCapability(this, false)
	if cerr != nil {
		return Undefined(), cerr
	}
	return r.rejectAbrupt(&pc, err)
}

// promiseWithResolvers implements Promise.withResolvers.
func promiseWithResolvers(r *Realm, this Value, args []Value) (Value, error) {
	pc, err := r.newPromiseCapability(this, true)
	if err != nil {
		return Undefined(), err
	}
	o := r.NewObject()
	o.DefineOwnDataFast(r, StringKey(atomPromiseLower), ObjectValue(pc.promise), attrDefault)
	o.DefineOwnDataFast(r, StringKey(AtomResolve), ObjectValue(pc.resolve), attrDefault)
	o.DefineOwnDataFast(r, StringKey(AtomReject), ObjectValue(pc.reject), attrDefault)
	return ObjectValue(o), nil
}

// combinator selects Promise.all, allSettled, any or race.
type combinator uint8

const (
	combineAll combinator = iota
	combineAllSettled
	combineAny
	combineRace
)

func promiseAll(r *Realm, this Value, args []Value) (Value, error) {
	return r.promiseCombine(this, Arg(args, 0), combineAll)
}

func promiseAllSettled(r *Realm, this Value, args []Value) (Value, error) {
	return r.promiseCombine(this, Arg(args, 0), combineAllSettled)
}

func promiseAny(r *Realm, this Value, args []Value) (Value, error) {
	return r.promiseCombine(this, Arg(args, 0), combineAny)
}

func promiseRace(r *Realm, this Value, args []Value) (Value, error) {
	return r.promiseCombine(this, Arg(args, 0), combineRace)
}

// promiseCombine implements the shared steps of Promise.all, allSettled,
// any and race: the capability, GetPromiseResolve, GetIterator, then
// closing the iterator and rejecting on an abrupt completion.
func (r *Realm) promiseCombine(this, iterable Value, kind combinator) (Value, error) {
	pc, err := r.newPromiseCapability(this, kind != combineAllSettled)
	if err != nil {
		return Undefined(), err
	}
	c := this.AsObject()
	resolve, err := c.GetProp(r, StringKey(AtomResolve))
	if err != nil {
		return r.rejectAbrupt(&pc, err)
	}
	if !IsCallable(resolve) {
		return r.rejectAbrupt(&pc, r.TypeError("Promise resolve %s is not a function", r.DisplayString(resolve)))
	}
	ir, err := r.getIterator(iterable)
	if err != nil {
		return r.rejectAbrupt(&pc, err)
	}
	if err := r.performCombine(&ir, c, &pc, resolve.AsObject(), kind); err != nil {
		return r.rejectAbrupt(&pc, ir.closeThrow(r, err))
	}
	return ObjectValue(pc.promise), nil
}

// combineState is what the element functions of one Promise.all,
// allSettled or any call share: the values (errors for any), the remaining
// elements count and the result capability.
type combineState struct {
	values    []Value
	remaining int
	cap       promiseCapability
	kind      combinator
}

// combineElement is the state of one element function; the two functions
// of an allSettled element share it, and so [[AlreadyCalled]].
type combineElement struct {
	state  *combineState
	index  int
	called bool
}

// performCombine implements PerformPromiseAll, PerformPromiseAllSettled,
// PerformPromiseAny and PerformPromiseRace.
func (r *Realm) performCombine(ir *iterRecord, c *Object, pc *promiseCapability, resolve *Object, kind combinator) error {
	var s *combineState
	if kind != combineRace {
		s = &combineState{remaining: 1, cap: *pc, kind: kind}
	}
	for index := 0; ; index++ {
		if err := interruptEvery(r, int64(index)); err != nil {
			return err
		}
		next, done, err := ir.step(r)
		if err != nil {
			return err
		}
		if done {
			if s == nil {
				return nil
			}
			if s.remaining--; s.remaining != 0 {
				return nil
			}
			if kind == combineAny {
				return r.Throw(r.promiseAggregateError(s.values))
			}
			return r.capResolve(pc, ObjectValue(r.NewArrayFromSlice(s.values)))
		}
		if s != nil {
			s.values = append(s.values, Undefined())
		}
		np, err := r.callStack(resolve, ObjectValue(c), next)
		if err != nil {
			return err
		}
		var onFulfilled, onRejected Value
		switch kind {
		case combineRace:
			onFulfilled, onRejected = ObjectValue(pc.resolve), ObjectValue(pc.reject)
		case combineAll:
			e := &combineElement{state: s, index: index}
			onFulfilled, onRejected = ObjectValue(r.NewNativeDataFunction(AtomEmpty, 1, promiseElementFulfilled, e)), ObjectValue(pc.reject)
		case combineAllSettled:
			e := &combineElement{state: s, index: index}
			onFulfilled = ObjectValue(r.NewNativeDataFunction(AtomEmpty, 1, promiseElementFulfilled, e))
			onRejected = ObjectValue(r.NewNativeDataFunction(AtomEmpty, 1, promiseElementRejected, e))
		case combineAny:
			e := &combineElement{state: s, index: index}
			onFulfilled, onRejected = ObjectValue(pc.resolve), ObjectValue(r.NewNativeDataFunction(AtomEmpty, 1, promiseElementRejected, e))
		}
		if s != nil {
			s.remaining++
		}
		if _, err := r.invokeThen(np, onFulfilled, onRejected); err != nil {
			return err
		}
	}
}

// promiseElementFulfilled is a Promise.all resolve element function or a
// Promise.allSettled resolve element function.
func promiseElementFulfilled(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	e := fd.Data().(*combineElement)
	if e.called {
		return Undefined(), nil
	}
	e.called = true
	x := Arg(args, 0)
	if e.state.kind == combineAllSettled {
		x = r.settledRecord(atomFulfilled, AtomValue, x)
	}
	return e.state.element(r, e.index, x)
}

// promiseElementRejected is a Promise.allSettled reject element function or
// a Promise.any reject element function.
func promiseElementRejected(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	e := fd.Data().(*combineElement)
	if e.called {
		return Undefined(), nil
	}
	e.called = true
	x := Arg(args, 0)
	if e.state.kind == combineAllSettled {
		x = r.settledRecord(atomRejected, atomReason, x)
	}
	return e.state.element(r, e.index, x)
}

// element records x at index and settles the result once every element
// has been, returning what the capability's function returned.
func (s *combineState) element(r *Realm, index int, x Value) (Value, error) {
	s.values[index] = x
	if s.remaining--; s.remaining != 0 {
		return Undefined(), nil
	}
	if s.kind == combineAny {
		return r.capSettle(&s.cap, true, r.promiseAggregateError(s.values))
	}
	return r.capSettle(&s.cap, false, ObjectValue(r.NewArrayFromSlice(s.values)))
}

// settledRecord creates {status, key: x} for Promise.allSettled.
func (r *Realm) settledRecord(status, key *String, x Value) Value {
	o := r.NewObject()
	o.DefineOwnDataFast(r, StringKey(atomStatus), StringValue(status), attrDefault)
	o.DefineOwnDataFast(r, StringKey(key), x, attrDefault)
	return ObjectValue(o)
}

// promiseAggregateError creates Promise.any's AggregateError, which has no
// message.
func (r *Realm) promiseAggregateError(errs []Value) Value {
	o := r.newErrorObject(r.AggregateErrorPrototype, Undefined())
	o.DefineOwnDataFast(r, StringKey(AtomErrors), ObjectValue(r.NewArrayFromSlice(errs)), attrHidden)
	return ObjectValue(o)
}

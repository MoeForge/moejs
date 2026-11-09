package engine

import (
	"errors"
	"reflect"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// NativeFunc is the Go signature of a builtin function. args may alias the
// interpreter's register stack: natives MUST NOT retain the slice.
type NativeFunc func(r *Realm, this Value, args []Value) (Value, error)

// NativeCtor is the [[Construct]] behaviour of a builtin constructor.
type NativeCtor func(r *Realm, args []Value, newTarget *Object) (Value, error)

// NativeDataFunc is the behaviour of a data native (NewNativeDataFunction):
// its per-function state is a value stored in the function object, read with
// fd.Data(), instead of variables captured by a closure, so one adapter
// serves every function of its kind and creating one costs only the function
// object. args may alias the register stack, as for NativeFunc.
type NativeDataFunc func(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error)

// FuncKind classifies function objects for Call/Construct dispatch.
type FuncKind uint8

const (
	// funcNone is the zero kind, the kind of a FunctionData that no
	// function object initialized: none of its accessors follows the state
	// the native and bound kinds keep after it (nat, bound).
	funcNone FuncKind = iota
	FuncNative
	FuncBytecode
	FuncBound
	FuncNativeData // a NativeDataFunc and its payload
	FuncProxy      // a callable proxy: dataFn is proxyCall, data the *proxyData
)

// FunctionData is the internal payload of ClassFunction objects. It holds
// what a bytecode function needs, which the other kinds share: at 64 bytes
// a closure with a `prototype` is 160 bytes (funcObject) and one without
// 144 (closureObject). The native kinds keep their state in a nativeState
// right after it (nat), a bound function its boundFunc (bound). A
// FunctionData lives inside its function object and must not be copied:
// a copy has no state after it (go vet's copylocks check reports copies).
type FunctionData struct {
	_    noCopy
	code *bytecode.Function
	env  *Env
	// thisValue is the captured this of an arrow function, the bound this of
	// a bound function, the promise of a resolve function (resolvingFns) and
	// the [[HomeObject]] of the other bytecode functions, as an object or
	// Undefined (HomeObject).
	thisValue Value
	realm     *Realm
	name      *String
	meta      *funcMeta // interpreter: materialized constants of code (interp.go)
	icBase    uint32    // interpreter: base of this function's inline caches in realm.ic
	kind      FuncKind  // after icBase so the two share a word
	// plain is set on a closure whose entry binds only its parameters: no
	// rest parameter, arguments object or captured binding (enterFrame).
	plain bool
	// ctorSlots is, in its low four bits (ctorSlotsMask), the most named
	// properties (up to smallObjectMax) an object constructed with this
	// function as new.target ended its construction with, or the room such
	// objects went on to take (noteCtorSlots): the next one is allocated
	// with room for them. The high four count the first ctorProbe objects.
	ctorSlots uint8
	// measurable is set on a host's functions, whose values kept for later
	// go to AddLiveRoots or LiveReporter, and on the Promise constructor: a
	// measurement may run in the JavaScript they call back (measurable). It
	// shares ctorSlots' word.
	measurable bool
}

// noCopy makes go vet's copylocks check report a copy of the struct that
// holds it, as sync's noCopy does. It takes no space as a first field.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// nativeState is the state of a function of the native kinds (FuncNative,
// FuncNativeData, FuncProxy), which their objects allocate right after its
// FunctionData (nat).
type nativeState struct {
	native NativeFunc     // [[Call]] of a native function
	dataFn NativeDataFunc // [[Call]] of a data native or callable proxy
	// data is the state of the kind: the payload of a data native, the
	// NativeCtor of a native constructor, the *proxyData of a callable
	// proxy.
	data any
}

// boundFunc is the state of a bound function beyond its bound this, which
// its object allocates right after its FunctionData (bound).
type boundFunc struct {
	target *Object
	args   []Value
}

// ctor returns the [[Construct]] behaviour of a native constructor.
func (fd *FunctionData) ctor() NativeCtor { return fd.nat().data.(NativeCtor) }

// setCtor records the [[Construct]] behaviour of a native function; nil
// makes it a plain function.
func (fd *FunctionData) setCtor(ctor NativeCtor) {
	n := fd.nat()
	n.data = nil
	if ctor != nil {
		n.data = ctor
	}
}

// Kind returns the dispatch kind.
func (fd *FunctionData) Kind() FuncKind { return fd.kind }

// Code returns the compiled template for bytecode functions.
func (fd *FunctionData) Code() *bytecode.Function { return fd.code }

// Env returns the closure environment for bytecode functions.
func (fd *FunctionData) Env() *Env { return fd.env }

// ThisValue returns the lexically captured this of an arrow function (the
// bound this of a bound function).
func (fd *FunctionData) ThisValue() Value {
	switch fd.kind {
	case FuncBytecode:
		if fd.code.Kind != bytecode.KindArrow {
			return Undefined() // its home object (HomeObject)
		}
	case FuncNativeData:
		if _, ok := fd.nat().data.(*resolvingFns); ok {
			return Undefined() // the promise it settles (resolvingFns)
		}
	}
	return fd.thisValue
}

// HomeObject returns the [[HomeObject]] of a method (nil otherwise).
func (fd *FunctionData) HomeObject() *Object {
	if fd.kind != FuncBytecode || fd.code.Kind == bytecode.KindArrow || !fd.thisValue.IsObject() {
		return nil
	}
	return fd.thisValue.AsObject()
}

// Realm returns the realm that created the function, or nil for shared
// intrinsics (natives must use the realm passed to them instead).
func (fd *FunctionData) Realm() *Realm { return fd.realm }

// IsForeign reports whether v is a function, generator or async generator
// whose code runs in another realm than r: a bytecode function another
// realm created, or the generator of a call to one, reached directly or
// through bound functions and proxy targets, or a callable proxy another
// realm created, whose traps are that realm's code whatever its target.
// That code's inline caches are bound in its own realm's table and its
// prototype checks follow that realm's epoch, so its frames fail in r with
// a TypeError before they start (errForeignFrame); a host keeps the objects
// of a realm inside it (engine-api.md, "One realm per value"). The closures
// of dynamic code (eval, the Function constructors, EvalScript) are the
// realm's that compiled the code, which created them, and are foreign
// elsewhere like any other. A revoked proxy of r has no target and runs no
// code (using it is a TypeError), so it is not foreign. IsForeign does not
// see a foreign function reached through an object of another realm, the
// traps of a proxy that is not callable among them.
func (r *Realm) IsForeign(v Value) bool {
	if !v.IsObject() {
		return false
	}
	// No call, so that a host checking its arguments pays little for the
	// objects that are no code (comma-ok assertions do not call). The
	// targets of bound functions and proxies are fixed at creation, so the
	// walk ends.
	o := v.AsObject()
	for {
		var fd *FunctionData
		switch o.class {
		case ClassFunction:
			fd = dataOf(o, kindFunction)
			if fd != nil && fd.kind == FuncBound {
				o = fd.bound().target
				continue
			}
		case ClassProxy:
			// A callable proxy's state hangs off its function payload; a
			// module namespace has none.
			var pd *proxyData
			if pf, ok := dataOk(o, kindFunction); ok {
				// A callable proxy of another realm runs that realm's
				// traps, whatever its target: foreign.
				if pf.realm != nil && pf.realm != r {
					return true
				}
				pd, _ = pf.nat().data.(*proxyData)
			} else {
				pd = dataOf(o, kindProxy)
			}
			if pd == nil || pd.target == nil {
				return false
			}
			o = pd.target
			continue
		case ClassGenerator:
			if g := dataOf(o, kindGenerator); g != nil {
				fd = g.fd
			}
		case ClassAsyncGenerator:
			if g := extDataOf(o, kindAsyncGenerator); g != nil {
				fd = g.g.fd
			}
		default:
			return false
		}
		return fd != nil && fd.kind == FuncBytecode && fd.realm != r && fd.realm != nil
	}
}

// HoldsCode reports whether o is a function, generator, async generator or
// proxy: the objects IsForeign can report, so that a host checks its
// arguments without a call for the rest.
func (o *Object) HoldsCode() bool {
	return o.class == ClassFunction || o.class == ClassGenerator || o.class == ClassAsyncGenerator || o.class == ClassProxy
}

// Name returns the function's initial name (may be nil).
func (fd *FunctionData) Name() *String { return fd.name }

// BoundTarget returns the target of a bound function (nil otherwise).
func (fd *FunctionData) BoundTarget() *Object {
	if fd.kind != FuncBound {
		return nil
	}
	return fd.bound().target
}

// Native returns the native implementation (nil for bytecode functions and
// data natives).
func (fd *FunctionData) Native() NativeFunc {
	if fd.kind != FuncNative {
		return nil
	}
	return fd.nat().native
}

// Data returns the payload of a data native (nil otherwise).
func (fd *FunctionData) Data() any {
	if fd.kind != FuncNativeData {
		return nil
	}
	return fd.nat().data
}

// IsConstructor reports whether the function has [[Construct]].
func (fd *FunctionData) IsConstructor() bool {
	switch fd.kind {
	case FuncNative:
		return fd.nat().data != nil
	case FuncBound:
		return fd.bound().target.funcData().IsConstructor()
	case FuncProxy:
		return fd.nat().data.(*proxyData).ctor
	}
	return fd.code != nil && (fd.code.Kind == bytecode.KindNormal || fd.code.Kind.IsClassCtor())
}

// funcObject co-allocates a bytecode function, its payload and the slots of
// its three intrinsic properties (length, name and prototype), so a closure
// costs one allocation: 160 bytes. Ordinary functions, generator functions
// and class constructors have a `prototype`.
type funcObject struct {
	obj   Object
	fd    FunctionData
	slots [3]Value
}

// closureObject is the funcObject of a bytecode function without a
// `prototype` (arrow functions, methods, async functions): two slots, 144
// bytes.
type closureObject struct {
	obj   Object
	fd    FunctionData
	slots [2]Value
}

// nativeFuncObject co-allocates a function of the native kinds with its
// nativeState and the slots of length and name: 176 bytes.
type nativeFuncObject struct {
	obj   Object
	fd    FunctionData
	nat   nativeState
	slots [2]Value
}

// nativeCtorObject is the nativeFuncObject of a native constructor, with a
// third slot for its `prototype`: 192 bytes.
type nativeCtorObject struct {
	obj   Object
	fd    FunctionData
	nat   nativeState
	slots [3]Value
}

// boundFuncObject co-allocates a bound function with its boundFunc: 176
// bytes.
type boundFuncObject struct {
	obj   Object
	fd    FunctionData
	bound boundFunc
	slots [2]Value
}

// NewEnv creates an environment with n slots initialized to undefined. It
// panics if n is negative.
func NewEnv(parent *Env, n int) *Env {
	if n < 0 {
		panic("engine: NewEnv with a negative slot count")
	}
	return newEnvSized(parent, n)
}

// Parent returns the enclosing environment.
func (e *Env) Parent() *Env { return e.parent }

// up returns the environment d levels out from e.
func (e *Env) up(d uint8) *Env {
	for ; d > 0; d-- {
		e = e.parent
	}
	return e
}

// slotAt is the slot a GetEnv or SetEnv names in w: slot C of the
// environment B out from e. Inlined, it costs run's size, which must stay
// below the compiler's big-function size (DESIGN §6), only its call.
func (e *Env) slotAt(w uint32) *Value {
	return &e.up(uint8(w >> 16)).slots()[uint8(w>>24)]
}

// getSlotAt is slotAt for GetEnv. Depths below three have their own ops;
// depth three reads its parents directly, without up's loop branches.
// Other depths, including those below three, use up.
func (e *Env) getSlotAt(w uint32) *Value {
	if d := uint8(w >> 16); d == 3 {
		e = e.parent.parent.parent
	} else {
		e = e.up(d)
	}
	return &e.slots()[uint8(w>>24)]
}

// Len returns the slot count.
func (e *Env) Len() int { return e.n }

// Slot reads slot i.
func (e *Env) Slot(i int) Value { return e.slots()[i] }

// SetSlot writes slot i.
func (e *Env) SetSlot(i int, v Value) { e.slots()[i] = v }

// Slots exposes the slot storage for the interpreter.
func (e *Env) Slots() []Value { return e.slots() }

// ModuleEnv is the top-level environment of a module. Exports are live
// bindings: GetBindingValue reads the slot each time.
type ModuleEnv struct {
	*Env
	module *bytecode.Module
}

// NewModuleEnv creates a module environment with nslots bindings and the
// local exports of module.
func NewModuleEnv(nslots int, module *bytecode.Module) *ModuleEnv {
	return &ModuleEnv{Env: NewEnv(nil, nslots), module: module}
}

// GetBindingValue returns the current value of a local export (the module's
// own binding, not one re-exported from another module). ok is false for
// unknown names. A Hole() value means the binding is in its TDZ.
func (m *ModuleEnv) GetBindingValue(name string) (Value, bool) {
	i, ok := m.module.Export(name)
	if !ok {
		return Undefined(), false
	}
	return m.slots()[i], true
}

// ExportNames returns the names of the local exports, sorted.
func (m *ModuleEnv) ExportNames() []string {
	names := make([]string, len(m.module.Exports))
	for i, e := range m.module.Exports {
		names[i] = e.Name
	}
	return names
}

// ErrNoInterpreter is returned by Construct on bytecode functions until the
// interpreter sets constructFunction in its init().
var ErrNoInterpreter = errors.New("engine: bytecode interpreter is not linked")

// constructFunction runs [[Construct]] of a bytecode function. The
// interpreter (engine/interp_*.go) replaces this stub in init().
var constructFunction = func(r *Realm, fn *Object, fd *FunctionData, args []Value, newTarget *Object) (Value, error) {
	return Undefined(), ErrNoInterpreter
}

// MaxCallDepth is the limit for call nesting: past it a call or a function
// constructor is a RangeError ("Maximum call stack size exceeded"). It caps
// callDepth (CallDepth), which the builtins' array iteration and flat also
// count a level at a time, and JSON walks take units of it (jsonWalkUnits).
//
// A call holds 0.56-1.2 KB of Go stack, a generator's yield* 2.2 KB: up to
// about 9 MB at the limit. Nested JSON walks hold under 5 MB
// (builtin_json.go). The register stack's worst case, a window of 256
// registers of 16 bytes for each frame, is 16 MiB; MemoryLimit counts it
// as it grows (moveStack).
const MaxCallDepth = 4096

// EnterCall accounts one frame; the interpreter calls it for JS-to-JS calls.
func (r *Realm) EnterCall() error {
	if r.callDepth >= MaxCallDepth {
		return r.RangeError("Maximum call stack size exceeded")
	}
	r.callDepth++
	return nil
}

// ExitCall releases a frame taken by EnterCall.
func (r *Realm) ExitCall() { r.callDepth-- }

// CallDepth returns the current frame count.
func (r *Realm) CallDepth() int { return int(r.callDepth) }

// CallState is a snapshot of the realm's call bookkeeping: the frame depth
// and the interpreter's register stack and frame table positions. A host
// takes it at its entry into the engine and restores it when a Go panic
// (a native's non-JavaScript panic, re-panicked to the host) unwound
// through interpreter frames without running their exits, so a host that
// recovers keeps a usable runtime.
type CallState struct {
	depth, sp, nframes int
}

// CallState returns the current call bookkeeping.
func (r *Realm) CallState() CallState {
	return CallState{depth: int(r.callDepth), sp: r.interp.sp, nframes: r.interp.nframes}
}

// RestoreCallState resets the call bookkeeping to a snapshot taken at the
// same host boundary. At the outermost boundary, DropJobs then discards the
// jobs the call left queued, and no builtin or parse is left holding values
// or bytes for a memory limit's measurements (holding, holdBytes), nor the
// job state the async function call a job had resumed (jobState.af).
func (r *Realm) RestoreCallState(cs CallState) {
	r.callDepth, r.interp.sp, r.interp.nframes = int32(cs.depth), cs.sp, cs.nframes
	if cs.depth != 0 {
		return
	}
	if m := r.mem(); m != nil {
		m.holding, m.held = 0, 0
	}
	if lz := r.lazy; lz != nil && lz.jobs != nil {
		lz.jobs.af = nil
	}
}

// IsCallable implements the IsCallable abstract operation.
func IsCallable(v Value) bool {
	return v.IsObject() && v.AsObject().IsCallable()
}

// IsConstructor implements the IsConstructor abstract operation.
func IsConstructor(v Value) bool {
	if !IsCallable(v) {
		return false
	}
	fd := dataOf(v.AsObject(), kindFunction)
	return fd != nil && fd.IsConstructor()
}

// Call implements the Call abstract operation.
func (r *Realm) Call(fn Value, this Value, args []Value) (Value, error) {
	if !IsCallable(fn) {
		return Undefined(), r.TypeError("%s is not a function", r.DisplayString(fn))
	}
	return r.CallObject(fn.AsObject(), this, args)
}

// CallObject calls a function object known to be callable.
func (r *Realm) CallObject(fn *Object, this Value, args []Value) (Value, error) {
	fd := (*FunctionData)(unsafe.Add(unsafe.Pointer(fn), unsafe.Sizeof(Object{}))) // funcData, inlined by hand: an inlined call would leave a NOP

	if r.callDepth >= MaxCallDepth {
		return Undefined(), r.RangeError("Maximum call stack size exceeded")
	}
	r.callDepth++
	var (
		res Value
		err error
	)
	switch fd.kind {
	case FuncNative:
		res, err = fd.nat().native(r, this, args)
	case FuncBytecode:
		res, err = r.enterFrame(fn, fd, this, args)
	default:
		res, err = r.callOther(fd, this, args)
	}
	r.callDepth--
	if r.callDepth == 0 && r.jobsPending {
		if j := r.lazy.jobs; !j.draining {
			j.result = res
		}
		err = r.endJob(err)
	}
	return res, err
}

// callOther calls the kinds outside the two hot dispatch arms (natives and
// bytecode): bound functions and data natives. The caller has counted the
// call depth.
func (r *Realm) callOther(fd *FunctionData, this Value, args []Value) (Value, error) {
	if fd.kind == FuncBound {
		b := fd.bound()
		return r.CallObject(b.target, fd.thisValue, appendArgs(b.args, args))
	}
	return fd.nat().dataFn(r, fd, this, args)
}

// Construct implements the Construct abstract operation. newTarget nil means
// the constructor itself.
func (r *Realm) Construct(fn Value, args []Value, newTarget *Object) (Value, error) {
	if !IsConstructor(fn) {
		return Undefined(), r.TypeError("%s is not a constructor", r.DisplayString(fn))
	}
	f := fn.AsObject()
	if newTarget == nil {
		newTarget = f
	}
	fd := f.funcData()
	if r.callDepth >= MaxCallDepth {
		return Undefined(), r.RangeError("Maximum call stack size exceeded")
	}
	r.callDepth++
	var (
		res Value
		err error
	)
	switch fd.kind {
	case FuncNative:
		res, err = fd.ctor()(r, args, newTarget)
	case FuncBound:
		b := fd.bound()
		if newTarget == f {
			newTarget = b.target
		}
		res, err = r.Construct(ObjectValue(b.target), appendArgs(b.args, args), newTarget)
	case FuncProxy:
		res, err = r.proxyConstruct(fd.nat().data.(*proxyData), args, newTarget)
	default:
		res, err = constructFunction(r, f, fd, args, newTarget)
	}
	r.callDepth--
	if r.callDepth == 0 && r.jobsPending {
		if j := r.lazy.jobs; !j.draining {
			j.result = res
		}
		err = r.endJob(err)
	}
	return res, err
}

func appendArgs(bound, args []Value) []Value {
	if len(bound) == 0 {
		return args
	}
	out := make([]Value, 0, len(bound)+len(args))
	out = append(out, bound...)
	return append(out, args...)
}

// newNativeCtorObject allocates a native function object with the realm's
// shared function shape (length, name) and room for a third property, its
// `prototype`.
func (r *Realm) newNativeCtorObject(name *String, length int) (*Object, *FunctionData) {
	fo := &nativeCtorObject{}
	r.chargeObject(unsafe.Sizeof(nativeCtorObject{}))
	r.initFunction(&fo.obj, &fo.fd, fo.slots[:2:3], name, length, FuncNative)
	return &fo.obj, &fo.fd
}

// newNativeFunctionObject is newNativeCtorObject for a function that has no
// `prototype`: a nativeFuncObject, from the bootstrap slab while the
// intrinsics of a mutable realm are being built.
func (r *Realm) newNativeFunctionObject(name *String, length int, kind FuncKind) (*Object, *FunctionData) {
	var fo *nativeFuncObject
	if b := r.boot; b != nil && len(b.funcs) < cap(b.funcs) {
		n := len(b.funcs)
		b.funcs = b.funcs[:n+1]
		fo = &b.funcs[n]
	} else {
		fo = &nativeFuncObject{}
		r.chargeObject(unsafe.Sizeof(nativeFuncObject{}))
	}
	r.initFunction(&fo.obj, &fo.fd, fo.slots[:], name, length, kind)
	return &fo.obj, &fo.fd
}

// initFunction links a function object with its payload, the realm's
// function shape and its length and name slots.
func (r *Realm) initFunction(o *Object, fd *FunctionData, slots []Value, name *String, length int, kind FuncKind) {
	o.shape = r.functionShape()
	o.class = ClassFunction
	o.flags = flagExtensible
	setData(o, kindFunction, fd)
	fd.kind = kind
	fd.realm = r
	fd.name = name
	slots[0] = IntValue(length)
	slots[1] = StringValue(name)
	o.slots = slots
}

// setFunctionName implements SetFunctionName on a function the Closure op
// just created (its `name` is still an own data property): object-literal
// methods with computed keys are named after the evaluated key.
func (r *Realm) setFunctionName(fn *Object, name *String) {
	fn.funcData().name = name
	if p, attrs, ok := fn.lookupNamed(StringKey(AtomName)); ok && attrs&attrAccessor == 0 {
		*p = StringValue(name)
	}
}

// allocSlots hands out slot storage, from a bootstrap slab when available.
func (r *Realm) allocSlots(n int) []Value {
	if b := r.boot; b != nil && len(b.slots)+n <= cap(b.slots) {
		start := len(b.slots)
		b.slots = b.slots[:start+n]
		return b.slots[start : start+n : start+n]
	}
	return make([]Value, n)
}

// NewNativeFunction creates a builtin function object. name should be an
// atom; length is the value of the `length` property.
func (r *Realm) NewNativeFunction(name *String, length int, fn NativeFunc) *Object {
	o, fd := r.newNativeFunctionObject(name, length, FuncNative)
	fd.nat().native = fn
	return o
}

// NewHostFunction is NewNativeFunction for a host's function. The
// measurement of a charge past the memory limit runs in the JavaScript it
// calls back, as in plain code: the host keeps no JavaScript value only in
// Go across such a call, or registers what it keeps (AddLiveRoots,
// LiveReporter).
func (r *Realm) NewHostFunction(name *String, length int, fn NativeFunc) *Object {
	o, fd := r.newNativeFunctionObject(name, length, FuncNative)
	fd.nat().native = fn
	fd.measurable = true
	return o
}

// measurableBuiltins are the builtins, by code pointer, that keep no
// JavaScript value only in Go while they call back into JavaScript
// (FunctionData.measurable): the ones that hold none, and the ones that
// keep what they build on the register stack (map, filter, flatMap,
// Array.from, reduce's accumulator, sort's list, the reviver's root) or
// report the Go bytes they build (replace, holdBytes). A builtin that
// holds values only in Go across a callback is not among them: a charge
// made in its callbacks waits for it to return. A measurement looks them
// up when it finds one below a frame (FunctionData.isMeasurable), so that
// making a builtin costs nothing more.
var measurableBuiltins map[uintptr]bool

// isMeasurable reports whether a measurement may run in the JavaScript fd
// calls back: a host's function, the Promise constructor or one of
// measurableBuiltins.
func (fd *FunctionData) isMeasurable() bool {
	return fd.measurable || fd.kind == FuncNative && measurableBuiltins[reflect.ValueOf(fd.nat().native).Pointer()]
}

func init() {
	measurableBuiltins = map[uintptr]bool{}
	for _, fn := range []NativeFunc{
		arrayProtoForEach, arrayProtoSome, arrayProtoEvery,
		arrayProtoFind, arrayProtoFindIndex, arrayProtoFindLast, arrayProtoFindLastIndex,
		arrayProtoMap, arrayProtoFilter, arrayProtoFlatMap,
		arrayProtoReduce, arrayProtoReduceRight, arrayProtoSort, arrayFrom,
		jsonParse, stringProtoReplace, stringProtoReplaceAll, regexpProtoSymReplace,
		mapProtoForEach, setProtoForEach,
		generatorNext, generatorReturn, generatorThrow,
	} {
		measurableBuiltins[reflect.ValueOf(fn).Pointer()] = true
	}
}

// NewNativeDataFunction creates a function object whose [[Call]] is fn with
// data as its payload (FunctionData.Data): a host binding creates one per Go
// value while fn stays one package-level adapter per Go signature. Data
// natives are not constructors.
func (r *Realm) NewNativeDataFunction(name *String, length int, fn NativeDataFunc, data any) *Object {
	o, fd := r.newNativeFunctionObject(name, length, FuncNativeData)
	n := fd.nat()
	n.dataFn, n.data = fn, data
	return o
}

// NewNativeConstructor creates a builtin constructor with both [[Call]] and
// [[Construct]] behaviour. call may be nil, in which case calling without
// `new` throws a TypeError.
func (r *Realm) NewNativeConstructor(name *String, length int, call NativeFunc, ctor NativeCtor) *Object {
	o, fd := r.newNativeCtorObject(name, length)
	if call == nil {
		call = func(r *Realm, this Value, args []Value) (Value, error) {
			return Undefined(), r.TypeError("Constructor %s requires 'new'", name.GoString())
		}
	}
	fd.nat().native = call
	fd.setCtor(ctor)
	return o
}

// SetHomeObject records the [[HomeObject]] of a method: a bytecode function
// other than an arrow function, which has none (its super is its
// enclosing function's).
func (fd *FunctionData) SetHomeObject(o *Object) {
	if fd.kind != FuncBytecode || fd.code.Kind == bytecode.KindArrow {
		return
	}
	fd.thisValue = Undefined()
	if o != nil {
		fd.thisValue = ObjectValue(o)
	}
}

// NewBoundFunction implements BoundFunctionCreate plus the `length`/`name`
// steps of Function.prototype.bind, so the bind method is a thin wrapper.
func (r *Realm) NewBoundFunction(target *Object, boundThis Value, boundArgs []Value) (*Object, error) {
	if !target.IsCallable() {
		return nil, r.TypeError("Bind must be called on a function")
	}
	proto, err := r.getPrototypeOf(target)
	if err != nil {
		return nil, err
	}
	length := IntValue(0)
	hasLength, err := r.hasOwnProperty(target, lengthKey)
	if err != nil {
		return nil, err
	}
	if hasLength {
		lv, err := target.GetProp(r, lengthKey)
		if err != nil {
			return nil, err
		}
		if lv.IsNumber() {
			switch f := lv.AsNumber(); {
			case f == posInf:
				length = NumberValue(posInf)
			case f == negInf || f != f:
				length = IntValue(0)
			default:
				length = NumberValue(max(0, ToIntegerOrInfinityFloat(f)-float64(len(boundArgs))))
			}
		}
	}
	nameKey := StringKey(AtomName)
	nv, err := target.GetProp(r, nameKey)
	if err != nil {
		return nil, err
	}
	targetName := AtomEmpty
	if nv.IsString() {
		targetName = nv.AsString()
	}
	name, err := r.Concat(AtomBoundSpace, targetName)
	if err != nil {
		return nil, err
	}
	bo := &boundFuncObject{}
	r.chargeObject(unsafe.Sizeof(boundFuncObject{}))
	o, fd := &bo.obj, &bo.fd
	r.initFunction(o, fd, bo.slots[:], name, 0, FuncBound)
	o.slots[0] = length
	bo.bound.target = target
	fd.thisValue = boundThis
	if len(boundArgs) != 0 {
		bo.bound.args = append([]Value(nil), boundArgs...)
	}
	if proto != r.FunctionPrototype {
		if proto != nil {
			r.markPrototype(proto)
		}
		o.shape = r.functionShape().rebase(r, r.rootShapeFor(proto))
	}
	return o, nil
}

// OrdinaryCreateFromConstructor creates an object whose prototype is
// newTarget.prototype when that is an object, else defaultProto.
func (r *Realm) OrdinaryCreateFromConstructor(newTarget *Object, defaultProto *Object, class Class) (*Object, error) {
	proto, err := r.GetPrototypeFromConstructor(newTarget, nil, defaultProto)
	if err != nil {
		return nil, err
	}
	r.markPrototype(proto)
	return r.newObject(class, r.rootShapeFor(proto)), nil
}

// constructThis is OrdinaryCreateFromConstructor(newTarget,
// "%Object.prototype%") for the [[Construct]] of a bytecode function: the
// object has room for the named properties the last objects constructed
// with newTarget ended up with (FunctionData.ctorSlots, noteCtorSlots), so
// a constructor that assigns them one by one does not grow its slots, and
// from the ctorProbe-th on for those most of them went on to take
// (probeCtor).
//
// newTarget.prototype is read each time. An ordinary function whose shape
// is still the one it was created with (newClosure: length, name and the
// writable, lazily created prototype) has it in that shape's slot; any
// other shape, a prototype that is not an object and the lazy marker go
// through [[Get]].
func (r *Realm) constructThis(newTarget *Object) (*Object, error) {
	var proto *Object
	if s := newTarget.shape; s.parent == r.funcShape && s.key == StringKey(AtomPrototype) && s.attrs == attrWritable|attrLazy {
		if v := newTarget.slots[s.slot]; v.IsObject() {
			proto = v.AsObject()
		}
	}
	if proto == nil {
		p, err := r.GetPrototypeFromConstructor(newTarget, nil, r.ObjectPrototype)
		if err != nil {
			return nil, err
		}
		proto = p
	}
	r.markPrototype(proto)
	root := r.rootShapeFor(proto)
	if fd := newTarget.FunctionData(); fd != nil {
		if fd.ctorSlots < ctorProbe<<4 {
			fd.probeCtor(r, root)
		}
		if n := fd.ctorSlots & ctorSlotsMask; n != 0 {
			return initObject(r.NewObjectCap(int(n)), ClassObject, root), nil
		}
	}
	return r.newObject(ClassObject, root), nil
}

// noteCtorSlots records in fd, the new.target of a [[Construct]] that
// returned v, how many named properties v has.
func noteCtorSlots(fd *FunctionData, v Value) {
	if !v.IsObject() {
		return
	}
	if n := len(v.AsObject().slots); n > int(fd.ctorSlots&ctorSlotsMask) && n <= smallObjectMax {
		fd.ctorSlots = fd.ctorSlots&^ctorSlotsMask | uint8(n)
	}
}

// probeCtor counts one of the first ctorProbe objects constructed with fd,
// whose root shape is root (constructThis). The last of them looks at
// where the objects before it went on past the properties they were
// constructed with (Shape.ctorRoom): where at least half of them grew
// their slots there, as a tree's nodes do when they take their children,
// it and the next ones get the room those took, up to smallObjectMax, and
// do not grow. Objects that stay as they were constructed get none.
func (fd *FunctionData) probeCtor(r *Realm, root *Shape) {
	c := fd.ctorSlots + 1<<4
	if own := int(c & ctorSlotsMask); c >= ctorProbe<<4 && root != r.plainRoot && !root.shared {
		if m, grew := root.ctorRoom(own); m > own && 2*grew >= ctorProbe-1 {
			c = c&^ctorSlotsMask | uint8(min(m, smallObjectMax))
		}
	}
	fd.ctorSlots = c
}

// ctorSlotsMask selects the room in FunctionData.ctorSlots; ctorProbe is
// the object of a constructor that looks at where the ones before it went
// on (noteCtorSlots), counted in the bits above.
const (
	ctorSlotsMask = 1<<4 - 1
	ctorProbe     = 15
)

// GetPrototypeFromConstructor implements GetPrototypeFromConstructor
// (ECMA-262 §10.1.14) for the [[Construct]] of a builtin constructor ctor:
// the prototype of the object it creates is newTarget.prototype when that
// is an object, else intrinsicDefault (the realm's own intrinsic: moejs has
// no cross-realm functions). This is what makes builtins subclassable: `new
// Sub()` for `class Sub extends Array` reaches arrayConstruct with newTarget
// Sub. newTarget nil (a [[Call]]) or ctor itself returns intrinsicDefault
// without a property read, so constructing the builtin directly pays
// nothing (ctor.prototype is intrinsicDefault and cannot change).
func (r *Realm) GetPrototypeFromConstructor(newTarget, ctor, intrinsicDefault *Object) (*Object, error) {
	if newTarget == nil || newTarget == ctor {
		return intrinsicDefault, nil
	}
	pv, err := newTarget.GetProp(r, StringKey(AtomPrototype))
	if err != nil {
		return nil, err
	}
	if pv.IsObject() {
		return pv.AsObject(), nil
	}
	if err := r.checkFunctionRealm(newTarget); err != nil {
		return nil, err
	}
	return intrinsicDefault, nil
}

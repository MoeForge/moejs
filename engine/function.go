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
	FuncNative FuncKind = iota
	FuncBytecode
	FuncBound
	FuncNativeData // a NativeDataFunc and its payload
	FuncProxy      // a callable proxy: dataFn is proxyCall, data the *proxyData
)

// FunctionData is the internal payload of ClassFunction objects.
// Fields used by one kind only share the struct with the others, and state
// only some functions of a kind have lives in data: at 96 bytes the payload
// fills a funcObject's 240-byte size class exactly and a nativeFuncObject's
// 224-byte one.
type FunctionData struct {
	native    NativeFunc
	code      *bytecode.Function
	env       *Env
	thisValue Value // captured this of an arrow function; the bound this of a bound function
	realm     *Realm
	name      *String
	meta      *funcMeta // interpreter: materialized constants of code (interp.go)
	// data is the state of the kind: the payload of a data native, the
	// NativeCtor of a native constructor, the [[HomeObject]] of a method,
	// the *boundFunc of a bound function.
	data   any
	dataFn NativeDataFunc // behaviour of a data native
	icBase uint32         // interpreter: base of this function's inline caches in realm.ic
	kind   FuncKind       // after icBase so the two share a word
	// plain is set on a closure whose entry binds only its parameters: no
	// rest parameter, arguments object or captured binding (enterFrame).
	plain bool
	// ctorSlots is the most named properties (up to smallObjectMax) an
	// object constructed with this function as new.target ended its
	// construction with: the next one is allocated with room for them.
	ctorSlots uint8
	// measurable is set on a host's functions, whose values kept for later
	// go to AddLiveRoots or LiveReporter, and on the Promise constructor: a
	// measurement may run in the JavaScript they call back (measurable). It
	// shares ctorSlots' word.
	measurable bool
}

// boundFunc is the state of a bound function beyond its bound this.
type boundFunc struct {
	target *Object
	args   []Value
}

// bound returns the state of a bound function.
func (fd *FunctionData) bound() *boundFunc { return fd.data.(*boundFunc) }

// ctor returns the [[Construct]] behaviour of a native constructor.
func (fd *FunctionData) ctor() NativeCtor { return fd.data.(NativeCtor) }

// setCtor records the [[Construct]] behaviour of a native function; nil
// makes it a plain function.
func (fd *FunctionData) setCtor(ctor NativeCtor) {
	fd.data = nil
	if ctor != nil {
		fd.data = ctor
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
	if _, ok := fd.data.(*resolvingFns); ok {
		return Undefined() // the promise it settles (resolvingFns)
	}
	return fd.thisValue
}

// HomeObject returns the [[HomeObject]] of a method (nil otherwise).
func (fd *FunctionData) HomeObject() *Object {
	if fd.kind != FuncBytecode {
		return nil
	}
	h, _ := fd.data.(*Object)
	return h
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
// prototype checks follow that realm's epoch, so calling or resuming it from
// r gives wrong results or a Go panic; a host keeps the objects of a realm
// inside it (engine-api.md, "One realm per value"). Dynamic code (eval, the
// Function constructors) binds its caches in the calling realm and is not
// foreign. A revoked proxy of r has no target and runs no code (using it is
// a TypeError), so it is not foreign. IsForeign does not see a foreign
// function reached through an object of another realm, the traps of a proxy
// that is not callable among them.
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
			fd, _ = o.internal.(*FunctionData)
			if fd != nil && fd.kind == FuncBound {
				b, _ := fd.data.(*boundFunc)
				if b == nil {
					return false
				}
				o = b.target
				continue
			}
		case ClassProxy:
			// A callable proxy's state hangs off its function payload; a
			// module namespace has none.
			var pd *proxyData
			if pf, ok := o.internal.(*FunctionData); ok {
				// A callable proxy of another realm runs that realm's
				// traps, whatever its target: foreign.
				if pf.realm != nil && pf.realm != r {
					return true
				}
				pd, _ = pf.data.(*proxyData)
			} else {
				pd, _ = o.internal.(*proxyData)
			}
			if pd == nil || pd.target == nil {
				return false
			}
			o = pd.target
			continue
		case ClassGenerator:
			if g, _ := o.internal.(*genFrame); g != nil {
				fd = g.fd
			}
		case ClassAsyncGenerator:
			if g, _ := o.internal.(*asyncGenerator); g != nil {
				fd = g.g.fd
			}
		default:
			return false
		}
		return fd != nil && fd.kind == FuncBytecode && fd.realm != r && fd.realm != nil && fd.meta.dyn == 0
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
func (fd *FunctionData) Native() NativeFunc { return fd.native }

// Data returns the payload of a data native (nil otherwise).
func (fd *FunctionData) Data() any {
	if fd.kind != FuncNativeData {
		return nil
	}
	return fd.data
}

// IsConstructor reports whether the function has [[Construct]].
func (fd *FunctionData) IsConstructor() bool {
	switch fd.kind {
	case FuncNative:
		return fd.data != nil
	case FuncBound:
		return fd.bound().target.internal.(*FunctionData).IsConstructor()
	case FuncProxy:
		return fd.data.(*proxyData).ctor
	}
	return fd.code != nil && (fd.code.Kind == bytecode.KindNormal || fd.code.Kind.IsClassCtor())
}

// funcObject co-allocates a function object, its payload and the slots of
// its three intrinsic properties (length, name and, for ordinary functions,
// prototype), so a closure or builtin function costs one allocation.
type funcObject struct {
	obj   Object
	fd    FunctionData
	slots [3]Value
}

// nativeFuncObject is the funcObject of a function without a `prototype`
// (plain and data natives): two slots, 224 bytes instead of 240.
type nativeFuncObject struct {
	obj   Object
	fd    FunctionData
	slots [2]Value
}

// boundFuncObject co-allocates a bound function with its boundFunc.
type boundFuncObject struct {
	obj   Object
	fd    FunctionData
	bound boundFunc
	slots [2]Value
}

// Env is a closure environment: captured bindings of one scope.
type Env struct {
	parent *Env
	slots  []Value
}

// NewEnv creates an environment with n slots initialized to undefined.
func NewEnv(parent *Env, n int) *Env {
	e := &Env{parent: parent, slots: make([]Value, n)}
	u := Undefined()
	for i := range e.slots {
		e.slots[i] = u
	}
	return e
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

// Len returns the slot count.
func (e *Env) Len() int { return len(e.slots) }

// Slot reads slot i.
func (e *Env) Slot(i int) Value { return e.slots[i] }

// SetSlot writes slot i.
func (e *Env) SetSlot(i int, v Value) { e.slots[i] = v }

// Slots exposes the slot storage for the interpreter.
func (e *Env) Slots() []Value { return e.slots }

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
	return m.slots[i], true
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

// MaxCallDepth is the frame limit before "Maximum call stack size exceeded".
const MaxCallDepth = 512

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
	fd, _ := v.AsObject().internal.(*FunctionData)
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
	fd := fn.internal.(*FunctionData)
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
		res, err = fd.native(r, this, args)
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
	return fd.dataFn(r, fd, this, args)
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
	fd := f.internal.(*FunctionData)
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
		res, err = r.proxyConstruct(fd.data.(*proxyData), args, newTarget)
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

// newFunctionObject allocates a function object with the realm's shared
// function shape (length, name) and room for a third property.
func (r *Realm) newFunctionObject(name *String, length int, kind FuncKind) (*Object, *FunctionData) {
	fo := &funcObject{}
	r.chargeObject(unsafe.Sizeof(funcObject{}))
	r.initFunction(&fo.obj, &fo.fd, fo.slots[:2:3], name, length, kind)
	return &fo.obj, &fo.fd
}

// newNativeFunctionObject is newFunctionObject for a function that has no
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
	o.proto = r.FunctionPrototype
	o.class = ClassFunction
	o.flags = flagExtensible
	o.internal = fd
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
	fn.internal.(*FunctionData).name = name
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
	fd.native = fn
	return o
}

// NewHostFunction is NewNativeFunction for a host's function. The
// measurement of a charge past the memory limit runs in the JavaScript it
// calls back, as in plain code: the host keeps no JavaScript value only in
// Go across such a call, or registers what it keeps (AddLiveRoots,
// LiveReporter).
func (r *Realm) NewHostFunction(name *String, length int, fn NativeFunc) *Object {
	o, fd := r.newNativeFunctionObject(name, length, FuncNative)
	fd.native = fn
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
	return fd.measurable || fd.kind == FuncNative && measurableBuiltins[reflect.ValueOf(fd.native).Pointer()]
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
	fd.dataFn = fn
	fd.data = data
	return o
}

// NewNativeConstructor creates a builtin constructor with both [[Call]] and
// [[Construct]] behaviour. call may be nil, in which case calling without
// `new` throws a TypeError.
func (r *Realm) NewNativeConstructor(name *String, length int, call NativeFunc, ctor NativeCtor) *Object {
	o, fd := r.newFunctionObject(name, length, FuncNative)
	if call == nil {
		call = func(r *Realm, this Value, args []Value) (Value, error) {
			return Undefined(), r.TypeError("Constructor %s requires 'new'", name.GoString())
		}
	}
	fd.native = call
	fd.setCtor(ctor)
	return o
}

// SetHomeObject records the [[HomeObject]] of a method.
func (fd *FunctionData) SetHomeObject(o *Object) { fd.data = o }

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
	fd.data = &bo.bound
	if proto != r.FunctionPrototype {
		o.proto = proto
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
// a constructor that assigns them one by one does not grow its slots.
func (r *Realm) constructThis(newTarget *Object) (*Object, error) {
	proto, err := r.GetPrototypeFromConstructor(newTarget, nil, r.ObjectPrototype)
	if err != nil {
		return nil, err
	}
	r.markPrototype(proto)
	if fd := newTarget.FunctionData(); fd != nil && fd.ctorSlots != 0 {
		return initObject(r.NewObjectCap(int(fd.ctorSlots)), ClassObject, r.rootShapeFor(proto)), nil
	}
	return r.newObject(ClassObject, r.rootShapeFor(proto)), nil
}

// noteCtorSlots records in fd, the new.target of a [[Construct]] that
// returned v, how many named properties v has.
func noteCtorSlots(fd *FunctionData, v Value) {
	if !v.IsObject() {
		return
	}
	if n := len(v.AsObject().slots); n > int(fd.ctorSlots) && n <= smallObjectMax {
		fd.ctorSlots = uint8(n)
	}
}

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

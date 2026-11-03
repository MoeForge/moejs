package engine

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unicode/utf8"
	"unsafe"
	"weak"

	"github.com/MoeForge/moejs/bytecode"
)

// Interpreter entry points and per-realm interpreter state.
//
// Frames are windows into one contiguous per-realm register stack; a call
// allocates nothing when the callee's registers fit. The stack grows by
// doubling and copying, so no Go pointer into it may be held across a call:
// the run loop re-slices its register window after every operation that can
// re-enter the interpreter.

// interpState is the interpreter's per-realm mutable state (embedded in Realm).
type interpState struct {
	stack   []Value // register stack; frames are windows [base, base+NumRegs); length: the extent used (growStack)
	sp      int     // first free register
	frames  []frameInfo
	nframes int
}

// frameInfo is the compact per-frame record used for stack capture: the
// running function, the pc of its current instruction and its register
// window base (interp_stack.go reads the caller's call instruction there).
type frameInfo struct {
	fn   *Object
	pc   uint32
	base uint32
}

const (
	initialStackSize = 64 // 1 KiB of registers; doubles on demand
	initialFrames    = 16
)

// funcMeta holds the process-wide materialized form of one compiled
// function: constants as engine values and property keys, and the metas of
// its children (parallel to Function.Children). It is built once per
// bytecode.Function and shared by every realm (strings are immutable and
// pre-hashed; keys are process-wide atoms). It holds no strong reference to
// the template so that the cache below cannot keep dead templates alive.
// Code compiled from a string has a realm-private tree instead (dynMeta).
type funcMeta struct {
	consts   []Value       // ConstNumber/ConstString/ConstBigInt materialized, ConstRegExp: the pattern; zero for other kinds
	keys     []PropertyKey // ConstString with Key set: interned property key
	names    []*String     // ConstString: the string (for messages and TDZ errors); ConstRegExp: the flags
	children []*funcMeta
	name     *String   // interned function name ("" -> AtomEmpty)
	root     *funcMeta // root of the tree (the root itself)
	index    uint32    // pre-order index in the tree: this function's IC base in Realm.icTrees
	nfuncs   uint32    // root only: functions in the tree
	totalICs uint32    // root only: inline-cache slots of the whole tree
	dyn      uint32    // dynamic code: dynFlag and the base+1 of the function's inline caches (dynamic.go); zero otherwise
}

// funcMetas caches materialized function trees by root template. Keys are
// weak so a template that is no longer referenced (a RunString script, a
// recompiled plugin) is collected; a cleanup drops its entry.
var funcMetas sync.Map // weak.Pointer[bytecode.Function] -> *funcMeta

// metaFor returns the materialized tree for root, building it on first use.
// A tree with a function that fails bytecode.VerifyRegisters is an error,
// which nothing caches.
func (r *Realm) metaFor(root *bytecode.Function) (*funcMeta, error) {
	k := weak.Make(root)
	if m, ok := funcMetas.Load(k); ok {
		return m.(*funcMeta), nil
	}
	n := uint32(0)
	m, err := r.materialize(root, nil, &n)
	if err != nil {
		return nil, err
	}
	m.nfuncs = n
	actual, loaded := funcMetas.LoadOrStore(k, m)
	if !loaded {
		runtime.AddCleanup(root, func(k weak.Pointer[bytecode.Function]) { funcMetas.Delete(k) }, k)
	}
	return actual.(*funcMeta), nil
}

// materialize builds the funcMeta of code and its children in the tree of
// root (nil: code is the root), numbering the templates pre-order through
// next.
func (r *Realm) materialize(code *bytecode.Function, root *funcMeta, next *uint32) (*funcMeta, error) {
	m := &funcMeta{}
	if root == nil {
		root = m
	}
	return m, r.fillMeta(m, code, root, next)
}

// fillMeta is materialize into m, a dynamic tree's when root is.
//
// It verifies code first (bytecode.VerifyRegisters): run indexes its
// registers without bounds checks (window), which is sound only for the
// registers and instructions the verifier checked. Every function the
// engine can run is materialized here, before any of its instructions
// runs: metaFor's trees (modules and their graphs, scripts, Realm.RunScript,
// EvaluateModule, newClosure's callers, Realm.AllocIC) and dynMeta's
// (indirect and direct eval, Realm.EvalScript and the Function,
// GeneratorFunction, AsyncFunction and AsyncGeneratorFunction
// constructors), each function of a tree with its children
// (materialize). A template that fails is a compiler bug or a hand-made
// bytecode.Function: its tree is an error, returned before the caller
// changes anything (a declaration, an instance, a cache), and none of its
// code runs.
func (r *Realm) fillMeta(m *funcMeta, code *bytecode.Function, root *funcMeta, next *uint32) error {
	if err := bytecode.VerifyRegisters(code); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	m.root, m.index = root, *next
	m.dyn = root.dyn & dynFlag
	*next++
	root.totalICs += code.ICCount
	m.name = AtomEmpty
	if code.Name != "" {
		if utf8.ValidString(code.Name) {
			m.name = r.InternGoString(code.Name)
		} else {
			m.name = r.internUTF16(fromWTF8Text(code.Name)) // a name with a lone surrogate (String.wtf8)
		}
		prepareSharedString(m.name)
	}
	if n := len(code.Consts); n > 0 {
		m.consts = make([]Value, n)
		m.keys = make([]PropertyKey, n)
		m.names = make([]*String, n)
	}
	for i, k := range code.Consts {
		switch k.Kind {
		case bytecode.ConstNumber:
			m.consts[i] = NumberValue(k.Num)
		case bytecode.ConstString:
			var s *String
			if k.IsASCII {
				s = asciiString(k.Str)
			} else {
				s = FromUTF16(k.Units)
			}
			// A short ASCII constant of shared code is an atom marked as a
			// constant (constASCIIAtom) where it can be: a computed key it
			// names (o[k] over ["url", "weight"]) then needs no interning.
			var a *String
			if k.IsASCII && m.dyn == 0 {
				a = constASCIIAtom(k.Str)
			}
			if k.Key {
				if a != nil {
					s = a
				} else {
					s = r.Intern(s)
				}
				m.keys[i] = StringKey(s)
			} else if a != nil {
				s = a
			} else if m.dyn == 0 && s != emptyString {
				// Every realm running the template shares it, and none
				// charged it: the live measurement skips it (memlive.go).
				s.flags |= strConst
			}
			prepareSharedString(s)
			m.names[i] = s
			m.consts[i] = StringValue(s)
		case bytecode.ConstBigInt:
			b, ok := newBigIntConst(k.Str)
			if !ok {
				b = NewBigIntFromInt64(0)
			}
			m.consts[i] = BigIntValue(b)
		case bytecode.ConstRegExp:
			// Pattern and flags are materialized once per template so a
			// regular expression literal costs only its RegExp object. The
			// pattern is an atom, whose bytes the realm's RegExp cache keeps
			// as its key (compileRegExp).
			pattern, flags := r.Intern(fromWTF8Text(k.Str)), FromGoString(k.Flags)
			prepareSharedString(pattern)
			prepareSharedString(flags)
			m.consts[i] = StringValue(pattern)
			m.names[i] = flags
		}
	}
	if m.keys != nil {
		addCodeKeys(m.keys)
	}
	if len(code.Children) > 0 {
		m.children = make([]*funcMeta, len(code.Children))
		for i, c := range code.Children {
			var err error
			if m.children[i], err = r.materialize(c, root, next); err != nil {
				return err
			}
		}
	}
	return nil
}

func init() {
	constructFunction = interpConstruct
	defaultStackCapture = captureStack
	defaultStackCaptureInto = captureStackInto
	defaultStackFormat = formatStack
}

// defaultStackCapture / defaultStackCaptureInto / defaultStackFormat are
// installed on every new realm by the realm constructors (realm.go).
var (
	defaultStackCapture     func(r *Realm) []StackFrame
	defaultStackCaptureInto func(r *Realm, buf []StackFrame, skip *Object, limit int) []StackFrame
	defaultStackFormat      func(r *Realm, frames []StackFrame) string
)

// ErrModulePending is EvaluateModule's error for a module with top-level
// await whose evaluation is still pending once the job queue has drained:
// it awaits a promise nothing settles.
var ErrModulePending = errors.New("engine: module evaluation is still pending after the job queue drained")

// EvaluateModule instantiates and runs a compiled module in r and returns
// its environment, whose slots back the live export bindings, or nil when
// the evaluation failed before the module's body started (an interrupt
// pending on entry), so no binding was initialized. The compiled
// template is shared: only per-realm state (function objects, the module
// Env, inline caches) is created here. A module with top-level await
// evaluates asynchronously; the job queue drains before an outermost
// EvaluateModule returns, and its error is then ModuleEvaluationError's.
func (r *Realm) EvaluateModule(code *bytecode.Function) (*ModuleEnv, error) {
	menv, p, err := r.EvaluateModuleAsync(code)
	if p == nil {
		return menv, err
	}
	return menv, r.ModuleEvaluationError(p, err)
}

// ModuleEvaluationError returns the error of the evaluation of a module
// with top-level await, whose promise EvaluateModuleAsync returned, once
// the jobs ran: the rejection reason as an *Exception, else err, the jobs'
// error (see ReleaseJobs), else ErrModulePending while p is pending. The
// rejection wins over err as a call's own exception does over its jobs',
// and an interrupt in err over the rejection, as over a call's exception.
func (r *Realm) ModuleEvaluationError(p *Object, err error) error {
	_, interrupted := err.(*InterruptedError)
	switch state, v, _ := p.PromiseResult(); {
	case state == PromiseRejected && !interrupted:
		return r.Throw(v)
	case err != nil:
		return err
	case state == PromisePending:
		return ErrModulePending
	}
	return nil
}

// EvaluateModuleAsync is EvaluateModule returning the promise of the
// evaluation of a module with top-level await (code.Async), as the job
// queue's drain left it, instead of its outcome. The promise is nil for any
// other module, which evaluates synchronously, and when an interrupt stops
// the evaluation before it starts, which leaves no environment either.
func (r *Realm) EvaluateModuleAsync(code *bytecode.Function) (*ModuleEnv, *Object, error) {
	if code.Kind != bytecode.KindModule {
		return nil, nil, errors.New("engine: EvaluateModule requires a module template")
	}
	if code.Module.Links != nil {
		return nil, nil, errors.New("engine: a module that requests other modules or uses import() or import.meta must be linked (LinkModules) and evaluated as a graph (EvaluateGraph)")
	}
	meta, err := r.metaFor(code)
	if err != nil {
		return nil, nil, err
	}
	fn, env := r.instantiateTopLevel(code, meta)
	res, err := r.CallObject(fn, Undefined(), nil)
	if err != nil && fn.internal.(*FunctionData).icBase == icUnbound {
		// The inline caches are still unbound: enterFrame did not run the
		// body.
		return nil, nil, err
	}
	menv := &ModuleEnv{Env: env, module: code.Module}
	if code.Async && isPromise(res) {
		return menv, res.AsObject(), err
	}
	if code.Async && err == nil {
		err = r.noBodyPromise()
	}
	return menv, nil, err
}

// RunScript runs a compiled script and returns its completion value.
// GlobalDeclarationInstantiation runs first (sloppy.go): its var and
// function declarations become properties of the global object and its let,
// const and class declarations bindings of the realm's global declarative
// environment, which later scripts and modules resolve names against too.
// A failing check creates nothing and returns its SyntaxError or TypeError.
func (r *Realm) RunScript(code *bytecode.Function) (Value, error) {
	if code.Kind != bytecode.KindScript {
		return Undefined(), errors.New("engine: RunScript requires a script template")
	}
	meta, err := r.metaFor(code)
	if err != nil {
		return Undefined(), err
	}
	if err := r.globalDeclarationInstantiation(code); err != nil {
		return Undefined(), err
	}
	fn, _ := r.instantiateTopLevel(code, meta)
	return r.CallObject(fn, ObjectValue(r.Global), nil)
}

// instantiateTopLevel creates the function object and environment of a
// module or script template whose tree is meta (metaFor).
func (r *Realm) instantiateTopLevel(code *bytecode.Function, meta *funcMeta) (*Object, *Env) {
	env := NewEnv(nil, len(code.CaptureLayout))
	fn, fd := r.newClosure(code, meta, env, Undefined())
	return fn, fd.env
}

// protoObject co-allocates the `prototype` object of an ordinary function
// with its single `constructor` slot.
type protoObject struct {
	obj   Object
	slots [1]Value
}

// newClosure creates a function object over code/meta closing over env; this
// is the lexical this for arrows. A KindNormal function gets its `prototype`
// property in the shape (so [[OwnPropertyKeys]] order and attributes are
// spec-exact) but the prototype object itself is created on first access:
// the slot holds the hole marker until materializePrototype replaces it
// (getOwnCell), which most plugin functions never trigger. The property
// carries attrLazy, which keeps the function's shape distinct from that of
// any ordinary object with the same keys (Object.create(Function.prototype)
// plus length, name and prototype) and keeps every slot read of it off the
// inline-cache fast path, so the marker can only be read through getOwnCell.
func (r *Realm) newClosure(code *bytecode.Function, meta *funcMeta, env *Env, this Value) (*Object, *FunctionData) {
	o, fd := r.newFunctionObjectCap(meta.name, code.Length, FuncBytecode, 3)
	fd.code = code
	fd.env = env
	fd.meta = meta
	fd.icBase = icUnbound
	fd.plain = !code.HasRest && !code.HasArguments && len(code.CaptureLayout) == 0
	fd.thisValue = Undefined()
	switch code.Kind {
	case bytecode.KindArrow:
		fd.thisValue = this
	case bytecode.KindNormal:
		o.shape = o.shape.addProperty(r, StringKey(AtomPrototype), attrWritable|attrLazy)
		o.slots = append(o.slots, Hole())
	}
	return o, fd
}

// materializePrototype creates the `prototype` object of an ordinary
// function whose slot still holds the lazy marker: an ordinary object with
// the function as its non-enumerable `constructor`, as MakeConstructor
// requires. Native functions never carry the marker (constructors get their
// prototype eagerly), so fd.realm is the creating realm.
func (o *Object) materializePrototype(slot *Value) {
	r := o.internal.(*FunctionData).realm
	po := &protoObject{}
	r.chargeObject(unsafe.Sizeof(protoObject{}))
	p := &po.obj
	p.shape = r.plainRoot.addProperty(r, StringKey(AtomConstructor), attrHidden)
	p.proto = r.ObjectPrototype
	p.class = ClassObject
	p.flags = flagExtensible
	po.slots[0] = ObjectValue(o)
	p.slots = po.slots[:1:1]
	*slot = ObjectValue(p)
}

// icUnbound is the icBase of a closure whose inline caches are not bound
// yet. enterFrame binds them on the first call, reserving them on the
// template's first call in the realm, so the functions of a program that
// never run cost no IC storage; closures of one template share them. The
// closures of dynamic code bind theirs through enterDynamic, and a reclaim
// round unbinds those no frame runs (dynamic.go).
const icUnbound = ^uint32(0)

// interpConstruct implements [[Construct]] for bytecode functions.
func interpConstruct(r *Realm, fn *Object, fd *FunctionData, args []Value, newTarget *Object) (Value, error) {
	if fd.code.NewTarget {
		return r.constructNT(fn, fd, args, newTarget)
	}
	obj, err := r.constructThis(newTarget)
	if err != nil {
		return Undefined(), err
	}
	res, err := r.enterFrame(fn, fd, ObjectValue(obj), args)
	if err != nil {
		return Undefined(), err
	}
	if !res.IsObject() {
		res = ObjectValue(obj)
	}
	if fn == newTarget {
		noteCtorSlots(fd, res)
	}
	return res, nil
}

// enterFrame reserves a register window, binds arguments and the closure
// environment, and runs the function. It allocates only when the stack or
// frame table must grow, when the function has captured bindings (its Env)
// or a rest parameter (its array). Nothing but run is called on the path of
// an ordinary call, so the arguments stay in registers: the interrupt
// check, the first call's IC binding and stack growth are enterFrameSlow's,
// a rest array, arguments object or captured binding enterFrameExtra's.
func (r *Realm) enterFrame(fn *Object, fd *FunctionData, this Value, args []Value) (Value, error) {
	code := fd.code
	st := &r.interp
	if r.interruptFlag.Load() != 0 || fd.icBase == icUnbound || st.sp+int(code.NumRegs) > len(st.stack) {
		if res, done, err := r.enterFrameSlow(fn, fd, this, args); done {
			return res, err
		}
	}
	base := st.sp
	top := base + int(code.NumRegs)
	regs := st.stack[base:top:top]
	np := int(code.NumParams)
	n := min(np, len(args))
	dst, src := regs[:n], args[:n]
	for i := range src {
		dst[i] = src[i]
	}
	u := Undefined()
	for i := n; i < np; i++ {
		regs[i] = u
	}
	env := fd.env
	if !fd.plain {
		env = r.enterFrameExtra(code, fd.env, regs, args)
	}
	if code.Kind == bytecode.KindArrow {
		this = fd.thisValue
	}
	fi := st.nframes
	if fi == len(st.frames) {
		r.growFrames()
	}
	st.frames[fi] = frameInfo{fn: fn, base: uint32(base)}
	st.nframes = fi + 1
	st.sp = top
	res, err := r.run(fi, fd, base, env, this, fn)
	st.sp = base
	st.nframes = fi
	return res, err
}

// enterFrameSlow prepares enterFrame's frame when an interrupt is pending,
// the closure's inline caches are unbound or the register stack must grow.
// done reports a call it finished itself: the interrupt's error, or the
// whole call of a closure of dynamic code (enterDynamic). It returns to
// enterFrame rather than calling it, so a first call is no deeper on the Go
// stack than the next ones.
func (r *Realm) enterFrameSlow(fn *Object, fd *FunctionData, this Value, args []Value) (res Value, done bool, err error) {
	if r.interruptFlag.Load() != 0 {
		if err := r.pendingInterrupt(); err != nil {
			return Undefined(), true, err
		}
	}
	if fd.icBase == icUnbound {
		if fd.meta.dyn != 0 {
			res, err = r.enterDynamic(fn, fd, this, args)
			return res, true, err
		}
		fd.icBase = r.icBaseFor(fd.meta, fd.code.ICCount)
	}
	if top := r.interp.sp + int(fd.code.NumRegs); top > len(r.interp.stack) {
		r.growStack(top)
	}
	return Undefined(), false, nil
}

// enterFrameExtra binds the rest array and the arguments object of a frame
// and returns its environment: a new one when the function has captured
// bindings, else env.
func (r *Realm) enterFrameExtra(code *bytecode.Function, env *Env, regs, args []Value) *Env {
	np := int(code.NumParams)
	if code.HasRest {
		var rest []Value
		if len(args) > np {
			rest = make([]Value, len(args)-np)
			copy(rest, args[np:])
		}
		regs[np] = ObjectValue(r.NewArrayFromSlice(rest))
	}
	if code.HasArguments {
		i := np
		if code.HasRest {
			i++
		}
		regs[i] = ObjectValue(r.newArguments(args))
	}
	if layout := code.CaptureLayout; len(layout) > 0 && code.Kind != bytecode.KindModule && code.Kind != bytecode.KindScript {
		env = newEnvSized(env, len(layout))
		r.charge(int(unsafe.Sizeof(Env{})) + len(layout)*valueSize)
		for i, reg := range layout {
			if reg != bytecode.NoRegister {
				env.slots[i] = regs[reg]
			}
		}
	}
	return env
}

// growFrames doubles the frame table.
func (r *Realm) growFrames() {
	st := &r.interp
	nf := make([]frameInfo, max(initialFrames, 2*len(st.frames)))
	copy(nf, st.frames)
	st.frames = nf
}

// Small environments are co-allocated with their slot storage so that a
// closure scope costs one allocation.
type env1 struct {
	e   Env
	buf [1]Value
}

type env2 struct {
	e   Env
	buf [2]Value
}

type env4 struct {
	e   Env
	buf [4]Value
}

// newEnvSized is NewEnv with inline storage for up to four slots.
func newEnvSized(parent *Env, n int) *Env {
	var e *Env
	switch {
	case n <= 1:
		x := &env1{}
		x.e.slots = x.buf[:n:n]
		e = &x.e
	case n == 2:
		x := &env2{}
		x.e.slots = x.buf[:n:n]
		e = &x.e
	case n <= 4:
		x := &env4{}
		x.e.slots = x.buf[:n:n]
		e = &x.e
	default:
		return NewEnv(parent, n)
	}
	e.parent = parent
	u := Undefined()
	for i := range e.slots {
		e.slots[i] = u
	}
	return e
}

// pushArgs reserves n registers above the live frames for the callback
// arguments of a builtin that calls back into JavaScript repeatedly (map,
// filter, sort, ...): a Go array would escape to the heap through the
// indirect call. The callee's frame starts above the window, so the window
// is copied out before it can be reused; a stale slice after the stack has
// grown is still valid (the old storage stays alive while referenced).
// popArgs releases the window.
func (r *Realm) pushArgs(n int) []Value {
	st := &r.interp
	if st.sp+n > len(st.stack) {
		r.growStack(st.sp + n)
	}
	args := st.stack[st.sp : st.sp+n : st.sp+n]
	st.sp += n
	return args
}

func (r *Realm) popArgs(n int) { r.interp.sp -= n }

// windowRegs is the size of the array through which run indexes a frame's
// registers: every register operand (a uint8) indexes it without a bounds
// check.
const windowRegs = bytecode.MaxRegister + 1

// window returns the registers from base as an array of windowRegs, which
// run indexes with the uint8 register operands and no bounds checks. Only
// the frame's first NumRegs registers are its own: past them the array
// may reach other frames' registers or past the end of the stack's
// allocation. An access is inside the allocation as long as it is below
// NumRegs and base+NumRegs <= cap(st.stack); run keeps the stronger
// base+NumRegs <= len(st.stack), for three reasons the checked build of
// run (regcheck.go) tests on every access:
//
//   - Every register operand and run of registers of the frame's bytecode
//     lies below NumRegs, and run executes no other instructions than
//     those: bytecode.VerifyRegisters, which fillMeta runs on every
//     function the engine can execute, checks the registers of every
//     instruction it decodes from pc 0, that every jump, handler and
//     module body lands on one of them, and that the pcs the engine saves
//     to resume or rerun a frame at, the next instruction's, fit beside
//     the environment depth (bytecode.PCBits). A register index run
//     computes (a+1 of a call block, a handler's register) lies in the
//     verified runs too and is checked against windowRegs besides.
//   - The frame's registers exist: whoever enters a frame grows the stack
//     to base+NumRegs first (callValue and enterFrame on a call, a native's
//     call back into JavaScript, resumeFrame for a generator or an async
//     function, module and script bodies), and nothing shrinks it below a
//     live frame (ReleaseCallData runs between calls only).
//   - The window is current: run takes it again after every operation
//     that may grow, and so move, the stack, as it re-sliced its registers
//     when they were a slice.
//
// The conversion makes a pointer to more memory than may be allocated
// there, which checkptr (on under -race) rejects. window is exempt from
// checkptr, and nothing else: a //go:nocheckptr function is not inlined
// when checkptr is on, so the exemption stays in this function, run
// keeps its instrumentation, and only checkptr builds pay for the call.
// Builds without checkptr inline it.
//
//go:nocheckptr
func (st *interpState) window(base int) *[windowRegs]Value {
	return (*[windowRegs]Value)(unsafe.Pointer(unsafe.SliceData(st.stack[base:])))
}

// growStack makes the register stack hold at least top registers. Its
// length is the extent the calls since the last ReleaseCallData have used,
// so the release clears no more than that (a pointer store, a register
// cleared included, costs a write barrier while the collector marks);
// between the length and the capacity every register is zero. growStack
// extends the length within the capacity and doubles the capacity past it.
func (r *Realm) growStack(top int) {
	st := &r.interp
	if top <= cap(st.stack) {
		st.stack = st.stack[:top]
		return
	}
	n := max(initialStackSize, 2*cap(st.stack))
	for n < top {
		n *= 2
	}
	ns := make([]Value, top, n)
	copy(ns, st.stack[:st.sp])
	st.stack = ns
}

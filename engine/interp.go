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
	consts   []Value       // ConstNumber/ConstString/ConstBigInt/ConstNull/ConstUndefined materialized, ConstRegExp: its index in regexps; zero for other kinds
	keys     []PropertyKey // consts as keys (constKeys): keys[i] is the interned property key of a ConstString with Key set
	regexps  []regexpConst // the ConstRegExp constants, in order; consts holds the index of each
	children []*funcMeta
	name     *String   // interned function name ("" -> AtomEmpty)
	root     *funcMeta // root of the tree (the root itself)
	index    uint32    // pre-order index in the tree: this function's IC base in Realm.icTrees
	nfuncs   uint32    // root only: functions in the tree
	totalICs uint32    // root only: inline-cache slots of the whole tree
	dyn      uint32    // dynamic code: dynFlag and the base+1 of the function's inline caches (dynamic.go); other code: its literal sites (nlit)
}

// nlit returns the count of m's array literals, whose sites take an
// inline-cache entry past the function's own (allocsite.go): none in a
// root or in dynamic code. The count is in dyn, which other code leaves
// zero.
func (m *funcMeta) nlit() uint32 {
	if m.dyn&dynFlag != 0 {
		return 0
	}
	return m.dyn
}

// regexpConst is the pattern and flags of a ConstRegExp constant, which
// the constant's consts slot indexes (fillMeta).
type regexpConst struct {
	pattern, flags *String
}

// regexpConst returns the pattern and flags of the ConstRegExp constant i
// of m.
func (m *funcMeta) regexpConst(i uint32) (pattern, flags *String) {
	c := &m.regexps[int(m.consts[i].AsNumber())]
	return c.pattern, c.flags
}

// constName returns the ConstString constant i of m, or nil when i is out
// of range or names no string.
func (m *funcMeta) constName(i uint32) *String {
	if int64(i) < int64(len(m.consts)) && m.consts[i].IsString() {
		return m.consts[i].AsString()
	}
	return nil
}

// A PropertyKey is a Value (propkey.go), so a function's constants double
// as its keys: the constant of a ConstString with Key set is the interned
// string StringKey wraps. The keys of other constants are never read. The
// only constant written after fillMeta is a ConstTemplate's of dynamic
// code (templateObject), which no instruction reads as a key.
var _ [0]struct{} = [unsafe.Sizeof(PropertyKey{}) - unsafe.Sizeof(Value{})]struct{}{}
var _ [0]struct{} = [unsafe.Sizeof(Value{}) - unsafe.Sizeof(PropertyKey{})]struct{}{}

// constKeys returns consts viewed as property keys.
func constKeys(consts []Value) []PropertyKey {
	return unsafe.Slice((*PropertyKey)(unsafe.Pointer(unsafe.SliceData(consts))), len(consts))
}

// metaSlab holds the funcMetas, children slices and constants of one tree,
// three allocations for the whole tree that fillMeta hands out in
// pre-order. The root reaches every meta of its tree, so no meta keeps
// memory alive that its tree would not.
type metaSlab struct {
	metas  []funcMeta
	kids   []*funcMeta
	consts []Value
}

// newMetaSlab returns the slab of the tree of code, with a meta for each of
// its functions but the root when the caller provides it (dynMeta).
func newMetaSlab(code *bytecode.Function, ownRoot bool) *metaSlab {
	var nf, nk, nc int
	var count func(f *bytecode.Function)
	count = func(f *bytecode.Function) {
		nf++
		nk += len(f.Children)
		nc += len(f.Consts)
		for _, c := range f.Children {
			count(c)
		}
	}
	count(code)
	if ownRoot {
		nf--
	}
	sl := &metaSlab{}
	if nf > 0 {
		sl.metas = make([]funcMeta, nf)
	}
	if nk > 0 {
		sl.kids = make([]*funcMeta, nk)
	}
	if nc > 0 {
		sl.consts = make([]Value, nc)
	}
	return sl
}

func (sl *metaSlab) meta() *funcMeta {
	m := &sl.metas[0]
	sl.metas = sl.metas[1:]
	return m
}

func (sl *metaSlab) children(n int) []*funcMeta {
	c := sl.kids[:n:n]
	sl.kids = sl.kids[n:]
	return c
}

func (sl *metaSlab) values(n int) []Value {
	c := sl.consts[:n:n]
	sl.consts = sl.consts[n:]
	return c
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
	sl := newMetaSlab(root, false)
	m := sl.meta()
	if err := r.fillMeta(m, root, m, &n, sl); err != nil {
		return nil, err
	}
	m.nfuncs = n
	actual, loaded := funcMetas.LoadOrStore(k, m)
	if !loaded {
		runtime.AddCleanup(root, func(k weak.Pointer[bytecode.Function]) { funcMetas.Delete(k) }, k)
	}
	return actual.(*funcMeta), nil
}

// fillMeta builds into m the funcMeta of code and its children in the
// tree of root, numbering the templates pre-order through next and taking
// their memory from sl. m is a dynamic tree's when root is.
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
// (fillMeta). A template that fails is a compiler bug or a hand-made
// bytecode.Function: its tree is an error, returned before the caller
// changes anything (a declaration, an instance, a cache), and none of its
// code runs.
func (r *Realm) fillMeta(m *funcMeta, code *bytecode.Function, root *funcMeta, next *uint32, sl *metaSlab) error {
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
		m.consts = sl.values(n)
		m.keys = constKeys(m.consts)
	}
	for i, k := range code.Consts {
		switch k.Kind {
		case bytecode.ConstNumber:
			m.consts[i] = NumberValue(k.Num)
		case bytecode.ConstNull:
			m.consts[i] = Null()
		case bytecode.ConstUndefined:
			m.consts[i] = Undefined()
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
				StringKey(s) // an atom: m.keys[i] is its key
			} else if a != nil {
				s = a
			} else if m.dyn == 0 && s != emptyString {
				// Every realm running the template shares it, and none
				// charged it: the live measurement skips it (memlive.go).
				s.flags |= strConst
			}
			prepareSharedString(s)
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
			m.consts[i] = IntValue(len(m.regexps))
			m.regexps = append(m.regexps, regexpConst{pattern, flags})
		}
	}
	if m.keys != nil {
		addConstKeys(m.keys, code.Consts)
	}
	if m.dyn == 0 && m != root {
		m.dyn = literalCount(code.Code)
	}
	if len(code.Children) > 0 {
		m.children = sl.children(len(code.Children))
		for i, c := range code.Children {
			m.children[i] = sl.meta()
			if err := r.fillMeta(m.children[i], c, root, next, sl); err != nil {
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
// The engine verifies a template once, on its first use, and then reads its
// code unchecked: a host must not modify a bytecode.Function it handed the
// engine.
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
	if err != nil && fn.funcData().icBase == icUnbound {
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
// The engine verifies a template once, on its first use, and then reads its
// code unchecked: a host must not modify a bytecode.Function it handed the
// engine.
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
// Generator functions and class constructors get theirs when the ops after
// the Closure complete them; the other kinds have none and take the
// two-slot closureObject.
func (r *Realm) newClosure(code *bytecode.Function, meta *funcMeta, env *Env, this Value) (*Object, *FunctionData) {
	var (
		o  *Object
		fd *FunctionData
	)
	length, name := IntValue(code.Length), StringValue(meta.name)
	if code.Kind == bytecode.KindNormal || code.Generator || code.Kind.IsClassCtor() {
		fo := &funcObject{}
		r.chargeObject(unsafe.Sizeof(funcObject{}))
		fo.slots[0], fo.slots[1] = length, name
		o, fd = &fo.obj, &fo.fd
		o.slots = fo.slots[:2:3]
	} else {
		fo := &closureObject{}
		r.chargeObject(unsafe.Sizeof(closureObject{}))
		fo.slots[0], fo.slots[1] = length, name
		o, fd = &fo.obj, &fo.fd
		o.slots = fo.slots[:]
	}
	o.shape = r.functionShape()
	o.class = ClassFunction
	o.flags = flagExtensible
	setData(o, kindFunction, fd)
	fd.kind = FuncBytecode
	fd.realm = r
	fd.name = meta.name
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
	r := o.funcData().realm
	po := &protoObject{}
	r.chargeObject(unsafe.Sizeof(protoObject{}))
	p := &po.obj
	p.shape = r.plainRoot.addProperty(r, StringKey(AtomConstructor), attrHidden)
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

// errICOutside is run's panic for a frame whose inline caches are not in
// the realm's table. A function of another realm, whose caches are bound
// in that realm's table (Realm.IsForeign), does not get there: its frame
// fails when it starts (errForeignFrame).
const errICOutside = "engine: a function runs with inline caches outside the realm's table (a function of another realm: Realm.IsForeign)"

// errForeignFrame returns the TypeError of a call or resumption, in r, of
// a bytecode function or generator another realm created (Realm.IsForeign),
// which fails before its frame starts. Its inline caches are bound in that
// realm's table, so it would read and write r's at their offsets: the
// frame starts (callValue, enterFrame, resumeFrame) send a function of
// another realm to the slow path, which returns this error instead. A
// bytecode function's realm is the realm that ran the code creating it, so
// the closures of dynamic code are their tree's realm's (dynamic.go).
func (r *Realm) errForeignFrame() error {
	return r.TypeError("a function or generator of another realm runs only in its own realm")
}

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
	if r.interruptFlag.Load() != 0 || fd.icBase == icUnbound || fd.realm != r || st.sp+int(code.NumRegs) > len(st.stack) {
		if res, done, err := r.enterFrameSlow(fn, fd, this, args); done {
			return res, err
		}
	}
	// The frame table grows first, while the stack pointer is below the
	// frame's registers, which hold what earlier frames left: the memory
	// measurement a charge may make reads registers up to it.
	fi := st.nframes
	if fi == len(st.frames) {
		r.growFrames()
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
	// The stack pointer is stored once, before enterFrameExtra raises it
	// over the arguments a frame keeps (ForwardArguments): top, set again
	// there, would be a value of two paths, which an ordinary call would
	// reload and jump over the other for.
	env := fd.env
	st.sp = top
	if !fd.plain {
		env = r.enterFrameExtra(code, fd.env, regs, args)
	}
	if code.Kind == bytecode.KindArrow {
		this = fd.thisValue
	}
	st.frames[fi] = frameInfo{fn: fn, base: uint32(base)}
	st.nframes = fi + 1
	res, err := r.run(fi, fd, base, env, this, fn)
	st.sp = base
	st.nframes = fi
	return res, err
}

// enterAt enters the frame of fd for callAt, at base: hb, the stack index
// of the call's argc arguments, for a call the compiler marked
// bytecode.CallOverlap, else the stack pointer, where enterFrame enters
// it with the arguments copied. A frame at hb has its parameters where
// the arguments are and the missing ones set to undefined in place; one
// with a rest parameter or an arguments object, which the entry makes
// from the arguments before it writes their registers, takes enterFrame's
// copy, as does a closure of dynamic code (enterFrameSlow). The stack
// pointer rises to the frame's registers only when they reach past it.
func (r *Realm) enterAt(fn *Object, fd *FunctionData, this Value, hb, argc, base int) (Value, error) {
	st := &r.interp
	code := fd.code
	if base != hb || code.HasRest || code.HasArguments {
		return r.enterFrame(fn, fd, this, st.stack[hb:hb+argc:hb+argc])
	}
	top := hb + int(code.NumRegs)
	if r.interruptFlag.Load() != 0 || fd.icBase == icUnbound || fd.realm != r || top > len(st.stack) {
		// The stack grows to the stack pointer and the frame's registers
		// past it, more than the frame at hb, below it, needs.
		if res, done, err := r.enterFrameSlow(fn, fd, this, st.stack[hb:hb+argc:hb+argc]); done {
			return res, err
		}
	}
	// The frame table grows first, while the stack pointer is the
	// caller's, as in enterFrame.
	fi := st.nframes
	if fi == len(st.frames) {
		r.growFrames()
	}
	regs := st.stack[hb:top:top]
	u := Undefined()
	for i := argc; i < int(code.NumParams); i++ {
		regs[i] = u
	}
	sp := st.sp
	st.sp = max(sp, top)
	env := fd.env
	if !fd.plain {
		env = r.enterFrameExtra(code, fd.env, regs, nil) // captured bindings only
	}
	if code.Kind == bytecode.KindArrow {
		this = fd.thisValue
	}
	st.frames[fi] = frameInfo{fn: fn, base: uint32(hb)}
	st.nframes = fi + 1
	res, err := r.run(fi, fd, hb, env, this, fn)
	st.sp = sp
	st.nframes = fi
	return res, err
}

// enterFrameSlow prepares enterFrame's frame when an interrupt is pending,
// the closure's inline caches are unbound, the closure is another realm's
// or the register stack must grow. done reports a call it finished itself:
// the interrupt's error, a closure of another realm's (errForeignFrame), or
// the whole call of a closure of dynamic code (enterDynamic). It returns to
// enterFrame rather than calling it, so a first call is no deeper on the Go
// stack than the next ones.
func (r *Realm) enterFrameSlow(fn *Object, fd *FunctionData, this Value, args []Value) (res Value, done bool, err error) {
	if r.interruptFlag.Load() != 0 {
		if err := r.pendingInterrupt(); err != nil {
			return Undefined(), true, err
		}
	}
	if fd.realm != r {
		return Undefined(), true, r.errForeignFrame()
	}
	if fd.icBase == icUnbound {
		if fd.meta.dyn&dynFlag != 0 {
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
// bindings, else env. The stack pointer is the top of the frame's
// registers, which a function with ForwardArguments raises over the
// arguments it keeps.
func (r *Realm) enterFrameExtra(code *bytecode.Function, env *Env, regs, args []Value) *Env {
	np := int(code.NumParams)
	if code.HasRest {
		regs[np] = ObjectValue(r.newArrayCopy(args[min(np, len(args)):]))
	}
	if code.HasArguments {
		i := np
		if code.HasRest {
			i++
		}
		if code.ForwardArguments {
			regs[i] = IntValue(len(args))
			r.keepArguments(args) // last: it may move the stack under regs
		} else {
			regs[i] = ObjectValue(r.newArguments(args))
		}
	}
	if layout := code.CaptureLayout; len(layout) > 0 && code.Kind != bytecode.KindModule && code.Kind != bytecode.KindScript {
		env = newEnvSized(env, len(layout))
		r.charge((len(layout) + 1) * valueSize)
		s := env.slots()
		for i, reg := range layout {
			if reg != bytecode.NoRegister {
				s[i] = regs[reg]
			}
		}
	}
	return env
}

// growFrames doubles the frame table, charging the records it adds as
// moveStack charges registers.
func (r *Realm) growFrames() {
	st := &r.interp
	n := max(initialFrames, 2*len(st.frames))
	r.charge((n - len(st.frames)) * int(unsafe.Sizeof(frameInfo{})))
	nf := make([]frameInfo, n)
	copy(nf, st.frames)
	st.frames = nf
}

// undefLoopMax is the longest run of registers UndefRange clears one at a
// time; undefRegs clears a longer one.
const undefLoopMax = 8

// undefBlock is a run of undefined values for undefRegs to copy.
var undefBlock = func() (b [32]Value) {
	for i := range b {
		b[i] = Undefined()
	}
	return b
}()

// undefRegs sets the registers rs to undefined by copying undefBlock over
// them. copy() of Values is typed: while the collector marks, its bulk
// barrier sees every pointer the copy overwrites, as the barrier of each
// store setScalar makes does; a memclr or memmove of the words would hide
// them. It is not inlined: run's UndefRange keeps a call, not the loop.
//
//go:noinline
func undefRegs(rs []Value) {
	for len(rs) > 0 {
		rs = rs[copy(rs, undefBlock[:]):]
	}
}

// newEnvSized is NewEnv for the interpreter: slots initialized to undefined
// in a fresh block, whose pointer words are already nil.
func newEnvSized(parent *Env, n int) *Env {
	e := newEnvBlock(parent, n)
	u := Undefined()
	s := e.slots()
	for i := range s {
		s[i].setScalarBits(u)
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
//     to base+NumRegs first (callValue, callAt and enterFrame on a call, a native's
//     call back into JavaScript, resumeFrame for a generator or an async
//     function, module and script bodies), and nothing shrinks it below a
//     live frame (ReleaseCallData runs between calls only).
//   - The window is current: run takes it again after every operation
//     that may grow, and so move, the stack, as it re-sliced its registers
//     when they were a slice.
//
// The window starts base registers past the stack's first, with no
// bounds check: base is at most len(st.stack), and growStack keeps the
// length below the capacity (a spare register), so the start lies inside
// the stack's allocation even for a frame of no registers at the top of
// a full stack. One past the allocation could point at memory the
// collector finds free. A stack of no capacity, which ReleaseCallData
// leaves, has no frame with registers: its window is nil, and run never
// indexes it.
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
	return (*[windowRegs]Value)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(st.stack)), base*valueSize))
}

// storePC stores pc as frame fi's: run's store before what can read the
// frame's pc (see run). Inlined, it costs run's size, which must stay
// below the compiler's big-function size (DESIGN §6), only its call.
func (st *interpState) storePC(fi, pc int) { st.frames[fi].pc = uint32(pc) }

// nonNilWindow returns w, as a value the compiler takes to be non-nil: it
// assumes so of the result of an unsafe.Add, and drops the add of 0, so
// the call costs no instruction. run takes its window through it at every
// instruction, which then needs no nil check at its register accesses (a
// window run indexes is not nil: see window). It is exempt from checkptr
// as window is: the conversion makes a pointer to a whole window.
//
//go:nocheckptr
func nonNilWindow(w *[windowRegs]Value) *[windowRegs]Value {
	return (*[windowRegs]Value)(unsafe.Add(unsafe.Pointer(w), 0))
}

// growStack makes the register stack hold at least top registers. Its
// length is the extent the calls since the last ReleaseCallData have used,
// so the release clears no more than that (a pointer store, a register
// cleared included, costs a write barrier while the collector marks);
// between the length and the capacity every register is zero. growStack
// extends the length within the capacity and doubles the capacity past it
// (moveStack), keeping a register past the length: window takes a frame's
// registers from its base, which may be the length, without a bounds
// check, and the start must lie inside the allocation.
func (r *Realm) growStack(top int) {
	if top >= cap(r.interp.stack) {
		r.moveStack(top)
		return
	}
	r.interp.stack = r.interp.stack[:top]
}

// moveStack moves the register stack to storage of at least top registers,
// doubling its capacity. Under a memory limit it charges the registers it
// adds, so the account counts the stack the realm holds and not the
// storage it leaves, which is garbage: a deep recursion stops at the limit.
func (r *Realm) moveStack(top int) {
	st := &r.interp
	n := max(initialStackSize, 2*cap(st.stack))
	for n <= top { // a register past the length (growStack)
		n *= 2
	}
	r.charge((n - cap(st.stack)) * valueSize)
	ns := make([]Value, top, n)
	copy(ns, st.stack[:st.sp])
	st.stack = ns
}

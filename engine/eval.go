package engine

import (
	"errors"
	"math"
	"strings"
	"sync/atomic"

	"github.com/Calcium-Ion/moejs/bytecode"
)

// Code generation from strings: eval, the Function constructors and
// Realm.EvalScript compile source text at run time. The engine does not
// import the parser or the compiler (DESIGN §2): the host installs them as
// a Compiler (SetCompiler), which the root package does on initialization.
// A process without one throws an EvalError from every one of these entry
// points.

// Compiler compiles source text for the engine. Its methods may run on any
// goroutine; the templates they return are immutable and shared like any
// other. The engine passes each a stop function that returns the
// *InterruptedError of an interrupt pending in the realm, and nil
// otherwise: a compiler calls it now and then, so that an interrupt stops a
// long compile too, and returns its error as is.
type Compiler interface {
	// CompileScript compiles a classic script (sloppy unless it says "use
	// strict") into a bytecode.KindScript template for Realm.RunScript.
	CompileScript(name, src string, stop func() error) (*bytecode.Function, error)
	// CompileEval compiles eval code into a bytecode.KindArrow template
	// whose call returns the completion value. scope describes the
	// environment of a direct eval call site (the EvalScope of its CallEval
	// instruction); nil compiles an indirect eval, which runs as global
	// code. Var and function declarations of sloppy eval code that land in
	// the global variable environment are listed in Extra.Globals.
	CompileEval(name, src string, scope *bytecode.EvalScope, stop func() error) (*bytecode.Function, error)
	// CompileFunction compiles the function CreateDynamicFunction builds
	// from params and body: the result is a bytecode.KindScript template
	// that declares nothing and returns the new function object. The
	// parameters and the body are checked to parse on their own.
	CompileFunction(name, params, body string, generator, async bool, stop func() error) (*bytecode.Function, error)
}

// DefaultMaxDynamicSource is the length of the longest source text a realm
// compiles at run time unless RealmOptions.MaxDynamicSource says otherwise:
// 1 MiB.
const DefaultMaxDynamicSource = 1 << 20

type compilerBox struct{ c Compiler }

var installedCompiler atomic.Pointer[compilerBox]

// SetCompiler installs the process-wide compiler behind eval, the Function
// constructors and Realm.EvalScript. The root package installs one when it
// is initialized; a host that uses the engine on its own may install the
// same (compiler.Hook) or none.
func SetCompiler(c Compiler) {
	if c == nil {
		installedCompiler.Store(nil)
		return
	}
	installedCompiler.Store(&compilerBox{c})
}

// compiler returns the installed compiler, or the EvalError for its absence.
func (r *Realm) compiler() (Compiler, error) {
	if b := installedCompiler.Load(); b != nil {
		return b.c, nil
	}
	return nil, &Exception{Value: ObjectValue(r.NewError(KindEvalError, "code generation from strings is not available: no compiler is installed (engine.SetCompiler)"))}
}

// compileError converts a compile failure into the SyntaxError it throws.
// The interrupt that stopped the compile (the Compiler's stop function)
// stays itself.
func (r *Realm) compileError(err error) error {
	if ie, ok := err.(*InterruptedError); ok {
		return ie
	}
	var m interface{ Message() string }
	if errors.As(err, &m) {
		return r.SyntaxError("%s", m.Message())
	}
	return r.SyntaxError("%s", err.Error())
}

// MaxDynamicSource returns the length of the longest source text the realm
// compiles at run time (RealmOptions.MaxDynamicSource), negative for no
// limit.
func (r *Realm) MaxDynamicSource() int {
	if r.host != nil && r.host.maxSource != 0 {
		return int(r.host.maxSource)
	}
	return DefaultMaxDynamicSource
}

// SetMaxDynamicSource sets the length of the longest source text the realm
// compiles at run time: zero restores DefaultMaxDynamicSource, a negative n
// removes the limit (see RealmOptions.MaxDynamicSource). A limit above
// math.MaxInt32 is math.MaxInt32.
func (r *Realm) SetMaxDynamicSource(n int) {
	if r.host == nil {
		if n == 0 {
			return
		}
		r.host = &realmHost{}
	}
	switch {
	case n > math.MaxInt32:
		n = math.MaxInt32
	case n < math.MinInt32:
		n = -1
	}
	r.host.maxSource = int32(n)
}

// DynamicCodeDisabled reports whether the realm refuses to compile code at
// run time (RealmOptions.DisableDynamicCode).
func (r *Realm) DynamicCodeDisabled() bool {
	return r.host != nil && r.host.noDynamic
}

// SetDynamicCodeDisabled sets whether the realm refuses to compile code at
// run time (see RealmOptions.DisableDynamicCode).
func (r *Realm) SetDynamicCodeDisabled(disabled bool) {
	if r.host == nil {
		if !disabled {
			return
		}
		r.host = &realmHost{}
	}
	r.host.noDynamic = disabled
}

// ensureCanCompile returns the EvalError of a realm that compiles no code
// at run time (DisableDynamicCode): the host's refusal to compile any
// (HostEnsureCanCompileStrings), as web hosts refuse eval under a Content
// Security Policy.
func (r *Realm) ensureCanCompile() error {
	if r.DynamicCodeDisabled() {
		return &Exception{Value: ObjectValue(r.NewError(KindEvalError, "code generation from strings is disabled for this realm (DisableDynamicCode)"))}
	}
	return nil
}

// checkSource returns the RangeError of source text of n bytes, in UTF-8,
// that is longer than the realm compiles at run time (MaxDynamicSource):
// the host's refusal to compile it (HostEnsureCanCompileStrings), as
// parsing and compiling allocate many times the size of the source.
func (r *Realm) checkSource(n int) error {
	if limit := r.MaxDynamicSource(); limit >= 0 && n > limit {
		return r.RangeError("Source text of %d bytes is longer than the %d this realm compiles at run time (MaxDynamicSource)", n, limit)
	}
	return nil
}

// sourceText converts the strings of source text compiled at run time to
// the UTF-8 the compiler parses, with lone surrogates as WTF-8 (String.wtf8)
// so the literals that hold them keep them, or returns a RangeError for
// their total length, joined by the caller with sep bytes of ASCII in all.
// Strings of more code units than the limit are refused by that count
// before any is converted or a rope flattened, as their UTF-8 is at least
// as long; otherwise the length checked is that of the conversions
// (checkSource). A realm that compiles no code at run time refuses them
// first (ensureCanCompile).
func (r *Realm) sourceText(strs []*String, sep int, out []string) ([]string, error) {
	if err := r.ensureCanCompile(); err != nil {
		return nil, err
	}
	if limit := r.MaxDynamicSource(); limit >= 0 {
		n := sep
		for _, s := range strs {
			n += s.Len()
		}
		if n > limit {
			return nil, r.RangeError("Source text of %d code units is longer than the %d bytes this realm compiles at run time (MaxDynamicSource)", n, limit)
		}
	}
	n := sep
	for _, s := range strs {
		src, _ := s.wtf8()
		n += len(src)
		out = append(out, src)
	}
	if err := r.checkSource(n); err != nil {
		return nil, err
	}
	return out, nil
}

// EvalScript compiles src as a classic script named name with the
// installed compiler and runs it (RunScript). A compile error is a thrown
// SyntaxError, a missing compiler or DisableDynamicCode a thrown EvalError,
// and a src of more bytes than MaxDynamicSource a thrown RangeError; an
// interrupt stops the compile. The script is dynamic code (dynamic.go): the
// realm keeps nothing for it once the program no longer runs its functions.
func (r *Realm) EvalScript(name, src string) (Value, error) {
	if err := r.ensureCanCompile(); err != nil {
		return Undefined(), err
	}
	if err := r.checkSource(len(src)); err != nil {
		return Undefined(), err
	}
	c, err := r.compiler()
	if err != nil {
		return Undefined(), err
	}
	code, err := c.CompileScript(name, src, r.CheckInterrupt)
	if err != nil {
		return Undefined(), r.compileError(err)
	}
	d, err := r.dynMeta(code, nil)
	if err != nil {
		return Undefined(), err
	}
	if err := r.globalDeclarationInstantiation(code); err != nil {
		return Undefined(), err
	}
	return r.CallObject(r.scriptClosure(d), ObjectValue(r.Global), nil)
}

// activeSource returns the source name and the root of the innermost
// running bytecode function ("" and nil outside any). The code it compiles
// from a string belongs to its script or module (GetActiveScriptOrModule):
// that code's stack frames carry the name, and its import() calls the
// referrer of the root (dynRoot.ref).
func (r *Realm) activeSource() (string, *funcMeta) {
	st := &r.interp
	for i := st.nframes - 1; i >= 0; i-- {
		if fn := st.frames[i].fn; fn != nil {
			if fd, ok := fn.internal.(*FunctionData); ok && fd.code != nil {
				return sourceName(fd.code), fd.meta.root
			}
		}
	}
	return "", nil
}

// --- eval ---------------------------------------------------------------------------

func init() { lateGlobal(StringKey(AtomEval), installEval) }

func installEval(r *Realm) {
	r.evalFn = r.NewNativeFunction(AtomEval, 1, globalEval)
	r.bindGlobal(AtomEval, ObjectValue(r.evalFn))
}

// globalEval is %eval% called other than by a direct eval: PerformEval
// with direct false (ES2025 19.2.1.1), which runs the code as global code
// of the realm.
func globalEval(r *Realm, this Value, args []Value) (Value, error) {
	x := Arg(args, 0)
	if !x.IsString() {
		return x, nil
	}
	d, err := r.compileEval(x.AsString(), nil)
	if err != nil {
		return Undefined(), err
	}
	g := ObjectValue(r.Global)
	fn, _ := r.newClosure(d.code, &d.funcMeta, nil, g)
	if err := r.globalDeclarationInstantiation(d.code); err != nil {
		return Undefined(), err
	}
	return r.CallObject(fn, g, nil)
}

// evalCacheSize bounds each of the realm's caches of compiled eval code
// and of compiled dynamic functions: a program that compiles more distinct
// strings than this starts over.
const evalCacheSize = 64

// evalKey identifies compiled eval code: the source, the root of the code
// evaluating it, whose script or module the eval code belongs to (its
// source name and its record, see dynRoot.ref), and the call site (nil for
// an indirect eval).
type evalKey struct {
	src   string
	root  *funcMeta
	scope *bytecode.EvalScope
}

// compileEval compiles eval code for the call site scope (nil: an indirect
// eval) into a dynamic tree (dynamic.go). The realm keeps the trees of the
// strings it evaluated last, so a loop evaluating the same string compiles
// it, and binds its inline caches, once. Code with a tagged template is
// compiled every time: each evaluation parses new template literals, whose
// template objects differ (ES2025 13.2.8.4 GetTemplateObject), while a tree
// keeps one per site of a template (template.go).
func (r *Realm) compileEval(s *String, scope *bytecode.EvalScope) (*dynRoot, error) {
	var buf [1]string
	text, err := r.sourceText([]*String{s}, 0, buf[:0])
	if err != nil {
		return nil, err
	}
	src := text[0]
	name, root := r.activeSource()
	k := evalKey{src, root, scope}
	ds := r.dynState()
	if d, ok := ds.evals[k]; ok {
		return d, nil
	}
	c, err := r.compiler()
	if err != nil {
		return nil, err
	}
	code, err := c.CompileEval(name, src, scope, r.CheckInterrupt)
	if err != nil {
		return nil, r.compileError(err)
	}
	r.charge(compiledSize(code))
	d, err := r.dynMeta(code, r.scriptOrModuleOf(root))
	if err != nil {
		return nil, err
	}
	if hasTemplate(code) {
		return d, nil
	}
	if ds.evals == nil || len(ds.evals) >= evalCacheSize {
		ds.evals = make(map[evalKey]*dynRoot, 4)
	}
	ds.evals[k] = d
	return d, nil
}

// hasTemplate reports whether fn or a function nested in it has a tagged
// template.
func hasTemplate(fn *bytecode.Function) bool {
	for i := range fn.Consts {
		if fn.Consts[i].Kind == bytecode.ConstTemplate {
			return true
		}
	}
	for _, c := range fn.Children {
		if hasTemplate(c) {
			return true
		}
	}
	return false
}

// callEval executes the CallEval instruction w (extra word x) of the frame
// of code whose registers start at base, running in env with this, and
// stores the call's result in the instruction's destination register. A
// callee other than the realm's %eval% is an ordinary call; %eval% is a
// direct eval (ES2025 13.3.6.1, PerformEval with direct true): the eval
// code runs in env, with the frame's this, new.target and super.
func (r *Realm) callEval(code *bytecode.Function, base int, w, x uint32, env *Env, this Value) error {
	res, err := r.directEval(code, base, w, x, env, this)
	if err == nil {
		r.interp.stack[base+int(uint8(w>>8))] = res
	}
	return err
}

func (r *Realm) directEval(code *bytecode.Function, base int, w, x uint32, env *Env, this Value) (Value, error) {
	st := &r.interp
	top := base + int(code.NumRegs)
	regs := st.stack[base:top:top]
	a := int(uint8(w >> 8))
	var args []Value
	if uint8(w>>24) != 0 {
		argv, err := r.spreadArgs(top, regs[a+2])
		if err != nil {
			return Undefined(), err
		}
		args = argv
	} else {
		argc := int(uint8(w >> 16))
		args = st.stack[base+a+2 : base+a+2+argc : base+a+2+argc]
	}
	callee := st.stack[base+a]
	if r.evalFn == nil {
		// %eval% is defined with the eval global: define it first, so that
		// the answer does not depend on whether the program has read it
		// (the shared %eval% another realm handed over is this one's).
		r.lateAt(lateEval)
	}
	if !callee.IsObject() || callee.AsObject() != r.evalFn {
		st.sp = max(st.sp, top+len(args)) // keep the callee's window above a spread argument list
		res, err := r.callValue(callee, st.stack[base+a+1], args)
		st.sp = top
		return res, err
	}
	if len(args) == 0 {
		return Undefined(), nil
	}
	src := args[0]
	if !src.IsString() {
		return src, nil
	}
	d, err := r.compileEval(src.AsString(), code.Extra.Evals[x])
	if err != nil {
		return Undefined(), err
	}
	fn, _ := r.newClosure(d.code, &d.funcMeta, env, this)
	if err := r.globalDeclarationInstantiation(d.code); err != nil {
		return Undefined(), err
	}
	return r.CallObject(fn, this, nil)
}

// --- CreateDynamicFunction ---------------------------------------------------------------

// dynamicKind selects the function kind a dynamic-function constructor
// creates.
type dynamicKind uint8

const (
	dynamicNormal dynamicKind = iota
	dynamicGenerator
	dynamicAsync
	dynamicAsyncGenerator
)

func functionCall(r *Realm, this Value, args []Value) (Value, error) {
	return r.createDynamicFunction(nil, dynamicNormal, args)
}

func functionConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	return r.createDynamicFunction(newTarget, dynamicNormal, args)
}

func generatorFunctionCall(r *Realm, this Value, args []Value) (Value, error) {
	return r.createDynamicFunction(nil, dynamicGenerator, args)
}

func generatorFunctionConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	return r.createDynamicFunction(newTarget, dynamicGenerator, args)
}

func asyncFunctionCall(r *Realm, this Value, args []Value) (Value, error) {
	return r.createDynamicFunction(nil, dynamicAsync, args)
}

func asyncFunctionConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	return r.createDynamicFunction(newTarget, dynamicAsync, args)
}

func asyncGeneratorFunctionCall(r *Realm, this Value, args []Value) (Value, error) {
	return r.createDynamicFunction(nil, dynamicAsyncGenerator, args)
}

func asyncGeneratorFunctionConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	return r.createDynamicFunction(newTarget, dynamicAsyncGenerator, args)
}

// funcKey identifies the compiled code of a dynamic function: its kind,
// parameters and body, and the root of the code creating it, as evalKey.
type funcKey struct {
	params, body string
	root         *funcMeta
	kind         dynamicKind
}

// compileFunction compiles the dynamic function of kind with params and
// body into a dynamic tree, cached as compileEval caches eval code: the
// tree's template is shared, while each constructor call runs it to create
// a new function object.
func (r *Realm) compileFunction(c Compiler, kind dynamicKind, params, body string) (*dynRoot, error) {
	name, root := r.activeSource()
	k := funcKey{params, body, root, kind}
	ds := r.dynState()
	if d, ok := ds.funcs[k]; ok {
		return d, nil
	}
	gen := kind == dynamicGenerator || kind == dynamicAsyncGenerator
	async := kind == dynamicAsync || kind == dynamicAsyncGenerator
	code, err := c.CompileFunction(name, params, body, gen, async, r.CheckInterrupt)
	if err != nil {
		return nil, r.compileError(err)
	}
	r.charge(compiledSize(code))
	d, err := r.dynMeta(code, r.scriptOrModuleOf(root))
	if err != nil {
		return nil, err
	}
	if hasTemplate(code) {
		return d, nil
	}
	if ds.funcs == nil || len(ds.funcs) >= evalCacheSize {
		ds.funcs = make(map[funcKey]*dynRoot, 4)
	}
	ds.funcs[k] = d
	return d, nil
}

// createDynamicFunction implements CreateDynamicFunction (ES2025
// 20.2.1.1.1) for the constructor of kind called with args: every argument
// but the last is a parameter list, the last the body. newTarget is nil for
// a call. Parameters, joined by commas, and body longer together than
// MaxDynamicSource throw a RangeError once all are converted to strings,
// where the host is asked whether it compiles them
// (HostEnsureCanCompileStrings), and a realm that compiles no code at run
// time (DisableDynamicCode) throws an EvalError there.
func (r *Realm) createDynamicFunction(newTarget *Object, kind dynamicKind, args []Value) (Value, error) {
	var buf [4]*String
	strs := buf[:0]
	for _, a := range args {
		s, err := r.ToString(a)
		if err != nil {
			return Undefined(), err
		}
		strs = append(strs, s)
	}
	// The parameters are joined by commas, one fewer than they are, which
	// count toward the limit as the text they add; the fixed text around them
	// (`function anonymous(` and the body's braces) does not.
	sep := max(len(strs)-2, 0)
	var textBuf [4]string
	text, err := r.sourceText(strs, sep, textBuf[:0])
	if err != nil {
		return Undefined(), err
	}
	var params []string
	body := ""
	if len(text) > 0 {
		params, body = text[:len(text)-1], text[len(text)-1]
	}
	c, err := r.compiler()
	if err != nil {
		return Undefined(), err
	}
	d, err := r.compileFunction(c, kind, strings.Join(params, ","), body)
	if err != nil {
		return Undefined(), err
	}
	var ctor, fallback *Object
	switch kind {
	case dynamicNormal:
		ctor, fallback = r.FunctionCtor, r.FunctionPrototype
	case dynamicGenerator:
		gi := r.generatorIntrinsics()
		ctor, fallback = gi.GeneratorFunction, gi.GeneratorFunctionPrototype
	case dynamicAsync:
		ai := r.asyncIntr()
		ctor, fallback = ai.AsyncFunction, ai.AsyncFunctionPrototype
	default:
		ai := r.asyncIntr()
		ctor, fallback = ai.AsyncGeneratorFunction, ai.AsyncGeneratorFunctionPrototype
	}
	proto, err := r.GetPrototypeFromConstructor(newTarget, ctor, fallback)
	if err != nil {
		return Undefined(), err
	}
	fv, err := r.CallObject(r.scriptClosure(d), ObjectValue(r.Global), nil)
	if err != nil {
		return Undefined(), err
	}
	fv.AsObject().SetPrototypeOf(r, proto)
	return fv, nil
}

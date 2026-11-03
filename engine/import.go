package engine

import (
	"errors"
	"strconv"

	"github.com/MoeForge/moejs/bytecode"
)

// Dynamic import() and import.meta (ECMA-262 §13.3.10, §13.3.12).
//
// Both need the script or module record of the running code
// (GetActiveScriptOrModule): the referrer import() hands the host, and
// the module whose import.meta it reads. The compiler flags the root
// template of a script or module whose code uses either
// (bytecode.Function.ScriptOrModule), and a realm keeps a record only for
// such roots, in its module map, keyed by the root's funcMeta: every
// function of a tree, however deeply nested, reaches its root's funcMeta
// in one load (FunctionData.meta.root), and there is one per template in
// the process, so the key is the template's identity without a pointer
// to it. EvaluateGraph records the modules it instantiates, with the
// value LinkOptions.HostDefined gave each; a host records a script
// before it runs it (SetHostDefined).
//
// Eval code and the code of the Function constructors belong to the
// script or module that evaluates them. The compiler flags the root of a
// script or module holding a direct eval call too, and a root compiled
// from a string shares the record of the root of the innermost running
// bytecode function (dynRoot.ref, dynamic.go): the caller of a direct
// eval, of an indirect eval or of a constructor. The record itself is
// shared, so code that evaluates more code passes it on, also when it
// holds no import() and no direct eval: an indirect eval or a constructor
// finds the record at run time only. Code whose root has no record
// imports with a nil referrer: a module evaluated alone (EvaluateModule),
// a script the host did not record, and so the code an indirect eval or a
// Function constructor compiles for a script or module whose own code
// uses neither import(), import.meta nor a direct eval.
//
// import() asks ImportHooks.Load for the module at once, rejecting its
// promise on failure, and links and evaluates it from a job (the
// reactions of ContinueDynamicImport): ImportHooks.Link returns its graph,
// which EvaluateGraph's instantiation and evaluation run in the realm's
// module map, so a module the realm evaluated before is reused, not
// evaluated again. A later job resolves the promise with the namespace.
// A module whose evaluation an interrupt stopped, before or after an
// await (see EvaluateGraph), stays failed: a later import() whose graph
// reaches it rejects with an Error, `Cannot import "specifier": its
// evaluation was interrupted`, which holds neither the interrupt's value
// nor a Go error.

// ImportHooks are the host's hooks for import() and import.meta
// (SetImportHooks). They may be shared by any number of realms.
type ImportHooks struct {
	// Load returns the module specifier names for referrer
	// (HostLoadImportedModule): the host value of the importing script or
	// module (SetHostDefined, LinkOptions.HostDefined), nil when it has
	// none. It runs within import(), and its error rejects import()'s
	// promise: an *Exception with its value, an *InterruptedError stops
	// the running code, any other error rejects with an Error (as a native
	// function's error throws). Returning no module rejects with a
	// TypeError.
	Load func(r *Realm, referrer any, specifier string) (module any, err error)
	// Link returns the linked graph (LinkModules) whose entry is the module
	// Load returned. It runs from a job; its error rejects as Load's does.
	// The realm evaluates the graph as EvaluateGraph does, so a host that
	// links a module once per process and returns the same graph pays
	// only for the instantiation and the evaluation in each realm.
	Link func(r *Realm, module any) (*ModuleGraph, error)
	// Meta, when set, fills the import.meta object of module, the host
	// value of the module (LinkOptions.HostDefined), when import.meta is
	// first evaluated in the module in the realm
	// (HostGetImportMetaProperties and HostFinalizeImportMeta). The object
	// has a null prototype and no properties. Its error is thrown by the
	// import.meta expression, and the next one calls Meta again.
	Meta func(r *Realm, module any, meta *Object) error
}

// SetImportHooks sets the hooks of import() and import.meta; nil removes
// them. Without Load and Link, import() rejects with a TypeError.
func (r *Realm) SetImportHooks(h *ImportHooks) {
	if r.host == nil {
		if h == nil {
			return
		}
		r.host = &realmHost{}
	}
	r.host.imports = h
}

// ImportHooks returns the realm's import hooks, or nil.
func (r *Realm) ImportHooks() *ImportHooks {
	if r.host == nil {
		return nil
	}
	return r.host.imports
}

// SetHostDefined records host as the host value of the script or module
// template code (its [[HostDefined]]): import() in its code passes it to
// ImportHooks.Load as the referrer, and import.meta to ImportHooks.Meta.
// A host calls it for a script before RunScript; EvaluateGraph records
// the modules it instantiates from LinkOptions.HostDefined. Only a root
// template whose code uses import() or import.meta, or holds a direct
// eval call (bytecode.Function.ScriptOrModule), has a record, which the
// realm keeps as long as it lives, and code compiled from a string in its
// code shares (dynRoot.ref); SetHostDefined of any other template does
// nothing and returns nil. A function of code's tree that fails
// bytecode.VerifyRegisters is an error, and nothing is recorded.
func (r *Realm) SetHostDefined(code *bytecode.Function, host any) error {
	if code == nil || !code.ScriptOrModule {
		return nil
	}
	m, err := r.metaFor(code)
	if err != nil {
		return err
	}
	r.modules().setScriptOrModule(m, host)
	return nil
}

// scriptOrModule is a realm's record of a script or module root whose code
// uses import() or import.meta, or holds a direct eval call.
type scriptOrModule struct {
	host any     // the host value (SetHostDefined, LinkOptions.HostDefined)
	meta *Object // import.meta once created
}

// setScriptOrModule records host for the root root, keeping its
// import.meta.
func (mm *moduleMap) setScriptOrModule(root *funcMeta, host any) {
	if mm.refs == nil {
		mm.refs = make(map[*funcMeta]*scriptOrModule)
	}
	if ref := mm.refs[root]; ref != nil {
		ref.host = host
		return
	}
	mm.refs[root] = &scriptOrModule{host: host}
}

// scriptOrModuleOf returns the record of the root root (nil: none), or nil.
// The eval code and the dynamic functions compiled by the code of root
// belong to its script or module (ES2025 19.2.1.1 PerformEval, 20.2.1.1.1
// CreateDynamicFunction), so their import() calls pass the same referrer.
func (r *Realm) scriptOrModuleOf(root *funcMeta) *scriptOrModule {
	if root == nil {
		return nil
	}
	if d := dynRootOf(root); d != nil {
		return d.ref
	}
	if lz := r.lazy; lz != nil && lz.modules != nil {
		return lz.modules.refs[root]
	}
	return nil
}

// moduleOp executes ImportCall and ImportMeta (from asyncOp's default
// case) of the frame whose registers start at base.
func (r *Realm) moduleOp(fd *FunctionData, base int, w uint32, pc int) (int, error) {
	var v Value
	var err error
	if bytecode.Op(w) == bytecode.ImportCall {
		v, err = r.importCall(fd.meta.root, r.interp.stack[base+int(uint8(w>>16))])
	} else {
		v, err = r.importMeta(fd.meta.root)
	}
	r.interp.stack[base+int(uint8(w>>8))] = v
	return pc, err
}

// importCall implements import(specifier) in the code of root
// (EvaluateImportCall, §13.3.10.2, with HostLoadImportedModule and
// ContinueDynamicImport): it returns the promise, which a failure
// rejects. Only an interrupt is returned.
func (r *Realm) importCall(root *funcMeta, specifier Value) (Value, error) {
	p := r.newPromise()
	s, err := r.ToString(specifier)
	if err != nil {
		return ObjectValue(p), r.importFailed(p, err)
	}
	h := r.ImportHooks()
	if h == nil || h.Load == nil || h.Link == nil {
		r.rejectPromise(p, ObjectValue(r.NewError(KindTypeError,
			"Cannot import %s: the host set no import hook (moejs.Options.Importer, engine.Realm.SetImportHooks)", strconv.Quote(s.GoString()))))
		return ObjectValue(p), nil
	}
	var referrer any
	if ref := r.scriptOrModuleOf(root); ref != nil {
		referrer = ref.host
	}
	module, err := h.Load(r, referrer, s.GoString())
	if err == nil && module == nil {
		err = r.TypeError("Cannot import %s: the host loaded no module", strconv.Quote(s.GoString()))
	}
	if err != nil {
		return ObjectValue(p), r.importFailed(p, err)
	}
	d := &dynamicImport{promise: p, specifier: s, module: module, hooks: h}
	d.re.reactor = d
	r.enqueue(job{reaction: &d.re})
	return ObjectValue(p), nil
}

// importFailed rejects the promise p of an import() with what err throws.
// It returns err only when it is the interrupt pending now: an
// *InterruptedError that is not, which a hook returned, rejects with an
// Error, as other host errors do.
func (r *Realm) importFailed(p *Object, err error) error {
	if _, ok := err.(*InterruptedError); ok {
		if r.Interrupted() {
			return err
		}
		r.rejectPromise(p, r.hostErrorValue(err))
		return nil
	}
	v, _ := r.thrownValue(err)
	r.rejectPromise(p, v)
	return nil
}

// dynamicImport is an import() whose module the host loaded: its reaction
// runs as the job that links and evaluates the module, then as the
// reaction to the evaluation that resolves the promise with the namespace.
type dynamicImport struct {
	re        promiseReaction // the reaction of both jobs, one at a time
	promise   *Object
	specifier *String
	module    any // what ImportHooks.Load returned, until linked
	hooks     *ImportHooks
	graph     *ModuleGraph // once linked
}

func (d *dynamicImport) promiseSettled(r *Realm, v Value, rejected bool) error {
	if d.graph == nil {
		return r.linkAndEvaluate(d)
	}
	if rejected {
		r.rejectPromise(d.promise, v)
		return nil
	}
	return r.resolvePromiseErr(d.promise, r.namespaceOf(d.graph, 0))
}

// linkAndEvaluate is the job of ContinueDynamicImport's linkAndEvaluate
// closure: it links the module, instantiates the graph's modules the realm
// has not, and evaluates the entry, reacting to its evaluation as
// PerformPromiseThen of the promise Evaluate returns does.
func (r *Realm) linkAndEvaluate(d *dynamicImport) error {
	g, err := d.hooks.Link(r, d.module)
	if err == nil && g == nil {
		err = errors.New("engine: ImportHooks.Link returned no module graph")
	}
	if err != nil {
		return r.importFailed(d.promise, err)
	}
	d.module = nil
	entry, err := r.instantiateGraph(g)
	if err != nil {
		return r.importFailed(d.promise, err)
	}
	d.graph = g
	p, err := r.evaluateModule(entry)
	switch {
	case err != nil:
		var v Value
		if _, ok := err.(*InterruptedError); ok {
			if r.Interrupted() {
				return err
			}
			// An earlier evaluation was interrupted, which evaluateModule
			// cached: the Error keeps the host's interrupt value out of
			// JavaScript.
			v = ObjectValue(r.NewError(KindError, "Cannot import %s: its evaluation was interrupted", strconv.Quote(d.specifier.GoString())))
		} else if v, ok = r.thrownValue(err); !ok {
			v = r.hostErrorValue(err)
		}
		r.enqueue(job{reaction: &d.re, arg: v, rejected: true})
	case p != nil:
		r.performPromiseThen(p, &d.re)
	default:
		r.enqueue(job{reaction: &d.re, arg: Undefined()})
	}
	return nil
}

// importMeta implements import.meta in the code of root (§13.3.12.1): the
// module's object, created and filled by ImportHooks.Meta on first use.
func (r *Realm) importMeta(root *funcMeta) (Value, error) {
	mm := r.modules()
	ref := mm.refs[root]
	if ref == nil {
		mm.setScriptOrModule(root, nil)
		ref = mm.refs[root]
	}
	if ref.meta != nil {
		return ObjectValue(ref.meta), nil
	}
	meta := r.NewObjectWithProto(nil)
	if h := r.ImportHooks(); h != nil && h.Meta != nil {
		if err := h.Meta(r, ref.host, meta); err != nil {
			return Undefined(), err
		}
	}
	ref.meta = meta
	return ObjectValue(meta), nil
}

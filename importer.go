package moejs

import (
	"errors"
	"sync"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/engine"
)

// Importer is the host's side of import() and import.meta in the runtimes
// whose Options name it. One Importer may serve any number of runtimes,
// concurrently; its fields must not change once a runtime uses it, and it
// must not be copied then. Without an Importer, import() rejects with a
// TypeError and import.meta is an empty object.
//
// import(specifier) asks Resolve for the module at once and links and
// evaluates it from a job: its promise settles with the module's namespace
// or what the resolution, the linking or the evaluation failed with. The
// module is the identity, as for Link: a runtime evaluates one module
// once, whether its code imports it statically or dynamically, so a module
// the runtime evaluated before is not evaluated again. The graph of a
// module is linked once per Importer (the graph of a module Link returned
// is its own), so each runtime only instantiates and evaluates it.
//
// A module whose evaluation an interrupt stopped stays failed in the
// runtime: one whose top level the interrupt stopped, before or after an
// await; one whose next step (resuming its top level, or running it once
// the modules it imports evaluated) the interrupt dropped with the
// runtime's queued jobs; and one that imports such a module. After
// ClearInterrupt, an import() whose graph reaches it rejects with an Error
// whose message is `Cannot import "<specifier>": its evaluation was
// interrupted`, with the specifier import() was given. The Error holds
// neither the interrupt's value nor a Go error: thrown out of a hook, its
// *Exception unwraps to nil, where the Error of a Resolve error unwraps to
// that error. An import() in flight when the interrupt came stays pending,
// and so does a module whose top level awaits a promise the interrupt left
// pending, as other code awaiting it does.
//
// Resolve and Meta are called from every goroutine that uses the Importer,
// concurrently. Each call runs within the call of the runtime that
// evaluates the import() or import.meta, or runs the job that links the
// module import() loaded, and Resolve may call back into that runtime.
//
// What the Importer and its runtimes keep grows with the distinct code
// they see. The Importer holds every module with imports that import()
// loaded, with its graph, for as long as it lives. A runtime holds a record
// of every distinct script and module whose code uses import() or
// import.meta or holds a direct eval and that it ran, with the *Script or
// *Module, for as long as it lives, however many times the code runs, and
// an entry for each piece of code it compiled from a string in them (eval
// code and the code of the Function constructors). A host that generates
// modules or scripts at run time must bound how many one Importer loads and
// one runtime runs.
type Importer struct {
	// Resolve returns the module import(specifier) names in the code of
	// referrer: the *Module whose code imports (Link's entry or a module a
	// resolver returned for its graph; Compile's module for a module Load
	// evaluated alone), or the *Script RunScript ran; nil for code of
	// neither. Eval code and the code of the Function constructors import
	// for the script or module whose code evaluates them when that code
	// uses import() or import.meta or holds a direct eval, and with a nil
	// referrer otherwise, as when a job calls eval or a constructor
	// directly, with no code of a script or module running
	// (Promise.resolve(s).then(eval)). It runs within import(), and its
	// error rejects import()'s promise: an *Exception with its value, a
	// *SyntaxError with a SyntaxError, an *InterruptedError stops the
	// running code, any other error rejects with an Error. The modules a
	// returned module imports are resolved through Resolve too, as Link
	// does.
	Resolve Resolver
	// Meta, when set, fills the import.meta object of module m when a
	// runtime first evaluates import.meta in m's code. The object has a null
	// prototype and no properties. Its error is thrown by the import.meta
	// expression, and the next evaluation calls Meta again.
	Meta func(r *Realm, m *Module, meta *Object) error

	once  sync.Once
	hooks engine.ImportHooks
	// links are the graphs of the modules with imports that import()
	// loaded: *Module to *engine.ModuleGraph, kept for the Importer's
	// life.
	links sync.Map
}

// engineHooks returns the engine's hooks of imp, built once.
func (imp *Importer) engineHooks() *engine.ImportHooks {
	imp.once.Do(func() {
		if imp.Resolve != nil {
			imp.hooks.Load, imp.hooks.Link = imp.load, imp.link
		}
		if imp.Meta != nil {
			imp.hooks.Meta = imp.meta
		}
	})
	return &imp.hooks
}

func (imp *Importer) load(r *engine.Realm, referrer any, specifier string) (any, error) {
	ref, _ := referrer.(Referrer)
	m, err := imp.Resolve(ref, specifier)
	if err != nil {
		return nil, importError(r, err)
	}
	if m == nil {
		return nil, nil
	}
	return m, nil
}

func (imp *Importer) link(r *engine.Realm, module any) (*engine.ModuleGraph, error) {
	m := module.(*Module)
	if m.graph != nil {
		return m.graph.g, nil
	}
	if l := m.code.Module.Links; l == nil || len(l.Requests) == 0 {
		return m.selfGraph()
	}
	if g, ok := imp.links.Load(m); ok {
		return g.(*engine.ModuleGraph), nil
	}
	lm, err := Link(m, imp.Resolve)
	if err != nil {
		return nil, importError(r, err)
	}
	g, _ := imp.links.LoadOrStore(m, lm.graph.g)
	return g.(*engine.ModuleGraph), nil
}

func (imp *Importer) meta(r *engine.Realm, module any, meta *engine.Object) error {
	m, _ := module.(*Module)
	return imp.Meta(r, m, meta)
}

// importError is what err of a resolution or a link rejects import() with:
// a *SyntaxError a SyntaxError.
func importError(r *engine.Realm, err error) error {
	var exc *Exception
	if errors.As(err, &exc) {
		return exc
	}
	var se *SyntaxError
	if errors.As(err, &se) {
		return r.SyntaxError("%s:%d:%d: %s", se.File, se.Line, se.Column, se.Message)
	}
	return err
}

// selfGraph returns the graph of m alone, linked once, for a module that
// requests no module: Load evaluates it through it when m uses import() or
// import.meta or the runtime has an Importer, and an Importer when
// import() loads m.
func (m *Module) selfGraph() (*engine.ModuleGraph, error) {
	if g := m.self.Load(); g != nil {
		return g, nil
	}
	g, err := engine.LinkModules(m.code, engine.LinkOptions{HostDefined: func(*bytecode.Function) any { return m }})
	if err != nil {
		return nil, err
	}
	if !m.self.CompareAndSwap(nil, g) {
		g = m.self.Load()
	}
	return g, nil
}

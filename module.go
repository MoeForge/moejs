package moejs

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/syntax"
)

// Module is a compiled ES module. It is immutable: any number of runtimes
// may load it, concurrently.
type Module struct {
	code *bytecode.Function // its Source.Name is the module's name
	// graph is the graph of a module Link returned, nil for one Compile
	// returned.
	graph *linked
	// early is the EarlyExports variant of the module, for a graph that can
	// call its exports before its body runs (engine.LinkOptions), once Link
	// needed it.
	early atomic.Pointer[bytecode.Function]
	// self is the graph of the module alone (selfGraph), for a module that
	// requests none, once Load or an Importer needed it.
	self atomic.Pointer[engine.ModuleGraph]
	ts   bool // CompileTS compiled it
}

// linked is the graph of a module Link returned.
type linked struct {
	g *engine.ModuleGraph
	// exports are the names of the entry's namespace, sorted, and bindings
	// what each one resolves to.
	exports  []string
	bindings []engine.ModuleBinding
}

// Compile parses and compiles an ES module. Bad source, including features
// the engine does not support yet, is a *SyntaxError. A module that imports
// others is loaded once Link linked it with them.
func Compile(name, source string) (*Module, error) {
	return compile(name, source, syntax.Options{})
}

// CompileTS is Compile for TypeScript source. The parser erases the type
// syntax as it reads it, so a *SyntaxError and a stack trace refer to the
// TypeScript text, and Function.prototype.toString returns it. An import
// specifier whose binding is only used as a type is dropped, and so is an
// import declaration left without bindings, as tsc drops them; an export
// of a type is dropped too. TypeScript that has run-time semantics (enum,
// a namespace with values, parameter properties, import = require,
// export =) is a *SyntaxError. The module links and loads like any other;
// a Resolver or an Importer may return modules compiled either way.
func CompileTS(name, source string) (*Module, error) {
	return compile(name, source, syntax.Options{TypeScript: true})
}

// CompileOptions are the options of CompileWith and CompileScriptWith.
type CompileOptions struct {
	// TypeScript compiles TypeScript, as CompileTS does. A script of
	// TypeScript is strict mode code, as a module is.
	TypeScript bool
	// Context, when not nil, bounds the compile: once it is done, the
	// compile stops and returns its error (context.Canceled or
	// context.DeadlineExceeded) rather than a *SyntaxError. It is checked
	// before every 1,024th statement of each statement list the parser,
	// the scope pass and the compiler go through, after every 1,024
	// items of the other lists they go through (the elements of array and
	// object literals, arguments, parameters, the operators of a chain of
	// binary ones, the members of union and intersection types,
	// interfaces, object and tuple types), so that one long statement stops
	// too, and every 1,024th time the TypeScript parser tries a reading
	// ahead (an arrow function or type arguments).
	Context context.Context
}

// CompileWith is Compile, or CompileTS, with the options of opts: a host
// that compiles source it does not trust bounds the compile with
// opts.Context, as Runtime.Interrupt does not stop a compile the host
// calls itself.
func CompileWith(name, source string, opts CompileOptions) (*Module, error) {
	return compile(name, source, syntax.Options{TypeScript: opts.TypeScript, Stop: stopFunc(opts.Context)})
}

// stopFunc returns the stop function of a compile that ctx bounds, nil for
// none.
func stopFunc(ctx context.Context) func() error {
	if ctx == nil {
		return nil
	}
	return ctx.Err
}

func compile(name, source string, opts syntax.Options) (*Module, error) {
	if opts.Stop != nil {
		if err := opts.Stop(); err != nil {
			return nil, err
		}
	}
	mod, err := syntax.ParseModule(name, source, opts)
	if err != nil {
		return nil, syntaxError(err)
	}
	code, err := compiler.CompileModuleStop(mod, opts.Stop)
	if err != nil {
		return nil, syntaxError(err)
	}
	return &Module{code: code, ts: opts.TypeScript}, nil
}

func syntaxError(err error) error {
	var se *syntax.Error
	if errors.As(err, &se) {
		return &SyntaxError{File: se.Name, Line: se.Line, Column: se.Col, Message: se.Message()}
	}
	var ce *compiler.Error
	if errors.As(err, &ce) {
		return &SyntaxError{File: ce.Name, Line: ce.Line, Column: ce.Col, Message: strings.TrimPrefix(ce.Msg, "SyntaxError: ")}
	}
	return err
}

// Name returns the name given to Compile.
func (m *Module) Name() string { return m.code.Source.Name }

// Exports returns the module's export names, sorted: for a module Link
// returned, the names it re-exports too, without those its star exports
// leave ambiguous.
func (m *Module) Exports() []string {
	if m.graph != nil {
		return slices.Clone(m.graph.exports)
	}
	out := make([]string, len(m.code.Module.Exports))
	for i, e := range m.code.Module.Exports {
		out[i] = e.Name
	}
	return out
}

// Requests returns the specifiers the module imports from, in the order
// they first appear in its source, nil when it imports none.
func (m *Module) Requests() []string {
	l := m.code.Module.Links
	if l == nil || len(l.Requests) == 0 {
		return nil
	}
	out := make([]string, len(l.Requests))
	for i, req := range l.Requests {
		out[i] = req.Specifier
	}
	return out
}

// A Referrer is the code that requests a module: a *Module or a *Script.
type Referrer interface {
	// Name returns the name the code was compiled with.
	Name() string
	referrer() // sealed
}

func (*Module) referrer() {}

// Resolver returns the module that specifier names in referrer
// (HostLoadImportedModule). Link asks it for the requests of the modules of
// a graph: the referrer is the importing *Module, Link's entry or a module
// the resolver returned. An Importer asks it for import() too, where the
// referrer can also be a *Script, or nil. moejs never reads files or the
// network: the host maps every specifier to a module it compiled. The
// module is the identity: a runtime evaluates one module once, so a
// resolver returns the same *Module for the same module every time, which
// also compiles a module shared by several graphs once. A module Link
// returned stands for the module it linked: its graph is not used, and Link
// resolves its requests again.
type Resolver func(referrer Referrer, specifier string) (*Module, error)

// ErrNoResolver is the Err of the *ResolveError of Link for a module that
// imports when the resolver is nil.
var ErrNoResolver = engine.ErrNoResolver

// ResolveError is Link's error when the resolver fails for an import, an
// export-from or a star export of a module, at File, Line and Column of
// that entry: Err is the resolver's error, ErrNoResolver, or an error when
// the resolver returned no module. Load returns one too when the graph
// resolves the request of a module the runtime instantiated before (from
// import()) to another module than the runtime did; an import() of such a
// graph rejects with an Error of it.
type ResolveError struct {
	File      string
	Line      int // 1-based
	Column    int // 1-based, in code points
	Specifier string
	Err       error
}

// Error formats as `file:line:col: cannot resolve module "specifier": err`.
func (e *ResolveError) Error() string {
	return e.File + ":" + strconv.Itoa(e.Line) + ":" + strconv.Itoa(e.Column) + ": cannot resolve module " + strconv.Quote(e.Specifier) + ": " + e.Err.Error()
}

func (e *ResolveError) Unwrap() error { return e.Err }

// Link resolves the modules entry imports, directly or not, asking resolve
// once per module and specifier, links them and returns entry linked with
// its graph: the Module runtimes load, whose Hook and Exports also reach
// the names entry re-exports. Linking happens once; the Module is
// immutable, and each runtime that loads it only instantiates and evaluates
// the graph. The Hooks of entry's own exports stay valid for the linked
// Module. An import that does not resolve to one binding is a *SyntaxError
// at the import in the importing module, and so is an indirect export; a
// failed resolution is a *ResolveError. Link runs no JavaScript: a panic of
// resolve propagates. A module that imports nothing and uses neither
// import() nor import.meta is returned as is. A nil entry, what a failed
// Compile returns, is an error.
func Link(entry *Module, resolve Resolver) (*Module, error) {
	if entry == nil {
		return nil, errNilLink
	}
	if entry.code.Module.Links == nil {
		return entry, nil
	}
	mods := map[*bytecode.Function]*Module{entry.code: entry}
	opts := engine.LinkOptions{
		EarlyExports: func(code *bytecode.Function) (*bytecode.Function, error) {
			return mods[code].earlyExports()
		},
		// The referrer of import() in a module's code, and the module of its
		// import.meta (Importer).
		HostDefined: func(code *bytecode.Function) any { return mods[code] },
	}
	if resolve != nil {
		opts.Resolve = func(referrer *bytecode.Function, request int) (*bytecode.Function, error) {
			dep, err := resolve(mods[referrer], referrer.Module.Links.Requests[request].Specifier)
			if err != nil || dep == nil {
				return nil, err
			}
			if _, ok := mods[dep.code]; !ok {
				mods[dep.code] = dep
			}
			return dep.code, nil
		}
	}
	g, err := engine.LinkModules(entry.code, opts)
	if err != nil {
		return nil, linkError(err)
	}
	l := &linked{g: g, exports: g.Exports()}
	l.bindings = make([]engine.ModuleBinding, len(l.exports))
	for i, name := range l.exports {
		l.bindings[i], _ = g.Binding(name)
	}
	return &Module{code: entry.code, graph: l, ts: entry.ts}, nil
}

// earlyExports returns the module compiled with its exported functions
// assumed to run before its body (syntax.Options.EarlyExports). Two Links
// that race compile it twice and keep one.
func (m *Module) earlyExports() (*bytecode.Function, error) {
	if code := m.early.Load(); code != nil {
		return code, nil
	}
	mod, err := syntax.ParseModule(m.code.Source.Name, m.code.Source.Src, syntax.Options{EarlyExports: true, TypeScript: m.ts})
	if err != nil {
		return nil, syntaxError(err)
	}
	code, err := compiler.CompileModule(mod)
	if err != nil {
		return nil, syntaxError(err)
	}
	if !m.early.CompareAndSwap(nil, code) {
		code = m.early.Load()
	}
	return code, nil
}

func linkError(err error) error {
	var le *engine.LinkError
	if !errors.As(err, &le) {
		return err
	}
	file := le.Module.Source.Name
	if le.Err != nil || le.Msg == "" {
		return &ResolveError{File: file, Line: le.Line, Column: le.Col, Specifier: le.Specifier, Err: le.Err}
	}
	return &SyntaxError{File: file, Line: le.Line, Column: le.Col, Message: le.Msg}
}

// Hook names an exported function, or a function below an exported object
// reached through own properties (members "openai", "decodeRequest" of
// export "protocols"). The export slot and the property keys are resolved
// here, once; the bindings stay live, so each Call reads the export and
// walks the members in the runtime at hand. An export the module does not
// declare, or re-export once linked, is ErrHookNotFound.
func (m *Module) Hook(export string, members ...string) (Hook, error) {
	slot, ok := m.code.Module.Export(export)
	if !ok {
		if m.graph == nil {
			return Hook{}, ErrHookNotFound
		}
		i, found := slices.BinarySearch(m.graph.exports, export)
		if !found {
			return Hook{}, ErrHookNotFound
		}
		slot = -1 - i
	}
	h := Hook{mod: m, slot: slot, name: export}
	if len(members) != 0 {
		h.keys = make([]engine.PropertyKey, len(members))
		for i, s := range members {
			h.keys[i] = engine.InternKey(s)
		}
		h.name = export + "." + strings.Join(members, ".")
	}
	return h, nil
}

// Hook is a resolved path to a function in a module (Module.Hook), which
// Call finds in a runtime that loaded the module. A Hook of an export the
// module declares itself also works in a runtime that loaded the Module
// Link returned for it; a re-exported name needs the Hook of that Module.
// The zero Hook names nothing.
type Hook struct {
	mod *Module
	// slot is the export's Env slot, or -1-i for export i of the module's
	// graph that it re-exports.
	slot int
	keys []engine.PropertyKey
	name string
}

// Name returns the path joined with dots ("protocols.openai.decodeRequest").
func (h Hook) Name() string { return h.name }

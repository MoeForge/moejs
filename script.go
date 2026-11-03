package moejs

import (
	"context"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/syntax"
)

// The engine compiles the source text of eval, the Function constructors
// and $262-style script evaluation through the compiler this package
// installs.
func init() { engine.SetCompiler(compiler.Hook{}) }

// Script is a compiled classic script. It is immutable: any number of
// runtimes may run it, concurrently, and a runtime may run it more than
// once.
type Script struct {
	name string
	code *bytecode.Function
}

// CompileScript parses and compiles a classic script. A script is sloppy
// mode code unless it starts with a "use strict" directive. Bad source,
// including features the engine does not support yet, is a *SyntaxError.
func CompileScript(name, source string) (*Script, error) {
	return compileScript(name, source, syntax.Options{})
}

// CompileScriptWith is CompileScript with the options of opts (see
// CompileWith). A TypeScript script is strict mode code.
func CompileScriptWith(name, source string, opts CompileOptions) (*Script, error) {
	return compileScript(name, source, syntax.Options{TypeScript: opts.TypeScript, Stop: stopFunc(opts.Context)})
}

func compileScript(name, source string, opts syntax.Options) (*Script, error) {
	if opts.Stop != nil {
		if err := opts.Stop(); err != nil {
			return nil, err
		}
	}
	s, err := syntax.ParseScript(name, source, opts)
	if err != nil {
		return nil, syntaxError(err)
	}
	code, err := compiler.CompileScriptStop(s, opts.Stop)
	if err != nil {
		return nil, syntaxError(err)
	}
	return &Script{name: name, code: code}, nil
}

// FunctionOptions are the options of CompileFunction and CompileFunctionTS:
// the kind of function.
type FunctionOptions struct {
	// Async compiles an async function, as the AsyncFunction constructor
	// does.
	Async bool
	// Generator compiles a generator function, as the GeneratorFunction
	// constructor does; with Async, an async generator function.
	Generator bool
	// Context bounds the compile, as CompileOptions.Context does.
	Context context.Context
}

// CompileFunction compiles a function from the source text of its
// parameter list and of its body, as the Function constructor does: params
// is the list without its parentheses ("a, b = 1", or "" for none), and the
// text is that of `function anonymous(params\n) {\nbody\n}`, in which
// neither part may end the other early. RunScript of the script returned
// creates the function, in the runtime's global environment, and returns
// it; CallFunction calls it. The script declares nothing, and the
// function's name, "anonymous", is not bound in its body. The function is
// sloppy mode code unless its body starts with a "use strict" directive.
// Bad source is a *SyntaxError, whose position is in the text above (the
// body starts on line 3). The script is immutable: any number of runtimes
// may run it, each getting a function of its own.
func CompileFunction(name, params, body string, opts FunctionOptions) (*Script, error) {
	return compileFunction(name, params, body, opts, false)
}

// CompileFunctionTS is CompileFunction for TypeScript, with CompileTS's
// rules: the parser erases the type syntax as it reads it, so parameters
// may have types and be optional, the body may declare types and use type
// assertions, and a *SyntaxError and a stack trace refer to the TypeScript
// text; TypeScript that has run-time semantics (enum, a namespace with
// values, a parameter property) is a *SyntaxError. The function is strict
// mode code, as the code of a module CompileTS compiles is.
func CompileFunctionTS(name, params, body string, opts FunctionOptions) (*Script, error) {
	return compileFunction(name, params, body, opts, true)
}

func compileFunction(name, params, body string, opts FunctionOptions, ts bool) (*Script, error) {
	stop := stopFunc(opts.Context)
	if stop != nil {
		if err := stop(); err != nil {
			return nil, err
		}
	}
	code, err := compiler.CompileFunction(name, params, body, opts.Generator, opts.Async, ts, stop)
	if err != nil {
		return nil, syntaxError(err)
	}
	return &Script{name: name, code: code}, nil
}

// Name returns the name given to CompileScript, CompileFunction or
// CompileFunctionTS.
func (s *Script) Name() string { return s.name }

func (*Script) referrer() {}

// RunScript runs s in the runtime's global environment and returns its
// completion value. Its var and function declarations become properties
// of the global object; its let, const and class declarations are global
// bindings that later scripts and the loaded module (before or after Load)
// see too. A declaration that conflicts with an existing global binding
// throws before anything runs. The jobs the script queued run before
// RunScript returns. A throw is an *Exception, an interrupt an
// *InterruptedError; a script whose bytecode the engine's verifier
// rejects (a compiler bug) does not run, and the error says why. A nil s,
// what a failed CompileScript returns, is an error.
func (rt *Runtime) RunScript(s *Script) (res Value, err error) {
	if s == nil {
		return engine.Undefined(), errNilScript
	}
	r := rt.realm
	defer func() {
		if err != nil {
			res = engine.Undefined()
		}
	}()
	defer rt.guard(&err, r.CallState(), len(rt.argStack))
	r.HoldJobs()
	if err = r.CheckInterrupt(); err == nil && s.code.ScriptOrModule {
		// The referrer of its import() calls.
		err = r.SetHostDefined(s.code, s)
	}
	if err == nil {
		res, err = r.RunScript(s.code)
	}
	return res, r.ReleaseJobs(err)
}

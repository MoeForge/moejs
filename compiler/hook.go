package compiler

import (
	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Hook compiles the source text the engine evaluates at run time: eval
// code, the functions of the Function constructors and Realm.EvalScript's
// scripts. It satisfies engine.Compiler (engine.SetCompiler), which the
// root package installs, so that the engine needs neither the parser nor
// the compiler. Its methods call stop, when not nil, before every
// 1024th statement of each statement list they parse, resolve and compile,
// the first included, and return the error it returns as is.
type Hook struct{}

// CompileScript parses and compiles a classic script.
func (Hook) CompileScript(name, src string, stop func() error) (*bytecode.Function, error) {
	s, err := syntax.ParseScript(name, src, syntax.Options{Stop: stop})
	if err != nil {
		return nil, err
	}
	return CompileScriptStop(s, stop)
}

// CompileEval parses and compiles eval code for the direct eval call site
// scope describes, or an indirect eval when scope is nil.
func (Hook) CompileEval(name, src string, scope *bytecode.EvalScope, stop func() error) (*bytecode.Function, error) {
	e, err := syntax.ParseEval(name, src, evalEnv(scope), syntax.Options{Stop: stop})
	if err != nil {
		return nil, err
	}
	return compileEvalStop(e, stop)
}

// evalEnv converts the EvalScope the compiler recorded at a call site back
// to what the parser resolves eval code against.
func evalEnv(sc *bytecode.EvalScope) *syntax.EvalEnv {
	if sc == nil {
		return nil
	}
	n := 0
	for l := sc.Env; l != nil; l = l.Outer {
		n++
	}
	env := &syntax.EvalEnv{
		Levels: make([]syntax.EvalLevel, 0, n),
		Var:    sc.Var, This: sc.This, Fields: sc.Fields,
		FuncKind: syntax.FuncKind(sc.FuncKind), Derived: sc.Derived,
		Strict: sc.Strict, InParams: sc.InParams, Module: sc.Module,
	}
	for l := sc.Env; l != nil; l = l.Outer {
		env.Levels = append(env.Levels, evalLevel{l})
	}
	return env
}

// evalLevel is a bytecode.EvalLevel as a syntax.EvalLevel.
type evalLevel struct{ l *bytecode.EvalLevel }

func (l evalLevel) Kind() syntax.ScopeKind { return syntax.ScopeKind(l.l.Kind) }
func (l evalLevel) Len() int               { return len(l.l.Names) }
func (l evalLevel) Slot(name string) int   { return l.l.Slot(name) }

func (l evalLevel) Binding(i int) (string, syntax.BindKind, bool) {
	return l.l.Names[i], syntax.BindKind(l.l.Kinds[i]), l.l.TDZ[i]
}

// CompileFunction compiles the function CreateDynamicFunction (ES2025
// 20.2.1.1.1) builds from the parameter list params and body: the source
// text is `function anonymous(params\n) {\nbody\n}` (with the prefix of the
// kind), which must parse as that one function with params and body
// exactly where they were put, so that neither can end the other early.
// The template it returns is a script that declares nothing and returns
// the function, created in the global environment: the name anonymous is
// not bound.
func (Hook) CompileFunction(name, params, body string, generator, async bool, stop func() error) (*bytecode.Function, error) {
	return compileFunction(name, params, body, generator, async, false, stop)
}

// CompileFunction compiles a function from its parameter list and body as
// Hook.CompileFunction does, for a host, which runs the template it returns
// to create the function: with typeScript the source text is TypeScript,
// whose type syntax the parser erases, and the function is strict mode
// code, as a TypeScript module is. It calls stop, when not nil, as
// CompileModuleStop does.
func CompileFunction(name, params, body string, generator, async, typeScript bool, stop func() error) (*bytecode.Function, error) {
	return compileFunction(name, params, body, generator, async, typeScript, stop)
}

func compileFunction(name, params, body string, generator, async, typeScript bool, stop func() error) (fn *bytecode.Function, err error) {
	prefix := "function"
	switch {
	case generator && async:
		prefix = "async function*"
	case generator:
		prefix = "function*"
	case async:
		prefix = "async function"
	}
	head := prefix + " anonymous(" + params + "\n) "
	src := head + "{\n" + body + "\n}"
	s, err := syntax.ParseScript(name, src, syntax.Options{Stop: stop, TypeScript: typeScript})
	if err != nil {
		return nil, err
	}
	var fd *syntax.FuncDecl
	if len(s.Body) == 1 {
		fd, _ = s.Body[0].(*syntax.FuncDecl)
	}
	switch {
	case fd != nil && fd.Func.Body != nil && fd.Func.Body.Pos != len(head):
		return nil, dynamicError(s.File, "Arg string terminates parameters early")
	case fd == nil || fd.Pos != 0 || fd.End != len(src) || fd.Func.IsGenerator != generator || fd.Func.IsAsync != async:
		return nil, dynamicError(s.File, "Function body ends the function early")
	}
	return compileUnit(func(noHoist map[*syntax.Function]bool) (*bytecode.Function, error) {
		return compileDynamic(s, fd, name, src, stop, noHoist)
	})
}

// compileDynamic compiles CompileFunction's script s, which declares the
// function fd, without hoisting in the functions of noHoist.
func compileDynamic(s *syntax.Script, fd *syntax.FuncDecl, name, src string, stop func() error, noHoist map[*syntax.Function]bool) (fn *bytecode.Function, err error) {
	c := newCompiler(s.File, &scriptState{stop: stop}, noHoist)
	defer c.recover(&err)
	defer c.release()
	f := c.newFuncState(nil, nil, bytecode.KindScript, s.Scope)
	f.out.Strict = false
	f.out.Source = &bytecode.SourceInfo{Name: name, Src: src, Start: 0, End: len(src)}
	// The function's references to the script's one binding, its own
	// name, are global references.
	for _, b := range s.Scope.Bindings {
		c.locs[b] = location{slot: len(c.script.globals), reg: -1}
		c.script.globals = append(c.script.globals, b)
	}
	r := f.alloc()
	f.setPos(fd.Pos)
	f.emitClosure(r, f.compileFunction(fd.Func, "anonymous"))
	f.emitA(bytecode.Ret, r)
	return f.finish(), nil
}

// dynamicError is the SyntaxError of a parameter list or body a dynamic
// function's source text does not keep apart.
func dynamicError(file *syntax.File, msg string) error {
	return &Error{Name: file.Name, Line: 1, Col: 1, Msg: "SyntaxError: " + msg}
}

// Message returns the message a JS SyntaxError for e carries: Msg without
// its "SyntaxError: " prefix.
func (e *Error) Message() string {
	const prefix = "SyntaxError: "
	if len(e.Msg) >= len(prefix) && e.Msg[:len(prefix)] == prefix {
		return e.Msg[len(prefix):]
	}
	return e.Msg
}

package syntax

// Direct eval (ES2025 19.2.1.1 PerformEval). A direct eval call runs code
// compiled at run time in the environment of its call site, so the resolver
// keeps that environment name-addressable: every binding on the scope chain
// of a call site lives in a closure environment (Captured) and has its TDZ
// checked, and a sloppy function containing a call gets a %evalvars binding
// for the object that receives the var and function declarations of its
// eval code. The compiler describes the chain in the call's
// bytecode.EvalScope; ParseEval rebuilds it as scopes and resolves the eval
// code against them. Code without a direct eval pays nothing.

// isDirectEval reports whether e is a direct eval call: `eval(...)` with an
// identifier callee, not optional. Whether the callee is %eval% is decided
// when the call runs.
func isDirectEval(e *CallExpr) bool {
	id, ok := e.Callee.(*Ident)
	return ok && id.Name == "eval" && !e.Optional
}

// hasDirectEval reports whether stmts contain a direct eval call outside
// any nested function.
func hasDirectEval(stmts []Stmt) bool {
	found := false
	for _, st := range stmts {
		Inspect(st, func(n Node) bool {
			switch n := n.(type) {
			case *Function:
				return false
			case *CallExpr:
				if isDirectEval(n) {
					found = true
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// directEval records a direct eval call at the current position. The eval
// code may read this, new.target, super and arguments of the function
// supplying them, so their bindings are declared; a sloppy function's
// variable environment gets its %evalvars binding (the eval code of a
// sloppy eval shares its caller's); and the scope is remembered for
// markEvalSites. A call in the scope and function of the previous one finds
// all of it done.
func (r *resolver) directEval() {
	frame := r.frames[len(r.frames)-1]
	if n := len(r.evalSites); n > 0 && r.evalSites[n-1] == r.scope && r.evalFrame == frame {
		return
	}
	r.markEval()
	last := len(r.frames) - 1
	for i := range r.inParams {
		if r.inParams[i].frame == last {
			r.inParams[i].eval = true
		}
	}
	r.markThis()
	if i := r.nonArrowFrame(); i > 0 {
		fn := r.frames[i].fn
		r.hidden(i, &fn.NewTargetBinding, hiddenNewTarget, false)
		if !r.inClassInitializer() {
			r.argumentsBinding()
		}
		switch fn.Kind {
		case FuncMethod, FuncGetter, FuncSetter, FuncClassConstructor, FuncClassFields, FuncClassStatic:
			r.hidden(i, &fn.HomeBinding, hiddenHome, false)
		}
		if fn.Kind == FuncClassConstructor && fn.Derived {
			r.hidden(i, &fn.ThisBinding, hiddenThis, true)
			r.hidden(i, &fn.CalleeBinding, hiddenCallee, false)
		}
	}
	if fn := r.currentFn(); fn != nil && fn != r.evalFn && !r.strict() {
		// The variable environment: the body's when it is kept apart from
		// the parameters (a call in the parameter list comes before the body
		// scope exists and binds in the parameters' one).
		s := fn.Scope
		if fn.Body != nil && fn.Body.Scope != nil {
			s = fn.Body.Scope
		}
		if s.Lookup(hiddenEvalVars) == nil {
			s.add(&Binding{Name: hiddenEvalVars, Kind: BindHidden, Pos: fn.Pos, Captured: true})
		}
	}
	r.evalSites = append(r.evalSites, r.scope)
	r.evalFrame = frame
}

// markEvalSites makes every binding visible from a direct eval call site
// reachable by the eval code: captured (in a closure environment, so that
// the eval code finds it by its slot) and, when it has one, TDZ-checked (the
// eval code may run before the declaration). Only the scopes on the chains
// of the call sites pay, each once: the chains of many calls share most of
// their scopes.
func (r *resolver) markEvalSites() {
	for _, s := range r.evalSites {
		for ; s != nil && !s.evalDone; s = s.Parent {
			s.evalDone = true
			for _, b := range s.Bindings {
				b.Captured = true
				if b.hasTDZ() {
					b.NeedsTDZ = true
				}
			}
		}
	}
}

// EvalEnv describes the environment of a direct eval call site for
// ParseEval, as recorded by the compiler (see bytecode.EvalScope, which
// holds the same data untyped).
type EvalEnv struct {
	// Levels are the scopes with a closure environment, innermost first.
	Levels []EvalLevel
	// Var is the level of the caller's variable environment (the scope
	// holding %evalvars), -1 for the global one. This is the outermost level
	// of the function supplying this (-1 outside any function), Fields the
	// level of the %fields binding a derived constructor's super() reads
	// (-1 when there is none).
	Var, This, Fields int
	// FuncKind is the kind of the function supplying this; Derived marks a
	// derived class constructor.
	FuncKind FuncKind
	Derived  bool
	// Strict is the strictness of the calling code; InParams marks a call
	// in the parameter list of a sloppy function with parameter expressions,
	// whose variable environment is outside the parameters; Module marks a
	// call in module code.
	Strict, InParams, Module bool
}

// EvalLevel is one level of an EvalEnv: a closure environment, whose slots
// are named by the bindings owning them. ParseEval looks up the names the
// eval code uses rather than reading every slot, so that small eval code
// is cheap next to a large environment.
type EvalLevel interface {
	// Kind is the kind of the scope owning the environment; Len is the
	// number of its slots.
	Kind() ScopeKind
	Len() int
	// Slot returns the slot of the binding named name, or -1 when there is
	// none.
	Slot(name string) int
	// Binding describes slot i: its binding's name ("" for a slot without
	// one), kind and whether it has a TDZ.
	Binding(i int) (name string, kind BindKind, tdz bool)
}

// Eval is parsed and resolved eval code. Its body is the body of Func, a
// synthetic arrow function whose scope (Scope) is the eval code's lexical
// environment and, for strict code, its variable environment.
type Eval struct {
	Program
	// Strict marks strict eval code; Module marks eval code called in
	// module code; HasDirectEval marks code with a direct eval call, at any
	// depth.
	Strict, Module, HasDirectEval bool
	Func                          *Function
	// Env is the environment the code was resolved against. Levels are the
	// scopes of its levels, innermost first, holding the bindings the code
	// may look up: every one is Captured, its Slot the environment slot.
	Env    *EvalEnv
	Levels []*Scope
	// This is the synthetic function standing for the caller's function of
	// EvalEnv.This (nil at the top level): its hidden bindings are those of
	// the levels. FieldsBinding is the class's %fields binding for a derived
	// constructor.
	This          *Function
	FieldsBinding *Binding
	// The declarations of sloppy eval code in its caller's variable
	// environment (EvalDeclarationInstantiation): the var names, the
	// top-level functions to instantiate (the last declaration of each name,
	// in source order) and the names only Annex B.3.2.3 block functions
	// declare. They have no binding in Scope, and their references resolve
	// dynamically through the %evalvars object (or the global object).
	Vars   []string
	Funcs  []*FuncDecl
	AnnexB []string
}

// ParseEval parses and resolves the eval code src named name (the
// referrer) for a direct eval call site described by env, or an indirect
// eval when env is nil. Eval code is a Script: return is not allowed,
// await is an identifier, HTML-like comments are allowed. It is strict when
// the caller is or when it has a "use strict" directive.
func ParseEval(name, src string, env *EvalEnv, opts Options) (*Eval, error) {
	if env == nil {
		env = &EvalEnv{Var: -1, This: -1, Fields: -1}
	}
	p := newParser(name, src, opts.Stop, false)
	if env.Strict || env.Module {
		p.strict = true
	}
	if env.This >= 0 {
		p.nonArrowDepth = 1 // new.target is allowed
	}
	e := &Eval{Module: env.Module, Env: env}
	if err := p.run(func() { e.Program = p.parseProgram(); e.Strict = p.strict }); err != nil {
		return nil, err
	}
	if err := resolveEval(e, env, opts.Stop); err != nil {
		return nil, err
	}
	return e, nil
}

func resolveEval(e *Eval, env *EvalEnv, stop func() error) error {
	r := &resolver{file: e.File, stop: stop, scriptStrict: e.Strict}
	return r.run(func() {
		var this *Function
		if env.This >= 0 {
			this = &Function{Kind: env.FuncKind, Derived: env.Derived, IsStrict: env.Strict}
		}
		// The names the code may look up, unless the environment is so
		// small that binding all of it costs less than listing them.
		var names []string
		var used map[string]bool // nil: all
		if n := env.slots(); n > 16 {
			names, used = evalNames(e.Body)
		}
		e.Levels = make([]*Scope, len(env.Levels))
		for i := len(env.Levels) - 1; i >= 0; i-- {
			l := env.Levels[i]
			// The levels' bindings are marked already.
			s := &Scope{Kind: l.Kind(), Parent: r.scope, evalDone: true}
			if i <= env.This {
				s.Func = this
			}
			level := func(j int, name string) {
				_, kind, tdz := l.Binding(j)
				b := &Binding{Name: name, Kind: kind, Captured: true, NeedsTDZ: tdz}
				s.add(b)
				b.Slot = j
			}
			// Whichever is shorter: the level's slots or the names.
			if n := l.Len(); used == nil || n <= len(names) {
				for j := range n {
					// Computed field keys all share one name, and nothing
					// resolves them by it.
					name, _, _ := l.Binding(j)
					if used == nil && name != "" && name != hiddenKey || used[name] {
						level(j, name)
					}
				}
			} else {
				for _, name := range names {
					if j := l.Slot(name); j >= 0 {
						level(j, name)
					}
				}
			}
			e.Levels[i] = s
			r.scope = s
		}
		if this != nil {
			s := e.Levels[env.This]
			this.Scope = s
			this.NewTargetBinding = s.Lookup(hiddenNewTarget)
			this.HomeBinding = s.Lookup(hiddenHome)
			if env.Derived {
				this.ThisBinding = s.Lookup(hiddenThis)
				this.CalleeBinding = s.Lookup(hiddenCallee)
			}
			r.frames = append(r.frames, &fnFrame{fn: this})
			e.This = this
			r.evalThis = this
		}
		if env.Fields >= 0 {
			e.FieldsBinding = e.Levels[env.Fields].Lookup(hiddenFields)
		}

		fn := &Function{Span: e.Span, Kind: FuncArrow, IsArrow: true, IsStrict: e.Strict, HasSimpleParams: true, Body: &BlockStmt{Span: e.Span, Body: e.Body}}
		es := r.newScope(ScopeFunction, fn)
		es.Func = fn
		fn.Scope = es
		e.Func = fn
		e.Scope = es
		frame := &fnFrame{fn: fn}
		r.frames = append(r.frames, frame)
		r.evalFn = fn
		r.scope = es
		r.hoist(es, e.Body, nil, true)
		r.declareLexical(es, e.Body)
		if !e.Strict {
			r.annexBFuncs(es, nil, e.Body)
			r.evalDecls(env, e, es)
		}
		// The eval code's functions (sloppy code's are not hoisted
		// declarations here) and nested evals may run before a lexical
		// declaration.
		for _, b := range es.Bindings {
			if b.Kind.IsLexical() {
				b.NeedsTDZ = true
			}
		}
		r.stmts(e.Body)
		r.finishFrame(frame)
		r.markEvalSites()
		e.HasDirectEval = len(r.evalSites) > 0
	})
}

// slots returns the number of slots of env's levels.
func (env *EvalEnv) slots() int {
	n := 0
	for _, l := range env.Levels {
		n += l.Len()
	}
	return n
}

// evalLookups are the names eval code may look up in its caller's scopes
// without writing them: the with statement's object (the one binding of a
// with scope), the hidden bindings its this, super, new.target and private
// methods reach, and the arguments object and module default export.
var evalLookups = []string{"%with", hiddenThis, hiddenCallee, hiddenNewTarget, hiddenHome,
	hiddenFields, hiddenBrand, hiddenArgs, hiddenEvalVars, "arguments", "*default*"}

// evalNames lists the names eval code body may look up in its caller's
// scopes, in order of first use, and returns them as a set too: its
// identifiers (declared names included, which the caller's may conflict
// with), its private names and evalLookups.
func evalNames(body []Stmt) ([]string, map[string]bool) {
	used := make(map[string]bool, len(evalLookups))
	names := make([]string, 0, len(evalLookups))
	add := func(name string) {
		if !used[name] {
			used[name] = true
			names = append(names, name)
		}
	}
	for _, name := range evalLookups {
		add(name)
	}
	for _, st := range body {
		Inspect(st, func(n Node) bool {
			switch n := n.(type) {
			case *Ident:
				add(n.Name)
			case *PrivateName:
				add("#" + n.Name)
			}
			return true
		})
	}
	return names, used
}

// evalDecls implements the checks of EvalDeclarationInstantiation (ES2025
// 19.2.1.3) and Annex B.3.2.3 and B.3.4 that the caller's scopes decide for
// the var and function declarations of sloppy eval code es, and moves those
// declarations out of es into e.Vars, e.Funcs and e.AnnexB. The checks
// against the global environment (its lexical bindings, CanDeclareGlobalVar
// and CanDeclareGlobalFunction) are made when the code runs.
func (r *resolver) evalDecls(env *EvalEnv, e *Eval, es *Scope) {
	levels := e.Levels
	below := len(levels) // the levels between the eval code and its variable environment
	if env.Var >= 0 {
		below = env.Var
	}
	// shadowed reports whether a binding of the caller's scopes that a var
	// named name would conflict with exists. Object environments (with) do
	// not count; catch parameters do for Annex B functions only (B.3.4).
	shadowed := func(name string, annexB bool) bool {
		for _, s := range levels[:below] {
			if s.Kind == ScopeWith || s.Kind == ScopeCatch && !annexB {
				continue
			}
			if s.Lookup(name) != nil {
				return true
			}
		}
		if env.Var < 0 {
			return false
		}
		// The variable environment itself: a function's body-level lexical
		// declarations are in an environment below it, and so are the
		// parameters and the arguments object when the call is in the
		// parameter list (the function's own name is above it).
		b := levels[env.Var].Lookup(name)
		return b != nil && (b.Kind.IsLexical() || env.InParams && b.Kind != BindFuncName)
	}
	for fd := range r.annexB {
		if shadowed(fd.Func.Name.Name, true) {
			delete(r.annexB, fd)
		}
	}
	accepted := make(map[string]bool, len(r.annexB))
	for fd := range r.annexB {
		accepted[fd.Func.Name.Name] = true
	}
	for _, b := range es.Bindings {
		switch {
		case b.Kind == BindVar && b.AnnexB:
			if accepted[b.Name] {
				e.AnnexB = append(e.AnnexB, b.Name)
			}
		case b.Kind == BindVar || b.Kind == BindFunction:
			if shadowed(b.Name, false) {
				r.fail(b.Pos, "Identifier '"+b.Name+"' has already been declared")
			}
			if b.Kind == BindVar {
				e.Vars = append(e.Vars, b.Name)
			}
		}
	}
	// functionsToInitialize: the last declaration of each name, in source
	// order.
	seen := make(map[string]bool, len(es.Funcs))
	for i := len(es.Funcs) - 1; i >= 0; i-- {
		fd := es.Funcs[i]
		if name := fd.Func.Name.Name; !seen[name] {
			seen[name] = true
			e.Funcs = append(e.Funcs, fd)
		}
	}
	for i, j := 0, len(e.Funcs)-1; i < j; i, j = i+1, j-1 {
		e.Funcs[i], e.Funcs[j] = e.Funcs[j], e.Funcs[i]
	}
	kept := es.Bindings[:0]
	for _, b := range es.Bindings {
		if b.Kind == BindVar || b.Kind == BindFunction {
			continue
		}
		b.Slot = len(kept)
		kept = append(kept, b)
	}
	clear(es.Bindings[len(kept):])
	es.Bindings = kept
	es.names = nil
	if len(kept) > 8 {
		es.names = make(map[string]*Binding, len(kept))
		for _, b := range kept {
			es.names[b.Name] = b
		}
	}
	es.Funcs = nil
	r.evalVars = es
}

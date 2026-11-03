package compiler

import (
	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Direct eval (ES2025 19.2.1.1 PerformEval). A direct eval call compiles to
// CallEval, whose Extra.Evals entry describes the environment chain of the
// call site: every binding on it is captured (syntax's markEvalSites), so the
// eval code, compiled when the call runs, finds each one by its Env hop
// count and slot. The var and function declarations of sloppy eval code go
// to its caller's variable environment: the global object, or the %evalvars
// object of the calling function, through which that function's names
// resolve as through a with statement's object (sloppy.go). Only functions
// containing a direct eval pay: other code compiles as before.

// isDirectEval reports whether c is a direct eval call site (syntax's
// isDirectEval): `eval(...)` with an identifier callee, not optional.
func isDirectEval(c *syntax.CallExpr) bool {
	id, ok := c.Callee.(*syntax.Ident)
	return ok && id.Name == "eval" && !c.Optional
}

// evalSite records the environment of the direct eval call at pos for the
// eval code it compiles and returns its index in Extra.Evals.
func (f *funcState) evalSite(pos int) uint32 {
	m := f.c.evalMemo(f)
	env := m.node(f)
	strict := f.out.Strict
	// A call in the parameter list of a function whose body has a variable
	// environment of its own (syntax's paramScope).
	fn := f.fn
	inParams := !strict && fn != nil && fn.Body != nil && fn.Body.Scope != nil && pos < fn.Body.Pos
	key := evalSiteKey{f, env, inParams}
	if x, ok := m.sites[key]; ok {
		return x
	}
	sc := bytecode.EvalScope{Var: -1, This: -1, Fields: -1, Strict: strict, InParams: inParams, Module: m.module}
	if env != nil {
		sc.Env = env.level
		if !strict && env.vars >= 0 {
			sc.Var = env.depth - env.vars
		}
		// The levels of the function supplying this, the outermost one;
		// and the one of the %fields binding of its class.
		g := f.thisFunc()
		if fn := g.fn; fn != nil {
			sc.FuncKind = uint8(fn.Kind)
			sc.Derived = fn.Kind == syntax.FuncClassConstructor && fn.Derived
			for _, es := range g.envStack {
				if es.scope.Func == fn {
					if n := m.nodes[es]; n != nil {
						sc.This = env.depth - n.depth
					}
					break
				}
			}
			if c := f.c.classOf[fn]; sc.Derived && c != nil && c.FieldsBinding != nil {
				if n := m.nodes[f.c.locs[c.FieldsBinding].env]; n != nil {
					sc.Fields = env.depth - n.depth
				}
			}
		}
	} else if fn := f.thisFunc().fn; fn != nil {
		sc.FuncKind = uint8(fn.Kind)
		sc.Derived = fn.Kind == syntax.FuncClassConstructor && fn.Derived
	}
	p := m.scopes[sc]
	if p == nil {
		p = &sc
		m.scopes[sc] = p
	}
	x := f.extra()
	x.Evals = append(x.Evals, p)
	i := uint32(len(x.Evals) - 1)
	m.sites[key] = i
	return i
}

// evalMemo shares the EvalLevels and EvalScopes of the direct eval call
// sites of a compilation (see bytecode.EvalScope): each environment on the
// chain of a call site gets one EvalLevel, linked to the one outside it, and
// each function one EvalScope per scope it has calls in. Recording a call
// then costs a map lookup rather than a copy of every environment it sees,
// which calls in scopes with many bindings, or nested many deep, would
// multiply. Everything it builds is part of the compiled code, immutable
// once the compilation is done.
type evalMemo struct {
	nodes map[*envScope]*evalNode
	// outer is the innermost level visible from a function outside its own
	// environments: its parent's, which stays the same while it compiles.
	outer  map[*funcState]*evalNode
	sites  map[evalSiteKey]uint32 // Extra.Evals index
	scopes map[bytecode.EvalScope]*bytecode.EvalScope
	module bool // module code
}

// evalNode is the EvalLevel of an environment.
type evalNode struct {
	level *bytecode.EvalLevel
	depth int // the number of levels outside it
	vars  int // the depth of the nearest level at or outside it with a %evalvars binding, -1 for none
}

// evalSiteKey identifies the direct eval call sites of a function that
// share an EvalScope: those whose innermost level is env.
type evalSiteKey struct {
	f        *funcState
	env      *evalNode
	inParams bool
}

func (c *compiler) evalMemo(f *funcState) *evalMemo {
	if c.script == nil {
		c.script = &scriptState{}
	}
	if c.script.evals == nil {
		root := f
		for root.parent != nil {
			root = root.parent
		}
		c.script.evals = &evalMemo{
			nodes:  make(map[*envScope]*evalNode),
			outer:  make(map[*funcState]*evalNode),
			sites:  make(map[evalSiteKey]uint32),
			scopes: make(map[bytecode.EvalScope]*bytecode.EvalScope),
			module: root.kind == bytecode.KindModule,
		}
	}
	return c.script.evals
}

// node returns the level of the innermost environment visible in f (nil
// when there is none), making those of the chain that have none yet.
func (m *evalMemo) node(f *funcState) *evalNode {
	var n *evalNode
	i := len(f.envStack)
	for ; i > 0; i-- {
		if n = m.nodes[f.envStack[i-1]]; n != nil {
			break
		}
	}
	if i == 0 && f.parent != nil {
		var ok bool
		if n, ok = m.outer[f]; !ok {
			n = m.node(f.parent)
			m.outer[f] = n
		}
	}
	for _, es := range f.envStack[i:] {
		l := f.c.evalLevel(es)
		if n != nil {
			l.Outer = n.level
		}
		n = m.add(es, l, n)
	}
	return n
}

// add records l as the level of es, outer the level outside it (l.Outer's).
func (m *evalMemo) add(es *envScope, l *bytecode.EvalLevel, outer *evalNode) *evalNode {
	n := &evalNode{level: l, vars: -1}
	if outer != nil {
		n.depth = outer.depth + 1
		n.vars = outer.vars
	}
	if es.scope.Lookup(evalVarsName) != nil {
		n.vars = n.depth
	}
	m.nodes[es] = n
	return n
}

// evalLevel describes the environment es: the binding owning each slot.
func (c *compiler) evalLevel(es *envScope) *bytecode.EvalLevel {
	l := &bytecode.EvalLevel{
		Kind:  uint8(es.scope.Kind),
		Names: make([]string, es.size),
		Kinds: make([]uint8, es.size),
		TDZ:   make([]bool, es.size),
	}
	for _, b := range es.scope.Bindings {
		if loc, ok := c.locs[b]; ok && loc.env == es {
			l.Names[loc.slot] = b.Name
			l.Kinds[loc.slot] = uint8(b.Kind)
			l.TDZ[loc.slot] = b.NeedsTDZ
		}
	}
	return l
}

// CompileEval compiles parsed eval code into the arrow function template
// the engine calls in the environment of the eval (the call site's Env for
// a direct eval, none for an indirect one) with its this value; the call
// returns the completion value. The global var and function declarations of
// sloppy eval code are listed in Extra.Globals (Eval set), for the engine's
// EvalDeclarationInstantiation before the call; the eval code itself
// creates those that go to a function's variable environment.
func CompileEval(e *syntax.Eval) (*bytecode.Function, error) {
	return compileEvalStop(e, nil)
}

// compileEvalStop is CompileEval, calling stop as scriptState.stop.
func compileEvalStop(e *syntax.Eval, stop func() error) (*bytecode.Function, error) {
	return compileUnit(func(noHoist map[*syntax.Function]bool) (*bytecode.Function, error) {
		return compileEvalWith(e, stop, noHoist)
	})
}

// compileEvalWith is compileEvalStop without hoisting in the functions of
// noHoist.
func compileEvalWith(e *syntax.Eval, stop func() error, noHoist map[*syntax.Function]bool) (fn *bytecode.Function, err error) {
	c := newCompiler(e.File, &scriptState{stop: stop}, noHoist)
	defer c.recover(&err)
	defer c.release()
	// The caller: a script or module top level and, when the eval code has
	// one, the function supplying this, holding the levels' environments.
	kind := bytecode.KindScript
	if e.Module {
		kind = bytecode.KindModule
	}
	top := c.newFuncState(nil, nil, kind, nil)
	p := top
	if e.This != nil {
		p = c.newFuncState(p, e.This, bytecode.KindNormal, nil)
		if e.FieldsBinding != nil {
			c.classOf = map[*syntax.Function]*syntax.Class{e.This: {FieldsBinding: e.FieldsBinding}}
		}
	}
	var vars *syntax.Scope // the caller's variable environment, when a function's
	var outer *evalNode
	for i := len(e.Levels) - 1; i >= 0; i-- {
		s := e.Levels[i]
		es := &envScope{scope: s, size: e.Env.Levels[i].Len()}
		for _, b := range s.Bindings {
			c.locs[b] = location{env: es, slot: b.Slot, reg: -1}
		}
		p.envStack = append(p.envStack, es)
		// The eval code's own direct evals see the whole levels.
		if e.HasDirectEval {
			outer = c.evalMemo(p).add(es, levelOf(e.Env.Levels[i], outer), outer)
		}
		if s.Kind == syntax.ScopeWith || s.Lookup(evalVarsName) != nil {
			c.script.withs = append(c.script.withs, s)
		}
		if s.Lookup(evalVarsName) != nil {
			vars = s
		}
	}

	f := c.newFuncState(p, e.Func, bytecode.KindArrow, e.Scope)
	f.out.Strict = e.Strict
	f.out.Source = &bytecode.SourceInfo{Name: e.File.Name, Src: e.File.Src, Start: 0, End: len(e.File.Src)}
	f.setPos(e.Pos)
	s := e.Scope
	if scopeNeedsEnv(s) {
		es := &envScope{scope: s}
		for _, b := range s.Bindings {
			if b.Captured {
				c.locs[b] = location{env: es, slot: es.size, reg: -1}
				es.size++
			}
		}
		f.ownEnv = es
		f.envStack = append(f.envStack, es)
	}
	f.completion = f.alloc()
	f.emitA(bytecode.LoadUndef, f.completion)
	varStart := f.nregs
	f.allocScopeRegs(s)
	if n := f.nregs - varStart; n > 0 {
		f.emitAB(bytecode.UndefRange, varStart, n)
	}
	f.initTDZ(s)
	switch {
	case e.Strict:
		f.instantiateFuncs(s)
	case vars != nil:
		f.evalDeclarations(e, vars)
	default:
		f.globalEvalDeclarations(e)
	}
	f.stmts(e.Body)
	f.emitA(bytecode.Ret, f.completion)
	out := f.finish()
	// The eval code is the root: its import() calls marked the caller's
	// stand-in.
	out.ScriptOrModule = top.out.ScriptOrModule
	return out, nil
}

// levelOf returns the EvalLevel l describes, whose Outer is outer's.
func levelOf(l syntax.EvalLevel, outer *evalNode) *bytecode.EvalLevel {
	var o *bytecode.EvalLevel
	if outer != nil {
		o = outer.level
	}
	if l, ok := l.(evalLevel); ok && l.l.Outer == o {
		return l.l
	}
	// Levels the compiler did not record.
	n := l.Len()
	bl := &bytecode.EvalLevel{Kind: uint8(l.Kind()), Names: make([]string, n), Kinds: make([]uint8, n), TDZ: make([]bool, n), Outer: o}
	for i := range n {
		name, kind, tdz := l.Binding(i)
		bl.Names[i], bl.Kinds[i], bl.TDZ[i] = name, uint8(kind), tdz
	}
	return bl
}

// evalDeclarations is EvalDeclarationInstantiation (ES2025 19.2.1.3) with
// Annex B.3.2.3 for sloppy eval code e whose variable environment is the
// function scope vs: a name that has a var or parameter binding there
// keeps it, any other gets a property of the %evalvars object, which the
// first eval code that declares such a name creates.
func (f *funcState) evalDeclarations(e *syntax.Eval, vs *syntax.Scope) {
	static := func(name string) *syntax.Binding {
		if b := vs.Lookup(name); b != nil && !b.Kind.IsLexical() && b.Kind != syntax.BindFuncName {
			return b
		}
		return nil
	}
	var names []string
	for _, list := range [][]string{e.AnnexB, e.Vars} {
		for _, name := range list {
			if static(name) == nil {
				names = append(names, name)
			}
		}
	}
	dynamic := len(names) > 0
	for _, fd := range e.Funcs {
		dynamic = dynamic || static(fd.Func.Name.Name) == nil
	}
	mark := f.nregs
	defer f.free(mark)
	obj := -1
	if dynamic {
		ev := vs.Lookup(evalVarsName)
		obj = f.alloc()
		f.loadLoc(ev, obj)
		have := f.newLabel()
		f.emitJump(bytecode.JmpNotUndef, obj, have)
		f.emitABx(bytecode.NewObject, obj, 0)
		t := f.alloc()
		f.emitA(bytecode.LoadNull, t)
		f.emitAB(bytecode.SetProto, obj, t)
		f.storeLoc(f.c.locs[ev], obj)
		f.bind(have)
		// The object, for the verifier, which proves DefineField's
		// instance only from an op that makes one (loadThisObject).
		f.emitAB(bytecode.ToObject, obj, obj)
	}
	for _, fd := range e.Funcs {
		name := fd.Func.Name.Name
		m := f.nregs
		v := f.alloc()
		f.setPos(fd.Pos)
		f.emitClosure(v, f.compileFunction(fd.Func, name))
		if b := static(name); b != nil {
			f.storeLoc(f.c.locs[b], v)
		} else {
			f.emitAB(bytecode.DefineField, obj, v)
			f.emitExtra(bytecode.ExtraArg(f.nameConst(name), f.newIC()))
		}
		f.free(m)
	}
	if len(names) == 0 {
		return
	}
	undef := f.alloc()
	f.emitA(bytecode.LoadUndef, undef)
	for _, name := range names {
		skip := f.newLabel()
		f.emitJump(bytecode.JmpWith, obj, skip)
		f.emitExtra(uint32(f.nameConst(name)))
		f.emitAB(bytecode.DefineField, obj, undef)
		f.emitExtra(bytecode.ExtraArg(f.nameConst(name), f.newIC()))
		f.bind(skip)
	}
}

// globalEvalDeclarations lists the declarations of sloppy eval code e whose
// variable environment is the global one for the engine, which checks and
// creates them before the code runs, and stores its functions.
func (f *funcState) globalEvalDeclarations(e *syntax.Eval) {
	if len(e.Vars)+len(e.Funcs)+len(e.AnnexB) == 0 {
		return
	}
	gn := f.globals()
	gn.Eval = true
	gn.Var = e.Vars
	gn.AnnexB = e.AnnexB
	for _, fd := range e.Funcs {
		gn.Function = append(gn.Function, fd.Func.Name.Name)
	}
	for _, fd := range e.Funcs {
		name := fd.Func.Name.Name
		mark := f.nregs
		v := f.alloc()
		f.setPos(fd.Pos)
		f.emitClosure(v, f.compileFunction(fd.Func, name))
		f.setGlobal(name, v)
		f.free(mark)
	}
}

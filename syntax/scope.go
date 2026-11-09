package syntax

import (
	"math"
	"slices"
)

// ScopeKind classifies scopes.
type ScopeKind uint8

const (
	ScopeModule   ScopeKind = iota // module top level
	ScopeScript                    // script top level: its declarations are global (see CompileScript)
	ScopeFunction                  // parameters, var declarations and body-level lexical declarations
	ScopeBlock                     // block or switch case block with lexical declarations
	ScopeCatch                     // catch parameter
	ScopeFor                       // for/for-in/for-of head with let/const
	ScopeClass                     // class body: inner name, private names and hidden class bindings
	ScopeWith                      // with statement body: its one hidden binding holds the object
)

func (k ScopeKind) String() string {
	return [...]string{"module", "script", "function", "block", "catch", "for", "class", "with"}[k]
}

// BindKind classifies bindings.
type BindKind uint8

const (
	BindVar      BindKind = iota // var, hoisted to the function/module scope
	BindLet                      // let
	BindConst                    // const
	BindParam                    // formal parameter
	BindFunction                 // function declaration, instantiated at scope entry (see Scope.Funcs)
	BindCatch                    // catch parameter
	BindClass                    // class declaration (the outer, mutable binding)
	BindFuncName                 // name of a named function expression, immutable, visible inside it
	BindImport                   // named or default import binding: immutable, reads the exporter's binding
	BindImportNS                 // import * as binding: immutable, holds the module namespace object
	BindPrivate                  // private name "#x" of a class (ScopeClass)
	BindHidden                   // resolver-created binding of the class/super machinery, named "%..."
	BindArgs                     // the function's arguments object (Function.ArgumentsBinding)
)

func (k BindKind) String() string {
	return [...]string{"var", "let", "const", "param", "function", "catch", "class", "funcname", "import", "namespace", "private", "hidden", "arguments"}[k]
}

// IsLexical reports whether the binding is a let/const/class binding, which
// has a temporal dead zone.
func (k BindKind) IsLexical() bool { return k == BindLet || k == BindConst || k == BindClass }

// Binding is a declared name. Every Ident that declares or references it
// points here.
type Binding struct {
	Name     string
	Kind     BindKind
	Scope    *Scope // declaring scope
	Slot     int    // index in Scope.Bindings
	Pos      int    // position of the (first) declaring identifier
	Captured bool   // referenced from a function nested inside the declaring function
	NeedsTDZ bool   // let/const/class that may be read before initialisation
	Exported bool   // module binding named by an export entry
	// AnnexB marks, on the binding of a block-level function declaration of
	// sloppy code, that evaluating the declaration also assigns the var
	// binding of the same name in the enclosing function or script scope
	// (Annex B.3.2); on a script-scope var binding, that only such
	// declarations create it.
	AnnexB bool
	// Assigned marks a binding that code other than its own declaration
	// may write: an assignment or update, a destructuring or for-in/of
	// target, a var declaration with an initializer or a function
	// declaration of the same name (eval code, which may write any binding
	// in reach, is HasDirectEval on the functions around it). The compiler
	// lets the arguments object of a function none of whose parameters is
	// Assigned alias them lazily (MapArguments).
	Assigned bool

	initEnd int // end offset of the initialising declarator (lexical bindings)
}

// Scope is a node of the scope tree.
type Scope struct {
	Kind     ScopeKind
	heritage bool // a class scope whose heritage is being resolved: its private names are not visible
	evalDone bool // markEvalSites has marked the scope, and so its ancestors
	Parent   *Scope
	Func     *Function // enclosing function; nil at module/script level
	Node     Node      // owning node: *Module, *Script, *Function, *BlockStmt, *SwitchStmt, *TryStmt, *ForStmt, *ForInOfStmt, *Class, *WithStmt
	Bindings []*Binding
	Funcs    []*FuncDecl // hoisted function declarations to instantiate on entry, in source order
	Children []*Scope

	names map[string]*Binding // built once the scope grows past a handful of bindings
}

// Lookup finds a binding declared directly in s.
func (s *Scope) Lookup(name string) *Binding {
	if s.names != nil {
		return s.names[name]
	}
	for _, b := range s.Bindings {
		if b.Name == name {
			return b
		}
	}
	return nil
}

// Resolve finds the nearest binding for name in s or its ancestors.
func (s *Scope) Resolve(name string) *Binding {
	for ; s != nil; s = s.Parent {
		if b := s.Lookup(name); b != nil {
			return b
		}
	}
	return nil
}

func (s *Scope) add(b *Binding) {
	b.Scope = s
	b.Slot = len(s.Bindings)
	s.Bindings = append(s.Bindings, b)
	if s.names != nil {
		s.names[b.Name] = b
		return
	}
	if len(s.Bindings) > 8 {
		s.names = make(map[string]*Binding, 32)
		for _, b := range s.Bindings {
			s.names[b.Name] = b
		}
	}
}

// ---- Resolver ---------------------------------------------------------------

// resolveDeep is the number of scopes resolve walks up from a reference
// before it memoizes what it finds.
const resolveDeep = 32

// deepKey is a name resolved from a scope (resolver.deep).
type deepKey struct {
	s    *Scope
	name string
}

// resolve is r.scope.Resolve(name) for an identifier reference. Past
// resolveDeep scopes it remembers, for each scope it walks past, what the
// name resolves to from there, so that the references of a nesting
// thousands of scopes deep, each naming something far up, do not walk to
// the top one by one, in time quadratic in the depth. That holds because
// the scopes around a reference have all their bindings by the time it is
// resolved, but for arguments and the hidden ones, which no identifier
// reference names here.
func (r *resolver) resolve(name string) *Binding {
	s := r.scope
	for i := 0; s != nil; i++ {
		if b := s.Lookup(name); b != nil {
			return b
		}
		s = s.Parent
		if i == resolveDeep && s != nil {
			return r.resolveDeep(s, name)
		}
	}
	return nil
}

// resolveDeep is resolve from s on, through r.deep.
func (r *resolver) resolveDeep(s *Scope, name string) *Binding {
	if r.deep == nil {
		r.deep = make(map[deepKey]*Binding)
	}
	from := s
	var b *Binding
	for ; s != nil; s = s.Parent {
		if found, ok := r.deep[deepKey{s, name}]; ok {
			b = found
			break
		}
		if b = s.Lookup(name); b != nil {
			break
		}
	}
	for ; from != s; from = from.Parent {
		r.deep[deepKey{from, name}] = b
	}
	return b
}

// fnFrame is one function on the resolver's stack. frames[0] is the module or
// script itself (fn == nil).
type fnFrame struct {
	fn      *Function
	hoisted bool     // a function declaration (reachable before its position)
	binding *Binding // the declaration's binding when hoisted
	// Per hoisted function directly nested in this frame: what it may reach
	// early. Used to decide TDZ for lexical bindings read from hoisted
	// functions.
	infos map[*Binding]*hoistInfo
}

// paramList is a function whose parameter list is being resolved
// (paramScope): refs holds the names referenced from it (nested functions
// included) that resolve to its parameter scope or beyond.
type paramList struct {
	frame int
	refs  map[string]bool
	eval  bool // a direct eval call appears in the parameter list
}

type hoistInfo struct {
	earlyRef int        // earliest position at which non-hoisted code references the function
	callees  []*Binding // hoisted functions referenced from inside it
	tdzRefs  []*Binding // lexical bindings it reads that are initialised before its own position
}

type resolver struct {
	file   *File
	stop   func() error // Options.Stop
	module *Module
	scope  *Scope
	// deep remembers what a name resolves to from a scope past
	// resolveDeep scopes up from a reference (resolve).
	deep     map[deepKey]*Binding
	frames   []*fnFrame
	varsIn   map[Node][]string // var names declared inside each block-like node
	tdzScope *Scope            // for-in/of head scope whose bindings are in TDZ while the RHS is evaluated
	inParams []paramList       // functions whose parameter list is being resolved, innermost last
	exports  map[string]bool
	// firstAwait is the position of the first await of the module's top
	// level, when Module.Async.
	firstAwait int
	err        *Error

	scriptStrict bool               // the script has a "use strict" directive
	earlyExports bool               // Options.EarlyExports
	withs        int                // with statements enclosing the code being resolved
	annexB       map[*FuncDecl]bool // block function declarations Annex B.3.2 applies to

	evalSites []*Scope  // scopes containing a direct eval call (see markEvalSites)
	evalFrame *fnFrame  // the frame of the last one
	evalFn    *Function // the eval code being resolved (resolveEval)
	evalThis  *Function // the function supplying the eval code's this (resolveEval)
	evalVars  *Scope    // the scope of sloppy eval code, whose var declarations it leaves out
}

func resolveModule(m *Module, opts Options) error {
	r := &resolver{file: m.File, stop: opts.Stop, earlyExports: opts.EarlyExports, module: m, exports: make(map[string]bool)}
	return r.run(func() {
		root := r.newScope(ScopeModule, m)
		m.Scope = root
		r.scope = root
		r.hoist(root, m.Body, nil, true)
		if m.ts != nil {
			r.declareAliases(root)
		}
		r.declareLexical(root, m.Body)
		r.stmts(m.Body)
		r.finishFrame(r.frames[0])
		r.markEvalSites()
		m.HasDirectEval = len(r.evalSites) > 0
		if m.ts != nil {
			r.elideTypeImports()
		}
	})
}

func resolveScript(s *Script, opts Options) error {
	r := &resolver{file: s.File, stop: opts.Stop, scriptStrict: s.Strict}
	return r.run(func() {
		root := r.newScope(ScopeScript, s)
		s.Scope = root
		r.scope = root
		r.hoist(root, s.Body, nil, true)
		r.declareLexical(root, s.Body)
		if !s.Strict {
			r.annexBFuncs(root, nil, s.Body)
		}
		r.stmts(s.Body)
		r.finishFrame(r.frames[0])
		r.markEvalSites()
		s.HasDirectEval = len(r.evalSites) > 0
	})
}

func (r *resolver) run(f func()) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			switch rec := rec.(type) {
			case bailout:
				err = r.err
			case stopped:
				err = rec.err
			default:
				panic(rec)
			}
		}
	}()
	r.frames = []*fnFrame{{}}
	f()
	return nil
}

func (r *resolver) fail(pos int, msg string) {
	r.err = r.file.errorAt(pos, msg)
	panic(bailout{})
}

func (r *resolver) currentFn() *Function { return r.frames[len(r.frames)-1].fn }

// strict reports whether the code being resolved is strict mode code.
func (r *resolver) strict() bool {
	if fn := r.currentFn(); fn != nil {
		return fn.IsStrict
	}
	return r.module != nil || r.scriptStrict
}

func (r *resolver) newScope(kind ScopeKind, node Node) *Scope {
	s := &Scope{Kind: kind, Parent: r.scope, Func: r.currentFn(), Node: node}
	if r.scope != nil {
		r.scope.Children = append(r.scope.Children, s)
	}
	return s
}

func (r *resolver) push(s *Scope) { r.scope = s }
func (r *resolver) pop()          { r.scope = r.scope.Parent }

// ---- Declarations -----------------------------------------------------------

func isVarLike(k BindKind) bool { return k == BindVar || k == BindFunction || k == BindParam }

// declare adds a binding to s or merges it with a compatible existing one.
func (r *resolver) declare(s *Scope, id *Ident, kind BindKind, initEnd int) *Binding {
	existing := s.Lookup(id.Name)
	if existing == nil {
		b := &Binding{Name: id.Name, Kind: kind, Pos: id.Pos, initEnd: initEnd}
		s.add(b)
		return b
	}
	if id.Name == "*default*" {
		r.fail(id.Pos, "Duplicate export of 'default'")
	}
	if isVarLike(kind) && isVarLike(existing.Kind) {
		// Function declarations are var-like only at function/script top
		// level; in modules and blocks they are lexical.
		funcLike := kind == BindFunction || existing.Kind == BindFunction
		if !funcLike || s.Kind == ScopeFunction || s.Kind == ScopeScript {
			if kind == BindFunction && existing.Kind == BindVar {
				existing.Kind = BindFunction
			}
			if kind == BindFunction && existing.Kind == BindParam {
				existing.Assigned = true // the function initializes the parameter
			}
			return existing
		}
	}
	r.fail(max(id.Pos, existing.Pos), "Identifier '"+id.Name+"' has already been declared")
	return nil
}

// declareLex declares a lexical binding, also rejecting a clash with a var
// declared anywhere inside the scope's node.
func (r *resolver) declareLex(s *Scope, id *Ident, kind BindKind, initEnd int) {
	if slices.Contains(r.varsIn[s.Node], id.Name) {
		r.fail(id.Pos, "Identifier '"+id.Name+"' has already been declared")
	}
	r.declare(s, id, kind, initEnd)
}

func (r *resolver) declareFunc(s *Scope, fd *FuncDecl, name *Ident) {
	if s.Kind == ScopeBlock && slices.Contains(r.varsIn[s.Node], name.Name) {
		// A function declaration in a block is lexical.
		r.fail(name.Pos, "Identifier '"+name.Name+"' has already been declared")
	}
	if s.Kind == ScopeBlock && plainFunc(fd) && !r.strict() {
		// Sloppy code may declare a plain function twice in one block
		// (B.3.2.4): both are instantiated, the last one wins.
		if b := s.Lookup(name.Name); b != nil && b.Kind == BindFunction && !slices.ContainsFunc(s.Funcs, func(g *FuncDecl) bool {
			return g.Func.Name.Name == name.Name && !plainFunc(g)
		}) {
			s.Funcs = append(s.Funcs, fd)
			return
		}
	}
	r.declare(s, name, BindFunction, 0)
	s.Funcs = append(s.Funcs, fd)
}

// plainFunc reports whether fd declares an ordinary function (not a
// generator or async function), the only kind Annex B extends.
func plainFunc(fd *FuncDecl) bool { return !fd.Func.IsGenerator && !fd.Func.IsAsync }

// labelledFunc returns the function declaration a labelled statement wraps
// (through any further labels), or nil.
func labelledFunc(st *LabeledStmt) *FuncDecl {
	for {
		switch b := st.Body.(type) {
		case *FuncDecl:
			return b
		case *LabeledStmt:
			st = b
		default:
			return nil
		}
	}
}

func defaultIdent(pos int) *Ident { return &Ident{Span: Span{pos, pos}, Name: "*default*"} }

// hoist declares var bindings and (at the top level) function declarations
// of a function or module body into s. path lists the enclosing block-like
// nodes so that lexical declarations in those blocks can detect clashes.
func (r *resolver) hoist(s *Scope, stmts []Stmt, path []Node, top bool) {
	for _, st := range stmts {
		r.hoistStmt(s, st, path, top)
	}
}

func (r *resolver) hoistStmt(s *Scope, st Stmt, path []Node, top bool) {
	switch st := st.(type) {
	case *VarDecl:
		if st.Kind == DeclVar {
			r.hoistVarDecl(s, st, path)
		}
	case *FuncDecl:
		if top {
			r.declareFunc(s, st, st.Func.Name)
		}
	case *ExportDecl:
		r.hoistStmt(s, st.Decl, path, top)
	case *ExportDefault:
		if fd, ok := st.Decl.(*FuncDecl); ok {
			name := fd.Func.Name
			if name == nil {
				name = defaultIdent(st.Pos)
			}
			r.declareFunc(s, fd, name)
		}
	case *BlockStmt:
		r.hoist(s, st.Body, append(path, st), false)
	case *IfStmt:
		r.hoistStmt(s, st.Then, path, false)
		if st.Else != nil {
			r.hoistStmt(s, st.Else, path, false)
		}
	case *ForStmt:
		inner := append(path, st)
		if vd, ok := st.Init.(*VarDecl); ok && vd.Kind == DeclVar {
			r.hoistVarDecl(s, vd, inner)
		}
		r.hoistStmt(s, st.Body, inner, false)
	case *ForInOfStmt:
		inner := append(path, st)
		if vd, ok := st.Left.(*VarDecl); ok && vd.Kind == DeclVar {
			r.hoistVarDecl(s, vd, inner)
		}
		r.hoistStmt(s, st.Body, inner, false)
	case *WhileStmt:
		r.hoistStmt(s, st.Body, path, false)
	case *DoWhileStmt:
		r.hoistStmt(s, st.Body, path, false)
	case *LabeledStmt:
		// A labelled function declaration (sloppy code) is placed like an
		// unlabelled one.
		r.hoistStmt(s, st.Body, path, top)
	case *WithStmt:
		r.hoistStmt(s, st.Body, path, false)
	case *SwitchStmt:
		inner := append(path, st)
		for _, c := range st.Cases {
			r.hoist(s, c.Body, inner, false)
		}
	case *TryStmt:
		r.hoist(s, st.Block.Body, append(path, st.Block), false)
		if st.Handler != nil {
			r.hoist(s, st.Handler.Body, append(path, st.Handler), false)
		}
		if st.Finalizer != nil {
			r.hoist(s, st.Finalizer.Body, append(path, st.Finalizer), false)
		}
	}
}

// hoistVarDecl declares the names of a var declaration in s.
func (r *resolver) hoistVarDecl(s *Scope, vd *VarDecl, path []Node) {
	for _, d := range vd.Decls {
		for _, id := range boundNames(d.Target, nil) {
			r.declare(s, id, BindVar, 0)
			for _, n := range path {
				if r.varsIn == nil {
					r.varsIn = make(map[Node][]string)
				}
				r.varsIn[n] = append(r.varsIn[n], id.Name)
			}
		}
	}
}

// declareLexical declares the let/const/class (and, in blocks, function)
// declarations directly contained in stmts.
func (r *resolver) declareLexical(s *Scope, stmts []Stmt) {
	for _, st := range stmts {
		switch st := st.(type) {
		case *VarDecl:
			if st.Kind == DeclVar {
				continue
			}
			kind := BindLet
			if st.Kind == DeclConst {
				kind = BindConst
			}
			for _, d := range st.Decls {
				for _, id := range boundNames(d.Target, nil) {
					r.declareLex(s, id, kind, d.End)
				}
			}
		case *ClassDecl:
			r.declareLex(s, st.Class.Name, BindClass, st.End)
		case *FuncDecl:
			if s.Kind == ScopeBlock {
				r.declareFunc(s, st, st.Func.Name)
			}
		case *LabeledStmt:
			if fd := labelledFunc(st); fd != nil && s.Kind == ScopeBlock {
				r.declareFunc(s, fd, fd.Func.Name)
			}
		case *ExportDecl:
			r.declareLexical(s, []Stmt{st.Decl})
		case *ImportDecl:
			r.importDecl(s, st)
		case *ExportNamed:
			if st.Source != nil {
				r.request(st.Source)
			}
		case *ExportAll:
			r.request(st.Source)
		case *ExportDefault:
			switch d := st.Decl.(type) {
			case *FuncDecl:
				// hoisted
			case *ClassDecl:
				name := d.Class.Name
				if name == nil {
					name = defaultIdent(st.Pos)
				}
				r.declareLex(s, name, BindClass, st.End)
			default:
				r.declareLex(s, defaultIdent(st.Pos), BindConst, st.End)
			}
		}
	}
}

// hasLexical reports whether stmts directly contain a lexical declaration.
func hasLexical(stmts []Stmt) bool {
	for _, st := range stmts {
		switch st := st.(type) {
		case *VarDecl:
			if st.Kind != DeclVar {
				return true
			}
		case *ClassDecl, *FuncDecl:
			return true
		case *LabeledStmt:
			if labelledFunc(st) != nil {
				return true
			}
		}
	}
	return false
}

// lexicalNames appends the names stmts declare lexically (in a block,
// function declarations included) to out.
func lexicalNames(stmts []Stmt, out []string) []string {
	for _, st := range stmts {
		switch st := st.(type) {
		case *VarDecl:
			if st.Kind != DeclVar {
				for _, d := range st.Decls {
					for _, id := range boundNames(d.Target, nil) {
						out = append(out, id.Name)
					}
				}
			}
		case *ClassDecl:
			out = append(out, st.Class.Name.Name)
		case *FuncDecl:
			out = append(out, st.Func.Name.Name)
		case *LabeledStmt:
			if fd := labelledFunc(st); fd != nil {
				out = append(out, fd.Func.Name.Name)
			}
		}
	}
	return out
}

// annexBFuncs applies Annex B.3.2 to the function or script body stmts of
// sloppy code: a plain function declaration directly in a block, case
// clause or if clause whose name a var declaration there could take without
// an early error (no let, const, class, block function or destructured
// catch parameter of that name in between, no parameter and no body-level
// lexical declaration) also gets a var binding in vs, the var scope, which
// the declaration assigns when evaluated. params is the parameter scope of
// a function (vs itself unless the body is kept apart), nil for a script.
// A function named arguments keeps to its block.
func (r *resolver) annexBFuncs(vs, params *Scope, stmts []Stmt) {
	var blocked []string
	candidate := func(fd *FuncDecl) {
		name := fd.Func.Name.Name
		if !plainFunc(fd) || slices.Contains(blocked, name) {
			return
		}
		if params != nil {
			if name == "arguments" {
				return
			}
			if b := params.Lookup(name); b != nil && b.Kind == BindParam {
				return
			}
		}
		b := vs.Lookup(name)
		if b != nil && b.Kind.IsLexical() {
			return
		}
		if b == nil {
			vs.add(&Binding{Name: name, Kind: BindVar, Pos: fd.Func.Name.Pos, AnnexB: params == nil})
		}
		if r.annexB == nil {
			r.annexB = make(map[*FuncDecl]bool)
		}
		r.annexB[fd] = true
	}
	var walk func(st Stmt)
	block := func(body []Stmt, extra []string) {
		mark := len(blocked)
		for _, st := range body {
			if fd, ok := st.(*FuncDecl); ok {
				candidate(fd)
			}
		}
		blocked = lexicalNames(body, append(blocked, extra...))
		for _, st := range body {
			walk(st)
		}
		blocked = blocked[:mark]
	}
	walk = func(st Stmt) {
		switch st := st.(type) {
		case *BlockStmt:
			block(st.Body, nil)
		case *IfStmt:
			walk(st.Then)
			if st.Else != nil {
				walk(st.Else)
			}
		case *ForStmt:
			mark := len(blocked)
			if vd, ok := st.Init.(*VarDecl); ok {
				blocked = lexicalNames([]Stmt{vd}, blocked)
			}
			walk(st.Body)
			blocked = blocked[:mark]
		case *ForInOfStmt:
			mark := len(blocked)
			if vd, ok := st.Left.(*VarDecl); ok {
				blocked = lexicalNames([]Stmt{vd}, blocked)
			}
			walk(st.Body)
			blocked = blocked[:mark]
		case *WhileStmt:
			walk(st.Body)
		case *DoWhileStmt:
			walk(st.Body)
		case *LabeledStmt:
			walk(st.Body)
		case *WithStmt:
			walk(st.Body)
		case *SwitchStmt:
			mark := len(blocked)
			for _, c := range st.Cases {
				for _, s := range c.Body {
					if fd, ok := s.(*FuncDecl); ok {
						candidate(fd)
					}
				}
			}
			for _, c := range st.Cases {
				blocked = lexicalNames(c.Body, blocked)
			}
			for _, c := range st.Cases {
				for _, s := range c.Body {
					walk(s)
				}
			}
			blocked = blocked[:mark]
		case *TryStmt:
			block(st.Block.Body, nil)
			if st.Handler != nil {
				var names []string
				if st.Param != nil && !isSimpleParam(st.Param) {
					for _, id := range boundNames(st.Param, nil) {
						names = append(names, id.Name)
					}
				}
				block(st.Handler.Body, names)
			}
			if st.Finalizer != nil {
				block(st.Finalizer.Body, nil)
			}
		}
	}
	for _, st := range stmts {
		walk(st)
	}
}

// ---- Statements -------------------------------------------------------------

func (r *resolver) stmts(list []Stmt) {
	for i, s := range list {
		poll(r.stop, i)
		r.stmt(s)
	}
}

func (r *resolver) stmt(s Stmt) {
	switch s := s.(type) {
	case *VarDecl:
		for _, d := range s.Decls {
			r.declPattern(d.Target)
			if d.Init != nil {
				markAssigned(d.Target)
				r.expr(d.Init)
			}
		}
	case *FuncDecl:
		r.funcDecl(s)
	case *ClassDecl:
		outer := r.scope.Resolve(s.Class.Name.Name)
		r.class(s.Class)
		s.Class.Name.Binding = outer
	case *ExprStmt:
		r.expr(s.X)
	case *BlockStmt:
		r.block(s, nil)
	case *EmptyStmt, *DebuggerStmt, *BreakStmt, *ContinueStmt:
	case *IfStmt:
		r.expr(s.Cond)
		r.stmt(s.Then)
		if s.Else != nil {
			r.stmt(s.Else)
		}
	case *ForStmt:
		r.forStmt(s)
	case *ForInOfStmt:
		r.forInOf(s)
	case *WhileStmt:
		r.expr(s.Cond)
		r.stmt(s.Body)
	case *DoWhileStmt:
		r.stmt(s.Body)
		r.expr(s.Cond)
	case *ReturnStmt:
		if s.Result != nil {
			r.expr(s.Result)
		}
	case *ThrowStmt:
		r.expr(s.X)
	case *LabeledStmt:
		r.stmt(s.Body)
	case *WithStmt:
		r.expr(s.Object)
		ws := r.newScope(ScopeWith, s)
		ws.add(&Binding{Name: "%with", Kind: BindHidden, Pos: s.Pos})
		s.Scope = ws
		r.push(ws)
		r.withs++
		r.stmt(s.Body)
		r.withs--
		r.pop()
	case *SwitchStmt:
		r.switchStmt(s)
	case *TryStmt:
		r.tryStmt(s)
	case *ExportDecl:
		r.stmt(s.Decl)
		r.exportDecl(s)
	case *ExportNamed:
		if s.Source != nil {
			req := r.request(s.Source)
			for _, spec := range s.Specs {
				r.addReexport(&ReexportEntry{Name: spec.Exported.Name, Request: req, Import: spec.Local.Name, Pos: spec.Pos})
			}
			return
		}
		for _, spec := range s.Specs {
			b := r.module.Scope.Lookup(spec.Local.Name)
			if b == nil {
				if r.tsTypeExport(spec.Local.Name) {
					r.tsDropNode(spec)
					continue
				}
				r.fail(spec.Local.Pos, "Export '"+spec.Local.Name+"' is not defined")
			}
			spec.Local.Binding = b
			switch b.Kind {
			case BindImport:
				// Re-export of an imported binding: it names the
				// exporter's binding, not a local one.
				ie := r.importOf(b)
				r.addReexport(&ReexportEntry{Name: spec.Exported.Name, Request: ie.Request, Import: ie.Name, Pos: spec.Pos})
				continue
			case BindImportNS:
				// Re-export of an imported namespace, as export * as:
				// modules that re-export the same namespace export one
				// binding.
				ie := r.importOf(b)
				r.addReexport(&ReexportEntry{Name: spec.Exported.Name, Request: ie.Request, All: true, Pos: spec.Pos})
				continue
			}
			r.addExport(spec.Exported.Name, spec.Local.Name, b, spec.Pos)
		}
	case *ExportDefault:
		r.exportDefault(s)
	case *ExportAll:
		req := r.request(s.Source)
		if s.As != nil {
			r.addReexport(&ReexportEntry{Name: s.As.Name, Request: req, All: true, Pos: s.As.Pos})
			return
		}
		r.module.Stars = append(r.module.Stars, &StarExport{Request: req, Pos: s.Pos})
	case *ImportDecl:
		// declared by declareLexical
	}
}

// block resolves a block, creating a scope when it declares lexical
// bindings. catchParams are the names bound by an enclosing catch clause,
// which the block may not redeclare.
func (r *resolver) block(b *BlockStmt, catchParams []*Ident) {
	if !hasLexical(b.Body) {
		r.stmts(b.Body)
		return
	}
	s := r.newScope(ScopeBlock, b)
	b.Scope = s
	r.push(s)
	r.declareLexical(s, b.Body)
	for _, bind := range s.Bindings {
		if slices.ContainsFunc(catchParams, func(id *Ident) bool { return id.Name == bind.Name }) {
			r.fail(bind.Pos, "Identifier '"+bind.Name+"' has already been declared")
		}
	}
	r.stmts(b.Body)
	r.pop()
}

func (r *resolver) funcDecl(fd *FuncDecl) {
	fn := fd.Func
	var b *Binding
	if fn.Name != nil {
		b = r.scope.Resolve(fn.Name.Name)
		if r.evalVars != nil && r.scope == r.evalVars {
			// A top-level function of sloppy eval code binds in the
			// caller's variable environment (EvalDeclarationInstantiation).
			b = nil
		}
		fn.Name.Binding = b
	} else {
		b = r.scope.Lookup("*default*")
	}
	r.function(fn, b, false)
	if b != nil && r.annexB[fd] {
		b.AnnexB = true
	}
}

// function resolves a function body in a new function scope. hoistedAs is
// the declaration binding for function declarations (nil otherwise); isExpr
// makes a name bind inside the function.
func (r *resolver) function(fn *Function, hoistedAs *Binding, isExpr bool) {
	s := r.newScope(ScopeFunction, fn)
	s.Func = fn
	fn.Scope = s
	frame := &fnFrame{fn: fn, hoisted: hoistedAs != nil, binding: hoistedAs}
	r.frames = append(r.frames, frame)
	prevScope := r.scope
	r.scope = s

	// With parameter expressions a parameter is in TDZ until its element
	// has been bound: initEnd makes checkTDZ treat it like a lexical.
	exprs := hasParamExprs(fn)
	for _, param := range fn.Params {
		r.declareParam(s, param, exprs)
	}
	if fn.Rest != nil {
		r.declareParam(s, fn.Rest, exprs)
	}
	if exprs && fn.Body != nil {
		r.paramScope(fn, s, frame, isExpr)
	} else {
		if fn.Body != nil {
			r.hoist(s, fn.Body.Body, nil, true)
			r.declareLexical(s, fn.Body.Body)
			if !fn.IsStrict {
				r.annexBFuncs(s, s, fn.Body.Body)
			}
		}
		r.selfBinding(fn, s, isExpr)
		for _, param := range fn.Params {
			r.declPattern(param)
		}
		if fn.Rest != nil {
			r.declPattern(fn.Rest)
		}
	}
	if fn.Body != nil {
		r.stmts(fn.Body.Body)
	} else {
		r.expr(fn.ExprBody)
	}

	// A sloppy function with simple parameters maps its arguments object
	// onto the parameters, which therefore live in its environment.
	if !fn.IsStrict && fn.HasSimpleParams && fn.ArgumentsBinding() != nil {
		for _, param := range fn.Params {
			if id := param.(*Ident); id.Binding != nil {
				id.Binding.Captured = true
			}
		}
	}

	r.finishFrame(frame)
	r.frames = r.frames[:len(r.frames)-1]
	r.scope = prevScope
}

func (r *resolver) declareParam(s *Scope, param Pattern, exprs bool) {
	initEnd := 0
	if exprs {
		_, initEnd = param.Range()
	}
	for _, id := range boundNames(param, nil) {
		r.declare(s, id, BindParam, initEnd)
	}
}

// selfBinding declares the name of a named function expression in s unless
// a binding of s already has it.
func (r *resolver) selfBinding(fn *Function, s *Scope, isExpr bool) {
	if isExpr && fn.Name != nil && s.Lookup(fn.Name.Name) == nil {
		b := &Binding{Name: fn.Name.Name, Kind: BindFuncName, Pos: fn.Name.Pos}
		s.add(b)
		fn.SelfBinding = b
		fn.Name.Binding = b
	}
}

// paramScope resolves the parameters of a function with parameter
// expressions, which see neither the body's var nor its lexical
// declarations, and then declares the body. The body goes into a scope of
// its own (Body.Scope) only when that is observable: a body
// declaration shares a name with a parameter or the function's own name (a
// body var starts with the parameter's value but is a distinct binding), or
// the parameter list refers to a name the body declares. Otherwise its
// bindings join s, as for simple parameters.
func (r *resolver) paramScope(fn *Function, s *Scope, frame *fnFrame, isExpr bool) {
	r.selfBinding(fn, s, isExpr)
	r.inParams = append(r.inParams, paramList{frame: len(r.frames) - 1})
	for _, param := range fn.Params {
		r.declPattern(param)
	}
	if fn.Rest != nil {
		r.declPattern(fn.Rest)
	}
	pl := r.inParams[len(r.inParams)-1]
	refs := pl.refs
	r.inParams = r.inParams[:len(r.inParams)-1]

	body := &Scope{Kind: ScopeFunction, Parent: s, Func: fn, Node: fn}
	r.hoist(body, fn.Body.Body, nil, true)
	r.declareLexical(body, fn.Body.Body)
	if !fn.IsStrict {
		r.annexBFuncs(body, s, fn.Body.Body)
	}
	// A direct eval in the parameter list declares its vars outside the
	// parameters, and one in the body of sloppy code in the body's own
	// variable environment (FunctionDeclarationInstantiation step 20 and
	// 28): either keeps the body apart.
	split := pl.eval || !fn.IsStrict && hasDirectEval(fn.Body.Body)
	for _, b := range body.Bindings {
		if outer := s.Lookup(b.Name); outer != nil {
			if b.Kind.IsLexical() && outer.Kind == BindParam {
				r.fail(b.Pos, "Identifier '"+b.Name+"' has already been declared")
			}
			split = true
		}
		if refs[b.Name] {
			split = true
		}
	}
	if !split {
		for _, b := range body.Bindings {
			s.add(b)
		}
		s.Funcs = append(s.Funcs, body.Funcs...)
		return
	}
	s.Children = append(s.Children, body)
	fn.Body.Scope = body
	r.scope = body
}

// hasParamExprs reports whether a parameter has a default or a computed key,
// the ContainsExpression of the specification.
func hasParamExprs(fn *Function) bool {
	if fn.HasSimpleParams {
		return false
	}
	for _, p := range fn.Params {
		if patternHasExpr(p) {
			return true
		}
	}
	return fn.Rest != nil && patternHasExpr(fn.Rest)
}

func patternHasExpr(p Pattern) bool {
	switch p := p.(type) {
	case *AssignPattern:
		return true
	case *ObjectPattern:
		for _, prop := range p.Props {
			if prop.Computed || patternHasExpr(prop.Value) {
				return true
			}
		}
		return p.Rest != nil && patternHasExpr(p.Rest)
	case *ArrayPattern:
		for _, e := range p.Elems {
			if e != nil && patternHasExpr(e) {
				return true
			}
		}
		return p.Rest != nil && patternHasExpr(p.Rest)
	}
	return false
}

// finishFrame decides TDZ for lexical bindings read from hoisted functions
// of the frame: a hoisted function is "early" if non-hoisted code
// references it (directly or through other hoisted functions) before the
// binding's initialiser has run.
func (r *resolver) finishFrame(frame *fnFrame) {
	infos := frame.infos
	if len(infos) == 0 {
		return
	}
	early := make(map[*Binding]int, len(infos))
	for b, info := range infos {
		early[b] = info.earlyRef
	}
	// The host calls a module's exports while its top level awaits, and a
	// module that requests others, or one compiled with EarlyExports, can
	// have its exports called by a module of an import cycle before its own
	// body runs.
	if frame.fn == nil && r.module != nil && (r.module.Async || len(r.module.Requests) > 0 || r.earlyExports) {
		at := r.firstAwait
		if len(r.module.Requests) > 0 || r.earlyExports {
			at = 0
		}
		for b := range infos {
			if b.Exported {
				early[b] = min(early[b], at)
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for b, info := range infos {
			for _, c := range info.callees {
				if early[b] < early[c] {
					early[c] = early[b]
					changed = true
				}
			}
		}
	}
	for b, info := range infos {
		for _, lex := range info.tdzRefs {
			if early[b] < lex.initEnd {
				lex.NeedsTDZ = true
			}
		}
	}
}

func (f *fnFrame) info(b *Binding) *hoistInfo {
	if f.infos == nil {
		f.infos = make(map[*Binding]*hoistInfo)
	}
	h := f.infos[b]
	if h == nil {
		h = &hoistInfo{earlyRef: math.MaxInt}
		f.infos[b] = h
	}
	return h
}

// declPattern binds the identifiers of a declaration pattern and resolves
// its default values and computed keys.
func (r *resolver) declPattern(pat Pattern) {
	switch pat := pat.(type) {
	case *Ident:
		pat.Binding = r.scope.Resolve(pat.Name)
	case *AssignPattern:
		r.declPattern(pat.Target)
		r.expr(pat.Default)
	case *ObjectPattern:
		for _, prop := range pat.Props {
			if prop.Computed {
				r.expr(prop.Key)
			}
			r.declPattern(prop.Value)
		}
		if pat.Rest != nil {
			r.declPattern(pat.Rest)
		}
	case *ArrayPattern:
		for _, e := range pat.Elems {
			if e != nil {
				r.declPattern(e)
			}
		}
		if pat.Rest != nil {
			r.declPattern(pat.Rest)
		}
	case *MemberExpr:
		r.expr(pat)
	}
}

// markAssigned marks the bindings that the declaration pattern pat,
// resolved, names Assigned.
func markAssigned(pat Pattern) {
	if id, ok := pat.(*Ident); ok {
		if id.Binding != nil {
			id.Binding.Assigned = true
		}
		return
	}
	for _, id := range boundNames(pat, nil) {
		if id.Binding != nil {
			id.Binding.Assigned = true
		}
	}
}

// assignTarget resolves an assignment target.
func (r *resolver) assignTarget(pat Pattern) {
	switch pat := pat.(type) {
	case *Ident:
		r.ident(pat)
		if pat.Binding != nil {
			pat.Binding.Assigned = true
		}
	case *MemberExpr:
		r.expr(pat)
	case *CallExpr:
		r.expr(pat)
	case *AssignPattern:
		r.assignTarget(pat.Target)
		r.expr(pat.Default)
	case *ObjectPattern:
		for _, prop := range pat.Props {
			if prop.Computed {
				r.expr(prop.Key)
			}
			r.assignTarget(prop.Value)
		}
		if pat.Rest != nil {
			r.assignTarget(pat.Rest)
		}
	case *ArrayPattern:
		for _, e := range pat.Elems {
			if e != nil {
				r.assignTarget(e)
			}
		}
		if pat.Rest != nil {
			r.assignTarget(pat.Rest)
		}
	}
}

func (r *resolver) forStmt(s *ForStmt) {
	vd, lexical := s.Init.(*VarDecl)
	lexical = lexical && vd.Kind != DeclVar
	if lexical {
		sc := r.newScope(ScopeFor, s)
		s.Scope = sc
		r.push(sc)
		r.declareLexical(sc, []Stmt{vd})
	}
	switch init := s.Init.(type) {
	case *VarDecl:
		r.stmt(init)
	case Expr:
		r.expr(init)
	}
	if s.Cond != nil {
		r.expr(s.Cond)
	}
	if s.Update != nil {
		r.expr(s.Update)
	}
	r.stmt(s.Body)
	if lexical {
		r.pop()
	}
}

func (r *resolver) forInOf(s *ForInOfStmt) {
	if s.Await {
		r.checkAwait(s.Pos)
	}
	switch left := s.Left.(type) {
	case *VarDecl:
		if left.Kind == DeclVar {
			r.declPattern(left.Decls[0].Target)
			markAssigned(left.Decls[0].Target)
			if init := left.Decls[0].Init; init != nil {
				r.expr(init)
			}
			r.expr(s.Right)
			break
		}
		sc := r.newScope(ScopeFor, s)
		s.Scope = sc
		r.push(sc)
		r.declareLexical(sc, []Stmt{left})
		prev := r.tdzScope
		r.tdzScope = sc
		r.expr(s.Right)
		r.tdzScope = prev
		r.declPattern(left.Decls[0].Target)
		r.stmt(s.Body)
		r.pop()
		return
	case Pattern:
		r.assignTarget(left)
		r.expr(s.Right)
	}
	r.stmt(s.Body)
}

func (r *resolver) switchStmt(s *SwitchStmt) {
	r.expr(s.Disc)
	lexical := false
	for _, c := range s.Cases {
		lexical = lexical || hasLexical(c.Body)
	}
	if lexical {
		sc := r.newScope(ScopeBlock, s)
		s.Scope = sc
		r.push(sc)
		for _, c := range s.Cases {
			r.declareLexical(sc, c.Body)
		}
	}
	for _, c := range s.Cases {
		if c.Test != nil {
			r.expr(c.Test)
		}
		r.stmts(c.Body)
	}
	if lexical {
		r.pop()
	}
}

func (r *resolver) tryStmt(s *TryStmt) {
	r.block(s.Block, nil)
	if s.Handler != nil {
		if s.Param == nil {
			r.block(s.Handler, nil)
		} else {
			cs := r.newScope(ScopeCatch, s)
			s.CatchScope = cs
			r.push(cs)
			names := boundNames(s.Param, nil)
			for _, id := range names {
				r.declare(cs, id, BindCatch, 0)
			}
			if !isSimpleParam(s.Param) {
				for _, name := range r.varsIn[s.Handler] {
					if slices.ContainsFunc(names, func(id *Ident) bool { return id.Name == name }) {
						r.fail(s.Handler.Pos, "Identifier '"+name+"' has already been declared")
					}
				}
			}
			r.declPattern(s.Param)
			r.block(s.Handler, names)
			r.pop()
		}
	}
	if s.Finalizer != nil {
		r.block(s.Finalizer, nil)
	}
}

// ---- Exports ----------------------------------------------------------------

func (r *resolver) addExport(name, local string, b *Binding, pos int) {
	if r.exports[name] {
		r.fail(pos, "Duplicate export of '"+name+"'")
	}
	r.exports[name] = true
	b.Exported = true
	r.module.Exports = append(r.module.Exports, &ExportEntry{Name: name, Local: local, Binding: b, Pos: pos})
}

func (r *resolver) addReexport(e *ReexportEntry) {
	if r.exports[e.Name] {
		r.fail(e.Pos, "Duplicate export of '"+e.Name+"'")
	}
	r.exports[e.Name] = true
	r.module.Reexports = append(r.module.Reexports, e)
}

// request returns the index of the module request for specifier src,
// adding it on first use.
func (r *resolver) request(src *StringLit) int {
	for i, req := range r.module.Requests {
		if req.Specifier == src.Value {
			return i
		}
	}
	r.module.Requests = append(r.module.Requests, &ModuleRequest{Specifier: src.Value, Pos: src.Pos})
	return len(r.module.Requests) - 1
}

// importDecl declares the bindings of an import declaration in the module
// scope s.
func (r *resolver) importDecl(s *Scope, d *ImportDecl) {
	req := r.request(d.Source)
	add := func(id *Ident, name string, ns bool, pos int) {
		kind := BindImport
		if ns {
			kind = BindImportNS
		}
		b := r.declare(s, id, kind, 0)
		id.Binding = b
		r.module.Imports = append(r.module.Imports, &ImportEntry{Request: req, Name: name, Namespace: ns, Binding: b, Pos: pos})
	}
	if d.Default != nil {
		add(d.Default, "default", false, d.Default.Pos)
	}
	if d.Namespace != nil {
		add(d.Namespace, "", true, d.Namespace.Pos)
	}
	for _, spec := range d.Specs {
		add(spec.Local, spec.Imported.Name, false, spec.Pos)
	}
}

// importOf returns the import entry that declares binding b.
func (r *resolver) importOf(b *Binding) *ImportEntry {
	for _, ie := range r.module.Imports {
		if ie.Binding == b {
			return ie
		}
	}
	panic("syntax: import binding without an import entry")
}

func (r *resolver) exportDecl(s *ExportDecl) {
	switch d := s.Decl.(type) {
	case *VarDecl:
		for _, decl := range d.Decls {
			for _, id := range boundNames(decl.Target, nil) {
				r.addExport(id.Name, id.Name, id.Binding, id.Pos)
			}
		}
	case *FuncDecl:
		r.addExport(d.Func.Name.Name, d.Func.Name.Name, d.Func.Name.Binding, d.Func.Name.Pos)
	case *ClassDecl:
		r.addExport(d.Class.Name.Name, d.Class.Name.Name, d.Class.Name.Binding, d.Class.Name.Pos)
	}
}

func (r *resolver) exportDefault(s *ExportDefault) {
	if r.tsTypeDefault(s.Decl) {
		r.tsDropNode(s)
		return
	}
	local := "*default*"
	switch d := s.Decl.(type) {
	case *FuncDecl:
		r.funcDecl(d)
		if d.Func.Name != nil {
			local = d.Func.Name.Name
		}
	case *ClassDecl:
		r.class(d.Class)
		if d.Class.Name != nil {
			local = d.Class.Name.Name
			d.Class.Name.Binding = r.scope.Resolve(local)
		}
	case Expr:
		r.expr(d)
	}
	r.addExport("default", local, r.module.Scope.Lookup(local), s.Pos)
}

// ---- Expressions ------------------------------------------------------------

func (r *resolver) exprs(list []Expr) {
	for i, e := range list {
		pollItem(r.stop, i)
		if e != nil {
			r.expr(e)
		}
	}
}

func (r *resolver) expr(e Expr) {
	switch e := e.(type) {
	case *Ident:
		r.ident(e)
	case *ThisExpr:
		r.markThis()
		e.Binding = r.derivedThis()
	case *SuperExpr:
		// super(...) and super.x are resolved by their CallExpr and
		// MemberExpr; `new super()` is the only other form the parser lets
		// through.
		r.fail(e.Pos, "'super' keyword unexpected here")
	case *NewTarget:
		e.Binding = r.newTarget()
	case *ImportCall:
		r.expr(e.Source)
	case *ImportMeta, *NumberLit, *BigIntLit, *StringLit, *BoolLit, *NullLit, *RegexLit:
	case *TemplateLit:
		r.exprs(e.Exprs)
	case *TaggedTemplate:
		r.expr(e.Tag)
		r.expr(e.Quasi)
	case *ArrayLit:
		r.exprs(e.Elems)
	case *ObjectLit:
		r.objectLit(e)
	case *Function:
		r.function(e, nil, true)
	case *Class:
		r.class(e)
		if e.Name != nil {
			e.Name.Binding = e.NameBinding
		}
	case *UnaryExpr:
		r.expr(e.X)
	case *UpdateExpr:
		r.assignTarget(e.X.(Pattern))
	case *BinaryExpr:
		r.expr(e.X)
		r.expr(e.Y)
	case *LogicalExpr:
		r.expr(e.X)
		r.expr(e.Y)
	case *AssignExpr:
		r.assignTarget(e.Target)
		r.expr(e.Value)
	case *CondExpr:
		r.expr(e.Test)
		r.expr(e.Cons)
		r.expr(e.Alt)
	case *SeqExpr:
		r.exprs(e.Exprs)
	case *CallExpr:
		if sup, ok := e.Callee.(*SuperExpr); ok {
			r.superCall(sup)
			r.exprs(e.Args)
			break
		}
		if isDirectEval(e) {
			r.directEval()
		}
		r.expr(e.Callee)
		r.exprs(e.Args)
	case *NewExpr:
		r.expr(e.Callee)
		r.exprs(e.Args)
	case *MemberExpr:
		if sup, ok := e.Object.(*SuperExpr); ok {
			if _, ok := e.Prop.(*PrivateName); ok {
				r.fail(e.Prop.(*PrivateName).Pos, "Unexpected private field")
			}
			r.superProperty(sup)
		} else {
			r.expr(e.Object)
		}
		if e.Computed {
			r.expr(e.Prop)
		} else if pn, ok := e.Prop.(*PrivateName); ok {
			r.privateName(pn)
		}
	case *OptChain:
		r.expr(e.X)
	case *SpreadElem:
		r.expr(e.X)
	case *YieldExpr:
		r.checkYield(e)
		if e.X != nil {
			r.expr(e.X)
		}
	case *AwaitExpr:
		r.checkClassAwait(e)
		r.checkAwait(e.Pos)
		r.expr(e.X)
	case *PrivateName:
		r.privateName(e)
	}
}

func (r *resolver) objectLit(o *ObjectLit) {
	if o.dupProto != 0 {
		r.fail(o.dupProto, "Duplicate __proto__ fields are not allowed in object literals")
	}
	for i, prop := range o.Props {
		pollItem(r.stop, i)
		if prop.Kind == PropCoverInit {
			r.fail(prop.Pos, "Invalid shorthand property initializer")
		}
		if prop.Computed {
			r.expr(prop.Key)
		}
		if fn, ok := prop.Value.(*Function); ok && prop.Kind != PropInit && prop.Kind != PropSpread {
			r.function(fn, nil, false)
			continue
		}
		r.expr(prop.Value)
	}
}

// ident resolves a reference and records capture and TDZ information.
func (r *resolver) ident(id *Ident) {
	var b *Binding
	if id.Name == "arguments" {
		r.checkClassArguments(id)
		b = r.argumentsBinding()
	} else {
		b = r.resolve(id.Name)
	}
	if r.withs > 0 {
		r.throughWith(b)
	}
	for i := range r.inParams {
		if pl := &r.inParams[i]; b == nil || r.ownerFrame(b) <= pl.frame {
			if pl.refs == nil {
				pl.refs = make(map[string]bool)
			}
			pl.refs[id.Name] = true
		}
	}
	if b == nil {
		return
	}
	id.Binding = b
	owner := r.ownerFrame(b)
	last := len(r.frames) - 1
	if owner != last {
		b.Captured = true
	}
	switch {
	case b.Kind == BindFunction:
		info := r.frames[owner].info(b)
		if owner == last {
			info.earlyRef = min(info.earlyRef, id.Pos)
		} else if first := r.frames[owner+1]; first.hoisted {
			r.frames[owner].info(first.binding).callees = append(r.frames[owner].info(first.binding).callees, b)
		} else {
			info.earlyRef = min(info.earlyRef, first.fn.Pos)
		}
	case (b.Kind.IsLexical() || b.Kind == BindParam && b.initEnd > 0) && !b.NeedsTDZ:
		r.checkTDZ(id, b, owner, last)
	}
}

// ownerFrame returns the index of the frame whose function declares b.
func (r *resolver) ownerFrame(b *Binding) int {
	for i := len(r.frames) - 1; i > 0; i-- {
		if r.frames[i].fn == b.Scope.Func {
			return i
		}
	}
	return 0
}

func (r *resolver) checkTDZ(id *Ident, b *Binding, owner, last int) {
	if _, inSwitch := b.Scope.Node.(*SwitchStmt); inSwitch || b.Scope == r.tdzScope {
		b.NeedsTDZ = true
		return
	}
	if owner == last {
		if id.Pos < b.initEnd {
			b.NeedsTDZ = true
		}
		return
	}
	first := r.frames[owner+1]
	if first.hoisted {
		info := r.frames[owner].info(first.binding)
		info.tdzRefs = append(info.tdzRefs, b)
		return
	}
	if first.fn.Pos < b.initEnd {
		b.NeedsTDZ = true
	}
}

// markThis flags `this` use on the innermost non-arrow function and every
// arrow between it and the reference.
func (r *resolver) markThis() {
	for i := len(r.frames) - 1; i > 0; i-- {
		fn := r.frames[i].fn
		fn.UsesThis = true
		if !fn.IsArrow {
			return
		}
	}
}

// throughWith marks the object binding of every with statement between the
// reference being resolved and b's scope (all of them for a global) as
// captured when the reference is in a function nested inside it.
func (r *resolver) throughWith(b *Binding) {
	last := len(r.frames) - 1
	for s := r.scope; s != nil && (b == nil || s != b.Scope); s = s.Parent {
		if s.Kind == ScopeWith && r.ownerFrame(s.Bindings[0]) != last {
			s.Bindings[0].Captured = true
		}
	}
}

// argumentsBinding resolves `arguments`. In a function (arrows looking
// through to the innermost non-arrow one) it is the function's arguments
// object, declared on first use, flagging the function and every arrow
// between it and the reference, unless a parameter, a function or a lexical
// declaration of the function (sloppy code only) takes the name; a var
// declaration named arguments denotes the object itself. At the top level
// `arguments` is an ordinary reference.
func (r *resolver) argumentsBinding() *Binding {
	b := r.scope.Resolve("arguments")
	i := len(r.frames) - 1
	for i > 0 && r.frames[i].fn.IsArrow {
		i--
	}
	if i == 0 {
		return b
	}
	fn := r.frames[i].fn
	if b != nil && r.ownerFrame(b) >= i {
		switch {
		case b.Kind == BindArgs:
		case b.Kind == BindVar && b.Scope == fn.Scope:
			b.Kind = BindArgs
		case b.Kind == BindVar && fn.Body != nil && b.Scope == fn.Body.Scope:
			// A body kept apart from parameter expressions starts its var
			// with the object (see enterBodyScope in the compiler).
			if fn.Scope.Lookup("arguments") == nil {
				r.declareArguments(fn, i)
			}
			return b
		default:
			return b
		}
	} else {
		b = r.declareArguments(fn, i)
	}
	for j := i; j < len(r.frames); j++ {
		r.frames[j].fn.UsesArguments = true
	}
	return b
}

// declareArguments declares the arguments object of fn, the function of
// frame i.
func (r *resolver) declareArguments(fn *Function, i int) *Binding {
	if fn == r.evalThis {
		// The caller's arguments binding is one of the eval scope's levels,
		// found by Resolve before this is reached.
		r.fail(fn.Pos, "internal: arguments of the eval code's function is not in its scope")
	}
	b := fn.Scope.Lookup("arguments")
	if b == nil {
		b = &Binding{Name: "arguments", Kind: BindArgs, Pos: fn.Pos}
		fn.Scope.add(b)
	}
	for j := i; j < len(r.frames); j++ {
		r.frames[j].fn.UsesArguments = true
	}
	return b
}

// ArgumentsBinding returns the binding of the function's own arguments
// object (BindArgs, declared in Scope), nil when nothing references it.
// Arrows have none: their `arguments` is the enclosing function's.
func (fn *Function) ArgumentsBinding() *Binding {
	if !fn.UsesArguments || fn.IsArrow {
		return nil
	}
	if b := fn.Scope.Lookup("arguments"); b != nil && b.Kind == BindArgs {
		return b
	}
	return nil
}

func (r *resolver) markEval() {
	for i := len(r.frames) - 1; i > 0; i-- {
		fn := r.frames[i].fn
		if fn.HasDirectEval {
			return // and so are the frames outside it
		}
		fn.HasDirectEval = true
	}
}

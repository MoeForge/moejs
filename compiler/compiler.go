// Package compiler translates the annotated AST produced by package syntax
// into bytecode.Function templates.
//
// The compiler consumes the scope annotations directly: every identifier
// already points at its Binding (or nil for a global), bindings know whether
// they are captured by inner functions and whether reads need a TDZ check,
// and each scope lists the hoisted function declarations to instantiate on
// entry. Un-captured bindings live in registers, captured ones in closure
// environments; module and script top-level bindings always live in the
// top-level environment so that exports are live and the register file stays
// small.
package compiler

import (
	"slices"
	"strconv"
	"strings"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Error is a compile-time failure with a source position.
type Error struct {
	Name string
	Pos  int
	Line int
	Col  int
	Msg  string
}

func (e *Error) Error() string {
	return e.Name + ":" + strconv.Itoa(e.Line) + ":" + strconv.Itoa(e.Col) + ": " + e.Msg
}

// unsupportedSuffix is appended to every deferred-feature message.
const unsupportedSuffix = " is not supported yet"

// CompileModule compiles a parsed module into its top-level function
// template (Kind == KindModule). The module's bindings become the slots of
// the function's own environment; Function.Module maps export names to
// those slots and lists what the module imports. A module with top-level
// await compiles as an async function body (Function.Async): the call
// returns the promise of its evaluation.
func CompileModule(m *syntax.Module) (*bytecode.Function, error) {
	return compileUnit(func(noHoist map[*syntax.Function]bool) (*bytecode.Function, error) {
		return compileModuleWith(m, nil, noHoist)
	})
}

// CompileModuleStop is CompileModule calling stop, when not nil, before
// every stopEvery-th statement of each statement list it compiles, the
// first included, and after every stopEvery elements of an array or
// object literal or arguments of a call: the error stop returns ends the
// compile, which returns it as is.
func CompileModuleStop(m *syntax.Module, stop func() error) (*bytecode.Function, error) {
	if stop == nil {
		return CompileModule(m) // whose closure holds less
	}
	return compileUnit(func(noHoist map[*syntax.Function]bool) (*bytecode.Function, error) {
		return compileModuleWith(m, stop, noHoist)
	})
}

// compileModuleWith is CompileModuleStop without hoisting in the functions
// of noHoist.
func compileModuleWith(m *syntax.Module, stop func() error, noHoist map[*syntax.Function]bool) (fn *bytecode.Function, err error) {
	var sc *scriptState // a module needs none, but to hold stop
	if stop != nil {
		sc = &scriptState{stop: stop}
	}
	c := newCompiler(m.File, sc, noHoist)
	defer c.recover(&err)
	defer c.release()
	f := c.newFuncState(nil, nil, bytecode.KindModule, m.Scope)
	f.out.Name = ""
	f.out.Source = &bytecode.SourceInfo{Name: m.File.Name, Src: m.File.Src, Start: m.Pos, End: m.End}
	f.out.Async = m.Async
	f.enterTopLevel(m.Scope, m.Body)
	bodyPC := f.pc()
	f.asyncStart()
	f.stmts(m.Body)
	f.endBody()
	out := f.finish()
	mod := &bytecode.Module{Exports: make([]bytecode.Export, 0, len(m.Exports)), BodyPC: bodyPC}
	for _, e := range m.Exports {
		mod.Exports = append(mod.Exports, bytecode.Export{Name: e.Name, Slot: c.moduleSlot(e.Binding, e.Pos, "export '"+e.Name+"'")})
	}
	slices.SortFunc(mod.Exports, func(a, b bytecode.Export) int { return strings.Compare(a.Name, b.Name) })
	if len(m.Requests) > 0 || out.ScriptOrModule {
		mod.Links = c.links(m)
	}
	// The import() calls of its eval code read the module's record too,
	// which a module evaluated alone has none of.
	out.ScriptOrModule = out.ScriptOrModule || m.HasDirectEval
	out.Module = mod
	return out, nil
}

// moduleSlot returns the environment slot of the binding b of an export or
// import entry (what).
func (c *compiler) moduleSlot(b *syntax.Binding, pos int, what string) int {
	loc := c.locs[b]
	if loc.env == nil {
		c.fail(pos, "SyntaxError: "+what+" is not a module binding")
	}
	return loc.slot
}

// links lists what module m requests and imports, with the source position
// of every entry for the errors of linking.
func (c *compiler) links(m *syntax.Module) *bytecode.Links {
	at := func(pos int) (int32, int32) {
		line, col := c.file.Position(pos)
		return int32(line), int32(col)
	}
	l := &bytecode.Links{
		Requests:  make([]bytecode.Request, len(m.Requests)),
		Imports:   make([]bytecode.Import, len(m.Imports)),
		Reexports: make([]bytecode.Reexport, len(m.Reexports)),
		Stars:     make([]bytecode.Star, len(m.Stars)),
	}
	for i, r := range m.Requests {
		l.Requests[i] = bytecode.Request{Specifier: r.Specifier}
		l.Requests[i].Line, l.Requests[i].Col = at(r.Pos)
	}
	for i, e := range m.Imports {
		l.Imports[i] = bytecode.Import{Request: e.Request, Name: e.Name, Namespace: e.Namespace, Slot: c.moduleSlot(e.Binding, e.Pos, "import '"+e.Binding.Name+"'")}
		l.Imports[i].Line, l.Imports[i].Col = at(e.Pos)
	}
	for i, e := range m.Reexports {
		l.Reexports[i] = bytecode.Reexport{Name: e.Name, Request: e.Request, Import: e.Import, All: e.All}
		l.Reexports[i].Line, l.Reexports[i].Col = at(e.Pos)
	}
	for i, e := range m.Stars {
		l.Stars[i] = bytecode.Star{Request: e.Request}
		l.Stars[i].Line, l.Stars[i].Col = at(e.Pos)
	}
	return l
}

// CompileScript compiles a script into a KindScript function whose return
// value is the completion value of the script. The script's declarations
// are global (GlobalDeclarationInstantiation, run by the engine before the
// function): its var and function names are properties of the global
// object and its let, const and class names bindings of the realm's global
// declarative environment, both read and written by name. The script is
// sloppy mode code unless its directive prologue says "use strict".
func CompileScript(s *syntax.Script) (*bytecode.Function, error) {
	return CompileScriptStop(s, nil)
}

// CompileScriptStop is CompileScript calling stop as CompileModuleStop
// does.
func CompileScriptStop(s *syntax.Script, stop func() error) (*bytecode.Function, error) {
	return compileUnit(func(noHoist map[*syntax.Function]bool) (*bytecode.Function, error) {
		return compileScriptWith(s, stop, noHoist)
	})
}

// compileScriptWith is CompileScriptStop without hoisting in the functions
// of noHoist.
func compileScriptWith(s *syntax.Script, stop func() error, noHoist map[*syntax.Function]bool) (fn *bytecode.Function, err error) {
	c := newCompiler(s.File, &scriptState{stop: stop}, noHoist)
	defer c.recover(&err)
	defer c.release()
	f := c.newFuncState(nil, nil, bytecode.KindScript, s.Scope)
	f.out.Strict = s.Strict
	f.out.ScriptOrModule = s.HasDirectEval // for its eval code's import()
	f.out.Source = &bytecode.SourceInfo{Name: s.File.Name, Src: s.File.Src, Start: s.Pos, End: s.End}
	f.completion = f.alloc()
	f.emitA(bytecode.LoadUndef, f.completion)
	f.enterScript(s.Scope)
	f.stmts(s.Body)
	f.emitA(bytecode.Ret, f.completion)
	return f.finish(), nil
}

// enterScript gives every declaration of the script scope s a global
// location, lists the names for GlobalDeclarationInstantiation and stores
// the hoisted function declarations.
func (f *funcState) enterScript(s *syntax.Scope) {
	sc := f.c.script
	for _, b := range s.Bindings {
		var names *[]string
		switch {
		case b.Kind.IsLexical():
			names = &f.globals().Lexical
		case b.Kind == syntax.BindFunction:
			// Listed below, in the order of their last declarations.
		case b.Kind == syntax.BindVar && b.AnnexB:
			names = &f.globals().AnnexB
		case b.Kind == syntax.BindVar:
			names = &f.globals().Var
		default:
			f.c.locs[b] = location{reg: f.alloc()}
			continue
		}
		f.c.locs[b] = location{slot: len(sc.globals), reg: -1}
		sc.globals = append(sc.globals, b)
		if names != nil {
			*names = append(*names, b.Name)
		}
	}
	// GlobalDeclarationInstantiation initializes the last declaration of
	// each function name, in the order of those declarations. The names
	// seen are a set, so that many functions compile in linear time.
	var fns []string
	seen := make(map[string]struct{}, len(s.Funcs))
	for i := len(s.Funcs) - 1; i >= 0; i-- {
		name := s.Funcs[i].Func.Name.Name
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			fns = append(fns, name)
		}
	}
	if len(fns) > 0 {
		slices.Reverse(fns)
		f.globals().Function = fns
	}
	f.instantiateFuncs(s)
}

// globals returns the script's GlobalNames, allocated on first use.
func (f *funcState) globals() *bytecode.GlobalNames {
	x := f.extra()
	if x.Globals == nil {
		x.Globals = &bytecode.GlobalNames{}
	}
	return x.Globals
}

// extra returns the function's Extra, allocated on first use.
func (f *funcState) extra() *bytecode.Extra {
	if f.out.Extra == nil {
		f.out.Extra = &bytecode.Extra{}
	}
	return f.out.Extra
}

// compiler holds per-compilation state shared by all function states.
type compiler struct {
	file *syntax.File
	locs map[*syntax.Binding]location

	classOf map[*syntax.Function]*syntax.Class    // class constructor -> its class
	bodies  map[*syntax.Function]func(*funcState) // synthetic member initializers (class.go)

	script *scriptState // CompileScript only
	hoist  *hoistState  // constants out of loops (hoist.go)
}

// newCompiler returns the compiler of one compilation of file, which
// hoists no constants in the functions of noHoist. The compilation ends
// with release.
func newCompiler(file *syntax.File, script *scriptState, noHoist map[*syntax.Function]bool) *compiler {
	c := &compiler{file: file, script: script}
	if noHoist != nil {
		c.hoist = getHoistState()
		c.hoist.noHoist = noHoist
	}
	return c
}

// scriptState is the compiler state only scripts (and code with direct
// eval calls) need, kept out of the compiler so that modules do not pay for
// it.
type scriptState struct {
	globals []*syntax.Binding // the script-scope bindings, by global location slot
	withs   []*syntax.Scope   // the with statements around the code being compiled, innermost last
	stop    func() error      // ends the compile with its error (Hook, CompileModuleStop, syntax.Options.Stop)
	evals   *evalMemo         // the direct eval call sites' scopes (eval.go)
}

// bailout is the panic that ends a compile fail stopped, carrying its
// error.
type bailout struct{ err *Error }

// stopped is the panic that ends a compile scriptState.stop stopped,
// carrying its error.
type stopped struct{ err error }

// stopEvery is the number of statements of a list between two calls of
// scriptState.stop, as syntax.Options.Stop's.
const stopEvery = 1024

func (c *compiler) recover(err *error) {
	if r := recover(); r != nil {
		switch r := r.(type) {
		case bailout:
			*err = r.err
		case stopped:
			*err = r.err
		case *hoistRetry:
			*err = r
		default:
			panic(r)
		}
	}
}

// poll calls the compile's stop function before the statement at index i
// of a list, every stopEvery statements.
func (c *compiler) poll(i int) {
	if i&(stopEvery-1) == 0 && c.script != nil && c.script.stop != nil {
		c.stopNow()
	}
}

// pollItem calls the compile's stop function after every stopEvery items
// of a list (array and object literals, arguments), of which n were
// compiled.
func (c *compiler) pollItem(n int) {
	if n&(stopEvery-1) == stopEvery-1 && c.script != nil && c.script.stop != nil {
		c.stopNow()
	}
}

//go:noinline
func (c *compiler) stopNow() {
	if err := c.script.stop(); err != nil {
		panic(stopped{err})
	}
}

// fail aborts compilation with a positioned error.
func (c *compiler) fail(pos int, msg string) {
	line, col := c.file.Position(pos)
	panic(bailout{&Error{Name: c.file.Name, Pos: pos, Line: line, Col: col, Msg: msg}})
}

// location says where a binding lives: a register of its function, a slot
// of a materialized environment, or, for a declaration of a script's top
// level, the global environment (env == nil, reg < 0; slot indexes
// scriptState.globals), accessed by name.
type location struct {
	env  *envScope
	slot int
	reg  int
}

// inReg reports whether the binding lives in register l.reg.
func (l location) inReg() bool { return l.env == nil && l.reg >= 0 }

// isGlobal reports whether the binding is a script's global declaration.
func (l location) isGlobal() bool { return l.env == nil && l.reg < 0 }

// envScope is one materialized closure environment (a scope with at least
// one captured binding, or the top-level scope).
type envScope struct {
	scope *syntax.Scope
	size  int
}

// label is a forward/backward jump target.
type label struct {
	pc      int // -1 until bound
	patches []int
}

// jumpTarget is a break/continue destination.
type jumpTarget struct {
	names         []string // label names attached to the statement
	isLoop        bool
	isSwitch      bool
	breakLbl      *label
	continueLbl   *label
	breakDepth    int // envDepth to unwind to before jumping to breakLbl
	continueDepth int
	finallyDepth  int // len(f.finallys) at the target
	// iterDepth and contIterDepth are the len(f.iterCloses) a break and a
	// continue unwind to: a for-of's continue keeps its own iterator open.
	iterDepth, contIterDepth int
}

// openIter is the iterator of an enclosing for-of that a break, continue or
// return leaving the loop must close (IteratorClose), or of an array
// pattern a generator's return leaves (only a yield resumes one inside
// an expression). finallyDepth is
// len(f.finallys) at the loop: an exit routed through a finally opened
// inside the loop closes the iterator only after that finally ran.
type openIter struct {
	reg          int
	finallyDepth int
	async        bool // a for await record: closing it awaits (asyncIterClose)
	guard        int  // the level of its loop's or pattern's guard: its close skips the rows past there
}

// finallyExit is one abrupt completion routed through a finally block.
type finallyExit struct {
	kind   int
	target *jumpTarget // break/continue target (nil for return)
	isCont bool
}

// finallyState is an active try/finally the compiler is inside of.
type finallyState struct {
	rk, rv   int    // completion kind / value registers (consecutive)
	body     *label // start of the finally body
	envDepth int    // env depth of the try statement
	exits    []finallyExit
	nextKind int
}

// funcState is the per-function compilation state.
type funcState struct {
	c      *compiler
	parent *funcState
	fn     *syntax.Function // nil at top level
	out    *bytecode.Function
	kind   bytecode.Kind
	// hoisted is set once a loop of the function keeps constants in
	// registers, scanned while the loop nest that looked for them compiles
	// (hoist.go).
	hoisted, scanned bool

	code     []uint32
	consts   []bytecode.Const
	numConst map[uint64]uint16
	strConst map[string]uint16
	keyConst map[string]uint16
	children []*bytecode.Function
	handlers []bytecode.Handler
	lines    []bytecode.LineEntry
	icCount  uint32

	nregs   int
	maxRegs int

	envStack []*envScope // materialized environments active in this function, innermost last
	envDepth int         // len(envStack) minus the function's own env (PushEnv count)
	ownEnv   *envScope   // the function's own Env (nil when it has none)

	targets       []*jumpTarget
	finallys      []*finallyState
	iterCloses    []openIter // iterators open at the current pc, innermost last
	guards        int        // guards open at the current pc
	closeGaps     []closeGap // iterator closes emitted in the open guards
	pendingLabels []string   // labels attached to the statement being compiled
	optLabel      *label     // short-circuit target of the enclosing optional chain

	curPos   int
	lastPos  int   // the position whose line and column emitWord last looked up
	lastLine int32 // the line and column of the last line-table entry
	lastCol  int32

	noFold, noFoldEnd syntax.Expr // left-spine nodes known not to fold (see foldChain)

	completion int // KindScript: register holding the completion value

	retReg int    // derived class constructor: the value being returned
	retLbl *label // derived class constructor: the DerivedResult epilogue

	genReg int // generator body: the generator (genStart); async body: its state (asyncStart)

	asyncTry int // async function body: the start of its catch-all handler (asyncStart)
}

func (c *compiler) newFuncState(parent *funcState, fn *syntax.Function, kind bytecode.Kind, scope *syntax.Scope) *funcState {
	f := &funcState{
		c:          c,
		parent:     parent,
		fn:         fn,
		kind:       kind,
		out:        &bytecode.Function{Strict: true, Kind: kind},
		numConst:   make(map[uint64]uint16),
		strConst:   make(map[string]uint16),
		keyConst:   make(map[string]uint16),
		lastPos:    -1,
		lastLine:   -1,
		completion: -1,
	}
	if c.locs == nil {
		c.locs = make(map[*syntax.Binding]location)
	}
	return f
}

// finish seals the function template. Its registers fit the 8-bit
// operands (alloc and compileBody fail first), as the engine's verifier
// requires of every template.
func (f *funcState) finish() *bytecode.Function {
	if f.maxRegs > bytecode.MaxRegister+1 {
		f.c.fail(f.curPos, "SyntaxError: function needs more than 256 registers"+unsupportedSuffix)
	}
	out := f.out
	out.Code = f.code
	out.Consts = f.consts
	out.Children = f.children
	out.Handlers = f.handlers
	out.LineTable = f.lines
	out.ICCount = f.icCount
	out.NumRegs = uint16(f.maxRegs)
	if f.ownEnv != nil && out.CaptureLayout == nil {
		out.CaptureLayout = make(bytecode.CaptureLayout, f.ownEnv.size)
		for i := range out.CaptureLayout {
			out.CaptureLayout[i] = bytecode.NoRegister
		}
	}
	return out
}

// --- registers ---------------------------------------------------------------

func (f *funcState) alloc() int {
	r := f.nregs
	f.nregs++
	if f.nregs > f.maxRegs {
		f.maxRegs = f.nregs
	}
	if r > bytecode.MaxRegister {
		if f.hoisted {
			panic(&hoistRetry{fn: f.fn})
		}
		f.c.fail(f.curPos, "SyntaxError: function needs more than 256 registers"+unsupportedSuffix)
	}
	return r
}

// allocN allocates n consecutive registers and returns the first.
func (f *funcState) allocN(n int) int {
	base := f.nregs
	for range n {
		f.alloc()
	}
	return base
}

// free releases registers down to mark.
func (f *funcState) free(mark int) { f.nregs = mark }

// --- constants -----------------------------------------------------------------

func (f *funcState) addConst(k bytecode.Const) uint16 {
	if len(f.consts) > bytecode.MaxBx {
		f.c.fail(f.curPos, "SyntaxError: function needs more than 65536 constants"+unsupportedSuffix)
	}
	f.consts = append(f.consts, k)
	return uint16(len(f.consts) - 1)
}

func (f *funcState) numberConst(v float64) uint16 {
	bits := float64Bits(v)
	if i, ok := f.numConst[bits]; ok {
		return i
	}
	i := f.addConst(bytecode.Const{Kind: bytecode.ConstNumber, Num: v})
	f.numConst[bits] = i
	return i
}

// stringConst interns a string literal (WTF-8) in the pool.
func (f *funcState) stringConst(s string) uint16 {
	if i, ok := f.strConst[s]; ok {
		return i
	}
	i := f.addConst(makeStringConst(s, false))
	f.strConst[s] = i
	return i
}

// nameConst interns a property-name constant (the engine interns the string).
func (f *funcState) nameConst(s string) uint16 {
	if i, ok := f.keyConst[s]; ok {
		return i
	}
	i := f.addConst(makeStringConst(s, true))
	f.keyConst[s] = i
	return i
}

func makeStringConst(s string, key bool) bytecode.Const {
	k := bytecode.Const{Kind: bytecode.ConstString, Str: s, Key: key, IsASCII: syntax.IsASCII(s)}
	if !k.IsASCII {
		k.Units = syntax.DecodeWTF8(s)
	}
	return k
}

func (f *funcState) newIC() uint16 {
	if f.icCount > bytecode.MaxBx {
		f.c.fail(f.curPos, "SyntaxError: function needs more than 65536 property and global variable accesses"+unsupportedSuffix)
	}
	f.icCount++
	return uint16(f.icCount - 1)
}

// --- emission ------------------------------------------------------------------

// setPos records the source position of the code emitted next.
func (f *funcState) setPos(pos int) { f.curPos = pos }

// setPosNode is setPos at the start of a node.
func (f *funcState) setPosNode(n syntax.Node) {
	pos, _ := n.Range()
	f.curPos = pos
}

func (f *funcState) emitWord(w uint32) int {
	// The words emitted at one position share its line-table entry, so the
	// position is looked up once.
	if f.curPos >= 0 && f.curPos != f.lastPos {
		f.lastPos = f.curPos
		line, col := f.c.file.Position(f.curPos)
		if int32(line) != f.lastLine || int32(col) != f.lastCol {
			f.lastLine, f.lastCol = int32(line), int32(col)
			f.lines = append(f.lines, bytecode.LineEntry{PC: uint32(len(f.code)), Line: int32(line), Col: int32(col)})
		}
	}
	f.code = append(f.code, w)
	return len(f.code) - 1
}

// emitExtra appends a raw ExtraArg word (no line-table entry).
func (f *funcState) emitExtra(x uint32) { f.code = append(f.code, x) }

func (f *funcState) emitNone(op bytecode.Op) { f.emitWord(bytecode.EncodeABC(op, 0, 0, 0)) }
func (f *funcState) emitA(op bytecode.Op, a int) {
	f.emitWord(bytecode.EncodeABC(op, uint8(a), 0, 0))
}
func (f *funcState) emitAB(op bytecode.Op, a, b int) {
	f.emitWord(bytecode.EncodeABC(op, uint8(a), uint8(b), 0))
}
func (f *funcState) emitABC(op bytecode.Op, a, b, c int) {
	f.emitWord(bytecode.EncodeABC(op, uint8(a), uint8(b), uint8(c)))
}
func (f *funcState) emitABx(op bytecode.Op, a int, bx uint16) {
	f.emitWord(bytecode.EncodeABx(op, uint8(a), bx))
}

func (f *funcState) emitMove(dst, src int) {
	if dst != src {
		f.emitAB(bytecode.Move, dst, src)
	}
}

func (f *funcState) newLabel() *label { return &label{pc: -1} }

// bind places lbl at the current pc and patches pending jumps.
func (f *funcState) bind(lbl *label) {
	lbl.pc = len(f.code)
	for _, at := range lbl.patches {
		f.patchJump(at, lbl.pc)
	}
	lbl.patches = nil
}

func (f *funcState) patchJump(at, target int) {
	off := target - (at + 1)
	if off < bytecode.MinSBx || off > bytecode.MaxSBx {
		f.c.fail(f.curPos, "SyntaxError: jump offset out of range (a jump over more than 32767 instructions)"+unsupportedSuffix)
	}
	w := f.code[at]
	f.code[at] = bytecode.EncodeAsBx(bytecode.DecodeOp(w), bytecode.DecodeA(w), int16(off))
}

// emitJump emits a jump instruction (Jmp or a conditional with register a)
// to lbl, patching later if the label is not yet bound.
func (f *funcState) emitJump(op bytecode.Op, a int, lbl *label) {
	at := f.emitWord(bytecode.EncodeAsBx(op, uint8(a), 0))
	if lbl.pc >= 0 {
		f.patchJump(at, lbl.pc)
		return
	}
	lbl.patches = append(lbl.patches, at)
}

// pc returns the address of the next instruction.
func (f *funcState) pc() int { return len(f.code) }

// --- bindings and environments ---------------------------------------------------

// scopeNeedsEnv reports whether a (non-top-level) scope has captured bindings.
func scopeNeedsEnv(s *syntax.Scope) bool {
	for _, b := range s.Bindings {
		if b.Captured {
			return true
		}
	}
	return false
}

// pushEnvScope materializes s as a nested environment: assigns slots to its
// captured bindings, emits PushEnv and returns the record (nil when the scope
// needs no environment).
func (f *funcState) pushEnvScope(s *syntax.Scope) *envScope {
	if s == nil || !scopeNeedsEnv(s) {
		return nil
	}
	es := &envScope{scope: s}
	for _, b := range s.Bindings {
		if b.Captured {
			f.c.locs[b] = location{env: es, slot: es.size, reg: -1}
			es.size++
		}
	}
	if es.size > bytecode.MaxBx {
		f.c.fail(f.curPos, "SyntaxError: block needs more than 65535 captured bindings"+unsupportedSuffix)
	}
	f.emitABx(bytecode.PushEnv, 0, uint16(es.size))
	f.envStack = append(f.envStack, es)
	f.envDepth++
	return es
}

func (f *funcState) popEnvScope(es *envScope) {
	if es == nil {
		return
	}
	f.emitNone(bytecode.PopEnv)
	f.envStack = f.envStack[:len(f.envStack)-1]
	f.envDepth--
}

// allocScopeRegs gives every un-captured binding of s (other than parameters,
// which already have registers) a fresh register. Returns the allocation mark
// to restore at scope exit.
func (f *funcState) allocScopeRegs(s *syntax.Scope) int {
	mark := f.nregs
	if s == nil {
		return mark
	}
	for _, b := range s.Bindings {
		if b.Captured {
			continue
		}
		if _, ok := f.c.locs[b]; ok {
			continue
		}
		f.c.locs[b] = location{env: nil, reg: f.alloc()}
	}
	return mark
}

// enterScope materializes a block-like scope: environment, registers, TDZ
// markers and hoisted function declarations. It returns the state needed by
// leaveScope.
func (f *funcState) enterScope(s *syntax.Scope) (es *envScope, mark int) {
	if s == nil {
		return nil, f.nregs
	}
	es = f.pushEnvScope(s)
	mark = f.allocScopeRegs(s)
	f.initTDZ(s)
	f.instantiateFuncs(s)
	return es, mark
}

func (f *funcState) leaveScope(es *envScope, mark int) {
	f.popEnvScope(es)
	f.free(mark)
}

// enterTopLevel materializes the module/script scope: every binding lives in
// the function's own environment.
func (f *funcState) enterTopLevel(s *syntax.Scope, body []syntax.Stmt) {
	es := &envScope{scope: s}
	for _, b := range s.Bindings {
		f.c.locs[b] = location{env: es, slot: es.size, reg: -1}
		es.size++
	}
	f.ownEnv = es
	f.envStack = append(f.envStack, es)
	f.initTDZ(s)
	f.instantiateFuncs(s)
}

// initTDZ writes the hole marker into every binding of s that needs it (a
// nil scope, such as a for-of loop over an existing binding, has none). An
// exported lexical binding gets it even when no code reads it early, so
// that the host can tell it uninitialized while the top level awaits or
// after it failed; its reads are checked only when it needs TDZ.
func (f *funcState) initTDZ(s *syntax.Scope) {
	if s == nil {
		return
	}
	var hole = -1
	mark := f.nregs
	for _, b := range s.Bindings {
		if !b.NeedsTDZ && !(b.Exported && b.Kind.IsLexical()) {
			continue
		}
		loc := f.c.locs[b]
		if loc.inReg() {
			f.emitA(bytecode.LoadHole, loc.reg)
			continue
		}
		if hole < 0 {
			hole = f.alloc()
			f.emitA(bytecode.LoadHole, hole)
		}
		f.storeLoc(loc, hole)
	}
	f.free(mark)
}

// instantiateFuncs creates the hoisted function declarations of s.
func (f *funcState) instantiateFuncs(s *syntax.Scope) {
	for _, fd := range s.Funcs {
		fn := fd.Func
		var b *syntax.Binding
		if fn.Name != nil {
			b = fn.Name.Binding
		}
		if b == nil {
			b = s.Lookup("*default*")
		}
		if b == nil {
			f.c.fail(fd.Pos, "internal: hoisted function without binding")
		}
		name := fn.Name
		nameStr := "default"
		if name != nil {
			nameStr = name.Name
		}
		loc := f.c.locs[b]
		mark := f.nregs
		dst := f.locTarget(loc)
		f.setPos(fd.Pos)
		idx := f.compileFunction(fn, nameStr)
		f.emitClosure(dst, idx)
		f.storeFromTarget(loc, dst)
		f.free(mark)
	}
}

// envDepthOf returns the runtime depth (number of parent links) from the
// current environment to es.
func (f *funcState) envDepthOf(es *envScope) int {
	depth := 0
	for fs := f; fs != nil; fs = fs.parent {
		for i := len(fs.envStack) - 1; i >= 0; i-- {
			if fs.envStack[i] == es {
				return depth
			}
			depth++
		}
	}
	f.c.fail(f.curPos, "internal: environment not on the static chain")
	return 0
}

// loadLoc emits code that loads a binding into dst.
func (f *funcState) loadLoc(b *syntax.Binding, dst int) {
	loc := f.c.locs[b]
	if loc.env == nil {
		if loc.reg < 0 {
			// A global declaration: the engine checks a lexical
			// binding's initialization.
			f.getGlobal(b.Name, dst)
			return
		}
		if b.NeedsTDZ {
			f.emitA(bytecode.CheckTDZ, loc.reg)
			f.emitExtra(uint32(f.stringConst(b.Name)))
		}
		f.emitMove(dst, loc.reg)
		return
	}
	depth := f.envDepthOf(loc.env)
	f.checkDepth(depth)
	if b.Kind == syntax.BindImport {
		f.loadImport(dst, depth, loc.slot, b.Name)
		return
	}
	if loc.slot <= bytecode.MaxRegister {
		if b.NeedsTDZ {
			f.emitABC(bytecode.GetEnvChk, dst, depth, loc.slot)
			f.emitExtra(uint32(f.stringConst(b.Name)))
			return
		}
		f.emitABC(bytecode.GetEnv, dst, depth, loc.slot)
		return
	}
	if b.NeedsTDZ {
		f.emitAB(bytecode.GetEnvChkW, dst, depth)
		f.emitExtra(uint32(loc.slot))
		f.emitExtra(uint32(f.stringConst(b.Name)))
		return
	}
	f.emitAB(bytecode.GetEnvW, dst, depth)
	f.emitExtra(uint32(loc.slot))
}

// loadImport emits the read of an import binding in slot of env^depth: the
// slot holds a reference to the exporter's binding, which is in its TDZ
// until the exporter initializes it.
func (f *funcState) loadImport(dst, depth, slot int, name string) {
	if slot <= bytecode.MaxRegister {
		f.emitABC(bytecode.GetImport, dst, depth, slot)
		f.emitExtra(uint32(f.stringConst(name)))
		return
	}
	f.emitAB(bytecode.GetImportW, dst, depth)
	f.emitExtra(uint32(slot))
	f.emitExtra(uint32(f.stringConst(name)))
}

// storeLoc emits code that stores register src into a binding location
// (initialization or assignment; no TDZ check). A global lexical
// declaration is initialized, a global var or function assigned.
func (f *funcState) storeLoc(loc location, src int) {
	if loc.env == nil {
		if loc.reg < 0 {
			b := f.c.script.globals[loc.slot]
			if !b.Kind.IsLexical() {
				f.setGlobal(b.Name, src)
				return
			}
			isConst := uint16(0)
			if b.Kind == syntax.BindConst {
				isConst = 1
			}
			f.emitA(bytecode.InitGlobal, src)
			f.emitExtra(bytecode.ExtraArg(f.nameConst(b.Name), isConst))
			return
		}
		f.emitMove(loc.reg, src)
		return
	}
	depth := f.envDepthOf(loc.env)
	f.checkDepth(depth)
	if loc.slot <= bytecode.MaxRegister {
		f.emitABC(bytecode.SetEnv, src, depth, loc.slot)
		return
	}
	f.emitAB(bytecode.SetEnvW, src, depth)
	f.emitExtra(uint32(loc.slot))
}

// locTarget returns a register that code may compute a binding's new value
// into: the binding's own register, or a fresh temporary for environment
// bindings (to be stored with storeFromTarget).
func (f *funcState) locTarget(loc location) int {
	if loc.inReg() {
		return loc.reg
	}
	return f.alloc()
}

// storeFromTarget completes a locTarget write.
func (f *funcState) storeFromTarget(loc location, reg int) {
	if !loc.inReg() {
		f.storeLoc(loc, reg)
	}
}

// getGlobal emits the read of global name into dst.
func (f *funcState) getGlobal(name string, dst int) {
	f.emitA(bytecode.GetGlobal, dst)
	f.emitExtra(bytecode.ExtraArg(f.nameConst(name), f.newIC()))
}

// setGlobal emits PutValue of src to global name: an unresolvable name
// throws in strict code and becomes a global object property in sloppy
// code.
func (f *funcState) setGlobal(name string, src int) {
	op := bytecode.SetGlobal
	if !f.out.Strict {
		op = bytecode.SetGlobalSloppy
	}
	f.emitA(op, src)
	f.emitExtra(bytecode.ExtraArg(f.nameConst(name), f.newIC()))
}

// resolveGlobal emits the strict ResolveBinding of the undeclared name into
// a fresh register, which it returns with the cache setGlobalRef shares.
func (f *funcState) resolveGlobal(name string) (ref int, ic uint16) {
	ref, ic = f.alloc(), f.newIC()
	f.emitA(bytecode.ResolveGlobal, ref)
	f.emitExtra(bytecode.ExtraArg(f.nameConst(name), ic))
	return ref, ic
}

// setGlobalRef emits PutValue of src to the reference to name that
// resolveGlobal resolved into ref with cache ic.
func (f *funcState) setGlobalRef(name string, src, ref int, ic uint16) {
	f.emitAB(bytecode.SetGlobalRef, src, ref)
	f.emitExtra(bytecode.ExtraArg(f.nameConst(name), ic))
}

// regOf returns the register of an un-captured binding that needs no TDZ
// check, or -1.
func (f *funcState) regOf(b *syntax.Binding) int {
	if b == nil || b.NeedsTDZ {
		return -1
	}
	loc, ok := f.c.locs[b]
	if !ok || loc.env != nil || f.withsFor(b) != nil {
		return -1
	}
	return loc.reg
}

func (f *funcState) checkDepth(d int) {
	if d > bytecode.MaxRegister {
		f.c.fail(f.curPos, "SyntaxError: closure nesting too deep"+unsupportedSuffix)
	}
}

// capHint returns the Bx operand of NewArray or NewObject for a literal of
// n elements or properties: the room the engine allocates first, a hint,
// so a longer literal is not limited by it.
func capHint(n int) uint16 { return uint16(min(n, bytecode.MaxBx)) }

// --- functions -------------------------------------------------------------------

// compileFunction compiles a nested function and returns its child index.
// inferredName is used when the function is anonymous (NamedEvaluation).
func (f *funcState) compileFunction(fn *syntax.Function, inferredName string) uint16 {
	kind := bytecode.KindNormal
	switch fn.Kind {
	case syntax.FuncArrow:
		kind = bytecode.KindArrow
	case syntax.FuncMethod, syntax.FuncGetter, syntax.FuncSetter, syntax.FuncClassFields, syntax.FuncClassStatic:
		kind = bytecode.KindMethod
	case syntax.FuncClassConstructor:
		kind = bytecode.KindClassCtor
		if fn.Derived {
			kind = bytecode.KindDerivedCtor
		}
	}
	if (fn.IsGenerator || fn.IsAsync) && kind != bytecode.KindArrow {
		kind = bytecode.KindMethod // not constructible
	}
	child := f.c.newFuncState(f, fn, kind, fn.Scope)
	name := inferredName
	if fn.Name != nil && fn.Kind != syntax.FuncGetter && fn.Kind != syntax.FuncSetter {
		// An accessor's inferred name carries the "get "/"set " prefix.
		name = fn.Name.Name
	}
	child.out.Strict = fn.IsStrict
	child.out.Name = name
	child.out.Length = fn.Length
	child.out.NumParams = uint16(fn.ParamCount)
	child.out.HasRest = fn.HasRest
	child.out.HasArguments = fn.ArgumentsBinding() != nil
	child.out.Generator = fn.IsGenerator
	child.out.Async = fn.IsAsync
	child.out.Source = &bytecode.SourceInfo{Name: f.c.file.Name, Src: f.c.file.Src, Start: fn.Pos, End: fn.End}
	child.compileBody()
	if len(f.children) > bytecode.MaxBx {
		f.c.fail(fn.Pos, "SyntaxError: function needs more than 65536 nested functions"+unsupportedSuffix)
	}
	f.children = append(f.children, child.finish())
	return uint16(len(f.children) - 1)
}

// compileBody emits a nested function's prologue and body.
func (f *funcState) compileBody() {
	fn := f.fn
	s := fn.Scope
	f.setPos(fn.Pos)

	// Parameter registers: simple identifier parameters bind directly to
	// R[i]; pattern parameters, and names in TDZ while earlier parameters are
	// bound, keep R[i] as the raw argument. The entry creates the arguments
	// object in the register after them.
	nparams := fn.ParamCount
	f.nregs = nparams
	if fn.HasRest {
		f.nregs++
	}
	argsReg := f.nregs
	args := fn.ArgumentsBinding()
	if b := args; b != nil {
		f.nregs++
		if !b.Captured {
			f.c.locs[b] = location{reg: argsReg}
		}
	}
	f.maxRegs = f.nregs
	if f.maxRegs > bytecode.MaxRegister+1 {
		// The parameters, the rest array and the arguments object take
		// their registers past alloc's check.
		f.c.fail(fn.Pos, "SyntaxError: function needs more than 256 registers"+unsupportedSuffix)
	}
	for i, p := range fn.Params {
		if b := paramBinding(p); b != nil && !b.Captured {
			f.c.locs[b] = location{reg: i}
		}
	}
	if b := paramBinding(fn.Rest); b != nil && !b.Captured {
		f.c.locs[b] = location{reg: nparams}
	}

	// Own environment for captured bindings, with parameters boxed by layout.
	if scopeNeedsEnv(s) {
		es := &envScope{scope: s}
		for _, b := range s.Bindings {
			if b.Captured {
				f.c.locs[b] = location{env: es, slot: es.size, reg: -1}
				es.size++
			}
		}
		f.ownEnv = es
		f.envStack = append(f.envStack, es)
		layout := make(bytecode.CaptureLayout, es.size)
		for i := range layout {
			layout[i] = bytecode.NoRegister
		}
		for i, p := range fn.Params {
			if b := paramBinding(p); b != nil && b.Captured {
				layout[f.c.locs[b].slot] = uint16(i)
			}
		}
		if b := paramBinding(fn.Rest); b != nil && b.Captured {
			layout[f.c.locs[b].slot] = uint16(nparams)
		}
		if b := args; b != nil && b.Captured {
			layout[f.c.locs[b].slot] = uint16(argsReg)
		}
		f.out.CaptureLayout = layout
	}

	// Registers for the remaining un-captured bindings; vars start undefined.
	varStart := f.nregs
	f.allocScopeRegs(s)
	if n := f.nregs - varStart; n > 0 {
		f.emitAB(bytecode.UndefRange, varStart, n)
	}
	f.initTDZ(s)
	f.prologue()
	if !fn.IsStrict {
		// OrdinaryCallBindThis and CreateMappedArgumentsObject, before
		// anything reads this or the arguments object.
		if fn.UsesThis && !fn.IsArrow {
			f.emitNone(bytecode.CoerceThis)
		}
		if fn.HasSimpleParams && args != nil {
			f.emitA(bytecode.MapArguments, argsReg)
		}
	}
	f.asyncStart()
	if fn.HasDirectEval && !fn.IsStrict {
		// Names resolve through the %evalvars objects of the variable
		// environments (sloppy.go).
		if f.c.script == nil {
			f.c.script = &scriptState{}
		}
		sc := f.c.script
		defer func(n int) { sc.withs = sc.withs[:n] }(len(sc.withs))
		f.evalVars(s)
	}

	// Named function expression self binding.
	if fn.SelfBinding != nil {
		loc := f.c.locs[fn.SelfBinding]
		mark := f.nregs
		dst := f.locTarget(loc)
		f.emitA(bytecode.LoadCallee, dst)
		f.storeFromTarget(loc, dst)
		f.free(mark)
	}

	// Parameter defaults and destructuring, in order.
	for i, p := range fn.Params {
		f.bindParam(p, i)
	}
	if fn.Rest != nil {
		if id, ok := fn.Rest.(*syntax.Ident); !ok {
			f.bindPattern(fn.Rest, nparams, bindInit)
		} else if id.Binding != nil && id.Binding.NeedsTDZ {
			f.storeLoc(f.c.locs[id.Binding], nparams)
		}
	}

	if fn.Body != nil && fn.Body.Scope != nil {
		f.enterBodyScope(fn.Body.Scope)
	} else {
		f.instantiateFuncs(s)
	}
	if fn.DefaultCtor && fn.Derived {
		f.defaultSuperCall()
	}

	if body := f.c.bodies[fn]; body != nil {
		body(f)
		f.endBody()
		return
	}
	if fn.Body != nil {
		if fn.IsGenerator {
			f.genStart()
		}
		f.stmts(fn.Body.Body)
		f.endBody()
		return
	}
	f.setPosNode(fn.ExprBody)
	f.returnExpr(fn.ExprBody)
	if f.retLbl != nil {
		f.endBody() // an async arrow's epilogue
	}
}

// enterBodyScope materializes the separate scope of a body whose
// declarations are kept apart from the parameters: a var named like a
// parameter starts with the parameter's value.
func (f *funcState) enterBodyScope(vs *syntax.Scope) {
	f.pushEnvScope(vs)
	varStart := f.nregs
	f.allocScopeRegs(vs)
	if n := f.nregs - varStart; n > 0 {
		f.emitAB(bytecode.UndefRange, varStart, n)
	}
	for _, b := range vs.Bindings {
		if b.Kind != syntax.BindVar {
			continue
		}
		if p := vs.Parent.Lookup(b.Name); p != nil && (p.Kind == syntax.BindParam || p.Kind == syntax.BindArgs) {
			mark := f.nregs
			r := f.alloc()
			f.loadLoc(p, r)
			f.storeLoc(f.c.locs[b], r)
			f.free(mark)
		}
	}
	f.initTDZ(vs)
	if f.fn.HasDirectEval && !f.fn.IsStrict {
		f.evalVars(vs)
	}
	f.instantiateFuncs(vs)
}

// paramBinding returns the binding of a parameter that is a plain name or a
// plain name with a default and lives where the argument arrives (R[i], or
// the environment slot the capture layout boxes it into), else nil.
func paramBinding(p syntax.Pattern) *syntax.Binding {
	var id *syntax.Ident
	switch p := p.(type) {
	case *syntax.Ident:
		id = p
	case *syntax.AssignPattern:
		id, _ = p.Target.(*syntax.Ident)
	}
	if id == nil || id.Binding == nil || id.Binding.NeedsTDZ {
		return nil
	}
	return id.Binding
}

// bindParam initializes parameter i from its argument register R[i]. Plain
// names already live in R[i] (or were boxed into the environment by the
// capture layout); defaults and patterns are applied here.
func (f *funcState) bindParam(p syntax.Pattern, i int) {
	switch p := p.(type) {
	case *syntax.Ident:
		if p.Binding != nil && p.Binding.NeedsTDZ {
			f.storeLoc(f.c.locs[p.Binding], i)
		}
		return
	case *syntax.AssignPattern:
		f.setPos(p.Pos)
		skip := f.newLabel()
		f.emitJump(bytecode.JmpNotUndef, i, skip)
		if id, ok := p.Target.(*syntax.Ident); ok {
			f.exprNamed(p.Default, i, id.Name)
			if b := id.Binding; b != nil && b.NeedsTDZ {
				f.bind(skip)
				f.storeLoc(f.c.locs[b], i)
				return
			}
			if id.Binding != nil && id.Binding.Captured {
				f.storeLoc(f.c.locs[id.Binding], i)
			}
			f.bind(skip)
			return
		}
		f.expr(p.Default, i)
		f.bind(skip)
		f.bindPattern(p.Target, i, bindInit)
	default:
		f.setPosNode(p)
		f.bindPattern(p, i, bindInit)
	}
}

// emitReturn returns the value in reg, routing through enclosing finally
// blocks when necessary.
func (f *funcState) emitReturn(reg int) {
	if n := len(f.finallys); n > 0 {
		fs := f.finallys[n-1]
		f.emitIterCloses(f.finallyIterBase())
		f.emitMove(fs.rv, reg)
		f.emitWord(bytecode.EncodeAsBx(bytecode.LoadInt, uint8(fs.rk), 2))
		f.emitPopEnvs(fs.envDepth)
		f.emitJump(bytecode.Jmp, 0, fs.body)
		fs.noteExit(finallyExit{kind: 2})
		return
	}
	f.emitIterCloses(0)
	if f.asyncBody() {
		f.asyncReturn(reg)
		return
	}
	if f.retLbl != nil {
		f.derivedReturn(reg)
		return
	}
	f.emitA(bytecode.Ret, reg)
}

// emitIterCloses closes the open for-of iterators from index from on,
// innermost first. Each close is a gap in the handler rows of the guards
// opened inside its loop (addHandler): a throwing return method replaces
// the exit's completion where the loop completes and reaches the handlers
// around the loop, which close the outer loops with the throw completion.
// The loop's own row still covers the close; it rethrows, the record being
// closed already.
func (f *funcState) emitIterCloses(from int) {
	for i := len(f.iterCloses) - 1; i >= from; i-- {
		start := f.pc()
		if f.iterCloses[i].async {
			f.asyncIterClose(f.iterCloses[i].reg)
		} else {
			f.emitWord(bytecode.EncodeAsBx(bytecode.IterClose, uint8(f.iterCloses[i].reg), 0))
		}
		if g := f.iterCloses[i].guard; g+1 < f.guards {
			f.closeGaps = append(f.closeGaps, closeGap{start: uint32(start), end: uint32(f.pc()), level: g})
		}
	}
}

// finallyIterBase returns the index of the first open iterator inside the
// innermost try/finally: an exit routed through that finally closes those
// before running it and the enclosing ones after (from the finally's
// completion dispatch).
func (f *funcState) finallyIterBase() int {
	n := len(f.iterCloses)
	for n > 0 && f.iterCloses[n-1].finallyDepth == len(f.finallys) {
		n--
	}
	return n
}

// emitPopEnvs pops nested environments down to depth.
func (f *funcState) emitPopEnvs(depth int) {
	for d := f.envDepth; d > depth; d-- {
		f.emitNone(bytecode.PopEnv)
	}
}

func (fs *finallyState) noteExit(e finallyExit) {
	for _, x := range fs.exits {
		if x.kind == e.kind {
			return
		}
	}
	fs.exits = append(fs.exits, e)
}

// exitKind returns the completion kind routed through fs for a break or
// continue to target.
func (fs *finallyState) exitKind(target *jumpTarget, isCont bool) int {
	for _, x := range fs.exits {
		if x.target == target && x.isCont == isCont {
			return x.kind
		}
	}
	if fs.nextKind < 3 {
		fs.nextKind = 3
	}
	k := fs.nextKind
	fs.nextKind++
	fs.exits = append(fs.exits, finallyExit{kind: k, target: target, isCont: isCont})
	return k
}

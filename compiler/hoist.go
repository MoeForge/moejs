package compiler

import (
	"cmp"
	"math"
	"slices"
	"sync/atomic"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Constants out of loops. An operand that is a number, string or BigInt
// literal is loaded into a temporary where it is used, so a loop loads it
// again on every iteration. The outermost for, while or do-while loop of a
// nest instead scans its condition, update and body for the literals its
// operators read, gives each a register before the loop and loads it there
// once; while the nest compiles, operand and exprReg return that register
// for the literal. The register is only read, as a variable's register is,
// so it keeps the constant for the whole nest, across yields and handlers
// included. A for-in or for-of loop uses the constants of a loop around it
// but does not start a nest: each of its iterations costs an iterator step
// that dwarfs a load, and the plugins' loops of that kind run a few times,
// where loading every constant of the body up front saves nothing and
// scanning it costs compile time.
//
// What is scanned is what binaryOp, the compound assignments and the
// computed member keys read through operand or exprReg, so that a hoisted
// constant replaces at least one load in the loop. Operands with an
// immediate form (x + 1, x - 1) and the literal of a typeof test load
// nothing and are left out, as are literals in return and throw
// statements, which run at most once per entry into the loop, and nested
// functions and classes. A literal of an inner loop counts four times one
// of the loop around it; past maxHoist literals the most used are kept.
//
// A loop that runs no iteration pays one load per hoisted constant that it
// did not pay before, and one that runs once breaks even: the loads are
// not guarded by the loop's condition on purpose, as a guard would cost
// every loop that does run.
//
// A function that runs out of registers with constants hoisted compiles
// again without hoisting in it (hoistRetry, compileUnit).

// maxHoist is the most constants one loop nest keeps in registers.
const maxHoist = 8

// hoistKey identifies a hoisted constant: a number by its bits, a string
// or a BigInt literal by its text.
type hoistKey struct {
	kind uint8 // kNumber, kString or hoistBigInt
	bits uint64
	str  string
}

// hoistBigInt is the hoistKey kind of a BigInt literal.
const hoistBigInt = uint8(kUndefined + 1)

// hoistReg is a constant held in register reg while a loop of function f
// compiles.
type hoistReg struct {
	f   *funcState
	key hoistKey
	reg int
}

// hoistState is the hoisting state of a compilation, taken from
// spareHoist with the first loop it compiles (or before, with noHoist, by
// newCompiler) and put back by release.
type hoistState struct {
	// regs holds the constants of the loop nests being compiled, the
	// innermost function's last: a function nested in a loop compiles
	// with the loop's entries below its own.
	regs []hoistReg
	// noHoist lists the functions that compile without hoisting; nil is
	// the key of top-level code.
	noHoist map[*syntax.Function]bool
	scan    hoistScan
}

// spareHoist keeps one hoistState, with its slices, from one compilation
// to the next, so that the state costs a compile no allocation and the
// bytes a compile allocates (a gate row, docs/DESIGN.md §14.2) do not grow
// with it. A compile running beside another one allocates its own. Not a
// sync.Pool: a compile allocates enough to run the collector every few
// compiles, and each collection empties a pool, whose next Put then
// allocates its per-P array again, about 1 KiB at GOMAXPROCS=8.
var spareHoist atomic.Pointer[hoistState]

func getHoistState() *hoistState {
	if h := spareHoist.Swap(nil); h != nil {
		return h
	}
	return new(hoistState)
}

// release keeps the compilation's hoisting state as spareHoist, with its
// slices' room but nothing they pointed to.
func (c *compiler) release() {
	h := c.hoist
	if h == nil {
		return
	}
	c.hoist = nil
	clear(h.regs[:cap(h.regs)])
	h.regs = h.regs[:0]
	h.noHoist = nil
	h.scan.reset()
	spareHoist.Store(h)
}

// hoistRetry is the panic of a function that ran out of registers after it
// hoisted constants: compileUnit compiles the unit again without hoisting
// in fn.
type hoistRetry struct{ fn *syntax.Function }

func (r *hoistRetry) Error() string { return "compiler: out of registers with hoisted constants" }

// compileUnit runs compile, a whole compilation with a fresh compiler, and
// runs it again without hoisting in each function that ran out of
// registers with it.
func compileUnit(compile func(noHoist map[*syntax.Function]bool) (*bytecode.Function, error)) (*bytecode.Function, error) {
	var noHoist map[*syntax.Function]bool
	for {
		fn, err := compile(noHoist)
		r, ok := err.(*hoistRetry)
		if !ok {
			return fn, err
		}
		if noHoist == nil {
			noHoist = make(map[*syntax.Function]bool)
		}
		noHoist[r.fn] = true
	}
}

// hoistedReg returns the register holding the constant e, a literal, while
// a loop of f has it hoisted, or -1.
func (f *funcState) hoistedReg(e syntax.Expr) int {
	if !f.scanned {
		return -1
	}
	h := f.c.hoist
	n := len(h.regs)
	if n == 0 || h.regs[n-1].f != f {
		return -1
	}
	k, ok := literalKey(e)
	if !ok {
		return -1
	}
	for i := n - 1; i >= 0 && h.regs[i].f == f; i-- {
		if h.regs[i].key == k {
			return h.regs[i].reg
		}
	}
	return -1
}

// literalKey returns the key of e when e is a number, string or BigInt
// literal, or a negated number literal.
func literalKey(e syntax.Expr) (hoistKey, bool) {
	switch e := e.(type) {
	case *syntax.NumberLit:
		return hoistKey{kind: uint8(kNumber), bits: math.Float64bits(e.Value)}, true
	case *syntax.StringLit:
		return hoistKey{kind: uint8(kString), str: e.Value}, true
	case *syntax.BigIntLit:
		return hoistKey{kind: hoistBigInt, str: e.Digits}, true
	case *syntax.UnaryExpr:
		if n, ok := e.X.(*syntax.NumberLit); ok && e.Op == syntax.Minus {
			return hoistKey{kind: uint8(kNumber), bits: math.Float64bits(-n.Value)}, true
		}
	}
	return hoistKey{}, false
}

// hoistLoop starts the for, while or do-while loop, after a for's init:
// when no loop of f around it scanned its nest, it loads the constants of
// the nest into registers it allocates (freed with the loop's own) and
// returns the mark endHoist takes, else -1.
func (f *funcState) hoistLoop(loop syntax.Stmt) int {
	h := f.c.hoist
	if f.scanned || h != nil && h.noHoist[f.fn] {
		return -1
	}
	if h == nil {
		h = getHoistState()
		f.c.hoist = h
	}
	f.scanned = true
	sc := &h.scan
	sc.reset()
	sc.loop(loop)
	mark := len(h.regs)
	if len(sc.cands) == 0 {
		return mark
	}
	keep := sc.keep()
	f.hoisted = true // before allocating: running out of registers retries
	base := f.allocN(len(keep))
	h.regs = slices.Grow(h.regs, len(keep))
	for i, c := range keep {
		f.emitHoisted(c.key, base+i)
		h.regs = append(h.regs, hoistReg{f: f, key: c.key, reg: base + i})
	}
	return mark
}

// emitHoisted loads the hoisted constant k into reg, as expr loads its
// literal.
func (f *funcState) emitHoisted(k hoistKey, reg int) {
	switch k.kind {
	case hoistBigInt:
		f.emitABx(bytecode.LoadConst, reg, f.addConst(bytecode.Const{Kind: bytecode.ConstBigInt, Str: k.str}))
	case uint8(kString):
		f.emitConst(constant{kind: kString, str: k.str}, reg)
	default:
		f.emitConst(constant{kind: kNumber, num: math.Float64frombits(k.bits)}, reg)
	}
}

// endHoist ends the loop that hoistLoop returned mark for.
func (f *funcState) endHoist(mark int) {
	if mark >= 0 {
		h := f.c.hoist
		h.regs = h.regs[:mark]
		f.scanned = false
	}
}

// hoistCand is a literal a loop nest loads: its key, its weight (4^depth
// per occurrence, depth counting the inner loops) and its rank in source
// order.
type hoistCand struct {
	key    hoistKey
	weight int64
	order  int32
}

// hoistScan finds the hoistable literals of a loop nest (syntax.Visitor).
type hoistScan struct {
	cands []hoistCand
	depth int
}

// keep returns the constants a nest hoists, in the order of their loads:
// past maxHoist candidates the most used, in source order.
func (s *hoistScan) keep() []hoistCand {
	keep := s.cands
	if len(keep) > maxHoist {
		slices.SortStableFunc(keep, func(a, b hoistCand) int { return cmp.Compare(b.weight, a.weight) })
		keep = keep[:maxHoist]
		slices.SortFunc(keep, func(a, b hoistCand) int { return cmp.Compare(a.order, b.order) })
	}
	return keep
}

func (s *hoistScan) reset() {
	clear(s.cands[:cap(s.cands)])
	s.cands = s.cands[:0]
	s.depth = 0
}

// maxHoistDepth caps the depth that weighs an occurrence.
const maxHoistDepth = 8

// maxHoistCands caps the distinct literals a scan counts: each occurrence
// is looked up among them, which in a body of n distinct literals cost
// O(n²) (16,000 took 91 ms to compile). Past the cap only the counted ones
// gain weight.
const maxHoistCands = 64

// loop scans the parts of loop that run on every iteration.
func (s *hoistScan) loop(loop syntax.Stmt) {
	switch l := loop.(type) {
	case *syntax.ForStmt:
		s.walk(l.Cond)
		s.walk(l.Update)
		s.walk(l.Body)
	case *syntax.WhileStmt:
		s.walk(l.Cond)
		s.walk(l.Body)
	case *syntax.DoWhileStmt:
		s.walk(l.Body)
		s.walk(l.Cond)
	case *syntax.ForInOfStmt: // nested in one of the others
		s.walk(l.Left)
		s.walk(l.Body)
	}
}

func (s *hoistScan) walk(n syntax.Node) {
	if n != nil {
		syntax.Walk(s, n)
	}
}

// Visit implements syntax.Visitor.
func (s *hoistScan) Visit(n syntax.Node) syntax.Visitor {
	switch n := n.(type) {
	case *syntax.Function, *syntax.Class, *syntax.ReturnStmt, *syntax.ThrowStmt:
		return nil
	case *syntax.ForStmt:
		s.walk(n.Init)
		s.inner(n)
		return nil
	case *syntax.ForInOfStmt:
		s.walk(n.Right)
		s.inner(n)
		return nil
	case *syntax.WhileStmt, *syntax.DoWhileStmt:
		s.inner(n.(syntax.Stmt))
		return nil
	case *syntax.BinaryExpr:
		s.binary(n)
	case *syntax.MemberExpr:
		// A computed key other than a property name is read with exprReg
		// (propName). o["5"] and o[5] take a register each: the string and
		// the number are different constants to the operators that read
		// them, though both name the same property here.
		if n.Computed {
			if lit, ok := n.Prop.(*syntax.StringLit); !ok || isArrayIndexName(lit.Value) {
				s.add(n.Prop)
			}
		}
	case *syntax.AssignExpr:
		if _, ok := compoundOps[n.Op]; ok {
			s.add(n.Value)
		}
	}
	return s
}

// inner scans a loop nested in the one being scanned, one level deeper.
func (s *hoistScan) inner(loop syntax.Stmt) {
	s.depth++
	s.loop(loop)
	s.depth--
}

// binary adds the literal operands binaryOp loads.
func (s *hoistScan) binary(e *syntax.BinaryExpr) {
	if _, ok := e.X.(*syntax.PrivateName); ok {
		s.add(e.Y)
		return
	}
	if _, _, ok := typeofIsParts(e); ok {
		return // TypeofIs
	}
	_, xLit := literalKey(e.X)
	_, yLit := literalKey(e.Y)
	if xLit && yLit {
		return // folded
	}
	if xLit {
		s.add(e.X)
	}
	if yLit && !immediateOperand(e) {
		s.add(e.Y)
	}
}

// immediateOperand reports whether binaryOp encodes the right operand of e
// in an AddImm or SubImm.
func immediateOperand(e *syntax.BinaryExpr) bool {
	if e.Op != syntax.Plus && e.Op != syntax.Minus {
		return false
	}
	k, ok := literalKey(e.Y)
	if !ok || k.kind != uint8(kNumber) {
		return false
	}
	n := math.Float64frombits(k.bits)
	i := int64(n)
	return float64(i) == n && i >= -128 && i <= 127 && !(n == 0 && math.Signbit(n))
}

// add counts an occurrence of the literal e (anything else is ignored).
func (s *hoistScan) add(e syntax.Expr) {
	k, ok := literalKey(e)
	if !ok {
		return
	}
	w := int64(1) << (2 * min(s.depth, maxHoistDepth))
	for i := range s.cands {
		if s.cands[i].key == k {
			s.cands[i].weight += w
			return
		}
	}
	if len(s.cands) == maxHoistCands {
		return
	}
	if s.cands == nil {
		s.cands = make([]hoistCand, 0, 2)
	}
	s.cands = append(s.cands, hoistCand{key: k, weight: w, order: int32(len(s.cands))})
}

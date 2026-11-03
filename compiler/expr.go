package compiler

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// bindMode distinguishes declaration initialization from assignment.
type bindMode uint8

const (
	bindInit   bindMode = iota // no TDZ check; const bindings may be written
	bindAssign                 // TDZ check; const bindings throw
)

// expr compiles e and leaves its value in register dst.
func (f *funcState) expr(e syntax.Expr, dst int) {
	if c, ok := f.fold(e); ok {
		f.emitConst(c, dst)
		return
	}
	f.setPosNode(e)
	switch e := e.(type) {
	case *syntax.Ident:
		f.loadIdent(e, dst)
	case *syntax.ThisExpr:
		if e.Binding != nil { // a derived constructor's `this`
			f.loadLoc(e.Binding, dst)
			return
		}
		f.emitA(bytecode.LoadThis, dst)
	case *syntax.BigIntLit:
		f.emitABx(bytecode.LoadConst, dst, f.addConst(bytecode.Const{Kind: bytecode.ConstBigInt, Str: e.Digits}))
	case *syntax.RegexLit:
		f.emitABx(bytecode.NewRegExp, dst, f.addConst(bytecode.Const{Kind: bytecode.ConstRegExp, Str: e.Pattern, Flags: e.Flags}))
	case *syntax.TemplateLit:
		f.template(e, dst)
	case *syntax.ArrayLit:
		f.arrayLit(e, dst)
	case *syntax.ObjectLit:
		f.objectLit(e, dst)
	case *syntax.Function:
		f.emitClosure(dst, f.compileFunction(e, ""))
	case *syntax.UnaryExpr:
		f.unary(e, dst)
	case *syntax.UpdateExpr:
		f.update(e, dst, true)
	case *syntax.BinaryExpr:
		f.binary(e, dst, nil)
	case *syntax.LogicalExpr:
		f.expr(e.X, dst)
		end := f.newLabel()
		switch e.Op {
		case syntax.LogOr:
			f.emitJump(bytecode.JmpT, dst, end)
		case syntax.LogAnd:
			f.emitJump(bytecode.JmpF, dst, end)
		default:
			f.emitJump(bytecode.JmpNotNullish, dst, end)
		}
		f.expr(e.Y, dst)
		f.bind(end)
	case *syntax.AssignExpr:
		f.assign(e, dst, true)
	case *syntax.CondExpr:
		alt, end := f.newLabel(), f.newLabel()
		f.condJump(e.Test, alt, false)
		f.expr(e.Cons, dst)
		f.emitJump(bytecode.Jmp, 0, end)
		f.bind(alt)
		f.expr(e.Alt, dst)
		f.bind(end)
	case *syntax.SeqExpr:
		for _, x := range e.Exprs[:len(e.Exprs)-1] {
			f.exprEffect(x)
		}
		f.expr(e.Exprs[len(e.Exprs)-1], dst)
	case *syntax.CallExpr:
		f.call(e, dst)
	case *syntax.NewExpr:
		f.newExpr(e, dst)
	case *syntax.MemberExpr:
		f.member(e, dst)
	case *syntax.OptChain:
		saved := f.optLabel
		f.optLabel = f.newLabel()
		f.expr(e.X, dst)
		done := f.newLabel()
		f.emitJump(bytecode.Jmp, 0, done)
		f.bind(f.optLabel)
		f.emitA(bytecode.LoadUndef, dst)
		f.bind(done)
		f.optLabel = saved
	case *syntax.SpreadElem:
		f.c.fail(e.Pos, "SyntaxError: unexpected spread element")
	case *syntax.Class:
		f.classExpr(e, dst, "", -1)
	case *syntax.NewTarget:
		f.loadLoc(e.Binding, dst)
	case *syntax.ImportMeta:
		f.markScriptOrModule()
		f.setPos(e.Pos)
		f.emitA(bytecode.ImportMeta, dst)
	case *syntax.ImportCall:
		f.importCall(e, dst)
	case *syntax.TaggedTemplate:
		f.taggedTemplate(e, dst)
	case *syntax.YieldExpr:
		f.yield(e, dst)
	case *syntax.AwaitExpr:
		f.awaitExpr(e, dst)
	case *syntax.PrivateName:
		f.loadLoc(e.Binding, dst)
	default:
		pos, _ := e.Range()
		f.c.fail(pos, "internal: unexpected expression node")
	}
}

// exprNamed is expr with NamedEvaluation: an anonymous function value takes
// name.
func (f *funcState) exprNamed(e syntax.Expr, dst int, name string) {
	switch x := e.(type) {
	case *syntax.Function:
		if x.Name == nil {
			f.setPos(x.Pos)
			f.emitClosure(dst, f.compileFunction(x, name))
			return
		}
	case *syntax.Class:
		if x.Name == nil {
			f.classExpr(x, dst, name, -1)
			return
		}
	}
	f.expr(e, dst)
}

// importCall compiles `import(x)` into dst.
func (f *funcState) importCall(e *syntax.ImportCall, dst int) {
	mark := f.nregs
	src := f.exprReg(e.Source)
	f.markScriptOrModule()
	f.setPos(e.Pos)
	f.emitAB(bytecode.ImportCall, dst, src)
	f.free(mark)
}

// markScriptOrModule records that the code uses import() or import.meta,
// which read the record of its script or module at run time.
func (f *funcState) markScriptOrModule() {
	for f.parent != nil {
		f = f.parent
	}
	f.out.ScriptOrModule = true
}

// exprReg compiles e and returns a register holding its value. Un-captured
// locals and the constants a loop hoisted (hoist.go) are returned directly
// (no copy); callers must only read the result.
func (f *funcState) exprReg(e syntax.Expr) int {
	switch x := e.(type) {
	case *syntax.Ident:
		if r := f.regOf(x.Binding); r >= 0 {
			return r
		}
	case *syntax.PrivateName:
		if r := f.regOf(x.Binding); r >= 0 {
			return r
		}
	}
	if r := f.hoistedReg(e); r >= 0 {
		return r
	}
	t := f.alloc()
	f.expr(e, t)
	return t
}

// operand is exprReg for an operand that is consumed only after the
// expressions in after have been evaluated: a local register is aliased only
// when none of them can assign to it.
func (f *funcState) operand(e syntax.Expr, after ...syntax.Expr) int {
	if id, ok := e.(*syntax.Ident); ok {
		if r := f.regOf(id.Binding); r >= 0 {
			safe := true
			for _, a := range after {
				if a != nil && containsAssign(a) {
					safe = false
					break
				}
			}
			if safe {
				return r
			}
		}
	}
	if r := f.hoistedReg(e); r >= 0 {
		return r
	}
	t := f.alloc()
	f.expr(e, t)
	return t
}

// exprEffect compiles e for its side effects only.
func (f *funcState) exprEffect(e syntax.Expr) {
	mark := f.nregs
	defer f.free(mark)
	f.setPosNode(e)
	switch e := e.(type) {
	case *syntax.AssignExpr:
		f.assign(e, -1, false)
	case *syntax.UpdateExpr:
		f.update(e, -1, false)
	case *syntax.SeqExpr:
		for _, x := range e.Exprs {
			f.exprEffect(x)
		}
	case *syntax.LogicalExpr:
		end := f.newLabel()
		switch e.Op {
		case syntax.LogOr:
			f.condJump(e.X, end, true)
		case syntax.LogAnd:
			f.condJump(e.X, end, false)
		default:
			t := f.exprReg(e.X)
			f.emitJump(bytecode.JmpNotNullish, t, end)
		}
		f.exprEffect(e.Y)
		f.bind(end)
	case *syntax.CondExpr:
		alt, end := f.newLabel(), f.newLabel()
		f.condJump(e.Test, alt, false)
		f.exprEffect(e.Cons)
		f.emitJump(bytecode.Jmp, 0, end)
		f.bind(alt)
		f.exprEffect(e.Alt)
		f.bind(end)
	case *syntax.ThisExpr:
		if e.Binding != nil {
			f.expr(e, f.alloc()) // the TDZ check of a derived constructor's `this`
		}
	case *syntax.NumberLit, *syntax.StringLit, *syntax.BoolLit, *syntax.NullLit, *syntax.Function:
		// no effect
	default:
		f.expr(e, f.alloc())
	}
}

// condJump jumps to lbl when ToBoolean(e) == jumpIf.
func (f *funcState) condJump(e syntax.Expr, lbl *label, jumpIf bool) {
	if c, ok := f.fold(e); ok {
		if c.truthy() == jumpIf {
			f.emitJump(bytecode.Jmp, 0, lbl)
		}
		return
	}
	switch x := e.(type) {
	case *syntax.UnaryExpr:
		if x.Op == syntax.Not {
			f.condJump(x.X, lbl, !jumpIf)
			return
		}
	case *syntax.LogicalExpr:
		switch x.Op {
		case syntax.LogAnd:
			if !jumpIf {
				f.condJump(x.X, lbl, false)
				f.condJump(x.Y, lbl, false)
				return
			}
			skip := f.newLabel()
			f.condJump(x.X, skip, false)
			f.condJump(x.Y, lbl, true)
			f.bind(skip)
			return
		case syntax.LogOr:
			if jumpIf {
				f.condJump(x.X, lbl, true)
				f.condJump(x.Y, lbl, true)
				return
			}
			skip := f.newLabel()
			f.condJump(x.X, skip, true)
			f.condJump(x.Y, lbl, false)
			f.bind(skip)
			return
		}
	}
	mark := f.nregs
	if b, ok := compareJump(e); ok {
		// What exprReg and expr do before binary, so that the operands
		// get the same registers and the jump the comparison's position.
		t := f.alloc()
		f.setPosNode(b)
		f.binary(b, t, &condJumpTo{lbl: lbl, jumpIf: jumpIf})
		f.free(mark)
		return
	}
	r := f.exprReg(e)
	if jumpIf {
		f.emitJump(bytecode.JmpT, r, lbl)
	} else {
		f.emitJump(bytecode.JmpF, r, lbl)
	}
	f.free(mark)
}

// emitConst loads a folded constant.
func (f *funcState) emitConst(c constant, dst int) {
	switch c.kind {
	case kNumber:
		if i := int64(c.num); float64(i) == c.num && i >= bytecode.MinSBx && i <= bytecode.MaxSBx && !(c.num == 0 && math.Signbit(c.num)) {
			f.emitWord(bytecode.EncodeAsBx(bytecode.LoadInt, uint8(dst), int16(i)))
			return
		}
		f.emitABx(bytecode.LoadConst, dst, f.numberConst(c.num))
	case kString:
		f.emitABx(bytecode.LoadConst, dst, f.stringConst(c.str))
	case kBool:
		if c.b {
			f.emitA(bytecode.LoadTrue, dst)
		} else {
			f.emitA(bytecode.LoadFalse, dst)
		}
	case kNull:
		f.emitA(bytecode.LoadNull, dst)
	default:
		f.emitA(bytecode.LoadUndef, dst)
	}
}

// --- identifiers -----------------------------------------------------------------

// loadIdent loads the value of the identifier into dst.
func (f *funcState) loadIdent(id *syntax.Ident, dst int) {
	if f.withsFor(id.Binding) != nil {
		mark := f.nregs
		f.get(f.resolveIdent(id), dst)
		f.free(mark)
		return
	}
	f.loadName(id, dst)
}

// loadName loads the identifier's binding, past any with statement.
func (f *funcState) loadName(id *syntax.Ident, dst int) {
	if id.Binding == nil {
		f.getGlobal(id.Name, dst)
		return
	}
	f.loadLoc(id.Binding, dst)
}

// immutable reports whether assigning to b throws a TypeError: const,
// the name of a named function expression, and import bindings (initialized
// indirect bindings, so the assignment never reads the exporter's binding).
func immutable(b *syntax.Binding) bool {
	switch b.Kind {
	case syntax.BindConst, syntax.BindFuncName, syntax.BindImport, syntax.BindImportNS:
		return true
	}
	return false
}

// storeIdent assigns register src to the identifier.
func (f *funcState) storeIdent(id *syntax.Ident, src int, mode bindMode) {
	if f.withsFor(id.Binding) != nil {
		mark := f.nregs
		f.put(f.resolveIdent(id), src, mode)
		f.free(mark)
		return
	}
	f.storeName(id, src, mode)
}

// storeName assigns register src to the identifier's binding, past any with
// statement.
func (f *funcState) storeName(id *syntax.Ident, src int, mode bindMode) {
	b := id.Binding
	if b == nil {
		f.setGlobal(id.Name, src)
		return
	}
	loc := f.c.locs[b]
	if mode == bindAssign {
		if loc.isGlobal() {
			// The engine checks a global lexical binding.
			f.setGlobal(b.Name, src)
			return
		}
		// An uninitialized binding throws a ReferenceError before a const
		// one throws its TypeError (SetMutableBinding).
		if b.NeedsTDZ {
			mark := f.nregs
			f.loadLoc(b, f.alloc()) // performs the TDZ check
			f.free(mark)
		}
		if immutable(b) {
			if b.Kind == syntax.BindFuncName && !f.out.Strict {
				return // a non-strict immutable binding ignores the write
			}
			f.emitNone(bytecode.ThrowConstAssign)
			f.emitExtra(uint32(f.stringConst(b.Name)))
			return
		}
	}
	f.storeLoc(loc, src)
}

// mentions reports whether e references binding b (outside nested functions,
// which cannot reference a register local).
func mentions(e syntax.Expr, b *syntax.Binding) bool {
	if b == nil {
		return false
	}
	found := false
	syntax.Inspect(e, func(n syntax.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *syntax.Ident:
			if n.Binding == b {
				found = true
			}
		case *syntax.Function:
			return false
		}
		return true
	})
	return found
}

// --- operators -------------------------------------------------------------------

var binaryOps = map[syntax.Token]bytecode.Op{
	syntax.Plus: bytecode.Add, syntax.Minus: bytecode.Sub, syntax.Mul: bytecode.Mul, syntax.Div: bytecode.Div,
	syntax.Rem: bytecode.Mod, syntax.Exp: bytecode.Exp, syntax.BitAnd: bytecode.BitAnd, syntax.BitOr: bytecode.BitOr,
	syntax.BitXor: bytecode.BitXor, syntax.Shl: bytecode.Shl, syntax.Shr: bytecode.Shr, syntax.UShr: bytecode.UShr,
	syntax.Eq: bytecode.Eq, syntax.NotEq: bytecode.Ne, syntax.StrictEq: bytecode.StrictEq, syntax.StrictNeq: bytecode.StrictNe,
	syntax.Lt: bytecode.Lt, syntax.Gt: bytecode.Gt, syntax.LtEq: bytecode.Le, syntax.GtEq: bytecode.Ge,
	syntax.KwIn: bytecode.In, syntax.KwInstanceof: bytecode.InstanceOf,
}

var compoundOps = map[syntax.Token]syntax.Token{
	syntax.AddAssign: syntax.Plus, syntax.SubAssign: syntax.Minus, syntax.MulAssign: syntax.Mul, syntax.DivAssign: syntax.Div,
	syntax.RemAssign: syntax.Rem, syntax.ExpAssign: syntax.Exp, syntax.ShlAssign: syntax.Shl, syntax.ShrAssign: syntax.Shr,
	syntax.UShrAssign: syntax.UShr, syntax.AndAssign: syntax.BitAnd, syntax.OrAssign: syntax.BitOr, syntax.XorAssign: syntax.BitXor,
}

// binary compiles a binary operator. The left spine of a left-nested chain
// (a op b op c ...) is compiled iteratively through one accumulator
// register: computing each level's left operand into a fresh temporary held
// one live register per term, so a 250-term string concatenation failed with
// the 256-register limit.
//
// With jmp set, e is a comparison that compareJump accepted: its result
// decides the jump instead of landing in dst.
func (f *funcState) binary(e *syntax.BinaryExpr, dst int, jmp *condJumpTo) {
	mark := f.nregs
	defer f.free(mark)
	if jmp == nil && f.typeofIs(e, dst) {
		return
	}
	x, ok := e.X.(*syntax.BinaryExpr)
	if !ok || !f.chainLink(x) {
		f.binaryOp(e, dst, -1, jmp)
		return
	}
	spine := []*syntax.BinaryExpr{e, x}
	for {
		next, ok := x.X.(*syntax.BinaryExpr)
		if !ok || !f.chainLink(next) {
			break
		}
		x = next
		spine = append(spine, x)
	}
	acc := f.alloc()
	f.binaryOp(spine[len(spine)-1], acc, -1, nil)
	for i := len(spine) - 2; i > 0; i-- {
		f.binaryOp(spine[i], acc, acc, nil)
	}
	f.binaryOp(e, dst, acc, jmp)
}

// chainLink reports whether x, the left operand of a binary node, is compiled
// as a plain binary operation (not a folded constant or a typeof fusion) and
// so can share the chain's accumulator.
func (f *funcState) chainLink(x *syntax.BinaryExpr) bool {
	if _, ok := f.fold(x); ok {
		return false
	}
	if _, ok := binaryOps[x.Op]; !ok {
		return false
	}
	_, _, ok := typeofIsParts(x)
	return !ok
}

// binaryOp emits one binary operation into dst; left is the register already
// holding the left operand, or -1 when e.X is still to be evaluated. With
// jmp set it emits the comparison's jump instead (binary).
func (f *funcState) binaryOp(e *syntax.BinaryExpr, dst, left int, jmp *condJumpTo) {
	mark := f.nregs
	defer f.free(mark)
	if pn, ok := e.X.(*syntax.PrivateName); ok && left < 0 {
		f.privateIn(pn, e.Y, dst)
		return
	}
	if e.Op == syntax.Plus || e.Op == syntax.Minus {
		if c, ok := f.fold(e.Y); ok && c.kind == kNumber {
			n := c.num
			if i := int64(n); float64(i) == n && i >= -128 && i <= 127 && !(n == 0 && math.Signbit(n)) {
				op := bytecode.AddImm
				if e.Op == syntax.Minus {
					op = bytecode.SubImm
				}
				b := left
				if b < 0 {
					b = f.exprReg(e.X)
				}
				f.emitABC(op, dst, b, int(uint8(int8(i))))
				return
			}
		}
	}
	op, ok := binaryOps[e.Op]
	if !ok {
		f.c.fail(e.Pos, "internal: unexpected binary operator "+e.Op.String())
	}
	b := left
	if b < 0 {
		b = f.operand(e.X, e.Y)
	}
	if jmp != nil && f.hoistedReg(e.Y) < 0 {
		// An integer the loop nest does not hold in a register is the
		// jump's X word itself.
		if k, ok := f.fold(e.Y); ok && k.kind == kNumber {
			if i, ok := bytecode.ImmediateOperand(k.num); ok {
				jop, _ := bytecode.CompareJump(op, jmp.jumpIf)
				f.emitJump(jop.Immediate(), b, jmp.lbl)
				f.emitExtra(uint32(i))
				return
			}
		}
	}
	c := f.operand(e.Y)
	if jmp != nil {
		jop, _ := bytecode.CompareJump(op, jmp.jumpIf)
		f.emitJump(jop, b, jmp.lbl)
		f.emitExtra(uint32(c))
		return
	}
	f.emitABC(op, dst, b, c)
}

// condJumpTo is the jump that a comparison compiled by binary ends in.
type condJumpTo struct {
	lbl    *label
	jumpIf bool
}

// compareJump reports whether condJump compiles e as one compare-and-branch
// op: a relational or strict equality comparison that is not a typeof test
// (TypeofIs).
func compareJump(e syntax.Expr) (*syntax.BinaryExpr, bool) {
	b, ok := e.(*syntax.BinaryExpr)
	if !ok {
		return nil, false
	}
	if _, ok := bytecode.CompareJump(binaryOps[b.Op], false); !ok {
		return nil, false
	}
	if _, _, ok := typeofIsParts(b); ok {
		return nil, false
	}
	return b, true
}

// typeofIs fuses `typeof x === "name"` (and ==, !==, !=) into TypeofIs.
func (f *funcState) typeofIs(e *syntax.BinaryExpr, dst int) bool {
	un, idx, ok := typeofIsParts(e)
	if !ok {
		return false
	}
	r := f.typeofOperand(un.X)
	f.emitABC(bytecode.TypeofIs, dst, r, int(idx))
	if e.Op == syntax.StrictNeq || e.Op == syntax.NotEq {
		f.emitAB(bytecode.Not, dst, dst)
	}
	return true
}

// typeofIsParts recognizes the `typeof x === "name"` shape (either operand
// order; ==, !==, != too) and returns the typeof node and the type index.
func typeofIsParts(e *syntax.BinaryExpr) (*syntax.UnaryExpr, uint8, bool) {
	switch e.Op {
	case syntax.StrictEq, syntax.Eq, syntax.StrictNeq, syntax.NotEq:
	default:
		return nil, 0, false
	}
	u, lit := e.X, e.Y
	un, ok := u.(*syntax.UnaryExpr)
	if !ok || un.Op != syntax.KwTypeof {
		un, ok = lit.(*syntax.UnaryExpr)
		if !ok || un.Op != syntax.KwTypeof {
			return nil, 0, false
		}
		lit = u
	}
	s, ok := lit.(*syntax.StringLit)
	if !ok {
		return nil, 0, false
	}
	idx, ok := bytecode.TypeNameIndex(s.Value)
	if !ok {
		return nil, 0, false
	}
	return un, idx, true
}

// typeofOperand evaluates the operand of typeof: unresolvable globals yield
// undefined instead of throwing.
func (f *funcState) typeofOperand(x syntax.Expr) int {
	id, ok := x.(*syntax.Ident)
	if !ok {
		return f.exprReg(x)
	}
	if ws := f.withsFor(id.Binding); ws != nil {
		t, ref := f.alloc(), f.alloc()
		f.withRef(ws, id.Name, ref)
		wl, end := f.newLabel(), f.newLabel()
		f.emitJump(bytecode.JmpNotUndef, ref, wl)
		f.typeofName(id, t)
		f.emitJump(bytecode.Jmp, 0, end)
		f.bind(wl)
		f.withOp(bytecode.WithGet, t, ref, id.Name)
		f.bind(end)
		f.free(t + 1)
		return t
	}
	if id.Binding == nil || f.c.locs[id.Binding].isGlobal() {
		t := f.alloc()
		f.typeofName(id, t)
		return t
	}
	return f.exprReg(x)
}

// typeofName loads the identifier's binding for typeof, past any with
// statement: an unresolvable name yields undefined, and so does a script's
// global var or function whose property is gone (an object environment
// record's HasBinding decides at run time), while a global lexical
// binding in its TDZ still throws.
func (f *funcState) typeofName(id *syntax.Ident, dst int) {
	if b := id.Binding; b != nil && !f.c.locs[b].isGlobal() {
		f.loadName(id, dst)
		return
	}
	f.emitA(bytecode.GetGlobalOrUndef, dst)
	f.emitExtra(bytecode.ExtraArg(f.nameConst(id.Name), f.newIC()))
}

func (f *funcState) unary(e *syntax.UnaryExpr, dst int) {
	mark := f.nregs
	defer f.free(mark)
	switch e.Op {
	case syntax.KwTypeof:
		r := f.typeofOperand(e.X)
		f.emitAB(bytecode.Typeof, dst, r)
	case syntax.KwVoid:
		f.exprEffect(e.X)
		f.emitA(bytecode.LoadUndef, dst)
	case syntax.KwDelete:
		f.deleteExpr(e.X, dst)
	case syntax.Minus:
		f.emitAB(bytecode.Neg, dst, f.exprReg(e.X))
	case syntax.Plus:
		f.emitAB(bytecode.Plus, dst, f.exprReg(e.X))
	case syntax.Not:
		f.emitAB(bytecode.Not, dst, f.exprReg(e.X))
	case syntax.BitNot:
		f.emitAB(bytecode.BitNot, dst, f.exprReg(e.X))
	default:
		f.c.fail(e.Pos, "internal: unexpected unary operator "+e.Op.String())
	}
}

func (f *funcState) deleteExpr(x syntax.Expr, dst int) {
	switch t := x.(type) {
	case *syntax.MemberExpr:
		f.deleteMember(t, dst)
	case *syntax.OptChain:
		m, ok := t.X.(*syntax.MemberExpr)
		if !ok {
			f.exprEffect(x)
			f.emitA(bytecode.LoadTrue, dst)
			return
		}
		saved := f.optLabel
		f.optLabel = f.newLabel()
		f.deleteMember(m, dst)
		done := f.newLabel()
		f.emitJump(bytecode.Jmp, 0, done)
		f.bind(f.optLabel)
		f.emitA(bytecode.LoadTrue, dst)
		f.bind(done)
		f.optLabel = saved
	case *syntax.Ident:
		// Sloppy code only: `delete identifier` is an early error in strict
		// code.
		f.deleteIdent(t, dst)
	default:
		f.exprEffect(x)
		f.emitA(bytecode.LoadTrue, dst)
	}
}

func (f *funcState) deleteMember(m *syntax.MemberExpr, dst int) {
	if isSuper(m) {
		f.superDelete(m)
		return
	}
	obj := f.operand(m.Object, m.Prop)
	if m.Optional {
		f.optShort(obj)
	}
	delProp, delElem := bytecode.DelProp, bytecode.DelElem
	if !f.out.Strict {
		delProp, delElem = bytecode.DelPropSloppy, bytecode.DelElemSloppy
	}
	if name, ok := f.propName(m); ok {
		f.emitAB(delProp, dst, obj)
		f.emitExtra(uint32(f.nameConst(name)))
		return
	}
	key := f.exprReg(m.Prop)
	f.emitABC(delElem, dst, obj, key)
}

// optShort emits the short-circuit test of an optional link.
func (f *funcState) optShort(reg int) {
	if f.optLabel == nil {
		f.c.fail(f.curPos, "internal: optional link outside an optional chain")
	}
	f.emitJump(bytecode.JmpNullish, reg, f.optLabel)
}

// propName returns the static property name of a member expression, when
// it has one that is not an array index.
func (f *funcState) propName(m *syntax.MemberExpr) (string, bool) {
	if !m.Computed {
		switch p := m.Prop.(type) {
		case *syntax.Ident:
			return p.Name, true
		}
		return "", false
	}
	if s, ok := m.Prop.(*syntax.StringLit); ok && !isArrayIndexName(s.Value) {
		return s.Value, true
	}
	return "", false
}

// member compiles a property read.
// member compiles a property read. The object spine of a chain (a.b.c.d)
// is read link by link through one register instead of one temporary per
// link, so its register use does not grow with its length.
func (f *funcState) member(e *syntax.MemberExpr, dst int) {
	mark := f.nregs
	defer f.free(mark)
	inner, ok := e.Object.(*syntax.MemberExpr)
	if !ok {
		if isSuper(e) {
			f.superGet(e, dst)
			return
		}
		f.memberGet(dst, f.operand(e.Object, e.Prop), e)
		return
	}
	spine := []*syntax.MemberExpr{e, inner}
	for {
		next, ok := inner.Object.(*syntax.MemberExpr)
		if !ok {
			break
		}
		inner = next
		spine = append(spine, inner)
	}
	acc := f.alloc()
	if isSuper(inner) {
		f.superGet(inner, acc)
	} else {
		f.memberGet(acc, f.operand(inner.Object, inner.Prop), inner)
	}
	for i := len(spine) - 2; i > 0; i-- {
		f.memberGet(acc, acc, spine[i])
	}
	f.memberGet(dst, acc, e)
}

// memberGet emits the (optionally short-circuiting) read of e from register
// obj into dst, releasing the registers the key needed.
func (f *funcState) memberGet(dst, obj int, e *syntax.MemberExpr) {
	mark := f.nregs
	defer f.free(mark)
	if e.Optional {
		f.optShort(obj)
	}
	f.emitGet(dst, obj, e)
}

// emitGet emits the property read of member expression e from register obj.
// The key's temporary is freed: a call's argument block must start right
// after its callee and this (base+2) even when the key is not a local.
func (f *funcState) emitGet(dst, obj int, e *syntax.MemberExpr) {
	if name, ok := f.propName(e); ok {
		f.emitGetNamed(dst, obj, name)
		return
	}
	mark := f.nregs
	f.emitGetKey(dst, obj, f.exprReg(e.Prop), e)
	f.free(mark)
}

func (f *funcState) emitGetNamed(dst, obj int, name string) {
	if name == "length" {
		f.emitAB(bytecode.GetLen, dst, obj)
		f.emitExtra(bytecode.ExtraArg(0, f.newIC()))
		return
	}
	f.emitAB(bytecode.GetProp, dst, obj)
	f.emitExtra(bytecode.ExtraArg(f.nameConst(name), f.newIC()))
}

// emitSet emits the property write R[obj][key] = R[val] for member e whose
// computed key (if any) is already in register key.
func (f *funcState) emitSet(obj, key, val int, e *syntax.MemberExpr) {
	setProp, setElem := bytecode.SetProp, bytecode.SetElem
	if !f.out.Strict {
		setProp, setElem = bytecode.SetPropSloppy, bytecode.SetElemSloppy
	}
	if name, ok := f.propName(e); ok {
		f.emitAB(setProp, obj, val)
		f.emitExtra(bytecode.ExtraArg(f.nameConst(name), f.newIC()))
		return
	}
	if _, ok := e.Prop.(*syntax.PrivateName); ok {
		f.emitABC(bytecode.SetPrivate, obj, key, val)
		return
	}
	f.emitABC(setElem, obj, key, val)
}

// --- assignment -------------------------------------------------------------------

// assign compiles an assignment; when want is set the value is left in dst.
func (f *funcState) assign(e *syntax.AssignExpr, dst int, want bool) {
	mark := f.nregs
	defer f.free(mark)
	switch target := e.Target.(type) {
	case *syntax.Ident:
		f.assignIdent(e, target, dst, want)
	case *syntax.MemberExpr:
		if isSuper(target) {
			f.superAssign(e, target, dst, want)
			return
		}
		obj := f.operand(target.Object, target.Prop, e.Value)
		key := -1
		if _, ok := f.propName(target); !ok {
			key = f.operand(target.Prop, e.Value)
		}
		if e.Op == syntax.Assign {
			v := dst
			if !want {
				v = f.alloc()
			}
			f.expr(e.Value, v)
			f.emitSet(obj, key, v, target)
			return
		}
		cur := dst
		if !want {
			cur = f.alloc()
		}
		if key >= 0 {
			key = f.emitGetRef(cur, obj, key, mark, target)
		} else {
			name, _ := f.propName(target)
			f.emitGetNamed(cur, obj, name)
		}
		if lbl := f.logicalSkip(e.Op, cur); lbl != nil {
			f.expr(e.Value, cur)
			f.emitSet(obj, key, cur, target)
			f.bind(lbl)
			return
		}
		v := f.operand(e.Value)
		f.emitABC(binaryOps[compoundOps[e.Op]], cur, cur, v)
		f.emitSet(obj, key, cur, target)
	case *syntax.ObjectPattern, *syntax.ArrayPattern:
		v := dst
		if !want {
			v = f.alloc()
		}
		f.expr(e.Value, v)
		f.bindPattern(target, v, bindAssign)
	case *syntax.CallExpr:
		f.callTarget(target, "Invalid left-hand side in assignment")
	default:
		pos, _ := e.Target.Range()
		f.c.fail(pos, "SyntaxError: Invalid left-hand side in assignment")
	}
}

// intoReg reports whether the value e may be compiled straight into the
// register of the variable it is assigned to. A handler of the function
// that catches a throw from the middle of e must not find a partial value
// there (the callee of a call, an array being built), so inside a try
// statement only a value that cannot throw once it has written its
// register may: an identifier (whose loads check before they write), a
// literal or a function. The guards of for-of loops, which only close the
// iterator, are the ones iterCloses counts; those of array patterns outside
// generators make the answer false where it could be true.
func (f *funcState) intoReg(e syntax.Expr) bool {
	if f.guards <= len(f.iterCloses) {
		return true
	}
	switch e := e.(type) {
	case *syntax.Ident, *syntax.NumberLit, *syntax.BigIntLit, *syntax.StringLit, *syntax.BoolLit, *syntax.NullLit:
		return true
	case *syntax.Function:
		return true
	case *syntax.TemplateLit:
		return len(e.Exprs) == 0
	}
	return false
}

// lastOpWrites reports whether e, compiled into the register of a variable
// it reads, still reads every old value it needs: e writes its register
// only with its last instruction, after it evaluated its operands into
// temporaries or read them where they are (a binary operator, a unary
// operator other than delete and void, a property read, or a comma
// expression ending in one of those). Evaluating an operand writes the
// register only for an assignment to the variable inside e, which the
// last instruction then overwrites as it must, and operand copies a
// variable an operand assigns before reading it. Each of those last
// instructions writes its register only when it completes, so a handler
// that catches its throw finds the variable's old value, as one that
// catches a throw from an operand does. A call is not one: its result
// register is fixed by the calling convention (callBase), and so is the
// partial value an update or a logical, conditional or array expression
// leaves.
func lastOpWrites(e syntax.Expr) bool {
	switch e := e.(type) {
	case *syntax.BinaryExpr:
		return true
	case *syntax.UnaryExpr:
		return e.Op != syntax.KwDelete && e.Op != syntax.KwVoid
	case *syntax.MemberExpr:
		return !isSuper(e) && !e.Optional
	case *syntax.SeqExpr:
		return lastOpWrites(e.Exprs[len(e.Exprs)-1])
	}
	return false
}

// logicalSkip emits the short-circuit jump of a logical assignment operator
// and returns its label, or nil for other operators.
func (f *funcState) logicalSkip(op syntax.Token, cur int) *label {
	var jop bytecode.Op
	switch op {
	case syntax.LogOrAssign:
		jop = bytecode.JmpT
	case syntax.LogAndAssign:
		jop = bytecode.JmpF
	case syntax.NullishAssign:
		jop = bytecode.JmpNotNullish
	default:
		return nil
	}
	lbl := f.newLabel()
	f.emitJump(jop, cur, lbl)
	return lbl
}

func (f *funcState) assignIdent(e *syntax.AssignExpr, target *syntax.Ident, dst int, want bool) {
	b := target.Binding
	writable := b == nil || !immutable(b)
	reg := -1
	if writable {
		reg = f.regOf(b)
	}
	name := target.Name
	if e.ParenTarget {
		name = ""
	}
	if e.Op == syntax.Assign {
		if reg >= 0 && (!mentions(e.Value, b) && f.intoReg(e.Value) || lastOpWrites(e.Value)) {
			f.exprNamed(e.Value, reg, name)
			if want {
				f.emitMove(dst, reg)
			}
			return
		}
		// The reference is resolved before the value is evaluated.
		r := f.resolveIdent(target)
		v := dst
		if !want {
			v = f.alloc()
		}
		if f.out.Strict && b == nil && r.ref < 0 && !f.runsNoCode(e.Value) {
			// A value that creates the undeclared global leaves the
			// strict reference unresolvable: PutValue throws.
			ref, ic := f.resolveGlobal(target.Name)
			f.exprNamed(e.Value, v, name)
			f.setGlobalRef(target.Name, v, ref, ic)
			return
		}
		f.exprNamed(e.Value, v, name)
		f.put(r, v, bindAssign)
		return
	}
	// Compound and logical assignment: an arithmetic one writes the register
	// with its one operator.
	if _, arith := compoundOps[e.Op]; reg >= 0 && !containsAssign(e.Value) && (arith || f.intoReg(e.Value)) {
		if lbl := f.logicalSkip(e.Op, reg); lbl != nil {
			f.exprNamed(e.Value, reg, name)
			f.bind(lbl)
		} else {
			v := f.operand(e.Value)
			f.emitABC(binaryOps[compoundOps[e.Op]], reg, reg, v)
		}
		if want {
			f.emitMove(dst, reg)
		}
		return
	}
	r := f.resolveIdent(target)
	cur := dst
	if !want {
		cur = f.alloc()
	}
	f.get(r, cur)
	if lbl := f.logicalSkip(e.Op, cur); lbl != nil {
		f.exprNamed(e.Value, cur, name)
		f.put(r, cur, bindAssign)
		f.bind(lbl)
		return
	}
	v := f.operand(e.Value)
	f.emitABC(binaryOps[compoundOps[e.Op]], cur, cur, v)
	f.put(r, cur, bindAssign)
}

// update compiles ++/--; when want is set the expression value is left in dst.
func (f *funcState) update(e *syntax.UpdateExpr, dst int, want bool) {
	mark := f.nregs
	defer f.free(mark)
	op := bytecode.Inc
	if e.Op == syntax.Dec {
		op = bytecode.Dec
	}
	if !want {
		e = &syntax.UpdateExpr{Span: e.Span, Op: e.Op, Prefix: true, X: e.X}
	}
	switch t := e.X.(type) {
	case *syntax.Ident:
		b := t.Binding
		if b != nil && immutable(b) && (b.Kind != syntax.BindFuncName || f.out.Strict) &&
			f.withsFor(b) == nil {
			// ToNumeric(oldValue) runs (a valueOf is observable) before
			// PutValue throws.
			cur := f.alloc()
			f.loadIdent(t, cur)
			f.emitAB(bytecode.ToNumeric, cur, cur)
			f.emitNone(bytecode.ThrowConstAssign)
			f.emitExtra(uint32(f.stringConst(b.Name)))
			return
		}
		if r := f.regOf(b); r >= 0 && b.Kind != syntax.BindFuncName {
			if e.Prefix {
				f.emitAB(op, r, r)
				if want {
					f.emitMove(dst, r)
				}
				return
			}
			if want {
				f.emitAB(bytecode.ToNumeric, dst, r)
				f.emitAB(op, r, dst)
				return
			}
			f.emitAB(op, r, r)
			return
		}
		ref := f.resolveIdent(t)
		cur := f.alloc()
		f.get(ref, cur)
		if e.Prefix {
			f.emitAB(op, cur, cur)
			f.put(ref, cur, bindAssign)
			if want {
				f.emitMove(dst, cur)
			}
			return
		}
		old := dst
		if !want {
			old = f.alloc()
		}
		f.emitAB(bytecode.ToNumeric, old, cur)
		f.emitAB(op, cur, old)
		f.put(ref, cur, bindAssign)
	case *syntax.MemberExpr:
		if isSuper(t) {
			f.superUpdate(e, t, op, dst, want)
			return
		}
		obj := f.operand(t.Object, t.Prop)
		key := -1
		if _, ok := f.propName(t); !ok {
			key = f.exprReg(t.Prop)
		}
		cur := f.alloc()
		if key >= 0 {
			key = f.emitGetRef(cur, obj, key, mark, t)
		} else {
			name, _ := f.propName(t)
			f.emitGetNamed(cur, obj, name)
		}
		if e.Prefix {
			f.emitAB(op, cur, cur)
			f.emitSet(obj, key, cur, t)
			if want {
				f.emitMove(dst, cur)
			}
			return
		}
		old := dst
		if !want {
			old = f.alloc()
		}
		f.emitAB(bytecode.ToNumeric, old, cur)
		f.emitAB(op, cur, old)
		f.emitSet(obj, key, cur, t)
	case *syntax.CallExpr:
		f.callTarget(t, "Invalid left-hand side expression in update operation")
	default:
		pos, _ := e.X.Range()
		f.c.fail(pos, "SyntaxError: Invalid left-hand side expression in update operation")
	}
}

// --- calls -----------------------------------------------------------------------

// callBase picks the first register of a call block whose result should end
// in dst: dst itself when it is the top temporary, else a fresh block.
func (f *funcState) callBase(dst int) int {
	if dst == f.nregs-1 {
		f.nregs = dst
		return dst
	}
	return f.nregs
}

func (f *funcState) call(e *syntax.CallExpr, dst int) {
	if _, ok := e.Callee.(*syntax.SuperExpr); ok {
		f.superCall(e, dst)
		return
	}
	mark := f.nregs
	defer f.free(mark)
	base := f.callBase(dst)
	f.allocN(2)
	// A plain Call with an undefined this says so in its C operand
	// (CallUndefThis) instead of loading undefined into R[base+1].
	plain := !isDirectEval(e) && !hasSpread(e.Args)
	undefThis := false
	// Callee and this.
	switch callee := e.Callee.(type) {
	case *syntax.MemberExpr:
		if isSuper(callee) {
			f.superCallee(callee, base)
			break
		}
		f.expr(callee.Object, base+1)
		f.setPos(callee.Pos)
		if callee.Optional {
			f.optShort(base + 1)
		}
		f.emitGet(base, base+1, callee)
	case *syntax.OptChain:
		// (a?.b)() still calls with this = a.
		m, ok := callee.X.(*syntax.MemberExpr)
		if !ok {
			f.expr(callee, base)
			undefThis = f.undefThis(plain, base)
			break
		}
		saved := f.optLabel
		f.optLabel = f.newLabel()
		f.expr(m.Object, base+1)
		f.setPos(m.Pos)
		if m.Optional {
			f.optShort(base + 1)
		}
		f.emitGet(base, base+1, m)
		done := f.newLabel()
		f.emitJump(bytecode.Jmp, 0, done)
		f.bind(f.optLabel)
		f.emitA(bytecode.LoadUndef, base)
		f.bind(done)
		f.optLabel = saved
	default:
		if id, ok := callee.(*syntax.Ident); ok && f.withsFor(id.Binding) != nil {
			f.withCallee(id, base)
			break
		}
		f.expr(callee, base)
		undefThis = f.undefThis(plain, base)
	}
	f.setPos(e.Pos)
	if e.Optional {
		f.optShort(base)
	}
	spread := f.args(e.Args, base)
	f.setPos(callPos(e.Callee))
	switch {
	case isDirectEval(e):
		n, c := len(e.Args), 0
		if spread {
			n, c = 0, 1
		}
		// The frame reruns from the next instruction after an eval that
		// grew the register stack, with the pc saved as at a suspension:
		// past the op and its extra word.
		if f.envDepth > genMaxDepth || f.pc()+1 >= genMaxCode {
			f.c.fail(f.curPos, "SyntaxError: function too large for a direct eval"+unsupportedSuffix)
		}
		f.emitABC(bytecode.CallEval, base, n, c)
		f.emitExtra(f.evalSite(e.Pos))
	case spread:
		f.emitA(bytecode.CallSpread, base)
	case undefThis:
		f.emitABC(bytecode.Call, base, len(e.Args), bytecode.CallUndefThis)
	default:
		f.emitAB(bytecode.Call, base, len(e.Args))
	}
	f.emitMove(dst, base)
}

// undefThis gives the call block at base an undefined this: by the Call's
// operand when the call is plain (it reports true), else by loading it.
func (f *funcState) undefThis(plain bool, base int) bool {
	if !plain {
		f.emitA(bytecode.LoadUndef, base+1)
	}
	return plain
}

// hasSpread reports whether an argument list has a spread element.
func hasSpread(list []syntax.Expr) bool {
	for _, a := range list {
		if _, ok := a.(*syntax.SpreadElem); ok {
			return true
		}
	}
	return false
}

// callPos is the source position reported for a call: the property name of
// a method call, else the start of the callee.
func callPos(callee syntax.Expr) int {
	if m, ok := callee.(*syntax.MemberExpr); ok && !m.Computed {
		pos, _ := m.Prop.Range()
		return pos
	}
	pos, _ := callee.Range()
	return pos
}

// args compiles an argument list into the call block starting at base+2 and
// reports whether it was packed into a single spread array.
func (f *funcState) args(list []syntax.Expr, base int) bool {
	if !hasSpread(list) {
		if len(list) > bytecode.MaxRegister {
			f.c.fail(f.curPos, "SyntaxError: too many arguments in one call"+unsupportedSuffix)
		}
		f.allocN(len(list))
		for i, a := range list {
			f.c.pollItem(i)
			f.expr(a, base+2+i)
		}
		return false
	}
	arr := f.alloc()
	f.emitABx(bytecode.NewArray, arr, capHint(len(list)))
	for i, a := range list {
		f.c.pollItem(i)
		mark := f.nregs
		if s, ok := a.(*syntax.SpreadElem); ok {
			t := f.exprReg(s.X)
			f.emitAB(bytecode.AppendSpread, arr, t)
		} else {
			t := f.exprReg(a)
			f.emitAB(bytecode.ArrayPush, arr, t)
		}
		f.free(mark)
	}
	return true
}

func (f *funcState) newExpr(e *syntax.NewExpr, dst int) {
	mark := f.nregs
	defer f.free(mark)
	base := f.callBase(dst)
	f.allocN(2)
	f.expr(e.Callee, base)
	spread := f.args(e.Args, base)
	f.setPos(e.Pos)
	if spread {
		f.emitA(bytecode.NewSpread, base)
	} else {
		f.emitAB(bytecode.New, base, len(e.Args))
	}
	f.emitMove(dst, base)
}

// taggedTemplate compiles tag`...` as a call of the tag with the site's
// template object and the substitution values; a member tag is the this
// value, as in a method call. The callee and this setup mirrors call()
// (optional chains cannot be tags).
func (f *funcState) taggedTemplate(e *syntax.TaggedTemplate, dst int) {
	mark := f.nregs
	defer f.free(mark)
	base := f.callBase(dst)
	f.allocN(2)
	undefThis := false
	switch tag := e.Tag.(type) {
	case *syntax.MemberExpr:
		f.expr(tag.Object, base+1)
		f.setPos(tag.Pos)
		f.emitGet(base, base+1, tag)
	default:
		if id, ok := tag.(*syntax.Ident); ok && f.withsFor(id.Binding) != nil {
			f.withCallee(id, base)
			break
		}
		f.expr(tag, base)
		undefThis = true
	}
	q := e.Quasi
	f.setPos(q.Pos)
	if len(q.Exprs)+1 > bytecode.MaxRegister {
		f.c.fail(f.curPos, "SyntaxError: too many arguments in one call"+unsupportedSuffix)
	}
	k := bytecode.Const{Kind: bytecode.ConstTemplate, Cooked: make([]string, len(q.Quasis)), Raw: make([]string, len(q.Quasis))}
	for i, el := range q.Quasis {
		k.Cooked[i], k.Raw[i] = el.Cooked, el.Raw
		if el.Cooked == syntax.InvalidCooked {
			k.Cooked[i] = bytecode.UndefinedCooked
		}
	}
	f.allocN(1 + len(q.Exprs))
	f.emitA(bytecode.GetTemplate, base+2)
	f.emitExtra(bytecode.ExtraArg(f.addConst(k), f.newIC()))
	for i, x := range q.Exprs {
		f.expr(x, base+3+i)
	}
	f.setPos(callPos(e.Tag))
	if undefThis {
		f.emitABC(bytecode.Call, base, 1+len(q.Exprs), bytecode.CallUndefThis)
	} else {
		f.emitAB(bytecode.Call, base, 1+len(q.Exprs))
	}
	f.emitMove(dst, base)
}

// --- literals --------------------------------------------------------------------

func (f *funcState) template(e *syntax.TemplateLit, dst int) {
	if len(e.Exprs) == 0 {
		f.emitABx(bytecode.LoadConst, dst, f.stringConst(e.Quasis[0].Cooked))
		return
	}
	started := false
	if q := e.Quasis[0].Cooked; q != "" {
		f.emitABx(bytecode.LoadConst, dst, f.stringConst(q))
		started = true
	}
	for i, sub := range e.Exprs {
		mark := f.nregs
		if c, ok := f.fold(sub); ok && c.kind == kString {
			t := f.alloc()
			f.emitABx(bytecode.LoadConst, t, f.stringConst(c.str))
			if started {
				f.emitABC(bytecode.Concat, dst, dst, t)
			} else {
				f.emitMove(dst, t)
				started = true
			}
			f.free(mark)
		} else {
			t := f.exprReg(sub)
			if started {
				s := f.alloc()
				f.emitAB(bytecode.ToStr, s, t)
				f.emitABC(bytecode.Concat, dst, dst, s)
			} else {
				f.emitAB(bytecode.ToStr, dst, t)
				started = true
			}
			f.free(mark)
		}
		if q := e.Quasis[i+1].Cooked; q != "" {
			t := f.alloc()
			f.emitABx(bytecode.LoadConst, t, f.stringConst(q))
			f.emitABC(bytecode.Concat, dst, dst, t)
			f.free(mark)
		}
	}
	if !started {
		f.emitABx(bytecode.LoadConst, dst, f.stringConst(""))
	}
}

func (f *funcState) arrayLit(e *syntax.ArrayLit, dst int) {
	f.emitABx(bytecode.NewArray, dst, capHint(len(e.Elems)))
	for i, el := range e.Elems {
		f.c.pollItem(i)
		mark := f.nregs
		switch el := el.(type) {
		case nil:
			f.emitA(bytecode.ArrayHole, dst)
		case *syntax.SpreadElem:
			t := f.exprReg(el.X)
			f.emitAB(bytecode.AppendSpread, dst, t)
		default:
			t := f.exprReg(el)
			f.emitAB(bytecode.ArrayPush, dst, t)
		}
		f.free(mark)
	}
}

// literalKeyName returns the string form of a non-computed property key.
func (f *funcState) literalKeyName(key syntax.Expr) string {
	switch k := key.(type) {
	case *syntax.Ident:
		return k.Name
	case *syntax.StringLit:
		return k.Value
	case *syntax.NumberLit:
		return numberKeyString(k.Value)
	case *syntax.BigIntLit:
		if hex, ok := strings.CutPrefix(k.Digits, "0x"); ok {
			v, _ := new(big.Int).SetString(hex, 16)
			return v.String()
		}
		return k.Digits
	case *syntax.PrivateName:
		return "#" + k.Name
	}
	pos, _ := key.Range()
	f.c.fail(pos, "internal: unexpected property key")
	return ""
}

// defineNamed emits the definition of a static key on object obj.
func (f *funcState) defineNamed(obj int, name string, val int) {
	if isArrayIndexName(name) {
		k := f.alloc()
		f.emitABx(bytecode.LoadConst, k, f.stringConst(name))
		f.emitABC(bytecode.DefineElem, obj, k, val)
		return
	}
	f.emitAB(bytecode.DefineField, obj, val)
	f.emitExtra(bytecode.ExtraArg(f.nameConst(name), f.newIC()))
}

func (f *funcState) objectLit(e *syntax.ObjectLit, dst int) {
	f.emitABx(bytecode.NewObject, dst, capHint(len(e.Props)))
	for i, p := range e.Props {
		f.c.pollItem(i)
		mark := f.nregs
		f.setPos(p.Pos)
		switch p.Kind {
		case syntax.PropSpread:
			t := f.exprReg(p.Value)
			f.emitAB(bytecode.CopyDataProps, dst, t)
		case syntax.PropInit, syntax.PropShorthand, syntax.PropMethod:
			if p.Computed {
				k := f.alloc()
				f.expr(p.Key, k)
				v := f.alloc()
				if fn, ok := p.Value.(*syntax.Function); ok && (p.Kind == syntax.PropMethod || fn.Name == nil) {
					// The function is named after the evaluated key at run time.
					f.emitClosure(v, f.compileFunction(fn, ""))
					f.setHome(fn, v, dst)
					f.emitABC(bytecode.DefineMethod, dst, k, v)
					break
				}
				if !isInertValue(p.Value) {
					// The value's evaluation may run code: ToPropertyKey first.
					f.emitAB(bytecode.ToPropertyKey, k, k)
				}
				if c, ok := p.Value.(*syntax.Class); ok && c.Name == nil {
					f.classExpr(c, v, "", k) // named after the key at run time
				} else {
					f.expr(p.Value, v)
				}
				f.emitABC(bytecode.DefineElem, dst, k, v)
				break
			}
			name := f.literalKeyName(p.Key)
			v := f.alloc()
			switch {
			case p.Kind == syntax.PropMethod:
				fn := p.Value.(*syntax.Function)
				f.emitClosure(v, f.compileFunction(fn, name))
				f.setHome(fn, v, dst)
			case p.Kind == syntax.PropInit && name == "__proto__":
				f.expr(p.Value, v) // not a NamedEvaluation
			default:
				f.exprNamed(p.Value, v, name)
			}
			if p.Kind == syntax.PropInit && name == "__proto__" {
				f.emitAB(bytecode.SetProto, dst, v)
				break
			}
			f.defineNamed(dst, name, v)
		case syntax.PropGetter, syntax.PropSetter:
			f.accessor(dst, p.Key, p.Computed, p.Kind == syntax.PropSetter, p.Value.(*syntax.Function), bytecode.AccessorEnumerable)
		case syntax.PropCoverInit:
			f.c.fail(p.Pos, "SyntaxError: Invalid shorthand property initializer")
		}
		f.free(mark)
	}
}

// runsNoCode reports whether evaluating e runs no code: an inert value or
// a variable in a register.
func (f *funcState) runsNoCode(e syntax.Expr) bool {
	if id, ok := e.(*syntax.Ident); ok {
		return f.regOf(id.Binding) >= 0
	}
	return isInertValue(e)
}

// isInertValue reports whether evaluating e runs no code and so cannot
// observe when a computed key before it is converted.
func isInertValue(e syntax.Expr) bool {
	switch e.(type) {
	case *syntax.NumberLit, *syntax.StringLit, *syntax.BoolLit, *syntax.NullLit, *syntax.BigIntLit, *syntax.RegexLit, *syntax.Function:
		return true
	}
	return false
}

// accessor emits the definition of getter or setter fn with key on object
// obj, its home object. A static key names the function at compile time, a
// computed one at run time.
func (f *funcState) accessor(obj int, key syntax.Expr, computed, setter bool, fn *syntax.Function, flags uint32) {
	prefix := "get "
	if setter {
		prefix, flags = "set ", flags|bytecode.AccessorSetter
	}
	k := f.alloc()
	name := ""
	if computed {
		f.expr(key, k)
		flags |= bytecode.AccessorNameKey
	} else {
		lit := f.literalKeyName(key)
		f.emitABx(bytecode.LoadConst, k, f.stringConst(lit))
		name = prefix + lit
	}
	v := f.alloc()
	f.emitClosure(v, f.compileFunction(fn, name))
	f.setHome(fn, v, obj)
	f.emitABC(bytecode.DefineAccessor, obj, k, v)
	f.emitExtra(flags)
}

// numberKeyString renders a numeric literal key as ToString would
// (Number::toString, ECMA-262 §6.1.6.1.20).
func numberKeyString(v float64) string {
	switch {
	case v != v:
		return "NaN"
	case v == 0:
		return "0"
	case math.IsInf(v, 0):
		if v < 0 {
			return "-Infinity"
		}
		return "Infinity"
	case v < 0:
		return "-" + numberKeyString(-v)
	}
	// The shortest round-tripping digits d1..dk and n, with v = 0.d1..dk × 10^n.
	mant, exp, _ := strings.Cut(strconv.FormatFloat(v, 'e', -1, 64), "e")
	digits := strings.Replace(mant, ".", "", 1)
	e, _ := strconv.Atoi(exp)
	k, n := len(digits), e+1
	switch {
	case k <= n && n <= 21:
		return digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits
	}
	if k > 1 {
		digits = digits[:1] + "." + digits[1:]
	}
	if e < 0 {
		return digits + "e-" + strconv.Itoa(-e)
	}
	return digits + "e+" + strconv.Itoa(e)
}

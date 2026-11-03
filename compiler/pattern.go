package compiler

import (
	"strconv"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// bindPattern stores the value in register src into pat: an identifier, a
// member expression (assignment only), or a destructuring pattern.
func (f *funcState) bindPattern(pat syntax.Pattern, src int, mode bindMode) {
	switch p := pat.(type) {
	case *syntax.Ident:
		f.storeIdent(p, src, mode)
	case *syntax.MemberExpr:
		mark := f.nregs
		if isSuper(p) {
			b := f.allocN(2)
			f.emitABC(f.setSuperOp(), b, f.superRef(p, b), src)
			f.free(mark)
			return
		}
		obj := f.operand(p.Object, p.Prop)
		key := -1
		if _, ok := f.propName(p); !ok {
			key = f.exprReg(p.Prop)
		}
		f.emitSet(obj, key, src, p)
		f.free(mark)
	case *syntax.AssignPattern:
		skip := f.newLabel()
		f.emitJump(bytecode.JmpNotUndef, src, skip)
		name := ""
		if id, ok := p.Target.(*syntax.Ident); ok {
			name = id.Name
		}
		f.exprNamed(p.Default, src, name)
		f.bind(skip)
		f.bindPattern(p.Target, src, mode)
	case *syntax.ObjectPattern:
		f.objectPattern(p, src, mode)
	case *syntax.ArrayPattern:
		f.arrayPattern(p, src, mode)
	default:
		pos, _ := pat.Range()
		f.c.fail(pos, "SyntaxError: Invalid destructuring assignment target")
	}
}

func (f *funcState) objectPattern(p *syntax.ObjectPattern, src int, mode bindMode) {
	mark := f.nregs
	defer f.free(mark)
	if len(p.Props) == 0 {
		// No property read would throw for null/undefined, but
		// RequireObjectCoercible(value) still applies (`{} = null`,
		// `{...rest} = null`).
		f.emitA(bytecode.RequireObjectCoercible, src)
	}
	excl := -1
	if p.Rest != nil {
		excl = f.alloc()
		f.emitABx(bytecode.NewArray, excl, capHint(len(p.Props)))
	}
	for _, prop := range p.Props {
		m := f.nregs
		f.setPos(prop.Pos)
		v := f.alloc()
		// Order per KeyedDestructuringAssignmentEvaluation: the property
		// name, the target reference, then the source read.
		k, name := -1, ""
		if prop.Computed {
			k = f.alloc()
			f.expr(prop.Key, k)
			if f.isRefTarget(prop.Value) {
				// The target's evaluation may run code: ToPropertyKey first.
				f.emitAB(bytecode.ToPropertyKey, k, k)
			}
		} else if name = f.literalKeyName(prop.Key); isArrayIndexName(name) || excl >= 0 {
			k = f.alloc()
			f.emitABx(bytecode.LoadConst, k, f.stringConst(name))
		}
		target, isRef := f.prepareTarget(prop.Value)
		if k >= 0 && (prop.Computed || isArrayIndexName(name)) {
			f.emitABC(bytecode.GetElem, v, src, k)
		} else {
			f.emitGetNamed(v, src, name)
		}
		if excl >= 0 {
			f.emitAB(bytecode.ArrayPush, excl, k)
		}
		if isRef {
			f.storeTarget(target, v, mode)
		} else {
			f.bindPattern(prop.Value, v, mode)
		}
		f.free(m)
	}
	if p.Rest != nil {
		target, isRef := f.prepareTarget(p.Rest)
		r := f.alloc()
		f.emitABx(bytecode.NewObject, r, 0)
		f.emitABC(bytecode.CopyDataPropsEx, r, src, excl)
		if isRef {
			f.storeTarget(target, r, mode)
		} else {
			f.bindPattern(p.Rest, r, mode)
		}
	}
}

func (f *funcState) arrayPattern(p *syntax.ArrayPattern, src int, mode bindMode) {
	mark := f.nregs
	defer f.free(mark)
	it := f.allocN(3)
	f.emitAB(bytecode.IterInit, it, src)
	// An abrupt completion of an element closes the iterator unless the
	// iterator itself failed (IterValue then marks it done); a normal one
	// closes it unless it was exhausted; so does a return resuming a
	// generator suspended in an element (emitReturn), the only return that
	// can leave a pattern. The elements are a guard, as a loop body is: the
	// closes of that return skip the rows of the guards inside it.
	start, depth := f.pc(), f.envDepth
	g := f.openGuard()
	if f.out.Generator {
		f.iterCloses = append(f.iterCloses, openIter{reg: it, finallyDepth: len(f.finallys), guard: g.level})
	}
	for _, el := range p.Elems {
		if el == nil {
			f.emitAB(bytecode.IterValue, it, it+2)
			continue
		}
		m := f.nregs
		f.setPosNode(el)
		// IteratorDestructuringAssignmentEvaluation: the target reference
		// is evaluated before the iterator is stepped.
		target, isRef := f.prepareTarget(el)
		f.emitAB(bytecode.IterValue, it, it+2)
		if isRef {
			f.storeTarget(target, it+2, mode)
		} else {
			f.bindPattern(el, it+2, mode)
		}
		f.free(m)
	}
	if p.Rest != nil {
		target, isRef := f.prepareTarget(p.Rest)
		r := f.alloc()
		f.emitAB(bytecode.IterRest, it, r)
		if isRef {
			f.storeTarget(target, r, mode)
		} else {
			f.bindPattern(p.Rest, r, mode)
		}
	}
	if f.out.Generator {
		f.iterCloses = f.iterCloses[:len(f.iterCloses)-1]
	}
	end := f.pc()
	f.emitWord(bytecode.EncodeAsBx(bytecode.IterClose, uint8(it), 1))
	f.addHandler(g, bytecode.Handler{
		Start: uint32(start), End: uint32(end), Handler: uint32(f.pc()),
		StackDepth: uint16(depth), Kind: bytecode.HandlerCatch, Reg: uint16(it + 2),
	})
	f.closeGuard()
	f.emitAB(bytecode.IterThrow, it, it+2)
}

// memberTarget is a destructuring target whose reference has been
// evaluated ahead of the source read, as the spec's destructuring does: a
// member expression (object and computed key), or an identifier a with
// statement may intercept (id, m nil); def is its default, if any.
type memberTarget struct {
	m        *syntax.MemberExpr
	obj, key int
	id       identRef
	def      syntax.Expr
}

// isRefTarget reports whether prepareTarget evaluates the reference of
// target pat ahead of the source read.
func (f *funcState) isRefTarget(pat syntax.Pattern) bool {
	if ap, ok := pat.(*syntax.AssignPattern); ok {
		pat = ap.Target
	}
	switch p := pat.(type) {
	case *syntax.MemberExpr:
		return true
	case *syntax.Ident:
		return f.withsFor(p.Binding) != nil
	}
	return false
}

// prepareTarget evaluates the reference of a member-expression target, or
// resolves an identifier target through the enclosing with statements (with
// or without a default), into fresh registers: the source read that follows
// may run user code that reassigns the locals the target names, so nothing
// is aliased. ok is false for every other target, which bindPattern stores
// after the read.
func (f *funcState) prepareTarget(pat syntax.Pattern) (memberTarget, bool) {
	var t memberTarget
	if !f.isRefTarget(pat) {
		return t, false
	}
	if ap, ok := pat.(*syntax.AssignPattern); ok {
		t.def = ap.Default
		pat = ap.Target
	}
	m, ok := pat.(*syntax.MemberExpr)
	if !ok {
		t.id = f.resolveIdent(pat.(*syntax.Ident))
		return t, true
	}
	t.m = m
	if isSuper(m) {
		t.obj = f.allocN(2)
		t.key = f.superRef(m, t.obj)
		return t, true
	}
	t.obj = f.alloc()
	f.expr(m.Object, t.obj)
	t.key = -1
	if _, ok := f.propName(m); !ok {
		t.key = f.alloc()
		f.expr(m.Prop, t.key)
	}
	return t, true
}

// storeTarget applies the default and stores register v through a prepared
// target.
func (f *funcState) storeTarget(t memberTarget, v int, mode bindMode) {
	if t.def != nil {
		skip := f.newLabel()
		f.emitJump(bytecode.JmpNotUndef, v, skip)
		if t.m == nil {
			f.exprNamed(t.def, v, t.id.id.Name)
		} else {
			f.expr(t.def, v)
		}
		f.bind(skip)
	}
	if t.m == nil {
		f.put(t.id, v, mode)
		return
	}
	if isSuper(t.m) {
		f.emitABC(f.setSuperOp(), t.obj, t.key, v)
		return
	}
	f.emitSet(t.obj, t.key, v, t.m)
}

func formatInt(i int64) string { return strconv.FormatInt(i, 10) }

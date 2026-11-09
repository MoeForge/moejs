package compiler

import "github.com/MoeForge/moejs/syntax"

// varFlow walks a function's body in evaluation order for writtenFirst,
// carrying the set of candidate vars that every path to the current point
// has written (definite assignment). A read of a candidate outside the set
// makes the UndefRange clear it; a candidate that every read finds in the
// set is left out.
//
// The walk is a single pass. Where paths join it keeps what all of them
// wrote: an if's branches; a conditional expression's arms. A path that
// cannot go on (after a return, a throw, a break or a continue) holds
// every candidate, so it adds nothing to a join. Code that may not run (a
// loop's body, an arm of && || ?? or of an optional chain, a switch's
// cases, a catch or finally block) is walked on a copy of the set it may
// start from, and what it writes is dropped, as is what runs before a
// break that leaves a labelled statement or a switch. A read in a loop
// body is checked against the set of the first iteration, which every
// later one contains. What the walk does not model (patterns, with
// statements, classes) counts every reference as a read and writes
// nothing.
type varFlow struct {
	s    *syntax.Scope
	st   []uint8 // writtenFirst's states by slot
	idx  []int32 // by slot: the candidate's bit, or -1
	nw   int     // words per set
	free [][]uint64
}

// newVarFlow returns the walk of s's candidates, the slots st holds as
// varUnseen.
func newVarFlow(s *syntax.Scope, st []uint8) *varFlow {
	v := &varFlow{s: s, st: st, idx: make([]int32, len(st))}
	n := int32(0)
	for i, x := range st {
		v.idx[i] = -1
		if x == varUnseen {
			v.idx[i] = n
			n++
		}
	}
	v.nw = int(n+63) / 64
	return v
}

func (v *varFlow) set() []uint64 {
	if k := len(v.free); k > 0 {
		s := v.free[k-1]
		v.free = v.free[:k-1]
		return s
	}
	return make([]uint64, v.nw)
}

// copyOf returns a new set holding what s does.
func (v *varFlow) copyOf(s []uint64) []uint64 {
	t := v.set()
	copy(t, s)
	return t
}

func (v *varFlow) release(s []uint64) { v.free = append(v.free, s) }

// unreachable marks s as the set of a path that cannot go on.
func unreachable(s []uint64) {
	for i := range s {
		s[i] = ^uint64(0)
	}
}

// candidate returns the bit of the var id references, or -1.
func (v *varFlow) candidate(id *syntax.Ident) int32 {
	if b := id.Binding; b != nil && b.Scope == v.s {
		return v.idx[b.Slot]
	}
	return -1
}

func (v *varFlow) read(id *syntax.Ident, s []uint64) {
	if i := v.candidate(id); i >= 0 && s[i>>6]&(1<<(i&63)) == 0 {
		v.st[id.Binding.Slot] = varClear
	}
}

func (v *varFlow) write(id *syntax.Ident, s []uint64) {
	if i := v.candidate(id); i >= 0 {
		s[i>>6] |= 1 << (i & 63)
		if v.st[id.Binding.Slot] == varUnseen {
			v.st[id.Binding.Slot] = varWritten
		}
	}
}

// refs counts every reference in n as a read.
func (v *varFlow) refs(n syntax.Node, s []uint64) {
	syntax.Inspect(n, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Ident:
			v.read(n, s)
		case *syntax.Function:
			return false // its references made the binding captured
		}
		return true
	})
}

// maybe walks e, which may not run, on a copy of s.
func (v *varFlow) maybe(e syntax.Expr, s []uint64) {
	t := v.copyOf(s)
	v.expr(e, t)
	v.release(t)
}

// maybeStmt walks n, which may not run or may be left by a break, on a
// copy of s.
func (v *varFlow) maybeStmt(n syntax.Stmt, s []uint64) {
	t := v.copyOf(s)
	v.stmt(n, t)
	v.release(t)
}

func (v *varFlow) expr(e syntax.Expr, s []uint64) {
	switch e := e.(type) {
	case nil, *syntax.NumberLit, *syntax.StringLit, *syntax.BoolLit, *syntax.NullLit, *syntax.BigIntLit, *syntax.ThisExpr:
	case *syntax.Ident:
		v.read(e, s)
	case *syntax.AssignExpr:
		switch t := e.Target.(type) {
		case *syntax.Ident:
			switch e.Op {
			case syntax.Assign:
				v.expr(e.Value, s)
				v.write(t, s)
			case syntax.LogAndAssign, syntax.LogOrAssign, syntax.NullishAssign:
				v.read(t, s)
				v.maybe(e.Value, s)
			default:
				v.read(t, s)
				v.expr(e.Value, s)
			}
		case *syntax.MemberExpr:
			v.expr(t.Object, s)
			if t.Computed {
				v.expr(t.Prop, s)
			}
			switch e.Op {
			case syntax.LogAndAssign, syntax.LogOrAssign, syntax.NullishAssign:
				v.maybe(e.Value, s)
			default:
				v.expr(e.Value, s)
			}
		default:
			v.refs(e, s)
		}
	case *syntax.BinaryExpr:
		v.expr(e.X, s)
		v.expr(e.Y, s)
	case *syntax.LogicalExpr:
		v.expr(e.X, s)
		v.maybe(e.Y, s)
	case *syntax.CondExpr:
		v.expr(e.Test, s)
		t := v.copyOf(s)
		v.expr(e.Cons, t)
		v.expr(e.Alt, s)
		for i := range s {
			s[i] &= t[i]
		}
		v.release(t)
	case *syntax.SeqExpr:
		for _, x := range e.Exprs {
			v.expr(x, s)
		}
	case *syntax.UnaryExpr:
		v.expr(e.X, s)
	case *syntax.UpdateExpr:
		v.expr(e.X, s)
	case *syntax.CallExpr:
		v.expr(e.Callee, s)
		for _, x := range e.Args {
			v.expr(x, s)
		}
	case *syntax.NewExpr:
		v.expr(e.Callee, s)
		for _, x := range e.Args {
			v.expr(x, s)
		}
	case *syntax.MemberExpr:
		v.expr(e.Object, s)
		if e.Computed {
			v.expr(e.Prop, s)
		}
	case *syntax.OptChain:
		v.maybe(e.X, s)
	case *syntax.SpreadElem:
		v.expr(e.X, s)
	case *syntax.ArrayLit:
		for _, x := range e.Elems {
			v.expr(x, s)
		}
	case *syntax.ObjectLit:
		for _, p := range e.Props {
			if p.Computed {
				v.expr(p.Key, s)
			}
			v.expr(p.Value, s)
		}
	case *syntax.TemplateLit:
		for _, x := range e.Exprs {
			v.expr(x, s)
		}
	case *syntax.TaggedTemplate:
		v.expr(e.Tag, s)
		v.expr(e.Quasi, s)
	default:
		v.refs(e, s)
	}
}

func (v *varFlow) stmts(list []syntax.Stmt, s []uint64) {
	for _, n := range list {
		v.stmt(n, s)
	}
}

func (v *varFlow) decl(d *syntax.VarDecl, s []uint64) {
	for _, x := range d.Decls {
		id, ok := x.Target.(*syntax.Ident)
		if !ok {
			v.expr(x.Init, s)
			v.refs(x.Target, s)
			continue
		}
		// `var x;` does nothing; a lexical binding is not a candidate.
		if x.Init != nil && d.Kind == syntax.DeclVar {
			v.expr(x.Init, s)
			v.write(id, s)
		} else {
			v.expr(x.Init, s)
		}
	}
}

func (v *varFlow) stmt(n syntax.Stmt, s []uint64) {
	switch n := n.(type) {
	case nil, *syntax.EmptyStmt, *syntax.DebuggerStmt, *syntax.FuncDecl:
	case *syntax.ExprStmt:
		v.expr(n.X, s)
	case *syntax.VarDecl:
		v.decl(n, s)
	case *syntax.BlockStmt:
		v.stmts(n.Body, s)
	case *syntax.IfStmt:
		v.expr(n.Cond, s)
		t := v.copyOf(s)
		v.stmt(n.Then, s)
		v.stmt(n.Else, t)
		for i := range s {
			s[i] &= t[i]
		}
		v.release(t)
	case *syntax.ForStmt:
		// The head's initialization and first test run once, before the
		// body; the update runs after the body or a continue.
		switch x := n.Init.(type) {
		case *syntax.VarDecl:
			v.decl(x, s)
		case syntax.Expr:
			v.expr(x, s)
		}
		v.expr(n.Cond, s)
		v.maybeStmt(n.Body, s)
		v.maybe(n.Update, s)
	case *syntax.WhileStmt:
		v.expr(n.Cond, s)
		v.maybeStmt(n.Body, s)
	case *syntax.DoWhileStmt:
		v.maybeStmt(n.Body, s)
		v.maybe(n.Cond, s)
	case *syntax.ForInOfStmt:
		d, decl := n.Left.(*syntax.VarDecl)
		if decl && d.Decls[0].Init != nil {
			v.refs(d, s) // an Annex B initializer, which runs first
		}
		v.expr(n.Right, s)
		t := v.copyOf(s)
		switch x := n.Left.(type) {
		case *syntax.Ident:
			v.write(x, t)
		case *syntax.VarDecl:
			if id, ok := x.Decls[0].Target.(*syntax.Ident); ok && x.Decls[0].Init == nil {
				if x.Kind == syntax.DeclVar {
					v.write(id, t)
				}
			} else {
				v.refs(x, t)
			}
		default:
			v.refs(n.Left, t)
		}
		v.stmt(n.Body, t)
		v.release(t)
	case *syntax.ReturnStmt:
		v.expr(n.Result, s)
		unreachable(s)
	case *syntax.ThrowStmt:
		v.expr(n.X, s)
		unreachable(s)
	case *syntax.BreakStmt, *syntax.ContinueStmt:
		unreachable(s)
	case *syntax.LabeledStmt:
		body := n.Body
		for {
			l, ok := body.(*syntax.LabeledStmt)
			if !ok {
				break
			}
			body = l.Body
		}
		switch body.(type) {
		case *syntax.ForStmt, *syntax.ForInOfStmt, *syntax.WhileStmt, *syntax.DoWhileStmt:
			v.stmt(body, s) // a break leaves it as it leaves an unlabelled loop
		default:
			v.maybeStmt(body, s)
		}
	case *syntax.SwitchStmt:
		// A case is entered by its test or from the case before it, whose
		// test may not have run.
		v.expr(n.Disc, s)
		t := v.set()
		for _, c := range n.Cases {
			copy(t, s)
			v.expr(c.Test, t)
			copy(t, s)
			v.stmts(c.Body, t)
		}
		v.release(t)
	case *syntax.TryStmt:
		// A throw reaches the catch block from any point of the try block,
		// and the finally block also runs after a return, a break or a
		// throw; after the statement, what the try block or the catch
		// block wrote holds, and what the finally block wrote.
		s0 := v.copyOf(s)
		v.stmt(n.Block, s)
		if n.Handler != nil {
			t := v.copyOf(s0)
			if n.Param != nil {
				v.refs(n.Param, t)
			}
			v.stmt(n.Handler, t)
			for i := range s {
				s[i] &= t[i]
			}
			v.release(t)
		}
		if n.Finalizer != nil {
			v.stmt(n.Finalizer, s0)
			for i := range s {
				s[i] |= s0[i]
			}
		}
		v.release(s0)
	case *syntax.WithStmt:
		// An assignment in its body may write the object's property.
		v.expr(n.Object, s)
		v.refs(n.Body, s)
	default:
		v.refs(n, s)
	}
}

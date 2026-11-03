package compiler

import (
	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// stmts compiles a statement list, dropping code after an abrupt completion.
func (f *funcState) stmts(list []syntax.Stmt) {
	for i, s := range list {
		f.c.poll(i)
		f.stmt(s)
		switch s.(type) {
		case *syntax.ReturnStmt, *syntax.ThrowStmt, *syntax.BreakStmt, *syntax.ContinueStmt:
			return
		}
	}
}

// clearCompletion sets a script's completion value to undefined where a
// statement's completion is UpdateEmpty(C, undefined) (if, the loops,
// switch, try, catch and with: 14.6.2, 14.7, 14.12.4, 14.15.3, 14.11.2),
// so that a body with no value of its own yields undefined.
func (f *funcState) clearCompletion() {
	if f.completion >= 0 {
		f.emitA(bytecode.LoadUndef, f.completion)
	}
}

func (f *funcState) stmt(s syntax.Stmt) {
	f.setPosNode(s)
	switch s.(type) {
	case *syntax.IfStmt, *syntax.ForStmt, *syntax.ForInOfStmt, *syntax.WhileStmt, *syntax.DoWhileStmt, *syntax.SwitchStmt, *syntax.TryStmt:
		f.clearCompletion()
	}
	switch s := s.(type) {
	case *syntax.VarDecl:
		f.varDecl(s)
	case *syntax.FuncDecl:
		// Instantiated at scope entry (Scope.Funcs).
		f.annexBCopy(s)
	case *syntax.ClassDecl:
		f.classDecl(s.Class, s.Class.Name.Binding, "")
	case *syntax.ExprStmt:
		if f.completion >= 0 {
			f.expr(s.X, f.completion)
			return
		}
		f.exprEffect(s.X)
	case *syntax.BlockStmt:
		f.block(s)
	case *syntax.EmptyStmt, *syntax.DebuggerStmt:
	case *syntax.IfStmt:
		els := f.newLabel()
		f.condJump(s.Cond, els, false)
		f.stmt(s.Then)
		if s.Else == nil {
			f.bind(els)
			return
		}
		end := f.newLabel()
		f.emitJump(bytecode.Jmp, 0, end)
		f.bind(els)
		f.stmt(s.Else)
		f.bind(end)
	case *syntax.ForStmt:
		f.forStmt(s)
	case *syntax.ForInOfStmt:
		f.forInOf(s)
	case *syntax.WhileStmt:
		f.whileStmt(s)
	case *syntax.DoWhileStmt:
		f.doWhile(s)
	case *syntax.ReturnStmt:
		mark := f.nregs
		if s.Result != nil {
			f.returnExpr(s.Result)
		} else if len(f.finallys) > 0 {
			t := f.alloc()
			f.emitA(bytecode.LoadUndef, t)
			f.emitReturn(t)
		} else {
			f.emitRetUndef()
		}
		f.free(mark)
	case *syntax.ThrowStmt:
		mark := f.nregs
		r := f.exprReg(s.X)
		f.setPos(s.Pos)
		f.emitA(bytecode.Throw, r)
		f.free(mark)
	case *syntax.BreakStmt:
		f.emitBreak(f.findTarget(s, s.Label, false), false)
	case *syntax.ContinueStmt:
		f.emitBreak(f.findTarget(s, s.Label, true), true)
	case *syntax.LabeledStmt:
		f.labeled(s)
	case *syntax.SwitchStmt:
		f.switchStmt(s)
	case *syntax.TryStmt:
		f.tryStmt(s)
	case *syntax.WithStmt:
		f.withStmt(s)
	case *syntax.ExportDecl:
		f.stmt(s.Decl)
	case *syntax.ExportNamed, *syntax.ExportAll, *syntax.ImportDecl:
		// bound when the module graph is linked and instantiated
	case *syntax.ExportDefault:
		f.exportDefault(s)
	default:
		pos, _ := s.Range()
		f.c.fail(pos, "internal: unexpected statement node")
	}
}

func (f *funcState) block(b *syntax.BlockStmt) {
	es, mark := f.enterScope(b.Scope)
	f.stmts(b.Body)
	f.leaveScope(es, mark)
}

func (f *funcState) exportDefault(s *syntax.ExportDefault) {
	switch d := s.Decl.(type) {
	case *syntax.FuncDecl:
		// hoisted
	case *syntax.ClassDecl:
		if d.Class.Name != nil {
			f.classDecl(d.Class, d.Class.Name.Binding, "")
			return
		}
		f.classDecl(d.Class, f.ownEnv.scope.Lookup("*default*"), "default")
	case syntax.Expr:
		b := f.ownEnv.scope.Lookup("*default*")
		if b == nil {
			f.c.fail(s.Pos, "internal: missing *default* binding")
		}
		mark := f.nregs
		t := f.alloc()
		f.exprNamed(d, t, "default")
		f.storeLoc(f.c.locs[b], t)
		f.free(mark)
	}
}

// varDecl compiles var/let/const declarators.
func (f *funcState) varDecl(d *syntax.VarDecl) {
	for _, decl := range d.Decls {
		f.setPos(decl.Pos)
		mark := f.nregs
		if decl.Init == nil {
			if d.Kind != syntax.DeclVar {
				if id, ok := decl.Target.(*syntax.Ident); ok && id.Binding != nil {
					loc := f.c.locs[id.Binding]
					t := f.locTarget(loc)
					f.emitA(bytecode.LoadUndef, t)
					f.storeFromTarget(loc, t)
				}
			}
			f.free(mark)
			continue
		}
		if id, ok := decl.Target.(*syntax.Ident); ok && id.Binding != nil {
			loc := f.c.locs[id.Binding]
			if f.withsFor(id.Binding) != nil {
				// A var declared outside the with statement: its reference
				// resolves through the with objects before the initializer.
				r := f.resolveIdent(id)
				t := f.alloc()
				f.exprNamed(decl.Init, t, id.Name)
				f.put(r, t, bindInit)
			} else if loc.inReg() && !mentions(decl.Init, id.Binding) && (d.Kind != syntax.DeclVar || f.intoReg(decl.Init)) {
				f.exprNamed(decl.Init, loc.reg, id.Name)
			} else {
				t := f.alloc()
				f.exprNamed(decl.Init, t, id.Name)
				f.storeLoc(loc, t)
			}
			f.free(mark)
			continue
		}
		if id, ok := decl.Target.(*syntax.Ident); ok {
			// A var of sloppy eval code, declared in its caller's variable
			// environment: resolved before the initializer runs.
			r := f.resolveIdent(id)
			t := f.alloc()
			f.exprNamed(decl.Init, t, id.Name)
			f.put(r, t, bindInit)
			f.free(mark)
			continue
		}
		v := f.exprReg(decl.Init)
		f.bindPattern(decl.Target, v, bindInit)
		f.free(mark)
	}
}

// returnExpr compiles `return e`. Where a return is one Ret (no finally
// to route it through, no iterator to close, no epilogue to jump to), a
// conditional returns from each of its branches instead of joining their
// values in one register with a Move and a Jmp.
func (f *funcState) returnExpr(e syntax.Expr) {
	if c, ok := e.(*syntax.CondExpr); ok && len(f.finallys) == 0 && len(f.iterCloses) == 0 && f.retLbl == nil {
		// A conditional that folds is one constant: expr loads it, where
		// splitting would compile a dead branch and a jump.
		if _, folds := f.fold(c); !folds {
			f.setPosNode(c)
			alt := f.newLabel()
			f.condJump(c.Test, alt, false)
			f.returnExpr(c.Cons)
			f.bind(alt)
			f.returnExpr(c.Alt)
			return
		}
	}
	mark := f.nregs
	f.emitReturn(f.returnValue(e))
	f.free(mark)
}

// --- labels, break and continue -----------------------------------------------------

func (f *funcState) takeLabels() []string {
	names := f.pendingLabels
	f.pendingLabels = nil
	return names
}

func (f *funcState) labeled(s *syntax.LabeledStmt) {
	f.pendingLabels = append(f.pendingLabels, s.Label.Name)
	switch s.Body.(type) {
	case *syntax.ForStmt, *syntax.ForInOfStmt, *syntax.WhileStmt, *syntax.DoWhileStmt, *syntax.SwitchStmt, *syntax.LabeledStmt:
		f.stmt(s.Body)
		return
	}
	t := &jumpTarget{names: f.takeLabels(), breakLbl: f.newLabel(), breakDepth: f.envDepth, finallyDepth: len(f.finallys), iterDepth: len(f.iterCloses)}
	f.targets = append(f.targets, t)
	f.stmt(s.Body)
	f.targets = f.targets[:len(f.targets)-1]
	f.bind(t.breakLbl)
}

func (f *funcState) pushLoopTarget(names []string, brk, cont *label) *jumpTarget {
	t := &jumpTarget{
		names: names, isLoop: true, breakLbl: brk, continueLbl: cont,
		breakDepth: f.envDepth, continueDepth: f.envDepth, finallyDepth: len(f.finallys),
		iterDepth: len(f.iterCloses), contIterDepth: len(f.iterCloses),
	}
	f.targets = append(f.targets, t)
	return t
}

func (f *funcState) popTarget() { f.targets = f.targets[:len(f.targets)-1] }

func (f *funcState) findTarget(s syntax.Stmt, lbl *syntax.Ident, isCont bool) *jumpTarget {
	for i := len(f.targets) - 1; i >= 0; i-- {
		t := f.targets[i]
		if lbl != nil {
			for _, n := range t.names {
				if n == lbl.Name {
					return t
				}
			}
			continue
		}
		if t.isLoop || (!isCont && t.isSwitch) {
			return t
		}
	}
	pos, _ := s.Range()
	if lbl != nil {
		f.c.fail(pos, "SyntaxError: Undefined label '"+lbl.Name+"'")
	}
	f.c.fail(pos, "SyntaxError: Illegal break/continue statement")
	return nil
}

// emitBreak jumps to a break/continue target, unwinding environments and
// routing through enclosing finally blocks.
func (f *funcState) emitBreak(t *jumpTarget, isCont bool) {
	if len(f.finallys) > t.finallyDepth {
		fs := f.finallys[len(f.finallys)-1]
		f.emitIterCloses(f.finallyIterBase())
		k := fs.exitKind(t, isCont)
		f.emitWord(bytecode.EncodeAsBx(bytecode.LoadInt, uint8(fs.rk), int16(k)))
		f.emitPopEnvs(fs.envDepth)
		f.emitJump(bytecode.Jmp, 0, fs.body)
		return
	}
	if isCont {
		f.emitIterCloses(t.contIterDepth)
		f.emitPopEnvs(t.continueDepth)
		f.emitJump(bytecode.Jmp, 0, t.continueLbl)
		return
	}
	f.emitIterCloses(t.iterDepth)
	f.emitPopEnvs(t.breakDepth)
	f.emitJump(bytecode.Jmp, 0, t.breakLbl)
}

// --- loops ----------------------------------------------------------------------

func (f *funcState) forStmt(s *syntax.ForStmt) {
	names := f.takeLabels()
	es, mark := f.enterScope(s.Scope)
	switch init := s.Init.(type) {
	case *syntax.VarDecl:
		f.varDecl(init)
	case syntax.Expr:
		f.exprEffect(init)
	}
	if es != nil {
		f.emitNone(bytecode.CopyEnv)
	}
	hoist := f.hoistLoop(s)
	loop, cont, cond, exit := f.newLabel(), f.newLabel(), f.newLabel(), f.newLabel()
	f.loopEntry(s.Cond, cond)
	f.bind(loop)
	f.loopTop(s.Cond, exit)
	f.pushLoopTarget(names, exit, cont)
	f.stmt(s.Body)
	f.popTarget()
	f.bind(cont)
	if es != nil {
		f.emitNone(bytecode.CopyEnv)
	}
	if s.Update != nil {
		f.exprEffect(s.Update)
	}
	f.bind(cond)
	f.loopBack(s.Cond, s.Pos, loop)
	f.bind(exit)
	f.endHoist(hoist)
	f.leaveScope(es, mark)
}

// rotates reports whether a loop with condition c is rotated, ending in
// its condition's jump (loopBack): when c is a relational comparison that
// compiles to one compare-and-branch op (compareJump). Only JmpLt .. JmpGe
// and their immediate forms then close loops, so only they check the
// interrupt flag; a loop with any other condition tests it at the top and
// ends in a Jmp, as do the equality tests, whose ops stay unchecked.
func (f *funcState) rotates(c syntax.Expr) bool {
	if c == nil {
		return false
	}
	if _, ok := f.fold(c); ok {
		return false
	}
	b, ok := compareJump(c)
	return ok && b.Op != syntax.StrictEq && b.Op != syntax.StrictNeq
}

// loopEntry enters a rotated for or while loop at its condition at cond.
func (f *funcState) loopEntry(c syntax.Expr, cond *label) {
	if f.rotates(c) {
		f.emitJump(bytecode.Jmp, 0, cond)
	}
}

// loopBack closes a loop: a rotated one at its condition, which jumps back
// to loop while cond holds, so an iteration takes one jump, the
// condition's, where a test at the top and a Jmp at the bottom took two;
// for and while enter it with a Jmp to the condition. A backward
// compare-and-branch checks the interrupt flag whichever way it goes, so
// a loop checks once per iteration, its last included, as it did with the
// Jmp. Any other loop ends in a Jmp at pos.
func (f *funcState) loopBack(cond syntax.Expr, pos int, loop *label) {
	if !f.rotates(cond) {
		f.setPos(pos)
		f.emitJump(bytecode.Jmp, 0, loop)
		return
	}
	f.setPosNode(cond)
	f.condJump(cond, loop, true)
}

// loopTop tests the condition of a loop that does not rotate at its top.
func (f *funcState) loopTop(cond syntax.Expr, exit *label) {
	if cond != nil && !f.rotates(cond) {
		f.setPosNode(cond)
		f.condJump(cond, exit, false)
	}
}

func (f *funcState) whileStmt(s *syntax.WhileStmt) {
	names := f.takeLabels()
	mark := f.nregs
	hoist := f.hoistLoop(s)
	loop, cont, exit := f.newLabel(), f.newLabel(), f.newLabel()
	f.loopEntry(s.Cond, cont)
	f.bind(loop)
	f.loopTop(s.Cond, exit)
	f.pushLoopTarget(names, exit, cont)
	f.stmt(s.Body)
	f.popTarget()
	f.bind(cont)
	f.loopBack(s.Cond, s.Pos, loop)
	f.bind(exit)
	f.endHoist(hoist)
	f.free(mark)
}

func (f *funcState) doWhile(s *syntax.DoWhileStmt) {
	names := f.takeLabels()
	mark := f.nregs
	hoist := f.hoistLoop(s)
	loop, cont, exit := f.newLabel(), f.newLabel(), f.newLabel()
	f.bind(loop)
	f.pushLoopTarget(names, exit, cont)
	f.stmt(s.Body)
	f.popTarget()
	f.bind(cont)
	if !f.rotates(s.Cond) {
		f.setPosNode(s.Cond)
		f.condJump(s.Cond, exit, false)
	}
	f.loopBack(s.Cond, s.Pos, loop)
	f.bind(exit)
	f.endHoist(hoist)
	f.free(mark)
}

func (f *funcState) forInOf(s *syntax.ForInOfStmt) {
	names := f.takeLabels()
	mark0 := f.nregs
	// The loop scope is materialized before the right-hand side so that
	// references to the loop variable inside it hit the TDZ marker.
	if d, ok := s.Left.(*syntax.VarDecl); ok && d.Decls[0].Init != nil {
		f.forInInit(d)
	}
	es, mark := f.enterScope(s.Scope)
	it := f.allocN(4)
	f.expr(s.Right, it)
	f.setPos(s.Pos)
	if s.Await {
		f.emitAB(bytecode.AsyncIterInit, it, it)
	} else if s.Of {
		f.emitAB(bytecode.IterInit, it, it)
	} else {
		f.emitAB(bytecode.ForInInit, it, it)
	}
	loop, cont, exit := f.newLabel(), f.newLabel(), f.newLabel()
	f.bind(loop)
	value := it + 2
	if s.Await {
		f.asyncIterNext(it, exit)
	} else if s.Of {
		f.emitJump(bytecode.IterNext, it, exit)
	} else {
		f.emitJump(bytecode.ForInNext, it, exit)
		value = it + 3
	}
	// ForIn/OfBodyEvaluation: an abrupt completion of the binding or the
	// body closes the iterator (a throw through the handler row below,
	// break and return through emitIterCloses); the next method's own
	// errors do not.
	bodyStart, loopDepth := f.pc(), f.envDepth
	t := f.pushLoopTarget(names, exit, cont)
	var g guard
	if s.Of {
		g = f.openGuard()
		f.iterCloses = append(f.iterCloses, openIter{reg: it, finallyDepth: len(f.finallys), async: s.Await, guard: g.level})
		t.contIterDepth = len(f.iterCloses)
	}
	if es != nil {
		f.emitNone(bytecode.CopyEnv)
	}
	// Each iteration gets a fresh TDZ environment: a binding that its own
	// pattern reads before it is initialized throws on every iteration.
	f.initTDZ(s.Scope)
	switch left := s.Left.(type) {
	case *syntax.VarDecl:
		f.bindPattern(left.Decls[0].Target, value, bindInit)
	case *syntax.CallExpr:
		if s.Of {
			f.callTarget(left, "Invalid left-hand side in for-of loop")
		} else {
			f.callTarget(left, "Invalid left-hand side in for-in loop")
		}
	case syntax.Pattern:
		f.bindPattern(left, value, bindAssign)
	}
	f.stmt(s.Body)
	f.popTarget()
	f.bind(cont)
	f.setPos(s.Pos)
	f.emitJump(bytecode.Jmp, 0, loop)
	if s.Of {
		f.iterCloses = f.iterCloses[:len(f.iterCloses)-1]
		f.addHandler(g, bytecode.Handler{
			Start: uint32(bodyStart), End: uint32(f.pc()), Handler: uint32(f.pc()),
			StackDepth: uint16(loopDepth), Kind: bytecode.HandlerCatch, Reg: uint16(it + 2),
		})
		f.closeGuard()
		if s.Await {
			f.asyncIterThrow(it, loopDepth)
		} else {
			f.emitAB(bytecode.IterThrow, it, it+2)
		}
	}
	f.bind(exit)
	f.leaveScope(es, mark)
	f.free(mark0)
}

// --- switch ---------------------------------------------------------------------

func (f *funcState) switchStmt(s *syntax.SwitchStmt) {
	names := f.takeLabels()
	mark := f.nregs
	disc := f.alloc()
	f.expr(s.Disc, disc)
	es, mark2 := f.enterScope(s.Scope)
	exit := f.newLabel()
	labels := make([]*label, len(s.Cases))
	var def *label
	for i, c := range s.Cases {
		labels[i] = f.newLabel()
		if c.Test == nil {
			def = labels[i]
			continue
		}
		m := f.nregs
		f.setPosNode(c.Test)
		v := f.operand(c.Test)
		f.emitJump(bytecode.JmpStrictEq, disc, labels[i])
		f.emitExtra(uint32(v))
		f.free(m)
	}
	if def != nil {
		f.emitJump(bytecode.Jmp, 0, def)
	} else {
		f.emitJump(bytecode.Jmp, 0, exit)
	}
	target := &jumpTarget{names: names, isSwitch: true, breakLbl: exit, breakDepth: f.envDepth, finallyDepth: len(f.finallys), iterDepth: len(f.iterCloses)}
	f.targets = append(f.targets, target)
	for i, c := range s.Cases {
		f.bind(labels[i])
		f.stmts(c.Body)
	}
	f.popTarget()
	f.bind(exit)
	f.leaveScope(es, mark2)
	f.free(mark)
}

// --- try/catch/finally ---------------------------------------------------------------

// A guard is a protected range whose handler rows are still to come: a
// try statement, a for-of body or an array pattern's elements. An iterator
// close (emitIterCloses) runs where its loop or pattern completes, outside
// the guards opened inside it: their rows skip it (closeGaps). Its own row
// covers it, and rethrows what it throws. level is the number of guards
// around this one, and gaps the closeGaps entries that predate it.
type guard struct{ level, gaps int }

// closeGap is the code of one iterator close, which the rows of the guards
// past level skip.
type closeGap struct {
	start, end uint32
	level      int
}

func (f *funcState) openGuard() guard {
	f.guards++
	return guard{level: f.guards - 1, gaps: len(f.closeGaps)}
}

// closeGuard ends the innermost guard once its rows are added.
func (f *funcState) closeGuard() {
	if f.guards--; f.guards == 0 {
		f.closeGaps = f.closeGaps[:0]
	}
}

// addHandler adds the row h of the guard g, split around the iterator
// closes it skips.
func (f *funcState) addHandler(g guard, h bytecode.Handler) {
	for _, x := range f.closeGaps[g.gaps:] {
		if x.level >= g.level || x.end <= h.Start || x.start >= h.End {
			continue
		}
		if x.start > h.Start {
			part := h
			part.End = x.start
			f.handlers = append(f.handlers, part)
		}
		h.Start = x.end
	}
	if h.Start < h.End {
		f.handlers = append(f.handlers, h)
	}
}

func (f *funcState) tryStmt(s *syntax.TryStmt) {
	mark := f.nregs
	var fs *finallyState
	if s.Finalizer != nil {
		rk := f.allocN(2)
		fs = &finallyState{rk: rk, rv: rk + 1, body: f.newLabel(), envDepth: f.envDepth}
		f.finallys = append(f.finallys, fs)
	}
	tryDepth := f.envDepth
	tryStart := f.pc()
	g := f.openGuard()
	f.block(s.Block)
	tryEnd := f.pc()
	protectedEnd := tryEnd

	if s.Handler != nil {
		after := f.newLabel()
		f.emitJump(bytecode.Jmp, 0, after)
		exc := f.alloc()
		f.addHandler(g, bytecode.Handler{
			Start: uint32(tryStart), End: uint32(tryEnd), Handler: uint32(f.pc()),
			StackDepth: uint16(tryDepth), Kind: bytecode.HandlerCatch, Reg: uint16(exc),
		})
		f.clearCompletion() // the try block's value is discarded
		es, m := f.enterScope(s.CatchScope)
		if s.Param != nil {
			f.setPosNode(s.Param)
			f.bindPattern(s.Param, exc, bindInit)
		}
		f.block(s.Handler)
		f.leaveScope(es, m)
		protectedEnd = f.pc()
		f.bind(after)
	}

	if fs == nil {
		f.closeGuard()
		f.free(mark)
		return
	}
	f.finallys = f.finallys[:len(f.finallys)-1]
	// Normal completion falls into the finally body with kind 0; the handler
	// row enters at the same pc after the interpreter stored kind 1 + value.
	f.emitWord(bytecode.EncodeAsBx(bytecode.LoadInt, uint8(fs.rk), 0))
	f.addHandler(g, bytecode.Handler{
		Start: uint32(tryStart), End: uint32(protectedEnd), Handler: uint32(f.pc()),
		StackDepth: uint16(tryDepth), Kind: bytecode.HandlerFinally, Reg: uint16(fs.rk),
	})
	f.closeGuard()
	f.bind(fs.body)
	saved := -1
	if f.completion >= 0 {
		// A finally block that completes normally keeps the completion
		// of the try statement's other blocks.
		saved = f.alloc()
		f.emitMove(saved, f.completion)
		f.clearCompletion()
	}
	f.block(s.Finalizer)
	if saved >= 0 {
		f.emitMove(f.completion, saved)
	}

	// Completion dispatch.
	f.setPos(s.Finalizer.Pos)
	t := f.alloc()
	next := f.newLabel()
	f.emitWord(bytecode.EncodeAsBx(bytecode.LoadInt, uint8(t), 1))
	f.emitABC(bytecode.StrictEq, t, fs.rk, t)
	f.emitJump(bytecode.JmpF, t, next)
	f.emitA(bytecode.Throw, fs.rv)
	f.bind(next)
	for _, x := range fs.exits {
		next = f.newLabel()
		f.emitWord(bytecode.EncodeAsBx(bytecode.LoadInt, uint8(t), int16(x.kind)))
		f.emitABC(bytecode.StrictEq, t, fs.rk, t)
		f.emitJump(bytecode.JmpF, t, next)
		if x.target == nil {
			f.emitReturn(fs.rv)
		} else {
			f.emitBreak(x.target, x.isCont)
		}
		f.bind(next)
	}
	f.free(mark)
}

package compiler

import (
	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Generator lowering. A generator function compiles to a KindMethod body
// that binds its parameters and declarations, then runs GenStart: the
// generator is created with the frame suspended there, and the call returns
// it. Each resumption continues after the suspending op with the sent value
// in R[A] and the mode in R[A+1] (undefined: next, true: throw, false:
// return); the code after a Yield dispatches on the mode with ordinary
// jumps, so throw and return run handlers, finally blocks and for-of closes
// like the statements.

// genMaxDepth bounds the environment depth and code size at a suspension
// point and at a direct eval: the engine keeps both in the saved pc
// (generator.go, and frameOp's rerun in sloppy.go).
const (
	genMaxDepth = 1<<8 - 1
	genMaxCode  = 1<<24 - 1
)

// emitClosure creates the closure of child idx in dst.
func (f *funcState) emitClosure(dst int, idx uint16) {
	f.emitABx(bytecode.Closure, dst, idx)
	switch c := f.children[idx]; {
	case c.Async:
		f.emitA(bytecode.AsyncFunc, dst)
	case c.Generator:
		f.emitA(bytecode.GenFunc, dst)
	}
}

// genStart creates the generator of a generator body after its declaration
// instantiation and suspends the frame; genReg holds the generator from
// then on.
func (f *funcState) genStart() {
	f.genReg = f.alloc()
	f.emitA(bytecode.LoadThis, f.genReg)
	op := bytecode.GenStart
	if f.out.Async {
		op = bytecode.AsyncGenStart
	}
	f.genSuspend(op, f.genReg, 0, 0)
}

// genSuspend emits a suspending op, which the resume pc follows.
func (f *funcState) genSuspend(op bytecode.Op, a, b, c int) {
	if f.envDepth > genMaxDepth || f.pc() >= genMaxCode {
		f.c.fail(f.curPos, "SyntaxError: generator too large"+unsupportedSuffix)
	}
	f.emitABC(op, a, b, c)
}

// yield compiles `yield X` and `yield* X` into dst.
func (f *funcState) yield(e *syntax.YieldExpr, dst int) {
	mark := f.nregs
	rv := f.allocN(2) // the value, then the resumption mode
	if e.X != nil {
		f.expr(e.X, rv)
	} else {
		f.emitA(bytecode.LoadUndef, rv)
	}
	f.setPos(e.Pos)
	switch {
	case e.Delegate && f.out.Async:
		f.asyncYieldStar(rv)
	case e.Delegate:
		f.yieldStar(rv)
	case f.out.Async:
		f.asyncYield(rv)
	default:
		f.genSuspend(bytecode.Yield, rv, f.genReg, 0)
		next, ret := f.newLabel(), f.newLabel()
		f.emitJump(bytecode.JmpNullish, rv+1, next)
		f.emitJump(bytecode.JmpF, rv+1, ret)
		f.emitA(bytecode.Throw, rv)
		f.bind(ret)
		f.emitReturn(rv)
		f.bind(next)
	}
	f.emitMove(dst, rv)
	f.free(mark)
}

// yieldStar delegates to the iterable in rv (§15.5.5): Delegate forwards
// each resumption to the inner iterator, and its results are yielded as
// they are until it is done.
func (f *funcState) yieldStar(rv int) {
	ri := f.allocN(2) // the inner iterator and its next method
	f.emitAB(bytecode.GenIter, ri, rv)
	f.emitA(bytecode.LoadUndef, rv)
	f.emitA(bytecode.LoadUndef, rv+1)
	loop, done, value := f.newLabel(), f.newLabel(), f.newLabel()
	f.bind(loop)
	f.emitAB(bytecode.Delegate, rv, ri)
	f.emitJump(bytecode.JmpNotNullish, rv+1, done)
	f.genSuspend(bytecode.Yield, rv, f.genReg, 1)
	f.emitJump(bytecode.Jmp, 0, loop)
	f.bind(done)
	f.emitJump(bytecode.JmpT, rv+1, value)
	f.emitReturn(rv)
	f.bind(value)
}

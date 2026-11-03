package compiler

import (
	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Async lowering. An async function body runs from a LoadThis of the
// call's this into genReg to an AsyncReturn that resolves the
// call's promise: every return runs one before its Ret (emitReturn), and
// the body's end jumps to the one of an epilogue through retReg/retLbl.
// A catch-all handler covering the body rejects the promise with what the
// body threw. Parameter binding runs inside that handler, so its errors
// reject the promise too. The engine makes the promise and the call's
// state at the first Await, or the promise at the end of a call that
// never awaited. `await x` suspends the frame at Await like a yield; the
// resumption hands the code after it the value in R[A] and, in R[A+1],
// undefined (fulfilled) or true (rejected), and a rejection throws where
// the await stands.
//
// An async generator starts like a generator (AsyncGenStart after its
// declaration instantiation) and completes into the engine's request
// queue, with no epilogue. `yield` awaits its operand before AsyncYield
// settles the head request, and a return resumption awaits the value it
// carries before returning. for await and async yield* step their
// iterators with separate call, await and result-check ops.

// asyncBody reports whether f compiles an async function body, the body
// with the promise epilogue (a module with top-level await is one).
func (f *funcState) asyncBody() bool { return f.out.Async && !f.out.Generator }

// asyncStart begins an async function body after its prologue: genReg
// holds the call's this, where the first Await leaves the call's state,
// and the catch-all handler covers the code from here to the epilogue
// (asyncEnd).
func (f *funcState) asyncStart() {
	if !f.asyncBody() {
		return
	}
	f.genReg = f.alloc()
	f.emitA(bytecode.LoadThis, f.genReg)
	f.retReg, f.retLbl = f.alloc(), f.newLabel()
	f.asyncTry = f.pc()
}

// asyncReturn returns R[reg] from an async function body: AsyncReturn
// resolves the call's promise with it and leaves the promise in reg for
// the Ret. It runs inside the catch-all handler, which it never reaches:
// AsyncReturn throws nothing a handler catches.
func (f *funcState) asyncReturn(reg int) {
	f.emitAB(bytecode.AsyncReturn, reg, f.genReg)
	f.emitA(bytecode.Ret, reg)
}

// asyncEnd emits the epilogue of an async function body at retLbl: the
// promise resolves with retReg, and the catch-all handler rejects it.
func (f *funcState) asyncEnd() {
	end := f.pc()
	f.asyncReturn(f.retReg)
	f.handlers = append(f.handlers, bytecode.Handler{
		Start: uint32(f.asyncTry), End: uint32(end), Handler: uint32(f.pc()),
		Kind: bytecode.HandlerCatch, Reg: uint16(f.retReg),
	})
	f.emitAB(bytecode.AsyncThrow, f.retReg, f.genReg)
	f.emitA(bytecode.Ret, f.retReg)
}

// await awaits R[rv] in place (R[rv+1] receives the outcome): the result
// replaces R[rv], and a rejection throws.
func (f *funcState) await(rv int) {
	ok := f.newLabel()
	f.genSuspend(bytecode.Await, rv, f.genReg, 0)
	f.emitJump(bytecode.JmpNullish, rv+1, ok)
	f.emitA(bytecode.Throw, rv)
	f.bind(ok)
}

// awaitExpr compiles `await X` into dst.
func (f *funcState) awaitExpr(e *syntax.AwaitExpr, dst int) {
	mark := f.nregs
	rv := f.allocN(2) // the value, then the outcome
	f.expr(e.X, rv)
	f.setPos(e.Pos)
	f.await(rv)
	f.emitMove(dst, rv)
	f.free(mark)
}

// returnValue evaluates the operand of a return statement into a
// register; an async generator awaits it first.
func (f *funcState) returnValue(e syntax.Expr) int {
	if !f.out.Async || !f.out.Generator {
		return f.exprReg(e)
	}
	rv := f.allocN(2)
	f.expr(e, rv)
	f.await(rv)
	return rv
}

// asyncYield suspends an async generator at `yield` with the value in rv
// (§27.6.3.8 AsyncGeneratorYield): the operand is awaited, AsyncYield
// settles the head request, and a return resumption awaits its value
// (AsyncGeneratorUnwrapYieldResumption).
func (f *funcState) asyncYield(rv int) {
	f.await(rv)
	f.genSuspend(bytecode.AsyncYield, rv, f.genReg, 0)
	next, throw, ret := f.newLabel(), f.newLabel(), f.newLabel()
	f.emitJump(bytecode.JmpNullish, rv+1, next)
	f.emitJump(bytecode.JmpT, rv+1, throw)
	f.genSuspend(bytecode.Await, rv, f.genReg, 0)
	f.emitJump(bytecode.JmpNullish, rv+1, ret)
	f.bind(throw)
	f.emitA(bytecode.Throw, rv)
	f.bind(ret)
	f.emitReturn(rv)
	f.bind(next)
}

// asyncYieldStar delegates to the async iterable in rv (§15.5.5 with
// generatorKind async). Each round calls the inner method for the
// resumption (AsyncDelegate C=0), awaits its result, and reads it (C=1):
// the inner values are yielded without an await, and a return resumption
// awaits its value first, a rejection there turning it into a throw. The
// generator returns the value of the inner return method's done result as
// it is, and awaits the received value only where there is no such method.
func (f *funcState) asyncYieldStar(rv int) {
	ri := f.allocN(3) // the inner iterator, its next method and the step's phase
	f.emitAB(bytecode.AsyncIterInit, ri, rv)
	f.emitA(bytecode.LoadUndef, rv)
	f.emitA(bytecode.LoadUndef, rv+1)
	loop, ret, yield, done := f.newLabel(), f.newLabel(), f.newLabel(), f.newLabel()
	f.bind(loop)
	f.emitABC(bytecode.AsyncDelegate, rv, ri, 0)
	f.emitJump(bytecode.JmpNotNullish, rv+1, ret)
	f.await(rv)
	f.emitABC(bytecode.AsyncDelegate, rv, ri, 1)
	f.emitJump(bytecode.JmpNullish, rv+1, yield)
	f.emitJump(bytecode.JmpT, rv+1, done)
	f.emitReturn(rv)
	f.bind(ret)
	f.await(rv)
	f.emitReturn(rv)
	f.bind(yield)
	f.genSuspend(bytecode.AsyncYield, rv, f.genReg, 0)
	f.emitJump(bytecode.JmpNullish, rv+1, loop)
	f.emitJump(bytecode.JmpT, rv+1, loop)
	f.genSuspend(bytecode.Await, rv, f.genReg, 0)
	f.emitJump(bytecode.JmpNotNullish, rv+1, loop)
	f.emitA(bytecode.LoadFalse, rv+1)
	f.emitJump(bytecode.Jmp, 0, loop)
	f.bind(done)
}

// The record of a for await loop is it: the iterator, its next method,
// the value and a scratch register for the awaits' outcome.

// asyncIterNext steps the for await record at it: the next method's result
// is awaited, then checked; done jumps to exit, else R[it+2] is the value.
func (f *funcState) asyncIterNext(it int, exit *label) {
	f.emitA(bytecode.AsyncIterNext, it)
	f.await(it + 2)
	f.emitJump(bytecode.AsyncIterResult, it, exit)
}

// asyncIterClose closes the for await record at it for a break, continue
// or return leaving the loop (AsyncIteratorClose with a normal completion):
// the return method's result is awaited and must be an object.
func (f *funcState) asyncIterClose(it int) {
	skip := f.newLabel()
	f.emitJump(bytecode.AsyncIterClose, it, skip)
	f.await(it + 2)
	f.emitA(bytecode.AsyncIterClosed, it)
	f.bind(skip)
}

// asyncIterThrow is the handler of a for await body: it closes the record
// at it with the throw completion in R[it+2] and rethrows it, dropping
// whatever closing throws or returns.
func (f *funcState) asyncIterThrow(it int, depth int) {
	f.emitMove(it+1, it+2)
	rethrow := f.newLabel()
	start := f.pc()
	f.emitJump(bytecode.AsyncIterClose, it, rethrow)
	f.genSuspend(bytecode.Await, it+2, f.genReg, 0)
	end := f.pc()
	f.bind(rethrow)
	f.handlers = append(f.handlers, bytecode.Handler{
		Start: uint32(start), End: uint32(end), Handler: uint32(f.pc()),
		StackDepth: uint16(depth), Kind: bytecode.HandlerCatch, Reg: uint16(it + 2),
	})
	f.emitA(bytecode.Throw, it+1)
}

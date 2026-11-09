package bytecode

import "fmt"

// PCBits bounds the pcs the engine stores to run a frame from later: a
// suspended generator's or async function's resume pc, the pc a frame
// reruns from, and a module body's first pc (Module.BodyPC). The engine
// packs the frame's environment depth above them.
const PCBits = 24

// VerifyRegisters checks what the engine's interpreter relies on to index
// a frame's registers without bounds checks. It walks fn.Code from pc 0,
// one instruction and its extra words at a time, and checks that
//
//   - every register an instruction names, and every run of registers it
//     reads or writes, lies below NumRegs: the operands A, B, C and the X
//     word's register where they are registers, a call's or a
//     construction's callee, this (or new.target) and arguments, the
//     argument array of the spread forms, UndefRange's run, iterator,
//     for-in and async-iterator records, the value and mode pair of a
//     suspension (RegisterSpans), and the registers its handlers write;
//   - the interpreter runs no other instructions than the ones the walk
//     checked: every jump lands on the start of one (Op.Jumps,
//     JumpTarget), and so do every handler and the body of a module
//     (Module.BodyPC). A jump onto an extra word would run it as an
//     instruction whose operands no one checked: the X word of an
//     immediate compare holds an integer of the source;
//   - the interpreter reads no word past the code, which it reads without
//     bounds checks: every instruction's extra words are inside it, and
//     the last instruction never continues at the next pc (a return, a
//     throw, Jmp, IterClose or IterThrow: ends), so neither a fallthrough
//     nor a suspension or rerun, which continues after its instruction,
//     leaves the code. A function has at least one instruction;
//   - the inline cache an instruction names in its X word's high 16 bits
//     (icOperand) is below ICCount, and a NewObjectLit's is named by no
//     other instruction (verifyLiteralICs);
//   - a NewObjectLit makes at least one property, and its keys are
//     property names;
//   - the pcs the engine stores fit in PCBits: a module body's, and every
//     pc of a function that can suspend or rerun (Op.SavesPC);
//   - each handler's range lies inside the code, and it is a catch or a
//     finally row (the engine enters a row of another kind as a finally);
//   - NumRegs is at most MaxRegister+1, the registers of the window;
//   - the constant an op reads as a given kind is one (constOperand):
//     NewRegExp's and RegExpTest's (in its X word) a regular expression,
//     NewPrivateName's and ThrowError's a string, and the name in the X
//     word of a property or global binding access a property name, a
//     string with Key set (keyOperand).
//     The engine reads it as that kind without looking;
//   - so is the register an op reads as a given kind (KindOperands):
//     every path to the op leaves one there (verifyKinds);
//   - the X word of JmpLt .. JmpNGe that names an environment slot
//     (EnvOperand) has A in its low byte: the interpreter reads the
//     register the low byte names before it looks at the rest;
//   - GetEnv0, GetEnv1 and GetEnv2 have B 0, 1 and 2: the interpreter
//     reads the environment their op names, the engine's other readers of
//     a GetEnv the one B names;
//   - a lazy MapArguments (B = 1) has parameter i in Env slot i, below
//     the Env's size (paramsInOrder): the engine indexes the slot by the
//     parameter. So does a mapped ApplyArguments (C = 1);
//   - a Call or New with CallOverlap is one after which no register from
//     R[A+2] up is live, in a function that may have such calls
//     (verifyOverlap); a Call's C has no other bits than CallUndefThis and
//     CallOverlap, a New's no other than CallOverlap;
//   - ApplyArguments, ArgsLen and ArgsGet name the arguments register of
//     a function with ForwardArguments, which is neither a generator nor
//     async: the engine reads the arguments the entry kept above the
//     registers. A mapped ArgsGet (X = 1) has the parameters at their Env
//     slots, as a mapped ApplyArguments does;
//
// A function with an opcode RegisterSpans has no layout for fails. A word
// that is no opcode names no register: the interpreter faults on it before
// it touches one. It does not look at fn's children.
//
// The engine runs it on every function it materializes (engine fillMeta),
// and the checked build of the interpreter (go test -tags moejsregcheck
// ./engine) compares every register access with RegisterSpans and every pc
// it dispatches with the walk's instruction starts.
func VerifyRegisters(fn *Function) error {
	n := int(fn.NumRegs)
	if n > MaxRegister+1 {
		// The interpreter's window has MaxRegister+1 registers, and the
		// computed ones (a handler's, a record's A+1) are checked
		// against it; the compiler never needs more.
		return fmt.Errorf("bytecode: %q: %d registers, more than the %d of a frame's window", fn.Name, n, MaxRegister+1)
	}
	code := fn.Code
	// starts marks the first word of every instruction the walk decodes; a
	// function below 4,096 words keeps it on the stack.
	var buf [64]uint64
	starts := buf[:]
	if k := len(code)>>6 + 1; k > len(buf) {
		starts = make([]uint64, k)
	}
	if len(code) == 0 {
		return fmt.Errorf("bytecode: %q has no instructions", fn.Name)
	}
	if fn.ForwardArguments && (!fn.HasArguments || fn.Generator || fn.Async) {
		// A suspended frame keeps its registers only.
		return fmt.Errorf("bytecode: %q forwards arguments without an arguments register or across a suspension", fn.Name)
	}
	savesPC, kinds, lits, overlap := false, false, false, false
	var tracked [4]uint64 // the registers of KindOperands, below NumRegs and so below 256
	last := 0             // the start of the last instruction
	for pc := 0; pc < len(code); {
		starts[pc>>6] |= 1 << (pc & 63)
		last = pc
		w := code[pc]
		op := DecodeOp(w)
		if int(op) >= OpCount {
			// The interpreter faults on it before it touches a register.
			pc++
			continue
		}
		info := verifyOps[op]
		extra := int(info >> verifyExtra)
		if pc+extra >= len(code) {
			return fmt.Errorf("bytecode: %q pc %d: %s lacks its extra words", fn.Name, pc, op)
		}
		var x uint32
		if extra > 0 {
			x = code[pc+1]
		}
		if info&verifyIC != 0 && x>>16 >= fn.ICCount {
			return fmt.Errorf("bytecode: %q pc %d: %s names inline cache %d of %d", fn.Name, pc, op, x>>16, fn.ICCount)
		}
		mask := registerMasks[op]
		operands := w & mask
		if mask&1 != 0 || mask != 0 && int(max(DecodeA(operands), DecodeB(operands), DecodeC(operands))) >= n {
			var bad error
			ok := RegisterSpans(op, w, x, func(r, k int) {
				if bad == nil && r+k > n {
					bad = fmt.Errorf("bytecode: %q pc %d: %s names registers %d..%d of %d", fn.Name, pc, op, r, r+k-1, n)
				}
			})
			if !ok {
				return fmt.Errorf("bytecode: %q pc %d: no register layout for %s", fn.Name, pc, op)
			}
			if bad != nil {
				return bad
			}
		}
		switch info & checkMask {
		case checkConst:
			want, _ := constOperand(op)
			if bx := int(DecodeBx(w)); bx >= len(fn.Consts) || fn.Consts[bx].Kind != want {
				return fmt.Errorf("bytecode: %q pc %d: %s names constant %d, which is no %s", fn.Name, pc, op, bx, constKindNames[want])
			}
		case checkRegExp:
			if k := int(ExtraLo(x)); k >= len(fn.Consts) || fn.Consts[k].Kind != ConstRegExp {
				return fmt.Errorf("bytecode: %q pc %d: %s names constant %d, which is no %s", fn.Name, pc, op, k, constKindNames[ConstRegExp])
			}
		case checkKey:
			whole, _ := keyOperand(op)
			k := int(uint16(x))
			if whole {
				k = int(x)
			}
			if k >= len(fn.Consts) || fn.Consts[k].Kind != ConstString || !fn.Consts[k].Key {
				return fmt.Errorf("bytecode: %q pc %d: %s names constant %d, which is no property name", fn.Name, pc, op, k)
			}
		case checkStrict:
			// The engine compares with the constant as with a value that is
			// not a number (run).
			if k := int(x); k >= len(fn.Consts) || !strictOperand(fn.Consts[k].Kind) {
				return fmt.Errorf("bytecode: %q pc %d: %s names constant %d, which is no string, null or undefined", fn.Name, pc, op, k)
			}
		case checkObject:
			// The engine takes the constants for property names, and
			// makes C properties.
			lo, k := int(ExtraLo(x)), int(DecodeC(w))
			if k == 0 || lo+k > len(fn.Consts) {
				return fmt.Errorf("bytecode: %q pc %d: %s of constants %d..%d of %d", fn.Name, pc, op, lo, lo+k-1, len(fn.Consts))
			}
			for i := lo; i < lo+k; i++ {
				if c := fn.Consts[i]; c.Kind != ConstString || !c.Key {
					return fmt.Errorf("bytecode: %q pc %d: %s names constant %d, which is no property name", fn.Name, pc, op, i)
				}
			}
			lits = true
		case checkArray:
			// The engine takes the constants' values as they are.
			lo, n := int(ExtraLo(x)), int(ExtraHi(x))
			if n == 0 || lo+n > len(fn.Consts) {
				return fmt.Errorf("bytecode: %q pc %d: %s of constants %d..%d of %d", fn.Name, pc, op, lo, lo+n-1, len(fn.Consts))
			}
			for k := lo; k < lo+n; k++ {
				if c := fn.Consts[k].Kind; c != ConstNumber && !strictOperand(c) {
					return fmt.Errorf("bytecode: %q pc %d: %s names constant %d, which is no number, string, null or undefined", fn.Name, pc, op, k)
				}
			}
		case checkEnvOperand:
			if x >= EnvOperand && (x>>16 != EnvOperand>>16 || x&0xff != uint32(DecodeA(w))) {
				return fmt.Errorf("bytecode: %q pc %d: %s with environment operand %#x for A %d", fn.Name, pc, op, x, DecodeA(w))
			}
		case checkEnvDepth:
			if op == GetEnv0 && DecodeB(w) != 0 || op == GetEnv1 && DecodeB(w) != 1 || op == GetEnv2 && DecodeB(w) != 2 {
				return fmt.Errorf("bytecode: %q pc %d: %s with B %d", fn.Name, pc, op, DecodeB(w))
			}
		case checkMapArguments:
			if DecodeB(w) != 0 && !paramsInOrder(fn, DecodeB(w)) {
				return fmt.Errorf("bytecode: %q pc %d: MapArguments B %d with parameters not at their Env slots", fn.Name, pc, DecodeB(w))
			}
		case checkApplyArguments:
			if !fn.ForwardArguments || int(DecodeB(w)) != argumentsRegister(fn) || DecodeC(w) != 0 && !paramsInOrder(fn, 1) {
				return fmt.Errorf("bytecode: %q pc %d: ApplyArguments of R[%d], which is no arguments register it may forward", fn.Name, pc, DecodeB(w))
			}
		case checkArguments:
			if !fn.ForwardArguments || int(DecodeB(w)) != argumentsRegister(fn) || op == ArgsGet && (x > 1 || x == 1 && !paramsInOrder(fn, 1)) {
				return fmt.Errorf("bytecode: %q pc %d: %s of R[%d], which is no arguments register it may read", fn.Name, pc, op, DecodeB(w))
			}
		case checkCall:
			allowed := uint8(CallOverlap)
			if op == Call {
				allowed |= CallUndefThis
			}
			if DecodeC(w)&^allowed != 0 {
				return fmt.Errorf("bytecode: %q pc %d: %s with C %d", fn.Name, pc, op, DecodeC(w))
			}
			overlap = overlap || DecodeC(w)&CallOverlap != 0
		}
		savesPC = savesPC || info&verifySavesPC != 0
		if info&verifyKindsRead != 0 {
			kinds = true
			KindOperands(op, w, func(r int, _ RegKind) { tracked[r>>6] |= 1 << (r & 63) })
		}
		pc += 1 + extra
	}
	if op := DecodeOp(code[last]); int(op) >= OpCount || !ends(op) {
		return fmt.Errorf("bytecode: %q pc %d: the last instruction continues past the code", fn.Name, last)
	}
	// The jumps, once every start is known, and for verifyKinds the
	// first instruction of each block their targets and the instructions
	// after them begin.
	var leadBuf [64]uint64
	lead := leadBuf[:]
	if kinds && len(code)>>6+1 > len(leadBuf) {
		lead = make([]uint64, len(code)>>6+1)
	}
	for pc := 0; pc < len(code); {
		w := code[pc]
		op := DecodeOp(w)
		if int(op) >= OpCount {
			pc++
			continue
		}
		info := verifyOps[op]
		next := pc + 1 + int(info>>verifyExtra)
		if info&verifyJumps != 0 {
			t := JumpTarget(pc, w)
			if !isStart(starts, len(code), t) {
				return fmt.Errorf("bytecode: %q pc %d: %s jumps to %d, which starts no instruction", fn.Name, pc, op, t)
			}
			if kinds {
				lead[t>>6] |= 1 << (t & 63)
				markStart(lead, len(code), next)
			}
		} else if kinds && info&verifyEnds != 0 {
			markStart(lead, len(code), next)
		}
		pc = next
	}
	for i, h := range fn.Handlers {
		if h.Kind != HandlerCatch && h.Kind != HandlerFinally {
			// The engine enters any other row as a finally, writing
			// R[Reg+1] too: no kind is left for it to read otherwise.
			return fmt.Errorf("bytecode: %q handler %d is of kind %d, neither a catch nor a finally", fn.Name, i, h.Kind)
		}
		k := handlerRegs(h.Kind)
		if int(h.Reg)+k > n {
			return fmt.Errorf("bytecode: %q handler %d writes registers %d..%d of %d", fn.Name, i, h.Reg, int(h.Reg)+k-1, n)
		}
		if !isStart(starts, len(code), int(h.Handler)) {
			return fmt.Errorf("bytecode: %q handler %d continues at %d, which starts no instruction", fn.Name, i, h.Handler)
		}
		if h.Start > h.End || int(h.End) > len(code) {
			return fmt.Errorf("bytecode: %q handler %d covers %d..%d of %d words", fn.Name, i, h.Start, h.End, len(code))
		}
	}
	if m := fn.Module; m != nil {
		if !isStart(starts, len(code), m.BodyPC) || m.BodyPC >= 1<<PCBits {
			return fmt.Errorf("bytecode: %q: the module body's pc %d starts no instruction below %d", fn.Name, m.BodyPC, 1<<PCBits)
		}
	}
	if savesPC && len(code) >= 1<<PCBits {
		return fmt.Errorf("bytecode: %q: %d words, which a function that suspends or reruns keeps below %d", fn.Name, len(code), 1<<PCBits)
	}
	if lits {
		if err := verifyLiteralICs(fn); err != nil {
			return err
		}
	}
	if overlap && fn.overlapSum != overlapSum(fn) { // else MarkOverlapCalls proved them
		if err := verifyOverlap(fn); err != nil {
			return err
		}
	}
	if kinds {
		return verifyKinds(fn, starts, lead, &tracked)
	}
	return nil
}

// constOperand reports the kind of the constant K[Bx] that op reads as that
// kind without looking.
func constOperand(op Op) (ConstKind, bool) {
	switch op {
	case NewRegExp:
		return ConstRegExp, true
	case NewPrivateName, ThrowError:
		return ConstString, true
	}
	return 0, false
}

// icOperand reports whether the high 16 bits of op's X word name an inline
// cache entry of the function: the interpreter reads some without a bounds
// check (engine frameEnv.ic), and a NewObjectLit's keeps a shape no other
// instruction may write over (verifyLiteralICs), which an index past the
// function's entries would do in another function's.
func icOperand(op Op) bool {
	switch op {
	case GetGlobal, GetGlobalOrUndef, SetGlobal, GetProp, GetLen, SetProp, DefineField, NewObjectLit,
		GetTemplate, SetGlobalSloppy, ResolveGlobal, SetGlobalRef, SetPropSloppy:
		return true
	}
	return false
}

// verifyLiteralICs checks that the inline cache entry each NewObjectLit of
// fn names is named by no other instruction of fn: the engine keeps there
// the shape of the literal's object, which it gives the object without
// looking at the shape's properties. VerifyRegisters has checked fn's
// instructions and their extra words, and found a NewObjectLit.
func verifyLiteralICs(fn *Function) error {
	code := fn.Code
	var buf [16]uint64
	lit := buf[:]
	if k := int(fn.ICCount)>>6 + 1; k > len(buf) {
		lit = make([]uint64, k)
	}
	for pass := 0; pass < 2; pass++ {
		for pc := 0; pc < len(code); {
			op := DecodeOp(code[pc])
			if int(op) >= OpCount {
				pc++
				continue
			}
			if icOperand(op) && (op == NewObjectLit) == (pass == 0) {
				i := code[pc+1] >> 16
				if lit[i>>6]&(1<<(i&63)) != 0 {
					return fmt.Errorf("bytecode: %q pc %d: %s names inline cache %d, which a NewObjectLit names", fn.Name, pc, op, i)
				}
				if pass == 0 {
					lit[i>>6] |= 1 << (i & 63)
				}
			}
			pc += 1 + op.ExtraWords()
		}
	}
	return nil
}

// strictOperand reports whether a constant of kind k can be the right
// operand of JmpStrictEqK, JmpStrictNeK and JmpCaseK.
func strictOperand(k ConstKind) bool {
	return k == ConstString || k == ConstNull || k == ConstUndefined
}

// keyOperand reports whether op's X word names a property-name constant
// (a ConstString with Key set), which the engine reads as an interned key
// without looking, and whether the name is all of X rather than its low
// 16 bits.
func keyOperand(op Op) (whole, ok bool) {
	switch op {
	case GetGlobal, GetGlobalOrUndef, SetGlobal, GetProp, SetProp, DefineField, InitGlobal,
		SetGlobalSloppy, ResolveGlobal, SetGlobalRef, SetPropSloppy, WithGet, WithSet:
		return false, true
	case DelProp, SetGlobalVar, DelGlobal, DelPropSloppy, JmpWith:
		return true, true
	}
	return false, false
}

var constKindNames = [...]string{ConstNumber: "number", ConstString: "string", ConstBigInt: "bigint",
	ConstRegExp: "regular expression", ConstTemplate: "template", ConstFunction: "function", ConstNull: "null",
	ConstUndefined: "undefined"}

// handlerRegs is the number of registers the engine writes when it enters
// a handler of kind k: the thrown value for a catch, and before it the
// completion kind for anything else (run).
func handlerRegs(k HandlerKind) int {
	if k == HandlerCatch {
		return 1
	}
	return 2
}

// markStart marks pc in a set of pcs of a function of n words, if pc is
// one of its words.
func markStart(set []uint64, n, pc int) {
	if pc < n {
		set[pc>>6] |= 1 << (pc & 63)
	}
}

// isStart reports whether pc is the start of an instruction of a function
// of n words, whose starts are marked in starts.
func isStart(starts []uint64, n, pc int) bool {
	return pc >= 0 && pc < n && starts[pc>>6]&(1<<(pc&63)) != 0
}

// RegisterSpans calls span(r, k) for each run of k registers from r that
// the instruction w (with its first extra word x) names, and reports
// whether op has a known layout. Operands that are not registers (counts,
// immediates, flags, constant and child indexes, environment slots) are
// left out; a count gives the length of the run it describes. A layout
// may name more than an instruction touches (New's this slot, IterClose's
// next method), never less: the engine's checked build fails on a
// register access a layout leaves out.
func RegisterSpans(op Op, w, x uint32, span func(r, k int)) bool {
	a, b, c := int(DecodeA(w)), int(DecodeB(w)), int(DecodeC(w))
	if mask := registerMasks[op]; mask&1 == 0 {
		if mask&0xff00 != 0 {
			span(a, 1)
		}
		if mask&0xff0000 != 0 {
			span(b, 1)
		}
		if mask&0xff000000 != 0 {
			span(c, 1)
		}
		return true
	}
	switch op {
	// A, and the C values from B.
	case NewObjectLit:
		span(a, 1)
		span(b, c)

	// A and the register in the X word, A alone when the X word names an
	// environment slot (EnvOperand, whose low byte is A).
	case JmpLt, JmpLe, JmpGt, JmpGe, JmpNLt, JmpNLe, JmpNGt, JmpNGe:
		span(a, 1)
		if x < EnvOperand {
			span(int(uint8(x)), 1)
		}
	case JmpStrictEq, JmpStrictNe:
		span(a, 1)
		span(int(uint8(x)), 1)
	case GetElemRef:
		span(a, 1)
		span(b, 1)
		span(c, 1)
		span(int(uint8(x)), 1)

	// Runs.
	case UndefRange:
		span(a, b)
	case Call, New, SuperCall: // callee (or constructor), this (or new.target), B arguments
		span(a, 2+b)
	case CallSpread, NewSpread, SuperCallSpread: // callee, this, the argument array
		span(a, 3)
	case ApplyArguments: // callee, this, 2 arguments, and the arguments register
		span(a, 4)
		span(b, 1)
	case RegExpTest: // the result and the argument
		span(a, 1)
		span(int(ExtraHi(x)), 1)
	case CallEval: // spread when C is not 0, as for directEval
		if c != 0 {
			span(a, 3)
		} else {
			span(a, 2+b)
		}
	case CreateClass: // the constructor and its prototype; R[B], the heritage, is read whatever C says
		span(a, 2)
		span(b, 1)
	case GetSuper: // the base and its receiver R[B+1]
		span(a, 1)
		span(b, 2)
		span(c, 1)
	case SetSuper, SetSuperSloppy: // the base and its receiver R[A+1]
		span(a, 2)
		span(b, 1)
		span(c, 1)

	// Iterator records: R[A], R[A+1], and the value in R[A+2] (R[A+3] for
	// for-in, whose record is three registers).
	case IterInit, GenIter, AsyncIterInit:
		span(a, 2)
		span(b, 1)
	case IterNext:
		span(a, 3)
	case IterValue, IterRest, IterThrow:
		span(a, 2)
		span(b, 1)
	case IterClose:
		span(a, 2)
	case ForInInit:
		span(a, 3)
		span(b, 1)
	case ForInNext:
		span(a, 4)
	case AsyncIterNext, AsyncIterResult, AsyncIterClose, AsyncIterClosed:
		span(a, 3)

	// Suspensions: the sent value in R[A] and the resumption mode in R[A+1].
	case Yield, Await, AsyncYield:
		span(a, 2)
		span(b, 1)
	case Delegate:
		span(a, 2)
		span(b, 2)
	case AsyncDelegate:
		span(a, 2)
		span(b, 3)

	default:
		return false
	}
	return true
}

// argumentsRegister is the register of fn's arguments object, after the
// parameters and the rest array.
func argumentsRegister(fn *Function) int {
	if fn.HasRest {
		return int(fn.NumParams) + 1
	}
	return int(fn.NumParams)
}

// paramsInOrder reports whether MapArguments B b may alias the parameters
// of fn lazily: b is 1 and the capture layout puts parameter i in Env slot
// i, which the engine indexes without the layout.
func paramsInOrder(fn *Function, b uint8) bool {
	if b != 1 || len(fn.CaptureLayout) < int(fn.NumParams) {
		return false
	}
	for i := range int(fn.NumParams) {
		if fn.CaptureLayout[i] != uint16(i) {
			return false
		}
	}
	return true
}

// checkNone .. checkCall select the check of an instruction's non-register
// operands in verifyOps. Its control flow and kind operands are checked
// independently.
const (
	checkNone = iota
	checkConst
	checkRegExp
	checkKey
	checkStrict
	checkObject
	checkArray
	checkEnvOperand
	checkEnvDepth
	checkMapArguments
	checkApplyArguments
	checkArguments
	checkCall
	checkMask = 15
)

// registerMasks has the bytes that name single registers in A, B and C
// for those layouts alone. Its low bit is set for every other layout,
// which RegisterSpans checks. Both the verifier and RegisterSpans use it.
var registerMasks = func() (t [256]uint32) {
	for i := range t {
		switch Op(i) {
		// No register.
		case PushEnv, PopEnv, CopyEnv, Jmp, RetUndef, ThrowConstAssign, ThrowError:
			t[i] = 0
		// A.
		case LoadConst, LoadInt, LoadUndef, LoadNull, LoadTrue, LoadFalse, LoadHole, LoadThis, CoerceThis, LoadCallee,
			GetEnv, GetEnv0, GetEnv1, GetEnv2, GetEnvChk, SetEnv, GetEnvW, GetEnvChkW, SetEnvW, CheckTDZ,
			GetGlobal, GetGlobalOrUndef, SetGlobal,
			NewObject, NewArray, NewArrayK, ArrayHole, ArrayPushK, Closure, NewRegExp, GetTemplate,
			RequireObjectCoercible,
			JmpT, JmpF, JmpNullish, JmpNotNullish, JmpNotUndef,
			Ret, Throw,
			CtorEntry, LoadNewTarget, LoadHome, CheckSuper, NewPrivateName,
			GenFunc, GenStart, AsyncFunc, AsyncStart, AsyncGenStart,
			GetImport, GetImportW, ImportMeta,
			SetGlobalSloppy, ResolveGlobal, InitGlobal, SetGlobalVar, DelGlobal, JmpWith, MapArguments,
			JmpLtI, JmpLeI, JmpGtI, JmpGeI, JmpNLtI, JmpNLeI, JmpNGtI, JmpNGeI, JmpStrictEqI, JmpStrictNeI,
			JmpStrictEqK, JmpStrictNeK, JmpTypeofIs, JmpTypeofNot, JmpCaseK:
			t[i] = 0xff00
		// A, B.
		case Move, GetProp, GetLen, SetProp, DelProp, DefineField, SetProto, CopyDataProps, ArrayPush, AppendSpread,
			AddImm, SubImm, Inc, Dec, Neg, Plus, Not, BitNot, ToNumeric, ToStr, Typeof, TypeofIs,
			SetHome, ToPropertyKey, GetProtoOf, DerivedResult, AddBrand,
			AsyncReturn, AsyncThrow, ImportCall,
			SetGlobalRef, SetPropSloppy, DelPropSloppy, ToObject, WithGet, WithSet, ArgsLen, PostInc, PostDec:
			t[i] = 0xffff00
		// A, B, C.
		case GetElem, SetElem, DelElem, In, InstanceOf, DefineElem, DefineMethod, DefineAccessor, CopyDataPropsEx,
			Add, Sub, Mul, Div, Mod, Exp, Concat, BitAnd, BitOr, BitXor, Shl, Shr, UShr,
			Eq, Ne, StrictEq, StrictNe, Lt, Le, Gt, Ge,
			DefineClassMethod, SetPrivateMethod, GetPrivate, SetPrivate, DefPrivate, InPrivate,
			SetElemSloppy, DelElemSloppy, ArgsGet, GetElemInc:
			t[i] = 0xffffff00
		default:
			t[i] = 1
		}
	}
	return t
}()

const (
	verifyIC uint16 = 1 << (4 + iota)
	verifySavesPC
	verifyKindsRead
	verifyJumps
	verifyEnds
	verifyExtra = 9
)

// verifyOps combines the layout facts the verifier's walks read for
// each instruction. Its low four bits select the operand check.
var verifyOps = func() (t [OpCount]uint16) {
	for op := range Op(OpCount) {
		_, constant := constOperand(op)
		_, key := keyOperand(op)
		var v uint16
		switch {
		case constant:
			v = checkConst
		case key:
			v = checkKey
		default:
			switch op {
			case RegExpTest:
				v = checkRegExp
			case JmpStrictEqK, JmpStrictNeK, JmpCaseK:
				v = checkStrict
			case NewObjectLit:
				v = checkObject
			case ArrayPushK, NewArrayK:
				v = checkArray
			case JmpLt, JmpLe, JmpGt, JmpGe, JmpNLt, JmpNLe, JmpNGt, JmpNGe:
				v = checkEnvOperand
			case GetEnv0, GetEnv1, GetEnv2:
				v = checkEnvDepth
			case MapArguments:
				v = checkMapArguments
			case ApplyArguments:
				v = checkApplyArguments
			case ArgsLen, ArgsGet:
				v = checkArguments
			case Call, New:
				v = checkCall
			}
		}
		v |= uint16(op.ExtraWords()) << verifyExtra
		if icOperand(op) {
			v |= verifyIC
		}
		if op.SavesPC() {
			v |= verifySavesPC
		}
		if readsKinds(op) {
			v |= verifyKindsRead
		}
		if op.Jumps() {
			v |= verifyJumps
		}
		if ends(op) {
			v |= verifyEnds
		}
		t[op] = v
	}
	return t
}()

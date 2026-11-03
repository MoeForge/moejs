package bytecode

import (
	"fmt"
	"math/bits"
)

// RegKind is a kind of value the verifier proves a register holds where
// an op reads it as that kind without looking (KindOperands).
type RegKind uint8

const (
	RegAny    RegKind = iota // nothing proven
	RegObject                // an object
	RegArray                 // an Array, which is an object too
	RegIndex                 // a non-negative integer number
)

var regKindNames = [...]string{RegAny: "any value", RegObject: "an object", RegArray: "an array", RegIndex: "a for-in index"}

func (k RegKind) String() string {
	if int(k) < len(regKindNames) {
		return regKindNames[k]
	}
	return fmt.Sprintf("RegKind(%d)", k)
}

// KindOperands calls use(r, kind) for each register the instruction w
// reads as kind without looking at it: the engine's interpreter reads
// DefineField's instance as an object, ArrayPush's literal as an array,
// and the for-in record of ForInNext as an object (the key list) and an
// index (engine run and forInNext).
func KindOperands(op Op, w uint32, use func(r int, kind RegKind)) {
	a := int(DecodeA(w))
	switch op {
	case DefineField:
		use(a, RegObject)
	case ArrayPush:
		use(a, RegArray)
	case ForInNext:
		use(a, RegObject)
		use(a+1, RegIndex)
	}
}

// readsKinds reports whether op has KindOperands.
func readsKinds(op Op) bool { return op == DefineField || op == ArrayPush || op == ForInNext }

// RegisterWrites calls write(r, k, kind) for each run of k registers from
// r that the instruction w (with its first extra word x) may write, and
// reports whether op has a known layout. kind is what the instruction
// leaves in the run whenever it completes, given that its KindOperands
// hold: an object for NewObject and ToObject, an array for NewArray, the
// key list and the index of ForInInit's record and ForInNext's index;
// RegAny for every other write. A row may name more than an instruction
// writes (the argument registers of a call, which a native callee gets as
// a slice of them), never less, and names only registers of the
// instruction's RegisterSpans: the verifier takes a register no row names
// to keep its value (verifyKinds), and the engine's checked build fails on
// an instruction that changes one or completes with another kind.
func RegisterWrites(op Op, w, x uint32, write func(r, k int, kind RegKind)) bool {
	a, b, c := int(DecodeA(w)), int(DecodeB(w)), int(DecodeC(w))
	switch op {
	// No register.
	case PushEnv, PopEnv, CopyEnv, Jmp, RetUndef, ThrowConstAssign, ThrowError, CoerceThis,
		SetEnv, SetEnvW, CheckTDZ, SetGlobal, SetGlobalSloppy, InitGlobal, SetGlobalVar, SetGlobalRef,
		RequireObjectCoercible, Ret, Throw, CheckSuper, GenFunc, AsyncFunc,
		JmpT, JmpF, JmpNullish, JmpNotNullish, JmpNotUndef, JmpWith,
		JmpLt, JmpLe, JmpGt, JmpGe, JmpNLt, JmpNLe, JmpNGt, JmpNGe, JmpStrictEq, JmpStrictNe,
		JmpLtI, JmpLeI, JmpGtI, JmpGeI, JmpNLtI, JmpNLeI, JmpNGtI, JmpNGeI, JmpStrictEqI, JmpStrictNeI,
		SetProp, SetPropSloppy, SetElem, SetElemSloppy, SetSuper, SetSuperSloppy, SetPrivate, WithSet,
		DefineField, DefineElem, DefineMethod, DefineAccessor, DefineClassMethod, SetProto, SetHome,
		CopyDataProps, CopyDataPropsEx, ArrayPush, ArrayHole, AppendSpread,
		SetPrivateMethod, DefPrivate, AddBrand, AsyncIterClosed:

	// A, of the kind it proves.
	case NewObject, ToObject:
		write(a, 1, RegObject)
	case NewArray:
		write(a, 1, RegArray)

	// A.
	case LoadConst, LoadInt, LoadUndef, LoadNull, LoadTrue, LoadFalse, LoadHole, LoadThis, LoadCallee, Move,
		GetEnv, GetEnvChk, GetEnvW, GetEnvChkW, GetGlobal, GetGlobalOrUndef, GetImport, GetImportW, ImportMeta,
		GetProp, GetLen, GetElem, GetSuper, GetPrivate, DelProp, DelElem, DelPropSloppy, DelElemSloppy,
		In, InstanceOf, InPrivate, ResolveGlobal, DelGlobal, WithGet,
		Closure, NewRegExp, GetTemplate, NewPrivateName, MapArguments, ImportCall,
		Add, Sub, Mul, Div, Mod, Exp, AddImm, SubImm, Inc, Dec, Neg, Plus, Not, BitNot, ToNumeric, ToStr,
		Concat, Typeof, TypeofIs, BitAnd, BitOr, BitXor, Shl, Shr, UShr,
		Eq, Ne, StrictEq, StrictNe, Lt, Le, Gt, Ge,
		CtorEntry, LoadNewTarget, LoadHome, ToPropertyKey, GetProtoOf, DerivedResult,
		GenStart, AsyncStart, AsyncGenStart, AsyncReturn, AsyncThrow:
		write(a, 1, RegAny)

	// Runs: a call's whole block, as RegisterSpans names it.
	case UndefRange:
		write(a, b, RegAny)
	case Call, New, SuperCall:
		write(a, 2+b, RegAny)
	case CallSpread, NewSpread, SuperCallSpread:
		write(a, 3, RegAny)
	case CallEval:
		if c != 0 {
			write(a, 3, RegAny)
		} else {
			write(a, 2+b, RegAny)
		}
	case GetElemRef: // the key it converted, and the element
		write(a, 1, RegAny)
		write(c, 1, RegAny)
	case CreateClass: // the constructor and its prototype
		write(a, 2, RegAny)

	// Iterator records.
	case IterInit, GenIter, AsyncIterInit, IterClose, IterThrow:
		write(a, 2, RegAny)
	case IterNext, AsyncIterNext, AsyncIterResult, AsyncIterClose:
		write(a, 3, RegAny)
	case IterValue, IterRest:
		write(a, 2, RegAny)
		write(b, 1, RegAny)
	case ForInInit:
		write(a, 1, RegObject)
		write(a+1, 1, RegIndex)
		write(a+2, 1, RegAny)
	case ForInNext:
		write(a+1, 1, RegIndex)
		write(a+3, 1, RegAny)

	// Suspensions: a resumption writes the value and the mode, and an async
	// function call's first Await leaves its state in R[B].
	case Yield, AsyncYield:
		write(a, 2, RegAny)
	case Await:
		write(a, 2, RegAny)
		write(b, 1, RegAny)
	case Delegate:
		write(a, 2, RegAny)
		write(b, 2, RegAny)
	case AsyncDelegate:
		write(a, 2, RegAny)
		write(b, 3, RegAny)

	default:
		return false
	}
	return true
}

// ends reports whether the instruction after one of op runs only when a
// jump lands on it: op jumps whenever it completes, throws or returns.
func ends(op Op) bool {
	switch op {
	case Jmp, IterClose, Ret, RetUndef, Throw, ThrowError, ThrowConstAssign, IterThrow:
		return true
	}
	return false
}

// verifyKinds proves the kinds KindOperands names, for VerifyRegisters,
// which has checked fn's registers, jumps and handlers and marked its
// instruction starts in starts. At each such operand, every path through
// fn that reaches the instruction leaves a value of that kind in the
// register: the last instruction on the path that writes the register
// writes it as that kind (RegisterWrites).
//
// A path starts where the engine starts a frame of fn, with whatever its
// registers hold (the arguments, and what earlier frames left in the
// others): at pc 0 and at the body of a module. It follows the jumps and
// the fallthroughs, through suspensions and reruns (a frame resumes with
// the registers it saved, but for the value and mode its Yield, Await or
// AsyncYield receives, which their rows write: the engine suspends a
// frame only into the generator or async state it runs, so no other
// function's frame resumes there), and from each instruction
// a handler's range covers to the first such handler (the one the engine
// enters), with the registers as the instruction found them or left them
// (a throw may come before or after a write) and the handler's own
// written. An instruction no path reaches is not checked: the engine
// never runs it.
//
// It is a forward analysis over the basic blocks, of three sets of the
// registers KindOperands names (tracked): those proven an object, an
// array and an index. One walk sums up each block: the registers it
// writes, the facts it leaves in them, those that hold at every point of
// it (for its handler), and the facts its operands need at its entry
// (an operand whose register the block wrote before is checked there).
// A block's entry is the meet of what its predecessors leave and is
// taken on again when it loses a fact, which each register can do at
// most twice. A function below 4,096 words with a few dozen blocks keeps
// its state on the stack.
//
// lead marks the targets of fn's jumps and the instructions after its
// jumps and ends (VerifyRegisters' second walk).
func verifyKinds(fn *Function, starts, lead []uint64, tracked *[4]uint64) error {
	code := fn.Code
	n := len(code)
	nw := n>>6 + 1
	nextStart := func(pc int) int {
		for pc < n && starts[pc>>6]&(1<<(pc&63)) == 0 {
			pc++
		}
		return pc
	}
	markStart(lead, n, 0)
	for _, h := range fn.Handlers {
		markStart(lead, n, int(h.Handler))
		markStart(lead, n, nextStart(int(h.Start)))
		markStart(lead, n, nextStart(int(h.End)))
	}
	if m := fn.Module; m != nil {
		markStart(lead, n, m.BodyPC)
	}

	// Per word of lead the blocks before it (rank); per block its first
	// pc, the block its jump goes to (or -1), the handler that catches its
	// throws (or -1); the tracked registers it writes (kill, W words), and
	// three sets of W words each (object, array, index) for the facts at
	// its entry (in), the facts its writes leave (gen), the registers
	// whose facts hold at every point of it (meet: written only with them,
	// or not at all) and the facts its operands need at its entry (need).
	// Per handler the facts at its entry. Bit sets of the blocks reached,
	// to take on again, that fall through and that have an operand that
	// fails whatever the entry, and of the handlers reached.
	W := (int(fn.NumRegs) + 63) >> 6
	S := 3 * W
	nh := len(fn.Handlers)
	nb := 0
	for _, x := range lead[:nw] {
		nb += bits.OnesCount64(x)
	}
	var ibuf [256]int32
	ints := ibuf[:]
	if k := nw + 3*nb + 2; k > len(ibuf) {
		ints = make([]int32, k)
	}
	rank, bstart := ints[:nw+1], ints[nw+1:nw+nb+2]
	jump, catch := ints[nw+nb+2:nw+2*nb+2], ints[nw+2*nb+2:nw+3*nb+2]
	for i, x := range lead[:nw] {
		rank[i+1] = rank[i] + int32(bits.OnesCount64(x))
	}
	var ubuf [1024]uint64
	words := ubuf[:]
	if k := nb*(4*S+W) + (nh+2)*S + 4*(nb>>6+1) + nh>>6 + 1; k > len(ubuf) {
		words = make([]uint64, k)
	}
	take := func(k int) []uint64 {
		s := words[:k:k]
		words = words[k:]
		return s
	}
	in, gen, meet, need, kill, hin := take(nb*S), take(nb*S), take(nb*S), take(nb*S), take(nb*W), take(nh*S)
	cur, tmp := take(S), take(S)
	reached, dirty, falls, bad, hreached := take(nb>>6+1), take(nb>>6+1), take(nb>>6+1), take(nb>>6+1), take(nh>>6+1)
	set := func(s []uint64, i int) bool { return s[i>>6]&(1<<(i&63)) != 0 }
	blockAt := func(pc int) int { // the block that starts at pc, a leader, or nb past the code
		if pc >= n {
			return nb
		}
		return int(rank[pc>>6]) + bits.OnesCount64(lead[pc>>6]&(1<<(pc&63)-1))
	}

	b := 0
	for i, x := range lead[:nw] {
		for x != 0 {
			bstart[b] = int32(i<<6 | bits.TrailingZeros64(x))
			x &= x - 1
			b++
		}
	}
	bstart[nb] = int32(n)
	for i := range catch {
		catch[i] = -1
	}
	for i := nh - 1; i >= 0; i-- { // the first row covering a block catches its throws (engine findHandler)
		h := fn.Handlers[i]
		for b := blockAt(nextStart(int(h.Start))); b < nb && int(bstart[b]) < int(h.End); b++ {
			catch[b] = int32(i)
		}
	}

	// write applies a write of kind to the tracked registers of the k from
	// r: to the facts s, the registers written kl and the facts m that
	// hold throughout, those not nil.
	write := func(s, kl, m []uint64, r, k int, kind RegKind) {
		has := regFacts[kind]
		for ; k > 0; r, k = r+1, k-1 {
			i, bit := r>>6, uint64(1)<<(r&63)
			if tracked[i]&bit == 0 {
				continue
			}
			if kl != nil {
				kl[i] |= bit
			}
			for j := 0; j < 3; j, i = j+1, i+W {
				if has&(1<<j) != 0 {
					s[i] |= bit
				} else {
					s[i] &^= bit
					if m != nil {
						m[i] &^= bit
					}
				}
			}
		}
	}
	// The sums of the blocks.
	var g, kl, m, nd []uint64
	wild := false // a row named a register past NumRegs: RegisterWrites broke its rule
	sum := func(r, k int, kind RegKind) {
		if r < 0 || r+k > int(fn.NumRegs) {
			wild = true
			return
		}
		for ; k > 0; r, k = r+1, k-1 {
			if tracked[r>>6]&(1<<(r&63)) != 0 {
				write(g, kl, m, r, 1, kind)
			}
		}
	}
	b = 0
	use := func(r int, kind RegKind) {
		j := int(kind) - 1 // the set of kind
		switch i, bit := r>>6, uint64(1)<<(r&63); {
		case kl[i]&bit == 0:
			nd[j*W+i] |= bit
		case g[j*W+i]&bit == 0:
			bad[b>>6] |= 1 << (b & 63)
		}
	}
	for ; b < nb; b++ {
		g, m, nd, kl = gen[b*S:(b+1)*S], meet[b*S:(b+1)*S], need[b*S:(b+1)*S], kill[b*W:(b+1)*W]
		for i := range m {
			m[i] = ^uint64(0)
		}
		jump[b] = -1
		fall := true
		for pc, end := int(bstart[b]), int(bstart[b+1]); pc < end; {
			w := code[pc]
			op := DecodeOp(w)
			if int(op) >= OpCount {
				pc, fall = pc+1, true
				continue
			}
			next := pc + 1 + op.ExtraWords()
			var x uint32
			if next > pc+1 {
				x = code[pc+1]
			}
			if readsKinds(op) {
				KindOperands(op, w, use)
			}
			RegisterWrites(op, w, x, sum)
			if op.Jumps() { // the last of its block
				jump[b] = int32(blockAt(JumpTarget(pc, w)))
			}
			pc, fall = next, !ends(op)
		}
		if fall && int(bstart[b+1]) < n {
			falls[b>>6] |= 1 << (b & 63)
		}
	}
	if wild {
		return fmt.Errorf("bytecode: %q: a write row names a register past its %d", fn.Name, fn.NumRegs)
	}

	// enter meets s into the entry of block b.
	enter := func(b int, s []uint64) {
		e := in[b*S : (b+1)*S]
		if !set(reached, b) {
			reached[b>>6] |= 1 << (b & 63)
			copy(e, s)
			dirty[b>>6] |= 1 << (b & 63)
			return
		}
		for i, v := range s {
			if e[i]&v != e[i] {
				e[i] &= v
				dirty[b>>6] |= 1 << (b & 63)
			}
		}
	}
	// flow takes the facts at the entry of block b to its successors and
	// to the handler that catches its throws.
	flow := func(b int) {
		e, g, kl := in[b*S:(b+1)*S], gen[b*S:(b+1)*S], kill[b*W:(b+1)*W]
		for j := 0; j < S; j += W {
			for i := range W {
				cur[j+i] = e[j+i]&^kl[i] | g[j+i]
			}
		}
		if t := jump[b]; t >= 0 {
			enter(int(t), cur)
		}
		if set(falls, b) {
			enter(b+1, cur)
		}
		h := int(catch[b])
		if h < 0 {
			return
		}
		m, he := meet[b*S:(b+1)*S], hin[h*S:(h+1)*S]
		changed := !set(hreached, h)
		if changed {
			hreached[h>>6] |= 1 << (h & 63)
			for i := range he {
				he[i] = e[i] & m[i]
			}
		} else {
			for i := range he {
				if v := he[i] & e[i] & m[i]; v != he[i] {
					he[i] = v
					changed = true
				}
			}
		}
		if !changed {
			return
		}
		copy(tmp, he)
		row := fn.Handlers[h]
		write(tmp, nil, nil, int(row.Reg), handlerRegs(row.Kind), RegAny)
		enter(blockAt(int(row.Handler)), tmp)
	}

	clear(tmp)
	enter(0, tmp)
	if m := fn.Module; m != nil {
		enter(blockAt(m.BodyPC), tmp)
	}
	for again := true; again; {
		again = false
		for i := range dirty {
			for dirty[i] != 0 {
				j := bits.TrailingZeros64(dirty[i])
				dirty[i] &^= 1 << j
				again = true
				flow(i<<6 | j)
			}
		}
	}

	// The operands of the reached blocks: a block whose operand fails is
	// walked again for the error.
	for b := range nb {
		if !set(reached, b) {
			continue
		}
		fails := set(bad, b)
		for i, v := range need[b*S : (b+1)*S] {
			fails = fails || v&^in[b*S+i] != 0
		}
		if fails {
			return kindError(fn, int(bstart[b]), int(bstart[b+1]), in[b*S:(b+1)*S], W)
		}
	}
	return nil
}

// kindError walks the instructions from pc to end of fn, from the facts
// s at pc (three sets of W words, verifyKinds'), to the first operand of
// KindOperands whose register is not of its kind, and returns its error.
func kindError(fn *Function, pc, end int, s []uint64, W int) error {
	var err error
	for err == nil && pc < end {
		w := fn.Code[pc]
		op := DecodeOp(w)
		if int(op) >= OpCount {
			pc++
			continue
		}
		KindOperands(op, w, func(r int, kind RegKind) {
			if j := int(kind) - 1; err == nil && s[j*W+(r>>6)]&(1<<(r&63)) == 0 {
				err = fmt.Errorf("bytecode: %q pc %d: %s reads register %d as %s, which a path to it leaves another value in", fn.Name, pc, op, r, kind)
			}
		})
		next := pc + 1 + op.ExtraWords()
		var x uint32
		if next > pc+1 {
			x = fn.Code[pc+1]
		}
		RegisterWrites(op, w, x, func(r, k int, kind RegKind) {
			for ; k > 0; r, k = r+1, k-1 {
				for j, i := 0, r>>6; j < 3; j, i = j+1, i+W {
					if regFacts[kind]&(1<<j) != 0 {
						s[i] |= 1 << (r & 63)
					} else {
						s[i] &^= 1 << (r & 63)
					}
				}
			}
		})
		pc = next
	}
	if err == nil { // the sums and this walk disagree
		err = fmt.Errorf("bytecode: %q pc %d: the proof of its kind operands fails at no operand", fn.Name, end)
	}
	return err
}

// regFacts are the sets of verifyKinds a value of each kind is in: bit 0
// the objects, bit 1 the arrays, bit 2 the indexes.
var regFacts = [...]uint8{RegAny: 0, RegObject: 1, RegArray: 3, RegIndex: 4}

package bytecode

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"slices"
	"sync"
	"unsafe"
)

// CallOverlap is the bit of the operand C of Call and New that lets the
// engine start the callee's frame at the call's arguments: the callee's registers begin
// at R[A+2], its parameters are the arguments where the caller left them,
// and nothing is copied. The callee writes its registers over the caller's
// from R[A+2] up, so the compiler sets the bit (MarkOverlapCalls) only on
// a call after which no register from R[A+2] up is read before it is
// written, on any path, the handler's that catches the call's throw
// included, and only in a function whose frame keeps nothing else above
// its call blocks: no generator or async function, which would save the
// callee's values with its registers at a suspension and keep them alive
// as long as it lives, no frame that keeps its arguments above its
// registers (ForwardArguments), and no module body, which may suspend.
// R[A] and R[A+1], the callee and this (New's is unused), stay the
// caller's: the stack walk and the memory measurement read them while the
// callee runs.
// VerifyRegisters proves it of every call that has the bit.
const CallOverlap = 2

// overlapFunction reports whether the calls of fn may have CallOverlap.
func overlapFunction(fn *Function) bool {
	return !fn.Generator && !fn.Async && !fn.ForwardArguments && fn.Kind != KindModule
}

// MarkOverlapCalls sets CallOverlap on every Call and New of fn after
// which the registers from R[A+2] up are dead (CallOverlap), clears it on
// the others, and returns how many it marked. It records that it proved
// the marks (overlapSum), for VerifyRegisters to take them as they are
// while fn's code, handlers and frame stay the same. fn's code is
// VerifyRegisters' to check: on code it rejects (an op of no layout, a
// register past the frame, a jump into the middle of an instruction),
// what it marks does not matter, and it does not fail.
func MarkOverlapCalls(fn *Function) int {
	if !overlapFunction(fn) {
		return 0
	}
	code, n := fn.Code, 0
	if !deadAfterCalls(fn, func(pc int, dead bool) {
		code[pc] &^= CallOverlap << 24
		if dead {
			code[pc] |= CallOverlap << 24
			n++
		}
	}) {
		return 0
	}
	if n > 0 {
		fn.overlapSum = overlapSum(fn)
	}
	return n
}

// overlapSeed keys overlapSum. It is drawn once per process: hash/maphash
// leaves no edit of the code that keeps a sum to be found without it.
var overlapSeed = maphash.MakeSeed()

// overlapSum sums what a proof of fn's CallOverlap marks rests on: its
// code, handlers, registers and the kind of function it is
// (overlapFunction), with hash/maphash under overlapSeed. It is never 0.
func overlapSum(fn *Function) uint64 {
	var h maphash.Hash
	h.SetSeed(overlapSeed)
	var buf [16]byte
	flags := byte(0)
	for i, v := range [...]bool{fn.Generator, fn.Async, fn.ForwardArguments, fn.Kind == KindModule} {
		if v {
			flags |= 1 << i
		}
	}
	b := binary.LittleEndian.AppendUint32(buf[:0], uint32(len(fn.Code)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(fn.Handlers)))
	b = binary.LittleEndian.AppendUint16(b, fn.NumRegs)
	h.Write(append(b, flags))
	// The code's byte order is this process's, as the seed is. Reading
	// its bytes directly avoids copying each word into a second buffer.
	h.Write(unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(fn.Code))), 4*len(fn.Code)))
	for _, r := range fn.Handlers {
		b = binary.LittleEndian.AppendUint32(buf[:0], r.Start)
		b = binary.LittleEndian.AppendUint32(b, r.End)
		b = binary.LittleEndian.AppendUint32(b, r.Handler)
		b = binary.LittleEndian.AppendUint16(b, r.Reg)
		h.Write(append(b, byte(r.Kind)))
	}
	return h.Sum64() | 1
}

// verifyOverlap proves CallOverlap of every Call and New of fn that has
// it, for VerifyRegisters, which has checked fn's registers, jumps and
// handlers and found one. It fails if deadAfterCalls gives up on fn.
func verifyOverlap(fn *Function) error {
	if !overlapFunction(fn) {
		return fmt.Errorf("bytecode: %q: a call overlaps its callee's frame in a generator, async function, module body or a function that forwards its arguments", fn.Name)
	}
	var err error
	if !deadAfterCalls(fn, func(pc int, dead bool) {
		if err == nil && !dead && DecodeC(fn.Code[pc])&CallOverlap != 0 {
			err = fmt.Errorf("bytecode: %q pc %d: %s overlaps its callee's frame with a register from R[A+2] live after it", fn.Name, pc, DecodeOp(fn.Code[pc]))
		}
	}) {
		// The analysis gave up on fn, which VerifyRegisters found a call
		// with the bit in: it proves no mark.
		return fmt.Errorf("bytecode: %q: a call overlaps its callee's frame where the analysis of its registers proves nothing", fn.Name)
	}
	return err
}

// deadAfterCalls calls visit(pc, dead) for every call (Call or New) of
// fn, in order, dead reporting whether no register from R[A+2] up is live
// after it: read on some path from the call's next instruction, or from
// the handler that catches its throw, before an instruction writes it
// whenever it completes. It reports false, and visits nothing, if fn has
// no call, an op of no layout or an instruction past its code's end. On a
// jump into the middle of an instruction what it visits does not matter,
// and it does not fail.
//
// It is a backward analysis over the instructions: an instruction has
// live at its start what it reads, what is live after it that it does
// not write, and what the handler that catches its throws has live, which
// a throw from it reaches with the registers as they were (the handler's
// entry writes its own registers). After an instruction is live what its
// next instruction has, unless it never continues there, and what the
// instruction it may jump to has. One pass from the last instruction up
// (livePass) is exact when no jump or handler goes up; otherwise passes
// repeat until one changes nothing at an instruction that a jump or a
// throw from below may continue at. The analysis keeps the registers in
// words of 64 (liveRegs), from register 0 up to fn's last; a word below
// the lowest A+2 of the calls decides nothing. It reads the instructions
// from the first call down and those above it that one of them, or a
// throw from one, may continue at: no other is on a path from a call.
func deadAfterCalls(fn *Function, visit func(pc int, dead bool)) bool {
	code := fn.Code
	n := len(code)
	start := firstCall(code)
	if start >= n {
		return false
	}
	sc := overlapScratchPool.Get().(*overlapScratch)
	defer overlapScratchPool.Put(sc)
	sc.facts = slices.Grow(sc.facts[:0], n)[:n]
	sc.live = slices.Grow(sc.live[:0], n+1)[:n+1]
	sc.back = slices.Grow(sc.back[:0], n>>6+1)[:n>>6+1]
	sc.kept = slices.Grow(sc.kept[:0], n>>6+1)[:n>>6+1]
	sc.calls = slices.Grow(sc.calls[:0], n)[:n]
	sc.rows = slices.Grow(sc.rows[:0], len(fn.Handlers))[:len(fn.Handlers)]
	facts, live, back, kept, rows := sc.facts, sc.live, sc.back, sc.kept, sc.rows
	clear(kept)
	var calls []int32
	for base := 0; ; base += 64 {
		// The instructions from the first call down, then from the
		// lowest pc an instruction or a throw from them may continue at
		// up to the last walked, until none continues lower.
		clear(back)
		nc, up, from, stop, low := 0, false, start, n, start
		for {
			for pc := from; ; {
				var u bool
				if pc, nc, low, u = walkFacts(code, pc, stop, 256-base, facts, sc.calls, nc, back, low); pc >= stop {
					up = up || u
					if pc > stop {
						return false
					}
					break
				}
				up = up || u
				// An op walkFacts leaves: one of spans of registers or of
				// no layout.
				w := code[pc]
				k := &opWalks[uint8(w)]
				if k.flags&walkNone != 0 || pc+int(k.step) > n {
					return false
				}
				var x uint32
				if k.step > 1 {
					x = code[pc+1]
					for e := pc + 1; e < pc+int(k.step); e++ {
						facts[e].jump = extraWord
					}
				}
				reads, writes := regEffectSpans(Op(uint8(w)), w, x, base)
				t := n
				if k.flags&walkJumps != 0 {
					if t = int(min(uint(JumpTarget(pc, w)), uint(n))); t <= pc {
						back[t>>6] |= 1 << (t & 63)
						up = true
						low = min(low, t)
					}
				}
				if k.flags&walkEnds != 0 {
					t = ^t
				}
				facts[pc] = instrFacts{reads: reads, writes: writes, jump: int32(t), catch: -1}
				pc += int(k.step)
			}
			for _, h := range fn.Handlers {
				if int(h.Start) < n && int(h.End) > low {
					low = min(low, int(h.Handler))
				}
			}
			if low >= from {
				break
			}
			from, stop = low, from
		}
		calls = sc.calls[:nc]
		if from < start { // calls below the first walked
			slices.Sort(calls)
		}
		lo := MaxRegister + 3
		for _, pc := range calls {
			lo = min(lo, int(DecodeA(code[pc]))+2)
		}
		if lo < base+64 { // a call decides a register of this word
			for r := len(fn.Handlers) - 1; r >= 0; r-- { // the first row covering an instruction catches its throws
				h := fn.Handlers[r]
				c := min(int(h.Handler), n)
				rows[r] = overlapRow{int32(c), liveRegs(int(h.Reg), handlerRegs(h.Kind), base)}
				for pc := max(int(h.Start), from); pc < min(int(h.End), n); pc++ {
					facts[pc].catch = int32(r)
				}
				if c < int(h.End) {
					back[c>>6] |= 1 << (c & 63)
					up = true
				}
			}
			clear(live)
			for livePass(facts, from, live, back, rows) && up {
			}
			for i, pc := range calls {
				after := live[pc+1] // a call never jumps
				if r := facts[pc].catch; r >= 0 {
					after |= live[rows[r].handler] &^ rows[r].kill
				}
				if h := int(DecodeA(code[pc])) + 2 - base; h < 64 && after>>max(h, 0) != 0 { // else its registers from R[A+2] up are in the next words
					kept[i>>6] |= 1 << (i & 63)
				}
			}
		}
		if base+64 >= min(int(fn.NumRegs), MaxRegister+1) {
			break
		}
	}
	for i, pc := range calls {
		visit(int(pc), kept[i>>6]&(1<<(i&63)) == 0)
	}
	return true
}

// firstCall returns the pc of the first call of code, or past its end if
// it has none, an op of no layout before it or an instruction past its
// end.
func firstCall(code []uint32) int {
	pc := 0
	for pc < len(code) {
		w := code[pc]
		if op := Op(uint8(w)); op == Call || op == New {
			return pc
		}
		k := opBits[uint8(w)]
		if k&(walkNone<<8) != 0 {
			break
		}
		pc += int(k & 255)
	}
	return len(code) + 1
}

// instrFacts is what deadAfterCalls knows of the instruction at a pc, of
// a word of registers: those it may read and those it writes whenever it
// completes; the pc it may jump to, or the code's end, ^ that if it never
// continues at its next instruction, or extraWord at a word past an
// instruction's first; and the row of the handlers that catches its
// throws, or -1.
type instrFacts struct {
	reads, writes uint64
	jump, catch   int32
}

// extraWord is the jump of an instrFacts of a word past an instruction's
// first.
const extraWord = -1 << 31

// overlapRow is what deadAfterCalls knows of a row of fn.Handlers: the
// pc of its handler, or the code's end, and the registers the handler's
// entry writes.
type overlapRow struct {
	handler int32
	kill    uint64
}

// walkFacts records in facts, at the pc of each instruction of code from
// pc, its instrFacts of the 64 registers from 256-off, a multiple of 64,
// in calls from the nc-th the pc of each call and in back the pc an
// instruction may jump up to, until stop or an op of spans of registers
// or of no layout. It returns the pc it stops at, past stop if an
// instruction runs past it, how many calls it has recorded, the lowest of
// low and the pcs an instruction may jump up to, and whether one may. It
// calls nothing, so that it keeps its state in registers.
func walkFacts(code []uint32, pc, stop, off int, facts []instrFacts, calls []int32, nc int, back []uint64, low int) (int, int, int, bool) {
	n := len(code)
	facts = facts[:n]
	stop = min(stop, n)
	up := false
	for pc < stop {
		w := code[pc]
		k := opBits[uint8(w)] // keep the masks above bit 31 on 32-bit hosts
		a := int(uint8(w>>8)) + off
		reads := bitAt[a&int(k>>16)&511] | bitAt[(int(uint8(w>>16))+off)&int(k>>25)&511] | bitAt[(int(w>>24)+off)&int(k>>34)&511]
		writes := bitAt[a&int(k>>43)&511]
		t := n
		if k&(walkSlow<<8) != 0 {
			if k&(walkCall<<8) != 0 { // R[A], this but for New's and a Call's with CallUndefThis, and the B arguments from R[A+2]
				r := a - 256
				reads = lowRegs[min(max(r+2+int(uint8(w>>16)), 0), 64)] &^ lowRegs[min(max(r, 0), 64)]
				if Op(uint8(w)) == New || w>>24&CallUndefThis != 0 {
					reads &^= bitAt[(a+1)&511]
				}
				writes = bitAt[a&511]
				if uint(nc) < uint(len(calls)) {
					calls[nc] = int32(pc)
					nc++
				}
			} else if k&(walkOne<<8) == 0 {
				break
			}
			if k&(walkJumps<<8) != 0 {
				if t = int(min(uint(JumpTarget(pc, w)), uint(n))); t <= pc && t>>6 < len(back) {
					back[t>>6] |= 1 << (t & 63)
					up = true
					low = min(low, t)
				}
			}
			if k&(walkEnds<<8) != 0 {
				t = ^t
			}
		}
		facts[pc] = instrFacts{reads: reads, writes: writes, jump: int32(t), catch: -1}
		e := pc + int(k&255)
		for pc++; pc < min(e, n); pc++ {
			facts[pc].jump = extraWord
		}
		pc = e
	}
	return pc, nc, low, up
}

// livePass runs a pass of deadAfterCalls' analysis over facts, from the
// last instruction up to the one at from, and reports whether it changed
// what an instruction back has has live at its start: one that a jump or
// a throw from below may continue at. live has what the instruction at
// each pc has live at its start and, at the code's end, nothing.
func livePass(facts []instrFacts, from int, live, back []uint64, rows []overlapRow) bool {
	live = live[:len(facts)+1]
	from = max(from, 0)
	back = back[:len(facts)>>6+1]
	var changed uint64
	l := uint64(0) // what the instruction after the one at pc has live
	for pc := len(facts); pc > from; {
		pc--
		f := &facts[pc]
		t := f.jump
		if t < 0 {
			if t == extraWord {
				continue
			}
			l, t = 0, ^t
		}
		in := f.reads | (l|live[t])&^f.writes
		if r := f.catch; r >= 0 {
			in |= live[rows[r].handler] &^ rows[r].kill
		}
		if in != live[pc] {
			live[pc] = in
			changed |= back[pc>>6] & (1 << (pc & 63))
		}
		l = in
	}
	return changed != 0
}

// opBits has, for every op's number, what deadAfterCalls reads of its
// opWalk in one word: the step in the low byte, the flags in the next,
// and from bit 16 a mask of 9 bits of each of A, B, C and A's write, for
// bitAt's index: 511 if the op reads (writes) it, else 0.
var opBits = func() (t [256]uint64) {
	m := func(v int8) uint64 { return uint64(v) & 511 }
	for i, k := range opWalks {
		t[i] = uint64(k.step) | uint64(k.flags)<<8 | m(k.mA)<<16 | m(k.mB)<<25 | m(k.mC)<<34 | m(k.mW)<<43
	}
	return t
}()

// bitAt[256+i] is register base+i in a word of liveRegs from base, for
// -256 <= i < 256: 0 unless 0 <= i < 64. bitAt[0] is 0, which walkFacts'
// masks pick for an operand an op does not read.
var bitAt = func() (t [512]uint64) {
	for i := range 64 {
		t[256+i] = 1 << i
	}
	return t
}()

// overlapScratch is the memory deadAfterCalls takes from
// overlapScratchPool.
type overlapScratch struct {
	facts            []instrFacts
	live, back, kept []uint64
	calls            []int32
	rows             []overlapRow
}

var overlapScratchPool = sync.Pool{New: func() any { return new(overlapScratch) }}

// liveRegs returns registers r..r+k-1 in a set of the 64 registers from
// base that deadAfterCalls keeps in a word: bit i is register base+i.
func liveRegs(r, k, base int) uint64 {
	return lowRegs[min(max(r+k-base, 0), 64)] &^ lowRegs[min(max(r-base, 0), 64)]
}

// lowRegs[k] has the first k registers of a word of liveRegs.
var lowRegs = func() (t [65]uint64) {
	for k := range t {
		t[k] = 1<<k - 1
	}
	return t
}()

// pureDest reports whether op writes its A register whenever it completes
// and reads it as no operand: R[A] is the result of the op and nothing
// else. A register its other operands name too is read through them.
func pureDest(op Op) bool {
	switch op {
	case LoadConst, LoadInt, LoadUndef, LoadNull, LoadTrue, LoadFalse, LoadHole, LoadThis, CoerceThis, LoadCallee, Move,
		GetEnv, GetEnv0, GetEnv1, GetEnv2, GetEnvChk, GetEnvW, GetEnvChkW, GetGlobal, GetGlobalOrUndef, GetImport, GetImportW, ImportMeta,
		GetProp, GetLen, GetElem, GetSuper, GetPrivate, DelProp, DelElem, DelPropSloppy, DelElemSloppy,
		In, InstanceOf, InPrivate, DelGlobal, WithGet,
		Closure, NewRegExp, GetTemplate, NewPrivateName, NewObject, NewArray, NewArrayK, NewObjectLit,
		Add, Sub, Mul, Div, Mod, Exp, AddImm, SubImm, Inc, Dec, Neg, Plus, Not, BitNot, ToNumeric, ToStr,
		Concat, Typeof, TypeofIs, BitAnd, BitOr, BitXor, Shl, Shr, UShr,
		Eq, Ne, StrictEq, StrictNe, Lt, Le, Gt, Ge,
		CtorEntry, LoadNewTarget, LoadHome, ToPropertyKey, GetProtoOf, ToObject, ArgsLen:
		return true
	}
	return false
}

// regsForm is what deadAfterCalls knows of an op, read once (opRegs):
// the operands of up to four spans of registers its instructions may read
// (RegisterSpans, but for the A of a pureDest op, the run UndefRange
// clears, New's this slot and that of a Call with CallUndefThis) and of
// one span they write whenever they complete (the A of a pureDest op,
// UndefRange's run, the result of a Call or a New), for an op whose
// instructions all span their registers the same way; ask for one whose
// reads depend on its operands' values too (a compare-and-branch's
// environment operand, CallEval's spread), and none for one of no
// layout.
//
// one says the same of an op that reads A, B and C, each one register,
// or some of them, and writes A or nothing (oneA .. oneWritesA), for
// walkFacts' fast path; fast reports whether the op is one of those.
type regsForm struct {
	none, ask, fast bool
	one             uint8
	reads           [4]regSpan
	writes          regSpan
}

const (
	oneA = 1 << iota
	oneB
	oneC
	oneWritesA
)

// regSpan names a span of registers by the operand of its first register
// (spanA .. spanA2) and its length (lenNone .. lenThis).
type regSpan struct{ first, len uint8 }

const (
	spanA   = iota
	spanB   //
	spanC   //
	spanXLo // the X word's low byte
	spanXHi // ExtraHi of the X word
	spanA1  // A+1
	spanA2  // A+2
)

const (
	lenNone = iota // no register
	len1
	len2
	len3
	len4
	lenB    // B
	lenC    // C
	lenB2   // B+2
	lenThis // 1, or 0 with CallUndefThis
)

// opRegs is the regsForm of every op, its reads read from RegisterSpans
// on three instructions whose operands, and the lengths they give, differ
// from each other and from 1 .. 4: the second's X word is below
// EnvOperand and the third's C is 0.
var opRegs = func() (t [OpCount]regsForm) {
	probes := [...][5]int{ // A, B, C, the X word's low byte, ExtraHi of the X word
		{10, 7, 5, 20, 30},
		{40, 11, 17, 21, 0},
		{50, 9, 0, 22, 33},
	}
	for op := range Op(OpCount) {
		f := &t[op]
		var reads []regSpan
		for i, p := range probes {
			var got []regSpan
			ask := false
			w := EncodeABC(op, uint8(p[0]), uint8(p[1]), uint8(p[2]))
			ok := RegisterSpans(op, w, uint32(p[3]|p[4]<<16), func(r, k int) {
				first, length := -1, -1
				for j, v := range p {
					if v == r {
						first = j
					}
				}
				switch {
				case k >= 1 && k <= 4:
					length = len1 + k - 1
				case k == p[1]:
					length = lenB
				case k == p[2]:
					length = lenC
				case k == p[1]+2:
					length = lenB2
				}
				ask = ask || first < 0 || length < 0
				got = append(got, regSpan{uint8(first), uint8(length)})
			})
			switch {
			case !ok:
				f.none = true
			case i == 0:
				reads = got
			}
			f.ask = f.ask || ask || !slices.Equal(got, reads)
		}
		if pureDest(op) { // R[A] is written, not read
			f.writes = regSpan{spanA, len1}
			for i, s := range reads {
				if s == f.writes {
					reads = append(reads[:i:i], reads[i+1:]...)
					break
				}
			}
		}
		switch op {
		case UndefRange:
			reads, f.writes = nil, regSpan{spanA, lenB}
		case Call:
			reads, f.writes = []regSpan{{spanA, len1}, {spanA1, lenThis}, {spanA2, lenB}}, regSpan{spanA, len1}
		case New:
			reads, f.writes = []regSpan{{spanA, len1}, {spanA2, lenB}}, regSpan{spanA, len1}
		}
		f.ask = !f.none && (f.ask || len(reads) > len(f.reads))
		if !f.ask {
			copy(f.reads[:], reads)
		}
		f.fast = !f.ask && !f.none && (f.writes == regSpan{} || f.writes == regSpan{spanA, len1})
		if f.writes.len != lenNone {
			f.one = oneWritesA
		}
		for _, s := range reads {
			if s.first > spanC || s.len != len1 {
				f.fast = false
				break
			}
			f.one |= 1 << s.first
		}
	}
	return t
}()

// opWalk is what deadAfterCalls reads of an op (opWalks): the words of
// its instructions, the flags walkOne .. walkSlow and, for one of
// walkOne, a mask of each of A, B and C, all ones if it is read and 0 if
// not, and one of A for its write.
type opWalk struct {
	step, flags    uint8
	mA, mB, mC, mW int8
}

const (
	walkOne   = 1 << iota // regsForm's fast
	walkNone              // regsForm's none
	walkJumps             // Jumps
	walkEnds              // ends
	walkCall              // Call or New
	walkSlow              // any of walkNone .. walkCall, or not walkOne
)

// opWalks is the opWalk of every op's number, an op of no number's too:
// one faults before it touches a register.
var opWalks = func() (t [256]opWalk) {
	mask := func(v bool) int8 {
		if v {
			return -1
		}
		return 0
	}
	for i := range t {
		k := &t[i]
		k.step, k.flags = 1, walkOne
		if i >= OpCount {
			continue
		}
		op, f := Op(i), &opRegs[i]
		k.step, k.flags = uint8(1+op.ExtraWords()), 0
		k.mA, k.mB, k.mC, k.mW = mask(f.one&oneA != 0), mask(f.one&oneB != 0), mask(f.one&oneC != 0), mask(f.one&oneWritesA != 0)
		for flag, v := range map[uint8]bool{
			walkNone:  f.none,
			walkJumps: op.Jumps(),
			walkEnds:  ends(op),
			walkCall:  op == Call || op == New,
		} {
			if v {
				k.flags |= flag | walkSlow
			}
		}
		if f.fast {
			k.flags |= walkOne
		} else {
			k.flags |= walkSlow
		}
	}
	return t
}()

// regEffectSpans returns the registers of the 64 from base, a multiple of
// 64, that the instruction w of an op that reads or writes a run of
// registers, or a register an extra word names, with its first extra
// word x, may read and those it writes whenever it completes (regsForm).
func regEffectSpans(op Op, w, x uint32, base int) (reads, writes uint64) {
	f := &opRegs[op]
	if f.ask {
		return regEffectAsk(op, w, x, base)
	}
	a, b, c := int(DecodeA(w)), int(DecodeB(w)), int(DecodeC(w))
	this := 1
	if c&CallUndefThis != 0 {
		this = 0
	}
	first := [...]int{a, b, c, int(uint8(x)), int(ExtraHi(x)), a + 1, a + 2}
	length := [...]int{0, 1, 2, 3, 4, b, c, b + 2, this}
	for _, s := range f.reads {
		reads |= liveRegs(first[s.first], length[s.len], base)
	}
	return reads, liveRegs(first[f.writes.first], length[f.writes.len], base)
}

// regEffectAsk is regEffectSpans' of an op that asks RegisterSpans.
func regEffectAsk(op Op, w, x uint32, base int) (reads, writes uint64) {
	a := int(DecodeA(w))
	skip := opRegs[op].writes.len != lenNone // a pureDest op's R[A] is written, not read
	if skip {
		writes = liveRegs(a, 1, base)
	}
	RegisterSpans(op, w, x, func(r, k int) {
		if skip && r == a && k == 1 {
			skip = false
			return
		}
		reads |= liveRegs(r, k, base)
	})
	return reads, writes
}

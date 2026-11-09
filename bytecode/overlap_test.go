package bytecode

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOverlapSum checks that the sum changes with what the proof of the
// marks rests on, and that equal functions have the same, nonzero sum.
func TestOverlapSum(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	long := make([]uint32, 129)
	for i := range long {
		long[i] = rnd.Uint32()
	}
	cases := []*Function{
		{},
		{NumRegs: 4, Code: []uint32{EncodeABC(RetUndef, 0, 0, 0)}},
		{NumRegs: 8, Code: []uint32{EncodeABC(Call, 0, 0, CallOverlap), EncodeABC(Ret, 0, 0, 0)},
			Handlers: []Handler{{Start: 0, End: 1, Handler: 1, Kind: HandlerCatch, Reg: 2}}},
		{NumRegs: 256, Code: long, Generator: true, Async: true, ForwardArguments: true, Kind: KindModule,
			Handlers: []Handler{{Start: 1, End: 63, Handler: 64, Kind: HandlerFinally, Reg: 7},
				{Start: 65, End: 127, Handler: 128, Kind: HandlerCatch, Reg: 200}}},
	}
	clone := func(fn *Function) *Function {
		f := *fn
		f.Code = slices.Clone(fn.Code)
		f.Handlers = slices.Clone(fn.Handlers)
		return &f
	}
	for i, fn := range cases {
		sum := overlapSum(fn)
		assert.NotZero(t, sum, "case %d", i)
		assert.Equal(t, sum, overlapSum(clone(fn)), "case %d", i)
		changed := func(f *Function) {
			got := overlapSum(f)
			assert.NotZero(t, got, "case %d", i)
			assert.NotEqual(t, sum, got, "case %d", i)
		}
		for pc := range fn.Code {
			f := clone(fn)
			f.Code[pc] ^= 1
			changed(f)
		}
		for h := range fn.Handlers {
			for _, change := range []func(*Handler){
				func(h *Handler) { h.Start ^= 1 },
				func(h *Handler) { h.End ^= 1 },
				func(h *Handler) { h.Handler ^= 1 },
				func(h *Handler) { h.Kind ^= 1 },
				func(h *Handler) { h.Reg ^= 1 },
			} {
				f := clone(fn)
				change(&f.Handlers[h])
				changed(f)
			}
		}
		for _, change := range []func(*Function){
			func(f *Function) { f.NumRegs ^= 1 },
			func(f *Function) { f.Code = append(f.Code, 0) },
			func(f *Function) { f.Handlers = append(f.Handlers, Handler{}) },
			func(f *Function) { f.Generator = !f.Generator },
			func(f *Function) { f.Async = !f.Async },
			func(f *Function) { f.ForwardArguments = !f.ForwardArguments },
			func(f *Function) {
				if f.Kind == KindModule {
					f.Kind = KindNormal
				} else {
					f.Kind = KindModule
				}
			},
		} {
			f := clone(fn)
			change(f)
			changed(f)
		}
		if len(fn.Code) > 0 {
			for range 1000 {
				f := clone(fn)
				f.Code[rnd.IntN(len(f.Code))] ^= 1 << rnd.IntN(32)
				changed(f)
			}
		}
	}
}

// TestOverlapAnalysisFailure checks that an analysis that gives up proves
// no mark, and that the verifier rejects it.
func TestOverlapAnalysisFailure(t *testing.T) {
	fn := &Function{Name: "f", NumRegs: 4, Code: []uint32{
		EncodeABC(Call, 0, 0, CallUndefThis|CallOverlap), EncodeABC(GetProp, 1, 0, 0)}}
	assert.False(t, deadAfterCalls(fn, func(int, bool) { t.Error("the analysis visited a call") }))
	assert.ErrorContains(t, verifyOverlap(fn), "the analysis of its registers proves nothing")
	assert.Zero(t, MarkOverlapCalls(fn))
	assert.Zero(t, fn.overlapSum)
}

// TestOverlapNoMarks checks that a proof with no marks records no sum:
// VerifyRegisters has no mark to take without proving it again.
func TestOverlapNoMarks(t *testing.T) {
	fn := &Function{Name: "f", NumRegs: 4, Code: []uint32{
		EncodeABC(Call, 0, 1, CallUndefThis), EncodeABC(Ret, 2, 0, 0)}}
	assert.Zero(t, MarkOverlapCalls(fn))
	assert.Zero(t, fn.overlapSum)
	assert.NoError(t, VerifyRegisters(fn))
}

// TestOverlapKinds checks that a call's marks cannot preserve kind facts
// about the registers its callee may write, even if the sum takes the
// marks without proving them again.
func TestOverlapKinds(t *testing.T) {
	abc, abx, asbx := EncodeABC, EncodeABx, EncodeAsBx
	obj := func(r uint8) uint32 { return abx(NewObject, r, 0) }
	arr := func(r uint8) uint32 { return abx(NewArray, r, 0) }
	def := func(r uint8) []uint32 { return []uint32{abc(DefineField, r, 1, 0), 0} }
	push := func(r uint8) uint32 { return abc(ArrayPush, r, 1, 0) }
	ret := abc(RetUndef, 0, 0, 0)
	cat := func(parts ...any) (code []uint32) {
		for _, p := range parts {
			switch p := p.(type) {
			case uint32:
				code = append(code, p)
			case []uint32:
				code = append(code, p...)
			}
		}
		return code
	}
	for _, op := range []Op{Call, New} {
		c := uint8(CallOverlap)
		if op == Call {
			c |= CallUndefThis
		}
		call := abc(op, 0, 0, c)
		for _, c := range []struct {
			name     string
			code     []uint32
			pc       int
			handlers []Handler
		}{
			{"an object", cat(obj(2), call, def(2), ret), 1, nil},
			{"an array", cat(arr(2), call, push(2), ret), 1, nil},
			{"an array and a constant", cat(arr(7), call, abc(ArrayPushK, 7, 0, 0), ExtraArg(0, 1), ret), 1, nil},
			{"a for-in record", cat(obj(6), abc(ForInInit, 2, 6, 0), call, asbx(ForInNext, 2, 0), ret), 2, nil},
			{"a catch reads the object", cat(obj(2), call, ret, def(2), ret), 1,
				[]Handler{{Start: 1, End: 2, Handler: 3, Kind: HandlerCatch, Reg: 1}}},
		} {
			fn := &Function{Name: "f", NumRegs: 8, Consts: nameConst, ICCount: 1, Code: c.code, Handlers: c.handlers}
			fn.overlapSum = overlapSum(fn)
			assert.ErrorContains(t, VerifyRegisters(fn), "which a path to it leaves another value in", "%s: %s", op, c.name)
			fn.overlapSum = 0
			assert.ErrorContains(t, VerifyRegisters(fn), "live after it", "%s: %s", op, c.name)
			fn.Code[c.pc] &^= CallOverlap << 24
			assert.NoError(t, VerifyRegisters(fn), "%s: %s without overlap", op, c.name)
		}
	}
}

// TestOverlapKindsWideFrames checks the first and last registers of the
// words an overlapping call clears, and that a real mark keeps a kind
// below the call's registers.
func TestOverlapKindsWideFrames(t *testing.T) {
	for _, op := range []Op{Call, New} {
		for _, r := range []uint8{2, 63, 64, 127, 128, 255} {
			fn := &Function{Name: "f", NumRegs: uint16(r) + 1, Consts: nameConst, ICCount: 1, Code: []uint32{
				EncodeABx(NewObject, r, 0), EncodeABC(op, r-2, 0, CallOverlap),
				EncodeABC(DefineField, r, 1, 0), 0, EncodeABC(RetUndef, 0, 0, 0)}}
			fn.overlapSum = overlapSum(fn)
			assert.ErrorContains(t, VerifyRegisters(fn), "which a path to it leaves another value in", "%s R[%d]", op, r)
			fn.Code[1] &^= CallOverlap << 24
			assert.NoError(t, VerifyRegisters(fn), "%s R[%d] without overlap", op, r)
			if r > 2 {
				fn.Code[0] = EncodeABx(NewObject, r-3, 0)
				fn.Code[2] = EncodeABC(DefineField, r-3, 1, 0)
				assert.Equal(t, 1, MarkOverlapCalls(fn), "%s R[%d] below the call", op, r-3)
				assert.NoError(t, VerifyRegisters(fn), "%s R[%d] below the call", op, r-3)
			}
		}
	}
}

// TestOverlapCalls checks which calls MarkOverlapCalls marks and that
// VerifyRegisters takes the mark only where the registers from R[A+2] up
// are dead after the call: read on no path before a write, the path
// through the handler that catches the call's throw included.
func TestOverlapCalls(t *testing.T) {
	ret := func(r uint8) uint32 { return EncodeABC(Ret, r, 0, 0) }
	call := func(a, b uint8) uint32 { return EncodeABC(Call, a, b, 0) }
	cases := []struct {
		name     string
		n        int
		code     []uint32
		handlers []Handler
		dead     string // per call, in order: 'y' dead after it, 'n' not
	}{
		{"result returned", 4, []uint32{call(0, 2), ret(0)}, nil, "y"},
		{"a register below the block", 4, []uint32{call(1, 1), ret(0)}, nil, "y"},
		{"this read after", 4, []uint32{call(0, 1), ret(1)}, nil, "y"},
		{"an argument read after", 4, []uint32{call(0, 1), ret(2)}, nil, "n"},
		{"a register past the block read after", 5, []uint32{EncodeAsBx(LoadInt, 4, 1), call(0, 1), ret(4)}, nil, "n"},
		{"written before it is read", 5, []uint32{call(0, 1), EncodeAsBx(LoadInt, 4, 1), ret(4)}, nil, "y"},
		{"read and written", 5, []uint32{call(0, 1), EncodeABC(Add, 4, 4, 0), ret(4)}, nil, "n"},
		{"cleared", 6, []uint32{call(0, 1), EncodeABC(UndefRange, 3, 3, 0), ret(5)}, nil, "y"},
		{"read on a branch", 5, []uint32{call(0, 1), EncodeAsBx(JmpT, 0, 1), ret(4), ret(0)}, nil, "n"},
		{"read around a loop", 5, []uint32{EncodeAsBx(LoadInt, 4, 1), call(0, 1), EncodeAsBx(JmpT, 4, -3), ret(0)}, nil, "n"},
		{"the handler reads it", 5, []uint32{call(0, 1), ret(0), ret(4)},
			[]Handler{{Start: 0, End: 1, Handler: 2, Kind: HandlerCatch, Reg: 0}}, "n"},
		{"the handler's own register", 5, []uint32{call(0, 1), ret(0), ret(4)},
			[]Handler{{Start: 0, End: 1, Handler: 2, Kind: HandlerCatch, Reg: 4}}, "y"},
		{"a finally's two registers", 5, []uint32{call(0, 1), ret(0), ret(4)},
			[]Handler{{Start: 0, End: 1, Handler: 2, Kind: HandlerFinally, Reg: 3}}, "y"},
		{"a plain call's this slot", 5, []uint32{call(0, 0), EncodeABC(Call, 1, 0, CallUndefThis), ret(1)}, nil, "yy"},
		{"a method call's this slot", 5, []uint32{call(0, 0), EncodeABC(Call, 1, 0, 0), ret(1)}, nil, "ny"},
		{"the handler of another range", 5, []uint32{call(0, 1), ret(0), EncodeABC(Throw, 0, 0, 0), ret(4)},
			[]Handler{{Start: 2, End: 3, Handler: 3, Kind: HandlerCatch, Reg: 0}}, "y"},
		{"a covered instruction after the call", 5, []uint32{call(0, 1), ret(0), ret(4)},
			[]Handler{{Start: 1, End: 2, Handler: 2, Kind: HandlerCatch, Reg: 0}}, "n"},
		{"read around an outer loop", 6, []uint32{EncodeABC(Move, 1, 4, 0), EncodeAsBx(LoadInt, 2, 1), call(0, 1),
			EncodeAsBx(JmpT, 0, -3), EncodeAsBx(JmpT, 0, -5), ret(0)}, nil, "n"},
		{"written around an outer loop", 6, []uint32{EncodeAsBx(LoadInt, 4, 1), EncodeABC(Move, 1, 4, 0), EncodeAsBx(LoadInt, 2, 1),
			call(0, 1), EncodeAsBx(JmpT, 0, -3), EncodeAsBx(JmpT, 0, -6), ret(0)}, nil, "y"},
		// The analysis reads the instructions from the first call down
		// and those above it a jump or a throw from them reaches.
		{"a handler above the call reads it", 5, []uint32{EncodeAsBx(Jmp, 0, 1), ret(4), call(0, 1), ret(0)},
			[]Handler{{Start: 2, End: 3, Handler: 1, Kind: HandlerCatch, Reg: 0}}, "n"},
		{"a handler above the call writes it", 5, []uint32{EncodeAsBx(Jmp, 0, 1), ret(4), call(0, 1), ret(0)},
			[]Handler{{Start: 2, End: 3, Handler: 1, Kind: HandlerCatch, Reg: 4}}, "y"},
		{"a loop above the handler above the call", 5, []uint32{EncodeABC(Move, 1, 4, 0), EncodeAsBx(Jmp, 0, 1), EncodeAsBx(JmpT, 0, -3),
			call(0, 1), ret(0)}, []Handler{{Start: 3, End: 4, Handler: 2, Kind: HandlerCatch, Reg: 0}}, "n"},
		// The registers 64 past the lowest call's R[A+2] are in a second
		// word.
		{"a register in the second word read after", 70, []uint32{call(0, 1), ret(66)}, nil, "n"},
		{"a register in the second word written first", 70, []uint32{call(0, 1), EncodeAsBx(LoadInt, 66, 1), ret(66)}, nil, "y"},
		{"a call in the second word", 70, []uint32{call(0, 0), call(65, 1), ret(65)}, nil, "ny"},
		{"a call in the second word, its argument read after", 70, []uint32{call(0, 0), call(65, 1), ret(67)}, nil, "nn"},
	}
	for _, c := range cases {
		fn := &Function{Name: "f", NumRegs: uint16(c.n), Code: append([]uint32(nil), c.code...), Handlers: c.handlers}
		MarkOverlapCalls(fn)
		assert.NoError(t, VerifyRegisters(fn), c.name)
		k := 0
		for pc, w := range fn.Code {
			if DecodeOp(w) != Call {
				continue
			}
			dead := c.dead[k] == 'y'
			k++
			assert.Equal(t, dead, DecodeC(w)&CallOverlap != 0, "%s: call %d", c.name, k)
			marked := &Function{Name: "f", NumRegs: uint16(c.n), Code: append([]uint32(nil), c.code...), Handlers: c.handlers}
			marked.Code[pc] |= CallOverlap << 24
			assert.Equal(t, dead, VerifyRegisters(marked) == nil, "%s: call %d marked by hand", c.name, k)
		}
		assert.Equal(t, len(c.dead), k, c.name)
	}

	plain := func() *Function {
		return &Function{Name: "f", NumRegs: 4, Code: []uint32{EncodeABC(Call, 0, 1, CallUndefThis), ret(0)}}
	}
	fn := plain()
	assert.Equal(t, 1, MarkOverlapCalls(fn))
	assert.Equal(t, CallUndefThis|CallOverlap, int(DecodeC(fn.Code[0])))
	assert.NoError(t, VerifyRegisters(fn))
	for name, set := range map[string]func(*Function){
		"generator":           func(f *Function) { f.Generator = true },
		"async":               func(f *Function) { f.Async = true },
		"module":              func(f *Function) { f.Kind = KindModule },
		"forwarded arguments": func(f *Function) { f.ForwardArguments, f.HasArguments, f.NumParams = true, true, 1 },
	} {
		fn := plain()
		set(fn)
		assert.Zero(t, MarkOverlapCalls(fn), name)
		fn.Code[0] |= CallOverlap << 24
		assert.Error(t, VerifyRegisters(fn), name)
	}
	fn = plain()
	fn.Code[0] |= 4 << 24
	assert.Error(t, VerifyRegisters(fn), "a Call's C of another bit")

	// What MarkOverlapCalls proved is proved again once it changes.
	for name, change := range map[string]func(*Function){
		"an argument read after": func(f *Function) { f.Code[1] = ret(2) },
		"a handler reading it": func(f *Function) {
			f.Code = append(f.Code, ret(2))
			f.Handlers = []Handler{{Start: 0, End: 1, Handler: 2}}
		},
		"a generator":             func(f *Function) { f.Generator = true },
		"a register past a frame": func(f *Function) { f.NumRegs = 300 },
	} {
		fn := plain()
		assert.Equal(t, 1, MarkOverlapCalls(fn), name)
		assert.NoError(t, VerifyRegisters(fn), name)
		change(fn)
		assert.Error(t, VerifyRegisters(fn), name)
	}
	fn = plain()
	fn.Code[0] |= CallOverlap << 24
	fn.Code[1] = ret(2)
	assert.Zero(t, MarkOverlapCalls(fn), "a mark the code does not allow")
	assert.Zero(t, DecodeC(fn.Code[0])&CallOverlap, "a mark the code does not allow")
	assert.NoError(t, VerifyRegisters(fn))
}

// regEffect returns the registers of the 64 from base, a multiple of 64,
// that the instruction w of op, with its first extra word x, may read and
// those it writes whenever it completes, as deadAfterCalls finds them:
// walkFacts', or regEffectSpans' of an op walkFacts leaves.
func regEffect(op Op, w, x uint32, base int) (reads, writes uint64) {
	code := []uint32{w, x, 0}
	var facts [3]instrFacts
	var calls [3]int32
	var back [1]uint64
	if pc, _, _, _ := walkFacts(code, 0, 1, 256-base, facts[:], calls[:], 0, back[:], 0); pc == 0 {
		return regEffectSpans(op, w, x, base)
	}
	return facts[0].reads, facts[0].writes
}

// TestRegEffect checks the registers regEffect finds an instruction
// reads and writes from opRegs against RegisterSpans' on random
// instructions of every op.
func TestRegEffect(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	for op := range Op(OpCount) {
		// The compare-and-branch ops of an environment operand and
		// CallEval, but no other, ask RegisterSpans.
		assert.Equal(t, op-JmpLt <= JmpNGe-JmpLt || op == CallEval, opRegs[op].ask, "%s", op)
		if opRegs[op].none {
			assert.False(t, RegisterSpans(op, uint32(op), 0, func(int, int) {}), "%s", op)
			continue
		}
		for range 2000 {
			w := uint32(op) | rnd.Uint32()&^0xff
			if rnd.IntN(4) == 0 {
				w &^= 0xff << 24 // C 0
			}
			x := rnd.Uint32()
			if rnd.IntN(2) == 0 {
				x &= EnvOperand - 1
			}
			base := rnd.IntN(4) * 64
			reads, kills := regEffect(op, w, x, base)
			var wantReads, wantKills uint64
			a := int(DecodeA(w))
			skip := pureDest(op)
			switch {
			case op == UndefRange:
				wantKills = liveRegs(a, int(DecodeB(w)), base)
			case op == New || op == Call && DecodeC(w)&CallUndefThis != 0:
				wantReads, wantKills = liveRegs(a, 1, base)|liveRegs(a+2, int(DecodeB(w)), base), liveRegs(a, 1, base)
			default:
				RegisterSpans(op, w, x, func(r, k int) {
					if skip && r == a && k == 1 {
						skip = false
						return
					}
					wantReads |= liveRegs(r, k, base)
				})
				if op == Call || pureDest(op) {
					wantKills = liveRegs(a, 1, base)
				}
			}
			if reads != wantReads || kills != wantKills {
				t.Fatalf("%s %#x x %#x base %d: reads %#x kills %#x, want %#x %#x", op, w, x, base, reads, kills, wantReads, wantKills)
			}
		}
	}
}

// TestLiveRegs checks the words of registers deadAfterCalls keeps.
func TestLiveRegs(t *testing.T) {
	for base := range MaxRegister + 1 {
		for r := range MaxRegister + 1 {
			var want uint64
			for k := range 260 {
				if i := r + k - 1; k > 0 && i >= base && i < base+64 {
					want |= 1 << (i - base)
				}
				if got := liveRegs(r, k, base); got != want {
					t.Fatalf("liveRegs(%d, %d, %d) = %#x, want %#x", r, k, base, got, want)
				}
			}
		}
	}
}

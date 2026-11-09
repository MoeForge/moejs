package bytecode

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRegisterSpansCoverEveryOp checks that VerifyRegisters knows the
// register layout of every opcode, so a new op without one fails here
// rather than run unchecked.
func TestRegisterSpansCoverEveryOp(t *testing.T) {
	for op := Op(0); int(op) < OpCount; op++ {
		assert.True(t, RegisterSpans(op, EncodeABC(op, 1, 2, 1), 4, func(int, int) {}), "%s has no register layout", op)
	}
	assert.False(t, RegisterSpans(Op(OpCount), 0, 0, func(int, int) {}))
}

// TestJumps pins the ops whose sBx operand is a jump: a new op with an sBx
// that is a value, as LoadInt's, must be left out of Jumps, or the
// verifier rejects every function that uses it.
func TestJumps(t *testing.T) {
	want := map[Op]bool{Jmp: true, JmpT: true, JmpF: true, JmpNullish: true, JmpNotNullish: true, JmpNotUndef: true,
		IterNext: true, IterClose: true, ForInNext: true, AsyncIterResult: true, AsyncIterClose: true, JmpWith: true, RegExpTest: true}
	for op := JmpLt; op <= JmpStrictNe; op++ {
		want[op], want[op.Immediate()] = true, true
	}
	want[JmpStrictEqK], want[JmpStrictNeK], want[JmpTypeofIs], want[JmpTypeofNot], want[JmpCaseK] = true, true, true, true, true
	for op := Op(0); int(op) < OpCount; op++ {
		assert.Equal(t, want[op], op.Jumps(), "%s", op)
	}
	assert.Equal(t, 10, JumpTarget(4, EncodeAsBx(Jmp, 0, 5)))
	assert.Equal(t, 0, JumpTarget(4, EncodeAsBx(JmpLtI, 0, -5)), "counted from the X word")
}

// TestVerifySingleRegisters checks every byte value in each single-register
// operand, including empty frames and frames spanning several words. The
// other bytes of an instruction need not name a register.
func TestVerifySingleRegisters(t *testing.T) {
	for _, c := range []struct {
		op Op
		n  int
	}{{LoadInt, 1}, {Move, 2}, {Add, 3}} {
		for _, n := range []uint16{0, 1, 2, 63, 64, 65, 255, 256} {
			for i := range c.n {
				for r := range 256 {
					var args [3]uint8
					for j := c.n; j < len(args); j++ {
						args[j] = 255
					}
					args[i] = uint8(r)
					fn := &Function{Name: "f", NumRegs: n, Code: []uint32{
						EncodeABC(c.op, args[0], args[1], args[2]), EncodeABC(RetUndef, 0, 0, 0)}}
					err := VerifyRegisters(fn)
					assert.Equal(t, r < int(n), err == nil, "%s operand %d R[%d] in %d: %v", c.op, i, r, n, err)
				}
			}
		}
	}
	assert.NoError(t, VerifyRegisters(&Function{Code: []uint32{EncodeABC(RetUndef, 255, 255, 255)}}))
}

// TestVerifyCallFlags checks that Call and New accept only their bits in C.
func TestVerifyCallFlags(t *testing.T) {
	for _, op := range []Op{Call, New} {
		allowed := CallOverlap
		if op == Call {
			allowed |= CallUndefThis
		}
		for c := range 256 {
			fn := &Function{Name: "f", NumRegs: 8, Code: []uint32{
				EncodeABC(op, 0, 0, uint8(c)), EncodeABC(RetUndef, 0, 0, 0)}}
			err := VerifyRegisters(fn)
			assert.Equal(t, c & ^allowed == 0, err == nil, "%s C %d: %v", op, c, err)
		}
	}
}

// TestVerifyRegisters checks the bounds of single registers, runs and the
// X word's register, and of handler registers.
func TestVerifyRegisters(t *testing.T) {
	fn := func(n int, code ...uint32) *Function { return &Function{Name: "f", NumRegs: uint16(n), Code: code} }
	ret := EncodeABC(RetUndef, 0, 0, 0)
	cases := []struct {
		name string
		n    int
		code []uint32
		ok   bool
	}{
		{"move in range", 6, []uint32{EncodeABC(Move, 5, 0, 0), EncodeABC(Ret, 5, 0, 0)}, true},
		{"move past", 5, []uint32{EncodeABC(Move, 5, 0, 0)}, false},
		{"source past", 5, []uint32{EncodeABC(Move, 0, 5, 0)}, false},
		{"add C past", 3, []uint32{EncodeABC(Add, 0, 1, 3)}, false},
		{"call block", 7, []uint32{EncodeABC(Call, 3, 2, 0), ret}, true},
		{"call block past", 6, []uint32{EncodeABC(Call, 3, 2, 0)}, false},
		{"plain call block past", 6, []uint32{EncodeABC(Call, 3, 2, CallUndefThis)}, false},
		{"spread call", 6, []uint32{EncodeABC(CallSpread, 3, 0, 0), ret}, true},
		{"spread call past", 5, []uint32{EncodeABC(CallSpread, 3, 0, 0)}, false},
		{"direct eval", 5, []uint32{EncodeABC(CallEval, 3, 0, 0), 0, ret}, true},
		{"spread direct eval past", 5, []uint32{EncodeABC(CallEval, 3, 0, 1), 0}, false},
		{"spread direct eval, C 2, past", 5, []uint32{EncodeABC(CallEval, 3, 0, 2), 0}, false},
		{"spread direct eval, C 2", 6, []uint32{EncodeABC(CallEval, 3, 0, 2), 0, ret}, true},
		{"undef range", 4, []uint32{EncodeABC(UndefRange, 1, 3, 0), ret}, true},
		{"undef range past", 4, []uint32{EncodeABC(UndefRange, 2, 3, 0)}, false},
		{"empty undef range", 1, []uint32{EncodeABC(UndefRange, 1, 0, 0), ret}, true},
		{"for-in record", 4, []uint32{EncodeABC(ForInInit, 0, 0, 0), EncodeAsBx(ForInNext, 0, 0), ret}, true},
		{"for-in record past", 3, []uint32{EncodeAsBx(ForInNext, 0, 0), ret}, false},
		{"compare X register", 3, []uint32{EncodeAsBx(JmpLt, 0, 1), 3, ret}, false},
		{"compare immediate", 1, []uint32{EncodeAsBx(JmpLtI, 0, 1), 300, ret}, true},
		{"compare environment operand", 3, []uint32{EncodeAsBx(JmpLt, 2, 1), EnvOperand | 7<<8 | 2, ret}, true},
		{"compare environment operand past", 2, []uint32{EncodeAsBx(JmpNGe, 2, 1), EnvOperand | 7<<8 | 2, ret}, false},
		{"compare environment operand, another register", 3, []uint32{EncodeAsBx(JmpLt, 1, 1), EnvOperand | 7<<8 | 2, ret}, false},
		{"compare environment operand, high bits", 3, []uint32{EncodeAsBx(JmpLt, 2, 1), 2*EnvOperand | 7<<8 | 2, ret}, false},
		{"element ref X", 4, []uint32{EncodeABC(GetElemRef, 0, 1, 2), 4}, false},
		{"missing extra word", 4, []uint32{EncodeAsBx(JmpLt, 0, 1)}, false},
		{"unknown op", 4, []uint32{uint32(OpCount), ret}, true},
		{"no registers", 0, []uint32{ret}, true},
	}
	for _, c := range cases {
		err := VerifyRegisters(fn(c.n, c.code...))
		assert.Equal(t, c.ok, err == nil, "%s: %v", c.name, err)
	}
	f := fn(3, ret)
	f.Handlers = []Handler{{Kind: HandlerFinally, Reg: 1}}
	assert.NoError(t, VerifyRegisters(f))
	f.Handlers = []Handler{{Kind: HandlerFinally, Reg: 2}}
	assert.Error(t, VerifyRegisters(f), "a finally handler writes two registers")
	f.Handlers = []Handler{{Kind: HandlerCatch, Reg: 3}}
	assert.Error(t, VerifyRegisters(f))
}

// TestVerifyCodeEnd checks that no instruction continues past the code,
// which the interpreter reads without bounds checks: a function has an
// instruction, its extra words are inside the code, and the last
// instruction does not continue at the next pc (ends). A conditional jump
// continues there when it is not taken, a call when it returns, a
// suspension when the frame resumes and CoerceThis when the frame reruns.
func TestVerifyCodeEnd(t *testing.T) {
	fn := func(code ...uint32) *Function { return &Function{Name: "f", NumRegs: 2, Code: code} }
	assert.ErrorContains(t, VerifyRegisters(fn()), "has no instructions")
	assert.ErrorContains(t, VerifyRegisters(fn(EncodeAsBx(JmpLtI, 0, -2))), "lacks its extra words")
	load := EncodeABC(LoadUndef, 0, 0, 0)
	for _, c := range []struct {
		name string
		last []uint32
		ok   bool
	}{
		{"return", []uint32{EncodeABC(Ret, 0, 0, 0)}, true},
		{"return undefined", []uint32{EncodeABC(RetUndef, 0, 0, 0)}, true},
		{"throw", []uint32{EncodeABC(Throw, 0, 0, 0)}, true},
		{"jump back", []uint32{EncodeAsBx(Jmp, 0, -2)}, true},
		{"load", []uint32{load}, false},
		{"conditional jump", []uint32{EncodeAsBx(JmpT, 0, -2)}, false},
		{"compare and branch", []uint32{EncodeAsBx(JmpLtI, 0, -2), 0}, false},
		{"call", []uint32{EncodeABC(Call, 0, 0, CallUndefThis)}, false},
		{"yield", []uint32{EncodeABC(Yield, 0, 0, 0)}, false},
		{"await", []uint32{EncodeABC(Await, 0, 0, 0)}, false},
		{"coerce this", []uint32{EncodeABC(CoerceThis, 0, 0, 0)}, false},
		{"unknown op", []uint32{uint32(OpCount)}, false},
	} {
		err := VerifyRegisters(fn(append([]uint32{load}, c.last...)...))
		assert.Equal(t, c.ok, err == nil, "%s: %v", c.name, err)
		if !c.ok {
			assert.ErrorContains(t, err, "the last instruction continues past the code", c.name)
		}
	}
}

// TestVerifyICOperands checks the inline cache an instruction names in
// its X word's high 16 bits against ICCount, for every op that names one.
func TestVerifyICOperands(t *testing.T) {
	consts := []Const{{Kind: ConstString, Str: "k", Key: true}}
	ret := EncodeABC(RetUndef, 0, 0, 0)
	for _, op := range []Op{GetGlobal, GetGlobalOrUndef, SetGlobal, GetProp, GetLen, SetProp, DefineField,
		GetTemplate, SetGlobalSloppy, ResolveGlobal, SetGlobalRef, SetPropSloppy} {
		assert.True(t, icOperand(op), "%s", op)
		for _, c := range []struct {
			ic, count uint32
			ok        bool
		}{
			{0, 1, true}, {2, 3, true}, {0xffff, 0x10000, true},
			{0, 0, false}, {3, 3, false}, {0xffff, 0xffff, false},
		} {
			fn := &Function{Name: "f", NumRegs: 2, Consts: consts, ICCount: c.count,
				Code: []uint32{EncodeABx(NewObject, 0, 0), EncodeABC(op, 0, 1, 0), ExtraArg(0, uint16(c.ic)), ret}}
			err := VerifyRegisters(fn)
			assert.Equal(t, c.ok, err == nil, "%s cache %d of %d: %v", op, c.ic, c.count, err)
			if !c.ok {
				assert.ErrorContains(t, err, "names inline cache")
			}
		}
	}
}

// TestVerifyObjectLit checks a NewObjectLit's operands: its C values from
// B in the window, at least one, its keys property names among the
// constants, and an inline cache entry below ICCount that no other
// instruction names, which the engine trusts to hold a shape of the
// literal's keys.
func TestVerifyObjectLit(t *testing.T) {
	keys := []Const{{Kind: ConstString, Str: "a", Key: true}, {Kind: ConstString, Str: "b", Key: true},
		{Kind: ConstString, Str: "c"}, {Kind: ConstNumber, Num: 1}}
	ret := EncodeABC(RetUndef, 0, 0, 0)
	lit := func(a, b, c uint8, lo, ic uint16) []uint32 {
		return []uint32{EncodeABC(NewObjectLit, a, b, c), ExtraArg(lo, ic)}
	}
	getProp := []uint32{EncodeABC(GetProp, 1, 0, 0), ExtraArg(0, 0)}
	cat := func(parts ...[]uint32) []uint32 { return append(slices.Concat(parts...), ret) }
	for _, c := range []struct {
		name string
		code []uint32
		want string // "" when it verifies
	}{
		{"two keys", cat(lit(0, 1, 2, 0, 0)), ""},
		{"one key", cat(lit(0, 2, 1, 1, 0)), ""},
		{"the object over its first value", cat(lit(1, 1, 2, 0, 0)), ""},
		{"no key", cat(lit(0, 1, 0, 0, 0)), "of constants"},
		{"keys past the constants", cat(lit(0, 1, 2, 3, 0)), "of constants"},
		{"a key no property name", cat(lit(0, 1, 2, 1, 0)), "which is no property name"},
		{"a key a number", cat(lit(0, 1, 1, 3, 0)), "which is no property name"},
		{"values past the window", cat(lit(0, 2, 2, 0, 0)), "register"},
		{"the cache past ICCount", cat(lit(0, 1, 2, 0, 2)), "names inline cache 2"},
		{"two literals, two caches", cat(lit(0, 1, 2, 0, 0), lit(0, 1, 2, 0, 1)), ""},
		{"two literals, one cache", cat(lit(0, 1, 2, 0, 1), lit(0, 1, 2, 0, 1)), "NewObjectLit names inline cache 1, which a NewObjectLit names"},
		{"a GetProp of the literal's cache after it", cat(lit(0, 1, 2, 0, 0), getProp), "GetProp names inline cache 0, which a NewObjectLit names"},
		{"a GetProp of the literal's cache before it", cat(getProp, lit(0, 1, 2, 0, 0)), "GetProp names inline cache 0, which a NewObjectLit names"},
		{"a GetProp of another cache", cat(lit(0, 1, 2, 0, 1), getProp), ""},
	} {
		err := VerifyRegisters(&Function{Name: "f", NumRegs: 3, Consts: keys, ICCount: 2, Code: c.code})
		if c.want == "" {
			assert.NoError(t, err, c.name)
		} else {
			assert.ErrorContains(t, err, c.want, c.name)
		}
	}
}

// TestVerifyHandlerKinds checks that a handler row is a catch or a
// finally: the engine enters a row of any other kind as a finally, writing
// R[Reg+1] too, which a check that sized it as a catch would let past the
// window (Reg = NumRegs-1) and the kind proof would let keep its facts.
func TestVerifyHandlerKinds(t *testing.T) {
	ret := EncodeABC(RetUndef, 0, 0, 0)
	for _, k := range []HandlerKind{2, 3, 255} {
		for _, reg := range []uint16{0, 2} {
			f := &Function{Name: "f", NumRegs: 3, Code: []uint32{ret}, Handlers: []Handler{{Kind: k, Reg: reg}}}
			assert.ErrorContains(t, VerifyRegisters(f), "neither a catch nor a finally", "kind %d, register %d", k, reg)
		}
	}
}

// TestVerifyWideFrames checks that NumRegs is at most MaxRegister+1, the
// window's registers, before anything indexes a set of registers: a frame
// past it fails with an error however its registers are named (a run past
// 255, a record at A = 255, a handler's register, a call block), with a
// kind operand or without.
func TestVerifyWideFrames(t *testing.T) {
	abc, abx := EncodeABC, EncodeABx
	obj, def, ret := abx(NewObject, 0, 0), []uint32{abc(DefineField, 0, 1, 0), 0}, abc(RetUndef, 0, 0, 0)
	cat := func(parts ...[]uint32) []uint32 { return slices.Concat(parts...) }
	cases := []struct {
		name     string
		code     []uint32
		handlers []Handler
	}{
		{"an UndefRange past 255", cat([]uint32{obj, abc(UndefRange, 250, 20, 0)}, def, []uint32{ret}), nil},
		{"the same without a kind operand", []uint32{obj, abc(UndefRange, 250, 20, 0), ret}, nil},
		{"a for-in record at 255", []uint32{abc(ForInInit, 255, 0, 0), EncodeAsBx(ForInNext, 255, 0), ret}, nil},
		{"a handler register past 255", cat([]uint32{obj}, def, []uint32{ret}), []Handler{{Start: 0, End: 1, Handler: 3, Kind: HandlerFinally, Reg: 280}}},
		{"a call block past 255", cat([]uint32{obj, abc(Call, 250, 20, 0)}, def, []uint32{ret}), nil},
	}
	for _, n := range []int{257, 300, 65535} {
		for _, c := range cases {
			err := VerifyRegisters(&Function{Name: "f", NumRegs: uint16(n), Consts: nameConst, ICCount: 1, Code: c.code, Handlers: c.handlers})
			assert.ErrorContains(t, err, "more than the 256 of a frame's window", "%s, %d registers", c.name, n)
		}
	}
	assert.NoError(t, VerifyRegisters(&Function{Name: "f", NumRegs: 256, Consts: nameConst, ICCount: 1, Code: cat([]uint32{obj, abc(UndefRange, 250, 6, 0)}, def, []uint32{ret})}))
}

// TestVerifyJumps checks every jump op against the instruction starts: a
// jump onto an instruction is accepted, one onto the X word of an
// instruction, past the end or before the first word is not.
func TestVerifyJumps(t *testing.T) {
	ret := EncodeABC(RetUndef, 0, 0, 0)
	// pc 0 is a two-word instruction, pc 2 makes ForInNext's record, pc 3
	// is the jump (with its extra words), then two returns.
	head := []uint32{EncodeAsBx(JmpLtI, 0, 1), 7, EncodeABC(ForInInit, 0, 0, 0)}
	for op := Op(0); int(op) < OpCount; op++ {
		if !op.Jumps() {
			continue
		}
		at, extra := len(head), op.ExtraWords()
		end := at + 1 + extra + 2 // the length of the code
		for _, c := range []struct {
			name   string
			target int
			ok     bool
		}{
			{"onto a return", at + 1 + extra, true},
			{"onto the last return", end - 1, true},
			{"back onto the first instruction", 0, true},
			{"onto the jump itself", at, true},
			{"onto an X word", 1, false},
			{"past the end", end, false},
			{"before the first word", -1, false},
		} {
			code := append(append([]uint32{}, head...), EncodeAsBx(op, 0, int16(c.target-(at+1))))
			for range extra {
				code = append(code, 0)
			}
			consts := nameConst
			if op == RegExpTest { // K0 its regular expression
				consts = []Const{{Kind: ConstRegExp, Str: "a"}}
			}
			code = append(code, ret, ret)
			err := VerifyRegisters(&Function{Name: "f", NumRegs: 8, Consts: consts, ICCount: 1, Code: code})
			assert.Equal(t, c.ok, err == nil, "%s %s: %v", op, c.name, err)
		}
	}
	if extra := JmpLtI.ExtraWords(); extra != 1 {
		t.Fatalf("JmpLtI has %d extra words", extra)
	}

	// The audit's function: a jump onto the X word of an immediate
	// compare, which decodes as Move R[200], R[0].
	x := EncodeABC(Move, 200, 0, 0)
	jump := &Function{Name: "jump", NumRegs: 1, Code: []uint32{EncodeAsBx(Jmp, 0, 1), EncodeAsBx(JmpLtI, 0, 1), x, ret}}
	assert.ErrorContains(t, VerifyRegisters(jump), "Jmp jumps to 2, which starts no instruction")
	jump.Code[0] = EncodeAsBx(Jmp, 0, 2)
	assert.NoError(t, VerifyRegisters(jump))
}

// TestVerifyHandlers checks a handler's pc against the instruction starts
// and its range against the code.
func TestVerifyHandlers(t *testing.T) {
	ret := EncodeABC(RetUndef, 0, 0, 0)
	x := EncodeABC(Move, 200, 0, 0)
	code := []uint32{EncodeABC(Throw, 0, 0, 0), EncodeAsBx(JmpLtI, 0, 1), x, ret}
	for _, c := range []struct {
		name string
		h    Handler
		ok   bool
	}{
		{"a catch", Handler{Start: 0, End: 1, Handler: 3, Kind: HandlerCatch}, true},
		{"onto the X word", Handler{Start: 0, End: 1, Handler: 2, Kind: HandlerCatch}, false},
		{"past the end", Handler{Start: 0, End: 1, Handler: 4, Kind: HandlerCatch}, false},
		{"the whole code", Handler{Start: 0, End: 4, Handler: 3, Kind: HandlerCatch}, true},
		{"a range past the end", Handler{Start: 0, End: 5, Handler: 3, Kind: HandlerCatch}, false},
		{"a range that ends before it starts", Handler{Start: 2, End: 1, Handler: 3, Kind: HandlerCatch}, false},
	} {
		err := VerifyRegisters(&Function{Name: "f", NumRegs: 1, Code: code, Handlers: []Handler{c.h}})
		assert.Equal(t, c.ok, err == nil, "%s: %v", c.name, err)
	}
}

// TestVerifyModuleBody checks a module's body pc against the instruction
// starts.
func TestVerifyModuleBody(t *testing.T) {
	ret := EncodeABC(RetUndef, 0, 0, 0)
	code := []uint32{EncodeABC(LoadUndef, 0, 0, 0), EncodeAsBx(JmpLtI, 0, 1), 7, ret}
	for _, c := range []struct {
		body int
		ok   bool
	}{{0, true}, {1, true}, {3, true}, {2, false}, {4, false}, {-1, false}} {
		fn := &Function{Name: "m", Kind: KindModule, NumRegs: 1, Code: code, Module: &Module{BodyPC: c.body}}
		err := VerifyRegisters(fn)
		assert.Equal(t, c.ok, err == nil, "body pc %d: %v", c.body, err)
	}
}

// TestVerifyResumablePCs checks that a function that can suspend or rerun
// stays below 1<<PCBits words: the engine packs a saved pc with the
// environment depth above them. The other words are no opcode, which the
// walk skips.
func TestVerifyResumablePCs(t *testing.T) {
	if testing.Short() {
		t.Skip("verifies functions of 16 M words")
	}
	saves := []Op{GenStart, Yield, AsyncGenStart, Await, AsyncYield, CoerceThis, CallEval}
	for op := Op(0); int(op) < OpCount; op++ {
		assert.Equal(t, slices.Contains(saves, op), op.SavesPC(), "%s", op)
	}
	code := make([]uint32, 1<<PCBits)
	for i := range code {
		code[i] = uint32(OpCount)
	}
	// Each length ends with a return: no instruction continues past the
	// code.
	ret := EncodeABC(RetUndef, 0, 0, 0)
	code[len(code)-2], code[len(code)-1] = ret, ret
	for _, op := range append(saves, Move) {
		code[0], code[1] = EncodeABC(op, 0, 1, 0), uint32(OpCount)
		if op.ExtraWords() > 0 {
			code[1] = 0
		}
		long := &Function{Name: "g", NumRegs: 3, Code: code}
		if op.SavesPC() {
			assert.ErrorContains(t, VerifyRegisters(long), "suspends or reruns", "%s", op)
		} else {
			assert.NoError(t, VerifyRegisters(long), "%s never resumes", op)
		}
		long.Code = code[:len(code)-1]
		assert.NoError(t, VerifyRegisters(long), "%s", op)
	}
}

// TestVerifyConstOperands checks the constants the engine reads as a
// given kind without looking: NewRegExp's and RegExpTest's must be a
// regular expression, NewPrivateName's and ThrowError's a string, and
// inside the pool.
func TestVerifyConstOperands(t *testing.T) {
	consts := []Const{{Kind: ConstString, Str: "s"}, {Kind: ConstRegExp, Str: "a", Flags: "g"}, {Kind: ConstNumber, Num: 1}}
	ret := EncodeABC(RetUndef, 0, 0, 0)
	for _, c := range []struct {
		op Op
		bx uint16
		ok bool
	}{
		{NewRegExp, 1, true}, {NewRegExp, 0, false}, {NewRegExp, 2, false}, {NewRegExp, 3, false},
		{NewPrivateName, 0, true}, {NewPrivateName, 1, false}, {NewPrivateName, 2, false}, {NewPrivateName, 9, false},
		{ThrowError, 0, true}, {ThrowError, 2, false}, {ThrowError, 3, false},
		{LoadConst, 2, true}, // any kind
	} {
		fn := &Function{Name: "f", NumRegs: 1, Consts: consts, Code: []uint32{EncodeABx(c.op, 0, c.bx), ret}}
		err := VerifyRegisters(fn)
		assert.Equal(t, c.ok, err == nil, "%s K%d: %v", c.op, c.bx, err)
	}
	// RegExpTest names its regular expression and its argument register in
	// the X word.
	for _, c := range []struct {
		x  uint32
		ok bool
	}{{1, true}, {1 | 2<<16, true}, {1 | 3<<16, false}, {0, false}, {2, false}, {3, false}} {
		fn := &Function{Name: "f", NumRegs: 3, Consts: consts, Code: []uint32{EncodeAsBx(RegExpTest, 0, 1), c.x, ret}}
		err := VerifyRegisters(fn)
		assert.Equal(t, c.ok, err == nil, "RegExpTest K%d: %v", c.x, err)
	}
}

// TestVerifyStrictOperands checks the constant of JmpStrictEqK and
// JmpStrictNeK, which the engine compares as a value that is no number: a
// string, null or undefined, inside the pool.
func TestVerifyStrictOperands(t *testing.T) {
	consts := []Const{{Kind: ConstString, Str: "s"}, {Kind: ConstNull}, {Kind: ConstUndefined}, {Kind: ConstNumber, Num: 1},
		{Kind: ConstBigInt, Str: "1"}}
	ret := EncodeABC(RetUndef, 0, 0, 0)
	for _, c := range []struct {
		op Op
		x  uint32
		ok bool
	}{
		{JmpStrictEqK, 0, true}, {JmpStrictEqK, 1, true}, {JmpStrictNeK, 2, true},
		{JmpStrictEqK, 3, false}, {JmpStrictNeK, 4, false}, {JmpStrictEqK, 5, false}, {JmpStrictNeK, 1 << 16, false},
	} {
		fn := &Function{Name: "f", NumRegs: 1, Consts: consts, Code: []uint32{EncodeAsBx(c.op, 0, 1), c.x, ret}}
		err := VerifyRegisters(fn)
		assert.Equal(t, c.ok, err == nil, "%s X=%#x: %v", c.op, c.x, err)
	}
}

// nameConst is the pool of the fixtures whose X words name K0 as a
// property (keyOperand).
var nameConst = []Const{{Kind: ConstString, Str: "k", Key: true}}

// TestVerifyKeyOperands checks the property-name constants of the X word:
// the engine reads K[name] as an interned key, so it must be a string with
// Key set, inside the pool; the name is the low 16 bits of X or all of it.
func TestVerifyKeyOperands(t *testing.T) {
	consts := []Const{{Kind: ConstString, Str: "k", Key: true}, {Kind: ConstString, Str: "s"}, {Kind: ConstNumber, Num: 1}}
	ret := EncodeABC(RetUndef, 0, 0, 0)
	for _, c := range []struct {
		op Op
		x  uint32
		ok bool
	}{
		{GetProp, 0, true}, {GetProp, 7 << 16, true}, {GetProp, 1, false}, {GetProp, 2, false}, {GetProp, 3, false},
		{GetGlobal, 0, true}, {GetGlobal, 1, false},
		{WithGet, 1 << 16, true}, {WithGet, 1<<16 | 1, false},
		{DelProp, 0, true}, {DelProp, 1, false}, {DelProp, 1 << 16, false},
		{SetGlobalVar, 0, true}, {SetGlobalVar, 2, false},
		{DefineField, 3 << 16, true}, {DefineField, 1, false},
	} {
		fn := &Function{Name: "f", NumRegs: 1, Consts: consts, ICCount: 8, Code: []uint32{EncodeABx(NewObject, 0, 0), EncodeABC(c.op, 0, 0, 0), c.x, ret}}
		err := VerifyRegisters(fn)
		assert.Equal(t, c.ok, err == nil, "%s X=%#x: %v", c.op, c.x, err)
	}
}

// TestVerifyLazyMapArguments checks the layout a lazy MapArguments needs:
// the engine aliases index i of the arguments object to Env slot i, so
// parameter i must be what the layout puts there.
func TestVerifyLazyMapArguments(t *testing.T) {
	ret := EncodeABC(RetUndef, 0, 0, 0)
	for _, c := range []struct {
		name   string
		b      uint8
		layout CaptureLayout
		ok     bool
	}{
		{"eager any layout", 0, CaptureLayout{1, 0, 2}, true},
		{"lazy in order", 1, CaptureLayout{0, 1, 2}, true},
		{"lazy in order with more slots", 1, CaptureLayout{0, 1, 2, NoRegister}, true},
		{"lazy swapped", 1, CaptureLayout{1, 0, 2}, false},
		{"lazy parameter not captured", 1, CaptureLayout{0, NoRegister, 2}, false},
		{"lazy layout too short", 1, CaptureLayout{0}, false},
		{"unknown B", 2, CaptureLayout{0, 1, 2}, false},
	} {
		fn := &Function{Name: "f", NumRegs: 3, NumParams: 2, CaptureLayout: c.layout, Code: []uint32{EncodeABC(MapArguments, 2, c.b, 0), ret}}
		err := VerifyRegisters(fn)
		assert.Equal(t, c.ok, err == nil, "%s: %v", c.name, err)
	}
}

// TestVerifyConstantJumps: the jumps on a constant or a type named by their
// X word (JmpStrictEqK, JmpStrictNeK, JmpTypeofIs, JmpTypeofNot and
// JmpCaseK), whose X word the interpreter reads without a bounds check,
// are rejected without it, as the last instruction, with a register past
// NumRegs, and, for the ones that compare with the constant, with one that
// is no string, null or undefined. TestVerifyJumps covers their targets.
func TestVerifyConstantJumps(t *testing.T) {
	consts := []Const{{Kind: ConstString, Str: "s"}, {Kind: ConstNumber, Num: 1}}
	ret := EncodeABC(RetUndef, 0, 0, 0)
	fn := func(code ...uint32) *Function { return &Function{Name: "f", NumRegs: 1, Consts: consts, Code: code} }
	for _, op := range []Op{JmpStrictEqK, JmpStrictNeK, JmpTypeofIs, JmpTypeofNot, JmpCaseK} {
		assert.NoError(t, VerifyRegisters(fn(EncodeAsBx(op, 0, 1), 0, ret)), "%s", op)
		assert.ErrorContains(t, VerifyRegisters(fn(ret, EncodeAsBx(op, 0, -2))), "lacks its extra words", "%s", op)
		assert.ErrorContains(t, VerifyRegisters(fn(ret, EncodeAsBx(op, 0, -2), 0)), "the last instruction continues past the code", "%s", op)
		assert.ErrorContains(t, VerifyRegisters(fn(EncodeAsBx(op, 1, 1), 0, ret)), "names registers", "%s", op)
		strict := op != JmpTypeofIs && op != JmpTypeofNot
		for _, x := range []uint32{1, 2} { // a number, past the pool
			err := VerifyRegisters(fn(EncodeAsBx(op, 0, 1), x, ret))
			assert.Equal(t, strict, err != nil, "%s X=%d: %v", op, x, err)
		}
	}
}

// TestVerifyRegExpTest: RegExpTest, whose X word the interpreter reads
// without a bounds check for the constant it matches with and the register
// of its argument, is rejected without the word, as the last instruction,
// with its result or argument register past NumRegs, and with a constant
// that is no regular expression or past the pool. TestVerifyJumps covers
// its target.
func TestVerifyRegExpTest(t *testing.T) {
	consts := []Const{{Kind: ConstRegExp, Str: "a"}, {Kind: ConstString, Str: "s"}}
	ret := EncodeABC(RetUndef, 0, 0, 0)
	fn := func(code ...uint32) *Function { return &Function{Name: "f", NumRegs: 2, Consts: consts, Code: code} }
	op := RegExpTest
	assert.NoError(t, VerifyRegisters(fn(EncodeAsBx(op, 0, 1), ExtraArg(0, 1), ret)))
	assert.ErrorContains(t, VerifyRegisters(fn(ret, EncodeAsBx(op, 0, -2))), "lacks its extra words")
	assert.ErrorContains(t, VerifyRegisters(fn(ret, EncodeAsBx(op, 0, -2), ExtraArg(0, 1))), "the last instruction continues past the code")
	assert.ErrorContains(t, VerifyRegisters(fn(EncodeAsBx(op, 2, 1), ExtraArg(0, 1), ret)), "names registers")
	assert.ErrorContains(t, VerifyRegisters(fn(EncodeAsBx(op, 0, 1), ExtraArg(0, 2), ret)), "names registers")
	for _, k := range []uint16{1, 2} { // a string, past the pool
		assert.ErrorContains(t, VerifyRegisters(fn(EncodeAsBx(op, 0, 1), ExtraArg(k, 1), ret)), "which is no regular expression", "K%d", k)
	}
}

// TestVerifyApplyArguments checks that ApplyArguments names the arguments
// register of a function with ForwardArguments, mapped only with its
// parameters at their Env slots, and that such a function neither suspends
// nor lacks the register.
func TestVerifyApplyArguments(t *testing.T) {
	ret := EncodeABC(RetUndef, 0, 0, 0)
	mk := func(np uint16, rest, fwd, gen bool, layout CaptureLayout, w uint32) *Function {
		return &Function{Name: "f", NumRegs: 8, NumParams: np, HasRest: rest, HasArguments: true, ForwardArguments: fwd,
			Generator: gen, CaptureLayout: layout, Code: []uint32{w, ret}}
	}
	ok := []*Function{
		mk(0, false, true, false, nil, EncodeABC(ApplyArguments, 2, 0, 1)),
		mk(2, false, true, false, CaptureLayout{0, 1}, EncodeABC(ApplyArguments, 3, 2, 1)),
		mk(2, true, true, false, nil, EncodeABC(ApplyArguments, 4, 3, 0)),
	}
	for i, fn := range ok {
		assert.NoError(t, VerifyRegisters(fn), "case %d", i)
	}
	bad := []*Function{
		mk(0, false, false, false, nil, EncodeABC(ApplyArguments, 2, 0, 0)),                // no ForwardArguments
		mk(1, false, true, false, nil, EncodeABC(ApplyArguments, 2, 0, 0)),                 // R[0] is a parameter
		mk(2, false, true, false, CaptureLayout{1, 0}, EncodeABC(ApplyArguments, 3, 2, 1)), // mapped out of order
		mk(2, false, true, false, nil, EncodeABC(ApplyArguments, 5, 2, 1)),                 // mapped with no Env
		mk(0, false, true, true, nil, EncodeABC(ApplyArguments, 2, 0, 0)),                  // a generator
		mk(0, false, true, false, nil, EncodeABC(ApplyArguments, 5, 0, 0)),                 // R[5..8] past NumRegs
	}
	for i, fn := range bad {
		assert.Error(t, VerifyRegisters(fn), "case %d", i)
	}
	noArgs := mk(0, false, true, false, nil, ret)
	noArgs.HasArguments = false
	assert.Error(t, VerifyRegisters(noArgs))
}

// TestVerifyArrayPushK checks ArrayPushK's constants, which the engine
// pushes without looking at them (one or more numbers, strings, null or
// undefined inside the pool), and its array, which it reads as one.
func TestVerifyArrayPushK(t *testing.T) {
	abc, abx := EncodeABC, EncodeABx
	ret := abc(RetUndef, 0, 0, 0)
	consts := []Const{{Kind: ConstNumber, Num: 1}, {Kind: ConstString, Str: "s", IsASCII: true}, {Kind: ConstNull},
		{Kind: ConstUndefined}, {Kind: ConstBigInt, Str: "1"}, {Kind: ConstRegExp, Str: "a"}}
	mk := func(code ...uint32) *Function {
		return &Function{Name: "f", NumRegs: 2, Consts: consts, Code: append(code, ret)}
	}
	arr := abx(NewArray, 0, 4)
	assert.NoError(t, VerifyRegisters(mk(arr, abc(ArrayPushK, 0, 0, 0), ExtraArg(0, 4))))
	assert.NoError(t, VerifyRegisters(mk(arr, abc(ArrayPushK, 0, 0, 0), ExtraArg(3, 1))))
	for i, fn := range []*Function{
		mk(arr, abc(ArrayPushK, 0, 0, 0), ExtraArg(0, 0)),                        // no constant
		mk(arr, abc(ArrayPushK, 0, 0, 0), ExtraArg(3, 2)),                        // a BigInt
		mk(arr, abc(ArrayPushK, 0, 0, 0), ExtraArg(5, 1)),                        // a regular expression
		mk(arr, abc(ArrayPushK, 0, 0, 0), ExtraArg(4, 3)),                        // past the pool
		mk(arr, abc(ArrayPushK, 0, 0, 0), ExtraArg(0xffff, 2)),                   // far past it
		mk(abx(NewObject, 0, 0), abc(ArrayPushK, 0, 0, 0), ExtraArg(0, 1)),       // no array
		mk(arr, abc(LoadInt, 0, 0, 0), abc(ArrayPushK, 0, 0, 0), ExtraArg(0, 1)), // overwritten
		mk(abc(ArrayPushK, 2, 0, 0), ExtraArg(0, 1)),                             // past NumRegs
	} {
		assert.Error(t, VerifyRegisters(fn), "case %d", i)
	}
}

// TestVerifyNewArrayK checks NewArrayK's constants, which the engine takes
// as ArrayPushK does, and the array it leaves, which ArrayPushK may push
// to.
func TestVerifyNewArrayK(t *testing.T) {
	abc := EncodeABC
	ret := abc(RetUndef, 0, 0, 0)
	consts := []Const{{Kind: ConstNumber, Num: 1}, {Kind: ConstString, Str: "s", IsASCII: true}, {Kind: ConstNull},
		{Kind: ConstUndefined}, {Kind: ConstBigInt, Str: "1"}, {Kind: ConstRegExp, Str: "a"}}
	mk := func(code ...uint32) *Function {
		return &Function{Name: "f", NumRegs: 2, Consts: consts, Code: append(code, ret)}
	}
	newK := abc(NewArrayK, 0, 0, 0)
	assert.NoError(t, VerifyRegisters(mk(newK, ExtraArg(0, 4))))
	assert.NoError(t, VerifyRegisters(mk(newK, ExtraArg(0, 2), abc(ArrayPushK, 0, 0, 0), ExtraArg(2, 2))))
	for i, fn := range []*Function{
		mk(newK, ExtraArg(0, 0)),                    // no constant
		mk(newK, ExtraArg(3, 2)),                    // a BigInt
		mk(newK, ExtraArg(5, 1)),                    // a regular expression
		mk(newK, ExtraArg(4, 3)),                    // past the pool
		mk(newK, ExtraArg(0xffff, 2)),               // far past it
		mk(abc(NewArrayK, 2, 0, 0), ExtraArg(0, 1)), // past NumRegs
	} {
		assert.Error(t, VerifyRegisters(fn), "case %d", i)
	}
}

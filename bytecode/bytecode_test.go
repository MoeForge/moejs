package bytecode

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecode(t *testing.T) {
	w := EncodeABC(Add, 1, 2, 3)
	assert.Equal(t, Add, DecodeOp(w))
	assert.Equal(t, uint8(1), DecodeA(w))
	assert.Equal(t, uint8(2), DecodeB(w))
	assert.Equal(t, uint8(3), DecodeC(w))

	w = EncodeABx(LoadConst, 255, 0xFFFF)
	assert.Equal(t, LoadConst, DecodeOp(w))
	assert.Equal(t, uint8(255), DecodeA(w))
	assert.Equal(t, uint16(0xFFFF), DecodeBx(w))

	for _, v := range []int16{0, 1, -1, MaxSBx, MinSBx} {
		w = EncodeAsBx(Jmp, 7, v)
		assert.Equal(t, v, DecodeSBx(w), "sBx %d", v)
		assert.Equal(t, uint8(7), DecodeA(w))
	}

	x := ExtraArg(0x1234, 0xABCD)
	assert.Equal(t, uint16(0x1234), ExtraLo(x))
	assert.Equal(t, uint16(0xABCD), ExtraHi(x))
}

func TestOpTable(t *testing.T) {
	seen := map[string]bool{}
	for op := Op(0); op < opCount; op++ {
		name := op.String()
		require.NotEmpty(t, name, "op %d has no name", op)
		assert.False(t, seen[name], "duplicate mnemonic %s", name)
		seen[name] = true
		assert.LessOrEqual(t, op.ExtraWords(), 2)
	}
	assert.Equal(t, "Op(255)", Op(255).String())
	assert.Equal(t, FmtABC, Add.Format())
	assert.Equal(t, FmtAsBx, JmpT.Format())
	assert.Equal(t, FmtSBx, Jmp.Format())
	assert.Equal(t, 1, GetProp.ExtraWords())
	assert.Equal(t, 2, GetEnvChkW.ExtraWords())
	assert.Equal(t, 0, Move.ExtraWords())
	assert.Equal(t, "module", KindModule.String())
	assert.Equal(t, "unknown", Kind(99).String())
}

// TestOpOrderGroups pins the runs of ops whose numbers the code computes
// from one another, which a change of the op order must keep together and
// in their order: Lt .. Ge (CompareJump, Lt+k&3 in the interpreter),
// JmpLt .. JmpStrictNe and their integer forms (Immediate), JmpStrictEq and
// JmpStrictNe and their constant forms (Constant), and the async function
// ops engine's coldOp tests as one range.
func TestOpOrderGroups(t *testing.T) {
	runs := [][]Op{
		{Lt, Le, Gt, Ge},
		{JmpLt, JmpLe, JmpGt, JmpGe, JmpNLt, JmpNLe, JmpNGt, JmpNGe, JmpStrictEq, JmpStrictNe},
		{JmpLtI, JmpLeI, JmpGtI, JmpGeI, JmpNLtI, JmpNLeI, JmpNGtI, JmpNGeI, JmpStrictEqI, JmpStrictNeI},
		{JmpStrictEqK, JmpStrictNeK},
		{AsyncGenStart, Await, AsyncReturn, AsyncThrow},
	}
	for _, r := range runs {
		for i, op := range r {
			assert.Equal(t, r[0]+Op(i), op, "%s is not %d after %s", op, i, r[0])
		}
	}
	for op := JmpLt; op <= JmpStrictNe; op++ {
		assert.Equal(t, op.String()+"I", op.Immediate().String())
	}
	assert.Equal(t, JmpStrictEqK, JmpStrictEq.Constant())
	assert.Equal(t, JmpStrictNeK, JmpStrictNe.Constant())
}

func TestTypeNames(t *testing.T) {
	i, ok := TypeNameIndex("string")
	assert.True(t, ok)
	assert.Equal(t, TypeString, i)
	_, ok = TypeNameIndex("nope")
	assert.False(t, ok)
	assert.Equal(t, "function", TypeNames[TypeFunction])
}

func TestPositionAndSource(t *testing.T) {
	fn := &Function{
		LineTable: []LineEntry{{PC: 0, Line: 1, Col: 1}, {PC: 3, Line: 2, Col: 5}, {PC: 7, Line: 9, Col: 2}},
		Source:    &SourceInfo{Name: "f.js", Src: "abc function() {} tail", Start: 4, End: 17},
	}
	for _, tt := range []struct {
		pc        uint32
		line, col int
	}{{0, 1, 1}, {2, 1, 1}, {3, 2, 5}, {6, 2, 5}, {7, 9, 2}, {100, 9, 2}} {
		line, col := fn.Position(tt.pc)
		assert.Equal(t, tt.line, line, "pc %d", tt.pc)
		assert.Equal(t, tt.col, col, "pc %d", tt.pc)
	}
	assert.Equal(t, "function() {}", fn.SourceText())
	line, col := (&Function{}).Position(0)
	assert.Equal(t, 0, line)
	assert.Equal(t, 0, col)
	assert.Equal(t, "", (&Function{}).SourceText())
	assert.Equal(t, "", (&Function{Source: &SourceInfo{Src: "ab", Start: 1, End: 5}}).SourceText())
}

func TestDisassembleHandBuilt(t *testing.T) {
	fn := &Function{
		Name: "demo", Kind: KindNormal, NumParams: 1, NumRegs: 3, HasRest: true, ICCount: 1,
		Consts: []Const{
			{Kind: ConstNumber, Num: 1.5},
			{Kind: ConstString, Str: "name", IsASCII: true, Key: true},
			{Kind: ConstBigInt, Str: "12"},
			{Kind: ConstRegExp, Str: "a+", Flags: "g"},
			{Kind: ConstTemplate, Cooked: []string{"a", "b"}, Raw: []string{"a", "b"}},
			{Kind: ConstFunction, Index: 0},
		},
		Code: []uint32{
			EncodeABx(LoadConst, 1, 0),
			EncodeABC(GetProp, 2, 0, 0), ExtraArg(1, 0),
			EncodeABC(GetEnv, 2, 1, 4),
			EncodeAsBx(JmpF, 2, 1),
			EncodeABC(AddImm, 2, 2, 0xFD), // int8(-3)
			EncodeABC(TypeofIs, 2, 1, TypeNumber),
			EncodeAsBx(LoadInt, 0, -7),
			EncodeABC(Call, 0, 2, 0),
			EncodeABC(RetUndef, 0, 0, 0),
		},
		Handlers: []Handler{{Start: 0, End: 5, Handler: 6, StackDepth: 1, Kind: HandlerFinally, Reg: 2}},
		Children: []*Function{{Name: "", Kind: KindArrow, Code: []uint32{EncodeABC(RetUndef, 0, 0, 0)}}},
	}
	want := `function demo (normal) params=1+rest regs=3 env=0 ics=1
  K0 = 1.5
  K1 = "name" (key)
  K2 = 12n
  K3 = /a+/g
  K4 = template(2)
  K5 = F0
  0000 LoadConst r1 K0
  0001 GetProp r2 r0 K1 ic0
  0003 GetEnv r2 env1[4]
  0004 JmpF r2 -> 0006
  0005 AddImm r2 r2 -3
  0006 TypeofIs r2 r1 "number"
  0007 LoadInt r0 -7
  0008 Call r0 2
  0009 RetUndef
  handler [0000, 0005) -> 0006 finally r2 env=1
  F0:
  function <anonymous> (arrow) params=0 regs=0 env=0 ics=0
    0000 RetUndef
`
	assert.Equal(t, want, Disassemble(fn))
}

func TestClassKindsAndOps(t *testing.T) {
	assert.False(t, KindNormal.IsClassCtor())
	assert.False(t, KindScript.IsClassCtor())
	assert.True(t, KindClassCtor.IsClassCtor())
	assert.True(t, KindDerivedCtor.IsClassCtor())
	assert.Equal(t, "class constructor", KindClassCtor.String())
	assert.Equal(t, "derived constructor", KindDerivedCtor.String())
	fn := &Function{
		Name: "A", Kind: KindDerivedCtor, NumRegs: 4,
		Consts: []Const{{Kind: ConstString, Str: "#x", IsASCII: true}},
		Code: []uint32{
			EncodeABC(CtorEntry, 0, 0, 0),
			EncodeABx(NewPrivateName, 1, 0),
			EncodeABC(SetPrivateMethod, 1, 2, 3), PrivateStatic | PrivateGetter,
			EncodeABC(GetSuper, 2, 2, 3),
			EncodeABx(ThrowError, ThrowReferenceError, 0),
		},
	}
	want := `function A (derived constructor) params=0 regs=4 env=0 ics=0
  K0 = "#x"
  0000 CtorEntry r0
  0001 NewPrivateName r1 K0
  0002 SetPrivateMethod r1 r2 r3 5
  0004 GetSuper r2 r2 r3
  0005 ThrowError r1 K0
`
	assert.Equal(t, want, Disassemble(fn))
}

// TestEvalLevelSlot: Slot finds the first slot a name owns, in a small
// level by a scan, in a larger one by the index it builds on first use,
// which concurrent callers share.
func TestEvalLevelSlot(t *testing.T) {
	small := &EvalLevel{Names: []string{"a", "", "b", "a"}}
	assert.Equal(t, 0, small.Slot("a"))
	assert.Equal(t, 2, small.Slot("b"))
	assert.Equal(t, -1, small.Slot("c"))
	assert.Equal(t, -1, small.Slot(""), "a slot without a binding has no name")

	names := make([]string, 100)
	for i := range names {
		if i%10 != 0 {
			names[i] = "v" + strconv.Itoa(i%50)
		}
	}
	big := &EvalLevel{Names: names}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.Equal(t, 1, big.Slot("v1"))
			assert.Equal(t, 49, big.Slot("v49"))
			assert.Equal(t, 11, big.Slot("v11"), "the first of the slots of a name")
			assert.Equal(t, -1, big.Slot("v0"))
			assert.Equal(t, -1, big.Slot(""))
		}()
	}
	wg.Wait()
}

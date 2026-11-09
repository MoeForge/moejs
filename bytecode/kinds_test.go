package bytecode

import (
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterWritesCoverEveryOp checks that every opcode has a row of the
// registers it writes, inside the registers its layout names: the verifier
// takes a register a row leaves out to keep its value, and the engine's
// checked build compares the writes with the rows inside the layouts.
func TestRegisterWritesCoverEveryOp(t *testing.T) {
	encodings := []struct {
		a, b, c uint8
		x       uint32
	}{{1, 2, 1, 4}, {0, 0, 0, 0}, {3, 4, 0, 7}, {5, 3, 2, 1}}
	for op := Op(0); int(op) < OpCount; op++ {
		for _, e := range encodings {
			w := EncodeABC(op, e.a, e.b, e.c)
			var spans, writes [4]uint64
			RegisterSpans(op, w, e.x, func(r, k int) {
				for i := r; i < r+k; i++ {
					spans[i>>6] |= 1 << (i & 63)
				}
			})
			ok := RegisterWrites(op, w, e.x, func(r, k int, _ RegKind) {
				for i := r; i < r+k; i++ {
					writes[i>>6] |= 1 << (i & 63)
				}
			})
			assert.True(t, ok, "%s has no write row", op)
			for i := range writes {
				assert.Zero(t, writes[i]&^spans[i], "%s %v writes a register its layout leaves out", op, e)
			}
		}
	}
	assert.False(t, RegisterWrites(Op(OpCount), 0, 0, func(int, int, RegKind) {}))
}

// TestKindOperands pins the operands the engine reads as a kind without a
// check, which the verifier proves, and the ops whose writes give a kind.
func TestKindOperands(t *testing.T) {
	type use struct {
		r    int
		kind RegKind
	}
	uses := func(op Op) (got []use) {
		KindOperands(op, EncodeABC(op, 3, 1, 2), func(r int, kind RegKind) { got = append(got, use{r, kind}) })
		return got
	}
	for op := Op(0); int(op) < OpCount; op++ {
		assert.Equal(t, len(uses(op)) > 0, readsKinds(op), "%s", op)
	}
	assert.Equal(t, []use{{3, RegObject}}, uses(DefineField))
	assert.Equal(t, []use{{3, RegArray}}, uses(ArrayPush))
	assert.Equal(t, []use{{3, RegObject}, {4, RegIndex}}, uses(ForInNext))

	kinds := map[Op][]use{NewObject: {{3, RegObject}}, NewObjectLit: {{3, RegObject}}, ToObject: {{3, RegObject}}, NewArray: {{3, RegArray}}, NewArrayK: {{3, RegArray}},
		ForInInit: {{3, RegObject}, {4, RegIndex}}, ForInNext: {{4, RegIndex}}}
	for op := Op(0); int(op) < OpCount; op++ {
		var got []use
		RegisterWrites(op, EncodeABC(op, 3, 1, 2), 0, func(r, k int, kind RegKind) {
			if kind != RegAny {
				got = append(got, use{r, kind})
			}
		})
		assert.Equal(t, kinds[op], got, "%s", op)
	}
}

// TestVerifyKinds runs the proof of the kind operands on hand-made
// functions: where some path to the operand leaves another value in the
// register it must fail, and where every path leaves one of the kind it
// must pass. Everything the compiler emits must pass too, which every
// test that runs JavaScript checks (the engine verifies each function it
// runs).
func TestVerifyKinds(t *testing.T) {
	const x = 0 // DefineField's extra word
	abc, abx, asbx := EncodeABC, EncodeABx, EncodeAsBx
	obj := func(r uint8) uint32 { return abx(NewObject, r, 0) }
	arr := func(r uint8) uint32 { return abx(NewArray, r, 0) }
	num := func(r uint8) uint32 { return asbx(LoadInt, r, 1) }
	def := func(r uint8) []uint32 { return []uint32{abc(DefineField, r, 1, 0), x} }
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
	catch := func(start, end, at uint32, reg uint16) []Handler {
		return []Handler{{Start: start, End: end, Handler: at, Kind: HandlerCatch, Reg: reg}}
	}
	finally := func(start, end, at uint32, reg uint16) []Handler {
		return []Handler{{Start: start, End: end, Handler: at, Kind: HandlerFinally, Reg: reg}}
	}
	rows := func(hs ...[]Handler) []Handler { return slices.Concat(hs...) }
	// A throw in pc 1..2 continues at pc 4, which defines a field on R[1].
	thrown := cat(obj(1), num(2), abc(Throw, 2, 0, 0), ret, def(1), ret)
	// Two handlers: pc 3 defines a field on R[0], and so does pc 6.
	two := cat(obj(0), num(1), abc(Throw, 1, 0, 0), def(0), ret, def(0), ret)
	// pc 2 jumps over pc 3..5 to pc 6: pc 4 (ThrowConstAssign, its X
	// word at pc 5) runs with R[0] written by pc 3, pc 6 with the object.
	jumped := cat(obj(0), asbx(LoadInt, 1, 0), asbx(JmpF, 1, 3), num(0), abc(ThrowConstAssign, 0, 0, 0), uint32(0), abc(Throw, 1, 0, 0), def(0), ret)
	// pc 2 (GetGlobal, its X word at pc 3) writes R[0].
	global := cat(obj(0), num(1), abc(GetGlobal, 0, 0, 0), uint32(0), abc(Throw, 1, 0, 0), def(0), ret)
	cases := []struct {
		name     string
		code     []uint32
		handlers []Handler
		bodyPC   int // a module's, when not 0
		ok       bool
	}{
		{"an object literal", cat(obj(0), num(1), def(0), def(0), ret), nil, 0, true},
		{"a register no op writes", cat(def(0), ret), nil, 0, false},
		{"another kind written since", cat(obj(0), num(0), def(0), ret), nil, 0, false},
		{"a move of the object", cat(obj(1), abc(Move, 0, 1, 0), def(0), ret), nil, 0, false},
		{"this", cat(abc(LoadThis, 0, 0, 0), def(0), ret), nil, 0, false},
		{"this made an object", cat(abc(LoadThis, 0, 0, 0), abc(ToObject, 0, 0, 0), def(0), ret), nil, 0, true},
		// pc 1 jumps over pc 2 to pc 3 on one path.
		{"another kind on one path", cat(obj(0), asbx(JmpT, 1, 1), num(0), def(0), ret), nil, 0, false},
		{"an object on both paths", cat(obj(0), asbx(JmpT, 1, 1), obj(0), def(0), ret), nil, 0, true},
		{"an array and an object joined, as an object", cat(arr(0), asbx(JmpT, 1, 1), obj(0), def(0), ret), nil, 0, true},
		{"an array and an object joined, as an array", cat(arr(0), asbx(JmpT, 1, 1), obj(0), push(0), ret), nil, 0, false},
		{"an array on both paths", cat(arr(0), asbx(JmpT, 1, 1), arr(0), push(0), ret), nil, 0, true},
		{"an object as an array", cat(obj(0), push(0), ret), nil, 0, false},
		// The back edge at pc 4 returns to pc 1.
		{"a loop that writes it", cat(obj(0), def(0), num(0), asbx(Jmp, 0, -4), ret), nil, 0, false},
		{"a loop that writes another", cat(obj(0), def(0), num(1), asbx(Jmp, 0, -4), ret), nil, 0, true},
		// A throw in pc 1..2 continues at pc 4, past the return.
		{"a handler whose range writes it", cat(obj(0), num(0), obj(0), ret, def(0), ret), catch(1, 3, 4, 2), 0, false},
		{"a handler whose range keeps it", cat(obj(0), num(1), abc(Throw, 1, 0, 0), ret, def(0), ret), catch(1, 3, 4, 2), 0, true},
		{"a handler that writes it", cat(obj(0), num(1), abc(Throw, 1, 0, 0), ret, def(0), ret), catch(1, 3, 4, 0), 0, false},
		{"a handler that covers the op that makes it", cat(obj(0), num(1), obj(0), ret, def(0), ret), catch(1, 3, 4, 2), 0, true},
		{"a handler entered before the op that makes it", cat(num(1), obj(0), ret, def(0), ret), catch(0, 2, 3, 2), 0, false},
		// A finally writes its register and the next one.
		{"a catch below it", thrown, catch(1, 3, 4, 0), 0, true},
		{"a finally that writes it with the thrown value", thrown, finally(1, 3, 4, 0), 0, false},
		{"a finally above it", thrown, finally(1, 3, 4, 2), 0, true},
		// The first row covering the throw catches it, not the narrowest.
		{"overlapping rows, the wider first", two, rows(catch(1, 3, 3, 2), catch(2, 3, 6, 0)), 0, true},
		{"overlapping rows, the narrower first", two, rows(catch(2, 3, 6, 0), catch(1, 3, 3, 2)), 0, false},
		// A range covers the instructions that start in it.
		{"a range starting inside an instruction", jumped, catch(5, 7, 7, 2), 0, true},
		{"a range starting on that instruction", jumped, catch(4, 7, 7, 2), 0, false},
		{"a range ending before an instruction", global, catch(1, 2, 5, 3), 0, true},
		{"a range ending inside it", global, catch(1, 3, 5, 3), 0, false},
		// A resumed frame finds the value and the mode its Yield receives.
		{"a resumption that writes it", cat(obj(0), abc(Yield, 0, 2, 0), def(0), ret), nil, 0, false},
		{"a resumption that writes others", cat(obj(0), abc(Yield, 1, 3, 0), def(0), ret), nil, 0, true},
		// A call writes its block, the argument registers included.
		{"a call above it", cat(obj(0), abc(Call, 2, 1, 0), def(0), ret), nil, 0, true},
		{"a call whose block holds it", cat(obj(3), abc(Call, 2, 1, 0), def(3), ret), nil, 0, false},
		// The body of a module is an entry: pc 2, after the prologue.
		{"a module body", cat(obj(0), ret, def(0), ret), nil, 2, false},
		{"a module body that makes it", cat(obj(0), ret, obj(0), def(0), ret), nil, 2, true},
		{"code no path reaches", cat(ret, def(0), ret), nil, 0, true},
		// The for-in record R[0..3] over R[4]: ForInNext at pc 2 exits to
		// pc 5, the back edge at pc 4 returns to it.
		{"a for-in loop", cat(obj(4), abc(ForInInit, 0, 4, 0), asbx(ForInNext, 0, 2), abc(Move, 4, 3, 0), asbx(Jmp, 0, -3), ret), nil, 0, true},
		{"a for-in body that writes the index", cat(obj(4), abc(ForInInit, 0, 4, 0), asbx(ForInNext, 0, 2), num(1), asbx(Jmp, 0, -3), ret), nil, 0, false},
		{"a for-in body that writes the key list", cat(obj(4), abc(ForInInit, 0, 4, 0), asbx(ForInNext, 0, 2), num(0), asbx(Jmp, 0, -3), ret), nil, 0, false},
		{"a for-in body that writes the object", cat(obj(4), abc(ForInInit, 0, 4, 0), asbx(ForInNext, 0, 2), num(2), asbx(Jmp, 0, -3), ret), nil, 0, true},
		{"a for-in record no ForInInit made", cat(obj(0), num(1), asbx(ForInNext, 0, 0), ret), nil, 0, false},
	}
	for _, c := range cases {
		fn := &Function{Name: "f", NumRegs: 8, Consts: nameConst, ICCount: 1, Code: c.code, Handlers: c.handlers}
		if c.bodyPC != 0 {
			fn.Module = &Module{BodyPC: c.bodyPC}
		}
		err := VerifyRegisters(fn)
		if assert.Equal(t, c.ok, err == nil, "%s: %v", c.name, err) && err != nil {
			assert.Contains(t, err.Error(), "which a path to it leaves another value in", c.name)
		}
	}
}

// TestVerifyKindsWideFrames runs the proof on registers past the first 64,
// whose facts are in the later words of each set.
func TestVerifyKindsWideFrames(t *testing.T) {
	ret := EncodeABC(RetUndef, 0, 0, 0)
	for _, r := range []uint8{63, 64, 130, 255} {
		code := []uint32{EncodeABx(NewArray, r, 0), EncodeAsBx(JmpT, 0, 1), EncodeAsBx(LoadInt, 0, 1), EncodeABC(ArrayPush, r, 0, 0), ret}
		assert.NoError(t, VerifyRegisters(&Function{Name: "f", NumRegs: 256, Code: code}), "R[%d]", r)
		code[2] = EncodeAsBx(LoadInt, r, 1)
		assert.Error(t, VerifyRegisters(&Function{Name: "f", NumRegs: 256, Code: code}), "R[%d] written on one path", r)
	}
}

// TestVerifyKindsRanges checks that a span of writes clears only its
// registers' kind facts, including spans crossing a word boundary.
func TestVerifyKindsRanges(t *testing.T) {
	for _, start := range []int{0, 1, 62, 63, 64, 126, 127, 128, 254} {
		for _, count := range []int{0, 1, 2, 3, 64} {
			count = min(count, 256-start)
			for _, r := range []uint8{0, 1, 61, 62, 63, 64, 65, 126, 127, 128, 129, 254, 255} {
				fn := &Function{Name: "f", NumRegs: 256, Consts: nameConst, ICCount: 1, Code: []uint32{
					EncodeABx(NewObject, r, 0), EncodeABC(UndefRange, uint8(start), uint8(count), 0),
					EncodeABC(DefineField, r, 1, 0), 0, EncodeABC(RetUndef, 0, 0, 0)}}
				err := VerifyRegisters(fn)
				if int(r) >= start && int(r) < start+count {
					assert.ErrorContains(t, err, "which a path to it leaves another value in", "R[%d], writes %d..%d", r, start, start+count)
				} else {
					assert.NoError(t, err, "R[%d], writes %d..%d", r, start, start+count)
				}
			}
		}
	}
}

// TestVerifyKindsAllocations bounds what the proof allocates: nothing for
// a function of a few dozen blocks, and at the widest frame (256
// registers, every one a kind operand's) with every instruction starting
// a block, under 512 bytes per word of code.
func TestVerifyKindsAllocations(t *testing.T) {
	small := &Function{Name: "f", NumRegs: 8, Consts: nameConst, ICCount: 1, Code: []uint32{
		EncodeABx(NewObject, 0, 0), EncodeAsBx(JmpT, 1, 1), EncodeABx(NewObject, 0, 0), EncodeABC(DefineField, 0, 1, 0), 0, EncodeABC(RetUndef, 0, 0, 0)}}
	require.NoError(t, VerifyRegisters(small))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = VerifyRegisters(small) }))

	var code []uint32
	for r := range 256 {
		code = append(code, EncodeABx(NewObject, uint8(r), 0))
	}
	for range 4000 {
		code = append(code, EncodeAsBx(JmpT, 0, 0)) // to the next instruction: a block each
	}
	for r := range 256 {
		code = append(code, EncodeABC(DefineField, uint8(r), 0, 0), 0)
	}
	code = append(code, EncodeABC(RetUndef, 0, 0, 0))
	wide := &Function{Name: "f", NumRegs: 256, Consts: nameConst, ICCount: 1, Code: code}
	require.NoError(t, VerifyRegisters(wide))
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	err := VerifyRegisters(wide)
	runtime.ReadMemStats(&m1)
	require.NoError(t, err)
	assert.Less(t, m1.TotalAlloc-m0.TotalAlloc, uint64(512*len(code)), "%d words", len(code))
}

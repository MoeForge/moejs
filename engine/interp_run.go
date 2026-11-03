//go:build !moejsregcheck

//go:generate go run ../internal/gen/regcheck

package engine

import (
	"math"

	"github.com/MoeForge/moejs/bytecode"
)

// errThrown is the in-frame throw marker: the thrown value travels in the
// run loop's local instead of a heap-allocated *Exception, which is created
// only when the exception leaves the frame.
type thrownMarker struct{}

func (thrownMarker) Error() string { return "engine: thrown" }

var errThrown error = thrownMarker{}

// run executes the frame fi of fd whose registers start at base. It returns
// the function's return value or the error that escaped it. No panic/recover
// is involved on the throw path: natives return errors and the loop unwinds
// through the handler table.
//
// It indexes its registers through an array of windowRegs (window) with
// the register operands and no bounds checks; interp_run_regcheck.go is a
// copy of this file, made by go generate, whose run checks every one of
// those accesses (build tag moejsregcheck, regcheck.go).
func (r *Realm) run(fi int, fd *FunctionData, base int, env *Env, this Value, callee *Object) (Value, error) {
	st := &r.interp
	code := fd.code
	meta := fd.meta
	icBase := fd.icBase
	insns := code.Code
	consts := meta.consts
	top := base + int(code.NumRegs)
	regs := st.window(base)
	fr := &st.frames[fi]
	// A call starts at 0, a resumed generator at its saved pc with the
	// environment depth in the high byte (generator.go).
	pc := int(fr.pc)
	// The environment and its depth stay in memory: as registers carried
	// around the loop they would be spilled at every dispatch.
	ev := &frameEnv{env: env, depth: pc >> genDepthShift}
	pc &= 1<<genDepthShift - 1
	// A case that finishes its instruction continues the loop; only one
	// that may have failed reaches the err check after the switch. err and
	// thrown live for one instruction: carried around the loop, every case
	// would reload them for that check and the back-edge store them again.
	// For the same reason the fast paths make no calls where they can
	// avoid one: a value live across any call is spilled at every dispatch.
	for {
		var (
			err    error
			thrown Value
		)
		w := insns[pc]
		ipc := pc
		fr.pc = uint32(ipc)
		pc++
		a := int(uint8(w >> 8))
		switch bytecode.Op(w) {
		// --- loads and moves ---
		case bytecode.LoadConst:
			regs[a] = consts[w>>16]
			continue
		case bytecode.LoadInt:
			regs[a] = IntValue(int(int16(w >> 16)))
			continue
		case bytecode.LoadUndef:
			regs[a] = Undefined()
			continue
		case bytecode.LoadNull:
			regs[a] = Null()
			continue
		case bytecode.LoadTrue:
			regs[a] = True()
			continue
		case bytecode.LoadFalse:
			regs[a] = False()
			continue
		case bytecode.LoadHole:
			regs[a] = Hole()
			continue
		case bytecode.LoadThis:
			regs[a] = this
			continue
		case bytecode.LoadCallee:
			regs[a] = ObjectValue(callee)
			continue
		case bytecode.Move:
			regs[a] = regs[uint8(w>>16)]
			continue
		case bytecode.UndefRange:
			u := Undefined()
			for i := range int(uint8(w >> 16)) {
				regs[a+i] = u
			}
			continue

		// --- closure environments ---
		case bytecode.GetEnv:
			e := ev.env.up(uint8(w >> 16))
			regs[a] = e.slots[uint8(w>>24)]
			continue
		case bytecode.SetEnv:
			e := ev.env.up(uint8(w >> 16))
			e.slots[uint8(w>>24)] = regs[a]
			continue
		case bytecode.PushEnv:
			ev.env = newEnvSized(ev.env, int(w>>16))
			ev.depth++
			continue
		case bytecode.PopEnv:
			ev.env = ev.env.parent
			ev.depth--
			continue
		case bytecode.CopyEnv:
			ne := newEnvSized(ev.env.parent, len(ev.env.slots))
			copy(ne.slots, ev.env.slots)
			ev.env = ne
			continue

		// --- globals ---
		case bytecode.GetGlobal, bytecode.GetGlobalOrUndef:
			x := insns[pc]
			pc++
			g := r.Global
			e := &r.ic[icBase+x>>16]
			if g.shape == e.Shape && r.protoEpoch == e.Epoch {
				if icCensus {
					censusHit(code, bytecode.Op(w), x, ipc, g.shape)
				}
				regs[a] = e.Holder(g).slots[e.Slot()]
				continue
			}
			if icCensus {
				censusMiss(code, bytecode.Op(w), x, ipc, e, g.shape)
			}
			var v Value
			v, err = r.getGlobalSlow(meta.keys[uint16(x)], e, bytecode.Op(w) == bytecode.GetGlobalOrUndef)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = v
			}
		case bytecode.SetGlobal:
			x := insns[pc]
			pc++
			g := r.Global
			e := &r.ic[icBase+x>>16]
			if g.shape == e.Shape && r.protoEpoch == e.Epoch {
				if icCensus {
					censusHit(code, bytecode.Op(w), x, ipc, g.shape)
				}
				g.slots[e.Slot()] = regs[a]
				continue
			}
			if icCensus {
				censusMiss(code, bytecode.Op(w), x, ipc, e, g.shape)
			}
			err = r.setGlobalSlow(meta.keys[uint16(x)], regs[a], e)
			regs, fr = st.window(base), &st.frames[fi]

		// --- properties ---
		case bytecode.GetProp:
			x := insns[pc]
			pc++
			v := regs[uint8(w>>16)]
			if v.IsObject() {
				o := v.AsObject()
				e := &r.ic[icBase+x>>16]
				if o.shape == e.Shape && r.protoEpoch == e.Epoch {
					if icCensus {
						censusHit(code, bytecode.Op(w), x, ipc, o.shape)
					}
					regs[a] = e.Holder(o).slots[e.Slot()]
					continue
				}
				if icCensus {
					censusMiss(code, bytecode.Op(w), x, ipc, e, o.shape)
				}
				var res Value
				res, err = r.getNamedSlow(o, v, meta.keys[uint16(x)], e)
				regs, fr = st.window(base), &st.frames[fi]
				if err == nil {
					regs[a] = res
				}
				break
			}
			if v.IsString() {
				// Methods of string primitives resolve on String.prototype;
				// the entry caches that lookup keyed by the prototype's shape.
				sp := r.StringPrototype
				e := &r.ic[icBase+x>>16]
				if sp.shape == e.Shape && r.protoEpoch == e.Epoch {
					if icCensus {
						censusHit(code, bytecode.Op(w), x, ipc, sp.shape)
					}
					regs[a] = e.Holder(sp).slots[e.Slot()]
					continue
				}
				if icCensus {
					censusMiss(code, bytecode.Op(w), x, ipc, e, sp.shape)
				}
				var res Value
				res, err = r.getStringProp(v, meta.keys[uint16(x)], e)
				regs, fr = st.window(base), &st.frames[fi]
				if err == nil {
					regs[a] = res
				}
				break
			}
			var res Value
			res, err = r.getPrimitiveProp(v, meta.keys[uint16(x)])
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.GetLen:
			x := insns[pc]
			pc++
			v := regs[uint8(w>>16)]
			if v.IsObject() {
				o := v.AsObject()
				if o.class == ClassArray {
					regs[a] = Uint32Value(o.internal.(*ArrayData).length)
					continue
				}
				e := &r.ic[icBase+x>>16]
				if o.shape == e.Shape && r.protoEpoch == e.Epoch {
					if icCensus {
						censusHit(code, bytecode.Op(w), x, ipc, o.shape)
					}
					regs[a] = e.Holder(o).slots[e.Slot()]
					continue
				}
				if icCensus {
					censusMiss(code, bytecode.Op(w), x, ipc, e, o.shape)
				}
				var res Value
				res, err = r.getNamedSlow(o, v, lengthKey, e)
				regs, fr = st.window(base), &st.frames[fi]
				if err == nil {
					regs[a] = res
				}
				break
			}
			if v.IsString() {
				regs[a] = IntValue(v.AsString().Len())
				continue
			}
			var res Value
			res, err = r.getPrimitiveProp(v, lengthKey)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.SetProp:
			x := insns[pc]
			pc++
			t := regs[a]
			v := regs[uint8(w>>16)]
			if t.IsObject() {
				o := t.AsObject()
				e := &r.ic[icBase+x>>16]
				if o.shape == e.Shape && r.protoEpoch == e.Epoch {
					if icCensus {
						censusHit(code, bytecode.Op(w), x, ipc, o.shape)
					}
					o.slots[e.Slot()] = v
					continue
				}
				if icCensus {
					censusMiss(code, bytecode.Op(w), x, ipc, e, o.shape)
				}
				err = r.setNamedSlow(o, meta.keys[uint16(x)], v, e)
			} else {
				err = r.SetV(t, meta.keys[uint16(x)], v)
			}
			regs, fr = st.window(base), &st.frames[fi]
		case bytecode.GetElem:
			o := regs[uint8(w>>16)]
			k := regs[uint8(w>>24)]
			if o.IsObject() && k.IsNumber() {
				obj := o.AsObject()
				f := k.AsNumber()
				if i := int(f); float64(i) == f && i >= 0 && i < len(obj.elements) && obj.class != ClassString {
					if v := obj.elements[i]; !v.IsHole() {
						regs[a] = v
						continue
					}
				}
				if obj.class == ClassTypedArray {
					// What getElemSlow does first, without its call.
					ta := obj.internal.(*typedArray)
					if x, ok := ta.float64At(f); ok {
						regs[a] = NumberValue(x)
						continue
					}
					regs[a] = typedArrayGetNumber(obj, f)
					continue
				}
			}
			var res Value
			res, err = r.getElemSlow(o, k)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.SetElem:
			o := regs[a]
			k := regs[uint8(w>>16)]
			v := regs[uint8(w>>24)]
			if o.IsObject() && k.IsNumber() {
				obj := o.AsObject()
				f := k.AsNumber()
				if i := int(f); float64(i) == f && i >= 0 && i < len(obj.elements) && obj.class == ClassArray && obj.flags&(flagFrozen|flagShared) == 0 {
					if !obj.elements[i].IsHole() {
						obj.elements[i] = v
						continue
					}
				}
				if obj.class == ClassTypedArray {
					// What setElemSlow does first, without its call.
					if v.IsNumber() && obj.internal.(*typedArray).setFloat64(f, v.AsNumber()) {
						continue
					}
					err = r.typedArraySetNumber(obj, f, v)
					regs, fr = st.window(base), &st.frames[fi]
					break
				}
			}
			err = r.setElemSlow(o, k, v)
			regs, fr = st.window(base), &st.frames[fi]

		// --- literals ---
		case bytecode.NewObject:
			regs[a] = ObjectValue(r.NewObjectCap(int(w >> 16)))
			continue
		case bytecode.NewArray:
			regs[a] = ObjectValue(r.NewArrayCap(int(w >> 16)))
			continue
		case bytecode.DefineField:
			x := insns[pc]
			pc++
			// R[A] is the instance being initialized, an object: the
			// bytecode verifier proves it (bytecode.KindOperands), so run
			// reads it without a check, which would move run's register
			// allocation and the placement of its blocks and cost the
			// call path several percent.
			o := regs[a].AsObject()
			v := regs[uint8(w>>16)]
			e := &r.ic[icBase+x>>16]
			if after := e.Shape; after != nil && after.parent == o.shape && o.flags&(flagIsPrototype|flagExtensible|flagDict) == flagExtensible {
				if icCensus {
					censusHit(code, bytecode.Op(w), x, ipc, o.shape)
				}
				o.shape = after
				o.slots = append(o.slots, v)
				continue
			}
			if icCensus {
				censusMiss(code, bytecode.Op(w), x, ipc, e, o.shape)
			}
			err = r.defineFieldSlow(o, meta.keys[uint16(x)], v, e)
			regs, fr = st.window(base), &st.frames[fi]
		case bytecode.ArrayPush:
			// R[A] is the array literal, which the verifier proves an
			// array as it does DefineField's instance.
			arr := regs[a].AsObject()
			arr.Push(r, regs[uint8(w>>16)])
			continue
		case bytecode.Closure:
			o, _ := r.newClosure(code.Children[w>>16], meta.children[w>>16], ev.env, this)
			regs[a] = ObjectValue(o)
			continue

		// --- arithmetic ---
		// These cases, the comparisons and the property reads (and In,
		// InstanceOf, GetPrivate and InPrivate off the loop) write R[A]
		// only once they have a result: the compiler computes an
		// assignment's value straight into the variable's register when
		// one of them is its last op (compiler/expr.go lastOpWrites), and
		// a handler that catches the op's throw must find the variable's
		// old value there (engine TestAssignIntoRegisterThrows).
		case bytecode.Add:
			x, y := regs[uint8(w>>16)], regs[uint8(w>>24)]
			if x.IsNumber() && y.IsNumber() {
				regs[a].setScalar(NumberValue(x.AsNumber() + y.AsNumber()))
				continue
			}
			if x.IsString() && y.IsString() {
				var res Value
				if res, err = r.concatValues(x.AsString(), y.AsString()); err == nil {
					regs[a] = res
				}
				break
			}
			var res Value
			res, err = r.Add(x, y)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.Sub, bytecode.Mul, bytecode.Div, bytecode.Mod, bytecode.Exp:
			x, y := regs[uint8(w>>16)], regs[uint8(w>>24)]
			if x.IsNumber() && y.IsNumber() {
				if f, ok := numberOp(bytecode.Op(w), x.AsNumber(), y.AsNumber()); ok {
					regs[a].setScalar(NumberValue(f))
					continue
				}
				regs[a].setScalar(NumberValue(numericOp(bytecode.Op(w), x.AsNumber(), y.AsNumber())))
				continue
			}
			var res Value
			res, err = r.arithSlow(bytecode.Op(w), x, y)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.AddImm:
			x := regs[uint8(w>>16)]
			imm := float64(int8(w >> 24))
			if x.IsNumber() {
				regs[a].setScalar(NumberValue(x.AsNumber() + imm))
				continue
			}
			var res Value
			res, err = r.Add(x, NumberValue(imm))
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.SubImm:
			x := regs[uint8(w>>16)]
			imm := float64(int8(w >> 24))
			if x.IsNumber() {
				regs[a].setScalar(NumberValue(x.AsNumber() - imm))
				continue
			}
			var res Value
			res, err = r.arithSlow(bytecode.Sub, x, NumberValue(imm))
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.Inc, bytecode.Dec:
			x := regs[uint8(w>>16)]
			d := 1.0
			if bytecode.Op(w) == bytecode.Dec {
				d = -1
			}
			if x.IsNumber() {
				regs[a].setScalar(NumberValue(x.AsNumber() + d))
				continue
			}
			var res Value
			res, err = r.incSlow(x, d)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.Neg:
			x := regs[uint8(w>>16)]
			if x.IsNumber() {
				regs[a].setScalar(NumberValue(-x.AsNumber()))
				continue
			}
			var res Value
			res, err = r.negSlow(x)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.Plus:
			x := regs[uint8(w>>16)]
			if x.IsNumber() {
				regs[a] = x
				continue
			}
			var f float64
			f, err = r.ToNumber(x)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = NumberValue(f)
			}
		case bytecode.Not:
			if x := regs[uint8(w>>16)]; x.IsBool() {
				regs[a] = Bool(!x.AsBool())
			} else {
				regs[a] = Bool(!ToBoolean(x))
			}
			continue
		case bytecode.BitNot:
			x := regs[uint8(w>>16)]
			if x.IsNumber() {
				regs[a].setScalar(IntValue(int(^ToInt32Float(x.AsNumber()))))
				continue
			}
			var res Value
			res, err = r.bitNotSlow(x)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.ToNumeric:
			x := regs[uint8(w>>16)]
			if x.IsNumber() {
				regs[a] = x
				continue
			}
			var res Value
			res, err = r.ToNumeric(x)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.ToStr:
			x := regs[uint8(w>>16)]
			if x.IsString() {
				regs[a] = x
				continue
			}
			var s *String
			s, err = r.ToString(x)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = StringValue(s)
			}
		case bytecode.Concat:
			x, y := regs[uint8(w>>16)], regs[uint8(w>>24)]
			if x.IsString() && y.IsString() {
				var res Value
				if res, err = r.concatValues(x.AsString(), y.AsString()); err == nil {
					regs[a] = res
				}
				break
			}
			var res Value
			res, err = r.Add(x, y)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.Typeof:
			regs[a] = StringValue(TypeOf(regs[uint8(w>>16)]))
			continue
		case bytecode.TypeofIs:
			regs[a] = Bool(typeIndex(regs[uint8(w>>16)]) == uint8(w>>24))
			continue
		case bytecode.BitAnd, bytecode.BitOr, bytecode.BitXor, bytecode.Shl, bytecode.Shr, bytecode.UShr:
			x, y := regs[uint8(w>>16)], regs[uint8(w>>24)]
			if x.IsNumber() && y.IsNumber() {
				// Operands that are int32 already: ToInt32 is the identity.
				fx, fy := x.AsNumber(), y.AsNumber()
				if ix, iy := int32(fx), int32(fy); float64(ix) == fx && float64(iy) == fy {
					regs[a].setScalar(Int64Value(bitwiseInt32(bytecode.Op(w), ix, iy)))
					continue
				}
				regs[a].setScalar(bitwiseOp(bytecode.Op(w), fx, fy))
				continue
			}
			var res Value
			res, err = r.bitwiseSlow(bytecode.Op(w), x, y)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}

		// --- comparison ---
		case bytecode.StrictEq, bytecode.StrictNe:
			// Numbers by value, other values by identity; strings and
			// BigInts with different storage compare by content.
			x, y := regs[uint8(w>>16)], regs[uint8(w>>24)]
			var eq bool
			if x.IsNumber() && y.IsNumber() {
				eq = x.AsNumber() == y.AsNumber()
			} else if x.bits != y.bits || x.ptr == y.ptr {
				eq = x.bits == y.bits
			} else {
				eq = sameNonNumber(x, y)
			}
			regs[a] = Bool(eq == (bytecode.Op(w) == bytecode.StrictEq))
			continue
		case bytecode.Eq, bytecode.Ne:
			x, y := regs[uint8(w>>16)], regs[uint8(w>>24)]
			var res bool
			if x.Type() == y.Type() {
				res = StrictEquals(x, y)
			} else {
				res, err = r.LooseEquals(x, y)
				regs, fr = st.window(base), &st.frames[fi]
				if err != nil {
					break
				}
			}
			regs[a] = Bool(res == (bytecode.Op(w) == bytecode.Eq))
		case bytecode.Lt, bytecode.Le, bytecode.Gt, bytecode.Ge:
			x, y := regs[uint8(w>>16)], regs[uint8(w>>24)]
			if x.IsNumber() && y.IsNumber() {
				regs[a] = Bool(compareNumbers(bytecode.Op(w), x.AsNumber(), y.AsNumber()))
				continue
			}
			var res bool
			res, err = r.compareSlow(bytecode.Op(w), x, y)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = Bool(res)
			}

		// --- control flow ---
		case bytecode.Jmp:
			off := int(int16(w >> 16))
			if off < 0 && r.interruptFlag.Load() < 0 {
				return Undefined(), r.interruptError()
			}
			pc += off
			continue
		case bytecode.JmpT, bytecode.JmpF:
			v := regs[a]
			t := v.IsTrue()
			if !t && !v.IsFalse() {
				t = ToBoolean(v)
			}
			if t == (bytecode.Op(w) == bytecode.JmpT) {
				pc += int(int16(w >> 16))
			}
			continue
		case bytecode.JmpNullish:
			if regs[a].IsNullish() {
				pc += int(int16(w >> 16))
			}
			continue
		case bytecode.JmpNotNullish:
			if !regs[a].IsNullish() {
				pc += int(int16(w >> 16))
			}
			continue
		case bytecode.JmpNotUndef:
			if !regs[a].IsUndefined() {
				pc += int(int16(w >> 16))
			}
			continue
		case bytecode.JmpLt, bytecode.JmpLe, bytecode.JmpGt, bytecode.JmpGe, bytecode.JmpNLt, bytecode.JmpNLe, bytecode.JmpNGt, bytecode.JmpNGe:
			// Lt .. Ge of R[A] and R[X] deciding the jump: the first four
			// jump when it holds, the N forms when it does not (NaN).
			k := bytecode.Op(w) - bytecode.JmpLt
			x, y := regs[a], regs[uint8(insns[pc])]
			var res bool
			if x.IsNumber() && y.IsNumber() {
				res = compareNumbers(bytecode.Lt+k&3, x.AsNumber(), y.AsNumber())
			} else {
				res, err = r.compareSlow(bytecode.Lt+k&3, x, y)
				regs, fr = st.window(base), &st.frames[fi]
				if err != nil {
					break
				}
			}
			if off := int(int16(w >> 16)); off < 0 && r.interruptFlag.Load() < 0 {
				// The back edge of a rotated loop, taken or not (its exit).
				return Undefined(), r.interruptError()
			} else if res == (k < 4) {
				pc += off // relative to the X word
			} else {
				pc++
			}
			continue
		case bytecode.JmpLtI, bytecode.JmpLeI, bytecode.JmpGtI, bytecode.JmpGeI, bytecode.JmpNLtI, bytecode.JmpNLeI, bytecode.JmpNGtI, bytecode.JmpNGeI:
			// JmpLt .. JmpNGe against the integer in the X word.
			k := bytecode.Op(w) - bytecode.JmpLtI
			x, y := regs[a], int(int32(insns[pc]))
			var res bool
			if x.IsNumber() {
				res = compareNumbers(bytecode.Lt+k&3, x.AsNumber(), float64(y))
			} else {
				res, err = r.compareSlow(bytecode.Lt+k&3, x, IntValue(y))
				regs, fr = st.window(base), &st.frames[fi]
				if err != nil {
					break
				}
			}
			if off := int(int16(w >> 16)); off < 0 && r.interruptFlag.Load() < 0 {
				// The back edge of a rotated loop, taken or not (its exit).
				return Undefined(), r.interruptError()
			} else if res == (k < 4) {
				pc += off // relative to the X word
			} else {
				pc++
			}
			continue
		case bytecode.JmpStrictEqI, bytecode.JmpStrictNeI:
			// A value that is not a number reads as a NaN payload or a
			// denormal (its pointer tag), never as an integer.
			eq := regs[a].AsNumber() == float64(int32(insns[pc]))
			if eq == (bytecode.Op(w) == bytecode.JmpStrictEqI) {
				pc += int(int16(w >> 16))
			} else {
				pc++
			}
			continue
		case bytecode.JmpStrictEq, bytecode.JmpStrictNe:
			x, y := regs[a], regs[uint8(insns[pc])]
			var eq bool
			if x.IsNumber() && y.IsNumber() {
				eq = x.AsNumber() == y.AsNumber()
			} else if x.bits != y.bits || x.ptr == y.ptr {
				eq = x.bits == y.bits
			} else {
				eq = sameNonNumber(x, y)
			}
			if eq == (bytecode.Op(w) == bytecode.JmpStrictEq) {
				pc += int(int16(w >> 16))
			} else {
				pc++
			}
			continue

		// --- calls ---
		case bytecode.Call:
			argc := int(uint8(w >> 16))
			if w>>24 == bytecode.CallUndefThis {
				// A plain call has a copy of its own: choosing this before
				// one shared call took 5 instructions more per iteration of
				// a loop that makes no call (Micro/propGetMono).
				var res Value
				res, err = r.callValue(regs[a], Undefined(), st.stack[base+a+2:base+a+2+argc:base+a+2+argc])
				regs, fr = st.window(base), &st.frames[fi]
				if err == nil {
					regs[a] = res
				}
				break
			}
			var res Value
			res, err = r.callValue(regs[a], regs[a+1], st.stack[base+a+2:base+a+2+argc:base+a+2+argc])
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.New:
			argc := int(uint8(w >> 16))
			var res Value
			res, err = r.constructValue(regs[a], st.stack[base+a+2:base+a+2+argc:base+a+2+argc])
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				regs[a] = res
			}
		case bytecode.Ret:
			return regs[a], nil
		case bytecode.RetUndef:
			return Undefined(), nil
		case bytecode.Throw:
			thrown = regs[a]
			err = errThrown

		// --- iteration ---
		// Only the index fast paths run inline; iterOp takes the rest.
		case bytecode.IterInit:
			if v := regs[uint8(w>>16)]; r.fastIterable(v) {
				regs[a] = v
				regs[a+1] = IntValue(0)
				continue
			}
			pc, err = r.iterOp(fd, base, w, pc)
			regs, fr = st.window(base), &st.frames[fi]
		case bytecode.IterNext:
			if it, pos := regs[a], regs[a+1]; it.IsObject() && pos.IsNumber() { // array fast mode
				o := it.AsObject()
				if i := int(pos.AsNumber()); i < len(o.elements) && i < int(o.internal.(*ArrayData).length) {
					if v := o.elements[i]; !v.IsHole() {
						regs[a+1] = IntValue(i + 1)
						regs[a+2] = v
						continue
					}
				}
			}
			pc, err = r.iterNext(fd, base, w, pc)
			regs, fr = st.window(base), &st.frames[fi]
		case bytecode.IterValue, bytecode.IterRest, bytecode.IterClose:
			pc, err = r.iterOp(fd, base, w, pc)
			regs, fr = st.window(base), &st.frames[fi]
		case bytecode.IterThrow:
			thrown = regs[uint8(w>>16)]
			pc, err = r.iterOp(fd, base, w, pc)
			regs, fr = st.window(base), &st.frames[fi]
			if err == nil {
				err = errThrown
			}
		case bytecode.ForInNext:
			// The record's key list and index are an object and an
			// index, which the verifier proves as it does DefineField's
			// instance (forInNext).
			k, ok := forInNext(&regs[a], &regs[a+1], regs[a+2])
			if !ok {
				pc += int(int16(w >> 16))
				continue
			}
			regs[a+3] = k
			continue
		case bytecode.GetElemRef:
			// GetElem whose key stays in R[C] for the SetElem of a compound
			// assignment or update. A dense element read needs no
			// conversion; coldOp reads any other. The read continues the
			// loop (err is nil): a break to the code after the switch
			// changes the register allocation of every case's back-edge.
			k := regs[uint8(insns[pc])]
			if o := regs[uint8(w>>16)]; o.IsObject() && k.IsNumber() {
				obj := o.AsObject()
				f := k.AsNumber()
				if i := int(f); float64(i) == f && i >= 0 && i < len(obj.elements) && obj.class != ClassString {
					if v := obj.elements[i]; !v.IsHole() {
						regs[uint8(w>>24)] = k
						regs[a] = v
						pc++
						continue
					}
				}
			}
			fallthrough
		default:
			pc, err = r.coldOp(fd, base, ev.env, w, pc) // rare ops, then class ops (interp_cold.go)
			regs, fr = st.window(base), &st.frames[fi]
		}

		if err == nil {
			continue
		}
		// --- unwinding ---
		if err != errThrown {
			switch e := err.(type) {
			case *InterruptedError:
				return Undefined(), err
			case *Exception:
				thrown = e.Value
			case *genFrame:
				return e.suspend(ev.env, ev.depth), nil // a generator suspends
			case frameOpMarker: // CoerceThis, MapArguments, CallEval (sloppy.go)
				var coerced Value
				if coerced, err = r.frameOp(insns[st.frames[fi].pc], code, st.stack[base:top:top], ev.env, this, callee); err == errFrameOp {
					// The loop never assigns this (a loop-carried this
					// slows every instruction) and does not reload regs
					// and fr after a direct eval: the frame reruns from
					// the next instruction, as a generator resumes.
					st.frames[fi].pc = uint32(pc | ev.depth<<genDepthShift)
					return r.run(fi, fd, base, ev.env, coerced, callee)
				}
				if err == nil {
					continue
				}
				// A direct eval's error, handled here: a jump back to
				// the unwinding code would change the register
				// allocation of the whole loop.
				var ok bool
				if thrown, ok = r.thrownValue(err); !ok {
					return Undefined(), err // an interrupt
				}
			default:
				thrown = r.hostErrorValue(err)
			}
		}
		h := findHandler(code.Handlers, st.frames[fi].pc)
		if h == nil {
			if err == errThrown {
				return Undefined(), &Exception{Value: thrown}
			}
			if _, ok := err.(*Exception); ok {
				return Undefined(), err
			}
			return Undefined(), &Exception{Value: thrown}
		}
		for ev.depth > int(h.StackDepth) {
			ev.env = ev.env.parent
			ev.depth--
		}
		regs, fr = st.window(base), &st.frames[fi]
		if h.Kind == bytecode.HandlerCatch {
			regs[h.Reg] = thrown
		} else {
			regs[h.Reg] = IntValue(1)
			regs[h.Reg+1] = thrown
		}
		pc = int(h.Handler)
	}
}

// getElemRef is GetElemRef off run's dense element read. GetValue
// converts an object key once, after ToObject of the base, so a nullish
// base keeps it and getElemSlow throws the TypeError. It returns the key
// for PutValue.
func (r *Realm) getElemRef(o, k Value) (Value, Value, error) {
	if k.IsObject() && !o.IsNullish() {
		key, err := r.ToPropertyKey(k)
		if err != nil {
			return k, Undefined(), err
		}
		k = key.Value()
	}
	res, err := r.getElemSlow(o, k)
	return k, res, err
}

// frameEnv is the environment of a running frame and the number of
// PushEnv scopes above the one it started with.
type frameEnv struct {
	env   *Env
	depth int
}

// findHandler returns the innermost handler row covering pc, or nil.
func findHandler(handlers []bytecode.Handler, pc uint32) *bytecode.Handler {
	for i := range handlers {
		h := &handlers[i]
		if pc >= h.Start && pc < h.End {
			return h
		}
	}
	return nil
}

// typeIndex maps a value to its bytecode.TypeNames index (typeof category).
func typeIndex(v Value) uint8 {
	switch v.Type() {
	case TypeUndefined:
		return bytecode.TypeUndefined
	case TypeNull:
		return bytecode.TypeObject
	case TypeBoolean:
		return bytecode.TypeBoolean
	case TypeNumber:
		return bytecode.TypeNumber
	case TypeString:
		return bytecode.TypeString
	case TypeSymbol:
		return bytecode.TypeSymbol
	case TypeBigInt:
		return bytecode.TypeBigInt
	case TypeObject:
		if v.AsObject().IsCallable() {
			return bytecode.TypeFunction
		}
		return bytecode.TypeObject
	}
	return bytecode.TypeUndefined
}

// compareNumbers applies a relational operator (Lt .. Ge) to two numbers.
func compareNumbers(op bytecode.Op, x, y float64) bool {
	switch op {
	case bytecode.Lt:
		return x < y
	case bytecode.Le:
		return x <= y
	case bytecode.Gt:
		return x > y
	}
	return x >= y
}

// numberOp applies -, * and / to two numbers, and % when both are positive
// integers up to 2^53 (the remainder is exact in int64 and has the
// dividend's sign). It reports false for the rest, which numericOp
// computes with a call.
func numberOp(op bytecode.Op, x, y float64) (float64, bool) {
	switch op {
	case bytecode.Sub:
		return x - y, true
	case bytecode.Mul:
		return x * y, true
	case bytecode.Div:
		return x / y, true
	case bytecode.Mod:
		if x >= 1 && x <= 1<<53 && y >= 1 && y <= 1<<53 {
			if xi, yi := int64(x), int64(y); float64(xi) == x && float64(yi) == y {
				return float64(xi % yi), true
			}
		}
	}
	return 0, false
}

// numericOp applies a binary arithmetic operator to two numbers.
func numericOp(op bytecode.Op, x, y float64) float64 {
	switch op {
	case bytecode.Sub:
		return x - y
	case bytecode.Mul:
		return x * y
	case bytecode.Div:
		return x / y
	case bytecode.Mod:
		return jsMod(x, y)
	case bytecode.Exp:
		return jsPow(x, y)
	}
	return x + y
}

// jsMod implements Number::remainder. Integral operands of magnitude up to
// 2^53 take an exact int64 remainder instead of math.Mod's frexp/ldexp loop;
// a zero remainder has the dividend's sign (-7 % 7 and -0 % 3 are -0).
func jsMod(x, y float64) float64 {
	const lim = 1 << 53
	if x >= -lim && x <= lim && y >= -lim && y <= lim {
		if xi, yi := int64(x), int64(y); float64(xi) == x && float64(yi) == y && yi != 0 {
			if r := xi % yi; r != 0 {
				return float64(r)
			}
			return math.Copysign(0, x)
		}
	}
	return math.Mod(x, y)
}

// jsPow implements Number::exponentiate, which differs from math.Pow for
// |base| == 1 with an infinite exponent (NaN in JavaScript).
func jsPow(x, y float64) float64 {
	if y != y {
		return math.NaN()
	}
	if y == 0 {
		return 1
	}
	if math.IsInf(y, 0) && (x == 1 || x == -1) {
		return math.NaN()
	}
	return math.Pow(x, y)
}

// bitwiseOp applies a bitwise or shift operator to two numbers.
func bitwiseOp(op bytecode.Op, x, y float64) Value {
	return Int64Value(bitwiseInt32(op, ToInt32Float(x), ToInt32Float(y)))
}

// bitwiseInt32 applies a bitwise or shift operator to two int32 operands.
// Every result is exact as a number (>>> gives a uint32).
func bitwiseInt32(op bytecode.Op, a, b int32) int64 {
	switch op {
	case bytecode.BitAnd:
		return int64(a & b)
	case bytecode.BitOr:
		return int64(a | b)
	case bytecode.BitXor:
		return int64(a ^ b)
	case bytecode.Shl:
		return int64(a << (b & 31))
	case bytecode.Shr:
		return int64(a >> (b & 31))
	}
	return int64(uint32(a) >> (b & 31))
}

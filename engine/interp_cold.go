package engine

import "github.com/MoeForge/moejs/bytecode"

// coldOp executes one op that the interpreter loop leaves to its default
// case, and returns the next pc. These are ops that are rare in loops (TDZ
// and import reads, wide environment slots, object-literal shapes beyond
// plain fields, spread calls, for-in setup, an iterator's close before a
// throw) or that call a slow helper on every execution anyway (delete, in,
// instanceof), so the extra call costs little next to their work. Keeping
// them out of (*Realm).run keeps it below the compiler's big-function size
// (DESIGN §6). Ops it does not handle go on to classOp. The compiler
// emits sloppy code's named property writes as SetProp's sloppy form
// (bytecode.SetPropSloppyFlag), which run executes in SetProp's case;
// SetPropSloppy, which it no longer emits, is executed here for bytecode
// that has it.
//
// Everything here may re-enter the interpreter: a result is written through
// st.stack after any call, never through a register slice taken before it.
// pc is the word after the instruction's first; coldOp first stores the
// instruction's own in the innermost frame's record, which is run's.
func (r *Realm) coldOp(fd *FunctionData, base int, env *Env, w uint32, pc int) (int, error) {
	r.interp.frames[r.interp.nframes-1].pc = uint32(pc - 1)
	// An object literal's (objectLit) first: run has no case for it, which
	// would move the blocks of its hot cases.
	if bytecode.Op(w) == bytecode.NewObjectLit {
		return r.objectLit(fd, base, w, pc), nil
	}
	// The async function ops first: they use none of the operands below.
	if op := bytecode.Op(w); op-bytecode.AsyncGenStart <= bytecode.AsyncThrow-bytecode.AsyncGenStart {
		switch op {
		case bytecode.Await:
			g, regs, err := r.await(fd, base, w)
			if g == nil {
				return pc, err
			}
			return pc, g.save(regs, pc)
		case bytecode.AsyncReturn, bytecode.AsyncThrow:
			return pc, r.asyncEnd(fd, base, w)
		}
		return r.asyncOp(fd, base, w, pc)
	}
	st := &r.interp
	code, meta := fd.code, fd.meta
	insns := code.Code
	top := base + int(code.NumRegs)
	regs := st.stack[base:top:top]
	a, b, c := int(uint8(w>>8)), int(uint8(w>>16)), int(uint8(w>>24))
	switch bytecode.Op(w) {
	case bytecode.JmpCaseK:
		return caseChain(fd, pc-1, regs[a]), nil
	case bytecode.MapArguments:
		// The prologue of every call of a sloppy function with simple
		// parameters that uses arguments: env is the function's own Env
		// and the innermost frame record's function the callee.
		v := regs[a]
		if !v.IsObject() || v.AsObject().class != ClassArguments {
			return pc, r.wrongKind(w, "an arguments object")
		}
		r.mapArguments(v.AsObject(), code, env, st.frames[st.nframes-1].fn, b != 0)
		return pc, nil
	// --- a sloppy property write in its own op (not emitted by the compiler) ---
	case bytecode.SetPropSloppy:
		x := insns[pc]
		t, v := regs[a], regs[b]
		if !t.IsObject() {
			return pc + 1, r.setVSloppy(t, meta.keys[uint16(x)], v)
		}
		o := t.AsObject()
		e := &r.ic[fd.icBase+x>>16]
		if o.shape == e.Shape && r.protoEpoch == e.Epoch {
			o.slots[e.Slot()] = v
			return pc + 1, nil
		}
		return pc + 1, r.setNamedSloppy(o, meta.keys[uint16(x)], v, e)

	// --- closure environments and imports ---
	case bytecode.GetEnvChk:
		v := env.up(uint8(b)).slots()[c]
		if v.IsHole() {
			return pc + 1, r.tdzError(meta, insns[pc])
		}
		regs[a] = v
		return pc + 1, nil
	case bytecode.GetEnvW:
		regs[a] = env.up(uint8(b)).slots()[insns[pc]]
		return pc + 1, nil
	case bytecode.GetEnvChkW:
		v := env.up(uint8(b)).slots()[insns[pc]]
		if v.IsHole() {
			return pc + 2, r.tdzError(meta, insns[pc+1])
		}
		regs[a] = v
		return pc + 2, nil
	case bytecode.SetEnvW:
		env.up(uint8(b)).slots()[insns[pc]] = regs[a]
		return pc + 1, nil
	case bytecode.GetImport:
		ref := env.up(uint8(b)).slots()[c]
		if !ref.isImportRef() {
			return pc + 1, r.wrongKind(w, "an import binding")
		}
		v := *ref.importTarget()
		if v.IsHole() {
			return pc + 1, r.tdzError(meta, insns[pc])
		}
		regs[a] = v
		return pc + 1, nil
	case bytecode.GetImportW:
		ref := env.up(uint8(b)).slots()[insns[pc]]
		if !ref.isImportRef() {
			return pc + 2, r.wrongKind(w, "an import binding")
		}
		v := *ref.importTarget()
		if v.IsHole() {
			return pc + 2, r.tdzError(meta, insns[pc+1])
		}
		regs[a] = v
		return pc + 2, nil
	case bytecode.CheckTDZ:
		if regs[a].IsHole() {
			return pc + 1, r.tdzError(meta, insns[pc])
		}
		return pc + 1, nil

	// --- properties ---
	case bytecode.DelProp:
		res, err := r.deleteSlow(regs[b], meta.keys[insns[pc]].Value())
		if err == nil {
			st.stack[base+a] = Bool(res)
		}
		return pc + 1, err
	case bytecode.DelElem:
		res, err := r.deleteSlow(regs[b], regs[c])
		if err == nil {
			st.stack[base+a] = Bool(res)
		}
		return pc, err
	case bytecode.In:
		res, err := r.HasPropertyIn(regs[b], regs[c])
		if err == nil {
			st.stack[base+a] = Bool(res)
		}
		return pc, err
	case bytecode.InstanceOf:
		res, ok := r.instanceOfPlain(regs[b], regs[c])
		if !ok {
			var err error
			if res, err = r.instanceOfSlow(regs[b], regs[c]); err != nil {
				return pc, err
			}
		}
		st.stack[base+a] = Bool(res)
		return pc, nil
	case bytecode.GetElemRef:
		// Past run's dense element read.
		k, v, err := r.getElemRef(regs[b], regs[uint8(insns[pc])])
		if err != nil {
			return pc + 1, err
		}
		st.stack[base+c] = k
		st.stack[base+a] = v
		return pc + 1, nil

	// --- literals ---
	// The object a literal builds is in R[A]; the compiler puts it there.
	case bytecode.DefineElem:
		o, err := r.objectArg(regs[a], w)
		if err != nil {
			return pc, err
		}
		return pc, r.defineElemSlow(o, regs[b], regs[c])
	case bytecode.DefineMethod:
		o, err := r.objectArg(regs[a], w)
		if err != nil {
			return pc, err
		}
		if _, _, err := r.closureArg(regs[c], w); err != nil {
			return pc, err
		}
		return pc, r.defineMethodSlow(o, regs[b], regs[c])
	case bytecode.DefineAccessor:
		o, err := r.objectArg(regs[a], w)
		if err != nil {
			return pc + 1, err
		}
		fn, _, err := r.closureArg(regs[c], w)
		if err != nil {
			return pc + 1, err
		}
		return pc + 1, r.defineAccessor(o, regs[b], fn, insns[pc])
	case bytecode.SetProto:
		o, err := r.objectArg(regs[a], w)
		if err != nil {
			return pc, err
		}
		if v := regs[b]; v.IsObject() {
			o.SetPrototypeOf(r, v.AsObject())
		} else if v.IsNull() {
			o.SetPrototypeOf(r, nil)
		}
		return pc, nil
	case bytecode.CopyDataProps, bytecode.CopyDataPropsEx:
		o, err := r.objectArg(regs[a], w)
		if err != nil {
			return pc, err
		}
		excluded := Undefined()
		if bytecode.Op(w) == bytecode.CopyDataPropsEx {
			excluded = regs[c]
		} else if regs[b].IsObject() {
			r.spreadPairRoom(o, regs[b].AsObject(), spreadPairSource(w, insns[pc:], regs, env))
		}
		return pc, r.copyDataProps(o, regs[b], excluded)
	case bytecode.ArrayHole:
		arr, err := r.arrayArg(regs[a], w)
		if err != nil {
			return pc, err
		}
		arr.setElems(r.growWithHoles(arr.elems(), arr.elemLen()+1))
		arr.setArrayLen(uint32(arr.elemLen()))
		return pc, nil
	case bytecode.AppendSpread:
		arr, err := r.arrayArg(regs[a], w)
		if err != nil {
			return pc, err
		}
		return pc, r.appendSpread(arr, regs[b])
	case bytecode.NewRegExp:
		// RegExpCreate with %RegExp.prototype% (the constructor's
		// prototype property is immutable, so this equals constructing
		// through %RegExp%).
		bx := w >> 16 // a ConstRegExp (bytecode.VerifyRegisters)
		rx, err := r.NewRegExp(meta.regexpConst(bx))
		if err == nil {
			st.stack[base+a] = ObjectValue(rx)
		}
		return pc, err
	case bytecode.RegExpTest:
		// The call /lit/.test(x) that follows: of a string, while test
		// and exec are the originals, no code could see the literal's
		// object, so the match runs without one.
		x := insns[pc]
		if v := regs[bytecode.ExtraHi(x)]; v.IsString() && r.regexpProtoIntact(rxTest|rxExec) {
			ok, done, err := r.literalTest(meta, uint32(bytecode.ExtraLo(x)), v.AsString())
			if done {
				if err == nil {
					st.stack[base+a] = Bool(ok)
				}
				return pc + int(int16(w>>16)), err
			}
		}
		return pc + 1, nil
	case bytecode.GetTemplate:
		x := insns[pc]
		regs[a] = ObjectValue(r.templateObject(&code.Consts[uint16(x)], &r.ic[fd.icBase+x>>16]))
		return pc + 1, nil

	// --- calls ---
	case bytecode.CallSpread, bytecode.NewSpread:
		// The list goes above the stack pointer, past the registers and
		// any arguments the frame keeps (ForwardArguments).
		sp := st.sp
		argv, err := r.spreadArgs(sp, regs[a+2])
		if err != nil {
			return pc, err
		}
		regs = st.stack[base:top:top]
		var res Value
		st.sp = sp + len(argv) // keep the callee's window above the argument list
		if bytecode.Op(w) == bytecode.CallSpread {
			res, err = r.callValue(regs[a], regs[a+1], argv)
		} else {
			res, err = r.constructValue(regs[a], argv)
		}
		st.sp = sp
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.ApplyArguments:
		res, err := r.applyArguments(fd, base, env, w)
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.ArgsLen:
		res, err := r.argsLen(fd, base, w)
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.ArgsGet:
		res, err := r.argsGet(fd, base, env, w, insns[pc])
		if err == nil {
			st.stack[base+a] = res
		}
		return pc + 1, err
	case bytecode.ArrayPushK:
		// R[A] is the array literal, which the verifier proves an array
		// (ArrayPush); the constants are numbers, strings, null and
		// undefined, which push as they are.
		x := insns[pc]
		lo, n := int(uint16(x)), int(x>>16)
		regs[a].AsObject().pushConsts(r, meta.consts[lo:lo+n:lo+n])
		return pc + 1, nil
	case bytecode.NewArrayK:
		// The constants are those ArrayPushK pushes (the verifier's check);
		// dynamic code does not share them (newArrayConsts).
		x := insns[pc]
		lo, n := int(uint16(x)), int(x>>16)
		regs[a] = ObjectValue(r.newArrayConsts(meta.consts[lo:lo+n:lo+n], meta.dyn&dynFlag == 0))
		return pc + 1, nil
	// --- operators rare in loops, off run's jump table ---
	// Like run's arithmetic, they write R[A] only once they have a result.
	case bytecode.LoadHole:
		regs[a] = Hole()
		return pc, nil
	case bytecode.Neg:
		if x := regs[b]; x.IsNumber() {
			regs[a] = NumberValue(-x.AsNumber())
			return pc, nil
		}
		res, err := r.negSlow(regs[b])
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.Plus:
		if x := regs[b]; x.IsNumber() {
			regs[a] = x
			return pc, nil
		}
		f, err := r.ToNumber(regs[b])
		if err == nil {
			st.stack[base+a] = NumberValue(f)
		}
		return pc, err
	case bytecode.BitNot:
		if x := regs[b]; x.IsNumber() {
			regs[a] = IntValue(int(^ToInt32Float(x.AsNumber())))
			return pc, nil
		}
		res, err := r.bitNotSlow(regs[b])
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.Typeof:
		regs[a] = StringValue(TypeOf(regs[b]))
		return pc, nil
	// --- operands of run's arithmetic that are not numbers ---
	// run computes these ops on numbers and sends the rest here.
	case bytecode.Inc, bytecode.Dec:
		d := 1.0
		if bytecode.Op(w) == bytecode.Dec {
			d = -1
		}
		res, err := r.incSlow(regs[b], d)
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.ToNumeric:
		// The update of a constant, before it throws (PostInc and PostDec
		// convert a postfix update's value).
		res, err := r.ToNumeric(regs[b])
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.PostInc, bytecode.PostDec:
		// ToNumeric A B, then Inc or Dec B A.
		old, err := r.ToNumeric(regs[b])
		if err != nil {
			return pc, err
		}
		st.stack[base+a] = old
		d := 1.0
		if bytecode.Op(w) == bytecode.PostDec {
			d = -1
		}
		res, err := r.incSlow(old, d)
		if err == nil {
			st.stack[base+b] = res
		}
		return pc, err
	case bytecode.GetElemInc:
		// Inc C C, then GetElem A B C.
		k, err := r.incSlow(regs[c], 1)
		if err != nil {
			return pc, err
		}
		st.stack[base+c] = k
		res, err := r.getElemSlow(st.stack[base+b], k)
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.Sub, bytecode.Mul, bytecode.Div, bytecode.Mod, bytecode.Exp:
		res, err := r.arithSlow(bytecode.Op(w), regs[b], regs[c])
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.BitAnd, bytecode.BitOr, bytecode.BitXor, bytecode.Shl, bytecode.Shr, bytecode.UShr:
		res, err := r.bitwiseSlow(bytecode.Op(w), regs[b], regs[c])
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.AddImm, bytecode.SubImm:
		imm := NumberValue(float64(int8(w >> 24)))
		var res Value
		var err error
		if bytecode.Op(w) == bytecode.AddImm {
			res, err = r.Add(regs[b], imm)
		} else {
			res, err = r.arithSlow(bytecode.Sub, regs[b], imm)
		}
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.ToStr:
		s, err := r.ToString(regs[b])
		if err == nil {
			st.stack[base+a] = StringValue(s)
		}
		return pc, err
	case bytecode.Concat:
		res, err := r.Add(regs[b], regs[c])
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
	case bytecode.Eq, bytecode.Ne:
		res, err := r.LooseEquals(regs[b], regs[c])
		if err == nil {
			st.stack[base+a] = Bool(res == (bytecode.Op(w) == bytecode.Eq))
		}
		return pc, err
	case bytecode.Lt, bytecode.Le, bytecode.Gt, bytecode.Ge:
		res, err := r.compareSlow(bytecode.Op(w), regs[b], regs[c])
		if err == nil {
			st.stack[base+a] = Bool(res)
		}
		return pc, err
	case bytecode.JmpLt, bytecode.JmpLe, bytecode.JmpGt, bytecode.JmpGe, bytecode.JmpNLt, bytecode.JmpNLe, bytecode.JmpNGt, bytecode.JmpNGe:
		k := bytecode.Op(w) - bytecode.JmpLt
		if x := insns[pc]; x >= bytecode.EnvOperand {
			return r.compareJump(w, k, regs[a], env.slots()[uint8(x>>8)], pc)
		}
		return r.compareJump(w, k, regs[a], regs[uint8(insns[pc])], pc)
	case bytecode.JmpLtI, bytecode.JmpLeI, bytecode.JmpGtI, bytecode.JmpGeI, bytecode.JmpNLtI, bytecode.JmpNLeI, bytecode.JmpNGtI, bytecode.JmpNGeI:
		k := bytecode.Op(w) - bytecode.JmpLtI
		return r.compareJump(w, k, regs[a], IntValue(int(int32(insns[pc]))), pc)

	// --- receivers of run's property ops that are not objects ---
	// run reads objects, a string's methods and length, and sends the rest
	// here. pc is at the X word, as for the ops above.
	case bytecode.GetProp:
		x := insns[pc]
		v, key, e := regs[b], meta.keys[uint16(x)], &r.ic[fd.icBase+x>>16]
		var res Value
		var err error
		switch {
		case v.IsObject():
			res, err = r.getPropSlow(v.AsObject(), v, key, e)
		case v.IsString():
			res, err = r.getStringProp(v, key, e)
		default:
			res, err = r.getPrimitiveProp(v, key)
		}
		if err == nil {
			st.stack[base+a] = res
		}
		return pc + 1, err
	case bytecode.GetLen:
		res, err := r.getPrimitiveProp(regs[b], lengthKey)
		if err == nil {
			st.stack[base+a] = res
		}
		return pc + 1, err
	case bytecode.SetProp:
		return pc + 1, r.setPropV(regs[a], meta.keys[uint16(insns[pc])], regs[b])

	case bytecode.ThrowConstAssign:
		return pc + 1, r.TypeError("Assignment to constant variable.")
	case bytecode.RequireObjectCoercible:
		if v := regs[a]; v.IsNullish() {
			return pc, r.TypeError("Cannot destructure '%s' as it is %s.", v.String(), v.String())
		}
		return pc, nil
	case bytecode.ToObject:
		// The with statement's object, and the `this` of a class's member
		// initializers, once per object constructed (compiler
		// loadThisObject): here rather than among the sloppy ops, four
		// calls further.
		o, err := r.ToObject(regs[b])
		if err == nil {
			regs[a] = ObjectValue(o)
		}
		return pc, err

	// --- async functions ---
	// The ops every call of an async function runs and every await: here,
	// they skip the class and generator ops' hops on the way to asyncOp.
	// --- iteration ---
	case bytecode.IterThrow:
		v := regs[b]
		pc, err := r.iterOp(fd, base, w, pc)
		if err == nil {
			err = r.Throw(v)
		}
		return pc, err
	case bytecode.ForInInit:
		keys, obj, err := r.forInInit(regs[b])
		if err == nil {
			regs = st.stack[base:top:top]
			regs[a] = keys
			regs[a+1] = IntValue(0)
			regs[a+2] = obj
		}
		return pc, err
	}
	return r.classOp(fd, base, w, pc) // class ops (class.go)
}

// wrongKind is the error of the instruction w when an operand it takes from
// the compiler as want (an object, an array, a function, a generator, a
// private name, an import binding) holds another kind of value: bytecode no
// compiler made, or a compiler bug. The payload accessors read a value as
// the kind they name without looking (value_as.go), so each op that trusts
// the compiler for a kind checks it first and fails with this, a TypeError
// as the engine's other internal errors are ("internal: unknown opcode"),
// rather than read one kind of value as another. Three sites in run do not
// (DefineField's instance, ArrayPush's literal, ForInNext's record): a
// check there costs run's own code generation, and the bytecode verifier
// proves those kinds instead (bytecode.KindOperands). The checked build's
// accessors check every read (value_as_regcheck.go).
//
//go:noinline
func (r *Realm) wrongKind(w uint32, want string) error {
	return r.TypeError("internal: %s expects %s", bytecode.Op(w), want)
}

// objectArg returns the object in v, an operand of w the compiler makes an
// object, or wrongKind.
func (r *Realm) objectArg(v Value, w uint32) (*Object, error) {
	if v.IsObject() {
		return v.AsObject(), nil
	}
	return nil, r.wrongKind(w, "an object")
}

// arrayArg is objectArg for an array literal: an array of the realm's own,
// whose elements the op writes directly (a shared intrinsic is frozen and
// every realm's).
func (r *Realm) arrayArg(v Value, w uint32) (*Object, error) {
	if v.IsObject() {
		if o := v.AsObject(); o.class == ClassArray && !o.IsShared() {
			return o, nil
		}
	}
	return nil, r.wrongKind(w, "an array literal")
}

// closureArg is objectArg for a closure of bytecode (the function a
// Closure made), with its FunctionData: the ops that take one write its
// internals directly (its home object, name, prototype or class), which a
// native or a shared intrinsic must not get.
func (r *Realm) closureArg(v Value, w uint32) (*Object, *FunctionData, error) {
	if v.IsObject() {
		o := v.AsObject()
		if fd, ok := dataOk(o, kindFunction); ok && fd.code != nil && !o.IsShared() {
			return o, fd, nil
		}
	}
	return nil, nil, r.wrongKind(w, "a closure")
}

// privateArg returns the private name in v, an operand of w the compiler
// makes one (NewPrivateName), or wrongKind.
func (r *Realm) privateArg(v Value, w uint32) (*PrivateName, error) {
	if v.isPrivate() {
		return v.asPrivate(), nil
	}
	return nil, r.wrongKind(w, "a private name")
}

// compareJump is a compare-and-branch of x and y that run sends here, an
// operand not being a number: k is the op's offset from the first of its
// forms (Lt .. Ge, then the N forms) and pc is at its X word. It returns the
// next pc. The back edge of a rotated loop, taken or not (its exit), checks
// for an interrupt as run's does.
func (r *Realm) compareJump(w uint32, k bytecode.Op, x, y Value, pc int) (int, error) {
	res, err := r.compareSlow(bytecode.Lt+k&3, x, y)
	if err != nil {
		return pc, err
	}
	if off := int(int16(w >> 16)); off < 0 && r.interruptFlag.Load() < 0 {
		return pc, r.interruptError()
	} else if res == (k < 4) {
		return pc + off, nil // relative to the X word
	}
	return pc + 1, nil
}

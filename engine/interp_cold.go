package engine

import "github.com/MoeForge/moejs/bytecode"

// coldOp executes one op that the interpreter loop leaves to its default
// case, and returns the next pc. These are ops that are rare in loops (TDZ
// and import reads, wide environment slots, object-literal shapes beyond
// plain fields, spread calls, for-in setup) or that call a slow helper on
// every execution anyway (delete, in, instanceof), so the extra call costs
// little next to their work. Keeping them out of (*Realm).run keeps it
// below the compiler's big-function size (DESIGN §6). Ops it does not
// handle go on to classOp.
//
// Everything here may re-enter the interpreter: a result is written through
// st.stack after any call, never through a register slice taken before it.
func (r *Realm) coldOp(fd *FunctionData, base int, env *Env, w uint32, pc int) (int, error) {
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
	// --- closure environments and imports ---
	case bytecode.GetEnvChk:
		v := env.up(uint8(b)).slots[c]
		if v.IsHole() {
			return pc + 1, r.tdzError(meta, insns[pc])
		}
		regs[a] = v
		return pc + 1, nil
	case bytecode.GetEnvW:
		regs[a] = env.up(uint8(b)).slots[insns[pc]]
		return pc + 1, nil
	case bytecode.GetEnvChkW:
		v := env.up(uint8(b)).slots[insns[pc]]
		if v.IsHole() {
			return pc + 2, r.tdzError(meta, insns[pc+1])
		}
		regs[a] = v
		return pc + 2, nil
	case bytecode.SetEnvW:
		env.up(uint8(b)).slots[insns[pc]] = regs[a]
		return pc + 1, nil
	case bytecode.GetImport:
		ref := env.up(uint8(b)).slots[c]
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
		ref := env.up(uint8(b)).slots[insns[pc]]
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
		res, err := r.InstanceOf(regs[b], regs[c])
		if err == nil {
			st.stack[base+a] = Bool(res)
		}
		return pc, err
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
		}
		return pc, r.copyDataProps(o, regs[b], excluded)
	case bytecode.ArrayHole:
		arr, err := r.arrayArg(regs[a], w)
		if err != nil {
			return pc, err
		}
		arr.elements = r.growWithHoles(arr.elements, len(arr.elements)+1)
		arr.internal.(*ArrayData).length = uint32(len(arr.elements))
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
		rx, err := r.NewRegExp(meta.consts[bx].AsString(), meta.names[bx])
		if err == nil {
			st.stack[base+a] = ObjectValue(rx)
		}
		return pc, err
	case bytecode.GetTemplate:
		x := insns[pc]
		regs[a] = ObjectValue(r.templateObject(&code.Consts[uint16(x)], &r.ic[fd.icBase+x>>16]))
		return pc + 1, nil

	// --- calls ---
	case bytecode.CallSpread, bytecode.NewSpread:
		argv, err := r.spreadArgs(top, regs[a+2])
		if err != nil {
			return pc, err
		}
		regs = st.stack[base:top:top]
		var res Value
		st.sp = top + len(argv) // keep the callee's window above the argument list
		if bytecode.Op(w) == bytecode.CallSpread {
			res, err = r.callValue(regs[a], regs[a+1], argv)
		} else {
			res, err = r.constructValue(regs[a], argv)
		}
		st.sp = top
		if err == nil {
			st.stack[base+a] = res
		}
		return pc, err
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
		if fd, ok := o.internal.(*FunctionData); ok && fd.code != nil && !o.IsShared() {
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

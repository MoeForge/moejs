package engine

import "github.com/MoeForge/moejs/bytecode"

// Sloppy mode, script globals and with. The ops are off the interpreter's
// jump table (asyncOp's default case): only sloppy code, script top
// levels and with statements emit them, so strict code and modules pay
// for their dispatch only in the assignment of an undeclared name from a
// value that may run code (ResolveGlobal and SetGlobalRef).

// frameOpMarker is the error sloppyOp returns for the ops that need the run
// loop's own state (this, and the environment and callee MapArguments and
// CallEval read): the unwinding code recognizes it and applies the op
// (frameOp), so the loop's hot path has no case for them.
type frameOpMarker struct{}

func (frameOpMarker) Error() string { return "engine: frame op" }

var errFrameOp error = frameOpMarker{}

// sloppyOp executes one op of this file (from asyncOp's default case) of the
// frame whose registers start at base and returns the next pc.
func (r *Realm) sloppyOp(fd *FunctionData, base int, w uint32, pc int) (int, error) {
	st := &r.interp
	code := fd.code
	top := base + int(code.NumRegs)
	regs := st.stack[base:top:top]
	a, b := int(uint8(w>>8)), int(uint8(w>>16))
	keys := fd.meta.keys
	switch bytecode.Op(w) {
	case bytecode.SetGlobalSloppy:
		x := code.Code[pc]
		g := r.Global
		e := &r.ic[fd.icBase+x>>16]
		if g.shape == e.Shape && r.protoEpoch == e.Epoch {
			g.slots[e.Slot()] = regs[a]
			return pc + 1, nil
		}
		return pc + 1, r.setGlobalSloppy(keys[uint16(x)], regs[a], e)
	case bytecode.ResolveGlobal:
		x := code.Code[pc]
		g := r.Global
		e := &r.ic[fd.icBase+x>>16]
		if g.shape == e.Shape && r.protoEpoch == e.Epoch || r.accessorIC(e, g) != nil {
			regs[a] = True()
			return pc + 1, nil
		}
		has, err := r.hasGlobalBinding(keys[uint16(x)], e)
		if err != nil {
			return pc + 1, err
		}
		st.stack[base+a] = Bool(has)
		return pc + 1, nil
	case bytecode.SetGlobalRef:
		x := code.Code[pc]
		key := keys[uint16(x)]
		if !regs[b].IsTrue() {
			return pc + 1, r.ReferenceError("%s is not defined", key.GoString())
		}
		g := r.Global
		e := &r.ic[fd.icBase+x>>16]
		if g.shape == e.Shape && r.protoEpoch == e.Epoch {
			g.slots[e.Slot()] = regs[a]
			return pc + 1, nil
		}
		return pc + 1, r.setGlobalResolved(key, regs[a], e)
	case bytecode.InitGlobal:
		x := code.Code[pc]
		return pc + 1, r.initGlobalLexical(keys[uint16(x)], regs[a], x>>16 != 0)
	case bytecode.SetGlobalVar:
		key := keys[code.Code[pc]]
		if l, _ := r.globalLexical(key, nil); l != nil {
			return pc + 1, nil
		}
		var e ICEntry
		return pc + 1, r.setGlobalSloppy(key, regs[a], &e)
	case bytecode.DelGlobal:
		key := keys[code.Code[pc]]
		res := false
		if l, _ := r.globalLexical(key, nil); l == nil {
			var err error
			if res, err = r.deleteProperty(r.Global, key); err != nil {
				return pc + 1, err
			}
		}
		st.stack[base+a] = Bool(res)
		return pc + 1, nil
	case bytecode.SetPropSloppy:
		x := code.Code[pc]
		t, v := regs[a], regs[b]
		if !t.IsObject() {
			return pc + 1, r.setVSloppy(t, keys[uint16(x)], v)
		}
		o := t.AsObject()
		e := &r.ic[fd.icBase+x>>16]
		if o.shape == e.Shape && r.protoEpoch == e.Epoch {
			o.slots[e.Slot()] = v
			return pc + 1, nil
		}
		return pc + 1, r.setNamedSloppy(o, keys[uint16(x)], v, e)
	case bytecode.SetElemSloppy:
		return pc, r.setElemSloppy(regs[a], regs[b], regs[uint8(w>>24)])
	case bytecode.DelPropSloppy:
		res, err := r.deleteSloppy(regs[b], keys[code.Code[pc]].Value())
		if err == nil {
			st.stack[base+a] = Bool(res)
		}
		return pc + 1, err
	case bytecode.DelElemSloppy:
		res, err := r.deleteSloppy(regs[b], regs[uint8(w>>24)])
		if err == nil {
			st.stack[base+a] = Bool(res)
		}
		return pc, err
	case bytecode.SetSuperSloppy:
		return pc, r.setSuperSloppy(regs[a], regs[a+1], regs[b], regs[uint8(w>>24)])
	case bytecode.JmpWith:
		if !regs[a].IsObject() {
			// The %evalvars object of a function whose direct evals have
			// declared nothing yet.
			return pc + 1, nil
		}
		key := keys[code.Code[pc]]
		has, err := r.withHasBinding(regs[a].AsObject(), key)
		if err != nil || !has {
			return pc + 1, err
		}
		return pc + int(int16(w>>16)), nil
	case bytecode.WithGet: // the with object, which ToObject made
		x := code.Code[pc]
		o, err := r.objectArg(regs[b], w)
		if err != nil {
			return pc + 1, err
		}
		v, err := r.withGet(o, keys[uint16(x)], x>>16 != 0)
		if err == nil {
			st.stack[base+a] = v
		}
		return pc + 1, err
	case bytecode.WithSet:
		x := code.Code[pc]
		o, err := r.objectArg(regs[a], w)
		if err != nil {
			return pc + 1, err
		}
		return pc + 1, r.withSet(o, keys[uint16(x)], regs[b], x>>16 != 0)
	case bytecode.CoerceThis, bytecode.MapArguments:
		return pc, errFrameOp
	case bytecode.CallEval:
		return pc + 1, errFrameOp
	default:
		return pc, r.TypeError("internal: unknown opcode")
	}
}

// frameOp applies the CoerceThis, MapArguments or CallEval op w of the
// running frame of code whose registers are regs, environment env, this
// this and callee callee. It returns errFrameOp and the frame's this when
// the frame must rerun from the next instruction: CoerceThis changed this,
// or a direct eval grew the register stack or the frame records, which the
// loop's regs and frame pointer still point into (both only grow, so this
// happens a bounded number of times). Otherwise it returns the error of a
// direct eval. The frame is the innermost one, whose record holds its
// register base and the pc of w (the run loop passes no more, so that its
// register allocation does not change for a cold op).
func (r *Realm) frameOp(w uint32, code *bytecode.Function, regs []Value, env *Env, this Value, callee *Object) (Value, error) {
	switch bytecode.Op(w) {
	case bytecode.MapArguments:
		v := regs[uint8(w>>8)]
		if !v.IsObject() || v.AsObject().class != ClassArguments {
			return this, r.wrongKind(w, "an arguments object")
		}
		r.mapArguments(v.AsObject(), code, env, callee)
		return this, nil
	case bytecode.CallEval:
		st := &r.interp
		fr, nstack, nframes := st.frames[st.nframes-1], cap(st.stack), len(st.frames)
		err := r.callEval(code, int(fr.base), w, code.Code[fr.pc+1], env, this)
		if err == nil && (cap(st.stack) != nstack || len(st.frames) != nframes) {
			return this, errFrameOp
		}
		return this, err
	}
	// OrdinaryCallBindThis for a sloppy function: the global this value
	// for undefined and null, a wrapper object for another primitive.
	switch {
	case this.IsObject():
		return this, nil
	case this.IsNullish():
		return ObjectValue(r.Global), errFrameOp
	}
	o, _ := r.ToObject(this)
	return ObjectValue(o), errFrameOp
}

// --- the global declarative environment -------------------------------------------

// globalLex is the realm's global declarative environment record (ES2025
// 9.1.1.4.1 [[DeclarativeRecord]]): the let, const and class declarations of
// scripts. Every global name resolves here first, then on the global
// object. A realm whose scripts never declared one has none, so its global
// name resolution pays a nil check on the slow paths only.
type globalLex struct {
	index  map[PropertyKey]int32 // binding name -> slot
	slots  []Value               // Hole() while the binding is uninitialized
	consts []bool                // immutable (const) bindings, set by InitGlobal
}

// globalLexShape is the Shape field of an inline-cache entry that caches a
// global lexical binding: the slot in globalLex.slots, valid while the
// entry's Epoch is the realm's protoEpoch (declaring global lexical bindings
// bumps it, which also invalidates every global-object entry of their names).
// No object has this shape, so the fast paths never match such an entry.
var globalLexShape = &Shape{isDict: true, key: rootKey, shared: true}

// globalLexical returns the global lexical environment and the slot of its
// binding key, or nil when there is no such binding. A non-nil e caches the
// slot.
func (r *Realm) globalLexical(key PropertyKey, e *ICEntry) (*globalLex, int) {
	if r.lazy == nil || r.lazy.lex == nil {
		return nil, 0
	}
	return r.lazy.lex.lookup(r, key, e)
}

// lookup is globalLexical once the realm has global lexical bindings.
func (l *globalLex) lookup(r *Realm, key PropertyKey, e *ICEntry) (*globalLex, int) {
	if e != nil && e.Shape == globalLexShape && e.Epoch == r.protoEpoch {
		return l, int(e.Slot())
	}
	i, ok := l.index[key]
	if !ok {
		return nil, 0
	}
	if e != nil {
		*e = newICEntry(globalLexShape, r.protoEpoch, uint32(i), 0)
	}
	return l, int(i)
}

// get is GetBindingValue of slot i, named key.
func (l *globalLex) get(r *Realm, key PropertyKey, i int) (Value, error) {
	v := l.slots[i]
	if v.IsHole() {
		return Undefined(), r.ReferenceError("Cannot access '%s' before initialization", key.GoString())
	}
	return v, nil
}

// set is SetMutableBinding of slot i, named key: const bindings are strict
// bindings, so assigning one throws in sloppy code too.
func (l *globalLex) set(r *Realm, key PropertyKey, i int, v Value) error {
	switch {
	case l.slots[i].IsHole():
		return r.ReferenceError("Cannot access '%s' before initialization", key.GoString())
	case l.consts[i]:
		return r.TypeError("Assignment to constant variable.")
	}
	l.slots[i] = v
	return nil
}

// initGlobalLexical is InitializeBinding of the global lexical binding key
// that GlobalDeclarationInstantiation created.
func (r *Realm) initGlobalLexical(key PropertyKey, v Value, isConst bool) error {
	l, i := r.globalLexical(key, nil)
	if l == nil {
		return r.TypeError("internal: undeclared global lexical binding '%s'", key.GoString())
	}
	l.slots[i] = v
	l.consts[i] = isConst
	return nil
}

// setGlobalSloppy is PutValue of an identifier resolved against the global
// environment in sloppy code: an unresolvable name becomes a property of
// the global object, a rejected write and a missing setter are ignored.
func (r *Realm) setGlobalSloppy(key PropertyKey, v Value, e *ICEntry) error {
	g := r.Global
	if a := r.accessorIC(e, g); a != nil {
		if a.Set == nil {
			return nil
		}
		return r.callSetter(a, ObjectValue(g), key, v)
	}
	if l, i := r.globalLexical(key, e); l != nil {
		return l.set(r, key, i, v)
	}
	// ResolveBinding's HasProperty, and SetMutableBinding's for a resolved
	// name: observable only through a proxy on the global's chain.
	has, err := r.hasProperty(g, key)
	if err != nil {
		return err
	}
	if has {
		for p := g.proto; p != nil; p = p.proto {
			if p.class == ClassProxy {
				if _, err := r.hasProperty(g, key); err != nil {
					return err
				}
				break
			}
		}
	}
	return r.setNamedSloppy(g, key, v, e)
}

// hasGlobalBinding is HasBinding of the global environment (ES2025
// 9.1.1.4.1): a global lexical binding, then HasProperty of the global
// object. A non-nil e caches a lexical binding's slot.
func (r *Realm) hasGlobalBinding(key PropertyKey, e *ICEntry) (bool, error) {
	if l, _ := r.globalLexical(key, e); l != nil {
		return true, nil
	}
	return r.hasProperty(r.Global, key)
}

// setGlobalResolved is SetMutableBinding of the global environment for a
// strict reference that hasGlobalBinding resolved before the value was
// evaluated: the HasProperty of the object record's SetMutableBinding
// throws a ReferenceError when the binding is gone (setGlobalSlow makes
// ResolveBinding's too).
func (r *Realm) setGlobalResolved(key PropertyKey, v Value, e *ICEntry) error {
	g := r.Global
	if a := r.accessorIC(e, g); a != nil {
		return r.callSetter(a, ObjectValue(g), key, v)
	}
	if l, i := r.globalLexical(key, e); l != nil {
		return l.set(r, key, i, v)
	}
	if has, err := r.hasProperty(g, key); err != nil {
		return err
	} else if !has {
		return r.ReferenceError("%s is not defined", key.GoString())
	}
	return r.setNamedSlow(g, key, v, e)
}

// --- GlobalDeclarationInstantiation -------------------------------------------------

// globalDeclarationInstantiation implements GlobalDeclarationInstantiation
// (ES2025 16.1.7) with Annex B.3.2.2 for the script code: it checks every
// declaration against the global environment, then creates the lexical
// bindings (uninitialized), the function bindings (undefined until the
// script's prologue stores the closures) and the var bindings. Nothing is
// created when a check fails. For sloppy eval code whose variable
// environment is the global one (gn.Eval) it is the global part of
// EvalDeclarationInstantiation (ES2025 19.2.1.3) with Annex B.3.2.3: the
// same checks and bindings, configurable, and no lexical ones.
func (r *Realm) globalDeclarationInstantiation(code *bytecode.Function) error {
	if code.Extra == nil || code.Extra.Globals == nil {
		return nil
	}
	gn := code.Extra.Globals
	g := r.Global
	// Sloppy eval code creates its global var and function bindings
	// deletable (EvalDeclarationInstantiation).
	attrs := attrWritable | attrEnumerable
	if gn.Eval {
		attrs |= attrConfigurable
	}
	var lex *globalLex
	if r.lazy != nil {
		lex = r.lazy.lex
	}
	declaredLex := func(k PropertyKey) bool {
		if lex == nil {
			return false
		}
		_, ok := lex.index[k]
		return ok
	}
	lexKeys := make([]PropertyKey, len(gn.Lexical))
	for i, name := range gn.Lexical {
		k := r.KeyFromGoString(name)
		lexKeys[i] = k
		if declaredLex(k) {
			return r.SyntaxError("Identifier '%s' has already been declared", name)
		}
		// HasRestrictedGlobalProperty. ES2025 has no [[VarNames]]: a var
		// or function binding is restricted by its non-configurable
		// property, and a lexical declaration may shadow a configurable one.
		if d, ok := g.GetOwnProperty(k); ok && !d.Configurable() {
			return r.SyntaxError("Identifier '%s' has already been declared", name)
		}
	}
	funcKeys := make([]PropertyKey, len(gn.Function))
	for i, name := range gn.Function {
		k := r.KeyFromGoString(name)
		funcKeys[i] = k
		if declaredLex(k) {
			return r.SyntaxError("Identifier '%s' has already been declared", name)
		}
	}
	varKeys := make([]PropertyKey, len(gn.Var))
	for i, name := range gn.Var {
		k := r.KeyFromGoString(name)
		varKeys[i] = k
		if declaredLex(k) {
			return r.SyntaxError("Identifier '%s' has already been declared", name)
		}
	}
	// CanDeclareGlobalFunction
	for i, k := range funcKeys {
		d, ok := g.GetOwnProperty(k)
		switch {
		case !ok && g.IsExtensible(), ok && d.Configurable():
		case ok && !d.IsAccessorDescriptor() && d.Writable() && d.Enumerable():
		default:
			return r.TypeError("Cannot redefine global function '%s'", gn.Function[i])
		}
	}
	// CanDeclareGlobalVar
	for i, k := range varKeys {
		if !g.IsExtensible() && !g.HasOwnProperty(k) {
			return r.TypeError("Cannot define global variable '%s', global object is not extensible", gn.Var[i])
		}
	}
	// Annex B.3.2.2: a block function name no lexical binding shadows gets
	// a var binding when CanDeclareGlobalVar holds.
	for _, name := range gn.AnnexB {
		k := r.KeyFromGoString(name)
		if declaredLex(k) || g.HasOwnProperty(k) || !g.IsExtensible() {
			continue
		}
		if err := g.DefinePropertyOrThrow(r, k, DataDescriptor(Undefined(), attrs)); err != nil {
			return err
		}
	}
	if len(lexKeys) > 0 {
		if lex == nil {
			lex = &globalLex{index: make(map[PropertyKey]int32, len(lexKeys))}
			r.lazyState().lex = lex
		}
		for _, k := range lexKeys {
			lex.index[k] = int32(len(lex.slots))
			lex.slots = append(lex.slots, Hole())
			lex.consts = append(lex.consts, false)
		}
		// Global-object cache entries of the new names are stale now.
		r.protoEpoch += 2
	}
	// CreateGlobalFunctionBinding
	for _, k := range funcKeys {
		desc := DataDescriptor(Undefined(), attrs)
		if d, ok := g.GetOwnProperty(k); ok && !d.Configurable() {
			desc = PropertyDescriptor{}
			desc.SetValue(Undefined())
		}
		if err := g.DefinePropertyOrThrow(r, k, desc); err != nil {
			return err
		}
	}
	// CreateGlobalVarBinding
	for _, k := range varKeys {
		if g.HasOwnProperty(k) || !g.IsExtensible() {
			continue
		}
		if err := g.DefinePropertyOrThrow(r, k, DataDescriptor(Undefined(), attrs)); err != nil {
			return err
		}
	}
	return nil
}

// --- sloppy property writes and deletes ------------------------------------------

// setVSloppy is PutValue on a property reference with a primitive base in
// sloppy code: ToObject's TypeError for null and undefined, otherwise a
// rejected assignment is ignored.
func (r *Realm) setVSloppy(t Value, key PropertyKey, v Value) error {
	switch t.Type() {
	case TypeObject:
		_, err := t.AsObject().Set(r, key, v, t)
		return err
	case TypeUndefined, TypeNull:
		return r.TypeError("Cannot set property '%s' of %s", key.GoString(), t.String())
	}
	proto, err := r.protoForPrimitive(t)
	if err != nil {
		return err
	}
	_, err = proto.Set(r, key, v, t)
	return err
}

// setNamedSloppy is setNamedSlow ignoring a rejected assignment.
func (r *Realm) setNamedSloppy(o *Object, key PropertyKey, v Value, e *ICEntry) error {
	if a := r.accessorIC(e, o); a != nil {
		if a.Set == nil {
			return nil
		}
		return r.callSetter(a, ObjectValue(o), key, v)
	}
	if r.setNamedAdd(o, key, v, e) {
		return nil
	}
	ok, err := o.Set(r, key, v, ObjectValue(o))
	if err != nil || !ok {
		return err
	}
	r.fillSetIC(e, o, key)
	return nil
}

// setSuperSloppy is setSuper ignoring a rejected assignment.
func (r *Realm) setSuperSloppy(base, receiver, k, v Value) error {
	if !base.IsObject() {
		return r.TypeError("Cannot set properties of %s (setting '%s')", base.String(), k.String())
	}
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return err
	}
	_, err = base.AsObject().Set(r, key, v, receiver)
	return err
}

// setElemSloppy is setElemSlow ignoring a rejected assignment.
func (r *Realm) setElemSloppy(o, k, v Value) error {
	if o.IsNullish() {
		return r.TypeError("Cannot set properties of %s (setting '%s')", o.String(), k.String())
	}
	if o.IsObject() && k.IsNumber() && o.AsObject().class == ClassTypedArray {
		return r.typedArraySetNumber(o.AsObject(), k.AsNumber(), v)
	}
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return err
	}
	return r.setVSloppy(o, key, v)
}

// deleteSloppy implements the sloppy `delete` operator for base and key
// values: false instead of a TypeError when the property is not
// configurable.
func (r *Realm) deleteSloppy(base, k Value) (bool, error) {
	if base.IsNullish() {
		return false, r.TypeError("Cannot convert undefined or null to object")
	}
	key, err := r.ToPropertyKey(k)
	if err != nil {
		return false, err
	}
	o, err := r.ToObject(base)
	if err != nil {
		return false, err
	}
	return r.deleteProperty(o, key)
}

// --- with --------------------------------------------------------------------------

var unscopablesKey = SymbolKey(SymUnscopables)

// withHasBinding is HasBinding of the object environment record of a with
// statement over o (ES2025 9.1.1.2.1).
func (r *Realm) withHasBinding(o *Object, key PropertyKey) (bool, error) {
	if has, err := r.hasProperty(o, key); err != nil || !has {
		return false, err
	}
	u, err := o.Get(r, unscopablesKey, ObjectValue(o))
	if err != nil || !u.IsObject() {
		return err == nil, err
	}
	blocked, err := u.AsObject().Get(r, key, u)
	if err != nil {
		return false, err
	}
	return !ToBoolean(blocked), nil
}

// withGet is GetBindingValue of the object environment record over o.
func (r *Realm) withGet(o *Object, key PropertyKey, strict bool) (Value, error) {
	has, err := r.hasProperty(o, key)
	if err != nil {
		return Undefined(), err
	}
	if !has {
		if strict {
			return Undefined(), r.ReferenceError("%s is not defined", key.GoString())
		}
		return Undefined(), nil
	}
	return o.Get(r, key, ObjectValue(o))
}

// withSet is SetMutableBinding of the object environment record over o.
func (r *Realm) withSet(o *Object, key PropertyKey, v Value, strict bool) error {
	has, err := r.hasProperty(o, key)
	if err != nil {
		return err
	}
	if !has && strict {
		return r.ReferenceError("%s is not defined", key.GoString())
	}
	if strict {
		return o.SetProp(r, key, v)
	}
	_, err = o.Set(r, key, v, ObjectValue(o))
	return err
}

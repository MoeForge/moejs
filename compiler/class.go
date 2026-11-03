package compiler

import (
	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Classes (ECMA-262 §15.7.14 ClassDefinitionEvaluation) compile to code in
// the function that evaluates the class, inside the class scope:
//
//  1. a new private name for every private identifier of the class, and
//     the brand of its instance private methods;
//  2. the heritage, then the constructor closure and CreateClass, which
//     links the heritage and creates the prototype;
//  3. the elements in source order: methods and accessors on the prototype
//     or the constructor, private methods under their names, computed field
//     keys into hidden bindings;
//  4. the instance member initializer (Class.Fields), a synthetic method
//     the constructor calls on each new object: it adds the brand, then
//     defines the instance fields in order;
//  5. the inner name binding, then the static initializer (Class.Static), a
//     synthetic method called once with the constructor as `this`: static
//     fields and static blocks in order.
//
// A base constructor calls the member initializer in its prologue, before
// its parameters are bound; a derived one calls it when super() returns.
// The hidden bindings the resolver declares (syntax/scope_class.go) carry
// new.target, the home object, the active function and a derived
// constructor's `this` to the code that reads them.

// classExpr evaluates class c into dst. name is its name (the binding
// identifier, or the NamedEvaluation name of an anonymous class); key >= 0
// is a register holding the property key an anonymous class is named after
// at run time (a field with a computed key).
func (f *funcState) classExpr(c *syntax.Class, dst int, name string, key int) {
	mark := f.nregs
	defer f.free(mark)
	if c.Name != nil {
		name = c.Name.Name
	}
	f.setPos(c.Pos)
	es, smark := f.enterScope(c.Scope)
	for _, b := range c.Scope.Bindings {
		desc := b.Name
		if b == c.BrandBinding {
			desc = name
		} else if b.Kind != syntax.BindPrivate {
			continue
		}
		loc := f.c.locs[b]
		m := f.nregs
		t := f.locTarget(loc)
		f.emitABx(bytecode.NewPrivateName, t, f.stringConst(desc))
		f.storeFromTarget(loc, t)
		f.free(m)
	}
	heritage, flags := 0, 0
	if c.Super != nil {
		heritage, flags = f.alloc(), 1
		f.expr(c.Super, heritage)
	}

	// The constructor and the prototype, in R[a] and R[a+1].
	a := f.allocN(2)
	if f.c.classOf == nil {
		f.c.classOf = make(map[*syntax.Function]*syntax.Class)
	}
	f.c.classOf[c.Ctor] = c
	idx := f.compileFunction(c.Ctor, name)
	ctor := f.children[idx]
	ctor.Name = name // an explicit constructor's own Name is "constructor"
	ctor.Source.Start, ctor.Source.End = c.Pos, c.End
	f.setPos(c.Pos)
	f.emitClosure(a, idx)
	if key >= 0 {
		f.emitMove(a+1, key)
		flags |= 2
	}
	f.emitABC(bytecode.CreateClass, a, heritage, flags)

	for _, m := range c.Members {
		if m.Value != c.Ctor {
			f.classElement(c, m, a)
		}
	}
	if c.Fields != nil {
		m := f.nregs
		v := f.alloc()
		f.synthetic(c.Fields, v, "<instance_members_initializer>", a+1, func(g *funcState) { g.instanceMembers(c) })
		f.storeLoc(f.c.locs[c.FieldsBinding], v)
		f.free(m)
	}
	if c.NameBinding != nil {
		f.storeLoc(f.c.locs[c.NameBinding], a)
	}
	if c.Static != nil {
		m := f.nregs
		b := f.allocN(2)
		f.synthetic(c.Static, b, "<static_initializer>", a, func(g *funcState) { g.staticMembers(c) })
		f.emitMove(b+1, a)
		f.emitAB(bytecode.Call, b, 0)
		f.free(m)
	}
	f.emitMove(dst, a)
	f.leaveScope(es, smark)
}

// classDecl evaluates a class declaration into its outer binding b.
func (f *funcState) classDecl(c *syntax.Class, b *syntax.Binding, name string) {
	if b == nil {
		f.c.fail(c.Pos, "internal: class declaration without binding")
	}
	mark := f.nregs
	loc := f.c.locs[b]
	t := f.locTarget(loc)
	f.classExpr(c, t, name, -1)
	f.storeFromTarget(loc, t)
	f.free(mark)
}

// classElement defines one method or accessor of class c (constructor in
// R[a], prototype in R[a+1]), or evaluates a computed field key.
func (f *funcState) classElement(c *syntax.Class, m *syntax.ClassMember, a int) {
	mark := f.nregs
	defer f.free(mark)
	f.setPos(m.Pos)
	home := a + 1
	if m.Static {
		home = a
	}
	switch m.Kind {
	case syntax.ClassField:
		if m.Computed {
			k := f.alloc()
			f.expr(m.Key, k)
			f.emitAB(bytecode.ToPropertyKey, k, k)
			f.storeLoc(f.c.locs[m.KeyBinding], k)
		}
		return
	case syntax.ClassStaticBlock:
		return
	}
	fn := m.Value.(*syntax.Function)
	if pn, ok := m.Key.(*syntax.PrivateName); ok {
		f.privateMethod(c, m, pn, fn, home)
		return
	}
	if m.Kind != syntax.ClassMethod {
		f.accessor(home, m.Key, m.Computed, m.Kind == syntax.ClassSetter, fn, 0)
		return
	}
	k := f.alloc()
	name := ""
	if m.Computed {
		f.expr(m.Key, k)
	} else {
		name = f.literalKeyName(m.Key)
		f.emitABx(bytecode.LoadConst, k, f.stringConst(name))
	}
	v := f.alloc()
	f.emitClosure(v, f.compileFunction(fn, name))
	f.emitABC(bytecode.DefineClassMethod, home, k, v)
}

// privateMethod records a private method or accessor under its private
// name, checked against the class brand (or, when static, the class).
func (f *funcState) privateMethod(c *syntax.Class, m *syntax.ClassMember, pn *syntax.PrivateName, fn *syntax.Function, home int) {
	prefix, flags := "", uint32(0)
	switch m.Kind {
	case syntax.ClassGetter:
		prefix, flags = "get ", bytecode.PrivateGetter
	case syntax.ClassSetter:
		prefix, flags = "set ", bytecode.PrivateSetter
	}
	owner := home
	if m.Static {
		flags |= bytecode.PrivateStatic
	} else {
		owner = f.bindingReg(c.BrandBinding)
	}
	p := f.bindingReg(pn.Binding)
	v := f.alloc()
	f.emitClosure(v, f.compileFunction(fn, prefix+"#"+pn.Name))
	f.setHome(fn, v, home)
	f.emitABC(bytecode.SetPrivateMethod, p, v, owner)
	f.emitExtra(flags)
}

// synthetic creates the closure of a member initializer (Class.Fields or
// Class.Static) whose body emits, with home object R[home].
func (f *funcState) synthetic(fn *syntax.Function, dst int, name string, home int, body func(*funcState)) {
	if f.c.bodies == nil {
		f.c.bodies = make(map[*syntax.Function]func(*funcState))
	}
	f.c.bodies[fn] = body
	f.setPos(fn.Pos)
	f.emitClosure(dst, f.compileFunction(fn, name))
	f.setHome(fn, dst, home)
}

// instanceMembers is the body of Class.Fields: InitializeInstanceElements
// on `this`.
func (f *funcState) instanceMembers(c *syntax.Class) {
	this := f.loadThisObject()
	if c.BrandBinding != nil {
		mark := f.nregs
		f.emitAB(bytecode.AddBrand, this, f.bindingReg(c.BrandBinding))
		f.free(mark)
	}
	for _, m := range c.Members {
		if m.Kind == syntax.ClassField && !m.Static {
			f.field(m, this)
		}
	}
}

// staticMembers is the body of Class.Static: the static fields and blocks
// on `this` (the class) in order.
func (f *funcState) staticMembers(c *syntax.Class) {
	this := f.loadThisObject()
	for _, m := range c.Members {
		switch {
		case m.Kind == syntax.ClassField && m.Static:
			f.field(m, this)
		case m.Kind == syntax.ClassStaticBlock:
			mark := f.nregs
			f.setPos(m.Pos)
			b := f.allocN(2)
			f.emitClosure(b, f.compileFunction(m.Block, ""))
			f.emitABC(bytecode.Call, b, 0, bytecode.CallUndefThis)
			f.free(mark)
		}
	}
}

// loadThisObject loads the `this` of a member initializer into a new
// register, as an object: the engine calls an initializer only on the
// object being initialized (or the class), but DefineField reads its
// instance without looking, and the bytecode verifier proves its kind
// only from an op that makes an object (bytecode.RegisterWrites). On an
// object ToObject is the identity.
func (f *funcState) loadThisObject() int {
	this := f.alloc()
	f.emitA(bytecode.LoadThis, this)
	f.emitAB(bytecode.ToObject, this, this)
	return this
}

// field defines field m on R[obj] (DefineField): its initializer is
// evaluated with NamedEvaluation for anonymous functions and classes.
func (f *funcState) field(m *syntax.ClassMember, obj int) {
	mark := f.nregs
	defer f.free(mark)
	f.setPos(m.Pos)
	v := f.alloc()
	switch k := m.Key.(type) {
	case *syntax.PrivateName:
		f.fieldValue(m.Value, v, "#"+k.Name, -1)
		f.emitABC(bytecode.DefPrivate, obj, f.bindingReg(k.Binding), v)
		return
	}
	if !m.Computed {
		name := f.literalKeyName(m.Key)
		f.fieldValue(m.Value, v, name, -1)
		f.defineNamed(obj, name, v)
		return
	}
	key := f.bindingReg(m.KeyBinding)
	if fn, ok := m.Value.(*syntax.Function); ok && fn.Name == nil {
		// Named after the key at run time as it is defined.
		f.emitClosure(v, f.compileFunction(fn, ""))
		f.emitABC(bytecode.DefineMethod, obj, key, v)
		return
	}
	f.fieldValue(m.Value, v, "", key)
	f.emitABC(bytecode.DefineElem, obj, key, v)
}

// fieldValue evaluates a field initializer (undefined when absent).
func (f *funcState) fieldValue(e syntax.Expr, dst int, name string, key int) {
	switch x := e.(type) {
	case nil:
		f.emitA(bytecode.LoadUndef, dst)
	case *syntax.Class:
		if x.Name == nil {
			f.classExpr(x, dst, name, key)
			return
		}
		f.expr(e, dst)
	default:
		f.exprNamed(e, dst, name)
	}
}

// setHome sets the [[HomeObject]] of closure R[fn] to R[home] when its
// code reads it (super property access).
func (f *funcState) setHome(fn *syntax.Function, reg, home int) {
	if fn.HomeBinding != nil {
		f.emitAB(bytecode.SetHome, reg, home)
	}
}

// bindingReg returns a register holding binding b: its own register, or a
// temporary loaded from its environment.
func (f *funcState) bindingReg(b *syntax.Binding) int {
	if r := f.regOf(b); r >= 0 {
		return r
	}
	t := f.alloc()
	f.loadLoc(b, t)
	return t
}

// loadRaw loads binding b without a TDZ check (a hole reads as is) and
// returns its register; a nil binding reads as the hole.
func (f *funcState) loadRaw(b *syntax.Binding) int {
	if b == nil {
		t := f.alloc()
		f.emitA(bytecode.LoadHole, t)
		return t
	}
	loc := f.c.locs[b]
	if loc.env == nil {
		return loc.reg
	}
	t := f.alloc()
	depth := f.envDepthOf(loc.env)
	f.checkDepth(depth)
	if loc.slot <= bytecode.MaxRegister {
		f.emitABC(bytecode.GetEnv, t, depth, loc.slot)
	} else {
		f.emitAB(bytecode.GetEnvW, t, depth)
		f.emitExtra(uint32(loc.slot))
	}
	return t
}

// --- function entry and exit --------------------------------------------------------

// prologue initializes the hidden bindings of a function (new.target, the
// home object, the active function) and, in a base class constructor, runs
// the instance member initializer. It precedes parameter binding, and
// CtorEntry or LoadNewTarget is the first op that can run JavaScript.
func (f *funcState) prologue() {
	fn := f.fn
	switch {
	case fn.Kind == syntax.FuncClassConstructor:
		f.out.NewTarget = true
		f.initHidden(fn.NewTargetBinding, bytecode.CtorEntry)
		if fn.Derived {
			f.retReg, f.retLbl = f.alloc(), f.newLabel()
		}
	case fn.NewTargetBinding == nil:
	case fn.Kind == syntax.FuncNormal:
		f.out.NewTarget = true
		f.initHidden(fn.NewTargetBinding, bytecode.LoadNewTarget)
	default:
		f.initHidden(fn.NewTargetBinding, bytecode.LoadUndef)
	}
	if fn.CalleeBinding != nil {
		f.initHidden(fn.CalleeBinding, bytecode.LoadCallee)
	}
	if fn.HomeBinding != nil {
		f.initHidden(fn.HomeBinding, bytecode.LoadHome)
	}
	if c := f.c.classOf[fn]; fn.Kind == syntax.FuncClassConstructor && !fn.Derived && c != nil && c.FieldsBinding != nil {
		mark := f.nregs
		b := f.alloc()
		f.emitA(bytecode.LoadThis, b)
		f.initFields(fn, b)
		f.free(mark)
	}
}

// initHidden stores the value op loads into hidden binding b (a scratch
// register when b is nil: CtorEntry still checks the call).
func (f *funcState) initHidden(b *syntax.Binding, op bytecode.Op) {
	mark := f.nregs
	defer f.free(mark)
	if b == nil {
		f.emitA(op, f.alloc())
		return
	}
	loc := f.c.locs[b]
	t := f.locTarget(loc)
	f.emitA(op, t)
	f.storeFromTarget(loc, t)
}

// initFields calls the instance member initializer of the class of
// constructor ctor, if any, on R[obj].
func (f *funcState) initFields(ctor *syntax.Function, obj int) {
	c := f.c.classOf[ctor]
	if c == nil || c.FieldsBinding == nil {
		return
	}
	mark := f.nregs
	b := f.allocN(2)
	f.loadLoc(c.FieldsBinding, b)
	f.emitMove(b+1, obj)
	f.emitAB(bytecode.Call, b, 0)
	f.free(mark)
}

// A derived constructor's returns (retLbl set) all jump to one epilogue
// after the body, outside every try block: the checks of DerivedResult
// belong to [[Construct]] and must not be caught by the constructor.

// derivedReturn returns R[reg] from a derived constructor.
func (f *funcState) derivedReturn(reg int) {
	f.emitMove(f.retReg, reg)
	f.emitPopEnvs(0)
	f.emitJump(bytecode.Jmp, 0, f.retLbl)
}

// endBody ends a function body that completes normally; in a derived
// constructor it is followed by the epilogue returning the value in
// retReg: an object as is, undefined as the initialized `this`.
func (f *funcState) endBody() {
	if f.retLbl == nil {
		f.emitNone(bytecode.RetUndef)
		return
	}
	f.emitA(bytecode.LoadUndef, f.retReg)
	f.bind(f.retLbl)
	if f.out.Async {
		f.asyncEnd()
		return
	}
	mark := f.nregs
	f.emitAB(bytecode.DerivedResult, f.retReg, f.loadRaw(f.fn.ThisBinding))
	f.emitA(bytecode.Ret, f.retReg)
	f.free(mark)
}

// emitRetUndef compiles a bare return: it closes the open for-of iterators
// and returns undefined (the initialized `this` of a derived constructor).
func (f *funcState) emitRetUndef() {
	if f.retLbl == nil {
		f.emitIterCloses(0)
		f.emitNone(bytecode.RetUndef)
		return
	}
	f.emitA(bytecode.LoadUndef, f.retReg)
	f.emitReturn(f.retReg)
}

// --- super, this and private names ----------------------------------------------------

// thisFunc returns the state of the innermost non-arrow function, which
// supplies this, new.target and super.
func (f *funcState) thisFunc() *funcState {
	g := f
	for g.kind == bytecode.KindArrow {
		g = g.parent
	}
	return g
}

// isSuper reports whether m is a super property reference.
func isSuper(m *syntax.MemberExpr) bool {
	_, ok := m.Object.(*syntax.SuperExpr)
	return ok
}

// superCall compiles super(args) in a derived constructor or an arrow
// nested in one: construct the parent with the constructor's new.target,
// bind `this`, run the member initializer.
func (f *funcState) superCall(e *syntax.CallExpr, dst int) {
	mark := f.nregs
	defer f.free(mark)
	ctor := f.thisFunc().fn
	base := f.callBase(dst)
	f.allocN(2)
	f.loadLoc(ctor.CalleeBinding, base)
	f.emitAB(bytecode.GetProtoOf, base, base)
	f.loadLoc(ctor.NewTargetBinding, base+1)
	spread := f.args(e.Args, base)
	f.setPos(e.Pos)
	if spread {
		f.emitA(bytecode.SuperCallSpread, base)
	} else {
		f.emitAB(bytecode.SuperCall, base, len(e.Args))
	}
	f.bindThis(ctor, base)
	f.emitMove(dst, base)
}

// defaultSuperCall is the body of a default derived constructor:
// super(...args) forwarding the rest array without iterating it.
func (f *funcState) defaultSuperCall() {
	fn := f.fn
	mark := f.nregs
	base := f.allocN(3)
	f.loadLoc(fn.CalleeBinding, base)
	f.emitAB(bytecode.GetProtoOf, base, base)
	f.loadLoc(fn.NewTargetBinding, base+1)
	f.loadLoc(fn.Rest.(*syntax.Ident).Binding, base+2) // the synthesized ...args
	f.emitA(bytecode.SuperCallSpread, base)
	f.bindThis(fn, base)
	f.free(mark)
}

// bindThis initializes the `this` binding of derived constructor ctor to
// R[obj] (ReferenceError when super() already ran) and runs the member
// initializer.
func (f *funcState) bindThis(ctor *syntax.Function, obj int) {
	mark := f.nregs
	f.emitA(bytecode.CheckSuper, f.loadRaw(ctor.ThisBinding))
	f.free(mark)
	f.storeLoc(f.c.locs[ctor.ThisBinding], obj)
	f.initFields(ctor, obj)
}

// loadThis loads `this` for a super property reference: the `this`
// binding of a derived constructor (TDZ-checked), else the frame's this.
func (f *funcState) loadThis(g *funcState, dst int) {
	if fn := g.fn; fn != nil && fn.Kind == syntax.FuncClassConstructor && fn.Derived {
		f.loadLoc(fn.ThisBinding, dst)
		return
	}
	f.emitA(bytecode.LoadThis, dst)
}

// superRef evaluates super property reference m into R[b] (the home
// object's prototype) and R[b+1] (this), which the caller allocated, and
// returns the register holding the key. The order is the spec's: this,
// the key, then the base.
func (f *funcState) superRef(m *syntax.MemberExpr, b int) int {
	g := f.thisFunc()
	f.loadThis(g, b+1)
	k := f.alloc()
	if m.Computed {
		f.expr(m.Prop, k)
	} else {
		f.emitABx(bytecode.LoadConst, k, f.stringConst(m.Prop.(*syntax.Ident).Name))
	}
	f.loadLoc(g.fn.HomeBinding, b)
	f.emitAB(bytecode.GetProtoOf, b, b)
	return k
}

// superGet compiles a super property read into dst.
func (f *funcState) superGet(m *syntax.MemberExpr, dst int) {
	mark := f.nregs
	defer f.free(mark)
	b := f.allocN(2)
	k := f.superRef(m, b)
	f.setPos(m.Pos)
	f.emitABC(bytecode.GetSuper, dst, b, k)
}

// superCallee loads the super method of call super.m(...) into R[base]
// and this into R[base+1].
func (f *funcState) superCallee(m *syntax.MemberExpr, base int) {
	k := f.superRef(m, base)
	f.setPos(m.Pos)
	f.emitABC(bytecode.GetSuper, base, base, k)
	f.free(base + 2)
}

// superAssign compiles an assignment to a super property.
func (f *funcState) superAssign(e *syntax.AssignExpr, m *syntax.MemberExpr, dst int, want bool) {
	b := f.allocN(2)
	k := f.superRef(m, b)
	cur := dst
	if !want {
		cur = f.alloc()
	}
	if e.Op == syntax.Assign {
		f.expr(e.Value, cur)
		f.emitABC(f.setSuperOp(), b, k, cur)
		return
	}
	f.emitAB(bytecode.ToPropertyKey, k, k)
	f.emitABC(bytecode.GetSuper, cur, b, k)
	if lbl := f.logicalSkip(e.Op, cur); lbl != nil {
		f.expr(e.Value, cur)
		f.emitABC(f.setSuperOp(), b, k, cur)
		f.bind(lbl)
		return
	}
	v := f.operand(e.Value)
	f.emitABC(binaryOps[compoundOps[e.Op]], cur, cur, v)
	f.emitABC(f.setSuperOp(), b, k, cur)
}

// superUpdate compiles ++/-- on a super property.
func (f *funcState) superUpdate(e *syntax.UpdateExpr, m *syntax.MemberExpr, op bytecode.Op, dst int, want bool) {
	b := f.allocN(2)
	k := f.superRef(m, b)
	f.emitAB(bytecode.ToPropertyKey, k, k)
	cur := f.alloc()
	f.emitABC(bytecode.GetSuper, cur, b, k)
	if e.Prefix {
		f.emitAB(op, cur, cur)
		f.emitABC(f.setSuperOp(), b, k, cur)
		if want {
			f.emitMove(dst, cur)
		}
		return
	}
	old := dst
	if !want {
		old = f.alloc()
	}
	f.emitAB(bytecode.ToNumeric, old, cur)
	f.emitAB(op, cur, old)
	f.emitABC(f.setSuperOp(), b, k, cur)
}

// superDelete compiles `delete super.x`: the reference is evaluated, then
// a ReferenceError is thrown.
func (f *funcState) superDelete(m *syntax.MemberExpr) {
	b := f.allocN(2)
	f.superRef(m, b)
	f.emitABx(bytecode.ThrowError, int(bytecode.ThrowReferenceError), f.stringConst("Unsupported reference to 'super'"))
}

// emitGetKey emits R[dst] = R[obj][R[key]] for member m, a private
// element read when m names one.
func (f *funcState) emitGetKey(dst, obj, key int, m *syntax.MemberExpr) {
	if _, ok := m.Prop.(*syntax.PrivateName); ok {
		f.emitABC(bytecode.GetPrivate, dst, obj, key)
		return
	}
	f.emitABC(bytecode.GetElem, dst, obj, key)
}

// emitGetRef emits the read R[dst] = R[obj][R[key]] of member m that a
// compound assignment or update then writes. GetValue converts the key
// once, after ToObject of the base, and PutValue reuses it (ES2025
// 6.2.5.5, 6.2.5.6), so an object key's toString runs once and not for a
// nullish base: GetElemRef leaves the converted key in a register. It
// returns the register of the key to write: key itself when it is a
// temporary (at or above mark), a new one when it is a variable's, which
// must keep its value. A private name, or a key primitive by its syntax,
// needs no conversion.
func (f *funcState) emitGetRef(dst, obj, key, mark int, m *syntax.MemberExpr) int {
	if _, ok := m.Prop.(*syntax.PrivateName); isPrimitiveExpr(m.Prop) || ok {
		f.emitGetKey(dst, obj, key, m)
		return key
	}
	k := key
	if key < mark {
		k = f.alloc()
	}
	f.emitABC(bytecode.GetElemRef, dst, obj, k)
	f.emitExtra(uint32(key))
	return k
}

// isPrimitiveExpr reports whether the value of e is primitive by its
// syntax: a literal other than a regular expression, a template, or an
// operator that yields a primitive.
func isPrimitiveExpr(e syntax.Expr) bool {
	switch e.(type) {
	case *syntax.NumberLit, *syntax.StringLit, *syntax.BoolLit, *syntax.NullLit, *syntax.BigIntLit,
		*syntax.TemplateLit, *syntax.UnaryExpr, *syntax.BinaryExpr, *syntax.UpdateExpr:
		return true
	}
	return false
}

// privateIn compiles `#x in o`.
func (f *funcState) privateIn(pn *syntax.PrivateName, o syntax.Expr, dst int) {
	p := f.bindingReg(pn.Binding)
	f.emitABC(bytecode.InPrivate, dst, p, f.operand(o))
}

package compiler

import (
	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// The with statement. Its object lives in the with scope's one hidden
// binding; a name referenced inside it resolves through the objects of the
// enclosing with statements, innermost first (JmpWith), before its own
// binding. Only code inside a with statement, which sloppy scripts alone can
// contain, emits these sequences.
//
// The variable environment of a sloppy function containing a direct eval
// resolves names the same way: the var and function declarations of its
// eval code land in the object of its %evalvars binding (created by the
// first eval code that declares one), so a name declared outside that
// environment is looked up in the object first. Code without a direct eval
// or a with statement emits none of this.

// evalVarsName is the hidden binding of a sloppy function's variable
// environment that holds the object receiving the declarations of its eval
// code (syntax's hiddenEvalVars).
const evalVarsName = "%evalvars"

// withObject returns the binding holding the object through which names
// resolve in scope s of scriptState.withs: a with statement's object or a
// variable environment's %evalvars object.
func withObject(s *syntax.Scope) *syntax.Binding {
	if s.Kind == syntax.ScopeWith {
		return s.Bindings[0]
	}
	return s.Lookup(evalVarsName)
}

// evalVars adds the variable environment s of a sloppy function containing
// a direct eval to the scopes names resolve through, when it holds a
// %evalvars binding. compileBody restores the list.
func (f *funcState) evalVars(s *syntax.Scope) {
	if s.Lookup(evalVarsName) != nil {
		f.c.script.withs = append(f.c.script.withs, s)
	}
}

// withStmt compiles `with (Object) Body`.
func (f *funcState) withStmt(s *syntax.WithStmt) {
	mark := f.nregs
	obj := f.alloc()
	f.expr(s.Object, obj)
	f.setPos(s.Pos)
	f.emitAB(bytecode.ToObject, obj, obj)
	es, smark := f.enterScope(s.Scope)
	f.storeLoc(f.c.locs[s.Scope.Bindings[0]], obj)
	f.clearCompletion()
	sc := f.c.script
	sc.withs = append(sc.withs, s.Scope)
	f.stmt(s.Body)
	sc.withs = sc.withs[:len(sc.withs)-1]
	f.leaveScope(es, smark)
	f.free(mark)
}

// withsFor returns the with statements (and %evalvars scopes) a reference
// to b (nil: an unresolved name) resolves through: those between the
// reference and b's declaring scope, outermost first. A function's own name
// is declared outside its variable environment, which eval code may shadow
// it in.
func (f *funcState) withsFor(b *syntax.Binding) []*syntax.Scope {
	sc := f.c.script
	if sc == nil || len(sc.withs) == 0 {
		return nil
	}
	i := len(sc.withs)
	for i > 0 {
		w := sc.withs[i-1]
		if b != nil && !encloses(b.Scope, w) && (w != b.Scope || b.Kind != syntax.BindFuncName) {
			break
		}
		i--
	}
	if i == len(sc.withs) {
		return nil
	}
	return sc.withs[i:]
}

// encloses reports whether scope a is a proper ancestor of scope s.
func encloses(a, s *syntax.Scope) bool {
	for p := s.Parent; p != nil; p = p.Parent {
		if p == a {
			return true
		}
	}
	return false
}

// identRef is a resolved identifier reference: ref is the register holding
// the with object that has the binding, or undefined when the name resolves
// past the with statements; -1 when no with statement is in the way.
type identRef struct {
	id  *syntax.Ident
	ref int
}

// resolveIdent emits ResolveBinding of id through the enclosing with
// statements into a fresh register.
func (f *funcState) resolveIdent(id *syntax.Ident) identRef {
	ws := f.withsFor(id.Binding)
	if ws == nil {
		return identRef{id: id, ref: -1}
	}
	ref := f.alloc()
	f.withRef(ws, id.Name, ref)
	return identRef{id: id, ref: ref}
}

// withRef emits the lookup of name in the objects of the with statements
// ws (innermost first) into register ref: the first whose object
// environment has the binding, else undefined.
func (f *funcState) withRef(ws []*syntax.Scope, name string, ref int) {
	found := f.newLabel()
	k := uint32(f.nameConst(name))
	for i := len(ws) - 1; i >= 0; i-- {
		f.loadLoc(withObject(ws[i]), ref)
		f.emitJump(bytecode.JmpWith, ref, found)
		f.emitExtra(k)
	}
	f.emitA(bytecode.LoadUndef, ref)
	f.bind(found)
}

// withOp emits the WithGet or WithSet op a with operand reg names.
func (f *funcState) withOp(op bytecode.Op, a, b int, name string) {
	strict := uint16(0)
	if f.out.Strict {
		strict = 1
	}
	f.emitAB(op, a, b)
	f.emitExtra(bytecode.ExtraArg(f.nameConst(name), strict))
}

// get emits GetValue of the reference into dst.
func (f *funcState) get(r identRef, dst int) {
	if r.ref < 0 {
		f.loadName(r.id, dst)
		return
	}
	wl, end := f.newLabel(), f.newLabel()
	f.emitJump(bytecode.JmpNotUndef, r.ref, wl)
	f.loadName(r.id, dst)
	f.emitJump(bytecode.Jmp, 0, end)
	f.bind(wl)
	f.withOp(bytecode.WithGet, dst, r.ref, r.id.Name)
	f.bind(end)
}

// put emits PutValue of register src to the reference.
func (f *funcState) put(r identRef, src int, mode bindMode) {
	if r.ref < 0 {
		f.storeName(r.id, src, mode)
		return
	}
	wl, end := f.newLabel(), f.newLabel()
	f.emitJump(bytecode.JmpNotUndef, r.ref, wl)
	f.storeName(r.id, src, mode)
	f.emitJump(bytecode.Jmp, 0, end)
	f.bind(wl)
	f.withOp(bytecode.WithSet, r.ref, src, r.id.Name)
	f.bind(end)
}

// withCallee loads the function a call of identifier id inside a with
// statement calls into base and its this value into base+1: the with object
// that has the binding, or undefined (also for a binding of a %evalvars
// object, which is a declarative environment's).
func (f *funcState) withCallee(id *syntax.Ident, base int) {
	ws := f.withsFor(id.Binding)
	vars := false
	for _, w := range ws {
		vars = vars || w.Kind != syntax.ScopeWith
	}
	if !vars {
		f.withRef(ws, id.Name, base+1)
		f.get(identRef{id: id, ref: base + 1}, base)
		return
	}
	foundWith, foundVars, end := f.newLabel(), f.newLabel(), f.newLabel()
	k := uint32(f.nameConst(id.Name))
	for i := len(ws) - 1; i >= 0; i-- {
		found := foundWith
		if ws[i].Kind != syntax.ScopeWith {
			found = foundVars
		}
		f.loadLoc(withObject(ws[i]), base+1)
		f.emitJump(bytecode.JmpWith, base+1, found)
		f.emitExtra(k)
	}
	f.emitA(bytecode.LoadUndef, base+1)
	f.loadName(id, base)
	f.emitJump(bytecode.Jmp, 0, end)
	f.bind(foundWith)
	f.withOp(bytecode.WithGet, base, base+1, id.Name)
	f.emitJump(bytecode.Jmp, 0, end)
	f.bind(foundVars)
	f.withOp(bytecode.WithGet, base, base+1, id.Name)
	f.emitA(bytecode.LoadUndef, base+1)
	f.bind(end)
}

// deleteIdent compiles `delete id` (sloppy code only): a binding of a with
// object is deleted from it, a global object property from the global
// object; a declarative binding is not deleted.
func (f *funcState) deleteIdent(id *syntax.Ident, dst int) {
	mark := f.nregs
	defer f.free(mark)
	r := f.resolveIdent(id)
	var wl, end *label
	if r.ref >= 0 {
		wl, end = f.newLabel(), f.newLabel()
		f.emitJump(bytecode.JmpNotUndef, r.ref, wl)
	}
	if b := id.Binding; b == nil || f.c.locs[b].isGlobal() {
		f.emitA(bytecode.DelGlobal, dst)
		f.emitExtra(uint32(f.nameConst(id.Name)))
	} else {
		f.emitA(bytecode.LoadFalse, dst)
	}
	if r.ref >= 0 {
		f.emitJump(bytecode.Jmp, 0, end)
		f.bind(wl)
		f.emitAB(bytecode.DelPropSloppy, dst, r.ref)
		f.emitExtra(uint32(f.nameConst(id.Name)))
		f.bind(end)
	}
}

// setSuperOp is the op of an assignment to a super property.
func (f *funcState) setSuperOp() bytecode.Op {
	if f.out.Strict {
		return bytecode.SetSuper
	}
	return bytecode.SetSuperSloppy
}

// callTarget compiles a function call that sloppy code uses as the target of
// an assignment, an update or a for-in/of head (Annex B, Runtime Errors for
// Function Call Assignment Targets): the call runs, then a ReferenceError is
// thrown before any value is read or stored.
func (f *funcState) callTarget(c *syntax.CallExpr, msg string) {
	mark := f.nregs
	f.expr(c, f.alloc())
	f.emitABx(bytecode.ThrowError, int(bytecode.ThrowReferenceError), f.stringConst(msg))
	f.free(mark)
}

// forInInit evaluates the initializer of `for (var x = init in o)` in sloppy
// code (Annex B, Initializers in ForIn Statement Heads), once, before the
// object expression.
func (f *funcState) forInInit(d *syntax.VarDecl) {
	f.varDecl(d)
}

// annexBCopy evaluates a block-level function declaration of sloppy code
// that Annex B.3.2 extends: the var binding of its name in the enclosing
// function or script takes the function (for a script, unless a global
// lexical binding of the name exists, see SetGlobalVar).
func (f *funcState) annexBCopy(fd *syntax.FuncDecl) {
	if fd.Func.Name == nil {
		return
	}
	b := fd.Func.Name.Binding
	if b == nil || !b.AnnexB || b.Kind != syntax.BindFunction {
		return
	}
	vs := b.Scope.Parent
	for vs != nil && vs.Kind != syntax.ScopeFunction && vs.Kind != syntax.ScopeScript {
		vs = vs.Parent
	}
	if vs == nil || vs == b.Scope {
		return
	}
	mark := f.nregs
	defer f.free(mark)
	v := f.bindingReg(b)
	if vs.Kind == syntax.ScopeScript {
		f.emitA(bytecode.SetGlobalVar, v)
		f.emitExtra(uint32(f.nameConst(b.Name)))
		return
	}
	vb := vs.Lookup(b.Name)
	if vb == nil {
		// Sloppy eval code: its var declarations are its caller's (see
		// syntax's evalDecls).
		f.evalVarCopy(b.Name, v, vs)
		return
	}
	f.storeLoc(f.c.locs[vb], v)
}

// evalVarCopy stores register v to the var name that sloppy eval code
// whose scope is vs declared in its caller's variable environment: a
// binding of that environment, the property of its %evalvars object that
// the eval code's prologue created, or the global var.
func (f *funcState) evalVarCopy(name string, v int, vs *syntax.Scope) {
	for s := vs.Parent; s != nil; s = s.Parent {
		ev := s.Lookup(evalVarsName)
		if ev == nil {
			continue
		}
		if b := s.Lookup(name); b != nil && !b.Kind.IsLexical() && b.Kind != syntax.BindFuncName {
			f.storeLoc(f.c.locs[b], v)
			return
		}
		o := f.alloc()
		f.loadLoc(ev, o)
		f.emitAB(bytecode.ToObject, o, o) // for the verifier (evalDeclarations)
		f.emitAB(bytecode.DefineField, o, v)
		f.emitExtra(bytecode.ExtraArg(f.nameConst(name), f.newIC()))
		return
	}
	f.emitA(bytecode.SetGlobalVar, v)
	f.emitExtra(uint32(f.nameConst(name)))
}

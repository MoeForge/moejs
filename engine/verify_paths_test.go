package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// starve leaves fn no registers, so every register operand it has is past
// its window.
func starve(fn *bytecode.Function) *bytecode.Function {
	fn.NumRegs = 0
	return fn
}

// starvingCompiler is compiler.Hook with no registers left to the root of
// what it compiles.
type starvingCompiler struct{ compiler.Hook }

func (c starvingCompiler) CompileScript(name, src string, stop func() error) (*bytecode.Function, error) {
	fn, err := c.Hook.CompileScript(name, src, stop)
	if err == nil {
		starve(fn)
	}
	return fn, err
}

func (c starvingCompiler) CompileEval(name, src string, scope *bytecode.EvalScope, stop func() error) (*bytecode.Function, error) {
	fn, err := c.Hook.CompileEval(name, src, scope, stop)
	if err == nil {
		starve(fn)
	}
	return fn, err
}

func (c starvingCompiler) CompileFunction(name, params, body string, generator, async bool, stop func() error) (*bytecode.Function, error) {
	fn, err := c.Hook.CompileFunction(name, params, body, generator, async, stop)
	if err == nil {
		starve(fn)
	}
	return fn, err
}

// verifierError checks that err is the verifier's.
func verifierError(t *testing.T, err error, want string) {
	t.Helper()
	require.Error(t, err)
	assert.Regexp(t, `^engine: bytecode: `, err.Error())
	assert.Contains(t, err.Error(), want)
}

func verifyModule(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	m, err := syntax.ParseModule("v.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	return code
}

func verifyScript(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	s, err := syntax.ParseScript("v.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileScript(s)
	require.NoError(t, err)
	return code
}

// TestVerifierPaths checks that every way a function reaches the engine
// verifies it before any of its code runs (fillMeta) and returns the
// verifier's error: module roots and their children, scripts, hand-made
// functions, Realm.AllocIC, handler registers, and the code compiled at
// run time, by Realm.EvalScript, direct and indirect eval and the four
// Function constructors. Code compiled from a string throws the error as
// an Error the program can catch, as a compile error.
func TestVerifierPaths(t *testing.T) {
	const names = "names registers"
	t.Run("module root", func(t *testing.T) {
		code := starve(verifyModule(t, `const a = [1, 2]; export const n = a.length;`))
		_, err := NewRealm().EvaluateModule(code)
		verifierError(t, err, names)
	})
	t.Run("a child no one calls", func(t *testing.T) {
		code := verifyModule(t, `export function f(a, b) { return a + b; } export const ok = 1;`)
		starve(code.Children[0])
		_, err := NewRealm().EvaluateModule(code)
		verifierError(t, err, names)
	})
	t.Run("a grandchild", func(t *testing.T) {
		code := verifyModule(t, `export function f() { return function (a) { return a * 2; }; }`)
		require.NotEmpty(t, code.Children[0].Children)
		starve(code.Children[0].Children[0])
		_, err := NewRealmWith(RealmOptions{SharedIntrinsics: true}).EvaluateModule(code)
		verifierError(t, err, names)
	})
	t.Run("script", func(t *testing.T) {
		code := starve(verifyScript(t, `var leaked = [1]; function leakedFn() {} leaked.length`))
		r := NewRealm()
		_, err := r.RunScript(code)
		verifierError(t, err, names)
		assert.Equal(t, "undefined,undefined", runScriptsIn(t, r, `[typeof leaked, typeof leakedFn].join()`),
			"the script declared its globals before the verifier failed it")
	})
	t.Run("hand-made function", func(t *testing.T) {
		code := &bytecode.Function{Name: "f", NumRegs: 2, Strict: true, Code: []uint32{
			bytecode.EncodeABC(bytecode.Move, 2, 0, 0), bytecode.EncodeABC(bytecode.RetUndef, 0, 0, 0)}}
		_, err := NewRealm().metaFor(code)
		verifierError(t, err, names)
	})
	t.Run("SetHostDefined", func(t *testing.T) {
		code := starve(verifyScript(t, `var a = [1]; eval("a")`))
		require.True(t, code.ScriptOrModule, "a direct eval gives the script a record")
		r := NewRealm()
		verifierError(t, r.SetHostDefined(code, "host"), names)
		assert.True(t, r.lazy == nil || r.lazy.modules == nil || len(r.lazy.modules.refs) == 0, "nothing recorded")
		assert.NoError(t, r.SetHostDefined(starve(verifyScript(t, `var b = [1]; b`)), "host"), "a template without a record is not looked at")
		assert.NoError(t, r.SetHostDefined(verifyScript(t, `var c = [1]; eval("c")`), "host"))
	})
	t.Run("AllocIC", func(t *testing.T) {
		code := starve(verifyScript(t, `var o = {x: 1}; o.x`))
		_, err := NewRealm().AllocIC(code, 1)
		verifierError(t, err, names)
	})
	t.Run("handler register", func(t *testing.T) {
		code := verifyModule(t, `export function f() { try { return g(); } catch (e) { return e; } }`)
		f := code.Children[0]
		require.NotEmpty(t, f.Handlers)
		f.Handlers[0].Reg = f.NumRegs
		_, err := NewRealm().EvaluateModule(code)
		verifierError(t, err, "writes registers")
	})

	// Code compiled at run time, through the installed compiler.
	SetCompiler(starvingCompiler{})
	t.Cleanup(func() { SetCompiler(nil) })
	t.Run("EvalScript", func(t *testing.T) {
		r := NewRealm()
		_, err := r.EvalScript("e.js", "var leaked = [1]; leaked.length")
		verifierError(t, err, names)
		assert.Equal(t, "undefined", runScriptsIn(t, r, `typeof leaked`), "the script declared its globals before the verifier failed it")
	})
	for name, src := range map[string]string{
		"direct eval":            `function f() { var x = 1; return eval("ran = x + 1"); } f();`,
		"indirect eval":          `(0, eval)("ran = [1].length");`,
		"Function":               `new Function("a", "ran = a + 1")(1);`,
		"GeneratorFunction":      `new (Object.getPrototypeOf(function* () {}).constructor)("ran = 1; yield 1")().next();`,
		"AsyncFunction":          `new (Object.getPrototypeOf(async function () {}).constructor)("ran = 1; return 1")();`,
		"AsyncGeneratorFunction": `new (Object.getPrototypeOf(async function* () {}).constructor)("ran = 1; yield 1")().next();`,
	} {
		t.Run(name, func(t *testing.T) {
			// Compiled as is; what it compiles at run time is starved.
			got := runScriptsIn(t, NewRealm(), `var ran = 0, msg; try { `+src+` } catch (e) { msg = e.message; } [ran, msg].join()`)
			assert.Regexp(t, `^0,engine: bytecode: .*`+names, got)
		})
	}
}

// TestVerifierControlFlow checks through the engine what run reaches: a
// jump, a handler or a module body that lands inside an instruction fails
// the verifier.
func TestVerifierControlFlow(t *testing.T) {
	ret := bytecode.EncodeABC(bytecode.RetUndef, 0, 0, 0)
	x := bytecode.EncodeABC(bytecode.Move, 200, 0, 0) // as the X word of an immediate compare
	jump := &bytecode.Function{Name: "jump", NumRegs: 1, Strict: true, Code: []uint32{
		bytecode.EncodeAsBx(bytecode.Jmp, 0, 1), bytecode.EncodeAsBx(bytecode.JmpLtI, 0, 1), x, ret}}
	_, err := NewRealm().metaFor(jump)
	verifierError(t, err, "Jmp jumps to 2, which starts no instruction")

	catch := &bytecode.Function{Name: "catch", NumRegs: 1, Strict: true, Code: []uint32{
		bytecode.EncodeABC(bytecode.Throw, 0, 0, 0), bytecode.EncodeAsBx(bytecode.JmpLtI, 0, 1), x, ret},
		Handlers: []bytecode.Handler{{Start: 0, End: 1, Handler: 2, Kind: bytecode.HandlerCatch, Reg: 0}}}
	_, err = NewRealm().metaFor(catch)
	verifierError(t, err, "handler 0 continues at 2, which starts no instruction")

	// The source picks the X word: x < 51209 compares with the integer
	// whose bits are Move R[200], R[0]. A jump onto it, as a compiler bug
	// could make, fails.
	require.Equal(t, uint32(51209), x, "Move's opcode moved: the source no longer encodes Move R[200], R[0]")
	code := verifyScript(t, `function f(x) { if (x < 51209) return 1; return 0; } f(0)`)
	f := code.Children[0]
	at := -1
	for pc := 0; pc < len(f.Code); pc += 1 + bytecode.DecodeOp(f.Code[pc]).ExtraWords() {
		if op := bytecode.DecodeOp(f.Code[pc]); op.Jumps() && op.ExtraWords() == 1 && f.Code[pc+1] == x {
			at = pc
		}
	}
	require.GreaterOrEqual(t, at, 0, "no compare against the X word %d", x)
	r := NewRealm()
	assert.Equal(t, "1", runScriptsIn(t, r, `(function f(x) { if (x < 51209) return 1; return 0; })(0)`))
	f.Code[at] = bytecode.EncodeAsBx(bytecode.DecodeOp(f.Code[at]), bytecode.DecodeA(f.Code[at]), 0) // onto its own X word
	_, err = NewRealm().RunScript(code)
	verifierError(t, err, "which starts no instruction")

	m := verifyModule(t, `export const a = [1, 2].length;`)
	m.Module.BodyPC = len(m.Code)
	_, err = NewRealm().EvaluateModule(m)
	verifierError(t, err, "the module body's pc")
}

// TestVerifierKindOperands checks through the engine the proof of the
// operands run reads as a kind without a check (bytecode.KindOperands):
// hand-made code with another kind of value there fails the verifier, and
// so does a member initializer whose this the compiler no longer makes an
// object (loadThisObject). What the compiler emits at those operands
// passes and runs: literals, spread calls, an object rest pattern, class
// fields of every sort, for-in across yields and handlers, the variables
// and functions sloppy direct eval declares in its caller, a module body.
func TestVerifierKindOperands(t *testing.T) {
	abc := bytecode.EncodeABC
	obj := bytecode.EncodeABx(bytecode.NewObject, 0, 0)
	ret := abc(bytecode.RetUndef, 0, 0, 0)
	for _, c := range []struct {
		name, want string
		code       []uint32
	}{
		{"DefineField on a string", "DefineField reads register 0 as an object", []uint32{abc(bytecode.Typeof, 0, 1, 0), abc(bytecode.DefineField, 0, 1, 0), 0, ret}},
		{"ArrayPush on a plain object", "ArrayPush reads register 0 as an array", []uint32{obj, abc(bytecode.ArrayPush, 0, 1, 0), ret}},
		{"ForInNext without ForInInit", "ForInNext reads register 1 as a for-in index", []uint32{obj, bytecode.EncodeAsBx(bytecode.LoadInt, 1, 0), bytecode.EncodeAsBx(bytecode.ForInNext, 0, 0), ret}},
	} {
		_, err := NewRealm().metaFor(&bytecode.Function{Name: "f", NumRegs: 8, Strict: true, Code: c.code})
		verifierError(t, err, c.want)
	}

	// The instance member initializer of A: LoadThis, then ToObject of it.
	code := verifyScript(t, `class A { x = 1; y = this.x + 1; } new A().y`)
	var edit func(fn *bytecode.Function) bool
	edit = func(fn *bytecode.Function) bool {
		for pc := 0; pc+1 < len(fn.Code); pc += 1 + bytecode.DecodeOp(fn.Code[pc]).ExtraWords() {
			if w, n := fn.Code[pc], fn.Code[pc+1]; bytecode.DecodeOp(w) == bytecode.LoadThis && n == abc(bytecode.ToObject, bytecode.DecodeA(w), bytecode.DecodeA(w), 0) {
				fn.Code[pc+1] = abc(bytecode.Move, bytecode.DecodeA(w), bytecode.DecodeA(w), 0)
				return true
			}
		}
		return slices.ContainsFunc(fn.Children, edit)
	}
	assert.Equal(t, "2", runScriptsIn(t, NewRealm(), `class A { x = 1; y = this.x + 1; } new A().y`))
	require.True(t, edit(code), "no initializer makes its this an object")
	_, err := NewRealm().RunScript(code)
	verifierError(t, err, "DefineField reads register")

	SetCompiler(compiler.Hook{}) // for direct eval
	t.Cleanup(func() { SetCompiler(nil) })
	got := runScriptsIn(t, NewRealm(), `
		var log = [];
		var o = {a: 1, ['b']: 2, get c() { return 3; }, __proto__: null, ...{d: 4}};
		var arr = [1, , ...[2, 3], 4];
		function sum() { var s = 0; for (var i = 0; i < arguments.length; i++) s += arguments[i]; return s; }
		var {a, ...rest} = {a: 1, b: 2, c: 3};
		log.push(o.a + o.b + o.c + o.d, arr.length, sum(...[1, 2, 3, 4], 5), Object.keys(rest).join());
		var k = 'kk';
		class B { pub = 1; [k] = 2; #priv = 3; static st = 4; static { this.blk = 5; } #m() { return this.#priv; } get m() { return this.#m(); } }
		class C extends B { more = this.pub + 10; }
		var c = new C();
		log.push(c.pub + c.kk + c.m + B.st + B.blk + c.more);
		function* g(obj) { for (var p in obj) { try { yield p; for (var q in {z: 1}) yield q; } catch (e) { yield 'caught'; } } }
		log.push([...g({x: 1, y: 2})].join());
		function ev() { eval('var e1 = 1; function e2() { return 2; } { function e3() { return 3; } }'); return e1 + e2() + e3(); }
		log.push(ev());
		log.join('|')`)
	assert.Equal(t, "10|5|15|b,c|26|x,z,y,z|6", got)
	_, err = NewRealm().EvaluateModule(verifyModule(t, `export const o = {a: [1, 2]}; export class D { f = 1; } for (const k in o) o[k].push(new D().f);`))
	assert.NoError(t, err)
}

// TestVerifierHandlerKindsAndWideFrames runs the audit's two programs
// through the engine: a handler row of kind 2, which run enters as a
// finally (writing R[Reg+1], here the object DefineField reads), and a
// frame of 300 registers whose UndefRange reaches past 255, also as the
// child of a script. Each is the verifier's error before any code runs,
// not a string read as an object or a panic.
func TestVerifierHandlerKindsAndWideFrames(t *testing.T) {
	abc := bytecode.EncodeABC
	thrown := func(kind bytecode.HandlerKind) *bytecode.Function {
		return &bytecode.Function{Name: "f", NumRegs: 4, Strict: true, ICCount: 1,
			Consts: []bytecode.Const{{Kind: bytecode.ConstString, Str: "abc", IsASCII: true, Key: true}},
			Code: []uint32{bytecode.EncodeABx(bytecode.NewObject, 1, 0), bytecode.EncodeABx(bytecode.LoadConst, 2, 0), abc(bytecode.Throw, 2, 0, 0),
				abc(bytecode.DefineField, 1, 0, 0), bytecode.ExtraArg(0, 0), abc(bytecode.Ret, 1, 0, 0)},
			Handlers: []bytecode.Handler{{Start: 1, End: 3, Handler: 3, Kind: kind, Reg: 0}}}
	}
	_, err := NewRealm().metaFor(thrown(2))
	verifierError(t, err, "handler 0 is of kind 2, neither a catch nor a finally")
	_, err = NewRealm().metaFor(thrown(bytecode.HandlerFinally))
	verifierError(t, err, "DefineField reads register 1 as an object")
	r := NewRealm()
	f, _ := testClosure(r, thrown(bytecode.HandlerCatch))
	v, err := r.CallObject(f, Undefined(), nil)
	require.NoError(t, err)
	assert.True(t, v.IsObject(), "the catch writes R[0] alone and the field goes on the object")

	wide := &bytecode.Function{Name: "f", NumRegs: 300, Strict: true, ICCount: 1, Code: []uint32{
		bytecode.EncodeABx(bytecode.NewObject, 0, 0), abc(bytecode.UndefRange, 250, 20, 0),
		abc(bytecode.DefineField, 0, 1, 0), bytecode.ExtraArg(0, 0), abc(bytecode.RetUndef, 0, 0, 0)}}
	_, err = NewRealm().metaFor(wide)
	verifierError(t, err, "300 registers, more than the 256 of a frame's window")
	code := verifyScript(t, `function f() { return 1; } f()`)
	*code.Children[0] = *wide
	_, err = NewRealm().RunScript(code)
	verifierError(t, err, "more than the 256 of a frame's window")
}

// TestVerifierManyParameters runs functions of 255 and 256 parameters,
// whose frames fill the window the verifier allows, and has one of 300 be
// the compiler's SyntaxError rather than a template the verifier refuses.
func TestVerifierManyParameters(t *testing.T) {
	params := func(n int) string {
		ps := make([]string, n)
		for i := range ps {
			ps[i] = "p" + strconv.Itoa(i)
		}
		return strings.Join(ps, ", ")
	}
	assert.Equal(t, "7,255", runScriptsIn(t, NewRealm(), `function f(`+params(255)+`) { return p0; } [f(7), f.length].join()`))
	assert.Equal(t, "8,256", runScriptsIn(t, NewRealm(), `function f(`+params(256)+`) { return p255; } var a = []; a[255] = 8; [f(...a), f.length].join()`))
	s, err := syntax.ParseScript("v.js", `function f(`+params(300)+`) { return p0; } f(7)`, syntax.Options{})
	require.NoError(t, err)
	_, err = compiler.CompileScript(s)
	assert.ErrorContains(t, err, "SyntaxError: function needs more than 256 registers")
}

// TestVerifierLeavesNoModule checks that a graph with a module that fails
// the verifier changes nothing in the realm: no module of it is
// instantiated, and no instance takes another template. A later graph of
// the same modules evaluates as if the failed one had not been tried.
func TestVerifierLeavesNoModule(t *testing.T) {
	t.Run("a dependency", func(t *testing.T) {
		h := newModuleHost(t, map[string]string{
			"main": `import { v } from "dep"; export const out = v + 1;`,
			"bad":  `export const v = [1].length;`,
			"good": `export const v = [1, 2].length;`,
		})
		starve(h.module("bad"))
		link := func(dep string) *ModuleGraph {
			g, err := LinkModules(h.module("main"), LinkOptions{Resolve: func(referrer *bytecode.Function, request int) (*bytecode.Function, error) {
				return h.module(dep), nil
			}})
			require.NoError(t, err)
			return g
		}
		r := NewRealm()
		_, _, err := r.EvaluateGraph(link("bad"))
		verifierError(t, err, "names registers")
		g := link("good")
		_, _, err = r.EvaluateGraph(g)
		require.NoError(t, err, "main was instantiated with the failed graph's dependency")
		assert.EqualValues(t, 3, graphExport(t, r, g, "out"))
	})
	t.Run("an EarlyExports variant", func(t *testing.T) {
		// TestModuleEarlyExportsAcrossGraphs' first case: unrun instantiates
		// x plain and does not run it; main needs x's variant.
		srcs := map[string]string{
			"x":     `let m = "M"; export function g() { return m; }`,
			"y":     `throw new Error("y");`,
			"main":  `import "b"; import { g } from "x"; export { out } from "b"; export function f() { return g(); }`,
			"b":     `import { f } from "main"; export let out; try { const v = f(); out = typeof v + ":" + String(v); } catch (e) { out = e.name; }`,
			"unrun": `import "y"; import "x";`,
		}
		h := newModuleHost(t, srcs)
		early := func(bad bool) func(*bytecode.Function) (*bytecode.Function, error) {
			return func(code *bytecode.Function) (*bytecode.Function, error) {
				m, err := syntax.ParseModule(code.Source.Name, srcs[code.Source.Name], syntax.Options{EarlyExports: true})
				require.NoError(t, err)
				v, err := compiler.CompileModule(m)
				if bad && err == nil {
					starve(v)
				}
				return v, err
			}
		}
		link := func(entry string, bad bool) *ModuleGraph {
			g, err := LinkModules(h.module(entry), LinkOptions{Resolve: h.resolve, EarlyExports: early(bad)})
			require.NoError(t, err)
			return g
		}
		r := NewRealm()
		_, _, err := r.EvaluateGraph(link("unrun", false))
		require.EqualError(t, err, "Error: y")
		_, _, err = r.EvaluateGraph(link("main", true))
		verifierError(t, err, "names registers")
		g := link("main", false)
		_, _, err = r.EvaluateGraph(g)
		require.NoError(t, err)
		assert.Equal(t, "ReferenceError", graphExport(t, r, g, "out"), "the failed graph replaced x's template")
	})
}

// TestSavesPCOps checks bytecode.Op.SavesPC against the engine's sources:
// an op whose case saves a pc to resume its frame at (genFrame.save) or
// makes run rerun the frame (errFrameOp) is one whose functions the
// verifier keeps below 1<<bytecode.PCBits words.
func TestSavesPCOps(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	byName := map[string]bytecode.Op{}
	for op := bytecode.Op(0); int(op) < bytecode.OpCount; op++ {
		byName[op.String()] = op
	}
	found := map[bytecode.Op]bool{}
	fs := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fs, path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			saves := false
			for _, st := range cc.Body {
				ast.Inspect(st, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						saves = saves || x.Sel.Name == "save"
					case *ast.Ident:
						saves = saves || x.Name == "errFrameOp"
					}
					return true
				})
			}
			if !saves {
				return true
			}
			for _, e := range cc.List {
				se, ok := e.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if id, ok := se.X.(*ast.Ident); !ok || id.Name != "bytecode" {
					continue
				}
				op, ok := byName[se.Sel.Name]
				require.True(t, ok, se.Sel.Name)
				found[op] = true
				assert.True(t, op.SavesPC(), "%s: %s saves a pc to run its frame from; bytecode.Op.SavesPC leaves it out", fs.Position(cc.Pos()), op)
			}
			return true
		})
	}
	for _, op := range []bytecode.Op{bytecode.Yield, bytecode.Await, bytecode.CoerceThis, bytecode.CallEval} {
		assert.True(t, found[op], "found no case of %s that saves a pc", op)
	}
}

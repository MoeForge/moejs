package engine

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWrongKindOperands runs hand-made functions whose operand holds
// another kind of value than its op takes from the compiler (a string where
// it reads an object, a function, a private name or an import binding; a
// plain object where it reads an array, a generator or an arguments
// object): each must end in the internal TypeError (Realm.wrongKind), not
// read one kind of value as another (the fast build's payload accessors do
// not look; the checked build's would panic) and not panic. The three
// operands run reads without a check, which the verifier proves, are
// TestVerifierKindOperands's and TestRegCheckKindOperands's.
func TestWrongKindOperands(t *testing.T) {
	const (
		x   = 0 // K0, the string "x", a key
		obj = 1 // K1, "Object", a key
	)
	abc := bytecode.EncodeABC
	str := func(r uint8) uint32 { return bytecode.EncodeABx(bytecode.LoadConst, r, x) }   // R[r] = "x"
	plain := func(r uint8) uint32 { return bytecode.EncodeABx(bytecode.NewObject, r, 0) } // R[r] = {}
	array := func(r uint8) uint32 { return bytecode.EncodeABx(bytecode.NewArray, r, 0) }  // R[r] = []
	pn := func(r uint8) uint32 { return bytecode.EncodeABx(bytecode.NewPrivateName, r, x) }
	ret := abc(bytecode.RetUndef, 0, 0, 0)
	cases := []struct {
		name, want string
		code       []uint32
	}{
		// Literals.
		{"DefineElem", "DefineElem expects an object", []uint32{str(0), abc(bytecode.DefineElem, 0, 1, 2), ret}},
		{"DefineMethod's object", "DefineMethod expects an object", []uint32{str(0), abc(bytecode.DefineMethod, 0, 1, 2), ret}},
		{"DefineMethod's function", "DefineMethod expects a closure", []uint32{plain(0), str(2), abc(bytecode.DefineMethod, 0, 1, 2), ret}},
		{"DefineAccessor's function", "DefineAccessor expects a closure", []uint32{plain(0), str(2), abc(bytecode.DefineAccessor, 0, 1, 2), 0, ret}},
		{"SetProto", "SetProto expects an object", []uint32{str(0), abc(bytecode.SetProto, 0, 1, 0), ret}},
		{"CopyDataProps", "CopyDataProps expects an object", []uint32{str(0), abc(bytecode.CopyDataProps, 0, 1, 0), ret}},
		{"CopyDataPropsEx", "CopyDataPropsEx expects an object", []uint32{str(0), abc(bytecode.CopyDataPropsEx, 0, 1, 2), ret}},
		{"ArrayHole", "ArrayHole expects an array literal", []uint32{plain(0), abc(bytecode.ArrayHole, 0, 0, 0), ret}},
		{"AppendSpread", "AppendSpread expects an array literal", []uint32{str(0), abc(bytecode.AppendSpread, 0, 1, 0), ret}},
		// Imports: a slot that holds no import binding.
		{"GetImport", "GetImport expects an import binding", []uint32{bytecode.EncodeABx(bytecode.PushEnv, 0, 1), abc(bytecode.GetImport, 0, 0, 0), x, ret}},
		{"GetImportW", "GetImportW expects an import binding", []uint32{bytecode.EncodeABx(bytecode.PushEnv, 0, 1), abc(bytecode.GetImportW, 0, 0, 0), 0, x, ret}},
		// Classes and private names.
		{"SetHome's function", "SetHome expects a closure", []uint32{plain(0), plain(1), abc(bytecode.SetHome, 0, 1, 0), ret}},
		{"SetHome's home", "SetHome expects an object", []uint32{bytecode.EncodeABx(bytecode.Closure, 0, 0), str(1), abc(bytecode.SetHome, 0, 1, 0), ret}},
		{"CreateClass", "CreateClass expects a closure", []uint32{str(0), abc(bytecode.CreateClass, 0, 2, 0), ret}},
		{"DefineClassMethod's object", "DefineClassMethod expects an object", []uint32{str(0), abc(bytecode.DefineClassMethod, 0, 1, 2), ret}},
		{"DefineClassMethod's function", "DefineClassMethod expects a closure", []uint32{plain(0), str(2), abc(bytecode.DefineClassMethod, 0, 1, 2), ret}},
		{"SetPrivateMethod's name", "SetPrivateMethod expects a private name", []uint32{str(0), abc(bytecode.SetPrivateMethod, 0, 1, 2), 0, ret}},
		{"SetPrivateMethod's function", "SetPrivateMethod expects an object", []uint32{pn(0), str(1), abc(bytecode.SetPrivateMethod, 0, 1, 2), 0, ret}},
		{"SetPrivateMethod's brand", "SetPrivateMethod expects a private name", []uint32{pn(0), plain(1), str(2), abc(bytecode.SetPrivateMethod, 0, 1, 2), 0, ret}},
		{"SetPrivateMethod's class", "SetPrivateMethod expects an object", []uint32{pn(0), plain(1), str(2), abc(bytecode.SetPrivateMethod, 0, 1, 2), bytecode.PrivateStatic, ret}},
		{"GetPrivate", "GetPrivate expects a private name", []uint32{plain(1), str(2), abc(bytecode.GetPrivate, 0, 1, 2), ret}},
		{"SetPrivate", "SetPrivate expects a private name", []uint32{plain(0), str(1), abc(bytecode.SetPrivate, 0, 1, 2), ret}},
		{"DefPrivate's name", "DefPrivate expects a private name", []uint32{plain(0), str(1), abc(bytecode.DefPrivate, 0, 1, 2), ret}},
		{"DefPrivate's instance", "DefPrivate expects an object", []uint32{str(0), pn(1), abc(bytecode.DefPrivate, 0, 1, 2), ret}},
		{"AddBrand's instance", "AddBrand expects an object", []uint32{str(0), pn(1), abc(bytecode.AddBrand, 0, 1, 0), ret}},
		{"InPrivate", "InPrivate expects a private name", []uint32{str(1), plain(2), abc(bytecode.InPrivate, 0, 1, 2), ret}},
		{"SuperCall's new.target", "SuperCall expects new.target", []uint32{abc(bytecode.GetGlobal, 0, 0, 0), obj, str(1), abc(bytecode.SuperCall, 0, 0, 0), ret}},
		{"SuperCallSpread's new.target", "SuperCall expects new.target", []uint32{abc(bytecode.GetGlobal, 0, 0, 0), obj, str(1), array(2), abc(bytecode.SuperCallSpread, 0, 0, 0), ret}},
		// Generators and async functions.
		{"GenFunc", "GenFunc expects a closure", []uint32{str(0), abc(bytecode.GenFunc, 0, 0, 0), ret}},
		// The closure a Closure just made: not a native (Function.prototype),
		// and of the right kind of function.
		{"GenFunc on Function.prototype", "GenFunc expects a closure", []uint32{bytecode.EncodeABx(bytecode.Closure, 0, 0), abc(bytecode.GetProtoOf, 1, 0, 0), abc(bytecode.GenFunc, 1, 0, 0), ret}},
		{"GenFunc on another closure", "GenFunc expects a generator function", []uint32{bytecode.EncodeABx(bytecode.Closure, 0, 0), abc(bytecode.GenFunc, 0, 0, 0), ret}},
		{"AsyncFunc on another closure", "AsyncFunc expects an async function", []uint32{bytecode.EncodeABx(bytecode.Closure, 0, 0), abc(bytecode.AsyncFunc, 0, 0, 0), ret}},
		{"Yield on a string", "Yield expects an object", []uint32{str(1), abc(bytecode.Yield, 2, 1, 0), ret}},
		{"Yield on a plain object", "Yield expects a generator", []uint32{plain(1), abc(bytecode.Yield, 2, 1, 0), ret}},
		{"AsyncFunc", "AsyncFunc expects a closure", []uint32{str(0), abc(bytecode.AsyncFunc, 0, 0, 0), ret}},
		{"Await on a string", "Await expects an object", []uint32{str(1), abc(bytecode.Await, 2, 1, 0), ret}},
		{"Await on a plain object", "Await expects an async function or generator", []uint32{plain(1), abc(bytecode.Await, 2, 1, 0), ret}},
		{"AsyncReturn", "AsyncReturn expects an async function", []uint32{plain(1), abc(bytecode.AsyncReturn, 2, 1, 0), ret}},
		{"AsyncThrow", "AsyncThrow expects an async function", []uint32{str(1), abc(bytecode.AsyncThrow, 2, 1, 0), ret}},
		{"AsyncYield", "AsyncYield expects an async generator", []uint32{plain(1), abc(bytecode.AsyncYield, 2, 1, 0), ret}},
		// Sloppy code.
		{"WithGet", "WithGet expects an object", []uint32{str(1), abc(bytecode.WithGet, 0, 1, 0), x, ret}},
		{"WithSet", "WithSet expects an object", []uint32{str(0), abc(bytecode.WithSet, 0, 1, 0), x, ret}},
		{"MapArguments", "MapArguments expects an arguments object", []uint32{plain(0), abc(bytecode.MapArguments, 0, 0, 0), ret}},
	}
	child := &bytecode.Function{Name: "c", Strict: true, Code: []uint32{ret}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fn := &bytecode.Function{Name: "f", NumRegs: 8, Strict: true, ICCount: 2, Code: c.code,
				Consts:   []bytecode.Const{{Kind: bytecode.ConstString, Str: "x", IsASCII: true, Key: true}, {Kind: bytecode.ConstString, Str: "Object", IsASCII: true, Key: true}},
				Children: []*bytecode.Function{child}}
			require.NoError(t, bytecode.VerifyRegisters(fn))
			r := NewRealm()
			f, _ := testClosure(r, fn)
			var err error
			require.NotPanics(t, func() { _, err = r.CallObject(f, Undefined(), nil) })
			require.Error(t, err)
			assert.Contains(t, fmt.Sprint(err), "TypeError")
			assert.Contains(t, fmt.Sprint(err), "internal: "+c.want)
		})
	}

	// A shared intrinsic is not the realm's to write: Array.prototype is an
	// array, but no literal.
	t.Run("ArrayHole on the shared Array.prototype", func(t *testing.T) {
		fn := &bytecode.Function{Name: "f", NumRegs: 2, Strict: true, Code: []uint32{
			array(0), abc(bytecode.GetProtoOf, 1, 0, 0), abc(bytecode.ArrayHole, 1, 0, 0), ret}}
		r := newShared()
		require.True(t, r.ArrayPrototype.IsShared())
		f, _ := testClosure(r, fn)
		_, err := r.CallObject(f, Undefined(), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "internal: ArrayHole expects an array literal")
		assert.Empty(t, r.ArrayPrototype.elements)
	})

	// The async-from-sync iterator's methods on another this.
	r := NewRealm()
	for _, m := range []func(*Realm, Value, []Value) (Value, error){asyncFromSyncNext, asyncFromSyncReturn, asyncFromSyncThrow} {
		_, err := m(r, StringValue(FromGoString("x")), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "internal: an async-from-sync iterator method called on another object")
	}
}

// TestGenFuncKeepsIntrinsics runs FuzzBytecode's input that made GenFunc of
// Function.prototype in a realm with shared intrinsics: makeGeneratorFunction
// set its [[Prototype]] to %GeneratorFunction.prototype%, whose own is
// Function.prototype, and every lookup through the shared intrinsics of the
// process looped from then on. GenFunc takes only a generator function's
// closure now.
func TestGenFuncKeepsIntrinsics(t *testing.T) {
	code := fuzzCompile(t, "function f(a){for(A of a+0)A.A=0}f([0,0])", true)
	fuzzEdit(fuzzTree(code, nil), []byte("02[0\x7f\x00pp0p"))
	r := newShared()
	_, err := r.EvaluateModule(code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal: GenFunc expects a closure")
	assert.Same(t, r.ObjectPrototype, r.FunctionPrototype.proto, "Function.prototype's prototype")
	assert.Equal(t, "function", runScriptsIn(t, newShared(), `typeof Function.prototype.call`), "a fresh shared realm")
}

// TestModulePrologueKinds instantiates one-module graphs, whose module's
// prologue runs outside run (runModulePrologue), where its GenFunc or
// AsyncFunc gets no fresh closure of its kind: the hole a LoadHole wrote,
// a register no instruction wrote, which keeps what an earlier user of the
// stack left above its top (a string, or Function.prototype of a realm with
// shared intrinsics), or a closure of the other kind. Each must end in the
// internal TypeError, not complete a value it reads as a closure.
func TestModulePrologueKinds(t *testing.T) {
	const src = "export function* g() {} export async function a() {} export async function* ag() {}"
	// prologue compiles src and returns the pcs of its prologue's GenFunc
	// and of the AsyncFunc after it, each after the Closure it completes.
	prologue := func(t *testing.T) (code *bytecode.Function, gen, async int) {
		code = fuzzCompile(t, src, true)
		require.NotNil(t, code)
		gen, async = -1, -1
		for pc := 0; pc < code.Module.BodyPC; {
			switch op := bytecode.DecodeOp(code.Code[pc]); {
			case op == bytecode.GenFunc && gen < 0:
				gen = pc
			case op == bytecode.AsyncFunc && async < 0:
				async = pc
			}
			pc += 1 + bytecode.DecodeOp(code.Code[pc]).ExtraWords()
		}
		require.True(t, gen > 0 && async > 0, "the prologue completes a generator and an async function")
		require.Equal(t, bytecode.Closure, bytecode.DecodeOp(code.Code[gen-1]))
		require.Equal(t, bytecode.Closure, bytecode.DecodeOp(code.Code[async-1]))
		return code, gen, async
	}
	reg := func(w uint32) uint8 { return uint8(w >> 8) }
	hole := func(r uint8) uint32 { return bytecode.EncodeABC(bytecode.LoadHole, r, 0, 0) }
	// unwritten points the op at pc to a register past the others, which
	// no instruction writes.
	unwritten := func(code *bytecode.Function, pc int) {
		code.Code[pc] = bytecode.EncodeABC(bytecode.DecodeOp(code.Code[pc]), uint8(code.NumRegs), 0, 0)
		code.NumRegs++
	}
	// stale leaves v in the registers the prologue will take.
	stale := func(r *Realm, n int, v Value) {
		regs := r.pushArgs(n)
		for i := range regs {
			regs[i] = v
		}
		r.popArgs(n)
	}
	for _, c := range []struct {
		name, want string
		shared     bool
		stale      func(r *Realm) Value
		edit       func(code *bytecode.Function, gen, async int)
	}{
		{"LoadHole then GenFunc", "GenFunc expects a closure", false, nil,
			func(code *bytecode.Function, gen, _ int) { code.Code[gen-1] = hole(reg(code.Code[gen])) }},
		{"GenFunc on a stale string", "GenFunc expects a closure", false, func(*Realm) Value { return StringValue(FromGoString("x")) },
			func(code *bytecode.Function, gen, _ int) { unwritten(code, gen) }},
		{"GenFunc on a stale Function.prototype", "GenFunc expects a closure", true, func(r *Realm) Value { return ObjectValue(r.FunctionPrototype) },
			func(code *bytecode.Function, gen, _ int) { unwritten(code, gen) }},
		{"AsyncFunc on a stale string", "AsyncFunc expects a closure", false, func(*Realm) Value { return StringValue(FromGoString("x")) },
			func(code *bytecode.Function, _, async int) { unwritten(code, async) }},
		{"AsyncFunc on a stale Function.prototype", "AsyncFunc expects a closure", true, func(r *Realm) Value { return ObjectValue(r.FunctionPrototype) },
			func(code *bytecode.Function, _, async int) { unwritten(code, async) }},
		{"GenFunc on an async function", "GenFunc expects a generator function", false, nil,
			func(code *bytecode.Function, _, async int) {
				code.Code[async] = bytecode.EncodeABC(bytecode.GenFunc, reg(code.Code[async]), 0, 0)
			}},
		{"AsyncFunc on a generator function", "AsyncFunc expects an async function", false, nil,
			func(code *bytecode.Function, gen, _ int) {
				code.Code[gen] = bytecode.EncodeABC(bytecode.AsyncFunc, reg(code.Code[gen]), 0, 0)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, gen, async := prologue(t)
			c.edit(code, gen, async)
			require.NoError(t, bytecode.VerifyRegisters(code))
			g, err := LinkModules(code, LinkOptions{})
			require.NoError(t, err)
			r := NewRealm()
			if c.shared {
				r = newShared()
			}
			if c.stale != nil {
				stale(r, int(code.NumRegs), c.stale(r))
			}
			require.NotPanics(t, func() { _, _, err = r.EvaluateGraph(g) })
			require.Error(t, err)
			assert.Contains(t, fmt.Sprint(err), "TypeError")
			assert.Contains(t, fmt.Sprint(err), "internal: "+c.want)
			assert.Same(t, r.ObjectPrototype, r.FunctionPrototype.proto, "Function.prototype's prototype")
		})
	}
	// The prologue as compiled completes all three.
	code, _, _ := prologue(t)
	g, err := LinkModules(code, LinkOptions{})
	require.NoError(t, err)
	r := NewRealm()
	menv, _, err := r.EvaluateGraph(g)
	require.NoError(t, err)
	ai := r.asyncIntr()
	for name, want := range map[string]*Object{"g": r.generatorIntrinsics().GeneratorFunctionPrototype, "a": ai.AsyncFunctionPrototype, "ag": ai.AsyncGeneratorFunctionPrototype} {
		v, ok := menv.GetBindingValue(name)
		require.True(t, ok, name)
		assert.Same(t, want, v.AsObject().proto, name)
	}
}

// TestAsyncModuleBodyPromise runs modules with top-level await whose body
// returns no promise (its first instruction, the LoadThis of the async
// body, replaced: bytecode no compiler makes), standalone and as a
// one-module graph: each ends in the internal TypeError, not a value read
// as the body's promise.
func TestAsyncModuleBodyPromise(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(code []uint32, pc int)
	}{
		{"undefined", func(code []uint32, pc int) { code[pc] = bytecode.EncodeABC(bytecode.RetUndef, 0, 0, 0) }},
		{"a plain object", func(code []uint32, pc int) {
			a := uint8(code[pc] >> 8)
			code[pc], code[pc+1] = bytecode.EncodeABx(bytecode.NewObject, a, 0), bytecode.EncodeABC(bytecode.Ret, a, 0, 0)
		}},
	} {
		for _, graph := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, graph %v", c.name, graph), func(t *testing.T) {
				code := fuzzCompile(t, "export let x = 1; await 0; x = 2;", true)
				require.NotNil(t, code)
				require.True(t, code.Async)
				pc := code.Module.BodyPC
				for pc < len(code.Code) && bytecode.DecodeOp(code.Code[pc]) != bytecode.LoadThis {
					pc += 1 + bytecode.DecodeOp(code.Code[pc]).ExtraWords()
				}
				require.Less(t, pc, len(code.Code), "the body starts with LoadThis")
				c.edit(code.Code, pc)
				require.NoError(t, bytecode.VerifyRegisters(code))
				g, err := LinkModules(code, LinkOptions{})
				require.NoError(t, err)
				r := NewRealm()
				require.NotPanics(t, func() {
					if !graph {
						_, err = r.EvaluateModule(code)
						return
					}
					var p *Object
					if _, p, err = r.EvaluateGraph(g); p != nil {
						err = r.ModuleEvaluationError(p, err)
					}
				})
				require.Error(t, err)
				assert.Contains(t, fmt.Sprint(err), "TypeError")
				assert.Contains(t, fmt.Sprint(err), "internal: an async module body returned no promise")
			})
		}
	}
}

// TestForInNextKeysThatAreNoStrings runs a for-in loop whose key list a
// verified function changed: the verifier proves the record's key list
// an object and its index a number, not what the list holds, so
// forInNext skips a key that is no string rather than read it as one.
func TestForInNextKeysThatAreNoStrings(t *testing.T) {
	abc, abx, asbx := bytecode.EncodeABC, bytecode.EncodeABx, bytecode.EncodeAsBx
	loop := func(change uint32) *bytecode.Function {
		return &bytecode.Function{Name: "f", NumRegs: 10, Strict: true, Code: []uint32{
			abx(bytecode.NewObject, 4, 0), abx(bytecode.LoadConst, 7, 0), asbx(bytecode.LoadInt, 8, 1),
			abc(bytecode.DefineElem, 4, 7, 8), // R[4] = {a: 1}
			abc(bytecode.ForInInit, 0, 4, 0),  // the record R[0..3], its key list ["a"]
			asbx(bytecode.LoadInt, 5, 0), abx(bytecode.NewObject, 6, 0),
			change,
			asbx(bytecode.LoadInt, 9, 0),
			asbx(bytecode.ForInNext, 0, 2), // pc 9, done at pc 12
			abc(bytecode.AddImm, 9, 9, 1),
			asbx(bytecode.Jmp, 0, -3),
			abc(bytecode.Ret, 9, 0, 0),
		}, Consts: []bytecode.Const{{Kind: bytecode.ConstString, Str: "a", IsASCII: true, Key: true}}}
	}
	run := func(fn *bytecode.Function) float64 {
		r := NewRealm()
		f, _ := testClosure(r, fn)
		v, err := r.CallObject(f, Undefined(), nil)
		require.NoError(t, err)
		return v.AsNumber()
	}
	assert.Equal(t, 1.0, run(loop(abc(bytecode.Move, 5, 5, 0))), "the loop over [\"a\"] runs once")
	assert.Equal(t, 0.0, run(loop(abc(bytecode.SetElem, 0, 5, 6))), "the loop over [{}] skips its key")
}

// TestSuspendIntoAnotherFrame runs a hand-made function that suspends into
// the generator or async generator another function's call made: its
// resumption would run the other function's code at this one's pc, with
// its registers, so each op ends in the internal TypeError.
func TestSuspendIntoAnotherFrame(t *testing.T) {
	abc := bytecode.EncodeABC
	for _, c := range []struct {
		op        bytecode.Op
		src, want string
	}{
		{bytecode.Yield, `(function* () { yield 1; })()`, "internal: Yield expects the generator its frame runs"},
		{bytecode.AsyncYield, `(async function* () { yield 1; })()`, "internal: AsyncYield expects the async generator its frame runs"},
		{bytecode.Await, `(async function* () { yield 1; })()`, "internal: Await expects the async function or generator its frame runs"},
	} {
		r := NewRealm()
		state, err := r.RunScript(verifyScript(t, c.src))
		require.NoError(t, err)
		// Await takes R[B] as the async generator in an async generator's
		// frame.
		async := c.op == bytecode.Await
		f, _ := testClosure(r, &bytecode.Function{Name: "f", NumParams: 1, NumRegs: 3, Strict: true, Async: async, Generator: async, Code: []uint32{
			abc(bytecode.LoadUndef, 1, 0, 0), abc(c.op, 1, 0, 0), abc(bytecode.RetUndef, 0, 0, 0)}})
		_, err = r.CallObject(f, Undefined(), []Value{state})
		require.Error(t, err, "%s", c.op)
		assert.Contains(t, err.Error(), c.want)
	}
}

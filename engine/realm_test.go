package engine

import (
	"errors"
	"testing"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRealmSkeleton(t *testing.T) {
	r := NewRealm()
	g := r.Global
	get := func(name string) Value {
		v, err := g.GetProp(r, key(r, name))
		require.NoError(t, err)
		return v
	}
	assert.Equal(t, ObjectValue(g), get("globalThis"))
	assert.True(t, get("undefined").IsUndefined())
	assert.True(t, get("NaN").IsNumber())
	assert.Equal(t, posInf, get("Infinity").AsNumber())
	for name, want := range map[string]*Object{
		"Object": r.ObjectCtor, "Function": r.FunctionCtor, "Array": r.ArrayCtor, "String": r.StringCtor,
		"Number": r.NumberCtor, "Boolean": r.BooleanCtor, "RegExp": r.RegExpCtor, "Date": r.DateCtor,
		"Error": r.ErrorConstructorFor(KindError), "TypeError": r.ErrorConstructorFor(KindTypeError),
		"RangeError": r.ErrorConstructorFor(KindRangeError), "SyntaxError": r.ErrorConstructorFor(KindSyntaxError),
		"ReferenceError": r.ErrorConstructorFor(KindReferenceError), "EvalError": r.ErrorConstructorFor(KindEvalError),
		"URIError": r.ErrorConstructorFor(KindURIError), "JSON": r.JSON, "Math": r.Math,
	} {
		assert.Same(t, want, get(name).AsObject(), name)
	}
	// Globals are non-enumerable except none; value globals are non-writable.
	assert.Empty(t, g.OwnEnumerableStringKeys())
	assert.ErrorContains(t, g.SetProp(r, key(r, "undefined"), IntValue(1)), "read only")

	// Prototype links.
	errCtor := r.ErrorConstructorFor(KindError)
	for _, c := range []struct {
		ctor, proto, ctorProto *Object
	}{
		{r.ObjectCtor, r.ObjectPrototype, r.FunctionPrototype}, {r.FunctionCtor, r.FunctionPrototype, r.FunctionPrototype},
		{r.ArrayCtor, r.ArrayPrototype, r.FunctionPrototype}, {r.StringCtor, r.StringPrototype, r.FunctionPrototype},
		{r.NumberCtor, r.NumberPrototype, r.FunctionPrototype}, {r.BooleanCtor, r.BooleanPrototype, r.FunctionPrototype},
		{r.RegExpCtor, r.RegExpPrototype, r.FunctionPrototype}, {r.DateCtor, r.DatePrototype, r.FunctionPrototype},
		{errCtor, r.ErrorPrototype, r.FunctionPrototype},
		{r.ErrorConstructorFor(KindTypeError), r.ErrorPrototypeFor(KindTypeError), errCtor},
		{r.ErrorConstructorFor(KindURIError), r.ErrorPrototypeFor(KindURIError), errCtor},
	} {
		pv, _ := c.ctor.GetProp(r, StringKey(AtomPrototype))
		assert.Same(t, c.proto, pv.AsObject())
		cv, _ := c.proto.GetProp(r, StringKey(AtomConstructor))
		assert.Same(t, c.ctor, cv.AsObject())
		assert.Same(t, c.ctorProto, c.ctor.Proto())
		desc, _ := c.ctor.GetOwnProperty(StringKey(AtomPrototype))
		assert.False(t, desc.Writable())
		assert.False(t, desc.Enumerable())
		assert.False(t, desc.Configurable())
	}
	assert.Same(t, r.ErrorConstructorFor(KindError), r.ErrorConstructorFor(KindTypeError).Proto())
	assert.Same(t, r.ErrorPrototype, r.ErrorPrototypeFor(KindRangeError).Proto())
	assert.Same(t, r.ObjectPrototype, r.FunctionPrototype.Proto())
	assert.Nil(t, r.ObjectPrototype.Proto())
	assert.True(t, r.FunctionPrototype.IsCallable())
	res, err := r.Call(ObjectValue(r.FunctionPrototype), Undefined(), []Value{IntValue(1)})
	require.NoError(t, err)
	assert.True(t, res.IsUndefined())
	assert.Equal(t, ClassArray, r.ArrayPrototype.Class())
	assert.Equal(t, uint32(0), r.ArrayPrototype.ArrayLength())

	// name/length on constructors.
	nv, _ := r.ArrayCtor.GetProp(r, StringKey(AtomName))
	assert.Equal(t, "Array", nv.AsString().GoString())
	lv, _ := r.ArrayCtor.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(1), lv)
	lv, _ = r.DateCtor.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(7), lv)
	nv, _ = r.ErrorPrototypeFor(KindTypeError).GetProp(r, StringKey(AtomName))
	assert.Equal(t, "TypeError", nv.AsString().GoString())
	mv, _ := r.ErrorPrototype.GetProp(r, StringKey(AtomMessage))
	assert.Equal(t, "", mv.AsString().GoString())
	assert.True(t, r.ObjectPrototype.IsPrototypeObject())
	assert.True(t, r.ArrayPrototype.IsPrototypeObject())
	assert.False(t, r.Math.IsPrototypeObject())
	assert.Nil(t, r.boot)
	assert.False(t, r.HasSharedIntrinsics())
	assert.False(t, r.ObjectPrototype.IsShared())
}

// TestBootstrapSlabsFilled checks that building the intrinsics uses up every
// bootstrap slab, so that no mutable realm allocates slab capacity it never
// uses; lower the bootstrap constants when builtins go away. Slot requests
// are contiguous runs, so the slot slab may keep a short tail.
func TestBootstrapSlabsFilled(t *testing.T) {
	r := newBareRealm()
	r.buildIntrinsics()
	b := r.boot
	assert.Equal(t, cap(b.objs), len(b.objs))
	assert.LessOrEqual(t, cap(b.slots)-len(b.slots), 4)
	assert.Equal(t, cap(b.funcs), len(b.funcs))
	assert.Equal(t, cap(b.shapes), len(b.shapes))
	assert.Equal(t, cap(b.trans), len(b.trans))
	assert.Equal(t, uintptr(224), unsafe.Sizeof(nativeFuncObject{}), "bootstrapFuncs assumes the 224-byte class")
	assert.Equal(t, uintptr(120), unsafe.Sizeof(Shape{}), "bootstrapShapes assumes 120-byte shapes")
}

// TestRealmSizeClass pins the realm structs to their size classes: a shared
// realm is the Realm alone (exactly the 288-byte class; state it rarely uses
// belongs in realmLazy), a mutable one the Realm with its Intrinsics
// (640-byte class with the 8-byte malloc header).
func TestRealmSizeClass(t *testing.T) {
	assert.Equal(t, uintptr(288), unsafe.Sizeof(Realm{}))
	// extIntrinsics (with its malloc header) keeps to the 1152-byte class.
	assert.LessOrEqual(t, unsafe.Sizeof(extIntrinsics{})+8, uintptr(1152))
	assert.LessOrEqual(t, unsafe.Sizeof(ownRealm{})+8, uintptr(640))
	assert.Same(t, newShared().Intrinsics, newShared().Intrinsics)
	assert.NotSame(t, NewRealm().Intrinsics, NewRealm().Intrinsics)
	// A shared realm's global has room for a few host globals without
	// spilling into a larger size class (the size-class fill gives more).
	sr := newShared()
	assert.GreaterOrEqual(t, cap(sr.Global.slots)-len(sr.Global.slots), 4)
	hostKeys := []PropertyKey{key(sr, "host_a"), key(sr, "host_b"), key(sr, "host_c"), key(sr, "host_d")}
	addCodeKeys(hostKeys)
	allocs := testing.AllocsPerRun(20, func() {
		r := newShared()
		for i, k := range hostKeys {
			r.Global.DefineOwnDataFast(r, k, IntValue(i), attrDefault)
		}
	})
	assert.Equal(t, testing.AllocsPerRun(20, func() { newShared() }), allocs)
}

func TestConstructorsBasics(t *testing.T) {
	r := NewRealm()
	// Array
	v, err := r.Construct(ObjectValue(r.ArrayCtor), []Value{IntValue(3)}, nil)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), v.AsObject().ArrayLength())
	v, err = r.Call(ObjectValue(r.ArrayCtor), Undefined(), []Value{IntValue(1), IntValue(2)})
	require.NoError(t, err)
	assert.Equal(t, uint32(2), v.AsObject().ArrayLength())
	v, _ = r.Construct(ObjectValue(r.ArrayCtor), []Value{str("3")}, nil)
	assert.Equal(t, uint32(1), v.AsObject().ArrayLength())
	_, err = r.Construct(ObjectValue(r.ArrayCtor), []Value{NumberValue(1.5)}, nil)
	assert.EqualError(t, err, "RangeError: Invalid array length")
	assert.True(t, IsArray(v))
	assert.False(t, IsArray(ObjectValue(r.NewObject())))
	// Object
	v, _ = r.Call(ObjectValue(r.ObjectCtor), Undefined(), nil)
	assert.Equal(t, ClassObject, v.AsObject().Class())
	v, _ = r.Call(ObjectValue(r.ObjectCtor), Undefined(), []Value{IntValue(1)})
	assert.Equal(t, ClassNumber, v.AsObject().Class())
	// String / Number / Boolean as functions and constructors
	v, _ = r.Call(ObjectValue(r.StringCtor), Undefined(), []Value{IntValue(12)})
	assert.Equal(t, "12", v.AsString().GoString())
	v, _ = r.Call(ObjectValue(r.StringCtor), Undefined(), nil)
	assert.Equal(t, "", v.AsString().GoString())
	v, _ = r.Call(ObjectValue(r.StringCtor), Undefined(), []Value{SymbolValue(SymIterator)})
	assert.Equal(t, "Symbol(Symbol.iterator)", v.AsString().GoString())
	v, _ = r.Construct(ObjectValue(r.StringCtor), []Value{str("ab")}, nil)
	assert.Equal(t, ClassString, v.AsObject().Class())
	lv, _ := v.AsObject().GetProp(r, lengthKey)
	assert.Equal(t, IntValue(2), lv)
	v, _ = r.Call(ObjectValue(r.NumberCtor), Undefined(), []Value{str(" 12 ")})
	assert.Equal(t, IntValue(12), v)
	v, _ = r.Call(ObjectValue(r.NumberCtor), Undefined(), nil)
	assert.Equal(t, IntValue(0), v)
	b, _ := NewBigIntFromDecimal("7")
	v, _ = r.Call(ObjectValue(r.NumberCtor), Undefined(), []Value{BigIntValue(b)})
	assert.Equal(t, IntValue(7), v)
	v, _ = r.Construct(ObjectValue(r.NumberCtor), []Value{IntValue(1)}, nil)
	assert.Equal(t, ClassNumber, v.AsObject().Class())
	v, _ = r.Call(ObjectValue(r.BooleanCtor), Undefined(), []Value{str("")})
	assert.Equal(t, False(), v)
	v, _ = r.Construct(ObjectValue(r.BooleanCtor), []Value{IntValue(1)}, nil)
	pv, _ := v.AsObject().PrimitiveValue()
	assert.Equal(t, True(), pv)
	// Function constructor is deferred.
	_, err = r.Construct(ObjectValue(r.FunctionCtor), nil, nil)
	assert.EqualError(t, err, "EvalError: code generation from strings is not available: no compiler is installed (engine.SetCompiler)")
	rx, err := r.Construct(ObjectValue(r.RegExpCtor), nil, nil)
	require.NoError(t, err, "RegExp is completed in place by the builtin installers")
	assert.Equal(t, "RegExp", rx.AsObject().ClassName())
	// Object.prototype.toString tags.
	ts, _ := r.ObjectPrototype.GetProp(r, StringKey(AtomToString))
	for _, c := range []struct {
		this Value
		want string
	}{
		{Undefined(), "[object Undefined]"}, {Null(), "[object Null]"}, {IntValue(1), "[object Number]"},
		{str("s"), "[object String]"}, {True(), "[object Boolean]"}, {ObjectValue(r.NewArray()), "[object Array]"},
		{ObjectValue(r.ObjectCtor), "[object Function]"}, {ObjectValue(r.NewError(KindError, "x")), "[object Error]"},
		{ObjectValue(r.NewObject()), "[object Object]"},
	} {
		res, err := r.Call(ts, c.this, nil)
		require.NoError(t, err)
		assert.Equal(t, c.want, res.AsString().GoString())
	}
	vo, _ := r.ObjectPrototype.GetProp(r, StringKey(AtomValueOf))
	res, _ := r.Call(vo, IntValue(1), nil)
	assert.Equal(t, ClassNumber, res.AsObject().Class())
	_, err = r.Call(vo, Undefined(), nil)
	assert.Error(t, err)
}

func TestErrors(t *testing.T) {
	r := NewRealm()
	e := r.NewError(KindTypeError, "bad %s %d", "thing", 3)
	assert.Equal(t, ClassError, e.Class())
	assert.Same(t, r.ErrorPrototypeFor(KindTypeError), e.Proto())
	mv, _ := e.GetProp(r, StringKey(AtomMessage))
	assert.Equal(t, "bad thing 3", mv.AsString().GoString())
	assert.True(t, e.HasOwnProperty(StringKey(AtomStack)))
	assert.Equal(t, []string{}, keyNames(e.OwnEnumerableStringKeys()), "message and stack are non-enumerable")
	exc := &Exception{Value: ObjectValue(e)}
	assert.Equal(t, "TypeError: bad thing 3", exc.Error())
	assert.Equal(t, exc.Error(), exc.String())
	exc.Stack = "    at f (x.js:1:1)"
	assert.Equal(t, "TypeError: bad thing 3\n    at f (x.js:1:1)", exc.Error())

	err := r.TypeError("plain")
	var ex *Exception
	require.ErrorAs(t, err, &ex)
	assert.Equal(t, "TypeError: plain", ex.Error())
	assert.Equal(t, "RangeError: r", r.RangeError("r").Error())
	assert.Equal(t, "SyntaxError: s", r.SyntaxError("s").Error())
	assert.Equal(t, "ReferenceError: x is not defined", r.ReferenceError("%s is not defined", "x").Error())
	assert.Equal(t, "abc", r.Throw(str("abc")).Error())
	assert.Equal(t, "42", r.Throw(IntValue(42)).Error())
	assert.Equal(t, "[object Object]", r.Throw(ObjectValue(r.NewObject())).Error())
	// Formatting goes through fmt only when args are present.
	assert.Equal(t, "Error: 100%", (&Exception{Value: ObjectValue(r.NewError(KindError, "%d%%", 100))}).Error())

	// Error constructor: message, cause, stack; call without new.
	opts := r.NewObject()
	mustSet(t, r, opts, "cause", IntValue(9))
	v, err := r.Call(ObjectValue(r.ErrorConstructorFor(KindRangeError)), Undefined(), []Value{str("m"), ObjectValue(opts)})
	require.NoError(t, err)
	eo := v.AsObject()
	assert.Same(t, r.ErrorPrototypeFor(KindRangeError), eo.Proto())
	cv, _ := eo.GetProp(r, StringKey(AtomCause))
	assert.Equal(t, IntValue(9), cv)
	assert.True(t, eo.HasOwnProperty(StringKey(AtomStack)))
	ts, _ := eo.GetProp(r, StringKey(AtomToString))
	res, err := r.Call(ts, v, nil)
	require.NoError(t, err)
	assert.Equal(t, "RangeError: m", res.AsString().GoString())
	assert.Equal(t, "RangeError: m", (&Exception{Value: v}).Error())
	// No message argument => no own message, toString yields the name.
	v, _ = r.Construct(ObjectValue(r.ErrorConstructorFor(KindError)), nil, nil)
	assert.False(t, v.AsObject().HasOwnProperty(StringKey(AtomMessage)))
	res, _ = r.Call(ts, v, nil)
	assert.Equal(t, "Error", res.AsString().GoString())
	assert.Equal(t, "Error", (&Exception{Value: v}).Error())
	// Custom name/message on the instance.
	mustSet(t, r, v.AsObject(), "name", str(""))
	mustSet(t, r, v.AsObject(), "message", str("only"))
	res, _ = r.Call(ts, v, nil)
	assert.Equal(t, "only", res.AsString().GoString())
	_, err = r.Call(ts, IntValue(1), nil)
	assert.ErrorContains(t, err, "requires that 'this' be an Object")

	// Without interpreter hooks the stack is just the header.
	sv, _ := e.GetProp(r, StringKey(AtomStack))
	assert.Equal(t, "TypeError: bad thing 3", sv.AsString().GoString())
	assert.Equal(t, "TypeError", KindTypeError.Name().GoString())
}

func TestErrorStackIsLazy(t *testing.T) {
	r := NewRealm()
	captures, formats := 0, 0
	r.SetStackHooks(
		func(r *Realm) []StackFrame {
			captures++
			return []StackFrame{{PC: 7}}
		},
		func(r *Realm, frames []StackFrame) string {
			formats++
			require.Len(t, frames, 1)
			assert.Equal(t, uint32(7), frames[0].PC)
			return "    at stub (plugin.js:1:1)"
		},
	)
	e := r.NewError(KindRangeError, "boom")
	assert.Equal(t, 1, captures, "frames are captured at creation")
	assert.Equal(t, 0, formats, "nothing is formatted until stack is read")
	require.Len(t, e.ErrorData().Frames(), 1)
	desc, ok := e.GetOwnProperty(StringKey(AtomStack))
	require.True(t, ok)
	assert.True(t, desc.IsAccessorDescriptor())
	assert.False(t, desc.Enumerable())
	assert.True(t, desc.Configurable())
	sv, err := e.GetProp(r, StringKey(AtomStack))
	require.NoError(t, err)
	assert.Equal(t, "RangeError: boom\n    at stub (plugin.js:1:1)", sv.AsString().GoString())
	assert.Equal(t, 1, formats)
	sv2, _ := e.GetProp(r, StringKey(AtomStack))
	assert.Same(t, sv.AsString(), sv2.AsString(), "formatted once and cached")
	assert.Equal(t, 1, formats)
	assert.Nil(t, e.ErrorData().Frames(), "frames are released after formatting")
	// Assignment replaces the value; deletion removes the accessor.
	require.NoError(t, e.SetProp(r, StringKey(AtomStack), str("custom")))
	sv, _ = e.GetProp(r, StringKey(AtomStack))
	assert.Equal(t, "custom", sv.AsString().GoString())
	assert.True(t, e.Delete(r, StringKey(AtomStack)))
	assert.False(t, e.HasOwnProperty(StringKey(AtomStack)))
	// Errors created by `new Error` from JS use the same machinery.
	v, err := r.Construct(ObjectValue(r.ErrorConstructorFor(KindError)), []Value{str("m")}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, captures)
	assert.Equal(t, []string{"message", "stack"}, keyNames(v.AsObject().OwnPropertyKeys()))
	sv, _ = v.AsObject().GetProp(r, StringKey(AtomStack))
	assert.Equal(t, "Error: m\n    at stub (plugin.js:1:1)", sv.AsString().GoString())
	// The getter on a non-error `this` yields undefined; the setter throws.
	g, _ := r.Call(ObjectValue(r.errorStackAccessor.Get), ObjectValue(r.NewObject()), nil)
	assert.True(t, g.IsUndefined())
	_, err = r.Call(ObjectValue(r.errorStackAccessor.Set), ObjectValue(r.NewObject()), []Value{str("x")})
	assert.ErrorContains(t, err, "non-error object")
}

func TestInterrupt(t *testing.T) {
	r := NewRealm()
	assert.NoError(t, r.CheckInterrupt())
	assert.False(t, r.Interrupted())
	cause := errors.New("timeout")
	r.Interrupt(cause)
	assert.True(t, r.Interrupted())
	err := r.CheckInterrupt()
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Same(t, cause, ie.Value)
	assert.Equal(t, "interrupted: timeout", ie.Error())
	assert.ErrorIs(t, err, cause, "Unwrap returns an error Value")
	assert.Same(t, cause, ie.Unwrap())
	// Stays pending until cleared.
	assert.Error(t, r.CheckInterrupt())
	r.ClearInterrupt()
	assert.NoError(t, r.CheckInterrupt())
	assert.False(t, r.Interrupted())
	r.Interrupt(nil)
	assert.Equal(t, "interrupted", r.CheckInterrupt().Error())
	r.ClearInterrupt()
	r.Interrupt("stop")
	assert.Equal(t, "interrupted: stop", r.CheckInterrupt().Error())
	r.ClearInterrupt()
	r.Interrupt(42)
	err = r.CheckInterrupt()
	assert.Equal(t, "interrupted: 42", err.Error())
	assert.Nil(t, errors.Unwrap(err), "a Value that is not an error unwraps to nil")
	r.ClearInterrupt()
	r.Interrupt(nil)
	assert.Nil(t, errors.Unwrap(r.CheckInterrupt()))
}

func TestCallDepthLimit(t *testing.T) {
	r := NewRealm()
	depth := 0
	var self *Object
	self = r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		depth++
		return r.Call(ObjectValue(self), Undefined(), nil)
	})
	_, err := r.Call(ObjectValue(self), Undefined(), nil)
	assert.EqualError(t, err, "RangeError: Maximum call stack size exceeded")
	assert.Equal(t, MaxCallDepth, depth)
	assert.Equal(t, 0, r.CallDepth(), "frames are released on unwind")
	// Construct path shares the counter.
	var ctor *Object
	ctor = r.NewNativeConstructor(AtomEmpty, 0, nil, func(r *Realm, args []Value, newTarget *Object) (Value, error) {
		return r.Construct(ObjectValue(ctor), nil, nil)
	})
	_, err = r.Construct(ObjectValue(ctor), nil, nil)
	assert.EqualError(t, err, "RangeError: Maximum call stack size exceeded")
	assert.Equal(t, 0, r.CallDepth())
	// EnterCall/ExitCall for the interpreter.
	for range MaxCallDepth {
		require.NoError(t, r.EnterCall())
	}
	assert.Error(t, r.EnterCall())
	for range MaxCallDepth {
		r.ExitCall()
	}
	assert.Equal(t, 0, r.CallDepth())
}

func TestCallAndConstructDispatch(t *testing.T) {
	r := NewRealm()
	_, err := r.Call(IntValue(1), Undefined(), nil)
	assert.EqualError(t, err, "TypeError: 1 is not a function")
	_, err = r.Call(ObjectValue(r.NewObject()), Undefined(), nil)
	assert.EqualError(t, err, "TypeError: [object Object] is not a function")
	f := r.NewNativeFunction(AtomEmpty, 0, func(*Realm, Value, []Value) (Value, error) { return Undefined(), nil })
	_, err = r.Construct(ObjectValue(f), nil, nil)
	assert.EqualError(t, err, "TypeError: function () { [native code] } is not a constructor")
	assert.False(t, IsConstructor(ObjectValue(f)))
	assert.True(t, IsConstructor(ObjectValue(r.ArrayCtor)))
	assert.True(t, IsCallable(ObjectValue(f)))
	assert.False(t, IsCallable(str("f")))
	// Constructor without call behaviour.
	c := r.NewNativeConstructor(r.InternGoString("Thing"), 2, nil, func(r *Realm, args []Value, newTarget *Object) (Value, error) {
		o, err := r.OrdinaryCreateFromConstructor(newTarget, r.ObjectPrototype, ClassObject)
		if err != nil {
			return Undefined(), err
		}
		o.DefineOwnDataFast(r, key(r, "n"), IntValue(len(args)), attrDefault)
		return ObjectValue(o), nil
	})
	_, err = r.Call(ObjectValue(c), Undefined(), nil)
	assert.EqualError(t, err, "TypeError: Constructor Thing requires 'new'")
	v, err := r.Construct(ObjectValue(c), []Value{IntValue(1), IntValue(2)}, nil)
	require.NoError(t, err)
	nv, _ := v.AsObject().GetProp(r, key(r, "n"))
	assert.Equal(t, IntValue(2), nv)
	lv, _ := c.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(2), lv)
	// newTarget with an object prototype is honoured.
	nt := r.NewNativeFunction(AtomEmpty, 0, nil)
	customProto := r.NewObject()
	nt.DefineOwnDataFast(r, StringKey(AtomPrototype), ObjectValue(customProto), attrHidden)
	v, err = r.Construct(ObjectValue(c), nil, nt)
	require.NoError(t, err)
	assert.Same(t, customProto, v.AsObject().Proto())
	// Bytecode closures run through the interpreter.
	bf, fd := testClosure(r, &bytecodeStubFunction)
	assert.Equal(t, FuncBytecode, fd.Kind())
	v, err = r.Call(ObjectValue(bf), Undefined(), nil)
	require.NoError(t, err)
	assert.True(t, v.IsUndefined())
	v, err = r.Construct(ObjectValue(bf), nil, nil)
	require.NoError(t, err)
	assert.True(t, v.IsObject())
	_, err = r.Call(ObjectValue(bf), Undefined(), nil)
	require.NoError(t, err)
	nv, _ = bf.GetProp(r, StringKey(AtomName))
	assert.Equal(t, "stub", nv.AsString().GoString())
	lv, _ = bf.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(2), lv)
	assert.Equal(t, "function", TypeOf(ObjectValue(bf)).GoString())
	// The kind-specific state shares one field: a method's home object, a
	// native's [[Construct]], a data native's payload never leak across.
	assert.Nil(t, fd.HomeObject())
	home := r.NewObject()
	fd.SetHomeObject(home)
	assert.Same(t, home, fd.HomeObject())
	assert.Nil(t, fd.Data())
	assert.Nil(t, fd.BoundTarget())
	assert.Nil(t, c.FunctionData().HomeObject())
	assert.Nil(t, c.FunctionData().Data())
	c.FunctionData().SetConstructor(nil)
	assert.False(t, IsConstructor(ObjectValue(c)))
	assert.True(t, IsCallable(ObjectValue(c)))
	noCtor := r.NewNativeConstructor(AtomEmpty, 0, nil, nil)
	assert.False(t, IsConstructor(ObjectValue(noCtor)))
}

func TestBoundFunctions(t *testing.T) {
	r := NewRealm()
	var seenThis Value
	var seenArgs []Value
	target := r.NewNativeFunction(r.InternGoString("target"), 3, func(r *Realm, this Value, args []Value) (Value, error) {
		seenThis = this
		seenArgs = append([]Value(nil), args...)
		return IntValue(len(args)), nil
	})
	bound, err := r.NewBoundFunction(target, str("T"), []Value{IntValue(1)})
	require.NoError(t, err)
	assert.Equal(t, FuncBound, bound.FunctionData().Kind())
	assert.Same(t, target, bound.FunctionData().BoundTarget())
	res, err := r.Call(ObjectValue(bound), Undefined(), []Value{IntValue(2), IntValue(3)})
	require.NoError(t, err)
	assert.Equal(t, IntValue(3), res)
	assert.Equal(t, str("T").AsString().GoString(), seenThis.AsString().GoString())
	assert.Equal(t, []Value{IntValue(1), IntValue(2), IntValue(3)}, seenArgs)
	nv, _ := bound.GetProp(r, StringKey(AtomName))
	assert.Equal(t, "bound target", nv.AsString().GoString())
	lv, _ := bound.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(2), lv)
	assert.False(t, IsConstructor(ObjectValue(bound)))
	// Bound constructor constructs the target.
	bctor, err := r.NewBoundFunction(r.ArrayCtor, Undefined(), []Value{IntValue(1)})
	require.NoError(t, err)
	assert.True(t, IsConstructor(ObjectValue(bctor)))
	v, err := r.Construct(ObjectValue(bctor), []Value{IntValue(2)}, nil)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), v.AsObject().ArrayLength())
	assert.Same(t, r.ArrayPrototype, v.AsObject().Proto())
	lv, _ = bctor.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(0), lv)
	_, err = r.NewBoundFunction(r.NewObject(), Undefined(), nil)
	assert.ErrorContains(t, err, "Bind must be called on a function")
	// Infinite target length stays infinite; missing length yields 0.
	inf := r.NewNativeFunction(AtomEmpty, 0, nil)
	var d PropertyDescriptor
	d.SetValue(NumberValue(posInf))
	require.NoError(t, inf.DefinePropertyOrThrow(r, lengthKey, d))
	binf, err := r.NewBoundFunction(inf, Undefined(), []Value{IntValue(1)})
	require.NoError(t, err)
	lv, _ = binf.GetProp(r, lengthKey)
	assert.Equal(t, posInf, lv.AsNumber())
	assert.True(t, inf.Delete(r, lengthKey))
	bnone, err := r.NewBoundFunction(inf, Undefined(), nil)
	require.NoError(t, err)
	lv, _ = bnone.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(0), lv)
}

func TestNativeDataFunctions(t *testing.T) {
	// One adapter, per-function payloads.
	adapter := func(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
		n := fd.Data().(int)
		if n < 0 {
			return Undefined(), r.TypeError("negative %d", n)
		}
		return IntValue(n + len(args)), nil
	}
	r := NewRealm()
	one := r.NewNativeDataFunction(r.InternGoString("one"), 2, adapter, 1)
	ten := r.NewNativeDataFunction(AtomEmpty, 0, adapter, 10)
	assert.Equal(t, FuncNativeData, one.FunctionData().Kind())
	assert.Equal(t, 1, one.FunctionData().Data())
	assert.Nil(t, one.FunctionData().Native())
	// The payload fields fit the function objects' size classes.
	assert.Equal(t, uintptr(240), unsafe.Sizeof(funcObject{}))
	assert.Equal(t, uintptr(224), unsafe.Sizeof(nativeFuncObject{}))
	assert.Equal(t, uintptr(256), unsafe.Sizeof(boundFuncObject{}))
	assert.Nil(t, one.FunctionData().BoundTarget())
	bone, err := r.NewBoundFunction(one, Undefined(), []Value{IntValue(5)})
	require.NoError(t, err)
	assert.Same(t, one, bone.FunctionData().BoundTarget())
	assert.Nil(t, bone.FunctionData().Data(), "a bound function's state is not a data payload")
	bres, err := r.Call(ObjectValue(bone), Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, IntValue(2), bres)
	assertNativeShape(t, r, ObjectValue(one), "one", 2)
	res, err := r.Call(ObjectValue(one), Undefined(), []Value{IntValue(0)})
	require.NoError(t, err)
	assert.Equal(t, IntValue(2), res)
	res, err = r.Call(ObjectValue(ten), Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, IntValue(10), res)
	_, err = r.Call(ObjectValue(r.NewNativeDataFunction(AtomEmpty, 0, adapter, -1)), Undefined(), nil)
	assert.EqualError(t, err, "TypeError: negative -1")
	// Not a constructor; prints as a native; binds like any function.
	assert.True(t, IsCallable(ObjectValue(one)))
	assert.False(t, IsConstructor(ObjectValue(one)))
	_, err = r.Construct(ObjectValue(one), nil, nil)
	assert.EqualError(t, err, "TypeError: function one() { [native code] } is not a constructor")
	bound, err := r.NewBoundFunction(ten, Undefined(), []Value{IntValue(1)})
	require.NoError(t, err)
	res, err = r.Call(ObjectValue(bound), Undefined(), []Value{IntValue(2)})
	require.NoError(t, err)
	assert.Equal(t, IntValue(12), res)
	// The interpreter's call path dispatches it too.
	f := evalModule(t, "export function f(g) { return g(1, 2, 3); }")
	g := f.r.NewNativeDataFunction(AtomEmpty, 0, adapter, 5)
	fn, ok := f.env.GetBindingValue("f")
	require.True(t, ok)
	res, err = f.r.Call(fn, Undefined(), []Value{ObjectValue(g)})
	require.NoError(t, err)
	assert.Equal(t, IntValue(8), res)
}

func TestProtoEpochRules(t *testing.T) {
	r := NewRealm()
	proto := r.NewObject()
	plain := r.NewObject()
	e0 := r.ProtoEpoch()
	// Mutating a non-prototype object never bumps the epoch.
	mustSet(t, r, plain, "a", IntValue(1))
	plain.Delete(r, key(r, "a"))
	assert.Equal(t, e0, r.ProtoEpoch())
	// Becoming a prototype does not bump by itself.
	child := r.NewObjectWithProto(proto)
	assert.True(t, proto.IsPrototypeObject())
	assert.Equal(t, e0, r.ProtoEpoch())
	// Adding a property to a prototype bumps.
	mustSet(t, r, proto, "m", IntValue(1))
	e1 := r.ProtoEpoch()
	assert.Greater(t, e1, e0)
	// Overwriting an existing data value does not bump (ICs cache slots, not values).
	mustSet(t, r, proto, "m", IntValue(2))
	assert.Equal(t, e1, r.ProtoEpoch())
	// Attribute change bumps.
	var ro PropertyDescriptor
	ro.SetWritable(false)
	proto.DefineOwnProperty(r, key(r, "m"), ro)
	e2 := r.ProtoEpoch()
	assert.Greater(t, e2, e1)
	// Delete bumps.
	proto.Delete(r, key(r, "m"))
	e3 := r.ProtoEpoch()
	assert.Greater(t, e3, e2)
	// Prototype change of a prototype object bumps.
	assert.True(t, proto.SetPrototypeOf(r, nil))
	e4 := r.ProtoEpoch()
	assert.Greater(t, e4, e3)
	// Prototype change of a non-prototype object does not bump.
	assert.True(t, child.SetPrototypeOf(r, r.ObjectPrototype))
	assert.Equal(t, e4, r.ProtoEpoch())
	// Index property on a prototype bumps too.
	mustSet(t, r, proto, "0", IntValue(1))
	assert.Greater(t, r.ProtoEpoch(), e4)
	// Intrinsic prototypes count as prototypes.
	e5 := r.ProtoEpoch()
	mustSet(t, r, r.ArrayPrototype, "extra", IntValue(1))
	assert.Greater(t, r.ProtoEpoch(), e5)
}

func TestInterning(t *testing.T) {
	r := NewRealm()
	a := r.Intern(FromGoString("dynamicKey"))
	b := r.Intern(FromGoString("dynamicKey"))
	assert.Same(t, a, b)
	assert.True(t, a.IsInterned())
	assert.Same(t, AtomLength, r.Intern(FromGoString("length")))
	assert.Same(t, AtomLength, r.InternGoString("length"))
	u1 := r.Intern(FromGoString("ключ"))
	u2 := r.Intern(FromGoString("ключ"))
	assert.Same(t, u1, u2)
	s, ok := LookupStaticAtom("prototype")
	assert.True(t, ok)
	assert.Same(t, AtomPrototype, s)
	_, ok = LookupStaticAtom("not-an-atom")
	assert.False(t, ok)
	// Static and dynamic atoms are both process-wide.
	r2 := NewRealm()
	assert.Same(t, r.Intern(FromGoString("push")), r2.Intern(FromGoString("push")))
	assert.Same(t, a, r2.Intern(FromGoString("dynamicKey")))
	assert.Panics(t, func() { StringKey(FromGoString("raw")) })
}

func TestEnvAndModuleEnv(t *testing.T) {
	parent := NewEnv(nil, 1)
	env := NewEnv(parent, 2)
	assert.Same(t, parent, env.Parent())
	assert.Equal(t, 2, env.Len())
	assert.True(t, env.Slot(0).IsUndefined())
	env.SetSlot(1, IntValue(5))
	assert.Equal(t, IntValue(5), env.Slots()[1])
	m := NewModuleEnv(2, &bytecode.Module{Exports: []bytecode.Export{{Name: "decodeRequest", Slot: 0}, {Name: "hooks", Slot: 1}}})
	m.SetSlot(0, IntValue(1))
	v, ok := m.GetBindingValue("decodeRequest")
	assert.True(t, ok)
	assert.Equal(t, IntValue(1), v)
	_, ok = m.GetBindingValue("missing")
	assert.False(t, ok)
	assert.Equal(t, []string{"decodeRequest", "hooks"}, m.ExportNames())
}

func TestInstallersAndICAlloc(t *testing.T) {
	r := NewRealm()
	target := r.NewObject()
	r.installBuiltins(target, []builtinDef{
		{r.InternGoString("f"), func(*Realm, Value, []Value) (Value, error) { return IntValue(1), nil }, 2},
	})
	r.installValues(target, []valueDef{{r.InternGoString("PI"), NumberValue(3.14), 0}})
	r.installGetters(target, []getterDef{
		{name: r.InternGoString("g"), get: func(r *Realm, this Value, args []Value) (Value, error) { return this, nil }},
	})
	assert.Equal(t, []string{"f", "PI", "g"}, keyNames(target.OwnPropertyKeys()))
	assert.Empty(t, target.OwnEnumerableStringKeys())
	fv, _ := target.GetProp(r, key(r, "f"))
	lv, _ := fv.AsObject().GetProp(r, lengthKey)
	assert.Equal(t, IntValue(2), lv)
	gv, _ := target.GetProp(r, key(r, "g"))
	assert.Equal(t, ObjectValue(target), gv)
	desc, _ := target.GetOwnProperty(key(r, "g"))
	gn, _ := desc.GetterObject().GetProp(r, StringKey(AtomName))
	assert.Equal(t, "get g", gn.AsString().GoString())
	// Re-installing replaces in place without duplicating keys.
	r.installBuiltins(target, []builtinDef{
		{r.InternGoString("f"), func(*Realm, Value, []Value) (Value, error) { return IntValue(2), nil }, 0},
	})
	assert.Equal(t, []string{"f", "PI", "g"}, keyNames(target.OwnPropertyKeys()))
	fv, _ = target.GetProp(r, key(r, "f"))
	res, _ := r.Call(fv, Undefined(), nil)
	assert.Equal(t, IntValue(2), res)
	// Stub constructors can be completed in place.
	r.RegExpCtor.FunctionData().SetConstructor(func(r *Realm, args []Value, newTarget *Object) (Value, error) {
		return ObjectValue(r.NewObject()), nil
	})
	_, err := r.Construct(ObjectValue(r.RegExpCtor), nil, nil)
	assert.NoError(t, err)

	allocIC := func(fn *bytecode.Function, count uint32) uint32 {
		base, err := r.AllocIC(fn, count)
		require.NoError(t, err)
		return base
	}
	base := allocIC(&bytecodeStubFunction, 3)
	assert.Equal(t, uint32(0), base)
	assert.Equal(t, base, allocIC(&bytecodeStubFunction, 3))
	assert.Len(t, r.ICSlots(), 3)
	other := bytecodeStubFunction
	assert.Equal(t, uint32(3), allocIC(&other, 1))
}

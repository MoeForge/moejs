package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFunctionPrototypeShape(t *testing.T) {
	r := NewRealm()
	fp := ObjectValue(r.FunctionPrototype)
	for _, c := range []struct {
		name   string
		length int
	}{{"apply", 2}, {"bind", 1}, {"call", 1}, {"toString", 0}} {
		assertNativeShape(t, r, jsGet(t, r, fp, c.name), c.name, c.length)
		assertOwnAttrs(t, r, r.FunctionPrototype, c.name, true, false, true)
	}
	assert.Equal(t, IntValue(0), jsGet(t, r, fp, "length"))
	assert.Equal(t, "", jsString(t, jsGet(t, r, fp, "name")))
	assert.Same(t, r.FunctionCtor, jsGet(t, r, fp, "constructor").AsObject())
	assert.Empty(t, r.FunctionPrototype.OwnEnumerableStringKeys())
	// Every native installed so far carries spec-shaped length/name.
	for _, fn := range []Value{jsGlobal(t, r, "parseInt"), jsGet(t, r, ObjectValue(r.ObjectCtor), "keys"), jsGet(t, r, ObjectValue(r.Math), "max")} {
		o := fn.AsObject()
		assert.Equal(t, []string{"length", "name"}, keyNames(o.OwnPropertyKeys()))
	}
	_, err := r.Call(ObjectValue(r.FunctionCtor), Undefined(), []Value{str("return 1")})
	assert.EqualError(t, err, "EvalError: code generation from strings is not available: no compiler is installed (engine.SetCompiler)")
}

// TestDynamicSourceRopeOverLimit: a rope of more code units than
// MaxDynamicSource is refused by that count before it is converted. It stays
// a rope: counting its UTF-8 bytes first would flatten it, a copy of the
// whole string per call.
func TestDynamicSourceRopeOverLimit(t *testing.T) {
	r := NewRealm()
	for _, unit := range []string{"x", "é"} {
		s := FromGoString(unit)
		for s.Len() < 1<<24 {
			s = ropeOf(s, s)
		}
		for _, c := range []struct {
			name string
			fn   Value
			args []Value
			want int
		}{
			{"eval", jsGlobal(t, r, "eval"), []Value{StringValue(s)}, 1 << 24},
			{"Function", ObjectValue(r.FunctionCtor), []Value{str("a"), StringValue(s)}, 1<<24 + 1},
		} {
			t.Run(unit+"/"+c.name, func(t *testing.T) {
				_, err := r.Call(c.fn, Undefined(), c.args)
				assert.EqualError(t, err, fmt.Sprintf("RangeError: Source text of %d code units is longer than the %d bytes this realm compiles at run time (MaxDynamicSource)", c.want, DefaultMaxDynamicSource))
				assert.Equal(t, strRope, s.kind, "not flattened")
				assert.NotNil(t, s.p)
			})
		}
	}
}

// recorder returns a native that records this and args and returns marker.
func recorder(r *Realm, marker Value, seenThis *Value, seenArgs *[]Value) Value {
	return nativeFn(r, func(this Value, args []Value) (Value, error) {
		*seenThis = this
		*seenArgs = append([]Value(nil), args...)
		return marker, nil
	})
}

func TestFunctionCallAndApply(t *testing.T) {
	r := NewRealm()
	var seenThis Value
	var seenArgs []Value
	f := recorder(r, IntValue(42), &seenThis, &seenArgs)

	res := jsCall(t, r, f, "call", str("T"), IntValue(1), IntValue(2))
	assert.Equal(t, IntValue(42), res)
	assert.Equal(t, "T", jsString(t, seenThis))
	assert.Equal(t, []Value{IntValue(1), IntValue(2)}, seenArgs)
	jsCall(t, r, f, "call")
	assert.True(t, seenThis.IsUndefined())
	assert.Empty(t, seenArgs)
	jsCall(t, r, f, "call", Null())
	assert.True(t, seenThis.IsNull(), "this is passed through untouched (strict callee)")

	res = jsCall(t, r, f, "apply", IntValue(7), jsArray(r, str("a"), str("b")))
	assert.Equal(t, IntValue(42), res)
	assert.Equal(t, IntValue(7), seenThis)
	assert.Len(t, seenArgs, 2)
	assert.Equal(t, "b", jsString(t, seenArgs[1]))
	jsCall(t, r, f, "apply", Undefined(), Null())
	assert.Empty(t, seenArgs)
	jsCall(t, r, f, "apply", Undefined())
	assert.Empty(t, seenArgs)
	// Array-likes and holes.
	al := r.NewObject()
	mustSet(t, r, al, "length", IntValue(3))
	mustSet(t, r, al, "0", IntValue(9))
	mustSet(t, r, al, "2", IntValue(8))
	jsCall(t, r, f, "apply", Undefined(), ObjectValue(al))
	assert.Equal(t, []Value{IntValue(9), Undefined(), IntValue(8)}, seenArgs)
	holes := r.NewArrayLen(2)
	jsCall(t, r, f, "apply", Undefined(), ObjectValue(holes))
	assert.Equal(t, []Value{Undefined(), Undefined()}, seenArgs)
	assert.ErrorContains(t, jsCallErr(t, r, f, "apply", Undefined(), IntValue(1)), "CreateListFromArrayLike called on non-object")
	// Errors thrown by the callee propagate.
	thrower := throwingFn(r, "boom")
	assert.EqualError(t, jsCallErr(t, r, thrower, "call"), "TypeError: boom")
	assert.EqualError(t, jsCallErr(t, r, thrower, "apply", Undefined(), jsArray(r)), "TypeError: boom")
	// Non-callable receivers.
	callFn := jsGet(t, r, ObjectValue(r.FunctionPrototype), "call")
	_, err := r.Call(callFn, ObjectValue(r.NewObject()), nil)
	assert.EqualError(t, err, "TypeError: Function.prototype.call was called on [object Object], which is not a function")
	applyFn := jsGet(t, r, ObjectValue(r.FunctionPrototype), "apply")
	_, err = r.Call(applyFn, IntValue(1), nil)
	assert.EqualError(t, err, "TypeError: Function.prototype.apply was called on 1, which is not a function")
	// Constructors called through call/apply behave like plain calls.
	v := jsCall(t, r, ObjectValue(r.ArrayCtor), "call", Undefined(), IntValue(2))
	assert.Equal(t, uint32(2), v.AsObject().ArrayLength())
	v = jsCall(t, r, ObjectValue(r.ArrayCtor), "apply", Undefined(), jsArray(r, IntValue(1), IntValue(2), IntValue(3)))
	assert.Equal(t, uint32(3), v.AsObject().ArrayLength())
}

func TestFunctionBind(t *testing.T) {
	r := NewRealm()
	var seenThis Value
	var seenArgs []Value
	target := r.NewNativeFunction(r.InternGoString("target"), 3, func(r *Realm, this Value, args []Value) (Value, error) {
		seenThis = this
		seenArgs = append([]Value(nil), args...)
		return IntValue(len(args)), nil
	})
	bound := jsCall(t, r, ObjectValue(target), "bind", str("T"), IntValue(1))
	require.True(t, IsCallable(bound))
	bo := bound.AsObject()
	assert.Equal(t, FuncBound, bo.FunctionData().Kind())
	assert.Same(t, target, bo.FunctionData().BoundTarget())
	assert.Equal(t, "bound target", jsString(t, jsGet(t, r, bound, "name")))
	assert.Equal(t, IntValue(2), jsGet(t, r, bound, "length"))
	assertOwnAttrs(t, r, bo, "name", false, false, true)
	assertOwnAttrs(t, r, bo, "length", false, false, true)
	assert.False(t, bo.HasOwnProperty(StringKey(AtomPrototype)))
	res, err := r.Call(bound, IntValue(99), []Value{IntValue(2), IntValue(3)})
	require.NoError(t, err)
	assert.Equal(t, IntValue(3), res)
	assert.Equal(t, "T", jsString(t, seenThis), "bound this wins over the call this")
	assert.Equal(t, []Value{IntValue(1), IntValue(2), IntValue(3)}, seenArgs)
	// Binding a bound function composes.
	bound2 := jsCall(t, r, bound, "bind", Undefined(), IntValue(10))
	assert.Equal(t, "bound bound target", jsString(t, jsGet(t, r, bound2, "name")))
	assert.Equal(t, IntValue(1), jsGet(t, r, bound2, "length"))
	_, err = r.Call(bound2, Undefined(), []Value{IntValue(20)})
	require.NoError(t, err)
	assert.Equal(t, []Value{IntValue(1), IntValue(10), IntValue(20)}, seenArgs)
	assert.Equal(t, "T", jsString(t, seenThis))
	// bind with no arguments.
	plain := jsCall(t, r, ObjectValue(target), "bind")
	assert.Equal(t, IntValue(3), jsGet(t, r, plain, "length"))
	_, err = r.Call(plain, Undefined(), nil)
	require.NoError(t, err)
	assert.True(t, seenThis.IsUndefined())
	// Length never goes negative; non-string names become "bound ".
	over := jsCall(t, r, ObjectValue(target), "bind", Undefined(), IntValue(1), IntValue(2), IntValue(3), IntValue(4))
	assert.Equal(t, IntValue(0), jsGet(t, r, over, "length"))
	var nameDesc PropertyDescriptor
	nameDesc.SetValue(IntValue(5))
	require.NoError(t, target.DefinePropertyOrThrow(r, StringKey(AtomName), nameDesc))
	renamed := jsCall(t, r, ObjectValue(target), "bind")
	assert.Equal(t, "bound ", jsString(t, jsGet(t, r, renamed, "name")))
	// new on a bound constructor constructs the target with the bound args
	// prepended and ignores the bound this.
	bctor := jsCall(t, r, ObjectValue(r.ArrayCtor), "bind", str("ignored"), IntValue(1))
	assert.True(t, IsConstructor(bctor))
	v, err := r.Construct(bctor, []Value{IntValue(2), IntValue(3)}, nil)
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, r.ToGo(v))
	assert.Same(t, r.ArrayPrototype, v.AsObject().Proto())
	ok, err := r.InstanceOf(v, bctor)
	require.NoError(t, err)
	assert.True(t, ok, "instanceof unwraps bound functions")
	assert.False(t, IsConstructor(bound), "bound non-constructors stay non-constructors")
	_, err = r.Construct(bound, nil, nil)
	assert.ErrorContains(t, err, "is not a constructor")
	// Non-callable receivers.
	bindFn := jsGet(t, r, ObjectValue(r.FunctionPrototype), "bind")
	_, err = r.Call(bindFn, ObjectValue(r.NewObject()), nil)
	assert.EqualError(t, err, "TypeError: Bind must be called on a function")
	// The bound function's prototype follows the target's.
	custom := r.NewObjectWithProto(r.FunctionPrototype)
	require.True(t, target.SetPrototypeOf(r, custom))
	b3 := jsCall(t, r, ObjectValue(target), "bind")
	assert.Same(t, custom, b3.AsObject().Proto())
}

func TestFunctionToString(t *testing.T) {
	r := NewRealm()
	assert.Equal(t, "function parseInt() { [native code] }", jsString(t, jsCall(t, r, jsGlobal(t, r, "parseInt"), "toString")))
	assert.Equal(t, "function Object() { [native code] }", jsString(t, jsCall(t, r, ObjectValue(r.ObjectCtor), "toString")))
	assert.Equal(t, "function () { [native code] }", jsString(t, jsCall(t, r, ObjectValue(r.FunctionPrototype), "toString")))
	anon := nativeFn(r, func(Value, []Value) (Value, error) { return Undefined(), nil })
	assert.Equal(t, "function () { [native code] }", jsString(t, jsCall(t, r, anon, "toString")))
	bound := jsCall(t, r, ObjectValue(r.ObjectCtor), "bind")
	assert.Equal(t, "function () { [native code] }", jsString(t, jsCall(t, r, bound, "toString")))
	getter := ObjectValue(r.errorStackAccessor.Get)
	assert.Equal(t, "function get stack() { [native code] }", jsString(t, jsCall(t, r, getter, "toString")))
	// A native name no PropertyName matches prints as a computed one, after
	// a "get " or "set " (nativeName).
	for name, want := range map[string]string{
		"get [Symbol.species]": "get [Symbol.species]",
		"set $_":               "set $_",
		"get ä1":               "get ä1",
		"get ":                 "get ",
		"get $&":               `get ["$&"]`,
		"set $'":               `set ["$'"]`,
		"$+":                   `["$+"]`,
		"get  x":               `get [" x"]`,
		"getter x":             `["getter x"]`,
		"a\"b\\\n\u2028":       `["a\"b\\\n` + "\u2028\"]",
		"1":                    `["1"]`,
	} {
		fn := ObjectValue(r.NewNativeFunction(FromGoString(name), 0, nil))
		assert.Equal(t, "function "+want+"() { [native code] }", jsString(t, jsCall(t, r, fn, "toString")), name)
	}
	lone := ObjectValue(r.NewNativeFunction(FromUTF16([]uint16{'g', 'e', 't', ' ', 0xD800}), 0, nil))
	assert.Equal(t, `function get ["\ud800"]() { [native code] }`, jsString(t, jsCall(t, r, lone, "toString")))
	// Bytecode functions print their source slice when the compiler recorded one.
	src := "const f = function f(a) { return a; };"
	withSource := bytecode.Function{Name: "f", Length: 1, Kind: bytecode.KindNormal, Source: &bytecode.SourceInfo{Name: "m.js", Src: src, Start: 10, End: 37}}
	bf, _ := testClosure(r, &withSource)
	assert.Equal(t, "function f(a) { return a; }", jsString(t, jsCall(t, r, ObjectValue(bf), "toString")))
	noSource, _ := testClosure(r, &bytecodeStubFunction)
	assert.Equal(t, "function stub() { [native code] }", jsString(t, jsCall(t, r, ObjectValue(noSource), "toString")))
	badRange := bytecode.Function{Name: "g", Source: &bytecode.SourceInfo{Src: "x", Start: 5, End: 2}}
	bg, _ := testClosure(r, &badRange)
	assert.Equal(t, "function g() { [native code] }", jsString(t, jsCall(t, r, ObjectValue(bg), "toString")))
	// Non-functions.
	ts := jsGet(t, r, ObjectValue(r.FunctionPrototype), "toString")
	for _, bad := range []Value{ObjectValue(r.NewObject()), IntValue(1), Undefined()} {
		_, err := r.Call(ts, bad, nil)
		assert.EqualError(t, err, "TypeError: Function.prototype.toString requires that 'this' be a Function")
	}
	// ToString of a function value goes through it.
	s, err := r.ToString(jsGlobal(t, r, "isNaN"))
	require.NoError(t, err)
	assert.Equal(t, "function isNaN() { [native code] }", s.GoString())
}

// TestFunctionToStringSourceText checks the source slice of every function
// form against V8 (Node 26 prints the same text for each).
func TestFunctionToStringSourceText(t *testing.T) {
	forms := []string{
		`function decl(a, b) { return a + b; }`,
		`function (x) { return x; }`,
		`function inner(x) { return x; }`,
		`(a, b) => a + b`,
		`x => x * 2`,
		`() => {}`,
		`function /* c1 */ name /* c2 */ ( /* c3 */ ) /* c4 */ { /* c5 */ }`,
		`function (a = 1, { b, c } = {}, ...rest) {}`,
		`function (a = function () {}, b = () => {}) {}`,
		"function crlf(a,\r\n b) {\r\n  return a;\r\n}",
		"function\ttabs\t(\t)\t{\t}",
		"function t() { return `${1 + 1}}`; }",
		`function r() { return /}/.test("}"); }`,
		`function 函数() { return "é😀"; }`,
	}
	var src strings.Builder
	for i, f := range forms {
		fmt.Fprintf(&src, "export const f%d = %s;\n", i, f)
	}
	// Method forms: the slice starts at the key (or get/set) and includes a
	// computed key's brackets; values of plain properties are their own
	// function source.
	src.WriteString(`const obj = {
  m(a) { return a; },
  get g() { return 1; },
  set s(v) {},
  ["comp" + "uted"](x) { return x; },
  'str key'() {},
  42() {},
  prop: function () {},
  arrowProp: () => 1,
};
const acc = (k, which) => Object.getOwnPropertyDescriptor(obj, k)[which];
export const methods = [obj.m, acc("g", "get"), acc("s", "set"), obj.computed, obj["str key"], obj[42],
  ({ get() { return "get"; } }).get, Object.getOwnPropertyDescriptor({ get get() { return 1; } }, "get").get, obj.prop, obj.arrowProp, (function outer() { return function innerMost() { return 1; }; })(),
  ((a) => (b) => a + b)(1)].map(f => Function.prototype.toString.call(f));
export function thrown() { throw function thrownFn() { return 1; }; }
`)
	// Class forms: a class is its whole declaration, a member (static or
	// not, private or not) starts at its key or get/set as for methods, and
	// a field's value is its own function source.
	src.WriteString(`class /* a */ Decl /* b */ extends /* c */ Object /* d */ { /* e */ constructor(x) { super(); this.x = x; } }
const Expr = class { m() {} };
const NamedExpr = class Inner extends Decl {};
class Members {
  static s(a) { return a; }
  get g() { return 1; }
  set g(v) {}
  static get sg() { return 2; }
  #p() { return 3; }
  static #sp() {}
  ["comp" + "uted"](x) { return x; }
  'str key'() {}
  42() {}
  0x10() {}
  static async() {}
  get() {}
  static static() {}
  field = function () {};
  arrowField = () => 1;
  static sfield = function named() {};
  p() { return this.#p; }
  static sp() { return this.#sp; }
}
class Base { constructor() {} }
class Derived extends Base {}
class   Spaced   extends   Base   {   }
const M = Members.prototype, inst = new Members();
const d = (o, k, w) => Object.getOwnPropertyDescriptor(o, k)[w];
export const classes = [Decl, Expr, NamedExpr, Members.s, d(M, "g", "get"), d(M, "g", "set"), d(Members, "sg", "get"),
  inst.p(), Members.sp(), M.computed, M["str key"], M[42], M[16], Members.async, M.get, Members.static,
  inst.field, inst.arrowField, Members.sfield, Base, Derived, Spaced, class {},
  (class { static m() { return class Nested {}; } }).m()].map(f => Function.prototype.toString.call(f));
`)
	f := evalModule(t, src.String())
	for i, want := range forms {
		fn, ok := f.env.GetBindingValue(fmt.Sprintf("f%d", i))
		require.True(t, ok)
		got, err := f.r.ToString(fn)
		require.NoError(t, err)
		assert.Equal(t, want, got.GoString())
	}
	assert.Equal(t, []any{
		`m(a) { return a; }`,
		`get g() { return 1; }`,
		`set s(v) {}`,
		`["comp" + "uted"](x) { return x; }`,
		`'str key'() {}`,
		`42() {}`,
		`get() { return "get"; }`,
		`get get() { return 1; }`,
		`function () {}`,
		`() => 1`,
		`function innerMost() { return 1; }`,
		`(b) => a + b`,
	}, f.export("methods"))
	assert.Equal(t, []any{
		`class /* a */ Decl /* b */ extends /* c */ Object /* d */ { /* e */ constructor(x) { super(); this.x = x; } }`,
		`class { m() {} }`,
		`class Inner extends Decl {}`,
		`s(a) { return a; }`,
		`get g() { return 1; }`,
		`set g(v) {}`,
		`get sg() { return 2; }`,
		`#p() { return 3; }`,
		`#sp() {}`,
		`["comp" + "uted"](x) { return x; }`,
		`'str key'() {}`,
		`42() {}`,
		`0x10() {}`,
		`async() {}`,
		`get() {}`,
		`static() {}`,
		`function () {}`,
		`() => 1`,
		`function named() {}`,
		`class Base { constructor() {} }`,
		`class Derived extends Base {}`,
		`class   Spaced   extends   Base   {   }`,
		`class {}`,
		`class Nested {}`,
	}, f.export("classes"))
	// A thrown function displays as its source.
	_, err := f.callErr("thrown")
	assert.Equal(t, "function thrownFn() { return 1; }", errMessage(t, err))
}

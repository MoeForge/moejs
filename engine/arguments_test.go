package engine

import (
	"testing"
	"time"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArgumentsObject checks CreateUnmappedArgumentsObject: indexed data
// properties, a non-enumerable length, @@iterator = %Array.prototype.values%,
// the %ThrowTypeError% callee accessor, arrow capture, and independence from
// the parameters (every function is strict).
func TestArgumentsObject(t *testing.T) {
	f := evalModule(t, `
const J = JSON.stringify;
function desc(o, k) { const d = Object.getOwnPropertyDescriptor(o, k); return d && [d.writable, d.enumerable, d.configurable]; }
export function basic(a, b) { return J([arguments.length, arguments[0], arguments[2], typeof arguments, Object.prototype.toString.call(arguments)]); }
export function props() { return J([Object.getOwnPropertyNames(arguments), Object.keys(arguments), desc(arguments, "length"), desc(arguments, 0), Object.getPrototypeOf(arguments) === Object.prototype, Array.isArray(arguments)]); }
export function callee() {
	const d = Object.getOwnPropertyDescriptor(arguments, "callee");
	const c = Object.getOwnPropertyDescriptor(Function.prototype, "caller");
	let msg;
	try { arguments.callee; } catch (e) { msg = e instanceof TypeError; }
	return J([d.get === d.set, d.get === c.get, d.enumerable, d.configurable, msg]);
}
export function unmapped(a) { a = 2; arguments[0] = 3; return J([a, arguments[0]]); }
export function arrow() { const g = () => arguments; return g()[1]; }
export function nested() { return (function () { return (() => arguments[0])(); })("inner"); }
function withDefault(a, g = () => arguments) { return g().length; }
export function defaults() { return withDefault(1, undefined, 3); }
export function rest(a, ...r) { return J([r, arguments.length, arguments[2]]); }
export function spread() { return J([...arguments]); }
export function forOf() { let s = 0; for (const x of arguments) s += x; return s; }
export function slice() { return Array.prototype.slice.call(arguments).join(); }
export function mutate() { arguments.length = 1; delete arguments[1]; arguments.x = 1; return J([arguments.length, 1 in arguments, Object.keys(arguments)]); }
export function frozen() { Object.freeze(arguments); try { arguments[0] = 9; return "no"; } catch (e) { return e.name + arguments[0]; } }
export function iterate(a) { let s = 0; for (const x of a) s += x; return s + "|" + [...a].join(); }
export function method() { const o = { m() { return arguments.length; }, get g() { return arguments.length; } }; return J([o.m(1, 2), o.g]); }
export function fresh() { return arguments; }
export function distinct() { return fresh() !== fresh() && arguments !== fresh(); }
export function topLevel() { return typeof globalThis.arguments; }
class A { m() { return () => arguments[0]; } }
class B extends A { m() { return super.m(7)(); } }
export function classArrow() { return J([new A().m(9)(), new B().m()]); }
`)
	assert.Equal(t, `[3,1,3,"object","[object Arguments]"]`, f.call("basic", 1, 2, 3))
	assert.Equal(t, `[["0","1","length","callee"],["0","1"],[true,false,true],[true,true,true],true,false]`, f.call("props", "a", "b"))
	assert.Equal(t, `[true,true,false,false,true]`, f.call("callee"))
	assert.Equal(t, `[2,3]`, f.call("unmapped", 1))
	assert.Equal(t, "y", f.call("arrow", "x", "y"))
	assert.Equal(t, "inner", f.call("nested", "outer"))
	assert.Equal(t, int64(3), f.call("defaults"))
	assert.Equal(t, `[[2,3],3,3]`, f.call("rest", 1, 2, 3))
	assert.Equal(t, `[1,"a",null]`, f.call("spread", 1, "a", nil))
	assert.Equal(t, int64(15), f.call("forOf", 1, 2, 3, 4, 5))
	assert.Equal(t, "1,2,3,4,5,6,7", f.call("slice", 1, 2, 3, 4, 5, 6, 7))
	assert.Equal(t, `[1,false,["0","x"]]`, f.call("mutate", 1, 2))
	assert.Equal(t, "TypeError1", f.call("frozen", 1))
	assert.Equal(t, `[2,0]`, f.call("method"))
	assert.Equal(t, "undefined", f.call("topLevel"))
	assert.Equal(t, `[9,7]`, f.call("classArrow"), "an arrow in a method sees the method's arguments")

	assert.Equal(t, true, f.call("distinct"))

	// @@iterator is %Array.prototype.values% (writable, configurable, not
	// enumerable); once replaced the object is no longer iterated as an
	// array-like.
	r := f.r
	fresh, _ := f.env.GetBindingValue("fresh")
	iterate, _ := f.env.GetBindingValue("iterate")
	av, err := r.Call(fresh, Undefined(), []Value{IntValue(1), IntValue(2)})
	require.NoError(t, err)
	d, ok := av.AsObject().GetOwnProperty(SymbolKey(SymIterator))
	require.True(t, ok)
	values, _ := r.ArrayPrototype.GetOwnProperty(StringKey(AtomValues))
	assert.Same(t, values.Value.AsObject(), d.Value.AsObject())
	assert.Equal(t, [3]bool{true, false, true}, [3]bool{d.Writable(), d.Enumerable(), d.Configurable()})
	v, err := r.Call(iterate, Undefined(), []Value{av})
	require.NoError(t, err)
	assert.Equal(t, "3|1,2", v.String())
	require.NoError(t, av.AsObject().SetProp(r, SymbolKey(SymIterator), Undefined()))
	_, err = r.Call(iterate, Undefined(), []Value{av})
	assert.Contains(t, errMessage(t, err), "TypeError")

	m, err := syntax.ParseModule("t.js", `function f() { return arguments; } arguments;`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	_, err = NewRealm().EvaluateModule(code)
	assert.Equal(t, "ReferenceError: arguments is not defined", errMessage(t, err))
}

// TestArgumentsAllocation checks that the arguments object is co-allocated
// with its named slots and up to four elements, and that functions that never
// reference `arguments` do not create one.
func TestArgumentsAllocation(t *testing.T) {
	skipInRegcheck(t)
	allocs, _, _ := callAllocs(t, `export function f(a, b) { return arguments.length + a + b; }`, IntValue(1), IntValue(2))
	assert.Equal(t, 1.0, allocs, "arguments object with inline elements")
	allocs, _, _ = callAllocs(t, `export function f() { return arguments.length; }`, IntValue(1), IntValue(2), IntValue(3), IntValue(4), IntValue(5))
	assert.Equal(t, 2.0, allocs, "arguments object plus an element array beyond four")
	allocs, _, _ = callAllocs(t, `export function f(a, b) { return a + b; }`, IntValue(1), IntValue(2))
	assert.Equal(t, 0.0, allocs, "no arguments object without a reference")
}

// TestArgumentsRealmCost checks that finding %Array.prototype.values% at
// realm creation does not build Array.prototype's lookup table, which cost
// every mutable realm five allocations.
func TestArgumentsRealmCost(t *testing.T) {
	r := NewRealm()
	assert.Nil(t, r.ArrayPrototype.shape.table)
	values, _ := r.ArrayPrototype.GetOwnProperty(StringKey(AtomValues))
	assert.Same(t, values.Value.AsObject(), r.arrayValuesFn)
}

// TestArgumentsHugeLengthInterruptible checks that spreading an arguments
// object whose length was set very large honours an interrupt.
func TestArgumentsHugeLengthInterruptible(t *testing.T) {
	f := evalModule(t, `export function f() { arguments.length = 1e9; return [...arguments].length; }
export function g(...r) { arguments.length = 1e9; const [a, ...rest] = arguments; return rest.length; }`)
	for _, name := range []string{"f", "g"} {
		go func() { time.Sleep(20 * time.Millisecond); f.r.Interrupt("stop") }()
		_, err := f.callErr(name)
		var ie *InterruptedError
		require.ErrorAs(t, err, &ie, name)
		f.r.ClearInterrupt()
	}
}

// TestArgumentsSharedIntrinsics checks arguments objects in realms that share
// the frozen intrinsics: %ThrowTypeError% and %Array.prototype.values% are
// the shared ones and the shape is per realm-safe.
func TestArgumentsSharedIntrinsics(t *testing.T) {
	m, err := syntax.ParseModule("t.js", `export function f() {
	const d = Object.getOwnPropertyDescriptor(arguments, "callee");
	return [d.get === Object.getOwnPropertyDescriptor(Function.prototype, "caller").get, [...arguments].join(), Object.getOwnPropertyNames(arguments).join()].join("|");
}`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	for range 2 {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
		env, err := r.EvaluateModule(code)
		require.NoError(t, err)
		fn, _ := env.GetBindingValue("f")
		v, err := r.Call(fn, Undefined(), []Value{IntValue(1), IntValue(2)})
		require.NoError(t, err)
		assert.Equal(t, "true|1,2|0,1,length,callee", v.String())
	}
}

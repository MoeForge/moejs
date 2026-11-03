package engine

import (
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callAllocs compiles a module exporting f and returns the allocations of
// one call after warm-up, together with the realm and function.
func callAllocs(t *testing.T, src string, args ...Value) (float64, *Realm, Value) {
	t.Helper()
	m, err := syntax.ParseModule("a.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	fn, ok := env.GetBindingValue("f")
	require.True(t, ok)
	_, err = r.Call(fn, Undefined(), args)
	require.NoError(t, err)
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := r.Call(fn, Undefined(), args); err != nil {
			t.Fatal(err)
		}
	})
	return allocs, r, fn
}

// TestLiteralAllocations guards the co-allocated small literals: a short
// array or object literal, Object.keys of a small object and map over a
// small array are each one allocation, and the values are right.
func TestLiteralAllocations(t *testing.T) {
	skipInRegcheck(t)
	allocs, r, fn := callAllocs(t, `export function f(x) { return ["input_text", "text"].includes(x); }`, str("text"))
	assert.Equal(t, 1.0, allocs, "two-element literal")
	v, err := r.Call(fn, Undefined(), []Value{str("text")})
	require.NoError(t, err)
	assert.True(t, v.IsTrue())

	allocs, r, fn = callAllocs(t, `export function f(a, b) { return {kind: "submit", model: a, action: b, n: 1, ok: true}; }`, str("m"), str("act"))
	assert.Equal(t, 1.0, allocs, "five-field object literal")
	v, err = r.Call(fn, Undefined(), []Value{str("m"), str("act")})
	require.NoError(t, err)
	assert.Equal(t, `{"kind":"submit","model":"m","action":"act","n":1,"ok":true}`, jsonOf(t, r, v))

	allocs, r, fn = callAllocs(t, `export function f() { const o = {}; o.a = 1; o.b = 2; o.c = 3; o.d = 4; return o; }`)
	assert.Equal(t, 1.0, allocs, "empty literal grown to four properties")
	v, err = r.Call(fn, Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, `{"a":1,"b":2,"c":3,"d":4}`, jsonOf(t, r, v))

	allocs, r, fn = callAllocs(t, `export function f() { const a = []; a.push(1); a.push(2); a.push(3); return a; }`)
	assert.Equal(t, 1.0, allocs, "empty array literal pushed three times")
	v, err = r.Call(fn, Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, `[1,2,3]`, jsonOf(t, r, v))

	allocs, r, fn = callAllocs(t, `export function f(o) { return Object.keys(o); }`, ObjectValue(objWith(NewRealm(), "a", "b", "c")))
	assert.Equal(t, 1.0, allocs, "Object.keys of three properties")

	_, r, fn = callAllocs(t, `export function f(a) { return a.map(function (u) { return u + 1; }); }`, ObjectValue(NewRealm().NewArray(IntValue(1), IntValue(2), IntValue(3), IntValue(4), IntValue(5))))
	v, err = r.Call(fn, Undefined(), []Value{ObjectValue(r.NewArray(IntValue(1), IntValue(2), IntValue(3), IntValue(4), IntValue(5)))})
	require.NoError(t, err)
	assert.Equal(t, `[2,3,4,5,6]`, jsonOf(t, r, v))

	// A literal that outgrows its inline storage keeps every element.
	_, r, fn = callAllocs(t, `export function f() { const a = [0, 1]; for (let i = 2; i < 40; i++) a.push(i); const o = {}; for (let i = 0; i < 40; i++) o["k" + i] = i; return [a.length, a[39], Object.keys(o).length, o.k39]; }`)
	v, err = r.Call(fn, Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, `[40,39,40,39]`, jsonOf(t, r, v))
}

func objWith(r *Realm, names ...string) *Object {
	o := r.NewObject()
	for i, n := range names {
		o.DefineOwnDataFast(r, r.KeyFromGoString(n), IntValue(i), attrDefault)
	}
	return o
}

func jsonOf(t *testing.T, r *Realm, v Value) string {
	t.Helper()
	s, err := r.JSONStringify(v)
	require.NoError(t, err)
	return s.GoString()
}

// TestRegExpLiteralPerEvaluation checks that a regular expression literal
// yields a fresh object on every evaluation with the literal's pattern and
// canonical flags, and costs only that object.
func TestRegExpLiteralPerEvaluation(t *testing.T) {
	skipInRegcheck(t)
	allocs, _, _ := callAllocs(t, `export function f() { const a = /\s+/g, b = /[A-Z]/i; return a !== b; }`)
	assert.Equal(t, 2.0, allocs, "two literals, one object each")
	_, r, fn := callAllocs(t, `export function f(s) { const a = /\s+/g, b = /[A-Z]/i; return [a !== f.last, a.flags, b.flags, a.source, s.replace(a, " "), b.test(s), (f.last = a) === a]; }`, str("x  Y"))
	v, err := r.Call(fn, Undefined(), []Value{str("x  Y")})
	require.NoError(t, err)
	assert.Equal(t, `[true,"g","i","\\s+","x Y",true,true]`, jsonOf(t, r, v))
}

// TestLiteralShapesStayTableFree checks that building an object literal does
// not materialize lookup tables on the intermediate shapes (they are probed
// by a chain walk); only shapes that are looked up afterwards build one.
func TestLiteralShapesStayTableFree(t *testing.T) {
	_, r, fn := callAllocs(t, `export function f() { return {a:1,b:2,c:3,d:4,e:5,f:6,g:7,h:8,i:9,j:10,k:11,l:12}; }`)
	v, err := r.Call(fn, Undefined(), nil)
	require.NoError(t, err)
	o := v.AsObject()
	tables := 0
	for s := o.Shape().parent; s != nil && s.count != 0; s = s.parent {
		if s.table != nil {
			tables++
		}
	}
	assert.Equal(t, 0, tables, "intermediate literal shapes have no lookup tables")
	assert.Equal(t, 12, o.Shape().Count())
	x, err := o.GetProp(r, key(r, "l"))
	require.NoError(t, err)
	assert.Equal(t, IntValue(12), x)
	assert.NotNil(t, o.Shape().table, "the final shape builds its table on lookup")
}

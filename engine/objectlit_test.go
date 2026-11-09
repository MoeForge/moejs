package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// litModule evaluates the module src in a realm of opts and returns the
// realm and the binding name of the module's environment.
func litModule(t *testing.T, opts RealmOptions, src string, names ...string) (*Realm, []Value) {
	t.Helper()
	m, err := syntax.ParseModule("a.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealmWith(opts)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	var out []Value
	for _, n := range names {
		v, ok := env.GetBindingValue(n)
		require.True(t, ok, n)
		out = append(out, v)
	}
	return r, out
}

// TestObjectLitShapes checks that the object of a NewObjectLit has the
// shape the same properties added one at a time give, and a constructor's
// assignments: the one *Shape, at the first run of the literal and at the
// runs after, whose entry keeps it.
func TestObjectLitShapes(t *testing.T) {
	r, fns := litModule(t, RealmOptions{}, `
export function lit(a, b) { return {a, b}; }
export function adds(a, b) { const o = {}; o.a = a; o.b = b; return o; }
function C(a, b) { this.a = a; this.b = b; }
export function ctor(a, b) { return new C(a, b); }
export function one(a) { return {a}; }
export function three(a) { return {a, b: a, c: a}; }
`, "lit", "adds", "ctor", "one", "three")
	call := func(fn Value, args ...Value) *Object {
		v, err := r.Call(fn, Undefined(), args)
		require.NoError(t, err)
		return v.AsObject()
	}
	before := call(fns[1], IntValue(1), IntValue(2)).shape
	first := call(fns[0], IntValue(1), IntValue(2))
	assert.Same(t, before, first.shape, "the first run's shape is the adds'")
	for i := range 3 {
		o := call(fns[0], IntValue(i), str("x"))
		assert.Same(t, before, o.shape, "run %d", i)
		require.Len(t, o.slots, 2)
		assert.Equal(t, IntValue(i), o.slots[0])
		assert.Equal(t, "x", o.slots[1].AsString().GoString())
		assert.Equal(t, 2, cap(o.slots), "the room NewObjectCap(2) gives")
		assert.Equal(t, ClassObject, o.class)
		assert.Equal(t, flagExtensible, o.flags)
	}
	assert.Same(t, before, call(fns[1], IntValue(1), IntValue(2)).shape, "the adds after")
	// A constructor's instances take the same keys from the root of its
	// prototype, a literal's of the same keys after them still the adds'.
	c := call(fns[2], IntValue(1), IntValue(2))
	assert.Same(t, c.shape, call(fns[2], IntValue(3), IntValue(4)).shape)
	assert.Same(t, before, call(fns[0], IntValue(1), IntValue(2)).shape)

	// The shapes along the chain are the adds': {a} is the parent of
	// {a, b}, and {a, b} of {a, b, c}.
	assert.Same(t, before.parent, call(fns[3], IntValue(1)).shape)
	assert.Same(t, before, call(fns[4], IntValue(1)).shape.parent)
}

// TestObjectLitSizes checks a literal of each size up to objectLitMax and
// one past it: the values, the key order and the room, which is the room
// NewObjectCap gives the property count, at the first run and after.
func TestObjectLitSizes(t *testing.T) {
	for n := 1; n <= 17; n++ {
		var props, want []string
		for i := range n {
			k := fmt.Sprintf("k%d", n-1-i) // keys in another order than their names
			props = append(props, fmt.Sprintf("%s: %d", k, i))
			want = append(want, fmt.Sprintf("%q:%d", k, i))
		}
		r, fns := litModule(t, RealmOptions{}, "export function f() { return {"+strings.Join(props, ", ")+"}; }", "f")
		for run := range 3 {
			v, err := r.Call(fns[0], Undefined(), nil)
			require.NoError(t, err)
			o := v.AsObject()
			assert.Equal(t, "{"+strings.Join(want, ",")+"}", jsonOf(t, r, v), "%d properties, run %d", n, run)
			assert.Equal(t, cap(r.NewObjectCap(n).slots), cap(o.slots), "%d properties, run %d", n, run)
		}
	}
}

// TestObjectLitCharges checks that a NewObjectLit's object costs the
// memory account what the literal made one property at a time costs, which
// a computed key makes: the object, its room, and the shapes its first run
// adds.
func TestObjectLitCharges(t *testing.T) {
	const src = `
export function lit(a) { return {a, b: a, c: a, d: a, e: a}; }
export function adds(a) { return {a, b: a, c: a, d: a, ["e"]: a}; }
export function big(a) { return {a, b: a, c: a, d: a, e: a, f: a, g: a, h: a, i: a, j: a, k: a, l: a, m: a, n: a, o: a, p: a}; }
export function bigAdds(a) { return {a, b: a, c: a, d: a, e: a, f: a, g: a, h: a, i: a, j: a, k: a, l: a, m: a, n: a, o: a, ["p"]: a}; }
export function small(a) { return {a, b: a}; }
export function smallAdds(a) { return {a, ["b"]: a}; }
`
	for _, c := range []struct{ lit, adds string }{{"lit", "adds"}, {"big", "bigAdds"}, {"small", "smallAdds"}} {
		cost := func(name string) (first, next int64) {
			r, fns := litModule(t, RealmOptions{MemoryLimit: 1 << 30}, src, name)
			m := r.lazy.mem
			for i := range 2 {
				t0 := m.total
				_, err := r.Call(fns[0], Undefined(), []Value{IntValue(1)})
				require.NoError(t, err)
				if i == 0 {
					first = m.total - t0
				} else {
					next = m.total - t0
				}
			}
			return first, next
		}
		lf, ln := cost(c.lit)
		af, an := cost(c.adds)
		assert.Equal(t, af, lf, "%s: the first run", c.lit)
		assert.Equal(t, an, ln, "%s: a run after", c.lit)
	}
}

// TestObjectLitAllocations checks that a literal's object is one
// allocation with its room, as it was property by property.
func TestObjectLitAllocations(t *testing.T) {
	skipInRegcheck(t)
	for _, src := range []string{
		`export function f(a) { return {a}; }`,
		`export function f(a) { return {a, b: a}; }`,
		`export function f(a) { return {a, b: a, c: a}; }`,
		`export function f(a) { return {a, b: a, c: a, d: a}; }`,
		`export function f(a) { return {a, b: a, c: a, d: a, e: a, f: a, g: a, h: a}; }`,
		`export function f(a) { return {a, b: a, c: a, d: a, e: a, f: a, g: a, h: a, i: a, j: a, k: a, l: a, m: a, n: a, o: a, p: a}; }`,
	} {
		allocs, _, _ := callAllocs(t, src, IntValue(1))
		assert.Equal(t, 1.0, allocs, src)
	}
}

// TestObjectLitOtherShapes checks that a literal's entry holds its keys'
// shape whatever the objects of the same keys elsewhere do: a getter or a
// read-only property of the prototype under one of the keys, an object of
// the literal's shape made a prototype, frozen or given another attribute.
func TestObjectLitOtherShapes(t *testing.T) {
	r, fns := litModule(t, RealmOptions{}, `
export function lit(a) { return {a, b: a}; }
export function spoil() {
	const p = lit(1); Object.create(p); p.c = 1;
	Object.freeze(lit(2));
	Object.defineProperty(lit(3), "a", {enumerable: false});
	Object.defineProperty(Object.prototype, "b", {get() { throw new Error("getter"); }, set(v) { throw new Error("setter"); }, configurable: true});
}
export function check() { const o = lit(4); const d = Object.getOwnPropertyDescriptor(o, "b"); return [o.a, o.b, d.writable, d.enumerable, d.configurable, Object.isFrozen(o), Object.keys(o).join()]; }
`, "lit", "spoil", "check")
	_, err := r.Call(fns[0], Undefined(), []Value{IntValue(0)})
	require.NoError(t, err)
	_, err = r.Call(fns[1], Undefined(), nil)
	require.NoError(t, err)
	for range 2 {
		v, err := r.Call(fns[2], Undefined(), nil)
		require.NoError(t, err)
		assert.Equal(t, `[4,4,true,true,true,false,"a,b"]`, jsonOf(t, r, v))
	}
}

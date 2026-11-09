package engine

import (
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArrayPushK runs array literals whose constant elements are pushed by
// ArrayPushK: each evaluation makes a fresh array, the values keep their
// order with holes, spreads and other elements between them, and they are
// defined, not set, past setters on Array.prototype. The expected results
// are Node 24's.
func TestArrayPushK(t *testing.T) {
	src := `
var out = [];
function f() { return [1, 2, 3]; }
var a = f(), b = f(); a.push(4); a[0] = 9;
out.push(a.join(), b.join(), a !== b, b.length);
function g(x) { return [-0, 'é', null, undefined, NaN, 1e300, x, 'k', 2, , 5, 6, ...[7, 8], 0.5, -1]; }
var c = g('x');
out.push(c.length, Object.is(c[0], -0), c[1], c[2], c[3] === undefined, 3 in c, isNaN(c[4]), c[5], c[6], c.slice(7).join('|'), 9 in c);
var keep = [];
for (var i = 0; i < 5; i++) keep.push(f());
keep[2][1] = 'z';
out.push(keep.map(function (r) { return r.join(''); }).join(' '));
Object.defineProperty(Array.prototype, '0', {set: function () { throw new Error('setter'); }, get: function () { return 'proto'; }, configurable: true});
Object.defineProperty(Array.prototype, '1', {set: function () { throw new Error('setter'); }, configurable: true});
var d = f();
out.push(d.join(), Object.getOwnPropertyNames(d).join());
delete Array.prototype[0]; delete Array.prototype[1];
var fz = Object.freeze(f()); out.push(Object.isFrozen(fz), f().length, Object.isFrozen(f()));
function h(n) { var s = 0, r; for (var i = 0; i < n; i++) { r = [i, 1, 2, 3, 'a', null]; s += r[1] + r[2] + r.length; } return s; }
out.push(h(1000));
out.join(' ');`
	assert.Equal(t, "9,2,3,4 1,2,3 true 3 16 true é  true true true 1e+300 x k|2||5|6|7|8|0.5|-1 false 123 123 1z3 123 123 1,2,3 0,1,2,length true 3 false 9000", runScripts(t, src))
}

// TestArrayPushKLong runs literals longer than one ArrayPushK and longer
// than the constant pool's room for them, whose rest is pushed one by one.
func TestArrayPushKLong(t *testing.T) {
	for _, n := range []int{255, 256, 257, 600, 5000} {
		var b strings.Builder
		b.WriteString("var a = [")
		for i := range n {
			if i > 0 {
				b.WriteString(", ")
			}
			if i%3 == 2 {
				b.WriteString("'" + strconv.Itoa(i) + "'")
			} else {
				b.WriteString(strconv.Itoa(i))
			}
		}
		b.WriteString("]; var ok = a.length === " + strconv.Itoa(n) + ";")
		b.WriteString("for (var i = 0; i < a.length; i++) if (a[i] !== (i % 3 === 2 ? String(i) : i)) ok = false; ok;")
		assert.Equal(t, "true", runScripts(t, b.String()), "n=%d", n)
	}
}

// TestArrayPushKCharge checks that a literal of constants charges the
// memory account as one of the same length whose elements are not
// constants.
func TestArrayPushKCharge(t *testing.T) {
	r := NewRealm()
	r.SetMemoryLimit(1 << 30)
	var k, v strings.Builder
	for i := range 40 {
		if i > 0 {
			k.WriteString(", ")
			v.WriteString(", ")
		}
		k.WriteString(strconv.Itoa(i))
		v.WriteString("x")
	}
	runScriptsIn(t, r, "var x = 1, last; function fk() { return ["+k.String()+"]; } function fv() { return ["+v.String()+"]; }")
	charge := func(fn string) int64 {
		before := r.Stats().AllocatedBytes
		runScriptsIn(t, r, "for (var i = 0; i < 100; i++) last = "+fn+"();")
		return r.Stats().AllocatedBytes - before
	}
	charge("fk")
	charge("fv")
	ck, cv := charge("fk"), charge("fv")
	require.Positive(t, ck)
	assert.Equal(t, cv, ck)
}

// TestPushConsts checks the stores of pushConsts: a number, null or
// undefined over a slot without a pointer takes only the value's bits; a
// string, or any constant over a slot that still holds a pointer past the
// length, is stored whole; storage that grows moves; a frozen array is
// left as Push leaves it.
func TestPushConsts(t *testing.T) {
	r := NewRealm()
	s := StringValue(FromGoString("s"))
	ks := []Value{NumberValue(1.5), s, Null(), Undefined(), NumberValue(-2)}

	o := r.NewArrayCap(len(ks))
	o.pushConsts(r, ks)
	assert.Equal(t, ks, o.elems())
	assert.Equal(t, uint32(len(ks)), o.ArrayLength())

	// Slots past the length that hold a string (as no literal does).
	o = r.NewArrayCap(4)
	o.Push(r, s, s, s)
	o.setElems(o.elems()[:1])
	o.aux = 1
	o.pushConsts(r, ks[2:])
	assert.Equal(t, []Value{s, Null(), Undefined(), NumberValue(-2)}, o.elems())

	// Past the capacity, the storage moves.
	o = r.NewArrayCap(2)
	p := o.inlineElems().p
	o.pushConsts(r, ks)
	o.pushConsts(r, ks[:2])
	assert.Equal(t, append(append([]Value{}, ks...), ks[:2]...), o.elems())
	assert.NotEqual(t, p, o.inlineElems().p)
	assert.Equal(t, uint32(len(ks)+2), o.ArrayLength())

	o = r.NewArrayCap(4)
	o.Push(r, s)
	o.flags |= flagFrozen
	o.flags &^= flagExtensible
	o.pushConsts(r, ks)
	assert.Equal(t, []Value{s}, o.elems())
	assert.Equal(t, uint32(1), o.ArrayLength())
}

// TestArrayStorageSizes checks that an array literal (of other than
// constants, newArrayConsts), Array.prototype.slice and Object.keys
// co-allocate the storage of up to 12, 14 and 16 elements
// with the array, and keep a separate slice for 13, 15 and more than 16,
// whose co-allocation would cost bytes: each co-allocated size lands in
// the size class of the object and a separate slice.
func TestArrayStorageSizes(t *testing.T) {
	skipInRegcheck(t)
	for n := 1; n <= 18; n++ {
		want := 1.0
		if n > coArrayMax || n == 13 || n == 15 {
			want = 2
		}
		var lit, vars, keys strings.Builder
		for i := range n {
			if i > 0 {
				lit.WriteString(", ")
				vars.WriteString(", ")
				keys.WriteString(", ")
			}
			lit.WriteString(strconv.Itoa(i))
			vars.WriteString("x")
			keys.WriteString("k" + strconv.Itoa(i) + ": 0")
		}
		m, err := syntax.ParseModule("sizes.js", "const src = ["+lit.String()+"], o = {"+keys.String()+"};"+
			"export function lit(x) { return ["+vars.String()+"]; }"+
			"export function slice() { return src.slice(); }"+
			"export function keys() { return Object.keys(o); }", syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileModule(m)
		require.NoError(t, err)
		r := NewRealm()
		env, err := r.EvaluateModule(code)
		require.NoError(t, err)
		for _, fn := range []string{"lit", "slice", "keys"} {
			f, _ := env.GetBindingValue(fn)
			v, err := r.Call(f, Undefined(), nil)
			require.NoError(t, err)
			require.Equal(t, uint32(n), v.AsObject().ArrayLength(), "%s n=%d", fn, n)
			assert.Equal(t, want, testing.AllocsPerRun(20, func() { _, _ = r.Call(f, Undefined(), nil) }), "%s n=%d", fn, n)
		}
	}
	if unsafe.Sizeof(uintptr(0)) == 8 {
		for _, c := range []struct {
			n    int
			size uintptr
		}{
			{9, unsafe.Sizeof(arrayObject9{})}, {10, unsafe.Sizeof(arrayObject10{})}, {11, unsafe.Sizeof(arrayObject11{})},
			{12, unsafe.Sizeof(arrayObject12{})}, {14, unsafe.Sizeof(arrayObject14{})}, {16, unsafe.Sizeof(arrayObject16{})},
		} {
			// 64 bytes of array and 16 of each element: the size classes
			// 208, 224, 240, 256, 288 and 320.
			assert.Equal(t, 64+16*uintptr(c.n), c.size, "n=%d", c.n)
		}
	}
}

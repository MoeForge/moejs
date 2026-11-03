package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/require"
)

// Audit A: Object, Array, Function, Error, Boolean, Number, Math and the
// global functions. Tests that demonstrate a bug start with t.Skip so the
// suite stays green; remove the Skip line to reproduce.

// auditAPrelude defines S (a printer that distinguishes -0, strings and
// arrays) and T (runs a thunk and reports "Name: message" for a throw).
const auditAPrelude = `
function S(v) {
  if (typeof v === 'string') return JSON.stringify(v);
  if (typeof v === 'number') return (v === 0 && 1 / v < 0) ? '-0' : String(v);
  if (Array.isArray(v)) return JSON.stringify(v);
  if (v === undefined) return 'undefined';
  if (v === null) return 'null';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}
function T(f) {
  try { return 'ok:' + S(f()); } catch (e) { return (e && e.name) + ': ' + (e && e.message); }
}
`

// auditAModule compiles body as the top level of a module with the prelude
// and returns the fixture (or the evaluation error).
func auditAModule(t *testing.T, body string) (*moduleFixture, error) {
	t.Helper()
	m, err := syntax.ParseModule("audit.js", auditAPrelude+body, syntax.Options{})
	require.NoError(t, err, "parse: %s", body)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err, "compile: %s", body)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	if err != nil {
		return &moduleFixture{t: t, r: r, env: env}, err
	}
	return &moduleFixture{t: t, r: r, env: env}, nil
}

// auditAExpr evaluates S(expr) inside a fresh module and returns the string.
func auditAExpr(t *testing.T, expr string) string {
	t.Helper()
	wrapped := "S(" + expr + ")"
	if strings.HasPrefix(expr, "T(") {
		wrapped = expr
	}
	f, err := auditAModule(t, "export function f() { return "+wrapped+"; }")
	require.NoError(t, err)
	v, err := f.callErr("f")
	if err != nil {
		return "THROW " + errMessage(t, err)
	}
	s, ok := v.(string)
	require.True(t, ok, "S() did not return a string: %#v", v)
	return s
}

// auditACall runs a statement list as the body of an exported function
// and returns its ToGo result and error.
func auditACall(t *testing.T, body string) (any, error) {
	t.Helper()
	f, err := auditAModule(t, "export function f() { "+body+" }")
	require.NoError(t, err)
	return f.callErr("f")
}

// ---------------------------------------------------------------------------
// 1. pushArgs/popArgs pairing
// ---------------------------------------------------------------------------

func TestAuditA_PushArgsBalanced(t *testing.T) {
	calls := []string{
		`[1,2,3].map(x => { throw 1 })`,
		`[1,2,3].filter(x => { throw 1 })`,
		`[1,2,3].forEach(x => { throw 1 })`,
		`[1,2,3].some(x => { throw 1 })`,
		`[1,2,3].every(x => { throw 1 })`,
		`[1,2,3].find(x => { throw 1 })`,
		`[1,2,3].findIndex(x => { throw 1 })`,
		`[1,2,3].findLast(x => { throw 1 })`,
		`[1,2,3].findLastIndex(x => { throw 1 })`,
		`[1,2,3].reduce((a, x) => { throw 1 })`,
		`[1,2,3].reduceRight((a, x) => { throw 1 })`,
		`[1,2,3].flatMap(x => { throw 1 })`,
		`[3,1,2].sort((a, b) => { throw 1 })`,
		`Array.from([1,2,3], x => { throw 1 })`,
		`Array.from({length: 3}, x => { throw 1 })`,
		`Array.from("abc", x => { throw 1 })`,
		`Array.from([1,2].values(), x => { throw 1 })`,
		`[[1],[2]].flat().map(x => { throw 1 })`,
		// Nested: a callback that itself runs a throwing callback builtin.
		`[1,2,3].map(x => [1].map(y => { throw 1 }))`,
		// Comparator that throws after a few compares (mergeSort mid-way).
		`Array.from({length: 40}, (_, i) => 40 - i).sort((a, b) => { if (a === 7) throw 1; return a - b })`,
	}
	var body string
	for i, c := range calls {
		body += fmt.Sprintf("export function f%d() { for (let i = 0; i < 1000; i++) { try { %s } catch (e) {} } }\n", i, c)
	}
	f, err := auditAModule(t, body)
	require.NoError(t, err)
	sp0 := f.r.interp.sp
	for i := range calls {
		f.call(fmt.Sprintf("f%d", i))
		require.Equal(t, sp0, f.r.interp.sp, "register stack leaked after %s", calls[i])
	}
	// Also from the host side (no interpreter frame active).
	arr, _ := f.r.FromGo([]any{1, 2, 3})
	thrower := throwingFn(f.r, "boom")
	for _, name := range []string{"map", "filter", "forEach", "some", "every", "find", "findIndex", "findLast", "findLastIndex", "reduce", "reduceRight", "flatMap", "sort"} {
		for range 100 {
			err := jsCallErr(t, f.r, arr, name, thrower)
			require.Error(t, err, name)
		}
		require.Equal(t, sp0, f.r.interp.sp, "register stack leaked after host-side %s", name)
	}
}

// ---------------------------------------------------------------------------
// 2. Native error mid-way consistency / mutation during natives
// ---------------------------------------------------------------------------

func TestAuditA_SortThrowingComparatorKeepsPermutation(t *testing.T) {
	got := auditAExpr(t, `(() => {
		const a = [5, 3, 9, 1, 7, 2, 8, 6, 4, 0, 11, 13, 12, 10, 15, 14];
		let n = 0;
		try { a.sort((x, y) => { if (++n === 20) throw new Error("cmp"); return x - y }); } catch (e) {}
		const copy = a.slice().sort((x, y) => x - y);
		return [a.length, copy.join(','), a.indexOf(undefined), Object.keys(a).length];
	})()`)
	require.Equal(t, `[16,"0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15",-1,16]`, got)
}

// Throwing accessors in the middle of the generic (array-like) algorithms
// must leave exactly the spec's partial state behind.
func TestAuditA_ThrowingAccessorMidWay(t *testing.T) {
	cases := []struct{ expr, want string }{
		// splice: the first element is collected, the getter at 1 throws
		// before any write.
		{`(() => { const o = {length: 3, 0: 'a', 2: 'c'}; Object.defineProperty(o, 1, {get() { throw new Error("g") }, configurable: true, enumerable: true}); return T(() => Array.prototype.splice.call(o, 0, 2)) + ':' + S([o[0], o[2], o.length]) })()`, `"Error: g:[\"a\",\"c\",3]"`},
		// reverse reads both ends before writing either.
		{`(() => { const o = {length: 2, 0: 'a'}; Object.defineProperty(o, 1, {get() { throw new Error("g") }, configurable: true}); return T(() => Array.prototype.reverse.call(o)) + ':' + S([o[0], o.length]) })()`, `"Error: g:[\"a\",2]"`},
		// shift: Set(0, Get(1)) succeeds, Set(1, Get(2)) hits the setter.
		{`(() => { const o = {length: 3, 0: 'a', 2: 'c'}; Object.defineProperty(o, 1, {get() { return 'b' }, set(v) { throw new Error("s" + v) }, configurable: true}); return T(() => Array.prototype.shift.call(o)) + ':' + S([o[0], o[1], o[2], o.length]) })()`, `"Error: sc:[\"b\",\"b\",\"c\",3]"`},
		// unshift moves from the top: Set(3, Get(2)) ok, Set(2, Get(1)) ok, Set(1, Get(0)) hits the setter.
		{`(() => { const o = {length: 3, 0: 'a', 2: 'c'}; Object.defineProperty(o, 1, {get() { return 'b' }, set(v) { throw new Error("s" + v) }, configurable: true}); return T(() => Array.prototype.unshift.call(o, 'x')) + ':' + S([o[0], o[1], o[2], o[3], o.length]) })()`, `"Error: sa:[\"a\",\"b\",\"b\",\"c\",3]"`},
		// fill: Set(0) ok, Set(1) hits the setter.
		{`(() => { const o = {length: 3, 0: 'a', 2: 'c'}; Object.defineProperty(o, 1, {set(v) { throw new Error("s" + v) }, configurable: true}); return T(() => Array.prototype.fill.call(o, 'z')) + ':' + S([o[0], o[2], o.length]) })()`, `"Error: sz:[\"z\",\"c\",3]"`},
		// sort on an array-like collects through the getter before writing.
		{`(() => { const o = {length: 3, 0: 'c', 2: 'a'}; Object.defineProperty(o, 1, {get() { throw new Error("g") }, configurable: true}); return T(() => Array.prototype.sort.call(o)) + ':' + S([o[0], o[2]]) })()`, `"Error: g:[\"c\",\"a\"]"`},
		// pop: Get(2) ok, the delete of a non-configurable index throws, length untouched.
		{`(() => { const o = {length: 3, 0: 'a'}; Object.defineProperty(o, 2, {value: 'c', configurable: false}); return T(() => Array.prototype.pop.call(o)) + ':' + S([o[2], o.length]) })()`, `"TypeError: Cannot delete property '2' of [object Object]:[\"c\",3]"`},
		// copy loop of splice with insertion: from the top, a read-only slot stops it.
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 2, {writable: false}); return T(() => a.splice(0, 0, 'x')) + ':' + S(a) })()`, `"TypeError: Cannot assign to read only property '2' of object:[1,2,3,3]"`},
	}
	for _, c := range cases {
		got := auditAExpr(t, c.expr)
		if got != c.want {
			t.Errorf("%s\n  want %s\n  got  %s", c.expr, c.want, got)
		}
	}
}

func TestAuditA_SortMutatingComparatorDoesNotPanic(t *testing.T) {
	cases := []string{
		`const a = [3,1,2,5,4,9,8,7,6,0,15,14,13,12,11,10]; a.sort((x, y) => { a.length = 0; return x - y }); return S(a.length)`,
		`const a = [3,1,2,5,4,9,8,7,6,0,15,14,13,12,11,10]; a.sort((x, y) => { a.push(100); return x - y }); return S(a.length >= 16)`,
		`const a = [3,1,2,5,4,9,8,7,6,0,15,14,13,12,11,10]; a.sort((x, y) => { a.pop(); return x - y }); return S(a.length)`,
		`const a = [3,1,2,5,4,9,8,7,6,0,15,14,13,12,11,10]; a.sort((x, y) => { delete a[0]; delete a[15]; return x - y }); return S(a.length)`,
		`const a = [3,1,2,5,4,9,8,7,6,0,15,14,13,12,11,10]; a.sort((x, y) => { a[1000] = 1; return x - y }); return S(a.length)`,
		`const a = [3,1,2,5,4,9,8,7,6,0,15,14,13,12,11,10]; return T(() => a.sort((x, y) => { Object.freeze(a); return x - y }))`,
		`const a = [3,1,2]; a.sort((x, y) => { a.length = 1; return x - y }); return S(a)`,
	}
	for _, c := range cases {
		require.NotPanics(t, func() {
			v, err := auditACall(t, c)
			t.Logf("%s => %v %v", c, v, err)
		}, c)
	}
}

// Finding: fill's dense fast path indexes o.elements with bounds computed
// from a length captured before the start/end arguments were coerced; a
// valueOf that shrinks the array makes the write run off the end.
func TestAuditA_FillShrinkDuringCoercionPanics(t *testing.T) {
	require.NotPanics(t, func() {
		v, err := auditACall(t, `const a = [1,2,3]; a.fill(0, {valueOf() { a.length = 0; return 0 }}); return S(a)`)
		require.NoError(t, err)
		require.Equal(t, `[0,0,0]`, v, "fill writes through the stale length per spec")
	})
	// Shrinking below end, shrinking the start, and growing during coercion.
	require.Equal(t, `[1,7,7]`, auditAExpr(t, `(() => { const a = [1,2,3]; a.fill(7, {valueOf() { a.length = 1; return 1 }}, 3); return a })()`))
	require.Equal(t, `[7,7,7,4,5]`, auditAExpr(t, `(() => { const a = [1,2,3]; a.fill(7, {valueOf() { a.push(4, 5); return 0 }}); return a })()`))
}

// Finding: splice's dense fast path slices o.elements with the stale
// length after start/deleteCount coercion shrank the array.
func TestAuditA_SpliceShrinkDuringCoercionPanics(t *testing.T) {
	require.NotPanics(t, func() {
		v, err := auditACall(t, `const a = [1,2,3,4,5]; const r = a.splice({valueOf() { a.length = 2; return 0 }}, 1); return S([a, r])`)
		require.NoError(t, err)
		require.Equal(t, `[[2,null,null,null],[1]]`, v)
	})
	require.NotPanics(t, func() {
		v, err := auditACall(t, `const a = [1,2,3]; const r = a.splice({valueOf() { a.length = 0; return 0 }}, 1); return S([a, r])`)
		require.NoError(t, err)
		require.Equal(t, `[[null,null],[null]]`, v)
	})
	// deleteCount coercion shrinks with items to insert; start coercion grows.
	require.Equal(t, `[[9,8,null],[1,null]]`, auditAExpr(t, `(() => { const a = [1,2,3]; const r = a.splice(0, {valueOf() { a.length = 1; return 2 }}, 9, 8); return [a, r] })()`))
	require.Equal(t, `[[1,3],[2]]`, auditAExpr(t, `(() => { const a = [1,2,3]; const r = a.splice({valueOf() { a.push(4, 5, 6); return 1 }}, 1); return [a, r] })()`))
}

// Finding: slice's dense fast path copies from o.elements[k:k+count] using
// the stale length; after a shrink the cleared backing store (zero Values,
// which read as the number 0) is returned instead of holes.
func TestAuditA_SliceShrinkDuringCoercionReadsStaleStorage(t *testing.T) {
	got := auditAExpr(t, `(() => { const a = [1,2,3]; const r = a.slice({valueOf() { a.length = 0; return 0 }}); return [r.length, 0 in r, r[0]] })()`)
	require.Equal(t, `[3,false,null]`, got)
	// end coercion shrinks the tail; start coercion shrinks below start.
	require.Equal(t, `[2,false,false]`, auditAExpr(t, `(() => { const a = [1,2,3]; const r = a.slice(1, {valueOf() { a.length = 1; return 3 }}); return [r.length, 0 in r, 1 in r] })()`))
	require.Equal(t, `[1,false]`, auditAExpr(t, `(() => { const a = [1,2,3]; const r = a.slice({valueOf() { a.length = 1; return 2 }}); return [r.length, 0 in r] })()`))
}

// ---------------------------------------------------------------------------
// 3. Interrupt coverage
// ---------------------------------------------------------------------------

// auditAInterrupted calls recv[name](args) as a native with the interrupt
// flag already set (natives do not check at entry, only bytecode functions
// do) and reports whether the loop observed it.
func auditAInterrupted(t *testing.T, r *Realm, recv Value, name string, args ...Value) (bool, any, error) {
	t.Helper()
	fn := jsGet(t, r, recv, name)
	require.True(t, IsCallable(fn))
	r.Interrupt("x")
	defer r.ClearInterrupt()
	res, err := r.Call(fn, recv, args)
	var ie *InterruptedError
	return errors.As(err, &ie), r.ToGo(res), err
}

func TestAuditA_InterruptHugeSparseArray(t *testing.T) {
	f, err := auditAModule(t, `export const a = new Array(2**32 - 1); export const like = {length: 2**32 - 1};`)
	require.NoError(t, err)
	r := f.r
	a, _ := f.env.GetBindingValue("a")
	like, _ := f.env.GetBindingValue("like")
	one := IntValue(1)
	cases := []struct {
		name string
		recv Value
		fn   string
		args []Value
	}{
		{"indexOf", a, "indexOf", []Value{one}},
		{"lastIndexOf", a, "lastIndexOf", []Value{one}},
		{"includes", a, "includes", []Value{one}},
		{"join", a, "join", nil},
		{"toString", a, "toString", nil},
		{"fill", a, "fill", []Value{IntValue(0)}},
		{"reverse", a, "reverse", nil},
		{"slice", a, "slice", nil},
		{"sort", a, "sort", nil},
		{"splice(0)", a, "splice", []Value{IntValue(0)}},
		{"shift", a, "shift", nil},
		{"unshift(1)", a, "unshift", []Value{one}},
		{"[].concat(a)", jsArray(r), "concat", []Value{a}},
		{"Array.from(like)", jsGlobal(t, r, "Array"), "from", []Value{like}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, res, err := auditAInterrupted(t, r, c.recv, c.fn, c.args...)
			require.True(t, ok, "%s did not return *InterruptedError: res=%v err=%v", c.name, res, err)
		})
	}
}

// Finding: callback builtins skip holes without an interrupt check, so a
// sparse receiver never reaches the callback (whose function-entry check
// is the only one) and the interrupt is not observed.
func TestAuditA_InterruptSparseCallbackBuiltins(t *testing.T) {
	f, err := auditAModule(t, `export const a = new Array(2**18); export const id = x => x; export const add = (x, y) => x;`)
	require.NoError(t, err)
	r := f.r
	a, _ := f.env.GetBindingValue("a")
	id, _ := f.env.GetBindingValue("id")
	add, _ := f.env.GetBindingValue("add")
	cases := []struct {
		name string
		recv Value
		fn   string
		args []Value
	}{
		{"forEach", a, "forEach", []Value{id}},
		{"map", a, "map", []Value{id}},
		{"filter", a, "filter", []Value{id}},
		{"some", a, "some", []Value{id}},
		{"every", a, "every", []Value{id}},
		{"reduce", a, "reduce", []Value{add, IntValue(0)}},
		{"reduceRight", a, "reduceRight", []Value{add, IntValue(0)}},
		{"flat", a, "flat", nil},
		{"flatMap", a, "flatMap", []Value{id}},
		{"Array.from(array)", jsGlobal(t, r, "Array"), "from", []Value{a}},
		{"Array.from(array, mapFn)", jsGlobal(t, r, "Array"), "from", []Value{a, id}},
		{"reduce(no initial)", a, "reduce", []Value{add}},
		{"reduceRight(no initial)", a, "reduceRight", []Value{add}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, res, err := auditAInterrupted(t, r, c.recv, c.fn, c.args...)
			require.True(t, ok, "%s did not return *InterruptedError: res=%v err=%v", c.name, res, err)
		})
	}
	// A dense receiver with a native callback (no function-entry check) is
	// covered by the same loop check.
	f, err = auditAModule(t, `export const zeros = new Array(2**14).fill(0); export const ones = new Array(2**14).fill(1);`)
	require.NoError(t, err)
	r = f.r
	zeros, _ := f.env.GetBindingValue("zeros")
	ones, _ := f.env.GetBindingValue("ones")
	boolean := jsGlobal(t, r, "Boolean")
	for _, name := range []string{"forEach", "map", "filter", "some", "flatMap"} {
		ok, res, err := auditAInterrupted(t, r, zeros, name, boolean)
		require.True(t, ok, "%s over a dense array with a native callback did not return *InterruptedError: res=%v err=%v", name, res, err)
	}
	ok, res, err := auditAInterrupted(t, r, ones, "every", boolean)
	require.True(t, ok, "every over a dense array with a native callback did not return *InterruptedError: res=%v err=%v", res, err)
	ok, res, err = auditAInterrupted(t, r, jsGlobal(t, r, "Array"), "from", []Value{zeros, boolean}...)
	require.True(t, ok, "Array.from(dense, native) did not return *InterruptedError: res=%v err=%v", res, err)
}

// Finding: find/findIndex/findLast/findLastIndex have no interrupt check of
// their own; with a native predicate nothing ever checks the flag.
func TestAuditA_InterruptFindNativePredicate(t *testing.T) {
	f, err := auditAModule(t, `export const a = new Array(2**18);`)
	require.NoError(t, err)
	r := f.r
	a, _ := f.env.GetBindingValue("a")
	boolean := jsGlobal(t, r, "Boolean")
	for _, name := range []string{"find", "findIndex", "findLast", "findLastIndex"} {
		ok, res, err := auditAInterrupted(t, r, a, name, boolean)
		require.True(t, ok, "%s did not return *InterruptedError: res=%v err=%v", name, res, err)
	}
}

// Finding: flat/flatMap preallocate make([]Value, 0, len) from the
// user-controlled length before visiting a single element, and
// Array.from(array) from the array's length (new Array(2**32-1) is a 64 GB
// request). The spec loops over every index, so the receivers throw from
// their first element to end the call; the crash was in the allocation
// before it.
func TestAuditA_FlatHugeLengthPanics(t *testing.T) {
	for _, expr := range []string{
		`[].flat.call(Object.defineProperty({length: 2**53 - 1}, 0, {get() { throw new Error("stop") }}))`,
		`[].flatMap.call(Object.defineProperty({length: 2**53 - 1}, 0, {get() { throw new Error("stop") }}), x => x)`,
		`[].flatMap.call({length: 2**53 - 1, 0: 1}, x => { throw new Error("stop") })`,
		`Array.from(new Array(2**32 - 1), x => { throw new Error("stop") })`,
		`Array.from(Object.defineProperty(new Array(2**32 - 1), 0, {get() { throw new Error("stop") }}))`,
	} {
		require.NotPanics(t, func() {
			v, err := auditACall(t, `return T(() => `+expr+`)`)
			require.NoError(t, err, expr)
			require.Equal(t, "Error: stop", v, expr)
		}, expr)
	}
	// Result semantics are unchanged: array-likes and sparse arrays flatten
	// to their present elements, dense arrays keep every element.
	require.Equal(t, `[1,2,3]`, auditAExpr(t, `[].flat.call({length: 5, 0: 1, 2: [2, 3]})`))
	require.Equal(t, `[7]`, auditAExpr(t, `(() => { const a = new Array(3000); a[2999] = [7]; return a.flat() })()`))
	require.Equal(t, `[1,2,3,4]`, auditAExpr(t, `[[1], [2, [3]], 4].flat(2)`))
	require.Equal(t, `[1,1,2,2]`, auditAExpr(t, `[1, 2].flatMap(x => [x, x])`))
	require.Equal(t, `[null,null,null]`, auditAExpr(t, `Array.from(new Array(3))`))
}

// Finding: splice's trailing delete loop has no interrupt check.
func TestAuditA_InterruptSpliceDeleteLoop(t *testing.T) {
	// The interrupt is raised by an accessor at the last index (2^18, which
	// is not 4095 mod 4096, so the collection loop's stride check does not
	// fire after it); the delete loop then runs 2^18+1 iterations without
	// looking at the flag and splice returns normally.
	f, err := auditAModule(t, `
export const a = new Array(2**18 + 1);
export function arm(interrupt) { Object.defineProperty(a, 2**18, {get() { interrupt(); return 1 }, configurable: true}); }`)
	require.NoError(t, err)
	r := f.r
	a, _ := f.env.GetBindingValue("a")
	arm, _ := f.env.GetBindingValue("arm")
	interrupt := nativeFn(r, func(Value, []Value) (Value, error) { r.Interrupt("x"); return Undefined(), nil })
	_, err = r.Call(arm, Undefined(), []Value{interrupt})
	require.NoError(t, err)
	defer r.ClearInterrupt()
	splice := jsGet(t, r, a, "splice")
	_, err = r.Call(splice, a, []Value{IntValue(0)})
	var ie *InterruptedError
	require.True(t, errors.As(err, &ie), "splice completed without observing the interrupt: %v", err)
}

// ---------------------------------------------------------------------------
// 4. Spec conformance table
// ---------------------------------------------------------------------------

func TestAuditA_SpecTable(t *testing.T) {
	cases := []struct{ expr, want string }{
		// Property enumeration order.
		{`Object.keys({b:1, 2:1, a:1, 1:1})`, `["1","2","b","a"]`},
		{`Object.keys({b:1, 4294967295:1, a:1, 4294967294:1, 1:1})`, `["1","4294967294","b","4294967295","a"]`},
		{`(() => { const o = {a:1, b:2, c:3}; delete o.a; o.a = 1; return Object.keys(o) })()`, `["b","c","a"]`},
		{`(() => { const o = {}; for (let i = 0; i < 70; i++) o['k' + i] = i; delete o.k3; o.k3 = 1; return Object.keys(o).length + ':' + Object.keys(o)[0] + ':' + Object.keys(o)[69] })()`, `"70:k0:k3"`},
		{`(() => { const o = {}; for (let i = 0; i < 70; i++) o['k' + i] = i; return Object.keys(o).every((k, i) => k === 'k' + i) })()`, `true`},
		{`Object.getOwnPropertyNames([1,2])`, `["0","1","length"]`},
		{`Object.keys("abc")`, `["0","1","2"]`},
		{`Object.getOwnPropertyNames("ab")`, `["0","1","length"]`},
		{`Object.entries({b:1, 1:2})`, `[["1",2],["b",1]]`},
		{`Object.values({b:1, 1:2})`, `[2,1]`},
		// Array length semantics.
		{`(() => { const a = [1,2,3]; a.length = 1; return a })()`, `[1]`},
		{`T(() => { const a = []; a.length = 1.5 })`, `RangeError: Invalid array length`},
		{`(() => { const a = [1]; a.length = "3"; return a.length })()`, `3`},
		{`T(() => { const a = [1]; Object.defineProperty(a, 'length', {writable: false}); a.push(2) })`, `TypeError: Cannot assign to read only property '1' of object`},
		{`T(() => { const a = [1]; Object.defineProperty(a, 'length', {writable: false}); a.length = 5 })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = Object.freeze([1]); a.length = 0 })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = Object.freeze([1]); a.push(2) })`, `TypeError: Cannot assign to read only property '1' of object`},
		{`T(() => { const a = Object.freeze([1]); a[0] = 2; return a })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, '5', {value: 1}); return a.length })()`, `6`},
		{`T(() => { const a = []; a.length = -1 })`, `RangeError: Invalid array length`},
		{`T(() => { const a = []; a.length = 2**32 })`, `RangeError: Invalid array length`},
		{`T(() => new Array(-1))`, `RangeError: Invalid array length`},
		{`T(() => new Array(1.5))`, `RangeError: Invalid array length`},
		{`new Array("3").length`, `1`},
		{`new Array(3).length`, `3`},
		{`(() => { const a = [1,2,3]; a.length = 0; return [a.length, 0 in a] })()`, `[0,false]`},
		// Number.prototype.toString(radix)
		{`(255).toString(16)`, `"ff"`},
		{`(-255).toString(2)`, `"-11111111"`},
		{`(0.5).toString(2)`, `"0.1"`},
		{`(-0.5).toString(2)`, `"-0.1"`},
		{`(NaN).toString(2)`, `"NaN"`},
		{`(Infinity).toString(16)`, `"Infinity"`},
		{`(-Infinity).toString(36)`, `"-Infinity"`},
		{`(0).toString(2)`, `"0"`},
		{`(-0).toString(2)`, `"0"`},
		{`(2**53).toString(2)`, `"100000000000000000000000000000000000000000000000000000"`},
		{`(1e21).toString(10)`, `"1e+21"`},
		{`(123.456).toString()`, `"123.456"`},
		{`T(() => (1).toString(1))`, `RangeError: toString() radix must be between 2 and 36`},
		{`T(() => (1).toString(37))`, `RangeError: toString() radix must be between 2 and 36`},
		{`(35).toString(36)`, `"z"`},
		// toFixed
		{`(0.5).toFixed(0)`, `"1"`},
		{`(1.5).toFixed(0)`, `"2"`},
		{`(2.5).toFixed(0)`, `"3"`},
		{`(-1.5).toFixed(0)`, `"-2"`},
		{`(-0.5).toFixed(0)`, `"-1"`},
		{`(1e21).toFixed(2)`, `"1e+21"`},
		{`(-1e21).toFixed(2)`, `"-1e+21"`},
		{`(1.005).toFixed(2)`, `"1.00"`},
		{`(1.255).toFixed(2)`, `"1.25"`},
		{`(0).toFixed(2)`, `"0.00"`},
		{`(-0).toFixed(2)`, `"0.00"`},
		{`(0.000001).toFixed(7)`, `"0.0000010"`},
		{`(123.456).toFixed(1)`, `"123.5"`},
		{`(1).toFixed(100).length`, `102`},
		{`T(() => (1).toFixed(101))`, `RangeError: toFixed() digits argument must be between 0 and 100`},
		{`T(() => (1).toFixed(-1))`, `RangeError: toFixed() digits argument must be between 0 and 100`},
		{`(NaN).toFixed(2)`, `"NaN"`},
		{`(Infinity).toFixed(2)`, `"Infinity"`},
		{`(-Infinity).toFixed(2)`, `"-Infinity"`},
		{`(0.1).toFixed(20)`, `"0.10000000000000000555"`},
		{`(1e20).toFixed(2)`, `"100000000000000000000.00"`},
		{`(2**53).toFixed(1)`, `"9007199254740992.0"`},
		{`(-1.5).toFixed()`, `"-2"`},
		{`(0.45).toFixed(1)`, `"0.5"`},
		{`(8.345).toFixed(2)`, `"8.35"`},
		{`(8.335).toFixed(2)`, `"8.34"`},
		// toPrecision
		{`(0.000001234).toPrecision(1)`, `"0.000001"`},
		{`(0.0000001234).toPrecision(2)`, `"1.2e-7"`},
		{`(123.456).toPrecision(2)`, `"1.2e+2"`},
		{`(123.456).toPrecision(4)`, `"123.5"`},
		{`(123.456).toPrecision(3)`, `"123"`},
		{`(0).toPrecision(3)`, `"0.00"`},
		{`(1e21).toPrecision(3)`, `"1.00e+21"`},
		{`(NaN).toPrecision(3)`, `"NaN"`},
		{`(123).toPrecision()`, `"123"`},
		{`(999.5).toPrecision(3)`, `"1.00e+3"`},
		{`(-1.5).toPrecision(1)`, `"-2"`},
		{`(2.5).toPrecision(1)`, `"3"`},
		{`T(() => (1).toPrecision(0))`, `RangeError: toPrecision() argument must be between 1 and 100`},
		{`T(() => (1).toPrecision(101))`, `RangeError: toPrecision() argument must be between 1 and 100`},
		// toExponential
		{`(123.456).toExponential(2)`, `"1.23e+2"`},
		{`(0).toExponential()`, `"0e+0"`},
		{`(0).toExponential(2)`, `"0.00e+0"`},
		{`(123456).toExponential()`, `"1.23456e+5"`},
		{`(0.00001).toExponential()`, `"1e-5"`},
		{`(-1.5).toExponential(0)`, `"-2e+0"`},
		{`(2.5).toExponential(0)`, `"3e+0"`},
		{`(NaN).toExponential(2)`, `"NaN"`},
		{`(Infinity).toExponential(2)`, `"Infinity"`},
		{`T(() => (1).toExponential(101))`, `RangeError: toExponential() argument must be between 0 and 100`},
		{`T(() => (1).toExponential(-1))`, `RangeError: toExponential() argument must be between 0 and 100`},
		{`T(() => (NaN).toExponential(-1))`, `ok:"NaN"`},
		{`T(() => (NaN).toPrecision(-1))`, `ok:"NaN"`},
		{`T(() => (NaN).toFixed(-1))`, `RangeError: toFixed() digits argument must be between 0 and 100`},
		// Number() conversions.
		{`Number.parseFloat("1e")`, `1`},
		{`parseInt("0x")`, `NaN`},
		{`parseInt("  -0x10")`, `-16`},
		{`Number("")`, `0`},
		{`Number(" 12 ")`, `12`},
		{`Number("0b101")`, `5`},
		{`Number("0o17")`, `15`},
		{`Number("0X1F")`, `31`},
		{`Number("1_000")`, `NaN`},
		{`Number("Infinity")`, `Infinity`},
		{`Number("-Infinity")`, `-Infinity`},
		{`Number("+Infinity")`, `Infinity`},
		{`Number("infinity")`, `NaN`},
		{`Number("-0")`, `-0`},
		{`Number("0x1g")`, `NaN`},
		{`Number("-0x10")`, `NaN`},
		{`Number(".5")`, `0.5`},
		{`Number("5.")`, `5`},
		{`Number(".")`, `NaN`},
		{`Number("+.5e-3")`, `0.0005`},
		{`Number("1e1000")`, `Infinity`},
		{`Number("-1e1000")`, `-Infinity`},
		{`Number("1e-1000")`, `0`},
		{`Number("  1 \uFEFF")`, `1`},
		{`Number("        　\t\n\v\f\r 7")`, `7`},
		{`Number("᠎7")`, `NaN`},
		{`Number("​7")`, `NaN`},
		{`Number(null)`, `0`},
		{`Number(undefined)`, `NaN`},
		{`Number([])`, `0`},
		{`Number([5])`, `5`},
		{`Number([1,2])`, `NaN`},
		{`Number({})`, `NaN`},
		{`Number(true)`, `1`},
		{`Number(false)`, `0`},
		{`Number("12abc")`, `NaN`},
		{`Number("0x")`, `NaN`},
		{`Number("0b")`, `NaN`},
		{`Number("0b2")`, `NaN`},
		{`Number("1e+")`, `NaN`},
		{`Number("+-1")`, `NaN`},
		{`Number("0.0000001")`, `1e-7`},
		{`Number("0xFFFFFFFFFFFFFFFFF")`, `295147905179352830000`},
		{`Number("0x20000000000001")`, `9007199254740992`},
		{`Number("0x20000000000003")`, `9007199254740996`},
		{`Number()`, `0`},
		{`Number("1n")`, `NaN`},
		{`Number(1n)`, `1`},
		// Math
		{`Math.max()`, `-Infinity`},
		{`Math.min()`, `Infinity`},
		{`Math.max(NaN, 1)`, `NaN`},
		{`Math.max(1, NaN)`, `NaN`},
		{`Math.min(0, -0)`, `-0`},
		{`Math.min(-0, 0)`, `-0`},
		{`Math.max(0, -0)`, `0`},
		{`Math.max(-0, 0)`, `0`},
		{`Math.max(-0, -0)`, `-0`},
		{`Math.round(-0.5)`, `-0`},
		{`Math.round(-0)`, `-0`},
		{`Math.round(0.49999999999999994)`, `0`},
		{`Math.round(2.5)`, `3`},
		{`Math.round(-2.5)`, `-2`},
		{`Math.round(-1.5)`, `-1`},
		{`Math.round(0.5)`, `1`},
		{`Math.round(-0.4)`, `-0`},
		{`Math.round(4503599627370495.5)`, `4503599627370496`},
		{`Math.round(-4503599627370495.5)`, `-4503599627370495`},
		{`Math.round(2**52 + 1)`, `4503599627370497`},
		{`Math.sign(-0)`, `-0`},
		{`Math.sign(0)`, `0`},
		{`Math.sign(-3)`, `-1`},
		{`Math.sign(NaN)`, `NaN`},
		{`Math.hypot()`, `0`},
		{`Math.hypot(3, 4)`, `5`},
		{`Math.hypot(NaN, Infinity)`, `Infinity`},
		{`Math.hypot(-0)`, `0`},
		{`Math.hypot(NaN)`, `NaN`},
		{`Math.atan2(1, 1)`, `0.7853981633974483`},
		{`Math.atan2(0, -0)`, `3.141592653589793`},
		{`Math.atan2(-0, -0)`, `-3.141592653589793`},
		{`Math.atan2(-0, 0)`, `-0`},
		{`Math.pow(NaN, 0)`, `1`},
		{`Math.pow(1, Infinity)`, `NaN`},
		{`Math.pow(-1, -Infinity)`, `NaN`},
		{`Math.pow(1, NaN)`, `NaN`},
		{`Math.pow(0, -1)`, `Infinity`},
		{`Math.pow(-0, -1)`, `-Infinity`},
		{`Math.pow(-0, -2)`, `Infinity`},
		{`Math.pow(-0, 3)`, `-0`},
		{`Math.pow(-Infinity, 3)`, `-Infinity`},
		{`Math.pow(-Infinity, -3)`, `-0`},
		{`Math.pow(-Infinity, 2)`, `Infinity`},
		{`Math.pow(0.5, Infinity)`, `0`},
		{`Math.pow(2, -1074)`, `5e-324`},
		{`Math.pow(10, -7)`, `1e-7`},
		{`Math.pow(10, 21)`, `1e+21`},
		{`(-8) ** (1/3)`, `NaN`},
		{`(-8) ** 3`, `-512`},
		{`2 ** -1`, `0.5`},
		{`Math.trunc(-0.9)`, `-0`},
		{`Math.trunc(0.9)`, `0`},
		{`Math.trunc(-1.9)`, `-1`},
		{`Math.abs(-0)`, `0`},
		{`Math.abs(-Infinity)`, `Infinity`},
		{`Math.abs("-3")`, `3`},
		{`Math.abs(null)`, `0`},
		{`Math.abs()`, `NaN`},
		{`Math.fround(5.5)`, `5.5`},
		{`Math.fround(5.05)`, `5.050000190734863`},
		{`Math.fround(2**150)`, `Infinity`},
		{`Math.fround(-0)`, `-0`},
		{`Math.fround(NaN)`, `NaN`},
		{`Math.fround(2**-150)`, `0`},
		{`Math.clz32(0)`, `32`},
		{`Math.clz32(1)`, `31`},
		{`Math.clz32(-1)`, `0`},
		{`Math.clz32(0.5)`, `32`},
		{`Math.clz32(2**32)`, `32`},
		{`Math.imul(0xffffffff, 5)`, `-5`},
		{`Math.imul(2**31, 2)`, `0`},
		{`Math.imul(3, 4)`, `12`},
		{`Math.imul(0x7fffffff, 0x7fffffff)`, `1`},
		{`Math.imul()`, `0`},
		{`Math.ceil(-0.5)`, `-0`},
		{`Math.floor(-0)`, `-0`},
		{`Math.sqrt(-0)`, `-0`},
		{`Math.sqrt(-1)`, `NaN`},
		{`Math.cbrt(-8)`, `-2`},
		{`Math.cbrt(-0)`, `-0`},
		{`Math.log10(1000)`, `3`},
		{`Math.log10(1e15)`, `15`},
		{`Math.log2(8)`, `3`},
		{`Math.log2(2**-1074)`, `-1074`},
		{`Math.log(-1)`, `NaN`},
		{`Math.log(0)`, `-Infinity`},
		{`Math.log1p(-1)`, `-Infinity`},
		{`Math.log1p(-2)`, `NaN`},
		{`Math.expm1(-0)`, `-0`},
		{`Math.exp(-Infinity)`, `0`},
		{`Math.acosh(0.5)`, `NaN`},
		{`Math.atanh(1)`, `Infinity`},
		{`Math.atanh(-0)`, `-0`},
		{`Math.asinh(-0)`, `-0`},
		{`Math.sinh(-0)`, `-0`},
		{`Math.tanh(Infinity)`, `1`},
		{`Math.cosh(0)`, `1`},
		{`Math.sin(-0)`, `-0`},
		{`Math.cos(Infinity)`, `NaN`},
		{`Math.tan(-0)`, `-0`},
		{`Math.asin(2)`, `NaN`},
		{`Math.acos(1)`, `0`},
		{`Math.atan(-0)`, `-0`},
		{`Math.atan(Infinity)`, `1.5707963267948966`},
		{`Math.max("3", 2)`, `3`},
		{`Math.min("a", 2)`, `NaN`},
		{`T(() => Math.max({valueOf() { throw new Error("v") }}, NaN))`, `Error: v`},
		{`(() => { const log = []; Math.max({valueOf() { log.push(1); return NaN }}, {valueOf() { log.push(2); return 1 }}); return log })()`, `[1,2]`},
		{`Math.PI`, `3.141592653589793`},
		{`Math.SQRT1_2`, `0.7071067811865476`},
		{`Math.LN2`, `0.6931471805599453`},
		{`typeof Math.random()`, `"number"`},
		// Object
		{`(() => { const t = {}; const s = Object.create({inherited: 1}); s.own = 2; Object.defineProperty(s, 'hidden', {value: 3, enumerable: false}); Object.defineProperty(s, 'g', {get() { return 4 }, enumerable: true}); Object.assign(t, s, null, undefined, "ab", 5); return t })()`, `{"0":"a","1":"b","own":2,"g":4}`},
		{`Object.keys(Object.assign({}, "ab"))`, `["0","1"]`},
		{`Object.assign({}, null, undefined) !== null`, `true`},
		{`T(() => Object.assign(null, {}))`, `TypeError: Cannot convert undefined or null to object`},
		{`T(() => Object.assign(Object.freeze({a: 1}), {a: 2}))`, `TypeError: Cannot assign to read only property 'a' of object`},
		{`Object.fromEntries([["a", 1], ["b", 2], ["a", 3]])`, `{"a":3,"b":2}`},
		{`Object.keys(Object.fromEntries([["a", 1], ["b", 2], ["a", 3]]))`, `["a","b"]`},
		{`Object.fromEntries([[1, "x"], ["1", "y"]])`, `{"1":"y"}`},
		{`T(() => Object.fromEntries([1]))`, `TypeError: Iterator value 1 is not an entry object`},
		{`T(() => Object.fromEntries(null))`, `TypeError: Object.fromEntries requires an iterable, got null`},
		{`(() => { const o = Object.create(null); o.x = 1; return ['x' in o, 'toString' in o, Object.getPrototypeOf(o)] })()`, `[true,false,null]`},
		{`(() => { const a = Object.freeze([1, 2]); return [Object.isFrozen(a), Object.getOwnPropertyNames(a)] })()`, `[true,["0","1","length"]]`},
		{`T(() => { const a = Object.freeze([1, 2]); a[0] = 9 })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => { const a = Object.freeze([1, 2]); a.length = 1 })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = Object.freeze([1, 2]); a[5] = 1 })`, `TypeError: Cannot assign to read only property '5' of object`},
		{`T(() => { const a = Object.freeze([1, 2]); delete a[0] })`, `TypeError: Cannot delete property '0' of [object Array]`},
		{`T(() => { const a = Object.freeze([1, 2]); a.pop() })`, `TypeError: Cannot delete property '1' of [object Array]`},
		{`T(() => { const a = Object.freeze([1, 2]); a.shift() })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => { const a = Object.freeze([1, 2]); a.reverse() })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => { const a = Object.freeze([1, 2]); a.sort() })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => { const a = Object.freeze([2, 1]); a.sort() })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => { const a = Object.freeze([1, 2]); a.fill(0) })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => { const a = Object.freeze([1, 2]); a.splice(0, 1) })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => { const a = Object.freeze([1, 2]); a.unshift(0) })`, `TypeError: Cannot assign to read only property '2' of object`},
		{`T(() => { const a = Object.freeze([]); a.push(1) })`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => { const a = Object.freeze([]); return a.fill(0) })`, `ok:[]`},
		{`Object.isFrozen([])`, `false`},
		{`Object.isFrozen(Object.freeze([]))`, `true`},
		{`Object.isFrozen({})`, `false`},
		{`Object.isFrozen(1)`, `true`},
		{`Object.isFrozen("s")`, `true`},
		{`Object.isFrozen(Object.freeze({a: 1}))`, `true`},
		{`Object.freeze(1)`, `1`},
		{`Object.getPrototypeOf(1) === Number.prototype`, `true`},
		{`Object.getPrototypeOf("s") === String.prototype`, `true`},
		{`Object.getPrototypeOf(true) === Boolean.prototype`, `true`},
		{`T(() => Object.getPrototypeOf(null))`, `TypeError: Cannot convert undefined or null to object`},
		{`Object.getPrototypeOf(Object.prototype)`, `null`},
		{`(() => { const o = {}; Object.setPrototypeOf(o, null); return [Object.getPrototypeOf(o), 'toString' in o] })()`, `[null,false]`},
		{`T(() => Object.setPrototypeOf({}, 1))`, `TypeError: Object prototype may only be an Object or null: 1`},
		{`T(() => Object.setPrototypeOf(null, {}))`, `TypeError: Object.setPrototypeOf called on null or undefined`},
		{`Object.setPrototypeOf(1, null)`, `1`},
		{`T(() => Object.setPrototypeOf(Object.freeze({}), {}))`, `TypeError: [object Object] is not extensible`},
		{`Object.setPrototypeOf(Object.freeze({}), Object.prototype) !== null`, `true`},
		{`T(() => { const a = {}; const b = Object.create(a); Object.setPrototypeOf(a, b) })`, `TypeError: Cyclic __proto__ value`},
		{`(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1}); return [o.x, Object.keys(o), JSON.stringify(o), T(() => delete o.x), o.x] })()`, `[1,[],"{}","TypeError: Cannot delete property 'x' of [object Object]",1]`},
		{`T(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1}); o.x = 2 })`, `TypeError: Cannot assign to read only property 'x' of object`},
		{`T(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1}); Object.defineProperty(o, 'x', {value: 2}) })`, `TypeError: Cannot redefine property: x`},
		{`T(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1}); Object.defineProperty(o, 'x', {value: 1}); return o.x })`, `ok:1`},
		{`T(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1}); Object.defineProperty(o, 'x', {enumerable: true}) })`, `TypeError: Cannot redefine property: x`},
		{`T(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1}); Object.defineProperty(o, 'x', {}); return o.x })`, `ok:1`},
		{`T(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1, writable: true}); Object.defineProperty(o, 'x', {value: 2}); Object.defineProperty(o, 'x', {writable: false}); return o.x })`, `ok:2`},
		{`T(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1, writable: true}); Object.defineProperty(o, 'x', {writable: false}); Object.defineProperty(o, 'x', {writable: true}) })`, `TypeError: Cannot redefine property: x`},
		{`T(() => Object.defineProperty(1, 'x', {value: 1}))`, `TypeError: Object.defineProperty called on non-object`},
		{`T(() => Object.defineProperty({}, 'x', 1))`, `TypeError: Property description must be an object: 1`},
		{`T(() => Object.defineProperty({}, 'x', {value: 1, get() {}}))`, `TypeError: Invalid property descriptor. Cannot both specify accessors and a value or writable attribute`},
		{`T(() => Object.defineProperty({}, 'x', {get: 1}))`, `TypeError: Getter must be a function: 1`},
		{`(() => { const o = {a: 1}; Object.defineProperty(o, 'a', {value: 2}); return [o.a, Object.keys(o), delete o.a] })()`, `[2,["a"],true]`},
		{`(() => { const o = {}; Object.defineProperty(o, 'x', {value: 1, writable: true, enumerable: true, configurable: true}); o.x = 5; return [o.x, Object.keys(o), delete o.x] })()`, `[5,["x"],true]`},
		{`(() => { const o = {}; Object.defineProperty(o, 'x', {get() { return 42 }}); return [o.x, Object.keys(o), typeof Object.getOwnPropertyNames(o)] })()`, `[42,[],"object"]`},
		{`(() => { const o = {}; let v = 0; Object.defineProperty(o, 'x', {set(n) { v = n * 2 }}); o.x = 4; return [o.x, v] })()`, `[null,8]`},
		{`T(() => { 'use strict'; const o = {}; Object.defineProperty(o, 'x', {get() { return 1 }}); o.x = 2 })`, `TypeError: Cannot assign to read only property 'x' of object`},
		{`Object.hasOwn({a: 1}, 'a')`, `true`},
		{`Object.hasOwn({a: 1}, 'toString')`, `false`},
		{`Object.hasOwn([1], 0)`, `true`},
		{`Object.hasOwn([1], 'length')`, `true`},
		{`Object.hasOwn("ab", 1)`, `true`},
		{`Object.hasOwn("ab", 2)`, `false`},
		{`T(() => Object.hasOwn(null, 'a'))`, `TypeError: Cannot convert undefined or null to object`},
		{`Object.keys(1)`, `[]`},
		{`T(() => Object.keys(null))`, `TypeError: Cannot convert undefined or null to object`},
		{`Object.prototype.toString.call([])`, `"[object Array]"`},
		{`Object.prototype.toString.call(null)`, `"[object Null]"`},
		{`Object.prototype.toString.call(undefined)`, `"[object Undefined]"`},
		{`Object.prototype.toString.call(1)`, `"[object Number]"`},
		{`Object.prototype.toString.call("s")`, `"[object String]"`},
		{`Object.prototype.toString.call(true)`, `"[object Boolean]"`},
		{`Object.prototype.toString.call(new TypeError())`, `"[object Error]"`},
		{`Object.prototype.toString.call(() => 1)`, `"[object Function]"`},
		{`Object.prototype.toString.call(/x/)`, `"[object RegExp]"`},
		{`Object.prototype.toString.call(new Date(0))`, `"[object Date]"`},
		{`Object.prototype.toString.call(Math)`, `"[object Math]"`},
		{`Object.prototype.toString.call(JSON)`, `"[object JSON]"`},
		{`Object.prototype.toString.call([].values())`, `"[object Array Iterator]"`},
		{`Object.prototype.hasOwnProperty.call("ab", "length")`, `true`},
		{`Object.prototype.hasOwnProperty.call("ab", 1)`, `true`},
		{`Object.prototype.hasOwnProperty.call("ab", 2)`, `false`},
		{`Object.prototype.hasOwnProperty.call(1, "toString")`, `false`},
		{`T(() => Object.prototype.hasOwnProperty.call(null, "a"))`, `TypeError: Cannot convert undefined or null to object`},
		{`T(() => Object.prototype.hasOwnProperty.call(null, {toString() { throw new Error("key first") }}))`, `Error: key first`},
		{`Object.prototype.propertyIsEnumerable.call([1], 0)`, `true`},
		{`Object.prototype.propertyIsEnumerable.call([1], 'length')`, `false`},
		{`Object.prototype.propertyIsEnumerable.call("ab", 0)`, `true`},
		{`Object.prototype.propertyIsEnumerable.call("ab", 'length')`, `false`},
		{`Object.prototype.isPrototypeOf.call(Array.prototype, [])`, `true`},
		{`Object.prototype.isPrototypeOf.call(Array.prototype, 1)`, `false`},
		{`T(() => Object.prototype.isPrototypeOf.call(null, {}))`, `TypeError: Cannot convert undefined or null to object`},
		{`Object.prototype.isPrototypeOf.call(null, 1)`, `false`},
		{`Object.prototype.valueOf.call(1) instanceof Number`, `true`},
		{`({}).toLocaleString()`, `"[object Object]"`},
		{`[1, [2, 3]].toLocaleString()`, `"1,2,3"`},
		{`Object(1) instanceof Number`, `true`},
		{`Object(null) !== null && typeof Object(null)`, `"object"`},
		{`Object("s").length`, `1`},
		{`Object.create({}, {x: {value: 1, enumerable: true}})`, `{"x":1}`},
		{`T(() => Object.create(1))`, `TypeError: Object prototype may only be an Object or null: 1`},
		{`T(() => Object.create(undefined))`, `TypeError: Object prototype may only be an Object or null: undefined`},
		{`T(() => Object.create({}, {x: 1}))`, `TypeError: Property description must be an object: 1`},
		// Error
		{`new Error("m", {cause: 1}).cause`, `1`},
		{`'cause' in new Error("m", {})`, `false`},
		{`'cause' in new Error("m", {cause: undefined})`, `true`},
		{`Object.keys(new Error("m", {cause: 1}))`, `[]`},
		{`new Error("m").toString()`, `"Error: m"`},
		{`new Error().toString()`, `"Error"`},
		{`(() => { const e = new Error(); e.name = ""; return e.toString() })()`, `""`},
		{`(() => { const e = new Error("m"); e.name = ""; return e.toString() })()`, `"m"`},
		{`(() => { const e = new Error(""); e.name = "X"; return e.toString() })()`, `"X"`},
		{`(() => { const e = new Error("m"); e.name = undefined; return e.toString() })()`, `"Error: m"`},
		{`(() => { const e = new Error(undefined); e.message = undefined; return e.toString() })()`, `"Error"`},
		{`Error.prototype.toString.call({name: "N", message: "M"})`, `"N: M"`},
		{`Error.prototype.toString.call({})`, `"Error"`},
		{`T(() => Error.prototype.toString.call(1))`, `TypeError: Error.prototype.toString requires that 'this' be an Object`},
		{`new TypeError() instanceof Error`, `true`},
		{`new TypeError() instanceof TypeError`, `true`},
		{`new RangeError() instanceof TypeError`, `false`},
		{`Error("x") instanceof Error`, `true`},
		{`Error("x").message`, `"x"`},
		{`TypeError("x").message`, `"x"`},
		{`new TypeError("msg").stack.split("\n")[0]`, `"TypeError: msg"`},
		{`new Error().stack.split("\n")[0]`, `"Error"`},
		{`RangeError.prototype.name`, `"RangeError"`},
		{`RangeError.prototype.message`, `""`},
		{`Object.getPrototypeOf(RangeError.prototype) === Error.prototype`, `true`},
		{`Object.getPrototypeOf(RangeError) === Error`, `true`},
		{`Error.prototype.name`, `"Error"`},
		{`Object.keys(new Error("m"))`, `[]`},
		{`Object.getOwnPropertyNames(new Error("m"))`, `["message","stack"]`},
		{`Object.getOwnPropertyNames(new Error())`, `["stack"]`},
		{`new Error(1).message`, `"1"`},
		{`new Error(undefined).hasOwnProperty("message")`, `false`},
		{`new Error(null).message`, `"null"`},
		{`(() => { const e = new Error("m"); e.message = "n"; return e.toString() })()`, `"Error: n"`},
		{`Object.prototype.hasOwnProperty.call(new Error("m"), "message")`, `true`},
		{`EvalError.prototype.name`, `"EvalError"`},
		{`URIError.prototype.name`, `"URIError"`},
		{`SyntaxError.prototype.name`, `"SyntaxError"`},
		{`ReferenceError.prototype.name`, `"ReferenceError"`},
		{`Error.length`, `1`},
		{`TypeError.length`, `1`},
		{`Error.name`, `"Error"`},
		{`T(() => new Error({toString() { throw new Error("ts") }}))`, `Error: ts`},
		{`T(() => { const o = {}; Object.defineProperty(o, 'cause', {get() { throw new Error("c") }}); return new Error("m", o) })`, `Error: c`},
		// Function
		{`(function (a, b = 1, c) {}).length`, `1`},
		{`(function (a, ...r) {}).length`, `1`},
		{`((a, b) => {}).length`, `2`},
		{`(function () {}).name`, `""`},
		{`(() => {}).name`, `""`},
		{`(() => { const f = () => {}; return f.name })()`, `"f"`},
		{`(() => { function f() {}; return f.bind(null).name })()`, `"bound f"`},
		{`(() => { function f() {}; return f.bind(null).bind(null).name })()`, `"bound bound f"`},
		{`(() => { function f(a, b, c) {}; return [f.bind(null).length, f.bind(null, 1).length, f.bind(null, 1, 2, 3, 4).length] })()`, `[3,2,0]`},
		{`(() => { function f() {}; Object.defineProperty(f, 'length', {value: Infinity}); return f.bind(null, 1).length })()`, `Infinity`},
		{`(() => { function f() {}; Object.defineProperty(f, 'length', {value: -Infinity}); return f.bind(null, 1).length })()`, `0`},
		{`(() => { function f() {}; Object.defineProperty(f, 'length', {value: "3"}); return f.bind(null, 1).length })()`, `0`},
		{`(() => { function f() {}; Object.defineProperty(f, 'length', {value: 2.7}); return f.bind(null, 1).length })()`, `1`},
		{`(() => { function f() {}; Object.defineProperty(f, 'name', {value: 7}); return f.bind(null).name })()`, `"bound "`},
		{`(() => { function f() { return this }; return f.call(null) })()`, `null`},
		{`(() => { function f() { return this }; return f.call(undefined) })()`, `undefined`},
		{`(() => { function f() { return typeof this }; return f.call(1) })()`, `"number"`},
		{`(() => { function f() { return this }; return f.apply(null) })()`, `null`},
		{`(() => { function f() { return [].slice.call(arguments_) }; function g(...arguments_) { return arguments_ }; return g.apply(null, {length: 2, 0: 'a', 1: 'b'}) })()`, `["a","b"]`},
		{`(() => { function g(...a) { return a }; return g.apply(null, null) })()`, `[]`},
		{`(() => { function g(...a) { return a }; return g.apply(null, undefined) })()`, `[]`},
		{`T(() => { function g(...a) { return a }; return g.apply(null, 1) })`, `TypeError: CreateListFromArrayLike called on non-object`},
		{`(() => { function g(...a) { return a }; return g.apply(null, {length: 3, 0: 1}) })()`, `[1,null,null]`},
		{`T(() => { function g(...a) { return a }; return g.apply(null, "ab") })`, `TypeError: CreateListFromArrayLike called on non-object`},
		{`(() => { function g(...a) { return a }; return g.apply(null, {length: -1}) })()`, `[]`},
		{`(() => { function f(a, b) { return [this, a, b] }; return f.bind(7, 1)(2) })()`, `[7,1,2]`},
		{`(() => { function F(a, b) { this.v = [a, b] }; const B = F.bind({ignored: true}, 1); const o = new B(2); return [o.v, o instanceof F, o instanceof B] })()`, `[[1,2],true,true]`},
		{`(() => { function F() {}; F.prototype.x = 1; const B = F.bind(null); return [B.prototype, new B().x, Object.getPrototypeOf(B) === Function.prototype] })()`, `[null,1,true]`},
		{`(() => { const B = (() => 1).bind(null); return T(() => new B()) })()`, `"TypeError: B is not a constructor"`},
		{`(() => { function f(a) { return a }; return f.toString() })()`, `"function f(a) { return a }"`},
		{`(x => x).toString()`, `"x => x"`},
		{`Math.max.toString()`, `"function max() { [native code] }"`},
		{`(function f() {}).bind(null).toString()`, `"function () { [native code] }"`},
		{`Function.prototype.toString()`, `"function () { [native code] }"`},
		{`typeof Function.prototype`, `"function"`},
		{`Function.prototype()`, `undefined`},
		{`Function.prototype.name`, `""`},
		{`Function.prototype.length`, `0`},
		{`Object.getPrototypeOf(Function.prototype) === Object.prototype`, `true`},
		{`Object.getPrototypeOf(Function) === Function.prototype`, `true`},
		{`Object.getPrototypeOf(Object) === Function.prototype`, `true`},
		{`Object.getPrototypeOf(Array) === Function.prototype`, `true`},
		{`Object.getPrototypeOf(Error) === Function.prototype`, `true`},
		{`T(() => Function.prototype.call.call(1))`, `TypeError: Function.prototype.call was called on 1, which is not a function`},
		{`T(() => Function.prototype.apply.call({}))`, `TypeError: Function.prototype.apply was called on [object Object], which is not a function`},
		{`T(() => Function.prototype.bind.call(1))`, `TypeError: Bind must be called on a function`},
		{`T(() => Function.prototype.toString.call(1))`, `TypeError: Function.prototype.toString requires that 'this' be a Function`},
		{`T(() => new Function("a", "return a"))`, `EvalError: code generation from strings is not available: no compiler is installed (engine.SetCompiler)`},
		{`T(() => Function("a", "return a"))`, `EvalError: code generation from strings is not available: no compiler is installed (engine.SetCompiler)`},
		{`(() => { function f(a, b) {}; return [Object.getOwnPropertyNames(f), f.hasOwnProperty('prototype')] })()`, `[["length","name","prototype"],true]`},
		{`(() => { const f = (a, b) => {}; return Object.getOwnPropertyNames(f) })()`, `["length","name"]`},
		{`(() => { function f() {}; return Object.getOwnPropertyNames(f.bind(null)) })()`, `["length","name"]`},
		{`Object.getOwnPropertyNames(Math.max)`, `["length","name"]`},
		{`Object.getOwnPropertyNames(Array)`, `["length","name","prototype","from","fromAsync","isArray","of"]`},
		{`T(() => { function f() {}; f.length = 5; return f.length })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`(() => { function f() {}; return [delete f.length, f.length, delete f.name, f.name] })()`, `[true,0,true,""]`},
		// Boolean
		{`!!new Boolean(false)`, `true`},
		{`new Boolean(false).valueOf()`, `false`},
		{`new Boolean(false) == false`, `true`},
		{`T(() => Boolean.prototype.valueOf.call(1))`, `TypeError: Boolean.prototype.valueOf requires that 'this' be a Boolean`},
		{`T(() => Boolean.prototype.toString.call({}))`, `TypeError: Boolean.prototype.toString requires that 'this' be a Boolean`},
		{`Boolean.prototype.toString.call(true)`, `"true"`},
		{`Boolean.prototype.valueOf.call(new Boolean(1))`, `true`},
		{`Boolean()`, `false`},
		{`Boolean("")`, `false`},
		{`Boolean("0")`, `true`},
		{`Boolean(-0)`, `false`},
		{`Boolean(NaN)`, `false`},
		{`Boolean([])`, `true`},
		{`Boolean(0n)`, `false`},
		{`Boolean.prototype.valueOf()`, `false`},
		{`typeof new Boolean(true)`, `"object"`},
		{`new Boolean(true) instanceof Boolean`, `true`},
		{`Boolean.length`, `1`},
		{`Number.length`, `1`},
		{`String(new Boolean(false))`, `"false"`},
		// Global functions
		{`isNaN("abc")`, `true`},
		{`isNaN("12")`, `false`},
		{`isNaN(undefined)`, `true`},
		{`isNaN()`, `true`},
		{`isNaN(null)`, `false`},
		{`isFinite("1")`, `true`},
		{`isFinite(Infinity)`, `false`},
		{`isFinite(null)`, `true`},
		{`isFinite()`, `false`},
		{`isFinite("abc")`, `false`},
		{`parseInt(" 0x1f", 16)`, `31`},
		{`parseInt("0x1f", 10)`, `0`},
		{`parseInt("0x1f", 0)`, `31`},
		{`parseInt("1e3")`, `1`},
		{`parseInt(null)`, `NaN`},
		{`parseInt("null", 36)`, `1112745`},
		{`parseInt("123", 1)`, `NaN`},
		{`parseInt("123", 37)`, `NaN`},
		{`parseInt("11", 2)`, `3`},
		{`parseInt("11", 2.9)`, `3`},
		{`parseInt("11", "2")`, `3`},
		{`parseInt("11", 4294967298)`, `3`},
		{`parseInt("11", -4294967294)`, `3`},
		{`parseInt("11", 36)`, `37`},
		{`parseInt("z", 36)`, `35`},
		{`parseInt("Z", 36)`, `35`},
		{`parseInt("12", 2)`, `1`},
		{`parseInt("-0")`, `-0`},
		{`parseInt("+0")`, `0`},
		{`parseInt("-")`, `NaN`},
		{`parseInt("- 1")`, `NaN`},
		{`parseInt("")`, `NaN`},
		{`parseInt("   ")`, `NaN`},
		{`parseInt("  42")`, `42`},
		{`parseInt("0x")`, `NaN`},
		{`parseInt("0x", 16)`, `NaN`},
		{`parseInt("0b11")`, `0`},
		{`parseInt("0o7")`, `0`},
		{`parseInt("08")`, `8`},
		{`parseInt("0.0000005")`, `0`},
		{`parseInt(0.0000005)`, `5`},
		{`parseInt(1e21)`, `1`},
		{`parseInt("123456789012345678901234567890")`, `1.2345678901234568e+29`},
		{`parseInt("9007199254740993")`, `9007199254740992`},
		{`parseInt("9007199254740993", 10)`, `9007199254740992`},
		{`parseInt("ffffffffffffffffff", 16)`, `4.722366482869645e+21`},
		{`parseInt("1" + "0".repeat(400))`, `Infinity`},
		{`parseInt("1" + "0".repeat(400), 16)`, `Infinity`},
		{`parseInt("1111111111111111111111111111111111111111111111111111111111111111", 2)`, `18446744073709552000`},
		{`parseInt("-ff", 16)`, `-255`},
		{`parseInt("0X1F")`, `31`},
		{`parseInt("  12  ")`, `12`},
		{`parseInt("12px")`, `12`},
		{`parseInt("١٢")`, `NaN`},
		{`parseInt("Infinity")`, `NaN`},
		{`parseInt({toString() { return "7" }})`, `7`},
		{`T(() => parseInt({toString() { throw new Error("s") }}, {valueOf() { throw new Error("r") }}))`, `Error: s`},
		{`parseFloat("-.5")`, `-0.5`},
		{`parseFloat("Infinityx")`, `Infinity`},
		{`parseFloat("-Infinity")`, `-Infinity`},
		{`parseFloat("+Infinity")`, `Infinity`},
		{`parseFloat("infinity")`, `NaN`},
		{`parseFloat("1e")`, `1`},
		{`parseFloat("1e+")`, `1`},
		{`parseFloat("1e-2x")`, `0.01`},
		{`parseFloat("1.")`, `1`},
		{`parseFloat(".")`, `NaN`},
		{`parseFloat("-.")`, `NaN`},
		{`parseFloat("-")`, `NaN`},
		{`parseFloat("")`, `NaN`},
		{`parseFloat("  1.5.3")`, `1.5`},
		{`parseFloat("0x10")`, `0`},
		{`parseFloat("1_0")`, `1`},
		{`parseFloat("-0")`, `-0`},
		{`parseFloat("-0.0e5")`, `-0`},
		{`parseFloat("1e1000")`, `Infinity`},
		{`parseFloat("1e-1000")`, `0`},
		{`parseFloat("  2.5")`, `2.5`},
		{`parseFloat("١")`, `NaN`},
		{`parseFloat(null)`, `NaN`},
		{`parseFloat("1e3")`, `1000`},
		{`parseFloat("1E3")`, `1000`},
		{`parseFloat("0.0000001")`, `1e-7`},
		{`parseFloat("١٢٣.5")`, `NaN`},
		{`parseFloat("1٢")`, `1`},
		{`parseFloat(" -1")`, `-1`},
		// Number statics
		{`Number.isInteger(5.0)`, `true`},
		{`Number.isInteger("5")`, `false`},
		{`Number.isInteger(Infinity)`, `false`},
		{`Number.isSafeInteger(2**53)`, `false`},
		{`Number.isSafeInteger(2**53 - 1)`, `true`},
		{`Number.isSafeInteger(-(2**53 - 1))`, `true`},
		{`Number.isNaN("NaN")`, `false`},
		{`Number.isNaN(NaN)`, `true`},
		{`Number.isFinite("1")`, `false`},
		{`Number.isFinite(1)`, `true`},
		{`Number.parseInt === parseInt`, `true`},
		{`Number.parseFloat === parseFloat`, `true`},
		{`Number.EPSILON`, `2.220446049250313e-16`},
		{`Number.MAX_SAFE_INTEGER`, `9007199254740991`},
		{`Number.MIN_SAFE_INTEGER`, `-9007199254740991`},
		{`Number.MAX_VALUE`, `1.7976931348623157e+308`},
		{`Number.MIN_VALUE`, `5e-324`},
		{`Number.NEGATIVE_INFINITY`, `-Infinity`},
		{`Number.POSITIVE_INFINITY`, `Infinity`},
		{`Number.NaN`, `NaN`},
		{`T(() => { Number.MAX_VALUE = 1; return Number.MAX_VALUE })`, `TypeError: Cannot assign to read only property 'MAX_VALUE' of object`},
		{`T(() => Number.prototype.toString.call("1"))`, `TypeError: Number.prototype.toString requires that 'this' be a Number`},
		{`T(() => Number.prototype.valueOf.call({}))`, `TypeError: Number.prototype.valueOf requires that 'this' be a Number`},
		{`Number.prototype.toString.call(new Number(255), 16)`, `"ff"`},
		{`new Number(5).valueOf()`, `5`},
		{`typeof new Number(5)`, `"object"`},
		{`(1).toLocaleString()`, `"1"`},
		{`(1234.5).toLocaleString()`, `"1,234.5"`},
		{`new Number("0x10").valueOf()`, `16`},
		{`Number("12", "junk")`, `12`},
		{`Number("1e21")`, `1e+21`},
		{`String(1e21)`, `"1e+21"`},
		{`String(1e-7)`, `"1e-7"`},
		{`String(123e-20)`, `"1.23e-18"`},
		{`String(0.000001)`, `"0.000001"`},
		{`String(1e20)`, `"100000000000000000000"`},
		{`String(-1e21)`, `"-1e+21"`},
		{`String(2**53 + 2)`, `"9007199254740994"`},
		{`String(5e-324)`, `"5e-324"`},
		{`String(1.7976931348623157e308)`, `"1.7976931348623157e+308"`},
		{`String(0.1 + 0.2)`, `"0.30000000000000004"`},
		{`String(-0)`, `"0"`},
		{`String(123456789012345680000)`, `"123456789012345680000"`},
		{`String(1234567890123456800000)`, `"1.2345678901234568e+21"`},
		// Array misc
		{`[1,2,3].at(-1)`, `3`},
		{`[1,2,3].at(3)`, `undefined`},
		{`[1,2,3].at(-4)`, `undefined`},
		{`[1,2,3].at("1")`, `2`},
		{`[1,2,3].at(NaN)`, `1`},
		{`[1,2,3].at(1.9)`, `2`},
		{`[1,2,3].at(-0.5)`, `1`},
		{`[1,2,3].at(Infinity)`, `undefined`},
		{`[1,,3].indexOf(undefined)`, `-1`},
		{`[1,,3].includes(undefined)`, `true`},
		{`[NaN].indexOf(NaN)`, `-1`},
		{`[NaN].includes(NaN)`, `true`},
		{`[0].includes(-0)`, `true`},
		{`[1,2,3].indexOf(3, -1)`, `2`},
		{`[1,2,3].indexOf(1, -Infinity)`, `0`},
		{`[1,2,3].indexOf(1, Infinity)`, `-1`},
		{`[1,2,3].lastIndexOf(1, -Infinity)`, `-1`},
		{`[1,2,3].lastIndexOf(3, Infinity)`, `2`},
		{`[1,2,3].lastIndexOf(3, -1)`, `2`},
		{`[1,2,3].lastIndexOf(3, -2)`, `-1`},
		{`[1,2,3].lastIndexOf(1, undefined)`, `0`},
		{`[1,2,3].lastIndexOf(1)`, `0`},
		{`[1,2,3].includes(1, -Infinity)`, `true`},
		{`[1,2,1].lastIndexOf(1, -2)`, `0`},
		{`[1,[2,[3,[4]]]].flat(Infinity)`, `[1,2,3,4]`},
		{`[1,[2,[3,[4]]]].flat()`, `[1,2,[3,[4]]]`},
		{`[1,[2,[3,[4]]]].flat(0)`, `[1,[2,[3,[4]]]]`},
		{`[1,[2,[3,[4]]]].flat(-1)`, `[1,[2,[3,[4]]]]`},
		{`[1,,3].flat()`, `[1,3]`},
		{`[1,2].flatMap(x => [x, [x]])`, `[1,[1],2,[2]]`},
		{`[1,2].flatMap(x => x)`, `[1,2]`},
		{`T(() => [1].flatMap())`, `TypeError: flatMap mapper function is not callable`},
		{`T(() => [1].map())`, `TypeError: undefined is not a function`},
		{`T(() => [1].reduce((a, b) => a))`, `ok:1`},
		{`T(() => [].reduce((a, b) => a))`, `TypeError: Reduce of empty array with no initial value`},
		{`T(() => [,].reduce((a, b) => a))`, `TypeError: Reduce of empty array with no initial value`},
		{`[,1].reduce((a, b) => a + b)`, `1`},
		{`[1,2,3].reduceRight((a, b) => a + "" + b)`, `"321"`},
		{`[1,2,3].reduce((a, b) => a + b, "")`, `"123"`},
		{`[1,2,3].reduce((a, b, i, arr) => arr === undefined ? 0 : a + b)`, `6`},
		{`(() => { const a = [1,2,3]; a.reduce((acc, x) => { a.push(x); return acc }); return a.length })()`, `5`},
		{`(() => { const log = []; [1,,3].forEach((x, i) => log.push(i)); return log })()`, `[0,2]`},
		{`(() => { const log = []; [1,,3].map((x, i) => log.push(i)); return log })()`, `[0,2]`},
		{`(() => { const a = [1,,3].map(x => x); return [a.length, 1 in a] })()`, `[3,false]`},
		{`[1,,3].filter(() => true)`, `[1,3]`},
		{`[1,,3].some(x => x === undefined)`, `false`},
		{`[1,,3].every(x => x !== undefined)`, `true`},
		{`[1,,3].find(x => x === undefined)`, `undefined`},
		{`[1,,3].findIndex(x => x === undefined)`, `1`},
		{`[1,,3].findLast(x => x === 1)`, `1`},
		{`[1,,3].findLastIndex(x => x === undefined)`, `1`},
		{`[3,1,2].findLast(x => x < 3)`, `2`},
		{`[].findLastIndex(x => true)`, `-1`},
		{`(() => { const a = [1,2,3]; a.forEach((x, i) => { if (i === 0) a.push(9) }); return a })()`, `[1,2,3,9]`},
		{`(() => { const a = [1,2,3]; const log = []; a.forEach((x, i) => { log.push(x); if (i === 0) a.pop() }); return log })()`, `[1,2]`},
		{`[1,2,3].join()`, `"1,2,3"`},
		{`[1,null,undefined,3].join("-")`, `"1---3"`},
		{`[1,[2,[3]]].join()`, `"1,2,3"`},
		{`[].join()`, `""`},
		{`[,].join()`, `""`},
		{`[1,2].join(undefined)`, `"1,2"`},
		{`[1,2].join(null)`, `"1null2"`},
		{`[1,2].join(1)`, `"112"`},
		{`(() => { const a = [1]; a.push(a); return a.join() })()`, `"1,"`},
		{`String([1,[2,3]])`, `"1,2,3"`},
		{`[] + []`, `""`},
		{`[1,2].toString()`, `"1,2"`},
		{`Array.prototype.toString.call({join() { return "J" }})`, `"J"`},
		{`Array.prototype.toString.call({})`, `"[object Object]"`},
		{`Array.prototype.toString.call(1)`, `"[object Number]"`},
		{`Array.prototype.join.call({length: 2, 0: "a", 1: "b"})`, `"a,b"`},
		{`Array.prototype.join.call("abc", "-")`, `"a-b-c"`},
		{`Array.prototype.join.call({length: 3})`, `",,"`},
		{`Array.prototype.join.call({length: "2", 0: 1, 1: 2})`, `"1,2"`},
		{`Array.prototype.join.call({length: -5, 0: 1})`, `""`},
		{`[3,1,10,2].sort()`, `[1,10,2,3]`},
		{`[3,1,10,2].sort((a, b) => a - b)`, `[1,2,3,10]`},
		{`[3,undefined,1,,2].sort()`, `[1,2,3,null,null]`},
		{`(() => { const a = [3,undefined,1,,2].sort(); return [a.length, 4 in a, 3 in a] })()`, `[5,false,true]`},
		{`(() => { const a = [3,,1,,2]; a.sort(); return [a.length, 3 in a, 4 in a, a.join()] })()`, `[5,false,false,"1,2,3,,"]`},
		{`T(() => { const a = [3,undefined,1]; a.sort((x, y) => { throw new Error("never for undefined") }); return "no" })`, `Error: never for undefined`},
		{`[3,1].sort((x, y) => 0)`, `[3,1]`},
		{`[3,1].sort((x, y) => NaN)`, `[3,1]`},
		{`[3,1].sort((x, y) => "1")`, `[1,3]`},
		{`[3,1].sort((x, y) => "-1")`, `[3,1]`},
		{`[3,1].sort((x, y) => -Infinity)`, `[3,1]`},
		{`[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16].sort((a, b) => b - a)`, `[16,15,14,13,12,11,10,9,8,7,6,5,4,3,2,1]`},
		{`[{k:1,v:'a'},{k:0,v:'b'},{k:1,v:'c'},{k:0,v:'d'}].sort((a, b) => a.k - b.k).map(x => x.v).join('')`, `"bdac"`},
		{`Array.from({length: 100}, (_, i) => ({k: i % 3, i})).sort((a, b) => a.k - b.k).every((x, j, arr) => j === 0 || arr[j-1].k < x.k || (arr[j-1].k === x.k && arr[j-1].i < x.i))`, `true`},
		{`T(() => [1].sort(1))`, `TypeError: The comparison function must be either a function or undefined`},
		{`T(() => [1].sort(null))`, `TypeError: The comparison function must be either a function or undefined`},
		{`["b", "a", "B", "10", "9"].sort()`, `["10","9","B","a","b"]`},
		{`[1, "1", 2, true, null].sort()`, `[1,"1",2,null,true]`},
		{`Array.prototype.sort.call({length: 3, 0: 3, 1: 1, 2: 2}, (a, b) => a - b)`, `{"0":1,"1":2,"2":3,"length":3}`},
		{`Array.prototype.sort.call({length: 3, 0: 3, 2: 2})`, `{"0":2,"1":3,"length":3}`},
		{`T(() => Array.prototype.sort.call("abc"))`, `TypeError: Cannot assign to read only property '0' of object`},
		{`[1,2,3].reverse()`, `[3,2,1]`},
		{`[1,,3].reverse()`, `[3,null,1]`},
		{`(() => { const a = [1,,3,4].reverse(); return [a.length, 2 in a, a.join()] })()`, `[4,false,"4,3,,1"]`},
		{`Array.prototype.reverse.call({length: 3, 0: 'a', 2: 'c'})`, `{"0":"c","2":"a","length":3}`},
		{`Array.prototype.reverse.call({length: 2, 0: 'a'})`, `{"1":"a","length":2}`},
		{`Array.prototype.reverse.call({length: 2, 1: 'b'})`, `{"0":"b","length":2}`},
		{`(() => { const a = [1,2,3,4,5]; return [a.splice(1, 2), a] })()`, `[[2,3],[1,4,5]]`},
		{`(() => { const a = [1,2,3,4,5]; return [a.splice(-2), a] })()`, `[[4,5],[1,2,3]]`},
		{`(() => { const a = [1,2,3,4,5]; return [a.splice(), a] })()`, `[[],[1,2,3,4,5]]`},
		{`(() => { const a = [1,2,3,4,5]; return [a.splice(1, 1, 'a', 'b', 'c'), a] })()`, `[[2],[1,"a","b","c",3,4,5]]`},
		{`(() => { const a = [1,2,3,4,5]; return [a.splice(1, 3, 'a'), a] })()`, `[[2,3,4],[1,"a",5]]`},
		{`(() => { const a = [1,2,3,4,5]; return [a.splice(1, 2, 'a', 'b'), a] })()`, `[[2,3],[1,"a","b",4,5]]`},
		{`(() => { const a = [1,2,3]; return [a.splice(1, undefined), a] })()`, `[[],[1,2,3]]`},
		{`(() => { const a = [1,2,3]; return [a.splice(1, -5), a] })()`, `[[],[1,2,3]]`},
		{`(() => { const a = [1,2,3]; return [a.splice(1, Infinity), a] })()`, `[[2,3],[1]]`},
		{`(() => { const a = [1,2,3]; return [a.splice(undefined), a] })()`, `[[1,2,3],[]]`},
		{`(() => { const a = [1,2,3]; return [a.splice(NaN, 1), a] })()`, `[[1],[2,3]]`},
		{`(() => { const a = [1,,3]; const r = a.splice(0, 3); return [r.length, 1 in r] })()`, `[3,false]`},
		{`(() => { const o = {length: 3, 0: 'a', 1: 'b', 2: 'c'}; const r = Array.prototype.splice.call(o, 1, 1, 'X', 'Y'); return [r, o] })()`, `[["b"],{"0":"a","1":"X","2":"Y","3":"c","length":4}]`},
		{`(() => { const o = {length: 3, 0: 'a', 1: 'b', 2: 'c'}; const r = Array.prototype.splice.call(o, 0, 2); return [r, o] })()`, `[["a","b"],{"0":"c","length":1}]`},
		{`(() => { const o = {length: 3, 0: 'a', 2: 'c'}; const r = Array.prototype.splice.call(o, 0, 1); return [r, o, 1 in o] })()`, `[["a"],{"1":"c","length":2},true]`},
		{`(() => { const a = [1,2,3]; return [a.shift(), a] })()`, `[1,[2,3]]`},
		{`(() => { const a = [,2,3]; return [a.shift(), a] })()`, `[null,[2,3]]`},
		{`(() => { const a = []; return [a.shift(), a] })()`, `[null,[]]`},
		{`(() => { const o = {length: 2, 0: 'a', 1: 'b'}; return [Array.prototype.shift.call(o), o] })()`, `["a",{"0":"b","length":1}]`},
		{`(() => { const o = {length: 0}; return [Array.prototype.shift.call(o), o] })()`, `[null,{"length":0}]`},
		{`(() => { const o = {}; return [Array.prototype.shift.call(o), o] })()`, `[null,{"length":0}]`},
		{`(() => { const o = {length: 3, 0: 'a', 2: 'c'}; return [Array.prototype.shift.call(o), o, 0 in o, 1 in o] })()`, `["a",{"1":"c","length":2},false,true]`},
		{`(() => { const a = [1,2]; return [a.unshift(-1, 0), a] })()`, `[4,[-1,0,1,2]]`},
		{`(() => { const a = [1,2]; return [a.unshift(), a] })()`, `[2,[1,2]]`},
		{`(() => { const o = {length: 2, 0: 'a', 1: 'b'}; return [Array.prototype.unshift.call(o, 'x'), o] })()`, `[3,{"0":"x","1":"a","2":"b","length":3}]`},
		{`(() => { const o = {length: 2, 1: 'b'}; Array.prototype.unshift.call(o, 'x'); return [o, 1 in o] })()`, `[{"0":"x","2":"b","length":3},false]`},
		{`(() => { const a = [1]; return [a.push(2, 3), a] })()`, `[3,[1,2,3]]`},
		{`(() => { const o = {length: 2}; return [Array.prototype.push.call(o, 'x'), o] })()`, `[3,{"2":"x","length":3}]`},
		{`(() => { const o = {}; return [Array.prototype.push.call(o, 'x'), o] })()`, `[1,{"0":"x","length":1}]`},
		{`T(() => Array.prototype.push.call({length: 2**53 - 1}, 1))`, `TypeError: Pushing 1 elements on an array-like of length 9007199254740991 is disallowed, as the total surpasses 2**53-1`},
		{`(() => { const o = {length: 2**53 - 1}; return T(() => Array.prototype.push.call(o)) })()`, `"ok:9007199254740991"`},
		{`(() => { const a = [1,2]; return [a.pop(), a] })()`, `[2,[1]]`},
		{`(() => { const o = {length: 2, 0: 'a', 1: 'b'}; return [Array.prototype.pop.call(o), o] })()`, `["b",{"0":"a","length":1}]`},
		{`(() => { const a = []; a.pop(); return a.length })()`, `0`},
		{`[1,2,3].slice(-2)`, `[2,3]`},
		{`[1,2,3].slice(1, -1)`, `[2]`},
		{`[1,2,3].slice(5)`, `[]`},
		{`[1,2,3].slice(2, 1)`, `[]`},
		{`(() => { const a = [1,,3].slice(); return [a.length, 1 in a] })()`, `[3,false]`},
		{`Array.prototype.slice.call({length: 2, 0: 'a', 1: 'b'})`, `["a","b"]`},
		{`Array.prototype.slice.call("ab")`, `["a","b"]`},
		{`Array.prototype.slice.call({length: 3, 0: 'a'}).length`, `3`},
		{`1 in Array.prototype.slice.call({length: 3, 0: 'a'})`, `false`},
		{`[1].concat(2, [3, [4]], "s")`, `[1,2,3,[4],"s"]`},
		{`(() => { const a = [1,,3].concat([4]); return [a.length, 1 in a] })()`, `[4,false]`},
		{`Array.prototype.concat.call(1, 2)`, `[1,2]`},
		{`Array.prototype.concat.call("ab", "c").length`, `2`},
		{`Array.prototype.concat.call({length: 2, 0: 'a'}, [1]).length`, `2`},
		{`[].concat({length: 2, 0: 'a'}).length`, `1`},
		{`[1,2,3].fill(0)`, `[0,0,0]`},
		{`[1,2,3].fill(0, 1)`, `[1,0,0]`},
		{`[1,2,3].fill(0, -1)`, `[1,2,0]`},
		{`[1,2,3].fill(0, 1, 2)`, `[1,0,3]`},
		{`[1,2,3].fill(0, 5)`, `[1,2,3]`},
		{`[1,2,3].fill(0, 1, -5)`, `[1,2,3]`},
		{`[1,2,3].fill(0, NaN, NaN)`, `[1,2,3]`},
		{`[1,2,3].fill(0, undefined, 1)`, `[0,2,3]`},
		{`[1,2,3].fill()`, `[null,null,null]`},
		{`(() => { const a = new Array(3).fill(1); return [a, 0 in a] })()`, `[[1,1,1],true]`},
		{`Array.prototype.fill.call({length: 2}, 7)`, `{"0":7,"1":7,"length":2}`},
		{`Array.prototype.fill.call({length: 2, 5: 1}, 7)`, `{"0":7,"1":7,"5":1,"length":2}`},
		{`Array.from("abc")`, `["a","b","c"]`},
		{`Array.from("a😀b").length`, `3`},
		{`Array.from({length: 2, 0: 'a'})`, `["a",null]`},
		{`Array.from({length: 2}, (v, i) => i * 2)`, `[0,2]`},
		{`Array.from([1,,3])`, `[1,null,3]`},
		{`(() => { const a = Array.from([1,,3]); return [a.length, 1 in a] })()`, `[3,true]`},
		{`Array.from([1,2], function (x) { return this.k + x }, {k: 10})`, `[11,12]`},
		{`T(() => Array.from([1], 1))`, `TypeError: 1 is not a function`},
		{`T(() => Array.from(null))`, `TypeError: Cannot convert undefined or null to object`},
		{`Array.from(1)`, `[]`},
		{`Array.from({length: -1})`, `[]`},
		{`Array.from({length: "2"}).length`, `2`},
		{`Array.from([1,2].keys())`, `[0,1]`},
		{`Array.from([1,2].entries())`, `[[0,1],[1,2]]`},
		{`Array.from(Array.from([9,8]).values())`, `[9,8]`},
		{`Array.of(1, 2, 3)`, `[1,2,3]`},
		{`Array.of()`, `[]`},
		{`Array.of(7).length`, `1`},
		{`Array.of(undefined)`, `[null]`},
		{`(() => { function C(n) { this.n = n }; const r = Array.from.call(C, {length: 2, 0: 'a', 1: 'b'}); return [r instanceof C, r.n, r.length, r[0], r[1]] })()`, `[true,2,2,"a","b"]`},
		{`(() => { function C(n) { this.n = n }; const r = Array.of.call(C, 'a'); return [r instanceof C, r.n, r.length, r[0]] })()`, `[true,1,1,"a"]`},
		{`(() => { function C() {}; const r = Array.from.call(C, [1, 2]); return [r instanceof C, r.length, r[1]] })()`, `[true,2,2]`},
		{`(() => { const r = Array.from.call(Object, [1, 2]); return [Array.isArray(r), r.length, r[1]] })()`, `[false,2,2]`},
		{`(() => { const r = Array.from.call(null, [1, 2]); return [Array.isArray(r), r.length] })()`, `[true,2]`},
		{`(() => { const r = Array.from.call(x => x, [1, 2]); return [Array.isArray(r), r.length] })()`, `[true,2]`},
		{`Array.isArray([])`, `true`},
		{`Array.isArray({length: 0})`, `false`},
		{`Array.isArray(Array.prototype)`, `true`},
		{`Array.isArray()`, `false`},
		{`Array(3).length`, `3`},
		{`Array(1, 2)`, `[1,2]`},
		{`Array("3")`, `["3"]`},
		{`new Array(3).join("-")`, `"--"`},
		{`(() => { const a = new Array(2); return [a.length, 0 in a] })()`, `[2,false]`},
		{`(() => { const a = new Array(5000); a[4999] = 1; return [a.length, a.indexOf(1), a.lastIndexOf(1), a.includes(1), a.join("").length, Object.keys(a)] })()`, `[5000,4999,4999,true,1,["4999"]]`},
		{`(() => { const a = []; a[4294967294] = 1; return [a.length, Object.keys(a)] })()`, `[4294967295,["4294967294"]]`},
		{`(() => { const a = []; a[4294967295] = 1; return [a.length, Object.keys(a)] })()`, `[0,["4294967295"]]`},
		{`(() => { const a = [1]; a["1"] = 2; a["01"] = 3; a[" 1"] = 4; return [a.length, Object.keys(a)] })()`, `[2,["0","1","01"," 1"]]`},
		{`(() => { const a = [1,2,3]; return [a.keys().next(), a.entries().next(), a.values().next()] })()`, `[{"value":0,"done":false},{"value":[0,1],"done":false},{"value":1,"done":false}]`},
		{`(() => { const it = [1].values(); it.next(); it.next(); return it.next() })()`, `{"done":true}`},
		{`(() => { const a = [1]; const it = a.values(); it.next(); a.push(2); return it.next() })()`, `{"value":2,"done":false}`},
		{`(() => { const a = [1]; const it = a.values(); it.next(); it.next(); a.push(2); return it.next() })()`, `{"done":true}`},
		{`(() => { const it = Array.prototype.values.call({length: 1, 0: 'x'}); return [it.next(), it.next()] })()`, `[{"value":"x","done":false},{"done":true}]`},
		{`T(() => [].values().next.call({}))`, `TypeError: next method called on incompatible receiver [object Object]`},
		{`(() => { const it = [1,2].keys(); const r = []; for (const k of it) r.push(k); return r })()`, `[0,1]`},
		{`(() => { const r = []; for (const [i, v] of ['a','b'].entries()) r.push(i + v); return r })()`, `["0a","1b"]`},
		{`Object.getPrototypeOf(Object.getPrototypeOf(Object.getPrototypeOf([].values()))) === Object.prototype`, `true`},
		{`Object.getPrototypeOf([].values()) === Object.getPrototypeOf([].keys())`, `true`},
		{`[].values().next.length`, `0`},
		{`[1,2,3].indexOf("1")`, `-1`},
		{`[1,2,3].includes("1")`, `false`},
		{`Array.prototype.indexOf.call({length: 3, 2: 1}, 1)`, `2`},
		{`Array.prototype.indexOf.call({length: 3, 2: undefined}, undefined)`, `2`},
		{`Array.prototype.indexOf.call({length: 3, 1: undefined}, undefined, 2)`, `-1`},
		{`Array.prototype.lastIndexOf.call({length: 3, 0: 1}, 1)`, `0`},
		{`Array.prototype.includes.call({length: 3}, undefined)`, `true`},
		{`Array.prototype.includes.call("abc", "b")`, `true`},
		{`Array.prototype.indexOf.call("abc", "c")`, `2`},
		{`Array.prototype.map.call("ab", x => x + x)`, `["aa","bb"]`},
		{`Array.prototype.filter.call("abc", x => x > "a")`, `["b","c"]`},
		{`Array.prototype.forEach.call("ab", function (x) { this.push(x) }, [])`, `undefined`},
		{`(() => { const acc = []; Array.prototype.forEach.call("ab", function (x) { this.push(x) }, acc); return acc })()`, `["a","b"]`},
		{`(() => { const acc = []; Array.prototype.forEach.call([1], function (x) { acc.push(this) }, 5); return typeof acc[0] })()`, `"number"`},
		{`(() => { const acc = []; [1].forEach(function (x) { acc.push(this) }); return acc[0] })()`, `undefined`},
		{`Array.prototype.at.call({length: 2, 1: 'b'}, -1)`, `"b"`},
		{`Array.prototype.every.call({length: 0}, x => false)`, `true`},
		{`Array.prototype.some.call({length: 0}, x => true)`, `false`},
		{`T(() => Array.prototype.map.call(null, x => x))`, `TypeError: Cannot convert undefined or null to object`},
		{`T(() => Array.prototype.forEach.call([1], null))`, `TypeError: null is not a function`},
		{`T(() => Array.prototype.forEach.call(null, null))`, `TypeError: Cannot convert undefined or null to object`},
		{`T(() => Array.prototype.map.call({length: 2**32}, x => x))`, `RangeError: Invalid array length`},
		{`Array.prototype.lastIndexOf.call({length: 2**53 - 1, 9007199254740990: 1}, 1)`, `9007199254740990`},
		{`Array.prototype.at.call({length: 2**53 - 1, 9007199254740990: 1}, -1)`, `1`},
		{`Array.prototype.includes.call({length: 2**53 - 1, 9007199254740990: 1}, 1, -1)`, `true`},
		{`T(() => Array.prototype.slice.call({length: 2**53 - 1}, 0, 2**33))`, `RangeError: Invalid array length`},
		{`T(() => Array.prototype.splice.call({length: 2**53 - 1}, 0, 0, 1))`, `TypeError: Splicing the array-like would surpass 2**53-1 elements`},
		{`T(() => Array.prototype.unshift.call({length: 2**53 - 1}, 1))`, `TypeError: Unshifting 1 elements on an array-like of length 9007199254740991 is disallowed, as the total surpasses 2**53-1`},
		{`T(() => Array.prototype.concat.call([], {length: 2**53 - 1}).length)`, `ok:1`},
		{`(() => { const o = {length: 2**53 - 1, 9007199254740990: 'last'}; return Array.prototype.pop.call(o) + ':' + o.length })()`, `"last:9007199254740990"`},
		{`(() => { const a = [1,2,3]; a.length = 2**32 - 1; return [a.length, a.at(-1), a.indexOf(3)] })()`, `[4294967295,null,2]`},
		{`(() => { const a = [1,2,3]; a.length = 2**32 - 1; a.length = 2; return [a.length, a] })()`, `[2,[1,2]]`},
		{`(() => { const a = [1,2,3]; a.length = 2000; a[1999] = 'x'; a.length = 3; return [a.length, a] })()`, `[3,[1,2,3]]`},
		{`T(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {value: 'nc', configurable: false}); a.length = 0; return [a.length, a[1]] })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {value: 'nc', configurable: false}); return T(() => { a.length = 0 }) + ':' + a.length })()`, `"TypeError: Cannot assign to read only property 'length' of object:2"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {value: 'nc', configurable: false}); return T(() => { a.pop(); a.pop(); a.pop() }) + ':' + a.length })()`, `"TypeError: Cannot delete property '1' of [object Array]:2"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {value: 'nc', configurable: false}); return T(() => Object.defineProperty(a, 'length', {value: 0})) + ':' + a.length })()`, `"TypeError: Cannot redefine property: length:2"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 'length', {value: 1, writable: false}); return [a.length, T(() => { a.length = 5 }), T(() => Object.defineProperty(a, 'length', {value: 0}))] })()`, `[1,"TypeError: Cannot assign to read only property 'length' of object","TypeError: Cannot redefine property: length"]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 'length', {writable: false}); return [T(() => Object.defineProperty(a, 'length', {value: 3})), T(() => Object.defineProperty(a, 'length', {value: 2})), T(() => { a[5] = 1 }), T(() => { a[2] = 9 }), a] })()`, `["ok:[1,2,3]","TypeError: Cannot redefine property: length","TypeError: Cannot assign to read only property '5' of object","ok:undefined",[1,2,9]]`},
		{`(() => { const a = []; return T(() => Object.defineProperty(a, 'length', {value: "abc"})) })()`, `"RangeError: Invalid array length"`},
		{`(() => { const a = []; return T(() => Object.defineProperty(a, 'length', {value: 2, get() {}})) })()`, `"TypeError: Invalid property descriptor. Cannot both specify accessors and a value or writable attribute"`},
		{`(() => { const a = []; return T(() => Object.defineProperty(a, 'length', {get() {}})) })()`, `"TypeError: Cannot redefine property: length"`},
		{`(() => { const a = []; return T(() => Object.defineProperty(a, 'length', {enumerable: true})) })()`, `"TypeError: Cannot redefine property: length"`},
		{`(() => { const a = []; return T(() => Object.defineProperty(a, 'length', {configurable: true})) })()`, `"TypeError: Cannot redefine property: length"`},
		{`(() => { const a = [1]; Object.defineProperty(a, 'length', {value: 2}); return [a.length, 1 in a] })()`, `[2,false]`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = {valueOf() { return 1.5 }} }) })()`, `"RangeError: Invalid array length"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = null }) + a.length })()`, `"ok:undefined0"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = true }) + a.length })()`, `"ok:undefined1"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = "" }) + a.length })()`, `"ok:undefined0"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = [] }) + a.length })()`, `"ok:undefined0"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = "0x2" }) + a.length })()`, `"ok:undefined2"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = " 2 " }) + a.length })()`, `"ok:undefined2"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = "2a" }) })()`, `"RangeError: Invalid array length"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = -0 }) + a.length })()`, `"ok:undefined0"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = NaN }) })()`, `"RangeError: Invalid array length"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = Infinity }) })()`, `"RangeError: Invalid array length"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = undefined }) })()`, `"RangeError: Invalid array length"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = 1n }) })()`, `"TypeError: Cannot convert a BigInt value to a number"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = {} }) })()`, `"RangeError: Invalid array length"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = 4294967295 }) + a.length })()`, `"ok:undefined4294967295"`},
		{`(() => { const a = [1,2,3]; return T(() => { a.length = 4294967296 }) })()`, `"RangeError: Invalid array length"`},
		{`T(() => { const a = [1,2,3]; delete a.length; return a.length })`, `TypeError: Cannot delete property 'length' of [object Array]`},
		{`(() => { const a = [1,2,3]; return T(() => delete a.length) })()`, `"TypeError: Cannot delete property 'length' of [object Array]"`},
		{`(() => { const a = [1,2,3]; return Object.keys(a).concat(Object.getOwnPropertyNames(a)) })()`, `["0","1","2","0","1","2","length"]`},
		{`(() => { const a = [1,2,3]; a.x = 1; a[10] = 1; a[5] = 1; return Object.keys(a) })()`, `["0","1","2","5","10","x"]`},
		{`(() => { const o = {}; o[4294967295] = 1; o[4294967294] = 1; o[1] = 1; o[0] = 1; o['-1'] = 1; o['1.0'] = 1; o['01'] = 1; return Object.keys(o) })()`, `["0","1","4294967294","4294967295","-1","1.0","01"]`},
		{`(() => { const o = {}; o[4294967296] = 1; o[2] = 1; o['4294967295'] = 1; o['4294967294'] = 1; return Object.keys(o) })()`, `["2","4294967294","4294967296","4294967295"]`},
		{`(() => { const o = {b: 1, 2: 1, a: 1, 1: 1}; for (let i = 0; i < 70; i++) o['k' + i] = 1; o[0] = 1; o.z = 1; const k = Object.keys(o); return [k.length, k.slice(0, 5), k[k.length - 1]] })()`, `[76,["0","1","2","b","a"],"z"]`},
		{`(() => { const o = {b: 1, 2: 1, a: 1, 1: 1}; for (let i = 0; i < 70; i++) o['k' + i] = 1; delete o.b; delete o[1]; o.b = 1; o[1] = 1; const k = Object.keys(o); return [k.length, k.slice(0, 4), k[k.length - 1]] })()`, `[74,["1","2","a","k0"],"b"]`},
		{`(() => { const o = {a: 1, b: 2}; const s = []; for (const k in o) { s.push(k); delete o.b; } return s })()`, `["a"]`},
		{`(() => { const o = {a: 1}; const s = []; for (const k in o) { s.push(k); o.b = 1 } return s })()`, `["a"]`},
		{`(() => { const p = {inh: 1, 5: 1}; const o = Object.create(p); o.own = 1; o[3] = 1; const s = []; for (const k in o) s.push(k); return s })()`, `["3","own","5","inh"]`},
		{`(() => { const s = []; for (const k in "ab") s.push(k); return s })()`, `["0","1"]`},
		{`(() => { const s = []; for (const k in [1, , 3]) s.push(k); return s })()`, `["0","2"]`},
		{`(() => { const s = []; for (const k in null) s.push(k); for (const k in undefined) s.push(k); return s })()`, `[]`},
		{`JSON.stringify({b: 1, 2: 1, a: 1, 1: 1})`, `"{\"1\":1,\"2\":1,\"b\":1,\"a\":1}"`},
		{`(() => { const o = {}; Object.defineProperty(o, 'x', {get() { return 1 }, enumerable: true}); return [Object.keys(o), Object.values(o), Object.entries(o), JSON.stringify(o)] })()`, `[["x"],[1],[["x",1]],"{\"x\":1}"]`},
		{`(() => { const o = {a: 1, b: 2}; Object.defineProperty(o, 'a', {get() { delete o.b; return 1 }, enumerable: true}); return [Object.values(o), Object.entries(o)] })()`, `[[1],[["a",1]]]`},
		{`(() => { const o = {a: 1, b: 2}; Object.defineProperty(o, 'a', {get() { o.c = 3; return 1 }, enumerable: true}); return Object.values(o) })()`, `[1,2]`},
		{`(() => { const o = {a: 1}; Object.defineProperty(o, 'g', {get() { throw new Error("g") }, enumerable: true}); return T(() => Object.values(o)) })()`, `"Error: g"`},
		{`(() => { const o = {a: 1}; Object.defineProperty(o, 'g', {get() { throw new Error("g") }, enumerable: true}); return T(() => Object.assign({}, o)) })()`, `"Error: g"`},
		{`(() => { const o = {a: 1}; Object.defineProperty(o, 'g', {get() { throw new Error("g") }, enumerable: false}); return T(() => Object.assign({}, o)) })()`, `"ok:{\"a\":1}"`},
		{`(() => { const t = {}; Object.defineProperty(t, 'a', {set(v) { this.got = v }}); Object.assign(t, {a: 5}); return t.got })()`, `5`},
		{`(() => { const t = {}; Object.defineProperty(t, 'a', {value: 1}); return T(() => Object.assign(t, {a: 5})) })()`, `"TypeError: Cannot assign to read only property 'a' of object"`},
		{`(() => { const t = Object.freeze({}); return T(() => Object.assign(t, {a: 5})) })()`, `"TypeError: Cannot assign to read only property 'a' of object"`},
		{`(() => { const t = Object.freeze({}); return T(() => Object.assign(t, {})) })()`, `"ok:{}"`},
		{`Object.assign([1, 2], [3])`, `[3,2]`},
		{`Object.assign([1, 2], {length: 1})`, `[1]`},
		{`Object.assign({}, [1, 2])`, `{"0":1,"1":2}`},
		{`Object.assign({}, [1,,2])`, `{"0":1,"2":2}`},
		{`Object.assign({}, new Error("m"))`, `{}`},
		{`Object.assign({}, /x/g)`, `{}`},
		{`Object.assign({}, () => 1)`, `{}`},
		{`Object.assign({}, {}, {b: 1}, {a: 1}, {b: 2})`, `{"b":2,"a":1}`},
		{`Object.keys(Object.assign({}, {b: 1}, {a: 1}, {b: 2}))`, `["b","a"]`},
		{`Object.assign(1, {a: 1}) instanceof Number`, `true`},
		{`Object.assign("s", {a: 1}).a`, `1`},
		{`T(() => Object.assign("s", {0: 'x'}))`, `TypeError: Cannot assign to read only property '0' of object`},
		{`T(() => Object.assign("s", {length: 3}))`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => Object.assign(true, {a: 1}))`, `ok:true`},
		{`Object.entries("ab")`, `[["0","a"],["1","b"]]`},
		{`Object.values("ab")`, `["a","b"]`},
		{`Object.entries([1, , 3])`, `[["0",1],["2",3]]`},
		{`Object.values(new Number(3))`, `[]`},
		{`Object.entries(1)`, `[]`},
		{`T(() => Object.entries(undefined))`, `TypeError: Cannot convert undefined or null to object`},
		{`T(() => Object.values(null))`, `TypeError: Cannot convert undefined or null to object`},
		{`T(() => Object.getOwnPropertyNames(null))`, `TypeError: Cannot convert undefined or null to object`},
		{`Object.getOwnPropertyNames(1)`, `[]`},
		{`Object.getOwnPropertyNames({b: 1, a: 2, 1: 3})`, `["1","b","a"]`},
		{`(() => { const o = {a: 1}; Object.defineProperty(o, 'h', {value: 1}); return Object.getOwnPropertyNames(o) })()`, `["a","h"]`},
		{`Object.getOwnPropertyNames(Object.freeze({a: 1, 0: 2}))`, `["0","a"]`},
		{`Object.getOwnPropertyNames(Object.freeze([1, , 3]))`, `["0","2","length"]`},
		{`Object.keys(Object.freeze([1, , 3]))`, `["0","2"]`},
		{`(() => { const a = [1, 2]; Object.defineProperty(a, 0, {enumerable: false}); return [Object.keys(a), Object.getOwnPropertyNames(a), a.length, a[0]] })()`, `[["1"],["0","1","length"],2,1]`},
		{`(() => { const a = [1, 2]; Object.defineProperty(a, 0, {enumerable: false}); a.length = 0; return [a.length, 0 in a] })()`, `[0,false]`},
		{`(() => { const a = [1, 2]; Object.defineProperty(a, 0, {writable: false}); return T(() => { a[0] = 5 }) + a[0] })()`, `"TypeError: Cannot assign to read only property '0' of object1"`},
		{`(() => { const a = [1, 2]; Object.defineProperty(a, 0, {writable: false}); return T(() => a.fill(9)) + a[0] })()`, `"TypeError: Cannot assign to read only property '0' of object1"`},
		{`(() => { const a = [1, 2]; Object.defineProperty(a, 0, {writable: false}); return T(() => a.reverse()) + S(a) })()`, `"TypeError: Cannot assign to read only property '0' of object[1,2]"`},
		{`(() => { const a = [2, 1]; Object.defineProperty(a, 0, {writable: false}); return T(() => a.sort()) + S(a) })()`, `"TypeError: Cannot assign to read only property '0' of object[2,1]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 0, {writable: false}); return T(() => a.shift()) + S(a) })()`, `"TypeError: Cannot assign to read only property '0' of object[1,2,3]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 2, {writable: false}); return T(() => a.shift()) + S(a) })()`, `"ok:1[2,3]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.splice(0, 1)) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[2,2,3]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 1, {configurable: false}); return T(() => a.splice(1, 1)) + S(a) })()`, `"ok:[2][1,3]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 0, {writable: false}); return T(() => a.unshift(0)) + S(a) })()`, `"TypeError: Cannot assign to read only property '0' of object[1,1,2,3]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 2, {writable: false}); return T(() => a.copyWithin(2, 0)) + S(a) })()`, `"TypeError: Cannot assign to read only property '2' of object[1,2,3]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 2, {writable: false}); return T(() => a.splice(0, 0, 'x')) + S(a) })()`, `"TypeError: Cannot assign to read only property '2' of object[1,2,3,3]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 2, {writable: false}); return T(() => a.pop()) + S(a) })()`, `"ok:3[1,2]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 2, {writable: false}); return T(() => a.push(4)) + S(a) })()`, `"ok:4[1,2,3,4]"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 2, {writable: false}); return T(() => { a.length = 5 }) + a.length })()`, `"ok:undefined5"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 2, {writable: false}); a.length = 0; return a.length })()`, `0`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 1, {configurable: false}); return T(() => { a.length = 0 }) + a.length })()`, `"TypeError: Cannot assign to read only property 'length' of object2"`},
		{`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 1, {configurable: false}); a.length = 2; return [a.length, a[1]] })()`, `[2,2]`},
		{`(() => { const a = [1, 2, 3]; a[1000] = 'x'; Object.defineProperty(a, 500, {value: 'nc', configurable: false}); return T(() => { a.length = 0 }) + a.length + ':' + (1000 in a) + ':' + (2 in a) })()`, `"TypeError: Cannot assign to read only property 'length' of object501:false:true"`},
		{`(() => { const a = [1, 2, 3]; a[1000] = 'x'; Object.defineProperty(a, 2, {value: 'nc', configurable: false}); return T(() => { a.length = 0 }) + a.length + ':' + (1000 in a) + ':' + (2 in a) + ':' + (1 in a) })()`, `"TypeError: Cannot assign to read only property 'length' of object3:false:true:true"`},
		{`(() => { const a = [1, 2, 3]; a[1000] = 'x'; Object.defineProperty(a, 1000, {configurable: false}); return T(() => { a.length = 0 }) + a.length })()`, `"TypeError: Cannot assign to read only property 'length' of object1001"`},
		{`(() => { const a = [1, 2, 3]; a[3000] = 'x'; a[2000] = 'y'; Object.defineProperty(a, 2000, {configurable: false}); return T(() => { a.length = 0 }) + a.length + (3000 in a) })()`, `"TypeError: Cannot assign to read only property 'length' of object2001false"`},
		{`(() => { const a = [1, 2, 3]; a[3000] = 'x'; a[2000] = 'y'; Object.defineProperty(a, 2000, {configurable: false}); a.length = 2500; return [a.length, 2000 in a, 3000 in a] })()`, `[2500,true,false]`},
		{`(() => { const a = []; a[5] = 1; a.length = 3; return [a.length, 5 in a] })()`, `[3,false]`},
		{`(() => { const a = []; a[5000] = 1; a.length = 3; return [a.length, 5000 in a, Object.keys(a)] })()`, `[3,false,[]]`},
		{`(() => { const a = []; a[5000] = 1; a[2] = 1; a.length = 3; return [a.length, Object.keys(a)] })()`, `[3,["2"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.length = 1; return [a.length, Object.keys(a)] })()`, `[1,["0"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); return [a.indexOf(2), a.includes(2), a.join(), JSON.stringify(a), a.map(x => x), a.slice(), a.concat(), a.filter(() => true), Object.keys(a)] })()`, `[1,true,"1,2,3","[1,2,3]",[1,2,3],[1,2,3],[1,2,3],[1,2,3],["0","2"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); return Object.assign({}, a) })()`, `{"0":1,"2":3}`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.reverse(); return [a, Object.keys(a)] })()`, `[[3,2,1],["0","2"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.sort((x, y) => y - x); return [a, Object.keys(a)] })()`, `[[3,2,1],["0","2"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.shift(); return [a, Object.keys(a)] })()`, `[[2,3],["0"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.splice(0, 1); return [a, Object.keys(a)] })()`, `[[2,3],["0"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.unshift(0); return [a, Object.keys(a)] })()`, `[[0,1,2,3],["0","2","3"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.fill(0); return [a, Object.keys(a)] })()`, `[[0,0,0],["0","2"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.pop(); a.pop(); return [a, Object.keys(a)] })()`, `[[1],["0"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a.length = 1; a.push(5); return [a, Object.keys(a)] })()`, `[[1,5],["0","1"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); a[1] = 7; return [a, Object.keys(a)] })()`, `[[1,7,3],["0","2"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {enumerable: false}); delete a[1]; a[1] = 7; return [a, Object.keys(a)] })()`, `[[1,7,3],["0","1","2"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a[1] = 7 }) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[1,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1 }) + S(a) })()`, `"ok:undefined[1]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.reverse()) + S(a) })()`, `"ok:[3,2,1][3,2,1]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 3, {value: 4, writable: false, configurable: true, enumerable: true}); return T(() => a.reverse()) + S(a) })()`, `"TypeError: Cannot assign to read only property '3' of object[4,2,3,4]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 5, {value: 4, writable: true, configurable: true, enumerable: true}); a.reverse(); return [a, Object.keys(a)] })()`, `[[4,null,null,3,2,1],["0","3","4","5"]]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 5, {value: 4, writable: true, configurable: true, enumerable: true}); return Object.getOwnPropertyNames(a) })()`, `["0","1","2","5","length"]`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.splice(1, 1)) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[1,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.splice(1, 0, 'x')) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[1,2,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.copyWithin(0, 1)) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[2,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.shift()) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[2,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.unshift(0)) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[1,2,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.fill(0)) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[0,2,3]"`},
		{`(() => { const a = [3,2,1]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.sort()) + S(a) })()`, `"TypeError: Cannot assign to read only property '1' of object[1,2,1]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.pop()) + S(a) })()`, `"ok:3[1,2]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.push(4)) + S(a) })()`, `"ok:4[1,2,3,4]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.map(x => x * 2)) })()`, `"ok:[2,4,6]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.slice(1)) })()`, `"ok:[2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.concat([4])) })()`, `"ok:[1,2,3,4]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.flat()) })()`, `"ok:[1,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.filter(x => x > 1)) })()`, `"ok:[2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Array.from(a)) })()`, `"ok:[1,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.join()) })()`, `"ok:\"1,2,3\""`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.indexOf(2)) })()`, `"ok:1"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.at(1)) })()`, `"ok:2"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.reduce((x, y) => x + y)) })()`, `"ok:6"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.reduceRight((x, y) => x + y)) })()`, `"ok:6"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.find(x => x === 2)) })()`, `"ok:2"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.findLast(x => x === 2)) })()`, `"ok:2"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.some(x => x === 2)) })()`, `"ok:true"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.every(x => x > 0)) })()`, `"ok:true"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.includes(2)) })()`, `"ok:true"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.lastIndexOf(2)) })()`, `"ok:1"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.flatMap(x => [x])) })()`, `"ok:[1,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => [...a.entries()]) })()`, `"ok:[[0,1],[1,2],[2,3]]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.toString()) })()`, `"ok:\"1,2,3\""`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.forEach(x => x)) })()`, `"ok:undefined"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.findIndex(x => x === 3)) })()`, `"ok:2"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.findLastIndex(x => x === 1)) })()`, `"ok:0"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Object.keys(a)) })()`, `"ok:[\"0\",\"1\",\"2\"]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Object.values(a)) })()`, `"ok:[1,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Object.entries(a)) })()`, `"ok:[[\"0\",1],[\"1\",2],[\"2\",3]]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Object.assign({}, a)) })()`, `"ok:{\"0\":1,\"1\":2,\"2\":3}"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => JSON.stringify(a)) })()`, `"ok:\"[1,2,3]\""`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Object.freeze(a) === a) })()`, `"ok:true"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Object.isFrozen(a)) })()`, `"ok:false"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Object.getOwnPropertyNames(a)) })()`, `"ok:[\"0\",\"1\",\"2\",\"length\"]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => Object.hasOwn(a, 1)) })()`, `"ok:true"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.propertyIsEnumerable(1)) })()`, `"ok:true"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.hasOwnProperty(1)) })()`, `"ok:true"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => 1 in a) })()`, `"ok:true"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => delete a[1]) + S(a) })()`, `"ok:true[1,null,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { for (const x of a) {} }) })()`, `"ok:undefined"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => [...a]) })()`, `"ok:[1,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { const [x, y, z] = a; return x + y + z }) })()`, `"ok:6"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a.length) })()`, `"ok:3"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => a[1]) })()`, `"ok:2"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a[0] = 9; return a }) })()`, `"ok:[9,2,3]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a[3] = 9; return a }) })()`, `"ok:[1,2,3,9]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 5; return a }) })()`, `"ok:[1,2,3,null,null]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 2; return a }) })()`, `"ok:[1,2]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; return a }) })()`, `"ok:[1]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 0; return a }) })()`, `"ok:[]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 2; a.length = 3; return a }) })()`, `"ok:[1,2,null]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 2; a[2] = 7; return a }) })()`, `"ok:[1,2,7]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a[1] = 7; return a }) })()`, `"ok:[1,7]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a.push(7); return a }) })()`, `"ok:[1,7]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a.unshift(7); return a }) })()`, `"ok:[7,1]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a.splice(0, 0, 7); return a }) })()`, `"ok:[7,1]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a.fill(7); return a }) })()`, `"ok:[7]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a.reverse(); return a }) })()`, `"ok:[1]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a.sort(); return a }) })()`, `"ok:[1]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a.shift(); return a }) })()`, `"ok:[]"`},
		{`(() => { const a = [1,2,3]; Object.defineProperty(a, 1, {writable: false}); return T(() => { a.length = 1; a.pop(); return a }) })()`, `"ok:[]"`},
	}
	for _, c := range cases {
		got := auditAExpr(t, c.expr)
		if got != c.want {
			t.Errorf("%s\n  want %s\n  got  %s", c.expr, c.want, got)
		}
	}
}

// Finding (outside the audited files, observed while probing): ArraySetLength
// coerces the new length once; the spec performs ToUint32 and ToNumber
// separately (two valueOf calls, as V8 does).
func TestAuditA_ArraySetLengthCoercesOnce(t *testing.T) {
	require.Equal(t, `[1,2]`, auditAExpr(t, `(() => { let n = 0; const a = [1,2]; a.length = {valueOf() { n++; return 1 }}; return [a.length, n] })()`))
	// A value that differs between the two conversions is a RangeError; a
	// string converts twice as well.
	require.Equal(t, `RangeError: Invalid array length`, auditAExpr(t, `T(() => { let n = 0; const a = [1,2,3]; a.length = {valueOf() { return n++ }}; return a.length })`))
	require.Equal(t, `[2,2]`, auditAExpr(t, `(() => { const a = [1,2,3]; a.length = "2"; return [a.length, a.length] })()`))
	require.Equal(t, `RangeError: Invalid array length`, auditAExpr(t, `T(() => { const a = []; a.length = 1.5; return a.length })`))
}

// Finding (outside the audited files, interpreter for-in): an own
// non-enumerable property must shadow a same-named enumerable prototype
// property (EnumerateObjectProperties "already processed" rule).
func TestAuditA_ForInShadowedByNonEnumerableOwn(t *testing.T) {
	require.Equal(t, `[]`, auditAExpr(t, `(() => { const p = {x: 1}; const o = Object.create(p); Object.defineProperty(o, 'x', {value: 2, enumerable: false}); const s = []; for (const k in o) s.push(k); return s })()`))
	// Shadowing at every depth, and enumerable own keys still listed first.
	require.Equal(t, `["a","y"]`, auditAExpr(t, `(() => { const g = {x: 1, y: 2}; const p = Object.create(g); Object.defineProperty(p, 'x', {value: 0, enumerable: false}); const o = Object.create(p); o.a = 1; const s = []; for (const k in o) s.push(k); return s })()`))
	require.Equal(t, `["b","x"]`, auditAExpr(t, `(() => { const p = {x: 1}; const o = Object.create(p); Object.defineProperty(o, 'h', {value: 2, enumerable: false}); o.b = 1; const s = []; for (const k in o) s.push(k); return s })()`))
	require.Equal(t, `["0","length2"]`, auditAExpr(t, `(() => { const a = [1]; Object.defineProperty(Array.prototype, 'length2', {value: 1, enumerable: true, configurable: true}); const s = []; for (const k in a) s.push(k); delete Array.prototype.length2; return s })()`))
}

// Finding (outside the audited files, compiler object literals): a method
// with a computed key gets no name.
func TestAuditA_ComputedMethodName(t *testing.T) {
	require.Equal(t, `["m","c1"]`, auditAExpr(t, `(() => { const o = {m() {}, ['c' + 1]() {}}; return [o.m.name, o.c1.name] })()`))
	// Numeric keys are named by their string form, the key's toString runs
	// once, and the name stays non-writable but configurable.
	require.Equal(t, `["1","0.5",1]`, auditAExpr(t, `(() => { let n = 0; const k = {toString() { n++; return "0.5" }}; const o = {[1]() {}, [k]() {}}; return [o[1].name, o["0.5"].name, n] })()`))
	require.Equal(t, `["c1",false,true]`, auditAExpr(t, `(() => { const o = {['c' + 1]() {}}; const d = Object.getOwnPropertyNames(o.c1); return [o.c1.name, Object.isFrozen(o.c1), d.includes("name")] })()`))
	require.Equal(t, `"c1"`, auditAExpr(t, `(() => { const o = {['c' + 1]() { return 1 }}; return String(o.c1).startsWith("") ? o.c1.name : "" })()`))
}

// Finding: setLength goes through (*Object).SetLength, whose
// ArraySetLength/[[DefineOwnProperty]] semantics accept an unchanged value
// on a non-writable length, while the spec's Set(O, "length", len, true)
// rejects any write to a non-writable property.
func TestAuditA_SetLengthOnReadOnlyLengthUnchangedValue(t *testing.T) {
	cases := []struct{ expr, want string }{
		{`T(() => Object.freeze([]).pop())`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = [1, 2]; Object.defineProperty(a, 'length', {writable: false}); return a.pop() })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = [1]; Object.defineProperty(a, 'length', {writable: false}); a.length = 1; return a.length })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = [1]; Object.defineProperty(a, 'length', {writable: false}); return Object.defineProperty(a, 'length', {value: 1}).length })`, `ok:1`},
		{`T(() => { const a = [1]; Object.defineProperty(a, 'length', {writable: false}); return [].concat.call(a).length })`, `ok:1`},
		{`T(() => { const a = [1, 2]; return [a.push(), a.unshift(), a.splice().length, a.length] })`, `ok:[2,2,0,2]`},
		{`T(() => Object.freeze([]).shift())`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = [1]; Object.defineProperty(a, 'length', {writable: false}); return a.push() })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = [1]; Object.defineProperty(a, 'length', {writable: false}); return a.unshift() })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => { const a = [1]; Object.defineProperty(a, 'length', {writable: false}); return a.splice() })`, `TypeError: Cannot assign to read only property 'length' of object`},
		{`T(() => Object.freeze([1]).splice(0, 0))`, `TypeError: Cannot assign to read only property 'length' of object`},
	}
	for _, c := range cases {
		got := auditAExpr(t, c.expr)
		if got != c.want {
			t.Errorf("%s\n  want %s\n  got  %s", c.expr, c.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. Fail loudly
// ---------------------------------------------------------------------------

func TestAuditA_FailLoudly(t *testing.T) {
	cases := []string{
		`T(() => Object.getOwnPropertyDescriptor({a: 1}, 'a'))`,
		`T(() => Object.defineProperties({}, {a: {value: 1}}))`,
		`T(() => Object.is(1, 1))`,
		`T(() => [1].copyWithin(0, 1))`,
		`T(() => [1].toSorted())`,
		`T(() => [1].toReversed())`,
		`T(() => [1].with(0, 2))`,
		`T(() => Array.from(new Set()))`,
		`T(() => [...new Map()])`,
		`T(() => Symbol('x'))`,
		`T(() => Reflect.ownKeys({}))`,
		`T(() => new Proxy({}, {}))`,
		`T(() => new Map())`,
		`T(() => new Set())`,
		`T(() => Promise.resolve(1))`,
		`T(() => new WeakMap())`,
		`T(() => structuredClone({}))`,
		`T(() => setTimeout(() => {}, 0))`,
		`T(() => globalThis.Symbol)`,
		`T(() => typeof Symbol)`,
		`T(() => Object.seal({}))`,
		`T(() => Object.isSealed({}))`,
		`T(() => Object.preventExtensions({}))`,
		`T(() => Object.isExtensible({}))`,
		`T(() => Object.getOwnPropertySymbols({}))`,
		`T(() => ({}).__proto__)`,
		`T(() => Array.prototype[Symbol.iterator])`,
		`T(() => [1].at.call(null))`,
		`T(() => Array.from({length: 2, [0]: 1}))`,
		`T(() => Object.entries(Object.create(null)))`,
		`T(() => Object.getOwnPropertyNames(() => 1))`,
		`T(() => Array.from(new Array(2).keys()))`,
		`T(() => Array.from([].values().__proto__))`,
		`T(() => Array.from({length: 1, 0: 1}.values))`,
		`T(() => new Array(2).fill(0).map((x, i) => i))`,
		`T(() => Array.prototype.values.call("ab").next())`,
		`T(() => Array.prototype.keys.call(null))`,
		`T(() => [1].values().toString())`,
		`T(() => [1].values()[Symbol.iterator])`,
		`T(() => String(Object.getOwnPropertyNames(Object.getPrototypeOf([].values()))))`,
		`T(() => Object.getOwnPropertyNames(Array.prototype).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Object).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Object.prototype).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Function.prototype).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Number).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Number.prototype).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Math).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Boolean.prototype).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Error.prototype).sort().join())`,
		`T(() => Object.getOwnPropertyNames(Error).sort().join())`,
		`T(() => Object.getOwnPropertyNames(globalThis).sort().join())`,
	}
	for _, c := range cases {
		t.Logf("%s => %s", c, auditAExpr(t, c))
	}
}

// ---------------------------------------------------------------------------
// 6. ClassName parity
// ---------------------------------------------------------------------------

func TestAuditA_ClassNames(t *testing.T) {
	f, err := auditAModule(t, `
export const typeErr = new TypeError("m");
export const iter = [1].values();
export const bound = (function f() {}).bind(null);
export const nullProto = Object.create(null);
export const arr = [];
export const fn = () => 1;
export const re = /x/;
export const date = new Date(0);
export const str = new String("s");
export const num = new Number(1);
export const boo = new Boolean(true);
export const err = new Error();
export const rangeErr = new RangeError();
export const obj = {};
export const mathObj = Math;
export const arrayProto = Array.prototype;
export const funcProto = Function.prototype;
export const errProto = Error.prototype;
`)
	require.NoError(t, err)
	want := map[string]string{
		"typeErr": "Error", "iter": "Array Iterator", "bound": "Function", "nullProto": "Object",
		"arr": "Array", "fn": "Function", "re": "RegExp", "date": "Date", "str": "String",
		"num": "Number", "boo": "Boolean", "err": "Error", "rangeErr": "Error", "obj": "Object",
		"mathObj": "Object", "arrayProto": "Array", "funcProto": "Function", "errProto": "Object",
	}
	for name, w := range want {
		v, ok := f.env.GetBindingValue(name)
		require.True(t, ok)
		require.Equal(t, w, v.AsObject().ClassName(), name)
	}
	// Host-converted objects enumerate sorted keys.
	hv, err := f.r.FromGo(map[string]any{"b": 1, "a": 2, "10": 3, "9": 4, "c": map[string]any{"z": 1, "y": 2}})
	require.NoError(t, err)
	keys := jsCall(t, f.r, jsGlobal(t, f.r, "Object"), "keys", hv)
	require.Equal(t, []any{"9", "10", "a", "b", "c"}, f.r.ToGo(keys))
	names := jsCall(t, f.r, jsGlobal(t, f.r, "Object"), "getOwnPropertyNames", hv)
	require.Equal(t, []any{"9", "10", "a", "b", "c"}, f.r.ToGo(names))
}

// ---------------------------------------------------------------------------
// 7. Misc probes used for the code-health review
// ---------------------------------------------------------------------------

func TestAuditA_ZeroValueIsNumberZero(t *testing.T) {
	// Documents the storage assumption behind the slice finding: the zero
	// Value reads as the number +0, not as a hole.
	var v Value
	require.True(t, v.IsNumber())
	require.Equal(t, 0.0, v.AsNumber())
	require.False(t, v.IsHole())
}

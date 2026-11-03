package engine

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newShared() *Realm { return NewRealmWith(RealmOptions{SharedIntrinsics: true}) }

func TestSharedRealmsShareFrozenIntrinsics(t *testing.T) {
	r1 := newShared()
	r2 := newShared()
	assert.True(t, r1.HasSharedIntrinsics())
	assert.Same(t, r1.ObjectPrototype, r2.ObjectPrototype)
	assert.Same(t, r1.ArrayCtor, r2.ArrayCtor)
	assert.Same(t, r1.ErrorPrototypeFor(KindTypeError), r2.ErrorPrototypeFor(KindTypeError))
	assert.Same(t, r1.Math, r2.Math)
	assert.NotSame(t, r1.Global, r2.Global)
	assert.Same(t, r1.Global.Shape(), r2.Global.Shape(), "global shape comes from the template")
	assert.True(t, r1.Global.Shape().IsShared())

	// Everything reachable from the intrinsics is frozen and shared.
	seen := map[*Object]bool{}
	var walk func(o *Object)
	walk = func(o *Object) {
		if o == nil || seen[o] {
			return
		}
		seen[o] = true
		assert.True(t, o.IsShared(), o.debugString())
		assert.True(t, o.IsFrozen(), o.debugString())
		assert.False(t, o.IsExtensible())
		assert.True(t, o.Shape().IsShared())
		if fd := o.FunctionData(); fd != nil {
			assert.Nil(t, fd.Realm(), "shared natives carry no realm")
		}
		for _, k := range o.OwnPropertyKeys() {
			d, _ := o.GetOwnProperty(k)
			assert.False(t, d.Configurable(), "%s.%s", o.debugString(), k.GoString())
			if d.IsAccessorDescriptor() {
				walk(d.GetterObject())
				walk(d.SetterObject())
				continue
			}
			assert.False(t, d.Writable(), "%s.%s", o.debugString(), k.GoString())
			if d.Value.IsObject() {
				walk(d.Value.AsObject())
			}
		}
		walk(o.Proto())
	}
	for _, o := range []*Object{r1.ObjectPrototype, r1.FunctionPrototype, r1.ArrayPrototype, r1.ObjectCtor, r1.Math, r1.JSON, r1.ErrorConstructorFor(KindURIError)} {
		walk(o)
	}
	assert.Greater(t, len(seen), 12)
	assert.False(t, r1.Global.IsShared(), "the global object is per realm")
	assert.True(t, r1.Global.IsExtensible())

	// globalThis points at each realm's own global; bindings are read-only.
	gt, _ := r1.Global.GetProp(r1, StringKey(AtomGlobalThis))
	assert.Same(t, r1.Global, gt.AsObject())
	gt, _ = r2.Global.GetProp(r2, StringKey(AtomGlobalThis))
	assert.Same(t, r2.Global, gt.AsObject())
	ov, _ := r1.Global.GetProp(r1, StringKey(AtomObject))
	assert.Same(t, r1.ObjectCtor, ov.AsObject())
	assert.ErrorContains(t, r1.Global.SetProp(r1, StringKey(AtomObject), IntValue(1)), "read only")
	assert.False(t, r1.Global.Delete(r1, StringKey(AtomArray)))

	// Host globals are per realm and do not disturb the other realm; the
	// extended global's shape is shared process-wide (every realm adds the
	// same host globals), its slots are not.
	mustSet(t, r1, r1.Global, "utils", IntValue(1))
	assert.True(t, r1.Global.HasOwnProperty(key(r1, "utils")))
	assert.False(t, r2.Global.HasOwnProperty(key(r2, "utils")))
	assert.NotSame(t, r1.Global.Shape(), r2.Global.Shape())
	assert.True(t, r1.Global.Shape().IsShared(), "the extended global's shape is cached process-wide")
	assert.True(t, r1.Global.Shape().parent.IsShared())
	mustSet(t, r2, r2.Global, "utils", IntValue(2))
	assert.Same(t, r1.Global.Shape(), r2.Global.Shape())
	v1, _ := r1.Global.GetProp(r1, key(r1, "utils"))
	v2, _ := r2.Global.GetProp(r2, key(r2, "utils"))
	assert.Equal(t, IntValue(1), v1)
	assert.Equal(t, IntValue(2), v2)
	// A different attribute set or order is a different shape.
	r3 := newShared()
	r3.Global.DefineOwnDataFast(r3, key(r3, "utils"), IntValue(3), attrHidden)
	assert.NotSame(t, r1.Global.Shape(), r3.Global.Shape())
	assert.True(t, r3.Global.Shape().IsShared())
}

// TestSharedTransitionCacheBounds checks that concurrent realms extending
// their globals agree on the cached shapes (run with -race), and that a
// shared shape stops publishing children at maxSharedTransitions: past it the
// transition continues in the realm's own tree, from the local equivalent of
// the shared shape.
func TestSharedTransitionCacheBounds(t *testing.T) {
	r := newShared()
	addCodeKeys([]PropertyKey{key(r, "conc_a"), key(r, "conc_b"), key(r, "bound_root")})
	var wg sync.WaitGroup
	results := make([]*Shape, 16)
	for i := range results {
		wg.Go(func() {
			rr := newShared()
			mustSet(t, rr, rr.Global, "conc_a", IntValue(1))
			mustSet(t, rr, rr.Global, "conc_b", IntValue(2))
			v, _ := rr.Global.GetProp(rr, key(rr, "conc_b"))
			if v != IntValue(2) {
				t.Error("wrong value")
			}
			results[i] = rr.Global.Shape()
		})
	}
	wg.Wait()
	for _, s := range results[1:] {
		assert.Same(t, results[0], s)
	}
	assert.True(t, results[0].IsShared())

	var probes []PropertyKey
	for i := range maxSharedTransitions + 8 {
		probes = append(probes, key(r, "bound_probe_"+NumberToGoString(float64(i))))
	}
	addCodeKeys(probes)
	rootObj := r.NewObject()
	mustSet(t, r, rootObj, "bound_root", IntValue(0))
	base := rootObj.Shape()
	require.True(t, base.IsShared())
	var shapes []*Shape
	for i, k := range probes {
		rr := newShared()
		o := rr.NewObject()
		mustSet(t, rr, o, "bound_root", IntValue(0))
		require.NoError(t, o.SetProp(rr, k, IntValue(i)))
		shapes = append(shapes, o.Shape())
	}
	cached := 0
	for _, s := range shapes {
		if s.IsShared() {
			assert.Same(t, base, s.parent)
			cached++
			continue
		}
		assert.Equal(t, base.Props(), s.parent.Props(), "the local parent is equivalent to the shared one")
		assert.Same(t, base.Proto(), s.Proto())
		assert.False(t, s.parent.IsShared())
	}
	assert.Equal(t, maxSharedTransitions, cached)
	assert.False(t, shapes[len(shapes)-1].IsShared(), "past the cap the child is realm-local")
	// Realm-local children still resolve and stay independent.
	o := r.NewObject()
	mustSet(t, r, o, "bound_root", IntValue(0))
	last := probes[len(probes)-1]
	require.NoError(t, o.SetProp(r, last, IntValue(8)))
	mustSet(t, r, o, "bound_probe_0", IntValue(7))
	v, _ := o.GetProp(r, last)
	assert.Equal(t, IntValue(8), v)
	v, _ = o.GetProp(r, key(r, "bound_probe_0"))
	assert.Equal(t, IntValue(7), v)
}

// sharedModules compiles src once and evaluates it in n fresh shared realms.
func sharedModules(t *testing.T, src string, n int) []*moduleFixture {
	t.Helper()
	m, err := syntax.ParseModule("shared.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	out := make([]*moduleFixture, n)
	for i := range out {
		r := newShared()
		env, err := r.EvaluateModule(code)
		require.NoError(t, err)
		out[i] = &moduleFixture{t: t, r: r, env: env}
	}
	return out
}

// object calls the exported function and returns the object it returns.
func (f *moduleFixture) object(name string) *Object {
	f.t.Helper()
	fn, ok := f.env.GetBindingValue(name)
	require.True(f.t, ok, "export %q", name)
	v, err := f.r.Call(fn, Undefined(), nil)
	require.NoError(f.t, err)
	return v.AsObject()
}

// TestSharedShapeTree checks the process-wide shape tree of shared realms:
// the same code-bound keys in the same order give the same shape in every
// realm, a branch does not disturb the props its sibling extended in place,
// big shapes build their table on first lookup, names only data supplies
// and realm symbols are bounded per shape, and past maxSharedShapes the
// realm continues in its own tree, where same-keyed objects still share.
func TestSharedShapeTree(t *testing.T) {
	var big strings.Builder
	big.WriteString("export function big() { return {")
	for i := range 20 {
		fmt.Fprintf(&big, "tree_big_%d: %d, ", i, i)
	}
	big.WriteString("}; }\n")
	src := `
export function abc() { return {tree_a: 1, tree_b: 2, tree_c: 3}; }
export function abd() { const o = {}; o.tree_a = 1; o.tree_b = 2; o.tree_d = 4; return o; }
export function dataRoot() { return {tree_data_root: 1}; }
export function dataRootCode() { const o = {tree_data_root: 1}; o.tree_data_code = 2; return o; }
export function conc() {
  const o = {};
  for (let i = 0; i < 24; i++) o["tree_conc_" + i] = i;
  return {tree_conc_0: 0, tree_conc_1: 1, tree_conc_2: 2, tree_conc_3: 3, tree_conc_4: 4, tree_conc_5: 5,
    tree_conc_6: 6, tree_conc_7: 7, tree_conc_8: 8, tree_conc_9: 9, tree_conc_10: 10, tree_conc_11: 11};
}
` + big.String()
	fs := sharedModules(t, src, 3)
	f1, f2, f3 := fs[0], fs[1], fs[2]
	names := func(s *Shape) (out []string) {
		for _, p := range s.Props() {
			out = append(out, p.key.GoString())
		}
		return out
	}
	a, b, c := f1.object("abc"), f2.object("abc"), f2.object("abd")
	assert.Same(t, a.Shape(), b.Shape())
	assert.True(t, a.Shape().IsShared())
	assert.Same(t, a.Shape().parent, c.Shape().parent)
	assert.Equal(t, []string{"tree_a", "tree_b", "tree_c"}, names(a.Shape()))
	assert.Equal(t, []string{"tree_a", "tree_b", "tree_d"}, names(c.Shape()))
	assert.Equal(t, []string{"tree_a", "tree_b"}, names(c.Shape().parent))

	bo := f1.object("big")
	assert.Nil(t, bo.Shape().table, "published shapes carry no eager table")
	for i := range 20 {
		slot, attrs, ok := bo.Shape().Lookup(key(f1.r, "tree_big_"+NumberToGoString(float64(i))))
		require.True(t, ok, i)
		assert.Equal(t, uint32(i), slot)
		assert.Equal(t, attrDefault, attrs)
	}
	_, _, ok := bo.Shape().Lookup(key(f1.r, "tree_a"))
	assert.False(t, ok)
	assert.Same(t, bo.Shape(), f2.object("big").Shape())

	// Concurrent realms publishing the same chains agree (run with -race).
	var wg sync.WaitGroup
	got := make([]*Shape, 16)
	more := sharedModules(t, src, len(got))
	for i := range got {
		wg.Go(func() {
			f := more[i]
			fn, _ := f.env.GetBindingValue("conc")
			v, err := f.r.Call(fn, Undefined(), nil)
			if err != nil {
				t.Error(err)
				return
			}
			o := v.AsObject()
			for j := range 12 {
				if slot, _, ok := o.Shape().Lookup(key(f.r, "tree_conc_"+NumberToGoString(float64(j)))); !ok || slot != uint32(j) {
					t.Error("lookup", j)
				}
			}
			got[i] = o.Shape()
		})
	}
	wg.Wait()
	for _, s := range got[1:] {
		assert.Same(t, got[0], s)
	}
	assert.True(t, got[0].IsShared())

	// Names only data supplies take at most maxSharedDataTransitions children
	// of a shape; code-bound names still publish past them.
	root := f1.object("dataRoot").Shape()
	require.True(t, root.IsShared())
	cached := 0
	for i := range maxSharedDataTransitions + 4 {
		f := fs[i%len(fs)]
		o := f.object("dataRoot")
		mustSet(t, f.r, o, "tree_data_"+NumberToGoString(float64(i)), IntValue(i))
		if o.Shape().IsShared() {
			assert.Same(t, root, o.Shape().parent)
			cached++
		} else {
			assert.Equal(t, root.Props(), o.Shape().parent.Props())
		}
		v, _ := o.GetProp(f.r, key(f.r, "tree_data_"+NumberToGoString(float64(i))))
		assert.Equal(t, IntValue(i), v)
	}
	assert.Equal(t, maxSharedDataTransitions, cached)
	assert.Equal(t, maxSharedDataTransitions, int(root.sharedTrans.Load().data))
	assert.True(t, f3.object("dataRootCode").Shape().IsShared())
	sym := f1.r.NewObject()
	require.NoError(t, sym.SetProp(f1.r, SymbolKey(NewSymbol(nil)), IntValue(1)))
	assert.False(t, sym.Shape().IsShared(), "realm symbols stay in the realm")
	it := f1.r.NewObject()
	require.NoError(t, it.SetProp(f1.r, SymbolKey(SymIterator), IntValue(1)))
	assert.True(t, it.Shape().IsShared())

	// Past the process-wide bound the realm localizes.
	sharedTransMu.Lock()
	saved := sharedShapeCount
	sharedShapeCount = maxSharedShapes
	sharedTransMu.Unlock()
	defer func() {
		sharedTransMu.Lock()
		sharedShapeCount = saved
		sharedTransMu.Unlock()
	}()
	r3 := f3.r
	addCodeKeys([]PropertyKey{key(r3, "tree_local")})
	build := func() *Object {
		o := r3.NewObject()
		mustSet(t, r3, o, "tree_a", IntValue(1))
		mustSet(t, r3, o, "tree_b", IntValue(2))
		mustSet(t, r3, o, "tree_local", IntValue(3))
		return o
	}
	x, y := build(), build()
	assert.False(t, x.Shape().IsShared())
	assert.Same(t, x.Shape(), y.Shape())
	assert.Equal(t, []string{"tree_a", "tree_b", "tree_local"}, names(x.Shape()))
	assert.Same(t, r3.ObjectPrototype, x.Shape().Proto())
	assert.Same(t, a.Shape(), f3.object("abc").Shape(), "published shapes are still found")
	n := r3.NewObjectWithProto(nil)
	mustSet(t, r3, n, "tree_local", IntValue(1))
	assert.False(t, n.Shape().IsShared())
	assert.Nil(t, n.Shape().Proto())
	v, _ := n.GetProp(r3, key(r3, "tree_local"))
	assert.Equal(t, IntValue(1), v)
}

func TestSharedIntrinsicWriteGuards(t *testing.T) {
	r := newShared()
	op := r.ObjectPrototype
	k := key(r, "extra")
	// [[Set]] with the shared object as receiver returns false (a frozen
	// object); the strict SetProp throws the shared-intrinsic TypeError.
	ok, err := op.Set(r, k, IntValue(1), ObjectValue(op))
	assert.NoError(t, err)
	assert.False(t, ok)
	err = op.SetProp(r, k, IntValue(1))
	assert.EqualError(t, err, "TypeError: Cannot modify property 'extra' of shared intrinsic [object Object]")
	err = op.SetProp(r, StringKey(AtomToString), IntValue(1))
	assert.ErrorContains(t, err, "shared intrinsic")
	// [[DefineOwnProperty]].
	_, err = op.DefineOwnProperty(r, k, DataDescriptor(IntValue(1), attrDefault))
	assert.ErrorContains(t, err, "shared intrinsic")
	assert.ErrorContains(t, op.DefinePropertyOrThrow(r, k, DataDescriptor(IntValue(1), attrDefault)), "shared intrinsic")
	cur, found := op.GetOwnProperty(StringKey(AtomToString))
	require.True(t, found)
	ok, err = op.DefineOwnProperty(r, StringKey(AtomToString), cur)
	assert.NoError(t, err)
	assert.True(t, ok, "a descriptor that changes nothing succeeds on a frozen object")
	// [[Delete]] and prototype/integrity changes.
	assert.False(t, op.Delete(r, StringKey(AtomToString)))
	assert.ErrorContains(t, op.DeletePropertyOrThrow(r, StringKey(AtomToString)), "Cannot delete")
	assert.False(t, r.ArrayPrototype.SetPrototypeOf(r, nil))
	assert.Same(t, op, r.ArrayPrototype.Proto())
	op.PreventExtensions(r)
	op.Seal(r)
	op.Freeze(r)
	assert.True(t, op.IsFrozen())
	// Arrays.
	assert.False(t, r.ArrayPrototype.Push(r, IntValue(1)))
	_, err = r.ArrayPrototype.SetLength(r, 3)
	assert.ErrorContains(t, err, "shared intrinsic")
	assert.Equal(t, uint32(0), r.ArrayPrototype.ArrayLength())
	// The stack setter refuses shared receivers.
	_, err = r.Call(ObjectValue(r.errorStackAccessor.Set), ObjectValue(r.ErrorPrototype), []Value{IntValue(1)})
	assert.ErrorContains(t, err, "shared intrinsic")
	// Internal helpers panic instead of racing.
	assert.Panics(t, func() { op.DefineOwnDataFast(r, k, IntValue(1), attrDefault) })
	assert.Panics(t, func() { op.DefineOwnAccessorFast(r, k, nil, nil, 0) })
	assert.Panics(t, func() { op.SetSlot(0, IntValue(1)) })
	assert.Panics(t, func() { op.SetInternal(1) })
	assert.Panics(t, func() { op.ReserveSlots(r, 4) })
	assert.Panics(t, func() { op.DefineLazyProperty(r, k, func(*Realm) Value { return Undefined() }) })
	// Nothing leaked into the shared object.
	assert.False(t, op.HasOwnProperty(k))
	tv, _ := op.GetProp(r, StringKey(AtomToString))
	assert.True(t, IsCallable(tv))
}

func TestSharedRealmLocalObjectsStayMutable(t *testing.T) {
	r := newShared()
	o := r.NewObject()
	mustSet(t, r, o, "x", IntValue(1))
	assert.False(t, o.IsShared())
	assert.Same(t, r.ObjectPrototype, o.Proto())
	// Shadowing a frozen intrinsic data property on a local object works
	// (override-mistake fix) and does not touch the intrinsic.
	f := r.NewNativeFunction(AtomEmpty, 0, func(*Realm, Value, []Value) (Value, error) { return str("mine"), nil })
	require.NoError(t, o.SetProp(r, StringKey(AtomToString), ObjectValue(f)))
	assert.True(t, o.HasOwnProperty(StringKey(AtomToString)))
	s, err := r.ToString(ObjectValue(o))
	require.NoError(t, err)
	assert.Equal(t, "mine", s.GoString())
	tv, _ := r.ObjectPrototype.GetProp(r, StringKey(AtomToString))
	assert.NotSame(t, f, tv.AsObject())
	e := r.NewError(KindTypeError, "x")
	require.NoError(t, e.SetProp(r, StringKey(AtomName), str("Custom")))
	assert.Equal(t, "Custom: x", (&Exception{Value: ObjectValue(e)}).Error())
	nv, _ := r.ErrorPrototypeFor(KindTypeError).GetProp(r, StringKey(AtomName))
	assert.Equal(t, "TypeError", nv.AsString().GoString())
	// A non-writable inherited property on a *local* prototype still rejects.
	proto := r.NewObject()
	proto.DefineOwnDataFast(r, key(r, "ro"), IntValue(1), attrEnumerable)
	child := r.NewObjectWithProto(proto)
	ok, err := child.Set(r, key(r, "ro"), IntValue(2), ObjectValue(child))
	require.NoError(t, err)
	assert.False(t, ok)
	// Errors, arrays, functions and wrappers work as in mutable realms.
	assert.EqualError(t, r.TypeError("t"), "TypeError: t")
	ok, _ = r.InstanceOf(ObjectValue(e), ObjectValue(r.ErrorConstructorFor(KindError)))
	assert.True(t, ok)
	arr := r.NewArray(IntValue(1))
	assert.True(t, arr.Push(r, IntValue(2)))
	assert.Same(t, r.ArrayPrototype, arr.Proto())
	v, err := r.Call(ObjectValue(r.ArrayCtor), Undefined(), []Value{IntValue(3)})
	require.NoError(t, err)
	assert.Equal(t, uint32(3), v.AsObject().ArrayLength())
	ts, _ := r.GetV(IntValue(255), StringKey(AtomToString))
	res, err := r.Call(ts, IntValue(255), []Value{IntValue(16)})
	require.NoError(t, err)
	assert.Equal(t, "ff", res.AsString().GoString())
	bound, err := r.NewBoundFunction(r.ArrayCtor, Undefined(), nil)
	require.NoError(t, err)
	assert.Same(t, r.FunctionPrototype, bound.Proto())
	// FromGo objects of two realms stay apart (their shapes may be shared).
	r2 := newShared()
	a, _ := r.FromGo(map[string]any{"model": 1})
	b, _ := r2.FromGo(map[string]any{"model": 1})
	require.NoError(t, a.AsObject().SetProp(r, key(r, "model"), IntValue(2)))
	bv, _ := b.AsObject().GetProp(r2, key(r2, "model"))
	assert.Equal(t, IntValue(1), bv)
}

func TestProcessWideInterning(t *testing.T) {
	r1 := newShared()
	r2 := NewRealm()
	a := r1.Intern(FromGoString("hostProvidedName"))
	b := r2.Intern(FromGoString("hostProvidedName"))
	assert.Same(t, a, b, "dynamic atoms are process-wide")
	assert.Equal(t, a.atom, b.atom)
	assert.NotZero(t, a.Hash())
	assert.Same(t, a, r1.InternGoString("hostProvidedName"))
	assert.Same(t, a, r2.KeyFromGoString("hostProvidedName").String())
	u := r1.Intern(FromGoString("ключ"))
	assert.Same(t, u, r2.Intern(FromGoString("ключ")))
	assert.Same(t, u, r2.InternGoString("ключ"))
	// Static atoms take precedence and are pre-hashed.
	assert.Same(t, AtomPush, r1.InternGoString("push"))
	assert.NotZero(t, AtomPush.hash)
	// Interning is safe from many goroutines.
	var wg sync.WaitGroup
	results := make([]*String, 16)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newShared()
			results[i] = r.Intern(FromGoString("concurrentName"))
		}()
	}
	wg.Wait()
	for _, s := range results {
		assert.Same(t, results[0], s)
	}
}

func TestSharedRealmsConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newShared()
			for i := range 200 {
				o := r.NewObject()
				require.NoError(t, o.SetProp(r, key(r, "n"), IntValue(i)))
				s, err := r.ToString(ObjectValue(o))
				require.NoError(t, err)
				assert.Equal(t, "[object Object]", s.GoString())
				e := r.NewError(KindTypeError, "g%d", g)
				sv, err := e.GetProp(r, StringKey(AtomStack))
				require.NoError(t, err)
				assert.Contains(t, sv.AsString().GoString(), "TypeError: g")
				keys := r.ObjectPrototype.OwnPropertyKeys()
				assert.NotEmpty(t, keys)
				_, _, ok := r.ArrayPrototype.Shape().Lookup(StringKey(AtomConstructor))
				assert.True(t, ok)
				_, err = o.Set(r, StringKey(AtomToString), IntValue(1), ObjectValue(o))
				require.NoError(t, err)
			}
		}()
	}
	wg.Wait()
}

func TestSharedRealmRetainedHeap(t *testing.T) {
	if icCensus {
		t.Skip("the inline-cache census grows its tables through the whole run")
	}
	newShared() // build the template outside the measurement
	const n = 512
	realms := make([]*Realm, 0, n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range n {
		realms = append(realms, newShared())
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	// Signed: other tests' garbage collected in between can make it negative.
	perRealm := (float64(after.HeapInuse) - float64(before.HeapInuse)) / n
	t.Logf("retained per shared realm: %.0f bytes (HeapInuse delta / %d)", perRealm, n)
	assert.Less(t, perRealm, 32*1024.0)
	runtime.KeepAlive(realms)
}

func TestSharedRealmAllocations(t *testing.T) {
	newShared()
	allocs := testing.AllocsPerRun(100, func() { newShared() })
	t.Logf("allocs per shared realm: %.0f", allocs)
	assert.LessOrEqual(t, allocs, 20.0)
}

func BenchmarkNewRealmShared(b *testing.B) {
	newShared()
	b.ReportAllocs()
	for b.Loop() {
		newShared()
	}
}

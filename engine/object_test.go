package engine

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func key(r *Realm, s string) PropertyKey { return r.KeyFromGoString(s) }

func keyNames(keys []PropertyKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.GoString()
	}
	return out
}

func mustSet(t *testing.T, r *Realm, o *Object, name string, v Value) {
	t.Helper()
	require.NoError(t, o.SetProp(r, key(r, name), v))
}

func TestPropertyEnumerationOrder(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	for _, n := range []string{"b", "2", "a", "1", "c", "0", "z"} {
		mustSet(t, r, o, n, IntValue(1))
	}
	o.DefineOwnDataFast(r, SymbolKey(SymIterator), IntValue(1), attrDefault)
	assert.Equal(t, []string{"0", "1", "2", "b", "a", "c", "z", "Symbol(Symbol.iterator)"}, keyNames(o.OwnPropertyKeys()))
	assert.Equal(t, []string{"0", "1", "2", "b", "a", "c", "z"}, keyNames(o.OwnEnumerableStringKeys()))

	// Non-tail delete switches to dictionary mode but preserves order.
	assert.True(t, o.Delete(r, key(r, "a")))
	assert.True(t, o.IsDictionaryMode())
	assert.Equal(t, []string{"0", "1", "2", "b", "c", "z"}, keyNames(o.OwnEnumerableStringKeys()))
	mustSet(t, r, o, "a", IntValue(2))
	mustSet(t, r, o, "5", IntValue(2))
	assert.Equal(t, []string{"0", "1", "2", "5", "b", "c", "z", "a"}, keyNames(o.OwnEnumerableStringKeys()))
	v, err := o.GetProp(r, key(r, "a"))
	require.NoError(t, err)
	assert.Equal(t, IntValue(2), v)

	// Sparse index far away still sorts numerically.
	mustSet(t, r, o, "100000", IntValue(3))
	mustSet(t, r, o, "3", IntValue(3))
	assert.Equal(t, []string{"0", "1", "2", "3", "5", "100000", "b", "c", "z", "a"}, keyNames(o.OwnEnumerableStringKeys()))
}

func TestShapeSharingAndDivergence(t *testing.T) {
	r := NewRealm()
	a := r.NewObject()
	b := r.NewObject()
	c := r.NewObject()
	for _, o := range []*Object{a, b} {
		mustSet(t, r, o, "x", IntValue(1))
		mustSet(t, r, o, "y", IntValue(2))
	}
	mustSet(t, r, c, "y", IntValue(2))
	mustSet(t, r, c, "x", IntValue(1))
	assert.Same(t, a.Shape(), b.Shape(), "same key order shares a shape")
	assert.NotSame(t, a.Shape(), c.Shape(), "different key order diverges")
	assert.Equal(t, 2, a.Shape().Count())

	slot, attrs, ok := a.Shape().Lookup(key(r, "y"))
	assert.True(t, ok)
	assert.Equal(t, uint32(1), slot)
	assert.Equal(t, attrDefault, attrs)
	assert.Equal(t, IntValue(2), a.Slot(slot))

	// Different prototype => different root => different shape.
	d := r.NewObjectWithProto(nil)
	mustSet(t, r, d, "x", IntValue(1))
	mustSet(t, r, d, "y", IntValue(2))
	assert.NotSame(t, a.Shape(), d.Shape())
	assert.Nil(t, d.Shape().Proto())
	assert.Same(t, r.ObjectPrototype, a.Shape().Proto())
}

// TestAddChainMatchesAddProperty checks that a shape chain built in one step
// (host objects) is the same chain addProperty builds key by key: shared
// prefixes are reused, later single steps find the slab transitions, and
// lookups see every key at its slot.
func TestAddChainMatchesAddProperty(t *testing.T) {
	r := NewRealm()
	names := []string{"base64", "hasCapability", "hmacSHA256", "json", "jwtSignHS256", "unixNow", "uuid", "volcSignV4", "x0", "x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8"}
	keys := make([]PropertyKey, len(names))
	for i, n := range names {
		keys[i] = key(r, n)
	}
	viaProps := r.plainRoot
	for _, k := range keys[:3] {
		viaProps = viaProps.addProperty(r, k, attrDefault)
	}
	chain := r.plainRoot.addChain(r, keys, attrDefault)
	assert.Equal(t, len(keys), chain.Count())
	for i, k := range keys {
		slot, attrs, ok := chain.Lookup(k)
		assert.True(t, ok, names[i])
		assert.Equal(t, uint32(i), slot)
		assert.Equal(t, attrDefault, attrs)
	}
	// The first three shapes were reused and the rest were cached as
	// ordinary transitions.
	step := r.plainRoot
	for _, k := range keys {
		step = step.addProperty(r, k, attrDefault)
	}
	assert.Same(t, chain, step)
	assert.Same(t, chain, r.plainRoot.addChain(r, keys, attrDefault))
	prefix := r.plainRoot.addChain(r, keys[:3], attrDefault)
	assert.Same(t, viaProps, prefix)
	// A different attribute set diverges at the first differing key.
	other := r.plainRoot.addChain(r, keys[:5], attrHidden)
	assert.NotSame(t, chain, other)
	assert.Equal(t, 5, other.Count())
	// Objects built through FromGo share the chain.
	m := map[string]any{}
	for _, n := range names {
		m[n] = 1
	}
	v1 := mustFromGo(t, r, m)
	v2 := mustFromGo(t, r, m)
	assert.Same(t, v1.AsObject().Shape(), v2.AsObject().Shape())
	assert.Equal(t, len(names), v1.AsObject().Shape().Count())
}

func TestTailDeleteReturnsToParentShape(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	mustSet(t, r, o, "x", IntValue(1))
	s1 := o.Shape()
	mustSet(t, r, o, "y", IntValue(2))
	assert.True(t, o.Delete(r, key(r, "y")))
	assert.Same(t, s1, o.Shape())
	assert.False(t, o.IsDictionaryMode())
	assert.False(t, o.HasOwnProperty(key(r, "y")))
	mustSet(t, r, o, "y", IntValue(3))
	v, _ := o.GetProp(r, key(r, "y"))
	assert.Equal(t, IntValue(3), v)
	// Deleting a missing property succeeds and changes nothing.
	assert.True(t, o.Delete(r, key(r, "nope")))
}

func TestShapeTableAboveThreshold(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	for i, n := range names {
		mustSet(t, r, o, n, IntValue(i))
	}
	assert.False(t, o.IsDictionaryMode())
	for i, n := range names {
		slot, _, ok := o.Shape().Lookup(key(r, n))
		require.True(t, ok, n)
		assert.Equal(t, uint32(i), slot)
	}
	_, _, ok := o.Shape().Lookup(key(r, "zz"))
	assert.False(t, ok)
	assert.Equal(t, names, keyNames(o.OwnPropertyKeys()))
}

func TestDictionaryModeAfter64Props(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	var names []string
	for i := range 70 {
		n := "p" + NumberToGoString(float64(i))
		names = append(names, n)
		mustSet(t, r, o, n, IntValue(i))
	}
	assert.True(t, o.IsDictionaryMode())
	assert.True(t, o.Shape().IsDictionary())
	assert.Same(t, o.Proto(), o.Shape().proto, "the sentinel carries the prototype")
	assert.Equal(t, names, keyNames(o.OwnPropertyKeys()))
	v, err := o.GetProp(r, key(r, "p69"))
	require.NoError(t, err)
	assert.Equal(t, IntValue(69), v)
	// Many deletes trigger compaction and keep order.
	for i := 0; i < 60; i++ {
		assert.True(t, o.Delete(r, key(r, names[i])))
	}
	assert.Equal(t, names[60:], keyNames(o.OwnPropertyKeys()))
}

func TestArrayLengthSemantics(t *testing.T) {
	r := NewRealm()
	t.Run("push grows length", func(t *testing.T) {
		a := r.NewArray()
		assert.True(t, a.Push(r, IntValue(1), IntValue(2)))
		assert.Equal(t, uint32(2), a.ArrayLength())
		mustSet(t, r, a, "5", IntValue(6))
		assert.Equal(t, uint32(6), a.ArrayLength())
		assert.Equal(t, []string{"0", "1", "5", "length"}, keyNames(a.OwnPropertyKeys()))
		v, _ := a.GetIndex(r, 3)
		assert.True(t, v.IsUndefined())
		assert.False(t, a.HasOwnProperty(IndexKey(3)))
		assert.False(t, a.IsDenseArray())
	})
	t.Run("shrink truncates", func(t *testing.T) {
		a := r.NewArray(IntValue(1), IntValue(2), IntValue(3))
		ok, err := a.SetLength(r, 1)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, uint32(1), a.ArrayLength())
		assert.False(t, a.HasOwnProperty(IndexKey(1)))
		assert.Equal(t, []string{"0", "length"}, keyNames(a.OwnPropertyKeys()))
		lv, _ := a.GetProp(r, lengthKey)
		assert.Equal(t, IntValue(1), lv)
	})
	t.Run("grow leaves holes", func(t *testing.T) {
		a := r.NewArray(IntValue(1))
		ok, err := a.SetLength(r, 10)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, uint32(10), a.ArrayLength())
		assert.Equal(t, []string{"0", "length"}, keyNames(a.OwnPropertyKeys()))
	})
	t.Run("invalid length is RangeError", func(t *testing.T) {
		a := r.NewArray()
		for _, bad := range []Value{NumberValue(-1), NumberValue(1.5), NumberValue(4294967296), NaN(), StringValue(FromGoString("abc"))} {
			var d PropertyDescriptor
			d.SetValue(bad)
			_, err := a.DefineOwnProperty(r, lengthKey, d)
			var exc *Exception
			require.ErrorAs(t, err, &exc, "%v", bad)
			assert.Equal(t, "RangeError: Invalid array length", exc.Error())
		}
		// Numeric strings are accepted.
		var d PropertyDescriptor
		d.SetValue(StringValue(FromGoString("3")))
		ok, err := a.DefineOwnProperty(r, lengthKey, d)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, uint32(3), a.ArrayLength())
	})
	t.Run("non-writable length", func(t *testing.T) {
		a := r.NewArray(IntValue(1), IntValue(2))
		var d PropertyDescriptor
		d.SetWritable(false)
		ok, err := a.DefineOwnProperty(r, lengthKey, d)
		require.NoError(t, err)
		assert.True(t, ok)
		ok, err = a.SetLength(r, 5)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, a.Push(r, IntValue(3)))
		ok, err = a.Set(r, IndexKey(7), IntValue(1), ObjectValue(a))
		require.NoError(t, err)
		assert.False(t, ok, "cannot add index beyond frozen length")
		ok, err = a.Set(r, IndexKey(0), IntValue(9), ObjectValue(a))
		require.NoError(t, err)
		assert.True(t, ok, "existing elements stay writable")
		assert.Equal(t, uint32(2), a.ArrayLength())
		err = a.SetProp(r, lengthKey, IntValue(0))
		assert.ErrorContains(t, err, "Cannot assign to read only property 'length'")
		assert.False(t, a.Delete(r, lengthKey), "length is never configurable")
		desc, ok := a.GetOwnProperty(lengthKey)
		require.True(t, ok)
		assert.False(t, desc.Writable())
		assert.False(t, desc.Enumerable())
		assert.False(t, desc.Configurable())
	})
	t.Run("truncate stops at non-configurable element", func(t *testing.T) {
		a := r.NewArray(IntValue(0), IntValue(1), IntValue(2), IntValue(3))
		ok, err := a.DefineOwnProperty(r, IndexKey(2), DataDescriptor(IntValue(2), attrWritable|attrEnumerable))
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = a.SetLength(r, 0)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, uint32(3), a.ArrayLength())
		assert.False(t, a.HasOwnProperty(IndexKey(3)))
		assert.True(t, a.HasOwnProperty(IndexKey(2)))
	})
	t.Run("sparse far write", func(t *testing.T) {
		a := r.NewArray()
		mustSet(t, r, a, "100000", IntValue(1))
		assert.Equal(t, uint32(100001), a.ArrayLength())
		assert.Less(t, len(a.Elements()), 10)
		v, _ := a.GetIndex(r, 100000)
		assert.Equal(t, IntValue(1), v)
		ok, err := a.SetLength(r, 5)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, a.HasOwnProperty(IndexKey(100000)))
	})
	t.Run("NewArrayLen", func(t *testing.T) {
		a := r.NewArrayLen(3)
		assert.Equal(t, uint32(3), a.ArrayLength())
		assert.Empty(t, a.OwnEnumerableStringKeys())
		big := r.NewArrayLen(1 << 20)
		assert.Equal(t, uint32(1<<20), big.ArrayLength())
		assert.Empty(t, big.Elements())
	})
}

func TestAccessorProperties(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	var store Value = IntValue(1)
	getter := r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		return store, nil
	})
	setter := r.NewNativeFunction(AtomEmpty, 1, func(r *Realm, this Value, args []Value) (Value, error) {
		store = Arg(args, 0)
		return Undefined(), nil
	})
	require.NoError(t, o.DefinePropertyOrThrow(r, key(r, "v"), AccessorDescriptor(getter, setter, attrEnumerable|attrConfigurable)))
	v, err := o.GetProp(r, key(r, "v"))
	require.NoError(t, err)
	assert.Equal(t, IntValue(1), v)
	require.NoError(t, o.SetProp(r, key(r, "v"), IntValue(5)))
	assert.Equal(t, IntValue(5), store)
	desc, ok := o.GetOwnProperty(key(r, "v"))
	require.True(t, ok)
	assert.True(t, desc.IsAccessorDescriptor())
	assert.Same(t, getter, desc.GetterObject())
	assert.Same(t, setter, desc.SetterObject())
	_, ok = o.GetOwnDataValue(key(r, "v"))
	assert.False(t, ok)

	// Inherited getter receives the receiver as this.
	child := r.NewObjectWithProto(o)
	var seenThis Value
	thisGetter := r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		seenThis = this
		return IntValue(7), nil
	})
	o.DefineOwnAccessorFast(r, key(r, "g"), thisGetter, nil, attrConfigurable)
	v, err = child.GetProp(r, key(r, "g"))
	require.NoError(t, err)
	assert.Equal(t, IntValue(7), v)
	assert.Equal(t, ObjectValue(child), seenThis)
	// Getter-only inherited accessor rejects assignment.
	ok, err = child.Set(r, key(r, "g"), IntValue(1), ObjectValue(child))
	require.NoError(t, err)
	assert.False(t, ok)
	// Converting data -> accessor on a non-configurable property is rejected.
	o.DefineOwnDataFast(r, key(r, "fixed"), IntValue(1), 0)
	ok, err = o.DefineOwnProperty(r, key(r, "fixed"), AccessorDescriptor(getter, nil, 0))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDefineOwnPropertyValidation(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	k := key(r, "x")
	require.NoError(t, o.DefinePropertyOrThrow(r, k, DataDescriptor(IntValue(1), attrEnumerable)))
	// Non-writable, non-configurable: same value ok, other value rejected.
	var same PropertyDescriptor
	same.SetValue(IntValue(1))
	ok, _ := o.DefineOwnProperty(r, k, same)
	assert.True(t, ok)
	var other PropertyDescriptor
	other.SetValue(IntValue(2))
	ok, _ = o.DefineOwnProperty(r, k, other)
	assert.False(t, ok)
	var cfg PropertyDescriptor
	cfg.SetConfigurable(true)
	ok, _ = o.DefineOwnProperty(r, k, cfg)
	assert.False(t, ok)
	var en PropertyDescriptor
	en.SetEnumerable(false)
	ok, _ = o.DefineOwnProperty(r, k, en)
	assert.False(t, ok)
	// Generic descriptor with nothing present is a no-op success.
	ok, _ = o.DefineOwnProperty(r, k, PropertyDescriptor{})
	assert.True(t, ok)
	assert.ErrorContains(t, o.DefinePropertyOrThrow(r, k, other), "Cannot redefine property: x")
	// Writable -> non-writable is allowed on a configurable property and
	// preserves slot/value.
	k2 := key(r, "y")
	mustSet(t, r, o, "y", IntValue(3))
	var ro PropertyDescriptor
	ro.SetWritable(false)
	ok, _ = o.DefineOwnProperty(r, k2, ro)
	assert.True(t, ok)
	desc, _ := o.GetOwnProperty(k2)
	assert.False(t, desc.Writable())
	assert.True(t, desc.Enumerable())
	assert.Equal(t, IntValue(3), desc.Value)
	assert.ErrorContains(t, o.SetProp(r, k2, IntValue(4)), "read only")
	// Adding to a non-extensible object fails.
	o.PreventExtensions(r)
	ok, _ = o.CreateDataProperty(r, key(r, "z"), IntValue(1))
	assert.False(t, ok)
	// Absent [[Value]] on a new property yields undefined.
	p := r.NewObject()
	var wOnly PropertyDescriptor
	wOnly.SetWritable(true)
	ok, _ = p.DefineOwnProperty(r, k, wOnly)
	assert.True(t, ok)
	v, _ := p.GetProp(r, k)
	assert.True(t, v.IsUndefined())
}

func TestFreezeSeal(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	mustSet(t, r, o, "a", IntValue(1))
	mustSet(t, r, o, "0", IntValue(0))
	twin := r.NewObject()
	mustSet(t, r, twin, "a", IntValue(1))
	assert.False(t, o.IsFrozen())
	assert.False(t, o.IsSealed())
	o.Seal(r)
	assert.True(t, o.IsSealed())
	assert.False(t, o.IsFrozen())
	assert.False(t, o.Delete(r, key(r, "a")))
	assert.False(t, o.Delete(r, IndexKey(0)))
	require.NoError(t, o.SetProp(r, key(r, "a"), IntValue(2)))
	o.Freeze(r)
	assert.True(t, o.IsFrozen())
	assert.ErrorContains(t, o.SetProp(r, key(r, "a"), IntValue(3)), "read only")
	ok, _ := o.Set(r, IndexKey(0), IntValue(3), ObjectValue(o))
	assert.False(t, ok)
	v, _ := o.GetProp(r, key(r, "a"))
	assert.Equal(t, IntValue(2), v)
	assert.False(t, o.IsDictionaryMode(), "freeze keeps shape mode")
	twin.Freeze(r)
	assert.Same(t, o.Shape(), twin.Shape(), "frozen shapes converge in the transition tree")

	arr := r.NewArray(IntValue(1))
	arr.Freeze(r)
	assert.True(t, arr.IsFrozen())
	assert.False(t, arr.Push(r, IntValue(2)))
	ok, err := arr.SetLength(r, 0)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, uint32(1), arr.ArrayLength())

	// Empty non-extensible object is frozen by definition.
	e := r.NewObject()
	e.PreventExtensions(r)
	assert.True(t, e.IsFrozen())
}

func TestStringExoticObject(t *testing.T) {
	r := NewRealm()
	o := r.NewStringObject(FromGoString("héy"))
	assert.Equal(t, ClassString, o.Class())
	assert.Equal(t, "String", o.ClassName())
	v, _ := o.GetIndex(r, 1)
	assert.Equal(t, "é", v.AsString().GoString())
	v, _ = o.GetProp(r, lengthKey)
	assert.Equal(t, IntValue(3), v)
	assert.True(t, o.HasProperty(IndexKey(2)))
	assert.False(t, o.HasProperty(IndexKey(3)))
	assert.False(t, o.Delete(r, IndexKey(0)))
	assert.False(t, o.Delete(r, lengthKey))
	ok, _ := o.Set(r, IndexKey(0), IntValue(1), ObjectValue(o))
	assert.False(t, ok)
	mustSet(t, r, o, "x", IntValue(1))
	mustSet(t, r, o, "7", IntValue(1))
	assert.Equal(t, []string{"0", "1", "2", "7", "length", "x"}, keyNames(o.OwnPropertyKeys()))
	desc, ok := o.GetOwnProperty(IndexKey(0))
	require.True(t, ok)
	assert.True(t, desc.Enumerable())
	assert.False(t, desc.Writable())
	assert.False(t, desc.Configurable())
	// Redefining an index with the same value is allowed, another is not.
	ok, _ = o.DefineOwnProperty(r, IndexKey(0), DataDescriptor(StringValue(FromGoString("h")), attrEnumerable))
	assert.True(t, ok)
	ok, _ = o.DefineOwnProperty(r, IndexKey(0), DataDescriptor(StringValue(FromGoString("x")), attrEnumerable))
	assert.False(t, ok)
	pv, ok := o.PrimitiveValue()
	assert.True(t, ok)
	assert.Equal(t, "héy", pv.AsString().GoString())
	o.Freeze(r)
	assert.Equal(t, []string{"0", "1", "2", "7", "x"}, keyNames(o.OwnEnumerableStringKeys()))
}

func TestSetPrototypeOf(t *testing.T) {
	r := NewRealm()
	a := r.NewObject()
	b := r.NewObject()
	mustSet(t, r, a, "x", IntValue(1))
	before := a.Shape()
	assert.True(t, a.SetPrototypeOf(r, b))
	assert.Same(t, b, a.Proto())
	assert.True(t, b.IsPrototypeObject())
	assert.NotSame(t, before, a.Shape())
	assert.Same(t, b, a.Shape().Proto())
	v, _ := a.GetProp(r, key(r, "x"))
	assert.Equal(t, IntValue(1), v)
	// Cycle rejected.
	assert.False(t, b.SetPrototypeOf(r, a))
	// Non-extensible rejected unless unchanged.
	a.PreventExtensions(r)
	assert.False(t, a.SetPrototypeOf(r, nil))
	assert.True(t, a.SetPrototypeOf(r, b))
	// Inherited lookup.
	mustSet(t, r, b, "y", IntValue(2))
	v, _ = a.GetProp(r, key(r, "y"))
	assert.Equal(t, IntValue(2), v)
	assert.True(t, a.HasProperty(key(r, "y")))
	assert.False(t, a.HasOwnProperty(key(r, "y")))
}

func TestSetThroughPrototypeCreatesOwnProperty(t *testing.T) {
	r := NewRealm()
	proto := r.NewObject()
	mustSet(t, r, proto, "x", IntValue(1))
	child := r.NewObjectWithProto(proto)
	mustSet(t, r, child, "x", IntValue(2))
	assert.True(t, child.HasOwnProperty(key(r, "x")))
	pv, _ := proto.GetProp(r, key(r, "x"))
	assert.Equal(t, IntValue(1), pv)
	// Non-writable inherited property blocks the assignment.
	proto.DefineOwnDataFast(r, key(r, "ro"), IntValue(1), attrEnumerable)
	ok, err := child.Set(r, key(r, "ro"), IntValue(2), ObjectValue(child))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.False(t, child.HasOwnProperty(key(r, "ro")))
}

func TestLazyProperties(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	calls := 0
	o.DefineLazyProperty(r, key(r, "lazy"), func(r *Realm) Value {
		calls++
		return IntValue(42)
	})
	o.DefineLazyProperty(r, key(r, "other"), func(r *Realm) Value { return IntValue(1) })
	assert.Equal(t, 0, calls)
	assert.True(t, o.HasOwnProperty(key(r, "lazy")))
	assert.Equal(t, 1, calls)
	v, _ := o.GetProp(r, key(r, "lazy"))
	assert.Equal(t, IntValue(42), v)
	assert.Equal(t, 1, calls)
	assert.Equal(t, []string{"lazy", "other"}, keyNames(o.OwnPropertyKeys()))
	assert.Empty(t, o.OwnEnumerableStringKeys(), "lazy properties are non-enumerable")
	assert.Equal(t, uint8(0), o.flags&flagHasLazy)
}

func TestGetOwnDataValueAndHasOwn(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	mustSet(t, r, o, "a", IntValue(1))
	v, ok := o.GetOwnDataValue(key(r, "a"))
	assert.True(t, ok)
	assert.Equal(t, IntValue(1), v)
	_, ok = o.GetOwnDataValue(key(r, "b"))
	assert.False(t, ok)
	assert.Equal(t, "Object", o.ClassName())
	assert.Equal(t, ClassObject, o.Class())
}

func TestICEntryValidity(t *testing.T) {
	r := NewRealm()
	proto := r.NewObject()
	mustSet(t, r, proto, "m", IntValue(1))
	o := r.NewObjectWithProto(proto)
	mustSet(t, r, o, "x", IntValue(1))
	slot, _, ok := proto.Shape().Lookup(key(r, "m"))
	require.True(t, ok)
	e := newICEntry(o.Shape(), r.ProtoEpoch(), slot, 1)
	assert.True(t, e.Valid(r, o))
	assert.Same(t, proto, e.Holder(o))
	assert.Equal(t, slot, e.Slot())
	assert.Equal(t, uint8(1), e.HolderDepth())
	assert.Equal(t, IntValue(1), e.Holder(o).Slot(e.Slot()))
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(16), unsafe.Sizeof(e))
	}
	// Adding a property to the prototype invalidates.
	mustSet(t, r, proto, "n", IntValue(2))
	assert.False(t, e.Valid(r, o))
	// A different-shaped receiver never validates.
	other := r.NewObject()
	assert.False(t, e.Valid(r, other))
}

// TestToPropertyKeyNegativeZero checks that -0 converts to the key "0"
// (ToString(-0) is "0"), not the string key "-0".
func TestToPropertyKeyNegativeZero(t *testing.T) {
	accTable(t, [][2]string{
		{`var o = {0: "z"}; return [o[-0], (-0) in o, Object.getOwnPropertyDescriptor(o, -0).value];`, `["z",true,"z"]`},
		{`var o = {}; o[-0] = 1; return Object.keys(o);`, `["0"]`},
		{`var a = ["x"]; return [a[-0], a.hasOwnProperty(-0)];`, `["x",true]`},
	})
}

// TestEnumeratedIndexKeys checks that the key strings for-in, Object.keys
// and Reflect.ownKeys produce (small ones are interned atoms) still name
// the index properties they came from.
func TestEnumeratedIndexKeys(t *testing.T) {
	accTable(t, [][2]string{
		{`var a = [5, 6]; var j = Object.keys(a)[1]; return [a[j], a.hasOwnProperty(j), j in a];`, `[6,true,true]`},
		{`var a = [5]; var r = []; for (var p in a) r.push(a[p], a.hasOwnProperty(p), p in a); return r;`, `[5,true,true]`},
		{`var o = {}; var k = Object.keys([1])[0]; o[k] = 1; return Object.getOwnPropertyDescriptor(o, 0).value;`, `1`},
		{`var a = [1, 2]; var k = Reflect.ownKeys(a)[0]; delete a[k]; return [0 in a, a.length];`, `[false,2]`},
		{`return ({"": 3})[Object.keys({"": 1})[0]];`, `3`},
	})
}

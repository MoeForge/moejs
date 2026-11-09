package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObjectBuiltinShape(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ObjectCtor)
	assertNativeShape(t, r, ctor, "Object", 1)
	for _, c := range []struct {
		name   string
		length int
	}{
		{"assign", 2}, {"create", 2}, {"defineProperties", 2}, {"defineProperty", 3}, {"entries", 1}, {"freeze", 1}, {"fromEntries", 1},
		{"getOwnPropertyDescriptor", 2}, {"getOwnPropertyDescriptors", 1}, {"getOwnPropertyNames", 1}, {"getOwnPropertySymbols", 1}, {"getPrototypeOf", 1},
		{"groupBy", 2}, {"hasOwn", 2}, {"is", 2}, {"isExtensible", 1}, {"isFrozen", 1}, {"isSealed", 1}, {"keys", 1}, {"preventExtensions", 1}, {"seal", 1},
		{"setPrototypeOf", 2}, {"values", 1},
	} {
		assertNativeShape(t, r, jsGet(t, r, ctor, c.name), c.name, c.length)
		assertOwnAttrs(t, r, r.ObjectCtor, c.name, true, false, true)
	}
	proto := ObjectValue(r.ObjectPrototype)
	for _, c := range []struct {
		name   string
		length int
	}{{"__defineGetter__", 2}, {"__defineSetter__", 2}, {"__lookupGetter__", 1}, {"__lookupSetter__", 1},
		{"hasOwnProperty", 1}, {"isPrototypeOf", 1}, {"propertyIsEnumerable", 1}, {"toLocaleString", 0}, {"toString", 0}, {"valueOf", 0}} {
		assertNativeShape(t, r, jsGet(t, r, proto, c.name), c.name, c.length)
		assertOwnAttrs(t, r, r.ObjectPrototype, c.name, true, false, true)
	}
	assert.Empty(t, r.ObjectPrototype.OwnEnumerableStringKeys())
	assert.Nil(t, r.ObjectPrototype.Proto())
}

func TestObjectConstructor(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ObjectCtor)
	for _, v := range []Value{Undefined(), Null()} {
		o, err := r.Call(ctor, Undefined(), []Value{v})
		require.NoError(t, err)
		assert.Equal(t, ClassObject, o.AsObject().Class())
		assert.Same(t, r.ObjectPrototype, o.AsObject().Proto())
	}
	o, _ := r.Call(ctor, Undefined(), nil)
	assert.Equal(t, ClassObject, o.AsObject().Class())
	w, _ := r.Call(ctor, Undefined(), []Value{str("ab")})
	assert.Equal(t, ClassString, w.AsObject().Class())
	assert.Equal(t, IntValue(2), jsGet(t, r, w, "length"))
	existing := r.NewObject()
	same, _ := r.Construct(ctor, []Value{ObjectValue(existing)}, nil)
	assert.Same(t, existing, same.AsObject())
	n, _ := r.Construct(ctor, []Value{IntValue(3)}, nil)
	assert.Equal(t, ClassNumber, n.AsObject().Class())
	nt := r.NewNativeFunction(AtomEmpty, 0, nil)
	custom := r.NewObject()
	nt.DefineOwnDataFast(r, StringKey(AtomPrototype), ObjectValue(custom), attrHidden)
	sub, err := r.Construct(ctor, []Value{IntValue(3)}, nt)
	require.NoError(t, err)
	assert.Same(t, custom, sub.AsObject().Proto())
	assert.Equal(t, ClassObject, sub.AsObject().Class(), "subclass newTarget ignores the value")
}

// TestListedIndexKeysAreIndices: the index names key listing returns (cached
// per realm for small indices) find the elements again when used as keys.
func TestListedIndexKeysAreIndices(t *testing.T) {
	assert.Equal(t, "a,b|a,b|0,1|true,true|0:a,1:b", evalExpr(t, `(() => {
		const a = ["a", "b"], out = [];
		const ks = Object.keys(a), names = Object.getOwnPropertyNames(a);
		const o = {};
		for (const k in a) { out.push(a[k]); o[k] = a[k]; }
		return [ks.map(k => a[k]), out, Object.keys(o), [ks[0] in a, a.hasOwnProperty(names[1])],
			names.slice(0, 2).map(k => k + ":" + Object.getOwnPropertyDescriptor(a, k).value)].join("|");
	})()`))
}

func TestObjectKeysValuesEntries(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ObjectCtor)
	o := r.NewObject()
	for _, n := range []string{"b", "2", "a", "10", "1"} {
		mustSet(t, r, o, n, str("v"+n))
	}
	o.DefineOwnDataFast(r, key(r, "hidden"), IntValue(0), attrHidden)
	o.DefineOwnDataFast(r, SymbolKey(SymIterator), IntValue(0), attrDefault)
	assert.Equal(t, []any{"1", "2", "10", "b", "a"}, r.ToGo(jsCall(t, r, ctor, "keys", ObjectValue(o))))
	assert.Equal(t, []any{"v1", "v2", "v10", "vb", "va"}, r.ToGo(jsCall(t, r, ctor, "values", ObjectValue(o))))
	assert.Equal(t, []any{[]any{"1", "v1"}, []any{"2", "v2"}, []any{"10", "v10"}, []any{"b", "vb"}, []any{"a", "va"}}, r.ToGo(jsCall(t, r, ctor, "entries", ObjectValue(o))))
	// Shape-only fast path and dictionary mode agree.
	plain := r.NewObject()
	mustSet(t, r, plain, "x", IntValue(1))
	mustSet(t, r, plain, "y", IntValue(2))
	plain.DefineOwnDataFast(r, key(r, "z"), IntValue(3), attrHidden)
	assert.Equal(t, []any{"x", "y"}, r.ToGo(jsCall(t, r, ctor, "keys", ObjectValue(plain))))
	assert.True(t, plain.Delete(r, key(r, "x")))
	assert.True(t, plain.IsDictionaryMode())
	assert.Equal(t, []any{"y"}, r.ToGo(jsCall(t, r, ctor, "keys", ObjectValue(plain))))
	assert.Equal(t, []any{int64(2)}, r.ToGo(jsCall(t, r, ctor, "values", ObjectValue(plain))))
	// Primitives are wrapped: strings expose indices, numbers nothing.
	assert.Equal(t, []any{"0", "1"}, r.ToGo(jsCall(t, r, ctor, "keys", str("ab"))))
	assert.Equal(t, []any{"a", "b"}, r.ToGo(jsCall(t, r, ctor, "values", str("ab"))))
	assert.Equal(t, []any{[]any{"0", "a"}}, r.ToGo(jsCall(t, r, ctor, "entries", str("a"))))
	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, ctor, "keys", IntValue(5))))
	assert.Equal(t, []any{"0", "1"}, r.ToGo(jsCall(t, r, ctor, "keys", jsArray(r, IntValue(7), IntValue(8)))))
	holes := r.NewArrayLen(3)
	mustSet(t, r, holes, "1", IntValue(1))
	assert.Equal(t, []any{"1"}, r.ToGo(jsCall(t, r, ctor, "keys", ObjectValue(holes))))
	for _, bad := range []Value{Undefined(), Null()} {
		for _, m := range []string{"keys", "values", "entries"} {
			assert.EqualError(t, jsCallErr(t, r, ctor, m, bad), "TypeError: Cannot convert undefined or null to object")
		}
	}
	assert.EqualError(t, jsCallErr(t, r, ctor, "keys"), "TypeError: Cannot convert undefined or null to object")
	// A getter that deletes a later key: the later key is skipped.
	g := r.NewObject()
	g.DefineOwnAccessorFast(r, key(r, "first"), nativeFn(r, func(this Value, _ []Value) (Value, error) {
		this.AsObject().Delete(r, key(r, "second"))
		return IntValue(1), nil
	}).AsObject(), nil, attrEnumerable|attrConfigurable)
	mustSet(t, r, g, "second", IntValue(2))
	assert.Equal(t, []any{int64(1)}, r.ToGo(jsCall(t, r, ctor, "values", ObjectValue(g))))
	mustSet(t, r, g, "second", IntValue(2))
	assert.Equal(t, []any{[]any{"first", int64(1)}}, r.ToGo(jsCall(t, r, ctor, "entries", ObjectValue(g))))
	// Getter errors propagate.
	e := r.NewObject()
	e.DefineOwnAccessorFast(r, key(r, "boom"), throwingFn(r, "getter").AsObject(), nil, attrEnumerable)
	assert.EqualError(t, jsCallErr(t, r, ctor, "values", ObjectValue(e)), "TypeError: getter")
	// Entries are fresh, independent arrays.
	entries := jsCall(t, r, ctor, "entries", ObjectValue(plain)).AsObject()
	first, _ := entries.GetIndex(r, 0)
	assert.True(t, first.AsObject().Push(r, IntValue(9)))
	assert.Equal(t, []any{"y", int64(2), int64(9)}, r.ToGo(first))
}

func TestObjectAssign(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ObjectCtor)
	target := r.NewObject()
	mustSet(t, r, target, "a", IntValue(1))
	src1 := r.NewObject()
	mustSet(t, r, src1, "b", IntValue(2))
	mustSet(t, r, src1, "a", IntValue(10))
	src1.DefineOwnDataFast(r, key(r, "hidden"), IntValue(0), attrHidden)
	src1.DefineOwnDataFast(r, SymbolKey(SymIterator), str("sym"), attrDefault)
	src2 := r.NewObject()
	mustSet(t, r, src2, "c", IntValue(3))
	mustSet(t, r, src2, "0", IntValue(0))
	res := jsCall(t, r, ctor, "assign", ObjectValue(target), ObjectValue(src1), Null(), Undefined(), ObjectValue(src2))
	assert.Same(t, target, res.AsObject())
	assert.Equal(t, []string{"0", "a", "b", "c", "Symbol(Symbol.iterator)"}, keyNames(target.OwnPropertyKeys()))
	assert.Equal(t, "sym", jsString(t, func() Value { v, _ := target.GetProp(r, SymbolKey(SymIterator)); return v }()))
	assert.Equal(t, map[string]any{"0": int64(0), "a": int64(10), "b": int64(2), "c": int64(3)}, r.ToGo(ObjectValue(target)))
	assert.False(t, target.HasOwnProperty(key(r, "hidden")))
	// Strings spread their indices; other primitives contribute nothing.
	t2 := jsCall(t, r, ctor, "assign", ObjectValue(r.NewObject()), str("xy"), IntValue(5), True())
	assert.Equal(t, map[string]any{"0": "x", "1": "y"}, r.ToGo(t2))
	// Arrays as sources copy their elements.
	t3 := jsCall(t, r, ctor, "assign", ObjectValue(r.NewObject()), jsArray(r, str("p"), str("q")))
	assert.Equal(t, map[string]any{"0": "p", "1": "q"}, r.ToGo(t3))
	// Getters on the source are invoked; setters on the target receive values.
	var log []string
	src := r.NewObject()
	src.DefineOwnAccessorFast(r, key(r, "g"), nativeFn(r, func(Value, []Value) (Value, error) {
		log = append(log, "get g")
		return IntValue(7), nil
	}).AsObject(), nil, attrEnumerable|attrConfigurable)
	tgt := r.NewObject()
	tgt.DefineOwnAccessorFast(r, key(r, "g"), nil, nativeFn(r, func(_ Value, args []Value) (Value, error) {
		log = append(log, "set g="+args[0].String())
		return Undefined(), nil
	}).AsObject(), attrEnumerable|attrConfigurable)
	jsCall(t, r, ctor, "assign", ObjectValue(tgt), ObjectValue(src))
	assert.Equal(t, []string{"get g", "set g=7"}, log)
	// A rejected [[Set]] is a TypeError (frozen target, read-only property).
	frozen := r.NewObject()
	mustSet(t, r, frozen, "a", IntValue(1))
	frozen.Freeze(r)
	err := jsCallErr(t, r, ctor, "assign", ObjectValue(frozen), ObjectValue(src1))
	assert.EqualError(t, err, "TypeError: Cannot assign to read only property 'b' of object")
	ro := r.NewObject()
	ro.DefineOwnDataFast(r, key(r, "a"), IntValue(1), attrEnumerable)
	assert.ErrorContains(t, jsCallErr(t, r, ctor, "assign", ObjectValue(ro), ObjectValue(src1)), "read only property 'a'")
	// Primitive targets are wrapped and returned as the wrapper.
	w := jsCall(t, r, ctor, "assign", IntValue(1), ObjectValue(src2))
	assert.Equal(t, ClassNumber, w.AsObject().Class())
	assert.Equal(t, IntValue(3), jsGet(t, r, w, "c"))
	for _, bad := range []Value{Undefined(), Null()} {
		assert.EqualError(t, jsCallErr(t, r, ctor, "assign", bad), "TypeError: Cannot convert undefined or null to object")
	}
	assert.Equal(t, ClassObject, jsCall(t, r, ctor, "assign", ObjectValue(r.NewObject())).AsObject().Class())
	// Dictionary-mode sources work through the generic path and keep order.
	dict := r.NewObject()
	for _, n := range []string{"k1", "k2", "k3"} {
		mustSet(t, r, dict, n, str(n))
	}
	dict.Delete(r, key(r, "k1"))
	mustSet(t, r, dict, "k1", str("again"))
	t4 := jsCall(t, r, ctor, "assign", ObjectValue(r.NewObject()), ObjectValue(dict))
	assert.Equal(t, []string{"k2", "k3", "k1"}, keyNames(t4.AsObject().OwnEnumerableStringKeys()))
	// Source getters that throw propagate.
	assert.EqualError(t, jsCallErr(t, r, ctor, "assign", ObjectValue(r.NewObject()), func() Value {
		e := r.NewObject()
		e.DefineOwnAccessorFast(r, key(r, "x"), throwingFn(r, "src").AsObject(), nil, attrEnumerable)
		return ObjectValue(e)
	}()), "TypeError: src")
}

func TestObjectFreezeAndPrototypes(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ObjectCtor)
	o := r.NewObject()
	mustSet(t, r, o, "a", IntValue(1))
	assert.Equal(t, False(), jsCall(t, r, ctor, "isFrozen", ObjectValue(o)))
	assert.Same(t, o, jsCall(t, r, ctor, "freeze", ObjectValue(o)).AsObject())
	assert.Equal(t, True(), jsCall(t, r, ctor, "isFrozen", ObjectValue(o)))
	assert.EqualError(t, o.SetProp(r, key(r, "a"), IntValue(2)), "TypeError: Cannot assign to read only property 'a' of object")
	assert.EqualError(t, o.SetProp(r, key(r, "b"), IntValue(2)), "TypeError: Cannot assign to read only property 'b' of object")
	assert.False(t, o.Delete(r, key(r, "a")))
	assert.Equal(t, IntValue(1), jsGet(t, r, ObjectValue(o), "a"))
	arr := r.NewArray(IntValue(1))
	jsCall(t, r, ctor, "freeze", ObjectValue(arr))
	assert.False(t, arr.Push(r, IntValue(2)))
	assert.True(t, arr.IsFrozen())
	for _, prim := range []Value{IntValue(1), str("s"), Undefined(), Null(), True()} {
		assert.Equal(t, prim, jsCall(t, r, ctor, "freeze", prim))
		assert.Equal(t, True(), jsCall(t, r, ctor, "isFrozen", prim))
	}
	assert.Equal(t, False(), jsCall(t, r, ctor, "isFrozen", ObjectValue(r.NewObject())))
	empty := r.NewObject()
	empty.PreventExtensions(r)
	assert.Equal(t, True(), jsCall(t, r, ctor, "isFrozen", ObjectValue(empty)))
	fn := nativeFn(r, func(Value, []Value) (Value, error) { return Undefined(), nil })
	jsCall(t, r, ctor, "freeze", fn)
	assert.True(t, fn.AsObject().IsFrozen())

	// getPrototypeOf / setPrototypeOf.
	assert.Same(t, r.ObjectPrototype, jsCall(t, r, ctor, "getPrototypeOf", ObjectValue(r.NewObject())).AsObject())
	assert.Same(t, r.StringPrototype, jsCall(t, r, ctor, "getPrototypeOf", str("s")).AsObject())
	assert.Same(t, r.NumberPrototype, jsCall(t, r, ctor, "getPrototypeOf", IntValue(1)).AsObject())
	assert.Same(t, r.FunctionPrototype, jsCall(t, r, ctor, "getPrototypeOf", ctor).AsObject())
	assert.True(t, jsCall(t, r, ctor, "getPrototypeOf", ObjectValue(r.ObjectPrototype)).IsNull())
	assert.EqualError(t, jsCallErr(t, r, ctor, "getPrototypeOf", Null()), "TypeError: Cannot convert undefined or null to object")
	p := r.NewObject()
	mustSet(t, r, p, "inherited", IntValue(42))
	c := r.NewObject()
	assert.Same(t, c, jsCall(t, r, ctor, "setPrototypeOf", ObjectValue(c), ObjectValue(p)).AsObject())
	assert.Same(t, p, c.Proto())
	assert.Equal(t, IntValue(42), jsGet(t, r, ObjectValue(c), "inherited"))
	jsCall(t, r, ctor, "setPrototypeOf", ObjectValue(c), Null())
	assert.Nil(t, c.Proto())
	assert.Equal(t, IntValue(7), jsCall(t, r, ctor, "setPrototypeOf", IntValue(7), Null()), "primitives pass through")
	assert.EqualError(t, jsCallErr(t, r, ctor, "setPrototypeOf", Undefined(), Null()), "TypeError: Object.setPrototypeOf called on null or undefined")
	assert.EqualError(t, jsCallErr(t, r, ctor, "setPrototypeOf", ObjectValue(c), IntValue(1)), "TypeError: Object prototype may only be an Object or null: 1")
	assert.EqualError(t, jsCallErr(t, r, ctor, "setPrototypeOf", ObjectValue(c), Undefined()), "TypeError: Object prototype may only be an Object or null: undefined")
	assert.EqualError(t, jsCallErr(t, r, ctor, "setPrototypeOf", ObjectValue(p), ObjectValue(p)), "TypeError: Cyclic __proto__ value")
	jsCall(t, r, ctor, "setPrototypeOf", ObjectValue(c), ObjectValue(p))
	assert.EqualError(t, jsCallErr(t, r, ctor, "setPrototypeOf", ObjectValue(p), ObjectValue(c)), "TypeError: Cyclic __proto__ value")
	assert.EqualError(t, jsCallErr(t, r, ctor, "setPrototypeOf", ObjectValue(o), Null()), "TypeError: [object Object] is not extensible")
	assert.Same(t, o, jsCall(t, r, ctor, "setPrototypeOf", ObjectValue(o), ObjectValue(r.ObjectPrototype)).AsObject(), "unchanged prototype is fine on frozen objects")
}

func TestObjectCreateAndDefineProperty(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ObjectCtor)
	nul := jsCall(t, r, ctor, "create", Null())
	assert.Nil(t, nul.AsObject().Proto())
	assert.True(t, jsGet(t, r, nul, "toString").IsUndefined())
	p := r.NewObject()
	mustSet(t, r, p, "x", IntValue(1))
	// Descriptor objects.
	desc := func(fields map[string]Value) Value {
		d := r.NewObject()
		for _, k := range []string{"value", "writable", "enumerable", "configurable", "get", "set"} {
			if v, ok := fields[k]; ok {
				mustSet(t, r, d, k, v)
			}
		}
		return ObjectValue(d)
	}
	props := r.NewObject()
	mustSet(t, r, props, "a", desc(map[string]Value{"value": IntValue(1), "enumerable": True()}))
	mustSet(t, r, props, "b", desc(map[string]Value{"value": IntValue(2)}))
	var log []string
	getter := nativeFn(r, func(this Value, _ []Value) (Value, error) {
		log = append(log, "get")
		return IntValue(3), nil
	})
	setter := nativeFn(r, func(_ Value, args []Value) (Value, error) {
		log = append(log, "set "+args[0].String())
		return Undefined(), nil
	})
	mustSet(t, r, props, "c", desc(map[string]Value{"get": getter, "set": setter, "enumerable": True(), "configurable": True()}))
	props.DefineOwnDataFast(r, key(r, "skipped"), desc(map[string]Value{"value": IntValue(9)}), attrHidden)
	o := jsCall(t, r, ctor, "create", ObjectValue(p), ObjectValue(props))
	oo := o.AsObject()
	assert.Same(t, p, oo.Proto())
	assert.Equal(t, IntValue(1), jsGet(t, r, o, "x"))
	assertOwnAttrs(t, r, oo, "a", false, true, false)
	assertOwnAttrs(t, r, oo, "b", false, false, false)
	assert.False(t, oo.HasOwnProperty(key(r, "skipped")))
	assert.Equal(t, IntValue(3), jsGet(t, r, o, "c"))
	require.NoError(t, oo.SetProp(r, key(r, "c"), IntValue(4)))
	assert.Equal(t, []string{"get", "set 4"}, log)
	d, _ := oo.GetOwnProperty(key(r, "c"))
	assert.True(t, d.IsAccessorDescriptor())
	assert.Same(t, getter.AsObject(), d.GetterObject())
	assert.Same(t, setter.AsObject(), d.SetterObject())
	assert.Equal(t, []any{"a", "c"}, r.ToGo(jsCall(t, r, ctor, "keys", o)), "enumerable accessors are keys")
	// Validation happens before any definition.
	bad := r.NewObject()
	mustSet(t, r, bad, "ok", desc(map[string]Value{"value": IntValue(1)}))
	mustSet(t, r, bad, "nope", IntValue(5))
	err := jsCallErr(t, r, ctor, "create", Null(), ObjectValue(bad))
	assert.EqualError(t, err, "TypeError: Property description must be an object: 5")
	assert.EqualError(t, jsCallErr(t, r, ctor, "create", IntValue(1)), "TypeError: Object prototype may only be an Object or null: 1")
	assert.EqualError(t, jsCallErr(t, r, ctor, "create"), "TypeError: Object prototype may only be an Object or null: undefined")
	assert.EqualError(t, jsCallErr(t, r, ctor, "create", Null(), Null()), "TypeError: Cannot convert undefined or null to object")

	// defineProperty.
	target := r.NewObject()
	assert.Same(t, target, jsCall(t, r, ctor, "defineProperty", ObjectValue(target), str("v"), desc(map[string]Value{"value": IntValue(1)})).AsObject())
	assertOwnAttrs(t, r, target, "v", false, false, false)
	assert.EqualError(t, target.SetProp(r, key(r, "v"), IntValue(2)), "TypeError: Cannot assign to read only property 'v' of object")
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", ObjectValue(target), str("v"), desc(map[string]Value{"value": IntValue(2)})), "TypeError: Cannot redefine property: v")
	jsCall(t, r, ctor, "defineProperty", ObjectValue(target), str("v"), desc(map[string]Value{"value": IntValue(1)})) // same value is allowed
	jsCall(t, r, ctor, "defineProperty", ObjectValue(target), IntValue(0), desc(map[string]Value{"value": str("idx"), "writable": True(), "enumerable": True(), "configurable": True()}))
	assert.Equal(t, "idx", jsString(t, jsGet(t, r, ObjectValue(target), "0")))
	jsCall(t, r, ctor, "defineProperty", ObjectValue(target), SymbolValue(SymIterator), desc(map[string]Value{"value": IntValue(1)}))
	assert.True(t, target.HasOwnProperty(SymbolKey(SymIterator)))
	// Accessor with only a getter; converting configurable data -> accessor.
	jsCall(t, r, ctor, "defineProperty", ObjectValue(target), str("w"), desc(map[string]Value{"value": IntValue(1), "configurable": True()}))
	jsCall(t, r, ctor, "defineProperty", ObjectValue(target), str("w"), desc(map[string]Value{"get": getter}))
	assert.Equal(t, IntValue(3), jsGet(t, r, ObjectValue(target), "w"))
	ok, err := target.Set(r, key(r, "w"), IntValue(1), ObjectValue(target))
	require.NoError(t, err)
	assert.False(t, ok, "getter-only accessor rejects writes")
	// Descriptor errors.
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", IntValue(1), str("x"), desc(nil)), "TypeError: Object.defineProperty called on non-object")
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", ObjectValue(target), str("x"), str("nope")), "TypeError: Property description must be an object: \"nope\"")
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", ObjectValue(target), str("x"), desc(map[string]Value{"get": IntValue(1)})), "TypeError: Getter must be a function: 1")
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", ObjectValue(target), str("x"), desc(map[string]Value{"set": str("s")})), "TypeError: Setter must be a function: s")
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", ObjectValue(target), str("x"), desc(map[string]Value{"get": getter, "value": IntValue(1)})), "TypeError: Invalid property descriptor. Cannot both specify accessors and a value or writable attribute")
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", ObjectValue(target), str("x"), desc(map[string]Value{"set": setter, "writable": False()})), "TypeError: Invalid property descriptor. Cannot both specify accessors and a value or writable attribute")
	// Descriptor fields are read in spec order through the prototype chain,
	// and get: undefined is accepted.
	inherited := r.NewObjectWithProto(desc(map[string]Value{"enumerable": True()}).AsObject())
	jsCall(t, r, ctor, "defineProperty", ObjectValue(target), str("inh"), ObjectValue(inherited))
	assertOwnAttrs(t, r, target, "inh", false, true, false)
	jsCall(t, r, ctor, "defineProperty", ObjectValue(target), str("undef"), desc(map[string]Value{"get": Undefined(), "configurable": True()}))
	assert.True(t, jsGet(t, r, ObjectValue(target), "undef").IsUndefined())
	// Array length and index interplay.
	arr := r.NewArray(IntValue(1), IntValue(2), IntValue(3))
	jsCall(t, r, ctor, "defineProperty", ObjectValue(arr), str("length"), desc(map[string]Value{"value": IntValue(1)}))
	assert.Equal(t, uint32(1), arr.ArrayLength())
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", ObjectValue(arr), str("length"), desc(map[string]Value{"value": NumberValue(1.5)})), "RangeError: Invalid array length")
	jsCall(t, r, ctor, "defineProperty", ObjectValue(arr), str("length"), desc(map[string]Value{"writable": False()}))
	assert.False(t, arr.Push(r, IntValue(1)))
	// Non-extensible targets reject new keys.
	sealed := r.NewObject()
	sealed.PreventExtensions(r)
	assert.EqualError(t, jsCallErr(t, r, ctor, "defineProperty", ObjectValue(sealed), str("n"), desc(map[string]Value{"value": IntValue(1)})), "TypeError: Cannot redefine property: n")
}

func TestObjectNamesFromEntriesHasOwn(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ObjectCtor)
	o := r.NewObject()
	mustSet(t, r, o, "b", IntValue(1))
	mustSet(t, r, o, "1", IntValue(1))
	o.DefineOwnDataFast(r, key(r, "hidden"), IntValue(0), attrHidden)
	o.DefineOwnDataFast(r, SymbolKey(SymIterator), IntValue(0), attrDefault)
	assert.Equal(t, []any{"1", "b", "hidden"}, r.ToGo(jsCall(t, r, ctor, "getOwnPropertyNames", ObjectValue(o))))
	assert.Equal(t, []any{"0", "1", "length"}, r.ToGo(jsCall(t, r, ctor, "getOwnPropertyNames", jsArray(r, IntValue(1), IntValue(2)))))
	assert.Equal(t, []any{"0", "1", "length"}, r.ToGo(jsCall(t, r, ctor, "getOwnPropertyNames", str("ab"))))
	assert.Equal(t, []any{"length", "name"}, r.ToGo(jsCall(t, r, ctor, "getOwnPropertyNames", jsGlobal(t, r, "parseInt"))))
	assert.Equal(t, []any{"length", "name", "prototype"}, r.ToGo(jsCall(t, r, ctor, "getOwnPropertyNames", ObjectValue(r.BooleanCtor))))
	assert.Equal(t, []any{"message", "stack"}, r.ToGo(jsCall(t, r, ctor, "getOwnPropertyNames", ObjectValue(r.NewError(KindError, "m")))))
	assert.EqualError(t, jsCallErr(t, r, ctor, "getOwnPropertyNames", Undefined()), "TypeError: Cannot convert undefined or null to object")

	// fromEntries over arrays of pairs.
	pairs := jsArray(r, jsArray(r, str("a"), IntValue(1)), jsArray(r, IntValue(2), str("two")), jsArray(r, str("a"), IntValue(3)))
	res := jsCall(t, r, ctor, "fromEntries", pairs)
	assert.Equal(t, map[string]any{"a": int64(3), "2": "two"}, r.ToGo(res))
	assert.Equal(t, []string{"2", "a"}, keyNames(res.AsObject().OwnPropertyKeys()))
	assert.Same(t, r.ObjectPrototype, res.AsObject().Proto())
	assert.Equal(t, map[string]any{}, r.ToGo(jsCall(t, r, ctor, "fromEntries", jsArray(r))))
	// Entries may be array-likes; missing slots read as undefined.
	entry := r.NewObject()
	mustSet(t, r, entry, "0", str("k"))
	res = jsCall(t, r, ctor, "fromEntries", jsArray(r, ObjectValue(entry)))
	assert.True(t, jsGet(t, r, res, "k").IsUndefined())
	assert.True(t, res.AsObject().HasOwnProperty(key(r, "k")))
	// Round trip with entries, including a key coerced through ToPropertyKey.
	src := r.NewObject()
	mustSet(t, r, src, "x", IntValue(1))
	mustSet(t, r, src, "y", str("z"))
	back := jsCall(t, r, ctor, "fromEntries", jsCall(t, r, ctor, "entries", ObjectValue(src)))
	assert.Equal(t, r.ToGo(ObjectValue(src)), r.ToGo(back))
	// Errors.
	assert.EqualError(t, jsCallErr(t, r, ctor, "fromEntries", jsArray(r, IntValue(1))), "TypeError: Iterator value 1 is not an entry object")
	assert.EqualError(t, jsCallErr(t, r, ctor, "fromEntries", ObjectValue(r.NewArrayLen(1))), "TypeError: Iterator value undefined is not an entry object")
	assert.EqualError(t, jsCallErr(t, r, ctor, "fromEntries", Undefined()), "TypeError: Object.fromEntries requires an iterable, got undefined")
	assert.EqualError(t, jsCallErr(t, r, ctor, "fromEntries", Null()), "TypeError: Object.fromEntries requires an iterable, got null")
	err := jsCallErr(t, r, ctor, "fromEntries", ObjectValue(r.NewObject()))
	assert.ErrorContains(t, err, "TypeError", "plain objects are not iterable")
	// A key whose ToPropertyKey throws propagates.
	badKey := r.NewObjectWithProto(nil)
	assert.ErrorContains(t, jsCallErr(t, r, ctor, "fromEntries", jsArray(r, jsArray(r, ObjectValue(badKey), IntValue(1)))), "Cannot convert object to primitive value")

	// hasOwn: ToObject first, then the key.
	assert.Equal(t, True(), jsCall(t, r, ctor, "hasOwn", ObjectValue(o), str("b")))
	assert.Equal(t, True(), jsCall(t, r, ctor, "hasOwn", ObjectValue(o), IntValue(1)))
	assert.Equal(t, True(), jsCall(t, r, ctor, "hasOwn", ObjectValue(o), str("hidden")))
	assert.Equal(t, False(), jsCall(t, r, ctor, "hasOwn", ObjectValue(o), str("toString")))
	assert.Equal(t, True(), jsCall(t, r, ctor, "hasOwn", str("ab"), IntValue(1)))
	assert.Equal(t, False(), jsCall(t, r, ctor, "hasOwn", str("ab"), IntValue(2)))
	assert.Equal(t, True(), jsCall(t, r, ctor, "hasOwn", str("ab"), str("length")))
	assert.Equal(t, False(), jsCall(t, r, ctor, "hasOwn", IntValue(1), str("toString")))
	assert.EqualError(t, jsCallErr(t, r, ctor, "hasOwn", Null(), ObjectValue(badKey)), "TypeError: Cannot convert undefined or null to object")
	assert.ErrorContains(t, jsCallErr(t, r, ctor, "hasOwn", ObjectValue(o), ObjectValue(badKey)), "Cannot convert object to primitive value")
}

func TestObjectPrototypeMethods(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	mustSet(t, r, o, "own", IntValue(1))
	mustSet(t, r, o, "3", IntValue(1))
	o.DefineOwnDataFast(r, key(r, "hidden"), IntValue(0), attrHidden)
	ov := ObjectValue(o)
	// hasOwnProperty: key coerced before this.
	assert.Equal(t, True(), jsCall(t, r, ov, "hasOwnProperty", str("own")))
	assert.Equal(t, True(), jsCall(t, r, ov, "hasOwnProperty", IntValue(3)))
	assert.Equal(t, True(), jsCall(t, r, ov, "hasOwnProperty", str("3")))
	assert.Equal(t, True(), jsCall(t, r, ov, "hasOwnProperty", str("hidden")))
	assert.Equal(t, False(), jsCall(t, r, ov, "hasOwnProperty", str("toString")))
	assert.Equal(t, False(), jsCall(t, r, ov, "hasOwnProperty"))
	mustSet(t, r, o, "undefined", IntValue(1))
	assert.Equal(t, True(), jsCall(t, r, ov, "hasOwnProperty"), "missing key is the string \"undefined\"")
	hop := jsGet(t, r, ObjectValue(r.ObjectPrototype), "hasOwnProperty")
	call := func(this Value, args ...Value) (Value, error) { return r.Call(hop, this, args) }
	v, _ := call(str("abc"), IntValue(1))
	assert.Equal(t, True(), v)
	v, _ = call(str("abc"), IntValue(3))
	assert.Equal(t, False(), v)
	v, _ = call(str("abc"), str("length"))
	assert.Equal(t, True(), v)
	v, _ = call(str("abc"), str("charAt"))
	assert.Equal(t, False(), v)
	v, _ = call(IntValue(1), str("toFixed"))
	assert.Equal(t, False(), v)
	v, _ = call(True(), str("valueOf"))
	assert.Equal(t, False(), v)
	_, err := call(Undefined(), str("x"))
	assert.EqualError(t, err, "TypeError: Cannot convert undefined or null to object")
	badKey := r.NewObjectWithProto(nil)
	_, err = call(Undefined(), ObjectValue(badKey))
	assert.ErrorContains(t, err, "Cannot convert object to primitive value", "the key is coerced first")
	v, _ = call(ObjectValue(r.NewError(KindError, "m")), str("stack"))
	assert.Equal(t, True(), v)
	v, _ = call(ObjectValue(r.ArrayPrototype), str("length"))
	assert.Equal(t, True(), v)

	// isPrototypeOf.
	child := r.NewObjectWithProto(o)
	grand := r.NewObjectWithProto(child)
	assert.Equal(t, True(), jsCall(t, r, ov, "isPrototypeOf", ObjectValue(grand)))
	assert.Equal(t, True(), jsCall(t, r, ObjectValue(r.ObjectPrototype), "isPrototypeOf", ObjectValue(grand)))
	assert.Equal(t, False(), jsCall(t, r, ObjectValue(grand), "isPrototypeOf", ov))
	assert.Equal(t, False(), jsCall(t, r, ov, "isPrototypeOf", ov))
	assert.Equal(t, False(), jsCall(t, r, ov, "isPrototypeOf", IntValue(1)))
	assert.Equal(t, False(), jsCall(t, r, ov, "isPrototypeOf"))
	ipo := jsGet(t, r, ObjectValue(r.ObjectPrototype), "isPrototypeOf")
	v, err = r.Call(ipo, Undefined(), []Value{IntValue(1)})
	require.NoError(t, err)
	assert.Equal(t, False(), v, "primitive argument short-circuits before ToObject(this)")
	_, err = r.Call(ipo, Undefined(), []Value{ov})
	assert.EqualError(t, err, "TypeError: Cannot convert undefined or null to object")
	assert.Equal(t, True(), jsCall(t, r, ObjectValue(r.FunctionPrototype), "isPrototypeOf", ObjectValue(r.ArrayCtor)))

	// propertyIsEnumerable.
	assert.Equal(t, True(), jsCall(t, r, ov, "propertyIsEnumerable", str("own")))
	assert.Equal(t, True(), jsCall(t, r, ov, "propertyIsEnumerable", IntValue(3)))
	assert.Equal(t, False(), jsCall(t, r, ov, "propertyIsEnumerable", str("hidden")))
	assert.Equal(t, False(), jsCall(t, r, ov, "propertyIsEnumerable", str("toString")))
	assert.Equal(t, False(), jsCall(t, r, ObjectValue(grand), "propertyIsEnumerable", str("own")), "inherited properties are not own")
	pie := jsGet(t, r, ObjectValue(r.ObjectPrototype), "propertyIsEnumerable")
	v, _ = r.Call(pie, str("ab"), []Value{IntValue(0)})
	assert.Equal(t, True(), v)
	v, _ = r.Call(pie, str("ab"), []Value{str("length")})
	assert.Equal(t, False(), v)
	v, _ = r.Call(pie, IntValue(1), []Value{str("x")})
	assert.Equal(t, False(), v)
	_, err = r.Call(pie, Null(), []Value{str("x")})
	assert.EqualError(t, err, "TypeError: Cannot convert undefined or null to object")
	arr := r.NewArray(IntValue(1))
	assert.Equal(t, False(), jsCall(t, r, ObjectValue(arr), "propertyIsEnumerable", str("length")))
	assert.Equal(t, True(), jsCall(t, r, ObjectValue(arr), "propertyIsEnumerable", IntValue(0)))

	// toString tags and toLocaleString delegation.
	ts := jsGet(t, r, ObjectValue(r.ObjectPrototype), "toString")
	for _, c := range []struct {
		this Value
		want string
	}{
		{Undefined(), "[object Undefined]"}, {Null(), "[object Null]"}, {IntValue(1), "[object Number]"}, {str("s"), "[object String]"},
		{True(), "[object Boolean]"}, {ObjectValue(arr), "[object Array]"}, {ObjectValue(r.ArrayPrototype), "[object Array]"},
		{ObjectValue(r.ObjectCtor), "[object Function]"}, {ObjectValue(r.NewError(KindTypeError, "x")), "[object Error]"},
		{ObjectValue(r.ErrorPrototype), "[object Object]"}, {ov, "[object Object]"}, {ObjectValue(r.Math), "[object Math]"},
		{ObjectValue(r.JSON), "[object JSON]"}, {ObjectValue(r.NumberPrototype), "[object Number]"}, {ObjectValue(r.NewObjectWithProto(nil)), "[object Object]"},
		{SymbolValue(SymIterator), "[object Symbol]"},
	} {
		res, err := r.Call(ts, c.this, nil)
		require.NoError(t, err)
		assert.Equal(t, c.want, jsString(t, res))
	}
	assert.Equal(t, "[object Object]", jsString(t, jsCall(t, r, ov, "toLocaleString")))
	o.DefineOwnDataFast(r, StringKey(AtomToString), nativeFn(r, func(this Value, _ []Value) (Value, error) {
		return str("custom:" + this.String()), nil
	}), attrHidden)
	assert.Equal(t, "custom:[object Object]", jsString(t, jsCall(t, r, ov, "toLocaleString")))
	assert.Equal(t, "1", jsString(t, jsCall(t, r, IntValue(1), "toLocaleString")))
	tls := jsGet(t, r, ObjectValue(r.ObjectPrototype), "toLocaleString")
	_, err = r.Call(tls, Undefined(), nil)
	assert.ErrorContains(t, err, "Cannot read property 'toString' of undefined")
	o.DefineOwnDataFast(r, key(r, "notfn"), IntValue(1), attrHidden)
	require.NoError(t, o.SetProp(r, StringKey(AtomToString), IntValue(1)))
	assert.EqualError(t, jsCallErr(t, r, ov, "toLocaleString"), "TypeError: 1 is not a function")
	// valueOf returns the object itself (wrapped for primitives).
	assert.Same(t, o, jsCall(t, r, ov, "valueOf").AsObject())
	vo := jsGet(t, r, ObjectValue(r.ObjectPrototype), "valueOf")
	w, _ := r.Call(vo, str("s"), nil)
	assert.Equal(t, ClassString, w.AsObject().Class())
}

// TestDefinePropertiesFromShape checks objectDefineProperties' path for
// descriptor objects in shape (descriptorsInShape) against the general
// path, which a proxy over the same descriptors takes: the properties, their
// attributes and their order, the strings before the symbols; and the
// cases it leaves to the general path, whose errors it must not preempt.
func TestDefinePropertiesFromShape(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		got := runScriptsIn(t, r, `
			var sym = Symbol("s"), P = {p: 1};
			var src = {};
			src[sym] = 0;
			src.a = 1;
			src.b = "x";
			Object.defineProperty(src, "g", {get() { return 1; }, set(v) {}, enumerable: true, configurable: true});
			Object.defineProperty(src, "s", {set(v) {}, enumerable: false, configurable: true});
			Object.defineProperty(src, "h", {value: 3, enumerable: false});
			Object.defineProperty(src, "ro", {value: 4, writable: false, enumerable: true, configurable: false});
			class K { #q = 1; k = 2; [sym] = 3; }
			var D = Object.getOwnPropertyDescriptors(src);
			var L = {
				l1: {value: 1, writable: true, enumerable: true, configurable: true},
				l2: {get: undefined, set: function() {}, enumerable: false, configurable: true},
				l3: {value: 2, writable: 0, enumerable: "", configurable: 1},
				l4: {configurable: true, value: 3},
				l5: {enumerable: true, get: function() { return 5; }},
			};
			var G = {}; Object.defineProperty(G, "value", {get() { return 6; }, enumerable: true});
			var V = {v: G}, P2 = {}; Object.create(P2);
			Object.defineProperty(L, "hidden", {value: {value: 9, writable: true, enumerable: true, configurable: true}, enumerable: false});
			function dump(o) {
				return Reflect.ownKeys(o).map(function(k) {
					var d = Object.getOwnPropertyDescriptor(o, k);
					return String(k) + ":" + ["value", "writable", "get", "set", "enumerable", "configurable"].map(function(f) {
						return f in d ? f + "=" + (typeof d[f] === "function" ? "fn" : String(d[f])) : "";
					}).join(",");
				}).join(" ");
			}
			var c1 = Object.create(P, D), c2 = Object.create(P, new Proxy(D, {}));
			var l1 = Object.defineProperties({}, L), l2 = Object.defineProperties({}, new Proxy(L, {}));
			globalThis.D = D; globalThis.L = L; globalThis.V = V; globalThis.P2 = P2;
			var p2 = Object.defineProperties(P2, L), v = Object.defineProperties({}, V);
			[dump(c1) === dump(c2), dump(l1) === dump(l2) && dump(p2) === dump(l1) && v.v === 6, dump(c1), dump(l1),
				Reflect.ownKeys(Object.getOwnPropertyDescriptors(new K())).map(String).join()].join("\n")`)
		assert.Equal(t, "true\ntrue\n"+
			"a:value=1,writable=true,,,enumerable=true,configurable=true "+
			"b:value=x,writable=true,,,enumerable=true,configurable=true "+
			"g:,,get=fn,set=fn,enumerable=true,configurable=true "+
			"s:,,get=undefined,set=fn,enumerable=false,configurable=true "+
			"h:value=3,writable=false,,,enumerable=false,configurable=false "+
			"ro:value=4,writable=false,,,enumerable=true,configurable=false "+
			"Symbol(s):value=0,writable=true,,,enumerable=true,configurable=true\n"+
			"l1:value=1,writable=true,,,enumerable=true,configurable=true "+
			"l2:,,get=undefined,set=fn,enumerable=false,configurable=true "+
			"l3:value=2,writable=false,,,enumerable=false,configurable=true "+
			"l4:value=3,writable=false,,,enumerable=false,configurable=true "+
			"l5:,,get=fn,set=undefined,enumerable=true,configurable=false\n"+
			"k,Symbol(s)", got, "shared %v", shared)
		D, L := jsGlobal(t, r, "D").AsObject(), jsGlobal(t, r, "L").AsObject()
		o := r.NewObject()
		assert.True(t, r.descriptorsInShape(o, D), "shared %v: getOwnPropertyDescriptors' result", shared)
		assert.True(t, r.descriptorsInShape(o, L), "shared %v: literals of the same keys", shared)
		assert.False(t, r.descriptorsInShape(D, D), "the target is the descriptors")
		assert.False(t, r.descriptorsInShape(jsGlobal(t, r, "P2").AsObject(), L), "the target is a prototype")
		assert.False(t, r.descriptorsInShape(o, jsGlobal(t, r, "V").AsObject()), "a field read by a getter")
		assert.False(t, r.descriptorsInShape(jsGet(t, r, ObjectValue(D), "a").AsObject(), D), "the target is a descriptor")

		// Left to the general path, which throws where the descriptors are read.
		got = runScriptsIn(t, r, `
			function err(f) { try { f(); return "ok"; } catch (e) { return e.constructor.name; } }
			var bad = {a: {get: 1, set: undefined, enumerable: true, configurable: true}};
			var acc = {}; Object.defineProperty(acc, "a", {get() { return {value: 1}; }, enumerable: true});
			var frozen = Object.freeze({});
			[err(function() { Object.defineProperties({}, bad); }),
				err(function() { Object.defineProperties(frozen, D); }),
				Object.defineProperties({}, acc).a].join()`)
		assert.Equal(t, "TypeError,TypeError,1", got)
		assert.False(t, r.descriptorsInShape(o, jsGlobal(t, r, "bad").AsObject()), "a getter that is not callable")
		assert.False(t, r.descriptorsInShape(o, jsGlobal(t, r, "acc").AsObject()), "an accessor on the descriptors")
		if shared {
			continue // the shared Object.prototype takes no property
		}
		got = runScriptsIn(t, r, `
			Object.prototype.get = function() {};
			var e = "ok";
			try { Object.defineProperties({}, D); } catch (x) { e = x.constructor.name; }
			delete Object.prototype.get;
			e`)
		assert.Equal(t, "TypeError", got, "an inherited get and a value")
	}
}

// TestDescriptorInPlace checks the descriptors toPropertyDescriptor reads
// in place (pureDescriptor) against what the general path reads: fields in
// any order and among other keys, fields inherited from Object.prototype
// before, while and after it has them (lacksDescFields keeps its answer
// for a prototype epoch) or from another prototype, a field read by a
// getter, and the descriptors the general path rejects. Node.js 24 gives
// the same.
func TestDescriptorInPlace(t *testing.T) {
	const prelude = `
		function dump(o) {
			return Reflect.ownKeys(o).map(function(k) {
				var d = Object.getOwnPropertyDescriptor(o, k);
				return String(k) + ":" + ["value", "writable", "get", "set", "enumerable", "configurable"].map(function(f) {
					return f in d ? (typeof d[f] === "function" ? "fn" : String(d[f])) : "-";
				}).join(",");
			}).join(" ");
		}
		function err(f) { try { f(); return "ok"; } catch (e) { return e.constructor.name; } }
		var o = {};
		Object.defineProperty(o, "a", {value: 1});
		Object.defineProperty(o, "b", {enumerable: true, get: function() { return 2; }});
		Object.defineProperty(o, "c", {configurable: true, writable: true, value: 3, enumerable: 1});
		Object.defineProperty(o, "d", {other: 0, value: 4, more: 1});
		Object.defineProperty(o, "e", Object.assign(Object.create({writable: true, x: 0}), {value: 5}));
		var acc = {}; Object.defineProperty(acc, "value", {get() { return 6; }, enumerable: true});
		Object.defineProperty(o, "f", acc);
		var errs = [{get: 1}, {get() {}, value: 1}, {set: null}, {writable: true, set() {}}].map(function(d) {
			return err(function() { Object.defineProperty({}, "x", d); });
		}).join();
	`
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		got := runScriptsIn(t, r, prelude+`[dump(o), errs].join("|")`)
		assert.Equal(t, "a:1,false,-,-,false,false b:-,-,fn,undefined,true,false c:3,true,-,-,true,true "+
			"d:4,false,-,-,false,false e:5,true,-,-,false,false f:6,false,-,-,false,false|"+
			"TypeError,TypeError,TypeError,TypeError", got, "shared %v", shared)
		if shared {
			continue // the shared Object.prototype takes no property
		}
		got = runScriptsIn(t, r, `
			var p = {};
			Object.defineProperty(p, "x", {value: 1});
			Object.prototype.enumerable = true;
			Object.defineProperty(p, "y", {value: 2});
			var c1 = Object.create(null, {z: {value: 3}});
			Object.prototype.get = function() { return 4; };
			var e1 = err(function() { Object.defineProperty({}, "v", {value: 1}); });
			var g = Object.defineProperties({}, {w: {configurable: true}});
			delete Object.prototype.get;
			delete Object.prototype.enumerable;
			Object.defineProperty(p, "u", {value: 5});
			var c2 = Object.create(null, {z: {value: 3}});
			[dump(p), dump(c1), e1, dump(g), dump(c2)].join("|")`)
		assert.Equal(t, "x:1,false,-,-,false,false y:2,false,-,-,true,false u:5,false,-,-,false,false|"+
			"z:3,false,-,-,true,false|TypeError|w:-,-,fn,undefined,true,true|z:3,false,-,-,false,false", got)
	}
}

// TestEntriesStorage checks Object.entries and Object.values, whose result
// and entries take their elements in their own allocation where
// newArrayStorage co-allocates: each entry is an array of its own, and
// every result grows as any array does.
func TestEntriesStorage(t *testing.T) {
	got := runScripts(t, `
		var e = Object.entries({a: 1, b: 2, c: 3});
		e[0].push(9); e[1][0] = "z"; e.push(["d", 4]); e[2].length = 1;
		var o9 = {}; for (var i = 0; i < 9; i++) o9["k" + i] = i;
		var v = Object.values({x: 1}); v.push(2, 3);
		var g = {get a() { delete this.b; return 1; }, b: 2, c: 3};
		[JSON.stringify(e), JSON.stringify(Object.entries({})), JSON.stringify(v),
			JSON.stringify(Object.entries(o9).slice(7)), JSON.stringify(Object.entries(g)),
			Object.entries({a: 1})[0] !== Object.entries({a: 1})[0]].join("|")`)
	assert.Equal(t, `[["a",1,9],["z",2],["c"],["d",4]]|[]|[1,2,3]|[["k7",7],["k8",8]]|[["a",1],["c",3]]|true`, got)
}

// descFieldsJS prints a property's descriptor: its value, writable, get,
// set, enumerable and configurable, "-" for a field it lacks.
const descFieldsJS = `
	function dd(o, k) {
		var x = Object.getOwnPropertyDescriptor(o, k);
		return ["value", "writable", "get", "set", "enumerable", "configurable"].map(function(f) {
			return f in x ? (typeof x[f] === "function" ? "fn" : String(x[f])) : "-";
		}).join(",");
	}
	function err(f) { try { f(); return "ok"; } catch (e) { return e.constructor.name; } }
`

// TestDescFieldsOnObjectPrototype gives Object.prototype descriptor fields
// after lacksDescFields found it has none, by each path that adds a
// property: a set, a define, an accessor, an add in dictionary mode, a
// host's lazy property and an add past maxShapeProps to an object the host
// keeps in shape mode (KeepShape). Each must reach the descriptors read
// next, and an inline cache that found a key absent must miss once a lazy
// property defines it. Node.js 24 gives the same, a lazy property being a
// writable, configurable, non-enumerable data property.
func TestDescFieldsOnObjectPrototype(t *testing.T) {
	r := NewRealm()
	got := runScriptsIn(t, r, descFieldsJS+`
		var o = {}, out = [];
		Object.defineProperty(o, "a", {writable: true});
		out.push(dd(o, "a"));
		Object.prototype.value = 7;
		Object.defineProperty(o, "b", {writable: true});
		out.push(dd(o, "b"));
		delete Object.prototype.value;
		Object.defineProperty(o, "c", {writable: true});
		out.push(dd(o, "c"));
		Object.defineProperty(Object.prototype, "get", {value: function() { return 8; }, writable: true, configurable: true});
		out.push(err(function() { Object.defineProperty(o, "d", {value: 1}); }));
		Object.defineProperty(o, "e", {enumerable: true});
		out.push(dd(o, "e") + "=" + o.e);
		var c1 = Object.create(null, {x: {configurable: true}});
		out.push(dd(c1, "x"));
		delete Object.prototype.get;
		Object.defineProperty(Object.prototype, "enumerable", {get: function() { return true; }, configurable: true});
		var g = Object.defineProperties({}, {y: {value: 2}});
		out.push(dd(g, "y"));
		delete Object.prototype.enumerable;
		Object.defineProperty(o, "f", {value: 3});
		out.push(dd(o, "f"));
		delete Object.prototype.toLocaleString;
		Object.defineProperty(o, "h", {value: 4});
		out.push(dd(o, "h"));
		Object.prototype.value = 9;
		Object.defineProperty(o, "i", {writable: true});
		out.push(dd(o, "i"));
		delete Object.prototype.value;
		Object.defineProperty(o, "j", {writable: true});
		out.push(dd(o, "j"));
		function rd(x) { return x.zz; }
		out.push(String(rd({})));
		out.join(" ")`)
	assert.Equal(t, "undefined,true,-,-,false,false 7,true,-,-,false,false undefined,true,-,-,false,false "+
		"TypeError -,-,fn,undefined,true,false=8 -,-,fn,undefined,false,true 2,false,-,-,true,false "+
		"3,false,-,-,false,false 4,false,-,-,false,false 9,true,-,-,false,false undefined,true,-,-,false,false "+
		"undefined", got)
	require.NotZero(t, r.ObjectPrototype.flags&flagDict, "the delete left Object.prototype in dictionary mode")
	r.ObjectPrototype.DefineLazyProperty(r, r.KeyFromGoString("value"), func(*Realm) Value { return IntValue(10) })
	r.ObjectPrototype.DefineLazyProperty(r, r.KeyFromGoString("zz"), func(*Realm) Value { return IntValue(11) })
	got = runScriptsIn(t, r, `
		Object.defineProperty(o, "k", {writable: true});
		[dd(o, "k"), rd({})].join(" ")`)
	assert.Equal(t, "10,true,-,-,false,false 11", got, "lazy properties of Object.prototype in dictionary mode")

	// The same in shape mode, where rd's inline cache found zz absent.
	r = NewRealm()
	got = runScriptsIn(t, r, descFieldsJS+`
		var o = {};
		function rd(x) { return x.zz; }
		Object.defineProperty(o, "a", {writable: true});
		[dd(o, "a"), rd({}), rd({})].join()`)
	assert.Equal(t, "undefined,true,-,-,false,false,,", got)
	require.Zero(t, r.ObjectPrototype.flags&flagDict)
	r.ObjectPrototype.DefineLazyProperty(r, r.KeyFromGoString("value"), func(*Realm) Value { return IntValue(10) })
	r.ObjectPrototype.DefineLazyProperty(r, r.KeyFromGoString("zz"), func(*Realm) Value { return IntValue(11) })
	got = runScriptsIn(t, r, `
		Object.defineProperty(o, "k", {writable: true});
		[dd(o, "k"), rd({})].join(" ")`)
	assert.Equal(t, "10,true,-,-,false,false 11", got, "lazy properties of Object.prototype in shape mode")

	r = NewRealm()
	r.ObjectPrototype.KeepShape()
	got = runScriptsIn(t, r, descFieldsJS+`
		for (var i = 0; i < `+fmt.Sprint(maxShapeProps)+`; i++) Object.prototype["p" + i] = i;
		var o = {}, out = [];
		Object.defineProperty(o, "a", {writable: true});
		out.push(dd(o, "a"));
		Object.prototype.value = 5;
		Object.defineProperty(o, "b", {writable: true});
		out.push(dd(o, "b"));
		out.join(" ")`)
	assert.Equal(t, "undefined,true,-,-,false,false 5,true,-,-,false,false", got)
	assert.True(t, r.ObjectPrototype.flags&flagDict == 0 && r.ObjectPrototype.shape.owned, "an owned chain")
}

// TestShapeKeysInPlace checks the built-ins that read an object's keys
// from its shape in place, rather than from a list of them: a getter that
// deletes a later key or adds one changes the object's shape, and the keys
// stay those of the start ([[OwnPropertyKeys]] before the first read), the
// strings before the symbols.
func TestShapeKeysInPlace(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		got := runScriptsIn(t, r, `
			var sym = Symbol("s");
			function mk() {
				var o = {};
				o[sym] = 5;
				o.x = 1;
				Object.defineProperty(o, "y", {get() { delete this.z; this.w = 4; return 2; }, enumerable: true, configurable: true});
				o.z = 3;
				return o;
			}
			function keys(o) { return Reflect.ownKeys(o).map(function(k) { return String(k) + "=" + String(o[k]); }).join(); }
			[keys({a: 0, ...mk()}),
				JSON.stringify(Object.entries(mk())),
				JSON.stringify(Object.values(mk())),
				Reflect.ownKeys(Object.getOwnPropertyDescriptors(mk())).map(String).join()].join("\n")`)
		assert.Equal(t, "a=0,x=1,y=2,Symbol(s)=5\n"+
			`[["x",1],["y",2]]`+"\n"+
			"[1,2]\n"+
			"x,y,z,Symbol(s)", got, "shared %v", shared)
	}
}

// TestSpreadTrunkRoom checks that an object a spread fills gets room for
// the properties the objects that took its shape went on to get, as a
// literal's own after the spread, and no more where none did.
func TestSpreadTrunkRoom(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		runScriptsIn(t, r, `
			var o = {}, p = {};
			for (var i = 0; i < 20; i++) { o["sp" + i] = i; p["sq" + i] = i; }
			function more(o) { return {...o, spExtra: 1}; }
			more(o);
			globalThis.m = more(o); globalThis.c = {...p};`)
		m, c := jsGlobal(t, r, "m").AsObject(), jsGlobal(t, r, "c").AsObject()
		assert.Equal(t, 21, len(m.slots))
		assert.Equal(t, cap(slices.Grow([]Value(nil), 21)), cap(m.slots), "shared %v: room for the field after the spread", shared)
		assert.Equal(t, cap(slices.Grow([]Value(nil), 20)), cap(c.slots), "shared %v: no object went on from p's shape", shared)
	}
}

// TestSpreadRoom checks that a spread into a literal with keys gives it
// room for the keys it copies at once, where append would double: a few
// keys into a literal of one ({ref, ...props}), and a few more into one of
// many ({...defaults, ...options}), and for a key the literal adds after
// the spread where an object with the same keys took it before. The keys
// the target has already take no room, and the spread's own result is that
// of the general path.
func TestSpreadRoom(t *testing.T) {
	r := NewRealm()
	runScriptsIn(t, r, `
		var src = {}, opts = {};
		for (var i = 0; i < 12; i++) src["k" + i] = i;
		for (var i = 0; i < 38; i++) opts["o" + i] = i;
		globalThis.a = {ref: 1, ...src};
		globalThis.b = {...opts, ...{o3: -1, n1: 1, n2: 2, n3: 3, n4: 4}};
		globalThis.c = {o0: 0, ...{o0: 1, o1: 2}};
		globalThis.ka = Object.keys(a).join(); globalThis.kb = JSON.stringify(b);
		function el(p) { return {a: 1, b: 2, ...p, children: 3}; }
		var px = {x1: 1, x2: 2, x3: 3, x4: 4, x5: 5}, py = {x1: 1, x2: 2, x3: 3, x4: 4, y5: 5};
		el(px); el(py); globalThis.d = el(px);`)
	a, b, c := jsGlobal(t, r, "a").AsObject(), jsGlobal(t, r, "b").AsObject(), jsGlobal(t, r, "c").AsObject()
	d := jsGlobal(t, r, "d").AsObject()
	assert.Equal(t, 8, len(d.slots))
	assert.Equal(t, 8, cap(d.slots), "room for the key the literal adds after the spread, where an object took that path before")
	assert.Equal(t, "ref,k0,k1,k2,k3,k4,k5,k6,k7,k8,k9,k10,k11", jsGlobal(t, r, "ka").String())
	assert.Equal(t, 13, len(a.slots))
	assert.Less(t, cap(a.slots), 16, "room for the 12 keys, not doubling")
	assert.Equal(t, 42, len(b.slots))
	assert.Less(t, cap(b.slots), 48, "room for the 4 new keys, not doubling")
	assert.Contains(t, jsGlobal(t, r, "kb").String(), `"o3":-1,`)
	assert.Equal(t, 2, len(c.slots))
	// One key new to a literal of many, after one it has, where the
	// objects before went on from the first source's shape to different
	// keys: room for that key once an object took it before, as append
	// doubles for the first. The second source is read between the
	// spreads (o.p), so the first does not take room for it
	// (spreadPairRoom).
	runScriptsIn(t, r, `
		var opts2 = {};
		for (var i = 0; i < 39; i++) opts2["q" + i] = i;
		function fx(o) { return {...opts2, ...o.p}; }
		globalThis.f1 = fx({p: {q1: -1, kp: 1}}); fx({p: {q1: -1, kq: 1}}); globalThis.f2 = fx({p: {q1: -1, kp: 1}});`)
	f1, f2 := jsGlobal(t, r, "f1").AsObject(), jsGlobal(t, r, "f2").AsObject()
	assert.Equal(t, 40, len(f2.slots))
	assert.Equal(t, cap(slices.Grow([]Value(nil), 40)), cap(f2.slots), "room for the one key")
	assert.Equal(t, cap(slices.Grow(make([]Value, 39), 1)), cap(f1.slots), "the first doubles")
}

// TestSpreadPairRoom checks that a literal of two spreads, the second
// right after the first (i18next's {...this.options, ...o}), takes room at
// once for the keys of both, the second in a register or a closure's
// variable, where the objects before went on from the
// first source's shape to many different keys: the object and its slots
// are as many allocations as a spread of one source with all the keys,
// where the first spread gave the room of its source and one more, and
// the second grew it.
func TestSpreadPairRoom(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		runScriptsIn(t, r, `
			var opts = {}, all = {}, os = [];
			for (var i = 0; i < 38; i++) opts["q" + i] = all["q" + i] = i;
			all.a0 = all.b0 = all.c0 = 1;
			for (var j = 0; j < 6; j++) os.push({q1: -1, ["a" + j]: 1, ["b" + j]: 2, ["c" + j]: 3});
			os[0].self = os[0];
			function pair(o) { return {...opts, ...o}; }
			function one() { return {...all}; }
			function later(o) { return {...opts, ...o.self}; }
			function capt(o) { let c = {...opts, ...o}; globalThis.keep = () => o; return c; }
			function captOne(o) { let c = {...all}; globalThis.keep = () => o; return c; }
			for (var j = 1; j < 6; j++) pair(os[j]);
			globalThis.p = pair(os[0]); globalThis.pk = [p.q1, p.a0, p.c0, p.q37].join();`)
		p := jsGlobal(t, r, "p").AsObject()
		assert.Equal(t, 42, len(p.slots))
		assert.Equal(t, cap(slices.Grow([]Value(nil), 42)), cap(p.slots), "shared %v: room for both sources' keys", shared)
		assert.Equal(t, "-1,1,3,37", jsGlobal(t, r, "pk").String())
		arg := []Value{jsGlobal(t, r, "os").AsObject().elems()[0]}
		// The room is on the slots in both builds, the second source a
		// closure's variable too.
		c, err := r.Call(jsGlobal(t, r, "capt"), Undefined(), arg)
		require.NoError(t, err)
		assert.Equal(t, 42, len(c.AsObject().slots))
		assert.Equal(t, cap(slices.Grow([]Value(nil), 42)), cap(c.AsObject().slots), "shared %v: room for a closure's variable's keys", shared)
		if regcheckBuild {
			continue // the checked build's run allocates its register checks, more for more instructions
		}
		allocs := func(name string) float64 {
			fn := jsGlobal(t, r, name)
			return testing.AllocsPerRun(50, func() {
				if _, err := r.Call(fn, Undefined(), arg); err != nil {
					t.Fatal(err)
				}
			})
		}
		assert.Equal(t, allocs("one"), allocs("pair"), "shared %v: a pair of spreads allocates as one", shared)
		assert.Equal(t, allocs("captOne"), allocs("capt"), "shared %v: also where the second source is a closure's variable", shared)
		if !shared {
			// Past shapeSmallTransitions children, the first spread gives
			// the room of its source and one more (Shape.room); the shared
			// tree keeps a list of the children, so it gives the most of
			// their trunks.
			assert.Greater(t, allocs("later"), allocs("pair"), "a second source read between the spreads grows the slots")
		}
	}
}

// TestKeyListHoles checks that the list of an object's keys takes room for
// the elements it holds, not for the holes between integer keys far apart
// (a jump table of micromark's), and an array's for all its elements.
func TestKeyListHoles(t *testing.T) {
	r := NewRealm()
	runScriptsIn(t, r, `
		var j = {a: 1}; j[3] = 1; j[400] = 2;
		var t = Object.assign({}, j);
		globalThis.j = j; globalThis.ks = Object.keys(t).join(); globalThis.arr = [1, 2, 3];`)
	assert.Equal(t, "3,400,a", jsGlobal(t, r, "ks").String())
	j := jsGlobal(t, r, "j").AsObject()
	assert.Equal(t, 401, j.elemLen(), "dense, with holes")
	assert.Equal(t, []PropertyKey{IndexKey(3), IndexKey(400), StringKey(r.InternGoString("a"))}, j.OwnPropertyKeys())
	assert.Less(t, cap(j.OwnPropertyKeys()), 8)
	assert.Less(t, cap(j.OwnEnumerableStringKeys()), 8)
	assert.GreaterOrEqual(t, cap(jsGlobal(t, r, "arr").AsObject().OwnPropertyKeys()), 4)
}

// TestDictObjectKeys checks Object.keys of objects in dictionary mode
// against the general path's (through a proxy, which lists the keys and
// reads each descriptor): integer keys first, dense and sparse, then the
// strings in creation order, without symbols, hidden or deleted keys, of
// an object frozen or not.
func TestDictObjectKeys(t *testing.T) {
	r := NewRealm()
	got := runScriptsIn(t, r, `
		function build(frozen) {
			var o = {};
			for (var i = 0; i < 80; i++) o["k" + i] = i;
			o[3] = 1; o[0] = 2; o[100000] = 3; o["419"] = 4;
			o[Symbol("s")] = 5;
			Object.defineProperty(o, "hidden", {value: 6, enumerable: false});
			Object.defineProperty(o, 7, {value: 7, enumerable: false});
			delete o.k5; o.k5 = 8; delete o.k10;
			if (frozen) Object.freeze(o);
			return o;
		}
		var out = [];
		for (var f of [false, true]) {
			var o = build(f), a = Object.keys(o), b = Object.keys(new Proxy(o, {}));
			out.push(a.length, a.slice(0, 8).join(), a[a.length - 1], a.join() === b.join());
		}
		out.join("|")`)
	assert.Equal(t, "83|0,3,419,100000,k0,k1,k2,k3|k5|true|83|0,3,419,100000,k0,k1,k2,k3|k5|true", got)
	o := jsGlobal(t, r, "o").AsObject()
	assert.NotZero(t, o.flags&flagDict, "dictionary mode")
	assert.NotNil(t, o.dictTable().sparse, "with a sparse index")
}

// pendingDescsJS reads the objects Object.getOwnPropertyDescriptors makes
// for ordinary objects by every path that makes their descriptor objects
// (pendingDescs.resolve) and by those that define from them without:
// Object.create and Object.defineProperties into an ordinary object, with
// Object.prototype gaining get, set or value after the descriptors were
// taken; into a proxy, a function and itself, a definition that fails
// midway, a source with a symbol before its strings, an accessor or one
// property, and one used as a prototype.
const pendingDescsJS = `
var s = Symbol("s"), out = [];
function keys(o) { return Reflect.ownKeys(o).map(String).join(","); }
function show(o) {
	return Reflect.ownKeys(o).map(function (k) {
		var d = Object.getOwnPropertyDescriptor(o, k);
		return String(k) + "=" + ("value" in d ? String(d.value) : "acc") + ":" + (d.writable ? "w" : "") + (d.enumerable ? "e" : "") + (d.configurable ? "c" : "");
	}).join(" ");
}
function err(f) { try { f(); return "ok"; } catch (e) { return e.constructor.name; } }
var src = {a: 1, b: "x", [s]: 3, c: 4};
Object.defineProperty(src, "h", {value: 5, enumerable: false});
var t = Object.getOwnPropertyDescriptors(src);
src.a = 9; delete src.b; src.n = 0;
var c = Object.create(Object.getPrototypeOf(src), t);
out.push(show(c));
out.push([t.__original, "__original" in t, t.toString === Object.prototype.toString, Object.prototype.hasOwnProperty.call(t, "zz")].join());
out.push(keys(t) + " " + JSON.stringify(t.a) + " " + (t.a === t.a) + " " + show(Object.create(null, t)));
var t2 = Object.getOwnPropertyDescriptors({p: 1, q: 2});
out.push(t2.q.value + " " + keys(t2) + " " + JSON.stringify(t2));
var t3 = Object.getOwnPropertyDescriptors({p: 1, q: 2}); t3.z = 0; out.push(keys(t3));
var t4 = Object.getOwnPropertyDescriptors({p: 1, q: 2}); delete t4.p; out.push(keys(t4));
var t5 = Object.getOwnPropertyDescriptors({p: 1, q: 2});
out.push(Object.keys(t5) + " " + Object.values(t5).map(function (d) { return d.value; }) + " " + JSON.stringify({...Object.getOwnPropertyDescriptors({p: 1, q: 2})}));
var t6 = Object.getOwnPropertyDescriptors({p: 1, q: 2}), fi = []; for (var k in t6) fi.push(k); out.push(fi.join());
var t7 = Object.getOwnPropertyDescriptors({p: 1, q: 2}); Object.freeze(t7); out.push(Object.isFrozen(t7) + " " + show(t7));
var t8 = Object.getOwnPropertyDescriptors({p: 1, q: 2}); Object.setPrototypeOf(t8, null);
out.push(keys(t8) + " " + Object.assign({}, Object.getOwnPropertyDescriptors({p: 1, q: 2})).q.value);
var t9 = Object.getOwnPropertyDescriptors({[s]: 1, a: 2, b: 3}); out.push(keys(t9) + " " + t9[s].value + t9.a.value + t9.b.value);
out.push(show(Object.defineProperties({}, Object.getOwnPropertyDescriptors(Object.freeze({f: 1, g: 2})))));
var t10 = Object.getOwnPropertyDescriptors({p: 1, q: 2}), ch = Object.create(t10);
function rz(o) { return o.z; }
function rp(o) { return o.p; }
var r1 = [rz(ch), rz(ch)]; t10.z = 5; r1.push(rz(ch), rp(ch).value, rp(ch).value);
out.push(r1.join());
var t11 = Object.getOwnPropertyDescriptors({p: 1, q: 2});
Object.prototype.get = function () {};
var e11 = err(function () { Object.create(null, t11); });
delete Object.prototype.get;
var t12 = Object.getOwnPropertyDescriptors({p: 1, q: 2});
Object.defineProperty(Object.prototype, "set", {get: function () { return undefined; }, configurable: true});
var e12 = err(function () { Object.defineProperties({}, t12); });
delete Object.prototype.set;
var t13 = Object.getOwnPropertyDescriptors({p: 1, q: 2});
Object.prototype.value = 7;
var c13 = Object.create(null, t13);
delete Object.prototype.value;
out.push(e11 + " " + e12 + " " + show(c13) + " " + show(Object.create(null, t11)));
var log = [], px = new Proxy({}, {defineProperty: function (tg, k, d) { log.push(k + ":" + JSON.stringify(d)); return Reflect.defineProperty(tg, k, d); }});
Object.defineProperties(px, Object.getOwnPropertyDescriptors({p: 1, q: 2}));
out.push(log.join(" "));
var tgt = {}; Object.defineProperty(tgt, "q", {value: 0});
var e15 = err(function () { Object.defineProperties(tgt, Object.getOwnPropertyDescriptors({p: 1, q: 2, r: 3})); });
out.push(e15 + " " + show(tgt));
var t16 = Object.getOwnPropertyDescriptors({p: 1, q: 2}); Object.defineProperties(t16, t16); out.push(show(t16));
var c17 = Object.create(null, Object.getOwnPropertyDescriptors({get g() { return 1; }, v: 2}));
var fn = function () {}; Object.defineProperties(fn, Object.getOwnPropertyDescriptors({p: 1, q: 2}));
out.push(show(c17) + " " + show(Object.create(null, Object.getOwnPropertyDescriptors({one: 1}))) + " " + (fn.p + fn.q));
var t18 = Object.getOwnPropertyDescriptors({p: 1, q: 2}), t19 = Object.getOwnPropertyDescriptors({p: 1, q: 2}), t20 = Object.getOwnPropertyDescriptors({p: 1, q: 2});
out.push([Object.prototype.hasOwnProperty.call(t18, "p"), Object.hasOwn(t19, "q"), "p" in t20, Object.getOwnPropertyDescriptor(Object.getOwnPropertyDescriptors({p: 1, q: 2}), "q").value.value].join());
out.join("\n");`

// TestPendingDescriptors runs pendingDescsJS in a mutable realm, where the
// descriptors are pending (a shared realm makes them at once, and its
// Object.prototype takes no field). Node.js 24 gives the same.
func TestPendingDescriptors(t *testing.T) {
	want := strings.Join([]string{
		"a=1:wec b=x:wec c=4:wec h=5: Symbol(s)=3:wec",
		",false,true,false",
		"a,b,c,h,Symbol(s) {\"value\":1,\"writable\":true,\"enumerable\":true,\"configurable\":true} true a=1:wec b=x:wec c=4:wec h=5: Symbol(s)=3:wec",
		"2 p,q {\"p\":{\"value\":1,\"writable\":true,\"enumerable\":true,\"configurable\":true},\"q\":{\"value\":2,\"writable\":true,\"enumerable\":true,\"configurable\":true}}",
		"p,q,z",
		"q",
		"p,q 1,2 {\"p\":{\"value\":1,\"writable\":true,\"enumerable\":true,\"configurable\":true},\"q\":{\"value\":2,\"writable\":true,\"enumerable\":true,\"configurable\":true}}",
		"p,q",
		"true p=[object Object]:e q=[object Object]:e",
		"p,q 2",
		"a,b,Symbol(s) 123",
		"f=1:e g=2:e",
		",,5,1,1",
		"TypeError TypeError p=1:wec q=2:wec p=1:wec q=2:wec",
		"p:{\"value\":1,\"writable\":true,\"enumerable\":true,\"configurable\":true} q:{\"value\":2,\"writable\":true,\"enumerable\":true,\"configurable\":true}",
		"TypeError q=0: p=1:wec",
		"p=1:wec q=2:wec",
		"g=acc:ec v=2:wec one=1:wec 3",
		"true,true,true,2",
	}, "\n")
	r := NewRealm()
	assert.Equal(t, want, runScriptsIn(t, r, pendingDescsJS))
	v := runScriptsIn(t, r, `var pt = Object.getOwnPropertyDescriptors({p: 1, q: 2}); Object.create(null, pt); Object.defineProperties({}, pt); pt.__original; "x"`)
	assert.Equal(t, "x", v)
	pt := jsGlobal(t, r, "pt").AsObject()
	_, ok := extDataOk(pt, kindPendingDescs)
	assert.True(t, ok, "read without being made")
	assert.Equal(t, "p,q", runScriptsIn(t, r, `Object.keys(pt).join()`))
	_, ok = extDataOk(pt, kindPendingDescs)
	assert.False(t, ok, "made by its key list")
	assert.Same(t, r.plainRoot, pt.shape.parent.parent, "in the plain objects' tree")
}

// TestPendingDescriptorsAdd: a private field (a base constructor's
// return override stamps it on any object) and a host's add of a property
// known to be absent make a pending result's descriptor objects first, so
// that a site reading a key the result holds does not cache what its
// prototype has for it.
func TestPendingDescriptorsAdd(t *testing.T) {
	r := NewRealm()
	assert.Equal(t, "function,object,object", runScriptsIn(t, r, `class B { constructor(o) { return o; } }
class S extends B { #x = 1; static has(o) { return #x in o; } }
function rd(x) { return typeof x.toString; }
var a = Object.getOwnPropertyDescriptors({p: 1, q: 2}), b = Object.getOwnPropertyDescriptors({toString: 1, q: 2});
new S(a); new S(b);
[rd(a), rd(b), typeof b.toString].join()`))
	runScriptsIn(t, r, `var c = Object.getOwnPropertyDescriptors({p: 1, q: 2}), d = Object.getOwnPropertyDescriptors({toString: 1, q: 2});
function rd2(x) { return typeof x.toString; }`)
	for _, name := range []string{"c", "d"} {
		o := jsGlobal(t, r, name).AsObject()
		_, ok := extDataOk(o, kindPendingDescs)
		require.True(t, ok, "%s: pending", name)
		o.DefineOwnDataFast(r, InternKey("z"), IntValue(1), attrDefault)
	}
	assert.Equal(t, "function,object,object,toString,q,z,true", runScriptsIn(t, r,
		`[rd2(c), rd2(d), typeof d.toString, Object.keys(d).join(), S.has(a)].join()`))
}

// TestPendingDescriptorsChain: a pending result of
// Object.getOwnPropertyDescriptors keeps what its descriptors will hold,
// not the source's prototype chain: the results for 64 instances of
// classes whose method closes over a string of 2 MiB fit a limit of 64
// MiB, where results that kept the source's shape, and with it each
// class, stopped with a RangeError after 27.
func TestPendingDescriptorsChain(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 64 << 20})
	assert.Equal(t, "64,63,true", runScriptsIn(t, r, `var res = [];
for (let i = 0; i < 64; i++) {
  const big = "x".repeat(2 << 20) + i;
  class C { constructor() { this.a = i; this.b = 2; } m() { return big; } }
  res.push(Object.getOwnPropertyDescriptors(new C()));
}
var plain = Object.getOwnPropertyDescriptors({a: 1, b: 2}), nul = Object.getOwnPropertyDescriptors(Object.create(null, {a: {value: 1}, b: {value: 2}}));
res.length + "," + res[63].a.value + "," + (res[0].b.value === 2)`))
	_, ok := extDataOk(jsGlobal(t, r, "res").AsObject().elems()[5].AsObject(), kindPendingDescs)
	assert.True(t, ok, "a class instance's: pending")
	for _, name := range []string{"plain", "nul"} {
		_, ok = extDataOk(jsGlobal(t, r, name).AsObject(), kindPendingDescs)
		assert.True(t, ok, "%s: pending", name)
	}
}

package engine

import (
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkArray builds an array from Go values (nil becomes null, not a hole). It
// converts eagerly, so the result is an ordinary array that exports fresh
// values rather than a host node that exports its Go slice.
func mkArray(t *testing.T, r *Realm, items ...any) Value {
	t.Helper()
	v, err := eagerFromGo(r, append([]any{}, items...))
	require.NoError(t, err)
	return v
}

// holeArray builds an array of length n with values at the given indices.
func holeArray(r *Realm, n uint32, set map[uint32]Value) Value {
	a := r.NewArrayLen(n)
	for i, v := range set {
		a.DefineOwnDataFast(r, IndexKey(i), v, attrDefault)
	}
	return ObjectValue(a)
}

// arrayLike builds a plain object with a length and the given indices.
func arrayLike(t *testing.T, r *Realm, length int, set map[string]Value) *Object {
	t.Helper()
	o := r.NewObject()
	mustSet(t, r, o, "length", IntValue(length))
	for k, v := range set {
		mustSet(t, r, o, k, v)
	}
	return o
}

// hasIdx reports whether v (an object) has an own property at index i.
func hasIdx(v Value, i uint32) bool { return v.AsObject().HasOwnProperty(IndexKey(i)) }

func TestArrayBuiltinShape(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ArrayCtor)
	assertNativeShape(t, r, ctor, "Array", 1)
	for _, c := range []struct {
		name   string
		length int
	}{{"from", 1}, {"isArray", 1}, {"of", 0}} {
		assertNativeShape(t, r, jsGet(t, r, ctor, c.name), c.name, c.length)
	}
	proto := ObjectValue(r.ArrayPrototype)
	for _, c := range []struct {
		name   string
		length int
	}{
		{"at", 1}, {"concat", 1}, {"copyWithin", 2}, {"entries", 0}, {"every", 1}, {"fill", 1}, {"filter", 1}, {"find", 1}, {"findIndex", 1},
		{"findLast", 1}, {"findLastIndex", 1}, {"flat", 0}, {"flatMap", 1}, {"forEach", 1}, {"includes", 1}, {"indexOf", 1},
		{"join", 1}, {"keys", 0}, {"lastIndexOf", 1}, {"map", 1}, {"pop", 0}, {"push", 1}, {"reduce", 1}, {"reduceRight", 1},
		{"reverse", 0}, {"shift", 0}, {"slice", 2}, {"some", 1}, {"sort", 1}, {"splice", 2},
		{"toLocaleString", 0}, {"toReversed", 0}, {"toSorted", 1}, {"toSpliced", 2}, {"toString", 0}, {"unshift", 1}, {"values", 0}, {"with", 2},
	} {
		assertNativeShape(t, r, jsGet(t, r, proto, c.name), c.name, c.length)
		assertOwnAttrs(t, r, r.ArrayPrototype, c.name, true, false, true)
	}
	assert.Equal(t, ClassArray, r.ArrayPrototype.Class())
	assert.Equal(t, IntValue(0), jsGet(t, r, proto, "length"))
	assert.Empty(t, r.ArrayPrototype.OwnEnumerableStringKeys())
}

func TestArrayConstructorAndStatics(t *testing.T) {
	r := NewRealm()
	ctor := ObjectValue(r.ArrayCtor)
	call := func(args ...Value) Value {
		v, err := r.Call(ctor, Undefined(), args)
		require.NoError(t, err)
		return v
	}
	assert.Equal(t, uint32(0), call().AsObject().ArrayLength())
	assert.Equal(t, uint32(3), call(IntValue(3)).AsObject().ArrayLength())
	assert.False(t, hasIdx(call(IntValue(3)), 0))
	assert.Equal(t, []any{"3"}, r.ToGo(call(str("3"))))
	assert.Equal(t, []any{int64(1), int64(2)}, r.ToGo(call(IntValue(1), IntValue(2))))
	assert.Equal(t, []any{nil}, r.ToGo(call(Undefined())))
	for _, bad := range []Value{IntValue(-1), NumberValue(1.5), NumberValue(4294967296), NaN()} {
		_, err := r.Construct(ctor, []Value{bad}, nil)
		assert.EqualError(t, err, "RangeError: Invalid array length", bad.String())
	}
	big := call(NumberValue(4294967295))
	assert.Equal(t, uint32(4294967295), big.AsObject().ArrayLength())

	assert.Equal(t, True(), jsCall(t, r, ctor, "isArray", jsArray(r)))
	assert.Equal(t, True(), jsCall(t, r, ctor, "isArray", ObjectValue(r.ArrayPrototype)))
	assert.Equal(t, False(), jsCall(t, r, ctor, "isArray", ObjectValue(arrayLike(t, r, 0, nil))))
	assert.Equal(t, False(), jsCall(t, r, ctor, "isArray", ObjectValue(r.NewObject())))
	assert.Equal(t, False(), jsCall(t, r, ctor, "isArray"))
	assert.Equal(t, False(), jsCall(t, r, ctor, "isArray", jsCall(t, r, jsArray(r), "values")))

	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, ctor, "of")))
	assert.Equal(t, []any{int64(7)}, r.ToGo(jsCall(t, r, ctor, "of", IntValue(7))), "Array.of(7) is [7], not a length")
	assert.Equal(t, []any{int64(1), nil, "x"}, r.ToGo(jsCall(t, r, ctor, "of", IntValue(1), Undefined(), str("x"))))

	// Array.from: arrays copy (holes become undefined), array-likes by length.
	src := holeArray(r, 3, map[uint32]Value{0: IntValue(1), 2: IntValue(3)})
	got := jsCall(t, r, ctor, "from", src)
	assert.Equal(t, []any{int64(1), nil, int64(3)}, r.ToGo(got))
	assert.True(t, hasIdx(got, 1), "holes are materialized as undefined")
	assert.NotSame(t, src.AsObject(), got.AsObject())
	al := arrayLike(t, r, 2, map[string]Value{"0": str("a"), "5": str("ignored")})
	assert.Equal(t, []any{"a", nil}, r.ToGo(jsCall(t, r, ctor, "from", ObjectValue(al))))
	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, ctor, "from", IntValue(5))))
	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, ctor, "from", ObjectValue(r.NewObject()))))
	var log []string
	mapped := jsCall(t, r, ctor, "from", mkArray(t, r, 10, 20), nativeFn(r, func(this Value, args []Value) (Value, error) {
		log = append(log, this.String()+":"+args[0].String()+"@"+args[1].String())
		return NumberValue(args[0].AsNumber() * 2), nil
	}), str("T"))
	assert.Equal(t, []any{int64(20), int64(40)}, r.ToGo(mapped))
	assert.Equal(t, []string{"T:10@0", "T:20@1"}, log)
	// Iterating an array observes growth (array iterator semantics).
	growing := r.NewArray(IntValue(1), IntValue(2))
	grown := jsCall(t, r, ctor, "from", ObjectValue(growing), nativeFn(r, func(_ Value, args []Value) (Value, error) {
		if args[1].AsNumber() == 0 {
			growing.Push(r, IntValue(3))
		}
		return args[0], nil
	}))
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, r.ToGo(grown))
	assert.EqualError(t, jsCallErr(t, r, ctor, "from", jsArray(r), IntValue(1)), "TypeError: 1 is not a function")
	assert.EqualError(t, jsCallErr(t, r, ctor, "from", Undefined()), "TypeError: Cannot convert undefined or null to object")
	assert.EqualError(t, jsCallErr(t, r, ctor, "from", jsArray(r, IntValue(1)), throwingFn(r, "map")), "TypeError: map")
	// Strings and iterator objects go through the realm iteration protocol,
	// which needs an interpreter; without one they fail loudly.
	if v, err := r.Call(jsGet(t, r, ctor, "from"), ctor, []Value{str("ab")}); err != nil {
		assert.ErrorContains(t, err, "not supported yet")
	} else {
		assert.Equal(t, []any{"a", "b"}, r.ToGo(v))
	}
	if v, err := r.Call(jsGet(t, r, ctor, "from"), ctor, []Value{jsCall(t, r, mkArray(t, r, 1, 2), "values")}); err != nil {
		assert.ErrorContains(t, err, "not supported yet")
	} else {
		assert.Equal(t, []any{int64(1), int64(2)}, r.ToGo(v))
	}
	// A custom constructor receives the length and is filled by index.
	var ctorArgs []Value
	custom := r.NewNativeConstructor(r.InternGoString("C"), 0, nil, func(r *Realm, args []Value, nt *Object) (Value, error) {
		ctorArgs = append([]Value(nil), args...)
		return ObjectValue(r.NewObject()), nil
	})
	res, err := r.Call(jsGet(t, r, ctor, "from"), ObjectValue(custom), []Value{ObjectValue(al)})
	require.NoError(t, err)
	assert.Equal(t, []Value{IntValue(2)}, ctorArgs)
	assert.Equal(t, ClassObject, res.AsObject().Class())
	assert.Equal(t, map[string]any{"0": "a", "1": nil, "length": int64(2)}, r.ToGo(res))
	res, err = r.Call(jsGet(t, r, ctor, "from"), ObjectValue(custom), []Value{mkArray(t, r, 9)})
	require.NoError(t, err)
	assert.Empty(t, ctorArgs, "iterables construct with no arguments")
	assert.Equal(t, map[string]any{"0": int64(9), "length": int64(1)}, r.ToGo(res))
	res, err = r.Call(jsGet(t, r, ctor, "of"), ObjectValue(custom), []Value{IntValue(4), IntValue(5)})
	require.NoError(t, err)
	assert.Equal(t, []Value{IntValue(2)}, ctorArgs)
	assert.Equal(t, map[string]any{"0": int64(4), "1": int64(5), "length": int64(2)}, r.ToGo(res))
	res, err = r.Call(jsGet(t, r, ctor, "of"), Undefined(), []Value{IntValue(4)})
	require.NoError(t, err)
	assert.True(t, IsArray(res), "a non-constructor this falls back to ArrayCreate")
}

func TestArrayPushPopShiftUnshift(t *testing.T) {
	r := NewRealm()
	a := mkArray(t, r, 1)
	assert.Equal(t, IntValue(3), jsCall(t, r, a, "push", IntValue(2), IntValue(3)))
	assert.Equal(t, IntValue(3), jsCall(t, r, a, "push"))
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, r.ToGo(a))
	assert.Equal(t, IntValue(3), jsCall(t, r, a, "pop"))
	assert.Equal(t, IntValue(1), jsCall(t, r, a, "shift"))
	assert.Equal(t, []any{int64(2)}, r.ToGo(a))
	assert.Equal(t, IntValue(3), jsCall(t, r, a, "unshift", IntValue(0), IntValue(1)))
	assert.Equal(t, []any{int64(0), int64(1), int64(2)}, r.ToGo(a))
	assert.Equal(t, IntValue(3), jsCall(t, r, a, "unshift"))
	empty := jsArray(r)
	assert.True(t, jsCall(t, r, empty, "pop").IsUndefined())
	assert.True(t, jsCall(t, r, empty, "shift").IsUndefined())
	assert.Equal(t, uint32(0), empty.AsObject().ArrayLength())
	// Holes: pop/shift read through the prototype chain (undefined) and
	// in-place shifts keep holes as holes.
	h := holeArray(r, 3, map[uint32]Value{1: str("m")})
	assert.True(t, jsCall(t, r, h, "pop").IsUndefined())
	assert.Equal(t, uint32(2), h.AsObject().ArrayLength())
	assert.True(t, jsCall(t, r, h, "shift").IsUndefined())
	assert.Equal(t, []any{"m"}, r.ToGo(h))
	h2 := holeArray(r, 3, map[uint32]Value{0: str("a"), 2: str("c")})
	assert.Equal(t, "a", jsString(t, jsCall(t, r, h2, "shift")))
	assert.Equal(t, uint32(2), h2.AsObject().ArrayLength())
	assert.False(t, hasIdx(h2, 0), "the hole moved down")
	assert.Equal(t, "c", jsString(t, jsGet(t, r, h2, "1")))
	jsCall(t, r, h2, "unshift", str("z"))
	assert.False(t, hasIdx(h2, 1))
	assert.Equal(t, []any{"z", nil, "c"}, r.ToGo(h2))
	// Array-likes through call.
	al := arrayLike(t, r, 2, map[string]Value{"0": str("a"), "1": str("b")})
	alv := ObjectValue(al)
	pushFn := jsGet(t, r, ObjectValue(r.ArrayPrototype), "push")
	res, err := r.Call(pushFn, alv, []Value{str("c")})
	require.NoError(t, err)
	assert.Equal(t, IntValue(3), res)
	assert.Equal(t, map[string]any{"0": "a", "1": "b", "2": "c", "length": int64(3)}, r.ToGo(alv))
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "shift"), alv, nil)
	require.NoError(t, err)
	assert.Equal(t, "a", jsString(t, res))
	assert.Equal(t, map[string]any{"0": "b", "1": "c", "length": int64(2)}, r.ToGo(alv))
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "unshift"), alv, []Value{str("u")})
	require.NoError(t, err)
	assert.Equal(t, IntValue(3), res)
	assert.Equal(t, map[string]any{"0": "u", "1": "b", "2": "c", "length": int64(3)}, r.ToGo(alv))
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "pop"), alv, nil)
	require.NoError(t, err)
	assert.Equal(t, "c", jsString(t, res))
	assert.False(t, al.HasOwnProperty(IndexKey(2)))
	noLen := r.NewObject()
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "pop"), ObjectValue(noLen), nil)
	require.NoError(t, err)
	assert.True(t, res.IsUndefined())
	assert.Equal(t, IntValue(0), jsGet(t, r, ObjectValue(noLen), "length"), "pop on an empty array-like sets length 0")
	// Frozen arrays and read-only lengths reject in strict style.
	frozen := mkArray(t, r, 1)
	frozen.AsObject().Freeze(r)
	assert.EqualError(t, jsCallErr(t, r, frozen, "push", IntValue(2)), "TypeError: Cannot assign to read only property '1' of object")
	assert.EqualError(t, jsCallErr(t, r, frozen, "pop"), "TypeError: Cannot delete property '0' of [object Array]")
	assert.EqualError(t, jsCallErr(t, r, frozen, "unshift", IntValue(0)), "TypeError: Cannot assign to read only property '1' of object")
	assert.EqualError(t, jsCallErr(t, r, frozen, "shift"), "TypeError: Cannot delete property '0' of [object Array]")
	roLen := mkArray(t, r, 1)
	var d PropertyDescriptor
	d.SetWritable(false)
	require.NoError(t, roLen.AsObject().DefinePropertyOrThrow(r, lengthKey, d))
	assert.EqualError(t, jsCallErr(t, r, roLen, "push", IntValue(2)), "TypeError: Cannot assign to read only property '1' of object")
	assert.EqualError(t, jsCallErr(t, r, roLen, "pop"), "TypeError: Cannot assign to read only property 'length' of object")
	// push at the array length limit.
	tail := r.NewArrayLen(4294967295)
	assert.EqualError(t, jsCallErr(t, r, ObjectValue(tail), "push", IntValue(1)), "RangeError: Invalid array length")
	// Errors from getters propagate.
	bad := arrayLike(t, r, 1, nil)
	bad.DefineOwnAccessorFast(r, IndexKey(0), throwingFn(r, "get0").AsObject(), nil, attrConfigurable|attrEnumerable)
	_, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "pop"), ObjectValue(bad), nil)
	assert.EqualError(t, err, "TypeError: get0")
}

func TestArraySliceSpliceConcat(t *testing.T) {
	r := NewRealm()
	a := mkArray(t, r, 0, 1, 2, 3, 4)
	assert.Equal(t, []any{int64(1), int64(2)}, r.ToGo(jsCall(t, r, a, "slice", IntValue(1), IntValue(3))))
	assert.Equal(t, []any{int64(3), int64(4)}, r.ToGo(jsCall(t, r, a, "slice", IntValue(-2))))
	assert.Equal(t, []any{int64(0), int64(1), int64(2), int64(3)}, r.ToGo(jsCall(t, r, a, "slice", Undefined(), IntValue(-1))))
	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, a, "slice", IntValue(3), IntValue(1))))
	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, a, "slice", IntValue(10))))
	assert.Equal(t, []any{int64(0), int64(1), int64(2), int64(3), int64(4)}, r.ToGo(jsCall(t, r, a, "slice", NumberValue(negInf), NumberValue(posInf))))
	assert.Equal(t, []any{int64(1), int64(2)}, r.ToGo(jsCall(t, r, a, "slice", str("1"), NumberValue(3.9))))
	cp := jsCall(t, r, a, "slice")
	assert.NotSame(t, a.AsObject(), cp.AsObject())
	assert.Equal(t, r.ToGo(a), r.ToGo(cp))
	h := holeArray(r, 4, map[uint32]Value{0: str("a"), 3: str("d")})
	hs := jsCall(t, r, h, "slice", IntValue(0), IntValue(3))
	assert.Equal(t, uint32(3), hs.AsObject().ArrayLength())
	assert.True(t, hasIdx(hs, 0))
	assert.False(t, hasIdx(hs, 1), "holes are preserved")
	al := arrayLike(t, r, 3, map[string]Value{"0": str("x"), "2": str("z")})
	als, err := r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "slice"), ObjectValue(al), []Value{IntValue(0)})
	require.NoError(t, err)
	assert.True(t, IsArray(als))
	assert.Equal(t, []any{"x", nil, "z"}, r.ToGo(als))
	assert.False(t, hasIdx(als, 1))

	// splice: remove, insert, replace, negative start, deleteCount forms.
	s := mkArray(t, r, "a", "b", "c", "d", "e")
	assert.Equal(t, []any{"b", "c"}, r.ToGo(jsCall(t, r, s, "splice", IntValue(1), IntValue(2))))
	assert.Equal(t, []any{"a", "d", "e"}, r.ToGo(s))
	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, s, "splice", IntValue(1), IntValue(0), str("X"), str("Y"))))
	assert.Equal(t, []any{"a", "X", "Y", "d", "e"}, r.ToGo(s))
	assert.Equal(t, []any{"X"}, r.ToGo(jsCall(t, r, s, "splice", IntValue(1), IntValue(1), str("Z"))))
	assert.Equal(t, []any{"a", "Z", "Y", "d", "e"}, r.ToGo(s))
	assert.Equal(t, []any{"d", "e"}, r.ToGo(jsCall(t, r, s, "splice", IntValue(-2))))
	assert.Equal(t, []any{"a", "Z", "Y"}, r.ToGo(s))
	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, s, "splice", IntValue(1), Undefined())), "explicit undefined deleteCount deletes nothing")
	assert.Equal(t, []any{}, r.ToGo(jsCall(t, r, s, "splice")))
	assert.Equal(t, []any{"a", "Z", "Y"}, r.ToGo(s))
	assert.Equal(t, []any{"Y"}, r.ToGo(jsCall(t, r, s, "splice", IntValue(2), IntValue(99))))
	assert.Equal(t, []any{"a", "Z"}, r.ToGo(jsCall(t, r, s, "splice", NumberValue(negInf))))
	assert.Equal(t, uint32(0), s.AsObject().ArrayLength())
	hsp := holeArray(r, 5, map[uint32]Value{0: str("a"), 2: str("c"), 4: str("e")})
	removed := jsCall(t, r, hsp, "splice", IntValue(1), IntValue(2), str("n"))
	assert.Equal(t, uint32(2), removed.AsObject().ArrayLength())
	assert.False(t, hasIdx(removed, 0))
	assert.Equal(t, "c", jsString(t, jsGet(t, r, removed, "1")))
	assert.Equal(t, []any{"a", "n", nil, "e"}, r.ToGo(hsp))
	assert.False(t, hasIdx(hsp, 2), "moved holes stay holes")
	spl := arrayLike(t, r, 3, map[string]Value{"0": IntValue(1), "1": IntValue(2), "2": IntValue(3)})
	rem, err := r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "splice"), ObjectValue(spl), []Value{IntValue(0), IntValue(1), str("i"), str("j")})
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1)}, r.ToGo(rem))
	assert.Equal(t, map[string]any{"0": "i", "1": "j", "2": int64(2), "3": int64(3), "length": int64(4)}, r.ToGo(ObjectValue(spl)))
	frozen := mkArray(t, r, 1, 2)
	frozen.AsObject().Freeze(r)
	assert.ErrorContains(t, jsCallErr(t, r, frozen, "splice", IntValue(0), IntValue(1)), "TypeError")
	assert.Equal(t, []any{int64(1), int64(2)}, r.ToGo(frozen))

	// concat.
	c := jsCall(t, r, mkArray(t, r, 1), "concat", mkArray(t, r, 2, 3), IntValue(4), mkArray(t, r), Undefined(), mkArray(t, r, mkArray(t, r, 5)))
	assert.Equal(t, []any{int64(1), int64(2), int64(3), int64(4), nil, []any{int64(5)}}, r.ToGo(c))
	hc := jsCall(t, r, holeArray(r, 2, map[uint32]Value{0: str("a")}), "concat", holeArray(r, 2, map[uint32]Value{1: str("d")}))
	assert.Equal(t, uint32(4), hc.AsObject().ArrayLength())
	assert.False(t, hasIdx(hc, 1))
	assert.False(t, hasIdx(hc, 2))
	assert.Equal(t, "d", jsString(t, jsGet(t, r, hc, "3")))
	sparse := r.NewArray()
	mustSet(t, r, sparse, "5000", str("far"))
	sc := jsCall(t, r, mkArray(t, r, "x"), "concat", ObjectValue(sparse), str("y"))
	assert.Equal(t, uint32(5003), sc.AsObject().ArrayLength())
	assert.Equal(t, "far", jsString(t, jsGet(t, r, sc, "5001")))
	assert.Equal(t, "y", jsString(t, jsGet(t, r, sc, "5002")))
	assert.False(t, hasIdx(sc, 1))
	nonArr, err := r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "concat"), ObjectValue(arrayLike(t, r, 1, map[string]Value{"0": IntValue(1)})), []Value{IntValue(2)})
	require.NoError(t, err)
	assert.Equal(t, []any{map[string]any{"0": int64(1), "length": int64(1)}, int64(2)}, r.ToGo(nonArr), "array-likes are not spread")
	unchanged := mkArray(t, r, 1)
	jsCall(t, r, unchanged, "concat", IntValue(2))
	assert.Equal(t, []any{int64(1)}, r.ToGo(unchanged))
}

func TestArrayJoinAndToString(t *testing.T) {
	r := NewRealm()
	assert.Equal(t, "1,2,3", jsString(t, jsCall(t, r, mkArray(t, r, 1, 2, 3), "join")))
	assert.Equal(t, "1-2-3", jsString(t, jsCall(t, r, mkArray(t, r, 1, 2, 3), "join", str("-"))))
	assert.Equal(t, "123", jsString(t, jsCall(t, r, mkArray(t, r, 1, 2, 3), "join", str(""))))
	assert.Equal(t, "1,2", jsString(t, jsCall(t, r, mkArray(t, r, 1, 2), "join", Undefined())))
	assert.Equal(t, "1null2", jsString(t, jsCall(t, r, mkArray(t, r, 1, 2), "join", Null())))
	assert.Equal(t, "a,,b,", jsString(t, jsCall(t, r, mkArray(t, r, "a", nil, "b", nil), "join")))
	assert.Equal(t, ",,", jsString(t, jsCall(t, r, ObjectValue(r.NewArrayLen(3)), "join")))
	assert.Equal(t, "", jsString(t, jsCall(t, r, jsArray(r), "join")))
	assert.Equal(t, "0.30000000000000004,1e+21,0,NaN,true", jsString(t, jsCall(t, r, jsArray(r, NumberValue(pointOne+pointTwo), NumberValue(1e21), negZero(), NaN(), True()), "join")))
	assert.Equal(t, "0", jsString(t, jsCall(t, r, jsArray(r, negZero()), "join")))
	nested := mkArray(t, r, 1, []any{2, []any{3, 4}}, "x")
	assert.Equal(t, "1,2,3,4,x", jsString(t, jsCall(t, r, nested, "join")))
	assert.Equal(t, "1,2,3,4,x", jsString(t, jsCall(t, r, nested, "toString")))
	assert.Equal(t, "é|ü", jsString(t, jsCall(t, r, mkArray(t, r, "é", "ü"), "join", str("|"))))
	al := arrayLike(t, r, 3, map[string]Value{"0": str("x"), "2": str("z")})
	res, err := r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "join"), ObjectValue(al), []Value{str("+")})
	require.NoError(t, err)
	assert.Equal(t, "x++z", jsString(t, res))
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "join"), str("abc"), nil)
	require.NoError(t, err)
	assert.Equal(t, "a,b,c", jsString(t, res))
	// Elements with custom toString; errors propagate; separator coerced first.
	o := r.NewObject()
	o.DefineOwnDataFast(r, StringKey(AtomToString), nativeFn(r, func(Value, []Value) (Value, error) { return str("O"), nil }), attrHidden)
	assert.Equal(t, "O,1", jsString(t, jsCall(t, r, jsArray(r, ObjectValue(o), IntValue(1)), "join")))
	assert.ErrorContains(t, jsCallErr(t, r, jsArray(r, ObjectValue(r.NewObjectWithProto(nil))), "join"), "Cannot convert object to primitive value")
	assert.ErrorContains(t, jsCallErr(t, r, jsArray(r, IntValue(1)), "join", SymbolValue(SymIterator)), "Symbol")
	// Number coercion routes through join.
	num := func(v Value) Value {
		n, err := r.Call(ObjectValue(r.NumberCtor), Undefined(), []Value{v})
		require.NoError(t, err)
		return n
	}
	assert.Equal(t, IntValue(5), num(mkArray(t, r, 5)))
	assert.Equal(t, IntValue(0), num(jsArray(r)))
	assert.True(t, math.IsNaN(jsNumber(t, num(mkArray(t, r, 1, 2)))))
	assert.Equal(t, False(), jsCall(t, r, ObjectValue(r.Global), "isNaN", jsArray(r)))
	// toString falls back to Object.prototype.toString when join is not callable.
	weird := r.NewArray(IntValue(1))
	mustSet(t, r, weird, "join", IntValue(0))
	assert.Equal(t, "[object Array]", jsString(t, jsCall(t, r, ObjectValue(weird), "toString")))
	plain := r.NewObject()
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "toString"), ObjectValue(plain), nil)
	require.NoError(t, err)
	assert.Equal(t, "[object Object]", jsString(t, res))
	// An array being joined joins as "" inside itself.
	cyc := r.NewArray(IntValue(1))
	cyc.Push(r, ObjectValue(cyc))
	assert.Equal(t, "1,", jsString(t, jsCall(t, r, ObjectValue(cyc), "join")))
	// Interrupts are honoured on long joins.
	items := make([]Value, 20000)
	for i := range items {
		items[i] = str("x")
	}
	big := ObjectValue(r.NewArrayFromSlice(items))
	r.Interrupt("stop")
	err = jsCallErr(t, r, big, "join")
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	r.ClearInterrupt()
	assert.Equal(t, strings.Repeat("x,", 19999)+"x", jsString(t, jsCall(t, r, big, "join")))
}

// TestArrayJoinWideAlloc checks that a long join with a UTF-16 piece
// starts in UTF-16 storage of the joined length: from bytes, it upgraded at
// that piece to twice the length written and grew from there, allocating
// about five times the result's units.
func TestArrayJoinWideAlloc(t *testing.T) {
	for _, at := range []string{"0", "1000", "1999"} {
		r := NewRealm()
		evalValue(t, r, "var a = Array(2000).fill('abcdefghijklmnopqrstuvwxyz'); a["+at+"] = '\\u00e9'")
		join := evalValue(t, r, "() => a.join()")
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		const n = 20
		var res Value
		for range n {
			v, err := r.Call(join, Undefined(), nil)
			require.NoError(t, err)
			res = v
		}
		runtime.ReadMemStats(&m1)
		units := 26*1999 + 1 + 1999
		require.Equal(t, units, res.AsString().Len())
		// The result's units, 2 bytes each, rounded up to whole pages, and a
		// little for the string's header, the call and the runtime.
		per := (m1.TotalAlloc - m0.TotalAlloc) / n
		assert.Less(t, per, uint64(2*units)*5/4, "UTF-16 piece at %s", at)
	}
}

// TestArrayJoinPastBudget checks that a join whose result is past the
// realm's memory budget does not reserve it: it allocated the whole result,
// 2 bytes a unit with a UTF-16 piece, before the limit stopped it.
func TestArrayJoinPastBudget(t *testing.T) {
	for _, fill := range []string{"'a'.repeat(400)", "'\\u00e9'.repeat(400)"} {
		r := NewRealm()
		r.SetMemoryLimit(8 << 20)
		evalValue(t, r, "var a = Array(100000).fill("+fill+")")
		join := evalValue(t, r, "() => a.join()")
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		_, err := r.Call(join, Undefined(), nil)
		runtime.ReadMemStats(&m1)
		require.ErrorIs(t, err, ErrMemoryLimit, fill)
		t.Logf("%s: %d KiB", fill, (m1.TotalAlloc-m0.TotalAlloc)>>10)
		assert.Less(t, m1.TotalAlloc-m0.TotalAlloc, uint64(32<<20), fill)
	}
}

// TestArrayJoinNumbersReserve checks that a join of integers with a UTF-16
// separator reserves their digits, not 24 units per number: a million small
// numbers reserved 48 MB, charged to the memory limit, for a few MB.
func TestArrayJoinNumbersReserve(t *testing.T) {
	r := NewRealm()
	evalValue(t, r, "var a = Array.from({length: 20000}, (_, i) => i % 2 ? i : -i)")
	join := evalValue(t, r, "() => a.join('\\u3001')")
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	const n = 20
	var res Value
	for range n {
		v, err := r.Call(join, Undefined(), nil)
		require.NoError(t, err)
		res = v
	}
	runtime.ReadMemStats(&m1)
	want := make([]string, 20000)
	for i := range want {
		if i%2 == 0 && i != 0 {
			want[i] = "-"
		}
		want[i] += strconv.Itoa(i)
	}
	require.Equal(t, strings.Join(want, "、"), jsString(t, res))
	units := res.AsString().Len()
	per := (m1.TotalAlloc - m0.TotalAlloc) / n
	assert.Less(t, per, uint64(2*units)*5/4)
	// A number joins as ToString writes it, in byte and in UTF-16 storage.
	const nums = "[NaN, Infinity, -Infinity, -0, 0, 0.1, 1e21, 1.5e-7, 2**53, 2**31, -(2**31), 123.456, -1e-300]"
	const digits = "NaN,Infinity,-Infinity,0,0,0.1,1e+21,1.5e-7,9007199254740992,2147483648,-2147483648,123.456,-1e-300"
	require.Equal(t, digits, jsString(t, evalValue(t, r, nums+".join()")))
	require.Equal(t, strings.ReplaceAll(digits, ",", "、"), jsString(t, evalValue(t, r, nums+".join('\\u3001')")))
	require.Equal(t, "é,"+digits, jsString(t, evalValue(t, r, "['é', ..."+nums+"].join()")))
}

// TestArrayJoinCycles covers the stack of objects being joined: an object
// joins as "" inside its own join or toLocaleString (as in V8),
// however the nested join is reached, and leaves the stack on every exit.
func TestArrayJoinCycles(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"self", `const a = [1]; a.push(a); return a.join()`, "1,"},
		{"only self", `const a = []; a[0] = a; a[1] = a; return '[' + a.join() + ']'`, "[,]"},
		{"nested", `const a = [1], b = [2, a]; a.push(b); return [a.join(), b.join('-')].join('|')`, "1,2,|2-1,"},
		{"separator inside", `const a = [1, 2]; a.push([3, a]); return a.join('-')`, "1-2-3,"},
		{"shared, not cyclic", `const b = [1, [2]]; return [b, b, [b]].join()`, "1,2,1,2,1,2"},
		{"toString", `const a = [1]; a.push(a); return [a.toString(), String(a), a + '', ` + "`${a}`" + `].join('|')`, "1,|1,|1,|1,"},
		{"toLocaleString", `const a = [1]; a.push(a); return a.toLocaleString()`, "1,"},
		{"toLocaleString inside join", `const a = [1, {toString() { return a.toLocaleString() }}]; return a.join()`, "1,"},
		{"join inside toLocaleString", `const a = [1, {toLocaleString() { return '<' + a.join() + '>' }}]; return a.toLocaleString()`, "1,<>"},
		{"toString of another array", `const a = [1], b = [a]; a.push({toString() { return b.join() + a.join() }}); return b.join()`, "1,"},
		{"getter", `const a = [1, 2]; Object.defineProperty(a, 1, {get() { return '<' + a.join('+') + '>' }}); return a.join()`, "1,<>"},
		{"inherited getter", `const a = [1, , 3]; Object.setPrototypeOf(a, Object.create(Array.prototype, {1: {get() { return '<' + this.join('+') + '>' }}})); return a.join()`, "1,<>,3"},
		{"separator", `const a = [1, 2]; return [[1].join({toString() { return a.join() }}), a.join({toString() { return a.join() }})].join('|')`, "1|12"},
		{"array-like", `const o = {length: 2, 0: 1, join: Array.prototype.join, toString() { return this.join() }}; o[1] = o; return o.join()`, "1,"},
		{"array-like length", `const o = {get length() { return '<' + Array.prototype.join.call(o) + '>' }}; return Array.prototype.join.call(o)`, ""},
		{"string", `return Array.prototype.join.call('ab', Array.prototype.join.call('cd'))`, "ac,db"},
		{"proxy of the array", `const a = [1]; const p = new Proxy(a, {}); a.push(p); return [a.join(), p.join(), String(p)].join('|')`, "1,1,|1,|1,"},
		{"proxy getter", `const a = [1, 2]; const p = new Proxy(a, {get(t, k, r) { return k === '1' ? '<' + p.join('+') + '>' : Reflect.get(t, k, r) }}); return p.join()`, "1,<>"},
		{"throw", `let n = 0; const a = [1, {toString() { if (n++ === 0) throw new Error('x'); return 'y' }}]; let e; try { a.join() } catch (x) { e = x.message } return [e, a.join()].join('|')`, "x|1,y"},
		{"throw inside", `let n = 0; const b = [2, {toString() { if (n++ === 0) throw new Error('x'); return 'y' }}]; const a = [1, b]; b.push(a); let e; try { a.join() } catch (x) { e = x.message } return [e, a.join(), b.join()].join('|')`, "x|1,2,y,|2,y,1,"},
		{"throw toLocaleString", `let n = 0; const a = [1, {toLocaleString() { if (n++ === 0) throw new Error('x'); return 'y' }}]; let e; try { a.toLocaleString() } catch (x) { e = x.message } return [e, a.toLocaleString(), a.join()].join('|')`, "x|1,y|1,[object Object]"},
		{"throw length", `let n = 0; const o = {get length() { if (n++ === 0) throw new Error('x'); return 1 }, 0: 'z'}; let e; try { Array.prototype.join.call(o) } catch (x) { e = x.message } return [e, Array.prototype.join.call(o)].join('|')`, "x|z"},
		{"throw separator", `const a = [1, 2]; let e; try { a.join({toString() { throw new Error('x') }}) } catch (x) { e = x.message } return [e, a.join('-')].join('|')`, "x|1-2"},
		{"symbol element", `const a = [1, Symbol()]; let e; try { a.join() } catch (x) { e = x.name } a[1] = a; return [e, a.join()].join('|')`, "TypeError|1,"},
		{"deep", `let a = [1]; for (let i = 0; i < 100000; i++) a = [a]; let e; try { a.join() } catch (x) { e = x.name + ': ' + x.message } return [e, String([[1, [2]], 3])].join('|')`, "RangeError: Maximum call stack size exceeded|1,2,3"},
		{"deep toLocaleString", `let a = [1]; for (let i = 0; i < 100000; i++) a = [a]; let e; try { a.toLocaleString() } catch (x) { e = x.name } const b = [1]; b.push(b); return [e, b.toLocaleString()].join('|')`, "RangeError|1,"},
	})
}

// TestArrayJoinStack checks that a join that can run no JavaScript stays off
// the stack (and allocates no lazy state), and that the stack is empty after
// a join ends in an error or a Go panic.
func TestArrayJoinStack(t *testing.T) {
	r := NewRealm()
	flat := ObjectValue(r.NewArray(IntValue(1), str("a"), True(), Null(), Undefined(), NumberValue(0.5)))
	assert.Equal(t, "1,a,true,,,0.5", jsString(t, jsCall(t, r, flat, "join")))
	assert.Equal(t, "1-a-true---0.5", jsString(t, jsCall(t, r, flat, "join", str("-"))))
	assert.Nil(t, r.lazy, "a join of primitives needs no stack")
	joins := func() int {
		if r.lazy == nil {
			return 0
		}
		return len(r.lazy.joins)
	}

	nested := ObjectValue(r.NewArray(IntValue(1), ObjectValue(r.NewArray(IntValue(2)))))
	assert.Equal(t, "1,2", jsString(t, jsCall(t, r, nested, "join")))
	require.NotNil(t, r.lazy)
	assert.Equal(t, 0, joins())
	assert.Equal(t, 8, cap(r.lazy.joins))

	thrower := r.NewObject()
	thrower.DefineOwnDataFast(r, StringKey(AtomToString), nativeFn(r, func(Value, []Value) (Value, error) { return Undefined(), r.TypeError("boom") }), attrHidden)
	errs := ObjectValue(r.NewArray(IntValue(1), ObjectValue(r.NewArray(ObjectValue(thrower)))))
	assert.EqualError(t, jsCallErr(t, r, errs, "join"), "TypeError: boom")
	assert.Equal(t, 0, joins())
	assert.EqualError(t, jsCallErr(t, r, errs, "toLocaleString"), "TypeError: boom")
	assert.Equal(t, 0, joins())

	panicker := r.NewObject()
	panicker.DefineOwnDataFast(r, StringKey(AtomToString), nativeFn(r, func(Value, []Value) (Value, error) { panic("host panic") }), attrHidden)
	panics := ObjectValue(r.NewArray(ObjectValue(r.NewArray(ObjectValue(panicker)))))
	for _, name := range []string{"join", "toLocaleString", "toString"} {
		cs := r.CallState()
		assert.PanicsWithValue(t, "host panic", func() { jsCall(t, r, panics, name) }, name)
		r.RestoreCallState(cs)
		assert.Equal(t, 0, joins(), name)
	}
	assert.Equal(t, "1,2", jsString(t, jsCall(t, r, nested, "join")))
}

func TestArraySearch(t *testing.T) {
	r := NewRealm()
	a := jsArray(r, IntValue(1), str("2"), NaN(), negZero(), IntValue(1), Undefined())
	idx := func(m string, args ...Value) float64 { return jsNumber(t, jsCall(t, r, a, m, args...)) }
	assert.Equal(t, 0.0, idx("indexOf", IntValue(1)))
	assert.Equal(t, 4.0, idx("indexOf", IntValue(1), IntValue(1)))
	assert.Equal(t, 4.0, idx("indexOf", IntValue(1), IntValue(-2)))
	assert.Equal(t, 0.0, idx("indexOf", IntValue(1), IntValue(-100)))
	assert.Equal(t, -1.0, idx("indexOf", IntValue(1), IntValue(100)))
	assert.Equal(t, -1.0, idx("indexOf", IntValue(1), NumberValue(posInf)))
	assert.Equal(t, 0.0, idx("indexOf", IntValue(1), NumberValue(negInf)))
	assert.Equal(t, -1.0, idx("indexOf", IntValue(2)), "strict equality")
	assert.Equal(t, 1.0, idx("indexOf", str("2")))
	assert.Equal(t, -1.0, idx("indexOf", NaN()))
	assert.Equal(t, 3.0, idx("indexOf", IntValue(0)), "+0 finds -0")
	assert.Equal(t, 5.0, idx("indexOf", Undefined()))
	assert.Equal(t, 5.0, idx("indexOf"), "a missing search element is undefined")
	assert.Equal(t, 4.0, idx("lastIndexOf", IntValue(1)))
	assert.Equal(t, 0.0, idx("lastIndexOf", IntValue(1), IntValue(3)))
	assert.Equal(t, 0.0, idx("lastIndexOf", IntValue(1), Undefined()), "explicit undefined fromIndex is 0")
	assert.Equal(t, 4.0, idx("lastIndexOf", IntValue(1), IntValue(-1)))
	assert.Equal(t, 4.0, idx("lastIndexOf", IntValue(1), IntValue(-2)))
	assert.Equal(t, 0.0, idx("lastIndexOf", IntValue(1), IntValue(-3)))
	assert.Equal(t, -1.0, idx("lastIndexOf", IntValue(1), IntValue(-100)))
	assert.Equal(t, -1.0, idx("lastIndexOf", IntValue(1), NumberValue(negInf)))
	assert.Equal(t, 4.0, idx("lastIndexOf", IntValue(1), NumberValue(posInf)))
	assert.Equal(t, -1.0, idx("lastIndexOf", NaN()))
	assert.Equal(t, True(), jsCall(t, r, a, "includes", NaN()), "SameValueZero finds NaN")
	assert.Equal(t, True(), jsCall(t, r, a, "includes", IntValue(0)))
	assert.Equal(t, True(), jsCall(t, r, a, "includes", IntValue(1), IntValue(4)))
	assert.Equal(t, False(), jsCall(t, r, a, "includes", IntValue(1), IntValue(5)))
	assert.Equal(t, True(), jsCall(t, r, a, "includes", IntValue(1), IntValue(-2)))
	assert.Equal(t, False(), jsCall(t, r, a, "includes", IntValue(2)))
	assert.Equal(t, False(), jsCall(t, r, a, "includes", IntValue(1), NumberValue(posInf)))
	// Holes: skipped by indexOf/lastIndexOf, read as undefined by includes.
	h := holeArray(r, 3, map[uint32]Value{0: IntValue(1)})
	assert.Equal(t, -1.0, jsNumber(t, jsCall(t, r, h, "indexOf", Undefined())))
	assert.Equal(t, -1.0, jsNumber(t, jsCall(t, r, h, "lastIndexOf", Undefined())))
	assert.Equal(t, True(), jsCall(t, r, h, "includes", Undefined()))
	assert.Equal(t, False(), jsCall(t, r, jsArray(r), "includes", Undefined()))
	assert.Equal(t, -1.0, jsNumber(t, jsCall(t, r, jsArray(r), "indexOf", Undefined())))
	// Inherited index properties are seen by the generic path.
	mustSet(t, r, r.ArrayPrototype, "1", str("inherited"))
	assert.Equal(t, 1.0, jsNumber(t, jsCall(t, r, h, "indexOf", str("inherited"))))
	assert.Equal(t, True(), jsCall(t, r, h, "includes", str("inherited")))
	require.True(t, r.ArrayPrototype.Delete(r, IndexKey(1)))
	// Strings and array-likes through call.
	inc := jsGet(t, r, ObjectValue(r.ArrayPrototype), "includes")
	res, err := r.Call(inc, str("abc"), []Value{str("b")})
	require.NoError(t, err)
	assert.Equal(t, True(), res)
	io := jsGet(t, r, ObjectValue(r.ArrayPrototype), "indexOf")
	res, err = r.Call(io, ObjectValue(arrayLike(t, r, 2, map[string]Value{"1": str("q")})), []Value{str("q")})
	require.NoError(t, err)
	assert.Equal(t, IntValue(1), res)
	// fromIndex coercion errors propagate; a long search is interruptible.
	assert.ErrorContains(t, jsCallErr(t, r, a, "indexOf", IntValue(1), SymbolValue(SymIterator)), "Symbol")
	items := make([]Value, 10000)
	for i := range items {
		items[i] = IntValue(i)
	}
	big := ObjectValue(r.NewArrayFromSlice(items))
	r.Interrupt(nil)
	var ie *InterruptedError
	assert.ErrorAs(t, jsCallErr(t, r, big, "indexOf", IntValue(-1)), &ie)
	r.ClearInterrupt()
	assert.Equal(t, 9999.0, jsNumber(t, jsCall(t, r, big, "lastIndexOf", IntValue(9999))))
}

func TestArrayFind(t *testing.T) {
	r := NewRealm()
	a := holeArray(r, 4, map[uint32]Value{0: IntValue(5), 1: IntValue(12), 3: IntValue(8)})
	var log []string
	pred := func(want float64) Value {
		return nativeFn(r, func(this Value, args []Value) (Value, error) {
			log = append(log, this.String()+":"+args[0].String()+"@"+args[1].String())
			assert.Same(t, a.AsObject(), args[2].AsObject())
			return Bool(args[0].IsNumber() && args[0].AsNumber() > want), nil
		})
	}
	assert.Equal(t, IntValue(12), jsCall(t, r, a, "find", pred(10), str("T")))
	assert.Equal(t, []string{"T:5@0", "T:12@1"}, log)
	log = nil
	assert.Equal(t, IntValue(1), jsCall(t, r, a, "findIndex", pred(10)))
	assert.Equal(t, []string{"undefined:5@0", "undefined:12@1"}, log)
	log = nil
	assert.Equal(t, IntValue(8), jsCall(t, r, a, "findLast", pred(6)))
	assert.Equal(t, []string{"undefined:8@3"}, log)
	log = nil
	assert.Equal(t, IntValue(1), jsCall(t, r, a, "findLastIndex", pred(10)))
	assert.Equal(t, []string{"undefined:8@3", "undefined:undefined@2", "undefined:12@1"}, log, "holes are visited as undefined")
	log = nil
	assert.True(t, jsCall(t, r, a, "find", pred(100)).IsUndefined())
	assert.Equal(t, IntValue(-1), jsCall(t, r, a, "findIndex", pred(100)))
	assert.True(t, jsCall(t, r, a, "findLast", pred(100)).IsUndefined())
	assert.Equal(t, IntValue(-1), jsCall(t, r, a, "findLastIndex", pred(100)))
	assert.Len(t, log, 16)
	assert.EqualError(t, jsCallErr(t, r, a, "find", throwingFn(r, "pred")), "TypeError: pred")
	for _, m := range []string{"find", "findIndex", "findLast", "findLastIndex"} {
		assert.EqualError(t, jsCallErr(t, r, a, m), "TypeError: undefined is not a function", m)
		assert.EqualError(t, jsCallErr(t, r, a, m, IntValue(1)), "TypeError: 1 is not a function", m)
	}
	// Truthiness of the predicate result is what counts.
	assert.Equal(t, IntValue(5), jsCall(t, r, a, "find", nativeFn(r, func(Value, []Value) (Value, error) { return str("yes"), nil })))
	assert.True(t, jsCall(t, r, a, "find", nativeFn(r, func(Value, []Value) (Value, error) { return str(""), nil })).IsUndefined())
	fn := jsGet(t, r, ObjectValue(r.ArrayPrototype), "findIndex")
	res, err := r.Call(fn, str("abc"), []Value{nativeFn(r, func(_ Value, args []Value) (Value, error) { return Bool(args[0].AsString().GoString() == "c"), nil })})
	require.NoError(t, err)
	assert.Equal(t, IntValue(2), res)
}

func TestArrayIterationMethods(t *testing.T) {
	r := NewRealm()
	a := holeArray(r, 4, map[uint32]Value{0: IntValue(1), 1: IntValue(2), 3: IntValue(4)})
	var log []string
	rec := func(ret func(v Value) Value) Value {
		return nativeFn(r, func(this Value, args []Value) (Value, error) {
			log = append(log, this.String()+":"+args[0].String()+"@"+args[1].String())
			assert.Same(t, a.AsObject(), args[2].AsObject())
			return ret(args[0]), nil
		})
	}
	ident := func(v Value) Value { return v }
	assert.True(t, jsCall(t, r, a, "forEach", rec(ident), str("T")).IsUndefined())
	assert.Equal(t, []string{"T:1@0", "T:2@1", "T:4@3"}, log, "holes are skipped")
	log = nil
	m := jsCall(t, r, a, "map", rec(func(v Value) Value { return NumberValue(v.AsNumber() * 10) }))
	assert.Equal(t, []any{int64(10), int64(20), nil, int64(40)}, r.ToGo(m))
	assert.False(t, hasIdx(m, 2), "map preserves holes")
	assert.Equal(t, uint32(4), m.AsObject().ArrayLength())
	log = nil
	f := jsCall(t, r, a, "filter", rec(func(v Value) Value { return Bool(v.AsNumber() != 2) }))
	assert.Equal(t, []any{int64(1), int64(4)}, r.ToGo(f))
	assert.Equal(t, uint32(2), f.AsObject().ArrayLength())
	log = nil
	assert.Equal(t, True(), jsCall(t, r, a, "some", rec(func(v Value) Value { return Bool(v.AsNumber() == 2) })))
	assert.Equal(t, []string{"undefined:1@0", "undefined:2@1"}, log, "some stops at the first hit")
	log = nil
	assert.Equal(t, False(), jsCall(t, r, a, "every", rec(func(v Value) Value { return Bool(v.AsNumber() == 1) })))
	assert.Equal(t, []string{"undefined:1@0", "undefined:2@1"}, log)
	assert.Equal(t, True(), jsCall(t, r, a, "every", rec(func(Value) Value { return str("truthy") })))
	assert.Equal(t, True(), jsCall(t, r, jsArray(r), "every", rec(ident)))
	assert.Equal(t, False(), jsCall(t, r, jsArray(r), "some", rec(ident)))
	// The length is fixed at entry; pushes inside the callback are not visited,
	// but mutations of not-yet-visited indices are.
	grow := r.NewArray(IntValue(1), IntValue(2))
	log = nil
	jsCall(t, r, ObjectValue(grow), "forEach", nativeFn(r, func(_ Value, args []Value) (Value, error) {
		log = append(log, args[0].String())
		if args[1].AsNumber() == 0 {
			grow.Push(r, IntValue(3))
			require.NoError(t, grow.SetProp(r, IndexKey(1), IntValue(20)))
		}
		return Undefined(), nil
	}))
	assert.Equal(t, []string{"1", "20"}, log)
	// reduce / reduceRight.
	sum := nativeFn(r, func(_ Value, args []Value) (Value, error) {
		log = append(log, args[0].String()+"+"+args[1].String()+"@"+args[2].String())
		assert.True(t, IsArray(args[3]))
		return NumberValue(args[0].AsNumber() + args[1].AsNumber()), nil
	})
	log = nil
	assert.Equal(t, IntValue(7), jsCall(t, r, a, "reduce", sum))
	assert.Equal(t, []string{"1+2@1", "3+4@3"}, log, "no initial value: first present element seeds")
	log = nil
	assert.Equal(t, IntValue(17), jsCall(t, r, a, "reduce", sum, IntValue(10)))
	assert.Equal(t, []string{"10+1@0", "11+2@1", "13+4@3"}, log)
	log = nil
	assert.Equal(t, IntValue(7), jsCall(t, r, a, "reduceRight", sum))
	assert.Equal(t, []string{"4+2@1", "6+1@0"}, log)
	log = nil
	assert.Equal(t, IntValue(107), jsCall(t, r, a, "reduceRight", sum, IntValue(100)))
	assert.Equal(t, []string{"100+4@3", "104+2@1", "106+1@0"}, log)
	assert.Equal(t, IntValue(9), jsCall(t, r, jsArray(r), "reduce", sum, IntValue(9)))
	assert.Equal(t, IntValue(9), jsCall(t, r, jsArray(r, IntValue(9)), "reduce", sum))
	assert.True(t, jsCall(t, r, jsArray(r), "reduce", sum, Undefined()).IsUndefined(), "an explicit undefined initial value counts as present")
	assert.EqualError(t, jsCallErr(t, r, jsArray(r), "reduce", sum), "TypeError: Reduce of empty array with no initial value")
	assert.EqualError(t, jsCallErr(t, r, ObjectValue(r.NewArrayLen(3)), "reduceRight", sum), "TypeError: Reduce of empty array with no initial value")
	// Callback errors and non-callables.
	for _, m := range []string{"forEach", "map", "filter", "some", "every", "reduce", "reduceRight"} {
		assert.EqualError(t, jsCallErr(t, r, a, m, throwingFn(r, m)), "TypeError: "+m, m)
		assert.EqualError(t, jsCallErr(t, r, a, m), "TypeError: undefined is not a function", m)
		assert.EqualError(t, jsCallErr(t, r, a, m, ObjectValue(r.NewObject())), "TypeError: [object Object] is not a function", m)
	}
	// Array-likes and strings through call.
	mapFn := jsGet(t, r, ObjectValue(r.ArrayPrototype), "map")
	res, err := r.Call(mapFn, str("ab"), []Value{nativeFn(r, func(_ Value, args []Value) (Value, error) { return args[1], nil })})
	require.NoError(t, err)
	assert.Equal(t, []any{int64(0), int64(1)}, r.ToGo(res))
	al := arrayLike(t, r, 3, map[string]Value{"0": IntValue(1), "2": IntValue(3)})
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "filter"), ObjectValue(al), []Value{nativeFn(r, func(Value, []Value) (Value, error) { return True(), nil })})
	require.NoError(t, err)
	assert.Equal(t, []any{int64(1), int64(3)}, r.ToGo(res))
	// A huge array-like length maps through the sparse path without
	// materializing storage.
	huge := arrayLike(t, r, 1<<20, map[string]Value{"5": str("v")})
	res, err = r.Call(mapFn, ObjectValue(huge), []Value{nativeFn(r, func(_ Value, args []Value) (Value, error) { return args[0], nil })})
	require.NoError(t, err)
	assert.Equal(t, uint32(1<<20), res.AsObject().ArrayLength())
	assert.Equal(t, "v", jsString(t, jsGet(t, r, res, "5")))
	assert.Less(t, len(res.AsObject().Elements()), 1024)
	tooBig := arrayLike(t, r, 0, nil)
	mustSet(t, r, tooBig, "length", NumberValue(4294967296))
	_, err = r.Call(mapFn, ObjectValue(tooBig), []Value{sum})
	assert.EqualError(t, err, "RangeError: Invalid array length")
}

func TestArraySort(t *testing.T) {
	r := NewRealm()
	sorted := func(v Value, args ...Value) []any {
		res := jsCall(t, r, v, "sort", args...)
		assert.Same(t, v.AsObject(), res.AsObject(), "sort returns this")
		return r.ToGo(res).([]any)
	}
	assert.Equal(t, []any{int64(1), int64(10), int64(9)}, sorted(mkArray(t, r, 10, 9, 1)), "default order is by code units")
	assert.Equal(t, []any{"B", "a", "b", "á"}, sorted(mkArray(t, r, "b", "á", "B", "a")))
	assert.Equal(t, []any{int64(1), int64(9), int64(10)}, sorted(mkArray(t, r, 10, 9, 1), nativeFn(r, func(_ Value, args []Value) (Value, error) {
		return NumberValue(args[0].AsNumber() - args[1].AsNumber()), nil
	})))
	assert.Equal(t, []any{int64(10), int64(9), int64(1)}, sorted(mkArray(t, r, 9, 1, 10), nativeFn(r, func(_ Value, args []Value) (Value, error) {
		return str(NumberToGoString(args[1].AsNumber() - args[0].AsNumber())), nil
	})), "comparator results are coerced with ToNumber")
	// undefined last, holes removed to the end (length unchanged).
	h := holeArray(r, 6, map[uint32]Value{0: IntValue(3), 1: Undefined(), 3: IntValue(1), 5: str("b")})
	res := jsCall(t, r, h, "sort")
	assert.Equal(t, uint32(6), res.AsObject().ArrayLength())
	assert.Equal(t, []any{int64(1), int64(3), "b", nil, nil, nil}, r.ToGo(res))
	assert.True(t, hasIdx(res, 3), "undefined is a real element")
	assert.False(t, hasIdx(res, 4), "holes moved past the undefineds")
	assert.False(t, hasIdx(res, 5))
	// Stability.
	type kv struct {
		k int
		v string
	}
	var items []Value
	for _, e := range []kv{{2, "a"}, {1, "b"}, {2, "c"}, {1, "d"}, {0, "e"}, {2, "f"}} {
		o := r.NewObject()
		mustSet(t, r, o, "k", IntValue(e.k))
		mustSet(t, r, o, "v", str(e.v))
		items = append(items, ObjectValue(o))
	}
	byK := nativeFn(r, func(_ Value, args []Value) (Value, error) {
		a, _ := args[0].AsObject().GetProp(r, key(r, "k"))
		b, _ := args[1].AsObject().GetProp(r, key(r, "k"))
		return NumberValue(a.AsNumber() - b.AsNumber()), nil
	})
	got := jsCall(t, r, ObjectValue(r.NewArrayFromSlice(items)), "sort", byK)
	var order []string
	for _, e := range r.ToGo(got).([]any) {
		order = append(order, e.(map[string]any)["v"].(string))
	}
	assert.Equal(t, []string{"e", "b", "d", "a", "c", "f"}, order)
	// A large reversed array with duplicates sorts correctly and stably.
	big := make([]Value, 5000)
	for i := range big {
		big[i] = IntValue((5000 - i) % 97)
	}
	bs := jsCall(t, r, ObjectValue(r.NewArrayFromSlice(big)), "sort", nativeFn(r, func(_ Value, args []Value) (Value, error) {
		return NumberValue(args[0].AsNumber() - args[1].AsNumber()), nil
	}))
	prev := -1.0
	for _, v := range bs.AsObject().Elements() {
		require.GreaterOrEqual(t, v.AsNumber(), prev)
		prev = v.AsNumber()
	}
	dflt := jsCall(t, r, ObjectValue(r.NewArrayFromSlice(append([]Value(nil), big...))), "sort")
	prevS := ""
	for _, v := range dflt.AsObject().Elements() {
		s := NumberToGoString(v.AsNumber())
		require.GreaterOrEqual(t, s, prevS)
		prevS = s
	}
	// Comparator exceptions propagate immediately; NaN/inconsistent results are tolerated.
	calls := 0
	err := jsCallErr(t, r, mkArray(t, r, 3, 2, 1), "sort", nativeFn(r, func(Value, []Value) (Value, error) {
		calls++
		return Undefined(), r.TypeError("cmp")
	}))
	assert.EqualError(t, err, "TypeError: cmp")
	assert.Equal(t, 1, calls)
	assert.Len(t, sorted(mkArray(t, r, 3, 2, 1), nativeFn(r, func(Value, []Value) (Value, error) { return NaN(), nil })), 3)
	// Comparator validation precedes this coercion; array-likes work.
	sortFn := jsGet(t, r, ObjectValue(r.ArrayPrototype), "sort")
	_, err = r.Call(sortFn, Undefined(), []Value{IntValue(1)})
	assert.EqualError(t, err, "TypeError: The comparison function must be either a function or undefined")
	_, err = r.Call(sortFn, Undefined(), nil)
	assert.EqualError(t, err, "TypeError: Cannot convert undefined or null to object")
	al := arrayLike(t, r, 4, map[string]Value{"0": str("d"), "1": str("b"), "3": str("a")})
	res, err = r.Call(sortFn, ObjectValue(al), nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"0": "a", "1": "b", "2": "d", "length": int64(4)}, r.ToGo(res))
	assert.False(t, al.HasOwnProperty(IndexKey(3)))
	// ToString of elements happens once each and its errors propagate.
	assert.ErrorContains(t, jsCallErr(t, r, jsArray(r, ObjectValue(r.NewObjectWithProto(nil)), IntValue(1)), "sort"), "Cannot convert object to primitive value")
	frozen := mkArray(t, r, 2, 1)
	frozen.AsObject().Freeze(r)
	assert.ErrorContains(t, jsCallErr(t, r, frozen, "sort"), "read only property '0'")
	assert.Equal(t, []any{int64(1)}, sorted(mkArray(t, r, 1)))
	assert.Equal(t, []any{}, sorted(jsArray(r)))
}

func TestArrayReverseFillAtFlat(t *testing.T) {
	r := NewRealm()
	a := holeArray(r, 4, map[uint32]Value{0: IntValue(1), 2: IntValue(3), 3: IntValue(4)})
	assert.Same(t, a.AsObject(), jsCall(t, r, a, "reverse").AsObject())
	assert.Equal(t, []any{int64(4), int64(3), nil, int64(1)}, r.ToGo(a))
	assert.False(t, hasIdx(a, 2))
	assert.Equal(t, []any{int64(2), int64(1)}, r.ToGo(jsCall(t, r, mkArray(t, r, 1, 2), "reverse")))
	assert.Equal(t, []any{int64(1)}, r.ToGo(jsCall(t, r, mkArray(t, r, 1), "reverse")))
	al := arrayLike(t, r, 3, map[string]Value{"0": str("x"), "1": str("y")})
	res, err := r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "reverse"), ObjectValue(al), nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"1": "y", "2": "x", "length": int64(3)}, r.ToGo(res))
	assert.False(t, al.HasOwnProperty(IndexKey(0)))
	frozen := mkArray(t, r, 1, 2)
	frozen.AsObject().Freeze(r)
	assert.ErrorContains(t, jsCallErr(t, r, frozen, "reverse"), "read only property")

	f := mkArray(t, r, 1, 2, 3, 4)
	assert.Equal(t, []any{int64(1), int64(0), int64(0), int64(4)}, r.ToGo(jsCall(t, r, f, "fill", IntValue(0), IntValue(1), IntValue(3))))
	assert.Equal(t, []any{int64(1), int64(0), int64(9), int64(9)}, r.ToGo(jsCall(t, r, f, "fill", IntValue(9), IntValue(-2))))
	assert.Equal(t, []any{"z", "z", "z", "z"}, r.ToGo(jsCall(t, r, f, "fill", str("z"))))
	assert.Equal(t, []any{"z", "z", "z", "z"}, r.ToGo(jsCall(t, r, f, "fill", str("q"), IntValue(3), IntValue(1))))
	hf := r.NewArrayLen(3)
	jsCall(t, r, ObjectValue(hf), "fill", IntValue(7))
	assert.Equal(t, []any{int64(7), int64(7), int64(7)}, r.ToGo(ObjectValue(hf)))
	assert.True(t, hf.IsDenseArray())
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "fill"), ObjectValue(arrayLike(t, r, 2, nil)), []Value{True()})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"0": true, "1": true, "length": int64(2)}, r.ToGo(res))
	assert.ErrorContains(t, jsCallErr(t, r, frozen, "fill", IntValue(0)), "read only property '0'")

	at := mkArray(t, r, "a", "b", "c")
	assert.Equal(t, "a", jsString(t, jsCall(t, r, at, "at", IntValue(0))))
	assert.Equal(t, "c", jsString(t, jsCall(t, r, at, "at", IntValue(-1))))
	assert.Equal(t, "b", jsString(t, jsCall(t, r, at, "at", NumberValue(1.7))))
	assert.Equal(t, "a", jsString(t, jsCall(t, r, at, "at")))
	assert.True(t, jsCall(t, r, at, "at", IntValue(3)).IsUndefined())
	assert.True(t, jsCall(t, r, at, "at", IntValue(-4)).IsUndefined())
	assert.True(t, jsCall(t, r, at, "at", NumberValue(posInf)).IsUndefined())
	assert.True(t, jsCall(t, r, at, "at", NumberValue(negInf)).IsUndefined())
	res, err = r.Call(jsGet(t, r, ObjectValue(r.ArrayPrototype), "at"), str("xyz"), []Value{IntValue(-1)})
	require.NoError(t, err)
	assert.Equal(t, "z", jsString(t, res))

	nested := mkArray(t, r, 1, []any{2, []any{3, []any{4}}}, []any{}, 5)
	assert.Equal(t, []any{int64(1), int64(2), []any{int64(3), []any{int64(4)}}, int64(5)}, r.ToGo(jsCall(t, r, nested, "flat")))
	assert.Equal(t, []any{int64(1), int64(2), int64(3), []any{int64(4)}, int64(5)}, r.ToGo(jsCall(t, r, nested, "flat", IntValue(2))))
	assert.Equal(t, []any{int64(1), int64(2), int64(3), int64(4), int64(5)}, r.ToGo(jsCall(t, r, nested, "flat", NumberValue(posInf))))
	assert.Equal(t, 4, len(r.ToGo(jsCall(t, r, nested, "flat", IntValue(0))).([]any)))
	assert.Equal(t, 4, len(r.ToGo(jsCall(t, r, nested, "flat", IntValue(-3))).([]any)))
	assert.Equal(t, []any{int64(1), int64(3)}, r.ToGo(jsCall(t, r, holeArray(r, 3, map[uint32]Value{0: IntValue(1), 2: IntValue(3)}), "flat")), "holes are dropped")
	inner := holeArray(r, 2, map[uint32]Value{1: str("i")})
	assert.Equal(t, []any{"i"}, r.ToGo(jsCall(t, r, jsArray(r, inner), "flat")))
	alObj := ObjectValue(arrayLike(t, r, 1, map[string]Value{"0": str("al")}))
	assert.Equal(t, []any{map[string]any{"0": "al", "length": int64(1)}}, r.ToGo(jsCall(t, r, jsArray(r, alObj), "flat")), "array-likes are not flattened")
	cyc := r.NewArray()
	cyc.Push(r, ObjectValue(cyc))
	assert.EqualError(t, jsCallErr(t, r, ObjectValue(cyc), "flat", NumberValue(posInf)), "RangeError: Maximum call stack size exceeded")
	assert.Equal(t, 0, r.CallDepth())
	var log []string
	fm := jsCall(t, r, mkArray(t, r, 1, 2, 3), "flatMap", nativeFn(r, func(this Value, args []Value) (Value, error) {
		log = append(log, this.String()+":"+args[1].String())
		n := args[0].AsNumber()
		if n == 2 {
			return mkArray(t, r, n, []any{n * 10}), nil
		}
		return mkArray(t, r, n, n), nil
	}), str("T"))
	assert.Equal(t, []any{int64(1), int64(1), int64(2), []any{int64(20)}, int64(3), int64(3)}, r.ToGo(fm), "flatMap flattens one level")
	assert.Equal(t, []string{"T:0", "T:1", "T:2"}, log)
	assert.Equal(t, []any{int64(1)}, r.ToGo(jsCall(t, r, mkArray(t, r, 1), "flatMap", nativeFn(r, func(_ Value, args []Value) (Value, error) { return args[0], nil }))))
	assert.EqualError(t, jsCallErr(t, r, mkArray(t, r, 1), "flatMap"), "TypeError: flatMap mapper function is not callable")
	assert.EqualError(t, jsCallErr(t, r, mkArray(t, r, 1), "flatMap", throwingFn(r, "fm")), "TypeError: fm")
}

// TestArrayCallbacksEnterFrames covers the callbacks of the iteration
// methods and sort, which CallObject now enters through enterFrame
// directly: every kind of callee, holes and length changes during the
// iteration, a callback that throws, re-enters or is interrupted, and the
// comparator's conversions of a result that is not a Number.
func TestArrayCallbacksEnterFrames(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"callees", `const a = [3, -1, 2]; const bound = function (x) { return this.k * x; }.bind({k: 10}); const p = new Proxy(x => x + 1, {}); return [a.map(x => x * 2), a.map(Math.abs), a.map(bound), a.map(p), a.map(function (x) { return this + x; }, "s"), a.map(function (x, i, o) { return arguments.length + i + (o === a ? 1 : 0); }), a.map((...r) => r.length)].join("|")`, "6,-2,4|3,1,2|30,-10,20|4,0,3|s3,s-1,s2|4,5,6|3,3,3"},
		{"class constructor", `return [1].map(class C {})`, "!TypeError"},
		{"holes and growth", `const a = [1, , 3]; const seen = []; a.forEach((x, i) => { seen.push(i + ":" + x); if (i === 0) a.push(9); }); const b = [1, 2, 3, 4]; const m = b.map((x, i) => { if (i === 1) b.length = 2; return x * 10; }); return [seen.join(" "), m.length, 2 in m, m.join(" ")].join("|")`, "0:1 2:3|4|false|10 20  "},
		{"filter reduce find", `const a = [5, 1, 4, , 2]; return [a.filter(x => x > 1).join(), a.reduce((s, x) => s + x), a.reduce((s, x) => s + x, 100), a.reduceRight((s, x) => s + "" + x, ""), a.find(x => x < 3), a.findIndex(x => x === 4), a.findLast(x => x > 3), a.findLastIndex(x => x === undefined), a.some(x => x === 2), a.every(x => x > 0)].join("|")`, "5,4,2|12|112|2415|1|2|4|3|true|true"},
		{"throw", `const a = [1, 2, 3]; let n = 0; let e; try { a.map(x => { n++; if (x === 2) throw new Error("at " + x); return x; }); } catch (x) { e = x.message; } return [e, n, a.map(x => x + 1).join()].join("|")`, "at 2|2|2,3,4"},
		{"re-entry", `const a = [1, 2]; return a.map(x => a.map(y => a.filter(z => z <= y).length * x).join("+")).join(",")`, "1+2,2+4"},
		{"re-entry past old limit", `function f(n) { return n === 0 ? 0 : [n].map(x => f(x - 1) + 1)[0]; } let e; try { f(1000); } catch (x) { e = x.name; } return [f(100), e].join()`, "100,"},
		{"deep re-entry", `function f(n) { return n === 0 ? 0 : [n].map(x => f(x - 1) + 1)[0]; } let e; try { f(5000); } catch (x) { e = x.name; } return [f(100), e].join()`, "100,RangeError"},
		{"sort", `const a = [3, 1, 2, 10]; return [a.slice().sort((p, q) => p - q), a.slice().sort((p, q) => String(q - p)), a.slice().sort((p, q) => ({valueOf() { return p - q; }})), a.slice().sort(() => NaN), a.slice().sort((p, q) => q > p ? 1 : -0), a.slice().sort((p, q) => (p - q) * Infinity), a.slice().sort(Math.max === 1 ? null : undefined)].join("|")`, "1,2,3,10|10,3,2,1|1,2,3,10|3,1,2,10|10,3,2,1|1,2,3,10|1,10,2,3"},
		{"sort comparator throws", `const a = [3, 1, 2]; let e; try { a.sort((p, q) => { throw new Error("cmp"); }); } catch (x) { e = x.message; } return [e, a.join()].join("|")`, "cmp|3,1,2"},
		{"sort comparator symbol", `return [2, 1].sort(() => Symbol())`, "!TypeError"},
		{"sort comparator bigint", `return [2, 1].sort(() => 1n)`, "!TypeError"},
		{"sort holes", `const a = [3, , undefined, 1]; a.sort((p, q) => p - q); return [a.length, a[0], a[1], a[2], 3 in a].join()`, "4,1,3,,false"},
	})
}

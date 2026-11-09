package engine

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func str(s string) Value { return StringValue(FromGoString(s)) }

func TestEqualityTables(t *testing.T) {
	r := NewRealm()
	o1 := ObjectValue(r.NewObject())
	o2 := ObjectValue(r.NewObject())
	b1, _ := NewBigIntFromDecimal("10")
	b2, _ := NewBigIntFromDecimal("10")
	negZero := NumberValue(math.Copysign(0, -1))
	cases := []struct {
		name          string
		a, b          Value
		strict, loose bool
		same, sameZ   bool
	}{
		{"numbers", IntValue(1), IntValue(1), true, true, true, true},
		{"nan", NaN(), NaN(), false, false, true, true},
		{"zeros", IntValue(0), negZero, true, true, false, true},
		{"strings", str("abc"), str("abc"), true, true, true, true},
		{"strings differ", str("abc"), str("abd"), false, false, false, false},
		{"same object", o1, o1, true, true, true, true},
		{"different objects", o1, o2, false, false, false, false},
		{"undefined null", Undefined(), Null(), false, true, false, false},
		{"undefined undefined", Undefined(), Undefined(), true, true, true, true},
		{"null null", Null(), Null(), true, true, true, true},
		{"bool bool", True(), True(), true, true, true, true},
		{"bool bool differ", True(), False(), false, false, false, false},
		{"number string", IntValue(1), str("1"), false, true, false, false},
		{"string number", str("1.0"), IntValue(1), false, true, false, false},
		{"number string nan", IntValue(1), str("x"), false, false, false, false},
		{"bool number", True(), IntValue(1), false, true, false, false},
		{"bool string", False(), str("0"), false, true, false, false},
		{"bigint bigint", BigIntValue(b1), BigIntValue(b2), true, true, true, true},
		{"bigint number", BigIntValue(b1), IntValue(10), false, true, false, false},
		{"bigint string", BigIntValue(b1), str("10"), false, true, false, false},
		{"bigint bad string", BigIntValue(b1), str("1x"), false, false, false, false},
		{"null number", Null(), IntValue(0), false, false, false, false},
		{"undefined bool", Undefined(), False(), false, false, false, false},
		{"symbol", SymbolValue(SymIterator), SymbolValue(SymIterator), true, true, true, true},
		{"symbols differ", SymbolValue(SymIterator), SymbolValue(SymToPrimitive), false, false, false, false},
		{"object null", o1, Null(), false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.strict, StrictEquals(c.a, c.b), "strict")
			assert.Equal(t, c.strict, StrictEquals(c.b, c.a), "strict reversed")
			loose, err := r.LooseEquals(c.a, c.b)
			require.NoError(t, err)
			assert.Equal(t, c.loose, loose, "loose")
			loose, err = r.LooseEquals(c.b, c.a)
			require.NoError(t, err)
			assert.Equal(t, c.loose, loose, "loose reversed")
			assert.Equal(t, c.same, SameValue(c.a, c.b), "SameValue")
			assert.Equal(t, c.sameZ, SameValueZero(c.a, c.b), "SameValueZero")
		})
	}
}

func TestLooseEqualsObjectToPrimitive(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	o.DefineOwnDataFast(r, StringKey(AtomValueOf), ObjectValue(r.NewNativeFunction(AtomValueOf, 0,
		func(r *Realm, this Value, args []Value) (Value, error) { return IntValue(5), nil })), attrHidden)
	eq, err := r.LooseEquals(ObjectValue(o), IntValue(5))
	require.NoError(t, err)
	assert.True(t, eq)
	eq, err = r.LooseEquals(str("5"), ObjectValue(o))
	require.NoError(t, err)
	assert.True(t, eq)
	// [] == "" through Array.prototype.toString (join).
	eq, err = r.LooseEquals(ObjectValue(r.NewArray()), str(""))
	require.NoError(t, err)
	assert.True(t, eq)
	eq, err = r.LooseEquals(ObjectValue(r.NewArray(IntValue(1), IntValue(2))), str("1,2"))
	require.NoError(t, err)
	assert.True(t, eq)
}

func TestToPrimitiveAndToString(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	s, err := r.ToString(ObjectValue(o))
	require.NoError(t, err)
	assert.Equal(t, "[object Object]", s.GoString())
	n, err := r.ToNumber(ObjectValue(o))
	require.NoError(t, err)
	assert.True(t, math.IsNaN(n))

	// Custom toString / valueOf order by hint.
	calls := []string{}
	o.DefineOwnDataFast(r, StringKey(AtomToString), ObjectValue(r.NewNativeFunction(AtomToString, 0,
		func(r *Realm, this Value, args []Value) (Value, error) {
			calls = append(calls, "toString")
			return str("str"), nil
		})), attrHidden)
	o.DefineOwnDataFast(r, StringKey(AtomValueOf), ObjectValue(r.NewNativeFunction(AtomValueOf, 0,
		func(r *Realm, this Value, args []Value) (Value, error) {
			calls = append(calls, "valueOf")
			return IntValue(3), nil
		})), attrHidden)
	s, err = r.ToString(ObjectValue(o))
	require.NoError(t, err)
	assert.Equal(t, "str", s.GoString())
	n, err = r.ToNumber(ObjectValue(o))
	require.NoError(t, err)
	assert.Equal(t, 3.0, n)
	assert.Equal(t, []string{"toString", "valueOf"}, calls)

	// valueOf returning an object falls through to toString.
	p := r.NewObject()
	p.DefineOwnDataFast(r, StringKey(AtomValueOf), ObjectValue(r.NewNativeFunction(AtomValueOf, 0,
		func(r *Realm, this Value, args []Value) (Value, error) { return this, nil })), attrHidden)
	n, err = r.ToNumber(ObjectValue(p))
	require.NoError(t, err)
	assert.True(t, math.IsNaN(n))
	// Neither callable => TypeError.
	q := r.NewObjectWithProto(nil)
	_, err = r.ToNumber(ObjectValue(q))
	assert.EqualError(t, err, "TypeError: Cannot convert object to primitive value")

	// Primitive conversions.
	for _, c := range []struct {
		v    Value
		want string
	}{
		{Undefined(), "undefined"}, {Null(), "null"}, {True(), "true"}, {False(), "false"},
		{NumberValue(1.5), "1.5"}, {str("x"), "x"},
	} {
		s, err := r.ToString(c.v)
		require.NoError(t, err)
		assert.Equal(t, c.want, s.GoString())
	}
	_, err = r.ToString(SymbolValue(SymIterator))
	assert.ErrorContains(t, err, "Cannot convert a Symbol value to a string")
	b, _ := NewBigIntFromDecimal("99")
	s, _ = r.ToString(BigIntValue(b))
	assert.Equal(t, "99", s.GoString())
	_, err = r.ToNumber(BigIntValue(b))
	assert.ErrorContains(t, err, "Cannot convert a BigInt value to a number")
	n, _ = r.ToNumber(Undefined())
	assert.True(t, math.IsNaN(n))
	n, _ = r.ToNumber(Null())
	assert.Equal(t, 0.0, n)
	n, _ = r.ToNumber(True())
	assert.Equal(t, 1.0, n)
	n, _ = r.ToNumber(str(" 0x10 "))
	assert.Equal(t, 16.0, n)
}

func TestToBoolean(t *testing.T) {
	r := NewRealm()
	b, _ := NewBigIntFromDecimal("0")
	assert.False(t, ToBoolean(Undefined()))
	assert.False(t, ToBoolean(Null()))
	assert.False(t, ToBoolean(False()))
	assert.False(t, ToBoolean(IntValue(0)))
	assert.False(t, ToBoolean(NumberValue(math.Copysign(0, -1))))
	assert.False(t, ToBoolean(NaN()))
	assert.False(t, ToBoolean(str("")))
	assert.False(t, ToBoolean(BigIntValue(b)))
	assert.True(t, ToBoolean(True()))
	assert.True(t, ToBoolean(IntValue(-1)))
	assert.True(t, ToBoolean(str("0")))
	assert.True(t, ToBoolean(ObjectValue(r.NewObject())))
	assert.True(t, ToBoolean(SymbolValue(SymIterator)))
}

func TestToObjectAndWrappers(t *testing.T) {
	r := NewRealm()
	_, err := r.ToObject(Undefined())
	assert.EqualError(t, err, "TypeError: Cannot convert undefined or null to object")
	_, err = r.ToObject(Null())
	assert.Error(t, err)
	n, err := r.ToObject(NumberValue(2.5))
	require.NoError(t, err)
	assert.Equal(t, ClassNumber, n.Class())
	assert.Same(t, r.NumberPrototype, n.Proto())
	pv, _ := n.PrimitiveValue()
	assert.Equal(t, NumberValue(2.5), pv)
	bo, _ := r.ToObject(True())
	assert.Equal(t, ClassBoolean, bo.Class())
	so, _ := r.ToObject(str("ab"))
	assert.Equal(t, ClassString, so.Class())
	oo, _ := r.ToObject(ObjectValue(n))
	assert.Same(t, n, oo)

	// Wrapper methods work through GetV without allocating a wrapper.
	v, err := r.GetV(NumberValue(255), StringKey(AtomToString))
	require.NoError(t, err)
	res, err := r.Call(v, NumberValue(255), []Value{IntValue(16)})
	require.NoError(t, err)
	assert.Equal(t, "ff", res.AsString().GoString())
	_, err = r.Call(v, NumberValue(255), []Value{IntValue(37)})
	assert.EqualError(t, err, "RangeError: toString() radix must be between 2 and 36")
	_, err = r.Call(v, str("x"), nil)
	assert.ErrorContains(t, err, "requires that 'this' be a Number")
	v, _ = r.GetV(str("héllo"), lengthKey)
	assert.Equal(t, IntValue(5), v)
	v, _ = r.GetV(str("héllo"), IndexKey(1))
	assert.Equal(t, "é", v.AsString().GoString())
	v, _ = r.GetV(str("héllo"), IndexKey(9))
	assert.True(t, v.IsUndefined())
	_, err = r.GetV(Undefined(), key(r, "foo"))
	assert.EqualError(t, err, "TypeError: Cannot read property 'foo' of undefined")
	_, err = r.GetV(Null(), IndexKey(0))
	assert.EqualError(t, err, "TypeError: Cannot read property '0' of null")
	assert.EqualError(t, r.SetV(Undefined(), key(r, "x"), IntValue(1)), "TypeError: Cannot set property 'x' of undefined")
	assert.EqualError(t, r.SetV(str("abc"), key(r, "x"), IntValue(1)), "TypeError: Cannot create property 'x' on \"abc\"")
	// Boolean/String prototypes.
	v, _ = r.GetV(True(), StringKey(AtomToString))
	res, _ = r.Call(v, True(), nil)
	assert.Equal(t, "true", res.AsString().GoString())
	v, _ = r.GetV(str("s"), StringKey(AtomValueOf))
	res, _ = r.Call(v, str("s"), nil)
	assert.Equal(t, "s", res.AsString().GoString())
	res, _ = r.Call(v, ObjectValue(so), nil)
	assert.Equal(t, "ab", res.AsString().GoString())
	_, err = r.Call(v, IntValue(1), nil)
	assert.ErrorContains(t, err, "requires that 'this' be a String")
}

func TestRelationalComparison(t *testing.T) {
	r := NewRealm()
	b5, _ := NewBigIntFromDecimal("5")
	cases := []struct {
		name string
		a, b Value
		lt   int8
	}{
		{"numbers", IntValue(1), IntValue(2), CompareTrue},
		{"numbers eq", IntValue(2), IntValue(2), CompareFalse},
		{"nan", NaN(), IntValue(2), CompareUndefined},
		{"strings", str("a"), str("b"), CompareTrue},
		{"strings units", str("Z"), str("a"), CompareTrue},
		{"string number", str("10"), IntValue(9), CompareFalse},
		{"string number 2", str("9"), IntValue(10), CompareTrue},
		{"string nan", str("x"), IntValue(1), CompareUndefined},
		{"bool", False(), True(), CompareTrue},
		{"null undefined", Null(), Undefined(), CompareUndefined},
		{"null zero", Null(), IntValue(1), CompareTrue},
		{"bigint number", BigIntValue(b5), NumberValue(5.5), CompareTrue},
		{"number bigint", NumberValue(4.5), BigIntValue(b5), CompareTrue},
		{"bigint string", BigIntValue(b5), str("6"), CompareTrue},
		{"bigint bad string", BigIntValue(b5), str("x"), CompareUndefined},
		{"bigint inf", BigIntValue(b5), NumberValue(math.Inf(1)), CompareTrue},
		{"bigint -inf", BigIntValue(b5), NumberValue(math.Inf(-1)), CompareFalse},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.IsLessThan(c.a, c.b, true)
			require.NoError(t, err)
			assert.Equal(t, c.lt, got)
		})
	}
	lt, _ := r.LessThan(IntValue(1), IntValue(2))
	assert.True(t, lt)
	le, _ := r.LessThanOrEqual(IntValue(2), IntValue(2))
	assert.True(t, le)
	le, _ = r.LessThanOrEqual(NaN(), IntValue(2))
	assert.False(t, le)
	gt, _ := r.GreaterThan(str("b"), str("a"))
	assert.True(t, gt)
	ge, _ := r.GreaterThanOrEqual(IntValue(2), IntValue(3))
	assert.False(t, ge)
	ge, _ = r.GreaterThanOrEqual(IntValue(3), IntValue(3))
	assert.True(t, ge)

	// Evaluation order: left operand converted first.
	order := []string{}
	mk := func(name string) Value {
		o := r.NewObject()
		o.DefineOwnDataFast(r, StringKey(AtomValueOf), ObjectValue(r.NewNativeFunction(AtomValueOf, 0,
			func(r *Realm, this Value, args []Value) (Value, error) {
				order = append(order, name)
				return IntValue(1), nil
			})), attrHidden)
		return ObjectValue(o)
	}
	_, err := r.GreaterThan(mk("left"), mk("right"))
	require.NoError(t, err)
	assert.Equal(t, []string{"left", "right"}, order)
}

func TestAddOperator(t *testing.T) {
	r := NewRealm()
	cases := []struct {
		name string
		a, b Value
		want Value
	}{
		{"numbers", IntValue(1), IntValue(2), IntValue(3)},
		{"strings", str("a"), str("b"), str("ab")},
		{"string number", str("a"), IntValue(1), str("a1")},
		{"number string", NumberValue(1.5), str("a"), str("1.5a")},
		{"undefined number", Undefined(), IntValue(1), NaN()},
		{"null number", Null(), IntValue(1), IntValue(1)},
		{"bool bool", True(), True(), IntValue(2)},
		{"string undefined", str("x"), Undefined(), str("xundefined")},
		{"string null", str("x"), Null(), str("xnull")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.Add(c.a, c.b)
			require.NoError(t, err)
			if c.want.IsString() {
				assert.Equal(t, c.want.AsString().GoString(), got.AsString().GoString())
				return
			}
			assert.True(t, SameValue(c.want, got), "got %v", got)
		})
	}
	got, err := r.Add(ObjectValue(r.NewObject()), str("!"))
	require.NoError(t, err)
	assert.Equal(t, "[object Object]!", got.AsString().GoString())
	b, _ := NewBigIntFromDecimal("1")
	got, err = r.Add(BigIntValue(b), BigIntValue(b))
	require.NoError(t, err)
	assert.Equal(t, "2", got.AsBigInt().ToString())
	_, err = r.Add(BigIntValue(b), IntValue(1))
	assert.EqualError(t, err, "TypeError: Cannot mix BigInt and other types, use explicit conversions")
	got, err = r.Add(BigIntValue(b), str("n"))
	require.NoError(t, err)
	assert.Equal(t, "1n", got.AsString().GoString())
}

func TestTypeOf(t *testing.T) {
	r := NewRealm()
	b, _ := NewBigIntFromDecimal("1")
	cases := []struct {
		v    Value
		want string
	}{
		{Undefined(), "undefined"}, {Null(), "object"}, {True(), "boolean"}, {IntValue(1), "number"},
		{str("s"), "string"}, {SymbolValue(SymIterator), "symbol"}, {BigIntValue(b), "bigint"},
		{ObjectValue(r.NewObject()), "object"}, {ObjectValue(r.ObjectCtor), "function"},
		{ObjectValue(r.NewArray()), "object"},
	}
	for _, c := range cases {
		got := TypeOf(c.v)
		assert.Equal(t, c.want, got.GoString())
		assert.True(t, got.IsInterned())
	}
}

func TestInstanceOfAndIn(t *testing.T) {
	r := NewRealm()
	arr := ObjectValue(r.NewArray())
	ok, err := r.InstanceOf(arr, ObjectValue(r.ArrayCtor))
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = r.InstanceOf(arr, ObjectValue(r.ObjectCtor))
	assert.True(t, ok)
	ok, _ = r.InstanceOf(arr, ObjectValue(r.StringCtor))
	assert.False(t, ok)
	ok, _ = r.InstanceOf(IntValue(1), ObjectValue(r.NumberCtor))
	assert.False(t, ok)
	_, err = r.InstanceOf(arr, IntValue(1))
	assert.EqualError(t, err, "TypeError: Right-hand side of 'instanceof' is not an object")
	_, err = r.InstanceOf(arr, ObjectValue(r.NewObject()))
	assert.EqualError(t, err, "TypeError: Right-hand side of 'instanceof' is not callable")
	te := ObjectValue(r.NewError(KindTypeError, "x"))
	ok, _ = r.InstanceOf(te, ObjectValue(r.ErrorConstructorFor(KindError)))
	assert.True(t, ok)
	ok, _ = r.InstanceOf(te, ObjectValue(r.ErrorConstructorFor(KindTypeError)))
	assert.True(t, ok)
	ok, _ = r.InstanceOf(te, ObjectValue(r.ErrorConstructorFor(KindRangeError)))
	assert.False(t, ok)
	// Bound function delegates to target.
	bound, err := r.NewBoundFunction(r.ArrayCtor, Undefined(), nil)
	require.NoError(t, err)
	ok, _ = r.InstanceOf(arr, ObjectValue(bound))
	assert.True(t, ok)
	// Function with non-object prototype.
	f := r.NewNativeFunction(AtomEmpty, 0, func(*Realm, Value, []Value) (Value, error) { return Undefined(), nil })
	f.DefineOwnDataFast(r, StringKey(AtomPrototype), IntValue(1), attrHidden)
	_, err = r.InstanceOf(arr, ObjectValue(f))
	assert.ErrorContains(t, err, "non-object prototype")

	o := r.NewObject()
	mustSet(t, r, o, "x", IntValue(1))
	has, err := r.HasPropertyIn(str("x"), ObjectValue(o))
	require.NoError(t, err)
	assert.True(t, has)
	has, _ = r.HasPropertyIn(str("toString"), ObjectValue(o))
	assert.True(t, has, "inherited")
	has, _ = r.HasPropertyIn(str("y"), ObjectValue(o))
	assert.False(t, has)
	has, _ = r.HasPropertyIn(IntValue(0), arr)
	assert.False(t, has)
	_, err = r.HasPropertyIn(str("x"), str("abc"))
	assert.EqualError(t, err, "TypeError: Cannot use 'in' operator to search for 'x' in \"abc\"")
}

func TestToPropertyKey(t *testing.T) {
	r := NewRealm()
	k, err := r.ToPropertyKey(IntValue(3))
	require.NoError(t, err)
	assert.True(t, k.IsIndex())
	assert.Equal(t, uint32(3), k.Index())
	k, _ = r.ToPropertyKey(str("3"))
	assert.True(t, k.IsIndex())
	k, _ = r.ToPropertyKey(str("03"))
	assert.True(t, k.IsString())
	assert.Equal(t, "03", k.GoString())
	k, _ = r.ToPropertyKey(NumberValue(1.5))
	assert.True(t, k.IsString())
	assert.Equal(t, "1.5", k.GoString())
	k, _ = r.ToPropertyKey(NumberValue(4294967295))
	assert.True(t, k.IsString())
	k, _ = r.ToPropertyKey(NumberValue(math.Copysign(0, -1)))
	assert.Equal(t, IndexKey(0), k)
	k, _ = r.ToPropertyKey(Undefined())
	assert.Equal(t, StringKey(AtomUndefined), k)
	k, _ = r.ToPropertyKey(SymbolValue(SymIterator))
	assert.True(t, k.IsSymbol())
	o := r.NewObject()
	k, err = r.ToPropertyKey(ObjectValue(o))
	require.NoError(t, err)
	assert.Equal(t, "[object Object]", k.GoString())
	// Same content interns to the same key.
	k1, _ := r.ToPropertyKey(str("dynamicName"))
	k2, _ := r.ToPropertyKey(StringValue(concat(FromGoString("dynamic"), FromGoString("Name"))))
	assert.Equal(t, k1, k2)
	assert.Same(t, k1.String(), k2.String())
	// UTF-16 keys are distinct from their U+FFFD folding.
	ku1, _ := r.ToPropertyKey(StringValue(FromUTF16([]uint16{0xD800})))
	ku2, _ := r.ToPropertyKey(StringValue(FromUTF16([]uint16{0xFFFD})))
	assert.NotEqual(t, ku1, ku2)
	assert.Equal(t, "3", IndexKey(3).ToJSString(r).GoString())
	assert.Equal(t, "300", IndexKey(300).ToJSString(r).GoString())
	// Small index strings are atoms and still name the index.
	s3 := IndexKey(3).ToJSString(r)
	require.True(t, s3.IsInterned())
	k, _ = r.ToPropertyKey(StringValue(s3))
	assert.Equal(t, IndexKey(3), k)
	assert.Equal(t, IndexKey(0), r.KeyFromString(r.Intern(FromGoString("0"))))
	assert.True(t, r.KeyFromString(r.Intern(FromGoString("07"))).IsString())
}

func TestToLengthToIndex(t *testing.T) {
	r := NewRealm()
	n, _ := r.ToLength(IntValue(-5))
	assert.Equal(t, int64(0), n)
	n, _ = r.ToLength(NumberValue(1e300))
	assert.Equal(t, int64(maxSafeInteger), n)
	n, _ = r.ToLength(str("12.9"))
	assert.Equal(t, int64(12), n)
	i, err := r.ToIndex(Undefined())
	require.NoError(t, err)
	assert.Equal(t, int64(0), i)
	_, err = r.ToIndex(IntValue(-1))
	assert.EqualError(t, err, "RangeError: Invalid index")
	i32, _ := r.ToInt32(str("0x10"))
	assert.Equal(t, int32(16), i32)
	u32, _ := r.ToUint32(IntValue(-1))
	assert.Equal(t, uint32(4294967295), u32)
	f, _ := r.ToIntegerOrInfinity(str("  7.9 "))
	assert.Equal(t, 7.0, f)
}

func TestCreateListFromArrayLike(t *testing.T) {
	r := NewRealm()
	arr := r.NewArray(IntValue(1), IntValue(2))
	list, err := r.CreateListFromArrayLike(ObjectValue(arr))
	require.NoError(t, err)
	assert.Equal(t, []Value{IntValue(1), IntValue(2)}, list)
	o := r.NewObject()
	mustSet(t, r, o, "length", IntValue(2))
	mustSet(t, r, o, "0", str("a"))
	list, err = r.CreateListFromArrayLike(ObjectValue(o))
	require.NoError(t, err)
	assert.Equal(t, "a", list[0].AsString().GoString())
	assert.True(t, list[1].IsUndefined())
	_, err = r.CreateListFromArrayLike(IntValue(1))
	assert.ErrorContains(t, err, "non-object")
	// Long lists honour interrupts.
	long := r.NewObject()
	mustSet(t, r, long, "length", IntValue(1<<20))
	r.Interrupt("stop")
	_, err = r.CreateListFromArrayLike(ObjectValue(long))
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	r.ClearInterrupt()
	n, err := r.LengthOfArrayLike(o)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	m, err := r.GetMethod(ObjectValue(arr), StringKey(AtomToString))
	require.NoError(t, err)
	assert.True(t, IsCallable(m))
	m, err = r.GetMethod(ObjectValue(arr), key(r, "nope"))
	require.NoError(t, err)
	assert.True(t, m.IsUndefined())
	_, err = r.GetMethod(ObjectValue(arr), lengthKey)
	assert.ErrorContains(t, err, "is not a function")
}

// TestJSMod compares jsMod with math.Mod bit for bit (the sign of a zero
// included) on the int64 fast path's edges and on the cases it leaves to
// math.Mod.
func TestJSMod(t *testing.T) {
	const safe = 1 << 53
	negZero := math.Copysign(0, -1)
	vals := []float64{
		0, negZero, 1, -1, 2, -2, 3, -3, 7, -7, 10, -10, 1000003, -1000003,
		safe, -safe, safe - 1, -(safe - 1), safe + 2, -(safe + 2), 1 << 62, -(1 << 62),
		1 << 63, -(1 << 63), 1e300, -1e300, 0.5, -0.5, 2.5, -2.5, 1e-310, -1e-310,
		math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64,
		math.Inf(1), math.Inf(-1), math.NaN(),
	}
	same := func(t *testing.T, x, y float64) {
		t.Helper()
		got, want := jsMod(x, y), math.Mod(x, y)
		if math.IsNaN(want) {
			assert.True(t, math.IsNaN(got), "%v %% %v = %v, want NaN", x, y, got)
			return
		}
		assert.Equal(t, math.Float64bits(want), math.Float64bits(got), "%v %% %v = %v, want %v", x, y, got, want)
	}
	for _, x := range vals {
		for _, y := range vals {
			same(t, x, y)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 100000 {
		x := float64(rng.Int64N(2*safe+1) - safe)
		y := float64(rng.Int64N(2*safe+1) - safe)
		if rng.IntN(2) == 0 {
			y = float64(rng.Int64N(2001) - 1000)
		}
		same(t, x, y)
	}
	assert.True(t, math.Signbit(jsMod(-7, 7)), "-7 % 7 is -0")
	assert.True(t, math.Signbit(jsMod(-7, -7)), "-7 % -7 is -0")
	assert.True(t, math.Signbit(jsMod(negZero, 3)), "-0 % 3 is -0")
	assert.False(t, math.Signbit(jsMod(7, -7)), "7 % -7 is +0")
	assert.True(t, math.IsNaN(jsMod(3, 0)), "3 % 0 is NaN")
	assert.True(t, math.IsNaN(jsMod(3, negZero)), "3 % -0 is NaN")
}

// TestInterpRemainder runs % through the interpreter: the number case, the
// conversions of arithSlow and the compound assignment, with -0 told apart
// from +0.
func TestInterpRemainder(t *testing.T) {
	f := evalModule(t, `
function tag(v) { return Object.is(v, -0) ? "-0" : String(v); }
export function rem(a, b) { return tag(a % b); }
export function remAssign(a, b) { let x = a; x %= b; return tag(x); }
export function remProp(a, b) { const o = { x: a }; o.x %= b; return tag(o.x); }
export function remSlow() {
  const v = { valueOf() { return -9; } };
  return [tag("7" % 2), tag(v % 3), tag(-9 % "3"), tag(true % 1), tag(null % 5), tag(5 % null)].join(",");
}
export function remBig() { return [10n % 3n, -10n % 3n].join(","); }
`)
	negZero := math.Copysign(0, -1)
	cases := []struct {
		a, b any
		want string
	}{
		{7, 3, "1"}, {-7, 3, "-1"}, {7, -3, "1"}, {-7, -3, "-1"},
		{-7, 7, "-0"}, {7, 7, "0"}, {negZero, 3, "-0"}, {0, 3, "0"},
		{-6, 2, "-0"}, {6, -2, "0"}, {5, 0, "NaN"}, {5, negZero, "NaN"},
		{int64(9007199254740992), 3, "2"}, {int64(-9007199254740992), 3, "-2"},
		{9007199254740993.0, 10, "2"}, {1e17, 7, "5"}, {-1e17, 7, "-5"},
		{5.5, 2, "1.5"}, {-5.5, 2, "-1.5"}, {5, 2.5, "0"}, {-5, 2.5, "-0"},
		{math.Inf(1), 3, "NaN"}, {3, math.Inf(1), "3"}, {-3, math.Inf(-1), "-3"},
		{negZero, math.Inf(1), "-0"}, {math.NaN(), 3, "NaN"}, {3, math.NaN(), "NaN"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, f.call("rem", c.a, c.b), "%v %% %v", c.a, c.b)
		assert.Equal(t, c.want, f.call("remAssign", c.a, c.b), "%v %%= %v", c.a, c.b)
		assert.Equal(t, c.want, f.call("remProp", c.a, c.b), "o.x %%= %v with %v", c.b, c.a)
	}
	assert.Equal(t, "1,-0,-0,0,0,NaN", f.call("remSlow"))
	assert.Equal(t, "1,-1", f.call("remBig"))
}

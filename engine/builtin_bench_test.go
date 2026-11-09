package engine

import "testing"

// Benchmarks for the builtins on the plugin hook path. Each reports
// allocs/op; the targets are allocation-minimal results: no per-element
// allocation beyond what the result requires.

// benchMethod resolves recv[name] once.
func benchMethod(b *testing.B, r *Realm, recv Value, name string) *Object {
	b.Helper()
	fn, err := r.GetV(recv, r.KeyFromGoString(name))
	if err != nil || !IsCallable(fn) {
		b.Fatalf("%s is not callable", name)
	}
	return fn.AsObject()
}

func BenchmarkArrayPush(b *testing.B) {
	r := NewRealm()
	arr := r.NewArrayFromSlice(make([]Value, 0, 1<<16))
	push := benchMethod(b, r, ObjectValue(arr), "push")
	this := ObjectValue(arr)
	args := []Value{IntValue(7)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if arr.ArrayLength() == 1<<16 {
			arr.setElems(arr.elems()[:0])
			arr.setArrayLen(0)
		}
		if _, err := r.CallObject(push, this, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkArrayIncludesSmall(b *testing.B) {
	r := NewRealm()
	arr := r.NewArray(str("gpt-4o"), str("gpt-4o-mini"), str("o1"), str("o3"), IntValue(1), IntValue(2), IntValue(3), IntValue(42))
	includes := benchMethod(b, r, ObjectValue(arr), "includes")
	this := ObjectValue(arr)
	args := []Value{IntValue(42)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(includes, this, args)
		if err != nil || !v.IsTrue() {
			b.Fatal("includes miss")
		}
	}
}

func BenchmarkArrayMap(b *testing.B) {
	r := NewRealm()
	items := make([]Value, 10)
	for i := range items {
		items[i] = IntValue(i)
	}
	arr := r.NewArrayFromSlice(items)
	mapFn := benchMethod(b, r, ObjectValue(arr), "map")
	double := r.NewNativeFunction(AtomEmpty, 1, func(r *Realm, this Value, args []Value) (Value, error) {
		return NumberValue(args[0].AsNumber() * 2), nil
	})
	this := ObjectValue(arr)
	args := []Value{ObjectValue(double)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(mapFn, this, args)
		if err != nil || v.AsObject().ArrayLength() != 10 {
			b.Fatal("map failed")
		}
	}
}

func BenchmarkArrayJoin(b *testing.B) {
	r := NewRealm()
	items := make([]Value, 10)
	for i := range items {
		items[i] = str("segment")
	}
	arr := r.NewArrayFromSlice(items)
	join := benchMethod(b, r, ObjectValue(arr), "join")
	this := ObjectValue(arr)
	args := []Value{str(", ")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(join, this, args)
		if err != nil || v.AsString().Len() != 88 {
			b.Fatal("join failed")
		}
	}
}

func BenchmarkObjectKeys10(b *testing.B) {
	r := NewRealm()
	o := r.NewObject()
	for _, n := range []string{"model", "prompt", "size", "duration", "seed", "quality", "style", "n", "user", "format"} {
		o.DefineOwnDataFast(r, key(r, n), IntValue(1), attrDefault)
	}
	keys := benchMethod(b, r, ObjectValue(r.ObjectCtor), "keys")
	this := ObjectValue(r.ObjectCtor)
	args := []Value{ObjectValue(o)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(keys, this, args)
		if err != nil || v.AsObject().ArrayLength() != 10 {
			b.Fatal("keys failed")
		}
	}
}

func BenchmarkObjectAssign(b *testing.B) {
	r := NewRealm()
	src := r.NewObject()
	for _, n := range []string{"model", "prompt", "size", "duration", "seed"} {
		src.DefineOwnDataFast(r, key(r, n), IntValue(1), attrDefault)
	}
	assign := benchMethod(b, r, ObjectValue(r.ObjectCtor), "assign")
	this := ObjectValue(r.ObjectCtor)
	args := []Value{Undefined(), ObjectValue(src)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		args[0] = ObjectValue(r.NewObject())
		v, err := r.CallObject(assign, this, args)
		if err != nil || v.AsObject().Shape().Count() != 5 {
			b.Fatal("assign failed")
		}
	}
}

func BenchmarkHasOwnPropertyCall(b *testing.B) {
	r := NewRealm()
	o := r.NewObject()
	for _, n := range []string{"model", "prompt", "size", "duration", "seed"} {
		o.DefineOwnDataFast(r, key(r, n), IntValue(1), attrDefault)
	}
	hop := benchMethod(b, r, ObjectValue(o), "hasOwnProperty")
	this := ObjectValue(o)
	args := []Value{str("duration")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(hop, this, args)
		if err != nil || !v.IsTrue() {
			b.Fatal("hasOwnProperty failed")
		}
	}
}

func BenchmarkNewError(b *testing.B) {
	r := NewRealm()
	ctor := ObjectValue(r.ErrorConstructorFor(KindError))
	args := []Value{str("invalid request: missing model")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Construct(ctor, args, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNumberToString(b *testing.B) {
	r := NewRealm()
	toString := benchMethod(b, r, ObjectValue(r.NumberPrototype), "toString")
	this := NumberValue(12345.678)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(toString, this, nil)
		if err != nil || v.AsString().Len() != 9 {
			b.Fatal("toString failed")
		}
	}
}

func BenchmarkNumberToStringInt(b *testing.B) {
	r := NewRealm()
	toString := benchMethod(b, r, ObjectValue(r.NumberPrototype), "toString")
	this := IntValue(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(toString, this, nil)
		if err != nil || v.AsString().Len() != 4 {
			b.Fatal("toString failed")
		}
	}
}

func BenchmarkNumberToFixed(b *testing.B) {
	r := NewRealm()
	toFixed := benchMethod(b, r, ObjectValue(r.NumberPrototype), "toFixed")
	this := NumberValue(1234.5678)
	args := []Value{IntValue(2)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(toFixed, this, args)
		if err != nil || v.AsString().Len() != 7 {
			b.Fatal("toFixed failed")
		}
	}
}

func BenchmarkParseInt(b *testing.B) {
	r := NewRealm()
	parseInt := benchMethod(b, r, ObjectValue(r.Global), "parseInt")
	args := []Value{str("12345")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.CallObject(parseInt, Undefined(), args)
		if err != nil || v.AsNumber() != 12345 {
			b.Fatal("parseInt failed")
		}
	}
}

func BenchmarkArraySortNumbers(b *testing.B) {
	r := NewRealm()
	cmp := r.NewNativeFunction(AtomEmpty, 2, func(r *Realm, this Value, args []Value) (Value, error) {
		return NumberValue(args[0].AsNumber() - args[1].AsNumber()), nil
	})
	sortFn := benchMethod(b, r, ObjectValue(r.ArrayPrototype), "sort")
	args := []Value{ObjectValue(cmp)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		items := make([]Value, 64)
		for i := range items {
			items[i] = IntValue((i * 37) % 64)
		}
		arr := r.NewArrayFromSlice(items)
		b.StartTimer()
		if _, err := r.CallObject(sortFn, ObjectValue(arr), args); err != nil {
			b.Fatal(err)
		}
	}
}

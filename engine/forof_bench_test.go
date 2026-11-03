package engine

import (
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
)

// benchmarkForOf runs f(), a for-of over 100 elements of an iterable
// that takes iterStep's generic path, b.N times.
func benchmarkForOf(b *testing.B, src string) {
	m, err := syntax.ParseModule("b.js", src, syntax.Options{})
	if err != nil {
		b.Fatal(err)
	}
	code, err := compiler.CompileModule(m)
	if err != nil {
		b.Fatal(err)
	}
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	env, err := r.EvaluateModule(code)
	if err != nil {
		b.Fatal(err)
	}
	fn, _ := env.GetBindingValue("f")
	b.ResetTimer()
	for range b.N {
		if _, err := r.Call(fn, Undefined(), nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkForOfUserIterator(b *testing.B) {
	benchmarkForOf(b, `const it = {i: 0, [Symbol.iterator]() { this.i = 0; return this }, next() { return this.i < 100 ? {value: this.i++, done: false} : {value: undefined, done: true} }};
export function f() { let s = 0; for (const x of it) s += x; return s }`)
}

func BenchmarkForOfGenerator(b *testing.B) {
	benchmarkForOf(b, `function* g() { for (let i = 0; i < 100; i++) yield i }
export function f() { let s = 0; for (const x of g()) s += x; return s }`)
}

func BenchmarkForOfMap(b *testing.B) {
	benchmarkForOf(b, `const m = new Map(); for (let i = 0; i < 100; i++) m.set(i, i);
export function f() { let s = 0; for (const [k, v] of m) s += v; return s }`)
}

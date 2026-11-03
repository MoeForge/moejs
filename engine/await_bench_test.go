package engine

import (
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
)

// benchmarkAsync calls f(b.N) once in a realm sharing the intrinsics, as
// the plugin runtimes do: f does one unit of the row's work per step, and
// the jobs drain when the call returns, so an op is one await, one then
// or one call.
func benchmarkAsync(b *testing.B, src string) {
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
	if _, err := r.Call(fn, Undefined(), []Value{IntValue(100)}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	v, err := r.Call(fn, Undefined(), []Value{IntValue(b.N)})
	if err != nil {
		b.Fatal(err)
	}
	if v.IsObject() && v.AsObject().class == ClassPromise {
		if st, res, _ := v.AsObject().PromiseResult(); st != PromiseFulfilled {
			b.Fatalf("f's promise is %v: %s", st, r.DisplayString(res))
		}
	}
}

// BenchmarkAwaitValue: await of a number.
func BenchmarkAwaitValue(b *testing.B) {
	benchmarkAsync(b, `export async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await i; return s }`)
}

// BenchmarkAwaitPromise: await of a fulfilled native promise.
func BenchmarkAwaitPromise(b *testing.B) {
	benchmarkAsync(b, `const p = Promise.resolve(1);
export async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await p; return s }`)
}

// BenchmarkAsyncCall: a call of an async function that returns without
// awaiting.
func BenchmarkAsyncCall(b *testing.B) {
	benchmarkAsync(b, `async function g(x) { return x }
export function f(n) { let p; for (let i = 0; i < n; i++) p = g(i); return p }`)
}

// BenchmarkAwaitAsyncCall: await of a call of an async function that
// returns without awaiting.
func BenchmarkAwaitAsyncCall(b *testing.B) {
	benchmarkAsync(b, `async function g(x) { return x }
export async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await g(i); return s }`)
}

// BenchmarkAwaitReturnPromise: await of a call of an async function that
// returns a promise, which its own promise adopts.
func BenchmarkAwaitReturnPromise(b *testing.B) {
	benchmarkAsync(b, `async function g(x) { return Promise.resolve(x) }
export async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await g(i); return s }`)
}

// BenchmarkPromiseThen: one link of a Promise.resolve().then chain.
func BenchmarkPromiseThen(b *testing.B) {
	benchmarkAsync(b, `const inc = (x) => x + 1;
export function f(n) { let p = Promise.resolve(0); for (let i = 0; i < n; i++) p = p.then(inc); return p }`)
}

// BenchmarkAwaitThen: await of a then of a fulfilled promise, so that few
// promises are live at once.
func BenchmarkAwaitThen(b *testing.B) {
	benchmarkAsync(b, `const p = Promise.resolve(1), inc = (x) => x + 1;
export async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await p.then(inc); return s }`)
}

// BenchmarkForAwaitAsyncGen: one step of a for await over an async
// generator.
func BenchmarkForAwaitAsyncGen(b *testing.B) {
	benchmarkAsync(b, `async function* g(n) { for (let i = 0; i < n; i++) yield i }
export async function f(n) { let s = 0; for await (const x of g(n)) s += x; return s }`)
}

// BenchmarkForAwaitIterator: one step of a for await over an object
// whose next returns a promise of the iterator result.
func BenchmarkForAwaitIterator(b *testing.B) {
	benchmarkAsync(b, `function it(n) { let i = 0; return { [Symbol.asyncIterator]() { return this }, next() { return Promise.resolve(i < n ? { value: i++, done: false } : { value: undefined, done: true }) } } }
export async function f(n) { let s = 0; for await (const x of it(n)) s += x; return s }`)
}

// BenchmarkForAwaitArray: one step of a for await over an array, through
// an async-from-sync iterator.
func BenchmarkForAwaitArray(b *testing.B) {
	benchmarkAsync(b, `const a = Array.from({ length: 1024 }, (_, i) => i);
export async function f(n) { let s = 0; for (let k = 0; k < n; k += a.length) for await (const x of a) s += x; return s }`)
}

// BenchmarkCallReturn: a call of an ordinary function that returns its
// argument, the floor of BenchmarkAsyncCall.
func BenchmarkCallReturn(b *testing.B) {
	benchmarkAsync(b, `function g(x) { return x }
export function f(n) { let p; for (let i = 0; i < n; i++) p = g(i); return p }`)
}

// BenchmarkNewPromiseAwait: await of a promise its executor fulfills.
func BenchmarkNewPromiseAwait(b *testing.B) {
	benchmarkAsync(b, `export async function f(n) { let s = 0; for (let i = 0; i < n; i++) s += await new Promise((res) => res(i)); return s }`)
}

// BenchmarkAwaitInterleaved: await of a fulfilled promise in two calls
// that take turns, so that every await suspends its frame and the other's
// job resumes the other frame.
func BenchmarkAwaitInterleaved(b *testing.B) {
	benchmarkAsync(b, `const p = Promise.resolve(1);
async function loop(n) { let s = 0; for (let i = 0; i < n; i++) s += await p; return s }
export function f(n) { return Promise.all([loop(n >> 1), loop(n - (n >> 1))]) }`)
}

// BenchmarkAwaitHostPromise: await of a pending promise that a host
// settles from Go through its resolve function (NewPromiseWithResolvers),
// as a host function's promise is: the host's settle runs from a native
// the loop calls.
func BenchmarkAwaitHostPromise(b *testing.B) {
	m, err := syntax.ParseModule("b.js", `export async function f(n, tool) { let s = 0; for (let i = 0; i < n; i++) s += await tool(); return s }`, syntax.Options{})
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
	// tool returns a promise and leaves its resolve function for the host,
	// which calls it once the realm is idle, as a loop's settle task does.
	var resolve *Object
	tool := r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		p, res, _ := r.NewPromiseWithResolvers()
		resolve = res
		return ObjectValue(p), nil
	})
	one := []Value{IntValue(1)}
	run := func(n int) {
		if _, err := r.Call(fn, Undefined(), []Value{IntValue(n), ObjectValue(tool)}); err != nil {
			b.Fatal(err)
		}
		for resolve != nil {
			res := resolve
			resolve = nil
			if _, err := r.CallObject(res, Undefined(), one); err != nil {
				b.Fatal(err)
			}
		}
	}
	run(100)
	b.ReportAllocs()
	b.ResetTimer()
	run(b.N)
}

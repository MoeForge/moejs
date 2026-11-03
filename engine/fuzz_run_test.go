package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
)

// fuzzRunDeadline bounds one evaluation or call; the engine has no step
// budget, so the interrupt deadline is what stops runaway programs.
const fuzzRunDeadline = 20 * time.Millisecond

// fuzzProbeSrc touches only syntax (closures, recursion, loops, throw and
// catch of primitives), so whatever the fuzzed program did to the globals
// and prototypes of its realm, the probe's value only changes if the realm
// itself was left broken.
const fuzzProbeSrc = `function f(n) { return n < 2 ? n : f(n - 1) + f(n - 2); }
let s = 0;
for (let i = 0; i < 100; i++) { try { if (i % 7 == 0) throw i; s += i; } catch (e) { s -= e; } }
export const v = s * 1000 + f(15);`

// fuzzIntrinsicsSrc reads a spread of intrinsics; in a fresh shared realm
// its value only changes if a fuzzed program mutated the frozen intrinsics.
const fuzzIntrinsicsSrc = `export const v = [[3, 1, 2].sort().map(x => x * 2).join(), JSON.stringify({a: [1, "b"]}),
  Object.keys({b: 1}), typeof Object.prototype.x, Math.max(1, 2), "ab".toUpperCase().padStart(4, "-"),
  Number("0x10"), Array.from("xy"), [..."é"].length].join("|");`

func fuzzCompile(t testing.TB, src string, module bool) *bytecode.Function {
	t.Helper()
	var code *bytecode.Function
	var err error
	if module {
		var m *syntax.Module
		if m, err = syntax.ParseModule("fuzz.js", src, syntax.Options{}); err == nil {
			code, err = compiler.CompileModule(m)
		}
	} else {
		var s *syntax.Script
		if s, err = syntax.ParseScript("fuzz.js", src, syntax.Options{}); err == nil {
			code, err = compiler.CompileScript(s)
		}
	}
	if err != nil {
		return nil
	}
	return code
}

// fuzzWithDeadline runs fn with an interrupt armed after fuzzRunDeadline and
// clears it afterwards, waiting for a timer that already fired so that its
// Interrupt cannot land after the ClearInterrupt.
func fuzzWithDeadline(r *Realm, fn func() error) error {
	done := make(chan struct{})
	timer := time.AfterFunc(fuzzRunDeadline, func() {
		r.Interrupt("fuzz deadline")
		close(done)
	})
	err := fn()
	if !timer.Stop() {
		<-done
	}
	r.ClearInterrupt()
	return err
}

// fuzzCheckErr accepts only the error kinds a run may end with; a module
// may also still be pending.
func fuzzCheckErr(t *testing.T, what string, err error) {
	t.Helper()
	switch err.(type) {
	case nil, *Exception, *InterruptedError:
	default:
		if what == "module" && err == ErrModulePending {
			return
		}
		t.Fatalf("%s: error is %T, want a JS exception or *InterruptedError: %v", what, err, err)
	}
}

// fuzzProbe evaluates a probe module in r and returns its export v.
func fuzzProbe(t testing.TB, r *Realm, probe *bytecode.Function) string {
	t.Helper()
	env, err := r.EvaluateModule(probe)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	v, _ := env.GetBindingValue("v")
	return v.String()
}

// FuzzRun evaluates small strict scripts and modules in a shared-intrinsics
// and a mutable realm under an interrupt deadline, then calls up to four
// exported functions of a module and resolves a host promise with each of
// the other exports. No Go panic may escape, every failure must be a JS
// exception or an *InterruptedError, no frame may be left on the call
// stack, the rejection tracker must see each promise rejected once and
// handled at most once after that, the realm must still run the probe
// afterwards, and a fresh shared realm must be unaffected (the shared
// intrinsics are frozen).
func FuzzRun(f *testing.F) {
	for _, s := range []string{
		"export function run() { return [1, 2, 3].map(x => x * 2).join(); }",
		"for (;;) {}",
		"while (true) { [].concat([1]); }",
		"function f() { return f() + 1; } f();",
		"try { for (;;); } finally { for (;;); }",
		"export function run() { try { throw new TypeError('x'); } catch (e) { return e.message; } }",
		"'x'.repeat(1 << 20).split('').reverse().join('');",
		"JSON.parse(JSON.stringify({a: [1, 'b', {c: null}]}));",
		"const r = /(a+)+b/u; r.test('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaac');",
		"Object.prototype.x = 1; Array.prototype.map = null; globalThis.JSON = 0;",
		"Object.freeze(Object.prototype); Math.max = () => 0; throw 1;",
		"function* g() { for (let i = 0; ; i++) yield i; } for (const x of g()) {}",
		"const o = {}; o.o = o; JSON.stringify(o);",
		"export let x = 1; export function inc() { return ++x; } export default () => { for (;;); };",
		"new Array(1000).fill(0).map((_, i) => String(i)).sort().join();",
		"`${{toString() { throw new Error('t'); }}}`;",
		"const a = [...'abc']; a.length = 1e9; Object.defineProperty(a, 0, {value: 1});",
		"function f() { arguments.length = 1e9; for (const x of arguments); return [...arguments]; } f(1, 2);",
		"function t(s, ...v) { return s === t.last ? s.raw : (t.last = s); } for (let i = 0; i < 3; i++) t`a${i}\\9`; Object.freeze(t``).raw;",
		"class A { #x = 1; static t(o) { return #x in o && o.#x; } } class B extends A { constructor() { super(); super(); } } new B();",
		"class E extends Array { constructor() { super(3); this.x = super.map; } } export function run() { return [new E().length, new (class extends Error {})().stack]; }",
		"class C extends null {} let n = 0; class D { static { for (;;) n++; } }",
		"class P { constructor() { return new.target === P ? new P() : this; } } new P();",
		"class F { f = new F(); } new F();",
		// Builtins: huge array-likes, argument lists and strings.
		"Array.prototype.toReversed.call({length: 2 ** 32 - 1}); Array.prototype.copyWithin.call({length: 2 ** 53 - 1}, 0, 1);",
		"Reflect.apply(Math.max, null, {length: 1e7}); Reflect.construct(Array, {length: 2 ** 32 - 1});",
		"export function run() { return [3, 1, 2].toSorted().with(0, 9).toSpliced(1, 1, 'x').at(-1); }",
		"Reflect.set(Math, 'max', 0); Reflect.defineProperty(Array.prototype, 'map', {value: 0}); Reflect.setPrototypeOf(Object.prototype, {});",
		"({}).__defineGetter__('x', function () { for (;;); }); Object.prototype.__lookupGetter__.call(Object.prototype, '__proto__');",
		"String.raw({raw: {length: 1e9}}, 1); String.fromCodePoint.apply(null, new Array(1e6).fill(0x1F600));",
		"atob(btoa('x'.repeat(1 << 20))); unescape(escape('\\u00e9\\ud800'.repeat(1 << 18))); 'b'.repeat(1 << 20).localeCompare('b'.repeat(1 << 20) + 'a');",
		"Math.sumPrecise(new Array(1 << 20).fill(1e308)); Error.isError(new Proxy({}, {}));",
		"(1e21).toString(36); (5e-324).toString(3); Number.MAX_VALUE.toString(7);",
		// Iterators: collections, protocols and structuredClone.
		"const m = new Map([[1, 1]]); for (const [k] of m) { m.delete(k); m.set(k + 1, k); }",
		"const s = new Set([1]); for (const v of s) { s.add(v + 1); } [...s];",
		"const it = {[Symbol.iterator]() { return {next() { return 1; }, return() { throw 2; }}; }}; for (const x of it) break;",
		"Array.prototype[Symbol.iterator] = function* () {}; RegExp.prototype.exec = () => ({index: 1e9}); 'a'.replace(/a/g, 'b'); [...[1]];",
		"const a = []; a.constructor = {[Symbol.species]: function () { return new Proxy([], {}); }}; a.map(x => x);",
		"const o = {}; o.o = o; const m = new Map([[o, [o, new Set([o])]]]); structuredClone([m, o, new Date(), /x/g, new Error('e')]);",
		"let o = {}; for (let i = 0; i < 1e6; i++) o = {o, m: new Map([[i, o]])}; structuredClone(o);",
		"const w = new WeakMap([[Symbol.iterator, 1], [{}, 2]]); new WeakRef(Symbol('s')).deref(); new WeakSet([Object.prototype]);",
		"Object.defineProperty(RegExp.prototype, 'flags', {get() { return 'gy'; }}); 'aaa'.split(/a/); [...'aa'.matchAll(/a/g)];",
		"Object.groupBy(new Set('abc'), c => c); Map.groupBy([1, 2], x => x % 2); new Set([1]).union(new Set([2]));",
		// Date, normalize, stacks, captureStackTrace and toString.
		"Date.parse('Sep 23 2026 10:20:30 GMT+0530 (IST)') + new Date(2026, 13, 40, 25).toString();",
		"new Date(8.64e15).toISOString(); new Date(-1).setFullYear(275760, 8, 14); Date.UTC(-271821, 3, 20);",
		"'\\u1E9B\\u0323\\uAC00\\u0344'.repeat(500).normalize('NFKD').normalize();",
		"Error.stackTraceLimit = 1e9; function r(n) { return n ? r(n - 1) : new Error('d').stack; } r(500);",
		"const o = {}; Error.captureStackTrace(o, Math.max); o.stack; Error.stackTraceLimit = {}; new Error().stack;",
		"[function f(a) { return a; }, x => x, {get g() { return 1; }}, Math.max.bind(null)].map(String).join();",
		// Promise and the job queue: an endless queue, then combinators.
		"function f() { queueMicrotask(f); } f(); Promise.resolve().then(function g() { return Promise.resolve().then(g); });",
		"Promise.all([1, Promise.reject(2), {then(r) { r(3); }}]).catch(e => e); Promise.any([]).catch(e => e.errors); Promise.withResolvers().resolve(Promise.try(() => { throw 1; }));",
		// Proxy: logging and throwing traps, revocation mid-operation, deep
		// chains of proxies, invariants, and proxies where builtins walk.
		"const h = new Proxy({}, {get(_, t) { return (...a) => Reflect[t](...a); }}); const p = new Proxy([1, {a: 2}], h); JSON.stringify(p); Object.keys(p); [...p]; p.push(3); Object.freeze(p); for (const k in p);",
		"let p = {}; for (let i = 0; i < 1e5; i++) p = new Proxy(p, {}); p.x; Object.keys(p); Array.isArray(p); p instanceof Object; typeof p;",
		"const p = new Proxy({}, {}); Object.setPrototypeOf(Object.prototype, p);",
		"const r = Proxy.revocable({}, {get() { r.revoke(); return 1; }, ownKeys() { for (;;); }}); r.proxy.x; r.proxy.y; Object.keys(new Proxy(r.proxy, {}));",
		"const p = new Proxy(function () {}, {apply(t, s, a) { return p(...a, 1); }, construct(t, a) { return new p(...a); }}); p();",
		"const t = Object.freeze({a: 1}); new Proxy(t, {get: () => 2}).a; Object.keys(new Proxy(t, {ownKeys: () => ['a', 'a']})); new Proxy(t, {getOwnPropertyDescriptor: () => ({value: 1, configurable: false, writable: false})});",
		"JSON.parse('[1, {\"a\": [2]}]', function (k, v) { return new Proxy(Array.isArray(v) ? [v] : {v}, {}); }); JSON.stringify(new Proxy([], {get: (t, k) => k === 'length' ? 1e9 : 0}));",
		"const o = {}; o.self = new Proxy(o, {}); structuredClone(o); Object.assign({}, new Proxy({a: 1}, {ownKeys: () => Array.from({length: 1e6}, (_, i) => String(i))}));",
		"export const p = new Proxy({then(r) { r(1); }}, {}); export const q = Proxy.revocable({}, {}).proxy; export function run() { const r = Proxy.revocable(() => 1, {}); r.revoke(); return new Proxy([r.proxy], {}); }",
		"Array.prototype.sort.call(new Proxy([3, 1, 2], {set(t, k, v) { t[k] = v; return k !== '1'; }})); [].concat(new Proxy([1], {})); Reflect.ownKeys(new Proxy([], {}));",
		"class A extends Object { constructor() { return new Proxy(super(), {}); } } new A(); Reflect.construct(Array, [], new Proxy(function () {}, {get() {}}));",
		// Binary data: TextEncoder and TextDecoder streams, resizable and
		// shared buffers, structuredClone of buffers and views with
		// transfer, detaching and resizing mid-clone, and base64 and hex.
		"const d = new TextDecoder('utf-8', {fatal: true}); d.decode(new Uint8Array([0xF0, 0x9F]), {stream: true}); try { d.decode(new Uint8Array([0x98, 0x80, 0xFF])); } catch {} d.decode(new Uint8Array([0xEF, 0xBB, 0xBF, 0x41]));",
		"new TextEncoder().encodeInto('\\u00e9\\ud83d\\ude00\\ud800'.repeat(1 << 18), new Uint8Array(new ArrayBuffer(8, {maxByteLength: 16}), 3)); new TextDecoder().decode(new TextEncoder().encode('\\ud800x'.repeat(1 << 18)));",
		"const b = new ArrayBuffer(8, {maxByteLength: 16}); structuredClone({get a() { b.resize(0); return new Uint8Array(b); }, b, v: new DataView(b)}, {transfer: [b]});",
		"const b = new ArrayBuffer(4); try { structuredClone([b, new Int16Array(b, 2)], {transfer: {*[Symbol.iterator]() { yield b; b.transfer(); }}}); } catch {} const c = new ArrayBuffer(4); structuredClone({get x() { c.transfer(); }}, {transfer: [c]});",
		"const s = new SharedArrayBuffer(4, {maxByteLength: 8}); const [t, v] = structuredClone([s, new DataView(s)]); s.grow(8); new Float16Array(t).fill(1.5); Atomics.add(new Int32Array(s), 1, v.byteLength);",
		"const b = new ArrayBuffer(1 << 20, {maxByteLength: 1 << 21}); const u = new BigInt64Array(b); b.resize(12); const [x, y, z] = structuredClone([u, new Uint8Array(b).subarray(1), b], {transfer: [b]}); z.resize(1 << 21); x.length + y.length + z.transferToFixedLength(2).byteLength;",
		"Uint8Array.fromBase64('A'.repeat(1 << 22), {lastChunkHandling: 'strict'}).toBase64({alphabet: 'base64url'}); new Uint8Array(1 << 21).toHex(); Uint8Array.fromHex('0'.repeat(1 << 22)); Uint8Array.fromBase64(' \\t'.repeat(1 << 20));",
		"const b = new ArrayBuffer(8, {maxByteLength: 16}); const t = new Uint8Array(b, 2); t.setFromHex('aabb'.repeat(4)); try { t.setFromBase64('Zm9vYmFy', {get alphabet() { b.resize(1); }}); } catch {} b.resize(16); t.setFromBase64('Zm9v\\u00e9', {lastChunkHandling: 'strict'});",
		// The host API: exports a host promise adopts, rejections the tracker sees.
		"export const t = {then(r) { r(Promise.reject(1)); for (;;); }}; export const u = Promise.reject(2); u.then(); export function v() { return Promise.reject(3).finally(() => {}); }",
		// Async functions, async generators, for await and top-level await.
		"export async function run() { for (;;) await null; } export async function f() { await f(); } export const g = async () => { for (;;); };",
		"await new Promise(() => {}); export const x = 1;",
		"export const a = 1; await Promise.reject(new Error('tla')); export const b = 2;",
		"export let n = 0; for await (const x of [1, Promise.resolve(2), {then(r) { r(3); }}]) n += x; while (true) await n++;",
		"async function* g() { try { yield 1; yield* {[Symbol.asyncIterator]() { return {next() { return {}; }, return() { throw 1; }}; }}; } finally { yield 2; } } const it = g(); it.next(); it.return(3); it.throw(4); it.next();",
		"const it = {[Symbol.iterator]() { return {next() { return {value: Promise.reject(1), done: false}; }, return() { for (;;); }}; }}; (async () => { for await (const x of it); })();",
		"export async function* run() { yield* run(); } export function f() { const it = run(); for (let i = 0; i < 1e4; i++) it.next(); }",
		"Array.fromAsync({length: 2 ** 32 - 1}); Array.fromAsync(new Proxy([1], {get(t, k) { throw k; }})); Array.fromAsync([1], async x => { for (;;) await x; });",
		"Object.defineProperty(Promise.prototype, 'then', {get() { throw 1; }}); (async () => { await null; await Promise.resolve(); })(); Promise.prototype.constructor = null;",
		"export default await (async function* () { for (;;) yield await new Proxy({}, {get(t, k) { return k === 'then' ? undefined : 0; }}); })().next();",
	} {
		f.Add(s, true)
		f.Add(s, false)
	}
	files, _ := filepath.Glob("../bench/corpus/*.js")
	for _, p := range files {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(string(b), true)
		}
	}
	// Keep runaway string growth well below the machine's memory.
	old := maxStringLength
	maxStringLength = 1 << 22
	f.Cleanup(func() { maxStringLength = old })
	probe := fuzzCompile(f, fuzzProbeSrc, true)
	want := fuzzProbe(f, NewRealm(), probe)
	intrinsics := fuzzCompile(f, fuzzIntrinsicsSrc, true)
	wantIntrinsics := fuzzProbe(f, NewRealmWith(RealmOptions{SharedIntrinsics: true}), intrinsics)
	f.Fuzz(func(t *testing.T, src string, module bool) {
		if len(src) > 4096 {
			return
		}
		code := fuzzCompile(t, src, module)
		if code == nil {
			return
		}
		for _, shared := range []bool{true, false} {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			tracked := map[*Object]PromiseRejectionOperation{}
			r.SetPromiseRejectionTracker(func(p *Object, op PromiseRejectionOperation) {
				state, _, _ := p.PromiseResult()
				prev, seen := tracked[p]
				if state != PromiseRejected || (op == PromiseRejectionReject) == seen || prev == PromiseRejectionHandle {
					t.Fatalf("shared=%v: tracker op %d on a promise in state %d, after %d (seen %v)", shared, op, state, prev, seen)
				}
				tracked[p] = op
			})
			if module {
				var env *ModuleEnv
				err := fuzzWithDeadline(r, func() (err error) {
					env, err = r.EvaluateModule(code)
					return err
				})
				fuzzCheckErr(t, "module", err)
				if env != nil {
					names := env.ExportNames()
					sort.Strings(names)
					for _, name := range names[:min(len(names), 4)] {
						fn, _ := env.GetBindingValue(name)
						if !IsCallable(fn) {
							_, resolve, _ := r.NewPromiseWithResolvers()
							err := fuzzWithDeadline(r, func() error {
								_, err := r.CallObject(resolve, Undefined(), []Value{fn})
								return err
							})
							fuzzCheckErr(t, "resolve with "+name, err)
							continue
						}
						err := fuzzWithDeadline(r, func() error {
							_, err := r.Call(fn, Undefined(), nil)
							return err
						})
						fuzzCheckErr(t, "export "+name, err)
					}
				}
			} else {
				err := fuzzWithDeadline(r, func() error {
					_, err := r.RunScript(code)
					return err
				})
				fuzzCheckErr(t, "script", err)
			}
			if d := r.CallDepth(); d != 0 {
				t.Fatalf("shared=%v: call depth %d after the run", shared, d)
			}
			if got := fuzzProbe(t, r, probe); got != want {
				t.Fatalf("shared=%v: probe = %v after the run, want %v", shared, got, want)
			}
		}
		if got := fuzzProbe(t, NewRealmWith(RealmOptions{SharedIntrinsics: true}), intrinsics); got != wantIntrinsics {
			t.Fatalf("fresh shared realm: intrinsics probe = %s, want %s", got, wantIntrinsics)
		}
	})
}

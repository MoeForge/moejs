package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAwaitOrder checks the order of await, then and queueMicrotask jobs,
// and the reads of then and constructor an await makes, against probes
// whose logs Node.js 24 printed (the reads of a patched constructor less
// the one of the harness's own await).
func TestAwaitOrder(t *testing.T) {
	runAsyncCases(t, []protoCase{
		{"await kinds interleave with then", `const t = { then(r) { log.push("then"); r("T"); } };
			async function a(v, n) { log.push(n + "0"); log.push(n + ":" + await v); }
			a(1, "p"); a(Promise.resolve(2), "q"); a(t, "r"); a({}, "o"); a({ then: 1 }, "x");
			Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")).then(() => log.push("j4"));
			log.push("sync")`,
			"p0|q0|r0|o0|x0|sync|p:1|q:2|then|o:[object Object]|x:[object Object]|j1|r:T|j2|j3|j4"},
		{"then getter runs at the await", `const o = { get then() { log.push("get"); return undefined; } };
			async function f() { log.push("before"); const v = await o; log.push(v === o); } f(); log.push("after")`,
			"before|get|after|true"},
		{"then getter throws", `const o = { get then() { log.push("get"); throw "boom"; } };
			async function f() { try { await o; } catch (e) { log.push("caught " + e); } } f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")); log.push("sync")`,
			"get|sync|caught boom|j1|j2"},
		{"constructor getter on a promise", `const p = Promise.resolve("v"); Object.defineProperty(p, "constructor", { get() { log.push("ctor"); return Promise; } });
			async function f() { log.push(await p); } f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"ctor|sync|v|j1|j2|j3"},
		{"constructor is another on a promise", `const p = Promise.resolve("v"); p.constructor = Object;
			async function f() { log.push(await p); } f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"sync|j1|j2|v|j3"},
		{"subclass promise is wrapped", `class P extends Promise {} const p = P.resolve("s");
			async function f() { log.push(await p); } f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")).then(() => log.push("j4")); log.push("sync")`,
			"sync|j1|j2|s|j3|j4"},
		{"rejected await order", `async function f() { try { await Promise.reject("r"); } catch (e) { log.push("c " + e); } } async function g() { try { await (async () => { throw "s"; })(); } catch (e) { log.push("d " + e); } }
			f(); g(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"sync|c r|d s|j1|j2|j3"},
		{"return value, promise and thenable", `async function a() { return 1; } async function b() { return Promise.resolve(2); } async function c() { return { then(r) { r(3); } }; }
			a().then(v => log.push("a" + v)); b().then(v => log.push("b" + v)); c().then(v => log.push("c" + v));
			Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")).then(() => log.push("j4")); log.push("sync")`,
			"sync|a1|j1|c3|j2|b2|j3|j4"},
		{"await in a loop against a then chain", `async function f() { for (let i = 0; i < 4; i++) { await undefined; log.push("f" + i); } }
			f(); let p = Promise.resolve(); for (let i = 0; i < 4; i++) p = p.then(() => log.push("t" + i))`,
			"f0|t0|f1|t1|f2|t2|f3|t3"},
		{"await the same promise twice in two functions", `const p = Promise.resolve("x"); async function f(n) { log.push(n + await p); log.push(n + await p); } f("a"); f("b"); log.push("sync")`,
			"sync|ax|bx|ax|bx"},
		{"await a pending promise resolved later", `let res; const p = new Promise(r => res = r); async function f(n) { log.push(n + await p); } f("a"); f("b"); p.then(v => log.push("t" + v)); f("c"); res("!"); log.push("sync")`,
			"sync|a!|b!|t!|c!"},
		{"await a pending promise rejected later", `let rej; const p = new Promise((_, r) => rej = r); async function f(n) { try { await p; } catch (e) { log.push(n + e); } } f("a"); f("b"); rej("!"); log.push("sync")`,
			"sync|a!|b!"},
		{"nested async calls", `async function g(x) { log.push("g" + x); await null; log.push("g" + x + "'"); return x; } async function f() { log.push("f"); const v = await g(1) + await g(2); log.push("f=" + v); }
			f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")).then(() => log.push("j4")).then(() => log.push("j5")); log.push("sync")`,
			"f|g1|sync|g1'|j1|g2|j2|g2'|j3|f=3|j4|j5"},
		{"for await over an async generator", `async function* g() { log.push("g0"); yield 1; log.push("g1"); yield 2; log.push("g2"); }
			async function f() { for await (const x of g()) log.push("x" + x); log.push("end"); } f();
			let p = Promise.resolve(); for (let i = 0; i < 8; i++) p = p.then(() => log.push("t" + i)); log.push("sync")`,
			"g0|sync|t0|x1|g1|t1|t2|x2|g2|t3|end|t4|t5|t6|t7"},
		{"for await over an array of promises and values", `async function f() { for await (const x of [1, Promise.resolve(2), 3]) log.push("x" + x); log.push("end"); } f();
			let p = Promise.resolve(); for (let i = 0; i < 8; i++) p = p.then(() => log.push("t" + i)); log.push("sync")`,
			"sync|t0|x1|t1|t2|x2|t3|t4|x3|t5|t6|end|t7"},
		{"async generator yield and await", `async function* g() { const a = await "a"; log.push(a); const b = yield 1; log.push(b); yield Promise.resolve(2); return 3; }
			const it = g(); it.next().then(r => log.push("n1 " + r.value + r.done)); it.next("B").then(r => log.push("n2 " + r.value + r.done)); it.next().then(r => log.push("n3 " + r.value + r.done)); it.next().then(r => log.push("n4 " + r.value + r.done));
			let p = Promise.resolve(); for (let i = 0; i < 6; i++) p = p.then(() => log.push("t" + i)); log.push("sync")`,
			"sync|a|t0|B|t1|n1 1false|t2|n2 2false|t3|n3 3true|n4 undefinedtrue|t4|t5"},
		{"queueMicrotask interleaving", `async function f() { log.push("f0"); await 0; log.push("f1"); queueMicrotask(() => log.push("q2")); await 0; log.push("f2"); }
			queueMicrotask(() => log.push("q0")); f(); queueMicrotask(() => log.push("q1")); log.push("sync")`,
			"f0|sync|q0|f1|q1|q2|f2"},
		{"then species and constructor reads", `const p = Promise.resolve(1); let reads = 0; Object.defineProperty(p, "constructor", { get() { reads++; return Promise; } });
			p.then(() => log.push("then " + reads)); log.push("sync " + reads)`,
			"sync 1|then 1"},
		{"then on a subclass uses species", `class P extends Promise { static get [Symbol.species]() { log.push("species"); return Promise; } } const p = P.resolve(1); const q = p.then(v => v); log.push(q instanceof P, q.constructor === Promise)`,
			"species|false|true"},
		{"Promise.resolve keeps a plain promise", `const p = Promise.resolve(1); log.push(Promise.resolve(p) === p); class P extends Promise {} log.push(P.resolve(p) === p, Promise.resolve(P.resolve(1)) instanceof P)`,
			"true|false|false"},
		{"await a promise whose then was replaced on the instance", `const p = Promise.resolve("v"); p.then = function (a, b) { log.push("own then"); return Promise.prototype.then.call(this, a, b); };
			async function f() { log.push(await p); } f(); log.push("sync")`,
			"sync|v"},
		{"rejection of an awaited value with a throwing then", `async function f() { await { then() { throw "t"; } }; } f().catch(e => log.push("c " + e)); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"sync|j1|j2|c t|j3"},
		{"a promise on another prototype is wrapped", `class Sub extends Promise {} const p = Promise.resolve("v"); Object.setPrototypeOf(p, Sub.prototype);
			async function f() { log.push(await p); } f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"sync|j1|j2|v|j3"},
		{"a frozen promise is awaited as it is", `const p = Object.freeze(Promise.resolve("v")); async function f() { log.push(await p); } f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")); log.push("sync")`,
			"sync|v|j1|j2"},
		{"a promise with an own symbol is awaited as it is", `const p = Promise.resolve("v"); p[Symbol("s")] = 1; p.x = 2; async function f() { log.push(await p); } f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")); log.push("sync")`,
			"sync|v|j1|j2"},
		{"then on a promise with an own constructor", `const p = Promise.resolve(1); class Sub extends Promise {} p.constructor = Sub; const q = p.then(v => v); log.push(q instanceof Sub)`,
			"true"},
		{"await a non-promise object with a then on its prototype", `class T { then(r) { log.push("T.then"); r("t"); } } async function f() { log.push(await new T()); } f(); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")); log.push("sync")`,
			"sync|T.then|j1|t|j2"},
		{"await in an async arrow of a class field", `class A { f = async () => { log.push("a" + await 1); }; } new A().f(); Promise.resolve().then(() => log.push("j1")); log.push("sync")`,
			"sync|a1|j1"},
		{"await of a function and a symbol", `async function f() { const s = Symbol("s"); log.push(typeof await function () {}, (await s) === s, await 1n, await null, await undefined); } f()`,
			"function|true|1||"},
		{"adoption of a promise with an own then", `const q = Promise.resolve(1); q.then = function (a, b) { log.push("own then"); return Promise.prototype.then.call(this, a, b); };
			const p = new Promise(r => r(q)); p.then(v => log.push("p" + v)); log.push("sync")`,
			"sync|own then|p1"},
		{"adoption of a promise with an own constructor getter", `const q = Promise.resolve(1); Object.defineProperty(q, "constructor", { get() { log.push("ctor"); return Promise; } });
			const p = new Promise(r => r(q)); p.then(v => log.push("p" + v)); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"sync|ctor|j1|j2|p1|j3"},
		{"adoption of a rejected promise", `const q = Promise.reject("r"); async function f() { return q; } f().catch(e => log.push("c " + e)); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")).then(() => log.push("j4")); log.push("sync")`,
			"sync|j1|j2|c r|j3|j4"},
		{"adoption of a pending promise", `let res; const q = new Promise(r => res = r); async function f() { return q; } f().then(v => log.push("f" + v)); q.then(v => log.push("q" + v)); Promise.resolve().then(() => { log.push("j1"); res("!"); }).then(() => log.push("j2")).then(() => log.push("j3")).then(() => log.push("j4")); log.push("sync")`,
			"sync|j1|q!|j2|f!|j3|j4"},
		{"then handler returns a promise", `Promise.resolve(1).then(() => Promise.resolve(2)).then(v => log.push("v" + v)); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")).then(() => log.push("j4")); log.push("sync")`,
			"sync|j1|j2|j3|v2|j4"},
		{"adoption chain of async functions", `async function a() { return 1; } async function b() { return a(); } async function c() { return b(); } c().then(v => log.push("c" + v));
			let p = Promise.resolve(); for (let i = 0; i < 8; i++) p = p.then(() => log.push("t" + i)); log.push("sync")`,
			"sync|t0|t1|t2|c1|t3|t4|t5|t6|t7"},
		{"adoption of a subclass promise", `class P extends Promise {} const q = P.resolve(1); async function f() { return q; } f().then(v => log.push("f" + v)); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")).then(() => log.push("j4")); log.push("sync")`,
			"sync|j1|j2|f1|j3|j4"},
		{"objects with an own then and without", `async function f(v, n) { const r = await v; log.push(n + ":" + (r === v ? "same" : r)); }
			f({ a: 1 }, "a"); f({ then: 5 }, "b"); f({ then(r) { r("t"); } }, "c"); f([1, 2], "d"); f(Object.create(null), "e"); f(Object.create({ then(r) { r("p"); } }), "g");
			Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")); log.push("sync")`,
			"sync|a:same|b:same|d:same|e:same|j1|c:t|g:p|j2"},
		{"resolve with objects", `const objs = [{}, { then: 1 }, [], { then(r) { r("t"); } }, new Proxy({}, { get(t, k) { log.push("trap " + String(k)); return undefined; } })];
			for (const o of objs) new Promise(r => r(o)).then(v => log.push(v === o ? "same" : v)); log.push("sync")`,
			"trap then|sync|same|same|same|same|t"},
		{"Promise.all resolves an array", `Promise.all([1, Promise.resolve(2)]).then(v => log.push(v.join())); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"sync|j1|1,2|j2|j3"},
		{"async function returns an object", `async function f() { return { k: 1 }; } f().then(v => log.push("k" + v.k)); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")); log.push("sync")`,
			"sync|k1|j1|j2"},
	})
	// The probes that patch the intrinsics run in mutable realms only: a
	// shared realm's are frozen.
	for _, c := range []protoCase{
		{"patched Promise.prototype.constructor", `const orig = Object.getOwnPropertyDescriptor(Promise.prototype, "constructor");
			Object.defineProperty(Promise.prototype, "constructor", { get() { log.push("pctor"); return Promise; }, configurable: true });
			async function f() { log.push(await Promise.resolve("v")); } f();
			Promise.resolve().then(() => log.push("j1")).then(() => { log.push("j2"); Object.defineProperty(Promise.prototype, "constructor", orig); }); log.push("sync")`,
			"pctor|pctor|pctor|sync|v|j1|j2"},
		{"patched then is not called for a native promise", `const then = Promise.prototype.then; Promise.prototype.then = function (a, b) { log.push("then"); return then.call(this, a, b); };
			async function f() { log.push(await Promise.resolve("v")); } f(); log.push("sync"); Promise.prototype.then = then`,
			"sync|v"},
		{"constructor replaced then restored", `async function f(n) { log.push(n + await Promise.resolve(n)); }
			const C = Promise.prototype.constructor; Promise.prototype.constructor = Object; f("a"); Promise.prototype.constructor = C; f("b");
			Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"sync|bb|j1|j2|aa|j3"},
		{"constructor deleted then restored", `async function f(n) { log.push(n + await Promise.resolve(n)); }
			const C = Promise.prototype.constructor; delete Promise.prototype.constructor; f("a"); const q = Promise.resolve(0).then(() => log.push("q"));
			Promise.prototype.constructor = C; f("b"); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")).then(() => log.push("j3")); log.push("sync")`,
			"sync|q|bb|j1|j2|aa|j3"},
		{"species getter then restored", `class Sub extends Promise {} const d = Object.getOwnPropertyDescriptor(Promise, Symbol.species);
			Object.defineProperty(Promise, Symbol.species, { get() { log.push("species"); return Sub; }, configurable: true });
			const q = Promise.resolve(1).then(v => v); log.push(q instanceof Sub);
			Object.defineProperty(Promise, Symbol.species, d); const q2 = Promise.resolve(1).then(v => v); log.push(q2 instanceof Sub)`,
			"species|true|false"},
		{"Promise.prototype on another prototype", `const P = Promise.prototype; const proto = Object.getPrototypeOf(P);
			Object.setPrototypeOf(P, { __proto__: proto, get then() { return 1; } });
			async function f() { log.push(await Promise.resolve("v")); } f(); Object.setPrototypeOf(P, proto); log.push("sync")`,
			"sync|v"},
		{"adoption reads species at the job", `const d = Object.getOwnPropertyDescriptor(Promise, Symbol.species);
			Object.defineProperty(Promise, Symbol.species, { get() { log.push("species"); return Promise; }, configurable: true });
			const q = Promise.resolve(1); log.push("made"); const p = new Promise(r => r(q)); log.push("resolved");
			p.then(v => { log.push("p" + v); Object.defineProperty(Promise, Symbol.species, d); }); Promise.resolve().then(() => log.push("j1")).then(() => log.push("j2")); log.push("sync")`,
			"made|resolved|species|species|species|sync|species|j1|j2|p1"},
		{"adoption calls a patched then", `const then = Promise.prototype.then; Promise.prototype.then = function (a, b) { log.push("then " + (this === q)); return then.call(this, a, b); };
			const q = Promise.resolve(1); async function f() { return q; } f().then(v => { log.push("f" + v); Promise.prototype.then = then; }); log.push("sync")`,
			"then false|sync|then true|f1"},
		{"adoption reads a patched constructor", `const orig = Object.getOwnPropertyDescriptor(Promise.prototype, "constructor");
			const q = Promise.resolve(1); const p = new Promise(r => r(q)); log.push("resolved");
			Object.defineProperty(Promise.prototype, "constructor", { get() { log.push("ctor"); return Promise; }, configurable: true });
			Promise.resolve().then(() => { log.push("j1"); Object.defineProperty(Promise.prototype, "constructor", orig); }); log.push("sync")`,
			"resolved|ctor|sync|ctor|j1"},
		{"then added to Object.prototype", `async function f(v, n) { log.push(n + ":" + (await v === v)); }
			f({}, "a"); f([], "b"); Object.prototype.then = function (r) { log.push("then " + (Array.isArray(this) ? "array" : "object")); r(this); };
			f({}, "c"); f([], "d"); Promise.resolve({}).then(v => log.push("r " + typeof v)); delete Object.prototype.then; f({}, "e"); log.push("sync")`,
			"sync|a:true|b:true|then object|then array|then object|e:true|c:true|d:true|r object"},
		{"then added to Array.prototype", `async function f(v, n) { log.push(n + ":" + (await v === v)); }
			Array.prototype.then = function (r) { log.push("then"); r(this); }; f([], "a"); f({}, "b"); delete Array.prototype.then; f([], "c"); log.push("sync")`,
			"sync|then|b:true|c:true|a:true"},
		{"Array.prototype on another prototype", `async function f(v, n) { log.push(n + ":" + (await v === v)); }
			const op = Object.getPrototypeOf(Array.prototype); Object.setPrototypeOf(Array.prototype, { __proto__: op, then(r) { log.push("then"); r(1); } });
			f([], "a"); Object.setPrototypeOf(Array.prototype, op); f([], "b"); log.push("sync")`,
			"sync|then|b:true|a:false"},
		{"then getter on Object.prototype", `Object.defineProperty(Object.prototype, "then", { get() { log.push("get"); return undefined; }, configurable: true });
			async function f() { log.push(await { x: 1 } !== undefined); } f(); const p = Promise.resolve({ y: 2 }); log.push("made"); delete Object.prototype.then; p.then(v => log.push(v.y)); log.push("sync")`,
			"get|get|made|sync|true|2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newAsyncFixture(t, c.body, false)
			f.call("f")
			assert.Equal(t, c.want, f.call("done"))
		})
	}
}

package engine

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAwaitOrderMore: more orderings of await, then, async generators and
// for await, with thenables, a then on Object.prototype or Array.prototype,
// patched then, constructor and species, and subclasses, against the logs
// Node.js 24.22 printed for the same bodies. The bodies patch intrinsics, so
// they run in a mutable realm.
func TestAwaitOrderMore(t *testing.T) {
	cases := []protoCase{
		{"thenable resolves twice and rejects", "const t = { then(res, rej) { log.push(\"then\"); res(\"a\"); res(\"b\"); rej(\"c\"); } };\n    async function g() { try { log.push(\"v=\" + await t); } catch (e) { log.push(\"e=\" + e); } }\n    g(); { let q = Promise.resolve(); for (let i = 1; i <= 3; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|then|j1|v=a|j2|j3"},
		{"thenable throws after resolving", "const t = { then(res) { res(1); throw 2; } };\n    (async () => { try { log.push(\"v\" + await t); } catch (e) { log.push(\"e\" + e); } })(); { let q = Promise.resolve(); for (let i = 1; i <= 3; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|v1|j2|j3"},
		{"thenable resolves with a thenable", "const inner = { then(r) { log.push(\"inner\"); r(\"deep\"); } };\n    const t = { then(r) { log.push(\"outer\"); r(inner); } };\n    (async () => log.push(await t))(); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|outer|j1|inner|j2|deep|j3|j4|j5"},
		{"then getter read once per await", "let n = 0; const o = { get then() { n++; log.push(\"get\" + n); return r => r(n); } };\n    (async () => { log.push(\"a\" + await o); log.push(\"b\" + await o); })(); log.push(\"sync\")",
			"get1|sync|a1|get2|b2"},
		{"callable thenable", "const fn = function () {}; fn.then = r => { log.push(\"then\"); r(7); };\n    (async () => log.push(\"v\" + await fn))(); { let q = Promise.resolve(); for (let i = 1; i <= 3; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|then|j1|v7|j2|j3"},
		{"object gains then after fulfilling", "const o = {}; const p = Promise.resolve(o); o.then = r => { log.push(\"then\"); r(42); };\n    (async () => log.push(\"await:\" + ((await p) === o)))();\n    new Promise(r => r(p)).then(v => log.push(\"adopt:\" + v));\n    (async () => p)().then(v => log.push(\"ret:\" + v));\n    log.push(\"res:\" + (Promise.resolve(p) === p)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"res:true|sync|await:true|j1|j2|then|then|j3|adopt:42|ret:42|j4|j5"},
		{"Object.prototype.then added after fulfilling", "const o = {}; const p = Promise.resolve(o);\n    Object.prototype.then = function (r) { log.push(\"then\"); delete Object.prototype.then; r(42); };\n    new Promise(r => r(p)).then(v => log.push(\"adopt:\" + v)); log.push(\"sync\")",
			"sync|then|adopt:42"},
		{"Array.prototype.then added after fulfilling", "const a = [1]; const p = Promise.resolve(a);\n    Array.prototype.then = function (r) { log.push(\"then \" + this.length); delete Array.prototype.then; r(43); };\n    (async () => p)().then(v => log.push(\"ret:\" + v)); log.push(\"sync\")",
			"sync|then 1|ret:43"},
		{"Object.prototype then getter throws", "Object.defineProperty(Object.prototype, \"then\", { get() { log.push(\"get\"); throw \"boom\"; }, configurable: true });\n    (async () => { try { await {}; log.push(\"no\"); } catch (e) { log.push(\"c:\" + e); } })();\n    Promise.resolve([]).then(null, e => log.push(\"r:\" + e));\n    delete Object.prototype.then; log.push(\"sync\")",
			"get|get|sync|c:boom|r:boom"},
		{"Object.prototype.then and a dictionary object", "const o = {}; for (let i = 0; i < 200; i++) o[\"k\" + i] = i; for (let i = 0; i < 200; i++) delete o[\"k\" + i];\n    Object.prototype.then = function (r) { log.push(\"then\"); delete Object.prototype.then; r(\"D\"); };\n    (async () => log.push(\"v\" + await o))(); log.push(\"sync\")",
			"sync|then|vD"},
		{"Object.prototype.then on a null-proto chain", "const o = Object.create(Object.create(Object.prototype));\n    Object.prototype.then = function (r) { log.push(\"then\"); delete Object.prototype.then; r(\"N\"); };\n    (async () => log.push(\"v\" + await o))(); log.push(\"sync\")",
			"sync|then|vN"},
		{"Object.prototype.then and Object.create", "const o = Object.create(Object.prototype, { x: { value: 1 } });\n    Object.prototype.then = function (r) { log.push(\"then\"); delete Object.prototype.then; r(\"X\"); };\n    (async () => log.push(\"v\" + await o))(); log.push(\"sync\")",
			"sync|then|vX"},
		{"Array.prototype.then shadowed by undefined", "Object.prototype.then = function (r) { log.push(\"othen\"); r(\"O\"); };\n    Array.prototype.then = undefined;\n    (async () => log.push(\"a:\" + Array.isArray(await [])))();\n    (async () => log.push(\"o:\" + await {}))();\n    delete Array.prototype.then; (async () => log.push(\"b:\" + await []))();\n    delete Object.prototype.then; log.push(\"sync\")",
			"sync|a:true|othen|othen|o:O|b:O"},
		{"Array.prototype then getter", "Object.defineProperty(Array.prototype, \"then\", { get() { log.push(\"get\"); return undefined; }, configurable: true });\n    (async () => log.push(\"a:\" + Array.isArray(await [1, 2])))(); Promise.resolve([3]).then(v => log.push(\"r:\" + v.length));\n    delete Array.prototype.then; log.push(\"sync\")",
			"get|get|sync|a:true|r:1"},
		{"then on Object.prototype and Promise combinators", "Object.prototype.then = function (r) { log.push(\"then\"); r(\"T\"); };\n    Promise.all([{}, 1]).then(v => log.push(\"all:\" + v)); Promise.race([{}]).then(v => log.push(\"race:\" + v));\n    Promise.allSettled([{}]).then(v => log.push(\"as:\" + v[0].value)); Promise.any([{}]).then(v => log.push(\"any:\" + v));\n    Promise.resolve().then(() => Promise.resolve().then(() => Promise.resolve().then(() => Promise.resolve().then(() => { delete Object.prototype.then; })))); log.push(\"sync\")",
			"sync|then|then|then|then|then|race:T|then|any:T|all:T|as:undefined"},
		{"then in a JSON.parse object", "(async () => log.push(\"v\" + await JSON.parse('{\"then\": 1}').then))();\n    const o = JSON.parse('{\"a\": 1}'); Object.prototype.then = function (r) { log.push(\"then\"); delete Object.prototype.then; r(\"J\"); };\n    (async () => log.push(\"w\" + await o))(); log.push(\"sync\")",
			"sync|v1|then|wJ"},
		{"array subclass then", "class A extends Array {} A.prototype.then = function (r) { log.push(\"athen\"); r(\"A\"); };\n    (async () => log.push(\"v\" + await new A()))(); (async () => log.push(\"w\" + Array.isArray(await [])))(); log.push(\"sync\")",
			"sync|athen|wtrue|vA"},
		{"arguments object with Object.prototype.then", "function args() { return arguments; } const a = args(1, 2);\n    Object.prototype.then = function (r) { log.push(\"then \" + this.length); delete Object.prototype.then; r(\"G\"); };\n    (async () => log.push(\"v\" + await a))(); log.push(\"sync\")",
			"sync|then 2|vG"},
		{"globalThis with own then", "(async () => log.push(\"g:\" + ((await globalThis) === globalThis)))();\n    globalThis.then = function (r) { log.push(\"gthen\"); delete globalThis.then; r(\"GT\"); };\n    (async () => log.push(\"v\" + await globalThis))(); log.push(\"sync\")",
			"sync|g:true|gthen|vGT"},
		{"await does not call a patched then", "const then = Promise.prototype.then;\n    Promise.prototype.then = function (a, b) { log.push(\"patched\"); return then.call(this, a, b); };\n    (async () => log.push(\"v\" + await Promise.resolve(1)))(); Promise.prototype.then = then; log.push(\"sync\")",
			"sync|v1"},
		{"patched then captured at resolve", "const then = Promise.prototype.then; const q = Promise.resolve(1);\n    Promise.prototype.then = function (a, b) { log.push(\"patched\"); return then.call(this, a, b); };\n    const p = new Promise(r => r(q)); Promise.prototype.then = then;\n    p.then(v => log.push(\"p\" + v)); log.push(\"sync\")",
			"sync|patched|p1"},
		{"original then captured, patched before the job", "const then = Promise.prototype.then; const q = Promise.resolve(1);\n    const p = new Promise(r => r(q));\n    Promise.prototype.then = function (a, b) { log.push(\"patched\"); return then.call(this, a, b); };\n    then.call(p, v => { log.push(\"p\" + v); Promise.prototype.then = then; }); log.push(\"sync\")",
			"sync|p1"},
		{"then not callable at an async return", "const then = Promise.prototype.then; const q = Promise.resolve(1);\n    Promise.prototype.then = 5; async function g() { return q; } const r = g(); Promise.prototype.then = then;\n    r.then(v => log.push(\"same:\" + (v === q))); log.push(\"sync\")",
			"sync|same:true"},
		{"then deleted, Object.prototype.then used", "const then = Promise.prototype.then; delete Promise.prototype.then;\n    Object.prototype.then = function (r) { log.push(\"othen \" + (this instanceof Promise)); delete Object.prototype.then; Promise.prototype.then = then; r(5); };\n    const q = Promise.resolve(1); const p = new Promise(r => r(q));\n    (async () => log.push(\"v\" + await p))(); log.push(\"sync\")",
			"sync|othen true|v5"},
		{"species is a subclass", "class P extends Promise { constructor(ex) { log.push(\"P\"); super(ex); } }\n    const d = Object.getOwnPropertyDescriptor(Promise, Symbol.species);\n    Object.defineProperty(Promise, Symbol.species, { value: P, configurable: true });\n    const t = Promise.resolve(1).then(v => log.push(\"v\" + v)); log.push(\"isP:\" + (t instanceof P));\n    new Promise(r => r(Promise.resolve(2))).then(v => { log.push(\"a\" + v); Object.defineProperty(Promise, Symbol.species, d); });\n    (async () => log.push(\"aw\" + await Promise.resolve(3)))(); log.push(\"sync\")",
			"P|isP:true|P|sync|v1|P|aw3|a2"},
		{"species getter returns undefined", "const d = Object.getOwnPropertyDescriptor(Promise, Symbol.species);\n    Object.defineProperty(Promise, Symbol.species, { get() { log.push(\"sp\"); return undefined; }, configurable: true });\n    const t = Promise.resolve(1).then(v => log.push(\"v\" + v)); log.push(\"ctor:\" + (t.constructor === Promise));\n    Object.defineProperty(Promise, Symbol.species, d); log.push(\"sync\")",
			"sp|ctor:true|sync|v1"},
		{"constructor is a subclass on Promise.prototype", "class P extends Promise { constructor(ex) { log.push(\"P\"); super(ex); } }\n    Promise.prototype.constructor = P;\n    (async () => log.push(\"aw\" + await Promise.resolve(1)))();\n    log.push(\"res:\" + (Promise.resolve(Promise.resolve(2)) instanceof P));\n    { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; Promise.prototype.constructor = Promise; log.push(\"sync\")",
			"res:false|P|P|P|P|sync|j1|j2|aw1|j3|j4"},
		{"constructor undefined on a promise", "const p = Promise.resolve(1); p.constructor = undefined;\n    const t = p.then(v => log.push(\"v\" + v)); log.push(\"plain:\" + (Object.getPrototypeOf(t) === Promise.prototype));\n    (async () => log.push(\"aw\" + await p))(); { let q = Promise.resolve(); for (let i = 1; i <= 3; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"plain:true|sync|v1|j1|j2|aw1|j3"},
		{"constructor changed in a reaction", "const p = Promise.resolve(1);\n    p.then(() => { Promise.prototype.constructor = Object; log.push(\"set\"); });\n    p.then(() => { (async () => log.push(\"aw\" + await Promise.resolve(2)))(); Promise.prototype.constructor = Promise; });\n    { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|set|j1|j2|j3|aw2|j4|j5"},
		{"then patched in a reaction", "const then = Promise.prototype.then; const q = Promise.resolve(1);\n    q.then(() => { Promise.prototype.then = function (a, b) { log.push(\"patched\"); return then.call(this, a, b); }; });\n    q.then(() => { new Promise(r => r(q)); (async () => q)(); });\n    q.then(() => { log.push(\"x\"); }); { let q = Promise.resolve(); for (let i = 1; i <= 3; i++) q = q.then(() => log.push(\"j\" + i)); }; Promise.resolve().then(() => {}).then(() => {}).then(() => {}).then(() => { Promise.prototype.then = then; }); log.push(\"sync\")",
			"sync|x|j1|patched|patched|j2|j3"},
		{"await a subclass instance", "class P extends Promise { constructor(ex) { log.push(\"ctor\"); super(ex); } }\n    const p = P.resolve(1); log.push(\"made\"); (async () => log.push(\"v\" + await p))(); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"ctor|made|sync|ctor|j1|j2|v1|j3|j4"},
		{"return a subclass instance", "class P extends Promise { constructor(ex) { log.push(\"ctor\"); super(ex); } }\n    const p = P.resolve(1); log.push(\"made\"); (async () => p)().then(v => log.push(\"v\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"ctor|made|sync|ctor|j1|j2|v1|j3|j4"},
		{"subclass with its own then", "class P extends Promise { then(a, b) { log.push(\"own then\"); return super.then(a, b); } }\n    const p = P.resolve(1); (async () => log.push(\"v\" + await p))(); new Promise(r => r(p)).then(v => log.push(\"a\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|own then|own then|j1|j2|v1|a1|j3|j4"},
		{"Promise.resolve of a subclass and the reverse", "class P extends Promise {} const s = P.resolve(1); const n = Promise.resolve(2);\n    log.push(\"a:\" + (Promise.resolve(s) === s) + \" b:\" + (P.resolve(n) === n) + \" c:\" + (P.resolve(s) === s));\n    Promise.resolve(s).then(v => log.push(\"s\" + v)); P.resolve(n).then(v => log.push(\"n\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"a:false b:false c:true|sync|j1|j2|s1|n2|j3|j4"},
		{"native promise with a subclass prototype", "class P extends Promise { constructor(ex) { log.push(\"P\"); super(ex); } }\n    const p = Promise.resolve(1); Object.setPrototypeOf(p, P.prototype);\n    (async () => log.push(\"aw\" + await p))(); const t = p.then(v => log.push(\"t\" + v)); log.push(\"isP:\" + (t instanceof P)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"P|isP:true|sync|P|t1|j1|j2|aw1|j3|j4"},
		{"subclass instance with Promise.prototype", "class P extends Promise {} const p = P.resolve(1); Object.setPrototypeOf(p, Promise.prototype);\n    (async () => log.push(\"aw\" + await p))(); new Promise(r => r(p)).then(v => log.push(\"a\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|aw1|j1|j2|a1|j3|j4"},
		{"await in finally", "async function g() { try { log.push(\"try\"); return \"r\"; } finally { log.push(\"fin0\"); await null; log.push(\"fin1\"); } }\n    g().then(v => log.push(\"v\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"try|fin0|sync|fin1|j1|vr|j2|j3|j4"},
		{"return await and return", "async function a() { return await Promise.resolve(\"a\"); } async function b() { return Promise.resolve(\"b\"); } async function c() { return await \"c\"; }\n    a().then(v => log.push(v)); b().then(v => log.push(v)); c().then(v => log.push(v)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|a|c|j2|b|j3|j4|j5"},
		{"throw in finally after await", "async function g() { try { await null; return 1; } finally { await null; throw \"F\"; } }\n    g().then(v => log.push(\"v\" + v), e => log.push(\"e\" + e)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|j2|eF|j3|j4|j5"},
		{"await in catch rethrows", "async function g() { try { await Promise.reject(\"x\"); } catch (e) { log.push(\"c\" + e); await null; throw e + \"!\"; } }\n    g().catch(e => log.push(\"e\" + e)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|cx|j1|j2|ex!|j3|j4|j5"},
		{"returned promise frozen and reshaped while running", "async function g() { await null; log.push(\"resumed\"); await null; return 1; }\n    const p = g(); p.x = 1; Object.freeze(p); log.push(\"frozen:\" + Object.isFrozen(p));\n    Promise.prototype.then.call(p, v => log.push(\"v\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"frozen:true|sync|resumed|j1|j2|v1|j3|j4"},
		{"returned promise loses its prototype while running", "async function g() { await null; return 2; }\n    const p = g(); Object.setPrototypeOf(p, null); Promise.prototype.then.call(p, v => log.push(\"v\" + v));\n    (async () => log.push(\"aw\" + await p))(); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|v2|j2|j3|j4|j5"},
		{"returned promise as a dictionary", "async function g() { await null; return 3; }\n    const p = g(); for (let i = 0; i < 100; i++) p[\"k\" + i] = i; for (let i = 0; i < 100; i++) delete p[\"k\" + i];\n    p.then(v => log.push(\"v\" + v)); (async () => log.push(\"aw\" + await p))(); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|v3|aw3|j2|j3|j4|j5"},
		{"returned promise with an own then", "async function g() { await null; return 4; }\n    const p = g(); p.then = function (r) { log.push(\"own\"); return Promise.prototype.then.call(this, r); };\n    (async () => log.push(\"aw\" + await p))(); new Promise(r => r(p)).then(v => log.push(\"a\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|own|j1|aw4|j2|a4|j3|j4|j5"},
		{"returns its own promise", "let p; async function g() { await null; return p; } p = g();\n    p.then(null, e => log.push(e.constructor.name + \": \" + e.message)); { let q = Promise.resolve(); for (let i = 1; i <= 3; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|TypeError: Chaining cycle detected for promise #<Promise>|j2|j3"},
		{"two promises awaiting each other", "let a, b; async function f1() { await null; return b; } async function f2() { await null; return a; }\n    a = f1(); b = f2(); a.then(v => log.push(\"a\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|j2|j3|j4|j5"},
		{"async calls are distinct promises", "const g = async () => 1; const p1 = g(), p2 = g();\n    log.push(String(p1 !== p2) + (p1 instanceof Promise) + (Object.getPrototypeOf(p1) === Promise.prototype) + (p1.constructor === Promise) + Object.keys(p1).length + Object.prototype.toString.call(p1))",
			"truetruetruetrue0[object Promise]"},
		{"await thenable that resolves with the awaiting promise", "let p; const t = { then(r) { r(p); } };\n    async function g() { await null; const v = await t; log.push(\"got \" + (v === p)); return 1; } p = g(); { let q = Promise.resolve(); for (let i = 1; i <= 6; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|j2|j3|j4|j5|j6"},
		{"await in a sync throw path", "async function g() { log.push(\"s\"); throw new Error(\"E\"); } g().catch(e => log.push(\"c\" + e.message)); { let q = Promise.resolve(); for (let i = 1; i <= 2; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"s|sync|cE|j1|j2"},
		{"await a rejected thenable", "const t = { then(_, rej) { rej(\"R\"); } };\n    (async () => { try { await t; } catch (e) { log.push(\"c\" + e); } })(); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|cR|j2|j3|j4"},
		{"many awaits interleaved", "async function g(n) { for (let i = 0; i < 3; i++) { log.push(n + i); await (i % 2 ? Promise.resolve() : i); } }\n    g(\"a\"); g(\"b\"); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"a0|b0|sync|a1|b1|j1|a2|b2|j2|j3|j4"},
		{"agen return() at start with a promise", "async function* g() { log.push(\"body\"); yield 1; }\n    const it = g(); it.return(Promise.resolve(\"r\")).then(v => log.push(JSON.stringify(v))); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|{\"value\":\"r\",\"done\":true}|j2|j3|j4|j5"},
		{"agen return() at a yield with an awaiting finally", "async function* g() { try { yield 1; yield 2; } finally { log.push(\"fin\"); await null; log.push(\"fin2\"); } }\n    const it = g(); it.next().then(v => log.push(\"n\" + v.value)); it.return(\"R\").then(v => log.push(\"r\" + v.value + v.done)); it.next().then(v => log.push(\"n2\" + v.done)); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|n1|fin|j2|fin2|j3|rRtrue|n2true|j4|j5|j6|j7|j8"},
		{"agen throw() caught at a yield", "async function* g() { while (true) { try { yield 1; } catch (e) { log.push(\"caught \" + e); await null; } } }\n    const it = g(); it.next().then(v => log.push(\"n\" + v.value)); it.throw(\"T\").then(v => log.push(\"t\" + v.value)); it.return(9).then(v => log.push(\"r\" + v.value)); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|caught T|j1|n1|j2|j3|t1|j4|r9|j5|j6|j7|j8"},
		{"agen throw() at start", "async function* g() { log.push(\"body\"); yield 1; }\n    const it = g(); it.throw(\"T\").catch(e => log.push(\"c\" + e)); it.next().then(v => log.push(\"n\" + v.done)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|cT|ntrue|j1|j2|j3|j4"},
		{"agen return() of a rejected promise when completed", "async function* g() {} const it = g(); it.next().then(v => log.push(\"n\" + v.done));\n    it.return(Promise.reject(\"X\")).catch(e => log.push(\"c\" + e)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|ntrue|j1|cX|j2|j3|j4|j5"},
		{"agen return() of a thenable", "async function* g() { yield 1; } const it = g();\n    it.return({ then(r) { log.push(\"then\"); r(\"TV\"); } }).then(v => log.push(\"r\" + v.value)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|then|j1|j2|rTV|j3|j4|j5"},
		{"agen return() of a thenable whose then getter throws", "async function* g() { yield 1; } const it = g();\n    it.return({ get then() { log.push(\"get\"); throw \"G\"; } }).catch(e => log.push(\"c\" + e)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"get|sync|j1|cG|j2|j3|j4|j5"},
		{"agen return() with Object.prototype.then", "async function* g() { yield 1; } const it = g();\n    Object.prototype.then = function (r) { log.push(\"othen\"); delete Object.prototype.then; r(\"OT\"); };\n    it.return({}).then(v => log.push(\"r\" + v.value)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|othen|j1|j2|rOT|j3|j4|j5"},
		{"agen yield a thenable and a rejected promise", "async function* g() { yield { then(r) { log.push(\"then\"); r(\"Y\"); } }; try { yield Promise.reject(\"RJ\"); } catch (e) { log.push(\"in \" + e); } yield 3; }\n    (async () => { for await (const v of g()) log.push(\"v\" + v); })(); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|then|j1|j2|vY|j3|in RJ|j4|j5|v3|j6|j7|j8"},
		{"agen yield an object with Object.prototype.then", "async function* g() { yield {}; yield [1]; }\n    Object.prototype.then = function (r) { log.push(\"othen\"); r(\"O\" + Array.isArray(this)); };\n    const it = g(); it.next().then(v => log.push(\"a\" + v.value)); it.next().then(v => { log.push(\"b\" + v.value); delete Object.prototype.then; }); { let q = Promise.resolve(); for (let i = 1; i <= 6; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|othen|j1|j2|othen|othen|j3|aundefined|j4|othen|j5|bundefined|j6"},
		{"agen yield* over an async generator with return()", "async function* inner() { try { yield 1; yield 2; } finally { log.push(\"ifin\"); } }\n    async function* outer() { try { yield* inner(); } finally { log.push(\"ofin\"); } }\n    const it = outer(); it.next().then(v => log.push(\"n\" + v.value)); it.return(\"R\").then(v => log.push(\"r\" + v.value)); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|j2|n1|j3|ifin|j4|ofin|j5|rR|j6|j7|j8"},
		{"agen yield* over a sync iterable of promises", "function* s() { yield Promise.resolve(1); yield 2; }\n    async function* g() { const r = yield* s(); log.push(\"ret \" + r); }\n    (async () => { for await (const v of g()) log.push(\"v\" + v); })(); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|j2|v1|j3|j4|j5|v2|j6|j7|ret undefined|j8"},
		{"agen awaits with queued next calls", "async function* g() { log.push(\"b0\"); await null; yield \"x\"; log.push(\"b1\"); await Promise.resolve(); yield \"y\"; }\n    const it = g(); for (let i = 0; i < 3; i++) it.next().then(v => log.push(i + \":\" + v.value + v.done)); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"b0|sync|j1|b1|j2|0:xfalse|j3|j4|1:yfalse|2:undefinedtrue|j5|j6|j7|j8"},
		{"for await over a sync generator with a rejection", "function* s() { try { yield 1; yield Promise.reject(\"x\"); yield 3; } finally { log.push(\"closed\"); } }\n    (async () => { try { for await (const v of s()) log.push(\"v\" + v); } catch (e) { log.push(\"c\" + e); } })(); { let q = Promise.resolve(); for (let i = 1; i <= 6; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|v1|j2|closed|j3|cx|j4|j5|j6"},
		{"for await over an array of thenables", "const t = n => ({ then(r) { log.push(\"then\" + n); r(n); } });\n    (async () => { for await (const v of [t(1), 2, t(3)]) log.push(\"v\" + v); })(); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|then1|j1|j2|v1|j3|j4|v2|j5|then3|j6|j7|v3|j8"},
		{"for await over an array with Object.prototype.then", "Object.prototype.then = function (r) { log.push(\"othen\"); r(\"O\"); };\n    (async () => { for await (const v of [{}, 2]) log.push(\"v\" + v); delete Object.prototype.then; })(); { let q = Promise.resolve(); for (let i = 1; i <= 6; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|othen|j1|j2|othen|j3|j4|j5|j6"},
		{"for await break closes a sync iterator", "function* s() { try { yield 1; yield 2; } finally { log.push(\"closed\"); } }\n    (async () => { for await (const v of s()) { log.push(\"v\" + v); break; } log.push(\"after\"); })(); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|v1|closed|j2|j3|after|j4|j5"},
		{"for await over pending promises resolved later", "const rs = []; const ps = [0, 1, 2].map(i => new Promise(r => rs.push(r)));\n    (async () => { for await (const v of ps) log.push(\"v\" + v); log.push(\"end\"); })(); rs[2](\"c\"); rs[0](\"a\"); setTimeout0: rs[1](\"b\"); { let q = Promise.resolve(); for (let i = 1; i <= 6; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|va|j2|j3|vb|j4|j5|vc|j6|end"},
		{"for await over a sync iterator returning thenables", "let i = 0; const it = { [Symbol.iterator]() { return this; }, next() { i++; return i > 2 ? { done: true } : { value: { then(r) { log.push(\"then\" + i); r(i); } }, done: false }; } };\n    (async () => { for await (const v of it) log.push(\"v\" + v); log.push(\"end\"); })(); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|then1|j1|j2|v1|j3|then2|j4|j5|v2|j6|j7|end|j8"},
		{"for await sync value whose then getter throws", "function* s() { try { yield { get then() { throw \"G\"; } }; } finally { log.push(\"closed\"); } }\n    (async () => { try { for await (const v of s()) log.push(\"v\"); } catch (e) { log.push(\"c\" + e); } })(); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|closed|j1|cG|j2|j3|j4|j5"},
		{"Array.fromAsync over promises and thenables", "Array.fromAsync([Promise.resolve(1), { then(r) { log.push(\"then\"); r(2); } }, 3]).then(v => log.push(\"got \" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 8; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|j2|then|j3|j4|j5|j6|j7|j8|got 1,2,3"},
		{"adoption of a pending native promise", "let res; const q = new Promise(r => res = r); const p = new Promise(r => r(q)); p.then(v => log.push(\"p\" + v));\n    q.then(v => log.push(\"q\" + v)); res(1); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|q1|j1|j2|p1|j3|j4"},
		{"adoption of a rejected native promise", "const q = Promise.reject(\"x\"); const p = new Promise(r => r(q)); p.catch(e => log.push(\"c\" + e)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|j2|cx|j3|j4"},
		{"resolve with a promise then reject", "const p = new Promise((res, rej) => { res(Promise.resolve(1)); rej(2); res(3); }); p.then(v => log.push(\"v\" + v), e => log.push(\"e\" + e)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|j2|v1|j3|j4"},
		{"Promise.resolve of a thenable promise-like", "const o = { then(r) { log.push(\"then\"); r(Promise.resolve(\"in\")); } }; Promise.resolve(o).then(v => log.push(\"v\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 6; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|then|j1|j2|j3|vin|j4|j5|j6"},
		{"finally on a plain promise", "Promise.resolve(1).finally(() => log.push(\"fin\")).then(v => log.push(\"v\" + v)); Promise.reject(2).finally(() => log.push(\"fin2\")).catch(e => log.push(\"e\" + e)); { let q = Promise.resolve(); for (let i = 1; i <= 6; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|fin|fin2|j1|j2|j3|v1|e2|j4|j5|j6"},
		{"finally with patched species", "class P extends Promise { constructor(ex) { log.push(\"P\"); super(ex); } }\n    const d = Object.getOwnPropertyDescriptor(Promise, Symbol.species); Object.defineProperty(Promise, Symbol.species, { value: P, configurable: true });\n    const t = Promise.resolve(1).finally(() => log.push(\"fin\")); Object.defineProperty(Promise, Symbol.species, d); t.then(v => log.push(\"v\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 5; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"P|P|sync|fin|P|P|j1|P|j2|j3|v1|j4|j5"},
		{"then callbacks hold their values once", "const p = Promise.resolve(1); const t = p.then(v => v + 1); t.then(v => log.push(\"t\" + v)); t.then(v => log.push(\"u\" + v)); { let q = Promise.resolve(); for (let i = 1; i <= 3; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|t2|u2|j2|j3"},
		{"then with non-callable handlers passes through", "Promise.resolve(1).then(5, 6).then(v => log.push(\"v\" + v)); Promise.reject(2).then(5).then(null, e => log.push(\"e\" + e)); { let q = Promise.resolve(); for (let i = 1; i <= 4; i++) q = q.then(() => log.push(\"j\" + i)); }; log.push(\"sync\")",
			"sync|j1|v1|e2|j2|j3|j4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAsyncFixture(t, c.body, false)
			f.call("f")
			assert.Equal(t, c.want, f.call("done"))
		})
	}
}

// TestUnhandledRejections: the rejections no handler took by the end of
// the drain, against the reasons Node.js 24.22 reported as unhandled for the
// same bodies.
func TestUnhandledRejections(t *testing.T) {
	cases := []protoCase{
		{"async throws", "(async () => { throw \"a\"; })();", "a"},
		{"await rejected, uncaught", "(async () => { await Promise.reject(\"b\"); })();", "b"},
		{"await rejected, caught", "(async () => { try { await Promise.reject(\"c\"); } catch {} })();", ""},
		{"return a rejected promise", "(async () => Promise.reject(\"d\"))();", "d"},
		{"resolve with a rejected promise", "new Promise(r => r(Promise.reject(\"e\")));", "e"},
		{"then on a rejected promise", "Promise.reject(\"f\").then(() => {});", "f"},
		{"handled in a later microtask", "const p = Promise.reject(\"g\"); queueMicrotask(() => p.catch(() => {}));", ""},
		{"await a rejecting thenable", "(async () => await { then(_, r) { r(\"h\"); } })();", "h"},
		{"finally on a rejected promise", "Promise.reject(\"i\").finally(() => {});", "i"},
		{"async generator throws", "(async function* () { throw \"j\"; })().next();", "j"},
		{"for await over a rejected promise", "(async () => { for await (const x of [Promise.reject(\"k\")]) {} })();", "k"},
		{"plain reject", "Promise.reject(\"l\");", "l"},
		{"handled after two awaits", "const p = Promise.reject(\"m\"); (async () => { await null; await null; p.catch(() => {}); })();", ""},
		{"then getter throws at an await, caught", "(async () => { try { await { get then() { throw \"n\"; } }; } catch {} })();", ""},
		{"then getter throws at an await, uncaught", "(async () => { await { get then() { throw \"o\"; } }; })();", "o"},
		{"adopt a rejected subclass", "class P extends Promise {} new Promise(r => r(P.reject(\"p\")));", "p"},
		{"async returns a rejected subclass", "class P extends Promise {} (async () => P.reject(\"q\"))();", "q"},
		{"then with species to a rejected", "class P extends Promise {} P.reject(\"r\").then(() => {});", "r"},
		{"Object.prototype.then rejects", "Object.prototype.then = function (_, rej) { delete Object.prototype.then; rej(\"s\"); }; (async () => { await {}; })();", "s"},
		{"agen return() of a rejected promise", "const it = (async function* () {})(); it.return(Promise.reject(\"t\"));", "t"},
		{"agen yield a rejected promise", "const it = (async function* () { yield Promise.reject(\"u\"); })(); it.next();", "u"},
		{"async-from-sync rejected value", "const it = (async function* () { yield* [Promise.reject(\"v\")]; })(); it.next();", "v"},
		{"Promise.all with a rejected", "Promise.all([Promise.reject(\"w\"), 1]);", "w"},
		{"await an async call that throws later", "(async () => { await (async () => { await null; throw \"x\"; })(); })();", "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAsyncFixture(t, c.body, false)
			pending := map[*Object]bool{}
			var order []*Object
			f.r.SetPromiseRejectionTracker(func(p *Object, op PromiseRejectionOperation) {
				if op == PromiseRejectionReject {
					pending[p] = true
					order = append(order, p)
				} else {
					delete(pending, p)
				}
			})
			_, err := f.callErr("f")
			require.NoError(t, err)
			var got []string
			for _, p := range order {
				if pending[p] {
					_, v, _ := p.PromiseResult()
					got = append(got, v.String())
					delete(pending, p)
				}
			}
			sort.Strings(got)
			assert.Equal(t, c.want, strings.Join(got, "|"))
		})
	}
}

package webapi_test

import (
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/webapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsCase is a script whose result is want.
type jsCase struct {
	name, src, want string
}

func runCases(t *testing.T, cases []jsCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, webapi.Options{})
			got, err := h.try(c.src)
			if err != nil {
				got = "REJECTED " + err.Error()
			}
			assert.Equal(t, c.want, got)
		})
	}
}

func TestGlobals(t *testing.T) {
	runCases(t, []jsCase{
		{"issue", `return Object.fromEntries([
  "fetch", "Headers", "Request", "Response",
  "ReadableStream", "TransformStream",
  "AbortController", "AbortSignal", "URL", "URLSearchParams", "performance"
].map(name => [name, typeof globalThis[name]]));`,
			`{"fetch":"function","Headers":"function","Request":"function","Response":"function","ReadableStream":"function","TransformStream":"function","AbortController":"function","AbortSignal":"function","URL":"function","URLSearchParams":"function","performance":"object"}`},
		{"descriptor", `const d = Object.getOwnPropertyDescriptor(globalThis, "ReadableStream");
return [d.writable, d.enumerable, d.configurable, typeof d.value, Object.keys(globalThis).includes("fetch")]`,
			`[true,false,true,"function",false]`},
		{"replaceable", `globalThis.fetch = 1; delete globalThis.URL; return [fetch, typeof URL]`, `[1,"undefined"]`},
		{"toStringTag", `return [new Headers(), new ReadableStream(), new AbortController().signal, performance].map(String)`,
			`["[object Headers]","[object ReadableStream]","[object AbortSignal]","[object Performance]"]`},
		{"illegal constructor", `try { new ReadableStreamDefaultController() } catch (e) { return e.name + ": " + e.message }`,
			`TypeError: Illegal constructor`},
		{"incompatible receiver", `try { Headers.prototype.get.call({}, "a") } catch (e) { return e instanceof TypeError }`, `true`},
		{"subclass", `class S extends ReadableStream { get x() { return 1 } }
const s = new S({ start(c) { c.enqueue("a"); c.close() } });
const r = s.getReader();
return [s instanceof ReadableStream, s.x, (await r.read()).value]`, `[true,1,"a"]`},
		{"performance", `const a = performance.now(); const b = performance.now();
return [typeof a, a >= 0, b >= a, performance.timeOrigin > 1.6e12, typeof performance.toJSON().timeOrigin]`,
			`["number",true,true,true,"number"]`},
	})
}

func TestReadableStream(t *testing.T) {
	runCases(t, []jsCase{
		{"start enqueue", `const s = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); c.close(); } });
const r = s.getReader(); const out = [];
for (;;) { const { done, value } = await r.read(); if (done) break; out.push(value); }
await r.closed; return out`, `[1,2]`},
		{"pull on demand", `let pulls = 0;
const s = new ReadableStream({ pull(c) { pulls++; c.enqueue(pulls); if (pulls === 3) c.close(); } }, { highWaterMark: 0 });
await null; const before = pulls;
const r = s.getReader(); const a = await r.read(); const b = await r.read();
return [before, a.value, b.value, pulls]`, `[0,1,2,2]`},
		{"async pull", `let n = 0;
const s = new ReadableStream({ async pull(c) { await new Promise(r => setTimeout(r, 1)); c.enqueue(++n); if (n === 3) c.close(); } });
const out = []; for await (const x of s) out.push(x); return out`, `[1,2,3]`},
		{"desiredSize", `let ctl; const s = new ReadableStream({ start(c) { ctl = c; } }, { highWaterMark: 2 });
const a = ctl.desiredSize; ctl.enqueue("x"); const b = ctl.desiredSize; ctl.enqueue("y"); ctl.enqueue("z");
return [a, b, ctl.desiredSize]`, `[2,1,-1]`},
		{"size strategy", `let ctl; new ReadableStream({ start(c) { ctl = c; } }, new ByteLengthQueuingStrategy({ highWaterMark: 10 }));
ctl.enqueue(new Uint8Array(4)); const a = ctl.desiredSize;
let ctl2; new ReadableStream({ start(c) { ctl2 = c; } }, new CountQueuingStrategy({ highWaterMark: 3 }));
ctl2.enqueue("abc"); return [a, ctl2.desiredSize]`, `[6,2]`},
		{"cancel", `let reason; const s = new ReadableStream({ start(c) { c.enqueue(1); }, cancel(r) { reason = r; } });
const r = s.getReader(); await r.cancel("why"); const x = await r.read();
return [reason, x.done, s.locked]`, `["why",true,true]`},
		{"release rejects reads", `const s = new ReadableStream();
const r = s.getReader(); const p = r.read(); r.releaseLock();
try { await p } catch (e) { return [e.name, s.locked] }`, `["TypeError",false]`},
		{"error", `const s = new ReadableStream({ start(c) { c.enqueue(1); c.error(new RangeError("bad")); } });
const r = s.getReader();
try { await r.read() } catch (e) { return e.name + " " + e.message }`, `RangeError bad`},
		{"error after reads", `let ctl; const s = new ReadableStream({ start(c) { ctl = c; } });
const r = s.getReader(); const p = r.read(); ctl.error(new Error("x"));
const res = await Promise.allSettled([p, r.closed]); return res.map(x => x.status)`, `["rejected","rejected"]`},
		{"locked", `const s = new ReadableStream(); s.getReader();
try { s.getReader() } catch (e) { return e.name }`, `TypeError`},
		{"byob unsupported", `try { new ReadableStream().getReader({ mode: "byob" }) } catch (e) { return e.name }`, `TypeError`},
		{"async iterator break cancels", `let canceled = false; let n = 0;
const s = new ReadableStream({ pull(c) { c.enqueue(++n); }, cancel() { canceled = true; } });
for await (const x of s) { if (x === 2) break; }
return [canceled, s.locked]`, `[true,false]`},
		{"values preventCancel", `let canceled = false;
const s = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); }, cancel() { canceled = true; } });
for await (const x of s.values({ preventCancel: true })) break;
const r = s.getReader(); return [canceled, (await r.read()).value]`, `[false,2]`},
		{"async iterator error", `const s = new ReadableStream({ start(c) { c.enqueue(1); c.error(new TypeError("boom")); } });
try { for await (const x of s) {} } catch (e) { return e.message }`, `boom`},
		{"tee", `const s = new ReadableStream({ start(c) { c.enqueue("a"); c.enqueue("b"); c.close(); } });
const [a, b] = s.tee(); const out = [];
for await (const x of a) out.push("a" + x);
for await (const x of b) out.push("b" + x);
return out`, `["aa","ab","ba","bb"]`},
		{"tee cancel both", `let reason; const s = new ReadableStream({ cancel(r) { reason = r; } });
const [a, b] = s.tee(); await Promise.all([a.cancel(1), b.cancel(2)]); return reason`, `[1,2]`},
		{"from async generator", `async function* g() { yield 1; yield 2; }
const out = []; for await (const x of ReadableStream.from(g())) out.push(x); return out`, `[1,2]`},
		{"from array of promises", `const out = []; for await (const x of ReadableStream.from([1, Promise.resolve(2)])) out.push(x); return out`, `[1,2]`},
		{"start rejects", `const s = new ReadableStream({ start() { return Promise.reject(new Error("no")); } });
try { await s.getReader().read() } catch (e) { return e.message }`, `no`},
		{"pull throws", `const s = new ReadableStream({ pull() { throw new Error("p"); } });
try { await s.getReader().read() } catch (e) { return e.message }`, `p`},
		{"enqueue after close", `let ctl; new ReadableStream({ start(c) { ctl = c; } }); ctl.close();
try { ctl.enqueue(1) } catch (e) { return e.name }`, `TypeError`},
		{"bytes type", `const s = new ReadableStream({ type: "bytes", start(c) { c.enqueue(new Uint8Array([1, 2])); c.close(); } });
const r = s.getReader(); const x = await r.read(); return [x.value.length, (await r.read()).done]`, `[2,true]`},
		{"queued read is resolved promise", `const s = new ReadableStream({ start(c) { c.enqueue(1); } });
const r = s.getReader(); const order = []; r.read().then(() => order.push("read")); Promise.resolve().then(() => order.push("tick"));
await new Promise(r => setTimeout(r, 1)); return order`, `["read","tick"]`},
	})
}

func TestWritableAndPipes(t *testing.T) {
	runCases(t, []jsCase{
		{"writer", `const got = []; const ws = new WritableStream({ write(c) { got.push(c); } });
const w = ws.getWriter(); await w.write(1); await w.write(2); await w.close(); return got`, `[1,2]`},
		{"backpressure", `let release; const ws = new WritableStream({ write() { return new Promise(r => release = r); } }, { highWaterMark: 1 });
const w = ws.getWriter(); const a = w.desiredSize; w.write(1); w.write(2);
const b = w.desiredSize; await null; release(); await null; await null;
return [a, b]`, `[1,-1]`},
		{"abort", `let reason; const ws = new WritableStream({ abort(r) { reason = r; } });
const w = ws.getWriter(); await w.abort("stop");
try { await w.write(1) } catch (e) { return [reason, e] }`, `["stop","stop"]`},
		{"sink error", `const ws = new WritableStream({ write() { throw new Error("w"); } });
const w = ws.getWriter(); try { await w.write(1) } catch (e) { return [e.message, (await w.closed.catch(e => e.message))] }`, `["w","w"]`},
		{"pipeTo", `const rs = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); c.close(); } });
const got = []; let closed = false;
await rs.pipeTo(new WritableStream({ write(c) { got.push(c); }, close() { closed = true; } }));
return [got, closed, rs.locked]`, `[[1,2],true,false]`},
		{"pipeTo source error aborts", `const rs = new ReadableStream({ start(c) { c.enqueue(1); c.error(new Error("src")); } });
let aborted; const p = rs.pipeTo(new WritableStream({ abort(r) { aborted = r.message; } }));
try { await p } catch (e) { return [e.message, aborted] }`, `["src","src"]`},
		{"pipeTo dest error cancels", `let canceled; const rs = new ReadableStream({ pull(c) { c.enqueue(1); }, cancel(r) { canceled = r.message; } });
try { await rs.pipeTo(new WritableStream({ write() { throw new Error("dst"); } })) } catch (e) { return [e.message, canceled] }`, `["dst","dst"]`},
		{"pipeTo signal", `const ac = new AbortController(); let n = 0;
const rs = new ReadableStream({ pull(c) { c.enqueue(++n); if (n === 3) ac.abort(); } });
try { await rs.pipeTo(new WritableStream(), { signal: ac.signal }) } catch (e) { return e.name }`, `AbortError`},
		{"pipeTo preventClose", `const rs = new ReadableStream({ start(c) { c.close(); } });
const ws = new WritableStream(); await rs.pipeTo(ws, { preventClose: true });
const w = ws.getWriter(); await w.write(1); return "open"`, `open`},
		{"pipeThrough transform", `const rs = new ReadableStream({ start(c) { c.enqueue("a"); c.enqueue("b"); c.close(); } });
const up = rs.pipeThrough(new TransformStream({ transform(ch, c) { c.enqueue(ch.toUpperCase()); c.enqueue(ch); } }));
const out = []; for await (const x of up) out.push(x); return out`, `["A","a","B","b"]`},
		{"transform flush", `const t = new TransformStream({ transform(ch, c) {}, flush(c) { c.enqueue("end"); } });
const w = t.writable.getWriter(); w.write(1); w.close();
const out = []; for await (const x of t.readable) out.push(x); return out`, `["end"]`},
		{"identity transform", `const t = new TransformStream(); const w = t.writable.getWriter();
w.write(1); w.write(2); w.close(); const out = []; for await (const x of t.readable) out.push(x); return out`, `[1,2]`},
		{"transform error", `const t = new TransformStream({ transform() { throw new Error("t"); } });
const w = t.writable.getWriter(); const r = t.readable.getReader();
const [a, b] = await Promise.allSettled([w.write(1), r.read()]); return [a.reason.message, b.reason.message]`, `["t","t"]`},
		{"terminate", `const t = new TransformStream({ start(c) { c.enqueue(1); c.terminate(); } });
const out = []; for await (const x of t.readable) out.push(x);
try { await t.writable.getWriter().write(2) } catch (e) { return [out, e.name] }`, `[[1],"TypeError"]`},
		{"readable cancel errors writable", `const t = new TransformStream(); await t.readable.cancel(new Error("c"));
try { await t.writable.getWriter().write(1) } catch (e) { return e.message }`, `c`},
		{"text decoder stream", `const bytes = new TextEncoder().encode("héllo wörld");
const rs = new ReadableStream({ start(c) { for (let i = 0; i < bytes.length; i += 3) c.enqueue(bytes.subarray(i, i + 3)); c.close(); } });
let s = ""; for await (const x of rs.pipeThrough(new TextDecoderStream())) s += x; return s`, `héllo wörld`},
		{"text decoder stream props", `const d = new TextDecoderStream("utf-8", { fatal: true }); return [d.encoding, d.fatal, d.ignoreBOM]`, `["utf-8",true,false]`},
		{"text encoder stream", `const t = new TextEncoderStream(); const w = t.writable.getWriter();
w.write("a\ud83d"); w.write("\ude00b"); w.write("\ud800"); w.close();
const out = []; for await (const x of t.readable) out.push(...x); return out`, `[97,240,159,152,128,98,239,191,189]`},
	})
}

func TestEvents(t *testing.T) {
	runCases(t, []jsCase{
		{"abort controller", `const ac = new AbortController(); const s = ac.signal; const log = [];
s.addEventListener("abort", e => log.push(e.type, e.target === s, e.isTrusted));
s.onabort = () => log.push("onabort");
ac.abort(); ac.abort();
return [s.aborted, s.reason.name, s.reason instanceof DOMException, s.reason.code, log]`,
			`[true,"AbortError",true,20,["abort",true,true,"onabort"]]`},
		{"abort reason", `const ac = new AbortController(); ac.abort(42); try { ac.signal.throwIfAborted() } catch (e) { return e }`, `42`},
		{"once and remove", `const t = new EventTarget(); let n = 0; const f = () => n++;
t.addEventListener("x", f, { once: true }); t.dispatchEvent(new Event("x")); t.dispatchEvent(new Event("x"));
t.addEventListener("y", f); t.removeEventListener("y", f); t.dispatchEvent(new Event("y")); return n`, `1`},
		{"listener signal", `const t = new EventTarget(); const ac = new AbortController(); let n = 0;
t.addEventListener("x", () => n++, { signal: ac.signal }); t.dispatchEvent(new Event("x")); ac.abort(); t.dispatchEvent(new Event("x")); return n`, `1`},
		{"handleEvent and preventDefault", `const t = new EventTarget();
t.addEventListener("x", { handleEvent(e) { e.preventDefault(); } });
return [t.dispatchEvent(new Event("x", { cancelable: true })), t.dispatchEvent(new Event("x"))]`, `[false,true]`},
		{"stopImmediatePropagation", `const t = new EventTarget(); const log = [];
t.addEventListener("x", e => { log.push(1); e.stopImmediatePropagation(); }); t.addEventListener("x", () => log.push(2));
t.dispatchEvent(new Event("x")); return log`, `[1]`},
		{"AbortSignal.abort", `const s = AbortSignal.abort(); return [s.aborted, s.reason.name]`, `[true,"AbortError"]`},
		{"AbortSignal.timeout", `const s = AbortSignal.timeout(5); const keep = setTimeout(() => {}, 1000);
await new Promise(r => s.onabort = r); clearTimeout(keep);
return [s.reason.name, s.reason.code, s.reason.message]`, `["TimeoutError",23,"The operation was aborted due to timeout"]`},
		{"AbortSignal.any", `const a = new AbortController(), b = new AbortController();
const s = AbortSignal.any([a.signal, b.signal]); const s2 = AbortSignal.any([s]);
b.abort("b"); return [s.aborted, s.reason, s2.aborted, s2.reason]`, `[true,"b",true,"b"]`},
		{"AbortSignal.any aborted", `return AbortSignal.any([AbortSignal.abort(1)]).reason`, `1`},
		{"DOMException", `const e = new DOMException("m", "NotFoundError"); const f = new DOMException("x", { name: "AbortError", cause: 1 });
return [e.name, e.message, e.code, e instanceof Error, String(e), f.name, f.cause, DOMException.ABORT_ERR, new DOMException().name]`,
			`["NotFoundError","m",8,true,"NotFoundError: m","AbortError",1,20,"Error"]`},
		{"timeout keeps nothing alive", `AbortSignal.timeout(100000); return "ok"`, `ok`},
	})
}

func TestHeaders(t *testing.T) {
	runCases(t, []jsCase{
		{"basic", `const h = new Headers({ "Content-Type": "text/plain", "X-A": "1" });
h.append("x-a", "2"); h.set("X-B", " v "); h.append("Set-Cookie", "a=1"); h.append("set-cookie", "b=2");
return [h.get("x-a"), h.get("CONTENT-TYPE"), h.get("x-b"), h.has("x-c"), h.get("x-c"), [...h], h.getSetCookie()]`,
			`["1, 2","text/plain","v",false,null,[["content-type","text/plain"],["set-cookie","a=1"],["set-cookie","b=2"],["x-a","1, 2"],["x-b","v"]],["a=1","b=2"]]`},
		{"init forms", `return [[...new Headers([["a", "1"], ["b", "2"]])], [...new Headers(new Headers({ c: "3" }))], Object.fromEntries(new Headers({ d: "4" }))]`,
			`[[["a","1"],["b","2"]],[["c","3"]],{"d":"4"}]`},
		{"invalid", `const out = [];
for (const f of [() => new Headers({ "a b": "1" }), () => new Headers({ a: "x\ny" }), () => new Headers([["a"]]), () => new Headers({ a: "Ā" })])
  try { f(); out.push("ok") } catch (e) { out.push(e.name) }
return out`, `["TypeError","TypeError","TypeError","TypeError"]`},
		{"latin1", `const h = new Headers({ a: "é" }); return h.get("a").charCodeAt(0)`, `233`},
		{"delete and forEach", `const h = new Headers({ a: "1", b: "2" }); h.delete("A"); const out = [];
h.forEach((v, k, o) => out.push(k, v, o === h)); return out`, `["b","2",true]`},
		{"iterator live", `const h = new Headers({ b: "1" }); const it = h.keys(); h.append("a", "2"); return [it.next().value, it.next().value, it.next().done]`,
			`["a","b",true]`},
		{"iterator proto", `const it = new Headers().entries(); return [Object.prototype.toString.call(it), typeof it[Symbol.iterator]]`,
			`["[object Headers Iterator]","function"]`},
	})
}

func TestRequestResponse(t *testing.T) {
	runCases(t, []jsCase{
		{"response text", `const r = new Response("héllo", { status: 201, statusText: "Made", headers: { "x-a": "1" } });
return [r.status, r.ok, r.statusText, r.headers.get("content-type"), r.headers.get("x-a"), r.bodyUsed, await r.text(), r.bodyUsed]`,
			`[201,true,"Made","text/plain;charset=UTF-8","1",false,"héllo",true]`},
		{"response used", `const r = new Response("a"); await r.text(); try { await r.text() } catch (e) { return e.name }`, `TypeError`},
		{"response json", `return await Response.json({ a: [1, 2] }).json()`, `{"a":[1,2]}`},
		{"response json content type", `return Response.json(1).headers.get("content-type")`, `application/json`},
		{"response bytes", `const r = new Response(new Uint8Array([104, 105])); const b = await r.clone().bytes();
return [b instanceof Uint8Array, [...b], new Uint8Array(await r.arrayBuffer()).length]`, `[true,[104,105],2]`},
		{"response stream body", `const r = new Response(new ReadableStream({ start(c) { c.enqueue(new TextEncoder().encode("ab")); c.enqueue(new TextEncoder().encode("c")); c.close(); } }));
return await r.text()`, `abc`},
		{"response body stream", `const r = new Response("xyz"); const rd = r.body.getReader(); const a = await rd.read(); const b = await rd.read();
return [new TextDecoder().decode(a.value), b.done, r.bodyUsed]`, `["xyz",true,true]`},
		{"response clone stream", `const r = new Response(new ReadableStream({ start(c) { c.enqueue(new Uint8Array([49])); c.close(); } }));
const c = r.clone(); return [await r.text(), await c.text()]`, `["1","1"]`},
		{"response null body", `const r = new Response(null, { status: 204 }); return [r.body, await r.text()]`, `[null,""]`},
		{"response bad status", `try { new Response("", { status: 99 }) } catch (e) { return e.name }`, `RangeError`},
		{"response body with 204", `try { new Response("x", { status: 204 }) } catch (e) { return e.name }`, `TypeError`},
		{"response error and redirect", `const e = Response.error(); const r = Response.redirect("https://a.example/x", 301);
return [e.type, e.status, r.status, r.headers.get("location")]`, `["error",0,301,"https://a.example/x"]`},
		{"invalid json", `try { await new Response("{").json() } catch (e) { return e.name }`, `SyntaxError`},
		{"invalid utf8", `return await new Response(new Uint8Array([0x61, 0xff, 0xe2, 0x82, 0x62])).text()`, "a��b"},
		{"bom", `return await new Response(new Uint8Array([0xef, 0xbb, 0xbf, 0x61])).text()`, `a`},
		{"request", `const r = new Request("https://a.example/p?q#f", { method: "post", body: "x", headers: { "x-a": "1" } });
return [r.method, r.url, r.headers.get("x-a"), r.headers.get("content-type"), r.redirect, r.signal.aborted, await r.text()]`,
			`["POST","https://a.example/p?q#f","1","text/plain;charset=UTF-8","follow",false,"x"]`},
		{"request get body", `try { new Request("https://a.example", { body: "x" }) } catch (e) { return e.name }`, `TypeError`},
		{"request relative url", `try { new Request("/x") } catch (e) { return e.name }`, `TypeError`},
		{"request from request", `const a = new Request("https://a.example", { method: "PUT", body: "b" });
const b = new Request(a, { headers: { y: "2" } }); return [b.method, b.url, b.headers.get("y"), a.bodyUsed, await b.text()]`,
			`["PUT","https://a.example/","2",true,"b"]`},
		{"request signal follows", `const ac = new AbortController(); const r = new Request("https://a.example", { signal: ac.signal });
ac.abort("x"); return [r.signal.aborted, r.signal.reason]`, `[true,"x"]`},
		{"request forbidden method", `try { new Request("https://a.example", { method: "CONNECT" }) } catch (e) { return e.name }`, `TypeError`},
		{"form body", `const r = new Request("https://a.example", { method: "POST", body: new URLSearchParams({ a: "1 2", b: "&" }) });
return [r.headers.get("content-type"), await r.text()]`, `["application/x-www-form-urlencoded;charset=UTF-8","a=1+2&b=%26"]`},
	})
}

func TestURL(t *testing.T) {
	runCases(t, []jsCase{
		{"parts", `const u = new URL("HTTPS://user:pw@Example.COM:8080/a/../b?x=1#h");
return [u.href, u.protocol, u.username, u.password, u.host, u.hostname, u.port, u.pathname, u.search, u.hash, u.origin]`,
			`["https://user:pw@example.com:8080/b?x=1#h","https:","user","pw","example.com:8080","example.com","8080","/b","?x=1","#h","https://example.com:8080"]`},
		{"base", `return [new URL("../c", "http://a.example/x/y/z").href, new URL("//b.example", "https://a.example").href]`,
			`["http://a.example/x/c","https://b.example/"]`},
		{"invalid", `try { new URL("nope") } catch (e) { return [e.name, URL.canParse("nope"), URL.canParse("a:b"), URL.parse("x"), URL.parse("http://a/").href] }`,
			`["TypeError",false,true,null,"http://a/"]`},
		{"setters", `const u = new URL("http://a.example/p"); u.pathname = "/q r"; u.search = "z=1"; u.hash = "x"; u.port = "81"; u.protocol = "https";
return u.href`, `https://a.example:81/q%20r?z=1#x`},
		{"searchParams", `const u = new URL("http://a.example/?a=1&b=2&a=3"); const p = u.searchParams;
const got = [p.get("a"), p.getAll("a"), p.has("b"), p.size];
p.append("c", "x y"); p.delete("a"); got.push(u.search); u.search = "?d=4"; got.push(p.get("d"), p === u.searchParams);
p.delete("d"); got.push(u.href); return got`,
			`["1",["1","3"],true,3,"?b=2&c=x+y","4",true,"http://a.example/"]`},
		{"URLSearchParams", `const p = new URLSearchParams("?b=2&a=1&a=0"); p.sort(); const q = new URLSearchParams([["x", "1"]]);
const r = new URLSearchParams({ y: "é" }); p.set("a", "9");
return [p.toString(), q.toString(), r.toString(), [...p.keys()], p.has("a", "9"), String(new URLSearchParams("a=%zz&b=%C3%A9+"))]`,
			`["a=9&b=2","x=1","y=%C3%A9",["a","b"],true,"a=%25zz&b=%C3%A9+"]`},
		{"toJSON", `return JSON.stringify({ u: new URL("http://a.example") })`, `{"u":"http://a.example/"}`},
	})
}

func TestURLInternational(t *testing.T) {
	runCases(t, []jsCase{
		{"idna", `return [new URL("https://Bücher.example/").host, new URL("https://xn--bcher-kva.example/").hostname, new URL("http://ＥＸＡＭＰＬＥ。com/").host]`,
			`["xn--bcher-kva.example","xn--bcher-kva.example","example.com"]`},
		{"nfc", `return new URL("http://é.example/").host === new URL("http://é.example/").host`, `true`},
	})
}

// TestPolyfillGuidance checks what the guide says about polyfills: with the
// shared frozen builtins, assigning TextEncoder throws in strict code and a
// guarded polyfill keeps moejs's own, while the package's globals can be
// replaced; with MutableIntrinsics the assignment works.
func TestPolyfillGuidance(t *testing.T) {
	src := `"use strict";
const out = [];
try { globalThis.TextEncoder = class {}; out.push("assigned") } catch (e) { out.push(e.name + ": " + e.message) }
if (typeof globalThis.TextEncoder === "undefined") globalThis.TextEncoder = class {};
out.push(typeof new TextEncoder().encode);
globalThis.ReadableStream = class Polyfill {}; out.push(ReadableStream.name);
return out`
	h := newHarness(t, webapi.Options{})
	assert.Equal(t, `["TypeError: Cannot assign to read only property 'TextEncoder' of object","function","Polyfill"]`, h.run(src))
	h = newHarnessRT(t, moejs.NewRuntime(moejs.Options{MutableIntrinsics: true}), webapi.Options{})
	assert.Equal(t, `["assigned","undefined","Polyfill"]`, h.run(src))
}

// TestHeadersRefuseControlCharacters checks that CR, LF and NUL in header
// names and values, and spaces in names, are refused.
func TestHeadersRefuseControlCharacters(t *testing.T) {
	h := newHarness(t, webapi.Options{})
	got := h.run(`const out = [];
for (const [k, v] of [["x", "a\r\nB: c"], ["x\r\ny", "1"], ["x", "a\nb"], ["x", "a\0b"], ["x y", "1"]]) {
  try { new Headers([[k, v]]); out.push("accepted") } catch (e) { out.push(e.name) }
}
return out`)
	assert.Equal(t, `["TypeError","TypeError","TypeError","TypeError","TypeError"]`, got)
}

// heapAfterGC returns the live heap.
func heapAfterGC() int64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// TestAbortSignalAnyCollected checks that the signals AbortSignal.any made
// and JavaScript dropped are collected, as plain signals are, while a
// long-lived source signal lives on; one with an abort listener lives as
// long as its sources and fires.
func TestAbortSignalAnyCollected(t *testing.T) {
	h := newHarness(t, webapi.Options{})
	h.run(`globalThis.root = new AbortController(); return 1`)
	grow := func(body string) int64 {
		before := heapAfterGC()
		h.run(body + `; return 1`)
		h.run(`let x = 0; for (let i = 0; i < 1000; i++) x += [i].length; return x`)
		return heapAfterGC() - before
	}
	plain := grow(`for (let i = 0; i < 200000; i++) new AbortController().signal`)
	any := grow(`for (let i = 0; i < 200000; i++) AbortSignal.any([root.signal])`)
	t.Logf("heap growth after 200k dropped signals: plain %d KiB, AbortSignal.any %d KiB", plain>>10, any>>10)
	assert.Less(t, any, int64(8<<20), "dropped dependent signals are retained")
	h.run(`globalThis.fired = 0;
for (let i = 0; i < 100; i++) AbortSignal.any([root.signal]).addEventListener("abort", () => fired++);
const s = AbortSignal.any([root.signal]); s.onabort = () => fired++; return 1`)
	heapAfterGC()
	assert.Equal(t, "101", h.run(`root.abort(); return fired`))
}

// TestMutableIntrinsics checks that with MutableIntrinsics the package
// uses the builtins the runtime had when Install ran: a script that
// deletes or patches them later changes nothing of the package, and a
// builtin missing then is a TypeError, not a panic.
func TestMutableIntrinsics(t *testing.T) {
	srv := newServer(t)
	for _, c := range []struct{ name, del, body, want string }{
		{"text", "TextEncoder", `return await new Response("x").text()`, "x"},
		{"read-in-task", "Uint8Array", `const r = await fetch(BASE + "/chunks?n=1&size=3"); const { value } = await r.body.getReader().read(); return value.length`, "3"},
		{"decoder", "TextDecoder", `const ts = new TextDecoderStream(); const w = ts.writable.getWriter(); w.write(new Uint8Array([104, 105])); w.close();
let s = ""; for await (const x of ts.readable) s += x; return s`, "hi"},
		{"promise", "Promise", `return await new Response("abc").text()`, "abc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := &http.Transport{}
			t.Cleanup(tr.CloseIdleConnections)
			h := newHarnessRT(t, moejs.NewRuntime(moejs.Options{MutableIntrinsics: true}), webapi.Options{Client: &http.Client{Transport: tr}})
			require.NoError(t, h.rt.SetGlobal("BASE", srv.URL))
			var got string
			var err error
			assert.NotPanics(t, func() { got, err = h.runErr(`delete globalThis.` + c.del + `; ` + c.body) })
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
	h := newHarnessRT(t, moejs.NewRuntime(moejs.Options{MutableIntrinsics: true}), webapi.Options{})
	// The harness's own then call happens at the first await.
	got := h.run(`await null; const orig = Promise.prototype.then; let calls = 0;
Promise.prototype.then = function (a, b) { calls++; return orig.call(this, a, b) };
const rs = new ReadableStream({ start(c) { c.enqueue("a"); c.close() } });
const ws = new WritableStream({ write() {} });
await rs.pipeTo(ws);
Promise.prototype.then = orig;
return calls`)
	assert.Equal(t, "0", got, "the package called the patched Promise.prototype.then")
	// A runtime that lost Uint8Array before Install.
	rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: true})
	s, err := moejs.CompileScript("del.js", `delete globalThis.Uint8Array`)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	h = newHarnessRT(t, rt, webapi.Options{})
	assert.Equal(t, `"TypeError: The runtime has no Uint8Array"`, h.run(`try { await new Response("x").bytes() } catch (e) { return JSON.stringify(e.name + ": " + e.message) }`))
	// The shared frozen builtins cannot be deleted.
	h = newHarness(t, webapi.Options{})
	assert.Equal(t, `[false,false,false,"function"]`, h.run(`return [delete globalThis.Uint8Array, delete globalThis.TextDecoder, delete globalThis.Promise, typeof Uint8Array]`))
}

// TestAbortSignalTimeoutLongDelay checks a delay longer than a Go timer
// holds: the signal does not abort.
func TestAbortSignalTimeoutLongDelay(t *testing.T) {
	h := newHarness(t, webapi.Options{})
	got := h.run(`const s = AbortSignal.timeout(1e13), t = AbortSignal.timeout(2 ** 53 - 1);
await new Promise(r => setTimeout(r, 50)); return [s.aborted, t.aborted]`)
	assert.Equal(t, `[false,false]`, got)
}

// TestCloseReleasesTimeoutSignal checks that Close stops the timers of
// AbortSignal.timeout, which then do not keep the runtime.
func TestCloseReleasesTimeoutSignal(t *testing.T) {
	collected := func(script string) bool {
		var gone atomic.Bool
		func() {
			rt := moejs.NewRuntime(moejs.Options{})
			loop, err := eventloop.New(rt, eventloop.Options{})
			require.NoError(t, err)
			api, err := webapi.Install(loop, webapi.Options{})
			require.NoError(t, err)
			s, err := moejs.CompileScript("t.js", script)
			require.NoError(t, err)
			require.NoError(t, loop.Run(func(rt *moejs.Runtime) error { _, err := rt.RunScript(s); return err }))
			api.Close()
			runtime.AddCleanup(rt, func(b *atomic.Bool) { b.Store(true) }, &gone)
		}()
		for range 50 {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
			if gone.Load() {
				return true
			}
		}
		return false
	}
	require.True(t, collected(`new AbortController(); 1`))
	assert.True(t, collected(`AbortSignal.timeout(600000).addEventListener("abort", () => {}); 1`), "a pending AbortSignal.timeout keeps the closed runtime")
}

// TestTwoRuntimesShareNothing checks that two runtimes with the shared
// frozen builtins and the package share no object of the package, and that
// Install changes nothing they share.
func TestTwoRuntimesShareNothing(t *testing.T) {
	a := newHarness(t, webapi.Options{})
	b := newHarness(t, webapi.Options{})
	got := a.run(`"use strict";
const out = [];
ReadableStream.prototype.x = 1; Headers.prototype.y = 2; DOMException.prototype.z = 3;
const it = new ReadableStream().values();
const asyncItProto = Object.getPrototypeOf(Object.getPrototypeOf(it));
try { asyncItProto.w = 4; out.push("asyncIteratorPrototype mutable") } catch (e) { out.push(e.name) }
const hit = new Headers().entries();
const itProto = Object.getPrototypeOf(Object.getPrototypeOf(hit));
try { itProto.v = 5; out.push("IteratorPrototype mutable") } catch (e) { out.push(e.name) }
out.push(Object.isFrozen(Object.getPrototypeOf(DOMException.prototype)));
out.push(Object.getPrototypeOf(DOMException.prototype) === Error.prototype);
return out`)
	assert.Equal(t, `["TypeError","TypeError",true,true]`, got)
	got = b.run(`const it = new ReadableStream().values();
return [ReadableStream.prototype.x, Headers.prototype.y, DOMException.prototype.z,
  Object.getPrototypeOf(Object.getPrototypeOf(it)).w, Object.getPrototypeOf(Object.getPrototypeOf(new Headers().entries())).v,
  ReadableStream === globalThis.ReadableStream]`)
	assert.Equal(t, `[null,null,null,null,null,true]`, got)
}

// TestPanicInTask checks that a Go panic in the jobs of a task of the
// package does not escape loop.Run: it is an *moejs.InternalError, as one
// below a call of the runtime is.
func TestPanicInTask(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	api, err := webapi.Install(loop, webapi.Options{})
	require.NoError(t, err)
	defer api.Close()
	require.NoError(t, rt.SetGlobal("boom", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		panic("boom")
	})))
	s, err := moejs.CompileScript("panic.js", `(async () => {
const keep = setTimeout(() => {}, 1000);
await new Promise(r => AbortSignal.timeout(1).addEventListener("abort", r));
clearTimeout(keep);
boom();
})()`)
	require.NoError(t, err)
	var ie *moejs.InternalError
	assert.NotPanics(t, func() {
		err = loop.Run(func(rt *moejs.Runtime) error { _, err := rt.RunScript(s); return err })
	})
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "boom", ie.Value)
}

// TestBodyTextMatchesTextDecoder checks that text() decodes invalid UTF-8
// as the runtime's TextDecoder does, on random byte strings.
func TestBodyTextMatchesTextDecoder(t *testing.T) {
	h := newHarness(t, webapi.Options{})
	got := h.run(`let seed = 12345; const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff;
const pool = [0x00, 0x41, 0x7f, 0x80, 0xbf, 0xc0, 0xc1, 0xc2, 0xdf, 0xe0, 0xe1, 0xed, 0xef, 0xf0, 0xf1, 0xf4, 0xf5, 0xff, 0x9f, 0xa0, 0x8f, 0x90];
const dec = new TextDecoder(); let bad = 0, first = null;
for (let i = 0; i < 20000; i++) {
  const n = 1 + Math.floor(rnd() * 8); const b = new Uint8Array(n);
  for (let j = 0; j < n; j++) b[j] = pool[Math.floor(rnd() * pool.length)];
  const a = await new Response(b).text(); const want = dec.decode(b);
  if (a !== want) { bad++; if (!first) first = [Array.from(b), a, want] }
}
return [bad, first]`)
	assert.Equal(t, `[0,null]`, got)
}

// TestPanicInTaskRestoresCallState checks a loop with OnError after a Go
// panic in the jobs of a task of the package: the loop goes on, with the
// call depth it had before, and streams and promises still work.
func TestPanicInTaskRestoresCallState(t *testing.T) {
	var mu sync.Mutex
	var errs []error
	var result string
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{OnError: func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }})
	require.NoError(t, err)
	api, err := webapi.Install(loop, webapi.Options{})
	require.NoError(t, err)
	defer api.Close()
	require.NoError(t, rt.SetGlobal("boom", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		panic("boom")
	})))
	require.NoError(t, rt.SetGlobal("done", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		mu.Lock()
		result = moejs.Arg(args, 0).String()
		mu.Unlock()
		return moejs.Undefined(), nil
	})))
	s, err := moejs.CompileScript("guard.js", `
const depth = () => { let n = 0; const f = () => { n++; f() }; try { f() } catch (e) {} return n };
let before;
setTimeout(() => { before = depth() }, 0);
(async () => {
  await new Promise(r => AbortSignal.timeout(10).addEventListener("abort", r));
  const deep = n => n ? deep(n - 1) : boom();
  deep(200);
})();
setTimeout(async () => {
  const after = depth();
  const rs = new ReadableStream({ start(c) { c.enqueue("x"); c.close() } });
  const v = (await rs.getReader().read()).value;
  done(JSON.stringify([before === after, v]));
}, 50);`)
	require.NoError(t, err)
	require.NoError(t, loop.Run(func(rt *moejs.Runtime) error { _, err := rt.RunScript(s); return err }))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, errs, 1)
	var ie *moejs.InternalError
	assert.ErrorAs(t, errs[0], &ie)
	assert.Equal(t, `[true,"x"]`, result)
}

// TestConcurrentInstall checks the first Installs of a process running
// concurrently, with shared and mutable builtins (for -race), and the
// globals in each runtime.
func TestConcurrentInstall(t *testing.T) {
	var wg sync.WaitGroup
	res := make([]string, 16)
	for i := range res {
		wg.Go(func() {
			rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: i%2 == 0})
			loop, err := eventloop.New(rt, eventloop.Options{})
			if err != nil {
				res[i] = err.Error()
				return
			}
			api, err := webapi.Install(loop, webapi.Options{})
			if err != nil {
				res[i] = err.Error()
				return
			}
			defer api.Close()
			s, err := moejs.CompileScript("x.js", `[typeof fetch, new URL("http://a/b?c=1").searchParams.get("c"), typeof ReadableStream, new Headers({a: "1"}).get("a")].join()`)
			if err != nil {
				res[i] = err.Error()
				return
			}
			_ = loop.Run(func(rt *moejs.Runtime) error {
				v, err := rt.RunScript(s)
				if err != nil {
					res[i] = err.Error()
					return nil
				}
				res[i] = v.String()
				return nil
			})
		})
	}
	wg.Wait()
	for i, r := range res {
		assert.Equal(t, "function,1,function,1", r, "runtime %d", i)
	}
}

// TestRequestMethodLimit checks that a Request's method longer than 4 KiB
// is a TypeError, and that one of 4 KiB is taken.
func TestRequestMethodLimit(t *testing.T) {
	h := newHarness(t, webapi.Options{})
	assert.Equal(t, `["TypeError: Failed to construct 'Request': the HTTP method is longer than 4096 bytes.",4096]`,
		h.run(`const out = [];
try { new Request("http://a/", { method: "X".repeat(4 << 20) }); out.push("made") } catch (e) { out.push(e.name + ": " + e.message) }
out.push(new Request("http://a/", { method: "X".repeat(4096) }).method.length);
return out`))
}

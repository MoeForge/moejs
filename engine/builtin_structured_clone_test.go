package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStructuredClone(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"function", `return [structuredClone.length, structuredClone.name, typeof globalThis.structuredClone].join()`, "1,structuredClone,function"},
		{"primitives", `return [structuredClone(1), structuredClone("s"), structuredClone(null) === null, structuredClone(undefined) === undefined, structuredClone(true), typeof structuredClone(1n), 1 / structuredClone(-0), isNaN(structuredClone(NaN))].join()`, "1,s,true,true,true,bigint,-Infinity,true"},
		{"no argument", `return structuredClone()`, "!TypeError: structuredClone requires 1 argument"},
		{"deep copy", `const o = {a: 1, b: {c: [1, 2, {d: "x"}]}}; const c = structuredClone(o); return [c !== o, c.b !== o.b, c.b.c !== o.b.c, JSON.stringify(c)].join("|")`, `true|true|true|{"a":1,"b":{"c":[1,2,{"d":"x"}]}}`},
		{"cycles and shared references", `const o = {}; o.self = o; const s = {}; o.x = s; o.y = s; const a = [o]; o.arr = a; const c = structuredClone(o); return [c.self === c, c.x === c.y, c.x !== s, c.arr[0] === c].join()`, "true,true,true,true"},
		{"ordinary object", `function F() { this.a = 1; } F.prototype.p = 2; const o = new F(); Object.defineProperty(o, "h", {value: 3, enumerable: false}); const s = Symbol("s"); o[s] = 4; const c = structuredClone(o); return [Object.getPrototypeOf(c) === Object.prototype, c.a, c.p === undefined, "h" in c, s in c].join()`, "true,1,true,false,false"},
		{"null prototype", `const o = Object.create(null); o.a = 1; o[2] = "i"; const c = structuredClone(o); return [Object.getPrototypeOf(c) === Object.prototype, c.a, c[2], Object.keys(c).join()].join()`, "true,1,i,2,a"},
		{"getter becomes data", `let n = 0; const o = {get g() { n++; return {v: 1}; }}; const c = structuredClone(o); const d = Object.getOwnPropertyDescriptor(c, "g"); return [n, typeof d.get, d.value.v, d.writable, d.enumerable, d.configurable].join()`, "1,undefined,1,true,true,true"},
		{"getters depth first", `const log = []; const o = {get a() { log.push("a"); return {get b() { log.push("b"); return 1; }}; }, get c() { log.push("c"); return 2; }}; structuredClone(o); return log.join()`, "a,b,c"},
		{"getter deletes a later key", `const o = {get a() { delete this.b; return 1; }, b: 2, c: 3}; const c = structuredClone(o); return ["b" in c, c.c].join()`, "false,3"},
		{"getter adds a key", `const o = {get a() { this.z = 1; return 1; }}; return "z" in structuredClone(o)`, "false"},
		{"getter throws", `return structuredClone({get a() { throw new RangeError("g"); }})`, "!RangeError: g"},
		{"array", `const a = [1, , 3]; a.length = 5; a.foo = "f"; const c = structuredClone(a); return [Array.isArray(c), c.length, 1 in c, c[2], c.foo, c !== a].join()`, "true,5,false,3,f,true"},
		{"huge sparse array", `const a = []; a[4294967294] = 1; const c = structuredClone(a); return [c.length, c[4294967294], Object.keys(c).length].join()`, "4294967295,1,1"},
		{"array getter", `const a = [1, 2]; Object.defineProperty(a, 0, {get() { a.length = 1; return "g"; }, enumerable: true}); const c = structuredClone(a); return [c.length, c[0], 1 in c].join()`, "2,g,false"},
		{"wrappers", `const c = [structuredClone(new Number(3)), structuredClone(new String("ab")), structuredClone(new Boolean(false))]; return c.map(x => typeof x + ":" + String(x.valueOf())).join()`, "object:3,object:ab,object:false"},
		{"bigint wrapper", `const b = Object(2n); const c = structuredClone(b); return [c !== b, typeof c, Object.prototype.toString.call(c), Object.getPrototypeOf(c) === Object.getPrototypeOf(b), c.valueOf() === 2n].join()`, "true,object,[object BigInt],true,true"},
		{"wrapper properties dropped", `const s = new String("ab"); s.x = 1; const c = structuredClone(s); return [c.x === undefined, c.length, c instanceof String].join()`, "true,2,true"},
		{"date", `const d = new Date(5); d.x = 1; const c = structuredClone(d); return [c !== d, c.getTime(), c instanceof Date, c.x === undefined, isNaN(structuredClone(new Date(NaN)).getTime())].join()`, "true,5,true,true,true"},
		{"deferred builtin objects", `return JSON.stringify([structuredClone(Map.prototype), structuredClone(Reflect), structuredClone(Set.prototype.add.call(new Set, 1))])`, `[{},{},{}]`},
		{"regexp", `const r = /a+/gi; r.lastIndex = 3; r.x = 1; const c = structuredClone(r); return [c !== r, c.source, c.flags, c.lastIndex, c.x === undefined, c.test("AA")].join()`, "true,a+,gi,0,true,true"},
		{"map", `const k = {}; const m = new Map([[k, {v: 1}], ["s", k]]); m.extra = 1; const c = structuredClone(m); const ck = [...c.keys()][0]; return [c instanceof Map, c.size, ck !== k, c.get(ck).v, c.get("s") === ck, c.extra === undefined].join()`, "true,2,true,1,true,true"},
		{"map cycle", `const m = new Map(); m.set(m, m); m.set("x", [m]); const c = structuredClone(m); return [c.get(c) === c, c.get("x")[0] === c, c.has(m)].join()`, "true,true,false"},
		{"map key before value", `const log = []; const k = {get a() { log.push("k"); return 1; }}; const v = {get b() { log.push("v"); return 2; }}; structuredClone(new Map([[k, v], [1, 2]])); return log.join()`, "k,v"},
		{"map entries snapshot", `const m = new Map([[1, {get g() { m.set(2, 2); m.delete(3); return 1; }}], [3, 3]]); const c = structuredClone(m); return [c.size, [...c.keys()].join(), m.size].join("|")`, "2|1,3|2"},
		{"set", `const o = {}; const s = new Set([1, o, "x", o]); const c = structuredClone([s, o]); const items = [...c[0]]; return [c[0] instanceof Set, c[0].size, items[1] === c[1], items[1] !== o, items[0], items[2]].join()`, "true,3,true,true,1,x"},
		{"set of equal copies", `const s = new Set([{}, {}]); return structuredClone(s).size`, "2"},
		{"grown map and set", `const s = new Set(); for (let i = 0; i < 300; i++) s.add(i); for (let i = 0; i < 300; i += 7) s.delete(i); const m = new Map(); for (let i = 0; i < 1000; i++) m.set("k" + i, i); m.delete("k999"); m.delete("k3"); const cs = structuredClone(s), cm = structuredClone(m); return [cs.size, [...cs].join() === [...s].join(), cm.size, [...cm].join() === [...m].join(), structuredClone(new Set([0, 1, 2, 3, 4, 5, 6, 7, 8])).size].join()`, "257,true,998,true,9"},
		{"error", `const e = new RangeError("boom"); e.extra = 1; const c = structuredClone(e); const d = Object.getOwnPropertyDescriptor(c, "message"); return [c !== e, c instanceof RangeError, c.name, c.message, c.extra === undefined, d.enumerable, d.writable, d.configurable].join()`, "true,true,RangeError,boom,true,false,true,true"},
		{"error kinds", `return [EvalError, RangeError, ReferenceError, SyntaxError, TypeError, URIError, Error].map(E => Object.getPrototypeOf(structuredClone(new E("m"))) === E.prototype).join()`, "true,true,true,true,true,true,true"},
		{"error name", `const e = new TypeError("t"); e.name = "Custom"; const c = structuredClone(e); const a = structuredClone(new AggregateError([], "ag")); return [Object.getPrototypeOf(c) === Error.prototype, c.message, Object.getPrototypeOf(a) === Error.prototype, a.message, "errors" in a].join()`, "true,t,true,ag,false"},
		{"error renamed to native", `const e = new Error("x"); e.name = "URIError"; return structuredClone(e) instanceof URIError`, "true"},
		{"error message", `const e = new Error(); const f = new Error(); f.message = 42; const g = new Error("m"); Object.defineProperty(g, "message", {get() { return "no"; }}); return [Object.prototype.hasOwnProperty.call(structuredClone(e), "message"), typeof structuredClone(f).message, Object.prototype.hasOwnProperty.call(structuredClone(g), "message")].join()`, "false,string,false"},
		{"error cause dropped", `return "cause" in structuredClone(new Error("x", {cause: 1}))`, "false"},
		{"error in object", `const e = new Error("m"); const c = structuredClone({a: e, b: e}); return [c.a === c.b, c.a.message].join()`, "true,m"},
		{"function throws", `return structuredClone(() => 1)`, "!TypeError: DataCloneError"},
		{"nested function throws", `return structuredClone({a: {f() {}}})`, "!TypeError: DataCloneError"},
		{"symbol throws", `return structuredClone(Symbol("s"))`, "!TypeError: DataCloneError"},
		{"symbol value throws", `return structuredClone({a: Symbol()})`, "!TypeError: DataCloneError"},
		{"symbol wrapper throws", `return structuredClone(Object(Symbol()))`, "!TypeError: DataCloneError"},
		{"WeakMap throws", `return structuredClone(new WeakMap())`, "!TypeError: DataCloneError"},
		{"WeakRef throws", `return structuredClone(new WeakRef({}))`, "!TypeError: DataCloneError"},
		{"iterator throws", `return structuredClone([].values())`, "!TypeError: DataCloneError"},
		{"map with function throws", `return structuredClone(new Map([[1, () => 0]]))`, "!TypeError: DataCloneError"},
		{"options", `return [structuredClone(1, null), structuredClone(1, {}), structuredClone(1, {transfer: []}), structuredClone(1, {transfer: undefined})].join()`, "1,1,1,1"},
		{"options not an object", `return structuredClone(1, 1)`, "!TypeError"},
		{"transfer not iterable", `return structuredClone(1, {transfer: {}})`, "!TypeError"},
		{"transfer primitive element", `return structuredClone(1, {transfer: [1]})`, "!TypeError"},
		{"transfer object", `return structuredClone(1, {transfer: [{}]})`, "!TypeError: DataCloneError"},
		{"transfer view", `return structuredClone(1, {transfer: [new Uint8Array(1)]})`, "!TypeError: DataCloneError"},
		{"deep object", `let o = {}; const root = o; for (let i = 0; i < 100000; i++) { o = o.n = {}; } let c = structuredClone(root); let d = 0; while (c.n) { c = c.n; d++; } return d`, "100000"},
		{"deep array", `let a = []; const root = a; for (let i = 0; i < 100000; i++) { const b = []; a.push(b); a = b; } let c = structuredClone(root); let d = 0; while (c.length) { c = c[0]; d++; } return d`, "100000"},
		{"deep map", `let m = new Map(); const root = m; for (let i = 0; i < 50000; i++) { const n = new Map(); m.set(i, n); m = n; } let c = structuredClone(root); let d = 0; while (c.size) { c = c.get(d); d++; } return d`, "50000"},
	})
}

func TestStructuredCloneBinary(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"buffer", `const b = new ArrayBuffer(4); new Uint8Array(b).set([1, 2, 3, 4]); b.x = 1; const c = structuredClone(b); new Uint8Array(b)[0] = 9; return [c !== b, Object.getPrototypeOf(c) === ArrayBuffer.prototype, c.byteLength, new Uint8Array(c).join(), c.resizable, c.x === undefined].join()`, "true,true,4,1,2,3,4,false,true"},
		{"empty buffer", `const c = structuredClone(new ArrayBuffer(0)); return [c.byteLength, c.detached].join()`, "0,false"},
		{"buffer subclass", `class B extends ArrayBuffer {} const c = structuredClone(new B(2)); return [Object.getPrototypeOf(c) === ArrayBuffer.prototype, c.byteLength].join()`, "true,2"},
		{"resizable buffer", `const b = new ArrayBuffer(2, {maxByteLength: 8}); new Uint8Array(b)[1] = 5; const c = structuredClone(b); c.resize(8); return [c.resizable, c.maxByteLength, c.byteLength, new Uint8Array(c).join(), b.byteLength].join()`, "true,8,8,0,5,0,0,0,0,0,0,2"},
		{"shared buffer", `const b = new SharedArrayBuffer(2); b.x = 1; const c = structuredClone(b); new Uint8Array(b)[0] = 7; new Uint8Array(c)[1] = 8; return [c !== b, c instanceof SharedArrayBuffer, new Uint8Array(c).join(), new Uint8Array(b).join(), c.x === undefined].join()`, "true,true,7,8,7,8,true"},
		{"growable shared buffer", `const b = new SharedArrayBuffer(1, {maxByteLength: 4}); const c = structuredClone(b); b.grow(3); new Uint8Array(c)[2] = 1; return [c.growable, c.maxByteLength, c.byteLength, new Uint8Array(b).join()].join()`, "true,4,3,0,0,1"},
		{"shared buffer twice", `const b = new SharedArrayBuffer(1); const c = structuredClone([b, b]); return [c[0] === c[1], c[0] !== b].join()`, "true,true"},
		{"detached buffer", `const b = new ArrayBuffer(1); b.transfer(); return structuredClone({b})`, "!TypeError: DataCloneError"},
		{"typed arrays", `const out = []; for (const T of [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array, Float16Array, Float32Array, Float64Array]) { const a = new T([1, -2, 3.5]); const c = structuredClone(a); a[0] = 0; out.push(Object.getPrototypeOf(c) === T.prototype && c.buffer !== a.buffer ? c.join(" ") : "bad"); } for (const T of [BigInt64Array, BigUint64Array]) { const c = structuredClone(new T([1n, 2n])); out.push(Object.getPrototypeOf(c) === T.prototype ? c.join(" ") : "bad"); } return out.join()`, "1 -2 3,1 254 3,1 0 4,1 -2 3,1 65534 3,1 -2 3,1 4294967294 3,1 -2 3.5,1 -2 3.5,1 -2 3.5,1 2,1 2"},
		{"typed array view", `const b = new ArrayBuffer(8); new Uint8Array(b).set([1, 2, 3, 4, 5, 6, 7, 8]); const a = new Uint16Array(b, 2, 2); a.x = 1; const c = structuredClone(a); return [c.byteOffset, c.length, c.buffer.byteLength, new Uint8Array(c.buffer).join(""), c.x === undefined].join()`, "2,2,8,12345678,true"},
		{"typed array subclass", `class U extends Uint8Array {} const c = structuredClone(new U([1])); return [Object.getPrototypeOf(c) === Uint8Array.prototype, c[0]].join()`, "true,1"},
		{"length-tracking typed array", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const c = structuredClone(new Uint8Array(b, 1)); c.buffer.resize(8); return [c.length, c.byteOffset].join()`, "7,1"},
		{"fixed typed array over resizable", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const c = structuredClone(new Uint8Array(b, 1, 2)); c.buffer.resize(8); return c.length`, "2"},
		{"out-of-bounds typed array", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const a = new Uint8Array(b, 2, 2); b.resize(3); return structuredClone(a)`, "!TypeError: DataCloneError"},
		{"typed array of detached buffer", `const a = new Uint8Array(2); a.buffer.transfer(); return structuredClone(a)`, "!TypeError: DataCloneError"},
		{"dataview", `const b = new ArrayBuffer(6); const v = new DataView(b, 1, 4); v.setInt16(0, -2); v.x = 1; const c = structuredClone(v); return [Object.getPrototypeOf(c) === DataView.prototype, c.byteOffset, c.byteLength, c.getInt16(0), c.buffer.byteLength, c.buffer !== b, c.x === undefined].join()`, "true,1,4,-2,6,true,true"},
		{"length-tracking dataview", `const b = new ArrayBuffer(2, {maxByteLength: 4}); const c = structuredClone(new DataView(b)); c.buffer.resize(4); return c.byteLength`, "4"},
		{"out-of-bounds dataview", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const v = new DataView(b, 3); b.resize(2); return structuredClone(v)`, "!TypeError: DataCloneError"},
		{"dataview over shared buffer", `const b = new SharedArrayBuffer(2); const c = structuredClone(new DataView(b)); c.setUint8(0, 3); return new Uint8Array(b)[0]`, "3"},
		{"views share one buffer", `const b = new ArrayBuffer(8); const o = {u8: new Uint8Array(b), i16: new Int16Array(b, 2, 1), dv: new DataView(b, 4), b}; const c = structuredClone(o); c.u8[2] = 1; return [c.u8.buffer === c.i16.buffer, c.dv.buffer === c.b, c.b === c.u8.buffer, c.i16[0], new Uint8Array(b)[2]].join()`, "true,true,true,1,0"},
		{"view before buffer", `const b = new ArrayBuffer(2); const c = structuredClone([new Uint8Array(b), b]); return c[0].buffer === c[1]`, "true"},
		{"same view twice", `const a = new Uint8Array(1); const c = structuredClone([a, a]); return [c[0] === c[1], c[0] !== a].join()`, "true,true"},
		{"views in a map", `const b = new ArrayBuffer(1); const c = structuredClone(new Map([[b, new Uint8Array(b)]])); const [k, v] = [...c][0]; return v.buffer === k`, "true"},

		{"transfer", `const b = new ArrayBuffer(3); new Uint8Array(b).set([1, 2, 3]); const c = structuredClone(b, {transfer: [b]}); return [c !== b, b.detached, b.byteLength, c.detached, new Uint8Array(c).join(), c.resizable].join()`, "true,true,0,false,1,2,3,false"},
		{"transfer not in value", `const b = new ArrayBuffer(1); return [structuredClone(1, {transfer: [b]}), b.detached].join()`, "1,true"},
		{"transfer views", `const b = new ArrayBuffer(4); const u = new Uint8Array(b); u[0] = 5; const c = structuredClone({u, v: new DataView(b, 2), b}, {transfer: [b]}); return [c.u.buffer === c.b, c.v.buffer === c.b, c.u[0], c.v.byteLength, u.length, b.detached].join()`, "true,true,5,2,0,true"},
		{"transfer resizable", `const b = new ArrayBuffer(2, {maxByteLength: 8}); const c = structuredClone(b, {transfer: [b]}); c.resize(6); return [c.resizable, c.maxByteLength, c.byteLength].join()`, "true,8,6"},
		{"transfer iterable", `const b = new ArrayBuffer(1); structuredClone(b, {transfer: new Set([b])}); return b.detached`, "true"},
		{"transfer in list order", `const b1 = new ArrayBuffer(1), b2 = new ArrayBuffer(2); const c = structuredClone([b2, b1], {transfer: [b1, b2]}); return [c[0].byteLength, c[1].byteLength, b1.detached, b2.detached].join()`, "2,1,true,true"},
		{"transfer duplicate", `const b = new ArrayBuffer(1); try { structuredClone(b, {transfer: [b, b]}); } catch (e) { return [e instanceof TypeError, /DataCloneError/.test(e.message), b.detached].join(); }`, "true,true,false"},
		{"transfer shared buffer", `return structuredClone(1, {transfer: [new SharedArrayBuffer(1)]})`, "!TypeError: DataCloneError"},
		{"transfer detached", `const b = new ArrayBuffer(1); b.transfer(); return structuredClone(1, {transfer: [b]})`, "!TypeError: DataCloneError"},
		{"transfer clone fails", `const b = new ArrayBuffer(1); try { structuredClone([b, () => 0], {transfer: [b]}); } catch (e) { return [e instanceof TypeError, b.detached].join(); }`, "true,false"},
		{"transfer detached while cloning", `const b1 = new ArrayBuffer(1), b2 = new ArrayBuffer(1); try { structuredClone({get a() { b2.transfer(); return 1; }}, {transfer: [b1, b2]}); } catch (e) { return [/DataCloneError/.test(e.message), b1.detached].join(); }`, "true,true"},
		{"transfer resized while cloning", `const b = new ArrayBuffer(1, {maxByteLength: 4}); const c = structuredClone({get a() { b.resize(3); return b; }}, {transfer: [b]}); return c.a.byteLength`, "3"},
		{"transfer element conversion order", `const log = []; function* g() { log.push(1); yield new ArrayBuffer(1); log.push(2); yield 1; log.push(3); } try { structuredClone(1, {transfer: g()}); } catch (e) { return [e instanceof TypeError, log.join("")].join(); }`, "true,12"},
		{"transfer buffer of out-of-bounds view", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const u = new Uint8Array(b, 2, 2); b.resize(1); return structuredClone(u, {transfer: [b]})`, "!TypeError: DataCloneError"},
	})
}

func TestStructuredCloneTransferMoves(t *testing.T) {
	r := NewRealm()
	data := make([]byte, 16)
	src, err := r.NewArrayBuffer(data)
	require.NoError(t, err)
	opts := r.NewObject()
	_, err = opts.CreateDataProperty(r, StringKey(atomTransfer), ObjectValue(r.NewArrayFromSlice([]Value{ObjectValue(src)})))
	require.NoError(t, err)
	out, err := globalStructuredClone(r, Undefined(), []Value{ObjectValue(src), ObjectValue(opts)})
	require.NoError(t, err)
	got, ok := out.AsObject().BufferData()
	require.True(t, ok)
	require.Len(t, got, 16)
	require.Same(t, &data[0], &got[0], "the transfer moves the data block")
	require.True(t, src.IsDetachedBuffer())
}

func TestStructuredCloneInterrupt(t *testing.T) {
	r := NewRealm()
	items := make([]Value, 10000)
	for i := range items {
		items[i] = ObjectValue(r.NewObject())
	}
	r.Interrupt("stop")
	_, err := globalStructuredClone(r, Undefined(), []Value{ObjectValue(r.NewArrayFromSlice(items))})
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	r.ClearInterrupt()

	// Copying a large buffer checks too.
	buf, err := r.NewArrayBuffer(make([]byte, 4*copyBytesChunk))
	require.NoError(t, err)
	r.Interrupt("stop")
	_, err = globalStructuredClone(r, Undefined(), []Value{ObjectValue(buf)})
	require.ErrorAs(t, err, &ie)
	r.ClearInterrupt()
}

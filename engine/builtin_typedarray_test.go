package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTypedArrayConstruct(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"lengths", `return [new Uint8Array().length, new Uint8Array(3).length, new Float64Array(2).byteLength, new Int16Array(new ArrayBuffer(8), 2).length, new Int16Array(new ArrayBuffer(8), 2, 1).length, Uint8Array.BYTES_PER_ELEMENT, Float64Array.prototype.BYTES_PER_ELEMENT, BigInt64Array.BYTES_PER_ELEMENT].join()`, "0,3,16,3,1,1,8,8"},
		{"array-like", `return new Int8Array({length: 3, 0: 1, 1: 200, 2: "3"}).join()`, "1,-56,3"},
		{"iterable", `return new Uint16Array(new Set([1, 70000, -1])).join()`, "1,4464,65535"},
		{"typed array", `return new Float32Array(new Int8Array([-1, 2])).join() + "|" + new Uint8Array(new Float64Array([1.5, -1, 300])).join()`, "-1,2|1,255,44"},
		{"clamped", `return new Uint8ClampedArray([-5, 1.5, 2.5, 300, NaN, 254.5, 0.5]).join()`, "0,2,2,255,0,254,0"},
		{"bigint", `const a = new BigInt64Array([2n ** 63n, -1n]); const b = new BigUint64Array(a); return [a.join(), b.join(), typeof a[0]].join("|")`, "-9223372036854775808,-1|9223372036854775808,18446744073709551615|bigint"},
		{"bigint from numbers", `return new BigInt64Array(new Int8Array(1))`, "!TypeError"},
		{"bigint from number", `return new BigInt64Array([1])`, "!TypeError"},
		{"float16", `const a = new Float16Array([1.1, 65520, -0, NaN]); return [a[0], a[1], Object.is(a[2], -0), a[3], new Float16Array(new Float64Array([65504]))[0], Math.f16round(1.1), Math.f16round(5e-8), Math.f16round.length].join()`, "1.099609375,Infinity,true,NaN,65504,1.099609375,5.960464477539063e-8,1"},
		{"call throws", `return Uint8Array(1)`, "!TypeError"},
		{"abstract", `const TA = Object.getPrototypeOf(Uint8Array); return [TA.name, TA.length, typeof TA.from, Object.getPrototypeOf(Int8Array.prototype) === TA.prototype, TA[Symbol.species] === TA].join()`, "TypedArray,0,function,true,true"},
		{"abstract throws", `return new (Object.getPrototypeOf(Uint8Array))()`, "!TypeError"},
		{"misaligned offset", `return new Int32Array(new ArrayBuffer(8), 1)`, "!RangeError"},
		{"misaligned length", `return new Int32Array(new ArrayBuffer(7))`, "!RangeError"},
		{"view past the end", `return new Int16Array(new ArrayBuffer(4), 2, 2)`, "!RangeError"},
		{"negative length", `return new Uint8Array(-1)`, "!RangeError"},
		{"too large", `return new Uint8Array(2 ** 31)`, "!RangeError"},
		{"too many bytes", `return new Float64Array(2 ** 28)`, "!RangeError"},
		{"array-like too large", `return new Uint8Array({length: 2 ** 40})`, "!RangeError"},
		{"detached source", `const b = new ArrayBuffer(4); b.transfer(); return new Uint8Array(b)`, "!TypeError"},
		{"of", `class M extends Uint8Array {} const a = M.of(1, 2); return [a instanceof M, a.join(), Int8Array.of().length].join()`, "true,1,2,0"},
		{"from", `const b = Uint8Array.from([1, 2, 3], function (x, i) { return x * 10 + i + this.k; }, {k: 100}); return [b.join(), Int8Array.from({length: 2, 0: 5}).join(), Float64Array.from(new Set([0.5])).join()].join("|")`, "110,121,132|5,0|0.5"},
		{"from not a constructor", `return Uint8Array.from.call({}, [])`, "!TypeError"},
		{"from mapper not callable", `return Uint8Array.from([], 1)`, "!TypeError"},
		{"of too small", `return Uint8Array.of.call(function () { return new Uint8Array(1); }, 1, 2)`, "!TypeError"},
		{"shape", `const P = Object.getPrototypeOf(Int8Array.prototype); return [Object.getOwnPropertyNames(P).length, P[Symbol.iterator] === P.values, P.toString === Array.prototype.toString, Int8Array.length, Int8Array.name, Object.getOwnPropertyNames(Int8Array.prototype).sort().join(" "), Object.getPrototypeOf(Int8Array) === Object.getPrototypeOf(Float16Array)].join()`, "36,true,true,3,Int8Array,BYTES_PER_ELEMENT constructor,true"},
		{"accessors", `const a = new Int16Array(new ArrayBuffer(8), 2, 2); const P = Object.getPrototypeOf(Int8Array.prototype); return [a.byteOffset, a.byteLength, a.length, a.buffer.byteLength, Object.prototype.toString.call(a), a[Symbol.toStringTag], Object.getOwnPropertyDescriptor(P, Symbol.toStringTag).get.call(1)].join()`, "2,4,2,8,[object Int16Array],Int16Array,"},
		{"accessor receivers", `return Object.getOwnPropertyDescriptor(Object.getPrototypeOf(Int8Array.prototype), "length").get.call(new DataView(new ArrayBuffer(1)))`, "!TypeError"},
		{"isView", `return ArrayBuffer.isView(new Uint8Array(1))`, "true"},
	})
	runMutableProtoCases(t, []protoCase{
		{"subclass", `class M extends Float64Array { sum() { return this.reduce((a, b) => a + b, 0); } } return new M([1, 2]).sum()`, "3"},
		{"global is writable", `globalThis.Uint8Array = 1; return [typeof Int8Array, globalThis.Uint8Array, typeof Atomics].join()`, "function,1,object"},
		{"toLocaleString calls elements", `Number.prototype.toLocaleString = function () { return "x" + this; }; return new Uint8Array([1, 2]).toLocaleString()`, "x1,x2"},
	})
}

func TestTypedArrayExotic(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"numeric keys", `const a = new Uint8Array(2); a["-0"] = 1; a["1.5"] = 1; a[2] = 1; a["Infinity"] = 1; a.x = 1; return [Object.keys(a).join(" "), a["-0"], a["1.5"], a[2], "-0" in a, "1" in a, "2" in a, a.Infinity, a.x, a["01"]].join()`, "0 1 x,,,,false,true,false,,1,"},
		{"string keys stay ordinary", `const a = new Uint8Array(1); a["01"] = 5; a["+0"] = 6; return [a["01"], a["+0"], Object.keys(a).join(" ")].join()`, "5,6,0 01 +0"},
		{"defineProperty", `const a = new Uint8Array(2); Object.defineProperty(a, "0", {value: 7}); const d = Object.getOwnPropertyDescriptor(a, 1); const out = [a[0], d.writable, d.enumerable, d.configurable]; for (const desc of [{value: 1, configurable: false}, {value: 1, enumerable: false}, {get() {}}]) { try { Object.defineProperty(a, "0", desc); out.push("ok"); } catch (e) { out.push(e.name); } } try { Object.defineProperty(a, "5", {value: 1}); } catch (e) { out.push(e.name); } return out.join()`, "7,true,true,true,TypeError,TypeError,TypeError,TypeError"},
		{"delete", `const a = new Uint8Array(2); let e; try { delete a[0]; } catch (x) { e = x.name; } return [e, delete a[5], delete a["-0"]].join()`, "TypeError,true,true"},
		{"freeze", `return Object.freeze(new Uint8Array(1))`, "!TypeError"},
		{"freeze empty", `return [Object.isFrozen(Object.freeze(new Uint8Array(0))), Object.isSealed(Object.seal(new Uint8Array(0)))].join()`, "true,true"},
		{"seal", `return Object.seal(new Uint8Array(1))`, "!TypeError"},
		{"preventExtensions length tracking", `return Object.preventExtensions(new Uint8Array(new ArrayBuffer(1, {maxByteLength: 2})))`, "!TypeError"},
		{"own keys", `const a = new Int8Array(3); a.z = 1; a[Symbol.iterator] = 1; return Reflect.ownKeys(a).map(String).join()`, "0,1,2,z,Symbol(Symbol.iterator)"},
		{"set coerces once", `const a = new Uint8Array(1); let n = 0; a[0] = {valueOf() { n++; return 3; }}; a[5] = {valueOf() { n++; return 3; }}; return [a[0], n].join()`, "3,2"},
		{"set on a receiver", `const a = new Uint8Array(1); const o = Object.create(a); o[0] = 5; return [a[0], Object.hasOwn(o, "0"), o[0]].join()`, "0,true,5"},
		{"set invalid index on a receiver", `const a = new Uint8Array(1); const o = Object.create(a); o[3] = 5; return [Object.hasOwn(o, "3"), o[3]].join()`, "false,"},
		{"Reflect.set receiver", `const a = new Uint8Array(1), b = new Uint8Array(1); return [Reflect.set(a, 0, 7, b), a[0], b[0]].join()`, "true,0,7"},
		{"inline caches", `function g(o) { return o[0]; } function h(o, k) { return o[k]; } const b = new ArrayBuffer(1, {maxByteLength: 2}); const a = new Uint8Array(b); a[0] = 1; const out = []; for (let i = 0; i < 3; i++) out.push(g(a), h(a, "0")); b.resize(0); out.push(g(a), h(a, "0"), h(a, 0)); b.resize(1); a[0] = 3; out.push(g(a), h(a, "0"), g({0: 4}), g(new Int8Array([-1]))); return out.join()`, "1,1,1,1,1,1,,,,3,3,4,-1"},
		{"loop writes", `const a = new Float64Array(8); for (let i = 0; i < 10; i++) a[i] = i / 2; let s = 0; for (const k in a) s += a[k]; return [s, a.length, a[9]].join()`, "14,8,"},
		{"key limit", `return Object.keys(new Uint8Array(2 ** 24 + 1))`, "!RangeError"},
		{"spread", `return [...new Uint8Array([1, 2]), ...new BigInt64Array([3n])].join()`, "1,2,3"},
		{"Array.from", `return Array.from(new Int16Array([-1, 2])).join()`, "-1,2"},
		{"Array.prototype methods", `const a = new Uint8Array([3, 1, 2]); return [Array.prototype.join.call(a, "-"), Array.prototype.slice.call(a, 1).join(), Array.prototype.includes.call(a, 1), Array.isArray(a)].join()`, "3-1-2,1,2,true,false"},
		{"JSON", `return JSON.stringify({a: new Uint8Array([1, 2])})`, `{"a":{"0":1,"1":2}}`},
		{"Object.entries", `return Object.entries(new Int8Array([5, 6])).join(";")`, "0,5;1,6"},
		// Array indices of 2^31 and more, which an int of 32 bits holds as
		// negative numbers, and an array out of bounds.
		{"index past 2^31", `const a = new Uint8Array([1, 2]), f = new Float64Array([1.5]), out = []; for (const k of ["2147483648", "4294967294", 2147483648, 4294967294]) { a[k] = 9; f[k] = 9; out.push(a[k], f[k], k in a, Object.hasOwn(f, k), Reflect.defineProperty(a, k, {value: 1}), delete a[k]); } out.push(a.join(), f.join(), Object.keys(a).join()); const b = new ArrayBuffer(2, {maxByteLength: 4}), v = new Uint8Array(b, 1, 1); b.resize(1); out.push(v["2147483648"], v[0], "2147483648" in v, 0 in v); return out.join()`, ",,false,false,false,true,,,false,false,false,true,,,false,false,false,true,,,false,false,false,true,1,2,1.5,0,1,,,false,false"},
	})
	runMutableProtoCases(t, []protoCase{
		{"prototype chain is skipped", `Object.prototype[3] = "p"; Object.prototype["-0"] = "q"; return [new Uint8Array(1)[3], 3 in new Uint8Array(1), new Uint8Array(1)["-0"], new Uint8Array(1)[4] = 1, Object.prototype[4]].join()`, ",false,,1,"},
	})
}

// TestTypedArrayKeyAtoms checks the numeric flag interning sets on an atom
// against CanonicalNumericIndexString, for static and dynamic atoms.
func TestTypedArrayKeyAtoms(t *testing.T) {
	r := NewRealm()
	for _, name := range []string{"-0", "1.5", "-1", "NaN", "Infinity", "-Infinity", "1e+21", "4294967295", "1e-7",
		"0", "7", "1e21", "01", "+0", "-0.0", "0x10", ".5", "1.50", "Inf", "NaNx", " 1", "1 ", "-", "", "x", "length", "\u0661"} {
		_, want := CanonicalNumericIndexString(FromGoString(name))
		assert.Equal(t, want, typedArrayKey(key(r, name)), "%q", name)
	}
	assert.True(t, typedArrayKey(StringKey(AtomNaN)))
	assert.True(t, typedArrayKey(StringKey(AtomInfinity)))
	assert.False(t, typedArrayKey(StringKey(AtomLength)))
	assert.False(t, typedArrayKey(SymbolKey(SymIterator)))
}

func TestTypedArrayMethods(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"subarray", `const a = new Uint8Array([1, 2, 3, 4, 5]); const s = a.subarray(1, -1); s[0] = 9; return [s.join(), s.byteOffset, s.buffer === a.buffer, a[1], a.subarray(-2).join(), a.subarray(3, 1).length].join("|")`, "9,3,4|1|true|9|4,5|0"},
		{"subarray length tracking", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const a = new Uint8Array(b); const s = a.subarray(1), f = a.subarray(1, 3); b.resize(8); return [s.length, f.length].join()`, "7,2"},
		{"species", `class M extends Uint8Array { static get [Symbol.species]() { return Int16Array; } } const m = new M([1, 2, 3]); return [m.map(x => x * 200).join(), m.slice(1) instanceof Int16Array, m.filter(x => x > 1).join()].join("|")`, "200,400,600|true|2,3"},
		{"species content type", `class M extends Uint8Array { static get [Symbol.species]() { return BigInt64Array; } } return new M(1).slice()`, "!TypeError"},
		{"species too short", `const a = new Uint8Array(4); a.constructor = {[Symbol.species]: function () { return new Uint8Array(1); }}; return a.map(x => x)`, "!TypeError"},
		{"set", `const a = new Uint8Array(6); a.set([1, 2], 1); a.set(new Int8Array([-1]), 5); let e; try { a.set([1, 2], 5); } catch (x) { e = x.name; } return [a.join(), e, a.set.length].join("|")`, "0,1,2,0,0,255|RangeError|1"},
		{"set overlap", `const a = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]); a.set(a.subarray(0, 6), 2); const b = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]); b.set(new Uint16Array(b.buffer, 0, 2), 2); return [a.join(), b.join()].join("|")`, "1,2,1,2,3,4,5,6|1,2,1,3,5,6,7,8"},
		{"set bigint", `const a = new BigInt64Array(3); a.set([1n, "2"]); a.set(new BigUint64Array([2n ** 64n - 1n]), 2); return a.join()`, "1,2,-1"},
		{"set mixed content", `return new BigInt64Array(1).set(new Int8Array(1))`, "!TypeError"},
		{"set negative offset", `return new Uint8Array(1).set([], -1)`, "!RangeError"},
		{"copyWithin", `return [new Uint8Array([1, 2, 3, 4, 5]).copyWithin(0, 3).join(), new Uint8Array([1, 2, 3, 4, 5]).copyWithin(1, 0, 3).join(), new Int16Array([1, 2, 3, 4, 5]).copyWithin(-2, 0).join(), new Uint8Array([1, 2, 3]).copyWithin(0, 1, -5).join()].join("|")`, "4,5,3,4,5|1,1,2,3,5|1,2,3,1,2|1,2,3"},
		{"copyWithin shrinks", `const out = [];
			for (const [to, from, es] of [[2, 1, 1], [1, 2, 1], [2, 0, 2], [0, 2, 2]]) {
				const b = new ArrayBuffer(4 * es, {maxByteLength: 8 * es}), a = es == 1 ? new Uint8Array(b) : new Int16Array(b);
				a.set([0, 1, 2, 3]);
				const evil = {valueOf() { b.resize(3 * es); return 2; }};
				a.copyWithin(to == 2 ? evil : to, from == 2 ? evil : from);
				out.push(a.join());
			}
			return out.join("|")`, "0,1,1|0,2,2|0,1,0|2,1,2"},
		{"fill", `return [new Uint8Array(5).fill(7, 1, -1).join(), new Float32Array(3).fill(0.1)[0], new BigInt64Array(2).fill(-1n).join(), new Uint8Array(3).fill(300).join(), new Int16Array(3).fill(-2, -1).join()].join("|")`, "0,7,7,7,0|0.10000000149011612|-1,-1|44,44,44|0,0,-2"},
		{"fill bigint with number", `return new BigInt64Array(1).fill(1)`, "!TypeError"},
		{"reverse", `return [new Int8Array([1, 2, 3]).reverse().join(), new Float64Array([1, 2]).toReversed().join(), new Uint16Array(0).reverse().length].join("|")`, "3,2,1|2,1|0"},
		{"search", `const a = new Float64Array([1, NaN, -0, 3, 1]); return [a.indexOf(NaN), a.includes(NaN), a.indexOf(0), a.lastIndexOf(1), a.lastIndexOf(1, -2), a.indexOf(1, 1), a.includes(3, -1), new Uint8Array([255]).indexOf(-1), new Int8Array([-1]).indexOf(-1), new Uint8Array(2).includes(undefined), new BigInt64Array([5n]).indexOf(5n), new BigInt64Array([5n]).indexOf(5), new Uint8Array([1]).indexOf(1.5), new Float32Array([0.1]).indexOf(0.1), new Float32Array([0.5]).includes(0.5), new BigUint64Array([1n]).indexOf(-1n), new Uint8Array([1]).lastIndexOf(1, -5)].join()`, "-1,true,2,4,0,4,false,-1,0,false,0,-1,-1,-1,true,-1,-1"},
		{"callbacks", `const a = new Int8Array([3, 1, 2]); const log = []; a.forEach((v, i, o) => log.push(v + ":" + i + ":" + (o === a))); return [log.join(), a.every(x => x > 0), a.some(x => x > 2), a.find(x => x < 3), a.findIndex(x => x < 3), a.findLast(x => x > 1), a.findLastIndex(x => x > 5), a.reduce((s, x) => s + x), a.reduceRight((s, x) => s + x, ""), a.map(x => x * 2).join(), a.filter((x, i) => i !== 1).join()].join("|")`, "3:0:true,1:1:true,2:2:true|true|true|1|1|2|-1|6|213|6,2,4|3,2"},
		{"callback not callable", `return new Uint8Array(1).map(1)`, "!TypeError"},
		{"reduce empty", `return new Uint8Array(0).reduce((a, b) => a)`, "!TypeError: Reduce of empty array with no initial value"},
		{"join", `const a = new Float64Array([1, -0, 1.5]); return [a.join(), a.join(" - "), a.toString(), String(new Uint8Array(0)), new BigInt64Array([1n, -2n]).toLocaleString(), new Uint8Array(2).join(undefined)].join("|")`, "1,0,1.5|1 - 0 - 1.5|1,0,1.5||1,-2|0,0"},
		{"iterators", `const a = new Uint8Array([7, 8]); return [[...a].join(), [...a.keys()].join(), [...a.entries()].join(";"), Object.prototype.toString.call(a.values())].join("|")`, "7,8|0,1|0,7;1,8|[object Array Iterator]"},
		{"iterator after detach", `const b = new ArrayBuffer(2); const it = new Uint8Array(b).values(); it.next(); b.transfer(); return it.next()`, "!TypeError"},
		{"at and with", `const a = new Int8Array([1, 2, 3]); return [a.at(-1), a.at(5), a.with(0, 9).join(), a.with(-1, 200).join(), a.join()].join("|")`, "3||9,2,3|1,2,-56|1,2,3"},
		{"with range", `return new Int8Array(2).with(2, 0)`, "!RangeError"},
		{"with bigint", `return new BigInt64Array(1).with(0, 1)`, "!TypeError"},
		{"slice", `const a = new Int16Array([1, 2, 3, 4]); return [a.slice(1, 3).join(), a.slice(-1).join(), a.slice(2, 1).length, a.slice().buffer !== a.buffer].join("|")`, "2,3|4|0|true"},
		{"slice species over the same buffer", `const a = new Uint8Array([1, 2, 3, 4, 5, 6]); a.constructor = {[Symbol.species]: function (n) { return new Uint8Array(a.buffer, 1, n); }}; a.slice(0, 4); return a.join()`, "1,1,1,1,1,6"},
		{"slice species other type", `const a = new Uint8Array([1, 255]); a.constructor = {[Symbol.species]: Int8Array}; return a.slice().join()`, "1,-1"},
		{"receivers", `return Object.getPrototypeOf(Int8Array.prototype).fill.call([1], 0)`, "!TypeError"},
		{"prototype receiver", `return Int8Array.prototype.join()`, "!TypeError"},
	})
}

func TestTypedArraySort(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"default", `const f = new Float64Array([3, NaN, -0, 0, -Infinity, 1, NaN, -1]).sort(); return [Array.from(f, x => Object.is(x, -0) ? "-0" : String(x)).join(), new Int8Array([5, -3, 0, -128, 127]).sort().join(), new Uint32Array([4294967295, 0, 7]).sort().join(), new BigInt64Array([3n, -5n, 0n]).sort().join(), new BigUint64Array([2n ** 64n - 1n, 0n]).sort().join(), new Float16Array([2, -1, NaN, 0.5]).sort().join(), new Int16Array([300, -300, 1]).sort().join(), new Uint8ClampedArray([9, 0, 255]).sort().join(), new Float32Array([-0.5, -Infinity, 1e-40]).sort().join()].join("|")`, "-Infinity,-1,-0,0,1,3,NaN,NaN|-128,-3,0,5,127|0,7,4294967295|-5,0,3|0,18446744073709551615|-1,0.5,2,NaN|-300,1,300|0,9,255|-Infinity,-0.5,9.99994610111476e-41"},
		{"comparator", `const a = new Uint8Array([1, 2, 3, 4, 5, 6]); a.sort((x, y) => (x % 2) - (y % 2)); const b = new Float32Array([1, 2, 3]).sort((x, y) => y - x); const c = new Int8Array([3, 1, 2]); const d = c.toSorted(); const e = new BigInt64Array([1n, 3n, 2n]).toSorted((x, y) => Number(y - x)); return [a.join(), b.join(), c.join(), d.join(), e.join(), new Uint8Array([2, 1]).sort(() => NaN).join()].join("|")`, "2,4,6,1,3,5|3,2,1|3,1,2|1,2,3|3,2,1|2,1"},
		{"comparator not callable", `return new Uint8Array(1).sort(1)`, "!TypeError: The comparison function must be either a function or undefined"},
		{"comparator checked first", `return Object.getPrototypeOf(Int8Array.prototype).toSorted.call({}, 1)`, "!TypeError: The comparison function"},
		{"comparator throws", `const a = new Uint8Array([2, 1]); try { a.sort(() => { throw new Error("x"); }); } catch (e) { return e.message + a.join(); }`, "x2,1"},
		{"comparator shrinks", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const a = new Uint8Array(b); a.set([4, 3, 2, 1]); a.sort((x, y) => { b.resize(2); return x - y; }); return a.join()`, "1,2"},
		{"comparator detaches", `const b = new ArrayBuffer(2); const a = new Uint8Array(b); a.set([2, 1]); a.sort((x, y) => { b.transfer(); return x - y; }); return a.length`, "0"},
		{"large", `function check(TA, n, f) { const a = new TA(n); let s = 12345; for (let i = 0; i < n; i++) { s = (s * 48271) % 2147483647; a[i] = f(s, i); } const b = a.slice().sort((x, y) => x < y ? -1 : x > y ? 1 : 0); a.sort(); for (let i = 0; i < n; i++) if (a[i] !== b[i]) return TA.name + " at " + i; return "ok"; } return [check(Float64Array, 10000, s => (s - 1e9) / 7), check(Float32Array, 5000, s => s - 1e9), check(Int32Array, 9000, s => s - 2 ** 30), check(Uint32Array, 9000, s => s * 2), check(Int16Array, 5000, s => s), check(Uint16Array, 5000, s => s & 0x00ff), check(BigInt64Array, 6000, s => BigInt(s - 1e9) * 1000003n), check(BigUint64Array, 6000, s => BigInt(s) << 32n), check(Float16Array, 5000, s => s / 1e5 - 1e4), check(Uint8Array, 5000, s => s)].join()`, "ok,ok,ok,ok,ok,ok,ok,ok,ok,ok"},
		{"stable", `const a = new Uint16Array(6000); for (let i = 0; i < a.length; i++) a[i] = (i % 3) << 12 | i >> 4; a.sort((x, y) => (x >> 12) - (y >> 12)); for (let i = 1; i < a.length; i++) if ((a[i - 1] >> 12) === (a[i] >> 12) && a[i - 1] > a[i]) return i; return "stable"`, "stable"},
		{"canonical NaN", `const f = new Float64Array(2); const u = new Uint32Array(f.buffer); u[0] = 1; u[1] = 0xfff00000; f.sort(); return [u[1].toString(16), u[0], u[3].toString(16)].join()`, "0,0,7ff80000"},
	})
}

func TestTypedArrayResizable(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"length tracking", `const b = new ArrayBuffer(4, {maxByteLength: 16}); const a = new Uint16Array(b); const f = new Uint16Array(b, 2, 1); const out = [a.length, f.length]; b.resize(10); out.push(a.length, a.byteLength, f.length); b.resize(3); out.push(a.length, f.length, f.byteOffset, f.byteLength, f[0]); b.resize(1); out.push(a.length, a.byteOffset); b.resize(8); a[3] = 5; out.push(a.length, a[3], f.length); return out.join()`, "2,1,5,10,1,1,0,0,0,,0,0,4,5,1"},
		{"offset at the end", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const a = new Uint8Array(b, 2); b.resize(2); return [a.length, a.byteOffset].join()`, "0,2"},
		{"out of bounds", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const a = new Uint8Array(b, 2); b.resize(1); const out = [a.length, a.byteOffset, a[0], 0 in a, Object.keys(a).length]; for (const f of [() => a.fill(1), () => a.join(), () => [...a], () => a.at(0), () => a.sort(), () => a.slice(), () => a.subarray(0), () => a.set([]), () => Atomics.load(a, 0)]) { try { f(); out.push("ok"); } catch (e) { out.push(e.name); } } return out.join()`, "0,0,,false,0,TypeError,TypeError,TypeError,TypeError,TypeError,TypeError,RangeError,TypeError,TypeError"},
		{"grows during iteration", `const b = new ArrayBuffer(2, {maxByteLength: 4}); const a = new Uint8Array(b); const seen = []; for (const x of a) { seen.push(x); if (seen.length === 1) b.resize(4); } return seen.length`, "4"},
		{"shrinks in a callback", `const b = new ArrayBuffer(4, {maxByteLength: 4}); const a = new Uint8Array(b); a.set([1, 2, 3, 4]); const seen = []; a.forEach(x => { seen.push(x); b.resize(2); }); return seen.join()`, "1,2,,"},
		{"fill clamps after coercion", `const b = new ArrayBuffer(4, {maxByteLength: 4}); const a = new Uint8Array(b); a.fill(7, 0, {valueOf() { b.resize(2); return 4; }}); return a.join()`, "7,7"},
		{"includes after shrink", `const b = new ArrayBuffer(4, {maxByteLength: 4}); const a = new Uint8Array(b); return a.includes(undefined, {valueOf() { b.resize(2); return 0; }})`, "true"},
		{"with after grow", `const b = new ArrayBuffer(2, {maxByteLength: 4}); const a = new Uint8Array(b); return a.with(0, {valueOf() { b.resize(4); return 1; }}).join()`, "1,0"},
		{"shared", `const b = new SharedArrayBuffer(8, {maxByteLength: 16}); const a = new Int32Array(b); b.grow(16); return [a.length, a.buffer === b].join()`, "4,true"},
		{"detached", `const b = new ArrayBuffer(4); const a = new Uint8Array(b); b.transfer(); const out = [a.length, a.byteLength, a.byteOffset, a[0], 0 in a]; a[0] = 1; for (const f of [() => a.fill(0), () => a.slice(), () => new Uint8Array(a), () => a.values()]) { try { f(); out.push("ok"); } catch (e) { out.push(e.name); } } return out.join()`, "0,0,0,,false,TypeError,TypeError,TypeError,TypeError"},
		{"filter detaches", `const b = new ArrayBuffer(2); const a = new Float32Array(b.transfer(8)); const s = new Float32Array(new ArrayBuffer(8)); s.set([1, 2]); return s.filter((x, i) => { if (i === 0) s.buffer.transfer(); return true; }).join()`, "1,NaN"},
	})
}

func TestAtomics(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"operations", `const a = new Int32Array(4); return [Atomics.add(a, 0, 5), Atomics.sub(a, 0, 2), Atomics.and(a, 0, 1), Atomics.or(a, 1, 6), Atomics.xor(a, 1, 3), Atomics.exchange(a, 2, 9), Atomics.compareExchange(a, 2, 9, 4), Atomics.compareExchange(a, 2, 9, 5), Atomics.load(a, 2), Atomics.store(a, 3, 3.7), a.join(), Atomics.notify(new Int32Array(new SharedArrayBuffer(4)), 0), Atomics.isLockFree(4), Atomics.isLockFree(3)].join("|")`, "0|5|3|0|6|0|9|4|4|3|1,5,4,3|0|true|false"},
		{"wrapping", `const u = new Uint8Array(1); const b = new BigInt64Array(1); return [Atomics.add(u, 0, 300), u[0], Atomics.sub(u, 0, 50), u[0], Atomics.compareExchange(new Int8Array([-1]), 0, 255, 1), Atomics.sub(b, 0, 1n), b[0], Atomics.compareExchange(new Uint16Array([65535]), 0, -1, 0), typeof Atomics.load(b, 0), Atomics.store(b, 0, 2n ** 64n + 1n), b[0], Atomics.store(u, 0, -0)].join()`, "0,44,44,250,-1,0,-1,65535,bigint,18446744073709551617,1,0"},
		{"shared buffer", `const a = new Int16Array(new SharedArrayBuffer(4)); Atomics.store(a, 1, -2); return [Atomics.load(a, 1), Atomics.exchange(a, 1, 3), a[1]].join()`, "-2,-2,3"},
		{"float", `return Atomics.add(new Float64Array(1), 0, 1)`, "!TypeError"},
		{"clamped", `return Atomics.load(new Uint8ClampedArray(1), 0)`, "!TypeError"},
		{"not a typed array", `return Atomics.load([1], 0)`, "!TypeError"},
		{"index", `return Atomics.load(new Int8Array(1), 1)`, "!RangeError: Invalid atomic access index"},
		{"negative index", `return Atomics.load(new Int8Array(1), -1)`, "!RangeError"},
		{"bigint value", `return Atomics.store(new BigInt64Array(1), 0, 1)`, "!TypeError"},
		{"wait", `return Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 0)`, "!TypeError: Atomics.wait cannot be called in this context"},
		{"wait bigint", `return Atomics.wait(new BigInt64Array(new SharedArrayBuffer(8)), 0, 0n)`, "!TypeError: Atomics.wait cannot be called in this context"},
		{"wait unshared", `return Atomics.wait(new Int32Array(4), 0, 0, 0)`, "!TypeError: Atomics.wait: the typed array must view a SharedArrayBuffer"},
		{"wait type", `return Atomics.wait(new Int16Array(new SharedArrayBuffer(4)), 0, 0, 0)`, "!TypeError: Atomics.wait: the typed array must be"},
		{"wait index", `return Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 1, 0, 0)`, "!RangeError"},
		{"notify type", `return Atomics.notify(new Uint32Array(4), 0)`, "!TypeError"},
		{"notify unshared", `return Atomics.notify(new Int32Array(4), 0, 1)`, "0"},
		{"shape", `return [Object.prototype.toString.call(Atomics), Object.getOwnPropertyNames(Atomics).sort().join(" "), Atomics.compareExchange.length, Atomics.wait.length, typeof Atomics.pause, typeof Atomics.waitAsync].join()`, "[object Atomics],add and compareExchange exchange isLockFree load notify or store sub wait xor,4,4,undefined,undefined"},
		{"revalidate shrink", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const a = new Int8Array(b); try { Atomics.store(a, 3, {valueOf() { b.resize(2); return 1; }}); } catch (e) { return e.name; }`, "RangeError"},
		{"revalidate detach", `const b = new ArrayBuffer(4); const a = new Int8Array(b); try { Atomics.store(a, 0, {valueOf() { b.transfer(); return 1; }}); } catch (e) { return e.name; }`, "TypeError"},
	})
}

// TestTypedArrayInterrupts checks that the typed array methods honour a
// pending interrupt on inputs well above their check intervals.
func TestTypedArrayInterrupts(t *testing.T) {
	cases := []struct {
		name, mk, method string
		args             []Value
	}{
		{"fill", `new Uint8Array(1 << 23)`, "fill", []Value{IntValue(1)}},
		{"fill pattern", `new Float64Array(1 << 20)`, "fill", []Value{NumberValue(0.5)}},
		{"copyWithin", `new Uint8Array(1 << 23)`, "copyWithin", []Value{IntValue(1), IntValue(0)}},
		{"slice", `new Uint8Array(1 << 23)`, "slice", nil},
		{"reverse", `new Uint8Array(1 << 16)`, "reverse", nil},
		{"indexOf", `new Uint8Array(1 << 16)`, "indexOf", []Value{IntValue(1)}},
		{"includes", `new Float64Array(1 << 16)`, "includes", []Value{NumberValue(0.5)}},
		{"join", `new Uint8Array(1 << 16)`, "join", nil},
		{"sort", `new Uint8Array(1 << 16)`, "sort", nil},
		{"sort radix", `new Float64Array(1 << 16).map((x, i) => -i)`, "sort", nil},
		{"toSorted", `new Int32Array(1 << 16).map((x, i) => -i)`, "toSorted", nil},
		{"toReversed", `new Uint8Array(1 << 16)`, "toReversed", nil},
		{"with", `new Uint8Array(1 << 23)`, "with", []Value{IntValue(0), IntValue(1)}},
		{"set", `new Uint8Array(1 << 23)`, "set", []Value{IntValue(0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, elapsed, err := auditBNativeInterrupt(t, `function mk() { return `+c.mk+`; }`, func(r *Realm, big Value) (Value, error) {
				args := c.args
				if c.method == "set" {
					args = []Value{big, IntValue(0)}
				}
				return auditBMethod(t, r, big, c.method, args...)
			})
			assert.True(t, ok, "%s ran %s with an interrupt pending: %v", c.name, elapsed, err)
		})
	}
	r, env := auditBRealm(t, `export const big = new Uint8Array(1 << 16), arr = new Array(1 << 16).fill(1);`)
	for _, name := range []string{"big", "arr"} {
		src, _ := env.GetBindingValue(name)
		for _, ctor := range []string{"Float64Array", "BigInt64Array"} {
			if ctor == "BigInt64Array" && name == "big" {
				continue
			}
			c, err := r.Global.GetProp(r, r.KeyFromGoString(ctor))
			require.NoError(t, err)
			r.Interrupt("x")
			_, err = r.Construct(c, []Value{src}, c.AsObject())
			var ie *InterruptedError
			if ctor == "BigInt64Array" {
				assert.Error(t, err, "BigInt64Array from numbers")
			} else {
				assert.ErrorAs(t, err, &ie, "%s from %s", ctor, name)
			}
			r.ClearInterrupt()
		}
	}
}

// TestTypedArrayLateGroup checks that the typed arrays join the binary late
// group: the constructors are bound by the one installer and share their
// abstract parent in both realm kinds.
func TestTypedArrayLateGroup(t *testing.T) {
	for _, shared := range []bool{true, false} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		b := r.binaryIntr()
		for k, c := range b.typedArrayCtors {
			name := typedArrayCtorNames[k].GoString()
			require.NotNil(t, c, name)
			assert.Same(t, b.TypedArrayCtor, c.Proto(), name)
			assert.Same(t, b.TypedArrayPrototype, b.typedArrayPrototypes[k].Proto(), name)
			v, ok := r.Global.GetOwnProperty(StringKey(typedArrayCtorNames[k]))
			require.True(t, ok, name)
			assert.Same(t, c, v.Value.AsObject(), name)
		}
		v, ok := r.Global.GetOwnProperty(InternKey("Atomics"))
		require.True(t, ok)
		assert.Same(t, b.Atomics, v.Value.AsObject())
	}
}

// TestTypedArrayElementNumberKeys covers the element reads and writes with a
// Number key (getElemSlow, setElemSlow): the in-place access of an index
// inside a fixed-length array whose buffer holds it, and every case it
// leaves to TypedArrayGetElement and TypedArraySetElement.
func TestTypedArrayElementNumberKeys(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"every type", `const out = []; for (const T of [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array, Float16Array, Float32Array, Float64Array]) { const a = new T(3); for (let i = 0; i < 3; i++) a[i] = 1.5 * i - 1; out.push(T.name + ":" + [a[0], a[1], a[2]].join(" ")); } return out.join()`,
			"Int8Array:-1 0 2,Uint8Array:255 0 2,Uint8ClampedArray:0 0 2,Int16Array:-1 0 2,Uint16Array:65535 0 2,Int32Array:-1 0 2,Uint32Array:4294967295 0 2,Float16Array:-1 0.5 2,Float32Array:-1 0.5 2,Float64Array:-1 0.5 2"},
		{"wrap and clamp", `const i8 = new Int8Array(1), u8 = new Uint8Array(1), c = new Uint8ClampedArray(4), u32 = new Uint32Array(1), i32 = new Int32Array(1); let k = 0; i8[k] = 128; u8[k] = -1.9; u32[k] = -1; i32[k] = 2 ** 32 + 5; c[k] = 1.5; c[k + 1] = 2.5; c[k + 2] = 300; c[k + 3] = NaN; return [i8[k], u8[k], u32[k], i32[k], c.join(" ")].join()`, "-128,255,4294967295,5,2 2 255 0"},
		{"float specials", `const a = new Float64Array(3), f = new Float32Array(2); let k = 0; a[k] = -0; a[k + 1] = NaN; a[k + 2] = -Infinity; f[k] = 1.1; f[k + 1] = 1e40; return [Object.is(a[k], -0), a[k + 1], a[k + 2], f[k], f[k + 1]].join()`, "true,NaN,-Infinity,1.100000023841858,Infinity"},
		{"keys", `const a = new Int16Array([10, 20]); const keys = [-0, 0.5, -1, 2, NaN, Infinity, -Infinity, 2 ** 53, 1e300, 2 ** 31]; return keys.map(k => String(a[k])).join() + "|" + keys.map(k => { a[k] = 7; return a.join(" "); }).join()`, "10,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined|7 20,7 20,7 20,7 20,7 20,7 20,7 20,7 20,7 20,7 20"},
		{"offset view", `const b = new ArrayBuffer(16); const a = new Uint16Array(b, 4, 3), all = new Uint16Array(b); let i = 0; a[i] = 1; a[i + 2] = 3; a[i + 3] = 9; return [all.join(" "), a[i + 2], a[i + 3]].join()`, "0 0 1 0 3 0 0 0,3,"},
		{"bigint types", `const a = new BigInt64Array(2), u = new BigUint64Array(1); let i = 0; a[i] = -5n; u[i] = 2n ** 64n - 1n; let e; try { a[i + 1] = 1; } catch (x) { e = x.name; } return [a[i], a[i + 1], u[i], e, typeof a[i]].join()`, "-5,0,18446744073709551615,TypeError,bigint"},
		{"bigint type out of range number", `const a = new BigInt64Array(1); let i = 5; a[i] = 1`, "!TypeError"},
		{"detached", `const b = new ArrayBuffer(8); const a = new Float64Array(b); let i = 0; a[i] = 1; b.transfer(); a[i] = 2; return [a[i], a.length, a[0]].join()`, ",0,"},
		{"detached by the conversion", `const b = new ArrayBuffer(8); const a = new Float64Array(b); let i = 0; a[i] = {valueOf() { b.transfer(); return 5; }}; return [a[i], a.length].join()`, ",0"},
		{"resized by the conversion", `const b = new ArrayBuffer(8, {maxByteLength: 16}); const a = new Uint8Array(b, 0, 8); let i = 7; a[i] = {valueOf() { b.resize(4); return 5; }}; return [a[i], a.length, b.byteLength].join()`, ",0,4"},
		{"fixed view over a resizable buffer", `const b = new ArrayBuffer(8, {maxByteLength: 16}); const a = new Uint8Array(b, 2, 4); let i = 3; a[i] = 9; const before = a[i]; b.resize(5); const out = [before, a[i], a[0], a.length]; a[0] = 1; b.resize(16); out.push(a[i], a[0], a.length); b.resize(6); out.push(a[i], a[1], a.length); return out.join()`, "9,,,0,0,0,4,0,0,4"}, // a shrink drops the bytes, a grow brings zeros
		{"length tracking", `const b = new ArrayBuffer(4, {maxByteLength: 16}); const a = new Uint8Array(b); let i = 5; a[i] = 1; const out = [a[i]]; b.resize(8); a[i] = 2; out.push(a[i], a.length); b.resize(2); out.push(a[i], a.length); return out.join()`, ",2,8,,2"},
		{"shared buffer", `const a = new Int32Array(new SharedArrayBuffer(8)); let i = 1; a[i] = -3; return a[i]`, "-3"},
		{"non-extensible", `const a = Object.preventExtensions(new Uint8Array(2)); let i = 1; a[i] = 5; a[i + 1] = 6; return [a[i], a[i + 1], Object.isExtensible(a)].join()`, "5,,false"},
		{"proxy", `const a = new Uint8Array(2); const log = []; const p = new Proxy(a, {get(t, k, r) { log.push("get " + String(k)); return Reflect.get(t, k); }, set(t, k, v) { log.push("set " + String(k)); return Reflect.set(t, k, v); }}); let i = 1; p[i] = 3; return [p[i], a[i], log.join(" ")].join()`, "3,3,set 1 get 1"},
		{"receiver of a subclass", `class M extends Float64Array { get extra() { return 1; } } const a = new M(2); let i = 1; a[i] = 2.5; return [a[i], a.extra].join()`, "2.5,1"},
	})
	runMutableProtoCases(t, []protoCase{
		{"no prototype lookup", `const a = new Uint8Array(1); const P = Object.getPrototypeOf(Uint8Array.prototype); let n = 0; Object.defineProperty(P, "0", {get() { n++; return 9; }, set(v) { n++; }, configurable: true}); Object.defineProperty(P, "3", {get() { n++; return 9; }, set(v) { n++; }, configurable: true}); let i = 0; a[i] = 4; a[i + 3] = 4; const r = [a[i], a[i + 3], n]; delete P[0]; delete P[3]; return r.join()`, "4,,0"},
	})
}

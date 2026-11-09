package engine

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFloat16Boundaries(t *testing.T) {
	for _, c := range []struct {
		f    float64
		bits uint16
	}{
		{0, 0x0000},
		{math.Copysign(0, -1), 0x8000},
		{1, 0x3c00},
		{-2, 0xc000},
		{65504, 0x7bff},                  // the largest finite value
		{65519.99, 0x7bff},               // just below the tie with 65536
		{65520, 0x7c00},                  // the tie rounds to even: Infinity
		{1e10, 0x7c00},                   // overflow
		{-1e300, 0xfc00},                 // overflow of the float64 exponent range
		{math.Inf(1), 0x7c00},            //
		{math.Inf(-1), 0xfc00},           //
		{math.Ldexp(1, -14), 0x0400},     // the smallest normal
		{math.Ldexp(1023, -24), 0x03ff},  // the largest subnormal
		{math.Ldexp(1, -24), 0x0001},     // the smallest subnormal
		{math.Ldexp(1, -25), 0x0000},     // half of it: the tie rounds to even, 0
		{math.Ldexp(3, -26), 0x0001},     // above the tie
		{math.Ldexp(3, -25), 0x0002},     // 1.5 units: the tie rounds to even, 2
		{math.Ldexp(5, -25), 0x0002},     // 2.5 units: the tie rounds to even, 2
		{math.Ldexp(2047, -25), 0x0400},  // rounds up into the smallest normal
		{-math.Ldexp(1, -26), 0x8000},    // rounds to -0
		{math.SmallestNonzeroFloat64, 0}, // a float64 subnormal
		{1 + math.Ldexp(1, -11), 0x3c00}, // the tie between 1 and the next: even
		{1 + math.Ldexp(3, -11), 0x3c02}, // the tie between 0x3c01 and 0x3c02: even
		{1 + math.Ldexp(1, -11) + math.Ldexp(1, -40), 0x3c01},
		{0.1, 0x2e66},
		{math.Pi, 0x4248},
	} {
		assert.Equalf(t, c.bits, float64ToFloat16(c.f), "float64ToFloat16(%v)", c.f)
	}
	assert.Equal(t, uint16(0x7e00), float64ToFloat16(math.NaN()))
	assert.True(t, math.IsNaN(float16ToFloat64(0x7e00)))
	assert.True(t, math.IsNaN(float16ToFloat64(0xfc01)))
}

// TestFloat16RoundTrip checks every binary16 value against its neighbours:
// each converts back to itself, the midpoint with the next value rounds to
// whichever of the two is even, and anything off the midpoint rounds to the
// nearer one.
func TestFloat16RoundTrip(t *testing.T) {
	for h := range 0x7c00 {
		for _, sign := range []uint16{0, 0x8000} {
			bits := uint16(h) | sign
			f := float16ToFloat64(bits)
			if float64ToFloat16(f) != bits {
				t.Fatalf("%#04x: %v converts back to %#04x", bits, f, float64ToFloat16(f))
			}
			next := float16ToFloat64(bits + 1)
			if h == 0x7bff {
				next = math.Copysign(65536, f) // where Infinity starts rounding
			}
			mid := (f + next) / 2
			even := bits
			if bits&1 != 0 {
				even = bits + 1
			}
			ulp := math.Abs(next-f) / 1024
			for _, c := range []struct {
				f    float64
				want uint16
			}{{mid, even}, {mid - math.Copysign(ulp, f+next), bits}, {mid + math.Copysign(ulp, f+next), bits + 1}} {
				if got := float64ToFloat16(c.f); got != c.want {
					t.Fatalf("float64ToFloat16(%v) = %#04x, want %#04x", c.f, got, c.want)
				}
			}
		}
	}
}

func TestArrayBufferSemantics(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"construct", `const b = new ArrayBuffer(8); return [b.byteLength, b.maxByteLength, b.resizable, b.detached, Object.prototype.toString.call(b), b instanceof ArrayBuffer].join()`, "8,8,false,false,[object ArrayBuffer],true"},
		{"length conversions", `return [new ArrayBuffer().byteLength, new ArrayBuffer(undefined).byteLength, new ArrayBuffer(2.9).byteLength, new ArrayBuffer("3").byteLength, new ArrayBuffer(-0.5).byteLength].join()`, "0,0,2,3,0"},
		{"negative length", `return new ArrayBuffer(-1)`, "!RangeError"},
		{"too large", `return new ArrayBuffer(2 ** 53 - 1)`, "!RangeError"},
		{"max too large", `return new ArrayBuffer(0, {maxByteLength: 7 * 2 ** 50})`, "!RangeError"},
		{"length above max", `return new ArrayBuffer(10, {maxByteLength: 8})`, "!RangeError"},
		{"length above max before prototype", `const nt = Object.defineProperty(function () {}.bind(null), "prototype", {get() { throw new Error("proto"); }}); try { Reflect.construct(ArrayBuffer, [10, {maxByteLength: 0}], nt); } catch (e) { return e.name; }`, "RangeError"},
		{"prototype before allocation", `const nt = Object.defineProperty(function () {}.bind(null), "prototype", {get() { throw new Error("proto"); }}); try { Reflect.construct(ArrayBuffer, [2 ** 52], nt); } catch (e) { return e.message; }`, "proto"},
		{"call throws", `return ArrayBuffer(1)`, "!TypeError"},
		{"shape", `return [ArrayBuffer.length, ArrayBuffer.name, ArrayBuffer.isView.length, ArrayBuffer[Symbol.species] === ArrayBuffer, ArrayBuffer.prototype.slice.length, ArrayBuffer.prototype.resize.length, ArrayBuffer.prototype.transfer.length, Object.getOwnPropertyNames(ArrayBuffer.prototype).sort().join(" "), Object.getOwnPropertyDescriptor(ArrayBuffer.prototype, "byteLength").get.name].join()`, "1,ArrayBuffer,1,true,2,1,0,byteLength constructor detached maxByteLength resizable resize slice transfer transferToFixedLength,get byteLength"},
		{"slice", `const b = new ArrayBuffer(8); new DataView(b).setUint32(0, 0x01020304); const s = b.slice(1, -4); return [s.byteLength, new DataView(s).getUint16(0), b.slice(-2).byteLength, b.slice(6, 2).byteLength, b.slice(0, Infinity).byteLength, s !== b].join()`, "3,515,2,0,8,true"},
		{"slice species", `class B extends ArrayBuffer {} const s = new B(4).slice(1); return [s instanceof B, s.byteLength].join()`, "true,3"},
		{"slice species too small", `class B extends ArrayBuffer { static get [Symbol.species]() { return function () { return new ArrayBuffer(1); }; } } return new B(4).slice(0)`, "!TypeError"},
		{"slice species same", `const b = new ArrayBuffer(4); b.constructor = {[Symbol.species]: function () { return b; }}; return b.slice(0)`, "!TypeError"},
		{"slice species shared", `const b = new ArrayBuffer(4); b.constructor = {[Symbol.species]: function (n) { return new SharedArrayBuffer(n); }}; return b.slice(0)`, "!TypeError"},
		{"resizable", `const b = new ArrayBuffer(2, {maxByteLength: 8}); const v = new DataView(b); v.setUint8(1, 7); b.resize(6); const a = [b.byteLength, b.maxByteLength, b.resizable, v.byteLength, v.getUint8(1), v.getUint8(5)]; b.resize(1); a.push(v.byteLength); b.resize(4); a.push(v.getUint8(1), b.byteLength); return a.join()`, "6,8,true,6,7,0,1,0,4"},
		{"resize above max", `return new ArrayBuffer(2, {maxByteLength: 8}).resize(9)`, "!RangeError"},
		{"resize fixed", `return new ArrayBuffer(2).resize(1)`, "!TypeError"},
		{"resize order", `const b = new ArrayBuffer(2, {maxByteLength: 8}); try { b.resize({valueOf() { b.transfer(); return 1; }}); } catch (e) { return e.name + b.detached; }`, "TypeErrortrue"},
		{"transfer", `const b = new ArrayBuffer(4, {maxByteLength: 16}); new DataView(b).setUint8(3, 9); const t = b.transfer(); return [b.detached, b.byteLength, b.maxByteLength, t.byteLength, t.maxByteLength, t.resizable, new DataView(t).getUint8(3)].join()`, "true,0,0,4,16,true,9"},
		{"transfer lengths", `const b = new ArrayBuffer(4); new DataView(b).setUint8(1, 5); const t = b.transfer(8); const u = t.transfer(1); return [t.detached, u.byteLength, new DataView(u).getUint8(0), u.resizable].join()`, "true,1,0,false"},
		{"transfer grow zeroes", `const b = new ArrayBuffer(4); new DataView(b).setUint32(0, -1); const s = b.transfer(2).transfer(4); return new DataView(s).getUint32(0)`, "4294901760"},
		{"transferToFixedLength", `const b = new ArrayBuffer(4, {maxByteLength: 16}); const t = b.transferToFixedLength(6); return [t.resizable, t.byteLength, t.maxByteLength, b.detached].join()`, "false,6,6,true"},
		{"transfer above max keeps source", `const b = new ArrayBuffer(4, {maxByteLength: 8}); try { b.transfer(9); } catch (e) { return e.name + b.detached; }`, "RangeErrorfalse"},
		{"transfer detached", `const b = new ArrayBuffer(4); b.transfer(); return b.transfer()`, "!TypeError"},
		{"detached accessors", `const b = new ArrayBuffer(4, {maxByteLength: 8}); b.transfer(); return [b.byteLength, b.maxByteLength, b.resizable, b.detached].join()`, "0,0,true,true"},
		{"detached slice", `const b = new ArrayBuffer(4); b.transfer(); return b.slice(0)`, "!TypeError"},
		{"isView", `const b = new ArrayBuffer(1); return [ArrayBuffer.isView(new DataView(b)), ArrayBuffer.isView(b), ArrayBuffer.isView({}), ArrayBuffer.isView()].join()`, "true,false,false,false"},
		{"receivers", `return Object.getOwnPropertyDescriptor(ArrayBuffer.prototype, "byteLength").get.call(new SharedArrayBuffer(1))`, "!TypeError: Method ArrayBuffer.prototype.byteLength called on incompatible receiver"},
		{"prototype is not a buffer", `return ArrayBuffer.prototype.byteLength`, "!TypeError"},
	})
	runMutableProtoCases(t, []protoCase{
		{"subclass prototype", `class B extends ArrayBuffer { m() { return this.byteLength; } } return new B(3).m()`, "3"},
		{"global is writable", `const A = ArrayBuffer; globalThis.ArrayBuffer = 1; return [typeof A, globalThis.ArrayBuffer, typeof DataView].join()`, "function,1,function"},
	})
}

func TestSharedArrayBufferSemantics(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"construct", `const b = new SharedArrayBuffer(4); return [b.byteLength, b.growable, b.maxByteLength, Object.prototype.toString.call(b), b instanceof ArrayBuffer, "detached" in b, "resize" in b].join()`, "4,false,4,[object SharedArrayBuffer],false,false,false"},
		{"grow", `const b = new SharedArrayBuffer(2, {maxByteLength: 8}); const v = new DataView(b); v.setUint8(1, 3); b.grow(6); b.grow(6); return [b.growable, b.byteLength, b.maxByteLength, v.byteLength, v.getUint8(1), v.getUint8(5)].join()`, "true,6,8,6,3,0"},
		{"grow shrinks", `const b = new SharedArrayBuffer(4, {maxByteLength: 8}); return b.grow(2)`, "!RangeError"},
		{"grow above max", `return new SharedArrayBuffer(4, {maxByteLength: 8}).grow(9)`, "!RangeError"},
		{"grow fixed", `return new SharedArrayBuffer(4).grow(4)`, "!TypeError"},
		{"slice", `const b = new SharedArrayBuffer(4); new DataView(b).setUint8(2, 8); const s = b.slice(2); return [s instanceof SharedArrayBuffer, s.byteLength, new DataView(s).getUint8(0)].join()`, "true,2,8"},
		{"slice species not shared", `const b = new SharedArrayBuffer(4); b.constructor = {[Symbol.species]: function (n) { return new ArrayBuffer(n); }}; return b.slice(0)`, "!TypeError"},
		{"receivers", `return SharedArrayBuffer.prototype.slice.call(new ArrayBuffer(1))`, "!TypeError"},
		{"not detachable", `return typeof new SharedArrayBuffer(1).transfer`, "undefined"},
		{"shape", `return [SharedArrayBuffer.length, SharedArrayBuffer[Symbol.species] === SharedArrayBuffer, Object.getOwnPropertyNames(SharedArrayBuffer.prototype).sort().join(" ")].join()`, "1,true,byteLength constructor grow growable maxByteLength slice"},
	})
}

func TestDataViewSemantics(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"construct", `const b = new ArrayBuffer(8); const v = new DataView(b, 2, 4); return [v.buffer === b, v.byteOffset, v.byteLength, new DataView(b, 3).byteLength, new DataView(b, 8).byteLength, Object.prototype.toString.call(v)].join()`, "true,2,4,5,0,[object DataView]"},
		{"offset out of range", `return new DataView(new ArrayBuffer(4), 5)`, "!RangeError"},
		{"length out of range", `return new DataView(new ArrayBuffer(4), 1, 4)`, "!RangeError"},
		{"not a buffer", `return new DataView({})`, "!TypeError"},
		{"call throws", `return DataView(new ArrayBuffer(1))`, "!TypeError"},
		{"detached", `const b = new ArrayBuffer(4); b.transfer(); return new DataView(b)`, "!TypeError"},
		{"detached by prototype read", `const b = new ArrayBuffer(4); const nt = Object.defineProperty(function () {}.bind(null), "prototype", {get() { b.transfer(); return DataView.prototype; }}); return Reflect.construct(DataView, [b, 0], nt)`, "!TypeError"},
		{"detached by length conversion", `const b = new ArrayBuffer(4); return new DataView(b, 0, {valueOf() { b.transfer(); return 4; }})`, "!TypeError"},
		{"grown by length conversion", `const b = new ArrayBuffer(8, {maxByteLength: 16}); return new DataView(b, 0, {valueOf() { b.resize(16); return 12; }})`, "!RangeError"},
		{"shared grown by length conversion", `const b = new SharedArrayBuffer(4, {maxByteLength: 16}); return new DataView(b, 0, {valueOf() { b.grow(16); return 8; }})`, "!RangeError"},
		{"shrunk by prototype read", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const nt = Object.defineProperty(function () {}.bind(null), "prototype", {get() { b.resize(1); return DataView.prototype; }}); return Reflect.construct(DataView, [b, 0, 2], nt)`, "!RangeError"},
		{"endianness", `const v = new DataView(new ArrayBuffer(8)); v.setUint32(0, 0x11223344); v.setUint16(4, 0x5566, true); return [v.getUint8(0), v.getUint32(0, true).toString(16), v.getUint16(4).toString(16), v.getUint16(4, true).toString(16), v.getInt32(0) === 0x11223344].join()`, "17,44332211,6655,5566,true"},
		{"integer wrapping", `const v = new DataView(new ArrayBuffer(8)); v.setInt8(0, 200); v.setUint8(1, -1); v.setInt16(2, 40000); v.setUint32(4, -2); return [v.getInt8(0), v.getUint8(1), v.getInt16(2), v.getUint16(2), v.getUint32(4), v.getInt32(4)].join()`, "-56,255,-25536,40000,4294967294,-2"},
		{"conversions", `const v = new DataView(new ArrayBuffer(8)); v.setInt8(0, 2 ** 40 + 3.9); v.setUint8(1, NaN); v.setUint16(2, Infinity); v.setInt32(4, "-7"); return [v.getInt8(0), v.getUint8(1), v.getUint16(2), v.getInt32(4)].join()`, "3,0,0,-7"},
		{"floats", `const v = new DataView(new ArrayBuffer(8)); v.setFloat32(0, 1.1); const a = [v.getFloat32(0)]; v.setFloat64(0, -0); a.push(Object.is(v.getFloat64(0), -0)); v.setFloat64(0, NaN); a.push(v.getFloat64(0)); v.setFloat32(0, 2 ** 128); a.push(v.getFloat32(0)); v.setFloat64(0, Math.PI, true); a.push(v.getFloat64(0, true)); return a.join()`, "1.100000023841858,true,NaN,Infinity,3.141592653589793"},
		{"float16", `const v = new DataView(new ArrayBuffer(2)); const out = []; for (const x of [1.1, 65504, 65520, -0, 2 ** -24, 2 ** -25, 3 * 2 ** -25, NaN, 1 / 3]) { v.setFloat16(0, x); out.push(Object.is(v.getFloat16(0), -0) ? "-0" : v.getFloat16(0), v.getUint16(0).toString(16)); } v.setFloat16(0, 1, true); out.push(v.getUint16(0, true).toString(16)); return out.join()`, "1.099609375,3c66,65504,7bff,Infinity,7c00,-0,8000,5.960464477539063e-8,1,0,0,1.1920928955078125e-7,2,NaN,7e00,0.333251953125,3555,3c00"},
		{"bigint get", `const v = new DataView(new ArrayBuffer(8)); v.setUint32(0, 0xffffffff); v.setUint32(4, 0xfffffffe); return [v.getBigInt64(0), v.getBigUint64(0), v.getBigUint64(0, true), typeof v.getBigInt64(0)].join()`, "-2,18446744073709551614,18374686479671623679,bigint"},
		{"bigint set", `const v = new DataView(new ArrayBuffer(16)); v.setBigInt64(0, -2n); v.setBigUint64(8, 2n ** 64n + 5n, true); const a = [v.getUint32(0).toString(16), v.getUint32(4).toString(16), v.getUint8(8), v.getBigInt64(8, true)]; v.setBigInt64(0, 2n ** 63n); a.push(v.getBigInt64(0), v.getBigUint64(0)); v.setBigUint64(0, -1n); a.push(v.getBigInt64(0)); v.setBigInt64(0, true); v.setBigUint64(8, "0x10"); a.push(v.getBigInt64(0), v.getBigUint64(8)); return a.join()`, "ffffffff,fffffffe,5,5,-9223372036854775808,9223372036854775808,-1,1,16"},
		{"bigint set number", `return new DataView(new ArrayBuffer(8)).setBigInt64(0, 1)`, "!TypeError"},
		{"bigint set bad string", `return new DataView(new ArrayBuffer(8)).setBigUint64(0, "1.5")`, "!SyntaxError"},
		{"bigint set order", `const log = []; const v = new DataView(new ArrayBuffer(4)); try { v.setBigInt64({valueOf() { log.push("index"); return 0; }}, {valueOf() { log.push("value"); return 1n; }}); } catch (e) { log.push(e.name); } return log.join()`, "index,value,RangeError"},
		{"bounds", `const v = new DataView(new ArrayBuffer(4), 1); return v.getUint16(2)`, "!RangeError: Offset is outside the bounds of the DataView"},
		{"negative index", `return new DataView(new ArrayBuffer(4)).getUint8(-1)`, "!RangeError"},
		{"index conversion", `const v = new DataView(new ArrayBuffer(4)); v.setUint8(1, 5); return [v.getUint8(1.7), v.getUint8("1"), v.getUint8(), v.getUint8(null)].join()`, "5,5,0,0"},
		{"set conversion order", `const log = []; const v = new DataView(new ArrayBuffer(1)); try { v.setUint16({valueOf() { log.push("index"); return 0; }}, {valueOf() { log.push("value"); return 1; }}); } catch (e) { log.push(e.name); } return log.join()`, "index,value,RangeError"},
		{"index conversion detaches", `const b = new ArrayBuffer(4); const v = new DataView(b); try { v.getUint8({valueOf() { b.transfer(); return 0; }}); } catch (e) { return e.name; }`, "TypeError"},
		{"length tracking", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const v = new DataView(b, 2); const f = new DataView(b, 1, 2); const a = [v.byteLength, f.byteLength]; b.resize(8); a.push(v.byteLength, f.byteLength); b.resize(2); a.push(v.byteLength); b.resize(1); try { v.byteLength; } catch (e) { a.push(e.name); } try { f.byteOffset; } catch (e) { a.push(e.name); } b.resize(3); a.push(f.byteLength, v.byteLength); return a.join()`, "2,2,6,2,0,TypeError,TypeError,2,1"},
		{"detached view", `const b = new ArrayBuffer(4); const v = new DataView(b); b.transfer(); const a = [v.buffer === b]; for (const f of [() => v.byteLength, () => v.byteOffset, () => v.getUint8(0), () => v.setUint8(0, 1)]) { try { f(); } catch (e) { a.push(e.name); } } return a.join()`, "true,TypeError,TypeError,TypeError,TypeError"},
		{"shared buffer", `const b = new SharedArrayBuffer(4); const v = new DataView(b); v.setInt16(2, -2); return [v.getInt16(2), v.buffer === b].join()`, "-2,true"},
		{"shape", `const p = DataView.prototype; return [DataView.length, p.getInt8.length, p.getFloat64.length, p.setInt8.length, p.setFloat16.length, Object.getOwnPropertyNames(p).length, "getUint8Clamped" in p, Object.getOwnPropertyDescriptor(p, "buffer").get.name].join()`, "1,1,1,2,2,26,false,get buffer"},
		{"receivers", `return DataView.prototype.getUint8.call(new ArrayBuffer(1), 0)`, "!TypeError: Method DataView.prototype.getUint8 called on incompatible receiver"},
	})
}

// TestResizeBytes checks when a resized buffer keeps its backing array:
// the gained bytes are zero, and a result much smaller than the array gets
// one of its own.
func TestResizeBytes(t *testing.T) {
	r := NewRealm()
	for _, c := range []struct {
		name            string
		len, cap, n     int
		maxLen          int
		wantCap         int
		wantSameBacking bool
	}{
		{"fixed same length", 64, 64, 64, -1, 64, true},
		{"fixed slightly shorter", 64, 64, 60, -1, 64, true},
		{"fixed much shorter", 64, 64, 16, -1, 16, false},
		{"fixed to zero", 64, 64, 0, -1, 0, false},
		{"fixed longer", 64, 64, 100, -1, 100, false},
		{"resizable shrink to a quarter", 64, 64, 16, 64, 64, true},
		{"resizable shrink below a quarter", 64, 64, 15, 64, 16, false}, // newBytes
		{"resizable grow within capacity", 16, 64, 48, 64, 64, true},
		{"resizable grow past capacity", 64, 64, 65, 1000, 128, false},
		{"resizable grow past twice the capacity", 64, 64, 200, 1000, 200, false},
		{"resizable grow capped at the maximum", 64, 64, 65, 100, 100, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := make([]byte, c.len, c.cap)
			for i := range data[:cap(data)] {
				data[:cap(data)][i] = 0xaa
			}
			got, err := r.resizeBytes(data, c.n, c.maxLen)
			require.NoError(t, err)
			assert.Len(t, got, c.n)
			assert.Equal(t, c.wantCap, cap(got))
			assert.Equal(t, c.wantSameBacking, c.n > 0 && &got[:1][0] == &data[:1][0])
			for i, v := range got {
				want := byte(0xaa)
				if i >= c.len {
					want = 0
				}
				if v != want {
					t.Fatalf("byte %d = %#x, want %#x", i, v, want)
				}
			}
		})
	}
}

// TestResizeBytesGrowth checks that growing a resizable buffer in small
// steps reallocates a logarithmic number of times, not once per step.
func TestResizeBytesGrowth(t *testing.T) {
	r := NewRealm()
	const step, maxLen = 4096, 16 << 20
	var data []byte
	moves := 0
	for n := step; n <= maxLen; n += step {
		grown, err := r.resizeBytes(data, n, maxLen)
		require.NoError(t, err)
		if cap(grown) != cap(data) {
			moves++
		}
		data = grown
	}
	assert.Equal(t, maxLen, len(data))
	assert.Equal(t, maxLen, cap(data))
	assert.LessOrEqual(t, moves, 13) // 4 KiB doubled up to 16 MiB
}

// TestBufferTransferRetention checks that transferring or shrinking a
// large buffer to a few bytes does not keep the large block reachable,
// while a same-length transfer still moves the bytes without a copy.
func TestBufferTransferRetention(t *testing.T) {
	fx := evalModuleWith(t, `
const M = 1 << 20;
export const a = new ArrayBuffer(M).transfer(16);
export const b = new ArrayBuffer(M).transferToFixedLength(0);
const rb = new ArrayBuffer(M, {maxByteLength: M}); rb.resize(16);
export const c = rb;
export const d = new ArrayBuffer(M, {maxByteLength: M}).transfer(16);
export const src = new ArrayBuffer(M);
export function move() { return src.transfer(); }`, RealmOptions{})
	for name, want := range map[string]int{"a": 16, "b": 0, "c": 16, "d": 16} {
		v, _ := fx.env.GetBindingValue(name)
		data := extDataOf(v.AsObject(), kindArrayBuffer).data
		assert.Equal(t, want, len(data), name)
		assert.Equal(t, want, cap(data), name)
	}
	sv, _ := fx.env.GetBindingValue("src")
	first := &extDataOf(sv.AsObject(), kindArrayBuffer).data[0]
	fn, _ := fx.env.GetBindingValue("move")
	moved, err := fx.r.Call(fn, Undefined(), nil)
	require.NoError(t, err)
	assert.Same(t, first, &extDataOf(moved.AsObject(), kindArrayBuffer).data[0], "a same-length transfer moves the backing array")
}

// TestDetachArrayBuffer checks the host hook ($262.detachArrayBuffer): the
// buffer reads as detached and its views go out of bounds, while a
// SharedArrayBuffer or a non-buffer is rejected.
func TestDetachArrayBuffer(t *testing.T) {
	for _, shared := range []bool{false, true} {
		fx := evalModuleWith(t, `
export const b = new ArrayBuffer(4), s = new SharedArrayBuffer(4), v = new DataView(b);
export function f() {
	const a = [b.detached, b.byteLength];
	try { v.getUint8(0); } catch (e) { a.push(e.name); }
	try { b.slice(); } catch (e) { a.push(e.name); }
	return a.join();
}`, RealmOptions{SharedIntrinsics: shared})
		bv, _ := fx.env.GetBindingValue("b")
		sv, _ := fx.env.GetBindingValue("s")
		assert.NoError(t, fx.r.DetachArrayBuffer(bv))
		assert.NoError(t, fx.r.DetachArrayBuffer(bv), "detaching twice is allowed")
		assert.Equal(t, "true,0,TypeError,TypeError", fx.call("f"))
		assert.ErrorContains(t, fx.r.DetachArrayBuffer(sv), "is not an ArrayBuffer")
		assert.ErrorContains(t, fx.r.DetachArrayBuffer(IntValue(1)), "is not an ArrayBuffer")
	}
}

// TestBinaryNames checks the names the binary group interns after package
// init (binaryNames): each is the key hosts and compiled code get for it,
// whether or not another table registered it as a static atom.
func TestBinaryNames(t *testing.T) {
	for _, shared := range []bool{true, false} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		b := r.binaryIntr()
		for _, o := range []*Object{b.ArrayBufferCtor, b.ArrayBufferPrototype, b.SharedArrayBufferPrototype, b.DataViewPrototype} {
			for _, k := range o.OwnPropertyKeys() {
				if k.IsString() {
					assert.Equal(t, InternKey(k.GoString()), k, k.GoString())
				}
			}
		}
		assert.False(t, isStaticAtom(binaryNames().byteLength))
		assert.True(t, isStaticAtom(binaryNames().buffer) == (staticAtoms["buffer"] != nil))
	}
}

package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestInterpLoopFastPaths covers the edges of the operators run finishes
// inline (int32 bitwise operands, the integer remainder, strict equality by
// identity, boolean tests of booleans, numbers and objects) next to the
// operands that leave those paths, and environment scopes left by a throw.
func TestInterpLoopFastPaths(t *testing.T) {
	f := evalModule(t, `
const show = (v) => Object.is(v, -0) ? "-0" : String(v);
export function bitwise() {
  const xs = [0, -0, 1, -1, 5, 2147483647, -2147483648, 2147483648, 4294967295, 4294967296, 1.5, -1.5, NaN, Infinity, -Infinity, 1e21, 2 ** 53];
  const out = [];
  for (const x of xs) for (const y of [0, 1, 3, 31, 32, 33, -1, -32, 1.5, NaN]) {
    out.push([x & y, x | y, x ^ y, x << y, x >> y, x >>> y].map(show).join(","));
  }
  return out.join(";");
}
export function remainder() {
  const xs = [0, -0, 1, 7, -7, 2 ** 53, 2 ** 53 + 2, -(2 ** 53), 9007199254740991, 1.5, 0.5, NaN, Infinity, -Infinity];
  const out = [];
  for (const x of xs) for (const y of [1, 3, 7, -3, 0, -0, 2 ** 53, 2 ** 53 + 2, 0.5, 1.5, NaN, Infinity]) out.push(show(x % y));
  return out.join(",");
}
export function arith() {
  const out = [];
  for (const [x, y] of [[1, 2], [0, -0], [-0, 0], [1, 0], [-1, 0], [0, 0], [Infinity, Infinity], [1e308, 10]]) {
    out.push([x - y, x * y, x / y, x ** y].map(show).join(","));
  }
  out.push(String("6" - 2), String("6" * "2"), String(null / 1), String(10n - 3n), String(7n % 4n));
  return out.join(";");
}
export function strict() {
  const o = {}, s1 = "ab", s2 = ["a", "b"].join(""), b1 = 10n ** 20n, b2 = 10n ** 20n;
  const pairs = [[NaN, NaN], [0, -0], [1, 1], [o, o], [o, {}], [s1, s2], [s1, "ac"], [b1, b2], [b1, 1n],
    [undefined, null], [null, null], [undefined, undefined], [true, true], [true, 1], ["1", 1], [s1, o], [Symbol.iterator, Symbol.iterator]];
  return pairs.map(([x, y]) => (x === y ? "T" : "F") + (x !== y ? "t" : "f")).join(",");
}
export function truthy() {
  const vs = [true, false, 0, -0, NaN, 1, "", "0", null, undefined, {}, 0n, 1n, Symbol(),
    -1, 0.5, Infinity, -Infinity, [], () => 0, 0 / 0, -(0 / 0), [, 1][0]];
  return vs.map((v) => { let s = (!v ? "n" : "y"); if (v) s += "T"; else s += "F"; while (v) { s += "w"; break; } return s; }).join(",");
}
export function scopes(n) {
  const fs = [];
  let caught = 0;
  for (let i = 0; i < n; i++) {
    try {
      let j = i * 2;
      fs.push(() => i + j);
      if (i % 3 === 0) { let k = i; fs.push(() => k); throw new Error("x" + k); }
    } catch (e) { caught += e.message.length; }
  }
  return fs.map((g) => g()).join(",") + ":" + caught;
}
export function* gen() {
  for (let i = 0; i < 3; i++) { let j = i; try { yield () => i + j; } finally { j = -1; } }
}
export function genRun() { return [...gen()].map((g) => g()).join(","); }
`)
	assert.Equal(t, bitwiseWant, f.call("bitwise"))
	assert.Equal(t, "0,0,0,0,NaN,NaN,0,0,0,0,NaN,0,-0,-0,-0,-0,NaN,NaN,-0,-0,-0,-0,NaN,-0,0,1,1,1,NaN,NaN,1,1,0,1,NaN,1,0,1,0,1,NaN,NaN,7,7,0,1,NaN,7,-0,-1,-0,-1,NaN,NaN,-7,-7,-0,-1,NaN,-7,0,2,4,2,NaN,NaN,0,9007199254740992,0,0.5,NaN,9007199254740992,0,1,6,1,NaN,NaN,2,0,0,1,NaN,9007199254740994,-0,-2,-4,-2,NaN,NaN,-0,-9007199254740992,-0,-0.5,NaN,-9007199254740992,0,1,3,1,NaN,NaN,9007199254740991,9007199254740991,0,1,NaN,9007199254740991,0.5,1.5,1.5,1.5,NaN,NaN,1.5,1.5,0,0,NaN,1.5,0.5,0.5,0.5,0.5,NaN,NaN,0.5,0.5,0,0.5,NaN,0.5,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN,NaN", f.call("remainder"))
	assert.Equal(t, "-1,2,0.5,1;0,-0,NaN,1;-0,-0,NaN,1;1,0,Infinity,1;-1,-0,-Infinity,1;0,0,NaN,1;NaN,Infinity,NaN,Infinity;1e+308,Infinity,1e+307,Infinity;4;12;0;7;3", f.call("arith"))
	assert.Equal(t, "Ft,Tf,Tf,Tf,Ft,Tf,Ft,Tf,Ft,Ft,Tf,Tf,Tf,Ft,Ft,Ft,Tf", f.call("strict"))
	assert.Equal(t, "yTw,nF,nF,nF,nF,yTw,nF,yTw,nF,nF,yTw,nF,yTw,yTw,yTw,yTw,yTw,yTw,yTw,yTw,nF,nF,nF", f.call("truthy"))
	assert.Equal(t, "0,0,3,6,9,3,12,15,18,6,21,24,27,9:8", f.call("scopes", 10))
	assert.Equal(t, "-1,0,1", f.call("genRun"))
}

// bitwiseWant is what V8 (node) returns for TestInterpLoopFastPaths'
// bitwise; the other expectations there are V8's too.
const bitwiseWant = "0,0,0,0,0,0;0,1,1,0,0,0;0,3,3,0,0,0;0,31,31,0,0,0;0,32,32,0,0,0;0,33,33,0,0,0;0,-1,-1,0,0,0;0,-32,-32,0,0,0;0,1,1,0,0,0;0,0,0,0,0,0;0,0,0,0,0,0;0,1,1,0,0,0;0,3,3,0,0,0;0,31,31,0,0,0;0,32,32,0,0,0;0,33,33,0,0,0;0,-1,-1,0,0,0;0,-32,-32,0,0,0;0,1,1,0,0,0;0,0,0,0,0,0;0,1,1,1,1,1;1,1,0,2,0,0;1,3,2,8,0,0;1,31,30,-2147483648,0,0;0,33,33,1,1,1;1,33,32,2,0,0;1,-1,-2,-2147483648,0,0;0,-31,-31,1,1,1;1,1,0,2,0,0;0,1,1,1,1,1;0,-1,-1,-1,-1,4294967295;1,-1,-2,-2,-1,2147483647;3,-1,-4,-8,-1,536870911;31,-1,-32,-2147483648,-1,1;32,-1,-33,-1,-1,4294967295;33,-1,-34,-2,-1,2147483647;-1,-1,0,-2147483648,-1,1;-32,-1,31,-1,-1,4294967295;1,-1,-2,-2,-1,2147483647;0,-1,-1,-1,-1,4294967295;0,5,5,5,5,5;1,5,4,10,2,2;1,7,6,40,0,0;5,31,26,-2147483648,0,0;0,37,37,5,5,5;1,37,36,10,2,2;5,-1,-6,-2147483648,0,0;0,-27,-27,5,5,5;1,5,4,10,2,2;0,5,5,5,5,5;0,2147483647,2147483647,2147483647,2147483647,2147483647;1,2147483647,2147483646,-2,1073741823,1073741823;3,2147483647,2147483644,-8,268435455,268435455;31,2147483647,2147483616,-2147483648,0,0;32,2147483647,2147483615,2147483647,2147483647,2147483647;33,2147483647,2147483614,-2,1073741823,1073741823;2147483647,-1,-2147483648,-2147483648,0,0;2147483616,-1,-2147483617,2147483647,2147483647,2147483647;1,2147483647,2147483646,-2,1073741823,1073741823;0,2147483647,2147483647,2147483647,2147483647,2147483647;0,-2147483648,-2147483648,-2147483648,-2147483648,2147483648;0,-2147483647,-2147483647,0,-1073741824,1073741824;0,-2147483645,-2147483645,0,-268435456,268435456;0,-2147483617,-2147483617,0,-1,1;0,-2147483616,-2147483616,-2147483648,-2147483648,2147483648;0,-2147483615,-2147483615,0,-1073741824,1073741824;-2147483648,-1,2147483647,0,-1,1;-2147483648,-32,2147483616,-2147483648,-2147483648,2147483648;0,-2147483647,-2147483647,0,-1073741824,1073741824;0,-2147483648,-2147483648,-2147483648,-2147483648,2147483648;0,-2147483648,-2147483648,-2147483648,-2147483648,2147483648;0,-2147483647,-2147483647,0,-1073741824,1073741824;0,-2147483645,-2147483645,0,-268435456,268435456;0,-2147483617,-2147483617,0,-1,1;0,-2147483616,-2147483616,-2147483648,-2147483648,2147483648;0,-2147483615,-2147483615,0,-1073741824,1073741824;-2147483648,-1,2147483647,0,-1,1;-2147483648,-32,2147483616,-2147483648,-2147483648,2147483648;0,-2147483647,-2147483647,0,-1073741824,1073741824;0,-2147483648,-2147483648,-2147483648,-2147483648,2147483648;0,-1,-1,-1,-1,4294967295;1,-1,-2,-2,-1,2147483647;3,-1,-4,-8,-1,536870911;31,-1,-32,-2147483648,-1,1;32,-1,-33,-1,-1,4294967295;33,-1,-34,-2,-1,2147483647;-1,-1,0,-2147483648,-1,1;-32,-1,31,-1,-1,4294967295;1,-1,-2,-2,-1,2147483647;0,-1,-1,-1,-1,4294967295;0,0,0,0,0,0;0,1,1,0,0,0;0,3,3,0,0,0;0,31,31,0,0,0;0,32,32,0,0,0;0,33,33,0,0,0;0,-1,-1,0,0,0;0,-32,-32,0,0,0;0,1,1,0,0,0;0,0,0,0,0,0;0,1,1,1,1,1;1,1,0,2,0,0;1,3,2,8,0,0;1,31,30,-2147483648,0,0;0,33,33,1,1,1;1,33,32,2,0,0;1,-1,-2,-2147483648,0,0;0,-31,-31,1,1,1;1,1,0,2,0,0;0,1,1,1,1,1;0,-1,-1,-1,-1,4294967295;1,-1,-2,-2,-1,2147483647;3,-1,-4,-8,-1,536870911;31,-1,-32,-2147483648,-1,1;32,-1,-33,-1,-1,4294967295;33,-1,-34,-2,-1,2147483647;-1,-1,0,-2147483648,-1,1;-32,-1,31,-1,-1,4294967295;1,-1,-2,-2,-1,2147483647;0,-1,-1,-1,-1,4294967295;0,0,0,0,0,0;0,1,1,0,0,0;0,3,3,0,0,0;0,31,31,0,0,0;0,32,32,0,0,0;0,33,33,0,0,0;0,-1,-1,0,0,0;0,-32,-32,0,0,0;0,1,1,0,0,0;0,0,0,0,0,0;0,0,0,0,0,0;0,1,1,0,0,0;0,3,3,0,0,0;0,31,31,0,0,0;0,32,32,0,0,0;0,33,33,0,0,0;0,-1,-1,0,0,0;0,-32,-32,0,0,0;0,1,1,0,0,0;0,0,0,0,0,0;0,0,0,0,0,0;0,1,1,0,0,0;0,3,3,0,0,0;0,31,31,0,0,0;0,32,32,0,0,0;0,33,33,0,0,0;0,-1,-1,0,0,0;0,-32,-32,0,0,0;0,1,1,0,0,0;0,0,0,0,0,0;0,-559939584,-559939584,-559939584,-559939584,3735027712;0,-559939583,-559939583,-1119879168,-279969792,1867513856;0,-559939581,-559939581,-184549376,-69992448,466878464;0,-559939553,-559939553,0,-1,1;0,-559939552,-559939552,-559939584,-559939584,3735027712;0,-559939551,-559939551,-1119879168,-279969792,1867513856;-559939584,-1,559939583,0,-1,1;-559939584,-32,559939552,-559939584,-559939584,3735027712;0,-559939583,-559939583,-1119879168,-279969792,1867513856;0,-559939584,-559939584,-559939584,-559939584,3735027712;0,0,0,0,0,0;0,1,1,0,0,0;0,3,3,0,0,0;0,31,31,0,0,0;0,32,32,0,0,0;0,33,33,0,0,0;0,-1,-1,0,0,0;0,-32,-32,0,0,0;0,1,1,0,0,0;0,0,0,0,0,0"

// TestInterpTypedArrayElements covers the typed array element reads and
// writes run makes without getElemSlow and setElemSlow: a Float64Array in
// place (a NaN payload, -0, offsets, keys that are not indices, no
// prototype lookup, a converted write, resizable, length-tracking and
// detached buffers) and the other element types through
// typedArrayGetNumber and typedArraySetNumber. The expectation is V8's.
func TestInterpTypedArrayElements(t *testing.T) {
	f := evalModule(t, `export function f64() {
  const out = [];
  const buf = new ArrayBuffer(32), dv = new DataView(buf), a = new Float64Array(buf), off = new Float64Array(buf, 8, 2);
  dv.setUint32(0, 0x7ff80000 | 0x1234, true); dv.setUint32(4, 0x7ff80000, true); // a NaN with a payload
  out.push(Object.is(a[0], NaN), a[0] !== a[0]);
  a[1] = 1.5; a[2] = -0; a[3] = "2.5";
  out.push(a[1], Object.is(a[2], -0), a[3], off[0], off[1], off[2], a[4], a[-1], a[1.5], a[NaN], a[-0]);
  Float64Array.prototype[7] = "proto"; Object.prototype[1.5] = "proto";
  out.push(a[7], a[1.5]);
  a[1.5] = 9; a[9] = 9; a[-0] = 7;
  out.push(a[0], a.length, Object.keys(a).join());
  let n = 0;
  const v = { valueOf() { n++; return 4; } };
  a[1] = v; a[10] = v;
  out.push(a[1], n);
  const rab = new ArrayBuffer(16, { maxByteLength: 32 }), track = new Float64Array(rab);
  track[1] = 3; rab.resize(32); track[3] = 5;
  out.push(track[1], track[3], track.length);
  rab.resize(8);
  out.push(track[1], track.length);
  const det = new ArrayBuffer(16), da = new Float64Array(det);
  da[0] = 1; det.transfer(); da[0] = 2;
  out.push(da[0], da.length);
  const i32 = new Int32Array(2), u8 = new Uint8Array(2), u8c = new Uint8ClampedArray(1);
  i32[0] = 2 ** 31; u8[0] = -1; u8c[0] = 300; i32[1] = 1.9;
  out.push(i32[0], u8[0], u8c[0], i32[1]);
  delete Float64Array.prototype[7]; delete Object.prototype[1.5];
  return out.map((x) => Object.is(x, -0) ? "-0" : String(x)).join(",");
}
`)
	assert.Equal(t, "true,true,1.5,true,2.5,1.5,-0,undefined,undefined,undefined,undefined,undefined,NaN,undefined,undefined,7,4,0,1,2,3,4,2,3,5,4,undefined,1,undefined,0,-2147483648,255,255,1", f.call("f64"))
}

// TestInterpElementBounds covers the dense element bounds in GetElem,
// GetElemInc and SetElem, and keys that must take the property path.
func TestInterpElementBounds(t *testing.T) {
	f := evalModule(t, `
export function bounds() {
  const keys = [-1, -2147483648, -4294967296, -(2 ** 63), 2147483648,
                4294967295, 4294967296, 2 ** 63, 1.5, NaN, Infinity, -Infinity];
  const a = [10, 20], out = [];
  for (let j = 0; j < keys.length; j++) {
    const k = keys[j];
    out.push(a[k] === undefined);
    a[k] = j + 30;
    out.push(a[k] === j + 30);
  }
  out.push(a[0] === 10, a[1] === 20, a.length === 2147483649);
  return out.every(x => x);
}
export function increment() {
  const a = [10, 20], out = [];
  a[-1] = 7;
  for (let k of [-2, -1, 0, 1, -2147483649, 4294967295, 1.5, NaN, Infinity])
    out.push(String(a[++k]));
  return out.join();
}
export function rounding() {
  const a = [10, 20, 30];
  for (let k of [-2, -1, -0, 0, 1, 1.5, 2 ** 52 - 0.5, 2 ** 53 - 1,
                2 ** 53, 2 ** 63, -(2 ** 63), NaN, Infinity, -Infinity,
                "1", true, null, undefined]) {
    const want = Number(k) + 1;
    const v = a[++k];
    if (!Object.is(k, want) || v !== a[want]) return false;
  }
  return true;
}
export function edges() {
  const a = [10, , 30];
  const p = Object.create(Array.prototype, {1: {value: 20, writable: true}});
  Object.setPrototypeOf(a, p);
  function get(k) { return a[k]; }
  function set(k, v) { a[k] = v; }
  const out = [get(-0), get(1), get(3)];
  set(-0, 11); set(1, 21); set(3, 40);
  out.push(get(0), get(1), get(3), a.length);
  return out.map(String).join();
}
`)
	assert.Equal(t, true, f.call("bounds"))
	assert.Equal(t, "7,10,20,undefined,undefined,undefined,undefined,undefined,undefined", f.call("increment"))
	assert.Equal(t, true, f.call("rounding"))
	assert.Equal(t, "10,20,undefined,11,21,40,4", f.call("edges"))
}

package engine

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUint8ArrayBase64Shape(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"statics", `return ["fromBase64", "fromHex"].map(k => [Uint8Array[k].name, Uint8Array[k].length].join(":")).join()`, "fromBase64:1,fromHex:1"},
		{"methods", `return ["toBase64", "setFromBase64", "toHex", "setFromHex"].map(k => [Uint8Array.prototype[k].name, Uint8Array.prototype[k].length].join(":")).join()`, "toBase64:0,setFromBase64:1,toHex:0,setFromHex:1"},
		{"hidden", `return [Object.keys(Uint8Array), Object.keys(Uint8Array.prototype), Object.getOwnPropertyNames(Uint8Array.prototype).sort()].join("|")`, "||BYTES_PER_ELEMENT,constructor,setFromBase64,setFromHex,toBase64,toHex"},
		{"only Uint8Array", `return [Int8Array, Uint8ClampedArray, Float64Array].flatMap(c => ["fromBase64", "fromHex"].map(k => k in c).concat(["toBase64", "setFromHex"].map(k => k in c.prototype))).includes(true)`, "false"},
		{"not constructors", `return new Uint8Array.fromHex("")`, "!TypeError"},
		{"statics ignore this", `const u = Uint8Array.fromBase64.call(Int8Array, "Zg=="), h = Uint8Array.fromHex.call(undefined, "ff"); return [Object.getPrototypeOf(u) === Uint8Array.prototype, u.buffer.resizable, u.byteLength, h[0]].join()`, "true,false,1,255"},
	})
	runMutableProtoCases(t, []protoCase{
		{"descriptor", `const d = Object.getOwnPropertyDescriptor(Uint8Array.prototype, "toHex"), s = Object.getOwnPropertyDescriptor(Uint8Array, "fromBase64"); return [d.writable, d.enumerable, d.configurable, s.writable, s.enumerable, s.configurable].join()`, "true,false,true,true,false,true"},
	})
}

func TestUint8ArrayFromBase64(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"chunks", `return ["Zm9vYmFy", "Zm9vYmE=", "Zm9vYg==", "Zm9vYmE", "Zm9vYg", ""].map(s => Uint8Array.fromBase64(s).join()).join("|")`, "102,111,111,98,97,114|102,111,111,98,97|102,111,111,98|102,111,111,98,97|102,111,111,98|"},
		{"whitespace", `return [" Z g = = ", "\tZm9v\nYmFy\f\r", "Zg=\n=", "Zg== "].map(s => Uint8Array.fromBase64(s).join()).join("|")`, "102|102,111,111,98,97,114|102|102"},
		{"loose extra bits", `return [Uint8Array.fromBase64("Zh==").join(), Uint8Array.fromBase64("Zm9vYh").join()].join("|")`, "102|102,111,111,98"},
		{"lone sextet", `return Uint8Array.fromBase64("Zm9vY")`, "!SyntaxError: Uint8Array.fromBase64: the string is not valid base64"},
		{"one pad", `return Uint8Array.fromBase64("Zg=")`, "!SyntaxError"},
		{"pad first", `return Uint8Array.fromBase64("Zm9v=")`, "!SyntaxError"},
		{"pad after one", `return Uint8Array.fromBase64("Z===")`, "!SyntaxError"},
		{"after padding", `return Uint8Array.fromBase64("Zg==Zg==")`, "!SyntaxError"},
		{"three pads", `return Uint8Array.fromBase64("Zg===")`, "!SyntaxError"},
		{"pad then char", `return Uint8Array.fromBase64("Zg=x")`, "!SyntaxError"},
		{"invalid char", `return Uint8Array.fromBase64("Zm9v.mFy")`, "!SyntaxError"},
		{"vertical tab", `return Uint8Array.fromBase64("Zm9v\vYmFy")`, "!SyntaxError"},
		{"not ascii", `return Uint8Array.fromBase64("Zm9vYmFyé")`, "!SyntaxError"},
		{"url", `return [Uint8Array.fromBase64("-_8", {alphabet: "base64url"}).join(), Uint8Array.fromBase64("+/8=").join()].join("|")`, "251,255|251,255"},
		{"url rejects +", `return Uint8Array.fromBase64("+/8=", {alphabet: "base64url"})`, "!SyntaxError"},
		{"base64 rejects -", `return Uint8Array.fromBase64("-_8=")`, "!SyntaxError"},
		{"strict", `return ["Zm9vYg==", "Zm9vYmE=", "Zm9vYmFy"].map(s => Uint8Array.fromBase64(s, {lastChunkHandling: "strict"}).join()).join("|")`, "102,111,111,98|102,111,111,98,97|102,111,111,98,97,114"},
		{"strict unpadded", `return Uint8Array.fromBase64("Zm9vYg", {lastChunkHandling: "strict"})`, "!SyntaxError"},
		{"strict extra bits 2", `return Uint8Array.fromBase64("Zh==", {lastChunkHandling: "strict"})`, "!SyntaxError"},
		{"strict extra bits 3", `return Uint8Array.fromBase64("Zm9vYmF=", {lastChunkHandling: "strict"})`, "!SyntaxError"},
		{"stop-before-partial", `return ["Zm9vYg", "Zm9vY", "Zm9vYg=", "Zm9vYg==", "Zm9vYmE=", "Zm9v "].map(s => Uint8Array.fromBase64(s, {lastChunkHandling: "stop-before-partial"}).join()).join("|")`, "102,111,111|102,111,111|102,111,111|102,111,111,98|102,111,111,98,97|102,111,111"},
		{"stop-before-partial pad then char", `return Uint8Array.fromBase64("Zm9vYg=x", {lastChunkHandling: "stop-before-partial"})`, "!SyntaxError"},
		{"trimmed", `const u = Uint8Array.fromBase64(" ".repeat(1000) + "Zg=="); return [u.length, u.buffer.byteLength, u[0]].join()`, "1,1,102"},
		{"not a string", `return Uint8Array.fromBase64(new String("Zg=="))`, "!TypeError: Uint8Array.fromBase64: the argument is not a string"},
		{"no argument", `return Uint8Array.fromBase64()`, "!TypeError"},
		{"options not an object", `return Uint8Array.fromBase64("Zg==", null)`, "!TypeError: Uint8Array.fromBase64: the options are not an object"},
		{"options string", `return Uint8Array.fromBase64("Zg==", "base64")`, "!TypeError"},
		{"function options", `const o = () => {}; o.alphabet = "base64url"; return Uint8Array.fromBase64("_w", o)[0]`, "255"},
		{"inherited options", `return Uint8Array.fromBase64("_w", Object.create({alphabet: "base64url"}))[0]`, "255"},
		{"alphabet case", `return Uint8Array.fromBase64("Zg==", {alphabet: "BASE64"})`, "!TypeError: Uint8Array.fromBase64: invalid alphabet option \"BASE64\""},
		{"alphabet object", `return Uint8Array.fromBase64("Zg==", {alphabet: new String("base64")})`, "!TypeError"},
		{"alphabet null", `return Uint8Array.fromBase64("Zg==", {alphabet: null})`, "!TypeError"},
		{"handling", `return Uint8Array.fromBase64("Zg==", {lastChunkHandling: "Strict"})`, "!TypeError: Uint8Array.fromBase64: invalid lastChunkHandling option \"Strict\""},
		{"order", `const log = []; const o = {get alphabet() { log.push("alphabet"); return "base64" }, get lastChunkHandling() { log.push("lastChunkHandling") }}; Uint8Array.fromBase64("Zg==", o); return log.join()`, "alphabet,lastChunkHandling"},
		{"bad alphabet stops", `const log = []; try { Uint8Array.fromBase64("Zg==", {alphabet: "x", get lastChunkHandling() { log.push("read") }}) } catch (e) { log.push(e.name) } return log.join()`, "TypeError"},
		{"string first", `const log = []; try { Uint8Array.fromBase64(1, {get alphabet() { log.push("read") }}) } catch (e) { log.push(e.name) } return log.join()`, "TypeError"},
		{"options before syntax", `return Uint8Array.fromBase64("é", {alphabet: "x"})`, "!TypeError"},
		{"options throw", `return Uint8Array.fromBase64("Zg==", {get alphabet() { throw new RangeError("x") }})`, "!RangeError"},
	})
}

func TestUint8ArraySetFromBase64(t *testing.T) {
	// set(n, s, opts) decodes s into a Uint8Array of n bytes set to 255
	// and returns read, written and the bytes, or the error and the bytes.
	const set = `const set = (n, s, opts) => { const t = new Uint8Array(n).fill(255); try { const r = t.setFromBase64(s, opts); return [r.read, r.written, t.join()].join(";") } catch (e) { return [e.name, t.join()].join(";") } };`
	runProtoCases(t, []protoCase{
		{"fits", set + `return set(6, "Zm9vYmFy")`, "8;6;102,111,111,98,97,114"},
		{"room left", set + `return set(8, "Zm9vYg==")`, "8;4;102,111,111,98,255,255,255,255"},
		{"last chunk fits", set + `return set(5, "Zm9vYmE")`, "7;5;102,111,111,98,97"},
		{"last chunk of 3 into 1", set + `return set(4, "Zm9vYmE")`, "4;3;102,111,111,255"},
		{"last chunk of 2 into 1", set + `return set(4, "Zm9vYg")`, "6;4;102,111,111,98"},
		{"chunk into 2", set + `return set(2, "Zm9vYmFy")`, "0;0;255,255"},
		{"chunk of 3 into 2", set + `return set(2, "Zm9")`, "3;2;102,111"},
		{"full", set + `return set(3, "Zm9v YmFy")`, "4;3;102,111,111"},
		{"full then invalid", set + `return set(3, "Zm9v!")`, "4;3;102,111,111"},
		{"full then not ascii", set + `return set(3, "Zm9vé")`, "4;3;102,111,111"},
		{"empty target", set + `return set(0, "!!!")`, "0;0;"},
		{"padded", set + `return set(4, "Zm9vYg== ")`, "9;4;102,111,111,98"},
		{"error writes chunks", set + `return set(6, "Zm9vYm!=")`, "SyntaxError;102,111,111,255,255,255"},
		{"not ascii writes chunks", set + `return set(6, "Zm9vé")`, "SyntaxError;102,111,111,255,255,255"},
		{"strict writes chunks", set + `return set(6, "Zm9vYg", {lastChunkHandling: "strict"})`, "SyntaxError;102,111,111,255,255,255"},
		{"stop-before-partial", set + `return set(6, "Zm9vYg", {lastChunkHandling: "stop-before-partial"})`, "4;3;102,111,111,255,255,255"},
		{"url", set + `return set(2, "-_8", {alphabet: "base64url"})`, "3;2;251,255"},
		{"subarray", `const b = new Uint8Array(6); const r = b.subarray(2, 5).setFromBase64("Zm9v"); return [r.read, r.written, b.join(), Object.keys(r), Object.getPrototypeOf(r) === Object.prototype].join(";")`, "4;3;0,0,102,111,111,0;read,written;true"},
		{"shared", `const t = new Uint8Array(new SharedArrayBuffer(3)); t.setFromBase64("Zm9v"); return t.join()`, "102,111,111"},
		{"length-tracking", `const b = new ArrayBuffer(8, {maxByteLength: 8}); const t = new Uint8Array(b, 2); b.resize(5); return JSON.stringify(t.setFromBase64("Zm9vYmFy"))`, `{"read":4,"written":3}`},
		{"clamped", `return Uint8Array.prototype.setFromBase64.call(new Uint8ClampedArray(3), "Zm9v")`, "!TypeError: Method Uint8Array.prototype.setFromBase64 called on incompatible receiver"},
		{"int8", `return Uint8Array.prototype.setFromBase64.call(new Int8Array(3), "Zm9v")`, "!TypeError"},
		{"prototype", `return Uint8Array.prototype.setFromBase64("Zm9v")`, "!TypeError"},
		{"receiver first", `return Uint8Array.prototype.setFromBase64.call({}, 1)`, "!TypeError: Method Uint8Array.prototype.setFromBase64 called on incompatible receiver"},
		{"not a string", `return new Uint8Array(3).setFromBase64(1)`, "!TypeError: Uint8Array.prototype.setFromBase64: the argument is not a string"},
		{"detached", `const t = new Uint8Array(3); t.buffer.transfer(); return t.setFromBase64("Zm9v")`, "!TypeError: Cannot perform Uint8Array.prototype.setFromBase64 on a detached or out-of-bounds TypedArray"},
		{"out of bounds", `const b = new ArrayBuffer(8, {maxByteLength: 8}); const t = new Uint8Array(b, 4, 4); b.resize(6); return t.setFromBase64("Zm9v")`, "!TypeError"},
		{"options before bounds", `const log = []; const t = new Uint8Array(3); t.buffer.transfer(); try { t.setFromBase64("Zm9v", {get alphabet() { log.push("alphabet") }, get lastChunkHandling() { log.push("lastChunkHandling") }}) } catch (e) { log.push(e.name) } return log.join()`, "alphabet,lastChunkHandling,TypeError"},
		{"options detach", `const t = new Uint8Array(3); return t.setFromBase64("Zm9v", {get alphabet() { t.buffer.transfer() }})`, "!TypeError"},
		{"options shrink", `const b = new ArrayBuffer(6, {maxByteLength: 6}); const t = new Uint8Array(b); const r = t.setFromBase64("Zm9vYmFy", {get alphabet() { b.resize(3) }}); return [r.read, r.written, t.join()].join(";")`, "4;3;102,111,111"},
	})
}

func TestUint8ArrayToBase64(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"padding", `return [[], [102], [102, 111], [102, 111, 111], [102, 111, 111, 98]].map(a => new Uint8Array(a).toBase64()).join("|")`, "|Zg==|Zm8=|Zm9v|Zm9vYg=="},
		{"omitPadding", `return [[102], [102, 111], [102, 111, 111]].map(a => new Uint8Array(a).toBase64({omitPadding: true})).join("|")`, "Zg|Zm8|Zm9v"},
		{"url", `const u = new Uint8Array([251, 255]); return [u.toBase64(), u.toBase64({alphabet: "base64url"}), u.toBase64({alphabet: "base64url", omitPadding: true})].join("|")`, "+/8=|-_8=|-_8"},
		{"omitPadding coerced", `const u = new Uint8Array([102]); return [u.toBase64({omitPadding: "false"}), u.toBase64({omitPadding: 0}), u.toBase64({omitPadding: {}})].join("|")`, "Zg|Zg==|Zg"},
		{"subarray", `return new Uint8Array([0, 102, 111, 111, 0]).subarray(1, 4).toBase64()`, "Zm9v"},
		{"order", `const log = []; new Uint8Array(1).toBase64({get alphabet() { log.push("alphabet") }, get omitPadding() { log.push("omitPadding") }, get lastChunkHandling() { log.push("lastChunkHandling") }}); return log.join()`, "alphabet,omitPadding"},
		{"bad alphabet stops", `const log = []; try { new Uint8Array(1).toBase64({alphabet: "url", get omitPadding() { log.push("read") }}) } catch (e) { log.push(e.name) } return log.join()`, "TypeError"},
		{"options before bounds", `const log = []; const t = new Uint8Array(3); t.buffer.transfer(); try { t.toBase64({get omitPadding() { log.push("omitPadding") }}) } catch (e) { log.push(e.name) } return log.join()`, "omitPadding,TypeError"},
		{"options shrink", `const b = new ArrayBuffer(6, {maxByteLength: 6}); const t = new Uint8Array(b); t.set([102, 111, 111, 98]); return t.toBase64({get omitPadding() { b.resize(3) }})`, "Zm9v"},
		{"options not an object", `return new Uint8Array(1).toBase64(null)`, "!TypeError"},
		{"clamped", `return Uint8Array.prototype.toBase64.call(new Uint8ClampedArray(1))`, "!TypeError"},
		{"detached", `const t = new Uint8Array(1); t.buffer.transfer(); return t.toBase64()`, "!TypeError: Cannot perform Uint8Array.prototype.toBase64 on a detached or out-of-bounds TypedArray"},
	})
}

func TestUint8ArrayHex(t *testing.T) {
	const set = `const set = (n, s) => { const t = new Uint8Array(n).fill(255); try { const r = t.setFromHex(s); return [r.read, r.written, t.join()].join(";") } catch (e) { return [e.name, t.join()].join(";") } };`
	runProtoCases(t, []protoCase{
		{"toHex", `return [[], [0, 15, 255, 16], [171, 205]].map(a => new Uint8Array(a).toHex()).join("|")`, "|000fff10|abcd"},
		{"toHex subarray", `return new Uint8Array([1, 2, 3]).subarray(1).toHex()`, "0203"},
		{"toHex detached", `const t = new Uint8Array(1); t.buffer.transfer(); return t.toHex()`, "!TypeError"},
		{"toHex int8", `return Uint8Array.prototype.toHex.call(new Int8Array(1))`, "!TypeError"},
		{"fromHex", `return [Uint8Array.fromHex("000fFf10").join(), Uint8Array.fromHex("").length].join("|")`, "0,15,255,16|0"},
		{"fromHex odd", `return Uint8Array.fromHex("abc")`, "!SyntaxError: Uint8Array.fromHex: the string is not valid hex"},
		{"fromHex invalid", `return Uint8Array.fromHex("0g")`, "!SyntaxError"},
		{"fromHex whitespace", `return Uint8Array.fromHex(" 0 0")`, "!SyntaxError"},
		{"fromHex not ascii", `return Uint8Array.fromHex("0é")`, "!SyntaxError"},
		{"fromHex not a string", `return Uint8Array.fromHex(0)`, "!TypeError"},
		{"set", set + `return [set(4, "aabb"), set(4, "aabbccddee"), set(0, "zz")].join("|")`, "4;2;170,187,255,255|8;4;170,187,204,221|0;0;"},
		{"set odd", set + `return [set(4, "aabbc"), set(0, "z")].join("|")`, "SyntaxError;255,255,255,255|SyntaxError;"},
		{"set invalid", set + `return set(4, "aabbzz")`, "SyntaxError;170,187,255,255"},
		{"set not ascii", set + `return [set(1, "aaéb"), set(2, "aaéb")].join("|")`, "2;1;170|SyntaxError;170,255"},
		{"set clamped", `return Uint8Array.prototype.setFromHex.call(new Uint8ClampedArray(1), "aa")`, "!TypeError"},
		{"set not a string", `return new Uint8Array(1).setFromHex(170)`, "!TypeError"},
		{"set detached", `const t = new Uint8Array(1); t.buffer.transfer(); return t.setFromHex("zz")`, "!TypeError"},
	})
}

// TestUint8ArrayDecodeInPlace checks that setFromBase64 and setFromHex read
// a string that is not ASCII in place: a long ASCII prefix before the unit
// that is not costs no allocation, whatever the target's size.
func TestUint8ArrayDecodeInPlace(t *testing.T) {
	r := NewRealm()
	u := make([]uint16, 1<<16)
	for i := range u {
		u[i] = 'a'
	}
	u = append(u, 0xE9, 'a') // an even length, for hex
	s := FromUTF16(u)
	dst := make([]byte, 3)
	for _, c := range []struct {
		name string
		f    func() (int, int, bool, error)
	}{
		{"base64", func() (int, int, bool, error) { return r.decodeBase64(dst, s, false, base64Loose, true) }},
		{"hex", func() (int, int, bool, error) { return r.decodeHex(dst, s) }},
	} {
		allocs := testing.AllocsPerRun(20, func() {
			if read, written, ok, err := c.f(); err != nil || !ok || written != 3 || read == 0 {
				t.Fatalf("%s: read %d, written %d, ok %v, err %v", c.name, read, written, ok, err)
			}
		})
		assert.Zero(t, allocs, c.name)
	}
}

// TestUint8ArrayBase64Interrupts checks that the encoders and decoders
// honour a pending interrupt on inputs well above their check interval.
func TestUint8ArrayBase64Interrupts(t *testing.T) {
	cases := []struct{ name, mk, method string }{
		{"toBase64", `new Uint8Array(1 << 23)`, "toBase64"},
		{"toHex", `new Uint8Array(1 << 23)`, "toHex"},
		{"fromBase64", `"A".repeat(1 << 23)`, "fromBase64"},
		{"fromBase64 whitespace", `" ".repeat(1 << 23)`, "fromBase64"},
		{"fromHex", `"a".repeat(1 << 23)`, "fromHex"},
		{"setFromBase64", `"A".repeat(1 << 23)`, "setFromBase64"},
		{"setFromBase64 not ascii", `"A".repeat(1 << 23) + "é"`, "setFromBase64"},
		{"setFromHex", `"a".repeat(1 << 23)`, "setFromHex"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, elapsed, err := auditBNativeInterrupt(t, `function mk() { return `+c.mk+`; }`, func(r *Realm, big Value) (Value, error) {
				r.ClearInterrupt()
				ctor, err := r.Global.GetProp(r, r.KeyFromGoString("Uint8Array"))
				require.NoError(t, err)
				recv, args := big, []Value{big}
				switch c.method {
				case "fromBase64", "fromHex":
					recv = ctor
				case "setFromBase64", "setFromHex":
					dst, err := r.allocTypedArray(r.binaryIntr().typedArrayPrototypes[elemUint8], elemUint8, 1<<23)
					require.NoError(t, err)
					recv = ObjectValue(dst)
				default:
					args = nil
				}
				r.Interrupt("x")
				return auditBMethod(t, r, recv, c.method, args...)
			})
			assert.True(t, ok, "%s ran %s with an interrupt pending: %v", c.name, elapsed, err)
		})
	}
}

// TestUint8ArrayBase64StringLimit checks that toBase64 and toHex raise a
// RangeError for a result over the string length limit.
func TestUint8ArrayBase64StringLimit(t *testing.T) {
	lowerMaxStringLength(t, 1000)
	f := evalModule(t, `
export function f(which) {
  switch (which) {
  case "base64": return new Uint8Array(751).toBase64().length;
  case "base64 max": return new Uint8Array(750).toBase64().length;
  case "unpadded": return new Uint8Array(751).toBase64({omitPadding: true}).length;
  case "unpadded max": return new Uint8Array(749).toBase64({omitPadding: true}).length;
  case "hex": return new Uint8Array(501).toHex().length;
  case "hex max": return new Uint8Array(500).toHex().length;
  }
}`)
	for which, want := range map[string]string{
		"base64":       "!RangeError",
		"base64 max":   "1000",
		"unpadded":     "!RangeError",
		"unpadded max": "999",
		"hex":          "!RangeError",
		"hex max":      "1000",
	} {
		got, err := f.callErr("f", which)
		if want[0] == '!' {
			require.Error(t, err, which)
			assert.Contains(t, err.Error(), want[1:], which)
			continue
		}
		require.NoError(t, err, which)
		assert.Equal(t, want, fmt.Sprint(got), which)
	}
}

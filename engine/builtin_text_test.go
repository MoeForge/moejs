package engine

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTextShape(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"globals", `return [typeof TextEncoder, TextEncoder.length, TextEncoder.name, TextDecoder.length, TextDecoder.name, Object.getOwnPropertyDescriptor(globalThis, "TextDecoder").enumerable].join()`, "function,0,TextEncoder,0,TextDecoder,false"},
		{"members", `return [Object.keys(TextEncoder.prototype), Object.keys(TextDecoder.prototype), Object.getOwnPropertyNames(TextEncoder.prototype)].join("|")`, "encoding,encode,encodeInto|encoding,fatal,ignoreBOM,decode|constructor,encoding,encode,encodeInto"},
		{"tags", `return [new TextEncoder(), new TextDecoder(), TextEncoder.prototype].map(o => Object.prototype.toString.call(o)).join()`, "[object TextEncoder],[object TextDecoder],[object TextEncoder]"},
		{"constructor", `return [TextEncoder.prototype.constructor === TextEncoder, Object.getPrototypeOf(new TextDecoder()) === TextDecoder.prototype, Object.getPrototypeOf(TextEncoder.prototype) === Object.prototype].join()`, "true,true,true"},
		{"call", `return TextEncoder()`, "!TypeError: Constructor TextEncoder requires 'new'"},
		{"call decoder", `return TextDecoder("utf-8")`, "!TypeError"},
		{"subclass", `class E extends TextEncoder {} class D extends TextDecoder {} const e = new E(), d = new D("utf8", {fatal: true}); return [e instanceof E, e.encode("a")[0], e.encoding, d.fatal, d.decode(e.encode("é"))].join()`, "true,97,utf-8,true,é"},
		{"new.target", `function F() {} F.prototype = Array.prototype; return Reflect.construct(TextEncoder, [], F) instanceof Array`, "true"},
		{"extensible", `const e = new TextEncoder(); e.x = 1; return [e.x, Object.keys(e).join(), JSON.stringify(new TextDecoder())].join()`, "1,x,{}"},
		{"encode receiver", `return TextEncoder.prototype.encode.call({}, "a")`, "!TypeError: Method TextEncoder.prototype.encode called on incompatible receiver"},
		{"encodeInto receiver", `return TextEncoder.prototype.encodeInto.call(new TextDecoder(), "a", new Uint8Array(1))`, "!TypeError"},
		{"encoding receiver", `return TextEncoder.prototype.encoding`, "!TypeError"},
		{"decoder getter receiver", `return Object.getOwnPropertyDescriptor(TextDecoder.prototype, "ignoreBOM").get.call(new TextEncoder())`, "!TypeError"},
		{"decode receiver", `return TextDecoder.prototype.decode.call(Object.create(TextDecoder.prototype))`, "!TypeError"},
	})
	// The shared realms freeze the intrinsics.
	runMutableProtoCases(t, []protoCase{
		{"getter", `const d = Object.getOwnPropertyDescriptor(TextDecoder.prototype, "fatal"); return [typeof d.get, d.set, d.enumerable, d.configurable, d.get.name, d.get.length].join()`, "function,,true,true,get fatal,0"},
		{"method", `const d = Object.getOwnPropertyDescriptor(TextEncoder.prototype, "encodeInto"); return [d.writable, d.enumerable, d.configurable, d.value.length, TextEncoder.prototype.encode.length, TextDecoder.prototype.decode.length].join()`, "true,true,true,2,0,0"},
		{"tag", `const d = Object.getOwnPropertyDescriptor(TextDecoder.prototype, Symbol.toStringTag); return [d.value, d.writable, d.enumerable, d.configurable].join()`, "TextDecoder,false,false,true"},
	})
}

func TestTextDecoderConstruct(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"labels", `return ["utf-8", "UTF8", " unicode-1-1-utf-8\n", "\tx-unicode20utf8\f", "unicode11utf8", "Unicode20UTF8\r", undefined].map(l => new TextDecoder(l).encoding).join()`, "utf-8,utf-8,utf-8,utf-8,utf-8,utf-8,utf-8"},
		{"defaults", `const d = new TextDecoder(); return [d.encoding, d.fatal, d.ignoreBOM].join()`, "utf-8,false,false"},
		{"options", `const d = new TextDecoder(undefined, {fatal: 1, ignoreBOM: "x"}), n = new TextDecoder("utf-8", null); return [d.fatal, d.ignoreBOM, n.fatal, n.ignoreBOM].join()`, "true,true,false,false"},
		{"inherited options", `return new TextDecoder("utf-8", Object.create({fatal: true})).fatal`, "true"},
		{"latin1", `return new TextDecoder("latin1")`, `!RangeError: The "latin1" encoding is not supported`},
		{"utf-16le", `return new TextDecoder("utf-16le")`, "!RangeError"},
		{"empty", `return new TextDecoder("")`, "!RangeError"},
		{"null", `return new TextDecoder(null)`, "!RangeError"},
		{"vertical tab", `return new TextDecoder("utf-8\v")`, "!RangeError"},
		{"nbsp", `return new TextDecoder("utf-8 ")`, "!RangeError"},
		{"dotted I", `return new TextDecoder("İutf-8")`, "!RangeError"},
		{"symbol", `return new TextDecoder(Symbol())`, "!TypeError"},
		{"options not an object", `return new TextDecoder("utf-8", 1)`, "!TypeError: The provided value is not of type 'TextDecoderOptions'"},
		{"order", `const log = []; const label = {toString() { log.push("label"); return "latin1" }}; const opts = {get fatal() { log.push("fatal"); return 1 }, get ignoreBOM() { log.push("ignoreBOM") }}; try { new TextDecoder(label, opts) } catch (e) { log.push(e.name) } return log.join()`, "label,fatal,ignoreBOM,RangeError"},
		{"prototype before label", `const log = []; const nt = new Proxy(function () {}, {get(t, k) { log.push(String(k)); return t[k] }}); try { Reflect.construct(TextDecoder, ["latin1", {get fatal() { log.push("fatal") }}], nt) } catch (e) { log.push(e.name) } return log.join()`, "fatal,prototype,RangeError"},
		{"options throw", `return new TextDecoder("latin1", {get fatal() { throw new SyntaxError("x") }})`, "!SyntaxError"},
	})
}

func TestTextEncoderEncode(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"code points", `return Array.from(new TextEncoder().encode("hé€\u{1F600}\uD800x\uDC00")).join()`, "104,195,169,226,130,172,240,159,152,128,239,191,189,120,239,191,189"},
		{"reversed pair", `return Array.from(new TextEncoder().encode("\uDC00\uD800")).join()`, "239,191,189,239,191,189"},
		{"boundaries", `return Array.from(new TextEncoder().encode("\x7f\x80߿ࠀ￿\u{10000}\u{10ffff}")).join()`, "127,194,128,223,191,224,160,128,239,191,191,240,144,128,128,244,143,191,191"},
		{"result", `const u = new TextEncoder().encode(); return [u.constructor === Uint8Array, Object.getPrototypeOf(u) === Uint8Array.prototype, u.length, u.buffer.byteLength, u.buffer.resizable].join()`, "true,true,0,0,false"},
		{"conversions", `const e = new TextEncoder(); return [e.encode(undefined).length, e.encode(null).length, e.encode(12).join(), e.encode({toString: () => "é"}).length].join()`, "0,4,49,50,2"},
		{"symbol", `return new TextEncoder().encode(Symbol())`, "!TypeError"},
		{"rope", `const s = "ab".repeat(1000) + "é" + "c"; const u = new TextEncoder().encode(s); return [u.length, u[2000], u[2001], u[2002]].join()`, "2003,195,169,99"},
		{"fresh buffer", `const e = new TextEncoder(); const a = e.encode("x"), b = e.encode("x"); return a.buffer === b.buffer`, "false"},
	})
}

func TestTextEncoderEncodeInto(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"fits", `const u = new Uint8Array(8); const r = new TextEncoder().encodeInto("aé\uD800", u); return [r.read, r.written, Array.from(u), Object.keys(r), Object.getPrototypeOf(r) === Object.prototype].join("|")`, "3|6|97,195,169,239,191,189,0,0|read,written|true"},
		{"pair does not fit", `const r = new TextEncoder().encodeInto("a\u{1F600}b", new Uint8Array(4)); return [r.read, r.written].join()`, "1,1"},
		{"pair read as two", `const r = new TextEncoder().encodeInto("\u{1F600}b", new Uint8Array(5)); return [r.read, r.written].join()`, "3,5"},
		{"ascii truncated", `const u = new Uint8Array(3); const r = new TextEncoder().encodeInto("abcdef", u); return [r.read, r.written, Array.from(u)].join()`, "3,3,97,98,99"},
		{"view", `const b = new ArrayBuffer(6); const r = new TextEncoder().encodeInto("abcd", new Uint8Array(b, 2, 3)); return [r.read, r.written, Array.from(new Uint8Array(b))].join()`, "3,3,0,0,97,98,99,0"},
		{"shared", `const u = new Uint8Array(new SharedArrayBuffer(2)); const r = new TextEncoder().encodeInto("abc", u); return [r.read, r.written, Array.from(u)].join()`, "2,2,97,98"},
		{"empty", `const r = new TextEncoder().encodeInto("", new Uint8Array(0)); return [r.read, r.written].join()`, "0,0"},
		{"detached", `const u = new Uint8Array(4); u.buffer.transfer(); const r = new TextEncoder().encodeInto("abc", u); return [r.read, r.written].join()`, "0,0"},
		{"length tracking", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const u = new Uint8Array(b); b.resize(2); const r = new TextEncoder().encodeInto("abc", u); return [r.read, r.written, u.length].join()`, "2,2,2"},
		{"out of bounds", `const b = new ArrayBuffer(4, {maxByteLength: 8}); const u = new Uint8Array(b, 2, 2); b.resize(1); const r = new TextEncoder().encodeInto("abc", u); return [r.read, r.written].join()`, "0,0"},
		{"clamped", `return new TextEncoder().encodeInto("a", new Uint8ClampedArray(1))`, "!TypeError: TextEncoder.prototype.encodeInto: the destination is not a Uint8Array"},
		{"int8", `return new TextEncoder().encodeInto("a", new Int8Array(1))`, "!TypeError"},
		{"buffer", `return new TextEncoder().encodeInto("a", new ArrayBuffer(1))`, "!TypeError"},
		{"dataview", `return new TextEncoder().encodeInto("a", new DataView(new ArrayBuffer(1)))`, "!TypeError"},
		{"array", `return new TextEncoder().encodeInto("a", [0])`, "!TypeError"},
		{"one argument", `return new TextEncoder().encodeInto("a")`, "!TypeError: TextEncoder.prototype.encodeInto requires 2 arguments, but only 1 present"},
		{"count before conversion", `let n = 0; try { new TextEncoder().encodeInto({toString() { n++; return "" }}) } catch (e) { return n + e.name }`, "0TypeError"},
		{"source first", `const log = []; try { new TextEncoder().encodeInto({toString() { log.push("source"); return "a" }}, {}) } catch (e) { log.push(e.name) } return log.join()`, "source,TypeError"},
	})
}

func TestTextDecoderDecode(t *testing.T) {
	const lib = `const td = new TextDecoder(); const hex = s => Array.from(s, c => c.codePointAt(0).toString(16)).join(" "); const dec = (bytes, opts) => hex(new TextDecoder("utf-8", opts).decode(new Uint8Array(bytes)));`
	cases := []protoCase{
		{"inputs", `const b = new Uint8Array([104, 105, 33, 0xE2, 0x82, 0xAC]).buffer; return [td.decode(b), td.decode(new Uint8Array(b, 1, 2)), td.decode(new DataView(b, 3)), td.decode(new Uint16Array(b, 0, 1)), td.decode(), td.decode(undefined), td.decode(new Uint8Array(new SharedArrayBuffer(1)).fill(65).buffer), td.decode(new Float64Array(0))].join("|")`, "hi!€|i!|€|hi|||A|"},
		{"string", `return td.decode("abc")`, "!TypeError: TextDecoder.prototype.decode: the input is not an ArrayBuffer or a view of one"},
		{"array", `return td.decode([65])`, "!TypeError"},
		{"null", `return td.decode(null)`, "!TypeError"},
		{"options not an object", `return td.decode(undefined, "x")`, "!TypeError: The provided value is not of type 'TextDecodeOptions'"},
		{"detached", `const u = new Uint8Array([65]); u.buffer.transfer(); return td.decode(u) + "|" + td.decode(u.buffer)`, "|"},
		{"options before bytes", `const b = new ArrayBuffer(2, {maxByteLength: 4}); new Uint8Array(b).set([65, 66]); return td.decode(b, {get stream() { b.resize(1); return false }})`, "A"},
		{"detached by options", `const b = new Uint8Array([65, 66]).buffer; return td.decode(b, {get stream() { b.transfer() }}).length`, "0"},
		{"input before options", `const log = []; try { td.decode(1, {get stream() { log.push("stream") }}) } catch (e) { log.push(e.name) } return log.join()`, "TypeError"},
		{"not a string copy", `const u = new TextEncoder().encode("héllo"); const s = td.decode(u); u.fill(0); return s`, "héllo"},
		{"ascii", `const s = "x".repeat(100) + "\x7f"; return td.decode(new TextEncoder().encode(s)) === s`, "true"},
		{"round trip", `let s = ""; for (let c = 0; c < 0x110000; c += 97) if (c < 0xD800 || c > 0xDFFF) s += String.fromCodePoint(c); return td.decode(new TextEncoder().encode(s)) === s`, "true"},
		{"bom", `return [dec([0xEF, 0xBB, 0xBF, 65]), dec([0xEF, 0xBB, 0xBF, 65], {ignoreBOM: true}), dec([0xEF, 0xBB, 0xBF, 0xEF, 0xBB, 0xBF]), dec([65, 0xEF, 0xBB, 0xBF]), dec([0xEF, 0xBB, 0xBF])].join("|")`, "41|feff 41|feff|41 feff|"},
		{"bom stream", `const d = new TextDecoder(); return [d.decode(new Uint8Array([0xEF, 0xBB]), {stream: true}), d.decode(new Uint8Array([0xBF, 0x41]), {stream: true}), d.decode(new Uint8Array([0xEF, 0xBB, 0xBF])), d.decode(new Uint8Array([0xEF, 0xBB, 0xBF, 0x42]))].map(hex).join("|")`, "|41|feff|42"},
		{"bom then utf-8", `return [dec([0xEF, 0xBB, 0xBF, 0xC3, 0xA9, 0xEF, 0xBB, 0xBF]), dec([0xEF, 0xBB, 0xBF, 0xFF])].join("|")`, "e9 feff|fffd"},
		{"bom alone streamed", `const d = new TextDecoder(); return [d.decode(new Uint8Array([0xEF, 0xBB, 0xBF]), {stream: true}), d.decode(new Uint8Array([0xEF, 0xBB, 0xBF, 0x41]))].map(hex).join("|")`, "|feff 41"},
		{"bom fatal", `const d = new TextDecoder("utf-8", {fatal: true}); try { d.decode(new Uint8Array([0xEF, 0xBB, 0xBF, 0xFF]), {stream: true}) } catch (e) {} return hex(d.decode(new Uint8Array([0xEF, 0xBB, 0xBF, 0x41])))`, "41"},
		{"bom after ascii", `const d = new TextDecoder(); return [d.decode(new Uint8Array([0x41]), {stream: true}), d.decode(new Uint8Array([0xEF, 0xBB, 0xBF]))].map(hex).join("|")`, "41|feff"},
		{"bom after empty", `const d = new TextDecoder(); return [d.decode(new Uint8Array(0), {stream: true}), d.decode(new Uint8Array([0xEF, 0xBB, 0xBF, 0x41]))].map(hex).join("|")`, "|41"},
		{"stream", `const bytes = [0xF0, 0x9F, 0x98, 0x80, 0x41]; const out = []; for (let i = 0; i <= 5; i++) { const d = new TextDecoder(); out.push(hex(d.decode(new Uint8Array(bytes.slice(0, i)), {stream: true}) + d.decode(new Uint8Array(bytes.slice(i))))) } return out.join("|")`, "1f600 41|1f600 41|1f600 41|1f600 41|1f600 41|1f600 41"},
		{"stream flush", `const d = new TextDecoder(); return [d.decode(new Uint8Array([0xF0, 0x9F]), {stream: true}), d.decode(), d.decode(new Uint8Array([0x98]))].map(hex).join("|")`, "|fffd|fffd"},
		{"stream resets", `const d = new TextDecoder(); d.decode(new Uint8Array([0xE2, 0x82]), {stream: true}); return [hex(d.decode(new Uint8Array([0xAC]))), hex(d.decode(new Uint8Array([0xAC])))].join("|")`, "20ac|fffd"},
		{"replacement", `return [[0x80], [0xC0, 0x80], [0xC2], [0xC2, 0x41], [0xE1, 0x80, 0x41], [0xE0, 0x80], [0xE0, 0x9F, 0xBF], [0xED, 0xA0, 0x80], [0xF4, 0x90, 0x80, 0x80], [0xF5], [0xF0, 0x90, 0x80], [0xF0, 0x90, 0x80, 0x41], [0xFF, 0xFE], [0xEF, 0xBF, 0xBF], [0xF4, 0x8F, 0xBF, 0xBF], [0xED, 0x9F, 0xBF], [0xF0, 0x80, 0x80, 0x80]].map(b => dec(b)).join("|")`, "fffd|fffd fffd|fffd|fffd 41|fffd 41|fffd fffd|fffd fffd fffd|fffd fffd fffd|fffd fffd fffd fffd|fffd|fffd|fffd 41|fffd fffd|ffff|10ffff|d7ff|fffd fffd fffd fffd"},
		{"fatal", `return new TextDecoder("utf-8", {fatal: true}).decode(new Uint8Array([0x41, 0xFF]))`, "!TypeError: The encoded data was not valid for encoding utf-8"},
		{"fatal flush", `return new TextDecoder("utf-8", {fatal: true}).decode(new Uint8Array([0xE2, 0x82]))`, "!TypeError"},
		{"fatal stream", `const d = new TextDecoder("utf-8", {fatal: true}); return d.decode(new Uint8Array([0xE2, 0x82]), {stream: true}).length + d.decode(new Uint8Array([0xAC]))`, "0€"},
		{"fatal goes on", `const d = new TextDecoder("utf-8", {fatal: true}); try { d.decode(new Uint8Array([0xFF, 0x41])) } catch (e) {} return d.decode(new Uint8Array([0x42]))`, "B"},
		{"fatal mid stream", `const d = new TextDecoder("utf-8", {fatal: true}); const out = [d.decode(new Uint8Array([0xE2]), {stream: true}).length]; try { d.decode(new Uint8Array([0x41, 0x42]), {stream: true}) } catch (e) { out.push(e.name) } out.push(d.decode(new Uint8Array([0x43]))); return out.join()`, "0,TypeError,C"},
		{"fatal keeps bom seen", `const d = new TextDecoder("utf-8", {fatal: true}); d.decode(new Uint8Array([0x41]), {stream: true}); try { d.decode(new Uint8Array([0xFF]), {stream: true}) } catch (e) {} return d.decode(new Uint8Array([0xEF, 0xBB, 0xBF])).length`, "1"},
	}
	for i := range cases {
		cases[i].body = lib + cases[i].body
	}
	runProtoCases(t, cases)
}

// refDecoder is the Encoding Standard's UTF-8 decoder and TextDecoder's
// decode steps, a byte at a time, for the differential test of decodeUTF8.
type refDecoder struct {
	fatal, ignoreBOM, doNotFlush, bomSeen bool
	needed, seen                          int
	cp                                    rune
	lower, upper                          int
}

func (d *refDecoder) reset() { d.needed, d.seen, d.cp, d.lower, d.upper = 0, 0, 0, 0x80, 0xBF }

// decode returns the code units of one decode call, ok false for a fatal
// error.
func (d *refDecoder) decode(in []byte, stream bool) (out []uint16, ok bool) {
	if !d.doNotFlush {
		d.reset()
		d.bomSeen = false
	}
	d.doNotFlush = stream
	var cps []rune
	n := len(in)
	if !stream {
		n++ // end-of-queue
	}
	for i := 0; i < n; i++ {
		b := -1
		if i < len(in) {
			b = int(in[i])
		}
		res, isErr := rune(-1), false
		switch {
		case b == -1 && d.needed != 0:
			d.needed, isErr = 0, true
		case b == -1:
		case d.needed == 0:
			switch {
			case b <= 0x7F:
				res = rune(b)
			case b >= 0xC2 && b <= 0xDF:
				d.needed, d.cp = 1, rune(b&0x1F)
			case b >= 0xE0 && b <= 0xEF:
				if b == 0xE0 {
					d.lower = 0xA0
				}
				if b == 0xED {
					d.upper = 0x9F
				}
				d.needed, d.cp = 2, rune(b&0xF)
			case b >= 0xF0 && b <= 0xF4:
				if b == 0xF0 {
					d.lower = 0x90
				}
				if b == 0xF4 {
					d.upper = 0x8F
				}
				d.needed, d.cp = 3, rune(b&0x7)
			default:
				isErr = true
			}
		case b < d.lower || b > d.upper:
			d.reset()
			i-- // prepend b to the queue
			isErr = true
		default:
			d.lower, d.upper = 0x80, 0xBF
			d.cp = d.cp<<6 | rune(b&0x3F)
			if d.seen++; d.seen == d.needed {
				res = d.cp
				d.reset()
			}
		}
		if isErr {
			if d.fatal {
				return nil, false
			}
			res = 0xFFFD
		}
		if res >= 0 {
			cps = append(cps, res)
		}
	}
	for _, c := range cps {
		if !d.ignoreBOM && !d.bomSeen {
			d.bomSeen = true
			if c == 0xFEFF {
				continue
			}
		}
		out = utf16.AppendRune(out, c)
	}
	return out, true
}

// TestTextDecoderDifferential checks decodeUTF8 against refDecoder on
// random inputs rich in the bytes the decoder branches on, split into
// random stream calls, and on sequences across its chunk boundary.
func TestTextDecoderDifferential(t *testing.T) {
	r := NewRealm()
	interesting := []byte{0x00, 0x41, 0x7F, 0x80, 0x8F, 0x90, 0x9F, 0xA0, 0xBB, 0xBF, 0xC0, 0xC1, 0xC2, 0xDF, 0xE0, 0xE1, 0xED, 0xEE, 0xEF, 0xF0, 0xF1, 0xF4, 0xF5, 0xFF}
	rng := rand.New(rand.NewPCG(1, 2))
	check := func(label string, fatal, ignoreBOM bool, chunks [][]byte, streams []bool) {
		t.Helper()
		d := &textDecoder{fatal: fatal, ignoreBOM: ignoreBOM}
		ref := &refDecoder{fatal: fatal, ignoreBOM: ignoreBOM}
		for i, c := range chunks {
			want, ok := ref.decode(c, streams[i])
			got, err := r.decodeUTF8(d, c, streams[i])
			if !ok {
				require.Error(t, err, "%s: call %d of %x", label, i, chunks)
				continue
			}
			if len(want) > maxStringLength {
				// The two decoders' states part here.
				require.ErrorContains(t, err, "Invalid string length", "%s: call %d of %x", label, i, chunks)
				return
			}
			require.NoError(t, err, "%s: call %d of %x", label, i, chunks)
			if !assert.Equal(t, append([]uint16{}, want...), append([]uint16{}, got.UTF16()...), "%s: call %d of %x (fatal %v, ignoreBOM %v, streams %v)", label, i, chunks, fatal, ignoreBOM, streams) {
				t.FailNow()
			}
			require.Equal(t, !slices.ContainsFunc(want, func(c uint16) bool { return c >= 0x80 }), got.IsASCII(), "%s: call %d of %x: an ASCII result is an ASCII string", label, i, chunks)
		}
	}
	random := func(iters, maxLen int) {
		for iter := range iters {
			var in []byte
			for n := rng.IntN(maxLen); len(in) < n; {
				switch rng.IntN(6) {
				case 0:
					in = append(in, byte(rng.IntN(256)))
				case 1:
					in = utf8.AppendRune(in, rune(rng.IntN(0x110000)))
				case 2:
					in = append(in, 0xEF, 0xBB, 0xBF)
				default:
					in = append(in, interesting[rng.IntN(len(interesting))])
				}
			}
			var chunks [][]byte
			var streams []bool
			for rest := in; ; {
				k := rng.IntN(len(rest) + 1)
				if rng.IntN(3) == 0 {
					k = len(rest)
				}
				chunks, streams = append(chunks, rest[:k]), append(streams, rng.IntN(8) != 0)
				if rest = rest[k:]; len(rest) == 0 && rng.IntN(2) == 0 {
					break
				}
			}
			streams[len(streams)-1] = rng.IntN(4) == 0
			check(fmt.Sprint("random ", iter), rng.IntN(4) == 0, rng.IntN(4) == 0, chunks, streams)
		}
	}
	random(20000, 24)
	// Sequences, valid and not, across the chunks of the decoder's loop
	// (after "é") and of its ASCII prefix scan.
	for _, tail := range [][]byte{{0xE2, 0x82, 0xAC}, {0xF0, 0x9F, 0x98, 0x80}, {0xE2, 0x82, 0x41}, {0xFF}, {0xEF, 0xBB, 0xBF}} {
		for k := 1; k <= 4; k++ {
			for _, lead := range []string{"", "é"} {
				in := append([]byte(lead+strings.Repeat("a", copyBytesChunk-k)), tail...)
				in = append(in, 'z')
				label := fmt.Sprintf("%q, tail %x at chunk-%d", lead, tail, k)
				check(label, false, false, [][]byte{in}, []bool{false})
				check(label+" fatal", true, false, [][]byte{in}, []bool{false})
				split := len(in) - len(tail) // after the tail's first byte
				check(label+" split", false, false, [][]byte{in[:split], in[split:]}, []bool{true, false})
			}
		}
	}
	check("ascii", false, false, [][]byte{[]byte(strings.Repeat("a", copyBytesChunk+5))}, []bool{false})
	// Around a lowered string length limit, where the loop's chunks shrink
	// to the room left.
	lowerMaxStringLength(t, 12)
	random(20000, 40)
}

// TestTextDecoderLimitNoGrowth checks that decode throws for an input over
// the string length limit before its code units outgrow their buffer: an
// input far over the limit allocates no more than one just over it.
func TestTextDecoderLimitNoGrowth(t *testing.T) {
	const limit = 4096
	lowerMaxStringLength(t, limit)
	r := NewRealm()
	allocs := func(n int) float64 {
		in := bytes.Repeat([]byte{0xFF}, n)
		return testing.AllocsPerRun(10, func() {
			if _, err := r.decodeUTF8(&textDecoder{}, in, false); err == nil {
				t.Fatalf("decode of %d bytes over a limit of %d succeeded", n, limit)
			}
		})
	}
	want := allocs(limit + 1)
	for _, n := range []int{limit + 3, limit + 64, 3 * copyBytesChunk} {
		assert.Equal(t, want, allocs(n), "%d bytes", n)
	}
}

// TestTextDecoderBOMFastPath checks that ASCII after a BOM takes decode's
// ASCII fast path: it allocates no more than ASCII alone.
func TestTextDecoderBOMFastPath(t *testing.T) {
	r := NewRealm()
	ascii := bytes.Repeat([]byte("a"), 4096)
	bom := append([]byte{0xEF, 0xBB, 0xBF}, ascii...)
	allocs := func(in []byte) float64 {
		d := &textDecoder{}
		return testing.AllocsPerRun(20, func() {
			if s, err := r.decodeUTF8(d, in, false); err != nil || s.Len() != len(ascii) {
				t.Fatalf("decode of %d bytes: %v", len(in), err)
			}
		})
	}
	assert.Equal(t, allocs(ascii), allocs(bom))
}

// TestTextInterrupts checks that encode, encodeInto and decode honour a
// pending interrupt on inputs well above their check interval.
func TestTextInterrupts(t *testing.T) {
	cases := []struct {
		name, mk, method string
		decode           bool
	}{
		{"encode ascii", `"a".repeat(1 << 23)`, "encode", false},
		{"encode utf-16", `"é".repeat(1 << 22)`, "encode", false},
		{"encodeInto ascii", `"a".repeat(1 << 23)`, "encodeInto", false},
		{"encodeInto utf-16", `"é".repeat(1 << 22)`, "encodeInto", false},
		{"decode ascii", `new Uint8Array(1 << 23).fill(65)`, "decode", true},
		{"decode utf-8", `new TextEncoder().encode("é".repeat(1 << 22))`, "decode", true},
		{"decode invalid", `new Uint8Array(1 << 23).fill(0xFF)`, "decode", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, elapsed, err := auditBNativeInterrupt(t, `function mk() { return `+c.mk+`; }`, func(r *Realm, big Value) (Value, error) {
				name := "TextEncoder"
				if c.decode {
					name = "TextDecoder"
				}
				r.ClearInterrupt()
				ctor, err := r.Global.GetProp(r, r.KeyFromGoString(name))
				require.NoError(t, err)
				obj, err := r.Construct(ctor, nil, ctor.AsObject())
				require.NoError(t, err)
				args := []Value{big}
				if c.method == "encodeInto" {
					dst, err := r.allocTypedArray(r.binaryIntr().typedArrayPrototypes[elemUint8], elemUint8, 1<<23)
					require.NoError(t, err)
					args = append(args, ObjectValue(dst))
				}
				r.Interrupt("x")
				return auditBMethod(t, r, obj, c.method, args...)
			})
			assert.True(t, ok, "%s ran %s with an interrupt pending: %v", c.name, elapsed, err)
		})
	}
}

// TestTextDecoderInterruptLatency checks that decode sees an interrupt that
// arrives after its ASCII prefix scan: while it widens a long prefix before
// a sequence that is not ASCII, and while it narrows the ASCII result left
// before a sequence the next call completes. An interrupt fired at any of
// many points through such a call must end it, or let it return, within
// the time the runtime takes to allocate the 64 MiB of code units (which it
// zeroes, and no check can break up) and a sixteenth of the call. Without
// the checks of either loop, one fired as that allocation or the loop's own
// (32 MiB) starts waits for it and then the whole loop. Each call starts
// after a collection, and so does the allocation timed before it, so both
// reuse memory they must zero; none runs during them, as one would stop the
// call for longer than its checks are apart: decoderBallast keeps the heap
// goal far above what a call allocates. A call that ran more than twice its
// time was descheduled, so its point runs again.
func TestTextDecoderInterruptLatency(t *testing.T) {
	decoderBallast = make([]byte, 1<<30)
	t.Cleanup(func() { decoderBallast = nil })
	r := NewRealm()
	ascii := bytes.Repeat([]byte("a"), 32<<20)
	for _, c := range []struct {
		name   string
		in     []byte
		stream bool
	}{
		{"widen", append(ascii, 0xC3, 0xA9), false},
		{"narrow", append(ascii, 0xE2), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			alloc := func() time.Duration {
				runtime.GC()
				start := time.Now()
				units := make([]uint16, len(c.in)+4)
				d := time.Since(start)
				runtime.KeepAlive(units)
				runtime.GC()
				return d
			}
			base := time.Duration(math.MaxInt64)
			for range 2 {
				runtime.GC()
				start := time.Now()
				_, err := r.decodeUTF8(&textDecoder{}, c.in, c.stream)
				require.NoError(t, err)
				base = min(base, time.Since(start))
			}
			points := 24
			if raceEnabled {
				points = 6 // each call runs for long under the detector
			}
			for i := 1; i < points; i++ {
				at := base * time.Duration(i) / time.Duration(points)
				var late, bound time.Duration
				for tries, slow := 0, 0; tries < 3; tries++ {
					bound = alloc()*5/4 + base/16
					var fired time.Time
					done := make(chan struct{})
					start := time.Now()
					timer := time.AfterFunc(at, func() {
						fired = time.Now()
						r.Interrupt("x")
						close(done)
					})
					_, err := r.decodeUTF8(&textDecoder{}, c.in, c.stream)
					end := time.Now()
					if timer.Stop() {
						late = 0 // done before the interrupt
						break
					}
					<-done
					r.ClearInterrupt()
					if err != nil {
						var ie *InterruptedError
						require.ErrorAs(t, err, &ie)
					}
					if late = end.Sub(fired); late < bound {
						break
					}
					if end.Sub(start) > 2*base && slow < 20 {
						slow++
						tries--
					}
				}
				assert.Less(t, late, bound, "an interrupt %v into a call of %v", at, base)
			}
		})
	}
}

// decoderBallast is TestTextDecoderInterruptLatency's ballast. A package
// variable links nothing into the test binary, where runtime/debug or a
// deferred runtime.KeepAlive would add runtime code linked before the
// engine and move the interpreter's code.
var decoderBallast []byte

// TestTextStringLimit checks that decode raises a RangeError for a string
// over the length limit, past a BOM it drops.
func TestTextStringLimit(t *testing.T) {
	lowerMaxStringLength(t, 1000)
	f := evalModule(t, `
const td = new TextDecoder();
// e(n, bom) is the UTF-8 of n "é", after a BOM if bom.
const e = (n, bom) => { const u = new Uint8Array(2 * n + (bom ? 3 : 0)); let i = 0; if (bom) u.set([0xEF, 0xBB, 0xBF]), i = 3; for (; i < u.length; i += 2) u.set([0xC3, 0xA9], i); return u };
export function f(which) {
  switch (which) {
  case "ascii": return td.decode(new Uint8Array(1001).fill(65)).length;
  case "ascii max": return td.decode(new Uint8Array(1000).fill(65)).length;
  case "utf-8": return td.decode(e(1001)).length;
  case "utf-8 max": return td.decode(e(1000)).length;
  case "bom": return td.decode(e(1000, true)).length;
  case "bom ascii": return td.decode(new Uint8Array([0xEF, 0xBB, 0xBF, ...new Uint8Array(1000).fill(65)])).length;
  case "bom ascii over": return td.decode(new Uint8Array([0xEF, 0xBB, 0xBF, ...new Uint8Array(1001).fill(65)])).length;
  case "bom kept": return new TextDecoder("utf-8", {ignoreBOM: true}).decode(e(1000, true)).length;
  case "replacement": return td.decode(new Uint8Array(1001).fill(0xFF)).length;
  }
}`)
	for which, want := range map[string]string{
		"ascii":          "!RangeError",
		"ascii max":      "1000",
		"utf-8":          "!RangeError",
		"utf-8 max":      "1000",
		"bom":            "1000",
		"bom ascii":      "1000",
		"bom ascii over": "!RangeError",
		"bom kept":       "!RangeError",
		"replacement":    "!RangeError",
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

// textEvalIn runs body as a function in r and returns its result as a
// string.
func textEvalIn(t *testing.T, r *Realm, body string) string {
	t.Helper()
	m, err := syntax.ParseModule("t.js", "export function f() {\n"+body+"\n}", syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	fn, _ := env.GetBindingValue("f")
	res, err := r.Call(fn, Undefined(), nil)
	require.NoError(t, err)
	return fmt.Sprint(r.ToGo(res))
}

// TestTextLateGroup checks that TextEncoder and TextDecoder are a late
// group of their own: one installer binds both and builds the binary group
// they need along, and in the shared realms the group is frozen, built
// once, and keeps the binary intrinsics a realm already holds.
func TestTextLateGroup(t *testing.T) {
	m := NewRealm()
	m.lateAt(lateArrayBuffer)
	assert.Nil(t, m.binary.text, "the binary group leaves the text group pending")
	assert.NotZero(t, m.coldGlobals&(1<<lateTextEncoder))
	u8 := m.binary.typedArrayPrototypes[elemUint8]
	m.ensureLate(StringKey(AtomTextDecoder))
	assert.Zero(t, m.coldGlobals&(3<<lateTextEncoder), "one installer binds both")
	txt := m.textIntr()
	require.NotNil(t, txt)
	assert.Same(t, u8, m.binary.typedArrayPrototypes[elemUint8])
	for k, c := range map[*String]*Object{AtomTextEncoder: txt.TextEncoderCtor, AtomTextDecoder: txt.TextDecoderCtor} {
		v, ok := m.Global.GetOwnDataValue(StringKey(k))
		require.True(t, ok, k.GoString())
		assert.Same(t, c, v.AsObject())
	}
	assert.False(t, txt.TextEncoderPrototype.IsFrozen())

	shared := RealmOptions{SharedIntrinsics: true}
	for _, first := range []*String{AtomArrayBuffer, AtomTextEncoder} {
		a := NewRealmWith(shared)
		a.ensureLate(StringKey(first))
		if first == AtomArrayBuffer {
			u8 = a.binary.typedArrayPrototypes[elemUint8]
			assert.NotZero(t, a.coldGlobals&(1<<lateTextEncoder), "the text group stays pending")
			assert.Equal(t, "true,function", textEvalIn(t, a, `return [new TextEncoder().encode("a") instanceof Uint8Array, typeof TextDecoder].join()`))
			assert.Same(t, u8, a.binary.typedArrayPrototypes[elemUint8], "the text group joins the binary intrinsics the realm holds")
		} else {
			assert.NotNil(t, a.binary, "the text group builds the binary group along")
			assert.NotZero(t, a.coldGlobals&(1<<lateArrayBuffer), "but a shared realm defines only the bindings it uses")
		}
		txt := a.textIntr()
		require.NotNil(t, txt)
		assert.Same(t, txt, NewRealmWith(shared).textIntr(), "built once per process")
		for _, o := range []*Object{txt.TextEncoderPrototype, txt.TextEncoderCtor, txt.TextDecoderPrototype, txt.TextDecoderCtor} {
			assert.True(t, o.IsFrozen())
			assert.NotZero(t, o.flags&flagShared)
		}
		assert.Equal(t, "TypeError,true", textEvalIn(t, a, `try { TextEncoder.prototype.x = 1 } catch (e) { return [e.name, Object.isFrozen(TextDecoder.prototype)].join() }`))
		assert.Equal(t, "é,1", textEvalIn(t, a, `const e = new TextEncoder(); e.x = 1; return [new TextDecoder().decode(e.encode("é")), e.x].join()`))
	}
	assert.Nil(t, sharedTpl.Intrinsics.binary)
}

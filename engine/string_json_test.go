package engine

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// refQuoteJSON is QuoteJSONString over code units, written out from the
// specification: the reference the fast paths are checked against.
func refQuoteJSON(u []uint16) []uint16 {
	out := []uint16{'"'}
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c == '"' || c == '\\':
			out = append(out, '\\', c)
		case c == '\b':
			out = append(out, '\\', 'b')
		case c == '\f':
			out = append(out, '\\', 'f')
		case c == '\n':
			out = append(out, '\\', 'n')
		case c == '\r':
			out = append(out, '\\', 'r')
		case c == '\t':
			out = append(out, '\\', 't')
		case c < 0x20:
			out = append(out, utf16.Encode([]rune(fmt.Sprintf(`\u%04x`, c)))...)
		case c >= 0xD800 && c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000:
			out = append(out, c, u[i+1])
			i++
		case c >= 0xD800 && c < 0xE000:
			out = append(out, utf16.Encode([]rune(fmt.Sprintf(`\u%04x`, c)))...)
		default:
			out = append(out, c)
		}
	}
	return append(out, '"')
}

func refJSONScan(s string, i int, high uint64) int {
	for ; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || (high != 0 && c >= 0x80) {
			return i
		}
	}
	return i
}

// TestJSONScan puts each special byte at every offset of strings around
// the eight- and sixteen-byte words, among fillers next to the special
// values, and a second special after the first (a borrow can flag the bytes
// above the first).
func TestJSONScan(t *testing.T) {
	specials := []byte{0x00, 0x08, 0x1f, '"', '\\', 0x80, 0xc3, 0xff}
	fillers := []byte{'A', 0x20, 0x21, 0x23, 0x5b, 0x5d, 0x7f}
	for n := 0; n <= 40; n++ {
		for _, f := range fillers {
			base := strings.Repeat(string(f), n)
			check := func(s string) {
				for _, high := range []uint64{0, swarMSB} {
					for i := 0; i <= n; i++ {
						require.Equal(t, refJSONScan(s, i, high), jsonScan(s, i, high), "%q from %d high %x", s, i, high)
					}
				}
			}
			check(base)
			for p := range n {
				for _, c := range specials {
					b := []byte(base)
					b[p] = c
					check(string(b))
					for _, q := range []int{p + 1, p + 7, p + 8} {
						if q < n {
							b2 := append([]byte(nil), b...)
							b2[q] = '"'
							check(string(b2))
						}
					}
				}
			}
		}
	}
}

// TestJSONASCII checks jsonASCII and isASCIILong against byte loops: each
// special byte, and the fillers jsonMaybeSpecial also flags (' ', '!') or
// sits next to, at every offset of strings around the 64-byte blocks, then
// past a filler ending the plain prefix, and a second special after the
// first (a borrow can flag the bytes above the first).
func TestJSONASCII(t *testing.T) {
	ref := func(s string) (ascii, plain bool) {
		return isASCII(s), refJSONScan(s, 0, swarMSB) == len(s)
	}
	check := func(s string) {
		t.Helper()
		ascii, plain := jsonASCII(s)
		wa, wp := ref(s)
		require.Equal(t, [2]bool{wa, wp}, [2]bool{ascii, plain}, "%q", s)
		require.Equal(t, wa, isASCIILong(s), "%q", s)
	}
	specials := []byte{0x00, 0x1f, '"', '\\', 0x80, 0xc3, 0xff}
	fillers := []byte{0x20, 0x21, 0x23, 0x5b, 0x5d, 0x7f}
	for _, n := range []int{0, 1, 31, 32, 33, 63, 64, 65, 127, 128, 129, 200} {
		base := strings.Repeat("QUJD+/==", n/8+1)[:n]
		check(base)
		for p := range n {
			for _, c := range append(specials, fillers...) {
				b := []byte(base)
				b[p] = c
				check(string(b))
				for _, q := range []int{p + 1, p + 8, p + 64} {
					if q < n {
						b2 := append([]byte(nil), b...)
						b2[q] = '"'
						check(string(b2))
					}
				}
			}
		}
	}
	for _, c := range append(specials, fillers...) {
		for _, p := range []int{0, jsonPlainMin / 2, jsonPlainMin - 1} {
			b := []byte(strings.Repeat("QUJD+/==", jsonPlainMin/8))
			b[p] = c
			check(string(b))
		}
	}
}

func stringifyUnits(t *testing.T, r *Realm, s *String) []uint16 {
	t.Helper()
	out, err := r.JSONStringify(StringValue(s))
	require.NoError(t, err)
	return out.UTF16()
}

// jsonOffsets is the escape offsets tried in a string of length n: every one
// in a short string, those around the words and the interrupt strides in a
// long one.
func jsonOffsets(n int) []int {
	if n <= 40 {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	var out []int
	for _, p := range []int{0, 1, 7, 8, 9, 15, 16, 17, interruptStride - 1, interruptStride, interruptStride + 1, 2*interruptStride - 1, 2 * interruptStride, n - 9, n - 8, n - 2, n - 1} {
		if p >= 0 && p < n {
			out = append(out, p)
		}
	}
	return out
}

var jsonTestLengths = []int{0, 1, 2, 3, 7, 8, 9, 15, 16, 17, 23, 24, 25, 31, 32, 33, 40, 63, 64, 65, interruptStride - 1, interruptStride, interruptStride + 1, 3*interruptStride + 5}

// TestJSONStringifyStringEscapes checks JSON.stringify of strings with an
// escape at each offset: ASCII strings (the scanning path), the same with a
// non-ASCII unit or a lone surrogate (UTF-16 strings), and ropes.
func TestJSONStringifyStringEscapes(t *testing.T) {
	r := NewRealm()
	escapes := []uint16{'"', '\\', '\n', '\b', 0x00, 0x01, 0x1f, 0x7f, 0x20, 0xe9, 0x4e2d, 0xd800, 0xdfff}
	filler := "abcdefghijklmnopqrstuvwxyz0123456789+/ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for _, n := range jsonTestLengths {
		base := make([]uint16, n)
		for i := range base {
			base[i] = uint16(filler[i%len(filler)])
		}
		require.Equal(t, refQuoteJSON(base), stringifyUnits(t, r, FromUTF16(base)), "n=%d", n)
		for _, p := range jsonOffsets(n) {
			for _, e := range escapes {
				u := append([]uint16(nil), base...)
				u[p] = e
				want := refQuoteJSON(u)
				require.Equal(t, want, stringifyUnits(t, r, FromUTF16(u)), "n=%d p=%d e=%#x", n, p, e)
				// A surrogate pair before p keeps the string UTF-16.
				if p >= 2 {
					v := append([]uint16(nil), u...)
					v[0], v[1] = 0xd83d, 0xde00
					require.Equal(t, refQuoteJSON(v), stringifyUnits(t, r, FromUTF16(v)), "n=%d p=%d e=%#x pair", n, p, e)
				}
				// The same string as a rope of two halves.
				if n >= ropeThreshold {
					s := FromUTF16(u)
					rope, err := r.Concat(s.Substring(0, n/2), s.Substring(n/2, n))
					require.NoError(t, err)
					require.Equal(t, want, stringifyUnits(t, r, rope), "n=%d p=%d e=%#x rope", n, p, e)
				}
			}
		}
	}
}

// TestJSONStringifyEscapeRuns quotes ASCII strings with escapes in a row
// and runs of every length up to two words between them, from every offset
// of a word: the runs quoteASCII stores whole and those it copies, with
// room in the output and without it (through the builder), for
// JSON.stringify (alone, and after a string that makes the builder UTF-16)
// and for AppendJSON, whose output fills its buffer at a different escape
// for each prefix.
func TestJSONStringifyEscapeRuns(t *testing.T) {
	r := NewRealm()
	filler := "abcdefghijklmnopqrstuvwxyz0123456789"
	for _, e := range []string{"\n", `"`, `\`, "\x01", "\t\t", "\n\x1f\"", "\x7f"} {
		for start := range 9 {
			for gap := range 18 {
				s := filler[:start] + e + filler[:gap] + e + filler[:gap%5] + e + filler[:20]
				u := utf16.Encode([]rune(s))
				want := refQuoteJSON(u)
				require.Equal(t, want, stringifyUnits(t, r, FromGoString(s)), "%q", s)
				arr := r.NewArrayFromSlice([]Value{StringValue(FromGoString("é")), StringValue(FromGoString(s))})
				got, err := r.JSONStringify(ObjectValue(arr))
				require.NoError(t, err)
				require.Equal(t, `["é",`+string(utf16.Decode(want))+`]`, got.GoString(), "%q", s)
			}
		}
	}
	long := strings.Repeat("\tab\n\"c\\defghijklmn\x02o", 40)
	for prefix := range 40 {
		s := filler[:prefix%len(filler)] + strings.Repeat("x", prefix) + long
		want := string(utf16.Decode(refQuoteJSON(utf16.Encode([]rune(s)))))
		_, _, err := r.AppendJSON(nil, NumberValue(1)) // a small size hint
		require.NoError(t, err)
		for _, dst := range [][]byte{nil, make([]byte, 0, len(want)-3), []byte("abc")} {
			out, ok, err := r.AppendJSON(dst, StringValue(FromGoString(s)))
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, string(dst)+want, string(out), "prefix=%d", prefix)
		}
	}
}

// TestJSONStringifyDenseEscapes quotes ASCII strings with an escape every
// few bytes across several interrupt strides, so that runs and escapes
// written in place meet the strides' ends, for JSON.stringify and for
// AppendJSON.
func TestJSONStringifyDenseEscapes(t *testing.T) {
	r := NewRealm()
	for _, seg := range []string{"\tif (x) {\n\t\treturn \"y\";\n", "\n\"", "ab\x01", "abcdefg\\"} {
		for _, extra := range []int{0, 1, 5, 7} {
			s := strings.Repeat("x", extra) + strings.Repeat(seg, (3*interruptStride+5)/len(seg))
			want := string(utf16.Decode(refQuoteJSON(utf16.Encode([]rune(s)))))
			got, err := r.JSONStringify(StringValue(FromGoString(s)))
			require.NoError(t, err)
			require.Equal(t, want, got.GoString(), "%q %d", seg, extra)
			out, ok, err := r.AppendJSON(nil, StringValue(FromGoString(s)))
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, want, string(out), "%q %d", seg, extra)
		}
	}
}

// TestAppendJSONSpareUntouched checks that an output that fits in dst's
// spare capacity leaves the bytes of dst past it as they were: the words
// quoteASCII stores whole end within the output.
func TestAppendJSONSpareUntouched(t *testing.T) {
	r := NewRealm()
	for _, seg := range []string{"ab\ncd\"ef", "\n\t", "abcdefg\n", "a\x01bc", "abcdefghijklmn\\"} {
		for reps := 1; reps < 40; reps++ {
			s := StringValue(FromGoString(strings.Repeat(seg, reps)))
			want, _, err := r.AppendJSON(nil, s)
			require.NoError(t, err)
			for _, plen := range []int{0, 3} {
				for extra := range 12 {
					// Room for the last output and an eighth (AppendJSON's
					// jsonSizeHint), so that the output stays in dst.
					arr := make([]byte, plen+max(len(want)+len(want)/8+1, 256)+extra)
					for i := range arr {
						arr[i] = 0xAA
					}
					out, ok, err := r.AppendJSON(arr[:plen], s)
					require.NoError(t, err)
					require.True(t, ok)
					require.Equal(t, string(want), string(out[plen:]))
					require.Same(t, &arr[0], &out[0], "%q x%d: the output left dst", seg, reps)
					for i := len(out); i < len(arr); i++ {
						require.Equal(t, byte(0xAA), arr[i], "%q x%d plen=%d: byte %d past the output of %d", seg, reps, plen, i, len(out))
					}
				}
			}
		}
	}
}

// TestJSONStringifyLongStrings checks very long strings: an escape-free
// one (one copy), one escape in each stride, and one escape per byte.
func TestJSONStringifyLongStrings(t *testing.T) {
	r := NewRealm()
	const n = 1<<20 + 3
	plain := strings.Repeat("QUJDRA+/", n/8) + "xyz"
	got, err := r.JSONStringify(StringValue(FromGoString(plain)))
	require.NoError(t, err)
	require.Equal(t, `"`+plain+`"`, got.GoString())

	b := []byte(plain)
	for i := 100; i < len(b); i += interruptStride {
		b[i] = '"'
	}
	got, err = r.JSONStringify(StringValue(FromGoString(string(b))))
	require.NoError(t, err)
	require.Equal(t, `"`+strings.ReplaceAll(string(b), `"`, `\"`)+`"`, got.GoString())

	dense := strings.Repeat("\n\"", 1<<15)
	got, err = r.JSONStringify(StringValue(FromGoString(dense)))
	require.NoError(t, err)
	require.Equal(t, `"`+strings.Repeat(`\n\"`, 1<<15)+`"`, got.GoString())
}

// TestAppendJSON checks AppendJSON against JSONStringify's UTF-8 text for
// strings with escapes, non-ASCII units and lone surrogates at each offset,
// containers, and every kind of dst: nil, empty with room, a prefix with
// room, a prefix without room (the result reallocates), a non-ASCII prefix.
func TestAppendJSON(t *testing.T) {
	r := NewRealm()
	escapes := []uint16{'"', '\\', '\n', 0x00, 0x1f, 0x7f, 0xe9, 0x7ff, 0x800, 0x4e2d, 0xffff, 0xd800, 0xdbff, 0xdc00, 0xdfff}
	var values []Value
	for _, n := range []int{0, 1, 7, 8, 9, 16, 17, 33, interruptStride + 1} {
		base := make([]uint16, n)
		for i := range base {
			base[i] = uint16('a' + i%26)
		}
		values = append(values, StringValue(FromUTF16(base)))
		for _, p := range jsonOffsets(n) {
			for _, e := range escapes {
				u := append([]uint16(nil), base...)
				u[p] = e
				values = append(values, StringValue(FromUTF16(u)))
				if p+1 < n && e >= 0xd800 && e < 0xdc00 {
					u[p+1] = 0xde00 // a pair
					values = append(values, StringValue(FromUTF16(u)))
				}
			}
		}
	}
	values = append(values,
		jsonParseGo(t, r, `{"a":[1,"xé",{"b":null,"中":true}],"c":"😀","d":-0.5}`),
		jsonParseGo(t, r, `["\ud800", "\udfff\ud800", "tail"]`),
		Undefined(), Null(), True(), NumberValue(1e21), StringValue(FromGoString("")),
	)
	prefixes := []func() []byte{
		func() []byte { return nil },
		func() []byte { return make([]byte, 0, 1<<16) },
		func() []byte { return append(make([]byte, 0, 1<<16), "pre:"...) },
		func() []byte { return []byte("pre:") },
		func() []byte { return []byte("中文:") },
	}
	for _, v := range values {
		s, err := r.JSONStringify(v)
		require.NoError(t, err)
		for k, mk := range prefixes {
			dst := mk()
			pre := string(dst)
			out, ok, err := r.AppendJSON(dst, v)
			require.NoError(t, err)
			if s == nil {
				require.False(t, ok)
				require.Equal(t, pre, string(out))
				continue
			}
			require.True(t, ok)
			require.Equal(t, pre+s.GoString(), string(out), "prefix %d", k)
		}
	}
}

// TestAppendJSONLongOutput writes outputs past the size hint, which
// reallocate, after a short one has made the hint small.
func TestAppendJSONLongOutput(t *testing.T) {
	r := NewRealm()
	big := strings.Repeat("QUJD", 1<<18)
	o := jsonParseGo(t, r, `{"a":1}`)
	require.NoError(t, o.AsObject().SetProp(r, r.KeyFromGoString("img"), StringValue(FromGoString(big))))
	require.NoError(t, o.AsObject().SetProp(r, r.KeyFromGoString("zh"), StringValue(FromGoString(strings.Repeat("中文", 1<<16)))))
	want := `{"a":1,"img":"` + big + `","zh":"` + strings.Repeat("中文", 1<<16) + `"}`
	for _, dst := range [][]byte{nil, []byte("x"), make([]byte, 0, 10)} {
		_, _, err := r.AppendJSON(nil, NumberValue(1))
		require.NoError(t, err)
		out, ok, err := r.AppendJSON(dst, o)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, string(dst)+want, string(out))
	}
}

// refParseJSONString decodes the body of a JSON string literal (WTF-8, up
// to the closing quote) into code units, from the specification; ok is
// false where JSON.parse throws.
func refParseJSONString(body string) (u []uint16, ok bool) {
	for i := 0; i < len(body); {
		c := body[i]
		switch {
		case c < 0x20 || c == '"':
			return nil, false
		case c == '\\':
			if i+1 >= len(body) {
				return nil, false
			}
			e := body[i+1]
			i += 2
			switch e {
			case '"', '\\', '/':
				u = append(u, uint16(e))
			case 'b':
				u = append(u, '\b')
			case 'f':
				u = append(u, '\f')
			case 'n':
				u = append(u, '\n')
			case 'r':
				u = append(u, '\r')
			case 't':
				u = append(u, '\t')
			case 'u':
				if i+4 > len(body) {
					return nil, false
				}
				var v uint16
				for _, h := range []byte(body[i : i+4]) {
					d := digitValue(h)
					if d < 0 || d >= 16 {
						return nil, false
					}
					v = v<<4 | uint16(d)
				}
				u = append(u, v)
				i += 4
			default:
				return nil, false
			}
		default:
			n := i + 1
			for n < len(body) && body[n] >= 0x80 && body[n] < 0xC0 {
				n++
			}
			u = appendWTF8Units(u, body[i:n])
			i = n
		}
	}
	return u, true
}

// TestJSONParseStrings parses string literals with each special at every
// offset (escapes, control characters, non-ASCII bytes, a quote that ends
// the literal early), at the word and stride boundaries of long ones, in
// ASCII text and in WTF-8 text.
func TestJSONParseStrings(t *testing.T) {
	r := NewRealm()
	specials := []string{`\"`, `\\`, `\/`, `\n`, `A`, `\ud800`, `\udc00x`, `😀`, "\x00", "\x1f", "é", "中", "😀", "\x7f", `\x`, `\u12`, `"`}
	filler := "abcdefghijklmnopqrstuvwxyz0123456789+/ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for _, n := range jsonTestLengths {
		base := make([]byte, n)
		for i := range base {
			base[i] = filler[i%len(filler)]
		}
		for _, p := range append(jsonOffsets(n), n) {
			for _, sp := range specials {
				body := string(base[:p]) + sp + string(base[p:])
				for _, text := range []string{`"` + body + `"`, `["中", "` + body + `"]`} {
					want, ok := refParseJSONString(body)
					wrapped := text[0] == '['
					v, err := r.JSONParse(FromGoString(text))
					if !ok { // a quote in body ends the literal early
						require.Error(t, err, "%q", text)
						continue
					}
					require.NoError(t, err, "%q", text)
					if wrapped {
						v = v.AsObject().Elements()[1]
					}
					require.Equal(t, want, v.AsString().UTF16(), "%q", text)
					// The parsed string quotes back to its canonical form
					// (a plain literal is copied without a scan).
					require.Equal(t, refQuoteJSON(want), stringifyUnits(t, r, v.AsString()), "%q", text)
				}
			}
		}
	}
}

// TestJSONParseStringErrors checks the positions JSON.parse reports.
func TestJSONParseStringErrors(t *testing.T) {
	r := NewRealm()
	for _, c := range []struct{ text, msg string }{
		{"\"abcdefghijklmnopq\x01\"", "Bad control character in string literal in JSON at position 18"},
		{"\"abc\\ndefghijklmnopq\x01\"", "Bad control character in string literal in JSON at position 20"},
		{`"abcdefghijklmnopqrstuvwxyz`, "Unterminated string in JSON at position 27"},
		{`"abc\ndefghijklmnopqrstuvwxyz`, "Unterminated string in JSON at position 29"},
		{`"abcdefghijklmnopqrstuvwxyz\q"`, "Bad escaped character in JSON at position 28"},
		{`"abcdefghijklmnopqrstuvwxyz\u12g4"`, "Bad Unicode escape in JSON at position 31"},
		{`"abcdefghijklmnopqrstuvwxyz\`, "Unexpected end of JSON input"},
		{`"中文中文中文中文中文\u004"`, "Bad Unicode escape in JSON at position 36"},
	} {
		_, err := r.JSONParse(FromGoString(c.text))
		require.Error(t, err, c.text)
		require.Contains(t, err.Error(), c.msg, c.text)
	}
}

// TestJSONParseEscapedStrings parses literals that start with an escape, so
// that the rest goes through strSlow, with a special at every offset after
// it: runs of backslashes before a quote (escaped or closing by their
// parity), a \u escape of a unit >= 0x80 that widens an ASCII value, bytes
// >= 0x80, control characters and bad escapes; at the word and stride
// boundaries of long literals, the short ones on both sides of the 64
// bytes decoded on the stack, in ASCII and in WTF-8 text.
func TestJSONParseEscapedStrings(t *testing.T) {
	r := NewRealm()
	prefixes := []string{`\n`, `\t\t\t`, `\\`, `\"`, `\u0041`, `\u00e9`, `\ud83d\ude00`, `é\n`, `\\u4f60`, `\u4f60\n`}
	specials := []string{``, `\"`, `\\`, `\\\"`, `\\\\`, `\\\\\"`, `\"\"`, `\u00e9`, `\u0080`, `\u07ff`, `\u0800`, `\ud800`, `\udc00`, `\ud83d\ude00`,
		`\u005c`, `\u0022`, `\/`, `\b\f\r`, `é`, `中`, `😀`, "\x00", "\x1f", `\x`, `\u12`, `\`, `"`, `\\"`,
		`\\u00e9`, `\\\u00e9`, `\u4f60\u597d\u4f60`, `\u中0`, `\u0中`}
	filler := "abcdefghijklmnopqrstuvwxyz0123456789+/ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for _, n := range jsonTestLengths {
		base := make([]byte, n)
		for i := range base {
			base[i] = filler[i%len(filler)]
		}
		for _, pre := range prefixes {
			for _, p := range append(jsonOffsets(n), n) {
				for _, sp := range specials {
					body := pre + string(base[:p]) + sp + string(base[p:])
					for _, text := range []string{`"` + body + `"`, `["中", "` + body + `"]`} {
						want, ok := refParseJSONString(body)
						v, err := r.JSONParseGoString(text)
						if !ok {
							require.Error(t, err, "%q", text)
							verr := err
							_, err = r.JSONParse(FromGoString(text))
							require.Error(t, err, "%q", text)
							require.Equal(t, verr.Error(), err.Error(), "%q", text)
							continue
						}
						require.NoError(t, err, "%q", text)
						if text[0] == '[' {
							v = v.AsObject().Elements()[1]
						}
						s := v.AsString()
						require.Equal(t, want, s.UTF16(), "%q", text)
						wide := false
						for _, c := range want {
							wide = wide || c >= 0x80
						}
						decoded := s
						if s.isLazy() {
							decoded = cacheOf(s)
						}
						require.Equal(t, wide, decoded.kind == strUTF16, "%q", text)
					}
				}
			}
		}
	}
}

// TestJSONParseEscapedStringErrors checks the errors of literals with an
// escape before the error, which strSlow reports while checking their
// syntax.
func TestJSONParseEscapedStringErrors(t *testing.T) {
	r := NewRealm()
	for _, c := range []struct{ text, msg string }{
		{"\"a\\nb\x01\"", "Bad control character in string literal in JSON at position 5"},
		{"\"a\\nb\x01", "Bad control character in string literal in JSON at position 5"},
		{"[\"a\\nb\x01\"]", "Bad control character in string literal in JSON at position 6"},
		{`"a\nb\qc"`, "Bad escaped character in JSON at position 6"},
		{`"a\nb\u12g4"`, "Bad Unicode escape in JSON at position 9"},
		{`"a\nb\u12"`, "Unexpected end of JSON input"},
		{`"a\nb\u1`, "Unexpected end of JSON input"},
		{`"a\nb\`, "Unexpected end of JSON input"},
		{`"a\nb\"`, "Unterminated string in JSON at position 7"},
		{`"a\nb\\\"cd`, "Unterminated string in JSON at position 11"},
		{`"a\n中\"`, "Unterminated string in JSON at position 9"},
		{`"` + strings.Repeat(`a\n`, 100) + `\q"`, "Bad escaped character in JSON at position 302"},
		{`"` + strings.Repeat(`é\n`, 100) + `\q"`, "Bad escaped character in JSON at position 402"},
		// A control character after a run of backslashes, escaped or not.
		{"\"ab\\\\\\\\\x01\"", "Bad control character in string literal in JSON at position 7"},
		{"\"ab\\\\\\\x01\"", "Bad escaped character in JSON at position 6"},
		{"\"ab\\\\\\\\\\\\\x1f", "Bad control character in string literal in JSON at position 9"},
		{"[\"ab\\\\\\\\\x01\"]", "Bad control character in string literal in JSON at position 8"},
		{`"` + strings.Repeat("a", 70) + `\\\\` + "\x01" + `"`, "Bad control character in string literal in JSON at position 75"},
		{`"` + strings.Repeat("a", 70) + `é` + "\x01" + `"`, "Bad control character in string literal in JSON at position 73"},
		{`"é` + strings.Repeat("a", 70) + `你` + "\x02", "Bad control character in string literal in JSON at position 76"},
		// \u digits that run into the closing quote, at the end of the text.
		{`"ab\u12"`, "Unexpected end of JSON input"},
		{`"ab\u1"`, "Unexpected end of JSON input"},
		{`"ab\u"`, "Unexpected end of JSON input"},
		{`"ab\u123"`, "Bad Unicode escape in JSON at position 8"},
		{`"ab\u00e"`, "Bad Unicode escape in JSON at position 8"},
		{`["ab\u12"]`, "Bad Unicode escape in JSON at position 8"},
		{`"abé\u12"`, "Unexpected end of JSON input"},
		{`"` + strings.Repeat("a", 70) + `\n\u12"`, "Unexpected end of JSON input"},
		{`"` + strings.Repeat("a", 70) + `\n\u123"`, "Bad Unicode escape in JSON at position 78"},
		{`"` + strings.Repeat("a", 70) + `é\u12g4"`, "Bad Unicode escape in JSON at position 77"},
		{`"` + strings.Repeat("a", 70) + `é\q"`, "Bad escaped character in JSON at position 74"},
		{`"é` + strings.Repeat("a", 70) + `你\u12"`, "Unexpected end of JSON input"},
		{`"你\\\"`, "Unterminated string in JSON at position 8"},
		// \u and bytes >= 0x80 for digits (FuzzJSON, FuzzJSONCheck).
		{"\"\\u\xb20\"", "Bad Unicode escape in JSON at position 3"},
		{"[\"\\u\xff0\"", "Bad Unicode escape in JSON at position 4"},
		{"\"é\\uéé0\"", "Bad Unicode escape in JSON at position 5"},
		{"\"\\u4f6é\"", "Bad Unicode escape in JSON at position 6"},
		{"\"" + strings.Repeat("a", 70) + "\\u\xb20\"", "Bad Unicode escape in JSON at position 73"},
		{"\"é\\n\\u00中\"", "Bad Unicode escape in JSON at position 9"},
	} {
		_, err := r.JSONParseGoString(c.text)
		require.Error(t, err, c.text)
		require.Contains(t, err.Error(), c.msg, c.text)
	}
}

// TestJSONParseBadLaterKey checks that a key after the first that does not
// parse is reported, also when a colon follows where it should be.
func TestJSONParseBadLaterKey(t *testing.T) {
	r := NewRealm()
	for _, c := range []struct{ text, msg string }{
		{`{"a":1,:{}}`, "Unexpected token : in JSON at position 7"},
		{`{"a":1, :2}`, "Unexpected token : in JSON at position 8"},
		{`{"0":[0,100000,1000E00,true,false,null],:{}}`, "Unexpected token : in JSON at position 40"},
		{`{"a":1,"\q":2}`, "Bad escaped character in JSON at position 9"},
		{"{\"a\":1,\"b\x01\":2}", "Bad control character in string literal in JSON at position 9"},
		{`{"a":1,"b`, "Unterminated string in JSON at position 9"},
		{`{"a":{"b":1,:2}}`, "Unexpected token : in JSON at position 12"},
	} {
		_, err := r.JSONParse(FromGoString(c.text))
		require.Error(t, err, c.text)
		require.Contains(t, err.Error(), c.msg, c.text)
	}
}

// TestJSONParseEscapedStringDecodesOnce checks that parsing an escaped
// literal keeps its contents and decodes them once, on the first read of
// the content.
func TestJSONParseEscapedStringDecodesOnce(t *testing.T) {
	r := NewRealm()
	for _, text := range []string{
		`"ab\ncd"`,
		`"` + strings.Repeat(`abc\n\t\"def`, 1000) + `"`,
		`"` + strings.Repeat(`中文\n`, 1000) + `"`,
		`"` + strings.Repeat(`中文`, 1000) + `"`,
		`"` + strings.Repeat(`abc\n`, 1000) + `\u00e9"`,
		`"Please summarize:\n` + strings.Repeat(`\u4f60`, 500) + `"`,
		`"` + strings.Repeat(`\u0041`, 1000) + `"`,
		`"ab\u00e9"`,
	} {
		v, err := r.JSONParseGoString(text)
		require.NoError(t, err)
		s := v.AsString()
		require.True(t, s.isLazy(), text[:min(20, len(text))])
		require.Nil(t, cacheOf(s))
		_ = s.UTF16()
		decoded := cacheOf(s)
		require.NotNil(t, decoded)
		_ = s.UTF16()
		require.Same(t, decoded, cacheOf(s))
	}
}

// TestJSONParseEscapedStringCharge checks that parsing an escaped literal
// charges the lazy block and the units it decodes to, which reading then
// charges no more.
func TestJSONParseEscapedStringCharge(t *testing.T) {
	block := int64(unsafe.Sizeof(rootedLazyStringBlock{}))
	for _, text := range []string{
		`"ab\ncd"`,
		`"` + strings.Repeat(`abc\n\t\"def`, 1000) + `"`,
		`"` + strings.Repeat(`中文\n`, 1000) + `"`,
		`"` + strings.Repeat(`\u4f60`, 500) + `"`,
		`"ab\u00e9"`,
	} {
		r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 30})
		before := r.Stats().RequestAllocatedBytes
		v, err := r.JSONParseGoString(text)
		require.NoError(t, err)
		s := v.AsString()
		require.Nil(t, cacheOf(s))
		require.Equal(t, block+int64(s.lazyDecodedSize()), r.Stats().RequestAllocatedBytes-before, text[:min(20, len(text))])
		parsed := r.Stats().RequestAllocatedBytes
		_ = s.UTF16()
		require.Equal(t, parsed, r.Stats().RequestAllocatedBytes, text[:min(20, len(text))])
	}
}

// TestWTF8Units checks wtf8Units against utf16 over text with sequences of
// each length at every offset of the eight-byte words, and over encoded
// surrogates.
func TestWTF8Units(t *testing.T) {
	for _, seq := range []string{"a", "é", "中", "😀", "\xed\xa0\x80", "\xed\xbf\xbf"} {
		for n := range 40 {
			for p := 0; p <= n; p++ {
				s := strings.Repeat("x", p) + seq + strings.Repeat("y", n-p) + seq + seq
				require.Equal(t, len(appendWTF8Units(nil, s)), wtf8Units(s), "%q", s)
			}
		}
	}
}

// TestJSONParseGoString checks that JSONParseGoString gives what JSONParse of
// FromGoString gives, values and error messages, for ASCII, valid UTF-8 and
// invalid UTF-8 text (bad bytes and an encoded surrogate read as U+FFFD).
func TestJSONParseGoString(t *testing.T) {
	r := NewRealm()
	texts := []string{
		`{"a":"b","n":[1,2.5,-0,true,null],"s":"x\"y\\u00e9"}`,
		`{"中":"文","emoji":"😀","esc":"😀\ud800"}`,
		"{\"bad\":\"a\xffb\",\"k\xfe\":1}",
		"[\"\xed\xa0\x80\",\"\xed\xa0\x80\xed\xb0\x80\"]",
		"[\"\xc3\"]",
		`"` + strings.Repeat("QUJD", 1<<12) + `中"`,
		"{\"a\":\"\xff\" \x01}",
		`{"a":1,}`,
		"[\"中\x01\"]",
		"中",
	}
	for _, text := range texts {
		want, werr := r.JSONParse(FromGoString(text))
		got, gerr := r.JSONParseGoString(text)
		if werr != nil {
			require.Error(t, gerr, "%q", text)
			require.Equal(t, werr.Error(), gerr.Error(), "%q", text)
			continue
		}
		require.NoError(t, gerr, "%q", text)
		ws, err := r.JSONStringify(want)
		require.NoError(t, err)
		gs, err := r.JSONStringify(got)
		require.NoError(t, err)
		require.Equal(t, ws.UTF16(), gs.UTF16(), "%q", text)
	}
}

// TestFromGoLongStringJSON quotes FromGo strings around jsonPlainMin, with
// an escape or a non-ASCII character at either end or in the middle, read by
// a script (materialized) and written untouched (the direct walk).
func TestFromGoLongStringJSON(t *testing.T) {
	r := NewRealm()
	for _, n := range []int{jsonPlainMin - 1, jsonPlainMin, jsonPlainMin + 1} {
		base := []byte(strings.Repeat("QUJD+/==", n/8+1)[:n])
		variants := []string{string(base)}
		for _, p := range []int{0, n / 2, n - 1} {
			for _, c := range []string{"\"", "\n", "\x00", "é", "\xff"} {
				b := append([]byte(nil), base[:p]...)
				b = append(b, c...)
				variants = append(variants, string(append(b, base[p+1:]...)))
			}
		}
		for i, g := range variants {
			want := refQuoteJSON(FromGoString(g).UTF16())
			o, err := r.FromGo(map[string]any{"s": g})
			require.NoError(t, err)
			v, err := o.AsObject().GetProp(r, r.KeyFromGoString("s"))
			require.NoError(t, err)
			s := v.AsString()
			lazy := len(g) >= jsonPlainMin || !isASCII(g)
			require.Equal(t, lazy, s.isLazy(), "n=%d", n)
			jsonFacts := func() uint32 {
				if !lazy {
					return 0
				}
				return s.lazyData().facts.Load() & (factJSONPlain | factJSONEscapes)
			}
			require.Zero(t, jsonFacts(), "n=%d", n)
			require.Equal(t, want, stringifyUnits(t, r, s), "n=%d", n)
			var facts uint32
			if len(g) >= jsonPlainMin {
				facts = factJSONEscapes
				if i == 0 { // variants[0] has no escape or non-ASCII byte
					facts = factJSONPlain
				}
			}
			require.Equal(t, facts, jsonFacts(), "n=%d", n)
			out, ok, err := r.AppendJSON(nil, v)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, string(utf16.Decode(want)), string(out), "n=%d", n)
			o, err = r.FromGo(map[string]any{"s": g})
			require.NoError(t, err)
			out, _, err = r.AppendJSON(nil, o)
			require.NoError(t, err)
			require.Equal(t, `{"s":`+string(utf16.Decode(want))+`}`, string(out), "n=%d", n)
		}
	}
}

// TestJSONParseSharesShortLiterals checks that repeated short plain
// literals are one String, and that literals of the same slot but other
// content and long ones are not merged.
func TestJSONParseSharesShortLiterals(t *testing.T) {
	r := NewRealm()
	long := strings.Repeat("x", jsonShortMax+1)
	v := jsonParseGo(t, r, `["user","assistant","user","ab","ba","aXYb","aZYb","`+long+`","`+long+`","user","user"]`)
	e := v.AsObject().Elements()
	s := func(i int) *String { return e[i].AsString() }
	require.Same(t, s(0), s(2))
	require.Same(t, s(0), s(10))
	require.Same(t, s(0), s(9))
	require.NotSame(t, s(7), s(8))
	for i, want := range []string{"user", "assistant", "user", "ab", "ba", "aXYb", "aZYb", long, long, "user", "user"} {
		require.Equal(t, want, s(i).GoString(), i)
	}
	require.Equal(t, strSlot("aXYb"), strSlot("aZYb")) // same length, first, middle and last bytes
	require.NotSame(t, s(5), s(6))
	out, err := r.JSONStringify(v)
	require.NoError(t, err)
	require.Equal(t, `["user","assistant","user","ab","ba","aXYb","aZYb","`+long+`","`+long+`","user","user"]`, out.GoString())
}

// TestAppendJSONOwnSized checks the buffer own gives the output when code
// runs: sized from what is written, not from the spare capacity of a dst
// the host reuses, which one large output made large. After a 4 MiB output and a
// small one, 50 calls with a Date into the reused buffer allocate kilobytes
// (they allocated the buffer's 4.5 MiB each), and the host keeps its buffer.
func TestAppendJSONOwnSized(t *testing.T) {
	r := NewRealm()
	big := evalValue(t, r, `({image: "A".repeat(4 << 20)})`)
	dated := evalValue(t, r, `({at: new Date(0), n: 2})`)
	buf, _, err := r.AppendJSON(nil, big)
	require.NoError(t, err)
	buf, _, err = r.AppendJSON(buf[:0], dated) // grows buf to the 4 MiB hint
	require.NoError(t, err)
	want := string(buf)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range 50 {
		buf, _, err = r.AppendJSON(buf[:0], dated)
		require.NoError(t, err)
	}
	runtime.ReadMemStats(&after)
	require.Equal(t, want, string(buf))
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20))
	require.GreaterOrEqual(t, cap(buf), 4<<20)
}

// TestAppendJSONOwnAlternating checks own's buffer for a host that
// alternates large and small results in one reused buffer: a small value
// with a Date, right after a 20 MiB output, allocates for what it writes,
// not for the last output's length, which each large result sets to 20
// MiB again. Ten such pairs allocate kilobytes (each small call allocated
// 23.6 MB), and the host keeps its buffer.
func TestAppendJSONOwnAlternating(t *testing.T) {
	r := NewRealm()
	big := evalValue(t, r, `({image: "A".repeat(20 << 20)})`)
	dated := evalValue(t, r, `({at: new Date(0), n: 2})`)
	// The buffer has the room AppendJSON reserves after a large output, so
	// that only own allocates.
	buf, _, err := r.AppendJSON(make([]byte, 0, 24<<20), big)
	require.NoError(t, err)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range 10 {
		buf, _, err = r.AppendJSON(buf[:0], big)
		require.NoError(t, err)
		buf, _, err = r.AppendJSON(buf[:0], dated)
		require.NoError(t, err)
		require.Equal(t, `{"at":"1970-01-01T00:00:00.000Z","n":2}`, string(buf))
	}
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(256<<10))
	require.GreaterOrEqual(t, cap(buf), 20<<20)
}

// TestAppendJSONDstDuringCode checks that AppendJSON's output leaves dst's
// spare capacity before JavaScript runs: a native function called from a
// toJSON, a getter, a proxy's trap, an element's getter, a Date's
// toISOString or a wrapper's valueOf overwrites that capacity and runs a
// nested AppendJSON into it, and the output is still JSONStringify's. A
// value that runs no code is written in place.
func TestAppendJSONDstDuringCode(t *testing.T) {
	r := NewRealm()
	var dst []byte
	scribble := r.NewNativeFunction(FromGoString("scribble"), 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		spare := dst[len(dst):cap(dst)]
		for i := range spare {
			spare[i] = '#'
		}
		_, _, err := r.AppendJSON(dst, StringValue(FromGoString("nested")))
		return Undefined(), err
	})
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("scribble"), ObjectValue(scribble)))
	evalValue(t, r, `Date.prototype.toISOString = function () { scribble(); return "D"; }; 0`)
	for _, src := range []string{
		`({a: 1, b: {toJSON() { scribble(); return "B"; }}, c: [1, 2]})`,
		`({a: 1, get b() { scribble(); return "B"; }, c: "x"})`,
		`(() => { const o = {a: 1, b: 2}; Object.defineProperty(o, "c", {get() { scribble(); return "C"; }, enumerable: true}); delete o.b; return o; })()`,
		`new Proxy({a: 1, b: [2]}, {get(t, k) { scribble(); return t[k]; }})`,
		`[new Proxy([1, 2], {get(t, k) { scribble(); return t[k]; }})]`,
		`(() => { const a = [1, 2, 3]; Object.defineProperty(a, 1, {get() { scribble(); return "B"; }, enumerable: true}); return a; })()`,
		`({at: new Date(0), x: "y"})`,
		`({n: Object.assign(new Number(1), {valueOf() { scribble(); return 2; }})})`,
		`[1, 2n]`, // a BigInt: no code, an error
	} {
		v := evalValue(t, r, src)
		want, werr := r.JSONStringify(v)
		for _, pre := range []string{"", "pre:"} {
			dst = append(make([]byte, 0, 1<<12), pre...)
			out, ok, err := r.AppendJSON(dst, v)
			if werr != nil {
				require.EqualError(t, err, werr.Error(), src)
				require.Equal(t, pre, string(out), src)
				continue
			}
			require.NoError(t, err, src)
			require.True(t, ok, src)
			require.Equal(t, pre+want.GoString(), string(out), src)
		}
	}
	dst = make([]byte, 0, 1<<12)
	out, _, err := r.AppendJSON(dst, evalValue(t, r, `({a: [1, "x", {b: null}]})`))
	require.NoError(t, err)
	require.Equal(t, `{"a":[1,"x",{"b":null}]}`, string(out))
	require.Same(t, unsafe.SliceData(dst), unsafe.SliceData(out), "in place")
	// A host array its Go value cannot be written from (a key not ASCII) is
	// materialized first, read without code.
	p, err := r.FromGo([]any{map[string]any{"中": 1}, "x"})
	require.NoError(t, err)
	out, _, err = r.AppendJSON(dst, p)
	require.NoError(t, err)
	require.Equal(t, `[{"中":1},"x"]`, string(out))
	require.Same(t, unsafe.SliceData(dst), unsafe.SliceData(out), "in place")
}

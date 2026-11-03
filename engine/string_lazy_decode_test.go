package engine

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// lazyUnitsOf counts the units text decodes to in mode, as the makers of
// lazy strings count them: hostUnits for Go text, wtf8Units for WTF-8 that
// is not ASCII, jsonDecodedUnits for JSON contents.
func lazyUnitsOf(text string, mode lazyStringMode) (int, bool) {
	switch mode {
	case lazyText:
		n, wide, _ := hostUnits(text)
		return n, wide
	case lazyWTF8:
		if isASCIILong(text) {
			return len(text), false
		}
		return wtf8Units(text), true
	}
	saved, wide := 0, false
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' {
			continue
		}
		if text[i+1] == 'u' {
			saved += 5
			wide = wide || wideEscape(text, i+2)
			i += 5
		} else {
			saved++
			i++
		}
	}
	return jsonDecodedUnits(text, saved, wide)
}

// newLazyString is an uncharged lazy string of text in mode.
func newLazyString(text string, mode lazyStringMode) *String {
	n, wide := lazyUnitsOf(text, mode)
	return newLazyStringSized(text, mode, false, n, wide)
}

// decodeLazyString decodes text in mode as a lazy string does.
func decodeLazyString(text string, mode lazyStringMode) *String {
	n, wide := lazyUnitsOf(text, mode)
	invalid := mode == lazyText && !utf8.ValidString(text)
	return decodeLazyStringSized(text, mode, n, wide, invalid)
}

// liveSize is what the live measurement counts of s alone.
func liveSize(s *String) int64 {
	var seen liveSet
	w := liveWalker{seen: &seen}
	w.str(s)
	seen.release()
	return w.size
}

// TestLazyDecodeStorage checks the content each mode decodes to and that
// the live measurement counts no more than its units and a header. It reads
// the engine's account, not the Go heap: TestLazySmallDecodeAllocations
// counts the allocations, and the decoders size their storage from the
// counted length (newUTF16Buf).
func TestLazyDecodeStorage(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		mode       lazyStringMode
		want       string
	}{
		{"text-cjk", strings.Repeat("中😀", 16384), lazyText, strings.Repeat("中😀", 16384)},
		{"wtf8-cjk", strings.Repeat("中😀", 16384), lazyWTF8, strings.Repeat("中😀", 16384)},
		{"json-cjk", strings.Repeat("中\\n😀", 16384), lazyJSONContents, strings.Repeat("中\n😀", 16384)},
		{"json-late-wide", strings.Repeat("a\\n", 32768) + "中", lazyJSONContents, strings.Repeat("a\n", 32768) + "中"},
		{"json-escaped-ascii", strings.Repeat("a\\n", 32768), lazyJSONVerbatim, strings.Repeat("a\n", 32768)},
		{"json-unicode", strings.Repeat("\\u4e2d\\ud83d\\ude00", 16384), lazyJSONContents, strings.Repeat("中😀", 16384)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := decodeLazyString(tc.text, tc.mode)
			retained := int(liveSize(s))
			if !s.Equals(FromGoString(tc.want)) {
				t.Fatal("decoded content differs")
			}
			bytes := s.Len()
			if s.kind == strUTF16 {
				bytes *= 2
			}
			if retained > bytes+stringHeaderSize {
				t.Fatalf("decoded storage = %d, content and header need %d", retained, bytes+stringHeaderSize)
			}
		})
	}
}

func BenchmarkLazyDecode(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		mode       lazyStringMode
	}{
		{"wtf8-cjk", strings.Repeat("中😀", 16384), lazyWTF8},
		{"json-agent", strings.Repeat("text\\n工具返回了内容😀。", 8192), lazyJSONContents},
		{"json-late-wide", strings.Repeat("a\\n", 32768) + "中", lazyJSONContents},
		{"json-ascii", strings.Repeat("a\\n", 32768), lazyJSONContents},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				decodeLazyString(tc.text, tc.mode)
			}
		})
	}
}

// randomText builds n sequences of random length: ASCII, two-, three- and
// four-byte UTF-8, a surrogate's WTF-8 encoding when wtf8, and an arbitrary
// byte when anyBytes.
func randomText(r *rand.Rand, n int, wtf8, anyBytes bool) string {
	var b []byte
	for range n {
		switch k := r.IntN(7); {
		case k < 2:
			b = append(b, byte(r.IntN(0x80)))
		case k == 2:
			b = utf8.AppendRune(b, rune(0x80+r.IntN(0x800-0x80)))
		case k == 3:
			c := rune(0x800 + r.IntN(0x10000-0x800))
			if c >= 0xD800 && c <= 0xDFFF {
				if wtf8 {
					b = append(b, 0xED, byte(0x80|(c>>6)&0x3F), byte(0x80|c&0x3F))
					continue
				}
				c = 0xFFFD
			}
			b = utf8.AppendRune(b, c)
		case k == 4:
			b = utf8.AppendRune(b, rune(0x10000+r.IntN(0x110000-0x10000)))
		case k == 5 && anyBytes:
			b = append(b, byte(r.IntN(256)))
		default:
			b = append(b, "中文"[r.IntN(2)*3:][:3]...)
		}
	}
	return string(b)
}

func TestUTF8UnitsMatchesValidString(t *testing.T) {
	cases := []string{"", "a", "é", "中", "😀", "\x80", "\xc0\x80", "\xc1\xbf", "\xc2\x80",
		"\xe0\x80\x80", "\xe0\xa0\x80", "\xed\x9f\xbf", "\xed\xa0\x80", "\xef\xbf\xbf",
		"\xf0\x8f\xbf\xbf", "\xf0\x90\x80\x80", "\xf4\x8f\xbf\xbf", "\xf4\x90\x80\x80",
		"\xf5\x80\x80\x80", "\xff", "\xe4\xb8", "\xf0\x9f\x98", "abcdefg\xe4", "abcdefgh中"}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		cases = append(cases, randomText(r, r.IntN(40), false, r.IntN(2) == 0))
	}
	for _, s := range cases {
		n, ok := utf8Units(s)
		require.Equal(t, utf8.ValidString(s), ok, "%q", s)
		if ok {
			require.Equal(t, len(utf16.Encode([]rune(s))), n, "%q", s)
		}
	}
}

func TestAppendWellFormedMatches(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 20000 {
		s := randomText(r, r.IntN(40), false, false)
		require.Equal(t, appendUTF16(nil, s), appendWellFormed(nil, s), "%q", s)
		w := randomText(r, r.IntN(40), true, false)
		got := appendWellFormed(nil, w)
		require.Equal(t, appendWTF8Units(nil, w), got, "%q", w)
		require.Equal(t, wtf8Units(w), len(got), "%q", w)
	}
}

func BenchmarkHostUnits(b *testing.B) {
	text := strings.Repeat("中文混合 text 内容😀。", 512)
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		hostUnits(text)
	}
}

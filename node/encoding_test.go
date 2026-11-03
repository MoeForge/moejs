package node

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"testing"

	"github.com/MoeForge/moejs/engine"
	"github.com/stretchr/testify/require"
)

// TestBase64 checks the table-driven encoder and decoder against the
// standard library's, at every length near the block sizes, and the
// lenient decoder's handling of what the fast one refuses.
func TestBase64(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for n := range 200 {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		std := base64.StdEncoding.EncodeToString(b)
		url := base64.RawURLEncoding.EncodeToString(b)
		require.Equal(t, std, string(encodeBase64(b, &stdPairs, true)), "length %d", n)
		require.Equal(t, url, string(encodeBase64(b, &urlPairs, false)), "length %d", n)
		for _, s := range []string{std, url, base64.RawStdEncoding.EncodeToString(b)} {
			got, ok := decodeBase64Fast(s)
			require.True(t, ok, s)
			require.Equal(t, b, got, s)
			require.Equal(t, b, decodeBase64(engine.FromGoString(s)), s)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"aGVs bG8=", "hello"}, {"aGVs\nbG8", "hello"}, {"YQ==YQ==", "a"}, {"Y", ""}, {"YQ", "a"},
		{"a-_b", "\x6b\xef\xdb"}, {"ab+/", "\x69\xbf\xbf"}, {"=", ""}, {"!!YWJj", "abc"},
	} {
		require.Equal(t, c.want, string(decodeBase64(engine.FromGoString(c.in))), c.in)
	}
}

// FuzzBase64 checks the encoder against encoding/base64, and the fast
// decoder against the lenient one wherever it takes the input.
func FuzzBase64(f *testing.F) {
	f.Add([]byte("hello, world"), "aGVsbG8=")
	f.Add(make([]byte, 100), "YQ==YQ==")
	f.Add([]byte{0xff, 0xfe, 0xfd}, "a-_b+/ab")
	f.Fuzz(func(t *testing.T, b []byte, s string) {
		std := base64.StdEncoding.EncodeToString(b)
		if got := string(encodeBase64(b, &stdPairs, true)); got != std {
			t.Fatalf("encode %x: %s, want %s", b, got, std)
		}
		if got, want := string(encodeBase64(b, &urlPairs, false)), base64.RawURLEncoding.EncodeToString(b); got != want {
			t.Fatalf("encode url %x: %s, want %s", b, got, want)
		}
		if got, ok := decodeBase64Fast(std); !ok || string(got) != string(b) {
			t.Fatalf("decode %s: %x %v", std, got, ok)
		}
		if got, ok := decodeBase64Fast(s); ok {
			if want := decodeBase64Lenient(engine.FromGoString(s)); string(got) != string(want) {
				t.Fatalf("decode %q: fast %x, lenient %x", s, got, want)
			}
		}
	})
}

// FuzzHex checks hex against encoding/hex, and the decoding of any
// string against Node's rule: the pairs up to the first that is not one.
func FuzzHex(f *testing.F) {
	f.Add([]byte("hello"), "6869zz")
	f.Add(make([]byte, 33), "ABcd0")
	f.Fuzz(func(t *testing.T, b []byte, s string) {
		h := hex.EncodeToString(b)
		if got := string(encodeHex(b)); got != h {
			t.Fatalf("encode %x: %s", b, got)
		}
		if got := decodeHex(engine.FromGoString(h)); string(got) != string(b) {
			t.Fatalf("decode %s: %x", h, got)
		}
		str := engine.FromGoString(s)
		var want []byte
		for i := 0; i+1 < str.Len(); i += 2 {
			// A character above U+00FF counts by its low byte, as in Node.
			c, d := byte(str.At(i)), byte(str.At(i+1))
			v, err := hex.DecodeString(string([]byte{c, d}))
			if err != nil {
				break
			}
			want = append(want, v...)
		}
		if got := decodeHex(str); string(got) != string(want) {
			t.Fatalf("decode %q: %x, want %x", s, got, want)
		}
	})
}

func BenchmarkBase64(b *testing.B) {
	data := make([]byte, 64<<10)
	for i := range data {
		data[i] = byte(i * 31)
	}
	text := base64.StdEncoding.EncodeToString(data)
	b.Run("encode", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			encodeBase64(data, &stdPairs, true)
		}
	})
	b.Run("decode", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			decodeBase64Fast(text)
		}
	})
	b.Run("stdlib-encode", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			base64.StdEncoding.EncodeToString(data)
		}
	})
	b.Run("stdlib-decode", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			base64.StdEncoding.DecodeString(text)
		}
	})
	b.Run("hex-encode", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			encodeHex(data)
		}
	})
}

// decodeQuad is the decoder of four one-character tables, kept to compare
// with the pair table in BenchmarkBase64.
func decodeQuad(a string) []byte {
	out := make([]byte, len(a)/4*3)
	t := &base64Dec
	t0, t1, t2, t3 := &t[0], &t[1], &t[2], &t[3]
	src, dst := []byte(a), out
	for len(src) >= 16 && len(dst) >= 14 {
		x := binary.LittleEndian.Uint64(src)
		y := binary.LittleEndian.Uint64(src[8:])
		v0 := t0[byte(x)] | t1[byte(x>>8)] | t2[byte(x>>16)] | t3[byte(x>>24)]
		v1 := t0[byte(x>>32)] | t1[byte(x>>40)] | t2[byte(x>>48)] | t3[byte(x>>56)]
		v2 := t0[byte(y)] | t1[byte(y>>8)] | t2[byte(y>>16)] | t3[byte(y>>24)]
		v3 := t0[byte(y>>32)] | t1[byte(y>>40)] | t2[byte(y>>48)] | t3[byte(y>>56)]
		if (v0|v1|v2|v3)&(1<<31) != 0 {
			return nil
		}
		binary.BigEndian.PutUint64(dst, uint64(v0)<<40|uint64(v1)<<16)
		binary.BigEndian.PutUint64(dst[6:], uint64(v2)<<40|uint64(v3)<<16)
		src, dst = src[16:], dst[12:]
	}
	return out
}

func BenchmarkBase64Quad(b *testing.B) {
	data := make([]byte, 64<<10)
	for i := range data {
		data[i] = byte(i * 31)
	}
	text := base64.StdEncoding.EncodeToString(data)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		decodeQuad(text)
	}
}

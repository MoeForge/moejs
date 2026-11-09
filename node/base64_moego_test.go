package node

import (
	"encoding/base64"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/engine"
)

// oldDecodeBase64 is decodeBase64 before moego: the four-character table
// decoder, else the lenient loop.
func oldDecodeBase64(s *engine.String) []byte {
	if a, ok := s.ASCII(); ok {
		if out, ok := decodeBase64Fast(a); ok {
			return out
		}
	}
	return decodeBase64Lenient(s)
}

// TestBase64MoegoDiff checks decodeBase64 against oldDecodeBase64 on
// base64 of either alphabet or both, with and without padding, at lengths
// around moego's 32-character blocks, and with one change somewhere:
// whitespace, an '=', a character of neither alphabet or one above U+00FF.
func TestBase64MoegoDiff(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	lengths := []int{0, 1, 2, 3, 4, 5, 22, 23, 24, 25, 47, 48, 49, 95, 96, 97, 191, 192, 193, 767, 768, 769, 3000, 1 << 16}
	edits := []string{"", "\n", "\r\n", " ", "\t", "=", "==", "!", "-", "_", "+", "/", "\x00", "\x80", "é", "€"}
	for _, n := range lengths {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		encs := []string{
			base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
			base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b),
		}
		// Both alphabets in one string.
		mixed := []byte(encs[0])
		for i := range mixed {
			if mixed[i] == '+' && rng.IntN(2) == 0 {
				mixed[i] = '-'
			}
		}
		encs = append(encs, string(mixed))
		for _, e := range encs {
			for _, ed := range edits {
				for k := range 3 {
					in := e
					if ed != "" {
						at := rng.IntN(len(e) + 1)
						if k == 1 {
							at = len(e)
						} else if k == 2 {
							at = len(e) &^ 3
						}
						in = e[:at] + ed + e[at:]
					}
					s := engine.FromGoString(in)
					want := oldDecodeBase64(s)
					for _, url := range []bool{false, true} {
						if got := decodeBase64(s, url); string(got) != string(want) {
							t.Fatalf("decode %.80q (length %d, url %v): %x, before moego %x", in, len(in), url, got, want)
						}
						// What moego takes, also where decodeBase64 does not use it.
						if got, ok := decodeBase64Moego(in, url); ok && string(got) != string(want) {
							t.Fatalf("moego decode %.80q (length %d, url %v): %x, before moego %x", in, len(in), url, got, want)
						}
					}
				}
			}
		}
	}
	// A character of the other alphabet at every position of a short
	// string.
	for i := range 40 {
		in := []byte(strings.Repeat("QUJD", 10))
		in[i] = "-_+/"[i%4]
		s := engine.FromGoString(string(in))
		for _, url := range []bool{false, true} {
			want := oldDecodeBase64(s)
			if got := decodeBase64(s, url); string(got) != string(want) {
				t.Fatalf("decode %q (url %v): %x, before moego %x", in, url, got, want)
			}
			got, ok := decodeBase64Moego(string(in), url)
			if !ok || string(got) != string(want) {
				t.Fatalf("moego decode %q (url %v): %x, %v, before moego %x", in, url, got, ok, want)
			}
		}
	}
}

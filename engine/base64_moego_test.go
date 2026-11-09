package engine

import (
	"bytes"
	stdbase64 "encoding/base64"
	"math/rand/v2"
	"testing"
)

// The base64 entry points before moego, kept to compare with: atobSlow is
// atob's old decoder whole, oldBtoa is btoa's old encoder, and
// oldDecodeBase64 is fromBase64 over all of the string, without
// decodeBase64Bulk.

func oldBtoa(r *Realm, s *String) (Value, error) {
	n := s.Len()
	size := (n + 2) / 3 * 4
	if size > maxStringLength || r.overBudget(size) {
		return Undefined(), r.invalidStringLength()
	}
	r.chargeString(size)
	out := make([]byte, 0, size)
	for i := 0; i < n; i += 3 {
		var acc uint32
		k := min(3, n-i)
		for j := range k {
			c := s.At(i + j)
			if c > 0xFF {
				return Undefined(), r.invalidCharacterError("Invalid character")
			}
			acc |= uint32(c) << (16 - 8*j)
		}
		out = append(out, base64Alphabet[acc>>18], base64Alphabet[acc>>12&63], '=', '=')
		if k > 1 {
			out[len(out)-2] = base64Alphabet[acc>>6&63]
		}
		if k > 2 {
			out[len(out)-1] = base64Alphabet[acc&63]
		}
	}
	return StringValue(asciiString(bytesToString(out))), nil
}

func (r *Realm) oldDecodeBase64(dst []byte, s *String, url bool, h base64Handling) (read, written int, ok bool, err error) {
	if a, ok := s.ASCII(); ok {
		return fromBase64(r, dst, asciiBytes(a), url, h)
	}
	return fromBase64(r, dst, s.UTF16(), url, h)
}

// base64DiffInputs returns base64 of random bytes of each size, in both
// alphabets with and without padding, as it is and changed once: an
// insertion (whitespace, padding, a character of neither alphabet or of
// the other, one that is not ASCII) at the start, the end, the last chunk
// boundary, a random place or around block boundaries, the last one to
// three characters dropped, or the last character before the padding
// replaced (trailing bits).
// Inputs past 64 KiB take fewer insertions and places, which the
// boundaries of copyBytesChunk need.
func base64DiffInputs(rng *rand.Rand, sizes []int, edits []string) []string {
	var out []string
	for _, n := range sizes {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		long := n > 64<<10
		for _, enc := range []*stdbase64.Encoding{stdbase64.StdEncoding, stdbase64.RawStdEncoding, stdbase64.URLEncoding, stdbase64.RawURLEncoding} {
			e := enc.EncodeToString(b)
			out = append(out, e)
			for _, ed := range edits {
				if long && !bytes.Contains([]byte("\n=!é"), []byte(ed)) {
					continue
				}
				places := []int{0, len(e), len(e) &^ 3, rng.IntN(len(e) + 1)}
				for _, p := range []int{4 << 10, 12 << 10, 1 << 20, 3 << 20} {
					if p < len(e) && (!long || p == 1<<20) {
						places = append(places, p-1, p, p+1)
					}
				}
				for _, at := range places {
					out = append(out, e[:at]+ed+e[at:])
				}
			}
			for k := 1; k <= 3 && k <= len(e); k++ {
				out = append(out, e[:len(e)-k])
			}
			body := len(e)
			for body > 0 && e[body-1] == '=' {
				body--
			}
			if body > 0 {
				x := []byte(e)
				x[body-1] = "AZaz09+/-_"[rng.IntN(10)]
				out = append(out, string(x))
			}
		}
	}
	return out
}

var base64DiffEdits = []string{"\n", "\r\n", " ", "\t", "\f", "=", "==", "!", "-", "_", "+", "/", "\x00", "é", "Ā"}

func base64DiffSizes(t *testing.T) []int {
	sizes := []int{0, 1, 2, 3, 4, 5, 6, 16, 23, 24, 25, 47, 48, 49, 256, 3069, 3070, 3071, 3072, 3073, 6143, 6144, 6145, 9216, 64 << 10}
	if !testing.Short() && !raceEnabled {
		sizes = append(sizes, 1<<20/4*3, 1<<20+5)
	}
	return sizes
}

// TestAtobMoegoDiff checks atob against atobSlow on base64DiffInputs.
func TestAtobMoegoDiff(t *testing.T) {
	r := NewRealm()
	rng := rand.New(rand.NewPCG(7, 8))
	for _, in := range base64DiffInputs(rng, base64DiffSizes(t), base64DiffEdits) {
		s := FromGoString(in)
		got, gotErr := globalAtob(r, Undefined(), []Value{StringValue(s)})
		want, wantErr := atobSlow(r, s)
		if (gotErr == nil) != (wantErr == nil) || gotErr == nil && !got.AsString().Equals(want.AsString()) {
			t.Fatalf("atob(%.60q, length %d) = %v, before moego %v", in, len(in), gotErr, wantErr)
		}
		if gotErr != nil && gotErr.Error() != wantErr.Error() {
			t.Fatalf("atob(%.60q): %v, before moego %v", in, gotErr, wantErr)
		}
	}
}

// TestBtoaMoegoDiff checks btoa against oldBtoa on ASCII, Latin-1 and
// UTF-16 strings, with a unit above U+00FF in several places, at lengths
// around btoaChunk.
func TestBtoaMoegoDiff(t *testing.T) {
	r := NewRealm()
	rng := rand.New(rand.NewPCG(9, 10))
	sizes := []int{0, 1, 2, 3, 4, 24, 25, 100, btoaChunk - 1, btoaChunk, btoaChunk + 1, 2*btoaChunk + 2, 100000}
	if !testing.Short() {
		sizes = append(sizes, encodeChunk-1, encodeChunk, encodeChunk+1, encodeChunk+3*btoaChunk+2)
	}
	for _, n := range sizes {
		for _, top := range []int{0x80, 0x100, 0x10000} {
			u := make([]uint16, n)
			for i := range u {
				u[i] = uint16(rng.IntN(top))
			}
			cases := [][]uint16{u}
			if n > 0 && top <= 0x100 {
				for _, at := range []int{0, n / 2, n - 1, min(n-1, btoaChunk)} {
					v := append([]uint16(nil), u...)
					v[at] = 0x100 + uint16(rng.IntN(0xFF00))
					cases = append(cases, v)
				}
			}
			for _, c := range cases {
				s := FromUTF16(append([]uint16(nil), c...))
				got, gotErr := globalBtoa(r, Undefined(), []Value{StringValue(s)})
				want, wantErr := oldBtoa(r, s)
				if (gotErr == nil) != (wantErr == nil) || gotErr == nil && !got.AsString().Equals(want.AsString()) {
					t.Fatalf("btoa(length %d, units below %#x) = %v, before moego %v", n, top, gotErr, wantErr)
				}
			}
		}
	}
}

// TestUint8ArrayBase64MoegoDiff checks decodeBase64 against
// oldDecodeBase64 on base64DiffInputs, both alphabets and every
// lastChunkHandling: into a fresh buffer of the size fromBase64 makes,
// and, as setFromBase64 does, into targets of several sizes around what
// the input decodes to, filled with 0xA5, all of whose bytes must end the
// same.
func TestUint8ArrayBase64MoegoDiff(t *testing.T) {
	r := NewRealm()
	rng := rand.New(rand.NewPCG(11, 12))
	for _, in := range base64DiffInputs(rng, base64DiffSizes(t), base64DiffEdits) {
		s := FromGoString(in)
		full := (len(in) + 3) / 4 * 3
		for _, url := range []bool{false, true} {
			for h := base64Loose; h <= base64StopBeforePartial; h++ {
				sizes := []int{full, 0, 1, 2, 3, 4, 5}
				m := len(in) / 4 * 3
				if m > 64<<10 {
					sizes = []int{full, m - 1, m}
				} else if m > 5 {
					sizes = append(sizes, m-3, m-2, m-1, m, m+1, rng.IntN(m))
				}
				for i, n := range sizes {
					a, b := bytes.Repeat([]byte{0xA5}, n), bytes.Repeat([]byte{0xA5}, n)
					read, written, ok, err := r.decodeBase64(a, s, url, h, i > 0)
					wantRead, wantWritten, wantOK, wantErr := r.oldDecodeBase64(b, s, url, h)
					if err != nil || wantErr != nil {
						t.Fatal(err, wantErr)
					}
					// A fresh buffer's bytes past written are not kept.
					if i == 0 {
						a, b = a[:written], b[:wantWritten]
					}
					if read != wantRead || written != wantWritten || ok != wantOK || !bytes.Equal(a, b) {
						t.Fatalf("decode %.60q (length %d, url %v, handling %d) into %d bytes = %d, %d, %v; before moego %d, %d, %v; bytes equal %v",
							in, len(in), url, h, n, read, written, ok, wantRead, wantWritten, wantOK, bytes.Equal(a, b))
					}
				}
			}
		}
	}
}

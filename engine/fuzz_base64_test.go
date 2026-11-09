package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// fuzzLatin1 turns bytes into the JS string of the same code units.
func fuzzLatin1(b []byte) *String {
	units := make([]uint16, len(b))
	for i, c := range b {
		units[i] = uint16(c)
	}
	return FromUTF16(units)
}

// forgivingBase64 is the WHATWG forgiving-base64 decode written against
// encoding/base64: ASCII whitespace is dropped, one or two trailing "=" are
// removed from a multiple-of-four length, and the rest must be unpadded
// base64 whose length is not 1 mod 4 (unused trailing bits are ignored).
func forgivingBase64(s string) ([]byte, bool) {
	s = strings.Map(func(c rune) rune {
		if c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' ' {
			return -1
		}
		return c
	}, s)
	if len(s)%4 == 0 {
		if strings.HasSuffix(s, "==") {
			s = s[:len(s)-2]
		} else if strings.HasSuffix(s, "=") {
			s = s[:len(s)-1]
		}
	}
	if len(s)%4 == 1 || strings.ContainsRune(s, '=') {
		return nil, false
	}
	b, err := base64.RawStdEncoding.DecodeString(s)
	return b, err == nil
}

// FuzzBase64 checks btoa against encoding/base64, that atob inverts it, that
// atob accepts exactly what forgiving-base64 accepts and that unescape
// inverts escape; then the Uint8Array members (fuzzUint8ArrayBase64).
func FuzzBase64(f *testing.F) {
	for _, s := range []string{"", "f", "foobar", "Zm9vYg", " Zm9v\nYg==\t", "Zg=", "====", "Zm+/", "Zh", "é", "%u0041%4",
		"Zm9vYmE", "Zm9vYh==", "Zg=\n=", "Zm9v=", "-_8", "0aF9", "abc", "aa\xe9b"} {
		f.Add([]byte(s), s)
	}
	f.Fuzz(func(t *testing.T, data []byte, s string) {
		if len(data)+len(s) > 1024 {
			return
		}
		r := NewRealm()
		call := func(name string, arg *String) (*String, error) {
			fn, err := r.Global.GetProp(r, r.KeyFromGoString(name))
			if err != nil {
				t.Fatal(err)
			}
			v, err := r.Call(fn, Undefined(), []Value{StringValue(arg)})
			if err != nil {
				return nil, err
			}
			return v.AsString(), nil
		}
		enc, err := call("btoa", fuzzLatin1(data))
		if err != nil {
			t.Fatalf("btoa(%q): %v", data, err)
		}
		if got, want := enc.GoString(), base64.StdEncoding.EncodeToString(data); got != want {
			t.Fatalf("btoa(%q) = %q, want %q", data, got, want)
		}
		dec, err := call("atob", enc)
		if err != nil || !dec.Equals(fuzzLatin1(data)) {
			t.Fatalf("atob(btoa(%q)) = %v, %v", data, dec, err)
		}

		want, ok := forgivingBase64(s)
		got, err := call("atob", FromGoString(s))
		if ok != (err == nil) {
			t.Fatalf("atob(%q): error %v, forgiving-base64 ok %v", s, err, ok)
		}
		if ok && !got.Equals(fuzzLatin1(want)) {
			t.Fatalf("atob(%q) = %v, want %q", s, got.UTF16(), want)
		}

		js := fuzzCollString(s)
		esc, err := call("escape", js)
		if err != nil {
			t.Fatal(err)
		}
		back, err := call("unescape", esc)
		if err != nil || !back.Equals(js) {
			t.Fatalf("unescape(escape(%q)) = %v, %v", s, back, err)
		}

		fuzzUint8ArrayBase64(t, r, data, s)
	})
}

// fuzzUint8ArrayBase64 checks toBase64 and toHex of data against
// encoding/base64 and encoding/hex and that fromBase64 and fromHex invert
// them; that fromBase64 (loose) accepts exactly what forgiving-base64
// accepts and fromHex what encoding/hex does, over the bytes of s as code
// units; that strict and stop-before-partial decode a prefix of what loose
// does; and that setFromBase64 and setFromHex into a short target write
// what decoding the part they read gives, all of it when it fits.
func fuzzUint8ArrayBase64(t *testing.T, r *Realm, data []byte, s string) {
	u8 := func(b []byte) Value {
		v, err := r.newUint8Array(append(newBytes(0, len(b)), b...))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	bytesOf := func(v Value) []byte { return dataOf(v.AsObject(), kindTypedArray).viewed() }
	opts := func(alphabet, handling string, omitPadding bool) Value {
		o := r.NewObject()
		a := binaryNames()
		o.CreateDataProperty(r, StringKey(a.alphabet), StringValue(FromGoString(alphabet)))
		o.CreateDataProperty(r, StringKey(a.lastChunkHandling), StringValue(FromGoString(handling)))
		o.CreateDataProperty(r, StringKey(a.omitPadding), Bool(omitPadding))
		return ObjectValue(o)
	}
	fromBase64 := func(s *String, alphabet, handling string) ([]byte, bool) {
		v, err := uint8ArrayFromBase64(r, Undefined(), []Value{StringValue(s), opts(alphabet, handling, false)})
		if err != nil {
			return nil, false
		}
		return bytesOf(v), true
	}
	fromHex := func(s *String) ([]byte, bool) {
		v, err := uint8ArrayFromHex(r, Undefined(), []Value{StringValue(s)})
		if err != nil {
			return nil, false
		}
		return bytesOf(v), true
	}

	u := u8(data)
	for i, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		alphabet := [2]string{"base64", "base64url"}[i/2]
		v, err := uint8ArrayToBase64(r, u, []Value{opts(alphabet, "", i%2 == 1)})
		if want := enc.EncodeToString(data); err != nil || v.AsString().GoString() != want {
			t.Fatalf("toBase64(%x, %s, omitPadding %v) = %v, %v, want %q", data, alphabet, i%2 == 1, v, err, want)
		}
		handling := "strict"
		if i%2 == 1 {
			handling = "loose"
		}
		if back, ok := fromBase64(v.AsString(), alphabet, handling); !ok || !bytes.Equal(back, data) {
			t.Fatalf("fromBase64(toBase64(%x), %s, %s) = %x, %v", data, alphabet, handling, back, ok)
		}
	}
	v, err := uint8ArrayToHex(r, u, nil)
	if want := hex.EncodeToString(data); err != nil || v.AsString().GoString() != want {
		t.Fatalf("toHex(%x) = %v, %v, want %q", data, v, err, want)
	}
	if back, ok := fromHex(v.AsString()); !ok || !bytes.Equal(back, data) {
		t.Fatalf("fromHex(toHex(%x)) = %x, %v", data, back, ok)
	}

	js := fuzzLatin1([]byte(s))
	want, ok := forgivingBase64(s)
	got, gotOK := fromBase64(js, "base64", "loose")
	if ok != gotOK || ok && !bytes.Equal(got, want) {
		t.Fatalf("fromBase64(%q) = %x, %v, want %x, %v", s, got, gotOK, want, ok)
	}
	if strict, sok := fromBase64(js, "base64", "strict"); sok && (!ok || !bytes.Equal(strict, want)) {
		t.Fatalf("fromBase64(%q, strict) = %x, loose %x, %v", s, strict, want, ok)
	}
	if partial, pok := fromBase64(js, "base64", "stop-before-partial"); ok && !pok || pok && ok && (!bytes.HasPrefix(want, partial) || len(want)-len(partial) > 2) {
		t.Fatalf("fromBase64(%q, stop-before-partial) = %x, %v, loose %x", s, partial, pok, want)
	}

	// set runs setFromBase64 or setFromHex into n bytes of 0xA5 and
	// checks what it wrote against decode of the units it read. A target
	// the bytes fill exactly stops reading after the chunk that fills it.
	n := len(data) % (len(want) + 2)
	set := func(name string, fn NativeFunc, decode func(*String) ([]byte, bool)) (read, written int, ok bool) {
		dst := u8(bytes.Repeat([]byte{0xA5}, n))
		v, err := fn(r, dst, []Value{StringValue(js)})
		if err != nil {
			return 0, 0, false
		}
		o := v.AsObject()
		a := binaryNames()
		rv, _ := o.GetProp(r, StringKey(a.read))
		wv, _ := o.GetProp(r, StringKey(a.written))
		read, written = int(rv.AsNumber()), int(wv.AsNumber())
		out := bytesOf(dst)
		part, pok := decode(fuzzLatin1([]byte(s)[:read]))
		if written > n || !pok || !bytes.Equal(part, out[:written]) || bytes.Count(out[written:], []byte{0xA5}) != len(out)-written {
			t.Fatalf("%s(%q) into %d bytes = %d, %d, %x; decoding what it read gives %x, %v", name, s, n, read, written, out, part, pok)
		}
		return read, written, true
	}
	if read, written, sok := set("setFromBase64", uint8ArraySetFromBase64, func(s *String) ([]byte, bool) { return fromBase64(s, "base64", "loose") }); ok &&
		(!sok || len(want) <= n && written != len(want) || len(want) < n && read != len(s) || len(want) > n && written != n/3*3) {
		t.Fatalf("setFromBase64(%q) into %d bytes = %d, %d, %v; it decodes to %x", s, n, read, written, sok, want)
	}
	hexWant, err := hex.DecodeString(s)
	if hexGot, hok := fromHex(js); hok != (err == nil) || hok && !bytes.Equal(hexGot, hexWant) {
		t.Fatalf("fromHex(%q) = %x, %v, want %x, %v", s, hexGot, hok, hexWant, err)
	}
	if read, written, sok := set("setFromHex", uint8ArraySetFromHex, fromHex); err == nil && (!sok || written != min(n, len(hexWant)) || read != 2*written) {
		t.Fatalf("setFromHex(%q) into %d bytes = %d, %d, %v; it decodes to %x", s, n, read, written, sok, hexWant)
	}
}

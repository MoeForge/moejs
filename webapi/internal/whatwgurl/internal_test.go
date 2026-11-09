package whatwgurl

import (
	"testing"
	"unicode/utf8"
)

func TestPunycode(t *testing.T) {
	for _, c := range []struct{ uni, puny string }{
		// RFC 3492 §7.1 samples.
		{"ليهمابتكلموشعربي؟", "egbpdaj6bu4bxfgehfvwxn"},
		{"他们为什么不说中文", "ihqwcrb4cv8a8dqg056pqjye"},
		{"Pročprostěnemluvíčesky", "Proprostnemluvesky-uyb24dma41a"},
		{"למההםפשוטלאמדבריםעברית", "4dbcagdahymbxekheh6e0a7fei0b"},
		{"3年B組金八先生", "3B-ww4c5e180e575a65lsy2b"},
		{"-> $1.00 <-", "-> $1.00 <--"},
		{"bücher", "bcher-kva"},
		{"ß", "zca"},
		{"\U0001f600", "e28h"},
	} {
		got, ok := punyEncode(nil, []rune(c.uni))
		if !ok || string(got) != c.puny {
			t.Errorf("punyEncode(%q) = %q, %v; want %q", c.uni, got, ok, c.puny)
		}
		dec, ok := punyDecode(nil, c.puny)
		if !ok || string(dec) != c.uni {
			t.Errorf("punyDecode(%q) = %q, %v; want %q", c.puny, string(dec), ok, c.uni)
		}
	}
	for _, bad := range []string{"-", "a-\u00e9", "99999999999", "zzzzzzzzzzzz", "a!", "-a"} {
		if dec, ok := punyDecode(nil, bad); ok {
			t.Errorf("punyDecode(%q) = %q, want failure", bad, string(dec))
		}
	}
	// Decoding to a surrogate or past U+10FFFF fails.
	for _, r := range []rune{0xd800, 0xdfff, 0x110000} {
		enc, _ := punyEncode(nil, []rune{r})
		if dec, ok := punyDecode(nil, string(enc)); ok {
			t.Errorf("punyDecode(%q) = %U, want failure", enc, dec)
		}
	}
}

func TestIdnaLookup(t *testing.T) {
	if idnaUnicodeVersion != "17.0.0" {
		t.Errorf("idnaUnicodeVersion = %q", idnaUnicodeVersion)
	}
	m := func(r rune) string {
		kind, start, data := idnaLookup(r)
		switch kind {
		case kindDisallowed:
			return "disallowed"
		case kindIgnored:
			return "ignored"
		case kindValid:
			return string(r)
		case kindPairs:
			return string(r + 1 - (r-start)&1)
		case kindDelta:
			return string(r + rune(data) - 1<<21)
		}
		off, stride, n := int(data>>16), int(data>>8&0xff), int(data&0xff)
		off += int(r-start) * stride
		return idnaPool[off : off+n]
	}
	for _, c := range []struct {
		r    rune
		want string
	}{
		{'a', "a"}, {'A', "a"}, {'Z', "z"}, {'@', "@"}, {0, "\x00"}, {0x7f, "\x7f"},
		{0xad, "ignored"}, {0xdf, "\u00df"}, {0xc0, "\u00e0"}, {0xd7, "\u00d7"}, {0xde, "\u00fe"},
		{0x100, "\u0101"}, {0x101, "\u0101"}, {0x130, "i\u0307"}, {0x3c2, "\u03c2"}, {0x200c, "\u200c"},
		{0x200b, "ignored"}, {0xfeff, "ignored"}, {0xfffd, "disallowed"}, {0x3002, "."}, {0xff21, "a"},
		{0x2488, "disallowed"}, {0x2474, "(1)"}, {0xfdfa, "\u0635\u0644\u0649 \u0627\u0644\u0644\u0647 \u0639\u0644\u064a\u0647 \u0648\u0633\u0644\u0645"},
		{0xf900, "\u8c48"}, {0x2f800, "\u4e3d"}, {0x1d400, "a"}, {0x1d6a8, "\u03b1"}, {0x10ffff, "disallowed"},
		{0x323b0, "\U000323b0"}, {0x3347a, "disallowed"},
	} {
		if got := m(c.r); got != c.want {
			t.Errorf("U+%04X maps to %+q, want %+q", c.r, got, c.want)
		}
	}
	for _, c := range []struct {
		r    rune
		prop uint32
	}{
		{'a', bidiL}, {'0', bidiEN}, {'-', bidiES}, {'.', bidiCS}, {0x5d0, bidiR}, {0x627, bidiAL | joinR<<propJoinShift},
		{0x628, bidiAL | joinD<<propJoinShift}, {0x660, bidiAN}, {0x300, bidiNSM | joinT<<propJoinShift | propMark},
		{0x94d, bidiNSM | joinT<<propJoinShift | propMark | propVirama}, {0x200c, bidiBN}, {0xa872, bidiL | joinL<<propJoinShift},
	} {
		if got := idnaProp(c.r); got != c.prop {
			t.Errorf("idnaProp(U+%04X) = %#x, want %#x", c.r, got, c.prop)
		}
	}
	// Every string of idnaPool a range points into is whole UTF-8.
	for i, e := range idnaTable {
		if e&(1<<10) == 0 {
			continue
		}
		d := idnaData[e&(1<<10-1)]
		if d&(1<<31) == 0 {
			continue
		}
		off, stride, n := int(d>>16&0x7fff), int(d>>8&0xff), int(d&0xff)
		end := rune(0x110000)
		if i+1 < len(idnaTable) {
			end = rune(idnaTable[i+1] >> 11)
		}
		for r := rune(e >> 11); r < end; r++ {
			s := idnaPool[off+int(r-rune(e>>11))*stride:][:n]
			if !utf8.ValidString(s) || s == "" {
				t.Fatalf("U+%04X maps to %+q", r, s)
			}
		}
	}
}

func TestHostSerializers(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"0x7f.1", "127.0.0.1"}, {"0300.0250.0.01", "192.168.0.1"}, {"4294967295", "255.255.255.255"},
		{"1.2.3.4.", "1.2.3.4"}, {"0", "0.0.0.0"}, {"0x", "0.0.0.0"}, {"1.256", "1.0.1.0"}, {"1.0x", "1.0.0.0"}, {"1.0x1g", "1.0x1g"},
	} {
		h, ok := (*Options)(nil).parseHost(c.in, false)
		if !ok || h != c.want {
			t.Errorf("parseHost(%q) = %q, %v; want %q", c.in, h, ok, c.want)
		}
	}
	for _, in := range []string{"4294967296", "1.2.3.256", "1.2.3.4.5", "0x100000000", "09", "1..2"} {
		if h, ok := (*Options)(nil).parseHost(in, false); ok {
			t.Errorf("parseHost(%q) = %q, want failure", in, h)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"[::]", "[::]"}, {"[::1]", "[::1]"}, {"[1::]", "[1::]"}, {"[0:0:1:0:0:0:0:1]", "[0:0:1::1]"},
		{"[1:0:0:2:0:0:0:3]", "[1:0:0:2::3]"}, {"[1:0:0:2:0:0:3:4]", "[1::2:0:0:3:4]"}, {"[1:2:3:4:5:6:7:8]", "[1:2:3:4:5:6:7:8]"},
		{"[::ffff:1.2.3.4]", "[::ffff:102:304]"}, {"[ABCD::0EF]", "[abcd::ef]"}, {"[1:0:2:0:3:0:4:0]", "[1:0:2:0:3:0:4:0]"},
	} {
		h, ok := (*Options)(nil).parseHost(c.in, false)
		if !ok || h != c.want {
			t.Errorf("parseHost(%q) = %q, %v; want %q", c.in, h, ok, c.want)
		}
	}
}

// TestNFCHook checks that domain to ASCII normalizes through the hook, and
// only for a host with non-ASCII input; the zero Options skip normalization.
func TestNFCHook(t *testing.T) {
	calls := 0
	o := &Options{NFC: func(s string) string {
		calls++
		if s == "e\u0301.example" {
			return "\u00e9.example"
		}
		return s
	}}
	u, err := o.Parse("https://e\u0301.example/", nil)
	if err != nil || u.Hostname() != "xn--9ca.example" || calls != 1 {
		t.Fatalf("with NFC: %v, %v, %d calls", u, err, calls)
	}
	// The setters use the URL's Options.
	u.SetHostname("E\u0301.example")
	if u.Hostname() != "xn--9ca.example" || calls != 2 {
		t.Errorf("hostname setter with NFC: %q, %d calls", u.Hostname(), calls)
	}
	c := u.Clone()
	c.SetHost("e\u0301.example:1")
	if c.Host() != "xn--9ca.example:1" || calls != 3 {
		t.Errorf("host setter of a clone: %q, %d calls", c.Host(), calls)
	}
	if _, err := o.Parse("https://api.example.com/v1", nil); err != nil || calls != 3 {
		t.Errorf("an ASCII host calls NFC: %d calls", calls)
	}
	u, err = Parse("https://e\u0301.example/", nil)
	if err != nil || u.Hostname() != "xn--e-xbb.example" {
		t.Errorf("without NFC: %v, %v", u, err)
	}
}

func TestParseAllocs(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  float64
		base string
	}{
		{"https://api.openai.com/v1/chat/completions?x=1", 1, ""},
		{"  https://api.openai.com/v1/chat/completions?x=1#top\n", 1, ""},
		{"https://user:pass@example.com:8443/a/b/c?d=e", 1, ""},
		{"http://127.0.0.1:8080/", 1, ""},
		{"http://[::1]:8080/", 1, ""},
		{"/v1/models", 2, "https://api.openai.com/v1/chat"},
		{"HTTPS://API.OpenAI.com/v1", 3, ""},
	} {
		var base *URL
		if c.base != "" {
			base, _ = Parse(c.base, nil)
		}
		allocs := testing.AllocsPerRun(100, func() {
			if _, err := Parse(c.in, base); err != nil {
				t.Fatal(err)
			}
		})
		if allocs > c.max {
			t.Errorf("Parse(%q) allocates %v times, want at most %v", c.in, allocs, c.max)
		}
	}
	u, _ := Parse("https://user:pw@api.openai.com:8443/v1/chat/completions?x=1#f", nil)
	if allocs := testing.AllocsPerRun(100, func() {
		_, _ = u.Href(), u.Origin()
		_, _ = u.Protocol(), u.Username()
		_, _ = u.Password(), u.Host()
		_, _ = u.Hostname(), u.Port()
		_, _ = u.Pathname(), u.Search()
		_ = u.Hash()
	}); allocs != 1 {
		// The origin of a URL with credentials is the only one to concatenate.
		t.Errorf("the getters allocate %v times, want 1", allocs)
	}
}

// TestHrefPastMax checks that the length of a serialization is counted
// past maxHref, math.MaxInt on 32-bit systems, without wrapping, so that
// set refuses it.
func TestHrefPastMax(t *testing.T) {
	w := hrefWriter{n: maxHref}
	w.put("x")
	if w.n != maxHref+1 {
		t.Fatalf("n = %d, want %d", w.n, int64(maxHref)+1)
	}
	w = hrefWriter{cmp: "x", eq: true, n: maxHref}
	w.put("x")
	if w.eq || w.n != maxHref+1 {
		t.Fatalf("compare past maxHref: eq = %v, n = %d", w.eq, w.n)
	}
}

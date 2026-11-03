package whatwgurl_test

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moejs/webapi/internal/whatwgurl"
)

// FuzzParse checks that the parser does not panic, that a URL it returns
// parses to itself, and that every setter keeps that so.
func FuzzParse(f *testing.F) {
	var cases []json.RawMessage
	readJSON(f, "urltestdata.json", &cases)
	for _, raw := range cases {
		var c struct {
			Input string
			Base  *string
		}
		if json.Unmarshal(raw, &c) == nil {
			base := ""
			if c.Base != nil {
				base = *c.Base
			}
			f.Add(c.Input, base, "x")
		}
	}
	f.Fuzz(func(t *testing.T, input, base, value string) {
		var b *whatwgurl.URL
		if base != "" {
			b, _ = whatwgurl.Parse(base, nil)
		}
		u, err := whatwgurl.Parse(input, b)
		if err != nil {
			return
		}
		checkFixedPoint(t, "Parse", u)
		_ = getters(u)
		for _, s := range []func(*whatwgurl.URL, string){
			(*whatwgurl.URL).SetProtocol, (*whatwgurl.URL).SetUsername, (*whatwgurl.URL).SetPassword,
			(*whatwgurl.URL).SetHost, (*whatwgurl.URL).SetHostname, (*whatwgurl.URL).SetPort,
			(*whatwgurl.URL).SetPathname, (*whatwgurl.URL).SetSearch, (*whatwgurl.URL).SetHash,
		} {
			for _, v := range []string{value, input} {
				c := u.Clone()
				s(c, v)
				checkFixedPoint(t, "a setter", c)
				_ = getters(c)
			}
		}
		c := u.Clone()
		c.SetQuery(string(whatwgurl.AppendForm(nil, whatwgurl.ParseForm(value))), value != "")
		checkFixedPoint(t, "SetQuery", c)
	})
}

// checkFixedPoint checks that u's serialization parses to itself with the
// same getters, and that the getters compose to it.
func checkFixedPoint(t *testing.T, what string, u *whatwgurl.URL) {
	t.Helper()
	p, err := whatwgurl.Parse(u.Href(), nil)
	if err != nil {
		t.Fatalf("%s gives %q, which fails to parse", what, u.Href())
	}
	if p.Href() != u.Href() {
		t.Fatalf("%s gives %q, which parses to %q", what, u.Href(), p.Href())
	}
	if g, h := getters(u), getters(p); !maps.Equal(g, h) {
		t.Fatalf("%s gives %q with getters %q, which parses to getters %q", what, u.Href(), g, h)
	}
	h := u.Protocol()
	if u.Host() != "" || strings.HasPrefix(u.Href()[len(h):], "//") {
		h += "//"
		if u.Username() != "" || u.Password() != "" {
			h += u.Username()
			if u.Password() != "" {
				h += ":" + u.Password()
			}
			h += "@"
		}
		h += u.Host()
	} else if strings.HasPrefix(u.Pathname(), "//") {
		h += "/."
	}
	h += u.Pathname()
	if q, ok := u.Query(); ok {
		h += "?" + q
	}
	if f, ok := u.Fragment(); ok {
		h += "#" + f
	}
	if h != u.Href() {
		t.Fatalf("%s gives %q, whose getters compose to %q", what, u.Href(), h)
	}
}

// TestHostileInputsLinear parses hostile inputs at size n and 2n: a parse
// time ratio near 4 would mean quadratic time.
func TestHostileInputsLinear(t *testing.T) {
	distinct := func(n int) string {
		var b strings.Builder
		for r := rune(0x4E00); b.Len() < 3*n; r++ {
			b.WriteRune(r)
		}
		return b.String()
	}
	cases := []struct {
		name string
		make func(n int) string
	}{
		{"distinct-cjk-label", func(n int) string { return "http://" + distinct(n/3) + "/" }},
		{"cjk-many-labels", func(n int) string { return "http://" + strings.Repeat("中.", n/4) + "/" }},
		{"xn-label", func(n int) string { return "http://xn--" + strings.Repeat("a", n) + "/" }},
		{"xn-label-digits", func(n int) string { return "http://xn--" + strings.Repeat("9a", n/2) + "/" }},
		{"many-dots", func(n int) string { return "http://" + strings.Repeat("a.", n/2) + "/" }},
		{"percent-host", func(n int) string { return "http://" + strings.Repeat("%41", n/3) + "/" }},
		{"dotdot-path", func(n int) string { return "http://h/" + strings.Repeat("a/", n/4) + strings.Repeat("../", n/6) }},
		{"dot-path", func(n int) string { return "http://h/" + strings.Repeat("./", n/2) }},
		{"pct-dotdot", func(n int) string { return "http://h/" + strings.Repeat("a/%2e%2E/", n/9) }},
		{"ipv4-long-hex", func(n int) string { return "http://0x" + strings.Repeat("0", n) + "1/" }},
		{"ipv4-many-parts", func(n int) string { return "http://" + strings.Repeat("1.", n/2) + "1/" }},
		{"ipv6-long", func(n int) string { return "http://[" + strings.Repeat("0", n) + "::1]/" }},
		{"query", func(n int) string { return "http://h/?" + strings.Repeat("&a=%zz", n/6) }},
		{"backslashes", func(n int) string { return "http:" + strings.Repeat("\\", n) + "h/" }},
		{"tabs", func(n int) string { return "http://h/" + strings.Repeat("a\t\n", n/3) }},
		{"file-drive", func(n int) string { return "file:///C:/" + strings.Repeat("../C:/", n/6) }},
		{"opaque", func(n int) string { return "foo:" + strings.Repeat(" ", n) + "x" }},
		{"userinfo-at", func(n int) string { return "http://" + strings.Repeat("a@", n/2) + "h/" }},
	}
	const n = 1 << 18
	for _, c := range cases {
		in1, in2 := c.make(n), c.make(2*n)
		timeIt := func(s string) time.Duration {
			best := time.Hour
			for range 3 {
				start := time.Now()
				_, _ = whatwgurl.Parse(s, nil)
				if d := time.Since(start); d < best {
					best = d
				}
			}
			return best
		}
		d1, d2 := timeIt(in1), timeIt(in2)
		ratio := float64(d2) / float64(max(d1, time.Microsecond))
		_, err := whatwgurl.Parse(in2, nil)
		t.Logf("%-20s len %7d: %10v, len %7d: %10v, ratio %.1f (err %v)", c.name, len(in1), d1, len(in2), d2, ratio, err != nil)
		if d2 > 200*time.Millisecond && ratio > 3 {
			t.Errorf("%s: quadratic: %v at %d bytes", c.name, d2, len(in2))
		}
	}
}

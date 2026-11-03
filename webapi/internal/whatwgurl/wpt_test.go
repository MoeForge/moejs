package whatwgurl_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/webapi/internal/whatwgurl"
)

// newOptions returns an Options whose NFC hook normalizes through a moejs
// runtime, as the JavaScript binding does.
func newOptions(t testing.TB) *whatwgurl.Options {
	rt := moejs.NewRuntime(moejs.Options{})
	return &whatwgurl.Options{NFC: func(s string) string {
		lit, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := moejs.CompileScript("nfc", string(lit)+`.normalize("NFC")`)
		if err != nil {
			t.Fatal(err)
		}
		v, err := rt.RunScript(sc)
		if err != nil {
			t.Fatal(err)
		}
		g, err := rt.ToGo(v)
		if err != nil {
			t.Fatal(err)
		}
		return g.(string)
	}}
}

// readJSON decodes testdata/name, gunzipping a .gz file.
func readJSON(t testing.TB, name string, v any) {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(name, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if b, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// getters returns the URL API's getters of u by name.
func getters(u *whatwgurl.URL) map[string]string {
	return map[string]string{
		"href": u.Href(), "origin": u.Origin(), "protocol": u.Protocol(), "username": u.Username(),
		"password": u.Password(), "host": u.Host(), "hostname": u.Hostname(), "port": u.Port(),
		"pathname": u.Pathname(), "search": u.Search(), "hash": u.Hash(),
	}
}

var getterNames = []string{"href", "origin", "protocol", "username", "password", "host", "hostname", "port", "pathname", "search", "hash"}

// tally counts the cases of a file and checks them against its known
// failures: a listed case must fail and an unlisted one pass.
type tally struct {
	t           *testing.T
	file        string
	known       map[string]string
	pass, total int
	seen        map[string]bool
}

func newTally(t *testing.T, file string, known map[string]string) *tally {
	return &tally{t: t, file: file, known: known, seen: map[string]bool{}}
}

func (ta *tally) check(key string, problem string) {
	ta.t.Helper()
	ta.total++
	_, listed := ta.known[key]
	if listed {
		ta.seen[key] = true
	}
	switch {
	case problem == "" && listed:
		ta.t.Errorf("%s: %q passes now; remove it from the known failures", ta.file, key)
	case problem == "":
		ta.pass++
	case !listed:
		ta.t.Errorf("%s: %q: %s", ta.file, key, problem)
	}
}

func (ta *tally) done() {
	ta.t.Helper()
	for k := range ta.known {
		if !ta.seen[k] {
			ta.t.Errorf("%s: known failure %q is no case", ta.file, k)
		}
	}
	ta.t.Logf("%s: %d of %d cases pass (%.1f %%), %d known failures", ta.file, ta.pass, ta.total, 100*float64(ta.pass)/float64(ta.total), len(ta.known))
}

// urlTestDataKnown lists the urltestdata.json cases that fail, by input
// and base, with the reason.
var urlTestDataKnown = map[string]string{}

func TestURLTestData(t *testing.T) {
	var cases []json.RawMessage
	readJSON(t, "urltestdata.json", &cases)
	o := newOptions(t)
	ta := newTally(t, "urltestdata.json", urlTestDataKnown)
	for _, raw := range cases {
		var c struct {
			Input   string
			Base    *string
			Failure bool
		}
		if json.Unmarshal(raw, &c) != nil {
			continue // a comment
		}
		var want map[string]any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		key := c.Input
		if c.Base != nil {
			key += " against " + *c.Base
		}
		ta.check(key, checkURLTest(o, c.Input, c.Base, c.Failure, want))
	}
	ta.done()
}

func checkURLTest(o *whatwgurl.Options, input string, base *string, failure bool, want map[string]any) string {
	var b *whatwgurl.URL
	if base != nil {
		var err error
		if b, err = o.Parse(*base, nil); err != nil {
			if failure {
				return ""
			}
			return fmt.Sprintf("base %q fails", *base)
		}
	}
	u, err := o.Parse(input, b)
	if failure {
		if err == nil {
			return fmt.Sprintf("parses to %q, want failure", u.Href())
		}
		return ""
	}
	if err != nil {
		return "fails"
	}
	got := getters(u)
	var bad []string
	for _, name := range getterNames {
		w, ok := want[name].(string)
		if ok && got[name] != w {
			bad = append(bad, fmt.Sprintf("%s %q, want %q", name, got[name], w))
		}
	}
	return strings.Join(bad, "; ")
}

// settersKnown lists the setters_tests.json cases that fail, by setter,
// href and new value, with the reason.
var settersKnown = map[string]string{}

func TestSetters(t *testing.T) {
	var all map[string]json.RawMessage
	readJSON(t, "setters_tests.json", &all)
	o := newOptions(t)
	ta := newTally(t, "setters_tests.json", settersKnown)
	for _, setter := range []string{"protocol", "username", "password", "host", "hostname", "port", "pathname", "search", "hash", "href"} {
		var cases []struct {
			Href     string
			NewValue string `json:"new_value"`
			Expected map[string]string
		}
		if err := json.Unmarshal(all[setter], &cases); err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			key := setter + " " + c.Href + " " + c.NewValue
			ta.check(key, checkSetter(o, setter, c.Href, c.NewValue, c.Expected))
		}
	}
	ta.done()
}

// set runs the setter by name; the href setter parses again.
func set(o *whatwgurl.Options, u *whatwgurl.URL, setter, v string) (*whatwgurl.URL, error) {
	switch setter {
	case "protocol":
		u.SetProtocol(v)
	case "username":
		u.SetUsername(v)
	case "password":
		u.SetPassword(v)
	case "host":
		u.SetHost(v)
	case "hostname":
		u.SetHostname(v)
	case "port":
		u.SetPort(v)
	case "pathname":
		u.SetPathname(v)
	case "search":
		u.SetSearch(v)
	case "hash":
		u.SetHash(v)
	case "href":
		return o.Parse(v, nil)
	default:
		panic(setter)
	}
	return u, nil
}

func checkSetter(o *whatwgurl.Options, setter, href, v string, want map[string]string) string {
	u, err := o.Parse(href, nil)
	if err != nil {
		return "href fails"
	}
	if u, err = set(o, u, setter, v); err != nil {
		return "the href setter fails"
	}
	got := getters(u)
	var bad []string
	for _, name := range getterNames {
		if w, ok := want[name]; ok && got[name] != w {
			bad = append(bad, fmt.Sprintf("%s %q, want %q", name, got[name], w))
		}
	}
	return strings.Join(bad, "; ")
}

// toASCIIKnown lists the toascii.json cases that fail, by input, with the
// reason.
var toASCIIKnown = map[string]string{}

// TestToASCII runs toascii.json as WPT's url/toascii.window.js does: through
// the URL constructor, the host setter and the hostname setter.
func TestToASCII(t *testing.T) {
	var cases []json.RawMessage
	readJSON(t, "toascii.json", &cases)
	o := newOptions(t)
	ta := newTally(t, "toascii.json", toASCIIKnown)
	for _, raw := range cases {
		var c struct {
			Input  string
			Output *string
		}
		if json.Unmarshal(raw, &c) != nil {
			continue // a comment
		}
		ta.check(c.Input, checkToASCII(o, c.Input, c.Output, true))
	}
	ta.done()
}

// idnaTestV2Known lists the IdnaTestV2.json and IdnaTestV2-removed.json
// cases that fail, by input, with the reason.
var idnaTestV2Known = map[string]string{
	// WPT generates IdnaTestV2.json from Unicode 18.0.0, which assigns
	// U+3D000..U+3FC3F (small seal characters); the table follows the
	// repository's pinned Unicode 17.0.0, where they are disallowed.
	"\u2d0d\U0003f8cd\ua868\u05ae.\u2d1e":             "U+3F8CD is unassigned in Unicode 17.0.0",
	"\u10ad\U0003f8cd\ua868\u05ae.\u10be":             "U+3F8CD is unassigned in Unicode 17.0.0",
	"\u10ad\U0003f8cd\ua868\u05ae.\u2d1e":             "U+3F8CD is unassigned in Unicode 17.0.0",
	"\U0003e8ac\u3002\u0729\u3002\ucbd95":             "U+3E8AC is unassigned in Unicode 17.0.0",
	"\U0003e8ac\u3002\u0729\u3002\u110d\u1173\u11ac5": "U+3E8AC is unassigned in Unicode 17.0.0",
	"\U0003e8ac.\u0729.\ucbd95":                       "U+3E8AC is unassigned in Unicode 17.0.0",
	"\U0003e8ac.\u0729.\u110d\u1173\u11ac5":           "U+3E8AC is unassigned in Unicode 17.0.0",
}

// TestIdnaTestV2 runs WPT's IdnaTestV2.json (gzipped; generated from the
// Unicode 17.0.0 IdnaTestV2.txt) and IdnaTestV2-removed.json as
// url/IdnaTestV2.any.js and url/IdnaTestV2-removed.any.js do: through the
// URL constructor.
func TestIdnaTestV2(t *testing.T) {
	o := newOptions(t)
	ta := newTally(t, "IdnaTestV2", idnaTestV2Known)
	for _, name := range []string{"IdnaTestV2.json.gz", "IdnaTestV2-removed.json"} {
		var cases []json.RawMessage
		readJSON(t, name, &cases)
		for _, raw := range cases {
			var c struct {
				Input  string
				Output *string
			}
			if json.Unmarshal(raw, &c) != nil || c.Input == "" {
				continue // a comment, or no host
			}
			ta.check(c.Input, checkToASCII(o, c.Input, c.Output, false))
		}
	}
	ta.done()
}

func checkToASCII(o *whatwgurl.Options, input string, output *string, setters bool) string {
	var bad []string
	u, err := o.Parse("https://"+input+"/x", nil)
	switch {
	case output == nil && err == nil:
		bad = append(bad, fmt.Sprintf("parses to %q, want failure", u.Href()))
	case output != nil && err != nil:
		bad = append(bad, "fails")
	case output != nil:
		if u.Host() != *output || u.Hostname() != *output || u.Pathname() != "/x" || u.Href() != "https://"+*output+"/x" {
			bad = append(bad, fmt.Sprintf("parses to %q", u.Href()))
		}
	}
	if !setters {
		return strings.Join(bad, "; ")
	}
	want := "x"
	if output != nil {
		want = *output
	}
	u, _ = o.Parse("https://x/x", nil)
	u.SetHost(input)
	if u.Host() != want {
		bad = append(bad, fmt.Sprintf("host setter: %q", u.Host()))
	}
	u, _ = o.Parse("https://x/x", nil)
	u.SetHostname(input)
	if u.Hostname() != want {
		bad = append(bad, fmt.Sprintf("hostname setter: %q", u.Hostname()))
	}
	return strings.Join(bad, "; ")
}

// TestWithoutNFC runs the WPT cases with the zero Options, which skip
// normalization, and logs how many of them that changes. A case with ASCII
// input must not change.
func TestWithoutNFC(t *testing.T) {
	type check struct {
		input string
		run   func(o *whatwgurl.Options) string
	}
	var checks []check
	var cases []json.RawMessage
	readJSON(t, "urltestdata.json", &cases)
	for _, raw := range cases {
		var c struct {
			Input   string
			Base    *string
			Failure bool
		}
		var want map[string]any
		if json.Unmarshal(raw, &c) == nil && json.Unmarshal(raw, &want) == nil {
			checks = append(checks, check{c.Input, func(o *whatwgurl.Options) string {
				return checkURLTest(o, c.Input, c.Base, c.Failure, want)
			}})
		}
	}
	for _, name := range []string{"toascii.json", "IdnaTestV2.json.gz", "IdnaTestV2-removed.json"} {
		cases = nil
		readJSON(t, name, &cases)
		for _, raw := range cases {
			var c struct {
				Input  string
				Output *string
			}
			if json.Unmarshal(raw, &c) == nil && c.Input != "" {
				checks = append(checks, check{c.Input, func(o *whatwgurl.Options) string {
					return checkToASCII(o, c.Input, c.Output, false)
				}})
			}
		}
	}
	o := newOptions(t)
	changed := 0
	for _, c := range checks {
		if c.run(o) == "" && c.run(nil) != "" {
			changed++
			if isASCII(c.input) {
				t.Errorf("%q needs NFC", c.input)
			}
		}
	}
	t.Logf("%d of %d cases need NFC", changed, len(checks))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

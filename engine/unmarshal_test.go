package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/require"
)

// evalValue runs src as a script in r and returns its completion value.
func evalValue(t *testing.T, r *Realm, src string) Value {
	t.Helper()
	s, err := syntax.ParseScript("u.js", src, syntax.Options{})
	require.NoError(t, err, src)
	code, err := compiler.CompileScript(s)
	require.NoError(t, err, src)
	v, err := r.RunScript(code)
	require.NoError(t, err, src)
	return v
}

// unmarshalRoundTrip is the reference: json.Unmarshal of AppendJSON's text.
func unmarshalRoundTrip(r *Realm, v Value, target any) error {
	data, ok, err := r.AppendJSON(nil, v)
	if err != nil {
		return err
	}
	if !ok {
		data = []byte("null")
	}
	return json.Unmarshal(data, target)
}

// unmarshalLikeRoot is the root API's Unmarshal: the walk, then the round
// trip when it did not complete.
func unmarshalLikeRoot(r *Realm, v Value, target any) (complete bool, err error) {
	ok, err := r.Unmarshal(v, target)
	if ok || err != nil {
		return ok, err
	}
	return false, unmarshalRoundTrip(r, v, target)
}

type umInner struct {
	X int     `json:"x"`
	Y *string `json:"y"`
}

type umTarget struct {
	ResponseType   string            `json:"responseType"`
	URL            string            `json:"url"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	Body           any               `json:"body"`
	Credentialless bool              `json:"credentialless"`
	Count          int8              `json:"count"`
	Ratio          float32           `json:"ratio"`
	Inner          *umInner          `json:"inner"`
	List           []umInner         `json:"list"`
	Pair           [2]int            `json:"pair"`
	Tags           []string          `json:"tags"`
	Num            json.Number       `json:"num"`
	Untagged       string
	Dash           string `json:"-"`
	DashComma      string `json:"-,"`
	hidden         string //nolint:unused
}

// umDup has two fields tagged "a" (neither wins), and a tagged "b" and a
// field B (the tagged one wins); built at run time, since vet rejects the
// repeated tag in source.
func umDup() reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "A1", Type: reflect.TypeFor[string](), Tag: `json:"a"`},
		{Name: "A2", Type: reflect.TypeFor[string](), Tag: `json:"a"`},
		{Name: "B1", Type: reflect.TypeFor[string](), Tag: `json:"b"`},
		{Name: "B", Type: reflect.TypeFor[string]()},
	})
}

type umEmbedded struct {
	umInner
	Z int
}

type umQuoted struct {
	N int `json:"n,string"`
}

type umKey string

type umUnicode struct {
	K  string // a key "\u212a" (Kelvin sign) folds to it
	SS string `json:"ſ"`
}

// TestUnmarshalMatchesRoundTrip compares the walk (and its fallback) with
// json.Unmarshal of AppendJSON's text for values into targets of every
// kind, starting from zero and from filled targets.
func TestUnmarshalMatchesRoundTrip(t *testing.T) {
	r := NewRealm()
	values := []string{
		`({responseType: "json", url: "https://x/y", method: "POST", headers: {a: "b", "Content-Type": "c"}, body: {x: [1, 2.5, {y: null}], s: "中\ud800", n: -0, big: 2 ** 70}, credentialless: true, extra: 1, count: 7, ratio: 0.1, inner: {x: 3, y: "s"}, list: [{x: 1}, {x: 2, y: null}], pair: [5, 6, 7], tags: ["a", "b"], num: 12.5, Untagged: "u", Dash: "d", "-": "dc"})`,
		`({URL: "upper", Url: "mixed", url: "last", METHOD: "m"})`,
		`({count: 300})`, `({count: 1.5})`, `({count: "7"})`, `({ratio: 1e40})`, `({num: "12"})`,
		`({inner: null, list: null, headers: null, body: null, pair: null, url: null})`,
		`({list: [], tags: [], pair: []})`,
		`({headers: {a: 1}})`, `({list: [1]})`, `({inner: [1]})`, `({body: undefined, url: undefined, f() {}, [Symbol()]: 1})`,
		`[1, 2, 3]`, `"str"`, `5`, `null`, `true`, `undefined`,
		`({a: "x", b: "y", B: "z"})`,
		`({x: 1, Z: 2})`, `({n: "5"})`,
		`({"\u212a": "kelvin", "S": "s", "ſ": "long s"})`,
		`({toJSON() { return {url: "from toJSON"}; }})`,
		`({url: new Date(0)})`,
		`({get url() { return "getter"; }})`,
		`new Proxy({url: "proxied"}, {})`,
		`({url: "x", body: new Map()})`,
		`({body: [1, , 3]})`,
		`(() => { const o = {}; let p = o; for (let i = 0; i < 80; i++) { p.next = {}; p = p.next; } return {body: o}; })()`,
		`({body: {"\ud800": 1, "\ud801": 2}})`,
		`({body: [undefined, function () {}, Symbol(), NaN, Infinity, -0]})`,
		`({count: 2 ** 53, body: 2 ** 62})`,
		`({count: 10n})`,
		`(() => { const o = {url: "a", image: undefined, method: "m", headers: {k: "v"}}; delete o.image; o.body = [1]; return o; })()`,
		`(() => { const o = {}; for (let i = 0; i < 100; i++) o["k" + i] = i; o.url = "many"; delete o.k3; return o; })()`,
		`(() => { const o = {url: "a", x: 1}; delete o.x; Object.defineProperty(o, "method", { get() { return "g"; }, enumerable: true }); return o; })()`,
		`(() => { const o = {url: "a", x: 1}; delete o.x; o.toJSON = () => ({url: "dict toJSON"}); return o; })()`,
	}
	type kind struct {
		name string
		make func() any
	}
	pre := func() *umTarget {
		y := "old"
		return &umTarget{URL: "keep", Headers: map[string]string{"old": "1"}, Body: map[string]any{"old": true}, Inner: &umInner{X: 9, Y: &y}, List: make([]umInner, 3, 5), Tags: []string{"z", "y", "x"}, Pair: [2]int{8, 9}}
	}
	kinds := []kind{
		{"struct", func() any { return new(umTarget) }},
		{"filled", func() any { return pre() }},
		{"ptr", func() any { p := new(*umTarget); return p }},
		{"any", func() any { return new(any) }},
		{"anyptr", func() any { a := any(&umInner{X: 1}); return &a }},
		{"map", func() any { return new(map[string]any) }},
		{"mapkey", func() any { return new(map[umKey]any) }},
		{"mapint", func() any { return new(map[int]any) }},
		{"slice", func() any { return new([]any) }},
		{"ints", func() any { return new([]int) }},
		{"string", func() any { return new(string) }},
		{"bytes", func() any { return new([]byte) }},
		{"int", func() any { return new(int) }},
		{"uint8", func() any { return new(uint8) }},
		{"float32", func() any { return new(float32) }},
		{"bool", func() any { return new(bool) }},
		{"dup", func() any { return reflect.New(umDup()).Interface() }},
		{"embedded", func() any { return new(umEmbedded) }},
		{"quoted", func() any { return new(umQuoted) }},
		{"unicode", func() any { return new(umUnicode) }},
		{"time", func() any { return new(struct{ URL time.Time }) }},
		{"raw", func() any { return new(struct{ Body json.RawMessage }) }},
		{"iface", func() any { return new(struct{ Body fmt.Stringer }) }},
	}
	completed := map[string]bool{}
	for _, src := range values {
		v := evalValue(t, r, src)
		for _, k := range kinds {
			want, got := k.make(), k.make()
			werr := unmarshalRoundTrip(r, v, want)
			complete, gerr := unmarshalLikeRoot(r, v, got)
			if complete {
				completed[src+" into "+k.name] = true
			}
			if werr != nil {
				require.Error(t, gerr, "%s into %s", src, k.name)
				require.Equal(t, werr.Error(), gerr.Error(), "%s into %s", src, k.name)
			} else {
				require.NoError(t, gerr, "%s into %s", src, k.name)
			}
			require.True(t, reflect.DeepEqual(want, got), "%s into %s (complete %v):\nwant %#v\ngot  %#v", src, k.name, complete, want, got)
		}
	}
	for _, k := range []string{"struct", "filled", "ptr", "any", "map", "mapkey"} {
		require.True(t, completed[values[0]+" into "+k], k)
	}
	require.True(t, completed[values[1]+" into struct"])
	require.True(t, completed[values[len(values)-4]+" into struct"]) // dictionary mode
	require.True(t, completed[values[len(values)-3]+" into map"])
	require.True(t, completed[values[7]+" into filled"])
}

// TestUnmarshalTags checks struct tags that the implementations of
// encoding/json read differently: Go 1.27's takes names the older one
// rejects and gives meaning to options it ignores, as Go 1.25 and 1.26 do
// with GOEXPERIMENT=jsonv2. The walk leaves a struct with a name the older
// one rejects or an option one of them reads to the round trip, for
// Unmarshal and ToGoInto, so the result is the round trip's under each;
// the tags all read alike complete.
func TestUnmarshalTags(t *testing.T) {
	r := NewRealm()
	str, num := reflect.TypeFor[string](), reflect.TypeFor[int]()
	for _, c := range []struct {
		typ      reflect.Type
		tag      string
		src      string
		complete bool
	}{
		// Names the older rules reject, which name the field by its Go
		// name: Go 1.27 takes them whole (invalid UTF-8 as U+FFFD) or,
		// with a backslash or a quote, the Go identifier they start with.
		{str, `json:"€"`, `({"€": "n", F: "f"})`, false},
		{str, `json:"a€"`, `({"a€": "n", F: "f"})`, false},
		{str, `json:"Ⅻ"`, `({"Ⅻ": "n", F: "f"})`, false},
		{str, `json:"a²"`, `({"a²": "n", F: "f"})`, false},
		{str, "json:\"a\u200db\"", `({"a\u200db": "n", F: "f"})`, false},
		{str, `json:"\xff"`, `({"\ufffd": "n", F: "f"})`, false},
		{str, `json:"a\\b"`, `({a: "n", F: "f", "a\\b": "b"})`, false},
		{str, `json:"d-e\\f"`, `({d: "n", F: "f"})`, false},
		{str, `json:"_\\x"`, `({_: "n", F: "f"})`, false},
		{str, `json:"a'b"`, `({a: "n", F: "f"})`, false},
		{str, `json:"a\"b"`, `({a: "n", F: "f"})`, false},
		{str, "json:\"a`b\"", `({a: "n", F: "f"})`, false},
		// Options one of them reads: Go 1.27 (a format option is an error
		// there), Go 1.25 and 1.26 with GOEXPERIMENT=jsonv2 (inline,
		// unknown, a quoted option) or all (",string", which Go 1.27 also
		// reads with bytes after it).
		{str, `json:"s,case:strict"`, `({S: "upper"})`, false},
		{reflect.TypeFor[umInner](), `json:",embed"`, `({x: 1, F: {x: 2}})`, false},
		{reflect.TypeFor[map[string]any](), `json:",embed"`, `({x: 1, F: {x: 2}})`, false},
		{reflect.TypeFor[time.Duration](), `json:"d,format:sec"`, `({d: 2})`, false},
		{reflect.TypeFor[time.Duration](), `json:"d,format:nano"`, `({d: 2})`, false},
		{reflect.TypeFor[[]byte](), `json:"b,format:hex"`, `({b: [1, 2]})`, false},
		{num, `json:"n,format:x"`, `({n: 1})`, false},
		{reflect.TypeFor[umInner](), `json:",inline"`, `({x: 1, F: {x: 2}})`, false},
		{reflect.TypeFor[map[string]any](), `json:",unknown"`, `({x: 1, F: {x: 2}})`, false},
		{num, `json:"n,'string'"`, `({n: 5})`, false},
		{num, `json:"n,string\\x"`, `({n: 5})`, false},
		{num, `json:"n,string "`, `({n: 5})`, false},
		{num, `json:"n,string"`, `({n: "5"})`, false},
		// Tags all read alike: options each ignores in Unmarshal,
		// case:ignore among them.
		{str, `json:"f"`, `({f: "x"})`, true},
		{str, `json:"f,omitempty"`, `({f: "x"})`, true},
		{str, `json:"f,omitzero"`, `({f: "x"})`, true},
		{str, `json:"f,"`, `({f: "x"})`, true},
		{str, `json:",omitempty"`, `({F: "x"})`, true},
		{str, `json:"f,required"`, `({f: "x"})`, true},
		{str, `json:"f,omitempty,nullable"`, `({f: "x"})`, true},
		{str, `json:"f,omitEmpty"`, `({f: "x"})`, true},
		{num, `json:"n,String"`, `({n: 5})`, true},
		{num, `json:"n,string1"`, `({n: 5})`, true},
		{str, `json:"s,case:ignore"`, `({S: "upper"})`, true},
		{str, `json:"k,case:ignore"`, `({"\u212a": "kelvin"})`, true},
		{str, `json:"-"`, `({"-": "x", F: "y"})`, true},
		{str, `json:"-,"`, `({"-": "x"})`, true},
		{str, ``, `({f: "x"})`, true},
		{str, `json:"a-b.c"`, `({"A-B.C": "x"})`, true},
		{str, `json:"ünï"`, `({"ÜNÏ": "x"})`, true},
	} {
		st := reflect.StructOf([]reflect.StructField{{Name: "F", Type: c.typ, Tag: reflect.StructTag(c.tag)}})
		v := evalValue(t, r, c.src)
		want, got := reflect.New(st).Interface(), reflect.New(st).Interface()
		werr := unmarshalRoundTrip(r, v, want)
		complete, gerr := unmarshalLikeRoot(r, v, got)
		require.Equal(t, errText(werr), errText(gerr), "%s: Unmarshal", c.tag)
		require.Equal(t, want, got, "%s: Unmarshal", c.tag)
		require.Equal(t, c.complete, complete, "%s: Unmarshal", c.tag)
		want, got = reflect.New(st).Interface(), reflect.New(st).Interface()
		_, werr = togoRoundTrip(r, v, want)
		complete, gerr = togoLikeRoot(r, v, got)
		require.Equal(t, errText(werr), errText(gerr), "%s: ToGoInto", c.tag)
		require.Equal(t, want, got, "%s: ToGoInto", c.tag)
		require.Equal(t, c.complete, complete, "%s: ToGoInto", c.tag)
	}
}

// TestUnmarshalCompletes checks that the walk handles the common cases
// itself (no text), including host placeholders in an interface.
func TestUnmarshalCompletes(t *testing.T) {
	r := NewRealm()
	v := evalValue(t, r, `({url: "u", method: "POST", headers: {"Content-Type": "application/json"}, body: {model: "m", n: 1, list: [1, "a", null, true]}})`)
	var d umTarget
	ok, err := r.Unmarshal(v, &d)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "u", d.URL)
	require.Equal(t, map[string]any{"model": "m", "n": 1.0, "list": []any{1.0, "a", nil, true}}, d.Body)

	payload := map[string]any{"messages": []any{map[string]any{"role": "user", "n": 1, "big": int64(1) << 60}}, "s": map[string]string{"a": "b"}, "l": map[string][]string{"x": {"1"}}, "e": []string{}, "nm": map[string]any(nil)}
	p, err := r.FromGo(payload)
	require.NoError(t, err)
	o := evalValue(t, r, `({})`).AsObject()
	require.NoError(t, o.SetProp(r, r.KeyFromGoString("body"), p))
	var got, want umTarget
	ok, err = r.Unmarshal(ObjectValue(o), &got)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, isPlaceholder(p))
	require.NoError(t, unmarshalRoundTrip(r, ObjectValue(o), &want))
	require.Equal(t, want, got)
	// The copy is new: changing it leaves the argument alone.
	got.Body.(map[string]any)["messages"].([]any)[0].(map[string]any)["role"] = "changed"
	require.Equal(t, "user", payload["messages"].([]any)[0].(map[string]any)["role"])
}

// TestUnmarshalHostValues compares host placeholders of every Go type with
// the round trip, into an interface (the walk) and a struct (the fallback).
func TestUnmarshalHostValues(t *testing.T) {
	r := NewRealm()
	for _, g := range []any{
		map[string]any{"i": 1, "f": 1.5, "neg0": math.Copysign(0, -1), "nan": math.NaN(), "u64": uint64(math.MaxUint64), "f32": float32(0.1), "s": "x", "b": false, "n": nil},
		map[string]any{"bad": "a\xffb"},
		map[string]any{"a\xff": 1},
		map[string]any{"fn": NativeFunc(func(*Realm, Value, []Value) (Value, error) { return Undefined(), nil })},
		[]any{[]string{"a"}, []map[string]any{{"x": 1}, nil}, map[string]string{"k": "v"}, map[string][]string{"k": nil}},
		map[string]any{"url": "u", "inner": map[string]any{"x": 2}},
	} {
		for _, mk := range []func() any{func() any { return new(any) }, func() any { return new(umTarget) }} {
			v, err := r.FromGo(g)
			require.NoError(t, err)
			want, got := mk(), mk()
			werr := unmarshalRoundTrip(r, v, want)
			_, gerr := unmarshalLikeRoot(r, v, got)
			require.Equal(t, werr == nil, gerr == nil, "%#v", g)
			require.True(t, reflect.DeepEqual(want, got), "%#v:\nwant %#v\ngot  %#v", g, want, got)
		}
	}
}

func TestUnmarshalInvalidTargets(t *testing.T) {
	r := NewRealm()
	v := evalValue(t, r, `({a: 1})`)
	var m map[string]any
	for _, target := range []any{nil, m, (*map[string]any)(nil), umTarget{}} {
		ok, err := r.Unmarshal(v, target)
		require.NoError(t, err)
		require.False(t, ok)
	}
	require.True(t, strings.HasPrefix(fmt.Sprint(unmarshalRoundTrip(r, v, nil)), "json: Unmarshal(nil)"))
}

// umDAG is {Known: 1, big: v} with v the n-deep {a: v, b: v} over leaf:
// n+2 objects whose text repeats leaf 2^n times.
func umDAG(t *testing.T, r *Realm, n int, leaf string) Value {
	return evalValue(t, r, fmt.Sprintf(`(() => { let v = %s; for (let i = 0; i < %d; i++) v = {a: v, b: v}; return {Known: 1, big: v}; })()`, leaf, n))
}

// umParsed is a string of n "A"s that ParseJSON's text gave, known plain.
func umParsed(t *testing.T, r *Realm, n int) Value {
	v, err := r.JSONParseGoString(`"` + strings.Repeat("A", n) + `"`)
	require.NoError(t, err)
	require.Equal(t, jsonPlain, v.AsString().json)
	return v
}

// TestUnmarshalOutputBound checks the string length limit: Unmarshal never
// completes where AppendJSON fails on it, falls back where an upper bound
// of the text passes it, and completes when the text is known to fit.
func TestUnmarshalOutputBound(t *testing.T) {
	lowerMaxStringLength(t, 1<<16)
	r := NewRealm()
	type known struct{ Known int }
	objects := []func() any{func() any { return &known{Known: 7} }, func() any { return new(any) }}
	arrays := []func() any{func() any { return &[]any{"old"} }, func() any { return new(any) }}
	for _, c := range []struct {
		name     string
		v        Value
		targets  []func() any
		complete bool // whether the bound fits the limit
		fails    bool // whether AppendJSON fails
	}{
		{"dag 8", umDAG(t, r, 8, `"x"`), objects, true, false},                     // text 3,591 bytes
		{"dag 12", umDAG(t, r, 12, `"x"`), objects, false, false},                  // text 57,351 bytes, bound over 64 KiB
		{"dag 14", umDAG(t, r, 14, `"x"`), objects, false, true},                   // text over 64 KiB
		{"dag 6 of 300", umDAG(t, r, 6, `"y".repeat(300)`), objects, false, false}, // text 20,039 bytes, bound over
		{"dag 8 of 300", umDAG(t, r, 8, `"y".repeat(300)`), objects, false, true},
		{"dag 4 of a parsed 300", umDAG(t, r, 4, `JSON.parse('"' + "y".repeat(300) + '"')`), objects, true, false},
		{"array root", evalValue(t, r, `(() => { let v = [1]; for (let i = 0; i < 8; i++) v = [v, v]; return v; })()`), arrays, true, false},
		{"array root, over", evalValue(t, r, `(() => { let v = [1]; for (let i = 0; i < 16; i++) v = [v, v]; return v; })()`), arrays, false, true},
		// A string known plain counts its length: the text of one of 65,534
		// characters is the limit exactly, one more passes it.
		{"parsed at the limit", umParsed(t, r, 65534), []func() any{func() any { return new(string) }}, true, false},
		{"parsed past the limit", umParsed(t, r, 65535), []func() any{func() any { return new(string) }}, false, true},
		// Another string counts six bytes a code unit.
		{"concatenated", evalValue(t, r, `"A".repeat(20000)`), []func() any{func() any { return new(string) }}, false, false},
		// Numbers of AppendNumber's longest text, 25 bytes: 2,600 of them
		// pass the limit by 2,066 bytes.
		{"longest numbers", evalValue(t, r, `Array.from({length: 2600}, () => -1.2345678901234567e-6)`), arrays, false, true},
	} {
		_, _, aerr := r.AppendJSON(nil, c.v)
		require.Equal(t, c.fails, aerr != nil, "%s: %v", c.name, aerr)
		for _, mk := range c.targets {
			want, got := mk(), mk()
			werr := unmarshalRoundTrip(r, c.v, want)
			complete, gerr := r.Unmarshal(c.v, got)
			require.NoError(t, gerr, c.name)
			require.Equal(t, c.complete, complete, c.name)
			if !complete {
				require.Equal(t, mk(), got, c.name) // nothing written
				_, gerr = unmarshalLikeRoot(r, c.v, got)
			}
			if werr != nil {
				require.EqualError(t, gerr, werr.Error(), c.name)
			} else {
				require.NoError(t, gerr, c.name)
			}
			require.Equal(t, want, got, c.name)
		}
	}
}

// TestUnmarshalHostStringBound checks the bound of long FromGo strings read
// by a script, which are scanned for the bytes JSON quoting escapes at
// their first use: whichever comes first, the bound of Unmarshal or
// ToGoInto or AppendJSON, a plain one counts its length, the text of one of
// 262,142 characters being the limit exactly, and one with an escape, a
// shorter one or a non-ASCII one counts six bytes a code unit, as when
// FromGo scanned them; the results are the round trip's.
func TestUnmarshalHostStringBound(t *testing.T) {
	const limit = 1 << 18
	lowerMaxStringLength(t, limit)
	r := NewRealm()
	plain := func(n int) string { return strings.Repeat("QUJD+/==", n/8+1)[:n] }
	with := func(n int, c string) string { g := plain(n - len(c)); return g[:n/2] + c + g[n/2:] }
	// value reads the strings from a FromGo argument, an array of them if
	// more than one: new Strings, which nothing has scanned.
	value := func(gs []string) Value {
		items := make([]Value, len(gs))
		for i, g := range gs {
			r.ReleaseCallData()
			o, err := r.FromGo(map[string]any{"s": g})
			require.NoError(t, err)
			items[i], err = o.AsObject().GetProp(r, r.KeyFromGoString("s"))
			require.NoError(t, err)
		}
		if len(items) == 1 {
			return items[0]
		}
		return ObjectValue(r.NewArray(items...))
	}
	for _, c := range []struct {
		name     string
		gs       []string
		complete bool // whether the bound fits the limit
		fails    bool // whether AppendJSON fails
	}{
		{"plain at the limit", []string{plain(limit - 2)}, true, false},
		{"plain past the limit", []string{plain(limit - 1)}, false, true},
		{"two plain at the limit", []string{plain(limit/2 - 4), plain(limit/2 - 4)}, true, false}, // text limit-1 bytes
		{"quote", []string{with(jsonPlainMin, `"`)}, false, false},
		{"newline at the end", []string{plain(jsonPlainMin-1) + "\n"}, false, false},
		{"plain and a backslash", []string{plain(jsonPlainMin), with(jsonPlainMin, `\`)}, false, false},
		{"short of the minimum", []string{plain(jsonPlainMin - 1)}, false, false},
		{"non-ASCII", []string{with(jsonPlainMin, "é")}, false, false},
	} {
		for _, first := range []string{"Unmarshal", "ToGoInto", "AppendJSON"} {
			name := c.name + ", " + first + " first"
			v := value(c.gs)
			_, _, aerr := r.AppendJSON(nil, v)
			if first != "AppendJSON" {
				v = value(c.gs)
			}
			got, want := new(any), new(any)
			var complete bool
			var gerr, werr error
			if first == "ToGoInto" {
				complete = r.ToGoInto(v, got)
				_, werr = togoRoundTrip(r, v, want)
				if !complete {
					require.Equal(t, new(any), got, name) // nothing written
					_, gerr = togoRoundTrip(r, v, got)
				}
			} else {
				complete, gerr = r.Unmarshal(v, got)
				require.NoError(t, gerr, name)
				werr = unmarshalRoundTrip(r, v, want)
				if !complete {
					require.Equal(t, new(any), got, name)
					gerr = unmarshalRoundTrip(r, v, got)
				}
			}
			require.Equal(t, c.complete, complete, name)
			require.Equal(t, c.fails, aerr != nil, "%s: %v", name, aerr)
			require.Equal(t, errText(werr), errText(gerr), name)
			require.Equal(t, want, got, name)
		}
	}
}

// TestJSONScalarMax checks jsonScalarMax against AppendNumber's longest
// texts: the forms the specification's Number::toString writes, with 17
// significant digits at every decimal exponent of a double, of both signs.
// The longest is 25 bytes, which jsonScalarMax must be.
func TestJSONScalarMax(t *testing.T) {
	longest := ""
	note := func(f float64) {
		var buf [32]byte
		if s := AppendNumber(buf[:0], f); len(s) > len(longest) {
			longest = string(s)
		}
	}
	for _, f := range []float64{0, math.Copysign(0, -1), 5e-324, 2.2250738585072014e-308, math.MaxFloat64, 1e21, 1e-7, 1 << 53, 123456789012345680000} {
		note(f)
		note(-f)
	}
	for e := -330; e <= 310; e++ {
		for _, m := range []string{"1.2345678901234567", "9.8765432109876543", "1.0000000000000002", "9.9999999999999989"} {
			f, err := strconv.ParseFloat(m+"e"+strconv.Itoa(e), 64)
			if err != nil || f == 0 {
				continue
			}
			note(f)
			note(-f)
		}
	}
	require.Equal(t, "-0.0000012345678901234567", longest)
	require.Equal(t, jsonScalarMax, len(longest))
}

// TestUnmarshalDAGBounded checks that plain's walk of a value repeating one
// object stops with the output bound, not after every path: a DAG 40 deep
// has 2^40 of them, AppendJSON stops at the limit, and so does Unmarshal.
// plain stops just past the limit; a walk of every path would not.
func TestUnmarshalDAGBounded(t *testing.T) {
	lowerMaxStringLength(t, 1<<16)
	r := NewRealm()
	stops := func(v Value, target any) {
		t.Helper()
		d := jsonDecoder{r: r, copyHosts: jsonTypeOf(reflect.TypeOf(target).Elem()).holdsAny}
		require.False(t, d.plain(v, 0))
		require.NoError(t, d.err)
		require.Greater(t, d.size, int64(maxStringLength))
		require.Less(t, d.size, 2*int64(maxStringLength))
	}
	v := umDAG(t, r, 40, `"x"`)
	for _, target := range []any{new(struct{ Known int }), new(any)} {
		stops(v, target)
		complete, err := r.Unmarshal(v, target)
		require.NoError(t, err)
		require.False(t, complete)
		_, err = unmarshalLikeRoot(r, v, target)
		require.EqualError(t, err, "RangeError: Invalid string length")
	}
	// The same through a host value: a Go map holding one map twice, 20
	// deep, whose text (15 MiB) passes the limit 230 times; a walk of every
	// path copies 2^20 maps (about 185 MiB), so a regression fails at once.
	var m any = "x"
	for range 20 {
		m = map[string]any{"a": m, "b": m}
	}
	p, err := r.FromGo(m)
	require.NoError(t, err)
	for _, target := range []any{new(struct{ Known int }), new(any)} {
		stops(p, target)
		complete, err := r.Unmarshal(p, target)
		require.NoError(t, err)
		require.False(t, complete)
		_, err = unmarshalLikeRoot(r, p, target)
		require.EqualError(t, err, "RangeError: Invalid string length")
	}
}

// TestUnmarshalPendingInterrupt checks that Unmarshal observes an interrupt
// pending at the call as AppendJSON does, every 4096 nodes: a small value
// completes, as AppendJSON writes it, and one of 5,000 nodes fails in plain,
// before anything is written, as AppendJSON fails on it.
func TestUnmarshalPendingInterrupt(t *testing.T) {
	r := NewRealm()
	small := evalValue(t, r, `({url: "u", n: 2})`)
	large := evalValue(t, r, `Array.from({length: 5000}, (_, i) => i)`)
	r.Interrupt("pending")
	t.Cleanup(r.ClearInterrupt)

	_, _, err := r.AppendJSON(nil, small)
	require.NoError(t, err)
	var d struct {
		URL string `json:"url"`
		N   int    `json:"n"`
	}
	complete, err := r.Unmarshal(small, &d)
	require.NoError(t, err)
	require.True(t, complete)
	require.Equal(t, "u", d.URL)
	require.Equal(t, 2, d.N)

	var ie *InterruptedError
	_, _, err = r.AppendJSON(nil, large)
	require.ErrorAs(t, err, &ie)
	list := []int{7}
	complete, err = r.Unmarshal(large, &list)
	require.False(t, complete)
	require.ErrorAs(t, err, &ie)
	require.Equal(t, []int{7}, list)
}

// TestUnmarshalInterruptInAny checks the interrupt in the walk into an
// empty interface: one pending at the call is not seen for an array of
// 3,000, each walk counting under 4096 nodes, as AppendJSON does not see
// it, and is seen in plain for an array of 5,000, the target untouched.
// One that arrives after plain stops the walk, the target untouched,
// counting an array's elements and an object's members, whose leaves the
// walk does not count one by one.
func TestUnmarshalInterruptInAny(t *testing.T) {
	r := NewRealm()
	small := evalValue(t, r, `Array.from({length: 3000}, (_, i) => i)`)
	large := evalValue(t, r, `Array.from({length: 5000}, (_, i) => i)`)
	r.Interrupt("pending")
	var out any = "untouched"
	complete, err := r.Unmarshal(small, &out)
	require.NoError(t, err)
	require.True(t, complete)
	require.Len(t, out, 3000)
	out = "untouched"
	complete, err = r.Unmarshal(large, &out)
	require.False(t, complete)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, "pending", ie.Value)
	require.Equal(t, "untouched", out)
	r.ClearInterrupt()

	for _, v := range []Value{large, evalValue(t, r, `Object.fromEntries(Array.from({length: 5000}, (_, i) => ["k" + i, i]))`)} {
		d := jsonDecoder{r: r, left: 4096, copyHosts: true}
		require.True(t, d.plain(v, 0))
		require.True(t, d.within(0))
		d.size, d.left = 0, 4096 // as Unmarshal
		r.Interrupt("late")
		require.False(t, d.value(v, reflect.ValueOf(&out).Elem(), 0))
		require.ErrorAs(t, d.err, &ie)
		require.Equal(t, "late", ie.Value)
		require.Equal(t, "untouched", out)
		r.ClearInterrupt()
	}
}

// TestUnmarshalHostInterrupt checks the walks of a host placeholder's Go
// value: a pending interrupt stops the check of a value the target
// discards (hostPlain) and the copy for an interface (hostCopy), whose
// target is untouched, and it stays pending.
func TestUnmarshalHostInterrupt(t *testing.T) {
	r := NewRealm()
	m := make(map[string]any, 20000)
	for i := range 20000 {
		m[fmt.Sprintf("k%05d", i)] = float64(i)
	}
	p, err := r.FromGo(m)
	require.NoError(t, err)
	o := evalValue(t, r, `({Known: 1})`).AsObject()
	require.NoError(t, o.SetProp(r, r.KeyFromGoString("big"), p))
	r.Interrupt("stop")
	t.Cleanup(r.ClearInterrupt)
	for _, target := range []any{
		&struct{ Known int }{Known: 7},
		&struct {
			Known int
			Big   any `json:"big"`
		}{Known: 7},
		new(any),
	} {
		before := reflect.ValueOf(target).Elem().Interface()
		complete, err := r.Unmarshal(ObjectValue(o), target)
		require.False(t, complete)
		var ie *InterruptedError
		require.ErrorAs(t, err, &ie, "%T", target)
		require.Equal(t, before, reflect.ValueOf(target).Elem().Interface())
	}
	require.Error(t, r.CheckInterrupt())
}

// TestUnmarshalHostStringsInterrupt checks the Go strings of placeholders
// in the interrupt budget: a string hostCopy checks counts a node per 4096
// bytes, so a pending interrupt is observed while copying 20 strings of 1
// MiB into an interface, from a map[string]any, a map[string]string or a
// []string, before anything is written, and not for five strings of 2 MiB
// (2,560 nodes), which AppendJSON writes too. A target that discards the
// placeholder completes: hostPlain scans no string, and AppendJSON, which
// writes a host string without a check inside it, completes too.
func TestUnmarshalHostStringsInterrupt(t *testing.T) {
	r := NewRealm()
	mib := strings.Repeat("x", 1<<20)
	anys, strs, list := map[string]any{}, map[string]string{}, make([]string, 20)
	for i := range list {
		k := fmt.Sprintf("k%02d", i)
		anys[k], strs[k], list[i] = mib, mib, mib
	}
	image := strings.Repeat("A", 2<<20)
	holder := func(g any) Value {
		p, err := r.FromGo(g)
		require.NoError(t, err)
		o := evalValue(t, r, `({Known: 1})`).AsObject()
		require.NoError(t, o.SetProp(r, r.KeyFromGoString("big"), p))
		return ObjectValue(o)
	}
	large := []Value{holder(anys), holder(strs), holder(list)}
	images := holder([]any{image, image, image, image, image})
	r.Interrupt("pending")
	t.Cleanup(r.ClearInterrupt)
	type withAny struct {
		Known int
		Big   any `json:"big"`
	}
	for i, v := range large {
		var a withAny
		complete, err := r.Unmarshal(v, &a)
		require.False(t, complete, i)
		var ie *InterruptedError
		require.ErrorAs(t, err, &ie, i)
		require.Equal(t, withAny{}, a, i)
		var k struct{ Known int }
		complete, err = r.Unmarshal(v, &k)
		require.NoError(t, err, i)
		require.True(t, complete, i)
		require.Equal(t, 1, k.Known, i)
		_, _, err = r.AppendJSON(nil, v)
		require.NoError(t, err, i)
	}
	var a withAny
	complete, err := r.Unmarshal(images, &a)
	require.NoError(t, err)
	require.True(t, complete)
	require.Len(t, a.Big, 5)
	_, _, err = r.AppendJSON(nil, images)
	require.NoError(t, err)
}

// TestUnmarshalHostDAG checks one placeholder referenced 2^k times, kept
// by plain's copies (an interface target) and checked by hostPlain (a
// struct that discards it): each reference counts, so the result is the
// round trip's, the RangeError included once the text passes the limit.
// A placeholder holding a 40,000-byte string, referenced once, has a text
// under the limit; referenced twice, over it.
func TestUnmarshalHostDAG(t *testing.T) {
	lowerMaxStringLength(t, 1<<16)
	r := NewRealm()
	p, err := r.FromGo(map[string]any{"k": "0123456789", "n": 1})
	require.NoError(t, err)
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("h"), p))
	long, err := r.FromGo(map[string]any{"s": strings.Repeat("Q", 40000)})
	require.NoError(t, err)
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("long"), long))
	var values []string
	for k := range 12 {
		values = append(values, fmt.Sprintf(`(() => { let v = h; for (let i = 0; i < %d; i++) v = {a: v, b: v}; return {Known: 1, big: v}; })()`, k))
	}
	values = append(values, `({Known: 1, big: long})`, `({Known: 1, big: {a: long, b: long}})`)
	for k, src := range values {
		v := evalValue(t, r, src)
		for _, mk := range []func() any{
			func() any { return new(struct{ Known int }) },
			func() any {
				return new(struct {
					Known int
					Big   any `json:"big"`
				})
			},
		} {
			want, got := mk(), mk()
			werr := unmarshalRoundTrip(r, v, want)
			_, gerr := unmarshalLikeRoot(r, v, got)
			if werr != nil {
				require.EqualError(t, gerr, werr.Error(), "k=%d", k)
			} else {
				require.NoError(t, gerr, "k=%d", k)
			}
			require.Equal(t, want, got, "k=%d", k)
		}
	}
}

// TestUnmarshalFloat32NonFinite checks that a float32 NaN or infinity in a
// host value reads back as null, as the round trip's text has it.
func TestUnmarshalFloat32NonFinite(t *testing.T) {
	r := NewRealm()
	for _, f := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), 1.5} {
		p, err := r.FromGo(map[string]any{"x": f, "list": []any{f}})
		require.NoError(t, err)
		o := evalValue(t, r, `({})`).AsObject()
		require.NoError(t, o.SetProp(r, r.KeyFromGoString("Body"), p))
		for _, mk := range []func() any{
			func() any { return new(any) },
			func() any { return new(struct{ Body any }) },
			func() any { return new(map[string]any) },
		} {
			want, got := mk(), mk()
			require.NoError(t, unmarshalRoundTrip(r, ObjectValue(o), want))
			complete, err := r.Unmarshal(ObjectValue(o), got)
			require.NoError(t, err)
			require.True(t, complete)
			require.Equal(t, want, got, "%v", f)
		}
	}
}

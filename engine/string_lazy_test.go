package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
	"unsafe"

	"github.com/Calcium-Ion/moejs/compiler"
	"github.com/Calcium-Ion/moejs/syntax"
	"github.com/stretchr/testify/require"
)

func TestLazyBridgeAndMaterialization(t *testing.T) {
	text := strings.Repeat("中😀a", 20000)
	s := FromHostString(text)
	require.True(t, s.isLazy())
	require.Nil(t, cacheOf(s))
	require.Equal(t, unsafe.StringData(text), unsafe.StringData(s.GoString()))
	require.Nil(t, cacheOf(s), "exporting valid UTF-8 shares the source")
	require.Equal(t, 80000, s.Len())
	require.Nil(t, cacheOf(s), "the length is known without decoding")
	require.Equal(t, uint16(0xd83d), s.At(1))
	p := cacheOf(s)
	require.Equal(t, uint16(0xde00), s.At(2))
	require.Same(t, p, cacheOf(s), "reuse cache")
	require.Equal(t, text, s.GoString())
}

func TestLazyInvalidAndSurrogates(t *testing.T) {
	for _, text := range []string{"x\xffy", "\xed\xa0\x80", "\xe2\x82", "�"} {
		got := FromHostString(text)
		want := FromGoString(text)
		require.Equal(t, want.UTF16(), got.UTF16())
		require.True(t, got.Equals(want))
		require.Equal(t, want.Hash(), got.Hash())
		require.Equal(t, want.GoString(), got.GoString())
	}
	s := FromHostString("😀")
	require.Equal(t, []uint16{0xd83d}, s.Substring(0, 1).UTF16())
	require.Equal(t, []uint16{0xde00}, s.Substring(1, 2).UTF16())
	require.Equal(t, "�", s.Substring(0, 1).GoString())
	require.False(t, s.Substring(0, 1).Equals(FromGoString("�")))
}

func TestLazyJSON(t *testing.T) {
	r := NewRealm()
	text := strings.Repeat("中😀", 2000)
	input := FromHostString(`"` + text + `"`)
	v, err := r.JSONParse(input)
	require.NoError(t, err)
	s := v.AsString()
	require.True(t, s.isLazy())
	require.Nil(t, cacheOf(input), "JSON.parse must read retained text directly")
	require.Nil(t, cacheOf(s))
	quoted, ok, err := r.AppendJSON(nil, v)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, `"`+text+`"`, string(quoted))
	require.Nil(t, cacheOf(s), "AppendJSON should preserve UTF-8")
	s.UTF16()
	p := cacheOf(s)
	s.UTF16()
	require.Same(t, p, cacheOf(s))
}

func TestLazyHostOwnership(t *testing.T) {
	r := NewRealm()
	text := strings.Repeat("中文😀", 2000)
	v, err := r.FromGo(text)
	require.NoError(t, err)
	s := v.AsString()
	require.True(t, s.isLazy())
	require.Nil(t, cacheOf(s))
	r.ReleaseCallData()
	for range 20 {
		_, err = r.FromGo(strings.Repeat("其他", 1000))
		require.NoError(t, err)
		r.ReleaseCallData()
	}
	runtime.GC()
	require.Equal(t, text, s.GoString())
	require.Nil(t, cacheOf(s))
	require.Equal(t, uint16(0xd83d), s.At(2))
	require.NotNil(t, cacheOf(s))
}

// TestLazyLimitedRealmChargesAtCreation: a parsed lazy string's decoded
// units are charged when it is made, as the eager conversion would be, and
// its length is known without decoding.
func TestLazyLimitedRealmChargesAtCreation(t *testing.T) {
	text := strings.Repeat("中", 32768)
	for _, limit := range []int64{1 << 40, 4096} {
		r := NewRealmWith(RealmOptions{MemoryLimit: limit})
		v, err := r.JSONParse(FromHostString(`"` + text + `"`))
		if limit == 4096 {
			requireLimitHit(t, r, v, err)
			continue
		}
		require.NoError(t, err)
		s := v.AsString()
		require.True(t, s.isLazy())
		require.GreaterOrEqual(t, r.Stats().RequestAllocatedBytes, int64(2*len([]rune(text))))
		before := r.Stats().RequestAllocatedBytes
		require.Equal(t, len([]rune(text)), s.Len())
		require.Nil(t, cacheOf(s), "the length is known without decoding")
		require.Equal(t, before, r.Stats().RequestAllocatedBytes)
		require.NoError(t, r.CheckInterrupt())
	}
}

// requireLimitHit checks that making v passed the memory limit: the call
// returned the hit, or a builtin made the charge and left the measurement
// to the interpreter, which hits while the host keeps v. Undecoded, v
// measures as decoded, as it was charged.
func requireLimitHit(t *testing.T, r *Realm, v Value, err error) {
	t.Helper()
	if err == nil {
		require.Nil(t, cacheOf(v.AsString()))
		remove := r.AddLiveRoots(func(add func(Value)) { add(v) })
		_, err = runScriptValue(t, r, `0`)
		remove()
	}
	require.ErrorIs(t, err, ErrMemoryLimit)
	require.Positive(t, r.Stats().MemoryLimitHits)
}

func TestLazyLimitedJSONChargesAtCreation(t *testing.T) {
	for _, size := range []int{1024, 200000, 600000} {
		r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
		text := strings.Repeat("中", size)
		v, err := r.JSONParseGoString(`"` + text + `"`)
		if 2*size > 1<<20 {
			requireLimitHit(t, r, v, err)
			continue
		}
		require.NoError(t, err)
		s := v.AsString()
		require.True(t, s.isLazy())
		require.GreaterOrEqual(t, r.Stats().RequestAllocatedBytes, int64(2*size))
		before := r.Stats().RequestAllocatedBytes
		require.Equal(t, size, s.Len())
		require.Nil(t, cacheOf(s))
		require.Equal(t, utf16.Encode([]rune(text)), s.UTF16())
		require.NotNil(t, cacheOf(s))
		require.Equal(t, before, r.Stats().RequestAllocatedBytes, "decoding charges nothing more")
		require.NoError(t, r.CheckInterrupt())
		require.Zero(t, r.Stats().MemoryLimitHits)
	}
}

// TestLazyParseChargeStopsNextCall: parsing charges each string as decoded
// when it makes it, and a builtin's charge past the limit leaves the
// measurement to the interpreter, so the call that follows hits the limit,
// although reading .length decodes nothing.
func TestLazyParseChargeStopsNextCall(t *testing.T) {
	const src = `
export function sum(xs) {
  let total = 0;
  for (let i = 0; i < xs.length; i++) total += xs[i].length;
  return total;
}
export function one(s) { return s.length; }
`
	const limit = int64(64 << 10)
	// A single decoded string must exceed the budget with exact capacity.
	one := `"` + strings.Repeat("中", 65536) + `"`
	many := "[" + strings.Repeat(one+",", 15) + one + "]"

	r := NewRealmWith(RealmOptions{MemoryLimit: limit})
	env := evalModuleOn(t, r, src)
	v, err := r.JSONParseGoString(many)
	require.NoError(t, err)
	fn, ok := env.GetBindingValue("sum")
	require.True(t, ok)
	_, err = r.Call(fn, Undefined(), []Value{v})
	require.ErrorIs(t, err, ErrMemoryLimit)
	require.Positive(t, r.Stats().MemoryLimitHits)

	r = NewRealmWith(RealmOptions{MemoryLimit: limit})
	env = evalModuleOn(t, r, src)
	v, err = r.JSONParseGoString(one)
	require.NoError(t, err)
	fn, ok = env.GetBindingValue("one")
	require.True(t, ok)
	_, err = r.Call(fn, Undefined(), []Value{v})
	require.ErrorIs(t, err, ErrMemoryLimit)
	require.Positive(t, r.Stats().MemoryLimitHits)

	r = NewRealmWith(RealmOptions{MemoryLimit: limit})
	env = evalModuleOn(t, r, src)
	v, err = r.JSONParseGoString(`["中","文"]`)
	require.NoError(t, err)
	fn, ok = env.GetBindingValue("sum")
	require.True(t, ok)
	got, err := r.Call(fn, Undefined(), []Value{v})
	require.NoError(t, err)
	require.Equal(t, float64(2), got.AsNumber())
	require.Zero(t, r.Stats().MemoryLimitHits)
}

func evalModuleOn(t *testing.T, r *Realm, src string) *ModuleEnv {
	t.Helper()
	m, err := syntax.ParseModule("t.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	return env
}

func TestLazyLimitedASCIISubstring(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
	s := FromHostString(strings.Repeat("a", jsonPlainMin+300))
	require.True(t, s.isLazy())
	out := r.substring(s, 0, 200)
	require.Equal(t, strings.Repeat("a", 200), out.GoString())
	require.NoError(t, r.CheckInterrupt())
	require.Equal(t, int64(stringHeaderSize), r.Stats().RequestAllocatedBytes, "a substring of ASCII text shares its bytes")
}

// cacheOf is the decoded result a lazy string has published, or nil. Tests
// read it once the goroutines that decode have finished.
func cacheOf(s *String) *String {
	return s.lazyPublished()
}

func TestLazyMoveDoesNotDecode(t *testing.T) {
	f := newHostLazyFixture(t, `export function move(req) {
 const s = req.content;
 return s && s !== "" ? {text:s} : {};
}`, RealmOptions{})
	r := f.r
	text := strings.Repeat("中文😀", 8192)
	v, err := r.JSONParseGoString(`{"content":"` + text + `"}`)
	require.NoError(t, err)
	content, err := v.AsObject().GetProp(r, key(r, "content"))
	require.NoError(t, err)
	s := content.AsString()
	require.Equal(t, int32(32768), s.n, "counted when made")
	require.Nil(t, cacheOf(s))
	require.True(t, ToBoolean(content))
	require.False(t, s.Equals(EmptyString()))
	require.Equal(t, text, s.GoString())
	require.True(t, s.Equals(FromHostString(text)))
	require.Nil(t, cacheOf(s))
	dst, err := r.Call(f.fn("move"), Undefined(), []Value{v})
	require.NoError(t, err)
	moved, err := dst.AsObject().GetProp(r, key(r, "text"))
	require.NoError(t, err)
	require.Same(t, s, moved.AsString(), "JS must move the string reference")
	out, ok, err := r.AppendJSON(nil, dst)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, `{"text":"`+text+`"}`, string(out))
	require.Nil(t, cacheOf(s), "assignment, truthiness and UTF-8 output must not decode")
	require.Equal(t, 32768, s.Len())
	require.Nil(t, cacheOf(s), "the length is known without decoding")
	require.Equal(t, int32(32768), s.resolved().n, "the decoding agrees with the count")
}

func TestLazyLengthOperations(t *testing.T) {
	r := NewRealm()
	for _, text := range []string{"中文😀a", "é", "\ue000😀"} {
		s := FromHostString(text)
		want := FromGoString(text)
		require.Equal(t, want.Substring(0, 1).UTF16(), s.Substring(0, 1).UTF16())
		require.True(t, s.Equals(want))
		require.Equal(t, want.Hash(), s.Hash())
		require.Equal(t, s.Hash(), s.Hash(), "cached")
		require.Equal(t, 0, s.Compare(want))
		require.Equal(t, want.IndexOf(FromGoString("😀"), 0), s.IndexOf(FromGoString("😀"), 0))
		joined, err := r.Concat(s, FromGoString("!"))
		require.NoError(t, err)
		require.Equal(t, text+"!", joined.GoString())
	}
	v, err := runScriptValue(t, r, `const a=JSON.parse('"中文😀a"'); [a.length,a.slice(2,3).charCodeAt(0),a.match(/./gu).length,a.indexOf("😀"),a.normalize("NFC")===a].join(",")`)
	require.NoError(t, err)
	require.Equal(t, "5,55357,4,2,true", v.AsString().GoString())
}

// goText is what GoString returns for a Go string converted to JS: each
// invalid UTF-8 byte becomes U+FFFD, as FromGoString replaces it.
func goText(text string) string { return string([]rune(text)) }

func TestLazyHostRoundTrip(t *testing.T) {
	f := newHostLazyFixture(t, `
export function move(s) { const box={a:s};return box.a; }
export function read(s) { return s.length; }
export function iterate(s) { let n=0;for(const c of s)n++;return n; }
export function append(s) { return s+"!"; }
`, RealmOptions{})
	for _, text := range []string{"", "ascii", strings.Repeat("x", jsonPlainMin), strings.Repeat("中文😀", 10000), "a\xffb", "\xed\xa0\x80", "a\x00b"} {
		s := FromHostString(text)
		want := goText(text)
		for range 10 {
			v, err := f.r.Call(f.fn("move"), Undefined(), []Value{StringValue(s)})
			require.NoError(t, err)
			require.Same(t, s, v.AsString())
			if s.isLazy() {
				require.Nil(t, cacheOf(s), "moving a string must not decode it")
			}
		}
		v, err := f.r.Call(f.fn("read"), Undefined(), []Value{StringValue(s)})
		require.NoError(t, err)
		require.Equal(t, float64(len(utf16.Encode([]rune(text)))), v.AsNumber())
		var decoded *String
		if s.isLazy() {
			require.Nil(t, cacheOf(s), "the length is known without decoding")
		}
		for range 3 {
			v, err = f.r.Call(f.fn("iterate"), Undefined(), []Value{StringValue(s)})
			require.NoError(t, err)
			require.Equal(t, float64(len([]rune(text))), v.AsNumber())
			if s.isLazy() {
				if decoded == nil {
					decoded = cacheOf(s)
					require.NotNil(t, decoded)
				}
				require.Same(t, decoded, cacheOf(s))
			}
		}
		require.Equal(t, want, s.GoString())
		if text != "" && want == text {
			require.Equal(t, unsafe.StringData(text), unsafe.StringData(s.GoString()), "valid UTF-8 is shared")
		}
		require.Equal(t, want, StringValue(s).String())
		require.Equal(t, want, f.r.ToGo(StringValue(s)))
		out, err := f.r.ToGoStrict(StringValue(s))
		require.NoError(t, err)
		require.Equal(t, want, out)
		var unmarshaled string
		complete, err := f.r.Unmarshal(StringValue(s), &unmarshaled)
		require.NoError(t, err)
		require.True(t, complete)
		require.Equal(t, want, unmarshaled, "ToGo and Unmarshal agree")
		v, err = f.r.Call(f.fn("append"), Undefined(), []Value{StringValue(s)})
		require.NoError(t, err)
		require.Equal(t, want+"!", v.String())
	}
}

func TestLazyEscapedJSON(t *testing.T) {
	f := newHostLazyFixture(t, `export function move(req){const s=req.content;return s && s!=="" ? {text:s}:{};}`, RealmOptions{})
	for _, tc := range []struct{ raw, text, canonical string }{
		{`\u0061`, "a", `"a"`}, {`\/`, "/", `"/"`},
		{`中\n文😀`, "中\n文😀", `"中\n文😀"`},
		{`\uD800`, "�", `"\ud800"`},
		{`\ud83d\ude00`, "😀", `"😀"`},
		{`a\\b\"c`, "a\\b\"c", `"a\\b\"c"`},
	} {
		v, err := f.r.JSONParseGoString(`{"content":"` + tc.raw + `"}`)
		require.NoError(t, err)
		x, err := v.AsObject().GetProp(f.r, key(f.r, "content"))
		require.NoError(t, err)
		s := x.AsString()
		require.True(t, s.isLazy())
		require.Nil(t, cacheOf(s))
		moved, err := f.r.Call(f.fn("move"), Undefined(), []Value{v})
		require.NoError(t, err)
		y, err := moved.AsObject().GetProp(f.r, key(f.r, "text"))
		require.NoError(t, err)
		require.Same(t, s, y.AsString())
		require.Nil(t, cacheOf(s), "field movement must not decode escapes")
		out, ok, err := f.r.AppendJSON(nil, x)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, tc.canonical, string(out))
		if s.lazyMode() == lazyJSONVerbatim {
			require.Nil(t, cacheOf(s), "canonical literal should copy without decoding")
		}
		require.Equal(t, tc.text, s.GoString())
		decoded := cacheOf(s)
		require.NotNil(t, decoded)
		out, _, err = f.r.AppendJSON(nil, x)
		require.NoError(t, err)
		require.Equal(t, tc.canonical, string(out))
		require.Same(t, decoded, cacheOf(s))
	}
}

func TestLazyJSONSyntaxBeforeAccess(t *testing.T) {
	r := NewRealm()
	for _, src := range []string{`{"unused":"\q"}`, `{"unused":"\uZ000"}`, `{"unused":"\u12"}`, `{"unused":"x}`, `{"unused":"ok"} trailing`, "{\"unused\":\"a\nb\"}"} {
		_, err := r.JSONParseGoString(src)
		require.Error(t, err, src)
	}
	v, err := r.JSONParseGoString(`{"a":1,"\u0061":2}`)
	require.NoError(t, err)
	x, err := v.AsObject().GetProp(r, key(r, "a"))
	require.NoError(t, err)
	require.Equal(t, float64(2), x.AsNumber())
}

func TestLazyASCIIRepresentation(t *testing.T) {
	f := newHostLazyFixture(t, `export function check(s){const o={};o[s]=7;const m=new Map([[s,9]]);return [o.a,m.get("a"),new Set([s]).has("a"),"cat".indexOf(s),"cat".lastIndexOf(s),"cat".includes(s),"aa".split(s).length,s==="a",s.localeCompare("a")];}`, RealmOptions{})
	v, err := f.r.Call(f.fn("check"), Undefined(), []Value{StringValue(FromHostString("a"))})
	require.NoError(t, err)
	out, _, err := f.r.AppendJSON(nil, v)
	require.NoError(t, err)
	require.Equal(t, `[7,9,true,1,1,true,3,true,0]`, string(out))
	r := f.r
	require.False(t, FromHostString("a").isLazy(), "short ASCII converts eagerly")
	require.True(t, r.KeyFromString(FromHostString("0")).IsIndex())
	long := FromHostString(strings.Repeat("a", jsonPlainMin))
	require.True(t, long.isLazy())
	require.True(t, r.KeyFromString(long).IsString())
	joined, err := r.Concat(long, FromHostString("b"))
	require.NoError(t, err)
	require.True(t, joined.IsASCII())
	require.Equal(t, strings.Repeat("a", jsonPlainMin)+"b", joined.GoString())
	a, b := FromHostString("\xff"), FromHostString("\xfe")
	require.True(t, a.Equals(b))
	require.Equal(t, a.Hash(), b.Hash())
}

func TestLazyInvalidHostBytesExportReplaced(t *testing.T) {
	s := FromHostString("a\xffb")
	r := NewRealm()
	require.Equal(t, "a�b", s.GoString())
	require.Equal(t, "a�b", StringValue(s).String())
	require.Equal(t, "a�b", r.ToGo(StringValue(s)))
	b, _, err := r.AppendJSON(nil, StringValue(s))
	require.NoError(t, err)
	require.Equal(t, `"a�b"`, string(b))
	parsed, err := r.JSONParse(FromHostString("{\"x\":\"a\xffb\"}"))
	require.NoError(t, err)
	out, _, err := r.AppendJSON(nil, parsed)
	require.NoError(t, err)
	require.True(t, json.Valid(out))
	require.Equal(t, `{"x":"a�b"}`, string(out))
	require.Equal(t, `"a�b"`, r.DisplayString(StringValue(s)))
	require.Equal(t, "Symbol(a�b)", NewSymbol(s).descriptiveString())
}

func TestLazyImportUsesDecodedSpecifier(t *testing.T) {
	h := newImportHost(t, map[string]string{
		"main": `import(specifier).then(ns => out(ns.value));`,
		"a�b":  `export const value = 42;`,
	})
	r := h.realm(h.hooks())
	s := FromHostString("a\xffb")
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("specifier"), StringValue(s)))
	_, err := h.evaluate(r, "main")
	require.NoError(t, err)
	require.Equal(t, []string{"main>a�b"}, h.loads)
	require.Equal(t, []string{"42"}, h.out)
}

func TestLazyNativeFunctionNames(t *testing.T) {
	r := NewRealm()
	for _, name := range []string{"", "plain", "中文", "get ä1", "set $'", "a\xffb", "get a\xffb", "set \xed\xa0\x80", "[\xff]", "get [\xff]", "a\x00b", "\xe2\x82"} {
		format := func(s *String) string {
			t.Helper()
			fn := ObjectValue(r.NewNativeFunction(s, 0, nil))
			v, err := functionProtoToString(r, fn, nil)
			require.NoError(t, err)
			return v.AsString().GoString()
		}
		require.Equal(t, format(FromGoString(name)), format(FromHostString(name)), "%q", name)
	}
}

func TestLazyHostContainerExportDoesNotDecode(t *testing.T) {
	text := strings.Repeat("中文😀", 8192)
	for _, replace := range []bool{false, true} {
		r := NewRealm()
		v, err := r.FromGo(map[string]any{"text": text})
		require.NoError(t, err)
		field, err := v.AsObject().GetProp(r, r.KeyFromGoString("text"))
		require.NoError(t, err)
		s := field.AsString()
		if replace {
			s = FromHostString(text)
			require.NoError(t, v.AsObject().SetProp(r, r.KeyFromGoString("text"), StringValue(s)))
		}
		require.Nil(t, cacheOf(s))
		out := r.ToGo(v).(map[string]any)
		require.Equal(t, text, out["text"])
		require.Equal(t, unsafe.StringData(text), unsafe.StringData(out["text"].(string)))
		require.Nil(t, cacheOf(s), "exporting a valid UTF-8 Host string must not decode it")
	}
}

func TestLazyHostContainerInvalidBytes(t *testing.T) {
	r := NewRealm()
	v, err := r.FromGo(map[string]any{"text": "a�b"})
	require.NoError(t, err)
	s := FromHostString("a\xffb")
	require.NoError(t, v.AsObject().SetProp(r, r.KeyFromGoString("text"), StringValue(s)))
	require.Equal(t, "a�b", r.ToGo(v).(map[string]any)["text"])
}

func TestLazyExceptionsUseDecodedContent(t *testing.T) {
	r := NewRealm()
	s := FromHostString("a\xffb")
	for _, value := range []Value{StringValue(s), r.NewErrorValue(KindError, s)} {
		ex := &Exception{Value: value}
		require.Equal(t, "a�b", ex.Message())
		want := "a�b"
		if value.IsObject() {
			want = "Error: " + want
		}
		require.Equal(t, want, ex.Error())
	}
}

func TestLazyOneResultConcurrent(t *testing.T) {
	s := FromHostString(strings.Repeat("中😀", 2048))
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan *String, 24)
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			require.Equal(t, 6144, s.Len())
			require.Equal(t, uint16(0xd83d), s.At(1))
			results <- s.resolved()
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for result := range results {
		require.Same(t, cacheOf(s), result)
	}
}

func TestLazyJSONReusesInputValidation(t *testing.T) {
	r := NewRealm()
	s := FromHostString(`{"x":"中文"}`)
	// Counting its units when it was made checked its UTF-8.
	require.NotZero(t, s.lazyData().facts.Load()&factUTF8Valid)
	for range 10 {
		_, err := r.JSONParse(s)
		require.NoError(t, err)
		require.NotZero(t, s.lazyData().facts.Load()&factUTF8Valid)
		require.Nil(t, cacheOf(s))
	}
}

func TestLazyConcurrentChecksAndDecode(t *testing.T) {
	for _, tc := range []struct {
		name, text   string
		valid, plain bool
	}{
		{"ascii", strings.Repeat("a", jsonPlainMin), true, true},
		{"quoted", strings.Repeat("a\"", jsonPlainMin/2), true, false},
		{"unicode", strings.Repeat("中😀", jsonPlainMin/4), true, false},
		{"invalid", strings.Repeat("a\xff", jsonPlainMin/2), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := FromHostString(tc.text)
			want := utf16.Encode([]rune(tc.text))
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range 24 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for range 10 {
						switch i % 4 {
						case 0:
							if s.Len() != len(want) || s.At(len(want)-1) != want[len(want)-1] {
								t.Error("decoded content changed")
							}
						case 1:
							if s.validLazyUTF8() != tc.valid {
								t.Error("UTF-8 check changed")
							}
						case 2:
							if s.plainLazyJSON() != tc.plain {
								t.Error("JSON check changed")
							}
						case 3:
							if s.Hash() != FromGoString(tc.text).Hash() {
								t.Error("hash changed")
							}
						}
					}
				}()
			}
			close(start)
			wg.Wait()
			require.NotNil(t, cacheOf(s))
			require.Equal(t, goText(tc.text), s.GoString())
		})
	}
}

func TestLazyJSONStaysLazyUnderMemoryLimit(t *testing.T) {
	for _, src := range []string{
		`"\"\\\/\b\f\n\r\t\u0000"`, `"\uD800x\uDC00"`,
		`"\ud83d\ude00中文"`, `"prefix\n` + strings.Repeat("中😀", 4096) + `\tend"`,
	} {
		var expected []uint16
		for _, limit := range []int64{1 << 40, 0} {
			r := NewRealmWith(RealmOptions{MemoryLimit: limit})
			v, err := r.JSONParseGoString(src)
			require.NoError(t, err)
			s := v.AsString()
			require.True(t, s.isLazy())
			require.Nil(t, cacheOf(s))
			require.Equal(t, limit != 0, s.rootedData() != nil)
			got := s.UTF16()
			if expected == nil {
				expected = got
			}
			require.Equal(t, expected, got)
			require.NotNil(t, cacheOf(s))
		}
	}
}

func TestLazyCanonicalQuoteInterrupt(t *testing.T) {
	r := NewRealm()
	v, err := r.JSONParseGoString(`"` + strings.Repeat(`中文\n`, 8192) + `"`)
	require.NoError(t, err)
	s := v.AsString()
	require.Equal(t, lazyJSONVerbatim, s.lazyMode())
	js := jsonStringifier{r: r, raw: true}
	js.sb.b = make([]byte, 0, 256)
	r.Interrupt("cancel")
	require.Error(t, js.quote(s))
	require.Empty(t, js.sb.b)
	require.Nil(t, cacheOf(s))
}

// TestLazyFromGoMemoryLimit: a Go string converted on its own charges
// nothing, as converting it eagerly charges nothing; a measurement counts
// it as decoded.
func TestLazyFromGoMemoryLimit(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		lazy       bool
	}{
		{"ascii", "ascii", false}, {"invalid", "a\xffb", true}, {"cjk", strings.Repeat("中", 200000), true}, {"cjk-over-budget", strings.Repeat("中", 600000), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text
			units := len(utf16.Encode([]rune(text)))
			r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
			v, err := r.FromGo(text)
			require.NoError(t, err)
			s := v.AsString()
			require.Equal(t, tc.lazy, s.isLazy())
			if !tc.lazy {
				require.Equal(t, text, s.GoString())
				return
			}
			require.Zero(t, r.Stats().RequestAllocatedBytes, "converting a Go string charges nothing")
			require.Equal(t, units, s.Len())
			require.Nil(t, cacheOf(s))
			if 2*units > 1<<20 {
				live, ok := r.measureLive(func(add func(Value)) { add(v) })
				require.True(t, ok)
				require.Greater(t, live, int64(1<<20), "measured as decoded, past the limit")
				require.Nil(t, cacheOf(s), "measuring decodes nothing")
				return
			}
			require.Equal(t, goText(text), s.GoString())
			require.NoError(t, r.CheckInterrupt())
		})
	}
}

func TestLazyNativeDebugString(t *testing.T) {
	r := NewRealm()
	got := ObjectValue(r.NewNativeFunction(FromHostString("a\xffb"), 0, nil))
	want := ObjectValue(r.NewNativeFunction(FromGoString("a\xffb"), 0, nil))
	display := func(v Value) []string { return []string{v.String(), r.DisplayString(v)} }
	require.Equal(t, display(want), display(got))
}

func TestLazyPlainJSONReuse(t *testing.T) {
	r := NewRealm()
	text := strings.Repeat("x", jsonPlainMin+1)
	s := FromHostString(text)
	want, err := json.Marshal(text)
	require.NoError(t, err)
	for range 3 {
		out, ok, err := r.AppendJSON(nil, StringValue(s))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, string(want), string(out))
		require.NotZero(t, s.lazyData().facts.Load()&factJSONPlain)
		require.Nil(t, cacheOf(s))
	}
	js := jsonStringifier{r: r, raw: true}
	js.sb.b = make([]byte, 0, 256)
	r.Interrupt("cancel")
	require.Error(t, js.quote(s))
	require.Empty(t, js.sb.b)
}

func TestLazyLimitedHostContainerRoundTrip(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
	text := "a\xffb"
	for _, input := range []any{map[string]any{"text": text}, map[string]string{"text": text}, []any{text}, []string{text}} {
		v, err := r.FromGo(input)
		require.NoError(t, err)
		var field Value
		switch input.(type) {
		case map[string]any, map[string]string:
			field, err = v.AsObject().GetProp(r, key(r, "text"))
		default:
			field, err = v.AsObject().GetIndex(r, 0)
		}
		require.NoError(t, err)
		require.Equal(t, goText(text), field.AsString().GoString())
		require.NoError(t, r.CheckInterrupt())
	}
}

func TestLazyPlainJSONStringifyReuse(t *testing.T) {
	f := newHostLazyFixture(t, `export function quote(s){return JSON.stringify(s);}`, RealmOptions{})
	text := strings.Repeat("x", jsonPlainMin+1)
	s := FromHostString(text)
	want, err := json.Marshal(text)
	require.NoError(t, err)
	for range 3 {
		v, err := f.r.Call(f.fn("quote"), Undefined(), []Value{StringValue(s)})
		require.NoError(t, err)
		require.Equal(t, string(want), v.AsString().GoString())
		require.NotZero(t, s.lazyData().facts.Load()&factJSONPlain)
		require.Nil(t, cacheOf(s))
	}
}

func TestLazyASCIIReadReusesValidation(t *testing.T) {
	s := FromHostString(strings.Repeat("x", jsonPlainMin+1))
	require.Equal(t, jsonPlainMin+1, s.Len())
	require.NotZero(t, s.lazyData().facts.Load()&factUTF8Valid)
	require.Equal(t, unsafe.StringData(s.s), unsafe.StringData(s.GoString()), "the source is shared")
	require.Nil(t, cacheOf(s), "exporting decodes nothing")
}

// TestLazyQuotedLen checks the decode measure of lazy strings, which
// avoids decoding where the source gives the length, against AppendJSON and
// json.Marshal of ToGo's result.
func TestLazyQuotedLen(t *testing.T) {
	r := NewRealm()
	var values []*String
	for _, text := range []string{"中文😀\n\"", "a\xffb\x01", "\xed\xa0\x80", strings.Repeat("x<&", jsonPlainMin), "\u2028"} {
		values = append(values, FromHostString(text))
	}
	for _, src := range []string{`"中\n文😀"`, `"\u0061\/"`, `"\ud800x"`, `"` + strings.Repeat("中", 100) + `"`} {
		v, err := r.JSONParseGoString(src)
		require.NoError(t, err)
		values = append(values, v.AsString())
	}
	for _, s := range values {
		out, ok, err := r.AppendJSON(nil, StringValue(s))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, int64(len(out)), jsQuotedLen(s), "%q", s.s)
		g, err := json.Marshal(r.ToGo(StringValue(s)))
		require.NoError(t, err)
		require.Equal(t, int64(len(g)), goQuotedLenJS(s), "%q", s.s)
	}
}

// lazyCase is text whose code units are units. isGo marks text FromHostString
// can carry, as host (maybe invalid UTF-8, read as U+FFFD); the others hold
// lone surrogates, which only JSON.parse makes lazily.
type lazyCase struct {
	name  string
	host  string
	isGo  bool
	units []uint16
}

func goCase(name, text string) lazyCase {
	return lazyCase{name: name, host: text, isGo: true, units: utf16.Encode([]rune(text))}
}

func unitsCase(name string, u ...uint16) lazyCase {
	return lazyCase{name: name, units: u}
}

var lazyCases = []lazyCase{
	goCase("empty", ""),
	goCase("ascii", "a"),
	goCase("ascii-specials", "a\"b\\c\n\t\x00\x1f/<&>"),
	goCase("cjk", "中文"),
	goCase("emoji", "😀"),
	goCase("mixed", "中😀a"),
	goCase("utf8-2byte", "é\u07ff"),
	goCase("utf8-3byte-min", "\u0800"),
	goCase("bmp-max", "\uffff"),
	goCase("astral-min", "\U00010000"),
	goCase("astral-max", "\U0010ffff"),
	goCase("private-use", "\ue000\U000f0000"),
	goCase("bom", "\ufeffa"),
	goCase("line-separators", "a\u2028b\u2029c"),
	goCase("zwj-family", "👨‍👩‍👧"),
	goCase("flag", "🇨🇳"),
	goCase("combining", "e\u0301 Å"),
	goCase("case-special", "İßΣﬁ"),
	goCase("whitespace", "\u3000 中 \u00a0"),
	goCase("replacement-char", "\ufffd"),
	goCase("numeric", " 12.5 "),
	goCase("index-like", "42"),
	goCase("invalid-byte", "\xff"),
	goCase("invalid-middle", "a\xffb"),
	goCase("overlong", "\xc0\x80"),
	goCase("utf8-surrogate-high", "\xed\xa0\x80"),
	goCase("utf8-surrogate-low", "\xed\xbf\xbf"),
	goCase("truncated", "\xe2\x82"),
	goCase("truncated-emoji", "\xf0\x9f\x98"),
	goCase("above-max", "\xf4\x90\x80\x80"),
	goCase("invalid-mixed", "中\xff😀\xe2\x82a"),
	unitsCase("lone-high", 0xd800),
	unitsCase("lone-low", 0xdc00),
	unitsCase("high-then-ascii", 0xd83d, 'a'),
	unitsCase("ascii-then-low", 'a', 0xde00),
	unitsCase("reversed-pair", 0xde00, 0xd83d),
	unitsCase("two-highs", 0xd800, 0xd800, 0x4e2d),
	unitsCase("pair-and-lone", 0xd83d, 0xde00, 0xd83d),
	goCase("long-cjk", strings.Repeat("中😀a", 10000)),
	goCase("long-ascii", strings.Repeat("ab", jsonPlainMin/2+1)),
	goCase("long-ascii-tail", strings.Repeat("a", jsonPlainMin)+"中"),
	goCase("long-invalid", strings.Repeat("中\xff", 5000)),
	unitsCase("long-lone", slices.Repeat([]uint16{0x4e2d, 0xd83d}, 10000)...),
}

// lazySource makes a fresh string from c, or reports false when it cannot
// carry c's units.
type lazySource struct {
	name string
	make func(t *testing.T, r *Realm, c lazyCase) (*String, bool)
}

var lazySources = []lazySource{
	{"host", func(t *testing.T, r *Realm, c lazyCase) (*String, bool) {
		return FromHostString(c.host), c.isGo
	}},
	{"json-raw", func(t *testing.T, r *Realm, c lazyCase) (*String, bool) {
		text := string(utf16.Decode(c.units))
		if !slices.Equal(utf16.Encode([]rune(text)), c.units) {
			return nil, false
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		require.NoError(t, enc.Encode(text))
		v, err := r.JSONParseGoString(strings.TrimSuffix(b.String(), "\n"))
		require.NoError(t, err)
		return v.AsString(), true
	}},
	{"json-escaped", func(t *testing.T, r *Realm, c lazyCase) (*String, bool) {
		var b strings.Builder
		b.WriteByte('"')
		for i, u := range c.units {
			switch {
			case u >= 0x20 && u < 0x7f && u != '"' && u != '\\':
				b.WriteByte(byte(u))
			case i%2 == 0:
				fmt.Fprintf(&b, `\u%04X`, u)
			default:
				fmt.Fprintf(&b, `\u%04x`, u)
			}
		}
		b.WriteByte('"')
		v, err := r.JSONParseGoString(b.String())
		require.NoError(t, err)
		return v.AsString(), true
	}},
	{"json-utf16", func(t *testing.T, r *Realm, c lazyCase) (*String, bool) {
		u := []uint16{'"'}
		for _, x := range c.units {
			if x == '"' || x == '\\' || x < 0x20 {
				u = append(u, utf16.Encode([]rune(fmt.Sprintf(`\u%04x`, x)))...)
				continue
			}
			u = append(u, x)
		}
		u = append(u, '"')
		v, err := r.JSONParse(FromUTF16(u))
		require.NoError(t, err)
		return v.AsString(), true
	}},
}

// eagerOf is c's content built from its code units, never lazy.
func eagerOf(c lazyCase) *String {
	return FromUTF16(slices.Clone(c.units))
}

// forEachLazy calls check with a fresh string per case and source, after
// checking a lazy one has not decoded yet, and checks its content after.
func forEachLazy(t *testing.T, r *Realm, check func(t *testing.T, c lazyCase, s *String)) {
	for _, c := range lazyCases {
		for _, src := range lazySources {
			s, ok := src.make(t, r, c)
			if !ok {
				continue
			}
			if !isASCIIUnits(c.units) || src.name == "host" && len(c.host) >= jsonPlainMin {
				require.True(t, s.isLazy(), "%s/%s: non-ASCII, or long host text, stays lazy", c.name, src.name)
			}
			if s.isLazy() {
				require.Nil(t, cacheOf(s), "%s/%s", c.name, src.name)
			}
			check(t, c, s)
			require.True(t, s.Equals(eagerOf(c)), "%s/%s: content after use", c.name, src.name)
			require.Equal(t, c.units, s.UTF16(), "%s/%s", c.name, src.name)
		}
	}
}

func isASCIIUnits(u []uint16) bool {
	for _, c := range u {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

const lazyMethodsModule = `
const T = {
  length: s => s.length,
  typeof: s => [typeof s, !!s, s ? 1 : 0],
  index: s => [s[0], s[1], s[s.length - 1], s[s.length]],
  charAt: s => [s.charAt(0), s.charAt(1), s.charAt(s.length - 1)],
  charCodeAt: s => [s.charCodeAt(0), s.charCodeAt(1), s.charCodeAt(s.length - 1)],
  codePointAt: s => [s.codePointAt(0), s.codePointAt(1), s.codePointAt(s.length - 1)],
  at: s => [s.at(0), s.at(-1), s.at(-2)],
  slice: s => [s.slice(1), s.slice(1, -1), s.slice(-3), s.slice(0, 1)],
  substring: s => [s.substring(1, 3), s.substring(2), s.substr(1, 2), s.substr(-3)],
  indexOf: s => [s.indexOf("😀"), s.indexOf("中"), s.indexOf("\uDE00"), s.indexOf(""), s.indexOf("a", 1), s.indexOf(s), s.indexOf("\uFFFD")],
  lastIndexOf: s => [s.lastIndexOf("😀"), s.lastIndexOf("中"), s.lastIndexOf("\uD83D"), s.lastIndexOf(s), s.lastIndexOf("")],
  includes: s => [s.includes("😀"), s.includes("文"), s.includes("\uFFFD"), s.includes("\uD800"), s.includes(s)],
  startsEnds: s => [s.startsWith("中"), s.startsWith("\uD83D"), s.startsWith(s.slice(0, 2)), s.endsWith("😀"), s.endsWith("\uDE00"), s.endsWith(s.slice(-1))],
  search: s => [s.search(/文/), s.search(/\uFFFD/), s.search(/[\uD800-\uDFFF]/), s.search(/\p{Emoji_Presentation}/u)],
  match: s => [s.match(/./gu), s.match(/./gs)?.length, s.match(/\p{Script=Han}+/u)?.[0]],
  matchAll: s => Array.from(s.matchAll(/(.)/gsu), m => [m.index, m[1]]),
  replace: s => [s.replace("中", "[$&]"), s.replace(/./u, "x"), s.replaceAll("😀", "e"), s.replace(/(?:)/gu, "|"), s.replace(/./gu, c => c.length)],
  split: s => [s.split(""), s.split("中"), s.split(/(?:)/u), s.split("😀", 2)],
  concat: s => [s + "!", "!" + s, s.concat("中", s), ` + "`<${s}>`" + `, s + s === s.repeat(2)],
  trim: s => [s.trim(), s.trimStart(), s.trimEnd(), (" \u3000" + s + "\uFEFF ").trim()],
  pad: s => [s.padStart(s.length + 3, "中"), s.padEnd(s.length + 3, "😀"), "x".padStart(5, s)],
  repeat: s => [s.repeat(0), s.repeat(3)],
  case: s => [s.toUpperCase(), s.toLowerCase(), s.toLocaleUpperCase(), s.toLocaleLowerCase()],
  normalize: s => ["NFC", "NFD", "NFKC", "NFKD"].map(f => s.normalize(f)),
  wellFormed: s => [s.isWellFormed(), s.toWellFormed()],
  localeCompare: s => [s.localeCompare("中"), s.localeCompare(s), s.localeCompare("")],
  compare: s => [s < "中", s > "a", s === "中😀a", s == "中😀a", s !== "", s === s.slice(0), s == new String(s)],
  number: s => [Number(s), parseInt(s), parseFloat(s), +s, s * 1],
  json: s => [JSON.stringify(s), JSON.stringify({[s]: s}), JSON.stringify([s, s]), JSON.parse(JSON.stringify(s)) === s.toWellFormed() || !s.isWellFormed()],
  uri: s => [encodeURIComponent(s), encodeURI(s)],
  escape: s => escape(s),
  spread: s => [...s],
  forOf: s => { const out = []; for (const c of s) out.push(c.codePointAt(0)); return out; },
  arrayFrom: s => Array.from(s, c => c.length),
  iterator: s => { const it = s[Symbol.iterator](); const out = []; for (let r = it.next(); !r.done; r = it.next()) out.push(r.value); return [out, String(it)]; },
  destructure: s => { const [a, b, ...rest] = s; return [a, b, rest.length]; },
  forIn: s => { let n = 0, last; for (const k in new String(s)) { n++; last = k; } return [n, last]; },
  arrayLike: s => [Array.prototype.map.call(s, c => c.charCodeAt(0)), Array.prototype.join.call(s, "|"), Array.prototype.indexOf.call(s, "中")],
  stringObject: s => { const o = new String(s); return [o.length, Object.keys(o).length, o[1], Object.getOwnPropertyNames(o).length, o.valueOf() === s, 0 in o, Object.getOwnPropertyDescriptor(o, "0")]; },
  entries: s => [...s.split("").entries()].slice(-2),
  propertyKey: s => { const o = {}; o[s] = 1; const m = new Map([[s, 2]]); const set = new Set([s, s.slice(0)]); return [o[s.slice(0)], Object.keys(o), m.get(s.slice(0)), set.size, s in o, Object.hasOwn(o, s)]; },
  regexp: s => { const re = new RegExp(s.slice(0, 50).replace(/[.*+?^${}()|[\]\\\/]/g, "\\$&")); return [re.test(s), re.source.length, re.exec(s)?.index]; },
  symbol: s => [Symbol(s).description === s, Symbol(s).toString(), Symbol.for(s) === Symbol.for(s.slice(0))],
  template: s => String.raw` + "`${s}|${s.length}`" + `,
  raw: s => String.raw({raw: [s, s]}, "-"),
  arrays: s => [[s].indexOf(s), [s].includes(s.slice(0)), [s, s].join(s).length, [s, "中", "a"].sort()],
  sort: s => s.split("").sort().join(""),
  fromCodes: s => [String.fromCharCode(...Array.from({length: Math.min(s.length, 100)}, (_, i) => s.charCodeAt(i))), String.fromCodePoint(...Array.from(s).slice(0, 100).map(c => c.codePointAt(0)))],
  errors: s => [new Error(s).message, String(new TypeError(s))],
  coercion: s => [` + "`${{ toString() { return s; } }}`" + `, String(s), s.toString(), s.valueOf()],
};
export function names() { return Object.keys(T).join(","); }
export function run(name, s) {
  try { return JSON.stringify(["ok", T[name](s)]); } catch (e) { return JSON.stringify(["throw", e.name]); }
}
`

// TestLazyStringMethods runs each String method and iterator as the first
// read of a fresh lazy string and compares the result with an eager string
// of the same units.
func TestLazyStringMethods(t *testing.T) {
	f := newHostLazyFixture(t, lazyMethodsModule, RealmOptions{})
	names := strings.Split(f.callValues("names"), ",")
	require.Greater(t, len(names), 40)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			arg := StringValue(FromGoString(name))
			forEachLazy(t, f.r, func(t *testing.T, c lazyCase, s *String) {
				want := f.callValues("run", arg, StringValue(eagerOf(c)))
				got := f.callValues("run", arg, StringValue(s))
				if got != want {
					t.Fatalf("%s\nwant %.300q\ngot  %.300q", c.name, want, got)
				}
			})
		})
	}
}

// TestLazyStringGoAPI is TestLazyStringMethods for the Go API of String and
// the realm helpers built on it.
func TestLazyStringGoAPI(t *testing.T) {
	r := NewRealm()
	ops := []struct {
		name string
		op   func(s, twin *String) any
	}{
		{"Len", func(s, _ *String) any { return s.Len() }},
		{"UTF16", func(s, _ *String) any { return s.UTF16() }},
		{"GoString", func(s, _ *String) any { return []string{s.GoString(), s.String()} }},
		{"Hash", func(s, _ *String) any { return s.Hash() }},
		{"IsASCII", func(s, _ *String) any { return []bool{s.IsASCII(), s.IsWellFormed()} }},
		{"ASCII", func(s, _ *String) any { a, ok := s.ASCII(); return []any{a, ok} }},
		{"At", func(s, _ *String) any {
			n := s.Len()
			if n == 0 {
				return nil
			}
			return []uint16{s.At(0), s.At(n / 2), s.At(n - 1)}
		}},
		{"Substring", func(s, _ *String) any {
			n := s.Len()
			return [][]uint16{s.Substring(0, n).UTF16(), s.Substring(min(1, n), n).UTF16(), s.Substring(0, n/2).UTF16(), s.Substring(n/2, n/2).UTF16(), r.substring(s, 0, n).UTF16()}
		}},
		{"Equals", func(s, twin *String) any {
			return []bool{s.Equals(twin), twin.Equals(s), s.Equals(s), s.Equals(EmptyString()), s.Equals(FromGoString("中"))}
		}},
		{"EqualsGoString", func(s, twin *String) any {
			return []bool{s.EqualsGoString(twin.GoString()), s.EqualsGoString("中😀a")}
		}},
		{"Compare", func(s, twin *String) any {
			return []int{s.Compare(twin), twin.Compare(s), s.Compare(FromGoString("中")), s.Compare(FromGoString("a")), s.Compare(FromUTF16([]uint16{0xd800}))}
		}},
		{"IndexOf", func(s, twin *String) any {
			return []int{s.IndexOf(FromGoString("😀"), 0), s.IndexOf(FromGoString("中"), 1), s.IndexOf(FromUTF16([]uint16{0xde00}), 0), s.IndexOf(EmptyString(), 0), s.IndexOf(twin, 0), twin.IndexOf(s, 0)}
		}},
		{"Concat", func(s, _ *String) any {
			a, err := r.Concat(s, FromGoString("!"))
			require.NoError(t, err)
			b, err := r.Concat(FromGoString("中"), s)
			require.NoError(t, err)
			c, err := r.Concat(s, s)
			require.NoError(t, err)
			return [][]uint16{a.UTF16(), b.UTF16(), c.UTF16()}
		}},
		{"Intern", func(s, twin *String) any { return []any{r.Intern(s).UTF16(), r.Intern(s) == r.Intern(twin)} }},
		{"Key", func(s, twin *String) any {
			o := r.NewObject()
			require.NoError(t, o.SetProp(r, r.KeyFromString(s), NumberValue(1)))
			v, err := o.GetProp(r, r.KeyFromString(twin))
			require.NoError(t, err)
			return []any{r.KeyFromString(s).IsIndex(), v.AsNumber()}
		}},
		{"AppendJSON", func(s, _ *String) any {
			out, ok, err := r.AppendJSON(nil, StringValue(s))
			require.NoError(t, err)
			return []any{string(out), ok}
		}},
		{"QuotedLen", func(s, _ *String) any { return []int64{jsQuotedLen(s), goQuotedLenJS(s)} }},
		{"ToGo", func(s, _ *String) any {
			var u string
			_, err := r.Unmarshal(StringValue(s), &u)
			require.NoError(t, err)
			strict, err := r.ToGoStrict(StringValue(s))
			require.NoError(t, err)
			return []any{r.ToGo(StringValue(s)), strict, u, StringValue(s).String()}
		}},
		{"ToNumber", func(s, _ *String) any {
			n, err := r.ToNumber(StringValue(s))
			require.NoError(t, err)
			return fmt.Sprint(n)
		}},
	}
	for _, o := range ops {
		t.Run(o.name, func(t *testing.T) {
			forEachLazy(t, r, func(t *testing.T, c lazyCase, s *String) {
				require.Equal(t, o.op(eagerOf(c), eagerOf(c)), o.op(s, eagerOf(c)), c.name)
			})
		})
	}
}

// TestLazyStringExport checks that valid UTF-8 leaves as the source string,
// without decoding, and anything else as the text FromGoString gives.
func TestLazyStringExport(t *testing.T) {
	r := NewRealm()
	for _, c := range lazyCases {
		if !c.isGo {
			continue
		}
		s := FromHostString(c.host)
		got := s.GoString()
		require.Equal(t, FromGoString(c.host).GoString(), got, c.name)
		require.Equal(t, goText(c.host), got, c.name)
		if s.isLazy() && got == c.host {
			require.Nil(t, cacheOf(s), "%s: valid UTF-8 export reads the source", c.name)
		}
		require.Equal(t, goText(c.host), r.ToGo(StringValue(s)), c.name)
	}
}

func TestLazySameSourceEqualsWithoutDecode(t *testing.T) {
	const contents = `a\n\u4e2d`
	const text = "a中"
	for _, c := range []struct {
		name string
		a, b *String
	}{
		{"json_modes", newLazyString(contents, lazyJSONContents), newLazyString(contents, lazyJSONVerbatim)},
		{"text_modes", newLazyString(text, lazyText), newLazyString(text, lazyWTF8)},
	} {
		require.True(t, c.a.Equals(c.b), c.name)
		require.True(t, c.b.Equals(c.a), c.name)
		require.Nil(t, cacheOf(c.a), c.name)
		require.Nil(t, cacheOf(c.b), c.name)
	}
	// One source in a text mode and a JSON mode: equal contents compare
	// equal by decoding.
	a, b := newLazyString(text, lazyText), newLazyString(text, lazyJSONContents)
	require.True(t, a.Equals(b))
	require.True(t, b.Equals(a))
}

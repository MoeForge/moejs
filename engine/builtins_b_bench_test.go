package engine

import (
	"strconv"
	"strings"
	"testing"
)

// Benchmarks for the hook hot path: string methods on
// ~1 KiB ASCII input, regexp replace with a cache hit, JSON parse/stringify
// of a 10 KiB document and encodeURIComponent of 150 characters.

var benchASCII1K = FromGoString(strings.TrimSpace(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 23))) // 1034 units, nothing to trim

func benchMethodB(b *testing.B, r *Realm, this Value, name string, args ...Value) {
	fn, err := r.GetV(this, r.KeyFromGoString(name))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, this, args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStringTrimNothing(b *testing.B) {
	benchMethodB(b, NewRealm(), StringValue(benchASCII1K), "trim")
}

func BenchmarkStringTrimSpaces(b *testing.B) {
	s := FromGoString("  \t" + strings.Repeat("x", 1000) + " \n ")
	benchMethodB(b, NewRealm(), StringValue(s), "trim")
}

func BenchmarkStringIncludes(b *testing.B) {
	benchMethodB(b, NewRealm(), StringValue(benchASCII1K), "includes", str("lazy dog. The quick"))
}

func BenchmarkStringStartsWith(b *testing.B) {
	benchMethodB(b, NewRealm(), StringValue(benchASCII1K), "startsWith", str("The quick"))
}

func BenchmarkStringIndexOf(b *testing.B) {
	benchMethodB(b, NewRealm(), StringValue(benchASCII1K), "indexOf", str("dog"), IntValue(500))
}

func BenchmarkStringToLowerCaseAlreadyLower(b *testing.B) {
	s := FromGoString(strings.Repeat("already lower case text ", 40))
	benchMethodB(b, NewRealm(), StringValue(s), "toLowerCase")
}

func BenchmarkStringToLowerCase(b *testing.B) {
	benchMethodB(b, NewRealm(), StringValue(benchASCII1K), "toLowerCase")
}

func BenchmarkStringSplitComma(b *testing.B) {
	s := FromGoString(strings.Repeat("alpha,beta,gamma,delta,", 10) + "end")
	benchMethodB(b, NewRealm(), StringValue(s), "split", str(","))
}

func BenchmarkStringSlice(b *testing.B) {
	benchMethodB(b, NewRealm(), StringValue(benchASCII1K), "slice", IntValue(100), IntValue(200))
}

func BenchmarkStringReplaceString(b *testing.B) {
	benchMethodB(b, NewRealm(), StringValue(benchASCII1K), "replace", str("lazy"), str("energetic"))
}

func BenchmarkStringReplaceAllString(b *testing.B) {
	benchMethodB(b, NewRealm(), StringValue(benchASCII1K), "replaceAll", str("fox"), str("cat"))
}

func BenchmarkRegExpReplaceWhitespace150(b *testing.B) {
	r := NewRealm()
	s := FromGoString("data:  application/json;   base64,\tAAAA BBBB  CCCC\n DDDD EEEE FFFF GGGG HHHH IIII JJJJ KKKK LLLL MMMM NNNN OOOO PPPP QQQQ RRRR SSSS TTTT UUUU VVVV WWWW")
	if s.Len() < 140 || s.Len() > 160 {
		b.Fatalf("subject length drifted: %d", s.Len())
	}
	// A cache hit: the pattern is compiled once, then each iteration
	// constructs the RegExp (as a literal evaluation would) and replaces.
	fn, _ := r.GetV(StringValue(s), StringKey(AtomReplace))
	pattern, flags := FromGoString(`\s+`), FromGoString("g")
	space := str(" ")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rx, err := r.NewRegExp(pattern, flags)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := r.Call(fn, StringValue(s), []Value{ObjectValue(rx), space}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRegExpReplaceWhitespaceUTF16 is the same collapse on a subject
// with non-ASCII text (the hook prompt in bench/baseline contains Chinese).
func BenchmarkRegExpReplaceWhitespaceUTF16(b *testing.B) {
	r := NewRealm()
	s := FromGoString("You are a helpful image generation assistant. 请根据用户描述生成图片。\nA watercolor painting of   a lighthouse at dusk,\nwith seagulls circling and warm light spilling onto the rocks.\nKeep the palette soft and the horizon low.")
	rx, err := r.NewRegExp(FromGoString(`\s+`), FromGoString("g"))
	if err != nil {
		b.Fatal(err)
	}
	benchMethodB(b, r, StringValue(s), "replace", ObjectValue(rx), str(" "))
}

func BenchmarkRegExpTest(b *testing.B) {
	r := NewRealm()
	rx, _ := r.NewRegExp(FromGoString(`^data:([a-z]+/[a-z0-9.+-]+);base64,`), FromGoString("i"))
	benchMethodB(b, r, ObjectValue(rx), "test", str("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="))
}

func BenchmarkRegExpExecCaptures(b *testing.B) {
	r := NewRealm()
	rx, _ := r.NewRegExp(FromGoString(`(\d+)x(\d+)`), FromGoString(""))
	benchMethodB(b, r, ObjectValue(rx), "exec", str("size=1280x720;fps=30"))
}

func BenchmarkRegExpSplit(b *testing.B) {
	r := NewRealm()
	rx, _ := r.NewRegExp(FromGoString(`\s*,\s*`), FromGoString(""))
	benchMethodB(b, r, str("alpha, beta ,gamma,  delta, epsilon"), "split", ObjectValue(rx))
}

func BenchmarkRegExpCompileMiss(b *testing.B) {
	r := NewRealm()
	pattern, flags := FromGoString(`^(?<scheme>[a-z]+):\/\/([^/]+)\/(.*)$`), FromGoString("i")
	b.ReportAllocs()
	for b.Loop() {
		clear(r.regexpState().cache)
		if _, err := r.NewRegExp(pattern, flags); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRegExpNew builds a RegExp of a pattern no cache holds (each
// iteration's differs), as `new RegExp("^item-" + id + "-(\\d+)$")` in a
// loop does; BenchmarkRegExpNewTest then tests it once.
func BenchmarkRegExpNew(b *testing.B) { benchRegExpNew(b, false) }

func BenchmarkRegExpNewTest(b *testing.B) { benchRegExpNew(b, true) }

func benchRegExpNew(b *testing.B, test bool) {
	r := NewRealm()
	flags := FromGoString("")
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		id := strconv.Itoa(i)
		rx, err := r.NewRegExp(FromGoString(`^item-`+id+`-(\d+)$`), flags)
		if err != nil {
			b.Fatal(err)
		}
		if test {
			if _, err := regexpProtoTest(r, ObjectValue(rx), []Value{str("item-" + id + "-42")}); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// benchJSONText10KiB is the sampleJSON10KiB document as text.
var benchJSONText10KiB = func() *String {
	var sb strings.Builder
	sb.WriteString(`{"model":"video-gen-1","prompt":"a cat surfing on a rainbow","size":"1280x720","duration":8,"seed":42,"metadata":{"user":"u_1","tags":["a","b","c"],"flags":{"hd":true,"audio":false}},"images":[`)
	for i := range 40 {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"url":"https://example.com/images/`)
		sb.WriteString(strings.Repeat("x", 40))
		sb.WriteString(`.png","width":1024,"height":768,"weight":0.75,"caption":"reference image number `)
		sb.WriteString(strings.Repeat("y", 60))
		sb.WriteString(`","nested":{"a":1,"b":[1,2,3],"c":null}}`)
	}
	sb.WriteString(`]}`)
	return FromGoString(sb.String())
}()

func BenchmarkJSONParse10KiB(b *testing.B) {
	r := NewRealm()
	benchMethodB(b, r, ObjectValue(r.JSON), "parse", StringValue(benchJSONText10KiB))
}

func BenchmarkJSONStringify10KiB(b *testing.B) {
	r := NewRealm()
	v, err := r.JSONParse(benchJSONText10KiB)
	if err != nil {
		b.Fatal(err)
	}
	benchMethodB(b, r, ObjectValue(r.JSON), "stringify", v)
}

func BenchmarkJSONParse2KiBHookArg(b *testing.B) {
	r := NewRealm()
	text := FromGoString(`{"model":"gpt-image-1","prompt":"` + strings.Repeat("a cat ", 40) + `","n":1,"size":"1024x1024","quality":"high","response_format":"b64_json","user":"u_42","metadata":{"trace":"abc123","tags":["x","y"],"nested":{"k":1.5,"flag":false,"list":[1,2,3,4,5]}},"image":"data:image/png;base64,` + strings.Repeat("QUJD", 300) + `"}`)
	benchMethodB(b, r, ObjectValue(r.JSON), "parse", StringValue(text))
}

func BenchmarkEncodeURIComponent150(b *testing.B) {
	r := NewRealm()
	s := str("https://example.com/v1/images/generations?model=gpt image&prompt=a cat surfing on a rainbow, photorealistic&size=1024x1024&n=1&user=u_42&x=1")
	if s.AsString().Len() < 140 || s.AsString().Len() > 160 {
		b.Fatalf("subject length drifted: %d", s.AsString().Len())
	}
	fn, _ := r.Global.GetProp(r, StringKey(AtomEncodeURIComponent))
	args := []Value{s}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDateNowAndISO(b *testing.B) {
	r := NewRealm()
	d, _ := r.Construct(ObjectValue(r.DateCtor), []Value{Int64Value(1758630000000)}, nil)
	benchMethodB(b, r, d, "toISOString")
}

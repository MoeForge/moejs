package moejs_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"
	"weak"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseJSONStringMatchesParseJSON: ParseJSONString gives what ParseJSON
// gives, value or error, on valid and invalid text, invalid UTF-8, a byte
// order mark, duplicate keys and the nesting limit.
func TestParseJSONStringMatchesParseJSON(t *testing.T) {
	deep := func(open, close string, n int) string {
		return strings.Repeat(open, n) + "0" + strings.Repeat(close, n)
	}
	corpus := []string{
		`{}`, `[]`, `null`, `true`, `false`, `0`, `-0`, `1e400`, `-12.5e-3`, `"text"`,
		`
 [1, "a", {"b": [true, null]}] `,
		`{"a": 1, "a": 2, "__proto__": {"x": 1}, "": "", "0": 0}`,
		`"é😀\ud800 \\ \/ \" \b\f\n\r\t"`,
		`{"model": "m", "prompt": "déjà vu 猫 😀"}`,
		"\"\xff\xfe\"", "{\"k\xc3\": \"v\xe2\x82\"}",
		``, `{`, `[1,]`, `{"a" 1}`, `01`, "\"\x01\"", `tru`, `1 2`, `"\u12"`, `{"a":1,}`, `'a'`, `NaN`, "\ufeff{}",
		deep("[", "]", 10000), deep(`{"a":`, "}", 10000),
		deep("[", "]", 10001), deep(`{"a":`, "}", 10001), strings.Repeat("[", 10001),
	}
	var many strings.Builder
	many.WriteString("{")
	for i := range 2000 {
		fmt.Fprintf(&many, `"k%d": [%d, "v%d"],`, i, i, i)
	}
	many.WriteString(`"end": 1}`)
	corpus = append(corpus, many.String())
	// Past 64 keys the parser finds a duplicate through an index: repeat
	// the first key and one added after the index was built.
	var wide strings.Builder
	wide.WriteString("{")
	for i := range 70 {
		fmt.Fprintf(&wide, `"k%d": %d, `, i, i)
	}
	wide.WriteString(`"k0": "again", "k66": "again"}`)
	corpus = append(corpus, wide.String())

	rt := moejs.NewRuntime(moejs.Options{})
	for i, text := range corpus {
		name := strconv.Itoa(i)
		want, wantErr := rt.ParseJSON([]byte(text))
		got, gotErr := rt.ParseJSONString(text)
		if wantErr != nil {
			require.Error(t, gotErr, name)
			assert.Equal(t, wantErr.Error(), gotErr.Error(), name)
			var we, ge *moejs.Exception
			require.True(t, errors.As(wantErr, &we), name)
			require.True(t, errors.As(gotErr, &ge), name)
			assert.Equal(t, we.Name(), ge.Name(), name)
			continue
		}
		require.NoError(t, gotErr, name)
		assert.Equal(t, string(mustAppendJSON(t, rt, want)), string(mustAppendJSON(t, rt, got)), name)
		wantGo, err := rt.ToGo(want)
		require.NoError(t, err, name)
		gotGo, err := rt.ToGo(got)
		require.NoError(t, err, name)
		assert.Equal(t, wantGo, gotGo, name)
	}

	v, err := rt.ParseJSONString(wide.String())
	require.NoError(t, err)
	m, err := rt.ToGo(v)
	require.NoError(t, err)
	assert.Len(t, m, 70)
	assert.Equal(t, "again", m.(map[string]any)["k0"])
	assert.Equal(t, "again", m.(map[string]any)["k66"])

	for _, text := range []string{deep("[", "]", 10001), deep(`{"a":`, "}", 10001)} {
		_, err := rt.ParseJSONString(text)
		var ex *moejs.Exception
		require.True(t, errors.As(err, &ex))
		assert.Equal(t, "RangeError", ex.Name())
	}
}

// imageBody is an ASCII JSON text shaped like an image request: base64
// images of the given sizes in bytes, and ordinary fields.
func imageBody(sizes ...int) string {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-image-1","n":1,"size":"1024x1024","stream":false,"images":[`)
	for i, n := range sizes {
		if i > 0 {
			sb.WriteString(",")
		}
		raw := make([]byte, n*3/4)
		for j := range raw {
			raw[j] = byte(j*7 + i)
		}
		sb.WriteString(`{"type":"image_url","image_url":{"url":"data:image/png;base64,`)
		sb.WriteString(base64.StdEncoding.EncodeToString(raw))
		sb.WriteString(`","detail":"high"}}`)
	}
	sb.WriteString(`],"messages":[`)
	for i := range 200 {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"role":"user","content":"turn %d: a cat on a windowsill, soft light, 35mm","tokens":%d}`, i, 12+i%7)
	}
	sb.WriteString(`],"user":"u-1234","metadata":{"trace":"abc123","tags":["a","b","c"]}}`)
	return sb.String()
}

const imageBodySource = `
export function inspect(req) {
  let size = 0;
  for (const im of req.images) size += im.image_url.url.length;
  return req.model + "|" + req.images.length + "|" + size + "|" + req.messages.length;
}
`

func newImageBodyRuntime(t testing.TB) (*moejs.Runtime, moejs.Hook) {
	t.Helper()
	mod, err := moejs.Compile("body.js", imageBodySource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	return rt, mustHook(t, mod, "inspect")
}

// TestParseJSONStringNoCopy: ParseJSONString of a large text whose bytes
// are mostly long strings allocates a small part of the text's size, where
// ParseJSON allocates at least a copy of it.
func TestParseJSONStringNoCopy(t *testing.T) {
	body := imageBody(1<<20, 1<<20, 512<<10)
	b := []byte(body)
	rt := moejs.NewRuntime(moejs.Options{})
	perCall := func(parse func() (moejs.Value, error)) uint64 {
		const n = 8
		v, err := parse()
		require.NoError(t, err)
		runtime.KeepAlive(v)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range n {
			v, err = parse()
			if err != nil {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(v)
		return (after.TotalAlloc - before.TotalAlloc) / n
	}
	copied := perCall(func() (moejs.Value, error) { return rt.ParseJSON(b) })
	shared := perCall(func() (moejs.Value, error) { return rt.ParseJSONString(body) })
	t.Logf("text %d bytes; per call: ParseJSON %d bytes, ParseJSONString %d bytes", len(body), copied, shared)
	assert.GreaterOrEqual(t, copied, uint64(len(body)))
	assert.Less(t, shared, uint64(len(body)/16))

	v, err := rt.ParseJSONString(body)
	require.NoError(t, err)
	url, err := rt.Get(v, "images")
	require.NoError(t, err)
	url, err = rt.Get(url, "0")
	require.NoError(t, err)
	url, err = rt.Get(url, "image_url")
	require.NoError(t, err)
	url, err = rt.Get(url, "url")
	require.NoError(t, err)
	s, err := rt.ToGo(url)
	require.NoError(t, err)
	start := strings.Index(body, `"data:`) + 1
	assert.Equal(t, unsafe.StringData(body[start:]), unsafe.StringData(s.(string)), "the exported string is the text's own bytes")
}

// parseBodyString parses an image request with ParseJSONString and runs a
// hook on it, from a string or from a []byte through unsafe.String, and
// returns a weak pointer to the text's memory, which only the runtime can
// keep alive once it returns.
//
//go:noinline
func parseBodyString(t *testing.T, rt *moejs.Runtime, h moejs.Hook, fromBytes bool) weak.Pointer[byte] {
	body := imageBody(1<<20, 256<<10)
	if fromBytes {
		b := []byte(body)
		body = unsafe.String(unsafe.SliceData(b), len(b))
	}
	v, err := rt.ParseJSONString(body)
	require.NoError(t, err)
	res, err := rt.Call(h, v)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.String(), "gpt-image-1|2|"), res.String())
	return weak.Make(unsafe.StringData(body))
}

// TestParseJSONStringReleased: once ReleaseCallData ran, the runtime keeps
// no reference to the text ParseJSONString parsed; without it the runtime
// keeps the text until the next request.
func TestParseJSONStringReleased(t *testing.T) {
	for _, fromBytes := range []bool{false, true} {
		for _, release := range []bool{true, false} {
			name := "string"
			if fromBytes {
				name = "bytes"
			}
			if release {
				name += "/release"
			} else {
				name += "/no-release"
			}
			t.Run(name, func(t *testing.T) {
				rt, h := newImageBodyRuntime(t)
				parseBodyString(t, rt, h, fromBytes)
				rt.ReleaseCallData()

				wp := parseBodyString(t, rt, h, fromBytes)
				if release {
					rt.ReleaseCallData()
				}
				runtime.GC()
				runtime.GC()
				if release {
					assert.Nil(t, wp.Value(), "the released text is collected")
				} else {
					assert.NotNil(t, wp.Value(), "the runtime keeps the text of the request it ran")
				}
				runtime.KeepAlive(rt)
			})
		}
	}
}

// ExampleRuntime_ParseJSONString parses a request body the host holds as a
// []byte without copying it: the host does not modify the body until the
// request is done and nothing derived from it is kept.
func ExampleRuntime_ParseJSONString() {
	mod, _ := moejs.Compile("hook.js", `export function decode(req) { return req.model + " " + req.image.length; }`)
	hook, _ := mod.Hook("decode")
	rt := moejs.NewRuntime(moejs.Options{})
	_ = rt.Load(mod)

	body := []byte(`{"model": "gpt-image-1", "image": "iVBORw0KGgo="}`)
	arg, _ := rt.ParseJSONString(unsafe.String(unsafe.SliceData(body), len(body)))
	res, _ := rt.Call(hook, arg)
	fmt.Println(res.String())
	rt.ReleaseCallData()
	// Output: gpt-image-1 12
}

// BenchmarkParseJSONBody parses an 8.1 MB image request (base64 images of
// 2, 2, 2, 1.5 and 0.25 MiB, and 200 messages) and runs a hook that reads
// it, then releases the call's data, as a pooling host does per request.
// live-B/call is the heap that each of 8 calls not yet released holds
// beyond the host's own text.
func BenchmarkParseJSONBody(b *testing.B) {
	body := imageBody(2<<20, 2<<20, 2<<20, 3<<19, 1<<18)
	text := []byte(body)
	for _, bc := range []struct {
		name  string
		parse func(rt *moejs.Runtime) (moejs.Value, error)
	}{
		{"ParseJSON", func(rt *moejs.Runtime) (moejs.Value, error) { return rt.ParseJSON(text) }},
		{"ParseJSONString", func(rt *moejs.Runtime) (moejs.Value, error) {
			return rt.ParseJSONString(unsafe.String(unsafe.SliceData(text), len(text)))
		}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			call := func(rt *moejs.Runtime, h moejs.Hook) {
				v, err := bc.parse(rt)
				if err != nil {
					b.Fatal(err)
				}
				if _, err = rt.Call(h, v); err != nil {
					b.Fatal(err)
				}
			}

			const inFlight = 8
			rts := make([]*moejs.Runtime, inFlight)
			hooks := make([]moejs.Hook, inFlight)
			for i := range rts {
				rts[i], hooks[i] = newImageBodyRuntime(b)
				call(rts[i], hooks[i])
				rts[i].ReleaseCallData()
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			for i, rt := range rts {
				call(rt, hooks[i])
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			live := (int64(after.HeapAlloc) - int64(before.HeapAlloc)) / inFlight
			for _, rt := range rts {
				rt.ReleaseCallData()
			}

			rt, h := rts[0], hooks[0]
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for b.Loop() {
				call(rt, h)
				rt.ReleaseCallData()
			}
			b.ReportMetric(float64(live), "live-B/call")
		})
	}
}

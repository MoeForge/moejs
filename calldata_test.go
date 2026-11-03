package moejs_test

import (
	"encoding/json"
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

const callDataSource = `
export let kept, keptPart;
export function reads(req) {
  const size = () => req.image.length + req.messages[0].content[0].image_url.url.length;
  return { model: req.model, size: size() };
}
export function ignores(req) { return { model: "none", size: 0 }; }
export function store(req) {
  kept = req;
  keptPart = req.messages[0];
  return req.model;
}
export function read() {
  return [JSON.stringify(kept), keptPart.content[0].image_url.url.length, keptPart.role, kept.tags.join()].join("|");
}
export function mid(req) {
  const role = req.messages[0].role;
  host.release();
  return [role, req.model, req.messages[0].content[0].type, req.tags.join(), req.image.length].join("|");
}
`

// callDataRequest is shaped like a new-api image request: the ASCII image
// (a data URL) twice, in nested maps and lists.
func callDataRequest(image string) map[string]any {
	return map[string]any{
		"model": "gpt-image-1",
		"image": image,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": image}},
				map[string]any{"type": "text", "text": "draw"},
			},
		}},
		"tags":   []string{"a", "b"},
		"stream": false,
	}
}

func newCallDataRuntime(t testing.TB) (*moejs.Runtime, *moejs.Module) {
	t.Helper()
	mod, err := moejs.Compile("plugin.js", callDataSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("host", map[string]any{
		"release": moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			rt.ReleaseCallData()
			return moejs.Undefined(), nil
		}),
	}))
	require.NoError(t, rt.Load(mod))
	return rt, mod
}

// callDataImage runs one request with a 1 MiB image through h and returns a
// weak pointer to the image's bytes, which only the runtime can keep alive
// once it returns.
//
//go:noinline
func callDataImage(t *testing.T, rt *moejs.Runtime, h moejs.Hook, want map[string]any) weak.Pointer[byte] {
	image := strings.Repeat("A", 1<<20)
	wp := weak.Make(unsafe.StringData(image))
	x, err := rt.FromGo(callDataRequest(image))
	require.NoError(t, err)
	res, err := rt.Call(h, x)
	require.NoError(t, err)
	out, err := rt.ToGo(res)
	require.NoError(t, err)
	require.Equal(t, want, out)
	return wp
}

var callDataReads = map[string]any{"model": "gpt-image-1", "size": int64(2 << 20)}

// TestReleaseCallDataCollectsRequest: once ReleaseCallData ran, the runtime
// no longer keeps the host data of the request it ran, whether the hook
// read its argument (through a closure) or ignored it; without it the
// runtime keeps the request until the next one.
func TestReleaseCallDataCollectsRequest(t *testing.T) {
	for _, hc := range []struct {
		name string
		want map[string]any
	}{
		{"reads", callDataReads},
		{"ignores", map[string]any{"model": "none", "size": int64(0)}},
	} {
		for _, release := range []bool{true, false} {
			name := hc.name + "/release"
			if !release {
				name = hc.name + "/no-release"
			}
			t.Run(name, func(t *testing.T) {
				rt, mod := newCallDataRuntime(t)
				h := mustHook(t, mod, hc.name)
				// A first request that reads its argument sizes the chunks,
				// so the next one's argument is carved from a chunk rather
				// than allocated on its own, also when the hook ignores it.
				callDataImage(t, rt, mustHook(t, mod, "reads"), callDataReads)
				rt.ReleaseCallData()

				wp := callDataImage(t, rt, h, hc.want)
				if release {
					rt.ReleaseCallData()
				}
				runtime.GC()
				runtime.GC()
				if release {
					assert.Nil(t, wp.Value(), "the released request is collected")
				} else {
					assert.NotNil(t, wp.Value(), "the runtime keeps the request it ran")
				}
				runtime.KeepAlive(rt)
			})
		}
	}
}

// TestReleaseCallDataKeepsStoredValues: what the module stored of a
// released request, the argument (only partly converted when it was
// released) and a part of it, stays valid: later requests and a GC in
// between change nothing of it.
func TestReleaseCallDataKeepsStoredValues(t *testing.T) {
	rt, mod := newCallDataRuntime(t)
	store, read := mustHook(t, mod, "store"), mustHook(t, mod, "read")
	image := strings.Repeat("B", 64)
	req := callDataRequest(image)
	x, err := rt.FromGo(req)
	require.NoError(t, err)
	res, err := rt.Call(store, x)
	require.NoError(t, err)
	assert.Equal(t, "gpt-image-1", res.String())
	rt.ReleaseCallData()

	for i := range 3 {
		other, err := rt.FromGo(callDataRequest(strings.Repeat("C", 64)))
		require.NoError(t, err)
		_, err = rt.Call(mustHook(t, mod, "reads"), other)
		require.NoError(t, err)
		rt.ReleaseCallData()
		runtime.GC()

		want, err := json.Marshal(req)
		require.NoError(t, err)
		res, err := rt.Call(read, moejs.Undefined())
		require.NoError(t, err, "read %d", i)
		assert.Equal(t, string(want)+"|64|user|a,b", res.String(), "read %d", i)
		rt.ReleaseCallData()

		kept, ok := rt.Export("kept")
		require.True(t, ok)
		out, err := rt.ToGo(kept)
		require.NoError(t, err)
		// Unchanged, the argument exports as the Go values it was made of.
		assert.Equal(t, req, out, "ToGo %d", i)
	}
}

// TestReleaseCallDataInsideCall: a host function that calls
// ReleaseCallData during a call changes nothing; the call goes on to
// convert the rest of its argument.
func TestReleaseCallDataInsideCall(t *testing.T) {
	rt, mod := newCallDataRuntime(t)
	mid := mustHook(t, mod, "mid")
	for i := range 3 {
		x, err := rt.FromGo(callDataRequest(strings.Repeat("D", 100+i)))
		require.NoError(t, err)
		res, err := rt.Call(mid, x)
		require.NoError(t, err)
		assert.Equal(t, "user|gpt-image-1|image_url|a,b|"+strconv.Itoa(100+i), res.String())
		rt.ReleaseCallData()
	}
}

// BenchmarkCallReleaseCallData is BenchmarkCall with ReleaseCallData after
// each call, as a host that pools runtimes runs one hook per request.
func BenchmarkCallReleaseCallData(b *testing.B) {
	mod, err := moejs.Compile("plugin.js", hostSource)
	require.NoError(b, err)
	rt := newHostRuntime(b, mod)
	images := mustHook(b, mod, "protocols", "openai.images", "decodeRequest")
	arg, err := rt.FromGo(map[string]any{"size": "1024x1024"})
	require.NoError(b, err)
	var buf []byte
	b.ReportAllocs()
	for b.Loop() {
		res, err := rt.Call(images, arg)
		if err != nil {
			b.Fatal(err)
		}
		if buf, err = rt.AppendJSON(buf[:0], res); err != nil {
			b.Fatal(err)
		}
		rt.ReleaseCallData()
	}
}

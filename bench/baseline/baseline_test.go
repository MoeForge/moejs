package baseline

// Scratch baseline: the same plugin-style hook driven from Go through three
// engines, measured the way new-api drives plugins (host value in, exported
// Go value out). Numbers here define the bar moejs has to beat.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moejs"
	quickjs "github.com/buke/quickjs-go"
	"github.com/grafana/sobek"
	v8 "rogchap.com/v8go"
)

const pluginModule = `
const MODELS = {
  "vendor-a": { maxImages: 4, sizes: ["1024x1024", "1536x1024", "1024x1536"] },
  "vendor-b": { maxImages: 1, sizes: ["512x512", "1024x1024"] },
  "vendor-c-pro": { maxImages: 8, sizes: ["2048x2048"] },
};

function trimmed(value) {
  return String(value || "").trim();
}

function responsesInput(req) {
  const texts = [], images = [];
  const input = req.input;
  if (typeof input === "string") texts.push(input);
  else if (Array.isArray(input)) {
    for (const item of input) {
      if (typeof item === "string") { texts.push(item); continue; }
      if (!item || typeof item !== "object" || Array.isArray(item)) continue;
      const content = item.content === undefined ? [item] : Array.isArray(item.content) ? item.content : [item.content];
      for (const part of content) {
        if (typeof part === "string") { texts.push(part); continue; }
        if (!part || typeof part !== "object" || Array.isArray(part)) continue;
        if (["input_text", "text"].includes(part.type) && typeof part.text === "string") texts.push(part.text);
        if (["input_image", "image_url"].includes(part.type)) {
          let image = part.image_url;
          if (image && typeof image === "object") image = image.url;
          if (trimmed(image)) images.push(trimmed(image));
        }
      }
    }
  }
  return { texts: texts, images: images };
}

export function decode(ctx) {
  const req = ctx.body && ctx.body.value;
  if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be a JSON object");
  const model = trimmed(req.model);
  const spec = MODELS[model];
  if (!spec) throw new Error("unsupported model: " + model);
  const parsed = responsesInput(req);
  const prompt = parsed.texts.join("\n").replace(/\s+/g, " ").trim();
  if (!prompt) throw new Error("prompt is required");
  if (parsed.images.length > spec.maxImages) throw new Error("too many reference images for " + model + ": " + parsed.images.length);
  const size = trimmed(req.size) || spec.sizes[0];
  if (!spec.sizes.includes(size)) throw new Error("unsupported size " + size + " for " + model + ", expected one of " + spec.sizes.join(", "));
  const body = Object.assign({}, { model: model, prompt: prompt, size: size, n: Number.isInteger(req.n) && req.n > 0 ? req.n : 1 });
  if (parsed.images.length) body.image_urls = parsed.images.map(function (u) { return u; });
  if (req.metadata && typeof req.metadata === "object") {
    body.metadata = {};
    for (const key of Object.keys(req.metadata)) {
      const value = req.metadata[key];
      if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") body.metadata[key] = value;
    }
  }
  const headers = {};
  for (const name of Object.keys(ctx.headers || {})) headers[name.toLowerCase()] = ctx.headers[name];
  const hasAuth = Object.prototype.hasOwnProperty.call(headers, "authorization");
  return {
    kind: "submit",
    model: ctx.model,
    action: parsed.images.length ? "image_to_image" : "text_to_image",
    requestBody: body,
    meta: { imageCount: parsed.images.length, hasAuth: hasAuth, promptBytes: encodeURIComponent(prompt).length, upstream: ctx.upstream ? ctx.upstream.kind : "vendor" },
  };
}
`

var pluginScript = strings.Replace(pluginModule, "export function decode", "function decode", 1) + "\nglobalThis.decode = decode;\n"

func hookArg(model string) map[string]any {
	image := "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ", 24)
	return map[string]any{
		"protocol":      "openai_responses",
		"operation":     "create",
		"model":         model,
		"upstreamModel": model,
		"stream":        false,
		"upstream":      map[string]any{"kind": "vendor"},
		"headers": map[string]any{
			"Authorization":   "Bearer sk-test-1234567890",
			"Content-Type":    "application/json",
			"X-Request-Id":    "req_0123456789abcdef",
			"User-Agent":      "new-api/1.0",
			"Accept-Encoding": "gzip",
		},
		"body": map[string]any{
			"kind": "json",
			"value": map[string]any{
				"model": model,
				"input": []any{
					map[string]any{"role": "system", "content": "You are a helpful image generation assistant. 请根据用户描述生成图片。"},
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "input_text", "text": "A watercolor painting of   a lighthouse at dusk,\nwith seagulls circling and warm light spilling onto the rocks."},
						map[string]any{"type": "input_image", "image_url": map[string]any{"url": image}},
						map[string]any{"type": "input_text", "text": "Keep the palette soft and the horizon low."},
					}},
				},
				"size":     "1536x1024",
				"n":        float64(2),
				"metadata": map[string]any{"trace": "abc123", "priority": float64(3), "draft": true, "nested": map[string]any{"ignored": true}},
			},
		},
	}
}

var (
	validArg    = hookArg("vendor-a")
	invalidArg  = hookArg("vendor-zzz")
	validJSON   = mustJSON(validArg)
	invalidJSON = mustJSON(invalidArg)
)

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func checkResult(tb testing.TB, out any) {
	m, ok := out.(map[string]any)
	if !ok || m["kind"] != "submit" || m["action"] != "image_to_image" {
		tb.Fatalf("unexpected hook result: %#v", out)
	}
}

// ---------------------------------------------------------------- Sobek

type sobekEngine struct {
	rt     *sobek.Runtime
	decode sobek.Callable
}

var sobekModule = sync.OnceValue(func() *sobek.SourceTextModuleRecord {
	m, err := sobek.ParseModule("plugin.js", pluginModule, func(any, string) (sobek.ModuleRecord, error) {
		return nil, errors.New("imports disabled")
	})
	if err != nil {
		panic(err)
	}
	if err := m.Link(); err != nil {
		panic(err)
	}
	return m
})

func newSobekRuntime() *sobek.Runtime {
	rt := sobek.New()
	_ = rt.Set("utils", map[string]any{"unixNow": func() int64 { return 1700000000 }, "uuid": func() string { return "u" }})
	console := rt.NewObject()
	_ = console.Set("log", func(sobek.FunctionCall) sobek.Value { return sobek.Undefined() })
	_ = rt.Set("console", console)
	return rt
}

func newSobekEngine(tb testing.TB) *sobekEngine {
	rt := newSobekRuntime()
	m := sobekModule()
	p := rt.CyclicModuleRecordEvaluate(m, func(any, string) (sobek.ModuleRecord, error) { return nil, errors.New("no imports") })
	if p.State() != sobek.PromiseStateFulfilled {
		tb.Fatalf("sobek evaluate: %v", p.Result())
	}
	fn, ok := sobek.AssertFunction(rt.GetModuleInstance(m).GetBindingValue("decode"))
	if !ok {
		tb.Fatal("decode is not callable")
	}
	return &sobekEngine{rt: rt, decode: fn}
}

func (e *sobekEngine) call(arg map[string]any) (any, error) {
	v, err := e.decode(sobek.Undefined(), e.rt.ToValue(arg))
	if err != nil {
		return nil, err
	}
	return v.Export(), nil
}

func BenchmarkNewRuntime_sobek(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		newSobekRuntime()
	}
}

func BenchmarkNewRuntimeAndInstantiate_sobek(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		newSobekEngine(b)
	}
}

func BenchmarkHook_sobek(b *testing.B) {
	e := newSobekEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := e.call(validArg)
		if err != nil {
			b.Fatal(err)
		}
		checkResult(b, out)
	}
}

func BenchmarkHookError_sobek(b *testing.B) {
	e := newSobekEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := e.call(invalidArg)
		var exc *sobek.Exception
		if !errors.As(err, &exc) || !strings.Contains(exc.Value().String(), "unsupported model") {
			b.Fatalf("expected JS error, got %v", err)
		}
	}
}

func BenchmarkHookParallel_sobek(b *testing.B) {
	pool := sync.Pool{New: func() any { return newSobekEngine(b) }}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		e := pool.Get().(*sobekEngine)
		defer pool.Put(e)
		for pb.Next() {
			out, err := e.call(validArg)
			if err != nil {
				b.Fatal(err)
			}
			checkResult(b, out)
		}
	})
}

// ---------------------------------------------------------------- quickjs-go (cgo)

type qjsEngine struct {
	rt     *quickjs.Runtime
	ctx    *quickjs.Context
	decode *quickjs.Value
}

func newQJSEngine(tb testing.TB) *qjsEngine {
	rt := quickjs.NewRuntime()
	ctx := rt.NewContext()
	utils := ctx.NewObject()
	utils.Set("unixNow", ctx.NewFunction(func(c *quickjs.Context, _ *quickjs.Value, _ []*quickjs.Value) *quickjs.Value {
		return c.NewInt64(1700000000)
	}))
	ctx.Globals().Set("utils", utils)
	console := ctx.NewObject()
	console.Set("log", ctx.NewFunction(func(c *quickjs.Context, _ *quickjs.Value, _ []*quickjs.Value) *quickjs.Value { return c.NewUndefined() }))
	ctx.Globals().Set("console", console)
	res := ctx.Eval(pluginScript, quickjs.EvalFileName("plugin.js"), quickjs.EvalFlagStrict(true))
	if res.IsException() {
		tb.Fatalf("quickjs eval: %v", ctx.Exception())
	}
	res.Free()
	fn := ctx.Globals().Get("decode")
	return &qjsEngine{rt: rt, ctx: ctx, decode: fn}
}

func (e *qjsEngine) close() {
	e.decode.Free()
	e.ctx.Close()
	e.rt.Close()
}

// call mirrors how quickjs-go users move JSON-shaped data: marshal in Go, parse
// in JS, stringify in JS, unmarshal in Go.
func (e *qjsEngine) call(arg map[string]any) (any, error) {
	encoded, err := json.Marshal(arg)
	if err != nil {
		return nil, err
	}
	jsArg := e.ctx.ParseJSON(string(encoded))
	defer jsArg.Free()
	res := e.decode.Execute(e.ctx.NewNull(), jsArg)
	defer res.Free()
	if res.IsException() {
		return nil, e.ctx.Exception()
	}
	var out any
	if err := json.Unmarshal([]byte(res.JSONStringify()), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func BenchmarkNewRuntime_quickjs(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		rt := quickjs.NewRuntime()
		ctx := rt.NewContext()
		ctx.Close()
		rt.Close()
	}
}

func BenchmarkNewRuntimeAndInstantiate_quickjs(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		newQJSEngine(b).close()
	}
}

func BenchmarkHook_quickjs(b *testing.B) {
	e := newQJSEngine(b)
	defer e.close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := e.call(validArg)
		if err != nil {
			b.Fatal(err)
		}
		checkResult(b, out)
	}
}

func BenchmarkHookError_quickjs(b *testing.B) {
	e := newQJSEngine(b)
	defer e.close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := e.call(invalidArg)
		if err == nil || !strings.Contains(err.Error(), "unsupported model") {
			b.Fatalf("expected JS error, got %v", err)
		}
	}
}

func BenchmarkHookParallel_quickjs(b *testing.B) {
	pool := sync.Pool{New: func() any { return newQJSEngine(b) }}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		e := pool.Get().(*qjsEngine)
		defer pool.Put(e)
		for pb.Next() {
			out, err := e.call(validArg)
			if err != nil {
				b.Fatal(err)
			}
			checkResult(b, out)
		}
	})
}

// ---------------------------------------------------------------- v8go (cgo)

type v8Engine struct {
	iso    *v8.Isolate
	ctx    *v8.Context
	decode *v8.Function
}

func newV8Engine(tb testing.TB) *v8Engine {
	iso := v8.NewIsolate()
	ctx := v8.NewContext(iso)
	if _, err := ctx.RunScript("globalThis.utils = {unixNow(){return 1700000000}}; globalThis.console = {log(){}};", "host.js"); err != nil {
		tb.Fatal(err)
	}
	if _, err := ctx.RunScript(pluginScript, "plugin.js"); err != nil {
		tb.Fatalf("v8 eval: %v", err)
	}
	val, err := ctx.Global().Get("decode")
	if err != nil {
		tb.Fatal(err)
	}
	fn, err := val.AsFunction()
	if err != nil {
		tb.Fatal(err)
	}
	return &v8Engine{iso: iso, ctx: ctx, decode: fn}
}

func (e *v8Engine) close() {
	e.ctx.Close()
	e.iso.Dispose()
}

func (e *v8Engine) call(arg map[string]any) (any, error) {
	encoded, err := json.Marshal(arg)
	if err != nil {
		return nil, err
	}
	jsArg, err := v8.JSONParse(e.ctx, string(encoded))
	if err != nil {
		return nil, err
	}
	res, err := e.decode.Call(v8.Undefined(e.iso), jsArg)
	if err != nil {
		return nil, err
	}
	s, err := v8.JSONStringify(e.ctx, res)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func BenchmarkNewRuntime_v8(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		iso := v8.NewIsolate()
		ctx := v8.NewContext(iso)
		ctx.Close()
		iso.Dispose()
	}
}

func BenchmarkNewRuntimeAndInstantiate_v8(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		newV8Engine(b).close()
	}
}

func BenchmarkHook_v8(b *testing.B) {
	e := newV8Engine(b)
	defer e.close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := e.call(validArg)
		if err != nil {
			b.Fatal(err)
		}
		checkResult(b, out)
	}
}

func BenchmarkHookError_v8(b *testing.B) {
	e := newV8Engine(b)
	defer e.close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := e.call(invalidArg)
		if err == nil || !strings.Contains(err.Error(), "unsupported model") {
			b.Fatalf("expected JS error, got %v", err)
		}
	}
}

func BenchmarkHookParallel_v8(b *testing.B) {
	if os.Getenv("V8_PARALLEL") == "" {
		b.Skip("set V8_PARALLEL=1 to run isolates from several goroutines")
	}
	pool := sync.Pool{New: func() any { return newV8Engine(b) }}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		e := pool.Get().(*v8Engine)
		defer pool.Put(e)
		for pb.Next() {
			out, err := e.call(validArg)
			if err != nil {
				b.Fatal(err)
			}
			checkResult(b, out)
		}
	})
}

// ---------------------------------------------------------------- footprint

func heapInuse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

func TestFootprint64Runtimes(t *testing.T) {
	const n = 64
	before := heapInuse()
	sobeks := make([]*sobekEngine, n)
	for i := range sobeks {
		sobeks[i] = newSobekEngine(t)
	}
	after := heapInuse()
	t.Logf("sobek: %d runtimes with module -> Go heap %.1f KiB per runtime", n, float64(after-before)/n/1024)
	runtime.KeepAlive(sobeks)

	before = heapInuse()
	qjs := make([]*qjsEngine, n)
	var cHeap uint64
	for i := range qjs {
		qjs[i] = newQJSEngine(t)
		cHeap += uint64(qjs[i].rt.MemoryUsage().MallocSize)
	}
	after = heapInuse()
	t.Logf("quickjs: %d runtimes with module -> C heap (malloc_size) %.1f KiB per runtime, Go heap %.1f KiB per runtime", n, float64(cHeap)/n/1024, float64(after-before)/n/1024)
	for _, e := range qjs {
		e.close()
	}

	before = heapInuse()
	v8s := make([]*v8Engine, n)
	var v8Heap uint64
	for i := range v8s {
		v8s[i] = newV8Engine(t)
		v8Heap += v8s[i].iso.GetHeapStatistics().TotalHeapSize
	}
	after = heapInuse()
	t.Logf("v8: %d isolates with module -> V8 heap (total_heap_size) %.1f KiB per isolate, Go heap %.1f KiB per isolate", n, float64(v8Heap)/n/1024, float64(after-before)/n/1024)
	for _, e := range v8s {
		e.close()
	}
	fmt.Fprintln(os.Stderr, "GOMAXPROCS", runtime.GOMAXPROCS(0))
}

func TestHookOutputsAgree(t *testing.T) {
	s := newSobekEngine(t)
	q := newQJSEngine(t)
	defer q.close()
	v := newV8Engine(t)
	defer v.close()
	so, err := s.call(validArg)
	if err != nil {
		t.Fatal(err)
	}
	qo, err := q.call(validArg)
	if err != nil {
		t.Fatal(err)
	}
	vo, err := v.call(validArg)
	if err != nil {
		t.Fatal(err)
	}
	// Sobek exports integral numbers as int64; normalize through JSON.
	if mustJSON(so) != mustJSON(qo) || mustJSON(qo) != mustJSON(vo) {
		t.Fatalf("engines disagree:\nsobek  %s\nquickjs %s\nv8     %s", mustJSON(so), mustJSON(qo), mustJSON(vo))
	}
}

// ---------------------------------------------------------------- moejs
//
// The same scratch hook through moejs's native API, so moejs can be read
// against the three baselines above: identical module, argument, validation
// and pooling.

type moejsEngine struct {
	rt     *moejs.Runtime
	decode moejs.Hook
}

var moejsModule = sync.OnceValue(func() *moejs.Module {
	m, err := moejs.Compile("plugin.js", pluginModule)
	if err != nil {
		panic(err)
	}
	return m
})

var moejsDecode = sync.OnceValue(func() moejs.Hook {
	h, err := moejsModule().Hook("decode")
	if err != nil {
		panic(err)
	}
	return h
})

func newMoejsRuntime() *moejs.Runtime {
	rt := moejs.NewRuntime(moejs.Options{})
	_ = rt.SetGlobal("utils", map[string]any{
		"unixNow": moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			return moejs.Int(1700000000), nil
		}),
		"uuid": moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			return moejs.String("u"), nil
		}),
	})
	_ = rt.SetGlobal("console", map[string]any{
		"log": moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			return moejs.Undefined(), nil
		}),
	})
	return rt
}

func newMoejsEngine(tb testing.TB) *moejsEngine {
	rt := newMoejsRuntime()
	if err := rt.Load(moejsModule()); err != nil {
		tb.Fatalf("moejs load: %v", err)
	}
	decode := moejsDecode()
	if ok, err := rt.Has(decode); !ok {
		tb.Fatalf("decode is not callable: %v", err)
	}
	return &moejsEngine{rt: rt, decode: decode}
}

func (e *moejsEngine) call(arg map[string]any) (any, error) {
	v, err := e.rt.FromGo(arg)
	if err != nil {
		return nil, err
	}
	res, err := e.rt.Call(e.decode, v)
	if err != nil {
		return nil, err
	}
	return e.rt.ToGo(res)
}

func BenchmarkNewRuntime_moejs(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		newMoejsRuntime()
	}
}

func BenchmarkNewRuntimeAndInstantiate_moejs(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		newMoejsEngine(b)
	}
}

func BenchmarkHook_moejs(b *testing.B) {
	e := newMoejsEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := e.call(validArg)
		if err != nil {
			b.Fatal(err)
		}
		checkResult(b, out)
	}
}

func BenchmarkHookError_moejs(b *testing.B) {
	e := newMoejsEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := e.call(invalidArg)
		var exc *moejs.Exception
		if !errors.As(err, &exc) || !strings.Contains(exc.Message(), "unsupported model") {
			b.Fatalf("expected JS error, got %v", err)
		}
	}
}

func BenchmarkHookParallel_moejs(b *testing.B) {
	pool := sync.Pool{New: func() any { return newMoejsEngine(b) }}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		e := pool.Get().(*moejsEngine)
		defer pool.Put(e)
		for pb.Next() {
			out, err := e.call(validArg)
			if err != nil {
				b.Fatal(err)
			}
			checkResult(b, out)
		}
	})
}

func TestFootprint64Runtimes_moejs(t *testing.T) {
	const n = 64
	before := heapInuse()
	rts := make([]*moejsEngine, n)
	for i := range rts {
		rts[i] = newMoejsEngine(t)
	}
	after := heapInuse()
	t.Logf("moejs: %d runtimes with module -> Go heap %.1f KiB per runtime", n, float64(after-before)/n/1024)
	runtime.KeepAlive(rts)
}

func TestHookOutputsAgree_moejs(t *testing.T) {
	s := newSobekEngine(t)
	m := newMoejsEngine(t)
	so, err := s.call(validArg)
	if err != nil {
		t.Fatal(err)
	}
	mo, err := m.call(validArg)
	if err != nil {
		t.Fatal(err)
	}
	if mustJSON(so) != mustJSON(mo) {
		t.Fatalf("engines disagree:\nsobek %s\nmoejs %s", mustJSON(so), mustJSON(mo))
	}
}

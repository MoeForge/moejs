package engine

import (
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
)

// benchModule compiles and evaluates src in a fresh realm and returns the
// realm and the named export.
func benchModule(b *testing.B, src, name string) (*Realm, Value) {
	b.Helper()
	m, err := syntax.ParseModule("b.js", src, syntax.Options{})
	if err != nil {
		b.Fatal(err)
	}
	code, err := compiler.CompileModule(m)
	if err != nil {
		b.Fatal(err)
	}
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	if err != nil {
		b.Fatal(err)
	}
	v, ok := env.GetBindingValue(name)
	if !ok {
		b.Fatalf("no export %q", name)
	}
	return r, v
}

func BenchmarkCall(b *testing.B) {
	r, fn := benchModule(b, `export function empty() {}`, "empty")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCallJSToJS(b *testing.B) {
	r, fn := benchModule(b, `function empty(a) { return a; } export function loop(n) { let s = 0; for (let i = 0; i < n; i++) s += empty(i); return s; }`, "loop")
	n := []Value{IntValue(1000)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPropertyGetMonomorphic(b *testing.B) {
	r, fn := benchModule(b, `export function get(o) { return o.model; }`, "get")
	obj, err := r.FromGo(map[string]any{"model": "m", "prompt": "p", "size": 1})
	if err != nil {
		b.Fatal(err)
	}
	args := []Value{obj}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.Call(fn, Undefined(), args)
		if err != nil || !v.IsString() {
			b.Fatal("bad get")
		}
	}
}

func BenchmarkPropertyGetPrototype(b *testing.B) {
	r, fn := benchModule(b, `function P() {} P.prototype.k = 1; const o = new P(); export function get() { return o.k; }`, "get")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.Call(fn, Undefined(), nil)
		if err != nil || !v.IsNumber() {
			b.Fatal("bad get")
		}
	}
}

func BenchmarkPropertySet(b *testing.B) {
	r, fn := benchModule(b, `const o = { n: 0 }; export function set(v) { o.n = v; }`, "set")
	args := []Value{IntValue(1)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), args); err != nil {
			b.Fatal(err)
		}
	}
}

// benchAccessorLoop runs fn(100), a loop of 100 accessor accesses.
func benchAccessorLoop(b *testing.B, src string) {
	r, fn := benchModule(b, src, "run")
	args := []Value{IntValue(100)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAccessorGetOwn(b *testing.B) {
	benchAccessorLoop(b, `const o = { v: 1, get x() { return this.v; } };
export function run(n) { let s = 0; for (let i = 0; i < n; i++) s += o.x; return s; }`)
}

func BenchmarkAccessorGetPrototype(b *testing.B) {
	benchAccessorLoop(b, `const p = { v: 1, get x() { return this.v; } }; const o = Object.create(p);
export function run(n) { let s = 0; for (let i = 0; i < n; i++) s += o.x; return s; }`)
}

func BenchmarkAccessorSetOwn(b *testing.B) {
	benchAccessorLoop(b, `const o = { v: 0, set x(v) { this.v = v; } };
export function run(n) { for (let i = 0; i < n; i++) o.x = i; }`)
}

func BenchmarkLoopSum(b *testing.B) {
	r, fn := benchModule(b, `export function sum(n) { let s = 0; for (let i = 0; i < n; i++) { s += i; } return s; }`, "sum")
	args := []Value{IntValue(100000)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.Call(fn, Undefined(), args)
		if err != nil || v.AsNumber() != 4999950000 {
			b.Fatal("bad sum")
		}
	}
}

func BenchmarkClosureCall(b *testing.B) {
	r, fn := benchModule(b, `let n = 0; const inc = () => { n++; return n; }; export function call() { return inc(); }`, "call")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClosureCreate(b *testing.B) {
	r, fn := benchModule(b, `export function mk(x) { return () => x; }`, "mk")
	args := []Value{IntValue(1)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkForOfArray(b *testing.B) {
	r, fn := benchModule(b, `export function sum(a) { let s = 0; for (const x of a) s += x; return s; }`, "sum")
	items := make([]Value, 1000)
	for i := range items {
		items[i] = IntValue(i)
	}
	args := []Value{ObjectValue(r.NewArrayFromSlice(items))}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		v, err := r.Call(fn, Undefined(), args)
		if err != nil || v.AsNumber() != 499500 {
			b.Fatal("bad sum")
		}
	}
}

func BenchmarkTryCatchThrow(b *testing.B) {
	r, fn := benchModule(b, `export function tc() { try { throw new Error("x"); } catch (e) { return e.message; } }`, "tc")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTryCatchNoThrow(b *testing.B) {
	r, fn := benchModule(b, `export function tc(x) { try { return x + 1; } catch (e) { return -1; } }`, "tc")
	args := []Value{IntValue(1)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), args); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStringConcatLoop(b *testing.B) {
	r, fn := benchModule(b, `export function cat(n) { let s = ""; for (let i = 0; i < n; i++) s += "ab"; return s.length; }`, "cat")
	args := []Value{IntValue(200)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), args); err != nil {
			b.Fatal(err)
		}
	}
}

// pluginLikeModule is a ~120-line module in the style of a new-api task
// plugin (validation helpers, request builders, response parsers).
const pluginLikeModule = `
const DEFAULT_MODEL = "video-gen-1";
const SIZES = ["1280x720", "720x1280", "1024x1024"];
const MAX_DURATION = 10;

function isString(v) { return typeof v === "string"; }
function isNumber(v) { return typeof v === "number" && v === v; }
function isObject(v) { return v !== null && typeof v === "object"; }

function fail(code, message) {
  const e = new Error(message);
  e.code = code;
  throw e;
}

function pick(obj, keys) {
  const out = {};
  for (const k of keys) {
    if (obj[k] !== undefined) out[k] = obj[k];
  }
  return out;
}

function normalizeSize(size) {
  if (!isString(size)) return SIZES[0];
  for (const s of SIZES) {
    if (s === size) return s;
  }
  const parts = size.split ? size : size;
  return SIZES[0];
}

function normalizeDuration(d) {
  if (!isNumber(d)) return 5;
  if (d < 1) return 1;
  if (d > MAX_DURATION) return MAX_DURATION;
  return d;
}

export function validateRequest(req) {
  if (!isObject(req)) fail("invalid_request", "request must be an object");
  const { model = DEFAULT_MODEL, prompt, size, duration, seed, images = [] } = req;
  if (!isString(prompt) || prompt.length === 0) fail("invalid_prompt", "prompt is required");
  if (images.length > 4) fail("too_many_images", "at most 4 images");
  return {
    model,
    prompt,
    size: normalizeSize(size),
    duration: normalizeDuration(duration),
    seed: isNumber(seed) ? seed : null,
    images,
  };
}

export function buildSubmitRequest(ctx) {
  const body = validateRequest(ctx.requestBody);
  const headers = { "Content-Type": "application/json" };
  if (ctx.apiKey) headers["Authorization"] = "Bearer " + ctx.apiKey;
  const input = { prompt: body.prompt, size: body.size, duration: body.duration };
  if (body.seed !== null) input.seed = body.seed;
  if (body.images.length > 0) {
    const refs = [];
    for (let i = 0; i < body.images.length; i++) {
      const img = body.images[i];
      refs[refs.length] = isString(img) ? { url: img } : pick(img, ["url", "weight"]);
    }
    input.references = refs;
  }
  return {
    url: (ctx.baseUrl || "https://api.example.com") + "/v1/videos",
    method: "POST",
    headers,
    body: { model: body.model, input, metadata: { user: ctx.userId || "anonymous" } },
  };
}

export function parseSubmitResponse(resp) {
  if (!isObject(resp)) fail("bad_response", "response is not an object");
  const id = resp.id || (resp.data && resp.data.task_id);
  if (!isString(id)) fail("bad_response", "missing task id");
  return { taskId: id, status: resp.status || "queued" };
}

const STATUS_MAP = { queued: "QUEUED", processing: "IN_PROGRESS", succeeded: "SUCCESS", failed: "FAILURE" };

export function parseTaskResult(resp) {
  const status = STATUS_MAP[resp.status] || "UNKNOWN";
  const result = { status, progress: 0 };
  switch (status) {
    case "QUEUED":
      result.progress = 0;
      break;
    case "IN_PROGRESS":
      result.progress = isNumber(resp.progress) ? resp.progress : 50;
      break;
    case "SUCCESS":
      result.progress = 100;
      result.url = resp.output?.video?.url ?? resp.output?.url ?? null;
      break;
    case "FAILURE":
      result.error = resp.error?.message ?? "unknown error";
      break;
  }
  return result;
}

export function extractUsage(resp) {
  const seconds = resp?.usage?.seconds ?? 0;
  const frames = resp?.usage?.frames ?? 0;
  return { seconds, frames, total: seconds * 30 + frames };
}
`

func BenchmarkModuleInstantiate(b *testing.B) {
	m, err := syntax.ParseModule("plugin.js", pluginLikeModule, syntax.Options{})
	if err != nil {
		b.Fatal(err)
	}
	code, err := compiler.CompileModule(m)
	if err != nil {
		b.Fatal(err)
	}
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	if _, err := r.EvaluateModule(code); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
		if _, err := r.EvaluateModule(code); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkModuleCompile(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		m, err := syntax.ParseModule("plugin.js", pluginLikeModule, syntax.Options{})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := compiler.CompileModule(m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPluginHook(b *testing.B) {
	r, fn := benchModule(b, pluginLikeModule, "buildSubmitRequest")
	ctx, err := r.FromGo(map[string]any{
		"apiKey":  "sk-test",
		"baseUrl": "https://example.test",
		"userId":  "u1",
		"requestBody": map[string]any{
			"model":    "video-gen-1",
			"prompt":   "a cat surfing",
			"size":     "1280x720",
			"duration": 8,
			"seed":     42,
			"images":   []any{"https://x/1.png", map[string]any{"url": "https://x/2.png", "weight": 0.5}},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	args := []Value{ctx}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Call(fn, Undefined(), args); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStringMethodsJS measures method calls on string primitives from
// JavaScript (String.prototype lookups through the inline cache).
func BenchmarkStringMethodsJS(b *testing.B) {
	r, fn := benchModule(b, `export function f(s) { return s.trim().toLowerCase().startsWith("data:") && s.includes("base64") && s.indexOf(",") > 0; }`, "f")
	args := []Value{StringValue(FromGoString("data:image/png;base64,iVBORw0KGgo"))}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if v, err := r.Call(fn, Undefined(), args); err != nil || !v.IsTrue() {
			b.Fatal("unexpected result", v, err)
		}
	}
}

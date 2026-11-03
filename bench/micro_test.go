package bench

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bench/engines"
)

// microIterations is the inner loop count of every micro program: one
// benchmark op is one hook call that performs this many operations, so the
// call overhead is amortized and identical for every engine.
const microIterations = 100

// microJSON is a ~10 KiB JSON document shaped like a plugin task payload.
var microJSON = func() string {
	items := make([]any, 0, 40)
	for i := range 40 {
		items = append(items, map[string]any{
			"id": fmt.Sprintf("item-%03d", i), "index": i, "ratio": float64(i) / 7, "active": i%3 == 0,
			"tags":   []any{"alpha", "beta", fmt.Sprintf("tag-%d", i%5)},
			"nested": map[string]any{"url": fmt.Sprintf("https://cdn.example.com/video/%03d.mp4?sig=abcdef%d", i, i), "size": "1280x720", "meta": map[string]any{"a": 1, "b": nil, "c": "日本語"}},
		})
	}
	data, _ := json.Marshal(map[string]any{"task_id": "task_pub_0001", "status": "SUCCESS", "progress": "100%", "items": items, "usage": map[string]any{"tokens": 108000, "seconds": 5}})
	return string(data)
}()

// microModule holds the micro programs; each export runs microIterations
// iterations and returns a checksum the harness compares with Sobek's.
var microModule = strings.ReplaceAll(`
const N = __N__;
const DOC = __DOC__;
const PARSED = JSON.parse(DOC);
const MONO = { a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7, h: 8 };
const POLY = [{ x: 1, y: 0 }, { y: 0, x: 2 }, { z: 0, x: 3 }, { x: 4, w: 0, v: 0 }];
const KEYS = {}; for (let i = 0; i < 20; i++) KEYS["key" + i] = i;
const TEXT = "  A watercolor painting of   a lighthouse at dusk,\nwith seagulls   circling and warm light spilling onto the rocks. ";
function add(a, b) { return a + b; }
export function propGetMono() { let s = 0; for (let i = 0; i < N; i++) s += MONO.a + MONO.d + MONO.h + MONO.c; return s; }
export function propGetPoly() { let s = 0; for (let i = 0; i < N; i++) s += POLY[i & 3].x; return s; }
export function call() { let s = 0; for (let i = 0; i < N; i++) s = add(s, i); return s; }
export function closure() { let s = 0; for (let i = 0; i < N; i++) { const f = (k) => k + i; s += f(1); } return s; }
export function arrayPushForOf() { const a = []; for (let i = 0; i < N; i++) a.push(i * 2); let s = 0; for (const v of a) s += v; return s + a.length; }
export function stringConcat() { let s = ""; for (let i = 0; i < N; i++) s += "part" + i + ","; return s.length; }
export function stringOps() { let n = 0; for (let i = 0; i < N; i++) { const t = TEXT.replace(/\s+/g, " ").trim(); n += t.split(" ").length + t.toLowerCase().indexOf("lighthouse") + (t.startsWith("A") ? 1 : 0); } return n; }
export function jsonParse() { let n = 0; for (let i = 0; i < N; i++) n += JSON.parse(DOC).items.length; return n; }
export function jsonStringify() { let n = 0; for (let i = 0; i < N; i++) n += JSON.stringify(PARSED).length; return n; }
export function objectKeysAssign() { let n = 0; for (let i = 0; i < N; i++) { const o = Object.assign({}, KEYS, { extra: i }); n += Object.keys(o).length; } return n; }
export function regexTestReplace() { let n = 0; for (let i = 0; i < N; i++) { if (/^https?:\/\//i.test("https://cdn.example.com/v.mp4?a=1&b=<x>")) n++; n += "https://x/<a>&\"b\"".replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;").length; } return n; }
export function errorThrowCatch() { let n = 0; for (let i = 0; i < N; i++) { try { throw new Error("hook failed " + i); } catch (e) { n += e.message.length; } } return n; }
export function identity(v) { return v; }
function walk(v) { if (Array.isArray(v)) { let n = v.length; for (const x of v) n += walk(x); return n; } if (v && typeof v === "object") { let n = 0; for (const k of Object.keys(v)) n += k.length + walk(v[k]); return n; } if (typeof v === "string") return v.length; if (typeof v === "number") return 1; return 0; }
export function traverse(v) { return walk(v); }
`, "__N__", fmt.Sprint(microIterations))

var microNames = []string{"propGetMono", "propGetPoly", "call", "closure", "arrayPushForOf", "stringConcat", "stringOps", "jsonParse", "jsonStringify", "objectKeysAssign", "regexTestReplace", "errorThrowCatch"}

func microSource() string {
	doc, _ := json.Marshal(microJSON)
	return strings.Replace(microModule, "__DOC__", string(doc), 1)
}

func microRuntime(tb testing.TB, e engines.Engine) engines.Runtime {
	mod, err := e.Compile("micro.js", microSource())
	if err != nil {
		tb.Fatal(err)
	}
	rt, err := e.NewRuntime()
	if err != nil {
		tb.Fatal(err)
	}
	if err := rt.Instantiate(mod); err != nil {
		tb.Fatal(err)
	}
	return rt
}

// BenchmarkMicro compares moejs with Sobek on the micro workloads. Every op
// is one call running microIterations inner iterations; the checksum is
// validated against Sobek's on every iteration.
func BenchmarkMicro(b *testing.B) {
	oracle := microRuntime(b, engines.NewSobekEngine())
	defer oracle.Close()
	expected := map[string]any{}
	for _, name := range microNames {
		out, err := oracle.Call(name, nil)
		if err != nil {
			b.Fatalf("sobek %s: %v", name, err)
		}
		expected[name], _ = Normalize(out)
	}
	for _, e := range engines.PureGo() {
		rt := microRuntime(b, e)
		for _, name := range microNames {
			want := expected[name]
			b.Run(name+"/"+e.Name(), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					out, err := rt.Call(name, nil)
					if err != nil {
						b.Fatal(err)
					}
					got, _ := Normalize(out)
					if got != want {
						b.Fatalf("%s: expected %v, got %v", name, want, got)
					}
				}
			})
		}
		rt.Close()
	}
}

// BenchmarkMicroHostRoundTrip measures host conversion of a ~10 KiB
// JSON-shaped value (one call each):
//
//   - identity: ToValue + Export through `v => v`. Sobek wraps Go maps and
//     slices as live proxies and Export hands the original back, so this is
//     nearly free for Sobek and a full snapshot copy for moejs.
//   - traverse: ToValue + a JavaScript walk over every key and value + Export
//     of the checksum, which is what plugin code actually does with its
//     argument: Sobek pays reflection per property read, moejs paid up front.
func BenchmarkMicroHostRoundTrip(b *testing.B) {
	var value any
	if err := json.Unmarshal([]byte(microJSON), &value); err != nil {
		b.Fatal(err)
	}
	oracle := microRuntime(b, engines.NewSobekEngine())
	want, err := oracle.Call("traverse", nil, value)
	oracle.Close()
	if err != nil {
		b.Fatal(err)
	}
	wantSum, _ := Normalize(want)
	for _, e := range engines.PureGo() {
		rt := microRuntime(b, e)
		b.Run("identity/"+e.Name(), func(b *testing.B) {
			b.SetBytes(int64(len(microJSON)))
			b.ReportAllocs()
			for range b.N {
				out, err := rt.Call("identity", nil, value)
				if err != nil {
					b.Fatal(err)
				}
				if m, ok := out.(map[string]any); !ok || len(m) != 5 {
					b.Fatalf("unexpected result %T", out)
				}
			}
		})
		b.Run("traverse/"+e.Name(), func(b *testing.B) {
			b.SetBytes(int64(len(microJSON)))
			b.ReportAllocs()
			for range b.N {
				out, err := rt.Call("traverse", nil, value)
				if err != nil {
					b.Fatal(err)
				}
				if got, _ := Normalize(out); got != wantSum {
					b.Fatalf("checksum %v, want %v", got, wantSum)
				}
			}
		})
		rt.Close()
	}
}

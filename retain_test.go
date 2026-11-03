package moejs_test

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pooled runtime runs request after request for its lifetime: what the
// requests made and dropped must not accumulate in it.

// TestFreshPrototypesRetained: objects whose prototype the program created
// and dropped retain nothing; like TestDynamicCodeRetained, it compares the
// heap a runtime retains after ten times the work (the audit's
// TestAuditFxRootShapes: 670 bytes per `new F()` with a fresh F, kept for the
// runtime's lifetime).
func TestFreshPrototypesRetained(t *testing.T) {
	cases := []struct{ name, src string }{
		{"constructor per iteration", `for (var i = 0; i < N; i++) { function F() { this.a = 1; } new F(); }`},
		{"class per iteration", `for (var i = 0; i < N; i++) { class C { constructor() { this.a = 1; } } new C(); }`},
		{"derived class with fields per iteration", `for (var i = 0; i < N; i++) { class C { a = 1; } class D extends C { b = 2; } new D(); }`},
		{"Object.create of a fresh prototype", `for (var i = 0; i < N; i++) { var p = {}; var o = Object.create(p); o.a = 1; }`},
		{"Object.setPrototypeOf to a fresh prototype", `for (var i = 0; i < N; i++) Object.setPrototypeOf({}, {}).a = 1;`},
		{"Function constructor per iteration", `for (var i = 0; i < N; i++) new (Function("this.a = 1"))();`},
		{"generator function per iteration", `for (var i = 0; i < N; i++) { function* g() { yield 1; } g().next(); }`},
		{"async generator function per iteration", `for (var i = 0; i < N; i++) { async function* g() { yield 1; } g().next(); }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			small := scriptRetained(t, tc.src, 2000)
			large := scriptRetained(t, tc.src, 20000)
			t.Logf("retained %.1f KiB after 2,000, %.1f KiB after 20,000", float64(small)/1024, float64(large)/1024)
			assert.Less(t, large, 2*small+256<<10, "the retained heap grows with the prototypes made")
		})
	}
}

func scriptRetained(t *testing.T, src string, n int) uint64 {
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("N", n))
	s := mustScript(t, src)
	before := heapAfterGC()
	_, err := rt.RunScript(s)
	require.NoError(t, err)
	after := heapAfterGC()
	runtime.KeepAlive(rt)
	if after < before {
		return 0
	}
	return after - before
}

// TestRequestsRetained: requests shaped like a gateway's (host maps keyed by
// ids, objects indexed with request values, ReleaseCallData after each) leave
// a bounded state in the runtime: the names they intern, the key sets of
// their host maps and the shapes of their objects (the audit's
// TestAuditInternGrowth). The name and key-set caches fill to a few MiB and
// then start over (internCacheMax, hostShapesMax, hostShapeKeysMax), so the
// retained heap oscillates below a fixed bound rather than following the
// request count; without the bounds or the weak transitions (strongChild),
// 20,000 such requests retained 25 to 90 MiB, and with the key sets bounded
// by their count alone, the 40-id maps kept about 60 MiB.
func TestRequestsRetained(t *testing.T) {
	// The measurement reads the whole process's heap, to which the state
	// the package's other tests leave behind (the name and key-set caches,
	// shared shapes) adds a few MiB at moments that depend on the order the
	// tests ran in, so it runs alone in a fresh process.
	if os.Getenv("MOEJS_RETAIN_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRequestsRetained$", "-test.v")
		cmd.Env = append(os.Environ(), "MOEJS_RETAIN_CHILD=1")
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		require.NoError(t, err, "the measurement in a fresh process failed")
		return
	}
	ids := func(i, n int) map[string]any {
		m := make(map[string]any, n)
		for j := range n {
			m["id_"+strconv.Itoa(i)+"_"+strconv.Itoa(j)] = map[string]any{"n": float64(j), "model": "gpt"}
		}
		return m
	}
	const byID = `
export function hook(req) {
	const out = {};
	let s = 0;
	for (const id of Object.keys(req.byId)) { out[id] = req.byId[id].n; s += out[id]; }
	const tally = {};
	tally[req.user] = (tally[req.user] || 0) + 1;
	tally[req.trace] = s;
	return Object.keys(out).length + Object.keys(tally).length;
}`
	byIDArg := func(n int) func(i int) any {
		return func(i int) any {
			return map[string]any{"byId": ids(i, n), "user": "user_" + strconv.Itoa(i), "trace": "трасса_" + strconv.Itoa(i)}
		}
	}
	cases := []struct {
		name, src string
		arg       func(i int) any
	}{
		{"keys of a host map", `export function hook(x) { return Object.keys(x).length; }`,
			func(i int) any { return ids(i, 8) }},
		{"objects keyed by ids and user values", byID, byIDArg(8)},
		{"objects keyed by 40 ids and user values", byID, byIDArg(40)},
		{"JSON bodies keyed by ids", `
export function hook(req) {
	const o = JSON.parse(req.body);
	const out = {};
	for (const k in o.usage) out[k] = o.usage[k] * 2;
	out[req.user] = 1;
	return Object.keys(out).length;
}`, func(i int) any {
			n := strconv.Itoa(i)
			return map[string]any{"body": `{"usage":{"k` + n + `_0":0,"k` + n + `_1":1,"k` + n + `_2":2}}`, "user": "u" + n}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := moejs.Compile("hook.js", tc.src)
			require.NoError(t, err)
			h, err := m.Hook("hook")
			require.NoError(t, err)
			measure := func(n int) uint64 {
				rt := moejs.NewRuntime(moejs.Options{})
				require.NoError(t, rt.Load(m))
				call := func(i int) {
					x, err := rt.FromGo(tc.arg(i))
					require.NoError(t, err)
					_, err = rt.Call(h, x)
					require.NoError(t, err)
					rt.ReleaseCallData()
				}
				for i := range 200 {
					call(i)
				}
				before := heapAfterGC()
				for i := 200; i < 200+n; i++ {
					call(i)
				}
				after := heapAfterGC()
				runtime.KeepAlive(rt)
				if after < before {
					return 0
				}
				return after - before
			}
			retained := measure(20000)
			t.Logf("retained %.1f KiB after 20,000 requests", float64(retained)/1024)
			assert.Less(t, retained, uint64(8<<20), "the retained heap grows with the requests")
		})
	}
}

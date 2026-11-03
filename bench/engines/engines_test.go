package engines_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/bench/engines"
	"github.com/grafana/sobek"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const smokePlugin = `
export const meta = { key: "smoke", n: 1 };
function trimmed(v) { return String(v || "").trim(); }
export function build(ctx) {
  const t = utils.jwtSignHS256({ iss: "a", exp: utils.unixNow() + 1800 }, "secret");
  return { url: ctx.baseUrl + "/x", token: t, id: utils.uuid(), b64: utils.base64URL("hi"),
    back: utils.base64URLDecode(utils.base64URL("héllo")), mac: utils.hmacSHA256("m", "k"),
    cap: utils.hasCapability("json-clone@1"), clone: utils.json.clone(ctx.nested), sig: utils.volcSignV4({}).Authorization,
    prompt: trimmed(ctx.prompt).replace(/\s+/g, " ") };
}
export const protocols = { openai_responses: { decodeRequest: function (ctx) {
  if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
  return { kind: "submit", model: ctx.model, n: ctx.body.value.n * 2 };
} } };
export const native = { decodeSubmit: function (ctx) { throw new TypeError("nope " + ctx.x); } };
`

func normalize(t *testing.T, v any) any {
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var out any
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

func TestAdaptersAgreeOnSmokePlugin(t *testing.T) {
	ctx := map[string]any{"baseUrl": "https://api.example.com", "prompt": "  a   b\nc ", "nested": map[string]any{"k": []any{1.0, "x", nil}}}
	decodeCtx := map[string]any{"model": "m", "body": map[string]any{"kind": "json", "value": map[string]any{"n": 21.0}}}
	var reference any
	for _, e := range engines.All() {
		t.Run(e.Name(), func(t *testing.T) {
			mod, err := e.Compile("smoke.js", smokePlugin)
			require.NoError(t, err)
			rt, err := e.NewRuntime()
			require.NoError(t, err)
			defer rt.Close()
			require.NoError(t, rt.Instantiate(mod))

			out, err := rt.Call("build", nil, ctx)
			require.NoError(t, err)
			got := normalize(t, out)
			if reference == nil {
				reference = got
			} else {
				assert.Equal(t, reference, got)
			}
			m := got.(map[string]any)
			assert.Equal(t, "a b c", m["prompt"])
			assert.Equal(t, map[string]any{"k": []any{1.0, "x", nil}}, m["clone"])

			nested, err := rt.Call("protocols", []string{"openai_responses", "decodeRequest"}, decodeCtx)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"kind": "submit", "model": "m", "n": 42.0}, normalize(t, nested))

			_, err = rt.Call("protocols", []string{"openai_responses", "decodeRequest"}, map[string]any{"model": "m"})
			he, ok := engines.AsHookError(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, "JSON body required", he.Message)
			assert.Equal(t, "Error", he.Name)

			_, err = rt.Call("native", []string{"decodeSubmit"}, map[string]any{"x": 1.0})
			he, ok = engines.AsHookError(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, "nope 1", he.Message)
			assert.Equal(t, "TypeError", he.Name)

			_, err = rt.Call("protocols", []string{"missing", "decodeRequest"}, decodeCtx)
			assert.ErrorIs(t, err, engines.ErrNotFound)
			_, err = rt.Call("meta", nil)
			assert.ErrorIs(t, err, engines.ErrNotFound)
		})
	}
}

func TestAdaptersExportPromises(t *testing.T) {
	const src = `
export function fulfilled() { return Promise.resolve(1).then(v => ({then(r) { r({v: v + 1}); }})); }
export function rejected() { return Promise.reject("no"); }
export function pending() { return new Promise(() => {}); }
`
	want := map[string]any{
		"fulfilled": map[string]any{"state": "fulfilled", "value": map[string]any{"v": 2.0}},
		"rejected":  map[string]any{"state": "rejected", "value": "no"},
		"pending":   map[string]any{"state": "pending", "value": nil},
	}
	for _, e := range engines.PureGo() {
		t.Run(e.Name(), func(t *testing.T) {
			mod, err := e.Compile("promise.js", src)
			require.NoError(t, err)
			rt, err := e.NewRuntime()
			require.NoError(t, err)
			defer rt.Close()
			require.NoError(t, rt.Instantiate(mod))
			for name, w := range want {
				out, err := rt.Call(name, nil)
				require.NoError(t, err, name)
				assert.Equal(t, w, normalize(t, out), name)
			}
		})
	}
}

// promiseDriver runs a promiseCase on one engine's promise host API. Its log
// gets the log(s) calls of the scripts, "R<id>" and "H<id>" for the rejection
// tracker's Reject and Handle, and "P<id> <state> <result>" for a promise a
// script returned, a module evaluation or a host promise at the end, ids by
// first sight of each promise (hostPromise() creates one from Go with
// NewPromise). reenter() logs "reenter <found> <x>" for the evaluated
// module's instance and its export x and evaluates the module again.
type promiseDriver interface {
	run(fn int)                        // runs script fn as the outermost call
	module(src string, interrupt bool) // evaluates src as the module, interrupted first if interrupt
	settle(h int, reject bool, v any)  // settles host promise h from Go; v may be a hostRef
	final() []string                   // the log, with the host promises' state
}

type hostRef int // host promise i, as a value to settle another with

type keptRef int // the i-th value a script passed to keep

type promiseLog struct {
	ids   map[any]int
	lines []string
	// While collect is set, tracker calls are held instead of logged (see
	// keepTracks).
	collect bool
	held    []heldTrack
	kept    []any // what keep(v) got, for keptRef
}

type heldTrack struct {
	at     int // len(lines) at the call
	p      any
	reject bool
}

func (l *promiseLog) id(p any) int {
	if l.ids == nil {
		l.ids = map[any]int{}
	}
	if _, ok := l.ids[p]; !ok {
		l.ids[p] = len(l.ids)
	}
	return l.ids[p]
}

func (l *promiseLog) track(p any, reject bool) {
	if l.collect {
		l.held = append(l.held, heldTrack{len(l.lines), p, reject})
		return
	}
	op := "H"
	if reject {
		op = "R"
	}
	l.lines = append(l.lines, fmt.Sprintf("%s%d", op, l.id(p)))
}

func (l *promiseLog) promise(p any, state int, result any) {
	l.lines = append(l.lines, fmt.Sprintf("P%d %d %v", l.id(p), state, result))
}

// keepTracks logs the tracker calls held since the log had start lines that
// name p, where they were made, and drops the others.
func (l *promiseLog) keepTracks(start int, p any) {
	rest := slices.Clone(l.lines[start:])
	l.lines, l.collect = l.lines[:start], false
	held := l.held
	l.held = nil
	for i := 0; i <= len(rest); i++ {
		for _, t := range held {
			if t.at == start+i && t.p == p {
				l.track(p, t.reject)
			}
		}
		if i < len(rest) {
			l.lines = append(l.lines, rest[i])
		}
	}
}

type sobekPromiseDriver struct {
	promiseLog
	rt       *sobek.Runtime
	fns      []string
	hosts    []*sobek.Promise
	settlers [][2]func(any) error
	mod      *sobek.SourceTextModuleRecord
}

func newSobekPromiseDriver(fns []string) promiseDriver {
	d := &sobekPromiseDriver{rt: sobek.New(), fns: fns}
	d.rt.SetPromiseRejectionTracker(func(p *sobek.Promise, op sobek.PromiseRejectionOperation) {
		d.track(p, op == sobek.PromiseRejectionReject)
	})
	_ = d.rt.Set("log", func(s string) { d.lines = append(d.lines, s) })
	_ = d.rt.Set("keep", func(v sobek.Value) { d.kept = append(d.kept, v) })
	_ = d.rt.Set("hostPromise", func() *sobek.Promise {
		p, resolve, reject := d.rt.NewPromise()
		d.id(p)
		d.hosts, d.settlers = append(d.hosts, p), append(d.settlers, [2]func(any) error{resolve, reject})
		return p
	})
	_ = d.rt.Set("reenter", func() {
		inst, x := d.rt.GetModuleInstance(d.mod), "undefined"
		if inst != nil {
			x = inst.GetBindingValue("x").String()
		}
		d.rt.CyclicModuleRecordEvaluate(d.mod, nil)
		d.lines = append(d.lines, fmt.Sprintf("reenter %v %s", inst != nil, x))
	})
	return d
}

func (d *sobekPromiseDriver) run(fn int) {
	v, err := d.rt.RunString("(function () {" + d.fns[fn] + "\n})()")
	if err != nil {
		d.lines = append(d.lines, "error")
		return
	}
	if p, ok := v.Export().(*sobek.Promise); ok {
		d.report(p)
	}
}

// module logs only the tracker calls for the promise the host gets: Sobek's
// evaluation also rejects an inner promise, and handles it unless
// interrupted.
func (d *sobekPromiseDriver) module(src string, interrupt bool) {
	var err error
	if d.mod, err = sobek.ParseModule("m.js", src, nil); err != nil {
		panic(err)
	}
	_ = d.mod.Link()
	if interrupt {
		d.rt.Interrupt("halt")
	}
	start := len(d.lines)
	d.collect = true
	p := d.rt.CyclicModuleRecordEvaluate(d.mod, nil)
	d.rt.ClearInterrupt()
	d.keepTracks(start, p)
	var result any
	switch e := p.Result().Export().(type) {
	case *sobek.Exception:
		result = e.Value().ToObject(d.rt).Get("message")
	case *sobek.InterruptedError:
		result = e.Value()
	}
	d.promise(p, int(p.State()), result)
}

func (d *sobekPromiseDriver) report(p *sobek.Promise) {
	var result any
	if p.Result() != nil {
		result = p.Result().Export()
	}
	d.promise(p, int(p.State()), result)
}

func (d *sobekPromiseDriver) settle(h int, reject bool, v any) {
	switch r := v.(type) {
	case hostRef:
		v = d.hosts[r]
	case keptRef:
		v = d.kept[r]
	}
	if err := d.settlers[h][boolIndex(reject)](v); err != nil {
		d.lines = append(d.lines, "error")
	}
}

func (d *sobekPromiseDriver) final() []string {
	for _, p := range d.hosts {
		d.report(p)
	}
	return d.lines
}

type moejsPromiseDriver struct {
	promiseLog
	rt       *moejs.Runtime
	fns      []string
	mod      *moejs.Module
	hosts    []moejs.Value
	settlers [][2]func(moejs.Value) error
}

func newMoejsPromiseDriver(fns []string) promiseDriver {
	d := &moejsPromiseDriver{rt: moejs.NewRuntime(moejs.Options{}), fns: fns}
	d.rt.SetPromiseRejectionTracker(func(p moejs.Value, op moejs.PromiseRejectionOperation) {
		d.track(p.AsObject(), op == moejs.PromiseRejectionReject)
	})
	_ = d.rt.SetGlobal("log", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		d.lines = append(d.lines, r.ToGo(moejs.Arg(args, 0)).(string))
		return moejs.Undefined(), nil
	}))
	_ = d.rt.SetGlobal("keep", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		d.kept = append(d.kept, moejs.Arg(args, 0))
		return moejs.Undefined(), nil
	}))
	_ = d.rt.SetGlobal("hostPromise", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		p, resolve, reject := d.rt.NewPromise()
		d.id(p.AsObject())
		d.hosts, d.settlers = append(d.hosts, p), append(d.settlers, [2]func(moejs.Value) error{resolve, reject})
		return p, nil
	}))
	_ = d.rt.SetGlobal("reenter", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		x, ok := d.rt.Export("x")
		_ = d.rt.Load(d.mod)
		d.lines = append(d.lines, fmt.Sprintf("reenter %v %s", ok, x.String()))
		return moejs.Undefined(), nil
	}))
	return d
}

// load loads the runtime's one module: top followed by the scripts as
// exported functions run<i>.
func (d *moejsPromiseDriver) load(top string) error {
	var src strings.Builder
	src.WriteString(top + "\n")
	for i, fn := range d.fns {
		fmt.Fprintf(&src, "export function run%d() {%s\n}\n", i, fn)
	}
	mod, err := moejs.Compile("promise.js", src.String())
	if err != nil {
		panic(err)
	}
	d.mod = mod
	return d.rt.Load(mod)
}

func (d *moejsPromiseDriver) run(fn int) {
	if d.mod == nil {
		if err := d.load(""); err != nil {
			panic(err)
		}
	}
	h, err := d.mod.Hook(fmt.Sprintf("run%d", fn))
	if err != nil {
		panic(err)
	}
	v, err := d.rt.Call(h)
	if err != nil {
		d.lines = append(d.lines, "error")
		return
	}
	d.report(v)
}

// module logs Load's outcome as the promise the Sobek APIs return: Load has
// none, and its error is the rejection the host is told of.
func (d *moejsPromiseDriver) module(src string, interrupt bool) {
	if interrupt {
		d.rt.Interrupt("halt")
	}
	err := d.load(src)
	d.rt.ClearInterrupt()
	p := new(int)
	var exc *moejs.Exception
	var ie *moejs.InterruptedError
	switch {
	case errors.As(err, &exc):
		d.track(p, true)
		d.promise(p, int(moejs.PromiseRejected), exc.Message())
	case errors.As(err, &ie):
		d.track(p, true)
		d.promise(p, int(moejs.PromiseRejected), ie.Value)
	default:
		d.promise(p, int(moejs.PromiseFulfilled), err)
	}
}

func (d *moejsPromiseDriver) report(p moejs.Value) {
	state, result, ok := moejs.PromiseResult(p)
	if !ok {
		return
	}
	var out any
	if state != moejs.PromisePending {
		out = d.rt.Realm().ToGo(result)
	}
	d.promise(p.AsObject(), int(state), out)
}

func (d *moejsPromiseDriver) settle(h int, reject bool, v any) {
	var jv moejs.Value
	switch r := v.(type) {
	case hostRef:
		jv = d.hosts[r]
	case keptRef:
		jv = d.kept[r].(moejs.Value)
	default:
		var err error
		if jv, err = d.rt.FromGo(v); err != nil {
			panic(err)
		}
	}
	if err := d.settlers[h][boolIndex(reject)](jv); err != nil {
		d.lines = append(d.lines, "error")
	}
}

func (d *moejsPromiseDriver) final() []string {
	for _, p := range d.hosts {
		d.report(p)
	}
	return d.lines
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestPromiseHostAPIMatchesSobek runs the same scripts and host settlements
// on Sobek and the native API and compares what the rejection tracker saw,
// when, of which promise, and the promises' state and result.
func TestPromiseHostAPIMatchesSobek(t *testing.T) {
	type settle struct {
		h      int
		reject bool
		v      any
	}
	type step struct {
		fn        int
		settle    *settle
		module    string
		interrupt bool // interrupts the module's evaluation
	}
	cases := []struct {
		name  string
		fns   []string
		steps []step // default: run each fn in order
		want  []string
	}{
		{name: "unhandled", fns: []string{`return Promise.reject(1)`}, want: []string{"R0", "P0 2 1"}},
		{name: "handled synchronously", fns: []string{`const p = Promise.reject(1); log("a"); p.then(null, e => log("c" + e)); log("b"); return p`},
			want: []string{"R0", "a", "H0", "b", "c1", "P0 2 1"}},
		{name: "derived", fns: []string{`return new Promise((_, reject) => reject(2)).then(v => v)`}, want: []string{"R0", "H0", "R1", "P1 2 2"}},
		{name: "throwing handler", fns: []string{`return Promise.resolve(1).then(v => { throw v + 1 })`}},
		{name: "caught and rethrown", fns: []string{`return Promise.reject(3).catch(e => { log("caught " + e); return Promise.reject(e + 1) })`}},
		{name: "executor throws", fns: []string{`return new Promise(() => { throw 4 })`}},
		{name: "all", fns: []string{`return Promise.all([Promise.reject(5), Promise.reject(6)])`}},
		{name: "handled in a job", fns: []string{`const p = Promise.reject(7); Promise.resolve().then(() => { log("job"); p.catch(() => {}) }); return p`}},
		{name: "rejected twice", fns: []string{`let rej; const p = new Promise((_, r) => { rej = r }); rej(8); rej(9); return p`}},
		{name: "adopts a rejection", fns: []string{`return new Promise(resolve => resolve(Promise.reject(10)))`}},
		{name: "finally", fns: []string{`return Promise.reject(11).finally(() => log("fin"))`}},
		{name: "then of then", fns: []string{`const p = Promise.reject(12); p.then(); return p.then(v => v, e => { throw e + 1 })`}},
		{name: "host reject unhandled",
			fns:   []string{`globalThis.p = hostPromise(); return p`, `return p.catch(e => log("c " + e))`},
			steps: []step{{fn: 0}, {settle: &settle{0, true, "x"}}, {fn: 1}},
			want:  []string{"P0 0 <nil>", "R0", "H0", "c x", "P1 1 <nil>", "P0 2 x"}},
		{name: "host reject handled",
			fns:   []string{`const p = hostPromise(); p.then(null, e => log("c " + e)); return p`},
			steps: []step{{fn: 0}, {settle: &settle{0, true, "y"}}, {settle: &settle{0, false, "late"}}}},
		{name: "host resolve",
			fns:   []string{`const p = hostPromise(); p.then(v => log("v " + v)); return p.then(v => { throw v })`},
			steps: []step{{fn: 0}, {settle: &settle{0, false, "z"}}}},
		{name: "host adopts host",
			fns:   []string{`globalThis.a = hostPromise(); globalThis.b = hostPromise(); return a`},
			steps: []step{{fn: 0}, {settle: &settle{0, false, hostRef(1)}}, {settle: &settle{1, true, "w"}}}},
		{name: "host adopts rejected host",
			fns:   []string{`globalThis.a = hostPromise(); globalThis.b = hostPromise(); return b`, `return a`},
			steps: []step{{fn: 0}, {settle: &settle{1, true, "v"}}, {settle: &settle{0, false, hostRef(1)}}, {fn: 1}}},
		// await drives the tracker through PerformPromiseThen on the awaited
		// promise and through the rejection of the async function's promise.
		{name: "async throws", fns: []string{`async function f() { throw 13 } return f()`}, want: []string{"R0", "P0 2 13"}},
		{name: "async throws after await", fns: []string{`const p = (async () => { await null; throw 14 })(); log("sync"); return p`},
			want: []string{"sync", "R0", "P0 2 14"}},
		{name: "await catches", fns: []string{`return (async () => { try { await Promise.reject(15) } catch (e) { log("c" + e) } return "ok" })()`},
			want: []string{"R0", "H0", "c15", "P1 1 ok"}},
		{name: "await rethrows", fns: []string{`return (async () => { await Promise.reject(16) })()`}, want: []string{"R0", "H0", "R1", "P1 2 16"}},
		{name: "async handled synchronously", fns: []string{`const p = (async () => { throw 17 })(); p.catch(e => log("c" + e)); return p`}},
		{name: "async handled in a job", fns: []string{`const p = (async () => { throw 18 })(); (async () => { await null; log("job"); p.catch(() => {}) })(); return p`}},
		{name: "async returns a rejection", fns: []string{`async function f() { return Promise.reject(19) } return f()`}},
		{name: "async returns a rejection after await", fns: []string{`async function f() { await null; return Promise.reject(20) } return f()`}},
		{name: "await a rejecting thenable", fns: []string{`return (async () => { try { await { then(_, r) { r(21) } } } catch (e) { log("c" + e) } })()`}},
		{name: "await all", fns: []string{`return (async () => { try { await Promise.all([Promise.reject(22), Promise.reject(23)]) } catch (e) { log("c" + e) } })()`}},
		{name: "await in finally", fns: []string{`return (async () => { try { throw 24 } finally { await Promise.reject(25).catch(() => log("inner")) } })()`}},
		{name: "chained async",
			fns: []string{`async function a() { await null; throw 26 } async function b() { try { return await a() } catch (e) { log("b " + e); throw e + 1 } }
				return b().catch(e => log("top " + e))`}},
		{name: "await a host rejection",
			fns:   []string{`globalThis.p = hostPromise(); return (async () => { try { await p } catch (e) { log("c " + e) } })()`},
			steps: []step{{fn: 0}, {settle: &settle{0, true, "u"}}}},
		{name: "await a rejected host",
			fns:   []string{`globalThis.p = hostPromise(); return p`, `return (async () => { try { await p } catch (e) { log("c " + e) } })()`},
			steps: []step{{fn: 0}, {settle: &settle{0, true, "t"}}, {fn: 1}}},
		{name: "async rejects a host",
			fns:   []string{`globalThis.p = hostPromise(); return (async () => { const v = await p; throw v + "!" })()`},
			steps: []step{{fn: 0}, {settle: &settle{0, false, "s"}}}},
		{name: "all over a proxied array", fns: []string{`return Promise.all(new Proxy([1, Promise.reject(2)], {get(t, k, r) { log("get " + String(k)); return Reflect.get(t, k, r) }}))`},
			want: []string{"R0", "get Symbol(Symbol.iterator)", "get length", "get 0", "get length", "get 1", "H0", "get length", "R1", "P1 2 2"}},
		{name: "iterable trap throws", fns: []string{`return Promise.race(new Proxy([], {get() { throw "it" }}))`}, want: []string{"R0", "P0 2 it"}},
		{name: "host resolves with a proxy thenable",
			fns: []string{`const p = hostPromise(); p.then(v => log("v " + v))
keep(new Proxy({}, {get(t, k) { log("get " + String(k)); return k === "then" ? r => r("pv") : undefined }})); return p`},
			steps: []step{{fn: 0}, {settle: &settle{0, false, keptRef(0)}}},
			want:  []string{"P0 0 <nil>", "get then", "v pv", "P0 1 pv"}},
		{name: "host resolves with a proxy whose then trap throws",
			fns:   []string{`keep(new Proxy({}, {get() { throw "trap" }})); return hostPromise()`},
			steps: []step{{fn: 0}, {settle: &settle{0, false, keptRef(0)}}},
			want:  []string{"P0 0 <nil>", "R0", "P0 2 trap"}},
		// The host finishes its own bookkeeping before the jobs run.
		{name: "module jobs see the module",
			steps: []step{{module: `export let x = 1; globalThis.count = (globalThis.count || 0) + 1;
Promise.resolve().then(() => { reenter(); log("count " + count) })`}},
			want: []string{"reenter true 1", "count 1", "P0 1 <nil>"}},
		{name: "module interrupted",
			steps: []step{{module: `export let x = 1; for (;;) {}`, interrupt: true}},
			want:  []string{"R0", "P0 2 halt"}},
	}
	drivers := []struct {
		name string
		new  func([]string) promiseDriver
	}{{"sobek", newSobekPromiseDriver}, {"moejs", newMoejsPromiseDriver}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			steps := c.steps
			if steps == nil {
				for i := range c.fns {
					steps = append(steps, step{fn: i})
				}
			}
			var reference []string
			for _, dr := range drivers {
				d := dr.new(c.fns)
				for _, s := range steps {
					switch {
					case s.settle != nil:
						d.settle(s.settle.h, s.settle.reject, s.settle.v)
					case s.module != "":
						d.module(s.module, s.interrupt)
					default:
						d.run(s.fn)
					}
				}
				got := d.final()
				if reference == nil {
					reference = got
					if c.want != nil {
						assert.Equal(t, c.want, got, "sobek")
					}
					continue
				}
				assert.Equal(t, reference, got, dr.name)
			}
		})
	}
}

func TestESMToGlobalsCoversCorpus(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("..", "testdata", "plugins", "*", "plugin.js"))
	require.NoError(t, err)
	if len(entries) == 0 {
		t.Skip("the new-api plugins are not fetched; run bench/testdata/plugins/fetch.sh")
	}
	require.Len(t, entries, 10)
	for _, path := range entries {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		script, err := engines.ESMToGlobals(string(src))
		require.NoError(t, err, path)
		assert.Contains(t, script, "globalThis.meta = meta;")
		assert.Contains(t, script, "globalThis.buildSubmitRequest = buildSubmitRequest;")
		assert.NotContains(t, script, "\nexport ")
	}
}

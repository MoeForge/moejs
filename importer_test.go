package moejs_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// importSources are modules that import others dynamically; each entry
// exports out, what its top level computed.
var importSources = map[string]string{
	"ns.js": `const ns = await import("./m.js");
		export const out = ns.v + " " + (ns === await import("./m.js")) + " " + Object.prototype.toString.call(ns);`,
	"static.js": `import * as s from "./counter.js";
		export const out = String(s === await import("./counter.js")) + " " + globalThis.runs;`,
	"graph.js":      `export const out = (await import("./a.js")).v;`,
	"nested.js":     `const a = await import("./a.js"); export const out = (await a.f()).w;`,
	"linkErr.js":    `export const out = await import("./ambiguous.js").catch(e => e.name + " " + e.message);`,
	"resolveErr.js": `export const out = await import("./nope.js").catch(e => String(e));`,
	"graphErr.js":   `export const out = await import("./missing.js").catch(e => String(e));`,
	"meta.js":       `export const out = import.meta.url + " " + (await import("./metaDep.js")).meta.url;`,
	"solo.js": `globalThis.runs = (globalThis.runs | 0) + 1; export const x = import.meta.url;
		export let out; import("./solo.js").then(ns => { out = ns.x + " " + globalThis.runs; });`,
	"throws.js": `const f = () => import("./bad.js").catch(e => e); const e1 = await f(), e2 = await f();
		export const out = e1 + " " + (e1 === e2);`,

	"m.js":         `export const v = "m";`,
	"counter.js":   `globalThis.runs = (globalThis.runs | 0) + 1;`,
	"a.js":         `import { w } from "./b.js"; export const v = w + "a"; export const f = () => import("./b.js");`,
	"b.js":         `export const w = "b";`,
	"ambiguous.js": `import { x } from "./stars.js";`,
	"stars.js":     `export * from "./x1.js"; export * from "./x2.js";`,
	"x1.js":        `export const x = 1;`,
	"x2.js":        `export const x = 2;`,
	"missing.js":   `import "./nope.js";`,
	"metaDep.js":   `export const meta = import.meta;`,
	"bad.js":       `throw new TypeError("bad");`,
}

// importMeta sets import.meta.url to the module's name.
func importMeta(r *moejs.Realm, m *moejs.Module, meta *moejs.Object) error {
	return meta.SetProp(r, r.KeyFromGoString("url"), moejs.String(m.Name()))
}

// TestImporter loads modules that import others dynamically.
func TestImporter(t *testing.T) {
	cases := []struct {
		entry, want string
		calls       map[string]int // resolutions, when checked
	}{
		{"ns.js", "m true [object Module]", map[string]int{"ns.js -> ./m.js": 2}},
		{"static.js", "true 1", nil},
		{"graph.js", "ba", map[string]int{"graph.js -> ./a.js": 1, "a.js -> ./b.js": 1}},
		{"nested.js", "b", nil},
		{"linkErr.js", `SyntaxError ambiguous.js:1:10: The requested module './stars.js' contains conflicting star exports for name 'x'`, nil},
		{"resolveErr.js", `Error: no module "./nope.js"`, nil},
		{"graphErr.js", `Error: missing.js:1:8: cannot resolve module "./nope.js": no module "./nope.js"`, nil},
		{"meta.js", "meta.js metaDep.js", nil},
		{"solo.js", "solo.js 1", map[string]int{"solo.js -> ./solo.js": 1}},
		{"throws.js", "TypeError: bad true", nil},
	}
	for _, c := range cases {
		t.Run(c.entry, func(t *testing.T) {
			h := newLinkHost(t, importSources)
			rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: h.resolve, Meta: importMeta}})
			mod := h.module(c.entry)
			if len(mod.Requests()) != 0 {
				mod = h.mustLink(c.entry)
			}
			require.NoError(t, rt.Load(mod))
			out, ok := rt.Export("out")
			require.True(t, ok)
			assert.Equal(t, c.want, out.String())
			if c.calls != nil {
				assert.Equal(t, c.calls, h.calls)
			}
		})
	}
}

// TestImporterMissing checks import() without an Importer or its resolver,
// or with a resolver that returns no module.
func TestImporterMissing(t *testing.T) {
	const noHook = `TypeError: Cannot import "./m.js": the host set no import hook (moejs.Options.Importer, engine.Realm.SetImportHooks)`
	cases := []struct {
		name     string
		importer *moejs.Importer
		want     string
	}{
		{"no Importer", nil, noHook},
		{"no Resolve", &moejs.Importer{Meta: importMeta}, noHook},
		{"no module", &moejs.Importer{Resolve: func(moejs.Referrer, string) (*moejs.Module, error) { return nil, nil }},
			`TypeError: Cannot import "./m.js": the host loaded no module`},
		{"an exception", &moejs.Importer{Resolve: func(moejs.Referrer, string) (*moejs.Module, error) {
			return nil, fmt.Errorf("wrapped: %w", &moejs.Exception{Value: moejs.Int(7)})
		}}, "7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mod, err := moejs.Compile("main.js", `export const out = await import("./m.js").then(() => "fulfilled", String);`)
			require.NoError(t, err)
			assert.Nil(t, mod.Requests())
			rt := moejs.NewRuntime(moejs.Options{Importer: c.importer})
			require.NoError(t, rt.Load(mod))
			out, _ := rt.Export("out")
			assert.Equal(t, c.want, out.String())
		})
	}
}

// TestImporterScript checks import() in a script, whose referrer is the
// *Script.
func TestImporterScript(t *testing.T) {
	h := newLinkHost(t, importSources)
	rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: h.resolve}})
	s, err := moejs.CompileScript("s.js", `import("./m.js").then(ns => { globalThis.got = ns.v; });`)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	v, err := rt.RunScript(mustScript(t, `got`))
	require.NoError(t, err)
	assert.Equal(t, "m", v.String())
	assert.Equal(t, map[string]int{"s.js -> ./m.js": 1}, h.calls)

	// The module the script imported is the runtime's.
	require.NoError(t, rt.Load(h.mustLink("static.js")))
	_, err = rt.RunScript(mustScript(t, `import("./counter.js")`))
	require.NoError(t, err)
	v, err = rt.RunScript(mustScript(t, `globalThis.runs`))
	require.NoError(t, err)
	assert.Equal(t, "1", v.String())
}

// TestImporterReferrer checks the referrers Resolve sees: the module Link
// linked, one Resolve returned, the script.
func TestImporterReferrer(t *testing.T) {
	h := newLinkHost(t, importSources)
	var got []moejs.Referrer
	imp := &moejs.Importer{Resolve: func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
		got = append(got, referrer)
		return h.resolve(referrer, specifier)
	}}
	rt := moejs.NewRuntime(moejs.Options{Importer: imp})
	require.NoError(t, rt.Load(h.module("nested.js")))
	require.Len(t, got, 3)
	assert.Same(t, h.module("nested.js"), got[0], "Compile's module, loaded alone")
	assert.Same(t, h.module("a.js"), got[1], "linking a.js")
	assert.Same(t, h.module("a.js"), got[2], "a.js's import()")
}

// TestImporterLinked checks import() of a module Link returned, whose graph
// it evaluates, and the Link of a module that imports nothing but uses
// import.meta.
func TestImporterLinked(t *testing.T) {
	h := newLinkHost(t, importSources)
	linkedA := h.mustLink("a.js")
	calls := 0
	imp := &moejs.Importer{Resolve: func(moejs.Referrer, string) (*moejs.Module, error) {
		calls++
		return linkedA, nil
	}, Meta: importMeta}
	entry, err := moejs.Compile("lm.js", `export const out = import.meta.url + " " + (await import("./a.js")).v;`)
	require.NoError(t, err)
	mod, err := moejs.Link(entry, nil)
	require.NoError(t, err)
	assert.NotSame(t, entry, mod, "linked alone")
	rt := moejs.NewRuntime(moejs.Options{Importer: imp})
	require.NoError(t, rt.Load(mod))
	out, _ := rt.Export("out")
	assert.Equal(t, "lm.js ba", out.String())
	assert.Equal(t, 1, calls, "a.js's graph is Link's")
}

// TestImporterLoadedAlone checks that import() in a runtime with an
// Importer finds a module Load evaluated that neither imports nor uses
// import() or import.meta, rather than evaluating it a second time, and
// that such a module loads with an Importer as it does without one.
func TestImporterLoadedAlone(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"m.js": `globalThis.mruns = (globalThis.mruns ?? 0) + 1; export let n = 0; export function inc() { n++; }`,
	})
	rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: h.resolve}})
	m := h.module("m.js")
	assert.Nil(t, m.Requests())
	require.NoError(t, rt.Load(m))
	inc, err := m.Hook("inc")
	require.NoError(t, err)
	_, err = rt.Call(inc)
	require.NoError(t, err)
	_, err = rt.RunScript(mustScript(t, `import("./m.js").then(ns => { globalThis.seen = ns.n + "," + globalThis.mruns; });`))
	require.NoError(t, err)
	v, err := rt.RunScript(mustScript(t, `seen`))
	require.NoError(t, err)
	assert.Equal(t, "1,1", v.String(), "one instance of m.js in the runtime")

	// What Load, Export and Call return is the same with and without an
	// Importer.
	sources := []string{
		`export let v = 1; export function f() { return v; }`,
		`export let v = 1; export function f() { return v; } v = 2; throw new Error("top");`,
		`export let v = 1; export function f() { return v; } await 0; v = 2;`,
		`export let v = 1; export function f() { return v; } await 0; v = 2; throw new Error("late");`,
		`export let v = 1; export function f() { return v; } await new Promise(() => {});`,
		`export let v = 1; export function f() { return v; } Promise.resolve().then(() => { v = 3; });`,
		`export let v = 1; export function f() { return v; } v = 2; boom();`,
		`export let v = 1; export function f() { return v; } await 0; v = 2; boom();`,
		`export let v = 1; export function f() { return v; } v = 2; stop(); for (;;) {}`,
	}
	load := func(src string, imp *moejs.Importer) string {
		m, err := moejs.Compile("m.js", src)
		require.NoError(t, err)
		rt := moejs.NewRuntime(moejs.Options{Importer: imp})
		require.NoError(t, rt.SetGlobal("boom", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			panic("boom")
		})))
		require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			r.Interrupt("stop")
			return moejs.Undefined(), nil
		})))
		lerr := rt.Load(m)
		rt.ClearInterrupt()
		v, ok := rt.Export("v")
		f, err := m.Hook("f")
		require.NoError(t, err)
		res, cerr := rt.Call(f)
		return fmt.Sprintf("load=%v export=%v,%v call=%v,%v", lerr, v, ok, res, cerr)
	}
	for _, src := range sources {
		without := load(src, nil)
		assert.Equal(t, without, load(src, &moejs.Importer{Resolve: h.resolve}), src)
	}
}

// TestImporterInconsistentResolution checks that Load of a graph that
// resolves a request of a module the runtime instantiated through import()
// to another module fails with a *ResolveError.
func TestImporterInconsistentResolution(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `import { d } from "./dep.js"; export const out = d;`,
		"dep.js":  `export const d = "one";`,
		"dep2.js": `export const d = "two";`,
	})
	rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: h.resolve}})
	_, err := rt.RunScript(mustScript(t, `import("./main.js").then(ns => { globalThis.out = ns.out; })`))
	require.NoError(t, err)
	mod, err := moejs.Link(h.module("main.js"), func(moejs.Referrer, string) (*moejs.Module, error) {
		return h.module("dep2.js"), nil
	})
	require.NoError(t, err)
	err = rt.Load(mod)
	var re *moejs.ResolveError
	require.ErrorAs(t, err, &re)
	assert.EqualError(t, err, `main.js:1:19: cannot resolve module "./dep.js": the graph resolves it to dep2.js, but the realm instantiated main.js with dep.js`)
	v, err := rt.RunScript(mustScript(t, `out`))
	require.NoError(t, err)
	assert.Equal(t, "one", v.String())
}

// TestImporterInterrupt checks that an interrupt within the evaluation of a
// module import() loaded stops Load, and that a later import() of it
// rejects with an Error that does not hold the interrupt.
func TestImporterInterrupt(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `export const out = await import("./spin.js");`,
		"spin.js": `stop(); for (;;) {}`,
	})
	rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: h.resolve}})
	require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
		r.Interrupt("stop")
		return moejs.Undefined(), nil
	})))
	err := rt.Load(h.module("main.js"))
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	rt.ClearInterrupt()
	_, err = rt.RunScript(mustScript(t, `import("./spin.js").catch(e => { globalThis.e = String(e); })`))
	require.NoError(t, err)
	v, err := rt.RunScript(mustScript(t, `e`))
	require.NoError(t, err)
	assert.Equal(t, `Error: Cannot import "./spin.js": its evaluation was interrupted`, v.String())
}

// TestImporterInterruptedGraph checks what a later import() of a module
// whose evaluation was interrupted, or of one that imports it, rejects
// with: an Error with a fixed message, which holds neither the interrupt's
// value nor a Go error, unlike the Error of a failed resolution.
func TestImporterInterruptedGraph(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `const why = e => e.constructor.name + ":" + e.message;
			export const imp = s => import(s).then(() => "ok", why);
			export const raw = s => import(s).then(() => undefined, e => e);`,
		"spin.js": `stop(); for (;;);`,
		"wrap.js": `import "./spin.js";`,
	})
	rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: h.resolve}})
	require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		rt.Interrupt("stop")
		return moejs.Undefined(), nil
	})))
	m := h.module("main.js")
	require.NoError(t, rt.Load(m))
	imp, err := m.Hook("imp")
	require.NoError(t, err)
	raw, err := m.Hook("raw")
	require.NoError(t, err)
	settled := func(h moejs.Hook, specifier string) moejs.Value {
		t.Helper()
		p, err := rt.Call(h, moejs.String(specifier))
		require.NoError(t, err)
		state, v, _ := p.AsObject().PromiseResult()
		require.NotEqual(t, moejs.PromisePending, state)
		return v
	}

	_, err = rt.Call(imp, moejs.String("./spin.js"))
	var ie *moejs.InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	rt.ClearInterrupt()
	assert.Equal(t, `Error:Cannot import "./spin.js": its evaluation was interrupted`, settled(imp, "./spin.js").String())
	assert.Equal(t, `Error:Cannot import "./wrap.js": its evaluation was interrupted`, settled(imp, "./wrap.js").String())

	interrupted := &moejs.Exception{Value: settled(raw, "./spin.js")}
	assert.NotContains(t, interrupted.Error(), "stop")
	assert.NoError(t, interrupted.Unwrap(), "no Go error")
	resolveErr := &moejs.Exception{Value: settled(raw, "./nope.js")}
	assert.Equal(t, `Error: no module "./nope.js"`, resolveErr.Error())
	assert.EqualError(t, resolveErr.Unwrap(), `no module "./nope.js"`)
}

// TestImporterInterruptedAsync checks that a module whose asynchronous
// evaluation an interrupt stopped stays failed, as one stopped before its
// first await does: the interrupt stops its resumed top level (spin.js),
// drops the job that would resume it (a.js), or stops the module run
// before it, which the module waits on (p1.js before p2.js and top.js).
// The import() the interrupt stopped stays pending.
func TestImporterInterruptedAsync(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		files       map[string]string
		failed      []string
	}{
		{"resumed", "./wrap.js", map[string]string{
			"spin.js": `await 0; stop(); for (;;);`,
			"wrap.js": `import "./spin.js"; globalThis.ran = 1;`,
		}, []string{"./spin.js", "./wrap.js"}},
		{"dropped", "./c.js", map[string]string{
			"a.js": `await 0; globalThis.ran = 1;`,
			"b.js": `stop(); for (;;);`,
			"c.js": `import "./a.js"; import "./b.js";`,
		}, []string{"./a.js", "./b.js", "./c.js"}},
		{"waiting", "./top.js", map[string]string{
			"s.js":   `await 0;`,
			"p1.js":  `import "./s.js"; stop(); for (;;);`,
			"p2.js":  `import "./s.js"; globalThis.ran = 1;`,
			"top.js": `import "./p1.js"; import "./p2.js";`,
		}, []string{"./p1.js", "./p2.js", "./top.js"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.files["main.js"] = `export const imp = s => import(s).then(() => "ok", e => e.constructor.name + ":" + e.message);`
			h := newLinkHost(t, tc.files)
			rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: h.resolve}})
			require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
				rt.Interrupt("stop")
				return moejs.Undefined(), nil
			})))
			m := h.module("main.js")
			require.NoError(t, rt.Load(m))
			imp, err := m.Hook("imp")
			require.NoError(t, err)

			first, err := rt.Call(imp, moejs.String(tc.entry))
			var ie *moejs.InterruptedError
			require.ErrorAs(t, err, &ie)
			rt.ClearInterrupt()
			for _, s := range tc.failed {
				p, err := rt.Call(imp, moejs.String(s))
				require.NoError(t, err)
				state, v, _ := p.AsObject().PromiseResult()
				require.Equal(t, moejs.PromiseFulfilled, state, s)
				assert.Equal(t, `Error:Cannot import "`+s+`": its evaluation was interrupted`, v.String())
			}
			if first.IsObject() {
				state, _, _ := first.AsObject().PromiseResult()
				assert.Equal(t, moejs.PromisePending, state)
			}
			ran, err := rt.RunScript(mustScript(t, `globalThis.ran`))
			require.NoError(t, err)
			assert.True(t, ran.IsUndefined())
		})
	}
}

// TestImporterReentrantResolve checks a Resolve that calls back into the
// runtime that evaluates the import().
func TestImporterReentrantResolve(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `export function go() { return import("./m.js").then(ns => ns.v); } export function side() { return "side"; }`,
		"m.js":    `export const v = "m";`,
	})
	m := h.module("main.js")
	side, err := m.Hook("side")
	require.NoError(t, err)
	var rt *moejs.Runtime
	var got []string
	rt = moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
		v, err := rt.Call(side)
		if err != nil {
			return nil, err
		}
		got = append(got, v.String())
		return h.resolve(referrer, specifier)
	}}})
	require.NoError(t, rt.Load(m))
	goHook, err := m.Hook("go")
	require.NoError(t, err)
	p, err := rt.Call(goHook)
	require.NoError(t, err)
	state, v, _ := p.AsObject().PromiseResult()
	assert.Equal(t, moejs.PromiseFulfilled, state)
	assert.Equal(t, "m", v.String())
	assert.Equal(t, []string{"side"}, got)
}

// TestImporterConcurrent loads modules that import others dynamically in
// many runtimes sharing one Importer; run it with -race.
func TestImporterConcurrent(t *testing.T) {
	mods := make(map[string]*moejs.Module)
	for name, src := range importSources {
		m, err := moejs.Compile(name, src)
		require.NoError(t, err)
		mods[name] = m
	}
	var resolveErr = errors.New("no module")
	imp := &moejs.Importer{Resolve: func(_ moejs.Referrer, specifier string) (*moejs.Module, error) {
		if m := mods[strings.TrimPrefix(specifier, "./")]; m != nil {
			return m, nil
		}
		return nil, resolveErr
	}, Meta: importMeta}
	entries := []string{"graph.js", "meta.js", "solo.js", "nested.js"}
	want := []string{"ba", "meta.js metaDep.js", "solo.js 1", "b"}
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt := moejs.NewRuntime(moejs.Options{Importer: imp})
			k := i % len(entries)
			if errs[i] = rt.Load(mods[entries[k]]); errs[i] != nil {
				return
			}
			if out, _ := rt.Export("out"); out.String() != want[k] {
				errs[i] = fmt.Errorf("%s: out %s, want %s", entries[k], out.String(), want[k])
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		assert.NoError(t, err)
	}
}

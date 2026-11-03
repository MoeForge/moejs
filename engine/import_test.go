package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// importHost is a test host of import(): Load returns the module of srcs a
// specifier names, Link links it once, and the host value of each module is
// its name, which Load logs as the referrer. Its realms have a global
// out(v) that appends String(v) to out.
type importHost struct {
	*moduleHost
	graphs map[*bytecode.Function]*ModuleGraph
	// loads are the "referrer>specifier" of the calls of Load.
	loads []string
	// meta counts the calls of Meta per module.
	meta map[any]int
	out  []string
}

func newImportHost(t *testing.T, srcs map[string]string) *importHost {
	return &importHost{moduleHost: newModuleHost(t, srcs), graphs: make(map[*bytecode.Function]*ModuleGraph), meta: make(map[any]int)}
}

func (h *importHost) graph(code *bytecode.Function) (*ModuleGraph, error) {
	if g := h.graphs[code]; g != nil {
		return g, nil
	}
	g, err := LinkModules(code, LinkOptions{Resolve: h.resolve, HostDefined: func(code *bytecode.Function) any { return code.Source.Name }})
	if err == nil {
		h.graphs[code] = g
	}
	return g, err
}

// hooks returns the host's hooks; Meta sets import.meta.url to the module's
// name.
func (h *importHost) hooks() *ImportHooks {
	return &ImportHooks{
		Load: func(r *Realm, referrer any, specifier string) (any, error) {
			h.loads = append(h.loads, fmt.Sprint(referrer)+">"+specifier)
			if _, ok := h.srcs[specifier]; !ok {
				return nil, fmt.Errorf("no module %q", specifier)
			}
			return h.module(specifier), nil
		},
		Link: func(r *Realm, module any) (*ModuleGraph, error) { return h.graph(module.(*bytecode.Function)) },
		Meta: func(r *Realm, module any, meta *Object) error {
			h.meta[module]++
			return meta.SetProp(r, r.KeyFromGoString("url"), StringValue(FromGoString(fmt.Sprint(module))))
		},
	}
}

// realm returns a realm with hooks and the global out.
func (h *importHost) realm(hooks *ImportHooks) *Realm {
	r := NewRealm()
	r.SetImportHooks(hooks)
	out := r.NewNativeFunction(FromGoString("out"), 1, func(r *Realm, _ Value, args []Value) (Value, error) {
		s, err := r.ToString(Arg(args, 0))
		if err != nil {
			return Undefined(), err
		}
		h.out = append(h.out, s.GoString())
		return Undefined(), nil
	})
	require.NoError(h.t, r.Global.SetProp(r, r.KeyFromGoString("out"), ObjectValue(out)))
	return r
}

// evaluate evaluates the graph of the module entry in r, with the jobs.
func (h *importHost) evaluate(r *Realm, entry string) (*ModuleGraph, error) {
	h.t.Helper()
	g, err := h.graph(h.module(entry))
	require.NoError(h.t, err)
	_, p, err := r.EvaluateGraph(g)
	if err == nil && p != nil {
		err = r.ModuleEvaluationError(p, nil)
	}
	return g, err
}

// script compiles src as a script.
func (h *importHost) script(name, src string) *bytecode.Function {
	h.t.Helper()
	s, err := syntax.ParseScript(name, src, syntax.Options{})
	require.NoError(h.t, err)
	code, err := compiler.CompileScript(s)
	require.NoError(h.t, err)
	return code
}

// TestImportCall evaluates modules that import others dynamically and log
// what they get.
func TestImportCall(t *testing.T) {
	const counted = `globalThis.runs = (globalThis.runs | 0) + 1; export const v = 1;`
	cases := []struct {
		name  string
		srcs  map[string]string
		want  string
		loads string // the Loads, when checked
	}{
		{"namespace", map[string]string{
			"main": `import("m").then(ns => out(ns.v + " " + Object.prototype.toString.call(ns) + " " + Object.getPrototypeOf(ns)));`,
			"m":    `export const v = 1;`,
		}, "1 [object Module] null", "main>m"},
		{"the realm's instance", map[string]string{
			"main": `import * as s from "m"; import("m").then(ns => out((ns === s) + " " + globalThis.runs));`,
			"m":    counted,
		}, "true 1", ""},
		{"twice", map[string]string{
			"main": `Promise.all([import("m"), import("m")]).then(([a, b]) => out((a === b) + " " + globalThis.runs));`,
			"m":    counted,
		}, "true 1", "main>m,main>m"},
		{"a graph", map[string]string{
			"main": `import("a").then(ns => out(ns.v));`,
			"a":    `import { w } from "b"; export const v = w + 1;`,
			"b":    `export const w = 1;`,
		}, "2", "main>a"},
		{"the referrer of nested code", map[string]string{
			"main": `const load = () => function (s) { return import(s); }; load()("a").then(ns => ns.f()).then(ns => out(ns.w));`,
			"a":    `export const f = () => import("b");`,
			"b":    `export const w = "b";`,
		}, "b", "main>a,a>b"},
		{"itself", map[string]string{
			"main": `import * as self from "main"; export const x = 1; globalThis.runs = (globalThis.runs | 0) + 1;
				import("main").then(ns => out((ns === self) + " " + ns.x + " " + globalThis.runs));`,
		}, "true 1 1", "main>main"},
		{"a specifier converted to a string", map[string]string{
			"main": `import({ toString() { return "m"; } }).then(ns => out(ns.v));`,
			"m":    `export const v = 1;`,
		}, "1", "main>m"},
		{"a conversion that throws", map[string]string{
			"main": `const p = import({ toString() { throw new RangeError("spec"); } }); out(p instanceof Promise); p.catch(e => out(e));`,
		}, "true,RangeError: spec", ""},
		{"a Load error", map[string]string{
			"main": `import("nope").catch(e => out(e));`,
		}, `Error: no module "nope"`, "main>nope"},
		{"a Link error", map[string]string{
			"main": `import("a").catch(e => out(e.name + " " + e.message.includes("nope")));`,
			"a":    `import { nope } from "b";`,
			"b":    `export const w = 1;`,
		}, "Error true", ""},
		{"an evaluation error, cached", map[string]string{
			"main": `const f = () => import("bad").catch(e => e); const e1 = await f(), e2 = await f(); out(e1 + " " + (e1 === e2));`,
			"bad":  `globalThis.runs = (globalThis.runs | 0) + 1; throw new TypeError("bad " + globalThis.runs);`,
		}, "TypeError: bad 1 true", ""},
		{"a thenable namespace", map[string]string{
			"main": `import("t").then(v => out(v));`,
			"t":    `export function then(resolve) { resolve("T"); }`,
		}, "T", ""},
		{"a then that throws", map[string]string{
			"main": `import("t").catch(e => out(e));`,
			"t":    `export function then() { throw new EvalError("then"); }`,
		}, "EvalError: then", ""},
		{"top-level await in the imported module", map[string]string{
			"main": `import("m").then(ns => out(ns.v)); out("sync");`,
			"m":    `export let v = 1; await 0; v = 2;`,
		}, "sync,2", ""},
		{"a rejection after an await", map[string]string{
			"main": `import("m").catch(e => out(e));`,
			"m":    `await 0; throw new RangeError("late");`,
		}, "RangeError: late", ""},
		{"from top-level await", map[string]string{
			"main": `const { v } = await import("m"); out(v);`,
			"m":    `export const v = await Promise.resolve("m");`,
		}, "m", ""},
		// Load runs within import(), linking and evaluating from a job, then
		// resolving from the reaction to the evaluation.
		{"the jobs", map[string]string{
			"main": `import("m").then(() => out("import")); Promise.resolve().then(() => out(1)).then(() => out(2)).then(() => out(3));`,
			"m":    `export const v = 1;`,
		}, "1,2,import,3", ""},
		{"a Load error rejects at once", map[string]string{
			"main": `import("nope").catch(() => out("import")); Promise.resolve().then(() => out(1));`,
		}, "import,1", ""},
		// The promises of a module waiting on another settle after the
		// other's, rejected as fulfilled (test262 top-level-await's
		// fulfillment-order.js and rejection-order.js).
		{"the fulfillment order", orderSrcs("resolve"), "B,A", ""},
		{"the rejection order", orderSrcs("reject"), "B,A", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newImportHost(t, c.srcs)
			r := h.realm(h.hooks())
			_, err := h.evaluate(r, "main")
			require.NoError(t, err)
			assert.Equal(t, c.want, strings.Join(h.out, ","))
			if c.loads != "" {
				assert.Equal(t, c.loads, strings.Join(h.loads, ","))
			}
		})
	}
}

// orderSrcs are modules a and b, where a waits on b, which awaits p1 that
// main settles with settle once both started.
func orderSrcs(settle string) map[string]string {
	return map[string]string{
		"main": `import { p1, pA, pB } from "setup"; const log = [];
			const a = pB.promise.then(() => import("a").finally(() => log.push("A"))).catch(() => {});
			const b = import("b").finally(() => log.push("B")).catch(() => {});
			Promise.all([pA.promise, pB.promise]).then(p1.` + settle + `);
			Promise.all([a, b]).then(() => out(log));`,
		"a":      `import "aStart"; import "b";`,
		"aStart": `import { pA } from "setup"; pA.resolve();`,
		"b":      `import "bStart"; import { p1 } from "setup"; await p1.promise;`,
		"bStart": `import { pB } from "setup"; pB.resolve();`,
		"setup":  `export const p1 = Promise.withResolvers(), pA = Promise.withResolvers(), pB = Promise.withResolvers();`,
	}
}

// TestImportCallHooks checks import() with hooks missing or failing.
func TestImportCallHooks(t *testing.T) {
	load := func(module any, err error) func(*Realm, any, string) (any, error) {
		return func(*Realm, any, string) (any, error) { return module, err }
	}
	const noHook = `TypeError: Cannot import "x": the host set no import hook (moejs.Options.Importer, engine.Realm.SetImportHooks)`
	cases := []struct {
		name  string
		hooks func(h *importHost) *ImportHooks
		want  string
	}{
		{"no hooks", func(*importHost) *ImportHooks { return nil }, noHook},
		{"no Link", func(h *importHost) *ImportHooks { return &ImportHooks{Load: h.hooks().Load} }, noHook},
		{"only Meta", func(h *importHost) *ImportHooks { return &ImportHooks{Meta: h.hooks().Meta} }, noHook},
		{"no module", func(h *importHost) *ImportHooks {
			return &ImportHooks{Load: load(nil, nil), Link: h.hooks().Link}
		}, `TypeError: Cannot import "x": the host loaded no module`},
		{"an exception", func(h *importHost) *ImportHooks {
			return &ImportHooks{Load: func(r *Realm, _ any, _ string) (any, error) { return nil, r.Throw(NumberValue(7)) }, Link: h.hooks().Link}
		}, "7"},
		{"a TypeError", func(h *importHost) *ImportHooks {
			return &ImportHooks{Load: func(r *Realm, _ any, s string) (any, error) { return nil, r.TypeError("bad %s", s) }, Link: h.hooks().Link}
		}, "TypeError: bad x"},
		{"a Link error", func(h *importHost) *ImportHooks {
			return &ImportHooks{Load: h.hooks().Load, Link: func(*Realm, any) (*ModuleGraph, error) { return nil, errors.New("no link") }}
		}, "Error: no link"},
		{"no graph", func(h *importHost) *ImportHooks {
			return &ImportHooks{Load: h.hooks().Load, Link: func(*Realm, any) (*ModuleGraph, error) { return nil, nil }}
		}, "Error: engine: ImportHooks.Link returned no module graph"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newImportHost(t, map[string]string{
				"main": `import("x").then(() => out("fulfilled"), e => out(e));`,
				"x":    `export const v = 1;`,
			})
			r := h.realm(c.hooks(h))
			_, err := h.evaluate(r, "main")
			require.NoError(t, err)
			assert.Equal(t, []string{c.want}, h.out)
		})
	}

	t.Run("removed", func(t *testing.T) {
		h := newImportHost(t, map[string]string{"main": `import("x").catch(e => out(e));`})
		r := h.realm(h.hooks())
		r.SetImportHooks(nil)
		_, err := h.evaluate(r, "main")
		require.NoError(t, err)
		assert.Equal(t, []string{noHook}, h.out)
	})
}

// TestImportCallScript checks import() in scripts: the referrer is the host
// value SetHostDefined recorded for the script's template, nil without one.
func TestImportCallScript(t *testing.T) {
	srcs := map[string]string{"m": `export const v = "m";`}
	cases := []struct {
		name, src string
		host      any
		loads     string
	}{
		{"recorded", `import("m").then(ns => out(ns.v));`, "s.js", "s.js>m"},
		{"nested", `function f() { return () => import("m"); } f()().then(ns => out(ns.v));`, "s.js", "s.js>m"},
		{"not recorded", `import("m").then(ns => out(ns.v));`, nil, "<nil>>m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newImportHost(t, srcs)
			r := h.realm(h.hooks())
			code := h.script("s.js", c.src)
			require.True(t, code.ScriptOrModule)
			if c.host != nil {
				r.SetHostDefined(code, c.host)
			}
			_, err := r.RunScript(code)
			require.NoError(t, err)
			assert.Equal(t, []string{"m"}, h.out)
			assert.Equal(t, c.loads, strings.Join(h.loads, ","))
		})
	}

	t.Run("a script that does not import", func(t *testing.T) {
		h := newImportHost(t, srcs)
		r := h.realm(h.hooks())
		code := h.script("s.js", `1 + 1;`)
		assert.False(t, code.ScriptOrModule)
		r.SetHostDefined(code, "s.js")
		assert.True(t, r.lazy == nil || r.lazy.modules == nil, "no module map")
	})

	t.Run("the module of a script", func(t *testing.T) {
		// A module the script imported is the realm's: a graph that imports it
		// statically later reuses it.
		h := newImportHost(t, map[string]string{
			"m":    `globalThis.runs = (globalThis.runs | 0) + 1; export const v = "m";`,
			"main": `import { v } from "m"; out(v + globalThis.runs);`,
		})
		r := h.realm(h.hooks())
		code := h.script("s.js", `import("m").then(ns => out(ns.v));`)
		r.SetHostDefined(code, "s.js")
		_, err := r.RunScript(code)
		require.NoError(t, err)
		_, err = h.evaluate(r, "main")
		require.NoError(t, err)
		assert.Equal(t, []string{"m", "m1"}, h.out)
	})
}

// TestImportCallLinkedButNotRun checks graphs that enter a realm's module
// map after a graph whose evaluation failed left some of its modules
// instantiated and not run.
func TestImportCallLinkedButNotRun(t *testing.T) {
	srcs := map[string]string{
		"a":  `import "b"; import "d"; out("a");`,
		"b":  `throw new Error("b");`,
		"d":  `globalThis.druns = (globalThis.druns | 0) + 1; export const v = "d"; out("d");`,
		"e":  `import { v } from "d"; import "f"; export const w = v + "e";`,
		"f":  `out("f");`,
		"ad": `import "a"; import "d";`,
	}
	cases := []struct {
		name, second string
		want         []string
	}{
		{"the unrun module", `import("d").then(ns => out(ns.v + globalThis.druns));`, []string{"d", "d1"}},
		{"a graph with it", `import("e").then(ns => out(ns.w));`, []string{"d", "f", "de"}},
		{"the failed module", `import("a").catch(e => out(e));`, []string{"Error: b"}},
		{"a graph with the failed module", `import("ad").catch(e => out(e + " " + globalThis.druns));`, []string{"Error: b undefined"}},
		{"both", `import("a").catch(e => out(e)); import("d").then(ns => out(ns.v));`, []string{"d", "Error: b", "d"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newImportHost(t, srcs)
			r := h.realm(h.hooks())
			_, err := h.evaluate(r, "a")
			require.EqualError(t, err, "Error: b")
			code := h.script("s.js", c.second)
			r.SetHostDefined(code, "s.js")
			_, err = r.RunScript(code)
			require.NoError(t, err)
			assert.Equal(t, c.want, h.out)
		})
	}
}

// TestImportMeta checks import.meta: one null-prototype object per module
// per realm, which Meta fills on first use.
func TestImportMeta(t *testing.T) {
	srcs := map[string]string{
		"main": `import { m as bm } from "b";
			export const same = import.meta === import.meta && import.meta === (() => import.meta)();
			export const proto = Object.getPrototypeOf(import.meta) === null && Object.isExtensible(import.meta);
			export const url = import.meta.url;
			export const other = bm !== import.meta && bm.url;
			export const keys = Object.keys(import.meta).join();`,
		"b": `export const m = import.meta;`,
	}
	h := newImportHost(t, srcs)
	for range 2 {
		r := h.realm(h.hooks())
		g, err := h.evaluate(r, "main")
		require.NoError(t, err)
		for name, want := range map[string]any{"same": true, "proto": true, "url": "main", "other": "b", "keys": "url"} {
			assert.Equal(t, want, graphExport(t, r, g, name), name)
		}
	}
	assert.Equal(t, map[any]int{"main": 2, "b": 2}, h.meta, "once per module per realm")

	t.Run("no hook", func(t *testing.T) {
		h := newImportHost(t, srcs)
		r := h.realm(nil)
		g, err := h.evaluate(r, "main")
		require.NoError(t, err)
		assert.Equal(t, true, graphExport(t, r, g, "proto"))
		assert.Equal(t, "", graphExport(t, r, g, "keys"))
	})

	t.Run("an error", func(t *testing.T) {
		h := newImportHost(t, map[string]string{
			"main": `let e; try { import.meta; } catch (x) { e = String(x); } export const err = e; export const url = import.meta.url;`,
		})
		hooks := h.hooks()
		meta := hooks.Meta
		hooks.Meta = func(r *Realm, module any, o *Object) error {
			if len(h.meta) == 0 {
				h.meta["failed"]++
				return errors.New("no meta")
			}
			return meta(r, module, o)
		}
		r := h.realm(hooks)
		g, err := h.evaluate(r, "main")
		require.NoError(t, err)
		assert.Equal(t, "Error: no meta", graphExport(t, r, g, "err"))
		assert.Equal(t, "main", graphExport(t, r, g, "url"), "Meta runs again")
	})

	t.Run("of a module imported dynamically", func(t *testing.T) {
		h := newImportHost(t, map[string]string{
			"main": `import("b").then(ns => out(ns.m.url + " " + (ns.m === import.meta)));`,
			"b":    `export const m = import.meta;`,
		})
		r := h.realm(h.hooks())
		_, err := h.evaluate(r, "main")
		require.NoError(t, err)
		assert.Equal(t, []string{"b false"}, h.out)
	})
}

// TestImportRecordsPerTemplate checks that a realm keeps one record per
// distinct script whose code uses import() that it ran, however often it
// runs, and none for a script whose code does not (SetHostDefined).
func TestImportRecordsPerTemplate(t *testing.T) {
	h := newImportHost(t, map[string]string{"m": `export const v = "m";`})
	r := h.realm(h.hooks())
	for i := range 50 {
		code := h.script(fmt.Sprintf("s%d.js", i), `import("m").catch(() => {});`)
		r.SetHostDefined(code, i)
		_, err := r.RunScript(code)
		require.NoError(t, err)
	}
	code := h.script("same.js", `import("m").catch(() => {});`)
	for range 10 {
		r.SetHostDefined(code, "same")
		_, err := r.RunScript(code)
		require.NoError(t, err)
	}
	refs := len(r.lazy.modules.refs)
	assert.Equal(t, 51, refs, "m uses neither import() nor import.meta")
	plain := h.script("plain.js", `1 + 1;`)
	r.SetHostDefined(plain, "plain")
	_, err := r.RunScript(plain)
	require.NoError(t, err)
	assert.Equal(t, refs, len(r.lazy.modules.refs))
}

// TestImportCallInterrupt checks that an interrupt stops the code that runs
// import(), within Load or its jobs, and that a module the interrupt stopped
// rejects a later import() with an Error that does not hold the interrupt.
func TestImportCallInterrupt(t *testing.T) {
	h := newImportHost(t, map[string]string{
		"main":  `import("m").then(() => out("fulfilled"), e => out("rejected"));`,
		"again": `import("m").then(() => out("fulfilled"), e => out(e));`,
		"m":     `stop(); for (;;) {}`,
	})
	r := h.realm(h.hooks())
	stop := r.NewNativeFunction(FromGoString("stop"), 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		r.Interrupt("stop")
		return Undefined(), nil
	})
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("stop"), ObjectValue(stop)))
	_, err := h.evaluate(r, "main")
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	assert.Empty(t, h.out, "the jobs after the interrupt are dropped")
	r.ClearInterrupt()
	_, err = h.evaluate(r, "again")
	require.NoError(t, err)
	assert.Equal(t, []string{`Error: Cannot import "m": its evaluation was interrupted`}, h.out)

	t.Run("in Load", func(t *testing.T) {
		h := newImportHost(t, map[string]string{"main": `out("before"); import("m"); out("after");`})
		hooks := h.hooks()
		hooks.Load = func(r *Realm, _ any, _ string) (any, error) {
			r.Interrupt("load")
			return nil, r.CheckInterrupt()
		}
		r := h.realm(hooks)
		_, err := h.evaluate(r, "main")
		var ie *InterruptedError
		require.ErrorAs(t, err, &ie)
		assert.Equal(t, "load", ie.Value)
		assert.Equal(t, []string{"before"}, h.out)
	})
}

// TestImportMetaEarlyExports checks that the record of a module whose
// EarlyExports variant a later graph instantiates in place of the template
// (TestModuleEarlyExportsAcrossGraphs) moves to the variant with the host
// value.
func TestImportMetaEarlyExports(t *testing.T) {
	srcs := map[string]string{
		"x":     `let m = "M"; export function g() { return m + import.meta.url; }`,
		"y":     `throw new Error("y");`,
		"unrun": `import "y"; import "x";`,
		"main":  `import "b"; import { g } from "x"; export { out } from "b"; export function f() { return g(); }`,
		"b":     `import { f } from "main"; export let out; try { out = f(); } catch (e) { out = e.name; }`,
		"late":  `import { g } from "x"; export const out = g();`,
	}
	h := newImportHost(t, srcs)
	link := func(entry string) *ModuleGraph {
		g, err := LinkModules(h.module(entry), LinkOptions{
			Resolve:     h.resolve,
			HostDefined: func(code *bytecode.Function) any { return code.Source.Name },
			EarlyExports: func(code *bytecode.Function) (*bytecode.Function, error) {
				m, err := syntax.ParseModule(code.Source.Name, srcs[code.Source.Name], syntax.Options{EarlyExports: true})
				require.NoError(t, err)
				return compiler.CompileModule(m)
			},
		})
		require.NoError(t, err)
		return g
	}
	r := h.realm(h.hooks())
	_, _, err := r.EvaluateGraph(link("unrun"))
	require.EqualError(t, err, "Error: y")
	g := link("main")
	_, _, err = r.EvaluateGraph(g)
	require.NoError(t, err)
	assert.Equal(t, "ReferenceError", graphExport(t, r, g, "out"))
	assert.Len(t, r.lazy.modules.refs, 1, "the template's record moved to the variant")
	g = link("late")
	_, _, err = r.EvaluateGraph(g)
	require.NoError(t, err)
	assert.Equal(t, "Mx", graphExport(t, r, g, "out"))
}

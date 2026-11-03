package engine

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleHost is a test host: it compiles each named source once and
// resolves a specifier to the module of that name.
type moduleHost struct {
	t    *testing.T
	srcs map[string]string
	code map[string]*bytecode.Function
	// resolved counts the calls of resolve.
	resolved int
}

func newModuleHost(t *testing.T, srcs map[string]string) *moduleHost {
	return &moduleHost{t: t, srcs: srcs, code: make(map[string]*bytecode.Function)}
}

// module returns the template of the module name.
func (h *moduleHost) module(name string) *bytecode.Function {
	h.t.Helper()
	if c := h.code[name]; c != nil {
		return c
	}
	m, err := syntax.ParseModule(name, h.srcs[name], syntax.Options{})
	require.NoError(h.t, err, name)
	c, err := compiler.CompileModule(m)
	require.NoError(h.t, err, name)
	h.code[name] = c
	return c
}

func (h *moduleHost) resolve(referrer *bytecode.Function, request int) (*bytecode.Function, error) {
	h.resolved++
	spec := referrer.Module.Links.Requests[request].Specifier
	if _, ok := h.srcs[spec]; !ok {
		return nil, fmt.Errorf("no module %q", spec)
	}
	return h.module(spec), nil
}

func (h *moduleHost) link(entry string) (*ModuleGraph, error) {
	return LinkModules(h.module(entry), LinkOptions{Resolve: h.resolve})
}

// graphExport returns ToGo of the entry's export name of g in r.
func graphExport(t *testing.T, r *Realm, g *ModuleGraph, name string) any {
	t.Helper()
	b, ok := g.Binding(name)
	require.True(t, ok, "export %q", name)
	v, ok := r.GraphBinding(g, b)
	require.True(t, ok, "export %q initialized", name)
	return r.ToGo(v)
}

// evalGraph links entry of srcs and evaluates it in a fresh realm; out is
// the entry's export "out".
func evalGraph(t *testing.T, srcs map[string]string, entry string) (r *Realm, g *ModuleGraph, err error) {
	t.Helper()
	g, err = newModuleHost(t, srcs).link(entry)
	require.NoError(t, err)
	r = NewRealm()
	_, p, err := r.EvaluateGraph(g)
	if err == nil && p != nil {
		err = r.ModuleEvaluationError(p, nil)
	}
	return r, g, err
}

const logPush = `globalThis.log = (globalThis.log || []).concat(%q);`

func logged(name string) string { return fmt.Sprintf(logPush, name) }

// TestModuleImports evaluates graphs whose entry computes "out" from what
// it imports.
func TestModuleImports(t *testing.T) {
	cases := []struct {
		name string
		srcs map[string]string
		want any
	}{
		{"import forms", map[string]string{
			"main": `import d, { a, b as c, "x y" as xy } from "m"; import * as ns from "m"; import "side";
export const out = [d, a, c, xy, ns.a, ns["x y"], globalThis.sideRan].join();`,
			"m":    `export default "D"; export const a = 1; export let b = 2; const q = 3; export { q as "x y" };`,
			"side": `globalThis.sideRan = true;`,
		}, "D,1,2,3,1,3,true"},
		{"live bindings", map[string]string{
			"main": `import { n, inc } from "m"; import * as ns from "m"; const before = n; inc(); inc();
export const out = [before, n, ns.n].join();`,
			"m": `export let n = 0; export function inc() { n++; }`,
		}, "0,2,2"},
		{"export from", map[string]string{
			"main": `import { x, y, ns, z, w } from "re"; export const out = [x, y, ns.v, z, w].join();`,
			"re":   `export { v as x } from "a"; export * from "b"; export * as ns from "a"; export { default as z } from "c"; import { v } from "a"; export { v as w };`,
			"a":    `export const v = "A";`,
			"b":    `export const y = "B";`,
			"c":    `export default "C";`,
		}, "A,B,A,C,A"},
		{"star skips default and local names win", map[string]string{
			"main": `import * as ns from "re"; export const out = ["default" in ns, ns.v].join();`,
			"re":   `export * from "a"; export const v = "local";`,
			"a":    `export default 1; export const v = "A";`,
		}, "false,local"},
		{"same binding through two stars", map[string]string{
			"main": `import { v } from "s"; export const out = v;`,
			"s":    `export * from "a"; export * from "a2";`,
			"a2":   `export * from "a";`,
			"a":    `export const v = "A";`,
		}, "A"},
		{"ambiguous star is left out of the namespace", map[string]string{
			"main": `import * as ns from "s"; export const out = ["x" in ns, ns.y].join();`,
			"s":    `export * from "a"; export * from "b";`,
			"a":    `export const x = 1, y = 2;`,
			"b":    `export const x = 3;`,
		}, "false,2"},
		{"a namespace import re-exported is one binding", map[string]string{
			"main": `import { ns } from "s"; import * as e from "e"; export const out = [ns === e, ns.v].join();`,
			"s":    `export * from "a"; export * from "b";`,
			"a":    `import * as ns from "e"; export { ns };`,
			"b":    `import * as ns from "e"; export { ns };`,
			"e":    `export const v = "E";`,
		}, "true,E"},
		{"re-exported namespace of a cycle", map[string]string{
			"main": `import { ns } from "a"; export const out = ns.ns.ns.v;`,
			"a":    `export * as ns from "a"; export const v = "self";`,
		}, "self"},
		{"hoisted functions across a cycle", map[string]string{
			"main": `import { fb } from "b"; export function fa() { return "fa"; } export const out = fb();`,
			"b":    `import { fa } from "main"; export function fb() { return "fb>" + fa(); }`,
		}, "fb>fa"},
		{"evaluation order of a cycle", map[string]string{
			"main": `import "b"; ` + logged("main") + ` export const out = globalThis.log.join();`,
			"b":    `import "main"; import "c"; ` + logged("b"),
			"c":    logged("c"),
		}, "c,b,main"},
		{"shared dependency runs once", map[string]string{
			"main": `import "b"; import "c"; ` + logged("main") + ` export const out = globalThis.log.join();`,
			"b":    `import "d"; ` + logged("b"),
			"c":    `import "d"; ` + logged("c"),
			"d":    logged("d"),
		}, "d,b,c,main"},
		{"uninitialized binding of a cycle", map[string]string{
			"main": `import { f } from "b"; export let x = 1; export const out = f();`,
			"b": `import { x } from "main"; import * as ns from "main"; let r = [];
try { x; } catch (e) { r.push(e.constructor.name); }
try { ns.x; } catch (e) { r.push(e.constructor.name); }
try { Object.keys(ns); } catch (e) { r.push(e.constructor.name); }
r.push("x" in ns, Reflect.ownKeys(ns).length);
export function f() { return r.concat(x).join(); }`,
		}, "ReferenceError,ReferenceError,ReferenceError,true,3,1"},
		{"imports are immutable", map[string]string{
			"main": `import { a } from "m"; import * as ns from "m"; const r = [];
try { a = 2; } catch (e) { r.push(e.constructor.name); }
try { a++; } catch (e) { r.push(e.constructor.name); }
try { ns = 1; } catch (e) { r.push(e.constructor.name); }
try { ns.a = 2; } catch (e) { r.push(e.constructor.name); }
export const out = r.concat(a).join();`,
			"m": `export let a = 1;`,
		}, "TypeError,TypeError,TypeError,TypeError,1"},
		{"module this and strictness", map[string]string{
			"main": `import { t } from "m"; export const out = [t, typeof this].join();`,
			"m":    `export const t = (function () { return typeof this; })();`,
		}, "undefined,undefined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, err := evalGraph(t, c.srcs, "main")
			require.NoError(t, err)
			assert.Equal(t, c.want, graphExport(t, r, g, "out"))
		})
	}
}

// TestModuleNamespace checks the internal methods of a namespace object.
func TestModuleNamespace(t *testing.T) {
	srcs := map[string]string{
		"main": `import * as ns from "m"; const r = [];
const keys = Reflect.ownKeys(ns).map(k => typeof k === "symbol" ? "@@" + k.description : k.codePointAt(0).toString(16));
r.push(keys.join(" "));
r.push(Object.prototype.toString.call(ns), ns[Symbol.toStringTag]);
r.push(Object.isExtensible(ns), Object.getPrototypeOf(ns), Reflect.setPrototypeOf(ns, null), Reflect.setPrototypeOf(ns, {}));
r.push(Reflect.set(ns, "a", 1), Reflect.set(ns, "zz", 1), Reflect.deleteProperty(ns, "a"), Reflect.deleteProperty(ns, "zz"));
r.push("a" in ns, "zz" in ns, Symbol.toStringTag in ns, Symbol.iterator in ns);
r.push(Reflect.defineProperty(ns, "a", { value: 2 }), Reflect.defineProperty(ns, "a", { value: 3 }),
  Reflect.defineProperty(ns, "a", { writable: false }), Reflect.defineProperty(ns, "a", { enumerable: false }),
  Reflect.defineProperty(ns, "a", { configurable: true }), Reflect.defineProperty(ns, "a", { get() {} }),
  Reflect.defineProperty(ns, "zz", { value: 1 }), Reflect.defineProperty(ns, Symbol.toStringTag, { value: "Module" }),
  Reflect.defineProperty(ns, Symbol.toStringTag, { value: "X" }));
const d = Object.getOwnPropertyDescriptor(ns, "a");
r.push(d.value, d.writable, d.enumerable, d.configurable);
const td = Object.getOwnPropertyDescriptor(ns, Symbol.toStringTag);
r.push(td.writable, td.enumerable, td.configurable);
try { Object.freeze(ns); } catch (e) { r.push(e.constructor.name); }
r.push(Object.isFrozen(ns), Object.isSealed(ns), Object.seal(ns) === ns);
try { "use strict"; delete ns.a; } catch (e) { r.push(e.constructor.name); }
r.push(JSON.stringify(ns), Object.keys(ns).length);
for (const k in ns) r.push("in:" + k);
export const out = r.join();`,
		"m": `export const b = 1, a = 2; export { a as "\u{1F600}", a as "￿" }; export let late = 3;`,
	}
	r, g, err := evalGraph(t, srcs, "main")
	require.NoError(t, err)
	want := "61 62 6c 1f600 ffff @@Symbol.toStringTag," +
		"[object Module],Module," +
		"false,,true,false," +
		"false,false,false,true," +
		"true,false,true,false," +
		"true,false,false,false,false,false,false,true,false," +
		"2,true,true,false," +
		"false,false,false," +
		"TypeError," +
		"false,true,true," +
		"TypeError," +
		`{"a":2,"b":1,"late":3,"😀":2,"￿":2},5,` +
		"in:a,in:b,in:late,in:😀,in:￿"
	assert.Equal(t, want, graphExport(t, r, g, "out"))

	b, ok := g.Binding("missing")
	assert.False(t, ok)
	assert.Zero(t, b)
}

// TestModuleNamespaceExports checks the entry's exports of a graph: its
// namespace names, re-exports included, and their bindings.
func TestModuleNamespaceExports(t *testing.T) {
	h := newModuleHost(t, map[string]string{
		"main": `export * from "a"; export * from "b"; export { v as w } from "a"; export * as nsb from "b"; export const own = 1;`,
		"a":    `export const v = "A", dup = 1;`,
		"b":    `export const u = "B", dup = 2;`,
	})
	g, err := h.link("main")
	require.NoError(t, err)
	assert.Equal(t, []string{"nsb", "own", "u", "v", "w"}, g.Exports())
	assert.Equal(t, []*bytecode.Function{h.module("main"), h.module("a"), h.module("b")}, g.Records())
	r := NewRealm()
	env, p, err := r.EvaluateGraph(g)
	require.NoError(t, err)
	assert.Nil(t, p)
	v, ok := env.GetBindingValue("own")
	require.True(t, ok)
	assert.Equal(t, int64(1), r.ToGo(v))
	assert.Equal(t, "A", graphExport(t, r, g, "w"))
	assert.Equal(t, "B", graphExport(t, r, g, "u"))
	b, _ := g.Binding("nsb")
	ns, ok := r.GraphBinding(g, b)
	require.True(t, ok)
	require.True(t, ns.IsObject())
	assert.True(t, ns.AsObject().IsModuleNamespace())
	assert.False(t, r.NewObject().IsModuleNamespace())

	_, ok = NewRealm().GraphBinding(g, b)
	assert.False(t, ok, "a realm that did not evaluate the graph")
}

// TestModuleLinkErrors checks the errors of LinkModules and their
// positions.
func TestModuleLinkErrors(t *testing.T) {
	cases := []struct {
		name      string
		srcs      map[string]string
		module    string // the module the error is in
		line, col int
		msg       string // SyntaxError message, or "" for a resolution error
		err       string // the resolution error
	}{
		{"missing export", map[string]string{
			"main": "const x = 1;\nimport { nope } from \"m\";",
			"m":    `export const a = 1;`,
		}, "main", 2, 10, "The requested module 'm' does not provide an export named 'nope'", ""},
		{"missing default", map[string]string{
			"main": `import d from "m";`,
			"m":    `export const a = 1;`,
		}, "main", 1, 8, "The requested module 'm' does not provide an export named 'default'", ""},
		{"ambiguous star", map[string]string{
			"main": `import { x } from "s";`,
			"s":    `export * from "a"; export * from "b";`,
			"a":    `export const x = 1;`,
			"b":    `export const x = 2;`,
		}, "main", 1, 10, "The requested module 's' contains conflicting star exports for name 'x'", ""},
		{"missing indirect export", map[string]string{
			"main": `import "re";`,
			"re":   "\n  export { q as r } from \"a\";",
			"a":    `export const v = 1;`,
		}, "re", 2, 12, "The requested module 'a' does not provide an export named 'q'", ""},
		{"circular indirect export", map[string]string{
			"main": `import { x } from "a";`,
			"a":    `export { x } from "b";`,
			"b":    `export { x } from "a";`,
		}, "b", 1, 10, "The requested module 'a' does not provide an export named 'x'", ""},
		{"unresolved request", map[string]string{
			"main": `import "a";`,
			"a":    "\n\nexport * from \"gone\";",
		}, "a", 3, 15, "", `no module "gone"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newModuleHost(t, c.srcs)
			_, err := h.link("main")
			var le *LinkError
			require.ErrorAs(t, err, &le)
			assert.Same(t, h.module(c.module), le.Module)
			assert.Equal(t, [2]int{c.line, c.col}, [2]int{le.Line, le.Col})
			assert.Equal(t, c.msg, le.Msg)
			if c.err == "" {
				assert.NoError(t, le.Err)
				assert.Equal(t, fmt.Sprintf("%s:%d:%d: SyntaxError: %s", c.module, c.line, c.col, c.msg), err.Error())
				return
			}
			assert.EqualError(t, le.Err, c.err)
			assert.Contains(t, err.Error(), fmt.Sprintf("%s:%d:%d: cannot resolve module", c.module, c.line, c.col))
		})
	}

	h := newModuleHost(t, map[string]string{"main": "\nimport x from \"a\";", "a": `export default 1;`})
	_, err := LinkModules(h.module("main"), LinkOptions{})
	assert.ErrorIs(t, err, ErrNoResolver)
	assert.EqualError(t, err, `main:2:15: cannot resolve module "a": no resolver: the host must resolve the module's requests`)

	_, err = LinkModules(h.module("main"), LinkOptions{Resolve: func(*bytecode.Function, int) (*bytecode.Function, error) { return nil, nil }})
	assert.ErrorContains(t, err, "the resolver returned no module template")

	sentinel := errors.New("denied")
	_, err = LinkModules(h.module("main"), LinkOptions{Resolve: func(*bytecode.Function, int) (*bytecode.Function, error) { return nil, sentinel }})
	assert.ErrorIs(t, err, sentinel)

	_, err = LinkModules(nil, LinkOptions{})
	assert.Error(t, err)

	g, err := LinkModules(h.module("a"), LinkOptions{})
	require.NoError(t, err, "an import-free module needs no resolver")
	assert.Equal(t, []string{"default"}, g.Exports())
}

// TestModuleResolveOnce checks that the resolver is asked once per module
// and request, a cycle and a diamond included.
func TestModuleResolveOnce(t *testing.T) {
	h := newModuleHost(t, map[string]string{
		"main": `import "a"; import "b";`,
		"a":    `import "c"; import "main";`,
		"b":    `import "c";`,
		"c":    `import "a";`,
	})
	g, err := h.link("main")
	require.NoError(t, err)
	assert.Equal(t, 6, h.resolved)
	assert.Len(t, g.Records(), 4)
}

// TestModuleSharedAcrossGraphs checks that a realm evaluates a module
// shared by two graphs once, and another realm again.
func TestModuleSharedAcrossGraphs(t *testing.T) {
	h := newModuleHost(t, map[string]string{
		"g1":     `import { n } from "shared"; export const out = n;`,
		"g2":     `import { n, bump } from "shared"; bump(); export const out = n;`,
		"shared": `globalThis.runs = (globalThis.runs || 0) + 1; export let n = globalThis.runs * 10; export function bump() { n++; }`,
	})
	g1, err := h.link("g1")
	require.NoError(t, err)
	g2, err := h.link("g2")
	require.NoError(t, err)
	for range 2 {
		r := NewRealm()
		_, _, err = r.EvaluateGraph(g1)
		require.NoError(t, err)
		_, _, err = r.EvaluateGraph(g2)
		require.NoError(t, err)
		_, _, err = r.EvaluateGraph(g2)
		require.NoError(t, err, "a graph evaluates once")
		assert.Equal(t, int64(10), graphExport(t, r, g1, "out"))
		assert.Equal(t, int64(11), graphExport(t, r, g2, "out"))
		runs, err := r.Global.GetProp(r, r.KeyFromGoString("runs"))
		require.NoError(t, err)
		assert.Equal(t, int64(1), r.ToGo(runs))
	}
}

// TestModuleInconsistentResolution checks that a graph that resolves a
// request of a module the realm instantiated to another module than the
// one it was instantiated with fails, before it instantiates any module,
// whether EvaluateGraph or import() evaluates it.
func TestModuleInconsistentResolution(t *testing.T) {
	h := newModuleHost(t, map[string]string{
		"main": `import { d } from "dep"; export const out = d;
			export function go(s) { return import(s).then(ns => ns.out, e => e.constructor.name + ": " + e.message); }`,
		"dep1":  `export const d = "one";`,
		"dep2":  `export const d = "two";`,
		"main2": `import { d } from "dep"; export const out = d;`,
		"main3": `import { out as o } from "main"; globalThis.runs = (globalThis.runs ?? 0) + 1; export const out = o;`,
	})
	// resolver returns a resolver of "dep" to dep.
	resolver := func(dep string) func(*bytecode.Function, int) (*bytecode.Function, error) {
		return func(referrer *bytecode.Function, request int) (*bytecode.Function, error) {
			if spec := referrer.Module.Links.Requests[request].Specifier; spec != "dep" {
				return h.module(spec), nil
			}
			return h.module(dep), nil
		}
	}
	link := func(entry, dep string) *ModuleGraph {
		g, err := LinkModules(h.module(entry), LinkOptions{Resolve: resolver(dep)})
		require.NoError(t, err)
		return g
	}
	r := NewRealm()
	r.SetImportHooks(&ImportHooks{
		Load: func(_ *Realm, _ any, spec string) (any, error) { return h.module(spec), nil },
		Link: func(_ *Realm, m any) (*ModuleGraph, error) {
			return LinkModules(m.(*bytecode.Function), LinkOptions{Resolve: resolver("dep2")})
		},
	})
	g1 := link("main", "dep1")
	_, _, err := r.EvaluateGraph(g1)
	require.NoError(t, err)
	assert.Equal(t, "one", graphExport(t, r, g1, "out"))
	b, _ := g1.Binding("go")
	fn, _ := r.GraphBinding(g1, b)
	imp := func(s string) string {
		res, err := r.Call(fn, Undefined(), []Value{StringValue(FromGoString(s))})
		require.NoError(t, err)
		r.HoldJobs()
		require.NoError(t, r.ReleaseJobs(nil))
		_, v, _ := res.AsObject().PromiseResult()
		return v.String()
	}
	assert.Equal(t, "two", imp("main2"), "a module the realm did not instantiate")

	const msg = `main:1:19: cannot resolve module "dep": the graph resolves it to dep2, but the realm instantiated main with dep1`
	_, _, err = r.EvaluateGraph(link("main", "dep2"))
	var le *LinkError
	require.ErrorAs(t, err, &le)
	assert.EqualError(t, err, msg)
	assert.Same(t, h.module("main"), le.Module)
	assert.Equal(t, "dep", le.Specifier)
	assert.Equal(t, "Error: "+msg, imp("main"))

	// The failed graph instantiated nothing: main3 runs once, from the
	// consistent graph, with its import bound.
	_, _, err = r.EvaluateGraph(link("main3", "dep2"))
	assert.EqualError(t, err, msg)
	g3 := link("main3", "dep1")
	_, _, err = r.EvaluateGraph(g3)
	require.NoError(t, err)
	assert.Equal(t, "one", graphExport(t, r, g3, "out"))
	runs, err := r.Global.GetProp(r, r.KeyFromGoString("runs"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), r.ToGo(runs))
}

// TestModuleEvaluationError checks that a realm caches the error of a
// module whose top level threw, for every graph that reaches it.
func TestModuleEvaluationError(t *testing.T) {
	h := newModuleHost(t, map[string]string{
		"g1":  `import "bad"; export const out = 1;`,
		"g2":  `import "mid"; export const out = 2;`,
		"mid": `import "bad"; ` + logged("mid"),
		"bad": `export let before = 1; throw new Error("boom"); export let after = 2;`,
	})
	g1, err := h.link("g1")
	require.NoError(t, err)
	g2, err := h.link("g2")
	require.NoError(t, err)
	gbad, err := h.link("bad")
	require.NoError(t, err)
	r := NewRealm()
	_, p, err1 := r.EvaluateGraph(g1)
	assert.Nil(t, p)
	var exc *Exception
	require.ErrorAs(t, err1, &exc)
	assert.Equal(t, "Error: boom", errorDisplayString(exc.Value))
	_, _, err2 := r.EvaluateGraph(g2)
	assert.Same(t, exc, err2, "the cached error, not a second evaluation")
	_, _, err3 := r.EvaluateGraph(gbad)
	assert.Same(t, exc, err3)
	log, _ := r.Global.GetProp(r, r.KeyFromGoString("log"))
	assert.True(t, log.IsUndefined(), "mid never ran")
	assert.Equal(t, int64(1), graphExport(t, r, gbad, "before"))
	b, _ := gbad.Binding("after")
	_, ok := r.GraphBinding(gbad, b)
	assert.False(t, ok, "uninitialized after the throw")
}

// TestModuleTopLevelAwait checks the asynchronous evaluation of graphs:
// the order of the bodies, the promise of Evaluate and a rejection.
func TestModuleTopLevelAwait(t *testing.T) {
	cases := []struct {
		name string
		srcs map[string]string
		want string
	}{
		{"async dependency", map[string]string{
			"main": `import "b"; import "c"; ` + logged("main") + ` export const out = globalThis.log.join();`,
			"b":    logged("b1") + ` await 0; ` + logged("b2"),
			"c":    logged("c"),
		}, "b1,c,b2,main"},
		{"async entry", map[string]string{
			"main": `import { v } from "b"; ` + logged("main1") + ` await 0; ` + logged("main2") + ` export const out = globalThis.log.join() + v;`,
			"b":    `export const v = "!"; ` + logged("b"),
		}, "b,main1,main2!"},
		{"async cycle", map[string]string{
			"main": `import "b"; ` + logged("main") + ` export const out = globalThis.log.join();`,
			"b":    `import "main"; import "c"; ` + logged("b1") + ` await 0; ` + logged("b2"),
			"c":    logged("c1") + ` await 0; ` + logged("c2"),
		}, "c1,c2,b1,b2,main"},
		{"parents run in async evaluation order", map[string]string{
			"main": `import "p1"; import "p2"; ` + logged("main") + ` export const out = globalThis.log.join();`,
			"p1":   `import "leaf"; ` + logged("p1"),
			"p2":   `import "leaf"; ` + logged("p2"),
			"leaf": logged("leaf1") + ` await 0; ` + logged("leaf2"),
		}, "leaf1,leaf2,p1,p2,main"},
		{"sibling async modules interleave", map[string]string{
			"main": `import "a"; import "b"; ` + logged("main") + ` export const out = globalThis.log.join();`,
			"a":    logged("a1") + ` await 0; ` + logged("a2") + ` await 0; ` + logged("a3"),
			"b":    logged("b1") + ` await 0; ` + logged("b2"),
		}, "a1,b1,a2,b2,a3,main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, err := evalGraph(t, c.srcs, "main")
			require.NoError(t, err)
			assert.Equal(t, c.want, graphExport(t, r, g, "out"))
		})
	}

	t.Run("rejection", func(t *testing.T) {
		h := newModuleHost(t, map[string]string{
			"main": `import "b"; ` + logged("main"),
			"b":    `await 0; throw new RangeError("late");`,
			"g2":   `import "main";`,
		})
		g, err := h.link("main")
		require.NoError(t, err)
		r := NewRealm()
		_, p, err := r.EvaluateGraph(g)
		require.NoError(t, err)
		require.NotNil(t, p)
		var exc *Exception
		require.ErrorAs(t, r.ModuleEvaluationError(p, nil), &exc)
		assert.Equal(t, "RangeError: late", errorDisplayString(exc.Value))
		_, p2, err := r.EvaluateGraph(g)
		require.NoError(t, err)
		assert.Same(t, p, p2, "the entry's top-level capability")
		g2, err := h.link("g2")
		require.NoError(t, err)
		_, _, err = r.EvaluateGraph(g2)
		require.ErrorAs(t, err, &exc, "the cached error of main")
		assert.Equal(t, "RangeError: late", errorDisplayString(exc.Value))
		log, _ := r.Global.GetProp(r, r.KeyFromGoString("log"))
		assert.True(t, log.IsUndefined(), "main never ran")
	})

	t.Run("entry throws before it awaits", func(t *testing.T) {
		h := newModuleHost(t, map[string]string{
			"main": `import "b"; export let x = 1; throw 7; await 0;`,
			"b":    `export const v = 1;`,
		})
		g, err := h.link("main")
		require.NoError(t, err)
		r := NewRealm()
		_, p, err := r.EvaluateGraph(g)
		require.NoError(t, err)
		require.NotNil(t, p)
		state, v, _ := p.PromiseResult()
		assert.Equal(t, PromiseRejected, state)
		assert.Equal(t, 7.0, v.AsNumber())
	})

	t.Run("pending", func(t *testing.T) {
		_, _, err := evalGraph(t, map[string]string{
			"main": `import "b";`,
			"b":    `await new Promise(() => {});`,
		}, "main")
		assert.ErrorIs(t, err, ErrModulePending)
	})
}

// TestModuleGraphConcurrent evaluates one linked graph in many realms at
// once (run under -race).
func TestModuleGraphConcurrent(t *testing.T) {
	h := newModuleHost(t, map[string]string{
		"main": `import { f } from "b"; import * as ns from "c"; export const out = f() + Object.keys(ns).join();`,
		"b":    `import { k } from "c"; export function f() { return k; }`,
		"c":    `import "main"; export const k = "k", z = 1;`,
	})
	g, err := h.link("main")
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			for range 20 {
				r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
				if _, _, err := r.EvaluateGraph(g); err != nil {
					errs <- err
					return
				}
				b, _ := g.Binding("out")
				v, _ := r.GraphBinding(g, b)
				if got := r.ToGo(v); got != "kk,z" {
					errs <- fmt.Errorf("out = %v", got)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestModuleGraphInterrupt checks an interrupt pending on entry, which
// instantiates nothing, and one during a dependency's body, which the
// realm then caches as the error of the modules it stopped.
func TestModuleGraphInterrupt(t *testing.T) {
	h := newModuleHost(t, map[string]string{
		"main": `import { n } from "b"; export const out = n;`,
		"b":    `export let n = 1; if (globalThis.spin) for (;;) {}`,
	})
	g, err := h.link("main")
	require.NoError(t, err)

	r := NewRealm()
	r.Interrupt("early")
	env, p, err := r.EvaluateGraph(g)
	require.ErrorAs(t, err, new(*InterruptedError))
	assert.Nil(t, env)
	assert.Nil(t, p)
	r.ClearInterrupt()
	_, _, err = r.EvaluateGraph(g)
	require.NoError(t, err)
	assert.Equal(t, int64(1), graphExport(t, r, g, "out"))

	r = NewRealm()
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("spin"), True()))
	time.AfterFunc(20*time.Millisecond, func() { r.Interrupt("stop") })
	_, _, err = r.EvaluateGraph(g)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	r.ClearInterrupt()
	_, _, err = r.EvaluateGraph(g)
	assert.Same(t, ie, err, "cached")
	gb, err := h.link("b")
	require.NoError(t, err)
	assert.Equal(t, int64(1), graphExport(t, r, gb, "n"))
}

// TestModuleEarlyExports links the EarlyExports variant of an import-free
// module that an import cycle calls into before its body runs, and only of
// that module.
func TestModuleEarlyExports(t *testing.T) {
	srcs := map[string]string{
		"main": `import "y"; import "b"; import { g } from "x";
			export { out } from "b";
			export function f() { return g(); }`,
		"b": `import { f } from "main";
			export let out;
			try { out = "" + f(); } catch (e) { out = e.name; }`,
		"x": `let m = 1; export function g() { return m; }`,
		"y": `let n = 1; export function h() { return n; }`,
	}
	h := newModuleHost(t, srcs)
	var asked []string
	early := func(code *bytecode.Function) (*bytecode.Function, error) {
		asked = append(asked, code.Source.Name)
		m, err := syntax.ParseModule(code.Source.Name, srcs[code.Source.Name], syntax.Options{EarlyExports: true})
		require.NoError(t, err)
		return compiler.CompileModule(m)
	}
	g, err := LinkModules(h.module("main"), LinkOptions{Resolve: h.resolve, EarlyExports: early})
	require.NoError(t, err)
	assert.Equal(t, []string{"x"}, asked, "y ran before the cycle formed")
	assert.Equal(t, h.module("x"), g.Records()[3], "the template stays the module's identity")
	r := NewRealm()
	_, _, err = r.EvaluateGraph(g)
	require.NoError(t, err)
	assert.Equal(t, "ReferenceError", graphExport(t, r, g, "out"))

	// A variant that does not match the module is refused.
	_, err = LinkModules(h.module("main"), LinkOptions{Resolve: h.resolve, EarlyExports: func(*bytecode.Function) (*bytecode.Function, error) {
		return h.module("y"), nil
	}})
	assert.ErrorContains(t, err, "does not match")
}

// TestModuleEarlyExportsAcrossGraphs checks a module that two graphs of a
// realm share when only one of them needs its EarlyExports variant: a later
// graph that calls into it early replaces the template a graph that never
// ran it instantiated, through the environment the earlier graph's
// namespaces point into, and never replaces a variant or a module that ran.
func TestModuleEarlyExportsAcrossGraphs(t *testing.T) {
	srcs := map[string]string{
		"x": `let m = "M"; export function g() { return m; }`,
		"y": `throw new Error("y");`,
		// A cycle that calls x's g before x runs, directly or through a
		// namespace.
		"main":   `import "b"; import { g } from "x"; export { out } from "b"; export function f() { return g(); }`,
		"b":      `import { f } from "main"; export let out; try { const v = f(); out = typeof v + ":" + String(v); } catch (e) { out = e.name; }`,
		"mainNS": `import "bNS"; import * as ns from "x"; export { out } from "bNS"; export function f() { return ns.g(); }`,
		"bNS":    `import { f } from "mainNS"; export let out; try { const v = f(); out = typeof v + ":" + String(v); } catch (e) { out = e.name; }`,
		// Graphs that instantiate x plain and do not run it, or run it.
		"unrun":   `import "y"; import "x";`,
		"unrunNS": `import "y"; import * as ns from "x"; export { ns };`,
		"ran":     `import "x";`,
		// A graph that instantiates x's variant, keeps its g and does not
		// run it; one that needs x plain.
		"keep": `import "c"; import "y"; import { g } from "x"; export function f() { return g; }`,
		"c":    `import { f } from "keep"; globalThis.saved = f();`,
		"same": `import { g } from "x"; export const same = g === globalThis.saved;`,
	}
	early := func(code *bytecode.Function) (*bytecode.Function, error) {
		m, err := syntax.ParseModule(code.Source.Name, srcs[code.Source.Name], syntax.Options{EarlyExports: true})
		require.NoError(t, err)
		return compiler.CompileModule(m)
	}
	tests := []struct {
		name, first, firstErr, second string
		firstEarly, secondEarly       bool // whether the graph links x's variant
		export                        string
		want                          any
	}{
		{"a module the first graph never ran", "unrun", "Error: y", "main", false, true, "out", "ReferenceError"},
		{"through a namespace of the first graph", "unrunNS", "Error: y", "mainNS", false, true, "out", "ReferenceError"},
		{"a module the first graph ran", "ran", "", "main", false, true, "out", "string:M"},
		{"a variant the first graph instantiated", "keep", "Error: y", "same", true, false, "same", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newModuleHost(t, srcs)
			link := func(entry string, wantEarly bool) *ModuleGraph {
				g, err := LinkModules(h.module(entry), LinkOptions{Resolve: h.resolve, EarlyExports: early})
				require.NoError(t, err)
				i := slices.Index(g.Records(), h.module("x"))
				require.GreaterOrEqual(t, i, 0)
				assert.Equal(t, wantEarly, g.records[i].code != g.records[i].key, entry)
				return g
			}
			g1, g2 := link(tt.first, tt.firstEarly), link(tt.second, tt.secondEarly)
			r := NewRealm()
			_, _, err := r.EvaluateGraph(g1)
			if tt.firstErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tt.firstErr)
			}
			_, _, err = r.EvaluateGraph(g2)
			require.NoError(t, err)
			assert.Equal(t, tt.want, graphExport(t, r, g2, tt.export))
		})
	}
}

// specResolveExport is ResolveExport (§16.2.1.7.2.2) as the spec states it,
// without the linker's memo.
func specResolveExport(l *linker, i int32, name string, set []resolveKey) (ModuleBinding, int) {
	k := resolveKey{i, name}
	if slices.Contains(set, k) {
		return ModuleBinding{}, resolvedNone
	}
	set = append(set, k)
	rec := &l.recs[i]
	m := rec.key.Module
	if slot, ok := m.Export(name); ok {
		return ModuleBinding{Module: i, Slot: int32(slot)}, resolvedBinding
	}
	links := m.Links
	if links == nil {
		return ModuleBinding{}, resolvedNone
	}
	for _, e := range links.Reexports {
		if e.Name != name {
			continue
		}
		dep := rec.deps[e.Request]
		if e.All {
			return ModuleBinding{Module: dep, Slot: -1}, resolvedBinding
		}
		return specResolveExport(l, dep, e.Import, set)
	}
	if name == "default" {
		return ModuleBinding{}, resolvedNone
	}
	var star ModuleBinding
	found := false
	for _, s := range links.Stars {
		b, st := specResolveExport(l, rec.deps[s.Request], name, set)
		switch {
		case st == resolvedAmbiguous:
			return ModuleBinding{}, resolvedAmbiguous
		case st == resolvedNone:
		case !found:
			star, found = b, true
		case b != star:
			return ModuleBinding{}, resolvedAmbiguous
		}
	}
	if !found {
		return ModuleBinding{}, resolvedNone
	}
	return star, resolvedBinding
}

// TestModuleResolveExportMemo checks the linker's memoized resolveExport
// against the spec's algorithm on random graphs of indirect, star and
// namespace exports with cycles, asking for every export in random order.
func TestModuleResolveExportMemo(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 1))
	names := []string{"a", "b", "c"}
	graphs := 400
	if testing.Short() {
		graphs = 100
	}
	for range graphs {
		n := 1 + rng.IntN(6)
		srcs := make(map[string]string, n)
		for i := range n {
			var src strings.Builder
			for _, name := range names {
				dep := fmt.Sprintf("m%d", rng.IntN(n))
				switch rng.IntN(10) {
				case 0, 1, 2:
					fmt.Fprintf(&src, "export const %s = 0;\n", name)
				case 3, 4, 5:
					fmt.Fprintf(&src, "export { %s as %s } from %q;\n", names[rng.IntN(len(names))], name, dep)
				case 6:
					fmt.Fprintf(&src, "export * as %s from %q;\n", name, dep)
				}
			}
			for range rng.IntN(3) {
				fmt.Fprintf(&src, "export * from %q;\n", fmt.Sprintf("m%d", rng.IntN(n)))
			}
			srcs[fmt.Sprintf("m%d", i)] = src.String()
		}
		h := newModuleHost(t, srcs)
		l := &linker{opts: LinkOptions{Resolve: h.resolve}, byKey: make(map[*bytecode.Function]int32)}
		l.add(h.module("m0"))
		require.NoError(t, l.load(0))
		var keys []resolveKey
		for i := range l.recs {
			for _, name := range names {
				keys = append(keys, resolveKey{int32(i), name})
			}
		}
		rng.Shuffle(len(keys), func(a, b int) { keys[a], keys[b] = keys[b], keys[a] })
		for _, k := range keys {
			wantB, wantSt := specResolveExport(l, k.module, k.name, nil)
			b, st := l.resolveExport(k.module, k.name)
			require.Equal(t, []any{wantB, wantSt}, []any{b, st}, "%v of %v", k, srcs)
		}
	}
}

// TestModuleLinkReexportChain links a long chain of indirect exports, which
// resolves once per module, not once per module and link along it.
func TestModuleLinkReexportChain(t *testing.T) {
	const n = 2000
	srcs := make(map[string]string, n)
	for i := range n - 1 {
		srcs[fmt.Sprintf("m%d", i)] = fmt.Sprintf(`export { x } from "m%d";`, i+1)
	}
	srcs[fmt.Sprintf("m%d", n-1)] = `export const x = 1;`
	h := newModuleHost(t, srcs)
	for name := range srcs {
		h.module(name)
	}
	start := time.Now()
	g, err := h.link("m0")
	elapsed := time.Since(start)
	require.NoError(t, err)
	b, ok := g.Binding("x")
	require.True(t, ok)
	assert.Equal(t, ModuleBinding{Module: n - 1, Slot: b.Slot}, b)
	t.Logf("link %v", elapsed)
	assert.Less(t, elapsed, 200*time.Millisecond, "a quadratic link takes about a second")
}

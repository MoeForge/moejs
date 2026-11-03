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

// linkHost is a test host: it compiles each named source once and resolves
// a specifier to the module of that name.
type linkHost struct {
	t    testing.TB
	srcs map[string]string
	mods map[string]*moejs.Module
	// calls counts the resolver's calls per "referrer -> specifier".
	calls map[string]int
}

func newLinkHost(t testing.TB, srcs map[string]string) *linkHost {
	return &linkHost{t: t, srcs: srcs, mods: make(map[string]*moejs.Module), calls: make(map[string]int)}
}

func (h *linkHost) module(name string) *moejs.Module {
	h.t.Helper()
	if m := h.mods[name]; m != nil {
		return m
	}
	m, err := moejs.Compile(name, h.srcs[name])
	require.NoError(h.t, err, name)
	h.mods[name] = m
	return m
}

func (h *linkHost) resolve(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
	h.calls[referrer.Name()+" -> "+specifier]++
	name := strings.TrimPrefix(specifier, "./")
	if _, ok := h.srcs[name]; !ok {
		return nil, fmt.Errorf("no module %q", specifier)
	}
	return h.module(name), nil
}

func (h *linkHost) link(entry string) (*moejs.Module, error) {
	return moejs.Link(h.module(entry), h.resolve)
}

func (h *linkHost) mustLink(entry string) *moejs.Module {
	h.t.Helper()
	m, err := h.link(entry)
	require.NoError(h.t, err)
	return m
}

// graphSources is a graph whose entry exports its own bindings and
// re-exports others: a name, a namespace, and those of a star export.
var graphSources = map[string]string{
	"main.js": `
import { count, inc } from "./counter.js";
import * as util from "./util.js";
export { inc };
export { twice as double } from "./util.js";
export * as utils from "./util.js";
export * from "./consts.js";
export function read() { return count; }
export function shout(s) { return util.upper(s) + "!"; }
export const self = "main";`,
	"counter.js": `
export let count = 0;
export function inc() { return ++count; }`,
	"util.js": `
export function upper(s) { return s.toUpperCase(); }
export function twice(n) { return 2 * n; }`,
	"consts.js": `
export const answer = 42;
export default "not re-exported by a star";`,
}

// TestLinkLoad loads a linked graph and reaches the entry's own exports and
// those it re-exports through Hook, Export and Exports.
func TestLinkLoad(t *testing.T) {
	h := newLinkHost(t, graphSources)
	entry := h.module("main.js")
	assert.Equal(t, []string{"./counter.js", "./util.js", "./consts.js"}, entry.Requests())
	assert.Nil(t, h.module("util.js").Requests())
	mod := h.mustLink("main.js")
	assert.NotSame(t, entry, mod)
	assert.Equal(t, "main.js", mod.Name())
	assert.Equal(t, []string{"answer", "double", "inc", "read", "self", "shout", "utils"}, mod.Exports())
	assert.Equal(t, []string{"read", "self", "shout"}, entry.Exports(), "the unlinked module has its own exports only")

	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	call := func(export string, members []string, args ...moejs.Value) string {
		t.Helper()
		res, err := rt.Call(mustHook(t, mod, export, members...), args...)
		require.NoError(t, err)
		return res.String()
	}
	assert.Equal(t, "0", call("read", nil))
	assert.Equal(t, "1", call("inc", nil), "a re-exported function")
	assert.Equal(t, "2", call("inc", nil))
	assert.Equal(t, "2", call("read", nil), "imports are live bindings")
	assert.Equal(t, "8", call("double", nil, moejs.Int(4)))
	assert.Equal(t, "HI!", call("shout", nil, moejs.String("hi")))
	assert.Equal(t, "YO", call("utils", []string{"upper"}, moejs.String("yo")), "a member of a re-exported namespace")

	v, ok := rt.Export("answer")
	require.True(t, ok)
	assert.Equal(t, "42", v.String())
	v, ok = rt.Export("utils")
	require.True(t, ok)
	assert.True(t, v.AsObject().IsModuleNamespace())
	_, ok = rt.Export("default")
	assert.False(t, ok, "a star export leaves default out")

	ok, err := rt.Has(mustHook(t, mod, "double"))
	require.NoError(t, err)
	assert.True(t, ok)
	_, err = mod.Hook("default")
	assert.ErrorIs(t, err, moejs.ErrHookNotFound)
	_, err = entry.Hook("double")
	assert.ErrorIs(t, err, moejs.ErrHookNotFound, "the unlinked module does not know its re-exports")
	res, err := rt.Call(mustHook(t, entry, "read"))
	require.NoError(t, err, "a hook of an own export of the unlinked module")
	assert.Equal(t, "2", res.String())
	again := h.mustLink("main.js")
	res, err = rt.Call(mustHook(t, again, "read"))
	require.NoError(t, err, "and of another Link of it")
	assert.Equal(t, "2", res.String())
	_, err = rt.Call(mustHook(t, again, "double"), moejs.Int(1))
	assert.ErrorIs(t, err, moejs.ErrHookNotFound, "a re-export is of the graph Link built")
	other := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, other.Load(h.mustLink("util.js")))
	_, err = other.Call(mustHook(t, entry, "read"))
	assert.ErrorIs(t, err, moejs.ErrHookNotFound, "a hook addresses the module the runtime loaded")
	ok, err = moejs.NewRuntime(moejs.Options{}).Has(mustHook(t, entry, "read"))
	require.NoError(t, err)
	assert.False(t, ok, "nothing loaded")
}

// TestLinkResolveOnce checks that Link asks the resolver once per module and
// specifier, that a module several graphs share keeps its identity, and
// that each runtime evaluates it once.
func TestLinkResolveOnce(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"a.js":      `import "shared.js"; import { n } from "shared.js"; import "b.js"; export function get() { return n; }`,
		"b.js":      `import "shared.js"; import "a.js";`,
		"c.js":      `export { n } from "shared.js";`,
		"shared.js": `globalThis.runs = (globalThis.runs || 0) + 1; export const n = runs;`,
	})
	a := h.mustLink("a.js")
	assert.Equal(t, map[string]int{"a.js -> shared.js": 1, "a.js -> b.js": 1, "b.js -> shared.js": 1, "b.js -> a.js": 1}, h.calls)
	c := h.mustLink("c.js")
	assert.Equal(t, 1, h.calls["c.js -> shared.js"])
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(a))
	res, err := rt.Call(mustHook(t, a, "get"))
	require.NoError(t, err)
	assert.Equal(t, "1", res.String(), "shared.js ran once in this runtime")
	rt = moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(c))
	v, ok := rt.Export("n")
	require.True(t, ok)
	assert.Equal(t, "1", v.String(), "and once in another runtime, of another graph")
}

// TestLinkReferrer checks the referrers Link passes to the resolver: the
// importing *Module, which a *Script could also be, a linked module a
// resolver returns standing for the module it linked, and a panic of the
// resolver propagating out of Link.
func TestLinkReferrer(t *testing.T) {
	var _ moejs.Referrer = (*moejs.Script)(nil)
	h := newLinkHost(t, map[string]string{
		"main.js": `import { v } from "dep.js"; export const out = v;`,
		"dep.js":  `import { w } from "leaf.js"; export const v = "dep" + w;`,
		"leaf.js": `export const w = "!";`,
		"top.js":  `import { v } from "dep.js"; export const t = v;`,
	})
	var seen []string
	resolve := func(referrer moejs.Referrer, spec string) (*moejs.Module, error) {
		m, ok := referrer.(*moejs.Module)
		require.True(t, ok, "%T", referrer)
		assert.Same(t, h.module(m.Name()), m)
		seen = append(seen, m.Name()+" -> "+spec)
		return h.resolve(referrer, spec)
	}
	main, err := moejs.Link(h.module("main.js"), resolve)
	require.NoError(t, err)
	assert.Equal(t, []string{"main.js -> dep.js", "dep.js -> leaf.js"}, seen)

	linkedDep, err := moejs.Link(h.module("dep.js"), h.resolve)
	require.NoError(t, err)
	seen = nil
	top, err := moejs.Link(h.module("top.js"), func(referrer moejs.Referrer, spec string) (*moejs.Module, error) {
		seen = append(seen, referrer.Name()+" -> "+spec)
		if spec == "dep.js" {
			return linkedDep, nil
		}
		assert.Same(t, linkedDep, referrer, "the referrer is the module the resolver returned")
		return h.resolve(referrer, spec)
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"top.js -> dep.js", "dep.js -> leaf.js"}, seen, "the linked module's requests are resolved again")
	for _, tc := range []struct {
		mod        *moejs.Module
		name, want string
	}{{main, "out", "dep!"}, {top, "t", "dep!"}} {
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.Load(tc.mod))
		v, ok := rt.Export(tc.name)
		require.True(t, ok)
		assert.Equal(t, tc.want, v.String())
	}

	assert.PanicsWithValue(t, "resolver boom", func() {
		_, _ = moejs.Link(h.module("main.js"), func(moejs.Referrer, string) (*moejs.Module, error) { panic("resolver boom") })
	})
}

// TestLinkErrors checks the errors of Link: a failed resolution is a
// *ResolveError and an import that does not resolve a *SyntaxError, both at
// the entry of the importing module; Load refuses a module that imports
// unless Link linked it.
func TestLinkErrors(t *testing.T) {
	errHost := errors.New("host says no")
	srcs := map[string]string{
		"main.js":      "export const x = 1;\nimport { missing } from \"a.js\";",
		"a.js":         `export const present = 1;`,
		"amb.js":       "import { dup } from \"star.js\";",
		"star.js":      `export * from "s1.js"; export * from "s2.js";`,
		"s1.js":        `export const dup = 1;`,
		"s2.js":        `export const dup = 2;`,
		"indirect.js":  "export { nope as yes } from \"a.js\";",
		"unknown.js":   "\n\n  import \"nowhere.js\";",
		"refused.js":   `import "secret.js";`,
		"nilmod.js":    `import "nil.js";`,
		"secret.js":    ``,
		"nil.js":       ``,
		"deep.js":      `import "deeper.js";`,
		"deeper.js":    "import { gone } from \"a.js\";",
		"circular.js":  `export { x } from "circular2.js";`,
		"circular2.js": `export { x } from "circular.js";`,
		"usecirc.js":   `import { x } from "circular.js";`,
	}
	h := newLinkHost(t, srcs)
	resolve := func(referrer moejs.Referrer, spec string) (*moejs.Module, error) {
		switch spec {
		case "secret.js":
			return nil, errHost
		case "nil.js":
			return nil, nil
		}
		return h.resolve(referrer, spec)
	}
	link := func(entry string) error {
		_, err := moejs.Link(h.module(entry), resolve)
		return err
	}
	syntaxErr := func(entry, want string) {
		t.Helper()
		var se *moejs.SyntaxError
		require.ErrorAs(t, link(entry), &se, entry)
		assert.Equal(t, want, se.Error())
	}
	syntaxErr("main.js", `main.js:2:10: SyntaxError: The requested module 'a.js' does not provide an export named 'missing'`)
	syntaxErr("amb.js", `amb.js:1:10: SyntaxError: The requested module 'star.js' contains conflicting star exports for name 'dup'`)
	syntaxErr("indirect.js", `indirect.js:1:10: SyntaxError: The requested module 'a.js' does not provide an export named 'nope'`)
	syntaxErr("deep.js", `deeper.js:1:10: SyntaxError: The requested module 'a.js' does not provide an export named 'gone'`)
	syntaxErr("usecirc.js", `circular2.js:1:10: SyntaxError: The requested module 'circular.js' does not provide an export named 'x'`)

	var re *moejs.ResolveError
	require.ErrorAs(t, link("unknown.js"), &re)
	assert.Equal(t, moejs.ResolveError{File: "unknown.js", Line: 3, Column: 10, Specifier: "nowhere.js", Err: re.Err}, *re)
	assert.Equal(t, `unknown.js:3:10: cannot resolve module "nowhere.js": no module "nowhere.js"`, re.Error())
	err := link("refused.js")
	assert.ErrorIs(t, err, errHost)
	assert.Equal(t, `refused.js:1:8: cannot resolve module "secret.js": host says no`, err.Error())
	require.ErrorAs(t, link("nilmod.js"), &re)
	assert.Equal(t, "nil.js", re.Specifier)

	_, err = moejs.Link(h.module("refused.js"), nil)
	assert.ErrorIs(t, err, moejs.ErrNoResolver)
	assert.Equal(t, `refused.js:1:8: cannot resolve module "secret.js": no resolver: the host must resolve the module's requests`, err.Error())

	err = moejs.NewRuntime(moejs.Options{}).Load(h.module("refused.js"))
	assert.EqualError(t, err, `moejs: module "refused.js" imports "secret.js": link it with moejs.Link and a resolver`)

	free := h.module("a.js")
	linked, err := moejs.Link(free, nil)
	require.NoError(t, err)
	assert.Same(t, free, linked, "an import-free module needs no resolver and no graph")
}

// TestLinkFailedGraph checks the hooks of a graph whose evaluation failed
// before the entry's top level ran to its end or first await: Call and Has
// return Load's error, Export refuses functions and namespaces and reads the
// rest. A failure after the entry's first await leaves its hooks callable,
// as for a module that imports nothing.
func TestLinkFailedGraph(t *testing.T) {
	srcs := map[string]string{
		"sync.js":  `export { f, ns, early } from "throws.js"; export function g() { return 1; }`,
		"async.js": `import "late.js"; export { f, ns, early } from "throws.js"; export function g() { return 1; }`,
		"throws.js": `export const early = 1; export function f() { return late; }
			export * as ns from "throws.js";
			throw new Error("dep"); export let late = 2;`,
		"late.js":  `await 0;`,
		"entry.js": `import "late.js"; export function g() { return 1; } await 0; throw new Error("entry");`,
	}
	for _, entry := range []string{"sync.js", "async.js"} {
		h := newLinkHost(t, srcs)
		mod := h.mustLink(entry)
		rt := moejs.NewRuntime(moejs.Options{})
		loadErr := rt.Load(mod)
		require.EqualError(t, loadErr, "Error: dep", entry)
		_, err := rt.Call(mustHook(t, mod, "g"))
		assert.Same(t, loadErr, err, entry)
		ok, err := rt.Has(mustHook(t, mod, "f"))
		assert.False(t, ok)
		assert.Same(t, loadErr, err, entry)
		_, ok = rt.Export("f")
		assert.False(t, ok, entry)
		_, ok = rt.Export("ns")
		assert.False(t, ok, entry)
		v, ok := rt.Export("early")
		require.True(t, ok, entry)
		assert.Equal(t, "1", v.String())
	}

	h := newLinkHost(t, srcs)
	mod := h.mustLink("entry.js")
	rt := moejs.NewRuntime(moejs.Options{})
	require.EqualError(t, rt.Load(mod), "Error: entry")
	res, err := rt.Call(mustHook(t, mod, "g"))
	require.NoError(t, err, "the entry's top level ran to its first await")
	assert.Equal(t, "1", res.String())
}

// TestLinkTopLevelAwait checks a graph whose modules await: Load returns
// once no job is left, with ErrModulePending while a dependency still
// awaits a host promise, and the evaluation goes on when the host settles
// it.
func TestLinkTopLevelAwait(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `import { v } from "dep.js"; import "log.js"; log("main");
			export function get() { return v; }
			export function order() { return seen.join(); }`,
		"dep.js": `import "log.js"; log("dep1"); export const v = await wait(); log("dep2");`,
		"log.js": `globalThis.seen = []; globalThis.log = (s) => seen.push(s);`,
	})
	mod := h.mustLink("main.js")
	rt := moejs.NewRuntime(moejs.Options{})
	var resolve func(moejs.Value) error
	require.NoError(t, rt.SetGlobal("wait", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		var p moejs.Value
		p, resolve, _ = rt.NewPromise()
		return p, nil
	})))
	assert.ErrorIs(t, rt.Load(mod), moejs.ErrModulePending)
	_, err := rt.Call(mustHook(t, mod, "get"))
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc, "main has not run: v is uninitialized")
	assert.Contains(t, exc.Error(), "ReferenceError")

	res, err := rt.Call(mustHook(t, mod, "order"))
	require.NoError(t, err)
	assert.Equal(t, "dep1", res.String())

	require.NoError(t, resolve(moejs.String("ready")))
	res, err = rt.Call(mustHook(t, mod, "get"))
	require.NoError(t, err)
	assert.Equal(t, "ready", res.String())
	res, err = rt.Call(mustHook(t, mod, "order"))
	require.NoError(t, err)
	assert.Equal(t, "dep1,dep2,main", res.String())
}

// TestLinkEarlyExports checks an import-free module that an import cycle
// calls into before its body runs: its lexical bindings keep their TDZ
// checks.
func TestLinkEarlyExports(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `import "b.js"; import { g } from "x.js"; export { out } from "b.js"; export function f() { return g(); }`,
		"b.js":    `import { f } from "main.js"; export let out; try { out = "" + f(); } catch (e) { out = e.name; }`,
		"x.js":    `let m = 1; export function g() { return m; }`,
	})
	mod := h.mustLink("main.js")
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	v, ok := rt.Export("out")
	require.True(t, ok)
	assert.Equal(t, "ReferenceError", v.String())
}

// TestLinkScriptGlobals checks that the modules of a graph see the global
// lexical bindings of the scripts that ran before, through the global
// declarative environment, and stay strict mode code.
func TestLinkScriptGlobals(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `import { log } from "dep.js"; export function get() { return log.join() + "|" + counter; }`,
		"dep.js": `export const log = [typeof this, counter, limit, globalThis.counter === undefined, sloppyVar];
			counter++;
			try { limit = 0; } catch (e) { log.push(e.name); }
			try { implicit = 1; } catch (e) { log.push(e.name); }`,
	})
	mod := h.mustLink("main.js")
	rt := moejs.NewRuntime(moejs.Options{})
	s, err := moejs.CompileScript("globals.js", `let counter = 1; const limit = 5; var sloppyVar = "var";`)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	require.NoError(t, rt.Load(mod))

	res, err := rt.Call(mustHook(t, mod, "get"))
	require.NoError(t, err)
	assert.Equal(t, "undefined,1,5,true,var,TypeError,ReferenceError|2", res.String())
	s, err = moejs.CompileScript("after.js", `counter + ":" + typeof implicit`)
	require.NoError(t, err)
	res, err = rt.RunScript(s)
	require.NoError(t, err)
	assert.Equal(t, "2:undefined", res.String(), "the module wrote the script's binding")
}

// TestLinkInterrupt interrupts the evaluation of a linked graph in a
// dependency's top level: Load returns the interrupt, the entry's hooks
// return it, and a runtime that loads the same graph afterwards is not
// affected.
func TestLinkInterrupt(t *testing.T) {
	h := newLinkHost(t, map[string]string{
		"main.js": `import "spin.js"; export function f() { return 1; }`,
		"spin.js": `stop(); for (;;);`,
	})
	mod := h.mustLink("main.js")
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		rt.Interrupt("stop")
		return moejs.Undefined(), nil
	})))
	loadErr := rt.Load(mod)
	var ie *moejs.InterruptedError
	require.ErrorAs(t, loadErr, &ie)
	assert.Equal(t, "stop", ie.Value)
	rt.ClearInterrupt()
	_, err := rt.Call(mustHook(t, mod, "f"))
	assert.Same(t, loadErr, err)

	rt2 := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt2.SetGlobal("stop", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Undefined(), nil
	})))
	rt2.Interrupt("early")
	require.ErrorAs(t, rt2.Load(mod), &ie)
	assert.Equal(t, "early", ie.Value, "an interrupt pending on entry stops Load")
}

// TestLinkLoadStopped checks the hooks after an interrupt or a host panic
// stopped Load: they return Load's error when the entry's top level had not
// run to its end or first await, in the asynchronous phase of a graph too,
// and stay callable when it had.
func TestLinkLoadStopped(t *testing.T) {
	tests := []struct {
		name      string
		srcs      map[string]string
		panics    bool // Load's error is an *InternalError, else an *InterruptedError
		hooksFail bool
	}{
		{"interrupt in the async phase of a graph", map[string]string{
			"main.js": `import "dep.js"; export function f() { return 1; } export const c = 1;`,
			"dep.js":  `await 0; stop(); for (;;);`,
		}, false, true},
		{"interrupt after the entry awaited", map[string]string{
			"main.js": `import "dep.js"; export function f() { return 1; } await 0; stop(); for (;;);`,
			"dep.js":  `await 0;`,
		}, false, false},
		{"host panic in a dependency", map[string]string{
			"main.js": `import "dep.js"; export function f() { return 1; } export const c = 1;`,
			"dep.js":  `boom();`,
		}, true, true},
		{"host panic in the async phase of a graph", map[string]string{
			"main.js": `import "dep.js"; export function f() { return 1; } export const c = 1;`,
			"dep.js":  `await 0; boom();`,
		}, true, true},
		{"host panic after the entry awaited", map[string]string{
			"main.js": `import "dep.js"; export function f() { return 1; } export const c = 1; await 0; boom();`,
			"dep.js":  `await 0;`,
		}, true, false},
		{"host panic in a module that imports nothing", map[string]string{
			"main.js": `export function f() { return 1; } export const c = 1; boom();`,
		}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mod := newLinkHost(t, tt.srcs).mustLink("main.js")
			rt := moejs.NewRuntime(moejs.Options{})
			require.NoError(t, rt.SetGlobal("stop", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
				rt.Interrupt("stop")
				return moejs.Undefined(), nil
			})))
			require.NoError(t, rt.SetGlobal("boom", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
				panic("host boom")
			})))
			loadErr := rt.Load(mod)
			if tt.panics {
				var ie *moejs.InternalError
				require.ErrorAs(t, loadErr, &ie)
				assert.Equal(t, "host boom", ie.Value)
			} else {
				var ie *moejs.InterruptedError
				require.ErrorAs(t, loadErr, &ie)
				rt.ClearInterrupt()
			}
			f := mustHook(t, mod, "f")
			res, err := rt.Call(f)
			ok, hasErr := rt.Has(f)
			if !tt.hooksFail {
				require.NoError(t, err)
				assert.Equal(t, "1", res.String())
				assert.True(t, ok)
				assert.NoError(t, hasErr)
				return
			}
			assert.Same(t, loadErr, err)
			assert.False(t, ok)
			assert.Same(t, loadErr, hasErr)
			_, ok = rt.Export("f")
			assert.False(t, ok)
		})
	}
}

// TestLinkConcurrentLoad loads one linked graph in many runtimes at once;
// run it with -race.
func TestLinkConcurrentLoad(t *testing.T) {
	mod := newLinkHost(t, graphSources).mustLink("main.js")
	inc := mustHook(t, mod, "inc")
	read := mustHook(t, mod, "read")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt := moejs.NewRuntime(moejs.Options{})
			if errs[i] = rt.Load(mod); errs[i] != nil {
				return
			}
			for range 100 {
				if _, errs[i] = rt.Call(inc); errs[i] != nil {
					return
				}
			}
			res, err := rt.Call(read)
			if err == nil && res.String() != "100" {
				err = fmt.Errorf("read %s, want 100: runtimes share no state", res.String())
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		assert.NoError(t, err)
	}
}

// ExampleLink links a plugin with a module it imports, which the host
// compiles from its own sources.
func ExampleLink() {
	sources := map[string]string{
		"plugin.js":    `import { greet } from "lib/greet.js"; export function hello(name) { return greet(name) + "!"; }`,
		"lib/greet.js": `export function greet(name) { return "hello, " + name; }`,
	}
	modules := make(map[string]*moejs.Module)
	compile := func(name string) (*moejs.Module, error) {
		if m, ok := modules[name]; ok {
			return m, nil
		}
		src, ok := sources[name]
		if !ok {
			return nil, fmt.Errorf("no module %q", name)
		}
		m, err := moejs.Compile(name, src)
		if err == nil {
			modules[name] = m
		}
		return m, err
	}

	// Once per process: compile, resolve and link the graph.
	entry, err := compile("plugin.js")
	if err != nil {
		panic(err)
	}
	mod, err := moejs.Link(entry, func(_ moejs.Referrer, specifier string) (*moejs.Module, error) {
		return compile(specifier)
	})
	if err != nil {
		panic(err)
	}
	hello, err := mod.Hook("hello")
	if err != nil {
		panic(err)
	}

	// Per runtime: instantiate and evaluate the graph, then call.
	rt := moejs.NewRuntime(moejs.Options{})
	if err := rt.Load(mod); err != nil {
		panic(err)
	}
	res, err := rt.Call(hello, moejs.String("moejs"))
	if err != nil {
		panic(err)
	}
	fmt.Println(res.String())
	// Output: hello, moejs!
}

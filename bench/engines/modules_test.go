package engines_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/grafana/sobek"
	"github.com/grafana/sobek/parser"
	"github.com/stretchr/testify/assert"
)

// errNoModule is the resolvers' error for a specifier that names no module.
var errNoModule = errors.New("no module")

// The graph drivers link the module graph of srcs["main.js"] on one engine,
// evaluate it and return what its modules logged, then the outcome: "ok",
// "pending", "exc <thrown>", "link <error name>" or "link unresolved".
// import() loads the modules of srcs through the same resolver.

// sobekGraph links with Sobek's ParseModule, Link and
// CyclicModuleRecordEvaluate, parsing each module once, and loads the
// modules of import() with SetImportModuleDynamically and
// FinishLoadingImportModule.
func sobekGraph(srcs map[string]string) []string {
	rt := sobek.New()
	var lines []string
	_ = rt.Set("log", func(s string) { lines = append(lines, s) })
	recs := make(map[string]sobek.ModuleRecord)
	var resolve sobek.HostResolveImportedModuleFunc
	resolve = func(_ any, spec string) (sobek.ModuleRecord, error) {
		if m, ok := recs[spec]; ok {
			return m, nil
		}
		src, ok := srcs[spec]
		if !ok {
			return nil, fmt.Errorf("%w %q", errNoModule, spec)
		}
		m, err := sobek.ParseModule(spec, src, resolve, parser.WithDisableSourceMaps)
		if err != nil {
			return nil, err
		}
		recs[spec] = m
		return m, nil
	}
	rt.SetImportModuleDynamically(func(referrer any, specifier sobek.Value, capability any) {
		m, err := resolve(referrer, specifier.String())
		rt.FinishLoadingImportModule(referrer, specifier, capability, m, err)
	})
	m, err := resolve(nil, "main.js")
	if err == nil {
		err = m.(*sobek.SourceTextModuleRecord).Link()
	}
	if err != nil {
		return append(lines, linkOutcome(err))
	}
	p := rt.CyclicModuleRecordEvaluate(m.(*sobek.SourceTextModuleRecord), resolve)
	switch p.State() {
	case sobek.PromiseStatePending:
		return append(lines, "pending")
	case sobek.PromiseStateRejected:
		v := p.Result()
		if ex, ok := v.Export().(*sobek.Exception); ok {
			v = ex.Value()
		}
		thrown, _, _ := strings.Cut(v.String(), "\n")
		// Sobek's Link resolves no import: the evaluation throws what Link
		// should have (a deviation the test normalizes).
		if strings.HasPrefix(thrown, "SyntaxError: The requested module") {
			return append(lines, "link SyntaxError")
		}
		return append(lines, "exc "+thrown)
	}
	return append(lines, "ok")
}

// moejsGraph links with Compile, Link and Load, and loads the modules of
// import() with an Importer.
func moejsGraph(srcs map[string]string) []string {
	mods := make(map[string]*moejs.Module)
	compile := func(name string) (*moejs.Module, error) {
		if m, ok := mods[name]; ok {
			return m, nil
		}
		src, ok := srcs[name]
		if !ok {
			return nil, fmt.Errorf("%w %q", errNoModule, name)
		}
		m, err := moejs.Compile(name, src)
		if err != nil {
			return nil, err
		}
		mods[name] = m
		return m, nil
	}
	resolve := func(_ moejs.Referrer, spec string) (*moejs.Module, error) { return compile(spec) }
	rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: resolve}})
	var lines []string
	_ = rt.SetGlobal("log", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		lines = append(lines, r.ToGo(moejs.Arg(args, 0)).(string))
		return moejs.Undefined(), nil
	}))
	entry, err := compile("main.js")
	if err == nil {
		entry, err = moejs.Link(entry, resolve)
	}
	if err != nil {
		return append(lines, linkOutcome(err))
	}
	var exc *moejs.Exception
	switch err := rt.Load(entry); {
	case err == nil:
		return append(lines, "ok")
	case errors.Is(err, moejs.ErrModulePending):
		return append(lines, "pending")
	case errors.As(err, &exc):
		thrown, _, _ := strings.Cut(exc.Error(), "\n") // no stack
		return append(lines, "exc "+thrown)
	default:
		return append(lines, "error "+err.Error())
	}
}

// linkOutcome is the outcome of a link error: "link unresolved" for a
// specifier the resolver refused, else "link" and the error's name.
func linkOutcome(err error) string {
	if errors.Is(err, errNoModule) {
		return "link unresolved"
	}
	for _, w := range strings.FieldsFunc(err.Error(), func(r rune) bool { return r == ' ' || r == ':' }) {
		if strings.HasSuffix(w, "Error") {
			return "link " + w
		}
	}
	return "link " + err.Error()
}

// TestModuleGraphsMatchSobek links and evaluates the same module graphs on
// Sobek and natively and compares what the modules observe: live bindings,
// namespaces, cycles, evaluation order with top-level await, and errors.
func TestModuleGraphsMatchSobek(t *testing.T) {
	cases := []struct {
		name string
		srcs map[string]string
		// native is moejs's log where it deviates from Sobek's (see NOTES,
		// "Wave 9 modules notes").
		native []string
	}{
		{name: "import forms", srcs: map[string]string{
			"main.js": `import d, { a, b as c } from "m.js"; import * as ns from "m.js"; import "side.js";
				log([d, a, c, ns.a, ns.default, globalThis.side].join());`,
			"m.js":    `export default "D"; export const a = 1; export let b = 2;`,
			"side.js": `globalThis.side = "S";`,
		}},
		{name: "live bindings", srcs: map[string]string{
			"main.js":  `import { n, inc } from "count.js"; import * as ns from "count.js"; log("" + n); inc(); inc(); log(n + "," + ns.n);`,
			"count.js": `export let n = 0; export function inc() { n++; }`,
		}},
		{name: "imports are immutable", srcs: map[string]string{
			"main.js": `import { n } from "m.js"; import * as ns from "m.js";
				try { n = 1; } catch (e) { log(e.name); }
				try { ns.n = 1; } catch (e) { log(e.name); }
				try { delete ns.n; } catch (e) { log(e.name); }
				log("" + delete ns.nope);
				try { Object.defineProperty(ns, "n", { value: 2 }); } catch (e) { log(e.name); }
				log("" + Reflect.defineProperty(ns, "n", { value: 0, writable: true, enumerable: true, configurable: false }));
				log("" + n);`,
			"m.js": `export let n = 0;`,
		},
			// Reflect.defineProperty of a namespace key with its own value
			// and attributes is true ([[DefineOwnProperty]] steps 7-8,
			// test262 namespace/internals/define-own-property.js).
			native: []string{"TypeError", "TypeError", "TypeError", "true", "TypeError", "true", "0", "ok"},
		},
		{name: "namespace object", srcs: map[string]string{
			"main.js": `import * as ns from "m.js";
				log(Object.keys(ns).join());
				log(Reflect.ownKeys(ns).map(String).join());
				log(Object.prototype.toString.call(ns) + " " + ns[Symbol.toStringTag]);
				log(Object.getPrototypeOf(ns) + " " + Object.isExtensible(ns) + " " + Object.isFrozen(ns) + " " + Object.isSealed(ns));
				const d = Object.getOwnPropertyDescriptor(ns, "b");
				log([d.value, d.writable, d.enumerable, d.configurable].join());
				const t = Object.getOwnPropertyDescriptor(ns, Symbol.toStringTag);
				log([t.value, t.writable, t.enumerable, t.configurable].join());
				log(["b" in ns, "z" in ns, Object.prototype.hasOwnProperty.call(ns, "a"), Reflect.setPrototypeOf(ns, null), Reflect.setPrototypeOf(ns, {}), Reflect.preventExtensions(ns)].join());
				for (const k in ns) log("in " + k);`,
			"m.js": `export const b = 2, a = 1, B = 3, _ = 4, $ = 5; export { a as "0" }; export default 6;`,
		},
			// for-in enumerates the namespace's [[OwnPropertyKeys]], which
			// sorts the names; Sobek enumerates them in declaration order.
			native: []string{"$,0,B,_,a,b,default", "$,0,B,_,a,b,default,Symbol(Symbol.toStringTag)", "[object Module] Module",
				"null false false true", "2,true,true,false", "Module,false,false,false", "true,false,true,true,false,true",
				"in $", "in 0", "in B", "in _", "in a", "in b", "in default", "ok"},
		},
		{name: "export from", srcs: map[string]string{
			"main.js": `import { x, y, ns, z, w } from "re.js"; import * as re from "re.js"; log([x, y, ns.v, z, w].join()); log(Object.keys(re).join());`,
			"re.js":   `export { v as x } from "a.js"; export * from "b.js"; export * as ns from "a.js"; export { default as z } from "c.js"; import { v } from "a.js"; export { v as w };`,
			"a.js":    `export const v = "A";`,
			"b.js":    `export const y = "B"; export default "not re-exported";`,
			"c.js":    `export default "C";`,
		}},
		{name: "ambiguous star left out", srcs: map[string]string{
			"main.js": `import * as ns from "s.js"; log(Object.keys(ns).join()); log("" + ("x" in ns));`,
			"s.js":    `export * from "a.js"; export * from "b.js";`,
			"a.js":    `export const x = 1, y = 2;`,
			"b.js":    `export const x = 3, z = 4;`,
		}},
		{name: "same binding through two stars", srcs: map[string]string{
			"main.js": `import { v } from "s.js"; log(v);`,
			"s.js":    `export * from "a.js"; export * from "a2.js";`,
			"a2.js":   `export * from "a.js";`,
			"a.js":    `export const v = "A";`,
		}},
		{name: "ambiguous import", srcs: map[string]string{
			"main.js": `import { x } from "s.js";`,
			"s.js":    `export * from "a.js"; export * from "b.js";`,
			"a.js":    `export const x = 1;`,
			"b.js":    `export const x = 3;`,
		}},
		{name: "missing export", srcs: map[string]string{
			"main.js": `import { nope } from "a.js";`,
			"a.js":    `export const x = 1;`,
		}},
		{name: "missing indirect export", srcs: map[string]string{
			"main.js": `import "re.js";`,
			"re.js":   `export { nope } from "a.js";`,
			"a.js":    `export const x = 1;`,
		}},
		{name: "circular indirect export", srcs: map[string]string{
			"main.js": `import { x } from "a.js";`,
			"a.js":    `export { x } from "b.js";`,
			"b.js":    `export { x } from "a.js";`,
		}},
		{name: "unresolved module", srcs: map[string]string{
			"main.js": `import "nowhere.js";`,
		}},
		{name: "cycle order and hoisting", srcs: map[string]string{
			"main.js": `import { fb } from "b.js"; log("main"); export function fa() { return "fa"; } log(fb());`,
			"b.js":    `import { fa } from "main.js"; import "c.js"; log("b " + fa()); export function fb() { return "fb>" + fa(); }`,
			"c.js":    `log("c");`,
		}},
		{name: "cycle TDZ", srcs: map[string]string{
			"main.js": `import { read } from "b.js"; export let x = 1; export const y = 2; log(read());`,
			"b.js": `import { x, y } from "main.js"; import * as ns from "main.js";
				export function read() { return x + "," + y; }
				try { x; } catch (e) { log("x " + e.name); }
				try { ns.y; } catch (e) { log("ns.y " + e.name); }
				try { Object.keys(ns); } catch (e) { log("keys " + e.name); }
				log(Reflect.ownKeys(ns).map(String).join());
				try { Object.getOwnPropertyDescriptor(ns, "x"); } catch (e) { log("desc " + e.name); }
				try { log(JSON.stringify(ns)); } catch (e) { log("json " + e.name); }`,
		}},
		{name: "self import", srcs: map[string]string{
			"main.js": `import * as self from "main.js"; import { v as w } from "main.js"; export const v = 1; log([self.v, w, Object.keys(self)].join());`,
		}},
		{name: "evaluates once", srcs: map[string]string{
			"main.js":   `import "a.js"; import "b.js"; import "shared.js"; log("main");`,
			"a.js":      `import "shared.js"; log("a");`,
			"b.js":      `import "shared.js"; log("b");`,
			"shared.js": `log("shared");`,
		}},
		{name: "a dependency throws", srcs: map[string]string{
			"main.js": `import "a.js"; log("main");`,
			"a.js":    `import "b.js"; log("a");`,
			"b.js":    `log("b"); throw new TypeError("b fails");`,
		}},
		{name: "cycle member throws", srcs: map[string]string{
			"main.js": `import "a.js"; log("main");`,
			"a.js":    `import "b.js"; log("a"); throw "a fails";`,
			"b.js":    `import "a.js"; log("b");`,
		}},
		{name: "string export names", srcs: map[string]string{
			"main.js": `import { "a b" as ab, "☃" as snow } from "m.js"; import * as ns from "m.js"; log([ab, snow, Object.keys(ns)].join());`,
			"m.js":    `const x = 1, y = 2; export { x as "a b", y as "☃" };`,
		}},
		{name: "default export forms", srcs: map[string]string{
			"main.js": `import f from "f.js"; import c from "c.js"; import e from "e.js"; log([f.name, f(), c.name, typeof c, e].join());`,
			"f.js":    `export default function () { return "f"; }`,
			"c.js":    `export default class {}`,
			"e.js":    `export default 1 + 2;`,
		}},
		{name: "top-level await order", srcs: map[string]string{
			"main.js": `import "a.js"; import "b.js"; log("main");`,
			"a.js":    `log("a1"); await 0; log("a2");`,
			"b.js":    `import "c.js"; log("b");`,
			"c.js":    `log("c1"); await null; await null; log("c2");`,
		}},
		{name: "top-level await in a cycle", srcs: map[string]string{
			"main.js": `import "a.js"; log("main");`,
			"a.js":    `import "b.js"; log("a1"); await 0; log("a2");`,
			"b.js":    `import "a.js"; import "c.js"; log("b");`,
			"c.js":    `log("c1"); await 0; log("c2");`,
		}},
		{name: "async sibling waits", srcs: map[string]string{
			"main.js":  `import "later.js"; import { v } from "slow.js"; import { w } from "fast.js"; log(v + w);`,
			"later.js": `globalThis.later = (r) => Promise.resolve().then(() => Promise.resolve()).then(r);`,
			"slow.js":  `export let v = "s"; await new Promise(r => later(r)); v = "S"; log("slow");`,
			"fast.js":  `export const w = "f"; log("fast");`,
		}},
		{name: "top-level await rejects", srcs: map[string]string{
			"main.js": `import "a.js"; import "b.js"; log("main");`,
			"a.js":    `log("a"); await 0; throw new RangeError("late");`,
			"b.js":    `log("b1"); await 0; await 0; log("b2");`,
		}},
		{name: "top-level await pending", srcs: map[string]string{
			"main.js": `import "a.js"; log("main");`,
			"a.js":    `log("a"); await new Promise(() => {});`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := sobekGraph(c.srcs)
			got := moejsGraph(c.srcs)
			if c.native != nil {
				assert.NotEqual(t, want, c.native, "the deviation from Sobek is gone")
				want = c.native
			}
			assert.Equal(t, want, got)
		})
	}
}

// TestDynamicImportMatchesSobek evaluates module graphs whose code imports
// modules dynamically on Sobek and natively and compares what they observe:
// the namespace, the one evaluation of a module per runtime, the errors,
// top-level await and import.meta.
func TestDynamicImportMatchesSobek(t *testing.T) {
	const counted = `globalThis.runs = (globalThis.runs | 0) + 1; export const v = "c";`
	cases := []struct {
		name string
		srcs map[string]string
		// native is moejs's log where it deviates from Sobek's.
		native []string
	}{
		{name: "namespace", srcs: map[string]string{
			"main.js": `import("m.js").then(ns => log([ns.v, ns.w, Object.prototype.toString.call(ns), Object.keys(ns), Object.getPrototypeOf(ns)].join()));`,
			"m.js":    `export const w = 2, v = 1;`,
		}},
		{name: "the static instance", srcs: map[string]string{
			"main.js": `import * as s from "c.js"; import("c.js").then(ns => log((ns === s) + " " + globalThis.runs));`,
			"c.js":    counted,
		}},
		{name: "twice", srcs: map[string]string{
			"main.js": `Promise.all([import("c.js"), import("c.js")]).then(([a, b]) => log((a === b) + " " + globalThis.runs));`,
			"c.js":    counted,
		}},
		{name: "a graph", srcs: map[string]string{
			"main.js": `log("main"); import("a.js").then(ns => log(ns.v));`,
			"a.js":    `import { w } from "b.js"; log("a"); export const v = w + "a";`,
			"b.js":    `log("b"); export const w = "b";`,
		}},
		{name: "from nested code", srcs: map[string]string{
			"main.js": `import("a.js").then(ns => ns.f()).then(ns => log(ns.w));`,
			"a.js":    `export function f() { return (() => import("b.js"))(); }`,
			"b.js":    `export const w = "b";`,
		}},
		{name: "itself", srcs: map[string]string{
			"main.js": `import * as self from "main.js"; export const x = "x"; import("main.js").then(ns => log((ns === self) + " " + ns.x));`,
		}},
		{name: "a specifier converted", srcs: map[string]string{
			"main.js": `import({ toString() { return "c.js"; } }).then(ns => log(ns.v));
				import({ toString() { throw new RangeError("spec"); } }).catch(e => log(e.name + " " + e.message));`,
			"c.js": counted,
		}},
		{name: "a missing module", srcs: map[string]string{
			"main.js": `import("nope.js").then(() => log("fulfilled"), () => log("rejected"));`,
		}},
		{name: "an evaluation error", srcs: map[string]string{
			"main.js": `const f = () => import("bad.js").catch(e => e); const e1 = await f(), e2 = await f(); log(e1 + " " + (e1 === e2) + " " + globalThis.runs);`,
			"bad.js":  `globalThis.runs = (globalThis.runs | 0) + 1; throw new TypeError("bad");`,
		}},
		{name: "a thenable namespace", srcs: map[string]string{
			"main.js": `import("t.js").then(v => log(v));`,
			"t.js":    `export function then(resolve) { resolve("T"); }`,
		}},
		{name: "top-level await in the module", srcs: map[string]string{
			"main.js": `import("m.js").then(ns => log("v" + ns.v)); log("sync");`,
			"m.js":    `export let v = 1; await 0; v = 2;`,
		}},
		{name: "a rejection after an await", srcs: map[string]string{
			"main.js": `import("m.js").catch(e => log(String(e)));`,
			"m.js":    `await 0; throw new RangeError("late");`,
		}},
		{name: "from top-level await", srcs: map[string]string{
			"main.js": `const { v } = await import("m.js"); log(v);`,
			"m.js":    `export const v = await Promise.resolve("m");`,
		}},
		// AsyncModuleExecutionFulfilled and AsyncModuleExecutionRejected
		// settle a module's promise before those of the modules waiting on it
		// (ECMA-262 2025 §16.2.1.5.3.4-5, test262 top-level-await/
		// fulfillment-order.js and rejection-order.js); Sobek settles them
		// first.
		{name: "the fulfillment order", srcs: settleOrder("resolve"), native: []string{"B,A", "ok"}},
		{name: "the rejection order", srcs: settleOrder("reject"), native: []string{"B,A", "ok"}},
		{name: "import.meta", srcs: map[string]string{
			"main.js": `import { m } from "b.js";
				log([typeof import.meta, Object.getPrototypeOf(import.meta), import.meta === import.meta, m === import.meta, Object.keys(import.meta).length, Object.isExtensible(import.meta)].join());`,
			"b.js": `export const m = import.meta;`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := sobekGraph(c.srcs)
			got := moejsGraph(c.srcs)
			if c.native != nil {
				assert.NotEqual(t, want, c.native, "the deviation from Sobek is gone")
				want = c.native
			}
			assert.Equal(t, want, got)
		})
	}
}

// settleOrder are modules a and b, where a waits on b, which awaits p1
// that main settles with settle once both started, and main logs the order
// in which the promises of their import() calls settle.
func settleOrder(settle string) map[string]string {
	return map[string]string{
		"main.js": `import { p1, pA, pB } from "setup.js"; const order = [];
			const a = pB.promise.then(() => import("a.js").finally(() => order.push("A"))).catch(() => {});
			const b = import("b.js").finally(() => order.push("B")).catch(() => {});
			Promise.all([pA.promise, pB.promise]).then(p1.` + settle + `);
			await Promise.all([a, b]); log(order.join());`,
		"a.js":      `import "aStart.js"; import "b.js";`,
		"aStart.js": `import { pA } from "setup.js"; pA.resolve();`,
		"b.js":      `import "bStart.js"; import { p1 } from "setup.js"; await p1.promise;`,
		"bStart.js": `import { pB } from "setup.js"; pB.resolve();`,
		"setup.js": `const d = () => { let resolve, reject; const promise = new Promise((s, j) => { resolve = s; reject = j; }); return { promise, resolve, reject }; };
			export const p1 = d(), pA = d(), pB = d();`,
	}
}

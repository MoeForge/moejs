package tsdiff

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/syntax"
)

// piAnySrc is a value whose every property is another such value, which
// can be called and constructed: the stub of every name a pi extension
// imports from a package.
const piAnySrc = `
const h = {
	get(t, k) {
		if (k === Symbol.toPrimitive) return () => "stub";
		if (k === Symbol.iterator) return function* () {};
		if (k === "then") return undefined;
		return mk();
	},
	apply() { return mk(); },
	construct() { return mk(); },
};
export function mk() { return new Proxy(function () {}, h); }
`

// piDriverSrc runs an extension's default export, its factory, with a pi
// object that records the calls.
const piDriverSrc = `
import factory from "ENTRY";
export const log = [];
export const pi = new Proxy({}, {
	get(t, k) {
		if (k === "then") return undefined;
		return (...args) => {
			const first = typeof args[0] === "string" ? args[0] : (args[0] && args[0].name) || "";
			log.push(String(k) + "(" + first + ")");
			if (k === "on") return () => {};
			return undefined;
		};
	},
});
export async function run() {
	try { await factory(pi); } catch (e) { return "REJECT " + ((e && e.stack) || String(e)); }
	return log.join(" ");
}
`

// piHost links one pi extension: its relative imports are TypeScript
// modules compiled with CompileTS, and each package it imports is a stub
// that exports the names the graph imports from it.
type piHost struct {
	mods  map[string]*moejs.Module
	stubs map[string]map[string]bool
	any   *moejs.Module
}

// relModule compiles the TypeScript module at path and records the names
// it imports from packages, after the erasure of the type-only ones.
func (h *piHost) relModule(path string) (*moejs.Module, error) {
	if m, ok := h.mods[path]; ok {
		return m, nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := moejs.CompileTS(path, string(src))
	if err != nil {
		return nil, err
	}
	h.mods[path] = m
	tree, err := syntax.ParseModule(path, string(src), syntax.Options{TypeScript: true})
	if err != nil {
		return nil, err
	}
	need := func(req int, name string) {
		spec := tree.Requests[req].Specifier
		if strings.HasPrefix(spec, ".") {
			return
		}
		if h.stubs[spec] == nil {
			h.stubs[spec] = make(map[string]bool)
		}
		if name != "" {
			h.stubs[spec][name] = true
		}
	}
	for _, ie := range tree.Imports {
		need(ie.Request, ie.Name)
	}
	for _, re := range tree.Reexports {
		need(re.Request, re.Import)
	}
	for _, st := range tree.Stars {
		need(st.Request, "")
	}
	return m, nil
}

// piResolveFile maps a relative specifier of the module at base to a file.
func piResolveFile(base, spec string) string {
	p := filepath.Join(filepath.Dir(base), spec)
	for _, c := range []string{p, strings.TrimSuffix(p, ".js") + ".ts", p + ".ts", filepath.Join(p, "index.ts")} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return p
}

func (h *piHost) resolve(ref moejs.Referrer, spec string) (*moejs.Module, error) {
	if spec == "moejs:any" {
		return h.any, nil
	}
	if strings.HasPrefix(spec, ".") {
		return h.relModule(piResolveFile(ref.(*moejs.Module).Name(), spec))
	}
	names := make([]string, 0, len(h.stubs[spec]))
	for n := range h.stubs[spec] {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("import { mk } from \"moejs:any\";\n")
	for _, n := range names {
		if n == "default" {
			b.WriteString("export default mk();\n")
		} else {
			fmt.Fprintf(&b, "export const %s = mk();\n", n)
		}
	}
	return moejs.Compile("stub:"+spec, b.String())
}

// walk compiles the relative graph of path first, so that every stub knows
// its names before the graph is linked.
func (h *piHost) walk(path string, seen map[string]bool) error {
	if seen[path] {
		return nil
	}
	seen[path] = true
	m, err := h.relModule(path)
	if err != nil {
		return err
	}
	for _, r := range m.Requests() {
		if strings.HasPrefix(r, ".") {
			if err := h.walk(piResolveFile(path, r), seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestPiExtensions compiles each example extension of pi's coding agent
// (packages/coding-agent/examples/extensions in the pi-mono checkout of the
// TypeScript corpus) with CompileTS, links it against stub packages and
// runs its factory with a recording pi object. The extensions are written
// for Node: the ones that need its globals (process) fail when the factory
// runs; none may fail to compile or link.
func TestPiExtensions(t *testing.T) {
	root := filepath.Join(tsCorpusDir(t), "pi-mono", "packages", "coding-agent", "examples", "extensions")
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("pi's extensions not found: %v", err)
	}
	var entries []string
	for _, e := range ents {
		p := filepath.Join(root, e.Name())
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(p, "index.ts")); err == nil {
				entries = append(entries, filepath.Join(p, "index.ts"))
			}
		} else if strings.HasSuffix(e.Name(), ".ts") {
			entries = append(entries, p)
		}
	}
	anyMod, err := moejs.Compile("moejs:any", piAnySrc)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, entry := range entries {
		rel, _ := filepath.Rel(root, entry)
		h := &piHost{mods: map[string]*moejs.Module{}, stubs: map[string]map[string]bool{}, any: anyMod}
		if err := h.walk(entry, map[string]bool{}); err != nil {
			t.Errorf("%s: compile: %v", rel, err)
			continue
		}
		drv, err := moejs.Compile("driver.js", strings.Replace(piDriverSrc, "ENTRY", entry, 1))
		if err != nil {
			t.Fatal(err)
		}
		linked, err := moejs.Link(drv, func(ref moejs.Referrer, spec string) (*moejs.Module, error) {
			if spec == entry {
				return h.mods[entry], nil
			}
			return h.resolve(ref, spec)
		})
		if err != nil {
			t.Errorf("%s: link: %v", rel, err)
			continue
		}
		rt := moejs.NewRuntime(moejs.Options{})
		if err := rt.Load(linked); err != nil {
			t.Logf("FAIL %s: load: %v", rel, err)
			continue
		}
		hook, _ := linked.Hook("run")
		res, err := rt.Call(hook)
		if err != nil {
			t.Logf("FAIL %s: factory: %v", rel, err)
			continue
		}
		st, v, _ := moejs.PromiseResult(res)
		if st != moejs.PromiseFulfilled {
			t.Logf("FAIL %s: factory: %v", rel, st)
			continue
		}
		if s := v.String(); strings.HasPrefix(s, "REJECT") {
			s, _, _ = strings.Cut(s, "\n")
			t.Logf("FAIL %s: %s", rel, s)
			continue
		}
		ran++
	}
	t.Logf("factory ran %d/%d", ran, len(entries))
	if ran < 71 {
		t.Errorf("factory ran %d/%d, want at least 71", ran, len(entries))
	}
}

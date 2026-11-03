package engine

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/MoeForge/moejs/bytecode"
)

// Module graphs (ECMA-262 §16.2.1.5 Cyclic Module Records).
//
// LinkModules loads and links a graph once per process: it asks the host
// for the module of every request, resolves every import and indirect
// export (ResolveExport) and lays out the namespaces the graph needs. The
// ModuleGraph it returns is immutable and shared by every realm that
// evaluates it. EvaluateGraph (module_eval.go) instantiates the modules a
// realm has not instantiated yet and evaluates the entry. A realm keeps one
// instance per module template, so a module shared by several graphs
// evaluates once per realm.

// ModuleGraph is a linked module graph: the entry (module 0) and every
// module it requests, directly or not, each once.
type ModuleGraph struct {
	records []moduleRecord
	// exports are the names of the entry's namespace sorted by
	// strings.Compare, and bindings what each one resolves to.
	exports  []string
	bindings []ModuleBinding
}

// ModuleBinding locates a binding of a linked graph: the Env slot Slot of
// the graph's module Module, or that module's namespace when Slot is -1.
type ModuleBinding struct {
	Module int32
	Slot   int32
}

// moduleRecord is a module of a graph.
type moduleRecord struct {
	// key is the template the host resolved, the module's identity: a
	// realm's module map is keyed by it.
	key *bytecode.Function
	// code is what is instantiated: key, or its EarlyExports variant.
	code *bytecode.Function
	// deps are the modules of Links.Requests.
	deps []int32
	// imports are the bindings of Links.Imports.
	imports []ModuleBinding
	// ns lays out the module's namespace when the graph can reach it.
	ns *nsLayout
	// host is the host value of a module whose code uses import() or
	// import.meta (LinkOptions.HostDefined).
	host any
}

// nsLayout is the layout of a module namespace (§10.4.6): the exported
// names that resolve, in the order of their UTF-16 code units, and the
// binding each one reads.
type nsLayout struct {
	names   []string
	keys    []PropertyKey
	index   map[PropertyKey]int
	targets []ModuleBinding
}

// LinkOptions are the host hooks of LinkModules.
type LinkOptions struct {
	// Resolve returns the module that entry request of referrer's
	// Links.Requests names (HostLoadImportedModule). It is called once per
	// module and request of a graph. The template is the module's identity:
	// the same template is one module, of this graph and of every other
	// graph a realm evaluates, so Resolve must answer the same referrer and
	// request with the same template every time.
	Resolve func(referrer *bytecode.Function, request int) (*bytecode.Function, error)
	// EarlyExports, when set, returns the variant of an import-free module
	// compiled with its exported functions assumed to run before its body
	// (syntax.Options.EarlyExports). LinkModules asks for it when the
	// evaluation order lets another module call them that early, which only
	// an import cycle that reaches the module after one of its members ran
	// can do; without it, such a call can read the module's uninitialized
	// lexical bindings without the ReferenceError.
	EarlyExports func(code *bytecode.Function) (*bytecode.Function, error)
	// HostDefined, when set, returns the host value of a module whose code
	// uses import() or import.meta, or holds a direct eval call
	// (bytecode.Function.ScriptOrModule): the referrer import() in its code
	// and in its eval code passes ImportHooks.Load, and the module
	// ImportHooks.Meta fills the import.meta of. It is called once per such
	// module of the graph; a realm records the value of the graph that
	// first instantiates the module.
	HostDefined func(code *bytecode.Function) any
}

// ErrNoResolver is the Err of the LinkError of a module that requests
// another when LinkOptions.Resolve is nil.
var ErrNoResolver = errors.New("no resolver: the host must resolve the module's requests")

// LinkError is an error of LinkModules at the request, import or export
// entry of Module that caused it (Line and Col are 1-based, the column in
// code points). Err is the error of resolving Specifier; when it is nil,
// Msg is the SyntaxError message of an import or indirect export that does
// not resolve. EvaluateGraph returns one too, with Err set, for a request
// of a module the realm instantiated that the graph resolves to another
// module.
type LinkError struct {
	Module    *bytecode.Function
	Line, Col int
	Specifier string
	Err       error
	Msg       string
}

func (e *LinkError) Error() string {
	name := ""
	if e.Module.Source != nil {
		name = e.Module.Source.Name
	}
	if e.Err != nil {
		return fmt.Sprintf("%s:%d:%d: cannot resolve module %q: %v", name, e.Line, e.Col, e.Specifier, e.Err)
	}
	return fmt.Sprintf("%s:%d:%d: SyntaxError: %s", name, e.Line, e.Col, e.Msg)
}

func (e *LinkError) Unwrap() error { return e.Err }

// Records returns the templates of the graph's modules, the entry first,
// as the host resolved them.
func (g *ModuleGraph) Records() []*bytecode.Function {
	out := make([]*bytecode.Function, len(g.records))
	for i := range g.records {
		out[i] = g.records[i].key
	}
	return out
}

// Exports returns the names of the entry's namespace, sorted by
// strings.Compare: its exports, re-exported ones included, without the
// names its star exports leave ambiguous.
func (g *ModuleGraph) Exports() []string { return slices.Clone(g.exports) }

// Binding returns what the entry's export name resolves to.
func (g *ModuleGraph) Binding(name string) (ModuleBinding, bool) {
	i, ok := slices.BinarySearch(g.exports, name)
	if !ok {
		return ModuleBinding{}, false
	}
	return g.bindings[i], true
}

// linker is the state of LinkModules.
type linker struct {
	opts  LinkOptions
	recs  []moduleRecord
	byKey map[*bytecode.Function]int32
	// resolved holds the outcomes of resolveExport that no circular
	// request decided, which are those of any resolve set it is asked
	// with: a chain of indirect exports resolves once, not once per module
	// that re-exports along it.
	resolved map[resolveKey]resolution
}

// LinkModules loads the graph of entry, asking opts.Resolve for the module
// of each request, and links it (InnerModuleLinking, less the per-realm
// InitializeEnvironment of EvaluateGraph): every import and indirect export
// must resolve to one binding. The graph is immutable; any number of realms
// may evaluate it, concurrently.
func LinkModules(entry *bytecode.Function, opts LinkOptions) (*ModuleGraph, error) {
	if entry == nil || entry.Kind != bytecode.KindModule || entry.Module == nil {
		return nil, errors.New("engine: LinkModules requires a module template")
	}
	l := &linker{opts: opts, byKey: make(map[*bytecode.Function]int32)}
	l.add(entry)
	if err := l.load(0); err != nil {
		return nil, err
	}
	if opts.EarlyExports != nil && len(l.recs) > 1 {
		if err := l.earlyExports(); err != nil {
			return nil, err
		}
	}
	var order []int32
	l.postOrder(0, make([]bool, len(l.recs)), &order)
	for _, i := range order {
		if err := l.link(i); err != nil {
			return nil, err
		}
	}
	g := &ModuleGraph{records: l.recs}
	entryNS := l.layout(0)
	g.exports = slices.Clone(entryNS.names)
	slices.Sort(g.exports)
	g.bindings = make([]ModuleBinding, len(g.exports))
	for i, name := range g.exports {
		g.bindings[i] = entryNS.targets[entryNS.index[InternKey(name)]]
	}
	return g, nil
}

func (l *linker) add(code *bytecode.Function) int32 {
	i := int32(len(l.recs))
	l.byKey[code] = i
	l.recs = append(l.recs, moduleRecord{key: code, code: code})
	if code.ScriptOrModule && l.opts.HostDefined != nil {
		l.recs[i].host = l.opts.HostDefined(code)
	}
	return i
}

// load resolves the requests of module i and loads the modules they name,
// depth first, in the order of the requests.
func (l *linker) load(i int32) error {
	code := l.recs[i].key
	links := code.Module.Links
	if links == nil {
		return nil
	}
	deps := make([]int32, len(links.Requests))
	for j, req := range links.Requests {
		fail := func(err error) error {
			return &LinkError{Module: code, Line: int(req.Line), Col: int(req.Col), Specifier: req.Specifier, Err: err}
		}
		if l.opts.Resolve == nil {
			return fail(ErrNoResolver)
		}
		dep, err := l.opts.Resolve(code, j)
		if err != nil {
			return fail(err)
		}
		if dep == nil || dep.Kind != bytecode.KindModule || dep.Module == nil {
			return fail(errors.New("the resolver returned no module template"))
		}
		k, ok := l.byKey[dep]
		if !ok {
			k = l.add(dep)
			if err := l.load(k); err != nil {
				return err
			}
		}
		deps[j] = k
	}
	l.recs[i].deps = deps
	return nil
}

// postOrder lists the modules in the order InnerModuleLinking initializes
// their environments.
func (l *linker) postOrder(i int32, seen []bool, out *[]int32) {
	seen[i] = true
	for _, d := range l.recs[i].deps {
		if !seen[d] {
			l.postOrder(d, seen, out)
		}
	}
	*out = append(*out, i)
}

// link resolves the indirect exports and the imports of module i, as
// InitializeEnvironment does before it creates the bindings.
func (l *linker) link(i int32) error {
	rec := &l.recs[i]
	links := rec.key.Module.Links
	if links == nil {
		return nil
	}
	fail := func(line, col int32, req int, format, name string) error {
		spec := links.Requests[req].Specifier
		return &LinkError{Module: rec.key, Line: int(line), Col: int(col), Msg: fmt.Sprintf(format, spec, name)}
	}
	for _, e := range links.Reexports {
		if e.All {
			continue
		}
		switch _, st := l.resolveExport(i, e.Name); st {
		case resolvedNone:
			return fail(e.Line, e.Col, e.Request, errNoExport, e.Import)
		case resolvedAmbiguous:
			return fail(e.Line, e.Col, e.Request, errAmbiguousExport, e.Import)
		}
	}
	rec.imports = make([]ModuleBinding, len(links.Imports))
	for j, im := range links.Imports {
		dep := rec.deps[im.Request]
		if im.Namespace {
			rec.imports[j] = ModuleBinding{Module: dep, Slot: -1}
			l.layout(dep)
			continue
		}
		b, st := l.resolveExport(dep, im.Name)
		switch st {
		case resolvedNone:
			return fail(im.Line, im.Col, im.Request, errNoExport, im.Name)
		case resolvedAmbiguous:
			return fail(im.Line, im.Col, im.Request, errAmbiguousExport, im.Name)
		}
		if b.Slot < 0 {
			l.layout(b.Module)
		}
		rec.imports[j] = b
	}
	return nil
}

const (
	errNoExport        = "The requested module '%s' does not provide an export named '%s'"
	errAmbiguousExport = "The requested module '%s' contains conflicting star exports for name '%s'"
)

// The outcomes of resolveExport.
const (
	resolvedBinding = iota
	resolvedNone
	resolvedAmbiguous
)

type resolveKey struct {
	module int32
	name   string
}

type resolution struct {
	b  ModuleBinding
	st int
}

// resolveExport implements ResolveExport (§16.2.1.7.2.2) of module i's
// export name.
func (l *linker) resolveExport(i int32, name string) (ModuleBinding, int) {
	r, _ := l.resolve(i, name, nil)
	return r.b, r.st
}

// resolve is resolveExport with the resolve set. circular reports that a
// circular import request decided the outcome, which then holds for this
// set only; any other outcome is memoized.
func (l *linker) resolve(i int32, name string, set []resolveKey) (r resolution, circular bool) {
	k := resolveKey{i, name}
	if r, ok := l.resolved[k]; ok {
		return r, false
	}
	if slices.Contains(set, k) {
		return resolution{st: resolvedNone}, true
	}
	if r, circular = l.resolveStep(i, name, append(set, k)); !circular {
		if l.resolved == nil {
			l.resolved = make(map[resolveKey]resolution)
		}
		l.resolved[k] = r
	}
	return r, circular
}

// resolveStep is resolve of module i's export name once set holds it.
func (l *linker) resolveStep(i int32, name string, set []resolveKey) (resolution, bool) {
	rec := &l.recs[i]
	m := rec.key.Module
	if slot, ok := m.Export(name); ok {
		return resolution{b: ModuleBinding{Module: i, Slot: int32(slot)}, st: resolvedBinding}, false
	}
	links := m.Links
	if links == nil {
		return resolution{st: resolvedNone}, false
	}
	for _, e := range links.Reexports {
		if e.Name != name {
			continue
		}
		dep := rec.deps[e.Request]
		if e.All {
			return resolution{b: ModuleBinding{Module: dep, Slot: -1}, st: resolvedBinding}, false
		}
		return l.resolve(dep, e.Import, set)
	}
	if name == "default" {
		return resolution{st: resolvedNone}, false
	}
	var star ModuleBinding
	found, circular := false, false
	for _, s := range links.Stars {
		r, c := l.resolve(rec.deps[s.Request], name, set)
		circular = circular || c
		switch {
		case r.st == resolvedAmbiguous:
			return r, circular
		case r.st == resolvedNone:
		case !found:
			star, found = r.b, true
		case r.b != star:
			return resolution{st: resolvedAmbiguous}, circular
		}
	}
	if !found {
		return resolution{st: resolvedNone}, circular
	}
	return resolution{b: star, st: resolvedBinding}, circular
}

// exportedNames implements GetExportedNames (§16.2.1.7.2.1) of module i.
func (l *linker) exportedNames(i int32, starSet []bool) []string {
	if starSet[i] {
		return nil
	}
	starSet[i] = true
	rec := &l.recs[i]
	m := rec.key.Module
	names := make([]string, 0, len(m.Exports))
	for _, e := range m.Exports {
		names = append(names, e.Name)
	}
	links := m.Links
	if links == nil {
		return names
	}
	for _, e := range links.Reexports {
		names = append(names, e.Name)
	}
	if len(links.Stars) == 0 {
		return names
	}
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		seen[n] = struct{}{}
	}
	for _, s := range links.Stars {
		for _, n := range l.exportedNames(rec.deps[s.Request], starSet) {
			if _, dup := seen[n]; n != "default" && !dup {
				seen[n] = struct{}{}
				names = append(names, n)
			}
		}
	}
	return names
}

// layout returns the namespace layout of module i (GetModuleNamespace less
// the object), computing it, and those of the namespaces it exports, on
// first use.
func (l *linker) layout(i int32) *nsLayout {
	if ns := l.recs[i].ns; ns != nil {
		return ns
	}
	ns := &nsLayout{}
	l.recs[i].ns = ns
	type entry struct {
		name string
		b    ModuleBinding
	}
	var entries []entry
	for _, name := range l.exportedNames(i, make([]bool, len(l.recs))) {
		if b, st := l.resolveExport(i, name); st == resolvedBinding {
			entries = append(entries, entry{name, b})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return compareUTF16(a.name, b.name) })
	ns.names = make([]string, len(entries))
	ns.keys = make([]PropertyKey, len(entries))
	ns.targets = make([]ModuleBinding, len(entries))
	ns.index = make(map[PropertyKey]int, len(entries))
	for j, e := range entries {
		ns.names[j], ns.targets[j] = e.name, e.b
		ns.keys[j] = InternKey(e.name)
		ns.index[ns.keys[j]] = j
	}
	for _, b := range ns.targets {
		if b.Slot < 0 {
			l.layout(b.Module)
		}
	}
	return ns
}

// compareUTF16 orders valid UTF-8 strings by their UTF-16 code units, the
// order of the keys of a module namespace: a supplementary character sorts
// by its lead surrogate, after U+D7FF and before U+E000.
func compareUTF16(a, b string) int {
	for a != "" && b != "" {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			return cmp.Compare(utf16Order(ra), utf16Order(rb))
		}
		a, b = a[na:], b[nb:]
	}
	return cmp.Compare(len(a), len(b))
}

// utf16Order maps a rune to a key that sorts runes by their UTF-16 code
// units: a BMP rune is its unit, a supplementary one its lead surrogate,
// with the rune itself deciding between two with the same lead.
func utf16Order(r rune) uint64 {
	if r < 0x10000 {
		return uint64(r) << 32
	}
	return uint64(0xD800+(r-0x10000)>>10)<<32 | uint64(r)
}

// earlyExports replaces the template of every import-free module that an
// import cycle can call into before its body runs by its EarlyExports
// variant. It simulates InnerModuleEvaluation: once the body of a module
// has run, the modules of the evaluation stack it can reach are marked, and
// an import-free module first visited while a marked module's requests are
// still being visited is at risk. Asynchronous evaluation only delays
// bodies, so the simulation is conservative for top-level await too.
func (l *linker) earlyExports() error {
	n := len(l.recs)
	s := &riskSim{l: l, index: make([]int32, n), ancestor: make([]int32, n), onStack: make([]bool, n), visiting: make([]bool, n), marked: make([]bool, n), atRisk: make([]bool, n)}
	s.visit(0)
	for i, risk := range s.atRisk {
		if !risk {
			continue
		}
		rec := &l.recs[i]
		code, err := l.opts.EarlyExports(rec.key)
		if err != nil {
			return err
		}
		if code == nil || code.Kind != bytecode.KindModule || code.Module == nil || requestsModules(code) || code.ScriptOrModule != rec.key.ScriptOrModule ||
			len(code.CaptureLayout) != len(rec.key.CaptureLayout) || !slices.Equal(code.Module.Exports, rec.key.Module.Exports) {
			return errors.New("engine: the EarlyExports variant of a module does not match it")
		}
		rec.code = code
	}
	return nil
}

// requestsModules reports whether the module template code requests
// other modules. One that does not may still have Links, for import() or
// import.meta.
func requestsModules(code *bytecode.Function) bool {
	return code.Module.Links != nil && len(code.Module.Links.Requests) != 0
}

// riskSim is the state of earlyExports' simulation.
type riskSim struct {
	l                                 *linker
	index, ancestor                   []int32 // DFS index + 1; 0 is unvisited
	next                              int32
	stack                             []int32
	onStack, visiting, marked, atRisk []bool
	active                            int // marked modules whose requests are being visited
}

func (s *riskSim) visit(i int32) {
	s.next++
	s.index[i], s.ancestor[i] = s.next, s.next
	s.stack = append(s.stack, i)
	s.onStack[i] = true
	rec := &s.l.recs[i]
	if !requestsModules(rec.key) && s.active > 0 {
		s.atRisk[i] = true
	}
	s.visiting[i] = true
	for _, d := range rec.deps {
		switch {
		case s.index[d] == 0:
			s.visit(d)
			s.ancestor[i] = min(s.ancestor[i], s.ancestor[d])
		case s.onStack[d]:
			s.ancestor[i] = min(s.ancestor[i], s.index[d])
		}
	}
	s.visiting[i] = false
	if s.marked[i] {
		s.active--
	}
	// The body of i runs: it can call into the modules it requests that are
	// still on the stack, and through them into theirs.
	for _, d := range rec.deps {
		s.mark(d)
	}
	if s.ancestor[i] == s.index[i] {
		for {
			top := s.stack[len(s.stack)-1]
			s.stack = s.stack[:len(s.stack)-1]
			s.onStack[top] = false
			if top == i {
				break
			}
		}
	}
}

func (s *riskSim) mark(i int32) {
	if !s.onStack[i] || s.marked[i] {
		return
	}
	s.marked[i] = true
	if s.visiting[i] {
		s.active++
	}
	for _, d := range s.l.recs[i].deps {
		s.mark(d)
	}
}

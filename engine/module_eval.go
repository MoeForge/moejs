package engine

import (
	"errors"
	"fmt"
	"slices"

	"github.com/MoeForge/moejs/bytecode"
)

// Per-realm module instances and their evaluation (ECMA-262 §16.2.1.5.3).
//
// A realm's module map (realmLazy.modules, created by the first graph the
// realm evaluates) holds one instance per module template: its function
// object, its environment and the state of the spec's Cyclic Module Record
// evaluation. Instantiating a module creates its environment, binds its
// imports to the exporters' slots and runs the prologue of its template,
// which creates the hoisted functions and writes the hole into the lexical
// bindings, so that a cycle can call a function of a module whose body has
// not run. Evaluating runs the bodies from Module.BodyPC in the order of
// InnerModuleEvaluation, asynchronously from the job queue for the modules
// with top-level await and those that wait on one.

// moduleMap is a realm's module map.
type moduleMap struct {
	instances map[*bytecode.Function]*moduleInstance
	// asyncOrder counts the modules that evaluated asynchronously
	// (IncrementModuleAsyncEvaluationCount); one realm's modules never wait
	// on another's.
	asyncOrder int
	// refs are the records of the scripts and modules whose code uses
	// import() or import.meta, by root (import.go).
	refs map[*funcMeta]*scriptOrModule
}

// moduleStatus is a module's [[Status]] from linked on.
type moduleStatus uint8

const (
	moduleLinked moduleStatus = iota
	moduleEvaluating
	moduleEvaluatingAsync
	moduleEvaluated
)

// asyncDone is the [[AsyncEvaluationOrder]] of a module whose asynchronous
// evaluation ended; 0 is unset.
const asyncDone = -1

// moduleInstance is a module of a realm.
type moduleInstance struct {
	code   *bytecode.Function
	fn     *Object
	env    *Env
	deps   []*moduleInstance // parallel to Links.Requests
	ns     Value             // the namespace object once created, else undefined
	status moduleStatus
	err    error // [[EvaluationError]]

	dfsIndex, dfsAncestor int
	cycleRoot             *moduleInstance
	asyncOrder            int // [[AsyncEvaluationOrder]]: 0 unset, asyncDone, or the order
	asyncParents          []*moduleInstance
	pending               int     // [[PendingAsyncDependencies]]
	capability            *Object // [[TopLevelCapability]]'s promise
	// body is the promise of the body of a module with top-level await once
	// it ran.
	body *Object
	// ran is set once the body ran to its end or its first await.
	ran bool
}

// modules returns the realm's module map, creating it on first use.
func (r *Realm) modules() *moduleMap {
	lz := r.lazyState()
	if lz.modules == nil {
		lz.modules = &moduleMap{instances: make(map[*bytecode.Function]*moduleInstance)}
	}
	return lz.modules
}

// EvaluateGraph evaluates the linked graph g in r: it instantiates the
// modules r has not instantiated yet, as ordered by g, and evaluates the
// entry (Evaluate), which runs the bodies of the modules it depends on
// that have not run. It returns the entry's environment, whose slots back
// its local exports (GraphBinding reads the others), and the promise of
// the evaluation when it is asynchronous (top-level await in the graph),
// which the job queue's drain before an outermost EvaluateGraph returns
// may have settled. The promise is already rejected when the entry's own
// top level threw before it first awaited. The error is that of a
// synchronous evaluation, which r caches for every module it failed: a
// later evaluation of one of them returns it again. An interrupt is
// cached that way too in each module whose evaluation it stops after an
// await, by stopping the module's top level resumed from a job or dropping
// the job that would resume it, or run it once the modules it waits on
// evaluated, and in each module waiting on one; the promise of such an
// evaluation stays pending. The environment is nil when an interrupt
// pending on entry stopped the evaluation before any module was
// instantiated.
//
// The first graph that instantiates a module in r decides the modules it
// imports; the host's resolution must not change between the graphs a
// realm evaluates. A later graph that resolves a request of a module r
// instantiated to another module fails, before it instantiates any, with
// a *LinkError at the request, which an import() of the graph rejects
// with as an Error. A later graph that needs the EarlyExports variant of a
// module (see LinkOptions.EarlyExports) that r instantiated plain and has
// not run yet instantiates the variant in its place; a module whose top
// level ran, or started, keeps its template. A function of the plain
// template that the host read through GraphBinding before is not the
// module's any more then and must not be called.
func (r *Realm) EvaluateGraph(g *ModuleGraph) (*ModuleEnv, *Object, error) {
	if err := r.CheckInterrupt(); err != nil {
		return nil, nil, err
	}
	r.HoldJobs()
	entry, err := r.instantiateGraph(g)
	if err != nil {
		return nil, nil, r.ReleaseJobs(err)
	}
	menv := &ModuleEnv{Env: entry.env, module: entry.code.Module}
	p, err := r.evaluateModule(entry)
	if p != nil && entry.body != nil {
		if state, _, _ := entry.body.PromiseResult(); state == PromiseRejected {
			p = entry.body
		}
	}
	return menv, p, r.ReleaseJobs(err)
}

// GraphBinding reads the binding b of g in r, which evaluated g: the value
// of an export, re-exported ones included (see ModuleGraph.Binding), or a
// module's namespace, which it creates on first use. It is false when r has
// not instantiated the module or the binding is not initialized.
func (r *Realm) GraphBinding(g *ModuleGraph, b ModuleBinding) (Value, bool) {
	lz := r.lazy
	if lz == nil || lz.modules == nil {
		return Undefined(), false
	}
	m := lz.modules.instances[g.records[b.Module].key]
	if m == nil {
		return Undefined(), false
	}
	if b.Slot < 0 {
		return r.namespaceOf(g, b.Module), true
	}
	v := m.env.slots[b.Slot]
	if v.IsHole() {
		return Undefined(), false
	}
	return v, true
}

// GraphRan reports whether the top level of g's entry, which r evaluated,
// ran to its end or its first await. A failure of the evaluation reported
// after that came from the entry's top level after its first await; one
// reported before came from it before, or from a module it imports, and
// left it unevaluated.
func (r *Realm) GraphRan(g *ModuleGraph) bool {
	lz := r.lazy
	if lz == nil || lz.modules == nil {
		return false
	}
	m := lz.modules.instances[g.records[0].key]
	return m != nil && m.ran
}

// instantiateGraph instantiates the modules of g that r has not, which
// InitializeEnvironment does for each module in InnerModuleLinking, and
// returns the entry's instance.
func (r *Realm) instantiateGraph(g *ModuleGraph) (*moduleInstance, error) {
	mm := r.modules()
	insts := make([]*moduleInstance, len(g.records))
	// The trees of the templates the graph instantiates (metaFor), all
	// verified before anything changes; a small graph keeps them on the
	// stack.
	var buf [8]*funcMeta
	metas := buf[:]
	if len(g.records) > len(buf) {
		metas = make([]*funcMeta, len(g.records))
	}
	for i := range g.records {
		rec := &g.records[i]
		m := mm.instances[rec.key]
		if m == nil && rec.code.Module.BodyPC >= 1<<genDepthShift {
			return nil, errors.New("engine: module prologue too large")
		}
		if m == nil || m.code != rec.code {
			var err error
			if metas[i], err = r.metaFor(rec.code); err != nil {
				return nil, err
			}
		}
		insts[i] = m
	}
	for i, m := range insts {
		if m == nil {
			continue
		}
		for j, d := range g.records[i].deps {
			if j < len(m.deps) && insts[d] != m.deps[j] {
				return nil, inconsistentResolution(g, i, j, m.deps[j])
			}
		}
	}
	var fresh []int
	for i := range g.records {
		rec := &g.records[i]
		m := insts[i]
		switch {
		case m == nil:
			fn, env := r.instantiateTopLevel(rec.code, metas[i])
			m = &moduleInstance{code: rec.code, fn: fn, env: env, ns: Undefined()}
			mm.instances[rec.key] = m
			fresh = append(fresh, i)
			if rec.code.ScriptOrModule {
				mm.setScriptOrModule(fn.internal.(*FunctionData).meta, rec.host)
			}
		case m.code != rec.code && m.code == rec.key && m.status == moduleLinked:
			// An earlier graph instantiated the template, which this graph
			// can call into before the body runs, and never ran it: the
			// variant replaces it over the same environment, which the
			// imports and namespaces that reach the module point into, as
			// if it had been instantiated first. No code read its
			// functions: the earlier graph could not call them early.
			if rec.key.ScriptOrModule {
				delete(mm.refs, m.fn.internal.(*FunctionData).meta)
				mm.setScriptOrModule(metas[i], rec.host)
			}
			m.code = rec.code
			m.fn, _ = r.newClosure(rec.code, metas[i], m.env, Undefined())
			u := Undefined()
			for j := range m.env.slots {
				m.env.slots[j] = u
			}
			fresh = append(fresh, i)
		}
		insts[i] = m
	}
	for _, i := range fresh {
		rec, m := &g.records[i], insts[i]
		if err := r.runModulePrologue(m); err != nil {
			return nil, err
		}
		links := rec.key.Module.Links
		if links == nil {
			continue
		}
		m.deps = make([]*moduleInstance, len(rec.deps))
		for j, d := range rec.deps {
			m.deps[j] = insts[d]
		}
		for j, im := range links.Imports {
			b := rec.imports[j]
			t := insts[b.Module]
			switch {
			case im.Namespace:
				m.env.slots[im.Slot] = r.namespaceOf(g, b.Module)
			case b.Slot < 0:
				r.namespaceOf(g, b.Module)
				m.env.slots[im.Slot] = importRef(&t.ns)
			default:
				m.env.slots[im.Slot] = importRef(&t.env.slots[b.Slot])
			}
		}
	}
	return insts[0], nil
}

// inconsistentResolution is the error of a graph g whose module i, which
// the realm instantiated before, resolves its request j to another module
// than dep, the one it was instantiated with.
func inconsistentResolution(g *ModuleGraph, i, j int, dep *moduleInstance) error {
	rec := &g.records[i]
	req := rec.key.Module.Links.Requests[j]
	return &LinkError{Module: rec.key, Line: int(req.Line), Col: int(req.Col), Specifier: req.Specifier,
		Err: fmt.Errorf("the graph resolves it to %s, but the realm instantiated %s with %s",
			sourceName(g.records[rec.deps[j]].key), sourceName(rec.key), sourceName(dep.code))}
}

// sourceName returns the name of code's source, "" when it has none.
func sourceName(code *bytecode.Function) string {
	if code.Source == nil {
		return ""
	}
	return code.Source.Name
}

// namespaceOf returns the namespace object of module i of g, which r
// instantiated, creating it and the namespaces it exports on first use
// (GetModuleNamespace).
func (r *Realm) namespaceOf(g *ModuleGraph, i int32) Value {
	instances := r.lazy.modules.instances
	m := instances[g.records[i].key]
	if m.ns.IsObject() {
		return m.ns
	}
	o, ns := newNamespace(g.records[i].ns)
	m.ns = ObjectValue(o)
	for j, b := range ns.layout.targets {
		t := instances[g.records[b.Module].key]
		if b.Slot < 0 {
			r.namespaceOf(g, b.Module)
			ns.cells[j] = &t.ns
			continue
		}
		ns.cells[j] = &t.env.slots[b.Slot]
	}
	return m.ns
}

// runModulePrologue runs the instructions of m's template before
// Module.BodyPC, which the compiler limits to writing the hole into
// bindings and creating the hoisted functions in m's environment, with
// registers reserved above the live frames.
func (r *Realm) runModulePrologue(m *moduleInstance) error {
	code := m.code
	fd := m.fn.internal.(*FunctionData)
	if code.Module.BodyPC >= 1<<genDepthShift {
		return errors.New("engine: module prologue too large")
	}
	insns := code.Code[:code.Module.BodyPC]
	n := int(code.NumRegs)
	regs := r.pushArgs(n)
	env := m.env
	var err error
	for pc := 0; pc < len(insns) && err == nil; {
		w := insns[pc]
		pc++
		a := int(uint8(w >> 8))
		switch bytecode.Op(w) {
		case bytecode.LoadHole:
			regs[a] = Hole()
		case bytecode.SetEnv:
			if uint8(w>>16) != 0 {
				err = errPrologue
				break
			}
			env.slots[uint8(w>>24)] = regs[a]
		case bytecode.SetEnvW:
			if uint8(w>>16) != 0 {
				err = errPrologue
				break
			}
			env.slots[insns[pc]] = regs[a]
			pc++
		case bytecode.Closure:
			o, _ := r.newClosure(code.Children[w>>16], fd.meta.children[w>>16], env, Undefined())
			regs[a] = ObjectValue(o)
		case bytecode.AsyncFunc:
			err = r.asyncFunc(regs[a], w)
		case bytecode.GenFunc:
			err = r.genFunc(regs[a], w)
		default:
			err = errPrologue
		}
	}
	clear(regs)
	r.popArgs(n)
	return err
}

var errPrologue = errors.New("engine: internal error: unexpected instruction in a module prologue")

// runModuleBody runs m's body from Module.BodyPC as a call of its function
// object: the body of a module with top-level await returns its promise.
func (r *Realm) runModuleBody(m *moduleInstance) (Value, error) {
	if r.callDepth >= MaxCallDepth {
		return Undefined(), r.RangeError("Maximum call stack size exceeded")
	}
	r.callDepth++
	res, err := r.enterModuleFrame(m.fn)
	r.callDepth--
	switch {
	case err != nil:
	case !m.code.Async:
		m.ran = true
	case isPromise(res):
		state, _, _ := res.AsObject().PromiseResult()
		m.ran = state != PromiseRejected
	default:
		res, err = Undefined(), r.noBodyPromise()
	}
	if r.callDepth == 0 && r.jobsPending {
		if j := r.lazy.jobs; !j.draining {
			j.result = res
		}
		err = r.endJob(err)
	}
	return res, err
}

// noBodyPromise is the internal TypeError of a module with top-level
// await whose body returned no promise: bytecode no compiler made (the
// compiler ends the body with AsyncReturn and AsyncThrow, and the call
// returns the promise they or its first Await make), which a Go caller
// would otherwise read as one.
func (r *Realm) noBodyPromise() error {
	return r.TypeError("internal: an async module body returned no promise")
}

// enterModuleFrame is enterFrame for a module body, which starts at
// Module.BodyPC in the environment the prologue filled.
func (r *Realm) enterModuleFrame(fn *Object) (Value, error) {
	if r.interruptFlag.Load() != 0 {
		if err := r.pendingInterrupt(); err != nil {
			return Undefined(), err
		}
	}
	fd := fn.internal.(*FunctionData)
	code := fd.code
	if fd.icBase == icUnbound {
		fd.icBase = r.icBaseFor(fd.meta, code.ICCount)
	}
	st := &r.interp
	base := st.sp
	top := base + int(code.NumRegs)
	if top > len(st.stack) {
		r.growStack(top)
	}
	fi := st.nframes
	if fi == len(st.frames) {
		nf := make([]frameInfo, max(initialFrames, 2*len(st.frames)))
		copy(nf, st.frames)
		st.frames = nf
	}
	st.frames[fi] = frameInfo{fn: fn, pc: uint32(code.Module.BodyPC), base: uint32(base)}
	st.nframes = fi + 1
	st.sp = top
	res, err := r.run(fi, fd, base, fd.env, Undefined(), fn)
	st.sp = base
	st.nframes = fi
	return res, err
}

// evaluateModule implements Evaluate of m (§16.2.1.5.3.1), creating the
// top-level capability only when the evaluation is asynchronous: it
// returns the capability's promise then, else the error of the
// synchronous evaluation.
func (r *Realm) evaluateModule(m *moduleInstance) (*Object, error) {
	switch m.status {
	case moduleEvaluating:
		return nil, errors.New("engine: a module graph is evaluated while one of its modules evaluates")
	case moduleEvaluatingAsync, moduleEvaluated:
		if m.cycleRoot != nil {
			m = m.cycleRoot
		}
	}
	if m.capability != nil {
		if _, ok := m.err.(*InterruptedError); ok {
			return nil, m.err // the capability stays pending
		}
		return m.capability, nil
	}
	var stack []*moduleInstance
	if _, err := r.innerModuleEvaluation(m, &stack, 0); err != nil {
		for _, s := range stack {
			s.status, s.err = moduleEvaluated, err
		}
		return nil, err
	}
	if m.status == moduleEvaluatingAsync {
		m.capability = r.newPromise()
		return m.capability, nil
	}
	return nil, nil
}

// innerModuleEvaluation implements InnerModuleEvaluation (§16.2.1.5.3.2).
func (r *Realm) innerModuleEvaluation(m *moduleInstance, stack *[]*moduleInstance, index int) (int, error) {
	switch m.status {
	case moduleEvaluatingAsync, moduleEvaluated:
		return index, m.err
	case moduleEvaluating:
		return index, nil
	}
	if err := r.CheckInterrupt(); err != nil {
		return index, err
	}
	m.status = moduleEvaluating
	m.dfsIndex, m.dfsAncestor = index, index
	m.pending = 0
	index++
	*stack = append(*stack, m)
	for _, req := range m.deps {
		var err error
		if index, err = r.innerModuleEvaluation(req, stack, index); err != nil {
			return index, err
		}
		if req.status == moduleEvaluating {
			m.dfsAncestor = min(m.dfsAncestor, req.dfsAncestor)
		} else {
			if req.cycleRoot != nil {
				req = req.cycleRoot
			}
			if req.err != nil {
				return index, req.err
			}
		}
		if req.asyncOrder > 0 {
			m.pending++
			req.asyncParents = append(req.asyncParents, m)
		}
	}
	if m.pending > 0 || m.code.Async {
		mm := r.lazy.modules
		mm.asyncOrder++
		m.asyncOrder = mm.asyncOrder
		if m.pending == 0 {
			if err := r.executeAsyncModule(m); err != nil {
				return index, err
			}
		}
	} else if _, err := r.runModuleBody(m); err != nil {
		return index, err
	}
	if m.dfsAncestor == m.dfsIndex {
		for {
			s := *stack
			top := s[len(s)-1]
			*stack = s[:len(s)-1]
			if top.asyncOrder == 0 {
				top.status = moduleEvaluated
			} else {
				top.status = moduleEvaluatingAsync
			}
			top.cycleRoot = m
			if top == m {
				break
			}
		}
	}
	return index, nil
}

// executeAsyncModule implements ExecuteAsyncModule (§16.2.1.5.3.4): the
// body's own promise stands for the capability the spec creates, and m
// reacts to it. Only an interrupt is returned; any other failure to run
// the body rejects.
func (r *Realm) executeAsyncModule(m *moduleInstance) error {
	res, err := r.runModuleBody(m)
	if err != nil {
		v, ok := r.thrownValue(err)
		if !ok {
			return err
		}
		p := r.newPromise()
		r.rejectPromise(p, v)
		res = ObjectValue(p)
	}
	m.body = res.AsObject()
	r.promiseReact(m.body, m)
	return nil
}

// promiseSettled is the reaction of an asynchronous module to the promise
// of its body.
func (m *moduleInstance) promiseSettled(r *Realm, v Value, rejected bool) error {
	if rejected {
		r.asyncModuleRejected(m, r.Throw(v))
		return nil
	}
	return r.asyncModuleFulfilled(m)
}

// asyncModuleFulfilled implements AsyncModuleExecutionFulfilled
// (§16.2.1.5.3.5). An interrupt while the modules waiting on m run stops
// them and is returned.
func (r *Realm) asyncModuleFulfilled(m *moduleInstance) error {
	if m.status == moduleEvaluated {
		return nil // an error was cached
	}
	m.asyncOrder, m.status = asyncDone, moduleEvaluated
	if m.capability != nil {
		r.resolvePromise(m.capability, Undefined())
	}
	var execList []*moduleInstance
	gatherAvailableAncestors(m, &execList)
	slices.SortFunc(execList, func(a, b *moduleInstance) int { return a.asyncOrder - b.asyncOrder })
	for i, x := range execList {
		err := r.CheckInterrupt()
		switch {
		case err != nil:
		case x.status == moduleEvaluated:
		case x.code.Async:
			err = r.executeAsyncModule(x)
		default:
			if _, err = r.runModuleBody(x); err == nil {
				x.asyncOrder, x.status = asyncDone, moduleEvaluated
				if x.capability != nil {
					r.resolvePromise(x.capability, Undefined())
				}
			} else if _, ok := r.thrownValue(err); ok {
				r.asyncModuleRejected(x, err)
				err = nil
			}
		}
		if err != nil {
			// The interrupt stops x and the modules after it.
			for _, y := range execList[i:] {
				r.asyncModuleStopped(y, err)
			}
			return err
		}
	}
	return nil
}

// gatherAvailableAncestors implements GatherAvailableAncestors
// (§16.2.1.5.3.6).
func gatherAvailableAncestors(m *moduleInstance, execList *[]*moduleInstance) {
	for _, p := range m.asyncParents {
		root := p.cycleRoot
		if root == nil {
			root = p
		}
		if slices.Contains(*execList, p) || root.err != nil {
			continue
		}
		if p.pending--; p.pending == 0 {
			*execList = append(*execList, p)
			if !p.code.Async {
				gatherAvailableAncestors(p, execList)
			}
		}
	}
}

// asyncModuleRejected implements AsyncModuleExecutionRejected
// (§16.2.1.5.3.7) for the thrown error err.
func (r *Realm) asyncModuleRejected(m *moduleInstance, err error) {
	if m.status == moduleEvaluated {
		return
	}
	m.err, m.status, m.asyncOrder = err, moduleEvaluated, asyncDone
	if m.capability != nil {
		v, _ := r.thrownValue(err)
		r.rejectPromise(m.capability, v)
	}
	for _, p := range m.asyncParents {
		r.asyncModuleRejected(p, err)
	}
}

// asyncModuleStopped fails m, whose asynchronous evaluation the interrupt
// err stopped, and the modules waiting on it, as asyncModuleRejected does
// for a throw, but settles no capability: the interrupt stops what waits
// on the evaluation, and a later Evaluate of one of them returns err.
func (r *Realm) asyncModuleStopped(m *moduleInstance, err error) {
	if m.status != moduleEvaluatingAsync {
		return
	}
	m.err, m.status, m.asyncOrder = err, moduleEvaluated, asyncDone
	for _, p := range m.asyncParents {
		r.asyncModuleStopped(p, err)
	}
}

// stopModules fails with the interrupt err the modules in asynchronous
// evaluation that the job of reaction stopped (when not nil) or the
// dropped jobs would have continued: the job that resumes a module's body
// from an await, or reports the body settled.
func (r *Realm) stopModules(reaction *promiseReaction, dropped []job, err error) {
	var bodies map[*Object]*moduleInstance
	stop := func(re *promiseReaction) {
		switch x := re.reactor.(type) {
		case *moduleInstance:
			r.asyncModuleStopped(x, err)
		case *asyncFrame:
			if bodies == nil {
				bodies = make(map[*Object]*moduleInstance)
				for _, m := range r.lazy.modules.instances {
					if m.status == moduleEvaluatingAsync && m.body != nil {
						bodies[m.body] = m
					}
				}
			}
			if m := bodies[x.promise]; m != nil {
				r.asyncModuleStopped(m, err)
			}
		}
	}
	if reaction != nil {
		stop(reaction)
	}
	for i := range dropped {
		if re := dropped[i].reaction; re != nil {
			stop(re)
		}
	}
}

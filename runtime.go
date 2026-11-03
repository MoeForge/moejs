package moejs

import (
	"errors"
	"runtime/debug"
	"slices"
	"strconv"
	"time"
	"unsafe"

	"github.com/MoeForge/moejs/engine"
)

// Options configures NewRuntime.
type Options struct {
	// MutableIntrinsics gives the runtime its own mutable copy of the
	// builtins (prototypes, constructors, Math, JSON). By default they are
	// built once per process, deeply frozen and shared by every runtime, and
	// writing to them throws a TypeError.
	MutableIntrinsics bool
	// TimeZone is the local time zone of Date; nil means time.Local.
	TimeZone *time.Location
	// Locale is the default locale of Intl and of the toLocaleString
	// methods, a BCP 47 language tag such as "zh-CN"; "" or a tag that is
	// not well-formed means "en-US" (engine.RealmOptions).
	Locale string
	// Importer resolves the modules of import() and fills import.meta; nil
	// means import() rejects with a TypeError.
	Importer *Importer
	// MaxDynamicSource is the length in UTF-8 bytes of the longest source
	// text eval, the Function constructors and Realm.EvalScript compile:
	// longer text throws a RangeError. Zero
	// means engine.DefaultMaxDynamicSource (1 MiB), a negative value no
	// limit (engine.RealmOptions). Compile and CompileScript have no limit.
	MaxDynamicSource int
	// DisableDynamicCode makes eval of a string, the Function constructors
	// and Realm.EvalScript throw an EvalError instead of compiling anything,
	// for hosts whose code needs none (engine.RealmOptions). Compile and
	// CompileScript are not affected.
	DisableDynamicCode bool
	// MemoryLimit is the number of bytes the runtime's JavaScript may
	// retain, as the last measurement of its heap found, plus allocate
	// since that measurement or the last reset (ReleaseCallData,
	// ResetAllocation, and the end of a Load or SetGlobal that succeeded),
	// before the runtime interrupts itself: the call returns an
	// *InterruptedError whose value is a *MemoryLimitError, which errors.Is
	// finds as ErrMemoryLimit. An allocation past the limit measures first,
	// so a program that keeps little runs on however much it allocates.
	// Zero means no limit. The guide's "Memory limit" tells what is
	// counted.
	MemoryLimit int64
}

// Runtime is one JavaScript global environment with at most one loaded
// module. It must be used from one goroutine at a time; only Interrupt and
// ClearInterrupt may be called concurrently.
//
// The objects its JavaScript creates are its own. Any object of another
// runtime can run that runtime's code when it is used, not only a function
// or generator but also through a method, an accessor, a proxy, a promise
// or a thenable, and that code reads the caches of the runtime running it:
// wrong results or an *InternalError. Only Go values (ToGo, FromGo) and
// JSON are safe to pass between runtimes. Call, CallFunction, SetGlobal,
// FromGo and the settlers of NewPromise return ErrForeign for a function or
// generator of another runtime (or a bound function or proxy of one), FromGo
// also for one inside the Go containers it converts (reading that member
// throws); they do not look inside other objects, and do not check a proxy
// of another runtime that is not callable, even inside a Go map: its traps
// run in the runtime that reads it.
type Runtime struct {
	realm *engine.Realm
	mod   *Module
	env   *engine.ModuleEnv
	// failed is what the module's top level threw or was interrupted with
	// before it ended or first awaited, which Call and Has return for its
	// hooks.
	failed error
	// argStack backs the argument slices Call hands to the engine, so the
	// caller's variadic slice does not escape and a call allocates nothing
	// for its arguments once the stack has grown.
	argStack []Value
}

// NewRuntime creates a runtime.
func NewRuntime(opts Options) *Runtime {
	rt := &Runtime{realm: engine.NewRealmWith(engine.RealmOptions{SharedIntrinsics: !opts.MutableIntrinsics, TimeZone: opts.TimeZone, Locale: opts.Locale, MaxDynamicSource: opts.MaxDynamicSource, DisableDynamicCode: opts.DisableDynamicCode, MemoryLimit: opts.MemoryLimit})}
	if opts.Importer != nil {
		rt.realm.SetImportHooks(opts.Importer.engineHooks())
	}
	return rt
}

// Realm returns the runtime's engine state, for host functions and tests
// that need the engine API directly.
func (rt *Runtime) Realm() *Realm { return rt.realm }

// SetGlobal assigns the global variable name. v is converted by FromGo, so a
// host installs a namespace of functions as a map[string]any with
// NativeFunc values. It writes the global object, whose property a script's
// global let, const or class declaration of the same name shadows
// (ECMA-262 9.1.1.4.1). Like Load, it is setup: called outside any call, it
// ends by starting a new budget of Options.MemoryLimit when it succeeds.
func (rt *Runtime) SetGlobal(name string, v any) (err error) {
	r := rt.realm
	defer rt.guard(&err, r.CallState(), len(rt.argStack))
	val, err := rt.FromGo(v)
	if err != nil {
		return err
	}
	r.HoldJobs()
	return rt.setupDone(r.ReleaseJobs(r.Global.SetProp(r, r.KeyFromGoString(name), val)))
}

// Function creates a host function with a name and a length, the values of
// its `name` and `length` properties; a negative length is 0, as no
// function's length is negative.
func (rt *Runtime) Function(name string, length int, fn NativeFunc) Value {
	return engine.ObjectValue(rt.realm.NewHostFunction(engine.FromGoString(name), max(length, 0), fn))
}

// Load evaluates m's top level in this runtime. A runtime loads one module
// once, and a Load made while the top level runs is refused too. A module
// that imports others loads once Link linked it: Load then evaluates the
// modules of its graph first, each once, in the order of their imports; a
// failure of one of them is a failure of m's top level, which then does not
// run. In a runtime with an Importer, import() of m, or of a module of its
// graph, finds the instance Load evaluated. When the top level throws, is
// interrupted or panics (an *InternalError) before it ends or first
// awaits, the error is returned
// and the module stays loaded: Export reads
// the bindings initialized before the failure other than functions and
// module namespaces (none when an interrupt stopped the top level before it
// started), and Call and Has return the error for the module's hooks: a
// function could find the other bindings uninitialized. The
// jobs the top level queued run after it, when Export and the module's
// hooks find its bindings; while the top level itself runs they find none.
// A module with top-level await evaluates asynchronously: its top level
// resumes from those jobs, with the bindings found, and Load returns once
// none is left, with what the evaluation rejected with (an interrupt of
// those jobs wins), or ErrModulePending when it still awaits; an exported
// function called while it awaits throws a ReferenceError for a binding it
// has not initialized yet.
//
// Load is setup: when it succeeds, or returns ErrModulePending, it ends by
// starting a new budget of Options.MemoryLimit (ResetAllocation), so the
// top level's allocation does not count against the first request. A
// failed Load leaves the count, and the error of a memory limit hit in
// Stats, as they are. A nil m, what a failed Compile returns, is an error.
func (rt *Runtime) Load(m *Module) (err error) {
	if m == nil {
		return errNilModule
	}
	if rt.mod != nil {
		return errors.New("moejs: runtime already loaded module " + rt.mod.Name())
	}
	if m.graph != nil {
		return rt.loadGraph(m, m.graph.g)
	}
	if l := m.code.Module.Links; l != nil {
		if len(l.Requests) == 0 {
			// It uses import() or import.meta: its record is the realm's.
			g, err := m.selfGraph()
			if err != nil {
				return err
			}
			return rt.loadGraph(m, g)
		}
		return errors.New("moejs: module " + strconv.Quote(m.Name()) + " imports " + strconv.Quote(l.Requests[0].Specifier) + ": link it with moejs.Link and a resolver")
	}
	r := rt.realm
	if r.ImportHooks() != nil {
		// import() of m in r finds the instance Load evaluates.
		g, err := m.selfGraph()
		if err != nil {
			return err
		}
		return rt.loadGraph(m, g)
	}
	defer rt.guardLoad(&err, r.CallState(), len(rt.argStack))
	rt.mod = m
	rt.rootModule()
	r.HoldJobs()
	var p *engine.Object
	rt.env, p, err = r.EvaluateModuleAsync(m.code)
	if p != nil && err == nil {
		if state, _, _ := p.PromiseResult(); state == engine.PromiseRejected {
			err, p = r.ModuleEvaluationError(p, nil), nil
		}
	}
	rt.failed = err
	if err = r.ReleaseJobs(err); p != nil {
		err = r.ModuleEvaluationError(p, err)
	}
	return rt.setupDone(err)
}

// setupDone ends a setup method, Load or SetGlobal, that returns err: when
// it succeeded outside any call (or left a module awaiting,
// ErrModulePending), it starts a new budget of Options.MemoryLimit, so
// that the host's setup does not count against the first request. A
// failure leaves the count, and a hit's error in Stats, as they are. From
// a host function it does nothing: a reset there would give the running
// script a new budget.
func (rt *Runtime) setupDone(err error) error {
	if (err == nil || err == ErrModulePending) && rt.realm.CallDepth() == 0 {
		rt.realm.ResetAllocation()
	}
	return err
}

// loadGraph is Load of a module through its graph g: Link's, or the graph
// of the module alone for one that uses import() or import.meta or loads
// in a runtime with an Importer. The
// runtime evaluates the modules of g it has not evaluated, each at most
// once. The entry's hooks return what the evaluation failed with when it
// failed before the entry's top level ended or first awaited, which covers
// the failures of the modules it imports: its top level did not run then.
func (rt *Runtime) loadGraph(m *Module, g *engine.ModuleGraph) (err error) {
	r := rt.realm
	defer rt.guardGraph(&err, g, r.CallState(), len(rt.argStack))
	rt.mod = m
	rt.rootModule()
	r.HoldJobs()
	var p *engine.Object
	rt.env, p, err = r.EvaluateGraph(g)
	if le, ok := err.(*engine.LinkError); ok {
		// A module of g the runtime instantiated resolves a request to
		// another module in g.
		err = linkError(le)
	}
	if p != nil && err == nil {
		if state, _, _ := p.PromiseResult(); state == engine.PromiseRejected {
			err, p = r.ModuleEvaluationError(p, nil), nil
		}
	}
	rt.failed = err
	if err = r.ReleaseJobs(err); p != nil {
		err = r.ModuleEvaluationError(p, err)
		_, interrupted := err.(*InterruptedError)
		if state, _, _ := p.PromiseResult(); (state == engine.PromiseRejected || interrupted) && !r.GraphRan(g) {
			rt.failed = err
		}
	}
	return rt.setupDone(err)
}

// Module returns the loaded module, or nil.
func (rt *Runtime) Module() *Module { return rt.mod }

// Export returns the current value of the loaded module's export name, a
// name it re-exports included. ok is false when there is no such export or
// the binding is not initialized yet, and, once the module's top level
// failed (Load), for a function or a module namespace object, whose code
// could find the module's other bindings uninitialized without the
// ReferenceError.
func (rt *Runtime) Export(name string) (v Value, ok bool) {
	if rt.env == nil {
		return engine.Undefined(), false
	}
	v, ok = rt.env.GetBindingValue(name)
	if !ok && rt.mod.graph != nil {
		v, ok = rt.reexport(name)
	}
	if !ok || v.IsHole() {
		return engine.Undefined(), false
	}
	if rt.failed != nil && v.IsObject() && (engine.IsCallable(v) || v.AsObject().IsModuleNamespace()) {
		return engine.Undefined(), false
	}
	return v, true
}

// reexport reads the export name of the loaded graph's entry that is not
// one of its own bindings.
func (rt *Runtime) reexport(name string) (Value, bool) {
	l := rt.mod.graph
	i, found := slices.BinarySearch(l.exports, name)
	if !found {
		return engine.Undefined(), false
	}
	return rt.realm.GraphBinding(l.g, l.bindings[i])
}

// Has reports whether h names a function in this runtime now. A getter on
// the path that throws is returned as the error, and so is Load's for the
// hooks of a module whose top level failed.
func (rt *Runtime) Has(h Hook) (ok bool, err error) {
	r := rt.realm
	defer rt.guard(&err, r.CallState(), len(rt.argStack))
	_, err = rt.lookup(h)
	switch err {
	case nil:
		return true, nil
	case ErrHookNotFound, ErrNotCallable:
		return false, nil
	}
	return false, err
}

// Call invokes the function h names with this = undefined. A path that
// does not lead to a value is ErrHookNotFound, one that leads to a value
// that is not a function ErrNotCallable; a throw is an *Exception, an
// interrupt an *InterruptedError, a Go panic below it an *InternalError.
// The hooks of a module whose top level failed return Load's error. An
// argument that is a function or generator of another runtime is
// ErrForeign. A Call made inside a host function gives its *InternalError
// to that host function, as CallFunction does: returned to JavaScript, it
// is an Error JavaScript can catch.
func (rt *Runtime) Call(h Hook, args ...Value) (res Value, err error) {
	r := rt.realm
	base := len(rt.argStack)
	defer rt.guard(&err, r.CallState(), base)
	fn, err := rt.lookup(h)
	if err != nil {
		return engine.Undefined(), err
	}
	// Bytecode checks for an interrupt when it is entered, natives do not.
	// The release drops any jobs left queued, as the return of an
	// interrupted call does.
	if err := r.CheckInterrupt(); err != nil {
		r.HoldJobs()
		return engine.Undefined(), r.ReleaseJobs(err)
	}
	for _, a := range args {
		if a.IsObject() && a.AsObject().HoldsCode() && r.IsForeign(a) {
			return engine.Undefined(), ErrForeign
		}
	}
	rt.argStack = append(rt.argStack, args...)
	top := len(rt.argStack)
	res, err = r.CallObject(fn, engine.Undefined(), rt.argStack[base:top:top])
	clear(rt.argStack[base:top])
	rt.argStack = rt.argStack[:base]
	return res, err
}

// ReleaseCallData drops the runtime's references to the host values of
// finished calls, so a pooled runtime does not keep the last request alive.
// A host that pools runtimes calls it after the request's last use of the
// runtime (Call, ToGo, Get, AppendJSON: each can run JavaScript), before it
// returns the runtime to its pool; Call does not, so that a request running
// several hooks converts its data as one, and hosts that do not pool pay
// nothing. Values already returned stay valid: their nodes keep their own
// storage, so ToGo and Get of them work after it. It also starts a new
// budget of Options.MemoryLimit (ResetAllocation). Between calls only; a
// no-op inside one (from a host function).
//
// What JavaScript keeps stays alive with what it references: a string of a
// FromGo argument the module stores keeps every string converted in its
// period (up to 512), and an object it stores keeps its chunk and, through
// the root placeholder in it, the whole argument; a substring of an ASCII
// string shares its bytes, so storing a slice of a large one keeps all of
// it. The names the runtime keeps for its caches (property names, RegExp
// patterns) are its own copies, whether they come from map keys, a parsed
// JSON text or a slice of a string.
func (rt *Runtime) ReleaseCallData() { rt.realm.ReleaseCallData() }

// lookup walks h in the loaded module: the export, then each member as an
// own property of the value before it (an own getter runs; for a proxy the
// getOwnPropertyDescriptor and get traps). undefined, null, a primitive
// before the last member or a missing member is ErrHookNotFound. The walk
// ends as an outermost call does: the jobs its getters and traps queued run
// before it returns, and the first exception they throw is its error, also
// when the path leads nowhere.
func (rt *Runtime) lookup(h Hook) (*Object, error) {
	if h.mod != rt.mod {
		return rt.lookupOwn(h)
	}
	if rt.failed != nil {
		return nil, rt.failed
	}
	if rt.env == nil {
		return nil, ErrHookNotFound
	}
	var v Value
	if h.slot >= 0 {
		v = rt.env.Slot(h.slot)
	} else {
		l := rt.mod.graph
		v, _ = rt.realm.GraphBinding(l.g, l.bindings[-1-h.slot])
	}
	for i, k := range h.keys {
		if !v.IsObject() {
			return nil, ErrHookNotFound
		}
		next, ok := v.AsObject().GetOwnDataValue(k)
		if !ok {
			var err error
			if v, err = rt.members(v.AsObject(), h.keys[i:]); err != nil {
				return nil, err
			}
			break
		}
		v = next
	}
	switch {
	case v.IsUndefined(), v.IsNull(), v.IsHole():
		return nil, ErrHookNotFound
	case !engine.IsCallable(v):
		return nil, ErrNotCallable
	}
	return v.AsObject(), nil
}

// lookupOwn is lookup for a hook of another module than the loaded one,
// which names nothing unless it is of an export the loaded module declares
// in a Module of the same code: the one Link linked, or another Link of it.
func (rt *Runtime) lookupOwn(h Hook) (*Object, error) {
	if h.slot < 0 || h.mod == nil || rt.mod == nil || h.mod.code != rt.mod.code {
		return nil, ErrHookNotFound
	}
	h.mod = rt.mod
	return rt.lookup(h)
}

// members walks keys from o for lookup, which reads own data properties
// directly (they run no code) and hands over at the first member that is not
// one of o: a getter, a proxy, whose traps run, or a missing member. It
// holds the jobs as an outermost call does. A path that leads nowhere is
// undefined.
func (rt *Runtime) members(o *Object, keys []engine.PropertyKey) (Value, error) {
	r := rt.realm
	r.HoldJobs()
	v := engine.ObjectValue(o)
	var err error
	for _, k := range keys {
		if !v.IsObject() {
			v = engine.Undefined()
			break
		}
		o := v.AsObject()
		var has bool
		if has, err = r.HasOwn(o, k); err != nil || !has {
			v = engine.Undefined()
			break
		}
		if v, err = o.GetProp(r, k); err != nil {
			break
		}
	}
	return v, r.ReleaseJobs(err)
}

// Interrupt stops running code: the pending Call (or Load, ToGo, ...)
// returns an *InterruptedError carrying v. It may be called from any
// goroutine. An interrupt that arrives while nothing runs stops the next
// Call, whatever the function it names, or Load, and any other method once
// it runs JavaScript, so a host calls ClearInterrupt before reusing the
// runtime.
func (rt *Runtime) Interrupt(v any) { rt.realm.Interrupt(v) }

// ClearInterrupt drops a pending interrupt.
func (rt *Runtime) ClearInterrupt() { rt.realm.ClearInterrupt() }

// ResetAllocation starts a new budget of Options.MemoryLimit: what the
// runtime counted since the last reset goes back to zero. The last
// measurement of RebaseAllocation stays, so that what a long-lived program
// keeps still counts. ReleaseCallData, Load and SetGlobal do it too; a
// host that does not pool runtimes calls this where a request starts. It
// leaves a pending interrupt pending.
func (rt *Runtime) ResetAllocation() { rt.realm.ResetAllocation() }

// RebaseAllocation bounds what a long-lived program keeps, where
// ResetAllocation bounds what one request allocates: it measures what the
// runtime's JavaScript retains and makes that the base of the budget of
// Options.MemoryLimit, in place of what was allocated since the last reset
// or rebase. From then on the limit counts the measurement plus what is
// allocated after it (Stats: LiveBytes and RequestAllocatedBytes). A host
// that runs its own event loop calls it between tasks, where it would
// call ResetAllocation, so that a program that keeps what it allocates
// stops at the limit, however many tasks it took to allocate it; package
// eventloop does so. A host that resets per request (ResetAllocation,
// SetGlobal, ReleaseCallData) can call it at each request's start too:
// the resets keep the measurement.
//
// The measurement walks what is reachable from the runtime's global
// object and global bindings, its module's bindings, the queued promise
// jobs, the roots registered with AddLiveRoots and the values roots adds
// (roots may be nil), and counts it as the limit counts allocation. A
// string that shares the units of a longer one of 64 units or more, a
// substring or a value JSON.parse or ParseJSONString read out of a longer
// text while the limit was set, counts the longer one, which it keeps
// alive. It cannot see
// values only Go code holds: what a NativeFunc's closure captured, the
// promise of NewPromise, which only the settle functions keep (and through
// it the async functions awaiting it), a value a pending task of the
// host's queue will pass to JavaScript. The host adds those through roots,
// which RebaseAllocation calls with add only when it measures, or
// registers them with AddLiveRoots.
//
// The measurement is paced: it is made only once the allocation since the
// last one, resets included, reached the larger of half of what the limit
// leaves above the last measurement, (MemoryLimit - LiveBytes) / 2, and
// half of the last measurement; otherwise RebaseAllocation returns at once,
// without allocating. When an allocation would pass the limit before the
// next measurement, the runtime measures first, with the module's bindings
// and the registered roots, and stops only when that measurement leaves no
// room for it: once between two calls of RebaseAllocation or
// ResetAllocation, and again each time the allocation since the last
// measurement reaches what paces RebaseAllocation, so that a call that
// makes much garbage runs on as it would across rebases. In the callbacks
// of forEach, map, filter, flatMap, reduce, sort, replace, Array.from, a
// JSON.parse reviver and a host function it runs as in plain code; an
// allocation another builtin makes waits for it to return, within half of
// what the limit leaves past it and the allocation that crosses that, and
// the runtime measures once it returns or at the next call, or between
// promise jobs; ParseJSON, ParseJSONString and FromGo wait so too. An
// allocation of the host's own Go code outside any call, and its
// ChargeMemory where no script runs (an event loop's task, or a host
// function called from Go), measure at once, as RebaseAllocation does:
// what the host holds between calls, it registers (AddLiveRoots). When the
// measurement of RebaseAllocation passes the limit, it
// returns the *MemoryLimitError (Live the measurement, Allocated 0) and
// counts a hit, but does not interrupt the runtime: the host stops the
// program, whose next allocation passes the limit anyway. An Interrupt
// made while it measures ends the measurement, which then changes
// nothing. Without a MemoryLimit it does nothing. It runs on the goroutine
// using the runtime, between calls.
func (rt *Runtime) RebaseAllocation(roots func(add func(Value))) error {
	if !rt.realm.StartRebase() {
		return nil
	}
	return rt.realm.RebaseAllocation(func(add func(Value)) {
		rt.liveRoots(add)
		if roots != nil {
			roots(add)
		}
	})
}

// AddLiveRoots registers roots, a function that adds to every measurement
// of the runtime's heap the JavaScript values only the host's Go code
// holds: the pending work of the host's event loop, the values a
// NativeFunc's closure keeps for a later call, the promises of NewPromise
// whose settle functions the host keeps. The measurements of
// RebaseAllocation and of an allocation that would pass the limit
// (RebaseAllocation) and Realm().MeasureLive call it, and nothing else:
// without a MemoryLimit it costs nothing but the registration. It returns
// the function that removes it. roots runs on the goroutine using the
// runtime, possibly in the middle of JavaScript (an allocation that would
// pass the limit): it must only call add, without running JavaScript or
// creating values, and must not wait for a lock that is held while the
// runtime runs JavaScript; a lock taken only to read the host's own state
// (a mutex that guards a queue) is fine. AddLiveRoots and remove run on
// the goroutine using the runtime. Package eventloop registers the loop's
// timers, immediates and the promises of Loop.NewPromise this way.
//
// A host function that calls back into JavaScript keeps no JavaScript value
// only in Go across the call, or registers it, since a measurement may run
// in the JavaScript it calls back.
//
// A registered root counts until it is removed, whether JavaScript still
// reaches what it adds or not. What the Go state of an object the host
// created keeps (a stream's queue, a body's bytes, header values kept as
// Go strings) reports through LiveReporter instead, so that it counts
// while the object is reachable and stops counting once JavaScript drops
// it; AddLiveRoots is for what Go alone holds, such as requests in flight
// and promises only the host will settle.
func (rt *Runtime) AddLiveRoots(roots func(add func(Value))) (remove func()) {
	return rt.realm.AddLiveRoots(roots)
}

// rootModule registers liveRoots with the realm when it has a memory
// limit as Load starts, so that every measurement of its heap reaches the
// module Load evaluates and what its top level threw: the one an
// allocation that would pass the limit makes in the middle of JavaScript
// and Realm().MeasureLive too, not only RebaseAllocation's, which adds
// them itself (for a limit set later through the Realm).
func (rt *Runtime) rootModule() {
	if rt.realm.MemoryLimit() > 0 {
		rt.realm.AddLiveRoots(rt.liveRoots)
	}
}

// liveRoots adds what the runtime holds for JavaScript outside the
// realm's roots: a module Load evaluated alone, which is not in the
// realm's module map, and what its top level threw.
func (rt *Runtime) liveRoots(add func(Value)) {
	if rt.env != nil && rt.env.Env != nil {
		for _, v := range rt.env.Slots() {
			add(v)
		}
	}
	if rt.failed != nil {
		var ex *Exception
		if errors.As(rt.failed, &ex) {
			add(ex.Value)
		}
	}
}

// Stats returns the runtime's counters, each read in constant time; the
// allocation counters count only while a MemoryLimit is set. It runs on
// the goroutine using the runtime, also from a host function.
func (rt *Runtime) Stats() Stats { return rt.realm.Stats() }

// FromGo converts a Go value: nil, bool, the integer and float kinds,
// string, json.Number, *big.Int (a bigint), Value, NativeFunc, and the
// JSON-shaped containers map[string]any, map[string]string,
// map[string][]string, []any, []string and []map[string]any. Containers
// convert lazily, one level when first touched, so a large argument the
// hook reads little of costs little; the Go value must not change while the
// result is in use, and JavaScript writes never reach it. Maps enumerate
// their keys sorted. A []byte becomes an ArrayBuffer over the same bytes,
// not a copy: JavaScript writes reach them, and the host must not modify
// them while JavaScript may read them.
//
// A json.RawMessage converts as JSON.parse of a copy of its text, an object
// or array text parsed when JavaScript first reads it; a named type as its
// underlying type; any other type encoding/json writes as JSON.parse of its
// json.Marshal text, a snapshot. An error without a MarshalJSON or
// MarshalText method, a struct none of whose fields json.Marshal writes, a
// channel, a func that is not a NativeFunc and a complex number are an
// error; inside a container, reading that member throws a TypeError. The
// guide's "Go values in" and "JSON in" give the details. A Value or *Object
// that is a function or generator of another runtime is ErrForeign; inside
// a container, reading its member throws a TypeError with ErrForeign's text.
func (rt *Runtime) FromGo(v any) (Value, error) {
	return rt.realm.FromGo(v)
}

// ParseJSON is JSON.parse of b. Nesting deeper than 10,000 arrays and
// objects (engine.MaxToGoDepth) is a RangeError that wraps ErrTooDeep. It
// parses a copy of b, so the host may reuse b once it returns; for a large
// text, ParseJSONString saves the copy.
func (rt *Runtime) ParseJSON(b []byte) (Value, error) {
	if err := rt.realm.ChargeMemory(len(b)); err != nil {
		return engine.Undefined(), err
	}
	return rt.realm.JSONParseGoString(string(b))
}

// ParseJSONString is ParseJSON of s without the copy when s is valid UTF-8
// (other text is converted first, by both): the strings of the result, and
// the Go strings ToGo, Unmarshal, ToGoInto and Value.String give for them,
// may share s's memory, as ParseJSON's share its copy, and keep it alive.
//
// A host that holds the text as a []byte may pass
// unsafe.String(unsafe.SliceData(b), len(b)) if it never modifies b while
// anything may still read the text: until ReleaseCallData, and for as long
// as the host or the module keeps values derived from it. The host is then
// responsible for that; nothing checks it.
func (rt *Runtime) ParseJSONString(s string) (Value, error) {
	return rt.realm.JSONParseGoString(s)
}

// Get reads property key of v, running a getter and walking the prototype
// chain; undefined and null have no properties and read as undefined.
func (rt *Runtime) Get(v Value, key string) (res Value, err error) {
	if v.IsUndefined() || v.IsNull() {
		return engine.Undefined(), nil
	}
	r := rt.realm
	defer rt.guard(&err, r.CallState(), len(rt.argStack))
	r.HoldJobs()
	res, err = r.GetV(v, r.KeyFromGoString(key))
	return res, r.ReleaseJobs(err)
}

// ToGo exports v: undefined and null become nil, booleans bool, strings
// string, integral numbers in int64 range int64 (except -0), other numbers
// float64, arrays []any, other objects map[string]any of their own
// enumerable string-keyed properties, Date time.Time, bigint *big.Int, and
// functions and symbols the engine value itself (*Object, *engine.Symbol).
// An ArrayBuffer or SharedArrayBuffer exports a copy of its bytes as a
// []byte, and a typed array or DataView a copy of the bytes it views (so a
// Uint16Array of 2 elements gives 4 bytes, little-endian); a detached
// buffer and a view out of its buffer's bounds give a nil []byte, and a
// zero-length buffer or view a non-nil empty one.
// A proxy exports through its traps (see engine.Realm.ToGo): []any when its
// target is an array, the map of its enumerable keys otherwise, the *Object
// when it is callable. An array or object FromGo converted from a non-empty
// Go map or slice exports as that Go value while JavaScript has not
// modified it (engine.Object.HostValue); an empty one converted to a plain
// array or object, and a nil map to null. A getter or trap that throws, a
// revoked proxy, or nesting deeper than 10,000 containers
// (engine.MaxToGoDepth, a RangeError that wraps ErrTooDeep) is returned as
// the error. The Go string of an ASCII
// string is the one v holds: for a value ParseJSON or ParseJSONString
// produced it may share the parsed text and keep it alive; strings.Clone
// what is kept.
func (rt *Runtime) ToGo(v Value) (out any, err error) {
	r := rt.realm
	defer rt.guard(&err, r.CallState(), len(rt.argStack))
	r.HoldJobs()
	out, err = r.ToGoStrict(v)
	return out, r.ReleaseJobs(err)
}

// AppendJSON appends JSON.stringify(v) to dst as UTF-8. A value with no
// JSON form (undefined, a function, a symbol) appends null. A proxy is
// serialized as JSON.stringify does it: its traps run. Nesting deeper than
// 10,000 arrays and objects (engine.MaxToGoDepth) is a RangeError that
// wraps ErrTooDeep, and an output of more than 2^30-24 bytes is a
// RangeError too: the string length limit counted in bytes of UTF-8,
// JSON.stringify's limit for ASCII text and reached first for other text.
//
// The output is written into dst's spare capacity, after reserving the last
// output's length plus an eighth: a host that passes its previous output
// back (buf, err = rt.AppendJSON(buf[:0], v)) allocates only when buf has
// less room than that, and after one very large output a call with a small
// dst allocates that much again. Before any JavaScript runs (a toJSON, a
// Date's included, a getter, a proxy's trap) the output moves to a buffer
// of its own, which holds twice what is written so far and grows as the
// rest is written, and is appended to dst when AppendJSON returns, so a
// host function that JavaScript calls meanwhile may append to dst or pass
// it to another AppendJSON; what it appended to dst's spare capacity is
// overwritten then, as append(dst, text...) would overwrite it. The same
// holds for a job (a promise reaction, queueMicrotask) that code queued,
// when AppendJSON is called outside any Call: the job runs before the
// output is appended to dst. Inside a host function the jobs run when the
// outermost Call returns, after AppendJSON has returned, and one that
// appends to the same dst overwrites what AppendJSON appended, as any later
// append would: a host function should use a buffer of its own.
func (rt *Runtime) AppendJSON(dst []byte, v Value) (out []byte, err error) {
	return rt.appendJSON(dst, v, false)
}

// JSONOptions are the options of AppendJSONWith.
type JSONOptions = engine.JSONOptions

// LossyJSONError is the error of AppendJSONWith with JSONOptions.Strict for
// a value JSON has no text for: its Path names where it is, as in
// $.messages[3].content, and its Value what it is, as in "undefined".
type LossyJSONError = engine.LossyJSONError

// AppendJSONWith is AppendJSON with the options of opts. With Strict, a
// value AppendJSON writes otherwise or drops is an error, a
// *LossyJSONError, and dst comes back as it was: an undefined, function or
// symbol value (which an object leaves out and an array writes as null,
// and which as the value itself AppendJSON writes as null), and NaN,
// Infinity and -Infinity (written as null). What toJSON returns is
// checked, so a toJSON that returns undefined fails; -0 is written as 0, as
// without Strict. Strict costs AppendJSON nothing; with it a Go map or
// slice FromGo converted is written through the values JavaScript sees.
// Without Strict it is AppendJSON.
func (rt *Runtime) AppendJSONWith(dst []byte, v Value, opts JSONOptions) (out []byte, err error) {
	return rt.appendJSON(dst, v, opts.Strict)
}

func (rt *Runtime) appendJSON(dst []byte, v Value, strict bool) (out []byte, err error) {
	r := rt.realm
	defer rt.guard(&err, r.CallState(), len(rt.argStack))
	r.HoldJobs()
	// What a panic returns (guard), the output being suspect: a host
	// function's inside the call, or a job's at the release.
	out = dst
	var res []byte
	var ok bool
	if strict {
		res, ok, err = r.AppendJSONWith(dst, v, engine.JSONOptions{Strict: true})
	} else {
		res, ok, err = r.AppendJSON(dst, v)
	}
	// The jobs a toJSON or getter queued run at the release, after the
	// output landed in dst's spare capacity; a host function they call may
	// use dst (above), so when any are pending the output first moves out.
	// A value that runs no code queues none and is not copied.
	if ok && err == nil && r.JobsPending() && len(dst) < len(res) && unsafe.SliceData(res) == unsafe.SliceData(dst) {
		text := append([]byte(nil), res[len(dst):]...)
		if err = r.ReleaseJobs(nil); err != nil {
			return dst, err
		}
		return append(dst, text...), nil
	}
	if err = r.ReleaseJobs(err); err != nil {
		return dst, err
	}
	if !ok {
		return append(dst, "null"...), nil
	}
	return res, nil
}

// StackTrace returns the `stack` of an Error thrown in this runtime:
// "Name: message" and one "    at ..." line per frame. It is "" when the
// thrown value is not an Error or its stack was replaced by a non-string.
// No user code runs.
func (rt *Runtime) StackTrace(exc *Exception) string {
	if exc == nil {
		return ""
	}
	return rt.realm.StackTrace(exc.Value)
}

// guard is deferred by every method that can run JavaScript. A Go panic
// that unwound through the engine (an engine bug or a panicking host
// function) skipped the interpreter's frame exits, so the call bookkeeping
// is reset to its value at entry and the panic becomes an *InternalError;
// at the outermost call the jobs it left queued are dropped, so that they
// do not run in the next call.
func (rt *Runtime) guard(err *error, saved engine.CallState, argBase int) {
	x := recover()
	if x == nil {
		return
	}
	rt.recovered(err, x, saved, argBase)
}

// recovered restores the runtime after the panic x and sets *err to it.
func (rt *Runtime) recovered(err *error, x any, saved engine.CallState, argBase int) {
	rt.realm.RestoreCallState(saved)
	clear(rt.argStack[argBase:])
	rt.argStack = rt.argStack[:argBase]
	*err = &InternalError{Value: x, Stack: debug.Stack()}
	rt.realm.DropJobs(*err)
}

// guardLoad is guard for Load: a panic that stopped the evaluation before
// the entry's top level ran to its end or first await fails the entry's
// hooks, as a throw does.
func (rt *Runtime) guardLoad(err *error, saved engine.CallState, argBase int) {
	if x := recover(); x != nil {
		rt.loadPanicked(err, x, nil, saved, argBase)
	}
}

// guardGraph is guardLoad for loadGraph of g.
func (rt *Runtime) guardGraph(err *error, g *engine.ModuleGraph, saved engine.CallState, argBase int) {
	if x := recover(); x != nil {
		rt.loadPanicked(err, x, g, saved, argBase)
	}
}

// loadPanicked handles the panic x of a Load, through the graph g if not
// nil.
func (rt *Runtime) loadPanicked(err *error, x any, g *engine.ModuleGraph, saved engine.CallState, argBase int) {
	rt.recovered(err, x, saved, argBase)
	ran := rt.env != nil && rt.failed == nil
	if g != nil {
		ran = rt.realm.GraphRan(g)
	}
	if !ran {
		rt.failed = *err
	}
}

package test262

import (
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/syntax"
)

// The runner compiles eval code and dynamic functions with the compiler
// the root package installs.
func init() { engine.SetCompiler(compiler.Hook{}) }

// host is the per-test host environment: every realm the test creates (the
// main one and those from $262.createRealm), so that the timeout interrupts
// all of them, and what the test printed.
type host struct {
	mu      sync.Mutex
	realms  []*engine.Realm
	stopped bool
	printed []string
	// imports are the test's import() hooks, which every realm of the test
	// uses.
	imports *engine.ImportHooks
}

// interrupt stops every realm of the test, including those created later.
func (h *host) interrupt() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	for _, r := range h.realms {
		r.Interrupt("test262 timeout")
	}
}

// newRealm creates a fresh SharedIntrinsics: false realm (test262 mutates
// builtins routinely) with the host-defined print and $262 globals. Date's
// local time zone is UTC so the baseline does not depend on the machine: the
// suite assumes whole-minute offsets, which local mean time before 1900 in
// most zones does not have.
func (h *host) newRealm() *engine.Realm {
	r := engine.NewRealmWith(engine.RealmOptions{SharedIntrinsics: false, TimeZone: time.UTC})
	r.SetImportHooks(h.imports)
	h.mu.Lock()
	h.realms = append(h.realms, r)
	if h.stopped {
		r.Interrupt("test262 timeout")
	}
	h.mu.Unlock()

	g := r.Global
	define(r, g, "print", r.NewNativeFunction(engine.FromGoString("print"), 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		parts := make([]string, len(args))
		for i, a := range args {
			if a.IsString() { // as is: doneprintHandle.js prints its result lines
				parts[i] = a.String()
			} else {
				parts[i] = r.DisplayString(a)
			}
		}
		h.mu.Lock()
		h.printed = append(h.printed, strings.Join(parts, " "))
		h.mu.Unlock()
		return engine.Undefined(), nil
	}))
	d := r.NewObject()
	define(r, d, "global", g)
	define(r, d, "gc", r.NewNativeFunction(engine.FromGoString("gc"), 0, func(*engine.Realm, engine.Value, []engine.Value) (engine.Value, error) {
		runtime.GC()
		return engine.Undefined(), nil
	}))
	define(r, d, "detachArrayBuffer", r.NewNativeFunction(engine.FromGoString("detachArrayBuffer"), 1, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		return engine.Undefined(), r.DetachArrayBuffer(engine.Arg(args, 0))
	}))
	define(r, d, "createRealm", r.NewNativeFunction(engine.FromGoString("createRealm"), 0, func(*engine.Realm, engine.Value, []engine.Value) (engine.Value, error) {
		nr := h.newRealm()
		return nr.Global.GetProp(nr, nr.KeyFromGoString("$262"))
	}))
	define(r, d, "evalScript", r.NewNativeFunction(engine.FromGoString("evalScript"), 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		arg := engine.Undefined()
		if len(args) > 0 {
			arg = args[0]
		}
		src, err := r.ToString(arg)
		if err != nil {
			return engine.Undefined(), err
		}
		return r.EvalScript("evalScript", src.GoString())
	}))
	define(r, g, "$262", d)
	return r
}

// define sets a host property on o.
func define(r *engine.Realm, o *engine.Object, name string, v *engine.Object) {
	if err := o.SetProp(r, r.KeyFromGoString(name), engine.ObjectValue(v)); err != nil {
		panic(err) // a fresh realm's global object and a new object accept any property
	}
}

// compileScript parses and compiles a script: sloppy mode code unless it
// starts with a "use strict" directive.
func compileScript(name, src string) (*bytecode.Function, error) {
	s, err := syntax.ParseScript(name, src, syntax.Options{})
	if err != nil {
		return nil, err
	}
	return compiler.CompileScript(s)
}

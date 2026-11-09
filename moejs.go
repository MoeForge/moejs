// Package moejs embeds the moejs JavaScript engine in a Go host that runs
// ES modules as plugins: compile a module once, evaluate it in any number of
// runtimes, call its exported functions with Go data and read their results
// back as Go data or JSON bytes.
//
//	mod, err := moejs.Compile("plugin.js", source)
//	hook, err := mod.Hook("protocols", "openai", "decodeRequest")
//	rt := moejs.NewRuntime(moejs.Options{})
//	err = rt.SetGlobal("utils", map[string]any{"now": moejs.NativeFunc(now)})
//	err = rt.Load(mod)
//	arg, err := rt.ParseJSON(body)
//	res, err := rt.Call(hook, arg)
//	out, err := rt.AppendJSON(nil, res)
//
// Values are engine values (package engine) under their own names, so host
// functions and the host share one representation and nothing is wrapped on
// the way in or out. Errors are returned, never panicked: a JavaScript throw
// is an *Exception, an interrupt an *InterruptedError, bad source a
// *SyntaxError.
//
// The jobs JavaScript queues (promise reactions, queueMicrotask callbacks)
// run before the method that ran it returns, getters and proxy traps
// included (Load, Call, CallFunction, Has, Get, ToGo, AppendJSON,
// SetGlobal, the settlers of NewPromise): the first exception a callback
// throws is the method's
// error when it succeeded otherwise, and an interrupt drops the jobs left.
//
// A module that imports others is linked once with the modules it imports,
// which the host resolves (moejs never reads files or the network):
//
//	mod, err := moejs.Link(entry, func(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
//		return host.modules[specifier], nil // compiled once, by the host
//	})
//
// A runtime that loads the linked module evaluates each module of its graph
// once; its hooks and exports are the entry's, re-exported names included.
// import() and import.meta are the host's too, through the Importer of
// Options, which resolves with the same kind of resolver:
//
//	rt := moejs.NewRuntime(moejs.Options{Importer: &moejs.Importer{Resolve: resolve}})
//
// A module import() loads joins the runtime's modules: one the runtime
// evaluated before is not evaluated again.
//
// A Module is immutable and may be loaded by many runtimes concurrently. A
// Runtime is one global environment with one loaded module; it must be used
// from one goroutine at a time, except Interrupt and ClearInterrupt.
package moejs

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/MoeForge/moejs/engine"
)

type (
	// Value is a JavaScript value.
	Value = engine.Value
	// Realm is the engine state a Runtime owns; host functions receive it.
	Realm = engine.Realm
	// Object is a JavaScript object.
	Object = engine.Object
	// NativeFunc is a host function. Returning an error throws: an
	// *Exception or *InterruptedError as is, any other error as an Error
	// whose message is err.Error() and which unwraps to err.
	NativeFunc = engine.NativeFunc
	// Exception is a thrown JavaScript value.
	Exception = engine.Exception
	// InterruptedError is returned when Interrupt stopped running code.
	InterruptedError = engine.InterruptedError
	// MemoryLimitError is the value of the *InterruptedError a runtime
	// stops with when what it keeps and allocates passes
	// Options.MemoryLimit; it unwraps to ErrMemoryLimit.
	MemoryLimitError = engine.MemoryLimitError
	// Stats are a runtime's counters (Runtime.Stats).
	Stats = engine.Stats
	// LiveReporter is implemented by the Go state a host attaches to an
	// object it creates (Object.SetInternal), or to a data function, that
	// keeps JavaScript values or Go memory for it: the measurements of the
	// memory limit call ReportLive when they reach the object, and count
	// what it reports while the object is reachable (engine.LiveReporter).
	// State reachable from no object, which Go alone holds, goes to
	// Runtime.AddLiveRoots instead.
	LiveReporter = engine.LiveReporter
)

// Arg returns args[i], or undefined when there are fewer arguments.
var Arg = engine.Arg

// Value constructors, for host functions. None allocates except String.
var (
	Undefined = engine.Undefined
	Null      = engine.Null
	Bool      = engine.Bool
	Number    = engine.NumberValue
	Int       = engine.Int64Value
)

// String converts a Go string. One that is not ASCII, or is at least 64 KiB
// long, keeps its bytes until its content is first read; its length is known
// without decoding. Invalid UTF-8 bytes read as U+FFFD.
func String(s string) Value { return engine.StringValue(engine.FromHostString(s)) }

var (
	// ErrHookNotFound is returned when a hook's export or one of its members
	// is missing, undefined or null.
	ErrHookNotFound = errors.New("moejs: hook not found")
	// ErrNotCallable is returned when a hook names a value that is not a
	// function, and when CallFunction is given one.
	ErrNotCallable = errors.New("moejs: not a function")
	// ErrForeign is returned for a function or generator of another
	// runtime (or a bound function or proxy of one) passed to Call,
	// CallFunction, SetGlobal, FromGo or a settler of NewPromise: it runs
	// only in the runtime that created it. Inside a Go map or slice FromGo
	// or SetGlobal converts it is not the error: reading that member throws
	// a TypeError with ErrForeign's text.
	ErrForeign = engine.ErrForeign
	// ErrModulePending is Load's error for a module with top-level await
	// whose evaluation still awaits once no job is left.
	ErrModulePending = engine.ErrModulePending
	// ErrMemoryLimit is what errors.Is finds in the error of a call that
	// passed Options.MemoryLimit (a *MemoryLimitError).
	ErrMemoryLimit = engine.ErrMemoryLimit
	// ErrTooDeep is what errors.Is finds in the RangeError of a value
	// nested deeper than 10,000 arrays and objects (engine.MaxToGoDepth):
	// the text ParseJSON, ParseJSONString or JSON.parse reads (a reviver's
	// walk included), the value AppendJSON or JSON.stringify writes, and
	// the value ToGo exports. Its message says what happened, as in "JSON
	// nesting is too deep (more than 10000 levels)". (Unmarshal's limits
	// wrap ErrTooLarge instead.)
	ErrTooDeep = engine.ErrTooDeep
)

// The errors of RunScript, Load and Link given nil, what a failed
// CompileScript or Compile returns with its error.
var (
	errNilScript = errors.New("moejs: RunScript of a nil *Script")
	errNilModule = errors.New("moejs: Load of a nil *Module")
	errNilLink   = errors.New("moejs: Link of a nil *Module")
)

// SyntaxError is a parse or early error in module source.
type SyntaxError struct {
	File    string
	Line    int // 1-based
	Column  int // 1-based, in code points
	Message string
}

// Error formats as "file:line:col: SyntaxError: message".
func (e *SyntaxError) Error() string {
	return e.File + ":" + strconv.Itoa(e.Line) + ":" + strconv.Itoa(e.Column) + ": SyntaxError: " + e.Message
}

// InternalError is a Go panic that escaped the engine: an engine bug or a
// host function that panicked. The runtime's call state is restored and the
// jobs left queued are dropped, but the host should drop the runtime.
type InternalError struct {
	Value any    // the recovered panic value
	Stack []byte // the goroutine stack at the panic
}

func (e *InternalError) Error() string {
	return fmt.Sprintf("moejs: internal error: %v", e.Value)
}

// Unwrap returns the panic value when it is an error.
func (e *InternalError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

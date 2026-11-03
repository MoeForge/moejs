// Package webapi installs Web APIs into a moejs runtime that runs on an
// event loop (package eventloop): fetch with streamed response bodies, the
// Streams, URL and URLSearchParams, Headers, Request and Response,
// AbortController and AbortSignal with EventTarget and Event, DOMException,
// and performance.now. A bare runtime has none of them: the host installs
// them explicitly, and fetch reaches only the network the host's
// http.Client and Allow hook permit.
//
//	rt := moejs.NewRuntime(moejs.Options{})
//	loop, err := eventloop.New(rt, eventloop.Options{})
//	api, err := webapi.Install(loop, webapi.Options{
//		Client: http.DefaultClient,
//		Allow:  webapi.AllowHosts("api.openai.com", "*.anthropic.com"),
//	})
//	defer api.Close()
//	err = loop.Run(func(rt *moejs.Runtime) error { ... })
//
// The globals are created when JavaScript first reads them, so a runtime
// that never uses one pays only for its name. They are writable and
// configurable properties of the global object, as Web IDL defines them,
// also when the runtime shares the frozen builtins.
//
// # Event loop
//
// Every promise of the package settles on the loop, in a task of its tasks
// phase, and the jobs that queues run before the next task. A fetch waiting
// for its response and a body read waiting for its chunk keep Run alive; a
// response body nobody reads does not. The objects belong to the runtime
// and are used on the loop only, like every JavaScript value; the Go side
// (the request, its body) runs on goroutines of its own and hands its
// results to the loop.
//
// # Streaming
//
// A response body is a ReadableStream whose source reads the
// http.Response's Body on a goroutine, from the stream's first read on, at
// most one chunk ahead of what JavaScript took: the chunk read ahead waits
// until the stream takes it, and the next read runs while JavaScript works
// on it. Each chunk is a Uint8Array over the bytes the read returned, not
// copied again, charged to the runtime's memory limit
// (moejs.Options.MemoryLimit) in the task that hands it over. text(),
// json(), arrayBuffer() and bytes() hand the whole body over in one task,
// so a body larger than the limit stops the loop with the limit's error,
// and Go reads little more of it than the limit. reader.cancel(), the
// request's AbortSignal, Close and the end of the body close the Body at
// once, also while a read of it is blocked, and cancel the request's
// context; an abort errors the stream with the signal's reason, so pending
// reads reject with it (an AbortError by default). A network error before
// the response is a TypeError "fetch failed" whose cause names the Go
// error; one during the body errors the stream with a TypeError
// "terminated". See the package's documentation in docs/guide.md for the
// whole contract and the deviations from the specifications.
package webapi

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/webapi/internal/whatwgurl"
)

// Options configures Install.
type Options struct {
	// Client sends the requests of fetch. Nil means no network: every
	// fetch rejects with a TypeError. The package copies the Client to
	// follow the redirect mode of a request, and calls the Client's own
	// CheckRedirect, when set, after its own checks.
	Client *http.Client
	// Allow, when not nil, approves each request before it is sent,
	// including each redirect: an error rejects the fetch with a TypeError
	// whose cause has the error's message. It sees the request as it is
	// sent, its Host when the script set that header too. It is called on
	// the request's goroutine, so concurrently for concurrent fetches.
	// AllowHosts returns one for a list of hosts. A nil Allow approves
	// every request: fetch reaches every address the Client can, the
	// intranet, loopback and 169.254.169.254 (cloud metadata) included, so
	// a host that runs scripts it does not trust sets one.
	Allow func(*http.Request) error
	// Context is the parent of every request's context: canceling it
	// fails the fetches in flight as a network error does. Nil means
	// context.Background(). API.SetContext adds a parent for the
	// fetches of one run.
	Context context.Context
}

// API is the Web APIs installed in one runtime.
type API struct{ e *env }

// Install installs the Web APIs as globals of the loop's runtime, as the
// package documentation describes. It is called once per runtime, while
// the loop does not run or on the loop.
func Install(loop *eventloop.Loop, opts Options) (*API, error) {
	if loop == nil {
		return nil, errors.New("webapi: Install needs a loop")
	}
	rt := loop.Runtime()
	r := rt.Realm()
	atoms.once.Do(func() { atoms.init(r) })
	e := &env{loop: loop, rt: rt, r: r, opts: opts, epoch: time.Now(), keys: &atoms.keys}
	e.install()
	if !r.HasSharedIntrinsics() {
		// A script can replace or delete mutable builtins: take them
		// before one runs.
		e.intr = e.captureIntrinsics()
	}
	return &API{e}, nil
}

// env is the state of one installation: the runtime's loop, the classes
// created so far and the requests in flight. Except for mu and what it
// guards, only the loop's goroutine uses it.
type env struct {
	loop  *eventloop.Loop
	rt    *moejs.Runtime
	r     *engine.Realm
	opts  Options
	epoch time.Time
	keys  *keys
	netState

	intr    *intrinsics
	classes [numClasses]*class
	// urlOpts are the URL parser's options.
	urlOpts *whatwgurl.Options
	// noop is a function that returns undefined, the rejection handler
	// that marks a promise handled.
	noop *engine.Object
	// live lists what the installation holds for JavaScript, for the
	// measurements of the memory limit (live.go).
	live liveSet
}

// keys are the property keys the package uses often.
type keys struct {
	value, done, then, length, message, name, cause, code engine.PropertyKey
	next, handleEvent, signal, stream, constructor        engine.PropertyKey
}

// atoms are the property keys of the package, interned once per process:
// an interned name is the same *engine.String in every realm
// (engine/intern.go), and holding them keeps them in the process-wide
// table, so that Install interns nothing.
var atoms atomTable

type atomTable struct {
	once    sync.Once
	keys    keys
	globals [len(globalNames)]engine.PropertyKey
}

func (a *atomTable) init(r *engine.Realm) {
	k := func(s string) engine.PropertyKey { return engine.StringKey(r.InternGoString(s)) }
	a.keys = keys{
		value: k("value"), done: k("done"), then: k("then"), length: k("length"),
		message: k("message"), name: k("name"), cause: k("cause"), code: k("code"),
		next: k("next"), handleEvent: k("handleEvent"), signal: k("signal"), stream: k("stream"),
		constructor: k("constructor"),
	}
	for i, g := range globalNames {
		a.globals[i] = r.KeyFromGoString(g.name)
	}
}

// intrinsics are the builtins the package calls: the realm's own, never
// what a script put in their place. With the shared frozen builtins (the
// default) no script can change them, and they are read on first use; a
// runtime with MutableIntrinsics reads them in Install. A nil field is one
// the runtime did not have then: its use is a TypeError. then still looks
// up the species constructor of the promise it is called on, which a
// script can define with MutableIntrinsics.
type intrinsics struct {
	promise, promiseResolve, then *engine.Object // %Promise%, its resolve and then
	uint8Array                    *engine.Object
	textDecoder, decode           *engine.Object // TextDecoder and its decode
	normalize                     *engine.Object // String.prototype.normalize
}

// intrinsics returns the builtins the package calls.
func (e *env) intrinsics() *intrinsics {
	if e.intr == nil {
		e.intr = e.captureIntrinsics()
	}
	return e.intr
}

func (e *env) captureIntrinsics() *intrinsics {
	r := e.r
	get := func(o *engine.Object, key engine.PropertyKey) *engine.Object {
		if o == nil {
			return nil
		}
		v, err := o.GetProp(r, key)
		if err != nil || !v.IsObject() {
			return nil
		}
		return v.AsObject()
	}
	name := func(s string) engine.PropertyKey { return r.KeyFromGoString(s) }
	// The prototype of a promise of the realm is %Promise.prototype%,
	// whatever the global Promise is.
	p := r.NewHostPromise()
	proto := p.Proto()
	x := &intrinsics{then: get(proto, e.keys.then), promise: get(proto, e.keys.constructor)}
	x.promiseResolve = get(x.promise, name("resolve"))
	x.uint8Array = get(r.Global, name("Uint8Array"))
	x.textDecoder = get(r.Global, name("TextDecoder"))
	x.decode = get(get(x.textDecoder, name("prototype")), name("decode"))
	x.normalize = get(r.StringPrototype, name("normalize"))
	return x
}

// need returns o, or the TypeError of a builtin the runtime does not have.
func (e *env) need(o *engine.Object, name string) (*engine.Object, error) {
	if o == nil || !o.IsCallable() {
		return nil, e.r.TypeError("The runtime has no %s", name)
	}
	return o, nil
}

// Property attribute bits of the engine (engine.DataDescriptor).
const (
	attrW = 1 << 0
	attrE = 1 << 1
	attrC = 1 << 2
	// attrMethod is the attribute set of a Web IDL operation or attribute:
	// writable (for data properties), enumerable and configurable.
	attrMethod = attrW | attrE | attrC
	// attrGlobal is the attribute set of an interface object on the global
	// object: writable and configurable, not enumerable.
	attrGlobal = attrW | attrC
)

// --- classes --------------------------------------------------------------------

// classID names a class the package creates on first use.
type classID uint8

const (
	clsDOMException classID = iota
	clsEvent
	clsEventTarget
	clsAbortSignal
	clsAbortController
	clsPerformance
	clsURL
	clsURLSearchParams
	clsURLSearchParamsIterator
	clsReadableStream
	clsReadableStreamDefaultReader
	clsReadableStreamDefaultController
	clsReadableStreamAsyncIterator
	clsWritableStream
	clsWritableStreamDefaultWriter
	clsWritableStreamDefaultController
	clsTransformStream
	clsTransformStreamDefaultController
	clsByteLengthQueuingStrategy
	clsCountQueuingStrategy
	clsTextDecoderStream
	clsTextEncoderStream
	clsHeaders
	clsHeadersIterator
	clsRequest
	clsResponse
	numClasses
)

// class is a constructor and its prototype.
type class struct {
	ctor, proto *engine.Object
}

// member is a property of a prototype or constructor: a method (fn), an
// accessor (get, and set for a writable one) or a constant (value).
type member struct {
	name     string
	sym      *engine.Symbol // the key instead of name
	length   int
	fn       engine.NativeFunc
	get, set engine.NativeFunc
	value    engine.Value
	isValue  bool
}

// classSpec describes a class for defineClass.
type classSpec struct {
	name   string
	length int
	// ctor is the [[Construct]]; nil makes the constructor throw an
	// "Illegal constructor" TypeError, for the interfaces that JavaScript
	// cannot construct.
	ctor    engine.NativeCtor
	parent  classID
	hasBase bool // parent is set
	members []member
	statics []member
	// noCtor makes a prototype with no constructor, for iterator
	// prototypes; proto is its [[Prototype]] (nil: Object.prototype).
	noCtor bool
	proto  *engine.Object
}

// class returns the class id, creating it on first use.
func (e *env) class(id classID) *class {
	if c := e.classes[id]; c != nil {
		return c
	}
	c := e.defineClass(e.specs(id))
	e.classes[id] = c
	return c
}

// proto returns the prototype of the class id.
func (e *env) proto(id classID) *engine.Object { return e.class(id).proto }

func (e *env) defineClass(s classSpec) *class {
	r := e.r
	protoProto := r.ObjectPrototype
	var parentCtor *engine.Object
	if s.hasBase {
		pc := e.class(s.parent)
		protoProto, parentCtor = pc.proto, pc.ctor
	}
	if s.proto != nil {
		protoProto = s.proto
	}
	proto := r.NewObjectWithProto(protoProto)
	c := &class{proto: proto}
	name := r.InternGoString(s.name)
	if !s.noCtor {
		ctor := s.ctor
		if ctor == nil {
			ctor = func(r *engine.Realm, _ []engine.Value, _ *engine.Object) (engine.Value, error) {
				return engine.Undefined(), r.TypeError("Illegal constructor")
			}
		}
		c.ctor = r.NewNativeConstructor(name, s.length, nil, ctor)
		if parentCtor != nil {
			c.ctor.SetPrototypeOf(r, parentCtor)
		}
		c.ctor.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("prototype")), engine.ObjectValue(proto), 0)
		proto.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("constructor")), engine.ObjectValue(c.ctor), attrW|attrC)
		e.defineMembers(c.ctor, s.name, s.statics)
	}
	e.defineMembers(proto, s.name+".prototype", s.members)
	proto.DefineOwnDataFast(r, engine.SymbolKey(engine.SymToStringTag), engine.StringValue(name), attrC)
	return c
}

func (e *env) defineMembers(o *engine.Object, owner string, ms []member) {
	r := e.r
	for _, m := range ms {
		var key engine.PropertyKey
		label := m.name
		if m.sym != nil {
			key = engine.SymbolKey(m.sym)
		} else {
			key = engine.StringKey(r.InternGoString(m.name))
		}
		switch {
		case m.isValue:
			o.DefineOwnDataFast(r, key, m.value, attrE)
		case m.fn != nil:
			o.DefineOwnDataFast(r, key, engine.ObjectValue(r.NewNativeFunction(r.InternGoString(label), m.length, m.fn)), attrMethod)
		default:
			get := r.NewNativeFunction(r.InternGoString("get "+label), 0, m.get)
			var set *engine.Object
			if m.set != nil {
				set = r.NewNativeFunction(r.InternGoString("set "+label), 1, m.set)
			}
			o.DefineOwnAccessorFast(r, key, get, set, attrE|attrC)
		}
		_ = owner
	}
}

// newObject creates an object of the class id for a constructor called
// with newTarget (nil: the class's own prototype), holding state.
func (e *env) newObject(id classID, newTarget *engine.Object, state any) (*engine.Object, error) {
	c := e.class(id)
	var o *engine.Object
	if newTarget == nil || newTarget == c.ctor {
		o = e.r.NewObjectWithProto(c.proto)
	} else {
		var err error
		if o, err = e.r.OrdinaryCreateFromConstructor(newTarget, c.proto, engine.ClassObject); err != nil {
			return nil, err
		}
	}
	o.SetInternal(state)
	return o, nil
}

// thisAs returns the state of type T the receiver v holds, or the
// TypeError of a method called on another object.
func thisAs[T any](r *engine.Realm, v engine.Value, method string) (T, error) {
	if v.IsObject() {
		if s, ok := v.AsObject().Internal().(T); ok {
			return s, nil
		}
	}
	var zero T
	return zero, r.TypeError("Method %s called on incompatible receiver %s", method, r.DisplayString(v))
}

// globalNames are the globals of the package: the interface objects of
// the classes, then fetch and performance.
var globalNames = [...]struct {
	name string
	id   classID
}{
	{"DOMException", clsDOMException}, {"Event", clsEvent}, {"EventTarget", clsEventTarget},
	{"AbortSignal", clsAbortSignal}, {"AbortController", clsAbortController},
	{"Performance", clsPerformance},
	{"URL", clsURL}, {"URLSearchParams", clsURLSearchParams},
	{"ReadableStream", clsReadableStream}, {"ReadableStreamDefaultReader", clsReadableStreamDefaultReader},
	{"ReadableStreamDefaultController", clsReadableStreamDefaultController},
	{"WritableStream", clsWritableStream}, {"WritableStreamDefaultWriter", clsWritableStreamDefaultWriter},
	{"WritableStreamDefaultController", clsWritableStreamDefaultController},
	{"TransformStream", clsTransformStream}, {"TransformStreamDefaultController", clsTransformStreamDefaultController},
	{"ByteLengthQueuingStrategy", clsByteLengthQueuingStrategy}, {"CountQueuingStrategy", clsCountQueuingStrategy},
	{"TextDecoderStream", clsTextDecoderStream}, {"TextEncoderStream", clsTextEncoderStream},
	{"Headers", clsHeaders}, {"Request", clsRequest}, {"Response", clsResponse},
	{"fetch", numClasses}, {"performance", numClasses},
}

// install defines the globals, each created on first read.
func (e *env) install() {
	e.r.Global.DefineLazyProperties(e.r, atoms.globals[:], e.global)
}

// global creates the global globalNames[i].
func (e *env) global(r *engine.Realm, i int) engine.Value {
	switch n := len(globalNames); i {
	case n - 2:
		return engine.ObjectValue(r.NewNativeFunction(r.InternGoString("fetch"), 1, e.fetch))
	case n - 1:
		o, _ := e.newObject(clsPerformance, nil, &eventTarget{})
		return engine.ObjectValue(o)
	}
	return engine.ObjectValue(e.class(globalNames[i].id).ctor)
}

// specs returns the description of the class id.
func (e *env) specs(id classID) classSpec {
	switch id {
	case clsDOMException:
		return e.domExceptionSpec()
	case clsEvent:
		return e.eventSpec()
	case clsEventTarget:
		return e.eventTargetSpec()
	case clsAbortSignal:
		return e.abortSignalSpec()
	case clsAbortController:
		return e.abortControllerSpec()
	case clsPerformance:
		return e.performanceSpec()
	case clsURL:
		return e.urlSpec()
	case clsURLSearchParams:
		return e.searchParamsSpec()
	case clsURLSearchParamsIterator:
		return e.pairIteratorSpec("URLSearchParams Iterator", searchParamsPairAt)
	case clsReadableStream:
		return e.readableStreamSpec()
	case clsReadableStreamDefaultReader:
		return e.readerSpec()
	case clsReadableStreamDefaultController:
		return e.rsControllerSpec()
	case clsReadableStreamAsyncIterator:
		return e.rsAsyncIteratorSpec()
	case clsWritableStream:
		return e.writableStreamSpec()
	case clsWritableStreamDefaultWriter:
		return e.writerSpec()
	case clsWritableStreamDefaultController:
		return e.wsControllerSpec()
	case clsTransformStream:
		return e.transformStreamSpec()
	case clsTransformStreamDefaultController:
		return e.tsControllerSpec()
	case clsByteLengthQueuingStrategy:
		return e.strategySpec("ByteLengthQueuingStrategy", true)
	case clsCountQueuingStrategy:
		return e.strategySpec("CountQueuingStrategy", false)
	case clsTextDecoderStream:
		return e.textDecoderStreamSpec()
	case clsTextEncoderStream:
		return e.textEncoderStreamSpec()
	case clsHeaders:
		return e.headersSpec()
	case clsHeadersIterator:
		return e.pairIteratorSpec("Headers Iterator", headersPairAt)
	case clsRequest:
		return e.requestSpec()
	case clsResponse:
		return e.responseSpec()
	}
	panic("webapi: unknown class")
}

// --- values ---------------------------------------------------------------------

var undefined = engine.Undefined()

// str converts a Go string.
func str(s string) engine.Value { return engine.StringValue(engine.FromGoString(s)) }

// goString converts v with ToString to a Go string; lone surrogates become
// U+FFFD (USVString).
func (e *env) goString(v engine.Value) (string, error) {
	if v.IsString() {
		return v.AsString().GoString(), nil
	}
	s, err := e.r.ToString(v)
	if err != nil {
		return "", err
	}
	return s.GoString(), nil
}

// newError creates an error of kind with message msg.
func (e *env) newError(kind engine.ErrorKind, msg string) *engine.Object {
	return e.r.NewErrorValue(kind, engine.FromGoString(msg)).AsObject()
}

// typeError returns a new TypeError object with message msg.
func (e *env) typeError(msg string) engine.Value {
	return engine.ObjectValue(e.newError(engine.KindTypeError, msg))
}

// rangeError returns a new RangeError object with message msg.
func (e *env) rangeError(msg string) engine.Value {
	return engine.ObjectValue(e.newError(engine.KindRangeError, msg))
}

// get reads o[key] of an object value; undefined for undefined and null.
func (e *env) get(v engine.Value, key engine.PropertyKey) (engine.Value, error) {
	if v.IsNullish() {
		return undefined, nil
	}
	return e.r.GetV(v, key)
}

// getName reads v[name].
func (e *env) getName(v engine.Value, name string) (engine.Value, error) {
	return e.get(v, e.r.KeyFromGoString(name))
}

// iterResult creates { value, done }.
func (e *env) iterResult(v engine.Value, done bool) engine.Value {
	o := e.r.NewObject()
	o.DefineOwnDataFast(e.r, e.keys.value, v, attrMethod)
	o.DefineOwnDataFast(e.r, e.keys.done, engine.Bool(done), attrMethod)
	return engine.ObjectValue(o)
}

// fn creates an anonymous native function.
func (e *env) fn(length int, f engine.NativeFunc) *engine.Object {
	return e.r.NewNativeFunction(engine.AtomEmpty, length, f)
}

// --- promises -------------------------------------------------------------------

// promise is a promise the package settles (engine.Realm.NewHostPromise).
type promise struct {
	p *engine.Object
}

func (e *env) newPromise() promise {
	return promise{e.r.NewHostPromise()}
}

// value returns the promise as a value.
func (p promise) value() engine.Value { return engine.ObjectValue(p.p) }

// settled reports whether the promise is no longer pending.
func (p promise) settled() bool {
	st, _, _ := p.p.PromiseResult()
	return st != engine.PromisePending
}

// fulfill resolves the promise with v; it returns only an interrupt's
// error, as the jobs run later.
func (e *env) fulfill(p promise, v engine.Value) error {
	return e.r.SettlePromise(p.p, v, false)
}

// rejectP rejects the promise with reason.
func (e *env) rejectP(p promise, reason engine.Value) error {
	return e.r.SettlePromise(p.p, reason, true)
}

// resolved returns a promise resolved with v (PromiseResolve).
func (e *env) resolved(v engine.Value) (engine.Value, error) {
	x := e.intrinsics()
	res, err := e.need(x.promiseResolve, "Promise.resolve")
	if err != nil {
		return undefined, err
	}
	return e.r.CallObject(res, engine.ObjectValue(x.promise), []engine.Value{v})
}

// rejected returns a promise rejected with reason.
func (e *env) rejected(reason engine.Value) engine.Value {
	p := e.newPromise()
	_ = e.rejectP(p, reason)
	return p.value()
}

// rejectedErr returns a promise rejected with what JavaScript would catch
// for err, or err itself for an interrupt.
func (e *env) rejectedErr(err error) (engine.Value, error) {
	reason, ok := e.r.ThrownValue(err)
	if !ok {
		return undefined, err
	}
	return e.rejected(reason), nil
}

// upon reacts to v, a promise or any value, once it settles: onFul with
// its value, onRej with its reason. The reactions return only an
// interrupt's error.
func (e *env) upon(v engine.Value, onFul, onRej func(engine.Value) error) error {
	p, err := e.resolved(v)
	if err != nil {
		return err
	}
	return e.uponPromise(p, onFul, onRej)
}

// uponPromise is upon for a value that is a promise of the realm.
func (e *env) uponPromise(p engine.Value, onFul, onRej func(engine.Value) error) error {
	return e.uponPromiseKeep(p, undefined, onFul, onRej)
}

// uponKeep is upon for reactions whose closures keep keep, an object only
// they hold, such as the promise they settle: under a memory limit the
// reactions report it (reaction), so that it counts while the promise
// they wait for is reachable.
func (e *env) uponKeep(v, keep engine.Value, onFul, onRej func(engine.Value) error) error {
	p, err := e.resolved(v)
	if err != nil {
		return err
	}
	return e.uponPromiseKeep(p, keep, onFul, onRej)
}

// uponPromiseKeep is uponKeep for a value that is a promise of the realm.
func (e *env) uponPromiseKeep(p, keep engine.Value, onFul, onRej func(engine.Value) error) error {
	keeps := keep.IsObject() && e.r.MemoryLimit() > 0
	react := func(f func(engine.Value) error) engine.Value {
		if f == nil {
			return engine.ObjectValue(e.noopFn())
		}
		if keeps {
			return engine.ObjectValue(e.r.NewNativeDataFunction(engine.AtomEmpty, 1, callReaction, &reaction{f, keep}))
		}
		return engine.ObjectValue(e.fn(1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
			return undefined, f(engine.Arg(args, 0))
		}))
	}
	then, err := e.need(e.intrinsics().then, "Promise.prototype.then")
	if err != nil {
		return err
	}
	_, err = e.r.CallObject(then, p, []engine.Value{react(onFul), react(onRej)})
	return err
}

// markHandled gives the promise p a rejection handler, as setting
// [[PromiseIsHandled]] does, so that its rejection is not reported.
func (e *env) markHandled(p engine.Value) {
	if p.IsObject() {
		if st, _, ok := p.AsObject().PromiseResult(); ok && st != engine.PromiseFulfilled {
			if then := e.intrinsics().then; then != nil {
				_, _ = e.r.CallObject(then, p, []engine.Value{undefined, engine.ObjectValue(e.noopFn())})
			}
		}
	}
}

func (e *env) noopFn() *engine.Object {
	if e.noop == nil {
		e.noop = e.fn(0, func(*engine.Realm, engine.Value, []engine.Value) (engine.Value, error) { return undefined, nil })
	}
	return e.noop
}

// method reads the method name of the dictionary d: undefined when absent,
// a TypeError when present and not callable.
func (e *env) method(d engine.Value, name string) (engine.Value, error) {
	v, err := e.getName(d, name)
	if err != nil || v.IsUndefined() {
		return undefined, err
	}
	if !engine.IsCallable(v) {
		return undefined, e.r.TypeError("%s must be a function", name)
	}
	return v, nil
}

// iterate calls f with each value of the iterable v, closing the iterator
// when f fails.
func (e *env) iterate(v engine.Value, f func(engine.Value) error) error {
	r := e.r
	m, err := r.GetMethod(v, engine.SymbolKey(engine.SymIterator))
	if err != nil {
		return err
	}
	if m.IsUndefined() {
		return r.TypeError("%s is not iterable", r.DisplayString(v))
	}
	it, err := r.Call(m, v, nil)
	if err != nil {
		return err
	}
	if !it.IsObject() {
		return r.TypeError("Result of the Symbol.iterator method is not an object")
	}
	next, err := e.get(it, e.keys.next)
	if err != nil {
		return err
	}
	for {
		res, err := r.Call(next, it, nil)
		if err != nil {
			return err
		}
		if !res.IsObject() {
			return r.TypeError("Iterator result %s is not an object", r.DisplayString(res))
		}
		done, err := e.get(res, e.keys.done)
		if err != nil {
			return err
		}
		if engine.ToBoolean(done) {
			return nil
		}
		val, err := e.get(res, e.keys.value)
		if err != nil {
			return err
		}
		if err := f(val); err != nil {
			if ret, gerr := r.GetMethod(it, e.r.KeyFromGoString("return")); gerr == nil && !ret.IsUndefined() {
				_, _ = r.Call(ret, it, nil)
			}
			return err
		}
	}
}

// --- tasks ----------------------------------------------------------------------

// post runs task on the loop with the jobs held until it returns, so that
// a task that settles several promises runs their reactions after its own
// steps, as an HTML task does. A Go panic in the task or its jobs is an
// *moejs.InternalError, as one below a call of the runtime is: the call
// state is restored and the jobs left are dropped, and the loop handles
// the error as a callback's. It reports false once the loop is
// terminated.
func (e *env) post(task func() error) bool {
	return eventloop.Post(e.loop, func(*moejs.Runtime) (err error) {
		r := e.r
		saved := r.CallState()
		defer func() {
			if x := recover(); x != nil {
				r.RestoreCallState(saved)
				err = &moejs.InternalError{Value: x, Stack: debug.Stack()}
				r.DropJobs(err)
			}
		}()
		r.HoldJobs()
		return r.ReleaseJobs(task())
	})
}

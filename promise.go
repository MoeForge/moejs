package moejs

import "github.com/MoeForge/moejs/engine"

type (
	// PromiseState is the state of a promise.
	PromiseState = engine.PromiseState
	// PromiseRejectionOperation is what a rejection tracker is told (see
	// SetPromiseRejectionTracker).
	PromiseRejectionOperation = engine.PromiseRejectionOperation
)

// Promise states.
const (
	PromisePending   = engine.PromisePending
	PromiseFulfilled = engine.PromiseFulfilled
	PromiseRejected  = engine.PromiseRejected
)

// Rejection tracker operations.
const (
	// PromiseRejectionReject: the promise was rejected with no handler.
	PromiseRejectionReject = engine.PromiseRejectionReject
	// PromiseRejectionHandle: the first handler was added to the promise,
	// rejected earlier with none.
	PromiseRejectionHandle = engine.PromiseRejectionHandle
)

// NewPromise creates a pending promise, for a host function to return, and
// the functions that settle it. resolve does what the promise's resolve
// function does in JavaScript: it adopts a thenable and fulfills with any
// other value; reject rejects. The first call of either decides and later
// calls do nothing. Both may be called after the host function returned,
// from the goroutine that uses the runtime: called outside a Call, they run
// the jobs they queue before returning, as the end of a Call does, and
// return what a Call would for those (an *InterruptedError, or the first
// exception a queueMicrotask callback threw). A function or generator of
// another runtime is ErrForeign and settles nothing.
func (rt *Runtime) NewPromise() (p Value, resolve, reject func(v Value) error) {
	o := rt.realm.NewHostPromise()
	return engine.ObjectValue(o), rt.settler(o, false), rt.settler(o, true)
}

// settler settles the promise p of NewHostPromise with the value it is
// given, rejecting it if reject.
func (rt *Runtime) settler(p *Object, reject bool) func(v Value) error {
	return func(v Value) (err error) {
		r := rt.realm
		if r.IsForeign(v) {
			return ErrForeign
		}
		defer rt.guard(&err, r.CallState(), len(rt.argStack))
		return r.SettlePromise(p, v, reject)
	}
}

// PromiseResult reports the state of the promise p and its result: the
// fulfillment value or the rejection reason, undefined while pending. ok is
// false when p is not a promise. No user code runs, so a promise a Call
// returned is read as the jobs that Call ran left it.
func PromiseResult(p Value) (state PromiseState, result Value, ok bool) {
	if !p.IsObject() {
		return PromisePending, engine.Undefined(), false
	}
	return p.AsObject().PromiseResult()
}

// SetPromiseRejectionTracker registers f to be called when a promise is
// rejected with no handler (PromiseRejectionReject) and when a promise so
// rejected gets its first handler (PromiseRejectionHandle), ECMAScript's
// HostPromiseRejectionTracker. A promise told Reject and not Handle when a
// Call returns has an unhandled rejection. f runs synchronously, in the
// reject or then that caused the operation. nil removes the tracker.
func (rt *Runtime) SetPromiseRejectionTracker(f func(p Value, op PromiseRejectionOperation)) {
	if f == nil {
		rt.realm.SetPromiseRejectionTracker(nil)
		return
	}
	rt.realm.SetPromiseRejectionTracker(func(p *Object, op PromiseRejectionOperation) {
		f(engine.ObjectValue(p), op)
	})
}

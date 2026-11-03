package engine

import "unsafe"

// The host side of promises: creating one with its resolving functions or
// one the host settles directly, reading its state, and the rejection
// tracker (HostPromiseRejectionTracker). Package moejs builds its promise
// API on these.

// PromiseRejectionOperation is the operation HostPromiseRejectionTracker
// reports.
type PromiseRejectionOperation uint8

const (
	// PromiseRejectionReject: the promise was rejected with no handler.
	PromiseRejectionReject PromiseRejectionOperation = iota
	// PromiseRejectionHandle: the first handler was added to the promise,
	// rejected earlier with none.
	PromiseRejectionHandle
)

// NewPromiseWithResolvers creates a pending promise and its resolve and
// reject functions, as Promise.withResolvers does. Calling them through
// CallObject settles the promise and, from the outermost call, runs the jobs
// that queues.
func (r *Realm) NewPromiseWithResolvers() (p, resolve, reject *Object) {
	p = r.newPromise()
	_, resolve, reject = r.createResolvingFunctions(p)
	return p, resolve, reject
}

// NewHostPromise creates a pending promise for the host to settle with
// SettlePromise: it has no resolving functions, so making and settling it
// allocates the promise only.
func (r *Realm) NewHostPromise() *Object {
	pi := r.promiseIntr()
	hp := &hostPromise{realm: r}
	r.chargeObject(unsafe.Sizeof(hostPromise{}))
	p := r.initPromise(&hp.promiseObject, pi.proto, pi.root)
	hp.data.host = true
	return p
}

// hostPromise is a promise of NewHostPromise with the realm that made it,
// the only one whose SettlePromise settles it.
type hostPromise struct {
	promiseObject
	realm *Realm
}

// SettlePromise does for p, a promise r's NewHostPromise made, what a call
// of its resolve function (reject false) or reject function with v would
// do: the first call resolves or rejects p, adopting a thenable v from a job
// as resolve does, and later calls do nothing. As a call, from outside a
// call it runs the jobs that settling queued and returns what CallObject
// would: an *InterruptedError, or the first exception a queueMicrotask
// callback threw. Like any call into the realm, it runs on the goroutine
// that uses it. It fails, settling nothing, for any other p: one another
// realm made, or one with resolving functions.
func (r *Realm) SettlePromise(p *Object, v Value, reject bool) error {
	d, ok := p.internal.(*promiseData)
	if !ok || !d.host || hostPromiseOfData(d).realm != r {
		return r.TypeError("SettlePromise: not a promise of this realm's NewHostPromise")
	}
	if d.resolved {
		return nil
	}
	d.resolved = true
	r.HoldJobs()
	var err error
	if reject {
		r.rejectPromise(p, v)
	} else {
		err = r.resolvePromiseErr(p, v)
	}
	return r.ReleaseJobs(err)
}

// PromiseResult returns the state of the promise o and its result: the
// fulfillment value or the rejection reason, undefined while pending. ok is
// false when o is not a promise.
func (o *Object) PromiseResult() (state PromiseState, result Value, ok bool) {
	if o.class != ClassPromise {
		return PromisePending, Undefined(), false
	}
	d := o.internal.(*promiseData)
	return d.state, d.result, true
}

// SetPromiseRejectionTracker registers f to be called when a promise is
// rejected with no handler (PromiseRejectionReject) and when a promise so
// rejected gets its first handler (PromiseRejectionHandle), synchronously,
// as HostPromiseRejectionTracker. A promise told Reject and not Handle when
// the outermost call returns has an unhandled rejection. nil removes the
// tracker.
func (r *Realm) SetPromiseRejectionTracker(f func(p *Object, op PromiseRejectionOperation)) {
	if f == nil && (r.lazy == nil || r.lazy.jobs == nil) {
		return
	}
	r.jobState().tracker = f
}

package moejs

import "github.com/MoeForge/moejs/engine"

// CallFunction calls the function fn with this and args, for a host that
// calls a function JavaScript gave it, such as a callback passed to a host
// function, after that call returned. It works as Call does: a value that is
// not a function is ErrNotCallable; a throw is an *Exception, an interrupt
// an *InterruptedError, also one that arrived before a host function fn
// runs, and a Go panic below it an *InternalError; fn, this or an argument
// that is a function or generator of another runtime is ErrForeign. The
// jobs the call queues run before it returns, and the first exception they
// throw is its error when it succeeded otherwise. Called inside another call
// (from a host function), it leaves them to the end of the outermost one,
// and its *InternalError goes to that host function: returned to
// JavaScript like any other Go error, it is thrown as an Error that
// JavaScript can catch, and reaches the outermost call, uncaught, as an
// *Exception that unwraps to it.
func (rt *Runtime) CallFunction(fn, this Value, args ...Value) (res Value, err error) {
	r := rt.realm
	base := len(rt.argStack)
	defer rt.guard(&err, r.CallState(), base)
	if !engine.IsCallable(fn) {
		return engine.Undefined(), ErrNotCallable
	}
	// As in Call: bytecode checks for an interrupt when it is entered,
	// natives do not.
	if err := r.CheckInterrupt(); err != nil {
		r.HoldJobs()
		return engine.Undefined(), r.ReleaseJobs(err)
	}
	if r.IsForeign(fn) || r.IsForeign(this) {
		return engine.Undefined(), ErrForeign
	}
	for _, a := range args {
		if a.IsObject() && a.AsObject().HoldsCode() && r.IsForeign(a) {
			return engine.Undefined(), ErrForeign
		}
	}
	rt.argStack = append(rt.argStack, args...)
	top := len(rt.argStack)
	res, err = r.CallObject(fn.AsObject(), this, rt.argStack[base:top:top])
	clear(rt.argStack[base:top])
	rt.argStack = rt.argStack[:base]
	return res, err
}

// ThrownValue returns the value JavaScript catches when a host function
// returns err: an *Exception's value, and for any other error an Error whose
// message is err.Error() and which, once thrown back to the host as an
// *Exception, unwraps to err. A host rejects a promise of NewPromise with it
// to give JavaScript the error a host function would have thrown. ok is
// false for nil and for an *InterruptedError, which JavaScript cannot
// catch. An *Exception must come from this runtime: one of another runtime
// gives that runtime's value, which this one must not use (Runtime). No
// JavaScript runs.
func (rt *Runtime) ThrownValue(err error) (v Value, ok bool) {
	return rt.realm.ThrownValue(err)
}

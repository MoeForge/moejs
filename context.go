package moejs

import "context"

// RunContext runs fn on the calling goroutine, interrupting JavaScript when
// ctx ends. One scope can include Load, argument conversion, Call and result
// decoding. Like other Runtime methods, it requires exclusive use of rt.
// Both ctx and fn must be non-nil.
//
// A context already canceled or a pending interrupt prevents fn from running.
// Cancellation returns an *InterruptedError carrying context.Cause(ctx).
// If fn returns an error, that error takes precedence and is returned unchanged.
// If fn returns nil after ctx ends, RunContext returns the cancellation error
// even if no JavaScript operation observed it. A panic in fn propagates.
//
// Before returning or propagating a panic, RunContext stops its cancellation
// callback or waits for it to finish. It does not clear interrupts, release
// call data or roll back mutations. After it returns, the host may safely call
// ClearInterrupt and ReleaseCallData before reusing rt; a failed Load still
// leaves its module failed, as usual.
//
// Cancellation is cooperative: it cannot stop blocking Go code in fn or a
// host function. Pass ctx to that code too. Host-side compilation uses
// CompileOptions.Context separately. For an event loop use Loop.Interrupt,
// which also wakes an idle loop; RunContext only interrupts the runtime.
func (rt *Runtime) RunContext(ctx context.Context, fn func(*Runtime) error) (err error) {
	if cause := context.Cause(ctx); cause != nil {
		return &InterruptedError{Value: cause}
	}
	if err := rt.realm.CheckInterrupt(); err != nil {
		return err
	}
	// Background and WithoutCancel contexts need no watcher or channel.
	if ctx.Done() == nil {
		return fn(rt)
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		rt.Interrupt(context.Cause(ctx))
		close(done)
	})
	defer func() {
		// AfterFunc's stop does not join an already started callback. Join
		// it here so that it cannot interrupt a later user of this runtime.
		if !stop() {
			<-done
		}
		if err == nil {
			if cause := context.Cause(ctx); cause != nil {
				err = &InterruptedError{Value: cause}
			}
		}
	}()
	return fn(rt)
}

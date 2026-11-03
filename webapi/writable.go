package webapi

import (
	"github.com/MoeForge/moejs/engine"
)

// The Streams Standard's WritableStream with its default controller and
// writer, and ReadableStream's pipeTo and pipeThrough.

type wsState uint8

const (
	wsWritable wsState = iota
	wsClosed
	wsErroring
	wsErrored
)

// writableStream is the state of a WritableStream.
type writableStream struct {
	obj    *engine.Object
	stored engine.Value
	writer *wsWriter
	ctrl   *wsController

	writeRequests []promise
	inFlightWrite *promise
	inFlightClose *promise
	closeRequest  *promise
	pendingAbort  *pendingAbort
	// held are the JavaScript values the sink's algorithms call, under a
	// memory limit, until they are cleared (readableStream.held).
	held         *[4]engine.Value
	state        wsState
	backpressure bool
}

type pendingAbort struct {
	p                  promise
	reason             engine.Value
	wasAlreadyErroring bool
}

// wsChunk is a queued write, or the close request (closing).
type wsChunk struct {
	v       engine.Value
	size    float64
	closing bool
}

// wsController is the state of a WritableStreamDefaultController.
type wsController struct {
	e      *env
	s      *writableStream
	obj    *engine.Object
	queue  []wsChunk
	total  float64
	hwm    float64
	size   engine.Value
	signal *abortSignal // created on first use of controller.signal
	// aborted and abortReason are the abort of a stream whose signal was
	// not created yet.
	abortReason engine.Value
	aborted     bool
	started     bool

	// The sink's algorithms; they return what to wait for. Nil once
	// cleared.
	write func(v engine.Value) (engine.Value, error)
	close func() (engine.Value, error)
	abort func(reason engine.Value) (engine.Value, error)
}

// wsWriter is the state of a WritableStreamDefaultWriter.
type wsWriter struct {
	obj    *engine.Object // nil for an internal writer
	s      *writableStream
	ready  promise
	closed promise
	// keep, under a memory limit, reports what the pipe an internal writer
	// serves keeps in its closures (env.reporter).
	keep *engine.Object
}

func (s *writableStream) locked() bool { return s.writer != nil }

func (s *writableStream) closeQueuedOrInFlight() bool {
	return s.closeRequest != nil || s.inFlightClose != nil
}

func (s *writableStream) hasOperationInFlight() bool {
	return s.inFlightWrite != nil || s.inFlightClose != nil
}

// --- creation -------------------------------------------------------------------

func (e *env) writableStreamSpec() classSpec {
	ws := func(r *engine.Realm, this engine.Value, name string) (*writableStream, error) {
		return thisAs[*writableStream](r, this, "WritableStream.prototype."+name)
	}
	return classSpec{
		name: "WritableStream",
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			s := &writableStream{stored: undefined}
			o, err := e.newObject(clsWritableStream, newTarget, s)
			if err != nil {
				return undefined, err
			}
			s.obj = o
			if err := e.setupFromSink(s, engine.Arg(args, 0), engine.Arg(args, 1)); err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "locked", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := ws(r, this, "locked")
				if err != nil {
					return undefined, err
				}
				return engine.Bool(s.locked()), nil
			}},
			{name: "abort", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := ws(r, this, "abort")
				if err != nil {
					return e.rejectedErr(err)
				}
				if s.locked() {
					return e.rejected(e.typeError("Cannot abort a locked WritableStream")), nil
				}
				return e.abortWritable(s, engine.Arg(args, 0))
			}},
			{name: "close", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := ws(r, this, "close")
				if err != nil {
					return e.rejectedErr(err)
				}
				if s.locked() {
					return e.rejected(e.typeError("Cannot close a locked WritableStream")), nil
				}
				if s.closeQueuedOrInFlight() {
					return e.rejected(e.typeError("Cannot close a WritableStream that is closing or closed")), nil
				}
				return e.closeWritable(s)
			}},
			{name: "getWriter", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := ws(r, this, "getWriter")
				if err != nil {
					return undefined, err
				}
				w, err := e.acquireWriter(s, true)
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(w.obj), nil
			}},
		},
	}
}

// setupFromSink sets up s for a JavaScript underlying sink and strategy.
func (e *env) setupFromSink(s *writableStream, sink, strategy engine.Value) error {
	r := e.r
	if !sink.IsNullish() && !sink.IsObject() {
		return r.TypeError("Failed to construct 'WritableStream': The provided value is not of type 'UnderlyingSink'.")
	}
	abortFn, err := e.method(sink, "abort")
	if err != nil {
		return err
	}
	closeFn, err := e.method(sink, "close")
	if err != nil {
		return err
	}
	startFn, err := e.method(sink, "start")
	if err != nil {
		return err
	}
	typ, err := e.getName(sink, "type")
	if err != nil {
		return err
	}
	if !typ.IsUndefined() {
		return r.RangeError("Invalid type is specified")
	}
	writeFn, err := e.method(sink, "write")
	if err != nil {
		return err
	}
	hwm, size, err := e.strategy(strategy, 1)
	if err != nil {
		return err
	}
	c := &wsController{e: e, s: s, hwm: hwm, size: size}
	s.ctrl = c
	s.held = e.held(sink, writeFn, closeFn, abortFn)
	ctrl := engine.ObjectValue(c.object())
	call := func(fn engine.Value, args ...engine.Value) (engine.Value, error) {
		if fn.IsUndefined() {
			return undefined, nil
		}
		v, err := r.Call(fn, sink, args)
		if err != nil {
			return e.rejectedErr(err)
		}
		return v, nil
	}
	c.write = func(v engine.Value) (engine.Value, error) { return call(writeFn, v, ctrl) }
	c.close = func() (engine.Value, error) { return call(closeFn) }
	c.abort = func(reason engine.Value) (engine.Value, error) { return call(abortFn, reason) }
	var start engine.Value = undefined
	if !startFn.IsUndefined() {
		if start, err = r.Call(startFn, sink, []engine.Value{ctrl}); err != nil {
			return err
		}
	}
	return e.startWritable(c, start)
}

// startWritable implements the last steps of
// SetUpWritableStreamDefaultController: start is what the start algorithm
// returned.
func (e *env) startWritable(c *wsController, start engine.Value) error {
	if err := e.updateBackpressure(c.s, c.backpressure()); err != nil {
		return err
	}
	return e.upon(start, func(engine.Value) error {
		c.started = true
		return e.advanceQueue(c)
	}, func(reason engine.Value) error {
		c.started = true
		return e.dealWithRejection(c.s, reason)
	})
}

func (c *wsController) object() *engine.Object {
	if c.obj == nil {
		c.obj, _ = c.e.newObject(clsWritableStreamDefaultController, nil, c)
	}
	return c.obj
}

func (c *wsController) desiredSize() float64 { return c.hwm - c.total }

func (c *wsController) backpressure() bool { return c.desiredSize() <= 0 }

func (c *wsController) clearAlgorithms() {
	c.write, c.close, c.abort, c.size = nil, nil, nil, undefined
	c.s.held = nil
}

// --- stream operations ----------------------------------------------------------

// abortWritable implements WritableStreamAbort.
func (e *env) abortWritable(s *writableStream, reason engine.Value) (engine.Value, error) {
	if s.state == wsClosed || s.state == wsErrored {
		return e.resolved(undefined)
	}
	if c := s.ctrl; c.signal != nil {
		if err := e.signalAbort(c.signal, reason); err != nil {
			return undefined, err
		}
	} else if !c.aborted {
		c.aborted, c.abortReason = true, reason
	}
	if s.state == wsClosed || s.state == wsErrored {
		return e.resolved(undefined)
	}
	if s.pendingAbort != nil {
		return s.pendingAbort.p.value(), nil
	}
	erroring := s.state == wsErroring
	if erroring {
		reason = undefined
	}
	s.pendingAbort = &pendingAbort{p: e.newPromise(), reason: reason, wasAlreadyErroring: erroring}
	p := s.pendingAbort.p
	if !erroring {
		if err := e.startErroring(s, reason); err != nil {
			return undefined, err
		}
	}
	return p.value(), nil
}

// closeWritable implements WritableStreamClose.
func (e *env) closeWritable(s *writableStream) (engine.Value, error) {
	if s.state == wsClosed || s.state == wsErrored {
		return e.rejected(e.typeError("The stream is closed or errored")), nil
	}
	p := e.newPromise()
	s.closeRequest = &p
	if w := s.writer; w != nil && s.backpressure && s.state == wsWritable {
		if err := e.fulfill(w.ready, undefined); err != nil {
			return undefined, err
		}
	}
	c := s.ctrl
	c.queue = append(c.queue, wsChunk{closing: true})
	if err := e.advanceQueue(c); err != nil {
		return undefined, err
	}
	return p.value(), nil
}

func (e *env) dealWithRejection(s *writableStream, reason engine.Value) error {
	if s.state == wsWritable {
		return e.startErroring(s, reason)
	}
	return e.finishErroring(s)
}

func (e *env) startErroring(s *writableStream, reason engine.Value) error {
	s.state, s.stored = wsErroring, reason
	if w := s.writer; w != nil {
		if err := e.ensureReadyRejected(w, reason); err != nil {
			return err
		}
	}
	if !s.hasOperationInFlight() && s.ctrl.started {
		return e.finishErroring(s)
	}
	return nil
}

func (e *env) finishErroring(s *writableStream) error {
	s.state = wsErrored
	c := s.ctrl
	clear(c.queue)
	c.queue, c.total = c.queue[:0], 0
	for _, p := range s.writeRequests {
		if err := e.rejectP(p, s.stored); err != nil {
			return err
		}
	}
	s.writeRequests = nil
	ab := s.pendingAbort
	if ab == nil {
		return e.rejectCloseAndClosed(s)
	}
	s.pendingAbort = nil
	if ab.wasAlreadyErroring {
		if err := e.rejectP(ab.p, s.stored); err != nil {
			return err
		}
		return e.rejectCloseAndClosed(s)
	}
	var res engine.Value = undefined
	if c.abort != nil {
		var err error
		if res, err = c.abort(ab.reason); err != nil {
			return err
		}
	}
	c.clearAlgorithms()
	return e.upon(res, func(engine.Value) error {
		if err := e.fulfill(ab.p, undefined); err != nil {
			return err
		}
		return e.rejectCloseAndClosed(s)
	}, func(reason engine.Value) error {
		if err := e.rejectP(ab.p, reason); err != nil {
			return err
		}
		return e.rejectCloseAndClosed(s)
	})
}

func (e *env) rejectCloseAndClosed(s *writableStream) error {
	if s.closeRequest != nil {
		if err := e.rejectP(*s.closeRequest, s.stored); err != nil {
			return err
		}
		s.closeRequest = nil
	}
	if w := s.writer; w != nil {
		if err := e.rejectP(w.closed, s.stored); err != nil {
			return err
		}
		e.markHandled(w.closed.value())
	}
	return nil
}

func (e *env) updateBackpressure(s *writableStream, bp bool) error {
	if w := s.writer; w != nil && bp != s.backpressure {
		if bp {
			w.ready = e.newPromise()
		} else if err := e.fulfill(w.ready, undefined); err != nil {
			return err
		}
	}
	s.backpressure = bp
	return nil
}

// advanceQueue implements WritableStreamDefaultControllerAdvanceQueueIfNeeded.
func (e *env) advanceQueue(c *wsController) error {
	s := c.s
	if !c.started || s.inFlightWrite != nil {
		return nil
	}
	if s.state == wsErroring {
		return e.finishErroring(s)
	}
	if len(c.queue) == 0 {
		return nil
	}
	if c.queue[0].closing {
		return e.processClose(c)
	}
	return e.processWrite(c, c.queue[0].v)
}

func (c *wsController) dequeue() {
	c.total -= c.queue[0].size
	if c.total < 0 {
		c.total = 0
	}
	c.queue[0] = wsChunk{}
	c.queue = c.queue[1:]
}

func (e *env) processClose(c *wsController) error {
	s := c.s
	s.inFlightClose, s.closeRequest = s.closeRequest, nil
	c.dequeue()
	var res engine.Value = undefined
	if c.close != nil {
		var err error
		if res, err = c.close(); err != nil {
			return err
		}
	}
	c.clearAlgorithms()
	return e.upon(res, func(engine.Value) error {
		if err := e.fulfill(*s.inFlightClose, undefined); err != nil {
			return err
		}
		s.inFlightClose = nil
		if s.state == wsErroring {
			s.stored = undefined
			if ab := s.pendingAbort; ab != nil {
				s.pendingAbort = nil
				if err := e.fulfill(ab.p, undefined); err != nil {
					return err
				}
			}
		}
		s.state = wsClosed
		if w := s.writer; w != nil {
			return e.fulfill(w.closed, undefined)
		}
		return nil
	}, func(reason engine.Value) error {
		if err := e.rejectP(*s.inFlightClose, reason); err != nil {
			return err
		}
		s.inFlightClose = nil
		if ab := s.pendingAbort; ab != nil {
			s.pendingAbort = nil
			if err := e.rejectP(ab.p, reason); err != nil {
				return err
			}
		}
		return e.dealWithRejection(s, reason)
	})
}

func (e *env) processWrite(c *wsController, v engine.Value) error {
	s := c.s
	p := s.writeRequests[0]
	s.writeRequests = s.writeRequests[1:]
	s.inFlightWrite = &p
	var res engine.Value = undefined
	if c.write != nil {
		var err error
		if res, err = c.write(v); err != nil {
			return err
		}
	}
	return e.upon(res, func(engine.Value) error {
		if err := e.fulfill(*s.inFlightWrite, undefined); err != nil {
			return err
		}
		s.inFlightWrite = nil
		c.dequeue()
		if !s.closeQueuedOrInFlight() && s.state == wsWritable {
			if err := e.updateBackpressure(s, c.backpressure()); err != nil {
				return err
			}
		}
		return e.advanceQueue(c)
	}, func(reason engine.Value) error {
		if s.state == wsWritable {
			c.clearAlgorithms()
		}
		if err := e.rejectP(*s.inFlightWrite, reason); err != nil {
			return err
		}
		s.inFlightWrite = nil
		return e.dealWithRejection(s, reason)
	})
}

// errorIfNeeded implements WritableStreamDefaultControllerErrorIfNeeded.
func (e *env) errorWritableIfNeeded(c *wsController, reason engine.Value) error {
	if c.s.state != wsWritable {
		return nil
	}
	c.clearAlgorithms()
	return e.startErroring(c.s, reason)
}

func (e *env) wsControllerSpec() classSpec {
	ctl := func(r *engine.Realm, this engine.Value, name string) (*wsController, error) {
		return thisAs[*wsController](r, this, "WritableStreamDefaultController.prototype."+name)
	}
	return classSpec{
		name: "WritableStreamDefaultController",
		members: []member{
			{name: "signal", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "signal")
				if err != nil {
					return undefined, err
				}
				if c.signal == nil {
					// Only an abort of the stream aborts the signal.
					c.signal = e.newSignal()
					if c.aborted {
						reason := c.abortReason
						if reason.IsUndefined() {
							reason = e.abortError()
						}
						c.signal.aborted, c.signal.reason = true, reason
						c.abortReason = undefined
					}
				}
				return engine.ObjectValue(c.signal.obj), nil
			}},
			{name: "error", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "error")
				if err != nil {
					return undefined, err
				}
				return undefined, e.errorWritableIfNeeded(c, engine.Arg(args, 0))
			}},
		},
	}
}

// --- writer ---------------------------------------------------------------------

// acquireWriter implements AcquireWritableStreamDefaultWriter.
func (e *env) acquireWriter(s *writableStream, withObject bool) (*wsWriter, error) {
	if s.locked() {
		return nil, e.r.TypeError("Cannot create writer when WritableStream is locked")
	}
	w := &wsWriter{s: s}
	if withObject {
		w.obj, _ = e.newObject(clsWritableStreamDefaultWriter, nil, w)
	}
	s.writer = w
	w.closed = e.newPromise()
	switch s.state {
	case wsWritable:
		w.ready = e.newPromise()
		if s.closeQueuedOrInFlight() || !s.backpressure {
			_ = e.fulfill(w.ready, undefined)
		}
	case wsErroring:
		w.ready = e.newPromise()
		_ = e.rejectP(w.ready, s.stored)
		e.markHandled(w.ready.value())
	case wsClosed:
		w.ready = e.newPromise()
		_ = e.fulfill(w.ready, undefined)
		_ = e.fulfill(w.closed, undefined)
	case wsErrored:
		w.ready = e.newPromise()
		_ = e.rejectP(w.ready, s.stored)
		e.markHandled(w.ready.value())
		_ = e.rejectP(w.closed, s.stored)
		e.markHandled(w.closed.value())
	}
	return w, nil
}

func (e *env) ensureReadyRejected(w *wsWriter, reason engine.Value) error {
	if w.ready.settled() {
		w.ready = e.newPromise()
	}
	if err := e.rejectP(w.ready, reason); err != nil {
		return err
	}
	e.markHandled(w.ready.value())
	return nil
}

func (e *env) ensureClosedRejected(w *wsWriter, reason engine.Value) error {
	if w.closed.settled() {
		w.closed = e.newPromise()
	}
	if err := e.rejectP(w.closed, reason); err != nil {
		return err
	}
	e.markHandled(w.closed.value())
	return nil
}

// releaseWriter implements WritableStreamDefaultWriterRelease.
func (e *env) releaseWriter(w *wsWriter) error {
	reason := e.typeError("Writer was released")
	if err := e.ensureReadyRejected(w, reason); err != nil {
		return err
	}
	if err := e.ensureClosedRejected(w, reason); err != nil {
		return err
	}
	w.s.writer, w.s = nil, nil
	return nil
}

// writerWrite implements WritableStreamDefaultWriterWrite.
func (e *env) writerWrite(w *wsWriter, v engine.Value) (engine.Value, error) {
	s := w.s
	c := s.ctrl
	size := 1.0
	if !c.size.IsUndefined() {
		sv, err := e.r.Call(c.size, undefined, []engine.Value{v})
		if err == nil {
			size, err = e.r.ToNumber(sv)
		}
		if err != nil {
			reason, ok := e.r.ThrownValue(err)
			if !ok {
				return undefined, err
			}
			if err := e.errorWritableIfNeeded(c, reason); err != nil {
				return undefined, err
			}
			size = 1
		}
		if w.s != s {
			return e.rejected(e.typeError("The writer was released while the chunk's size was computed")), nil
		}
	}
	switch {
	case s.state == wsErrored:
		return e.rejected(s.stored), nil
	case s.closeQueuedOrInFlight() || s.state == wsClosed:
		return e.rejected(e.typeError("The stream is closing or closed and cannot be written to")), nil
	case s.state == wsErroring:
		return e.rejected(s.stored), nil
	}
	p := e.newPromise()
	s.writeRequests = append(s.writeRequests, p)
	if size != size || size < 0 || size > 1.7976931348623157e308 {
		if err := e.errorWritableIfNeeded(c, e.rangeError("The return value of a queuing strategy's size function must be a finite, non-NaN, non-negative number")); err != nil {
			return undefined, err
		}
		return p.value(), nil
	}
	c.queue = append(c.queue, wsChunk{v: v, size: size})
	c.total += size
	if !s.closeQueuedOrInFlight() && s.state == wsWritable {
		if err := e.updateBackpressure(s, c.backpressure()); err != nil {
			return undefined, err
		}
	}
	return p.value(), e.advanceQueue(c)
}

// writerDesiredSize implements WritableStreamDefaultWriterGetDesiredSize.
func writerDesiredSize(s *writableStream) engine.Value {
	switch s.state {
	case wsErrored, wsErroring:
		return engine.Null()
	case wsClosed:
		return engine.Int64Value(0)
	}
	return engine.NumberValue(s.ctrl.desiredSize())
}

func (e *env) writerSpec() classSpec {
	writer := func(r *engine.Realm, this engine.Value, name string) (*wsWriter, error) {
		return thisAs[*wsWriter](r, this, "WritableStreamDefaultWriter.prototype."+name)
	}
	released := func(what string) engine.Value {
		return e.rejected(e.typeError("This writable stream writer has been released and cannot be used to " + what))
	}
	return classSpec{
		name: "WritableStreamDefaultWriter", length: 1,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			s, err := thisAs[*writableStream](r, engine.Arg(args, 0), "WritableStreamDefaultWriter")
			if err != nil {
				return undefined, r.TypeError("Failed to construct 'WritableStreamDefaultWriter': parameter 1 is not of type 'WritableStream'.")
			}
			// The object, which reads newTarget's prototype, is created
			// before the stream is locked.
			o, err := e.newObject(clsWritableStreamDefaultWriter, newTarget, nil)
			if err != nil {
				return undefined, err
			}
			w, err := e.acquireWriter(s, false)
			if err != nil {
				return undefined, err
			}
			w.obj = o
			o.SetInternal(w)
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "closed", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				w, err := writer(r, this, "closed")
				if err != nil {
					return e.rejectedErr(err)
				}
				return w.closed.value(), nil
			}},
			{name: "desiredSize", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				w, err := writer(r, this, "desiredSize")
				if err != nil {
					return undefined, err
				}
				if w.s == nil {
					return undefined, r.TypeError("This writable stream writer has been released and cannot be used to get the desired size")
				}
				return writerDesiredSize(w.s), nil
			}},
			{name: "ready", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				w, err := writer(r, this, "ready")
				if err != nil {
					return e.rejectedErr(err)
				}
				return w.ready.value(), nil
			}},
			{name: "abort", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				w, err := writer(r, this, "abort")
				if err != nil {
					return e.rejectedErr(err)
				}
				if w.s == nil {
					return released("abort"), nil
				}
				return e.abortWritable(w.s, engine.Arg(args, 0))
			}},
			{name: "close", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				w, err := writer(r, this, "close")
				if err != nil {
					return e.rejectedErr(err)
				}
				if w.s == nil {
					return released("close"), nil
				}
				if w.s.closeQueuedOrInFlight() {
					return e.rejected(e.typeError("Cannot close a WritableStream that is closing or closed")), nil
				}
				return e.closeWritable(w.s)
			}},
			{name: "releaseLock", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				w, err := writer(r, this, "releaseLock")
				if err != nil {
					return undefined, err
				}
				if w.s == nil {
					return undefined, nil
				}
				return undefined, e.releaseWriter(w)
			}},
			{name: "write", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				w, err := writer(r, this, "write")
				if err != nil {
					return e.rejectedErr(err)
				}
				if w.s == nil {
					return released("write to a stream"), nil
				}
				return e.writerWrite(w, engine.Arg(args, 0))
			}},
		},
	}
}

// --- pipes ----------------------------------------------------------------------

// pipeOptions are the options of pipeTo and pipeThrough.
type pipeOptions struct {
	preventClose, preventAbort, preventCancel bool
	signal                                    *abortSignal
}

func (e *env) readPipeOptions(v engine.Value) (pipeOptions, error) {
	var o pipeOptions
	if v.IsNullish() {
		return o, nil
	}
	if !v.IsObject() {
		return o, e.r.TypeError("The options of a pipe must be an object")
	}
	for _, f := range []struct {
		name string
		dst  *bool
	}{{"preventAbort", &o.preventAbort}, {"preventCancel", &o.preventCancel}, {"preventClose", &o.preventClose}} {
		x, err := e.getName(v, f.name)
		if err != nil {
			return o, err
		}
		*f.dst = engine.ToBoolean(x)
	}
	sv, err := e.get(v, e.keys.signal)
	if err != nil {
		return o, err
	}
	if !sv.IsUndefined() {
		s, err := thisAs[*abortSignal](e.r, sv, "pipeTo")
		if err != nil {
			return o, e.r.TypeError("Failed to execute 'pipeTo' on 'ReadableStream': member signal is not of type 'AbortSignal'.")
		}
		o.signal = s
	}
	return o, nil
}

// pipeToJS implements ReadableStream.prototype.pipeTo.
func (e *env) pipeToJS(s *readableStream, dest, options engine.Value) (engine.Value, error) {
	d, err := thisAs[*writableStream](e.r, dest, "pipeTo")
	if err != nil {
		return e.rejected(e.typeError("Failed to execute 'pipeTo' on 'ReadableStream': parameter 1 is not of type 'WritableStream'.")), nil
	}
	o, err := e.readPipeOptions(options)
	if err != nil {
		return e.rejectedErr(err)
	}
	if s.locked() {
		return e.rejected(e.typeError("Cannot pipe a locked stream")), nil
	}
	if d.locked() {
		return e.rejected(e.typeError("Cannot pipe to a locked stream")), nil
	}
	return e.pipe(s, d, o)
}

// pipeThrough implements ReadableStream.prototype.pipeThrough.
func (e *env) pipeThrough(s *readableStream, transform, options engine.Value) (engine.Value, error) {
	r := e.r
	wv, err := e.getName(transform, "writable")
	if err != nil {
		return undefined, err
	}
	w, err := thisAs[*writableStream](r, wv, "pipeThrough")
	if err != nil {
		return undefined, r.TypeError("Failed to execute 'pipeThrough' on 'ReadableStream': Failed to read the 'writable' property from 'ReadableWritablePair'.")
	}
	rv, err := e.getName(transform, "readable")
	if err != nil {
		return undefined, err
	}
	if _, err := thisAs[*readableStream](r, rv, "pipeThrough"); err != nil {
		return undefined, r.TypeError("Failed to execute 'pipeThrough' on 'ReadableStream': Failed to read the 'readable' property from 'ReadableWritablePair'.")
	}
	o, err := e.readPipeOptions(options)
	if err != nil {
		return undefined, err
	}
	if s.locked() {
		return undefined, r.TypeError("Cannot pipe a locked stream")
	}
	if w.locked() {
		return undefined, r.TypeError("Cannot pipe to a locked stream")
	}
	p, err := e.pipe(s, w, o)
	if err != nil {
		return undefined, err
	}
	e.markHandled(p)
	return rv, nil
}

// pipe implements ReadableStreamPipeTo.
func (e *env) pipe(src *readableStream, dest *writableStream, o pipeOptions) (engine.Value, error) {
	rd, err := e.acquireReader(src, false)
	if err != nil {
		return undefined, err
	}
	w, err := e.acquireWriter(dest, false)
	if err != nil {
		return undefined, err
	}
	src.disturbed = true
	p := e.newPromise()
	// What the pipe's closures keep, reported from its reader and writer,
	// the reactions it waits with and its abort algorithm.
	var keep engine.Value = undefined
	if rep := e.reporter(&pipeKeep{p, src, dest, o.signal}); rep != nil {
		rd.keep, w.keep = rep, rep
		keep = engine.ObjectValue(rep)
	}
	var (
		shuttingDown bool
		lastWrite    engine.Value = undefined // the promise of the last write
		removeAbort  func()
		stepping     bool // a step runs; a synchronous chunk loops in it
		again        bool
	)
	finalize := func(reason engine.Value, isErr bool) error {
		if err := e.releaseWriter(w); err != nil {
			return err
		}
		if rd.s != nil {
			if err := e.release(rd); err != nil {
				return err
			}
		}
		if removeAbort != nil {
			removeAbort()
		}
		if isErr {
			return e.rejectP(p, reason)
		}
		return e.fulfill(p, undefined)
	}
	// waitWrites runs f once the chunks read so far are written, when the
	// destination still takes them.
	waitWrites := func(f func() error) error {
		if dest.state == wsWritable && !dest.closeQueuedOrInFlight() && !lastWrite.IsUndefined() {
			done := func(engine.Value) error { return f() }
			return e.uponPromiseKeep(lastWrite, keep, done, done)
		}
		return f()
	}
	shutdownWith := func(action func() (engine.Value, error), reason engine.Value, isErr bool) error {
		if shuttingDown {
			return nil
		}
		shuttingDown = true
		return waitWrites(func() error {
			res, err := action()
			if err != nil {
				return err
			}
			return e.uponKeep(res, keep, func(engine.Value) error { return finalize(reason, isErr) },
				func(r engine.Value) error { return finalize(r, true) })
		})
	}
	shutdown := func(reason engine.Value, isErr bool) error {
		if shuttingDown {
			return nil
		}
		shuttingDown = true
		return waitWrites(func() error { return finalize(reason, isErr) })
	}
	// check propagates the states of the two streams, as the pipe's
	// conditions do; it reports whether the pipe shuts down.
	check := func() (bool, error) {
		if shuttingDown {
			return true, nil
		}
		switch {
		case src.state == rsErrored:
			reason := src.stored
			if !o.preventAbort {
				return true, shutdownWith(func() (engine.Value, error) { return e.abortWritable(dest, reason) }, reason, true)
			}
			return true, shutdown(reason, true)
		case dest.state == wsErrored:
			// An erroring destination becomes errored once its write in
			// flight settles; the pipe waits for that.
			reason := dest.stored
			if !o.preventCancel {
				return true, shutdownWith(func() (engine.Value, error) { return e.cancelStream(src, reason) }, reason, true)
			}
			return true, shutdown(reason, true)
		case src.state == rsClosed:
			if !o.preventClose {
				return true, shutdownWith(func() (engine.Value, error) { return e.closeWithErrorPropagation(w) }, undefined, false)
			}
			return true, shutdown(undefined, false)
		case dest.closeQueuedOrInFlight() || dest.state == wsClosed:
			reason := e.typeError("the destination writable stream closed before all data could be piped to it")
			if !o.preventCancel {
				return true, shutdownWith(func() (engine.Value, error) { return e.cancelStream(src, reason) }, reason, true)
			}
			return true, shutdown(reason, true)
		}
		return false, nil
	}
	var step func() error
	step = func() error {
		if stepping {
			again = true
			return nil
		}
		stepping = true
		defer func() { stepping = false }()
		for {
			again = false
			if done, err := check(); done || err != nil {
				return err
			}
			if st, _, _ := w.ready.p.PromiseResult(); st != engine.PromiseFulfilled {
				if st == engine.PromiseRejected {
					// The destination is erroring: the closed promise
					// reports when it errored.
					return nil
				}
				next := func(engine.Value) error { return step() }
				return e.uponPromiseKeep(w.ready.value(), keep, next, next)
			}
			err := e.read(rd, readRequest{
				onChunk: func(v engine.Value) error {
					wp, err := e.writerWrite(w, v)
					if err != nil {
						return err
					}
					e.markHandled(wp)
					lastWrite = wp
					return step()
				},
				onClose: func() error { _, err := check(); return err },
				onError: func(engine.Value) error { _, err := check(); return err },
			})
			if err != nil || !again {
				return err
			}
		}
	}
	// The closed promises report the state changes that happen while the
	// pipe waits.
	onState := func(engine.Value) error {
		if _, err := check(); err != nil {
			return err
		}
		return nil
	}
	if err := e.uponPromiseKeep(w.closed.value(), keep, onState, onState); err != nil {
		return undefined, err
	}
	if err := e.uponPromiseKeep(rd.closed.value(), keep, onState, onState); err != nil {
		return undefined, err
	}
	if o.signal != nil {
		abort := func(reason engine.Value) error {
			return shutdownWith(func() (engine.Value, error) {
				var actions []engine.Value
				if !o.preventAbort && dest.state == wsWritable {
					a, err := e.abortWritable(dest, reason)
					if err != nil {
						return undefined, err
					}
					actions = append(actions, a)
				}
				if !o.preventCancel && src.state == rsReadable {
					c, err := e.cancelStream(src, reason)
					if err != nil {
						return undefined, err
					}
					actions = append(actions, c)
				}
				return e.waitAll(actions)
			}, reason, true)
		}
		if o.signal.aborted {
			if err := abort(o.signal.reason); err != nil {
				return undefined, err
			}
			return p.value(), nil
		}
		removeAbort = o.signal.addAlgorithm(abort, keep)
	}
	if err := step(); err != nil {
		return undefined, err
	}
	return p.value(), nil
}

// pipeKeep reports what the closures of a pipe keep while it runs: its
// promise and its two streams.
type pipeKeep struct {
	p      promise
	src    *readableStream
	dest   *writableStream
	signal *abortSignal
}

func (k *pipeKeep) ReportLive(add func(engine.Value), _ func(int)) {
	if k.p.settled() {
		return
	}
	k.p.liveValues(add)
	addObject(add, k.src.obj)
	addObject(add, k.dest.obj)
	if k.signal != nil {
		addObject(add, k.signal.obj)
	}
}

// closeWithErrorPropagation implements
// WritableStreamDefaultWriterCloseWithErrorPropagation.
func (e *env) closeWithErrorPropagation(w *wsWriter) (engine.Value, error) {
	s := w.s
	if s.closeQueuedOrInFlight() || s.state == wsClosed {
		return e.resolved(undefined)
	}
	if s.state == wsErrored {
		return e.rejected(s.stored), nil
	}
	return e.closeWritable(s)
}

// waitAll returns a promise fulfilled when every promise of ps is, and
// rejected with the first rejection.
func (e *env) waitAll(ps []engine.Value) (engine.Value, error) {
	p := e.newPromise()
	n := len(ps)
	if n == 0 {
		return p.value(), e.fulfill(p, undefined)
	}
	for _, x := range ps {
		err := e.uponKeep(x, p.value(), func(engine.Value) error {
			n--
			if n == 0 {
				return e.fulfill(p, undefined)
			}
			return nil
		}, func(r engine.Value) error { return e.rejectP(p, r) })
		if err != nil {
			return undefined, err
		}
	}
	return p.value(), nil
}

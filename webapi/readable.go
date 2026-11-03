package webapi

import (
	"math"

	"github.com/MoeForge/moejs/engine"
)

// The Streams Standard's ReadableStream with its default controller and
// reader. A stream's controller runs the algorithms of its source: Go
// functions, which for a JavaScript underlying source call its methods.

type rsState uint8

const (
	rsReadable rsState = iota
	rsClosed
	rsErrored
)

// readableStream is the state of a ReadableStream.
type readableStream struct {
	obj    *engine.Object
	stored engine.Value // the stored error
	reader *rsReader
	ctrl   *rsController
	// held are the JavaScript values the source's algorithms call, under
	// a memory limit, until they are cleared: the algorithms are Go
	// closures, which the limit's measurements do not see into.
	held *[4]engine.Value
	// src reports what a source implemented in Go keeps outside the
	// JavaScript heap, or is nil.
	src       engine.LiveReporter
	state     rsState
	disturbed bool
}

func (s *readableStream) locked() bool { return s.reader != nil }

// chunk is a queued chunk and its size.
type chunk struct {
	v    engine.Value
	size float64
}

// rsController is the state of a ReadableStreamDefaultController.
type rsController struct {
	e     *env
	s     *readableStream
	obj   *engine.Object // the JavaScript controller, nil until needed
	queue []chunk
	head  int
	total float64 // the queue's total size
	hwm   float64
	size  engine.Value // the strategy's size function, or undefined
	bytes bool         // a byte stream: chunks are ArrayBufferViews

	started, closeRequested, pulling, pullAgain bool
	// inPull is set while the pull algorithm runs, pulledSync when it
	// completed before returning.
	inPull, pulledSync bool

	// pull starts pulling: it calls pulled when done, or errors the
	// stream. cancel cancels the source and returns what to wait for (a
	// promise or any value). Nil once cleared.
	pull   func() error
	cancel func(reason engine.Value) (engine.Value, error)
}

// readRequest is a pending read of a default reader: a promise for
// reader.read(), or the steps of an internal reader.
type readRequest struct {
	p       promise
	onChunk func(engine.Value) error
	onClose func() error
	onError func(engine.Value) error
}

// rsReader is the state of a ReadableStreamDefaultReader.
type rsReader struct {
	obj    *engine.Object // nil for an internal reader
	s      *readableStream
	closed promise
	reqs   []readRequest
	head   int
	// keep, under a memory limit, reports what the operation an internal
	// reader serves keeps in its closures: a pipe, a tee, a whole read
	// (env.reporter).
	keep *engine.Object
}

// --- creation -------------------------------------------------------------------

// newReadable creates a ReadableStream whose controller has the given
// high-water mark and algorithms, started at once. The stream object is
// created for the class's prototype.
func (e *env) newReadable(hwm float64, pull func(c *rsController) error, cancel func(reason engine.Value) (engine.Value, error)) *readableStream {
	s := &readableStream{stored: undefined}
	s.obj, _ = e.newObject(clsReadableStream, nil, s)
	c := &rsController{e: e, s: s, hwm: hwm, size: undefined, started: true}
	s.ctrl = c
	if pull != nil {
		c.pull = func() error { return pull(c) }
	} else {
		c.pull = func() error { return c.pulled() }
	}
	c.cancel = cancel
	return s
}

// controllerObject returns the JavaScript object of the controller.
func (c *rsController) object() *engine.Object {
	if c.obj == nil {
		c.obj, _ = c.e.newObject(clsReadableStreamDefaultController, nil, c)
	}
	return c.obj
}

func (e *env) readableStreamSpec() classSpec {
	rs := func(r *engine.Realm, this engine.Value, name string) (*readableStream, error) {
		return thisAs[*readableStream](r, this, "ReadableStream.prototype."+name)
	}
	return classSpec{
		name: "ReadableStream", length: 0,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			s := &readableStream{stored: undefined}
			o, err := e.newObject(clsReadableStream, newTarget, s)
			if err != nil {
				return undefined, err
			}
			s.obj = o
			if err := e.setupFromSource(s, engine.Arg(args, 0), engine.Arg(args, 1)); err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "locked", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := rs(r, this, "locked")
				if err != nil {
					return undefined, err
				}
				return engine.Bool(s.locked()), nil
			}},
			{name: "cancel", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := rs(r, this, "cancel")
				if err != nil {
					return e.rejectedErr(err)
				}
				if s.locked() {
					return e.rejected(e.typeError("Cannot cancel a locked ReadableStream")), nil
				}
				return e.cancelStream(s, engine.Arg(args, 0))
			}},
			{name: "getReader", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := rs(r, this, "getReader")
				if err != nil {
					return undefined, err
				}
				if opts := engine.Arg(args, 0); !opts.IsNullish() {
					mode, err := e.getName(opts, "mode")
					if err != nil {
						return undefined, err
					}
					if !mode.IsUndefined() {
						m, err := e.goString(mode)
						if err != nil {
							return undefined, err
						}
						if m != "byob" {
							return undefined, r.TypeError("Failed to execute 'getReader' on 'ReadableStream': The provided value '%s' is not a valid enum value of type ReadableStreamReaderMode.", m)
						}
						return undefined, r.TypeError("ReadableStream.getReader: BYOB readers are not supported")
					}
				}
				rd, err := e.acquireReader(s, true)
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(rd.obj), nil
			}},
			{name: "pipeThrough", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := rs(r, this, "pipeThrough")
				if err != nil {
					return undefined, err
				}
				return e.pipeThrough(s, engine.Arg(args, 0), engine.Arg(args, 1))
			}},
			{name: "pipeTo", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := rs(r, this, "pipeTo")
				if err != nil {
					return e.rejectedErr(err)
				}
				return e.pipeToJS(s, engine.Arg(args, 0), engine.Arg(args, 1))
			}},
			{name: "tee", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := rs(r, this, "tee")
				if err != nil {
					return undefined, err
				}
				b1, b2, err := e.tee(s)
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(r.NewArrayFromSlice([]engine.Value{engine.ObjectValue(b1.obj), engine.ObjectValue(b2.obj)})), nil
			}},
			{name: "values", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := rs(r, this, "values")
				if err != nil {
					return undefined, err
				}
				return e.asyncIterator(s, engine.Arg(args, 0))
			}},
			{sym: engine.SymAsyncIterator, name: "[Symbol.asyncIterator]", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := rs(r, this, "[Symbol.asyncIterator]")
				if err != nil {
					return undefined, err
				}
				return e.asyncIterator(s, engine.Arg(args, 0))
			}},
		},
		statics: []member{
			{name: "from", length: 1, fn: func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := e.readableFrom(engine.Arg(args, 0))
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(s.obj), nil
			}},
		},
	}
}

// strategy reads a queuing strategy: its high-water mark (def when absent)
// and size function.
func (e *env) strategy(v engine.Value, def float64) (hwm float64, size engine.Value, err error) {
	size, hwm = undefined, def
	if v.IsNullish() {
		return
	}
	h, err := e.getName(v, "highWaterMark")
	if err != nil {
		return
	}
	if !h.IsUndefined() {
		if hwm, err = e.r.ToNumber(h); err != nil {
			return
		}
		if math.IsNaN(hwm) || hwm < 0 {
			return 0, size, e.r.RangeError("Invalid highWaterMark")
		}
	}
	if size, err = e.getName(v, "size"); err != nil {
		return
	}
	if !size.IsUndefined() && !engine.IsCallable(size) {
		return 0, undefined, e.r.TypeError("The size of a queuing strategy must be a function")
	}
	return
}

// setupFromSource sets up the stream s for a JavaScript underlying source
// and strategy.
func (e *env) setupFromSource(s *readableStream, src, strategy engine.Value) error {
	r := e.r
	if !src.IsNullish() && !src.IsObject() {
		return r.TypeError("Failed to construct 'ReadableStream': The provided value is not of type 'UnderlyingSource'.")
	}
	cancelFn, err := e.method(src, "cancel")
	if err != nil {
		return err
	}
	pullFn, err := e.method(src, "pull")
	if err != nil {
		return err
	}
	startFn, err := e.method(src, "start")
	if err != nil {
		return err
	}
	typ, err := e.getName(src, "type")
	if err != nil {
		return err
	}
	bytes := false
	if !typ.IsUndefined() {
		t, err := e.goString(typ)
		if err != nil {
			return err
		}
		if t != "bytes" {
			return r.TypeError("Failed to construct 'ReadableStream': The provided value '%s' is not a valid enum value of type ReadableStreamType.", t)
		}
		bytes = true
	}
	def := 1.0
	if bytes {
		def = 0
	}
	hwm, size, err := e.strategy(strategy, def)
	if err != nil {
		return err
	}
	if bytes && !size.IsUndefined() {
		return r.RangeError("The strategy of a byte stream cannot have a size function")
	}
	c := &rsController{e: e, s: s, hwm: hwm, size: size, bytes: bytes}
	s.ctrl = c
	s.held = e.held(src, pullFn, cancelFn, undefined)
	ctrl := engine.ObjectValue(c.object())
	c.pull = func() error {
		if pullFn.IsUndefined() {
			return c.pulled()
		}
		v, err := r.Call(pullFn, src, []engine.Value{ctrl})
		if err != nil {
			if v, err = e.rejectedErr(err); err != nil {
				return err
			}
		}
		// A pull that returned no promise is done now. moejs does not
		// wait the microtask the specification's promise takes.
		if !v.IsObject() {
			return c.pulled()
		}
		return e.upon(v, func(engine.Value) error { return c.pulled() }, c.errorWith)
	}
	c.cancel = func(reason engine.Value) (engine.Value, error) {
		if cancelFn.IsUndefined() {
			return undefined, nil
		}
		v, err := r.Call(cancelFn, src, []engine.Value{reason})
		if err != nil {
			return e.rejectedErr(err)
		}
		return v, nil
	}
	var start engine.Value = undefined
	if !startFn.IsUndefined() {
		if start, err = r.Call(startFn, src, []engine.Value{ctrl}); err != nil {
			return err
		}
	}
	return e.upon(start, func(engine.Value) error {
		c.started = true
		return c.callPullIfNeeded()
	}, c.errorWith)
}

// --- controller -----------------------------------------------------------------

func (c *rsController) desiredSize() (float64, bool) {
	switch c.s.state {
	case rsErrored:
		return 0, false
	case rsClosed:
		return 0, true
	}
	return c.hwm - c.total, true
}

func (c *rsController) canCloseOrEnqueue() bool {
	return !c.closeRequested && c.s.state == rsReadable
}

func (c *rsController) shouldCallPull() bool {
	if !c.canCloseOrEnqueue() || !c.started {
		return false
	}
	if rd := c.s.reader; rd != nil && rd.pending() > 0 {
		return true
	}
	d, _ := c.desiredSize()
	return d > 0
}

// callPullIfNeeded implements ReadableStreamDefaultControllerCallPullIfNeeded.
// A pull that completes before it returns (a source whose pull returned no
// promise) loops here instead of recursing, so that a source that enqueues
// synchronously fills the queue up to its high-water mark without growing
// the stack.
func (c *rsController) callPullIfNeeded() error {
	for {
		if !c.shouldCallPull() || c.pull == nil {
			return nil
		}
		if c.pulling {
			c.pullAgain = true
			return nil
		}
		c.pulling, c.inPull, c.pulledSync = true, true, false
		err := c.pull()
		c.inPull = false
		if err != nil || !c.pulledSync {
			return err
		}
		c.pulling = false
		if !c.pullAgain {
			return nil
		}
		c.pullAgain = false
	}
}

// pulled ends a pull: the source's pull promise fulfilled. Called while
// the pull runs, it lets callPullIfNeeded go on.
func (c *rsController) pulled() error {
	if c.inPull {
		c.pulledSync = true
		return nil
	}
	c.pulling = false
	if c.pullAgain {
		c.pullAgain = false
		return c.callPullIfNeeded()
	}
	return nil
}

func (c *rsController) clearAlgorithms() {
	c.pull, c.cancel, c.size = nil, nil, undefined
	c.s.held = nil
}

// enqueue implements ReadableStreamDefaultControllerEnqueue for a chunk
// that passed canCloseOrEnqueue.
func (c *rsController) enqueue(v engine.Value) error {
	s := c.s
	if rd := s.reader; rd != nil && rd.pending() > 0 {
		if err := rd.fulfill(v); err != nil {
			return err
		}
	} else {
		size := 1.0
		if !c.size.IsUndefined() {
			sv, err := c.e.r.Call(c.size, undefined, []engine.Value{v})
			if err == nil {
				size, err = c.e.r.ToNumber(sv)
			}
			if err != nil {
				return c.failWith(err)
			}
		} else if c.bytes {
			size = viewLength(v)
		}
		if math.IsNaN(size) || size < 0 || math.IsInf(size, 1) {
			return c.failWith(c.e.r.RangeError("The return value of a queuing strategy's size function must be a finite, non-NaN, non-negative number"))
		}
		c.queue = append(c.queue, chunk{v, size})
		c.total += size
	}
	return c.callPullIfNeeded()
}

// failWith errors the stream with what err throws and returns err.
func (c *rsController) failWith(err error) error {
	reason, ok := c.e.r.ThrownValue(err)
	if !ok {
		return err
	}
	if err := c.errorWith(reason); err != nil {
		return err
	}
	return err
}

// errorFrom errors the stream with what err throws, for a reaction, which
// returns only an interrupt.
func (c *rsController) errorFrom(err error) error {
	reason, ok := c.e.r.ThrownValue(err)
	if !ok {
		return err
	}
	return c.errorWith(reason)
}

// dequeue removes the first chunk. A queue that never empties moves its
// chunks back to the front once half of it is dequeued, so that it does
// not keep growing.
func (c *rsController) dequeue() engine.Value {
	ch := c.queue[c.head]
	c.queue[c.head] = chunk{}
	c.head++
	if c.head == len(c.queue) {
		c.queue, c.head = c.queue[:0], 0
	} else if c.head >= 16 && 2*c.head >= len(c.queue) {
		n := copy(c.queue, c.queue[c.head:])
		clear(c.queue[n:])
		c.queue, c.head = c.queue[:n], 0
	}
	c.total -= ch.size
	if c.total < 0 {
		c.total = 0
	}
	return ch.v
}

func (c *rsController) queued() int { return len(c.queue) - c.head }

func (c *rsController) resetQueue() {
	clear(c.queue)
	c.queue, c.head, c.total = c.queue[:0], 0, 0
}

// close implements ReadableStreamDefaultControllerClose.
func (c *rsController) close() error {
	if !c.canCloseOrEnqueue() {
		return nil
	}
	c.closeRequested = true
	if c.queued() == 0 {
		c.clearAlgorithms()
		return c.e.closeStream(c.s)
	}
	return nil
}

// errorWith implements ReadableStreamDefaultControllerError.
func (c *rsController) errorWith(reason engine.Value) error {
	if c.s.state != rsReadable {
		return nil
	}
	c.resetQueue()
	c.clearAlgorithms()
	return c.e.errorStream(c.s, reason)
}

// pullSteps implements [[PullSteps]] for the read request req.
func (c *rsController) pullSteps(req readRequest) error {
	s := c.s
	if c.queued() > 0 {
		v := c.dequeue()
		if c.closeRequested && c.queued() == 0 {
			c.clearAlgorithms()
			if err := c.e.closeStream(s); err != nil {
				return err
			}
		} else if err := c.callPullIfNeeded(); err != nil {
			return err
		}
		return c.e.chunkSteps(req, v)
	}
	s.reader.add(req)
	return c.callPullIfNeeded()
}

func (e *env) rsControllerSpec() classSpec {
	ctl := func(r *engine.Realm, this engine.Value, name string) (*rsController, error) {
		return thisAs[*rsController](r, this, "ReadableStreamDefaultController.prototype."+name)
	}
	return classSpec{
		name: "ReadableStreamDefaultController",
		members: []member{
			{name: "desiredSize", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "desiredSize")
				if err != nil {
					return undefined, err
				}
				d, ok := c.desiredSize()
				if !ok {
					return engine.Null(), nil
				}
				return engine.NumberValue(d), nil
			}},
			{name: "close", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "close")
				if err != nil {
					return undefined, err
				}
				if !c.canCloseOrEnqueue() {
					return undefined, r.TypeError("Invalid state: Controller is already closed")
				}
				return undefined, c.close()
			}},
			{name: "enqueue", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "enqueue")
				if err != nil {
					return undefined, err
				}
				if !c.canCloseOrEnqueue() {
					return undefined, r.TypeError("Invalid state: Controller is already closed")
				}
				v := engine.Arg(args, 0)
				if c.bytes {
					if _, ok := viewBytes(v); !ok {
						return undefined, r.TypeError("The chunk of a byte stream must be an ArrayBufferView")
					}
				}
				return undefined, c.enqueue(v)
			}},
			{name: "error", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "error")
				if err != nil {
					return undefined, err
				}
				return undefined, c.errorWith(engine.Arg(args, 0))
			}},
			{name: "byobRequest", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				if _, err := ctl(r, this, "byobRequest"); err != nil {
					return undefined, err
				}
				return engine.Null(), nil
			}},
		},
	}
}

// --- stream operations ----------------------------------------------------------

// closeStream implements ReadableStreamClose.
func (e *env) closeStream(s *readableStream) error {
	s.state = rsClosed
	rd := s.reader
	if rd == nil {
		return nil
	}
	if err := e.fulfill(rd.closed, undefined); err != nil {
		return err
	}
	for rd.pending() > 0 {
		req := rd.take()
		var err error
		if req.onClose != nil {
			err = req.onClose()
		} else {
			err = e.fulfill(req.p, e.iterResult(undefined, true))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// errorStream implements ReadableStreamError.
func (e *env) errorStream(s *readableStream, reason engine.Value) error {
	s.state, s.stored = rsErrored, reason
	rd := s.reader
	if rd == nil {
		return nil
	}
	if err := e.rejectP(rd.closed, reason); err != nil {
		return err
	}
	e.markHandled(rd.closed.value())
	return rd.errorRequests(e, reason)
}

// cancelStream implements ReadableStreamCancel.
func (e *env) cancelStream(s *readableStream, reason engine.Value) (engine.Value, error) {
	s.disturbed = true
	switch s.state {
	case rsClosed:
		return e.resolved(undefined)
	case rsErrored:
		return e.rejected(s.stored), nil
	}
	if err := e.closeStream(s); err != nil {
		return undefined, err
	}
	c := s.ctrl
	c.resetQueue()
	cancel := c.cancel
	c.clearAlgorithms()
	var res engine.Value = undefined
	if cancel != nil {
		var err error
		if res, err = cancel(reason); err != nil {
			return undefined, err
		}
	}
	p := e.newPromise()
	err := e.uponKeep(res, p.value(), func(engine.Value) error { return e.fulfill(p, undefined) },
		func(r engine.Value) error { return e.rejectP(p, r) })
	return p.value(), err
}

// chunkSteps runs the chunk steps of req.
func (e *env) chunkSteps(req readRequest, v engine.Value) error {
	if req.onChunk != nil {
		return req.onChunk(v)
	}
	return e.fulfill(req.p, e.iterResult(v, false))
}

// --- reader ---------------------------------------------------------------------

func (rd *rsReader) pending() int { return len(rd.reqs) - rd.head }

func (rd *rsReader) add(req readRequest) { rd.reqs = append(rd.reqs, req) }

// take removes the first read request, compacting as dequeue does.
func (rd *rsReader) take() readRequest {
	req := rd.reqs[rd.head]
	rd.reqs[rd.head] = readRequest{}
	rd.head++
	if rd.head == len(rd.reqs) {
		rd.reqs, rd.head = rd.reqs[:0], 0
	} else if rd.head >= 16 && 2*rd.head >= len(rd.reqs) {
		n := copy(rd.reqs, rd.reqs[rd.head:])
		clear(rd.reqs[n:])
		rd.reqs, rd.head = rd.reqs[:n], 0
	}
	return req
}

// fulfill implements ReadableStreamFulfillReadRequest for a chunk.
func (rd *rsReader) fulfill(v engine.Value) error {
	return rd.s.ctrl.e.chunkSteps(rd.take(), v)
}

// errorRequests rejects the pending read requests with reason.
func (rd *rsReader) errorRequests(e *env, reason engine.Value) error {
	for rd.pending() > 0 {
		req := rd.take()
		var err error
		if req.onError != nil {
			err = req.onError(reason)
		} else {
			err = e.rejectP(req.p, reason)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// acquireReader implements AcquireReadableStreamDefaultReader; withObject
// creates the reader's JavaScript object.
func (e *env) acquireReader(s *readableStream, withObject bool) (*rsReader, error) {
	if s.locked() {
		return nil, e.r.TypeError("ReadableStreamDefaultReader constructor can only accept readable streams that are not yet locked to a reader")
	}
	rd := &rsReader{s: s, closed: e.newPromise()}
	if withObject {
		rd.obj, _ = e.newObject(clsReadableStreamDefaultReader, nil, rd)
	}
	s.reader = rd
	switch s.state {
	case rsClosed:
		_ = e.fulfill(rd.closed, undefined)
	case rsErrored:
		_ = e.rejectP(rd.closed, s.stored)
		e.markHandled(rd.closed.value())
	}
	return rd, nil
}

// read implements ReadableStreamDefaultReaderRead.
func (e *env) read(rd *rsReader, req readRequest) error {
	s := rd.s
	s.disturbed = true
	switch s.state {
	case rsClosed:
		if req.onClose != nil {
			return req.onClose()
		}
		return e.fulfill(req.p, e.iterResult(undefined, true))
	case rsErrored:
		if req.onError != nil {
			return req.onError(s.stored)
		}
		return e.rejectP(req.p, s.stored)
	}
	return s.ctrl.pullSteps(req)
}

// release implements ReadableStreamDefaultReaderRelease; a reader
// released already is left alone.
func (e *env) release(rd *rsReader) error {
	s := rd.s
	if s == nil {
		return nil
	}
	err := e.typeError("Reader was released")
	if s.state == rsReadable {
		if e := e.rejectP(rd.closed, err); e != nil {
			return e
		}
	} else {
		rd.closed = e.newPromise()
		if e := e.rejectP(rd.closed, err); e != nil {
			return e
		}
	}
	e.markHandled(rd.closed.value())
	s.reader, rd.s = nil, nil
	return rd.errorRequests(e, e.typeError("Reader was released"))
}

func (e *env) readerSpec() classSpec {
	reader := func(r *engine.Realm, this engine.Value, name string) (*rsReader, error) {
		return thisAs[*rsReader](r, this, "ReadableStreamDefaultReader.prototype."+name)
	}
	return classSpec{
		name: "ReadableStreamDefaultReader", length: 1,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			s, err := thisAs[*readableStream](r, engine.Arg(args, 0), "ReadableStreamDefaultReader")
			if err != nil {
				return undefined, r.TypeError("Failed to construct 'ReadableStreamDefaultReader': parameter 1 is not of type 'ReadableStream'.")
			}
			// The object, which reads newTarget's prototype, is created
			// before the stream is locked.
			o, err := e.newObject(clsReadableStreamDefaultReader, newTarget, nil)
			if err != nil {
				return undefined, err
			}
			rd, err := e.acquireReader(s, false)
			if err != nil {
				return undefined, err
			}
			rd.obj = o
			o.SetInternal(rd)
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "closed", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				rd, err := reader(r, this, "closed")
				if err != nil {
					return e.rejectedErr(err)
				}
				return rd.closed.value(), nil
			}},
			{name: "read", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				rd, err := reader(r, this, "read")
				if err != nil {
					return e.rejectedErr(err)
				}
				if rd.s == nil {
					return e.rejected(e.typeError("This readable stream reader has been released and cannot be used to read from its previous owner stream")), nil
				}
				// A chunk already queued resolves a promise created
				// fulfilled, without resolving functions. The pull it
				// starts may release the reader.
				if s := rd.s; s.state == rsReadable && s.ctrl.queued() > 0 {
					s.disturbed = true
					var v engine.Value
					if err := s.ctrl.pullSteps(readRequest{onChunk: func(x engine.Value) error { v = x; return nil }}); err != nil {
						return undefined, err
					}
					return e.resolved(e.iterResult(v, false))
				}
				req := readRequest{p: e.newPromise()}
				if err := e.read(rd, req); err != nil {
					return undefined, err
				}
				return req.p.value(), nil
			}},
			{name: "releaseLock", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				rd, err := reader(r, this, "releaseLock")
				if err != nil {
					return undefined, err
				}
				if rd.s == nil {
					return undefined, nil
				}
				return undefined, e.release(rd)
			}},
			{name: "cancel", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				rd, err := reader(r, this, "cancel")
				if err != nil {
					return e.rejectedErr(err)
				}
				if rd.s == nil {
					return e.rejected(e.typeError("This readable stream reader has been released and cannot be used to cancel its previous owner stream")), nil
				}
				return e.cancelStream(rd.s, engine.Arg(args, 0))
			}},
		},
	}
}

// --- async iteration ------------------------------------------------------------

// rsIterator is the state of a ReadableStream async iterator. ongoing is
// the promise of the last next() or return() (Web IDL's ongoing promise):
// each call waits for the one before it to settle, so that a stream is
// read, released and canceled in the order of the calls.
type rsIterator struct {
	rd            *rsReader
	preventCancel bool
	finished      bool
	ongoing       promise // p is nil while there is none
}

func (e *env) asyncIterator(s *readableStream, opts engine.Value) (engine.Value, error) {
	it := &rsIterator{}
	if !opts.IsNullish() {
		v, err := e.getName(opts, "preventCancel")
		if err != nil {
			return undefined, err
		}
		it.preventCancel = engine.ToBoolean(v)
	}
	rd, err := e.acquireReader(s, false)
	if err != nil {
		return undefined, err
	}
	it.rd = rd
	o, _ := e.newObject(clsReadableStreamAsyncIterator, nil, it)
	return engine.ObjectValue(o), nil
}

// chain runs steps once the ongoing promise of it settles, or at once
// when there is none, and makes its result the ongoing promise.
func (e *env) chain(it *rsIterator, steps func() (promise, error)) (promise, error) {
	prev := it.ongoing
	if prev.p == nil || prev.settled() {
		p, err := steps()
		if err == nil {
			it.ongoing = p
		}
		return p, err
	}
	after := e.newPromise()
	it.ongoing = after
	run := func(engine.Value) error {
		p, err := steps()
		if err != nil {
			if reason, ok := e.r.ThrownValue(err); ok {
				return e.rejectP(after, reason)
			}
			return err
		}
		return e.fulfill(after, p.value())
	}
	return after, e.uponPromise(prev.value(), run, run)
}

// iterNext implements the next steps of the async iterator: a promise of
// the next iteration result.
func (e *env) iterNext(it *rsIterator) (promise, error) {
	p := e.newPromise()
	rd := it.rd
	if it.finished || rd.s == nil {
		return p, e.fulfill(p, e.iterResult(undefined, true))
	}
	err := e.read(rd, readRequest{
		p:       p, // for the measurements of the memory limit
		onChunk: func(v engine.Value) error { return e.fulfill(p, e.iterResult(v, false)) },
		onClose: func() error {
			it.finished = true
			if err := e.release(rd); err != nil {
				return err
			}
			return e.fulfill(p, e.iterResult(undefined, true))
		},
		onError: func(reason engine.Value) error {
			it.finished = true
			if err := e.release(rd); err != nil {
				return err
			}
			return e.rejectP(p, reason)
		},
	})
	return p, err
}

// iterReturn implements the return steps of the async iterator for value.
func (e *env) iterReturn(it *rsIterator, value engine.Value) (promise, error) {
	p := e.newPromise()
	rd := it.rd
	if it.finished || rd.s == nil {
		return p, e.fulfill(p, e.iterResult(value, true))
	}
	it.finished = true
	if it.preventCancel {
		if err := e.release(rd); err != nil {
			return p, err
		}
		return p, e.fulfill(p, e.iterResult(value, true))
	}
	res, err := e.cancelStream(rd.s, value)
	if err != nil {
		return p, err
	}
	if err := e.release(rd); err != nil {
		return p, err
	}
	err = e.uponPromiseKeep(res, p.value(), func(engine.Value) error { return e.fulfill(p, e.iterResult(value, true)) },
		func(reason engine.Value) error { return e.rejectP(p, reason) })
	return p, err
}

func (e *env) rsAsyncIteratorSpec() classSpec {
	iter := func(r *engine.Realm, this engine.Value, name string) (*rsIterator, error) {
		return thisAs[*rsIterator](r, this, "ReadableStream async iterator "+name)
	}
	return classSpec{
		name: "ReadableStream AsyncIterator", noCtor: true,
		members: []member{
			{name: "next", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				it, err := iter(r, this, "next")
				if err != nil {
					return e.rejectedErr(err)
				}
				p, err := e.chain(it, func() (promise, error) { return e.iterNext(it) })
				return p.value(), err
			}},
			{name: "return", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				it, err := iter(r, this, "return")
				if err != nil {
					return e.rejectedErr(err)
				}
				v := engine.Arg(args, 0)
				p, err := e.chain(it, func() (promise, error) { return e.iterReturn(it, v) })
				return p.value(), err
			}},
			{sym: engine.SymAsyncIterator, name: "[Symbol.asyncIterator]", fn: func(_ *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				return this, nil
			}},
		},
	}
}

// --- tee ------------------------------------------------------------------------

// tee implements ReadableStreamDefaultTee: both branches get the same
// chunks, but for a stream of bytes, whose second branch gets copies, as
// ReadableByteStreamTee gives it.
func (e *env) tee(s *readableStream) (*readableStream, *readableStream, error) {
	rd, err := e.acquireReader(s, false)
	if err != nil {
		return nil, nil, err
	}
	var (
		reading, readAgain   bool
		canceled1, canceled2 bool
		reason1, reason2     engine.Value
		b1, b2               *readableStream
		cancelP              = e.newPromise()
	)
	var pull func(*rsController) error
	pull = func(c *rsController) error {
		if err := c.pulled(); err != nil {
			return err
		}
		if reading {
			readAgain = true
			return nil
		}
		reading = true
		return e.read(rd, readRequest{
			onChunk: func(v engine.Value) error {
				readAgain = false
				if !canceled1 {
					if err := b1.ctrl.enqueueIfOpen(v); err != nil {
						return err
					}
				}
				if !canceled2 {
					if s.ctrl.bytes && !canceled1 {
						var err error
						if v, err = e.cloneView(v); err != nil {
							return err
						}
					}
					if err := b2.ctrl.enqueueIfOpen(v); err != nil {
						return err
					}
				}
				reading = false
				if readAgain {
					readAgain = false
					return pull(b1.ctrl)
				}
				return nil
			},
			onClose: func() error {
				reading = false
				if !canceled1 {
					if err := b1.ctrl.close(); err != nil {
						return err
					}
				}
				if !canceled2 {
					if err := b2.ctrl.close(); err != nil {
						return err
					}
				}
				if !canceled1 || !canceled2 {
					return e.fulfill(cancelP, undefined)
				}
				return nil
			},
			onError: func(engine.Value) error { reading = false; return nil },
		})
	}
	cancel := func(first bool) func(engine.Value) (engine.Value, error) {
		return func(reason engine.Value) (engine.Value, error) {
			other := b1
			if first {
				canceled1, reason1 = true, reason
				other = b2
			} else {
				canceled2, reason2 = true, reason
			}
			if other.ctrl.cancel != nil && other.held != nil {
				// The other branch's cancel keeps the reason.
				other.held[0] = reason
			}
			if canceled1 && canceled2 {
				composite := engine.ObjectValue(e.r.NewArrayFromSlice([]engine.Value{reason1, reason2}))
				res, err := e.cancelStream(s, composite)
				if err != nil {
					return undefined, err
				}
				if err := e.fulfill(cancelP, res); err != nil {
					return undefined, err
				}
			}
			return cancelP.value(), nil
		}
	}
	b1 = e.newReadable(1, pull, cancel(true))
	b2 = e.newReadable(1, pull, cancel(false))
	b1.ctrl.bytes, b2.ctrl.bytes = s.ctrl.bytes, s.ctrl.bytes
	if rep := e.reporter(&teeKeep{b1, b2, cancelP}); rep != nil {
		// What the closures of the tee keep: the branches keep the
		// source, each other and the cancel promise, with a cancel
		// reason (held[0]); the source's reader keeps the branches.
		rd.keep = rep
		b1.held = &[4]engine.Value{undefined, cancelP.value(), engine.ObjectValue(s.obj), engine.ObjectValue(b2.obj)}
		b2.held = &[4]engine.Value{undefined, cancelP.value(), engine.ObjectValue(s.obj), engine.ObjectValue(b1.obj)}
	}
	err = e.uponPromise(rd.closed.value(), nil, func(reason engine.Value) error {
		if err := b1.ctrl.errorWith(reason); err != nil {
			return err
		}
		if err := b2.ctrl.errorWith(reason); err != nil {
			return err
		}
		if !canceled1 || !canceled2 {
			return e.fulfill(cancelP, undefined)
		}
		return nil
	})
	return b1, b2, err
}

// teeKeep reports what the source's reader of a tee keeps (rsReader.keep).
type teeKeep struct {
	b1, b2  *readableStream
	cancelP promise
}

func (t *teeKeep) ReportLive(add func(engine.Value), _ func(int)) {
	addObject(add, t.b1.obj)
	addObject(add, t.b2.obj)
	t.cancelP.liveValues(add)
}

// cloneView implements CloneAsUint8Array: a Uint8Array over a copy of the
// bytes of the view v.
func (e *env) cloneView(v engine.Value) (engine.Value, error) {
	b, _ := viewBytes(v)
	if err := e.r.ChargeMemory(len(b)); err != nil {
		return undefined, err
	}
	return e.uint8Array(append([]byte{}, b...))
}

// enqueueIfOpen enqueues v when the stream still takes chunks.
func (c *rsController) enqueueIfOpen(v engine.Value) error {
	if !c.canCloseOrEnqueue() {
		return nil
	}
	return c.enqueue(v)
}

// --- ReadableStream.from --------------------------------------------------------

// readableFrom implements ReadableStream.from(asyncIterable): an async
// iterator, or a sync one whose values are awaited.
func (e *env) readableFrom(v engine.Value) (*readableStream, error) {
	r := e.r
	async := true
	m, err := r.GetMethod(v, engine.SymbolKey(engine.SymAsyncIterator))
	if err != nil {
		return nil, err
	}
	if m.IsUndefined() {
		async = false
		if m, err = r.GetMethod(v, engine.SymbolKey(engine.SymIterator)); err != nil {
			return nil, err
		}
		if m.IsUndefined() {
			return nil, r.TypeError("ReadableStream.from: %s is not async iterable", r.DisplayString(v))
		}
	}
	it, err := r.Call(m, v, nil)
	if err != nil {
		return nil, err
	}
	if !it.IsObject() {
		return nil, r.TypeError("ReadableStream.from: the iterator is not an object")
	}
	next, err := e.get(it, e.keys.next)
	if err != nil {
		return nil, err
	}
	var s *readableStream
	pull := func(c *rsController) error {
		res, err := r.Call(next, it, nil)
		if err != nil {
			if res, err = e.rejectedErr(err); err != nil {
				return err
			}
		}
		step := func(res engine.Value) error {
			if !res.IsObject() {
				return c.errorWith(e.typeError("The iterator result is not an object"))
			}
			done, err := e.get(res, e.keys.done)
			if err != nil {
				return c.errorFrom(err)
			}
			if engine.ToBoolean(done) {
				if err := c.close(); err != nil {
					return err
				}
				return c.pulled()
			}
			val, err := e.get(res, e.keys.value)
			if err != nil {
				return c.errorFrom(err)
			}
			if async {
				if err := c.enqueueIfOpen(val); err != nil {
					return err
				}
				return c.pulled()
			}
			// A sync iterator's value is awaited, as by
			// CreateAsyncFromSyncIterator.
			return e.upon(val, func(x engine.Value) error {
				if err := c.enqueueIfOpen(x); err != nil {
					return err
				}
				return c.pulled()
			}, c.errorWith)
		}
		return e.upon(res, step, c.errorWith)
	}
	cancel := func(reason engine.Value) (engine.Value, error) {
		ret, err := r.GetMethod(it, r.KeyFromGoString("return"))
		if err != nil {
			return e.rejectedErr(err)
		}
		if ret.IsUndefined() {
			return undefined, nil
		}
		res, err := r.Call(ret, it, []engine.Value{reason})
		if err != nil {
			return e.rejectedErr(err)
		}
		p := e.newPromise()
		err = e.uponKeep(res, p.value(), func(x engine.Value) error {
			if !x.IsObject() {
				return e.rejectP(p, e.typeError("The iterator's return result is not an object"))
			}
			return e.fulfill(p, undefined)
		}, func(reason engine.Value) error { return e.rejectP(p, reason) })
		return p.value(), err
	}
	s = e.newReadable(0, pull, cancel)
	s.held = e.held(it, next, undefined, undefined)
	return s, nil
}

// --- bytes ----------------------------------------------------------------------

// viewBytes returns the bytes an ArrayBufferView views; ok is false for
// any other value.
func viewBytes(v engine.Value) ([]byte, bool) {
	if !v.IsObject() {
		return nil, false
	}
	o := v.AsObject()
	switch o.Class() {
	case engine.ClassTypedArray, engine.ClassDataView:
		return o.BufferData()
	}
	return nil, false
}

// viewLength is the byte length of an ArrayBufferView, 0 for another value.
func viewLength(v engine.Value) float64 {
	b, _ := viewBytes(v)
	return float64(len(b))
}

// uint8Array returns a Uint8Array over data, which it does not copy.
func (e *env) uint8Array(data []byte) (engine.Value, error) {
	buf, err := e.r.NewArrayBuffer(data)
	if err != nil {
		return undefined, err
	}
	return e.uint8ArrayOver(buf)
}

// uint8ArrayOver returns a Uint8Array over the ArrayBuffer buf.
func (e *env) uint8ArrayOver(buf *engine.Object) (engine.Value, error) {
	c, err := e.need(e.intrinsics().uint8Array, "Uint8Array")
	if err != nil {
		return undefined, err
	}
	return e.r.Construct(engine.ObjectValue(c), []engine.Value{engine.ObjectValue(buf)}, nil)
}

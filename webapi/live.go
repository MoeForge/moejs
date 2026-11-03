package webapi

import (
	"sync/atomic"

	"github.com/MoeForge/moejs/engine"
)

// The memory limit's measurements of what a program retains walk the
// JavaScript heap. What the package keeps for an object in Go (the queue,
// source and promises of a stream, the listeners of a signal, header
// values and URLs kept as Go strings, the bytes of a body) they see
// through the object's state, which reports it when a measurement reaches
// the object (engine.LiveReporter): it counts while JavaScript can reach
// the object, and no longer once it cannot, without waiting for Go's
// collector. A state that keeps another object's state alive adds that
// object, which reports its own. What only a Go closure keeps is reported
// by the closure's owner: a pipe, a tee or a whole read of a stream hangs a
// reporter on its internal reader or writer, and a reaction or an abort
// algorithm that settles a promise only Go holds is a function whose
// payload reports it. The work no object leads to (a fetch in flight, a
// whole-body read, the timer of AbortSignal.timeout) is listed while it
// runs and reported through one Runtime.AddLiveRoots registration per
// installation. Without a memory limit the package does no work for it.

// liveSet lists the work in flight of an installation, which only Go
// holds. Only the loop uses it, but for reg, which the loop sets and Close
// reads with the env's mu held.
type liveSet struct {
	ops map[engine.LiveReporter]struct{}
	obj *engine.Object // a reporter (env.reporter) of the set, the root it adds
	reg *liveReg       // the registration with the runtime, made on first use
}

// add lists op while it runs, under a memory limit; it registers the
// installation's provider with the runtime first, unless Close ran.
func (l *liveSet) add(e *env, op engine.LiveReporter) {
	if e.r.MemoryLimit() <= 0 {
		return
	}
	if l.reg == nil {
		g := &liveReg{}
		g.e.Store(e)
		g.remove = e.rt.AddLiveRoots(g.roots)
		e.mu.Lock()
		closed := e.closed
		if !closed {
			l.reg = g
		}
		e.mu.Unlock()
		if closed {
			g.remove()
			return
		}
		l.ops = map[engine.LiveReporter]struct{}{}
		l.obj = e.reporter(l)
	}
	l.ops[op] = struct{}{}
}

// remove ends the listing of op.
func (l *liveSet) remove(op engine.LiveReporter) {
	if l.ops != nil {
		delete(l.ops, op)
	}
}

// ReportLive reports the work in flight.
func (l *liveSet) ReportLive(add func(engine.Value), addBytes func(int)) {
	for op := range l.ops {
		op.ReportLive(add, addBytes)
	}
}

// liveReg is the provider of an installation, registered with the runtime
// (Runtime.AddLiveRoots), which keeps it as long as it lives: Close, which
// may run on any goroutine, drops the installation from it, and it
// removes itself from the runtime at the next measurement.
type liveReg struct {
	e      atomic.Pointer[env]
	remove func()
}

// roots adds the reporter of the work in flight to a measurement. It
// takes no lock.
func (g *liveReg) roots(add func(engine.Value)) {
	if e := g.e.Load(); e != nil {
		addObject(add, e.live.obj)
	} else {
		g.remove()
	}
}

// reporter returns, under a memory limit, a function JavaScript never
// sees whose payload is x: a measurement that reaches it asks x what it
// keeps. It hangs what a Go closure keeps on a state or a promise that
// leads to the closure. Without a limit it returns nil.
func (e *env) reporter(x engine.LiveReporter) *engine.Object {
	if e.r.MemoryLimit() <= 0 {
		return nil
	}
	return e.r.NewNativeDataFunction(engine.AtomEmpty, 0, callReporter, x)
}

func callReporter(*engine.Realm, *engine.FunctionData, engine.Value, []engine.Value) (engine.Value, error) {
	return undefined, nil
}

// reaction is the payload of a promise reaction that keeps keep, which
// only the closure of f holds otherwise (uponKeep).
type reaction struct {
	f    func(engine.Value) error
	keep engine.Value
}

func (x *reaction) ReportLive(add func(engine.Value), _ func(int)) { add(x.keep) }

func callReaction(_ *engine.Realm, fd *engine.FunctionData, _ engine.Value, args []engine.Value) (engine.Value, error) {
	return undefined, fd.Data().(*reaction).f(engine.Arg(args, 0))
}

// held returns the values for the held field of a stream whose algorithms
// call them, under a memory limit; nil without one.
func (e *env) held(a, b, c, d engine.Value) *[4]engine.Value {
	if e.r.MemoryLimit() <= 0 {
		return nil
	}
	return &[4]engine.Value{a, b, c, d}
}

// bytesOf reports the bytes of a Go buffer a closure keeps.
type bytesOf []byte

func (b bytesOf) ReportLive(_ func(engine.Value), addBytes func(int)) { addBytes(len(b)) }

// --- what the states keep ---------------------------------------------------------

func addObject(add func(engine.Value), o *engine.Object) {
	if o != nil {
		add(engine.ObjectValue(o))
	}
}

func addHeld(add func(engine.Value), h *[4]engine.Value) {
	if h != nil {
		for _, v := range h {
			add(v)
		}
	}
}

// liveValues adds the promise.
func (p promise) liveValues(add func(engine.Value)) {
	addObject(add, p.p)
}

// ReportLive reports the stream's stored error, what its algorithms call
// and what its source keeps in Go, its controller's object, queued chunks
// and size function, and its reader (rsReader.report).
func (s *readableStream) ReportLive(add func(engine.Value), addBytes func(int)) {
	add(s.stored)
	addHeld(add, s.held)
	if s.src != nil {
		s.src.ReportLive(add, addBytes)
	}
	if c := s.ctrl; c != nil {
		addObject(add, c.obj)
		add(c.size)
		for _, ch := range c.queue[c.head:] {
			add(ch.v)
		}
	}
	if rd := s.reader; rd != nil {
		rd.report(add)
	}
}

// report adds the reader's object, closed promise and pending reads, and
// the reporter of the operation an internal reader serves.
func (rd *rsReader) report(add func(engine.Value)) {
	addObject(add, rd.obj)
	rd.closed.liveValues(add)
	for _, req := range rd.reqs[rd.head:] {
		req.p.liveValues(add)
	}
	addObject(add, rd.keep)
}

// ReportLive reports the reader and the stream it reads.
func (rd *rsReader) ReportLive(add func(engine.Value), _ func(int)) {
	rd.report(add)
	if s := rd.s; s != nil {
		addObject(add, s.obj)
	}
}

// ReportLive reports the stream the controller controls, which reports
// the controller's state.
func (c *rsController) ReportLive(add func(engine.Value), _ func(int)) { addObject(add, c.s.obj) }

// ReportLive reports the iterator's ongoing promise and the stream it
// reads.
func (it *rsIterator) ReportLive(add func(engine.Value), _ func(int)) {
	if it.ongoing.p != nil {
		it.ongoing.liveValues(add)
	}
	if s := it.rd.s; s != nil {
		addObject(add, s.obj)
	}
}

// ReportLive reports the stream's stored error, promises and what its
// algorithms call, its controller's object, queued chunks, size function,
// signal and abort reason, and its writer (wsWriter.report).
func (s *writableStream) ReportLive(add func(engine.Value), _ func(int)) {
	add(s.stored)
	addHeld(add, s.held)
	for _, p := range s.writeRequests {
		p.liveValues(add)
	}
	for _, p := range [...]*promise{s.inFlightWrite, s.inFlightClose, s.closeRequest} {
		if p != nil {
			p.liveValues(add)
		}
	}
	if a := s.pendingAbort; a != nil {
		a.p.liveValues(add)
		add(a.reason)
	}
	if c := s.ctrl; c != nil {
		addObject(add, c.obj)
		add(c.size)
		add(c.abortReason)
		if c.signal != nil {
			addObject(add, c.signal.obj)
		}
		for _, ch := range c.queue {
			add(ch.v)
		}
	}
	if w := s.writer; w != nil {
		w.report(add)
	}
}

// report adds the writer's object and promises, and the reporter of the
// operation an internal writer serves.
func (w *wsWriter) report(add func(engine.Value)) {
	addObject(add, w.obj)
	w.ready.liveValues(add)
	w.closed.liveValues(add)
	addObject(add, w.keep)
}

// ReportLive reports the writer and the stream it writes.
func (w *wsWriter) ReportLive(add func(engine.Value), _ func(int)) {
	w.report(add)
	if s := w.s; s != nil {
		addObject(add, s.obj)
	}
}

// ReportLive reports the stream the controller controls.
func (c *wsController) ReportLive(add func(engine.Value), _ func(int)) { addObject(add, c.s.obj) }

// ReportLive reports the stream's two sides and backpressure promise, and
// its controller's object, finish promise and what the transformer's
// algorithms call.
func (t *transformStream) ReportLive(add func(engine.Value), _ func(int)) {
	if t.readable != nil {
		addObject(add, t.readable.obj)
	}
	if t.writable != nil {
		addObject(add, t.writable.obj)
	}
	if t.hasBPChange {
		t.bpChange.liveValues(add)
	}
	if c := t.ctrl; c != nil {
		addObject(add, c.obj)
		addHeld(add, c.held)
		if c.finish != nil {
			c.finish.liveValues(add)
		}
	}
}

// ReportLive reports the stream the controller controls.
func (c *tsController) ReportLive(add func(engine.Value), _ func(int)) { addObject(add, c.t.obj) }

// ReportLive reports the transform and the decoder it calls.
func (d *textDecoderStream) ReportLive(add func(engine.Value), _ func(int)) {
	add(d.decoder)
	add(d.decode)
	add(d.stream)
	if d.t != nil {
		addObject(add, d.t.obj)
	}
}

// ReportLive reports the transform.
func (x *textEncoderStream) ReportLive(add func(engine.Value), _ func(int)) {
	if x.t != nil {
		addObject(add, x.t.obj)
	}
}

// ReportLive reports the listeners, their types and the event handlers.
func (t *eventTarget) ReportLive(add func(engine.Value), addBytes func(int)) {
	for _, l := range t.listeners {
		add(l.callback)
		addBytes(len(l.typ))
	}
	for _, v := range t.handlers {
		add(v)
	}
}

// ReportLive reports the signal's reason, listeners and handlers, what its
// abort algorithms keep, and the signals it follows and those following it
// that wait for its abort.
func (s *abortSignal) ReportLive(add func(engine.Value), addBytes func(int)) {
	add(s.reason)
	s.eventTarget.ReportLive(add, addBytes)
	for _, a := range s.algorithms {
		if a.f != nil {
			add(a.keep)
		}
	}
	for _, src := range s.sources {
		addObject(add, src.obj)
	}
	for d := range s.busy {
		addObject(add, d.obj)
	}
}

// ReportLive reports the controller's signal.
func (c *abortController) ReportLive(add func(engine.Value), _ func(int)) {
	addObject(add, c.signal.obj)
}

// ReportLive reports the event's type and targets.
func (x *event) ReportLive(add func(engine.Value), _ func(int)) {
	add(x.typ)
	add(x.target)
	add(x.currentTarget)
}

// ReportLive reports the exception's name and message.
func (d *domException) ReportLive(_ func(engine.Value), addBytes func(int)) {
	addBytes(len(d.name) + len(d.message))
}

// bytes is the bytes of the names and values the list keeps, and those of
// src while it is not read.
func (h *headers) bytes() int {
	if h.src != nil {
		return h.size + mapBytes(h.src)
	}
	return h.size
}

// ReportLive reports the names and values.
func (h *headers) ReportLive(_ func(engine.Value), addBytes func(int)) { addBytes(h.bytes()) }

// reportFrom reports the headers of a request or response: their object,
// which reports them, or their bytes.
func (h *headers) reportFrom(add func(engine.Value), addBytes func(int)) {
	if h == nil {
		return
	}
	if h.obj != nil {
		add(engine.ObjectValue(h.obj))
	} else {
		addBytes(h.bytes())
	}
}

// ReportLive reports the pairs and the URL whose query they are.
func (sp *searchParams) ReportLive(add func(engine.Value), addBytes func(int)) {
	addBytes(sp.size)
	if x := sp.url; x != nil {
		addObject(add, x.obj)
	}
}

// ReportLive reports the URL's serialization and search params.
func (x *urlObject) ReportLive(add func(engine.Value), addBytes func(int)) {
	addBytes(len(x.u.Href()))
	add(x.href)
	if x.params != nil {
		addObject(add, x.params.obj)
	}
}

// ReportLive reports the headers or search params the iterator lists.
func (it *pairIterator) ReportLive(add func(engine.Value), _ func(int)) {
	switch t := it.target.(type) {
	case *headers:
		addObject(add, t.obj)
	case *searchParams:
		addObject(add, t.obj)
	}
}

// report adds the body's stream and, for a body of bytes or the body of a
// fetch's response, what Go keeps of it.
func (b *body) report(add func(engine.Value), addBytes func(int)) {
	addBytes(len(b.src))
	if b.stream != nil {
		addObject(add, b.stream.obj)
	}
	if nb := b.net; nb != nil {
		nb.report(add)
	}
}

// ReportLive reports the request's strings, headers, body and signals.
func (rq *request) ReportLive(add func(engine.Value), addBytes func(int)) {
	if rq.url != nil {
		addBytes(len(rq.url.Href()))
	}
	addBytes(len(rq.referrer) + len(rq.integrity))
	rq.headers.reportFrom(add, addBytes)
	rq.body.report(add, addBytes)
	if rq.signal != nil {
		addObject(add, rq.signal.obj)
	}
	if rq.ownSignal != nil {
		addObject(add, rq.ownSignal.obj)
	}
}

// ReportLive reports the response's strings, headers and body.
func (res *response) ReportLive(add func(engine.Value), addBytes func(int)) {
	addBytes(len(res.url) + len(res.statusText))
	res.headers.reportFrom(add, addBytes)
	res.body.report(add, addBytes)
}

// report adds the abort reason and stream of a fetch's response body, and
// the promise of a whole read of it.
func (nb *netBody) report(add func(engine.Value)) {
	add(nb.reason)
	if nb.stream != nil {
		addObject(add, nb.stream.obj)
	}
	if p := nb.consume; p != nil && !p.settled() {
		p.liveValues(add)
	}
}

// ReportLive reports a fetch in flight: its promise while pending, its
// request, the stream its body is read from, and its response's body.
func (f *fetchState) ReportLive(add func(engine.Value), addBytes func(int)) {
	if !f.p.settled() {
		f.p.liveValues(add)
	}
	f.rq.ReportLive(add, addBytes)
	if rb := f.reqBody; rb != nil && rb.rd.s != nil {
		addObject(add, rb.rd.s.obj)
	}
	if nb := f.net; nb != nil {
		nb.report(add)
	}
}

// ReportLive reports a whole-body read in flight: its promise and the
// bytes it charged, and the body it reads.
func (br *bodyRead) ReportLive(add func(engine.Value), addBytes func(int)) {
	br.p.liveValues(add)
	addBytes(br.charged)
	br.nb.report(add)
}

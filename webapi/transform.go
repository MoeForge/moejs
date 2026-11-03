package webapi

import (
	"unicode/utf16"
	"unicode/utf8"

	"github.com/MoeForge/moejs/engine"
)

// The Streams Standard's TransformStream and queuing strategies, and the
// Encoding Standard's TextDecoderStream and TextEncoderStream over the
// runtime's TextDecoder and TextEncoder.

// transformStream is the state of a TransformStream.
type transformStream struct {
	obj          *engine.Object
	readable     *readableStream
	writable     *writableStream
	bpChange     promise // the backpressure change promise
	ctrl         *tsController
	backpressure bool
	hasBPChange  bool
}

// tsController is the state of a TransformStreamDefaultController.
type tsController struct {
	e   *env
	t   *transformStream
	obj *engine.Object
	// The transformer's algorithms; they return what to wait for. Nil once
	// cleared.
	transform func(v engine.Value) (engine.Value, error)
	flush     func() (engine.Value, error)
	cancel    func(reason engine.Value) (engine.Value, error)
	finish    *promise
	// held are the JavaScript values the transformer's algorithms call,
	// under a memory limit, until they are cleared
	// (readableStream.held).
	held *[4]engine.Value
}

func (c *tsController) object() *engine.Object {
	if c.obj == nil {
		c.obj, _ = c.e.newObject(clsTransformStreamDefaultController, nil, c)
	}
	return c.obj
}

func (c *tsController) clearAlgorithms() {
	c.transform, c.flush, c.cancel, c.held = nil, nil, nil, nil
}

// transformer are the algorithms of a native transformer; a nil one is the
// default (identity transform, nothing to flush or cancel).
type transformer struct {
	transform func(c *tsController, v engine.Value) (engine.Value, error)
	flush     func(c *tsController) (engine.Value, error)
}

func (e *env) transformStreamSpec() classSpec {
	ts := func(r *engine.Realm, this engine.Value, name string) (*transformStream, error) {
		return thisAs[*transformStream](r, this, "TransformStream.prototype."+name)
	}
	return classSpec{
		name: "TransformStream",
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			t, err := e.constructTransform(newTarget, engine.Arg(args, 0), engine.Arg(args, 1), engine.Arg(args, 2))
			if err != nil {
				return undefined, err
			}
			return engine.ObjectValue(t.obj), nil
		},
		members: []member{
			{name: "readable", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				t, err := ts(r, this, "readable")
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(t.readable.obj), nil
			}},
			{name: "writable", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				t, err := ts(r, this, "writable")
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(t.writable.obj), nil
			}},
		},
	}
}

// constructTransform implements new TransformStream(transformer,
// writableStrategy, readableStrategy).
func (e *env) constructTransform(newTarget *engine.Object, tr, ws, rs engine.Value) (*transformStream, error) {
	r := e.r
	if !tr.IsNullish() && !tr.IsObject() {
		return nil, r.TypeError("Failed to construct 'TransformStream': The provided value is not of type 'Transformer'.")
	}
	cancelFn, err := e.method(tr, "cancel")
	if err != nil {
		return nil, err
	}
	flushFn, err := e.method(tr, "flush")
	if err != nil {
		return nil, err
	}
	for _, n := range []string{"readableType", "writableType"} {
		v, err := e.getName(tr, n)
		if err != nil {
			return nil, err
		}
		if !v.IsUndefined() {
			return nil, r.RangeError("Invalid %s is specified", n)
		}
	}
	startFn, err := e.method(tr, "start")
	if err != nil {
		return nil, err
	}
	transformFn, err := e.method(tr, "transform")
	if err != nil {
		return nil, err
	}
	whwm, wsize, err := e.strategy(ws, 1)
	if err != nil {
		return nil, err
	}
	rhwm, rsize, err := e.strategy(rs, 0)
	if err != nil {
		return nil, err
	}
	t := &transformStream{}
	if t.obj, err = e.newObject(clsTransformStream, newTarget, t); err != nil {
		return nil, err
	}
	startP := e.newPromise()
	e.initTransform(t, startP.value(), whwm, wsize, rhwm, rsize)
	c := &tsController{e: e, t: t}
	t.ctrl = c
	c.held = e.held(tr, transformFn, flushFn, cancelFn)
	ctrl := engine.ObjectValue(c.object())
	call := func(fn engine.Value, args ...engine.Value) (engine.Value, error) {
		v, err := r.Call(fn, tr, args)
		if err != nil {
			return e.rejectedErr(err)
		}
		return v, nil
	}
	if transformFn.IsUndefined() {
		c.transform = func(v engine.Value) (engine.Value, error) { return e.identityTransform(c, v) }
	} else {
		c.transform = func(v engine.Value) (engine.Value, error) { return call(transformFn, v, ctrl) }
	}
	c.flush = func() (engine.Value, error) {
		if flushFn.IsUndefined() {
			return undefined, nil
		}
		return call(flushFn, ctrl)
	}
	c.cancel = func(reason engine.Value) (engine.Value, error) {
		if cancelFn.IsUndefined() {
			return undefined, nil
		}
		return call(cancelFn, reason)
	}
	var start engine.Value = undefined
	if !startFn.IsUndefined() {
		if start, err = r.Call(startFn, tr, []engine.Value{ctrl}); err != nil {
			return nil, err
		}
	}
	return t, e.fulfill(startP, start)
}

// identityTransform enqueues v and returns undefined, or the rejected
// promise of a failed enqueue.
func (e *env) identityTransform(c *tsController, v engine.Value) (engine.Value, error) {
	if err := e.tsEnqueue(c, v); err != nil {
		return e.rejectedErr(err)
	}
	return undefined, nil
}

// newTransform creates a TransformStream of the class's prototype for a
// native transformer, started at once.
func (e *env) newTransform(obj *engine.Object, tr transformer) *transformStream {
	t := &transformStream{obj: obj}
	if obj == nil {
		t.obj, _ = e.newObject(clsTransformStream, nil, t)
	}
	start, _ := e.resolved(undefined)
	e.initTransform(t, start, 1, undefined, 0, undefined)
	c := &tsController{e: e, t: t}
	t.ctrl = c
	c.transform = func(v engine.Value) (engine.Value, error) {
		if tr.transform == nil {
			return e.identityTransform(c, v)
		}
		return tr.transform(c, v)
	}
	c.flush = func() (engine.Value, error) {
		if tr.flush == nil {
			return undefined, nil
		}
		return tr.flush(c)
	}
	c.cancel = func(engine.Value) (engine.Value, error) { return undefined, nil }
	return t
}

// initTransform implements InitializeTransformStream.
func (e *env) initTransform(t *transformStream, start engine.Value, whwm float64, wsize engine.Value, rhwm float64, rsize engine.Value) {
	// The writable side, whose sink is the transform.
	w := &writableStream{stored: undefined}
	w.obj, _ = e.newObject(clsWritableStream, nil, w)
	wc := &wsController{e: e, s: w, hwm: whwm, size: wsize}
	w.ctrl = wc
	// Each side's algorithms keep the transform.
	w.held = e.held(engine.ObjectValue(t.obj), undefined, undefined, undefined)
	wc.write = func(v engine.Value) (engine.Value, error) { return e.tsSinkWrite(t, v) }
	wc.close = func() (engine.Value, error) { return e.tsSinkClose(t) }
	wc.abort = func(reason engine.Value) (engine.Value, error) { return e.tsSinkAbort(t, reason) }
	t.writable = w
	_ = e.startWritable(wc, start)

	// The readable side, whose pull releases the backpressure.
	rd := &readableStream{stored: undefined}
	rd.obj, _ = e.newObject(clsReadableStream, nil, rd)
	rc := &rsController{e: e, s: rd, hwm: rhwm, size: rsize}
	rd.ctrl = rc
	rd.held = e.held(engine.ObjectValue(t.obj), undefined, undefined, undefined)
	rc.pull = func() error {
		if err := e.setBackpressure(t, false); err != nil {
			return err
		}
		return e.uponPromise(t.bpChange.value(), func(engine.Value) error { return rc.pulled() }, rc.errorWith)
	}
	rc.cancel = func(reason engine.Value) (engine.Value, error) { return e.tsSourceCancel(t, reason) }
	t.readable = rd
	_ = e.upon(start, func(engine.Value) error {
		rc.started = true
		return rc.callPullIfNeeded()
	}, rc.errorWith)
	_ = e.setBackpressure(t, true)
}

func (e *env) setBackpressure(t *transformStream, bp bool) error {
	if t.hasBPChange {
		if err := e.fulfill(t.bpChange, undefined); err != nil {
			return err
		}
	}
	t.bpChange, t.hasBPChange = e.newPromise(), true
	t.backpressure = bp
	return nil
}

func (e *env) tsSinkWrite(t *transformStream, v engine.Value) (engine.Value, error) {
	if !t.backpressure {
		return e.performTransform(t, v)
	}
	p := e.newPromise()
	err := e.uponPromise(t.bpChange.value(), func(engine.Value) error {
		if w := t.writable; w.state == wsErroring {
			return e.rejectP(p, w.stored)
		}
		res, err := e.performTransform(t, v)
		if err != nil {
			return err
		}
		return e.fulfill(p, res)
	}, func(r engine.Value) error { return e.rejectP(p, r) })
	return p.value(), err
}

// performTransform runs the transform algorithm; its rejection errors the
// stream.
func (e *env) performTransform(t *transformStream, v engine.Value) (engine.Value, error) {
	c := t.ctrl
	if c.transform == nil {
		return undefined, nil
	}
	res, err := c.transform(v)
	if err != nil {
		return undefined, err
	}
	if res.IsUndefined() {
		return undefined, nil
	}
	p := e.newPromise()
	err = e.upon(res, func(engine.Value) error { return e.fulfill(p, undefined) }, func(r engine.Value) error {
		if err := e.tsError(t, r); err != nil {
			return err
		}
		return e.rejectP(p, r)
	})
	return p.value(), err
}

// finishWith runs action, the transformer's flush or cancel, once for the
// stream and settles the finish promise from its outcome with ok and fail.
func (e *env) finishWith(c *tsController, action func() (engine.Value, error), ok func() error, fail func(engine.Value) error) (engine.Value, error) {
	if c.finish != nil {
		return c.finish.value(), nil
	}
	f := e.newPromise()
	c.finish = &f
	var res engine.Value = undefined
	if action != nil {
		var err error
		if res, err = action(); err != nil {
			return undefined, err
		}
	}
	c.clearAlgorithms()
	err := e.upon(res, func(engine.Value) error {
		if err := ok(); err != nil {
			return err
		}
		if !f.settled() {
			return e.fulfill(f, undefined)
		}
		return nil
	}, func(r engine.Value) error {
		if err := fail(r); err != nil {
			return err
		}
		return e.rejectP(f, r)
	})
	return f.value(), err
}

func (e *env) tsSinkClose(t *transformStream) (engine.Value, error) {
	c := t.ctrl
	rd := t.readable
	return e.finishWith(c, c.flush, func() error {
		if rd.state == rsErrored {
			return e.rejectP(*c.finish, rd.stored)
		}
		return rd.ctrl.close()
	}, rd.ctrl.errorWith)
}

func (e *env) tsSinkAbort(t *transformStream, reason engine.Value) (engine.Value, error) {
	c := t.ctrl
	rd := t.readable
	var cancel func() (engine.Value, error)
	if c.cancel != nil {
		cancel = func() (engine.Value, error) { return c.cancel(reason) }
	}
	return e.finishWith(c, cancel, func() error {
		if rd.state == rsErrored {
			return e.rejectP(*c.finish, rd.stored)
		}
		return rd.ctrl.errorWith(reason)
	}, rd.ctrl.errorWith)
}

func (e *env) tsSourceCancel(t *transformStream, reason engine.Value) (engine.Value, error) {
	c := t.ctrl
	w := t.writable
	var cancel func() (engine.Value, error)
	if c.cancel != nil {
		cancel = func() (engine.Value, error) { return c.cancel(reason) }
	}
	return e.finishWith(c, cancel, func() error {
		if w.state == wsErrored {
			return e.rejectP(*c.finish, w.stored)
		}
		if err := e.errorWritableIfNeeded(w.ctrl, reason); err != nil {
			return err
		}
		return e.unblockWrite(t)
	}, func(r engine.Value) error {
		if err := e.errorWritableIfNeeded(w.ctrl, r); err != nil {
			return err
		}
		return e.unblockWrite(t)
	})
}

// tsError implements TransformStreamError.
func (e *env) tsError(t *transformStream, reason engine.Value) error {
	if err := t.readable.ctrl.errorWith(reason); err != nil {
		return err
	}
	return e.errorWritableAndUnblock(t, reason)
}

func (e *env) errorWritableAndUnblock(t *transformStream, reason engine.Value) error {
	t.ctrl.clearAlgorithms()
	if err := e.errorWritableIfNeeded(t.writable.ctrl, reason); err != nil {
		return err
	}
	return e.unblockWrite(t)
}

func (e *env) unblockWrite(t *transformStream) error {
	if t.backpressure {
		return e.setBackpressure(t, false)
	}
	return nil
}

// tsEnqueue implements TransformStreamDefaultControllerEnqueue.
func (e *env) tsEnqueue(c *tsController, v engine.Value) error {
	t := c.t
	rc := t.readable.ctrl
	if !rc.canCloseOrEnqueue() {
		return e.r.TypeError("Readable side is not in a state that permits enqueue")
	}
	if err := rc.enqueue(v); err != nil {
		reason, ok := e.r.ThrownValue(err)
		if !ok {
			return err
		}
		if err := e.errorWritableAndUnblock(t, reason); err != nil {
			return err
		}
		return e.r.Throw(t.readable.stored)
	}
	if bp := !rc.shouldCallPull(); bp != t.backpressure && bp {
		return e.setBackpressure(t, true)
	}
	return nil
}

// tsTerminate implements TransformStreamDefaultControllerTerminate.
func (e *env) tsTerminate(c *tsController) error {
	if err := c.t.readable.ctrl.close(); err != nil {
		return err
	}
	return e.errorWritableAndUnblock(c.t, e.typeError("TransformStream terminated"))
}

func (e *env) tsControllerSpec() classSpec {
	ctl := func(r *engine.Realm, this engine.Value, name string) (*tsController, error) {
		return thisAs[*tsController](r, this, "TransformStreamDefaultController.prototype."+name)
	}
	return classSpec{
		name: "TransformStreamDefaultController",
		members: []member{
			{name: "desiredSize", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "desiredSize")
				if err != nil {
					return undefined, err
				}
				d, ok := c.t.readable.ctrl.desiredSize()
				if !ok {
					return engine.Null(), nil
				}
				return engine.NumberValue(d), nil
			}},
			{name: "enqueue", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "enqueue")
				if err != nil {
					return undefined, err
				}
				return undefined, e.tsEnqueue(c, engine.Arg(args, 0))
			}},
			{name: "error", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "error")
				if err != nil {
					return undefined, err
				}
				return undefined, e.tsError(c.t, engine.Arg(args, 0))
			}},
			{name: "terminate", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				c, err := ctl(r, this, "terminate")
				if err != nil {
					return undefined, err
				}
				return undefined, e.tsTerminate(c)
			}},
		},
	}
}

// --- queuing strategies ---------------------------------------------------------

// queuingStrategy is the state of a ByteLengthQueuingStrategy or a
// CountQueuingStrategy.
type queuingStrategy struct {
	hwm   engine.Value
	bytes bool
}

func (e *env) strategySpec(name string, bytes bool) classSpec {
	var size *engine.Object
	sizeFn := func() *engine.Object {
		if size == nil {
			if bytes {
				size = e.r.NewNativeFunction(e.r.InternGoString("size"), 1, func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
					return e.getName(engine.Arg(args, 0), "byteLength")
				})
			} else {
				size = e.r.NewNativeFunction(e.r.InternGoString("size"), 0, func(*engine.Realm, engine.Value, []engine.Value) (engine.Value, error) {
					return engine.Int64Value(1), nil
				})
			}
		}
		return size
	}
	id := clsCountQueuingStrategy
	if bytes {
		id = clsByteLengthQueuingStrategy
	}
	return classSpec{
		name: name, length: 1,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			init := engine.Arg(args, 0)
			if !init.IsObject() {
				return undefined, r.TypeError("Failed to construct '%s': 1 argument required, but only 0 present.", name)
			}
			h, err := e.getName(init, "highWaterMark")
			if err != nil {
				return undefined, err
			}
			if h.IsUndefined() {
				return undefined, r.TypeError("Failed to construct '%s': required member highWaterMark is undefined.", name)
			}
			n, err := r.ToNumber(h)
			if err != nil {
				return undefined, err
			}
			o, err := e.newObject(id, newTarget, &queuingStrategy{hwm: engine.NumberValue(n), bytes: bytes})
			if err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "highWaterMark", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				q, err := thisAs[*queuingStrategy](r, this, name+".prototype.highWaterMark")
				if err != nil || q.bytes != bytes {
					return undefined, r.TypeError("Method %s.prototype.highWaterMark called on incompatible receiver %s", name, r.DisplayString(this))
				}
				return q.hwm, nil
			}},
			{name: "size", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				q, err := thisAs[*queuingStrategy](r, this, name+".prototype.size")
				if err != nil || q.bytes != bytes {
					return undefined, r.TypeError("Method %s.prototype.size called on incompatible receiver %s", name, r.DisplayString(this))
				}
				return engine.ObjectValue(sizeFn()), nil
			}},
		},
	}
}

// --- TextDecoderStream and TextEncoderStream ------------------------------------

// textDecoderStream is the state of a TextDecoderStream: a transform over
// one TextDecoder of the runtime.
type textDecoderStream struct {
	t       *transformStream
	decoder engine.Value
	decode  engine.Value // TextDecoder.prototype.decode
	stream  engine.Value // { stream: true }
}

func (e *env) textDecoderStreamSpec() classSpec {
	tds := func(r *engine.Realm, this engine.Value, name string) (*textDecoderStream, error) {
		return thisAs[*textDecoderStream](r, this, "TextDecoderStream.prototype."+name)
	}
	prop := func(name string) member {
		return member{name: name, get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			d, err := tds(r, this, name)
			if err != nil {
				return undefined, err
			}
			return e.getName(d.decoder, name)
		}}
	}
	return classSpec{
		name: "TextDecoderStream",
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			x := e.intrinsics()
			ctor, err := e.need(x.textDecoder, "TextDecoder")
			if err != nil {
				return undefined, err
			}
			decode, err := e.need(x.decode, "TextDecoder.prototype.decode")
			if err != nil {
				return undefined, err
			}
			dec, err := r.Construct(engine.ObjectValue(ctor), args, nil)
			if err != nil {
				return undefined, err
			}
			opts := r.NewObject()
			opts.DefineOwnDataFast(r, e.keys.stream, engine.Bool(true), attrMethod)
			d := &textDecoderStream{decoder: dec, decode: engine.ObjectValue(decode), stream: engine.ObjectValue(opts)}
			o, err := e.newObject(clsTextDecoderStream, newTarget, d)
			if err != nil {
				return undefined, err
			}
			d.t = e.newTransform(nil, transformer{
				transform: func(c *tsController, v engine.Value) (engine.Value, error) {
					s, err := r.Call(d.decode, d.decoder, []engine.Value{v, d.stream})
					if err != nil {
						return e.rejectedErr(err)
					}
					if s.IsString() && s.AsString().Len() > 0 {
						if err := e.tsEnqueue(c, s); err != nil {
							return e.rejectedErr(err)
						}
					}
					return undefined, nil
				},
				flush: func(c *tsController) (engine.Value, error) {
					s, err := r.Call(d.decode, d.decoder, nil)
					if err != nil {
						return e.rejectedErr(err)
					}
					if s.IsString() && s.AsString().Len() > 0 {
						if err := e.tsEnqueue(c, s); err != nil {
							return e.rejectedErr(err)
						}
					}
					return undefined, nil
				},
			})
			// The transformer's closures keep the decoder.
			d.t.ctrl.held = e.held(d.decoder, d.decode, d.stream, undefined)
			return engine.ObjectValue(o), nil
		},
		members: []member{
			prop("encoding"), prop("fatal"), prop("ignoreBOM"),
			{name: "readable", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				d, err := tds(r, this, "readable")
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(d.t.readable.obj), nil
			}},
			{name: "writable", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				d, err := tds(r, this, "writable")
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(d.t.writable.obj), nil
			}},
		},
	}
}

// textEncoderStream is the state of a TextEncoderStream.
type textEncoderStream struct {
	t       *transformStream
	pending uint16 // a high surrogate that ended the last chunk
}

func (e *env) textEncoderStreamSpec() classSpec {
	tes := func(r *engine.Realm, this engine.Value, name string) (*textEncoderStream, error) {
		return thisAs[*textEncoderStream](r, this, "TextEncoderStream.prototype."+name)
	}
	return classSpec{
		name: "TextEncoderStream",
		ctor: func(r *engine.Realm, _ []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			x := &textEncoderStream{}
			o, err := e.newObject(clsTextEncoderStream, newTarget, x)
			if err != nil {
				return undefined, err
			}
			x.t = e.newTransform(nil, transformer{
				transform: func(c *tsController, v engine.Value) (engine.Value, error) {
					s, err := r.ToString(v)
					if err != nil {
						return e.rejectedErr(err)
					}
					b := x.encode(s)
					if len(b) == 0 {
						return undefined, nil
					}
					if err := e.r.ChargeMemory(len(b)); err != nil {
						return undefined, err
					}
					u, err := e.uint8Array(b)
					if err == nil {
						err = e.tsEnqueue(c, u)
					}
					if err != nil {
						return e.rejectedErr(err)
					}
					return undefined, nil
				},
				flush: func(c *tsController) (engine.Value, error) {
					if x.pending == 0 {
						return undefined, nil
					}
					x.pending = 0
					u, err := e.uint8Array([]byte("�"))
					if err == nil {
						err = e.tsEnqueue(c, u)
					}
					if err != nil {
						return e.rejectedErr(err)
					}
					return undefined, nil
				},
			})
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "encoding", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				if _, err := tes(r, this, "encoding"); err != nil {
					return undefined, err
				}
				return str("utf-8"), nil
			}},
			{name: "readable", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				x, err := tes(r, this, "readable")
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(x.t.readable.obj), nil
			}},
			{name: "writable", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				x, err := tes(r, this, "writable")
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(x.t.writable.obj), nil
			}},
		},
	}
}

// encode returns the UTF-8 bytes of s, joining a surrogate pair split
// across chunks; a lone surrogate is U+FFFD.
func (x *textEncoderStream) encode(s *engine.String) []byte {
	n := s.Len()
	if x.pending == 0 && s.IsASCII() {
		return []byte(s.GoString())
	}
	u := make([]uint16, 0, n+1)
	if x.pending != 0 {
		u = append(u, x.pending)
		x.pending = 0
	}
	for i := range n {
		u = append(u, s.At(i))
	}
	if k := len(u); k > 0 && utf16.IsSurrogate(rune(u[k-1])) && u[k-1] < 0xDC00 {
		x.pending = u[k-1]
		u = u[:k-1]
	}
	b := make([]byte, 0, len(u)*3)
	for _, c := range utf16.Decode(u) {
		b = utf8.AppendRune(b, c)
	}
	return b
}

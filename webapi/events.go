package webapi

import (
	"math"
	"slices"
	"time"
	"weak"

	"github.com/MoeForge/moejs/engine"
)

// --- DOMException ---------------------------------------------------------------

// domException is the state of a DOMException.
type domException struct {
	name, message string
}

// The legacy codes of the DOMException names that have one.
var domExceptionCodes = map[string]int{
	"IndexSizeError": 1, "HierarchyRequestError": 3, "WrongDocumentError": 4,
	"InvalidCharacterError": 5, "NoModificationAllowedError": 7, "NotFoundError": 8,
	"NotSupportedError": 9, "InvalidStateError": 11, "SyntaxError": 12,
	"InvalidModificationError": 13, "NamespaceError": 14, "InvalidAccessError": 15,
	"TypeMismatchError": 17, "SecurityError": 18, "NetworkError": 19, "AbortError": 20,
	"URLMismatchError": 21, "QuotaExceededError": 22, "TimeoutError": 23,
	"InvalidNodeTypeError": 24, "DataCloneError": 25,
}

var domExceptionConstants = []struct {
	name string
	code int
}{
	{"INDEX_SIZE_ERR", 1}, {"DOMSTRING_SIZE_ERR", 2}, {"HIERARCHY_REQUEST_ERR", 3},
	{"WRONG_DOCUMENT_ERR", 4}, {"INVALID_CHARACTER_ERR", 5}, {"NO_DATA_ALLOWED_ERR", 6},
	{"NO_MODIFICATION_ALLOWED_ERR", 7}, {"NOT_FOUND_ERR", 8}, {"NOT_SUPPORTED_ERR", 9},
	{"INUSE_ATTRIBUTE_ERR", 10}, {"INVALID_STATE_ERR", 11}, {"SYNTAX_ERR", 12},
	{"INVALID_MODIFICATION_ERR", 13}, {"NAMESPACE_ERR", 14}, {"INVALID_ACCESS_ERR", 15},
	{"VALIDATION_ERR", 16}, {"TYPE_MISMATCH_ERR", 17}, {"SECURITY_ERR", 18},
	{"NETWORK_ERR", 19}, {"ABORT_ERR", 20}, {"URL_MISMATCH_ERR", 21},
	{"QUOTA_EXCEEDED_ERR", 22}, {"TIMEOUT_ERR", 23}, {"INVALID_NODE_TYPE_ERR", 24},
	{"DATA_CLONE_ERR", 25},
}

func (e *env) domExceptionSpec() classSpec {
	consts := make([]member, len(domExceptionConstants))
	for i, c := range domExceptionConstants {
		consts[i] = member{name: c.name, value: engine.Int64Value(int64(c.code)), isValue: true}
	}
	get := func(name string, f func(*domException) engine.Value) member {
		return member{name: name, get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			d, err := thisAs[*domException](r, this, "DOMException.prototype."+name)
			if err != nil {
				return undefined, err
			}
			return f(d), nil
		}}
	}
	members := append([]member{
		get("name", func(d *domException) engine.Value { return str(d.name) }),
		get("message", func(d *domException) engine.Value { return str(d.message) }),
		get("code", func(d *domException) engine.Value { return engine.Int64Value(int64(domExceptionCodes[d.name])) }),
	}, consts...)
	return classSpec{
		name: "DOMException", length: 0, members: members, statics: consts,
		proto: e.r.ErrorPrototype,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			d := &domException{name: "Error"}
			var cause engine.Value
			hasCause := false
			if v := engine.Arg(args, 0); !v.IsUndefined() {
				s, err := e.goString(v)
				if err != nil {
					return undefined, err
				}
				d.message = s
			}
			// The second argument is the name, or an options object with
			// name and cause, as in Node.js.
			if v := engine.Arg(args, 1); v.IsObject() {
				n, err := e.getName(v, "name")
				if err != nil {
					return undefined, err
				}
				if !n.IsUndefined() {
					if d.name, err = e.goString(n); err != nil {
						return undefined, err
					}
				}
				if hasCause = v.AsObject().HasProperty(e.keys.cause); hasCause {
					if cause, err = e.get(v, e.keys.cause); err != nil {
						return undefined, err
					}
				}
			} else if !v.IsUndefined() {
				s, err := e.goString(v)
				if err != nil {
					return undefined, err
				}
				d.name = s
			}
			// The name and message are Go strings the object keeps.
			if err := r.ChargeMemory(len(d.name) + len(d.message)); err != nil {
				return undefined, err
			}
			o, err := e.newObject(clsDOMException, newTarget, d)
			if err != nil {
				return undefined, err
			}
			if hasCause {
				o.DefineOwnDataFast(r, e.keys.cause, cause, attrW|attrC)
			}
			return engine.ObjectValue(o), nil
		},
	}
}

// domException creates a DOMException with name and message.
func (e *env) domException(name, message string) engine.Value {
	o, _ := e.newObject(clsDOMException, nil, &domException{name: name, message: message})
	return engine.ObjectValue(o)
}

// abortError is the reason of an abort without one.
func (e *env) abortError() engine.Value {
	return e.domException("AbortError", "This operation was aborted")
}

// --- Event ----------------------------------------------------------------------

const (
	phaseNone = iota
	phaseCapturing
	phaseAtTarget
	phaseBubbling
)

// event is the state of an Event.
type event struct {
	typ                     engine.Value // a string
	target, currentTarget   engine.Value
	phase                   uint8
	bubbles, cancelable     bool
	composed, trusted       bool
	canceled, stop, stopNow bool
	dispatching, inPassive  bool
	timeStamp               float64
}

func (e *env) eventSpec() classSpec {
	consts := []member{
		{name: "NONE", value: engine.Int64Value(phaseNone), isValue: true},
		{name: "CAPTURING_PHASE", value: engine.Int64Value(phaseCapturing), isValue: true},
		{name: "AT_TARGET", value: engine.Int64Value(phaseAtTarget), isValue: true},
		{name: "BUBBLING_PHASE", value: engine.Int64Value(phaseBubbling), isValue: true},
	}
	ev := func(r *engine.Realm, this engine.Value, name string) (*event, error) {
		return thisAs[*event](r, this, "Event.prototype."+name)
	}
	get := func(name string, f func(*event) engine.Value) member {
		return member{name: name, get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			x, err := ev(r, this, name)
			if err != nil {
				return undefined, err
			}
			return f(x), nil
		}}
	}
	nullable := func(v engine.Value) engine.Value {
		if v.IsUndefined() {
			return engine.Null()
		}
		return v
	}
	act := func(name string, f func(*event)) member {
		return member{name: name, fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			x, err := ev(r, this, name)
			if err != nil {
				return undefined, err
			}
			f(x)
			return undefined, nil
		}}
	}
	cancel := func(x *event) {
		if x.cancelable && !x.inPassive {
			x.canceled = true
		}
	}
	members := append([]member{
		get("type", func(x *event) engine.Value { return x.typ }),
		get("target", func(x *event) engine.Value { return nullable(x.target) }),
		get("srcElement", func(x *event) engine.Value { return nullable(x.target) }),
		get("currentTarget", func(x *event) engine.Value { return nullable(x.currentTarget) }),
		{name: "composedPath", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			x, err := ev(r, this, "composedPath")
			if err != nil {
				return undefined, err
			}
			if x.currentTarget.IsUndefined() || x.currentTarget.IsNull() {
				return engine.ObjectValue(r.NewArrayFromSlice(nil)), nil
			}
			return engine.ObjectValue(r.NewArrayFromSlice([]engine.Value{x.currentTarget})), nil
		}},
		get("eventPhase", func(x *event) engine.Value { return engine.Int64Value(int64(x.phase)) }),
		act("stopPropagation", func(x *event) { x.stop = true }),
		{name: "cancelBubble",
			get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				x, err := ev(r, this, "cancelBubble")
				if err != nil {
					return undefined, err
				}
				return engine.Bool(x.stop), nil
			},
			set: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				x, err := ev(r, this, "cancelBubble")
				if err != nil {
					return undefined, err
				}
				if engine.ToBoolean(engine.Arg(args, 0)) {
					x.stop = true
				}
				return undefined, nil
			}},
		act("stopImmediatePropagation", func(x *event) { x.stop, x.stopNow = true, true }),
		get("bubbles", func(x *event) engine.Value { return engine.Bool(x.bubbles) }),
		get("cancelable", func(x *event) engine.Value { return engine.Bool(x.cancelable) }),
		{name: "returnValue",
			get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				x, err := ev(r, this, "returnValue")
				if err != nil {
					return undefined, err
				}
				return engine.Bool(!x.canceled), nil
			},
			set: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				x, err := ev(r, this, "returnValue")
				if err != nil {
					return undefined, err
				}
				if !engine.ToBoolean(engine.Arg(args, 0)) {
					cancel(x)
				}
				return undefined, nil
			}},
		act("preventDefault", cancel),
		get("defaultPrevented", func(x *event) engine.Value { return engine.Bool(x.canceled) }),
		get("composed", func(x *event) engine.Value { return engine.Bool(x.composed) }),
		get("isTrusted", func(x *event) engine.Value { return engine.Bool(x.trusted) }),
		get("timeStamp", func(x *event) engine.Value { return engine.NumberValue(x.timeStamp) }),
	}, consts...)
	return classSpec{
		name: "Event", length: 1, members: members, statics: consts,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			if len(args) == 0 {
				return undefined, r.TypeError("Failed to construct 'Event': 1 argument required, but only 0 present.")
			}
			typ, err := r.ToString(args[0])
			if err != nil {
				return undefined, err
			}
			x := &event{typ: engine.StringValue(typ), timeStamp: e.now()}
			if init := engine.Arg(args, 1); !init.IsNullish() {
				if !init.IsObject() {
					return undefined, r.TypeError("Failed to construct 'Event': The provided value is not of type 'EventInit'.")
				}
				for _, f := range []struct {
					name string
					dst  *bool
				}{{"bubbles", &x.bubbles}, {"cancelable", &x.cancelable}, {"composed", &x.composed}} {
					v, err := e.getName(init, f.name)
					if err != nil {
						return undefined, err
					}
					*f.dst = engine.ToBoolean(v)
				}
			}
			o, err := e.newObject(clsEvent, newTarget, x)
			if err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
	}
}

// --- EventTarget ----------------------------------------------------------------

// listener is an event listener of an EventTarget.
type listener struct {
	typ      string
	callback engine.Value // a function or an object with handleEvent
	capture  bool
	once     bool
	passive  bool
	removed  bool
	// handler marks the listener of an event handler attribute (onabort),
	// which calls the attribute's current value.
	handler bool
}

// eventTarget is the state of an EventTarget.
type eventTarget struct {
	listeners []*listener
	// handlers are the values of the event handler attributes, by type.
	handlers map[string]engine.Value
}

// target is implemented by the states of EventTarget and its subclasses.
type target interface{ targetState() *eventTarget }

func (t *eventTarget) targetState() *eventTarget { return t }

// hasListeners reports whether a listener of typ is registered.
func (t *eventTarget) hasListeners(typ string) bool {
	for _, l := range t.listeners {
		if l.typ == typ && !l.removed {
			return true
		}
	}
	return false
}

func (t *eventTarget) remove(l *listener) {
	l.removed = true
	for i, x := range t.listeners {
		if x == l {
			t.listeners = append(t.listeners[:i:i], t.listeners[i+1:]...)
			return
		}
	}
}

func (e *env) eventTargetSpec() classSpec {
	tgt := func(r *engine.Realm, this engine.Value, name string) (*eventTarget, error) {
		t, err := thisAs[target](r, this, "EventTarget.prototype."+name)
		if err != nil {
			return nil, err
		}
		return t.targetState(), nil
	}
	return classSpec{
		name: "EventTarget", length: 0,
		ctor: func(r *engine.Realm, _ []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			t := &eventTarget{}
			o, err := e.newObject(clsEventTarget, newTarget, t)
			if err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "addEventListener", length: 2, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				t, err := tgt(r, this, "addEventListener")
				if err != nil {
					return undefined, err
				}
				err = e.addEventListener(t, args)
				listenersChanged(this)
				return undefined, err
			}},
			{name: "removeEventListener", length: 2, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				t, err := tgt(r, this, "removeEventListener")
				if err != nil {
					return undefined, err
				}
				typ, err := e.goString(engine.Arg(args, 0))
				if err != nil {
					return undefined, err
				}
				capture, err := e.captureOption(engine.Arg(args, 2))
				if err != nil {
					return undefined, err
				}
				cb := engine.Arg(args, 1)
				for _, l := range t.listeners {
					if !l.handler && l.typ == typ && l.capture == capture && engine.SameValue(l.callback, cb) {
						t.remove(l)
						break
					}
				}
				listenersChanged(this)
				return undefined, nil
			}},
			{name: "dispatchEvent", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				t, err := tgt(r, this, "dispatchEvent")
				if err != nil {
					return undefined, err
				}
				x, err := thisAs[*event](r, engine.Arg(args, 0), "EventTarget.prototype.dispatchEvent")
				if err != nil {
					return undefined, r.TypeError("Failed to execute 'dispatchEvent' on 'EventTarget': parameter 1 is not of type 'Event'.")
				}
				if x.dispatching {
					return undefined, r.Throw(e.domException("InvalidStateError", "The event is already being dispatched."))
				}
				x.trusted = false
				ok, err := e.dispatch(t, this, engine.Arg(args, 0), x)
				return engine.Bool(ok), err
			}},
		},
	}
}

// listenersChanged tells a signal whose abort listeners may have changed.
func listenersChanged(this engine.Value) {
	if s, ok := this.AsObject().Internal().(*abortSignal); ok {
		s.hold()
	}
}

// captureOption reads the capture of an options argument: a boolean, or a
// dictionary's capture member.
func (e *env) captureOption(v engine.Value) (bool, error) {
	if !v.IsObject() {
		return engine.ToBoolean(v), nil
	}
	c, err := e.getName(v, "capture")
	return engine.ToBoolean(c), err
}

// addEventListener implements EventTarget.prototype.addEventListener.
func (e *env) addEventListener(t *eventTarget, args []engine.Value) error {
	r := e.r
	typ, err := e.goString(engine.Arg(args, 0))
	if err != nil {
		return err
	}
	cb := engine.Arg(args, 1)
	l := &listener{typ: typ, callback: cb}
	var signal *abortSignal
	if opts := engine.Arg(args, 2); opts.IsObject() {
		for _, f := range []struct {
			name string
			dst  *bool
		}{{"capture", &l.capture}, {"once", &l.once}, {"passive", &l.passive}} {
			v, err := e.getName(opts, f.name)
			if err != nil {
				return err
			}
			*f.dst = engine.ToBoolean(v)
		}
		sv, err := e.get(opts, e.keys.signal)
		if err != nil {
			return err
		}
		if !sv.IsUndefined() {
			s, ok := sv.IsObject(), false
			if s {
				signal, ok = sv.AsObject().Internal().(*abortSignal)
			}
			if !ok {
				return r.TypeError("Failed to execute 'addEventListener' on 'EventTarget': member signal is not of type 'AbortSignal'.")
			}
		}
	} else {
		l.capture = engine.ToBoolean(opts)
	}
	if signal != nil && signal.aborted {
		return nil
	}
	if cb.IsNullish() {
		return nil
	}
	if !cb.IsObject() {
		return r.TypeError("Failed to execute 'addEventListener' on 'EventTarget': parameter 2 is not of type 'Object'.")
	}
	for _, x := range t.listeners {
		if !x.handler && x.typ == typ && x.capture == l.capture && engine.SameValue(x.callback, cb) {
			return nil
		}
	}
	// The listener keeps its type, a Go string.
	if err := r.ChargeMemory(len(typ)); err != nil {
		return err
	}
	t.listeners = append(t.listeners, l)
	if signal != nil {
		signal.addAlgorithm(func(engine.Value) error {
			t.remove(l)
			return nil
		}, cb)
	}
	return nil
}

// dispatch dispatches the event x (object ev) at the target t (object
// this) and reports whether it was not canceled. What a listener throws is
// reported to the loop, not thrown; dispatch returns only an interrupt.
func (e *env) dispatch(t *eventTarget, this, ev engine.Value, x *event) (bool, error) {
	x.dispatching = true
	x.target, x.currentTarget, x.phase = this, this, phaseAtTarget
	defer func() {
		x.dispatching = false
		x.currentTarget, x.phase = engine.Null(), phaseNone
		x.stop, x.stopNow = false, false
	}()
	typ := x.typ.AsString().GoString()
	ls := append([]*listener(nil), t.listeners...)
	for _, l := range ls {
		if l.removed || l.typ != typ {
			continue
		}
		if l.once {
			t.remove(l)
		}
		cb := l.callback
		if l.handler {
			cb = t.handlers[typ]
			if !engine.IsCallable(cb) {
				continue
			}
		}
		x.inPassive = l.passive
		var err error
		if engine.IsCallable(cb) {
			_, err = e.r.Call(cb, this, []engine.Value{ev})
		} else {
			var h engine.Value
			if h, err = e.get(cb, e.keys.handleEvent); err == nil {
				if engine.IsCallable(h) {
					_, err = e.r.Call(h, cb, []engine.Value{ev})
				} else {
					err = e.r.TypeError("The handleEvent of an event listener is not a function")
				}
			}
		}
		x.inPassive = false
		if err != nil {
			if err := e.report(err); err != nil {
				return false, err
			}
		}
		if x.stopNow {
			break
		}
	}
	return !x.canceled, nil
}

// report hands what a callback threw, which no caller catches, to the loop,
// which handles it as a callback's exception: Options.OnError, or the
// loop stops. It returns an interrupt, which stops the caller.
func (e *env) report(err error) error {
	if _, ok := e.r.ThrownValue(err); !ok {
		return err
	}
	e.post(func() error { return err })
	return nil
}

// setHandler sets the event handler attribute typ of t to v.
func (t *eventTarget) setHandler(typ string, v engine.Value) {
	if !v.IsObject() {
		v = engine.Null()
	}
	old, had := t.handlers[typ]
	if t.handlers == nil {
		t.handlers = map[string]engine.Value{}
	}
	t.handlers[typ] = v
	active := had && !old.IsNull()
	switch {
	case v.IsNull() && active:
		for _, l := range t.listeners {
			if l.handler && l.typ == typ {
				t.remove(l)
				break
			}
		}
	case !v.IsNull() && !active:
		t.listeners = append(t.listeners, &listener{typ: typ, handler: true})
	}
}

// handler returns the event handler attribute typ of t.
func (t *eventTarget) handler(typ string) engine.Value {
	if v, ok := t.handlers[typ]; ok {
		return v
	}
	return engine.Null()
}

// fire fires a trusted event of type typ at t (object this), creating the
// Event only when a listener waits for it.
func (e *env) fire(t *eventTarget, this engine.Value, typ string) error {
	if !t.hasListeners(typ) {
		return nil
	}
	x := &event{typ: str(typ), trusted: true, timeStamp: e.now()}
	o, _ := e.newObject(clsEvent, nil, x)
	_, err := e.dispatch(t, this, engine.ObjectValue(o), x)
	return err
}

// --- AbortSignal ----------------------------------------------------------------

// abortSignal is the state of an AbortSignal.
type abortSignal struct {
	eventTarget
	obj     *engine.Object
	aborted bool
	reason  engine.Value
	// algorithms are the abort algorithms, run in order on abort; removed
	// counts those removed and still in the list.
	algorithms []*abortAlgorithm
	removed    int
	// dependents are the signals of AbortSignal.any that follow this one,
	// held weakly as the specification has it: one JavaScript dropped is
	// collected, and pruned from the list (live counts them at the last
	// prune). sources are the signals a dependent one follows.
	dependents []weak.Pointer[abortSignal]
	live       int
	sources    []*abortSignal
	dependent  bool
	// busy holds the dependents that have abort algorithms or abort
	// listeners, which must not be collected while this signal may still
	// abort; held is set on a dependent its sources hold so.
	busy map[*abortSignal]struct{}
	held bool
}

// newSignal creates an AbortSignal.
func (e *env) newSignal() *abortSignal {
	s := &abortSignal{reason: undefined}
	s.obj, _ = e.newObject(clsAbortSignal, nil, s)
	return s
}

// abortAlgorithm is an abort algorithm; f is nil once removed.
type abortAlgorithm struct {
	f func(reason engine.Value) error
	// keep is what only f's closure keeps, which the signal reports
	// while f may run (abortSignal.ReportLive), or undefined.
	keep engine.Value
}

// addAlgorithm adds an abort algorithm and returns the function that
// removes it. A signal that outlives many fetches drops the removed ones
// once they are the majority.
func (s *abortSignal) addAlgorithm(f func(engine.Value) error, keep engine.Value) (remove func()) {
	a := &abortAlgorithm{f, keep}
	s.algorithms = append(s.algorithms, a)
	s.hold()
	return func() {
		if a.f == nil {
			return
		}
		a.f = nil
		if s.removed++; s.removed > 16 && 2*s.removed > len(s.algorithms) {
			s.algorithms = slices.DeleteFunc(s.algorithms, func(a *abortAlgorithm) bool { return a.f == nil })
			s.removed = 0
		}
		s.hold()
	}
}

// hold makes the sources of a dependent signal hold it while it has abort
// algorithms or abort listeners, and only then: what waits for its abort
// keeps it, as the specification's garbage collection rule does.
func (s *abortSignal) hold() {
	if !s.dependent {
		return
	}
	want := !s.aborted && (len(s.algorithms) > s.removed || s.hasListeners("abort"))
	if want == s.held {
		return
	}
	s.held = want
	for _, src := range s.sources {
		if want {
			if src.busy == nil {
				src.busy = map[*abortSignal]struct{}{}
			}
			src.busy[s] = struct{}{}
		} else {
			delete(src.busy, s)
		}
	}
}

// follow makes the dependent signal d follow s.
func (s *abortSignal) follow(d *abortSignal) {
	if n := len(s.dependents); n >= 16 && n >= 2*s.live {
		s.dependents = slices.DeleteFunc(s.dependents, func(w weak.Pointer[abortSignal]) bool { return w.Value() == nil })
		s.live = len(s.dependents)
	}
	s.dependents = append(s.dependents, weak.Make(d))
}

// signalAbort implements "signal abort": reason undefined is an
// AbortError.
func (e *env) signalAbort(s *abortSignal, reason engine.Value) error {
	if s.aborted {
		return nil
	}
	if reason.IsUndefined() {
		reason = e.abortError()
	}
	s.aborted, s.reason = true, reason
	var deps []*abortSignal
	for _, w := range s.dependents {
		if d := w.Value(); d != nil && !d.aborted {
			d.aborted, d.reason = true, reason
			deps = append(deps, d)
		}
	}
	s.dependents, s.live, s.busy = nil, 0, nil
	for _, d := range deps {
		d.hold()
	}
	if err := e.runAbortSteps(s); err != nil {
		return err
	}
	for _, d := range deps {
		if err := e.runAbortSteps(d); err != nil {
			return err
		}
	}
	return nil
}

// runAbortSteps runs the signal's abort algorithms and fires abort.
func (e *env) runAbortSteps(s *abortSignal) error {
	algs := s.algorithms
	s.algorithms, s.removed = nil, 0
	for _, a := range algs {
		if f := a.f; f != nil {
			a.f = nil
			if err := f(s.reason); err != nil {
				return err
			}
		}
	}
	return e.fire(&s.eventTarget, engine.ObjectValue(s.obj), "abort")
}

// thisSignal returns the AbortSignal state of this.
func thisSignal(r *engine.Realm, this engine.Value, name string) (*abortSignal, error) {
	return thisAs[*abortSignal](r, this, "AbortSignal.prototype."+name)
}

func (e *env) abortSignalSpec() classSpec {
	return classSpec{
		name: "AbortSignal", parent: clsEventTarget, hasBase: true,
		members: []member{
			{name: "aborted", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := thisSignal(r, this, "aborted")
				if err != nil {
					return undefined, err
				}
				return engine.Bool(s.aborted), nil
			}},
			{name: "reason", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := thisSignal(r, this, "reason")
				if err != nil {
					return undefined, err
				}
				return s.reason, nil
			}},
			{name: "throwIfAborted", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := thisSignal(r, this, "throwIfAborted")
				if err != nil {
					return undefined, err
				}
				if s.aborted {
					return undefined, r.Throw(s.reason)
				}
				return undefined, nil
			}},
			{name: "onabort",
				get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
					s, err := thisSignal(r, this, "onabort")
					if err != nil {
						return undefined, err
					}
					return s.handler("abort"), nil
				},
				set: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
					s, err := thisSignal(r, this, "onabort")
					if err != nil {
						return undefined, err
					}
					s.setHandler("abort", engine.Arg(args, 0))
					s.hold()
					return undefined, nil
				}},
		},
		statics: []member{
			{name: "abort", fn: func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
				s := e.newSignal()
				s.aborted, s.reason = true, engine.Arg(args, 0)
				if s.reason.IsUndefined() {
					s.reason = e.abortError()
				}
				return engine.ObjectValue(s.obj), nil
			}},
			{name: "timeout", length: 1, fn: func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
				ms, err := r.ToNumber(engine.Arg(args, 0))
				if err != nil {
					return undefined, err
				}
				if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 || ms > 1<<53-1 {
					return undefined, r.TypeError("Failed to execute 'timeout' on 'AbortSignal': Value is outside the 'unsigned long long' value range.")
				}
				s := e.newSignal()
				// The timer does not keep the loop alive, as a browser's
				// does not keep a page alive: what waits on the signal
				// does. Close stops it. A delay past what a Duration
				// holds (292 years) is the longest one.
				d := time.Duration(math.MaxInt64)
				if ms < float64(math.MaxInt64/int64(time.Millisecond)) {
					d = time.Duration(math.Trunc(ms)) * time.Millisecond
				}
				// The timer keeps the signal, which no object may lead to.
				e.live.add(e, s)
				e.startTimer(d, func() {
					e.post(func() error {
						e.live.remove(s)
						return e.signalAbort(s, e.domException("TimeoutError", "The operation was aborted due to timeout"))
					})
				})
				return engine.ObjectValue(s.obj), nil
			}},
			{name: "any", length: 1, fn: func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
				var signals []*abortSignal
				err := e.iterate(engine.Arg(args, 0), func(v engine.Value) error {
					s, err := thisAs[*abortSignal](r, v, "AbortSignal.any")
					if err != nil {
						return r.TypeError("Failed to execute 'any' on 'AbortSignal': Failed to convert value to 'AbortSignal'.")
					}
					signals = append(signals, s)
					return nil
				})
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(e.anySignal(signals).obj), nil
			}},
		},
	}
}

// startTimer runs f after d on a timer of its own, which Close stops; f
// does not run after Close.
func (e *env) startTimer(d time.Duration, f func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx != nil && e.ctx.Err() != nil {
		return
	}
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		e.mu.Lock()
		_, ok := e.timers[t]
		delete(e.timers, t)
		e.mu.Unlock()
		if ok {
			f()
		}
	})
	if e.timers == nil {
		e.timers = map[*time.Timer]bool{}
	}
	e.timers[t] = true
}

// anySignal implements "create a dependent abort signal": the new signal
// follows the signals, or the sources of those that are dependent.
func (e *env) anySignal(signals []*abortSignal) *abortSignal {
	res := e.newSignal()
	for _, s := range signals {
		if s.aborted {
			res.aborted, res.reason = true, s.reason
			return res
		}
	}
	res.dependent = true
	follow := func(src *abortSignal) {
		for _, x := range res.sources {
			if x == src {
				return
			}
		}
		res.sources = append(res.sources, src)
		src.follow(res)
	}
	for _, s := range signals {
		if !s.dependent {
			follow(s)
			continue
		}
		for _, src := range s.sources {
			follow(src)
		}
	}
	return res
}

func (e *env) abortControllerSpec() classSpec {
	thisCtl := func(r *engine.Realm, this engine.Value, name string) (*abortSignal, error) {
		c, err := thisAs[*abortController](r, this, "AbortController.prototype."+name)
		if err != nil {
			return nil, err
		}
		return c.signal, nil
	}
	return classSpec{
		name: "AbortController",
		ctor: func(r *engine.Realm, _ []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			c := &abortController{signal: e.newSignal()}
			o, err := e.newObject(clsAbortController, newTarget, c)
			if err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "signal", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				s, err := thisCtl(r, this, "signal")
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(s.obj), nil
			}},
			{name: "abort", fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				s, err := thisCtl(r, this, "abort")
				if err != nil {
					return undefined, err
				}
				return undefined, e.signalAbort(s, engine.Arg(args, 0))
			}},
		},
	}
}

// abortController is the state of an AbortController.
type abortController struct{ signal *abortSignal }

// --- performance ----------------------------------------------------------------

// now is the time since Install in milliseconds, the clock of
// performance.now and Event.timeStamp.
func (e *env) now() float64 {
	return float64(time.Since(e.epoch)) / float64(time.Millisecond)
}

func (e *env) performanceSpec() classSpec {
	origin := float64(e.epoch.UnixNano()) / float64(time.Millisecond)
	return classSpec{
		name: "Performance", parent: clsEventTarget, hasBase: true,
		members: []member{
			{name: "now", fn: func(*engine.Realm, engine.Value, []engine.Value) (engine.Value, error) {
				return engine.NumberValue(e.now()), nil
			}},
			{name: "timeOrigin", get: func(*engine.Realm, engine.Value, []engine.Value) (engine.Value, error) {
				return engine.NumberValue(origin), nil
			}},
			{name: "toJSON", fn: func(r *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
				o := r.NewObject()
				o.DefineOwnDataFast(r, r.KeyFromGoString("timeOrigin"), engine.NumberValue(origin), attrMethod)
				return engine.ObjectValue(o), nil
			}},
		},
	}
}

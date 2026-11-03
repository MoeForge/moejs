package eventloop

import (
	"container/heap"
	"math"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// maxDelay is Node's TIMEOUT_MAX, the longest delay in milliseconds: a
// longer one, NaN or one below 1 is 1.
const maxDelay = 1<<31 - 1

// timer is the state of a Timeout object, of setTimeout or setInterval.
// The loop's mutex guards its mutable fields.
type timer struct {
	loop   *Loop
	id     int64
	fn     moejs.Value // undefined once cleared
	this   moejs.Value // the Timeout object
	args   []moejs.Value
	delay  int64 // ns
	repeat bool
	when   int64 // the loop's clock when due
	seq    uint64
	index  int  // in the heap, or -1
	ref    bool // hasRef
	// active is true while the timer is scheduled or, for an interval, its
	// callback runs; a ref'd active timer keeps the loop alive.
	active     bool
	cleared    bool // by clearTimeout, clearInterval or close
	registered bool // in the loop's ids, its id read by Symbol.toPrimitive
}

// immediate is the state of an Immediate object, of setImmediate.
type immediate struct {
	loop    *Loop
	fn      moejs.Value
	this    moejs.Value // the Immediate object
	args    []moejs.Value
	pending bool // set and neither run nor cleared
	ref     bool // hasRef: false once run or cleared
}

// timerHeap orders the scheduled timers by expiry, then by scheduling.
type timerHeap []*timer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if h[i].when != h[j].when {
		return h[i].when < h[j].when
	}
	return h[i].seq < h[j].seq
}
func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *timerHeap) Push(x any) {
	t := x.(*timer)
	t.index = len(*h)
	*h = append(*h, t)
}
func (h *timerHeap) Pop() any {
	old := *h
	t := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	t.index = -1
	return t
}

// pop removes and returns the first timer.
func (h *timerHeap) pop() *timer { return heap.Pop(h).(*timer) }

// remove removes t, which is in the heap.
func (h *timerHeap) remove(t *timer) { heap.Remove(h, t.index) }

// push schedules t, which is not in the heap, at t.when, after the timers
// scheduled before it for the same time. l.mu is held.
func (l *Loop) push(t *timer) {
	l.seq++
	t.seq = l.seq
	heap.Push(&l.timers, t)
	if !t.active {
		t.active = true
		if t.ref {
			l.refs++
		}
	}
}

// deactivate ends t's active period: it fired, as a timeout, or was
// cleared. l.mu is held.
func (l *Loop) deactivate(t *timer) {
	if t.active {
		t.active = false
		if t.ref {
			l.refs--
		}
	}
	if t.registered {
		t.registered = false
		delete(l.ids, t.id)
	}
}

// clearTimer is clearTimeout of t. l.mu is held.
func (l *Loop) clearTimer(t *timer) {
	if t.cleared || l.terminated {
		return
	}
	t.cleared = true
	if t.index >= 0 {
		l.timers.remove(t)
	}
	l.deactivate(t)
	t.fn, t.args = moejs.Undefined(), nil
}

// settleImmediate marks im run or cleared. l.mu is held.
func (l *Loop) settleImmediate(im *immediate) {
	im.pending = false
	if im.ref {
		im.ref = false
		l.refs--
	}
	im.fn, im.args = moejs.Undefined(), nil
}

// install sets the timer functions as globals of the runtime.
func (l *Loop) install() error {
	for _, g := range []struct {
		name   string
		length int
		fn     moejs.NativeFunc
	}{
		{"setTimeout", 5, l.setTimeout},
		{"clearTimeout", 1, l.clearTimeout},
		{"setInterval", 5, l.setInterval},
		{"clearInterval", 1, l.clearTimeout},
		{"setImmediate", 4, l.setImmediate},
		{"clearImmediate", 1, l.clearImmediate},
	} {
		if err := l.rt.SetGlobal(g.name, l.rt.Function(g.name, g.length, g.fn)); err != nil {
			return err
		}
	}
	return nil
}

// setTimeout implements setTimeout(callback, delay, ...args).
func (l *Loop) setTimeout(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	return l.setTimer(r, "setTimeout", args, false)
}

// setInterval implements setInterval(callback, delay, ...args).
func (l *Loop) setInterval(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	return l.setTimer(r, "setInterval", args, true)
}

// setTimer creates and schedules a Timeout, as Node's setTimeout and
// setInterval do: a callback that is not a function is a TypeError, and
// the delay goes through ToNumber and is truncated.
func (l *Loop) setTimer(r *moejs.Realm, name string, args []moejs.Value, repeat bool) (moejs.Value, error) {
	fn := moejs.Arg(args, 0)
	if !engine.IsCallable(fn) {
		return moejs.Undefined(), callbackError(r, fn)
	}
	ms, err := r.ToNumber(moejs.Arg(args, 1))
	if err != nil {
		return moejs.Undefined(), err
	}
	if !(ms >= 1 && ms <= maxDelay) {
		ms = 1
	}
	// Node's insert truncates the delay to whole milliseconds.
	t := &timer{loop: l, fn: fn, delay: int64(ms) * int64(time.Millisecond), repeat: repeat, index: -1, ref: true}
	if len(args) > 2 {
		t.args = slices.Clone(args[2:])
	}
	o := r.NewObjectWithProto(l.timeoutPrototype())
	o.SetInternal(t)
	t.this = engine.ObjectValue(o)
	// Nothing allocates for JavaScript with l.mu held (roots).
	l.mu.Lock()
	if l.terminated {
		l.mu.Unlock()
		return moejs.Undefined(), terminatedError(r, name)
	}
	l.lastID++
	t.id = l.lastID
	t.when = l.now() + t.delay
	l.push(t)
	l.mu.Unlock()
	return t.this, nil
}

// clearTimeout implements clearTimeout and clearInterval: the argument is a
// Timeout or the id its Symbol.toPrimitive returned, as a number or a
// string; anything else is ignored.
func (l *Loop) clearTimeout(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	v := moejs.Arg(args, 0)
	l.mu.Lock()
	defer l.mu.Unlock()
	var t *timer
	switch {
	case v.IsObject():
		t = l.timerOf(v)
	case v.IsNumber():
		if f := v.AsNumber(); f == math.Trunc(f) && f >= 1 && f <= float64(l.lastID) {
			t = l.ids[int64(f)]
		}
	case v.IsString():
		s := v.AsString().GoString()
		if id, err := strconv.ParseInt(s, 10, 64); err == nil && strconv.FormatInt(id, 10) == s {
			t = l.ids[id]
		}
	}
	if t != nil {
		l.clearTimer(t)
	}
	return moejs.Undefined(), nil
}

// setImmediate implements setImmediate(callback, ...args).
func (l *Loop) setImmediate(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	fn := moejs.Arg(args, 0)
	if !engine.IsCallable(fn) {
		return moejs.Undefined(), callbackError(r, fn)
	}
	im := &immediate{loop: l, fn: fn, pending: true, ref: true}
	if len(args) > 1 {
		im.args = slices.Clone(args[1:])
	}
	o := r.NewObjectWithProto(l.immediatePrototype())
	o.SetInternal(im)
	im.this = engine.ObjectValue(o)
	// Nothing allocates for JavaScript with l.mu held (roots).
	l.mu.Lock()
	if l.terminated {
		l.mu.Unlock()
		return moejs.Undefined(), terminatedError(r, "setImmediate")
	}
	l.immediates = append(l.immediates, im)
	l.refs++
	l.mu.Unlock()
	return im.this, nil
}

// clearImmediate implements clearImmediate: the argument is an Immediate;
// anything else is ignored.
func (l *Loop) clearImmediate(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if im := l.immediateOf(moejs.Arg(args, 0)); im != nil && im.pending && !l.terminated {
		l.settleImmediate(im)
	}
	return moejs.Undefined(), nil
}

// timerOf returns the timer of v when it is a Timeout of this loop.
func (l *Loop) timerOf(v moejs.Value) *timer {
	if !v.IsObject() {
		return nil
	}
	t, _ := v.AsObject().Internal().(*timer)
	if t == nil || t.loop != l {
		return nil
	}
	return t
}

// immediateOf returns the immediate of v when it is an Immediate of this
// loop.
func (l *Loop) immediateOf(v moejs.Value) *immediate {
	if !v.IsObject() {
		return nil
	}
	im, _ := v.AsObject().Internal().(*immediate)
	if im == nil || im.loop != l {
		return nil
	}
	return im
}

// timeoutPrototype returns the prototype of the Timeout objects, with
// Node's methods: ref, unref, hasRef, refresh, close and
// Symbol.toPrimitive.
func (l *Loop) timeoutPrototype() *moejs.Object {
	if l.timeoutProto != nil {
		return l.timeoutProto
	}
	r := l.rt.Realm()
	p := r.NewObject()
	method := func(key engine.PropertyKey, name string, length int, fn func(t *timer) moejs.Value) {
		l.defineMethod(p, key, name, length, func(r *moejs.Realm, this moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			t := l.timerOf(this)
			if t == nil {
				return moejs.Undefined(), r.TypeError("Timeout.prototype.%s called on an object that is not a Timeout", name)
			}
			l.mu.Lock()
			defer l.mu.Unlock()
			return fn(t), nil
		})
	}
	method(r.KeyFromGoString("ref"), "ref", 0, func(t *timer) moejs.Value {
		if !t.ref && !l.terminated {
			t.ref = true
			if t.active {
				l.refs++
			}
		}
		return t.this
	})
	method(r.KeyFromGoString("unref"), "unref", 0, func(t *timer) moejs.Value {
		if t.ref && !l.terminated {
			t.ref = false
			if t.active {
				l.refs--
			}
		}
		return t.this
	})
	method(r.KeyFromGoString("hasRef"), "hasRef", 0, func(t *timer) moejs.Value {
		return moejs.Bool(t.ref)
	})
	method(r.KeyFromGoString("refresh"), "refresh", 0, func(t *timer) moejs.Value {
		// A timeout that fired runs again; a cleared one does not.
		if t.cleared || l.terminated {
			return t.this
		}
		t.when = l.now() + t.delay
		if t.index >= 0 {
			l.timers.remove(t)
		}
		l.push(t)
		return t.this
	})
	method(r.KeyFromGoString("close"), "close", 0, func(t *timer) moejs.Value {
		l.clearTimer(t)
		return t.this
	})
	method(engine.SymbolKey(engine.SymToPrimitive), "[Symbol.toPrimitive]", 0, func(t *timer) moejs.Value {
		// Node: clearTimeout finds an active timer by the id this returns.
		if t.active && !t.registered && !l.terminated {
			if l.ids == nil {
				l.ids = make(map[int64]*timer)
			}
			l.ids[t.id] = t
			t.registered = true
		}
		return moejs.Int(t.id)
	})
	l.timeoutProto = p
	return p
}

// immediatePrototype returns the prototype of the Immediate objects, with
// Node's methods: ref, unref and hasRef.
func (l *Loop) immediatePrototype() *moejs.Object {
	if l.immediateProto != nil {
		return l.immediateProto
	}
	r := l.rt.Realm()
	p := r.NewObject()
	method := func(name string, fn func(im *immediate) moejs.Value) {
		l.defineMethod(p, r.KeyFromGoString(name), name, 0, func(r *moejs.Realm, this moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			im := l.immediateOf(this)
			if im == nil {
				return moejs.Undefined(), r.TypeError("Immediate.prototype.%s called on an object that is not an Immediate", name)
			}
			l.mu.Lock()
			defer l.mu.Unlock()
			return fn(im), nil
		})
	}
	// Node: an Immediate that ran or was cleared has no ref to change.
	method("ref", func(im *immediate) moejs.Value {
		if im.pending && !im.ref && !l.terminated {
			im.ref = true
			l.refs++
		}
		return im.this
	})
	method("unref", func(im *immediate) moejs.Value {
		if im.pending && im.ref && !l.terminated {
			im.ref = false
			l.refs--
		}
		return im.this
	})
	method("hasRef", func(im *immediate) moejs.Value {
		return moejs.Bool(im.ref)
	})
	l.immediateProto = p
	return p
}

// defineMethod defines a method on o as a class does: writable,
// configurable and not enumerable.
func (l *Loop) defineMethod(o *moejs.Object, key engine.PropertyKey, name string, length int, fn moejs.NativeFunc) {
	var d engine.PropertyDescriptor
	d.SetValue(l.rt.Function(name, length, fn))
	d.SetWritable(true)
	d.SetEnumerable(false)
	d.SetConfigurable(true)
	// A fresh ordinary object accepts any property.
	_, _ = o.DefineOwnProperty(l.rt.Realm(), key, d)
}

// callbackError is Node's TypeError for a callback that is not a function.
func callbackError(r *moejs.Realm, v moejs.Value) error {
	err := r.TypeError("The \"callback\" argument must be of type function. Received %s", received(v))
	if exc, ok := err.(*moejs.Exception); ok && exc.Value.IsObject() {
		_, _ = exc.Value.AsObject().CreateDataProperty(r, r.KeyFromGoString("code"), moejs.String("ERR_INVALID_ARG_TYPE"))
	}
	return err
}

// received describes v as the message of Node's ERR_INVALID_ARG_TYPE does,
// without running JavaScript.
func received(v moejs.Value) string {
	var typ string
	switch v.Type() {
	case engine.TypeUndefined, engine.TypeNull:
		return v.String()
	case engine.TypeObject:
		return "an instance of " + v.AsObject().ClassName()
	case engine.TypeString:
		s := v.String()
		if utf8.RuneCountInString(s) > 25 {
			s = string([]rune(s)[:25]) + "..."
		}
		return "type string ('" + s + "')"
	case engine.TypeBoolean:
		typ = "boolean"
	case engine.TypeNumber:
		typ = "number"
	case engine.TypeSymbol:
		typ = "symbol"
	case engine.TypeBigInt:
		return "type bigint (" + v.String() + "n)"
	}
	return "type " + typ + " (" + v.String() + ")"
}

// terminatedError is what the timer functions throw once Terminate ended
// the loop.
func terminatedError(r *moejs.Realm, name string) error {
	return &moejs.Exception{Value: engine.ObjectValue(r.NewError(engine.KindError, "%s: %s", name, ErrTerminated.Error()))}
}

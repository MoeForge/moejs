package engine

// The formatting is ported from V8's stack trace serialization
// (src/objects/call-site-info.cc and the functions it calls; BSD-3-Clause;
// see THIRD_PARTY_NOTICES).

import (
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MoeForge/moejs/bytecode"
)

// Stack traces in V8's format (Error.prototype.stack, Error.captureStackTrace).
//
// Capture runs when an error object is created and only copies pointers: for
// each live interpreter frame, innermost first, the function and the pc of
// its current instruction. The receiver of a frame and the native functions
// between frames are read off the caller's call instruction (Call,
// CallSpread, New, NewSpread, SuperCall, SuperCallSpread), whose callee and
// this registers are intact while the callee runs: a frame whose caller
// called it directly gets the caller's this (the method-call form
// "Type.method") or the construct flag ("new F"), and a native the caller
// called is a frame of its own ("Array.forEach (<anonymous>)") placed
// between the two. f.call, f.apply, Reflect.apply and Reflect.construct are
// looked through to the function they enter. Frames entered any other way
// (getters, conversions, callbacks of natives, the host) are plain calls.
//
// Formatting runs at the first read of `stack` and follows V8's
// SerializeJSStackFrame and AppendMethodCall (call-site-info.cc) over data
// properties only, so no user code runs.

// defaultStackTraceLimit is the initial value of Error.stackTraceLimit.
const defaultStackTraceLimit = 10

// StackFrame flags.
const (
	// frameConstruct marks a frame called with new.
	frameConstruct uint32 = 1 << iota
	// frameMethod marks a method call: Recv is the receiver.
	frameMethod
	// framePrimitive marks a primitive receiver: Recv is the prototype its
	// wrapper would have (nil for symbols) and the bits from
	// frameClassShift hold the wrapper's Class.
	framePrimitive

	frameClassShift = 8
)

// stackScratch is the room captureStackInto collects frames in on the Go
// stack before it copies them out; longer traces (Error.stackTraceLimit
// above it) spill to the heap.
const stackScratch = 16

// captureStack records the live frames without skipping or buffer reuse.
func captureStack(r *Realm) []StackFrame {
	limit, ok := r.stackTraceLimit()
	if !ok {
		return nil
	}
	return captureStackInto(r, nil, nil, limit)
}

// captureStackInto records up to limit frames of the live stack, innermost
// first, into buf when it has the capacity (the error object's inline
// buffer), so a shallow throw allocates nothing for its stack. With skip
// set, the frames up to and including the innermost frame of skip are left
// out (V8's SKIP_UNTIL_SEEN: the Error constructor or new.target, the
// function given to Error.captureStackTrace); when skip is not on the stack
// the trace is empty for a bytecode skip, as in V8, and complete for a native
// one (a constructor the host called).
func captureStackInto(r *Realm, buf []StackFrame, skip *Object, limit int) []StackFrame {
	st := &r.interp
	n := st.nframes
	if n == 0 || limit <= 0 {
		return nil
	}
	w := stackWalk{skip: skip, limit: limit}
	if callee, recv, flags, ok := r.callSite(n - 1); ok && callee.internal.(*FunctionData).isNative() {
		w.add(StackFrame{Fn: callee, Recv: recv, Flags: flags})
	}
	for j := n - 1; j >= 0 && !w.full(); j-- {
		fr := &st.frames[j]
		f := StackFrame{Fn: fr.fn, PC: fr.pc}
		var native StackFrame
		if j > 0 {
			if callee, recv, flags, ok := r.callSite(j - 1); ok {
				switch {
				case callee == fr.fn:
					f.Recv, f.Flags = recv, flags
				case callee.internal.(*FunctionData).isNative():
					native = StackFrame{Fn: callee, Recv: recv, Flags: flags}
				}
			}
		}
		if j < n-1 || !atCtorEntry(fr) {
			w.add(f) // V8 shows no frame for a class constructor called without new
		}
		if native.Fn != nil {
			w.add(native)
		}
	}
	if w.n == 0 || w.skip != nil && w.skip.class == ClassFunction && !w.skip.internal.(*FunctionData).isNative() {
		return nil
	}
	if cap(buf) < w.n {
		buf = make([]StackFrame, w.n)
	}
	buf = buf[:w.n]
	for i := range min(w.n, stackScratch) {
		buf[i] = w.buf[i]
	}
	if len(w.spill) != 0 {
		copy(buf[stackScratch:], w.spill)
	}
	return buf
}

// stackWalk collects the frames of one capture: the first stackScratch in
// buf (the walk lives on the Go stack), the rest in spill.
type stackWalk struct {
	buf   [stackScratch]StackFrame
	spill []StackFrame
	n     int
	skip  *Object // nil once seen
	limit int
}

func (w *stackWalk) add(f StackFrame) {
	if w.skip != nil && f.Fn == w.skip {
		w.skip, w.n, w.spill = nil, 0, w.spill[:0]
		return
	}
	switch {
	case w.n >= w.limit:
	case w.n < stackScratch:
		w.buf[w.n] = f
		w.n++
	default:
		w.spill = append(w.spill, f)
		w.n++
	}
}

// full reports whether no further frame can be kept.
func (w *stackWalk) full() bool { return w.skip == nil && w.n >= w.limit }

// callSite decodes the instruction frame i is executing when it is a call:
// the callee with bound functions unwrapped, and the receiver and flags of
// the callee's frame.
func (r *Realm) callSite(i int) (callee, recv *Object, flags uint32, ok bool) {
	st := &r.interp
	fr := &st.frames[i]
	if fr.fn == nil {
		return nil, nil, 0, false
	}
	code := fr.fn.internal.(*FunctionData).code
	if code == nil || int(fr.pc) >= len(code.Code) {
		return nil, nil, 0, false
	}
	w := code.Code[fr.pc]
	op := bytecode.Op(w)
	construct := false
	switch op {
	case bytecode.Call, bytecode.CallSpread:
	case bytecode.New, bytecode.NewSpread, bytecode.SuperCall, bytecode.SuperCallSpread:
		construct = true
	default:
		return nil, nil, 0, false
	}
	a := int(fr.base) + int(uint8(w>>8))
	if a+1 >= len(st.stack) || !st.stack[a].IsObject() {
		return nil, nil, 0, false
	}
	callee = st.stack[a].AsObject()
	fd := callee.FunctionData()
	if fd == nil {
		return nil, nil, 0, false
	}
	this := st.stack[a+1]
	if op == bytecode.Call && w>>24 == bytecode.CallUndefThis {
		this = Undefined()
	}
	if op == bytecode.Call && r.isCallOrApply(callee, fd) {
		// f.call(t, ...) and f.apply(t, args) enter f with this t, and V8
		// shows no frame of their own.
		if !this.IsObject() || this.AsObject().FunctionData() == nil {
			return nil, nil, 0, false
		}
		callee = this.AsObject()
		fd = callee.internal.(*FunctionData)
		this = Undefined()
		if uint8(w>>16) > 0 && a+2 < len(st.stack) {
			this = st.stack[a+2]
		}
	}
	if op == bytecode.Call && r.isReflectCall(callee, fd) {
		// Reflect.apply(f, t, args) enters f with this t and
		// Reflect.construct(F, args) constructs F; V8 shows no frame of
		// their own either, not even when they throw.
		construct = fd.name == AtomConstruct
		argc := int(uint8(w >> 16))
		if argc == 0 || a+2 >= len(st.stack) || !st.stack[a+2].IsObject() || st.stack[a+2].AsObject().FunctionData() == nil {
			return nil, nil, 0, false
		}
		callee = st.stack[a+2].AsObject()
		fd = callee.internal.(*FunctionData)
		this = Undefined()
		if argc > 1 && a+3 < len(st.stack) {
			this = st.stack[a+3]
		}
	}
	for fd.kind == FuncBound {
		callee, this = fd.bound().target, fd.thisValue
		fd = callee.internal.(*FunctionData)
	}
	if construct {
		if !fd.IsConstructor() {
			return nil, nil, 0, false // the New itself throws "is not a constructor"
		}
		return callee, nil, frameConstruct, true
	}
	recv, flags = r.stackReceiver(this)
	return callee, recv, flags, true
}

// isReflectCall reports whether fn is the realm's Reflect.apply or
// Reflect.construct.
func (r *Realm) isReflectCall(fn *Object, fd *FunctionData) bool {
	if fd.kind != FuncNative || fd.name != AtomApply && fd.name != AtomConstruct {
		return false
	}
	ns, ok := r.Global.GetOwnDataValue(StringKey(AtomReflect))
	if !ok || !ns.IsObject() {
		return false
	}
	v, ok := ns.AsObject().GetOwnDataValue(StringKey(fd.name))
	return ok && v.IsObject() && v.AsObject() == fn
}

// atCtorEntry reports whether frame fr is at the CtorEntry of a class
// constructor, which throws when the class is called without new.
func atCtorEntry(fr *frameInfo) bool {
	code := fr.fn.internal.(*FunctionData).code
	return code != nil && int(fr.pc) < len(code.Code) && bytecode.Op(code.Code[fr.pc]) == bytecode.CtorEntry
}

// isNative reports whether fd is a Go function, plain or with a payload
// (host bindings): the stack shows both as native frames.
func (fd *FunctionData) isNative() bool { return fd.kind == FuncNative || fd.kind == FuncNativeData }

// isCallOrApply reports whether fn is the realm's Function.prototype.call or
// apply.
func (r *Realm) isCallOrApply(fn *Object, fd *FunctionData) bool {
	if fd.kind != FuncNative || fd.name != AtomCall && fd.name != AtomApply {
		return false
	}
	v, ok := r.FunctionPrototype.GetOwnDataValue(StringKey(fd.name))
	return ok && v.IsObject() && v.AsObject() == fn
}

// stackReceiver classifies the this of a call: undefined, null and the
// global object make a top-level call (V8's IsToplevel), anything else a
// method call.
func (r *Realm) stackReceiver(v Value) (*Object, uint32) {
	var proto *Object
	var class Class
	switch {
	case v.IsObject():
		if o := v.AsObject(); o != r.Global {
			return o, frameMethod
		}
		return nil, 0
	case v.IsString():
		proto, class = r.StringPrototype, ClassString
	case v.IsNumber():
		proto, class = r.NumberPrototype, ClassNumber
	case v.IsBool():
		proto, class = r.BooleanPrototype, ClassBoolean
	case v.IsBigInt():
		r.lateAt(lateBigInt)
		proto, class = r.BigIntPrototype, ClassBigInt
	case v.IsSymbol():
		class = ClassSymbol
	default:
		return nil, 0
	}
	return proto, frameMethod | framePrimitive | uint32(class)<<frameClassShift
}

// stackTraceLimit returns the frame limit of a new trace: Error.
// stackTraceLimit read as V8 does (a data property; any value but a Number
// means no trace at all), truncated and clamped at 0. Shared intrinsics are
// frozen, so their limit is always the default.
func (r *Realm) stackTraceLimit() (int, bool) {
	ctor := r.errorCtors[KindError]
	if r.sharedIntrinsics || ctor == nil {
		return defaultStackTraceLimit, true
	}
	// The property is installed last, so unless the program has added
	// properties to Error since, it is the leaf of the shape and its slot is
	// read without a lookup.
	key := StringKey(AtomStackTraceLimit)
	var v Value
	var ok bool
	if s := ctor.shape; ctor.flags&(flagDict|flagHasLazy) == 0 && s.key == key && s.attrs&(attrAccessor|attrLazy) == 0 {
		v, ok = ctor.slots[s.slot], true
	} else {
		v, ok = lookupDataNoThrow(ctor, key)
	}
	if !ok || !v.IsNumber() {
		return 0, false
	}
	switch f := v.AsNumber(); {
	case f != f || f <= 0:
		return 0, true
	case f >= math.MaxInt32:
		return math.MaxInt32, true
	default:
		return int(f), true
	}
}

// formatStack renders captured frames as V8's "    at ..." lines separated
// by newlines, without a trailing newline.
func formatStack(r *Realm, frames []StackFrame) string {
	var b []byte
	var steps int
	for i, f := range frames {
		if i > 0 {
			b = append(b, '\n')
		}
		b = append(b, "    at "...)
		b = r.appendCallSite(b, f, &steps)
	}
	return string(b)
}

// FormatStack renders captured frames the way the `stack` property does.
func FormatStack(r *Realm, frames []StackFrame) string { return formatStack(r, frames) }

// appendCallSite is V8's SerializeJSStackFrame without async and eval
// frames.
func (r *Realm) appendCallSite(b []byte, f StackFrame, steps *int) []byte {
	name := stackFunctionName(f.Fn)
	switch {
	case f.Flags&frameConstruct != 0:
		b = append(b, "new "...)
		if name == "" {
			name = "<anonymous>"
		}
		b = append(b, name...)
	case f.Flags&frameMethod != 0:
		b = r.appendMethodCall(b, f, name, steps)
	case name != "":
		b = append(b, name...)
	default:
		return appendFrameLocation(b, f)
	}
	b = append(b, " ("...)
	b = appendFrameLocation(b, f)
	return append(b, ')')
}

// appendMethodCall is V8's AppendMethodCall.
func (r *Realm) appendMethodCall(b []byte, f StackFrame, fnName string, steps *int) []byte {
	typeName := r.stackTypeName(f)
	methodName := r.stackMethodName(f, steps)
	if fnName != "" {
		if typeName != "" && isIdentifierName(fnName) && fnName != typeName {
			b = append(b, typeName...)
			b = append(b, '.')
		}
		b = append(b, fnName...)
		if methodName != "" && !endsWithMethodName(fnName, methodName) {
			b = append(b, " [as "...)
			b = append(b, methodName...)
			b = append(b, ']')
		}
		return b
	}
	if typeName != "" {
		b = append(b, typeName...)
		b = append(b, '.')
	}
	if methodName == "" {
		methodName = "<anonymous>"
	}
	return append(b, methodName...)
}

// appendFrameLocation is V8's AppendFileLocation: "file:line:col", with
// "<anonymous>" for natives and unnamed sources.
func appendFrameLocation(b []byte, f StackFrame) []byte {
	code := f.Code()
	if code == nil || code.Source == nil || code.Source.Name == "" {
		b = append(b, "<anonymous>"...)
	} else {
		b = append(b, code.Source.Name...)
	}
	if code == nil {
		return b
	}
	if line, col := code.Position(f.PC); line > 0 {
		b = append(b, ':')
		b = strconv.AppendInt(b, int64(line), 10)
		b = append(b, ':')
		b = strconv.AppendInt(b, int64(col), 10)
	}
	return b
}

// stackFunctionName is V8's JSFunction::GetDebugName: the `name` data
// property when it is a string, else the function's initial name.
func stackFunctionName(fn *Object) string {
	if fn == nil {
		return ""
	}
	if v, ok := lookupDataNoThrow(fn, StringKey(AtomName)); ok && v.IsString() {
		return v.AsString().GoString()
	}
	return initialFunctionName(fn)
}

// initialFunctionName is the name a function was created with (V8's
// SharedFunctionInfo::DebugName).
func initialFunctionName(fn *Object) string {
	if fd := fn.FunctionData(); fd != nil && fd.name != nil {
		return fd.name.GoString()
	}
	return ""
}

// stackTypeName is V8's CallSiteInfo::GetTypeName for a method call.
func (r *Realm) stackTypeName(f StackFrame) string {
	if f.Flags&framePrimitive != 0 {
		return r.constructorName(f.Recv, nil, Class(f.Flags>>frameClassShift))
	}
	o := f.Recv
	if o.class == ClassFunction {
		if name := stackFunctionName(o); name != "" {
			return name
		}
	}
	return r.constructorName(o, o, o.class)
}

// constructorName is V8's JSReceiver::GetConstructorName over the chain from
// start, where self is the receiver itself (nil when it is a primitive's
// wrapper): the first @@toStringTag, else the first `constructor` of a
// prototype naming a function other than Object, else the class name.
// V8 first consults the constructor that created the object, which the
// engine does not record; for objects made by `new F` the chain gives the
// same F unless F.prototype.constructor was changed.
func (r *Realm) constructorName(start, self *Object, class Class) string {
	for obj := start; obj != nil; obj = obj.proto {
		if tag := r.intrinsicToStringTag(obj); tag != "" {
			return tag
		}
		if obj == self {
			continue
		}
		if v, ok := obj.GetOwnDataValue(StringKey(AtomConstructor)); ok && v.IsObject() {
			if fd := v.AsObject().FunctionData(); fd != nil && fd.kind != FuncBound {
				if name := initialFunctionName(v.AsObject()); name != "" && name != "Object" {
					return name
				}
			}
		}
	}
	return class.String()
}

// intrinsicToStringTag stands in for the own @@toStringTag of the intrinsics
// that have one in V8.
func (r *Realm) intrinsicToStringTag(o *Object) string {
	switch o {
	case r.JSON:
		return "JSON"
	case r.Math:
		return "Math"
	case r.ArrayIteratorPrototype:
		return "Array Iterator"
	}
	return ""
}

// stackMethodName is V8's CallSiteInfo::GetMethodName: the function's
// initial name (without a "get "/"set " prefix) when looking it up on the
// receiver finds the function, else the one enumerable string key on the
// receiver's chain holding it.
func (r *Realm) stackMethodName(f StackFrame, steps *int) string {
	fd := f.Fn.FunctionData()
	if fd == nil || f.Flags&framePrimitive != 0 && f.Recv == nil {
		return ""
	}
	name := initialFunctionName(f.Fn)
	if strings.HasPrefix(name, "get ") || strings.HasPrefix(name, "set ") {
		name = name[4:]
	}
	if name != "" {
		key := r.KeyFromString(FromGoString(name))
		for obj := f.Recv; obj != nil; obj = obj.proto {
			if r.stackStep(steps) {
				return ""
			}
			c, ok := obj.getOwnCell(key)
			if !ok {
				continue
			}
			if holdsFunction(c, f.Fn) {
				return name
			}
			break
		}
	}
	return r.inferMethodName(f.Recv, f.Fn, steps)
}

// inferMethodName is V8's InferMethodName: the enumerable string-keyed own
// properties of the chain from start whose value (or accessor) is fn; ""
// when there is none or two keys differ.
func (r *Realm) inferMethodName(start, fn *Object, steps *int) string {
	var found PropertyKey
	have := false
	for obj := start; obj != nil; obj = obj.proto {
		if obj.flags&flagDict == 0 {
			for i, p := range obj.shape.Props() {
				if r.stackStep(steps) {
					return ""
				}
				if p.attrs&(attrEnumerable|attrLazy) != attrEnumerable || !p.key.IsString() {
					continue
				}
				if holdsFunction(propCell{value: obj.slots[i], attrs: p.attrs}, fn) {
					if have && found != p.key {
						return ""
					}
					found, have = p.key, true
				}
			}
			continue
		}
		for i := range obj.dict.entries {
			if r.stackStep(steps) {
				return ""
			}
			e := &obj.dict.entries[i]
			if !e.live || e.cell.attrs&attrEnumerable == 0 || !e.key.IsString() {
				continue
			}
			if holdsFunction(e.cell, fn) {
				if have && found != e.key {
					return ""
				}
				found, have = e.key, true
			}
		}
	}
	if !have {
		return ""
	}
	return found.String().GoString()
}

// holdsFunction reports whether c is fn or an accessor with fn as getter or
// setter.
func holdsFunction(c propCell, fn *Object) bool {
	if c.attrs&attrAccessor != 0 {
		acc := c.value.asAccessor()
		return acc != nil && (acc.Get == fn || acc.Set == fn)
	}
	return c.value.IsObject() && c.value.AsObject() == fn
}

// stackStep counts one unit of formatting work and reports, every
// interruptStride units, whether the realm was interrupted, in which case
// the name inference gives up.
func (r *Realm) stackStep(steps *int) bool {
	*steps++
	return *steps%interruptStride == 0 && r.interrupted()
}

// isIdentifierName is V8's String::IsIdentifier.
func isIdentifierName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if c == '$' || c == '_' || c < utf8.RuneSelf && (c|0x20 >= 'a' && c|0x20 <= 'z' || i > 0 && c >= '0' && c <= '9') {
			continue
		}
		if c < utf8.RuneSelf {
			return false
		}
		start := unicode.IsLetter(c) || unicode.Is(unicode.Nl, c) || unicode.Is(unicode.Other_ID_Start, c)
		if i == 0 && !start {
			return false
		}
		if !start && !unicode.Is(unicode.Mn, c) && !unicode.Is(unicode.Mc, c) && !unicode.Is(unicode.Nd, c) &&
			!unicode.Is(unicode.Pc, c) && !unicode.Is(unicode.Other_ID_Continue, c) && c != 0x200C && c != 0x200D {
			return false
		}
	}
	return true
}

// endsWithMethodName is V8's StringEndsWithMethodName: subject is pattern or
// ends with "." or " " followed by it.
func endsWithMethodName(subject, pattern string) bool {
	if subject == pattern {
		return true
	}
	if len(subject) <= len(pattern) || !strings.HasSuffix(subject, pattern) {
		return false
	}
	c := subject[len(subject)-len(pattern)-1]
	return c == '.' || c == ' '
}

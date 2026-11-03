package engine

import (
	"fmt"
	"strconv"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// ErrorKind selects an Error constructor family.
type ErrorKind uint8

const (
	KindError ErrorKind = iota
	KindTypeError
	KindRangeError
	KindSyntaxError
	KindReferenceError
	KindEvalError
	KindURIError
	numErrorKinds
)

var errorKindNames = [numErrorKinds]*String{
	KindError:          AtomError,
	KindTypeError:      AtomTypeError,
	KindRangeError:     AtomRangeError,
	KindSyntaxError:    AtomSyntaxError,
	KindReferenceError: AtomReferenceError,
	KindEvalError:      AtomEvalError,
	KindURIError:       AtomURIError,
}

// Name returns the constructor name of the kind.
func (k ErrorKind) Name() *String { return errorKindNames[k] }

// Exception is a thrown JavaScript value travelling through Go error returns.
type Exception struct {
	Value Value
	Stack string
}

// Error renders the exception without running user code: "Name: message"
// for Error objects (or the string form of the value), followed by Stack
// when set.
func (e *Exception) Error() string {
	s := errorDisplayString(e.Value)
	if e.Stack != "" {
		return s + "\n" + e.Stack
	}
	return s
}

// String is Error.
func (e *Exception) String() string { return e.Error() }

// Name is the thrown value's `name` when it is a string data property (own
// or inherited, so "TypeError" for a TypeError), else "". It never runs a
// getter.
func (e *Exception) Name() string {
	if !e.Value.IsObject() {
		return ""
	}
	if v, ok := lookupDataNoThrow(e.Value.AsObject(), StringKey(AtomName)); ok && v.IsString() {
		return v.AsString().GoString()
	}
	return ""
}

// Message is the thrown value's `message` when it is a primitive data
// property (own or inherited), the value itself when a primitive was
// thrown, else "". undefined and null are "". It never runs a getter or
// toString.
func (e *Exception) Message() string {
	v := e.Value
	if v.IsObject() {
		var ok bool
		if v, ok = lookupDataNoThrow(v.AsObject(), StringKey(AtomMessage)); !ok || v.IsObject() {
			return ""
		}
	}
	switch v.Type() {
	case TypeUndefined, TypeNull, TypeHole:
		return ""
	case TypeBigInt:
		return v.AsBigInt().ToString()
	}
	return v.String()
}

// Unwrap returns the Go error a native function returned when the thrown
// value is the Error built for it (see hostErrorValue), so errors.Is and
// errors.As see through a throw that JavaScript did not replace; for a
// RangeError of a value nested too deep, ErrTooDeep.
func (e *Exception) Unwrap() error {
	if !e.Value.IsObject() {
		return nil
	}
	if ed := e.Value.AsObject().ErrorData(); ed != nil {
		return ed.hostErr
	}
	return nil
}

// errorDisplayString renders a thrown value without running user code. A
// thrown BigInt is shown in full, as Message returns it.
func errorDisplayString(v Value) string {
	if !v.IsObject() {
		if v.IsBigInt() {
			return v.AsBigInt().ToString() + "n"
		}
		return v.String()
	}
	o := v.AsObject()
	switch o.class {
	case ClassError:
		return errorHeader(o)
	case ClassFunction:
		// A thrown function prints as its source text, as toString does.
		if src := functionSourceText(o); src != "" {
			return src
		}
	}
	return o.debugString()
}

// errorHeader is the first line of a stack trace, V8's ErrorUtils::ToString
// over data properties only: "name: message", or whichever of the two is
// not empty; a name or message that is not a string reads as its default.
func errorHeader(o *Object) string {
	name := "Error"
	if nv, ok := lookupDataNoThrow(o, StringKey(AtomName)); ok && nv.IsString() {
		name = nv.AsString().GoString()
	}
	msg := ""
	if mv, ok := lookupDataNoThrow(o, StringKey(AtomMessage)); ok && mv.IsString() {
		msg = mv.AsString().GoString()
	}
	switch {
	case name == "":
		return msg
	case msg == "":
		return name
	}
	return name + ": " + msg
}

// lookupDataNoThrow walks the prototype chain for a data property.
func lookupDataNoThrow(o *Object, key PropertyKey) (Value, bool) {
	for obj := o; obj != nil; obj = obj.proto {
		if v, ok := obj.GetOwnDataValue(key); ok {
			return v, true
		}
		if obj.HasOwnProperty(key) {
			return Value{}, false
		}
	}
	return Value{}, false
}

// InterruptedError is returned when Realm.Interrupt stopped execution.
type InterruptedError struct {
	Value any
}

// Error implements error.
func (e *InterruptedError) Error() string {
	if e.Value == nil {
		return "interrupted"
	}
	if err, ok := e.Value.(error); ok {
		return "interrupted: " + err.Error()
	}
	if s, ok := e.Value.(string); ok {
		return "interrupted: " + s
	}
	return "interrupted: " + fmt.Sprint(e.Value)
}

// Unwrap returns Value when it is an error, so that errors.Is and errors.As
// see what Interrupt was called with: a host that interrupts with
// context.Cause(ctx) finds context.DeadlineExceeded or its own cause through
// the *InterruptedError. It returns nil for any other Value.
func (e *InterruptedError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

// Throw wraps a JavaScript value as an error.
func (r *Realm) Throw(v Value) error { return &Exception{Value: v} }

// StackFrame is one compact stack entry captured when an error object is
// created: the running function (bytecode, or a native called from the
// frame below it; nil when unknown) and, for bytecode, the pc of its current
// instruction. Recv and Flags describe how the function was called (see
// interp_stack.go). Formatting into text happens lazily on the first read
// of `stack`.
type StackFrame struct {
	Fn    *Object
	Recv  *Object
	PC    uint32
	Flags uint32
}

// Code returns the compiled function of a bytecode frame, or nil.
func (f StackFrame) Code() *bytecode.Function {
	if f.Fn == nil {
		return nil
	}
	if fd := f.Fn.FunctionData(); fd != nil {
		return fd.code
	}
	return nil
}

// ErrorData is the internal payload of ClassError objects.
type ErrorData struct {
	frames       []StackFrame
	stack        Value // formatted string, or whatever was assigned to `stack`
	materialized bool
	hostErr      error // the Go error this Error was thrown for (hostErrorValue), or ErrTooDeep
}

// Frames returns the captured compact stack.
func (ed *ErrorData) Frames() []StackFrame { return ed.frames }

// errorObject co-allocates an error object, its payload, its two slots
// (message, stack) and room for the compact stack of a shallow throw, so
// `new Error(msg)` up to two frames deep costs one allocation plus the
// message.
type errorObject struct {
	obj    Object
	data   ErrorData
	slots  [2]Value
	frames [2]StackFrame
}

// SetStackHooks installs the interpreter's stack capture and formatter.
// capture returns the current frames (it may return nil or a slice it does
// not retain); format renders them as "    at fn (file:line:col)" lines
// without a trailing newline. Either may be nil. Installing a capture hook
// replaces the interpreter's buffer-reusing capture.
func (r *Realm) SetStackHooks(capture func(r *Realm) []StackFrame, format func(r *Realm, frames []StackFrame) string) {
	r.stackCapture = capture
	r.stackCaptureInto = nil
	r.stackFormat = format
}

// NewError creates an Error instance of the given kind with a formatted
// message and a captured stack. fmt is used only when args are present.
func (r *Realm) NewError(kind ErrorKind, format string, args ...any) *Object {
	msg := format
	if len(args) != 0 {
		msg = fmt.Sprintf(format, args...)
	}
	s := FromGoString(msg)
	r.chargeGoString(s)
	return r.newErrorObject(r.errorProtos[kind], StringValue(s))
}

// newErrorObject builds an error object with own `message` (omitted when
// message is undefined) and the lazy own `stack` accessor.
func (r *Realm) newErrorObject(proto *Object, message Value) *Object {
	return r.newErrorObjectSkip(proto, message, nil)
}

// newErrorObjectSkip is newErrorObject leaving out the stack frames up to
// and including the innermost call of skip (captureStackInto).
func (r *Realm) newErrorObjectSkip(proto *Object, message Value, skip *Object) *Object {
	eo := &errorObject{}
	r.chargeObject(unsafe.Sizeof(errorObject{}))
	o := &eo.obj
	shape := r.rootShapeFor(proto)
	o.proto = proto
	o.class = ClassError
	o.flags = flagExtensible
	o.internal = &eo.data
	o.slots = eo.slots[:0:2]
	if !message.IsUndefined() {
		shape = shape.addProperty(r, StringKey(AtomMessage), attrHidden)
		o.slots = append(o.slots, message)
	}
	shape = shape.addProperty(r, StringKey(AtomStack), attrConfigurable|attrAccessor)
	o.slots = append(o.slots, accessorValue(r.errorStackAccessor))
	o.shape = shape
	if r.stackCaptureInto != nil {
		if limit, ok := r.stackTraceLimit(); ok {
			eo.data.frames = r.stackCaptureInto(r, eo.frames[:0], skip, limit)
		} else {
			eo.data.setStack(Undefined()) // a non-Number Error.stackTraceLimit
		}
	} else if r.stackCapture != nil {
		eo.data.frames = r.stackCapture(r)
	}
	if o.proto.flags&flagIsPrototype == 0 {
		r.markPrototype(o.proto)
	}
	return o
}

// ErrorData returns the payload of an error object created by the engine,
// or nil.
func (o *Object) ErrorData() *ErrorData {
	ed, _ := o.internal.(*ErrorData)
	return ed
}

// errorStackGet is the getter of the own `stack` accessor: it formats the
// captured frames on first use and caches the result.
func errorStackGet(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), nil
	}
	o := this.AsObject()
	ed := o.ErrorData()
	if ed == nil {
		return Undefined(), nil
	}
	return ed.stackValue(r, o), nil
}

// stackValue returns the `stack` of holder: the header and the captured
// frames, formatted on first use and cached.
func (ed *ErrorData) stackValue(r *Realm, holder *Object) Value {
	if !ed.materialized {
		text := errorHeader(holder)
		if r.stackFormat != nil && len(ed.frames) != 0 {
			if frames := r.stackFormat(r, ed.frames); frames != "" {
				text += "\n" + frames
			}
		}
		// The text holds the message, which can be a script's string: it is
		// charged as NewError charges a message.
		s := FromGoString(text)
		r.chargeGoString(s)
		ed.stack = StringValue(s)
		ed.materialized = true
		ed.frames = nil
	}
	return ed.stack
}

// errorStackSet is the setter of the own `stack` accessor.
func errorStackSet(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("Error.prototype.stack setter called on non-object")
	}
	o := this.AsObject()
	if o.flags&flagShared != 0 {
		return Undefined(), r.sharedWriteError(o, StringKey(AtomStack))
	}
	ed := o.ErrorData()
	if ed == nil {
		return Undefined(), r.TypeError("stack setter called on a non-error object")
	}
	ed.setStack(Arg(args, 0))
	return Undefined(), nil
}

// setStack replaces the stack with an assigned value.
func (ed *ErrorData) setStack(v Value) {
	ed.stack = v
	ed.materialized = true
	ed.frames = nil
}

// hostErrorValue is what a native function's Go error throws when it is
// neither an *Exception nor an *InterruptedError: an Error whose message is
// err.Error() and which keeps err for Exception.Unwrap.
func (r *Realm) hostErrorValue(err error) Value {
	s := FromGoString(err.Error())
	r.chargeGoString(s)
	o := r.newErrorObject(r.errorProtos[KindError], StringValue(s))
	o.ErrorData().hostErr = err
	return ObjectValue(o)
}

// StackTrace returns the `stack` of an Error object created by the engine
// when it is a string (the header and the captured frames, formatted on
// first use, or a string assigned to it), else "". It never runs user code.
func (r *Realm) StackTrace(v Value) string {
	if !v.IsObject() {
		return ""
	}
	o := v.AsObject()
	ed := o.ErrorData()
	if ed == nil {
		return ""
	}
	if s := ed.stackValue(r, o); s.IsString() {
		return s.AsString().GoString()
	}
	return ""
}

// NewErrorValue creates an error of kind with a preformatted message.
func (r *Realm) NewErrorValue(kind ErrorKind, message *String) Value {
	return ObjectValue(r.newErrorObject(r.errorProtos[kind], StringValue(message)))
}

// ErrorPrototypeFor returns the intrinsic prototype for an error kind.
func (r *Realm) ErrorPrototypeFor(kind ErrorKind) *Object { return r.errorProtos[kind] }

// ErrorConstructorFor returns the intrinsic constructor for an error kind.
func (r *Realm) ErrorConstructorFor(kind ErrorKind) *Object { return r.errorCtors[kind] }

// TypeError returns a thrown TypeError.
func (r *Realm) TypeError(format string, args ...any) error {
	return &Exception{Value: ObjectValue(r.NewError(KindTypeError, format, args...))}
}

// RangeError returns a thrown RangeError.
func (r *Realm) RangeError(format string, args ...any) error {
	return &Exception{Value: ObjectValue(r.NewError(KindRangeError, format, args...))}
}

// SyntaxError returns a thrown SyntaxError.
func (r *Realm) SyntaxError(format string, args ...any) error {
	return &Exception{Value: ObjectValue(r.NewError(KindSyntaxError, format, args...))}
}

// invalidStringLength is the RangeError for a string result that would
// exceed MaxStringLength.
func (r *Realm) invalidStringLength() error {
	return r.RangeError("Invalid string length")
}

// ReferenceError returns a thrown ReferenceError.
func (r *Realm) ReferenceError(format string, args ...any) error {
	return &Exception{Value: ObjectValue(r.NewError(KindReferenceError, format, args...))}
}

// DisplayString renders a value for error messages without running user
// code: strings are quoted-free, objects show their class or function name.
func (r *Realm) DisplayString(v Value) string {
	switch v.Type() {
	case TypeString:
		return strconv.Quote(v.AsString().GoString())
	case TypeObject:
		return v.AsObject().debugString()
	}
	return v.String()
}

package engine

// installError fills the Error family prototypes: name and
// message on each prototype, Error.prototype.toString, Error.isError and
// V8's stack trace API (Error.captureStackTrace and Error.stackTraceLimit,
// interp_stack.go). The constructors, the error object layout and the lazy
// `stack` accessor live in errors.go and initConstructors; this file holds
// the JavaScript-visible behaviour.
func installError(r *Realm) {
	ctor := r.errorCtors[KindError]
	ctor.ReserveSlots(r, len(errorStaticMethods)+1)
	r.installBuiltins(ctor, errorStaticMethods)
	r.installValues(ctor, []valueDef{{AtomStackTraceLimit, IntValue(defaultStackTraceLimit), attrDefault}})
	r.installBuiltins(r.ErrorPrototype, errorPrototypeMethods)
	for k := KindError; k < numErrorKinds; k++ {
		r.installValues(r.errorProtos[k], []valueDef{
			{AtomName, StringValue(k.Name()), attrHidden},
			{AtomMessage, StringValue(AtomEmpty), attrHidden},
		})
	}
}

var errorStaticMethods = []builtinDef{
	{AtomIsError, errorIsError, 1},
	{AtomCaptureStackTrace, errorCaptureStackTrace, 2},
}

var errorPrototypeMethods = []builtinDef{
	{AtomToString, errorProtoToString, 0},
}

// errorIsError implements Error.isError: whether the argument has an
// [[ErrorData]] slot, which only the error constructors (and engine-thrown
// errors) give; Error.prototype and objects that merely inherit from it
// have none. The NativeError constructors inherit it from Error.
func errorIsError(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	return Bool(v.IsObject() && v.AsObject().ErrorData() != nil), nil
}

// errorConstruct implements the Error and NativeError constructors (called
// with or without new): the prototype comes from newTarget, a non-undefined
// message is coerced with ToString and stored as an own non-enumerable
// property, and InstallErrorCause adds `cause` only when options has it.
func errorConstruct(r *Realm, kind ErrorKind, args []Value, newTarget *Object) (Value, error) {
	proto, err := r.GetPrototypeFromConstructor(newTarget, r.errorCtors[kind], r.errorProtos[kind])
	if err != nil {
		return Undefined(), err
	}
	// The trace starts below the constructor call (V8 skips frames up to
	// new.target, which is the constructor itself without subclassing).
	skip := newTarget
	if skip == nil {
		skip = r.errorCtors[kind]
	}
	o, err := r.constructError(proto, Arg(args, 0), Arg(args, 1), skip)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(o), nil
}

// constructError creates an error object with the given prototype and
// message (ToString unless undefined), then InstallErrorCause(options). Its
// stack trace leaves out the frames up to skip.
func (r *Realm) constructError(proto *Object, message, options Value, skip *Object) (*Object, error) {
	if !message.IsUndefined() {
		s, err := r.ToString(message)
		if err != nil {
			return nil, err
		}
		message = StringValue(s)
	}
	o := r.newErrorObjectSkip(proto, message, skip)
	if options.IsObject() {
		causeKey := StringKey(AtomCause)
		if has, err := r.hasProperty(options.AsObject(), causeKey); err != nil {
			return nil, err
		} else if has {
			cause, err := options.AsObject().GetProp(r, causeKey)
			if err != nil {
				return nil, err
			}
			o.DefineOwnDataFast(r, causeKey, cause, attrHidden)
		}
	}
	return o, nil
}

// errorProtoToString implements Error.prototype.toString: name defaults to
// "Error", message to "", and an empty side is omitted together with the
// ": " separator.
func errorProtoToString(r *Realm, this Value, args []Value) (Value, error) {
	if !this.IsObject() {
		return Undefined(), r.TypeError("Error.prototype.toString requires that 'this' be an Object")
	}
	o := this.AsObject()
	name := AtomError
	nv, err := o.GetProp(r, StringKey(AtomName))
	if err != nil {
		return Undefined(), err
	}
	if !nv.IsUndefined() {
		if name, err = r.ToString(nv); err != nil {
			return Undefined(), err
		}
	}
	msg := AtomEmpty
	mv, err := o.GetProp(r, StringKey(AtomMessage))
	if err != nil {
		return Undefined(), err
	}
	if !mv.IsUndefined() {
		if msg, err = r.ToString(mv); err != nil {
			return Undefined(), err
		}
	}
	switch {
	case name.Len() == 0:
		return StringValue(msg), nil
	case msg.Len() == 0:
		return StringValue(name), nil
	}
	var sb StringBuilder
	sb.Grow(name.Len() + 2 + msg.Len())
	sb.WriteString(name)
	sb.WriteASCII(':')
	sb.WriteASCII(' ')
	sb.WriteString(msg)
	return StringValue(r.builtString(&sb)), nil
}

// errorCaptureStackTrace implements Error.captureStackTrace(object,
// constructorOpt) as V8 has it (ErrorUtils::CaptureStackTrace): it defines
// a non-enumerable, configurable `stack` accessor on object whose getter
// formats the frames captured now under a header built from object's name
// and message. The frames start below the innermost call of constructorOpt
// when it is a function that is not bound (none when it is not on the
// stack), else below captureStackTrace itself.
func errorCaptureStackTrace(r *Realm, this Value, args []Value) (Value, error) {
	ov := Arg(args, 0)
	if !ov.IsObject() {
		return Undefined(), r.TypeError("invalid_argument")
	}
	o := ov.AsObject()
	key := StringKey(AtomStack)
	if o.flags&flagShared != 0 {
		return Undefined(), r.sharedWriteError(o, key)
	}
	if !o.IsExtensible() {
		return Undefined(), r.TypeError("Cannot define property stack, object is not extensible")
	}
	var skip *Object
	if fv := Arg(args, 1); fv.IsObject() {
		if fd := fv.AsObject().FunctionData(); fd != nil && fd.kind != FuncBound {
			skip = fv.AsObject()
		}
	}
	if n := r.interp.nframes; skip == nil && n > 0 {
		if callee, _, _, ok := r.callSite(n - 1); ok && callee.funcData().isNative() {
			skip = callee
		}
	}
	ed := o.ErrorData()
	acc := r.errorStackAccessor
	if ed == nil {
		ed = &ErrorData{}
		acc = capturedStackAccessor(r, o, ed)
	}
	if err := o.DefinePropertyOrThrow(r, key, AccessorDescriptor(acc.Get, acc.Set, attrConfigurable)); err != nil {
		return Undefined(), err
	}
	ed.frames, ed.stack, ed.materialized = nil, Undefined(), false
	switch limit, ok := r.stackTraceLimit(); {
	case !ok:
		ed.materialized = true
	case r.stackCaptureInto != nil:
		ed.frames = r.stackCaptureInto(r, nil, skip, limit)
	case r.stackCapture != nil:
		ed.frames = r.stackCapture(r)
	}
	return Undefined(), nil
}

// capturedStackAccessor is the `stack` accessor Error.captureStackTrace
// defines on an object that is not an error: its getter and setter keep the
// trace in ed and read the header from holder.
func capturedStackAccessor(r *Realm, holder *Object, ed *ErrorData) *Accessor {
	get := r.NewNativeFunction(asciiString("get stack"), 0, func(r *Realm, this Value, args []Value) (Value, error) {
		return ed.stackValue(r, holder), nil
	})
	set := r.NewNativeFunction(asciiString("set stack"), 1, func(r *Realm, this Value, args []Value) (Value, error) {
		ed.setStack(Arg(args, 0))
		return Undefined(), nil
	})
	return &Accessor{Get: get, Set: set}
}

package engine

import "strings"

// installFunction fills Function.prototype. The Function constructor
// compiles its source through the installed compiler (eval.go).
func installFunction(r *Realm) {
	// +3: caller, arguments and @@hasInstance (installSymbol).
	r.FunctionPrototype.ReserveSlots(r, len(functionPrototypeMethods)+3)
	r.installBuiltins(r.FunctionPrototype, functionPrototypeMethods)
	apply, _ := r.FunctionPrototype.GetOwnDataValue(StringKey(AtomApply))
	r.funcProtoApply = apply.AsObject()
	// AddRestrictedFunctionProperties: caller and arguments are accessors
	// whose getter and setter are both %ThrowTypeError%.
	thrower := r.NewNativeFunction(AtomEmpty, 0, throwTypeError)
	thrower.Freeze(r)
	r.restrictedAccessor = &Accessor{Get: thrower, Set: thrower}
	restricted := accessorValue(r.restrictedAccessor)
	r.installOrReplace(r.FunctionPrototype, StringKey(AtomCaller), propCell{value: restricted, attrs: attrConfigurable | attrAccessor})
	r.installOrReplace(r.FunctionPrototype, StringKey(AtomArguments), propCell{value: restricted, attrs: attrConfigurable | attrAccessor})
}

// throwTypeError is %ThrowTypeError%, the frozen anonymous function behind
// the restricted caller and arguments properties.
func throwTypeError(r *Realm, this Value, args []Value) (Value, error) {
	return Undefined(), r.TypeError("'caller', 'callee', and 'arguments' properties may not be accessed on strict mode functions or the arguments objects for calls to them")
}

var functionPrototypeMethods = []builtinDef{
	{AtomApply, functionProtoApply, 2},
	{AtomBind, functionProtoBind, 1},
	{AtomCall, functionProtoCall, 1},
	{AtomToString, functionProtoToString, 0},
}

// functionProtoApply implements Function.prototype.apply(thisArg, argArray).
func functionProtoApply(r *Realm, this Value, args []Value) (Value, error) {
	if !IsCallable(this) {
		return Undefined(), r.TypeError("Function.prototype.apply was called on %s, which is not a function", r.DisplayString(this))
	}
	argArray := Arg(args, 1)
	if argArray.IsNullish() {
		return r.callValue(this, Arg(args, 0), nil)
	}
	list, err := r.CreateListFromArrayLike(argArray)
	if err != nil {
		return Undefined(), err
	}
	return r.callValue(this, Arg(args, 0), list)
}

// functionProtoCall implements Function.prototype.call(thisArg, ...args).
// The remaining arguments are passed through without copying.
func functionProtoCall(r *Realm, this Value, args []Value) (Value, error) {
	if !IsCallable(this) {
		return Undefined(), r.TypeError("Function.prototype.call was called on %s, which is not a function", r.DisplayString(this))
	}
	var rest []Value
	if len(args) > 1 {
		rest = args[1:]
	}
	return r.callValue(this, Arg(args, 0), rest)
}

// functionProtoBind implements Function.prototype.bind(thisArg, ...args);
// BoundFunctionCreate and the length/name steps live in NewBoundFunction.
func functionProtoBind(r *Realm, this Value, args []Value) (Value, error) {
	if !IsCallable(this) {
		return Undefined(), r.TypeError("Bind must be called on a function")
	}
	var bound []Value
	if len(args) > 1 {
		bound = args[1:]
	}
	f, err := r.NewBoundFunction(this.AsObject(), Arg(args, 0), bound)
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(f), nil
}

// functionProtoToString implements Function.prototype.toString: the source
// slice recorded by the compiler for bytecode functions, otherwise the
// NativeFunction form `function name() { [native code] }` (bound functions
// print no name, "bound f" not being a valid PropertyName, and nativeName
// quotes a name no PropertyName matches).
func functionProtoToString(r *Realm, this Value, args []Value) (Value, error) {
	if !IsCallable(this) {
		return Undefined(), r.TypeError("Function.prototype.toString requires that 'this' be a Function")
	}
	if src := functionSourceText(this.AsObject()); src != "" {
		return StringValue(fromWTF8Text(src)), nil
	}
	fd := this.AsObject().FunctionData() // nil for a callable proxy
	name := AtomEmpty
	if fd != nil && fd.kind != FuncBound && fd.name != nil {
		var err error
		if name, err = nativeName(r, fd.name); err != nil {
			return Undefined(), err
		}
	}
	var sb StringBuilder
	sb.Grow(name.Len() + 32)
	sb.WriteGoString("function ")
	sb.WriteString(name)
	sb.WriteGoString("() { [native code] }")
	return StringValue(sb.String()), nil
}

// nativeName returns the PropertyName a native function's NativeFunction
// form prints for its name: the name, when it is empty or, after an
// optional "get " or "set ", an IdentifierName or a computed [...] form (a
// symbol-keyed method's); otherwise that rest as a string literal in
// brackets, a ComputedPropertyName. The getters of RegExp.$&, $+, $` and
// $' are the built-ins whose [[InitialName]], "get $&" and so on, no
// PropertyName matches; they print get ["$&"], as the legacy RegExp
// features proposal's issue 19 suggests.
func nativeName(r *Realm, name *String) (*String, error) {
	s := name.GoString()
	prefix := 0
	if strings.HasPrefix(s, "get ") || strings.HasPrefix(s, "set ") {
		prefix = 4
	}
	if s = s[prefix:]; s == "" || s[0] == '[' || isIdentifierName(s) {
		return name, nil
	}
	js := jsonStringifier{r: r}
	js.sb.WriteString(name.Substring(0, prefix))
	js.sb.WriteASCII('[')
	if err := js.quote(name.Substring(prefix, name.Len())); err != nil {
		return nil, err
	}
	js.sb.WriteASCII(']')
	return js.sb.String(), nil
}

// functionSourceText is the source slice the compiler recorded for a
// bytecode function (a range into the one retained copy of its file), or ""
// for natives, bound functions and code without a source.
func functionSourceText(fn *Object) string {
	fd := fn.FunctionData()
	if fd == nil || fd.kind != FuncBytecode || fd.code == nil {
		return ""
	}
	return fd.code.SourceText()
}

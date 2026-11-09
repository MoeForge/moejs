package engine

// installBoolean fills Boolean.prototype. The constructor
// itself is created by initConstructors.
func installBoolean(r *Realm) {
	r.installBuiltins(r.BooleanPrototype, booleanPrototypeMethods)
}

var booleanPrototypeMethods = []builtinDef{
	{AtomToString, booleanProtoToString, 0},
	{AtomValueOf, booleanProtoValueOf, 0},
}

// booleanCall implements Boolean(value).
func booleanCall(r *Realm, this Value, args []Value) (Value, error) {
	return Bool(ToBoolean(Arg(args, 0))), nil
}

// booleanConstruct implements new Boolean(value).
func booleanConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	o, err := r.OrdinaryCreateFromConstructor(newTarget, r.BooleanPrototype, ClassBoolean)
	if err != nil {
		return Undefined(), err
	}
	setExtData(o, kindPrimitive, &primitiveWrapper{value: Bool(ToBoolean(Arg(args, 0)))})
	return ObjectValue(o), nil
}

// thisBooleanValue implements ThisBooleanValue.
func thisBooleanValue(r *Realm, this Value, method string) (bool, error) {
	if this.IsBool() {
		return this.AsBool(), nil
	}
	if this.IsObject() && this.AsObject().class == ClassBoolean {
		return extDataOf(this.AsObject(), kindPrimitive).value.AsBool(), nil
	}
	return false, r.TypeError("Boolean.prototype.%s requires that 'this' be a Boolean", method)
}

func booleanProtoValueOf(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBooleanValue(r, this, "valueOf")
	if err != nil {
		return Undefined(), err
	}
	return Bool(b), nil
}

func booleanProtoToString(r *Realm, this Value, args []Value) (Value, error) {
	b, err := thisBooleanValue(r, this, "toString")
	if err != nil {
		return Undefined(), err
	}
	if b {
		return StringValue(AtomTrue), nil
	}
	return StringValue(AtomFalse), nil
}

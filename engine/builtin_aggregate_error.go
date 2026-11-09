package engine

// AggregateError (ES2025 §20.5.7): a NativeError-shaped constructor whose
// first argument is an iterable of errors.

func installAggregateError(r *Realm) {
	r.AggregateErrorPrototype = r.newIntrinsic(ClassObject, r.ErrorPrototype, 3)
	r.installValues(r.AggregateErrorPrototype, []valueDef{
		{AtomName, StringValue(AtomAggregateError), attrHidden},
		{AtomMessage, StringValue(AtomEmpty), attrHidden},
	})
	call := func(r *Realm, this Value, args []Value) (Value, error) {
		return aggregateErrorConstruct(r, args, nil)
	}
	ctor := r.newConstructor(AtomAggregateError, 2, call, aggregateErrorConstruct, r.AggregateErrorPrototype)
	r.markPrototype(r.errorCtors[KindError])
	ctor.shape = ctor.shape.rebase(r, r.rootShapeFor(r.errorCtors[KindError]))
	r.AggregateErrorCtor = ctor
	r.bindGlobal(AtomAggregateError, ObjectValue(ctor))
}

// aggregateErrorConstruct implements AggregateError(errors, message,
// options), called with or without new.
func aggregateErrorConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	proto, err := r.GetPrototypeFromConstructor(newTarget, r.AggregateErrorCtor, r.AggregateErrorPrototype)
	if err != nil {
		return Undefined(), err
	}
	skip := newTarget
	if skip == nil {
		skip = r.AggregateErrorCtor
	}
	o, err := r.constructError(proto, Arg(args, 1), Arg(args, 2), skip)
	if err != nil {
		return Undefined(), err
	}
	it, err := r.getIterator(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	errors, err := r.iterRest(&it.it, &it.pos)
	if err != nil {
		return Undefined(), err
	}
	o.DefineOwnDataFast(r, StringKey(AtomErrors), errors, attrHidden)
	return ObjectValue(o), nil
}

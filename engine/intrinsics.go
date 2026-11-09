package engine

// installIntrinsics is the ordered list of builtin installers (one line per
// builtin group; see the file convention in realm.go). It runs after the
// skeleton objects exist, once per mutable realm and once per process for
// the shared template.
func installIntrinsics(r *Realm) {
	r.errorStackAccessor = &Accessor{
		Get: r.NewNativeFunction(asciiString("get stack"), 0, errorStackGet),
		Set: r.NewNativeFunction(asciiString("set stack"), 1, errorStackSet),
	}
	installObject(r)
	installFunction(r)
	installArray(r)
	installArrayIterator(r)
	installError(r)
	installGlobal(r) // before installNumber: Number.parseInt === parseInt
	installBoolean(r)
	installNumber(r)
	installMath(r)
	installString(r)
	installRegExp(r)
	installJSON(r)
	installDate(r)
	installBigInt(r)
	installURI(r)
	installReflect(r)
	installSymbol(r)
	installMap(r)
	installSet(r)
	installWeakMap(r)
	installWeakSet(r)
	installWeakRef(r)
	installAggregateError(r)
	installGenerators(r)
	installSpecies(r)
	installRegExpStatics(r) // after @@species
	installLateGlobals(r)   // last: the lateGlobal bindings
}

// notAvailableCall/Construct are the placeholders for constructors whose
// behaviour an installer supplies later.
func notAvailableCall(name string) NativeFunc {
	return func(r *Realm, this Value, args []Value) (Value, error) {
		return Undefined(), r.TypeError("%s is not available yet", name)
	}
}

func notAvailableConstruct(name string) NativeCtor {
	return func(r *Realm, args []Value, newTarget *Object) (Value, error) {
		return Undefined(), r.TypeError("%s is not available yet", name)
	}
}

// --- String ------------------------------------------------------------------

func stringCall(r *Realm, this Value, args []Value) (Value, error) {
	if len(args) == 0 {
		return StringValue(AtomEmpty), nil
	}
	if args[0].IsSymbol() {
		return StringValue(FromGoString(args[0].AsSymbol().descriptiveString())), nil
	}
	s, err := r.ToString(args[0])
	if err != nil {
		return Undefined(), err
	}
	return StringValue(s), nil
}

func stringConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	s := AtomEmpty
	if len(args) != 0 {
		var err error
		if s, err = r.ToString(args[0]); err != nil {
			return Undefined(), err
		}
	}
	o, err := r.OrdinaryCreateFromConstructor(newTarget, r.StringPrototype, ClassString)
	if err != nil {
		return Undefined(), err
	}
	setExtData(o, kindString, s)
	return ObjectValue(o), nil
}

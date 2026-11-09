package engine

// Symbol constructor, Symbol.prototype and Function.prototype[@@hasInstance]
// (ES2025 §20.4, §20.2.3.6).

var (
	AtomFor                = staticAtom("for")
	AtomKeyFor             = staticAtom("keyFor")
	AtomAsyncIterator      = staticAtom("asyncIterator")
	AtomIsConcatSpreadable = staticAtom("isConcatSpreadable")
	AtomSearch             = staticAtom("search")
	AtomSpecies            = staticAtom("species")
	AtomUnscopables        = staticAtom("unscopables")
)

var symbolCtorMethods = []builtinDef{
	{AtomFor, symbolFor, 1},
	{AtomKeyFor, symbolKeyFor, 1},
}

var symbolProtoMethods = []builtinDef{
	{AtomToString, symbolProtoToString, 0},
	{AtomValueOf, symbolProtoValueOf, 0},
}

var symbolProtoGetters = []getterDef{
	newGetterDef(AtomDescription, symbolProtoDescription),
}

// Names of the symbol-keyed builtins: static, so installing them builds no
// strings.
var (
	atomIteratorFn    = staticAtom("[Symbol.iterator]")
	atomHasInstanceFn = staticAtom("[Symbol.hasInstance]")
	atomToPrimitiveFn = staticAtom("[Symbol.toPrimitive]")
	atomMatchFn       = staticAtom("[Symbol.match]")
	atomMatchAllFn    = staticAtom("[Symbol.matchAll]")
	atomReplaceFn     = staticAtom("[Symbol.replace]")
	atomSearchFn      = staticAtom("[Symbol.search]")
	atomSplitFn       = staticAtom("[Symbol.split]")
	atomGetSpeciesFn  = staticAtom("get [Symbol.species]")
)

// wellKnownSymbols lists the Symbol.* constants in spec order.
var wellKnownSymbols = [...]struct {
	name *String
	sym  *Symbol
}{
	{AtomAsyncIterator, SymAsyncIterator},
	{AtomHasInstance, SymHasInstance},
	{AtomIsConcatSpreadable, SymIsConcatSpreadable},
	{AtomIterator, SymIterator},
	{AtomMatch, SymMatch},
	{AtomMatchAll, SymMatchAll},
	{AtomReplace, SymReplace},
	{AtomSearch, SymSearch},
	{AtomSpecies, SymSpecies},
	{AtomSplit, SymSplit},
	{AtomToPrimitive, SymToPrimitive},
	{AtomToStringTag, SymToStringTag},
	{AtomUnscopables, SymUnscopables},
}

func installSymbol(r *Realm) {
	r.SymbolPrototype = r.newIntrinsic(ClassObject, r.ObjectPrototype, r.deferredCap(symbolProtoProps+1, 1))
	r.SymbolCtor = r.newConstructor(AtomSymbol, 0, symbolCall, symbolConstruct, r.SymbolPrototype)
	c := r.SymbolCtor
	c.ReserveSlots(r, len(symbolCtorMethods)+len(wellKnownSymbols))
	r.installBuiltins(c, symbolCtorMethods)
	for _, w := range wellKnownSymbols {
		c.DefineOwnDataFast(r, StringKey(w.name), SymbolValue(w.sym), attrFrozen)
	}
	r.installDeferred(r.SymbolPrototype, installSymbolPrototype)
	r.funcProtoHasInstance = r.installSymbolMethod(r.FunctionPrototype, SymHasInstance, atomHasInstanceFn, functionProtoHasInstance, 1, attrFrozen)
	// The @@toStringTag of intrinsics installed before Symbol existed.
	r.installToStringTag(r.JSON, AtomJSON)
	if r.BigIntPrototype != nil {
		r.installToStringTag(r.BigIntPrototype, AtomBigInt)
	}
	r.bindGlobal(AtomSymbol, ObjectValue(c))
}

// symbolProtoProps counts the properties installSymbolPrototype defines.
var symbolProtoProps = len(symbolProtoMethods) + len(symbolProtoGetters) + 2

func installSymbolPrototype(r *Realm, p *Object) {
	p.ReserveSlots(r, symbolProtoProps)
	r.installBuiltins(p, symbolProtoMethods)
	r.installGetters(p, symbolProtoGetters)
	r.installSymbolMethod(p, SymToPrimitive, atomToPrimitiveFn, symbolProtoToPrimitive, 1, attrConfigurable)
	r.installToStringTag(p, AtomSymbol)
}

// symbolCall implements Symbol(description) called as a function.
func symbolCall(r *Realm, this Value, args []Value) (Value, error) {
	var desc *String
	if d := Arg(args, 0); !d.IsUndefined() {
		var err error
		if desc, err = r.ToString(d); err != nil {
			return Undefined(), err
		}
		desc.flatten() // symbols are immutable: never flatten on a later read
	}
	return SymbolValue(NewSymbol(desc)), nil
}

// symbolConstruct rejects `new Symbol()`: Symbol is a constructor (it may
// appear in an extends clause) whose [[Construct]] always throws.
func symbolConstruct(r *Realm, args []Value, newTarget *Object) (Value, error) {
	return Undefined(), r.TypeError("Symbol is not a constructor")
}

// symbolFor implements Symbol.for(key) over the process-wide registry.
func symbolFor(r *Realm, this Value, args []Value) (Value, error) {
	key, err := r.ToString(Arg(args, 0))
	if err != nil {
		return Undefined(), err
	}
	return SymbolValue(registeredSymbol(r.Intern(key))), nil
}

// symbolKeyFor implements Symbol.keyFor(sym).
func symbolKeyFor(r *Realm, this Value, args []Value) (Value, error) {
	v := Arg(args, 0)
	if !v.IsSymbol() {
		return Undefined(), r.TypeError("%s is not a symbol", r.DisplayString(v))
	}
	if s := v.AsSymbol(); s.registered {
		return StringValue(s.description), nil
	}
	return Undefined(), nil
}

// thisSymbolValue implements ThisSymbolValue.
func thisSymbolValue(r *Realm, v Value, method string) (*Symbol, error) {
	if v.IsSymbol() {
		return v.AsSymbol(), nil
	}
	if v.IsObject() {
		if s, ok := extDataOk(v.AsObject(), kindSymbol); ok && v.AsObject().class == ClassSymbol {
			return s, nil
		}
	}
	return nil, r.TypeError("Symbol.prototype.%s requires that 'this' be a Symbol", method)
}

func symbolProtoToString(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisSymbolValue(r, this, "toString")
	if err != nil {
		return Undefined(), err
	}
	return StringValue(s.descriptiveJSString()), nil
}

func symbolProtoValueOf(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisSymbolValue(r, this, "valueOf")
	if err != nil {
		return Undefined(), err
	}
	return SymbolValue(s), nil
}

func symbolProtoDescription(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisSymbolValue(r, this, "description")
	if err != nil {
		return Undefined(), err
	}
	if s.description == nil {
		return Undefined(), nil
	}
	return StringValue(s.description), nil
}

func symbolProtoToPrimitive(r *Realm, this Value, args []Value) (Value, error) {
	s, err := thisSymbolValue(r, this, "[Symbol.toPrimitive]")
	if err != nil {
		return Undefined(), err
	}
	return SymbolValue(s), nil
}

// functionProtoHasInstance implements Function.prototype[@@hasInstance].
func functionProtoHasInstance(r *Realm, this Value, args []Value) (Value, error) {
	ok, err := r.OrdinaryHasInstance(this, Arg(args, 0))
	return Bool(ok), err
}

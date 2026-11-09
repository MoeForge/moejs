package engine

// extIntrinsics holds the intrinsic objects Intrinsics has no room for. The
// Realm embeds Intrinsics by value and must stay within its 896-byte size
// class (realm.go), so new intrinsics live behind this one pointer instead of
// growing every realm: a shared realm copies the template's pointer (no
// allocation), a mutable realm allocates its own in buildIntrinsics.
//
// Adding an intrinsic: add the field here, create it in its installer
// (intrinsics.go order) and list it in visitExtIntrinsics so the shared
// template freezes it.
type extIntrinsics struct {
	// errorStackAccessor is the shared getter/setter pair installed as the
	// own `stack` accessor of every error object (see errors.go).
	errorStackAccessor *Accessor
	// restrictedAccessor is the %ThrowTypeError% getter/setter pair behind
	// Function.prototype.caller/arguments and every arguments.callee.
	restrictedAccessor *Accessor

	SymbolPrototype *Object
	SymbolCtor      *Object

	// funcProtoHasInstance is the original Function.prototype[@@hasInstance];
	// instanceof skips the call when the lookup finds it (compare.go).
	funcProtoHasInstance *Object
	// funcProtoApply is the original Function.prototype.apply, which
	// ApplyArguments calls through without its arguments object.
	funcProtoApply *Object

	IteratorPrototype       *Object
	StringIteratorPrototype *Object
	// The original iteration methods, recognized by the iteration fast
	// paths (iter.go): Array.prototype.values (also its @@iterator and
	// arguments[@@iterator]), %ArrayIteratorPrototype%.next,
	// String.prototype[@@iterator] and %StringIteratorPrototype%.next.
	arrayValuesFn, arrayIterNextFn, stringIterFn, stringIterNextFn *Object
	// protoGuards watch those methods, Array.prototype.constructor and
	// %Array%[@@species] in place (protocol.go).
	protoGuards [numProtoGuards]protoGuard
	// absentGuards cache that Array.prototype and Object.prototype lack
	// @@isConcatSpreadable (protocol.go).
	absentGuards [numAbsentGuards]absentGuard

	MapPrototype, MapCtor, MapIteratorPrototype *Object
	SetPrototype, SetCtor, SetIteratorPrototype *Object
	// The original Map.prototype.set and Set.prototype.add (the constructors
	// store directly while they are the adder) and the collection iterators'
	// next methods (iterStep steps their payload directly).
	mapSetFn, setAddFn, mapIterNextFn, setIterNextFn *Object

	WeakMapPrototype, WeakMapCtor *Object
	WeakSetPrototype, WeakSetCtor *Object
	WeakRefPrototype, WeakRefCtor *Object
	// The original WeakMap.prototype.set and WeakSet.prototype.add.
	weakMapSetFn, weakSetAddFn *Object

	AggregateErrorPrototype, AggregateErrorCtor *Object

	// evalFn is %eval%, nil until the realm defines the eval global: a
	// CallEval site whose callee is it runs a direct eval (eval.go), and
	// defines the global first.
	evalFn *Object

	RegExpStringIteratorPrototype, regexpStringIterNextFn *Object
	// regexpGuard watches the %RegExp.prototype% properties the RegExp
	// Symbol protocol reads (regexp_protocol.go).
	regexpGuard regexpGuard

	// async holds the generator intrinsics; nil in a mutable realm until
	// first use (generator.go).
	async   *asyncIntrinsics
	promise *promiseIntrinsics // builtin_promise.go

	// binary holds the ArrayBuffer, SharedArrayBuffer and DataView
	// intrinsics, nil until the realm defines one of them (binaryIntr).
	binary *binaryIntrinsics

	// intl holds the Intl intrinsics, nil until the realm defines Intl
	// (builtin_intl.go).
	intl *intlIntrinsics
}

// visitExtIntrinsics feeds every extIntrinsics root to visit: to the
// freeze walk of the shared template (freezeIntrinsics) and to the live
// measurement of a mutable realm (visitIntrinsicRoots).
func (r *Realm) visitExtIntrinsics(visit func(*Object)) {
	x := r.extIntrinsics
	// A mutable realm defines the stack accessor and the generator
	// intrinsics on first use; the template has every one.
	if a := x.errorStackAccessor; a != nil {
		visit(a.Get)
		visit(a.Set)
	}
	for _, o := range [...]*Object{
		x.SymbolPrototype, x.SymbolCtor, x.funcProtoHasInstance, x.funcProtoApply,
		x.IteratorPrototype, x.StringIteratorPrototype,
		x.arrayValuesFn, x.arrayIterNextFn, x.stringIterFn, x.stringIterNextFn,
		x.MapPrototype, x.MapCtor, x.MapIteratorPrototype,
		x.SetPrototype, x.SetCtor, x.SetIteratorPrototype,
		x.mapSetFn, x.setAddFn, x.mapIterNextFn, x.setIterNextFn,
		x.WeakMapPrototype, x.WeakMapCtor, x.WeakSetPrototype, x.WeakSetCtor,
		x.WeakRefPrototype, x.WeakRefCtor, x.weakMapSetFn, x.weakSetAddFn,
		x.AggregateErrorPrototype, x.AggregateErrorCtor,
		x.RegExpStringIteratorPrototype, x.regexpStringIterNextFn,
	} {
		visit(o)
	}
	if g := x.async; g != nil {
		for _, o := range [...]*Object{
			g.GeneratorFunction, g.GeneratorFunctionPrototype, g.GeneratorPrototype,
			g.AsyncFunction, g.AsyncFunctionPrototype,
			g.AsyncGeneratorFunction, g.AsyncGeneratorFunctionPrototype, g.AsyncGeneratorPrototype,
			g.AsyncIteratorPrototype, g.AsyncFromSyncIteratorPrototype, g.asyncFromSyncNext,
		} {
			visit(o)
		}
	}
	if x.evalFn != nil {
		visit(x.evalFn)
	}
	if p := x.promise; p != nil {
		visit(p.proto)
		visit(p.ctor)
	}
	if x.binary != nil {
		x.binary.visit(visit)
	}
	if x.intl != nil {
		x.intl.visit(visit)
	}
}

// bindGlobal defines a global binding for an intrinsic installed after
// initGlobal: writable and configurable in a mutable realm, read-only in
// the shared template (the lockdown model, as initGlobal does).
func (r *Realm) bindGlobal(name *String, v Value) {
	attrs := attrHidden
	if r.buildingShared {
		attrs = attrFrozen
	}
	if r.coldGlobals != 0 {
		// A late installer (lateGlobal) binding one of its keys.
		if i := r.coldGlobalIndex(StringKey(name)); i >= 0 {
			r.coldGlobals &^= 1 << i
		}
	}
	r.Global.DefineOwnDataFast(r, StringKey(name), v, attrs)
}

// symbolMethodDef describes one symbol-keyed builtin method; name is its
// static "[Symbol.x]" atom (builtin_symbol.go).
type symbolMethodDef struct {
	sym    *Symbol
	name   *String
	fn     NativeFunc
	length int
}

// installSymbolMethod defines target[sym] as a builtin method with the given
// attributes and returns the function.
func (r *Realm) installSymbolMethod(target *Object, sym *Symbol, name *String, fn NativeFunc, length int, attrs uint8) *Object {
	f := r.NewNativeFunction(name, length, fn)
	r.installOrReplace(target, SymbolKey(sym), propCell{value: ObjectValue(f), attrs: attrs})
	return f
}

// installSymbolMethods is installSymbolMethod for a table.
func (r *Realm) installSymbolMethods(target *Object, defs []symbolMethodDef, attrs uint8) {
	target.ReserveSlots(r, len(defs))
	for _, d := range defs {
		r.installSymbolMethod(target, d.sym, d.name, d.fn, d.length, attrs)
	}
}

// installSpeciesGetter defines ctor[@@species] as a configurable getter-only
// accessor returning the receiver and returns the getter.
func (r *Realm) installSpeciesGetter(ctor *Object) *Object {
	f := r.NewNativeFunction(atomGetSpeciesFn, 0, speciesGetter)
	r.installOrReplace(ctor, speciesKey, propCell{
		value: accessorValue(&Accessor{Get: f}),
		attrs: attrConfigurable | attrAccessor,
	})
	return f
}

// installToStringTag defines target[@@toStringTag] = tag (configurable only).
func (r *Realm) installToStringTag(target *Object, tag *String) {
	r.installOrReplace(target, SymbolKey(SymToStringTag), propCell{value: StringValue(tag), attrs: attrConfigurable})
}

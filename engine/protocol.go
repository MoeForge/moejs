package engine

// Protocol guards: the well-known-symbol lookups the spec performs on every
// ToPrimitive, Object.prototype.toString, instanceof, ... are skipped when
// their result is known without running user code, so the plugin path pays
// a shape probe instead of a prototype-chain walk.

// lacksWellKnown reports whether o provably has no property key (a
// well-known symbol) on its prototype chain, looking only at shapes: false
// means "unknown", and the caller performs the ordinary [[Get]]. In a
// shared realm Object.prototype is frozen and has no symbol-keyed
// properties, so the walk stops there. Nor has a host placeholder
// (hostlazy.go), which materializes to string keys only. A proxy answers
// through its get trap, so it is always "unknown". A pending compile
// method of %RegExp.prototype% or static of %RegExp% is not such a key
// (onlyLegacyPending).
func (r *Realm) lacksWellKnown(o *Object, key PropertyKey) bool {
	for p := o; p != nil; p = p.Proto() {
		if p == r.ObjectPrototype && r.sharedIntrinsics {
			return true
		}
		if p.class == ClassProxy {
			return false
		}
		if p.flags&(flagDict|flagHasLazy) != 0 && (p.shape.key != hostSentinelKey || !key.IsSymbol()) && !p.onlyLegacyPending() {
			return false
		}
		if _, _, ok := p.shape.Lookup(key); ok {
			return false
		}
	}
	return true
}

// hasInstanceMethod returns c's @@hasInstance method, or nil when it has
// none or it is the original Function.prototype[@@hasInstance] (the caller
// then runs OrdinaryHasInstance directly). That property is non-writable
// and non-configurable, so a function whose prototype is Function.prototype
// and that has no own @@hasInstance always inherits the original.
func (r *Realm) hasInstanceMethod(c *Object) (*Object, error) {
	key := SymbolKey(SymHasInstance)
	if c.Proto() == r.FunctionPrototype && (c.flags&(flagDict|flagHasLazy) == 0 || c.onlyLegacyPending()) {
		if _, _, ok := c.shape.Lookup(key); !ok {
			return nil, nil
		}
	}
	m, err := r.GetMethod(ObjectValue(c), key)
	if err != nil || m.IsUndefined() || m.AsObject() == r.funcProtoHasInstance {
		return nil, err
	}
	return m.AsObject(), nil
}

// protoGuard watches one data property of an intrinsic (o[key] == want) so a
// fast path can depend on it with a shape compare and a slot load. A shared
// realm's intrinsics are frozen, so its guards always hold; a mutable realm
// caches the shape and slot where the property was last found intact.
type protoGuard struct {
	obj   *Object
	key   PropertyKey
	want  Value
	shape *Shape
	slot  uint32
}

// The iteration guards (iter.go) and the species guards (below; the RegExp
// protocol reads guardRegExpSpecies, regexp_protocol.go).
const (
	guardArrayIter     = iota // Array.prototype[@@iterator] is Array.prototype.values
	guardArrayNext            // %ArrayIteratorPrototype%.next is the original
	guardStringIter           // String.prototype[@@iterator] is the original
	guardStringNext           // %StringIteratorPrototype%.next is the original
	guardArrayCtor            // Array.prototype.constructor is %Array%
	guardArraySpecies         // %Array%[@@species] is the original getter
	guardRegExpSpecies        // %RegExp%[@@species] is the original getter
	numProtoGuards
)

// guardHolds reports whether g's property still holds its original value
// (an accessor guard's want is the original accessor pair).
func (r *Realm) guardHolds(g *protoGuard) bool {
	return r.sharedIntrinsics || g.obj.shape == g.shape && g.obj.slots[g.slot] == g.want || g.rearm()
}

// rearm re-resolves g after its object changed shape or the slot changed.
// A dictionary-mode or lazily materialized object is never cached: the
// probe falls back to a lookup every time. %RegExp% with only its statics
// pending is cached: the watched key is @@species.
func (g *protoGuard) rearm() bool {
	o := g.obj
	g.shape = nil
	accessor := g.want.isAccessor()
	if o.flags&(flagDict|flagHasLazy) != 0 && !o.onlyLegacyPending() {
		c, ok := o.getOwnCell(g.key)
		return ok && (c.attrs&attrAccessor != 0) == accessor && c.value == g.want
	}
	slot, attrs, ok := o.shape.Lookup(g.key)
	if !ok || (attrs&attrAccessor != 0) != accessor || o.slots[slot] != g.want {
		return false
	}
	g.shape, g.slot = o.shape, slot
	return true
}

// absentGuard watches the absence of one own property of an intrinsic,
// caching the shape at which it was last found missing (a shape names its
// key set). A shared realm's frozen intrinsics never gain properties.
type absentGuard struct {
	obj   *Object
	key   PropertyKey
	shape *Shape
}

// The absence guards.
const (
	absentArraySpreadable  = iota // Array.prototype has no own @@isConcatSpreadable
	absentObjectSpreadable        // Object.prototype has no own @@isConcatSpreadable
	numAbsentGuards
)

// absentHolds reports whether g's property is still missing.
func (r *Realm) absentHolds(g *absentGuard) bool {
	return r.sharedIntrinsics || g.obj.shape == g.shape || g.rearm()
}

func (g *absentGuard) rearm() bool {
	o := g.obj
	g.shape = nil
	if o.flags&(flagDict|flagHasLazy) != 0 {
		_, ok := o.getOwnCell(g.key)
		return !ok
	}
	if _, _, ok := o.shape.Lookup(g.key); ok {
		return false
	}
	g.shape = o.shape
	return true
}

// --- species --------------------------------------------------------------------

var (
	constructorKey = StringKey(AtomConstructor)
	speciesKey     = SymbolKey(SymSpecies)
	spreadableKey  = SymbolKey(SymIsConcatSpreadable)
)

// installSpecies defines get %Array%[@@species] and get %RegExp%[@@species]
// (Map's and Set's are installed with them) and arms the species and
// @@isConcatSpreadable guards.
func installSpecies(r *Realm) {
	r.installSpeciesGetter(r.ArrayCtor)
	r.installSpeciesGetter(r.RegExpCtor)
	species, _ := r.ArrayCtor.getOwnCell(speciesKey)
	rxSpeciesCell, _ := r.RegExpCtor.getOwnCell(speciesKey)
	r.protoGuards[guardArrayCtor] = protoGuard{obj: r.ArrayPrototype, key: constructorKey, want: ObjectValue(r.ArrayCtor)}
	r.protoGuards[guardArraySpecies] = protoGuard{obj: r.ArrayCtor, key: speciesKey, want: species.value}
	r.protoGuards[guardRegExpSpecies] = protoGuard{obj: r.RegExpCtor, key: speciesKey, want: rxSpeciesCell.value}
	r.absentGuards[absentArraySpreadable] = absentGuard{obj: r.ArrayPrototype, key: spreadableKey}
	r.absentGuards[absentObjectSpreadable] = absentGuard{obj: r.ObjectPrototype, key: spreadableKey}
}

// spreadableProtosClean reports whether Array.prototype and
// Object.prototype lack @@isConcatSpreadable and form the usual chain.
func (r *Realm) spreadableProtosClean() bool {
	ap, op := r.ArrayPrototype, r.ObjectPrototype
	return r.sharedIntrinsics || ap.Proto() == op && op.Proto() == nil &&
		r.absentHolds(&r.absentGuards[absentArraySpreadable]) && r.absentHolds(&r.absentGuards[absentObjectSpreadable])
}

// lacksSpreadable is lacksWellKnown(o, @@isConcatSpreadable) with the
// answer for Array.prototype and Object.prototype cached; clean is
// spreadableProtosClean(), which concat computes once for all its items.
func (r *Realm) lacksSpreadable(o *Object, clean bool) bool {
	if o.lazyOrDict() && o.shape.key != hostSentinelKey || o.class == ClassProxy {
		return false
	}
	if o.shape.count != 0 {
		if _, _, ok := o.shape.Lookup(spreadableKey); ok {
			return false
		}
	}
	if clean && (o.Proto() == r.ArrayPrototype || o.Proto() == r.ObjectPrototype) {
		return true
	}
	return o.Proto() == nil || r.lacksWellKnown(o.Proto(), spreadableKey)
}

// arraySpeciesCtor implements the constructor lookup of
// ArraySpeciesCreate(o, length): nil means ArrayCreate(length), which the
// caller's own array paths perform. An array whose constructor is found
// with shape probes to be the original %Array% with the original
// @@species resolves without a property read, so the common case runs no
// user code and allocates nothing. %Array% itself also yields nil
// (Construct(%Array%, «length») is ArrayCreate(length)).
//
// The cross-realm step (a constructor that is another realm's %Array%
// becomes undefined) is not implemented: realms exchange no functions
// from script. A proxy is an array when its target is (IsArray).
func (r *Realm) arraySpeciesCtor(o *Object) (*Object, error) {
	if o.class != ClassArray {
		if o.class == ClassProxy {
			if isArr, err := r.isArray(ObjectValue(o)); err != nil || !isArr {
				return nil, err
			}
			return r.arraySpeciesLookup(o)
		}
		return nil, nil
	}
	if o.Proto() == r.ArrayPrototype && r.speciesIntact() {
		if !o.lazyOrDict() {
			if o.shape.count == 0 {
				return nil, nil
			}
			if _, _, own := o.shape.Lookup(constructorKey); !own {
				return nil, nil
			}
		} else if o.shape.key == hostSentinelKey {
			return nil, nil // a host slice placeholder has no named property
		}
	}
	return r.arraySpeciesLookup(o)
}

// speciesIntact reports whether Array.prototype.constructor is %Array% and
// %Array%[@@species] its original getter.
func (r *Realm) speciesIntact() bool {
	return r.sharedIntrinsics ||
		r.guardHolds(&r.protoGuards[guardArrayCtor]) && r.guardHolds(&r.protoGuards[guardArraySpecies])
}

// arraySpeciesLookup reads o.constructor[@@species].
func (r *Realm) arraySpeciesLookup(o *Object) (*Object, error) {
	c, err := o.GetProp(r, constructorKey)
	if err != nil {
		return nil, err
	}
	if c.IsObject() {
		if c, err = c.AsObject().GetProp(r, speciesKey); err != nil {
			return nil, err
		}
		if c.IsNull() {
			c = Undefined()
		}
	}
	if c.IsUndefined() {
		return nil, nil
	}
	if !IsConstructor(c) {
		return nil, r.TypeError("object.constructor[Symbol.species] is not a constructor")
	}
	if c.AsObject() == r.ArrayCtor {
		return nil, nil
	}
	return c.AsObject(), nil
}

// speciesCreate constructs the species result Construct(c, «length»).
func (r *Realm) speciesCreate(c *Object, length int64) (*Object, error) {
	argv := r.pushArgs(1)
	defer r.popArgs(1)
	argv[0] = Int64Value(length)
	v, err := r.Construct(ObjectValue(c), argv, nil)
	if err != nil {
		return nil, err
	}
	if !v.IsObject() {
		return nil, r.TypeError("species constructor returned a non-object")
	}
	return v.AsObject(), nil
}

// speciesConstructor implements SpeciesConstructor(o, defaultCtor).
func (r *Realm) speciesConstructor(o, defaultCtor *Object) (*Object, error) {
	c, err := o.GetProp(r, constructorKey)
	if err != nil {
		return nil, err
	}
	if c.IsUndefined() {
		return defaultCtor, nil
	}
	if !c.IsObject() {
		return nil, r.TypeError("object.constructor is not an object")
	}
	s, err := c.AsObject().GetProp(r, speciesKey)
	if err != nil {
		return nil, err
	}
	if s.IsNullish() {
		return defaultCtor, nil
	}
	if !IsConstructor(s) {
		return nil, r.TypeError("object.constructor[Symbol.species] is not a constructor")
	}
	return s.AsObject(), nil
}

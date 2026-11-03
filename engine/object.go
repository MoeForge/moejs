package engine

import (
	"slices"
	"unsafe"
)

// Class is the object's exotic/internal kind.
type Class uint8

const (
	ClassObject Class = iota
	ClassArray
	ClassFunction
	ClassError
	ClassBoolean
	ClassNumber
	ClassString
	ClassSymbol
	ClassBigInt
	ClassRegExp
	ClassDate
	ClassArguments
	ClassArrayIterator
	ClassStringIterator
	ClassMap
	ClassSet
	ClassWeakMap
	ClassWeakSet
	ClassWeakRef
	ClassMapIterator
	ClassSetIterator
	ClassRegExpStringIterator
	ClassGenerator
	ClassPromise
	ClassProxy
	ClassArrayBuffer
	ClassSharedArrayBuffer
	ClassDataView
	ClassAsyncGenerator
	ClassTypedArray
	ClassTextEncoder
	ClassTextDecoder
	ClassIntlLocale
	ClassIntlSegmenter
	ClassIntlSegments
	ClassIntlSegmentIterator
	ClassIntlNumberFormat
	ClassIntlPluralRules
	ClassIntlDateTimeFormat
)

var classNames = [...]string{
	ClassObject:               "Object",
	ClassArray:                "Array",
	ClassFunction:             "Function",
	ClassError:                "Error",
	ClassBoolean:              "Boolean",
	ClassNumber:               "Number",
	ClassString:               "String",
	ClassSymbol:               "Symbol",
	ClassBigInt:               "BigInt",
	ClassRegExp:               "RegExp",
	ClassDate:                 "Date",
	ClassArguments:            "Arguments",
	ClassArrayIterator:        "Array Iterator",
	ClassStringIterator:       "String Iterator",
	ClassMap:                  "Map",
	ClassSet:                  "Set",
	ClassWeakMap:              "WeakMap",
	ClassWeakSet:              "WeakSet",
	ClassWeakRef:              "WeakRef",
	ClassMapIterator:          "Map Iterator",
	ClassSetIterator:          "Set Iterator",
	ClassRegExpStringIterator: "RegExp String Iterator",
	ClassGenerator:            "Generator",
	ClassPromise:              "Promise",
	ClassProxy:                "Object",
	ClassArrayBuffer:          "ArrayBuffer",
	ClassSharedArrayBuffer:    "SharedArrayBuffer",
	ClassDataView:             "DataView",
	ClassAsyncGenerator:       "AsyncGenerator",
	ClassTypedArray:           "TypedArray",
	ClassTextEncoder:          "TextEncoder",
	ClassTextDecoder:          "TextDecoder",
	ClassIntlLocale:           "Intl.Locale",
	ClassIntlSegmenter:        "Intl.Segmenter",
	ClassIntlSegments:         "Segments",
	ClassIntlSegmentIterator:  "Segmenter String Iterator",
	ClassIntlNumberFormat:     "Intl.NumberFormat",
	ClassIntlPluralRules:      "Intl.PluralRules",
	ClassIntlDateTimeFormat:   "Intl.DateTimeFormat",
}

// String returns the class name ("Object", "Array", ...).
func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "Object"
}

// Object flag bits.
const (
	flagExtensible uint8 = 1 << iota
	flagFrozen
	flagSealed
	flagIsPrototype
	flagDict    // named properties live in dict (shape is the dictionary sentinel)
	flagHasLazy // internal is *lazyProps with unresolved entries (or on a global object with pending coldGlobalKeys), *lazyKeys, *pendingCompile, the *Realm of a global object with pending coldGlobalKeys, or the *FunctionData of a %RegExp% with its statics pending
	// flagShared marks a frozen intrinsic shared by every realm of the
	// process. Every mutating path checks it first: [[Set]] and the other
	// boolean methods return false (strict callers throw sharedWriteError,
	// sloppy code ignores it as for any frozen object), the other methods
	// returning error return a TypeError, and the internal bootstrap helpers
	// panic, because a write would be a cross-realm race.
	flagShared
	// flagHostNode marks a host node (hostlazy.go): an object that keeps the
	// Go value it was converted from (a map's in internal, a slice's in its
	// empty slots).
	flagHostNode
)

// denseGrowLimit bounds how far past the dense storage an indexed write may
// land before the object switches that index to sparse storage.
const denseGrowLimit = 1024

// Object is a JavaScript object.
type Object struct {
	shape    *Shape
	slots    []Value // named property storage indexed by Shape slot (shape mode)
	elements []Value // dense indexed properties; Hole() marks absence
	proto    *Object // [[Prototype]]; nil for null
	class    Class
	flags    uint8
	// keepShape marks an object a host built as an API surface
	// (KeepShape): past maxShapeProps it continues in an owned chain, as
	// a global object does, rather than in a dictionary. The byte is
	// padding before internal, so Object keeps its size.
	keepShape bool
	internal  any        // *FunctionData, *ArrayData, *String, *primitiveWrapper, *lazyProps, ...
	dict      *dictProps // dictionary-mode named properties and/or sparse elements
}

// ArrayData is the internal payload of ClassArray objects.
type ArrayData struct {
	length         uint32
	lengthWritable bool
	host           uint8 // a host array node's Go slice type (hostSliceAny, ...); 0 otherwise
}

// primitiveWrapper is the internal payload of Number and Boolean wrappers.
type primitiveWrapper struct {
	value Value
}

// lazyProps holds not-yet-materialized properties: one by one (lazy
// properties, DefineLazyProperty and DefineLazyProperties) or all at once
// (install, a deferred builtin installer). On a global object the lazy
// properties coexist with the cold bindings its realm has yet to define
// (Realm.coldGlobals, builtin_defer.go): the object keeps the table, and
// flagHasLazy, while either is pending.
type lazyProps struct {
	r *Realm
	// defs are the lazy properties in registration order; a defined one
	// keeps its place without its init until none is pending.
	defs    []lazyDef
	install func(*Realm, *Object)
	// index maps the key of each pending definition to its place in defs
	// once defs may be longer than lazyLinear (grow).
	index map[PropertyKey]int32
}

// lazyDef is one lazy property: init(r) creates its value, or batch(r, i)
// for one of DefineLazyProperties.
type lazyDef struct {
	key   PropertyKey
	init  func(*Realm) Value
	batch func(*Realm, int) Value
	i     int
}

var (
	lengthKey    = StringKey(AtomLength)
	prototypeKey = StringKey(AtomPrototype)
)

// Class returns the object's class.
func (o *Object) Class() Class { return o.class }

// ClassName returns the name of the object's class (Class.String).
func (o *Object) ClassName() string { return o.class.String() }

// Shape returns the current shape (the dictionary sentinel in dict mode). An
// unmaterialized host node (hostlazy.go) has no layout yet: Shape
// materializes it.
func (o *Object) Shape() *Shape {
	o.materializeHost()
	return o.shape
}

// Proto returns [[Prototype]] or nil.
func (o *Object) Proto() *Object { return o.proto }

// Internal returns the class-specific payload (nil for an ordinary object,
// including a host map node, whose internal is its Go map).
func (o *Object) Internal() any {
	if o.flags&flagHostNode != 0 && o.class == ClassObject {
		return nil
	}
	return o.internal
}

// SetInternal replaces the class-specific payload (for the builtins).
func (o *Object) SetInternal(v any) {
	o.mustBeMutable()
	o.internal = v
}

// Slot reads named slot i (shape mode only; callers hold a valid ICEntry).
func (o *Object) Slot(i uint32) Value { return o.slots[i] }

// SetSlot writes named slot i. The caller must know the slot is a writable
// data property (an IC hit on a store); shared intrinsics have none.
func (o *Object) SetSlot(i uint32, v Value) {
	o.mustBeMutable()
	o.slots[i] = v
}

// IsExtensible reports [[Extensible]].
func (o *Object) IsExtensible() bool { return o.flags&flagExtensible != 0 }

// IsPrototypeObject reports whether o is used as some object's [[Prototype]].
func (o *Object) IsPrototypeObject() bool { return o.flags&flagIsPrototype != 0 }

// IsShared reports whether o is a frozen intrinsic shared across realms.
func (o *Object) IsShared() bool { return o.flags&flagShared != 0 }

// sharedDefineAllowed reports whether [[DefineOwnProperty]] of desc on the
// shared intrinsic o succeeds: ValidateAndApplyPropertyDescriptor without
// the write. A shared intrinsic is frozen, so an allowed desc changes
// nothing (an empty or identical descriptor, for example).
func (o *Object) sharedDefineAllowed(key PropertyKey, desc PropertyDescriptor) bool {
	cur, ok := o.GetOwnProperty(key)
	if !ok {
		return ValidateAndApplyPropertyDescriptor(nil, nil, key, o.IsExtensible(), desc, nil)
	}
	return ValidateAndApplyPropertyDescriptor(nil, nil, key, o.IsExtensible(), desc, &cur)
}

// mustBeMutable panics on an internal write to a shared intrinsic.
func (o *Object) mustBeMutable() {
	if o.flags&flagShared != 0 {
		panic("engine: write to shared intrinsic " + o.debugString())
	}
}

// sharedWriteError is the TypeError for a JavaScript-visible write to a
// shared intrinsic.
func (r *Realm) sharedWriteError(o *Object, key PropertyKey) error {
	return r.TypeError("Cannot modify property '%s' of shared intrinsic %s", key.GoString(), o.debugString())
}

// readOnlyError is the TypeError of a strict [[Set]] of key that returned
// false with o as the receiver: sharedWriteError for a shared intrinsic, the
// falsish-trap error for a proxy.
func (r *Realm) readOnlyError(o *Object, key PropertyKey) error {
	if o.flags&flagShared != 0 {
		return r.sharedWriteError(o, key)
	}
	return r.falsishError(o, AtomSet, key, "Cannot assign to read only property '%s' of object", key.GoString())
}

// IsDictionaryMode reports whether named properties live in the dictionary
// (materializing an unmaterialized host node first, like Shape).
func (o *Object) IsDictionaryMode() bool {
	o.materializeHost()
	return o.flags&flagDict != 0
}

// IsCallable reports whether the object has a [[Call]] internal method: a
// function, or a proxy whose target was callable.
func (o *Object) IsCallable() bool {
	return o.class == ClassFunction || o.class == ClassProxy && o.isCallableProxy()
}

// FunctionData returns the function payload or nil.
func (o *Object) FunctionData() *FunctionData {
	if o.class != ClassFunction {
		return nil
	}
	fd, _ := o.internal.(*FunctionData)
	return fd
}

// PrimitiveValue returns the wrapped primitive of a Boolean/Number/String/
// BigInt wrapper object.
func (o *Object) PrimitiveValue() (Value, bool) {
	switch o.class {
	case ClassString:
		return StringValue(o.internal.(*String)), true
	case ClassNumber, ClassBoolean:
		return o.internal.(*primitiveWrapper).value, true
	case ClassBigInt:
		return BigIntValue(o.internal.(*BigInt)), true
	}
	return Value{}, false
}

func (o *Object) debugString() string {
	if o.IsCallable() {
		if fd := o.FunctionData(); fd != nil && fd.name != nil {
			return "function " + fd.name.GoString() + "() { [native code] }"
		}
		return "function () { [native code] }"
	}
	if ta, ok := o.internal.(*typedArray); ok { // its tag, as Object.prototype.toString shows it
		return "[object " + typedArrayCtorNames[ta.kind].GoString() + "]"
	}
	return "[object " + o.class.String() + "]"
}

// --- construction -----------------------------------------------------------

// newObject allocates an object with the given shape. During realm bootstrap
// objects come from a slab to keep allocation counts low.
func (r *Realm) newObject(class Class, shape *Shape) *Object {
	var o *Object
	if b := r.boot; b != nil && len(b.objs) < cap(b.objs) {
		n := len(b.objs)
		b.objs = b.objs[:n+1]
		o = &b.objs[n]
	} else {
		o = new(Object)
	}
	r.chargeObject(unsafe.Sizeof(Object{}))
	return initObject(o, class, shape)
}

// initObject fills a zeroed Object as an extensible instance of shape.
func initObject(o *Object, class Class, shape *Shape) *Object {
	o.shape = shape
	o.proto = shape.proto
	o.class = class
	o.flags = flagExtensible
	return o
}

// NewObject creates an ordinary object whose prototype is Object.prototype.
func (r *Realm) NewObject() *Object { return r.newObject(ClassObject, r.plainRoot) }

// Small objects created by literals co-allocate their slot storage with the
// object; the buckets land in the 128-, 160-, 192- and 224-byte size
// classes, the same bytes a separate slot slice would cost.
const smallObjectMax = 8

type object2 struct {
	Object
	buf [2]Value
}

type object4 struct {
	Object
	buf [4]Value
}

type object6 struct {
	Object
	buf [6]Value
}

type object8 struct {
	Object
	buf [smallObjectMax]Value
}

// NewObjectCap creates an ordinary object with room for n named properties
// (an object literal about to be filled by DefineField). An empty literal
// gets room for four properties because it is almost always assigned to
// afterwards.
func (r *Realm) NewObjectCap(n int) *Object {
	var o *Object
	switch {
	case n <= 2 && n > 0:
		x := &object2{}
		o, x.slots = &x.Object, x.buf[:0:2]
	case n <= 4:
		x := &object4{}
		o, x.slots = &x.Object, x.buf[:0:4]
	case n <= 6:
		x := &object6{}
		o, x.slots = &x.Object, x.buf[:0:6]
	case n <= smallObjectMax:
		x := &object8{}
		o, x.slots = &x.Object, x.buf[:0:smallObjectMax]
	default:
		o = &Object{slots: make([]Value, 0, n)}
	}
	r.chargeObject(unsafe.Sizeof(Object{}) + uintptr(cap(o.slots)*valueSize))
	return initObject(o, ClassObject, r.plainRoot)
}

// NewObjectWithProto creates an ordinary object with the given prototype
// (nil for a null prototype), as Object.create does.
func (r *Realm) NewObjectWithProto(proto *Object) *Object {
	if proto != nil {
		r.markPrototype(proto)
	}
	return r.newObject(ClassObject, r.rootShapeFor(proto))
}

// markPrototype records that p is some object's [[Prototype]]. Shared
// intrinsics are pre-marked, so they are never written here.
func (r *Realm) markPrototype(p *Object) {
	if p.flags&(flagIsPrototype|flagShared) == 0 {
		p.flags |= flagIsPrototype
	}
}

// bumpEpoch invalidates prototype-chain inline caches when o is a prototype.
func (r *Realm) bumpEpoch(o *Object) {
	if o.flags&flagIsPrototype != 0 {
		r.protoEpoch += 2 // stays even; odd epochs tag accessor IC entries
	}
}

// --- named property storage ---------------------------------------------------

// lookupNamed finds a named (string/symbol) own property and returns a
// pointer to its storage plus attributes.
func (o *Object) lookupNamed(key PropertyKey) (*Value, uint8, bool) {
	if o.flags&flagDict == 0 {
		slot, attrs, ok := o.shape.Lookup(key)
		if !ok {
			return nil, 0, false
		}
		return &o.slots[slot], attrs, true
	}
	c, ok := o.dict.lookup(key)
	if !ok {
		return nil, 0, false
	}
	return &c.value, c.attrs, true
}

// addNamed appends a named property that is known to be absent.
func (o *Object) addNamed(r *Realm, key PropertyKey, cell propCell) {
	if o.flags&flagDict == 0 {
		if o.shape.count < maxShapeProps {
			o.shape = o.shape.addProperty(r, key, cell.attrs)
			c := cap(o.slots)
			if o.slots = append(o.slots, cell.value); cap(o.slots) != c {
				r.charge(cap(o.slots) * valueSize)
			}
			r.bumpEpoch(o)
			return
		}
		if o.shape.owned || o == r.Global || o.keepShape {
			o.addOwnedNamed(r, key, cell)
			return
		}
		o.toDictionary(r)
	}
	o.dict.add(key, cell)
	r.charge(dictEntrySize)
	r.bumpEpoch(o)
}

// KeepShape marks o, an object a host builds as an API surface (a
// module's exports, a class's prototype, a namespace of functions), to
// keep a shape past maxShapeProps (64) named properties, as the global
// object does: its properties continue in a chain of shapes of its own
// (Shape.addOwned), so that reads of them keep their inline caches, where
// any other object moves to dictionary mode. The chain changes only at its
// end; a deletion before the end, new attributes, a new prototype or
// freezing moves o to dictionary mode, as any other change of an object
// past maxShapeProps does. What JavaScript sees of o is the same either
// way. Mark o before it gets its 65th property; an object in dictionary
// mode stays there.
func (o *Object) KeepShape() { o.keepShape = true }

// addOwnedNamed is addNamed for a global object past maxShapeProps, which
// continues in an owned chain (Shape.addOwned) rather than in a dictionary:
// its global variables keep their inline caches. An object a host marked
// with KeepShape does the same.
func (o *Object) addOwnedNamed(r *Realm, key PropertyKey, cell propCell) {
	o.shape = o.shape.addOwned(r, key, cell.attrs)
	c := cap(o.slots)
	if o.slots = append(o.slots, cell.value); cap(o.slots) != c {
		r.charge(cap(o.slots) * valueSize)
	}
	r.bumpEpoch(o)
}

// replaceNamed overwrites a named property that is known to be present.
func (o *Object) replaceNamed(r *Realm, key PropertyKey, cell propCell) {
	if o.flags&flagDict == 0 {
		slot, attrs, _ := o.shape.Lookup(key)
		o.slots[slot] = cell.value
		if attrs == cell.attrs {
			return
		}
		if !o.shape.owned {
			o.shape = o.shape.replaceAttrs(r, key, cell.attrs)
			r.bumpEpoch(o)
			return
		}
		o.toDictionary(r) // an owned chain changes only at its end (Shape.addOwned)
	}
	c, _ := o.dict.lookup(key)
	if c.attrs != cell.attrs {
		r.bumpEpoch(o)
	}
	*c = cell
}

// removeNamed deletes a named property that is known to be present.
func (o *Object) removeNamed(r *Realm, key PropertyKey) {
	if o.flags&flagDict == 0 {
		slot, _, _ := o.shape.Lookup(key)
		if slot == o.shape.count-1 {
			if o.shape.owned {
				o.shape = o.shape.ownedPop(r)
			} else {
				o.shape = o.shape.parent
			}
			o.slots[slot] = Value{}
			o.slots = o.slots[:slot]
			r.bumpEpoch(o)
			return
		}
		o.toDictionary(r)
	}
	o.dict.remove(key)
	r.bumpEpoch(o)
}

// toDictionary moves named properties into the dictionary and points the
// shape at the realm's sentinel so inline caches always miss.
func (o *Object) toDictionary(r *Realm) {
	if o.dict == nil {
		o.dict = &dictProps{}
	}
	props := o.shape.Props()
	o.dict.index = make(map[PropertyKey]int32, len(props)+8)
	o.dict.entries = make([]dictEntry, 0, len(props)+8)
	r.charge((len(props) + 8) * dictEntrySize)
	for i, p := range props {
		o.dict.add(p.key, propCell{value: o.slots[i], attrs: p.attrs})
	}
	o.shape = dictShape
	o.slots = nil
	o.flags |= flagDict
}

// --- indexed property storage ---------------------------------------------------

// elementAttrs returns the attributes of dense elements.
func (o *Object) elementAttrs() uint8 {
	attrs := attrDefault
	if o.flags&flagSealed != 0 {
		attrs &^= attrConfigurable
	}
	if o.flags&flagFrozen != 0 {
		attrs &^= attrWritable | attrConfigurable
	}
	return attrs
}

// lookupIndex finds an own indexed property.
func (o *Object) lookupIndex(i uint32) (propCell, bool) {
	if int(i) < len(o.elements) {
		if v := o.elements[i]; !v.IsHole() {
			return propCell{value: v, attrs: o.elementAttrs()}, true
		}
	}
	if o.dict != nil && o.dict.sparse != nil {
		c, ok := o.dict.sparse[i]
		if ok && o.class == ClassArguments {
			if v, mapped := o.mappedValue(i); mapped {
				c.value = v
			}
		}
		return c, ok
	}
	if o.flags&flagHasLazy != 0 && o.materializeHost() {
		return o.lookupIndex(i)
	}
	return propCell{}, false
}

func (o *Object) sparseMap() map[uint32]propCell {
	if o.dict == nil {
		o.dict = &dictProps{}
	}
	if o.dict.sparse == nil {
		o.dict.sparse = make(map[uint32]propCell)
	}
	return o.dict.sparse
}

// addIndex stores an absent indexed property.
func (o *Object) addIndex(r *Realm, i uint32, cell propCell) {
	if o.class == ClassArray {
		ad := o.internal.(*ArrayData)
		if i >= ad.length {
			ad.length = i + 1
		}
	}
	n := len(o.elements)
	if cell.attrs == o.elementAttrs() && int(i) < n+denseGrowLimit && (o.dict == nil || o.dict.sparse == nil) {
		switch {
		case int(i) < n:
			o.elements[i] = cell.value
		case int(i) == n:
			o.elements = r.appendValue(o.elements, cell.value)
		default:
			o.elements = r.growWithHoles(o.elements, int(i)+1)
			o.elements[i] = cell.value
		}
		r.bumpEpoch(o)
		return
	}
	o.sparseMap()[i] = cell
	r.charge(sparseEntrySize)
	r.bumpEpoch(o)
}

// growWithHoles is growWithHoles charging the storage it allocates.
func (r *Realm) growWithHoles(elements []Value, n int) []Value {
	c := cap(elements)
	if elements = growWithHoles(elements, n); cap(elements) != c {
		r.charge(cap(elements) * valueSize)
	}
	return elements
}

// growWithHoles extends elements to length n, filling new slots with holes.
func growWithHoles(elements []Value, n int) []Value {
	old := len(elements)
	if cap(elements) >= n {
		elements = elements[:n]
	} else {
		ne := make([]Value, n, max(n, 2*cap(elements)))
		copy(ne, elements)
		elements = ne
	}
	hole := Hole()
	for i := old; i < n; i++ {
		elements[i] = hole
	}
	return elements
}

// replaceIndex overwrites a present indexed property.
func (o *Object) replaceIndex(r *Realm, i uint32, cell propCell) {
	if int(i) < len(o.elements) && !o.elements[i].IsHole() {
		if cell.attrs == o.elementAttrs() {
			o.elements[i] = cell.value
			return
		}
		o.elements[i] = Hole()
		o.sparseMap()[i] = cell
		r.bumpEpoch(o)
		return
	}
	prev := o.dict.sparse[i]
	if prev.attrs != cell.attrs {
		r.bumpEpoch(o)
	}
	o.dict.sparse[i] = cell
}

// removeIndex deletes a present indexed property.
func (o *Object) removeIndex(r *Realm, i uint32) {
	if int(i) < len(o.elements) && !o.elements[i].IsHole() {
		o.elements[i] = Hole()
		if int(i) == len(o.elements)-1 {
			o.trimTrailingHoles()
		}
		r.bumpEpoch(o)
		return
	}
	delete(o.dict.sparse, i)
	if o.class == ClassArguments {
		o.unmapArgument(i)
	}
	r.bumpEpoch(o)
}

func (o *Object) trimTrailingHoles() {
	n := len(o.elements)
	for n > 0 && o.elements[n-1].IsHole() {
		n--
	}
	clear(o.elements[n:])
	o.elements = o.elements[:n]
}

// --- own property access ---------------------------------------------------------

// getOwnCell returns the storage form of an own property, synthesizing
// descriptors for exotic properties (array length, string indices).
func (o *Object) getOwnCell(key PropertyKey) (propCell, bool) {
	if key.IsIndex() {
		switch o.class {
		case ClassString:
			s := o.internal.(*String)
			if i := key.Index(); int(i) < s.Len() {
				return propCell{value: StringValue(charString(s.At(int(i)))), attrs: attrEnumerable}, true
			}
		case ClassTypedArray:
			return typedArrayOwnCell(o, key)
		}
		return o.lookupIndex(key.Index())
	}
	if key == lengthKey {
		switch o.class {
		case ClassArray:
			ad := o.internal.(*ArrayData)
			attrs := uint8(0)
			if ad.lengthWritable {
				attrs = attrWritable
			}
			return propCell{value: Uint32Value(ad.length), attrs: attrs}, true
		case ClassString:
			return propCell{value: IntValue(o.internal.(*String).Len()), attrs: 0}, true
		}
	}
	if o.flags&flagHasLazy != 0 && !o.resolveLazy(key) {
		return propCell{}, false
	}
	v, attrs, ok := o.lookupNamed(key)
	if !ok {
		return propCell{}, false
	}
	if attrs&attrLazy != 0 {
		// The lazily created `prototype` of an ordinary function (interp.go):
		// the slot holds the hole marker until the first read.
		if v.IsHole() {
			o.materializePrototype(v)
		}
		attrs &^= attrLazy
	}
	return propCell{value: *v, attrs: attrs}, true
}

// GetOwnProperty implements [[GetOwnProperty]].
func (o *Object) GetOwnProperty(key PropertyKey) (PropertyDescriptor, bool) {
	c, ok := o.getOwnCell(key)
	if !ok {
		return PropertyDescriptor{}, false
	}
	return descriptorFromCell(c), true
}

// HasOwnProperty reports whether key is an own property.
func (o *Object) HasOwnProperty(key PropertyKey) bool {
	_, ok := o.getOwnCell(key)
	return ok
}

// HasProperty implements [[HasProperty]] for a chain without proxies: it
// stops at a proxy, which has no own properties and a null prototype for
// the infallible methods. Realm.hasProperty runs the has trap.
func (o *Object) HasProperty(key PropertyKey) bool {
	for obj := o; obj != nil; obj = obj.proto {
		if obj.HasOwnProperty(key) {
			return true
		}
		if obj.class == ClassTypedArray && typedArrayKey(key) {
			return false
		}
	}
	return false
}

// GetOwnDataValue returns the value of an own data property without invoking
// getters, or false when absent or an accessor. It never runs user code.
func (o *Object) GetOwnDataValue(key PropertyKey) (Value, bool) {
	c, ok := o.getOwnCell(key)
	if !ok || c.attrs&attrAccessor != 0 {
		return Value{}, false
	}
	return c.value, true
}

// Get implements [[Get]] with an explicit receiver.
func (o *Object) Get(r *Realm, key PropertyKey, receiver Value) (Value, error) {
	obj := o
	for ; ; obj = obj.proto {
		c, ok := obj.getOwnCell(key)
		if !ok {
			// A typed array answers a numeric key itself.
			if obj.proto == nil || obj.class == ClassTypedArray && typedArrayKey(key) {
				break
			}
			continue
		}
		if c.attrs&attrAccessor == 0 {
			return c.value, nil
		}
		a := c.value.asAccessor()
		if a.Get == nil {
			return Undefined(), nil
		}
		return r.Call(ObjectValue(a.Get), receiver, nil)
	}
	// A proxy's proto is nil, so it can only end the chain; checking there
	// keeps the trap call out of the walk.
	if obj.class == ClassProxy {
		return r.proxyGet(obj, key, receiver)
	}
	return Undefined(), nil
}

// GetProp is [[Get]] with the object itself as receiver.
func (o *Object) GetProp(r *Realm, key PropertyKey) (Value, error) {
	return o.Get(r, key, ObjectValue(o))
}

// Lookup is [[Get]] with the object itself as receiver that also reports
// whether the property was found, for hosts that tell an absent property
// from an undefined one: found is false when neither the object nor its
// prototype chain has it. No has trap runs: a proxy ending the chain answers
// with its get trap, found, or without one continues on its target.
func (o *Object) Lookup(r *Realm, key PropertyKey) (v Value, found bool, err error) {
	return o.lookup(r, key, ObjectValue(o))
}

func (o *Object) lookup(r *Realm, key PropertyKey, receiver Value) (Value, bool, error) {
	obj := o
	for ; ; obj = obj.proto {
		c, ok := obj.getOwnCell(key)
		if !ok {
			if obj.proto == nil {
				break
			}
			continue
		}
		if c.attrs&attrAccessor == 0 {
			return c.value, true, nil
		}
		a := c.value.asAccessor()
		if a.Get == nil {
			return Undefined(), true, nil
		}
		v, err := r.Call(ObjectValue(a.Get), receiver, nil)
		return v, true, err
	}
	if obj.class == ClassProxy {
		return r.proxyLookup(obj, key, receiver)
	}
	return Undefined(), false, nil
}

// GetIndex is [[Get]] for an array index.
func (o *Object) GetIndex(r *Realm, i uint32) (Value, error) {
	if int(i) < len(o.elements) && o.class != ClassString {
		if v := o.elements[i]; !v.IsHole() {
			return v, nil
		}
	}
	return o.Get(r, IndexKey(i), ObjectValue(o))
}

// Set implements OrdinarySet. It returns false when the assignment is
// rejected; strict-mode callers turn that into a TypeError (Realm.SetProp).
// A shared intrinsic as the receiver rejects every assignment before any
// setter runs.
func (o *Object) Set(r *Realm, key PropertyKey, v Value, receiver Value) (bool, error) {
	// Fast path: receiver is o and the property is an own writable data slot.
	if receiver.IsObject() && receiver.AsObject() == o {
		if o.flags&flagShared != 0 {
			return false, nil
		}
		if key.IsIndex() {
			if i := key.Index(); int(i) < len(o.elements) && o.class != ClassString {
				if !o.elements[i].IsHole() && o.flags&flagFrozen == 0 {
					o.elements[i] = v
					return true, nil
				}
			}
			if o.setAbsentIndex(r, key.Index(), v) {
				return true, nil
			}
		} else if o.class != ClassArray || key != lengthKey {
			if o.flags&flagHasLazy == 0 || o.resolveLazyWrite(key) {
				if p, attrs, ok := o.lookupNamed(key); ok && attrs&attrAccessor == 0 {
					if attrs&attrWritable == 0 {
						return false, nil
					}
					*p = v
					return true, nil
				}
			}
		}
	}
	// Generic path: find the first own property along the chain.
	obj := o
	for ; ; obj = obj.proto {
		c, ok := obj.getOwnCell(key)
		if !ok {
			// A typed array answers a numeric key itself. Its valid indices
			// take the found path: receiverSet redefines the element through
			// typedArrayDefine, as TypedArraySetElement would.
			if obj.class == ClassTypedArray && typedArrayKey(key) {
				return r.typedArraySet(obj, key, v, receiver)
			}
			if obj.proto == nil {
				break
			}
			continue
		}
		if c.attrs&attrAccessor != 0 {
			a := c.value.asAccessor()
			if a.Set == nil {
				return false, nil
			}
			_, err := r.Call(ObjectValue(a.Set), receiver, []Value{v})
			return err == nil, err
		}
		if !receiver.IsObject() {
			return false, nil
		}
		recv := receiver.AsObject()
		// A non-writable inherited data property rejects the assignment,
		// except when it lives on a shared frozen intrinsic and the receiver
		// is a different object: then the property is shadowed on the
		// receiver (the Hardened JS "override mistake" fix), so plugin code
		// such as `err.name = "X"` keeps working with frozen intrinsics.
		if c.attrs&attrWritable == 0 && (obj.flags&flagShared == 0 || obj == recv) {
			return false, nil
		}
		return r.receiverSet(recv, key, v)
	}
	if obj.class == ClassProxy { // only at the chain's end, as in Get
		return r.proxySet(obj, key, v, receiver)
	}
	if !receiver.IsObject() {
		return false, nil
	}
	if recv := receiver.AsObject(); recv != o {
		// The receiver may have the property (Reflect.set, or a proxy
		// forwarding its receiver): update it rather than redefine it.
		return r.receiverSet(recv, key, v)
	}
	return o.createAbsent(r, key, v)
}

// createAbsent is CreateDataProperty(o, key, v) for a key o was just found
// not to have. An ordinary extensible object gets the property directly:
// ValidateAndApplyPropertyDescriptor of a new default data property only
// checks extensibility. Lazy, shared, proxy and typed array objects, and
// indices on objects other than arrays and ordinary ones, take
// DefineOwnProperty.
func (o *Object) createAbsent(r *Realm, key PropertyKey, v Value) (bool, error) {
	if o.flags&(flagHasLazy|flagShared) == 0 && o.class != ClassProxy && o.class != ClassTypedArray {
		if !key.IsIndex() {
			if o.flags&flagExtensible == 0 {
				return false, nil
			}
			o.addNamed(r, key, propCell{value: v, attrs: attrDefault})
			return true, nil
		}
		if o.class == ClassObject || o.class == ClassArray {
			if o.flags&flagExtensible == 0 || o.class == ClassArray && key.Index() >= o.internal.(*ArrayData).length && !o.internal.(*ArrayData).lengthWritable {
				return false, nil
			}
			o.addIndex(r, key.Index(), propCell{value: v, attrs: attrDefault})
			return true, nil
		}
	}
	return o.DefineOwnProperty(r, key, DataDescriptor(v, attrDefault))
}

// setAbsentIndex is OrdinarySet of index i on o with o as the receiver when
// it adds the element to the dense storage: o is an extensible array or
// ordinary object without sparse elements, nothing on its prototype chain
// has indexed properties (indexFreeProtos), and i is a hole in the storage
// or the index right after it. An array's length grows past i if it is
// writable. It reports whether it did.
func (o *Object) setAbsentIndex(r *Realm, i uint32, v Value) bool {
	n := len(o.elements)
	if int(i) > n || int(i) < n && !o.elements[i].IsHole() ||
		o.flags&(flagExtensible|flagHasLazy) != flagExtensible || o.dict != nil && o.dict.sparse != nil || !indexFreeProtos(o) {
		return false
	}
	switch o.class {
	case ClassArray:
		if ad := o.internal.(*ArrayData); i >= ad.length {
			if !ad.lengthWritable {
				return false
			}
			ad.length = i + 1
		}
	case ClassObject:
	default:
		return false
	}
	if int(i) < n {
		o.elements[i] = v
	} else {
		o.elements = r.appendValue(o.elements, v)
	}
	r.bumpEpoch(o)
	return true
}

// SetProp is [[Set]] with the object as receiver, throwing a TypeError in
// the strict-mode manner when the assignment is rejected.
func (o *Object) SetProp(r *Realm, key PropertyKey, v Value) error {
	ok, err := o.Set(r, key, v, ObjectValue(o))
	if err != nil {
		return err
	}
	if !ok {
		return r.readOnlyError(o, key)
	}
	return nil
}

// CreateDataProperty implements CreateDataProperty.
func (o *Object) CreateDataProperty(r *Realm, key PropertyKey, v Value) (bool, error) {
	return o.DefineOwnProperty(r, key, DataDescriptor(v, attrDefault))
}

// CreateDataPropertyOrThrow implements CreateDataPropertyOrThrow.
func (o *Object) CreateDataPropertyOrThrow(r *Realm, key PropertyKey, v Value) error {
	ok, err := o.CreateDataProperty(r, key, v)
	if err != nil {
		return err
	}
	if !ok {
		return r.TypeError("Cannot define property '%s'", key.GoString())
	}
	return nil
}

// DefineOwnProperty implements [[DefineOwnProperty]] including the Array and
// String exotic behaviour. The error is non-nil only for RangeError on an
// invalid array length or when coercing the length value throws.
func (o *Object) DefineOwnProperty(r *Realm, key PropertyKey, desc PropertyDescriptor) (bool, error) {
	if o.flags&flagShared != 0 {
		if o.sharedDefineAllowed(key, desc) {
			return true, nil
		}
		return false, r.sharedWriteError(o, key)
	}
	if o.flags&flagHasLazy != 0 && !o.materializeHost() && !key.IsIndex() {
		o.resolveLazyWrite(key)
	}
	if o.class != ClassObject { // one branch for the common ordinary object
		switch o.class {
		case ClassProxy:
			return r.proxyDefineOwnProperty(o, key, desc)
		case ClassArray:
			if key == lengthKey {
				return o.arraySetLength(r, desc)
			}
			if key.IsIndex() {
				ad := o.internal.(*ArrayData)
				if key.Index() >= ad.length && !ad.lengthWritable {
					return false, nil
				}
			}
		case ClassString:
			if key == lengthKey || (key.IsIndex() && int(key.Index()) < o.internal.(*String).Len()) {
				cur, _ := o.GetOwnProperty(key)
				return ValidateAndApplyPropertyDescriptor(r, nil, key, o.IsExtensible(), desc, &cur), nil
			}
		case ClassTypedArray:
			if typedArrayKey(key) {
				return r.typedArrayDefine(o, key, desc)
			}
		case ClassArguments:
			if key.IsIndex() && o.internal != nil {
				return r.argumentsDefine(o, key, desc), nil
			}
		}
	}
	cur, ok := o.GetOwnProperty(key)
	if !ok {
		return ValidateAndApplyPropertyDescriptor(r, o, key, o.IsExtensible(), desc, nil), nil
	}
	return ValidateAndApplyPropertyDescriptor(r, o, key, o.IsExtensible(), desc, &cur), nil
}

// DefinePropertyOrThrow implements DefinePropertyOrThrow.
func (o *Object) DefinePropertyOrThrow(r *Realm, key PropertyKey, desc PropertyDescriptor) error {
	ok, err := o.DefineOwnProperty(r, key, desc)
	if err != nil {
		return err
	}
	if !ok {
		return r.falsishError(o, AtomDefineProperty, key, "Cannot redefine property: %s", key.GoString())
	}
	return nil
}

// addProp stores an absent property (either storage kind).
func (o *Object) addProp(r *Realm, key PropertyKey, cell propCell) {
	if key.IsIndex() {
		o.addIndex(r, key.Index(), cell)
		return
	}
	o.addNamed(r, key, cell)
}

// replaceProp overwrites a present property (either storage kind).
func (o *Object) replaceProp(r *Realm, key PropertyKey, cell propCell) {
	if key.IsIndex() {
		o.replaceIndex(r, key.Index(), cell)
		return
	}
	o.replaceNamed(r, key, cell)
}

// Delete implements [[Delete]] for an ordinary object. It returns false when
// the property exists and is not configurable. It runs no trap
// (Realm.deleteProperty does).
func (o *Object) Delete(r *Realm, key PropertyKey) bool {
	if o.flags&flagHasLazy != 0 {
		o.resolveLazyWrite(key)
	}
	if o.class == ClassTypedArray && typedArrayKey(key) {
		_, valid := o.internal.(*typedArray).index(key)
		return !valid
	}
	c, ok := o.getOwnCell(key)
	if !ok {
		return true
	}
	if c.attrs&attrConfigurable == 0 || o.flags&flagShared != 0 {
		return false
	}
	if key.IsIndex() {
		o.removeIndex(r, key.Index())
		return true
	}
	o.removeNamed(r, key)
	return true
}

// DeletePropertyOrThrow implements the strict-mode `delete` operator.
func (o *Object) DeletePropertyOrThrow(r *Realm, key PropertyKey) error {
	ok, err := r.deleteProperty(o, key)
	if err != nil {
		return err
	}
	if !ok {
		return r.falsishError(o, AtomDeleteProperty, key, "Cannot delete property '%s' of %s", key.GoString(), o.debugString())
	}
	return nil
}

// DefineOwnDataFast adds a data property to an object that is known not to
// have it yet (fresh objects during bootstrap, literals, host conversion).
func (o *Object) DefineOwnDataFast(r *Realm, key PropertyKey, v Value, attrs uint8) {
	o.mustBeMutable()
	o.addProp(r, key, propCell{value: v, attrs: attrs &^ attrAccessor})
}

// DefineOwnAccessorFast adds an accessor property known to be absent.
func (o *Object) DefineOwnAccessorFast(r *Realm, key PropertyKey, get, set *Object, attrs uint8) {
	o.mustBeMutable()
	o.addProp(r, key, propCell{
		value: accessorValue(&Accessor{Get: get, Set: set}),
		attrs: (attrs &^ attrWritable) | attrAccessor,
	})
}

// --- key enumeration ------------------------------------------------------------

// OwnPropertyKeys implements [[OwnPropertyKeys]]: integer indices ascending,
// then strings in insertion order, then symbols in insertion order.
func (o *Object) OwnPropertyKeys() []PropertyKey {
	if o.flags&flagHasLazy != 0 {
		o.resolveAllLazy()
	}
	keys := make([]PropertyKey, 0, o.keyCountHint())
	keys = o.appendIndexKeys(keys)
	if o.class == ClassArray || o.class == ClassString {
		keys = append(keys, lengthKey)
	}
	keys = o.appendNamedKeys(keys, true)
	keys = o.appendNamedKeys(keys, false)
	return keys
}

// OwnEnumerableStringKeys returns own enumerable string keys in spec order
// (Object.keys, JSON.stringify, for-in). Indices are returned as index keys.
func (o *Object) OwnEnumerableStringKeys() []PropertyKey {
	if o.flags&flagHasLazy != 0 {
		o.resolveAllLazy()
	}
	keys := make([]PropertyKey, 0, o.keyCountHint())
	if o.flags&(flagSealed|flagFrozen) != 0 || (o.dict != nil && o.dict.sparse != nil) {
		all := o.appendIndexKeys(nil)
		for _, k := range all {
			if c, ok := o.getOwnCell(k); ok && c.attrs&attrEnumerable != 0 {
				keys = append(keys, k)
			}
		}
	} else {
		keys = o.appendIndexKeys(keys)
	}
	if o.flags&flagDict == 0 {
		for _, p := range o.shape.Props() {
			if p.attrs&attrEnumerable != 0 && p.key.IsString() {
				keys = append(keys, p.key)
			}
		}
		return keys
	}
	for i := range o.dict.entries {
		e := &o.dict.entries[i]
		if e.live && e.cell.attrs&attrEnumerable != 0 && e.key.IsString() {
			keys = append(keys, e.key)
		}
	}
	return keys
}

func (o *Object) keyCountHint() int {
	n := len(o.elements)
	if o.flags&flagDict == 0 {
		n += int(o.shape.count)
	} else {
		n += o.dict.namedCount()
	}
	if o.dict != nil {
		n += len(o.dict.sparse)
	}
	return n + 1
}

func (o *Object) appendIndexKeys(keys []PropertyKey) []PropertyKey {
	if o.class == ClassTypedArray {
		// A typed array's indices are its elements alone (Realm.ownPropertyKeys
		// bounds how many).
		n := max(o.internal.(*typedArray).length(), 0)
		keys = slices.Grow(keys, n)
		for i := range n {
			keys = append(keys, IndexKey(uint32(i)))
		}
		return keys
	}
	if o.class == ClassString {
		n := o.internal.(*String).Len()
		for i := range n {
			keys = append(keys, IndexKey(uint32(i)))
		}
	}
	var sparse []uint32
	if o.dict != nil {
		sparse = o.dict.sparseKeys()
	}
	si := 0
	for i, v := range o.elements {
		if v.IsHole() {
			continue
		}
		for si < len(sparse) && int(sparse[si]) < i {
			keys = append(keys, IndexKey(sparse[si]))
			si++
		}
		keys = append(keys, IndexKey(uint32(i)))
	}
	for ; si < len(sparse); si++ {
		keys = append(keys, IndexKey(sparse[si]))
	}
	return keys
}

// appendNamedKeys appends the string (or symbol) keys in insertion order.
// Private names share the named storage but are never property keys.
func (o *Object) appendNamedKeys(keys []PropertyKey, strings bool) []PropertyKey {
	if o.flags&flagDict == 0 {
		for _, p := range o.shape.Props() {
			if p.key.IsString() == strings && !p.key.IsPrivate() {
				keys = append(keys, p.key)
			}
		}
		return keys
	}
	for i := range o.dict.entries {
		e := &o.dict.entries[i]
		if e.live && e.key.IsString() == strings && !e.key.IsPrivate() {
			keys = append(keys, e.key)
		}
	}
	return keys
}

// --- prototype and integrity -----------------------------------------------------

// SetPrototypeOf implements OrdinarySetPrototypeOf; the realm's
// Object.prototype is an immutable prototype exotic object.
func (o *Object) SetPrototypeOf(r *Realm, proto *Object) bool {
	if proto == o.proto {
		return true
	}
	if !o.IsExtensible() || o.flags&flagShared != 0 || o == r.ObjectPrototype {
		return false
	}
	for p := proto; p != nil; p = p.proto {
		if p == o {
			return false
		}
	}
	if o.flags&flagHasLazy != 0 {
		o.resolveAllLazy() // a lazy object keeps a shape no other object has
	}
	o.proto = proto
	if proto != nil {
		r.markPrototype(proto)
	}
	if o.flags&flagDict == 0 {
		if o.shape.owned {
			o.toDictionary(r) // an owned chain changes only at its end (Shape.addOwned)
		} else {
			o.shape = o.shape.rebase(r, r.rootShapeFor(proto))
		}
	}
	r.bumpEpoch(o)
	return true
}

// PreventExtensions implements [[PreventExtensions]]. It is a no-op on
// shared intrinsics, which are already non-extensible.
func (o *Object) PreventExtensions(r *Realm) {
	if o.flags&flagShared != 0 {
		return
	}
	if o.flags&flagHasLazy != 0 {
		o.resolveAllLazy() // no property may appear after this
	}
	o.flags &^= flagExtensible
	r.bumpEpoch(o)
}

// Seal makes every own property non-configurable and prevents extensions.
func (o *Object) Seal(r *Realm) {
	if o.flags&flagShared != 0 {
		return
	}
	o.PreventExtensions(r)
	o.flags |= flagSealed
	o.rewriteAttrs(r, func(a uint8) uint8 { return a &^ attrConfigurable })
}

// Freeze makes every own data property non-writable and non-configurable,
// every accessor non-configurable, and prevents extensions.
func (o *Object) Freeze(r *Realm) {
	if o.flags&flagShared != 0 {
		return
	}
	if o.class == ClassArguments {
		o.freezeArguments()
	}
	o.PreventExtensions(r)
	o.flags |= flagSealed | flagFrozen
	o.rewriteAttrs(r, func(a uint8) uint8 {
		if a&attrAccessor != 0 {
			return a &^ attrConfigurable
		}
		return a &^ (attrWritable | attrConfigurable)
	})
	if o.class == ClassArray {
		o.internal.(*ArrayData).lengthWritable = false
	}
}

func (o *Object) rewriteAttrs(r *Realm, fn func(uint8) uint8) {
	if o.flags&flagDict == 0 && o.shape.owned {
		o.toDictionary(r) // an owned chain changes only at its end (Shape.addOwned)
	}
	if o.flags&flagDict == 0 {
		if o.shape.count != 0 {
			o.shape = o.shape.mapAttrs(r, fn)
		}
	} else {
		for i := range o.dict.entries {
			e := &o.dict.entries[i]
			if e.live {
				e.cell.attrs = fn(e.cell.attrs)
			}
		}
	}
	if o.dict != nil {
		for k, c := range o.dict.sparse {
			c.attrs = fn(c.attrs)
			o.dict.sparse[k] = c
		}
	}
	r.bumpEpoch(o)
}

// IsFrozen implements TestIntegrityLevel(frozen).
func (o *Object) IsFrozen() bool {
	if o.IsExtensible() || o.class == ClassTypedArray && o.internal.(*typedArray).length() > 0 {
		return false // a typed array's elements are writable and configurable
	}
	if o.flags&flagFrozen != 0 {
		return true
	}
	for _, k := range o.OwnPropertyKeys() {
		c, _ := o.getOwnCell(k)
		if c.attrs&attrConfigurable != 0 {
			return false
		}
		if c.attrs&attrAccessor == 0 && c.attrs&attrWritable != 0 {
			return false
		}
	}
	return true
}

// IsSealed implements TestIntegrityLevel(sealed).
func (o *Object) IsSealed() bool {
	if o.IsExtensible() || o.class == ClassTypedArray && o.internal.(*typedArray).length() > 0 {
		return false
	}
	if o.flags&flagSealed != 0 {
		return true
	}
	for _, k := range o.OwnPropertyKeys() {
		if c, _ := o.getOwnCell(k); c.attrs&attrConfigurable != 0 {
			return false
		}
	}
	return true
}

// --- lazy properties -------------------------------------------------------------

// DefineLazyProperty registers a property whose value is computed by init on
// first access. It is used for lazily materialized globals (Math, JSON, ...).
// The property is installed non-enumerable, writable and configurable.
// A key registered again while pending keeps its place and takes the new
// init.
func (o *Object) DefineLazyProperty(r *Realm, key PropertyKey, init func(*Realm) Value) {
	o.lazyTable(r, 1).add(lazyDef{key: key, init: init})
}

// DefineLazyProperties registers the properties keys, as DefineLazyProperty
// does each: the first access to keys[i] defines it with the value of
// init(r, i). A key keys lists more than once is registered once, in the
// place of its first occurrence, with the index of its last. One call costs
// a few allocations however many keys it registers, and on a global object
// it leaves the builtin globals the realm has yet to define pending too.
func (o *Object) DefineLazyProperties(r *Realm, keys []PropertyKey, init func(r *Realm, i int) Value) {
	lp := o.lazyTable(r, len(keys))
	for i, k := range keys {
		lp.add(lazyDef{key: k, batch: init, i: i})
	}
}

// lazyLinear is the length of lazyProps.defs up to which find scans it;
// past it a map indexes the pending definitions.
const lazyLinear = 32

// add registers d, replacing the pending definition of its key.
func (lp *lazyProps) add(d lazyDef) {
	if i := lp.find(d.key); i >= 0 {
		lp.defs[i] = d
		return
	}
	if lp.index != nil {
		lp.index[d.key] = int32(len(lp.defs))
	}
	lp.defs = append(lp.defs, d)
}

// grow makes room for n more definitions, and indexes the pending ones
// once there may be more than lazyLinear.
func (lp *lazyProps) grow(n int) {
	lp.defs = slices.Grow(lp.defs, n)
	if lp.index == nil && len(lp.defs)+n > lazyLinear {
		lp.index = make(map[PropertyKey]int32, len(lp.defs)+n)
		for i := range lp.defs {
			if lp.defs[i].pending() {
				lp.index[lp.defs[i].key] = int32(i)
			}
		}
	}
}

// lazyTable returns the lazy properties of o, with room for n more: a
// lazyKeys object, %RegExp.prototype% with compile pending, %RegExp% with
// its statics pending and an object with a deferred install define their
// properties first, a host placeholder materializes. A global object with
// cold bindings pending keeps them: its table belongs to its realm, which
// defines a cold binding before a lazy property of the same key.
func (o *Object) lazyTable(r *Realm, n int) *lazyProps {
	o.mustBeMutable()
	switch in := o.internal.(type) {
	case *lazyProps:
		if in.install == nil {
			in.grow(n)
			return in
		}
		o.runInstall(in)
	case *Realm:
		r = in
	case *lazyKeys, *pendingCompile:
		o.resolveAllLazy()
	default:
		o.materializeHost()
	}
	lp := &lazyProps{r: r}
	lp.grow(n)
	o.internal = lp
	o.flags |= flagHasLazy
	return lp
}

// lazyPending reports whether a lookup of key on o, which has flagHasLazy,
// would define properties first.
func (o *Object) lazyPending(key PropertyKey) bool {
	switch lp := o.internal.(type) {
	case *lazyProps:
		return lp.install != nil || lp.find(key) >= 0 || o == lp.r.Global && lp.r.coldGlobalIndex(key) >= 0
	case *lazyKeys:
		return lp.isPending(key)
	case *Realm:
		return lp.coldGlobalIndex(key) >= 0
	case *pendingCompile:
		return key == compileKey
	case *FunctionData:
		return isStaticsKey(key)
	}
	return true // a host placeholder, which is never shape-mode, materializes on any lookup
}

// deferInstall postpones install, which defines the properties of o, to the
// first access to any own property or the key list of o. o must not have an
// internal payload. It is used for the builtin prototypes of mutable realms
// that most programs never touch.
func (o *Object) deferInstall(r *Realm, install func(*Realm, *Object)) {
	o.mustBeMutable()
	o.internal = &lazyProps{r: r, install: install}
	o.flags |= flagHasLazy
	if o.flags&flagDict == 0 && !o.shape.shared {
		o.shape.noFill = noFillAll
	}
}

// runInstall runs a deferred installer; o is an ordinary object again when
// it starts.
func (o *Object) runInstall(lp *lazyProps) {
	o.internal = nil
	o.flags &^= flagHasLazy
	lp.install(lp.r, o)
}

// resolveLazy prepares o, which has flagHasLazy, for a lookup of the named
// key: it runs a deferred installer or a pending lazy definition of key,
// defines key on a lazyKeys object, a global object with pending cold
// globals or %RegExp.prototype% (pendingCompile), the statics of %RegExp%
// on a lookup of one of them, or materializes a host placeholder
// (resolveHost). It reports false
// when the lookup must not reach o's storage because key is certainly absent
// (an unmaterialized placeholder has none).
func (o *Object) resolveLazy(key PropertyKey) bool {
	lp, ok := o.internal.(*lazyProps)
	if !ok {
		switch in := o.internal.(type) {
		case *lazyKeys:
			in.resolve(o, key)
			return true
		case *Realm:
			in.resolveColdGlobal(key)
			return true
		case *pendingCompile:
			if key == compileKey {
				in.define(o)
			}
			return true
		case *FunctionData:
			if isStaticsKey(key) {
				definePendingStatics(o, in)
			}
			return true
		}
		return o.resolveHost(key)
	}
	if lp.install != nil {
		o.runInstall(lp)
		return true
	}
	if o == lp.r.Global {
		lp.r.resolveColdGlobal(key)
	}
	if i := lp.find(key); i >= 0 {
		lp.define(o, i)
	}
	return true
}

// find returns the index in lp.defs of the pending lazy property key, -1
// if there is none.
func (lp *lazyProps) find(key PropertyKey) int {
	if lp.index != nil {
		if i, ok := lp.index[key]; ok {
			return int(i)
		}
		return -1
	}
	for i := range lp.defs {
		if lp.defs[i].key == key && lp.defs[i].pending() {
			return i
		}
	}
	return -1
}

// pending reports whether d is still to define.
func (d *lazyDef) pending() bool { return d.init != nil || d.batch != nil }

// anyPending reports whether any definition of lp is pending.
func (lp *lazyProps) anyPending() bool {
	if lp.index != nil {
		return len(lp.index) != 0
	}
	for i := range lp.defs {
		if lp.defs[i].pending() {
			return true
		}
	}
	return false
}

// define defines the pending lazy property lp.defs[i] on o, replacing an
// own property of its key: a cold global binding of the same key, which
// the realm defines first (resolveLazy), or one the host defined itself.
// The definition leaves the table before init runs, so a lookup init makes
// of the key finds it absent rather than running init again. A defined
// entry keeps its place until none is pending, which empties the table.
func (lp *lazyProps) define(o *Object, i int) {
	d := lp.defs[i]
	lp.defs[i].init, lp.defs[i].batch = nil, nil
	if lp.index != nil {
		delete(lp.index, d.key)
	}
	if !lp.anyPending() {
		clear(lp.defs)
		lp.defs, lp.index = lp.defs[:0], nil
		if o != lp.r.Global || lp.r.coldGlobals == 0 {
			o.flags &^= flagHasLazy
			o.internal = nil
		}
	}
	var v Value
	if d.init != nil {
		v = d.init(lp.r)
	} else {
		v = d.batch(lp.r, d.i)
	}
	cell := propCell{value: v, attrs: attrHidden}
	if _, _, ok := o.lookupNamed(d.key); ok {
		o.replaceNamed(lp.r, d.key, cell)
		return
	}
	o.addNamed(lp.r, d.key, cell)
}

// resolveLazyWrite prepares o for a write, redefinition or deletion of
// key: a lazyKeys object, %RegExp.prototype% with compile pending or
// %RegExp% with its statics pending defines all its properties first. It
// reports false like resolveLazy.
func (o *Object) resolveLazyWrite(key PropertyKey) bool {
	switch o.internal.(type) {
	case *lazyKeys, *pendingCompile, *FunctionData:
		o.resolveAllLazy()
		return true
	}
	return o.resolveLazy(key)
}

func (o *Object) resolveAllLazy() {
	lp, ok := o.internal.(*lazyProps)
	if !ok {
		switch in := o.internal.(type) {
		case *lazyKeys:
			in.resolveAll(o)
		case *Realm:
			in.resolveColdGlobals()
		case *pendingCompile:
			in.define(o)
		case *FunctionData:
			definePendingStatics(o, in)
		default:
			o.materializeHost()
		}
		return
	}
	if lp.install != nil {
		o.runInstall(lp)
		return
	}
	// A global object defines its pending cold bindings first, in their
	// order, as it did before the host's lazy properties were registered.
	if o == lp.r.Global {
		lp.r.resolveColdGlobals()
	}
	for i := 0; i < len(lp.defs); i++ {
		if lp.defs[i].pending() {
			lp.define(o, i)
		}
	}
}

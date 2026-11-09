package engine

import (
	"slices"
	"sync/atomic"
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
	flagDict    // named properties live in dict (shape is a dictionary sentinel)
	flagHasLazy // internal is *lazyProps with unresolved entries (or on a global object with pending coldGlobalKeys), *lazyKeys, *pendingCompile, *pendingDescs, the *Realm of a global object with pending coldGlobalKeys, or the *FunctionData of a %RegExp% with its statics pending; or an array's elements are shared (bitSharedElems)
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

// Object is a JavaScript object: a 48-byte header. An array or arguments
// object keeps its dense elements right after the header (elemStore,
// bitInlineElems); any other object keeps them, its dictionary-mode
// properties and its sparse elements in an objectExt. The class-specific
// payload, of the type pkind names, sits right after the header of the
// kinds that co-allocate it; any other is a pointer in ext
// (object_unsafe.go).
type Object struct {
	shape *Shape
	slots []Value // named property storage indexed by Shape slot (shape mode)
	class Class
	flags uint8
	pkind payloadKind // the type of the payload
	bits  uint8       // bitKeepShape, bitInlineElems, bitLengthReadOnly, bitHasExt, the host slice type
	// aux is an array's length and the aliased count of a lazily mapped
	// arguments object (argumentsEnv). It is zero in any object without
	// inline elements: IterNext's array fast mode reads them below it.
	aux uint32
	// ext is the *objectExt with bitHasExt; without, the payload itself, of
	// a kind not co-allocated, or nil.
	ext unsafe.Pointer
}

// objectExt holds what few objects have: dictionary-mode named
// properties, sparse elements, weak-collection entries, the root of the
// shapes inheriting from the object, the dense elements of an object
// that is neither an array nor an arguments object, and the payload of
// a kind not co-allocated with the object.
type objectExt struct {
	dictProps
	root     *Shape // root shape of the realm-local objects inheriting from it (Realm.localRoot)
	elements elemStore
	payload  unsafe.Pointer
}

// Object.bits.
const (
	// bitKeepShape marks an object a host built as an API surface
	// (KeepShape): past maxShapeProps it continues in an owned chain, as a
	// global object does, rather than in a dictionary.
	bitKeepShape uint8 = 1 << iota
	// bitInlineElems marks an object allocated with an elemStore right
	// after its header (inlineObject): every array, and arguments objects.
	bitInlineElems
	// bitLengthReadOnly marks an array whose length is not writable.
	bitLengthReadOnly
	// bitHasExt marks an object whose ext is its objectExt.
	bitHasExt
	// bitSharedElems marks an array whose elements are a run of its code's
	// constants (NewArrayK, newArrayConsts), shared by every array of that
	// literal and never written. Such an array has flagHasLazy too, no
	// payload and no other lazy state, and its element store has a
	// capacity equal to its length, so no append can write into it. elems
	// gives it a copy of its own first, which every path that writes the
	// elements takes; only the reads that elemsReadOnly lists read the
	// shared store. The interpreter's SetElem fast path stops at
	// flagHasLazy, and a path that clears flagHasLazy (a lazy property
	// defined, an object made ordinary) never meets such an array: it gets
	// a payload only through lazyTable, which gives it its copy first.
	// Until then the array keeps its tree's constant slab alive, every
	// constant of the tree (newMetaSlab), which only code that is not
	// dynamic shares this way (newArrayConsts).
	bitSharedElems
	// The top bits hold a host array node's Go slice type (hostSliceAny,
	// ...); 0 otherwise.
	hostKindShift = 5
)

// The accessors of Object's elements, payload, dictionary,
// keepShape bit, array length and rebuilds are the only code that reads or
// writes those fields, so the header's layout can change behind them.

// elems returns the dense indexed properties, which the caller may
// write: an array whose elements are shared (bitSharedElems) gets a copy
// of its own first. The element accessors read the elemStore themselves
// rather than through its methods: a nested inlined call leaves a NOP at
// every call site.
func (o *Object) elems() []Value {
	var e *elemStore
	if o.bits&bitInlineElems != 0 {
		e = &(*inlineObject)(unsafe.Pointer(o)).el
		if o.bits&bitSharedElems != 0 {
			// The copy was charged with the array (newArrayConsts).
			e.p = unsafe.SliceData(append([]Value(nil), unsafe.Slice(e.p, e.n)...))
			o.bits &^= bitSharedElems
			o.flags &^= flagHasLazy
		}
	} else if o.bits&bitHasExt != 0 {
		e = &(*objectExt)(o.ext).elements
	} else {
		return nil
	}
	h := sliceHeader{e.p, int(e.n), int(e.c)}
	return *(*[]Value)(unsafe.Pointer(&h))
}

// elemsReadOnly returns the dense indexed properties as elems does, but
// leaves shared elements (bitSharedElems) shared: the caller only reads
// them, and keeps the slice no longer than elems' callers may. These are
// its callers, each a read:
//
//   - lookupIndex and GetIndex, [[Get]] and [[GetOwnProperty]] of an index
//     (ToGo's export of an array among their callers)
//   - arrayElementAt, the iteration and spread of an array
//   - primitiveElements and join, Array.prototype.join
//   - Array.prototype.slice, concat and the copies of toReversed, toSorted,
//     toSpliced and with (appendDense), which copy the values out
//   - typedArrayFromDense, a typed array made of an array's numbers
//   - forInNext, the key list of a for-in record
//   - copyDataProps, the keys an object rest pattern excludes
//   - the walks of ToGoInto and Unmarshal (jsonDecoder, decodeMeasure)
//   - the live measurement (liveWalker.scan)
//
// The interpreter's GetElem and for-of fast paths, denseElement
// (indexOf, includes, lastIndexOf, join's reads) and JSON.stringify read
// the store as inlineElems or denseElems give it, which is the shared one.
func (o *Object) elemsReadOnly() []Value {
	var e *elemStore
	if o.bits&bitInlineElems != 0 {
		e = &(*inlineObject)(unsafe.Pointer(o)).el
	} else if o.bits&bitHasExt != 0 {
		e = &(*objectExt)(o.ext).elements
	} else {
		return nil
	}
	h := sliceHeader{e.p, int(e.n), int(e.c)}
	return *(*[]Value)(unsafe.Pointer(&h))
}

// elemLen returns len(o.elems()).
func (o *Object) elemLen() int {
	if o.bits&bitInlineElems != 0 {
		return int((*inlineObject)(unsafe.Pointer(o)).el.n)
	}
	if o.bits&bitHasExt != 0 {
		return int((*objectExt)(o.ext).elements.n)
	}
	return 0
}

// denseElems returns the dense elements of an array or arguments object
// and nil for any other object (a String wrapper among them), whose
// elements, if any, the slow paths read: the run loop's element fast
// paths use it.
func (o *Object) denseElems() []Value {
	if o.bits&bitInlineElems == 0 {
		return nil
	}
	e := &(*inlineObject)(unsafe.Pointer(o)).el
	h := sliceHeader{e.p, int(e.n), int(e.c)}
	return *(*[]Value)(unsafe.Pointer(&h))
}

// setElems replaces the dense indexed properties.
func (o *Object) setElems(s []Value) {
	var e *elemStore
	if o.bits&bitInlineElems != 0 {
		e = &(*inlineObject)(unsafe.Pointer(o)).el
	} else if o.bits&bitHasExt != 0 {
		e = &(*objectExt)(o.ext).elements
	} else if cap(s) == 0 {
		return
	} else {
		e = &o.newExt().elements
	}
	e.p, e.n, e.c = unsafe.SliceData(s), uint32(len(s)), uint32(min(uint(cap(s)), 1<<32-1))
}

// funcData returns the payload of a ClassFunction object, which is always
// its *FunctionData, right after its header (setPayload).
func (o *Object) funcData() *FunctionData {
	return (*FunctionData)(unsafe.Add(unsafe.Pointer(o), unsafe.Sizeof(Object{})))
}

// arrayLen returns the length of a ClassArray object.
func (o *Object) arrayLen() uint32 { return o.aux }

// setArrayLen sets the length of a ClassArray object.
func (o *Object) setArrayLen(n uint32) { o.aux = n }

// lengthWritable reports whether the length of a ClassArray object is
// writable.
func (o *Object) lengthWritable() bool { return o.bits&bitLengthReadOnly == 0 }

// setLengthReadOnly makes the length of a ClassArray object non-writable.
func (o *Object) setLengthReadOnly() { o.bits |= bitLengthReadOnly }

// hostSliceKind returns the Go slice type of a host array node (hostSliceAny,
// ...), or 0.
func (o *Object) hostSliceKind() uint8 { return o.bits >> hostKindShift }

// setHostSliceKind records the Go slice type of a host array node.
func (o *Object) setHostSliceKind(k uint8) {
	o.bits = o.bits&(1<<hostKindShift-1) | k<<hostKindShift
}

// dictTable returns the dictionary-mode properties and sparse elements, or
// nil.
func (o *Object) dictTable() *dictProps {
	if o.bits&bitHasExt != 0 {
		return &(*objectExt)(o.ext).dictProps
	}
	return nil
}

// ensureDict returns the dictionary-mode properties and sparse elements,
// creating the table if needed.
func (o *Object) ensureDict() *dictProps {
	x := o.extData()
	if x == nil {
		x = o.newExt()
	}
	return &x.dictProps
}

// extData returns the objectExt, or nil.
func (o *Object) extData() *objectExt {
	if o.bits&bitHasExt == 0 {
		return nil
	}
	return (*objectExt)(o.ext)
}

// newExt gives o, which has no objectExt, one, and moves into it the
// payload ext held.
func (o *Object) newExt() *objectExt {
	x := &objectExt{payload: o.ext}
	o.ext = unsafe.Pointer(x)
	o.bits |= bitHasExt
	return x
}

// chargeExt charges the objectExt that o's elements or dictionary are about
// to allocate, which a script can make one per object.
func (o *Object) chargeExt(r *Realm) {
	if o.extData() == nil {
		r.chargeObject(unsafe.Sizeof(objectExt{}))
	}
}

// keepsShape reports whether a host marked the object with KeepShape.
func (o *Object) keepsShape() bool { return o.bits&bitKeepShape != 0 }

// globalRebuilds returns the shape-mode rebuilds the realm's global object
// has taken (removeNamed).
func (r *Realm) globalRebuilds() uint8 {
	if r.lazy == nil || r.lazy.shapes == nil {
		return 0
	}
	return r.lazy.shapes.globalRebuilds
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

// Shape returns the current shape (a dictionary sentinel in dict mode). An
// unmaterialized host node (hostlazy.go) has no layout yet: Shape
// materializes it.
func (o *Object) Shape() *Shape {
	o.materializeHost()
	return o.shape
}

// Proto returns [[Prototype]] or nil.
func (o *Object) Proto() *Object { return o.shape.proto }

// Internal returns the class-specific payload (nil for an ordinary object,
// including a host map node, whose internal is its Go map).
//
// It is hostPayload, with the host map node, written out to stay
// inlinable.
func (o *Object) Internal() (v any) {
	k, p := o.pkind, o.ext
	if k == 0 || o.flags&flagHostNode != 0 && o.class == ClassObject {
		return nil
	}
	if o.bits&bitHasExt != 0 {
		p = (*objectExt)(p).payload
	}
	if k < firstExtKind {
		p = unsafe.Add(unsafe.Pointer(o), unsafe.Sizeof(Object{}))
	} else if k == kindBoxed {
		return *(*any)(p)
	}
	e := (*[2]unsafe.Pointer)(unsafe.Pointer(&v))
	e[0], e[1] = atomic.LoadPointer(&payloadTypes[k]), p
	return v
}

// SetInternal replaces the class-specific payload (for the builtins).
func (o *Object) SetInternal(v any) {
	o.mustBeMutable()
	o.setPayload(v)
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
	fd := dataOf(o, kindFunction)
	return fd
}

// PrimitiveValue returns the wrapped primitive of a Boolean/Number/String/
// BigInt wrapper object.
func (o *Object) PrimitiveValue() (Value, bool) {
	switch o.class {
	case ClassString:
		return StringValue(extDataOf(o, kindString)), true
	case ClassNumber, ClassBoolean:
		return extDataOf(o, kindPrimitive).value, true
	case ClassBigInt:
		return BigIntValue(extDataOf(o, kindBigInt)), true
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
	if ta, ok := dataOk(o, kindTypedArray); ok { // its tag, as Object.prototype.toString shows it
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
	o.class = class
	o.flags = flagExtensible
	return o
}

// NewObject creates an ordinary object whose prototype is Object.prototype.
func (r *Realm) NewObject() *Object { return r.newObject(ClassObject, r.plainRoot) }

// Small objects co-allocate their slot storage with the object. A literal
// of up to eight properties takes the bucket of two, four, six or eight
// slots that holds them, in the 80- to 176-byte size classes, the bytes a
// separate slot slice would cost: an odd count's spare slot takes the
// property the program often adds next. Up to coSlotsMax, a literal takes
// exactly its slots through thirteen (the 256-byte size class) and one more
// past them, which its size class holds.
const coSlotsMax = 17

// smallObjectMax bounds the room a constructor's objects are given
// (FunctionData.ctorSlots).
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
	buf [8]Value
}

type (
	object9 struct {
		Object
		buf [9]Value
	}
	object10 struct {
		Object
		buf [10]Value
	}
	object11 struct {
		Object
		buf [11]Value
	}
	object13 struct {
		Object
		buf [13]Value
	}
	object15 struct {
		Object
		buf [15]Value
	}
	object17 struct {
		Object
		buf [coSlotsMax]Value
	}
)

// NewObjectCap creates an ordinary object with room for n named properties
// (an object literal about to be filled by DefineField). An empty literal
// gets room for four properties because it is almost always assigned to
// afterwards.
func (r *Realm) NewObjectCap(n int) *Object {
	var o *Object
	switch n {
	case 1, 2:
		x := &object2{}
		o, x.slots = &x.Object, x.buf[:0]
	case 0, 3, 4:
		x := &object4{}
		o, x.slots = &x.Object, x.buf[:0]
	case 5, 6:
		x := &object6{}
		o, x.slots = &x.Object, x.buf[:0]
	case 7, 8:
		x := &object8{}
		o, x.slots = &x.Object, x.buf[:0]
	case 9:
		x := &object9{}
		o, x.slots = &x.Object, x.buf[:0]
	case 10:
		x := &object10{}
		o, x.slots = &x.Object, x.buf[:0]
	case 11:
		x := &object11{}
		o, x.slots = &x.Object, x.buf[:0]
	case 12:
		x := &object12{}
		o, x.slots = &x.Object, x.buf[:0]
	case 13:
		x := &object13{}
		o, x.slots = &x.Object, x.buf[:0]
	case 14, 15:
		x := &object15{}
		o, x.slots = &x.Object, x.buf[:0]
	case 16, coSlotsMax:
		x := &object17{}
		o, x.slots = &x.Object, x.buf[:0]
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

// createObject is NewObjectWithProto for Object.create, with room for n
// properties, the ones its caller is about to define (Object.create's
// second argument), or for those the objects created from proto before it
// went on to take where they are more: the chain of only children below
// proto's root shape (Shape.trunk), as a constructor's objects have room
// for theirs (constructThis). Past coSlotsMax it gives the chain's room
// only once the objects given coSlotsMax show the chain is their common
// path (Shape.trunkRoom), as growSlots does. Only the root of a prototype
// the program made is proto's alone: the null prototype's,
// Object.prototype's and the shared roots also hold the shapes of the
// intrinsics and of objects that never came from Object.create, such as
// the null-prototype unscopables of Array.prototype, with seventeen
// properties.
func (r *Realm) createObject(proto *Object, n int) *Object {
	root := r.nullProtoRoot
	if proto != nil {
		r.markPrototype(proto)
		root = r.rootShapeFor(proto)
		if root != r.plainRoot && !root.shared {
			if end, grew := root.trunkPast(coSlotsMax); end != root {
				if m := int(end.count); m <= coSlotsMax || root.trunkRoom(grew) {
					n = max(n, m)
				} else {
					n = max(n, coSlotsMax)
					root.tallyGrowth()
				}
			}
		}
	}
	if n > 0 {
		return initObject(r.NewObjectCap(min(n, maxShapeProps)), ClassObject, root)
	}
	return r.newObject(ClassObject, root)
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
	c, ok := o.dictTable().lookup(key)
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
			if len(o.slots) == cap(o.slots) {
				o.growSlots(r)
			}
			o.slots = append(o.slots, cell.value)
			r.bumpEpoch(o)
			return
		}
		if o.shape.owned || o == r.Global || o.keepsShape() {
			o.addOwnedNamed(r, key, cell)
			return
		}
		// The object goes on past maxShapeProps: room for as many again, or
		// for what the last object that went on from its shape took.
		d := r.dictShapeAt(o.shape)
		o.toDictionaryIn(r, d, max(maxShapeProps, int(d.tally)-maxShapeProps))
	}
	d := o.dictTable()
	c := cap(d.entries)
	r.charge(d.add(key, cell))
	if s := o.shape; cap(d.entries) != c && !s.shared {
		s.tally = uint16(min(cap(d.entries), dictRoomMax))
	}
	r.bumpEpoch(o)
}

// growSlots gives o, whose slots are full, room for the property its
// shape, just extended, adds, and for those the objects that took the
// shape before went on to add: the shapes below it down a chain of only
// children (Shape.trunk), and past the chain's end where objects went on
// from there (Shape.room). It gives no less than append, which doubles, so
// an object that goes past where the others stopped grows no more often
// than it would without them, and no more than append's growth or
// coSlotsMax slots, whichever is more, until the objects given that much
// show the trunk is their common path (Shape.trunkRoom). Where objects
// went on from the shape, short of append's growth, it gives the room
// they took: the objects that went on stopped there.
func (o *Object) growSlots(r *Realm) {
	n, l, s := 1, len(o.slots), o.shape
	b := max(l, coSlotsMax)
	end, grew := s.trunkPast(l + b)
	if m := end.room(); m > 2*l {
		if n = m - l; n > b && !s.trunkRoom(grew) {
			n = b
		}
	} else if m > l+1 {
		s.tallyGrowth()
		ns := slices.Grow([]Value(nil), m)[:l]
		copy(ns, o.slots)
		o.slots = ns
		r.charge(cap(ns) * valueSize)
		return
	}
	if n <= b {
		s.tallyGrowth()
	}
	o.slots = slices.Grow(o.slots, n)
	r.charge(cap(o.slots) * valueSize)
}

// trunkSlots returns the slots to give an object that takes s with its n
// properties at once (spreadFresh): room for those the objects that took s
// before went on to add down the trunk below it and past its end
// (Shape.room), within the bound
// growSlots keeps, and n where the trunk adds none.
func (s *Shape) trunkSlots(n int) int {
	b := max(n, coSlotsMax)
	end, grew := s.trunkPast(n + b)
	m := end.room()
	if m-n > b && !s.trunkRoom(grew) {
		return n + b
	}
	return max(m, n)
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
func (o *Object) KeepShape() { o.bits |= bitKeepShape }

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
		o.toDictionary(r, 0) // an owned chain changes only at its end (Shape.addOwned)
	}
	c, _ := o.dictTable().lookup(key)
	if c.attrs != cell.attrs {
		r.bumpEpoch(o)
	}
	*c = cell
}

// Deletions before the end of a global object's properties. A bundle
// deletes a few globals it defined itself (a fetch polyfill's `fetch` and
// `Request`, which `Headers` and `Response` follow): the first
// maxGlobalRebuilds such deletions that leave at most globalRebuildTail
// properties after the key keep the object in shape mode (Shape.without),
// so that its globals keep their inline caches; any other moves it to
// dictionary mode, as it does any object. A rebuild takes time in the tail
// and, in an owned chain, in the number of properties: the bounds keep
// repeated deletions linear, where a rebuild at each made them quadratic
// (deleting 5,000 of 10,000 globals took 8.9 s).
const (
	maxGlobalRebuilds = 8
	globalRebuildTail = maxShapeProps
)

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
		if o == r.Global && r.globalRebuilds() < maxGlobalRebuilds && o.shape.count-1-slot <= globalRebuildTail {
			r.shapeState().globalRebuilds++
			o.shape = o.shape.without(r, slot)
			n := len(o.slots) - 1
			copy(o.slots[slot:], o.slots[slot+1:])
			o.slots[n] = Value{}
			o.slots = o.slots[:n]
			r.bumpEpoch(o)
			return
		}
		o.toDictionary(r, 0)
	}
	if _, n := o.dictTable().remove(key); n != 0 {
		r.charge(n) // a compaction moved the entries to a smaller array
	}
	r.bumpEpoch(o)
}

// toDictionary moves named properties into the dictionary, with room for
// extra more, and points the shape at the realm's sentinel so inline caches
// always miss.
func (o *Object) toDictionary(r *Realm, extra int) {
	o.toDictionaryIn(r, r.dictShapeFor(o.Proto()), extra)
}

// toDictionaryIn is toDictionary to the sentinel to.
func (o *Object) toDictionaryIn(r *Realm, to *Shape, extra int) {
	o.chargeExt(r)
	d := o.ensureDict()
	props := o.shape.Props()
	d.entries = slices.Grow([]dictEntry(nil), len(props)+extra)[:len(props)] // all its size class holds
	for i, p := range props {
		d.entries[i] = dictEntry{key: p.key, cell: propCell{value: o.slots[i], attrs: p.attrs}}
	}
	r.charge(cap(d.entries)*int(unsafe.Sizeof(dictEntry{})) + d.reindex())
	o.shape = to
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

// lookupIndex finds an own indexed property. An index compares with a
// length as int64, here and below: past 2**31 it is no int on 32-bit
// systems.
func (o *Object) lookupIndex(i uint32) (propCell, bool) {
	if int64(i) < int64(o.elemLen()) {
		if v := o.elemsReadOnly()[i]; !v.IsHole() {
			return propCell{value: v, attrs: o.elementAttrs()}, true
		}
	}
	if o.dictTable() != nil && o.dictTable().sparse != nil {
		c, ok := o.dictTable().sparse[i]
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
	d := o.ensureDict()
	if d.sparse == nil {
		d.sparse = make(map[uint32]propCell)
	}
	return d.sparse
}

// addIndex stores an absent indexed property.
func (o *Object) addIndex(r *Realm, i uint32, cell propCell) {
	if o.class == ClassArray && i >= o.aux {
		o.aux = i + 1
	}
	e := o.elems()
	if cell.attrs == o.elementAttrs() && int64(i) < int64(len(e)+denseGrowLimit) && (o.bits&bitHasExt == 0 || (*objectExt)(o.ext).sparse == nil) {
		if o.bits&bitInlineElems == 0 {
			o.chargeExt(r)
		}
		switch {
		case int64(i) < int64(len(e)):
			e[i] = cell.value
		case int(i) == len(e):
			o.setElems(r.appendValue(e, cell.value))
		default:
			e = r.growWithHoles(e, int(i)+1)
			e[i] = cell.value
			o.setElems(e)
		}
		r.bumpEpoch(o)
		return
	}
	o.chargeExt(r)
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
	if int64(i) < int64(o.elemLen()) && !o.elems()[i].IsHole() {
		if cell.attrs == o.elementAttrs() {
			o.elems()[i] = cell.value
			return
		}
		o.elems()[i] = Hole()
		o.sparseMap()[i] = cell
		r.bumpEpoch(o)
		return
	}
	prev := o.dictTable().sparse[i]
	if prev.attrs != cell.attrs {
		r.bumpEpoch(o)
	}
	o.dictTable().sparse[i] = cell
}

// removeIndex deletes a present indexed property.
func (o *Object) removeIndex(r *Realm, i uint32) {
	if int64(i) < int64(o.elemLen()) && !o.elems()[i].IsHole() {
		o.elems()[i] = Hole()
		if int(i) == o.elemLen()-1 {
			o.trimTrailingHoles()
		}
		r.bumpEpoch(o)
		return
	}
	delete(o.dictTable().sparse, i)
	if o.class == ClassArguments {
		o.unmapArgument(i)
	}
	r.bumpEpoch(o)
}

func (o *Object) trimTrailingHoles() {
	e := o.elems()
	n := len(e)
	for n > 0 && e[n-1].IsHole() {
		n--
	}
	clear(e[n:])
	o.setElems(e[:n])
}

// --- own property access ---------------------------------------------------------

// getOwnCell returns the storage form of an own property, synthesizing
// descriptors for exotic properties (array length, string indices).
func (o *Object) getOwnCell(key PropertyKey) (propCell, bool) {
	if key.IsIndex() {
		switch o.class {
		case ClassString:
			s := extDataOf(o, kindString)
			if i := key.Index(); int64(i) < int64(s.Len()) {
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
			attrs := uint8(0)
			if o.lengthWritable() {
				attrs = attrWritable
			}
			return propCell{value: Uint32Value(o.arrayLen()), attrs: attrs}, true
		case ClassString:
			return propCell{value: IntValue(extDataOf(o, kindString).Len()), attrs: 0}, true
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
	for obj := o; obj != nil; obj = obj.Proto() {
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
	for ; ; obj = obj.Proto() {
		c, ok := obj.getOwnCell(key)
		if !ok {
			// A typed array answers a numeric key itself.
			if obj.Proto() == nil || obj.class == ClassTypedArray && typedArrayKey(key) {
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
	for ; ; obj = obj.Proto() {
		c, ok := obj.getOwnCell(key)
		if !ok {
			if obj.Proto() == nil {
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
	if int64(i) < int64(o.elemLen()) && o.class != ClassString {
		if v := o.elemsReadOnly()[i]; !v.IsHole() {
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
	ownMiss := false // the fast path found o has no such named property
	if receiver.IsObject() && receiver.AsObject() == o {
		if o.flags&flagShared != 0 {
			return false, nil
		}
		if key.IsIndex() {
			if i := key.Index(); int64(i) < int64(o.elemLen()) && o.class != ClassString {
				if !o.elems()[i].IsHole() && o.flags&flagFrozen == 0 {
					o.elems()[i] = v
					if o.pkind == payloadKind(kindArgumentsEnv) {
						o.setLazyArgument(i, v)
					}
					return true, nil
				}
			}
			if o.setAbsentIndex(r, key.Index(), v) {
				return true, nil
			}
		} else if o.class != ClassArray || key != lengthKey {
			if o.flags&flagHasLazy == 0 || o.resolveLazyWrite(key) {
				p, attrs, ok := o.lookupNamed(key)
				if ok && attrs&attrAccessor == 0 {
					if attrs&attrWritable == 0 {
						return false, nil
					}
					*p = v
					return true, nil
				}
				ownMiss = !ok && o.flags&flagHasLazy == 0 && (o.class == ClassObject || o.class == ClassArray || o.class == ClassFunction)
			}
		}
	}
	// Generic path: find the first own property along the chain, from o's
	// prototype where the fast path found o has none.
	obj := o
	if ownMiss {
		if obj = o.Proto(); obj == nil {
			return o.createAbsent(r, key, v)
		}
	}
	for ; ; obj = obj.Proto() {
		c, ok := obj.getOwnCell(key)
		if !ok {
			// A typed array answers a numeric key itself. Its valid indices
			// take the found path: receiverSet redefines the element through
			// typedArrayDefine, as TypedArraySetElement would.
			if obj.class == ClassTypedArray && typedArrayKey(key) {
				return r.typedArraySet(obj, key, v, receiver)
			}
			if obj.Proto() == nil {
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
			if o.flags&flagExtensible == 0 || o.class == ClassArray && key.Index() >= o.arrayLen() && !o.lengthWritable() {
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
	n := o.elemLen()
	if int64(i) > int64(n) || int64(i) < int64(n) && !o.elems()[i].IsHole() ||
		o.flags&(flagExtensible|flagHasLazy) != flagExtensible || o.dictTable() != nil && o.dictTable().sparse != nil || !indexFreeProtos(o) {
		return false
	}
	switch o.class {
	case ClassArray:
		if i >= o.arrayLen() {
			if !o.lengthWritable() {
				return false
			}
			o.setArrayLen(i + 1)
		}
	case ClassObject:
		o.chargeExt(r)
	default:
		return false
	}
	if int64(i) < int64(n) {
		o.elems()[i] = v
	} else {
		o.setElems(r.appendValue(o.elems(), v))
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
				if key.Index() >= o.arrayLen() && !o.lengthWritable() {
					return false, nil
				}
			}
		case ClassString:
			if key == lengthKey || (key.IsIndex() && int64(key.Index()) < int64(extDataOf(o, kindString).Len())) {
				cur, _ := o.GetOwnProperty(key)
				return ValidateAndApplyPropertyDescriptor(r, nil, key, o.IsExtensible(), desc, &cur), nil
			}
		case ClassTypedArray:
			if typedArrayKey(key) {
				return r.typedArrayDefine(o, key, desc)
			}
		case ClassArguments:
			if key.IsIndex() && o.pkind != 0 {
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
		_, valid := dataOf(o, kindTypedArray).index(key)
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
	o.makePendingDescs()
	o.addProp(r, key, propCell{value: v, attrs: attrs &^ attrAccessor})
}

// DefineOwnAccessorFast adds an accessor property known to be absent.
func (o *Object) DefineOwnAccessorFast(r *Realm, key PropertyKey, get, set *Object, attrs uint8) {
	o.mustBeMutable()
	o.makePendingDescs()
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
	keys := make([]PropertyKey, 0, o.keyCap())
	keys = o.appendIndexKeys(keys)
	if o.class == ClassArray || o.class == ClassString {
		keys = append(keys, lengthKey)
	}
	keys = o.appendNamedKeys(keys, true)
	keys = o.appendNamedKeys(keys, false)
	return keys
}

// ownStringKeys returns o's own string keys in [[OwnPropertyKeys]] order,
// enumerable or not, but for the length of an array or a String, which is
// never enumerable: the keys EnumerableOwnProperties (Object.entries and
// Object.values) takes before it checks each one's [[Enumerable]] at its
// turn.
func (o *Object) ownStringKeys() []PropertyKey {
	if o.flags&flagHasLazy != 0 {
		o.resolveAllLazy()
	}
	keys := o.appendIndexKeys(make([]PropertyKey, 0, o.keyCap()))
	return o.appendNamedKeys(keys, true)
}

// OwnEnumerableStringKeys returns own enumerable string keys in spec order
// (Object.keys, JSON.stringify, for-in). Indices are returned as index keys.
func (o *Object) OwnEnumerableStringKeys() []PropertyKey {
	if o.flags&flagHasLazy != 0 {
		o.resolveAllLazy()
	}
	keys := o.appendEnumerableIndexKeys(make([]PropertyKey, 0, o.keyCap()))
	if o.flags&flagDict == 0 {
		for _, p := range o.shape.Props() {
			if p.attrs&attrEnumerable != 0 && p.key.IsString() {
				keys = append(keys, p.key)
			}
		}
		return keys
	}
	for i := range o.dictTable().entries {
		e := &o.dictTable().entries[i]
		if e.live() && e.cell.attrs&attrEnumerable != 0 && e.key.IsString() {
			keys = append(keys, e.key)
		}
	}
	return keys
}

// appendEnumerableIndexKeys appends o's enumerable integer keys, in
// ascending order.
func (o *Object) appendEnumerableIndexKeys(keys []PropertyKey) []PropertyKey {
	if o.flags&(flagSealed|flagFrozen) != 0 || (o.dictTable() != nil && o.dictTable().sparse != nil) {
		all := o.appendIndexKeys(nil)
		for _, k := range all {
			if c, ok := o.getOwnCell(k); ok && c.attrs&attrEnumerable != 0 {
				keys = append(keys, k)
			}
		}
		return keys
	}
	return o.appendIndexKeys(keys)
}

// keyCountHint estimates o's own keys, for a capacity. The shape of an
// object in dictionary mode is a sentinel, of no properties.
func (o *Object) keyCountHint() int {
	n := o.elemLen() + int(o.shape.count) + 1
	if o.bits&bitHasExt != 0 {
		x := (*objectExt)(o.ext)
		n += len(x.sparse)
		if o.flags&flagDict != 0 {
			n += x.namedCount()
		}
	}
	return n
}

// keyCap is keyCountHint for a list of o's keys: the elements of an
// object other than an array, which may hold a few integer keys far
// apart, count without their holes.
func (o *Object) keyCap() int {
	n := o.keyCountHint()
	if o.class != ClassArray {
		for _, v := range o.elems() {
			if v.IsHole() {
				n--
			}
		}
	}
	return n
}

func (o *Object) appendIndexKeys(keys []PropertyKey) []PropertyKey {
	if o.class == ClassTypedArray {
		// A typed array's indices are its elements alone (Realm.ownPropertyKeys
		// bounds how many).
		n := max(dataOf(o, kindTypedArray).length(), 0)
		keys = slices.Grow(keys, n)
		for i := range n {
			keys = append(keys, IndexKey(uint32(i)))
		}
		return keys
	}
	if o.class == ClassString {
		n := extDataOf(o, kindString).Len()
		for i := range n {
			keys = append(keys, IndexKey(uint32(i)))
		}
	}
	var sparse []uint32
	if o.dictTable() != nil {
		sparse = o.dictTable().sparseKeys()
	}
	si := 0
	for i, v := range o.elems() {
		if v.IsHole() {
			continue
		}
		for si < len(sparse) && int64(sparse[si]) < int64(i) {
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
	for i := range o.dictTable().entries {
		e := &o.dictTable().entries[i]
		if e.live() && e.key.IsString() == strings && !e.key.IsPrivate() {
			keys = append(keys, e.key)
		}
	}
	return keys
}

// --- prototype and integrity -----------------------------------------------------

// SetPrototypeOf implements OrdinarySetPrototypeOf; the realm's
// Object.prototype is an immutable prototype exotic object.
func (o *Object) SetPrototypeOf(r *Realm, proto *Object) bool {
	if proto == o.Proto() {
		return true
	}
	if !o.IsExtensible() || o.flags&flagShared != 0 || o == r.ObjectPrototype {
		return false
	}
	for p := proto; p != nil; p = p.Proto() {
		if p == o {
			return false
		}
	}
	if o.flags&flagHasLazy != 0 {
		o.resolveAllLazy() // a lazy object keeps a shape no other object has
	}
	if proto != nil {
		r.markPrototype(proto)
	}
	if o.flags&flagDict == 0 && o.shape.owned {
		o.toDictionary(r, 0) // an owned chain changes only at its end (Shape.addOwned)
	}
	if o.flags&flagDict != 0 {
		o.shape = r.dictShapeFor(proto)
	} else {
		o.shape = o.shape.rebase(r, r.rootShapeFor(proto))
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
		o.setLengthReadOnly()
	}
}

func (o *Object) rewriteAttrs(r *Realm, fn func(uint8) uint8) {
	if o.flags&flagDict == 0 && o.shape.owned {
		o.toDictionary(r, 0) // an owned chain changes only at its end (Shape.addOwned)
	}
	if o.flags&flagDict == 0 {
		if o.shape.count != 0 {
			o.shape = o.shape.mapAttrs(r, fn)
		}
	} else {
		for i := range o.dictTable().entries {
			e := &o.dictTable().entries[i]
			if e.live() {
				e.cell.attrs = fn(e.cell.attrs)
			}
		}
	}
	if o.dictTable() != nil {
		for k, c := range o.dictTable().sparse {
			c.attrs = fn(c.attrs)
			o.dictTable().sparse[k] = c
		}
	}
	r.bumpEpoch(o)
}

// IsFrozen implements TestIntegrityLevel(frozen).
func (o *Object) IsFrozen() bool {
	if o.IsExtensible() || o.class == ClassTypedArray && dataOf(o, kindTypedArray).length() > 0 {
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
	if o.IsExtensible() || o.class == ClassTypedArray && dataOf(o, kindTypedArray).length() > 0 {
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
//
// A key registered on a prototype is one an inline cache may have found
// absent there, as may lacksDescFields: registering advances the epoch.
func (o *Object) lazyTable(r *Realm, n int) *lazyProps {
	o.mustBeMutable()
	if o.class == ClassFunction {
		panic("engine: lazy properties on a function")
	}
	r.bumpEpoch(o)
	switch in := o.payload().(type) {
	case *lazyProps:
		if in.install == nil {
			in.grow(n)
			return in
		}
		o.runInstall(in)
	case *Realm:
		r = in
	case *lazyKeys, *pendingCompile, *pendingDescs:
		o.resolveAllLazy()
	default:
		o.materializeHost()
		o.elems() // shared elements get their copy before flagHasLazy means lazyProps
	}
	lp := &lazyProps{r: r}
	lp.grow(n)
	setExtData(o, kindLazyProps, lp)
	o.flags |= flagHasLazy
	return lp
}

// lazyPending reports whether a lookup of key on o, which has flagHasLazy,
// would define properties first.
func (o *Object) lazyPending(key PropertyKey) bool {
	switch lp := o.payload().(type) {
	case *lazyProps:
		return lp.install != nil || lp.find(key) >= 0 || o == lp.r.Global && lp.r.coldGlobalIndex(key) >= 0
	case *lazyKeys:
		return lp.isPending(key)
	case *Realm:
		return lp.coldGlobalIndex(key) >= 0
	case *pendingCompile:
		return key == compileKey
	case *pendingDescs:
		return lp.has(key)
	case *FunctionData:
		return isStaticsKey(key)
	}
	// An array's shared elements have no property pending; a host
	// placeholder, which is never shape-mode, materializes on any lookup.
	return o.bits&bitSharedElems == 0
}

// hasLazy reports whether o has lazy properties or is a host placeholder:
// flagHasLazy for more than shared elements (bitSharedElems).
func (o *Object) hasLazy() bool {
	return o.flags&flagHasLazy != 0 && o.bits&bitSharedElems == 0
}

// lazyOrDict reports whether o's shape may not show its named properties:
// o is in dictionary mode or hasLazy.
func (o *Object) lazyOrDict() bool {
	return o.flags&flagDict != 0 || o.hasLazy()
}

// deferInstall postpones install, which defines the properties of o, to the
// first access to any own property or the key list of o. o must not have an
// internal payload. It is used for the builtin prototypes of mutable realms
// that most programs never touch.
func (o *Object) deferInstall(r *Realm, install func(*Realm, *Object)) {
	o.mustBeMutable()
	if o.class == ClassFunction {
		panic("engine: a deferred install on a function")
	}
	setExtData(o, kindLazyProps, &lazyProps{r: r, install: install})
	o.flags |= flagHasLazy
	if o.flags&flagDict == 0 && !o.shape.shared {
		o.shape.noFill = noFillAll
	}
}

// runInstall runs a deferred installer; o is an ordinary object again when
// it starts.
func (o *Object) runInstall(lp *lazyProps) {
	o.clearPayload()
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
	lp, ok := extDataOk(o, kindLazyProps)
	if !ok {
		switch in := o.payload().(type) {
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
		case *pendingDescs:
			if in.has(key) {
				in.resolve(o)
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
			o.clearPayload()
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
	switch o.payload().(type) {
	case *lazyKeys, *pendingCompile, *pendingDescs, *FunctionData:
		o.resolveAllLazy()
		return true
	}
	return o.resolveLazy(key)
}

func (o *Object) resolveAllLazy() {
	lp, ok := extDataOk(o, kindLazyProps)
	if !ok {
		switch in := o.payload().(type) {
		case *lazyKeys:
			in.resolveAll(o)
		case *Realm:
			in.resolveColdGlobals()
		case *pendingCompile:
			in.define(o)
		case *pendingDescs:
			in.resolve(o)
		case *FunctionData:
			definePendingStatics(o, in)
		default:
			o.materializeHost()
			o.elems() // an array's shared elements get a copy of their own
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

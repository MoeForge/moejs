package engine

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/MoeForge/moejs/bytecode"
)

// Builtin file convention:
//
//   - One file per namespace: engine/builtin_object.go, builtin_array.go,
//     builtin_string.go, builtin_number.go, builtin_boolean.go,
//     builtin_error.go, builtin_function.go, builtin_math.go, builtin_json.go,
//     builtin_regexp.go, builtin_date.go, builtin_global.go.
//   - Each file exposes `func installXxx(r *Realm)` that fills the already
//     created intrinsic objects (r.ArrayPrototype, r.ArrayCtor, ...) through
//     r.installBuiltins / r.installValues / r.installGetters with
//     package-level `[]builtinDef` tables (the tables are immutable and shared
//     by all realms; only the function objects are per realm or per process).
//   - buildIntrinsics calls the install functions after the skeleton exists.
//     It runs once per mutable realm and once per process for shared realms
//     (NewRealmWith(RealmOptions{SharedIntrinsics: true})), so installers
//     must not capture the realm in closures: natives receive the calling
//     realm as their first argument.
//   - Lazy globals (Object.DefineLazyProperty) are only valid for mutable
//     realms; in shared mode everything is materialized in the template
//     (a shared realm's global object defines the coldGlobalKeys bindings
//     on first lookup, builtin_defer.go) but the late groups and the async
//     intrinsics, which are built once per process on first use
//     (buildLateGroup, sharedAsyncIntrinsics).
//   - Constructors that the skeleton creates as stubs (RegExp, Date,
//     Function) are completed by replacing their behaviour with
//     (*FunctionData).SetNative / SetConstructor rather than by creating new
//     objects, so that the global bindings and prototype links stay valid.

// RealmOptions configures NewRealmWith.
type RealmOptions struct {
	// SharedIntrinsics selects the Hardened-JS model: every intrinsic object
	// (prototypes, constructors, namespaces, builtin functions) is built once
	// per process, deeply frozen and shared by all such realms. Per-realm
	// mutable state is only the global object, module environments, inline
	// caches, the dynamic intern cache and the interrupt flag.
	SharedIntrinsics bool

	// TimeZone is the local time zone of Date; nil means time.Local. It can
	// be changed later with Realm.SetTimeZone.
	TimeZone *time.Location

	// MaxDynamicSource is the length in bytes of the longest source text the
	// realm compiles at run time, in the UTF-8 the compiler parses: the
	// string of an eval, the parameters and body of a Function,
	// GeneratorFunction, AsyncFunction or AsyncGeneratorFunction call
	// together, and the source of Realm.EvalScript. Longer text throws a
	// RangeError before it is parsed, as parsing and compiling allocate many
	// times its size; a string of more code units than the limit is refused
	// by that count before it is converted. Zero means
	// DefaultMaxDynamicSource (1 MiB), a negative value no limit. It can be
	// changed later with Realm.SetMaxDynamicSource. Code the host compiles
	// (the compiler package) has no limit.
	MaxDynamicSource int

	// DisableDynamicCode makes the realm refuse to compile any source text at
	// run time: eval of a string, the Function, GeneratorFunction,
	// AsyncFunction and AsyncGeneratorFunction constructors and
	// Realm.EvalScript throw an EvalError where MaxDynamicSource would throw
	// its RangeError, once the arguments are converted to strings. eval of a
	// value that is not a string still returns it. It can be changed later
	// with Realm.SetDynamicCodeDisabled. Code the host compiles is not
	// affected.
	DisableDynamicCode bool

	// MemoryLimit is the number of bytes the realm's JavaScript may retain,
	// as the last measurement of its heap found, plus allocate since that
	// measurement or the last reset (ReleaseCallData, ResetAllocation),
	// before it interrupts itself with a *MemoryLimitError (memlimit.go); 0
	// means no limit. It can be changed later with Realm.SetMemoryLimit.
	MemoryLimit int64

	// Locale is the default locale of Intl and of the toLocaleString
	// methods, a BCP 47 language tag; "" or a tag that is not well-formed
	// means "en-US". It is canonicalized without its Unicode extension and
	// can be changed later with Realm.SetLocale.
	Locale string
}

// Intrinsics holds the intrinsic objects of a realm. A realm points at its
// own (allocated with it) or, in shared mode, at the process-wide template's,
// so the struct is written only while the intrinsics are built.
type Intrinsics struct {
	ObjectPrototype   *Object
	FunctionPrototype *Object
	ArrayPrototype    *Object
	StringPrototype   *Object
	NumberPrototype   *Object
	BooleanPrototype  *Object
	ErrorPrototype    *Object
	RegExpPrototype   *Object
	DatePrototype     *Object
	// BigIntPrototype carries toString and valueOf for bigint primitives;
	// the BigInt late global adds the rest (builtin_bigint.go). The shared
	// template has none: the BigInt group builds it (lateAt(lateBigInt)
	// before use).
	BigIntPrototype *Object
	// ArrayIteratorPrototype is %ArrayIteratorPrototype%, the prototype of
	// the objects returned by Array.prototype.keys/values/entries.
	ArrayIteratorPrototype *Object

	ObjectCtor   *Object
	FunctionCtor *Object
	ArrayCtor    *Object
	StringCtor   *Object
	NumberCtor   *Object
	BooleanCtor  *Object
	RegExpCtor   *Object
	DateCtor     *Object

	// JSON and Math are the namespace objects (filled by their installers).
	JSON *Object
	Math *Object

	errorProtos [numErrorKinds]*Object
	errorCtors  [numErrorKinds]*Object

	// *extIntrinsics holds the remaining intrinsics (Symbol,
	// iterators, keyed collections, ...) and the shared error `stack`
	// accessor behind one pointer, so the Realm keeps its size class
	// (intrinsics_ext.go). Its fields are promoted: r.SymbolPrototype.
	*extIntrinsics
}

// Realm is one isolated JavaScript global environment. It is single-goroutine
// except for Interrupt/ClearInterrupt.
type Realm struct {
	Global *Object
	*Intrinsics

	// Shapes. A realm over the shared intrinsics builds its objects on the
	// process-wide shared tree whose roots are the template's (shape.go:
	// publish), so its plain objects, literals, functions and host objects
	// reuse the shapes (and lookup tables) of every other such realm. The
	// roots of its own prototypes live in the prototypes (localRoot); those
	// of the continuation of the shared tree past its bounds (localize), and
	// of a mutable realm's intrinsics, in rootShapes.
	plainRoot     *Shape // root for objects whose prototype is Object.prototype
	nullProtoRoot *Shape
	arrayRoot     *Shape // created on first array
	funcShape     *Shape // FunctionPrototype root + length + name; created on first function
	rootShapes    map[*Object]*Shape
	protoEpoch    uint32
	// jsonSizeHint is the length of the last JSON.stringify output; the next
	// call pre-sizes its buffer to it (builtin_json.go). It shares the word
	// with protoEpoch: a shared realm must stay within the 576-byte size
	// class (568 bytes plus the malloc header), or every runtime costs 64 B
	// more.
	jsonSizeHint int32

	// Interning: per-realm cache in front of the process-wide table.
	internCacheASCII  map[string]*String
	internCacheUTF16  map[string]*String
	smallIndexStrings *[smallIndexCached]*String

	// Host conversion: the shape cache (bounded: hostShapesMax) and the
	// reusable FromGo scratch (hostconv.go), both created on first use.
	hostShapes map[uint64]*hostShapeEntry
	fromGo     fromGoState

	// lazy holds rarely used state (Math.random's generator, the arguments
	// shape, tagged-template objects, the job queue, the targets WeakRef
	// keeps alive for the current job and the arrays being joined); created
	// on first use so realms that never need it pay one pointer.
	lazy *realmLazy

	// Interrupts and limits.
	interruptFlag atomic.Int32

	// coldGlobals has a bit for each coldGlobalKeys binding a shared
	// realm's global object has yet to define (builtin_defer.go).
	// sharedIntrinsics is true for realms created with SharedIntrinsics.
	// buildingShared is true while the process-wide template is built.
	// internLookups counts interning through the process-wide tables before
	// the realm's caches exist (intern.go). jobsPending is set when a job is
	// queued, an object kept alive for the current job, or the memory limit
	// left the run's end work (endRunSees), and cleared when the end of a
	// job leaves none (jobs.go). They and callDepth share two
	// words with interruptFlag: the Realm is exactly a 288-byte size class.
	coldGlobals      uint32
	sharedIntrinsics bool
	buildingShared   bool
	internLookups    uint8
	jobsPending      bool
	callDepth        int32

	interruptValue atomic.Pointer[interruptPayload]

	// newTarget carries new.target from constructNT to the first op of the
	// constructed function (class.go); nil everywhere else.
	newTarget *Object

	// Stack capture hooks installed by the interpreter (errors.go).
	// stackCaptureInto is the interpreter's capture writing into the error
	// object's inline buffer; a host-installed capture hook clears it.
	stackCapture     func(r *Realm) []StackFrame
	stackCaptureInto func(r *Realm, buf []StackFrame, skip *Object, limit int) []StackFrame
	stackFormat      func(r *Realm, frames []StackFrame) string

	// Inline caches, owned by the interpreter. A function's
	// entries are appended on its first call (icBaseFor); icTrees holds, per
	// compiled tree, the base+1 of each function's entries (zero: unbound).
	ic      []ICEntry
	icTrees map[*funcMeta][]uint32

	// Interpreter state (interp.go): the register stack and the frame chain.
	interp interpState

	// host is the host's Date clock and time zone (SetNow, SetTimeZone in
	// date_time.go) and import hooks (SetImportHooks in import.go); nil
	// while all are the defaults: time.Now, time.Local and no hooks.
	host *realmHost

	// regexps holds the per-realm compiled-pattern cache, the RegExp
	// instance/exec-result shapes and the UTF-8 working copies of UTF-16
	// subjects (builtin_regexp.go). It is created on first use.
	regexps *regexpState

	// boot holds the bootstrap slabs; dropped after the intrinsics are built.
	boot *bootstrapSlabs
}

// bootstrapSlabs back the objects, slots, functions and shapes of the
// intrinsics of a mutable realm while they are built, so the bootstrap costs
// a handful of allocations rather than one per builtin.
type bootstrapSlabs struct {
	objs   []Object
	slots  []Value
	funcs  []nativeFuncObject
	shapes []Shape
	trans  []transition // first transition records of bootstrap shapes
	nsRoot *Shape       // the root the namespace objects start from (installKeys)
}

// Bootstrap sizing: each slab holds at most what building the intrinsics of
// a mutable realm uses (TestBootstrapSlabsFilled). Where the whole demand
// would leave kilobytes of the next page or size class unused (shapes), the
// slab is rounded down to fill the one below and the overflow is allocated
// one by one; either way a slab never carries spare capacity. A mutable
// realm installs the deferred prototypes (Map, Set, Symbol, ...), Math and
// Reflect on first touch (builtin_defer.go), outside the slabs.
const (
	bootstrapObjects     = 33  // 3168 B in the 3200-byte class
	bootstrapSlots       = 365 // 5840 B in the 6144-byte class
	bootstrapFuncs       = 199 // 6 pages of 224-byte objects (5 would leave 17 to allocate)
	bootstrapShapes      = 273 // the 32768-byte class (the intrinsics make 274)
	bootstrapTransitions = 255 // 8160 B in the 8192-byte class
)

// dictShape is the process-wide dictionary-mode sentinel shape; objects in
// dictionary mode point at it so inline caches never match them.
var dictShape = &Shape{isDict: true, key: rootKey, shared: true}

// realmLazy is the rarely used per-realm state behind Realm.lazy.
type realmLazy struct {
	rng            *rand.Rand     // Math.random
	jobs           *jobState      // the job queue and WeakRef's [[KeptAlive]] (jobs.go)
	argumentsShape *Shape         // unmapped arguments objects (arguments.go)
	mappedShape    *Shape         // mapped arguments objects (arguments.go)
	lex            *globalLex     // the global declarative environment (sloppy.go)
	dyn            *dynState      // code compiled from strings (dynamic.go)
	templates      []*Object      // tagged-template objects by site (template.go)
	joins          []*Object      // the arrays being joined, innermost last (builtin_array.go)
	modules        *moduleMap     // the module map (module_eval.go)
	json           *jsonStack     // JSON.parse's reused stack (builtin_json.go)
	statics        *regexpStatics // the RegExp statics' match (regexp_legacy.go)
	mem            *memAccount    // the memory limit's account (memlimit.go)
}

// lazyState returns the realm's rarely used state, creating it on first use.
func (r *Realm) lazyState() *realmLazy {
	if r.lazy == nil {
		r.lazy = &realmLazy{}
	}
	return r.lazy
}

// NewRealm creates a realm with mutable, per-realm intrinsics.
func NewRealm() *Realm { return NewRealmWith(RealmOptions{}) }

// NewRealmWith creates a realm according to opts.
func NewRealmWith(opts RealmOptions) *Realm {
	var r *Realm
	if opts.SharedIntrinsics {
		r = newSharedRealm()
	} else {
		r = newBareRealm()
		r.buildIntrinsics()
		r.boot = nil
	}
	r.SetTimeZone(opts.TimeZone)
	r.SetMaxDynamicSource(opts.MaxDynamicSource)
	r.SetDynamicCodeDisabled(opts.DisableDynamicCode)
	r.SetMemoryLimit(opts.MemoryLimit)
	r.SetLocale(opts.Locale)
	return r
}

// HasSharedIntrinsics reports whether the realm uses the shared frozen
// intrinsics.
func (r *Realm) HasSharedIntrinsics() bool { return r.sharedIntrinsics }

// ownRealm co-allocates a mutable realm with its intrinsics.
type ownRealm struct {
	realm Realm
	intr  Intrinsics
}

func newBareRealm() *Realm {
	own := &ownRealm{}
	r := &own.realm
	*r = Realm{
		Intrinsics: &own.intr,
		boot: &bootstrapSlabs{
			objs:   make([]Object, 0, bootstrapObjects),
			slots:  make([]Value, 0, bootstrapSlots),
			funcs:  make([]nativeFuncObject, 0, bootstrapFuncs),
			shapes: make([]Shape, 0, bootstrapShapes),
			trans:  make([]transition, 0, bootstrapTransitions),
		},
	}
	r.nullProtoRoot = newRootShape(nil)
	r.stackCapture, r.stackCaptureInto, r.stackFormat = defaultStackCapture, defaultStackCaptureInto, defaultStackFormat
	return r
}

// buildIntrinsics creates the intrinsic skeleton in r.
func (r *Realm) buildIntrinsics() {
	r.extIntrinsics = &extIntrinsics{}
	r.ObjectPrototype = r.newObject(ClassObject, r.nullProtoRoot)
	r.markPrototype(r.ObjectPrototype)
	r.plainRoot = newRootShape(r.ObjectPrototype)

	r.initFunctionPrototype()
	r.initPrototypes()
	r.initConstructors()
	r.initGlobal()
	installIntrinsics(r)
}

// --- shared intrinsics -------------------------------------------------------------

// sharedTemplate is the process-wide frozen intrinsic set.
type sharedTemplate struct {
	*Intrinsics
	globalShape    *Shape
	globalSlots    []Value
	globalThisSlot uint32
	// The shared roots of plain, null-prototype and array objects and the
	// shared shape of a fresh function.
	plainRoot, nullRoot, arrayRoot, funcShape *Shape
	// globalSlotsCap is the slot capacity of a shared realm's global object:
	// sharedGlobalHeadroom free slots for host globals, rounded up to fill
	// the allocation size class.
	globalSlotsCap int
	// dataDescShape and accessorDescShape are the descriptor object shapes
	// of Object.getOwnPropertyDescriptor (builtin_object.go), and
	// iterResultShape that of an iterator result (builtin_iterator.go).
	dataDescShape, accessorDescShape, iterResultShape *Shape
	// coldCells are the coldGlobalKeys bindings but the late ones
	// (lateCells), which globalShape and globalSlots leave out.
	coldCells [len(coldGlobalKeys)]propCell
}

var (
	sharedOnce sync.Once
	sharedTpl  *sharedTemplate
)

// sharedGlobalHeadroom is the minimum number of free global slots a shared
// realm starts with (new-api's host sets two globals: utils and console).
// append rounds the capacity up to the size class, so builtin globals added
// later cost bytes only when they cross one.
const sharedGlobalHeadroom = 4

func buildSharedTemplate() {
	tpl := newBareRealm()
	tpl.buildingShared = true
	tpl.buildIntrinsics()
	tpl.freezeIntrinsics()
	// The template's roots for the (now shared) intrinsic prototypes become
	// the roots of the shared tree.
	roots := make(map[*Object]*Shape, len(tpl.rootShapes)+1)
	roots[tpl.ObjectPrototype] = tpl.plainRoot
	for proto, root := range tpl.rootShapes {
		if proto.flags&flagShared != 0 {
			roots[proto] = root
		}
	}
	for _, root := range roots {
		root.prepareShared()
	}
	tpl.nullProtoRoot.prepareShared()
	sharedRoots.Store(&roots)
	sharedIntr.Store(tpl.Intrinsics)
	funcShape := tpl.functionShape()
	funcShape.prepareShared()
	slot, _, _ := tpl.Global.shape.Lookup(StringKey(AtomGlobalThis))
	hot, cold := tpl.coldGlobalCells()
	dataDesc, accessorDesc := descriptorShapeFrom(tpl, tpl.plainRoot, false), descriptorShapeFrom(tpl, tpl.plainRoot, true)
	dataDesc.prepareShared()
	accessorDesc.prepareShared()
	iterResult := iterResultShapeFrom(tpl)
	iterResult.prepareShared()
	sharedTpl = &sharedTemplate{
		Intrinsics:        tpl.Intrinsics,
		globalShape:       hot,
		globalSlots:       tpl.Global.slots[:hot.count],
		globalThisSlot:    slot,
		plainRoot:         tpl.plainRoot,
		nullRoot:          tpl.nullProtoRoot,
		arrayRoot:         tpl.arrayShape(),
		funcShape:         funcShape,
		globalSlotsCap:    cap(append([]Value(nil), make([]Value, hot.count+sharedGlobalHeadroom)...)),
		dataDescShape:     dataDesc,
		accessorDescShape: accessorDesc,
		iterResultShape:   iterResult,
		coldCells:         cold,
	}
}

// newSharedRealm creates a light realm over the shared template: the realm
// struct and the global object.
func newSharedRealm() *Realm {
	sharedOnce.Do(buildSharedTemplate)
	tpl := sharedTpl
	r := &Realm{Intrinsics: tpl.Intrinsics, sharedIntrinsics: true}
	r.stackCapture, r.stackCaptureInto, r.stackFormat = defaultStackCapture, defaultStackCaptureInto, defaultStackFormat
	r.nullProtoRoot, r.plainRoot, r.arrayRoot, r.funcShape = tpl.nullRoot, tpl.plainRoot, tpl.arrayRoot, tpl.funcShape
	g := &Object{shape: tpl.globalShape, proto: r.ObjectPrototype, class: ClassObject, flags: flagExtensible | flagHasLazy, internal: r}
	r.coldGlobals = 1<<len(coldGlobalKeys) - 1
	g.slots = make([]Value, len(tpl.globalSlots), tpl.globalSlotsCap)
	copy(g.slots, tpl.globalSlots)
	g.slots[tpl.globalThisSlot] = ObjectValue(g)
	r.Global = g
	return r
}

// freezeIntrinsics deep-freezes every object reachable from the intrinsics
// and marks it shared, then prepares their shapes for lock-free reads. The
// walk stops at the objects already shared: a late group (buildLateGroup)
// freezes only what it created.
func (r *Realm) freezeIntrinsics() {
	seen := make(map[*Object]bool, 128)
	var visit func(o *Object)
	visit = func(o *Object) {
		if o == nil || seen[o] || o.flags&flagShared != 0 {
			return
		}
		seen[o] = true
		visit(o.proto)
		for _, k := range o.OwnPropertyKeys() {
			c, _ := o.getOwnCell(k)
			if c.attrs&attrAccessor != 0 {
				a := c.value.asAccessor()
				visit(a.Get)
				visit(a.Set)
				continue
			}
			switch {
			case c.value.IsObject():
				visit(c.value.AsObject())
			case c.value.IsString():
				prepareSharedString(c.value.AsString())
			}
		}
		r.shareObject(o)
	}
	r.visitIntrinsicRoots(visit)
	// The global object stays per realm; only its shape and slot values are
	// shared, and every value must already be shared.
	for _, k := range r.Global.OwnPropertyKeys() {
		if c, _ := r.Global.getOwnCell(k); c.value.IsObject() && c.value.AsObject() != r.Global {
			visit(c.value.AsObject())
		}
	}
	r.Global.shape.prepareShared()
}

// visitIntrinsicRoots feeds every root of the intrinsics to visit, nil for
// one a mutable realm has yet to define: to the freeze walk of the shared
// template (freezeIntrinsics) and to the live measurement of a mutable
// realm (memlive.go).
func (r *Realm) visitIntrinsicRoots(visit func(*Object)) {
	for _, o := range [...]*Object{
		r.ObjectPrototype, r.FunctionPrototype, r.ArrayPrototype, r.StringPrototype, r.NumberPrototype,
		r.BooleanPrototype, r.ErrorPrototype, r.RegExpPrototype, r.DatePrototype, r.BigIntPrototype, r.ArrayIteratorPrototype,
		r.ObjectCtor, r.FunctionCtor, r.ArrayCtor, r.StringCtor, r.NumberCtor, r.BooleanCtor,
		r.RegExpCtor, r.DateCtor, r.JSON, r.Math,
	} {
		visit(o)
	}
	for k := range numErrorKinds {
		visit(r.errorProtos[k])
		visit(r.errorCtors[k])
	}
	r.visitExtIntrinsics(visit)
}

// shareObject freezes o (Object.freeze semantics), detaches it from the
// template realm and marks it shared.
func (r *Realm) shareObject(o *Object) {
	o.Freeze(r)
	if fd := o.FunctionData(); fd != nil {
		fd.realm = nil
		if fd.name != nil {
			prepareSharedString(fd.name)
		}
	}
	if s, ok := o.internal.(*String); ok {
		prepareSharedString(s)
	}
	o.flags |= flagShared | flagIsPrototype
	o.shape.prepareShared()
}

// prepareSharedString flattens and hashes a string so later reads never write.
func prepareSharedString(s *String) {
	s.flatten()
	s.Hash()
}

// --- bootstrap helpers ----------------------------------------------------------------

// initFunctionPrototype creates Function.prototype (itself callable) and the
// shared function shape.
func (r *Realm) initFunctionPrototype() {
	lengthK, nameK := StringKey(AtomLength), StringKey(AtomName)
	fo := &funcObject{}
	fp := &fo.obj
	fp.class = ClassFunction
	fp.flags = flagExtensible | flagIsPrototype
	fp.proto = r.ObjectPrototype
	fp.shape = r.plainRoot.addProperty(r, lengthK, attrConfigurable).addProperty(r, nameK, attrConfigurable)
	fp.slots = r.allocSlotsCap(4)
	fp.slots = append(fp.slots, IntValue(0), StringValue(AtomEmpty))
	fp.internal = &fo.fd
	fo.fd.kind = FuncNative
	fo.fd.realm = r
	fo.fd.name = AtomEmpty
	fo.fd.native = func(*Realm, Value, []Value) (Value, error) { return Undefined(), nil }
	r.FunctionPrototype = fp
	r.funcShape = r.rootShapeFor(fp).addProperty(r, lengthK, attrConfigurable).addProperty(r, nameK, attrConfigurable)
}

// functionShape returns the shape of a fresh function object.
func (r *Realm) functionShape() *Shape {
	if r.funcShape == nil {
		r.funcShape = r.rootShapeFor(r.FunctionPrototype).
			addProperty(r, StringKey(AtomLength), attrConfigurable).
			addProperty(r, StringKey(AtomName), attrConfigurable)
	}
	return r.funcShape
}

// arrayShape returns the root shape of arrays.
func (r *Realm) arrayShape() *Shape {
	if r.arrayRoot == nil {
		r.arrayRoot = r.rootShapeFor(r.ArrayPrototype)
	}
	return r.arrayRoot
}

// newIntrinsic allocates a prototype/namespace object with room for nprops.
func (r *Realm) newIntrinsic(class Class, proto *Object, nprops int) *Object {
	r.markPrototype(proto)
	o := r.newObject(class, r.rootShapeFor(proto))
	o.flags |= flagIsPrototype
	o.slots = r.allocSlotsCap(nprops)
	return o
}

func (r *Realm) initPrototypes() {
	op := r.ObjectPrototype
	op.slots = r.allocSlotsCap(8)

	r.ArrayPrototype = r.newIntrinsic(ClassArray, op, 4)
	r.ArrayPrototype.internal = &ArrayData{lengthWritable: true}
	r.arrayRoot = r.rootShapeFor(r.ArrayPrototype)

	r.StringPrototype = r.newIntrinsic(ClassString, op, 4)
	r.StringPrototype.internal = emptyString
	r.NumberPrototype = r.newIntrinsic(ClassNumber, op, 4)
	r.NumberPrototype.internal = &primitiveWrapper{value: IntValue(0)}
	r.BooleanPrototype = r.newIntrinsic(ClassBoolean, op, 4)
	r.BooleanPrototype.internal = &primitiveWrapper{value: False()}

	r.ErrorPrototype = r.newIntrinsic(ClassObject, op, 4)
	r.errorProtos[KindError] = r.ErrorPrototype
	for k := KindTypeError; k < numErrorKinds; k++ {
		r.errorProtos[k] = r.newIntrinsic(ClassObject, r.ErrorPrototype, 3)
	}
	r.RegExpPrototype = r.newIntrinsic(ClassObject, op, 2)
	r.DatePrototype = r.newIntrinsic(ClassObject, op, 2)
	if !r.buildingShared {
		// The shared template builds it with the BigInt group
		// (installBigIntGlobal): bigints need the global's group anyway.
		r.BigIntPrototype = r.newIntrinsic(ClassObject, op, 3) // 2 methods and @@toStringTag
	}

	r.JSON = r.newIntrinsic(ClassObject, op, 3) // 2 methods and @@toStringTag
	r.JSON.flags &^= flagIsPrototype
	r.Math = r.newIntrinsic(ClassObject, op, 2)
	r.Math.flags &^= flagIsPrototype
}

func (r *Realm) initConstructors() {
	r.ObjectCtor = r.newConstructor(AtomObject, 1, objectCall, objectConstruct, r.ObjectPrototype)
	r.FunctionCtor = r.newConstructor(AtomFunction, 1, functionCall, functionConstruct, r.FunctionPrototype)
	r.ArrayCtor = r.newConstructor(AtomArray, 1, arrayCall, arrayConstruct, r.ArrayPrototype)
	r.StringCtor = r.newConstructor(AtomString, 1, stringCall, stringConstruct, r.StringPrototype)
	r.NumberCtor = r.newConstructor(AtomNumber, 1, numberCall, numberConstruct, r.NumberPrototype)
	r.BooleanCtor = r.newConstructor(AtomBoolean, 1, booleanCall, booleanConstruct, r.BooleanPrototype)
	r.RegExpCtor = r.newConstructor(AtomRegExp, 2, notAvailableCall("RegExp"), notAvailableConstruct("RegExp"), r.RegExpPrototype)
	r.DateCtor = r.newConstructor(AtomDate, 7, notAvailableCall("Date"), notAvailableConstruct("Date"), r.DatePrototype)
	for k := KindError; k < numErrorKinds; k++ {
		kind := k
		call := func(r *Realm, this Value, args []Value) (Value, error) {
			return errorConstruct(r, kind, args, nil)
		}
		ctor := func(r *Realm, args []Value, newTarget *Object) (Value, error) {
			return errorConstruct(r, kind, args, newTarget)
		}
		r.errorCtors[k] = r.newConstructor(kind.Name(), 1, call, ctor, r.errorProtos[k])
	}
	for k := KindTypeError; k < numErrorKinds; k++ {
		r.errorCtors[k].proto = r.errorCtors[KindError]
		r.errorCtors[k].shape = r.errorCtors[k].shape.rebase(r, r.rootShapeFor(r.errorCtors[KindError]))
	}
}

// newConstructor creates a builtin constructor linked to its prototype.
func (r *Realm) newConstructor(name *String, length int, call NativeFunc, ctor NativeCtor, proto *Object) *Object {
	o, fd := r.newFunctionObjectCap(name, length, FuncNative, 3)
	fd.native = call
	fd.setCtor(ctor)
	o.DefineOwnDataFast(r, StringKey(AtomPrototype), ObjectValue(proto), 0)
	proto.DefineOwnDataFast(r, StringKey(AtomConstructor), ObjectValue(o), attrHidden)
	return o
}

func (r *Realm) initGlobal() {
	g := r.newObject(ClassObject, r.plainRoot)
	g.slots = r.allocSlotsCap(48)
	r.Global = g
	// Shared realms bind the intrinsics read-only (lockdown model); mutable
	// realms keep the spec's writable, configurable bindings.
	bindAttrs := attrHidden
	if r.buildingShared {
		bindAttrs = attrFrozen
	}
	def := func(name *String, v Value, attrs uint8) {
		g.DefineOwnDataFast(r, StringKey(name), v, attrs)
	}
	def(AtomGlobalThis, ObjectValue(g), bindAttrs)
	def(AtomUndefined, Undefined(), 0)
	def(AtomNaN, NaN(), 0)
	def(AtomInfinity, NumberValue(posInf), 0)
	for _, c := range [...]*Object{
		r.ObjectCtor, r.FunctionCtor, r.ArrayCtor, r.StringCtor, r.NumberCtor, r.BooleanCtor,
	} {
		def(c.FunctionData().name, ObjectValue(c), bindAttrs)
	}
	for k := KindError; k < numErrorKinds; k++ {
		def(k.Name(), ObjectValue(r.errorCtors[k]), bindAttrs)
	}
	def(AtomRegExp, ObjectValue(r.RegExpCtor), bindAttrs)
	def(AtomDate, ObjectValue(r.DateCtor), bindAttrs)
	def(AtomJSON, ObjectValue(r.JSON), bindAttrs)
	def(AtomMath, ObjectValue(r.Math), bindAttrs)
}

// allocSlotsCap returns an empty slot slice with capacity n.
func (r *Realm) allocSlotsCap(n int) []Value {
	if b := r.boot; b != nil && len(b.slots)+n <= cap(b.slots) {
		start := len(b.slots)
		b.slots = b.slots[:start+n]
		return b.slots[start : start : start+n]
	}
	return make([]Value, 0, n)
}

// ReserveSlots ensures capacity for n more named properties without
// reallocation (used before bulk installs).
func (o *Object) ReserveSlots(r *Realm, n int) {
	o.mustBeMutable()
	if o.flags&flagDict != 0 || cap(o.slots)-len(o.slots) >= n {
		return
	}
	ns := r.allocSlotsCap(len(o.slots) + n)
	r.charge(cap(ns) * valueSize)
	ns = append(ns, o.slots...)
	o.slots = ns
}

// newFunctionObjectCap is newFunctionObject with extra slot capacity.
func (r *Realm) newFunctionObjectCap(name *String, length int, kind FuncKind, capacity int) (*Object, *FunctionData) {
	fo := &funcObject{}
	r.chargeObject(unsafe.Sizeof(funcObject{}))
	o := &fo.obj
	o.shape = r.functionShape()
	o.proto = r.FunctionPrototype
	o.class = ClassFunction
	o.flags = flagExtensible
	o.internal = &fo.fd
	fo.fd.kind = kind
	fo.fd.realm = r
	fo.fd.name = name
	if capacity <= len(fo.slots) {
		o.slots = fo.slots[:0]
	} else {
		o.slots = r.allocSlotsCap(capacity)
		r.charge(capacity * valueSize)
	}
	o.slots = append(o.slots, IntValue(length), StringValue(name))
	return o, &fo.fd
}

// --- table-driven installer ---------------------------------------------------------

// builtinDef describes one builtin method: name atom, implementation and the
// value of its `length` property.
type builtinDef struct {
	name   *String
	fn     NativeFunc
	length int
}

// valueDef describes one builtin data constant (Math.PI, Number.MAX_VALUE).
type valueDef struct {
	name  *String
	value Value
	attrs uint8
}

// getterDef describes one builtin accessor with a getter only. fnName is
// the getter's name, "get " + name, as a static atom so that installing it
// builds no string: builtin tables are package-level and use newGetterDef
// (nil builds the name at install time).
type getterDef struct {
	name, fnName *String
	get          NativeFunc
}

// newGetterDef makes a getterDef during package initialization (it
// registers the static atom "get " + name).
func newGetterDef(name *String, get NativeFunc) getterDef {
	return getterDef{name, staticAtom("get " + name.GoString()), get}
}

// installBuiltins defines each method on target as a non-enumerable,
// writable, configurable data property, replacing an existing property with
// the same name.
func (r *Realm) installBuiltins(target *Object, defs []builtinDef) {
	r.installBuiltinsAttrs(target, defs, attrHidden)
}

// installBuiltinsAttrs is installBuiltins with the given property
// attributes.
func (r *Realm) installBuiltinsAttrs(target *Object, defs []builtinDef, attrs uint8) {
	target.ReserveSlots(r, len(defs))
	for _, d := range defs {
		fn := r.NewNativeFunction(d.name, d.length, d.fn)
		r.installOrReplace(target, StringKey(d.name), propCell{value: ObjectValue(fn), attrs: attrs})
	}
}

// installValues defines data constants.
func (r *Realm) installValues(target *Object, defs []valueDef) {
	target.ReserveSlots(r, len(defs))
	for _, d := range defs {
		r.installOrReplace(target, StringKey(d.name), propCell{value: d.value, attrs: d.attrs})
	}
}

// installGetters defines getter-only accessors (configurable, non-enumerable).
func (r *Realm) installGetters(target *Object, defs []getterDef) {
	r.installGettersAttrs(target, defs, attrConfigurable)
}

// installGettersAttrs is installGetters with the given property attributes.
func (r *Realm) installGettersAttrs(target *Object, defs []getterDef, attrs uint8) {
	target.ReserveSlots(r, len(defs))
	accs := make([]Accessor, len(defs)) // one allocation for the table
	for i, d := range defs {
		fnName := d.fnName
		if fnName == nil {
			var sb StringBuilder
			sb.WriteGoString("get ")
			sb.WriteString(d.name)
			fnName = sb.String()
		}
		accs[i].Get = r.NewNativeFunction(fnName, 0, d.get)
		r.installOrReplace(target, StringKey(d.name), propCell{
			value: accessorValue(&accs[i]),
			attrs: attrs | attrAccessor,
		})
	}
}

func (r *Realm) installOrReplace(target *Object, key PropertyKey, cell propCell) {
	if target.flags&flagDict == 0 {
		// Walk the chain instead of Shape.Lookup: a lookup on a shape with
		// more than shapeTableThreshold properties builds that shape's hash
		// table, and doing so for every intermediate bootstrap shape of a
		// 30-method prototype is quadratic in allocations.
		for n := target.shape; n.count != 0; n = n.parent {
			if n.key == key {
				target.replaceNamed(r, key, cell)
				return
			}
		}
		target.addNamed(r, key, cell)
		return
	}
	if _, ok := target.dict.lookup(key); ok {
		target.replaceNamed(r, key, cell)
		return
	}
	target.addNamed(r, key, cell)
}

// SetNative replaces the [[Call]] behaviour of a native function (used by
// the installers to complete stub constructors before the realm is shared).
func (fd *FunctionData) SetNative(fn NativeFunc) { fd.native = fn }

// SetConstructor replaces the [[Construct]] behaviour of a native function.
func (fd *FunctionData) SetConstructor(ctor NativeCtor) { fd.setCtor(ctor) }

// --- inline cache storage -------------------------------------------------------------

// ICSlots returns the realm's inline-cache table (owned by the interpreter).
func (r *Realm) ICSlots() []ICEntry { return r.ic }

// AllocIC reserves count IC entries for fn and returns the base index; a
// second call for the same template returns the same base. A function of
// fn's tree that fails bytecode.VerifyRegisters is an error.
func (r *Realm) AllocIC(fn *bytecode.Function, count uint32) (uint32, error) {
	m, err := r.metaFor(fn)
	if err != nil {
		return 0, err
	}
	return r.icBaseFor(m, count), nil
}

// icBaseFor returns the base of the count IC entries of the template of m,
// appending them on the template's first use in the realm.
func (r *Realm) icBaseFor(m *funcMeta, count uint32) uint32 {
	bases := r.icTrees[m.root]
	if bases == nil {
		if r.icTrees == nil {
			r.icTrees = make(map[*funcMeta][]uint32, 1)
		}
		bases = make([]uint32, m.root.nfuncs)
		r.icTrees[m.root] = bases
	}
	b := bases[m.index] // base+1; zero is unbound
	if b == 0 {
		n := len(r.ic)
		r.growIC(n + int(count))
		r.ic = r.ic[:n+int(count)]
		b = uint32(n) + 1
		bases[m.index] = b
	}
	return b - 1
}

// icExactTrees bounds the trees growIC sums the sites of.
const icExactTrees = 64

// growIC makes room for need entries. The table doubles but stops at the
// sites of every tree bound so far and the entries dynamic code owns
// (allocDynIC), so a realm that ends up running all of its functions holds
// one entry per site, as an eager table would. A realm that has bound more
// than icExactTrees trees only doubles: summing their sites on every growth
// would cost each new tree time in proportion to the trees before it, and
// doubling keeps the copies amortized.
func (r *Realm) growIC(need int) {
	if need <= cap(r.ic) {
		return
	}
	n := 2 * cap(r.ic)
	if len(r.icTrees) <= icExactTrees {
		limit := 0
		if r.lazy != nil && r.lazy.dyn != nil {
			limit = r.lazy.dyn.size
		}
		for root := range r.icTrees {
			limit += int(root.totalICs)
		}
		n = min(n, limit)
	}
	r.resizeIC(max(n, need))
}

// resizeIC reallocates the table with capacity n.
func (r *Realm) resizeIC(n int) {
	ic := make([]ICEntry, len(r.ic), n)
	copy(ic, r.ic)
	r.ic = ic
}

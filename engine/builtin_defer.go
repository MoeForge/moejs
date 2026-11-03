package engine

import (
	"sync"
	"sync/atomic"
)

// The builtin objects most programs never look at (the Map, Set, WeakMap,
// WeakSet, WeakRef and Symbol prototypes, %RegExpStringIteratorPrototype%
// and the Array.prototype[@@unscopables] object) are filled by
// installDeferred: at once while building the shared template, on first
// touch in a mutable realm (deferInstall), the way Date.prototype is
// (builtin_date.go). A mutable realm then pays one lazyProps per object at
// creation instead of its methods, shapes and slots; a program that uses
// one pays the same as before on first touch. The Math and Reflect
// namespaces go further (installKeys): a mutable realm defines each of
// their properties on the first lookup of its key.

// installDeferred runs install, which defines the properties of the builtin
// object o, now while building the shared template and when a program first
// looks at o in a mutable realm. o was created with deferredCap slots;
// install reserves the rest.
func (r *Realm) installDeferred(o *Object, install func(*Realm, *Object)) {
	if r.buildingShared {
		install(r, o)
		return
	}
	o.deferInstall(r, install)
}

// deferredCap is the slot capacity to create an object installDeferred
// fills with: all n properties in the shared template, only the eager ones
// (a prototype's constructor) in a mutable realm.
func (r *Realm) deferredCap(n, eager int) int {
	if r.buildingShared {
		return n
	}
	return eager
}

// coldGlobalKeys are the global bindings few programs name (Symbol, the
// collections, AggregateError and the late bindings), which their
// installers define last. A shared realm's global object starts with the
// template's shape before them (buildSharedTemplate), so its slots keep the
// size class they had without them, and defines each on the first lookup of
// its key (resolveLazy); Realm.coldGlobals has a bit for each binding still
// to define and the global object's internal is the realm while any is, or
// the table of the host's lazy properties (lazyProps) once the host has
// defined some: they leave the cold bindings pending.
//
// The late bindings (lateGlobal) come after the others. A mutable realm
// does not install them at creation either: its global object defines them
// the same way, by running their installer on first use. The shared
// template leaves them out too: each late group is built once per process,
// on the first lookup of one of its keys in any shared realm
// (buildLateGroup).
var coldGlobalKeys = [...]PropertyKey{
	StringKey(AtomSymbol), StringKey(AtomMapName), StringKey(AtomSetName), StringKey(AtomWeakMap),
	StringKey(AtomWeakSet), StringKey(AtomWeakRef), StringKey(AtomAggregateError),
	// Late bindings.
	StringKey(AtomStructuredClone), StringKey(AtomBigInt), StringKey(AtomPromise),
	StringKey(AtomQueueMicrotask), StringKey(AtomProxy),
	StringKey(AtomArrayBuffer), StringKey(AtomSharedArrayBuffer), StringKey(AtomDataView),
	StringKey(AtomInt8Array), StringKey(AtomUint8Array), StringKey(AtomUint8ClampedArray),
	StringKey(AtomInt16Array), StringKey(AtomUint16Array), StringKey(AtomInt32Array),
	StringKey(AtomUint32Array), StringKey(AtomFloat16Array), StringKey(AtomFloat32Array),
	StringKey(AtomFloat64Array), StringKey(AtomBigInt64Array), StringKey(AtomBigUint64Array),
	StringKey(AtomAtomics), StringKey(AtomTextEncoder), StringKey(AtomTextDecoder),
	StringKey(AtomEval), StringKey(AtomIntl),
}

// coldGlobalSlot maps the atom id of each coldGlobalKeys key, a static
// atom, to its index, and every other id up to the highest of them to -1:
// coldGlobalIndex looks a key up with one load, and the globals a host
// defines on a new runtime, dynamic atoms above every static one, fail its
// bounds check. It is read-only after package initialization.
var coldGlobalSlot = func() []int8 {
	var n uint32
	for _, k := range coldGlobalKeys {
		if !isStaticAtom(k.String()) {
			panic("engine: a coldGlobalKeys key must be a static atom")
		}
		n = max(n, k.String().atom)
	}
	t := make([]int8, n+1)
	for i := range t {
		t[i] = -1
	}
	for i, k := range coldGlobalKeys {
		t[k.String().atom] = int8(i)
	}
	return t
}()

// coldGlobalLate holds the installers of the late bindings by
// coldGlobalKeys index (lateGlobal).
var coldGlobalLate [len(coldGlobalKeys)]func(*Realm)

// lateGlobal makes key, a late coldGlobalKeys binding, one that install
// defines on first use: in a mutable realm, and once per process for the
// shared realms (buildLateGroup). install binds key, and the other late
// keys it installs (its group), with bindGlobal, in coldGlobalKeys order. It
// creates every object it writes to: the template's objects are frozen by
// the time a group is built. Installers register from init: they reach
// coldGlobalKeys themselves, so a table literal would be an initialization
// cycle.
func lateGlobal(key PropertyKey, install func(*Realm)) {
	coldGlobalLate[lateIndex(key)] = install
}

// lateIndex returns the coldGlobalKeys index of key, a late binding.
func lateIndex(key PropertyKey) int {
	for i, k := range coldGlobalKeys {
		if k == key && i >= firstLateGlobal {
			return i
		}
	}
	panic("engine: lateGlobal key is not a late coldGlobalKeys binding")
}

// firstLateGlobal is the coldGlobalKeys index of the first late binding
// (lateIndex and coldGlobalCells check it).
const firstLateGlobal = 7

// The late bindings whose intrinsics code needs without the program naming
// them (lateAt).
var (
	lateBigInt      = lateIndex(StringKey(AtomBigInt))
	latePromise     = lateIndex(StringKey(AtomPromise))
	lateArrayBuffer = lateIndex(StringKey(AtomArrayBuffer))
	lateEval        = lateIndex(StringKey(AtomEval))
)

// installLateGlobals leaves the late bindings pending in a mutable realm.
// The shared template has none of them (buildLateGroup).
func installLateGlobals(r *Realm) {
	if r.buildingShared {
		return
	}
	for i, install := range coldGlobalLate {
		if install != nil {
			r.coldGlobals |= 1 << i
		}
	}
	r.Global.flags |= flagHasLazy
	r.Global.internal = r
}

// ensureLate defines the late binding key if this realm has yet to, which
// installs its group: code that needs a late intrinsic whether or not the
// program names its global (an async function needs %Promise%) calls it
// first. A shared realm may hold the intrinsics of a group it has not
// defined (defineColdGlobal), so test the binding (ensureLate, lateAt), not
// the intrinsic, for the realm to behave the same whatever other realms of
// the process did.
func (r *Realm) ensureLate(key PropertyKey) {
	if r.coldGlobals != 0 {
		r.resolveColdGlobal(key)
	}
}

// lateAt is ensureLate for the late binding at coldGlobalKeys index i
// (lateIndex), without the key scan.
func (r *Realm) lateAt(i int) {
	if r.coldGlobals&(1<<i) != 0 {
		r.defineColdGlobal(i)
	}
}

// coldGlobalCells returns the shape of the template's global object before
// the coldGlobalKeys bindings and the bindings but the late ones.
func (r *Realm) coldGlobalCells() (*Shape, [len(coldGlobalKeys)]propCell) {
	var cells [len(coldGlobalKeys)]propCell
	s := r.Global.shape
	for i := firstLateGlobal - 1; i >= 0; i-- {
		if s.key != coldGlobalKeys[i] {
			panic("engine: coldGlobalKeys must be the last global bindings, in order")
		}
		cells[i] = propCell{value: r.Global.slots[s.slot], attrs: s.attrs}
		s = s.parent
	}
	s.prepareShared()
	return s, cells
}

// coldGlobalIndex returns the index of key in coldGlobalKeys if the global
// object has yet to define it, -1 otherwise.
func (r *Realm) coldGlobalIndex(key PropertyKey) int {
	if !key.IsString() {
		return -1
	}
	if a := int(key.String().atom); a < len(coldGlobalSlot) {
		if i := coldGlobalSlot[a]; i >= 0 && r.coldGlobals&(1<<i) != 0 {
			return int(i)
		}
	}
	return -1
}

// resolveColdGlobal defines the cold global binding key if it is pending.
func (r *Realm) resolveColdGlobal(key PropertyKey) {
	if i := r.coldGlobalIndex(key); i >= 0 {
		r.defineColdGlobal(i)
	}
}

// resolveColdGlobals defines every pending cold global binding, in order.
func (r *Realm) resolveColdGlobals() {
	for i := range coldGlobalKeys {
		if r.coldGlobals&(1<<i) != 0 {
			r.defineColdGlobal(i)
		}
	}
}

func (r *Realm) defineColdGlobal(i int) {
	g := r.Global
	var cell propCell
	switch {
	case !r.sharedIntrinsics:
		coldGlobalLate[i](r)
		if r.coldGlobals&(1<<i) != 0 {
			panic("engine: a late installer must bind its key")
		}
	case i >= firstLateGlobal:
		c := lateCells[i].Load()
		if c == nil {
			c = buildLateGroup(i)
		}
		cell = *c
		// The latest shared intrinsics hold the group's, and those of every
		// group built before it.
		r.Intrinsics = sharedIntr.Load()
		r.coldGlobals &^= 1 << i
	default:
		cell = sharedTpl.coldCells[i]
		r.coldGlobals &^= 1 << i
	}
	if r.coldGlobals == 0 {
		// A host's lazy properties may still be pending (lazyProps).
		if lp, ok := g.internal.(*lazyProps); !ok || len(lp.defs) == 0 {
			g.flags &^= flagHasLazy
			g.internal = nil
		}
	}
	if r.sharedIntrinsics {
		g.addNamed(r, coldGlobalKeys[i], cell)
	}
}

// The late groups of the shared realms. buildLateGroup runs a late
// installer once per process, in a builder realm over the latest shared
// intrinsics, freezes and shares what it created like the template, and
// publishes a copy of the intrinsics that holds it (sharedIntr) and the
// group's bindings (lateCells). A process that never names a late global
// carries none of its objects.
var (
	lateMu sync.Mutex // serializes the group builds (newGroupBuilder)
	// sharedIntr is the template's intrinsics with every group built so
	// far: each build publishes a new copy, the old ones stay valid.
	sharedIntr atomic.Pointer[Intrinsics]
	// lateCells are the late bindings of the groups built so far.
	lateCells [len(coldGlobalKeys)]atomic.Pointer[propCell]
)

// buildLateGroup builds the group of the late binding i for the shared
// realms, if no other goroutine has, and returns the binding.
func buildLateGroup(i int) *propCell {
	lateMu.Lock()
	defer lateMu.Unlock()
	if c := lateCells[i].Load(); c != nil {
		return c
	}
	// The other groups not built yet stay pending: an installer that needs
	// one (ensureLate) builds it along.
	b := newGroupBuilder()
	for j, install := range coldGlobalLate {
		if install != nil && lateCells[j].Load() == nil {
			b.coldGlobals |= 1 << j
		}
	}
	pending := b.coldGlobals
	b.defineColdGlobal(i)
	b.freezeIntrinsics()
	addSharedRoots(b.rootShapes)
	cells := make([]propCell, 0, 4)
	keys := b.Global.OwnPropertyKeys()
	for _, k := range keys {
		j := lateIndex(k)
		if pending&^b.coldGlobals&(1<<j) == 0 {
			panic("engine: a late installer bound a key outside its group")
		}
		c, _ := b.Global.getOwnCell(k)
		cells = append(cells, c)
	}
	// Readers load a cell, then sharedIntr: publish the intrinsics first.
	sharedIntr.Store(b.Intrinsics)
	for n, k := range keys {
		lateCells[lateIndex(k)].Store(&cells[n])
	}
	return lateCells[i].Load()
}

// newGroupBuilder returns a realm that builds a group of intrinsics for the
// shared realms (buildLateGroup, sharedAsyncIntrinsics) into a copy of the
// latest shared intrinsics, which it publishes once it has frozen them. It
// creates its objects from the shared roots, so their shapes join the shared
// tree (publish, under its lock), and binds globals on a global object of
// its own. The caller holds lateMu.
func newGroupBuilder() *Realm {
	tpl := sharedTpl
	intr := *sharedIntr.Load()
	ext := *intr.extIntrinsics
	intr.extIntrinsics = &ext
	b := &Realm{Intrinsics: &intr, buildingShared: true}
	b.stackCapture, b.stackCaptureInto, b.stackFormat = defaultStackCapture, defaultStackCaptureInto, defaultStackFormat
	b.nullProtoRoot, b.plainRoot, b.arrayRoot, b.funcShape = tpl.nullRoot, tpl.plainRoot, tpl.arrayRoot, tpl.funcShape
	b.Global = &Object{shape: newRootShape(nil), class: ClassObject, flags: flagExtensible}
	return b
}

// keyTable lists the properties of a namespace object (Math, Reflect) in
// definition order. A mutable realm defines each on the first lookup of its
// key (lazyKeys): a program that calls Math.floor pays for one function and
// shape rather than Math's 44.
type keyTable struct {
	defs []keyDef
}

// keyDef is one property of a keyTable: a native function when fn is set,
// a data constant otherwise.
type keyDef struct {
	key    PropertyKey
	name   *String
	value  Value
	fn     NativeFunc
	length int
	attrs  uint8
}

// newKeyTable lists values, then functions, then @@toStringTag tag.
func newKeyTable(values []valueDef, fns []builtinDef, tag *String) *keyTable {
	t := &keyTable{defs: make([]keyDef, 0, len(values)+len(fns)+1)}
	for _, d := range values {
		t.defs = append(t.defs, keyDef{key: StringKey(d.name), value: d.value, attrs: d.attrs})
	}
	for _, d := range fns {
		t.defs = append(t.defs, keyDef{key: StringKey(d.name), name: d.name, fn: d.fn, length: d.length, attrs: attrHidden})
	}
	t.defs = append(t.defs, keyDef{key: SymbolKey(SymToStringTag), value: StringValue(tag), attrs: attrConfigurable})
	if len(t.defs) >= 64 {
		panic("engine: a keyTable holds at most 63 properties")
	}
	return t
}

func (d *keyDef) cell(r *Realm) propCell {
	if d.fn != nil {
		return propCell{value: ObjectValue(r.NewNativeFunction(d.name, d.length, d.fn)), attrs: d.attrs}
	}
	return propCell{value: d.value, attrs: d.attrs}
}

// installKeys defines the properties of the namespace object o from t: at
// once while building the shared template, key by key in a mutable realm.
// There o starts from the root shape of the namespaces, which no other
// object has, so no inline cache filled on another object can hit it while
// a property is pending. The namespaces share it: one fills a cache only
// for a key it has defined (fillGetIC, setNamedSlow), which every
// namespace of that shape holds in the same slot. A namespace installed
// after the bootstrap (Atomics, with the binary globals) starts from a root
// of its own, since that root went with the bootstrap slabs.
func (r *Realm) installKeys(o *Object, t *keyTable) {
	if !r.buildingShared {
		o.mustBeMutable()
		if r.boot == nil {
			o.shape = newRootShape(o.proto)
		} else {
			if r.boot.nsRoot == nil {
				r.boot.nsRoot = newRootShape(o.proto)
			}
			o.shape = r.boot.nsRoot
		}
		o.internal = &lazyKeys{r: r, t: t, pending: 1<<len(t.defs) - 1}
		o.flags |= flagHasLazy
		return
	}
	o.ReserveSlots(r, len(t.defs))
	for i := range t.defs {
		o.addNamed(r, t.defs[i].key, t.defs[i].cell(r))
	}
}

// lazyKeys is the internal of a namespace object of a mutable realm while
// some of its properties are undefined: pending has a bit for each keyTable
// entry still to define. A lookup of an entry's key defines that entry
// alone; a lookup of any other key, the key list, a write, a redefinition,
// a deletion or a new prototype defines them all (resolveAll). So while
// any is pending, o's properties are the entries defined so far, as the
// table has them, and resolveAll can lay all of them out in table order:
// the key order of a shared realm, whatever the program touched first.
type lazyKeys struct {
	r       *Realm
	t       *keyTable
	pending uint64
}

// isPending reports whether a lookup of key would define properties.
func (lk *lazyKeys) isPending(key PropertyKey) bool {
	for i := range lk.t.defs {
		if lk.t.defs[i].key == key {
			return lk.pending&(1<<i) != 0
		}
	}
	return true
}

// resolve defines key, about to be read, if it is a pending entry, and
// every entry if key is not in the table.
func (lk *lazyKeys) resolve(o *Object, key PropertyKey) {
	for i := range lk.t.defs {
		if lk.t.defs[i].key == key {
			if lk.pending&(1<<i) != 0 {
				lk.pending &^= 1 << i
				if lk.pending == 0 {
					o.internal = nil
					o.flags &^= flagHasLazy
				}
				o.addNamed(lk.r, key, lk.t.defs[i].cell(lk.r))
			}
			return
		}
	}
	lk.resolveAll(o)
}

// resolveAll defines every pending entry, restarting o's shape from its
// root when some entries were defined already (keeping their values).
func (lk *lazyKeys) resolveAll(o *Object) {
	r, defs := lk.r, lk.t.defs
	var kept [63]Value
	if lk.pending != 1<<len(defs)-1 {
		for i := range defs {
			if lk.pending&(1<<i) == 0 {
				v, _, _ := o.lookupNamed(defs[i].key)
				kept[i] = *v
			}
		}
		for o.shape.count != 0 {
			o.shape = o.shape.parent
		}
		clear(o.slots)
		o.slots = o.slots[:0]
		r.bumpEpoch(o)
	}
	o.internal = nil
	o.flags &^= flagHasLazy
	o.ReserveSlots(r, len(defs))
	for i := range defs {
		cell := propCell{value: kept[i], attrs: defs[i].attrs}
		if lk.pending&(1<<i) != 0 {
			cell = defs[i].cell(r)
		}
		o.addNamed(r, defs[i].key, cell)
	}
}

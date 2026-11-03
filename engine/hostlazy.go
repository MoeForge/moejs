package engine

import (
	"encoding/json"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// Lazy host conversion. FromGo turns a non-empty JSON-shaped Go map
// or slice into a placeholder node that holds the Go value and nothing else;
// the node materializes on the first operation that needs its own
// properties, converting exactly one level: its direct children become
// placeholders in turn (or leaves: strings, numbers, functions, ...). A hook
// therefore pays for the parts of its argument it reads.
//
// Every JavaScript-observable operation behaves as it did under the eager
// conversion: materialization produces the object eager conversion built
// (named keys sorted and laid out by the shared host shape, canonical array
// indices stored as elements, the same attributes), and each Go container
// reached along a property path becomes one JavaScript object, materialized
// at most once. What changes is when the Go value is read: at the first
// touch of each node, not at the call.
//
// A placeholder is in dictionary mode with no dictionary and has its realm's
// host sentinel as shape, so every inline cache misses and every shape-mode
// fast path skips it; flagHasLazy routes the slow paths to resolveHost, which
// materializes the node. The sentinel is the first field of the realm's
// hostHeap, which is how a placeholder finds its realm (hostlazy_unsafe.go).
// A node of a map is an ordinary Object whose internal holds the Go map (a
// pointer, so boxing it allocates nothing). A node of a slice is an ordinary
// array, whose internal must stay the *ArrayData every array path reads; it
// keeps the address of the boxed Go slice header as the data pointer of its
// empty slots (hostlazy_unsafe.go) and the slice's type in the ArrayData's
// spare byte, so it is no larger than an eager array. A placeholder's slots
// are unused (it is in dictionary mode), and a materialized array has no named
// properties until JavaScript adds one, which replaces the slots: from then
// on the node counts as modified. Map nodes keep their Go value for the
// node's lifetime; Internal hides it.
//
// Export (ToGo) of a node JavaScript did not modify returns the original Go
// value, so a map or slice JavaScript only passed through comes back as
// itself. Writes are not intercepted (the inline caches store into slots
// directly), so "unmodified" is decided at export, per node: a placeholder
// is unmodified, and a materialized node is when its own properties still
// are exactly what materialization made of src, each child container being
// the same Go value and itself unmodified (hostUnchanged). Anything else
// exports a snapshot of the node's current JavaScript state, whose
// unmodified child nodes still export their Go values. An empty container
// is no node (hostRoot, mapChild, sliceChild make it an ordinary object or
// array), and a []byte or function child is a new object at each
// materialization, which hostUnchanged cannot match: its parent exports a
// snapshot once materialized.

// hostSentinelKey is the key of every realm's host sentinel shape; like
// rootKey it is a reserved bit pattern no property key can have.
var hostSentinelKey = PropertyKey{Value{bits: reservedPrefix | 5}}

// hostHeap is a realm's lazy-conversion state (Realm.fromGo), allocated by
// the first container conversion.
//
// Storage: roots, nodes and values are carved from chunks, one per kind. A
// chunk is sized from the usage of earlier periods. A period ends at
// Realm.ReleaseCallData, which a host that pools realms calls where a
// request ends, or else at the first top-level FromGo of a container (one
// made outside any JavaScript call) after the period materialized something.
// In a hook runtime it is therefore one request, or one call without
// ReleaseCallData: all the arguments plus everything the calls materialize,
// including containers host functions return. A hook that reads the same
// parts of a same-shaped argument on every call therefore costs one chunk of
// each kind per call, as the eager conversion's slabs did. Ending a period
// drops the realm's references to its chunks: they live on only through the
// nodes that point into them, so a string the module stores keeps every
// string converted in its period (up to 512), and an object it stores keeps
// its chunk and, through the root placeholder in it, the whole argument.
// Until then the realm retains the period's chunks, at most one of each
// kind, and through their placeholders the Go values of the arguments, as
// the interpreter's register stack retains the last call's arguments until
// ReleaseCallData clears it. Without a prediction (a realm's first
// conversions) a root is allocated on its own rather than opening a chunk
// for one object.
type hostHeap struct {
	// sentinel must stay the first field: hostHeapOf maps a placeholder's
	// shape back to its heap.
	sentinel Shape
	r        *Realm
	// touched is set by a materialization: the next top-level root starts
	// a new period.
	touched bool

	maps    []Object
	arrays  []arrayObject
	values  []Value
	strings []String

	used hostCounts // handed out in the current period
	hint hostCounts // chunk sizing: the recent periods' usage

	// strs holds the strings converted since the current strings chunk
	// began, indexed by a hash of their content (strSlot): a hook argument
	// often holds one Go string in several places (new-api copies the model
	// into upstreamModel), and sharing an immutable String is unobservable.
	strs [hostStrSlots]*String

	// shapes predicts the host shape of the next map from its size: the
	// entries of the last two key sets of each size (modulo hostShapeSlots)
	// laid out, most recent first. A map of the size that holds every key
	// of one is laid out by looking its keys up in the entry's order, which
	// skips collecting, sorting and hashing its entries: the maps of a hook
	// argument's list mostly share one key set.
	shapes [hostShapeSlots][2]*hostShapeEntry
	// shapeKeys counts the keys of the entries in Realm.hostShapes
	// (hostShapeKeysMax).
	shapeKeys int

	// Pair scratch for materializing maps, kept between calls (cleared of
	// host data) so steady-state materialization allocates none.
	anyPairs  []hostPair[any]
	strPairs  []hostPair[string]
	listPairs []hostPair[[]string]
}

// hostCounts tallies chunk storage by kind.
type hostCounts struct {
	maps, arrays, values, strings int
}

// The Go slice types a host array node converts (ArrayData.host); a
// json.RawMessage is the text of an array (hostraw.go).
const (
	hostSliceAny uint8 = 1 + iota
	hostSliceString
	hostSliceMaps
	hostSliceRaw
)

// Chunk sizing: a chunk holds exactly what the recent periods predict the
// rest of the period needs. Past the prediction (or without one) a chunk
// holds what the period has already used beyond it, at least a small
// per-kind minimum, so the overflow doubles from a few items: hooks that
// rotate through one runtime (a plugin's decodeRequest, then its
// buildSubmitRequest) mispredict by a few nodes, and a large first chunk
// wasted more than the extra allocation of a later one costs. Chunks are
// capped per kind (about 32 KiB) unless a single request is larger.
const (
	minChunkMaps     = 4  // 384 B
	minChunkArrays   = 4  // 416 B
	minChunkValues   = 16 // 256 B
	minChunkStrings  = 6  // 432 B
	maxChunkMaps     = 256
	maxChunkArrays   = 256
	maxChunkValues   = 2048
	maxChunkStrings  = 512
	maxHostHintDecay = 2 // an idle period halves the hint
	hostStrBits      = 4
	hostStrSlots     = 1 << hostStrBits
	hostShapeSlots   = 8
)

// hostHeap returns the realm's heap, allocating it on first use.
func (r *Realm) hostHeap() *hostHeap {
	h := r.fromGo.heap
	if h == nil {
		h = &hostHeap{r: r}
		h.sentinel = Shape{key: hostSentinelKey, isDict: true}
		r.fromGo.heap = h
	}
	return h
}

// seal ends the period: its usage becomes the chunk hint (decaying rather
// than dropping when usage shrinks) and the realm lets go of its chunks.
// ReleaseCallData seals a period that materialized nothing too, whose root
// placeholders hold their Go values; its roots then are its usage.
func (h *hostHeap) seal() {
	if h.used != (hostCounts{}) {
		h.hint = hostCounts{
			maps:    max(h.used.maps, h.hint.maps/maxHostHintDecay),
			arrays:  max(h.used.arrays, h.hint.arrays/maxHostHintDecay),
			values:  max(h.used.values, h.hint.values/maxHostHintDecay),
			strings: max(h.used.strings, h.hint.strings/maxHostHintDecay),
		}
		h.used = hostCounts{}
	}
	h.maps, h.arrays, h.values, h.strings = nil, nil, nil, nil
	h.strs = [hostStrSlots]*String{}
	h.touched = false
}

// chunkCap sizes a new chunk that must hold need items.
func chunkCap(need, hint, used, least, limit int) int {
	c := hint - used
	if c <= 0 {
		c = max(-c, least)
	}
	return max(min(c, limit), need)
}

func (h *hostHeap) object() *Object {
	if len(h.maps) == cap(h.maps) {
		h.maps = make([]Object, 0, chunkCap(1, h.hint.maps, h.used.maps, minChunkMaps, maxChunkMaps))
	}
	n := len(h.maps)
	h.maps = h.maps[:n+1]
	h.used.maps++
	return &h.maps[n]
}

// rootObject returns storage for the root of a map: from the chunk when it
// has room or the hint predicts more maps this period, else on its own.
func (h *hostHeap) rootObject() *Object {
	if len(h.maps) == cap(h.maps) && h.hint.maps <= h.used.maps {
		h.used.maps++
		return new(Object)
	}
	return h.object()
}

// rootArray is rootObject for the root of a slice.
func (h *hostHeap) rootArray() *arrayObject {
	if len(h.arrays) == cap(h.arrays) && h.hint.arrays <= h.used.arrays {
		h.used.arrays++
		return new(arrayObject)
	}
	return h.array()
}

func (h *hostHeap) array() *arrayObject {
	if len(h.arrays) == cap(h.arrays) {
		h.arrays = make([]arrayObject, 0, chunkCap(1, h.hint.arrays, h.used.arrays, minChunkArrays, maxChunkArrays))
	}
	n := len(h.arrays)
	h.arrays = h.arrays[:n+1]
	h.used.arrays++
	return &h.arrays[n]
}

// valueSlice returns n zero values whose capacity ends at n, so an append
// by the object that owns them reallocates instead of overwriting a neighbour.
func (h *hostHeap) valueSlice(n int) []Value {
	start := len(h.values)
	if start+n > cap(h.values) {
		h.values = make([]Value, 0, chunkCap(n, h.hint.values, h.used.values, minChunkValues, maxChunkValues))
		start = 0
	}
	h.values = h.values[:start+n]
	h.used.values += n
	return h.values[start : start+n : start+n]
}

// strSlot is the strs index of the non-empty g, a Fibonacci hash of its
// length and its first, middle and last bytes. It depends on the content
// alone, never on where the linker or the heap put g, so which strings
// evict each other, and with that the chunk usage, is the same in every
// build and process; a hit is still the same Go string (sameGoString).
func strSlot(g string) int {
	n := len(g)
	k := uint64(n) ^ uint64(g[0])<<8 ^ uint64(g[n>>1])<<16 ^ uint64(g[n-1])<<24
	return int(k * 0x9E3779B97F4A7C15 >> (64 - hostStrBits))
}

// str is FromHostString with a short ASCII String's header carved from a
// chunk; a Go string already converted this period yields the same String.
// Any other is a lazy string, charged as decoded (accountLazy), as its
// conversion was.
func (h *hostHeap) str(g string) *String {
	if len(g) == 0 {
		return emptyString
	}
	slot := strSlot(g)
	if s := h.strs[slot]; s != nil && sameGoString(s.s, g) {
		return s
	}
	var s *String
	if len(g) < jsonPlainMin && isASCII(g) {
		if len(h.strings) == cap(h.strings) {
			h.strings = make([]String, 0, chunkCap(1, h.hint.strings, h.used.strings, minChunkStrings, maxChunkStrings))
			h.strs = [hostStrSlots]*String{} // keep no old chunk alive
		}
		n := len(h.strings)
		h.strings = h.strings[:n+1]
		h.used.strings++
		s = &h.strings[n]
		*s = String{s: g, n: int32(len(g)), kind: strASCII, flags: strHost}
	} else {
		s = newHostLazyString(g)
		// A substring shares none of it, as one of a carved string
		// (shareRoot): the host owns the text.
		s.flags |= strHost
		h.r.accountLazy(s)
	}
	h.strs[slot] = s
	return s
}

// --- placeholders -----------------------------------------------------------------

// mapNode makes o (zeroed) a placeholder for the non-empty map src.
func (h *hostHeap) mapNode(o *Object, src any) *Object {
	o.shape = &h.sentinel
	o.proto = h.r.ObjectPrototype
	o.class = ClassObject
	o.flags = flagExtensible | flagDict | flagHasLazy | flagHostNode
	o.internal = src
	return o
}

// sliceNode makes a (zeroed) a placeholder for the non-empty slice src of
// length n and type kind.
func (h *hostHeap) sliceNode(a *arrayObject, src any, kind uint8, n int) *Object {
	o := &a.obj
	o.shape = &h.sentinel
	o.proto = h.r.ArrayPrototype
	o.class = ClassArray
	o.flags = flagExtensible | flagDict | flagHasLazy | flagHostNode
	a.ad = ArrayData{length: uint32(n), lengthWritable: true, host: kind}
	o.internal = &a.ad
	o.slots = boxedSliceOf(src).slots()
	return o
}

// hostRoot converts a FromGo argument that is a native container: a placeholder
// for a non-empty one, an ordinary empty object or array otherwise, null for
// a nil map. It reports false for any other value.
func (r *Realm) hostRoot(v any) (Value, bool) {
	n, slice := 0, uint8(0)
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return Null(), true
		}
		n = len(x)
	case []any:
		n, slice = len(x), hostSliceAny
	case map[string]string:
		if x == nil {
			return Null(), true
		}
		n = len(x)
	case []string:
		n, slice = len(x), hostSliceString
	case map[string][]string:
		if x == nil {
			return Null(), true
		}
		n = len(x)
	case []map[string]any:
		n, slice = len(x), hostSliceMaps
	default:
		return Value{}, false
	}
	if n == 0 {
		if slice != 0 {
			return ObjectValue(r.NewArrayFromSlice(nil)), true
		}
		return ObjectValue(r.NewObject()), true
	}
	h := r.hostHeap()
	if h.touched && r.callDepth == 0 {
		h.seal()
	}
	used := h.used
	var root *Object
	if slice != 0 {
		root = h.sliceNode(h.rootArray(), v, slice, n)
	} else {
		root = h.mapNode(h.rootObject(), v)
	}
	h.chargeSince(used)
	return ObjectValue(root), true
}

// child converts a value found inside a materializing container.
func (h *hostHeap) child(v any) (Value, error) {
	switch x := v.(type) {
	case string:
		return StringValue(h.str(x)), nil
	case map[string]any:
		return h.mapChild(x), nil
	case []any:
		return h.sliceChild(v, hostSliceAny, len(x)), nil
	case float64:
		return NumberValue(x), nil
	case bool:
		return Bool(x), nil
	case nil:
		return Null(), nil
	case map[string]string:
		if x == nil {
			return Null(), nil
		}
		if len(x) == 0 {
			return h.emptyObject(), nil
		}
		return ObjectValue(h.mapNode(h.object(), v)), nil
	case map[string][]string:
		if x == nil {
			return Null(), nil
		}
		if len(x) == 0 {
			return h.emptyObject(), nil
		}
		return ObjectValue(h.mapNode(h.object(), v)), nil
	case []string:
		return h.sliceChild(v, hostSliceString, len(x)), nil
	case []map[string]any:
		return h.sliceChild(v, hostSliceMaps, len(x)), nil
	}
	return h.r.fromGoOther(v, h)
}

// mapChild converts a map[string]any child; boxing the map (a pointer) into
// the placeholder's internal allocates nothing.
func (h *hostHeap) mapChild(m map[string]any) Value {
	if m == nil {
		return Null()
	}
	if len(m) == 0 {
		return h.emptyObject()
	}
	return ObjectValue(h.mapNode(h.object(), m))
}

// sliceChild converts a slice child already boxed in v.
func (h *hostHeap) sliceChild(v any, kind uint8, n int) Value {
	a := h.array()
	if n == 0 {
		return ObjectValue(h.r.initArray(a, nil, 0))
	}
	return ObjectValue(h.sliceNode(a, v, kind, n))
}

func (h *hostHeap) emptyObject() Value {
	return ObjectValue(initObject(h.object(), ClassObject, h.r.plainRoot))
}

// stringArray converts a []string value of a map[string][]string eagerly:
// boxing the slice for a placeholder would cost an allocation per entry.
func (h *hostHeap) stringArray(list []string) Value {
	items := h.valueSlice(len(list))
	for i, s := range list {
		items[i] = StringValue(h.str(s))
	}
	return ObjectValue(h.r.initArray(h.array(), items, uint32(len(items))))
}

// cellOf is the property a converted child becomes: a data property, or,
// when the child could not be converted (a Go type FromGo rejects), an
// accessor whose getter throws the conversion error. Eager conversion failed
// the whole FromGo instead; lazily the error surfaces where the value is read.
func (h *hostHeap) cellOf(v Value, err error) propCell {
	if err == nil {
		return propCell{value: v, attrs: attrDefault}
	}
	get := h.r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		switch err.(type) {
		case *Exception, *InterruptedError:
			return Undefined(), err
		}
		return Undefined(), r.TypeError("%s", err.Error())
	})
	return propCell{value: accessorValue(&Accessor{Get: get}), attrs: attrEnumerable | attrConfigurable | attrAccessor}
}

// --- materialization ----------------------------------------------------------------

// materializeHost materializes o if it is a host placeholder and reports
// whether it was one.
func (o *Object) materializeHost() bool {
	if o.flags&flagHasLazy == 0 || o.shape.key != hostSentinelKey {
		return false
	}
	hostHeapOf(o.shape).materialize(o)
	return true
}

// resolveHost prepares a host placeholder for a lookup of key. It reports
// false, leaving o unmaterialized, when key cannot be one of its properties
// (a symbol, or a non-index string on an array: Go containers have neither);
// otherwise o is materialized (or was not a placeholder) and the caller looks
// key up as usual.
func (o *Object) resolveHost(key PropertyKey) bool {
	if o.shape.key != hostSentinelKey {
		return true
	}
	if key.IsSymbol() || (o.class == ClassArray && !key.IsIndex()) {
		return false
	}
	hostHeapOf(o.shape).materialize(o)
	return true
}

// HostValue returns the Go map or slice o was converted from by FromGo when
// JavaScript has not modified o (see the file comment), and for a
// json.RawMessage JavaScript has not read a new RawMessage with its text;
// ok is false for every other object.
func (o *Object) HostValue() (v any, ok bool) {
	// A placeholder is unchanged: no call of hostUnchanged for it.
	if o.flags&flagHostNode == 0 || o.shape.key != hostSentinelKey && !o.hostUnchanged(0) {
		return nil, false
	}
	src := o.hostSrc()
	if t, ok := src.(rawText); ok {
		return json.RawMessage(t), true // a copy, which the host may keep and change
	}
	return src, true
}

// hostSrc is the Go value of the host node o; nil for an array node whose
// slots JavaScript replaced by adding a named property.
func (o *Object) hostSrc() any {
	if o.class == ClassArray {
		b, ok := boxedSliceIn(o.slots)
		if !ok {
			return nil
		}
		return b.slice(o.internal.(*ArrayData).host)
	}
	return o.internal
}

// materialize converts one level of the placeholder o in place.
func (h *hostHeap) materialize(o *Object) {
	used := h.used
	o.flags &^= flagDict | flagHasLazy
	h.touched = true
	if o.class == ClassArray {
		h.materializeArray(o)
	} else {
		switch m := o.internal.(type) {
		case map[string]any:
			materializeMap(h, o, m, &h.anyPairs, hostConvAny{})
		case map[string]string:
			materializeMap(h, o, m, &h.strPairs, hostConvString{})
		case map[string][]string:
			materializeMap(h, o, m, &h.listPairs, hostConvList{})
		case rawText: // a json.RawMessage (hostraw.go)
			h.materializeRaw(o)
		}
	}
	h.r.bumpEpoch(o)
	h.chargeSince(used)
}

// chargeSince charges the storage the heap handed out since its usage was
// used to the realm's memory limit: the objects, arrays, values and
// strings of the chunks, by what a conversion takes, not by chunk.
func (h *hostHeap) chargeSince(used hostCounts) {
	if h.r.mem() == nil {
		return
	}
	d := h.used
	n := (d.maps-used.maps)*int(unsafe.Sizeof(Object{})) + (d.arrays-used.arrays)*int(unsafe.Sizeof(arrayObject{})) +
		(d.values-used.values)*valueSize + (d.strings-used.strings)*stringHeaderSize
	if n > 0 {
		h.r.chargeMany(n, memObject, d.maps-used.maps+d.arrays-used.arrays)
	}
}

func (h *hostHeap) materializeArray(o *Object) {
	if o.proto == h.r.ArrayPrototype {
		o.shape = h.r.arrayShape()
	} else {
		o.shape = h.r.rootShapeFor(o.proto)
	}
	switch x := o.hostSrc().(type) {
	case []any:
		items := h.valueSlice(len(x))
		o.elements = items
		for i, e := range x {
			v, err := h.child(e)
			if err != nil {
				items[i] = Hole()
				o.sparseMap()[uint32(i)] = h.cellOf(v, err)
				continue
			}
			items[i] = v
		}
	case []map[string]any:
		items := h.valueSlice(len(x))
		for i, m := range x {
			items[i] = h.mapChild(m)
		}
		o.elements = items
	case []string:
		items := h.valueSlice(len(x))
		for i, s := range x {
			items[i] = StringValue(h.str(s))
		}
		o.elements = items
	default: // a json.RawMessage
		h.materializeRaw(o)
	}
}

// hostConv converts one map value; the zero-size implementations keep
// materializeMap generic over the map's value type.
type hostConv[V any] interface {
	conv(h *hostHeap, v V) (Value, error)
}

type (
	hostConvAny    struct{}
	hostConvString struct{}
	hostConvList   struct{}
)

func (hostConvAny) conv(h *hostHeap, v any) (Value, error) { return h.child(v) }

func (hostConvString) conv(h *hostHeap, v string) (Value, error) {
	return StringValue(h.str(v)), nil
}

func (hostConvList) conv(h *hostHeap, v []string) (Value, error) { return h.stringArray(v), nil }

// materializeMap lays out the entries of m as eager conversion did: named
// keys sorted under the cached host shape (replayed onto the object's root
// if its prototype was changed while it was a placeholder) or, past
// maxShapeProps, in a dictionary, canonical array indices as elements.
func materializeMap[V any, C hostConv[V]](h *hostHeap, o *Object, m map[string]V, scratch *[]hostPair[V], c C) {
	r := h.r
	pairs := (*scratch)[:0]
	if cap(pairs) < len(m) {
		pairs = make([]hostPair[V], 0, max(len(m), initialPairs))
	}
	var ways *[2]*hostShapeEntry
	var e *hostShapeEntry
	if len(m) > 0 && len(m) <= maxShapeProps {
		ways = &h.shapes[len(m)%hostShapeSlots]
		pairs, e = predictPairs(ways, m, pairs)
	}
	named := pairs
	var hash uint64
	if e == nil {
		for k, v := range m {
			pairs = append(pairs, hostPair[V]{k, v})
		}
		orderPairs(pairs)
		named = pairs
		hash = uint64(14695981039346656037)
		for i := range pairs {
			k := pairs[i].k
			if _, ok := parseArrayIndex(k); ok {
				named = pairs[:i]
				break
			}
			for j := range len(k) {
				hash = (hash ^ uint64(k[j])) * 1099511628211
			}
			hash = (hash ^ 0xFF) * 1099511628211
		}
	}
	root := r.rootShapeFor(o.proto)
	o.shape = root
	if len(named) > maxShapeProps {
		// Past what a shape holds: a dictionary from the start.
		d := o.dict
		if d == nil {
			d = &dictProps{}
			o.dict = d
		}
		d.index = make(map[PropertyKey]int32, len(named))
		d.entries = make([]dictEntry, 0, len(named))
		for i := range named {
			d.add(StringKey(r.InternGoString(named[i].k)), h.cellOf(c.conv(h, named[i].v)))
		}
		if len(d.index) != len(d.entries) {
			d.dropShadowed() // keys that decode to one atom (shadowHostKeys)
		}
		o.shape, o.slots = dictShape, nil
		o.flags |= flagDict
		r.bumpEpoch(o)
	} else if len(named) > 0 {
		slots := h.valueSlice(len(named))
		for i := range named {
			v, err := c.conv(h, named[i].v)
			if err != nil {
				// The all-data host shape cannot describe the throwing
				// accessor: lay the object out key by key instead.
				for j := range i {
					o.addHostNamed(r, named[j].k, propCell{value: slots[j], attrs: attrDefault})
				}
				o.addHostNamed(r, named[i].k, h.cellOf(v, err))
				for j := i + 1; j < len(named); j++ {
					o.addHostNamed(r, named[j].k, h.cellOf(c.conv(h, named[j].v)))
				}
				slots = nil
				break
			}
			slots[i] = v
		}
		if slots != nil {
			if e == nil {
				e = hostShapeFor(h, hash, named)
				if ways != nil && len(named) == len(pairs) {
					ways[0], ways[1] = e, ways[0]
				}
			}
			shape := e.shape
			if root != r.plainRoot {
				shape = shape.rebase(r, root)
			}
			o.shape, o.slots = shape, slots
		}
	}
	for _, p := range pairs[len(named):] {
		idx, _ := parseArrayIndex(p.k)
		o.addIndex(r, idx, h.cellOf(c.conv(h, p.v)))
	}
	clear(pairs)
	*scratch = trimScratch(pairs)
}

// addHostNamed adds the entry k of a Go map laid out key by key. A key that
// decodes to the atom of an earlier one replaces it (shadowHostKeys).
func (o *Object) addHostNamed(r *Realm, k string, c propCell) {
	key := StringKey(r.InternGoString(k))
	if key.String().kind != strASCII {
		if _, _, ok := o.lookupNamed(key); ok {
			o.removeNamed(r, key)
		}
	}
	o.addNamed(r, key, c)
}

// predictPairs fills pairs with the entries of m in the key order of the
// predicted entry that lists exactly m's keys, moving it to the front, and
// returns it; without one it returns pairs empty and a nil entry.
func predictPairs[V any](ways *[2]*hostShapeEntry, m map[string]V, pairs []hostPair[V]) ([]hostPair[V], *hostShapeEntry) {
next:
	for w, e := range ways {
		if e == nil || len(e.keys) != len(m) {
			continue
		}
		pairs = pairs[:len(m)]
		for i, k := range e.keys {
			v, ok := m[k]
			if !ok {
				continue next
			}
			pairs[i] = hostPair[V]{k, v}
		}
		if w != 0 {
			ways[0], ways[1] = e, ways[0]
		}
		return pairs, e
	}
	clear(pairs)
	return pairs[:0], nil
}

// --- export ------------------------------------------------------------------------

// maxHostVerifyDepth bounds how deep hostUnchanged follows materialized
// children; below it a node counts as modified, which only costs a snapshot
// export. The bound also keeps a verification that fails deep down from being
// repeated for every level of the export that follows it.
const maxHostVerifyDepth = 64

// hostUnchanged reports whether the host node o still holds exactly what
// materialization made of its Go value, down through its materialized
// children (a placeholder holds nothing yet, so it is unchanged). Only what
// Export reads is compared: own enumerable string-keyed data properties of a
// map node (their count, keys and values) and the length and elements of a
// slice node; attributes that do not change the export (writable,
// configurable, extensible, the prototype) are ignored. A value counts as
// what materialization made of a Go value when it converts to it again
// without allocating: an equal string or number, the same object, or, for a
// container, the node of that very Go value (same data pointer), itself
// unchanged. Values Export would read through a getter, or that come from a
// host hook or a Go function, make the node modified.
func (o *Object) hostUnchanged(depth int) bool {
	if o.shape.key == hostSentinelKey {
		return true
	}
	if depth >= maxHostVerifyDepth {
		return false
	}
	if o.class == ClassArray {
		return o.hostSliceUnchanged(depth)
	}
	return o.hostMapUnchanged(depth)
}

func (o *Object) hostMapUnchanged(depth int) bool {
	src := o.internal
	// Canonical array-index keys live in the elements (or the sparse map).
	var sparse map[uint32]propCell
	if o.dict != nil {
		sparse = o.dict.sparse
	}
	indices := len(sparse)
	for _, v := range o.elements {
		if !v.IsHole() {
			indices++
		}
	}
	if o.flags&flagDict == 0 {
		props := o.shape.Props()
		if len(props)+indices != hostLen(src) {
			return false
		}
		for i := range props {
			if !hostPropMatches(src, props[i].key, props[i].attrs, o.slots[i]) {
				return false
			}
		}
		for _, v := range o.slots {
			if !hostChildUnchanged(v, depth) {
				return false
			}
		}
	} else {
		d := o.dict
		if d == nil || len(d.entries)-d.dead+indices != hostLen(src) {
			return false
		}
		for i := range d.entries {
			e := &d.entries[i]
			if e.live && !hostPropMatches(src, e.key, e.cell.attrs, e.cell.value) {
				return false
			}
		}
		for i := range d.entries {
			if e := &d.entries[i]; e.live && !hostChildUnchanged(e.cell.value, depth) {
				return false
			}
		}
	}
	if indices == 0 {
		return true
	}
	var buf [10]byte
	attrs := o.elementAttrs()
	for i, v := range o.elements {
		if !v.IsHole() && (!hostEntryMatches(src, string(strconv.AppendUint(buf[:0], uint64(i), 10)), attrs, v) || !hostChildUnchanged(v, depth)) {
			return false
		}
	}
	for i, c := range sparse {
		if !hostEntryMatches(src, string(strconv.AppendUint(buf[:0], uint64(i), 10)), c.attrs, c.value) || !hostChildUnchanged(c.value, depth) {
			return false
		}
	}
	return true
}

func (o *Object) hostSliceUnchanged(depth int) bool {
	src := o.hostSrc()
	n := hostLen(src)
	if src == nil || o.internal.(*ArrayData).length != uint32(n) || len(o.elements) != n || (o.dict != nil && len(o.dict.sparse) != 0) {
		return false
	}
	items := o.elements
	switch x := src.(type) {
	case []any:
		for i, e := range x {
			if !hostValueMatches(items[i], e) {
				return false
			}
		}
	case []string:
		for i, s := range x {
			if !items[i].IsString() || !hostStringEquals(items[i].AsString(), s) {
				return false
			}
		}
	case []map[string]any:
		for i, m := range x {
			if !hostValueMatches(items[i], m) {
				return false
			}
		}
	}
	for _, v := range items {
		if !hostChildUnchanged(v, depth) {
			return false
		}
	}
	return true
}

// hostPropMatches compares one own property of a map node with the entry of
// src under the same key; the caller has checked that the counts agree, so
// matching every property matches the key sets too.
func hostPropMatches(src any, key PropertyKey, attrs uint8, v Value) bool {
	return key.IsString() && hostEntryMatches(src, key.GoString(), attrs, v)
}

// hostEntryMatches compares the own property k (with attributes attrs and
// value v) with the entry of src under k.
func hostEntryMatches(src any, k string, attrs uint8, v Value) bool {
	if attrs&(attrEnumerable|attrAccessor) != attrEnumerable {
		return false
	}
	switch m := src.(type) {
	case map[string]any:
		e, ok := m[k]
		return ok && hostValueMatches(v, e)
	case map[string]string:
		s, ok := m[k]
		return ok && v.IsString() && hostStringEquals(v.AsString(), s)
	case map[string][]string:
		list, ok := m[k]
		return ok && hostStringsMatch(v, list)
	}
	return false
}

// hostValueMatches reports whether v is what materialization makes of the Go
// value e (a child container only by identity; hostChildUnchanged checks the
// child's own state).
func hostValueMatches(v Value, e any) bool {
	switch x := e.(type) {
	case string:
		return v.IsString() && hostStringEquals(v.AsString(), x)
	case float64:
		return v == NumberValue(x)
	case bool:
		return v == Bool(x)
	case nil:
		return v.IsNull()
	case map[string]any:
		return hostContainerMatches(v, e, len(x), x == nil, false)
	case map[string]string:
		return hostContainerMatches(v, e, len(x), x == nil, false)
	case map[string][]string:
		return hostContainerMatches(v, e, len(x), x == nil, false)
	case []any:
		return hostContainerMatches(v, e, len(x), false, true)
	case []string:
		return hostContainerMatches(v, e, len(x), false, true)
	case []map[string]any:
		return hostContainerMatches(v, e, len(x), false, true)
	}
	want, ok, err := scalarFromGo(e)
	return ok && err == nil && v == want
}

// hostContainerMatches matches a child container: null for a nil map, an
// empty object or array for an empty one (converted to an ordinary object),
// the node of e itself otherwise.
func hostContainerMatches(v Value, e any, n int, isNil, isArray bool) bool {
	if isNil {
		return v.IsNull()
	}
	if !v.IsObject() {
		return false
	}
	c := v.AsObject()
	if n != 0 {
		return c.flags&flagHostNode != 0 && sameEface(c.hostSrc(), e)
	}
	// Elements, dense or sparse, are a change; a dictProps without sparse
	// elements is not one by itself (dictionary mode is checked below): an
	// empty object that became a prototype or a WeakMap key has one (its
	// root, its weak entries) and still matches.
	if len(c.elements) != 0 || c.dict != nil && len(c.dict.sparse) != 0 {
		return false
	}
	if isArray {
		return c.class == ClassArray && c.internal.(*ArrayData).length == 0
	}
	return c.class == ClassObject && c.flags&flagDict == 0 && c.shape.count == 0
}

// hostChildUnchanged is hostUnchanged for a matched child value.
func hostChildUnchanged(v Value, depth int) bool {
	if !v.IsObject() {
		return true
	}
	c := v.AsObject()
	return c.flags&flagHostNode == 0 || c.hostUnchanged(depth+1)
}

// hostStringsMatch matches the eagerly converted array of a
// map[string][]string entry.
func hostStringsMatch(v Value, list []string) bool {
	if !v.IsObject() {
		return false
	}
	a := v.AsObject()
	if a.class != ClassArray || a.internal.(*ArrayData).length != uint32(len(list)) || len(a.elements) != len(list) || (a.dict != nil && len(a.dict.sparse) != 0) {
		return false
	}
	for i, s := range list {
		if !a.elements[i].IsString() || !hostStringEquals(a.elements[i].AsString(), s) {
			return false
		}
	}
	return true
}

// hostLen is the length of a host container value.
func hostLen(src any) int {
	switch x := src.(type) {
	case map[string]any:
		return len(x)
	case map[string]string:
		return len(x)
	case map[string][]string:
		return len(x)
	case []any:
		return len(x)
	case []string:
		return len(x)
	case []map[string]any:
		return len(x)
	}
	return -1
}

// hostStringEquals is EqualsGoString without the allocation of a UTF-16
// string's Go form; like it, it is false for a g that is not valid UTF-8
// (materialization replaced its bad bytes). A lazy string whose source is
// valid UTF-8 compares the source.
func hostStringEquals(s *String, g string) bool {
	if src, ok := s.sourceUTF8(); ok {
		return src == g
	}
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		return f.s == g
	}
	u, i := f.units(), 0
	for _, c := range g {
		if c < 0x10000 {
			if i >= len(u) || u[i] != uint16(c) {
				return false
			}
			i++
			continue
		}
		hi, lo := utf16.EncodeRune(c)
		if i+1 >= len(u) || u[i] != uint16(hi) || u[i+1] != uint16(lo) {
			return false
		}
		i += 2
	}
	return i == len(u) && utf8.ValidString(g)
}

package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxToGoDepth bounds container nesting in ToGo/ToGoStrict: a plugin can
// build a 3e6-deep array in well under a second, and exporting it
// recursively overflowed the Go stack, a fatal error. Past the limit
// ToGoStrict returns a RangeError that wraps ErrTooDeep and ToGo exports
// the deeper containers as nil.
const MaxToGoDepth = 10000

// ErrTooDeep is what the RangeError of a value nested deeper than
// MaxToGoDepth arrays and objects wraps, so that errors.Is finds it in the
// *Exception: JSON.parse's text and its reviver's walk, JSON.stringify's
// value, and the value ToGoStrict exports.
var ErrTooDeep = errors.New("nesting is too deep")

// tooDeep returns the RangeError of a value nested deeper than
// MaxToGoDepth, whose message is format with the limit, wrapping
// ErrTooDeep.
func (r *Realm) tooDeep(format string) error {
	o := r.NewError(KindRangeError, format, MaxToGoDepth)
	o.ErrorData().hostErr = ErrTooDeep
	return &Exception{Value: ObjectValue(o)}
}

// ErrFromGoDepth is not returned by FromGo: the lazy conversion converts one
// level per touch, so no depth needs bounding. The variable is kept so
// errors.Is checks against it still compile.
var ErrFromGoDepth = errors.New("engine: FromGo value nesting exceeds depth limit")

// ErrForeign is FromGo's error for a function or generator of another realm
// (IsForeign), a Value or an *Object, which runs only in its own realm; the
// root package returns it as moejs.ErrForeign.
var ErrForeign = errors.New("moejs: function or generator of another runtime")

// hostShapeEntry caches the shape for one sorted set of non-index keys.
type hostShapeEntry struct {
	keys  []string
	shape *Shape
	next  *hostShapeEntry // hash-collision chain
}

// hostShapesMax bounds the key sets in a realm's host shape cache
// (Realm.hostShapes): a runtime fed maps keyed by ids builds a new key set
// per request, and the cache would keep every one with its shapes and names.
// A cache that reaches it is emptied on the next miss; the key sets in use
// come back on their next conversion, and the predictions of the conversion
// in progress (hostHeap.shapes) hold their own entries.
//
// hostShapeKeysMax bounds the keys of those key sets as well
// (hostHeap.shapeKeys): an entry keeps a shape per key, and the lookup
// tables and props of the shapes the conversions touched, about 64 KB for
// 40 keys, so 1,024 entries of wide key sets would hold 64 MiB until the
// cache starts over. 8,192 keys are 1,024 entries of eight keys or 204 of
// forty; hostShapesMax keeps the narrow key sets, whose entries cost more
// than their shapes, to 1,024.
const (
	hostShapesMax    = 1024
	hostShapeKeysMax = 8192
)

// FromGo converts a Go value to a JavaScript value. Scalars
// convert at once; a *big.Int becomes a bigint (a copy; nil is null). The
// JSON-shaped containers (map[string]any, map[string]string,
// map[string][]string, []any, []string, []map[string]any) convert lazily (hostlazy.go): the result is a
// placeholder that reads the Go value on its first touch and converts one
// level then, its children becoming placeholders in turn. Every observable
// result is the one eager conversion produced (maps get their keys sorted
// and share a Shape per key set, slices become dense arrays); the Go value is
// read later, node by node, and must not change while the result is in use.
// A []byte becomes an ArrayBuffer over its bytes (NewArrayBuffer: not a copy,
// so the bytes must not change while JavaScript may read them). A
// json.RawMessage converts as JSONParse of a copy of its text, the text of
// an object or an array on its first touch (hostraw.go). A named type
// whose underlying type is one of these converts as that type (a named map
// or slice lazily), and any other type encoding/json writes (a struct, a
// pointer, a map or slice of other element types, a type with a MarshalJSON
// or MarshalText method) converts from its json.Marshal text, parsed by
// JSONParse: a snapshot of the value (hostconv_types.go). A channel, a
// function that is not a NativeFunc, a complex number, an unsafe.Pointer,
// and without a MarshalJSON or MarshalText method an error and a struct
// none of whose fields json.Marshal writes, are an error, and so is a value
// json.Marshal fails on. A Value or *Object
// that is a function or generator of another realm is ErrForeign, and so is
// one inside a container: there the read of its member throws a TypeError
// with ErrForeign's text, as for a member of a type FromGo does not convert.
func (r *Realm) FromGo(v any) (Value, error) {
	if res, ok := r.hostRoot(v); ok {
		return res, nil
	}
	return r.fromGoOther(v, nil)
}

// fromGoState is the realm's host-conversion state (Realm.fromGo).
type fromGoState struct {
	heap *hostHeap
}

// fromGoOther converts a value that is not a JSON-shaped container: a
// scalar, an engine value, a native function, a *big.Int or a []byte, and
// through fromGoTyped any other type. h is the heap of the container being
// materialized, nil for a top-level FromGo.
func (r *Realm) fromGoOther(v any, h *hostHeap) (Value, error) {
	if res, ok, err := scalarFromGo(v); ok {
		if res.IsObject() && res.AsObject().HoldsCode() && r.IsForeign(res) {
			return Undefined(), ErrForeign
		}
		return res, err
	}
	switch x := v.(type) {
	case string:
		// Charged nothing, as the eager conversion was; a measurement
		// counts a lazy string as decoded.
		return StringValue(FromHostString(x)), nil
	case NativeFunc:
		return ObjectValue(r.NewHostFunction(AtomEmpty, 0, x)), nil
	case func(*Realm, Value, []Value) (Value, error):
		return ObjectValue(r.NewHostFunction(AtomEmpty, 0, NativeFunc(x))), nil
	case *big.Int:
		if x == nil {
			return Null(), nil
		}
		b, ok := NewBigIntFromBig(x)
		if !ok {
			return Undefined(), errBigIntTooBig(r)
		}
		return BigIntValue(b), nil
	}
	if x, ok := v.([]byte); ok {
		o, err := r.NewArrayBuffer(x)
		if err != nil {
			return Undefined(), err
		}
		return ObjectValue(o), nil
	}
	return r.fromGoTyped(v, h)
}

// scalarFromGo converts the Go values whose conversion allocates nothing and
// always gives the same Value (numbers, booleans, nil, engine values); ok is
// false for any other type.
func scalarFromGo(v any) (res Value, ok bool, err error) {
	switch x := v.(type) {
	case nil:
		return Null(), true, nil
	case Value:
		return x, true, nil
	case bool:
		return Bool(x), true, nil
	case float64:
		return NumberValue(x), true, nil
	case float32:
		return NumberValue(float64(x)), true, nil
	case int:
		return IntValue(x), true, nil
	case int64:
		return Int64Value(x), true, nil
	case int32:
		return IntValue(int(x)), true, nil
	case int16:
		return IntValue(int(x)), true, nil
	case int8:
		return IntValue(int(x)), true, nil
	case uint:
		return NumberValue(float64(x)), true, nil
	case uint64:
		return NumberValue(float64(x)), true, nil
	case uint32:
		return NumberValue(float64(x)), true, nil
	case uint16:
		return IntValue(int(x)), true, nil
	case uint8:
		return IntValue(int(x)), true, nil
	case json.Number:
		res, err = jsonNumberValue(x)
		return res, true, err
	case *Object:
		return ObjectValue(x), true, nil
	case *String:
		return StringValue(x), true, nil
	}
	return Value{}, false, nil
}

// hostPair is one map entry collected for materialization.
type hostPair[V any] struct {
	k string
	v V
}

func comparePairs[V any](a, b hostPair[V]) int { return strings.Compare(a.k, b.k) }

// smallPairSort is the longest map orderPairs sorts by insertion, with the
// keys compared in place rather than through comparePairs.
const smallPairSort = 12

// orderPairs puts one map's entries in build order: the named keys sorted
// (the object's shape and enumeration order), then the canonical array
// indices, which are stored as elements (their relative order is
// irrelevant).
func orderPairs[V any](seg []hostPair[V]) {
	if len(seg) <= smallPairSort {
		// A map's keys are distinct, so any sort gives the one order.
		for i := 1; i < len(seg); i++ {
			for j := i; j > 0 && seg[j].k < seg[j-1].k; j-- {
				seg[j], seg[j-1] = seg[j-1], seg[j]
			}
		}
	} else {
		slices.SortFunc(seg, comparePairs[V])
	}
	named := 0
	for i := range seg {
		if _, ok := parseArrayIndex(seg[i].k); ok {
			continue
		}
		if i != named {
			seg[named], seg[i] = seg[i], seg[named]
		}
		named++
	}
}

// Pair scratch sizing: the first map materialized by a realm gets room for
// initialPairs entries (a host globals map or a hook context fits), and a
// scratch grown past maxRetainedPairs is dropped after use.
const (
	initialPairs     = 16
	maxRetainedPairs = 1024
)

func trimScratch[T any](s []T) []T {
	if cap(s) > maxRetainedPairs {
		return nil
	}
	return s[:0]
}

// hostShapeKey interns the host's key k for a new cache entry and returns
// the atom and the key the entry keeps: the atom's content, or a copy of k
// for a non-ASCII name, never k itself, which may share the memory of a
// request body (a zero-copy JSON decoder). It sets *nonASCII for a non-ASCII
// name.
func (r *Realm) hostShapeKey(k string, nonASCII *bool) (*String, string) {
	a := r.InternGoString(k)
	if a.kind != strASCII {
		*nonASCII = true
		return a, bytesToString([]byte(k))
	}
	return a, a.s
}

// hostShadowNames key the Go map entries another entry shadows (shadowHostKeys):
// private names no code holds, which no operation on the object reaches.
var hostShadowNames = func() (names [maxShapeProps - 1]PrivateName) {
	desc := asciiString("#shadowed")
	for i := range names {
		names[i].desc = desc
	}
	return
}()

// shadowHostKeys replaces each key of the sorted host key list that a later
// key equals by one of hostShadowNames and reports whether it replaced any.
// Two Go keys that are not valid UTF-8 can decode to the same atom (each
// invalid byte becomes U+FFFD): the later entry wins, as in JSON.parse, and
// the shape still has a slot for each entry of the map.
func shadowHostKeys(pks []PropertyKey) bool {
	n := 0
	for i, k := range pks {
		if k.IsString() && k.String().kind != strASCII && slices.Contains(pks[i+1:], k) {
			pks[i] = PrivateKey(&hostShadowNames[n])
			n++
		}
	}
	return n > 0
}

// hostShapeFor returns the cache entry for a sorted named-key list, building
// the transition chain on first use.
func hostShapeFor[V any](hh *hostHeap, h uint64, named []hostPair[V]) *hostShapeEntry {
	r := hh.r
	var chains [2]*hostShapeEntry
	if r.plainRoot.shared {
		chains[0] = sharedHostShapeChain(h)
	}
	chains[1] = r.hostShapes[h]
	for _, chain := range chains {
	next:
		for e := chain; e != nil; e = e.next {
			if len(e.keys) != len(named) {
				continue
			}
			for i := range named {
				if e.keys[i] != named[i].k {
					continue next
				}
			}
			return e
		}
	}
	keys := make([]string, len(named))
	var pkBuf [16]PropertyKey
	pks := pkBuf[:0]
	if len(named) > len(pkBuf) {
		pks = make([]PropertyKey, 0, len(named))
	}
	nonASCII := false
	for i := range named {
		a, k := r.hostShapeKey(named[i].k, &nonASCII)
		keys[i] = k
		pks = append(pks, StringKey(a))
	}
	var shape *Shape
	if nonASCII && shadowHostKeys(pks) {
		// The shadowed entries are hidden, and private names are never
		// published, so the chain leaves the shared tree at the first.
		shape = r.plainRoot
		for _, k := range pks {
			attrs := uint8(attrDefault)
			if k.IsPrivate() {
				attrs = 0
			}
			shape = shape.addProperty(r, k, attrs)
		}
	} else {
		shape = r.plainRoot.addChain(r, pks, attrDefault)
	}
	if shape.shared {
		if e := publishHostShape(h, keys, shape); e != nil {
			return e
		}
	}
	if r.hostShapes == nil {
		r.hostShapes = make(map[uint64]*hostShapeEntry)
	} else if len(r.hostShapes) >= hostShapesMax || int(hh.shapeKeys)+len(keys) > hostShapeKeysMax {
		clear(r.hostShapes)
		hh.shapeKeys = 0
	}
	hh.shapeKeys += int32(len(keys))
	e := &hostShapeEntry{keys: keys, shape: shape, next: r.hostShapes[h]}
	r.hostShapes[h] = e
	return e
}

func jsonNumberValue(n json.Number) (Value, error) {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return Int64Value(i), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
			return NumberValue(f), nil
		}
		return Undefined(), fmt.Errorf("engine: FromGo: invalid json.Number %q", s)
	}
	return NumberValue(f), nil
}

// ToGo exports a JavaScript value to Go: integral numbers within int64 range
// become int64 (except -0, which stays float64 so the sign survives), other
// numbers float64; null and undefined become nil; arrays become []any; other
// objects become map[string]any of their own enumerable string keys;
// function objects are exported as the *Object itself;
// Number/String/Boolean/BigInt wrappers export their primitive; Date objects
// export time.Time in the realm's time zone (the zero time for an invalid
// date); bigint exports *big.Int; symbols export the *Symbol. An ArrayBuffer
// or SharedArrayBuffer exports a copy of its bytes as a []byte, and a typed
// array or DataView a copy of the bytes of its buffer it views (nil for a
// detached buffer or a view out of its buffer's bounds, non-nil and empty
// for a zero-length one). A proxy exports through its traps as what it
// stands for: a callable proxy like a function (the *Object), one whose
// target is an array as []any (length and elements read with the get trap),
// any other as the map of Object.keys through ownKeys,
// getOwnPropertyDescriptor and get; a revoked proxy is an error. Cycles
// yield the already-exported container (a proxy array only once complete).
// Getters and traps that throw yield nil; use ToGoStrict to observe the
// error.
func (r *Realm) ToGo(v Value) any {
	if !v.IsObject() {
		out, _ := r.toGo(v, nil) // a primitive needs no state
		return out
	}
	var st toGoState
	out, _ := r.toGo(v, &st)
	return out
}

// ToGoStrict is ToGo that stops at the first error raised while reading a
// property (a throwing getter, or an interrupt observed while one runs) and
// returns it instead of a partial result.
func (r *Realm) ToGoStrict(v Value) (any, error) {
	if !v.IsObject() {
		return r.toGo(v, nil) // a primitive needs no state
	}
	st := toGoState{strict: true}
	return r.toGo(v, &st)
}

// toGoState is the scratch of one export: the containers exported so far
// (for cycles and shared references) live in a small array on the caller's
// stack and spill into a map only for wide graphs, so a typical hook result
// costs its Go containers and nothing else.
type toGoState struct {
	strict bool
	depth  int // containers being exported (MaxToGoDepth)
	n      int
	objs   [toGoSmall]*Object
	outs   [toGoSmall]any
	seen   map[*Object]any
}

// toGoSmall is how many containers an export tracks in toGoState's arrays
// before it builds the map: a hook result of a few dozen objects and
// arrays costs no map.
const toGoSmall = 32

func (st *toGoState) lookup(o *Object) (any, bool) {
	for i := range st.n {
		if st.objs[i] == o {
			return st.outs[i], true
		}
	}
	if st.seen == nil {
		return nil, false
	}
	out, ok := st.seen[o]
	return out, ok
}

func (st *toGoState) add(o *Object, out any) {
	if st.n < len(st.objs) {
		st.objs[st.n], st.outs[st.n] = o, out
		st.n++
		return
	}
	if st.seen == nil {
		st.seen = make(map[*Object]any, 16)
	}
	st.seen[o] = out
}

func (r *Realm) toGo(v Value, st *toGoState) (any, error) {
	switch v.Type() {
	case TypeUndefined, TypeNull, TypeHole:
		return nil, nil
	case TypeBoolean:
		return v.AsBool(), nil
	case TypeNumber:
		f := v.AsNumber()
		if f == math.Trunc(f) && f >= -9223372036854775808 && f < 9223372036854775808 && (f != 0 || !math.Signbit(f)) {
			return int64(f), nil
		}
		return f, nil
	case TypeString:
		return v.AsString().GoString(), nil
	case TypeBigInt:
		return v.AsBigInt().Big(), nil
	case TypeSymbol:
		return v.AsSymbol(), nil
	}
	o := v.AsObject()
	if o.flags&flagHostNode != 0 {
		if src, ok := o.HostValue(); ok {
			return src, nil // unmodified: the Go value it was converted from
		}
	}
	if pv, ok := o.PrimitiveValue(); ok {
		return r.toGo(pv, st)
	}
	if o.IsCallable() {
		return o, nil
	}
	if tv, ok := o.DateValue(); ok {
		if tv != tv {
			return time.Time{}, nil
		}
		return r.DateTime(tv), nil
	}
	if prev, ok := st.lookup(o); ok {
		return prev, nil
	}
	if st.depth >= MaxToGoDepth {
		if st.strict {
			return nil, r.tooDeep("Nesting is too deep to export to Go (more than %d levels)")
		}
		return nil, nil
	}
	st.depth++
	out, err := r.toGoContainer(o, v, st)
	st.depth--
	return out, err
}

// collectionToGo exports a Map as [][2]any of its entries and a Set as []any
// of its elements: the length is the size when the export starts, and
// entries a getter deletes meanwhile leave zero items.
func (r *Realm) collectionToGo(o *Object, c *collection, st *toGoState) (any, error) {
	n := c.size()
	cur := c.cursor()
	if o.class == ClassSet {
		items := make([]any, n)
		var out any = items
		st.add(o, out)
		for i := range n {
			k, _, ok := cur.next()
			if !ok {
				break
			}
			var err error
			if items[i], err = r.toGo(k, st); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	items := make([][2]any, n)
	var out any = items
	st.add(o, out)
	for i := range n {
		k, v, ok := cur.next()
		if !ok {
			break
		}
		var err error
		if items[i][0], err = r.toGo(k, st); err != nil {
			return nil, err
		}
		if items[i][1], err = r.toGo(v, st); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// toGoContainer exports an array or a plain object (st.depth counts it).
func (r *Realm) toGoContainer(o *Object, v Value, st *toGoState) (any, error) {
	if o.class == ClassArray {
		n := int(o.ArrayLength())
		items := make([]any, n)
		var out any = items // boxed once: stored for cycle detection and returned
		st.add(o, out)
		for i := range n {
			ev, err := o.GetIndex(r, uint32(i))
			if err != nil {
				if st.strict {
					return nil, err
				}
				ev = Undefined()
			}
			if items[i], err = r.toGo(ev, st); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if c, ok := dataOk(o, kindCollection); ok {
		return r.collectionToGo(o, c, st)
	}
	if o.class == ClassProxy {
		return r.proxyToGo(o, v, st)
	}
	if hasOnlyShapeProps(o) {
		// The shape's property list is the key snapshot; slots are read
		// directly while the shape is unchanged and through [[Get]] once a
		// getter has run user code that may have mutated the object.
		shape := o.shape
		props := shape.Props()
		n := 0
		for i := range props {
			if props[i].attrs&attrEnumerable != 0 && props[i].key.IsString() {
				n++
			}
		}
		out := make(map[string]any, n)
		st.add(o, out)
		for i := range props {
			p := &props[i]
			if p.attrs&attrEnumerable == 0 || !p.key.IsString() {
				continue
			}
			var ev Value
			if o.shape == shape && p.attrs&attrAccessor == 0 {
				ev = o.slots[i]
			} else {
				var err error
				if ev, err = o.Get(r, p.key, v); err != nil {
					if st.strict {
						return nil, err
					}
					ev = Undefined()
				}
			}
			var err error
			if out[p.key.GoString()], err = r.toGo(ev, st); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if data, ok := o.BufferData(); ok {
		out := any(slices.Clone(data))
		st.add(o, out)
		return out, nil
	}
	keys, err := r.enumerableOwnKeys(o) // bounds a typed array's
	if err != nil {
		if st.strict {
			return nil, err
		}
		return nil, nil
	}
	out := make(map[string]any, len(keys))
	st.add(o, out)
	for _, k := range keys {
		ev, err := o.Get(r, k, v)
		if err != nil {
			if st.strict {
				return nil, err
			}
			ev = Undefined()
		}
		if out[k.GoString()], err = r.toGo(ev, st); err != nil {
			return nil, err
		}
	}
	return out, nil
}

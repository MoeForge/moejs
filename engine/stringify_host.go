package engine

import (
	"slices"
	"unicode/utf8"
)

// JSON text of host placeholders (hostlazy.go) straight from their Go
// values. A placeholder JSON.stringify or AppendJSON reaches untouched (an
// argument the hook forwards without reading, such as a request body's
// payload) is written from its Go map or slice without being materialized:
// no objects, shapes or strings are made for it. The output is the
// materialized object's: maps in materialization order (the keys sorted),
// the same numbers and the same string escapes; and since a walk runs no
// JavaScript, it may skip the toJSON lookups when no toJSON can be found:
// none on Object.prototype and Array.prototype (the prototypes of every
// container it converts) and no "toJSON" key in a map. Whatever the walk
// does not handle (a key that is not plain ASCII or is an array index, a
// "toJSON" key, a Go type other than JSON's, nesting at jsonScanDepth, a
// replacer, a property list or an indentation) makes it give up the
// placeholder: its output is cut off and the placeholder is materialized and
// serialized as before.

// protosLackToJSON reports whether Object.prototype and Array.prototype
// have no toJSON. Those of a shared realm are the frozen template's, which
// has none and never gets one; in a mutable realm a positive answer is kept
// in *epoch for the prototype epoch it was found in (adding a property to a
// prototype bumps it).
func (r *Realm) protosLackToJSON(epoch *uint32) bool {
	op, ap := r.ObjectPrototype, r.ArrayPrototype
	if op.flags&ap.flags&flagShared != 0 {
		return true
	}
	if ap.Proto() != op {
		return false
	}
	if *epoch == r.protoEpoch+1 {
		return true
	}
	key := StringKey(AtomToJSON)
	if _, ok := op.getOwnCell(key); ok {
		return false
	}
	if _, ok := ap.getOwnCell(key); ok {
		return false
	}
	*epoch = r.protoEpoch + 1
	return true
}

// lacksToJSON reports whether o has no toJSON, found without a lookup that
// could run code or materialize o: o is an ordinary object or array, or a
// host placeholder, with the realm's Object.prototype or Array.prototype as
// its prototype, neither of which has a toJSON, and o has none of its own
// (in its shape or dictionary, or in a placeholder's map: a "toJSON" of a
// map of strings or of string lists is never callable).
func (r *Realm) lacksToJSON(o *Object, epoch *uint32) bool {
	if o.Proto() != r.ObjectPrototype && o.Proto() != r.ArrayPrototype {
		return false
	}
	if o.hasLazy() {
		if o.shape.key != hostSentinelKey || !r.protosLackToJSON(epoch) {
			return false
		}
		if m, ok := o.payload().(map[string]any); ok {
			if _, has := m["toJSON"]; has {
				return false
			}
		}
		return true
	}
	if o.class != ClassObject && o.class != ClassArray || !r.protosLackToJSON(epoch) {
		return false
	}
	if o.flags&flagDict != 0 {
		_, has := o.dictTable().lookup(StringKey(AtomToJSON))
		return !has
	}
	_, _, has := o.shape.Lookup(StringKey(AtomToJSON))
	return !has
}

// hostNode writes the placeholder o from its Go value and reports whether
// it did; when it did not, nothing was written and the caller serializes o
// as an object.
func (js *jsonStringifier) hostNode(o *Object) (bool, error) {
	if o.shape.key != hostSentinelKey || js.replacerFn.IsObject() || js.hasList || js.gap != nil || js.strict || !js.r.protosLackToJSON(&js.plainEpoch) {
		return false, nil
	}
	mark := js.sb.Len()
	ok, err := js.hostValue(hostHeapOf(o.shape), o.hostSrc(), js.depth)
	if !ok && err == nil {
		js.sb.truncate(mark)
	}
	return ok, err
}

// hostValue writes the Go value v of a container at depth containers below
// the stringifier's stack (as push would count them); ok is false when the
// walk gives up.
func (js *jsonStringifier) hostValue(h *hostHeap, v any, depth int) (ok bool, err error) {
	if err := js.tick(); err != nil {
		return false, err
	}
	switch x := v.(type) {
	case string:
		err = js.quoteGo(x)
	case map[string]any:
		if x == nil {
			js.sb.writeASCIIBytes("null")
			break
		}
		return hostMap(js, h, x, depth, &h.anyPairs, true)
	case []any:
		if depth >= jsonScanDepth {
			return false, nil
		}
		js.sb.WriteASCII('[')
		for i, e := range x {
			if i > 0 {
				js.sb.WriteASCII(',')
			}
			if ok, err := js.hostValue(h, e, depth+1); !ok || err != nil {
				return ok, err
			}
		}
		js.sb.WriteASCII(']')
	case float64:
		js.number(x)
	case bool:
		if x {
			js.sb.writeASCIIBytes("true")
		} else {
			js.sb.writeASCIIBytes("false")
		}
	case nil:
		js.sb.writeASCIIBytes("null")
	case map[string]string:
		if x == nil {
			js.sb.writeASCIIBytes("null")
			break
		}
		return hostMap(js, h, x, depth, &h.strPairs, false)
	case []string:
		if depth >= jsonScanDepth {
			return false, nil
		}
		return hostStrings(js, x)
	case map[string][]string:
		if x == nil {
			js.sb.writeASCIIBytes("null")
			break
		}
		return hostMap(js, h, x, depth, &h.listPairs, false)
	case []map[string]any:
		if depth >= jsonScanDepth {
			return false, nil
		}
		js.sb.WriteASCII('[')
		for i, m := range x {
			if i > 0 {
				js.sb.WriteASCII(',')
			}
			if ok, err := js.hostValue(h, m, depth+1); !ok || err != nil {
				return ok, err
			}
		}
		js.sb.WriteASCII(']')
	case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8, float32:
		n, _, _ := scalarFromGo(x)
		js.number(n.AsNumber())
	default:
		return false, nil // Value, *Object, functions, []byte, json.Number, ...
	}
	if err != nil {
		return false, err
	}
	return true, js.sb.checkLength(js.r)
}

func hostString(js *jsonStringifier, _ *hostHeap, s string, _ int) (bool, error) {
	if err := js.tick(); err != nil {
		return false, err
	}
	if err := js.quoteGo(s); err != nil {
		return false, err
	}
	return true, js.sb.checkLength(js.r) // each value: the walk stops at the first past the limit
}

// hostStringList writes a map[string][]string value: a nil list is an
// empty array, as stringArray makes it.
func hostStringList(js *jsonStringifier, _ *hostHeap, list []string, depth int) (bool, error) {
	if err := js.tick(); err != nil {
		return false, err
	}
	if depth >= jsonScanDepth {
		return false, nil
	}
	return hostStrings(js, list)
}

func hostStrings(js *jsonStringifier, list []string) (bool, error) {
	js.sb.WriteASCII('[')
	for i, s := range list {
		if i > 0 {
			js.sb.WriteASCII(',')
		}
		if err := js.tick(); err != nil {
			return false, err
		}
		if err := js.quoteGo(s); err != nil {
			return false, err
		}
		if err := js.sb.checkLength(js.r); err != nil { // each string, as hostValue checks each value
			return false, err
		}
	}
	js.sb.WriteASCII(']')
	return true, js.sb.checkLength(js.r)
}

// hostPairValue writes the value of a host map's entry with the function
// for the map's value type. It switches on the type rather than taking a
// function value: through an indirect call the stringifier would escape to
// the heap, one allocation per stringify.
func hostPairValue[V any](js *jsonStringifier, h *hostHeap, v *V, depth int) (bool, error) {
	switch x := any(v).(type) {
	case *any:
		return js.hostValue(h, *x, depth)
	case *string:
		return hostString(js, h, *x, depth)
	case *[]string:
		return hostStringList(js, h, *x, depth)
	}
	panic("engine: hostPairValue of an unexpected map value type")
}

// hostMap writes the Go map m with its keys sorted, as materializeMap lays
// them out when none is an array index or not ASCII; a "toJSON" entry stops
// the walk when its value could be callable (anyValues). The entries are
// collected on the end of the heap's pair scratch, which nothing else uses
// during a walk (it materializes nothing), so nested maps stack their
// entries after the outer map's.
func hostMap[V any](js *jsonStringifier, h *hostHeap, m map[string]V, depth int, scratch *[]hostPair[V], anyValues bool) (ok bool, err error) {
	if depth >= jsonScanDepth {
		return false, nil
	}
	if len(m) == 0 {
		js.sb.writeASCIIBytes("{}")
		return true, nil
	}
	base := len(*scratch)
	pairs := *scratch
	for k, v := range m {
		if _, index := parseArrayIndex(k); index || anyValues && k == "toJSON" || !isASCII(k) {
			clear(pairs[base:])
			*scratch = trimScratch(pairs[:base])
			return false, nil
		}
		pairs = append(pairs, hostPair[V]{k, v})
	}
	*scratch = pairs
	seg := pairs[base:]
	slices.SortFunc(seg, comparePairs[V])
	js.sb.WriteASCII('{')
	for i := range seg {
		if i > 0 {
			js.sb.WriteASCII(',')
		}
		js.sb.WriteASCII('"')
		if err = js.quoteASCII(seg[i].k); err != nil {
			break
		}
		js.sb.writeASCIIBytes(`":`)
		if ok, err = hostPairValue(js, h, &seg[i].v, depth+1); !ok || err != nil {
			break
		}
	}
	// A nested map may have moved the scratch to a larger array: keep it, so
	// that the next map does not grow it again. Each map reads its entries
	// from its own pairs, never from the scratch, so taking the newer array
	// only needs base within its length: a nested map that gave up left the
	// scratch at length 0 (trimScratch), and this map, giving up too, keeps
	// its own array then.
	if s := *scratch; cap(s) > cap(pairs) && len(s) >= base {
		pairs = s
	}
	clear(pairs[base:])
	pairs = pairs[:base]
	if base == 0 {
		pairs = trimScratch(pairs)
	}
	*scratch = pairs
	if !ok || err != nil {
		return false, err
	}
	js.sb.WriteASCII('}')
	return true, js.sb.checkLength(js.r) // the keys and the braces
}

// quoteGo quotes the Go string g as quote quotes FromGoString(g). An ASCII
// string, and for AppendJSON valid UTF-8 (whose bytes it writes as they
// are), is quoted in place; one without a byte to escape is copied after a
// single scan. Its callers check the length limit after the string
// (hostValue after each value, hostString and hostStrings after each
// string), as write checks it after quote: the walk of a host map or list
// of strings stops at the first string past the limit, which tick,
// counting strings rather than bytes, does not see.
func (js *jsonStringifier) quoteGo(g string) error {
	i := jsonScan(g, 0, swarMSB)
	plain := i == len(g)
	if !plain && (g[i] >= 0x80 || !isASCII(g[i:])) && !(js.raw && utf8.ValidString(g)) {
		return js.quote(FromGoString(g))
	}
	js.sb.WriteASCII('"')
	if plain {
		writeASCIIString(&js.sb, g)
	} else if err := js.quoteASCII(g); err != nil {
		return err
	}
	js.sb.WriteASCII('"')
	return nil
}

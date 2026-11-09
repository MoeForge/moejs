package engine

import (
	"encoding/json"
	"unsafe"
)

// json.RawMessage in FromGo. A RawMessage is JSON text the host already
// holds (a stored task state, a request body); FromGo converts it to what
// JSON.parse gives for it, parsed by the engine's parser, never by
// encoding/json.
//
// The conversion copies the text first (one string conversion, a memmove)
// and works on the copy only: the host may reuse its buffer as soon as the
// RawMessage is converted, which is at once for a top-level FromGo and, for
// one inside a Go map or slice, when that container is materialized. A text
// that is an object or an array becomes a host placeholder (hostlazy.go) of
// its own, which holds the copy as a rawText and parses it, in place, on
// its first touch, so a hook that never reads the value never pays for its
// parse. A materialization cannot fail, so the copy is checked when the
// placeholder is made (jsonCheck: one scan, no allocation, the bytes of
// strings skipped eight at a time), and a text the check rejects is parsed
// at once, which gives the parser's SyntaxError: FromGo's error at the top
// level, the error thrown where the member is read inside a container
// (cellOf). The check also counts an array's elements: an array
// placeholder has its length before it is materialized. Since the copy is
// the engine's own and never changes, the parse of a checked copy cannot
// fail.
//
// Any other text (a string, a number, true, false, null) is parsed at once,
// as is a text the check rejects; a nil RawMessage is null, as json.Marshal
// writes it, and an empty one is the parser's SyntaxError, as it is
// json.Marshal's error. The strings of a parse share the copy, never the
// host's buffer.
//
// An untouched placeholder exports (HostValue, ToGo) a new json.RawMessage
// with the text, which the host may keep and change; once materialized it is
// an ordinary object or array, exported from its JavaScript state like any
// other.

// rawText is the engine's copy of a json.RawMessage's text that a
// placeholder holds (in internal for an object, in its slots, as a boxed
// value, for an array): a string, so that nothing can change it.
type rawText string

// fromRawJSON converts the RawMessage raw; h is the heap of the container
// being materialized, nil for a top-level FromGo.
func (r *Realm) fromRawJSON(raw json.RawMessage, h *hostHeap) (Value, error) {
	if raw == nil {
		return Null(), nil
	}
	s := string(raw) // the copy: checked, kept and parsed
	i := jsonSkipSpace(s, 0)
	if i < len(s) && (s[i] == '{' || s[i] == '[') && len(s) <= maxStringLength {
		if n, ok := jsonCheck(s, i); ok {
			top := h == nil
			if top {
				h = r.hostHeap()
				if h.touched && r.callDepth == 0 {
					h.seal()
				}
			}
			if s[i] == '{' {
				switch {
				case n == 0 && top:
					return ObjectValue(r.NewObject()), nil
				case n == 0:
					return h.emptyObject(), nil
				case top:
					return ObjectValue(h.mapNode(h.rootObject(), kindRawText, rawText(s))), nil
				}
				return ObjectValue(h.mapNode(h.object(), kindRawText, rawText(s))), nil
			}
			var a *arrayObject
			if top {
				a = h.rootArray()
			} else {
				a = h.array()
			}
			if n == 0 {
				return ObjectValue(r.initArray(a, nil, 0)), nil
			}
			return ObjectValue(h.sliceNode(a, rawText(s), hostSliceRaw, n)), nil
		}
	}
	return r.parseQuiet(s)
}

// rawUnparsable is the panic of a materialization whose text the parser
// rejects although jsonCheck accepted it: an engine bug (FuzzJSONCheck
// looks for one), never a host's doing, since the text is the engine's own
// copy.
const rawUnparsable = "moejs: internal error: a checked json.RawMessage text does not parse"

// rawTextOf is the text of the RawMessage placeholder o.
func rawTextOf(o *Object) rawText {
	if o.class == ClassArray {
		b, _ := boxedSliceIn(o.slots)
		return *(*rawText)(b.p)
	}
	return o.payload().(rawText)
}

// unreadRawText is the text of v when v is a RawMessage placeholder
// JavaScript has not read.
func unreadRawText(v Value) (rawText, bool) {
	if !v.IsObject() {
		return "", false
	}
	o := v.AsObject()
	if o.flags&flagHostNode == 0 || o.shape.key != hostSentinelKey {
		return "", false
	}
	t, ok := o.hostSrc().(rawText)
	return t, ok
}

// rawBytes is a read-only view of t's bytes, for a reader that copies what
// it keeps (json.Marshal, json.Valid): never written to.
func rawBytes(t rawText) []byte { return unsafe.Slice(unsafe.StringData(string(t)), len(t)) }

// materializeRaw materializes the placeholder o of a RawMessage: the object
// or array JSON.parse gives for its text, laid out in o (its prototype kept,
// as materializeMap keeps it). o is an ordinary object or array afterwards,
// no longer a host node.
func (h *hostHeap) materializeRaw(o *Object) {
	r := h.r
	text := rawTextOf(o)
	o.flags &^= flagHostNode
	v, err := r.parseQuiet(string(text)) // the strings of the result share the copy
	if err != nil || !v.IsObject() {
		panic(rawUnparsable)
	}
	if o.class == ClassArray {
		o.slots = nil
		o.setHostSliceKind(0)
		o.setArrayLen(v.AsObject().ArrayLength()) // the check counted it already
		o.setElems(v.AsObject().elems())
		return
	}
	o.clearPayload()
	root := r.rootShapeFor(o.Proto())
	o.shape = root
	src := v.AsObject()
	o.setElems(src.elems())
	if src.flags&flagDict != 0 {
		o.shape = r.dictShapeFor(root.proto)
		o.flags |= flagDict
	} else {
		o.slots = src.slots
		o.shape = src.shape
		if root != r.plainRoot {
			o.shape = src.shape.rebase(r, root)
		}
	}
	if d := src.dictTable(); d != nil {
		// Keep what o's own dictionary holds besides properties (its weak
		// entries, the root of the objects inheriting from it).
		od := o.ensureDict()
		od.index, od.entries, od.sparse = d.index, d.entries, d.sparse
	}
}

// jsonValidRaw reports whether the parser accepts raw.
func jsonValidRaw(raw []byte) bool {
	s := bytesToString(raw)
	i := jsonSkipSpace(s, 0)
	if i < len(s) && (s[i] == '{' || s[i] == '[') {
		_, ok := jsonCheck(s, i)
		return ok
	}
	return json.Valid(raw) // a string, a number or a literal: the same grammar
}

// jsonSkipSpace returns the index of the first byte of s at or after i that
// is not JSON white space.
func jsonSkipSpace(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// jsonCheck reports whether the parser accepts s, whose first byte that is
// not white space, at i, is '{' or '[': one value, nested at most
// jsonMaxDepth arrays and objects deep, with only white space after it. n is
// the number of members or elements of that outermost object or array. Its
// strings may hold bytes that are not valid UTF-8, which the parser reads as
// U+FFFD; anywhere else a byte >= 0x80 is an error, as it is the parser's.
func jsonCheck(s string, i int) (n int, ok bool) {
	var arrays [(jsonMaxDepth + 63) / 64]uint64 // the containers open, a bit set for an array
	depth := 0
	for {
		// A value starts at i.
		i = jsonSkipSpace(s, i)
		if i >= len(s) {
			return 0, false
		}
		switch c := s[i]; c {
		case '{', '[':
			if depth == jsonMaxDepth {
				return 0, false
			}
			bit := uint64(1) << (depth & 63)
			if c == '[' {
				arrays[depth>>6] |= bit
			} else {
				arrays[depth>>6] &^= bit
			}
			depth++
			i = jsonSkipSpace(s, i+1)
			if i < len(s) && s[i] == c+2 { // '}' or ']'
				i++
				depth--
				break // an empty container: a value that ended
			}
			if c == '{' {
				if i, ok = jsonCheckKey(s, i); !ok {
					return 0, false
				}
			}
			continue // the first member's value
		case '"':
			if i, ok = jsonCheckString(s, i); !ok {
				return 0, false
			}
		case 't':
			if i, ok = jsonCheckLiteral(s, i, "true"); !ok {
				return 0, false
			}
		case 'f':
			if i, ok = jsonCheckLiteral(s, i, "false"); !ok {
				return 0, false
			}
		case 'n':
			if i, ok = jsonCheckLiteral(s, i, "null"); !ok {
				return 0, false
			}
		default:
			if i, ok = jsonCheckNumber(s, i); !ok {
				return 0, false
			}
		}
		// A value ended at i: the separators and closing brackets after it.
		for {
			if depth == 0 {
				return n, jsonSkipSpace(s, i) == len(s)
			}
			if depth == 1 {
				n++
			}
			i = jsonSkipSpace(s, i)
			if i >= len(s) {
				return 0, false
			}
			array := arrays[(depth-1)>>6]&(1<<((depth-1)&63)) != 0
			if c := s[i]; c == ',' {
				i++
				if !array {
					if i, ok = jsonCheckKey(s, jsonSkipSpace(s, i)); !ok {
						return 0, false
					}
				}
				break // the next value
			} else if array && c == ']' || !array && c == '}' {
				i++
				depth--
				continue
			}
			return 0, false
		}
	}
}

// jsonCheckKey checks a member's key and the colon after it, from i; it
// returns the index after the colon.
func jsonCheckKey(s string, i int) (int, bool) {
	if i >= len(s) || s[i] != '"' {
		return 0, false
	}
	i, ok := jsonCheckString(s, i)
	if !ok {
		return 0, false
	}
	i = jsonSkipSpace(s, i)
	if i >= len(s) || s[i] != ':' {
		return 0, false
	}
	return i + 1, true
}

// jsonCheckString checks the string literal at i (its opening quote) and
// returns the index after its closing quote.
func jsonCheckString(s string, i int) (int, bool) {
	for i = jsonScan(s, i+1, 0); i < len(s); i = jsonScan(s, i, 0) {
		switch c := s[i]; {
		case c == '"':
			return i + 1, true
		case c < 0x20:
			return 0, false
		}
		// A backslash.
		if i+1 >= len(s) {
			return 0, false
		}
		switch s[i+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i += 2
		case 'u':
			if i+5 >= len(s) {
				return 0, false
			}
			for k := i + 2; k <= i+5; k++ {
				if d := digitValue(s[k]); d < 0 || d >= 16 {
					return 0, false
				}
			}
			i += 6
		default:
			return 0, false
		}
	}
	return 0, false
}

func jsonCheckLiteral(s string, i int, lit string) (int, bool) {
	if len(s)-i < len(lit) || s[i:i+len(lit)] != lit {
		return 0, false
	}
	return i + len(lit), true
}

// jsonCheckNumber checks the number at i as jsonParser.number reads it and
// returns the index after it.
func jsonCheckNumber(s string, i int) (int, bool) {
	if s[i] == '-' {
		i++
	}
	switch {
	case i >= len(s):
		return 0, false
	case s[i] == '0':
		i++
	case s[i] >= '1' && s[i] <= '9':
		i = jsonDigits(s, i)
	default:
		return 0, false
	}
	if i < len(s) && s[i] == '.' {
		j := jsonDigits(s, i+1)
		if j == i+1 {
			return 0, false
		}
		i = j
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		j := jsonDigits(s, i)
		if j == i {
			return 0, false
		}
		i = j
	}
	return i, true
}

func jsonDigits(s string, i int) int {
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

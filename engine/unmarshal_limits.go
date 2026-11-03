package engine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounded decodes. A host that decodes a hook's result into Go (Unmarshal,
// ToGoInto, ToGo) can bound what the decode makes before it makes anything:
// DecodeOptions limits the bytes and the values of the decode's JSON text,
// the text the round trip writes (AppendJSON's for Unmarshal, json.Marshal's
// of ToGo's value for ToGoInto and ToGo), whether or not the decode writes
// it. Both are exact: a value whose text is n bytes passes MaxBytes n and
// fails MaxBytes n-1, by whichever path the decode takes.
//
// MeasureUnmarshal and MeasureToGo count them on the JavaScript value before
// the decode, for
// what can be read without running JavaScript: the values the direct walks
// of Unmarshal and ToGoInto handle (unmarshal.go), host placeholders
// included, whose Go values it reads as the stringifier and json.Marshal
// write them. It mirrors the writers byte for byte (each string's escapes,
// each number's digits), and stops at the first limit passed. For anything
// else (a toJSON, a getter, a proxy, a Date, a Map, a json.RawMessage inside
// a Go map) it gives no verdict, and the host checks the text once the
// round trip has written it (MeasureDecodeText), before it is decoded, or,
// for ToGo, the Go value ToGo returned (MeasureDecodeGo). The limits cost
// nothing to a decode that sets none.

// DecodeOptions bounds a decode of a JavaScript value into Go: a field that
// is zero (or negative) sets no limit.
type DecodeOptions struct {
	// MaxBytes bounds the length in bytes of the decode's JSON text:
	// AppendJSON's text of the value for Unmarshal, json.Marshal's text of
	// ToGo's value for ToGoInto and ToGo.
	MaxBytes int
	// MaxNodes bounds the number of values in that text: each object,
	// array, string, number, true, false and null counts one; keys and
	// members AppendJSON leaves out do not count.
	MaxNodes int
}

// ErrTooLarge is the error, wrapped with the limit it passed, of a decode
// whose JSON text passes one of its DecodeOptions limits; a value nested
// deeper than 10,000 arrays and objects, a cycle included, passes them all.
var ErrTooLarge = errors.New("moejs: decoded value too large")

// decodeMeasure counts the bytes and values of a decode's text.
type decodeMeasure struct {
	r          *Realm
	togo       bool // json.Marshal's text of ToGo's value, else AppendJSON's
	bytes      int64
	nodes      int64
	maxBytes   int64
	maxNodes   int64
	left       int // values until the next interrupt check (Unmarshal)
	err        error
	plainEpoch uint32
}

func newDecodeMeasure(r *Realm, opts DecodeOptions, togo bool) decodeMeasure {
	m := decodeMeasure{r: r, togo: togo, maxBytes: math.MaxInt64, maxNodes: math.MaxInt64, left: 4096}
	if opts.MaxBytes > 0 {
		m.maxBytes = int64(opts.MaxBytes)
	}
	if opts.MaxNodes > 0 {
		m.maxNodes = int64(opts.MaxNodes)
	}
	return m
}

// add counts b bytes and n values; it is false once the text passes a
// limit (err is set) or an interrupt is pending for a measure of
// Unmarshal's text, which observes one every 4096 values, as AppendJSON
// does.
func (m *decodeMeasure) add(b, n int64) bool {
	m.bytes += b
	m.nodes += n
	switch {
	case m.bytes > m.maxBytes:
		m.err = fmt.Errorf("%w: more than %d bytes of JSON", ErrTooLarge, m.maxBytes)
		return false
	case m.nodes > m.maxNodes:
		m.err = fmt.Errorf("%w: more than %d JSON values", ErrTooLarge, m.maxNodes)
		return false
	}
	if !m.togo && n > 0 {
		if m.left -= int(n); m.left <= 0 {
			m.left = 4096
			if m.err = m.r.CheckInterrupt(); m.err != nil {
				return false
			}
		}
	}
	return true
}

// deep is the verdict on a value nested past jsonMaxDepth: its text, a
// cycle's included, passes every limit.
func (m *decodeMeasure) deep() bool {
	m.err = fmt.Errorf("%w: nested deeper than %d arrays and objects", ErrTooLarge, jsonMaxDepth)
	return false
}

// MeasureUnmarshal checks AppendJSON's text of v, the text of a decode by
// Unmarshal, against opts before the decode. err wraps ErrTooLarge when the
// text passes a limit, or is an interrupt, observed every 4096 values as
// AppendJSON observes it. measured reports whether the whole text was
// counted, within the limits; when it is false and err is nil, part of v can
// only be read by running JavaScript, and the host checks the text once it
// is written (MeasureDecodeText). No JavaScript runs, and nothing is written;
// a json.RawMessage FromGo converted and JavaScript did not read is parsed
// to be counted, unless a lower bound of its text, from one scan, already
// passes a limit.
func (r *Realm) MeasureUnmarshal(v Value, opts DecodeOptions) (measured bool, err error) {
	m := newDecodeMeasure(r, opts, false)
	ok := m.js(v, 0)
	return ok, m.err
}

// MeasureToGo checks json.Marshal's text of what ToGo gives for v, the text
// of a decode by ToGoInto or ToGo, against opts before the decode, as
// MeasureUnmarshal does; it observes no interrupt, as ToGo observes none for
// such a value, and when measured is false and err is nil the host checks
// ToGo's value (MeasureDecodeGo).
func (r *Realm) MeasureToGo(v Value, opts DecodeOptions) (measured bool, err error) {
	m := newDecodeMeasure(r, opts, true)
	ok := m.js(v, 0)
	return ok, m.err
}

// MeasureDecodeText checks text, a JSON text written by AppendJSON or
// json.Marshal, against opts: its length and the values in it.
func MeasureDecodeText(text []byte, opts DecodeOptions) error {
	m := newDecodeMeasure(nil, opts, true)
	if !m.add(int64(len(text)), 0) {
		return m.err
	}
	_, n := jsonTextCount(bytesToString(text), false)
	m.add(0, n)
	return m.err
}

// MeasureDecodeGo checks json.Marshal's text of g, a value ToGo returned,
// against opts, without writing it: a NaN or an infinity, which json.Marshal
// fails on, counts as null, and a value json.Marshal cannot write counts as
// null too.
func MeasureDecodeGo(g any, opts DecodeOptions) error {
	m := newDecodeMeasure(nil, opts, true)
	m.goJSON(g, 0)
	return m.err
}

// js counts the text of v.
func (m *decodeMeasure) js(v Value, depth int) bool {
	switch v.Type() {
	case TypeUndefined, TypeNull:
		return m.add(4, 1) // null (an undefined member is the caller's)
	case TypeBoolean:
		if v.AsBool() {
			return m.add(4, 1)
		}
		return m.add(5, 1)
	case TypeNumber:
		if m.togo {
			return m.add(togoNumberLen(v.AsNumber()), 1)
		}
		return m.add(jsNumberLen(v.AsNumber()), 1)
	case TypeString:
		if m.togo {
			return m.add(goQuotedLenJS(v.AsString()), 1)
		}
		return m.add(jsQuotedLen(v.AsString()), 1)
	case TypeSymbol:
		if !m.togo {
			return m.add(4, 1) // an element: null
		}
	case TypeObject:
		if depth >= jsonMaxDepth {
			return m.deep()
		}
		if m.togo {
			return m.togoObject(v.AsObject(), depth)
		}
		return m.object(v.AsObject(), depth)
	}
	return false
}

// object counts AppendJSON's text of the object o: a host placeholder, a
// plain object or a dense array none of whose prototypes has a toJSON (the
// walk's kind).
func (m *decodeMeasure) object(o *Object, depth int) bool {
	if !m.r.lacksToJSON(o, &m.plainEpoch) {
		return false
	}
	if o.flags&flagHasLazy != 0 {
		src := o.hostSrc()
		t, raw := src.(rawText)
		if !raw {
			return m.goJS(src, depth)
		}
		// AppendJSON writes the text of the value a RawMessage parses to,
		// which only the parse gives exactly: a lower bound of it, from one
		// scan of the engine's copy, fails a text plainly past a limit
		// first, without the parse.
		if b, n := rawLowerBound(string(t)); m.bytes+b > m.maxBytes || m.nodes+n > m.maxNodes {
			return m.add(b, n)
		}
		o.materializeHost()
	}
	if o.class == ClassArray {
		return m.elements(o, depth)
	}
	if len(o.elements) != 0 || o.dict != nil && o.dict.sparse != nil {
		return false
	}
	if !m.add(2, 1) {
		return false
	}
	first := true
	return jsonMembers(o, func(key PropertyKey, e Value) bool {
		if e.IsUndefined() || e.IsSymbol() {
			return true // left out
		}
		if e.IsObject() && !m.r.lacksToJSON(e.AsObject(), &m.plainEpoch) {
			return false // a function or a toJSON may leave the member out
		}
		b := jsQuotedLen(key.String()) + 1
		if !first {
			b++
		}
		first = false
		return m.add(b, 0) && m.js(e, depth+1)
	})
}

// elements counts the elements of the dense array a; false for a hole.
func (m *decodeMeasure) elements(a *Object, depth int) bool {
	n := int(a.internal.(*ArrayData).length)
	if n != len(a.elements) || a.dict != nil && len(a.dict.sparse) != 0 {
		return false
	}
	if !m.add(int64(2+max(n-1, 0)), 1) {
		return false
	}
	for _, e := range a.elements {
		if e.IsHole() || !m.js(e, depth+1) {
			return false
		}
	}
	return true
}

// togoObject counts json.Marshal's text of what ToGo gives for o: a host
// node's Go value, an array or the map of an ordinary object's own
// enumerable data properties.
func (m *decodeMeasure) togoObject(o *Object, depth int) bool {
	if o.flags&flagHostNode != 0 && o.hostUnchanged(0) {
		return m.goJSON(o.hostSrc(), depth) // HostValue without its copy of a RawMessage
	}
	switch {
	case o.flags&flagHasLazy != 0:
		return false
	case o.class == ClassArray:
		return m.elements(o, depth)
	case o.class != ClassObject || o.internal != nil && o.flags&flagHostNode == 0 ||
		len(o.elements) != 0 || o.dict != nil && o.dict.sparse != nil:
		return false
	}
	if !m.add(2, 1) {
		return false
	}
	first := true
	return jsonMembers(o, func(key PropertyKey, e Value) bool {
		k := key.String()
		f := k // goQuotedLenJS takes k: a view stays on the stack
		if k.needsFlat() {
			f = k.flat(new(String))
		}
		if f.kind != strASCII && hasLoneSurrogate(f.units()) {
			return false // two keys could be one Go string: ToGo keeps one
		}
		b := goQuotedLenJS(k) + 1
		if !first {
			b++
		}
		first = false
		return m.add(b, 0) && m.js(e, depth+1)
	})
}

func hasLoneSurrogate(u []uint16) bool {
	for i := 0; i < len(u); i++ {
		if c := u[i]; c >= 0xD800 && c < 0xE000 {
			if c >= 0xDC00 || i+1 >= len(u) || u[i+1] < 0xDC00 || u[i+1] >= 0xE000 {
				return true
			}
			i++
		}
	}
	return false
}

// goJS counts AppendJSON's text of the Go value of a host placeholder: the
// text of what materialization makes of it, which the stringifier writes
// from the Go value. It gives no verdict for a Go type the stringifier's
// walk does not write, for a json.RawMessage, and for a map whose keys are
// not valid UTF-8 (two of them could be one key).
func (m *decodeMeasure) goJS(g any, depth int) bool {
	if depth >= jsonMaxDepth {
		return m.deep()
	}
	switch x := g.(type) {
	case string:
		return m.add(jsQuotedLenGo(x), 1)
	case float64:
		return m.add(jsNumberLen(x), 1)
	case bool:
		if x {
			return m.add(4, 1)
		}
		return m.add(5, 1)
	case nil:
		return m.add(4, 1)
	case map[string]any:
		if x == nil {
			return m.add(4, 1)
		}
		return goMapJS(m, x, func(e any) bool { return m.goJS(e, depth+1) })
	case []any:
		if !m.add(int64(2+max(len(x)-1, 0)), 1) {
			return false
		}
		for _, e := range x {
			if !m.goJS(e, depth+1) {
				return false
			}
		}
		return true
	case map[string]string:
		if x == nil {
			return m.add(4, 1)
		}
		return goMapJS(m, x, func(s string) bool { return m.add(jsQuotedLenGo(s), 1) })
	case []string:
		return m.goStringsJS(x)
	case map[string][]string:
		if x == nil {
			return m.add(4, 1)
		}
		return goMapJS(m, x, m.goStringsJS)
	case []map[string]any:
		if !m.add(int64(2+max(len(x)-1, 0)), 1) {
			return false
		}
		for _, e := range x {
			if !m.goJS(e, depth+1) {
				return false
			}
		}
		return true
	case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8, float32:
		n, _, _ := scalarFromGo(x)
		return m.add(jsNumberLen(n.AsNumber()), 1)
	}
	return false
}

// goStringsJS counts a []string as materialization makes it: an array, empty
// for a nil list.
func (m *decodeMeasure) goStringsJS(list []string) bool {
	if !m.add(int64(2+max(len(list)-1, 0)), 1) {
		return false
	}
	for _, s := range list {
		if !m.add(jsQuotedLenGo(s), 1) {
			return false
		}
	}
	return true
}

// goMapJS counts a Go map as materialization makes it, its values counted
// by value.
func goMapJS[V any](m *decodeMeasure, x map[string]V, value func(V) bool) bool {
	if !m.add(int64(2+max(len(x)-1, 0)), 1) {
		return false
	}
	for k, e := range x {
		if !utf8.ValidString(k) {
			return false
		}
		if !m.add(jsQuotedLenGo(k)+1, 0) || !value(e) {
			return false
		}
	}
	return true
}

// goJSON counts json.Marshal's text of g.
func (m *decodeMeasure) goJSON(g any, depth int) bool {
	if depth >= jsonMaxDepth {
		return m.deep()
	}
	switch x := g.(type) {
	case string:
		return m.add(goQuotedLen(x), 1)
	case float64:
		return m.add(goFloatLen(x, 64), 1)
	case bool:
		if x {
			return m.add(4, 1)
		}
		return m.add(5, 1)
	case nil:
		return m.add(4, 1)
	case int64:
		return m.add(intLen(x), 1)
	case map[string]any:
		if x == nil {
			return m.add(4, 1)
		}
		return goMapJSON(m, x, func(e any) bool { return m.goJSON(e, depth+1) })
	case []any:
		if x == nil {
			return m.add(4, 1)
		}
		if !m.add(int64(2+max(len(x)-1, 0)), 1) {
			return false
		}
		for _, e := range x {
			if !m.goJSON(e, depth+1) {
				return false
			}
		}
		return true
	case map[string]string:
		if x == nil {
			return m.add(4, 1)
		}
		return goMapJSON(m, x, func(s string) bool { return m.add(goQuotedLen(s), 1) })
	case []string:
		return m.goStringsJSON(x)
	case map[string][]string:
		if x == nil {
			return m.add(4, 1)
		}
		return goMapJSON(m, x, m.goStringsJSON)
	case []map[string]any:
		if x == nil {
			return m.add(4, 1)
		}
		if !m.add(int64(2+max(len(x)-1, 0)), 1) {
			return false
		}
		for _, e := range x {
			if !m.goJSON(e, depth+1) {
				return false
			}
		}
		return true
	case [][2]any: // a Map's entries
		if x == nil {
			return m.add(4, 1)
		}
		if !m.add(int64(2+max(len(x)-1, 0)), 1) {
			return false
		}
		for _, e := range x {
			if !m.add(3, 1) || !m.goJSON(e[0], depth+2) || !m.goJSON(e[1], depth+2) {
				return false
			}
		}
		return true
	case int:
		return m.add(intLen(int64(x)), 1)
	case int32:
		return m.add(intLen(int64(x)), 1)
	case int16:
		return m.add(intLen(int64(x)), 1)
	case int8:
		return m.add(intLen(int64(x)), 1)
	case uint:
		return m.add(uintLen(uint64(x)), 1)
	case uint64:
		return m.add(uintLen(x), 1)
	case uint32:
		return m.add(uintLen(uint64(x)), 1)
	case uint16:
		return m.add(uintLen(uint64(x)), 1)
	case uint8:
		return m.add(uintLen(uint64(x)), 1)
	case float32:
		return m.add(goFloatLen(float64(x), 32), 1)
	case json.Number:
		if x == "" {
			return m.add(1, 1) // 0
		}
		return m.add(int64(len(x)), 1)
	case json.RawMessage:
		if x == nil {
			return m.add(4, 1)
		}
		b, n := jsonTextCount(bytesToString(x), true)
		return m.add(b, n)
	case rawText: // an unread placeholder's text: ToGo exports it as a RawMessage
		b, n := jsonTextCount(string(x), true)
		return m.add(b, n)
	case []byte:
		if x == nil {
			return m.add(4, 1)
		}
		return m.add(int64(base64.StdEncoding.EncodedLen(len(x))+2), 1)
	case *big.Int:
		if x == nil {
			return m.add(4, 1)
		}
		return m.add(int64(len(x.Text(10))), 1)
	case time.Time:
		b, err := x.MarshalJSON()
		if err != nil {
			return m.add(4, 1)
		}
		return m.add(int64(len(b)), 1)
	}
	// Any other type (a function or a symbol ToGo returns as itself, a Go
	// value of the host's): its text, written.
	b, err := json.Marshal(g)
	if err != nil {
		return m.add(4, 1)
	}
	_, n := jsonTextCount(bytesToString(b), false)
	return m.add(int64(len(b)), n)
}

// goStringsJSON counts json.Marshal's text of a []string: null when nil.
func (m *decodeMeasure) goStringsJSON(list []string) bool {
	if list == nil {
		return m.add(4, 1)
	}
	if !m.add(int64(2+max(len(list)-1, 0)), 1) {
		return false
	}
	for _, s := range list {
		if !m.add(goQuotedLen(s), 1) {
			return false
		}
	}
	return true
}

// goMapJSON counts json.Marshal's text of a non-nil Go map, its values
// counted by value.
func goMapJSON[V any](m *decodeMeasure, x map[string]V, value func(V) bool) bool {
	if !m.add(int64(2+max(len(x)-1, 0)), 1) {
		return false
	}
	for k, e := range x {
		if !m.add(goQuotedLen(k)+1, 0) || !value(e) {
			return false
		}
	}
	return true
}

// --- the lengths of the writers' texts ------------------------------------------

// jsNumberLen is the length of what the stringifier writes for f.
func jsNumberLen(f float64) int64 {
	if f != f || math.IsInf(f, 0) {
		return 4 // null
	}
	var buf [32]byte
	return int64(len(AppendNumber(buf[:0], f)))
}

// togoNumberLen is the length of json.Marshal's text of ToGo's value of the
// number f (togoNumberInto): an int64's digits, or a float64's.
func togoNumberLen(f float64) int64 {
	if f == math.Trunc(f) && f >= -1<<63 && f < 1<<63 && (f != 0 || !math.Signbit(f)) {
		return intLen(int64(f))
	}
	return goFloatLen(f, 64)
}

// goFloatLen is the length of json.Marshal's text of a float of bits bits;
// a NaN or an infinity counts as null.
func goFloatLen(f float64, bits int) int64 {
	if f != f || math.IsInf(f, 0) {
		return 4
	}
	var buf [32]byte
	if bits == 64 {
		return int64(len(appendGoJSONFloat(buf[:0], f)))
	}
	format := byte('f')
	if abs := float32(math.Abs(f)); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b := strconv.AppendFloat(buf[:0], f, format, -1, 32)
	if n := len(b); format == 'e' && n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
		b = b[:n-1]
	}
	return int64(len(b))
}

func intLen(n int64) int64 {
	var buf [24]byte
	return int64(len(strconv.AppendInt(buf[:0], n, 10)))
}

func uintLen(n uint64) int64 {
	var buf [24]byte
	return int64(len(strconv.AppendUint(buf[:0], n, 10)))
}

// jsEscapeLen is the length of the stringifier's text for the ASCII byte c
// in a string: \", \\, \b, \f, \n, \r, \t, \u00XX for another control
// character.
func jsEscapeLen(c byte) int64 {
	switch {
	case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
		return 2
	case c < 0x20:
		return 6
	}
	return 1
}

// jsQuotedLen is the length of AppendJSON's text of the string s: UTF-8,
// a lone surrogate escaped. A lazy string's text whose length its source
// gives is measured without decoding (jsQuotedLenLazy).
func jsQuotedLen(s *String) int64 {
	if s.isLazy() {
		if n, ok := jsQuotedLenLazy(s); ok {
			return n
		}
	}
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.json == jsonPlain {
		return int64(f.n) + 2
	}
	if f.kind == strASCII {
		n := int64(len(f.s)) + 2
		for i := jsonScan(f.s, 0, 0); i < len(f.s); i = jsonScan(f.s, i+1, 0) {
			n += jsEscapeLen(f.s[i]) - 1
		}
		return n
	}
	u := f.units()
	n := int64(2)
	for i := 0; i < len(u); i++ {
		switch c := u[i]; {
		case c < 0x80:
			n += jsEscapeLen(byte(c))
		case c < 0x800:
			n += 2
		case c < 0xD800 || c >= 0xE000:
			n += 3
		case c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000:
			n += 4
			i++
		default:
			n += 6
		}
	}
	return n
}

// jsQuotedLenLazy is jsQuotedLen of a lazy string from its source, as
// quoteLazy writes it: copied when plain or verbatim, and Go text, whose
// invalid bytes decode to U+FFFD, measured as jsQuotedLenGo measures it.
// WTF-8 that may encode a surrogate and escaped JSON need decoding.
func jsQuotedLenLazy(s *String) (int64, bool) {
	switch {
	case s.plainLazyJSON() || s.lazyMode() == lazyJSONVerbatim:
		return int64(len(s.s)) + 2, true
	case s.lazyMode() == lazyText || s.validLazyUTF8():
		return jsQuotedLenGo(s.s), true
	}
	return 0, false
}

// jsQuotedLenGo is jsQuotedLen of FromGoString(g): an invalid byte is
// U+FFFD, three bytes of UTF-8.
func jsQuotedLenGo(g string) int64 {
	if isASCII(g) {
		n := int64(len(g)) + 2
		for i := jsonScan(g, 0, 0); i < len(g); i = jsonScan(g, i+1, 0) {
			n += jsEscapeLen(g[i]) - 1
		}
		return n
	}
	n := int64(2)
	for i := 0; i < len(g); {
		if c := g[i]; c < utf8.RuneSelf {
			n += jsEscapeLen(c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(g[i:])
		if r == utf8.RuneError && size == 1 {
			n += 3
		} else {
			n += int64(size)
		}
		i += size
	}
	return n
}

// goEscapeLen is the length of json.Marshal's text for the ASCII byte c in
// a string: as jsEscapeLen, with <, > and & escaped too.
func goEscapeLen(c byte) int64 {
	if c == '<' || c == '>' || c == '&' {
		return 6
	}
	return jsEscapeLen(c)
}

// goInvalidLen is the length of json.Marshal's text for a byte of a Go
// string that is not UTF-8: the escape \ufffd, six bytes, by encoding/json's
// own encoder (Go 1.26 and before, GOEXPERIMENT=nojsonv2 after), U+FFFD
// itself, three bytes, by encoding/json/v2's, which json.Marshal runs on from
// Go 1.27 (GOEXPERIMENT=jsonv2 before). Set when the package initializes, so
// that goQuotedLen's loop reads a variable rather than calling a function.
var goInvalidLen = func() int64 {
	b, _ := json.Marshal("\xff")
	return int64(len(b)) - 2
}()

// goQuotedLen is the length of json.Marshal's text of the Go string g: an
// invalid byte is goInvalidLen bytes, U+2028 and U+2029 are escaped.
func goQuotedLen(g string) int64 {
	if isASCII(g) {
		n := int64(len(g)) + 2 + 5*int64(strings.Count(g, "<")+strings.Count(g, ">")+strings.Count(g, "&"))
		for i := jsonScan(g, 0, 0); i < len(g); i = jsonScan(g, i+1, 0) {
			n += jsEscapeLen(g[i]) - 1
		}
		return n
	}
	n := int64(2)
	for i := 0; i < len(g); {
		if c := g[i]; c < utf8.RuneSelf {
			n += goEscapeLen(c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(g[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += goInvalidLen
		case r == '\u2028', r == '\u2029':
			n += 6
		default:
			n += int64(size)
		}
		i += size
	}
	return n
}

// goQuotedLenJS is goQuotedLen of the Go string of s (GoString: a lone
// surrogate is U+FFFD).
func goQuotedLenJS(s *String) int64 {
	if g, ok := s.sourceUTF8(); ok {
		return goQuotedLen(g)
	}
	f := s
	if s.needsFlat() {
		f = s.flat(new(String))
	}
	if f.kind == strASCII {
		return goQuotedLen(f.s)
	}
	u := f.units()
	n := int64(2)
	for i := 0; i < len(u); i++ {
		switch c := u[i]; {
		case c < 0x80:
			n += goEscapeLen(byte(c))
		case c < 0x800:
			n += 2
		case c == 0x2028 || c == 0x2029:
			n += 6
		case c < 0xD800 || c >= 0xE000:
			n += 3
		case c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000:
			n += 4
			i++
		default:
			n += 3 // U+FFFD
		}
	}
	return n
}

// jsonTextCount counts the values in the JSON text s, and returns its
// length, or with compact the length json.Marshal gives it as a
// json.RawMessage: white space between tokens dropped, <, >, &, U+2028 and
// U+2029 escaped.
func jsonTextCount(s string, compact bool) (bytes, nodes int64) {
	if !compact {
		bytes = int64(len(s))
	}
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		case '"':
			start := i
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				} else if compact {
					switch {
					case s[i] == '<' || s[i] == '>' || s[i] == '&':
						bytes += 5
					case s[i] == 0xE2 && i+2 < len(s) && s[i+1] == 0x80 && (s[i+2] == 0xA8 || s[i+2] == 0xA9):
						bytes += 3
					}
				}
				i++
			}
			i++ // the closing quote
			j := jsonSkipSpace(s, i)
			if j >= len(s) || s[j] != ':' {
				nodes++ // a key is no value
			}
			if compact {
				bytes += int64(min(i, len(s)) - start)
			}
			continue
		case '{', '[':
			nodes++
		case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', 't', 'f', 'n':
			nodes++
			start := i
			for i < len(s) && s[i] != ',' && s[i] != ']' && s[i] != '}' && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
				i++
			}
			if compact {
				bytes += int64(i - start)
			}
			continue
		}
		if compact {
			bytes++
		}
		i++
	}
	return bytes, nodes
}

// rawLowerBound returns lower bounds of the bytes and the values of
// AppendJSON's text of the value the checked JSON text s parses to, from one
// scan and no parse. A string writes at least its quotes, a byte for each
// byte of its literal outside escapes (an invalid one is U+FFFD, three) and
// one for each escape; a number at least one byte; true, false and null
// themselves; an array its elements and commas. Of an object's members only
// those certainly not replaced by a later member with the same key count:
// its last member, and, when every key of the object is written in plain
// ASCII without escapes (keys then equal as written), every member whose key
// no later member repeats.
func rawLowerBound(s string) (bytes, nodes int64) {
	lb := rawBound{s: s}
	lb.value(jsonSkipSpace(s, 0))
	return lb.bytes, lb.nodes
}

type rawBound struct {
	s            string
	bytes, nodes int64
	members      []rawMember // the members of the objects open, as a stack
}

type rawMember struct {
	start, end   int // the key, quoted
	bytes, nodes int64
}

// value counts the value at i and returns the index after it.
func (lb *rawBound) value(i int) int {
	s := lb.s
	switch c := s[i]; c {
	case '"':
		j, b := rawStringBound(s, i)
		lb.bytes += b
		lb.nodes++
		return j
	case '{':
		return lb.object(i)
	case '[':
		lb.bytes += 2
		lb.nodes++
		if i = jsonSkipSpace(s, i+1); s[i] == ']' {
			return i + 1
		}
		for {
			if i = jsonSkipSpace(s, lb.value(i)); s[i] == ']' {
				return i + 1
			}
			lb.bytes++ // a comma
			i = jsonSkipSpace(s, i+1)
		}
	case 't', 'n':
		lb.bytes += 4
		lb.nodes++
		return i + 4
	case 'f':
		lb.bytes += 5
		lb.nodes++
		return i + 5
	}
	lb.bytes++ // a number
	lb.nodes++
	for i++; i < len(s); i++ {
		if c := s[i]; c == ',' || c == ']' || c == '}' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			break
		}
	}
	return i
}

// object counts the object at i and returns the index after it.
func (lb *rawBound) object(i int) int {
	s := lb.s
	lb.nodes++
	if i = jsonSkipSpace(s, i+1); s[i] == '}' {
		lb.bytes += 2
		return i + 1
	}
	base := len(lb.members)
	plain := true
	for {
		start := i
		end, kb := rawStringBound(s, i)
		for _, c := range []byte(s[start+1 : end-1]) {
			if c >= 0x80 || c == '\\' {
				plain = false
				break
			}
		}
		vb, vn := lb.bytes, lb.nodes
		i = jsonSkipSpace(s, lb.value(jsonSkipSpace(s, jsonSkipSpace(s, end)+1)))
		// A member: its key, a colon and its value; counted below if it
		// certainly stays.
		lb.members = append(lb.members, rawMember{start: start, end: end, bytes: kb + 1 + lb.bytes - vb, nodes: lb.nodes - vn})
		lb.bytes, lb.nodes = vb, vn
		if s[i] == '}' {
			i++
			break
		}
		i = jsonSkipSpace(s, i+1)
	}
	members := lb.members[base:]
	var seen map[string]struct{} // the keys of the later members, for a wide object
	if plain && len(members) > 64 {
		seen = make(map[string]struct{}, len(members))
	}
	kept := int64(0)
	for k := len(members) - 1; k >= 0; k-- {
		m := members[k]
		key := s[m.start:m.end]
		stays := k == len(members)-1
		switch {
		case stays || !plain:
		case seen != nil:
			_, repeated := seen[key]
			stays = !repeated
		default:
			stays = true
			for _, later := range members[k+1:] {
				if s[later.start:later.end] == key {
					stays = false
					break
				}
			}
		}
		if seen != nil {
			seen[key] = struct{}{}
		}
		if stays {
			lb.bytes += m.bytes
			lb.nodes += m.nodes
			kept++
		}
	}
	lb.bytes += 2 + kept - 1 // the braces and the commas
	lb.members = lb.members[:base]
	return i
}

// rawStringBound returns the index after the checked string literal at i and
// a lower bound of the bytes AppendJSON writes for its value.
func rawStringBound(s string, i int) (int, int64) {
	b := int64(2)
	for j := i + 1; ; {
		k := jsonScan(s, j, 0) // a quote or a backslash: the text is checked
		b += int64(k - j)
		if s[k] == '"' {
			return k + 1, b
		}
		b++ // an escape writes a byte at least
		if s[k+1] == 'u' {
			j = k + 6
		} else {
			j = k + 2
		}
	}
}

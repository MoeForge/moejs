package engine

import (
	"encoding"
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Unmarshal without the text. Realm.Unmarshal stores a value into a Go
// value as encoding/json's Unmarshal stores the text AppendJSON writes for
// it, walking the value and the target together. It covers what a hook
// returns: plain objects, dense arrays, strings, numbers, booleans, null,
// and host placeholders reached through an interface. Whatever it could
// not reproduce exactly makes it stop and report that it did not complete;
// the caller then runs the round trip, which writes every part of the
// target again (encoding/json decodes into what it finds, as the walk did),
// so the result is the round trip's, its error included. The walk starts
// only once all of v is plain (plain), the parts the target discards and
// the Go values of host placeholders included: a value AppendJSON would
// fail on or run code for falls back before anything is written, and when
// AppendJSON fails the target is left as it was, json.Unmarshal never
// running. That includes the string length limit: plain adds up an upper
// bound of AppendJSON's output (six bytes a code unit, one for a string
// known plain) and falls back once it passes the limit, which also bounds
// its walk of a value that repeats one object (plain visits no more than
// AppendJSON writes). After an error of json.Unmarshal (a value the
// target's type rejects) the target holds what it wrote, as with the round
// trip.
//
// The walk runs no JavaScript: a value with a toJSON, an accessor, a proxy
// or any object that is not a plain object or dense array stops it before
// anything could run.
//
// Realm.ToGoInto is the same walk for another round trip, json.Unmarshal of
// the text json.Marshal writes for what ToGo gives (togo). That text holds
// other things: a member that is undefined stays, as null; no toJSON is
// called and prototypes do not matter; an integer of int64's range is an
// int64 and any other number (-0 included) a float64, which reads back as
// itself; a NaN or an infinity is json.Marshal's error; and a host
// placeholder JavaScript has not modified is its Go value as json.Marshal
// writes it (a nil slice as null). json.Marshal sorts a map's keys, and the
// walk writes an object's members in that order (togoObject). ToGo observes
// no interrupt for a value the walk handles, and the walk observes none
// either.

// Unmarshal stores v in the Go value target points to as encoding/json's
// Unmarshal stores the text AppendJSON writes for v, without the text, and
// reports whether it completed; when it did not, the caller is to finish with
// the round trip (Unmarshal of AppendJSON's text), which writes every part
// again. It writes nothing unless all of v, the parts target discards and the
// Go values of host placeholders included, is plain; it does not complete
// for: a value AppendJSON would run JavaScript for (a toJSON, an accessor, a
// proxy, a function, which a toJSON could replace) or fail on (a BigInt, a
// cycle, a host's Go value it cannot write) or whose text could pass the
// string length limit (an upper bound of six bytes a code unit, one for a
// string known plain), an object that is not a plain
// object, dense array or host placeholder, a host placeholder anywhere but in
// an empty interface (but in a json.RawMessage, which receives the text of
// its value, rawInto), a target type that unmarshals itself
// (json.Unmarshaler, encoding.TextUnmarshaler; not json.RawMessage) or has
// an embedded field or a tag jsonStructFields leaves to the round trip
// (",string" among them), a
// non-empty interface, an interface holding a pointer, a []byte from a
// string, and any value the target's type rejects. Strings are not copied:
// the Go string of an ASCII string is the one v holds. err is an interrupt,
// observed every 4096 nodes of each walk (the check and the store), as
// AppendJSON's walk observes it, a host's Go string counting a node per
// 4096 bytes when the check of valid UTF-8 scans it; AppendJSON also
// observes one inside a JavaScript string it escapes, which the walks,
// copying no string, do not scan.
func (r *Realm) Unmarshal(v Value, target any) (complete bool, err error) {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || jsonTypeOf(rv.Type()).custom {
		return false, nil
	}
	// Nothing is written unless all of v is plain: then AppendJSON cannot
	// fail or run code, and a walk that stops (a value the target's type
	// rejects) leaves the round trip to write every part again.
	d := jsonDecoder{r: r, left: 4096, copyHosts: jsonTypeOf(rv.Type()).holdsAny}
	// plain checks the bound before each container; the leaves after the
	// last one count here.
	if !d.plain(v, 0) || !d.within(0) {
		return false, d.err
	}
	// The walk's copies of host placeholders count again; plain's bound
	// already covers them. The walk has a budget of its own, as
	// AppendJSON's one walk has: a pending interrupt fails a value one of
	// the walks counts 4096 nodes in, not one the two count 4096 in
	// together.
	d.size, d.left = 0, 4096
	ok := d.value(v, rv.Elem(), 0)
	return ok && d.err == nil, d.err
}

// ToGoInto stores v in the Go value target points to as encoding/json's
// Unmarshal stores the text json.Marshal writes for ToGoStrict(v), without
// the text, and reports whether it completed; when it did not, the caller
// is to finish with that round trip, which writes every part again. It
// writes nothing unless all of v, the parts target discards and the Go
// values of host placeholders included, is plain: ToGo runs no JavaScript
// for it, and json.Marshal does not fail on what ToGo gives (a NaN or an
// infinity, a cycle). It does not complete for: a bigint, a symbol, a
// function, an object that is not an ordinary object or an array (a Date,
// a Map, a typed array, a boxed primitive, a proxy), an accessor, a hole,
// a value whose text could pass the string length limit (plain's bound,
// kept as a bound of the walk), a host placeholder anywhere but in an
// empty interface or a json.RawMessage, and what Unmarshal does not
// complete for on the
// target's side. Strings are not copied. No interrupt is observed, as ToGo
// observes none for such a value.
func (r *Realm) ToGoInto(v Value, target any) (complete bool) {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || jsonTypeOf(rv.Type()).custom {
		return false
	}
	d := jsonDecoder{r: r, left: 4096, togo: true, copyHosts: jsonTypeOf(rv.Type()).holdsAny}
	if !d.plain(v, 0) || !d.within(0) {
		return false
	}
	d.size, d.left = 0, 4096
	return d.value(v, rv.Elem(), 0)
}

// CanUnmarshalDirect reports whether all of v is plain, as Unmarshal checks
// it before writing: AppendJSON writes its text without running JavaScript
// or failing, and Unmarshal stores it without the text when the target's
// type allows. It writes nothing and copies no host value; a pending
// interrupt makes it false.
func (r *Realm) CanUnmarshalDirect(v Value) bool {
	d := jsonDecoder{r: r, left: 4096}
	return d.plain(v, 0) && d.within(0)
}

// CanToGoIntoDirect is CanUnmarshalDirect for ToGoInto: ToGo reads v without
// running JavaScript and json.Marshal does not fail on what it gives.
func (r *Realm) CanToGoIntoDirect(v Value) bool {
	d := jsonDecoder{r: r, left: 4096, togo: true}
	return d.plain(v, 0) && d.within(0)
}

type jsonDecoder struct {
	r          *Realm
	plainEpoch uint32 // lacksToJSON's prototype answer
	left       int    // the nodes until the next interrupt check (tick)
	err        error
	// togo: the round trip is json.Unmarshal of json.Marshal of ToGo's
	// result (ToGoInto), not of AppendJSON's text.
	togo bool
	// size is an upper bound, in bytes, of the output AppendJSON writes for
	// what plain has checked: past the string length limit AppendJSON
	// fails, so plain stops there (within) and the round trip decides.
	size int64
	// copyHosts: the target is likely to hold a host placeholder in an
	// empty interface (jsonHoldsAny), so plain checks a placeholder by the
	// copy hostCopy makes of it, and keeps the copies, in the order it
	// met the placeholders, for the walk.
	copyHosts bool
	hosts     []jsonHostCopy
	next      int // the first copy the walk has not taken
}

type jsonHostCopy struct {
	o *Object
	g any
	_ [0]func() // not comparable: no equality function to link
}

// JSON kinds of a value, as AppendJSON sees it.
const (
	jkOmitted = iota // undefined, a symbol: no member, a null element
	jkNull           // also a number that is not finite
	jkBool
	jkNumber
	jkString
	jkObject
	jkArray
	jkHost // a host placeholder
)

// kind classifies v; ok is false for a value the walk does not handle.
func (d *jsonDecoder) kind(v Value) (k int, ok bool) {
	switch v.Type() {
	case TypeUndefined:
		if d.togo {
			return jkNull, true // nil: a member stays, as null
		}
		return jkOmitted, true
	case TypeSymbol:
		return jkOmitted, !d.togo
	case TypeNull:
		return jkNull, true
	case TypeBoolean:
		return jkBool, true
	case TypeNumber:
		if f := v.AsNumber(); math.IsNaN(f) || math.IsInf(f, 0) {
			return jkNull, !d.togo // json.Marshal fails on it
		}
		return jkNumber, true
	case TypeString:
		return jkString, true
	case TypeObject:
		if d.togo {
			return togoKind(v.AsObject())
		}
		// A function is omitted only when no toJSON of its own or of its
		// prototypes replaces it: the round trip finds out.
		o := v.AsObject()
		if !d.r.lacksToJSON(o, &d.plainEpoch) {
			return 0, false
		}
		switch {
		case o.flags&flagHasLazy != 0:
			return jkHost, true
		case o.class == ClassArray:
			return jkArray, true
		case len(o.elements) == 0 && (o.dict == nil || o.dict.sparse == nil):
			return jkObject, true
		}
	}
	return 0, false
}

// togoKind is kind of an object for ToGoInto: the JSON kind of the text
// json.Marshal writes for what ToGo gives for o. ok is false for what the
// walk does not handle: every object but a host placeholder JavaScript has
// not modified (its Go value), an array and an ordinary object (ToGo reads
// neither prototypes nor toJSON). An object with internal data is left to
// the round trip, but a host node's Go value, which a modified one keeps.
// kind's other differences for ToGoInto: undefined is null (ToGo's nil, a
// member that stays), and a symbol, a bigint and a number json.Marshal
// fails on (NaN, ±Infinity) are not handled.
func togoKind(o *Object) (k int, ok bool) {
	if o.flags&flagHostNode != 0 && o.hostUnchanged(0) { // HostValue without its copy of a RawMessage
		return jkHost, true
	}
	switch {
	case o.flags&flagHasLazy != 0: // ToGo materializes it
	case o.class == ClassArray:
		return jkArray, true
	case o.class == ClassObject && (o.internal == nil || o.flags&flagHostNode != 0) &&
		len(o.elements) == 0 && (o.dict == nil || o.dict.sparse == nil):
		return jkObject, true
	}
	return 0, false
}

// value stores v (an object member or array element) in rv.
func (d *jsonDecoder) value(v Value, rv reflect.Value, depth int) bool {
	if depth >= jsonScanDepth || !d.tick(0) {
		return false
	}
	k, ok := d.kind(v)
	if !ok {
		return false
	}
	t := rv.Type()
	jt := jsonTypeOf(t)
	if jt.custom {
		return jt.raw && d.rawInto(v, k, rv)
	}
	if rv.Kind() == reflect.Interface && !rv.IsNil() {
		if e := rv.Elem(); e.Kind() == reflect.Pointer && !e.IsNil() {
			return false // Unmarshal decodes into what it points to
		}
	}
	if k == jkNull || k == jkOmitted {
		switch rv.Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice:
			rv.SetZero()
		}
		return true
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			rv.Set(reflect.New(t.Elem()))
		}
		return d.value(v, rv.Elem(), depth)
	case reflect.Interface:
		if t.NumMethod() != 0 {
			return false
		}
		g, ok := d.any(v, k, depth)
		if !ok {
			return false
		}
		if g == nil {
			rv.SetZero()
		} else {
			rv.Set(reflect.ValueOf(g))
		}
		return true
	}
	switch k {
	case jkBool:
		if rv.Kind() != reflect.Bool {
			return false
		}
		rv.SetBool(v.AsBool())
	case jkNumber:
		return d.numberInto(v.AsNumber(), rv, t)
	case jkString:
		if rv.Kind() != reflect.String || t == reflect.TypeFor[json.Number]() {
			return false
		}
		rv.SetString(v.AsString().GoString())
	case jkObject:
		switch rv.Kind() {
		case reflect.Struct, reflect.Map:
			if d.togo {
				return d.togoObject(v.AsObject(), rv, jt, depth)
			}
			return d.object(v.AsObject(), rv, jt, depth)
		}
		return false
	case jkArray:
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return false
		}
		return d.array(v.AsObject(), rv, depth)
	default: // jkHost: only into an empty interface
		return false
	}
	return true
}

// rawInto stores in the json.RawMessage rv the text the round trip gives
// it, which is the text of v in the round trip's text: AppendJSON's text of
// v for Unmarshal, json.Marshal's text of ToGo's value of v for ToGoInto
// (sorted keys, <, > and & escaped), null for null and for an undefined
// element. Like RawMessage.UnmarshalJSON, it writes into rv's array when the
// text fits. plain has checked v: AppendJSON runs no code for it and ToGo
// reads no getter, so the text is v's whatever v is part of.
func (d *jsonDecoder) rawInto(v Value, k int, rv reflect.Value) bool {
	dst := rv.Bytes()[:0]
	switch {
	case k == jkNull || k == jkOmitted:
		dst = append(dst, "null"...)
	case d.togo:
		var text []byte
		if t, ok := unreadRawText(v); ok {
			// ToGo's copy of the text, compacted by json.Marshal, without
			// the copy.
			text, _ = json.Marshal(json.RawMessage(rawBytes(t)))
		} else {
			g, err := d.r.ToGoStrict(v)
			if err != nil {
				return false
			}
			if text, err = json.Marshal(g); err != nil {
				return false
			}
		}
		dst = append(dst, text...)
	default:
		js := jsonStringifier{r: d.r, raw: true, inDst: true}
		js.sb.b = slices.Grow(dst, 64) // not nil: a nil b is the builder's inline array
		if ok, err := js.serialize(v); !ok {
			d.err = err // an interrupt: AppendJSON observes it too
			return false
		}
		dst = js.sb.b
	}
	rv.SetBytes(dst)
	return true
}

// numberInto stores a finite number: togoNumberInto for ToGoInto,
// jsonNumberInto otherwise. value calls it once, which keeps value small.
func (d *jsonDecoder) numberInto(f float64, rv reflect.Value, t reflect.Type) bool {
	if d.togo {
		return togoNumberInto(f, rv, t)
	}
	return jsonNumberInto(f, rv, t)
}

// jsonNumberInto stores a finite number as Unmarshal stores the number
// AppendNumber writes for it. Every number reads back as itself (but -0 as
// 0), and an integer of magnitude below 2^53 is written with all its
// digits; a larger one is written with its shortest digits padded with
// zeros, so an integer target parses that text, as a float32 does (rounding
// twice could differ).
func jsonNumberInto(f float64, rv reflect.Value, t reflect.Type) bool {
	var buf [32]byte
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if f != math.Trunc(f) {
			return false
		}
		n := int64(f)
		if f <= -1<<53 || f >= 1<<53 {
			var err error
			if n, err = strconv.ParseInt(string(AppendNumber(buf[:0], f)), 10, 64); err != nil {
				return false
			}
		}
		if rv.OverflowInt(n) {
			return false
		}
		rv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if f != math.Trunc(f) || f < 0 {
			return false
		}
		n := uint64(f)
		if f >= 1<<53 {
			var err error
			if n, err = strconv.ParseUint(string(AppendNumber(buf[:0], f)), 10, 64); err != nil {
				return false
			}
		}
		if rv.OverflowUint(n) {
			return false
		}
		rv.SetUint(n)
	case reflect.Float64:
		rv.SetFloat(f + 0) // -0 reads back as 0
	case reflect.Float32:
		g, err := strconv.ParseFloat(string(AppendNumber(buf[:0], f)), 32)
		if err != nil || rv.OverflowFloat(g) {
			return false
		}
		rv.SetFloat(g)
	case reflect.String:
		if t != reflect.TypeFor[json.Number]() {
			return false
		}
		rv.SetString(string(AppendNumber(buf[:0], f)))
	default:
		return false
	}
	return true
}

// togoNumberInto stores a finite number as Unmarshal stores the text
// json.Marshal writes for what ToGo gives for it: an int64, written with all
// its digits, for an integer of int64's range but -0, and a float64
// otherwise, written with its shortest digits (appendGoJSONFloat). Either
// text reads back as the number itself, -0 included, into a float64.
func togoNumberInto(f float64, rv reflect.Value, t reflect.Type) bool {
	if f == math.Trunc(f) && f >= -1<<63 && f < 1<<63 && (f != 0 || !math.Signbit(f)) {
		n := int64(f)
		switch rv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if rv.OverflowInt(n) {
				return false
			}
			rv.SetInt(n)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			if n < 0 || rv.OverflowUint(uint64(n)) {
				return false
			}
			rv.SetUint(uint64(n))
		case reflect.Float64:
			rv.SetFloat(f)
		case reflect.Float32:
			rv.SetFloat(float64(float32(f))) // the digits are f exactly: one rounding
		case reflect.String:
			if t != reflect.TypeFor[json.Number]() {
				return false
			}
			rv.SetString(strconv.FormatInt(n, 10))
		default:
			return false
		}
		return true
	}
	var buf [32]byte
	s := appendGoJSONFloat(buf[:0], f)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(string(s), 10, 64) // only "-0" parses
		if err != nil || rv.OverflowInt(n) {
			return false
		}
		rv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := strconv.ParseUint(string(s), 10, 64)
		if err != nil || rv.OverflowUint(n) {
			return false
		}
		rv.SetUint(n)
	case reflect.Float64:
		rv.SetFloat(f)
	case reflect.Float32:
		g, err := strconv.ParseFloat(string(s), 32)
		if err != nil || rv.OverflowFloat(g) {
			return false
		}
		rv.SetFloat(g)
	case reflect.String:
		if t != reflect.TypeFor[json.Number]() {
			return false
		}
		rv.SetString(string(s))
	default:
		return false
	}
	return true
}

// appendGoJSONFloat appends f as encoding/json writes a float64: its
// shortest digits, in exponent form below 1e-6 and from 1e21 on, with no
// leading zero in a negative exponent.
func appendGoJSONFloat(b []byte, f float64) []byte {
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 64)
	if n := len(b); format == 'e' && n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
		b[n-2] = b[n-1] // e-07 to e-7
		b = b[:n-1]
	}
	return b
}

// object stores the members of the plain object o in a struct or a map,
// in AppendJSON's order (the shape's), as Unmarshal meets them.
func (d *jsonDecoder) object(o *Object, rv reflect.Value, jt *jsonType, depth int) bool {
	isMap := rv.Kind() == reflect.Map
	if isMap {
		if jt.mapKeys {
			return false
		}
		if rv.IsNil() {
			rv.Set(reflect.MakeMap(rv.Type()))
		}
	} else if jt.byExact == nil {
		return false
	}
	var elem reflect.Value
	return jsonMembers(o, func(key PropertyKey, v Value) bool {
		if k, ok := d.kind(v); !ok {
			return false
		} else if k == jkOmitted {
			return true
		}
		name := key.String().GoString()
		if isMap {
			if !elem.IsValid() {
				elem = reflect.New(rv.Type().Elem()).Elem()
			} else {
				elem.SetZero()
			}
			if !d.value(v, elem, depth+1) {
				return false
			}
			k := reflect.New(rv.Type().Key()).Elem()
			k.SetString(name)
			rv.SetMapIndex(k, elem)
			return true
		}
		if f := jt.field(name); f >= 0 {
			return d.value(v, rv.Field(f), depth+1)
		}
		return true // a member with no field: plain checked it
	})
}

// togoObject is object for ToGoInto. Unmarshal meets the members in the
// order json.Marshal writes the keys of ToGo's map in, by their Go strings,
// and of the members whose keys have one Go string only the last (ToGo's
// map holds it): the walk takes them in that order too, so that what it
// writes before it stops is what the round trip writes first, also when
// the round trip meets an error further on (at a type that unmarshals
// itself or a json.Number from a string, where the walk stops too: the
// older encoding/json ends Unmarshal there, Go 1.27's goes on with the
// other members), and two members that go to one field, by case folding,
// are written in the round trip's order.
func (d *jsonDecoder) togoObject(o *Object, rv reflect.Value, jt *jsonType, depth int) bool {
	isMap := rv.Kind() == reflect.Map
	if isMap {
		if jt.mapKeys {
			return false
		}
		if rv.IsNil() {
			rv.Set(reflect.MakeMap(rv.Type()))
		}
	} else if jt.byExact == nil {
		return false
	}
	var buf [16]togoMember
	var elem reflect.Value
	for _, m := range togoMembers(o, buf[:0]) {
		if isMap {
			if !elem.IsValid() {
				elem = reflect.New(rv.Type().Elem()).Elem()
			} else {
				elem.SetZero()
			}
			if !d.value(m.v, elem, depth+1) {
				return false
			}
			k := reflect.New(rv.Type().Key()).Elem()
			k.SetString(m.key)
			rv.SetMapIndex(k, elem)
		} else if f := jt.field(m.key); f >= 0 && !d.value(m.v, rv.Field(f), depth+1) {
			return false
		}
	}
	return true
}

type togoMember struct {
	key string
	v   Value
}

// togoMembers appends the members of the plain object o to ms in the order
// of togoObject: sorted by key (a stable sort, of a shape that is sorted
// already for an object from a Go map), the last of those with one key.
func togoMembers(o *Object, ms []togoMember) []togoMember {
	jsonMembers(o, func(key PropertyKey, v Value) bool {
		ms = append(ms, togoMember{key.String().GoString(), v})
		return true
	})
	slices.SortStableFunc(ms, func(a, b togoMember) int { return strings.Compare(a.key, b.key) })
	out := ms[:0]
	for i, m := range ms {
		if i+1 < len(ms) && ms[i+1].key == m.key {
			continue
		}
		out = append(out, m)
	}
	return out
}

// plain reports whether AppendJSON writes v without running JavaScript or
// failing, down to its last member. Unmarshal checks all of v before
// writing anything: the round trip serializes every part, also those the
// target discards, so a nested BigInt, a cycle, a toJSON that throws or a
// getter anywhere decides the result, and so does the length of the text
// (size). A host placeholder's Go value is checked too, by hostPlain, or by
// the copy hostCopy makes of it when the target is likely to hold it in an
// interface (copyHosts): the walk then takes that copy instead of making
// it again.
func (d *jsonDecoder) plain(v Value, depth int) bool {
	if depth >= jsonScanDepth || !d.tick(0) {
		return false
	}
	k, ok := d.kind(v)
	switch {
	case !ok:
		return false
	case k == jkObject:
		if !d.within(2) {
			return false
		}
		return jsonMembers(v.AsObject(), func(key PropertyKey, e Value) bool {
			d.size += 6*int64(key.String().n) + 4 // quoted, a colon and a comma
			return d.plain(e, depth+1)
		})
	case k == jkArray:
		a := v.AsObject()
		if int(a.internal.(*ArrayData).length) != len(a.elements) || a.dict != nil && len(a.dict.sparse) != 0 {
			return false
		}
		if !d.within(2 + int64(len(a.elements))) {
			return false
		}
		for _, e := range a.elements {
			if e.IsHole() || !d.plain(e, depth+1) {
				return false
			}
		}
	case k == jkString:
		s := v.AsString()
		d.size += jsonQuotedMax(s)
	case k == jkHost && !d.copyHosts:
		return d.hostPlain(v.AsObject().hostSrc(), depth)
	case k == jkHost:
		g, ok := d.hostCopy(v.AsObject().hostSrc(), depth)
		d.hosts = append(d.hosts, jsonHostCopy{o: v.AsObject(), g: g})
		return ok
	default:
		d.size += jsonScalarMax
	}
	return true
}

// tick counts a node and its n members (those not visited one by one) and
// checks the interrupt every 4096 nodes, as AppendJSON's tick does, so a
// pending interrupt fails a small value in neither. A Go string hostCopy
// copies counts a node per 4096 bytes its check of UTF-8 scans, so that
// the copy of a large host value stays interruptible: hostCopy's strings, a
// map[string]string's values and hostStrings' elements tick where they are
// checked, inline.
func (d *jsonDecoder) tick(n int) bool {
	if d.left -= n + 1; d.left > 0 {
		return true
	}
	return d.checkpoint()
}

// checkpoint is the check of tick and open every 4096 nodes: the interrupt,
// and the bound for open. It is out of line, where it costs the walks no
// registers.
//
//go:noinline
func (d *jsonDecoder) checkpoint() bool {
	d.left = 4096
	if !d.togo { // ToGo observes no interrupt
		d.err = d.r.CheckInterrupt()
	}
	return d.err == nil && d.size <= int64(maxStringLength)
}

// within adds n bytes to size and reports whether it is still within the
// string length limit. plain and the host walks check it once a container,
// before its members: a leaf adds at most its own text to what the check
// allowed.
func (d *jsonDecoder) within(n int64) bool {
	d.size += n
	return d.size <= int64(maxStringLength)
}

// open is tick and within for a container of the host walks: n members, of
// size bytes of brackets and separators.
func (d *jsonDecoder) open(n int, size int64) bool {
	d.size += size
	if d.left -= n + 1; d.left > 0 {
		return d.size <= int64(maxStringLength)
	}
	return d.checkpoint()
}

// jsonScalarMax is the most bytes AppendJSON writes for a number, true,
// false or null: AppendNumber's longest text is a negative number of 17
// digits with five zeros after the point, -0.0000012345678901234567, 25
// bytes (an exponent form is at most 24: -2.2250738585072014e-308).
const jsonScalarMax = 25

// jsonQuotedMax is the most bytes AppendJSON writes for s quoted: six a code
// unit (\uXXXX), one for a string known plain.
// A lazy string's length is known without decoding it.
func jsonQuotedMax(s *String) int64 {
	if s.isLazy() {
		if s.plainLazyJSON() {
			return int64(s.n) + 2
		}
		return 6*int64(s.n) + 2
	}
	if s.json == jsonPlain {
		return int64(s.n) + 2
	}
	return 6*int64(s.n) + 2
}

// jsonGoQuotedMax is jsonQuotedMax for the Go string g, six bytes a byte:
// FromGoString(g) has no more code units than g has bytes.
func jsonGoQuotedMax(g string) int64 { return 6*int64(len(g)) + 2 }

// hostAny is the copy of the placeholder o that plain made. The walk meets
// the placeholders in plain's order, skipping those the target discards;
// another copy is made for one plain did not keep.
func (d *jsonDecoder) hostAny(o *Object, depth int) (any, bool) {
	for i := d.next; i < len(d.hosts); i++ {
		if d.hosts[i].o == o {
			d.next = i + 1
			return d.hosts[i].g, true
		}
	}
	return d.hostCopy(o.hostSrc(), depth)
}

// jsonMembers calls f with the enumerable string-keyed own data properties
// of the plain object o (no index keys) in AppendJSON's order, its shape's
// or its dictionary's, until f returns false; it reports false then and for
// an accessor.
func jsonMembers(o *Object, f func(key PropertyKey, v Value) bool) bool {
	if o.flags&flagDict != 0 {
		for i := range o.dict.entries {
			e := &o.dict.entries[i]
			if !e.live || e.cell.attrs&attrEnumerable == 0 || !e.key.IsString() {
				continue
			}
			if e.cell.attrs&attrAccessor != 0 || !f(e.key, e.cell.value) {
				return false
			}
		}
		return true
	}
	for i, p := range o.shape.Props() {
		if p.attrs&attrEnumerable == 0 || !p.key.IsString() {
			continue
		}
		if p.attrs&attrAccessor != 0 || !f(p.key, o.slots[i]) {
			return false
		}
	}
	return true
}

// array stores the elements of the dense array a in a slice or array, as
// Unmarshal's array does: a slice grows by Grow(1) and keeps the elements
// in its backing array, which are decoded into.
func (d *jsonDecoder) array(a *Object, rv reflect.Value, depth int) bool {
	n := int(a.internal.(*ArrayData).length)
	if n != len(a.elements) || a.dict != nil && len(a.dict.sparse) != 0 {
		return false
	}
	slice := rv.Kind() == reflect.Slice
	i := 0
	for ; i < n; i++ {
		e := a.elements[i]
		if e.IsHole() {
			return false
		}
		if slice {
			if i >= rv.Cap() {
				rv.Grow(1)
			}
			if i >= rv.Len() {
				rv.SetLen(i + 1)
			}
		}
		// Past a fixed array the element is discarded; plain checked it.
		if i < rv.Len() && !d.value(e, rv.Index(i), depth+1) {
			return false
		}
	}
	if i < rv.Len() {
		if slice {
			rv.SetLen(i)
		} else {
			for ; i < rv.Len(); i++ {
				rv.Index(i).SetZero()
			}
		}
	}
	if n == 0 && slice {
		rv.Set(reflect.MakeSlice(rv.Type(), 0, 0))
	}
	return len(a.elements) == n
}

// any is the value Unmarshal stores in an empty interface: map[string]any,
// []any, float64, string, bool or nil.
func (d *jsonDecoder) any(v Value, k int, depth int) (any, bool) {
	switch k {
	case jkNull, jkOmitted:
		return nil, true
	case jkBool:
		return v.AsBool(), true
	case jkNumber:
		if d.togo {
			return v.AsNumber(), true // -0 reads back as itself
		}
		return v.AsNumber() + 0, true
	case jkString:
		return v.AsString().GoString(), true
	case jkHost:
		return d.hostAny(v.AsObject(), depth)
	}
	if depth >= jsonScanDepth {
		return nil, false
	}
	o := v.AsObject()
	if k == jkArray {
		n := int(o.internal.(*ArrayData).length)
		if n != len(o.elements) || o.dict != nil && len(o.dict.sparse) != 0 || !d.tick(n) {
			return nil, false
		}
		out := make([]any, n)
		for i, e := range o.elements {
			if e.IsHole() {
				return nil, false
			}
			ek, ok := d.kind(e)
			if !ok {
				return nil, false
			}
			if out[i], ok = d.any(e, ek, depth+1); !ok {
				return nil, false
			}
		}
		return out, true
	}
	n := o.keyCountHint()
	if !d.tick(n) {
		return nil, false
	}
	out := make(map[string]any, n)
	ok := jsonMembers(o, func(key PropertyKey, e Value) bool {
		ek, ok := d.kind(e)
		if !ok {
			return false
		}
		if ek == jkOmitted {
			return true
		}
		g, ok := d.any(e, ek, depth+1)
		out[key.String().GoString()] = g
		return ok
	})
	return out, ok
}

// hostCopy is what Unmarshal stores in an empty interface for the Go value
// of a host placeholder: a copy in its JSON types (numbers as float64, every
// container new), as the text of the converted value reads back. ok is false
// for what the stringifier's direct walk gives up on, for a string or key
// that is not valid UTF-8, on an interrupt and once size passes the string
// length limit; hostPlain counts the size of a value as hostCopy does. For
// ToGoInto (togo) the text is json.Marshal's of the Go value itself: a nil
// slice is null, a float32 is written with a float32's shortest digits, and
// json.Marshal fails on a NaN or an infinity.
func (d *jsonDecoder) hostCopy(v any, depth int) (any, bool) {
	if depth >= jsonScanDepth {
		return nil, false
	}
	switch x := v.(type) {
	case string:
		d.size += jsonGoQuotedMax(x)
		if len(x) >= 4096 && !d.tick(len(x)>>12-1) { // a node per 4096 bytes (tick)
			return nil, false
		}
		return x, utf8.ValidString(x)
	case float64:
		d.size += jsonScalarMax
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, !d.togo // json.Marshal fails on it
		}
		if d.togo {
			return x, true
		}
		return x + 0, true
	case float32:
		d.size += jsonScalarMax
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, !d.togo
		}
		if d.togo {
			// json.Marshal writes a float32's shortest digits as a float32,
			// which read back as a float64.
			var buf [32]byte
			g, err := strconv.ParseFloat(string(strconv.AppendFloat(buf[:0], f, 'g', -1, 32)), 64)
			return g, err == nil
		}
		return f + 0, true
	case bool:
		d.size += jsonScalarMax
		return x, true
	case nil:
		d.size += jsonScalarMax
		return nil, true
	case map[string]any:
		if x == nil {
			d.size += 4
			return nil, true
		}
		return jsonHostMap(d, x, depth, func(e any) (any, bool) { return d.hostCopy(e, depth+1) })
	case []any:
		if x == nil && d.togo {
			d.size += 4
			return nil, true // json.Marshal writes null
		}
		if !d.open(len(x), 2+int64(len(x))) {
			return nil, false
		}
		out := make([]any, len(x))
		for i, e := range x {
			var ok bool
			if out[i], ok = d.hostCopy(e, depth+1); !ok {
				return nil, false
			}
		}
		return out, true
	case map[string]string:
		if x == nil {
			d.size += 4
			return nil, true
		}
		return jsonHostMap(d, x, depth, func(s string) (any, bool) {
			d.size += jsonGoQuotedMax(s)
			if len(s) >= 4096 && !d.tick(len(s)>>12-1) {
				return nil, false
			}
			return s, utf8.ValidString(s)
		})
	case []string:
		return d.hostStrings(x)
	case map[string][]string:
		if x == nil {
			d.size += 4
			return nil, true
		}
		return jsonHostMap(d, x, depth, d.hostStrings)
	case []map[string]any:
		if x == nil && d.togo {
			d.size += 4
			return nil, true
		}
		if !d.open(len(x), 2+int64(len(x))) {
			return nil, false
		}
		out := make([]any, len(x))
		for i, m := range x {
			var ok bool
			if out[i], ok = d.hostCopy(m, depth+1); !ok {
				return nil, false
			}
		}
		return out, true
	case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
		d.size += jsonScalarMax
		n, _, _ := scalarFromGo(x)
		return n.AsNumber(), true
	}
	return nil, false
}

// hostPlain reports whether the Go value of a host placeholder is made of
// JSON's types only (strings in any encoding, the common number kinds), so
// that AppendJSON writes it without failing; false (the round trip runs) is
// always safe. It counts the value in size and ticks as hostCopy does, but
// for a string's bytes, which it does not scan.
func (d *jsonDecoder) hostPlain(v any, depth int) bool {
	if depth >= jsonScanDepth {
		return false
	}
	switch x := v.(type) {
	case string:
		d.size += jsonGoQuotedMax(x)
	case float64:
		d.size += jsonScalarMax
		if d.togo && (math.IsNaN(x) || math.IsInf(x, 0)) {
			return false // json.Marshal fails on it
		}
	case bool, nil, int, int64, uint64:
		d.size += jsonScalarMax
	case []string:
		return d.stringsPlain(x)
	case map[string]string:
		// Four bytes for the braces, or for null when the map is nil.
		if !d.open(len(x), 4) {
			return false
		}
		for k, s := range x {
			d.size += jsonGoQuotedMax(k) + 2 + jsonGoQuotedMax(s)
		}
	case map[string][]string:
		if !d.open(len(x), 4) {
			return false
		}
		for k, list := range x {
			if d.size += jsonGoQuotedMax(k) + 2; !d.stringsPlain(list) {
				return false
			}
		}
	case map[string]any:
		if !d.open(len(x), 4) {
			return false
		}
		for k, e := range x {
			if d.size += jsonGoQuotedMax(k) + 2; !d.hostPlain(e, depth+1) {
				return false
			}
		}
	case []any:
		if !d.open(len(x), 2+int64(len(x))) {
			return false
		}
		for _, e := range x {
			if !d.hostPlain(e, depth+1) {
				return false
			}
		}
	case []map[string]any:
		if !d.open(len(x), 2+int64(len(x))) {
			return false
		}
		for _, m := range x {
			if !d.hostPlain(m, depth+1) {
				return false
			}
		}
	case json.RawMessage:
		return d.rawPlain(x)
	case rawText: // a RawMessage placeholder's text, checked when it was made
		d.size += 6*int64(len(x)) + 4
	default:
		return false
	}
	return true
}

// rawPlain is hostPlain of a json.RawMessage: plain when the parser accepts
// it (or it is nil, null), so that AppendJSON writes the value JSON.parse
// gives for it and json.Marshal compacts it without failing. Its text is at
// most six times as long either way: an escape of six bytes stands for one
// byte or more (a <, an invalid byte, which is U+FFFD), and the longest
// rewriting of a number (1e20, 21 digits) is less than six times as long.
func (d *jsonDecoder) rawPlain(raw json.RawMessage) bool {
	d.size += 6*int64(len(raw)) + 4
	if raw == nil {
		return true
	}
	if len(raw) >= 4096 && !d.tick(len(raw)>>12-1) { // a node per 4096 bytes scanned (tick)
		return false
	}
	return jsonValidRaw(raw)
}

// jsonHostMap copies m for hostCopy, its values converted by conv.
func jsonHostMap[V any](d *jsonDecoder, m map[string]V, depth int, conv func(V) (any, bool)) (any, bool) {
	if depth+1 >= jsonScanDepth || !d.open(len(m), 4) {
		return nil, false
	}
	out := make(map[string]any, len(m))
	for k, e := range m {
		if !utf8.ValidString(k) {
			return nil, false
		}
		d.size += jsonGoQuotedMax(k) + 2 // a colon and a comma
		g, ok := conv(e)
		if !ok {
			return nil, false
		}
		out[k] = g
	}
	return out, true
}

// hostStrings copies a []string for hostCopy.
func (d *jsonDecoder) hostStrings(list []string) (any, bool) {
	if list == nil && d.togo {
		d.size += 4
		return nil, true // json.Marshal writes null
	}
	if !d.open(len(list), 2+int64(len(list))) {
		return nil, false
	}
	out := make([]any, len(list))
	for i, s := range list {
		if len(s) >= 4096 && !d.tick(len(s)>>12-1) {
			return nil, false
		}
		if !utf8.ValidString(s) {
			return nil, false
		}
		d.size += jsonGoQuotedMax(s)
		out[i] = s
	}
	return out, true
}

// stringsPlain counts a []string for hostPlain.
func (d *jsonDecoder) stringsPlain(list []string) bool {
	if !d.open(len(list), 2+int64(len(list))) {
		return false
	}
	for _, s := range list {
		d.size += jsonGoQuotedMax(s)
	}
	return true
}

// --- target types --------------------------------------------------------------

// jsonType is what the walk needs of a target type.
type jsonType struct {
	// custom: the type or a pointer to it unmarshals itself, but for a
	// pointer to a json.RawMessage, which the walk follows.
	custom bool
	// raw: the type is json.RawMessage (custom too, so that the walk's
	// check of custom finds it): it receives the text of its value.
	raw bool
	// mapKeys: a map whose key type is not a string kind or unmarshals
	// itself from text (Unmarshal parses or decodes such keys).
	mapKeys bool
	// byExact and byFold index the fields of a struct by encoding/json's
	// rules; byExact is nil for a struct with an embedded field or a tag
	// that jsonStructFields leaves to the round trip.
	byExact map[string]int
	byFold  map[string]int
	// holdsAny: jsonHoldsAny.
	holdsAny bool
}

var jsonTypes sync.Map // reflect.Type -> *jsonType

func jsonTypeOf(t reflect.Type) *jsonType {
	if jt, ok := jsonTypes.Load(t); ok {
		return jt.(*jsonType)
	}
	jt := &jsonType{}
	pt := reflect.PointerTo(t)
	u, tu := reflect.TypeFor[json.Unmarshaler](), reflect.TypeFor[encoding.TextUnmarshaler]()
	switch t {
	case reflect.TypeFor[json.RawMessage]():
		jt.custom, jt.raw = true, true
	case reflect.TypeFor[*json.RawMessage]():
	default:
		jt.custom = t.Implements(u) || t.Implements(tu) || pt.Implements(u) || pt.Implements(tu)
	}
	switch t.Kind() {
	case reflect.Map:
		kt := t.Key()
		jt.mapKeys = kt.Kind() != reflect.String || reflect.PointerTo(kt).Implements(tu)
	case reflect.Struct:
		jt.byExact, jt.byFold = jsonStructFields(t)
	}
	jt.holdsAny = jsonHoldsAny(t)
	actual, _ := jsonTypes.LoadOrStore(t, jt)
	return actual.(*jsonType)
}

// jsonHoldsAny reports whether t or what it holds directly (its element,
// a field), through pointers, is an empty interface. It is a hint: plain
// copies a host placeholder for an interface the walk is likely to store
// it in, and the walk makes the copy itself otherwise.
func jsonHoldsAny(t reflect.Type) bool {
	t = jsonDeref(t)
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		t = jsonDeref(t.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			if f := jsonDeref(t.Field(i).Type); f.Kind() == reflect.Interface && f.NumMethod() == 0 {
				return true
			}
		}
	}
	return t.Kind() == reflect.Interface && t.NumMethod() == 0
}

func jsonDeref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// jsonStructFields indexes the fields Unmarshal decodes into: exported, not
// tagged "-", named by the tag or, when it has no name, the field name; of
// several fields with one name, the only tagged one, or none. byFold holds
// the first field of each folded name. Both are nil, for the round trip,
// when a field is embedded or the tag of one Unmarshal decodes into has a
// name the older encoding/json rejects or options jsonOptsIgnored does not
// pass: the walk does not handle ",string", and Go 1.27's encoding/json,
// built on its v2 code, takes such a name or a part of it and reads
// options the older one ignores (case:strict, embed, format).
func jsonStructFields(t reflect.Type) (byExact, byFold map[string]int) {
	var names []string
	var index []int
	var tagged []bool
	for i := range t.NumField() {
		sf := t.Field(i)
		if sf.Anonymous {
			return nil, nil
		}
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := cutTag(tag)
		if name != "" && !jsonValidTag(name) || !jsonOptsIgnored(opts) {
			return nil, nil
		}
		named := name != ""
		if !named {
			name = sf.Name
		}
		names, index, tagged = append(names, name), append(index, i), append(tagged, named)
	}
	byExact = make(map[string]int, len(names))
	byFold = make(map[string]int, len(names))
	for i, name := range names {
		// Of the fields that share a name, the one tagged field wins, else none.
		same, taggedSame := 0, 0
		for j, other := range names {
			if other == name {
				same++
				if tagged[j] {
					taggedSame++
				}
			}
		}
		if same > 1 && !(tagged[i] && taggedSame == 1) {
			continue
		}
		byExact[name] = index[i]
		if _, ok := byFold[jsonFoldName(name)]; !ok {
			byFold[jsonFoldName(name)] = index[i]
		}
	}
	return byExact, byFold
}

// field returns the index of the field a member named key goes to, or -1.
func (jt *jsonType) field(key string) int {
	if i, ok := jt.byExact[key]; ok {
		return i
	}
	if i, ok := jt.byFold[jsonFoldName(key)]; ok {
		return i
	}
	return -1
}

func cutTag(tag string) (name, opts string, found bool) {
	for i := range len(tag) {
		if tag[i] == ',' {
			return tag[:i], tag[i+1:], true
		}
	}
	return tag, "", false
}

// jsonOptsIgnored reports whether opts, the options of a json tag, are ones
// every implementation of encoding/json ignores in Unmarshal: empty ones,
// case:ignore (which matches as without it) and words of ASCII letters,
// digits and underscores, which the v2 code reads whole, but string, case,
// embed, format, and inline and unknown (read by Go 1.25 and 1.26 with
// GOEXPERIMENT=jsonv2). A later Go may read other words.
func jsonOptsIgnored(opts string) bool {
	for opts != "" {
		var o string
		o, opts, _ = cutTag(opts)
		switch o {
		case "", "case:ignore":
			continue
		case "string", "case", "embed", "format", "inline", "unknown":
			return false
		}
		for i := range len(o) {
			if c := o[i]; !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_') {
				return false
			}
		}
	}
	return true
}

// jsonValidTag is encoding/json's isValidTag.
func jsonValidTag(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
			// Backslash and quote chars are reserved.
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// jsonFoldName is encoding/json's foldName: ASCII letters upper case, other
// runes the smallest of their fold set.
func jsonFoldName(s string) string {
	ascii := true
	for i := range len(s) {
		if c := s[i]; c >= utf8.RuneSelf || 'a' <= c && c <= 'z' {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r < utf8.RuneSelf {
			if 'a' <= r && r <= 'z' {
				r -= 'a' - 'A'
			}
			out = append(out, byte(r))
			continue
		}
		for {
			r2 := unicode.SimpleFold(r)
			if r2 <= r {
				r = r2
				break
			}
			r = r2
		}
		out = utf8.AppendRune(out, r)
	}
	return string(out)
}

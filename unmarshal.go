package moejs

import (
	"encoding/json"

	"github.com/MoeForge/moejs/engine"
)

// Unmarshal stores v in the value target points to as json.Unmarshal stores
// the text AppendJSON writes for v, and returns the error json.Unmarshal (or
// AppendJSON) would. A value made of plain objects, arrays, strings,
// numbers, booleans and null, and arguments FromGo converted that it
// returns untouched (in an `any` of target), goes into target without the
// text: no JSON is written or parsed, and strings are not copied (the Go
// string of an ASCII string is the one v holds, as with ToGo). A
// json.RawMessage in target (a field, a map value, an element, target
// itself) receives AppendJSON's text of its value, as json.Unmarshal would
// give it, written into the RawMessage's own array when it fits; an untouched
// RawMessage argument that JavaScript returns passes through without a parse
// on the Go side. Anything else
// — a toJSON method, a getter, a proxy, a Date, a target type that
// unmarshals itself, a value target's type rejects — makes it fall back to
// AppendJSON and json.Unmarshal, with their result and error; JavaScript
// code then runs as AppendJSON runs it. When AppendJSON would fail (a
// BigInt, a cycle, a Go value it cannot write, a text past the string length
// limit), target is left as it was: v is checked, its Go values included,
// before anything is written, and a value whose text could pass the limit
// (counting six bytes a character of most strings) goes through the round
// trip. An interrupt is observed every 4096 values, as AppendJSON observes
// it, in the check and again in the store; an argument copied into an
// `any` counts a value per 4096 bytes of each of its Go strings, whether a
// string itself or in a map[string]string, a []string or another of its
// containers. One pending at the call fails a large value before anything
// is written, and not a small one. AppendJSON also observes one inside a
// long JavaScript string it escapes, which Unmarshal, copying no string,
// does not. After an error of json.Unmarshal (a value target's type
// rejects) or an interrupt, target may hold part of v, as with
// json.Unmarshal. A string of a value ParseJSON or ParseJSONString
// produced may share the parsed text and keep it alive for as long as the
// host holds it: strings.Clone a string kept beyond the request.
func (rt *Runtime) Unmarshal(v Value, target any) (err error) {
	ok, err := rt.realm.Unmarshal(v, target)
	if ok || err != nil {
		return err
	}
	data, err := rt.AppendJSON(nil, v)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// ToGoInto stores v in the value target points to as json.Unmarshal stores
// the text json.Marshal writes for what ToGo returns for v, and returns the
// error ToGo, json.Marshal or json.Unmarshal would. A value made of
// ordinary objects (class instances and objects with a null prototype
// included), arrays, strings, numbers, booleans, null and undefined, and
// arguments FromGo converted that it returns untouched (in an `any` of
// target), goes into target without the round trip: no Go map or slice of
// ToGo is built, no JSON is written or parsed, and strings are not copied.
// A json.RawMessage in target receives json.Marshal's text of ToGo's value
// for its part of v (keys sorted, <, > and & escaped), the bytes the round
// trip gives it, without the round trip for the rest; an untouched
// RawMessage argument passes through as json.Marshal compacts it.
// Anything else (a getter, a proxy, a Date, a Map, a typed array, a BigInt,
// a function, a target type that unmarshals itself, a value target's type
// rejects) makes it fall back to ToGo, json.Marshal and json.Unmarshal,
// with their result and error. When ToGo or json.Marshal fails (a getter
// that throws, a NaN or an infinity, a cycle), target is left as it was.
// After an error of json.Unmarshal, target may hold part of v, as with
// json.Unmarshal.
//
// The result differs from Unmarshal's where ToGo's Go value differs from
// AppendJSON's text: a member whose value is undefined is kept, as null;
// -0 stays -0; a NaN or an infinity is json.Marshal's error; toJSON is not
// called; an argument FromGo converted that JavaScript has not modified is
// its Go value as json.Marshal writes it (a nil slice as null). No
// interrupt is observed, as ToGo observes none for such a value. A string
// of a value ParseJSON or ParseJSONString produced may share the parsed
// text and keep it alive for as long as the host holds it: strings.Clone a
// string kept beyond the request.
func (rt *Runtime) ToGoInto(v Value, target any) error {
	if rt.realm.ToGoInto(v, target) {
		return nil
	}
	g, err := rt.ToGo(v)
	if err != nil {
		return err
	}
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// DecodeOptions bounds the JSON text of a decode of a JavaScript value into
// Go (UnmarshalWith, ToGoIntoWith, ToGoWith): MaxBytes its length in bytes,
// MaxNodes the values in it (each object, array, string, number, boolean
// and null; keys do not count). The text is the one the decode's round trip
// writes: AppendJSON's text of the value for UnmarshalWith, json.Marshal's
// text of what ToGo returns for ToGoIntoWith and ToGoWith, counted exactly,
// whether or not the decode writes it. A zero field sets no limit.
type DecodeOptions = engine.DecodeOptions

// ErrTooLarge is the error, wrapped in one that names the limit, of a decode
// whose JSON text passes one of its DecodeOptions limits. A value nested
// deeper than 10,000 arrays and objects, a cycle included, passes them all.
var ErrTooLarge = engine.ErrTooLarge

// UnmarshalWith is Unmarshal with the limits of opts: when AppendJSON's text
// of v passes one, it returns an error wrapping ErrTooLarge, and target is
// left as it was. The text is counted on v before anything is decoded or
// allocated for it, without being written, for every value Unmarshal stores
// without the round trip and for the arguments FromGo converted; a value
// that is read by running JavaScript (a toJSON, a getter, a proxy, a Date)
// is counted on AppendJSON's text once that is written, before
// json.Unmarshal reads it. With no limit set it is Unmarshal.
func (rt *Runtime) UnmarshalWith(v Value, target any, opts DecodeOptions) error {
	if opts.MaxBytes <= 0 && opts.MaxNodes <= 0 {
		return rt.Unmarshal(v, target)
	}
	measured, err := rt.measure(v, opts, false)
	if err != nil {
		return err
	}
	if measured {
		return rt.Unmarshal(v, target)
	}
	// Asked before AppendJSON, whose code (a toJSON, a getter) could change
	// v: a plain value the measure did not count (a json.RawMessage in a Go
	// map) goes to the walk once its text passed, and to json.Unmarshal
	// only where the walk declines.
	direct := rt.realm.CanUnmarshalDirect(v)
	data, err := rt.AppendJSON(nil, v)
	if err != nil {
		return err
	}
	if err := engine.MeasureDecodeText(data, opts); err != nil {
		return err
	}
	if direct {
		if ok, err := rt.realm.Unmarshal(v, target); ok || err != nil {
			return err
		}
	}
	return json.Unmarshal(data, target)
}

// ToGoIntoWith is ToGoInto with the limits of opts: when json.Marshal's text
// of what ToGo returns for v passes one, it returns an error wrapping
// ErrTooLarge, and target is left as it was. The text is counted on v
// before anything is decoded or allocated for it, without being written,
// for every value ToGoInto stores without the round trip and for the
// arguments FromGo converted; a value that is read by running JavaScript (a
// getter, a proxy) or that ToGo converts to another kind of Go value (a
// Date, a Map, a typed array) is counted on ToGo's result, before
// json.Marshal writes it. With no limit set it is ToGoInto.
func (rt *Runtime) ToGoIntoWith(v Value, target any, opts DecodeOptions) error {
	if opts.MaxBytes <= 0 && opts.MaxNodes <= 0 {
		return rt.ToGoInto(v, target)
	}
	measured, err := rt.measure(v, opts, true)
	if err != nil {
		return err
	}
	if measured {
		return rt.ToGoInto(v, target)
	}
	direct := rt.realm.CanToGoIntoDirect(v) // before ToGo runs a getter, as for UnmarshalWith
	g, err := rt.ToGo(v)
	if err != nil {
		return err
	}
	if err := engine.MeasureDecodeGo(g, opts); err != nil {
		return err
	}
	if direct && rt.realm.ToGoInto(v, target) {
		return nil
	}
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// ToGoWith is ToGo with the limits of opts, which bound json.Marshal's text
// of the result: when it passes one, ToGoWith returns nil and an error
// wrapping ErrTooLarge. The text is counted on v before ToGo builds
// anything, as for ToGoIntoWith, and on ToGo's result for a value that is
// read by running JavaScript or that ToGo converts to another kind of Go
// value. A NaN or an infinity, which json.Marshal fails on, counts as null,
// as does a Go value json.Marshal cannot write. With no limit set it is
// ToGo.
func (rt *Runtime) ToGoWith(v Value, opts DecodeOptions) (any, error) {
	if opts.MaxBytes <= 0 && opts.MaxNodes <= 0 {
		return rt.ToGo(v)
	}
	measured, err := rt.measure(v, opts, true)
	if err != nil {
		return nil, err
	}
	g, err := rt.ToGo(v)
	if err != nil || measured {
		return g, err
	}
	if err := engine.MeasureDecodeGo(g, opts); err != nil {
		return nil, err
	}
	return g, nil
}

// measure is MeasureToGo when togo is set, else MeasureUnmarshal. It runs
// no JavaScript; the guard turns an engine bug into an *InternalError.
func (rt *Runtime) measure(v Value, opts DecodeOptions, togo bool) (measured bool, err error) {
	r := rt.realm
	defer rt.guard(&err, r.CallState(), len(rt.argStack))
	if togo {
		return r.MeasureToGo(v, opts)
	}
	return r.MeasureUnmarshal(v, opts)
}

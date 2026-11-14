package engine

import (
	"encoding"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/MoeForge/moego/json"
)

// FromGo of the types its type switches do not name. They are rare in a hook
// argument, so they are converted here, after every switch has missed, and
// the switches cost the common types nothing more.
//
//   - A named type whose underlying type FromGo converts (type Settings
//     map[string]any, type Status string, type Files []map[string]any) is
//     converted as a value of that underlying type: a named map or slice
//     lazily, as the unnamed one.
//   - A json.RawMessage is JSON text, parsed when JavaScript first reads
//     it (hostraw.go).
//   - Any other type encoding/json writes (a struct, a pointer, a map or
//     slice of other element types, a type with a MarshalJSON or MarshalText
//     method) is converted from its JSON text: json.Marshal, then the
//     engine's own parser. The result is a snapshot, made when FromGo (or
//     the materialization of the container holding the value) runs, and is
//     what JSON.parse gives for that text: struct tags, omitempty and the
//     Marshal methods apply, a []byte inside is a base64 string, a NaN is
//     json.Marshal's error.
//   - A channel, a function other than a NativeFunc, a complex number and
//     an unsafe.Pointer stay an error, and so do, unless they have a
//     MarshalJSON or MarshalText method, an error (json.Marshal writes most
//     as {}) and a struct, or a pointer to one, with fields of which
//     json.Marshal writes none (a sync.Mutex, a context).

var (
	fromGoMarshalerType     = reflect.TypeFor[json.Marshaler]()
	fromGoTextMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	fromGoErrorType         = reflect.TypeFor[error]()
)

// fromGoTyped converts v, which no type switch of FromGo names; h is the heap
// of the container being materialized, nil for a top-level FromGo.
func (r *Realm) fromGoTyped(v any, h *hostHeap) (Value, error) {
	if raw, ok := v.(json.RawMessage); ok {
		return r.fromRawJSON(raw, h)
	}
	rv := reflect.ValueOf(v)
	t := rv.Type()
	switch t.Kind() {
	case reflect.Chan, reflect.Func, reflect.Complex64, reflect.Complex128, reflect.UnsafePointer:
		return Undefined(), fmt.Errorf("engine: FromGo: unsupported Go type %T", v)
	}
	if !t.Implements(fromGoMarshalerType) && !t.Implements(fromGoTextMarshalerType) {
		// json.Marshal writes an error, a mutex or a context as {} or
		// another text that says nothing of it: a host that passes one by
		// mistake (map[string]any{"error": err}) gets an error, not an
		// empty object. A struct with no fields at all stays {}.
		if t.Implements(fromGoErrorType) {
			return Undefined(), fmt.Errorf("engine: FromGo: unsupported Go type %T (an error: pass err.Error())", v)
		}
		if base := fromGoBase(t); base != nil {
			g := rv.Convert(base).Interface()
			if h != nil {
				return h.child(g)
			}
			return r.FromGo(g)
		}
		if st := jsonDeref(t); st.Kind() == reflect.Struct && st.NumField() > 0 && !jsonHasFields(st, 0) {
			return Undefined(), fmt.Errorf("engine: FromGo: unsupported Go type %T (no exported fields)", v)
		}
	}
	text, err := json.Marshal(v)
	if err != nil {
		return Undefined(), fmt.Errorf("engine: FromGo: %T: %w", v, err)
	}
	// The text is FromGo's own: its strings may alias it.
	return r.parseQuiet(bytesToString(text))
}

// jsonHasFields reports whether json.Marshal can write a field of the
// struct type t: an exported field not tagged "-", or such a field of an
// embedded struct, whose fields encoding/json promotes; an embedded struct
// with a json tag name, which encoding/json writes as a named field; an
// embedded type that is not a struct counts when its name is exported.
func jsonHasFields(t reflect.Type, depth int) bool {
	if depth > 8 {
		return true // deep embedding: leave it to json.Marshal
	}
	for i := range t.NumField() {
		sf := t.Field(i)
		if sf.Tag.Get("json") == "-" {
			continue
		}
		if sf.Anonymous {
			ft := jsonDeref(sf.Type)
			if name, _, _ := cutTag(sf.Tag.Get("json")); jsonValidTag(name) && (ft.Kind() == reflect.Struct || sf.IsExported()) {
				return true // a tag name makes it a named field, as for encoding/json
			}
			if ft.Kind() == reflect.Struct {
				if jsonHasFields(ft, depth+1) {
					return true
				}
				continue
			}
		}
		if sf.IsExported() {
			return true
		}
	}
	return false
}

// fromGoBase is the unnamed type FromGo converts that t, a named type, has
// as its underlying type; nil when there is none.
func fromGoBase(t reflect.Type) reflect.Type {
	switch t.Kind() {
	case reflect.Bool:
		return reflect.TypeFor[bool]()
	case reflect.Int:
		return reflect.TypeFor[int]()
	case reflect.Int8:
		return reflect.TypeFor[int8]()
	case reflect.Int16:
		return reflect.TypeFor[int16]()
	case reflect.Int32:
		return reflect.TypeFor[int32]()
	case reflect.Int64:
		return reflect.TypeFor[int64]()
	case reflect.Uint:
		return reflect.TypeFor[uint]()
	case reflect.Uint8:
		return reflect.TypeFor[uint8]()
	case reflect.Uint16:
		return reflect.TypeFor[uint16]()
	case reflect.Uint32:
		return reflect.TypeFor[uint32]()
	case reflect.Uint64:
		return reflect.TypeFor[uint64]()
	case reflect.Float32:
		return reflect.TypeFor[float32]()
	case reflect.Float64:
		return reflect.TypeFor[float64]()
	case reflect.String:
		return reflect.TypeFor[string]()
	case reflect.Map, reflect.Slice:
		for _, base := range [...]reflect.Type{
			reflect.TypeFor[map[string]any](),
			reflect.TypeFor[map[string]string](),
			reflect.TypeFor[map[string][]string](),
			reflect.TypeFor[[]any](),
			reflect.TypeFor[[]string](),
			reflect.TypeFor[[]map[string]any](),
			reflect.TypeFor[[]byte](),
		} {
			// The same underlying type: same kind, key and element (a
			// map[string]int is none of them).
			if t.Kind() == base.Kind() && t.Elem() == base.Elem() && (t.Kind() == reflect.Slice || t.Key() == base.Key()) {
				return base
			}
		}
	}
	return nil
}

// parseQuiet is JSONParseGoString observing no interrupt: FromGo never
// returns one, and a materialization, which parses a json.RawMessage, has
// nowhere to return one to. Its allocation waits as JSONParseGoString's
// does.
func (r *Realm) parseQuiet(text string) (Value, error) {
	p := jsonParser{r: r, quiet: true}
	if isASCII(text) || utf8.ValidString(text) {
		p.knownUTF8 = true
		return p.parseText(text, nil)
	}
	return p.parse(FromGoString(text))
}

package webapi

import (
	"net/http"
	"net/textproto"
	"slices"
	"strings"

	"github.com/MoeForge/moejs/engine"
)

// The Fetch Standard's Headers. Names are kept lowercase, values as byte
// strings (Go strings of the header's bytes): a JavaScript string becomes
// one with ByteString's rules, and goes back as each byte one code unit.

const (
	guardNone uint8 = iota
	guardRequest
	guardResponse
	guardImmutable
)

// hdr is one header of a header list.
type hdr struct{ name, value string }

// headers is the state of a Headers object, or the header list of a
// request or response before JavaScript asks for its Headers object.
type headers struct {
	obj   *engine.Object
	list  []hdr
	guard uint8
	// src is the header map of a fetch response, read until the list is
	// first needed.
	src http.Header
	// version counts the changes, for the sorted list and iterators.
	version, sortedVersion int
	sorted                 []hdr
	// size is the bytes of the names and values of list, which Go holds
	// for JavaScript: the memory limit's measurements count them.
	size int
}

// materialize builds the list from src.
func (h *headers) materialize() {
	if h.src == nil {
		return
	}
	for k, vs := range h.src {
		name := strings.ToLower(k)
		for _, v := range vs {
			h.list = append(h.list, hdr{name, v})
			h.size += len(name) + len(v)
		}
	}
	h.src = nil
	h.version++
}

// mapBytes is the bytes of the names and values of a header map.
func mapBytes(m http.Header) int {
	n := 0
	for k, vs := range m {
		for _, v := range vs {
			n += len(k) + len(v)
		}
	}
	return n
}

// get returns the combined value of name (lowercase) and whether the list
// has it.
func (h *headers) get(name string) (string, bool) {
	if h.src != nil {
		vs := h.src[textproto.CanonicalMIMEHeaderKey(name)]
		if len(vs) == 0 {
			if vs = h.src[name]; len(vs) == 0 {
				return "", false
			}
		}
		if len(vs) == 1 {
			return vs[0], true
		}
		return strings.Join(vs, ", "), true
	}
	var b strings.Builder
	found := false
	for _, x := range h.list {
		if x.name == name {
			if found {
				b.WriteString(", ")
			}
			b.WriteString(x.value)
			found = true
		}
	}
	return b.String(), found
}

func (h *headers) has(name string) bool {
	if h.src != nil {
		_, ok := h.get(name)
		return ok
	}
	for _, x := range h.list {
		if x.name == name {
			return true
		}
	}
	return false
}

func (h *headers) append(name, value string) {
	h.materialize()
	h.list = append(h.list, hdr{name, value})
	h.size += len(name) + len(value)
	h.version++
}

func (h *headers) delete(name string) {
	h.materialize()
	h.list = slices.DeleteFunc(h.list, func(x hdr) bool { return h.drop(x, name) })
	h.version++
}

// drop reports whether x is a header name, and takes it out of size.
func (h *headers) drop(x hdr, name string) bool {
	if x.name != name {
		return false
	}
	h.size -= len(x.name) + len(x.value)
	return true
}

// set replaces the first header name by value and removes the others.
func (h *headers) set(name, value string) {
	h.materialize()
	for i, x := range h.list {
		if x.name == name {
			h.size += len(value) - len(x.value)
			h.list[i].value = value
			rest := h.list[i+1:]
			h.list = append(h.list[:i+1], slices.DeleteFunc(rest, func(x hdr) bool { return h.drop(x, name) })...)
			h.version++
			return
		}
	}
	h.list = append(h.list, hdr{name, value})
	h.size += len(name) + len(value)
	h.version++
}

// sortedList implements "sort and combine".
func (h *headers) sortedList() []hdr {
	h.materialize()
	if h.sorted != nil && h.sortedVersion == h.version {
		return h.sorted
	}
	l := slices.Clone(h.list)
	slices.SortStableFunc(l, func(a, b hdr) int { return strings.Compare(a.name, b.name) })
	out := l[:0:0]
	for _, x := range l {
		if n := len(out); n > 0 && out[n-1].name == x.name && x.name != "set-cookie" {
			out[n-1].value += ", " + x.value
			continue
		}
		out = append(out, x)
	}
	h.sorted, h.sortedVersion = out, h.version
	return out
}

func (h *headers) clone() *headers {
	h.materialize()
	return &headers{list: slices.Clone(h.list), guard: h.guard, size: h.size}
}

// --- names and values -----------------------------------------------------------

// isToken reports whether s is a header name (RFC 9110 token).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !tokenChar[s[i]] {
			return false
		}
	}
	return true
}

var tokenChar = func() (t [256]bool) {
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		t[c], t[c-'a'+'A'] = true, true
	}
	for _, c := range "!#$%&'*+-.^_`|~" {
		t[c] = true
	}
	return
}()

// byteString converts v with ToString to a ByteString: a code unit above
// 0xFF is a TypeError.
func (e *env) byteString(v engine.Value) (string, error) {
	s, err := e.r.ToString(v)
	if err != nil {
		return "", err
	}
	if s.IsASCII() {
		return s.GoString(), nil
	}
	n := s.Len()
	b := make([]byte, n)
	for i := range n {
		c := s.At(i)
		if c > 0xFF {
			return "", e.r.TypeError("Cannot convert argument to a ByteString because the character at index %d has a value of %d which is greater than 255.", i, c)
		}
		b[i] = byte(c)
	}
	return string(b), nil
}

// byteValue converts a byte string to a JavaScript string, each byte one
// code unit (isomorphic decode).
func byteValue(s string) engine.Value {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return str(s)
	}
	u := make([]uint16, len(s))
	for i := 0; i < len(s); i++ {
		u[i] = uint16(s[i])
	}
	return engine.StringValue(engine.FromUTF16(u))
}

// headerName reads a header name: a token, lowercased.
func (e *env) headerName(v engine.Value) (string, error) {
	s, err := e.byteString(v)
	if err != nil {
		return "", err
	}
	if !isToken(s) {
		return "", e.r.TypeError("Headers: '%s' is not a valid HTTP header field name.", s)
	}
	return lowerASCII(s), nil
}

// headerValue reads a header value, normalized: without leading and
// trailing HTTP whitespace, and with no NUL, CR or LF.
func (e *env) headerValue(v engine.Value) (string, error) {
	s, err := e.byteString(v)
	if err != nil {
		return "", err
	}
	s = strings.Trim(s, "\t\n\r ")
	if strings.ContainsAny(s, "\x00\r\n") {
		return "", e.r.TypeError("Headers: '%s' is not a valid HTTP header field value.", s)
	}
	return s, nil
}

func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return strings.ToLower(s)
		}
	}
	return s
}

// fill implements "fill" a Headers object from init: a Headers, a
// sequence of pairs, or a record.
func (e *env) fillHeaders(h *headers, init engine.Value) error {
	r := e.r
	if init.IsUndefined() {
		return nil
	}
	if !init.IsObject() {
		return r.TypeError("Failed to construct 'Headers': The provided value is not of type '(record<ByteString, ByteString> or sequence<sequence<ByteString>>)'.")
	}
	if src, ok := init.AsObject().Internal().(*headers); ok {
		for _, x := range src.sortedListRaw() {
			if err := e.appendHeader(h, x.name, x.value); err != nil {
				return err
			}
		}
		return nil
	}
	m, err := r.GetMethod(init, engine.SymbolKey(engine.SymIterator))
	if err != nil {
		return err
	}
	if !m.IsUndefined() {
		return e.iterate(init, func(pair engine.Value) error {
			var items []engine.Value
			if err := e.iterate(pair, func(v engine.Value) error {
				items = append(items, v)
				return nil
			}); err != nil {
				return err
			}
			if len(items) != 2 {
				return r.TypeError("Failed to construct 'Headers': Invalid value")
			}
			name, err := e.headerName(items[0])
			if err != nil {
				return err
			}
			value, err := e.headerValue(items[1])
			if err != nil {
				return err
			}
			return e.appendHeader(h, name, value)
		})
	}
	keys, err := r.OwnKeys(init.AsObject())
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.IsSymbol() {
			continue
		}
		desc, ok := init.AsObject().GetOwnProperty(k)
		if !ok || !desc.Enumerable() {
			continue
		}
		v, err := init.AsObject().GetProp(r, k)
		if err != nil {
			return err
		}
		name, err := e.headerName(engine.StringValue(k.ToJSString(r)))
		if err != nil {
			return err
		}
		value, err := e.headerValue(v)
		if err != nil {
			return err
		}
		if err := e.appendHeader(h, name, value); err != nil {
			return err
		}
	}
	return nil
}

// sortedListRaw returns the header list in order (a copy is not needed: the
// caller only reads it).
func (h *headers) sortedListRaw() []hdr {
	h.materialize()
	return h.list
}

// appendHeader implements Headers append for a validated name and value,
// which the list keeps: they are charged to the memory limit.
func (e *env) appendHeader(h *headers, name, value string) error {
	if h.guard == guardImmutable {
		return e.r.TypeError("Headers are immutable")
	}
	if err := e.r.ChargeMemory(len(name) + len(value)); err != nil {
		return err
	}
	h.append(name, value)
	return nil
}

func (e *env) headersSpec() classSpec {
	hs := func(r *engine.Realm, this engine.Value, name string) (*headers, error) {
		return thisAs[*headers](r, this, "Headers.prototype."+name)
	}
	args2 := func(r *engine.Realm, args []engine.Value, method string, n int) error {
		if len(args) < n {
			return r.TypeError("Failed to execute '%s' on 'Headers': %d arguments required, but only %d present.", method, n, len(args))
		}
		return nil
	}
	return classSpec{
		name: "Headers",
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			h := &headers{}
			o, err := e.newObject(clsHeaders, newTarget, h)
			if err != nil {
				return undefined, err
			}
			h.obj = o
			if err := e.fillHeaders(h, engine.Arg(args, 0)); err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "append", length: 2, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				h, err := hs(r, this, "append")
				if err == nil {
					err = args2(r, args, "append", 2)
				}
				if err != nil {
					return undefined, err
				}
				name, err := e.headerName(args[0])
				if err != nil {
					return undefined, err
				}
				value, err := e.headerValue(args[1])
				if err != nil {
					return undefined, err
				}
				return undefined, e.appendHeader(h, name, value)
			}},
			{name: "delete", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				h, err := hs(r, this, "delete")
				if err == nil {
					err = args2(r, args, "delete", 1)
				}
				if err != nil {
					return undefined, err
				}
				name, err := e.headerName(args[0])
				if err != nil {
					return undefined, err
				}
				if h.guard == guardImmutable {
					return undefined, r.TypeError("Headers are immutable")
				}
				if h.has(name) {
					h.delete(name)
				}
				return undefined, nil
			}},
			{name: "get", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				h, err := hs(r, this, "get")
				if err == nil {
					err = args2(r, args, "get", 1)
				}
				if err != nil {
					return undefined, err
				}
				name, err := e.headerName(args[0])
				if err != nil {
					return undefined, err
				}
				v, ok := h.get(name)
				if !ok {
					return engine.Null(), nil
				}
				return byteValue(v), nil
			}},
			{name: "getSetCookie", fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				h, err := hs(r, this, "getSetCookie")
				if err != nil {
					return undefined, err
				}
				h.materialize()
				var out []engine.Value
				for _, x := range h.list {
					if x.name == "set-cookie" {
						out = append(out, byteValue(x.value))
					}
				}
				return engine.ObjectValue(r.NewArrayFromSlice(out)), nil
			}},
			{name: "has", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				h, err := hs(r, this, "has")
				if err == nil {
					err = args2(r, args, "has", 1)
				}
				if err != nil {
					return undefined, err
				}
				name, err := e.headerName(args[0])
				if err != nil {
					return undefined, err
				}
				return engine.Bool(h.has(name)), nil
			}},
			{name: "set", length: 2, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				h, err := hs(r, this, "set")
				if err == nil {
					err = args2(r, args, "set", 2)
				}
				if err != nil {
					return undefined, err
				}
				name, err := e.headerName(args[0])
				if err != nil {
					return undefined, err
				}
				value, err := e.headerValue(args[1])
				if err != nil {
					return undefined, err
				}
				if h.guard == guardImmutable {
					return undefined, r.TypeError("Headers are immutable")
				}
				if err := r.ChargeMemory(len(name) + len(value)); err != nil {
					return undefined, err
				}
				h.set(name, value)
				return undefined, nil
			}},
			{name: "forEach", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				h, err := hs(r, this, "forEach")
				if err != nil {
					return undefined, err
				}
				cb := engine.Arg(args, 0)
				if !engine.IsCallable(cb) {
					return undefined, r.TypeError("Failed to execute 'forEach' on 'Headers': The callback provided as parameter 1 is not a function.")
				}
				for i := 0; ; i++ {
					l := h.sortedList()
					if i >= len(l) {
						return undefined, nil
					}
					if _, err := r.Call(cb, engine.Arg(args, 1), []engine.Value{byteValue(l[i].value), str(l[i].name), this}); err != nil {
						return undefined, err
					}
				}
			}},
			e.iterMethod("entries", clsHeadersIterator, iterEntries),
			e.iterMethod("keys", clsHeadersIterator, iterKeys),
			e.iterMethod("values", clsHeadersIterator, iterValues),
			func() member {
				m := e.iterMethod("[Symbol.iterator]", clsHeadersIterator, iterEntries)
				m.sym = engine.SymIterator
				m.name = "entries"
				return m
			}(),
		},
	}
}

func (*headers) iteratorClass() classID { return clsHeadersIterator }

// headersPairAt returns the pair i of a Headers iterator: of the sorted
// and combined list, names and values as JavaScript sees them.
func headersPairAt(t any, i int) (k, v engine.Value, ok bool) {
	l := t.(*headers).sortedList()
	if i >= len(l) {
		return undefined, undefined, false
	}
	return str(l[i].name), byteValue(l[i].value), true
}

// --- Go headers -----------------------------------------------------------------

// canonical returns the canonical form of a lowercase header name, cached
// for the names a runtime uses.
func (e *env) canonical(name string) string {
	if c, ok := e.canon[name]; ok {
		return c
	}
	c := textproto.CanonicalMIMEHeaderKey(name)
	if e.canon == nil {
		e.canon = map[string]string{}
	}
	if len(e.canon) < 256 {
		e.canon[name] = c
	}
	return c
}

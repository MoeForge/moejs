package webapi

import (
	"slices"
	"unicode/utf8"

	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/webapi/internal/whatwgurl"
)

// The URL Standard's URL and URLSearchParams over package whatwgurl.

// urlObject is the state of a URL.
type urlObject struct {
	u      *whatwgurl.URL
	href   engine.Value // the serialized URL, undefined until read
	params *searchParams
	obj    *engine.Object
}

// searchParams is the state of a URLSearchParams.
type searchParams struct {
	list []whatwgurl.Pair
	url  *urlObject // the URL whose query this is, or nil
	obj  *engine.Object
	// size is the bytes of the names and values of list, which Go holds
	// for JavaScript: the memory limit's measurements count them.
	size int
}

// pairBytes is the bytes of the names and values of l.
func pairBytes(l []whatwgurl.Pair) int {
	n := 0
	for _, p := range l {
		n += len(p.Name) + len(p.Value)
	}
	return n
}

// add appends a pair.
func (sp *searchParams) add(name, value string) {
	sp.list = append(sp.list, whatwgurl.Pair{Name: name, Value: value})
	sp.size += len(name) + len(value)
}

// urlOptions are the URL parser's options: domains are normalized to NFC
// by the runtime's String.prototype.normalize (the intrinsic).
func (e *env) urlOptions() *whatwgurl.Options {
	if e.urlOpts == nil {
		e.urlOpts = &whatwgurl.Options{NFC: e.nfc}
	}
	return e.urlOpts
}

// nfc normalizes s to NFC through the runtime.
func (e *env) nfc(s string) string {
	r := e.r
	normalize := e.intrinsics().normalize
	if normalize == nil {
		return s
	}
	v, err := r.CallObject(normalize, str(s), nil)
	if err != nil || !v.IsString() {
		return s
	}
	return v.AsString().GoString()
}

// parseURL parses input against base (undefined: none); ok is false when
// either is not a valid URL, err is the error of converting them.
func (e *env) parseURL(input, base engine.Value) (u *whatwgurl.URL, ok bool, err error) {
	s, err := e.goString(input)
	if err != nil {
		return nil, false, err
	}
	var b *whatwgurl.URL
	if !base.IsUndefined() {
		bs, err := e.goString(base)
		if err != nil {
			return nil, false, err
		}
		if b, err = e.urlOptions().Parse(bs, nil); err != nil {
			return nil, false, nil
		}
	}
	if u, err = e.urlOptions().Parse(s, b); err != nil {
		return nil, false, nil
	}
	return u, true, nil
}

// changed records that the URL changed: the cached href is stale. The URL
// keeps its new serialization, a Go string charged to the memory limit.
func (x *urlObject) changed(r *engine.Realm) error {
	x.href = undefined
	return r.ChargeMemory(len(x.u.Href()))
}

// syncParams sets the search params' list from the URL's query, whose
// pairs it charges to the memory limit.
func (x *urlObject) syncParams(r *engine.Realm) error {
	sp := x.params
	if sp == nil {
		return nil
	}
	if q, ok := x.u.Query(); ok {
		sp.list = whatwgurl.ParseForm(q)
	} else {
		sp.list = sp.list[:0]
	}
	sp.size = pairBytes(sp.list)
	return r.ChargeMemory(sp.size)
}

func (e *env) newURLObject(newTarget *engine.Object, u *whatwgurl.URL) (*engine.Object, error) {
	if err := e.r.ChargeMemory(len(u.Href())); err != nil {
		return nil, err
	}
	x := &urlObject{u: u, href: undefined}
	o, err := e.newObject(clsURL, newTarget, x)
	x.obj = o
	return o, err
}

func (e *env) urlSpec() classSpec {
	thisURL := func(r *engine.Realm, this engine.Value, name string) (*urlObject, error) {
		return thisAs[*urlObject](r, this, "URL.prototype."+name)
	}
	href := func(x *urlObject) engine.Value {
		if x.href.IsUndefined() {
			x.href = str(x.u.Href())
		}
		return x.href
	}
	attr := func(name string, get func(*whatwgurl.URL) string, set func(*whatwgurl.URL, string)) member {
		m := member{name: name, get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			x, err := thisURL(r, this, name)
			if err != nil {
				return undefined, err
			}
			return str(get(x.u)), nil
		}}
		if set != nil {
			m.set = func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				x, err := thisURL(r, this, name)
				if err != nil {
					return undefined, err
				}
				v, err := e.goString(engine.Arg(args, 0))
				if err != nil {
					return undefined, err
				}
				set(x.u, v)
				if err := x.changed(r); err != nil {
					return undefined, err
				}
				if name == "search" {
					return undefined, x.syncParams(r)
				}
				return undefined, nil
			}
		}
		return m
	}
	stringify := func(name string) member {
		return member{name: name, fn: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
			x, err := thisURL(r, this, name)
			if err != nil {
				return undefined, err
			}
			return href(x), nil
		}}
	}
	return classSpec{
		name: "URL", length: 1,
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			if len(args) == 0 {
				return undefined, r.TypeError("Failed to construct 'URL': 1 argument required, but only 0 present.")
			}
			u, ok, err := e.parseURL(args[0], engine.Arg(args, 1))
			if err != nil {
				return undefined, err
			}
			if !ok {
				return undefined, r.TypeError("Failed to construct 'URL': Invalid URL")
			}
			o, err := e.newURLObject(newTarget, u)
			if err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "href",
				get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
					x, err := thisURL(r, this, "href")
					if err != nil {
						return undefined, err
					}
					return href(x), nil
				},
				set: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
					x, err := thisURL(r, this, "href")
					if err != nil {
						return undefined, err
					}
					s, err := e.goString(engine.Arg(args, 0))
					if err != nil {
						return undefined, err
					}
					u, err := e.urlOptions().Parse(s, nil)
					if err != nil {
						return undefined, r.TypeError("Failed to set the 'href' property on 'URL': Invalid URL")
					}
					x.u = u
					if err := x.changed(r); err != nil {
						return undefined, err
					}
					return undefined, x.syncParams(r)
				}},
			attr("origin", (*whatwgurl.URL).Origin, nil),
			attr("protocol", (*whatwgurl.URL).Protocol, (*whatwgurl.URL).SetProtocol),
			attr("username", (*whatwgurl.URL).Username, (*whatwgurl.URL).SetUsername),
			attr("password", (*whatwgurl.URL).Password, (*whatwgurl.URL).SetPassword),
			attr("host", (*whatwgurl.URL).Host, (*whatwgurl.URL).SetHost),
			attr("hostname", (*whatwgurl.URL).Hostname, (*whatwgurl.URL).SetHostname),
			attr("port", (*whatwgurl.URL).Port, (*whatwgurl.URL).SetPort),
			attr("pathname", (*whatwgurl.URL).Pathname, (*whatwgurl.URL).SetPathname),
			attr("search", (*whatwgurl.URL).Search, (*whatwgurl.URL).SetSearch),
			{name: "searchParams", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				x, err := thisURL(r, this, "searchParams")
				if err != nil {
					return undefined, err
				}
				if x.params == nil {
					x.params = &searchParams{url: x}
					if err := x.syncParams(r); err != nil {
						return undefined, err
					}
					x.params.obj, _ = e.newObject(clsURLSearchParams, nil, x.params)
				}
				return engine.ObjectValue(x.params.obj), nil
			}},
			attr("hash", (*whatwgurl.URL).Hash, (*whatwgurl.URL).SetHash),
			stringify("toJSON"),
			stringify("toString"),
		},
		statics: []member{
			{name: "canParse", length: 1, fn: func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
				_, ok, err := e.parseURL(engine.Arg(args, 0), engine.Arg(args, 1))
				return engine.Bool(ok), err
			}},
			{name: "parse", length: 1, fn: func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
				u, ok, err := e.parseURL(engine.Arg(args, 0), engine.Arg(args, 1))
				if err != nil || !ok {
					return engine.Null(), err
				}
				o, err := e.newURLObject(nil, u)
				if err != nil {
					return undefined, err
				}
				return engine.ObjectValue(o), nil
			}},
		},
	}
}

// --- URLSearchParams ------------------------------------------------------------

// update implements the URLSearchParams update steps.
func (sp *searchParams) update(r *engine.Realm) error {
	x := sp.url
	if x == nil {
		return nil
	}
	q := whatwgurl.AppendForm(nil, sp.list)
	x.u.SetQuery(string(q), len(q) > 0)
	return x.changed(r)
}

func (*searchParams) iteratorClass() classID { return clsURLSearchParamsIterator }

func searchParamsPairAt(t any, i int) (k, v engine.Value, ok bool) {
	l := t.(*searchParams).list
	if i >= len(l) {
		return undefined, undefined, false
	}
	return str(l[i].Name), str(l[i].Value), true
}

// fillParams fills sp from the constructor's init.
func (e *env) fillParams(sp *searchParams, init engine.Value) error {
	r := e.r
	if init.IsObject() {
		if m, err := r.GetMethod(init, engine.SymbolKey(engine.SymIterator)); err != nil {
			return err
		} else if !m.IsUndefined() {
			return e.iterate(init, func(pair engine.Value) error {
				var items []engine.Value
				if err := e.iterate(pair, func(v engine.Value) error {
					items = append(items, v)
					return nil
				}); err != nil {
					return err
				}
				if len(items) != 2 {
					return r.TypeError("Failed to construct 'URLSearchParams': Sequence initializer must only contain pair elements")
				}
				name, err := e.goString(items[0])
				if err != nil {
					return err
				}
				value, err := e.goString(items[1])
				if err != nil {
					return err
				}
				sp.add(name, value)
				return nil
			})
		}
		o := init.AsObject()
		keys, err := r.OwnKeys(o)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if k.IsSymbol() {
				continue
			}
			if d, ok := o.GetOwnProperty(k); !ok || !d.Enumerable() {
				continue
			}
			v, err := o.GetProp(r, k)
			if err != nil {
				return err
			}
			value, err := e.goString(v)
			if err != nil {
				return err
			}
			sp.add(k.ToJSString(r).GoString(), value)
		}
		return nil
	}
	if init.IsUndefined() {
		return nil
	}
	s, err := e.goString(init)
	if err != nil {
		return err
	}
	if len(s) > 0 && s[0] == '?' {
		s = s[1:]
	}
	sp.list = whatwgurl.ParseForm(s)
	sp.size = pairBytes(sp.list)
	return nil
}

// compareUTF16 compares a and b by their UTF-16 code units.
func compareUTF16(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			ua, ub := firstUnit(ra), firstUnit(rb)
			if ua != ub {
				if ua < ub {
					return -1
				}
				return 1
			}
			if ra < rb {
				return -1
			}
			return 1
		}
		a, b = a[na:], b[nb:]
	}
	switch {
	case len(a) > 0:
		return 1
	case len(b) > 0:
		return -1
	}
	return 0
}

// firstUnit is the first UTF-16 code unit of c.
func firstUnit(c rune) rune {
	if c < 0x10000 {
		return c
	}
	return 0xD800 + (c-0x10000)>>10
}

func (e *env) searchParamsSpec() classSpec {
	sps := func(r *engine.Realm, this engine.Value, name string) (*searchParams, error) {
		return thisAs[*searchParams](r, this, "URLSearchParams.prototype."+name)
	}
	need := func(r *engine.Realm, args []engine.Value, method string, n int) error {
		if len(args) < n {
			return r.TypeError("Failed to execute '%s' on 'URLSearchParams': %d argument%s required, but only %d present.", method, n, map[bool]string{true: "s"}[n > 1], len(args))
		}
		return nil
	}
	// strs converts the first n arguments.
	strs := func(args []engine.Value, n int) ([]string, error) {
		out := make([]string, n)
		for i := range n {
			s, err := e.goString(args[i])
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	}
	method := func(name string, n int, f func(sp *searchParams, s []string, args []engine.Value) (engine.Value, error)) member {
		return member{name: name, length: n, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
			sp, err := sps(r, this, name)
			if err == nil {
				err = need(r, args, name, n)
			}
			if err != nil {
				return undefined, err
			}
			s, err := strs(args, n)
			if err != nil {
				return undefined, err
			}
			return f(sp, s, args)
		}}
	}
	// value reads the optional value argument of delete and has.
	value := func(args []engine.Value) (string, bool, error) {
		if v := engine.Arg(args, 1); !v.IsUndefined() {
			s, err := e.goString(v)
			return s, true, err
		}
		return "", false, nil
	}
	return classSpec{
		name: "URLSearchParams",
		ctor: func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
			sp := &searchParams{}
			o, err := e.newObject(clsURLSearchParams, newTarget, sp)
			if err != nil {
				return undefined, err
			}
			sp.obj = o
			if err := e.fillParams(sp, engine.Arg(args, 0)); err != nil {
				return undefined, err
			}
			// The pairs are Go strings the object keeps.
			if err := r.ChargeMemory(sp.size); err != nil {
				return undefined, err
			}
			return engine.ObjectValue(o), nil
		},
		members: []member{
			{name: "size", get: func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
				sp, err := sps(r, this, "size")
				if err != nil {
					return undefined, err
				}
				return engine.Int64Value(int64(len(sp.list))), nil
			}},
			method("append", 2, func(sp *searchParams, s []string, _ []engine.Value) (engine.Value, error) {
				if err := e.r.ChargeMemory(len(s[0]) + len(s[1])); err != nil {
					return undefined, err
				}
				sp.add(s[0], s[1])
				return undefined, sp.update(e.r)
			}),
			method("delete", 1, func(sp *searchParams, s []string, args []engine.Value) (engine.Value, error) {
				v, hasValue, err := value(args)
				if err != nil {
					return undefined, err
				}
				sp.list = slices.DeleteFunc(sp.list, func(p whatwgurl.Pair) bool {
					return p.Name == s[0] && (!hasValue || p.Value == v)
				})
				sp.size = pairBytes(sp.list)
				return undefined, sp.update(e.r)
			}),
			method("get", 1, func(sp *searchParams, s []string, _ []engine.Value) (engine.Value, error) {
				for _, p := range sp.list {
					if p.Name == s[0] {
						return str(p.Value), nil
					}
				}
				return engine.Null(), nil
			}),
			method("getAll", 1, func(sp *searchParams, s []string, _ []engine.Value) (engine.Value, error) {
				var out []engine.Value
				for _, p := range sp.list {
					if p.Name == s[0] {
						out = append(out, str(p.Value))
					}
				}
				return engine.ObjectValue(e.r.NewArrayFromSlice(out)), nil
			}),
			method("has", 1, func(sp *searchParams, s []string, args []engine.Value) (engine.Value, error) {
				v, hasValue, err := value(args)
				if err != nil {
					return undefined, err
				}
				for _, p := range sp.list {
					if p.Name == s[0] && (!hasValue || p.Value == v) {
						return engine.Bool(true), nil
					}
				}
				return engine.Bool(false), nil
			}),
			method("set", 2, func(sp *searchParams, s []string, _ []engine.Value) (engine.Value, error) {
				if err := e.r.ChargeMemory(len(s[0]) + len(s[1])); err != nil {
					return undefined, err
				}
				found := false
				sp.list = slices.DeleteFunc(sp.list, func(p whatwgurl.Pair) bool {
					if p.Name != s[0] {
						return false
					}
					if !found {
						found = true
						return false
					}
					return true
				})
				if found {
					for i := range sp.list {
						if sp.list[i].Name == s[0] {
							sp.list[i].Value = s[1]
							break
						}
					}
				} else {
					sp.list = append(sp.list, whatwgurl.Pair{Name: s[0], Value: s[1]})
				}
				sp.size = pairBytes(sp.list)
				return undefined, sp.update(e.r)
			}),
			method("sort", 0, func(sp *searchParams, _ []string, _ []engine.Value) (engine.Value, error) {
				slices.SortStableFunc(sp.list, func(a, b whatwgurl.Pair) int { return compareUTF16(a.Name, b.Name) })
				return undefined, sp.update(e.r)
			}),
			method("toString", 0, func(sp *searchParams, _ []string, _ []engine.Value) (engine.Value, error) {
				return str(string(whatwgurl.AppendForm(nil, sp.list))), nil
			}),
			{name: "forEach", length: 1, fn: func(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
				sp, err := sps(r, this, "forEach")
				if err != nil {
					return undefined, err
				}
				cb := engine.Arg(args, 0)
				if !engine.IsCallable(cb) {
					return undefined, r.TypeError("Failed to execute 'forEach' on 'URLSearchParams': The callback provided as parameter 1 is not a function.")
				}
				for i := 0; i < len(sp.list); i++ {
					p := sp.list[i]
					if _, err := r.Call(cb, engine.Arg(args, 1), []engine.Value{str(p.Value), str(p.Name), this}); err != nil {
						return undefined, err
					}
				}
				return undefined, nil
			}},
			e.iterMethod("entries", clsURLSearchParamsIterator, iterEntries),
			e.iterMethod("keys", clsURLSearchParamsIterator, iterKeys),
			e.iterMethod("values", clsURLSearchParamsIterator, iterValues),
			func() member {
				m := e.iterMethod("entries", clsURLSearchParamsIterator, iterEntries)
				m.sym = engine.SymIterator
				return m
			}(),
		},
	}
}

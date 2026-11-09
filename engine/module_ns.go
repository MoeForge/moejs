package engine

// Module namespace exotic objects (ECMA-262 §10.4.6).
//
// A namespace is an object of class ClassProxy, so every internal method
// that can observe it already takes the proxy path (the shared empty
// proxyShape, no inline cache, a nil [[Prototype]], not extensible), and
// each proxy internal method in proxy.go starts with a branch here. Its
// internal is the *moduleNamespace instead of a *proxyData: the export
// names of the layout the linker computed, and the binding cell each one
// reads, which is the slot of the exporting module's environment (so the
// namespace sees the live binding) or the namespace cell of a module for an
// `export * as ns` binding.

// moduleNamespace is the state of a namespace object.
type moduleNamespace struct {
	layout *nsLayout
	cells  []*Value // parallel to layout.names
}

// namespaceObject co-allocates a namespace with its state.
type namespaceObject struct {
	obj Object
	ns  moduleNamespace
}

// newNamespace creates the namespace object of layout; the caller fills
// the cells.
func newNamespace(layout *nsLayout) (*Object, *moduleNamespace) {
	no := &namespaceObject{ns: moduleNamespace{layout: layout, cells: make([]*Value, len(layout.names))}}
	o := &no.obj
	o.shape = proxyShape
	o.class = ClassProxy
	setData(o, kindNamespace, &no.ns)
	return o, &no.ns
}

// nsOf returns the state of o when it is a namespace, which the caller
// knows is of class ClassProxy.
func nsOf(o *Object) *moduleNamespace {
	ns := dataOf(o, kindNamespace)
	return ns
}

// IsModuleNamespace reports whether o is a module namespace object.
func (o *Object) IsModuleNamespace() bool { return o.class == ClassProxy && nsOf(o) != nil }

// nsTagString is the value of a namespace's @@toStringTag.
var nsTagString = func() *String {
	s := asciiString("Module")
	prepareSharedString(s)
	return s
}()

// nsTag is the namespace's only symbol-keyed property, @@toStringTag.
func nsTag() PropertyDescriptor { return DataDescriptor(StringValue(nsTagString), 0) }

// lookup returns the index of the export key, or -1.
func (ns *moduleNamespace) lookup(key PropertyKey) int {
	if key.IsSymbol() {
		return -1
	}
	if i, ok := ns.layout.index[key]; ok {
		return i
	}
	return -1
}

// get reads export i: a binding not initialized yet is a ReferenceError.
func (ns *moduleNamespace) get(r *Realm, i int) (Value, error) {
	v := *ns.cells[i]
	if v.IsHole() {
		return Undefined(), r.ReferenceError("Cannot access '%s' before initialization", ns.layout.names[i])
	}
	return v, nil
}

// nsGetOwnProperty implements [[GetOwnProperty]] (§10.4.6.5).
func (r *Realm) nsGetOwnProperty(ns *moduleNamespace, key PropertyKey) (PropertyDescriptor, bool, error) {
	if key.IsSymbol() {
		if key.Symbol() == SymToStringTag {
			return nsTag(), true, nil
		}
		return PropertyDescriptor{}, false, nil
	}
	i := ns.lookup(key)
	if i < 0 {
		return PropertyDescriptor{}, false, nil
	}
	v, err := ns.get(r, i)
	if err != nil {
		return PropertyDescriptor{}, false, err
	}
	return DataDescriptor(v, attrWritable|attrEnumerable), true, nil
}

// nsDefineOwnProperty implements [[DefineOwnProperty]] (§10.4.6.6).
func (r *Realm) nsDefineOwnProperty(ns *moduleNamespace, key PropertyKey, desc PropertyDescriptor) (bool, error) {
	if key.IsSymbol() {
		if key.Symbol() != SymToStringTag {
			return false, nil // not extensible
		}
		cur := nsTag()
		return ValidateAndApplyPropertyDescriptor(r, nil, key, false, desc, &cur), nil
	}
	cur, ok, err := r.nsGetOwnProperty(ns, key)
	switch {
	case err != nil:
		return false, err
	case !ok,
		desc.HasConfigurable() && desc.Configurable(),
		desc.HasEnumerable() && !desc.Enumerable(),
		desc.IsAccessorDescriptor(),
		desc.HasWritable() && !desc.Writable():
		return false, nil
	case desc.HasValue():
		return SameValue(desc.Value, cur.Value), nil
	}
	return true, nil
}

// nsGet implements [[Get]] (§10.4.6.8).
func (r *Realm) nsGet(ns *moduleNamespace, key PropertyKey) (Value, bool, error) {
	if key.IsSymbol() {
		if key.Symbol() == SymToStringTag {
			return StringValue(nsTagString), true, nil
		}
		return Undefined(), false, nil
	}
	i := ns.lookup(key)
	if i < 0 {
		return Undefined(), false, nil
	}
	v, err := ns.get(r, i)
	return v, true, err
}

// nsHas implements [[HasProperty]] (§10.4.6.7).
func nsHas(ns *moduleNamespace, key PropertyKey) bool {
	if key.IsSymbol() {
		return key.Symbol() == SymToStringTag
	}
	return ns.lookup(key) >= 0
}

// nsDelete implements [[Delete]] (§10.4.6.10).
func nsDelete(ns *moduleNamespace, key PropertyKey) bool {
	if key.IsSymbol() {
		return key.Symbol() != SymToStringTag
	}
	return ns.lookup(key) < 0
}

// nsOwnKeys implements [[OwnPropertyKeys]] (§10.4.6.11).
func nsOwnKeys(ns *moduleNamespace) []PropertyKey {
	keys := make([]PropertyKey, len(ns.layout.keys), len(ns.layout.keys)+1)
	copy(keys, ns.layout.keys)
	return append(keys, SymbolKey(SymToStringTag))
}

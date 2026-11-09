package engine

// The Proxy constructor and Proxy.revocable (ECMA-262 §28.2), a late
// global. The proxy objects themselves are in proxy.go.

func init() { lateGlobal(StringKey(AtomProxy), installProxyGlobal) }

func installProxyGlobal(r *Realm) {
	// Proxy has no "prototype" property: newConstructor would add one.
	c := r.NewNativeConstructor(AtomProxy, 2, nil, proxyCtor)
	r.installBuiltins(c, proxyCtorMethods)
	r.bindGlobal(AtomProxy, ObjectValue(c))
}

var proxyCtorMethods = []builtinDef{
	{AtomRevocable, proxyRevocable, 2},
}

// proxyCreate implements ProxyCreate(target, handler).
func (r *Realm) proxyCreate(target, handler Value) (*Object, error) {
	if !target.IsObject() || !handler.IsObject() {
		return nil, r.TypeError("Cannot create proxy with a non-object as target or handler")
	}
	return r.newProxy(target.AsObject(), handler.AsObject()), nil
}

// proxyCtor implements new Proxy(target, handler).
func proxyCtor(r *Realm, args []Value, newTarget *Object) (Value, error) {
	p, err := r.proxyCreate(Arg(args, 0), Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	return ObjectValue(p), nil
}

// proxyRevocable implements Proxy.revocable(target, handler): the result is
// { proxy, revoke }, revoke a data native over the proxy's state.
func proxyRevocable(r *Realm, this Value, args []Value) (Value, error) {
	p, err := r.proxyCreate(Arg(args, 0), Arg(args, 1))
	if err != nil {
		return Undefined(), err
	}
	revoke := r.NewNativeDataFunction(AtomEmpty, 0, proxyRevoke, proxyOf(p))
	res := r.NewObject()
	res.DefineOwnDataFast(r, StringKey(AtomProxyLower), ObjectValue(p), attrDefault)
	res.DefineOwnDataFast(r, StringKey(AtomRevoke), ObjectValue(revoke), attrDefault)
	return ObjectValue(res), nil
}

// proxyRevoke is a revoke function: the first call revokes the proxy and
// drops the function's reference to it, later calls do nothing.
func proxyRevoke(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	if pd, ok := fd.nat().data.(*proxyData); ok {
		fd.nat().data = nil
		pd.target, pd.handler = nil, nil
	}
	return Undefined(), nil
}

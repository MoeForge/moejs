package engine

import (
	"slices"
	"unsafe"
)

// Proxy exotic objects (ECMA-262 §10.5).
//
// Module namespace objects (module_ns.go) are of class ClassProxy too, and
// each proxy internal method below starts with their branch.
//
// A proxy is an object of class ClassProxy with the shared empty proxyShape
// (noFill: no inline cache is ever filled through it), a nil [[Prototype]]
// and no flags, so the ordinary infallible methods (HasOwnProperty,
// OwnPropertyKeys, IsExtensible, Proto, ...) see an empty, non-extensible
// object with a null prototype and never run a trap. Every internal method
// that can observe a proxy reaches the traps below: [[Get]], [[Set]] and
// [[DefineOwnProperty]] through their class checks in object.go (on the slow
// path only: a proxy has no own property to find), the others through the
// fallible Realm-level dispatchers at the end of this file (getPrototypeOf,
// hasProperty, getOwnProperty, ownPropertyKeys, ...), which the builtins and
// the interpreter call where the operand may be a proxy.
//
// A proxy whose target is callable is callable itself: its internal is a
// *FunctionData of kind FuncProxy whose data is the *proxyData, so [[Call]]
// is the rare callOther arm of CallObject (the data-native adapter
// proxyCall) and [[Construct]] a rare arm of Construct; the hot arms do not
// change. A non-callable proxy's internal is the *proxyData itself.
//
// A chain of proxies without traps forwards through Go recursion: every
// internal method counts itself in the call depth (proxyEnter), and [[Call]]
// and [[Construct]] are counted by CallObject and Construct, so a chain
// deeper than MaxCallDepth is a RangeError.

// proxyData is the state of a proxy: [[ProxyTarget]] and [[ProxyHandler]],
// both nil once the proxy is revoked.
type proxyData struct {
	target, handler *Object
	ctor            bool // the target was a constructor when the proxy was created
}

// proxyObject co-allocates a non-callable proxy with its state.
type proxyObject struct {
	obj Object
	pd  proxyData
}

// callableProxyObject co-allocates a callable proxy with its function
// payload, its nativeState and its state (168 bytes, the 176-byte size
// class).
type callableProxyObject struct {
	obj Object
	fd  FunctionData
	nat nativeState
	pd  proxyData
}

// proxyShape is the shape of every proxy: empty, process-wide and never
// cached by an inline cache (noFill). Nothing adds a property to a proxy:
// [[DefineOwnProperty]] goes to the defineProperty trap.
var proxyShape = &Shape{key: rootKey, shared: true, noFill: noFillAll}

// newProxy implements ProxyCreate for object operands.
func (r *Realm) newProxy(target, handler *Object) *Object {
	var o *Object
	if target.IsCallable() {
		p := &callableProxyObject{}
		r.chargeObject(unsafe.Sizeof(callableProxyObject{}))
		p.pd = proxyData{target: target, handler: handler, ctor: target.funcData().IsConstructor()}
		p.fd = FunctionData{kind: FuncProxy, realm: r}
		p.nat = nativeState{dataFn: proxyCall, data: &p.pd}
		o = &p.obj
		setData(o, kindFunction, &p.fd)
	} else {
		p := &proxyObject{pd: proxyData{target: target, handler: handler}}
		r.chargeObject(unsafe.Sizeof(proxyObject{}))
		o = &p.obj
		setData(o, kindProxy, &p.pd)
	}
	o.shape = proxyShape
	o.class = ClassProxy
	return o
}

// proxyOf returns the state of the proxy o.
func proxyOf(o *Object) *proxyData {
	if fd, ok := dataOk(o, kindFunction); ok {
		return fd.nat().data.(*proxyData)
	}
	return dataOf(o, kindProxy)
}

// isCallableProxy reports whether the proxy o has [[Call]].
func (o *Object) isCallableProxy() bool {
	return o.pkind == payloadKind(kindFunction)
}

// IsRevokedProxy reports whether o is a proxy that has been revoked.
func (o *Object) IsRevokedProxy() bool {
	return o.class == ClassProxy && nsOf(o) == nil && proxyOf(o).handler == nil
}

// proxyEnter counts an internal method of a proxy in the call depth; the
// caller defers proxyLeave when it succeeds.
func (r *Realm) proxyEnter() error {
	if r.callDepth >= MaxCallDepth {
		return r.RangeError("Maximum call stack size exceeded")
	}
	r.callDepth++
	return nil
}

func (r *Realm) proxyLeave() { r.callDepth-- }

// proxyTrap validates that the proxy is not revoked and returns its target,
// its handler and GetMethod(handler, name) (undefined when there is no
// trap). The target and handler are read first: the trap lookup may run a
// getter that revokes the proxy.
func (r *Realm) proxyTrap(pd *proxyData, name *String) (target, handler *Object, trap Value, err error) {
	if pd.handler == nil {
		return nil, nil, Undefined(), r.TypeError("Cannot perform '%s' on a proxy that has been revoked", name.GoString())
	}
	target, handler = pd.target, pd.handler
	trap, err = r.GetMethod(ObjectValue(handler), StringKey(name))
	return target, handler, trap, err
}

// proxyError is the TypeError of a trap result that breaks an invariant.
func (r *Realm) proxyError(trap *String, format string, args ...any) error {
	return r.TypeError("'%s' on proxy: "+format, append([]any{trap.GoString()}, args...)...)
}

// falsishError is the TypeError of a rejected strict-mode operation on o:
// for a proxy (whose invariant violations throw, so a rejection is always
// the trap's answer) the V8 "trap returned falsish" message, else format.
func (r *Realm) falsishError(o *Object, trap *String, key PropertyKey, format string, args ...any) error {
	if o.class == ClassProxy && nsOf(o) == nil {
		return r.proxyError(trap, "trap returned falsish for property '%s'", key.GoString())
	}
	return r.TypeError(format, args...)
}

// callTrap calls trap with the handler as this.
func (r *Realm) callTrap(trap Value, handler *Object, args ...Value) (Value, error) {
	return r.Call(trap, ObjectValue(handler), args)
}

// proxyGetPrototypeOf implements [[GetPrototypeOf]] (§10.5.1).
func (r *Realm) proxyGetPrototypeOf(o *Object) (*Object, error) {
	if nsOf(o) != nil {
		return nil, nil
	}
	if err := r.proxyEnter(); err != nil {
		return nil, err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomGetPrototypeOf)
	if err != nil {
		return nil, err
	}
	if trap.IsUndefined() {
		return r.getPrototypeOf(target)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target))
	if err != nil {
		return nil, err
	}
	if !v.IsObject() && !v.IsNull() {
		return nil, r.proxyError(AtomGetPrototypeOf, "trap returned neither object nor null")
	}
	var proto *Object
	if v.IsObject() {
		proto = v.AsObject()
	}
	ext, err := r.isExtensible(target)
	if err != nil || ext {
		return proto, err
	}
	targetProto, err := r.getPrototypeOf(target)
	if err != nil {
		return nil, err
	}
	if proto != targetProto {
		return nil, r.proxyError(AtomGetPrototypeOf, "proxy target is non-extensible but the trap did not return its actual prototype")
	}
	return proto, nil
}

// proxySetPrototypeOf implements [[SetPrototypeOf]] (§10.5.2).
func (r *Realm) proxySetPrototypeOf(o, proto *Object) (bool, error) {
	if nsOf(o) != nil {
		return proto == nil, nil
	}
	if err := r.proxyEnter(); err != nil {
		return false, err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomSetPrototypeOf)
	if err != nil {
		return false, err
	}
	if trap.IsUndefined() {
		return r.setPrototypeOf(target, proto)
	}
	pv := Null()
	if proto != nil {
		pv = ObjectValue(proto)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target), pv)
	if err != nil || !ToBoolean(v) {
		return false, err
	}
	ext, err := r.isExtensible(target)
	if err != nil || ext {
		return err == nil, err
	}
	targetProto, err := r.getPrototypeOf(target)
	if err != nil {
		return false, err
	}
	if proto != targetProto {
		return false, r.proxyError(AtomSetPrototypeOf, "trap returned truish for setting a new prototype on the non-extensible proxy target")
	}
	return true, nil
}

// proxyIsExtensible implements [[IsExtensible]] (§10.5.3).
func (r *Realm) proxyIsExtensible(o *Object) (bool, error) {
	if nsOf(o) != nil {
		return false, nil
	}
	if err := r.proxyEnter(); err != nil {
		return false, err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomIsExtensible)
	if err != nil {
		return false, err
	}
	if trap.IsUndefined() {
		return r.isExtensible(target)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target))
	if err != nil {
		return false, err
	}
	ext, err := r.isExtensible(target)
	if err != nil {
		return false, err
	}
	if ToBoolean(v) != ext {
		return false, r.proxyError(AtomIsExtensible, "trap result does not reflect extensibility of proxy target (which is '%v')", ext)
	}
	return ext, nil
}

// proxyPreventExtensions implements [[PreventExtensions]] (§10.5.4).
func (r *Realm) proxyPreventExtensions(o *Object) (bool, error) {
	if nsOf(o) != nil {
		return true, nil
	}
	if err := r.proxyEnter(); err != nil {
		return false, err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomPreventExtensions)
	if err != nil {
		return false, err
	}
	if trap.IsUndefined() {
		return r.preventExtensions(target)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target))
	if err != nil || !ToBoolean(v) {
		return false, err
	}
	ext, err := r.isExtensible(target)
	if err != nil {
		return false, err
	}
	if ext {
		return false, r.proxyError(AtomPreventExtensions, "trap returned truish but the proxy target is extensible")
	}
	return true, nil
}

// proxyGetOwnProperty implements [[GetOwnProperty]] (§10.5.5).
func (r *Realm) proxyGetOwnProperty(o *Object, key PropertyKey) (PropertyDescriptor, bool, error) {
	if ns := nsOf(o); ns != nil {
		return r.nsGetOwnProperty(ns, key)
	}
	var none PropertyDescriptor
	if err := r.proxyEnter(); err != nil {
		return none, false, err
	}
	defer r.proxyLeave()
	name := AtomGetOwnPropertyDescriptor
	target, handler, trap, err := r.proxyTrap(proxyOf(o), name)
	if err != nil {
		return none, false, err
	}
	if trap.IsUndefined() {
		return r.getOwnProperty(target, key)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target), keyValue(r, key))
	if err != nil {
		return none, false, err
	}
	if !v.IsObject() && !v.IsUndefined() {
		return none, false, r.proxyError(name, "trap returned neither object nor undefined for property '%s'", key.GoString())
	}
	targetDesc, found, err := r.getOwnProperty(target, key)
	if err != nil {
		return none, false, err
	}
	if v.IsUndefined() {
		if !found {
			return none, false, nil
		}
		if !targetDesc.Configurable() {
			return none, false, r.proxyError(name, "trap returned undefined for property '%s' which is non-configurable in the proxy target", key.GoString())
		}
		ext, err := r.isExtensible(target)
		if err != nil {
			return none, false, err
		}
		if !ext {
			return none, false, r.proxyError(name, "trap returned undefined for property '%s' which exists in the non-extensible proxy target", key.GoString())
		}
		return none, false, nil
	}
	ext, err := r.isExtensible(target)
	if err != nil {
		return none, false, err
	}
	desc, err := r.toPropertyDescriptor(v)
	if err != nil {
		return none, false, err
	}
	completePropertyDescriptor(&desc)
	var cur *PropertyDescriptor
	if found {
		cur = &targetDesc
	}
	if !ValidateAndApplyPropertyDescriptor(r, nil, key, ext, desc, cur) {
		return none, false, r.proxyError(name, "trap returned descriptor for property '%s' that is incompatible with the existing property in the proxy target", key.GoString())
	}
	if !desc.Configurable() {
		if !found || targetDesc.Configurable() {
			return none, false, r.proxyError(name, "trap reported non-configurability for property '%s' which is either non-existent or configurable in the proxy target", key.GoString())
		}
		if desc.HasWritable() && !desc.Writable() && targetDesc.Writable() {
			return none, false, r.proxyError(name, "trap reported non-configurable and writable for property '%s' which is non-configurable, non-writable in the proxy target", key.GoString())
		}
	}
	return desc, true, nil
}

// proxyDefineOwnProperty implements [[DefineOwnProperty]] (§10.5.6).
func (r *Realm) proxyDefineOwnProperty(o *Object, key PropertyKey, desc PropertyDescriptor) (bool, error) {
	if ns := nsOf(o); ns != nil {
		return r.nsDefineOwnProperty(ns, key, desc)
	}
	if err := r.proxyEnter(); err != nil {
		return false, err
	}
	defer r.proxyLeave()
	name := AtomDefineProperty
	target, handler, trap, err := r.proxyTrap(proxyOf(o), name)
	if err != nil {
		return false, err
	}
	if trap.IsUndefined() {
		// A shared intrinsic target answers as the frozen object it is,
		// without the TypeError, so a sloppy write through the proxy is
		// ignored like one to the target itself; Object.defineProperty
		// still throws for a descriptor that would change it.
		if target.flags&flagShared != 0 {
			return target.sharedDefineAllowed(key, desc), nil
		}
		return target.DefineOwnProperty(r, key, desc)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target), keyValue(r, key), r.fromPartialDescriptor(desc))
	if err != nil || !ToBoolean(v) {
		return false, err
	}
	targetDesc, found, err := r.getOwnProperty(target, key)
	if err != nil {
		return false, err
	}
	ext, err := r.isExtensible(target)
	if err != nil {
		return false, err
	}
	settingConfigFalse := desc.HasConfigurable() && !desc.Configurable()
	if !found {
		if !ext {
			return false, r.proxyError(name, "trap returned truish for adding property '%s' to the non-extensible proxy target", key.GoString())
		}
		if settingConfigFalse {
			return false, r.proxyError(name, "trap returned truish for defining non-configurable property '%s' which is either non-existent or configurable in the proxy target", key.GoString())
		}
		return true, nil
	}
	if !ValidateAndApplyPropertyDescriptor(r, nil, key, ext, desc, &targetDesc) {
		return false, r.proxyError(name, "trap returned truish for adding property '%s' that is incompatible with the existing property in the proxy target", key.GoString())
	}
	if settingConfigFalse && targetDesc.Configurable() {
		return false, r.proxyError(name, "trap returned truish for defining non-configurable property '%s' which is either non-existent or configurable in the proxy target", key.GoString())
	}
	if targetDesc.IsDataDescriptor() && !targetDesc.Configurable() && targetDesc.Writable() && desc.HasWritable() && !desc.Writable() {
		return false, r.proxyError(name, "trap returned truish for defining non-configurable property '%s' which cannot be non-writable, unless there exists a corresponding non-configurable, non-writable own property of the target object", key.GoString())
	}
	return true, nil
}

// proxyHas implements [[HasProperty]] (§10.5.7).
func (r *Realm) proxyHas(o *Object, key PropertyKey) (bool, error) {
	if ns := nsOf(o); ns != nil {
		return nsHas(ns, key), nil
	}
	if err := r.proxyEnter(); err != nil {
		return false, err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomHas)
	if err != nil {
		return false, err
	}
	if trap.IsUndefined() {
		return r.hasProperty(target, key)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target), keyValue(r, key))
	if err != nil {
		return false, err
	}
	if ToBoolean(v) {
		return true, nil
	}
	targetDesc, found, err := r.getOwnProperty(target, key)
	if err != nil || !found {
		return false, err
	}
	if !targetDesc.Configurable() {
		return false, r.proxyError(AtomHas, "trap returned falsish for property '%s' which exists in the proxy target as non-configurable", key.GoString())
	}
	ext, err := r.isExtensible(target)
	if err != nil {
		return false, err
	}
	if !ext {
		return false, r.proxyError(AtomHas, "trap returned falsish for property '%s' but the proxy target is not extensible", key.GoString())
	}
	return false, nil
}

// proxyGet implements [[Get]] (§10.5.8).
func (r *Realm) proxyGet(o *Object, key PropertyKey, receiver Value) (Value, error) {
	if ns := nsOf(o); ns != nil {
		v, _, err := r.nsGet(ns, key)
		return v, err
	}
	if err := r.proxyEnter(); err != nil {
		return Undefined(), err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomGet)
	if err != nil {
		return Undefined(), err
	}
	if trap.IsUndefined() {
		return target.Get(r, key, receiver)
	}
	return r.proxyGetTrap(target, handler, trap, key, receiver)
}

// proxyLookup is proxyGet for Object.Lookup: without a get trap the lookup
// continues on the target, which may not have the property.
func (r *Realm) proxyLookup(o *Object, key PropertyKey, receiver Value) (Value, bool, error) {
	if ns := nsOf(o); ns != nil {
		return r.nsGet(ns, key)
	}
	if err := r.proxyEnter(); err != nil {
		return Undefined(), false, err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomGet)
	if err != nil {
		return Undefined(), false, err
	}
	if trap.IsUndefined() {
		return target.lookup(r, key, receiver)
	}
	v, err := r.proxyGetTrap(target, handler, trap, key, receiver)
	return v, true, err
}

// proxyGetTrap calls a get trap and checks its result against the target.
func (r *Realm) proxyGetTrap(target, handler *Object, trap Value, key PropertyKey, receiver Value) (Value, error) {
	v, err := r.callTrap(trap, handler, ObjectValue(target), keyValue(r, key), receiver)
	if err != nil {
		return Undefined(), err
	}
	targetDesc, found, err := r.getOwnProperty(target, key)
	if err != nil {
		return Undefined(), err
	}
	if found && !targetDesc.Configurable() {
		if targetDesc.IsDataDescriptor() && !targetDesc.Writable() && !SameValue(v, targetDesc.Value) {
			return Undefined(), r.proxyError(AtomGet, "property '%s' is a read-only and non-configurable data property on the proxy target but the proxy did not return its actual value", key.GoString())
		}
		if targetDesc.IsAccessorDescriptor() && targetDesc.Get.IsUndefined() && !v.IsUndefined() {
			return Undefined(), r.proxyError(AtomGet, "property '%s' is a non-configurable accessor property on the proxy target and does not have a getter function, but the trap did not return 'undefined'", key.GoString())
		}
	}
	return v, nil
}

// proxySet implements [[Set]] (§10.5.9).
func (r *Realm) proxySet(o *Object, key PropertyKey, v, receiver Value) (bool, error) {
	if nsOf(o) != nil {
		return false, nil
	}
	if err := r.proxyEnter(); err != nil {
		return false, err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomSet)
	if err != nil {
		return false, err
	}
	if trap.IsUndefined() {
		return target.Set(r, key, v, receiver)
	}
	res, err := r.callTrap(trap, handler, ObjectValue(target), keyValue(r, key), v, receiver)
	if err != nil || !ToBoolean(res) {
		return false, err
	}
	targetDesc, found, err := r.getOwnProperty(target, key)
	if err != nil {
		return false, err
	}
	if found && !targetDesc.Configurable() {
		if targetDesc.IsDataDescriptor() && !targetDesc.Writable() && !SameValue(v, targetDesc.Value) {
			return false, r.proxyError(AtomSet, "trap returned truish for property '%s' which exists in the proxy target as a non-configurable and non-writable data property with a different value", key.GoString())
		}
		if targetDesc.IsAccessorDescriptor() && targetDesc.Set.IsUndefined() {
			return false, r.proxyError(AtomSet, "trap returned truish for property '%s' which exists in the proxy target as a non-configurable and non-writable accessor property without a setter", key.GoString())
		}
	}
	return true, nil
}

// proxyDelete implements [[Delete]] (§10.5.10).
func (r *Realm) proxyDelete(o *Object, key PropertyKey) (bool, error) {
	if ns := nsOf(o); ns != nil {
		return nsDelete(ns, key), nil
	}
	if err := r.proxyEnter(); err != nil {
		return false, err
	}
	defer r.proxyLeave()
	name := AtomDeleteProperty
	target, handler, trap, err := r.proxyTrap(proxyOf(o), name)
	if err != nil {
		return false, err
	}
	if trap.IsUndefined() {
		return r.deleteProperty(target, key)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target), keyValue(r, key))
	if err != nil || !ToBoolean(v) {
		return false, err
	}
	targetDesc, found, err := r.getOwnProperty(target, key)
	if err != nil || !found {
		return err == nil, err
	}
	if !targetDesc.Configurable() {
		return false, r.proxyError(name, "trap returned truish for property '%s' which is non-configurable in the proxy target", key.GoString())
	}
	ext, err := r.isExtensible(target)
	if err != nil {
		return false, err
	}
	if !ext {
		return false, r.proxyError(name, "trap returned truish for property '%s' but the proxy target is non-extensible", key.GoString())
	}
	return true, nil
}

// proxyOwnKeys implements [[OwnPropertyKeys]] (§10.5.11). The result keeps
// the trap's order.
func (r *Realm) proxyOwnKeys(o *Object) ([]PropertyKey, error) {
	if ns := nsOf(o); ns != nil {
		return nsOwnKeys(ns), nil
	}
	if err := r.proxyEnter(); err != nil {
		return nil, err
	}
	defer r.proxyLeave()
	target, handler, trap, err := r.proxyTrap(proxyOf(o), AtomOwnKeys)
	if err != nil {
		return nil, err
	}
	if trap.IsUndefined() {
		return r.ownPropertyKeys(target)
	}
	v, err := r.callTrap(trap, handler, ObjectValue(target))
	if err != nil {
		return nil, err
	}
	keys, err := r.proxyKeyList(v)
	if err != nil {
		return nil, err
	}
	if dup, err := hasDuplicateKey(r, keys); err != nil || dup {
		if err == nil {
			err = r.proxyError(AtomOwnKeys, "trap returned duplicate entries")
		}
		return nil, err
	}
	ext, err := r.isExtensible(target)
	if err != nil {
		return nil, err
	}
	targetKeys, err := r.ownPropertyKeys(target)
	if err != nil {
		return nil, err
	}
	var configurable, fixed []PropertyKey
	for i, k := range targetKeys {
		d, found, err := r.getOwnProperty(target, k)
		if err != nil {
			return nil, err
		}
		if found && !d.Configurable() {
			fixed = append(fixed, k)
		} else {
			configurable = append(configurable, k)
		}
		if err := interruptEvery(r, int64(i)); err != nil {
			return nil, err
		}
	}
	if ext && len(fixed) == 0 {
		return keys, nil
	}
	unchecked := make(map[PropertyKey]struct{}, len(keys))
	for _, k := range keys {
		unchecked[k] = struct{}{}
	}
	for _, k := range fixed {
		if _, ok := unchecked[k]; !ok {
			return nil, r.proxyError(AtomOwnKeys, "trap result did not include '%s'", k.GoString())
		}
		delete(unchecked, k)
	}
	if ext {
		return keys, nil
	}
	for _, k := range configurable {
		if _, ok := unchecked[k]; !ok {
			return nil, r.proxyError(AtomOwnKeys, "trap result did not include '%s'", k.GoString())
		}
		delete(unchecked, k)
	}
	if len(unchecked) != 0 {
		return nil, r.proxyError(AtomOwnKeys, "trap returned extra keys but proxy target is non-extensible")
	}
	return keys, nil
}

// proxyKeyList is CreateListFromArrayLike(v, « String, Symbol ») for the
// ownKeys trap result, as property keys. The list grows as it is read, so
// a huge length on an object with few elements fails at its first missing
// element (a TypeError, as in the spec) without allocating it; a list that
// reaches 2^24 keys is a RangeError.
func (r *Realm) proxyKeyList(v Value) ([]PropertyKey, error) {
	if !v.IsObject() {
		return nil, r.TypeError("CreateListFromArrayLike called on non-object")
	}
	o := v.AsObject()
	n, err := r.LengthOfArrayLike(o)
	if err != nil {
		return nil, err
	}
	keys := make([]PropertyKey, 0, min(n, 16))
	for i := range n {
		if i == 1<<24 {
			return nil, r.RangeError("Too many properties in the ownKeys trap result (only %d allowed)", 1<<24)
		}
		ev, err := o.GetIndex(r, uint32(i))
		if err != nil {
			return nil, err
		}
		switch {
		case ev.IsString():
			keys = append(keys, r.KeyFromString(ev.AsString()))
		case ev.IsSymbol():
			keys = append(keys, SymbolKey(ev.AsSymbol()))
		default:
			return nil, r.TypeError("%s is not a valid property name", r.DisplayString(ev))
		}
		if err := interruptEvery(r, i); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// hasDuplicateKey reports whether keys lists a key twice: pairwise for a
// short list, through a set otherwise.
func hasDuplicateKey(r *Realm, keys []PropertyKey) (bool, error) {
	if len(keys) <= 16 {
		for i, k := range keys {
			if slices.Contains(keys[:i], k) {
				return true, nil
			}
		}
		return false, nil
	}
	seen := make(map[PropertyKey]struct{}, len(keys))
	for i, k := range keys {
		if _, ok := seen[k]; ok {
			return true, nil
		}
		seen[k] = struct{}{}
		if err := interruptEvery(r, int64(i)); err != nil {
			return false, err
		}
	}
	return false, nil
}

// proxyCall implements [[Call]] (§10.5.12); it is the data-native behaviour
// of a callable proxy's FunctionData, reached through callOther with the
// call depth already counted.
func proxyCall(r *Realm, fd *FunctionData, this Value, args []Value) (Value, error) {
	target, handler, trap, err := r.proxyTrap(fd.nat().data.(*proxyData), AtomApply)
	if err != nil {
		return Undefined(), err
	}
	if trap.IsUndefined() {
		return r.CallObject(target, this, args)
	}
	argArray := r.NewArrayFromSlice(slices.Clone(args))
	return r.callTrap(trap, handler, ObjectValue(target), this, ObjectValue(argArray))
}

// proxyConstruct implements [[Construct]] (§10.5.13) for Construct's
// FuncProxy arm (the call depth is counted there).
func (r *Realm) proxyConstruct(pd *proxyData, args []Value, newTarget *Object) (Value, error) {
	target, handler, trap, err := r.proxyTrap(pd, AtomConstruct)
	if err != nil {
		return Undefined(), err
	}
	if trap.IsUndefined() {
		return r.Construct(ObjectValue(target), args, newTarget)
	}
	argArray := r.NewArrayFromSlice(slices.Clone(args))
	v, err := r.callTrap(trap, handler, ObjectValue(target), ObjectValue(argArray), ObjectValue(newTarget))
	if err != nil {
		return Undefined(), err
	}
	if !v.IsObject() {
		return Undefined(), r.proxyError(AtomConstruct, "trap returned non-object ('%s')", r.DisplayString(v))
	}
	return v, nil
}

// --- descriptors ------------------------------------------------------------------

// completePropertyDescriptor implements CompletePropertyDescriptor.
func completePropertyDescriptor(d *PropertyDescriptor) {
	if d.IsAccessorDescriptor() {
		if !d.HasGet() {
			d.SetGet(Undefined())
		}
		if !d.HasSet() {
			d.SetSet(Undefined())
		}
		d.attrs |= attrAccessor
	} else {
		if !d.HasValue() {
			d.SetValue(Undefined())
		}
		if !d.HasWritable() {
			d.SetWritable(false)
		}
	}
	if !d.HasEnumerable() {
		d.SetEnumerable(false)
	}
	if !d.HasConfigurable() {
		d.SetConfigurable(false)
	}
}

// fromPartialDescriptor implements FromPropertyDescriptor for a descriptor
// that may lack fields (the defineProperty trap's argument): the object has
// the present fields in the spec's order.
func (r *Realm) fromPartialDescriptor(d PropertyDescriptor) Value {
	const data = descHasValue | descHasWritable | descHasEnumerable | descHasConfigurable
	const accessor = descHasGet | descHasSet | descHasEnumerable | descHasConfigurable
	if d.present == data || d.present == accessor {
		return r.FromPropertyDescriptor(d)
	}
	o := r.NewObject()
	add := func(name *String, v Value) { o.DefineOwnDataFast(r, StringKey(name), v, attrDefault) }
	if d.HasValue() {
		add(AtomValue, d.Value)
	}
	if d.HasWritable() {
		add(AtomWritable, Bool(d.Writable()))
	}
	if d.HasGet() {
		add(AtomGet, d.Get)
	}
	if d.HasSet() {
		add(AtomSet, d.Set)
	}
	if d.HasEnumerable() {
		add(AtomEnumerable, Bool(d.Enumerable()))
	}
	if d.HasConfigurable() {
		add(AtomConfigurable, Bool(d.Configurable()))
	}
	return ObjectValue(o)
}

// --- fallible internal methods ------------------------------------------------------

// OwnKeys is o.[[OwnPropertyKeys]](): for a proxy the ownKeys trap runs
// and its error is returned. The Object methods see a proxy as an empty
// object without running code.
func (r *Realm) OwnKeys(o *Object) ([]PropertyKey, error) { return r.ownPropertyKeys(o) }

// EnumerableOwnKeys returns the own enumerable string keys as Object.keys
// does, through the ownKeys and getOwnPropertyDescriptor traps for a proxy.
func (r *Realm) EnumerableOwnKeys(o *Object) ([]PropertyKey, error) { return r.enumerableOwnKeys(o) }

// IsArray is the spec's IsArray: true for an array and for a proxy whose
// target is one (followed through nested proxies without running a trap),
// an error for a revoked proxy on the way. The package-level IsArray sees
// only arrays.
func (r *Realm) IsArray(v Value) (bool, error) { return r.isArray(v) }

// PrototypeOf is o.[[GetPrototypeOf]]() (the getPrototypeOf trap for a
// proxy; Object.Proto reads nil for one).
func (r *Realm) PrototypeOf(o *Object) (*Object, error) { return r.getPrototypeOf(o) }

// HasOwn is HasOwnProperty(o, key): the getOwnPropertyDescriptor trap for a
// proxy, whose error is returned; Object.HasOwnProperty is false for one.
func (r *Realm) HasOwn(o *Object, key PropertyKey) (bool, error) { return r.hasOwnProperty(o, key) }

// The dispatchers below are the object internal methods for an operand that
// may be a proxy: the ordinary method for any other object, the trap for a
// proxy. Code that reads an arbitrary object through the infallible Object
// methods sees a proxy as an empty null-prototype object instead.

// getPrototypeOf is o.[[GetPrototypeOf]]().
func (r *Realm) getPrototypeOf(o *Object) (*Object, error) {
	if o.class == ClassProxy {
		return r.proxyGetPrototypeOf(o)
	}
	return o.shape.proto, nil
}

// setPrototypeOf is o.[[SetPrototypeOf]](proto).
func (r *Realm) setPrototypeOf(o, proto *Object) (bool, error) {
	if o.class == ClassProxy {
		return r.proxySetPrototypeOf(o, proto)
	}
	return o.SetPrototypeOf(r, proto), nil
}

// isExtensible is IsExtensible(o).
func (r *Realm) isExtensible(o *Object) (bool, error) {
	if o.class == ClassProxy {
		return r.proxyIsExtensible(o)
	}
	return o.IsExtensible(), nil
}

// preventExtensions is o.[[PreventExtensions]]().
func (r *Realm) preventExtensions(o *Object) (bool, error) {
	switch o.class {
	case ClassProxy:
		return r.proxyPreventExtensions(o)
	case ClassTypedArray:
		if !dataOf(o, kindTypedArray).fixedLength() {
			return false, nil
		}
	}
	o.PreventExtensions(r)
	return true, nil
}

// getOwnProperty is o.[[GetOwnProperty]](key).
func (r *Realm) getOwnProperty(o *Object, key PropertyKey) (PropertyDescriptor, bool, error) {
	if o.class == ClassProxy {
		return r.proxyGetOwnProperty(o, key)
	}
	d, ok := o.GetOwnProperty(key)
	return d, ok, nil
}

// hasOwnProperty is HasOwnProperty(o, key).
func (r *Realm) hasOwnProperty(o *Object, key PropertyKey) (bool, error) {
	if o.class == ClassProxy {
		_, ok, err := r.proxyGetOwnProperty(o, key)
		return ok, err
	}
	return o.HasOwnProperty(key), nil
}

// isOwnEnumerable reports whether key is an own enumerable property of o.
func (r *Realm) isOwnEnumerable(o *Object, key PropertyKey) (bool, error) {
	if o.class == ClassProxy {
		d, ok, err := r.proxyGetOwnProperty(o, key)
		return ok && d.Enumerable(), err
	}
	c, ok := o.getOwnCell(key)
	return ok && c.attrs&attrEnumerable != 0, nil
}

// chainMayHave reports whether key may be found on the prototype chain
// starting at o without running user code: an object on it has it as an
// own property, or is a proxy.
func chainMayHave(o *Object, key PropertyKey) bool {
	for ; o != nil; o = o.Proto() {
		if o.class == ClassProxy || o.HasOwnProperty(key) {
			return true
		}
	}
	return false
}

// hasProperty is o.[[HasProperty]](key).
func (r *Realm) hasProperty(o *Object, key PropertyKey) (bool, error) {
	for obj := o; obj != nil; obj = obj.Proto() {
		if obj.class == ClassProxy {
			return r.proxyHas(obj, key)
		}
		if obj.HasOwnProperty(key) {
			return true, nil
		}
		if obj.class == ClassTypedArray && typedArrayKey(key) {
			return false, nil
		}
	}
	return false, nil
}

// deleteProperty is o.[[Delete]](key).
func (r *Realm) deleteProperty(o *Object, key PropertyKey) (bool, error) {
	if o.class == ClassProxy {
		return r.proxyDelete(o, key)
	}
	return o.Delete(r, key), nil
}

// ownPropertyKeys is o.[[OwnPropertyKeys]]().
func (r *Realm) ownPropertyKeys(o *Object) ([]PropertyKey, error) {
	switch o.class {
	case ClassProxy:
		return r.proxyOwnKeys(o)
	case ClassTypedArray:
		if err := r.typedArrayKeysLimit(o); err != nil {
			return nil, err
		}
	}
	return o.OwnPropertyKeys(), nil
}

// enumerableOwnKeys is the key list of EnumerableOwnProperties(o, key):
// the own enumerable string keys, through the ownKeys and
// getOwnPropertyDescriptor traps for a proxy.
func (r *Realm) enumerableOwnKeys(o *Object) ([]PropertyKey, error) {
	return r.countedEnumerableOwnKeys(o, nil)
}

// countedEnumerableOwnKeys is enumerableOwnKeys for a caller that counts
// its work in *work and checks the interrupt every 4096 units
// (JSON.stringify): every key the walk visits is a unit, the non-enumerable
// and symbol keys it drops included, so that many references to an object
// with many such keys cannot run far past an interrupt. A proxy's keys are
// counted as they are visited, an object's once its walk is done. work may
// be nil.
func (r *Realm) countedEnumerableOwnKeys(o *Object, work *int) ([]PropertyKey, error) {
	if o.class != ClassProxy {
		if o.class == ClassTypedArray {
			if err := r.typedArrayKeysLimit(o); err != nil {
				return nil, err
			}
		}
		keys := o.OwnEnumerableStringKeys()
		if work == nil {
			return keys, nil
		}
		n := o.elemLen()
		if o.dictTable() != nil {
			n += len(o.dictTable().sparse)
		}
		if o.flags&flagDict != 0 {
			n += len(o.dictTable().entries)
		} else {
			n += int(o.shape.count)
		}
		return keys, r.countWork(work, n)
	}
	keys, err := r.proxyOwnKeys(o)
	if err != nil {
		return nil, err
	}
	out := keys[:0]
	for _, k := range keys {
		if work != nil {
			if err := r.countWork(work, 1); err != nil {
				return nil, err
			}
		}
		if k.IsSymbol() {
			continue
		}
		d, ok, err := r.proxyGetOwnProperty(o, k)
		if err != nil {
			return nil, err
		}
		if ok && d.Enumerable() {
			out = append(out, k)
		}
	}
	return out, nil
}

// countWork adds n units to *work and checks the interrupt when the count
// passes a multiple of 4096, as JSON.stringify's tick does one unit at a
// time.
func (r *Realm) countWork(work *int, n int) error {
	w := *work
	*work = w + n
	if (w+n)>>12 == w>>12 {
		return nil
	}
	return r.CheckInterrupt()
}

// checkFunctionRealm is GetFunctionRealm(o) (ECMA-262 §7.3.24) with one
// realm: it can only fail, on a revoked proxy on o's chain of targets.
func (r *Realm) checkFunctionRealm(o *Object) error {
	for i := int64(0); o.class == ClassProxy && nsOf(o) == nil; i++ {
		pd := proxyOf(o)
		if pd.handler == nil {
			return r.TypeError("Cannot perform 'GetFunctionRealm' on a proxy that has been revoked")
		}
		o = pd.target
		if err := interruptEvery(r, i); err != nil {
			return err
		}
	}
	return nil
}

// isArray implements IsArray: a proxy is an array when its target is.
func (r *Realm) isArray(v Value) (bool, error) {
	if !v.IsObject() {
		return false, nil
	}
	o := v.AsObject()
	for i := int64(0); o.class == ClassProxy && nsOf(o) == nil; i++ {
		pd := proxyOf(o)
		if pd.handler == nil {
			return false, r.TypeError("Cannot perform 'IsArray' on a proxy that has been revoked")
		}
		o = pd.target
		if err := interruptEvery(r, i); err != nil {
			return false, err
		}
	}
	return o.class == ClassArray, nil
}

// receiverSet is the receiver half of OrdinarySetWithOwnDescriptor for a
// data property: it updates key's value on recv, or creates key when recv
// lacks it, through recv's [[GetOwnProperty]] and [[DefineOwnProperty]].
func (r *Realm) receiverSet(recv *Object, key PropertyKey, v Value) (bool, error) {
	if recv.flags&flagShared != 0 { // frozen: nothing to update or create
		return false, nil
	}
	existing, ok, err := r.getOwnProperty(recv, key)
	if err != nil {
		return false, err
	}
	if !ok {
		return recv.createAbsent(r, key, v)
	}
	if existing.IsAccessorDescriptor() || !existing.Writable() {
		return false, nil
	}
	var d PropertyDescriptor
	d.SetValue(v)
	return recv.DefineOwnProperty(r, key, d)
}

// setIntegrityLevel implements SetIntegrityLevel through the internal
// methods, for a proxy (Object.seal and Object.freeze keep their fast
// ordinary paths).
func (r *Realm) setIntegrityLevel(o *Object, frozen bool) (bool, error) {
	ok, err := r.preventExtensions(o)
	if err != nil || !ok {
		return false, err
	}
	keys, err := r.ownPropertyKeys(o)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		var d PropertyDescriptor
		d.SetConfigurable(false)
		if frozen {
			cur, found, err := r.getOwnProperty(o, k)
			if err != nil {
				return false, err
			}
			if !found {
				continue
			}
			if !cur.IsAccessorDescriptor() {
				d.SetWritable(false)
			}
		}
		if err := o.DefinePropertyOrThrow(r, k, d); err != nil {
			return false, err
		}
	}
	return true, nil
}

// testIntegrityLevel implements TestIntegrityLevel through the internal
// methods, for a proxy.
func (r *Realm) testIntegrityLevel(o *Object, frozen bool) (bool, error) {
	ext, err := r.isExtensible(o)
	if err != nil || ext {
		return false, err
	}
	keys, err := r.ownPropertyKeys(o)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		d, found, err := r.getOwnProperty(o, k)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		if d.Configurable() || frozen && d.IsDataDescriptor() && d.Writable() {
			return false, nil
		}
	}
	return true, nil
}

// proxyToGo exports a non-callable proxy through its traps, as
// JSON.stringify reads it: []any over its length and [[Get]] when IsArray
// says it is an array, else a map of the keys of
// EnumerableOwnProperties(key) and their [[Get]] values. A throwing trap,
// or a revoked proxy, fails a strict export; a lenient one exports nil for
// the proxy.
func (r *Realm) proxyToGo(o *Object, v Value, st *toGoState) (any, error) {
	fail := func(err error) (any, error) {
		if st.strict {
			return nil, err
		}
		return nil, nil
	}
	isArr, err := r.isArray(v)
	if err != nil {
		return fail(err)
	}
	if isArr {
		n, err := r.LengthOfArrayLike(o)
		if err != nil {
			return fail(err)
		}
		// Grown by append, the length being the trap's word rather than
		// storage, and so recorded for cycles only once complete: a proxy
		// array that contains itself nests up to MaxToGoDepth.
		items := make([]any, 0, min(n, 1024))
		for i := int64(0); i < n; i++ {
			if err := interruptEvery(r, i); err != nil {
				return nil, err
			}
			ev, err := r.proxyGet(o, indexKey(r, i), v)
			if err != nil {
				return fail(err)
			}
			item, err := r.toGo(ev, st)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		st.add(o, items)
		return items, nil
	}
	keys, err := r.enumerableOwnKeys(o)
	if err != nil {
		return fail(err)
	}
	out := make(map[string]any, len(keys))
	st.add(o, out)
	for _, k := range keys {
		ev, err := r.proxyGet(o, k, v)
		if err != nil {
			if st.strict {
				return nil, err
			}
			ev = Undefined()
		}
		if out[k.GoString()], err = r.toGo(ev, st); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// copyProxyDataProps is copyDataProps for a proxy source: the ownKeys,
// getOwnPropertyDescriptor and get traps decide the copied properties.
func (r *Realm) copyProxyDataProps(target, from *Object, excl []PropertyKey) error {
	keys, err := r.proxyOwnKeys(from)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if slices.Contains(excl, k) {
			continue
		}
		d, ok, err := r.proxyGetOwnProperty(from, k)
		if err != nil {
			return err
		}
		if !ok || !d.Enumerable() {
			continue
		}
		v, err := r.proxyGet(from, k, ObjectValue(from))
		if err != nil {
			return err
		}
		if err := target.CreateDataPropertyOrThrow(r, k, v); err != nil {
			return err
		}
	}
	return nil
}

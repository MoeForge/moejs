package engine

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deferredProtos names the objects installDeferred (installKeys for Math and
// Reflect) fills on first touch in a mutable realm, with a JS expression for
// each.
var deferredProtos = []struct {
	expr string
	obj  func(r *Realm) *Object
}{
	{"Map.prototype", func(r *Realm) *Object { return r.MapPrototype }},
	{"Set.prototype", func(r *Realm) *Object { return r.SetPrototype }},
	{"WeakMap.prototype", func(r *Realm) *Object { return r.WeakMapPrototype }},
	{"WeakSet.prototype", func(r *Realm) *Object { return r.WeakSetPrototype }},
	{"WeakRef.prototype", func(r *Realm) *Object { return r.WeakRefPrototype }},
	{"Symbol.prototype", func(r *Realm) *Object { return r.SymbolPrototype }},
	{"Object.getPrototypeOf(/a/g[Symbol.matchAll]('a'))", func(r *Realm) *Object { return r.RegExpStringIteratorPrototype }},
	{"Array.prototype[Symbol.unscopables]", func(r *Realm) *Object {
		v, _ := r.ArrayPrototype.GetOwnProperty(SymbolKey(SymUnscopables))
		return v.Value.AsObject()
	}},
	{"Math", func(r *Realm) *Object { return r.Math }},
	{"Reflect", func(r *Realm) *Object {
		v, _ := r.Global.GetOwnProperty(StringKey(AtomReflect))
		return v.Value.AsObject()
	}},
	{"Object.getPrototypeOf(new Map().entries())", func(r *Realm) *Object { return r.MapIteratorPrototype }},
	{"Object.getPrototypeOf(new Set().values())", func(r *Realm) *Object { return r.SetIteratorPrototype }},
}

// TestPrototypesDeferred checks that a mutable realm defines each
// deferred builtin object on the first look at it, whichever way that
// happens, with the keys of a shared realm (whose frozen intrinsics are not
// writable), and leaves the others alone.
func TestPrototypesDeferred(t *testing.T) {
	r := NewRealm()
	for _, d := range deferredProtos[:10] {
		assert.NotZero(t, d.obj(r).flags&flagHasLazy, d.expr)
	}
	v, err := r.Construct(ObjectValue(r.MapCtor), nil, nil)
	assert.NoError(t, err)
	assert.NotZero(t, r.MapPrototype.flags&flagHasLazy, "new Map() looks at no property")
	callMethod(t, r, v, "set", IntValue(1), IntValue(2))
	assert.Zero(t, r.MapPrototype.flags&flagHasLazy)
	assert.Equal(t, 2, len(r.MapIteratorPrototype.slots), "Map.prototype defines %MapIteratorPrototype%")
	assert.NotZero(t, r.SetPrototype.flags&flagHasLazy)
	assert.NotZero(t, r.SymbolPrototype.flags&flagHasLazy)

	const keys = "Reflect.ownKeys(%s).map(k => { const d = Object.getOwnPropertyDescriptor(%s, k); return String(k) + ':' + d.enumerable + (typeof d.get) }).join()"
	for _, d := range deferredProtos {
		src := "export function f() { return " + fmt.Sprintf(keys, d.expr, d.expr) + "; }"
		want := evalModuleWith(t, src, RealmOptions{SharedIntrinsics: true}).call("f")
		assert.NotEmpty(t, want, d.expr)
		assert.Equal(t, want, evalModule(t, src).call("f"), d.expr)
	}
}

// TestPrototypesFirstTouch runs each first touch in a fresh mutable
// realm.
func TestPrototypesFirstTouch(t *testing.T) {
	for src, res := range map[string]any{
		"new Map([[1, 2], [3, 4]]).get(3)": "4",
		"new Set([1, 1, 2]).size":          "2",
		"[new WeakMap([[Object, 1]]).get(Object), new WeakSet([Object]).has(Object)]":                             "1,true",
		"typeof new WeakRef({}).deref()":                                                                          "object",
		"[...new Set([1, 2])].concat([...new Map([[3, 4]]).keys()])":                                              "1,2,3",
		"Array.from(new Map([[1, 2]]), ([k, v]) => k + v)":                                                        "3",
		"(() => { let s = 0; for (const [k, v] of new Map([[1, 2], [3, 4]])) s += k * v; return s })()":           "14",
		"Object.prototype.toString.call(new Set().entries()) + String(Map.prototype)":                             "[object Set Iterator][object Map]",
		"new (class extends Map { set(k, v) { return super.set(k, v * 2) } })([[1, 2]]).get(1)":                   "4",
		"Map.prototype.set = function (k, v) { this.n = k + v; return this }, new Map([[1, 2]]).n":                "3",
		"Object.freeze(Map.prototype), [Object.isFrozen(Map.prototype), new Map([[1, 2]]).size]":                  "true,1",
		"Object.preventExtensions(Set.prototype), [Object.isExtensible(Set.prototype), typeof Set.prototype.add]": "false,function",
		"WeakMap.prototype.x = 1, Object.getOwnPropertyNames(WeakMap.prototype).slice(-2)":                        "set,x",
		"delete WeakSet.prototype.add, [typeof WeakSet.prototype.add, 'has' in WeakSet.prototype]":                "undefined,true",
		"[Symbol('a').description, Object(Symbol('b')).toString(), typeof Object(Symbol()).valueOf()]":            "a,Symbol(b),symbol",
		"[Object(Symbol.iterator) == Symbol.iterator, Symbol.prototype[Symbol.toStringTag]]":                      "true,Symbol",
		"[...'a1b2'.matchAll(/\\d/g)].join('') + String('x'.matchAll(/x/g))":                                      "12[object RegExp String Iterator]",
		"[Array.prototype[Symbol.unscopables].flat, Object.getPrototypeOf(Array.prototype[Symbol.unscopables])]":  "true,",
		"[Math.max(1, 2), Math.PI > 3, String(Math), Object.keys(Math).length]":                                   "2,true,[object Math],0",
		"Math.x = 1, delete Math.abs, [Object.getOwnPropertyNames(Math).slice(-2), typeof Math.abs]":              "trunc,x,undefined",
		"[Reflect.ownKeys({a: 1}), Reflect.has(Reflect, 'get'), String(Reflect)]":                                 "a,true,[object Reflect]",
		// Inline caches filled on an object of a deferred prototype's shape.
		"(() => { class A {}; const f = o => o.toString; f(A.prototype), f(A.prototype); return [f(Symbol.prototype), f(Date.prototype)].map(g => g === Object.prototype.toString) })()":                               "false,false",
		"(() => { const d = Object.defineProperty({}, 'constructor', {value: 1, writable: true, configurable: true}); Object.prototype.get = 1; const f = o => o.get; f(d), f(d); return typeof f(Map.prototype) })()": "function",
	} {
		assert.Equal(t, res, evalModule(t, "export function f() { return String(("+src+")); }").call("f"), src)
	}
}

// TestNamespacesByKey checks that a mutable realm defines a Math or
// Reflect property on the first lookup of its key, lays all of them out in
// the order of a shared realm once anything else looks, and that no inline
// cache filled on another object skips a pending one.
func TestNamespacesByKey(t *testing.T) {
	fx := evalModule(t, "export function f() { return Math.abs(-1) + Math.PI; }")
	fx.call("f")
	r := fx.r
	assert.NotZero(t, r.Math.flags&flagHasLazy)
	assert.Equal(t, 2, len(r.Math.slots))
	assert.NotSame(t, r.plainRoot, r.Math.shape.root(), "a lazy namespace shares no shape")
	reflect, _ := r.Global.GetProp(r, key(r, "Reflect"))
	assert.Same(t, r.Math.shape.root(), reflect.AsObject().shape, "but the root of the namespaces")

	const order = "Reflect.ownKeys(%[1]s).map(k => String(k) + ':' + Object.getOwnPropertyDescriptor(%[1]s, k).enumerable).join()"
	const all = "Reflect.ownKeys(Math), Reflect.ownKeys(Reflect), "
	for _, pre := range []string{"0", "Math.floor, Math.E, Reflect.set, Reflect.apply", "Math.trunc = 1, Reflect.x = 2", "Object.setPrototypeOf(Math, null), delete Reflect.get"} {
		for _, ns := range []string{"Math", "Reflect"} {
			src := "export function f() { " + pre + "; return " + fmt.Sprintf(order, ns) + "; }"
			want := evalModule(t, strings.Replace(src, "{ ", "{ "+all, 1)).call("f")
			if !strings.ContainsAny(pre, "=(") {
				want = evalModuleWith(t, src, RealmOptions{SharedIntrinsics: true}).call("f")
			}
			assert.Equal(t, want, evalModule(t, src).call("f"), pre)
		}
	}
	for src, res := range map[string]any{
		"(() => { const f = Math.floor, a = Reflect.apply; Object.keys(Math), Object.freeze(Reflect); return [f === Math.floor, a === Reflect.apply] })()":          "true,true",
		"(() => { Object.prototype.max = 1; const g = o => typeof o.max; g({}), g({}); return [g(Math), delete Object.prototype.max] })()":                          "function,true",
		"(() => { const s = o => { o.y = 1 }; s({}), s({}), s(Math); return [Math.y, typeof Math.abs, Object.getOwnPropertyNames(Math).at(-1)] })()":                "1,function,y",
		"(() => { const o = Object.create(Math), g = x => x.abs; g(o), g(o); Math.floor, Object.keys(Math); return [g(o) === Math.abs, typeof o.sqrt] })()":         "true,function",
		"(() => { const o = Object.create(Reflect); return [typeof o.has, Object.getPrototypeOf(o) === Reflect, Reflect.ownKeys(Reflect).length] })()":              "function,true,14",
		"(() => { let t = o => o[Symbol.toStringTag], g = o => o.has; t(Reflect), t(Math), g(Reflect), g(Reflect); return [t(Math), t(Reflect), g(Math)] })()":      "Math,Reflect,",
		"(() => { Object.defineProperty(Math, 'max', {enumerable: true}); return [Object.keys(Math), Math.max(1, 3), Object.getOwnPropertyNames(Math).length] })()": "max,3,45",
		"(() => { let t = o => o[Symbol.toStringTag], g = o => o.add; t(Math), t(Math), g(Atomics), g(Atomics); return [t(Atomics), g(Math)] })()":                  "Atomics,",
	} {
		assert.Equal(t, res, evalModule(t, "export function f() { return String(("+src+")); }").call("f"), src)
	}
}

// TestColdGlobalsShared checks that a shared realm's global object starts
// with the template's slots before the coldGlobalKeys bindings and defines
// each on the first lookup of its key, whichever way that happens, once.
func TestColdGlobalsShared(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	g := r.Global
	assert.NotZero(t, g.flags&flagHasLazy)
	assert.Equal(t, len(sharedTpl.globalSlots), len(g.slots))
	late := 0
	for _, install := range coldGlobalLate {
		if install != nil {
			late++
		}
	}
	assert.Equal(t, len(sharedTpl.globalSlots)+len(coldGlobalKeys)-late, len(NewRealm().Global.slots))
	v, ok := g.GetOwnDataValue(StringKey(AtomMapName))
	assert.True(t, ok)
	assert.Equal(t, ObjectValue(r.MapCtor), v)
	assert.Equal(t, uint32(1<<len(coldGlobalKeys)-1)&^2, r.coldGlobals)
	assert.NoError(t, g.SetProp(r, r.KeyFromGoString("utils"), IntValue(1)))
	assert.Equal(t, len(sharedTpl.globalSlots)+2, len(g.slots), "a host global defines no cold binding")
	names := func(r *Realm) []string {
		var s []string
		for _, k := range r.Global.OwnPropertyKeys() {
			s = append(s, k.GoString())
		}
		return s
	}
	assert.ElementsMatch(t, append(names(NewRealm()), "utils"), names(r))
	assert.Zero(t, g.flags&flagHasLazy)
	assert.Nil(t, g.internal)

	for src, res := range map[string]any{
		"[typeof Symbol, typeof WeakRef, typeof structuredClone]":                                                                                      "function,function,function",
		"new Map([[1, 2]]).get(1) + new Set([3]).size":                                                                                                 "3",
		"[globalThis.structuredClone === structuredClone, 'WeakSet' in globalThis, 'Nope' in globalThis]":                                              "true,true,false",
		"(d => [d.writable, d.enumerable, d.configurable, d.value === AggregateError])(Object.getOwnPropertyDescriptor(globalThis, 'AggregateError'))": "false,false,false,true",
		"(() => { try { delete globalThis.Map } catch (e) { return [e.name, typeof Map, typeof Set] } })()":                                            "TypeError,function,function",
		"(() => { try { WeakMap = 1 } catch (e) { return e.name + typeof WeakMap } })()":                                                               "TypeErrorfunction",
		"Object.defineProperty(globalThis, 'Set', {value: Set}) === globalThis":                                                                        "true",
		"Object.freeze(globalThis), [Object.isFrozen(globalThis), typeof Symbol]":                                                                      "true,function",
		"Object.create(globalThis).WeakRef === WeakRef":                                                                                                "true",
		"Object.getOwnPropertyNames(globalThis).slice(-32)":                                                                                            "Symbol,Map,Set,WeakMap,WeakSet,WeakRef,AggregateError,structuredClone,BigInt,Promise,queueMicrotask,Proxy,ArrayBuffer,SharedArrayBuffer,DataView,Int8Array,Uint8Array,Uint8ClampedArray,Int16Array,Uint16Array,Int32Array,Uint32Array,Float16Array,Float32Array,Float64Array,BigInt64Array,BigUint64Array,Atomics,TextEncoder,TextDecoder,eval,Intl",
		"typeof Set, Object.getOwnPropertyNames(globalThis).slice(-32)":                                                                                "Set,Symbol,Map,WeakMap,WeakSet,WeakRef,AggregateError,structuredClone,BigInt,Promise,queueMicrotask,Proxy,ArrayBuffer,SharedArrayBuffer,DataView,Int8Array,Uint8Array,Uint8ClampedArray,Int16Array,Uint16Array,Int32Array,Uint32Array,Float16Array,Float32Array,Float64Array,BigInt64Array,BigUint64Array,Atomics,TextEncoder,TextDecoder,eval,Intl",
		"typeof DataView, Object.getOwnPropertyNames(globalThis).slice(-23)":                                                                           "BigInt,Promise,queueMicrotask,Proxy,ArrayBuffer,SharedArrayBuffer,Int8Array,Uint8Array,Uint8ClampedArray,Int16Array,Uint16Array,Int32Array,Uint32Array,Float16Array,Float32Array,Float64Array,BigInt64Array,BigUint64Array,Atomics,TextEncoder,TextDecoder,eval,Intl",
		"typeof Uint8Array, Object.getOwnPropertyNames(globalThis).slice(-17)":                                                                         "DataView,Int8Array,Uint8ClampedArray,Int16Array,Uint16Array,Int32Array,Uint32Array,Float16Array,Float32Array,Float64Array,BigInt64Array,BigUint64Array,Atomics,TextEncoder,TextDecoder,eval,Intl",
		"globalThis.utils = 1, [typeof Map, Object.keys(globalThis)]":                                                                                  "function,utils",
		"(() => { let n = 0; for (let i = 0; i < 3; i++) n += (typeof JSON) + (typeof Map); return n })()":                                             "0objectfunctionobjectfunctionobjectfunction",
	} {
		f := evalModuleWith(t, "export function f() { return String(("+src+")); }", RealmOptions{SharedIntrinsics: true})
		assert.Equal(t, res, f.call("f"), src)
	}
}

// TestColdGlobalIndex checks the coldGlobalSlot table: every pending cold
// binding is found, and no other key is, a defined binding included.
func TestColdGlobalIndex(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	cold := map[PropertyKey]bool{}
	for i, k := range coldGlobalKeys {
		assert.Equal(t, i, r.coldGlobalIndex(k), k.GoString())
		cold[k] = true
	}
	for _, a := range staticAtoms {
		if k := StringKey(a); !cold[k] {
			assert.Negative(t, r.coldGlobalIndex(k), a.GoString())
		}
	}
	for _, k := range []PropertyKey{IndexKey(0), SymbolKey(SymIterator), r.KeyFromGoString("console"), r.KeyFromGoString("utils")} {
		assert.Negative(t, r.coldGlobalIndex(k), k.GoString())
	}
	r.ensureLate(StringKey(AtomDataView))
	assert.Negative(t, r.coldGlobalIndex(StringKey(AtomDataView)), "a defined binding")
	assert.Equal(t, lateArrayBuffer, r.coldGlobalIndex(StringKey(AtomArrayBuffer)))
}

// TestLateGlobalsMutable checks that a mutable realm defines the late
// bindings on first use, whichever way that happens, and that ensureLate
// installs them without a lookup.
func TestLateGlobalsMutable(t *testing.T) {
	r := NewRealm()
	g := r.Global
	key := StringKey(AtomStructuredClone)
	assert.NotZero(t, g.flags&flagHasLazy)
	_, _, ok := g.shape.Lookup(key)
	assert.False(t, ok)
	r.ensureLate(key)
	assert.Negative(t, r.coldGlobalIndex(key))
	assert.NotZero(t, g.flags&flagHasLazy, "the other late bindings stay cold")
	v, ok := g.GetOwnDataValue(key)
	assert.True(t, ok)
	assert.True(t, IsCallable(v))
	r.ensureLate(key)
	w, _ := g.GetOwnDataValue(key)
	assert.Equal(t, v, w)
	r.ensureLate(StringKey(AtomBigInt))
	r.ensureLate(StringKey(AtomPromise))
	r.ensureLate(StringKey(AtomQueueMicrotask))
	assert.NotZero(t, r.coldGlobals&(1<<lateIndex(StringKey(AtomProxy))), "Proxy stays cold")
	r.ensureLate(StringKey(AtomProxy))
	assert.NotZero(t, r.coldGlobals, "the binary data globals are still pending")
	r.ensureLate(StringKey(AtomSharedArrayBuffer))
	lateEval := lateIndex(StringKey(AtomEval))
	assert.Equal(t, uint32(1<<lateTextEncoder|1<<(lateTextEncoder+1)|1<<lateEval|1<<lateIntl), r.coldGlobals, "one installer binds ArrayBuffer, SharedArrayBuffer and DataView")
	r.ensureLate(StringKey(AtomTextDecoder))
	assert.Equal(t, uint32(1<<lateEval|1<<lateIntl), r.coldGlobals, "and one TextEncoder and TextDecoder")
	r.ensureLate(StringKey(AtomEval))
	assert.Equal(t, uint32(1<<lateIntl), r.coldGlobals, "eval is a group of its own")
	r.ensureLate(StringKey(AtomIntl))
	assert.Zero(t, r.coldGlobals, "and so is Intl")
	assert.Zero(t, g.flags&flagHasLazy)
	assert.Nil(t, g.internal)

	for src, res := range map[string]any{
		"typeof structuredClone":            "function",
		"structuredClone([1, {a: 2}])[1].a": "2",
		"(d => [d.writable, d.enumerable, d.configurable])(Object.getOwnPropertyDescriptor(globalThis, 'structuredClone'))": "true,false,true",
		"globalThis.structuredClone = 1, structuredClone":                                                                   "1",
		"delete globalThis.structuredClone, typeof structuredClone":                                                         "undefined",
		"'structuredClone' in globalThis":                                                                                   "true",
		"Object.getOwnPropertyNames(globalThis).slice(-25)":                                                                 "structuredClone,BigInt,Promise,queueMicrotask,Proxy,ArrayBuffer,SharedArrayBuffer,DataView,Int8Array,Uint8Array,Uint8ClampedArray,Int16Array,Uint16Array,Int32Array,Uint32Array,Float16Array,Float32Array,Float64Array,BigInt64Array,BigUint64Array,Atomics,TextEncoder,TextDecoder,eval,Intl",
		"typeof DataView, Object.getOwnPropertyNames(globalThis).slice(-25)":                                                "ArrayBuffer,SharedArrayBuffer,DataView,Int8Array,Uint8Array,Uint8ClampedArray,Int16Array,Uint16Array,Int32Array,Uint32Array,Float16Array,Float32Array,Float64Array,BigInt64Array,BigUint64Array,Atomics,structuredClone,BigInt,Promise,queueMicrotask,Proxy,TextEncoder,TextDecoder,eval,Intl",
		"typeof Atomics, Object.getOwnPropertyNames(globalThis).slice(-25)":                                                 "ArrayBuffer,SharedArrayBuffer,DataView,Int8Array,Uint8Array,Uint8ClampedArray,Int16Array,Uint16Array,Int32Array,Uint32Array,Float16Array,Float32Array,Float64Array,BigInt64Array,BigUint64Array,Atomics,structuredClone,BigInt,Promise,queueMicrotask,Proxy,TextEncoder,TextDecoder,eval,Intl",
		"Object.preventExtensions(globalThis), typeof structuredClone":                                                      "function",
		"Object.freeze(globalThis), Object.isFrozen(globalThis) && typeof structuredClone":                                  "function",
	} {
		assert.Equal(t, res, evalModule(t, "export function f() { return String(("+src+")); }").call("f"), src)
	}
}

// TestLateGroupsShared checks the late groups of the shared realms: the
// template has none of their objects, the first use in any shared realm
// builds a group once for the process (frozen and shared like the
// template), and a realm defines only the bindings it uses, whatever other
// realms did.
func TestLateGroupsShared(t *testing.T) {
	shared := RealmOptions{SharedIntrinsics: true}
	b := NewRealmWith(shared) // before any group it may see built
	tpl := sharedTpl.Intrinsics
	for _, k := range coldGlobalKeys[firstLateGlobal:] {
		assert.NotContains(t, sharedTpl.globalShape.Props(), shapeProp{k, attrFrozen}, k.GoString())
	}

	a := NewRealmWith(shared)
	a.ensureLate(StringKey(AtomPromise))
	assert.Zero(t, a.coldGlobals&(1<<latePromise))
	assert.NotZero(t, a.coldGlobals&(1<<lateArrayBuffer), "the other groups stay pending")
	p := a.promise
	assert.NotNil(t, p)
	assert.NotZero(t, p.proto.flags&flagShared)
	assert.True(t, p.proto.IsFrozen())
	assert.NotZero(t, p.proto.shape.shared)
	v, ok := a.Global.GetOwnDataValue(StringKey(AtomPromise))
	assert.True(t, ok)
	assert.Equal(t, ObjectValue(p.ctor), v)
	assert.Same(t, p, NewRealmWith(shared).promiseIntr(), "built once per process")
	hp, _, _ := NewRealmWith(shared).NewPromiseWithResolvers()
	assert.Same(t, p.proto, hp.proto, "the host API defines the group too")
	// Each group joins a copy of the template's intrinsics.
	assert.Nil(t, tpl.BigIntPrototype)
	assert.Nil(t, tpl.promise)
	assert.Nil(t, tpl.binary)

	// b adopts the latest intrinsics with its first late binding but keeps
	// the others pending.
	b.lateAt(lateBigInt)
	assert.NotNil(t, b.BigIntPrototype)
	assert.NotNil(t, b.promise)
	assert.NotZero(t, b.coldGlobals&(1<<latePromise))
	assert.Same(t, p, b.promiseIntr())

	for src, res := range map[string]any{
		"[10n.toString(), typeof BigInt, BigInt.asIntN(8, 255n), Object.getOwnPropertyNames(BigInt.prototype)]": "10,function,-1,toString,valueOf,constructor,toLocaleString",
		"[Object(1n) instanceof BigInt, Object.prototype.toString.call(1n), Object.isFrozen(BigInt.prototype)]": "true,[object BigInt],true",
		"(() => { try { BigInt.prototype.x = 1 } catch (e) { return e.name } })()":                              "TypeError",
		"[Object.getOwnPropertyNames(globalThis).slice(-20, -17), Object.isFrozen(Promise.prototype)]":          "ArrayBuffer,SharedArrayBuffer,DataView,true",
		"new DataView(new ArrayBuffer(8), 2).byteLength + new SharedArrayBuffer(3).slice(1).byteLength":         "8",
		"[new Proxy({}, {get: (t, k) => k}).x, Object.isFrozen(Proxy), typeof Proxy.revocable]":                 "x,true,function",
	} {
		f := evalModuleWith(t, "export function f() { return String(("+src+")); }", shared)
		assert.Equal(t, res, f.call("f"), src)
	}
	keys := func(o *Object) (s []string) {
		for _, k := range o.OwnPropertyKeys() {
			s = append(s, k.GoString())
		}
		return s
	}
	m := NewRealm()
	m.lateAt(lateBigInt)
	assert.Equal(t, keys(m.BigIntPrototype), keys(b.BigIntPrototype), "as in a mutable realm")
}

// TestLateGroupsCrossRealm calls the natives of a group from a shared realm
// that has not defined it: they run in the caller's realm, which defines
// the group's binding as the natives need its intrinsics.
func TestLateGroupsCrossRealm(t *testing.T) {
	shared := RealmOptions{SharedIntrinsics: true}
	a := evalModuleWith(t, `
export const buf = new ArrayBuffer(8);
export const slice = ArrayBuffer.prototype.slice;
export const then = Promise.prototype.then;
export const p = Promise.resolve(1);
export const DV = DataView;
`, shared)
	get := func(name string) Value {
		v, ok := a.env.GetBindingValue(name)
		require.True(t, ok)
		return v
	}
	b := NewRealmWith(shared)
	res, err := b.Call(get("slice"), get("buf"), []Value{IntValue(2)})
	require.NoError(t, err)
	assert.Equal(t, a.r.binary.ArrayBufferPrototype, res.AsObject().proto)
	assert.Zero(t, b.coldGlobals&(1<<lateArrayBuffer))
	assert.NotZero(t, b.coldGlobals&(1<<latePromise))
	_, err = b.Call(get("then"), get("p"), nil)
	require.NoError(t, err)
	assert.Zero(t, b.coldGlobals&(1<<latePromise))
	res, err = b.Construct(get("DV"), []Value{get("buf")}, nil)
	require.NoError(t, err)
	assert.Equal(t, a.r.binary.DataViewPrototype, res.AsObject().proto)
}

// TestLateGroupsConcurrent has the shared realms of many goroutines use the
// late globals first, in different orders, in a process that has built none
// of their groups yet: a child process, so run it with -race. Every realm
// must see the same, fully built group.
func TestLateGroupsConcurrent(t *testing.T) {
	if os.Getenv("MOEJS_LATE_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLateGroupsConcurrent$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "MOEJS_LATE_CHILD=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "child process failed:\n%s", out)
		return
	}
	for i := range lateCells {
		require.Nil(t, lateCells[i].Load(), "a fresh process has built no group")
	}
	require.Nil(t, globalInternASCII.lookup("transferToFixedLength"), "nor interned a name only a group defines")
	uses := []struct{ src, res string }{
		{"new ArrayBuffer(8).slice(2).byteLength", "6"},
		{"new DataView(new SharedArrayBuffer(4), 1).byteLength", "3"},
		{"(10n ** 20n).toString(16)", "56bc75e2d63100000"},
		{"typeof BigInt.asUintN", "function"},
		{"Promise.resolve(1) instanceof Promise", "true"},
		{"typeof queueMicrotask", "function"},
		{"structuredClone({a: [1]}).a[0]", "1"},
		{"new Proxy({}, {get: () => 7}).x", "7"},
		{"new TextDecoder().decode(new TextEncoder().encode('é'))", "é"},
		{"Object.getOwnPropertyNames(globalThis).length", "?"},
	}
	var codes []*bytecode.Function
	for i := range uses {
		// Goroutine i uses the globals from uses[i] on, wrapping around.
		var b strings.Builder
		b.WriteString("export function f() { return [")
		for j := range uses {
			fmt.Fprintf(&b, "String(%s),", uses[(i+j)%len(uses)].src)
		}
		b.WriteString("].join(' '); }")
		m, err := syntax.ParseModule("late.js", b.String(), syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileModule(m)
		require.NoError(t, err)
		codes = append(codes, code)
	}
	const n = 16
	realms := make([]*Realm, n)
	for g := range realms {
		realms[g] = NewRealmWith(RealmOptions{SharedIntrinsics: true})
	}
	results := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for g, r := range realms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if g%3 == 0 {
				r.ensureLate(StringKey(AtomPromise)) // as an async function does
			}
			env, err := r.EvaluateModule(codes[g%len(codes)])
			if err != nil {
				errs[g] = err
				return
			}
			fn, _ := env.GetBindingValue("f")
			v, err := r.Call(fn, Undefined(), nil)
			if err != nil {
				errs[g] = err
				return
			}
			results[g] = v.AsString().GoString()
		}()
	}
	close(start)
	wg.Wait()
	all := len(NewRealm().Global.OwnPropertyKeys())
	for g, r := range realms {
		require.NoError(t, errs[g])
		got := strings.Split(results[g], " ")
		for j, res := range got {
			u := uses[(g%len(codes)+j)%len(uses)]
			if u.res == "?" {
				u.res = strconv.Itoa(all)
			}
			assert.Equal(t, u.res, res, "goroutine %d: %s", g, u.src)
		}
		for _, k := range coldGlobalKeys[firstLateGlobal:] {
			v, _ := r.Global.GetOwnDataValue(k)
			w, _ := realms[0].Global.GetOwnDataValue(k)
			assert.Equal(t, w, v, "one %s per process", k.GoString())
			assert.NotZero(t, v.AsObject().flags&flagShared)
		}
	}
}

// TestLazyGlobalsKeepCold checks that a host's lazy globals leave the
// builtin globals the realm has yet to define (coldGlobalKeys) pending,
// and that the two keep the semantics of each: a read defines the binding
// alone, in the order of first use, and the key list defines the pending
// cold bindings, in their order, then the pending lazy properties, in
// registration order.
func TestLazyGlobalsKeepCold(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		t.Run(fmt.Sprint(opts.SharedIntrinsics), func(t *testing.T) {
			r := NewRealmWith(opts)
			g := r.Global
			cold := r.coldGlobals
			count := g.shape.count
			keys := make([]PropertyKey, 30)
			for i := range keys {
				keys[i] = r.KeyFromGoString(fmt.Sprintf("lazy%d", i))
			}
			calls := 0
			g.DefineLazyProperties(r, keys, func(r *Realm, i int) Value {
				calls++
				return IntValue(i)
			})
			assert.Equal(t, cold, r.coldGlobals, "the cold bindings stay pending")
			assert.Equal(t, count, g.shape.count, "nothing is defined yet")
			assert.Zero(t, calls)

			lateProxy := lateIndex(StringKey(AtomProxy))
			assert.Equal(t, "10,function", runScriptsIn(t, r, "[lazy7 + lazy3, typeof Proxy].join()"))
			assert.Equal(t, 2, calls)
			assert.Equal(t, cold&^(1<<lateProxy), r.coldGlobals)
			assert.True(t, globalCached(r, "lazy7"))
			assert.False(t, globalCached(r, "lazy8"), "a pending lazy property fills no cache")
			assert.False(t, globalCached(r, "Intl"), "nor a pending cold binding")
			d, ok := g.GetOwnProperty(keys[9])
			assert.True(t, ok)
			assert.True(t, d.Writable() && d.Configurable() && !d.Enumerable())
			assert.Equal(t, 3, calls)

			// The key list defines the rest: the cold bindings, then the
			// lazy properties.
			pending := r.coldGlobals
			want := []string{"lazy7", "lazy3", "Proxy", "lazy9"}
			for i, k := range coldGlobalKeys {
				if pending&(1<<i) != 0 {
					want = append(want, k.GoString())
				}
			}
			for i := range 30 {
				if i != 3 && i != 7 && i != 9 {
					want = append(want, fmt.Sprintf("lazy%d", i))
				}
			}
			var names []string
			for _, k := range g.OwnPropertyKeys() {
				names = append(names, k.GoString())
			}
			require.GreaterOrEqual(t, len(names), len(want))
			assert.Equal(t, want, names[len(names)-len(want):])
			assert.Equal(t, 30, calls)
			assert.Zero(t, g.flags&flagHasLazy)
			assert.Nil(t, g.internal)
			assert.Zero(t, r.coldGlobals)
			assert.Equal(t, "function,29", runScriptsIn(t, r, "[typeof Intl.NumberFormat, lazy29].join()"))
		})
	}
}

// TestLazyGlobalsSemantics checks in, deletion, redefinition and
// assignment to a pending lazy global and to a pending cold binding while
// both kinds are pending.
func TestLazyGlobalsSemantics(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		r := NewRealmWith(opts)
		keys := []PropertyKey{r.KeyFromGoString("lzA"), r.KeyFromGoString("lzB"), r.KeyFromGoString("lzC"), r.KeyFromGoString("lzD")}
		calls := 0
		r.Global.DefineLazyProperties(r, keys, func(r *Realm, i int) Value {
			calls++
			return IntValue(i + 1)
		})
		// A shared realm's builtins are frozen, a mutable realm's writable.
		ro := "ok"
		if opts.SharedIntrinsics {
			ro = "TypeError"
		}
		for _, c := range []struct{ src, want string }{
			{"['lzA' in globalThis, 'Intl' in globalThis, 'lzE' in globalThis].join()", "true,true,false"},
			{"[delete globalThis.lzB, typeof lzB, 'lzB' in globalThis].join()", "true,undefined,false"},
			{"Object.defineProperty(globalThis, 'lzC', {value: 'c', enumerable: true}), [lzC, Object.keys(globalThis).includes('lzC')].join()", "c,true"},
			{"(() => { 'use strict'; lzD = 'd'; return lzD })()", "d"},
			{"(() => { 'use strict'; try { DataView = 1; return 'ok' } catch (e) { return e.name } })()", ro},
			{"[typeof Uint8Array, typeof Intl.Segmenter].join()", "function,function"},
		} {
			assert.Equal(t, c.want, runScriptsIn(t, r, c.src), c.src)
		}
		assert.Equal(t, 4, calls, "each init runs once")
		assert.Equal(t, "1", runScriptsIn(t, r, "lzA"))
		assert.Equal(t, 4, calls)
	}
}

// TestLazyPropertiesDuplicate registers keys more than once, in one
// DefineLazyProperties call and in a later DefineLazyProperty, with few
// keys and with enough for the index (lazyLinear): each key is registered
// once, in the place of its first registration, with the init of its last,
// each init runs at most once, and a value a script stores stays.
func TestLazyPropertiesDuplicate(t *testing.T) {
	for _, n := range []int{3, 100} {
		for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
			r := NewRealmWith(opts)
			keys := make([]PropertyKey, 0, n+2)
			for i := range n {
				keys = append(keys, r.KeyFromGoString(fmt.Sprintf("dup%d", i)))
			}
			keys = append(keys, keys[0], keys[1])
			calls := map[int]int{}
			r.Global.DefineLazyProperties(r, keys, func(r *Realm, i int) Value {
				calls[i]++
				return IntValue(i)
			})
			r.Global.DefineLazyProperty(r, keys[2], func(*Realm) Value {
				calls[-1]++
				return IntValue(-1)
			})
			assert.Equal(t, fmt.Sprintf("%d,%d,-1", n, n+1), runScriptsIn(t, r, "[dup0, dup1, dup2].join()"))
			assert.Equal(t, "x,y,true", runScriptsIn(t, r, "dup0 = 'x'; globalThis.dup1 = 'y'; [dup0, dup1, 'dup1' in globalThis].join()"))
			var names []string
			for _, k := range r.Global.OwnPropertyKeys() {
				if k.IsString() && strings.HasPrefix(k.GoString(), "dup") {
					names = append(names, k.GoString())
				}
			}
			var want []string
			for i := range n {
				want = append(want, fmt.Sprintf("dup%d", i))
			}
			assert.Equal(t, want, names, "once each, in registration order")
			assert.Equal(t, "x,y,-1", runScriptsIn(t, r, "[dup0, dup1, dup2].join()"))
			for i := range n + 2 {
				want := 1
				if i < 3 {
					want = 0
				}
				assert.Equal(t, want, calls[i], "n=%d init(%d)", n, i)
			}
			assert.Equal(t, 1, calls[-1])
			assert.Zero(t, r.Global.flags&flagHasLazy)
		}
	}
}

// TestLazyGlobalOverridesBuiltin defines lazy globals under the keys of
// builtin ones, a pending cold binding (TextEncoder) and, in a mutable
// realm, a defined one (Map): the lazy value replaces the builtin, which
// the realm still defines first, so its other bindings and the intrinsics
// the engine needs stay as they were.
func TestLazyGlobalOverridesBuiltin(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		r := NewRealmWith(opts)
		g := r.Global
		g.DefineLazyProperty(r, StringKey(AtomTextEncoder), func(*Realm) Value { return IntValue(1) })
		g.DefineLazyProperty(r, StringKey(AtomMapName), func(*Realm) Value { return IntValue(2) })
		assert.Equal(t, "1,function,2,1", runScriptsIn(t, r, `
[TextEncoder, typeof TextDecoder, Map, new Set([1]).size].join()`))
		r.ensureLate(StringKey(AtomTextEncoder))
		n := 0
		for _, k := range g.OwnPropertyKeys() {
			if k == StringKey(AtomTextEncoder) || k == StringKey(AtomMapName) {
				n++
			}
		}
		assert.Equal(t, 2, n, "each key once")
		v, _ := g.GetOwnDataValue(StringKey(AtomTextEncoder))
		assert.Equal(t, IntValue(1), v)
	}
}

package engine

import (
	"runtime"
	"testing"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWeakMapSemantics(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"basic", `const wm = new WeakMap(); const a = {}, b = {}; wm.set(a, 1).set(b, 2); return [wm.get(a), wm.get(b), wm.has(a), wm.delete(a), wm.has(a), wm.get(a), wm.delete(a), wm.get({})].join()`, "1,2,true,true,false,,false,"},
		{"overwrite", `const k = {}; const wm = new WeakMap([[k, 1], [k, 2]]); return wm.get(k)`, "2"},
		{"one key, many maps", `const k = {}; const m1 = new WeakMap([[k, 1]]), m2 = new WeakMap([[k, 2]]), m3 = new WeakMap([[k, 3]]); m1.delete(k); m3.set(k, 4); return [m1.get(k), m2.get(k), m3.get(k), m1.has(k)].join()`, ",2,4,false"},
		{"primitive keys", `const wm = new WeakMap(); return [wm.get(1), wm.has("x"), wm.delete(null), wm.has(undefined)].join()`, ",false,false,false"},
		{"set primitive", `return new WeakMap().set(1, 1)`, "!TypeError"},
		{"set registered symbol", `return new WeakMap().set(Symbol.for("x"), 1)`, "!TypeError"},
		{"missing strong keys", `const wm = new WeakMap(); return [wm.get(Symbol.hasInstance) === undefined, wm.get(Object.prototype) === undefined, wm.get(Math) === undefined].join()`, "true,true,true"},
		{"registered symbol lookups", `const wm = new WeakMap(); return [wm.has(Symbol.for("x")), wm.get(Symbol.for("x")), wm.delete(Symbol.for("x"))].join()`, "false,,false"},
		{"symbol keys", `const s = Symbol("x"); const wm = new WeakMap([[s, 1]]); return [wm.get(s), wm.has(Symbol("x")), wm.has(Symbol.iterator), wm.set(Symbol.iterator, 2).get(Symbol.iterator), wm.delete(Symbol.iterator), wm.has(Symbol.iterator), wm.delete(s), wm.has(s)].join()`, "1,false,false,2,true,false,true,false"},
		{"intrinsic keys", `const wm = new WeakMap(); wm.set(Object.prototype, 1).set(Math, 2); return [wm.get(Object.prototype), wm.get(Math), wm.delete(Math), wm.has(Math), new WeakMap().has(Object.prototype)].join()`, "1,2,true,false,false"},
		{"function keys", `const f = function () {}; const wm = new WeakMap([[f, "f"], [Array.prototype.push, "p"]]); return wm.get(f) + wm.get(Array.prototype.push)`, "fp"},
		{"dictionary and sparse keys", `const a = []; a[1000000] = 1; const o = {}; const wm = new WeakMap([[a, "a"], [o, "o"]]); a[5000000] = 2; delete a[1000000]; for (let i = 0; i < 100; i++) o["p" + i] = i; for (let i = 0; i < 100; i += 2) delete o["p" + i]; return [wm.get(a), a.length, wm.get(o), Object.keys(o).length, o.p99].join()`, "a,5000001,o,50,99"},
		{"key becomes sparse later", `const a = [1]; const wm = new WeakMap([[a, "a"]]); a[1000000] = 2; return [wm.get(a), a.length, Object.keys(a).join()].join()`, "a,1000001,0,1000000"},
		{"invisible on the key", `const o = {a: 1}; new WeakMap([[o, 1]]); new WeakSet([o]); return JSON.stringify(o) + Object.getOwnPropertyNames(o).length + Object.getOwnPropertySymbols(o).length + JSON.stringify(Object.assign({}, o))`, `{"a":1}10{"a":1}`},
		{"frozen key", `const o = Object.freeze({}); const wm = new WeakMap([[o, 1]]); return wm.get(o) + "," + Object.isFrozen(o)`, "1,true"},
		{"bad entry closes", `const log = []; const it = {[Symbol.iterator]() { return {next() { return {value: [1, 1], done: false}; }, return() { log.push("r"); return {}; }}; }}; try { new WeakMap(it); } catch (e) { log.push(e.name); } return log.join()`, "r,TypeError"},
		{"non-object entry", `return new WeakMap([1])`, "!TypeError"},
		{"null iterable", `return new WeakMap(null) instanceof WeakMap && new WeakMap(undefined) instanceof WeakMap`, "true"},
		{"shape", `return [WeakMap.length, WeakMap.prototype.set.length, WeakMap.prototype.get.length, "clear" in WeakMap.prototype, "size" in WeakMap.prototype, "forEach" in WeakMap.prototype, Symbol.iterator in WeakMap.prototype, WeakMap.prototype.constructor === WeakMap, Object.prototype.toString.call(new WeakMap())].join()`, "0,2,1,false,false,false,false,true,[object WeakMap]"},
		{"call throws", `return WeakMap()`, "!TypeError"},
		{"receiver", `return WeakMap.prototype.get.call(new Map(), {})`, "!TypeError"},
		{"receiver set", `return WeakMap.prototype.set.call(new WeakSet(), {}, 1)`, "!TypeError"},
	})
	runMutableProtoCases(t, []protoCase{
		{"custom set adder", `const orig = WeakMap.prototype.set; const log = []; WeakMap.prototype.set = function (k, v) { log.push(v); return orig.call(this, k, v + 1); }; const k = {}; const wm = new WeakMap([[k, 1], [{}, 2]]); WeakMap.prototype.set = orig; return log.join() + "|" + wm.get(k)`, "1,2|2"},
		{"custom adder skips key check", `const orig = WeakMap.prototype.set; WeakMap.prototype.set = function () { return this; }; try { return new WeakMap([[1, 1]]) instanceof WeakMap; } finally { WeakMap.prototype.set = orig; }`, "true"},
	})
}

func TestWeakSetSemantics(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"basic", `const a = {}, s = Symbol(); const ws = new WeakSet([a, a]); return [ws.has(a), ws.add(s) === ws, ws.has(s), ws.has({}), ws.delete(a), ws.has(a), ws.delete(a), ws.has(1), ws.delete(1)].join()`, "true,true,true,false,true,false,false,false,false"},
		{"add primitive", `return new WeakSet().add("x")`, "!TypeError"},
		{"add registered symbol", `return new WeakSet().add(Symbol.for("y"))`, "!TypeError"},
		{"bad value closes", `const log = []; const it = {[Symbol.iterator]() { return {next() { return {value: 1, done: false}; }, return() { log.push("r"); return {}; }}; }}; try { new WeakSet(it); } catch (e) { log.push(e.name); } return log.join()`, "r,TypeError"},
		{"shared with WeakMap key", `const k = {}; const ws = new WeakSet([k]); const wm = new WeakMap([[k, 1]]); ws.delete(k); return [ws.has(k), wm.get(k)].join()`, "false,1"},
		{"shape", `return [WeakSet.length, WeakSet.prototype.add.length, "size" in WeakSet.prototype, Object.prototype.toString.call(new WeakSet())].join()`, "0,1,false,[object WeakSet]"},
		{"call throws", `return WeakSet()`, "!TypeError"},
		{"receiver", `return WeakSet.prototype.has.call(new WeakMap(), {})`, "!TypeError"},
	})
	runMutableProtoCases(t, []protoCase{
		{"custom add", `const orig = WeakSet.prototype.add; const log = []; WeakSet.prototype.add = function (v) { log.push(typeof v); return orig.call(this, v); }; new WeakSet([{}, Symbol()]); WeakSet.prototype.add = orig; return log.join()`, "object,symbol"},
	})
}

func TestWeakRefSemantics(t *testing.T) {
	runProtoCases(t, []protoCase{
		{"deref", `const o = {}; const w = new WeakRef(o); return [w.deref() === o, w.deref() === o].join()`, "true,true"},
		{"symbol targets", `const s = Symbol("x"); return [new WeakRef(s).deref() === s, new WeakRef(Symbol.iterator).deref() === Symbol.iterator].join()`, "true,true"},
		{"intrinsic target", `return new WeakRef(Math).deref() === Math`, "true"},
		{"primitive target", `return new WeakRef(1)`, "!TypeError"},
		{"registered symbol target", `return new WeakRef(Symbol.for("x"))`, "!TypeError"},
		{"no target", `return new WeakRef()`, "!TypeError"},
		{"call throws", `return WeakRef({})`, "!TypeError"},
		{"receiver", `return WeakRef.prototype.deref.call({})`, "!TypeError"},
		{"shape", `return [WeakRef.length, WeakRef.prototype.deref.length, WeakRef.prototype.constructor === WeakRef, Object.prototype.toString.call(new WeakRef({})), Object.getPrototypeOf(new WeakRef({})) === WeakRef.prototype].join()`, "1,0,true,[object WeakRef],true"},
	})
}

// TestWeakNewTarget covers GetPrototypeFromConstructor(newTarget).
func TestWeakNewTarget(t *testing.T) {
	r := NewRealm()
	proto := r.NewObject()
	nt := r.NewNativeFunction(AtomEmpty, 0, nil)
	require.NoError(t, nt.SetProp(r, StringKey(AtomPrototype), ObjectValue(proto)))
	for _, tc := range []struct {
		ctor  NativeCtor
		class Class
	}{{weakMapConstruct, ClassWeakMap}, {weakSetConstruct, ClassWeakSet}, {weakRefConstruct, ClassWeakRef}} {
		v, err := tc.ctor(r, []Value{ObjectValue(r.NewObject())}[:boolInt(tc.class == ClassWeakRef)], nt)
		require.NoError(t, err)
		assert.Equal(t, proto, v.AsObject().Proto())
		assert.Equal(t, tc.class, v.AsObject().class)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// gcUntil runs the collector until cond holds (weak pointers are cleared by
// the sweep following the mark that found the object dead).
func gcUntil(cond func() bool) bool {
	for range 5 {
		runtime.GC()
		if cond() {
			return true
		}
	}
	return false
}

// TestWeakMapDoesNotRetain checks that neither the key nor a value that
// references its own key is kept alive by the collection.
func TestWeakMapDoesNotRetain(t *testing.T) {
	r := NewRealm()
	wmv, err := weakMapConstruct(r, nil, nil)
	require.NoError(t, err)
	wsv, err := weakSetConstruct(r, nil, nil)
	require.NoError(t, err)
	wm := dataOf(wmv.AsObject(), kindWeakColl)
	ws := dataOf(wsv.AsObject(), kindWeakColl)

	wk, wv, ws2 := func() (weak.Pointer[Object], weak.Pointer[Object], weak.Pointer[Symbol]) {
		key := r.NewObject()
		val := r.NewObject()
		require.NoError(t, val.SetProp(r, StringKey(AtomKeys), ObjectValue(key)))
		wm.set(ObjectValue(key), ObjectValue(val))
		ws.set(ObjectValue(key), Undefined())
		sym := NewSymbol(nil)
		symVal := r.NewObject()
		require.NoError(t, symVal.SetProp(r, StringKey(AtomKeys), SymbolValue(sym)))
		wm.set(SymbolValue(sym), ObjectValue(symVal))
		got, ok := wm.get(ObjectValue(key))
		require.True(t, ok)
		require.Equal(t, val, got.AsObject())
		return weak.Make(key), weak.Make(val), weak.Make(sym)
	}()
	assert.True(t, gcUntil(func() bool { return wk.Value() == nil && wv.Value() == nil && ws2.Value() == nil }),
		"key, self-referencing value and symbol key are collected while the collections live")
	runtime.KeepAlive(wmv)
	runtime.KeepAlive(wsv)
}

// TestWeakMapDeadCollection checks that the entries of a collected WeakMap
// are pruned from a live key on its next insertion.
func TestWeakMapDeadCollection(t *testing.T) {
	r := NewRealm()
	key := r.NewObject()
	wv := func() weak.Pointer[Object] {
		wmv, err := weakMapConstruct(r, nil, nil)
		require.NoError(t, err)
		val := r.NewObject()
		dataOf(wmv.AsObject(), kindWeakColl).set(ObjectValue(key), ObjectValue(val))
		return weak.Make(val)
	}()
	require.True(t, gcUntil(func() bool {
		return key.dictTable().weak.entries[0].owner.Value() == nil
	}), "the collection is collected")
	wm2v, err := weakMapConstruct(r, nil, nil)
	require.NoError(t, err)
	dataOf(wm2v.AsObject(), kindWeakColl).set(ObjectValue(key), IntValue(1))
	assert.Len(t, key.dictTable().weak.entries, 1)
	assert.True(t, gcUntil(func() bool { return wv.Value() == nil }), "the dead collection's value is released")
	runtime.KeepAlive(wm2v)
}

// TestWeakRefKeepDuringJob checks that a WeakRef target created or
// dereferenced in a job stays alive until the job ends, and is then
// collectable.
func TestWeakRefKeepDuringJob(t *testing.T) {
	r := NewRealm()
	refv, wt := func() (Value, weak.Pointer[Object]) {
		target := r.NewObject()
		v, err := weakRefConstruct(r, []Value{ObjectValue(target)}, nil)
		require.NoError(t, err)
		return v, weak.Make(target)
	}()
	runtime.GC()
	runtime.GC()
	require.NotNil(t, wt.Value(), "kept until the job ends")
	require.Len(t, r.lazy.jobs.kept, 1)
	// The outermost call returning ends the job.
	_, err := r.CallObject(r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, this Value, args []Value) (Value, error) {
		return Undefined(), nil
	}), Undefined(), nil)
	require.NoError(t, err)
	require.Empty(t, r.lazy.jobs.kept)
	assert.True(t, gcUntil(func() bool { return wt.Value() == nil }))
	v, err := weakRefProtoDeref(r, refv, nil)
	require.NoError(t, err)
	assert.True(t, v.IsUndefined(), "deref after collection")
	assert.Empty(t, r.lazy.jobs.kept)
}

// TestWeakRefKeptOncePerJob checks that repeated derefs in one job add the
// target to the kept list once.
func TestWeakRefKeptOncePerJob(t *testing.T) {
	f := evalModule(t, `
const o = {};
const w = new WeakRef(o);
export function spin() { let n = 0; for (let i = 0; i < 1000; i++) if (w.deref() === o) n++; return n; }
`)
	res, err := f.callErr("spin")
	require.NoError(t, err)
	assert.Equal(t, int64(1000), res)
	assert.Empty(t, f.r.lazy.jobs.kept, "cleared when the call returned")
	assert.Equal(t, uint64(2), f.r.lazy.jobs.keptGen, "module evaluation and the call each ended a job")
}

// TestWeakRefFromScript checks collection through the interpreter: targets
// created by the module and by a call — one also a WeakMap key whose value
// references it — are collectable once their jobs ended.
func TestWeakRefFromScript(t *testing.T) {
	f := evalModule(t, `
export const ref = new WeakRef({big: new Array(1000).fill(1)});
export const wm = new WeakMap();
export let kref;
export function add() { const k = {}; wm.set(k, {k}); kref = new WeakRef(k); }
`)
	_, err := f.callErr("add")
	require.NoError(t, err)
	// Stale registers above the stack pointer may still hold the targets.
	clear(f.r.interp.stack[f.r.interp.sp:])
	gone := func(name string) bool {
		v, ok := f.env.GetBindingValue(name)
		require.True(t, ok)
		res, err := weakRefProtoDeref(f.r, v, nil)
		require.NoError(t, err)
		return res.IsUndefined()
	}
	require.False(t, gone("kref"), "kept by the deref until the job ends")
	f.r.lazy.jobs.clearKept()
	assert.True(t, gcUntil(func() bool { return gone("ref") && gone("kref") }))
}

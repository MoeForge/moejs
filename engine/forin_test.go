package engine

import (
	"fmt"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forInKeys is a script function that lists the keys a for-in loop visits.
const forInKeys = `function keys(o) { const out = []; for (const k in o) out.push(k); return out.join() }
`

// TestForInSharedKeys walks a key list supplied by an array of constants:
// reading the list leaves it shared, and advancing the index keeps a number.
func TestForInSharedKeys(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	ks := make([]Value, sharedConstsMin)
	for i := range ks {
		key := r.KeyFromGoString(fmt.Sprint("k", i))
		ks[i] = StringValue(key.String())
		require.NoError(t, o.SetProp(r, key, IntValue(i)))
	}
	arr := r.newArrayConsts(ks, true)
	keys, idx, dst := ObjectValue(arr), IntValue(0), Undefined()
	for i := range ks {
		require.True(t, forInNext(&dst, &keys, &idx, ObjectValue(o)))
		assert.Equal(t, ks[i], dst)
		assert.True(t, idx.IsNumber())
		assert.Equal(t, float64(i+1), idx.AsNumber())
	}
	assert.False(t, forInNext(&dst, &keys, &idx, ObjectValue(o)))
	assert.NotZero(t, arr.bits&bitSharedElems)
	assert.Same(t, &ks[0], &arr.elemsReadOnly()[0])
}

// TestForInOwn runs for-in over objects of each kind and checks the keys
// against Node.js 24's, and which loops take forInOwn: the plain ones, whose
// prototypes have no enumerable string key. The others take forInInit's
// walk, which shadows and deduplicates.
func TestForInOwn(t *testing.T) {
	for _, c := range []struct {
		name, obj, want string
		fast            bool
	}{
		{"plain", `({b: 1, a: 2, 1: 'x', 0: 'y'})`, "0,1,b,a", true},
		{"symbols", `({a: 1, [Symbol('s')]: 2, b: 3})`, "a,b", true},
		{"hidden", `Object.defineProperty({a: 1, c: 2}, 'h', {value: 1, enumerable: false})`, "a,c", true},
		{"array", `(() => { const a = [3, , 5]; a.x = 1; return a })()`, "0,2,x", true},
		{"dictionary", `(() => { const o = {}; for (let i = 0; i < 70; i++) o['k' + i] = i; delete o.k5; delete o.k68; o[2] = 'two'; return o })()`,
			"2,k0,k1,k2,k3,k4,k6,k7", true},
		{"middle delete", `(() => { const o = {a: 1, b: 2, c: 3}; delete o.a; o.d = 4; return o })()`, "b,c,d", true},
		{"function", `(() => { function f() {} f.p = 1; return f })()`, "p", true},
		{"class instance", `(() => { class A { constructor() { this.x = 1 } m() {} static s() {} } class B extends A { n() {} } return new B() })()`, "x", true},
		{"null prototype", `(() => { const o = Object.create(null); o.z = 1; o.a = 2; return o })()`, "z,a", true},
		{"frozen", `Object.freeze({a: 1, 0: 'z'})`, "0,a", true},
		{"inherited", `(() => { const o = Object.create({inh: 1}); o.a = 2; return o })()`, "a,inh", false},
		{"shadow hidden", `(() => { const o = Object.create({a: 1, b: 2}); Object.defineProperty(o, 'a', {value: 0, enumerable: false}); return o })()`, "b", false},
		{"shadow own", `(() => { const o = Object.create({a: 1, b: 2}); o.a = 3; o.c = 4; return o })()`, "a,c,b", false},
		{"prototype elements", `(() => { const o = Object.create(['p', 'q']); o.x = 1; return o })()`, "x,0,1", false},
		{"hidden shadows deeper", `(() => { const deep = {a: 1, b: 2}; const mid = Object.create(deep); Object.defineProperty(mid, 'a', {value: 0, enumerable: false}); return Object.create(mid) })()`, "b", false},
		{"deep chain", `(() => { let o = {}; for (let i = 0; i < 12; i++) o = Object.create(o); o.a = 1; return o })()`, "a", false},
		{"proxy", `new Proxy({a: 1, b: 2}, {})`, "a,b", false},
		{"String wrapper", `(() => { const s = new String('ab'); s.x = 1; return s })()`, "0,1,x", false},
		{"typed array", `(() => { const t = new Uint8Array(2); t.x = 1; return t })()`, "0,1,x", false},
		{"arguments", `(function () { return arguments })(1, 2)`, "0,1", false},
		{"mapped arguments", `(function (a, b) { return arguments })(1, 2)`, "0,1", false},
		{"sparse", `(() => { const a = []; a[5000] = 1; a[0] = 2; a.x = 3; return a })()`, "0,5000,x", false},
		{"hidden element", `Object.defineProperty([1, 2], 0, {enumerable: false})`, "1", false},
		{"large indices", `({4294967295: 1, 4294967294: 2, 1: 3})`, "1,4294967294,4294967295", false},
		{"Number wrapper", `Object(5)`, "", false},
	} {
		for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
			t.Run(fmt.Sprint(c.name, "/", opts.SharedIntrinsics), func(t *testing.T) {
				r := NewRealmWith(opts)
				got := runScriptsIn(t, r, forInKeys+"var o = "+c.obj+"; keys(o)")
				if c.name == "dictionary" {
					require.Equal(t, "69", runScriptsIn(t, r, "keys(o).split(',').length"))
					got = runScriptsIn(t, r, "keys(o).split(',').slice(0, 8).join()")
					o, _ := r.Global.GetProp(r, r.KeyFromGoString("o"))
					require.NotZero(t, o.AsObject().flags&flagDict)
				}
				assert.Equal(t, c.want, got)
				o, err := r.Global.GetProp(r, r.KeyFromGoString("o"))
				require.NoError(t, err)
				assert.Equal(t, c.fast, r.forInOwn(o.AsObject()) != nil)
			})
		}
	}
	assert.Equal(t, "0,1///", runScripts(t, forInKeys+"keys('xy') + '/' + keys(5) + '/' + keys(null) + '/' + keys(undefined)"))
}

// TestForInPrototypeChanges changes Object.prototype and Array.prototype
// between loops, as a polyfill does: each loop checks the chain again, so
// an enumerable property added or made enumerable later is listed from the
// next loop on, and one deleted or hidden is not. Node.js 24 lists the same.
func TestForInPrototypeChanges(t *testing.T) {
	r := NewRealm()
	runScriptsIn(t, r, forInKeys+"var o = {a: 1}, arr = [1]")
	for _, c := range []struct{ src, want string }{
		{"keys(o)", "a"},
		{"Object.prototype.x = 1, keys(o)", "a,x"},
		{"delete Object.prototype.x, keys(o)", "a"},
		{"Object.defineProperty(Object.prototype, 'y', {value: 1, enumerable: true, configurable: true}), keys(o)", "a,y"},
		{"Object.defineProperty(Object.prototype, 'y', {enumerable: false}), keys(o)", "a"},
		{"Object.defineProperty(Object.prototype, 'toString', {enumerable: true}), keys(o)", "a,toString"},
		{"Object.defineProperty(Object.prototype, 'toString', {enumerable: false}), keys(o)", "a"},
		{"Object.prototype[0] = 'e', keys(o)", "a,0"},
		{"delete Object.prototype[0], keys(o)", "a"},
		{"Array.prototype.extra = function () {}, keys(arr)", "0,extra"},
		{"delete Array.prototype.extra, keys(arr)", "0"},
		{"Object.setPrototypeOf(o, {z: 1}), keys(o)", "a,z"},
		{"Object.setPrototypeOf(o, Object.prototype), keys(o)", "a"},
		{"Function.prototype.fp = 1, keys(function f() {})", "fp"},
		{"delete Function.prototype.fp, keys(function f() {})", ""},
	} {
		assert.Equal(t, c.want, runScriptsIn(t, r, c.src), c.src)
	}
}

// TestForInMutation changes the object a fast loop iterates: a deleted key
// is skipped, one added is not visited, one deleted and added again or
// still on the chain is, as in Node.js 24.
func TestForInMutation(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"const o = {a: 1, b: 2, c: 3}; const seen = []; for (const k in o) { seen.push(k); delete o.c; } seen.join()", "a,b"},
		{"const o = {a: 1, b: 2, c: 3}; const seen = []; for (const k in o) { seen.push(k); o.d = 4; } seen.join()", "a,b,c"},
		{"const o = {a: 1, b: 2, c: 3}; const seen = []; for (const k in o) { seen.push(k); delete o.b; o.b = 5; } seen.join()", "a,b,c"},
		{"const o = [1, 2, 3]; const seen = []; for (const k in o) { seen.push(k); o.length = 1; } seen.join()", "0"},
		{"const o = {a: 1, b: 2}; const seen = []; for (const k in o) { seen.push(k); if (k === 'a') Object.prototype.late = 1; } delete Object.prototype.late; seen.join()", "a,b"},
		{"const p = {}; const o = Object.create(p); o.a = 1; o.b = 2; const seen = []; for (const k in o) { seen.push(k); delete o.b; p.b = 'p'; } seen.join()", "a,b"},
		{"const o = {}; for (let i = 0; i < 70; i++) o['k' + i] = i; const seen = []; for (const k in o) { seen.push(k); delete o['k' + (+k.slice(1) + 1)]; } seen.length", "35"},
	} {
		assert.Equal(t, c.want, runScripts(t, c.src), c.src)
	}
}

// TestForInOwnFallback checks the objects whose keys forInOwn leaves to
// the walk although their class is plain: a host node over a Go map or
// slice, an object with lazy properties pending, and a module namespace.
func TestForInOwnFallback(t *testing.T) {
	r := NewRealm()
	runScriptsIn(t, r, forInKeys)
	for _, v := range []any{map[string]any{"b": 1, "a": 2, "0": 3}, []any{1, "x"}} {
		x := mustFromGo(t, r, v)
		require.NotZero(t, x.AsObject().flags&flagHostNode)
		require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("h"), x))
		assert.Nil(t, r.forInOwn(x.AsObject()))
		assert.Equal(t, runScriptsIn(t, r, "Object.keys(h).join()"), runScriptsIn(t, r, "keys(h)"))
		assert.Nil(t, r.forInOwn(x.AsObject()), "a materialized host node")
	}

	o := r.NewObject()
	require.NoError(t, o.SetProp(r, r.KeyFromGoString("a"), IntValue(1)))
	o.DefineLazyProperty(r, r.KeyFromGoString("lz"), func(*Realm) Value { return IntValue(2) })
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("lazy"), ObjectValue(o)))
	assert.Nil(t, r.forInOwn(o))
	assert.Equal(t, "a", runScriptsIn(t, r, "keys(lazy)"))
	assert.Equal(t, "a,lz,2", runScriptsIn(t, r, "Object.getOwnPropertyNames(lazy).join() + ',' + lazy.lz"))

	r, _, err := evalGraph(t, map[string]string{
		"main": forInKeys + `import * as ns from "m"; globalThis.ns = ns; globalThis.out = keys(ns);`,
		"m":    `export const b = 1; export let a = 2; export default 3;`,
	}, "main")
	require.NoError(t, err)
	ns, err := r.Global.GetProp(r, r.KeyFromGoString("ns"))
	require.NoError(t, err)
	assert.Nil(t, r.forInOwn(ns.AsObject()))
	out, err := r.Global.GetProp(r, r.KeyFromGoString("out"))
	require.NoError(t, err)
	assert.Equal(t, "a,b,default", out.String())
}

// warmForIn sets the realm past forInCacheAfter, after which it caches the
// key arrays of its loops (forInSlot).
func warmForIn(r *Realm) {
	r.shapeState().forInMisses = forInCacheAfter
}

// TestForInCache runs loops over objects of one shape, which share a frozen
// key array from the realm's cache, and then changes the objects and their
// prototypes: each change that changes the keys gives the object another
// shape, or is on the chain, which every loop checks, so each loop lists
// the keys Node.js 24 lists.
func TestForInCache(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		t.Run(fmt.Sprint(opts.SharedIntrinsics), func(t *testing.T) {
			r := NewRealmWith(opts)
			warmForIn(r)
			runScriptsIn(t, r, forInKeys+"var a = {x: 1, y: 2}, b = {x: 3, y: 4}, p = {}, c = Object.create(p); c.x = 1")
			a, b := globalObject(t, r, "a"), globalObject(t, r, "b")
			require.Same(t, a.shape, b.shape)
			ka := r.forInOwn(a)
			require.NotNil(t, ka)
			assert.Same(t, ka, r.forInOwn(b), "one array for the shape")
			assert.True(t, ka.IsFrozen())
			for _, c := range []struct {
				src, want string
				mutable   bool // a shared realm's intrinsics are frozen
			}{
				{"keys(a) + '/' + keys(b)", "x,y/x,y", false},
				{"b.z = 5, keys(a) + '/' + keys(b)", "x,y/x,y,z", false},
				{"delete b.x, keys(a) + '/' + keys(b)", "x,y/y,z", false},
				{"Object.defineProperty(a, 'x', {enumerable: false}), keys(a)", "y", false},
				{"Object.defineProperty(a, 'x', {enumerable: true}), keys(a)", "x,y", false},
				{"Object.freeze(a), keys(a)", "x,y", false},
				{"a[0] = 1, keys(a)", "x,y", false}, // frozen: no element
				{"var d = {x: 1, y: 2}; d[0] = 'e'; keys(d)", "0,x,y", false},
				{"delete d[0], keys(d)", "x,y", false},
				{"Object.prototype.late = 1, keys(d)", "x,y,late", true},
				{"delete Object.prototype.late, keys(d)", "x,y", true},
				{"keys(c)", "x", false},
				{"p.inh = 1, keys(c)", "x,inh", false},
				{"delete p.inh, keys(c)", "x", false},
				{"Object.setPrototypeOf(c, {z: 1}), keys(c)", "x,z", false},
				{"Object.setPrototypeOf(c, p), keys(c)", "x", false},
				{"var s = {}; s[Symbol()] = 1; s.k = 2; keys(s)", "k", false},
				{"var e = {}; keys(e) + '/' + keys({})", "/", false},
			} {
				if c.mutable && opts.SharedIntrinsics {
					continue
				}
				assert.Equal(t, c.want, runScriptsIn(t, r, c.src), c.src)
			}
			// Each object's keys, through the cache or not, are those of a
			// loop in a realm without it.
			cold := NewRealmWith(opts)
			src := forInKeys + `var out = [];
for (const o of [{a: 1, b: 2}, {a: 1, b: 2}, [1, 2], (() => { const x = [5]; x.p = 1; return x })(), function f() {}, Object.create(null)]) out.push(keys(o));
out.join('|')`
			assert.Equal(t, runScriptsIn(t, cold, src), runScriptsIn(t, r, src))
			assert.Nil(t, cold.lazy.shapes.forIn, "a realm that ran few loops has no cache")
		})
	}
}

// TestForInCacheSkips checks the objects whose loops the cache leaves out:
// those with elements, in dictionary mode, and an owned global object,
// each of whose loops makes its own array.
func TestForInCacheSkips(t *testing.T) {
	r := NewRealm()
	warmForIn(r)
	runScriptsIn(t, r, `for (const k in {a: 1}) ;
var arr = [1, 2], dict = {}; for (let i = 0; i < 70; i++) dict['k' + i] = i;
for (var i = 0; i < 100; i++) globalThis['g' + i] = i`)
	require.True(t, r.Global.shape.owned)
	for _, name := range []string{"arr", "dict"} {
		o := globalObject(t, r, name)
		k := r.forInOwn(o)
		require.NotNil(t, k, name)
		assert.NotSame(t, k, r.forInOwn(o), name)
		assert.False(t, k.IsFrozen(), name)
	}
	if k := r.forInOwn(r.Global); k != nil {
		assert.NotSame(t, k, r.forInOwn(r.Global))
	}
	require.NotNil(t, r.lazy.shapes.forIn)
	for i := range r.lazy.shapes.forIn {
		assert.False(t, r.lazy.shapes.forIn[i].shape != nil && r.lazy.shapes.forIn[i].shape.owned)
	}
}

// TestForInCacheShared runs loops over objects of one shape that overlap:
// a generator suspended inside a loop over one object, a loop over another
// that deletes keys as it goes, and the generator resumed. The two share
// the array of keys and each skips only the keys deleted from its own
// object.
func TestForInCacheShared(t *testing.T) {
	r := NewRealm()
	warmForIn(r)
	got := runScriptsIn(t, r, `function* g(o) { for (const k in o) yield k }
const a = {p: 1, q: 2, r: 3}, b = {p: 1, q: 2, r: 3};
const it = g(a), out = [it.next().value];
for (const k in b) { out.push(k); delete b.q; delete b.r }
out.push('|');
for (let s = it.next(); !s.done; s = it.next()) out.push(s.value);
for (const k in b) out.push(k);
out.join()`)
	assert.Equal(t, "p,p,|,q,r,p", got)
}

// TestForInCacheLazy: a realm creates the cache after forInCacheAfter
// loops the cache would take, and the live measurement counts it.
func TestForInCacheLazy(t *testing.T) {
	r := NewRealm()
	runScriptsIn(t, r, forInKeys+"var o = {a: 1}")
	o := globalObject(t, r, "o")
	before := r.MeasureLive(nil)
	for range forInCacheAfter {
		require.NotNil(t, r.forInOwn(o))
	}
	require.Nil(t, r.lazy.shapes.forIn)
	k := r.forInOwn(o)
	require.NotNil(t, r.lazy.shapes.forIn)
	assert.Same(t, k, r.forInOwn(o))
	assert.Equal(t, "a", runScriptsIn(t, r, "keys(o)"))
	assert.GreaterOrEqual(t, r.MeasureLive(nil)-before, int64(unsafe.Sizeof(forInCache{})))
}

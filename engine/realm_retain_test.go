package engine

import (
	"runtime"
	"strconv"
	"testing"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pooled runtime runs request after request for its lifetime, so what one
// request creates must be collectable once its objects are gone: these tests
// cover the per-realm structures that kept something of every request.

// fixtureFn returns the function the module exports as name.
func fixtureFn(f *moduleFixture, name string) Value {
	f.t.Helper()
	fn, ok := f.env.GetBindingValue(name)
	require.True(f.t, ok, "export %q", name)
	return fn
}

// freshPrototypeSource has one maker per way of making an object whose
// prototype is new: each makes the prototype, an object inheriting from it
// with a property, and returns the prototype. (The engine tests run without a
// compiler; the Function and GeneratorFunction constructors are covered at
// the root by TestFreshPrototypesRetained and TestDynamicCodeRetained.)
const freshPrototypeSource = `
const makers = [
	function () { function F() { this.a = 1; } new F(); return F.prototype; },
	function () { class C { constructor() { this.a = 1; } } new C(); return C.prototype; },
	function () { class C { a = 1; } class D extends C { b = 2; } new D(); return D.prototype; },
	function () { const p = {}; Object.create(p).a = 1; return p; },
	function () { const p = {}; Object.setPrototypeOf({}, p).a = 1; return p; },
	function () { function* g() { yield 1; } g().next(); return g.prototype; },
	function () { async function* g() { yield 1; } g().next(); return g.prototype; },
	function () { function F() {} const o = Reflect.construct(Object, [], F); o.a = 1; return F.prototype; },
];
export const count = makers.length;
export function make(i) { return makers[i](); }
`

// TestFreshPrototypeCollected: a prototype the program created, with the
// root shape of the objects inheriting from it and their transitions, is
// collected once the program drops it; the realm keeps no table entry for it
// (the auditor's TestAuditFxRootShapes: 670 bytes per `new F()` with a fresh
// F, kept for the realm's lifetime).
func TestFreshPrototypeCollected(t *testing.T) {
	for _, rc := range hostLazyRealms {
		t.Run(rc.name, func(t *testing.T) {
			f := evalModuleWith(t, freshPrototypeSource, rc.opts)
			r, mk := f.r, fixtureFn(f, "make")
			for i := range int(f.export("count").(int64)) {
				t.Run(strconv.Itoa(i), func(t *testing.T) {
					argv := []Value{NumberValue(float64(i))}
					wp, roots := freshPrototype(t, r, mk, argv)
					// More fresh prototypes through the same code, so the inline
					// caches that saw the first one see others.
					for range 8 {
						_, err := r.Call(mk, Undefined(), argv)
						require.NoError(t, err)
					}
					runtime.GC()
					runtime.GC()
					assert.Nil(t, wp.Value(), "the prototype is collected")
					assert.Equal(t, roots, len(r.rootShapes), "the realm keeps no root shape per prototype")
				})
			}
			runtime.KeepAlive(r)
		})
	}
}

// freshPrototype calls fn and returns a weak pointer to the prototype it
// returns, whose root shape lives in the prototype, and the size of the
// realm's root table after the call.
//
//go:noinline
func freshPrototype(t *testing.T, r *Realm, mk Value, argv []Value) (weak.Pointer[Object], int) {
	v, err := r.Call(mk, Undefined(), argv)
	require.NoError(t, err)
	p := v.AsObject()
	if assert.NotNil(t, p.ext, "the prototype holds its root shape") && assert.NotNil(t, p.extData().root) {
		assert.Same(t, p, p.extData().root.proto)
		assert.Same(t, p.extData().root, r.rootShapeFor(p))
	}
	return weak.Make(p), len(r.rootShapes)
}

// TestPrototypeRootKeepsShapeProps: the root shape a prototype holds does
// not make its own properties look like dictionary or sparse storage: the
// fast paths that read a shape's property list still apply to it.
func TestPrototypeRootKeepsShapeProps(t *testing.T) {
	f := evalModuleWith(t, `
export function proto() { function F() {} F.prototype.x = 1; F.prototype.y = 2; new F(); return F.prototype; }
export function keys(p) { return Object.keys(p).join() + "|" + JSON.stringify(p) + "|" + Object.keys(Object.assign({}, p)).join(); }
`, RealmOptions{SharedIntrinsics: true})
	p, err := f.r.Call(fixtureFn(f, "proto"), Undefined(), nil)
	require.NoError(t, err)
	require.NotNil(t, p.AsObject().extData().root)
	assert.True(t, hasOnlyShapeProps(p.AsObject()))
	res, err := f.r.Call(fixtureFn(f, "keys"), Undefined(), []Value{p})
	require.NoError(t, err)
	assert.Equal(t, `x,y|{"x":1,"y":2}|x,y`, f.r.ToGo(res))
}

// TestHostPrototypeRoot: a host node becomes a prototype before or after it
// is materialized; the root shape it then holds changes neither its lazy
// materialization nor its private elements nor what HostValue sees of it and
// of the node it hangs from.
func TestHostPrototypeRoot(t *testing.T) {
	for _, rc := range hostLazyRealms {
		t.Run(rc.name, func(t *testing.T) {
			f := evalModuleWith(t, `
class Base { constructor(o) { return o; } }
class Stamp extends Base { #id = 7; static id(o) { return o.#id; } }
export function inherit(p) { const o = Object.create(p); o.own = 1; return o.own + ":" + o.model + ":" + Object.keys(o).join(); }
export function protoOf(p) { Object.create(p); }
export function stamp(p) { new Stamp(p); return Stamp.id(p) + ":" + p.model; }
export function inheritEmpty(x) { Object.create(x.empty).own = 1; return Object.keys(x).join(); }
`, rc.opts)
			r := f.r
			call := func(name string, v Value) any {
				res, err := r.Call(fixtureFn(f, name), Undefined(), []Value{v})
				require.NoError(t, err)
				return r.ToGo(res)
			}
			fromGo := func(m map[string]any) Value {
				v, err := r.FromGo(m)
				require.NoError(t, err)
				return v
			}

			src := map[string]any{"model": "gpt", "n": 1.0}
			p := fromGo(src)
			assert.Equal(t, "1:gpt:own", call("inherit", p))
			require.NotNil(t, p.AsObject().extData().root)
			got, ok := p.AsObject().HostValue()
			if assert.True(t, ok, "a host node that is a prototype is unchanged") {
				assertSameGo(t, src, got)
			}

			// A placeholder that is a prototype stays a placeholder.
			q := fromGo(map[string]any{"model": "claude"})
			call("protoOf", q)
			require.Equal(t, hostSentinelKey, q.AsObject().shape.key, "still a placeholder")
			require.NotNil(t, q.AsObject().extData().root)
			assert.Equal(t, "7:claude", call("stamp", q))

			in := map[string]any{"empty": map[string]any{}, "a": 1.0}
			x := fromGo(in)
			assert.Equal(t, "a,empty", call("inheritEmpty", x))
			got, ok = x.AsObject().HostValue()
			if assert.True(t, ok, "an empty child that is a prototype is unchanged") {
				assertSameGo(t, in, got)
			}
		})
	}
}

// TestNameCachesBounded: requests whose host maps are keyed by ids, and
// whose code indexes objects with request values, intern new names and
// build new key sets on every call; the realm's caches of them stay within
// their bounds, and a name the module holds keeps its identity across the
// caches' resets (the auditor's TestAuditInternGrowth).
func TestNameCachesBounded(t *testing.T) {
	for _, rc := range hostLazyRealms {
		t.Run(rc.name, func(t *testing.T) {
			f := evalModuleWith(t, `
const kept = { persist_one: 1, "名前_persist": 2 };
export function hook(req) {
	const out = {};
	for (const id of Object.keys(req.byId)) out[id] = req.byId[id];
	out[req.user] = 1;
	out[req.wide] = 2;
	return Object.keys(out).length + kept[req.name] + kept[req.wideName];
}`, rc.opts)
			r, hook := f.r, fixtureFn(f, "hook")
			const requests = 1500
			maxKeys := 0
			for i := range requests {
				n := strconv.Itoa(i)
				byId := make(map[string]any, 8)
				for j := range 8 {
					byId["id_"+n+"_"+strconv.Itoa(j)] = j
				}
				for j := range 3 {
					byId["标识_"+n+"_"+strconv.Itoa(j)] = 8 + j
				}
				req := map[string]any{
					"byId": byId, "user": "user_" + n, "wide": "用户_" + n,
					"name": "persist_one", "wideName": "名前_persist",
				}
				x, err := r.FromGo(req)
				require.NoError(t, err)
				res, err := r.Call(hook, Undefined(), []Value{x})
				require.NoError(t, err)
				require.Equal(t, 13+3, int(res.AsNumber()), "request %d", i)
				r.ReleaseCallData()
				maxKeys = max(maxKeys, cachedHostShapeKeys(r))
			}
			// 1,500 requests intern 19,500 names, 6,000 of them UTF-16, and
			// build 1,500 key sets of 11 keys: the key bound of hostShapes
			// is reached before its entry bound.
			assert.LessOrEqual(t, len(r.internCacheASCII), internCacheMax)
			assert.LessOrEqual(t, len(r.internCacheUTF16), internCacheMax)
			assert.LessOrEqual(t, len(r.hostShapes), hostShapesMax)
			assert.LessOrEqual(t, maxKeys, hostShapeKeysMax)
			assert.Equal(t, cachedHostShapeKeys(r), int(r.fromGo.heap.shapeKeys))
			assert.NotEmpty(t, r.internCacheASCII)
			assert.NotEmpty(t, r.internCacheUTF16)
			assert.NotEmpty(t, r.hostShapes)
		})
	}
}

// cachedHostShapeKeys counts the keys of the entries in r.hostShapes.
func cachedHostShapeKeys(r *Realm) int {
	n := 0
	for _, e := range r.hostShapes {
		for ; e != nil; e = e.next {
			n += len(e.keys)
		}
	}
	return n
}

// TestDataKeyedShapesCollected: the shapes of objects keyed by request data
// (ids, user values) die with the objects: a realm's own tree holds only the
// first shapeStrongTransitions children of a shape, the others weakly, and
// sweeps the collected ones. A transition taken again after its shape was
// collected builds an equivalent shape that the inline caches read through.
func TestDataKeyedShapesCollected(t *testing.T) {
	for _, rc := range hostLazyRealms {
		t.Run(rc.name, func(t *testing.T) {
			f := evalModuleWith(t, `
export function make(id) { const o = {}; o[id] = 1; o[id + "_b"] = 2; return o; }
export function read(o, id) { return o[id] * 10 + o[id + "_b"]; }`, rc.opts)
			r, mk, read := f.r, fixtureFn(f, "make"), fixtureFn(f, "read")
			call := func(id string) Value {
				k := StringValue(FromGoString(id))
				o, err := r.Call(mk, Undefined(), []Value{k})
				require.NoError(t, err)
				v, err := r.Call(read, Undefined(), []Value{o, k})
				require.NoError(t, err)
				require.Equal(t, 12.0, v.AsNumber(), id)
				return o
			}
			// Past the shared tree's bounds for data keys: the objects continue
			// in the realm's own tree.
			for i := range 100 {
				call("warm_" + strconv.Itoa(i))
			}
			wp, root := dataKeyedShape(t, call)
			strong := len(root.transMap)
			require.GreaterOrEqual(t, strong, shapeStrongTransitions)
			const requests = 5000
			for i := range requests {
				call("id_" + strconv.Itoa(i))
				if i%500 == 0 {
					runtime.GC()
				}
			}
			runtime.GC()
			runtime.GC()
			assert.Nil(t, wp.Value(), "the shape of a dropped data-keyed object is collected")
			assert.Equal(t, strong, len(root.transMap), "the children past the strong ones are weak")
			if tr := root.sharedTrans.Load(); assert.NotNil(t, tr) {
				assert.Less(t, len(tr.weak), requests/4, "the collected children are swept")
			}
			// The transitions taken again after their shapes were collected.
			for i := range 100 {
				call("warm_" + strconv.Itoa(i))
				call("id_" + strconv.Itoa(i))
			}
		})
	}
}

// TestDeadWeakTransitionSweeps: a transition that finds its weak child
// collected sweeps the weak tier, so a burst of children that died after the
// tier's last sweep does not keep its entries, and their names, until the
// tier grows to the threshold that sweep set (the auditor's
// TestAuditWeakSweepKeepsLive). The live children stay in the tier.
func TestDeadWeakTransitionSweeps(t *testing.T) {
	for _, rc := range hostLazyRealms {
		t.Run(rc.name, func(t *testing.T) {
			f := evalModuleWith(t, `
export function make(id) { const o = {}; o[id] = 1; o[id + "_b"] = 2; return o; }
export function read(o, id) { return o[id] * 10 + o[id + "_b"]; }`, rc.opts)
			r, mk, read := f.r, fixtureFn(f, "make"), fixtureFn(f, "read")
			check := func(o Value, id string) {
				v, err := r.Call(read, Undefined(), []Value{o, StringValue(FromGoString(id))})
				require.NoError(t, err)
				require.Equal(t, 12.0, v.AsNumber(), id)
			}
			call := func(id string) Value {
				o, err := r.Call(mk, Undefined(), []Value{StringValue(FromGoString(id))})
				require.NoError(t, err)
				check(o, id)
				return o
			}
			for i := range 100 {
				call("warm_" + strconv.Itoa(i))
			}
			_, root := dataKeyedShape(t, call)
			const live, dead = 300, 6000
			objs := make([]Value, live)
			for i := range live {
				objs[i] = call("live_" + strconv.Itoa(i))
			}
			// The burst lives through the tier's sweeps, which count it as
			// live, and dies after the last of them.
			burst := make([]Value, dead)
			for i := range dead {
				burst[i] = call("dead_" + strconv.Itoa(i))
			}
			clear(burst)
			r.ReleaseCallData()
			runtime.GC()
			runtime.GC()
			tr := root.sharedTrans.Load()
			require.NotNil(t, tr)
			before := len(tr.weak)
			require.Greater(t, before, live+shapeWeakSweep, "dead children outlive the tier's last sweep")
			call("dead_" + strconv.Itoa(dead-1))
			t.Logf("weak tier: %d entries, then %d after a dead transition (sweep at %d)", before, len(tr.weak), tr.sweep)
			assert.Less(t, len(tr.weak), live+16, "the dead transition swept the tier")
			assert.LessOrEqual(t, int(tr.sweep), 2*(live+16)+shapeWeakSweep)
			for i := range live {
				id := "live_" + strconv.Itoa(i)
				require.Same(t, objs[i].AsObject().shape.parent, root.weakTransition(StringKey(r.InternGoString(id)), attrDefault), id)
				check(objs[i], id)
			}
		})
	}
}

// dataKeyedShape makes an object keyed by a fresh id and returns a weak
// pointer to its shape and the shape of the realm's own tree its first key
// leaves from.
//
//go:noinline
func dataKeyedShape(t *testing.T, call func(string) Value) (weak.Pointer[Shape], *Shape) {
	o := call("fresh_id")
	s := o.AsObject().shape
	require.False(t, s.shared)
	require.Equal(t, 2, s.Count())
	return weak.Make(s), s.parent.parent
}

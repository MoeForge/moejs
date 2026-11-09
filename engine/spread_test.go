package engine

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spreadShow is a script function that renders an object's own keys in
// [[OwnPropertyKeys]] order, each with its value and attributes (w, e, c;
// a for an accessor), and marks a prototype other than Object.prototype.
const spreadShow = `function show(o) {
  return Reflect.ownKeys(o).map(k => {
    const d = Object.getOwnPropertyDescriptor(o, k);
    const n = typeof k === 'symbol' ? k.toString() : k;
    const a = (d.writable ? 'w' : '') + (d.enumerable ? 'e' : '') + (d.configurable ? 'c' : '') + ('get' in d ? 'a' : '');
    return n + '=' + String(d.value) + ':' + a;
  }).join(' ') + (Object.getPrototypeOf(o) === Object.prototype ? '' : ' !proto');
}
`

// spreadKeys renders want for n keys prefix0..prefix(n-1) with their index
// as value, as show does.
func spreadKeys(prefix string, n int, skip int) string {
	var b strings.Builder
	for i := range n {
		if i == skip {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s%d=%d:wec", prefix, i, i)
	}
	return b.String()
}

// The paths copyDataProps takes for a fresh target (spreadFresh).
const (
	spreadGeneral = iota // the general path: spreadFresh refuses the source
	spreadAdd            // spreadFresh adds the keys from target's root
	spreadAdopt          // spreadFresh gives target the source's shape
)

// globalObject returns the value of the global variable name.
func globalObject(t *testing.T, r *Realm, name string) *Object {
	t.Helper()
	v, err := r.Global.GetProp(r, r.KeyFromGoString(name))
	require.NoError(t, err)
	require.True(t, v.IsObject(), name)
	return v.AsObject()
}

// TestSpreadFresh spreads sources of each kind into a fresh object and
// checks the result against Node.js 24's (keys in [[OwnPropertyKeys]]
// order, values, attributes), and which path the spread takes: an
// ordinary source of Object.prototype whose properties are all plain data
// lends its shape; one with hidden, read-only or symbol keys mixed in, or
// another prototype, has its keys added; any other takes the general path.
func TestSpreadFresh(t *testing.T) {
	for _, c := range []struct {
		name, obj, want string
		path            int
	}{
		{"plain", `({b: 1, a: 2})`, "b=1:wec a=2:wec", spreadAdopt},
		{"order", `({z: 1, 2: 'two', [Symbol('s1')]: 's', a: 3, 0: 'zero', [Symbol('s2')]: 't', b: 4})`,
			"0=zero:wec 2=two:wec z=1:wec a=3:wec b=4:wec Symbol(s1)=s:wec Symbol(s2)=t:wec", spreadAdopt},
		{"symbols", `(() => { const o = {a: 1}; o[Symbol('e')] = 2; Object.defineProperty(o, Symbol('h'), {value: 3, enumerable: false}); o.b = 4; return o })()`,
			"a=1:wec b=4:wec Symbol(e)=2:wec", spreadAdd},
		{"hidden", `Object.defineProperty({a: 1, c: 2}, 'h', {value: 1, enumerable: false})`, "a=1:wec c=2:wec", spreadAdd},
		{"readonly", `Object.defineProperty({a: 1, c: 2}, 'r', {value: 9, enumerable: true, writable: false})`, "a=1:wec c=2:wec r=9:wec", spreadAdd},
		{"frozen", `Object.freeze({a: 1, 0: 'z'})`, "0=z:wec a=1:wec", spreadAdd},
		{"elements", `(() => { const o = {x: 1}; o[3] = 'c'; o[1] = 'a'; return o })()`, "1=a:wec 3=c:wec x=1:wec", spreadAdopt},
		{"accessor", `({a: 1, get g() { return 'got' }, b: 2})`, "a=1:wec g=got:wec b=2:wec", spreadGeneral},
		{"proto accessor", `Object.create(Object.defineProperty({}, 'p', {get() { return 'pg' }, enumerable: true}), {own: {value: 1, enumerable: true}})`,
			"own=1:wec", spreadAdd},
		{"proto data", `(() => { const o = Object.create({inh: 1}); o.a = 2; return o })()`, "a=2:wec", spreadAdd},
		{"dictionary", `(() => { const o = {}; for (let i = 0; i < 70; i++) o['k' + i] = i; delete o.k5; o[2] = 'two'; o[Symbol('d')] = 'ds'; return o })()`,
			"2=two:wec " + spreadKeys("k", 70, 5) + " Symbol(d)=ds:wec", spreadGeneral},
		{"middle delete", `(() => { const o = {a: 1, b: 2, c: 3}; delete o.a; o.d = 4; return o })()`, "b=2:wec c=3:wec d=4:wec", spreadGeneral},
		{"class instance", `(() => { class A { #p = 1; x = 1; constructor() { this.y = 2 } m() {} get g() { return 1 } } return new A() })()`,
			"x=1:wec y=2:wec", spreadAdd},
		{"null prototype", `(() => { const o = Object.create(null); o.z = 1; o.a = 2; return o })()`, "z=1:wec a=2:wec", spreadAdd},
		{"array", `(() => { const a = [3, , 5]; a.x = 1; return a })()`, "0=3:wec 2=5:wec x=1:wec", spreadGeneral},
		{"proxy", `new Proxy({a: 1, b: 2}, {})`, "a=1:wec b=2:wec", spreadGeneral},
		{"String wrapper", `(() => { const s = new String('ab'); s.x = 1; return s })()`, "0=a:wec 1=b:wec x=1:wec", spreadGeneral},
		{"typed array", `(() => { const t = new Uint8Array(2); t.x = 1; return t })()`, "0=0:wec 1=0:wec x=1:wec", spreadGeneral},
		{"arguments", `(function () { return arguments })(1, 2)`, "0=1:wec 1=2:wec", spreadGeneral},
		{"mapped arguments", `(function (a, b) { return arguments })(1, 2)`, "0=1:wec 1=2:wec", spreadGeneral},
		{"sparse", `(() => { const o = {a: 1}; o[5000] = 's'; o[0] = 'z'; return o })()`, "0=z:wec 5000=s:wec a=1:wec", spreadGeneral},
		{"function", `(() => { function f() {} f.p = 1; return f })()`, "p=1:wec", spreadGeneral},
		{"error", `(() => { const e = new Error('m'); e.x = 1; return e })()`, "x=1:wec", spreadGeneral},
		{"Number wrapper", `(() => { const n = Object(5); n.x = 1; return n })()`, "x=1:wec", spreadGeneral},
		{"empty", `({})`, "", spreadAdopt},
		{"many", `(() => { const o = {}; for (let i = 0; i < 64; i++) o['p' + i] = i; return o })()`, spreadKeys("p", 64, -1), spreadAdopt},
		{"__proto__ key", `JSON.parse('{"__proto__": 1, "a": 2}')`, "__proto__=1:wec a=2:wec", spreadAdopt},
	} {
		for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
			t.Run(fmt.Sprint(c.name, "/", opts.SharedIntrinsics), func(t *testing.T) {
				r := NewRealmWith(opts)
				assert.Equal(t, c.want, runScriptsIn(t, r, spreadShow+"var o = "+c.obj+"; var t = {...o}; show(t)"))
				o, tgt := globalObject(t, r, "o"), globalObject(t, r, "t")
				assert.Equal(t, c.path != spreadGeneral, r.spreadFresh(r.NewObjectCap(0), o, nil))
				if c.path != spreadGeneral && o.shape.count != 0 {
					assert.Equal(t, c.path == spreadAdopt, tgt.shape == o.shape)
				}
				if c.path != spreadGeneral {
					// The source is unchanged, and the target is its own.
					assert.Equal(t, c.want, runScriptsIn(t, r, "show({...o})"))
					assert.Zero(t, tgt.flags&flagDict)
				}
			})
		}
	}
	for _, src := range []string{"'hi'", "null", "undefined", "5"} {
		assert.Equal(t, map[string]string{"'hi'": "0=h:wec 1=i:wec"}[src], runScripts(t, spreadShow+"show({..."+src+"})"), src)
	}
}

// TestSpreadRest takes the rest of an object pattern, which excludes the
// keys the pattern names (CopyDataPropsEx), and checks it against Node.js
// 24's: an excluded key that the source has keeps its shape from the rest,
// an excluded index leaves a hole in its elements.
func TestSpreadRest(t *testing.T) {
	for _, c := range []struct {
		pattern, obj, want string
		adopt              bool
	}{
		{"{a, ...t}", `{a: 1, b: 2, c: 3}`, "b=2:wec c=3:wec", false},
		{"{0: x, ...t}", `{0: 'a', 1: 'b', y: 1}`, "1=b:wec y=1:wec", true},
		{"{1: x, ...t}", `{0: 'a', 1: 'b', y: 1}`, "0=a:wec y=1:wec", true},
		{"{[s]: x, ...t}", `{a: 1, [s]: 2, [Symbol('t')]: 3}`, "a=1:wec Symbol(t)=3:wec", false},
		{"{q, ...t}", `{a: 1, b: 2}`, "a=1:wec b=2:wec", true},
	} {
		for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
			t.Run(fmt.Sprint(c.pattern, "/", opts.SharedIntrinsics), func(t *testing.T) {
				r := NewRealmWith(opts)
				src := spreadShow + "var s = Symbol('k'); var o = " + c.obj + "; var " + c.pattern + " = o; show(t)"
				assert.Equal(t, c.want, runScriptsIn(t, r, src))
				assert.Equal(t, c.adopt, globalObject(t, r, "t").shape == globalObject(t, r, "o").shape)
			})
		}
	}
	// A getter on the source runs once, through the general path.
	assert.Equal(t, "b=1:wec", runScripts(t, spreadShow+"let n = 0; const {a, ...r} = {a: 1, get b() { return ++n }}; show(r)"))
	// A pattern naming more keys than the excluded-key buffer holds.
	for _, n := range []int{31, 32, 33, 40} {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprint("k", i)
		}
		src := spreadShow + "var o = {}; for (let i = 0; i < 42; i++) o['k' + i] = i; var {" + strings.Join(keys, ", ") + ", ...t} = o; show(t)"
		var want []string
		for i := n; i < 42; i++ {
			want = append(want, fmt.Sprintf("k%d=%d:wec", i, i))
		}
		assert.Equal(t, strings.Join(want, " "), runScripts(t, src), n)
	}
}

// TestSpreadTargets spreads into targets that are not fresh, which take
// the general path, and mutates sources and targets after a spread that
// lent its shape: neither sees the other's changes, and inline caches
// filled on either object serve the other. Node.js 24 gives the same.
func TestSpreadTargets(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		t.Run(fmt.Sprint(opts.SharedIntrinsics), func(t *testing.T) {
			r := NewRealmWith(opts)
			for _, c := range []struct{ src, want string }{
				{"(() => { const s = {b: 2, a: 3}; return show({a: 1, ...s, c: 4}) })()", "a=3:wec b=2:wec c=4:wec"},
				{"show({...{a: 1, b: 2}, ...{b: 3, c: 4}})", "a=1:wec b=3:wec c=4:wec"},
				{"show({...null, ...{}, ...{a: 1}})", "a=1:wec"},
				{"show({...false, ...1, ...Symbol(), ...10n, ...'ab', ...''})", "0=a:wec 1=b:wec"},
				{"(() => { Number.prototype.x = 1; const {...q} = 7; const t = {...5, ...true}; delete Number.prototype.x; return show(q) + '|' + show(t) })()", "|"},
				{"(() => { const s = {a: 1}; return show({...s, b: 2}) })()", "a=1:wec b=2:wec"},
				{"(() => { const s = {a: 1, b: 2}; const t = {...s}; s.a = 9; s.c = 3; delete s.b; return show(t) + ' | ' + show(s) })()",
					"a=1:wec b=2:wec | a=9:wec c=3:wec"},
				{"(() => { const s = {a: 1, b: 2}; const t = {...s}; t.a = 9; t.c = 3; delete t.b; Object.freeze(t); return show(t) + ' | ' + show(s) })()",
					"a=9:e c=3:e | a=1:wec b=2:wec"},
				{"(() => { const s = {0: 'x', a: 1}; const t = {...s}; s[0] = 'y'; t[1] = 'z'; return show(t) + ' | ' + show(s) })()",
					"0=x:wec 1=z:wec a=1:wec | 0=y:wec a=1:wec"},
				{"(() => { function ga(o) { return o.a } const s = {a: 1, b: 2}; let r = ''; for (let i = 0; i < 3; i++) { const t = {...s}; r += ga(s) + ga(t); t.a = i; r += ga(t) + ga(s) } return r })()",
					"212223"},
				{"(() => { function sa(o, v) { o.a = v } const s = {a: 1, b: 2}; const t = {...s}; sa(s, 5); sa(t, 7); sa(s, 6); return [s.a, t.a].join() })()", "6,7"},
				{"(() => { function f(s) { return {...s, z: 1} } return [f({a: 1}), f({b: 2}), f({a: 1, b: 2}), f(Object.create(null)), f({a: 3})].map(show).join('|') })()",
					"a=1:wec z=1:wec|b=2:wec z=1:wec|a=1:wec b=2:wec z=1:wec|z=1:wec|a=3:wec z=1:wec"},
				{"(() => { const o = {['x'.repeat(70)]: 1, b: 2}; const t = {...o}; return show(t).length + ',' + t.b })()", "84,2"},
				// Into a literal with keys of its own, which createDataProp
				// adds to or overwrites, or leaves to the general path.
				{"show({a: 1, b: 2, ...{b: 3, c: 4}})", "a=1:wec b=3:wec c=4:wec"},
				{"show({get b() { return 0 }, set c(v) {}, ...{b: 3, c: 4}})", "b=3:wec c=4:wec"},
				{"show({z: 0, ...JSON.parse('{\"__proto__\": 1, \"a\": 2}')})", "z=0:wec __proto__=1:wec a=2:wec"},
				{"(() => { const k = Symbol('k'); return show({[k]: 1, x: 0, ...{[k]: 2, x: 3, y: 4}}) })()", "x=3:wec y=4:wec Symbol(k)=2:wec"},
				{"(() => { const s = {a: 1, get b() { s.c = 9; return 2 }, c: 3}; return show({z: 0, ...s}) })()", "z=0:wec a=1:wec b=2:wec c=9:wec"},
			} {
				assert.Equal(t, c.want, runScriptsIn(t, r, spreadShow+c.src), c.src)
			}
			// The global object: its owned chain is never lent.
			assert.Equal(t, spreadKeys("k", 70, -1)+"/69", runScriptsIn(t, r, spreadShow+
				"for (var i = 0; i < 70; i++) globalThis['k' + i] = i; var g = {...globalThis};"+
				"show(g).split(' ').filter(k => /^k\\d+=/.test(k)).join(' ') + '/' + g.k69"))
			require.True(t, r.Global.shape.owned)
			assert.False(t, r.spreadFresh(r.NewObjectCap(0), r.Global, nil))
		})
	}
}

// TestSpreadSharedTree checks which shapes a spread lends in a realm over
// the shared intrinsics: a shape of the shared tree, and one of the realm's
// own continuation of it (a key longer than the shared tree takes), but
// not one of another realm's continuation, nor one of another mutable
// realm's tree, whose keys the target gets added from its own root.
func TestSpreadSharedTree(t *testing.T) {
	long := strings.Repeat("x", maxSharedDataKey+6)
	src := "var o = {['" + long + "']: 1, b: 2}; var t = {...o}; [Object.keys(t).length, t.b].join()"
	a, b := NewRealmWith(RealmOptions{SharedIntrinsics: true}), NewRealmWith(RealmOptions{SharedIntrinsics: true})
	assert.Equal(t, "2,2", runScriptsIn(t, a, src))
	o := globalObject(t, a, "o")
	require.False(t, o.shape.shared)
	assert.Same(t, o.shape, globalObject(t, a, "t").shape)

	fresh := b.NewObjectCap(0)
	require.True(t, b.spreadFresh(fresh, o, nil))
	assert.NotSame(t, o.shape, fresh.shape)
	assert.Same(t, b.rootShapes[b.ObjectPrototype], fresh.shape.root())
	assert.Equal(t, o.slots, fresh.slots)

	// A descriptor object has a shape of the shared tree.
	runScriptsIn(t, a, "var p = Object.getOwnPropertyDescriptor({x: 1}, 'x')")
	p := globalObject(t, a, "p")
	require.True(t, p.shape.shared)
	fresh = b.NewObjectCap(0)
	require.True(t, b.spreadFresh(fresh, p, nil))
	assert.Same(t, p.shape, fresh.shape)

	m1, m2 := NewRealm(), NewRealm()
	runScriptsIn(t, m1, "var q = {c: 1, d: 2}")
	q := globalObject(t, m1, "q")
	fresh = m2.NewObjectCap(0)
	require.True(t, m2.spreadFresh(fresh, q, nil))
	assert.NotSame(t, q.shape, fresh.shape)
	assert.Same(t, m2.plainRoot, fresh.shape.root())
}

// TestSpreadFallback checks the plain objects whose properties spreadFresh
// leaves to the general path: a host node over a Go map or slice, an
// object with lazy properties pending, one past maxShapeProps in an owned
// chain (KeepShape), one whose shape bars inline caches (noFill), and a
// module namespace.
func TestSpreadFallback(t *testing.T) {
	r := NewRealm()
	runScriptsIn(t, r, spreadShow)
	for _, v := range []any{map[string]any{"b": 1, "a": 2, "0": 3}, []any{1, "x"}} {
		x := mustFromGo(t, r, v)
		require.NotZero(t, x.AsObject().flags&flagHostNode)
		require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("h"), x))
		assert.False(t, r.spreadFresh(r.NewObjectCap(0), x.AsObject(), nil))
		assert.Equal(t, runScriptsIn(t, r, "Object.keys(h).map(k => k + '=' + h[k] + ':wec').join(' ')"), runScriptsIn(t, r, "show({...h})"))
		assert.False(t, r.spreadFresh(r.NewObjectCap(0), x.AsObject(), nil), "a materialized host node")
	}

	o := r.NewObject()
	require.NoError(t, o.SetProp(r, r.KeyFromGoString("a"), IntValue(1)))
	o.DefineLazyProperty(r, r.KeyFromGoString("lz"), func(*Realm) Value { return IntValue(2) })
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("lazy"), ObjectValue(o)))
	assert.False(t, r.spreadFresh(r.NewObjectCap(0), o, nil))
	assert.Equal(t, "a=1:wec", runScriptsIn(t, r, "show({...lazy})"))

	k := r.NewObject()
	k.KeepShape()
	for i := range 70 {
		require.NoError(t, k.SetProp(r, r.KeyFromGoString(fmt.Sprint("k", i)), IntValue(i)))
	}
	require.True(t, k.shape.owned)
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("kept"), ObjectValue(k)))
	assert.False(t, r.spreadFresh(r.NewObjectCap(0), k, nil))
	assert.Equal(t, spreadKeys("k", 70, -1), runScriptsIn(t, r, "show({...kept})"))

	runScriptsIn(t, r, "var nf = {nf1: 1}")
	nf := globalObject(t, r, "nf")
	nf.shape.noFill = noFillAll // as a deferred install marks its object's shape
	assert.False(t, r.spreadFresh(r.NewObjectCap(0), nf, nil))
	assert.Equal(t, "nf1=1:wec", runScriptsIn(t, r, "show({...nf})"))
	nf.shape.noFill = 0

	r, _, err := evalGraph(t, map[string]string{
		"main": spreadShow + `import * as ns from "m"; globalThis.ns = ns; globalThis.out = show({...ns});`,
		"m":    `export const b = 1; export let a = 2; export default 3;`,
	}, "main")
	require.NoError(t, err)
	assert.False(t, r.spreadFresh(r.NewObjectCap(0), globalObject(t, r, "ns"), nil))
	out, err := r.Global.GetProp(r, r.KeyFromGoString("out"))
	require.NoError(t, err)
	assert.Equal(t, "a=2:wec b=1:wec default=3:wec", out.String())
}

// TestSpreadMemory checks that a spread charges the slots and elements it
// allocates to the realm's memory account: when it lends its shape, exactly
// the target's new slot and element storage; when it adds the keys, that
// storage at least (and the shapes it creates).
func TestSpreadMemory(t *testing.T) {
	for _, opts := range []RealmOptions{{MemoryLimit: 1 << 40}, {MemoryLimit: 1 << 40, SharedIntrinsics: true}} {
		t.Run(fmt.Sprint(opts.SharedIntrinsics), func(t *testing.T) {
			r := NewRealmWith(opts)
			runScriptsIn(t, r, `var o = {}; for (let i = 0; i < 20; i++) o['m' + i] = i; for (let i = 0; i < 10; i++) o[i] = i;
var n = Object.create(null); for (let i = 0; i < 20; i++) n['m' + i] = i;`)
			for _, c := range []struct {
				name  string
				adopt bool
			}{{"o", true}, {"n", false}} {
				from := globalObject(t, r, c.name)
				fresh := r.NewObjectCap(0)
				before := r.Stats().RequestAllocatedBytes
				require.True(t, r.spreadFresh(fresh, from, nil))
				charged := r.Stats().RequestAllocatedBytes - before
				storage := int64((cap(fresh.slots) + cap(fresh.elems())) * valueSize)
				assert.Equal(t, c.adopt, fresh.shape == from.shape)
				assert.Len(t, fresh.slots, 20)
				if c.adopt {
					assert.Len(t, fresh.elems(), 10)
					assert.Equal(t, storage, charged)
				} else {
					assert.GreaterOrEqual(t, charged, storage)
				}
			}
			// Through the interpreter, with the target object itself.
			before := r.Stats().RequestAllocatedBytes
			runScriptsIn(t, r, "var t = {...o}")
			tgt := globalObject(t, r, "t")
			assert.GreaterOrEqual(t, r.Stats().RequestAllocatedBytes-before,
				int64(unsafe.Sizeof(Object{}))+int64((cap(tgt.slots)+cap(tgt.elems()))*valueSize))
		})
	}
}

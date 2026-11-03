package engine

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keepShapePair makes two objects of n properties p0... that a host
// built: keep is marked with KeepShape, dict is not, and is in dictionary
// mode past maxShapeProps. They are the globals keep and dict.
func keepShapePair(t *testing.T, r *Realm, n int) (keep, dict *Object) {
	keep, dict = r.NewObject(), r.NewObject()
	keep.KeepShape()
	for i := range n {
		k := r.KeyFromGoString(fmt.Sprintf("p%d", i))
		keep.DefineOwnDataFast(r, k, IntValue(i), attrDefault)
		dict.DefineOwnDataFast(r, k, IntValue(i), attrDefault)
	}
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("keep"), ObjectValue(keep)))
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("dict"), ObjectValue(dict)))
	return keep, dict
}

// TestKeepShape gives an object a host marked with KeepShape more
// properties than maxShapeProps: it stays in shape mode, in an owned
// chain, where the same object unmarked moves to dictionary mode, and its
// reads hit inline caches. Each step runs the same property operations on
// both, a differential against dictionary mode: what a script sees is the
// same, and each change other than at the end moves the marked object to
// dictionary mode, as for the global object.
func TestKeepShape(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		t.Run(fmt.Sprint(opts.SharedIntrinsics), func(t *testing.T) {
			r := NewRealmWith(opts)
			keep, dict := keepShapePair(t, r, 100)
			require.Zero(t, keep.flags&flagDict)
			require.True(t, keep.shape.owned)
			require.NotZero(t, dict.flags&flagDict)
			var e ICEntry
			r.fillGetIC(&e, keep, r.KeyFromGoString("p80"))
			assert.True(t, e.Valid(r, keep))
			runScriptsIn(t, r, `
function readP80(o) { return o.p80 }
function writeP60(o, v) { o.p60 = v }
function addX(o, v) { o.x = v }`)
			owned := true
			for _, c := range []struct {
				src  string
				dict bool // the step moves the marked object to dictionary mode
			}{
				{"readP80(@) + readP80(@)", false},
				{"writeP60(@, 7), writeP60(@, 8), @.p60", false},
				{"Object.keys(@).slice(-3).join() + Object.keys(@).length", false},
				{"[Object.keys(@).indexOf('p51') - Object.keys(@).indexOf('p49'), 'p49' in @, 'p100' in @].join()", false},
				{"addX(@, 1), addX(@, 2), [@.x, Object.keys(@).slice(-2).join()].join()", false},
				{"delete @.x, [typeof @.x, 'x' in @, Object.keys(@).slice(-1).join()].join()", false},
				{"@.x = 'again', @.y = 1, [@.x, @.y, Object.keys(@).slice(-2).join()].join()", false},
				{"delete @.y, delete @.x, @.x = 3, [@.x, typeof @.y].join()", false},
				{"JSON.stringify(Object.getOwnPropertyDescriptor(@, 'p99')) + Object.getOwnPropertyNames(@).length", false},
				{"(() => { let s = ''; for (const k in @) s += k; return s.length })()", false},
				{"delete @.p50", true},
				{"[typeof @.p50, 'p50' in @, readP80(@), Object.keys(@).length].join()", false},
				{"@.p50 = 'again', [@.p50, Object.keys(@).slice(-2).join()].join()", false},
				{"Object.defineProperty(@, 'p10', {writable: false}), (() => { 'use strict'; try { @.p10 = 1 } catch (e) { return e.name + @.p10 } })()", false},
				{"Object.defineProperty(@, 'p20', {get() { return 'g' }, set(v) { this.p21 = v }, configurable: true}), @.p20 = 5, @.p20 + @.p21", false},
				{"Object.freeze(@), [Object.isFrozen(@), delete @.p31, @.p31].join()", false},
			} {
				got := runScriptsIn(t, r, strings.ReplaceAll(c.src, "@", "keep"))
				want := runScriptsIn(t, r, strings.ReplaceAll(c.src, "@", "dict"))
				assert.Equal(t, want, got, c.src)
				owned = owned && !c.dict
				assert.Equal(t, !owned, keep.flags&flagDict != 0, c.src)
			}
		})
	}
}

// TestKeepShapeFallback checks that each change of a marked object other
// than at its end moves it to dictionary mode, with what a script sees the
// same as for the unmarked one.
func TestKeepShapeFallback(t *testing.T) {
	for _, src := range []string{
		"delete @.p5, [typeof @.p5, @.p6, Object.keys(@).length].join()",
		"Object.defineProperty(@, 'p5', {enumerable: false}), [@.p5, Object.keys(@).length].join()",
		"Object.setPrototypeOf(@, {inherited: 1}), [@.inherited, @.p99].join()",
		"Object.seal(@), [Object.isSealed(@), delete @.p99, @.p99].join()",
		"Object.freeze(@), [Object.isFrozen(@), (@.p99 = 1, @.p99)].join()",
		"Object.preventExtensions(@), @.z = 1, [Object.isExtensible(@), typeof @.z, @.p99].join()",
	} {
		for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
			r := NewRealmWith(opts)
			keep, _ := keepShapePair(t, r, 100)
			require.True(t, keep.shape.owned)
			want := runScriptsIn(t, r, strings.ReplaceAll(src, "@", "dict"))
			assert.Equal(t, want, runScriptsIn(t, r, strings.ReplaceAll(src, "@", "keep")), src)
			assert.Equal(t, "0,99", runScriptsIn(t, r, "[keep.p0, keep.p99].join()"), src)
		}
	}
}

// TestKeepShapeRandom runs random property operations on a marked and an
// unmarked object and compares what a script sees after each.
func TestKeepShapeRandom(t *testing.T) {
	r := NewRealm()
	keepShapePair(t, r, 70)
	ops := []string{
		"@.k%d = %d", "delete @.k%d; %d", "@.p%d = %d", "delete @.p%d; %d",
		"Object.defineProperty(@, 'k%d', {value: %d, enumerable: false, configurable: true, writable: true})",
		"'k%d' in @ || %d", "@.k%d + %d",
	}
	check := "JSON.stringify([Object.getOwnPropertyNames(@), Object.keys(@), Object.getOwnPropertyNames(@).map(k => @[k])])"
	rng := rand.New(rand.NewPCG(9, 10))
	for step := range 600 {
		op := fmt.Sprintf(ops[rng.IntN(len(ops))], rng.IntN(80), step)
		a := runScriptsIn(t, r, strings.ReplaceAll(op, "@", "keep"))
		b := runScriptsIn(t, r, strings.ReplaceAll(op, "@", "dict"))
		require.Equal(t, b, a, "step %d %s", step, op)
		require.Equal(t, runScriptsIn(t, r, strings.ReplaceAll(check, "@", "dict")),
			runScriptsIn(t, r, strings.ReplaceAll(check, "@", "keep")), "step %d %s", step, op)
	}
}

// TestKeepShapeAddCache checks that no other object takes a marked
// object's owned shape through an add cache: an object with the same 64
// properties, which shares the marked object's shape until its 65th, moves
// to dictionary mode when the same code adds that property.
func TestKeepShapeAddCache(t *testing.T) {
	r := NewRealm()
	keep, dict := keepShapePair(t, r, 64)
	require.Same(t, keep.shape, dict.shape)
	runScriptsIn(t, r, "function addX(o, v) { o.x = v }; addX(keep, 1); addX(dict, 2)")
	require.True(t, keep.shape.owned)
	require.NotZero(t, dict.flags&flagDict)
	assert.Equal(t, "1,2,65,65", runScriptsIn(t, r, "[keep.x, dict.x, Object.keys(keep).length, Object.keys(dict).length].join()"))
}

// TestKeepShapeSize checks that the mark takes no room: Object keeps its
// size.
func TestKeepShapeSize(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		assert.Equal(t, uintptr(96), unsafe.Sizeof(Object{}))
	}
}

// TestOwnedPropsSnapshot: what takes a snapshot of an object's keys before
// it runs code (Object.assign, JSON.stringify, a spread, Object.entries,
// for-in) does not see a key that the code adds after deleting the last
// one, on a marked object past maxShapeProps, as on the unmarked one: the
// shape the deletion leaves does not extend the props the snapshot reads.
func TestOwnedPropsSnapshot(t *testing.T) {
	for _, src := range []string{
		// A setter on the target deletes the source's last key and adds zz.
		`(() => { const o = @; const to = { set p0(v) { delete o.p99; o.zz = 'new' } };
		  Object.assign(to, o); return [String(to.zz), String(to.p99), 'zz' in o].join() })()`,
		// A toJSON of the first value does the same.
		`(() => { const o = @; o.p0 = { toJSON() { delete o.p98; o.yy = 'new'; return 0 } };
		  const j = JSON.stringify(o); return [j.includes('yy'), j.includes('p98')].join() })()`,
		// An accessor added at the end, read before the last key, does the same.
		`(() => { const o = @; Object.defineProperty(o, 'acc', {get() { delete o.tail; o.zz = 'new'; return 1 }, enumerable: true, configurable: true});
		  o.tail = 2; const c = {...o}; return ['zz' in c, 'tail' in c, c.acc].join() })()`,
		`(() => { const o = @; Object.defineProperty(o, 'acc', {get() { delete o.tail; o.zz = 'new'; return 1 }, enumerable: true, configurable: true});
		  o.tail = 2; return Object.entries(o).slice(-2).join(';') })()`,
		`(() => { const o = @; o.tail = 2; const seen = [];
		  for (const k in o) { if (k === 'p0') { delete o.tail; o.zz = 'new' } if (k === 'zz' || k === 'tail') seen.push(k) }
		  return seen.join() })()`,
	} {
		for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
			r := NewRealmWith(opts)
			keepShapePair(t, r, 100)
			got := runScriptsIn(t, r, strings.ReplaceAll(src, "@", "keep"))
			want := runScriptsIn(t, r, strings.ReplaceAll(src, "@", "dict"))
			assert.Equal(t, want, got, src)
		}
	}
}

// TestOwnedPropsSnapshotGlobal is TestOwnedPropsSnapshot's Object.assign
// on a global object past maxShapeProps, which continues in an owned chain
// the same way.
func TestOwnedPropsSnapshotGlobal(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		r := NewRealmWith(opts)
		// The key list defines every builtin first: the g globals are last.
		runScriptsIn(t, r, "Object.getOwnPropertyNames(globalThis); for (let i = 0; i < 80; i++) globalThis['g' + i] = i")
		require.True(t, r.Global.shape.owned)
		got := runScriptsIn(t, r, `(() => { const o = globalThis; const ks = Object.keys(o); const last = ks[ks.length - 1];
		  const to = { set [ks[0]](v) { delete o[last]; o.zz = 'new' } };
		  Object.assign(to, o); return [last, String(to.zz), String(to[last]), 'zz' in o].join() })()`)
		assert.Equal(t, "g79,undefined,undefined,true", got)
	}
}

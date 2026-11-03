package engine

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// globalCached reports whether a read of the global name hits an inline
// cache filled by the read before it.
func globalCached(r *Realm, name string) bool {
	var e ICEntry
	r.fillGetIC(&e, r.Global, r.KeyFromGoString(name))
	return e.Valid(r, r.Global)
}

// TestGlobalShapeOwned gives the global object more properties than
// maxShapeProps: it stays in shape mode, where any other object moves to
// dictionary mode, so its globals keep their inline caches, through adds
// and deletions of the last property, in mutable and shared realms. Any
// other change moves it to dictionary mode, as before. Each step checks
// what a script sees.
func TestGlobalShapeOwned(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		t.Run(fmt.Sprint(opts.SharedIntrinsics), func(t *testing.T) {
			r := NewRealmWith(opts)
			g := r.Global
			run := func(src string) string {
				t.Helper()
				return runScriptsIn(t, r, src)
			}
			// The key list defines every builtin first: the k globals are last.
			run(`
Object.getOwnPropertyNames(globalThis);
for (var i = 0; i < 100; i++) globalThis['k' + i] = i;
function readK50() { return k50 }
function readK99() { return k99 }
function writeK60(v) { k60 = v }`)
			require.Zero(t, g.flags&flagDict)
			require.True(t, g.shape.owned)
			assert.True(t, globalCached(r, "k50"))
			assert.True(t, globalCached(r, "JSON"))
			owned := true
			for _, c := range []struct {
				src, want string
				dict      bool // the step moves the global object to dictionary mode
			}{
				{"readK50() + readK50()", "100", false},
				{"writeK60(7), writeK60(8), k60", "8", false},
				{"Object.keys(globalThis).slice(-3).join()", "k97,k98,k99", false},
				{"[Object.keys(globalThis).indexOf('k51') - Object.keys(globalThis).indexOf('k49'), 'k49' in globalThis, 'k100' in globalThis].join()", "2,true,false", false},
				// Deleting the last property and adding others.
				{"delete globalThis.k99, [typeof k99, 'k99' in globalThis, Object.keys(globalThis).slice(-1).join()].join()", "undefined,false,k98", false},
				{"(() => { try { return readK99() } catch (e) { return e.name } })()", "ReferenceError", false},
				{"globalThis.k99 = 'again', globalThis.k100 = 100, [readK99(), k100, Object.keys(globalThis).slice(-2).join()].join()", "again,100,k99,k100", false},
				{"delete globalThis.k100, delete globalThis.k99, globalThis.k99 = 3, [readK99(), typeof k100].join()", "3,undefined", false},
				// A deletion in the middle keeps the order of the rest, and
				// the caches filled before it miss.
				{"delete globalThis.k50", "true", true},
				{"[typeof k50, 'k50' in globalThis, Object.keys(globalThis).indexOf('k51') - Object.keys(globalThis).indexOf('k49'), readK99()].join()", "undefined,false,1,3", false},
				{"(() => { try { return readK50() } catch (e) { return e.name } })()", "ReferenceError", false},
				{"globalThis.k50 = 'again', [readK50(), Object.keys(globalThis).slice(-2).join()].join()", "again,k99,k50", false},
				// Redefinitions.
				{"Object.defineProperty(globalThis, 'k10', {writable: false}), (() => { 'use strict'; try { k10 = 1 } catch (e) { return e.name + k10 } })()", "TypeError10", false},
				{"k10 = 2, k10", "10", false},
				{"Object.defineProperty(globalThis, 'k20', {get() { return 'g' }, set(v) { this.k21 = v }, configurable: true}), k20 = 5, k20 + k21", "g5", false},
				{"Object.defineProperty(globalThis, 'k20', {value: 'v'}), k20", "v", false},
				{"Object.getOwnPropertyNames(globalThis).filter(k => k[0] == 'k' && k.length > 1).length", "100", false},
				// Freezing.
				{"Object.freeze(globalThis), Object.isFrozen(globalThis)", "true", false},
				{"(() => { 'use strict'; try { k30 = 1 } catch (e) { return e.name + k30 } })()", "TypeError30", false},
				{"(() => { 'use strict'; try { globalThis.k101 = 1 } catch (e) { return e.name + typeof k101 } })()", "TypeErrorundefined", false},
				{"[delete globalThis.k31, k31, readK50()].join()", "false,31,again", false},
			} {
				assert.Equal(t, c.want, run(c.src), c.src)
				owned = owned && !c.dict
				assert.Equal(t, !owned, g.flags&flagDict != 0, c.src)
			}
		})
	}
}

// TestGlobalShapeFallback checks that each change of an owned global
// object other than at its end moves it to dictionary mode, with what a
// script sees unchanged.
func TestGlobalShapeFallback(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"delete globalThis.k5, [typeof k5, k6, Object.keys(globalThis).length].join()", "undefined,6,100"},
		{"Object.defineProperty(globalThis, 'k5', {enumerable: false}), [k5, Object.keys(globalThis).length].join()", "5,100"},
		{"Object.setPrototypeOf(globalThis, {inherited: 1}), [inherited, k99].join()", "1,99"},
		{"Object.seal(globalThis), [Object.isSealed(globalThis), delete globalThis.k99, k99].join()", "true,false,99"},
		{"Object.freeze(globalThis), [Object.isFrozen(globalThis), (k99 = 1, k99)].join()", "true,99"},
	} {
		for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
			r := NewRealmWith(opts)
			runScriptsIn(t, r, "for (var i = 0; i < 100; i++) globalThis['k' + i] = i")
			require.True(t, r.Global.shape.owned)
			assert.Equal(t, c.want, runScriptsIn(t, r, c.src), c.src)
			assert.NotZero(t, r.Global.flags&flagDict, c.src)
			assert.Equal(t, "0,99", runScriptsIn(t, r, "[k0, k99].join()"), c.src)
		}
	}
}

// TestGlobalShapeChurn adds and deletes the last globals of an owned
// global object at random and checks every key after each step: the
// object stays in shape mode, never returns to a shape it had, and its
// lookup table drops each deleted key (shapeIndex.remove).
func TestGlobalShapeChurn(t *testing.T) {
	r := NewRealm()
	g := r.Global
	runScriptsIn(t, r, "for (var i = 0; i < 70; i++) globalThis['base' + i] = i")
	base := g.shape.count
	var keys []PropertyKey
	seen := map[*Shape]bool{}
	rng := rand.New(rand.NewPCG(1, 2))
	for step := range 3000 {
		if len(keys) > 0 && rng.IntN(5) < 2 {
			k := keys[len(keys)-1]
			keys = keys[:len(keys)-1]
			require.True(t, g.Delete(r, k))
		} else {
			// A small pool of names: most adds are of a name deleted before.
			k := r.KeyFromGoString(fmt.Sprintf("x%d", rng.IntN(60)))
			if g.HasOwnProperty(k) {
				continue
			}
			require.NoError(t, g.SetProp(r, k, IntValue(step)))
			keys = append(keys, k)
		}
		require.Zero(t, g.flags&flagDict)
		require.False(t, seen[g.shape], "a shape the object had")
		seen[g.shape] = true
		require.Equal(t, base+uint32(len(keys)), g.shape.count)
		for i, k := range keys {
			slot, _, ok := g.shape.Lookup(k)
			require.True(t, ok, "step %d key %d", step, i)
			require.True(t, g.shape.has(k))
			require.Equal(t, base+uint32(i), slot)
		}
		for range 3 {
			k := r.KeyFromGoString(fmt.Sprintf("x%d", rng.IntN(60)))
			_, _, ok := g.shape.Lookup(k)
			require.Equal(t, ok, g.shape.has(k))
		}
	}
}

// TestShapeIndexRemove removes the last slots of a lookup table one by one,
// with keys that collide, and finds every key left.
func TestShapeIndexRemove(t *testing.T) {
	r := NewRealm()
	props := make([]shapeProp, 300)
	for i := range props {
		props[i] = shapeProp{key: r.KeyFromGoString(fmt.Sprintf("p%d", i)), attrs: attrDefault}
	}
	tab := newShapeTable(props)
	for n := len(props); n > 0; n-- {
		tab.remove(props[:n], uint32(n-1))
		for i := range n - 1 {
			require.Equal(t, uint32(i+1), tab.find(props[:n-1], props[i].key), "n=%d i=%d", n, i)
		}
		require.Zero(t, tab.find(props[:n], props[n-1].key))
	}
}

// TestGlobalShapeDelete deletes a global that is not the last property:
// the global object moves to dictionary mode, as any object does, with
// what a script sees unchanged. A script's many top-level declarations
// keep it in shape mode.
func TestGlobalShapeDelete(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		r := NewRealmWith(opts)
		assert.Equal(t, "undefined,2,readB,b", runScriptsIn(t, r, `
globalThis.a = 1; globalThis.b = 2;
function readB() { return b }
readB();
delete globalThis.a;
[typeof a, readB(), Object.keys(globalThis).join()].join()`))
		assert.NotZero(t, r.Global.flags&flagDict)
		r = NewRealmWith(opts)
		var b strings.Builder
		for i := range 200 {
			fmt.Fprintf(&b, "function f%d() { return %d }\nvar v%d = %d;\n", i, i, i, i)
		}
		b.WriteString("f150() + v199")
		assert.Equal(t, "349", runScriptsIn(t, r, b.String()))
		assert.Zero(t, r.Global.flags&flagDict)
		assert.True(t, globalCached(r, "f150"))
	}
}

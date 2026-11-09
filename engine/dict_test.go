package engine

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDictModel runs random additions, deletions and lookups on a
// dictionary against a Go map and an insertion-ordered list, through the
// linear scan of a few entries, the index, its growth, the deletions that
// move slots back and the compactions.
func TestDictModel(t *testing.T) {
	r := NewRealm()
	keys := make([]PropertyKey, 3010)
	for i := range keys {
		if i%7 == 0 {
			keys[i] = SymbolKey(NewSymbol(asciiString(fmt.Sprint(i))))
		} else {
			keys[i] = StringKey(r.InternGoString(fmt.Sprintf("k%d", i)))
		}
	}
	for _, n := range []int{5, 9, 40, 300, 3000} {
		rng := rand.New(rand.NewPCG(uint64(n), 1))
		var d dictProps
		model := map[PropertyKey]int{}
		var order []PropertyKey
		for step := range 20 * n {
			k := keys[rng.IntN(n)]
			if _, ok := model[k]; ok && rng.IntN(3) == 0 {
				ok, _ := d.remove(k)
				require.True(t, ok)
				delete(model, k)
				for i, o := range order {
					if o == k {
						order = append(order[:i], order[i+1:]...)
						break
					}
				}
			} else if !ok {
				d.add(k, propCell{value: IntValue(step)})
				model[k] = step
				order = append(order, k)
			}
			if step%97 == 0 || step == 20*n-1 {
				require.Equal(t, len(model), d.namedCount(), "n=%d step %d", n, step)
				for _, k := range keys[:n+5] {
					c, ok := d.lookup(k)
					v, want := model[k]
					require.Equal(t, want, ok, "n=%d step %d %s", n, step, k.GoString())
					if ok {
						require.Equal(t, IntValue(v), c.value)
					}
				}
				var live []PropertyKey
				for i := range d.entries {
					if e := &d.entries[i]; e.live() {
						live = append(live, e.key)
					}
				}
				require.Equal(t, order, live, "n=%d step %d: insertion order", n, step)
				if len(d.entries) > dictLinear {
					require.NotNil(t, d.index, "an index past dictLinear entries")
				}
			}
		}
		ok, _ := d.remove(keys[n])
		require.False(t, ok, "absent key")
	}
}

// TestDictShadowed checks that entries built without add keep the last of
// a key's entries, with and without an index.
func TestDictShadowed(t *testing.T) {
	r := NewRealm()
	for _, n := range []int{4, 20} {
		var d dictProps
		for i := range n {
			k := StringKey(r.InternGoString(fmt.Sprintf("s%d", i%(n/2))))
			d.entries = append(d.entries, dictEntry{key: k, cell: propCell{value: IntValue(i)}})
		}
		d.dropShadowed()
		require.Len(t, d.entries, n/2)
		for i := range n / 2 {
			c, ok := d.lookup(StringKey(r.InternGoString(fmt.Sprintf("s%d", i))))
			require.True(t, ok)
			require.Equal(t, IntValue(i+n/2), c.value, "the last entry wins")
		}
	}
}

// TestDictObject checks objects in dictionary mode from JavaScript: one
// that goes on past maxShapeProps, one a deletion moved there, and their
// keys, values and order after deletions, additions and re-additions.
func TestDictObject(t *testing.T) {
	r := NewRealm()
	got := runScriptsIn(t, r, `
		var big = {};
		for (var i = 0; i < 500; i++) big["z" + i] = i;
		for (var i = 0; i < 500; i += 3) delete big["z" + i];
		for (var i = 0; i < 500; i += 6) big["z" + i] = -i;
		var ok = Object.keys(big).length === 500 - 167 + 84;
		for (var i = 0; i < 500; i++) {
			var want = i % 6 === 0 ? -i : i % 3 === 0 ? undefined : i;
			if (big["z" + i] !== want || ("z" + i in big) !== (want !== undefined)) ok = "z" + i;
		}
		var ks = Object.keys(big);
		if (ks[0] !== "z1" || ks[ks.length - 1] !== "z498") ok = ks[0] + ks[ks.length - 1];
		var small = {a: 1, b: 2, c: 3};
		delete small.a;
		small.d = 4;
		[ok, JSON.stringify(small), "a" in small].join()`)
	require.Equal(t, `true,{"b":2,"c":3,"d":4},false`, got)
	require.NotZero(t, jsGlobal(t, r, "big").AsObject().flags&flagDict)
	require.NotZero(t, jsGlobal(t, r, "small").AsObject().flags&flagDict)
}

// TestDictRoom checks that an object that goes on past maxShapeProps takes
// the entries the last object that went on from its shape grew to, at
// most dictRoomMax, and room for as many again from a shape of the shared
// tree, whose sentinel records nothing. Over the shared intrinsics the
// chain of keys from data is shared or the realm's own, as the realms
// before it in the process left the tree's room for such keys.
func TestDictRoom(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		generic := false // the objects share their prototype's sentinel
		room := func(n int) int {
			runScriptsIn(t, r, fmt.Sprintf(`
				var o = {};
				for (var i = 0; i < %d; i++) o["dr" + i] = i;
				globalThis.o = o;`, n))
			o := jsGlobal(t, r, "o").AsObject()
			require.NotZero(t, o.flags&flagDict)
			require.Equal(t, n, o.dictTable().namedCount())
			generic = o.shape == r.dictShapeFor(o.Proto())
			return cap(o.dictTable().entries)
		}
		// The capacity n entries take, which their size class rounds up.
		capOf := func(n int) int { return cap(slices.Grow([]dictEntry(nil), n)) }
		require.Equal(t, capOf(2*maxShapeProps), room(70), "room for as many again")
		require.False(t, generic && !shared, "a realm's own shape has a sentinel of its own")
		require.GreaterOrEqual(t, room(245), 245)
		want := capOf(dictRoomMax)
		if generic {
			want = capOf(2 * maxShapeProps)
		}
		require.Equal(t, want, room(70), "shared %v: the room the last one took", shared)
		room(3000)
		require.Equal(t, want, room(70), "shared %v: at most dictRoomMax", shared)
	}
}

package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

// inlineSlots reports whether o's slots are the buffer allocated with o.
func inlineSlots(o *Object) bool {
	// Compared as integers: o may end where its slots would start, and
	// checkptr (-race) rejects a pointer past the end of an allocation.
	return cap(o.slots) != 0 && uintptr(unsafe.Pointer(unsafe.SliceData(o.slots))) == uintptr(unsafe.Pointer(o))+unsafe.Sizeof(Object{})
}

// TestLiteralSlots checks that an object literal's slots are allocated
// with it, in the bucket of two, four, six or eight that holds them up to
// eight, exactly up to thirteen and with one more past them up to
// coSlotsMax, and in a slice of their own past that.
func TestLiteralSlots(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		for n, want := range map[int]int{1: 2, 2: 2, 3: 4, 4: 4, 5: 6, 7: 8, 8: 8, 9: 9, 10: 10, 11: 11, 12: 12, 13: 13, 14: 15, 15: 15, 16: 17, 17: 17, 18: 18} {
			var b strings.Builder
			for i := range n {
				fmt.Fprintf(&b, "p%d: %d, ", i, i)
			}
			runScriptsIn(t, r, "globalThis.o = {"+b.String()+"};")
			o := jsGlobal(t, r, "o").AsObject()
			assert.Equal(t, n, len(o.slots), "%d properties", n)
			assert.Equal(t, want, cap(o.slots), "%d properties", n)
			assert.Equal(t, n <= coSlotsMax, inlineSlots(o), "%d properties", n)
		}
	}
}

// TestSlotsGrowDownTrunk checks that an object whose slots are full grows
// them to the slots the objects before it took down the chain of only
// children below its shape (Object.growSlots), to the end of the chain
// where that has no children, with one slot more where it has several,
// and as append does for the first object down a chain.
func TestSlotsGrowDownTrunk(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		runScriptsIn(t, r, `
			function mk() { var o = {trunkA: 1}; o.trunkB = 2; o.trunkC = 3; o.trunkD = 4; o.trunkE = 5; return o; }
			globalThis.x = mk(); globalThis.y = mk();
			function br(k) { var o = {trunkF: 1}; o.trunkG = 2; o.trunkH = 3; o.trunkK = 4; o.trunkL = 5; if (k) o[k] = 6; return o; }
			br("trunkM"); br("trunkN");
			globalThis.z = br("");`)
		x, y, z := jsGlobal(t, r, "x").AsObject(), jsGlobal(t, r, "y").AsObject(), jsGlobal(t, r, "z").AsObject()
		assert.Equal(t, 8, cap(x.slots), "the first down the chain doubles: 2, 4, 8")
		assert.Equal(t, 5, cap(y.slots), "the second grows once, to the chain's end")
		assert.Equal(t, 6, cap(z.slots), "a chain ending in a branch gives its slots and one more")
		assert.Equal(t, 5, len(y.slots))
		assert.Equal(t, 5, len(z.slots))
	}
}

// TestSlotsGrowToLeaf checks that an object whose slots are full, where
// the objects before it went on from its shape, short of append's
// doubling, grows to where they stopped: the end of a chain no object went
// past, or the most of the chains below a branch (Shape.room); the first
// object down the chain doubles as append does.
func TestSlotsGrowToLeaf(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		runScriptsIn(t, r, `
			function mk() { var o = {la: 1, lb: 2, lc: 3, ld: 4}; o.le = 5; o.lf = 6; return o; }
			globalThis.x = mk(); globalThis.y = mk();
			function br(k) { var o = {ma: 1, mb: 2, mc: 3, md: 4}; o.me = 5; o.mf = 6; if (k) o[k] = 7; return o; }
			br("mg"); br("mh");
			globalThis.z = br("");`)
		x, y, z := jsGlobal(t, r, "x").AsObject(), jsGlobal(t, r, "y").AsObject(), jsGlobal(t, r, "z").AsObject()
		assert.Equal(t, 8, cap(x.slots), "shared %v: the first down the chain doubles", shared)
		assert.Equal(t, 6, cap(y.slots), "shared %v: the second grows to the chain's end", shared)
		if is64 { // a 64-bit layout
			assert.Equal(t, 7, cap(z.slots), "shared %v: a chain ending in a branch grows to the most below it", shared)
		}
		assert.Equal(t, "5,6", runScriptsIn(t, r, `[y.le, y.lf].join()`))
	}
}

// TestCreateSlots checks that Object.create gives its object room for the
// properties the objects created from the prototype before it took, or
// for those of its second argument, in the object's own allocation.
func TestCreateSlots(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		runScriptsIn(t, r, `
			var P = {};
			function mk() { var o = Object.create(P); o.ca = 1; o.cb = 2; o.cc = 3; return o; }
			globalThis.c1 = mk(); globalThis.c2 = mk();
			globalThis.n1 = Object.create(null);
			var ds = {da: {value: 1}, db: {value: 2}, dc: {value: 3}, dd: {value: 4}, de: {value: 5}};
			globalThis.d1 = Object.create(P, ds); globalThis.d2 = Object.create(null, ds);`)
		c1, c2 := jsGlobal(t, r, "c1").AsObject(), jsGlobal(t, r, "c2").AsObject()
		assert.Equal(t, 4, cap(c1.slots), "the first grows as append does: 1, 2, 4")
		assert.False(t, inlineSlots(c1))
		assert.Equal(t, 4, cap(c2.slots), "the second has room for the three")
		assert.True(t, inlineSlots(c2))
		assert.Equal(t, "1,2,3", runScriptsIn(t, r, `[c2.ca, c2.cb, c2.cc].join()`))
		assert.Equal(t, 0, cap(jsGlobal(t, r, "n1").AsObject().slots))
		for _, name := range []string{"d1", "d2"} {
			d := jsGlobal(t, r, name).AsObject()
			assert.Equal(t, 5, len(d.slots), name)
			assert.Equal(t, 6, cap(d.slots), "%s: room for the five descriptors", name)
			assert.True(t, inlineSlots(d), name)
		}
	}
}

// mkTrunk defines mk(n), an object of wa and wb and then n keys more, and
// cr(n), one Object.create makes from a prototype of the program's and
// then gives n keys.
const mkTrunk = `
	function mk(n) { var o = {wa: 1}; o.wb = 2; for (var i = 0; i < n; i++) o["wk" + i] = i; return o; }
	var P = {};
	function cr(n) { var o = Object.create(P); for (var i = 0; i < n; i++) o["ck" + i] = i; return o; }`

// TestSlotsTrunkBounded checks the room a chain gives when one object went
// on far past where a thousand others stopped: objects that grow there, or
// that Object.create makes from the prototype, get at most coSlotsMax
// slots past their count beyond what append gives, however far that one
// went.
func TestSlotsTrunkBounded(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, far := range []int{28, maxShapeProps - 2} {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			runScriptsIn(t, r, mkTrunk+fmt.Sprintf(`
				for (var i = 0; i < 1000; i++) { mk(0); cr(0); }
				globalThis.far = mk(%d); cr(%d);
				globalThis.ws = []; globalThis.cs = [];
				for (var i = 0; i < 10; i++) { ws.push(mk(1)); cs.push(cr(0)); }`, far, far))
			assert.Equal(t, far+2, len(jsGlobal(t, r, "far").AsObject().slots))
			ws, cs := jsGlobal(t, r, "ws").AsObject(), jsGlobal(t, r, "cs").AsObject()
			for i := range 10 {
				w, c := ws.elems()[i].AsObject(), cs.elems()[i].AsObject()
				assert.Equal(t, 3, len(w.slots))
				// Room for coSlotsMax past the two, as append rounds it to its
				// size class.
				assert.Equal(t, cap(slices.Grow(make([]Value, 2), coSlotsMax)), cap(w.slots), "%d keys, object %d", far+2, i)
				assert.Equal(t, 0, len(c.slots))
				assert.Equal(t, coSlotsMax, cap(c.slots), "%d keys, object %d: room for coSlotsMax", far, i)
			}
		}
	}
}

// TestSlotsTrunkTypical checks that the room of the whole chain comes back
// once the objects given less show it is their common path: after
// trunkProbe objects that went on to its end, the next ones that grow
// there, or that Object.create makes, take it at once. Shapes of the
// shared tree, which keep no tally, keep the bound; over the shared
// intrinsics, mk's shapes are of the shared tree unless the realms before
// it in the process filled the tree.
func TestSlotsTrunkTypical(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, far := range []int{28, maxShapeProps - 2} {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			runScriptsIn(t, r, mkTrunk+fmt.Sprintf(`
				for (var i = 0; i < %d; i++) { mk(%d); cr(%d); }
				globalThis.w = mk(1); globalThis.c = cr(0);`, trunkProbe+2, far, far))
			w, c := jsGlobal(t, r, "w").AsObject(), jsGlobal(t, r, "c").AsObject()
			want := cap(slices.Grow(make([]Value, 2), far))
			if w.shape.parent.shared {
				// The shape {wa, wb} w grew at; cr's are below P's own root.
				want = cap(slices.Grow(make([]Value, 2), coSlotsMax))
			}
			assert.Equal(t, want, cap(w.slots), "shared %v, %d keys", shared, far+2)
			assert.Equal(t, far, cap(c.slots), "shared %v, %d keys", shared, far)
		}
	}
}

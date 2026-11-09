package engine

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unsafe"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warmKeys sets the realm past keyMemoAfter, after which it remembers keys
// (rememberKey).
func warmKeys(r *Realm) {
	r.lazyState().keyMisses = keyMemoAfter
}

// memoHolds reports whether the realm's key memo holds s, in either way of
// its set.
func memoHolds(r *Realm, s *String) bool {
	set := &r.lazy.keys[keyMemoSlot(s)]
	return set[0].s == s || set[1].s == s
}

// TestKeyMemo reads properties through names kept in an array of string
// literals, as the Intl polyfill's option tables do, whose content the
// realm interned from data first (JSON.parse here): the code's constants
// then keep their own Strings, not the atoms (constASCIIAtom), and after
// the first reads their keys come from the memo, each the atom Intern
// gives for the content.
func TestKeyMemo(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		t.Run(fmt.Sprint(opts.SharedIntrinsics), func(t *testing.T) {
			r := NewRealmWith(opts)
			warmKeys(r)
			// Names no other test interns, so that their atoms come from
			// this realm's data and not from another test's constants.
			p := fmt.Sprintf("memo%vK", opts.SharedIntrinsics)
			names := []string{p + "weekday", p + "era", p + "year", p + "month", p + "day"}
			runScriptsIn(t, r, fmt.Sprintf(`var data = JSON.parse('{"%s": 1, "%s": 2, "%s": 3, "%s": 4, "%s": 5}')`,
				names[0], names[1], names[2], names[3], names[4]))
			got := runScriptsIn(t, r, fmt.Sprintf(`var names = ["%s", "%s", "%s", "%s", "%s"];
var o = data, sum = 0;
for (let i = 0; i < 100; i++) for (const n of names) sum += o[n];
for (const n of names) o[n] += 10;
sum + ',' + names.map(n => o[n]).join()`, names[0], names[1], names[2], names[3], names[4]))
			assert.Equal(t, "1500,11,12,13,14,15", got)
			lit, err := r.Global.GetProp(r, r.KeyFromGoString("names"))
			require.NoError(t, err)
			lits := lit.AsObject().elems()
			for _, v := range lits {
				require.Zero(t, v.AsString().atom, "the literal keeps its own String")
			}
			require.NotNil(t, r.lazy)
			require.NotNil(t, r.lazy.keys)
			// The memo set is by address (keyMemoSlot), so three of the
			// names can share one, the latest reads evicting the earliest:
			// each name is in the memo unless its set holds two others.
			for _, v := range lits {
				if memoHolds(r, v.AsString()) {
					continue
				}
				for _, e := range &r.lazy.keys[keyMemoSlot(v.AsString())] {
					assert.True(t, slices.ContainsFunc(lits, func(w Value) bool { return w.AsString() == e.s }),
						"%s: its set holds %v", v.AsString(), e.s)
				}
			}
			for i := range r.lazy.keys {
				for _, e := range r.lazy.keys[i] {
					if e.s == nil {
						continue
					}
					assert.Zero(t, e.s.atom, "only strings that are not atoms are remembered")
					assert.Equal(t, StringKey(r.Intern(e.s)), e.k, e.s.String())
					assert.Equal(t, uint32(i), keyMemoSlot(e.s))
					assert.Equal(t, e.k, r.KeyFromString(e.s))
				}
			}
		})
	}
}

// TestKeyMemoEligible checks which strings the memo takes: short flat
// strings that keep nothing else alive. A rope, a long string, a substring
// sharing a longer string's units, a string starting an append buffer and
// a string of a host conversion are interned each time, and their keys
// are the same.
func TestKeyMemoEligible(t *testing.T) {
	r := NewRealm()
	warmKeys(r)
	long := FromGoString(strings.Repeat("abcdefgh", 16))
	// Concat makes ropes from ropeThreshold units on, past the memo's
	// bound; a short one still is not taken.
	rope := &String{p: unsafe.Pointer(FromGoString("ab")), right: FromGoString("cdefgh"), n: 8, kind: strRope}
	sub := long.Substring(8, 16)
	require.NotNil(t, sub.right)
	for _, c := range []struct {
		name string
		s    *String
		memo bool
	}{
		{"flat", FromGoString("memoName"), true},
		{"utf16", FromGoString("名前"), true},
		{"rope", rope, false},
		{"long", FromGoString(strings.Repeat("k", maxSharedDataKey+1)), false},
		{"longest", FromGoString(strings.Repeat("j", maxSharedDataKey)), true},
		{"shares a root", sub, false},
		{"append buffer", &String{s: "inBuf", n: 5, kind: strASCII, flags: strInBuf}, false},
		{"host", &String{s: "host", n: 4, kind: strASCII, flags: strHost}, false},
		{"json slice", &String{s: "slice", n: 5, kind: strASCII, flags: strSlice}, false},
	} {
		k := r.KeyFromString(c.s)
		assert.Equal(t, StringKey(r.Intern(c.s)), k, c.name)
		require.NotNil(t, r.lazy.keys, c.name)
		assert.Equal(t, c.memo, memoHolds(r, c.s), c.name)
		assert.Equal(t, k, r.KeyFromString(c.s), c.name)
	}
	// An index is a key of its own, never an atom in the memo.
	idx := FromGoString("42")
	assert.Equal(t, IndexKey(42), r.KeyFromString(idx))
	assert.False(t, memoHolds(r, idx))
}

// TestKeyMemoLazy: a realm creates the memo only after keyMemoAfter
// strings it would take, and only with lazy state, so a runtime that
// converts a few pays nothing.
func TestKeyMemoLazy(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	r.KeyFromString(FromGoString("first"))
	assert.True(t, r.lazy == nil || r.lazy.keys == nil)
	r.lazyState()
	for i := range keyMemoAfter {
		r.KeyFromString(FromGoString(fmt.Sprint("name", i)))
	}
	require.Nil(t, r.lazy.keys)
	assert.Equal(t, uint16(keyMemoAfter), r.lazy.keyMisses)
	s := FromGoString("later")
	r.KeyFromString(s)
	require.NotNil(t, r.lazy.keys)
	assert.Same(t, s, r.lazy.keys[keyMemoSlot(s)][0].s)
}

// TestKeyMemoCollision: two Strings of one set are both kept, so names a
// loop reads in turn hit however their Strings fell; a third taking the
// set replaces the one remembered first, which is interned again on its
// next use and goes first.
func TestKeyMemoCollision(t *testing.T) {
	r := NewRealm()
	warmKeys(r)
	first := FromGoString("collideA")
	r.KeyFromString(first)
	var more []*String
	for i := 0; len(more) < 2; i++ {
		require.Less(t, i, 100000)
		if s := FromGoString(fmt.Sprint("collide", i)); keyMemoSlot(s) == keyMemoSlot(first) {
			more = append(more, s)
		}
	}
	second, third := more[0], more[1]
	set := &r.lazy.keys[keyMemoSlot(first)]
	k2 := r.KeyFromString(second)
	for range 3 {
		assert.Equal(t, StringKey(r.Intern(first)), r.KeyFromString(first))
		assert.Equal(t, k2, r.KeyFromString(second))
		assert.Same(t, second, set[0].s, "a hit moves nothing")
		assert.Same(t, first, set[1].s)
	}
	k3 := r.KeyFromString(third)
	assert.Same(t, third, set[0].s)
	assert.Same(t, second, set[1].s)
	assert.False(t, memoHolds(r, first))
	assert.Equal(t, StringKey(r.Intern(first)), r.KeyFromString(first))
	assert.Same(t, first, set[0].s)
	assert.Same(t, third, set[1].s)
	assert.Equal(t, k3, r.KeyFromString(third))
}

// TestKeyMemoHoldsString: the memo holds the String it maps, so it is not
// collected while its entry stands and no other String can come to its
// address and be taken for it.
func TestKeyMemoHoldsString(t *testing.T) {
	r := NewRealm()
	warmKeys(r)
	s := FromGoString("heldName")
	slot := keyMemoSlot(s)
	k := r.KeyFromString(s)
	w := weak.Make(s)
	s = nil
	runtime.GC()
	runtime.GC()
	held := w.Value()
	require.NotNil(t, held)
	assert.Same(t, held, r.lazy.keys[slot][0].s)
	assert.Equal(t, k, r.KeyFromString(held))
}

// TestKeyMemoJSONSlice: in a realm without a memory limit a short
// JSON.parse literal is a slice of the JSON text (jsonParser.plain), which
// the key memo does not take: as a key it kept its whole text alive after
// the program dropped it, for as many texts as the memo has entries.
func TestKeyMemoJSONSlice(t *testing.T) {
	r := NewRealm()
	warmKeys(r)
	require.Nil(t, r.lazy.mem, "no memory limit")
	runScriptsIn(t, r, `var probe = {}; function use(t) { return probe[JSON.parse(t).name]; }`)
	use := jsGlobal(t, r, "use")
	const texts, size = 2 * keyMemoSets * keyMemoWays, 64 << 10
	pad := strings.Repeat("x", size)
	alive := make([]weak.Pointer[byte], texts)
	for i := range alive {
		text := FromGoString(fmt.Sprintf(`{"name":"slice%d","pad":"%s"}`, i, pad))
		alive[i] = weak.Make(unsafe.StringData(text.s))
		_, err := r.Call(use, Undefined(), []Value{StringValue(text)})
		require.NoError(t, err)
	}
	runScriptsIn(t, r, `use('{"name":"x"}')`)
	runtime.GC()
	runtime.GC()
	kept := 0
	for _, w := range alive {
		if w.Value() != nil {
			kept++
		}
	}
	assert.Zero(t, kept, "JSON texts of %d KiB kept after GC", size>>10)
	runtime.KeepAlive(r)
}

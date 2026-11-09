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

// TestConcatMemo concatenates random short ASCII pieces, some many times
// and split in different places, as a render builds its class names and
// keys: each result is the concatenation of its own pieces, and one
// already in the memo comes from it without an allocation.
func TestConcatMemo(t *testing.T) {
	r := NewRealm()
	warmMemos(r)
	rng := rand.New(rand.NewPCG(5, 6))
	var names []string
	for range 200 {
		b := make([]byte, 2+rng.IntN(ropeThreshold-2))
		for i := range b {
			b[i] = byte('!' + rng.IntN(94))
		}
		names = append(names, string(b))
	}
	for range 6000 {
		g := names[rng.IntN(len(names))]
		k := 1 + rng.IntN(len(g)-1)
		a, b := FromGoString(g[:k]), FromGoString(g[k:])
		s, err := r.Concat(a, b)
		require.NoError(t, err)
		require.Equal(t, g, s.GoString())
		require.Equal(t, strASCII, s.kind)
		require.Equal(t, g[:k], a.GoString(), "the pieces are untouched")
		require.Equal(t, g[k:], b.GoString())
	}
	// A result in the memo comes from it, of other pieces with the same
	// units or the same pieces, and allocates nothing.
	x, y := FromGoString("MuiButton-"), FromGoString("root")
	s1, _ := r.Concat(x, y)
	s2, _ := r.Concat(FromGoString("MuiButton"), FromGoString("-root"))
	assert.Same(t, s1, s2)
	assert.Equal(t, 0.0, testing.AllocsPerRun(50, func() { r.Concat(x, y) }))
	assert.Equal(t, "MuiButton-root", s1.GoString())
	// Appending to a result in the memo leaves it as it was.
	long := strings.Repeat("z", 60)
	for range 3 {
		s, _ := r.Concat(s1, FromGoString(long))
		assert.Equal(t, "MuiButton-root"+long, s.GoString())
	}
	s3, _ := r.Concat(x, y)
	assert.Same(t, s1, s3)
	assert.Equal(t, "MuiButton-root", s3.GoString())
	// Through scripts, as a render concatenates.
	assert.Equal(t, "css-1a2b,css-1a2b,true",
		runScriptsIn(t, r, `var p = 'css-', h = '1a2b'; var a = p + h, b = 'css-1' + 'a2b'; [a, b, a === b].join()`))
}

// TestConcatMemoRecent: a run of the same concatenations, as a tree of
// leaves built with one key makes, returns the last two results the memo
// returned, from the same pieces, without the hash or a comparison of
// units: even once the memo has dropped them, and without an allocation.
// Pieces with the same units elsewhere take the memo. Only pieces that own
// their units are kept: an atom, a number's string, a short
// concatenation's result; of a substring, a host's string or a JSON value
// the entry keeps the address of its units, and a hit compares them. The
// live measurement counts the results and the pieces kept.
func TestConcatMemoRecent(t *testing.T) {
	r := NewRealm()
	warmMemos(r)
	key, in := r.InternGoString("String for key "), r.InternGoString(" in leaf node")
	tag := NumberToString(0.25)
	require.True(t, ownsUnits(key) && ownsUnits(in) && ownsUnits(tag))
	var s1, s2 *String
	for range 3 {
		s1, _ = r.Concat(key, tag)
		s2, _ = r.Concat(s1, in)
	}
	require.True(t, ownsUnits(s1))
	ms := r.lazy.memos
	require.NotNil(t, ms)
	assert.ElementsMatch(t, []concatRecent{{a: key, b: tag, s: s1}, {a: s1, b: in, s: s2}}, ms.recent[:])
	clear(ms.concats[:])
	hits := ms.concat.hits
	for range 3 {
		x, _ := r.Concat(key, tag)
		assert.Same(t, s1, x)
		y, _ := r.Concat(x, in)
		assert.Same(t, s2, y)
	}
	assert.Equal(t, hits+6, ms.concat.hits)
	assert.Equal(t, 0.0, testing.AllocsPerRun(50, func() {
		x, _ := r.Concat(key, tag)
		r.Concat(x, in)
	}))
	// A new tag's first leaf keeps its results, which its second takes.
	tag2 := NumberToString(0.5)
	x, _ := r.Concat(key, tag2)
	y, _ := r.Concat(x, in)
	assert.ElementsMatch(t, []concatRecent{{a: key, b: tag2, s: x}, {a: x, b: in, s: y}}, ms.recent[:])
	assert.Equal(t, "String for key 0.5 in leaf node", y.GoString())
	for _, c := range [][2]string{
		{"String for key ", "0.25"}, {"String for key 0", ".25"}, {"String for key ", "0.26"},
		{"String for kez ", "0.25"}, {"String for key 0.25", " in leaf nodE"},
	} {
		s, err := r.Concat(FromGoString(strings.Clone(c[0])), FromGoString(strings.Clone(c[1])))
		require.NoError(t, err)
		assert.Equal(t, c[0]+c[1], s.GoString())
		assert.NotSame(t, s1, s)
		assert.NotSame(t, s2, s)
	}
	// Pieces that share a larger string's units are kept by the addresses
	// of their units, which keep nothing alive: a hit compares the units.
	big := asciiStringCopy([]byte(strings.Repeat("0.75", 1000)))
	text := strings.Repeat(`{"k":"0.75"}`, 100)
	views := []*String{
		big.Substring(0, 4), FromGoString("0.75"),
		{s: text[6:10], n: 4, kind: strASCII, flags: strSlice},
	}
	g := &ms.recentGate
	for _, v := range views {
		require.False(t, ownsUnits(v))
		ms.recent = [2]concatRecent{}
		x, _ := r.Concat(key, v)
		assert.Equal(t, "String for key 0.75", x.GoString())
		hits := g.hits
		for range 3 {
			y, _ := r.Concat(key, v)
			assert.Same(t, x, y)
		}
		assert.Equal(t, hits+3, g.hits)
		for _, c := range ms.recent {
			if c.s == x {
				assert.Equal(t, concatRecent{s: x, pa: unitsAddr(key), pb: unitsAddr(v)}, c)
			}
		}
		assert.Equal(t, 0.0, testing.AllocsPerRun(20, func() { r.Concat(key, v) }))
	}
	// An entry kept by addresses whose pieces' units differ, as when a new
	// piece reuses an address (Go's string constants share theirs), is not
	// returned.
	hv := FromGoString("0.25")
	forge := func() {
		ms.recent[0] = concatRecent{s: FromGoString("String for key 0.99"), pa: unitsAddr(key), pb: unitsAddr(hv)}
		ms.recent[1] = concatRecent{s: FromGoString("String for key 0.25 in leaf nodE"), pa: unitsAddr(s1), pb: unitsAddr(in)}
	}
	forge()
	x, _ = r.Concat(key, hv)
	assert.Equal(t, "String for key 0.25", x.GoString())
	forge()
	y, _ = r.Concat(s1, in)
	assert.Equal(t, "String for key 0.25 in leaf node", y.GoString())
	// A realm whose concatenations seldom repeat their pieces skips the
	// check, and keeps no recent results while it does.
	require.Zero(t, g.skip)
	for i := range 2 * memoWindow {
		r.Concat(key, FromGoString(fmt.Sprint(i%50)))
	}
	assert.NotZero(t, g.skip)
	ms.recent = [2]concatRecent{}
	for range 3 {
		r.Concat(key, tag)
	}
	assert.Equal(t, [2]concatRecent{}, ms.recent)
	g.skip = 0
	// The results and pieces the memo has dropped are still measured while
	// recent.
	r.SetMemoryLimit(1 << 30)
	clear(ms.concats[:])
	ms.recent = [2]concatRecent{{a: key, b: tag, s: s1}, {a: s1, b: in, s: s2}}
	live := r.MeasureLive(nil)
	ms.recent = [2]concatRecent{}
	assert.GreaterOrEqual(t, live-r.MeasureLive(nil), int64(3*stringHeaderSize+tag.Len()+s1.Len()+s2.Len()))
}

// TestConcatTo checks the register-writing concatenation with and without
// memo state, and that an oversized result leaves its destination alone.
func TestConcatTo(t *testing.T) {
	for _, memo := range []bool{false, true} {
		r := NewRealm()
		if memo {
			warmMemos(r)
		}
		for _, pieces := range [][2]string{
			{"", "x"}, {"x", ""}, {"", ""}, {"foo", "bar"},
			{"中", "文"}, {"x", "文"}, {"中", "x"},
			{strings.Repeat("a", ropeThreshold), "b"},
		} {
			for range 3 {
				dst := IntValue(42)
				err := r.concatTo(&dst, FromGoString(pieces[0]), FromGoString(pieces[1]))
				require.NoError(t, err)
				assert.Equal(t, pieces[0]+pieces[1], dst.String())
			}
		}
		dst := IntValue(42)
		err := r.concatTo(&dst, &String{n: MaxStringLength}, FromGoString("x"))
		require.Error(t, err)
		assert.Equal(t, IntValue(42), dst)
	}
}

// TestConcatMemoBound: the memo holds at most concatMemoSets*concatMemoWays
// results, each a flat ASCII string shorter than ropeThreshold that Concat
// made, keeping nothing but its own units and never appended to in place;
// UTF-16 and longer results are made anew each time.
func TestConcatMemoBound(t *testing.T) {
	r := NewRealm()
	warmMemos(r)
	for i := range 5000 {
		r.Concat(FromGoString("item-"), FromGoString(fmt.Sprint(i)))
	}
	require.NotNil(t, r.lazy.memos)
	held := 0
	for i := range &r.lazy.memos.concats {
		set := &r.lazy.memos.concats[i]
		for j, s := range &set.s {
			if s == nil {
				continue
			}
			var k [ropeThreshold]byte
			copy(k[:], s.s)
			assert.Equal(t, uint16(concatHash(&k, s.Len())>>16), set.tag[j], "the tag of %q", s.s)
			assert.Equal(t, i, int(concatHash(&k, s.Len())>>(64-concatMemoBits)), "the set of %q", s.s)
			held++
			assert.Equal(t, strASCII, s.kind)
			assert.Less(t, s.Len(), ropeThreshold)
			assert.Nil(t, s.right)
			assert.Zero(t, s.flags)
		}
	}
	// The first memoWindow misses went in, at ways the count of
	// misses picked; the rest skipped the memo, which hit none of them.
	assert.LessOrEqual(t, held, concatMemoSets*concatMemoWays)
	assert.Greater(t, held, concatMemoSets*concatMemoWays/2)
	a, b := FromGoString(strings.Repeat("a", 40)), FromGoString(strings.Repeat("b", 30))
	l1, _ := r.Concat(a, b)
	l2, _ := r.Concat(a, b)
	assert.NotSame(t, l1, l2)
	w, z := FromGoString("Grö"), FromGoString("ße")
	w1, _ := r.Concat(w, z)
	w2, _ := r.Concat(w, z)
	assert.NotSame(t, w1, w2)
	assert.Equal(t, "Größe", w1.GoString())
}

// TestConcatMemoLazy: a realm creates its memos, which hold the concat
// memo, after memoAfter concatenations they would take, and only with lazy
// state; the memory account is charged for them, and the live measurement
// counts them.
func TestConcatMemoLazy(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	r.Concat(FromGoString("fir"), FromGoString("st"))
	assert.True(t, r.lazy == nil || r.lazy.memos == nil)
	r.SetMemoryLimit(1 << 30)
	before := r.MeasureLive(nil)
	for i := range memoAfter {
		r.Concat(FromGoString("name"), FromGoString(fmt.Sprint(i)))
	}
	require.Nil(t, r.lazy.memos)
	charged := r.Stats().AllocatedBytes
	a, b := FromGoString("lat"), FromGoString("er")
	out, _ := r.Concat(a, b)
	require.NotNil(t, r.lazy.memos)
	tables := int64(unsafe.Sizeof(realmMemos{}))
	assert.GreaterOrEqual(t, r.Stats().AllocatedBytes-charged, tables)
	assert.GreaterOrEqual(t, r.MeasureLive(nil)-before, tables+int64(stringHeaderSize+5))
	again, _ := r.Concat(a, b)
	assert.Same(t, out, again)
	r.ReleaseCallData()
	again, _ = r.Concat(a, b)
	assert.Same(t, out, again, "ReleaseCallData leaves the memo")
}

// TestConcatMemoSkip: a realm whose concatenations hit the memo less than
// once in memoRate lookups skips it for memoSkip of them, then looks in it
// again; one that hits more keeps it.
func TestConcatMemoSkip(t *testing.T) {
	// window makes concatenations until the memo ends a window of
	// memoWindow misses: the first of a string it then repeats after every
	// k-1 others, each new, so that about one lookup in k hits (the string
	// repeated misses again when a new one replaces it).
	window := func(r *Realm, k int) {
		same := func() { r.Concat(FromGoString("same-"), FromGoString("key")) }
		same()
		g := &r.lazy.memos.concat
		for i := 1; g.misses != 0; i++ {
			r.Concat(FromGoString("id-"), FromGoString(fmt.Sprint(i)))
			if i%(k-1) == 0 && g.misses != 0 {
				same()
			}
		}
	}
	for _, c := range []struct {
		k    int
		skip bool
	}{{4, false}, {memoRate - 1, false}, {memoRate + 2, true}, {64, true}} {
		r := NewRealm()
		warmMemos(r)
		window(r, c.k)
		require.NotNil(t, r.lazy.memos)
		m := &r.lazy.memos.concat
		if !c.skip {
			assert.Zero(t, m.skip, "a hit in %d lookups keeps the memo", c.k)
			continue
		}
		assert.Equal(t, uint32(memoSkip), m.skip, "a hit in %d lookups skips the memo", c.k)
		// Skipped, the memo is not read: a result it holds is made anew,
		// as without a memo, and the hit is not counted.
		a, b := FromGoString("same-"), FromGoString("key")
		s1, _ := r.Concat(a, b)
		s2, _ := r.Concat(a, b)
		assert.NotSame(t, s1, s2)
		assert.Equal(t, "same-key", s1.GoString())
		assert.Equal(t, 1.0, testing.AllocsPerRun(50, func() { r.Concat(a, b) }))
		assert.Zero(t, m.hits+m.misses)
		// After memoSkip of them, it looks in the memo again.
		m.skip = 1
		r.Concat(a, b)
		s3, _ := r.Concat(a, b)
		s4, _ := r.Concat(a, b)
		assert.Same(t, s3, s4)
		assert.Equal(t, 0.0, testing.AllocsPerRun(50, func() { r.Concat(a, b) }))
	}
}

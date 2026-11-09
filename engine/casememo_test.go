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

// warmMemos sets the realm past memoAfter, after which it has memos
// (newMemos).
func warmMemos(r *Realm) {
	r.lazyState().memoCount = memoAfter
}

// TestCaseMemo converts random short ASCII strings, some many times and in
// both directions, as a component library converts its style and event
// names: each result is the conversion of its own string, and one already
// in the memo comes from it without an allocation.
func TestCaseMemo(t *testing.T) {
	r := NewRealm()
	warmMemos(r)
	rng := rand.New(rand.NewPCG(3, 4))
	const alphabet = "aAbBzZ-_09@[`{"
	var names []string
	for range 300 {
		b := make([]byte, 2+rng.IntN(caseMemoMax+4))
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		names = append(names, string(b))
	}
	for range 4000 {
		g := names[rng.IntN(len(names))]
		s := FromGoString(g)
		lo, err := stringToLower(r, s)
		require.NoError(t, err)
		up, err := stringToUpper(r, s)
		require.NoError(t, err)
		require.Equal(t, strings.ToLower(g), lo.GoString(), g)
		require.Equal(t, strings.ToUpper(g), up.GoString(), g)
		require.Equal(t, g, s.GoString(), "the source is untouched")
	}
	// A conversion in the memo comes from it, of a String with the same
	// content or the same String, and allocates nothing.
	a, b := FromGoString("onMouseEnter"), FromGoString("onMouseEnter")
	lo, _ := stringToLower(r, a)
	lo2, _ := stringToLower(r, b)
	assert.Same(t, lo, lo2)
	assert.Equal(t, 0.0, testing.AllocsPerRun(50, func() { stringToLower(r, a) }))
	assert.Equal(t, "onmouseenter", lo.GoString())
	// The other direction has its own result; a string already in the
	// case it converts to is returned as is.
	up, _ := stringToUpper(r, a)
	assert.Equal(t, "ONMOUSEENTER", up.GoString())
	up2, _ := stringToUpper(r, b)
	assert.Same(t, up, up2)
	same, _ := stringToLower(r, lo)
	assert.Same(t, lo, same)
	// Through the builtins, as a script converts.
	assert.Equal(t, "border-top-left-radius,BORDER-TOP-LEFT-RADIUS,border-top-left-radius",
		runScriptsIn(t, r, `var n = 'Border-Top-Left-Radius'; [n.toLowerCase(), n.toUpperCase(), n.toUpperCase().toLocaleLowerCase()].join()`))
}

// TestCaseMemoBound: the memo holds at most caseMemoSets*caseMemoWays
// results, each a flat ASCII string of at most caseMemoMax units that the
// conversions made, keeping nothing but its own units; longer strings and
// UTF-16 ones are converted anew each time.
func TestCaseMemoBound(t *testing.T) {
	r := NewRealm()
	warmMemos(r)
	for i := range 5000 {
		s := FromGoString(fmt.Sprintf("Name%dX", i))
		stringToLower(r, s)
		stringToUpper(r, s)
	}
	require.NotNil(t, r.lazy.memos.cases)
	held := 0
	for i := range r.lazy.memos.cases {
		for _, s := range &r.lazy.memos.cases[i] {
			require.NotNil(t, s, "5000 results fill every way")
			held++
			assert.Equal(t, strASCII, s.kind)
			assert.LessOrEqual(t, s.Len(), caseMemoMax)
			assert.Nil(t, s.right)
			assert.Zero(t, s.flags)
		}
	}
	assert.Equal(t, caseMemoSets*caseMemoWays, held)
	long := FromGoString(strings.Repeat("Ab", caseMemoMax/2) + "C")
	l1, _ := stringToLower(r, long)
	l2, _ := stringToLower(r, long)
	assert.NotSame(t, l1, l2)
	assert.Equal(t, strings.ToLower(long.GoString()), l1.GoString())
	wide := FromGoString("Größe")
	w1, _ := stringToUpper(r, wide)
	w2, _ := stringToUpper(r, wide)
	assert.NotSame(t, w1, w2)
	assert.Equal(t, "GRÖSSE", w1.GoString())
}

// TestCaseMemoLazy: a realm creates its memos after memoAfter conversions
// they would take, and only with lazy state; the memory account is charged
// for the memos and the case memo, and the live measurement counts them.
func TestCaseMemoLazy(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	stringToLower(r, FromGoString("First"))
	assert.True(t, r.lazy == nil || r.lazy.memos == nil)
	r.SetMemoryLimit(1 << 30)
	before := r.MeasureLive(nil)
	for i := range memoAfter {
		stringToLower(r, FromGoString(fmt.Sprint("Name", i)))
	}
	require.Nil(t, r.lazy.memos)
	charged := r.Stats().AllocatedBytes
	s := FromGoString("Later")
	out, _ := stringToLower(r, s)
	require.NotNil(t, r.lazy.memos)
	require.NotNil(t, r.lazy.memos.cases)
	tables := int64(unsafe.Sizeof(caseMemo{}) + unsafe.Sizeof(realmMemos{}))
	assert.GreaterOrEqual(t, r.Stats().AllocatedBytes-charged, tables)
	assert.GreaterOrEqual(t, r.MeasureLive(nil)-before, tables+int64(stringHeaderSize+5))
	again, _ := stringToLower(r, s)
	assert.Same(t, out, again)
	r.ReleaseCallData()
	again, _ = stringToLower(r, s)
	assert.Same(t, out, again, "ReleaseCallData leaves the memo")
}

// TestASCIIToCase checks the scan and the conversion of ASCII bytes eight
// at a time against a byte at a time, over every byte value at every
// offset.
func TestASCIIToCase(t *testing.T) {
	var all []byte
	for c := range 0x80 {
		all = append(all, byte(c))
	}
	for off := range 9 {
		for _, from := range []byte{'A', 'a'} {
			b := append([]byte(nil), all[off:]...)
			asciiToCase(b, from)
			want := strings.ToLower(string(all[off:]))
			if from == 'a' {
				want = strings.ToUpper(string(all[off:]))
			}
			require.Equal(t, want, string(b), "from %c offset %d", from, off)
			for _, c := range all[off:] {
				one := strings.Repeat(".", off) + string(c) + "z"
				want := off
				if c-from >= 26 {
					want = off + 1
					if from == 'A' {
						want = off + 2
					}
				}
				require.Equal(t, want, asciiCaseIndex(one, from), "%q from %c", one, from)
			}
		}
	}
}

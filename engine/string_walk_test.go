package engine

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// walkRope builds a rope of the leaves, joined in a shape chosen by rng: a
// balanced tree, a left-deep chain or a right-deep one, with some nodes
// flattened first, so that a walk meets published contents in the middle.
func walkRope(rng *rand.Rand, leaves []*String) *String {
	var build func(l []*String) *String
	build = func(l []*String) *String {
		if len(l) == 1 {
			return l[0]
		}
		k := 1 + rng.IntN(len(l)-1)
		switch rng.IntN(3) {
		case 0:
			k = len(l) - 1 // left-deep
		case 1:
			k = 1 // right-deep
		}
		a, b := build(l[:k]), build(l[k:])
		s := &String{p: unsafe.Pointer(a), right: b, n: a.n + b.n, kind: strRope}
		if rng.IntN(8) == 0 {
			s.flatten()
		}
		return s
	}
	return build(leaves)
}

// TestRopeWalk checks the reads that walk a rope's leaves, IndexOf,
// hasSubstringAt and GoString, against the flat string over random ropes of
// ASCII and UTF-16 leaves with surrogate pairs split across leaves, lone
// surrogates and search strings that span leaves.
func TestRopeWalk(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	alphabet := []rune{'a', 'b', 'c', 'é', 'ü', '😀', 0xD800, 0xDC00}
	piece := func(n int, wide bool) []uint16 {
		var u []uint16
		for len(u) < n {
			c := alphabet[rng.IntN(3)]
			if wide && rng.IntN(3) == 0 {
				c = alphabet[3+rng.IntN(len(alphabet)-3)]
			}
			if c >= 0x10000 {
				u = utf16.AppendRune(u, c)
			} else {
				u = append(u, uint16(c))
			}
		}
		return u
	}
	for round := range 400 {
		nl := 1 + rng.IntN(40)
		if round%10 == 0 {
			nl = 70 // past the walk's depth for a chain
		}
		var leaves []*String
		var all []uint16
		for range nl {
			u := piece(rng.IntN(12), rng.IntN(3) == 0)
			if len(u) == 0 {
				u = []uint16{'a'}
			}
			leaves = append(leaves, FromUTF16(append([]uint16(nil), u...)))
			all = append(all, u...)
		}
		flat := FromUTF16(all)
		want := flat.GoString()
		for range 20 {
			s := walkRope(rng, leaves)
			if s.kind != strRope {
				continue
			}
			from := rng.IntN(len(all) + 1)
			a := rng.IntN(len(all))
			b := a + 1 + rng.IntN(min(len(all)-a, 70))
			sub := FromUTF16(append([]uint16(nil), all[a:b]...))
			miss := FromUTF16(append(append([]uint16(nil), all[a:b]...), 'z'))
			for _, x := range []*String{sub, miss} {
				require.Equal(t, flat.IndexOf(x, from), s.IndexOf(x, from), "round %d from %d", round, from)
				require.Equal(t, hasSubstringAt(flat, x, a), hasSubstringAt(s, x, a))
				require.Equal(t, hasSubstringAt(flat, x, from), hasSubstringAt(s, x, from))
			}
			require.Equal(t, want, s.GoString())
		}
	}
}

// TestRopeWalkNoCopy checks that includes, startsWith and the conversion to
// a Go string of a long rendered page, a rope of a few UTF-16 and ASCII
// pieces, do not flatten it.
func TestRopeWalkNoCopy(t *testing.T) {
	r := NewRealm()
	head := FromGoString("<!doctype html><html><head>" + strings.Repeat("<meta x>", 200))
	body := FromGoString("<body>" + strings.Repeat("<p>é</p>", 2000) + "</body>")
	tail := FromGoString(strings.Repeat("<script>x</script>", 50) + "</html>")
	page := concat(concat(head, body), tail)
	require.True(t, isRopeForTest(page))
	want := head.GoString() + body.GoString() + tail.GoString()
	across, prefix, suffix := FromGoString("</p></body><script>"), FromGoString("<!doctype"), FromGoString("</html>")
	for _, c := range []struct {
		name string
		read func()
	}{
		{"includes", func() { page.IndexOf(across, 0) }},
		{"startsWith", func() { hasSubstringAt(page, prefix, 0) }},
		{"endsWith", func() { hasSubstringAt(page, suffix, page.Len()-7) }},
	} {
		assert.Equal(t, 0.0, testing.AllocsPerRun(20, c.read), c.name)
	}
	assert.Equal(t, 1.0, testing.AllocsPerRun(20, func() { page.GoString() }))
	_, published := page.published()
	assert.False(t, published)
	assert.Equal(t, want, page.GoString())
	// The same reads from JS, against the flat page.
	read := evalValue(t, r, "(s) => [s.includes('</p></body><script>'), s.includes('</p></body><scripx'), s.startsWith('<!doctype'), s.endsWith('</html>'), s.indexOf('\u00e9</p></body>')].join()")
	got, err := r.Call(read, Undefined(), []Value{StringValue(concat(concat(head, body), tail))})
	require.NoError(t, err)
	flat, err := r.Call(read, Undefined(), []Value{StringValue(FromGoString(want))})
	require.NoError(t, err)
	assert.Equal(t, jsString(t, flat), jsString(t, got))
	assert.True(t, strings.HasPrefix(jsString(t, got), "true,false,true,true,"))
}

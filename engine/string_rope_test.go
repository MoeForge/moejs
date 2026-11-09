package engine

import (
	"math/rand/v2"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ropeRead is one read of a rope that needs its units, checked against the
// string's Go form; r is the reading goroutine's own realm.
type ropeRead struct {
	name string
	read func(r *Realm, s *String, want string) bool
}

var ropeReads = []ropeRead{
	{"At", func(r *Realm, s *String, want string) bool {
		u := FromGoString(want)
		return s.At(0) == u.At(0) && s.At(s.Len()/2) == u.At(s.Len()/2) && s.At(s.Len()-1) == u.At(s.Len()-1)
	}},
	{"GoString", func(r *Realm, s *String, want string) bool { return s.GoString() == want }},
	{"Equals", func(r *Realm, s *String, want string) bool {
		w := FromGoString(want)
		return s.Equals(w) && w.Equals(s) && s.EqualsGoString(want) && s.Compare(w) == 0
	}},
	{"Hash", func(r *Realm, s *String, want string) bool { return s.Hash() == FromGoString(want).Hash() }},
	{"Intern", func(r *Realm, s *String, want string) bool {
		return r.Intern(s) == r.Intern(FromGoString(want)) && r.KeyFromString(s) == r.KeyFromGoString(want)
	}},
	{"Concat", func(r *Realm, s *String, want string) bool {
		x, err := r.Concat(s, FromGoString("!"))
		y, err2 := r.Concat(FromGoString("<"+strings.Repeat("-", 100)), s)
		return err == nil && err2 == nil && x.GoString() == want+"!" && y.GoString() == "<"+strings.Repeat("-", 100)+want
	}},
	{"Substring", func(r *Realm, s *String, want string) bool {
		w := FromGoString(want)
		return s.Substring(1, s.Len()-1).Equals(w.Substring(1, w.Len()-1)) && s.IndexOf(w.Substring(w.Len()-3, w.Len()), w.Len()-3) == w.Len()-3
	}},
	{"Search", func(r *Realm, s *String, want string) bool {
		// Around the middle, near where two leaves meet in most cases' ropes.
		w := FromGoString(want)
		mid := w.Substring(w.Len()/2-5, w.Len()/2+5)
		return s.IndexOf(mid, 0) == w.IndexOf(mid, 0) && hasSubstringAt(s, mid, w.Len()/2-5) && !hasSubstringAt(s, FromGoString("\x00"), 0)
	}},
	{"Units", func(r *Realm, s *String, want string) bool {
		a, ok := s.ASCII()
		return s.IsASCII() == isASCII(want) && ok == isASCII(want) && (!ok || a == want) &&
			FromUTF16(s.UTF16()).GoString() == want && s.IsWellFormed()
	}},
	{"JSON", func(r *Realm, s *String, want string) bool {
		b, ok, err := r.AppendJSON(nil, StringValue(s))
		w, _, _ := r.AppendJSON(nil, StringValue(FromGoString(want)))
		return err == nil && ok && string(b) == string(w)
	}},
}

func isRopeForTest(s *String) bool { return s.kind == strRope }

// ropeCase builds fresh ropes for one round: the strings the goroutines read
// and their Go forms.
type ropeCase struct {
	name  string
	build func() ([]*String, []string)
}

func ropeCases() []ropeCase {
	pair := func(a, b string) func() ([]*String, []string) {
		return func() ([]*String, []string) {
			return []*String{concat(FromGoString(a), FromGoString(b))}, []string{a + b}
		}
	}
	return []ropeCase{
		{"ascii short", pair(strings.Repeat("a", 30), strings.Repeat("b", 40))},
		{"ascii long", pair(strings.Repeat("x", 1500), strings.Repeat("y", 2600))},
		{"utf16 short", pair(strings.Repeat("é", 30), strings.Repeat("c", 40))},
		{"utf16 long", pair(strings.Repeat("p", 1500), strings.Repeat("q😀", 900))},
		// A DAG of ropes: each goroutine reads one node, so flattening an
		// outer one walks inner ones others flatten at the same time.
		{"nested", func() ([]*String, []string) {
			a, b, c, d := strings.Repeat("A", 50), strings.Repeat("ü", 60), strings.Repeat("C", 30), strings.Repeat("D", appendPieceMax)
			r1 := concat(FromGoString(a), FromGoString(b)) // prepend
			r2 := concat(r1, FromGoString(c))              // append to a rope
			r3 := concat(FromGoString(d[:20]), r2)         // prepend to a rope
			r4 := concat(r3, r1)                           // r1 twice
			r5 := concat(FromGoString(d), FromGoString(d)) // join of big ASCII pieces
			r6 := concat(r5, r5)
			return []*String{r1, r2, r3, r4, r5, r6}, []string{a + b, a + b + c, d[:20] + a + b + c, d[:20] + a + b + c + a + b, d + d, d + d + d + d}
		}},
	}
}

// TestRopeConcurrentFlatten reads the same ropes from several goroutines at
// once, each with its own realm, as strings a host keeps may be: every read
// that needs a rope's units flattens it, the first one to finish publishes
// its copy, and every goroutine sees the whole string. The race detector
// checks the publication; without it, a reader could follow a child another
// goroutine had dropped, or see the new kind before the units.
func TestRopeConcurrentFlatten(t *testing.T) {
	const goroutines = 6
	rounds := 150
	if testing.Short() {
		rounds = 30
	}
	realms := make([]*Realm, goroutines)
	for i := range realms {
		realms[i] = NewRealm()
	}
	for _, c := range ropeCases() {
		t.Run(c.name, func(t *testing.T) {
			for round := range rounds {
				strs, wants := c.build()
				for _, s := range strs {
					require.True(t, isRopeForTest(s), "%s: a rope", c.name)
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				for g := range goroutines {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						i := (g + round) % len(strs)
						op := ropeReads[(g+round)%len(ropeReads)]
						assert.True(t, op.read(realms[g], strs[i], wants[i]), "%s round %d: %s of string %d", c.name, round, op.name, i)
					}()
				}
				close(start)
				wg.Wait()
				for i, s := range strs {
					assert.Equal(t, wants[i], s.GoString())
				}
			}
		})
	}
}

// TestRopeReadNoAlloc: reading a flattened rope allocates nothing. Each
// read takes a view of the published contents in storage on its own stack
// (flat), which a read that lets the view reach a method with a rope branch
// would move to the heap.
func TestRopeReadNoAlloc(t *testing.T) {
	r := NewRealm()
	for _, tail := range []string{"b", "é"} {
		want := strings.Repeat("a", 30) + strings.Repeat(tail, 100)
		s := concat(FromGoString(strings.Repeat("a", 30)), FromGoString(strings.Repeat(tail, 100)))
		require.True(t, isRopeForTest(s))
		w, sub := FromGoString(want), FromGoString(strings.Repeat(tail, 3))
		s.flatten()
		r.KeyFromString(s) // the realm caches the atom
		for name, read := range map[string]func(){
			"At":             func() { s.At(70) },
			"IsASCII":        func() { s.IsASCII() },
			"ASCII":          func() { s.ASCII() },
			"UTF16":          func() { s.UTF16() },
			"Equals":         func() { s.Equals(w); w.Equals(s) },
			"EqualsGoString": func() { s.EqualsGoString(want) },
			"Compare":        func() { s.Compare(w) },
			"Hash":           func() { s.Hash() },
			"IndexOf":        func() { s.IndexOf(sub, 0) },
			"lastIndexOf":    func() { lastIndexOfString(s, sub, s.Len()) },
			"Substring":      func() { s.Substring(0, s.Len()) },
			"IsWellFormed":   func() { s.IsWellFormed() },
			"KeyFromString":  func() { r.KeyFromString(s) },
			"collHash":       func() { collHash(StringValue(s)) },
		} {
			switch {
			case name == "UTF16" && tail == "b":
				continue // an ASCII string's units are a copy
			case (name == "EqualsGoString" || name == "KeyFromString") && tail == "é":
				continue // a flat UTF-16 string allocates its UTF-8 form, its key
			}
			assert.Equal(t, 0.0, testing.AllocsPerRun(50, read), "%s %q", name, tail)
		}
		assert.Equal(t, want, s.GoString())
	}
}

var fromUTF16Sink *String

// TestFromUTF16ASCIIOneAlloc: an all-ASCII string made of units, as a
// slice of a UTF-16 string can be, from fromUTF16Joint to smallASCIIMax
// units long takes one allocation, its header and bytes together, of the
// bytes the two would take; a shorter one takes two, its bytes the tiny
// allocator's.
func TestFromUTF16ASCIIOneAlloc(t *testing.T) {
	for _, n := range []int{1, 5, 15, 16, 24, fromUTF16Joint, 32, 33, 48, 49, smallASCIIMax, smallASCIIMax + 1} {
		u := make([]uint16, n)
		for i := range u {
			u[i] = uint16('a' + i%26)
		}
		want := 1.0
		if n < fromUTF16Joint || n > smallASCIIMax {
			want = 2
		}
		assert.Equal(t, want, testing.AllocsPerRun(50, func() { fromUTF16Sink = FromUTF16(u) }), "%d units", n)
		assert.Equal(t, strASCII, fromUTF16Sink.kind)
		assert.Equal(t, n, fromUTF16Sink.Len())
		assert.Equal(t, byte('a'+(n-1)%26), fromUTF16Sink.GoString()[n-1])
	}
}

// TestRopeSliceChild: a slice of a rope within one of its children, as far
// as ropeSliceDepth levels down, is the child's substring and leaves the
// rope unflattened; one across children is as before. Every slice holds
// the rope's units at its range.
func TestRopeSliceChild(t *testing.T) {
	a, b, c := strings.Repeat("a", 80)+"Q", strings.Repeat("é", 90), strings.Repeat("c", 70)
	ac, bc, cc := FromGoString(a), FromGoString(b), FromGoString(c)
	abc := concat(concat(ac, bc), cc)
	want := []rune(a + b + c) // one unit each
	for _, rg := range [][2]int{{1, 81}, {0, 81}, {81, 171}, {85, 100}, {171, 241}, {200, 241}, {80, 82}, {10, 200}, {0, 241}, {170, 172}} {
		assert.Equal(t, string(want[rg[0]:rg[1]]), abc.Substring(rg[0], rg[1]).GoString(), "%v", rg)
	}
	abc = concat(concat(ac, bc), cc)
	l, _ := abc.children()
	require.NotNil(t, l)
	s := abc.Substring(1, 81)
	assert.Equal(t, a[1:], s.GoString())
	assert.Same(t, ac, s.right, "the child's substring shares the child")
	assert.Same(t, cc, abc.Substring(171, 241), "a whole child is the child")
	assert.Same(t, bc, abc.Substring(81, 171))
	l2, _ := abc.children()
	assert.NotNil(t, l2, "slices within a child leave the rope unflattened")
	_, ll := l.children()
	assert.NotNil(t, ll)
}

// TestRopeSliceDepth: ropeSubstring reaches a child ropeSliceDepth levels
// down, here the leaf of a left-deep chain at that depth, which a covering
// slice returns and a slice within shares; a leaf one level deeper is read
// from the flat copy of the node at that depth, the rope above it left as
// it was.
func TestRopeSliceDepth(t *testing.T) {
	const n = ropeSliceDepth + 4
	var leaves []*String
	var want strings.Builder
	for i := range n {
		g := strings.Repeat(string(rune('a'+i)), 100)
		leaves = append(leaves, FromGoString(g))
		want.WriteString(g)
	}
	// nodes[d] is the node at depth d: leaves 0 to n-1-d.
	nodes := make([]*String, n)
	s := leaves[0]
	for i := 1; i < n; i++ {
		s = ropeOf(s, leaves[i])
		nodes[n-1-i] = s
	}
	root := nodes[0]
	deep := n - ropeSliceDepth // the leaf at depth ropeSliceDepth
	at := 100 * deep
	assert.Same(t, leaves[deep], root.Substring(at, at+100))
	sub := root.Substring(at+10, at+90)
	assert.Same(t, leaves[deep], sub.right)
	assert.Equal(t, want.String()[at+10:at+90], sub.GoString())
	at -= 100 // one level deeper
	sub = root.Substring(at+10, at+90)
	assert.Equal(t, want.String()[at+10:at+90], sub.GoString())
	assert.Same(t, nodes[ropeSliceDepth], sub.right, "the flat copy of the node at the depth")
	_, published := nodes[ropeSliceDepth].published()
	assert.True(t, published)
	for d := range ropeSliceDepth {
		_, published := nodes[d].published()
		assert.False(t, published, "depth %d", d)
	}
	assert.Equal(t, want.String(), root.GoString())
}

// TestRopeSliceOwnership: a range within a child that shares its units (a
// substring, an append buffer's string, a host string) is read from a flat
// copy of the rope node that holds it, and the slice keeps nothing of the
// shared root; a range covering such a child is the child.
func TestRopeSliceOwnership(t *testing.T) {
	r := NewRealm()
	big := FromGoString(strings.Repeat("B", 1<<16))
	sub := big.Substring(0, 100)
	host := &String{s: strings.Repeat("H", 100), n: 100, kind: strASCII, flags: strHost}
	buf, err := r.Concat(FromGoString(strings.Repeat("p", 70)), FromGoString("q"))
	require.NoError(t, err)
	require.NotZero(t, buf.flags&strInBuf)
	for _, c := range []*String{sub, host, buf} {
		mk := func() *String {
			return ropeOf(ropeOf(FromGoString(strings.Repeat("x", 64)), c), FromGoString(strings.Repeat("y", 64)))
		}
		assert.Same(t, c, mk().Substring(64, 64+int(c.n)), "a covering range is the child")
		rope := mk()
		part := rope.Substring(64+2, 64+10)
		assert.Equal(t, c.GoString()[2:10], part.GoString())
		assert.NotSame(t, c, part.right)
		if part.right != nil {
			l, _ := part.right.children()
			assert.Nil(t, l)
			assert.Nil(t, part.right.right, "the flat copy shares no root")
		}
		l, _ := rope.children()
		require.NotNil(t, l, "the root keeps its children")
		li, _ := l.children()
		assert.Nil(t, li, "the node holding the shared child is flattened")
	}
}

// TestRopeSliceRandom slices random ropes of mixed children (owned ASCII
// and UTF-16, substrings, append buffers' strings, flattened nodes, short
// leaves) at random and at child boundaries: each slice reads the units of
// the range, and one that keeps a rope keeps it flattened.
func TestRopeSliceRandom(t *testing.T) {
	r := NewRealm()
	rng := rand.New(rand.NewPCG(1, 2))
	big := FromGoString(strings.Repeat("B", 4000))
	bigU := FromGoString(strings.Repeat("Ü", 4000))
	leaf := func() (*String, string) {
		n := 1 + rng.IntN(120)
		switch rng.IntN(7) {
		case 0:
			g := strings.Repeat(string(rune('a'+rng.IntN(26))), n)
			return FromGoString(g), g
		case 1:
			g := strings.Repeat("é", n)
			return FromGoString(g), g
		case 2:
			return big.Substring(3, 3+n), strings.Repeat("B", n)
		case 3:
			return bigU.Substring(3, 3+n), strings.Repeat("Ü", n)
		case 4: // an append buffer's string: b.n <= a.n
			n = min(n, 70)
			s, err := r.Concat(FromGoString(strings.Repeat("p", 70)), FromGoString(strings.Repeat("q", n)))
			require.NoError(t, err)
			require.NotZero(t, s.flags&strInBuf)
			return s, strings.Repeat("p", 70) + strings.Repeat("q", n)
		case 5:
			g := strings.Repeat("m", n) + "ü"
			return FromGoString(g), g
		default:
			g := strings.Repeat("s", 1+rng.IntN(5))
			return FromGoString(g), g
		}
	}
	var build func(depth int) (*String, string)
	build = func(depth int) (*String, string) {
		if depth == 0 || rng.IntN(4) == 0 {
			return leaf()
		}
		l, lg := build(depth - 1)
		rt, rg := build(depth - 1)
		s := ropeOf(l, rt)
		if rng.IntN(3) == 0 {
			s = concat(l, rt)
		}
		if s.kind == strRope && rng.IntN(6) == 0 {
			s.flatten()
		}
		return s, lg + rg
	}
	for iter := range 300 {
		s, g := build(1 + rng.IntN(7))
		want := []rune(g) // one unit each
		n := len(want)
		require.Equal(t, n, s.Len())
		for k := range 40 {
			a, b := rng.IntN(n+1), rng.IntN(n+1)
			if a > b {
				a, b = b, a
			}
			if k%5 == 0 && s.kind == strRope {
				if l, _ := s.children(); l != nil {
					m := int(l.n)
					switch k % 15 {
					case 0:
						a, b = m, min(n, m+1+rng.IntN(3))
					case 5:
						a, b = max(0, m-1-rng.IntN(3)), m
					default:
						a, b = max(0, m-1), min(n, m+1)
					}
				}
			}
			sub := s.Substring(a, b)
			require.Equal(t, string(want[a:b]), sub.GoString(), "iter %d slice [%d,%d)", iter, a, b)
			require.Equal(t, b-a, sub.Len())
			if sub.right != nil && sub.right.kind == strRope {
				l, _ := sub.right.children()
				assert.Nil(t, l, "a slice keeps a rope only flattened")
			}
		}
		require.Equal(t, g, s.GoString(), "iter %d", iter)
	}
}

package engine

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFromGoStringKinds(t *testing.T) {
	a := FromGoString("hello world, this is ascii text with 40+ bytes")
	assert.Equal(t, strASCII, a.kind)
	assert.True(t, a.IsASCII())
	assert.Equal(t, 46, a.Len())
	assert.Equal(t, uint16('h'), a.At(0))

	u := FromGoString("héllo")
	assert.Equal(t, strUTF16, u.kind)
	assert.Equal(t, 5, u.Len())
	assert.Equal(t, uint16(0xE9), u.At(1))

	e := FromGoString("😀")
	assert.Equal(t, 2, e.Len())
	assert.Equal(t, uint16(0xD83D), e.At(0))
	assert.Equal(t, uint16(0xDE00), e.At(1))
	assert.Equal(t, "😀", e.GoString())

	assert.Same(t, emptyString, FromGoString(""))
	// Invalid UTF-8 becomes U+FFFD, like Go's range loop.
	bad := FromGoString("a\xffb")
	assert.Equal(t, 3, bad.Len())
	assert.Equal(t, uint16(0xFFFD), bad.At(1))
}

func TestIsASCIISWAR(t *testing.T) {
	for n := range 40 {
		s := strings.Repeat("a", n)
		assert.True(t, isASCII(s), n)
		for pos := range n {
			b := []byte(s)
			b[pos] = 0x80
			assert.False(t, isASCII(string(b)), "n=%d pos=%d", n, pos)
			b[pos] = 0xFF
			assert.False(t, isASCII(string(b)), "n=%d pos=%d", n, pos)
		}
	}
}

func TestGoStringZeroCopy(t *testing.T) {
	g := "zero copy ascii"
	s := FromGoString(g)
	assert.Equal(t, g, s.GoString())
	got, ok := s.ASCII()
	assert.True(t, ok)
	assert.Equal(t, g, got)
}

func TestGoStringLoneSurrogates(t *testing.T) {
	cases := []struct {
		name string
		u    []uint16
		want string
	}{
		{"lone high", []uint16{'a', 0xD800, 'b'}, "a�b"},
		{"lone low", []uint16{0xDC00}, "�"},
		{"pair", []uint16{0xD83D, 0xDE00}, "😀"},
		{"high at end", []uint16{'x', 0xD83D}, "x�"},
		{"reversed pair", []uint16{0xDE00, 0xD83D}, "��"},
		{"bmp", []uint16{0x4E2D, 0x6587}, "中文"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, FromUTF16(c.u).GoString())
		})
	}
}

func TestFromUTF16NormalizesASCII(t *testing.T) {
	s := FromUTF16([]uint16{'a', 'b', 'c'})
	assert.Equal(t, strASCII, s.kind)
	assert.Equal(t, "abc", s.GoString())
	assert.True(t, s.Equals(FromGoString("abc")))
	assert.Equal(t, FromGoString("abc").Hash(), s.Hash())
}

func TestConcatAndRope(t *testing.T) {
	a := FromGoString("short")
	b := FromGoString("er")
	c := concat(a, b)
	assert.Equal(t, strASCII, c.kind, "short concat is eager")
	assert.Equal(t, "shorter", c.GoString())

	// A long result whose right piece is the longer is a rope (a prepend).
	long1 := FromGoString(strings.Repeat("x", 30))
	long2 := FromGoString(strings.Repeat("y", 40))
	rope := concat(long1, long2)
	require.Equal(t, strRope, rope.kind)
	assert.Equal(t, 70, rope.Len())
	// At flattens it: the rope publishes its contents and stays a rope.
	assert.Equal(t, uint16('y'), rope.At(45))
	assert.Equal(t, strRope, rope.kind)
	assert.Equal(t, strASCII, flatKind(t, rope))
	assert.Equal(t, strings.Repeat("x", 30)+strings.Repeat("y", 40), rope.s, "an ASCII rope's s is its contents")
	assert.Nil(t, rope.p, "the children are released")
	assert.Nil(t, rope.right)
	assert.Equal(t, strings.Repeat("x", 30)+strings.Repeat("y", 40), rope.GoString())
	// Flatten is idempotent.
	d := rope.s
	rope.flatten()
	assert.Equal(t, strASCII, flatKind(t, rope))
	assert.Equal(t, unsafe.StringData(d), unsafe.StringData(rope.s), "published once")
	assert.Equal(t, 70, rope.Len())

	// Mixed kinds flatten to utf16.
	mixed := concat(long1, FromGoString(strings.Repeat("é", 40)))
	require.Equal(t, strRope, mixed.kind)
	assert.Equal(t, uint16(0xE9), mixed.At(50))
	assert.Equal(t, strUTF16, flatKind(t, mixed))
	assert.Equal(t, "", mixed.s)

	// Empty operands are returned as-is.
	assert.Same(t, a, concat(a, emptyString))
	assert.Same(t, a, concat(emptyString, a))
}

func TestDeepRopeFlatten(t *testing.T) {
	// Left-deep rope like `s += "ab"` in a loop made before append
	// buffers; must not recurse deeply.
	s := FromGoString(strings.Repeat("a", 64))
	for range 20000 {
		s = ropeOf(s, FromGoString("ab"))
	}
	assert.Equal(t, 64+40000, s.Len())
	assert.Equal(t, uint16('b'), s.At(64+1))
	assert.Equal(t, strASCII, flatKind(t, s))
}

// flatKind is the kind of the contents the rope s published.
func flatKind(t *testing.T, s *String) uint8 {
	t.Helper()
	require.Equal(t, strRope, s.kind)
	f, ok := s.published()
	require.True(t, ok, "published")
	return f.kind
}

func TestSubstringAcrossKinds(t *testing.T) {
	a := FromGoString("hello world")
	sub := a.Substring(6, 11)
	assert.Equal(t, "world", sub.GoString())
	assert.Equal(t, strASCII, sub.kind)
	assert.Same(t, a, a.Substring(0, 11))
	assert.Same(t, emptyString, a.Substring(3, 3))

	u := FromGoString("héllo wörld")
	assert.Equal(t, "wörld", u.Substring(6, 11).GoString())
	ascPart := u.Substring(2, 5)
	assert.Equal(t, strASCII, ascPart.kind, "ascii-only substring of utf16 normalizes")
	assert.Equal(t, "llo", ascPart.GoString())

	rope := ropeOf(FromGoString(strings.Repeat("a", 40)), FromGoString(strings.Repeat("b", 40)))
	assert.Equal(t, "ab", rope.Substring(39, 41).GoString())
}

func TestCompareAndEquals(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"a", "b", -1},
		{"b", "a", 1},
		{"abc", "abc", 0},
		{"ab", "abc", -1},
		{"abc", "ab", 1},
		{"", "a", -1},
		{"é", "z", 1},       // 0xE9 > 'z'
		{"😀", "\uFFFF", -1}, // surrogate 0xD83D < 0xFFFF (code unit order, not code point)
		{"a😀", "aé", 1},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, FromGoString(c.a).Compare(FromGoString(c.b)), "%q vs %q", c.a, c.b)
		assert.Equal(t, c.want == 0, FromGoString(c.a).Equals(FromGoString(c.b)))
	}
	assert.True(t, FromGoString("héllo").EqualsGoString("héllo"))
	assert.False(t, FromGoString("hello").EqualsGoString("héllo"))
	assert.Equal(t, 0, FromGoString("é").Compare(FromUTF16([]uint16{0xE9})))
}

func TestIndexOf(t *testing.T) {
	s := FromGoString("hello hello")
	sub := FromGoString("llo")
	assert.Equal(t, 2, s.IndexOf(sub, 0))
	assert.Equal(t, 8, s.IndexOf(sub, 3))
	assert.Equal(t, -1, s.IndexOf(sub, 9))
	assert.Equal(t, 3, s.IndexOf(emptyString, 3))
	assert.Equal(t, 11, s.IndexOf(emptyString, 50))
	u := FromGoString("héllo héllo")
	assert.Equal(t, 7, u.IndexOf(FromGoString("éll"), 2))
	assert.Equal(t, -1, FromGoString("abc").IndexOf(FromGoString("é"), 0))
}

func TestStringBuilder(t *testing.T) {
	var sb StringBuilder
	sb.WriteGoString("abc")
	sb.WriteASCII('d')
	sb.WriteUnit('e')
	assert.Equal(t, 5, sb.Len())
	s := sb.String()
	assert.Equal(t, strASCII, s.kind)
	assert.Equal(t, "abcde", s.GoString())

	sb.Reset()
	sb.WriteGoString("abc")
	sb.WriteRune('é')
	sb.WriteRune('😀')
	sb.WriteString(FromGoString("xyz"))
	sb.WriteString(FromGoString("ü"))
	assert.Equal(t, 10, sb.Len())
	u := sb.String()
	assert.Equal(t, strUTF16, u.kind)
	assert.Equal(t, "abcé😀xyzü", u.GoString())

	var empty StringBuilder
	assert.Same(t, emptyString, empty.String())

	var grow StringBuilder
	grow.Grow(100)
	grow.WriteGoString("x")
	assert.Equal(t, "x", grow.String().GoString())
}

// TestStringBuilderWrites writes every kind of piece after prefixes of
// every length around the inline bytes and the first heap capacities, so
// that each write meets each store (inline, heap ASCII with and without
// room, UTF-16) and the moves between them, and checks the result against
// the code units written. The sizes of the builder and of the structs that
// embed it are part of their size classes.
func TestStringBuilderWrites(t *testing.T) {
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(120), unsafe.Sizeof(StringBuilder{}))
		assert.Equal(t, uintptr(224), unsafe.Sizeof(jsonParser{}))
		assert.Equal(t, uintptr(360), unsafe.Sizeof(jsonStringifier{}))
	}
	filler := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789", 8)
	wide := FromGoString("éü中")
	pieces := []struct {
		name  string
		write func(*StringBuilder)
		units []uint16
	}{
		{"bytes", func(sb *StringBuilder) { sb.writeASCIIBytes("xyz") }, []uint16{'x', 'y', 'z'}},
		{"no bytes", func(sb *StringBuilder) { sb.writeASCIIBytes("") }, nil},
		{"long bytes", func(sb *StringBuilder) { sb.writeASCIIBytes(filler[:70]) }, utf16.Encode([]rune(filler[:70]))},
		{"byte", func(sb *StringBuilder) { sb.WriteASCII('q') }, []uint16{'q'}},
		{"two bytes", func(sb *StringBuilder) { sb.writeASCII2('\\', 'n') }, []uint16{'\\', 'n'}},
		{"units", func(sb *StringBuilder) { sb.WriteUTF16(wide.units()) }, wide.units()},
		{"string", func(sb *StringBuilder) { sb.WriteString(wide) }, wide.units()},
		{"ASCII string", func(sb *StringBuilder) { sb.WriteString(FromGoString(filler[:9])) }, utf16.Encode([]rune(filler[:9]))},
	}
	for n := 0; n <= 2*smallASCIIMax+20; n++ {
		for _, first := range pieces {
			for _, second := range pieces {
				for _, upgraded := range []bool{false, true} {
					var sb StringBuilder
					var want []uint16
					if upgraded {
						sb.WriteUnit(0xe9)
						want = append(want, 0xe9)
					}
					sb.writeASCIIBytes(filler[:n])
					want = append(want, utf16.Encode([]rune(filler[:n]))...)
					first.write(&sb)
					second.write(&sb)
					want = append(append(want, first.units...), second.units...)
					require.Equal(t, len(want), sb.Len(), "n=%d %s %s %v", n, first.name, second.name, upgraded)
					if got := sb.String().UTF16(); len(want) == 0 {
						require.Empty(t, got)
					} else {
						require.Equal(t, want, got, "n=%d %s %s %v", n, first.name, second.name, upgraded)
					}
				}
			}
		}
	}
}

// TestStringBuilderLen checks Len through every move between the builder's
// stores (inline, heap ASCII, UTF-16): Len sums all three, which holds only
// while each move empties the store it leaves.
func TestStringBuilderLen(t *testing.T) {
	long := strings.Repeat("a", smallASCIIMax+1)
	for _, c := range []struct {
		name  string
		write func(*StringBuilder)
		want  string
	}{
		{"inline", func(sb *StringBuilder) { sb.WriteGoString("abc") }, "abc"},
		{"spill by a byte", func(sb *StringBuilder) {
			sb.WriteGoString(long[:smallASCIIMax])
			sb.WriteASCII('b')
		}, long[:smallASCIIMax] + "b"},
		{"spill by a string", func(sb *StringBuilder) { sb.WriteGoString("x"); sb.WriteGoString(long) }, "x" + long},
		{"spill by Grow", func(sb *StringBuilder) { sb.WriteGoString("xy"); sb.Grow(smallASCIIMax); sb.WriteASCII('z') }, "xyz"},
		{"upgrade from inline", func(sb *StringBuilder) { sb.WriteGoString("ab"); sb.WriteUnit('é') }, "abé"},
		{"upgrade from heap", func(sb *StringBuilder) { sb.WriteGoString(long); sb.WriteRune('😀') }, long + "😀"},
		{"UTF-16 from the start", func(sb *StringBuilder) {
			sb.growFor(FromGoString("é"), 4)
			sb.WriteString(FromGoString("éa"))
		}, "éa"},
		{"UTF-16 slice after ASCII", func(sb *StringBuilder) {
			sb.WriteGoString("ab")
			writeSlice(sb, FromGoString("xüy"), 1, 3)
		}, "abüy"},
	} {
		var sb StringBuilder
		c.write(&sb)
		want := FromGoString(c.want)
		assert.Equal(t, want.Len(), sb.Len(), c.name)
		assert.Equal(t, c.want, sb.String().GoString(), c.name)
		sb.Reset()
		assert.Zero(t, sb.Len(), c.name)
	}
}

func TestHash(t *testing.T) {
	a := FromGoString("property")
	b := FromGoString("property")
	assert.Equal(t, a.Hash(), b.Hash())
	assert.NotZero(t, a.Hash())
	assert.NotEqual(t, a.Hash(), FromGoString("propertx").Hash())
	rope := ropeOf(FromGoString(strings.Repeat("p", 40)), FromGoString(strings.Repeat("q", 40)))
	flat := FromGoString(strings.Repeat("p", 40) + strings.Repeat("q", 40))
	assert.Equal(t, flat.Hash(), rope.Hash())
}

func TestIsWellFormed(t *testing.T) {
	assert.True(t, FromGoString("abc").IsWellFormed())
	assert.True(t, FromGoString("😀").IsWellFormed())
	assert.False(t, FromUTF16([]uint16{0xD800}).IsWellFormed())
	assert.False(t, FromUTF16([]uint16{0xDC00, 'a'}).IsWellFormed())
}

func TestBigInt(t *testing.T) {
	b, ok := NewBigIntFromDecimal("123456789012345678901234567890")
	require.True(t, ok)
	assert.Equal(t, "123456789012345678901234567890", b.ToString())
	c, _ := NewBigIntFromDecimal("123456789012345678901234567890")
	assert.True(t, b.StrictEquals(c))
	assert.False(t, b.StrictEquals(NewBigIntFromInt64(1)))
	assert.True(t, NewBigIntFromInt64(0).IsZero())
	_, ok = NewBigIntFromDecimal("12x")
	assert.False(t, ok)
	assert.Equal(t, "bigint", TypeOf(BigIntValue(b)).GoString())
}

// ropeOf builds a rope node directly, whatever Concat would build.
func ropeOf(a, b *String) *String {
	return &String{p: unsafe.Pointer(a), right: b, n: a.n + b.n, kind: strRope}
}

func TestRopeFlattenShapes(t *testing.T) {
	leaf := func(i int) *String {
		if i%7 == 3 {
			return FromGoString(string(rune('α' + i%20)))
		}
		return FromGoString(string(rune('a' + i%26)))
	}
	for _, mixed := range []bool{false, true} {
		var want strings.Builder
		pieces := make([]*String, 3000)
		for i := range pieces {
			p := FromGoString(string(rune('a' + i%26)))
			if mixed {
				p = leaf(i)
			}
			pieces[i] = p
			want.WriteString(p.GoString())
		}
		var balanced func(lo, hi int) *String
		balanced = func(lo, hi int) *String {
			if hi-lo == 1 {
				return pieces[lo]
			}
			m := (lo + hi) / 2
			return ropeOf(balanced(lo, m), balanced(m, hi))
		}
		left, right := pieces[0], pieces[len(pieces)-1]
		for i := 1; i < len(pieces); i++ {
			left = ropeOf(left, pieces[i])
			right = ropeOf(pieces[len(pieces)-1-i], right)
		}
		zig := pieces[0]
		for i := 1; i < len(pieces); i++ {
			if i%2 == 0 {
				zig = ropeOf(zig, pieces[i])
			} else {
				// A right child that is itself a rope.
				zig = ropeOf(zig, ropeOf(pieces[i], emptyString))
			}
		}
		for name, s := range map[string]*String{"left-deep": left, "right-deep": right, "balanced": balanced(0, len(pieces)), "zigzag": zig} {
			assert.Equal(t, want.String(), s.GoString(), "%s mixed=%v", name, mixed)
			s.flatten() // GoString reads a mixed rope's leaves if it can
			assert.Equal(t, !mixed, flatKind(t, s) == strASCII, "%s mixed=%v", name, mixed)
			assert.Nil(t, s.p, "the left child is released")
			assert.Nil(t, s.right)
		}
	}
	// The contents are one allocation: no growing stack, no copy into a
	// Go string.
	for _, tail := range []string{"bb", "é"} {
		a := FromGoString(strings.Repeat("a", 100))
		b := FromGoString(tail)
		ropes := make([]*String, 21) // AllocsPerRun's warm-up run and 20
		for i := range ropes {
			ropes[i] = ropeOf(ropeOf(a, b), ropeOf(b, a))
		}
		assert.Equal(t, 1.0, testing.AllocsPerRun(20, func() {
			ropes[0].flatten()
			ropes = ropes[1:]
		}), tail)
	}
}

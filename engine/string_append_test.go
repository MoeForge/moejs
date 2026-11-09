package engine

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bufStart is the address of a flat string's first unit.
func bufStart(s *String) unsafe.Pointer {
	if s.kind == strASCII {
		return unsafe.Pointer(unsafe.StringData(s.s))
	}
	return s.p
}

// TestAppendInPlace covers Concat's append buffers: the first append
// copies, the string ending the buffer writes in place, any other copies,
// growth keeps the capacity within about twice the length, and ASCII
// buffers turn UTF-16 when a non-ASCII piece arrives.
func TestAppendInPlace(t *testing.T) {
	base := FromGoString(strings.Repeat("a", 63))
	s1 := concat(base, FromGoString("b"))
	require.True(t, s1.flags&strInBuf != 0, "a long result appending a shorter piece is in a buffer")
	assert.Equal(t, strASCII, s1.kind)
	h := s1.appendBuf()
	assert.Equal(t, int32(64), h.used.Load())
	assert.Equal(t, int32(72), h.cap, "exact up to the 128-byte block")
	assert.False(t, base.flags&strInBuf != 0)

	s2 := concat(s1, FromGoString("c"))
	assert.True(t, s2.flags&strInBuf != 0)
	assert.Equal(t, bufStart(s1), bufStart(s2), "s1 ends its buffer: in place")
	assert.Equal(t, int32(65), h.used.Load())

	// s1 no longer ends the buffer: it copies, and s2 keeps its unit.
	s3 := concat(s1, FromGoString("d"))
	assert.NotEqual(t, bufStart(s1), bufStart(s3))
	assert.Equal(t, strings.Repeat("a", 63)+"bc", s2.GoString())
	assert.Equal(t, strings.Repeat("a", 63)+"bd", s3.GoString())
	assert.Equal(t, strings.Repeat("a", 63)+"b", s1.GoString())
	assert.Equal(t, 64, len(s1.GoString()), "a string exposes its own length only")

	// Growth: every step's content, and the capacity bound.
	want := []byte(s2.GoString())
	s := s2
	grew := 0
	for i := range 5000 {
		piece := string(rune('a' + i%26))
		if i%10 == 0 {
			piece = "xyz"
		}
		prev := bufStart(s)
		s = concat(s, FromGoString(piece))
		want = append(want, piece...)
		if bufStart(s) != prev {
			grew++
			c := int(s.appendBuf().cap)
			assert.LessOrEqual(t, c, 2*s.Len(), "capacity %d for %d units", c, s.Len())
		}
	}
	assert.Equal(t, string(want), s.GoString())
	assert.Less(t, grew, 30, "the buffer grows geometrically")

	// s + s writes a copy of itself after itself.
	d := concat(s1, s1)
	assert.Equal(t, strings.Repeat(strings.Repeat("a", 63)+"b", 2), d.GoString())
	dd := concat(d, d)
	assert.Equal(t, strings.Repeat(strings.Repeat("a", 63)+"b", 4), dd.GoString())
}

func TestAppendUTF16(t *testing.T) {
	ascii := concat(FromGoString(strings.Repeat("a", 63)), FromGoString("b"))
	require.True(t, ascii.flags&strInBuf != 0)
	// ASCII to UTF-16: a new buffer of units.
	w := concat(ascii, FromGoString("é"))
	assert.True(t, w.flags&strInBuf != 0)
	assert.Equal(t, strUTF16, w.kind)
	assert.Equal(t, strings.Repeat("a", 63)+"bé", w.GoString())
	// ASCII and UTF-16 pieces in place in the UTF-16 buffer.
	w2 := concat(w, FromGoString("x"))
	w3 := concat(w2, FromGoString("中"))
	assert.Equal(t, bufStart(w), bufStart(w3))
	assert.Equal(t, strings.Repeat("a", 63)+"béx中", w3.GoString())
	assert.Equal(t, strings.Repeat("a", 63)+"bé", w.GoString())
	assert.Equal(t, strings.Repeat("a", 63)+"b", ascii.GoString(), "the ASCII buffer is untouched")
	// The ASCII buffer still extends in place from its end.
	a2 := concat(ascii, FromGoString("c"))
	assert.Equal(t, bufStart(ascii), bufStart(a2))
	// A UTF-16 string not in a buffer copies.
	u := concat(FromGoString(strings.Repeat("é", 40)), FromGoString(strings.Repeat("z", 30)))
	assert.True(t, u.flags&strInBuf != 0)
	assert.Equal(t, strings.Repeat("é", 40)+strings.Repeat("z", 30), u.GoString())
	// units has no room past the string: appending to it copies.
	units := w2.UTF16()
	assert.Equal(t, len(units), cap(units))
	_ = append(units, 'Q')
	assert.Equal(t, strings.Repeat("a", 63)+"béx中", w3.GoString())
	// Equality and hashes match a flat string's; a cached hash stays right.
	flat := FromGoString(strings.Repeat("a", 63) + "béx中")
	assert.True(t, w3.Equals(flat))
	assert.Equal(t, flat.Hash(), w3.Hash())
	hw := w.Hash()
	concat(w3, FromGoString("!"))
	assert.Equal(t, hw, w.Hash())
	assert.Equal(t, FromGoString(strings.Repeat("a", 63)+"bé").Hash(), hw)
}

// TestAppendKeepsRopes: prepends, joins of big pieces and appends to ropes
// stay ropes; an appended rope piece is copied without being flattened.
func TestAppendKeepsRopes(t *testing.T) {
	short, long := FromGoString(strings.Repeat("s", 10)), FromGoString(strings.Repeat("l", 100))
	assert.Equal(t, strRope, concat(short, long).kind, "prepend")
	big := FromGoString(strings.Repeat("B", appendPieceMax))
	assert.Equal(t, strRope, concat(big, big).kind, "join of big pieces")
	r := concat(short, long)
	assert.Equal(t, strRope, concat(r, short).kind, "append to a rope")
	// An append buffer takes a big piece too.
	inBuf := concat(FromGoString(strings.Repeat("x", appendPieceMax+1)), short)
	require.True(t, inBuf.flags&strInBuf != 0)
	got := concat(inBuf, big)
	assert.True(t, got.flags&strInBuf != 0)
	assert.Equal(t, strings.Repeat("x", appendPieceMax+1)+short.GoString()+big.GoString(), got.GoString())
	// A rope piece, ASCII or not.
	for _, tail := range []string{"t", "é"} {
		piece := ropeOf(FromGoString(strings.Repeat("p", 40)), FromGoString(strings.Repeat(tail, 30)))
		host := concat(FromGoString(strings.Repeat("h", 200)), FromGoString("!"))
		got := concat(host, piece)
		assert.Equal(t, strings.Repeat("h", 200)+"!"+strings.Repeat("p", 40)+strings.Repeat(tail, 30), got.GoString())
		assert.Equal(t, strRope, piece.kind, "copied without flattening")
	}
	// A substring of a buffered string is not in the buffer: it copies.
	s := concat(FromGoString(strings.Repeat("q", 100)), FromGoString("rs"))
	sub := s.Substring(0, 101)
	assert.False(t, sub.flags&strInBuf != 0)
	assert.Equal(t, strings.Repeat("q", 100)+"rz", concat(sub, FromGoString("z")).GoString())
	assert.Equal(t, strings.Repeat("q", 100)+"rs", s.GoString())
}

// TestAppendLimit: the length limit is checked before anything is written.
func TestAppendLimit(t *testing.T) {
	defer func(old int) { maxStringLength = old }(maxStringLength)
	maxStringLength = 100
	r := NewRealm()
	s := concat(FromGoString(strings.Repeat("a", 80)), FromGoString("b"))
	require.True(t, s.flags&strInBuf != 0)
	_, err := r.Concat(s, FromGoString(strings.Repeat("c", 20)))
	require.Error(t, err)
	assert.Equal(t, int32(81), s.appendBuf().used.Load())
	ok, err := r.Concat(s, FromGoString(strings.Repeat("c", 19)))
	require.NoError(t, err)
	assert.Equal(t, 100, ok.Len())
}

func TestAppendAllocations(t *testing.T) {
	base := FromGoString(strings.Repeat("a", 100))
	c := FromGoString("c")
	assert.Equal(t, 1.0, testing.AllocsPerRun(50, func() { concat(base, c) }), "a new buffer comes with its String")
	s := concat(base, c)
	assert.Equal(t, 1.0, testing.AllocsPerRun(50, func() { s = concat(s, c) }), "in place or grown, one allocation a step")
	// Past appendBlockMax a buffer is an allocation of its own.
	huge := FromGoString(strings.Repeat("h", appendBlockMax))
	assert.Equal(t, 2.0, testing.AllocsPerRun(10, func() { concat(huge, c) }))
}

// TestAppendBlockSizes: each block fills the size class strBlocks names.
func TestAppendBlockSizes(t *testing.T) {
	classes := []uint64{128, 192, 256, 384, 512, 768, 1024, 1536, 2048, 3072, 4096, 6144, 8192, 12288, 16384, 24576, 32768}
	require.Len(t, classes, len(strBlocks))
	for i, b := range strBlocks {
		if i > 0 {
			assert.Greater(t, b.bytes, strBlocks[i-1].bytes)
		}
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		const n = 1000
		for range n {
			b.alloc()
		}
		runtime.ReadMemStats(&m1)
		// The runtime allocates a little of its own meanwhile.
		got := float64(m1.TotalAlloc-m0.TotalAlloc) / n
		if is64 { // a 64-bit layout
			assert.True(t, got >= float64(classes[i]) && got < float64(classes[i])*1.02, "block of %d bytes: %.0f bytes an allocation, want %d", b.bytes, got, classes[i])
		}
	}
	assert.Equal(t, appendBlockMax, strBlocks[len(strBlocks)-1].bytes)
}

// TestAppendConcurrent appends to one buffered string from several
// goroutines at once, each with its own realm, as host values allow: one
// writes in place, the others copy, and nobody sees another's units (the
// race detector checks the buffer's accesses).
func TestAppendConcurrent(t *testing.T) {
	shared := concat(FromGoString(strings.Repeat("s", 100)), FromGoString("!"))
	want := shared.GoString()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := NewRealm()
			for round := range 200 {
				tail := strings.Repeat(string(rune('A'+g)), 1+round%5)
				s, err := r.Concat(shared, FromGoString(tail))
				if !assert.NoError(t, err) {
					return
				}
				s2, _ := r.Concat(s, FromGoString("é"))
				s3, _ := r.Concat(s2, FromGoString(tail))
				if !assert.Equal(t, want+tail+"é"+tail, s3.GoString()) || !assert.Equal(t, want+tail, s.GoString()) {
					return
				}
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, want, shared.GoString())
}

// TestAppendJS covers the patterns that reach Concat from JavaScript.
func TestAppendJS(t *testing.T) {
	accTable(t, [][2]string{
		{`var s = "", parts = [];
		  for (var i = 0; i < 3000; i++) { var c = String.fromCharCode(97 + i % 26); if (i % 500 === 499) c = "é"; s += c; parts.push(c); }
		  return [s === parts.join(""), s.length, s.charCodeAt(499), s.indexOf("é"), s.slice(495, 502)];`,
			`[true,3000,233,499,"bcdeégh"]`},
		{`var s = "x".repeat(70), t = s; s += "a"; t += "b"; var u = s; s += "c"; u += "d";
		  return [s, t, u].map(function (v) { return v.slice(68); });`, `["xxac","xxb","xxad"]`},
		{`var s = "ab".repeat(40); for (var i = 0; i < 6; i++) s = s + s; return [s.length, s.slice(-3), s === "ab".repeat(2560)];`,
			`[5120,"bab",true]`},
		{`var s = "seed".repeat(20); for (var i = 0; i < 200; i++) s = "(" + s + ")"; return [s.length, s.slice(0, 3), s.slice(-3), s.indexOf("s")];`,
			`[480,"(((",")))",200]`},
		{`var s = "", n = 0; for (var i = 0; i < 300; i++) { s = ` + "`${s}<${i}>`" + `; } return [s.length, s.slice(-7), JSON.stringify(s).length];`,
			`[1390,"8><299>",1392]`},
		{`var o = {}, m = new Map(), s = "key-".repeat(20);
		  for (var i = 0; i < 5; i++) { s += i; o[s] = i; m.set(s, i); }
		  var k = "key-".repeat(20) + "012"; return [o[k], m.get(k), Object.keys(o).length, s.concat("!", "?").slice(-4)];`,
			`[2,2,5,"34!?"]`},
	})
}

// TestAppendToGo: a buffered string exported to Go is its own bytes, which
// later appends past it leave alone.
func TestAppendToGo(t *testing.T) {
	r := NewRealm()
	s := concat(FromGoString(strings.Repeat("g", 70)), FromGoString("o"))
	got := r.ToGo(StringValue(s)).(string)
	s2 := concat(s, FromGoString("ne"))
	assert.Equal(t, strings.Repeat("g", 70)+"o", got)
	assert.Equal(t, strings.Repeat("g", 70)+"one", r.ToGo(StringValue(s2)))
	b, ok, err := r.AppendJSON(nil, StringValue(s))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `"`+strings.Repeat("g", 70)+`o"`, string(b))
}

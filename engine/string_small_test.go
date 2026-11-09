package engine

import (
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmallASCIICoallocation checks that strings built through the
// co-allocated small-string path (Concat, StringBuilder, case conversion,
// NumberToString) hold the right content across the size buckets and the
// boundary to separately allocated payloads.
func TestSmallASCIICoallocation(t *testing.T) {
	r := NewRealm()
	for n := 1; n <= smallASCIIMax+8; n++ {
		src := strings.Repeat("Ab", n/2) + strings.Repeat("c", n%2)
		a, b := src[:n/2], src[n/2:]
		got := concat(asciiString(a), asciiString(b))
		assert.Equal(t, src, got.GoString(), "concat n=%d", n)
		assert.Equal(t, n, got.Len())

		var sb StringBuilder
		for i := range n {
			sb.WriteASCII(src[i])
		}
		assert.Equal(t, src, sb.String().GoString(), "builder n=%d", n)
		sb.Reset()
		sb.WriteGoString(src)
		assert.Equal(t, src, sb.String().GoString(), "builder string n=%d", n)

		lower, _ := stringToLower(r, asciiString(src))
		assert.Equal(t, strings.ToLower(src), lower.GoString(), "lower n=%d", n)
		upper, _ := stringToUpper(r, asciiString(src))
		assert.Equal(t, strings.ToUpper(src), upper.GoString(), "upper n=%d", n)
		// The source is untouched by the in-place case conversion.
		assert.Equal(t, strings.Repeat("Ab", n/2)+strings.Repeat("c", n%2), src)
	}
	// A builder that upgrades to UTF-16 after inline ASCII bytes keeps them.
	var sb StringBuilder
	sb.WriteGoString("abc")
	sb.WriteUnit(0x4E2D)
	sb.WriteGoString("def")
	assert.Equal(t, "abc中def", sb.String().GoString())
	// A builder that spills past the inline array keeps the prefix.
	sb.Reset()
	long := strings.Repeat("x", smallASCIIMax-1) + "yz" + strings.Repeat("w", 100)
	for i := range len(long) {
		sb.WriteASCII(long[i])
	}
	assert.Equal(t, len(long), sb.Len())
	assert.Equal(t, long, sb.String().GoString())

	for _, f := range []float64{0, 7, 10, -3, 12345, 1e21, 0.5, -1.25e-7, 1234567890123456} {
		assert.Equal(t, NumberToGoString(f), NumberToString(f).GoString(), "number %v", f)
	}
	assert.Equal(t, "42", NumberToString(42).GoString())
	assert.Equal(t, strconv.FormatInt(1<<40, 10), NumberToString(1<<40).GoString())
}

func TestSmallASCIIAllocations(t *testing.T) {
	a, b := asciiString("Content-"), asciiString("Type")
	assert.Equal(t, 1.0, testing.AllocsPerRun(50, func() { concat(a, b) }))
	mixed := asciiString("X-Request-Id")
	r := NewRealm()
	assert.Equal(t, 1.0, testing.AllocsPerRun(50, func() { stringToLower(r, mixed) }))
	assert.Equal(t, 1.0, testing.AllocsPerRun(50, func() { NumberToString(3700) })) // past intStrings
	assert.Equal(t, 1.0, testing.AllocsPerRun(50, func() {
		var sb StringBuilder
		sb.WriteString(a)
		sb.WriteASCII(':')
		sb.WriteString(b)
		sb.String()
	}))
}

// TestStringSize pins String to the 48-byte size class, with the small
// co-allocated strings filling theirs: a field added to String moves every
// string, rope node and split piece up a class.
func TestStringSize(t *testing.T) {
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(48), unsafe.Sizeof(String{}))
		assert.Equal(t, uintptr(64), unsafe.Sizeof(asciiBuf16{}))
		assert.Equal(t, uintptr(80), unsafe.Sizeof(asciiBuf32{}))
		assert.Equal(t, uintptr(96), unsafe.Sizeof(asciiBuf48{}))
		assert.Equal(t, uintptr(112), unsafe.Sizeof(asciiBuf64{}))
		assert.Equal(t, uintptr(64), unsafe.Sizeof(utf16Buf[[8]uint16]{}))
		assert.Equal(t, uintptr(176), unsafe.Sizeof(utf16Buf[[64]uint16]{}))
	}
}

var smallASCIISink *String

// TestSmallASCIISizeClasses checks that a co-allocated string takes the
// size class of its length: the header and up to 16, 32, 48 or 64 bytes,
// 64 to 112 bytes.
func TestSmallASCIISizeClasses(t *testing.T) {
	for n := 1; n <= smallASCIIMax; n++ {
		per := uint64(math.MaxUint64)
		for range 3 { // the least of three: the runtime allocates now and then
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			for range 1000 {
				smallASCIISink, _ = newASCIIBuf(n)
			}
			runtime.ReadMemStats(&m1)
			per = min(per, (m1.TotalAlloc-m0.TotalAlloc)/1000)
		}
		if is64 { // a 64-bit layout
			assert.Equal(t, uint64(64+16*((n-1)/16)), per, "n=%d", n)
		}
	}
}

// TestSmallUTF16Concat: a UTF-16 concatenation shorter than ropeThreshold
// is one allocation, its header and units together, of the size class of
// its length, 64 to 176 bytes, and holds the units of both sides.
func TestSmallUTF16Concat(t *testing.T) {
	r := NewRealm()
	for n := 2; n < ropeThreshold; n++ {
		a := asciiString(strings.Repeat("a", n/2))
		b := FromGoString(strings.Repeat("中", n-n/2))
		s, err := r.Concat(a, b)
		require.NoError(t, err)
		assert.Equal(t, a.GoString()+b.GoString(), s.GoString(), "n=%d", n)
		assert.Equal(t, n, s.Len())
		s, err = r.Concat(b, s)
		require.NoError(t, err)
		assert.Equal(t, b.GoString()+a.GoString()+b.GoString(), s.GoString(), "n=%d", n)
		assert.Equal(t, 1.0, testing.AllocsPerRun(20, func() { _, _ = r.Concat(a, b) }), "n=%d", n)
		per := uint64(math.MaxUint64)
		for range 3 { // the least of three: the runtime allocates now and then
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			for range 1000 {
				smallASCIISink, _ = newUTF16Buf(n)
			}
			runtime.ReadMemStats(&m1)
			per = min(per, (m1.TotalAlloc-m0.TotalAlloc)/1000)
		}
		if is64 { // a 64-bit layout
			assert.Equal(t, uint64(64+16*((n-1)/8)), per, "n=%d", n)
		}
	}
	// The units outlive a collection with only the String held.
	s, err := r.Concat(asciiString("held "), FromGoString("中文"))
	require.NoError(t, err)
	runtime.GC()
	runtime.GC()
	assert.Equal(t, "held 中文", s.GoString())
}

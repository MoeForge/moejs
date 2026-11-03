package engine

import (
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

func TestLazySmallDecodeAllocations(t *testing.T) {
	for _, n := range []int{1, 8, 9, 16, 17, 24, 25, 32, 33, utf16BufMax, utf16BufMax + 1} {
		for _, mode := range []lazyStringMode{lazyText, lazyWTF8, lazyJSONContents} {
			text := strings.Repeat("中", n)
			if mode == lazyJSONContents {
				text = strings.Repeat(`\u4e2d`, n)
			}
			var decoded *String
			allocs := testing.AllocsPerRun(50, func() {
				decoded = decodeLazyString(text, mode)
			})
			want := 1.0
			if n > utf16BufMax {
				want = 2
			}
			require.Equal(t, want, allocs, "n=%d, mode=%d", n, mode)
			require.Equal(t, strings.Repeat("中", n), decoded.GoString())
		}
	}
}

func TestLazySmallDecodeSurrogatesAndLifetime(t *testing.T) {
	for _, n := range []int{utf16BufMax, utf16BufMax + 1} {
		// A pair and a lone surrogate straddle the small-buffer cutoff.
		text := strings.Repeat("a", n-3) + "😀" + "\xed\xa0\x80"
		s := decodeLazyString(text, lazyWTF8)
		require.Equal(t, n, s.Len())
		require.Equal(t, int64(stringHeaderSize+2*n), liveSize(s), "exactly the units and a header")
		sub := s.Substring(n-3, n)
		s = nil
		runtime.GC()
		require.Equal(t, []uint16{0xd83d, 0xde00, 0xd800}, sub.UTF16())
	}
}

// A parse under a limit charges a lazy string's block and decoded units once,
// when it makes the string; decoding charges nothing more.
func TestLazySmallChargeOnce(t *testing.T) {
	for _, n := range []int{1, 8, 9, 16, 17, 24, 25, 32, 33, utf16BufMax, utf16BufMax + 1} {
		r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
		text := strings.Repeat("中", n)
		before := r.Stats().RequestAllocatedBytes
		s := r.lazyStringSized(text, lazyWTF8, wtf8Units(text), true)
		want := int64(s.lazyBlockSize() + stringHeaderSize + 2*n)
		require.Equal(t, want, r.Stats().RequestAllocatedBytes-before, "charged when made")
		require.Equal(t, n, s.Len())
		require.Nil(t, cacheOf(s))
		require.Equal(t, utf16.Encode([]rune(text)), s.UTF16())
		require.NotNil(t, cacheOf(s))
		require.Equal(t, want, r.Stats().RequestAllocatedBytes-before, "decoding charges nothing more")
	}
}

package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsASCII checks isASCII at every length up to 100, with no byte
// above 0x7f and with one at each position, against a loop over the
// bytes, and FromGoString's choice of representation with it.
func TestIsASCII(t *testing.T) {
	ref := func(s string) bool {
		for i := 0; i < len(s); i++ {
			if s[i] >= 0x80 {
				return false
			}
		}
		return true
	}
	for n := range 101 {
		base := strings.Repeat("abcdefgh\x7f", n/9+1)[:n]
		require.True(t, isASCII(base), n)
		require.True(t, FromGoString(base).IsASCII(), n)
		for i := range n {
			for _, c := range []byte{0x80, 0xc3, 0xff} {
				s := base[:i] + string([]byte{c}) + base[i+1:]
				require.Equal(t, ref(s), isASCII(s), "%q", s)
				require.False(t, isASCII(s), "%q", s)
				require.False(t, FromGoString(s).IsASCII(), "%q", s)
			}
		}
	}
}

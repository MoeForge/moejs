package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGrowValuesLarge pushes values one at a time, as Array.prototype.push
// does: the slice doubles, past growLargeMin values too, so the slices
// allocated on the way add up to about twice the last one, not five times
// as with append's growth, and each is charged.
func TestGrowValuesLarge(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 40})
	before := r.Stats().RequestAllocatedBytes
	var s []Value
	total := 0
	for i := range 100000 {
		c := cap(s)
		s = r.appendValue(s, IntValue(i))
		if cap(s) == c {
			continue
		}
		total += cap(s)
		if c >= growLargeMin {
			assert.GreaterOrEqual(t, cap(s), 2*c, "grown from %d", c)
		}
	}
	require.Len(t, s, 100000)
	assert.Equal(t, IntValue(99999), s[99999])
	assert.LessOrEqual(t, total, 2*cap(s)+2*growLargeMin)
	assert.GreaterOrEqual(t, r.Stats().RequestAllocatedBytes-before, int64(total*valueSize))

	// A growth past double takes what it needs.
	big := r.growValues(make([]Value, growLargeMin, growLargeMin), 10*growLargeMin)
	assert.Len(t, big, growLargeMin)
	assert.Equal(t, 11*growLargeMin, cap(big))

	assert.Equal(t, "100000,99999,4999950000", runScripts(t, `const a = []; for (let i = 0; i < 100000; i++) a.push(i);
let sum = 0; for (const v of a) sum += v; [a.length, a[99999], sum].join()`))
}

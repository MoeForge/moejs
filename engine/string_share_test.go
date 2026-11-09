package engine

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSharedStringHash: a string a host keeps, used by realms on two
// goroutines at once as a Map key in both, and as a Set key in one while
// the other compares it with === (Equals reads the hash the first Hash
// stores). Under -race the hash's first store must not race with the
// other goroutine's reads and stores.
func TestSharedStringHash(t *testing.T) {
	const src = `export function mapped(k) { const m = new Map(); m.set(k, 1); return m.get(k) + m.size; }
export function setOf(k) { const s = new Set(); s.add(k); return s.has(k) ? 1 : 0; }
export function same(k, o) { let n = 0; for (let i = 0; i < 4; i++) if (k === o) n++; return n; }`
	run := func(f *moduleFixture, name string, args ...Value) Value {
		fn, _ := f.env.GetBindingValue(name)
		v, err := f.r.Call(fn, Undefined(), args)
		require.NoError(t, err)
		return v
	}
	var fs [2]*moduleFixture
	for i := range fs {
		fs[i] = evalModuleWith(t, src, RealmOptions{})
	}
	var got [2]Value
	key := StringValue(FromGoString("tenant-42"))
	var wg sync.WaitGroup
	for i := range fs {
		wg.Go(func() { got[i] = run(fs[i], "mapped", key) })
	}
	wg.Wait()
	assert.Equal(t, []any{int64(2), int64(2)}, []any{fs[0].r.ToGo(got[0]), fs[1].r.ToGo(got[1])})

	key, other := StringValue(FromGoString("tenant-42")), StringValue(FromGoString("tenant-43"))
	wg.Go(func() { got[0] = run(fs[0], "setOf", key) })
	wg.Go(func() { got[1] = run(fs[1], "same", key, other) })
	wg.Wait()
	assert.Equal(t, []any{int64(1), int64(0)}, []any{fs[0].r.ToGo(got[0]), fs[1].r.ToGo(got[1])})
}

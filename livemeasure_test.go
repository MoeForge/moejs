package moejs_test

// The pooled storage of measurements under concurrent realms, the pacing
// of the measurement mid-JavaScript, and root providers that remove
// themselves.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMeasureLiveConcurrentRealms: realms on their own goroutines measure at
// once, through the process-wide pool of walk storage, each always to the
// same size for an unchanged heap, and to the size it measures alone.
func TestMeasureLiveConcurrentRealms(t *testing.T) {
	const realms = 12
	sizes := make([]int64, realms)
	rts := make([]*moejs.Runtime, realms)
	for i := range realms {
		rts[i] = moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 30, MutableIntrinsics: i%2 == 1})
		s, err := moejs.CompileScript("heap.js", fmt.Sprintf(`
globalThis.kept = [];
for (let i = 0; i < %d; i++) kept.push({i, s: "v" + i, a: [i, i + 1], m: i %% 50 ? null : new Map([[i, {i}]])});
globalThis.str = "x".repeat(%d).slice(3, 40);`, 2000+i*1500, 1000+i*100))
		require.NoError(t, err)
		_, err = rts[i].RunScript(s)
		require.NoError(t, err)
		sizes[i] = rts[i].Realm().MeasureLive(nil)
	}
	var wg sync.WaitGroup
	for i := range realms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				if got := rts[i].Realm().MeasureLive(nil); got != sizes[i] {
					t.Errorf("realm %d: %d, alone %d", i, got, sizes[i])
					return
				}
			}
		}()
	}
	wg.Wait()
}

// countMeasurements registers a provider that counts the measurements of rt.
func countMeasurements(rt *moejs.Runtime) *atomic.Int64 {
	var n atomic.Int64
	rt.AddLiveRoots(func(func(moejs.Value)) { n.Add(1) })
	return &n
}

const pacingSource = `
const kept = [];
export function keep(mib) { for (let i = 0; i < mib * 16; i++) kept.push("k".repeat(65536 - 16) + i); }
export function churn(mib) { let n = 0; for (let i = 0; i < mib * 64; i++) { const a = []; for (let j = 0; j < 1000; j++) a.push(j); n += a.length; } return n; }
export function retry(n, size) { let caught = 0; for (let i = 0; i < n; i++) { try { "y".repeat(size); } catch (e) { caught++; } } return caught; }
`

// TestRemeasurePacingBounded: the measurement mid-JavaScript paces itself and
// neither loops nor walks once per allocation near the limit: a program
// keeping half its limit makes 20 times its limit of garbage in one call
// with about one measurement per quarter limit; one keeping 97 % stops
// after one more measurement; one that catches the RangeError of a
// String.prototype.repeat too long for what the limit leaves, 100,000
// times, does not measure each time.
func TestRemeasurePacingBounded(t *testing.T) {
	mod, err := moejs.Compile("pacing.js", pacingSource)
	require.NoError(t, err)
	const limit = 32 << 20
	setup := func(keep int64) (*moejs.Runtime, *atomic.Int64) {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		require.NoError(t, rt.Load(mod))
		n := countMeasurements(rt)
		for i := int64(0); i < keep; i++ {
			_, err := rt.Call(mustHook(t, mod, "keep"), moejs.Int(1))
			require.NoError(t, err)
			require.NoError(t, rt.RebaseAllocation(nil))
		}
		require.NoError(t, rt.RebaseAllocation(nil))
		return rt, n
	}
	// Half the limit kept, 20 times the limit of garbage.
	rt, n := setup(16)
	n.Store(0)
	start := time.Now()
	_, err = rt.Call(mustHook(t, mod, "churn"), moejs.Int(20*32))
	t.Logf("half kept, 640 MiB of garbage: %v in %v, %d measurements, live %d MiB", err, time.Since(start), n.Load(), rt.Stats().LiveBytes>>20)
	assert.NoError(t, err)
	assert.LessOrEqual(t, n.Load(), int64(120), "measurements")
	// 97 % kept: one more measurement, then the hit.
	rt, n = setup(31)
	n.Store(0)
	start = time.Now()
	_, err = rt.Call(mustHook(t, mod, "churn"), moejs.Int(64))
	t.Logf("97%% kept, garbage: %v in %v, %d measurements", err, time.Since(start), n.Load())
	assert.ErrorIs(t, err, moejs.ErrMemoryLimit)
	assert.LessOrEqual(t, n.Load(), int64(3))
	assert.Less(t, time.Since(start), 10*time.Second)
	// A RangeError caught 100,000 times.
	rt, n = setup(24)
	n.Store(0)
	start = time.Now()
	v, err := rt.Call(mustHook(t, mod, "retry"), moejs.Int(100000), moejs.Int(12<<20))
	t.Logf("repeat too long for the room, caught 100,000 times: %v %v in %v, %d measurements", v, err, time.Since(start), n.Load())
	assert.LessOrEqual(t, n.Load(), int64(5), "measurements")
	assert.Less(t, time.Since(start), 10*time.Second)
}

// TestLiveRootsRemoveThemselves: providers that remove themselves,
// remove others or register new ones while a measurement calls them do
// not break the measurement, and a removed one is not called again.
func TestLiveRootsRemoveThemselves(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 64 << 20})
	calls := make([]atomic.Int64, 6)
	removes := make([]func(), 6)
	for i := range removes {
		removes[i] = rt.AddLiveRoots(func(func(moejs.Value)) {
			calls[i].Add(1)
			switch i {
			case 0, 3:
				removes[i]() // itself
				removes[i]() // twice
			case 1:
				removes[4]() // a later one, before its turn
			case 2:
				rt.AddLiveRoots(func(func(moejs.Value)) { calls[5].Add(1) })
			}
		})
	}
	for range 3 {
		rt.Realm().MeasureLive(nil)
	}
	got := make([]int64, 6)
	for i := range got {
		got[i] = calls[i].Load()
	}
	t.Logf("calls: %v", got)
	assert.Equal(t, int64(1), got[0], "removed itself")
	assert.Equal(t, int64(3), got[1])
	assert.Equal(t, int64(3), got[2])
	assert.Equal(t, int64(1), got[3], "removed itself")
	assert.Equal(t, int64(0), got[4], "removed before its turn")
}

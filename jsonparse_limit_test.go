package moejs_test

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// numbersArray is the JSON text of an array of n small numbers.
func numbersArray(n int) string {
	var b strings.Builder
	b.Grow(5 * n)
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i % 1000))
	}
	b.WriteByte(']')
	return b.String()
}

// peakHeapBy returns the most the Go heap's objects grew by while f ran,
// sampled every 100 µs, and what the process allocated in all.
func peakHeapBy(f func()) (peak, alloc uint64) {
	const objects = "/memory/classes/heap/objects:bytes"
	read := func() uint64 {
		s := []metrics.Sample{{Name: objects}}
		metrics.Read(s)
		return s[0].Value.Uint64()
	}
	var top atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	alloc = allocatedBy(func() {
		base := read()
		go func() {
			defer close(done)
			tick := time.NewTicker(100 * time.Microsecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					if v := read(); v > top.Load() {
						top.Store(v)
					}
				}
			}
		}()
		f()
		close(stop)
		<-done
		if v := read(); v > top.Load() {
			top.Store(v)
		}
		if t := top.Load(); t > base {
			peak = t - base
		}
	})
	return peak, alloc
}

// allocatedBy returns what the process allocated while f ran.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestMemoryLimitJSONParseLargeArray: a parse of a large array of numbers,
// by JSON.parse and by the host's ParseJSONString, is charged what it
// holds: the elements waiting on the parser's stack until the array closes
// are charged as the stack grows, the capacity it adds and no more. Under a
// 64 MiB limit, 1 million numbers parse and 10 and 30 million stop within
// the limit's margin, and under 512 MiB 20 million parse. What is charged
// is what the parse holds, its stack and the array: twice the array's
// size, and less than the Go heap's growth, which is at most a few times
// the charge (append's 1.25x steps leave garbage until Go collects it).
func TestMemoryLimitJSONParseLargeArray(t *testing.T) {
	for _, c := range []struct {
		limit int64
		n     int
		fits  bool
	}{
		{64 << 20, 1_000_000, true},
		{64 << 20, 10_000_000, false},
		{64 << 20, 30_000_000, false},
		{512 << 20, 20_000_000, true},
	} {
		text := numbersArray(c.n)
		for _, api := range []string{"JSON.parse", "ParseJSONString"} {
			name := fmt.Sprintf("%s of %d numbers under %d MiB", api, c.n, c.limit>>20)
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: c.limit})
			require.NoError(t, rt.SetGlobal("text", text))
			s, err := moejs.CompileScript("parse.js", `JSON.parse(text).length`)
			require.NoError(t, err)
			var v moejs.Value
			peak, alloc := peakHeapBy(func() {
				if api == "JSON.parse" {
					v, err = rt.RunScript(s)
				} else {
					v, err = rt.ParseJSONString(text)
				}
			})
			charged := rt.Stats().AllocatedBytes
			t.Logf("%s: %v; charged %d MiB, Go heap grew by %d MiB at most, Go allocated %d MiB in all", name, err, charged>>20, peak>>20, alloc>>20)
			if c.fits {
				require.NoError(t, err, name)
				if api == "JSON.parse" {
					assert.Equal(t, strconv.Itoa(c.n), v.String(), name)
				}
				held := int64(c.n) * 16 // the array's elements
				assert.GreaterOrEqual(t, charged, 2*held, "%s: charged less than the stack and the array hold", name)
				assert.LessOrEqual(t, charged, 2*held+held/4+(8<<20), "%s: charged more than the stack and the array hold", name)
			} else {
				var mle *moejs.MemoryLimitError
				require.ErrorAs(t, err, &mle, name)
				assert.LessOrEqual(t, mle.Live+mle.Allocated, c.limit+c.limit/2+c.limit, "%s: past the margin", name)
			}
			assert.LessOrEqual(t, uint64(charged), peak+(8<<20), "%s: charged more than the Go heap grew", name)
			assert.Less(t, peak, 4*uint64(charged)+(32<<20), "%s: the Go heap grew past a few times the charge", name)
			runtime.KeepAlive(rt)
		}
	}
}

// TestMemoryLimitJSONParseObjects: a parse of an array of objects stops
// near the limit, as it did when only the objects were charged.
func TestMemoryLimitJSONParseObjects(t *testing.T) {
	const limit = 64 << 20
	text := "[" + strings.Repeat(`{"a":1,"b":"x"},`, 3_000_000) + `{"a":1}]`
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	require.NoError(t, rt.SetGlobal("text", text))
	s, err := moejs.CompileScript("parse.js", `JSON.parse(text).length`)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	t.Logf("3 million objects: %v", err)
	assert.ErrorIs(t, err, moejs.ErrMemoryLimit)
}

// TestMemoryLimitRunEndsWithItsOverage: a run whose last allocation passed
// the limit with nothing after it that checks (the end of the script)
// fails itself: a JSON.parse of 10 million numbers with nothing after it,
// and a host function whose charge passes the margin and which returns as
// if it had not. After the host clears the interrupt and resets the
// budget, as after any hit, the next run starts clean.
func TestMemoryLimitRunEndsWithItsOverage(t *testing.T) {
	const limit = 64 << 20
	text := numbersArray(10_000_000)
	for _, c := range []string{"JSON.parse", "host function"} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		require.NoError(t, rt.SetGlobal("text", text))
		require.NoError(t, rt.SetGlobal("big", moejs.NativeFunc(func(r *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			_ = r.ChargeMemory(4 * limit) // the error ignored
			return moejs.Undefined(), nil
		})))
		src := `JSON.parse(text);`
		if c == "host function" {
			src = `big();`
		}
		s, err := moejs.CompileScript("run.js", src)
		require.NoError(t, err)
		_, err = rt.RunScript(s)
		t.Logf("%s: %v", c, err)
		assert.ErrorIs(t, err, moejs.ErrMemoryLimit, c)
		rt.ClearInterrupt()
		rt.ResetAllocation()
		next, err := moejs.CompileScript("next.js", `1 + 1`)
		require.NoError(t, err)
		v, err := rt.RunScript(next)
		require.NoError(t, err, c)
		assert.Equal(t, "2", v.String(), c)
	}
}

// TestMemoryLimitBuiltinListsCharged: builtins that build a Go list before
// they make the array stop within the limit: split, whose list's size is
// known, throws its RangeError rather than allocate it, and Array.from of
// an iterator charges its list as it grows.
func TestMemoryLimitBuiltinListsCharged(t *testing.T) {
	const limit = 64 << 20
	for _, c := range []struct{ name, src, want string }{
		{"split into characters", `const s = "x".repeat(30e6); try { s.split("").length } catch (e) { e.name }`, "RangeError"},
		{"split at a separator", `const s = "x,".repeat(15e6); try { s.split(",").length } catch (e) { e.name }`, "RangeError"},
		{"Array.from of a generator", `function* g() { for (let i = 0; i < 3e7; i++) yield i; } Array.from(g()).length`, ""},
	} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		s, err := moejs.CompileScript("list.js", c.src)
		require.NoError(t, err)
		var v moejs.Value
		alloc := allocatedBy(func() { v, err = rt.RunScript(s) })
		t.Logf("%s: %v %v; Go allocated %d MiB", c.name, v, err, alloc>>20)
		if c.want != "" {
			require.NoError(t, err, c.name)
			assert.Equal(t, c.want, v.String(), c.name)
			assert.Less(t, alloc, uint64(6*limit), c.name)
		} else {
			// The iterator's results are garbage the limit forgives: what Go
			// allocated in all is no bound here.
			assert.ErrorIs(t, err, moejs.ErrMemoryLimit, c.name)
		}
	}
}

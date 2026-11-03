package bench

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bench/engines"
)

// heapInuse returns the Go heap in use after two full collections.
func heapInuse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

// footprintEngines are the engines the footprint table covers: moejs in
// shared and mutable intrinsics mode plus the baselines.
func footprintEngines() []engines.Engine {
	return []engines.Engine{
		engines.NewMoejsEngine(),
		engines.NewMoejsEngineWith(engines.MoejsOptions{MutableIntrinsics: true, Host: engines.DefaultHost}),
		engines.NewSobekEngine(),
		engines.NewModerncQuickJSEngine(),
		engines.NewQuickJSEngine(),
		engines.NewQuickJSTunedEngine(),
		engines.NewV8Engine(),
	}
}

// rssBytes returns the process's resident set size, or 0 where
// /proc/self/statm is missing.
func rssBytes() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	var size, resident uint64
	if _, err := fmt.Sscan(string(b), &size, &resident); err != nil {
		return 0
	}
	return resident * uint64(os.Getpagesize())
}

// footprintRow measures N live runtimes of one engine with one plugin
// instantiated: Go heap retained per runtime, plus the engine's own heap
// where it lives outside Go (quickjs malloc_size, V8 total/used heap).
// modernc.org/quickjs allocates through its own allocator, which keeps no
// in-use count, so its row adds how much the resident set grew per runtime.
func footprintRow(t *testing.T, e engines.Engine, key string, n int) string {
	runner, err := NewRunner(e, key)
	if err != nil {
		t.Fatal(err)
	}
	before := heapInuse()
	rss0 := rssBytes()
	rts := make([]engines.Runtime, n)
	var cHeap, v8Total, v8Used uint64
	for i := range rts {
		rt, err := runner.Runtime(key)
		if err != nil {
			t.Fatal(err)
		}
		rts[i] = rt
	}
	after := heapInuse()
	rss1 := rssBytes()
	_, modernc := e.(*engines.ModerncQuickJSEngine)
	for _, rt := range rts {
		switch r := rt.(type) {
		case *engines.QuickJSRuntime:
			cHeap += uint64(r.QuickJS().MemoryUsage().MallocSize)
		case *engines.V8Runtime:
			hs := r.Isolate().GetHeapStatistics()
			v8Total += hs.TotalHeapSize
			v8Used += hs.UsedHeapSize
		}
	}
	goPer := (float64(after) - float64(before)) / float64(n) / 1024
	row := fmt.Sprintf("%-22s %-10s N=%-4d Go heap %8.1f KiB/runtime", e.Name(), key, n, goPer)
	if cHeap > 0 {
		row += fmt.Sprintf("   C heap (malloc_size) %8.1f KiB/runtime", float64(cHeap)/float64(n)/1024)
	}
	if modernc && rss0 > 0 {
		row += fmt.Sprintf("   RSS +%8.1f KiB/runtime", (float64(rss1)-float64(rss0))/float64(n)/1024)
	}
	if v8Total > 0 {
		row += fmt.Sprintf("   V8 heap total %8.1f KiB, used %8.1f KiB/isolate", float64(v8Total)/float64(n)/1024, float64(v8Used)/float64(n)/1024)
	}
	runtime.KeepAlive(rts)
	for _, rt := range rts {
		rt.Close()
	}
	return row
}

// TestFootprint prints the retained-memory table: the largest
// plugin (alibaba) at N=64 and N=512 live runtimes for every engine, then
// every plugin at N=64 for the pure-Go engines. FOOTPRINT_N overrides the
// large N (the V8 rows allocate ~1 MiB per isolate).
func TestFootprint(t *testing.T) {
	skipWithoutPlugins(t)
	if testing.Short() {
		t.Skip("footprint measurement is slow")
	}
	large := 512
	if v := os.Getenv("FOOTPRINT_N"); v != "" {
		large, _ = strconv.Atoi(v)
	}
	var rows []string
	for _, n := range []int{64, large} {
		for _, e := range footprintEngines() {
			rows = append(rows, footprintRow(t, e, "alibaba", n))
		}
	}
	for _, key := range PluginKeys {
		for _, e := range footprintEngines()[:4] {
			rows = append(rows, footprintRow(t, e, key, 64))
		}
	}
	t.Logf("footprint (GOMAXPROCS=%d):\n%s", runtime.GOMAXPROCS(0), strings.Join(rows, "\n"))
}

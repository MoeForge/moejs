package bench

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/bench/engines"
)

// ---------------------------------------------------------------- shared

func allCases(tb testing.TB) []HookCase {
	cases, err := LoadCases()
	if err != nil {
		tb.Fatal(err)
	}
	return cases
}

// benchEngines returns the engines the benchmarks compare; BENCH_ENGINES
// (comma-separated names) narrows the set. An unknown name panics, so a
// renamed or removed engine cannot turn a benchmark run into a silent no-op.
func benchEngines() []engines.Engine {
	all := engines.All()
	filter := os.Getenv("BENCH_ENGINES")
	if filter == "" {
		return all
	}
	names := make([]string, len(all))
	for i, e := range all {
		names[i] = e.Name()
	}
	var out []engines.Engine
	for _, name := range splitComma(filter) {
		i := slices.Index(names, name)
		if i < 0 {
			panic(fmt.Sprintf("BENCH_ENGINES: unknown engine %q (known: %s)", name, strings.Join(names, ", ")))
		}
		out = append(out, all[i])
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// pluginRuntimes instantiates one runtime per plugin on the engine.
func pluginRuntimes(tb testing.TB, runner *Runner) map[string]engines.Runtime {
	rts := make(map[string]engines.Runtime, len(PluginKeys))
	for _, key := range PluginKeys {
		rt, err := runner.Runtime(key)
		if err != nil {
			tb.Fatal(err)
		}
		rts[key] = rt
	}
	return rts
}

func closeAll(rts map[string]engines.Runtime) {
	for _, rt := range rts {
		rt.Close()
	}
}

// skipWithoutPlugins skips tb until bench/testdata/plugins/fetch.sh has run.
func skipWithoutPlugins(tb testing.TB) {
	tb.Helper()
	if _, err := PluginSource(PluginKeys[0]); errors.Is(err, ErrPluginsNotFetched) {
		tb.Skip(err)
	}
}

func mustRunner(tb testing.TB, e engines.Engine) *Runner {
	skipWithoutPlugins(tb)
	r, err := NewRunner(e)
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

// ---------------------------------------------------------------- 1. NewRuntime

// BenchmarkNewRuntime measures runtime creation with the host globals
// (utils and console) installed, as new-api's newRuntime does before
// evaluating the module.
func BenchmarkNewRuntime(b *testing.B) {
	for _, e := range benchEngines() {
		b.Run(e.Name(), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				rt, err := e.NewRuntime()
				if err != nil {
					b.Fatal(err)
				}
				rt.Close()
			}
		})
	}
	b.Run("moejs-mutable", func(b *testing.B) {
		e := engines.NewMoejsEngineWith(engines.MoejsOptions{MutableIntrinsics: true, Host: engines.DefaultHost})
		b.ReportAllocs()
		for range b.N {
			rt, err := e.NewRuntime()
			if err != nil {
				b.Fatal(err)
			}
			rt.Close()
		}
	})
}

// ---------------------------------------------------------------- 2. Compile

// BenchmarkCompile measures parse+compile (moejs) / parse+link (Sobek) /
// bytecode compilation (quickjs-go-tuned) of each plugin. Default quickjs-go
// and v8go compile per context; that cost is inside BenchmarkInstantiate for
// them.
func BenchmarkCompile(b *testing.B) {
	skipWithoutPlugins(b)
	for _, e := range benchEngines() {
		if e.Name() == "quickjs-go" || e.Name() == "v8go" {
			continue
		}
		for _, key := range PluginKeys {
			src, err := PluginSource(key)
			if err != nil {
				b.Fatal(err)
			}
			b.Run(e.Name()+"/"+key, func(b *testing.B) {
				b.SetBytes(int64(len(src)))
				b.ReportAllocs()
				for range b.N {
					if _, err := e.Compile(key+".js", src); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------- 3. Instantiate

// BenchmarkInstantiate measures NewRuntime (with host globals) plus module
// instantiation per plugin: what new-api pays to grow a plugin's pool by one
// runtime. For default quickjs-go and v8go this includes compiling the
// script; quickjs-go-tuned loads the bytecode Compile produced.
func BenchmarkInstantiate(b *testing.B) {
	for _, e := range benchEngines() {
		runner := mustRunner(b, e)
		for _, key := range PluginKeys {
			b.Run(e.Name()+"/"+key, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					rt, err := runner.Runtime(key)
					if err != nil {
						b.Fatal(err)
					}
					rt.Close()
				}
			})
		}
	}
}

// ---------------------------------------------------------------- 4. Hook calls

// BenchmarkHook replays every recorded fixture case as its own sub-benchmark
// (<engine>/<plugin>/<case>), validating the result on every iteration. The
// runtime is created inside the sub-benchmark (outside the timer): testing
// runs every b.Run on a new goroutine and default quickjs-go v0.7.7 only
// accepts calls from the goroutine that created the runtime.
func BenchmarkHook(b *testing.B) {
	cases := allCases(b)
	for _, e := range benchEngines() {
		runner := mustRunner(b, e)
		for i := range cases {
			h := &cases[i]
			b.Run(e.Name()+"/"+h.Plugin+"/"+h.Case.Name, func(b *testing.B) {
				rt, err := runner.Runtime(h.Plugin)
				if err != nil {
					b.Fatal(err)
				}
				defer rt.Close()
				benchOneCase(b, e, rt, h)
			})
		}
	}
}

func benchOneCase(b *testing.B, e engines.Engine, rt engines.Runtime, h *HookCase) {
	out, err := rt.Call(h.Case.Hook, h.Case.HookPath(), h.Args...)
	if msg := h.FullCheck(out, err, JSONBoundary(e)); msg != "" && !h.Accepted(e) {
		b.Skipf("result differs from the oracle (see TestDifferentialFixtures): %s", msg)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := rt.Call(h.Case.Hook, h.Case.HookPath(), h.Args...)
		if msg := h.Check(out, err); msg != "" {
			b.Fatal(msg)
		}
	}
}

// BenchmarkHookSuite cycles through all recorded cases of all plugins on one
// runtime per plugin: the aggregate cost of one representative hook call.
func BenchmarkHookSuite(b *testing.B) {
	cases := allCases(b)
	for _, e := range benchEngines() {
		b.Run(e.Name(), func(b *testing.B) {
			runner := mustRunner(b, e)
			rts := pluginRuntimes(b, runner)
			defer closeAll(rts)
			suiteWarmup(b, e, rts, cases)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				h := &cases[i%len(cases)]
				out, err := rts[h.Plugin].Call(h.Case.Hook, h.Case.HookPath(), h.Args...)
				if msg := h.Check(out, err); msg != "" {
					b.Fatalf("%s/%s: %s", h.Plugin, h.Case.Name, msg)
				}
			}
		})
		if _, ok := e.(*engines.MoejsEngine); ok {
			b.Run(e.Name()+"+release", func(b *testing.B) { hookSuiteRelease(b, e, cases) })
		}
	}
}

// hookSuiteRelease is BenchmarkHookSuite with ReleaseCallData after each
// call, as a host that pools runtimes runs one hook per request.
func hookSuiteRelease(b *testing.B, e engines.Engine, cases []HookCase) {
	runner := mustRunner(b, e)
	rts := pluginRuntimes(b, runner)
	defer closeAll(rts)
	suiteWarmup(b, e, rts, cases)
	native := make(map[string]*moejs.Runtime, len(rts))
	for key, rt := range rts {
		native[key] = rt.(*engines.MoejsRuntime).Moejs()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		h := &cases[i%len(cases)]
		out, err := rts[h.Plugin].Call(h.Case.Hook, h.Case.HookPath(), h.Args...)
		if msg := h.Check(out, err); msg != "" {
			b.Fatalf("%s/%s: %s", h.Plugin, h.Case.Name, msg)
		}
		native[h.Plugin].ReleaseCallData()
	}
}

// suiteWarmup runs every case once with the full comparison so the suite
// only times cases the engine gets right (mismatches are reported by
// TestDifferentialFixtures, not hidden in a benchmark).
func suiteWarmup(b *testing.B, e engines.Engine, rts map[string]engines.Runtime, cases []HookCase) {
	for i := range cases {
		h := &cases[i]
		out, err := rts[h.Plugin].Call(h.Case.Hook, h.Case.HookPath(), h.Args...)
		if msg := h.FullCheck(out, err, JSONBoundary(e)); msg != "" && !h.Accepted(e) {
			b.Fatalf("%s/%s differs from the oracle: %s", h.Plugin, h.Case.Name, msg)
		}
	}
}

// BenchmarkHookPhases splits one hook call into its host-conversion and
// interpretation phases on the engines that expose them (moejs, sobek):
// fromgo (arguments only), call (pre-converted arguments, raw result
// discarded), togo (export of a pre-computed raw result) and total. Cases:
// the first decodeRequest and buildSubmitRequest of each plugin
// (BENCH_PHASES_ALL=1 runs every case).
func BenchmarkHookPhases(b *testing.B) {
	cases := allCases(b)
	selected := phaseCases(cases)
	for _, e := range benchEngines() {
		runner := mustRunner(b, e)
		probe, err := runner.Runtime(PluginKeys[0])
		if err != nil {
			b.Fatal(err)
		}
		_, phased := probe.(engines.Phased)
		probe.Close()
		if !phased {
			continue
		}
		rts := pluginRuntimes(b, runner)
		for _, h := range selected {
			rt := rts[h.Plugin].(engines.Phased)
			name := e.Name() + "/" + h.Plugin + "/" + h.Case.Name
			b.Run(name+"/fromgo", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if _, err := rt.Prepare(h.Args...); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(name+"/call", func(b *testing.B) {
				prepared, err := rt.Prepare(h.Args...)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					raw, err := rt.CallPrepared(h.Case.Hook, h.Case.HookPath(), prepared)
					if h.Case.ExpectedError == "" && (err != nil || raw == nil) {
						b.Fatal(err)
					}
					if h.Case.ExpectedError != "" && err == nil {
						b.Fatal("expected throw")
					}
				}
			})
			if h.Case.ExpectedError == "" {
				b.Run(name+"/togo", func(b *testing.B) {
					prepared, err := rt.Prepare(h.Args...)
					if err != nil {
						b.Fatal(err)
					}
					raw, err := rt.CallPrepared(h.Case.Hook, h.Case.HookPath(), prepared)
					if err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if msg := h.Check(rt.Export(raw), nil); msg != "" {
							b.Fatal(msg)
						}
					}
				})
			}
			b.Run(name+"/total", func(b *testing.B) {
				benchOneCase(b, e, rts[h.Plugin], h)
			})
		}
		closeAll(rts)
	}
}

func phaseCases(cases []HookCase) []*HookCase {
	if os.Getenv("BENCH_PHASES_ALL") != "" {
		out := make([]*HookCase, len(cases))
		for i := range cases {
			out[i] = &cases[i]
		}
		return out
	}
	var out []*HookCase
	seen := map[string]bool{}
	for i := range cases {
		h := &cases[i]
		hook := h.Case.HookName()
		family := ""
		switch {
		case hook == "protocols.openai_responses.decodeRequest" && h.Case.ExpectedError == "":
			family = "decode"
		case hook == "buildSubmitRequest" && h.Case.ExpectedError == "":
			family = "build"
		case hook == "parseTaskResult" && h.Case.ExpectedError == "":
			family = "parse"
		case h.Case.ExpectedError != "" && hook == "protocols.openai_responses.decodeRequest":
			family = "throw"
		}
		if family == "" || seen[h.Plugin+family] {
			continue
		}
		seen[h.Plugin+family] = true
		out = append(out, h)
	}
	return out
}

// ---------------------------------------------------------------- 5. Parallel

// gcConfig is one garbage-collector configuration of the parallel suite.
type gcConfig struct {
	name    string
	percent int // GOGC value; 0 keeps the process default
	ballast int // bytes of pointer-free heap kept alive for the run
}

var gcConfigs = []gcConfig{
	{name: "gogc=default"},
	{name: "gogc=400", percent: 400},
	{name: "ballast=256MiB", ballast: 256 << 20},
}

// BenchmarkHookSuiteParallel runs the hook suite from every benchmark
// goroutine (-cpu N), each goroutine owning one pooled runtime per plugin as
// new-api does with sync.Pool. Every Go engine runs under three GC
// configurations: the process default GOGC, GOGC=400, and a 256 MiB ballast
// that paces the collector as a server-sized live heap would. The cgo engines
// are not paced by the Go collector and run the default configuration only.
// Reported metrics: p50-ns and p99-ns per call, hooks/s, gc-cpu-pct (share of
// the process CPU spent in GC over the timed section) and gc-cycles.
func BenchmarkHookSuiteParallel(b *testing.B) {
	cases := allCases(b)
	for _, e := range benchEngines() {
		if e.Name() == "v8go" && os.Getenv("V8_PARALLEL") == "" {
			b.Run(e.Name(), func(b *testing.B) { b.Skip("set V8_PARALLEL=1 to run isolates from several goroutines") })
			continue
		}
		configs := gcConfigs
		if engines.Cgo(e) {
			configs = gcConfigs[:1]
		}
		for _, cfg := range configs {
			b.Run(e.Name()+"/"+cfg.name, func(b *testing.B) {
				runParallelSuite(b, e, cases, cfg)
			})
		}
	}
}

// pluginPool hands out one instantiated runtime per plugin per goroutine.
type pluginPool struct {
	runner *Runner
	pools  map[string]*sync.Pool
}

func newPluginPool(b *testing.B, runner *Runner) *pluginPool {
	p := &pluginPool{runner: runner, pools: make(map[string]*sync.Pool, len(PluginKeys))}
	for _, key := range PluginKeys {
		p.pools[key] = &sync.Pool{New: func() any {
			rt, err := runner.Runtime(key)
			if err != nil {
				b.Fatal(err)
			}
			return rt
		}}
	}
	return p
}

func runParallelSuite(b *testing.B, e engines.Engine, cases []HookCase, cfg gcConfig) {
	if cfg.percent > 0 {
		old := debug.SetGCPercent(cfg.percent)
		defer debug.SetGCPercent(old)
	}
	var ballast []byte
	if cfg.ballast > 0 {
		ballast = make([]byte, cfg.ballast)
		defer runtime.KeepAlive(ballast)
	}
	runtime.GC()
	runner := mustRunner(b, e)
	// One warm runtime set validates every case once against the oracle.
	warm := pluginRuntimes(b, runner)
	suiteWarmup(b, e, warm, cases)
	closeAll(warm)
	pool := newPluginPool(b, runner)
	// Per-goroutine latency buffers and case cursors: no shared counter and
	// no adjacent writes to one slice, so the harness adds no cache-line
	// contention of its own. Goroutine k starts at case k and every goroutine
	// walks the whole suite, so all cases are exercised at every -cpu.
	var mu sync.Mutex
	var durations []int64
	var goroutines atomic.Int64
	gcBefore := readGCSample()
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		// Each goroutine takes one runtime per plugin from the pools for the
		// whole run, as a new-api worker holding pooled instances would.
		local := make(map[string]engines.Runtime, len(PluginKeys))
		for key, p := range pool.pools {
			local[key] = p.Get().(engines.Runtime)
		}
		defer func() {
			for key, rt := range local {
				pool.pools[key].Put(rt)
			}
		}()
		mine := make([]int64, 0, 1<<16)
		cursor := int(goroutines.Add(1)-1) % len(cases)
		for pb.Next() {
			h := &cases[cursor]
			cursor++
			if cursor == len(cases) {
				cursor = 0
			}
			t0 := time.Now()
			out, err := local[h.Plugin].Call(h.Case.Hook, h.Case.HookPath(), h.Args...)
			mine = append(mine, int64(time.Since(t0)))
			if msg := h.Check(out, err); msg != "" {
				b.Fatalf("%s/%s: %s", h.Plugin, h.Case.Name, msg)
			}
		}
		mu.Lock()
		durations = append(durations, mine...)
		mu.Unlock()
	})
	elapsed := time.Since(start)
	b.StopTimer()
	gcAfter := readGCSample()
	reportLatency(b, durations)
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "hooks/s")
	reportGC(b, gcBefore, gcAfter)
	if ballast != nil {
		b.ReportMetric(float64(len(ballast))/(1<<20), "ballast-MiB")
	}
}

func reportLatency(b *testing.B, durations []int64) {
	if len(durations) == 0 {
		return
	}
	sorted := make([]int64, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	b.ReportMetric(float64(sorted[len(sorted)*50/100]), "p50-ns")
	b.ReportMetric(float64(sorted[min(len(sorted)*99/100, len(sorted)-1)]), "p99-ns")
}

// gcSample is the GC accounting read around a timed section.
type gcSample struct {
	gcCPU, totalCPU float64
	cycles          uint64
}

var gcMetricNames = []string{"/cpu/classes/gc/total:cpu-seconds", "/cpu/classes/total:cpu-seconds", "/gc/cycles/total:gc-cycles"}

func readGCSample() gcSample {
	samples := make([]metrics.Sample, len(gcMetricNames))
	for i, n := range gcMetricNames {
		samples[i].Name = n
	}
	metrics.Read(samples)
	s := gcSample{}
	if samples[0].Value.Kind() == metrics.KindFloat64 {
		s.gcCPU = samples[0].Value.Float64()
	}
	if samples[1].Value.Kind() == metrics.KindFloat64 {
		s.totalCPU = samples[1].Value.Float64()
	}
	if samples[2].Value.Kind() == metrics.KindUint64 {
		s.cycles = samples[2].Value.Uint64()
	}
	return s
}

func reportGC(b *testing.B, before, after gcSample) {
	total := after.totalCPU - before.totalCPU
	if total > 0 {
		b.ReportMetric(100*(after.gcCPU-before.gcCPU)/total, "gc-cpu-pct")
	}
	b.ReportMetric(float64(after.cycles-before.cycles), "gc-cycles")
}

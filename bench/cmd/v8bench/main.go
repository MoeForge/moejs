// Command v8bench runs the V8 benchmark suite, version 7, on moejs.
//
// The suite is not in the repository: bench/v8suite/fetch.sh downloads the
// pinned files into ~/.cache/moejs/v8-v7 (or $MOEJS_V8_DIR).
//
//	bench/v8suite/fetch.sh
//	cd bench && go run ./cmd/v8bench score
//	go run ./cmd/v8bench -only richards,splay score
//	go run ./cmd/v8bench -bench Richards -n 200 run
//
// score runs the suite as its run.js does: each benchmark runs for a second
// to warm up, then for at least a second and 32 iterations, and the score is
// the suite's own, the geometric mean over the reference times.
//
// run does a fixed amount of work for instruction counts and profiles: it
// loads base.js and one benchmark's file, runs the benchmark's setup, then
// -warm and -n iterations, then its teardown. Only the -n iterations are
// timed. Two runs at two -n counted by an outside counter differ only by
// the extra iterations, so loading and setup cancel out.
//
// Runtimes have their own mutable intrinsics (Options.MutableIntrinsics),
// as in the cross-engine harness; -shared uses the frozen shared ones.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
)

// files are the benchmark files in run.js's order, after base.js.
var files = []string{"richards", "deltablue", "crypto", "raytrace", "earley-boyer", "regexp", "splay", "navier-stokes"}

// suiteFile maps the benchmark names a suite declares to their file.
var suiteFile = map[string]string{
	"Richards": "richards", "DeltaBlue": "deltablue", "Crypto": "crypto",
	"Encrypt": "crypto", "Decrypt": "crypto", "RayTrace": "raytrace",
	"EarleyBoyer": "earley-boyer", "Earley": "earley-boyer", "Boyer": "earley-boyer",
	"RegExp": "regexp", "Splay": "splay", "NavierStokes": "navier-stokes",
}

func main() {
	dir := flag.String("dir", defaultDir(), "the suite's files (bench/v8suite/fetch.sh)")
	only := flag.String("only", "", "score: comma-separated files to run, without .js (richards,splay)")
	bench := flag.String("bench", "", "run: a benchmark (Richards, Encrypt, Earley) or a suite (Crypto) to run each of its benchmarks")
	n := flag.Int("n", 100, "run: timed iterations")
	warm := flag.Int("warm", 0, "run: iterations before the timed ones")
	shared := flag.Bool("shared", false, "use the frozen shared intrinsics instead of a mutable copy")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile of the timed part to this file")
	asJSON := flag.Bool("json", false, "print the result as JSON")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: v8bench [flags] score|run")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: !*shared})
	if *cpuprofile != "" {
		startProfile = func() {
			f, err := os.Create(*cpuprofile)
			check(err)
			check(pprof.StartCPUProfile(f))
			stopProfile = func() { pprof.StopCPUProfile(); check(f.Close()) }
		}
	}
	switch flag.Arg(0) {
	case "score":
		var sel []string
		if *only != "" {
			sel = strings.Split(*only, ",")
			for _, f := range sel {
				if !slices.Contains(files, f) {
					fatal(fmt.Errorf("no benchmark file %s.js", f))
				}
			}
		}
		res := score(rt, *dir, sel)
		stopProfile()
		res.print(*asJSON)
	case "run":
		f, ok := suiteFile[*bench]
		if !ok {
			fatal(fmt.Errorf("no benchmark %q", *bench))
		}
		res := fixed(rt, *dir, f, *bench, *warm, *n)
		stopProfile()
		res.print(*asJSON)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// startProfile starts the CPU profile, if any, before the timed part.
var startProfile, stopProfile = func() {}, func() {}

func defaultDir() string {
	if d := os.Getenv("MOEJS_V8_DIR"); d != "" {
		return d
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "v8-v7"
	}
	return filepath.Join(h, ".cache", "moejs", "v8-v7")
}

func load(rt *moejs.Runtime, dir, file string) {
	src, err := os.ReadFile(filepath.Join(dir, file+".js"))
	if err != nil {
		fatal(fmt.Errorf("%w (fetch the suite with bench/v8suite/fetch.sh)", err))
	}
	eval(rt, file+".js", string(src))
}

func eval(rt *moejs.Runtime, name, src string) moejs.Value {
	s, err := moejs.CompileScript(name, src)
	check(err)
	v, err := rt.RunScript(s)
	check(err)
	return v
}

type scoreResult struct {
	Scores map[string]string `json:"scores"` // suite -> score, or "error: ..."
	WallS  float64           `json:"wall_s"`
	order  []string
}

// score runs the suites of sel (every file when empty) as run.js does.
func score(rt *moejs.Runtime, dir string, sel []string) scoreResult {
	if len(sel) == 0 {
		sel = files
	}
	load(rt, dir, "base")
	for _, f := range files {
		if slices.Contains(sel, f) {
			load(rt, dir, f)
		}
	}
	startProfile()
	start := time.Now()
	out := eval(rt, "v8bench-score.js", `(function () {
  var res = {}, order = [];
  BenchmarkSuite.RunSuites({
    NotifyResult: function (name, result) { res[name] = String(result); order.push(name); },
    NotifyError: function (name, error) { res[name] = "error: " + error; order.push(name); },
    NotifyScore: function (score) { res.Score = String(score); order.push("Score"); }
  });
  return JSON.stringify({ scores: res, order: order });
})()`)
	r := scoreResult{WallS: time.Since(start).Seconds()}
	var raw struct {
		Scores map[string]string `json:"scores"`
		Order  []string          `json:"order"`
	}
	check(json.Unmarshal([]byte(out.String()), &raw))
	r.Scores, r.order = raw.Scores, raw.Order
	return r
}

func (r scoreResult) print(asJSON bool) {
	if asJSON {
		b, err := json.Marshal(r)
		check(err)
		fmt.Println(string(b))
		return
	}
	for _, k := range r.order {
		fmt.Printf("%s: %s\n", k, r.Scores[k])
	}
	fmt.Printf("wall: %.1f s\n", r.WallS)
}

type runResult struct {
	Bench     string  `json:"bench"`
	N         int     `json:"n"`
	NsPerIter float64 `json:"ns_per_iter"`
	BytesPer  float64 `json:"bytes_per_iter"`
	AllocsPer float64 `json:"allocs_per_iter"`
	GCs       uint32  `json:"gcs"`
	TotalS    float64 `json:"total_s"`
}

// fixed runs -warm then n timed iterations of the benchmark named bench (or
// of each benchmark of the suite named bench) after its setup.
func fixed(rt *moejs.Runtime, dir, file, bench string, warm, n int) runResult {
	load(rt, dir, "base")
	load(rt, dir, file)
	q := strconv.Quote(bench)
	eval(rt, "v8bench-setup.js", `var __v8b = [];
BenchmarkSuite.suites.forEach(function (s) {
  s.benchmarks.forEach(function (b, k) {
    if (s.name !== `+q+` && b.name !== `+q+`) return;
    // A benchmark may need what the suite's earlier ones leave behind,
    // as Decrypt needs Encrypt's result: run them once first, in order.
    if (s.name !== `+q+`) for (var i = 0; i < k; i++) s.benchmarks[i].run();
    __v8b.push(b);
  });
});
if (__v8b.length === 0) throw new Error("no benchmark " + `+q+`);
__v8b.forEach(function (b) { b.Setup(); });
function __v8run(n) {
  for (var i = 0; i < n; i++)
    for (var j = 0; j < __v8b.length; j++) __v8b[j].run();
}`)
	if warm > 0 {
		eval(rt, "v8bench-warm.js", "__v8run("+strconv.Itoa(warm)+")")
	}
	loop, err := moejs.CompileScript("v8bench-run.js", "__v8run("+strconv.Itoa(n)+")")
	check(err)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	startProfile()
	start := time.Now()
	_, err = rt.RunScript(loop)
	d := time.Since(start)
	check(err)
	runtime.ReadMemStats(&m1)
	eval(rt, "v8bench-teardown.js", "__v8b.forEach(function (b) { b.TearDown(); })")
	fn := float64(max(n, 1))
	return runResult{
		Bench: bench, N: n, TotalS: d.Seconds(),
		NsPerIter: float64(d.Nanoseconds()) / fn,
		BytesPer:  float64(m1.TotalAlloc-m0.TotalAlloc) / fn,
		AllocsPer: float64(m1.Mallocs-m0.Mallocs) / fn,
		GCs:       m1.NumGC - m0.NumGC,
	}
}

func (r runResult) print(asJSON bool) {
	if asJSON {
		b, err := json.Marshal(r)
		check(err)
		fmt.Println(string(b))
		return
	}
	fmt.Printf("%s n=%d %.0f ns/iter %.0f B/iter %.0f allocs/iter gcs=%d total=%.2fs\n",
		r.Bench, r.N, r.NsPerIter, r.BytesPer, r.AllocsPer, r.GCs, r.TotalS)
}

func check(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "v8bench:", err)
	os.Exit(1)
}

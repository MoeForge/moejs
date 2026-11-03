// Command pluginbench times the new-api plugin scenarios on the Go-hosted
// engines. Each worker owns one runtime that has loaded plugins.js and runs
// requests back to back. A request is one or more hook calls; each call parses
// its argument array from JSON bytes inside the engine, calls __call, and
// serializes the result back to bytes the host owns.
//
// The scenarios are made by bench/scenarios/gen.py, into
// bench/testdata/scenarios, which pluginbench finds from the working
// directory up:
//
//	bench/testdata/plugins/fetch.sh
//	python3 bench/scenarios/gen.py
//	cd bench && go run ./cmd/pluginbench -engine moejs -scenario agent100k -workers 8
//
// bench/scripts/plugin_ab.sh runs them against two moejs checkouts.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/MoeForge/moejs"
	qjsgo "github.com/buke/quickjs-go"
	"github.com/grafana/sobek"
	mqjs "modernc.org/quickjs"
)

type call struct {
	hook int
	args []byte
}

type request []call

func loadScenario(p string) ([]request, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		return strings.TrimSuffix(s, "\n"), err
	}
	s, err := line()
	if err != nil {
		return nil, err
	}
	n, _ := strconv.Atoi(s)
	reqs := make([]request, n)
	for i := range reqs {
		s, err = line()
		if err != nil {
			return nil, err
		}
		nc, _ := strconv.Atoi(s)
		for range nc {
			s, err = line()
			if err != nil {
				return nil, err
			}
			var c call
			var size int
			if _, err := fmt.Sscanf(s, "%d %d", &c.hook, &size); err != nil {
				return nil, err
			}
			c.args = make([]byte, size+1)
			if _, err := io.ReadFull(r, c.args); err != nil {
				return nil, err
			}
			c.args = c.args[:size]
			reqs[i] = append(reqs[i], c)
		}
	}
	return reqs, nil
}

// worker runs calls on its own runtime. run returns the result as JSON bytes,
// valid until the next run. done ends a request.
type worker interface {
	run(c call, last bool) ([]byte, error)
	done()
}

type factory func() (worker, error)

// moejs: ParseJSON, Call, AppendJSON into a reused buffer, ReleaseCallData per request.
func moejsFactory(src string, noCopy bool) (factory, error) {
	mod, err := moejs.Compile("plugins.mjs", src)
	if err != nil {
		return nil, err
	}
	h, err := mod.Hook("__call")
	if err != nil {
		return nil, err
	}
	return func() (worker, error) {
		rt := moejs.NewRuntime(moejs.Options{})
		if err := rt.Load(mod); err != nil {
			return nil, err
		}
		return &moejsWorker{rt: rt, hook: h, noCopy: noCopy}, nil
	}, nil
}

type moejsWorker struct {
	rt     *moejs.Runtime
	hook   moejs.Hook
	out    []byte
	noCopy bool
}

func (w *moejsWorker) run(c call, last bool) ([]byte, error) {
	var a moejs.Value
	var err error
	if w.noCopy {
		// The scenario bytes are never modified, so the text may be shared
		// as ParseJSONString's documentation allows.
		a, err = w.rt.ParseJSONString(unsafe.String(unsafe.SliceData(c.args), len(c.args)))
	} else {
		a, err = w.rt.ParseJSON(c.args)
	}
	if err != nil {
		return nil, err
	}
	r, err := w.rt.Call(w.hook, moejs.Int(int64(c.hook)), a)
	if err != nil {
		return nil, err
	}
	w.out, err = w.rt.AppendJSON(w.out[:0], r)
	if last {
		parkPoint()
	}
	return w.out, err
}

func (w *moejsWorker) done() { w.rt.ReleaseCallData() }

// Sobek: JSON.parse and JSON.stringify called from Go.
func sobekFactory(src string) (factory, error) {
	prog, err := sobek.Compile("plugins.js", src, true)
	if err != nil {
		return nil, err
	}
	return func() (worker, error) {
		vm := sobek.New()
		if _, err := vm.RunProgram(prog); err != nil {
			return nil, err
		}
		fn, _ := sobek.AssertFunction(vm.Get("__call"))
		j := vm.Get("JSON").ToObject(vm)
		parse, _ := sobek.AssertFunction(j.Get("parse"))
		stringify, _ := sobek.AssertFunction(j.Get("stringify"))
		return &sobekWorker{vm: vm, call: fn, parse: parse, stringify: stringify}, nil
	}, nil
}

type sobekWorker struct {
	vm                     *sobek.Runtime
	call, parse, stringify sobek.Callable
	out                    []byte
}

func (w *sobekWorker) run(c call, last bool) ([]byte, error) {
	a, err := w.parse(sobek.Undefined(), w.vm.ToValue(string(c.args)))
	if err != nil {
		return nil, err
	}
	r, err := w.call(sobek.Undefined(), w.vm.ToValue(c.hook), a)
	if err != nil {
		return nil, err
	}
	s, err := w.stringify(sobek.Undefined(), r)
	if err != nil {
		return nil, err
	}
	w.out = append(w.out[:0], s.String()...)
	if last {
		parkPoint()
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(r)
	runtime.KeepAlive(s)
	return w.out, nil
}

func (w *sobekWorker) done() {}

// quickjs-go (cgo): Context.ParseJSON, Value.Execute, then JSON.stringify.
func quickjsGoFactory(src string, ownerCheck bool) (factory, error) {
	return func() (worker, error) {
		rt := qjsgo.NewRuntime(qjsgo.WithOwnerGoroutineCheck(ownerCheck))
		ctx := rt.NewContext()
		res := ctx.Eval(src, qjsgo.EvalFileName("plugins.js"), qjsgo.EvalFlagStrict(true))
		defer res.Free()
		if res.IsException() {
			return nil, ctx.Exception()
		}
		g := ctx.Globals()
		j := g.Get("JSON")
		defer j.Free()
		return &quickjsGoWorker{ctx: ctx, call: g.Get("__call"), stringify: j.Get("stringify")}, nil
	}, nil
}

type quickjsGoWorker struct {
	ctx             *qjsgo.Context
	call, stringify *qjsgo.Value
	out             []byte
}

func (w *quickjsGoWorker) run(c call, last bool) ([]byte, error) {
	a := w.ctx.ParseJSON(string(c.args))
	defer a.Free()
	if a.IsException() {
		return nil, w.ctx.Exception()
	}
	i := w.ctx.NewInt32(int32(c.hook))
	defer i.Free()
	r := w.call.Execute(w.ctx.NewUndefined(), i, a)
	defer r.Free()
	if r.IsException() {
		return nil, w.ctx.Exception()
	}
	// Value.JSONStringify in v0.7.7 never frees the string JS_JSONStringify
	// returns, so the result of every call leaks. Calling JSON.stringify and
	// freeing its result does the same work without the leak.
	str := w.stringify.Execute(w.ctx.NewUndefined(), r)
	defer str.Free()
	if str.IsException() {
		return nil, w.ctx.Exception()
	}
	w.out = append(w.out[:0], str.ToString()...)
	if last {
		parkPoint()
	}
	return w.out, nil
}

func (w *quickjsGoWorker) done() {}

// modernc.org/quickjs (QuickJS translated to Go): it has no JSON API, so the
// argument goes in as a string and __callJSON parses and serializes in JS.
func modernFactory(src string) (factory, error) {
	return func() (worker, error) {
		vm, err := mqjs.NewVM()
		if err != nil {
			return nil, err
		}
		if _, err := vm.Eval(src, mqjs.EvalGlobal); err != nil {
			return nil, err
		}
		fn, err := vm.EvalValue("__callJSON", mqjs.EvalGlobal)
		if err != nil {
			return nil, err
		}
		// __callJSONPark is __callJSON stopping at parkPoint after
		// JSON.stringify, while the parsed argument, the result and its text
		// are all still referenced.
		if err := vm.RegisterFunc("__park", func() { parkPoint() }, false); err != nil {
			return nil, err
		}
		if _, err := vm.Eval("function __callJSONPark(i, t) { var a = JSON.parse(t), r = __call(i, a), s = JSON.stringify(r); __park(); return s; }", mqjs.EvalGlobal); err != nil {
			return nil, err
		}
		park, err := vm.EvalValue("__callJSONPark", mqjs.EvalGlobal)
		if err != nil {
			return nil, err
		}
		return &modernWorker{fn: fn, park: park}, nil
	}, nil
}

type modernWorker struct {
	fn, park mqjs.Value
	out      []byte
}

func (w *modernWorker) run(c call, last bool) ([]byte, error) {
	fn := w.fn
	if last && inuseSamples > 0 {
		fn = w.park
	}
	r, err := fn.Call(mqjs.Value{}, c.hook, string(c.args))
	if err != nil {
		return nil, err
	}
	s, ok := r.(string)
	if !ok {
		return nil, fmt.Errorf("modernc: result %T", r)
	}
	w.out = append(w.out[:0], s...)
	return w.out, nil
}

func (w *modernWorker) done() {}

func newFactory(engine, js, mjs string) (factory, error) {
	switch engine {
	case "moejs":
		return moejsFactory(mjs, false)
	case "moejs-nocopy":
		return moejsFactory(mjs, true)
	case "sobek":
		return sobekFactory(js)
	case "quickjs-go":
		return quickjsGoFactory(js, true)
	case "quickjs-go-nocheck":
		return quickjsGoFactory(js, false)
	case "modernc":
		return modernFactory(js)
	}
	return nil, fmt.Errorf("unknown engine %q", engine)
}

// check runs every request once and writes each call's result.
func check(f factory, reqs []request, out string) error {
	w, err := f()
	if err != nil {
		return err
	}
	file, err := os.Create(out)
	if err != nil {
		return err
	}
	defer file.Close()
	bw := bufio.NewWriter(file)
	for _, req := range reqs {
		for _, c := range req {
			b, err := w.run(c, false)
			if err != nil {
				return fmt.Errorf("hook %d: %w", c.hook, err)
			}
			fmt.Fprintf(bw, "%d %d\n", c.hook, len(b))
			bw.Write(b)
			bw.WriteByte('\n')
		}
		w.done()
	}
	return bw.Flush()
}

type result struct {
	Engine   string  `json:"engine"`
	Scenario string  `json:"scenario"`
	Workers  int     `json:"workers"`
	Requests int     `json:"requests"`
	Seconds  float64 `json:"seconds"`
	RPS      float64 `json:"rps"`
	P50ms    float64 `json:"p50_ms"`
	P99ms    float64 `json:"p99_ms"`
	MaxMs    float64 `json:"max_ms"`
	HWMMiB   float64 `json:"hwm_mib"`
	NumGC    uint32  `json:"num_gc"`
	// Filled with -mem: peaks of the Go heap while measuring, and bytes
	// allocated per request.
	LiveMiB    float64 `json:"live_mib,omitempty"`
	GoalMiB    float64 `json:"goal_mib,omitempty"`
	ObjectsMiB float64 `json:"objects_mib,omitempty"`
	AllocMiB   float64 `json:"alloc_mib_per_req,omitempty"`
}

var memFlag bool

var memProfile string

var memNames = []string{"/gc/heap/live:bytes", "/gc/heap/goal:bytes", "/memory/classes/heap/objects:bytes", "/gc/heap/allocs:bytes"}

func readMem(s []metrics.Sample) []uint64 {
	metrics.Read(s)
	v := make([]uint64, len(s))
	for i := range s {
		v[i] = s[i].Value.Uint64()
	}
	return v
}

func bench(f factory, reqs []request, workers int, warm, dur time.Duration) (result, error) {
	var phase atomic.Int32 // 0 warm-up, 1 measuring, 2 stop
	lat := make([][]time.Duration, workers)
	errs := make(chan error, workers)
	var wg, ready sync.WaitGroup
	ready.Add(workers)
	for id := range workers {
		wg.Go(func() {
			// Each worker makes its runtime on its own goroutine: quickjs-go
			// binds a runtime to the first goroutine that uses it.
			w, err := f()
			ready.Done()
			if err != nil {
				errs <- err
				phase.Store(2)
				return
			}
			i := id * 7919 % len(reqs)
			for {
				t0 := time.Now()
				for j, c := range reqs[i] {
					if _, err := w.run(c, j == len(reqs[i])-1); err != nil {
						errs <- err
						phase.Store(2)
						return
					}
				}
				w.done()
				d := time.Since(t0)
				switch phase.Load() {
				case 1:
					lat[id] = append(lat[id], d)
				case 2:
					return
				}
				if i++; i == len(reqs) {
					i = 0
				}
			}
		})
	}
	ready.Wait()
	time.Sleep(warm)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gc0 := ms.NumGC
	samples := make([]metrics.Sample, len(memNames))
	for i, n := range memNames {
		samples[i].Name = n
	}
	var a0 uint64
	peak := make([]uint64, 3)
	stopMem := make(chan struct{})
	memDone := make(chan struct{})
	if memFlag {
		a0 = readMem(samples)[3]
		go func() {
			defer close(memDone)
			t := time.NewTicker(2 * time.Millisecond)
			defer t.Stop()
			s := make([]metrics.Sample, len(samples))
			copy(s, samples)
			for {
				select {
				case <-stopMem:
					return
				case <-t.C:
					v := readMem(s)
					for i := range peak {
						peak[i] = max(peak[i], v[i])
					}
				}
			}
		}()
	} else {
		close(memDone)
	}
	phase.Store(1)
	t0 := time.Now()
	time.Sleep(dur)
	if memProfile != "" {
		// The heap profile reflects the objects live at the last GC.
		if f, err := os.Create(memProfile); err == nil {
			pprof.Lookup("heap").WriteTo(f, 0)
			f.Close()
		}
	}
	phase.Store(2)
	elapsed := time.Since(t0)
	var a1 uint64
	if memFlag {
		close(stopMem)
		<-memDone
		a1 = readMem(samples)[3]
	}
	wg.Wait()
	select {
	case err := <-errs:
		return result{}, err
	default:
	}
	runtime.ReadMemStats(&ms)
	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	slices.Sort(all)
	q := func(p float64) float64 {
		if len(all) == 0 {
			return 0
		}
		return float64(all[min(len(all)-1, int(p*float64(len(all))))]) / 1e6
	}
	mib := func(b uint64) float64 { return float64(b) / (1 << 20) }
	var allocPerReq float64
	if memFlag && len(all) > 0 {
		allocPerReq = mib(a1-a0) / float64(len(all))
	}
	return result{
		LiveMiB:    mib(peak[0]),
		GoalMiB:    mib(peak[1]),
		ObjectsMiB: mib(peak[2]),
		AllocMiB:   allocPerReq,
		Workers:    workers,
		Requests:   len(all),
		Seconds:    elapsed.Seconds(),
		RPS:        float64(len(all)) / elapsed.Seconds(),
		P50ms:      q(0.50),
		P99ms:      q(0.99),
		MaxMs:      q(1),
		HWMMiB:     hwm(),
		NumGC:      ms.NumGC - gc0,
	}, nil
}

// hwm is the process's peak resident set size.
func hwm() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		// macOS reports Maxrss in bytes.
		var u syscall.Rusage
		if syscall.Getrusage(syscall.RUSAGE_SELF, &u) != nil {
			return 0
		}
		return float64(u.Maxrss) / (1 << 20)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(l, "VmHWM:"); ok {
			kb, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")))
			return float64(kb) / 1024
		}
	}
	return 0
}

// scenarioDir is the first testdata/scenarios or bench/testdata/scenarios
// holding plugins.mjs from the working directory up, or "." when there is
// none.
func scenarioDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		for _, d := range []string{filepath.Join(dir, "testdata", "scenarios"), filepath.Join(dir, "bench", "testdata", "scenarios")} {
			if _, err := os.Stat(filepath.Join(d, "plugins.mjs")); err == nil {
				return d
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func main() {
	engine := flag.String("engine", "moejs", "moejs, moejs-nocopy, sobek, quickjs-go, quickjs-go-nocheck or modernc")
	scn := flag.String("scenario", "small", "scenario name under -dir/scn: small, agent100k, big8m or multi8x1m")
	dir := flag.String("dir", scenarioDir(), "directory with plugins.js, plugins.mjs and scn/, which bench/scenarios/gen.py writes")
	workers := flag.Int("workers", 1, "concurrent workers, one runtime each")
	warm := flag.Duration("warm", 2*time.Second, "warm-up")
	dur := flag.Duration("dur", 6*time.Second, "measured time")
	checkOut := flag.String("check", "", "run every request once and write the results here")
	flag.BoolVar(&memFlag, "mem", false, "sample Go heap metrics while measuring")
	flag.StringVar(&memProfile, "memprofile", "", "write a heap profile at the end of measuring")
	flag.IntVar(&inuseSamples, "inuse", 0, "stop every worker this many times and measure the bytes still referenced")
	flag.Parse()

	js, err := os.ReadFile(*dir + "/plugins.js")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pluginbench: %v (bench/scenarios/gen.py writes the scenarios; -dir names their directory)\n", err)
		os.Exit(1)
	}
	mjs, err := os.ReadFile(*dir + "/plugins.mjs")
	if err != nil {
		panic(err)
	}
	reqs, err := loadScenario(*dir + "/scn/" + *scn + ".bin")
	if err != nil {
		panic(err)
	}
	f, err := newFactory(*engine, string(js), string(mjs))
	if err != nil {
		panic(err)
	}
	if *checkOut != "" {
		if err := check(f, reqs, *checkOut); err != nil {
			fmt.Fprintln(os.Stderr, *engine, err)
			os.Exit(1)
		}
		return
	}
	if inuseSamples > 0 {
		res, err := inuse(f, reqs, *workers, *warm, *dur)
		if err != nil {
			fmt.Fprintln(os.Stderr, *engine, err)
			os.Exit(1)
		}
		res.Engine, res.Scenario = *engine, *scn
		b, _ := json.Marshal(res)
		fmt.Println(string(b))
		return
	}
	res, err := bench(f, reqs, *workers, *warm, *dur)
	if err != nil {
		fmt.Fprintln(os.Stderr, *engine, err)
		os.Exit(1)
	}
	res.Engine, res.Scenario = *engine, *scn
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
}

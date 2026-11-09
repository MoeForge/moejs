// Command pluginrace times the new-api plugin workload one request at a time
// for the race video: one worker serves a fixed sequence of requests, cycling
// through a scenario of the plugin benchmark, and records the time after every
// completed request. Each engine moves JSON the way the plugin benchmark's
// runner does: JSON bytes in, the hook call, JSON bytes out.
//
// Before the race it runs every request of the scenario once and hashes the
// results in the benchmark's check format ("hook length\nresult\n" per call);
// -ref makes a different hash an error. It prints one JSON line with the hash
// and the milliseconds from the start at which each request was done.
//
//	pluginrace -engine moejs -dir bench/testdata/scenarios -scenario agent100k -n 1600
//
// -dir holds plugins.js, plugins.mjs and scn/, which bench/scenarios/gen.py
// writes. The C QuickJS host that measure.py runs beside it speaks the same
// protocol (see README.md).
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/dop251/goja"
	qjs "github.com/go-quickjs/go-quickjs"
	"github.com/grafana/sobek"
	"github.com/robomotionio/goant"
	mqjs "modernc.org/quickjs"
)

type call struct {
	hook int
	args []byte
}

type request []call

// loadScenario reads a scenario file: the number of requests, then per
// request the number of calls and per call "hook length\nargs\n".
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
		if s, err = line(); err != nil {
			return nil, err
		}
		nc, _ := strconv.Atoi(s)
		for range nc {
			if s, err = line(); err != nil {
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
	run(c call) ([]byte, error)
	done()
}

// moejs: ParseJSON, Call, AppendJSON into a reused buffer, ReleaseCallData
// per request.
type moejsWorker struct {
	rt   *moejs.Runtime
	hook moejs.Hook
	out  []byte
}

func newMoejs(mjs string) (worker, error) {
	mod, err := moejs.Compile("plugins.mjs", mjs)
	if err != nil {
		return nil, err
	}
	h, err := mod.Hook("__call")
	if err != nil {
		return nil, err
	}
	rt := moejs.NewRuntime(moejs.Options{})
	if err := rt.Load(mod); err != nil {
		return nil, err
	}
	return &moejsWorker{rt: rt, hook: h}, nil
}

func (w *moejsWorker) run(c call) ([]byte, error) {
	a, err := w.rt.ParseJSON(c.args)
	if err != nil {
		return nil, err
	}
	r, err := w.rt.Call(w.hook, moejs.Int(int64(c.hook)), a)
	if err != nil {
		return nil, err
	}
	w.out, err = w.rt.AppendJSON(w.out[:0], r)
	return w.out, err
}

func (w *moejsWorker) done() { w.rt.ReleaseCallData() }

// Sobek: JSON.parse and JSON.stringify called from Go.
type sobekWorker struct {
	vm                     *sobek.Runtime
	call, parse, stringify sobek.Callable
	out                    []byte
}

func newSobek(js string) (worker, error) {
	prog, err := sobek.Compile("plugins.js", js, true)
	if err != nil {
		return nil, err
	}
	vm := sobek.New()
	if _, err := vm.RunProgram(prog); err != nil {
		return nil, err
	}
	fn, _ := sobek.AssertFunction(vm.Get("__call"))
	j := vm.Get("JSON").ToObject(vm)
	parse, _ := sobek.AssertFunction(j.Get("parse"))
	stringify, _ := sobek.AssertFunction(j.Get("stringify"))
	return &sobekWorker{vm: vm, call: fn, parse: parse, stringify: stringify}, nil
}

func (w *sobekWorker) run(c call) ([]byte, error) {
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
	return w.out, nil
}

func (w *sobekWorker) done() {}

// goja, the engine Sobek was forked from, the same way as Sobek.
type gojaWorker struct {
	vm                     *goja.Runtime
	call, parse, stringify goja.Callable
	out                    []byte
}

func newGoja(js string) (worker, error) {
	prog, err := goja.Compile("plugins.js", js, true)
	if err != nil {
		return nil, err
	}
	vm := goja.New()
	if _, err := vm.RunProgram(prog); err != nil {
		return nil, err
	}
	fn, _ := goja.AssertFunction(vm.Get("__call"))
	j := vm.Get("JSON").ToObject(vm)
	parse, _ := goja.AssertFunction(j.Get("parse"))
	stringify, _ := goja.AssertFunction(j.Get("stringify"))
	return &gojaWorker{vm: vm, call: fn, parse: parse, stringify: stringify}, nil
}

func (w *gojaWorker) run(c call) ([]byte, error) {
	a, err := w.parse(goja.Undefined(), w.vm.ToValue(string(c.args)))
	if err != nil {
		return nil, err
	}
	r, err := w.call(goja.Undefined(), w.vm.ToValue(c.hook), a)
	if err != nil {
		return nil, err
	}
	s, err := w.stringify(goja.Undefined(), r)
	if err != nil {
		return nil, err
	}
	w.out = append(w.out[:0], s.String()...)
	return w.out, nil
}

func (w *gojaWorker) done() {}

// go-quickjs uses a cached JS function because its host API has no byte JSON
// entry point. JSON text crosses the host boundary, just as for modernc.
type goQuickWorker struct {
	rt  *qjs.Runtime
	fn  qjs.Value
	out []byte
}

func newGoQuick(js string) (worker, error) {
	rt := qjs.New()
	if _, err := rt.EvalFile("plugins.js", js); err != nil {
		return nil, err
	}
	fn, err := rt.Get("__callJSON")
	return &goQuickWorker{rt: rt, fn: fn}, err
}
func (w *goQuickWorker) run(c call) ([]byte, error) {
	v, err := w.fn.Call(c.hook, string(c.args))
	if err != nil {
		return nil, err
	}
	w.out = append(w.out[:0], v.String()...)
	return w.out, nil
}
func (w *goQuickWorker) done() {}

// goant exposes byte-oriented eager JSON APIs: its native host API, with the
// interpreter and the normal collector.
type goantWorker struct {
	rt  *goant.Runtime
	fn  *goant.Function
	out []byte
}

func newGoant(js string) (worker, error) {
	rt := goant.New(goant.WithJIT(false))
	if _, err := rt.RunString(js); err != nil {
		return nil, err
	}
	v, err := rt.Get("__call")
	if err != nil {
		return nil, err
	}
	if v.Function() == nil {
		return nil, fmt.Errorf("goant: missing __call")
	}
	return &goantWorker{rt: rt, fn: v.Function()}, nil
}
func (w *goantWorker) run(c call) ([]byte, error) {
	a, err := w.rt.ParseJSON(c.args)
	if err != nil {
		return nil, err
	}
	v, err := w.fn.Call(c.hook, a)
	if err != nil {
		return nil, err
	}
	var ok bool
	w.out, ok, err = v.AppendJSON(w.out[:0])
	if err == nil && !ok {
		err = fmt.Errorf("goant: result is not JSON")
	}
	return w.out, err
}
func (w *goantWorker) done() {}

// modernc.org/quickjs (QuickJS translated to Go): it has no JSON API, so the
// argument goes in as a string and __callJSON parses and serializes in JS.
type modernWorker struct {
	fn  mqjs.Value
	out []byte
}

func newModern(js string) (worker, error) {
	vm, err := mqjs.NewVM()
	if err != nil {
		return nil, err
	}
	if _, err := vm.Eval(js, mqjs.EvalGlobal); err != nil {
		return nil, err
	}
	fn, err := vm.EvalValue("__callJSON", mqjs.EvalGlobal)
	if err != nil {
		return nil, err
	}
	return &modernWorker{fn: fn}, nil
}

func (w *modernWorker) run(c call) ([]byte, error) {
	r, err := w.fn.Call(mqjs.Value{}, c.hook, string(c.args))
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

func newWorker(engine, js, mjs string) (worker, error) {
	switch engine {
	case "moejs":
		return newMoejs(mjs)
	case "sobek":
		return newSobek(js)
	case "goja":
		return newGoja(js)
	case "go-quickjs":
		return newGoQuick(js)
	case "goant":
		return newGoant(js)
	case "modernc":
		return newModern(js)
	}
	return nil, fmt.Errorf("unknown engine %q", engine)
}

// check runs every request once and hashes each call's result in the
// benchmark's check format.
func check(w worker, reqs []request) (string, error) {
	h := sha256.New()
	for _, req := range reqs {
		for _, c := range req {
			b, err := w.run(c)
			if err != nil {
				return "", fmt.Errorf("hook %d: %w", c.hook, err)
			}
			fmt.Fprintf(h, "%d %d\n", c.hook, len(b))
			h.Write(b)
			h.Write([]byte{'\n'})
		}
		w.done()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func main() {
	engine := flag.String("engine", "moejs", "moejs, sobek, goja, modernc, go-quickjs or goant")
	dir := flag.String("dir", ".", "directory with plugins.js, plugins.mjs and scn/")
	scn := flag.String("scenario", "agent100k", "scenario name under -dir/scn")
	n := flag.Int("n", 1000, "requests in the race")
	ref := flag.String("ref", "", "expected check hash")
	flag.Parse()
	die := func(err error) {
		fmt.Fprintf(os.Stderr, "pluginrace %s: %v\n", *engine, err)
		os.Exit(1)
	}
	js, err := os.ReadFile(*dir + "/plugins.js")
	if err != nil {
		die(err)
	}
	mjs, err := os.ReadFile(*dir + "/plugins.mjs")
	if err != nil {
		die(err)
	}
	reqs, err := loadScenario(*dir + "/scn/" + *scn + ".bin")
	if err != nil {
		die(err)
	}
	w, err := newWorker(*engine, string(js), string(mjs))
	if err != nil {
		die(err)
	}
	sum, err := check(w, reqs)
	if err != nil {
		die(err)
	}
	if *ref != "" && sum != *ref {
		die(fmt.Errorf("check hash %s, want %s", sum, *ref))
	}
	ms := make([]float64, *n)
	start := time.Now()
	for k := range ms {
		for _, c := range reqs[k%len(reqs)] {
			if _, err := w.run(c); err != nil {
				die(fmt.Errorf("request %d hook %d: %w", k, c.hook, err))
			}
		}
		w.done()
		ms[k] = float64(time.Since(start).Microseconds()) / 1e3
	}
	out := map[string]any{"engine": *engine, "scenario": *scn, "requests": *n, "check": sum, "ms": ms}
	b, _ := json.Marshal(out)
	os.Stdout.Write(append(b, '\n'))
}

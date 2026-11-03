// Command pgo profiles the moejs hook suite (every recorded fixture case of
// every plugin, single goroutine, through the native API) and writes the
// CPU profile to the repository root as default.pgo. Run it with -trimpath,
// so that the profile names source files by module and import path rather
// than by where the checkout and the Go installation are.
//
//	go run -trimpath ./cmd/pgo [-duration 30s] [-engine moejs] [-out ../default.pgo]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"

	"github.com/MoeForge/moejs/bench"
	"github.com/MoeForge/moejs/bench/engines"
)

func main() {
	var (
		duration = flag.Duration("duration", 30*time.Second, "profiling duration")
		engine   = flag.String("engine", "moejs", "engine adapter to profile (an engines.All name)")
		out      = flag.String("out", filepath.Join(bench.RepoRoot(), "default.pgo"), "profile output path")
	)
	flag.Parse()
	e := engines.ByName(*engine)
	if e == nil {
		log.Fatalf("unknown engine %q", *engine)
	}
	cases, err := bench.LoadCases()
	if err != nil {
		log.Fatal(err)
	}
	runner, err := bench.NewRunner(e)
	if err != nil {
		log.Fatal(err)
	}
	rts := make(map[string]engines.Runtime, len(bench.PluginKeys))
	for _, key := range bench.PluginKeys {
		rt, err := runner.Runtime(key)
		if err != nil {
			log.Fatal(err)
		}
		rts[key] = rt
	}
	for i := range cases {
		h := &cases[i]
		out, err := h.Run(rts[h.Plugin])
		if msg := h.FullCheck(out, err, bench.JSONBoundary(e)); msg != "" {
			log.Fatalf("%s/%s: %s", h.Plugin, h.Case.Name, msg)
		}
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		log.Fatal(err)
	}
	start := time.Now()
	calls := 0
	for time.Since(start) < *duration {
		for i := range cases {
			h := &cases[i]
			out, err := h.Run(rts[h.Plugin])
			if msg := h.Check(out, err); msg != "" {
				log.Fatalf("%s/%s: %s", h.Plugin, h.Case.Name, msg)
			}
			calls++
		}
	}
	pprof.StopCPUProfile()
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
	elapsed := time.Since(start)
	fmt.Printf("%s: %d hook calls in %s (%.1f µs/call) -> %s\n", e.Name(), calls, elapsed.Round(time.Millisecond), float64(elapsed.Microseconds())/float64(calls), *out)
}

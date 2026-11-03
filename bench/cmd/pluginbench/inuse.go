package main

import (
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/libc"
)

// -inuse measures the bytes still referenced, leaving out garbage the Go GC
// has not collected yet. Every worker stops at parkPoint, which a worker
// reaches after the last call of a request has serialized its result into
// the output buffer and before the host releases anything of that call. With
// all workers stopped there, a full GC leaves only live objects. The total is
// the Go heap, C malloc (QuickJS through cgo) and the modernc libc allocator,
// which needs the memory.counters build tag.

var inuseSamples int

var (
	parkOn   atomic.Bool
	parkArr  atomic.Int32
	parkMu   sync.Mutex
	parkGate chan struct{}
)

func parkPoint() {
	if !parkOn.Load() {
		return
	}
	parkMu.Lock()
	g := parkGate
	parkMu.Unlock()
	parkArr.Add(1)
	<-g
}

type memSnap struct{ goLive, malloc, mallocMax, modernc uint64 }

func (m memSnap) total() uint64 { return m.goLive + m.malloc + m.modernc }

func snap() memSnap {
	runtime.GC()
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	inUse, maxInUse := mallocStats()
	return memSnap{goLive: s[0].Value.Uint64(), malloc: inUse, mallocMax: maxInUse, modernc: uint64(libc.MemStat().Bytes)}
}

type inuseResult struct {
	Engine   string `json:"engine"`
	Scenario string `json:"scenario"`
	Workers  int    `json:"workers"`
	// Rounds in which every worker stopped at parkPoint.
	Samples int `json:"samples"`
	// Runtimes loaded and scenario read, before the first request.
	IdleMiB float64 `json:"idle_mib"`
	// Largest and smallest round, and the parts of the largest.
	PeakMiB      float64 `json:"peak_mib"`
	MinMiB       float64 `json:"min_mib"`
	GoMiB        float64 `json:"go_mib"`
	MallocMiB    float64 `json:"malloc_mib"`
	ModerncMiB   float64 `json:"modernc_mib"`
	MallocMaxMiB float64 `json:"malloc_max_mib"`
}

func inuse(f factory, reqs []request, workers int, warm, dur time.Duration) (inuseResult, error) {
	var stop atomic.Bool
	errs := make(chan error, workers)
	start := make(chan struct{})
	var wg, ready sync.WaitGroup
	ready.Add(workers)
	for id := range workers {
		wg.Go(func() {
			w, err := f()
			ready.Done()
			if err != nil {
				errs <- err
				return
			}
			<-start
			i := id * 7919 % len(reqs)
			for !stop.Load() {
				for j, c := range reqs[i] {
					if _, err := w.run(c, j == len(reqs[i])-1); err != nil {
						errs <- err
						stop.Store(true)
						return
					}
				}
				w.done()
				if i++; i == len(reqs) {
					i = 0
				}
			}
		})
	}
	ready.Wait()
	idle := snap()
	close(start)
	time.Sleep(warm)
	var rounds []memSnap
	for range inuseSamples {
		time.Sleep(dur / time.Duration(inuseSamples))
		g := make(chan struct{})
		parkMu.Lock()
		parkGate = g
		parkMu.Unlock()
		parkArr.Store(0)
		parkOn.Store(true)
		deadline := time.Now().Add(30 * time.Second)
		for parkArr.Load() < int32(workers) && time.Now().Before(deadline) && len(errs) == 0 {
			time.Sleep(200 * time.Microsecond)
		}
		full := parkArr.Load() >= int32(workers)
		var m memSnap
		if full {
			m = snap()
		}
		parkOn.Store(false)
		close(g)
		if full {
			rounds = append(rounds, m)
		}
	}
	stop.Store(true)
	wg.Wait()
	select {
	case err := <-errs:
		return inuseResult{}, err
	default:
	}
	mib := func(b uint64) float64 { return float64(b) / (1 << 20) }
	r := inuseResult{Workers: workers, Samples: len(rounds), IdleMiB: mib(idle.total())}
	if len(rounds) > 0 {
		slices.SortFunc(rounds, func(a, b memSnap) int { return int(int64(a.total()) - int64(b.total())) })
		p := rounds[len(rounds)-1]
		r.PeakMiB, r.MinMiB = mib(p.total()), mib(rounds[0].total())
		r.GoMiB, r.MallocMiB, r.ModerncMiB, r.MallocMaxMiB = mib(p.goLive), mib(p.malloc), mib(p.modernc), mib(p.mallocMax)
	}
	return r, nil
}

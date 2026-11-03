//go:build iccensus

package bench

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bench/engines"
	"github.com/MoeForge/moejs/engine"
)

// TestICCensus reports the inline-cache misses of the plugins (loading and
// the hook suite, first and steady-state calls) and of the corpus programs,
// on the moejs adapter. Run with:
//
//	go test -tags iccensus -run TestICCensus -v .
//
// Steady-state polymorphic misses (a site whose entry held another shape)
// are the sites where a polymorphic cache would pay.
func TestICCensus(t *testing.T) {
	const rounds = 5
	cases := allCases(t)
	for _, e := range []engines.Engine{engines.NewMoejsEngine()} {
		t.Run(e.Name(), func(t *testing.T) {
			runner := mustRunner(t, e)
			engine.ResetICCensus()
			rts := pluginRuntimes(t, runner)
			defer closeAll(rts)
			reportCensus(t, "plugins: load")

			suite := func() {
				for i := range cases {
					h := &cases[i]
					out, err := rts[h.Plugin].Call(h.Case.Hook, h.Case.HookPath(), h.Args...)
					if msg := h.Check(out, err); msg != "" {
						t.Fatalf("%s/%s: %s", h.Plugin, h.Case.Name, msg)
					}
				}
			}
			engine.ResetICCensus()
			suite()
			reportCensus(t, "hook suite: first call")
			engine.ResetICCensus()
			for range rounds {
				suite()
			}
			reportCensus(t, fmt.Sprintf("hook suite: steady state (%d rounds)", rounds))

			files, err := filepath.Glob(filepath.Join(moduleRoot(), "corpus", "*.js"))
			if err != nil || len(files) == 0 {
				t.Fatal("no corpus", err)
			}
			var progs []engines.Runtime
			engine.ResetICCensus()
			for _, file := range files {
				src, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(string(src), "// script\n") {
					continue // a script has no run to call again
				}
				name := strings.TrimSuffix(filepath.Base(file), ".js")
				mod, err := e.Compile(name+".js", string(src))
				if err != nil {
					t.Fatal(err)
				}
				rt, err := e.NewRuntime()
				if err != nil {
					t.Fatal(err)
				}
				defer rt.Close()
				if err := rt.Instantiate(mod); err != nil {
					t.Fatal(err)
				}
				rt.Call("run", nil, corpusArg())
				progs = append(progs, rt)
			}
			reportCensus(t, "corpus: first run")
			engine.ResetICCensus()
			for range rounds {
				for _, rt := range progs {
					rt.Call("run", nil, corpusArg())
				}
			}
			reportCensus(t, fmt.Sprintf("corpus: steady state (%d rounds)", rounds))
		})
	}
}

func reportCensus(t *testing.T, phase string) {
	sites := engine.ICCensus()
	var hits, cold, stale, poly, saved2, saved4 uint64
	missing := 0
	for i := range sites {
		s := &sites[i]
		hits += s.Hits
		cold += s.Cold
		stale += s.Stale
		poly += s.Poly
		saved2 += s.Saved2
		saved4 += s.Saved4
		if s.Misses() != 0 {
			missing++
		}
	}
	total := hits + cold + stale + poly
	if total == 0 {
		t.Logf("%s: no inline-cache accesses", phase)
		return
	}
	t.Logf("%s: %d sites, %d accesses, hit %.2f%%; misses: cold %d, stale %d, poly %d (%d sites miss); a 2-entry cache saves %d poly misses, a 4-entry one %d",
		phase, len(sites), total, 100*float64(hits)/float64(total), cold, stale, poly, missing, saved2, saved4)
	for i := range min(len(sites), 12) {
		s := &sites[i]
		if s.Poly == 0 {
			break
		}
		t.Logf("  poly %5d/%-5d saved %4d/%-4d %s", s.Poly, s.Hits+s.Misses(), s.Saved2, s.Saved4, censusSite(s))
	}
	slices.SortFunc(sites, func(a, b engine.ICSiteCensus) int { return cmp.Compare(b.Cold+b.Stale, a.Cold+a.Stale) })
	for i := range min(len(sites), 8) {
		s := &sites[i]
		if s.Cold+s.Stale == 0 {
			break
		}
		t.Logf("  cold %5d stale %5d /%-5d %s", s.Cold, s.Stale, s.Hits+s.Misses(), censusSite(s))
	}
}

func censusSite(s *engine.ICSiteCensus) string {
	line, col := s.Code.Position(uint32(s.PC))
	file := "?"
	if s.Code.Source != nil {
		file = s.Code.Source.Name
	}
	fn := s.Code.Name
	if fn == "" {
		fn = "(anonymous)"
	}
	return fmt.Sprintf("%-10s %-16q %s %s:%d:%d (shapes %d)", s.Op, s.Key, fn, file, line, col, s.Shapes)
}

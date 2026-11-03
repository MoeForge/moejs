//go:build linux && (amd64 || arm64)

package node

import (
	"math/rand/v2"
	"testing"
)

// refSplitName is splitName one component at a time.
func refSplitName(rel string) (dir, name string, ok bool) {
	start := 0
	last := -1
	for i := 0; i <= len(rel); i++ {
		if i < len(rel) && rel[i] != '/' {
			continue
		}
		c := rel[start:i]
		if c == "" || c == "." || c == ".." {
			return "", "", false
		}
		if i < len(rel) {
			last = i
		}
		start = i + 1
	}
	if last < 0 {
		return "", rel, true
	}
	return rel[:last], rel[last+1:], true
}

// TestSplitName compares splitName with the component walk on paths of
// names, dots and slashes.
func TestSplitName(t *testing.T) {
	parts := []string{"a", "b", ".", "..", "...", ".a", "a.", "/", "/", "//", "x.y"}
	rng := rand.New(rand.NewPCG(5, 6))
	for range 200000 {
		var p string
		for n := rng.IntN(7); n > 0; n-- {
			p += parts[rng.IntN(len(parts))]
		}
		d, n, ok := splitName(p)
		wd, wn, wok := refSplitName(p)
		if d != wd || n != wn || ok != wok {
			t.Fatalf("%q: %q %q %v, want %q %q %v", p, d, n, ok, wd, wn, wok)
		}
	}
}

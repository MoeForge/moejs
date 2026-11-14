package tests

import (
	"sync"
	"testing"

	"github.com/MoeForge/moejs"
)

// TestHostStringTwoRuntimes: a host keeps one string value and hands it to
// two runtimes, which use it as a Map key at the same time; under -race
// the hash the first use stores must not race.
func TestHostStringTwoRuntimes(t *testing.T) {
	mod, err := moejs.Compile("m.js", `const m = new Map(); export function hook(k) { m.set(k, 1); return m.get(k); }`)
	if err != nil {
		t.Fatal(err)
	}
	h, err := mod.Hook("hook")
	if err != nil {
		t.Fatal(err)
	}
	var rts [2]*moejs.Runtime
	for i := range rts {
		rts[i] = moejs.NewRuntime(moejs.Options{})
		if err := rts[i].Load(mod); err != nil {
			t.Fatal(err)
		}
	}
	key := moejs.String("tenant-42")
	var wg sync.WaitGroup
	for i := range rts {
		wg.Go(func() {
			if v, err := rts[i].Call(h, key); err != nil || v.String() != "1" {
				t.Errorf("%v %v", v, err)
			}
		})
	}
	wg.Wait()
}

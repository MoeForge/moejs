package node

import (
	"testing"
	"time"
)

// TestBudgetOvershootUnit: listings that each hold a byte and then grow,
// in age order, by the limit less one. The oldest goes over, the next
// waits for it, so what is held stays within the limit and that one
// growth.
func TestBudgetOvershootUnit(t *testing.T) {
	for _, k := range []int{4, 16} {
		const L = 1000
		b := newBudget(L)
		halt := func() error { return nil }
		hs := make([]*holding, k)
		for i := range hs {
			hs[i] = &holding{what: "listing"}
			if err := b.take(hs[i], 1, halt); err != nil {
				t.Fatal(err)
			}
		}
		granted := 0
		for _, h := range hs {
			done := make(chan error, 1)
			go func() { done <- b.take(h, L-1, halt) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
				granted++
				continue
			case <-time.After(200 * time.Millisecond):
			}
			break
		}
		b.mu.Lock()
		used := b.used
		b.mu.Unlock()
		b.close()
		if granted != 1 || used > 2*L {
			t.Errorf("%d listings: %d growths granted, %d held with limit %d", k, granted, used, L)
		}
	}
}

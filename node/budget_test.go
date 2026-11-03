package node_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// TestBudgetOvershootPeak: as many recursive listings as workers, each of
// a directory holding one subdirectory of about 0.7 times the limit,
// which each takes at once after a few bytes for the top directory. Only
// the oldest goes over the budget, so what the workers hold stays within
// about twice the limit, and every listing ends.
func TestBudgetOvershootPeak(t *testing.T) {
	const limit = 8 << 20
	for _, workers := range []int{4, 12} {
		t.Run(fmt.Sprint("workers", workers), func(t *testing.T) {
			h := newHarnessRT(t, moejs.Options{MemoryLimit: limit}, node.Options{FS: &node.FSOptions{Workers: workers}})
			sub := filepath.Join(h.dir, "t", "sub")
			require.NoError(t, os.MkdirAll(sub, 0o755))
			pad := strings.Repeat("x", 200)
			for i := range 24000 {
				f, err := os.Create(filepath.Join(sub, fmt.Sprintf("%s%06d", pad, i)))
				require.NoError(t, err)
				f.Close()
			}
			var peak atomic.Int64
			stop := make(chan struct{})
			sampled := make(chan struct{})
			go func() {
				defer close(sampled)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if u := node.BudgetUsed(h.api); u > peak.Load() {
						peak.Store(u)
					}
					time.Sleep(200 * time.Microsecond)
				}
			}()
			res, err, fired := runDeadline(t, h, fmt.Sprintf(`
			const ops = [];
			for (let r = 0; r < 3; r++) {
				const round = [];
				for (let i = 0; i < %d; i++) round.push(fsp.readdir("t", {recursive: true}).then(l => l.length, e => e.code || e.name));
				ops.push(...await Promise.all(round));
			}
			return JSON.stringify(ops.slice(0, 6))`, workers), 60*time.Second)
			close(stop)
			<-sampled
			t.Logf("%s: peak held %.2f times the limit", res, float64(peak.Load())/limit)
			require.NoError(t, err)
			require.False(t, fired, "the listings did not end")
			require.LessOrEqual(t, peak.Load(), int64(2*limit))
			require.Zero(t, node.BudgetUsed(h.api))
		})
	}
}

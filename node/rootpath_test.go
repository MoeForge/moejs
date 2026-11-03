package node_test

import (
	"testing"

	"github.com/MoeForge/moejs/node"
)

// TestRootPath runs the differential and sandbox tests again with every
// operation going the os.Root way, which Linux's openat2 path replaces
// for reads, listings and stat, and which the other systems take.
func TestRootPath(t *testing.T) {
	node.SetNoFast(true)
	defer node.SetNoFast(false)
	t.Run("NodeDiffRead", TestNodeDiffRead)
	t.Run("NodeDiffWrite", TestNodeDiffWrite)
	t.Run("Escapes", TestEscapes)
	t.Run("MountAndCwd", TestMountAndCwd)
	t.Run("Allow", TestAllow)
	t.Run("MemoryLimit", TestMemoryLimit)
	t.Run("ConcurrentOps", TestConcurrentOps)
	t.Run("CacheOwnChanges", TestCacheOwnChanges)
	t.Run("CacheSwapRace", TestCacheSwapRace)
}

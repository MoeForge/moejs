package node_test

import (
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// TestFsAssignSnapshot: Object.assign from fs and from Buffer.prototype,
// which keep a shape past 64 properties, with a setter on the target that
// deletes the source's last key and adds another: the added key is not in
// the source's key list when the copy starts, so it is not copied.
func TestFsAssignSnapshot(t *testing.T) {
	for _, mutable := range []bool{false, true} {
		h := newHarnessRT(t, moejs.Options{MutableIntrinsics: mutable}, node.Options{FS: &node.FSOptions{}, Buffer: true})
		res := h.run(`
	const { Buffer } = await import("node:buffer");
	function probe(o) {
		o.tail = 1; // the last slot
		const ks = Object.keys(o); const last = ks[ks.length - 1];
		const to = { set [ks[0]](v) { delete o[last]; o.zz = "new" } };
		Object.assign(to, o);
		return [ks.length > 64, last, String(to.zz), String(to.tail)].join(",");
	}
	return probe(fs.default) + "|" + probe(Buffer.prototype)`)
		require.Equal(t, "true,tail,undefined,undefined|true,tail,undefined,undefined", res, "mutable %v", mutable)
	}
}

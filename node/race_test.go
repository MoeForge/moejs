package node_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// TestConcurrentOps runs many promise and callback operations at once, on
// the pool's workers, reads and writes; with -race it checks that the
// workers share nothing with the loop but what they hand over.
func TestConcurrentOps(t *testing.T) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 256 << 20}, node.Options{FS: &node.FSOptions{Write: true, Workers: 3}, Buffer: true})
	tree := map[string]string{}
	for i := range 50 {
		tree[fmt.Sprintf("d%d/f%d.txt", i%5, i)] = strings.Repeat("x", i*100)
	}
	h.write(tree)
	res := h.run(`
	const ops = [];
	for (let i = 0; i < 50; i++) {
		const p = "d" + (i % 5) + "/f" + i + ".txt";
		ops.push(fsp.readFile(p, "utf8").then(s => s.length));
		ops.push(fsp.stat(p).then(s => s.size));
		ops.push(new Promise((res, rej) => fs.readFile(p, (e, b) => e ? rej(e) : res(b.length))));
		ops.push(fsp.writeFile("w" + i + ".txt", Buffer.alloc(i, 97)).then(() => fsp.readFile("w" + i + ".txt")).then(b => b.length));
		ops.push(fsp.readdir("d" + (i % 5)).then(l => l.length));
		ops.push(Array.fromAsync(fsp.glob("**/*.txt")).then(l => l.length > 0 ? 1 : 0));
	}
	const all = await Promise.all(ops);
	let sum = 0; for (const n of all) sum += n;
	return sum`)
	want := 0
	for i := range 50 {
		want += i*100 + i*100 + i*100 + i + 10 + 1
	}
	require.Equal(t, fmt.Sprint(want), res)
}

// TestCloseInFlight closes the installation while operations wait for a
// worker: they settle with EBADF, and the loop ends.
func TestCloseInFlight(t *testing.T) {
	h := newHarness(t, node.Options{FS: &node.FSOptions{Workers: 1}})
	h.write(map[string]string{"a.txt": "a"})
	require.NoError(t, h.rt.SetGlobal("closeNow", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Undefined(), h.api.Close()
	})))
	res := h.run(`
	const ops = [];
	for (let i = 0; i < 100; i++) ops.push(fsp.readFile("a.txt", "utf8").then(s => s, e => e.code));
	closeNow();
	const out = await Promise.all(ops);
	return out.every(x => x === "a" || x === "EBADF")`)
	require.Equal(t, "true", res)
}

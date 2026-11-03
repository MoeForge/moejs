package node_test

import (
	"testing"

	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// TestForms reads one file in the three forms, and the other entry
// points once each.
func TestForms(t *testing.T) {
	h := newHarness(t, node.Options{FS: &node.FSOptions{}, Buffer: true})
	h.write(map[string]string{"a.txt": "hello", "dir/b.ts": "x", "dir/sub/c.ts": "y", "link": "->a.txt"})
	require.Equal(t, "hello", h.run(`return fs.readFileSync("a.txt", "utf8")`))
	require.Equal(t, "aGVsbG8=", h.run(`return fs.readFileSync("a.txt").toString("base64")`))
	require.Equal(t, "hello", h.run(`return await fsp.readFile("a.txt", "utf8")`))
	require.Equal(t, "hello", h.run(`return await new Promise((res, rej) => fs.readFile("a.txt", "utf8", (e, d) => e ? rej(e) : res(d)))`))
	require.Equal(t, "true", h.run(`return Buffer.isBuffer(await fsp.readFile("a.txt"))`))
	require.Equal(t, `["a.txt","dir","link"]`, h.run(`return fs.readdirSync(".")`))
	require.Equal(t, `["dir/b.ts","dir/sub/c.ts"]`, h.run(`return fs.globSync("**/*.ts")`))
	require.Equal(t, `["dir/b.ts","dir/sub/c.ts"]`, h.run(`return Array.fromAsync(fsp.glob("**/*.ts"))`))
	require.Equal(t, `["dir/b.ts","dir/sub/c.ts"]`, h.run(`return await new Promise(res => fs.glob("**/*.ts", (e, m) => res(m)))`))
	require.Equal(t, "true", h.run(`const { Buffer: B } = await import("node:buffer"); return B === Buffer`))
	require.Equal(t, `"function"`, h.run(`return JSON.stringify(typeof (await import("fs/promises")).readFile)`))
}

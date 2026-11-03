package node_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// The directory cache (fast_linux.go) keeps descriptors of directories;
// these tests change the tree under it.

// TestCacheOwnChanges: the program's own rename, rm, rmdir and unlink
// take effect at once, also for a directory whose descriptor is cached,
// and a link renamed into the place of a cached directory is resolved
// anew: one that leads out of the root fails with EACCES.
func TestCacheOwnChanges(t *testing.T) {
	base, root := outside(t)
	writeTree(t, root, map[string]string{"a/x.txt": "a", "c/x.txt": "c", "out": "->" + filepath.Join(base, "secretdir"),
		"lnk": "->c", "e/f/x.txt": "ef"})
	require.NoError(t, os.WriteFile(filepath.Join(base, "secretdir", "x.txt"), []byte("SECRET"), 0o644))
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Write: true}})
	try := func(body string) string {
		return h.run(`try { return ` + body + ` } catch (e) { return e.code }`)
	}
	require.Equal(t, "a", try(`fs.readFileSync("a/x.txt", "utf8")`))
	require.Equal(t, "1", try(`fs.statSync("a/x.txt").size`))
	// Renamed away: the path no longer names it.
	h.run(`fs.renameSync("a", "b")`)
	require.Equal(t, "ENOENT", try(`fs.statSync("a/x.txt").size`))
	require.Equal(t, "a", try(`fs.readFileSync("b/x.txt", "utf8")`))
	// A link out renamed into its place: EACCES, not the old directory.
	h.run(`fs.renameSync("out", "a")`)
	require.Equal(t, "EACCES", try(`fs.statSync("a/x.txt").size`))
	require.Equal(t, "EACCES", try(`fs.readFileSync("a/x.txt", "utf8")`))
	require.Equal(t, "EACCES", try(`fs.readdirSync("a")`))
	// A directory removed and made again: the new one.
	require.Equal(t, "c", try(`fs.readFileSync("c/x.txt", "utf8")`))
	h.run(`fs.rmSync("c", {recursive: true}); fs.mkdirSync("c"); fs.writeFileSync("c/y.txt", "new")`)
	require.Equal(t, "ENOENT", try(`fs.statSync("c/x.txt").size`))
	require.Equal(t, "new", try(`fs.readFileSync("c/y.txt", "utf8")`))
	// A link to a directory unlinked: the path is gone.
	require.Equal(t, "new", try(`fs.readFileSync("lnk/y.txt", "utf8")`))
	h.run(`fs.unlinkSync("lnk")`)
	require.Equal(t, "ENOENT", try(`fs.readFileSync("lnk/y.txt", "utf8")`))
	// rmdir.
	require.Equal(t, "2", try(`fs.statSync("e/f/x.txt").size`))
	h.run(`fs.rmSync("e/f/x.txt"); fs.rmdirSync("e/f"); fs.mkdirSync("e/f")`)
	require.Equal(t, "ENOENT", try(`fs.statSync("e/f/x.txt").size`))
	// The same through the promise forms, which run on workers.
	require.Equal(t, "ef", h.run(`await fsp.writeFile("e/f/x.txt", "ef"); return fsp.readFile("e/f/x.txt", "utf8")`))
	require.Equal(t, "ENOENT", h.run(`await fsp.rename("e", "g"); try { await fsp.stat("e/f/x.txt") } catch (e) { return e.code }`))
}

// TestCacheExternalChanges: a directory another process renames out of
// the root stops being found once the cache's entries expire.
func TestCacheExternalChanges(t *testing.T) {
	base, root := outside(t)
	writeTree(t, root, map[string]string{"a/x.txt": "a"})
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root}})
	require.Equal(t, "a", h.run(`return fs.readFileSync("a/x.txt", "utf8")`))
	require.NoError(t, os.Rename(filepath.Join(root, "a"), filepath.Join(base, "moved")))
	time.Sleep(5 * time.Millisecond)
	require.Equal(t, "ENOENT", h.run(`try { return fs.readFileSync("a/x.txt", "utf8") } catch (e) { return e.code }`))
	// And a link out put in its place by another process is refused.
	require.NoError(t, os.Symlink(filepath.Join(base, "moved"), filepath.Join(root, "a")))
	require.Equal(t, "EACCES", h.run(`try { return fs.readFileSync("a/x.txt", "utf8") } catch (e) { return e.code }`))
}

// TestCacheSwapRace swaps a directory of the root with a link out of it,
// from another goroutine, as fast as it can, while the program stats and
// reads a file in it through every form: no result may come from outside
// the root.
func TestCacheSwapRace(t *testing.T) {
	base, root := outside(t)
	require.NoError(t, os.WriteFile(filepath.Join(base, "secretdir", "f.txt"), []byte("OUTSIDE-OUTSIDE"), 0o644))
	writeTree(t, root, map[string]string{"d/f.txt": "in", "d/sub/f.txt": "in"})
	h := newHarnessRT(t, moejs.Options{}, node.Options{FS: &node.FSOptions{Root: root}})
	var stop atomic.Bool
	var wg sync.WaitGroup
	swaps := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		d, tmp, link := filepath.Join(root, "d"), filepath.Join(root, "d.real"), filepath.Join(base, "secretdir")
		for !stop.Load() {
			if os.Rename(d, tmp) != nil {
				continue
			}
			_ = os.Symlink(link, d)
			_ = os.Remove(d)
			_ = os.Rename(tmp, d)
			swaps++
		}
	}()
	res := h.run(`
	const seen = {};
	const note = (k) => { seen[k] = (seen[k] || 0) + 1 };
	const check = (s) => { if (s === "OUTSIDE-OUTSIDE" || s === 15) throw new Error("read outside the root: " + s); note("ok") };
	for (let i = 0; i < 4000; i++) {
		for (const p of ["d/f.txt", "d/sub/f.txt"]) {
			try { check(fs.readFileSync(p, "utf8")) } catch (e) { if (e.message.startsWith("read outside")) throw e; note(e.code) }
			try { check(fs.statSync(p).size) } catch (e) { if (e.message.startsWith("read outside")) throw e; note(e.code) }
		}
		if (i % 100 === 0) {
			const all = await Promise.allSettled([fsp.readFile("d/f.txt", "utf8"), fsp.stat("d/f.txt"), fsp.readdir("d")]);
			for (const r of all) {
				if (r.status === "rejected") { note(r.reason.code); continue }
				const v = r.value;
				check(typeof v === "string" ? v : Array.isArray(v) ? (v.includes("sub") ? "in" : "OUTSIDE-OUTSIDE") : v.size);
			}
		}
	}
	return seen`)
	stop.Store(true)
	wg.Wait()
	t.Logf("%d swaps, results %s", swaps, res)
	require.NotContains(t, res, "outside")
	for _, code := range []string{"EIO", "EBADF", "UNKNOWN"} {
		require.False(t, strings.Contains(res, `"`+code+`"`), res)
	}
}

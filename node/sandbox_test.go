package node_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// outside makes a directory with a root below it and a file beside the
// root: what the root's escapes would reach.
func outside(t *testing.T) (base, root string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root = filepath.Join(base, "root")
	require.NoError(t, os.Mkdir(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "secret.txt"), []byte("secret"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(base, "secretdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "secretdir", "s.txt"), []byte("s"), 0o644))
	return base, root
}

func TestEscapes(t *testing.T) {
	base, root := outside(t)
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Write: true}})
	h.write(map[string]string{
		"a.txt":       "a",
		"sub/b.txt":   "b",
		"abs-out":     "->" + filepath.Join(base, "secret.txt"),
		"rel-out":     "->../secret.txt",
		"dir-out":     "->../secretdir",
		"deep/up":     "->../../secretdir",
		"etc":         "->/etc",
		"abs-in":      "->" + filepath.Join(root, "a.txt"),
		"abs-in-dir":  "->" + filepath.Join(root, "sub"),
		"sub/rel-in":  "->../a.txt",
		"sub/to-root": "->..",
	})
	cases := []struct{ name, body, want string }{
		{"dotdot", `fs.readFileSync("../secret.txt")`, "EACCES"},
		{"dotdot deep", `fs.readFileSync("sub/../../secret.txt")`, "EACCES"},
		{"absolute outside", `fs.readFileSync("/etc/passwd")`, "EACCES"},
		{"absolute root dotdot", `fs.readFileSync(ROOT + "/../secret.txt")`, "EACCES"},
		{"absolute link out", `fs.readFileSync("abs-out")`, "EACCES"},
		{"relative link out", `fs.readFileSync("rel-out")`, "EACCES"},
		{"through dir link", `fs.readFileSync("dir-out/s.txt")`, "EACCES"},
		{"through deep link", `fs.readFileSync("deep/up/s.txt")`, "EACCES"},
		{"through /etc link", `fs.readFileSync("etc/passwd")`, "EACCES"},
		{"stat link out", `fs.statSync("abs-out")`, "EACCES"},
		{"readdir link out", `fs.readdirSync("dir-out")`, "EACCES"},
		{"realpath link out", `fs.realpathSync("rel-out")`, "EACCES"},
		{"realpath dotdot", `fs.realpathSync("..")`, "EACCES"},
		{"access link out", `fs.accessSync("dir-out/s.txt")`, "EACCES"},
		{"write through link", `fs.writeFileSync("dir-out/new.txt", "x")`, "EACCES"},
		{"write dotdot", `fs.writeFileSync("../new.txt", "x")`, "EACCES"},
		{"append link out", `fs.appendFileSync("rel-out", "x")`, "EACCES"},
		{"mkdir dotdot", `fs.mkdirSync("../newdir")`, "EACCES"},
		{"mkdir recursive out", `fs.mkdirSync("dir-out/a/b", {recursive: true})`, "EACCES"},
		{"rename out", `fs.renameSync("a.txt", "../moved.txt")`, "EACCES"},
		{"rename through link", `fs.renameSync("a.txt", "dir-out/moved.txt")`, "EACCES"},
		{"rename in from out", `fs.renameSync("../secret.txt", "stolen.txt")`, "EACCES"},
		{"copy from out", `fs.copyFileSync("rel-out", "stolen.txt")`, "EACCES"},
		{"copy to out", `fs.copyFileSync("a.txt", "dir-out/c.txt")`, "EACCES"},
		{"cp to out", `fs.cpSync("sub", "dir-out/sub", {recursive: true})`, "EACCES"},
		{"rm out", `fs.rmSync("dir-out/s.txt")`, "EACCES"},
		{"unlink out", `fs.unlinkSync("../secret.txt")`, "EACCES"},
		{"readFile promise", `await fsp.readFile("rel-out")`, "EACCES"},
		{"readdir promise", `await fsp.readdir("deep/up")`, "EACCES"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := strings.ReplaceAll(c.body, "ROOT", `"`+root+`"`)
			res := h.run(`try { ` + body + `; return "no error" } catch (e) { return e.code + " " + e.message }`)
			require.True(t, strings.HasPrefix(res, c.want+" "), res)
		})
	}
	// Nothing changed outside.
	ents, err := os.ReadDir(base)
	require.NoError(t, err)
	var names []string
	for _, d := range ents {
		names = append(names, d.Name())
	}
	require.Equal(t, []string{"root", "secret.txt", "secretdir"}, names)
	ents, err = os.ReadDir(filepath.Join(base, "secretdir"))
	require.NoError(t, err)
	require.Len(t, ents, 1)

	// Links inside the root, absolute ones included, work.
	require.Equal(t, "a", h.run(`return fs.readFileSync("abs-in", "utf8")`))
	require.Equal(t, `["b.txt","rel-in","to-root"]`, h.run(`return fs.readdirSync("abs-in-dir")`))
	require.Equal(t, "a", h.run(`return fs.readFileSync("sub/rel-in", "utf8")`))
	require.Equal(t, "a", h.run(`return fs.readFileSync("sub/to-root/a.txt", "utf8")`))
	require.Equal(t, root+"/a.txt", h.run(`return fs.realpathSync("abs-in")`))
	require.Equal(t, root+"/sub", h.run(`return fs.realpathSync("abs-in-dir")`))
	require.Equal(t, "true", h.run(`return fs.lstatSync("abs-out").isSymbolicLink()`))
	require.Equal(t, root+"/a.txt", h.run(`return fs.readlinkSync("abs-in")`))
	// The glob walk stays inside: no link out is followed.
	require.Equal(t, `["a.txt","sub/b.txt"]`, h.run(`return fs.globSync("**/*.txt")`))
	// followSymlinks follows the links in, not into a cycle, as Node.js.
	require.Equal(t, `["a.txt","abs-in-dir/b.txt","sub/b.txt"]`, h.run(`return fs.globSync("**/*.txt", {followSymlinks: true})`))
	require.Equal(t, `[]`, h.run(`return fs.globSync("dir-out/*")`))
	require.Equal(t, `[]`, h.run(`return fs.globSync("../*")`))
	// Removing a link out removes the link only.
	h.run(`fs.rmSync("dir-out"); fs.unlinkSync("abs-out")`)
	_, err = os.Stat(filepath.Join(base, "secretdir", "s.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(base, "secret.txt"))
	require.NoError(t, err)
}

func TestMountAndCwd(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{"a.txt": "a", "proj/b.txt": "b", "proj/link": "->../a.txt"})
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: "/workspace", Cwd: "proj"}})
	require.Equal(t, "b", h.run(`return fs.readFileSync("b.txt", "utf8")`))
	require.Equal(t, "b", h.run(`return fs.readFileSync("/workspace/proj/b.txt", "utf8")`))
	require.Equal(t, "a", h.run(`return fs.readFileSync("../a.txt", "utf8")`))
	require.Equal(t, "a", h.run(`return fs.readFileSync("/workspace/a.txt", "utf8")`))
	require.Equal(t, "/workspace/a.txt", h.run(`return fs.realpathSync("link")`))
	require.Equal(t, "/workspace/proj", h.run(`return fs.realpathSync(".")`))
	require.Equal(t, "/workspace", h.run(`return fs.realpathSync("/workspace/proj/..")`))
	require.Equal(t, `["b.txt","link"]`, h.run(`return fs.readdirSync("/workspace/proj")`))
	require.Equal(t, `["../a.txt"]`, h.run(`return fs.globSync("../*.txt")`))
	require.Equal(t, `["/workspace/proj/b.txt"]`, h.run(`return fs.globSync("/workspace/proj/*.txt")`))
	require.Equal(t, `[]`, h.run(`return fs.globSync("/elsewhere/*")`))
	// The host's own path of the root is outside JavaScript's view.
	res := h.run(`try { fs.readFileSync(` + quote(root+"/a.txt") + `) } catch (e) { return e.code + " " + e.message }`)
	require.Equal(t, "EACCES EACCES: permission denied, open '"+root+"/a.txt'", res)
	res = h.run(`try { fs.readFileSync("../../x") } catch (e) { return e.code + " " + e.path }`)
	require.Equal(t, "EACCES ../../x", res)
	res = h.run(`try { fs.readFileSync("/workspace2/x") } catch (e) { return e.code }`)
	require.Equal(t, "EACCES", res)

	_, err := node.InstallLoop(h.loop, node.Options{FS: &node.FSOptions{Root: root, Mount: "workspace"}})
	require.ErrorContains(t, err, "not an absolute path")
	_, err = node.InstallLoop(h.loop, node.Options{FS: &node.FSOptions{Root: root, Mount: "/w", Cwd: "/elsewhere"}})
	require.ErrorContains(t, err, "not under the mount")
	_, err = node.InstallLoop(h.loop, node.Options{FS: &node.FSOptions{Root: filepath.Join(root, "nope")}})
	require.Error(t, err)
}

func realDirT(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func TestReadOnly(t *testing.T) {
	h := newHarness(t, node.Options{FS: &node.FSOptions{}})
	h.write(map[string]string{"a.txt": "a", "d/x": "x", "e/": ""})
	writes := []string{
		`fs.writeFileSync("n.txt", "x")`, `fs.writeFileSync("a.txt", "x")`, `fs.appendFileSync("a.txt", "x")`,
		`fs.mkdirSync("m")`, `fs.mkdirSync("m/n", {recursive: true})`, `fs.rmSync("a.txt")`,
		`fs.rmSync("d", {recursive: true, force: true})`, `fs.rmdirSync("e")`, `fs.unlinkSync("a.txt")`,
		`fs.renameSync("a.txt", "b.txt")`, `fs.copyFileSync("a.txt", "c.txt")`, `fs.cpSync("d", "d2", {recursive: true})`,
		`fs.readFileSync("a.txt", {flag: "a+"})`, `fs.readFileSync("a.txt", {flag: "w"})`, `fs.accessSync("a.txt", fs.constants.W_OK)`,
		`await fsp.writeFile("n.txt", "x")`, `await fsp.rm("a.txt")`, `await fsp.mkdir("m")`,
		`await new Promise((res, rej) => fs.unlink("a.txt", e => e ? rej(e) : res()))`,
	}
	for _, w := range writes {
		res := h.run(`try { ` + w + `; return "no error" } catch (e) { return e.code + " " + e.message }`)
		require.True(t, strings.HasPrefix(res, "EROFS EROFS: read-only file system, "), "%s: %s", w, res)
	}
	require.Equal(t, "a", h.run(`return fs.readFileSync("a.txt", {encoding: "utf8", flag: "r"})`))
	require.Equal(t, "undefined", h.run(`return fs.accessSync("a.txt", fs.constants.R_OK)`))
	ents, err := os.ReadDir(h.dir)
	require.NoError(t, err)
	require.Len(t, ents, 3)
	data, err := os.ReadFile(filepath.Join(h.dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "a", string(data))
}

func TestAllow(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	allow := func(op, p string) error {
		mu.Lock()
		calls = append(calls, op+" "+p)
		mu.Unlock()
		switch base := filepath.Base(p); {
		case base == ".env":
			return errors.New("denied")
		case base == "hidden.txt":
			return fs.ErrNotExist
		case base == "perm.txt":
			return syscall.EPERM
		case strings.HasPrefix(p, "/w/secret") && op == "write":
			return errors.New("read-only area")
		}
		return nil
	}
	root := realDirT(t)
	writeTree(t, root, map[string]string{".env": "KEY=1", "a.txt": "a", "env-link": "->.env", "dir/env": "->../.env",
		"hidden.txt": "h", "perm.txt": "p", "secret/s.txt": "s"})
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: "/w", Write: true, Allow: allow}})
	for _, c := range []struct{ body, want string }{
		{`fs.readFileSync(".env")`, "EACCES EACCES: permission denied, open '.env'"},
		{`fs.readFileSync("env-link")`, "EACCES EACCES: permission denied, open 'env-link'"},
		{`fs.readFileSync("dir/env")`, "EACCES EACCES: permission denied, open 'dir/env'"},
		{`fs.statSync(".env")`, "EACCES EACCES: permission denied, stat '.env'"},
		{`fs.copyFileSync("env-link", "copy")`, "EACCES EACCES: permission denied, copyfile 'env-link' -> 'copy'"},
		{`fs.realpathSync("env-link")`, "EACCES EACCES: permission denied, realpath 'env-link'"},
		{`fs.readFileSync("hidden.txt")`, "ENOENT ENOENT: no such file or directory, open 'hidden.txt'"},
		{`fs.readFileSync("perm.txt")`, "EPERM EPERM: operation not permitted, open 'perm.txt'"},
		{`fs.writeFileSync("secret/n.txt", "x")`, "EACCES EACCES: permission denied, open 'secret/n.txt'"},
		{`fs.rmSync("secret/s.txt")`, "EACCES EACCES: permission denied, rm 'secret/s.txt'"},
		{`await fsp.readFile(".env")`, "EACCES EACCES: permission denied, open '.env'"},
	} {
		res := h.run(`try { ` + c.body + `; return "no error" } catch (e) { return e.code + " " + e.message }`)
		require.Equal(t, c.want, res, c.body)
	}
	require.Equal(t, "false", h.run(`return fs.existsSync(".env")`))
	require.Equal(t, "s", h.run(`return fs.readFileSync("secret/s.txt", "utf8")`))
	mu.Lock()
	calls = nil
	mu.Unlock()
	h.run(`fs.readFileSync("dir/../a.txt"); fs.writeFileSync("new.txt", "n"); fs.readdirSync("dir")`)
	mu.Lock()
	// The path as written (a.txt), then each directory the resolution
	// walks (dir), then the paths of the next two calls.
	require.Equal(t, []string{"read /w/a.txt", "read /w/dir", "write /w/new.txt", "read /w/dir"}, calls)
	mu.Unlock()
	// A name is listed: the hook guards paths, not the listings of their
	// directory.
	require.Contains(t, h.run(`return fs.readdirSync(".")`), `".env"`)
}

func TestMemoryLimit(t *testing.T) {
	small := strings.Repeat("y", 512<<10)
	files := map[string]string{"big.txt": strings.Repeat("x", 6<<20), "small.txt": small}
	newH := func() *harness {
		h := newHarnessRT(t, moejs.Options{MemoryLimit: 4 << 20}, node.Options{FS: &node.FSOptions{}})
		h.write(files)
		return h
	}
	h := newH()
	res := h.run(`try { fs.readFileSync("big.txt") } catch (e) { return [e.name, e.code, e.message] }`)
	require.Equal(t, `["RangeError","ERR_FS_FILE_TOO_LARGE","File size (6291456) is greater than the memory limit (4194304)"]`, res)
	res = h.run(`try { await fsp.readFile("big.txt", "utf8") } catch (e) { return [e.name, e.code] }`)
	require.Equal(t, `["RangeError","ERR_FS_FILE_TOO_LARGE"]`, res)
	res = h.run(`return await new Promise(res => fs.readFile("big.txt", e => res(e.code)))`)
	require.Equal(t, `ERR_FS_FILE_TOO_LARGE`, res)
	// Reading and dropping runs on.
	require.Equal(t, "64", h.run(`let n = 0; for (let i = 0; i < 64; i++) n += fs.readFileSync("small.txt", "utf8").length > 0; return n`))
	require.Equal(t, "64", h.run(`let n = 0; for (let i = 0; i < 64; i++) n += (await fsp.readFile("small.txt")).length > 0; return n`))
	// Keeping what is read passes the limit and stops the program.
	_, err := newH().runErr(`const kept = []; for (let i = 0; i < 64; i++) kept.push(fs.readFileSync("small.txt", "utf8")); return kept.length`)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	_, err = newH().runErr(`const kept = []; for (let i = 0; i < 64; i++) kept.push(await fsp.readFile("small.txt")); return kept.length`)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	_, err = newH().runErr(`const kept = []; for (let i = 0; i < 64; i++) kept.push(await new Promise(r => fs.readFile("small.txt", "latin1", (e, s) => r(s)))); return kept.length`)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	_, err = newH().runErr(`const { Buffer } = await import("node:buffer"); const s = fs.readFileSync("small.txt", "latin1");
	const kept = []; for (let i = 0; i < 64; i++) kept.push(Buffer.from(s, "latin1")); return kept.length`)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
}

func TestClose(t *testing.T) {
	h := newHarness(t, node.Options{FS: &node.FSOptions{}})
	h.write(map[string]string{"a.txt": "a"})
	require.NoError(t, h.api.Close())
	require.NoError(t, h.api.Close())
	res := h.run(`const out = []; try { fs.readFileSync("a.txt") } catch (e) { out.push(e.code) }
	try { await fsp.readFile("a.txt") } catch (e) { out.push(e.code) }
	out.push(await new Promise(res => fs.readFile("a.txt", e => res(e.code))))
	out.push(fs.existsSync("a.txt"))
	return out`)
	require.Equal(t, `["EBADF","EBADF","EBADF",false]`, res)
}

func TestNotInstalled(t *testing.T) {
	h := newHarness(t, node.Options{})
	res := h.run(`try { await import("node:fs") } catch (e) { return e.message }`)
	require.Equal(t, `Cannot import "node:fs": this runtime has no file system`, res)
	require.Equal(t, "true", h.run(`const { Buffer } = await import("buffer"); return typeof Buffer.from === "function"`))
	require.Equal(t, "undefined", h.run(`return typeof Buffer`))
	res = h.run(`try { await import("node:path") } catch (e) { return e.message }`)
	require.Equal(t, `No such built-in module: node:path`, res)
	// A runtime without Install.
	rt := moejs.NewRuntime(moejs.Options{Importer: node.Importer(nil)})
	_ = rt
}

func TestNotImplemented(t *testing.T) {
	h := newHarness(t, node.Options{FS: &node.FSOptions{}})
	res := h.run(`try { fs.createReadStream("a") } catch (e) { return [e.code, e.message] }`)
	require.Equal(t, `["ERR_METHOD_NOT_IMPLEMENTED","The fs.createReadStream() method is not implemented"]`, res)
	res = h.run(`try { await fsp.open("a") } catch (e) { return [e.code, e.message] }`)
	require.Equal(t, `["ERR_METHOD_NOT_IMPLEMENTED","The fsPromises.open() method is not implemented"]`, res)
	res = h.run(`try { fs.statSync(".", {bigint: true}) } catch (e) { return e.code }`)
	require.Equal(t, `ERR_METHOD_NOT_IMPLEMENTED`, res)
	res = h.run(`const m = await import("node:fs/promises"); return [typeof m.readFile, m.default === fsp, typeof m.constants.R_OK]`)
	require.Equal(t, `["function",true,"number"]`, res)
	res = h.run(`const m = await import("fs"); return [m === fs, m.default.readFileSync === fs.readFileSync, m.default.promises === fsp]`)
	require.Equal(t, `[true,true,true]`, res)
}

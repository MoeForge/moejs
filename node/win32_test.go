package node_test

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// TestWin32Paths runs the package with the Windows path forms on any
// system: drive letters, UNC shares, rooted and drive-relative paths,
// either separator, without case in the mount check, and backslashes in
// the paths the package builds.
func TestWin32Paths(t *testing.T) {
	node.SetWin32ForTest(true)
	defer node.SetWin32ForTest(false)
	t.Run("Forms", testWin32Forms)
	t.Run("Cwd", testWin32Cwd)
	t.Run("Escapes", testWin32Escapes)
	t.Run("Output", testWin32Output)
	t.Run("Allow", testWin32Allow)
	t.Run("FileURL", testWin32FileURL)
	t.Run("Glob", testWin32Glob)
	t.Run("Errors", testWin32Errors)
	t.Run("Options", testWin32Options)
}

// jsq quotes s as a JavaScript string.
func jsq(s string) string { return strconv.Quote(s) }

func testWin32Forms(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{"a.txt": "a", "sub/b.txt": "b"})
	read := func(h *harness, p string) string {
		t.Helper()
		return h.run(`try { return fs.readFileSync(` + jsq(p) + `, "utf8") } catch (e) { return e.code }`)
	}
	for _, mount := range []string{`C:\work`, `c:/work`, `C:\work\`} {
		h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: mount}})
		for _, p := range []string{`C:\work\a.txt`, `C:/work/a.txt`, `c:\WORK\a.txt`, `C:\work/sub\..\a.txt`,
			`\work\a.txt`, `/work/a.txt`, `a.txt`, `.\a.txt`, `sub\..\a.txt`, `C:a.txt`, `c:a.txt`, `C:\\work\\\a.txt`} {
			require.Equal(t, "a", read(h, p), "%s with mount %s", p, mount)
		}
		require.Equal(t, "b", read(h, `sub\b.txt`), mount)
	}
	// A share.
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `\\server\share`}})
	for _, p := range []string{`\\server\share\a.txt`, `//server/share/a.txt`, `\\SERVER\Share\sub\..\a.txt`,
		`\\server\share\..\a.txt`, `\a.txt`, `a.txt`, `\\?\UNC\server\share\a.txt`} {
		require.Equal(t, "a", read(h, p), p)
	}
	// A directory of a share.
	h = newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `//server/share/dir`}})
	require.Equal(t, "a", read(h, `\\server\share\dir\a.txt`))
	require.Equal(t, "a", read(h, `\dir\a.txt`))
	// No volume: every rooted path is on the mount's.
	h = newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: "/workspace"}})
	for _, p := range []string{`/workspace/a.txt`, `\workspace\a.txt`, `\WorkSpace\sub\b.txt\..\..\a.txt`, `a.txt`} {
		require.Equal(t, "a", read(h, p), p)
	}
	// A \\?\ path of a drive is the drive's path; other device paths are
	// outside.
	h = newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `C:\work`}})
	require.Equal(t, "a", read(h, `\\?\C:\work\a.txt`))
	require.Equal(t, "a", read(h, `\\.\C:\work\a.txt`))
	require.Equal(t, "EACCES", read(h, `\\.\pipe\x`))
	require.Equal(t, "EACCES", read(h, `\\?\Volume{0}\a.txt`))
}

func testWin32Cwd(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{"a.txt": "a", "proj/b.txt": "b"})
	for _, cwd := range []string{`C:\work\proj`, `c:/WORK/proj`, `proj`, `.\proj\`, `\work\proj`} {
		h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `C:\work`, Cwd: cwd}})
		require.Equal(t, "b", h.run(`return fs.readFileSync("b.txt", "utf8")`), cwd)
		require.Equal(t, "a", h.run(`return fs.readFileSync("..\\a.txt", "utf8")`), cwd)
		require.Equal(t, "b", h.run(`return fs.readFileSync("C:b.txt", "utf8")`), cwd)
		require.Equal(t, `C:\work\proj`, h.run(`return fs.realpathSync(".")`), cwd)
	}
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: "/workspace", Cwd: "proj"}})
	require.Equal(t, "b", h.run(`return fs.readFileSync("b.txt", "utf8")`))
	require.Equal(t, `\workspace\proj`, h.run(`return fs.realpathSync(".")`))
	h = newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `\\srv\sh`, Cwd: `\\srv\sh\proj`}})
	require.Equal(t, "b", h.run(`return fs.readFileSync("b.txt", "utf8")`))
	require.Equal(t, `\\srv\sh\proj`, h.run(`return fs.realpathSync(".")`))
}

func testWin32Escapes(t *testing.T) {
	base, root := outside(t)
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `C:\root`, Write: true}})
	h.write(map[string]string{
		"a.txt":      "a",
		"sub/b.txt":  "b",
		"abs-out":    "->" + filepath.Join(base, "secret.txt"),
		"rel-out":    "->../secret.txt",
		"dir-out":    "->../secretdir",
		"abs-in":     "->" + filepath.Join(root, "a.txt"),
		"abs-in-dir": "->" + filepath.Join(root, "sub"),
		"sub/rel-in": "->../a.txt",
	})
	for _, body := range []string{
		`fs.readFileSync("..\\secret.txt")`,
		`fs.readFileSync("../secret.txt")`,
		`fs.readFileSync("sub\\..\\..\\secret.txt")`,
		`fs.readFileSync("C:\\root\\..\\secret.txt")`,
		`fs.readFileSync("C:\\root2\\a.txt")`,
		`fs.readFileSync("C:\\rootx")`,
		`fs.readFileSync("C:\\secret.txt")`,
		`fs.readFileSync("D:\\root\\a.txt")`,
		`fs.readFileSync("D:a.txt")`,
		`fs.readFileSync("\\\\server\\share\\a.txt")`,
		`fs.readFileSync("\\..\\secret.txt")`,
		`fs.readFileSync("abs-out")`,
		`fs.readFileSync("rel-out")`,
		`fs.readFileSync("dir-out\\s.txt")`,
		`fs.realpathSync("..")`,
		`fs.writeFileSync("..\\new.txt", "x")`,
		`fs.renameSync("a.txt", "..\\moved.txt")`,
		`fs.copyFileSync("a.txt", "dir-out\\c.txt")`,
		`fs.mkdirSync("D:\\x", {recursive: true})`,
	} {
		res := h.run(`try { ` + body + `; return "no error" } catch (e) { return e.code }`)
		require.Equal(t, "EACCES", res, body)
	}
	// Another share, or a drive, from a share.
	hs := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `\\server\share`}})
	for _, p := range []string{`\\server\other\a.txt`, `\\other\share\a.txt`, `C:\a.txt`, `C:a.txt`} {
		res := hs.run(`try { fs.readFileSync(` + jsq(p) + `) } catch (e) { return e.code }`)
		require.Equal(t, "EACCES", res, p)
	}
	// Links inside the root, absolute ones included, work.
	require.Equal(t, "a", h.run(`return fs.readFileSync("abs-in", "utf8")`))
	require.Equal(t, `["b.txt","rel-in"]`, h.run(`return fs.readdirSync("abs-in-dir")`))
	require.Equal(t, "a", h.run(`return fs.readFileSync("sub\\rel-in", "utf8")`))
	require.Equal(t, `C:\root\a.txt`, h.run(`return fs.realpathSync("abs-in")`))
}

func testWin32Output(t *testing.T) {
	root := realDirT(t)
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `c:\work`, Cwd: `proj`, Write: true}})
	h.write(map[string]string{
		"proj/a.txt":      "a",
		"proj/tree/x.txt": "x",
		"proj/tree/s/y":   "y",
		"proj/link-rel":   "->a.txt",
		"proj/link-up":    "->..",
		"proj/link-abs":   "->" + filepath.Join(root, "proj", "a.txt"),
	})
	// realpath: backslashes, and the mount as spelt.
	require.Equal(t, `c:\work\proj\a.txt`, h.run(`return fs.realpathSync("a.txt")`))
	require.Equal(t, `c:\work\proj`, h.run(`return fs.realpathSync(".")`))
	require.Equal(t, `c:\work`, h.run(`return fs.realpathSync("..")`))
	require.Equal(t, `c:\work\proj\a.txt`, h.run(`return fs.realpathSync("link-rel")`))
	require.Equal(t, `c:\work`, h.run(`return fs.realpathSync("link-up")`))
	require.Equal(t, `c:\work\proj\a.txt`, h.run(`return fs.realpathSync("link-abs")`))
	require.Equal(t, `c:\work\proj\a.txt`, h.run(`return await fsp.realpath("C:/WORK/proj/a.txt")`))
	// readlink: an absolute target under the root as JavaScript sees it.
	require.Equal(t, `c:\work\proj\a.txt`, h.run(`return fs.readlinkSync("link-abs")`))
	require.Equal(t, `a.txt`, h.run(`return fs.readlinkSync("link-rel")`))
	// A recursive listing joins the names with backslashes.
	require.Equal(t, `["s","x.txt","s\\y"]`, h.run(`return fs.readdirSync("tree", {recursive: true})`))
	require.Equal(t, `["s","x.txt","s\\y"]`, h.run(`return await fsp.readdir("tree", {recursive: true})`))
	require.Equal(t, `[["s","tree"],["x.txt","tree"],["y","tree\\s"]]`,
		h.run(`return fs.readdirSync("tree", {recursive: true, withFileTypes: true}).map(d => [d.name, d.parentPath])`))
	require.Equal(t, `[["s","C:/work/proj/tree"],["x.txt","C:/work/proj/tree"],["y","C:\\work\\proj\\tree\\s"]]`,
		h.run(`return fs.readdirSync("C:/work/proj/tree", {recursive: true, withFileTypes: true}).map(d => [d.name, d.parentPath])`))
	// mkdir recursive returns the first directory it made, namespaced, as
	// Node's toNamespacedPath has it.
	require.Equal(t, `\\?\c:\work\proj\m\a`, h.run(`fs.mkdirSync("m"); return fs.mkdirSync("m\\a\\b", {recursive: true})`))
	require.Equal(t, `\\?\c:\work\proj\n`, h.run(`return await fsp.mkdir("C:/work/proj/n/a", {recursive: true})`))
	require.Equal(t, "undefined", h.run(`return fs.mkdirSync("m\\a", {recursive: true})`))
	// What the program gave stays as it gave it.
	require.Equal(t, "ENOENT C:/WORK/nope", h.run(`try { fs.readFileSync("C:/WORK/nope") } catch (e) { return e.code + " " + e.path }`))
	// realpathSync fails with the path of the component it reached.
	require.Equal(t, `ENOENT lstat c:\work\proj\nope`, h.run(`try { fs.realpathSync("nope\\x") } catch (e) { return e.code + " " + e.syscall + " " + e.path }`))

	// A volume's root, a share and no volume as the mount.
	hr := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `D:\`}})
	require.Equal(t, `D:\proj\a.txt`, hr.run(`return fs.realpathSync("proj/a.txt")`))
	require.Equal(t, `D:\`, hr.run(`return fs.realpathSync(".")`))
	hs := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `\\srv\sh`}})
	require.Equal(t, `\\srv\sh\proj\a.txt`, hs.run(`return fs.realpathSync("proj/a.txt")`))
	hw := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: "/workspace"}})
	require.Equal(t, `\workspace\proj\a.txt`, hw.run(`return fs.realpathSync("proj/a.txt")`))
}

func testWin32Allow(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	allow := func(op, p string) error {
		mu.Lock()
		calls = append(calls, op+" "+p)
		mu.Unlock()
		return nil
	}
	root := realDirT(t)
	writeTree(t, root, map[string]string{"dir/a.txt": "a"})
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `C:\w`, Write: true, Allow: allow}})
	h.run(`fs.readFileSync("c:/W/dir/a.txt"); fs.writeFileSync("dir\\n.txt", "n")`)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{`read C:\w\dir`, `read C:\w\dir\a.txt`, `read C:\w\dir`, `write C:\w\dir\n.txt`}, calls)
}

func testWin32FileURL(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{"x.txt": "x", "a b.txt": "a b"})
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `C:\w`}})
	try := func(h *harness, href string) string {
		t.Helper()
		// The harness has no URL: an object with href and protocol is a
		// URL to the package, as to Node's isURL.
		return h.run(`try { return fs.readFileSync({href: ` + jsq(href) + `, protocol: "file:"}, "utf8") } catch (e) { return e.code + " " + e.message }`)
	}
	require.Equal(t, "x", try(h, "file:///C:/w/x.txt"))
	require.Equal(t, "x", try(h, "file:///c:/W/x.txt"))
	require.Equal(t, "x", try(h, "file://localhost/C:/w/x.txt"))
	require.Equal(t, "a b", try(h, "file:///C:/w/a%20b.txt"))
	require.Equal(t, "ERR_INVALID_FILE_URL_PATH File URL path must be absolute", try(h, "file:///w/x.txt"))
	require.Equal(t, `ERR_INVALID_FILE_URL_PATH File URL path must not include encoded \ or / characters`, try(h, "file:///C:/w%2Fx.txt"))
	require.Equal(t, `ERR_INVALID_FILE_URL_PATH File URL path must not include encoded \ or / characters`, try(h, "file:///C:/w%5cx.txt"))
	require.Equal(t, `EACCES EACCES: permission denied, open 'D:\w\x.txt'`, try(h, "file:///D:/w/x.txt"))
	// A host names a share.
	hs := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `\\host\sh`}})
	require.Equal(t, "x", try(hs, "file://host/sh/x.txt"))
	require.Equal(t, "x", try(hs, "file://HOST/sh/x.txt"))
}

func testWin32Glob(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{"a.txt": "a", "sub/b.txt": "b", "sub/c.js": "c"})
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `C:\w`}})
	require.Equal(t, `["a.txt"]`, h.run(`return fs.globSync("*.txt")`))
	require.Equal(t, `["sub\\b.txt","sub\\c.js"]`, h.run(`return fs.globSync("sub\\*")`))
	require.Equal(t, `["sub\\b.txt"]`, h.run(`return fs.globSync("**/*.txt", {exclude: ["a.txt"]})`))
	require.Equal(t, `["C:\\w\\a.txt"]`, h.run(`return fs.globSync("C:\\w\\*.txt")`))
	require.Equal(t, `["C:\\w\\sub\\b.txt"]`, h.run(`return fs.globSync("C:/w/sub/*.txt")`))
	require.Equal(t, `["c:\\w\\sub\\b.txt"]`, h.run(`return fs.globSync("c:/w/**/b.txt")`))
	require.Equal(t, `[]`, h.run(`return fs.globSync("D:/w/*.txt")`))
	require.Equal(t, `["b.txt"]`, h.run(`return fs.globSync("*.txt", {cwd: "C:\\w\\sub"})`))
	require.Equal(t, `[["b.txt","sub"]]`, h.run(`return fs.globSync("sub/*.txt", {withFileTypes: true}).map(d => [d.name, d.parentPath])`))
	require.Equal(t, `[["b.txt","C:\\w\\sub"]]`, h.run(`return fs.globSync("*.txt", {cwd: "C:/w/sub", withFileTypes: true}).map(d => [d.name, d.parentPath])`))
	require.Equal(t, `[["b.txt","C:\\w\\sub"]]`, h.run(`return fs.globSync("C:/w/sub/*.txt", {withFileTypes: true}).map(d => [d.name, d.parentPath])`))
	hs := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `\\srv\sh`}})
	require.Equal(t, `["\\\\srv\\sh\\a.txt"]`, hs.run(`return fs.globSync("\\\\srv\\sh\\*.txt")`))
	require.Equal(t, `["\\\\srv\\sh\\sub\\b.txt"]`, hs.run(`return fs.globSync("//srv/sh/sub/*.txt")`))
}

func testWin32Errors(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{"a.txt": "a", "d/": "", "d2/": "", "full/x": "x"})
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: `C:\w`, Write: true}})
	code := func(body string) string {
		t.Helper()
		return h.run(`try { ` + body + `; return "no error" } catch (e) { return e.code }`)
	}
	// What libuv reports on Windows.
	require.Equal(t, "ENOENT", code(`fs.rmdirSync("a.txt")`))
	require.Equal(t, "EPERM", code(`fs.unlinkSync("d")`))
	require.Equal(t, "EPERM", code(`fs.renameSync("d", "d2")`))
	require.Equal(t, "EPERM", code(`fs.renameSync("a.txt", "d")`))
	require.Equal(t, "ENOTDIR", code(`fs.readdirSync("a.txt")`))
	require.Equal(t, "ENOENT", code(`fs.readFileSync("nope.txt")`))
	require.Equal(t, "EEXIST", code(`fs.mkdirSync("d")`))
	require.Equal(t, "ENOTEMPTY", code(`fs.rmdirSync("full")`))
	require.Equal(t, "EACCES", code(`fs.readFileSync("D:\\x")`))
	require.Equal(t, "no error", code(`fs.renameSync("a.txt", "b.txt")`))
}

func testWin32Options(t *testing.T) {
	root := realDirT(t)
	loop := newHarness(t, node.Options{}).loop
	for _, c := range []struct{ mount, cwd, err string }{
		{"work", "", "not an absolute path"},
		{"C:work", "", "not an absolute path"},
		{`\\.\pipe\x`, "", "not an absolute path"},
		{`C:\w`, `D:\w`, "not under the mount"},
		{`C:\w`, `..`, "not under the mount"},
		{`C:\w`, `C:\wx`, "not under the mount"},
		{`\\s\sh`, `\\s\other`, "not under the mount"},
	} {
		_, err := node.InstallLoop(loop, node.Options{FS: &node.FSOptions{Root: root, Mount: c.mount, Cwd: c.cwd}})
		require.ErrorContains(t, err, c.err, "%s %s", c.mount, c.cwd)
	}
}

package node_test

// Tests of the sandbox's edges: escapes through /proc and odd paths, the
// Allow hook over whole trees and as an oracle, interrupts of long native
// loops, link cycles, the memory a program's reads take in Go, and
// Buffer.concat over a huge sparse array.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// treeBeside is a root inside base, with a secret beside it whose content
// and names occur nowhere inside the root.
func treeBeside(t *testing.T) (base, root string) {
	t.Helper()
	base = realDirT(t)
	root = filepath.Join(base, "root")
	writeTree(t, base, map[string]string{
		"root/":                        "",
		"secretdir/SECRETNAME.txt":     "SECRET",
		"secretdir/sub/SECRETDEEP.txt": "SECRET",
		"secret.txt":                   "SECRET",
	})
	return base, root
}

// TestEscapesMore tries the escapes the package's own tests do not:
// links to /proc's magic links, rm -rf through and of links that lead
// out, cp and glob and recursive readdir over such links, and odd paths.
func TestEscapesMore(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprintf("noFast=%v", slow), func(t *testing.T) {
			node.SetNoFast(slow)
			defer node.SetNoFast(false)
			base, root := treeBeside(t)
			dirfd, err := os.Open(filepath.Join(base, "secretdir"))
			require.NoError(t, err)
			defer dirfd.Close()
			writeTree(t, root, map[string]string{
				"a.txt":           "a",
				"proc-root":       "->/proc/self/root",
				"proc-cwd":        "->/proc/self/cwd",
				"proc-fd":         "->/proc/self/fd/" + strconv.Itoa(int(dirfd.Fd())),
				"proc-fds":        "->/proc/self/fd",
				"rel-proc":        "->../../../../../../../../../../proc/self/root",
				"dir-out":         "->" + filepath.Join(base, "secretdir"),
				"box/inner-out":   "->../../secretdir",
				"box/f.txt":       "f",
				"loop/self":       "->.",
				"abs-in-via-proc": "->/proc/self/root" + root + "/a.txt",
				"chain1":          "->chain2",
				"chain2":          "->../secretdir",
				"unicode-‥":       "u",
				"back\\slash.txt": "b",
			})
			h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Write: true}})
			body := `
			const out = [];
			const B = ` + quote(base) + `;
			const tries = {
				procRoot: () => fs.readFileSync("proc-root" + B + "/secret.txt", "utf8"),
				procCwd: () => fs.readdirSync("proc-cwd"),
				procFd: () => fs.readdirSync("proc-fd"),
				procFdFile: () => fs.readFileSync("proc-fd/SECRETNAME.txt", "utf8"),
				procFds: () => fs.readdirSync("proc-fds"),
				relProc: () => fs.readFileSync("rel-proc" + B + "/secret.txt", "utf8"),
				absInViaProc: () => fs.readFileSync("abs-in-via-proc", "utf8"),
				realProc: () => fs.realpathSync("proc-root"),
				readlinkProc: () => fs.readlinkSync("proc-root"),
				chain: () => fs.readdirSync("chain1"),
				statChain: () => fs.statSync("chain1/SECRETNAME.txt").size,
				existsOut: () => fs.existsSync("dir-out/SECRETNAME.txt"),
				accessOut: () => fs.accessSync("dir-out/SECRETNAME.txt"),
				globFollow: () => fs.globSync("**", {followSymlinks: true}),
				globLit: () => fs.globSync("dir-out/*"),
				globAbs: () => fs.globSync(B + "/secretdir/*"),
				globDotdot: () => fs.globSync("../secretdir/*"),
				globCwd: () => fs.globSync("*", {cwd: ".."}),
				readdirRec: () => fs.readdirSync(".", {recursive: true}),
				readdirRecTypes: () => fs.readdirSync(".", {recursive: true, withFileTypes: true}).map(d => d.parentPath + "/" + d.name),
				cpOut: () => fs.cpSync("box", "box-copy", {recursive: true}),
				cpFromOut: () => fs.cpSync("dir-out", "stolen", {recursive: true}),
				cpChain: () => fs.cpSync("chain1", "stolen2", {recursive: true}),
				copyProc: () => fs.copyFileSync("proc-fd/SECRETNAME.txt", "stolen3"),
				renameProc: () => fs.renameSync("proc-fd/SECRETNAME.txt", "stolen4"),
				rmTrailing: () => fs.rmSync("dir-out/", {recursive: true, force: true}),
				rmdirTrailing: () => fs.rmdirSync("dir-out/", {recursive: true}),
				rmInner: () => fs.rmSync("box", {recursive: true, force: true}),
				rmLink: () => fs.rmSync("dir-out", {recursive: true, force: true}),
				nul: () => fs.readFileSync("a.txt\u0000../../secret.txt", "utf8"),
				nulBuf: () => fs.readFileSync(new Uint8Array([97, 0, 46, 46]), "utf8"),
				backslash: () => fs.readFileSync("..\\secret.txt", "utf8"),
				backslashName: () => fs.readFileSync("back\\slash.txt", "utf8"),
				unicode: () => fs.readFileSync("‥/secret.txt", "utf8"),
				fullwidth: () => fs.readFileSync("．．/secret.txt", "utf8"),
				long: () => fs.readFileSync("a/".repeat(5000) + "x", "utf8"),
				longDots: () => fs.readFileSync("./".repeat(100000) + "a.txt", "utf8"),
				url: () => fs.readFileSync(new URL("file://" + B + "/secret.txt"), "utf8"),
				urlDots: () => fs.readFileSync(new URL("file://" + B + "/root/%2e%2e/secret.txt"), "utf8"),
				urlSlash: () => fs.readFileSync(new URL("file://" + B + "/root/a%2F..%2F..%2Fsecret.txt"), "utf8"),
				statDotdotSlash: () => fs.statSync("../").ino,
				statSubDotdotSlash: () => fs.statSync("box/../../").ino,
				readdirDotdotSlash: () => fs.readdirSync("../"),
				readdirDotdotDot: () => fs.readdirSync("../."),
				realDotdotSlash: () => fs.realpathSync("..//"),
				globDotdotSlash: () => fs.globSync("../*/"),
				rmDotdotSlash: () => fs.rmSync("../", {recursive: true}),
				mkdirDotdotSlash: () => fs.mkdirSync("../newdir/", {recursive: true}),
				renameDotdotSlash: () => fs.renameSync("a.txt", "../"),
				copyDotdotSlash: () => fs.copyFileSync("a.txt", "../x"),
				writeDotdotSlash: () => fs.writeFileSync("box/../../x", "x"),
				mountDotdot: () => fs.readFileSync(` + quote(root) + ` + "/../secret.txt", "utf8"),
				mountTwice: () => fs.readFileSync(` + quote(root) + ` + "/../root/a.txt", "utf8"),
			};
			for (const [k, f] of Object.entries(tries)) {
				try { const v = f(); out.push(k + "=OK " + JSON.stringify(v)); }
				catch (e) { out.push(k + "=" + (e.code || e.name) + " " + e.message); }
			}
			return out.join("\n")`
			res := h.run(body)
			t.Log("\n" + res)
			for _, line := range strings.Split(res, "\n") {
				if strings.Contains(line, "=OK ") {
					require.NotContains(t, line, "SECRET", "a secret leaked")
				}
			}
			// Nothing outside the root changed.
			for _, p := range []string{"secretdir/SECRETNAME.txt", "secretdir/sub/SECRETDEEP.txt", "secret.txt"} {
				b, err := os.ReadFile(filepath.Join(base, p))
				require.NoError(t, err, p)
				require.Equal(t, "SECRET", string(b), p)
			}
			ents, _ := os.ReadDir(base)
			require.Len(t, ents, 3, "files appeared beside the root")
		})
	}
}

// TestRenameRaceInFlight races promise operations on the workers against
// renames on the loop that swap a directory with a link out of the root.
// The root must hold whatever the interleaving.
func TestRenameRaceInFlight(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprintf("noFast=%v", slow), func(t *testing.T) {
			node.SetNoFast(slow)
			defer node.SetNoFast(false)
			base, root := treeBeside(t)
			writeTree(t, root, map[string]string{
				"in/inside.txt": "inside",
				"in/sub/x.txt":  "x",
				"out":           "->../secretdir",
				"outabs":        "->" + filepath.Join(base, "secretdir"),
			})
			h := newHarnessRT(t, moejs.Options{MemoryLimit: 512 << 20}, node.Options{FS: &node.FSOptions{Root: root, Write: true, Workers: 8}, Buffer: true})
			res := h.run(`
			const seen = new Set();
			let swaps = 0;
			for (let round = 0; round < 10; round++) {
			const ops = [];
			const note = v => { const s = typeof v === "string" ? v : JSON.stringify(v); if (s && s.includes("SECRET")) seen.add(s); return s; };
			const ign = () => null;
			for (let i = 0; i < 400; i++) {
				ops.push(fsp.readFile("in/SECRETNAME.txt", "utf8").then(note, ign));
				ops.push(fsp.readFile("in/sub/SECRETDEEP.txt", "utf8").then(note, ign));
				ops.push(fsp.readFile("in/inside.txt", "utf8").then(note, ign));
				ops.push(fsp.stat("in/SECRETNAME.txt").then(() => note("SECRET stat"), ign));
				ops.push(fsp.readdir("in/sub").then(note, ign));
				ops.push(fsp.readdir("in", {recursive: true}).then(note, ign));
				ops.push(fsp.cp("in", "c" + i, {recursive: true}).then(ign, ign));
				ops.push(fsp.copyFile("in/SECRETNAME.txt", "f" + i).then(ign, ign));
				ops.push(fsp.copyFile("in/inside.txt", "g" + i).then(ign, ign));
				ops.push(fsp.realpath("in/sub").then(note, ign));
				ops.push(Array.fromAsync(fsp.glob("in/**", {followSymlinks: true})).then(note, ign));
				ops.push(fsp.rename("in/sub/x.txt", "in/sub/x.txt").then(ign, ign));
			}
			for (let j = 0; j < 3000; j++) {
				try { fs.renameSync("in", "tmp"); } catch {}
				try { fs.renameSync(j % 2 ? "out" : "outabs", "in"); } catch {}
				try { fs.renameSync("in", j % 2 ? "out" : "outabs"); } catch {}
				try { fs.renameSync("tmp", "in"); swaps++; } catch {}
			}
			await Promise.all(ops);
			}
			return swaps + " " + [...seen].join("|")`)
			t.Log(res)
			require.NotContains(t, res, "SECRET")
			// No copy holds a secret.
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.Type()&fs.ModeSymlink != 0 || d.IsDir() {
					return nil
				}
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), "SECRET") || strings.Contains(p, "SECRET") {
					return errors.New("secret copied into " + p)
				}
				return nil
			})
			require.NoError(t, err)
		})
	}
}

// TestAllowDescendants: the hook is asked about what lies below a
// directory that rm -rf removes or rename moves, so a denied file below
// one it allows stays where it is.
func TestAllowDescendants(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{
		"app/.env":            "KEY=1",
		"app/main.js":         "x",
		"proj/secret/key.pem": "PRIVATE",
		"proj/readme.txt":     "r",
	})
	// The guide's policy (hide .env) and a prefix policy.
	allow := func(op, p string) error {
		if filepath.Base(p) == ".env" {
			return fs.ErrNotExist
		}
		if p == "/w/proj/secret" || strings.HasPrefix(p, "/w/proj/secret/") {
			return errors.New("denied")
		}
		return nil
	}
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: "/w", Write: true, Allow: allow}})
	res := h.run(`
	const out = [];
	const t = (k, f) => { try { out.push(k + "=OK " + JSON.stringify(f())); } catch (e) { out.push(k + "=" + e.code); } };
	t("direct read .env", () => fs.readFileSync("/w/app/.env", "utf8"));
	t("direct rm .env", () => fs.rmSync("/w/app/.env"));
	t("direct read key", () => fs.readFileSync("/w/proj/secret/key.pem", "utf8"));
	t("rename secret", () => fs.renameSync("/w/proj/secret", "/w/x"));
	t("rename parent", () => fs.renameSync("/w/proj", "/w/proj2"));
	t("read key after", () => fs.readFileSync("/w/proj2/secret/key.pem", "utf8"));
	t("rm -rf app", () => fs.rmSync("/w/app", {recursive: true}));
	return out.join("\n")`)
	t.Log("\n" + res)
	_, err := os.Stat(filepath.Join(root, "app/.env"))
	require.NoError(t, err, "rm -rf of the parent removed .env, which the hook denies")
	require.NotContains(t, res, "PRIVATE", "rename of the parent made a denied file readable")
}

// TestAllowOracle: the hook is asked about each directory before the
// path is resolved in it, so a denied directory tells a program nothing
// of the names it holds, of their kinds, or of where its links lead.
func TestAllowOracle(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{
		"secret/present.txt": "p",
		"secret/dir/":        "",
		"secret/link":        "->../pub/x.txt",
		"pub/x.txt":          "x",
	})
	for _, hookErr := range []error{errors.New("denied"), fs.ErrNotExist} {
		t.Run(fmt.Sprint(hookErr), func(t *testing.T) {
			var asked []string
			allow := func(op, p string) error {
				asked = append(asked, op+" "+p)
				if p == "/w/secret" || strings.HasPrefix(p, "/w/secret/") {
					return hookErr
				}
				return nil
			}
			h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: "/w", Allow: allow}})
			res := h.run(`
			const out = [];
			const t = (k, f) => { try { out.push(k + "=OK " + JSON.stringify(f())); } catch (e) { out.push(k + "=" + e.code + " " + e.message); } };
			t("missing", () => fs.statSync("/w/secret/nope.txt"));
			t("present", () => fs.statSync("/w/secret/present.txt"));
			t("file/x", () => fs.statSync("/w/secret/present.txt/x"));
			t("dir/x", () => fs.statSync("/w/secret/dir/x"));
			t("missing/x", () => fs.statSync("/w/secret/nope/x"));
			t("via link", () => fs.readFileSync("/w/secret/link", "utf8"));
			t("realpath link", () => fs.realpathSync("/w/secret/link"));
			return out.join("\n")`)
			t.Log("\n" + res + "\nhook asked: " + strings.Join(asked, ", "))
			lines := strings.Split(res, "\n")
			code := func(i int) string { return strings.SplitN(strings.SplitN(lines[i], "=", 2)[1], " ", 2)[0] }
			require.Equal(t, code(1), code(0), "a missing and a present name in a denied directory fail differently")
			require.Equal(t, code(1), code(2), "a file and a directory in a denied directory fail differently")
			require.Equal(t, code(1), code(4))
			require.NotContains(t, res, "via link=OK", "a link inside a denied directory was followed")
		})
	}
}

// TestMkdirRecursiveAllow: mkdir -p of more than one missing level
// with an Allow hook that allows everything.
func TestMkdirRecursiveAllow(t *testing.T) {
	h := newHarness(t, node.Options{FS: &node.FSOptions{Mount: "/w", Write: true, Allow: func(op, p string) error { return nil }}})
	res := h.run(`
	const out = [];
	const t = (k, f) => { try { out.push(k + "=OK " + JSON.stringify(f())); } catch (e) { out.push(k + "=" + e.code + " " + e.message); } };
	t("mkdir -p a/b/c", () => fs.mkdirSync("a/b/c", {recursive: true}));
	t("cp to new nested", () => { fs.writeFileSync("f.txt", "x"); return fs.cpSync("f.txt", "n1/n2/f.txt"); });
	return out.join("\n")`)
	t.Log("\n" + res)
	// Node.js returns the first directory it made as the path was given
	// (the differential test checks it): "a".
	require.Contains(t, res, `mkdir -p a/b/c=OK "a"`)
	require.Contains(t, res, `cp to new nested=OK undefined`)
}

// TestReadOnlyFlags: with Write unset, nothing changes the tree, whatever
// the flags, numeric ones included.
func TestReadOnlyFlags(t *testing.T) {
	h := newHarness(t, node.Options{FS: &node.FSOptions{}})
	h.write(map[string]string{"a.txt": "a", "d/b.txt": "b"})
	res := h.run(`
	const C = fs.constants, out = [];
	const t = (k, f) => { try { out.push(k + "=OK " + JSON.stringify(f())); } catch (e) { out.push(k + "=" + e.code); } };
	const P = C.O_CREAT | C.O_EXCL;
	for (const fl of ["a", "a+", "w", "w+", "r+", "rs+", "wx", "ax", C.O_RDWR, C.O_WRONLY, C.O_TRUNC, C.O_CREAT | C.O_EXCL,
		C.O_APPEND, 0x410000 | C.O_RDWR, C.O_RDONLY | C.O_TRUNC, 2 ** 31 + 2, -1, 1.5, 512.9])
		t("readFile " + fl, () => fs.readFileSync(fl === P ? "new.txt" : "a.txt", {flag: fl}).length);
	t("writeFile r", () => fs.writeFileSync("a.txt", "x", {flag: "r"}));
	t("appendFile", () => fs.appendFileSync("a.txt", "x"));
	t("mkdir", () => fs.mkdirSync("m", {recursive: true}));
	t("cp", () => fs.cpSync("d", "e", {recursive: true}));
	t("copyFile", () => fs.copyFileSync("a.txt", "c.txt"));
	t("rename", () => fs.renameSync("a.txt", "z.txt"));
	t("rm", () => fs.rmSync("d", {recursive: true}));
	t("rmdir", () => fs.rmdirSync("d", {recursive: true}));
	t("unlink", () => fs.unlinkSync("a.txt"));
	t("access W", () => fs.accessSync("a.txt", C.W_OK));
	return out.join("\n")`)
	t.Log("\n" + res)
	b, err := os.ReadFile(filepath.Join(h.dir, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "a", string(b))
	ents, _ := os.ReadDir(h.dir)
	require.Len(t, ents, 2)
}

// globBomb is a pattern whose segment matcher backtracks through C(n, k)
// splits of a name of n characters that it does not match.
func globBomb(k int) string { return strings.Repeat("*?", k) + "Z" }

// TestGlobInterrupt: a pattern that makes a backtracking matcher
// exponential matches in linear time, and Terminate stops globSync.
func TestGlobInterrupt(t *testing.T) {
	k := 9
	if s := os.Getenv("NODE_TEST_GLOB_K"); s != "" {
		k, _ = strconv.Atoi(s)
	}
	h := newHarness(t, node.Options{FS: &node.FSOptions{}})
	h.write(map[string]string{strings.Repeat("a", 40): ""})
	waited := make(chan time.Duration, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		start := time.Now()
		h.loop.Terminate()
		waited <- time.Since(start)
	}()
	start := time.Now()
	_, err := h.runErr(`return fs.globSync(` + quote(globBomb(k)) + `).length`)
	el := time.Since(start)
	t.Logf("k=%d: globSync ended after %v with %v; Terminate waited %v", k, el, err, <-waited)
	require.Less(t, el, 2*time.Second, "the host's Terminate did not stop globSync")
}

// TestCpLinkCycle: cp dereferences links, so one link back to its
// own directory recurses until the system's link limit (40 on Linux), and
// two make it exponential: 2^40 directories. The host's Terminate stops
// it; the test runs it in a child process, which it kills if it does not.
func TestCpLinkCycle(t *testing.T) {
	if os.Getenv("NODE_TEST_CP_CHILD") != "" {
		root := os.Getenv("NODE_TEST_CP_ROOT")
		h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Write: true}})
		go func() { time.Sleep(200 * time.Millisecond); h.loop.Terminate() }()
		_, err := h.runErr(`return fs.cpSync("src", "dst", {recursive: true})`)
		fmt.Println("CHILD DONE", err)
		return
	}
	for _, links := range []int{1, 2} {
		root := realDirT(t)
		tree := map[string]string{"src/": ""}
		for i := range links {
			tree[fmt.Sprintf("src/l%d", i)] = "->."
		}
		writeTree(t, root, tree)
		ctx := exec.Command(os.Args[0], "-test.run=^TestCpLinkCycle$", "-test.v")
		ctx.Env = append(os.Environ(), "NODE_TEST_CP_CHILD=1", "NODE_TEST_CP_ROOT="+root)
		var out strings.Builder
		ctx.Stdout, ctx.Stderr = &out, &out
		require.NoError(t, ctx.Start())
		done := make(chan error, 1)
		go func() { done <- ctx.Wait() }()
		killed := false
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			ctx.Process.Kill()
			<-done
			killed = true
		}
		dirs, depth := 0, 0
		filepath.WalkDir(filepath.Join(root, "dst"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				dirs++
				depth = max(depth, strings.Count(p, "/"))
			}
			return nil
		})
		t.Logf("links=%d: killed after 4s=%v, %d directories created, depth %d; child: %s",
			links, killed, dirs, depth-strings.Count(filepath.Join(root, "dst"), "/"), lastLine(out.String()))
		if links == 2 {
			require.False(t, killed, "cpSync over two self links ran past the host's Terminate")
		}
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// TestWorkersEnd: the workers end once their queue is empty, and
// Close leaves none behind.
func TestWorkersEnd(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 5 {
		h := newHarness(t, node.Options{FS: &node.FSOptions{Write: true, Workers: 6}})
		h.write(map[string]string{"a.txt": "a"})
		h.run(`
		const ops = [];
		for (let i = 0; i < 300; i++) {
			ops.push(fsp.readFile("a.txt"), fsp.stat("a.txt"), fsp.readdir("."), fsp.writeFile("w" + (i % 7), "x"));
			ops.push(new Promise(r => fs.stat("a.txt", r)));
		}
		await Promise.all(ops);
		return 1`)
		require.NoError(t, h.api.Close())
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("goroutines before %d, after %d", before, runtime.NumGoroutine())
	require.LessOrEqual(t, runtime.NumGoroutine(), before+2)
}

// TestMemoryConcurrentReads: listings and reads under a small memory
// limit fail with an error, and the Go memory the reads in flight take
// stays near the limit.
func TestMemoryConcurrentReads(t *testing.T) {
	for _, mode := range []string{"reads", "readdir"} {
		t.Run(mode, func(t *testing.T) { memoryCase(t, mode) })
	}
}

func memoryCase(t *testing.T, mode string) {
	// The peak should show what the reads hold, not the garbage of those
	// already handed over and dropped, which the collector's pacing
	// decides: collect early.
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 16 << 20}, node.Options{FS: &node.FSOptions{Workers: 8}, Buffer: true})
	big := filepath.Join(h.dir, "big")
	require.NoError(t, os.Mkdir(big, 0o755))
	for i := range 60000 {
		f, err := os.Create(filepath.Join(big, fmt.Sprintf("%0200d", i)))
		require.NoError(t, err)
		f.Close()
	}
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "f15"), make([]byte, 15<<20), 0o644))
	require.NoError(t, h.rt.SetGlobal("MODE", moejs.String(mode)))
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	stop, finished := make(chan struct{}), make(chan struct{})
	peak := uint64(0)
	go func() {
		defer close(finished)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapAlloc)
		}
	}()
	res, err := h.runErr(`
	const out = [];
	const t = async (k, f) => { try { const v = await f(); out.push(k + "=OK " + v); } catch (e) { out.push(k + "=" + (e.code || e.name) + " " + String(e.message).slice(0, 80)); } };
	if (globalThis.MODE === "reads") await t("8 reads of 15 MiB", async () => (await Promise.all(Array.from({length: 8}, () => fsp.readFile("f15").then(b => b.length)))).length);
	else await t("readdir big", () => fs.readdirSync("big").length);
	return out.join("\n")`)
	close(stop)
	<-finished
	t.Logf("\n%s\nerr=%v\nGo heap peak above the start: %d MiB (limit 16 MiB)", res, err, (peak-base)>>20)
	if mode == "reads" {
		// The reads in flight hold at most the limit of the budget, plus
		// the read the loop is charging: not one limit per worker.
		// Eight reads of 15 MiB held at once would be 120 MiB; one or two
		// in flight, and the one the loop hands over, stay well below.
		require.Less(t, int64(peak)-int64(base), int64(80<<20))
	}
}

var _ = syscall.EACCES

// TestReaddirLinkCycle: readdirSync with recursive lists breadth
// first and follows links to directories, as Node.js does, so two links
// back to their own directory list 2^40 paths. The listing is charged to
// the memory limit as it grows, and the host's Terminate stops it. It
// runs in a child process that the test kills if it does not stop.
func TestReaddirLinkCycle(t *testing.T) {
	if os.Getenv("NODE_TEST_RD_CHILD") != "" {
		h := newHarnessRT(t, moejs.Options{MemoryLimit: 64 << 20},
			node.Options{FS: &node.FSOptions{Root: os.Getenv("NODE_TEST_RD_ROOT")}})
		go func() { time.Sleep(200 * time.Millisecond); h.loop.Terminate() }()
		_, err := h.runErr(`return fs.readdirSync("src", {recursive: true}).length`)
		fmt.Println("CHILD DONE", err)
		return
	}
	root := realDirT(t)
	writeTree(t, root, map[string]string{"src/a": "->.", "src/b": "->."})
	cmd := exec.Command(os.Args[0], "-test.run=^TestReaddirLinkCycle$", "-test.v")
	cmd.Env = append(os.Environ(), "NODE_TEST_RD_CHILD=1", "NODE_TEST_RD_ROOT="+root)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	peak, killed := 0, false
	start := time.Now()
wait:
	for {
		select {
		case <-done:
			break wait
		case <-time.After(100 * time.Millisecond):
		}
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid))
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				kb, _ := strconv.Atoi(strings.Fields(line)[1])
				peak = max(peak, kb>>10)
			}
		}
		if peak > 3<<10 || time.Since(start) > 8*time.Second {
			cmd.Process.Kill()
			<-done
			killed = true
			break
		}
	}
	t.Logf("killed=%v after %v, child RSS peak %d MiB (memory limit 64 MiB); child: %s", killed, time.Since(start), peak, lastLine(out.String()))
	require.False(t, killed, "readdirSync over two self links ran past the host's Terminate and the memory limit")
}

// TestConcatHoles: Buffer.concat over new Array(2**32 - 1) fails at the
// first hole, as in Node.js, rather than make room for every item.
func TestConcatHoles(t *testing.T) {
	if os.Getenv("NODE_TEST_CONCAT_CHILD") != "" {
		h := newHarnessRT(t, moejs.Options{MemoryLimit: 32 << 20}, node.Options{Buffer: true})
		res, err := h.runErr(`try { Buffer.concat(new Array(2 ** 32 - 1)); return "no error" } catch (e) { return "caught " + e }`)
		t.Logf("CHILD res=%q err=%v", res, err)
		return
	}
	cmd := exec.Command("bash", "-c", "ulimit -v 8000000; exec "+os.Args[0]+" -test.run=^TestConcatHoles$ -test.v")
	cmd.Env = append(os.Environ(), "NODE_TEST_CONCAT_CHILD=1")
	out, err := cmd.CombinedOutput()
	s := string(out)
	if i := strings.Index(s, "fatal error"); i >= 0 {
		s = s[i:min(len(s), i+200)]
	} else if len(s) > 600 {
		s = s[len(s)-600:]
	}
	t.Logf("child exit: %v\n%s", err, s)
	if strings.Contains(string(out), "fatal error") {
		t.Fatal("Buffer.concat(new Array(2**32-1)) killed the process under an 8 GB address-space limit, past MemoryLimit 32 MiB")
	}
}

package node_test

// Tests of the directory descriptor cache under churn and outside
// renames, of the Allow hook's opacity on every operation, of the budget
// of what the workers hold (no operation waits on itself or on another
// that waits, and an interrupt wedges nothing), and of extglob matching,
// whose time and memory stay bounded.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// manyFiles makes n files with names of 200 characters in each of dirs
// directories below root/top.
func manyFiles(t *testing.T, root, top string, dirs, n int) {
	t.Helper()
	for d := range dirs {
		dir := filepath.Join(root, top, fmt.Sprintf("d%02d", d))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		for i := range n {
			f, err := os.Create(filepath.Join(dir, fmt.Sprintf("%0200d", i)))
			require.NoError(t, err)
			f.Close()
		}
	}
}

// runDeadline runs body and interrupts the loop after d: it returns the
// result, the error, and whether the deadline fired.
func runDeadline(t *testing.T, h *harness, body string, d time.Duration) (string, error, bool) {
	t.Helper()
	var fired atomic.Bool
	stop := make(chan struct{})
	go func() {
		select {
		case <-stop:
		case <-time.After(d):
			fired.Store(true)
			h.loop.Interrupt(errors.New("deadline"))
		}
	}()
	res, err := h.runErr(body)
	close(stop)
	return res, err, fired.Load()
}

// TestBudgetSelfDeadlock: a recursive listing on a worker grows its hold
// of the budget directory by directory; past the limit it fails with the
// limit's error, as the synchronous form does, rather than wait for a
// release that only its own hand-over would make.
func TestBudgetSelfDeadlock(t *testing.T) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 8 << 20}, node.Options{FS: &node.FSOptions{}})
	manyFiles(t, h.dir, "big", 20, 2000) // 40000 entries, about 9.4 MB of listing
	start := time.Now()
	res, err, fired := runDeadline(t, h, `return await fsp.readdir("big", {recursive: true}).then(l => "OK " + l.length, e => "ERR " + e.code + " " + e.message)`, 10*time.Second)
	t.Logf("promise form: %q err=%v deadline fired=%v after %v", res, err, fired, time.Since(start))
	require.NoError(t, h.api.Close())
	require.False(t, fired, "fsp.readdir recursive hung: its worker waits on the budget it holds itself")
}

// TestBudgetTwoListings: two recursive listings that each fit the limit
// alone, run at once: neither waits for the other's part of the budget.
func TestBudgetTwoListings(t *testing.T) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 8 << 20}, node.Options{FS: &node.FSOptions{}})
	manyFiles(t, h.dir, "a", 10, 2200) // 22000 entries, about 5.2 MB each: 60 % of the limit
	manyFiles(t, h.dir, "b", 10, 2200)
	// Each alone.
	res, err, fired := runDeadline(t, h, `return await fsp.readdir("a", {recursive: true}).then(l => l.length)`, 10*time.Second)
	t.Logf("one alone: %q err=%v fired=%v", res, err, fired)
	h.rt.ClearInterrupt()
	start := time.Now()
	res, err, fired = runDeadline(t, h, `
	const n = await Promise.all(["a", "b"].map(d => fsp.readdir(d, {recursive: true}).then(l => l.length, e => e.code)));
	return n.join(" ")`, 10*time.Second)
	t.Logf("two at once: %q err=%v deadline fired=%v after %v", res, err, fired, time.Since(start))
	require.NoError(t, h.api.Close())
	require.False(t, fired, "two concurrent recursive listings deadlocked on the budget")
}

// TestCacheChurn races the descriptor cache: promise operations on
// the workers read through directories that the loop renames, replaces
// with links out of the root, removes and recreates, over more
// directories than the cache holds; with and without an Allow hook that
// denies a directory.
func TestCacheChurn(t *testing.T) {
	for _, withAllow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%v", withAllow), func(t *testing.T) {
			base := realDirT(t)
			root := filepath.Join(base, "root")
			tree := map[string]string{
				"root/pub/x.txt":    "pub",
				"root/decoy/x.txt":  "decoy",
				"root/secret/x.txt": "SECRET",
				"root/out":          "->../outside",
				"root/outabs":       "->" + filepath.Join(base, "outside"),
				"outside/x.txt":     "OUTSIDE",
			}
			for i := range 150 {
				tree[fmt.Sprintf("root/d%03d/x.txt", i)] = "d"
			}
			writeTree(t, base, tree)
			var allow func(op, p string) error
			if withAllow {
				allow = func(op, p string) error {
					if p == "/w/secret" || strings.HasPrefix(p, "/w/secret/") {
						return errors.New("denied")
					}
					return nil
				}
			}
			h := newHarnessRT(t, moejs.Options{MemoryLimit: 512 << 20},
				node.Options{FS: &node.FSOptions{Root: root, Mount: "/w", Write: true, Workers: 8, Allow: allow}})
			res := h.run(`
			const ALLOW = ` + fmt.Sprint(withAllow) + `;
			const seen = new Set(), ign = () => null;
			const note = v => { const s = typeof v === "string" ? v : JSON.stringify(v); if (s && (s.includes("OUTSIDE") || ALLOW && s.includes("SECRET"))) seen.add(s); return s; };
			let ok = 0;
			for (let round = 0; round < 8; round++) {
				const ops = [];
				for (let i = 0; i < 300; i++) {
					ops.push(fsp.readFile("pub/x.txt", "utf8").then(note, ign));
					ops.push(fsp.readFile("m/pub/x.txt", "utf8").then(note, ign));
					ops.push(fsp.stat("pub/x.txt").then(s => s.size === 7 ? note("OUTSIDE size") : s.size === 6 ? note("SECRET size") : null, ign));
					ops.push(fsp.readdir("pub", {withFileTypes: true}).then(l => note(l.map(d => d.name)), ign));
					ops.push(fsp.readFile("d" + String(i % 150).padStart(3, "0") + "/x.txt", "utf8").then(note, ign));
					ops.push(fsp.copyFile("pub/x.txt", "c" + (i % 20)).then(ign, ign));
					ops.push(fsp.access("pub/x.txt").then(ign, ign));
					ops.push(Array.fromAsync(fsp.glob("pub/*")).then(note, ign));
				}
				for (let j = 0; j < 1500; j++) {
					const t = (f) => { try { f(); } catch {} };
					t(() => fs.renameSync("pub", "tmp"));
					t(() => fs.renameSync(j % 3 === 0 ? "out" : j % 3 === 1 ? "outabs" : "decoy", "pub"));
					if (!ALLOW) t(() => fs.renameSync("secret", "pub"));
					t(() => fs.statSync("pub/x.txt"));
					t(() => fs.renameSync("pub", j % 3 === 0 ? "out" : j % 3 === 1 ? "outabs" : "decoy"));
					if (!ALLOW) t(() => fs.renameSync("pub", "secret"));
					t(() => fs.renameSync("tmp", "pub"));
					t(() => fs.mkdirSync("m/pub", {recursive: true}));
					t(() => fs.writeFileSync("m/pub/x.txt", "m"));
					t(() => fs.rmSync("m", {recursive: true}));
					if (ALLOW) { t(() => fs.renameSync("secret", "pub")); t(() => fs.renameSync("/w", "/w/pub")); }
					ok++;
				}
				await Promise.all(ops);
			}
			return ok + " " + [...seen].join("|")`)
			t.Log(res)
			require.NotContains(t, res, "OUTSIDE", "a file outside the root was read")
			if withAllow {
				require.NotContains(t, res, "SECRET", "the denied directory was read")
			}
			ents, _ := os.ReadDir(filepath.Join(base, "outside"))
			require.Len(t, ents, 1)
			err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
					return nil
				}
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), "OUTSIDE") || withAllow && strings.Contains(string(b), "SECRET") && !strings.Contains(p, "/secret/") {
					return errors.New("copied into " + p)
				}
				return nil
			})
			require.NoError(t, err)
		})
	}
}

// TestCacheOutsideRename quantifies the admitted staleness: another
// process moves a directory out of the root, writes a file into it there,
// removes the file and moves the directory back, while the program reads
// that file's path. A read that finds it read a file outside the root.
func TestCacheOutsideRename(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprintf("noFast=%v", slow), func(t *testing.T) {
			node.SetNoFast(slow)
			defer node.SetNoFast(false)
			cacheOutsideRename(t, false)
		})
	}
	// With the cache off, the fast path has only the race the os.Root
	// path has too.
	t.Run("noDescriptorCache", func(t *testing.T) { cacheOutsideRename(t, true) })
}

func cacheOutsideRename(t *testing.T, noCache bool) {
	base := realDirT(t)
	root := filepath.Join(base, "root")
	writeTree(t, base, map[string]string{"root/in/a.txt": "a"})
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, NoDescriptorCache: noCache}})
	var stop atomic.Bool
	moves := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			if os.Rename(filepath.Join(root, "in"), filepath.Join(base, "away")) != nil {
				continue
			}
			os.WriteFile(filepath.Join(base, "away", "s.txt"), []byte("SECRET-OUTSIDE"), 0o644)
			time.Sleep(50 * time.Microsecond)
			os.Remove(filepath.Join(base, "away", "s.txt"))
			os.Rename(filepath.Join(base, "away"), filepath.Join(root, "in"))
			moves++
			time.Sleep(50 * time.Microsecond)
		}
	}()
	res := h.run(`
	let hits = 0, tries = 0;
	const end = Date.now() + 2000;
	while (Date.now() < end) {
		tries++;
		try { fs.statSync("in/a.txt"); } catch {}
		try { if (fs.readFileSync("in/s.txt", "utf8").includes("SECRET")) hits++; } catch {}
	}
	return hits + " of " + tries`)
	stop.Store(true)
	<-done
	t.Logf("reads of a file written outside the root after its directory left it: %s (host moves %d)", res, moves)
}

// TestAllowOracles: with a hook that denies a directory with an
// error of its own, no operation tells what the directory holds, by
// result or by error code.
func TestAllowOracles(t *testing.T) {
	root := realDirT(t)
	writeTree(t, root, map[string]string{
		"secret/present.txt": "SECRET",
		"secret/dir/":        "",
		"secret/inner":       "->../pub",
		"pub/x.txt":          "x",
		"pub/tosecret":       "->../secret/present.txt",
		"pub/todir":          "->../secret/dir",
		"pub/viainner":       "->../secret/inner",
		"allowed/":           "",
	})
	allow := func(op, p string) error {
		if p == "/w/secret" || strings.HasPrefix(p, "/w/secret/") {
			return errors.New("denied")
		}
		return nil
	}
	h := newHarness(t, node.Options{FS: &node.FSOptions{Root: root, Mount: "/w", Write: true, Allow: allow}})
	res := h.run(`
	const out = [];
	const t = (k, f) => { try { out.push(k + "=OK " + JSON.stringify(f())); } catch (e) { out.push(k + "=" + (e.code || e.name)); } };
	for (const p of ["secret/present.txt", "secret/nope.txt", "secret/dir", "secret/present.txt/x", "secret/nope/x", "secret/inner/x.txt"]) {
		t("exists " + p, () => fs.existsSync(p));
		t("access " + p, () => fs.accessSync(p));
		t("stat " + p, () => fs.statSync(p).size);
		t("lstat " + p, () => fs.lstatSync(p).size);
		t("readlink " + p, () => fs.readlinkSync(p));
		t("realpath " + p, () => fs.realpathSync(p));
		t("readdir " + p, () => fs.readdirSync(p));
		t("glob " + p, () => fs.globSync(p));
		t("cp from " + p, () => fs.cpSync(p, "allowed/c", {recursive: true}));
		t("cp to " + p, () => fs.cpSync("pub", p + "/sub", {recursive: true}));
		t("copyFile " + p, () => fs.copyFileSync(p, "allowed/f"));
		t("mkdir " + p, () => fs.mkdirSync(p + "/m/n", {recursive: true}));
		t("rm " + p, () => fs.rmSync(p, {recursive: true, force: true}));
		t("rename " + p, () => fs.renameSync(p, "allowed/r"));
	}
	t("via tosecret", () => fs.readFileSync("pub/tosecret", "utf8"));
	t("via todir", () => fs.readdirSync("pub/todir"));
	t("via viainner", () => fs.readdirSync("pub/viainner"));
	t("readlink viainner", () => fs.readlinkSync("pub/viainner"));
	t("glob all", () => fs.globSync("**", {followSymlinks: true}).filter(p => p.includes("present") || p.includes("secret/")));
	t("glob dot", () => fs.globSync("*/*.txt"));
	t("readdir rec", () => { try { return fs.readdirSync(".", {recursive: true}).filter(p => p.startsWith("secret/")); } catch (e) { return e.code; } });
	t("readdir types", () => fs.readdirSync(".", {withFileTypes: true}).map(d => d.name + ":" + d.isDirectory()));
	t("cp root", () => fs.cpSync(".", "allowed/all", {recursive: true}));
	t("rm pub", () => fs.rmSync("pub", {recursive: true}));
	return out.join("\n")`)
	t.Log("\n" + res)
	require.NotContains(t, res, "SECRET")
	// Every probe of the denied directory fails alike, whether the name
	// exists or not.
	byOp := map[string]map[string]bool{}
	for _, line := range strings.Split(res, "\n") {
		k, v, _ := strings.Cut(line, "=")
		op, p, ok := strings.Cut(k, " ")
		if !ok || !strings.HasPrefix(p, "secret/") {
			continue
		}
		if byOp[op] == nil {
			byOp[op] = map[string]bool{}
		}
		byOp[op][v] = true
	}
	for op, vs := range byOp {
		require.Len(t, vs, 1, "%s answers differently inside the denied directory: %v", op, vs)
	}
	b, err := os.ReadFile(filepath.Join(root, "secret/present.txt"))
	require.NoError(t, err)
	require.Equal(t, "SECRET", string(b))
}

// TestEdgeFixes runs edge cases of Buffer and glob, each in a runtime of
// its own, with the Go heap the case took.
func TestEdgeFixes(t *testing.T) {
	cases := []struct{ name, body string }{
		{"fill transfer", `const b = Buffer.alloc(16); b.fill({ valueOf() { b.buffer.transfer(); return 1; } }, 8); return b.length`},
		{"fill resize", `const rab = new ArrayBuffer(16, {maxByteLength: 16}); const b = Buffer.from(rab); b.fill({ valueOf() { rab.resize(4); return 1; } }, 8); return b.length`},
		{"concat Infinity", `return Buffer.concat([{length: Infinity}]).length`},
		{"concat 2^62", `return Buffer.concat([{length: 2 ** 62}, {length: 2 ** 62}]).length`},
		{"concat holes 2^24", `return Buffer.concat(new Array(2 ** 24)).length`},
		{"concat holes 2^32-1", `return Buffer.concat(new Array(2 ** 32 - 1)).length`},
		{"concat bufs 2^22", `const a = new Array(2 ** 22).fill(Buffer.alloc(0)); return Buffer.concat(a).length`},
		{"concat getter detach", `const a = Buffer.alloc(8, 1); return Buffer.concat([a, {get length() { a.buffer.transfer(); return 8; }}]).length`},
		{"concat total valueOf detach", `const a = Buffer.alloc(64); return Buffer.concat([a, a], {valueOf() { a.buffer.transfer(); return 128; }}).length`},
		{"toJSON 8MiB", `return Buffer.alloc(8 * 2 ** 20).toJSON().data.length`},
		{"stringify 4MiB", `return JSON.stringify(Buffer.alloc(4 * 2 ** 20)).length`},
		{"glob *? x60", `fs.writeFileSync("a".repeat(250), ""); return fs.globSync("*?".repeat(60) + "Z").length`},
		{"extglob", `fs.writeFileSync("a".repeat(250), ""); return fs.globSync("+(a|aa)".repeat(30) + "Z").length`},
		{"extglob nested", `fs.writeFileSync("a".repeat(250), ""); return fs.globSync("+(+(a|aa)|+(aa|a))".repeat(20) + "Z").length`},
		{"extglob neg", `fs.writeFileSync("a".repeat(250), ""); return fs.globSync("!(a)".repeat(40) + "Z").length`},
		{"exclude **", `fs.writeFileSync("a".repeat(250), ""); return fs.globSync("**", {exclude: ["**/".repeat(30) + "Z"]}).length`},
		{"braces", `return fs.globSync("{a,b}".repeat(40)).length`},
		{"glob promise extglob", `fs.writeFileSync("a".repeat(250), ""); return (await Array.fromAsync(fsp.glob("+(a|aa)".repeat(30) + "Z"))).length`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessRT(t, moejs.Options{MemoryLimit: 32 << 20}, node.Options{FS: &node.FSOptions{Write: true}, Buffer: true})
			var ms runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&ms)
			base := ms.TotalAlloc
			start := time.Now()
			res, err, fired := runDeadline(t, h, `try { return "OK " + await (async () => { `+c.body+` })(); } catch (e) { return (e.code || e.name) + " " + String(e.message).slice(0, 70); }`, 10*time.Second)
			runtime.ReadMemStats(&ms)
			t.Logf("%-28s %v | err=%v | %v | allocated %d MiB", c.name, res, err, time.Since(start).Round(time.Millisecond), (ms.TotalAlloc-base)>>20)
			require.False(t, fired, "ran past 10 s")
		})
	}
}

// TestBudgetWedge: after an interrupt during listings that pass the
// budget, the host clears the interrupt and runs the program's next step:
// no worker is left blocked, so a one-byte read finishes.
func TestBudgetWedge(t *testing.T) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 8 << 20}, node.Options{FS: &node.FSOptions{Workers: 2}})
	manyFiles(t, h.dir, "big", 20, 2000)
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "a.txt"), []byte("a"), 0o644))
	before := runtime.NumGoroutine()
	_, err, fired := runDeadline(t, h, `await Promise.all([fsp.readdir("big", {recursive: true}), fsp.readdir("big", {recursive: true})]); return 1`, 3*time.Second)
	t.Logf("step 1: err=%v fired=%v; goroutines %d -> %d", err, fired, before, runtime.NumGoroutine())
	h.rt.ClearInterrupt()
	res, err, fired := runDeadline(t, h, `return await fsp.readFile("a.txt", "utf8")`, 3*time.Second)
	t.Logf("step 2 (a one-byte read): %q err=%v fired=%v; goroutines %d", res, err, fired, runtime.NumGoroutine())
	require.NoError(t, h.api.Close())
	require.False(t, fired, "the interrupt left the workers blocked on the budget: the installation's promise operations are wedged until Close")
}

// TestExtglobScale: the time and Go memory of extglob matching as
// the pattern grows, and whether the host's Interrupt stops it.
func TestExtglobScale(t *testing.T) {
	for _, c := range []struct {
		pat  string
		reps []int
	}{{"+(a|aa)", []int{30, 60, 120, 240}}, {"!(a)", []int{40, 80, 160}}, {"@(a|*(a))", []int{30, 60, 120}}} {
		for _, n := range c.reps {
			h := newHarnessRT(t, moejs.Options{MemoryLimit: 32 << 20}, node.Options{FS: &node.FSOptions{Write: true}})
			var peak atomic.Uint64
			stop := make(chan struct{})
			go func() {
				var m runtime.MemStats
				for {
					select {
					case <-stop:
						return
					case <-time.After(20 * time.Millisecond):
					}
					runtime.ReadMemStats(&m)
					if m.HeapInuse > peak.Load() {
						peak.Store(m.HeapInuse)
					}
				}
			}()
			start := time.Now()
			res, err, fired := runDeadline(t, h, `fs.writeFileSync("a".repeat(250), ""); return fs.globSync(`+quote(strings.Repeat(c.pat, n)+"Z")+`).length`, 3*time.Second)
			el := time.Since(start)
			close(stop)
			t.Logf("%s x%d: %q err=%v fired=%v in %v, heap in use peak %d MiB", c.pat, n, res, err, fired, el.Round(time.Millisecond), peak.Load()>>20)
			if fired && el > 3500*time.Millisecond {
				t.Errorf("%s x%d: the interrupt waited %v", c.pat, n, el-3*time.Second)
			}
			if fired {
				break
			}
		}
	}
}

// TestExtglobMemory: a long extglob pattern against a host file with a
// 64-byte name stays within bounded memory and honours the interrupt. It
// runs in a child process under an address space limit; the parent
// watches its RSS and kills it at 3 GiB.
func TestExtglobMemory(t *testing.T) {
	if os.Getenv("NODE_TEST_EXT_CHILD") != "" {
		h := newHarnessRT(t, moejs.Options{MemoryLimit: 32 << 20}, node.Options{FS: &node.FSOptions{Root: os.Getenv("NODE_TEST_EXT_ROOT")}})
		reps, _ := strconv.Atoi(os.Getenv("NODE_TEST_EXT_REPS"))
		go func() { time.Sleep(time.Second); h.loop.Interrupt("deadline 1s") }()
		start := time.Now()
		_, err := h.runErr(`return fs.globSync(` + quote(strings.Repeat("@(a|*(a))", reps)+"Z") + `).length`)
		fmt.Printf("CHILD DONE after %v: %v\n", time.Since(start), err)
		return
	}
	root := realDirT(t)
	nameLen := 64
	if v := os.Getenv("NODE_TEST_EXT_NAME"); v != "" {
		nameLen, _ = strconv.Atoi(v)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, strings.Repeat("a", nameLen)), nil, 0o644))
	repsList := []int{100, 1000}
	if v := os.Getenv("NODE_TEST_EXT_LIST"); v != "" {
		repsList = nil
		for _, f := range strings.Split(v, ",") {
			n, _ := strconv.Atoi(f)
			repsList = append(repsList, n)
		}
	}
	for _, reps := range repsList {
		cmd := exec.Command("bash", "-c", "ulimit -v 6000000; exec "+os.Args[0]+" -test.run=^TestExtglobMemory$ -test.v")
		cmd.Env = append(os.Environ(), "NODE_TEST_EXT_CHILD=1", "NODE_TEST_EXT_ROOT="+root, "NODE_TEST_EXT_REPS="+strconv.Itoa(reps))
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
			if peak > 3<<10 || time.Since(start) > 30*time.Second {
				cmd.Process.Kill()
				<-done
				killed = true
				break
			}
		}
		s := out.String()
		if i := strings.Index(s, "fatal error"); i >= 0 {
			s = s[i:min(len(s), i+60)]
		} else if i := strings.Index(s, "CHILD DONE"); i >= 0 {
			s = s[i:min(len(s), i+160)]
		} else {
			s = ""
		}
		t.Logf("@(a|*(a)) x%d (pattern %d bytes), name %d bytes, MemoryLimit 32 MiB, interrupt at 1 s: killed=%v after %v, RSS peak %d MiB; %s",
			reps, reps*9+1, nameLen, killed, time.Since(start).Round(time.Millisecond), peak, s)
		if killed || strings.Contains(s, "fatal error") {
			t.Errorf("x%d: the matcher grew past 3 GiB or ran 30 s, past the interrupt and the memory limit", reps)
		}
	}
}

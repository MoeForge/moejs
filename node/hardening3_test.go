package node_test

// Tests of what one call of glob may compile and match, and of the
// workers' budget under many operations at once, an interrupt, Close and
// the runs after them.

import (
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

// childWatch runs this test binary's test name in a child with env, under
// a 6 GB address space, kills it at 3 GiB of RSS or after limit, and
// returns whether it was killed, its RSS peak in MiB, the time, and its
// CHILD line or fatal error.
func childWatch(t *testing.T, name string, env []string, limit time.Duration) (bool, int, time.Duration, string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", "ulimit -v 6000000; exec "+os.Args[0]+" -test.run=^"+name+"$ -test.v")
	cmd.Env = append(append(os.Environ(), "NODE_TEST3_CHILD=1"), env...)
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
		if peak > 3<<10 || time.Since(start) > limit {
			cmd.Process.Kill()
			<-done
			killed = true
			break
		}
	}
	s := out.String()
	if i := strings.Index(s, "fatal error"); i >= 0 {
		s = s[i:min(len(s), i+60)]
	} else if i := strings.Index(s, "CHILD"); i >= 0 {
		s = s[i:min(len(s), i+200)]
	} else {
		s = ""
	}
	return killed, peak, time.Since(start), s
}

// childRun is the child's side: a runtime under a 32 MiB limit over the
// root of NODE_TEST3_ROOT, interrupted after a second, running body.
func childRun(t *testing.T, write bool, body string) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 32 << 20}, node.Options{FS: &node.FSOptions{Root: os.Getenv("NODE_TEST3_ROOT"), Write: write}})
	go func() { time.Sleep(time.Second); h.loop.Interrupt("deadline 1s") }()
	start := time.Now()
	res, err := h.runErr(body)
	fmt.Printf("CHILD DONE after %v: %q %v\n", time.Since(start).Round(time.Millisecond), res, err)
}

const catchExpr = `try { return "OK " + JSON.stringify(%s).slice(0, 80); } catch (e) { return (e.code || e.name) + " " + String(e.message).slice(0, 80); }`

// TestGlobPatternArray: an array that repeats one 64 KiB pattern in many
// slots, as patterns or as exclude patterns, costs what the pattern does:
// the patterns of a call are bounded in all, each distinct one compiled
// once.
func TestGlobPatternArray(t *testing.T) {
	pat := strings.Repeat("+(a|b)", 10900) // 65,400 bytes
	cases := map[string]string{
		"patterns": `fs.globSync(Array(N).fill(P)).length`,
		"exclude":  `fs.globSync("x", {exclude: Array(N).fill(P)}).length`,
	}
	if os.Getenv("NODE_TEST3_CHILD") != "" {
		n := os.Getenv("NODE_TEST3_N")
		childRun(t, false, `const N = `+n+`, P = `+quote(pat)+`; `+fmt.Sprintf(catchExpr, cases[os.Getenv("NODE_TEST3_CASE")]))
		return
	}
	root := realDirT(t)
	for _, c := range []string{"patterns", "exclude"} {
		for _, n := range []int{100, 20000} {
			killed, peak, el, s := childWatch(t, "TestGlobPatternArray", []string{"NODE_TEST3_ROOT=" + root, "NODE_TEST3_CASE=" + c, "NODE_TEST3_N=" + strconv.Itoa(n)}, 30*time.Second)
			t.Logf("%s: Array(%d).fill(64 KiB extglob), MemoryLimit 32 MiB, interrupt at 1 s: killed=%v after %v, RSS peak %d MiB; %s", c, n, killed, el.Round(time.Millisecond), peak, s)
			if killed || strings.Contains(s, "fatal") {
				t.Errorf("%s x%d grew past 3 GiB or ran 30 s, past the memory limit and the interrupt", c, n)
			}
		}
	}
}

// TestGlobNegCPU: !(...) tries every split of a name with three
// expressions; the work counted is the bytes each try reads times its
// expression's size, and the interrupt is checked as it grows.
func TestGlobNegCPU(t *testing.T) {
	if os.Getenv("NODE_TEST3_CHILD") != "" {
		childRun(t, false, `const P = `+quote(os.Getenv("NODE_TEST3_PAT"))+`; `+fmt.Sprintf(catchExpr, `fs.globSync(P).length`))
		return
	}
	for _, c := range []struct {
		unit    string
		reps    []int
		nameLen int
	}{
		{"*(a|a)", []int{9000}, 64},
	} {
		root := realDirT(t)
		require.NoError(t, os.WriteFile(filepath.Join(root, strings.Repeat("a", c.nameLen)), nil, 0o644))
		for _, reps := range c.reps {
			for _, form := range []string{"!(%s)", "a!(%s)a"} {
				p := fmt.Sprintf(form, strings.Repeat(c.unit, reps))
				killed, peak, el, s := childWatch(t, "TestGlobNegCPU", []string{"NODE_TEST3_ROOT=" + root, "NODE_TEST3_PAT=" + p}, 30*time.Second)
				t.Logf("%s x%d in %q (%d bytes), name %d bytes: killed=%v after %v, RSS %d MiB; %s", c.unit, reps, form, len(p), c.nameLen, killed, el.Round(time.Millisecond), peak, s)
				if killed || el > 5*time.Second {
					t.Errorf("%s x%d: ran %v past an interrupt at 1 s", c.unit, reps, el)
				}
			}
		}
	}
}

// TestGlobRegexpWide: a segment's expression near the instruction cap,
// matched against every name of a large directory, stops at the
// interrupt, or fails the call once its work passes the bound.
func TestGlobRegexpWide(t *testing.T) {
	if os.Getenv("NODE_TEST3_CHILD") != "" {
		childRun(t, false, `const P = `+quote(os.Getenv("NODE_TEST3_PAT"))+`; `+fmt.Sprintf(catchExpr, `fs.globSync(P).length`))
		return
	}
	root := realDirT(t)
	for i := range 3000 {
		f, err := os.Create(filepath.Join(root, fmt.Sprintf("%0200d", i)))
		require.NoError(t, err)
		f.Close()
	}
	for _, p := range []string{
		strings.Repeat("*(0|0)", 9000) + "Z",
		strings.Repeat("@([0-9]|[[:digit:]]|0)", 2700) + "Z",
		strings.Repeat("?(0)", 10000) + "Z",
		"{" + strings.Repeat("*(0|0)", 1500) + "," + strings.Repeat("+(0|00)", 1300) + "}Z",
	} {
		killed, peak, el, s := childWatch(t, "TestGlobRegexpWide", []string{"NODE_TEST3_ROOT=" + root, "NODE_TEST3_PAT=" + p}, 30*time.Second)
		t.Logf("%q... (%d bytes) over 3000 names of 200 bytes: killed=%v after %v, RSS %d MiB; %s", p[:24], len(p), killed, el.Round(time.Millisecond), peak, s)
		if killed || el > 5*time.Second {
			t.Errorf("ran %v past an interrupt at 1 s", el)
		}
	}
}

// TestBudgetMixed: many mixed operations at once under a limit, an
// interrupt in the middle, more operations after ClearInterrupt, and Close
// while operations wait; what the workers hold returns to 0. Under 64 MiB
// the program, which keeps little, runs to its end.
func TestBudgetMixed(t *testing.T) {
	for _, limit := range []int64{64 << 20, 8 << 20} {
		t.Run(fmt.Sprint(limit>>20, "MiB"), func(t *testing.T) { budgetMixed(t, limit) })
	}
}

func budgetMixed(t *testing.T, limit int64) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: limit}, node.Options{FS: &node.FSOptions{Workers: 4}, Buffer: true})
	manyFiles(t, h.dir, "big", 10, 1500) // a listing of about 3.5 MB
	for i := range 6 {
		require.NoError(t, os.WriteFile(filepath.Join(h.dir, fmt.Sprintf("f%d", i)), make([]byte, 1<<20), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "a.txt"), []byte("a"), 0o644))
	mixed := `
	const ops = [];
	for (let i = 0; i < 40; i++) {
		ops.push(fsp.readFile("f" + (i % 6)).then(b => b.length, e => e.code || e.name));
		ops.push(fsp.readdir("big", {recursive: true}).then(l => l.length, e => e.code || e.name));
		ops.push(Array.fromAsync(fsp.glob("big/**")).then(l => l.length, e => e.code || e.name));
		ops.push(new Promise(r => fs.readFile("f" + (i % 6), (e, b) => r(e ? e.code || e.name : b.length))));
		ops.push(fsp.stat("a.txt").then(s => s.size, e => e.code));
		ops.push(fsp.readFile("missing").then(() => 0, e => e.code));
	}
	const res = await Promise.all(ops);
	const count = {};
	for (const r of res) { const k = typeof r === "number" ? "ok" : r; count[k] = (count[k] || 0) + 1; }
	return JSON.stringify(count)`
	// drain runs the loop with the interrupt cleared until the operations
	// in flight have settled, and returns what the workers hold then.
	drain := func() int64 {
		h.rt.ClearInterrupt()
		_, err, fired := runDeadline(t, h, `await new Promise(r => setTimeout(r, 50)); return 1`, 10*time.Second)
		require.False(t, fired, "drain hung")
		_ = err
		return node.BudgetUsed(h.api)
	}
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
		}
	}()
	res, err, fired := runDeadline(t, h, mixed, 30*time.Second)
	close(stop)
	t.Logf("1. mixed: %s err=%v fired=%v used=%d, Go heap in use peak %d MiB", res, err, fired, node.BudgetUsed(h.api), peak.Load()>>20)
	require.False(t, fired, "deadlock")
	require.Zero(t, node.BudgetUsed(h.api), "the program ended with operations holding the budget")
	if limit == 64<<20 {
		// The program keeps only the lengths: it runs to its end.
		require.NoError(t, err)
		require.Equal(t, `{"ok":200,"ENOENT":40}`, res)
		require.Less(t, peak.Load(), uint64(512<<20))
	}
	for i := 0; i < 15; i++ {
		h.rt.ClearInterrupt()
		_, err, fired := runDeadline(t, h, `await new Promise(r => setTimeout(r, 50)); return 1`, 10*time.Second)
		t.Logf("   drain %d: err=%v fired=%v used=%d", i, err, fired, node.BudgetUsed(h.api))
		if err == nil && node.BudgetUsed(h.api) == 0 {
			break
		}
	}
	require.Zero(t, drain())

	res, err, fired = runDeadline(t, h, mixed, 100*time.Millisecond)
	t.Logf("2. interrupted at 100 ms: %s err=%v fired=%v used=%d", res, err, fired, node.BudgetUsed(h.api))
	t.Logf("   after drain: used=%d", drain())
	require.Zero(t, drain())
	res, err, fired = runDeadline(t, h, `return await fsp.readFile("a.txt", "utf8")`, 10*time.Second)
	t.Logf("3. after ClearInterrupt: %q err=%v fired=%v", res, err, fired)
	require.False(t, fired)
	res, err, fired = runDeadline(t, h, mixed, 30*time.Second)
	t.Logf("4. mixed again: %s err=%v fired=%v, after drain %d", res, err, fired, drain())
	require.False(t, fired)
	require.Zero(t, drain())

	require.NoError(t, h.rt.SetGlobal("closeNow", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		go func() { time.Sleep(20 * time.Millisecond); h.api.Close() }()
		return moejs.Undefined(), nil
	})))
	res, err, fired = runDeadline(t, h, `closeNow(); `+mixed, 10*time.Second)
	t.Logf("5. Close in flight: %s err=%v fired=%v, after drain used=%d", res, err, fired, drain())
	require.False(t, fired)
	require.Zero(t, drain())
}

// TestBudgetTerminate: Terminate drops the loop's tasks, the settlements
// of the results ready among them: once the workers end, what they held
// is released without Close.
func TestBudgetTerminate(t *testing.T) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 64 << 20}, node.Options{FS: &node.FSOptions{Workers: 4}})
	manyFiles(t, h.dir, "big", 4, 1500)
	for i := range 4 {
		require.NoError(t, os.WriteFile(filepath.Join(h.dir, fmt.Sprintf("f%d", i)), make([]byte, 1<<20), 0o644))
	}
	go func() { time.Sleep(30 * time.Millisecond); h.loop.Terminate() }()
	_, err := h.runErr(`
	const ops = [];
	for (let i = 0; i < 60; i++) {
		ops.push(fsp.readFile("f" + (i % 4)).then(b => b.length));
		ops.push(fsp.readdir("big", {recursive: true}).then(l => l.length));
	}
	await Promise.all(ops);
	return 1`)
	t.Logf("run: %v", err)
	deadline := time.Now().Add(10 * time.Second)
	for node.BudgetUsed(h.api) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.Zero(t, node.BudgetUsed(h.api))
}

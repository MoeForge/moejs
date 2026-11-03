package node_test

// Tests of the bounds of glob's brace expansion and patterns, and a fuzz
// of glob patterns under a memory limit and an interrupt.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
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

// heapPeak samples the Go heap every millisecond until stop is called,
// which returns the peak of HeapInuse.
func heapPeak() (stop func() uint64) {
	var peak atomic.Uint64
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return func() uint64 {
		close(done)
		<-finished
		return peak.Load()
	}
}

// TestGlobBraceTransient: a 64 KiB pattern whose twelve braces expand to
// 4096 patterns of 64 KiB each fails with the TypeError of the expansion's
// bound before it builds them.
func TestGlobBraceTransient(t *testing.T) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 16 << 20}, node.Options{FS: &node.FSOptions{}})
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stop := heapPeak()
	res, err, fired := runDeadline(t, h, `const p = "{a,b}".repeat(12) + "x".repeat(65000);
	try { await Array.fromAsync(fsp.glob(p)); return "OK" } catch (e) { return e.name + " " + e.message }`, 30*time.Second)
	peak := stop()
	require.NoError(t, err)
	require.False(t, fired)
	require.Contains(t, res, "TypeError The braces of the glob patterns expand to more than")
	require.Less(t, peak, before.HeapInuse+64<<20, "HeapInuse peak")
}

// TestGlobEmptyPatterns: an array of 2**31 empty strings, a new one each
// time it is read, fails once it gives more than the patterns one call
// may have, or stops at the interrupt, without growing the heap. A
// pattern that is not an array fails through the iterator, as Node's
// async generator does.
func TestGlobEmptyPatterns(t *testing.T) {
	h := newHarnessRT(t, moejs.Options{MemoryLimit: 16 << 20}, node.Options{FS: &node.FSOptions{}})
	const handler = `let n = 0; const handler = {get(t, k) { if (k === "length") return 2 ** 31; n++; return "ab".slice(1, 1 + (n & 0)); }};`
	require.Equal(t, "ERR_INVALID_ARG_TYPE", h.run(handler+`
	try { await Array.fromAsync(fsp.glob(new Proxy({}, handler))); return "OK" } catch (e) { return e.code }`))
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stop := heapPeak()
	res, err, fired := runDeadline(t, h, handler+`
	try { await Array.fromAsync(fsp.glob(new Proxy([], handler))); return "OK " + n } catch (e) { return e.name + " " + n }`, 2*time.Second)
	peak := stop()
	t.Logf("%q err=%v fired=%v", res, err, fired)
	if !fired {
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(res, "TypeError"), res)
	}
	require.Less(t, peak, before.HeapInuse+64<<20, "HeapInuse peak")
}

// TestGlobBraceDeep: "{a,b}" repeated N times, up to 13000 (65000 bytes,
// under the bound on a pattern), fails with the expansion's TypeError at
// once, in either form. NODE_BRACE_DEEP_N adds an N.
func TestGlobBraceDeep(t *testing.T) {
	ns := []int{500, 1000, 2000, 13000}
	if n, _ := strconv.Atoi(os.Getenv("NODE_BRACE_DEEP_N")); n > 0 {
		ns = append(ns, n)
	}
	for _, n := range ns {
		h := newHarnessRT(t, moejs.Options{MemoryLimit: 16 << 20}, node.Options{FS: &node.FSOptions{}})
		start := time.Now()
		res, err, fired := runDeadline(t, h, fmt.Sprintf(`const p = "{a,b}".repeat(%d); const out = [];
		try { fs.globSync(p); out.push("OK") } catch (e) { out.push(e.name + " " + e.message) }
		try { await Array.fromAsync(fsp.glob(p)); out.push("OK") } catch (e) { out.push(e.name + " " + e.message) }
		return out.join("; ")`, n), 2*time.Second)
		d := time.Since(start)
		require.NoError(t, err, "N=%d", n)
		require.False(t, fired, "N=%d", n)
		require.Equal(t, 2, strings.Count(res, "TypeError The braces of the glob patterns expand to more than"), "N=%d: %s", n, res)
		require.Less(t, d, time.Second, "N=%d", n)
	}
}

// --- the fuzz -----------------------------------------------------------------------

// fuzzTree is the tree the fuzz globs over.
var fuzzTree = map[string]string{
	"a": "", "b": "", "ab": "", "abc.txt": "", "a,b": "", "{a}": "", ".hidden": "", "x.js": "",
	"dir/": "", "dir/x.js": "", "dir/y.ts": "", "dir/.h": "", "dir/sub/": "", "dir/sub/z.md": "",
	"dir/sub/deep/": "", "dir/sub/deep/a": "", "dir/sub/deep/b.txt": "", "l1": "->dir", "l2": "->dir/sub/deep",
	"aa/": "", "aa/bb/": "", "aa/bb/cc": "", strings.Repeat("n", 200): "", "dir/" + strings.Repeat("m", 120) + ".js": "",
}

// fuzzGen makes random glob patterns.
type fuzzGen struct{ r *rand.Rand }

var fuzzNames = []string{"a", "b", "ab", "abc", "dir", "sub", "deep", "x", "js", "txt", ".", "..", "n", "aa", "bb", "cc", ".h", "{a}", "a,b", "l1"}

// piece writes one random piece of a pattern at nesting depth d.
func (g *fuzzGen) piece(b *strings.Builder, d int) {
	r := g.r
	switch k := r.Intn(26); {
	case k < 4 || d > 4:
		b.WriteString(fuzzNames[r.Intn(len(fuzzNames))])
	case k == 4:
		b.WriteString([]string{"*", "?", "**", "/", "**/", "*.*", "./", "/"}[r.Intn(8)])
	case k == 5:
		b.WriteString([]string{"[abc]", "[!a-z]", "[^x]", "[[:alpha:]]", "[[:digit:]x]", "[a-", "[", "]", "[]]", "[z-a]", "[\\]]"}[r.Intn(11)])
	case k == 6 || k == 7:
		// An extglob, closed or not.
		b.WriteString([]string{"@(", "?(", "*(", "+(", "!("}[r.Intn(5)])
		for i, n := 0, 1+r.Intn(4); i < n; i++ {
			if i > 0 {
				b.WriteByte('|')
			}
			g.seq(b, d+1, 1+r.Intn(3))
		}
		if r.Intn(8) > 0 {
			b.WriteByte(')')
		}
	case k == 8 || k == 9:
		// A list in braces, closed or not.
		b.WriteByte('{')
		for i, n := 0, 1+r.Intn(5); i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			g.seq(b, d+1, r.Intn(3))
		}
		if r.Intn(8) > 0 {
			b.WriteByte('}')
		}
	case k == 10:
		b.WriteString([]string{"{1..3}", "{a..z}", "{001..999}", "{-5..05}", "{1..10..3}", "{1..100000}", "{z..a..2}",
			"{" + strings.Repeat("0", r.Intn(70000)) + "1..20}", "{1..9223372036854775807}"}[r.Intn(9)])
	case k == 11:
		// A long literal.
		b.WriteString(strings.Repeat(fuzzNames[r.Intn(len(fuzzNames))], 1+r.Intn(70000/3)))
	case k == 12:
		// A piece repeated many times.
		var p strings.Builder
		g.piece(&p, d+1)
		if p.Len() > 0 {
			b.WriteString(strings.Repeat(p.String(), 1+r.Intn(70000/p.Len()+1)))
		}
	case k == 13:
		b.WriteString([]string{"{a,b}", "{,}", "@(a|b)", "*(a|*(b))", "!(a)", "+(a|ab)", "**/", "*", "?(*)", "{a,{b,c}}"}[r.Intn(10)])
	default:
		g.seq(b, d+1, 1+r.Intn(4))
	}
}

func (g *fuzzGen) seq(b *strings.Builder, d, n int) {
	for range n {
		if b.Len() > 140000 {
			return
		}
		g.piece(b, d)
	}
}

// pattern returns a random pattern, at most a little over the 64 KiB
// bound most of the time.
func (g *fuzzGen) pattern() string {
	var b strings.Builder
	g.seq(&b, 0, 1+g.r.Intn(6))
	s := b.String()
	if len(s) > 66000 && g.r.Intn(8) > 0 {
		s = s[:g.r.Intn(66000)]
	}
	return s
}

// TestGlobFuzz globs random patterns over a small tree for the duration
// NODE_GLOB_FUZZ (skipped without it), with NODE_GLOB_FUZZ_SEED's seed,
// under a 16 MiB memory limit, interrupting each run at 200 ms. Every run
// must return within a second of the interrupt, with the Go heap growing
// by at most a few times the limit, and fail only with JavaScript errors,
// the limit's or the interrupt.
func TestGlobFuzz(t *testing.T) {
	dur, err := time.ParseDuration(os.Getenv("NODE_GLOB_FUZZ"))
	if err != nil || dur <= 0 {
		t.Skip("NODE_GLOB_FUZZ is not set")
	}
	seed, _ := strconv.ParseInt(os.Getenv("NODE_GLOB_FUZZ_SEED"), 10, 64)
	const limit = 16 << 20
	root := t.TempDir()
	writeTree(t, root, fuzzTree)
	g := &fuzzGen{r: rand.New(rand.NewSource(seed))}
	stats := map[string]int{}
	var slowest time.Duration
	var maxPeak uint64
	failures := 0
	end := time.Now().Add(dur)
	runs := 0
	for iter := 0; time.Now().Before(end) && failures < 20; iter++ {
		runs++
		var pats []string
		for n := 1 + max(0, g.r.Intn(12)-9)*g.r.Intn(40); len(pats) < n; {
			pats = append(pats, g.pattern())
		}
		var pv any = pats[0]
		if len(pats) > 1 || g.r.Intn(4) == 0 {
			pv = pats
		}
		pj, _ := json.Marshal(pv)
		opts := []string{`{}`, `{withFileTypes: true}`, `{exclude: ["dir/**", "{a,b}"]}`, `{exclude: (d) => false}`,
			`{followSymlinks: true}`, `{exclude: ` + string(pj) + `}`}[g.r.Intn(6)]
		call := []string{
			`fs.globSync(P, O).length`,
			`(await Array.fromAsync(fsp.glob(P, O))).length`,
			`await new Promise((res, rej) => fs.glob(P, O, (e, m) => e ? rej(e) : res(m.length)))`,
		}[g.r.Intn(3)]
		body := `const P = ` + string(pj) + `, O = ` + opts + `;
		try { return "OK " + (` + call + `) } catch (e) { return "THROW " + (e && e.name) + " " + String(e && e.message).slice(0, 60) }`
		goroutines := runtime.NumGoroutine()
		h := buildHarness(t, moejs.Options{MemoryLimit: limit}, node.Options{FS: &node.FSOptions{Root: root}})
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		stop := heapPeak()
		start := time.Now()
		res, rerr, fired := runDeadline(t, h, body, 200*time.Millisecond)
		took := time.Since(start)
		h.api.Close()
		closed := time.Since(start)
		peak := stop()
		// The workers end within a second of Close.
		for time.Since(start) < took+time.Second && runtime.NumGoroutine() > goroutines {
			time.Sleep(time.Millisecond)
		}
		lingering := runtime.NumGoroutine() - goroutines
		var stacks string
		if lingering > 0 {
			buf := make([]byte, 1<<20)
			stacks = string(buf[:runtime.Stack(buf, true)])
			if i := strings.Index(stacks, "moejs/node."); i >= 0 {
				stacks = stacks[max(0, i-200):min(len(stacks), i+1500)]
			}
		}
		peak -= min(peak, before.HeapInuse)
		maxPeak = max(maxPeak, peak)
		slowest = max(slowest, closed)
		kind := res
		if i := strings.IndexAny(kind, " "); i > 0 && strings.HasPrefix(kind, "THROW") {
			kind = "THROW " + strings.SplitN(res, " ", 3)[1]
		} else if strings.HasPrefix(kind, "OK") {
			kind = "OK"
		}
		switch {
		case fired:
			kind = "interrupted"
		case rerr != nil && strings.Contains(rerr.Error(), "memory"):
			kind = "memory limit"
		case rerr != nil:
			kind = "error"
		}
		stats[kind]++
		bad := ""
		switch {
		case lingering > 0:
			bad = fmt.Sprintf("%d goroutines still run a second after Close: %s", lingering, stacks)
		case took > 200*time.Millisecond+time.Second:
			bad = fmt.Sprintf("returned after %v", took)
		case closed > 200*time.Millisecond+time.Second:
			bad = fmt.Sprintf("Close returned after %v", closed)
		case peak > 4*limit:
			bad = fmt.Sprintf("HeapInuse grew by %d MiB", peak>>20)
		case rerr != nil && !fired && !strings.Contains(rerr.Error(), "memory"):
			bad = "error " + rerr.Error()
		case !fired && rerr == nil && !strings.HasPrefix(res, "OK") && !strings.HasPrefix(res, "THROW"):
			bad = "result " + res
		}
		if bad != "" {
			failures++
			t.Errorf("seed %d iteration %d: %s; %s; patterns %.300s (%d bytes)", seed, iter, bad, call, pj, len(pj))
			if dir := os.Getenv("NODE_GLOB_FUZZ_OUT"); dir != "" {
				os.WriteFile(filepath.Join(dir, fmt.Sprintf("case-%d-%d.js", seed, iter)), []byte(body), 0o644)
			}
		}
	}
	t.Logf("seed %d: %d runs %v; slowest return %v, HeapInuse grew by at most %d MiB", seed, runs, stats, slowest.Round(time.Millisecond), maxPeak>>20)
}

// TestGlobFuzzCase runs the body a failed run of TestGlobFuzz saved in
// the file NODE_GLOB_CASE (skipped without it), as the fuzz runs it.
func TestGlobFuzzCase(t *testing.T) {
	file := os.Getenv("NODE_GLOB_CASE")
	if file == "" {
		t.Skip("NODE_GLOB_CASE is not set")
	}
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	root := t.TempDir()
	writeTree(t, root, fuzzTree)
	h := buildHarness(t, moejs.Options{MemoryLimit: 16 << 20}, node.Options{FS: &node.FSOptions{Root: root}})
	defer h.api.Close()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stop := heapPeak()
	start := time.Now()
	res, rerr, fired := runDeadline(t, h, string(body), 200*time.Millisecond)
	peak := stop()
	t.Logf("%.200s %v %v after %v; HeapInuse grew by %d MiB", res, rerr, fired, time.Since(start), (peak-min(peak, before.HeapInuse))>>20)
}

package eventloop_test

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// smallObjectBytes is what MeasureLive counts for one {a, b} object kept
// in an array.
func smallObjectBytes(t *testing.T) float64 {
	rt := moejs.NewRuntime(moejs.Options{})
	base := rt.Realm().MeasureLive(nil)
	s, err := moejs.CompileScript("cal.js", "globalThis.kept = []; for (let i = 0; i < 100000; i++) kept.push({a: i, b: i});")
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	require.NoError(t, err)
	return float64(rt.Realm().MeasureLive(nil)-base) / 100000
}

// keepSmall is a script that keeps about bytes in small objects (many
// nodes for a measurement to walk), then runs a chain of n immediates that
// each allocate garbage bytes and keep nothing; the first allocates
// first bytes of garbage, so that the loop measures right after it.
func keepSmall(t *testing.T, bytes int64, n, first, garbage int) string {
	objs := int(float64(bytes) / smallObjectBytes(t))
	return fmt.Sprintf(`
globalThis.kept = [];
for (let i = 0; i < %d; i++) kept.push({a: i, b: i});
let n = 0;
function step() {
	const g = "x".repeat(n === 0 ? %d : %d);
	if (g.length === 0) throw new Error();
	if (++n < %d) setImmediate(step);
}
setImmediate(step);
`, objs, first, garbage, n)
}

func runScript(t *testing.T, loop *eventloop.Loop, src string) error {
	return runScriptLimit(t, loop, src, 0)
}

// runScriptLimit runs src on loop, then sets the runtime's memory limit to
// limit (when not 0) before the loop starts: the setup's allocation does
// not count.
func runScriptLimit(t *testing.T, loop *eventloop.Loop, src string, limit int64) error {
	t.Helper()
	s, err := moejs.CompileScript("measure.js", src)
	require.NoError(t, err)
	return loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(s)
		if limit != 0 {
			rt.Realm().SetMemoryLimit(limit)
		}
		return err
	})
}

// TestMemoryLimitMeasuresWithoutBlockingPosters: a goroutine posting tasks
// (RunOnLoop, or a settle function) is not held up by the loop's
// measurements of the heap, which walk tens of MiB of small objects: the
// loop holds its mutex only while it adds its roots. A root provider runs
// inside each measurement the loop makes, on the loop's goroutine; there
// it posts from another goroutine and waits for the post, which completes
// before the measurement does unless the measurement holds the loop's
// mutex, when it waits until its timeout (no time is compared but that).
func TestMemoryLimitMeasuresWithoutBlockingPosters(t *testing.T) {
	const limit = 128 << 20
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	var posted, blocked atomic.Int64
	var worst atomic.Int64
	remove := rt.AddLiveRoots(func(func(moejs.Value)) {
		done := make(chan struct{})
		start := time.Now()
		go func() {
			loop.RunOnLoop(func(*moejs.Runtime) {})
			close(done)
		}()
		select {
		case <-done:
			posted.Add(1)
			if d := int64(time.Since(start)); d > worst.Load() {
				worst.Store(d)
			}
		case <-time.After(30 * time.Second):
			blocked.Add(1)
		}
	})
	// 48 MiB kept in small objects; each step allocates a quarter of what
	// the limit leaves, so that about every other step measures.
	err = runScript(t, loop, keepSmall(t, 48<<20, 40, 20<<20, 20<<20))
	remove()
	loop.Terminate()
	require.NoError(t, err)
	t.Logf("%d posts made inside measurements (measured %d MiB live), the longest in %v; %d waited for the measurement",
		posted.Load(), rt.Stats().LiveBytes>>20, time.Duration(worst.Load()), blocked.Load())
	assert.Positive(t, posted.Load(), "no measurement ran")
	assert.Zero(t, blocked.Load(), "a post waited for a measurement")
}

// TestMemoryLimitNearLimitCost: a program that keeps most of its limit in
// small objects and allocates a little garbage per callback, which the
// loop measures now and then: every half of what it keeps, and where an
// allocation would pass the limit before that, once more then. The test
// logs the cost per callback at each level, with the limit and without
// (the control: the Go GC's own cost grows with the heap too), and asserts
// that at 75 % of the limit the measurements at most double a callback's
// cost, and at 95 % at most quadruple it (a walk of 60 MiB of small
// objects every 3 MiB allocated).
func TestMemoryLimitNearLimitCost(t *testing.T) {
	const limit = 64 << 20
	const steps = 2000
	for _, pct := range []int{0, 25, 50, 75, 90, 95, 98} {
		var per [2]time.Duration
		var live int64
		for i, lim := range []int64{0, limit} {
			rt := moejs.NewRuntime(moejs.Options{})
			loop, err := eventloop.New(rt, eventloop.Options{})
			require.NoError(t, err)
			start := time.Now()
			err = runScriptLimit(t, loop, keepSmall(t, int64(pct)*limit/100, steps, limit/2+(1<<20), 64<<10), lim)
			took := time.Since(start)
			require.NoError(t, err)
			per[i] = took / steps
			live = rt.Stats().LiveBytes
		}
		t.Logf("%2d %% (%2d MiB measured): %8v per callback allocating 64 KiB with the limit, %8v without: %.1fx", pct, live>>20, per[1], per[0], float64(per[1])/float64(per[0]))
		switch pct {
		case 75:
			assert.Less(t, per[1], 2*per[0], "at 75 %% of the limit a callback costs %v, without a limit %v", per[1], per[0])
		case 95:
			assert.Less(t, per[1], 4*per[0], "at 95 %% of the limit a callback costs %v, without a limit %v", per[1], per[0])
		}
	}
}

// TestMemoryLimitKeepingPropertyNames: a program that keeps its data in
// property names, rather than in values, is bounded by the limit like one
// that keeps it in values, and what its names hold dies with what holds
// them: with the shared intrinsics, a long name that only data supplies
// stays in the realm's own shapes, not in the process-wide shape tree,
// whose shapes live as long as the process. A program that keeps the
// objects stops near the limit; one that drops them runs on and keeps
// what the realm's own transitions keep, which the limit counts too.
func TestMemoryLimitKeepingPropertyNames(t *testing.T) {
	const limit = 16 << 20
	const size = 256 << 10
	const steps = 400 // 100 MiB of names
	names := make([]string, 100)
	for i := range names {
		names[i] = fmt.Sprintf("a%d: 0", i)
	}
	// heap is the Go heap once the cleanups of the names that died (the
	// process-wide intern table's) have run.
	heap := func() int64 {
		var m runtime.MemStats
		for range 3 {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
		}
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	for _, keep := range []bool{true, false} {
		before := heap()
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		loop, err := eventloop.New(rt, eventloop.Options{})
		require.NoError(t, err)
		ticks := 0
		require.NoError(t, rt.SetGlobal("tick", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
			ticks++
			return moejs.Undefined(), nil
		})))
		src := fmt.Sprintf(`
const codeNames = {%s}; // names bounded by code, as parents of the data names
const base = "k".repeat(%d - 8);
const kept = [];
let n = 0;
function step() {
	const o = {};
	o["a" + (n %% 100)] = 0;
	o[base + n] = 1; // a name only data supplies, of %d KiB
	if (%v) kept.push(o);
	tick();
	if (++n < %d) setTimeout(step, 0);
}
setTimeout(step, 0);
`, strings.Join(names, ", "), size, size>>10, keep, steps)
		err = runScript(t, loop, src)
		st := rt.Stats()
		during := heap() - before
		loop.Terminate()
		rt, loop = nil, nil
		after := heap() - before
		t.Logf("keep=%v: %d steps (%d MiB of names) under a %d MiB limit: err %v; measured %d KiB live; Go heap %d MiB more, %d MiB once the runtime was dropped",
			keep, ticks, ticks*size>>20, limit>>20, err, st.LiveBytes>>10, during>>20, after>>20)
		if keep {
			assert.ErrorIs(t, err, moejs.ErrMemoryLimit, "the names kept passed the limit")
			assert.Less(t, ticks, 2*limit/size, "stopped near the limit")
		} else {
			require.NoError(t, err)
			assert.Less(t, during, int64(limit), "what the realm keeps of the names")
		}
		assert.Less(t, after, int64(limit), "what the program pinned outlived its runtime")
	}
}

// TestMemoryLimitRunPerRequest: a host that keeps one runtime and runs the
// loop once per request (Run returns when the loop is idle) bounds what
// the program keeps across requests too: each Run starts a new budget,
// which keeps the last measurement, and what was allocated in the earlier
// Runs paces the next one.
func TestMemoryLimitRunPerRequest(t *testing.T) {
	const limit = 16 << 20
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	setup, err := moejs.CompileScript("setup.js", `globalThis.kept = []; globalThis.handle = () => setTimeout(() => kept.push("x".repeat(1048576 - 8) + kept.length), 0);`)
	require.NoError(t, err)
	_, err = rt.RunScript(setup)
	require.NoError(t, err)
	call, err := moejs.CompileScript("call.js", `handle();`)
	require.NoError(t, err)
	requests := 0
	for ; requests < 100; requests++ {
		err = loop.Run(func(rt *moejs.Runtime) error {
			_, err := rt.RunScript(call)
			return err
		})
		if err != nil {
			break
		}
	}
	t.Logf("%d requests of 1 MiB kept each under a %d MiB limit: %v (live measured %d KiB)", requests, limit>>20, err, rt.Stats().LiveBytes>>10)
	assert.ErrorIs(t, err, moejs.ErrMemoryLimit)
	assert.Less(t, requests, 40, "the limit stopped the program near %d MiB", limit>>20)
}

// TestMemoryLimitKeepingSlices: a program that keeps a short slice of
// each long string it makes keeps the long strings' bytes (the slice
// shares them), which the limit bounds: the measurement counts the bytes a
// slice keeps alive.
func TestMemoryLimitKeepingSlices(t *testing.T) {
	const limit = 16 << 20
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	ticks := 0
	require.NoError(t, rt.SetGlobal("tick", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		ticks++
		return moejs.Undefined(), nil
	})))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err = runScript(t, loop, `
const kept = [];
let n = 0;
function step() {
	kept.push(("y".repeat(1048576 - 8) + (1000000 + n)).slice(0).slice(0, 16));
	tick();
	if (++n < 200) setTimeout(step, 0);
}
setTimeout(step, 0);
`)
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("%d steps keeping a 16-byte slice of a 1 MiB string under a %d MiB limit: err %v; measured %d KiB live; Go heap %d MiB more",
		ticks, limit>>20, err, rt.Stats().LiveBytes>>10, (int64(after.HeapAlloc)-int64(before.HeapAlloc))>>20)
	assert.ErrorIs(t, err, moejs.ErrMemoryLimit, "200 MiB kept under a 16 MiB limit")
	assert.Less(t, ticks, 40, "stopped near the limit")
	runtime.KeepAlive(rt)
}

// TestTerminatedLoopsAreFreed: a runtime that gets a new Loop after
// Terminate, as Terminate's documentation allows, does not keep the old
// ones, which registered their roots with it, whether Terminate came
// between runs or ended a run; the loop it has then measures with its own
// roots.
func TestTerminatedLoopsAreFreed(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 64 << 20})
	var freed atomic.Int64
	const loops = 200
	for i := range loops {
		loop, err := eventloop.New(rt, eventloop.Options{})
		require.NoError(t, err)
		// A cleanup, not a finalizer: a loop that made a Timeout is in a cycle
		// (its prototype's methods hold it).
		runtime.AddCleanup(loop, func(f *atomic.Int64) { f.Add(1) }, &freed)
		if i%2 == 0 {
			require.NoError(t, runScript(t, loop, `setTimeout(() => {}, 0);`))
		} else {
			require.NoError(t, loop.Start())
			ran := make(chan struct{})
			require.True(t, loop.RunOnLoop(func(*moejs.Runtime) { close(ran) }))
			<-ran
		}
		loop.Terminate()
		rt.ClearInterrupt()
	}
	for range 5 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("%d of %d loops freed", freed.Load(), loops)
	assert.Greater(t, freed.Load(), int64(loops*9/10), "the runtime keeps the loops it had")

	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	require.NoError(t, runScript(t, loop, `{ const big = "b".repeat(8 << 20); setTimeout(() => big.length, 1e9).unref(); } setTimeout(() => {}, 0);`))
	assert.Greater(t, rt.Realm().MeasureLive(nil), int64(8<<20), "the string only the new loop's timer keeps")
	loop.Terminate()
	rt.ClearInterrupt()
	assert.Less(t, rt.Realm().MeasureLive(nil), int64(8<<20), "dropped with the terminated loop's timers")
	runtime.KeepAlive(rt)
}

// TestLoopRunsAfterRootsPanic: a root provider that panics fails Run, as
// a panicking task does, without leaving the loop's mutex held: once it
// is removed, the loop runs again.
func TestLoopRunsAfterRootsPanic(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 16 << 20})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	remove := rt.AddLiveRoots(func(func(moejs.Value)) { panic("roots") })
	src := `(() => { let n = 0; function step() { const g = "x".repeat(1 << 20) + n; if (++n < 40 && g) setTimeout(step, 0); } setTimeout(step, 0); })();`
	assert.PanicsWithValue(t, "roots", func() { _ = runScript(t, loop, src) })
	remove()
	rt.ClearInterrupt()
	done := make(chan struct{})
	go func() {
		defer close(done)
		err = runScript(t, loop, src)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("Run after a root provider panicked: not done after a minute")
	}
	assert.NoError(t, err)
	assert.True(t, loop.RunOnLoop(func(*moejs.Runtime) {}))
}

// churnSource defines churn(rounds), whose rounds allocate objects,
// arrays, strings, JSON and a Map, in JavaScript and through builtins, and
// keep none of it.
const churnSource = `
function churn(rounds) {
	let n = 0;
	for (let r = 0; r < rounds; r++) {
		const o = {r, s: "v" + r, a: [r, r + 1, r + 2]};
		const t = JSON.stringify({o, list: Array.from({length: 64}, (_, i) => i * r)});
		n += JSON.parse(t).list.length + t.length + ("x".repeat(256) + r).length + new Map([[r, o]]).size;
	}
	return n;
}
`

// TestOneTaskMakesGarbage: a single timer callback, or a single task
// posted with RunOnLoop, that allocates ten times the memory limit and
// keeps little runs to its end: the first charge past the limit measures,
// though the loop's rebase before the task was not due.
func TestOneTaskMakesGarbage(t *testing.T) {
	const limit = 16 << 20
	cal := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 40})
	s, err := moejs.CompileScript("cal.js", churnSource+"churn(100);")
	require.NoError(t, err)
	_, err = cal.RunScript(s)
	require.NoError(t, err)
	rounds := 10 * limit * 100 / cal.Stats().AllocatedBytes
	for _, task := range []string{"timer", "RunOnLoop"} {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
		loop, err := eventloop.New(rt, eventloop.Options{})
		require.NoError(t, err)
		src := churnSource + "churn;"
		if task == "timer" {
			src = churnSource + fmt.Sprintf("setTimeout(() => churn(%d), 0);", rounds)
		}
		s, err := moejs.CompileScript("task.js", src)
		require.NoError(t, err)
		err = loop.Run(func(rt *moejs.Runtime) error {
			fn, err := rt.RunScript(s)
			if err != nil || task != "RunOnLoop" {
				return err
			}
			loop.RunOnLoop(func(rt *moejs.Runtime) {
				if _, err := rt.CallFunction(fn, moejs.Undefined(), moejs.Int(rounds)); err != nil {
					t.Error(err)
				}
			})
			return nil
		})
		st := rt.Stats()
		t.Logf("%s: %v; allocated %d MiB, last measurement %d KiB", task, err, st.AllocatedBytes>>20, st.LiveBytes>>10)
		require.NoError(t, err, task)
		assert.GreaterOrEqual(t, st.AllocatedBytes, int64(9*limit), task)
		assert.Zero(t, st.MemoryLimitHits, task)
	}
}

package eventloop_test

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const memLimit = 16 << 20

// memEnv is a loop over a runtime with a 16 MiB memory limit and the host
// functions of a tool-calling agent: tool() returns a promise of
// NewPromise that another goroutine settles, and tick()
// counts the program's iterations.
type memEnv struct {
	rt    *moejs.Runtime
	loop  *eventloop.Loop
	ticks int
}

func newMemEnv(t *testing.T, opts eventloop.Options) *memEnv {
	t.Helper()
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: memLimit})
	loop, err := eventloop.New(rt, opts)
	require.NoError(t, err)
	e := &memEnv{rt: rt, loop: loop}
	require.NoError(t, rt.SetGlobal("tool", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		p, settle := loop.NewPromise()
		go settle(nil)
		return p, nil
	})))
	require.NoError(t, rt.SetGlobal("tick", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		e.ticks++
		return moejs.Undefined(), nil
	})))
	return e
}

// agentSource is a module whose async function awaits n tool results and
// makes a string of size bytes after each, which it keeps when keep is
// true.
func agentSource(n, size int, keep bool) string {
	return fmt.Sprintf(`
const base = "x".repeat(%d - 8);
async function agent(n, keepIt) {
	const keep = [];
	for (let i = 0; i < n; i++) {
		await tool();
		const s = base + i;
		if (keepIt) keep.push(s);
		else if (s.length === 0) throw new Error("empty");
		tick();
	}
	return keep.length;
}
agent(%d, %t);
`, size, n, keep)
}

// runModule runs src as a module in e's loop.
func (e *memEnv) runModule(t *testing.T, src string) error {
	t.Helper()
	mod, err := moejs.Compile("agent.js", src)
	require.NoError(t, err)
	return e.loop.Run(func(rt *moejs.Runtime) error { return rt.Load(mod) })
}

// requireStoppedNearLimit checks that err is a memory limit hit and that
// the program kept about the limit, ticks strings of size bytes, when it
// stopped.
func requireStoppedNearLimit(t *testing.T, e *memEnv, err error, size int) {
	t.Helper()
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
	var interrupted *moejs.InterruptedError
	require.ErrorAs(t, err, &interrupted)
	var mle *moejs.MemoryLimitError
	require.ErrorAs(t, err, &mle)
	assert.Equal(t, int64(memLimit), mle.Limit)
	kept := int64(e.ticks) * int64(size)
	assert.GreaterOrEqual(t, kept, int64(memLimit/2), "kept when it stopped")
	assert.LessOrEqual(t, kept, int64(2*memLimit), "kept when it stopped")
	st := e.rt.Stats()
	assert.GreaterOrEqual(t, st.MemoryLimitHits, int64(1))
	assert.Positive(t, st.LiveBytes, "measured")
	t.Logf("stopped after %d iterations: %d KiB kept, %d KiB measured, %d KiB allocated since", e.ticks, kept>>10, st.LiveBytes>>10, st.RequestAllocatedBytes>>10)
}

// TestMemoryLimitKeepingAgent reproduces a program that keeps what it
// allocates while it awaits host results, a little at a time: the limit
// stops it near the limit, where a budget per task let it grow without
// bound. One that allocates as much and keeps nothing runs on.
func TestMemoryLimitKeepingAgent(t *testing.T) {
	const size = 64 << 10
	t.Run("keeps", func(t *testing.T) {
		e := newMemEnv(t, eventloop.Options{})
		err := e.runModule(t, agentSource(1000, size, true))
		requireStoppedNearLimit(t, e, err, size)
		assert.Less(t, e.ticks, 1000)
	})
	t.Run("keeps, budget per task", func(t *testing.T) {
		e := newMemEnv(t, eventloop.Options{ResetMemoryPerTask: true})
		require.NoError(t, e.runModule(t, agentSource(400, size, true)))
		assert.Equal(t, 400, e.ticks, "kept %d MiB under a %d MiB limit", 400*size>>20, memLimit>>20)
	})
	t.Run("drops", func(t *testing.T) {
		e := newMemEnv(t, eventloop.Options{})
		require.NoError(t, e.runModule(t, agentSource(2600, size, false)))
		assert.Equal(t, 2600, e.ticks)
		st := e.rt.Stats()
		assert.GreaterOrEqual(t, st.AllocatedBytes, int64(10*memLimit), "allocated in all")
		assert.Zero(t, st.MemoryLimitHits)
	})
}

// timerSource is a script whose chain of n timeouts makes a string of
// size bytes in each, which a closure keeps when keep is true.
func timerSource(n, size int, keep bool) string {
	return fmt.Sprintf(`(function () {
	const base = "x".repeat(%d - 8);
	const keep = [];
	let n = 0;
	function step() {
		const s = base + n;
		if (%t) keep.push(s);
		else if (s.length === 0) throw new Error("empty");
		tick();
		if (++n < %d) setTimeout(step, 0);
	}
	setTimeout(step, 0);
})();`, size, keep, n)
}

// TestMemoryLimitKeepingTimers: a chain of timeouts that keeps its data in
// a closure, which only the pending timer holds, stops near the limit; one
// that drops it runs on. So does a chain of immediates that passes its
// data on in their arguments.
func TestMemoryLimitKeepingTimers(t *testing.T) {
	t.Run("keeps", func(t *testing.T) {
		const size = 256 << 10
		e := newMemEnv(t, eventloop.Options{})
		err := e.loop.Run(script(t, timerSource(400, size, true)))
		requireStoppedNearLimit(t, e, err, size)
	})
	t.Run("drops", func(t *testing.T) {
		const size = 1 << 20
		e := newMemEnv(t, eventloop.Options{})
		require.NoError(t, e.loop.Run(script(t, timerSource(200, size, false))))
		assert.Equal(t, 200, e.ticks)
		assert.GreaterOrEqual(t, e.rt.Stats().AllocatedBytes, int64(10*memLimit))
	})
	t.Run("immediate arguments", func(t *testing.T) {
		const size = 64 << 10
		e := newMemEnv(t, eventloop.Options{})
		err := e.loop.Run(script(t, fmt.Sprintf(`
const base = "x".repeat(%d - 8);
function step(keep) {
	keep.push(base + keep.length);
	tick();
	if (keep.length < 1000) setImmediate(step, keep);
}
setImmediate(step, []);
`, size)))
		requireStoppedNearLimit(t, e, err, size)
	})
}

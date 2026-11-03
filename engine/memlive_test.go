package engine

import (
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireLive checks that a measurement is within ±50% of want.
func requireLive(t *testing.T, got, want int64, msg string) {
	t.Helper()
	assert.GreaterOrEqual(t, got, want/2, msg)
	assert.LessOrEqual(t, got, want+want/2, msg)
}

// TestMeasureLiveStructures: what a global keeps through each kind of
// structure counts about its size, once, and nothing once it is dropped.
func TestMeasureLiveStructures(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		name, keep string
		want       int64
	}{
		{"strings", `Array.from({length: 16}, (_, i) => big64k + i)`, mib},
		{"utf16 strings", `Array.from({length: 16}, (_, i) => "é".repeat(32768 - 4) + i)`, mib},
		{"one string many times", `Array.from({length: 1000}, () => big)`, mib},
		{"substrings", `[big, big.slice(0, 1000), big.slice(0, 2000)]`, mib},
		{"rope", `"y" + big`, mib},
		{"flattened rope", `(() => { const s = "y" + big; s.charCodeAt(5); return s; })()`, mib},
		{"append buffer", `(() => { let s = big; for (let i = 0; i < 100; i++) s += "z"; return [s, s.slice(0, 10)]; })()`, 2 * mib},
		{"map", `new Map(Array.from({length: 16}, (_, i) => [i, big64k + i]))`, mib},
		{"set", `new Set(Array.from({length: 16}, (_, i) => big64k + i))`, mib},
		{"map iterator", `(() => { const m = new Map([[1, big + 1]]); return m.values(); })()`, mib},
		{"dictionary object", `(() => { const o = {}; for (let i = 0; i < 16; i++) o["k" + i] = big64k + i; delete o.k0; return o; })()`, mib * 15 / 16},
		{"sparse elements", `(() => { const a = []; a[1e6] = big + 1; return a; })()`, mib},
		{"closure", `(() => { const s = big + 1; return () => s; })()`, mib},
		{"bound function", `(function () {}).bind(null, big + 1)`, mib},
		// The frames keep their registers, so their strings come from mk:
		// big + 1 would keep big in a temporary too.
		{"generator", `(() => { function* g() { const s = mk(); yield 1; return s; } const it = g(); it.next(); return it; })()`, mib},
		{"awaiting async function", `(() => { const p = new Promise(() => {}); (async () => { const s = mk(); await p; return s; })(); return p; })()`, mib},
		{"promise result", `Promise.resolve(big + 1)`, mib},
		{"then callback", `(() => { const p = new Promise(() => {}); p.then(((s) => () => s)(big + 1)); return p; })()`, mib},
		{"proxy", `new Proxy({s: big + 1}, {})`, mib},
		{"error cause", `new Error("m", {cause: big + 1})`, mib},
		{"symbol", `Symbol(big + 1)`, mib},
		{"bigint", `Array.from({length: 8}, (_, i) => (1n << 1000000n) + BigInt(i))`, 1000000},
		{"accessor", `({get s() { return 1; }, set s(v) {}, t: big + 1})`, mib},
		{"array buffer and views", `(() => { const b = new ArrayBuffer(1 << 20); return [b, new Uint8Array(b), new DataView(b), new Uint8Array(b, 16)]; })()`, mib},
		{"weak map entry", `(() => { const k = {}, wm = new WeakMap(); wm.set(k, big + 1); return [k, wm]; })()`, mib},
		{"weak map entry, map first", `(() => { const k = {}, wm = new WeakMap(); wm.set(k, big + 1); return [wm, k]; })()`, mib},
		{"class private field", `new (class { #s = big + 1; })()`, mib},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			empty := r.MeasureLive(nil)
			_, err := runScriptValue(t, r, `let big = "x".repeat(1 << 20), big64k = "x".repeat(65536 - 4); function mk() { return "x".repeat(1 << 20) + 1; }`)
			require.NoError(t, err)
			_, err = runScriptValue(t, r, `var keep = `+c.keep+`; big = big64k = undefined;`)
			require.NoError(t, err)
			requireLive(t, r.MeasureLive(nil)-empty, c.want, "kept")
			_, err = runScriptValue(t, r, `keep = undefined`)
			require.NoError(t, err)
			assert.Less(t, r.MeasureLive(nil)-empty, int64(64<<10), "dropped")
		})
	}
}

// TestMeasureLiveWeak: a WeakMap's value counts only while both its key
// and the map are reached.
func TestMeasureLiveWeak(t *testing.T) {
	for _, drop := range []string{"wm", "k"} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
		empty := r.MeasureLive(nil)
		_, err := runScriptValue(t, r, `var k = {}, wm = new WeakMap(); wm.set(k, "x".repeat(1 << 20) + 1);`)
		require.NoError(t, err)
		requireLive(t, r.MeasureLive(nil)-empty, 1<<20, "the key and the map")
		_, err = runScriptValue(t, r, drop+` = undefined`)
		require.NoError(t, err)
		assert.Less(t, r.MeasureLive(nil)-empty, int64(64<<10), "without "+drop)
	}
}

// TestMeasureLiveRoots: a value only the roots hold counts, once, and an
// empty realm measures little.
func TestMeasureLiveRoots(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	empty := r.MeasureLive(nil)
	assert.Less(t, empty, int64(16<<10), "an empty realm")
	v, err := runScriptValue(t, r, `"x".repeat(1 << 20) + 1`)
	require.NoError(t, err)
	assert.Less(t, r.MeasureLive(nil)-empty, int64(1<<10), "a result JavaScript does not keep")
	got := r.MeasureLive(func(add func(Value)) {
		add(v)
		add(v)
		add(Undefined())
	})
	requireLive(t, got-empty, 1<<20, "a root")
}

// TestMeasureLiveMutableRealm: the intrinsics of a mutable realm count
// for nothing, what a program hangs on them counts.
func TestMeasureLiveMutableRealm(t *testing.T) {
	r := NewRealm()
	empty := r.MeasureLive(nil)
	assert.Less(t, empty, int64(64<<10), "an empty mutable realm")
	_, err := runScriptValue(t, r, `function* g() {} async function f() {} new Map(); new WeakRef({}); new ArrayBuffer(8); Promise.resolve(); [].values();`)
	require.NoError(t, err)
	used := r.MeasureLive(nil)
	assert.Less(t, used, int64(128<<10), "with the deferred intrinsics defined")
	_, err = runScriptValue(t, r, `Array.prototype.big = "x".repeat(1 << 20) + 1`)
	require.NoError(t, err)
	requireLive(t, r.MeasureLive(nil)-used, 1<<20, "data on an intrinsic")
}

// TestMeasureLiveHostNodes: the host nodes of FromGo count without their
// Go values, before and after JavaScript reads them.
func TestMeasureLiveHostNodes(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	items := make([]any, 1000)
	for i := range items {
		items[i] = map[string]any{"name": "item", "tags": []string{"a", "b"}, "n": i}
	}
	v, err := r.FromGo(map[string]any{"items": items, "list": []any{1, "two", map[string]any{"x": 3}}})
	require.NoError(t, err)
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("arg"), v))
	before := r.MeasureLive(nil)
	assert.Less(t, before, int64(64<<10), "an unread argument")
	_, err = runScriptValue(t, r, `var n = 0; for (const it of arg.items) n += it.tags.length + it.n; arg.list[2].x + n`)
	require.NoError(t, err)
	after := r.MeasureLive(nil)
	assert.Greater(t, after, before, "the read nodes count")
	assert.Less(t, after, int64(4<<20))
}

// TestShareRoots: a substring and a split piece keep the string of 64 units
// or more whose units they share, also a substring of one; a substring of a
// shorter string, or of a string FromGo carved from its slab, keeps only
// the units.
func TestShareRoots(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	long := FromGoString(strings.Repeat("x", 100) + "," + strings.Repeat("y", 100))
	sub := long.Substring(10, 20)
	assert.Same(t, long, sub.right)
	assert.Same(t, long, sub.Substring(1, 5).right, "the root of a substring of a substring")
	wide := FromGoString(strings.Repeat("é", 100))
	assert.Same(t, wide, wide.Substring(10, 20).right, "UTF-16 units")
	assert.Nil(t, FromGoString(strings.Repeat("s", 63)).Substring(1, 3).right, "a short string")
	parts, err := splitByString(r, long, FromGoString(","), 1<<32-1)
	require.NoError(t, err)
	piece, _ := parts.Get(r, IndexKey(0), ObjectValue(parts))
	assert.Same(t, long, piece.AsString().right)

	v, err := r.FromGo(map[string]any{"s": strings.Repeat("h", 100)})
	require.NoError(t, err)
	hs, err := v.AsObject().Get(r, r.KeyFromGoString("s"), v)
	require.NoError(t, err)
	require.NotZero(t, hs.AsString().flags&strHost)
	assert.Nil(t, hs.AsString().Substring(10, 20).right, "a FromGo slab's string")
}

// TestRebaseAllocation: the pacing of the measurements, the measurement
// an allocation past the limit makes, and a measurement past the limit.
func TestRebaseAllocation(t *testing.T) {
	r := NewRealm()
	require.NoError(t, r.RebaseAllocation(func(func(Value)) { t.Fatal("measured without a limit") }))

	const limit = 8 << 20
	r = NewRealmWith(RealmOptions{SharedIntrinsics: true, MemoryLimit: limit})
	_, err := runScriptValue(t, r, `var keep = [], base = "x".repeat(65536 - 8); function grow(n) { for (let i = 0; i < n; i++) keep.push(base + i); }`)
	require.NoError(t, err)
	r.ResetAllocation()
	measured := 0
	for i := 0; i < 1000; i++ {
		before := r.Stats()
		due := r.lazy.mem.total-r.lazy.mem.mark >= max((limit-before.LiveBytes)/2, before.LiveBytes/2)
		called := false
		err := r.RebaseAllocation(func(func(Value)) { called = true })
		require.NoError(t, err)
		after := r.Stats()
		require.Equal(t, due, called, "measured when due, at %d", i)
		if called {
			measured++
			assert.Zero(t, after.RequestAllocatedBytes)
			requireLive(t, after.LiveBytes, int64(i)*65536, "the strings kept")
		} else {
			assert.Equal(t, before, after, "nothing changes until due")
		}
		_, err = runScriptValue(t, r, `grow(1)`)
		if err != nil {
			var mle *MemoryLimitError
			require.ErrorAs(t, err, &mle)
			assert.Equal(t, int64(limit), mle.Limit)
			// The allocation measured once more before it hit.
			assert.True(t, r.lazy.mem.remeasured)
			assert.GreaterOrEqual(t, mle.Live, after.LiveBytes)
			assert.Greater(t, mle.Live+mle.Allocated, int64(limit))
			kept := int64(i) * 65536
			assert.Greater(t, kept, int64(limit/2), "stopped near the limit")
			assert.Less(t, kept, int64(2*limit))
			break
		}
		require.Less(t, i, 999, "the limit stops the program")
	}
	assert.GreaterOrEqual(t, measured, 2)
	assert.Less(t, measured, 30, "paced")

	// A measurement past the limit: the error, a hit, no interrupt; a reset
	// keeps it, so that the next allocation measures again and hits.
	r.ClearInterrupt()
	r.SetMemoryLimit(1 << 20)
	err = r.RebaseAllocation(nil)
	var mle *MemoryLimitError
	require.ErrorAs(t, err, &mle)
	require.ErrorIs(t, err, ErrMemoryLimit)
	assert.Equal(t, int64(1<<20), mle.Limit)
	assert.Zero(t, mle.Allocated)
	assert.Greater(t, mle.Live, int64(1<<20))
	st := r.Stats()
	assert.Equal(t, mle.Live, st.LiveBytes)
	assert.Same(t, mle, st.LastMemoryLimitError)
	hits := st.MemoryLimitHits
	require.NoError(t, r.CheckInterrupt(), "a measurement does not interrupt")
	_, err = runScriptValue(t, r, `[1]`)
	require.True(t, errors.Is(err, ErrMemoryLimit), "the next allocation passes the limit")
	r.ClearInterrupt()
	r.ResetAllocation()
	assert.Equal(t, mle.Live, r.Stats().LiveBytes)
	_, err = runScriptValue(t, r, `[1]`)
	require.True(t, errors.Is(err, ErrMemoryLimit), "measured again, still past the limit")
	assert.Equal(t, hits+2, r.Stats().MemoryLimitHits)
	r.ClearInterrupt()
	r.SetMemoryLimit(4 * limit)
	_, err = runScriptValue(t, r, `[1]`)
	require.NoError(t, err)
}

// TestMeasureLiveResolvingFunctions: a promise's resolve and reject
// functions share one allocation, which either one keeps, so holding one
// counts both, as much as holding the pair.
func TestMeasureLiveResolvingFunctions(t *testing.T) {
	const n = 1000
	measure := func(keep string) int64 {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
		empty := r.MeasureLive(nil)
		_, err := runScriptValue(t, r, `var keep = Array.from({length: 1000}, () => { let fns; new Promise((res, rej) => { fns = [res, rej]; }); return `+keep+`; });`)
		require.NoError(t, err)
		return r.MeasureLive(nil) - empty
	}
	pair := measure("fns")
	for _, keep := range []string{"fns[0]", "fns[1]"} {
		one := measure(keep)
		assert.GreaterOrEqual(t, one, int64(n*unsafe.Sizeof(resolvingFns{})), keep)
		// The pair adds a two-element array per promise.
		assert.Less(t, pair-one, int64(n*(objectSize+4*liveValue)), keep)
	}
}

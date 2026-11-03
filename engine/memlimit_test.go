package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runScriptValue runs src in r and returns its completion value or error.
func runScriptValue(t *testing.T, r *Realm, src string) (Value, error) {
	t.Helper()
	s, err := syntax.ParseScript("mem.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileScript(s)
	require.NoError(t, err)
	return r.RunScript(code)
}

// TestMemoryLimitRopeFlatten: a rope costs its node when it is made and
// its units when it is flattened, so a chain of prepends charges little
// until the string is read, and reading it charges the copy.
func TestMemoryLimitRopeFlatten(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 40})
	_, err := runScriptValue(t, r, `var big = "x".repeat(1 << 20)`)
	require.NoError(t, err)
	before := r.Stats().RequestAllocatedBytes
	v, err := runScriptValue(t, r, `var s = big; for (let i = 0; i < 100; i++) s = "y" + s; s.length`)
	require.NoError(t, err)
	assert.Equal(t, float64(1<<20+100), v.AsNumber())
	ropes := r.Stats().RequestAllocatedBytes - before
	assert.Less(t, ropes, int64(1<<14), "a hundred rope nodes")
	_, err = runScriptValue(t, r, `s.charCodeAt(5)`)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, r.Stats().RequestAllocatedBytes-before-ropes, int64(1<<20))

	// A rope of a realm with a limit is a ropeNode; its flattening charges
	// the units it copies and nothing else.
	for _, src := range []string{`"y" + "x".repeat(5000)`, `"y" + "é".repeat(5000)`} {
		v, err = runScriptValue(t, r, src)
		require.NoError(t, err)
		rope := v.AsString()
		require.Equal(t, strRope, rope.kind, src)
		require.NotZero(t, rope.flags&strCharged, src)
		assert.Same(t, r.mem(), (*ropeNode)(unsafe.Pointer(rope)).m)
		units := 1
		if !rope.ropeASCII() {
			units = 2
		}
		before := r.Stats().RequestAllocatedBytes
		rope.flatten()
		assert.Equal(t, int64(units*5001), r.Stats().RequestAllocatedBytes-before, src)
		assert.Equal(t, "y", rope.Substring(0, 1).GoString())
	}
	assert.Equal(t, uintptr(48), unsafe.Sizeof(String{}))
	assert.Equal(t, uintptr(56), unsafe.Sizeof(ropeNode{}), "a ropeNode stays in the 64-byte class")
}

// TestMemoryLimitRopeBeforeLimit: a rope made before the realm had a limit
// is a plain String, whose flattening is not charged (an accepted
// undercount), and a realm without a limit makes plain ropes.
func TestMemoryLimitRopeBeforeLimit(t *testing.T) {
	r := NewRealm()
	v, err := runScriptValue(t, r, `"y" + "x".repeat(5000)`)
	require.NoError(t, err)
	rope := v.AsString()
	require.Equal(t, strRope, rope.kind)
	assert.Zero(t, rope.flags&strCharged)
	r.SetMemoryLimit(1 << 40)
	rope.flatten()
	assert.Zero(t, r.Stats().RequestAllocatedBytes)
	assert.Equal(t, "y", rope.Substring(0, 1).GoString())
}

// TestMemoryLimitRopeOtherGoroutine: a rope the realm made may flatten on
// another goroutine while the realm runs (a value the host kept); the
// account takes the bytes atomically and the next charge counts them.
func TestMemoryLimitRopeOtherGoroutine(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 40})
	v, err := runScriptValue(t, r, `var s = "x".repeat(1 << 17); for (let i = 0; i < 10; i++) s = "y" + s; s`)
	require.NoError(t, err)
	rope := v.AsString()
	require.Equal(t, strRope, rope.kind)
	before := r.Stats().AllocatedBytes
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rope.flatten()
	}()
	_, err = runScriptValue(t, r, `const a = []; for (let i = 0; i < 10000; i++) a.push({i}); a.length`)
	require.NoError(t, err)
	wg.Wait()
	assert.GreaterOrEqual(t, r.Stats().AllocatedBytes-before, int64(1<<17))
}

// TestMemoryLimitPreChecks: the producers that know their size first throw
// their RangeError for a size past the budget, which the script catches,
// and fill reserves its storage before it allocates it.
func TestMemoryLimitPreChecks(t *testing.T) {
	cases := []struct{ src, want string }{
		{`"x".repeat(1 << 22)`, "RangeError: Invalid string length"},
		{`"x".padStart(1 << 22)`, "RangeError: Invalid string length"},
		{`new ArrayBuffer(1 << 22).byteLength`, "RangeError: Array buffer allocation failed"},
		{`new Uint8Array(1 << 22).length`, "RangeError: Invalid typed array length: 4194304"},
		{`new Float64Array(1 << 20).length`, "RangeError: Invalid typed array length: 1048576"},
		{`btoa("x".repeat(3 << 19))`, "RangeError: Invalid string length"},
	}
	for _, c := range cases {
		r := NewRealmWith(RealmOptions{MemoryLimit: 2 << 20})
		v, err := runScriptValue(t, r, `try { `+c.src+` } catch (e) { e.name + ": " + e.message }`)
		require.NoError(t, err, c.src)
		assert.Equal(t, c.want, v.String(), c.src)
		assert.Zero(t, r.Stats().MemoryLimitHits, c.src)
	}

	r := NewRealmWith(RealmOptions{MemoryLimit: 2 << 20})
	_, err := runScriptValue(t, r, `try { new Array(1e7).fill(0) } finally { globalThis.ran = true }`)
	require.ErrorIs(t, err, ErrMemoryLimit)
	assert.Less(t, r.Stats().RequestAllocatedBytes, int64(200<<20), "fill stopped before it allocated")
	_, ok := r.Global.GetOwnDataValue(key(r, "ran"))
	assert.False(t, ok)
}

// TestMemoryLimitCoverage: each kind of storage a script can grow without
// bound is charged, so a loop growing it passes the limit.
func TestMemoryLimitCoverage(t *testing.T) {
	cases := []string{
		`const a = []; for (;;) a.push(a.length)`,
		`const a = []; for (let i = 0; ; i++) a[i] = i`,
		`const o = {}; for (let i = 0; ; i++) o["k" + i] = i`,
		`const a = []; for (let i = 0; ; i += 2) a[i * 1000] = i`,
		`const m = new Map(); for (let i = 0; ; i++) m.set(i, i)`,
		`const s = new Set(); for (let i = 0; ; i++) s.add(i)`,
		`let s = ""; for (;;) s += "abcdefgh"`,
		`const a = []; for (;;) a.push("x".repeat(1000) + a.length)`,
		`const a = []; for (;;) a.push(() => a.length)`,
		`const a = []; for (;;) a.push(new Uint8Array(1000))`,
		`const a = []; for (;;) a.push([1, 2, 3].join(",".repeat(100)))`,
		`const a = []; for (;;) a.push(JSON.stringify({a: [1, 2, 3], b: "xyz"}))`,
		`const a = []; for (;;) a.push(JSON.parse('{"a": [1, 2, 3], "b": "xyz"}'))`,
		`const a = []; for (;;) a.push(Object.keys({a: 1, b: 2}))`,
		`let b = 1n; const a = []; for (;;) a.push(b++ * 12345678901234567890n)`,
		`const a = []; function* g() { for (;;) yield a.length } for (const x of g()) a.push(x)`,
		`const a = []; for (;;) a.push(new Error("e"))`,
		`const a = []; for (;;) a.push(new Promise(() => {}))`,
		`const a = []; for (;;) a.push(Symbol.iterator.toString().slice(1))`,
		`const a = []; for (;;) a.push("é".repeat(20).toUpperCase())`,
		`class P { constructor(i) { this.i = i } } const a = []; for (;;) a.push(new P(a.length))`,
	}
	for _, src := range cases {
		r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
		_, err := runScriptValue(t, r, src)
		if ex, ok := err.(*Exception); ok {
			// A sized producer (repeat, a typed array) refused what was
			// left of the budget with its RangeError.
			assert.Equal(t, "RangeError", errorDisplayString(ex.Value)[:10], src)
			assert.Greater(t, r.Stats().LiveBytes+r.Stats().RequestAllocatedBytes, int64(1<<20-1<<14), src)
			continue
		}
		assert.ErrorIs(t, err, ErrMemoryLimit, src)
		var mle *MemoryLimitError
		if assert.True(t, errors.As(err, &mle), src) {
			assert.Contains(t, mle.Stack, "mem.js:1:", src)
		}
	}
}

// TestMemoryLimitNoAccount: a realm without a limit keeps no account.
func TestMemoryLimitNoAccount(t *testing.T) {
	r := NewRealm()
	_, err := runScriptValue(t, r, `const a = []; for (let i = 0; i < 1000; i++) a.push({i}); a.length`)
	require.NoError(t, err)
	assert.Nil(t, r.mem())
	assert.Equal(t, Stats{ICEntries: len(r.ic), RegisterStackBytes: r.Stats().RegisterStackBytes}, r.Stats())
}

// TestMemoryLimitRopeConcurrentFlatten: goroutines flattening one charged
// rope at once each charge the copy they made to the rope's account (the
// losers' too, an accepted overcount) and never write into the string.
func TestMemoryLimitRopeConcurrentFlatten(t *testing.T) {
	const readers = 8
	for range 20 {
		r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 40})
		v, err := runScriptValue(t, r, `"y" + "x".repeat(4000)`)
		require.NoError(t, err)
		rope := v.AsString()
		require.NotZero(t, rope.flags&strCharged)
		before := r.Stats().AllocatedBytes
		var wg sync.WaitGroup
		got := make([]string, readers)
		for i := range readers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i] = rope.GoString()
			}()
		}
		wg.Wait()
		want := "y" + strings.Repeat("x", 4000)
		for _, g := range got {
			require.Equal(t, want, g)
		}
		charged := r.Stats().AllocatedBytes - before
		assert.GreaterOrEqual(t, charged, int64(4001))
		assert.LessOrEqual(t, charged, int64(readers*4001))
		assert.Zero(t, charged%4001, "whole copies only")
	}
}

// TestMemoryLimitErrorMessages: the message of an error the engine throws
// holds the script's string it names (a property key, an option's value),
// and is charged to the memory limit, so 300 kept errors with 1 MB messages
// do not fit 32 MB.
func TestMemoryLimitErrorMessages(t *testing.T) {
	for _, throw := range []string{
		`null[big]`,
		`undefined[big]()`,
		`new Intl.DateTimeFormat("en", {timeZone: big})`,
		`new Intl.NumberFormat("en", {style: big})`,
		`new Intl.NumberFormat("en", {style: "currency", currency: big})`,
		`new Intl.NumberFormat("en", {style: "unit", unit: big})`,
		`new Intl.DateTimeFormat("en", {calendar: big})`,
		`new Intl.NumberFormat("en", {numberingSystem: big})`,
	} {
		t.Run(throw, func(t *testing.T) {
			r := NewRealmWith(RealmOptions{MemoryLimit: 32 << 20})
			_, err := runScriptValue(t, r, `const big = "A".repeat(1 << 20); const keep = [];
for (let i = 0; i < 300; i++) { try { `+throw+` } catch (e) { if (e instanceof RangeError || e instanceof TypeError) keep.push(e); else throw e } }
keep.length`)
			assert.True(t, errors.Is(err, ErrMemoryLimit), "300 errors with 1 MB messages fit a 32 MB limit: %v", err)
		})
	}
}

// memChargeOf runs src in r and returns what it charged.
func memChargeOf(t *testing.T, r *Realm, src string) int64 {
	t.Helper()
	before := r.Stats().AllocatedBytes
	_, err := runScriptValue(t, r, src)
	require.NoError(t, err)
	return r.Stats().AllocatedBytes - before
}

// TestMemoryLimitErrorMessageOnce: an engine-made error with a 1 MB
// message is charged about 1 MB once (not twice: under 1.25 MB with the
// Intl objects a first use makes), whether NewError or a host function's
// Go error makes it, and an error whose message is the script's own
// string costs no copy.
func TestMemoryLimitErrorMessageOnce(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 40})
	big := strings.Repeat("A", 1<<20)
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("hostFail"), ObjectValue(r.NewNativeFunction(AtomEmpty, 0,
		func(r *Realm, this Value, args []Value) (Value, error) { return Undefined(), errors.New(big) }))))
	_, err := runScriptValue(t, r, `var big = "A".repeat(1 << 20), keep = []`)
	require.NoError(t, err)
	for _, c := range []struct {
		throw    string
		min, max int64
	}{
		{`null[big]`, 1 << 20, 1<<20 + 1<<18},
		{`new Intl.NumberFormat("en", {style: big})`, 1 << 20, 1<<20 + 1<<18},
		{`new Intl.DateTimeFormat("en", {timeZone: big})`, 1 << 20, 1<<20 + 1<<18},
		{`new Intl.NumberFormat("en", {style: "currency", currency: big})`, 1 << 20, 1<<20 + 1<<18},
		{`hostFail()`, 1 << 20, 1<<20 + 1<<18},
		{`throw new RangeError(big)`, 0, 1 << 14},
	} {
		n := memChargeOf(t, r, `try { `+c.throw+` } catch (e) { keep.push(e) }`)
		t.Logf("%s: charged %d", c.throw, n)
		assert.GreaterOrEqual(t, n, c.min, "%s: charged %d", c.throw, n)
		assert.Less(t, n, c.max, "%s: charged %d (twice?)", c.throw, n)
	}
}

// TestMemoryLimitErrorStacks: reading an error's stack makes a new string
// holding its message (1 MB here) and the frames, which is charged, so 300
// kept stacks of one 1 MB message do not fit a 32 MB limit.
func TestMemoryLimitErrorStacks(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 32 << 20})
	_, err := runScriptValue(t, r, `const big = "A".repeat(1 << 20); const keep = [];
for (let i = 0; i < 300; i++) { try { throw new RangeError(big) } catch (e) { keep.push(e.stack) } }
keep.length`)
	assert.True(t, errors.Is(err, ErrMemoryLimit), "300 stacks of 1 MB fit a 32 MB limit: %v", err)
}

// TestMemoryLimitErrorsPastLimit: errors made once the limit is
// exceeded, from the script and from Go (NewError, a host function's Go
// error), neither recurse nor panic; the run stops with the
// MemoryLimitError, and after the host clears the interrupt the next
// charge hits again.
func TestMemoryLimitErrorsPastLimit(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 4 << 20})
	big := strings.Repeat("B", 1<<20)
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("hostFail"), ObjectValue(r.NewNativeFunction(AtomEmpty, 0,
		func(r *Realm, this Value, args []Value) (Value, error) { return Undefined(), errors.New(big) }))))
	_, err := runScriptValue(t, r, `const big = "A".repeat(1 << 20); const keep = [];
for (let i = 0; i < 100; i++) { for (const f of [() => null[big], () => hostFail(), () => new Intl.NumberFormat("en", {style: big})]) try { f() } catch (e) { keep.push(e) } }`)
	require.True(t, errors.Is(err, ErrMemoryLimit), "%v", err)
	hits := r.Stats().MemoryLimitHits
	for range 10 {
		o := r.NewError(KindTypeError, "%s", big)
		require.NotNil(t, o)
		require.True(t, r.hostErrorValue(errors.New(big)).IsObject())
	}
	assert.Equal(t, hits, r.Stats().MemoryLimitHits, "an interrupt is pending: no new hit")
	r.ClearInterrupt()
	r.NewError(KindTypeError, "%s", big)
	assert.Equal(t, hits+1, r.Stats().MemoryLimitHits, "past the limit with no interrupt pending: a new hit")
	r.ClearInterrupt()
	r.ResetAllocation()
	// A reset keeps the last measurement, and the script still keeps more
	// than the limit: the next allocation hits again, and a higher limit
	// lets it run.
	_, err = runScriptValue(t, r, `1`)
	require.ErrorIs(t, err, ErrMemoryLimit)
	r.ClearInterrupt()
	r.SetMemoryLimit(64 << 20)
	_, err = runScriptValue(t, r, `1`)
	require.NoError(t, err)
}

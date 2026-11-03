package moejs_test

// Builtins that call back into JavaScript keep what they build where the
// walk of the heap reaches it, or the measurement waits for them, and
// Function.prototype.apply's argument list counts; what Intl objects keep
// counts once.

import (
	"errors"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runAfterMeasure runs src as a script under a 32 MiB limit after a first
// measurement, and returns the error and what the account held.
func runAfterMeasure(t *testing.T, src string) (error, moejs.Stats) {
	t.Helper()
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 32 << 20})
	base, err := moejs.CompileScript("base.js", `globalThis.garbage = (n) => { let s = 0; for (let i = 0; i < n; i++) s += [i, {i}, "g" + i].length; return s; };`)
	require.NoError(t, err)
	_, err = rt.RunScript(base)
	require.NoError(t, err)
	_ = rt.RebaseAllocation(nil)
	s, err := moejs.CompileScript("case.js", src)
	require.NoError(t, err)
	_, err = rt.RunScript(s)
	return err, rt.Stats()
}

// TestMemoryLimitCountsWhatBuiltinsHold: what a builtin holds while it
// calls back into JavaScript counts, in Go or on the register stack, so
// that 48 MiB held there under a 32 MiB limit stops the call. Each case's
// callbacks make garbage, so that the limit is passed (and a measurement
// made) while the builtin holds.
func TestMemoryLimitCountsWhatBuiltinsHold(t *testing.T) {
	big := `"v".repeat(256 * 1024 - 8) + i`
	for _, c := range []struct{ name, src string }{
		{"Function.prototype.apply, getters of an array-like", `
const al = {length: 192};
for (let i = 0; i < 192; i++) Object.defineProperty(al, i, {get() { garbage(3000); return ` + big + `; }});
(function (...a) { return a.length; }).apply(null, al);`},
		{"Reflect.apply, getters of an array-like (control)", `
const al = {length: 192};
for (let i = 0; i < 192; i++) Object.defineProperty(al, i, {get() { garbage(3000); return ` + big + `; }});
Reflect.apply(function (...a) { return a.length; }, null, al);`},
		{"Promise.prototype.finally, the settled value", `
globalThis.k = [];
Promise.resolve().then(() => { const a = []; for (let i = 0; i < 80; i++) a.push(` + big + `); return a; })
	.finally(() => { for (let i = 0; i < 64; i++) { k.push(` + big + `); garbage(2000); } });`},
		{"Array.from of a fresh iterator holding data (control)", `
const it = {*[Symbol.iterator]() { const keep = []; for (let i = 0; i < 192; i++) { keep.push(` + big + `); yield i; } }};
Array.from(it, (x) => garbage(3000));`},
		{"map building its result (control)", `Array.from({length: 192}, (_, i) => i).map((i) => { garbage(3000); return ` + big + `; });`},
		{"replace with a function, its builder (control)", `"ab".repeat(192).replace(/b/g, (m, i) => { garbage(3000); return "w".repeat(256 * 1024); });`},
		{"JSON.stringify with a replacer (control)", `JSON.stringify(Array.from({length: 192}, (_, i) => i), (k, v) => { garbage(3000); return k === "" ? v : "w".repeat(256 * 1024) + v; });`},
		{"new Map of a generator (control)", `function* g() { for (let i = 0; i < 192; i++) { garbage(3000); yield [i, ` + big + `]; } } new Map(g());`},
	} {
		err, st := runAfterMeasure(t, c.src)
		var mle *moejs.MemoryLimitError
		t.Logf("%s: %v (live %d MiB, %d MiB on the account)", c.name, err, st.LiveBytes>>20, (st.LiveBytes+st.RequestAllocatedBytes)>>20)
		assert.True(t, errors.As(err, &mle), "%s: 48 MiB held under a 32 MiB limit ran on", c.name)
	}
}

// TestMeasureLiveIntlKeptOnce: what Intl objects keep counts once: a Segments
// object and its iterator over an 8 MiB string the program dropped, a
// Locale with long private-use subtags, and the string counted once when
// the program keeps it too.
func TestMeasureLiveIntlKeptOnce(t *testing.T) {
	measure := func(src string) int64 {
		rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 30})
		base := rt.Realm().MeasureLive(nil)
		s, err := moejs.CompileScript("intl.js", src)
		require.NoError(t, err)
		_, err = rt.RunScript(s)
		require.NoError(t, err)
		return rt.Realm().MeasureLive(nil) - base
	}
	const mib = 1 << 20
	str := `const s = ("w ".repeat(4 * 1024 * 1024) + "end").slice(0);`
	for _, c := range []struct {
		name     string
		src      string
		min, max int64
	}{
		{"Segments of a dropped string", str + `globalThis.k = new Intl.Segmenter("en", {granularity: "word"}).segment(s);`, 8 * mib, 9 * mib},
		{"segment iterator only", str + `globalThis.k = new Intl.Segmenter("en", {granularity: "word"}).segment(s)[Symbol.iterator](); k.next();`, 8 * mib, 9 * mib},
		{"Segments and the string", str + `globalThis.k = [s, new Intl.Segmenter().segment(s), new Intl.Segmenter().segment(s)];`, 8 * mib, 9 * mib},
		{"Locale with long private-use subtags", `globalThis.k = Array.from({length: 64}, (_, i) => new Intl.Locale("en-x-" + Array.from({length: 2000}, (_, j) => "abcdefgh").join("-") + "-" + String(i).padStart(8, "0")));`, mib, 4 * mib},
		{"DateTimeFormat with its bound format", `globalThis.k = Array.from({length: 2000}, () => { const f = new Intl.DateTimeFormat("en", {timeZone: "America/Los_Angeles", dateStyle: "full", timeStyle: "long"}); f.format; return f; });`, 1, 64 * mib},
		{"NumberFormat with its bound format", `globalThis.k = Array.from({length: 2000}, () => { const f = new Intl.NumberFormat("en", {style: "currency", currency: "EUR"}); f.format; return f; });`, 1, 64 * mib},
	} {
		got := measure(c.src)
		t.Logf("%-38s %d KiB", c.name, got>>10)
		assert.GreaterOrEqual(t, got, c.min, c.name)
		assert.Less(t, got, c.max, c.name)
	}
	_ = strings.Repeat
}

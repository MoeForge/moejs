package engine

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MoeForge/moejs/compiler"
)

// callArray calls fn with the arguments and returns its array result.
func callArray(t *testing.T, r *Realm, fn Value, args ...Value) *Object {
	t.Helper()
	v, err := r.Call(fn, Undefined(), args)
	require.NoError(t, err)
	require.True(t, IsArray(v))
	return v.AsObject()
}

// warmSites runs fn sitesWarm-1 times, after which fn's literals take
// their sites on its second run from there, as they would on its second
// run with a sitesWarm of one (TestAllocSiteOnce).
func warmSites(t *testing.T, r *Realm, fn Value, args ...Value) {
	t.Helper()
	for range sitesWarm - 1 {
		_, err := r.Call(fn, Undefined(), args)
		require.NoError(t, err)
	}
}

// TestAllocSiteRoom: an array literal filled by push starts, from its
// site's third result, with the room the smaller of the last two took; a
// site whose sizes vary gives no more than the smaller; the array is the
// empty literal's in every other respect.
func TestAllocSiteRoom(t *testing.T) {
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(16), unsafe.Sizeof(allocSite{}), "a site's feedback, which a realm keeps for every literal of a function it ran")
	}
	r := NewRealm()
	fill := evalValue(t, r, "(n => { var a = []; for (var i = 0; i < n; i++) a.push(i); return a })")
	warmSites(t, r, fill, IntValue(1))
	var caps []int
	for range 4 {
		caps = append(caps, cap(callArray(t, r, fill, IntValue(100)).elems()))
	}
	assert.Equal(t, 100, caps[3], "the fourth result needs no growth: %v", caps)
	assert.Greater(t, caps[0], 100, "the first grew past it")
	callArray(t, r, fill, IntValue(10))
	o := callArray(t, r, fill, IntValue(100))
	assert.Greater(t, cap(o.elems()), 100, "after a result of 10 the room is 10, then growth")
	o = callArray(t, r, fill, IntValue(0))
	assert.Equal(t, 10, cap(o.elems()), "the smaller of 100 and 10")
	assert.Equal(t, uint32(0), o.ArrayLength())
	// Only the capacity changes.
	sem := evalValue(t, r, `(() => { var a = []; a[3] = 'x'; a.push(1); return a })`)
	for range sitesWarm + 3 {
		o := callArray(t, r, sem)
		assert.Equal(t, uint32(5), o.ArrayLength())
	}
	assert.Equal(t, "4,,,,x,1,false,true,false", runScriptsIn(t, r,
		`function g() { var a = []; a[3] = 'x'; a.push(1); return a }
var x; for (var i = 0; i < 20; i++) x = g();
[x.length - 1, x.join(), 0 in x, 3 in x, Object.isFrozen(x)].join()`))
	f := evalValue(t, r, `(() => { var a = []; for (var i = 0; i < 50; i++) a.push(i); return Object.freeze(a) })`)
	for range sitesWarm + 3 {
		o := callArray(t, r, f)
		assert.False(t, o.IsExtensible())
		assert.Equal(t, uint32(50), o.ArrayLength())
	}
}

// TestAllocSiteJump: a site whose results are mostly empty and one in
// three large, as React's segments' chunks, gives the large one, growing
// past half of growLargeMin, the room the last two large ones took, the
// smaller.
func TestAllocSiteJump(t *testing.T) {
	r := NewRealm()
	seg := evalValue(t, r, `(globalThis.seg = () => [])`)
	page := evalValue(t, r, `(n => {
	var a = seg(), b = seg(), c = seg();
	for (var i = 0; i < n; i++) c.push('chunk');
	return c })`)
	warmSites(t, r, seg)
	var caps []int
	for _, n := range []int{3000, 2000, 2000, 2500} {
		caps = append(caps, cap(callArray(t, r, page, IntValue(n)).elems()))
	}
	assert.Equal(t, 2000, caps[2], "two large results seen, the smaller: %v", caps)
	assert.Equal(t, 4000, caps[3], "2500 past the room of 2000 doubles: %v", caps)
	// The first grew as without feedback, as does a large array of another
	// site.
	const fill = `(n => { var a = []; for (var i = 0; i < n; i++) a.push(i); return a })`
	ctl := NewRealm()
	assert.Equal(t, cap(callArray(t, ctl, evalValue(t, ctl, fill), IntValue(3000)).elems()), caps[0])
	callArray(t, r, page, IntValue(3000))
	o := callArray(t, r, evalValue(t, r, fill), IntValue(300))
	assert.Equal(t, cap(callArray(t, ctl, evalValue(t, ctl, fill), IntValue(300)).elems()), cap(o.elems()))
}

// TestAllocSiteSettle: a site whose results stay within the literal's
// room settles after siteSettle of them and keeps none; one that makes a
// large result never does.
func TestAllocSiteSettle(t *testing.T) {
	r := NewRealm()
	pair := evalValue(t, r, "((x) => [x, 2])")
	warmSites(t, r, pair)
	for range siteSettle + 2 {
		callArray(t, r, pair)
	}
	tbl := r.lazy.sites
	require.NotNil(t, tbl)
	settled := 0
	for i := range tbl.s {
		if tbl.s[i].still == siteSettled {
			settled++
			assert.Nil(t, tbl.s[i].last)
		}
	}
	assert.Equal(t, 1, settled)
	// Dynamic code has no sites.
	SetCompiler(compiler.Hook{})
	t.Cleanup(func() { SetCompiler(nil) })
	n := len(tbl.s)
	assert.Equal(t, "3", runScriptsIn(t, r, `var d = new Function("var a = []; a.push(1, 2, 3); return a"); d(); d(); d().length`))
	assert.Equal(t, "2", runScriptsIn(t, r, `eval("[1, 2]").length`))
	assert.Equal(t, n, len(tbl.s))
}

// TestAllocSiteRelease: the end of the outermost call and releaseScratch
// drop every site's result, having observed it; the memory account is
// charged for the table and the live measurement counts it with what it
// holds.
func TestAllocSiteRelease(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	r.SetMemoryLimit(1 << 30)
	before := r.MeasureLive(nil)
	charged := r.Stats().AllocatedBytes
	fill := evalValue(t, r, "(n => { var a = []; for (var i = 0; i < n; i++) a.push(i); return a })")
	warmSites(t, r, fill, IntValue(1))
	holding := func() (n int) {
		for i := range r.lazy.sites.s {
			if r.lazy.sites.s[i].last != nil {
				n++
			}
		}
		return n
	}
	// Inside a call (HoldJobs), the site holds its latest result.
	r.HoldJobs()
	for range 3 {
		callArray(t, r, fill, IntValue(5000))
	}
	require.NotNil(t, r.lazy.sites)
	table := int64(unsafe.Sizeof(siteTable{}) + unsafe.Sizeof(allocSite{}))
	assert.GreaterOrEqual(t, r.Stats().AllocatedBytes-charged, table)
	require.Equal(t, 1, holding())
	assert.GreaterOrEqual(t, r.MeasureLive(nil)-before, table+5000*int64(valueSize), "the table and the array it holds")
	require.NoError(t, r.ReleaseJobs(nil))
	assert.Zero(t, holding(), "the end of the outermost call drops it")
	assert.Empty(t, r.lazy.sites.held)
	// The room stays: the next result starts with it.
	o := callArray(t, r, fill, IntValue(5000))
	assert.Equal(t, 5000, cap(o.elems()))
	assert.Zero(t, holding())
	// releaseScratch drops them too.
	r.HoldJobs()
	callArray(t, r, fill, IntValue(10))
	require.Equal(t, 1, holding())
	r.releaseScratch()
	assert.Zero(t, holding())
	require.NoError(t, r.ReleaseJobs(nil))
	o = callArray(t, r, fill, IntValue(10))
	assert.Equal(t, 10, cap(o.elems()), "observed when dropped: the smaller of 5000 and 10")
}

// TestAllocSiteSweep: inside a long call, a site that stops running holds
// its latest result for at most two sweeps, unless it is a candidate's; a
// large result of a site that is not one is dropped and makes it one.
func TestAllocSiteSweep(t *testing.T) {
	r := NewRealm()
	const src = "(n => { var a = []; for (var i = 0; i < n; i++) a.push(i); return a })"
	once, big, wide := evalValue(t, r, src), evalValue(t, r, src), evalValue(t, r, src)
	// Results that outgrow the literal: the site never settles.
	other := evalValue(t, r, "(n => { for (var i = 0; i < n; i++) [].push(1, 2, 3, 4, 5) })")
	for _, f := range []Value{once, big, wide} {
		warmSites(t, r, f, IntValue(1))
		callArray(t, r, f, IntValue(1))
	}
	callArray(t, r, once, IntValue(1))
	callArray(t, r, big, IntValue(1000)) // observed at the call's end: a candidate
	callArray(t, r, wide, IntValue(1))
	r.HoldJobs()
	small := callArray(t, r, once, IntValue(10))
	large := callArray(t, r, big, IntValue(1000))
	wl := callArray(t, r, wide, IntValue(1000))
	tbl := r.lazy.sites
	site := func(o *Object) int {
		for i := range tbl.s {
			if tbl.s[i].last == o {
				return i
			}
		}
		return -1
	}
	require.NotEqual(t, -1, site(small))
	w := site(wl)
	require.NotEqual(t, -1, w)
	assert.False(t, tbl.isCandidate(uint32(w)))
	_, err := r.Call(other, Undefined(), []Value{IntValue(siteSweep + sitesWarm)})
	require.NoError(t, err)
	assert.NotEqual(t, -1, site(small), "one sweep keeps it")
	_, err = r.Call(other, Undefined(), []Value{IntValue(siteSweep)})
	require.NoError(t, err)
	assert.Equal(t, -1, site(small), "the second drops it")
	assert.NotEqual(t, -1, site(large), "a candidate's latest result stays")
	assert.Equal(t, -1, site(wl), "a large result of a site that is not a candidate is dropped")
	assert.True(t, tbl.isCandidate(uint32(w)), "and makes it one")
	assert.Equal(t, uint16(1000), tbl.s[w].fin[0])
	require.NoError(t, r.ReleaseJobs(nil))
	assert.Equal(t, -1, site(large))
}

// TestAllocSiteSweepBound: inside a long call, many sites that each make
// one large array, which the program drops, keep none of them past two
// sweeps but the candidates' latest, and the live measurement then counts
// no more than those.
func TestAllocSiteSweepBound(t *testing.T) {
	const sites, n = 12, 2000
	r := NewRealm()
	var fs []Value
	for range sites {
		f := evalValue(t, r, "(n => { var a = []; for (var i = 0; i < n; i++) a.push(i); return a.length })")
		for range sitesWarm + 1 {
			_, err := r.Call(f, Undefined(), []Value{IntValue(1)})
			require.NoError(t, err)
		}
		fs = append(fs, f)
	}
	other := evalValue(t, r, "(n => { for (var i = 0; i < n; i++) [].push(1, 2, 3, 4, 5) })")
	tbl := r.lazy.sites
	require.NotNil(t, tbl)
	holding := func() (k int) {
		for i := range tbl.s {
			if l := tbl.s[i].last; l != nil && l.elemLen() == n {
				k++
			}
		}
		return k
	}
	base := r.MeasureLive(nil)
	r.HoldJobs()
	for _, f := range fs {
		_, err := r.Call(f, Undefined(), []Value{IntValue(n)})
		require.NoError(t, err)
	}
	require.Equal(t, sites, holding(), "each site holds its dropped result")
	arrays := int64(n) * int64(valueSize)
	assert.GreaterOrEqual(t, r.MeasureLive(nil)-base, sites*arrays)
	for _, k := range []int{siteSweep + sitesWarm, siteSweep} {
		_, err := r.Call(other, Undefined(), []Value{IntValue(k)})
		require.NoError(t, err)
	}
	assert.LessOrEqual(t, holding(), siteCands)
	assert.Less(t, r.MeasureLive(nil)-base, (siteCands+1)*arrays, "after the sweeps the measurement counts at most the candidates' results")
	require.NoError(t, r.ReleaseJobs(nil))
	assert.Zero(t, holding())
}

// TestAllocSiteOnce: code that runs once, straight through, takes no
// sites, nor does code whose literals run again fewer than sitesWarm
// times, nor a script's own code; a loop or calls past that bind a
// function's.
func TestAllocSiteOnce(t *testing.T) {
	r := NewRealm()
	assert.Equal(t, "60", runScriptsIn(t, r, `var n = 0; for (var i = 0; i < 20; i++) n += [i].length + [i, i].length; n`))
	assert.True(t, r.lazy == nil || r.lazy.sites == nil, "a script's own literals take none")
	loop := evalValue(t, r, `(k => { var n = 0; for (var i = 0; i < k; i++) n += [i].length + [i, i].length + [].length; return [n] })`)
	callArray(t, r, loop, IntValue(sitesWarm))
	assert.True(t, r.lazy == nil || r.lazy.sites == nil, "again fifteen times")
	callArray(t, r, loop, IntValue(sitesWarm+1))
	require.NotNil(t, r.lazy.sites)
	assert.Len(t, r.lazy.sites.s, 4, "the loop's three literals and the result's")
	f := evalValue(t, r, "(() => [[], []])")
	for range sitesWarm {
		callArray(t, r, f)
	}
	assert.Len(t, r.lazy.sites.s, 4, "sitesWarm calls")
	callArray(t, r, f)
	assert.Len(t, r.lazy.sites.s, 7, "the next binds the function's three")
}

// TestAllocSitePanic: a Go panic out of a call skips the end of the
// outermost call; the host's recovery (RestoreCallState and DropJobs)
// drops the sites' results, and the end of a later call drops its own.
func TestAllocSitePanic(t *testing.T) {
	r := NewRealm()
	boom := r.NewNativeFunction(FromGoString("boom"), 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		panic("host panic")
	})
	fill := evalValue(t, r, "((n, f) => { var a = []; for (var i = 0; i < n; i++) a.push(i); if (f) f(); return a })")
	other := evalValue(t, r, "(n => { var b = []; for (var i = 0; i < n; i++) b.push(i); return b })")
	warmSites(t, r, fill, IntValue(1), Undefined())
	warmSites(t, r, other, IntValue(1))
	for range 2 {
		callArray(t, r, fill, IntValue(10), Undefined())
		callArray(t, r, other, IntValue(10))
	}
	require.NotNil(t, r.lazy.sites)
	holding := func() (n int) {
		for i := range r.lazy.sites.s {
			if r.lazy.sites.s[i].last != nil {
				n++
			}
		}
		return n
	}
	require.Zero(t, holding())
	saved := r.CallState()
	func() {
		defer func() {
			require.NotNil(t, recover())
			r.RestoreCallState(saved)
			r.DropJobs(nil)
		}()
		r.Call(fill, Undefined(), []Value{IntValue(1000), ObjectValue(boom)})
	}()
	assert.Zero(t, holding(), "DropJobs drops what the call's sites hold")
	assert.Empty(t, r.lazy.sites.held)
	callArray(t, r, other, IntValue(2000))
	assert.Zero(t, holding(), "the end of the next call drops its result")
	o := callArray(t, r, fill, IntValue(1000), Undefined())
	assert.Equal(t, 1000, cap(o.elems()), "the panicked call's result was observed: the smaller of its 1000 and 1000")
}

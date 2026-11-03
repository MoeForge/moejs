package engine

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests of ReleaseCallData (realm_calldata.go). The root package checks,
// through the public API, that the host data of a released request is
// collected; these check each reference it drops, and that it drops none
// inside a call.

// callDataSource reads its argument through a closure (whose frame record
// the call leaves above the live frames) and matches a UTF-16 subject on
// the RE2 path (whose working copy the realm caches, and whose match the
// legacy statics, read once, keep).
const callDataSource = `
export function hook(x, inside) {
	const names = () => x.list.map(m => m.name + x.model);
	const m = /(中+)文/.exec(x.text);
	if (RegExp.$1 !== m[1]) throw new Error("RegExp.$1");
	inside();
	return names().join() + "|" + m[1].length + "|" + x.meta.tag;
}
export function deep(n) {
	const a = n + 1, b = a + 1, c = b + 1, d = c + 1, e = d + 1, f = e + 1, g = f + 1, h = g + 1;
	const i = h + 1, j = i + 1, k = j + 1, l = k + 1, o = l + 1, p = o + 1, q = p + 1, s = q + 1;
	return n > 0 ? deep(n - 1) + a + b + c + d + e + f + g + h + i + j + k + l + o + p + q + s : 0;
}
`

func callDataArg(i int) map[string]any {
	n := strconv.Itoa(i)
	return map[string]any{
		"model": "m" + n,
		"list":  []any{map[string]any{"name": "a"}, map[string]any{"name": "b" + n}},
		"text":  strings.Repeat("中", 64) + "文" + n,
		"meta":  map[string]any{"tag": "t" + n},
	}
}

func callDataWant(i int) string {
	n := strconv.Itoa(i)
	return "am" + n + ",b" + n + "m" + n + "|64|t" + n
}

var callDataNoop = func(*Realm, Value, []Value) (Value, error) { return Undefined(), nil }

// TestReleaseCallData: a request leaves its host conversion chunks, the
// registers and frame records of its finished calls, the working copy of
// its RegExp subject and its last match in the realm; ReleaseCallData drops
// each, and the values the request returned or the module kept stay valid.
func TestReleaseCallData(t *testing.T) {
	for _, rc := range hostLazyRealms {
		t.Run(rc.name, func(t *testing.T) {
			f := newHostLazyFixture(t, callDataSource, rc.opts)
			r, st := f.r, &f.r.interp
			noop := ObjectValue(r.NewNativeFunction(AtomEmpty, 0, callDataNoop))
			for i := range 3 {
				x, err := r.FromGo(callDataArg(i))
				require.NoError(t, err)
				require.Equal(t, callDataWant(i), f.callValues("hook", x, noop))
				h := r.fromGo.heap
				require.NotNil(t, h.maps, "request %d: maps chunk", i)
				require.NotNil(t, h.strings, "request %d: strings chunk", i)
				require.NotEqual(t, [hostStrSlots]*String{}, h.strs, "request %d: string cache", i)
				require.True(t, slices.ContainsFunc(st.stack[st.sp:], func(v Value) bool { return v.ptr != nil }), "request %d: stale registers", i)
				require.True(t, slices.ContainsFunc(st.frames[st.nframes:], func(fi frameInfo) bool { return fi.fn != nil }), "request %d: stale frames", i)
				require.NotNil(t, r.regexps.subjects[0].s, "request %d: RegExp subject", i)
				require.NotNil(t, r.regexps.lastS, "request %d: RegExp statics' subject", i)
				require.NotNil(t, r.lazy.statics, "request %d: RegExp statics' match", i)

				r.ReleaseCallData()
				assert.Nil(t, h.maps)
				assert.Nil(t, h.arrays)
				assert.Nil(t, h.values)
				assert.Nil(t, h.strings)
				assert.Equal(t, [hostStrSlots]*String{}, h.strs)
				assert.False(t, h.touched)
				assert.Equal(t, 0, st.sp)
				assert.Equal(t, 0, st.nframes)
				assert.Empty(t, st.stack)
				assert.Equal(t, initialStackSize, cap(st.stack), "a shallow request keeps its stack")
				assert.False(t, slices.ContainsFunc(st.stack[:cap(st.stack)], func(v Value) bool { return v != Value{} }), "registers")
				assert.False(t, slices.ContainsFunc(st.frames, func(fi frameInfo) bool { return fi != frameInfo{} }), "frame records")
				for j := range r.regexps.subjects {
					assert.Nil(t, r.regexps.subjects[j].s, "RegExp subject %d", j)
					assert.Nil(t, r.regexps.subjects[j].text, "RegExp working copy %d", j)
				}
				assert.Equal(t, int32(0), r.regexps.subjNext)
				assert.Nil(t, r.regexps.lastS, "RegExp statics' subject")
				assert.Nil(t, r.regexps.lastC, "RegExp statics' program")
				assert.Nil(t, r.lazy.statics, "RegExp statics' match")

				// The released argument keeps working, materialized parts
				// and placeholders alike, and so does the next call.
				hostLazyChurn()
				require.Equal(t, callDataWant(i), f.callValues("hook", x, noop))
				r.ReleaseCallData()
			}
		})
	}
}

// TestReleaseCallDataInsideCall: called from a native function during a
// call, ReleaseCallData drops nothing, and the call goes on to materialize
// more of its argument from the same chunks.
func TestReleaseCallDataInsideCall(t *testing.T) {
	f := newHostLazyFixture(t, callDataSource, RealmOptions{})
	r, st := f.r, &f.r.interp
	calls := 0
	inside := ObjectValue(r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		calls++
		h := r.fromGo.heap
		maps, values, strs := h.maps, h.values, h.strs
		stack, frames := slices.Clone(st.stack), slices.Clone(st.frames)
		sp, nframes := st.sp, st.nframes
		subject := r.regexps.subjects[0]
		require.NotNil(t, subject.s)

		r.ReleaseCallData()
		assert.Equal(t, len(maps), len(h.maps))
		assert.Same(t, unsafe.SliceData(maps), unsafe.SliceData(h.maps), "maps chunk")
		assert.Same(t, unsafe.SliceData(values), unsafe.SliceData(h.values), "values chunk")
		assert.Equal(t, strs, h.strs)
		assert.True(t, h.touched)
		assert.Equal(t, sp, st.sp)
		assert.Equal(t, nframes, st.nframes)
		assert.True(t, slices.Equal(stack, st.stack), "registers")
		assert.True(t, slices.Equal(frames, st.frames), "frame records")
		assert.Same(t, subject.s, r.regexps.subjects[0].s)
		assert.Same(t, unsafe.SliceData(subject.text), unsafe.SliceData(r.regexps.subjects[0].text))
		return Undefined(), nil
	}))
	for i := range 3 {
		x, err := r.FromGo(callDataArg(i))
		require.NoError(t, err)
		// x.meta and the list's maps materialize after the release.
		require.Equal(t, callDataWant(i), f.callValues("hook", x, inside))
	}
	assert.Equal(t, 3, calls)
}

// TestReleaseCallDataDeepRecursion: a stack a deep recursion grew past
// maxRetainedStack, and a frame table past maxRetainedFrames, are dropped
// rather than cleared, and later calls, shallow and deep, grow them again.
func TestReleaseCallDataDeepRecursion(t *testing.T) {
	f := newHostLazyFixture(t, callDataSource, RealmOptions{})
	r, st := f.r, &f.r.interp
	deep := func(n int) float64 {
		res, err := r.Call(f.fn("deep"), Undefined(), []Value{IntValue(n)})
		require.NoError(t, err)
		return res.AsNumber()
	}
	shallow := deep(3)
	shallowStack, shallowFrames := cap(st.stack), len(st.frames)
	require.LessOrEqual(t, shallowStack, maxRetainedStack)
	deepest := deep(400)
	require.Greater(t, cap(st.stack), maxRetainedStack)
	require.Greater(t, len(st.frames), maxRetainedFrames)

	r.ReleaseCallData()
	assert.Nil(t, st.stack)
	assert.Nil(t, st.frames)
	assert.Equal(t, shallow, deep(3))
	assert.Equal(t, shallowStack, cap(st.stack))
	assert.Len(t, st.frames, shallowFrames)
	r.ReleaseCallData()
	assert.Equal(t, shallowStack, cap(st.stack), "a shallow call's stack is cleared, not dropped")
	assert.Len(t, st.frames, shallowFrames)
	assert.Equal(t, deepest, deep(400))
	r.ReleaseCallData()
	assert.Nil(t, st.stack)
	assert.Equal(t, shallow, deep(3))
}

// TestReleaseCallDataExtent: the length of the register stack is the extent
// the calls since the last release used; past it, up to the capacity, every
// register is zero. A release clears the extent and empties the stack, so a
// shallow request after a deep one has only its own registers to clear.
func TestReleaseCallDataExtent(t *testing.T) {
	f := newHostLazyFixture(t, callDataSource, RealmOptions{})
	r, st := f.r, &f.r.interp
	deep := func(n int) {
		t.Helper()
		_, err := r.Call(f.fn("deep"), Undefined(), []Value{IntValue(n)})
		require.NoError(t, err)
	}
	zeroPast := func(when string) {
		t.Helper()
		assert.False(t, slices.ContainsFunc(st.stack[len(st.stack):cap(st.stack)], func(v Value) bool { return v != Value{} }), "registers past the extent, %s", when)
	}
	deep(40)
	zeroPast("after a deep call")
	r.ReleaseCallData()
	assert.Empty(t, st.stack)
	zeroPast("after the release")
	stack := cap(st.stack)
	require.Greater(t, stack, 4*initialStackSize)

	deep(1)
	assert.Less(t, len(st.stack), stack/4, "a shallow call extends the stack only by its registers")
	assert.True(t, slices.ContainsFunc(st.stack, func(v Value) bool { return v != Value{} }), "the registers it used")
	zeroPast("after a shallow call")
	deep(40)
	assert.Equal(t, stack, cap(st.stack), "the capacity is kept")
	zeroPast("after a deep call again")
	r.ReleaseCallData()
	assert.Empty(t, st.stack)
	zeroPast("after the last release")
}

// BenchmarkReleaseCallData measures ReleaseCallData after a hook call, with
// the register stack at its initial size, at the largest size it clears and
// past it (dropped).
func BenchmarkReleaseCallData(b *testing.B) {
	for _, size := range []int{initialStackSize, maxRetainedStack, 2 * maxRetainedStack} {
		b.Run("stack="+strconv.Itoa(size), func(b *testing.B) {
			m, err := syntax.ParseModule("calldata.js", callDataSource, syntax.Options{})
			if err != nil {
				b.Fatal(err)
			}
			code, err := compiler.CompileModule(m)
			if err != nil {
				b.Fatal(err)
			}
			r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			env, err := r.EvaluateModule(code)
			if err != nil {
				b.Fatal(err)
			}
			hook, _ := env.GetBindingValue("hook")
			x, err := r.FromGo(callDataArg(0))
			if err != nil {
				b.Fatal(err)
			}
			noop := ObjectValue(r.NewNativeFunction(AtomEmpty, 0, callDataNoop))
			if _, err := r.Call(hook, Undefined(), []Value{x, noop}); err != nil {
				b.Fatal(err)
			}
			stack := make([]Value, size)
			b.ReportAllocs()
			for b.Loop() {
				r.interp.stack = stack
				r.ReleaseCallData()
			}
		})
	}
}

// TestReleaseScratch: ReleaseScratch drops the registers, frame records and
// RegExp working copies a request left, as ReleaseCallData does, and keeps
// what the host or JavaScript can observe: the host conversion's chunks
// and the legacy RegExp statics. Inside a call it does nothing.
func TestReleaseScratch(t *testing.T) {
	f := newHostLazyFixture(t, callDataSource, RealmOptions{})
	r, st := f.r, &f.r.interp
	calls := 0
	inside := ObjectValue(r.NewNativeFunction(AtomEmpty, 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		calls++
		stack, frames := slices.Clone(st.stack), slices.Clone(st.frames)
		r.ReleaseScratch()
		assert.True(t, slices.Equal(stack, st.stack), "registers")
		assert.True(t, slices.Equal(frames, st.frames), "frame records")
		assert.NotNil(t, r.regexps.subjects[0].s)
		return Undefined(), nil
	}))
	for i := range 3 {
		x, err := r.FromGo(callDataArg(i))
		require.NoError(t, err)
		require.Equal(t, callDataWant(i), f.callValues("hook", x, inside))
		require.True(t, slices.ContainsFunc(st.stack, func(v Value) bool { return v.ptr != nil }), "request %d: stale registers", i)
		require.NotNil(t, r.regexps.subjects[0].s, "request %d: RegExp subject", i)

		r.ReleaseScratch()
		assert.Empty(t, st.stack)
		assert.False(t, slices.ContainsFunc(st.stack[:cap(st.stack)], func(v Value) bool { return v != Value{} }), "registers")
		assert.False(t, slices.ContainsFunc(st.frames, func(fi frameInfo) bool { return fi != frameInfo{} }), "frame records")
		for j := range r.regexps.subjects {
			assert.Nil(t, r.regexps.subjects[j].s, "RegExp subject %d", j)
		}
		assert.NotNil(t, r.fromGo.heap.maps, "the host conversion's chunks stay")
		assert.NotNil(t, r.regexps.lastS, "the RegExp statics stay")
		assert.NotNil(t, r.lazy.statics, "the RegExp statics' match stays")
		r.ReleaseCallData()
	}
	assert.Equal(t, 3, calls)
}

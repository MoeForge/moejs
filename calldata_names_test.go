package moejs_test

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"
	"weak"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The names a runtime keeps (atoms, its intern caches, shape keys, compiled
// patterns) must not share the memory of the request they came from: a name
// outlives the request, for the runtime's lifetime or, through the shared
// shape tree and pattern cache, the process's. These tests take a name from
// a large host string and check that the string is collected once the
// request is released.

const callDataNamesSource = `
export function parse(s) { return Object.keys(JSON.parse(s)).length; }
export function computed(s, n) { const o = {}; o[s.slice(0, n)] = 1; return Object.keys(o)[0].length; }
export function pattern(s, n) { return new RegExp(s.slice(0, n)).source.length; }
export function keys(m) { return Object.keys(m).length; }
export function evaluate(s) { return (0, eval)(s); }
`

var nameSeq int

// newName returns a name no test has used in this process, so its atom is
// new to the process unless the test creates it first.
func newName(prefix string) string {
	nameSeq++
	return prefix + strconv.Itoa(nameSeq) + "_x"
}

func newNamesRuntime(t *testing.T, opts moejs.Options) (*moejs.Runtime, *moejs.Module) {
	t.Helper()
	mod, err := moejs.Compile("names.js", callDataNamesSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(opts)
	require.NoError(t, rt.Load(mod))
	// Enough names through the realm that it has its intern caches
	// (engine.internCacheAfter), so the names below go through them.
	var warm []string
	for i := range 16 {
		warm = append(warm, `"w`+strconv.Itoa(i)+`":0`)
	}
	callNames(t, rt, mustHook(t, mod, "parse"), "{"+strings.Join(warm, ",")+"}")
	rt.ReleaseCallData()
	return rt, mod
}

// jsonBody is an ASCII JSON text of about size bytes whose first key is
// key, and a weak pointer to its bytes.
//
//go:noinline
func jsonBody(size int, key string) (string, weak.Pointer[byte]) {
	var sb strings.Builder
	sb.WriteString(`{"` + key + `":1,"model":"m","input":[`)
	for sb.Len() < size {
		sb.WriteString(`{"role":"user","content":"` + strings.Repeat("x", 200) + `"},`)
	}
	sb.WriteString(`{"role":"end"}]}`)
	s := sb.String()
	return s, weak.Make(unsafe.StringData(s))
}

// callNames converts s and calls h with it and the extra arguments.
func callNames(t *testing.T, rt *moejs.Runtime, h moejs.Hook, s string, args ...moejs.Value) moejs.Value {
	t.Helper()
	x, err := rt.FromGo(s)
	require.NoError(t, err)
	res, err := rt.Call(h, append([]moejs.Value{x}, args...)...)
	require.NoError(t, err)
	return res
}

// parseBody runs JSON.parse on a 4 MiB host text whose first key is key.
//
//go:noinline
func parseBody(t *testing.T, rt *moejs.Runtime, mod *moejs.Module, key string) weak.Pointer[byte] {
	body, wp := jsonBody(4<<20, key)
	res := callNames(t, rt, mustHook(t, mod, "parse"), body)
	assert.Equal(t, 3.0, res.AsNumber())
	return wp
}

// TestReleaseCallDataCollectsParsedText: JSON.parse of a host text whose
// key is new to the process, or only to the realm, keeps no part of the
// text: neither the realm's intern cache nor the atom, which the shared
// shape tree keeps for the process, shares its bytes.
func TestReleaseCallDataCollectsParsedText(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts moejs.Options
		atom bool // the name's atom exists before the parse
	}{
		{name: "new-atom", opts: moejs.Options{}},
		{name: "new-atom/mutable-intrinsics", opts: moejs.Options{MutableIntrinsics: true}},
		{name: "new-in-realm", opts: moejs.Options{}, atom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, mod := newNamesRuntime(t, tc.opts)
			key := newName("json")
			var held engine.PropertyKey
			if tc.atom {
				held = engine.InternKey(key)
			}
			wp := parseBody(t, rt, mod, key)
			rt.ReleaseCallData()
			runtime.GC()
			runtime.GC()
			assert.Nil(t, wp.Value(), "the parsed text is collected once released")
			runtime.KeepAlive(rt)

			rt, mod = nil, nil
			runtime.GC()
			runtime.GC()
			assert.Nil(t, wp.Value(), "the parsed text is collected once the runtime is dropped")
			runtime.KeepAlive(held)
		})
	}
}

// computedKey sets obj[s.slice(0, len(prefix))] = 1 in JavaScript, s being
// prefix and 1 MiB more, and returns a weak pointer to the memory of s: its
// bytes, or the code units of its conversion when it is not ASCII.
//
//go:noinline
func computedKey(t *testing.T, rt *moejs.Runtime, mod *moejs.Module, prefix, fill string) weak.Pointer[byte] {
	s := prefix + strings.Repeat(fill, 1<<20)
	x, err := rt.FromGo(s)
	require.NoError(t, err)
	wp, n := weak.Make(unsafe.StringData(s)), len(prefix)
	if !x.AsString().IsASCII() {
		wp = weak.Make((*byte)(unsafe.Pointer(unsafe.SliceData(x.AsString().UTF16()))))
		n = len([]rune(prefix))
	}
	res, err := rt.Call(mustHook(t, mod, "computed"), x, moejs.Int(int64(n)))
	require.NoError(t, err)
	assert.Equal(t, float64(n), res.AsNumber())
	return wp
}

// TestReleaseCallDataCollectsComputedKey: a property name sliced from a
// large string (a substring shares its string's memory) keeps no part of
// the string.
func TestReleaseCallDataCollectsComputedKey(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, fill string
		atom               bool
	}{
		{name: "ascii/new-atom", prefix: "key", fill: "Z"},
		{name: "ascii/new-in-realm", prefix: "key", fill: "Z", atom: true},
		{name: "utf16/new-atom", prefix: "键", fill: "字"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, mod := newNamesRuntime(t, moejs.Options{})
			prefix := newName(tc.prefix)
			var held engine.PropertyKey
			if tc.atom {
				held = engine.InternKey(prefix)
			}
			wp := computedKey(t, rt, mod, prefix, tc.fill)
			rt.ReleaseCallData()
			runtime.GC()
			runtime.GC()
			assert.Nil(t, wp.Value(), "the string is collected once released")
			runtime.KeepAlive(rt)
			runtime.KeepAlive(held)
		})
	}
}

// TestReleaseCallDataCollectsPatternSource: a RegExp pattern taken from a
// large string keeps no part of it through the compiled-pattern caches,
// the realm's and the process-wide one: a pattern sliced from the string,
// or a literal in code evaluated from it, whose compiled code slices the
// pattern from the source text.
func TestReleaseCallDataCollectsPatternSource(t *testing.T) {
	t.Run("sliced", func(t *testing.T) {
		rt, mod := newNamesRuntime(t, moejs.Options{})
		pattern := newName("re")
		wp := func() weak.Pointer[byte] {
			s := pattern + strings.Repeat("Z", 1<<20)
			res := callNames(t, rt, mustHook(t, mod, "pattern"), s, moejs.Int(int64(len(pattern))))
			assert.Equal(t, float64(len(pattern)), res.AsNumber())
			return weak.Make(unsafe.StringData(s))
		}()
		rt.ReleaseCallData()
		runtime.GC()
		runtime.GC()
		assert.Nil(t, wp.Value(), "the string is collected once released")
		runtime.KeepAlive(rt)
	})
	t.Run("literal-in-eval", func(t *testing.T) {
		rt, mod := newNamesRuntime(t, moejs.Options{})
		pattern, ev := newName("lit"), mustHook(t, mod, "evaluate")
		wp := func() weak.Pointer[byte] {
			// No property access: code with inline caches stays held until
			// a reclaim round of the realm's dynamic code.
			s := "/" + pattern + "/; 1 //" + strings.Repeat("Z", 1<<19)
			res := callNames(t, rt, ev, s)
			assert.Equal(t, 1.0, res.AsNumber())
			return weak.Make(unsafe.StringData(s))
		}()
		// The realm keeps the code of the last 64 strings it evaluated
		// (engine.evalCacheSize), and with it their source text.
		for i := range 64 {
			callNames(t, rt, ev, strconv.Itoa(i))
		}
		rt.ReleaseCallData()
		runtime.GC()
		runtime.GC()
		assert.Nil(t, wp.Value(), "the source text is collected once released")
		runtime.KeepAlive(rt)
	})
}

// hostMapKey converts a host map whose key is a slice of a large string,
// as a zero-copy JSON decoder gives, and reads its keys in JavaScript.
//
//go:noinline
func hostMapKey(t *testing.T, rt *moejs.Runtime, mod *moejs.Module, name, fill string) weak.Pointer[byte] {
	s := name + strings.Repeat(fill, 1<<20)
	x, err := rt.FromGo(map[string]any{s[:len(name)]: 1.0, "model": "m"})
	require.NoError(t, err)
	res, err := rt.Call(mustHook(t, mod, "keys"), x)
	require.NoError(t, err)
	assert.Equal(t, 2.0, res.AsNumber())
	return weak.Make(unsafe.StringData(s))
}

// TestReleaseCallDataCollectsHostMapKey: the host shape cache keeps its own
// copy of a new key set, never the host's key strings.
func TestReleaseCallDataCollectsHostMapKey(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, fill string
		atom               bool
	}{
		{name: "ascii/new-atom", prefix: "key", fill: "Z"},
		{name: "ascii/existing-atom", prefix: "key", fill: "Z", atom: true},
		{name: "utf16", prefix: "键", fill: "字"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, mod := newNamesRuntime(t, moejs.Options{})
			name := newName(tc.prefix)
			var held engine.PropertyKey
			if tc.atom {
				held = engine.InternKey(name)
			}
			wp := hostMapKey(t, rt, mod, name, tc.fill)
			rt.ReleaseCallData()
			runtime.GC()
			runtime.GC()
			assert.Nil(t, wp.Value(), "the key's string is collected once released")
			runtime.KeepAlive(rt)
			runtime.KeepAlive(held)
		})
	}
}

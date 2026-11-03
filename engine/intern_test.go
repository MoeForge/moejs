package engine

import (
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
	"weak"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInternCrossRealmKeysAfterManyNames is an audit reproducer,
// generalized: after the process-wide table has absorbed far more names
// than a per-shard cap would hold, a property key that one realm's module
// materializes into the shared funcMeta must be the same pointer every
// other realm produces through FromGo, so `o.prop` reads the value in each
// realm. With the cap, the first realm's key was realm-local and the second
// realm read undefined.
func TestInternCrossRealmKeysAfterManyNames(t *testing.T) {
	const prop = "internTestProp"
	filler := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	names := make([]string, 40_000)
	for i := range names {
		names[i] = "intern_fill_" + strconv.Itoa(i)
		filler.InternGoString(names[i])
	}
	m, err := syntax.ParseModule("intern.js", "export function get(o) { return o."+prop+"; }", syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	call := func(r *Realm) any {
		env, err := r.EvaluateModule(code)
		require.NoError(t, err)
		fn, _ := env.GetBindingValue("get")
		arg, err := r.FromGo(map[string]any{prop: 42})
		require.NoError(t, err)
		res, err := r.Call(fn, Undefined(), []Value{arg})
		require.NoError(t, err)
		return r.ToGo(res)
	}
	r1 := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	r2 := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	require.Equal(t, int64(42), call(r1), "first realm")
	require.Equal(t, int64(42), call(r2), "second realm reads through the shared funcMeta")
	// Every name has one process-wide identity.
	for _, i := range []int{0, 1, 999, 20_000, 39_999} {
		require.Same(t, filler.InternGoString(names[i]), r2.InternGoString(names[i]), names[i])
	}
	require.Same(t, r1.InternGoString(prop), r2.InternGoString(prop))
}

// TestInternTableConcurrent interns the same names from many realms on
// many goroutines at once: every realm must end up with the same pointer
// for each name (the insert race resolves to one winner).
func TestInternTableConcurrent(t *testing.T) {
	const goroutines, names = 16, 2000
	results := make([][]*String, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			out := make([]*String, names)
			for i := range names {
				out[i] = r.InternGoString("intern_race_" + strconv.Itoa(i))
			}
			results[g] = out
		}()
	}
	wg.Wait()
	for i := range names {
		for g := 1; g < goroutines; g++ {
			require.Same(t, results[0][i], results[g][i], "goroutine %d name %d", g, i)
		}
		require.True(t, results[0][i].IsInterned())
	}
}

// internOnce interns name in a throwaway realm and returns only a weak
// pointer to the atom, so nothing keeps it alive.
//
//go:noinline
func internOnce(name string) weak.Pointer[String] {
	r := NewRealm()
	return weak.Make(r.InternGoString(name))
}

// TestInternTableReleasesUnusedNames checks that the table does not pin
// names: an atom referenced by no realm is collected, and the name interns
// again afterwards (the dead entry is replaced or already dropped).
func TestInternTableReleasesUnusedNames(t *testing.T) {
	const name = "intern_released_only_here"
	wp := internOnce(name)
	runtime.GC()
	runtime.GC()
	require.Nil(t, wp.Value(), "an atom no realm references must be collectable")
	r := NewRealm()
	again := r.InternGoString(name)
	require.True(t, again.IsInterned())
	require.Same(t, again, NewRealm().InternGoString(name))
	require.Same(t, again, globalInternASCII.lookup(name))
}

// TestAtomIDWrapSkipsStaticIDs checks that a wrapped atom counter resumes
// above the static atoms rather than handing out their ids again.
func TestAtomIDWrapSkipsStaticIDs(t *testing.T) {
	var c atomic.Uint32
	c.Store(^uint32(0) - 1)
	require.Equal(t, ^uint32(0), takeAtomID(&c))
	require.Equal(t, uint32(staticAtomCount+1), takeAtomID(&c))
	require.Equal(t, uint32(staticAtomCount+2), takeAtomID(&c))
}

var identityRuns atomic.Int32

// contentIn reports whether the content of a starts inside that of b; both
// are flat.
func contentIn(a, b *String) bool {
	span := func(s *String) (uintptr, uintptr) {
		if s.kind == strASCII {
			return uintptr(unsafe.Pointer(unsafe.StringData(s.s))), uintptr(len(s.s))
		}
		return uintptr(unsafe.Pointer(unsafe.SliceData(s.units()))), 2 * uintptr(s.n)
	}
	p, _ := span(a)
	q, n := span(b)
	return p >= q && p < q+n
}

// TestInternIdentityAcrossCopies checks that an atom's own copy of its
// content (globalASCIIAtom, globalUTF16Atom) leaves one atom per name: the
// name as a Go string, a clone of it, a JS string, a zero-copy substring of
// a large string, through KeyFromGoString and InternKey, in two realms with
// shared intrinsics and one with mutable intrinsics, is one pointer, whose
// content is not the substring's.
func TestInternIdentityAcrossCopies(t *testing.T) {
	run := strconv.Itoa(int(identityRuns.Add(1)))
	realms := []*Realm{
		NewRealmWith(RealmOptions{SharedIntrinsics: true}),
		NewRealmWith(RealmOptions{SharedIntrinsics: true}),
		NewRealmWith(RealmOptions{}),
	}
	for i, r := range realms {
		// Past internCacheAfter names, so the names below go through the
		// realm's caches as well as the process-wide tables.
		for j := range internCacheAfter + 1 {
			r.InternGoString("identity_warm_" + run + "_" + strconv.Itoa(j))
		}
		require.NotNil(t, r.internCacheASCII, "realm %d", i)
	}
	for _, tc := range []struct{ kind, prefix, fill string }{
		{"ascii", "identity_", "Z"},
		{"utf16", "名字_", "字"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			name := tc.prefix + run
			js := FromGoString(name)
			lookup := func() *String {
				if js.kind == strASCII {
					return globalInternASCII.lookup(name)
				}
				return globalInternUTF16.lookup(utf16Key(js.units()))
			}
			require.Nil(t, lookup(), "the name is new to the process")
			big := name + strings.Repeat(tc.fill, 4096)
			bigJS := FromGoString(big)
			sub := bigJS.Substring(0, int(js.n))
			require.True(t, contentIn(sub, bigJS), "the substring shares the large string")

			var atom *String
			for i, r := range realms {
				forms := []struct {
					how string
					got *String
				}{
					// First, so realm 0 makes the atom from the substring.
					{"JS substring", r.Intern(sub)},
					{"Go substring", r.InternGoString(big[:len(name)])},
					{"Go string", r.InternGoString(name)},
					{"strings.Clone", r.InternGoString(strings.Clone(name))},
					{"JS string", r.Intern(FromGoString(name))},
					{"KeyFromGoString", r.KeyFromGoString(name).String()},
					{"InternKey", InternKey(name).String()},
				}
				if atom == nil {
					atom = forms[0].got
				}
				for _, f := range forms {
					require.Same(t, atom, f.got, "realm %d: %s", i, f.how)
				}
				cached := r.internCacheASCII[name]
				if js.kind != strASCII {
					cached = r.internCacheUTF16[utf16Key(js.units())]
				}
				require.Same(t, atom, cached, "realm %d caches the atom", i)
			}
			require.True(t, atom.IsInterned())
			require.Same(t, atom, lookup())
			assert.False(t, contentIn(atom, bigJS), "the atom's content is its own")
		})
	}
}

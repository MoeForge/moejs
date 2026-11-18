package tests

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/require"
)

// A document statistics plugin needs code points, not UTF-16 code units.
// Keep its word-count intermediates in the same request and memory budget.
func TestArrayFromStringDocumentBudget(t *testing.T) {
	mod, err := moejs.Compile("statistics.js", `
export function count(markdown) {
  const words = markdown.trim().split(/\s+/).filter(Boolean).length;
  const characters = Array.from(markdown).length;
  return {words, characters};
}`)
	require.NoError(t, err)
	hook := mustHook(t, mod, "count")
	for _, tc := range []struct {
		name, text string
		words      int
	}{
		{"ASCII", strings.Repeat("a", 1<<20), 1},
		{"prose", strings.Repeat("A useful thought with emphasis.\n", 32768), 5 * 32768},
		{"mixed", strings.Repeat("Text \u4e2d\u6587 \U0001f600.\n", 50000), 3 * 50000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 32 << 20})
			require.NoError(t, rt.Load(mod))
			arg, err := rt.FromGo(tc.text)
			require.NoError(t, err)
			v, err := rt.Call(hook, arg)
			require.NoError(t, err)
			var out struct{ Words, Characters int }
			require.NoError(t, rt.Unmarshal(v, &out))
			require.Equal(t, tc.words, out.Words)
			require.Equal(t, utf8.RuneCountInString(tc.text), out.Characters)
		})
	}
}

func BenchmarkArrayFromString(b *testing.B) {
	mod, err := moejs.Compile("array-from.js", `export function run(s) { return Array.from(s).length; }`)
	if err != nil {
		b.Fatal(err)
	}
	hook, err := mod.Hook("run")
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct{ name, text string }{
		{"small", "Hello world"},
		{"ASCII1MiB", strings.Repeat("a", 1<<20)},
		{"mixed", strings.Repeat("a\u4e2d\U0001f600", 1<<15)},
	} {
		for _, limited := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/limited=%t", tc.name, limited), func(b *testing.B) {
				opts := moejs.Options{}
				if limited {
					// Let the baseline finish so both revisions measure the same work.
					opts.MemoryLimit = 256 << 20
				}
				rt := moejs.NewRuntime(opts)
				if err := rt.Load(mod); err != nil {
					b.Fatal(err)
				}
				want := int64(utf8.RuneCountInString(tc.text))
				b.ReportAllocs()
				b.SetBytes(int64(len(tc.text)))
				for b.Loop() {
					arg, err := rt.FromGo(tc.text)
					if err != nil {
						b.Fatal(err)
					}
					v, err := rt.Call(hook, arg)
					if err != nil || v != moejs.Int(want) {
						b.Fatalf("count = %v, error = %v", v, err)
					}
					rt.ReleaseCallData()
				}
			})
		}
	}
}

func TestArrayFromStringMemoryLimit(t *testing.T) {
	mod, err := moejs.Compile("limit.js", `export function run(s) { return Array.from(s); }`)
	require.NoError(t, err)
	hook := mustHook(t, mod, "run")
	for _, tc := range []struct{ name, text string }{
		{"array storage", strings.Repeat("a", 1<<20)},
		{"BMP strings", strings.Repeat("\u4e2d", 8192)},
		{"surrogate pairs", strings.Repeat("\U0001f600", 8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 256 << 10})
			require.NoError(t, rt.Load(mod))
			arg, err := rt.FromGo(tc.text)
			require.NoError(t, err)
			_, err = rt.Call(hook, arg)
			// In the Unicode cases the array alone fits: its character
			// strings must also count against the budget.
			require.ErrorIs(t, err, moejs.ErrMemoryLimit)
			rt.ClearInterrupt()
			rt.ReleaseCallData()
			arg, err = rt.FromGo("ok")
			require.NoError(t, err)
			v, err := rt.Call(hook, arg)
			require.NoError(t, err)
			var out []string
			require.NoError(t, rt.Unmarshal(v, &out))
			require.Equal(t, []string{"o", "k"}, out)
		})
	}
}

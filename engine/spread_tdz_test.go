package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpreadPairRoomTDZ: {...a, ...b} whose b is a closure's variable read
// with a TDZ check (GetEnvChk, whose extra word comes before the second
// spread) gets the room for both sources' keys at once, as with GetEnv
// (spreadPairSource): it allocates as one spread of the same keys.
func TestSpreadPairRoomTDZ(t *testing.T) {
	const src = `var opts = {}, all = {}, os = [];
for (var i = 0; i < 38; i++) opts["q" + i] = all["q" + i] = i;
all.a0 = all.b0 = all.c0 = 1;
function one(o) { let f = () => (b, {...all}); let b = o; return f(); }
for (var j = 0; j < 6; j++) os.push({q1: -1, ["a" + j]: 1, ["b" + j]: 2, ["c" + j]: 3});
function tdz(o) { let f = () => ({...opts, ...b}); let b = o; return f(); }
for (var j = 1; j < 6; j++) tdz(os[j]);
var p = tdz(os[0]), pk = [p.q1, p.a0, p.c0, p.q37].join();`
	s, err := syntax.ParseScript("tdz.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileScript(s)
	require.NoError(t, err)
	require.True(t, strings.Contains(bytecode.Disassemble(code), "GetEnvChk"), "b is read with a TDZ check")
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		_, err := r.RunScript(code)
		require.NoError(t, err)
		p := jsGlobal(t, r, "p").AsObject()
		assert.Equal(t, 41, len(p.slots))
		assert.Equal(t, cap(slices.Grow([]Value(nil), 41)), cap(p.slots), "shared %v: room for both sources' keys", shared)
		assert.Equal(t, "-1,1,3,37", jsGlobal(t, r, "pk").String())
		if regcheckBuild {
			continue // the checked build's run allocates its register checks, more for more instructions
		}
		arg := []Value{jsGlobal(t, r, "os").AsObject().elems()[0]}
		allocs := func(name string) float64 {
			fn := jsGlobal(t, r, name)
			return testing.AllocsPerRun(50, func() {
				if _, err := r.Call(fn, Undefined(), arg); err != nil {
					t.Fatal(err)
				}
			})
		}
		assert.Equal(t, allocs("one"), allocs("tdz"), "shared %v: the pair allocates as one spread", shared)
	}
}

package engine

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllocICCount: AllocIC reserves the template's own ICCount entries
// and refuses another count. A count of 0 once left the template's caches
// overlapping the next tree's, so that the script's loop summed NaN.
func TestAllocICCount(t *testing.T) {
	s, err := syntax.ParseScript("sum.js", `var o = {p: 1}, s = 0; for (var i = 0; i < 3; i++) s += o.p; s`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileScript(s)
	require.NoError(t, err)
	require.NotZero(t, code.ICCount)

	r := NewRealm()
	for _, count := range []uint32{0, code.ICCount - 1, code.ICCount + 1} {
		_, err := r.AllocIC(code, count)
		assert.EqualError(t, err, fmt.Sprintf("engine: AllocIC of %d entries for a template of %d", count, code.ICCount))
	}
	assert.Empty(t, r.ICSlots(), "a refused count reserves nothing")
	runScriptsIn(t, r, `var x = {q: "a", p: "b"}; x.q + x.p + x.q`)
	v, err := r.RunScript(code)
	require.NoError(t, err)
	assert.Equal(t, "3", v.String())

	r = NewRealm()
	base, err := r.AllocIC(code, code.ICCount)
	require.NoError(t, err)
	runScriptsIn(t, r, `var x = {q: "a", p: "b"}; x.q + x.p + x.q`)
	v, err = r.RunScript(code)
	require.NoError(t, err)
	assert.Equal(t, "3", v.String())
	again, err := r.AllocIC(code, code.ICCount)
	require.NoError(t, err)
	assert.Equal(t, base, again)
}

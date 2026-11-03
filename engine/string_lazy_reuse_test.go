package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONParseSharesShortLazyLiterals(t *testing.T) {
	for _, literal := range []string{`"日本語"`, `"\u4e2d"`, `"a\n"`, `"\ud800"`, `"\ud83d\ude00"`} {
		for _, limit := range []int64{0, 1 << 20} {
			r := NewRealmWith(RealmOptions{MemoryLimit: limit})
			v := jsonParseGo(t, r, `[`+literal+`,`+literal+`]`)
			e := v.AsObject().Elements()
			a, b := e[0].AsString(), e[1].AsString()
			require.Same(t, a, b, "identical short raw literals reuse storage: %s, limit=%d", literal, limit)
			require.Equal(t, a.UTF16(), b.UTF16())
		}
	}
}

func TestJSONShortLazyCacheKeepsContentAndScope(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
	a, b := `\u4e2d`, `\u5e2d`
	require.Equal(t, strSlot(a), strSlot(b), "exercise an actual cache collision")
	v := jsonParseGo(t, r, `["`+a+`","`+b+`","中","\ud800","\ud83d\ude00"]`)
	e := v.AsObject().Elements()
	for i, want := range [][]uint16{{0x4e2d}, {0x5e2d}, {0x4e2d}, {0xd800}, {0xd83d, 0xde00}} {
		require.Equal(t, want, e[i].AsString().UTF16(), "element %d", i)
	}

	// Interning a key may already have decoded the cached literal. A hit
	// must preserve its published-cache facts.
	v = jsonParseGo(t, r, `{"日本語":0,"value":"日本語"}`)
	x, err := v.AsObject().Get(r, key(r, "value"), v)
	require.NoError(t, err)
	require.Equal(t, "日本語", x.AsString().GoString())
	require.NotNil(t, cacheOf(x.AsString()))

	old := jsonParseGo(t, r, `["日本語","日本語"]`).AsObject().Elements()[0].AsString()
	r.SetMemoryLimit(0)
	r.SetMemoryLimit(1 << 20)
	fresh := jsonParseGo(t, r, `["日本語","日本語"]`).AsObject().Elements()[0].AsString()
	require.NotSame(t, old, fresh, "the short cache belongs to one parse")
}

func TestJSONShortLazyCacheBoundAndReviver(t *testing.T) {
	r := NewRealm()
	v, err := r.JSONParse(FromUTF16([]uint16{'[', '"', 0xd800, '"', ',', '"', 0xd800, '"', ']'}))
	require.NoError(t, err)
	w := v.AsObject().Elements()
	require.Same(t, w[0].AsString(), w[1].AsString())
	require.Equal(t, []uint16{0xd800}, w[0].AsString().UTF16())
	for _, n := range []int{jsonShortMax, jsonShortMax + 1} {
		raw := `\n` + strings.Repeat("x", n-2)
		v := jsonParseGo(t, r, `["`+raw+`","`+raw+`"]`)
		e := v.AsObject().Elements()
		if n == jsonShortMax {
			require.Same(t, e[0].AsString(), e[1].AsString())
		} else {
			require.NotSame(t, e[0].AsString(), e[1].AsString())
		}
	}
	v, err = runScriptValue(t, r, `let visits = 0;
const values = JSON.parse('["日本語","日本語"]', (k, v) => {
  if (typeof v === "string") { visits++; return k + v; }
  return v;
}); visits + ":" + values.join(",");`)
	require.NoError(t, err)
	require.Equal(t, "2:0日本語,1日本語", v.AsString().GoString())
}

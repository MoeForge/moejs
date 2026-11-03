package moejs_test

import (
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTimeZoneOption checks that Options.TimeZone is the local zone of Date,
// with shared and mutable intrinsics: the local getters and toString use it,
// Dates export as time.Time in it, and a Date made from a time.Time keeps
// its instant. The clock is frozen through the realm's SetNow; nil restores
// time.Now. Without the option the zone is time.Local.
func TestTimeZoneOption(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	mod, err := moejs.Compile("date.js", `
export function now() { return Date.now(); }
export function show() { const d = new Date(); return [String(d), d.getHours(), d.getTimezoneOffset()]; }
export function local() { return new Date(2024, 0, 2, 3, 4, 5); }
export function iso(d) { return d.toISOString(); }
`)
	require.NoError(t, err)
	frozen := time.Date(2024, 7, 4, 12, 0, 0, 0, time.UTC)
	for _, mutable := range []bool{false, true} {
		rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable, TimeZone: ny})
		require.NoError(t, rt.Load(mod))
		rt.Realm().SetNow(func() time.Time { return frozen })
		call := func(export string, args ...moejs.Value) any {
			t.Helper()
			v, err := rt.Call(mustHook(t, mod, export), args...)
			require.NoError(t, err)
			out, err := rt.ToGo(v)
			require.NoError(t, err)
			return out
		}
		assert.Equal(t, frozen.UnixMilli(), call("now"))
		assert.Equal(t, []any{"Thu Jul 04 2024 08:00:00 GMT-0400 (EDT)", int64(8), int64(240)}, call("show"))
		d, ok := call("local").(time.Time)
		require.True(t, ok)
		assert.Same(t, ny, d.Location())
		assert.Equal(t, time.Date(2024, 1, 2, 3, 4, 5, 0, ny), d)
		date := engine.ObjectValue(rt.Realm().NewDate(time.Date(2024, 1, 2, 3, 4, 5, 6_999_999, time.UTC)))
		assert.Equal(t, "2024-01-02T03:04:05.006Z", call("iso", date))
		rt.Realm().SetNow(nil)
		assert.InDelta(t, float64(time.Now().UnixMilli()), float64(call("now").(int64)), 60_000)
	}
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	v, err := rt.Call(mustHook(t, mod, "local"))
	require.NoError(t, err)
	d, err := rt.ToGo(v)
	require.NoError(t, err)
	assert.Same(t, time.Local, d.(time.Time).Location())
}

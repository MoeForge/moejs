package moejs_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/require"
)

type descriptor struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    any               `json:"body"`
	When    json.RawMessage   `json:"when"`
}

// TestUnmarshal compares Runtime.Unmarshal with json.Unmarshal of
// AppendJSON's text for a hook's result: plain (no text), with an argument
// forwarded untouched, with a toJSON and a RawMessage (the text), and a
// value AppendJSON rejects.
func TestUnmarshal(t *testing.T) {
	mod, err := moejs.Compile("u.js", `
export function build(req) {
  return { url: "https://x/" + req.model, method: "POST", headers: { "Content-Type": "application/json" }, body: req.payload, when: req.when };
}
export function dated() { return { url: "d", body: { at: new Date(0) } }; }
export function big() { return { body: 1n }; }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	arg, err := rt.FromGo(map[string]any{"model": "m", "payload": map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 100)}}, "n": 2}})
	require.NoError(t, err)
	for _, c := range []struct {
		hook string
		args []moejs.Value
	}{
		{"build", []moejs.Value{arg}},
		{"dated", nil},
		{"big", nil},
	} {
		res, err := rt.Call(mustHook(t, mod, c.hook), c.args...)
		require.NoError(t, err)
		var got, want descriptor
		gerr := rt.Unmarshal(res, &got)
		data, werr := rt.AppendJSON(nil, res)
		if werr == nil {
			werr = json.Unmarshal(data, &want)
		}
		if werr != nil {
			require.EqualError(t, gerr, werr.Error(), c.hook)
			continue
		}
		require.NoError(t, gerr, c.hook)
		require.Equal(t, want, got, c.hook)
	}
}

// TestUnmarshalDAG checks a result that repeats one object: 2^20 paths
// whose text (14 MiB) is under the length limit go into a struct that
// discards them, as the round trip has it.
func TestUnmarshalDAG(t *testing.T) {
	if testing.Short() {
		t.Skip("walks 2^21 nodes")
	}
	mod, err := moejs.Compile("d.js", `export function dag(n) {
  let v = "x";
  for (let i = 0; i < n; i++) v = {a: v, b: v};
  return {Known: 1, big: v};
}`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	res, err := rt.Call(mustHook(t, mod, "dag"), moejs.Int(20))
	require.NoError(t, err)
	var k struct{ Known int }
	require.NoError(t, rt.Unmarshal(res, &k))
	require.Equal(t, 1, k.Known)
}

// TestUnmarshalInterruptAsAppendJSON checks an interrupt pending at the
// call: an array of 3,000 goes into a []int, as AppendJSON writes it, each
// of Unmarshal's two walks counting under 4096 values; one of 5,000 fails
// in both, the target untouched. A string of 5,000 characters fails
// AppendJSON, which observes the interrupt as it escapes the string, and
// goes into the target with Unmarshal, which copies no string.
func TestUnmarshalInterruptAsAppendJSON(t *testing.T) {
	mod, err := moejs.Compile("i.js", `
export function list(n) { return Array.from({length: n}, (_, i) => i); }
export function str() { return {S: "x".repeat(5000)}; }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	small, err := rt.Call(mustHook(t, mod, "list"), moejs.Int(3000))
	require.NoError(t, err)
	large, err := rt.Call(mustHook(t, mod, "list"), moejs.Int(5000))
	require.NoError(t, err)
	str, err := rt.Call(mustHook(t, mod, "str"))
	require.NoError(t, err)
	rt.Interrupt("pending")
	defer rt.ClearInterrupt()

	_, err = rt.AppendJSON(nil, small)
	require.NoError(t, err)
	var ints []int
	require.NoError(t, rt.Unmarshal(small, &ints))
	require.Len(t, ints, 3000)

	var ie *moejs.InterruptedError
	_, err = rt.AppendJSON(nil, large)
	require.ErrorAs(t, err, &ie)
	ints = []int{7}
	require.ErrorAs(t, rt.Unmarshal(large, &ints), &ie)
	require.Equal(t, []int{7}, ints)

	_, err = rt.AppendJSON(nil, str)
	require.ErrorAs(t, err, &ie)
	var s struct{ S string }
	require.NoError(t, rt.Unmarshal(str, &s))
	require.Len(t, s.S, 5000)
}

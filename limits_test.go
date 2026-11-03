package moejs_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const limitsSource = `
export function big(n) {
	return { taskId: "t", list: Array.from({length: n}, (_, i) => ({i, s: "x".repeat(10)})), state: {n} };
}
export function viaToJSON(n) {
	return { taskId: "t", list: { toJSON() { return Array.from({length: n}, (_, i) => ({i})); } } };
}
export function viaGetter(n) {
	return { taskId: "t", get list() { return Array.from({length: n}, (_, i) => ({i})); } };
}
export function cyclic() { const o = {taskId: "t"}; o.self = o; return o; }
export function echo(x) { return x; }
`

type limitsResult struct {
	TaskID string           `json:"taskId"`
	List   []map[string]any `json:"list"`
	State  json.RawMessage  `json:"state"`
}

// TestDecodeLimitsBeforeWrite: a decode past a limit returns ErrTooLarge and
// leaves a struct, a slice and a map target as they were, whether the value
// is counted before the decode or on the round trip's text.
func TestDecodeLimitsBeforeWrite(t *testing.T) {
	mod, err := moejs.Compile("limits.js", limitsSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	call := func(name string, n int) moejs.Value {
		t.Helper()
		v, err := rt.Call(mustHook(t, mod, name), moejs.Int(int64(n)))
		require.NoError(t, err)
		return v
	}
	targets := func() []any {
		return []any{
			&limitsResult{TaskID: "old", List: []map[string]any{{"keep": 1.0}}, State: json.RawMessage(`"old"`)},
			&[]any{"old", 1.0},
			&map[string]any{"old": true},
		}
	}
	opts := moejs.DecodeOptions{MaxBytes: 1 << 10}
	for _, hook := range []string{"big", "viaToJSON", "viaGetter"} {
		v := call(hook, 1000)
		for _, decode := range []struct {
			name string
			run  func(target any) error
		}{
			{"UnmarshalWith", func(target any) error { return rt.UnmarshalWith(v, target, opts) }},
			{"ToGoIntoWith", func(target any) error { return rt.ToGoIntoWith(v, target, opts) }},
		} {
			for i, target := range targets() {
				err := decode.run(target)
				if hook == "viaToJSON" && decode.name == "ToGoIntoWith" {
					// ToGo calls no toJSON: the object's text is small.
					continue
				}
				assert.ErrorIs(t, err, moejs.ErrTooLarge, "%s %s %T", hook, decode.name, target)
				assert.ErrorContains(t, err, "more than 1024 bytes of JSON")
				assert.Equal(t, targets()[i], target, "%s %s left the target as it was", hook, decode.name)
			}
		}
		g, err := rt.ToGoWith(v, opts)
		if hook != "viaToJSON" {
			assert.ErrorIs(t, err, moejs.ErrTooLarge, hook)
			assert.Nil(t, g)
		}
	}

	// Within the limits the result is the unbounded decode's.
	v := call("big", 3)
	for _, opts := range []moejs.DecodeOptions{{}, {MaxBytes: 1 << 20, MaxNodes: 1 << 10}} {
		var a, b limitsResult
		require.NoError(t, rt.UnmarshalWith(v, &a, opts))
		require.NoError(t, rt.Unmarshal(v, &b))
		assert.Equal(t, b, a)
		require.NoError(t, rt.ToGoIntoWith(v, &a, opts))
		require.NoError(t, rt.ToGoInto(v, &b))
		assert.Equal(t, b, a)
		ga, err := rt.ToGoWith(v, opts)
		require.NoError(t, err)
		gb, err := rt.ToGo(v)
		require.NoError(t, err)
		assert.Equal(t, gb, ga)
	}

	// A cycle passes every limit.
	v = call("cyclic", 0)
	var m map[string]any
	for _, err := range []error{rt.UnmarshalWith(v, &m, opts), rt.ToGoIntoWith(v, &m, opts)} {
		assert.ErrorIs(t, err, moejs.ErrTooLarge)
	}
	_, err = rt.ToGoWith(v, opts)
	assert.ErrorIs(t, err, moejs.ErrTooLarge)
	assert.Nil(t, m)
}

// TestDecodeLimitsDifferential: over the Unmarshal and ToGoInto corpora, a
// bounded decode fails with ErrTooLarge exactly when the round trip's text
// passes the limit, by bytes or by values, and otherwise gives the
// unbounded decode's result, whichever path the value takes.
func TestDecodeLimitsDifferential(t *testing.T) {
	exprs := append(append([]string(nil), umExprs...), tgExprs...)
	var sb strings.Builder
	sb.WriteString("export function v(i, HOST, BAD, ODD) { return [\n")
	for _, e := range exprs {
		sb.WriteString("() => (" + e + "),\n")
	}
	sb.WriteString("][i](); }\n")
	mod, err := moejs.Compile("lim.js", sb.String())
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	hook := mustHook(t, mod, "v")
	from := func(g any) moejs.Value {
		v, err := rt.FromGo(g)
		require.NoError(t, err)
		return v
	}
	value := func(i int) moejs.Value {
		v, err := rt.Call(hook, umNum(t, rt, i), from(map[string]any{
			"model": "m<>", "n": 2, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "tags": []string{"a"},
			"hdr": map[string]string{"k": "v"}, "big": int64(1<<53 + 1), "raw": json.RawMessage(`{"z": [1, 2.50]}`),
			"rawlist": json.RawMessage(`[1, {"x": "é"}]`), "rawprim": json.RawMessage(`"s"`), "rawnull": json.RawMessage(nil),
		}), from(map[string]any{"n": 1}), from(map[string]any{"num": json.Number("1.50")}))
		require.NoError(t, err)
		return v
	}
	checked := 0
	for i, e := range exprs {
		for _, togo := range []bool{false, true} {
			var text []byte
			if togo {
				g, err := rt.ToGo(value(i))
				if err != nil {
					continue
				}
				if text, err = json.Marshal(g); err != nil {
					continue
				}
			} else {
				var err error
				if text, err = rt.AppendJSON(nil, value(i)); err != nil {
					continue
				}
			}
			nodes := limitsCount(t, text)
			for _, c := range []struct {
				opts moejs.DecodeOptions
				over bool
			}{
				{moejs.DecodeOptions{MaxBytes: len(text)}, false},
				{moejs.DecodeOptions{MaxBytes: len(text) - 1}, len(text) > 1},
				{moejs.DecodeOptions{MaxNodes: nodes}, false},
				{moejs.DecodeOptions{MaxNodes: nodes - 1}, nodes > 1},
			} {
				for _, tg := range umTargets() {
					if tg.name == "not-pointer" || tg.name == "nil-pointer" || tg.name == "nil-any" {
						continue
					}
					ta, tb := tg.mk()
					var errA, errB error
					if togo {
						errA = rt.ToGoIntoWith(value(i), ta, c.opts)
						errB = rt.ToGoInto(value(i), tb)
					} else {
						errA = rt.UnmarshalWith(value(i), ta, c.opts)
						errB = rt.Unmarshal(value(i), tb)
					}
					checked++
					if c.over {
						fresh, _ := tg.mk()
						if !assert.ErrorIs(t, errA, moejs.ErrTooLarge, "togo=%v %s into %s %+v: text %s", togo, e, tg.name, c.opts, text) {
							continue
						}
						assert.Equal(t, tgDump(fresh), tgDump(ta), "togo=%v %s into %s: target written", togo, e, tg.name)
						continue
					}
					assert.Equal(t, umErr(errB), umErr(errA), "togo=%v %s into %s %+v", togo, e, tg.name, c.opts)
					assert.Equal(t, tgDump(tb), tgDump(ta), "togo=%v %s into %s %+v", togo, e, tg.name, c.opts)
				}
				if togo {
					g, err := rt.ToGoWith(value(i), c.opts)
					if c.over {
						assert.ErrorIs(t, err, moejs.ErrTooLarge, "ToGoWith %s %+v", e, c.opts)
					} else {
						require.NoError(t, err, e)
						want, _ := rt.ToGo(value(i))
						a, _ := json.Marshal(g)
						b, _ := json.Marshal(want)
						assert.Equal(t, string(b), string(a), e)
					}
				}
			}
		}
		rt.ReleaseCallData()
	}
	t.Logf("%d bounded decodes checked", checked)
}

// limitsCount counts the values of a JSON text: its tokens but the keys
// and the closing delimiters.
func limitsCount(t *testing.T, text []byte) int {
	dec := json.NewDecoder(bytes.NewReader(text))
	var objects []bool // the open containers, true for an object
	key := false       // the next token of the object is a key
	n := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			return n
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			objects = objects[:len(objects)-1]
			key = len(objects) > 0 && objects[len(objects)-1]
			continue
		}
		if key {
			key = false
			continue
		}
		n++
		if d, ok := tok.(json.Delim); ok {
			objects = append(objects, d == '{')
			key = d == '{'
			continue
		}
		key = len(objects) > 0 && objects[len(objects)-1]
	}
}

// ExampleRuntime_ToGoIntoWith bounds a hook's result before anything is
// decoded: a result whose JSON would pass 1 MiB is rejected with
// ErrTooLarge, and the target stays as it was.
func ExampleRuntime_ToGoIntoWith() {
	mod, _ := moejs.Compile("hook.js", `export function submit() {
		return { taskId: "t1", state: { blob: "x".repeat(2 << 20) } };
	}`)
	hook, _ := mod.Hook("submit")
	rt := moejs.NewRuntime(moejs.Options{})
	_ = rt.Load(mod)

	res, _ := rt.Call(hook)
	var out struct {
		TaskID string          `json:"taskId"`
		State  json.RawMessage `json:"state"`
	}
	err := rt.ToGoIntoWith(res, &out, moejs.DecodeOptions{MaxBytes: 1 << 20})
	fmt.Println(errors.Is(err, moejs.ErrTooLarge), out.TaskID == "")
	fmt.Println(err)
	// Output:
	// true true
	// moejs: decoded value too large: more than 1048576 bytes of JSON
}

// TestNestingTooDeep: JSON and exports nested past 10,000 arrays and
// objects fail with a RangeError that says so and wraps ErrTooDeep, from
// the host APIs and in JavaScript.
func TestNestingTooDeep(t *testing.T) {
	mod, err := moejs.Compile("deep.js", `
export function deep(n) { let v = []; for (let i = 1; i < n; i++) v = [v]; return v; }
function caught(f) { try { f(); return "ok"; } catch (e) { return e.name + ": " + e.message; } }
export function parse(s) { return caught(() => JSON.parse(s)); }
export function revive(s) { return caught(() => JSON.parse(s, (k, v) => v)); }
export function stringify(n) { const v = deep(n); return caught(() => JSON.stringify(v)); }
`)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	const msg = "RangeError: JSON nesting is too deep (more than 10000 levels)"
	text := strings.Repeat("[", 10001) + strings.Repeat("]", 10001)
	call := func(name string, arg moejs.Value) moejs.Value {
		t.Helper()
		v, err := rt.Call(mustHook(t, mod, name), arg)
		require.NoError(t, err)
		return v
	}

	_, err = rt.ParseJSON([]byte(text))
	assert.ErrorIs(t, err, moejs.ErrTooDeep)
	assert.ErrorContains(t, err, msg)
	_, err = rt.ParseJSONString(text)
	assert.ErrorIs(t, err, moejs.ErrTooDeep)
	assert.ErrorContains(t, err, msg)
	limit, err := rt.ParseJSONString(text[1 : len(text)-1])
	require.NoError(t, err, "10,000 levels parse")
	_, err = rt.AppendJSON(nil, limit)
	require.NoError(t, err, "and serialize")

	deep := call("deep", moejs.Int(10001))
	_, err = rt.AppendJSON(nil, deep)
	assert.ErrorIs(t, err, moejs.ErrTooDeep)
	assert.ErrorContains(t, err, msg)
	_, err = rt.ToGo(deep)
	assert.ErrorIs(t, err, moejs.ErrTooDeep)
	assert.ErrorContains(t, err, "RangeError: Nesting is too deep to export to Go (more than 10000 levels)")

	assert.Equal(t, msg, call("parse", moejs.String(text)).String())
	assert.Equal(t, msg, call("revive", moejs.String(text)).String())
	assert.Equal(t, msg, call("stringify", moejs.Int(10001)).String())
	assert.Equal(t, "ok", call("stringify", moejs.Int(10000)).String())
}

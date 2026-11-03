package moejs_test

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rawJSONSource = `
export function poll(ctx) {
	return { status: ctx.task.status, state: ctx.state, n: ctx.list.length };
}
export function touch(ctx) {
	return { status: ctx.task.status, step: ctx.state.step + 1, body: ctx.body.model };
}
`

// TestRawJSONArguments: json.RawMessage arguments arrive as JSON.parse of
// their text; one the hook does not read is never parsed, and one it passes
// on untouched goes into a RawMessage field as json.Marshal's compact text
// of it.
func TestRawJSONArguments(t *testing.T) {
	mod, err := moejs.Compile("raw.js", rawJSONSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))

	body := json.RawMessage(`{"model": "m", "image": "aGVsbG8="}`)
	ctx := map[string]any{
		"task":  json.RawMessage(`{"status": "running", "id": 7}`),
		"state": json.RawMessage(` {"step": 1, "ids": [1, 2]} `),
		"body":  body,
		"list":  json.RawMessage(`[1, 2, 3]`),
	}
	arg, err := rt.FromGo(ctx)
	require.NoError(t, err)
	res, err := rt.Call(mustHook(t, mod, "poll"), arg)
	require.NoError(t, err)
	var out struct {
		Status string          `json:"status"`
		State  json.RawMessage `json:"state"`
		N      int             `json:"n"`
	}
	require.NoError(t, rt.ToGoInto(res, &out))
	assert.Equal(t, "running", out.Status)
	assert.Equal(t, `{"step":1,"ids":[1,2]}`, string(out.State))
	assert.Equal(t, 3, out.N)
	require.NoError(t, rt.Unmarshal(res, &out))
	assert.Equal(t, `{"step":1,"ids":[1,2]}`, string(out.State))

	// The argument the hook read is a snapshot export now; the members it
	// never read are the host's RawMessages.
	g, err := rt.ToGo(arg)
	require.NoError(t, err)
	m := g.(map[string]any)
	assert.Equal(t, body, m["body"])
	assert.Equal(t, map[string]any{"status": "running", "id": int64(7)}, m["task"])
	rt.ReleaseCallData()

	arg, err = rt.FromGo(ctx)
	require.NoError(t, err)
	res, err = rt.Call(mustHook(t, mod, "touch"), arg)
	require.NoError(t, err)
	assert.Equal(t, `{"status":"running","step":2,"body":"m"}`, string(mustAppendJSON(t, rt, res)))
	rt.ReleaseCallData()

	// Invalid text is the parser's SyntaxError where the member is read.
	arg, err = rt.FromGo(map[string]any{"task": json.RawMessage(`{"status": }`), "state": nil, "list": json.RawMessage(`[]`)})
	require.NoError(t, err)
	_, err = rt.Call(mustHook(t, mod, "poll"), arg)
	var exc *moejs.Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "SyntaxError", exc.Name())
	assert.Equal(t, "Unexpected token } in JSON at position 11", exc.Message())
	rt.ReleaseCallData()
	_, err = rt.FromGo(json.RawMessage(`{"status": }`))
	assert.EqualError(t, err, "SyntaxError: Unexpected token } in JSON at position 11")
}

func mustAppendJSON(t *testing.T, rt *moejs.Runtime, v moejs.Value) []byte {
	t.Helper()
	b, err := rt.AppendJSON(nil, v)
	require.NoError(t, err)
	return b
}

// TestRawJSONReleased: after ReleaseCallData a pooled runtime keeps no part
// of a RawMessage argument, read or not.
func TestRawJSONReleased(t *testing.T) {
	mod, err := moejs.Compile("raw.js", rawJSONSource)
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	hook := mustHook(t, mod, "poll")
	var refs []weak.Pointer[[1 << 16]byte]
	for _, read := range []bool{false, true} {
		buf := new([1 << 16]byte)
		text := append(buf[:0], `{"step": 1, "pad": "`...)
		for len(text) < len(buf)-2 {
			text = append(text, 'x')
		}
		text = append(text, `"}`...)
		refs = append(refs, weak.Make(buf))
		ctx := map[string]any{"task": json.RawMessage(`{"status": "s"}`), "state": json.RawMessage(text), "list": json.RawMessage(`[1]`)}
		arg, err := rt.FromGo(ctx)
		require.NoError(t, err)
		res, err := rt.Call(hook, arg)
		require.NoError(t, err)
		if read {
			_, err = rt.Get(res, "state")
			require.NoError(t, err)
			b := mustAppendJSON(t, rt, res)
			assert.Contains(t, string(b), `"step":1`)
		}
		rt.ReleaseCallData()
	}
	runtime.GC()
	runtime.GC()
	for i, p := range refs {
		assert.Nil(t, p.Value(), "argument %d still reachable after ReleaseCallData", i)
	}
	runtime.KeepAlive(rt)
}

// ExampleRuntime_FromGo_rawMessage passes stored JSON text: the hook reads
// it as an object, and the engine parses it only when the hook reads it.
func ExampleRuntime_FromGo_rawMessage() {
	mod, _ := moejs.Compile("hook.js", `export function step(ctx) { return ctx.state.step + 1; }`)
	hook, _ := mod.Hook("step")
	rt := moejs.NewRuntime(moejs.Options{})
	_ = rt.Load(mod)

	stored := json.RawMessage(`{"step": 2, "history": [1, 2]}`)
	arg, _ := rt.FromGo(map[string]any{"state": stored})
	res, _ := rt.Call(hook, arg)
	fmt.Println(res.String())
	rt.ReleaseCallData()
	// Output: 3
}

// ExampleRuntime_ToGoInto_rawMessage keeps a subtree of a hook's result as
// JSON text: the json.RawMessage field receives what json.Marshal writes for
// it, and the rest of the struct is filled without a round trip.
func ExampleRuntime_ToGoInto_rawMessage() {
	mod, _ := moejs.Compile("hook.js", `export function submit() {
		return { taskId: "t1", state: { attempt: 1, ids: ["b", "a"] } };
	}`)
	hook, _ := mod.Hook("submit")
	rt := moejs.NewRuntime(moejs.Options{})
	_ = rt.Load(mod)

	res, _ := rt.Call(hook)
	var out struct {
		TaskID string          `json:"taskId"`
		State  json.RawMessage `json:"state"`
	}
	if err := rt.ToGoInto(res, &out); err != nil {
		panic(err)
	}
	fmt.Println(out.TaskID, string(out.State))
	// Output: t1 {"attempt":1,"ids":["b","a"]}
}

const rawReuseSource = `
export function keep(ctx) { globalThis.saved = ctx.state; return ctx.body.model; }
export function readSaved() { return JSON.stringify(globalThis.saved); }
export function pass(ctx) { return { state: ctx.state }; }
`

// TestRawJSONBufferReused: a pooled host reuses its buffer for the next
// request after ReleaseCallData; a RawMessage the module kept unread still
// holds the first request's text, whatever the buffer holds now.
func TestRawJSONBufferReused(t *testing.T) {
	mod, err := moejs.Compile("reuse.js", rawReuseSource)
	require.NoError(t, err)
	for _, next := range []string{`{"step": 7, "tenant": "B"}`, `{"step": }`, `[1, 2, 3]`} {
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.Load(mod))
		buf := make([]byte, 0, 256)
		buf = append(buf, `{"step": 1, "tenant": "A"}`...)
		arg, err := rt.FromGo(map[string]any{"body": json.RawMessage(`{"model": "m"}`), "state": json.RawMessage(buf)})
		require.NoError(t, err)
		_, err = rt.Call(mustHook(t, mod, "keep"), arg)
		require.NoError(t, err)
		rt.ReleaseCallData()
		buf = append(buf[:0], next...) // the next request's body, in the same buffer
		res, err := rt.Call(mustHook(t, mod, "readSaved"))
		require.NoError(t, err)
		assert.Equal(t, `{"step":1,"tenant":"A"}`, res.String(), "after the buffer held %s", next)
	}
}

// TestRawJSONChangedBeforeRead: bytes changed between FromGo and the read
// (here between the hook's return and the decode) change nothing: every
// decode gives the text FromGo converted, and none panics.
func TestRawJSONChangedBeforeRead(t *testing.T) {
	mod, err := moejs.Compile("reuse.js", rawReuseSource)
	require.NoError(t, err)
	for _, mode := range []string{"Unmarshal", "ToGoInto", "UnmarshalWith", "ToGoIntoWith", "AppendJSON", "ToGo"} {
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.Load(mod))
		buf := append(make([]byte, 0, 64), `[10, 2]`...)
		arg, err := rt.FromGo(json.RawMessage(buf))
		require.NoError(t, err)
		state := map[string]any{"body": json.RawMessage(`{"model": "m"}`), "state": arg}
		sv, err := rt.FromGo(state)
		require.NoError(t, err)
		res, err := rt.Call(mustHook(t, mod, "pass"), sv)
		require.NoError(t, err)
		buf = append(buf[:0], `[1,2,3]`...) // same kind, another length
		var out struct {
			State json.RawMessage `json:"state"`
		}
		want := `[10,2]`
		switch mode {
		case "Unmarshal":
			err = rt.Unmarshal(res, &out)
		case "ToGoInto":
			err = rt.ToGoInto(res, &out)
		case "UnmarshalWith":
			err = rt.UnmarshalWith(res, &out, moejs.DecodeOptions{MaxBytes: 1 << 20})
		case "ToGoIntoWith":
			err = rt.ToGoIntoWith(res, &out, moejs.DecodeOptions{MaxBytes: 1 << 20})
		case "AppendJSON":
			var b []byte
			b, err = rt.AppendJSON(nil, res)
			out.State = json.RawMessage(strings.TrimSuffix(strings.TrimPrefix(string(b), `{"state":`), `}`))
		case "ToGo":
			var g any
			g, err = rt.ToGo(res)
			raw := g.(map[string]any)["state"].(json.RawMessage)
			assert.NotSame(t, &buf[0], &raw[0], "a copy of the text, not the host's buffer")
			out.State = raw
			want = `[10, 2]` // the text as FromGo got it
		}
		require.NoError(t, err, mode)
		assert.Equal(t, want, string(out.State), mode)
	}
}

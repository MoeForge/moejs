package moejs_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type userSetting map[string]any

type taskView struct {
	ID       int64             `json:"id"`
	Status   string            `json:"status"`
	Setting  userSetting       `json:"setting"`
	Headers  map[string]string `json:"headers,omitempty"`
	internal string
}

// TestFromGoProbeTypes is the probe of new-api's argument types: a struct,
// map[string]int, []map[string]string and a named map, at the top level and
// inside a map, all arrive in JavaScript as JSON.parse of json.Marshal's
// text gives them.
func TestFromGoProbeTypes(t *testing.T) {
	mod, err := moejs.Compile("probe.js", "export function echo(x) { return JSON.stringify(x); }")
	require.NoError(t, err)
	hook := mustHook(t, mod, "echo")
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))

	task := taskView{ID: 3, Status: "running", Setting: userSetting{"lang": "en"}, internal: "x"}
	for _, v := range []any{
		task,
		&task,
		map[string]int{"b": 2, "a": 1},
		[]map[string]string{{"role": "user", "content": "hi"}, nil},
		userSetting{"theme": "dark", "n": 2.0},
		map[string]any{"task": task, "counts": map[string]int{"x": 1}, "files": []map[string]string{{"name": "a.png"}}, "setting": userSetting{"k": "v"}},
	} {
		arg, err := rt.FromGo(v)
		require.NoError(t, err, "%T", v)
		res, err := rt.Call(hook, arg)
		require.NoError(t, err, "%T", v)
		want, err := json.Marshal(v)
		require.NoError(t, err)
		assert.JSONEq(t, string(want), res.String(), "%T", v)
		rt.ReleaseCallData()
	}

	_, err = rt.FromGo(struct{ C chan int }{})
	assert.ErrorContains(t, err, "unsupported type: chan int")
}

// ExampleRuntime_FromGo_struct passes a struct and a named map: the struct
// arrives as JSON.parse of its json.Marshal text, the named map as its
// underlying map[string]any.
func ExampleRuntime_FromGo_struct() {
	mod, _ := moejs.Compile("hook.js", `export function describe(task) {
		return task.status + " " + task.setting.lang + " " + Object.keys(task);
	}`)
	hook, _ := mod.Hook("describe")
	rt := moejs.NewRuntime(moejs.Options{})
	_ = rt.Load(mod)

	arg, err := rt.FromGo(taskView{ID: 3, Status: "running", Setting: userSetting{"lang": "en"}})
	if err != nil {
		panic(err)
	}
	res, _ := rt.Call(hook, arg)
	fmt.Println(res.String())
	// Output: running en id,status,setting
}

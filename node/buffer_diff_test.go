package node_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// TestBufferDiff runs the cases of testdata/bufprobe.mjs, encodings and
// their round trips, the methods and their errors, in moejs and compares
// them with Node.js 24's (testdata/buf_node.json, which
// testdata/bufgen.mjs writes).
func TestBufferDiff(t *testing.T) {
	src, err := os.ReadFile("testdata/bufprobe.mjs")
	require.NoError(t, err)
	probe, err := moejs.Compile("bufprobe.mjs", string(src))
	require.NoError(t, err)
	main, err := moejs.Compile("main.mjs", `import { Buffer } from "node:buffer";
import { probeBuffer } from "./bufprobe.mjs";
export function main() { return JSON.stringify(probeBuffer(Buffer)); }`)
	require.NoError(t, err)
	linked, err := moejs.Link(main, node.Resolver(func(_ moejs.Referrer, spec string) (*moejs.Module, error) {
		if spec == "./bufprobe.mjs" {
			return probe, nil
		}
		return nil, os.ErrNotExist
	}))
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	api, err := node.InstallLoop(loop, node.Options{})
	require.NoError(t, err)
	defer api.Close()
	require.NoError(t, rt.Load(linked))
	hook, err := linked.Hook("main")
	require.NoError(t, err)
	res, err := rt.Call(hook)
	require.NoError(t, err)
	var got, want map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.String()), &got))
	raw, err := os.ReadFile("testdata/buf_node.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &want))
	// moejs's largest ArrayBuffer is 2 GiB - 1 bytes, Node's 2 ** 53 - 1:
	// the message of a bad size names the bound.
	for _, name := range []string{"alloc negative"} {
		require.Contains(t, got[name].(map[string]any)["err"].(map[string]any)["message"], "<= 2147483647")
		delete(got, name)
		delete(want, name)
	}
	compareCases(t, want, got, false)
}

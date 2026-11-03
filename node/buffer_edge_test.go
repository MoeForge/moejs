package node_test

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// TestBufferEdgeDiff runs testdata/bufedge.mjs in moejs and compares
// with Node.js 24's results (testdata/bufedge_node.json).
func TestBufferEdgeDiff(t *testing.T) {
	src, err := os.ReadFile("testdata/bufedge.mjs")
	require.NoError(t, err)
	probe, err := moejs.Compile("bufedge.mjs", string(src))
	require.NoError(t, err)
	main, err := moejs.Compile("main.mjs", `import * as nb from "node:buffer";
import { edgeBuffer } from "./bufedge.mjs";
export function main() { return JSON.stringify(edgeBuffer(nb.Buffer, nb, { skipPanics: true })); }`)
	require.NoError(t, err)
	linked, err := moejs.Link(main, node.Resolver(func(_ moejs.Referrer, spec string) (*moejs.Module, error) {
		if spec == "./bufedge.mjs" {
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
	raw, err := os.ReadFile("testdata/bufedge_node.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &want))
	for name := range want {
		if bufferDeviation(name) {
			delete(want, name)
			delete(got, name)
		}
	}
	compareCases(t, want, got, false)
}

// bufferDeviation reports whether moejs's Buffer is known to differ from
// Node.js 24's in the case name, for one of the reasons TODO.md lists:
// the APIs it leaves out (BigInt reads and writes, copyBytesFrom, the
// *Slice and *Write methods, Blob, File, SlowBuffer, transcode,
// resolveObjectURL, the lengths of some methods); buffer.atob and btoa,
// which are moejs's global ones; a detached or resized buffer, where
// moejs reads what is left rather than throwing; a receiver that is not a
// Uint8Array; Node's pool, which shares an ArrayBuffer among small
// Buffers; copy's conversion of odd offsets and of other typed arrays;
// the constructor of a subclass's slices; and the engine's own message
// for a bad offset of the Uint8Array constructor.
func bufferDeviation(name string) bool {
	for _, p := range []string{"atob ", "btoa ", "detach ", "share ", "copy 4 ", "copy from u16", "from ab [\"-Infinity\"]",
		"from bigint", "fill 6 [\"U16:0\"]"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	switch name {
	case "api", "props", "static props", "constants", "offset parent", "subclass species", "Buffer name length",
		"atob no arg", "btoa no arg":
		return true
	}
	return strings.HasSuffix(name, " this") || name == "toJSON obj"
}

// edgeEval runs src as a script with Buffer global and returns its
// completion value and the error of the run.
func edgeEval(t *testing.T, ropts moejs.Options, src string) (string, error) {
	t.Helper()
	rt := moejs.NewRuntime(ropts)
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	api, err := node.InstallLoop(loop, node.Options{Buffer: true})
	require.NoError(t, err)
	defer api.Close()
	s, err := moejs.CompileScript("edge.js", src)
	require.NoError(t, err)
	var out string
	err = loop.Run(func(rt *moejs.Runtime) error {
		v, err := rt.RunScript(s)
		if err != nil {
			return err
		}
		out = v.String()
		return nil
	})
	return out, err
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestBufferEdgePanics runs the cases that may panic in Go, each in a
// runtime of its own.
func TestBufferEdgePanics(t *testing.T) {
	cases := []struct{ name, src string }{
		{"fill detach", `const b = Buffer.alloc(16); b.fill({ valueOf() { b.buffer.transfer(); return 1; } }, 8); b.length`},
		{"fill shrink", `const rab = new ArrayBuffer(16, { maxByteLength: 16 }); const b = Buffer.from(rab); b.fill({ valueOf() { rab.resize(4); return 1; } }, 8, 16); b.length`},
		{"fill shrink no end", `const rab = new ArrayBuffer(16, { maxByteLength: 16 }); const b = Buffer.from(rab); b.fill({ valueOf() { rab.resize(4); return 1; } }, 8); b.length`},
		{"concat Infinity", `try { Buffer.concat([{ length: Infinity }]).length } catch (e) { e.code }`},
		{"concat 1e300", `try { Buffer.concat([{ length: 1e300 }]).length } catch (e) { e.code }`},
		{"concat overflow", `try { Buffer.concat([{ length: 2 ** 62 }, { length: 2 ** 62 }]).length } catch (e) { e.code }`},
		{"concat 2^53", `try { Buffer.concat([{ length: 2 ** 53 }]).length } catch (e) { e.code }`},
		{"from ab 1e300 off", `try { Buffer.from(new ArrayBuffer(4), 1e300).length } catch (e) { e.code }`},
		{"toString huge end", `Buffer.from('abc').toString('utf8', 0, -1e300)`},
		{"indexOf detach encoding", `const b = Buffer.from('abcabc'); b.indexOf('c', 0, { toString() { b.buffer.transfer(); return 'utf8'; } })`},
		{"lastIndexOf detach encoding", `const b = Buffer.from('abcabc'); b.lastIndexOf('c', 5, { toString() { b.buffer.transfer(); return 'utf8'; } })`},
		{"indexOf detach offset", `const b = Buffer.from('abcabc'); b.indexOf('c', { valueOf() { b.buffer.transfer(); return 0; } })`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := edgeEval(t, moejs.Options{}, c.src)
			var ie *moejs.InternalError
			if errors.As(err, &ie) {
				t.Errorf("Go panic: %v\n%s", ie.Value, firstLines(string(ie.Stack), 14))
				return
			}
			t.Logf("out=%q err=%v", out, err)
		})
	}
}

// TestBufferEdgeMemory runs allocation-heavy cases under a 32 MiB memory
// limit and reports what each allocated.
func TestBufferEdgeMemory(t *testing.T) {
	const limit = 32 << 20
	cases := []struct{ name, src string }{
		{"alloc 2^31-1", `Buffer.alloc(2**31 - 1).length`},
		{"allocUnsafe 64M", `Buffer.allocUnsafe(64 * 2**20).length`},
		{"allocUnsafeSlow 64M", `Buffer.allocUnsafeSlow(64 * 2**20).length`},
		{"new Buffer 64M", `new Buffer(64 * 2**20).length`},
		{"alloc fill 64M", `Buffer.alloc(64 * 2**20, 'ab').length`},
		{"concat total 64M", `Buffer.concat([Buffer.alloc(1)], 64 * 2**20).length`},
		{"from arraylike 64M", `Buffer.from({ length: 64 * 2**20 }).length`},
		{"from array 2^22", `Buffer.from(new Array(2**22).fill(1)).length`},
		{"toString hex 12M", `Buffer.alloc(12 * 2**20).toString('hex').length`},
		{"toString base64 24M", `Buffer.alloc(24 * 2**20).toString('base64').length`},
		{"toString latin1 nonascii 20M", `Buffer.alloc(20 * 2**20, 0xe9).toString('latin1').length`},
		{"toString utf8 nonascii 20M", `Buffer.alloc(20 * 2**20, 0xe9).toString('utf8').length`},
		{"toString utf16le 20M", `Buffer.alloc(20 * 2**20, 0x41).toString('utf16le').length`},
		{"repeat 64M", `'a'.repeat(64 * 2**20).length`},
		{"from utf16le 12M string", `Buffer.from('a'.repeat(12 * 2**20), 'utf16le').length`},
		{"from utf8 of 3-byte 8M string", `Buffer.from('€'.repeat(8 * 2**20)).length`},
		{"toJSON 8M", `Buffer.alloc(8 * 2**20).toJSON().data.length`},
		{"stringify 4M", `JSON.stringify(Buffer.alloc(4 * 2**20)).length`},
		{"concat sparse 2^24", `try { Buffer.concat(new Array(2**24)) } catch (e) { e.name + ': ' + e.message }`},
		{"byteLength 16M", `Buffer.byteLength('é'.repeat(8 * 2**20))`},
		{"write huge latin1 into 1", `Buffer.alloc(1).write('a'.repeat(16 * 2**20), 'latin1')`},
		{"indexOf huge needle", `Buffer.alloc(1).indexOf('a'.repeat(16 * 2**20))`},
		{"fill huge pattern", `Buffer.alloc(1).fill('a'.repeat(16 * 2**20)).length`},
		{"inspect 16M", `Buffer.alloc(16 * 2**20)[Symbol.for('nodejs.util.inspect.custom')]().length`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runtime.GC()
			debug.FreeOSMemory()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			out, err := edgeEval(t, moejs.Options{MemoryLimit: limit}, c.src)
			runtime.ReadMemStats(&after)
			var ie *moejs.InternalError
			if errors.As(err, &ie) {
				t.Errorf("Go panic: %v", ie.Value)
			}
			t.Logf("out=%q err=%v | TotalAlloc +%d MiB, HeapSys %d MiB, memlimit=%v", out, err, (after.TotalAlloc-before.TotalAlloc)>>20, after.HeapSys>>20, errors.Is(err, moejs.ErrMemoryLimit))
		})
	}
}

// TestBufferWriteCopy checks that the asynchronous fs writes copy a
// Buffer's bytes when called, so that a script mutating it afterwards
// neither races with the worker (-race) nor changes what is written.
func TestBufferWriteCopy(t *testing.T) {
	h := newHarness(t, node.Options{FS: &node.FSOptions{Write: true, Workers: 2}, Buffer: true})
	res := h.run(`
	const buf = Buffer.alloc(1 << 20, 7);
	const p = fsp.writeFile('out.bin', buf);
	const q = new Promise((res, rej) => fs.writeFile('out2.bin', buf, e => e ? rej(e) : res()));
	const r = fsp.appendFile('out3.bin', buf.subarray(0, 4096));
	for (let i = 0; i < buf.length; i += 4096) buf[i] = 9;
	buf.fill(1, 0, 100000);
	await p; await q; await r;
	const a = fs.readFileSync('out.bin'), b = fs.readFileSync('out2.bin'), c = fs.readFileSync('out3.bin');
	return [a.indexOf(9), a.indexOf(1), b.indexOf(9), b.indexOf(1), c.indexOf(9), c.indexOf(1), a.length, b.length, c.length]`)
	t.Log(res)
	require.Equal(t, "[-1,-1,-1,-1,-1,-1,1048576,1048576,4096]", res)
}

package node_test

import (
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// benchFn returns a JavaScript function of the harness's runtime, made
// from body with fs in scope.
func benchFn(b *testing.B, h *harness, body string) moejs.Value {
	b.Helper()
	var fn moejs.Value
	require.NoError(b, h.rt.SetGlobal("setFn", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		fn = moejs.Arg(args, 0)
		return moejs.Undefined(), nil
	})))
	h.run(`setFn(() => { ` + body + ` })`)
	return fn
}

func BenchmarkSync(b *testing.B) {
	h := newHarness(&testing.T{}, node.Options{FS: &node.FSOptions{Root: b.TempDir()}, Buffer: true})
	writeTree(&testing.T{}, h.dir, map[string]string{"d/small.txt": strings.Repeat("x", 4096), "d/s.txt": "s"})
	for _, c := range []struct{ name, body string }{
		{"readFileSync-4KiB-utf8", `return fs.readFileSync("d/small.txt", "utf8").length`},
		{"readFileSync-4KiB-Buffer", `return fs.readFileSync("d/small.txt").length`},
		{"statSync", `return fs.statSync("d/s.txt").size`},
	} {
		b.Run(c.name, func(b *testing.B) {
			fn := benchFn(b, h, c.body)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := h.rt.CallFunction(fn, moejs.Undefined()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

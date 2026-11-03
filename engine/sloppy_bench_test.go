package engine

import (
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
)

// BenchmarkSloppyOps runs loops of ops dispatched off the jump table
// (sloppy.go): sloppy global writes, and with statement reads.
func BenchmarkSloppyOps(b *testing.B) {
	for _, bc := range []struct{ name, src string }{
		{"global", `var g = 0, h = 0; function loop(n) { for (var i = 0; i < n; i++) { g = i; h = g; } return h; }`},
		{"with", `var o = {x: 1, y: 2}; function loop(n) { var s = 0; with (o) { for (var i = 0; i < n; i++) s = x + y; } return s; }`},
	} {
		b.Run(bc.name, func(b *testing.B) {
			s, err := syntax.ParseScript("b.js", bc.src, syntax.Options{})
			if err != nil {
				b.Fatal(err)
			}
			code, err := compiler.CompileScript(s)
			if err != nil {
				b.Fatal(err)
			}
			r := NewRealm()
			if _, err := r.RunScript(code); err != nil {
				b.Fatal(err)
			}
			fn, err := r.Global.GetProp(r, r.KeyFromGoString("loop"))
			if err != nil {
				b.Fatal(err)
			}
			n := []Value{IntValue(100)}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := r.Call(fn, Undefined(), n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

package moejs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHeldAsyncPromiseRetains: a promise an async function call returned,
// held after the call settled it, keeps the result only: not the
// function's closure and the scope it captured, its this, or its frame's
// registers. Held while the call waits, it does not keep the frame
// either: the promise the call awaits does. A hundred held promises whose
// calls each captured, received or held a 320 KB array retain a few
// hundred KB, not the arrays.
func TestHeldAsyncPromiseRetains(t *testing.T) {
	cases := []struct{ name, src string }{
		{"closure captures a big local, no await", `var held = [];
			function mk(i) { const big = new Array(20000).fill(i); const g = async () => big.length; return g(); }
			for (var i = 0; i < N; i++) held.push(mk(i));`},
		{"closure captures a big local, after an await", `var held = [];
			function mk(i) { const big = new Array(20000).fill(i); const g = async () => { await null; return big.length; }; return g(); }
			for (var i = 0; i < N; i++) held.push(mk(i));`},
		{"method of a big this, no await", `var held = [];
			for (var i = 0; i < N; i++) { const o = { big: new Array(20000).fill(i), async m() { return 1; } }; held.push(o.m()); }`},
		{"big local in a register, after an await", `var held = [];
			async function g(i) { const big = new Array(20000).fill(i); await null; return big.length; }
			for (var i = 0; i < N; i++) held.push(g(i));`},
		{"waiting on a promise nothing settles or holds", `var held = [];
			async function g(i) { const big = new Array(20000).fill(i); await new Promise(() => {}); return big.length; }
			for (var i = 0; i < N; i++) held.push(g(i));`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scriptRetained(t, tc.src, 100)
			t.Logf("retained %.1f KiB", float64(got)/1024)
			assert.Less(t, got, uint64(8<<20), "held settled promises keep their calls")
		})
	}
}

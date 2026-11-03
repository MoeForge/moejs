package engine

import (
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/stretchr/testify/assert"
)

// TestRegisterWindowMoves runs frames whose register window moves while
// they wait: a deep recursion grows, and so reallocates, the register
// stack inside a native's callback, between two steps of a generator and
// of an async function, inside a getter, a sort comparator and direct
// eval code, and before an exception reaches a catch. Each frame must find
// its registers afterwards (run takes its window again after every
// operation that may move the stack); the checked build (regcheck.go)
// fails on a window kept across a move or a frame past the stack.
func TestRegisterWindowMoves(t *testing.T) {
	SetCompiler(compiler.Hook{}) // for direct eval
	t.Cleanup(func() { SetCompiler(nil) })
	const deep = `function deep(n) { return n === 0 ? 0 : 1 + deep(n - 1); } `
	for _, tc := range []scriptCase{
		{"a native's callback", []string{deep + `function f() { var a = 1, b = 2, c = 3; var r = [1, 2].map(function (x) { return deep(500) + x; }); return [a, b, c, r].join(); } f()`}, "1,2,3,501,502"},
		{"generator steps", []string{deep + `function* g(x) { var k = x * 2; yield k; deep(300); yield k + x; } function f() { var it = g(5), a = it.next().value; deep(350); var b = it.next().value; return a + ":" + b; } f()`}, "10:15"},
		{"async steps", []string{deep + `var out; async function h(x) { var k = x + 1; await null; deep(300); out = k + ":" + deep(10); } h(41); deep(400);`, `out`}, "42:10"},
		{"a getter", []string{deep + `var o = {get v() { return deep(300); }}; function f() { var a = "a", b = o.v, c = "c"; return a + b + c; } f()`}, "a300c"},
		{"a sort comparator", []string{deep + `function f() { var tag = "t"; var s = [3, 1, 2].sort(function (x, y) { deep(300); return x - y; }); return tag + s.join(""); } f()`}, "t123"},
		{"direct eval", []string{deep + `function f() { var a = 7; var r = eval("deep(300) + a"); return [a, r].join(); } f()`}, "7,307"},
		{"a catch after the move", []string{deep + `function boom(n) { if (n === 0) throw new Error("e" + deep(50)); return boom(n - 1); } function f() { var keep = "k"; try { boom(300); } catch (e) { return keep + e.message; } } f()`}, "ke50"},
		{"a finally after the move", []string{deep + `function boom(n) { if (n === 0) throw 1; return boom(n - 1); } function f() { var log = []; try { try { boom(300); } finally { log.push(deep(20)); } } catch (e) { log.push(e); } return log.join(); } f()`}, "20,1"},
		{"a host call back into JavaScript", []string{deep + `function f() { var a = 1; var s = String({toString() { return "x" + deep(400); }}); return a + s; } f()`}, "1x400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRealm()
			assert.Equal(t, tc.want, runScriptsIn(t, r, tc.srcs...))
			// From the initial 64 registers the stack doubled at least four
			// times, each time into a new array.
			assert.GreaterOrEqual(t, cap(r.interp.stack), 16*initialStackSize, "the stack moved")
		})
	}
}

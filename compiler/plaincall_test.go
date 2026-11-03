package compiler

import (
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
)

// words returns the instructions of fn, each with its extra words.
func words(fn *bytecode.Function) [][]uint32 {
	var out [][]uint32
	for pc := 0; pc < len(fn.Code); {
		n := 1 + bytecode.DecodeOp(fn.Code[pc]).ExtraWords()
		out = append(out, fn.Code[pc:pc+n])
		pc += n
	}
	return out
}

// TestPlainCallOperand checks that a plain call says its this is undefined
// in the Call's C operand instead of loading undefined, and that method
// calls, spread calls and direct evals keep their this register.
func TestPlainCallOperand(t *testing.T) {
	fn := compileModule(t, `
export function plain(g) { return g(1); }
export function method(o) { return o.m(1); }
export function spread(g) { return g(...[1]); }
export function tagged(g) { return g`+"`x`"+`; }
export function optional(g) { return g?.(); }`)
	call := func(name string) (calls []uint8, undefs int) {
		for _, w := range words(child(t, fn, name)) {
			switch bytecode.DecodeOp(w[0]) {
			case bytecode.Call:
				calls = append(calls, bytecode.DecodeC(w[0]))
			case bytecode.LoadUndef:
				undefs++
			}
		}
		return calls, undefs
	}
	// The optional call's one LoadUndef is its value when g is nullish.
	for name, want := range map[string]int{"plain": 0, "tagged": 0, "optional": 1} {
		calls, undefs := call(name)
		assert.Equal(t, []uint8{bytecode.CallUndefThis}, calls, name)
		assert.Equal(t, want, undefs, name)
	}
	calls, undefs := call("method")
	assert.Equal(t, []uint8{0}, calls)
	assert.Zero(t, undefs)
	assert.Contains(t, ops(child(t, fn, "spread")), bytecode.LoadUndef, "a spread call loads its this")
}

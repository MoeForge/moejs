package compiler

import (
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
)

// TestCompareImmediateEmission checks which comparisons that decide a jump
// take their integer right operand in the X word (JmpLtI .. JmpStrictNeI):
// int32 literals but -0, not a literal a loop nest holds in a register, and
// not a comparison whose value is used.
func TestCompareImmediateEmission(t *testing.T) {
	fn := compileModule(t, `
export function imm(n) { if (n < 2) return n; if (n === -7) return 0; return n >= 2147483647 ? 1 : n <= -2147483648 ? 2 : 3; }
export function reg(n) { return n < 1.5 || n > -0 || n < 2147483648 || n === "2" ? 1 : 0; }
export function value(n) { return n < 2; }
export function loop(n) { var s = 0; for (var i = 0; i < 1000; i++) s += n; return s; }`)
	imms := func(name string) (got []int32, regs int) {
		for _, w := range words(child(t, fn, name)) {
			switch op := bytecode.DecodeOp(w[0]); {
			case op >= bytecode.JmpLtI && op <= bytecode.JmpStrictNeI:
				got = append(got, int32(w[1]))
			case op >= bytecode.JmpLt && op <= bytecode.JmpStrictNe:
				regs++
			}
		}
		return got, regs
	}
	got, regs := imms("imm")
	assert.Equal(t, []int32{2, -7, 2147483647, -2147483648}, got)
	assert.Zero(t, regs)
	got, regs = imms("reg")
	assert.Empty(t, got)
	assert.Equal(t, 4, regs)
	got, regs = imms("value")
	assert.Empty(t, got)
	assert.Zero(t, regs)
	assert.Contains(t, ops(child(t, fn, "value")), bytecode.Lt)
	got, regs = imms("loop")
	assert.Empty(t, got, "the loop's bound is hoisted into a register")
	assert.Equal(t, 1, regs)
}

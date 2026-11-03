package compiler

import (
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
)

// TestRotatedLoopShape checks which loops end in their condition's jump:
// for, while and do-while with a relational condition (register or
// immediate operand) enter at the condition and close with a backward
// compare-and-branch; truthiness and equality conditions, constant ones
// and missing ones keep the test at the top and a backward Jmp.
func TestRotatedLoopShape(t *testing.T) {
	fn := compileModule(t, `
export function forReg(n) { var s = 0; for (var i = 0; i < n; i++) s++; return s; }
export function whileHoisted(n) { var s = 0; while (n > 3) { n--; s++; } return s; }
export function whileImm(n) { var s = 0; while (n > 3) { n--; s = s * 10 * 10 + s * 11 * 11 + s * 12 * 12 + s * 13 * 13 + s * 14 * 14 + s * 15 * 15 + s * 16 * 16 + s * 17 * 17 + s * 18 * 18; } return s; }
export function doReg(n) { var i = 0; do i++; while (i <= n); return i; }
export function whileTruthy(n) { while (n) n--; return n; }
export function forStrict(n) { var i = 0; for (; i !== n; i++); return i; }
export function forever() { for (;;) return 1; }
export function whileTrue(n) { while (true) { if (n-- < 0) break; } return n; }`)
	// backEdge returns the op of each backward jump and whether the code
	// before the loop enters it with a forward Jmp to its end.
	backEdge := func(name string) (ops []bytecode.Op, entry bool) {
		f := child(t, fn, name)
		for _, w := range words(f) {
			op := bytecode.DecodeOp(w[0])
			if fm := op.Format(); fm == bytecode.FmtSBx || fm == bytecode.FmtAsBx && op != bytecode.LoadInt {
				if bytecode.DecodeSBx(w[0]) < 0 {
					ops = append(ops, op)
				} else if op == bytecode.Jmp && len(ops) == 0 {
					entry = true
				}
			}
		}
		return ops, entry
	}
	// whileHoisted's 3 is a constant the loop nest holds in a register;
	// whileImm's is not among the eight it keeps.
	for name, want := range map[string]bytecode.Op{"forReg": bytecode.JmpLt, "whileHoisted": bytecode.JmpGt, "whileImm": bytecode.JmpGtI, "doReg": bytecode.JmpLe} {
		ops, entry := backEdge(name)
		assert.Equal(t, []bytecode.Op{want}, ops, name)
		assert.Equal(t, name != "doReg", entry, "%s: entered at the condition", name)
	}
	for _, name := range []string{"whileTruthy", "forStrict", "forever", "whileTrue"} {
		ops, _ := backEdge(name)
		assert.Equal(t, []bytecode.Op{bytecode.Jmp}, ops, name)
	}
}

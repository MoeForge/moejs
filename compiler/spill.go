package compiler

import (
	"cmp"
	"slices"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Spilling bindings out of registers. An un-captured binding of a function
// lives in a register, so a function declaring more than the 8-bit
// operands reach (bundlers' outer IIFEs: hundreds of vars and function
// declarations) cannot compile. When such a function runs out of
// registers, compileUnit compiles the unit again with some of its
// bindings spilled (spillRetry): they take slots of the function's own
// environment after its captured bindings, as if captured, and are read
// and written with GetEnv and SetEnv; their TDZ checks are those of a
// captured binding. The environment is the function's, entered at the
// call, so spilling adds no environment to the chain: the depths of the
// inner functions' references do not change, nor do the PopEnv counts of
// the function's exits. A function that compiles without spilling compiles
// as it did.
//
// The bindings spilled are those of the function's own scope that are not
// parameters (which arrive in registers) or the arguments object, the
// least referenced first, until at most spillKeep stay in registers, or at
// least spillMin are spilled when the bindings alone did not run out. A
// function with a direct eval call spills nothing: its eval code reads the
// environment's slots by the names its levels record.

// spillKeep is the most bindings of a spilling function left in registers,
// leaving the rest of the 256 to temporaries.
const spillKeep = 192

// spillMin is the fewest bindings a spilling function spills.
const spillMin = 32

// spillRetry is the panic of a function that ran out of registers with
// bindings it may spill: compileUnit compiles the unit again with them
// spilled.
type spillRetry struct{ fn *syntax.Function }

func (r *spillRetry) Error() string { return "compiler: out of registers with bindings to spill" }

// canSpill reports whether b, a binding of a function's own scope, may be
// spilled.
func canSpill(b *syntax.Binding) bool {
	if b.Captured {
		return false
	}
	switch b.Kind {
	case syntax.BindParam, syntax.BindArgs, syntax.BindHidden, syntax.BindFuncName:
		return false
	}
	return true
}

// spillable reports whether fn may spill bindings when it runs out of
// registers.
func spillable(fn *syntax.Function) bool {
	if fn == nil || fn.Scope == nil || fn.HasDirectEval {
		return false
	}
	for _, b := range fn.Scope.Bindings {
		if canSpill(b) {
			return true
		}
	}
	return false
}

// spillVictims returns the bindings fn spills, in the order of its scope.
func spillVictims(fn *syntax.Function) map[*syntax.Binding]bool {
	uses := make(map[*syntax.Binding]int)
	var cands []*syntax.Binding
	for _, b := range fn.Scope.Bindings {
		if canSpill(b) {
			uses[b] = 0
			cands = append(cands, b)
		}
	}
	syntax.Inspect(fn, func(n syntax.Node) bool {
		if id, ok := n.(*syntax.Ident); ok && id.Binding != nil {
			if u, ok := uses[id.Binding]; ok {
				uses[id.Binding] = u + 1
			}
		}
		return true
	})
	n := max(len(cands)-spillKeep, spillMin)
	n = min(n, len(cands))
	captured := 0
	for _, b := range fn.Scope.Bindings {
		if b.Captured {
			captured++
		}
	}
	if captured+n > bytecode.MaxBx+1 {
		return nil // past the environment's slots: the function spills nothing
	}
	order := slices.Clone(cands)
	slices.SortStableFunc(order, func(a, b *syntax.Binding) int { return cmp.Compare(uses[a], uses[b]) })
	out := make(map[*syntax.Binding]bool, n)
	for _, b := range order[:n] {
		out[b] = true
	}
	return out
}

// spilled returns the bindings fn spills, and whether the compilation was
// retried for fn so (when it spills nothing, fn was not).
func (c *compiler) spilled(fn *syntax.Function) (map[*syntax.Binding]bool, bool) {
	if c.hoist == nil || c.hoist.spill == nil {
		return nil, false
	}
	m, ok := c.hoist.spill[fn]
	return m, ok
}

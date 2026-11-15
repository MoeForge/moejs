package syntax

// HasNonNullResult reports whether evaluating e successfully always produces
// a value other than null or undefined. It does not permit skipping the
// evaluation: even a known result may require user code or throw.
func HasNonNullResult(e Expr) bool {
	switch e.(type) {
	case *ObjectLit, *ArrayLit, *Function, *Class, *NewExpr,
		*StringLit, *NumberLit, *BigIntLit, *BoolLit, *RegexLit, *TemplateLit:
		return true
	}
	return false
}

// HoldsNonNull reports whether every initialized read of b yields a value
// other than null or undefined: a const, or a let that no code can write
// after its declaration, whose declaration initializes it with such a
// value. A read in the temporal dead zone still throws first.
func (b *Binding) HoldsNonNull() bool {
	if !b.NonNullInit {
		return false
	}
	switch b.Kind {
	case BindConst:
		return true
	case BindLet:
		return !b.Reassigned && !b.EvalReach
	}
	return false
}

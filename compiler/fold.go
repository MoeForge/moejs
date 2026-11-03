package compiler

import (
	"math"
	"strings"

	"github.com/MoeForge/moejs/syntax"
)

// constKind classifies a folded constant.
type constKind uint8

const (
	kNone constKind = iota
	kNumber
	kString
	kBool
	kNull
	kUndefined
)

// constant is a compile-time literal value.
type constant struct {
	kind constKind
	num  float64
	str  string // WTF-8
	b    bool
}

func float64Bits(f float64) uint64 { return math.Float64bits(f) }

// fold evaluates e when it is a literal expression built from literals and
// pure operators; ok is false otherwise.
func (f *funcState) fold(e syntax.Expr) (constant, bool) {
	switch e := e.(type) {
	case *syntax.NumberLit:
		return constant{kind: kNumber, num: e.Value}, true
	case *syntax.StringLit:
		return constant{kind: kString, str: e.Value}, true
	case *syntax.BoolLit:
		return constant{kind: kBool, b: e.Value}, true
	case *syntax.NullLit:
		return constant{kind: kNull}, true
	case *syntax.Ident:
		if e.Binding == nil && f.withsFor(nil) == nil {
			switch e.Name {
			case "undefined":
				return constant{kind: kUndefined}, true
			case "NaN":
				return constant{kind: kNumber, num: math.NaN()}, true
			case "Infinity":
				return constant{kind: kNumber, num: math.Inf(1)}, true
			}
		}
	case *syntax.UnaryExpr:
		x, ok := f.fold(e.X)
		if !ok {
			return constant{}, false
		}
		switch e.Op {
		case syntax.Minus:
			if x.kind == kNumber {
				return constant{kind: kNumber, num: -x.num}, true
			}
		case syntax.Plus:
			if x.kind == kNumber {
				return x, true
			}
		case syntax.Not:
			return constant{kind: kBool, b: !x.truthy()}, true
		case syntax.KwVoid:
			return constant{kind: kUndefined}, true
		case syntax.KwTypeof:
			return constant{kind: kString, str: x.typeName()}, true
		case syntax.BitNot:
			if x.kind == kNumber {
				return constant{kind: kNumber, num: float64(^toInt32(x.num))}, true
			}
		}
	case *syntax.BinaryExpr, *syntax.LogicalExpr:
		return f.foldChain(e)
	case *syntax.CondExpr:
		t, ok := f.fold(e.Test)
		if !ok {
			return constant{}, false
		}
		if t.truthy() {
			return f.fold(e.Cons)
		}
		return f.fold(e.Alt)
	case *syntax.SeqExpr:
		var last constant
		for _, x := range e.Exprs {
			c, ok := f.fold(x)
			if !ok {
				return constant{}, false
			}
			last = c
		}
		return last, true
	}
	return constant{}, false
}

// foldChain folds a binary or logical expression, walking the left spine of
// a left-nested chain (a+b+c..., a&&b||c...) iteratively. Compiling a chain
// folds the left operand again at every level on the way down, which was
// quadratic in the chain length (a 100,000-term sum took 55 s to compile), so
// the spine nodes a failed fold proved unfoldable are remembered: the next
// queries down the spine (f.noFold through f.noFoldEnd) answer in O(1).
func (f *funcState) foldChain(e syntax.Expr) (constant, bool) {
	if e == f.noFold {
		if e == f.noFoldEnd {
			f.noFold = nil
		} else {
			f.noFold, _ = leftOperand(e)
		}
		return constant{}, false
	}
	var buf [8]syntax.Expr
	spine := buf[:0]
	x := e
	for {
		l, ok := leftOperand(x)
		if !ok {
			break
		}
		spine = append(spine, x)
		x = l
	}
	c, ok := f.fold(x)
	end := len(spine) - 1 // spine[:end+1] does not fold when !ok
	// A string grown by + links is built in cat (c.str == cat.String() while
	// cat is not empty): concatenating at every level copied O(n^2) bytes for
	// a long "a" + "b" + ... chain.
	var cat strings.Builder
	for ; ok && end >= 0; end-- {
		switch l := spine[end].(type) {
		case *syntax.BinaryExpr:
			if c.kind == kString && l.Op == syntax.Plus {
				var y constant
				if y, ok = f.fold(l.Y); ok && y.kind == kString {
					if cat.Len() == 0 {
						cat.WriteString(c.str)
					}
					cat.WriteString(y.str)
					c.str = cat.String()
					continue
				}
				ok = false
			}
		case *syntax.LogicalExpr:
			if shortCircuits(l.Op, c) {
				continue
			}
		}
		if !ok {
			break
		}
		if c, ok = f.foldLink(spine[end], c); !ok {
			break
		}
		cat.Reset()
	}
	if ok {
		return c, true
	}
	if end >= 1 {
		f.noFold, f.noFoldEnd = spine[1], spine[end]
	}
	return constant{}, false
}

// leftOperand returns the left operand of a binary or logical expression.
func leftOperand(e syntax.Expr) (syntax.Expr, bool) {
	switch e := e.(type) {
	case *syntax.BinaryExpr:
		return e.X, true
	case *syntax.LogicalExpr:
		return e.X, true
	}
	return nil, false
}

// foldLink folds one binary or logical node whose left operand folded to x.
func (f *funcState) foldLink(e syntax.Expr, x constant) (constant, bool) {
	switch e := e.(type) {
	case *syntax.BinaryExpr:
		y, ok := f.fold(e.Y)
		if !ok {
			return constant{}, false
		}
		return foldBinary(e.Op, x, y)
	case *syntax.LogicalExpr:
		if shortCircuits(e.Op, x) {
			return x, true
		}
		return f.fold(e.Y)
	}
	return constant{}, false
}

// shortCircuits reports whether the logical operator op yields its left
// operand x without evaluating the right one.
func shortCircuits(op syntax.Token, x constant) bool {
	switch op {
	case syntax.LogOr:
		return x.truthy()
	case syntax.LogAnd:
		return !x.truthy()
	case syntax.Nullish:
		return x.kind != kNull && x.kind != kUndefined
	}
	return false
}

func (c constant) truthy() bool {
	switch c.kind {
	case kNumber:
		return c.num != 0 && !math.IsNaN(c.num)
	case kString:
		return c.str != ""
	case kBool:
		return c.b
	}
	return false
}

func (c constant) typeName() string {
	switch c.kind {
	case kNumber:
		return "number"
	case kString:
		return "string"
	case kBool:
		return "boolean"
	case kNull:
		return "object"
	}
	return "undefined"
}

func foldBinary(op syntax.Token, x, y constant) (constant, bool) {
	if x.kind == kString && y.kind == kString && op == syntax.Plus {
		return constant{kind: kString, str: x.str + y.str}, true
	}
	if x.kind != kNumber || y.kind != kNumber {
		return constant{}, false
	}
	a, b := x.num, y.num
	switch op {
	case syntax.Plus:
		return constant{kind: kNumber, num: a + b}, true
	case syntax.Minus:
		return constant{kind: kNumber, num: a - b}, true
	case syntax.Mul:
		return constant{kind: kNumber, num: a * b}, true
	case syntax.Div:
		return constant{kind: kNumber, num: a / b}, true
	case syntax.Rem:
		return constant{kind: kNumber, num: math.Mod(a, b)}, true
	case syntax.Exp:
		return constant{kind: kNumber, num: jsPow(a, b)}, true
	case syntax.BitAnd:
		return constant{kind: kNumber, num: float64(toInt32(a) & toInt32(b))}, true
	case syntax.BitOr:
		return constant{kind: kNumber, num: float64(toInt32(a) | toInt32(b))}, true
	case syntax.BitXor:
		return constant{kind: kNumber, num: float64(toInt32(a) ^ toInt32(b))}, true
	case syntax.Shl:
		return constant{kind: kNumber, num: float64(toInt32(a) << (toUint32(b) & 31))}, true
	case syntax.Shr:
		return constant{kind: kNumber, num: float64(toInt32(a) >> (toUint32(b) & 31))}, true
	case syntax.UShr:
		return constant{kind: kNumber, num: float64(toUint32(a) >> (toUint32(b) & 31))}, true
	case syntax.Lt:
		return constant{kind: kBool, b: a < b}, true
	case syntax.Gt:
		return constant{kind: kBool, b: a > b}, true
	case syntax.LtEq:
		return constant{kind: kBool, b: a <= b}, true
	case syntax.GtEq:
		return constant{kind: kBool, b: a >= b}, true
	case syntax.StrictEq, syntax.Eq:
		return constant{kind: kBool, b: a == b}, true
	case syntax.StrictNeq, syntax.NotEq:
		return constant{kind: kBool, b: a != b}, true
	}
	return constant{}, false
}

// jsPow implements Number::exponentiate (math.Pow differs for |base| == 1
// with an infinite exponent and for NaN exponents).
func jsPow(x, y float64) float64 {
	if math.IsNaN(y) {
		return math.NaN()
	}
	if y == 0 {
		return 1
	}
	if math.IsInf(y, 0) && (x == 1 || x == -1) {
		return math.NaN()
	}
	return math.Pow(x, y)
}

// toInt32 implements ToInt32 on a number.
func toInt32(f float64) int32 {
	if f >= -2147483648 && f <= 2147483647 {
		return int32(f)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	f = math.Mod(math.Trunc(f), 4294967296)
	return int32(uint32(int64(f)))
}

// toUint32 implements ToUint32 on a number.
func toUint32(f float64) uint32 {
	if f >= 0 && f <= 4294967295 {
		return uint32(f)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	f = math.Mod(math.Trunc(f), 4294967296)
	return uint32(int64(f))
}

// containsAssign reports whether evaluating e may assign to a register
// local: it contains an assignment or update expression outside nested
// functions (bindings assigned from nested functions are captured and never
// live in registers).
func containsAssign(e syntax.Expr) bool {
	found := false
	syntax.Inspect(e, func(n syntax.Node) bool {
		if found {
			return false
		}
		switch n.(type) {
		case *syntax.AssignExpr, *syntax.UpdateExpr:
			found = true
			return false
		case *syntax.Function:
			return false
		}
		return true
	})
	return found
}

// isArrayIndexName reports whether s is a canonical array index string, in
// which case it must not be used as a named-property constant.
func isArrayIndexName(s string) bool {
	n := len(s)
	if n == 0 || n > 10 {
		return false
	}
	if s[0] == '0' {
		return n == 1
	}
	var v uint64
	for i := range n {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
		v = v*10 + uint64(c-'0')
	}
	return v <= 1<<32-2
}

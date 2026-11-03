package compiler

// calleePrinter is ported from V8's CallPrinter (src/ast/prettyprinter.cc;
// BSD-3-Clause; see THIRD_PARTY_NOTICES).

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"weak"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// Callee descriptions. A call of a value that is not callable names the
// callee expression as written, in V8's wording ("obj.foo is not a
// function"). Nothing is recorded at compile time: the first such error in a
// function re-parses its source, pairs its call instructions with the call
// sites by line table position, and caches the descriptions of all of them.
// Compiled size and the success path are untouched.

func init() { bytecode.DescribeCallee = describeCallee }

// calleeTables caches, per template (weakly), the descriptions of its call
// sites by pc. It only holds functions whose calls have failed and is
// dropped as a whole when it grows past maxCalleeTables.
var calleeTables struct {
	sync.Mutex
	m map[weak.Pointer[bytecode.Function]]map[uint32]string
}

const maxCalleeTables = 64

func describeCallee(fn *bytecode.Function, pc uint32) string {
	if fn == nil || int(pc) >= len(fn.Code) {
		return ""
	}
	k := weak.Make(fn)
	calleeTables.Lock()
	t, ok := calleeTables.m[k]
	calleeTables.Unlock()
	if !ok {
		t = calleeTable(fn)
		calleeTables.Lock()
		if calleeTables.m == nil || len(calleeTables.m) >= maxCalleeTables {
			calleeTables.m = make(map[weak.Pointer[bytecode.Function]]map[uint32]string)
		}
		calleeTables.m[k] = t
		calleeTables.Unlock()
	}
	return t[pc]
}

// siteKey is the source position of a call (ctor false) or new instruction.
type siteKey struct {
	ctor      bool
	line, col int
}

// calleeTable describes the callees of the call and new instructions of fn
// by pc. The instructions at one position must pair up one to one with the
// syntax nodes there, else none of them is described.
func calleeTable(fn *bytecode.Function) map[uint32]string {
	src := fn.Source
	if src == nil {
		return nil
	}
	prog := parseSource(src.Name, src.Src)
	if prog == nil {
		return nil
	}
	code := []syntax.Node{prog}
	if fn.Kind != bytecode.KindModule && fn.Kind != bytecode.KindScript {
		code = functionCode(prog, fn)
		if code == nil && fn.Kind == bytecode.KindArrow && src.Start == 0 && src.End == len(src.Src) {
			code = []syntax.Node{prog} // eval code (CompileEval): the whole source, like a script
		}
		if code == nil {
			return nil
		}
	}
	sites := callSites(prog.File, code)
	pcs := make(map[siteKey][]uint32)
	for pc := 0; pc < len(fn.Code); {
		op := bytecode.DecodeOp(fn.Code[pc])
		switch op {
		case bytecode.Call, bytecode.CallSpread, bytecode.CallEval, bytecode.New, bytecode.NewSpread:
			line, col := fn.Position(uint32(pc))
			k := siteKey{op == bytecode.New || op == bytecode.NewSpread, line, col}
			pcs[k] = append(pcs[k], uint32(pc))
		}
		pc += 1 + op.ExtraWords()
	}
	t := make(map[uint32]string)
	for k, list := range pcs {
		nodes := sites[k]
		if len(nodes) != len(list) {
			continue
		}
		for i, pc := range list {
			t[pc] = describeSite(nodes[i])
		}
	}
	return t
}

// parseSource parses the source text of a compiled module or script.
func parseSource(name, src string) *syntax.Program {
	if m, err := syntax.ParseModule(name, src, syntax.Options{}); err == nil {
		return &m.Program
	}
	if s, err := syntax.ParseScript(name, src, syntax.Options{}); err == nil {
		return &s.Program
	}
	return nil
}

// functionCode returns the syntax compiled into fn's own code: the function
// spanning its source, or nil. A class constructor and the synthetic
// initializers of a class's fields and static elements have the class's
// source; theirs is the constructor or the field initializers (a static
// block is a function of its own).
func functionCode(prog *syntax.Program, fn *bytecode.Function) []syntax.Node {
	start, end := fn.Source.Start, fn.Source.End
	var found []syntax.Node
	syntax.Inspect(prog, func(n syntax.Node) bool {
		if found != nil {
			return false
		}
		switch n := n.(type) {
		case *syntax.Function:
			if n.Pos == start && n.End == end {
				found = []syntax.Node{n}
			}
		case *syntax.ClassMember:
			if b := n.Block; b != nil && b.Pos == start && b.End == end {
				found = []syntax.Node{b}
			}
		case *syntax.Class:
			if n.Pos == start && n.End == end {
				found = classCode(n, fn)
			}
		}
		return found == nil
	})
	return found
}

// classCode returns the syntax compiled into fn, the constructor or a
// synthetic initializer of class c.
func classCode(c *syntax.Class, fn *bytecode.Function) []syntax.Node {
	if fn.Kind.IsClassCtor() {
		return []syntax.Node{c.Ctor}
	}
	static := fn.Name == "<static_initializer>"
	var code []syntax.Node
	for _, m := range c.Members {
		if _, isFn := m.Value.(*syntax.Function); m.Kind == syntax.ClassField && m.Static == static && m.Value != nil && !isFn {
			code = append(code, m.Value)
		}
	}
	return code
}

// callSites groups the calls, tagged templates and new expressions compiled
// into code (not into nested functions) by the position the compiler gives
// their instruction (callPos, or the `new` keyword), in emission order.
func callSites(file *syntax.File, code []syntax.Node) map[siteKey][]syntax.Node {
	sites := make(map[siteKey][]syntax.Node)
	add := func(ctor bool, pos int, n syntax.Node) {
		line, col := file.Position(pos)
		k := siteKey{ctor, line, col}
		sites[k] = append(sites[k], n)
	}
	for _, root := range code {
		syntax.Inspect(root, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.Function:
				return syntax.Node(n) == root
			case *syntax.CallExpr:
				if _, super := n.Callee.(*syntax.SuperExpr); !super { // a SuperCall
					add(false, callPos(n.Callee), n)
				}
			case *syntax.TaggedTemplate:
				add(false, callPos(n.Tag), n)
			case *syntax.NewExpr:
				add(true, n.Pos, n)
			}
			return true
		})
	}
	// Sites at one position are nested through their callees, and an inner
	// call runs (and is emitted) before the call containing it; Inspect
	// visits the outer one first.
	for _, list := range sites {
		slices.Reverse(list)
	}
	return sites
}

// describeSite prints the callee of a call site.
func describeSite(n syntax.Node) string {
	var p calleePrinter
	switch n := n.(type) {
	case *syntax.CallExpr:
		p.find(n.Callee, true)
	case *syntax.TaggedTemplate:
		p.find(n.Tag, true)
	case *syntax.NewExpr:
		p.find(n.Callee, true)
	}
	return p.String()
}

const intermediate = "(intermediate value)"

// calleePrinter prints an expression as V8's CallPrinter does for the
// callee of a failed call: names, member chains and literals as written,
// inner calls as "f(...)", operators parenthesised, and
// "(intermediate value)" for what it does not spell out. Literal operands
// are folded as V8's parser folds them (-1, !0, 1 + 2).
type calleePrinter struct{ strings.Builder }

// find prints n when print is set, and "(intermediate value)" when that
// prints nothing or print is not set.
func (p *calleePrinter) find(n syntax.Node, print bool) {
	if print && n != nil {
		l := p.Len()
		p.visit(n)
		if p.Len() != l {
			return
		}
	}
	p.WriteString(intermediate)
}

func (p *calleePrinter) visit(n syntax.Node) {
	if e, ok := n.(syntax.Expr); ok {
		if c, ok := v8Literal(e); ok {
			p.literal(c, true)
			return
		}
	}
	switch n := n.(type) {
	case *syntax.Ident:
		p.WriteString(n.Name)
	case *syntax.ThisExpr:
		p.WriteString("this")
	case *syntax.RegexLit:
		p.WriteString("/" + n.Pattern + "/")
		for _, c := range "dgimsuvy" {
			if strings.ContainsRune(n.Flags, c) {
				p.WriteRune(c)
			}
		}
	case *syntax.TemplateLit:
		for _, x := range n.Exprs {
			p.find(x, true)
		}
	case *syntax.ArrayLit:
		p.WriteByte('[')
		for i, x := range n.Elems {
			if i > 0 {
				p.WriteByte(',')
			}
			p.find(x, true)
		}
		p.WriteByte(']')
	case *syntax.ObjectLit:
		p.WriteString("{" + strings.Repeat(intermediate, len(n.Props)) + "}")
	case *syntax.UnaryExpr:
		p.WriteString("(" + n.Op.String())
		if n.Op == syntax.KwTypeof || n.Op == syntax.KwVoid || n.Op == syntax.KwDelete {
			p.WriteByte(' ')
		}
		p.find(n.X, true)
		p.WriteByte(')')
	case *syntax.UpdateExpr:
		p.WriteByte('(')
		if n.Prefix {
			p.WriteString(n.Op.String())
		}
		p.find(n.X, true)
		if !n.Prefix {
			p.WriteString(n.Op.String())
		}
		p.WriteByte(')')
	case *syntax.BinaryExpr:
		switch n.Op {
		case syntax.Lt, syntax.Gt, syntax.LtEq, syntax.GtEq, syntax.Eq, syntax.NotEq,
			syntax.StrictEq, syntax.StrictNeq, syntax.KwInstanceof, syntax.KwIn:
			p.operands(n.Op, []syntax.Expr{n.X, n.Y})
			return
		}
		// V8 collapses a left-nested chain of one operator (but **).
		ops := []syntax.Expr{n.Y}
		x := n.X
		for n.Op != syntax.Exp {
			l, ok := x.(*syntax.BinaryExpr)
			if _, lit := v8Literal(x); !ok || lit || l.Op != n.Op {
				break
			}
			ops, x = append(ops, l.Y), l.X
		}
		ops = append(ops, x)
		slices.Reverse(ops)
		p.operands(n.Op, ops)
	case *syntax.LogicalExpr:
		ops := []syntax.Expr{n.Y}
		x := n.X
		for {
			l, ok := x.(*syntax.LogicalExpr)
			if !ok || l.Op != n.Op {
				break
			}
			ops, x = append(ops, l.Y), l.X
		}
		ops = append(ops, x)
		slices.Reverse(ops)
		p.operands(n.Op, ops)
	case *syntax.SeqExpr:
		p.operands(syntax.Comma, n.Exprs)
	case *syntax.AssignExpr:
		p.find(n.Target, true)
	case *syntax.CondExpr:
		p.WriteString(intermediate + intermediate + intermediate)
	case *syntax.CallExpr:
		p.find(n.Callee, true)
		p.WriteString("(...)")
	case *syntax.TaggedTemplate:
		p.find(n.Tag, true)
		p.WriteString("(...)")
	case *syntax.NewExpr:
		p.WriteString(intermediate)
	case *syntax.MemberExpr:
		p.find(n.Object, true)
		if name, ok := memberKey(n); ok {
			if n.Optional {
				p.WriteByte('?')
			}
			p.WriteString("." + name)
			return
		}
		if n.Optional {
			p.WriteString("?.")
		}
		p.WriteByte('[')
		p.find(n.Prop, true)
		p.WriteByte(']')
	case *syntax.OptChain:
		p.WriteString(intermediate)
	case *syntax.SpreadElem:
		p.WriteString("(...")
		p.find(n.X, true)
		p.WriteByte(')')
	case *syntax.ObjectPattern:
		k := len(n.Props)
		if n.Rest != nil {
			k++
		}
		p.WriteString("{" + strings.Repeat(intermediate, k) + "}")
	case *syntax.ArrayPattern:
		p.WriteByte('[')
		for i, x := range n.Elems {
			if i > 0 {
				p.WriteByte(',')
			}
			if x == nil {
				p.WriteString(intermediate)
				continue
			}
			p.find(x, true)
		}
		if n.Rest != nil {
			if len(n.Elems) > 0 {
				p.WriteByte(',')
			}
			p.WriteString("(...")
			p.find(n.Rest, true)
			p.WriteByte(')')
		}
		p.WriteByte(']')
	case *syntax.AssignPattern:
		p.find(n.Target, true)
	}
	// Functions, classes and the unsupported forms print nothing.
}

// operands prints "(x op y op z)".
func (p *calleePrinter) operands(op syntax.Token, xs []syntax.Expr) {
	p.WriteByte('(')
	for i, x := range xs {
		if i > 0 {
			p.WriteString(" " + op.String() + " ")
		}
		p.find(x, true)
	}
	p.WriteByte(')')
}

func (p *calleePrinter) literal(c constant, quote bool) {
	switch c.kind {
	case kNumber:
		p.WriteString(numberString(c.num))
	case kString:
		if quote {
			p.WriteString(`"` + c.str + `"`)
		} else {
			p.WriteString(c.str)
		}
	case kBool:
		p.WriteString(strconv.FormatBool(c.b))
	case kNull:
		p.WriteString("null")
	}
}

// memberKey returns the name printed after a dot: a non-computed name or a
// string literal key.
func memberKey(m *syntax.MemberExpr) (string, bool) {
	if !m.Computed {
		id, ok := m.Prop.(*syntax.Ident)
		if !ok {
			return "", false
		}
		return id.Name, true
	}
	if c, ok := v8Literal(m.Prop); ok && c.kind == kString {
		return c.str, true
	}
	return "", false
}

// v8Literal evaluates e when V8's parser turns it into a literal: a literal,
// a template without substitutions, ! of a literal, + - ~ of a number and
// arithmetic on two numbers.
func v8Literal(e syntax.Expr) (constant, bool) {
	switch e := e.(type) {
	case *syntax.NumberLit:
		return constant{kind: kNumber, num: e.Value}, true
	case *syntax.StringLit:
		return constant{kind: kString, str: e.Value}, true
	case *syntax.BoolLit:
		return constant{kind: kBool, b: e.Value}, true
	case *syntax.NullLit:
		return constant{kind: kNull}, true
	case *syntax.TemplateLit:
		if len(e.Exprs) == 0 {
			return constant{kind: kString, str: e.Quasis[0].Cooked}, true
		}
	case *syntax.UnaryExpr:
		x, ok := v8Literal(e.X)
		if !ok {
			break
		}
		switch {
		case e.Op == syntax.Not:
			return constant{kind: kBool, b: !x.truthy()}, true
		case x.kind != kNumber:
		case e.Op == syntax.Plus:
			return x, true
		case e.Op == syntax.Minus:
			return constant{kind: kNumber, num: -x.num}, true
		case e.Op == syntax.BitNot:
			return constant{kind: kNumber, num: float64(^toInt32(x.num))}, true
		}
	case *syntax.BinaryExpr:
		switch e.Op {
		case syntax.Plus, syntax.Minus, syntax.Mul, syntax.Div, syntax.Rem, syntax.Exp,
			syntax.BitAnd, syntax.BitOr, syntax.BitXor, syntax.Shl, syntax.Shr, syntax.UShr:
			x, okx := v8Literal(e.X)
			y, oky := v8Literal(e.Y)
			if okx && oky && x.kind == kNumber && y.kind == kNumber {
				return foldBinary(e.Op, x, y)
			}
		}
	}
	return constant{}, false
}

// numberString formats v as Number.prototype.toString does.
func numberString(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	case v == 0:
		return "0"
	case v < 0:
		return "-" + numberString(-v)
	}
	mant, exp, _ := strings.Cut(strconv.FormatFloat(v, 'e', -1, 64), "e")
	digits := strings.Replace(mant, ".", "", 1)
	n, _ := strconv.Atoi(exp)
	n++ // v = 0.digits × 10^n
	k := len(digits)
	switch {
	case k <= n && n <= 21:
		return digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits
	}
	s := digits[:1]
	if k > 1 {
		s += "." + digits[1:]
	}
	if n > 0 {
		return s + "e+" + strconv.Itoa(n-1)
	}
	return s + "e-" + strconv.Itoa(1-n)
}

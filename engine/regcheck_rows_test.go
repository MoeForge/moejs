//go:build moejsregcheck

package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rowCorpus runs every op run indexes inline, with operands in distinct
// registers (TestRegCheckRowSensitivity checks that it does).
var rowCorpus = []string{
	// Arithmetic, bitwise, string and comparison operators.
	`function arith(a, b, c) {
		var r = [];
		var x = a + b, y = a - b, z = a * b, w = a / b, m = a % b, e = a ** b;
		r.push(x, y, z, w, m, e, a & b, a | b, a ^ b, a << b, a >> b, a >>> b);
		var n = ~a, g = -a, p = +c, q = !a, s = "s" + a, u = ` + "`t${a}u${b}`" + `;
		r.push(n, g, p, q, s, u, a == b, a != b, a === b, a !== b, a < b, a <= b, a > b, a >= b);
		var i = a, j = i++, k = i--, l = ++i, o = --i, v = a + 1, h = a - 1;
		var t = typeof a, ts = typeof c === "string";
		r.push(i, j, k, l, o, v, h, t, ts);
		return r.join();
	}
	arith(7, 3, "4");`,
	// Conditional jumps and compare-and-branch, register and immediate
	// forms, taken and not.
	`function flow(n, o, u, d) {
		var s = 0;
		for (var i = 0; i < n; i++) s += i;
		for (var i = n; i > 0; i--) s += i;
		for (var i = 0; i <= o; i++) s++;
		for (var i = o; i >= 1; i--) s++;
		for (var i = 0; i < 4; i++) s++;
		for (var i = 4; i > 0; i--) s++;
		for (var i = 0; i <= 4; i++) s++;
		for (var i = 4; i >= 0; i--) s++;
		if (n < o) s++; if (n <= o) s++; if (n > o) s++; if (n >= o) s++;
		if (!(n < o)) s++; if (!(n <= o)) s++; if (!(n > o)) s++; if (!(n >= o)) s++;
		if (n < 4) s++; if (n <= 4) s++; if (n > 4) s++; if (n >= 4) s++;
		if (!(n < 4)) s++; if (!(n <= 4)) s++; if (!(n > 4)) s++; if (!(n >= 4)) s++;
		if (n === 5) s++; if (n !== 5) s++; if (n === o) s++; if (n !== o) s++;
		if (n) s++; if (!o) s++;
		var w = u ?? n, x = d ?? o, y = u?.p, z = d?.length;
		function dflt(q = n, r = o) { return q + r; }
		return [s, w, x, y, z, dflt(), dflt(1, 2)].join();
	}
	flow(5, 3, undefined, "ab"); flow(2, 9, null, "");`,
	// Objects, elements, globals, closures and their environments, strict
	// (sloppy code stores through the Sloppy ops).
	`"use strict";
	var g = 1;
	function objs(k, i) {
		var o = {a: 1, b: 2}, arr = [1, 2, 3], none = null, yes = true, no = false, hole = [, 1];
		o.c = o.a + arr.length;
		arr[i] = o[k];
		arr[i] += 2;
		arr[i]++;
		g = g + o.b;
		var cap = 5;
		var f = function () { cap++; return cap; };
		var fe = function self(n) { return n ? self(n - 1) : self.name; };
		class C { x = o.a; y = arr; }
		var c = new C(), c2 = new C(1, 2);
		var sp = [...arr, ...hole];
		var tu = typeof undeclaredGlobal;
		return [o.c, arr[1], g, f(), f(), fe(2), c.x, c2.y.length, sp.length, none, yes, no, hole.length, tu].join();
	}
	function tdz(c) {
		if (c) {
			try { y; } catch (e) {}
		}
		let y = 2;
		return y;
	}
	objs("a", 1); objs("b", 0); tdz(true);`,
	// Calls: plain, method with and without arguments, constructors,
	// this, returns, undefined locals.
	`function plain() { return 1; }
	function args(a, b, c) { return a + b + c; }
	var o = {m() { return this; }, n(a) { return this.k + a; }, k: 2};
	function Ctor(a, b) { this.s = a + b; }
	function locals() { var p, q, r; return [p, q, r].length; }
	function calls(x) {
		var r = [plain(), args(1, 2, x), o.m() === o, o.n(x), new Ctor(x, 2).s, new Ctor().s, locals()];
		return r.join();
	}
	calls(3);`,
	// Iteration: for-of over arrays and strings, for-in, destructuring and
	// a throw out of a loop.
	`function iter(arr, obj) {
		var s = 0, ks = "";
		for (var v of arr) s += v;
		for (var c of "ab") ks += c;
		for (var k in obj) ks += k;
		var [a, b, ...rest] = arr;
		var {x, y} = obj;
		try { for (var v of arr) { if (v > 1) throw v; } } catch (e) { s += e; }
		var it = arr[Symbol.iterator]();
		try { var [p] = {[Symbol.iterator]() { return {next() { throw 1; }, return() {}}; }}; } catch (e) { s += e; }
		return [s, ks, a, b, rest.length, x, y].join();
	}
	iter([1, 2, 3, 4], {x: 1, y: 2});`,
	// Throw and catch, finally.
	`function thrower(v) { if (v) throw new Error("e" + v); return v; }
	function handles(v) {
		var out = [];
		try { thrower(v); } catch (e) { out.push(e.message); }
		try { try { throw v; } finally { out.push("f"); } } catch (e) { out.push(e); }
		return out.join();
	}
	handles(1); handles(0);`,
}

// rowHandMade are programs the compiler does not emit: Concat with its
// operands in other registers than its result (the compiler concatenates
// in place).
var rowHandMade = []*bytecode.Function{{Name: "concat", NumRegs: 3, Strict: true, Code: []uint32{
	bytecode.EncodeAsBx(bytecode.LoadInt, 1, 4), bytecode.EncodeAsBx(bytecode.LoadInt, 2, 5),
	bytecode.EncodeABC(bytecode.Concat, 0, 1, 2), bytecode.EncodeABC(bytecode.Ret, 0, 0, 0)}}}

// rowRun runs program p of rowCorpus and then rowHandMade and returns the
// message of the panic it ends with, or "".
func rowRun(t *testing.T, p int) (msg string) {
	t.Helper()
	if p < len(rowCorpus) {
		return regcheckRun(t, rowCorpus[p])
	}
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	r := NewRealm()
	fn, _ := testClosure(r, rowHandMade[p-len(rowCorpus)])
	_, err := r.CallObject(fn, Undefined(), nil)
	require.NoError(t, err)
	return ""
}

// inlineOps returns the ops whose case of run's switch indexes the
// registers, from interp_run.go.
func inlineOps(t *testing.T) []bytecode.Op {
	t.Helper()
	byName := map[string]bytecode.Op{}
	for op := bytecode.Op(0); int(op) < bytecode.OpCount; op++ {
		byName[op.String()] = op
	}
	f, err := parser.ParseFile(token.NewFileSet(), "interp_run.go", nil, 0)
	require.NoError(t, err)
	var ops []bytecode.Op
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "run" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			uses := false
			for _, st := range cc.Body {
				ast.Inspect(st, func(n ast.Node) bool {
					if ix, ok := n.(*ast.IndexExpr); ok {
						if id, ok := ix.X.(*ast.Ident); ok && id.Name == "regs" {
							uses = true
						}
					}
					return true
				})
			}
			for _, e := range cc.List {
				if se, ok := e.(*ast.SelectorExpr); ok && uses {
					if id, ok := se.X.(*ast.Ident); ok && id.Name == "bytecode" {
						op, ok := byName[se.Sel.Name]
						require.True(t, ok, se.Sel.Name)
						ops = append(ops, op)
					}
				}
			}
			return true
		})
	}
	require.Greater(t, len(ops), 50, "run's inline ops")
	return ops
}

// rowCut is a way to break a row of the register layouts: drop one of its
// runs, or the first or the last register of a run.
type rowCut int

const (
	cutRun rowCut = iota
	cutFirst
	cutLast
)

func (c rowCut) String() string { return [...]string{"run", "first", "last"}[c] }

// cutRow returns the register layouts with the run-th run of op's row cut.
func cutRow(op bytecode.Op, run int, cut rowCut) func(bytecode.Op, uint32, uint32, func(r, k int)) bool {
	return func(o bytecode.Op, w, x uint32, span func(r, k int)) bool {
		if o != op {
			return bytecode.RegisterSpans(o, w, x, span)
		}
		i := 0
		return bytecode.RegisterSpans(o, w, x, func(r, k int) {
			switch {
			case i != run:
				span(r, k)
			case cut == cutFirst && k > 1:
				span(r+1, k-1)
			case cut == cutLast && k > 1:
				span(r, k-1)
			}
			i++
		})
	}
}

// rowUntouched lists the cuts the checked run cannot see, because run
// reaches those registers other than through its window, or not at all on
// its inline path; each must still pass, so the list stays exact.
var rowUntouched = map[string]string{
	"New/0/last":        "the block's last register is an argument, which run passes as a slice of the stack, or without arguments the slot after the constructor, which it does not read",
	"IterThrow/0/run":   "run reads the thrown value inline; iterOp closes the record through a slice of the frame",
	"IterThrow/0/first": "as IterThrow/0/run",
	"IterThrow/0/last":  "as IterThrow/0/run",
}

// TestRegCheckRowSensitivity checks every row of the register layouts
// that run uses inline: a corpus that runs each of those ops runs under a
// layout with one run of the op's row, or its first or last register, cut
// out, and the checked run must fail on it. The row may name no register
// run does not touch there, but those rowUntouched lists.
func TestRegCheckRowSensitivity(t *testing.T) {
	t.Cleanup(func() { regSpans, regcheckObserve = bytecode.RegisterSpans, nil })
	ops := inlineOps(t)
	// Which programs run each op, and the runs of its rows: how many, and
	// which of them were ever longer than one register.
	type shape struct {
		runs  int
		multi [8]bool
	}
	progs := map[bytecode.Op][]int{}
	shapes := map[bytecode.Op]*shape{}
	for i := range len(rowCorpus) + len(rowHandMade) {
		seen := map[bytecode.Op]bool{}
		regcheckObserve = func(op bytecode.Op, w, x uint32) {
			seen[op] = true
			sh := shapes[op]
			if sh == nil {
				sh = &shape{}
				shapes[op] = sh
			}
			j := 0
			bytecode.RegisterSpans(op, w, x, func(r, k int) {
				if k > 1 && j < len(sh.multi) {
					sh.multi[j] = true
				}
				j++
			})
			sh.runs = max(sh.runs, j)
		}
		require.Empty(t, rowRun(t, i), "program %d", i)
		regcheckObserve = nil
		for op := range seen {
			progs[op] = append(progs[op], i)
		}
	}
	var missing []string
	for _, op := range ops {
		if len(progs[op]) == 0 {
			missing = append(missing, op.String())
		}
	}
	require.Empty(t, missing, "rowCorpus does not run these inline ops")
	tried := map[string]bool{}
	for _, op := range ops {
		sh := shapes[op]
		for run := range sh.runs {
			cuts := []rowCut{cutRun}
			if sh.multi[run] {
				cuts = append(cuts, cutFirst, cutLast)
			}
			for _, cut := range cuts {
				name := fmt.Sprintf("%s/%d/%s", op, run, cut)
				tried[name] = true
				t.Run(name, func(t *testing.T) {
					regSpans = cutRow(op, run, cut)
					var msg string
					for _, p := range progs[op] {
						if msg = rowRun(t, p); msg != "" {
							break
						}
					}
					regSpans = bytecode.RegisterSpans
					if why, ok := rowUntouched[name]; ok {
						assert.Empty(t, msg, "%s is listed as untouched (%s)", name, why)
						return
					}
					require.NotEmpty(t, msg, "the cut went unnoticed")
					assert.Contains(t, msg, "engine: regcheck: ")
				})
			}
		}
	}
	for name := range rowUntouched {
		assert.True(t, tried[name], "rowUntouched lists %s, which the test never cuts", name)
	}
}

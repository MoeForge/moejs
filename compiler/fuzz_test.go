package compiler

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
)

// FuzzCompile compiles whatever parses, as a module or as a script. A
// compile error must be a positioned *Error carrying a SyntaxError (an
// "internal:" message is a compiler bug); a compiled template must pass
// fuzzVerify and compile to the same bytecode a second time.
func FuzzCompile(f *testing.F) {
	for _, s := range []string{
		"export function run() { return 1 + 2; }",
		"let a = 1; const b = a ? 'x' : `y${a}`; a ??= b;",
		"function f(a, b = 1, ...c) { try { return a(b) } catch { return c } finally { a = 0 } }",
		"for (let i = 0; i < 3; i++) { const g = () => i; if (i) continue; else break; }",
		"l: for (const k in {a: 1}) { for (const v of [k]) { if (v) break l; } }",
		"const {x, y: [z = 1, ...w], ...r} = {x: 1, y: []}; ({x} = {x: 2});",
		"switch (1) { case 0: let q; break; default: q = 2 }",
		"export default class A { #p = 1; static s = 2; m() { return this.#p } }",
		"function* g() { yield 1; yield* [2]; } const it = g(); it.next();",
		"const o = {a, b() {}, get c() { return 1 }, [\"d\"]: 2, ...{e: 3}}; delete o.a;",
		"x?.y?.[0]?.(1); typeof z === 'undefined'; 1 ** 2 ** 3; a = b = c;",
		"let k = 'm'; const f = () => k; o[k](...a); o[f()](...a, 1); o?.[k](...a); o[k + 1](a, b);",
		"const r = /a+(?<n>b)/gu; 'ab'.replace(r, '$<n>');",
		"'a' + 'b' + 1 + 2 - 3 * 4 / 5 % 6 << 1 >> 2 >>> 3 & 4 | 5 ^ 6;",
		"while (true) { do { throw new Error('x') } while (false) }",
		"function f(a, g = () => arguments) { for (const x of arguments) a += x; return [...arguments, g().length]; }",
		"const o = { t(s, ...v) { return s.raw.join() + v; } }; o.t`a${1}\\u{zz}b${o}`; new o.t`x`; (s => s)``;",
		"const \\u0061b = { \\u0069f: 1, c\\u{61}t: 2 }; export const π = \\u0061b.\\u0069f + ab.cat; function \\u{66}(\\u0061rg) { return arguments.length + arg; }",
		"class A { static #c = 0; #m() { return #m in this; } get #g() { return 1 } static { A.#c++; } [`k${1}`] = () => this; }",
		"class B extends A { x = super.m(); constructor(a, ...b) { const f = () => super(...b); f(); return new.target ? undefined : {}; } }",
		"export const C = class extends (0, Object) { static s = class {}; m() { super.x += 1; [super.y] = [2]; delete super.z; } };",
		"function f(it) { for (const x of it) { for (let [a, b] of x) { if (a) return; if (b) continue; } } }",
		"const [a, [b, ...c] = [], ...d] = new Set([1]); for ([x.y, ...z[0]] of new Map([[1, 2]])) try { return; } finally {}",
		"export async function run(a) { try { for await (const [k, v = await k] of a) { if (v) break; } } finally { await null; } }",
		"async function* g() { try { yield* h(); yield await 1; } catch (e) { return yield e; } finally { for await (const x of []) continue; } }",
		"export const v = await Promise.resolve(1); for await (const x of [v]) try { await x; } catch { break; } finally { await 2; }",
		"const o = { async m() { return super.m?.(await 1); }, async *[Symbol.asyncIterator]() { yield* [1]; } }; class C { static async #s() {} }",
	} {
		f.Add(s, true)
		f.Add(s, false)
	}
	files, _ := filepath.Glob("../bench/corpus/*.js")
	for _, p := range files {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(string(b), true)
		}
	}
	f.Fuzz(func(t *testing.T, src string, module bool) {
		compile := func() (*bytecode.Function, error) {
			if module {
				m, err := syntax.ParseModule("fuzz.js", src, syntax.Options{})
				if err != nil {
					return nil, nil
				}
				return CompileModule(m)
			}
			s, err := syntax.ParseScript("fuzz.js", src, syntax.Options{})
			if err != nil {
				return nil, nil
			}
			return CompileScript(s)
		}
		fn, err := compile()
		if err != nil {
			ce, ok := err.(*Error)
			if !ok {
				t.Fatalf("error is %T, want *compiler.Error: %v", err, err)
			}
			if !strings.HasPrefix(ce.Msg, "SyntaxError: ") || ce.Name != "fuzz.js" || ce.Pos < 0 || ce.Pos > len(src) || ce.Line < 1 || ce.Col < 1 {
				t.Fatalf("malformed compile error %#v", ce)
			}
			return
		}
		if fn == nil {
			return // does not parse
		}
		fuzzVerify(t, fn, "top")
		fn2, err := compile()
		if err != nil {
			t.Fatalf("second compile failed: %v", err)
		}
		if d1, d2 := bytecode.Disassemble(fn), bytecode.Disassemble(fn2); d1 != d2 {
			t.Fatalf("compilation is not deterministic:\n%s\n---\n%s", d1, d2)
		}
	})
}

// fuzzVerify is a structural bytecode check: known opcodes whose extra words
// fit, jumps onto instruction boundaries, constant and child indices in
// range, handler rows inside the code, and an ordered, positioned line
// table. Register operands are not checked (some ops use A for counts).
func fuzzVerify(t *testing.T, fn *bytecode.Function, path string) {
	t.Helper()
	code := fn.Code
	if fn.NumRegs > 256 {
		t.Fatalf("%s: %d registers", path, fn.NumRegs)
	}
	starts := make([]bool, len(code)+1)
	var jumps []int
	for pc := 0; pc < len(code); {
		w := code[pc]
		op := bytecode.DecodeOp(w)
		if int(op) >= bytecode.OpCount || strings.HasPrefix(op.String(), "Op(") {
			t.Fatalf("%s: pc %d: unknown opcode %d", path, pc, op)
		}
		starts[pc] = true
		switch op.Format() {
		case bytecode.FmtAsBx:
			if op != bytecode.LoadInt {
				jumps = append(jumps, pc+1+int(bytecode.DecodeSBx(w)))
			}
		case bytecode.FmtSBx:
			jumps = append(jumps, pc+1+int(bytecode.DecodeSBx(w)))
		case bytecode.FmtABx:
			bx := int(bytecode.DecodeBx(w))
			switch op {
			case bytecode.LoadConst, bytecode.NewRegExp:
				if bx >= len(fn.Consts) {
					t.Fatalf("%s: pc %d: %s K%d of %d constants", path, pc, op, bx, len(fn.Consts))
				}
			case bytecode.Closure:
				if bx >= len(fn.Children) {
					t.Fatalf("%s: pc %d: Closure F%d of %d children", path, pc, bx, len(fn.Children))
				}
			}
		}
		if op == bytecode.GetTemplate && pc+1 < len(code) {
			x := code[pc+1]
			k, ic := int(bytecode.ExtraLo(x)), bytecode.ExtraHi(x)
			if k >= len(fn.Consts) || fn.Consts[k].Kind != bytecode.ConstTemplate || len(fn.Consts[k].Cooked) != len(fn.Consts[k].Raw) || uint32(ic) >= fn.ICCount {
				t.Fatalf("%s: pc %d: GetTemplate K%d ic%d of %d constants, %d caches", path, pc, k, ic, len(fn.Consts), fn.ICCount)
			}
		}
		pc += 1 + op.ExtraWords()
		if pc > len(code) {
			t.Fatalf("%s: %s extra words run past the end of the code", path, op)
		}
	}
	starts[len(code)] = true
	for _, to := range jumps {
		if to < 0 || to >= len(code) || !starts[to] {
			t.Fatalf("%s: jump to pc %d is not an instruction (code has %d words)", path, to, len(code))
		}
	}
	for _, h := range fn.Handlers {
		if h.Start > h.End || int(h.End) > len(code) || int(h.Handler) >= len(code) || !starts[h.Handler] {
			t.Fatalf("%s: handler %+v outside the %d-word code", path, h, len(code))
		}
	}
	for i, e := range fn.LineTable {
		if int(e.PC) >= len(code) || e.Line < 1 || e.Col < 1 || i > 0 && e.PC < fn.LineTable[i-1].PC {
			t.Fatalf("%s: line table entry %d %+v", path, i, e)
		}
	}
	for i, c := range fn.Children {
		fuzzVerify(t, c, path+"/"+strconv.Itoa(i))
	}
}

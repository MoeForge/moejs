package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
)

// fuzzBytecodeSeeds are programs with control flow of every kind the
// verifier walks: loops of each compare form, conditionals, try, catch and
// finally, generators, async functions, iteration, with and direct eval.
var fuzzBytecodeSeeds = []string{
	"function f(n) { var s = 0; for (var i = 0; i < n; i++) s += i; for (var j = n; j > 0; j--) s -= j; return s; } f(10);",
	"function f(a, b) { if (a < 7) return a; if (a === b) return 0; if (!(b >= 3)) return b; return a ?? b; } [f(1, 2), f(9, 9), f(9, 1)].join();",
	"function f(o) { try { if (o.x) throw o.x; return 1; } catch (e) { return e; } finally { o.y = 2; } } f({x: 3}); f({});",
	"function* g(n) { try { for (let i = 0; i < n; i++) yield i; } finally { yield -1; } } [...g(3)].join();",
	"async function f(x) { await null; try { await Promise.reject(x); } catch (e) { return e + 1; } } f(1);",
	"function f(a) { var s = ''; for (const v of a) s += v; for (const k in {p: 1, q: 2}) s += k; const [x, ...y] = a; return s + x + y.length; } f([1, 2, 3]);",
	"function f(o) { with (o) { return x + (typeof y); } } f({x: 1});",
	"function f(s) { var v = 1; return eval(s); } f('v + 1');",
	"var o = {m() { return this.k; }, k: 3}; function f(a, b, c) { return o.m() + a + b + c; } f(1, 2, 3); new (class C { x = 1; constructor(a) { this.a = a; } })(2);",
	"function f(x) { switch (x) { case 1: return 'a'; case 2: return 'b'; default: return x?.y ?? 'c'; } } [f(1), f(2), f({y: 4})].join();",
	"label: for (var i = 0; i < 3; i++) { for (var j = 0; j < 3; j++) { if (j == 1) continue label; if (i == 2) break label; } }",
	"async function* g() { yield 1; yield await Promise.resolve(2); } (async () => { let s = 0; for await (const v of g()) s += v; return s; })();",
	"export let n = 0; export function inc(by = 1) { n += by; return n; } for (let i = 0; i < 4; i++) inc(i);",
	"export async function run() { for (let i = 0; i < 3; i++) await i; return [1, 2].map(x => x * 2); } await run();",
	// Module-level generator, async and async generator declarations,
	// which the module's prologue completes (runModulePrologue).
	"export function* g(n) { for (let i = 0; i < n; i++) yield i; } export async function a(x) { return await x; } export async function* ag() { yield 1; } function* h() { yield* g(2); } let s = 0; for (const v of h()) s += v; a(s); ag().next();",
	"export let s = [...g(2)].length; function* g(n) { yield n; yield n + 1; } async function* ag() { yield await 1; } for await (const v of ag()) s += v; export async function a() { return s; } async function b() { return a(); } await b();",
}

// FuzzBytecode mutates the bytecode the compiler made of a fuzzed script
// or module and runs what bytecode.VerifyRegisters accepts, so that the
// verifier meets bytecode no compiler emits: jumps off its walk, operands
// past NumRegs, handlers anywhere, NumRegs too small, operands of another
// kind than the compiler gives them. (FuzzRun mutates source, so it only
// reaches what the compiler emits.) In the checked build (go test -tags
// moejsregcheck) every register access and every pc the run dispatches is
// compared with what the verifier checked, and every payload read with
// its kind (value_as_regcheck.go): a panic of either is a hole, a function
// the verifier let through that would read or write past its window, or
// an op that reads an operand as a kind it did not check (Realm.wrongKind),
// or that the verifier let through unproven (bytecode.KindOperands); the
// checked build also compares what each instruction writes with
// bytecode.RegisterWrites, the rows the proof takes it from.
// Any other panic is the engine meeting a value no compiler would give it
// that Go caught (a type assertion, an index), and is not a failure. A
// program that could loop without reaching an interrupt check (a backward
// jump other than Jmp's and the compare-and-branch ops', a handler before
// its range's end) is skipped.
//
// muts is a list of five-byte edits: function (in pre-order), word index
// (two bytes), what to change, and the value (fuzzEdit: besides words, a
// handler's pc, range, register and kind, NumRegs up to 65535 and a
// module body's pc). A module whose muts has a byte past its last edit is
// evaluated as a one-module graph (LinkModules, EvaluateGraph), whose
// instantiation runs the module's prologue outside run
// (runModulePrologue); any other as EvaluateModule does, in run.
//
// With FUZZ_CRASH_DIR set, each fuzzing process writes the output of a
// fatal error or an unrecovered panic to crash-<pid>.txt there: the
// coordinator reports a process that dies without what it printed.
func FuzzBytecode(f *testing.F) {
	if dir := os.Getenv("FUZZ_CRASH_DIR"); dir != "" {
		if out, err := os.Create(filepath.Join(dir, "crash-"+strconv.Itoa(os.Getpid())+".txt")); err == nil {
			debug.SetCrashOutput(out, debug.CrashOptions{})
			out.Close()
		}
	}
	edits := [][]byte{
		{},
		{0, 2, 0, 4, 1},                        // a jump nudged by one, or a word's high bits
		{0, 1, 0, 0, 0x2c},                     // another op
		{0, 3, 0, 1, 200},                      // A past the window
		{1, 4, 0, 2, 7, 1, 5, 0, 3, 9},         // B and C of a child
		{0, 0, 0, 7, 1},                        // NumRegs
		{1, 0, 0, 6, 0xff},                     // a handler moved back
		{0, 0, 0, 0, byte(bytecode.GenFunc)},   // the first word, a module prologue's Closure, made GenFunc
		{0, 1, 0, 0, byte(bytecode.AsyncFunc)}, // its GenFunc made AsyncFunc
		{0, 0, 3, 6, 2},                        // a handler of kind 2, which run enters as a finally
		{0, 0, 4, 6, 1},                        // a handler's register moved up by one
		{0, 1, 0, 7, 44},                       // 300 registers
		{0, 3, 0, 7, 0xff},                     // a module body's pc moved back by one
	}
	for _, s := range fuzzBytecodeSeeds {
		module := strings.Contains(s, "export")
		for _, e := range edits {
			f.Add(s, module, e)
			if module {
				f.Add(s, module, append(e[:len(e):len(e)], 0)) // as a graph
			}
		}
	}
	files, _ := filepath.Glob("../bench/corpus/*.js")
	for _, p := range files {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(string(b), true, edits[1])
		}
	}
	old := maxStringLength
	maxStringLength = 1 << 22
	f.Cleanup(func() { maxStringLength = old })
	f.Fuzz(func(t *testing.T, src string, module bool, muts []byte) {
		if len(src) > 4096 || len(muts) > 200 {
			return
		}
		code := fuzzCompile(t, src, module)
		if code == nil {
			return
		}
		fns := fuzzTree(code, nil)
		fuzzEdit(fns, muts)
		if !fuzzEnds(fns) {
			return
		}
		r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
		defer func() {
			if p := recover(); p != nil {
				if msg := fmt.Sprint(p); strings.HasPrefix(msg, "engine: regcheck") || strings.HasPrefix(msg, "engine: payload") {
					t.Fatalf("%s\n%s", msg, bytecode.Disassemble(code))
				}
			}
		}()
		fuzzWithDeadline(r, func() error {
			if module && len(muts)%5 != 0 {
				g, err := LinkModules(code, LinkOptions{})
				if err == nil {
					_, _, err = r.EvaluateGraph(g)
				}
				return err
			}
			if module {
				_, err := r.EvaluateModule(code)
				return err
			}
			_, err := r.RunScript(code)
			return err
		})
	})
}

// fuzzTree appends fn and its descendants in pre-order.
func fuzzTree(fn *bytecode.Function, out []*bytecode.Function) []*bytecode.Function {
	out = append(out, fn)
	for _, c := range fn.Children {
		out = fuzzTree(c, out)
	}
	return out
}

// fuzzEdit applies the five-byte edits of muts to fns.
func fuzzEdit(fns []*bytecode.Function, muts []byte) {
	for ; len(muts) >= 5; muts = muts[5:] {
		fn, what, v := fns[int(muts[0])%len(fns)], muts[3]%8, muts[4]
		switch what {
		case 6: // a handler's pc, range or register moved by a signed byte, or its kind
			if len(fn.Handlers) == 0 {
				continue
			}
			h, d := &fn.Handlers[int(muts[1])%len(fn.Handlers)], uint32(int32(int8(v)))
			switch muts[2] % 5 {
			case 0:
				h.Handler += d
			case 1:
				h.Start += d
			case 2:
				h.End += d
			case 3: // a catch, a finally, or a kind run enters as a finally
				h.Kind = bytecode.HandlerKind(v % 4)
			default:
				h.Reg += uint16(d)
			}
			continue
		case 7: // NumRegs, past the 256 of the window too, or a module body's pc
			switch muts[1] % 4 {
			case 0:
				fn.NumRegs = uint16(v)
			case 1:
				fn.NumRegs = 256 + uint16(v)
			case 2:
				fn.NumRegs = uint16(v)<<8 | 0xff
			default:
				if fn.Module != nil {
					fn.Module.BodyPC += int(int8(v))
				}
			}
			continue
		}
		if len(fn.Code) == 0 {
			continue
		}
		i := (int(muts[1]) | int(muts[2])<<8) % len(fn.Code)
		w := &fn.Code[i]
		switch what {
		case 0: // another op, the same operands
			*w = *w&^0xff | uint32(int(v)%bytecode.OpCount)
		case 1, 2, 3: // A, B or C
			s := 8 * uint(what)
			*w = *w&^(0xff<<s) | uint32(v)<<s
		case 4: // the sBx (a jump's offset) moved by a signed byte
			*w = *w&0xffff | uint32(uint16(int16(*w>>16)+int16(int8(v))))<<16
		case 5: // bits of any byte
			*w ^= uint32(v) << (8 * uint(i%4))
		}
	}
}

// fuzzEnds reports whether every backward transfer of fns reaches an
// interrupt check: the backward jumps are Jmp's and the compare-and-branch
// ops' (run checks the interrupt flag on those), and every handler lies
// after its range. Any other loop could run past the fuzz deadline.
func fuzzEnds(fns []*bytecode.Function) bool {
	for _, fn := range fns {
		for pc := 0; pc < len(fn.Code); {
			w := fn.Code[pc]
			op := bytecode.DecodeOp(w)
			if int(op) >= bytecode.OpCount {
				pc++
				continue
			}
			checks := op == bytecode.Jmp || op >= bytecode.JmpLt && op <= bytecode.JmpNGe || op >= bytecode.JmpLtI && op <= bytecode.JmpNGeI
			if op.Jumps() && bytecode.JumpTarget(pc, w) <= pc && !checks {
				return false
			}
			pc += 1 + op.ExtraWords()
		}
		for _, h := range fn.Handlers {
			if h.Handler < h.End {
				return false
			}
		}
	}
	return true
}

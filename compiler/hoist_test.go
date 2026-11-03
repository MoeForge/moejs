package compiler

import (
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outerBackEdge returns the target and the pc of fn's backward jump that
// reaches furthest back, the back edge of its outermost loop: a Jmp or the
// conditional jump of a loop's condition (rotated loops), or -1, -1.
func outerBackEdge(fn *bytecode.Function) (start, end int) {
	start, end = -1, -1
	for pc := 0; pc < len(fn.Code); {
		w := fn.Code[pc]
		op := bytecode.DecodeOp(w)
		if f := op.Format(); f == bytecode.FmtSBx || f == bytecode.FmtAsBx && op != bytecode.LoadInt {
			if off := int(bytecode.DecodeSBx(w)); off < 0 && (start < 0 || pc+1+off < start) {
				start, end = pc+1+off, pc
			}
		}
		pc += 1 + op.ExtraWords()
	}
	return start, end
}

// loopLoads counts the constant loads (LoadConst, LoadInt) inside fn's
// outermost loop, from the target of its last back edge to that edge, and
// before it.
func loopLoads(t *testing.T, fn *bytecode.Function) (inside, before int) {
	t.Helper()
	start, end := outerBackEdge(fn)
	require.GreaterOrEqual(t, start, 0, "no loop")
	for pc := 0; pc < len(fn.Code); {
		op := bytecode.DecodeOp(fn.Code[pc])
		if op == bytecode.LoadConst || op == bytecode.LoadInt {
			switch {
			case pc >= start && pc <= end:
				inside++
			case pc < start:
				before++
			}
		}
		pc += 1 + op.ExtraWords()
	}
	return inside, before
}

func TestHoistIntLoop(t *testing.T) {
	fn := child(t, compileModule(t, `export function intLoop() {
  let s = 0;
  for (let i = 0; i < 1000000; i++) s = (s + (i ^ (i >>> 3)) * 7) % 1000003;
  return s;
}`), "intLoop")
	inside, before := loopLoads(t, fn)
	assert.Equal(t, 0, inside, bytecode.Disassemble(fn))
	assert.Equal(t, 6, before, bytecode.Disassemble(fn)) // s = 0, i = 0 and the four constants
}

func TestHoistNested(t *testing.T) {
	fn := child(t, compileModule(t, `export function f(n, a) {
  let s = 0;
  for (let i = 0; i < n; i++)
    for (let j = 0; j < n; j++) {
      let k = 0;
      while (k < 3000) { s = (s + a[k] * 1000) % 65537; k += 1000; }
    }
  return s;
}`), "f")
	// The inner loops' constants are loaded before the outer one; what is
	// left inside are the inits of j and k.
	inside, before := loopLoads(t, fn)
	assert.Equal(t, 2, inside, bytecode.Disassemble(fn))
	assert.Equal(t, 5, before, bytecode.Disassemble(fn)) // s = 0, i = 0, 3000, 65537 and 1000
}

func TestHoistNestedFunction(t *testing.T) {
	m := compileModule(t, `export function f(n) {
  const out = [];
  for (let i = 0; i < n; i++) out.push(x => { let t = 0; for (let j = 0; j < x; j++) t = (t + j * 1000) % 65537; return t; });
  return out;
}`)
	f := child(t, m, "f")
	inner := f.Children[0]
	in, before := loopLoads(t, inner)
	assert.Equal(t, 0, in, bytecode.Disassemble(inner))
	assert.Equal(t, 4, before, bytecode.Disassemble(inner)) // t = 0, j = 0, 1000, 65537
}

func TestHoistSkips(t *testing.T) {
	fn := child(t, compileModule(t, `export function f(a, x) {
  for (let i = 0; i < a.length; i++) {
    if (typeof a[i] === "string") return "found" + 1000;
    if (a[i] === null) throw new Error("bad");
    x = x + 1 - 2;
  }
  return x;
}`), "f")
	inside, before := loopLoads(t, fn)
	// The typeof test and the immediates load nothing, and what the return
	// and the throw load stays where they are.
	assert.Equal(t, 3, inside, bytecode.Disassemble(fn)) // "found", 1000, "bad"
	assert.Equal(t, 1, before, bytecode.Disassemble(fn)) // i = 0
}

func TestHoistMostUsed(t *testing.T) {
	var b strings.Builder
	b.WriteString("export function f(a, n) {\n  let s = 0;\n  for (let i = 0; i < n; i++) {\n")
	for k := range maxHoist + 4 {
		b.WriteString("    s = s * " + strconv.Itoa(1000+k) + ";\n")
	}
	b.WriteString("    for (let j = 0; j < n; j++) s = s % 99991;\n  }\n  return s;\n}")
	fn := child(t, compileModule(t, b.String()), "f")
	d := bytecode.Disassemble(fn)
	inside, before := loopLoads(t, fn)
	// Kept: the inner loop's 99991, which weighs 4, and the first
	// maxHoist-1 of the outer loop's; s = 0 and i = 0 load before too.
	assert.Equal(t, maxHoist+2, before, d)
	// Left: the other outer constants and j = 0.
	assert.Equal(t, (maxHoist+4)-(maxHoist-1)+1, inside, d)
}

// TestHoistRegisterLimit compiles a loop whose body needs nearly every
// register: a function that runs out of registers with constants hoisted
// compiles again without hoisting in it, and only in it.
func TestHoistRegisterLimit(t *testing.T) {
	src := func(nargs int) string {
		args := make([]string, nargs)
		for i := range args {
			args[i] = "i"
		}
		return `export function g(n, h) {
  let s = 0;
  for (let i = 0; i < n; i++) { s = (s + i * 3000) % 7919; h(` + strings.Join(args, ", ") + `); }
  return s;
}
export function k(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + i * 3000) % 7919; return s; }`
	}
	compile := func(nargs int) (*bytecode.Function, error) {
		m, err := syntax.ParseModule("t.js", src(nargs), syntax.Options{})
		require.NoError(t, err)
		return CompileModule(m)
	}
	// The most arguments that compile at all.
	most := 0
	for n := 200; n < 300; n++ {
		if _, err := compile(n); err != nil {
			require.Contains(t, err.Error(), "more than 256 registers")
			break
		}
		most = n
	}
	require.Greater(t, most, 200)
	m, err := compile(most)
	require.NoError(t, err)
	g := child(t, m, "g")
	assert.Equal(t, 256, int(g.NumRegs))
	in, _ := loopLoads(t, g)
	assert.Equal(t, 2, in, "no hoisting in g at the limit") // 3000 and 7919
	in, _ = loopLoads(t, child(t, m, "k"))
	assert.Equal(t, 0, in, "k still hoists")
	// Further from the limit g hoists.
	m, err = compile(most - 4)
	require.NoError(t, err)
	in, _ = loopLoads(t, child(t, m, "g"))
	assert.Equal(t, 0, in)
}

// TestHoistForOf checks that a for-of loop starts no nest of its own but
// uses the constants of a loop around it.
func TestHoistForOf(t *testing.T) {
	m := compileModule(t, `export function f(xs) { let s = 0; for (const x of xs) s = (s + x * 1000) % 65537; return s; }
export function g(xs, n) {
  let s = 0;
  for (let i = 0; i < n; i++) for (const x of xs) s = (s + x * 1000) % 65537;
  return s;
}`)
	in, _ := loopLoads(t, child(t, m, "f"))
	assert.Equal(t, 2, in)
	in, before := loopLoads(t, child(t, m, "g"))
	assert.Equal(t, 0, in)
	assert.Equal(t, 4, before) // s = 0, i = 0, 1000, 65537
}

// TestHoistStateRecycled checks that a compilation leaves its hoisting
// state for the next one holding nothing of its own: no function states,
// no keys, no noHoist list, also after a compile that failed or retried.
func TestHoistStateRecycled(t *testing.T) {
	clean := func(name string) {
		t.Helper()
		h := spareHoist.Load()
		require.NotNil(t, h, name)
		assert.Empty(t, h.regs, name)
		for _, r := range h.regs[:cap(h.regs)] {
			assert.Equal(t, hoistReg{}, r, name)
		}
		for _, c := range h.scan.cands[:cap(h.scan.cands)] {
			assert.Equal(t, hoistCand{}, c, name)
		}
		assert.Nil(t, h.noHoist, name)
	}
	src := `export function f(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + i * 1000) % 65537; return s; }`
	compileModule(t, src)
	clean("compiled")
	h := spareHoist.Load()
	compileModule(t, src)
	assert.Same(t, h, spareHoist.Load(), "reused")
	limit := func(nargs int) error {
		args := strings.TrimSuffix(strings.Repeat("i, ", nargs), ", ")
		m, err := syntax.ParseModule("t.js", `export function g(n, h) { let s = 0; for (let i = 0; i < n; i++) { s = (s + i * 3000) % 7919; h(`+args+`); } return s; }`, syntax.Options{})
		require.NoError(t, err)
		_, err = CompileModule(m)
		return err
	}
	require.NoError(t, limit(250)) // through the retry without hoisting in g
	clean("retried")
	require.ErrorContains(t, limit(252), "more than 256 registers") // retried, then failed
	clean("failed")
}

// TestHoistScanCap checks that a scan counts the first maxHoistCands
// distinct literals of a nest and no more: a literal past them is not
// hoisted however often the loop uses it.
func TestHoistScanCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("export function f(y, n) {\n  let x = 0;\n  for (let i = 0; i < n; i++) {\n")
	for k := range maxHoistCands + 6 {
		b.WriteString("    x = y * " + strconv.Itoa(1000+k) + ";\n")
	}
	late := strconv.Itoa(1000 + maxHoistCands + 5)
	for range 50 {
		b.WriteString("    x = y * " + late + ";\n")
	}
	b.WriteString("  }\n  return x;\n}")
	fn := child(t, compileModule(t, b.String()), "f")
	inside, before := loopLoads(t, fn)
	assert.Equal(t, maxHoist+2, before)                  // the first maxHoist, x = 0 and i = 0
	assert.Equal(t, maxHoistCands+6-maxHoist+50, inside) // the rest, the late one every time
}

// TestHoistScanLinear pins the scan's linear cost in the number of
// distinct literals of a nest: one loop of 16n of them must compile in
// about the time of sixteen loops of n (as TestFoldChainLinear measures);
// counting every distinct literal made it quadratic.
func TestHoistScanLinear(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := func(n int) string {
		var b strings.Builder
		b.WriteString("export function f(y, n) { let x = 0; for (let i = 0; i < n; i++) {\n")
		for k := range n {
			b.WriteString("x = y * " + strconv.Itoa(1000+k) + ";\n")
		}
		b.WriteString("} return x; }")
		return b.String()
	}
	parse := func(n int) *syntax.Module {
		m, err := syntax.ParseModule("loop.js", src(n), syntax.Options{})
		require.NoError(t, err)
		return m
	}
	compile := func(ms ...*syntax.Module) time.Duration {
		return cpuTime(func() {
			for _, m := range ms {
				if _, err := CompileModule(m); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	const k, n = 16, 1000
	one, many := time.Duration(1<<62), time.Duration(1<<62)
	for range 3 {
		small := make([]*syntax.Module, k)
		for i := range small {
			small[i] = parse(n)
		}
		many = min(many, compile(small...))
		one = min(one, compile(parse(k*n)))
	}
	t.Logf("1 x %d literals %v, %d x %d literals %v", k*n, one, k, n, many)
	assert.Less(t, one, 4*many+2*time.Millisecond, "compile time grows quadratically with a loop's distinct literals")
}

// readsA lists the ops whose A operand, when they have one, is only read:
// a hoisted register may appear there. Every other op writes R[A].
var readsA = map[bytecode.Op]bool{
	bytecode.SetEnv: true, bytecode.SetEnvW: true, bytecode.CheckTDZ: true,
	bytecode.SetGlobal: true, bytecode.SetGlobalSloppy: true, bytecode.SetGlobalRef: true, bytecode.SetGlobalVar: true, bytecode.InitGlobal: true,
	bytecode.SetProp: true, bytecode.SetPropSloppy: true, bytecode.SetElem: true, bytecode.SetElemSloppy: true,
	bytecode.SetSuper: true, bytecode.SetPrivate: true, bytecode.DefPrivate: true, bytecode.AddBrand: true,
	bytecode.DefineField: true, bytecode.DefineElem: true, bytecode.DefineMethod: true, bytecode.DefineAccessor: true,
	bytecode.SetProto: true, bytecode.CopyDataProps: true, bytecode.CopyDataPropsEx: true,
	bytecode.ArrayPush: true, bytecode.ArrayHole: true, bytecode.AppendSpread: true, bytecode.RequireObjectCoercible: true,
	bytecode.JmpT: true, bytecode.JmpF: true, bytecode.JmpNullish: true, bytecode.JmpNotNullish: true, bytecode.JmpNotUndef: true,
	bytecode.JmpWith: true, bytecode.JmpLt: true, bytecode.JmpLe: true, bytecode.JmpGt: true, bytecode.JmpGe: true,
	bytecode.JmpNLt: true, bytecode.JmpNLe: true, bytecode.JmpNGt: true, bytecode.JmpNGe: true,
	bytecode.JmpStrictEq: true, bytecode.JmpStrictNe: true,
	bytecode.Ret: true, bytecode.Throw: true, bytecode.IterClose: true, bytecode.IterThrow: true, bytecode.WithSet: true,
	bytecode.PushEnv: true, // A unused
}

// outerLoop returns the first for, while or do-while loop of fn's body
// outside nested functions.
func outerLoop(fn *syntax.Function) syntax.Stmt {
	var loop syntax.Stmt
	syntax.Inspect(fn.Body, func(n syntax.Node) bool {
		if loop != nil {
			return false
		}
		switch n := n.(type) {
		case *syntax.Function, *syntax.Class:
			return false
		case *syntax.ForStmt, *syntax.WhileStmt, *syntax.DoWhileStmt:
			loop = n.(syntax.Stmt)
			return false
		}
		return true
	})
	return loop
}

// checkHoistedOnlyRead checks the nest of fn's outermost loop: the
// constants the scan keeps are loaded by the instructions right before the
// loop, and no instruction of the loop writes their registers.
func checkHoistedOnlyRead(t *testing.T, src string, fn *bytecode.Function, sf *syntax.Function) {
	t.Helper()
	d := bytecode.Disassemble(fn)
	var sc hoistScan
	sc.loop(outerLoop(sf))
	keep := sc.keep()
	require.NotEmpty(t, keep, "%s: nothing hoisted\n%s", src, d)
	start, end := outerBackEdge(fn)
	// The loads come before the loop, and before the Jmp that enters a
	// for or while loop at its condition.
	first := start
	if w := fn.Code[max(start-1, 0)]; start > 0 && bytecode.DecodeOp(w) == bytecode.Jmp && bytecode.DecodeSBx(w) > 0 {
		first--
	}
	require.GreaterOrEqual(t, first, len(keep), d)
	hoisted := map[int]bool{}
	for i := range keep {
		w := fn.Code[first-len(keep)+i]
		op := bytecode.DecodeOp(w)
		require.True(t, op == bytecode.LoadConst || op == bytecode.LoadInt, "%s: pc %d is %s, not a hoisted load\n%s", src, first-len(keep)+i, op, d)
		hoisted[int(bytecode.DecodeA(w))] = true
	}
	for pc := start; pc <= end; {
		w := fn.Code[pc]
		op := bytecode.DecodeOp(w)
		if f := op.Format(); f != bytecode.FmtNone && f != bytecode.FmtSBx && !readsA[op] {
			assert.False(t, hoisted[int(bytecode.DecodeA(w))], "%s: pc %d %s writes a hoisted register\n%s", src, pc, op, d)
		}
		if op == bytecode.GetElemRef { // writes the converted key into C too
			assert.False(t, hoisted[int(bytecode.DecodeC(w))], "%s: pc %d GetElemRef writes a hoisted register\n%s", src, pc, d)
		}
		pc += 1 + op.ExtraWords()
	}
}

// TestHoistedRegistersOnlyRead uses hoisted constants in every position an
// operand reaches, in modules, scripts, generators and async functions,
// and checks that no instruction of a nest writes a hoisted register.
func TestHoistedRegistersOnlyRead(t *testing.T) {
	body := `
    s = (s + i * 1000) % 65537;
    t = t + "k" + i;
    if (i === 1000 || i !== -3 || o["k"] === 1000 || "k" in o) s ^= 1000;
    a[1000] = a[1000] + 1000;
    a[1000] += 1000;
    a[1000]++;
    a["k"] -= 1000;
    delete a[1000];
    o.k = 1000 - i;
    o[i % 1000] = "k";
    switch (i * 1000) { case 1000: s++; break; case 65537: s--; }
    const [d = 1000] = xs;
    const {k = 1000} = o;
    s += d * k;
    s = typeof s === "number" ? s : 1000;
    s = s instanceof Object ? 1000 : s;
    s = Math.max(s, 1000, ...xs);
    s = [s, 1000, "k"].length + s;
    s = {k: 1000, [1000]: s}.k + s;
    s = ` + "`${1000}${s}`" + ` | 0;
    t = t || "k";
    s = s ?? 1000;
    for (const x of xs) s = (s + x * 1000) % 65537;
    try { if (s === 1000) throw 1000; } catch (e) { s = e * 1000; } finally { s = s % 65537; }
    s = -s * -3 + (s - -3);
    s = 1000 / (i - 1000 || 1);
    while (s > 65537) s = s - 65537 * 1000;`
	sources := []struct {
		script bool
		src    string
	}{
		{false, "export function f(n, o, a, xs) {\n  let s = 0, t = \"\";\n  for (let i = 0; i < n; i++) {" + body + "\n  }\n  return s + t;\n}"},
		{false, "export function* f(n, o, a, xs) {\n  let s = 0, t = \"\";\n  for (let i = 0; i < n; i++) {" + body + "\n    s = s * 1000 + (yield 1000);\n  }\n  return s + t;\n}"},
		{false, "export async function f(n, o, a, xs) {\n  let s = 0, t = \"\", i = 0;\n  do {" + body + "\n    s = s * 1000 + await 1000;\n  } while (++i < n);\n  return s + t;\n}"},
		{true, "function f(n, o, a, xs) {\n  var s = 0, t = \"\", i = 0;\n  while (i++ < n) {" + body + "\n    with (5) { s = s + 1000; }\n    with (o) { k = k * 1000 + 5; }\n  }\n  return s + t;\n}"},
	}
	for _, c := range sources {
		var top *bytecode.Function
		var sf *syntax.Function
		if c.script {
			s, err := syntax.ParseScript("t.js", c.src, syntax.Options{})
			require.NoError(t, err)
			top, err = CompileScript(s)
			require.NoError(t, err)
			sf = s.Body[0].(*syntax.FuncDecl).Func
		} else {
			m, err := syntax.ParseModule("t.js", c.src, syntax.Options{})
			require.NoError(t, err)
			top, err = CompileModule(m)
			require.NoError(t, err)
			sf = m.Body[0].(*syntax.ExportDecl).Decl.(*syntax.FuncDecl).Func
		}
		checkHoistedOnlyRead(t, c.src[:40], child(t, top, "f"), sf)
	}
}

// TestHoistRegisterLimitEntryPoints runs TestHoistRegisterLimit's
// near-limit function through every compile entry point: each retries
// without hoisting in g, keeps hoisting in k, compiles the same bytecode
// twice, and fails as on dev one argument further.
func TestHoistRegisterLimitEntryPoints(t *testing.T) {
	src := func(nargs int) string {
		args := strings.TrimSuffix(strings.Repeat("i, ", nargs), ", ")
		return `function g(n, h) {
  let s = 0;
  for (let i = 0; i < n; i++) { s = (s + i * 3000) % 7919; h(` + args + `); }
  return s;
}
function k(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + i * 3000) % 7919; return s; }`
	}
	var find func(fn *bytecode.Function, name string) *bytecode.Function
	find = func(fn *bytecode.Function, name string) *bytecode.Function {
		if fn.Name == name {
			return fn
		}
		for _, c := range fn.Children {
			if r := find(c, name); r != nil {
				return r
			}
		}
		return nil
	}
	var h Hook
	entries := []struct {
		name    string
		compile func(src string) (*bytecode.Function, error)
	}{
		{"module", func(src string) (*bytecode.Function, error) {
			m, err := syntax.ParseModule("m.js", src, syntax.Options{})
			require.NoError(t, err)
			return CompileModule(m)
		}},
		{"script", func(src string) (*bytecode.Function, error) {
			s, err := syntax.ParseScript("s.js", src, syntax.Options{})
			require.NoError(t, err)
			return CompileScript(s)
		}},
		{"eval", func(src string) (*bytecode.Function, error) { return h.CompileEval("e.js", src, nil, nil) }},
		{"function", func(src string) (*bytecode.Function, error) {
			return h.CompileFunction("anonymous", "", src+"\nreturn [g, k];", false, false, nil)
		}},
	}
	for _, e := range entries {
		fn, err := e.compile(src(250))
		require.NoError(t, err, e.name)
		g := find(fn, "g")
		require.NotNil(t, g, e.name)
		assert.Equal(t, 256, int(g.NumRegs), e.name)
		in, _ := loopLoads(t, g)
		assert.Equal(t, 2, in, "%s: g at the limit hoists nothing", e.name)
		in, _ = loopLoads(t, find(fn, "k"))
		assert.Equal(t, 0, in, "%s: k still hoists", e.name)
		again, err := e.compile(src(250))
		require.NoError(t, err, e.name)
		assert.Equal(t, bytecode.Disassemble(fn), bytecode.Disassemble(again), "%s: deterministic", e.name)
		_, err = e.compile(src(251))
		assert.ErrorContains(t, err, "more than 256 registers", e.name)
	}
}

// TestHoistStateParallel compiles hoisting units on several goroutines at
// once: each takes the spare state or its own, and every result is the
// bytecode a lone compile gives.
func TestHoistStateParallel(t *testing.T) {
	src := `export function f(n, a) { let s = 0; for (let i = 0; i < n; i++) { s = (s + a[i % 1000] * 1000) % 65537; for (let j = 0; j < 3; j++) s ^= j * 7919; } return s; }
export function g(n) { let t = ""; for (let i = 0; i < n; i++) t = t + "k" + (i * 3000) % 7; return t; }`
	m, err := syntax.ParseModule("t.js", src, syntax.Options{})
	require.NoError(t, err)
	fn, err := CompileModule(m)
	require.NoError(t, err)
	want := bytecode.Disassemble(fn)
	const workers, rounds = 8, 50
	errs := make(chan string, workers)
	for range workers {
		go func() {
			for range rounds {
				m, err := syntax.ParseModule("t.js", src, syntax.Options{})
				if err != nil {
					errs <- err.Error()
					return
				}
				fn, err := CompileModule(m)
				if err != nil {
					errs <- err.Error()
					return
				}
				if got := bytecode.Disassemble(fn); got != want {
					errs <- "different bytecode:\n" + got
					return
				}
			}
			errs <- ""
		}()
	}
	for range workers {
		assert.Empty(t, <-errs)
	}
}

package compiler

import (
	"errors"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCallEval checks which calls are direct eval call sites and what their
// EvalScope records.
func TestCallEval(t *testing.T) {
	tests := []struct {
		name, src string
		direct    int
	}{
		{"direct", `function f() { eval("1"); }`, 1},
		{"spread arguments", `function f(a) { eval(...a); }`, 1},
		{"member", `function f(o) { o.eval("1"); }`, 0},
		{"parenthesized sequence", `function f() { (0, eval)("1"); }`, 0},
		{"optional call", `function f() { eval?.("1"); }`, 0},
		{"new", `function f() { new eval("1"); }`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := child(t, compileScript(t, tc.src), "f")
			assert.Equal(t, tc.direct, count(f, bytecode.CallEval))
			if tc.direct == 0 {
				assert.Nil(t, f.Extra, "a function without a direct eval pays nothing")
				return
			}
			require.NotNil(t, f.Extra)
			assert.Len(t, f.Extra.Evals, tc.direct)
		})
	}

	fn := compileScript(t, `function f(a) { let l = 1; return () => { eval("a + l"); }; }`)
	arrow := child(t, child(t, fn, "f"), "")
	require.NotNil(t, arrow.Extra)
	sc := arrow.Extra.Evals[0]
	var names []string
	for l := sc.Env; l != nil; l = l.Outer {
		for _, n := range l.Names {
			if n != "" {
				names = append(names, n)
			}
		}
	}
	assert.Subset(t, names, []string{"a", "l"}, "the bindings of the call site are captured")
	assert.GreaterOrEqual(t, sc.This, 0, "the arrow's this is f's")
	assert.GreaterOrEqual(t, sc.Var, 0, "sloppy eval code declares its vars in f")
	assert.Equal(t, -1, sc.Fields)
	assert.False(t, sc.Strict || sc.Module || sc.InParams)

	strict := child(t, compileModule(t, `export function f() { eval("1"); }`), "f")
	sc = strict.Extra.Evals[0]
	assert.True(t, sc.Strict && sc.Module)
	assert.Equal(t, -1, sc.Var, "strict eval code has a variable environment of its own")

	params := child(t, compileScript(t, `function f(a = eval("1")) { var v; }`), "f")
	assert.True(t, params.Extra.Evals[0].InParams)
}

// TestEvalSiteSharing: the direct eval call sites of a compilation share
// what they have in common (evalMemo), so that each costs little however
// many bindings it sees: the calls in one scope of a function one Extra.Evals
// entry, the calls of different functions with the same environment and
// this one EvalScope, and every environment one EvalLevel, linked to by the
// levels inside it.
func TestEvalSiteSharing(t *testing.T) {
	f := child(t, compileScript(t, `function f(a) {
	eval("1"); eval("2");
	{ let b; eval("3"); }
	{ let c; eval("4"); eval("5"); }
	var p = () => { "use strict"; return eval("6"); }, q = () => { "use strict"; return eval("7"); };
	return () => eval("8");
}`), "f")
	assert.Equal(t, 5, count(f, bytecode.CallEval))
	require.NotNil(t, f.Extra)
	ev := f.Extra.Evals
	require.Len(t, ev, 3, "one entry for the calls of each scope")
	assert.Equal(t, []string{"b"}, ev[1].Env.Names)
	assert.Equal(t, []string{"c"}, ev[2].Env.Names)
	assert.Same(t, ev[0].Env, ev[1].Env.Outer, "the blocks' levels link to f's")
	assert.Same(t, ev[0].Env, ev[2].Env.Outer)
	assert.Equal(t, []int{0, 1, 1}, []int{ev[0].This, ev[1].This, ev[2].This})
	// Strict arrows have no environment of their own: their calls see f's
	// environments and this.
	p, q := child(t, f, "p"), child(t, f, "q")
	require.True(t, p.Extra != nil && q.Extra != nil)
	assert.Same(t, p.Extra.Evals[0], q.Extra.Evals[0])
	assert.Same(t, ev[0].Env, p.Extra.Evals[0].Env)
	// A sloppy arrow's %evalvars binding gives it one.
	arrow := child(t, f, "")
	require.NotNil(t, arrow.Extra)
	assert.Same(t, ev[0].Env, arrow.Extra.Evals[0].Env.Outer)
}

func TestHookCompileEval(t *testing.T) {
	var h Hook
	fn, err := h.CompileEval("e.js", `var v; function g() {} { function b() {} } let l; v`, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, bytecode.KindArrow, fn.Kind)
	assert.Equal(t, "e.js", fn.Source.Name, "the referrer of the code's module requests")
	require.NotNil(t, fn.Extra)
	g := fn.Extra.Globals
	require.NotNil(t, g)
	assert.True(t, g.Eval)
	assert.Equal(t, []string{"v"}, g.Var)
	assert.Equal(t, []string{"g"}, g.Function)
	assert.Equal(t, []string{"b"}, g.AnnexB)
	assert.Empty(t, g.Lexical, "eval code's lexical declarations stay in the eval")

	fn, err = h.CompileEval("e.js", `"use strict"; var v;`, nil, nil)
	require.NoError(t, err)
	assert.True(t, fn.Strict)
	assert.Nil(t, fn.Extra, "strict eval code declares nothing globally")

	_, err = h.CompileEval("e.js", `a b`, nil, nil)
	var m interface{ Message() string }
	require.ErrorAs(t, err, &m)
	assert.Equal(t, "Unexpected identifier 'b'", m.Message(), "the message of the SyntaxError eval throws")
}

func TestHookCompileFunction(t *testing.T) {
	var h Hook
	fn, err := h.CompileFunction("d.js", "a,b", "return a + b", false, false, nil)
	require.NoError(t, err)
	assert.Equal(t, bytecode.KindScript, fn.Kind)
	assert.Nil(t, fn.Extra, "the function's name is not declared")
	f := child(t, fn, "anonymous")
	assert.Equal(t, "function anonymous(a,b\n) {\nreturn a + b\n}", f.SourceText())
	assert.Equal(t, "d.js", f.Source.Name)
	assert.Equal(t, 2, f.Length)

	g, err := h.CompileFunction("d.js", "", "yield 1", true, true, nil)
	require.NoError(t, err)
	gf := child(t, g, "anonymous")
	assert.True(t, gf.Generator && gf.Async)
	assert.Equal(t, "async function* anonymous(\n) {\nyield 1\n}", gf.SourceText())

	tests := []struct {
		name, params, body, msg string
	}{
		{"parameters end early", "/*", "*/){", "Arg string terminates parameters early"},
		{"body ends early", "", "}; function x() {", "Function body ends the function early"},
		{"body closes the function", "", "}{", "Function body ends the function early"},
		{"parameters do not parse", "}", "", "Unexpected token '}'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.CompileFunction("d.js", tc.params, tc.body, false, false, nil)
			var m interface{ Message() string }
			require.ErrorAs(t, err, &m)
			assert.Equal(t, tc.msg, m.Message())
		})
	}
}

// TestHookStop: the Hook calls stop before the first and every 1024th
// statement of each list it parses, resolves and compiles, and a stop that
// fails ends the compile there, in any of the three passes, with its error.
func TestHookStop(t *testing.T) {
	var h Hook
	list := strings.Repeat("x = x + 1;", stopEvery+1) // two calls a pass
	src := "function g() {" + list + "} {" + list + "} switch (0) { case 0: " + list + "} " + list
	compiles := []struct {
		name    string
		calls   int
		compile func(stop func() error) (*bytecode.Function, error)
	}{
		// Two for each of the four lists in each pass.
		{"script", 24, func(stop func() error) (*bytecode.Function, error) { return h.CompileScript("s.js", src, stop) }},
		{"eval", 24, func(stop func() error) (*bytecode.Function, error) { return h.CompileEval("e.js", src, nil, stop) }},
		// And the function's body: one more list, whose one statement,
		// the function, the compiler compiles without it.
		{"function", 26, func(stop func() error) (*bytecode.Function, error) {
			return h.CompileFunction("f.js", "", src, false, false, stop)
		}},
	}
	errStop := errors.New("stop")
	for _, tc := range compiles {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := tc.compile(func() error { calls++; return nil })
			require.NoError(t, err)
			require.Equal(t, tc.calls, calls, "calls of a stop that returns nil")
			for at := 1; at <= tc.calls; at++ {
				calls = 0
				fn, err := tc.compile(func() error {
					if calls++; calls == at {
						return errStop
					}
					return nil
				})
				assert.Nil(t, fn)
				assert.Same(t, errStop, err, "a stop failing at call %d", at)
				assert.Equal(t, at, calls, "a stop failing at call %d", at)
			}
		})
	}
}

// TestCallEvalLimit: a direct eval needs the pc after its call site in the
// range a generator's suspension point needs, since the frame reruns from
// there with the pc saved the same way (frameOp), past the op and its extra
// word. Each elision of the pattern is one instruction word.
func TestCallEvalLimit(t *testing.T) {
	if raceEnabled {
		t.Skip("compiles 64 MB of code")
	}
	compile := func(words int) (*bytecode.Function, error) {
		src := "function f(a) { var [" + strings.Repeat(",", words) + "] = a; eval(''); }"
		s, err := syntax.ParseScript("s.js", src, syntax.Options{})
		require.NoError(t, err)
		return CompileScript(s)
	}
	site := func(words int) int {
		fn, err := compile(words)
		require.NoError(t, err)
		f := child(t, fn, "f")
		for pc := 0; pc < len(f.Code); {
			op := bytecode.DecodeOp(f.Code[pc])
			if op == bytecode.CallEval {
				return pc - words
			}
			pc += 1 + op.ExtraWords()
		}
		t.Fatal("no CallEval")
		return 0
	}
	off := site(1000)
	require.Equal(t, off, site(1001))
	// The last site whose rerun pc fits, then the first that does not.
	_, err := compile(genMaxCode - 2 - off)
	require.NoError(t, err)
	_, err = compile(genMaxCode - 1 - off)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SyntaxError: function too large for a direct eval is not supported yet")
}

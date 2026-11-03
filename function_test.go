package moejs_test

import (
	"errors"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runFunction runs the script CompileFunction or CompileFunctionTS
// returned and binds the function it creates to the global name.
func runFunction(t *testing.T, rt *moejs.Runtime, s *moejs.Script, name string) moejs.Value {
	t.Helper()
	fn, err := rt.RunScript(s)
	require.NoError(t, err)
	require.NoError(t, rt.SetGlobal(name, fn))
	return fn
}

// eval runs src as a script and returns its completion value as a string.
func eval(t *testing.T, rt *moejs.Runtime, src string) string {
	t.Helper()
	s, err := moejs.CompileScript("eval.js", src)
	require.NoError(t, err)
	v, err := rt.RunScript(s)
	require.NoError(t, err)
	return v.String()
}

// TestCompileFunction: CompileFunction builds what the Function
// constructors build, from the host.
func TestCompileFunction(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	s, err := moejs.CompileFunction("add.js", "a, b = 1", "return a + b", moejs.FunctionOptions{})
	require.NoError(t, err)
	assert.Equal(t, "add.js", s.Name())
	fn := runFunction(t, rt, s, "add")
	v, err := rt.CallFunction(fn, moejs.Undefined(), moejs.Int(2), moejs.Int(3))
	require.NoError(t, err)
	assert.Equal(t, "5", v.String())
	assert.Equal(t, "3|anonymous|1|function anonymous(a, b = 1\n) {\nreturn a + b\n}", eval(t, rt, "[add(2), add.name, add.length, String(add)].join('|')"))

	sloppy, err := moejs.CompileFunction("this.js", "", "return this === globalThis", moejs.FunctionOptions{})
	require.NoError(t, err)
	runFunction(t, rt, sloppy, "sloppy")
	assert.Equal(t, "true", eval(t, rt, "sloppy()"), "sloppy mode code")

	gen, err := moejs.CompileFunction("gen.js", "n", "for (let i = 0; i < n; i++) yield i", moejs.FunctionOptions{Generator: true})
	require.NoError(t, err)
	runFunction(t, rt, gen, "gen")
	assert.Equal(t, "0,1,2", eval(t, rt, "[...gen(3)].join()"))

	// Each runtime that runs the script gets a function of its own.
	other := moejs.NewRuntime(moejs.Options{})
	runFunction(t, other, s, "add")
	assert.Equal(t, "false", eval(t, other, "Object.getPrototypeOf(add) !== Function.prototype"))

	for _, c := range []struct{ params, body, msg string }{
		{"a", "return a +", "Unexpected token '}'"},
		{"/*", "*/){", "Arg string terminates parameters early"},
		{"a", "}; function f() {", "Function body ends the function early"},
	} {
		_, err := moejs.CompileFunction("bad.js", c.params, c.body, moejs.FunctionOptions{})
		var se *moejs.SyntaxError
		require.True(t, errors.As(err, &se), "%s: %v", c.body, err)
		assert.Equal(t, c.msg, se.Message, c.body)
	}
}

// TestCompileFunctionTS: CompileFunctionTS reads TypeScript with
// CompileTS's rules, into a strict mode function.
func TestCompileFunctionTS(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	params := "input: { messages: string[] }, limit?: number"
	body := "type N = number;\ninterface Unused { x: N }\nconst n: N = (limit ?? 2) as N;\nreturn input.messages.slice(0, n).join(',')!;"
	s, err := moejs.CompileFunctionTS("count.ts", params, body, moejs.FunctionOptions{})
	require.NoError(t, err)
	runFunction(t, rt, s, "count")
	assert.Equal(t, "a,b|a|2", eval(t, rt, "[count({messages: ['a', 'b', 'c']}), count({messages: ['a', 'b']}, 1), count.length].join('|')"))
	assert.Equal(t, "function anonymous("+params+"\n) {\n"+body+"\n}", eval(t, rt, "String(count)"), "toString returns the TypeScript")

	strict, err := moejs.CompileFunctionTS("this.ts", "", "return this === undefined", moejs.FunctionOptions{})
	require.NoError(t, err)
	runFunction(t, rt, strict, "strict")
	assert.Equal(t, "true", eval(t, rt, "strict()"), "strict mode code")

	async, err := moejs.CompileFunctionTS("async.ts", "x: Promise<number>", "const v = await x as number;\nreturn v * 2;", moejs.FunctionOptions{Async: true})
	require.NoError(t, err)
	runFunction(t, rt, async, "twice")
	eval(t, rt, "twice(Promise.resolve(21)).then(v => { globalThis.out = v })")
	assert.Equal(t, "42", eval(t, rt, "out"))

	gen, err := moejs.CompileFunctionTS("gen.ts", "n: number", "for (let i: number = 0; i < n; i++) yield <number>i;", moejs.FunctionOptions{Generator: true})
	require.NoError(t, err)
	runFunction(t, rt, gen, "gen")
	assert.Equal(t, "0,1,2", eval(t, rt, "[...gen(3)].join()"))

	for _, c := range []struct {
		params, body string
		err          moejs.SyntaxError
	}{
		{"a: number", "enum E { A }\nreturn a", moejs.SyntaxError{File: "bad.ts", Line: 3, Column: 1, Message: "A TypeScript enum is not supported: only erasable TypeScript is"}},
		{"public a: number", "return a", moejs.SyntaxError{File: "bad.ts", Line: 1, Column: 20, Message: "A parameter property is not supported: only erasable TypeScript is"}},
		{"a: number", "let x: = 1", moejs.SyntaxError{File: "bad.ts", Line: 3, Column: 8, Message: "Unexpected token '='"}},
		{"a", "with (a) {}", moejs.SyntaxError{File: "bad.ts", Line: 3, Column: 1, Message: "Strict mode code may not include a with statement"}},
		{"a", "import x from 'm'", moejs.SyntaxError{File: "bad.ts", Line: 3, Column: 1, Message: "'import' and 'export' may only appear at the top level"}},
		{"a: number): void; function anonymous(b", "return b", moejs.SyntaxError{File: "bad.ts", Line: 1, Column: 1, Message: "Function body ends the function early"}},
	} {
		_, err := moejs.CompileFunctionTS("bad.ts", c.params, c.body, moejs.FunctionOptions{})
		var se *moejs.SyntaxError
		require.True(t, errors.As(err, &se), "%s: %v", c.body, err)
		assert.Equal(t, c.err, *se, c.params+" / "+c.body)
	}
}

package moejs_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countdownContext is a context whose Err is context.Canceled from its
// left-th call on (never when left is negative), counting the calls.
type countdownContext struct {
	context.Context
	left, calls atomic.Int64
}

func newCountdown(left int64) *countdownContext {
	c := &countdownContext{Context: context.Background()}
	c.left.Store(left)
	return c
}

func (c *countdownContext) Err() error {
	c.calls.Add(1)
	if c.left.Add(-1) == 0 {
		c.left.Store(0)
		return context.Canceled
	}
	if c.left.Load() == 0 {
		return context.Canceled
	}
	return nil
}

// nested returns n levels of open around mid, closed by close.
func nested(head, open, mid, close string, n int) string {
	return head + strings.Repeat(open, n) + mid + strings.Repeat(close, n) + ";"
}

// TestCompileContext: CompileOptions.Context and FunctionOptions.Context
// stop a compile: before it starts, while the parser, the scope pass and
// the compiler go through statements, and while the TypeScript parser
// reads ahead through a nesting that holds no statement.
func TestCompileContext(t *testing.T) {
	statements := strings.Repeat("x = x + 1;\n", 5000)
	compiles := []struct {
		name    string
		compile func(ctx context.Context) error
	}{
		{"module", func(ctx context.Context) error {
			_, err := moejs.CompileWith("m.js", "let x = 0;\n"+statements, moejs.CompileOptions{Context: ctx})
			return err
		}},
		{"TypeScript module", func(ctx context.Context) error {
			_, err := moejs.CompileWith("m.ts", "let x: number = 0;\n"+statements, moejs.CompileOptions{TypeScript: true, Context: ctx})
			return err
		}},
		{"script", func(ctx context.Context) error {
			_, err := moejs.CompileScriptWith("s.js", "var x = 0;\n"+statements, moejs.CompileOptions{Context: ctx})
			return err
		}},
		{"function", func(ctx context.Context) error {
			_, err := moejs.CompileFunction("f.js", "x", statements, moejs.FunctionOptions{Context: ctx})
			return err
		}},
		{"TypeScript nesting", func(ctx context.Context) error {
			_, err := moejs.CompileWith("n.ts", nested("export const x = ", "a ? (b): T => ", "c", "", 2000), moejs.CompileOptions{TypeScript: true, Context: ctx})
			return err
		}},
	}
	for _, c := range compiles {
		// A context that is never done is asked now and then: the count
		// it gives is how often the compile checks.
		count := newCountdown(-1)
		require.NoError(t, c.compile(count), c.name)
		checks := count.calls.Load()
		assert.Greater(t, checks, int64(2), c.name)
		// Done before the compile, or at any check, the compile stops
		// with its error.
		for _, at := range []int64{1, 2, checks / 2, checks} {
			err := c.compile(newCountdown(at))
			assert.True(t, errors.Is(err, context.Canceled), "%s, done at check %d of %d: %v", c.name, at, checks, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := moejs.CompileWith("m.js", "export const x = 1;", moejs.CompileOptions{Context: ctx})
	assert.ErrorIs(t, err, context.Canceled)
	_, err = moejs.CompileFunctionTS("f.ts", "", "return 1", moejs.FunctionOptions{Context: ctx})
	assert.ErrorIs(t, err, context.Canceled)
}

// TestCompileScriptWithTypeScript: CompileScriptWith compiles a script of
// TypeScript, strict mode code.
func TestCompileScriptWithTypeScript(t *testing.T) {
	s, err := moejs.CompileScriptWith("s.ts", "type N = number;\nvar n: N = 2;\n(function (this: unknown) { return this === undefined; })() && n as N;", moejs.CompileOptions{TypeScript: true})
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	v, err := rt.RunScript(s)
	require.NoError(t, err)
	assert.Equal(t, "2", v.String())
	_, err = moejs.CompileScriptWith("s.ts", "with (x) {}", moejs.CompileOptions{TypeScript: true})
	var se *moejs.SyntaxError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, "Strict mode code may not include a with statement", se.Message)
}

// TestCompileNestingLinear: TypeScript nested 2,000 levels deep in the
// shapes that made the parser read the nesting again at every level
// compiles, and a nesting past the limit is a SyntaxError that says so.
func TestCompileNestingLinear(t *testing.T) {
	for _, sh := range compileNestingShapes {
		_, err := moejs.CompileWith("n.ts", nested(sh.head, sh.open, sh.mid, sh.close, 2000), moejs.CompileOptions{TypeScript: true})
		require.NoError(t, err, sh.name)
	}
	_, err := moejs.CompileTS("n.ts", nested("export const x = ", "(", "1", ")", 20000))
	var se *moejs.SyntaxError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, "Nesting too deep (the parser allows 10000 levels)", se.Message)
}

var compileNestingShapes = []struct{ name, head, open, mid, close string }{
	{"conditional-return-type", "export const x = ", "a ? (b): T => ", "c", ""},
	{"generic-conditional-return-type", "export const x = ", "a ? <T>(b): T => ", "c", ""},
	{"async-comparison", "export const x = ", "async < b > ((c) => ", "c", ")"},
	{"function-type-default", "export let x: ", "([a = (c: ", "T", ") => 0]) => void"},
	{"js-arrows", "export const x = ", "(a) => ", "a", ""},
}

// BenchmarkCompileNesting compiles TypeScript nested ever deeper in the
// shapes that made the parser read the nesting again at every level, and
// nested arrow functions of JavaScript as the control: the time per
// compile grows in proportion to the depth.
func BenchmarkCompileNesting(b *testing.B) {
	for _, sh := range compileNestingShapes {
		depths := []int{250, 500, 1000, 2000}
		if sh.name == "function-type-default" {
			depths = []int{8, 12, 16, 20, 250, 500, 1000, 2000}
		}
		for _, n := range depths {
			src := nested(sh.head, sh.open, sh.mid, sh.close, n)
			b.Run(sh.name+"/"+strconv.Itoa(n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := moejs.CompileTS("n.ts", src); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// TestDeepScopes: references nested hundreds of scopes deep resolve to
// the nearest binding, as the resolver's memo of far lookups must keep:
// some levels of the nesting shadow a, none b.
func TestDeepScopes(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("const b = 'global b';\nconst r = ")
	const levels = 500
	for i := range levels {
		switch {
		case i%50 == 7:
			sb.WriteString("(a) => ")
		case i == 300:
			sb.WriteString("(p300 = a) => ") // the a of level 257
		default:
			sb.WriteString("(p" + strconv.Itoa(i) + ") => ")
		}
	}
	sb.WriteString("[a, b, typeof c, p300]")
	sb.WriteString(";\nexport function run() { let f = r; for (let i = 0; i < " + strconv.Itoa(levels) + "; i++) f = f(i === 300 ? undefined : i); return JSON.stringify(f); }")
	mod, err := moejs.Compile("deep.js", sb.String())
	require.NoError(t, err)
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	v, err := rt.Call(mustHook(t, mod, "run"))
	require.NoError(t, err)
	assert.Equal(t, `[457,"global b","undefined",257]`, v.String())
}

// afterContext is done once its deadline passed, checked on every Err call
// (no timer goroutine).
type afterContext struct {
	context.Context
	at time.Time
}

func (c afterContext) Err() error {
	if time.Now().After(c.at) {
		return context.DeadlineExceeded
	}
	return nil
}

// TestCompileContextStopsLongStatementInTime: a context that expires early
// in a compile stops it soon after, whatever the source's shape, also a
// source of a few megabytes that holds one statement: the parser, the
// scope pass and the compiler poll the context in long lists too.
func TestCompileContextStopsLongStatementInTime(t *testing.T) {
	shapes := []struct {
		name, src string
		ts        bool
	}{
		{"statements", strings.Repeat("x = [1, 2, 3, 4, 5, 6, 7, 8];\n", 60000), false},
		{"local concatenation", "export function f(a) { return a" + strings.Repeat(" + a", 400000) + "; }", false},
		{"array literal (past the operand limit)", "export const x = [" + strings.Repeat("1, ", 1500000) + "];", false},
		{"union type", "type T = " + strings.Repeat("'a' | ", 1000000) + "'a';", true},
		{"interface", "interface I { " + strings.Repeat("a: string; ", 500000) + "}", true},
		{"tuple type", "let x: [" + strings.Repeat("string, ", 1000000) + "] = [];", true},
		{"object type", "let x: {" + strings.Repeat("a: (b: number) => void; ", 300000) + "} = {};", true},
	}
	for _, sh := range shapes {
		for _, ts := range []bool{sh.ts} {
			start := time.Now()
			_, err := moejs.CompileWith("m.js", sh.src, moejs.CompileOptions{TypeScript: ts})
			full := time.Since(start)
			if err != nil {
				t.Logf("%s ts=%v: %v (after %v)", sh.name, ts, err, full)
			}
			budget := full / 10
			start = time.Now()
			_, err = moejs.CompileWith("m.js", sh.src, moejs.CompileOptions{TypeScript: ts, Context: afterContext{context.Background(), start.Add(budget)}})
			took := time.Since(start)
			t.Logf("%s ts=%v (%d KB): full compile %v, stopped after %v (err %v)", sh.name, ts, len(sh.src)>>10, full, took, err)
			assert.ErrorIs(t, err, context.DeadlineExceeded, sh.name)
			assert.Less(t, took, budget+full/5+20*time.Millisecond, "%s ts=%v: a context that expired after %v stopped a %v compile only after %v", sh.name, ts, budget, full, took)
		}
	}
}

// TestCompileContextStopsLongStatement: the items of one long statement
// (array and object literals, arguments, parameters, a chain of binary
// operators, union and intersection types, interface and object type
// members, tuple types) are polled every 1,024 by the parser, and the
// literals and arguments by the scope pass and the compiler too: a context
// done at any of those polls stops the compile with its error.
func TestCompileContextStopsLongStatement(t *testing.T) {
	const n = 5000
	list := func(f func(i int) string, sep string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = f(i)
		}
		return strings.Join(parts, sep)
	}
	one := func(int) string { return "1" }
	shapes := []struct {
		name, src string
		ts        bool
	}{
		{"array literal", "export const x = [" + list(one, ", ") + "];", false},
		{"array literal with holes", "export const x = [" + strings.Repeat(",", n) + "];", false},
		{"object literal", "export const x = {" + list(func(i int) string { return "k" + strconv.Itoa(i) + ": 1" }, ", ") + "};", false},
		{"arguments", "export function f() {} f(...[], " + list(one, ", ") + ");", false},
		{"binary operators", "export const x = " + list(one, " + ") + ";", false},
		{"parameters", "export function f(" + list(func(i int) string { return "p" + strconv.Itoa(i) }, ", ") + ") {}", false},
		{"union type", "type T = " + list(func(int) string { return "'a'" }, " | ") + ";", true},
		{"intersection type", "type T = " + list(func(int) string { return "{}" }, " & ") + ";", true},
		{"interface", "interface I { " + list(func(i int) string { return "a" + strconv.Itoa(i) + ": string;" }, " ") + " }", true},
		{"object type", "let x: { " + list(func(i int) string { return "a" + strconv.Itoa(i) + ": string;" }, " ") + " } = null!;", true},
		{"tuple type", "let x: [" + list(func(int) string { return "string" }, ", ") + "] = null!;", true},
	}
	for _, sh := range shapes {
		all := newCountdown(-1)
		_, err := moejs.CompileWith("m.ts", sh.src, moejs.CompileOptions{TypeScript: sh.ts, Context: all})
		if err != nil {
			// More parameters than a function has registers: the compiler
			// fails after the parser and the scope pass polled them.
			var se *moejs.SyntaxError
			require.ErrorAs(t, err, &se, sh.name)
		}
		calls := all.calls.Load()
		assert.GreaterOrEqual(t, calls, int64(1+n/1024), "%s: the list's items are polled", sh.name)
		for k := int64(2); k <= calls; k++ {
			_, err := moejs.CompileWith("m.ts", sh.src, moejs.CompileOptions{TypeScript: sh.ts, Context: newCountdown(k)})
			assert.ErrorIs(t, err, context.Canceled, "%s: stopped at poll %d of %d", sh.name, k, calls)
		}
	}
}

package moejs_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repeatN concatenates f(0) through f(n-1).
func repeatN(n int, f func(i string) string) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(f(strconv.Itoa(i)))
	}
	return b.String()
}

// noInternal fails t when err is an *InternalError: code too large for
// the bytecode runs or is a clean error, never an engine panic.
func noInternal(t *testing.T, err error) {
	t.Helper()
	var ie *moejs.InternalError
	if errors.As(err, &ie) {
		t.Fatalf("internal error: %v\n%s", ie.Value, ie.Stack)
	}
}

// TestManyDeclarations runs code with 100,000 declarations, constants,
// properties, cases or elements: each runs, or is a SyntaxError that
// names the limit of one function's bytecode it passes (65536 constants,
// 65536 property and global variable accesses, 65536 nested functions,
// 256 registers, a jump over 32767 instructions).
func TestManyDeclarations(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("compiles megabytes of source")
	}
	const n = 100000
	last := strconv.Itoa(n - 1)
	scripts := []struct {
		name, src string
		want      string // the completion value's string, or
		err       string // the SyntaxError's text
	}{
		{name: "var", src: repeatN(n, func(i string) string { return "var v" + i + ";" }) + "v0 = 1; v" + last + " = 2; v0 + v" + last, want: "3"},
		{name: "var list", src: "var " + repeatN(n, func(i string) string { return "v" + i + "," }) + "w; typeof v" + last, want: "undefined"},
		// A name and a number constant each past LoadInt's range.
		{name: "var with initializers", src: repeatN(n, func(i string) string { return "var v" + i + " = " + i + ";\n" }), err: "x.js:49153:5: SyntaxError: function needs more than 65536 constants"},
		{name: "65536 vars with an initializer", src: repeatN(65536, func(i string) string { return "var v" + i + " = 0;" }), want: "undefined"},
		{name: "65537 vars with an initializer", src: repeatN(65537, func(i string) string { return "var v" + i + " = 0;" }), err: "SyntaxError: function needs more than 65536 constants"},
		{name: "65535 functions", src: repeatN(65535, func(i string) string { return "function f" + i + "() {}" }) + "typeof f65534", want: "function"},
		{name: "functions", src: repeatN(n, func(i string) string { return "function f" + i + "() {}" }), err: "SyntaxError: function needs more than 65536"},
		{name: "let", src: repeatN(n, func(i string) string { return "let l" + i + ";" }), err: "SyntaxError: function needs more than 65536 constants"},
		{name: "const", src: repeatN(n, func(i string) string { return "const c" + i + " = 0;" }), err: "SyntaxError: function needs more than 65536 constants"},
		{name: "block let", src: "{" + repeatN(n, func(i string) string { return "let l" + i + ";" }) + "}", err: "SyntaxError: function needs more than 256 registers"},
		{name: "function var", src: "(function () {" + repeatN(n, func(i string) string { return "var v" + i + ";" }) + "})()", err: "SyntaxError: function needs more than 256 registers"},
		{name: "global reads", src: "var x = 1;" + repeatN(n, func(string) string { return "x;" }), err: "SyntaxError: function needs more than 65536 property and global variable accesses"},
		{name: "strings", src: "(function () { var a;" + repeatN(n, func(i string) string { return "a = 's" + i + "';" }) + "})()", err: "SyntaxError: function needs more than 65536 constants"},
		{name: "property names", src: "(function (o) {" + repeatN(n, func(i string) string { return "o.p" + i + " = 1;" }) + "})({})", err: "SyntaxError: function needs more than 65536 constants"},
		{name: "object literal", src: "Object.keys({" + repeatN(n, func(i string) string { return "p" + i + ": 0," }) + "}).length", err: "SyntaxError: function needs more than 65536 constants"},
		{name: "object literal of 60000", src: "Object.keys({" + repeatN(60000, func(i string) string { return "p" + i + ": 0," }) + "}).length", want: "60000"},
		{name: "switch", src: "(function (x) { switch (x) {" + repeatN(n, func(i string) string { return "case " + strconv.Itoa(len(i)) + ": x++;" }) + "} })(1)", err: "SyntaxError: jump offset out of range (a jump over more than 32767 instructions)"},
		{name: "array literal", src: "[" + strings.Repeat("0,", n) + "].length", want: strconv.Itoa(n)},
		{name: "array literal of numbers", src: "[" + repeatN(n, func(i string) string { return i + "," }) + "].length", err: "SyntaxError: function needs more than 65536 constants"},
		{name: "spread call", src: "Math.max(...[]," + strings.Repeat("1,", n) + "2)", want: "2"},
	}
	for _, c := range scripts {
		t.Run(c.name, func(t *testing.T) {
			s, err := moejs.CompileScript("x.js", c.src)
			if c.err != "" {
				var se *moejs.SyntaxError
				require.ErrorAs(t, err, &se)
				assert.ErrorContains(t, err, c.err)
				_, err = moejs.NewRuntime(moejs.Options{}).RunScript(s)
				assert.EqualError(t, err, "moejs: RunScript of a nil *Script")
				return
			}
			require.NoError(t, err)
			for _, mutable := range []bool{false, true} {
				v, err := moejs.NewRuntime(moejs.Options{MutableIntrinsics: mutable}).RunScript(s)
				noInternal(t, err)
				require.NoError(t, err, "mutable %v", mutable)
				assert.Equal(t, c.want, v.String(), "mutable %v", mutable)
			}
		})
	}

	t.Run("module", func(t *testing.T) {
		exports := repeatN(n, func(i string) string { return "export var e" + i + " = 0;" })
		lib, err := moejs.Compile("lib.js", exports)
		require.NoError(t, err)
		main, err := moejs.Compile("main.js", "import {"+repeatN(n, func(i string) string { return "e" + i + "," })+"} from 'lib.js'; export const r = e0 + e"+last+";")
		require.NoError(t, err)
		linked, err := moejs.Link(main, func(moejs.Referrer, string) (*moejs.Module, error) { return lib, nil })
		require.NoError(t, err)
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.Load(linked))
		v, ok := rt.Export("r")
		require.True(t, ok)
		assert.Equal(t, "0", v.String())

		_, err = moejs.Compile("x.js", repeatN(n, func(i string) string { return "export function f" + i + "() {}" }))
		assert.ErrorContains(t, err, "SyntaxError: function needs more than 65536 nested functions")
		m, err := moejs.Compile("x.js", "export const o = {};"+repeatN(n, func(i string) string { return "o.p" + i + " = 1;" }))
		assert.ErrorContains(t, err, "SyntaxError: function needs more than 65536 constants")
		assert.EqualError(t, moejs.NewRuntime(moejs.Options{}).Load(m), "moejs: Load of a nil *Module")
		_, err = moejs.Link(m, nil)
		assert.EqualError(t, err, "moejs: Link of a nil *Module")
	})

	t.Run("TypeScript", func(t *testing.T) {
		m, err := moejs.CompileTS("x.ts", repeatN(n, func(i string) string { return "var v" + i + ": number;" })+"export const r = typeof v"+last+";")
		require.NoError(t, err)
		rt := moejs.NewRuntime(moejs.Options{})
		require.NoError(t, rt.Load(m))
		v, _ := rt.Export("r")
		assert.Equal(t, "undefined", v.String())
		_, err = moejs.CompileTS("x.ts", "function f(): void {"+repeatN(n, func(i string) string { return "let v" + i + ": number = " + i + ";" })+"}")
		assert.ErrorContains(t, err, "SyntaxError: function needs more than 256 registers")
		_, err = moejs.CompileScriptWith("x.ts", repeatN(n, func(i string) string { return "var v" + i + ": string = '" + i + "';" }), moejs.CompileOptions{TypeScript: true})
		assert.ErrorContains(t, err, "SyntaxError: function needs more than 65536 constants")
	})

	t.Run("dynamic", func(t *testing.T) {
		vars := strconv.Quote(repeatN(n, func(i string) string { return "var v" + i + ";" }))
		inits := strconv.Quote(repeatN(n, func(i string) string { return "var v" + i + " = " + i + ";" }))
		run := func(maxSource int, src string) (moejs.Value, error) {
			s, err := moejs.CompileScript("x.js", src)
			require.NoError(t, err)
			v, err := moejs.NewRuntime(moejs.Options{MaxDynamicSource: maxSource}).RunScript(s)
			noInternal(t, err)
			return v, err
		}
		_, err := run(0, "eval("+vars+")")
		assert.ErrorContains(t, err, "RangeError: Source text of")
		for _, src := range []string{"eval(" + vars + "); typeof v" + last, "(0, eval)(" + vars + "); typeof v" + last} {
			v, err := run(-1, src)
			require.NoError(t, err, src[:20])
			assert.Equal(t, "undefined", v.String())
		}
		for _, c := range []struct{ src, err string }{
			{"eval(" + inits + ")", "SyntaxError: function needs more than 65536 constants"},
			// A function's eval code declares its vars by name.
			{"(function () { eval(" + vars + "); })()", "SyntaxError: function needs more than 65536 constants"},
			{"new Function(" + vars + ")", "SyntaxError: function needs more than 256 registers"},
			{"new Function(" + strconv.Quote(repeatN(n, func(i string) string { return "a" + i + "," })) + ", '')", "SyntaxError: function needs more than 256 registers"},
			{"eval(" + strconv.Quote("["+strings.Repeat("0,", n)+"].length") + ")", ""},
		} {
			v, err := run(-1, c.src)
			if c.err == "" {
				require.NoError(t, err)
				assert.Equal(t, strconv.Itoa(n), v.String())
				continue
			}
			var exc *moejs.Exception
			require.ErrorAs(t, err, &exc, c.src[:20])
			assert.ErrorContains(t, err, c.err)
		}
	})
}

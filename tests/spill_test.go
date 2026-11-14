package tests

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/MoeForge/moejs"
)

// spillRun runs src as a script and returns its completion value as a
// string, or the error.
func spillRun(t *testing.T, src string) (string, error) {
	t.Helper()
	s, err := moejs.CompileScript("spill.js", src)
	if err != nil {
		return "", err
	}
	v, err := moejs.NewRuntime(moejs.Options{}).RunScript(s)
	noInternal(t, err)
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

// TestSpill runs functions declaring more bindings than a frame's 256
// registers: the bindings spilled to the function's environment (the
// compiler's spill.go) read, write, close over and keep their TDZ as
// those in registers do.
func TestSpill(t *testing.T) {
	const n = 400
	decls := func(kw string) string {
		return repeatN(n, func(i string) string { return kw + " v" + i + " = " + i + ";" })
	}
	sum := strconv.Itoa(n * (n - 1) / 2)
	sumAll := "return " + strings.TrimSuffix(repeatN(n, func(i string) string { return "v" + i + "+" }), "+") + ";"
	for _, c := range []struct{ name, src, want string }{
		{"var", "(function () {" + decls("var") + sumAll + "})()", sum},
		{"let", "(function () {" + decls("let") + sumAll + "})()", sum},
		{"const", "(function () {" + decls("const") + sumAll + "})()", sum},
		{"functions", "(function () {" + repeatN(n, func(i string) string { return "function f" + i + "() { return " + i + "; }" }) +
			"return " + strings.TrimSuffix(repeatN(n, func(i string) string { return "f" + i + "()+" }), "+") + ";})()", sum},
		{"classes", "(function () {" + repeatN(n, func(i string) string { return "class C" + i + " { static v = " + i + "; }" }) +
			"return " + strings.TrimSuffix(repeatN(n, func(i string) string { return "C" + i + ".v+" }), "+") + ";})()", sum},
		{"assign", "(function () {" + decls("let") + repeatN(n, func(i string) string { return "v" + i + " += 1;" }) + sumAll + "})()", strconv.Itoa(n*(n-1)/2 + n)},
		{"closures", "(function () {" + decls("let") + "const g = () => v0 + v" + strconv.Itoa(n-1) + "; v0 = 10; return g();})()", strconv.Itoa(10 + n - 1)},
		{"loop", "(function () {" + decls("let") + "let s = 0; for (let i = 0; i < 10; i++) { s += v1 + v" + strconv.Itoa(n-1) + "; } return s;})()", strconv.Itoa(10 * n)},
		{"generator", "(function* () {" + decls("let") + "yield v1; yield v" + strconv.Itoa(n-1) + ";})().next().value", "1"},
		{"async", "(async function () {" + decls("let") + "await 0; return v" + strconv.Itoa(n-1) + ";})() instanceof Promise", "true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := spillRun(t, c.src)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestSpillTDZ reads and writes spilled let, const and class bindings
// before their declarations: each throws the ReferenceError of a binding in
// its temporal dead zone, and a const stays immutable.
func TestSpillTDZ(t *testing.T) {
	const n = 400
	// Each p is referenced more often than x, so x is among the spilled.
	pad := repeatN(n, func(i string) string { return "let p" + i + " = " + i + "; p" + i + "; p" + i + ";" })
	use := "p0 + p" + strconv.Itoa(n-1)
	for _, c := range []struct{ name, src, err string }{
		{"let read", "(function () {" + pad + "x; let x = 1; return " + use + ";})()", "ReferenceError"},
		{"let write", "(function () {" + pad + "x = 2; let x = 1; return " + use + ";})()", "ReferenceError"},
		{"const read", "(function () {" + pad + "x; const x = 1; return " + use + ";})()", "ReferenceError"},
		{"class read", "(function () {" + pad + "new X(); class X {} return " + use + ";})()", "ReferenceError"},
		{"closure read", "(function () {" + pad + "const g = () => x; g(); let x = 1; return " + use + ";})()", "ReferenceError"},
		{"const assign", "(function () {" + pad + "const x = 1; x = 2; return " + use + ";})()", "TypeError"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := spillRun(t, c.src)
			assert.ErrorContains(t, err, c.err)
		})
	}
	// After its declaration a spilled let reads its value.
	got, err := spillRun(t, "(function () {"+pad+"let x = 7; return x + "+use+";})()")
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(7+n-1), got)
}

// TestSpillDirectEval runs a function with a direct eval call and more
// bindings than registers: direct eval captures every binding, so the
// function needs no spilling, and eval code reads them by name.
func TestSpillDirectEval(t *testing.T) {
	got, err := spillRun(t, "(function () {"+repeatN(400, func(i string) string { return "let v" + i + " = " + i + ";" })+"return eval('v1 + v399');})()")
	require.NoError(t, err)
	assert.Equal(t, "400", got)
}

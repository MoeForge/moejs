package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestEnvOrderByReferences runs a function with more captured bindings than
// GetEnv reaches, whose environment orders them by references (compiler
// envOrder): closures read and write hot and cold ones, and its arguments
// object still aliases the parameters.
func TestEnvOrderByReferences(t *testing.T) {
	var b strings.Builder
	b.WriteString("function outer(a, b) {\n")
	for i := range 300 {
		fmt.Fprintf(&b, "var v%d = %d; function g%d() { return v%d; }\n", i, i, i, i)
	}
	b.WriteString(`function hot() { v299++; return v299 + v299 + a + b; }
function setCold(x) { v0 = x; }
arguments[0] = 10;
var r = [hot(), g0(), g150(), g299(), a];
setCold(-1); b = 5;
r.push(g0(), hot(), arguments[1]);
return r.join(); }
outer(1, 2)`)
	runScriptCases(t, []scriptCase{
		{"values", []string{b.String()}, "612,0,150,300,10,-1,617,5"},
	})
}

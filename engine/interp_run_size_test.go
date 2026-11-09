package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunIsNotBig checks that the compiler does not count run as a big
// function. Go's inliner calls a function of 5,000 or more IR nodes big and
// inlines into it only callees of cost 20 or less, so NumberValue (cost 29)
// and the other helpers run's cases inline would become calls, and a loop
// like LoopSum runs about a quarter slower. run is a few nodes under the
// limit: a case added to it has to pay for its nodes by moving others out
// (DESIGN, "run stays below 5,000 IR nodes"). The stock compiler does not
// print the count, so the test reads -m=2's output: run is not "considered
// 'big'", and every NumberValue call written in run is inlined.
func TestRunIsNotBig(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the engine package")
	}
	if !is64 {
		t.Skip("the run loop it sizes is the 64-bit one, and its -m=2 output is past what a 32-bit process holds")
	}
	gocmd, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	// The fast run, whatever tags GOFLAGS sets: the checked build's copy
	// (-tags moejsregcheck, interp_run_regcheck.go) is big, and is not the
	// run this test is about. A -tags on the command line overrides
	// GOFLAGS'.
	out, err := exec.Command(gocmd, "build", "-tags=", "-gcflags=-m=2", ".").CombinedOutput()
	require.NoError(t, err, "go build -tags= -gcflags=-m=2:\n%s", out)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "interp_run.go", nil, 0)
	require.NoError(t, err)
	var run *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "run" && fn.Recv != nil {
			run = fn
		}
	}
	require.NotNil(t, run, "interp_run.go declares (*Realm).run")
	first, last := fset.Position(run.Pos()).Line, fset.Position(run.End()).Line
	calls := 0
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "NumberValue" {
				calls++
			}
		}
		return true
	})
	require.NotZero(t, calls, "run calls NumberValue")

	const rule = "run has reached the 5,000 IR nodes of a big function, which inlines only callees of cost 20 or less: move code out of run (DESIGN, \"run stays below 5,000 IR nodes\")"
	inlined := 0
	site := regexp.MustCompile(`^\./interp_run\.go:(\d+):\d+: inlining call to NumberValue$`)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "function (*Realm).run considered 'big'") {
			t.Fatalf("%s\n%s", line, rule)
		}
		if m := site.FindStringSubmatch(line); m != nil {
			if n, _ := strconv.Atoi(m[1]); n >= first && n <= last {
				inlined++
			}
		}
	}
	require.Equal(t, calls, inlined, "NumberValue calls in run inlined; %s", rule)
}

// Command suite runs one generated V8 benchmark suite v7 program
// (make_suite.py) in an embedded engine and prints the program's result: the
// fixed-work relay's completion timestamps or the suite's own scores.
//
//	suite -engine moejs fixed.js
//
// Compilation happens before the program's own clock starts.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/MoeForge/moejs"
	"github.com/dop251/goja"
	qjs "github.com/go-quickjs/go-quickjs"
	"github.com/grafana/sobek"
	"github.com/robomotionio/goant"
	mqjs "modernc.org/quickjs"
)

func run(engine, name, src string) (string, error) {
	switch engine {
	case "moejs", "moejs-old":
		p, err := moejs.CompileScript(name, src)
		if err != nil {
			return "", err
		}
		rt := moejs.NewRuntime(moejs.Options{MutableIntrinsics: true})
		v, err := rt.RunScript(p)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	case "go-quickjs":
		rt := qjs.New()
		defer rt.Close()
		v, err := rt.EvalFile(name, src)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	case "goant":
		rt := goant.New(goant.WithJIT(false))
		defer rt.Close()
		p, err := rt.Compile(name, src)
		if err != nil {
			return "", err
		}
		v, err := rt.RunProgram(p)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	case "goja":
		v, err := goja.New().RunScript(name, src)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	case "sobek":
		v, err := sobek.New().RunScript(name, src)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	case "modernc":
		rt, err := mqjs.NewVM()
		if err != nil {
			return "", err
		}
		defer rt.Close()
		v, err := rt.Eval(src, mqjs.EvalGlobal)
		return fmt.Sprint(v), err
	}
	return "", fmt.Errorf("unknown engine %q", engine)
}

func main() {
	engine := flag.String("engine", "moejs", "moejs, moejs-old, go-quickjs, goant, goja, sobek or modernc")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: suite -engine ID file.js")
		os.Exit(2)
	}
	src, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := run(*engine, flag.Arg(0), string(src))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(out)
}

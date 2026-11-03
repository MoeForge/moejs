// Command awaitbench measures await and promise jobs against Node.js with
// the same script (await-bench.mjs), on package eventloop's loop:
//
//	awaitbench -js await-bench.mjs [-only NAME]   # the script in moejs
//	node await-bench.mjs [NAME]                   # the script in Node.js
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"
)

func main() {
	js := flag.String("js", "await-bench.mjs", "the benchmark script")
	only := flag.String("only", "", "run the cases whose name holds this")
	cpu := flag.String("cpuprofile", "", "write a CPU profile")
	flag.Parse()
	if *cpu != "" {
		f, err := os.Create(*cpu)
		check(err)
		check(pprof.StartCPUProfile(f))
		defer pprof.StopCPUProfile()
	}
	src, err := os.ReadFile(*js)
	check(err)
	mod, err := moejs.Compile(*js, string(src))
	check(err)
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{})
	check(err)
	epoch := time.Now()
	check(rt.SetGlobal("benchNow", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Number(float64(time.Since(epoch).Nanoseconds()) / 1e6), nil
	})))
	check(rt.SetGlobal("benchOnly", moejs.String(*only)))
	check(rt.SetGlobal("console", consoleObject(rt)))
	check(loop.Run(func(rt *moejs.Runtime) error {
		err := rt.Load(mod)
		if err == moejs.ErrModulePending {
			return nil
		}
		return err
	}))
}

func consoleObject(rt *moejs.Runtime) moejs.Value {
	r := rt.Realm()
	o := r.NewObject()
	_, _ = o.CreateDataProperty(r, r.KeyFromGoString("log"), rt.Function("log", 1, func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = a.String()
		}
		fmt.Println(strings.Join(parts, " "))
		return moejs.Undefined(), nil
	}))
	return engine.ObjectValue(o)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

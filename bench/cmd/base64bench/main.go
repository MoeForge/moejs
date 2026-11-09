// Command base64bench runs base64-bench.js, the base64 benchmark of
// atob, btoa, Uint8Array's toBase64 and fromBase64 and Buffer's base64
// and base64url, in moejs with package node's Buffer:
//
//	base64bench -js base64-bench.js [-only 'atob 1048576'] [-engine name]
//	node base64-bench.js
//
// Each case prints a line of JSON with its median ns/op.
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
	"github.com/MoeForge/moejs/node"
)

func main() {
	js := flag.String("js", "base64-bench.js", "the benchmark script")
	only := flag.String("only", "", "run the cases whose name holds this")
	name := flag.String("engine", "moejs", "the engine name the results carry")
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
	api, err := node.InstallLoop(loop, node.Options{Buffer: true})
	check(err)
	defer api.Close()
	epoch := time.Now()
	check(rt.SetGlobal("benchNow", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Number(float64(time.Since(epoch).Nanoseconds()) / 1e6), nil
	})))
	check(rt.SetGlobal("benchOnly", moejs.String(*only)))
	check(rt.SetGlobal("benchEngine", moejs.String(*name)))
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

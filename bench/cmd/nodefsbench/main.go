// Command nodefsbench measures package node's node:fs and Buffer against
// Node.js with the same script (fs-bench.mjs) over the same fixture tree:
//
//	nodefsbench -fixture DIR                 # writes the fixture tree
//	nodefsbench -js fs-bench.mjs -dir DIR    # the script in moejs
//	node fs-bench.mjs DIR                    # the script in Node.js
//
// The fixture has a 4 KiB and a 1 MiB text file, a directory of 1,000
// files, a source tree of about 3,000 files in nested directories, and
// 64 KiB of binary data.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/node"
)

func main() {
	fixture := flag.String("fixture", "", "write the fixture tree into this directory and exit")
	js := flag.String("js", "", "the benchmark script")
	dir := flag.String("dir", "", "the fixture directory")
	only := flag.String("only", "", "run the cases whose name holds this")
	cpu := flag.String("cpuprofile", "", "write a CPU profile")
	flag.Parse()
	if *fixture != "" {
		check(writeFixture(*fixture))
		return
	}
	if *cpu != "" {
		f, err := os.Create(*cpu)
		check(err)
		check(pprof.StartCPUProfile(f))
		defer pprof.StopCPUProfile()
	}
	src, err := os.ReadFile(*js)
	check(err)
	abs, err := filepath.Abs(*dir)
	check(err)
	abs, err = filepath.EvalSymlinks(abs)
	check(err)
	mod, err := moejs.Compile(*js, string(src))
	check(err)
	mod, err = moejs.Link(mod, node.Resolver(nil))
	check(err)
	rt := moejs.NewRuntime(moejs.Options{Importer: node.Importer(nil)})
	loop, err := eventloop.New(rt, eventloop.Options{})
	check(err)
	api, err := node.InstallLoop(loop, node.Options{FS: &node.FSOptions{Root: abs}, Buffer: true})
	check(err)
	defer api.Close()
	epoch := time.Now()
	check(rt.SetGlobal("benchNow", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Number(float64(time.Since(epoch).Nanoseconds()) / 1e6), nil
	})))
	check(rt.SetGlobal("benchDir", moejs.String(abs)))
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

func writeFixture(dir string) error {
	rng := rand.New(rand.NewPCG(1, 2))
	text := func(n int) []byte {
		const words = "const let function return import export from interface type async await if else for of in "
		b := make([]byte, 0, n)
		for len(b) < n {
			i := rng.IntN(len(words) - 8)
			b = append(b, words[i:i+8]...)
			if rng.IntN(12) == 0 {
				b = append(b, '\n')
			}
		}
		return b[:n]
	}
	write := func(name string, data []byte) error {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, data, 0o644)
	}
	if err := write("small.txt", text(4096)); err != nil {
		return err
	}
	if err := write("big.txt", text(1<<20)); err != nil {
		return err
	}
	bin := make([]byte, 64<<10)
	for i := range bin {
		bin[i] = byte(rng.IntN(256))
	}
	if err := write("b64.bin", bin); err != nil {
		return err
	}
	for i := range 1000 {
		if err := write(fmt.Sprintf("flat/file-%04d.txt", i), text(100+rng.IntN(400))); err != nil {
			return err
		}
	}
	exts := []string{".ts", ".ts", ".ts", ".tsx", ".js", ".json", ".md"}
	var walk func(d string, depth int) error
	count := 0
	walk = func(d string, depth int) error {
		for i := range 8 + rng.IntN(8) {
			if err := write(fmt.Sprintf("%s/m%02d%s", d, i, exts[rng.IntN(len(exts))]), text(200)); err != nil {
				return err
			}
			count++
		}
		if depth < 4 {
			for i := range 2 + rng.IntN(3) {
				if err := walk(fmt.Sprintf("%s/pkg%d", d, i), depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk("tree", 0); err != nil {
		return err
	}
	log.Printf("fixture: %d files in the tree", count)
	return nil
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

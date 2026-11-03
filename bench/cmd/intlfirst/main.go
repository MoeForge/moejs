// Command intlfirst times the first use of Intl in a fresh process: it
// creates a runtime, compiles the script given as its argument, runs it once
// and prints how long the run took, in microseconds.
// bench/scripts/intl_first_node.mjs does the same in Node.js:
//
//	TZ=UTC intlfirst '(1234.5).toLocaleString()'
//	TZ=UTC node bench/scripts/intl_first_node.mjs '(1234.5).toLocaleString()'
package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/MoeForge/moejs"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: intlfirst SCRIPT")
	}
	rt := moejs.NewRuntime(moejs.Options{})
	s, err := moejs.CompileScript("first.js", os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	t0 := time.Now()
	v, err := rt.RunScript(s)
	d := time.Since(t0)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%.1f us\t%s\n", float64(d.Nanoseconds())/1e3, v.String())
}

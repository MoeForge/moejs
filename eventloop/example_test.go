package eventloop_test

import (
	"errors"
	"fmt"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
)

// ExampleLoop_Run runs a script that sets timers: Run returns once the
// last timer ran. The promise jobs of the script run before the loop
// starts.
func ExampleLoop_Run() {
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{})
	if err != nil {
		panic(err)
	}
	err = rt.SetGlobal("print", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		fmt.Println(moejs.Arg(args, 0).String())
		return moejs.Undefined(), nil
	}))
	if err != nil {
		panic(err)
	}
	script, err := moejs.CompileScript("main.js", `
let n = 0;
const tick = setInterval(() => {
	print("tick " + ++n);
	if (n === 3) clearInterval(tick);
}, 10);
setTimeout(() => print("timeout"), 5);
Promise.resolve().then(() => print("job"));
print("script");`)
	if err != nil {
		panic(err)
	}
	err = loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(script)
		return err
	})
	if err != nil {
		panic(err)
	}
	// Output:
	// script
	// job
	// timeout
	// tick 1
	// tick 2
	// tick 3
}

// ExampleLoop_NewPromise gives JavaScript an asynchronous host function: it
// returns a promise of NewPromise and does its work on another goroutine,
// which settles the promise on the loop. The Go error of a failed lookup
// rejects it with an Error.
func ExampleLoop_NewPromise() {
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{})
	if err != nil {
		panic(err)
	}
	err = rt.SetGlobal("lookup", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		name := moejs.Arg(args, 0).String()
		p, settle := loop.NewPromise()
		go func() {
			// Blocking work, such as I/O, runs off the loop.
			var err error
			if name == "" {
				err = errors.New("empty name")
			}
			settle(func(rt *moejs.Runtime) (moejs.Value, error) {
				if err != nil {
					return moejs.Undefined(), err
				}
				return rt.FromGo(map[string]any{"name": name, "length": len(name)})
			})
		}()
		return p, nil
	}))
	if err != nil {
		panic(err)
	}
	mod, err := moejs.Compile("plugin.js", `export async function describe(name) {
	try {
		const r = await lookup(name);
		return r.name + " has " + r.length + " letters";
	} catch (e) {
		return "failed: " + e.message;
	}
}`)
	if err != nil {
		panic(err)
	}
	describe, err := mod.Hook("describe")
	if err != nil {
		panic(err)
	}
	var results []moejs.Value
	err = loop.Run(func(rt *moejs.Runtime) error {
		if err := rt.Load(mod); err != nil {
			return err
		}
		for _, name := range []string{"moejs", ""} {
			p, err := rt.Call(describe, moejs.String(name))
			if err != nil {
				return err
			}
			results = append(results, p)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	// Run returned: every promise of NewPromise is settled.
	for _, p := range results {
		_, v, _ := moejs.PromiseResult(p)
		fmt.Println(v.String())
	}
	// Output:
	// moejs has 5 letters
	// failed: empty name
}

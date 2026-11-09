package eventloop_test

import (
	"errors"
	"fmt"
	"time"

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

// ExampleLoop_hooks gives a host a CPU-time watchdog and a limited number
// of execution slots, through Options.OnIdle and OnWake: the watchdog
// counts only the time the loop runs JavaScript, and the slot is free
// while the loop waits for a lookup that takes longer than the whole
// budget. The lookup does not stop the loop, but the endless loop after it
// does.
func ExampleLoop_hooks() {
	slots := make(chan struct{}, 1) // one execution slot
	rt := moejs.NewRuntime(moejs.Options{})
	var loop *eventloop.Loop
	wd := &watchdog{budget: 50 * time.Millisecond, expire: func() { loop.Interrupt("CPU budget spent") }}
	loop, err := eventloop.New(rt, eventloop.Options{
		OnIdle: func() {
			wd.pause()
			<-slots
		},
		OnWake: func() error {
			slots <- struct{}{} // waits for a free slot
			wd.resume()
			return nil
		},
	})
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
	err = rt.SetGlobal("lookup", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		p, settle := loop.NewPromise()
		go func() {
			time.Sleep(100 * time.Millisecond) // I/O
			settle(func(*moejs.Runtime) (moejs.Value, error) { return moejs.String("found"), nil })
		}()
		return p, nil
	}))
	if err != nil {
		panic(err)
	}
	script, err := moejs.CompileScript("main.js", `(async () => {
	print("lookup: " + await lookup());
	for (;;) {}
})();`)
	if err != nil {
		panic(err)
	}

	slots <- struct{}{} // the run takes a slot
	wd.resume()
	err = loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(script)
		return err
	})
	if wd.running {
		// The run ended awake, holding its slot; one that ended idle
		// paused the watchdog and released the slot in OnIdle.
		wd.pause()
		<-slots
	}
	var ie *moejs.InterruptedError
	if errors.As(err, &ie) {
		fmt.Println("stopped:", ie.Value)
		rt.ClearInterrupt()
	}
	fmt.Println("free slots:", cap(slots)-len(slots))
	// Output:
	// lookup: found
	// stopped: CPU budget spent
	// free slots: 1
}

// watchdog calls expire once the time it ran, between resume and pause,
// adds up to budget.
type watchdog struct {
	budget  time.Duration
	expire  func()
	running bool
	used    time.Duration
	since   time.Time
	timer   *time.Timer
}

func (w *watchdog) resume() {
	w.running, w.since = true, time.Now()
	w.timer = time.AfterFunc(w.budget-w.used, w.expire)
}

func (w *watchdog) pause() {
	w.timer.Stop()
	w.running, w.used = false, w.used+time.Since(w.since)
}

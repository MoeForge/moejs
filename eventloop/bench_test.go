package eventloop_test

// The event loop's cost per callback, task and promise round trip, with no
// memory limit and with one (whose measurements the loop paces).

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
)

// benchLoop returns a loop over a runtime with the memory limit limit (0:
// none) and the host function tool, whose promise of NewPromise another
// goroutine settles.
func benchLoop(b *testing.B, limit int64) *eventloop.Loop {
	rt := moejs.NewRuntime(moejs.Options{MemoryLimit: limit})
	loop, err := eventloop.New(rt, eventloop.Options{})
	if err != nil {
		b.Fatal(err)
	}
	err = rt.SetGlobal("tool", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		p, settle := loop.NewPromise()
		go settle(nil)
		return p, nil
	}))
	if err != nil {
		b.Fatal(err)
	}
	return loop
}

func benchRun(b *testing.B, loop *eventloop.Loop, src string) {
	s, err := moejs.CompileScript("bench.js", src)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	err = loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(s)
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
}

var benchLimits = []struct {
	name  string
	limit int64
}{{"nolimit", 0}, {"limit64M", 64 << 20}}

// BenchmarkLoopTimeout: a chain of setTimeout(0), one callback per op.
func BenchmarkLoopTimeout(b *testing.B) {
	for _, l := range benchLimits {
		b.Run(l.name, func(b *testing.B) {
			b.ReportAllocs()
			benchRun(b, benchLoop(b, l.limit), fmt.Sprintf(`let n = 0; function step() { if (++n < %d) setTimeout(step, 0); } setTimeout(step, 0);`, b.N))
		})
	}
}

// BenchmarkLoopImmediate: a chain of setImmediate, one callback per op.
func BenchmarkLoopImmediate(b *testing.B) {
	for _, l := range benchLimits {
		b.Run(l.name, func(b *testing.B) {
			b.ReportAllocs()
			benchRun(b, benchLoop(b, l.limit), fmt.Sprintf(`let n = 0; function step(x) { if (++n < %d) setImmediate(step, x); } setImmediate(step, {});`, b.N))
		})
	}
}

// BenchmarkLoopPromisePingPong: an async function awaiting a promise
// of NewPromise that another goroutine settles, one round trip per op.
func BenchmarkLoopPromisePingPong(b *testing.B) {
	for _, l := range benchLimits {
		b.Run(l.name, func(b *testing.B) {
			b.ReportAllocs()
			benchRun(b, benchLoop(b, l.limit), fmt.Sprintf(`(async () => { for (let i = 0; i < %d; i++) await tool(); })();`, b.N))
		})
	}
}

// BenchmarkLoopPromiseSettled: an async function awaiting a promise
// of NewPromise that the host function settles before it returns, so the
// settle task runs on the loop's next turn, one round trip per op.
func BenchmarkLoopPromiseSettled(b *testing.B) {
	for _, l := range benchLimits {
		b.Run(l.name, func(b *testing.B) {
			b.ReportAllocs()
			loop := benchLoop(b, l.limit)
			err := loop.Runtime().SetGlobal("tool", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
				p, settle := loop.NewPromise()
				settle(nil)
				return p, nil
			}))
			if err != nil {
				b.Fatal(err)
			}
			benchRun(b, loop, fmt.Sprintf(`(async () => { for (let i = 0; i < %d; i++) await tool(); })();`, b.N))
		})
	}
}

// BenchmarkLoopTasks: tasks posted with RunOnLoop from another
// goroutine, each calling a JavaScript function, one task per op.
func BenchmarkLoopTasks(b *testing.B) {
	for _, l := range benchLimits {
		b.Run(l.name, func(b *testing.B) {
			b.ReportAllocs()
			loop := benchLoop(b, l.limit)
			s, err := moejs.CompileScript("bench.js", `globalThis.count = 0; globalThis.f = () => { count++; }; f;`)
			if err != nil {
				b.Fatal(err)
			}
			n := b.N
			b.ResetTimer()
			err = loop.Run(func(rt *moejs.Runtime) error {
				f, err := rt.RunScript(s)
				if err != nil {
					return err
				}
				done := loop.Hold()
				go func() {
					for i := 0; i < n; i++ {
						loop.RunOnLoop(func(rt *moejs.Runtime) {
							if _, err := rt.CallFunction(f, moejs.Undefined()); err != nil {
								panic(err)
							}
						})
					}
					done(nil)
				}()
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
		})
	}
}

package tests

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Module initialization, calls and result getters share one cancellation scope.
func TestRunContextRoundTrip(t *testing.T) {
	mod, err := moejs.Compile("process.js", `
export function process(input) {
  return {get name() { return input.name.trim(); }, active: input.active};
}`)
	require.NoError(t, err)
	hook := mustHook(t, mod, "process")
	for _, cancellable := range []bool{false, true} {
		t.Run(fmt.Sprint(cancellable), func(t *testing.T) {
			ctx := context.Background()
			if cancellable {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			rt := moejs.NewRuntime(moejs.Options{})
			var out struct {
				Name   string `json:"name"`
				Active bool   `json:"active"`
			}
			err := rt.RunContext(ctx, func(r *moejs.Runtime) error {
				require.Same(t, rt, r)
				if err := r.Load(mod); err != nil {
					return err
				}
				arg, err := r.FromGo(map[string]any{"name": "  sample  ", "active": true})
				if err != nil {
					return err
				}
				value, err := r.Call(hook, arg)
				if err != nil {
					return err
				}
				return r.Unmarshal(value, &out)
			})
			require.NoError(t, err)
			assert.Equal(t, "sample", out.Name)
			assert.True(t, out.Active)
		})
	}
}

func TestRunContextCanceledBeforeEntry(t *testing.T) {
	cause := errors.New("request closed")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	rt := moejs.NewRuntime(moejs.Options{})
	err := rt.RunContext(ctx, func(*moejs.Runtime) error {
		t.Fatal("canceled scope ran its callback")
		return nil
	})
	require.ErrorAs(t, err, new(*moejs.InterruptedError))
	require.ErrorIs(t, err, cause)
	// Refusing entry did not install a watcher or mutate the runtime.
	require.NoError(t, rt.RunContext(context.Background(), func(*moejs.Runtime) error { return nil }))
}

func TestRunContextPendingInterrupt(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	cause := errors.New("host shutdown")
	rt.Interrupt(cause)
	err := rt.RunContext(t.Context(), func(*moejs.Runtime) error {
		t.Fatal("scope ran with a pending interrupt")
		return nil
	})
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, rt.Realm().CheckInterrupt(), cause)
	rt.ClearInterrupt()
}

func TestRunContextCancellationStages(t *testing.T) {
	for _, stage := range []string{"load", "call", "decode", "script"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("request canceled")
			rt := moejs.NewRuntime(moejs.Options{})
			// Signal cancellation from the exact phase under test. The loop
			// then has to observe the watcher running on another goroutine.
			require.NoError(t, rt.SetGlobal("cancel", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
				cancel(cause)
				return moejs.Undefined(), nil
			})))
			var run func(*moejs.Runtime) error
			if stage == "script" {
				script, err := moejs.CompileScript("cancel.js", "cancel(); for (;;) {}")
				require.NoError(t, err)
				run = func(r *moejs.Runtime) error { _, err := r.RunScript(script); return err }
			} else {
				source := map[string]string{
					"load":   `cancel(); for (;;) {} export function run() { return 1; }`,
					"call":   `export function run() { cancel(); for (;;) {} }`,
					"decode": `export function run() { return {get value() { cancel(); for (;;) {} }}; }`,
				}[stage]
				mod, err := moejs.Compile("cancel.js", source)
				require.NoError(t, err)
				hook := mustHook(t, mod, "run")
				run = func(r *moejs.Runtime) error {
					if err := r.Load(mod); err != nil {
						return err
					}
					value, err := r.Call(hook)
					if err != nil {
						return err
					}
					var out map[string]string
					return r.Unmarshal(value, &out)
				}
			}
			err := rt.RunContext(ctx, run)
			require.ErrorAs(t, err, new(*moejs.InterruptedError))
			require.ErrorIs(t, err, cause)
			require.ErrorIs(t, rt.Realm().CheckInterrupt(), cause)
			rt.ClearInterrupt()
			rt.ReleaseCallData()
			// A canceled Load leaves that module failed; other scripts can
			// still run, and a canceled hook does not poison later work.
			script, err := moejs.CompileScript("next.js", "6 * 7")
			require.NoError(t, err)
			require.NoError(t, rt.RunContext(context.Background(), func(r *moejs.Runtime) error {
				v, err := r.RunScript(script)
				assert.Equal(t, moejs.Int(42), v)
				return err
			}))
		})
	}
}

func TestRunContextDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		rt := moejs.NewRuntime(moejs.Options{})
		err := rt.RunContext(ctx, func(*moejs.Runtime) error {
			<-ctx.Done() // Cooperative host work; fake time advances here.
			return nil
		})
		require.ErrorAs(t, err, new(*moejs.InterruptedError))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		rt.ClearInterrupt()
		synctest.Wait()
		require.NoError(t, rt.Realm().CheckInterrupt())
	})
}

func TestRunContextPreservesErrors(t *testing.T) {
	for _, tc := range []struct {
		source      string
		memoryLimit bool
	}{
		{`export function run() { throw new Error("bad input"); }`, false},
		{`export function run() { const keep = []; for (;;) keep.push({value: keep.length}); }`, true},
	} {
		t.Run(tc.source, func(t *testing.T) {
			mod, err := moejs.Compile("error.js", tc.source)
			require.NoError(t, err)
			rt := moejs.NewRuntime(moejs.Options{MemoryLimit: 1 << 20})
			require.NoError(t, rt.Load(mod))
			hook := mustHook(t, mod, "run")
			var inner error
			err = rt.RunContext(t.Context(), func(r *moejs.Runtime) error {
				_, inner = r.Call(hook)
				return inner
			})
			require.Error(t, inner)
			require.Same(t, inner, err)
			if tc.memoryLimit {
				require.ErrorIs(t, err, moejs.ErrMemoryLimit)
				require.ErrorIs(t, rt.Realm().CheckInterrupt(), moejs.ErrMemoryLimit)
			} else {
				require.ErrorAs(t, err, new(*moejs.Exception))
			}
		})
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rt := moejs.NewRuntime(moejs.Options{})
		want := errors.New("decode failed")
		err := rt.RunContext(ctx, func(*moejs.Runtime) error {
			cancel()
			return want
		})
		require.Same(t, want, err)
		rt.ClearInterrupt()
		synctest.Wait()
		require.NoError(t, rt.Realm().CheckInterrupt())
	})
}

func TestRunContextWatcherCleanup(t *testing.T) {
	for _, panicInScope := range []bool{false, true} {
		for _, cancelInScope := range []bool{false, true} {
			t.Run(fmt.Sprintf("panic=%t/cancel=%t", panicInScope, cancelInScope), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					rt := moejs.NewRuntime(moejs.Options{})
					var err error
					run := func() {
						err = rt.RunContext(ctx, func(*moejs.Runtime) error {
							if cancelInScope {
								cancel()
								synctest.Wait() // Ensure the watcher has run.
							}
							if panicInScope {
								panic("host panic")
							}
							return nil
						})
					}
					if panicInScope {
						require.PanicsWithValue(t, "host panic", run)
					} else {
						run()
						if cancelInScope {
							require.ErrorIs(t, err, context.Canceled)
						} else {
							require.NoError(t, err)
						}
					}
					rt.ClearInterrupt()
					cancel() // Must not reach this runtime after the scope.
					synctest.Wait()
					require.NoError(t, rt.Realm().CheckInterrupt())
				})
			})
		}
	}
}

func TestRunContextReuseRace(t *testing.T) {
	mod, err := moejs.Compile("reuse.js", "export function render() { return 42; }")
	require.NoError(t, err)
	hook := mustHook(t, mod, "render")
	rt := moejs.NewRuntime(moejs.Options{})
	require.NoError(t, rt.Load(mod))
	for range 1000 {
		ctx, cancel := context.WithCancel(context.Background())
		start, done := make(chan struct{}), make(chan struct{})
		go func() {
			<-start
			cancel()
			close(done)
		}()
		err := rt.RunContext(ctx, func(*moejs.Runtime) error { close(start); return nil })
		if err != nil {
			require.ErrorIs(t, err, context.Canceled)
		}
		rt.ClearInterrupt()
		<-done
		require.NoError(t, rt.RunContext(context.Background(), func(r *moejs.Runtime) error {
			value, err := r.Call(hook)
			assert.Equal(t, moejs.Int(42), value)
			return err
		}))
	}
}

func TestRunContextJavaScriptDeadline(t *testing.T) {
	script, err := moejs.CompileScript("loop.js", "for (;;) {}")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rt := moejs.NewRuntime(moejs.Options{})
	err = rt.RunContext(ctx, func(r *moejs.Runtime) error {
		_, err := r.RunScript(script)
		return err
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func BenchmarkRunContext(b *testing.B) {
	mod, err := moejs.Compile("render.js", "export function render() { return 42; }")
	require.NoError(b, err)
	hook := mustHook(b, mod, "render")
	for _, mode := range []string{"direct", "background", "cancellable", "manual"} {
		b.Run(mode, func(b *testing.B) {
			rt := moejs.NewRuntime(moejs.Options{})
			require.NoError(b, rt.Load(mod))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			call := func(r *moejs.Runtime) error { _, err := r.Call(hook); return err }
			var run func() error
			switch mode {
			case "direct":
				run = func() error { return call(rt) }
			case "background":
				run = func() error { return rt.RunContext(context.Background(), call) }
			case "cancellable":
				run = func() error { return rt.RunContext(ctx, call) }
			case "manual":
				// The existing host-side AfterFunc/join pattern, for comparison.
				run = func() error {
					done := make(chan struct{})
					stop := context.AfterFunc(ctx, func() { rt.Interrupt(context.Cause(ctx)); close(done) })
					defer func() {
						if !stop() {
							<-done
						}
					}()
					return call(rt)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func ExampleRuntime_RunContext() {
	mod, _ := moejs.Compile("normalize.js", `
export function normalize(input) { return {name: input.name.trim()}; }
`)
	hook, _ := mod.Hook("normalize")
	rt := moejs.NewRuntime(moejs.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out struct {
		Name string `json:"name"`
	}
	err := rt.RunContext(ctx, func(r *moejs.Runtime) error {
		if err := r.Load(mod); err != nil {
			return err
		}
		arg, err := r.FromGo(map[string]string{"name": "  sample  "})
		if err != nil {
			return err
		}
		value, err := r.Call(hook, arg)
		if err != nil {
			return err
		}
		return r.Unmarshal(value, &out)
	})
	// The watcher is stopped or joined before cleanup and reuse.
	rt.ClearInterrupt()
	rt.ReleaseCallData()
	fmt.Println(out.Name, err)
	// Output: sample <nil>
}

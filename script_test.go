package moejs_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileScript(t *testing.T) {
	s, err := moejs.CompileScript("s.js", `var x = 1; x + 1`)
	require.NoError(t, err)
	assert.Equal(t, "s.js", s.Name())

	_, err = moejs.CompileScript("bad.js", "var = ;")
	var se *moejs.SyntaxError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "bad.js", se.File)

	// Scripts are sloppy unless they say "use strict"; modules never are.
	_, err = moejs.CompileScript("with.js", `with ({}) {}`)
	require.NoError(t, err)
	_, err = moejs.CompileScript("strict.js", `"use strict"; with ({}) {}`)
	require.ErrorAs(t, err, &se)
	_, err = moejs.Compile("mod.js", `with ({}) {}`)
	require.ErrorAs(t, err, &se)
}

func TestRunScript(t *testing.T) {
	tests := []struct {
		name string
		srcs []string
		want any
	}{
		{"completion value", []string{`1; 2 + 3`}, int64(5)},
		{"empty completion", []string{`var a;`}, nil},
		{"sloppy this", []string{`(function () { return this === globalThis; })()`}, true},
		{"implicit global", []string{`undeclared = 7;`, `undeclared`}, int64(7)},
		{"var is a property", []string{`var v = 1;`, `globalThis.v`}, int64(1)},
		{"let is not a property", []string{`let l = 1;`, `[l, "l" in globalThis]`}, []any{int64(1), false}},
		{"function declaration", []string{`function f() { return "f"; }`, `f()`}, "f"},
		{"with", []string{`var o = {a: 2}; with (o) { a *= 3; } o.a`}, int64(6)},
		{"mapped arguments", []string{`function f(a) { arguments[0] = 9; return a; } f(1)`}, int64(9)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := moejs.NewRuntime(moejs.Options{})
			var v moejs.Value
			for _, src := range tc.srcs {
				s, err := moejs.CompileScript("t.js", src)
				require.NoError(t, err)
				v, err = rt.RunScript(s)
				require.NoError(t, err)
			}
			got, err := rt.ToGo(v)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRunScriptErrors(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	run := func(src string) error {
		s, err := moejs.CompileScript("e.js", src)
		require.NoError(t, err)
		v, err := rt.RunScript(s)
		if err != nil {
			assert.True(t, v.IsUndefined())
		}
		return err
	}
	var exc *moejs.Exception
	require.ErrorAs(t, run(`throw new TypeError("boom")`), &exc)
	assert.Equal(t, "TypeError: boom", exc.Error()[:len("TypeError: boom")])

	// A conflicting declaration throws before anything runs.
	require.NoError(t, run(`let taken = 1;`))
	require.ErrorAs(t, run(`var fresh = 1; var taken;`), &exc)
	assert.Equal(t, "SyntaxError", exc.Name())
	v, err := rt.RunScript(mustScript(t, `typeof fresh`))
	require.NoError(t, err)
	assert.Equal(t, "undefined", v.String())

	// A throwing job is the error, as for Call.
	require.ErrorAs(t, run(`queueMicrotask(() => { throw new RangeError("job"); })`), &exc)
	assert.Equal(t, "RangeError", exc.Name())

	// Interrupts.
	rt.Interrupt("stop")
	var ie *moejs.InterruptedError
	require.ErrorAs(t, run(`globalThis.ran = true`), &ie)
	assert.Equal(t, "stop", ie.Value)
	rt.ClearInterrupt()
	v, err = rt.RunScript(mustScript(t, `typeof ran`))
	require.NoError(t, err)
	assert.Equal(t, "undefined", v.String(), "an interrupted script does not start")

	loop := mustScript(t, `for (;;) {}`)
	done := make(chan error)
	go func() { _, err := rt.RunScript(loop); done <- err }()
	rt.Interrupt("late")
	require.ErrorAs(t, <-done, &ie)
	rt.ClearInterrupt()

	// A Go panic out of a host function is an *InternalError and leaves the
	// runtime usable.
	require.NoError(t, rt.SetGlobal("boom", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		panic("host bug")
	})))
	var internal *moejs.InternalError
	require.ErrorAs(t, run(`boom()`), &internal)
	v, err = rt.RunScript(mustScript(t, `1 + 1`))
	require.NoError(t, err)
	assert.Equal(t, "2", v.String())
}

func mustScript(t *testing.T, src string) *moejs.Script {
	t.Helper()
	s, err := moejs.CompileScript("m.js", src)
	require.NoError(t, err)
	return s
}

// TestScriptsAndModules checks that a module sees the global lexical
// bindings of scripts run before and after Load, and scripts see the
// module's global writes.
func TestScriptsAndModules(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	_, err := rt.RunScript(mustScript(t, `let before = "b"; var shared = 1;`))
	require.NoError(t, err)
	mod, err := moejs.Compile("m.js", `
export function read() { return [before, typeof after === "undefined" ? "-" : after, shared]; }
export function bump() { shared++; globalThis.fromModule = true; }
`)
	require.NoError(t, err)
	require.NoError(t, rt.Load(mod))
	read, err := mod.Hook("read")
	require.NoError(t, err)
	bump, err := mod.Hook("bump")
	require.NoError(t, err)

	v, err := rt.Call(read)
	require.NoError(t, err)
	got, _ := rt.ToGo(v)
	assert.Equal(t, []any{"b", "-", int64(1)}, got)

	_, err = rt.RunScript(mustScript(t, `const after = "a";`))
	require.NoError(t, err)
	_, err = rt.Call(bump)
	require.NoError(t, err)
	v, err = rt.Call(read)
	require.NoError(t, err)
	got, _ = rt.ToGo(v)
	assert.Equal(t, []any{"b", "a", int64(2)}, got)

	v, err = rt.RunScript(mustScript(t, `fromModule && shared`))
	require.NoError(t, err)
	assert.Equal(t, "2", v.String())
}

// TestSetGlobalShadowedByLet: SetGlobal writes the global object, which a
// script's global lexical binding of the same name shadows (9.1.1.4.1).
func TestSetGlobalShadowedByLet(t *testing.T) {
	rt := moejs.NewRuntime(moejs.Options{})
	_, err := rt.RunScript(mustScript(t, `let x = "let"; var v = "var";`))
	require.NoError(t, err)
	require.NoError(t, rt.SetGlobal("x", "host"))
	require.NoError(t, rt.SetGlobal("v", "host"))
	v, err := rt.RunScript(mustScript(t, `[x, globalThis.x, v].join()`))
	require.NoError(t, err)
	assert.Equal(t, "let,host,host", v.String())
}

// TestScriptSharedAcrossRuntimes runs one compiled script in many runtimes
// at once.
func TestScriptSharedAcrossRuntimes(t *testing.T) {
	s := mustScript(t, `var n = 0; function f(a) { with ({a: a}) { n += a; } return n; } let total = f(1) + f(2); total`)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				// A second run in one runtime would redeclare total.
				rt := moejs.NewRuntime(moejs.Options{})
				v, err := rt.RunScript(s)
				if err == nil && v.String() != "4" {
					err = errors.New("wrong result " + v.String())
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestAssignmentThrowKeepsVariable: an assignment whose value throws leaves
// the variable as it was, also where a handler of the same function sees it
// (the value is not built in the variable's register).
func TestAssignmentThrowKeepsVariable(t *testing.T) {
	tests := []struct{ name, body string }{
		{"call", `a = thrower();`},
		{"method call", `a = o.m();`},
		{"new", `a = new thrower();`},
		{"var initializer", `var a = thrower();`},
		{"logical assignment", `a &&= thrower();`},
		{"array", `a = [2, thrower()];`},
		{"spread", `a = [...thrower()];`},
		{"object", `a = {x: thrower()};`},
		{"template", "a = `${b}${thrower()}`;"},
		{"conditional", `a = b ? thrower() : 3;`},
		{"logical", `a = b && thrower();`},
		{"nullish", `a = o.x ?? thrower();`},
		{"sequence", `a = (b, thrower());`},
		{"in a loop", `for (var i = 0; i < 2; i++) a = thrower();`},
		{"parameter", `p = thrower();`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := `function thrower() { throw 0; }
var o = {}, b = 2;
function f(p) { var a = 1; try { ` + tc.body + ` } catch (e) {} return [a, p]; }
function g(p) { var a = 1, seen; try { ` + tc.body + ` } finally { seen = [a, p]; return seen; } }
[f(1), g(1)]`
			rt := moejs.NewRuntime(moejs.Options{})
			v, err := rt.RunScript(mustScript(t, src))
			require.NoError(t, err)
			got, err := rt.ToGo(v)
			require.NoError(t, err)
			one := []any{int64(1), int64(1)}
			assert.Equal(t, []any{one, one}, got)
		})
	}

	// A generator resumed with a throw in the middle of the value.
	rt := moejs.NewRuntime(moejs.Options{})
	v, err := rt.RunScript(mustScript(t, `function* g() { var a = 1; try { a = [2, yield]; } catch (e) { return a; } }
var it = g(); it.next(); it.throw(0).value`))
	require.NoError(t, err)
	assert.Equal(t, "1", v.String())
}

package engine

// Audit regression tests: interrupt-then-reuse at every observable point,
// the intern table under contention and the shared-intrinsic write guards.
// The reproducers of findings 1-4 (string length limit, FromGo map order,
// process-wide atoms, lazy prototype hole) live ungated in
// string_limit_test.go, hostconv_test.go, intern_test.go and
// lazy_prototype_test.go.

import (
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/require"
)

// evalModuleWith is evalModule for a realm created with opts.
func evalModuleWith(t *testing.T, src string, opts RealmOptions) *moduleFixture {
	t.Helper()
	m, err := syntax.ParseModule("audit.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealmWith(opts)
	env, err := r.EvaluateModule(code)
	require.NoError(t, err)
	return &moduleFixture{t: t, r: r, env: env}
}

// --- Checklist C: interrupt at every observable point, then reuse ----------

// tickInterrupter is a native the hook calls between operations; it
// interrupts the realm on the target-th call so every run stops at a
// different, deterministic point (no timers, no randomness).
type tickInterrupter struct {
	n, target int
}

func (ti *tickInterrupter) native(r *Realm, _ Value, _ []Value) (Value, error) {
	ti.n++
	if ti.n == ti.target {
		r.Interrupt("audit")
	}
	return Undefined(), nil
}

const auditInterruptHook = `
export function hook(ctx) {
  let acc = 0;
  for (let i = 0; i < 6; i++) {
    tick();
    const s = "ab".repeat(1000);
    tick();
    const parts = s.split("a");
    tick();
    const j = JSON.parse(JSON.stringify({a: parts.length, b: [1, 2, 3], c: ctx}));
    tick();
    const arr = [];
    for (let k = 0; k < 20; k++) { tick(); arr.push((k * 7919) % 101); }
    arr.sort((x, y) => { tick(); return x - y; });
    tick();
    acc += arr.map(x => { tick(); return x + 1; }).filter(x => x % 2).reduce((p, c) => p + c, 0);
    try { tick(); inner(i); } catch (e) { tick(); acc += e.message.length; } finally { tick(); }
    acc += j.a + s.length + ("x" + i).length + [1, 2, 3].join("-").length + /b+/g.exec(s).index;
  }
  return acc;
}
function inner(i) { tick(); if (i % 3 === 0) throw new Error("boom" + i); return deeper(i, 5); }
function deeper(i, d) { tick(); return d === 0 ? i : deeper(i, d - 1); }
`

func TestAuditInterruptEveryPointThenReuse(t *testing.T) {
	f := evalModule(t, auditInterruptHook)
	r := f.r
	ti := &tickInterrupter{}
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("tick"), ObjectValue(r.NewNativeFunction(AtomEmpty, 0, ti.native))))
	ctx := map[string]any{"model": "m", "n": 3, "list": []any{1, "two", map[string]any{"k": true}}}

	checkIdle := func(what string) {
		t.Helper()
		require.Equal(t, 0, r.interp.sp, "%s: register stack pointer leaked", what)
		require.Equal(t, 0, r.interp.nframes, "%s: frame leaked", what)
		require.Zero(t, r.callDepth, "%s: call depth leaked", what)
	}

	ti.target = -1
	want := f.call("hook", ctx)
	total := ti.n
	require.Greater(t, total, 500, "the hook must offer many interrupt points")
	checkIdle("baseline")

	stride := max(1, total/1000)
	runs := 0
	for target := 1; target <= total; target += stride {
		ti.n, ti.target = 0, target
		_, err := f.callErr("hook", ctx)
		var ie *InterruptedError
		require.True(t, errors.As(err, &ie), "target %d: got %v, want *InterruptedError", target, err)
		require.Equal(t, "audit", ie.Value)
		checkIdle("after interrupt at tick " + strconv.Itoa(target))
		r.ClearInterrupt()
		ti.n, ti.target = 0, -1
		require.Equal(t, want, f.call("hook", ctx), "rerun after interrupt at tick %d", target)
		checkIdle("after rerun")
		runs++
	}
	require.GreaterOrEqual(t, runs, 500)
}

// Interrupt requested before the call: every entry point must report it and
// leave the realm reusable.
func TestAuditInterruptPendingBeforeCall(t *testing.T) {
	f := evalModule(t, `export function f(a) { return a + 1; }`)
	r := f.r
	r.Interrupt("pre")
	_, err := f.callErr("f", 1)
	var ie *InterruptedError
	require.True(t, errors.As(err, &ie), "%v", err)
	require.Equal(t, 0, r.interp.sp)
	require.Zero(t, r.callDepth)
	r.ClearInterrupt()
	require.Equal(t, int64(2), f.call("f", 1))
}

// --- Checklist B: interning from many goroutines while realms run ----------

func TestAuditInternTableRace(t *testing.T) {
	m, err := syntax.ParseModule("audit.js", `
export function hook(ctx, i) {
  const o = {};
  for (let k = 0; k < 8; k++) o["dyn_" + i + "_" + k] = k;
  const back = JSON.parse(JSON.stringify(o));
  let sum = 0;
  for (const key in back) sum += back[key] + ctx[key.slice(0, 4) === "dyn_" ? "base" : "none"];
  return sum + Object.keys(ctx).length;
}`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			env, err := r.EvaluateModule(code)
			if err != nil {
				t.Error(err)
				return
			}
			fn, _ := env.GetBindingValue("hook")
			for i := range 64 {
				name := "race_" + strconv.Itoa(g) + "_" + strconv.Itoa(i)
				r.InternGoString(name)
				ctx, err := r.FromGo(map[string]any{"base": 1, name: i, "ué_" + strconv.Itoa(i): "x"})
				if err != nil {
					t.Error(err)
					return
				}
				res, err := r.Call(fn, Undefined(), []Value{ctx, IntValue(g*1000 + i)})
				if err != nil {
					t.Error(err)
					return
				}
				// sum over k of (k + 1) for k in 0..7 = 36, plus 3 keys.
				if got := r.ToGo(res); got != int64(39) {
					t.Errorf("goroutine %d iteration %d: got %v", g, i, got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// --- Checklist B: no JavaScript write path reaches a shared intrinsic ------

func TestAuditSharedIntrinsicWriteGuards(t *testing.T) {
	cases := []string{
		`Array.prototype.push.call(Array.prototype, 1)`,
		`Array.prototype.length = 0`,
		`Object.prototype.x = 1`,
		`Object.defineProperty(Object.prototype, "x", {value: 1})`,
		`Object.defineProperty(Math, "PI", {value: 3})`,
		`Object.setPrototypeOf(Array.prototype, null)`,
		`Object.assign(Math, {PI: 3})`,
		`Math.PI = 3`,
		`JSON.parse = null`,
		`String.prototype.trim = function () {}`,
		`Error.prototype.name = "X"`,
		`Error.prototype.stack = "s"`,
		`Array.prototype[0] = 1`,
		`Array.prototype.splice.call(Array.prototype, 0, 0, 1)`,
		`Array.prototype.unshift.call(Array.prototype, 1)`,
		`RegExp.prototype.lastIndex = 1`,
		`Function.prototype.name = "f"`,
		`Object.freeze(Object.prototype).y = 1`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			m, err := syntax.ParseModule("audit.js", "export function run() { "+src+"; }", syntax.Options{})
			require.NoError(t, err)
			code, err := compiler.CompileModule(m)
			require.NoError(t, err)
			r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			env, err := r.EvaluateModule(code)
			require.NoError(t, err)
			fn, _ := env.GetBindingValue("run")
			_, err = r.Call(fn, Undefined(), nil)
			require.Error(t, err, "write to a shared intrinsic must throw")
			require.Contains(t, errMessage(t, err), "TypeError")
			// The template must be untouched: a fresh realm sees the original values.
			r2 := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			require.False(t, r2.ObjectPrototype.HasOwnProperty(r2.KeyFromGoString("x")))
			require.False(t, r2.ObjectPrototype.HasOwnProperty(r2.KeyFromGoString("y")))
			require.Equal(t, uint32(0), r2.ArrayPrototype.ArrayLength())
			pi, _ := r2.Math.GetOwnDataValue(r2.KeyFromGoString("PI"))
			require.InDelta(t, 3.141592653589793, pi.AsNumber(), 0)
		})
	}
}

package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evalTLA evaluates src as a module in a fresh realm and returns the
// fixture with EvaluateModule's error.
func evalTLA(t *testing.T, src string, shared bool) (*moduleFixture, error) {
	t.Helper()
	m, err := syntax.ParseModule("tla.js", src, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
	env, err := r.EvaluateModule(code)
	return &moduleFixture{t: t, r: r, env: env}, err
}

// TestTopLevelAwait evaluates modules with top-level await in a mutable
// and a shared realm: the evaluation completes in the drain before
// EvaluateModule returns, and the exports are read after it.
func TestTopLevelAwait(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"await order", `const log = []; Promise.resolve().then(() => log.push("p1")).then(() => log.push("p2")); log.push("s"); await null; log.push("m");
await Promise.resolve(); log.push("n"); export const out = log.join();`, "s,p1,m,p2,n"},
		{"statements", `const log = [];
try { await Promise.reject(new Error("r")); } catch (e) { log.push("caught " + e.message); } finally { log.push(await "f"); }
{ let i = 0; const g = () => i; await 0; i = 5; log.push(g()); }
let s = 0; for await (const x of [1, Promise.resolve(2)]) s += x; log.push(s);
log.push(typeof this);
async function* ag() { yield 1; yield 2; } for await (const v of ag()) log.push(v);
label: for (const x of [1, 2]) { await x; if (x == 1) continue label; log.push("x" + x); }
switch (await 3) { case 3: log.push("three"); }
const o = { a: await 1, [await "k"]: 2 }; log.push(JSON.stringify(o));
export const out = log.join("|");`, `caught r|f|5|3|undefined|1|2|x2|three|{"a":1,"k":2}`},
		{"exported function after await", `export let v = 1; v = await Promise.resolve(2); export function out() { return "v" + v; }`, "v2"},
	}
	for _, shared := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s/shared=%v", c.name, shared), func(t *testing.T) {
				f, err := evalTLA(t, c.src, shared)
				require.NoError(t, err)
				v, ok := f.env.GetBindingValue("out")
				require.True(t, ok)
				if v.IsObject() {
					assert.Equal(t, c.want, f.call("out"))
					return
				}
				assert.Equal(t, c.want, f.r.ToGo(v))
			})
		}
	}
}

// TestTopLevelAwaitOutcome checks EvaluateModule's error for a rejected
// and a pending evaluation, and EvaluateModuleAsync's promise.
func TestTopLevelAwaitOutcome(t *testing.T) {
	_, err := evalTLA(t, `await 0; throw new TypeError("boom");`, false)
	var exc *Exception
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, "TypeError: boom", errorDisplayString(exc.Value))

	_, err = evalTLA(t, `throw 7; await 0;`, false)
	require.ErrorAs(t, err, &exc)
	assert.Equal(t, 7.0, exc.Value.AsNumber())

	f, err := evalTLA(t, `export const a = 1; await new Promise(() => {}); export const b = 2;`, false)
	assert.ErrorIs(t, err, ErrModulePending)
	assert.Equal(t, int64(1), f.export("a"))

	evalAsync := func(src string) *Object {
		m, err := syntax.ParseModule("tla.js", src, syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileModule(m)
		require.NoError(t, err)
		_, p, err := NewRealm().EvaluateModuleAsync(code)
		require.NoError(t, err)
		return p
	}
	assert.Nil(t, evalAsync("export const x = 1;"), "a module without top-level await has no promise")
	p := evalAsync(`await new Promise(() => {});`)
	require.NotNil(t, p)
	state, _, ok := p.PromiseResult()
	require.True(t, ok)
	assert.Equal(t, PromisePending, state)
	p = evalAsync(`await 0;`)
	require.NotNil(t, p)
	state, v, _ := p.PromiseResult()
	assert.Equal(t, PromiseFulfilled, state)
	assert.True(t, v.IsUndefined())

	// An interrupt pending on entry stops the evaluation before it
	// initializes any binding, which leaves no environment to read.
	for _, src := range []string{"export function f() {} export const x = 1;", "export const x = 1; await 0;"} {
		m, err := syntax.ParseModule("tla.js", src, syntax.Options{})
		require.NoError(t, err)
		code, err := compiler.CompileModule(m)
		require.NoError(t, err)
		r := NewRealm()
		r.Interrupt("idle")
		menv, p, err := r.EvaluateModuleAsync(code)
		require.ErrorAs(t, err, new(*InterruptedError), src)
		assert.Nil(t, menv, src)
		assert.Nil(t, p, src)
	}
}

// TestTopLevelAwaitRejectionTracked checks that a rejected evaluation is
// reported to the rejection tracker like any unhandled rejection.
func TestTopLevelAwaitRejectionTracked(t *testing.T) {
	m, err := syntax.ParseModule("tla.js", `await 0; throw "r";`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	var ops []PromiseRejectionOperation
	r.SetPromiseRejectionTracker(func(p *Object, op PromiseRejectionOperation) { ops = append(ops, op) })
	_, err = r.EvaluateModule(code)
	require.Error(t, err)
	assert.Equal(t, []PromiseRejectionOperation{PromiseRejectionReject}, ops)
}

// TestTopLevelAwaitInterrupt checks that an interrupt stops a module
// resumed from a job, drops the queue and leaves the realm reusable.
func TestTopLevelAwaitInterrupt(t *testing.T) {
	m, err := syntax.ParseModule("tla.js", `export let n = 0; export function one() { return 1; } await 0; n++; for (;;) {}`, syntax.Options{})
	require.NoError(t, err)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err)
	r := NewRealm()
	time.AfterFunc(20*time.Millisecond, func() { r.Interrupt("stop") })
	env, err := r.EvaluateModule(code)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	f := &moduleFixture{t: t, r: r, env: env}
	assert.Equal(t, int64(1), f.export("n"))
	assert.Zero(t, queued(r))
	r.ClearInterrupt()
	assert.Equal(t, int64(1), f.call("one"))
}

// TestModuleGraphInterruptedAsync checks that a graph whose asynchronous
// evaluation an interrupt stopped stays failed in the realm: its promise
// stays pending, and a later EvaluateGraph of it, or of a graph that
// imports it, returns the interrupt.
func TestModuleGraphInterruptedAsync(t *testing.T) {
	h := newModuleHost(t, map[string]string{
		"spin":  `await 0; stop(); for (;;) {}`,
		"main":  `import "spin"; globalThis.ran = 1;`,
		"other": `import "main";`,
	})
	r := NewRealm()
	stop := r.NewNativeFunction(FromGoString("stop"), 0, func(r *Realm, _ Value, _ []Value) (Value, error) {
		r.Interrupt("stop")
		return Undefined(), nil
	})
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("stop"), ObjectValue(stop)))
	g, err := h.link("main")
	require.NoError(t, err)
	_, p, err := r.EvaluateGraph(g)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	require.NotNil(t, p)
	state, _, _ := p.PromiseResult()
	assert.Equal(t, PromisePending, state)
	r.ClearInterrupt()

	_, p, err = r.EvaluateGraph(g)
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)
	assert.Nil(t, p)
	other, err := h.link("other")
	require.NoError(t, err)
	_, p, err = r.EvaluateGraph(other)
	require.ErrorAs(t, err, &ie)
	assert.Nil(t, p)
	ran, err := r.Global.Get(r, r.KeyFromGoString("ran"), ObjectValue(r.Global))
	require.NoError(t, err)
	assert.True(t, ran.IsUndefined())
}

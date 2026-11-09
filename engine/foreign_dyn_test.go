package engine

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs/compiler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// globalOf returns the global variable name of r.
func globalOf(t *testing.T, r *Realm, name string) Value {
	t.Helper()
	v, err := r.Global.GetProp(r, r.KeyFromGoString(name))
	require.NoError(t, err, name)
	return v
}

// TestForeignDynamicClosure calls, from realm A, closures of dynamic code
// that realm B compiled and ran. Their inline caches stay bound in B's
// table, so A once ran them on its own table at B's offsets: with A's
// caches there filled, f returned o.q, 2, and g wrote 99 to o.q, with no
// error, and from a fresh realm the call panicked (errICOutside). A frame
// of them in A now fails with errForeignFrame's TypeError before it starts
// and writes nothing, whether the host calls them, A's code does
// (callValue) or A resumes a generator of them, and whether B bound their
// caches or not; IsForeign reports them to A and not to B, and B runs them
// as before.
func TestForeignDynamicClosure(t *testing.T) {
	SetCompiler(compiler.Hook{})
	t.Cleanup(func() { SetCompiler(nil) })
	const foreign = "TypeError: a function or generator of another realm runs only in its own realm"
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("SharedIntrinsics=%v", shared), func(t *testing.T) {
			b := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			evalValue(t, b, `var f = new Function("o", "return o.p"); var g = new Function("o", "o.p = 99");
f({p: 1, q: 2}); g({p: 1, q: 2});`)
			evalValue(t, b, `var e = eval("(function (o) { return o.p; })");
var unbound = new Function("o", "return o.p");
var it = new Function("return function* (o) { yield o.p; yield o.p + 1; }")()({p: 1, q: 2});
it.next(); e({p: 1, q: 2});`)
			f, g, e, unbound, it := globalOf(t, b, "f"), globalOf(t, b, "g"), globalOf(t, b, "e"), globalOf(t, b, "unbound"), globalOf(t, b, "it")
			require.NotEqual(t, icUnbound, f.AsObject().funcData().icBase, "B bound f's caches")
			require.Equal(t, icUnbound, unbound.AsObject().funcData().icBase)

			a := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			// A's own af and ag fill A's caches where B's f and g have
			// theirs, with o's shape and q's slot.
			evalValue(t, a, `var af = new Function("o", "return o.q"); var ag = new Function("o", "o.q = 99");
af({p: 1, q: 2}); ag({p: 1, q: 2});`)
			o := evalValue(t, a, `var o = {p: 1, q: 2}; o`)
			fresh := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			for _, v := range []Value{f, g, e, unbound, it} {
				assert.True(t, a.IsForeign(v))
				assert.True(t, fresh.IsForeign(v))
				assert.False(t, b.IsForeign(v))
			}

			for i, fn := range []Value{f, g, e, unbound} {
				name := []string{"f", "g", "e", "unbound"}[i]
				res, err := a.Call(fn, Undefined(), []Value{o})
				require.Error(t, err, "%s returned %v", name, res)
				assert.Equal(t, foreign, errorString(err), name)
				_, err = fresh.Call(fn, Undefined(), []Value{o})
				assert.Equal(t, foreign, errorString(err), name, "no errICOutside panic")
				_, err = a.Construct(fn, []Value{o}, nil)
				assert.Equal(t, foreign, errorString(err), name)
			}
			assert.Equal(t, "1,2", runScriptsIn(t, a, `o.p + "," + o.q`), "g wrote nothing")

			// From A's code: a plain call (callValue's fast path) and a
			// generator's resumption.
			for _, name := range []string{"f", "g", "it"} {
				require.NoError(t, a.Global.SetProp(a, a.KeyFromGoString("b_"+name), globalOf(t, b, name)))
			}
			assert.Equal(t, "TypeError,TypeError,TypeError,1,2", runScriptsIn(t, a, `var out = [];
for (var c of [() => b_f(o), () => b_g(o), () => b_it.next()]) {
  try { out.push("ran " + c()); } catch (x) { out.push(x instanceof TypeError ? "TypeError" : "other " + x); }
}
out.push(o.p, o.q); out.join()`))
			assert.Equal(t, "1,2", runScriptsIn(t, a, `o.p + "," + o.q`))

			// B runs them as before, its generator from where it stopped.
			assert.Equal(t, "1,99,1,1,2,true", runScriptsIn(t, b, `var o = {p: 1, q: 2}, out = [f(o)];
g(o); out.push(o.p, e({p: 1}), unbound({p: 1}), it.next().value, it.next().done); out.join()`))
		})
	}
}

// TestDynamicClosureSameRealm checks the closures of dynamic code in the
// realm that compiled them: eval and the Function constructor, the shared
// one included, make closures of the realm, which are not foreign, run
// from the realm's code and the host, and keep their inline caches bound
// between calls.
func TestDynamicClosureSameRealm(t *testing.T) {
	SetCompiler(compiler.Hook{})
	t.Cleanup(func() { SetCompiler(nil) })
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("SharedIntrinsics=%v", shared), func(t *testing.T) {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
			assert.Equal(t, "15", runScriptsIn(t, r, `var h = new Function("o", "return o.p + o.q");
var e = eval("(function (o) { return o.q; })");
var gen = new Function("return function* (o) { yield o.p; }")();
var n = 0;
for (var i = 0; i < 3; i++) n += h({p: 1, q: 2}) + e({p: 1, q: 2});
n`))
			o := evalValue(t, r, `({p: 1, q: 2})`)
			for _, name := range []string{"h", "e", "gen"} {
				fn := globalOf(t, r, name)
				assert.False(t, r.IsForeign(fn), name)
				assert.Same(t, r, fn.AsObject().funcData().Realm(), name)
			}
			h := globalOf(t, r, "h")
			fd := h.AsObject().funcData()
			base := fd.icBase
			require.NotEqual(t, icUnbound, base)
			for range 2 {
				res, err := r.Call(h, Undefined(), []Value{o})
				require.NoError(t, err)
				assert.Equal(t, "3", res.String())
				assert.Equal(t, base, fd.icBase, "the caches stay bound")
			}
			hit := false
			for _, e := range r.ic[base : base+fd.code.ICCount] {
				hit = hit || e.Shape == o.AsObject().shape
			}
			assert.True(t, hit, "h's caches hold o's shape")
			it, err := r.Call(globalOf(t, r, "gen"), Undefined(), []Value{o})
			require.NoError(t, err)
			assert.False(t, r.IsForeign(it))
			assert.Equal(t, "1", runScriptsIn(t, r, `gen({p: 1}).next().value`))
		})
	}
}

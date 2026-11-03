package engine

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestObjectTemplate compares the objects NewObjectFrom creates with
// those NewObjectWithProto and DefineOwnDataFast make, one property at a
// time: the same shape (or both in dictionary mode past maxShapeProps),
// keys in the same order, the same descriptors and prototype, before and
// after the same changes, in mutable and shared realms.
func TestObjectTemplate(t *testing.T) {
	attrSets := []uint8{attrDefault, attrWritable | attrEnumerable, attrHidden, attrEnumerable, 0}
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		r := NewRealmWith(opts)
		custom := r.NewObject()
		custom.DefineOwnDataFast(r, r.KeyFromGoString("inherited"), IntValue(1), attrDefault)
		rng := rand.New(rand.NewPCG(7, 8))
		for _, n := range []int{0, 1, 2, 7, 8, 9, 12, 13, 14, 16, 17, 40, 64, 65, 100} {
			for _, proto := range []*Object{nil, r.ObjectPrototype, custom} {
				keys := make([]PropertyKey, n)
				vals := make([]Value, n)
				for i := range keys {
					if i == 3 {
						keys[i] = SymbolKey(NewSymbol(FromGoString(fmt.Sprintf("s%d", n))))
					} else {
						keys[i] = r.KeyFromGoString(fmt.Sprintf("k%d_%d", n, i))
					}
					vals[i] = IntValue(i * 10)
				}
				attrs := attrSets[rng.IntN(len(attrSets))]
				tmpl := r.NewObjectTemplate(proto, keys, attrs)
				a := r.NewObjectFrom(tmpl, vals)
				b := r.NewObjectWithProto(proto)
				for i, k := range keys {
					b.DefineOwnDataFast(r, k, vals[i], attrs)
				}
				name := fmt.Sprintf("n=%d attrs=%d proto=%v shared=%v", n, attrs, proto != nil, opts.SharedIntrinsics)
				same := func(step string) {
					t.Helper()
					require.Equal(t, b.IsDictionaryMode(), a.IsDictionaryMode(), "%s %s", name, step)
					if !b.IsDictionaryMode() {
						require.Same(t, b.Shape(), a.Shape(), "%s %s", name, step)
					}
					require.Same(t, b.Proto(), a.Proto(), "%s %s", name, step)
					require.Equal(t, b.IsExtensible(), a.IsExtensible(), "%s %s", name, step)
					ka, kb := a.OwnPropertyKeys(), b.OwnPropertyKeys()
					require.Equal(t, kb, ka, "%s %s", name, step)
					for _, k := range kb {
						da, _ := a.GetOwnProperty(k)
						db, _ := b.GetOwnProperty(k)
						require.Equal(t, db, da, "%s %s %v", name, step, k)
					}
				}
				same("created")
				if n > 0 {
					okA, errA := a.Set(r, keys[n-1], IntValue(-1), ObjectValue(a))
					okB, errB := b.Set(r, keys[n-1], IntValue(-1), ObjectValue(b))
					require.Equal(t, [2]any{okB, errB}, [2]any{okA, errA}, name)
					same("set")
					a.Delete(r, keys[0])
					b.Delete(r, keys[0])
					same("delete first")
				}
				extra := r.KeyFromGoString("extra")
				require.NoError(t, a.SetProp(r, extra, IntValue(5)))
				require.NoError(t, b.SetProp(r, extra, IntValue(5)))
				same("add")
			}
		}
	}
}

// TestObjectTemplateScript reads objects of a template from JavaScript as
// it reads the same objects built one property at a time.
func TestObjectTemplateScript(t *testing.T) {
	r := NewRealm()
	proto := r.NewObject()
	keys := []PropertyKey{r.KeyFromGoString("dev"), r.KeyFromGoString("mode"), r.KeyFromGoString("size")}
	tmpl := r.NewObjectTemplate(proto, keys, attrDefault)
	a := r.NewObjectFrom(tmpl, []Value{IntValue(1), IntValue(2), IntValue(3)})
	b := r.NewObjectWithProto(proto)
	for i, k := range keys {
		b.DefineOwnDataFast(r, k, IntValue(i+1), attrDefault)
	}
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("a"), ObjectValue(a)))
	require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("b"), ObjectValue(b)))
	for _, src := range []string{
		"JSON.stringify(Object.getOwnPropertyDescriptors(X))",
		"Object.keys(X).join() + (Object.getPrototypeOf(X) === Object.getPrototypeOf(a))",
		"(() => { let s = 0; for (let i = 0; i < 100; i++) s += X.size; return s })()",
		"(X.extra = 1, delete X.dev, Object.entries(X).join(';'))",
		"(Object.freeze(X), X.mode = 9, [Object.isFrozen(X), X.mode].join())",
	} {
		want := runScriptsIn(t, r, strings.ReplaceAll(src, "X", "b"))
		require.Equal(t, want, runScriptsIn(t, r, strings.ReplaceAll(src, "X", "a")), src)
	}
}

// TestObjectTemplatePanics checks the keys a template refuses and the
// template of another realm.
func TestObjectTemplatePanics(t *testing.T) {
	r := NewRealm()
	k := r.KeyFromGoString("a")
	require.Panics(t, func() { r.NewObjectTemplate(nil, []PropertyKey{k, k}, attrDefault) })
	require.Panics(t, func() { r.NewObjectTemplate(nil, []PropertyKey{IndexKey(0)}, attrDefault) })
	tmpl := r.NewObjectTemplate(nil, []PropertyKey{k}, attrDefault)
	require.Panics(t, func() { NewRealm().NewObjectFrom(tmpl, []Value{IntValue(1)}) })
	require.Panics(t, func() { r.NewObjectFrom(tmpl, nil) })
}

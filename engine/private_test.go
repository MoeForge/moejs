package engine

import (
	"fmt"
	"testing"

	"github.com/MoeForge/moejs/bytecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrivateKeysInvisible checks that private elements stored next to
// ordinary properties never show up as keys, in shape and dictionary mode.
func TestPrivateKeysInvisible(t *testing.T) {
	f := evalModule(t, `
export function probe(o) {
  const forIn = [];
  for (const k in o) forIn.push(k);
  return JSON.stringify([
    Object.keys(o), Object.getOwnPropertyNames(o),
    forIn, JSON.parse(JSON.stringify(o)), Object.assign({}, o), { ...o }, Object.entries(o),
    Object.isFrozen(Object.freeze(o)),
  ]);
}`)
	r := f.r
	fn, ok := f.env.GetBindingValue("probe")
	require.True(t, ok)
	for _, n := range []int{1, maxShapeProps + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			o := r.NewObject()
			o.CreateDataPropertyOrThrow(r, r.KeyFromGoString("a"), IntValue(1))
			pn := newPrivateName(r.InternGoString("#p"))
			require.NoError(t, r.privateDefine(ObjectValue(o), pn, IntValue(2)))
			for i := 1; i < n; i++ {
				o.CreateDataPropertyOrThrow(r, r.KeyFromGoString(fmt.Sprint("k", i)), IntValue(i))
			}
			assert.Equal(t, n > maxShapeProps, o.IsDictionaryMode())
			res, err := r.Call(fn, Undefined(), []Value{ObjectValue(o)})
			require.NoError(t, err)
			s := res.String()
			assert.NotContains(t, s, "#p")
			assert.Contains(t, s, `[["a"`)
			assert.Contains(t, s, `true]`)
			// The field survives freezing and stays writable.
			v, err := r.privateGet(ObjectValue(o), pn)
			require.NoError(t, err)
			assert.Equal(t, 2.0, v.AsNumber())
			require.NoError(t, r.privateSet(ObjectValue(o), pn, IntValue(3)))
			v, _ = r.privateGet(ObjectValue(o), pn)
			assert.Equal(t, 3.0, v.AsNumber())
			assert.Equal(t, "#p", PrivateKey(pn).GoString())
		})
	}
}

func TestPrivateOps(t *testing.T) {
	r := NewRealm()
	o, other := ObjectValue(r.NewObject()), ObjectValue(r.NewObject())
	field := newPrivateName(r.InternGoString("#f"))
	_, err := r.privateGet(o, field)
	assert.ErrorContains(t, err, "Cannot read private member #f from an object whose class did not declare it")
	assert.ErrorContains(t, r.privateSet(o, field, Undefined()), "Cannot write private member #f to an object whose class did not declare it")
	require.NoError(t, r.privateDefine(o, field, IntValue(1)))
	assert.ErrorContains(t, r.privateDefine(o, field, IntValue(1)), "Cannot initialize #f twice on the same object")
	in, err := r.privateIn(o, field)
	require.NoError(t, err)
	assert.True(t, in)
	in, _ = r.privateIn(other, field)
	assert.False(t, in)
	_, err = r.privateIn(IntValue(1), field)
	assert.ErrorContains(t, err, "Cannot use 'in' operator to search for '#f' in 1")

	// A second name with the same description is a different key.
	twin := newPrivateName(r.InternGoString("#f"))
	_, err = r.privateGet(o, twin)
	require.Error(t, err)
	assert.NotEqual(t, PrivateKey(field), PrivateKey(twin))

	// Methods check the brand; static ones check the class.
	brand := newPrivateName(r.InternGoString("A"))
	m := newPrivateName(r.InternGoString("#m"))
	method := r.NewObject()
	definePrivateMethod(m, method, privateValue(brand), 0)
	_, err = r.privateGet(o, m)
	require.Error(t, err)
	require.NoError(t, r.privateAddBrand(o, brand))
	assert.ErrorContains(t, r.privateAddBrand(o, brand), "Cannot initialize private methods of class A twice on the same object")
	v, err := r.privateGet(o, m)
	require.NoError(t, err)
	assert.Same(t, method, v.AsObject())
	assert.ErrorContains(t, r.privateSet(o, m, Undefined()), "Private method '#m' is not writable")

	s := newPrivateName(r.InternGoString("#s"))
	definePrivateMethod(s, method, other, bytecode.PrivateStatic|bytecode.PrivateSetter)
	in, _ = r.privateIn(other, s)
	assert.True(t, in)
	in, _ = r.privateIn(o, s)
	assert.False(t, in)
	_, err = r.privateGet(other, s)
	assert.ErrorContains(t, err, "'#s' was defined without a getter")

}

// TestPrivateShared checks that a private element never enters a shape of
// the shared intrinsics: shared intrinsics refuse it and an object with a
// shared shape (the global object) moves to dictionary mode first.
func TestPrivateShared(t *testing.T) {
	r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	pn := newPrivateName(r.InternGoString("#x"))
	assert.ErrorContains(t, r.privateDefine(ObjectValue(r.ObjectPrototype), pn, Undefined()),
		"Cannot define private member #x on shared intrinsic")
	g := r.Global
	require.True(t, g.shape.IsShared())
	require.NoError(t, r.privateDefine(ObjectValue(g), pn, IntValue(7)))
	assert.True(t, g.IsDictionaryMode())
	v, err := r.privateGet(ObjectValue(g), pn)
	require.NoError(t, err)
	assert.Equal(t, 7.0, v.AsNumber())
	// A second realm still sees the pristine shared global shape.
	r2 := NewRealmWith(RealmOptions{SharedIntrinsics: true})
	assert.False(t, r2.Global.IsDictionaryMode())
}

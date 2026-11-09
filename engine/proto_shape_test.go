package engine

import (
	"sync"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dictObject returns an object past what a shape holds.
func dictObject(t *testing.T, r *Realm, o *Object) *Object {
	t.Helper()
	for i := range maxShapeProps + 2 {
		mustSet(t, r, o, "p"+NumberToGoString(float64(i)), IntValue(i))
	}
	require.True(t, o.IsDictionaryMode())
	return o
}

// TestDictionaryKeepsItsProto: an object in dictionary mode has no shape of
// its own, and its prototype is still its shape's.
func TestDictionaryKeepsItsProto(t *testing.T) {
	for _, shared := range []bool{false, true} {
		r := NewRealmWith(RealmOptions{SharedIntrinsics: shared})
		p, q := r.NewObject(), r.NewObject()
		a, b := dictObject(t, r, r.NewObjectWithProto(p)), dictObject(t, r, r.NewObjectWithProto(p))
		assert.Same(t, p, a.Proto())
		assert.Same(t, a.Shape(), b.Shape(), "objects of one prototype share a sentinel")
		assert.Same(t, r.ObjectPrototype, dictObject(t, r, r.NewObject()).Proto())
		assert.Same(t, r.ArrayPrototype, dictObject(t, r, r.NewArray()).Proto())
		assert.Nil(t, dictObject(t, r, r.NewObjectWithProto(nil)).Proto())

		require.True(t, a.SetPrototypeOf(r, q))
		assert.Same(t, q, a.Proto())
		assert.Same(t, p, b.Proto(), "another object of the sentinel keeps its prototype")
		assert.True(t, a.Shape().IsDictionary())
		require.True(t, a.SetPrototypeOf(r, nil))
		assert.Nil(t, a.Proto())
		v, err := a.GetProp(r, key(r, "p1"))
		require.NoError(t, err)
		assert.Equal(t, IntValue(1), v)
	}
}

// TestSharedDictSentinelGoroutines: realms on goroutines of their own make
// the sentinels of the shared prototypes.
func TestSharedDictSentinelGoroutines(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r := NewRealmWith(RealmOptions{SharedIntrinsics: true})
			assert.Same(t, r.ObjectPrototype, dictObject(t, r, r.NewObject()).Proto())
			assert.Same(t, r.DatePrototype, dictObject(t, r, r.NewObjectWithProto(r.DatePrototype)).Proto())
		})
	}
	wg.Wait()
}

// TestHostPlaceholderProtos: a placeholder's shape is a sentinel of its heap
// with its prototype, an array's and an object's alike, before and after it
// materializes.
func TestHostPlaceholderProtos(t *testing.T) {
	r := NewRealm()
	in := map[string]any{"list": []any{int64(1), int64(2)}, "obj": map[string]any{"k": "v"}}
	root := mustFromGo(t, r, in).AsObject()
	h := r.hostHeap()
	assert.Same(t, r.ObjectPrototype, root.Proto())
	assert.Same(t, h, hostHeapOf(root.shape))

	list, err := root.GetProp(r, key(r, "list"))
	require.NoError(t, err)
	a := list.AsObject()
	assert.Same(t, r.ArrayPrototype, a.Proto())
	assert.Same(t, h, hostHeapOf(a.shape))
	assert.Equal(t, hostSentinelKey, a.shape.key)
	a.materializeHost()
	assert.Same(t, r.ArrayPrototype, a.Proto())
	assert.Equal(t, 2, len(a.Elements()))

	obj, err := root.GetProp(r, key(r, "obj"))
	require.NoError(t, err)
	o := obj.AsObject()
	require.True(t, o.SetPrototypeOf(r, nil))
	assert.Nil(t, o.Proto())
	v, err := o.GetProp(r, key(r, "k"))
	require.NoError(t, err)
	assert.Equal(t, "v", v.AsString().GoString())
}

// TestHostHeapSize: the heap holds the object sentinel and stays in the
// 640-byte class with the 8-byte header the allocator gives an object
// past 512 bytes; the array sentinel, allocated with the first slice
// placeholder, fills the 128-byte class.
func TestHostHeapSize(t *testing.T) {
	assert.LessOrEqual(t, unsafe.Sizeof(hostHeap{}), uintptr(640-8))
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(128), unsafe.Sizeof(hostArraySentinel{}))
	}
	r := NewRealm()
	mustFromGo(t, r, map[string]any{"k": "v"})
	h := r.hostHeap()
	assert.Nil(t, h.arraySentinel, "no slice converted")
	mustFromGo(t, r, []any{int64(1)})
	require.NotNil(t, h.arraySentinel)
	assert.Same(t, h, hostHeapOf(&h.arraySentinel.shape))
}

// TestInlineCacheThroughDictionaryProto: a hit on a property two prototypes
// up walks through a prototype in dictionary mode, and a change of that
// prototype's own prototype invalidates it.
func TestInlineCacheThroughDictionaryProto(t *testing.T) {
	r := NewRealm()
	v, err := runScriptValue(t, r, `
const base = {x: 1};
const mid = Object.create(base);
for (let i = 0; i < 100; i++) mid["p" + i] = i;
const o = Object.create(mid);
function get(o) { return o.x; }
let n = 0;
for (let i = 0; i < 100; i++) n += get(o);
Object.setPrototypeOf(mid, {x: 1000});
for (let i = 0; i < 100; i++) n += get(o);
n`)
	require.NoError(t, err)
	assert.Equal(t, 100100.0, v.AsNumber())
}

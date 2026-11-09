package engine

import (
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type payloadTestState struct{ n int }

type payloadTestValue struct{ a, b int }

func TestSetInternalKeepsHostValues(t *testing.T) {
	r := NewRealm()
	values := []any{
		&payloadTestState{n: 1},
		payloadTestValue{a: 2, b: 3},
		7,
		"text",
		[]int{4, 5},
		map[string]int{"k": 6},
		func() int { return 8 },
	}
	objs := make([]*Object, len(values))
	for i, v := range values {
		objs[i] = r.NewObject()
		objs[i].SetInternal(v)
	}
	runtime.GC()
	for i, v := range values {
		got := objs[i].Internal()
		if f, ok := v.(func() int); ok {
			assert.Equal(t, f(), got.(func() int)())
			continue
		}
		assert.Equal(t, v, got)
	}
	// The same type gets the same kind.
	o := r.NewObject()
	o.SetInternal(&payloadTestState{n: 9})
	assert.Equal(t, objs[0].pkind, o.pkind)
	assert.Equal(t, 9, o.Internal().(*payloadTestState).n)
	o.SetInternal(nil)
	assert.Nil(t, o.Internal())
}

func TestPayloadKeepsItsReferentAlive(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	freed := make(chan struct{})
	func() {
		s := &payloadTestState{n: 1}
		runtime.AddCleanup(s, func(c chan struct{}) { close(c) }, freed)
		o.SetInternal(s)
	}()
	for range 3 {
		runtime.GC()
	}
	select {
	case <-freed:
		t.Fatal("the payload was collected while its object held it")
	default:
	}
	assert.Equal(t, 1, o.Internal().(*payloadTestState).n)
	runtime.KeepAlive(o)
}

func TestBoxedPayload(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	v := payloadTestValue{a: 1, b: 2}
	o.setExtPayload(kindBoxed, boxPayload(v))
	runtime.GC()
	assert.Equal(t, v, o.Internal())
	_, isBox := o.payload().(*any)
	assert.True(t, isBox, "payload is the box")
	// A kind past the table's last falls back to a box.
	saved := payloadCount.Load()
	defer payloadCount.Store(saved)
	payloadCount.Store(int32(int(kindBoxed) - int(staticKinds)))
	assert.Equal(t, kindBoxed, payloadKindOf(struct{ x, y, z int }{}))
	p := r.NewObject()
	p.SetInternal(struct{ x, y, z int }{1, 2, 3})
	require.Equal(t, kindBoxed, p.pkind)
	assert.Equal(t, struct{ x, y, z int }{1, 2, 3}, p.Internal())
}

// TestFunctionPayloadIsItsData: funcData reads a function's payload
// unchecked, so SetInternal may not replace it.
func TestFunctionPayloadIsItsData(t *testing.T) {
	r := NewRealm()
	v, err := runScriptValue(t, r, `(function f() { return 7 })`)
	require.NoError(t, err)
	f := v.AsObject()
	fd := f.funcData()
	const msg = "engine: the payload of a function must be its *FunctionData"
	assert.PanicsWithValue(t, msg, func() { f.SetInternal(&payloadTestState{}) })
	assert.PanicsWithValue(t, msg, func() { f.SetInternal(nil) })
	g, err := runScriptValue(t, r, `(function g() {})`)
	require.NoError(t, err)
	assert.PanicsWithValue(t, msg, func() { f.SetInternal(g.AsObject().funcData()) }, "another function's")
	assert.Same(t, fd, f.funcData())
	f.SetInternal(fd)
	got, err := r.Call(v, Undefined(), nil)
	require.NoError(t, err)
	assert.Equal(t, IntValue(7), got)
}

func TestObjectHeaderSize(t *testing.T) {
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(48), unsafe.Sizeof(Object{}))
		assert.Equal(t, uintptr(64), unsafe.Sizeof(arrayObject{}))
		assert.Equal(t, uintptr(16), unsafe.Sizeof(elemStore{}))
		assert.Equal(t, uintptr(80), unsafe.Sizeof(objectExt{}), "the 80-byte class")
	}
}

// TestExtPayload: a payload that is not co-allocated sits in ext while the
// object has no objectExt and moves into the objectExt that dictionary
// mode, an indexed property or a root shape gives it.
func TestExtPayload(t *testing.T) {
	r := NewRealm()
	st := &payloadTestState{n: 1}
	for name, give := range map[string]func(o *Object){
		"dictionary": func(o *Object) { o.toDictionary(r, 0) },
		"index": func(o *Object) {
			_, err := o.Set(r, IndexKey(3), IntValue(1), ObjectValue(o))
			require.NoError(t, err)
		},
		"root": func(o *Object) { r.NewObjectWithProto(o) },
	} {
		o := r.NewObject()
		o.SetInternal(st)
		assert.Zero(t, o.bits&bitHasExt, name)
		assert.Equal(t, unsafe.Pointer(st), o.ext, name)
		give(o)
		assert.NotZero(t, o.bits&bitHasExt, name)
		require.NotNil(t, o.extData(), name)
		assert.Same(t, st, o.Internal(), name)
		o.SetInternal(nil)
		assert.Nil(t, o.Internal(), name)
		assert.NotNil(t, o.extData(), name)
	}
	o := r.NewObject()
	o.SetInternal(st)
	o.SetInternal(nil)
	assert.Zero(t, o.bits&bitHasExt)
	assert.Nil(t, o.ext)
}

// TestCoAllocatedKindSetApart: a value of a co-allocated kind that a host
// sets on another object is boxed, so the engine never reads it as that
// object's; setData panics on one.
func TestCoAllocatedKindSetApart(t *testing.T) {
	r := NewRealm()
	o := r.NewObject()
	dd := &DateData{}
	o.SetInternal(dd)
	assert.Same(t, dd, o.Internal())
	assert.Nil(t, dataOf(o, kindDate))
	assert.Panics(t, func() { setData(r.NewObject(), kindDate, dd) })
	d, err := runScriptValue(t, r, `new Date(0)`)
	require.NoError(t, err)
	own := dataOf(d.AsObject(), kindDate)
	require.NotNil(t, own)
	d.AsObject().SetInternal(own)
	assert.Same(t, own, dataOf(d.AsObject(), kindDate))
}

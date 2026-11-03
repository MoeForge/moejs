package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewUint8Array checks that NewUint8Array gives the object new
// Uint8Array(new ArrayBuffer) gives, over the host's bytes, with
// Uint8Array.prototype or a subclass's prototype, in mutable and shared
// realms.
func TestNewUint8Array(t *testing.T) {
	for _, opts := range []RealmOptions{{}, {SharedIntrinsics: true}} {
		r := NewRealmWith(opts)
		runScriptsIn(t, r, "class Sub extends Uint8Array { sum() { let s = 0; for (const x of this) s += x; return s } }; globalThis.Sub = Sub")
		data := []byte{1, 2, 3, 4}
		a, err := r.NewUint8Array(data, nil)
		require.NoError(t, err)
		subCtor, err := r.Global.GetProp(r, r.KeyFromGoString("Sub"))
		require.NoError(t, err)
		subProto, err := subCtor.AsObject().GetProp(r, r.KeyFromGoString("prototype"))
		require.NoError(t, err)
		b, err := r.NewUint8Array([]byte{5, 6, 7}, subProto.AsObject())
		require.NoError(t, err)
		e, err := r.NewUint8Array(nil, nil)
		require.NoError(t, err)
		require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("a"), ObjectValue(a)))
		require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("b"), ObjectValue(b)))
		require.NoError(t, r.Global.SetProp(r, r.KeyFromGoString("e"), ObjectValue(e)))
		for _, c := range []struct{ src, want string }{
			{"Object.getPrototypeOf(a) === Uint8Array.prototype", "true"},
			{"[a.length, a.byteOffset, a.byteLength, a.buffer.byteLength, a.buffer instanceof ArrayBuffer].join()", "4,0,4,4,true"},
			{"[...a].join()", "1,2,3,4"},
			{"a[0] = 9, a.buffer.resizable, ArrayBuffer.isView(a), Object.prototype.toString.call(a)", "[object Uint8Array]"},
			{"[b instanceof Sub, b instanceof Uint8Array, b.sum(), b.constructor === Sub, b.length].join()", "true,true,18,true,3"},
			{"[b.subarray(1) instanceof Sub, b.slice(0, 1) instanceof Sub].join()", "true,true"},
			{"[e.length, e.buffer.byteLength, Object.keys(a).join(' '), JSON.stringify(a)].join()", `0,0,0 1 2 3,{"0":9,"1":2,"2":3,"3":4}`},
			{"a.buffer.transfer(8).byteLength + ':' + a.length", "8:0"},
		} {
			assert.Equal(t, c.want, runScriptsIn(t, r, c.src), c.src)
		}
		assert.Equal(t, byte(9), data[0], "the array writes the host's bytes")
	}
}

// TestNewUint8ArrayCharges checks that the two objects are charged to the
// memory limit, as Construct charges them.
func TestNewUint8ArrayCharges(t *testing.T) {
	r := NewRealm()
	r.SetMemoryLimit(64 << 20)
	m := r.mem()
	// The typed array intrinsics are made, and charged, on first use.
	_, err := r.NewUint8Array(nil, nil)
	require.NoError(t, err)
	before := m.total
	_, err = r.NewUint8Array(make([]byte, 16), nil)
	require.NoError(t, err)
	viaNew := m.total - before
	before = m.total
	ab, err := r.NewArrayBuffer(make([]byte, 16))
	require.NoError(t, err)
	_, err = r.Construct(ObjectValue(r.binaryIntr().typedArrayCtors[elemUint8]), []Value{ObjectValue(ab)}, nil)
	require.NoError(t, err)
	viaConstruct := m.total - before
	assert.Positive(t, viaNew)
	assert.Equal(t, viaConstruct, viaNew)
}

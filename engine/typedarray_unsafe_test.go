package engine

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBufferDataAligned checks that the data blocks the engine allocates
// start at a multiple of 8 whatever their length, so that the slices
// TypedArrayElements builds over them are aligned for their element type.
// It pins the runtime behaviour newBytes relies on: the tiny allocator
// packs noscan blocks of under 16 bytes at any offset, but 8-aligns those
// whose size is a multiple of 8.
func TestBufferDataAligned(t *testing.T) {
	f := evalModule(t, `export const out = [];
		for (let n = 0; n < 40; n++) {
			out.push(new ArrayBuffer(n), new Uint8Array(n).buffer,
				new ArrayBuffer(n + 1).transfer(n), new ArrayBuffer(n, { maxByteLength: 64 }));
		}`)
	r := f.r
	arr, ok := f.env.GetBindingValue("out")
	require.True(t, ok)
	n, err := r.LengthOfArrayLike(arr.AsObject())
	require.NoError(t, err)
	for i := range uint32(n) {
		v, err := arr.AsObject().GetIndex(r, i)
		require.NoError(t, err)
		b := extDataOf(v.AsObject(), kindArrayBuffer).data
		if cap(b) == 0 {
			continue
		}
		p := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
		assert.Zero(t, p%8, "buffer %d (%d bytes) at %#x", i, len(b), p)
	}
}

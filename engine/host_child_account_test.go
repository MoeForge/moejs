package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHostChildChargedAsDecoded checks the FromGo container path: the
// container materializes, its string is charged as decoded then, and its
// bytes stay undecoded until its content is read.
func TestHostChildChargedAsDecoded(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 20})
	before := r.Stats().RequestAllocatedBytes
	v, err := r.FromGo(map[string]any{"text": strings.Repeat("中", 8192)})
	require.NoError(t, err)
	require.True(t, v.AsObject().materializeHost(), "limited FromGo uses the host heap")
	x, err := v.AsObject().Get(r, key(r, "text"), v)
	require.NoError(t, err)
	s := x.AsString()
	require.True(t, s.isLazy())
	require.Nil(t, cacheOf(s))
	require.GreaterOrEqual(t, r.Stats().RequestAllocatedBytes-before, int64(stringHeaderSize+2*8192))
	charged := r.Stats().RequestAllocatedBytes
	require.Equal(t, 8192, s.Len())
	_ = s.UTF16()
	require.Equal(t, charged, r.Stats().RequestAllocatedBytes, "decoding charges nothing more")
}

// Reading v.text materializes the container, which charges its string as
// decoded: that passes the limit and stops the call.
func TestHostChildChargeStopsCall(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 64 << 10})
	env := evalModuleOn(t, r, `export function read(v) { return v.text.length; }`)
	r.ResetAllocation()
	v, err := r.FromGo(map[string]any{"text": strings.Repeat("中", 65536)})
	require.NoError(t, err)
	remove := r.AddLiveRoots(func(add func(Value)) { add(v) })
	defer remove()
	fn, ok := env.GetBindingValue("read")
	require.True(t, ok)
	_, err = r.Call(fn, Undefined(), []Value{v})
	require.ErrorIs(t, err, ErrMemoryLimit)
}

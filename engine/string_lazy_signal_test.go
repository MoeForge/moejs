package engine

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLazyHashAndEqualityPublishConcurrently(t *testing.T) {
	text := strings.Repeat("中😀a", 1024)
	s := FromHostString(text)
	eager := FromGoString(text)
	want := eager.Hash()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if s.Hash() != want || !s.Equals(eager) {
					t.Error("concurrent hash or equality changed the content")
				}
			}
		}()
	}
	wg.Wait()
}

// A host's text measures as its block and what it decodes to, before and
// after decoding; the Go text itself is the host's, as on main.
func TestLazyLiveStorageCountsDecodedUnits(t *testing.T) {
	text := strings.Repeat("中", 1024)
	n, wide, _ := hostUnits(text)
	for _, rooted := range []bool{false, true} {
		s := newLazyStringSized(text, lazyText, rooted, n, wide)
		var seen liveSet
		w := liveWalker{seen: &seen}
		w.str(s)
		want := int64(s.lazyBlockSize() + stringHeaderSize + 2*s.Len())
		require.Equal(t, want, w.size, "rooted=%v, before decoding", rooted)
		w.seen.release()
		_ = s.UTF16()
		w = liveWalker{seen: &seen}
		w.str(s)
		require.Equal(t, want, w.size, "rooted=%v, after decoding", rooted)
		w.str(s)
		require.Equal(t, want, w.size, "the same string counts once")
		w.seen.release()
	}
}

// Decoding a string a limited realm charged charges nothing more, on the
// realm's goroutine or another.
func TestLazyDecodeChargesNoAccount(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 30})
	v, err := r.JSONParseGoString(`"` + strings.Repeat("中", 65536) + `"`)
	require.NoError(t, err)
	s := v.AsString()
	require.NotNil(t, s.rootedData(), "made under the limit")
	before := r.Stats().RequestAllocatedBytes
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.UTF16()
	}()
	<-done
	require.NotNil(t, cacheOf(s))
	require.Equal(t, before, r.Stats().RequestAllocatedBytes)
	require.Zero(t, r.mem().flat.Load())
}

// A substring of a host container's field keeps no share root, as one of a
// string FromGo carved keeps none (strHost): the measurement counts its
// own units alone. One of a Go string converted on its own keeps the
// string, whose decoded units it keeps alive, as main's substrings keep
// the converted string.
func TestLazyHostSubstringShareRoot(t *testing.T) {
	r := NewRealm()
	for _, text := range []string{strings.Repeat("中", 4096), strings.Repeat("a", jsonPlainMin+100)} {
		box, err := r.FromGo(map[string]any{"t": text})
		require.NoError(t, err)
		v, err := box.AsObject().Get(r, key(r, "t"), box)
		require.NoError(t, err)
		field := v.AsString()
		require.True(t, field.isLazy())
		require.NotZero(t, field.flags&strHost)
		sub := field.Substring(10, 200)
		require.Nil(t, sub.right, "field %.8q", text)
		require.Equal(t, field.UTF16()[10:200], sub.UTF16())

		top := FromHostString(text)
		require.True(t, top.isLazy())
		require.Zero(t, top.flags&strHost)
		sub = top.Substring(10, 200)
		require.Same(t, top, sub.right, "top-level %.8q", text)
		require.Equal(t, top.UTF16()[10:200], sub.UTF16())
	}
}

// TestLazyJSONTextParseSharesDecoded: JSON text held as a JSON string with
// escapes (double-encoded JSON) decodes to an ASCII String of its own, out
// of which a parse under a memory limit reads its values: they share that
// String, which the live measurement counts once, with the lazy string, and
// not again in a String of the parse's own over the same bytes.
func TestLazyJSONTextParseSharesDecoded(t *testing.T) {
	r := NewRealmWith(RealmOptions{MemoryLimit: 1 << 40})
	outer, err := json.Marshal(`{"k":"` + strings.Repeat("x", 1<<16) + `"}`)
	require.NoError(t, err)
	v, err := r.JSONParseGoString(string(outer))
	require.NoError(t, err)
	s := v.AsString()
	require.True(t, s.isLazy())
	parsed, err := r.JSONParse(s)
	require.NoError(t, err)
	k, err := parsed.AsObject().Get(r, r.KeyFromGoString("k"), parsed)
	require.NoError(t, err)
	decoded := cacheOf(s)
	require.NotNil(t, decoded)
	require.Equal(t, strASCII, decoded.kind)
	assert.Same(t, decoded, k.AsString().right, "the decoded String is the parse's share root")
	var seen liveSet
	w := liveWalker{seen: &seen}
	w.str(s)
	counted := w.size
	w.str(k.AsString())
	assert.Equal(t, int64(stringHeaderSize), w.size-counted, "the value adds its header: the decoded String counted with s")
	w.seen.release()
}

// TestJSONParseRopeConcurrent: realms on two goroutines parse one rope
// while a third flattens it to export it. A parse reads the rope's units
// only through the flattening it publishes (run with -race).
func TestJSONParseRopeConcurrent(t *testing.T) {
	body := strings.Repeat("a", 1<<16)
	for range 20 {
		rope, err := NewRealm().Concat(asciiString(`"`), asciiString(body+`"`))
		require.NoError(t, err)
		require.Equal(t, strRope, rope.kind)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			if g := rope.GoString(); g != `"`+body+`"` {
				t.Errorf("exported %d bytes", len(g))
			}
		})
		for range 2 {
			r := NewRealm()
			wg.Go(func() {
				<-start
				v, err := r.JSONParse(rope)
				if err != nil || v.AsString().Len() != len(body) {
					t.Error("parse", err)
				}
			})
		}
		close(start)
		wg.Wait()
	}
}

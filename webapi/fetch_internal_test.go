package webapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadBodyLimits checks readBody around its limit, with limits up to
// math.MaxInt64 (a limit set only for the counters), with a stop short of
// the body's end, and with a body whose Read panics.
func TestReadBodyLimits(t *testing.T) {
	for _, c := range []struct {
		n, size, stop int
		limit         int64
		err           error
	}{
		{999, 16, math.MaxInt, 1000, io.EOF}, {1000, 16, math.MaxInt, 1000, io.EOF}, {1001, 16, math.MaxInt, 1000, errTooLarge},
		{64 << 10, 16 << 10, math.MaxInt, 0, io.EOF},
		{64 << 10, 16 << 10, math.MaxInt, math.MaxInt64, io.EOF},
		{64 << 10, 16 << 10, math.MaxInt, math.MaxInt64 - 1, io.EOF},
		{64 << 10, 16 << 10, math.MaxInt, 1 << 62, io.EOF},
		{64 << 10, 16, 20000, 0, nil}, {64 << 10, 16 << 10, 20000, 1 << 20, nil},
		{20000, 16, 20000, 1 << 20, nil}, {1001, 16, 1001, 1000, errTooLarge},
	} {
		var b []byte
		var err error
		require.NotPanics(t, func() {
			b, err = readBody(bytes.NewReader(make([]byte, c.n)), make([]byte, 0, c.size), c.stop, c.limit)
		}, "limit %d", c.limit)
		assert.Equal(t, c.err, err, "n %d stop %d limit %d", c.n, c.stop, c.limit)
		switch c.err {
		case io.EOF:
			assert.Len(t, b, c.n, "limit %d", c.limit)
		case nil:
			assert.Len(t, b, c.stop, "a read stops at stop")
		default:
			assert.Len(t, b, int(c.limit)+1, "a read past the limit stops after one byte more")
		}
	}
	var ie *moejs.InternalError
	_, err := readBody(panicReader{}, make([]byte, 0, 16), math.MaxInt, 0)
	assert.ErrorAs(t, err, &ie)
	_, err = readSome(panicReader{}, make([]byte, 16))
	assert.ErrorAs(t, err, &ie)
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic(errors.New("read panicked")) }

// TestSetContextCanceledAtFetchStart checks the fetches of runs whose
// context is canceled while they start: each one's conn leaves the
// installation's list, and -race sees no race on it.
func TestSetContextCanceledAtFetchStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	rt := moejs.NewRuntime(moejs.Options{})
	loop, err := eventloop.New(rt, eventloop.Options{})
	require.NoError(t, err)
	api, err := Install(loop, Options{Client: &http.Client{Transport: tr}})
	require.NoError(t, err)
	defer api.Close()
	require.NoError(t, rt.SetGlobal("BASE", srv.URL))
	require.NoError(t, rt.SetGlobal("newRun", moejs.NativeFunc(func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		ctx, cancel := context.WithCancel(context.Background())
		api.SetContext(ctx)
		go cancel()
		return moejs.Undefined(), nil
	})))
	s, err := moejs.CompileScript("runs.js", `(async () => {
  const ps = [];
  for (let i = 0; i < 5000; i++) { newRun(); ps.push(fetch(BASE).then(r => r.text(), () => 0)); }
  await Promise.all(ps);
})()`)
	require.NoError(t, err)
	require.NoError(t, loop.Run(func(rt *moejs.Runtime) error { _, err := rt.RunScript(s); return err }))
	deadline := time.Now().Add(5 * time.Second)
	for {
		api.e.mu.Lock()
		n := len(api.e.conns)
		api.e.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d conns stay listed after their fetches settled", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

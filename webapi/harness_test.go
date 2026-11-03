package webapi_test

import (
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/webapi"
	"github.com/stretchr/testify/require"
)

// harness is a runtime with a loop and the Web APIs, and a global
// done(value) that records the result of a test script.
type harness struct {
	t    *testing.T
	rt   *moejs.Runtime
	loop *eventloop.Loop
	api  *webapi.API

	mu     sync.Mutex
	result string
	done   bool
	errs   []string
}

func newHarness(t *testing.T, opts webapi.Options) *harness {
	t.Helper()
	return newHarnessRT(t, moejs.NewRuntime(moejs.Options{}), opts)
}

func newHarnessRT(t *testing.T, rt *moejs.Runtime, opts webapi.Options) *harness {
	t.Helper()
	return newHarnessLoop(t, rt, eventloop.Options{}, opts)
}

// newHarnessLoop is newHarnessRT with the loop's options lopts, whose
// OnError it sets.
func newHarnessLoop(t *testing.T, rt *moejs.Runtime, lopts eventloop.Options, opts webapi.Options) *harness {
	t.Helper()
	h := &harness{t: t, rt: rt}
	var err error
	lopts.OnError = func(err error) {
		h.mu.Lock()
		h.errs = append(h.errs, err.Error())
		h.mu.Unlock()
	}
	h.loop, err = eventloop.New(h.rt, lopts)
	require.NoError(t, err)
	h.api, err = webapi.Install(h.loop, opts)
	require.NoError(t, err)
	require.NoError(t, h.rt.SetGlobal("done", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		h.mu.Lock()
		h.result, h.done = moejs.Arg(args, 0).String(), true
		h.mu.Unlock()
		return moejs.Undefined(), nil
	})))
	t.Cleanup(h.api.Close)
	return h
}

// run runs body as the body of an async function and returns the string
// its result converts to, JSON for an object; a rejection fails the test.
func (h *harness) run(body string) string {
	h.t.Helper()
	res, err := h.try(body)
	require.NoError(h.t, err)
	return res
}

type jsError string

func (e jsError) Error() string { return string(e) }

// try is run that returns a rejection as an error.
func (h *harness) try(body string) (string, error) {
	h.t.Helper()
	src := `(async () => {` + body + `
})().then(v => done(typeof v === "object" && v !== null ? JSON.stringify(v) : String(v)),
          e => done("REJECTED " + (e && e.name ? e.name + ": " + e.message : String(e))));`
	s, err := moejs.CompileScript("test.js", src)
	require.NoError(h.t, err)
	h.mu.Lock()
	h.done, h.result = false, ""
	h.mu.Unlock()
	require.NoError(h.t, h.loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(s)
		return err
	}))
	h.mu.Lock()
	defer h.mu.Unlock()
	require.True(h.t, h.done, "the script did not finish")
	require.Empty(h.t, h.errs, "errors reported to the loop")
	if strings.HasPrefix(h.result, "REJECTED ") {
		return "", jsError(strings.TrimPrefix(h.result, "REJECTED "))
	}
	return h.result, nil
}

// runErr runs body as try does and returns loop.Run's error instead of
// failing on it, with the script's result so far.
func (h *harness) runErr(body string) (string, error) {
	h.t.Helper()
	src := `(async () => {` + body + `
})().then(v => done(typeof v === "object" && v !== null ? JSON.stringify(v) : String(v)),
          e => done("REJECTED " + (e && e.name ? e.name + ": " + e.message : String(e))));`
	s, err := moejs.CompileScript("test.js", src)
	require.NoError(h.t, err)
	h.mu.Lock()
	h.done, h.result = false, ""
	h.mu.Unlock()
	err = h.loop.Run(func(rt *moejs.Runtime) error {
		_, err := rt.RunScript(s)
		return err
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result, err
}

// noLeaks fails the test when goroutines started during it are still
// running after it, once the transport's idle connections are closed.
func noLeaks(t *testing.T, transports ...*http.Transport) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		for _, tr := range transports {
			tr.CloseIdleConnections()
		}
		deadline := time.Now().Add(5 * time.Second)
		n := runtime.NumGoroutine()
		for n > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			n = runtime.NumGoroutine()
		}
		if n > before {
			buf := make([]byte, 1<<20)
			t.Errorf("goroutines: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
		}
	})
}

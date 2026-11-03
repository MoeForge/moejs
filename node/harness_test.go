package node_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// harness is a runtime with a loop and the package installed over a
// temporary directory, and a global done(value) that records the result
// of a test script.
type harness struct {
	t    *testing.T
	dir  string // the root
	rt   *moejs.Runtime
	loop *eventloop.Loop
	api  *node.API

	mu     sync.Mutex
	result string
	done   bool
	errs   []string
}

// newHarness installs the package with opts over a new temporary
// directory, which opts.FS.Root names when it is empty.
func newHarness(t *testing.T, opts node.Options) *harness {
	t.Helper()
	return newHarnessRT(t, moejs.Options{}, opts)
}

func newHarnessRT(t *testing.T, ropts moejs.Options, opts node.Options) *harness {
	t.Helper()
	h := buildHarness(t, ropts, opts)
	t.Cleanup(func() { h.api.Close() })
	return h
}

// buildHarness is newHarnessRT for a test that closes the installation
// itself: the test keeps nothing of it.
func buildHarness(t *testing.T, ropts moejs.Options, opts node.Options) *harness {
	t.Helper()
	h := &harness{t: t}
	if opts.FS != nil && opts.FS.Root == "" {
		fo := *opts.FS
		fo.Root = t.TempDir()
		opts.FS = &fo
	}
	if opts.FS != nil {
		h.dir = opts.FS.Root
	}
	ropts.Importer = node.Importer(ropts.Importer)
	h.rt = moejs.NewRuntime(ropts)
	var err error
	h.loop, err = eventloop.New(h.rt, eventloop.Options{OnError: func(err error) {
		h.mu.Lock()
		h.errs = append(h.errs, err.Error())
		h.mu.Unlock()
	}})
	require.NoError(t, err)
	h.api, err = node.InstallLoop(h.loop, opts)
	require.NoError(t, err)
	require.NoError(t, h.rt.SetGlobal("done", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		h.mu.Lock()
		h.result, h.done = moejs.Arg(args, 0).String(), true
		h.mu.Unlock()
		return moejs.Undefined(), nil
	})))
	return h
}

// write creates the files of tree under the root: a name ending in "/"
// is a directory, "->" makes a link.
func (h *harness) write(tree map[string]string) {
	h.t.Helper()
	writeTree(h.t, h.dir, tree)
}

func writeTree(t *testing.T, dir string, tree map[string]string) {
	t.Helper()
	for name, content := range tree {
		p := filepath.Join(dir, filepath.FromSlash(name))
		switch {
		case strings.HasSuffix(name, "/"):
			require.NoError(t, os.MkdirAll(p, 0o755))
		case strings.HasPrefix(content, "->"):
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.Symlink(strings.TrimPrefix(content, "->"), p))
		default:
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		}
	}
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

const scriptHead = `(async () => { const fs = await import("node:fs"); const fsp = fs.promises; `

// try is run that returns a rejection as an error.
func (h *harness) try(body string) (string, error) {
	h.t.Helper()
	res, err := h.runErr(body)
	require.NoError(h.t, err)
	h.mu.Lock()
	defer h.mu.Unlock()
	require.True(h.t, h.done, "the script did not finish")
	require.Empty(h.t, h.errs, "errors reported to the loop")
	if strings.HasPrefix(res, "REJECTED ") {
		return "", jsError(strings.TrimPrefix(res, "REJECTED "))
	}
	return res, nil
}

// runErr runs body and returns loop.Run's error, with the script's
// result so far.
func (h *harness) runErr(body string) (string, error) {
	h.t.Helper()
	head := scriptHead
	if h.dir == "" {
		head = `(async () => {`
	}
	src := head + body + `
})().then(v => done(typeof v === "object" && v !== null ? JSON.stringify(v) : String(v)),
          e => done("REJECTED " + (e && e.code ? e.code + " " : "") + (e && e.name ? e.name + ": " + e.message : String(e))));`
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

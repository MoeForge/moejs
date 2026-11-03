package node_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/stretchr/testify/require"
)

// miniLoop is the smallest loop a host could drive itself, as
// dsh-ptc-runtime-moejs drives its own: a queue of posted tasks, a count
// of holds, and a run that ends when neither is left or a task fails.
type miniLoop struct {
	rt    *moejs.Runtime
	mu    sync.Mutex
	tasks []func(*moejs.Runtime) error
	holds int
	over  bool
	wake  chan struct{}
}

func newMiniLoop(rt *moejs.Runtime) *miniLoop {
	return &miniLoop{rt: rt, wake: make(chan struct{}, 1)}
}

func (l *miniLoop) Runtime() *moejs.Runtime { return l.rt }

func (l *miniLoop) Post(task func(*moejs.Runtime) error) bool {
	l.mu.Lock()
	if l.over {
		l.mu.Unlock()
		return false
	}
	l.tasks = append(l.tasks, task)
	l.mu.Unlock()
	l.signal()
	return true
}

func (l *miniLoop) Hold() func() {
	l.mu.Lock()
	l.holds++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.holds--
			l.mu.Unlock()
			l.signal()
		})
	}
}

func (l *miniLoop) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// run calls start, then runs the posted tasks until no task or hold is
// left, or one fails.
func (l *miniLoop) run(start func(*moejs.Runtime) error) error {
	if err := start(l.rt); err != nil {
		return err
	}
	for {
		l.mu.Lock()
		tasks, holds := l.tasks, l.holds
		l.tasks = nil
		l.mu.Unlock()
		if len(tasks) == 0 {
			if holds == 0 {
				return nil
			}
			<-l.wake
			continue
		}
		for _, t := range tasks {
			if err := t(l.rt); err != nil {
				return err
			}
		}
	}
}

// TestCustomLoop runs the package on a loop of the host's own, not
// package eventloop's.
func TestCustomLoop(t *testing.T) {
	dir := realDirT(t)
	writeTree(t, dir, map[string]string{"a.txt": "hello", "d/b.ts": "b", "d/c.ts": "c"})
	rt := moejs.NewRuntime(moejs.Options{Importer: node.Importer(nil), MemoryLimit: 64 << 20})
	loop := newMiniLoop(rt)
	api, err := node.Install(loop, node.Options{FS: &node.FSOptions{Root: dir, Write: true}, Buffer: true})
	require.NoError(t, err)
	defer api.Close()
	var got string
	require.NoError(t, rt.SetGlobal("done", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		got = moejs.Arg(args, 0).String()
		return moejs.Undefined(), nil
	})))
	runJS := func(body string) error {
		s, err := moejs.CompileScript("t.js", `(async () => { const fs = await import("node:fs"); const fsp = fs.promises; `+body+`
})().then(v => done(JSON.stringify(v)), e => done("REJECTED " + e.code + " " + e.message));`)
		require.NoError(t, err)
		got = ""
		return loop.run(func(rt *moejs.Runtime) error {
			_, err := rt.RunScript(s)
			return err
		})
	}
	require.NoError(t, runJS(`
	const out = [fs.readFileSync("a.txt", "utf8")];
	out.push(await fsp.readFile("a.txt", "utf8"));
	out.push(await new Promise((res, rej) => fs.readFile("a.txt", "utf8", (e, s) => e ? rej(e) : res(s))));
	out.push(await new Promise(res => fs.exists("a.txt", res)));
	out.push(await Array.fromAsync(fsp.glob("d/*.ts")));
	await fsp.writeFile("w.txt", Buffer.from("written"));
	out.push(fs.readFileSync("w.txt", "utf8"));
	const all = await Promise.all(Array.from({length: 50}, (_, i) => fsp.stat(i % 2 ? "a.txt" : "d")));
	out.push(all.filter(s => s.isDirectory()).length);
	try { await fsp.readFile("nope") } catch (e) { out.push(e.code) }
	return out`))
	require.Equal(t, `["hello","hello","hello",true,["d/b.ts","d/c.ts"],"written",25,"ENOENT"]`, got)

	// An exception in a callback reaches the loop as the task's error.
	err = runJS(`fs.readFile("a.txt", () => { throw new Error("from the callback") }); return "started"`)
	var exc *moejs.Exception
	require.True(t, errors.As(err, &exc), "%v", err)
	require.Contains(t, err.Error(), "from the callback")

	// After Close, the operations still to start fail, and the loop
	// ends.
	require.NoError(t, api.Close())
	require.NoError(t, runJS(`try { await fsp.readFile("a.txt") } catch (e) { return e.code }`))
	require.Equal(t, `"EBADF"`, got)

	// The memory limit stops the program from a task: the promises in
	// flight count, with the async functions awaiting them.
	writeTree(t, dir, map[string]string{"big.txt": strings.Repeat("x", 1<<20)})
	rt = moejs.NewRuntime(moejs.Options{Importer: node.Importer(nil), MemoryLimit: 16 << 20})
	loop = newMiniLoop(rt)
	api, err = node.Install(loop, node.Options{FS: &node.FSOptions{Root: dir}})
	require.NoError(t, err)
	defer api.Close()
	require.NoError(t, rt.SetGlobal("done", moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		return moejs.Undefined(), nil
	})))
	err = runJS(`const kept = []; for (let i = 0; i < 100; i++) kept.push(await fsp.readFile("big.txt", "latin1")); return kept.length`)
	require.ErrorIs(t, err, moejs.ErrMemoryLimit)
}

package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zone of a corpus program does not depend on the host

	"github.com/MoeForge/moejs/bench/engines"
	"github.com/stretchr/testify/require"
)

// corpusArg is the host-shaped argument every corpus program receives (most
// ignore it; plugin_shapes.js walks it like a Responses decoder does).
func corpusArg() map[string]any {
	image := "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ", 24)
	return map[string]any{
		"model": "vendor-a", "upstream": map[string]any{"kind": "vendor"},
		"headers": map[string]any{"Authorization": "Bearer sk-test-1234567890", "Content-Type": "application/json", "X-Request-Id": "req_0123456789abcdef"},
		"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": "vendor-a", "size": "1536x1024", "n": float64(2), "seconds": "8",
			"input": []any{
				map[string]any{"role": "system", "content": "You are a helpful image generation assistant. 请根据用户描述生成图片。"},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "A watercolor painting of   a lighthouse at dusk,\nwith seagulls circling."},
					map[string]any{"type": "input_image", "image_url": map[string]any{"url": image}},
					map[string]any{"type": "input_text", "text": "Keep the palette soft."},
				}},
			},
			"metadata": map[string]any{"trace": "abc123", "priority": float64(3), "draft": true, "nested": map[string]any{"ignored": true}},
		}},
	}
}

func corpusFiles(t *testing.T) []string {
	files, err := filepath.Glob(filepath.Join(moduleRoot(), "corpus", "*.js"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	return files
}

// TestCorpusAgainstSobek evaluates every bench/corpus/*.js program on Sobek
// (oracle) and on moejs and compares the exported results. A program whose
// first line is "// script" is a script, not a module: sloppy unless it
// starts with a "use strict" directive, with its completion value as the
// result instead of run(arg).
func TestCorpusAgainstSobek(t *testing.T) {
	oracle := engines.NewSobekEngine()
	subjects := []engines.Engine{engines.NewMoejsEngine()}
	for _, file := range corpusFiles(t) {
		name := strings.TrimSuffix(filepath.Base(file), ".js")
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		t.Run(name, func(t *testing.T) {
			corpusZone(t, string(src))
			expected := runCorpus(t, oracle, name, string(src))
			for _, e := range subjects {
				actual := runCorpus(t, e, name, string(src))
				if d := Diff(expected, actual); d != "" {
					t.Errorf("%s: %s\n  sobek: %s\n  %s: %s", e.Name(), d, CompactJSON(expected), e.Name(), CompactJSON(actual))
				}
			}
		})
	}
}

// corpusZone applies a first line "// zone: Area/City": the program runs
// with time.Local set to that zone (Sobek and both moejs adapters read it at
// call time) until the subtest ends.
func corpusZone(t *testing.T, src string) {
	name, ok := strings.CutPrefix(src, "// zone: ")
	if !ok {
		return
	}
	name, _, _ = strings.Cut(name, "\n")
	loc, err := time.LoadLocation(strings.TrimSpace(name))
	require.NoError(t, err)
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

// sobekPolyfills gives Sobek what it lacks: queueMicrotask (a host API, not
// ECMAScript), where a reaction to a fulfilled promise queues one job at the
// same point, and Promise.withResolvers (ES2024).
const sobekPolyfills = `Object.defineProperty(globalThis, "queueMicrotask", {writable: true, configurable: true, value: function queueMicrotask(cb) {
	if (typeof cb !== "function") throw new TypeError("queueMicrotask: the argument is not a function");
	Promise.resolve().then(() => { cb(); });
}});
Object.defineProperty(Promise, "withResolvers", {writable: true, configurable: true, value: function withResolvers() {
	let resolve, reject;
	const promise = new this((res, rej) => { resolve = res; reject = rej; });
	return {promise, resolve, reject};
}});`

// runCorpus returns the normalized result of run(arg), or a map describing
// the thrown error so that engines are also compared on failures. A promise
// run returns is compared as {state, value} once the call has drained the
// job queue.
func runCorpus(t *testing.T, e engines.Engine, name, src string) any {
	if strings.HasPrefix(src, "// script\n") {
		return runCorpusScript(t, e, name, src)
	}
	mod, err := e.Compile(name+".js", src)
	if err != nil {
		return map[string]any{"compileError": err.Error()}
	}
	rt, err := e.NewRuntime()
	require.NoError(t, err)
	defer rt.Close()
	if s, ok := rt.(*engines.SobekRuntime); ok {
		_, err := s.Sobek().RunString(sobekPolyfills)
		require.NoError(t, err)
	}
	if err := rt.Instantiate(mod); err != nil {
		return map[string]any{"instantiateError": err.Error()}
	}
	out, err := rt.Call("run", nil, corpusArg())
	return corpusResult(t, out, err)
}

// scriptRunner is an engine runtime that runs scripts.
type scriptRunner interface {
	RunScript(name, source string) (any, error)
}

// runCorpusScript returns the normalized completion value of a script
// program, or a map describing its error.
func runCorpusScript(t *testing.T, e engines.Engine, name, src string) any {
	rt, err := e.NewRuntime()
	require.NoError(t, err)
	defer rt.Close()
	sr, ok := rt.(scriptRunner)
	require.True(t, ok, "%s runs no scripts", e.Name())
	out, err := sr.RunScript(name+".js", src)
	return corpusResult(t, out, err)
}

// corpusResult normalizes a program's result, or describes its error so that
// engines are also compared on failures.
func corpusResult(t *testing.T, out any, err error) any {
	if err != nil {
		if he, ok := engines.AsHookError(err); ok {
			return map[string]any{"thrown": he.Name + ": " + he.Message}
		}
		return map[string]any{"error": err.Error()}
	}
	normalized, err := Normalize(out)
	require.NoError(t, err)
	return normalized
}

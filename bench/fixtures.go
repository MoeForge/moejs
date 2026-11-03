// Package bench holds the moejs benchmark and differential-test harness over
// the real new-api task plugins (see engines for the adapters).
package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/MoeForge/moejs/bench/engines"
)

// PluginKeys lists the plugin corpus in report order.
var PluginKeys = []string{"alibaba", "doubao", "google", "hailuo", "jimeng", "kling", "sora", "sunoapi", "vertex-ai", "vidu"}

// Fixture is new-api's pkg/jsplugin.Fixture JSON format.
type Fixture struct {
	UnixNow *int64        `json:"unixNow"`
	Cases   []FixtureCase `json:"cases"`
}

// FixtureCase is one recorded hook call.
type FixtureCase struct {
	Name          string            `json:"name"`
	Hook          string            `json:"hook"`
	Member        string            `json:"member,omitempty"`
	Path          []string          `json:"path,omitempty"`
	Args          []json.RawMessage `json:"args"`
	Expected      json.RawMessage   `json:"expected,omitempty"`
	ExpectedError string            `json:"expectedError,omitempty"`
}

// HookPath returns the member path of the case (member and path are the two
// spellings new-api accepts).
func (c FixtureCase) HookPath() []string {
	if c.Member != "" {
		return []string{c.Member}
	}
	return c.Path
}

// HookName is the dotted hook name used in reports.
func (c FixtureCase) HookName() string { return engines.HookName(c.Hook, c.HookPath()) }

// DecodedArgs returns the JSON arguments as Go values (map[string]any, []any,
// float64, string, bool, nil), exactly what new-api hands to the engine.
func (c FixtureCase) DecodedArgs() ([]any, error) {
	args := make([]any, len(c.Args))
	for i, raw := range c.Args {
		if err := json.Unmarshal(raw, &args[i]); err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
	}
	return args, nil
}

// DecodedExpected returns the expected value as a Go value.
func (c FixtureCase) DecodedExpected() (any, error) {
	var v any
	if len(c.Expected) == 0 {
		return nil, nil
	}
	err := json.Unmarshal(c.Expected, &v)
	return v, err
}

// TestdataDir locates bench/testdata relative to this source file so tests
// and commands work from any working directory inside the module.
func TestdataDir() string {
	if dir := os.Getenv("MOEJS_BENCH_TESTDATA"); dir != "" {
		return dir
	}
	return filepath.Join(moduleRoot(), "testdata")
}

func moduleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// ErrPluginsNotFetched reports that the plugin corpus has not been
// downloaded yet.
var ErrPluginsNotFetched = errors.New("the new-api plugins are not fetched; run bench/testdata/plugins/fetch.sh")

// PluginSource reads bench/testdata/plugins/<key>/plugin.js.
func PluginSource(key string) (string, error) {
	data, err := os.ReadFile(filepath.Join(TestdataDir(), "plugins", key, "plugin.js"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: %w", key, ErrPluginsNotFetched)
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// LoadFixture reads bench/testdata/fixtures/<key>.json.
func LoadFixture(key string) (*Fixture, error) {
	data, err := os.ReadFile(filepath.Join(TestdataDir(), "fixtures", key+".json"))
	if err != nil {
		return nil, err
	}
	var f Fixture
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("fixture %s: %w", key, err)
	}
	return &f, nil
}

// Normalize round-trips a value through encoding/json so that Sobek's int64
// exports, moejs's exports and the cgo engines' unmarshalled JSON compare
// equal (the same normalization new-api's ReplayFixture applies).
func Normalize(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StripNulls removes null-valued object members recursively. JSON.stringify
// drops `undefined` members while Export() keeps them as nil, so results that
// crossed a JSON boundary (quickjs-go, v8go) are compared after both sides
// are stripped.
func StripNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			if item == nil {
				continue
			}
			out[k] = StripNulls(item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = StripNulls(item)
		}
		return out
	}
	return v
}

// CompactJSON renders a normalized value with sorted keys for diagnostics.
func CompactJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(data)
}

// Diff describes the first difference between two normalized values as a
// JSON pointer-ish path, or "" when they are deeply equal.
func Diff(expected, actual any) string {
	return diffAt("$", expected, actual)
}

func diffAt(path string, a, b any) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: expected object, got %s", path, kind(b))
		}
		keys := make([]string, 0, len(x)+len(y))
		for k := range x {
			keys = append(keys, k)
		}
		for k := range y {
			if _, dup := x[k]; !dup {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			xv, xok := x[k]
			yv, yok := y[k]
			switch {
			case !yok:
				return fmt.Sprintf("%s.%s: missing (expected %s)", path, k, CompactJSON(xv))
			case !xok:
				return fmt.Sprintf("%s.%s: unexpected %s", path, k, CompactJSON(yv))
			}
			if d := diffAt(path+"."+k, xv, yv); d != "" {
				return d
			}
		}
		return ""
	case []any:
		y, ok := b.([]any)
		if !ok {
			return fmt.Sprintf("%s: expected array, got %s", path, kind(b))
		}
		if len(x) != len(y) {
			return fmt.Sprintf("%s: expected %d elements, got %d", path, len(x), len(y))
		}
		for i := range x {
			if d := diffAt(fmt.Sprintf("%s[%d]", path, i), x[i], y[i]); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Sprintf("%s: expected %s, got %s", path, CompactJSON(a), CompactJSON(b))
	}
	return ""
}

func kind(v any) string {
	if v == nil {
		return "null"
	}
	return strings.TrimPrefix(reflect.TypeOf(v).String(), "interface {}")
}

// Runner is an engine with every plugin compiled once; it hands out
// instantiated runtimes per plugin.
type Runner struct {
	Engine  engines.Engine
	Modules map[string]engines.CompiledModule
}

// NewRunner compiles the given plugins on the engine.
func NewRunner(e engines.Engine, keys ...string) (*Runner, error) {
	if len(keys) == 0 {
		keys = PluginKeys
	}
	r := &Runner{Engine: e, Modules: make(map[string]engines.CompiledModule, len(keys))}
	for _, key := range keys {
		src, err := PluginSource(key)
		if err != nil {
			return nil, err
		}
		m, err := e.Compile(key+".js", src)
		if err != nil {
			return nil, fmt.Errorf("%s: compile %s: %w", e.Name(), key, err)
		}
		r.Modules[key] = m
	}
	return r, nil
}

// Runtime creates a runtime with the plugin instantiated.
func (r *Runner) Runtime(key string) (engines.Runtime, error) {
	m, ok := r.Modules[key]
	if !ok {
		return nil, fmt.Errorf("%s: plugin %s not compiled", r.Engine.Name(), key)
	}
	rt, err := r.Engine.NewRuntime()
	if err != nil {
		return nil, err
	}
	if err := rt.Instantiate(m); err != nil {
		rt.Close()
		return nil, fmt.Errorf("%s: instantiate %s: %w", r.Engine.Name(), key, err)
	}
	return rt, nil
}

// Replay runs one fixture case on rt and returns the normalized result or
// the hook error.
func Replay(rt engines.Runtime, c FixtureCase) (any, error) {
	args, err := c.DecodedArgs()
	if err != nil {
		return nil, err
	}
	out, err := rt.Call(c.Hook, c.HookPath(), args...)
	if err != nil {
		return nil, err
	}
	return Normalize(out)
}

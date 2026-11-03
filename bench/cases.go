package bench

import (
	"fmt"
	"sync"

	"github.com/MoeForge/moejs/bench/engines"
)

// HookCase is one fixture case with its arguments decoded once and its
// expectation prepared for cheap per-iteration validation (benchmarks and
// the PGO profiler share it).
type HookCase struct {
	Plugin string
	Case   FixtureCase
	Args   []any
	// Expected is the normalized oracle result (nil for throw cases).
	Expected any
	Shape    Shape
}

// Shape is the cheap per-iteration validator: the Go kind of the result and
// its top-level size, or the expected error message. It catches an engine
// that fails fast or returns the wrong thing without paying for a deep
// comparison inside a timed loop; the first iteration of every benchmark
// still runs the full comparison (FullCheck).
type Shape struct {
	Kind    byte // 'm' map, 'a' array, 's' string, 'n' number, 'b' bool, '0' null, 'e' error
	Size    int
	Message string
}

func shapeOf(expected any, expectedError string) Shape {
	if expectedError != "" {
		return Shape{Kind: 'e', Message: expectedError}
	}
	switch t := expected.(type) {
	case map[string]any:
		return Shape{Kind: 'm', Size: len(t)}
	case []any:
		return Shape{Kind: 'a', Size: len(t)}
	case string:
		return Shape{Kind: 's', Size: len(t)}
	case float64:
		return Shape{Kind: 'n'}
	case bool:
		return Shape{Kind: 'b'}
	}
	return Shape{Kind: '0'}
}

// Check validates a hook outcome against the shape; it returns "" when fine.
func (h *HookCase) Check(out any, err error) string {
	s := h.Shape
	if s.Kind == 'e' {
		he, ok := engines.AsHookError(err)
		if !ok {
			return fmt.Sprintf("expected throw %q, got %v / %v", s.Message, out, err)
		}
		if he.Message != s.Message {
			return fmt.Sprintf("expected throw %q, got %q", s.Message, he.Message)
		}
		return ""
	}
	if err != nil {
		return fmt.Sprintf("unexpected error: %v", err)
	}
	switch t := out.(type) {
	case map[string]any:
		if s.Kind != 'm' || len(t) != s.Size {
			return fmt.Sprintf("expected %c/%d, got object with %d keys", s.Kind, s.Size, len(t))
		}
	case []any:
		if s.Kind != 'a' || len(t) != s.Size {
			return fmt.Sprintf("expected %c/%d, got array with %d elements", s.Kind, s.Size, len(t))
		}
	case string:
		if s.Kind != 's' || len(t) != s.Size {
			return fmt.Sprintf("expected %c/%d, got string of %d bytes", s.Kind, s.Size, len(t))
		}
	case nil:
		if s.Kind != '0' {
			return fmt.Sprintf("expected %c, got null", s.Kind)
		}
	case bool:
		if s.Kind != 'b' {
			return fmt.Sprintf("expected %c, got bool", s.Kind)
		}
	default:
		// numbers export as int64 or float64 depending on the engine
		if s.Kind != 'n' {
			return fmt.Sprintf("expected %c, got %T", s.Kind, out)
		}
	}
	return ""
}

// FullCheck runs the complete JSON-normalized comparison against the oracle.
// lenient drops null-valued object members on both sides for engines whose
// results crossed a JSON.stringify boundary (undefined members vanish there):
// both quickjs-go configurations and v8go, see JSONBoundary.
func (h *HookCase) FullCheck(out any, err error, lenient bool) string {
	if h.Case.ExpectedError != "" {
		return h.Check(out, err)
	}
	if err != nil {
		return fmt.Sprintf("unexpected error: %v", err)
	}
	actual, nerr := Normalize(out)
	if nerr != nil {
		return nerr.Error()
	}
	expected := h.Expected
	if lenient {
		expected, actual = StripNulls(expected), StripNulls(actual)
	}
	return Diff(expected, actual)
}

// JSONBoundary reports whether the engine moves values as JSON text, so that
// `undefined` object members are dropped on the way back: the cgo engines and
// modernc.org/quickjs.
func JSONBoundary(e engines.Engine) bool {
	if _, ok := e.(*engines.ModerncQuickJSEngine); ok {
		return true
	}
	return engines.Cgo(e)
}

// Run invokes the case on rt and returns the raw outcome.
func (h *HookCase) Run(rt engines.Runtime) (any, error) {
	return rt.Call(h.Case.Hook, h.Case.HookPath(), h.Args...)
}

// LoadCases loads every fixture case of every plugin, once per process.
var LoadCases = sync.OnceValues(func() ([]HookCase, error) {
	var cases []HookCase
	for _, key := range PluginKeys {
		f, err := LoadFixture(key)
		if err != nil {
			return nil, err
		}
		for _, c := range f.Cases {
			args, err := c.DecodedArgs()
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", key, c.Name, err)
			}
			expected, err := c.DecodedExpected()
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", key, c.Name, err)
			}
			cases = append(cases, HookCase{Plugin: key, Case: c, Args: args, Expected: expected, Shape: shapeOf(expected, c.ExpectedError)})
		}
	}
	return cases, nil
})

// AcceptedDifferences lists fixture cases whose result legitimately differs
// from the recorded Sobek oracle on a given engine, keyed by
// "<engine>/<plugin>/<case>". Every entry needs a justification. Benchmarks
// skip the full oracle comparison for these cases and keep the shape check.
var AcceptedDifferences = map[string]string{
	"sobek/sora/build submit multipart with file": "Sobek exposes Go map iteration order through Object.keys on host maps (ToValue wraps map[string]any as a live proxy), so the multipart parts[] order is random between Sobek runs; the recording captured one order. moejs, quickjs-go and v8go see the keys in sorted (JSON) order and always match.",
	"modernc-quickjs/sunoapi/parse batch result":  "modernc.org/quickjs drops explicit-null object members during its internal JSON round-trip (Object.Into), identical to quickjs-go's JSON boundary behaviour.",
}

// Accepted reports whether the case is an accepted difference for the engine.
func (h *HookCase) Accepted(e engines.Engine) bool {
	_, ok := AcceptedDifferences[e.Name()+"/"+h.Plugin+"/"+h.Case.Name]
	return ok
}

// RepoRoot is the moejs repository root (the parent of the bench module).
func RepoRoot() string { return moduleRoot() + "/.." }

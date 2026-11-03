package bench

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/bench/engines"
	"github.com/stretchr/testify/require"
)

// mismatch is one differential-test failure, reported in full so it can be
// reproduced from the fixture alone.
type mismatch struct {
	engine, plugin, name, hook, detail string
}

func (m mismatch) String() string {
	return fmt.Sprintf("%s: %s/%q (%s): %s", m.engine, m.plugin, m.name, m.hook, m.detail)
}

// replayCase compares one fixture case on rt with its recorded expectation.
// lenient drops null-valued object members on both sides for engines whose
// results crossed a JSON.stringify boundary (undefined members vanish there).
func replayCase(rt engines.Runtime, c FixtureCase, lenient bool) string {
	actual, err := Replay(rt, c)
	if c.ExpectedError != "" {
		if err == nil {
			return fmt.Sprintf("expected throw %q, got result %s", c.ExpectedError, CompactJSON(actual))
		}
		he, ok := engines.AsHookError(err)
		if !ok {
			return fmt.Sprintf("expected throw %q, got non-JS error: %v", c.ExpectedError, err)
		}
		if he.Message != c.ExpectedError {
			return fmt.Sprintf("expected throw %q, got %q", c.ExpectedError, he.Message)
		}
		return ""
	}
	if err != nil {
		if errors.Is(err, engines.ErrNotFound) {
			return "hook not found"
		}
		return fmt.Sprintf("unexpected error: %v", err)
	}
	expected, derr := c.DecodedExpected()
	if derr != nil {
		return fmt.Sprintf("fixture: %v", derr)
	}
	if lenient {
		expected, actual = StripNulls(expected), StripNulls(actual)
	}
	return Diff(expected, actual)
}

// TestDifferentialFixtures replays every recorded plugin hook on every engine
// and compares JSON-normalized results with the Sobek oracle. Sobek itself is
// replayed as a determinism check of the recording.
func TestDifferentialFixtures(t *testing.T) {
	skipWithoutPlugins(t)
	var all []mismatch
	for _, e := range engines.All() {
		lenient := JSONBoundary(e)
		t.Run(e.Name(), func(t *testing.T) {
			runner, err := NewRunner(e)
			require.NoError(t, err)
			total, passed := 0, 0
			for _, key := range PluginKeys {
				fixture, err := LoadFixture(key)
				require.NoError(t, err)
				rt, err := runner.Runtime(key)
				if err != nil {
					t.Errorf("%s: instantiate %s: %v", e.Name(), key, err)
					all = append(all, mismatch{e.Name(), key, "<instantiate>", "", err.Error()})
					continue
				}
				for _, c := range fixture.Cases {
					total++
					detail := replayCase(rt, c, lenient)
					if detail == "" {
						passed++
						continue
					}
					id := e.Name() + "/" + key + "/" + c.Name
					if why, ok := AcceptedDifferences[id]; ok {
						t.Logf("accepted difference %s: %s (%s)", id, detail, why)
						passed++
						continue
					}
					m := mismatch{e.Name(), key, c.Name, c.HookName(), detail}
					all = append(all, m)
					t.Errorf("%s", m)
				}
				rt.Close()
			}
			t.Logf("%s: %d/%d fixture cases match the oracle", e.Name(), passed, total)
		})
	}
	if len(all) == 0 {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].String() < all[j].String() })
	var b strings.Builder
	for _, m := range all {
		b.WriteString("  ")
		b.WriteString(m.String())
		b.WriteByte('\n')
	}
	t.Logf("differential mismatches (%d):\n%s", len(all), b.String())
}

// TestFixturesCoverHookFamilies guards the fixture corpus itself: every
// plugin has at least six cases and covers decode, build, parse and usage
// hooks, as the benchmark contract requires.
func TestFixturesCoverHookFamilies(t *testing.T) {
	families := map[string]func(string) bool{
		"decode": func(h string) bool {
			return strings.HasSuffix(h, "decodeRequest") || strings.HasPrefix(h, "native.decode") || strings.HasPrefix(h, "native.create")
		},
		"build": func(h string) bool { return strings.HasPrefix(h, "build") },
		"parse": func(h string) bool { return strings.HasPrefix(h, "parse") },
		"usage": func(h string) bool { return strings.HasPrefix(h, "extractUsage") },
	}
	for _, key := range PluginKeys {
		fixture, err := LoadFixture(key)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(fixture.Cases), 6, key)
		for family, match := range families {
			found := false
			for _, c := range fixture.Cases {
				if match(c.HookName()) {
					found = true
					break
				}
			}
			require.True(t, found, "%s has no %s hook case", key, family)
		}
	}
}

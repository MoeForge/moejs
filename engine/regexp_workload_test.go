package engine

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// regexpWorkloadPin is the engine every regular expression of the plugin
// workload (bench/testdata/plugins, pinned by fetch.sh) and of the bench
// corpus selected at b7ad134, before the Annex B grammar work of sloppy M3:
// the syntax the plugins use must keep its engine. A line is the file and
// byte offset of the literal or RegExp call, the engine (simple for the
// single-class fast path, re2 with +cr or +fold when some subjects need the
// backtracking VM, bt, or error), and the FNV-32a hash of /pattern/flags,
// which keeps the plugins' text (AGPL-3.0) out of the repository. A change
// to it is a decision to move a pattern to another engine, never an update.
const regexpWorkloadPin = "testdata/regexp_workload_pin.txt"

// TestRegExpWorkloadSelection: every regular expression of the workload
// selects the engine it selected at b7ad134 (regexpWorkloadPin), and outside
// the backtracking corpus program, which exercises the VM on purpose, runs
// on RE2 or the single-class fast path: the backtracking VM only takes
// patterns RE2 cannot run exactly.
func TestRegExpWorkloadSelection(t *testing.T) {
	plugins, err := filepath.Glob("../bench/testdata/plugins/*/plugin.js")
	require.NoError(t, err)
	corpus, err := filepath.Glob("../bench/corpus/*.js")
	require.NoError(t, err)
	require.NotEmpty(t, corpus)
	pin, err := os.ReadFile(regexpWorkloadPin)
	require.NoError(t, err)
	var want []string
	for _, line := range strings.Split(strings.TrimSpace(string(pin)), "\n") {
		if strings.HasPrefix(line, "#") || len(plugins) == 0 && strings.HasPrefix(line, "bench/testdata/plugins/") {
			continue
		}
		want = append(want, line)
	}
	got := workloadSelection(t, append(plugins, corpus...))
	assert.Equal(t, strings.Join(want, "\n"), strings.Join(got, "\n"))
	if len(plugins) == 0 {
		t.Skip("the new-api plugins are not fetched (run bench/testdata/plugins/fetch.sh): only the corpus is checked")
	}
	assert.Greater(t, len(got), 50, "the workload's regular expressions were found")
	t.Logf("%d regular expressions in %d files", len(got), len(plugins)+len(corpus))
}

// workloadSelection returns the pin lines of the regular expressions of
// files, in file and source order.
func workloadSelection(t *testing.T, files []string) []string {
	r := NewRealm()
	var lines []string
	for _, path := range files {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		var prog syntax.Node
		if strings.HasPrefix(string(src), "// script\n") { // a sloppy corpus script
			prog, err = syntax.ParseScript(path, string(src), syntax.Options{})
		} else {
			prog, err = syntax.ParseModule(path, string(src), syntax.Options{})
		}
		require.NoError(t, err, path)
		syntax.Inspect(prog, func(n syntax.Node) bool {
			pattern, flags, ok := workloadRegExp(n)
			if !ok {
				return true
			}
			f, valid := parseRegExpFlags(flags)
			require.True(t, valid, "%s: /%s/%s", path, pattern, flags)
			engine := "error"
			c, err := r.compileRegExp(FromGoString(pattern), f)
			if err == nil {
				engine = regexpEngine(c)
			}
			if filepath.Base(path) != "regexp_bt.js" {
				if pattern == "(" {
					assert.Error(t, err, "the corpus's invalid pattern")
				} else if assert.NoError(t, err, "%s: /%s/%s", path, pattern, flags) {
					assert.True(t, c.re != nil || c.simple != nil, "%s: /%s/%s must run on RE2 or the fast path", path, pattern, flags)
				}
			}
			h := fnv.New32a()
			fmt.Fprintf(h, "/%s/%s", pattern, flags)
			pos, _ := n.Range()
			lines = append(lines, fmt.Sprintf("%s:%d %s %08x", strings.TrimPrefix(path, "../"), pos, engine, h.Sum32()))
			return true
		})
	}
	return lines
}

// regexpEngine names the engine c selects.
func regexpEngine(c *compiledRegExp) string {
	switch {
	case c.simple != nil:
		return "simple"
	case c.re == nil:
		return "bt"
	}
	engine := "re2"
	if c.btCR {
		engine += "+cr"
	}
	if c.btFoldWord {
		engine += "+fold"
	}
	return engine
}

// workloadRegExp returns the pattern and flags of a regular expression
// literal or of RegExp called with string literals.
func workloadRegExp(n syntax.Node) (pattern, flags string, ok bool) {
	var callee syntax.Expr
	var args []syntax.Expr
	switch n := n.(type) {
	case *syntax.RegexLit:
		return n.Pattern, n.Flags, true
	case *syntax.NewExpr:
		callee, args = n.Callee, n.Args
	case *syntax.CallExpr:
		callee, args = n.Callee, n.Args
	default:
		return "", "", false
	}
	if id, isIdent := callee.(*syntax.Ident); !isIdent || id.Name != "RegExp" || len(args) == 0 || len(args) > 2 {
		return "", "", false
	}
	for i, a := range args {
		s, isStr := a.(*syntax.StringLit)
		if !isStr {
			return "", "", false
		}
		if i == 0 {
			pattern = s.Value
		} else {
			flags = s.Value
		}
	}
	return pattern, flags, true
}

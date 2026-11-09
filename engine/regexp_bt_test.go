package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Semantics of the backtracking VM (regexp_bt*.go). The expected values are
// node's (V8) unless a comment says the spec differs.
func TestRegExpBacktrackSemantics(t *testing.T) {
	for _, c := range regexpBTCases {
		t.Run(c.js, func(t *testing.T) {
			assert.Equal(t, c.expected(), auditEvalJS(t, c.js), "%s", c.js)
		})
	}
}

var regexpBTCases = []auditJSCase{
	// --- backreferences: numbered, named, forward, non-participating, case-folded ---
	{js: "/(a)\\1/.exec(\"baab\")", want: "[\"aa\",\"a\"]{index:1,input:\"baab\",groups:undefined}"},
	{js: "/(a)\\1/.exec(\"ab\")", want: "null"},
	{js: "/\\1(a)/.exec(\"aa\")", want: "[\"a\",\"a\"]{index:0,input:\"aa\",groups:undefined}"},
	{js: "/(?:(a)|b)\\1c/.exec(\"bc\")", want: "[\"bc\",undefined]{index:0,input:\"bc\",groups:undefined}"},
	{js: "/(?<x>[ab])\\k<x>/.exec(\"abba\")", want: "[\"bb\",\"b\"]{index:1,input:\"abba\",groups:{x:\"b\"}}"},
	{js: "/\\k<x>(?<x>a)/.exec(\"a\")", want: "[\"a\",\"a\"]{index:0,input:\"a\",groups:{x:\"a\"}}"},
	{js: "/(a)|\\1b/.exec(\"b\")", want: "[\"b\",undefined]{index:0,input:\"b\",groups:undefined}"},
	{js: "/(a*)b\\1+/.exec(\"baaaac\")", want: "[\"b\",\"\"]{index:0,input:\"baaaac\",groups:undefined}"},
	{js: "/(\\w+)\\s\\1/.exec(\"hello hello world\")", want: "[\"hello hello\",\"hello\"]{index:0,input:\"hello hello world\",groups:undefined}"},
	{js: "/(a)\\1/i.exec(\"aA\")", want: "[\"aA\",\"a\"]{index:0,input:\"aA\",groups:undefined}"},
	{js: "/(\u017f)\\1/iu.exec(\"\u017fs\")", want: "[\"\u017fs\",\"\u017f\"]{index:0,input:\"\u017fs\",groups:undefined}"},
	{js: "/(\u017f)\\1/i.exec(\"\u017fs\")", want: "null"},
	{js: "/(\u00df)\\1/iu.exec(\"\u00df\u1e9e\")", want: "[\"\u00df\u1e9e\",\"\u00df\"]{index:0,input:\"\u00df\u1e9e\",groups:undefined}"},
	// --- lookahead and lookbehind (right to left), captures inside them ---
	{js: "/a(?=b)/.exec(\"acab\")", want: "[\"a\"]{index:2,input:\"acab\",groups:undefined}"},
	{js: "/a(?=(b))/.exec(\"ab\")", want: "[\"a\",\"b\"]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/a(?!b)/.exec(\"abac\")", want: "[\"a\"]{index:2,input:\"abac\",groups:undefined}"},
	{js: "/a(?!(b))/.exec(\"ac\")", want: "[\"a\",undefined]{index:0,input:\"ac\",groups:undefined}"},
	{js: "/(?=(a+))a*b\\1/.exec(\"baaabac\")", want: "[\"aba\",\"a\"]{index:3,input:\"baaabac\",groups:undefined}"},
	{js: "/(?=(a+))/.exec(\"baaabac\")", want: "[\"\",\"aaa\"]{index:1,input:\"baaabac\",groups:undefined}"},
	{js: "/(.*?)a(?!(a+)b\\2c)\\2(.*)/.exec(\"baaabaac\")", want: "[\"baaabaac\",\"ba\",undefined,\"abaac\"]{index:0,input:\"baaabaac\",groups:undefined}"},
	{js: "/(?<=\\$)\\d+/.exec(\"cost $42\")", want: "[\"42\"]{index:6,input:\"cost $42\",groups:undefined}"},
	{js: "/(?<!\\$)\\b\\d+/.exec(\"$4 and 17\")", want: "[\"17\"]{index:7,input:\"$4 and 17\",groups:undefined}"},
	{js: "/(?<=(\\d+)(\\d+))$/.exec(\"1053\")", want: "[\"\",\"1\",\"053\"]{index:4,input:\"1053\",groups:undefined}"},
	{js: "/(?<=\\1(a))b/.exec(\"aab\")", want: "[\"b\",\"a\"]{index:2,input:\"aab\",groups:undefined}"},
	{js: "/(?<=(a)\\1)b/.exec(\"aab\")", want: "[\"b\",\"a\"]{index:2,input:\"aab\",groups:undefined}"},
	{js: "/(?<=([ab]+))c/.exec(\"abc\")", want: "[\"c\",\"ab\"]{index:2,input:\"abc\",groups:undefined}"},
	{js: "\"a,bb,c\".replace(/(?<=^|,)\\w+/g, \"[$&]\")", want: "\"[a],[bb],[c]\""},
	{js: "/(?<=a(?=b)b)c/.exec(\"abc\")", want: "[\"c\"]{index:2,input:\"abc\",groups:undefined}"},
	{js: "/(?<!a)b/.exec(\"abcb\")", want: "[\"b\"]{index:3,input:\"abcb\",groups:undefined}"},
	{js: "/(?<=\\u{1F600})x/u.exec(\"\\u{1F600}x\")", want: "[\"x\"]{index:2,input:\"\U0001f600x\",groups:undefined}"},
	{js: "/(?<=.)x/u.exec(\"\\u{1F600}x\")", want: "[\"x\"]{index:2,input:\"\U0001f600x\",groups:undefined}"},
	{js: "/(?<=^.)x/u.exec(\"\\u{1F600}x\")", want: "[\"x\"]{index:2,input:\"\U0001f600x\",groups:undefined}"},
	{js: "/(?<=^.)x/.exec(\"\\u{1F600}x\")", want: "null"},
	{js: "/(?<=^..)x/.exec(\"\\u{1F600}x\")", want: "[\"x\"]{index:2,input:\"\U0001f600x\",groups:undefined}"},
	// --- RepeatMatcher: per-iteration capture reset, the empty check, lazy quantifiers ---
	{js: "/(z)((a+)?(b+)?(c))*/.exec(\"zaacbbbcac\")", want: "[\"zaacbbbcac\",\"z\",\"ac\",\"a\",undefined,\"c\"]{index:0,input:\"zaacbbbcac\",groups:undefined}"},
	{js: "/(a)|b/.exec(\"b\")", want: "[\"b\",undefined]{index:0,input:\"b\",groups:undefined}"},
	{js: "/(?:(a)|b)+/.exec(\"ab\")", want: "[\"ab\",undefined]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/(?:(a)b|(a)c)+/.exec(\"abac\")", want: "[\"abac\",undefined,\"a\"]{index:0,input:\"abac\",groups:undefined}"},
	{js: "/((a)|(b))+/.exec(\"ab\")", want: "[\"ab\",\"b\",undefined,\"b\"]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/(?:a|())*/.exec(\"aa\")", want: "[\"aa\",undefined]{index:0,input:\"aa\",groups:undefined}"},
	{js: "/(a*)+/.exec(\"b\")", want: "[\"\",\"\"]{index:0,input:\"b\",groups:undefined}"},
	{js: "/(a*)*?b/.exec(\"b\")", want: "[\"b\",undefined]{index:0,input:\"b\",groups:undefined}"},
	{js: "/(a|)*b/.exec(\"aab\")", want: "[\"aab\",\"a\"]{index:0,input:\"aab\",groups:undefined}"},
	{js: "/(?:a*?)*b/.exec(\"aab\")", want: "[\"aab\"]{index:0,input:\"aab\",groups:undefined}"},
	{js: "/(a*?)*b/.exec(\"aab\")", want: "[\"aab\",\"a\"]{index:0,input:\"aab\",groups:undefined}"},
	{js: "/(?:(a)|b)*?c/.exec(\"abc\")", want: "[\"abc\",undefined]{index:0,input:\"abc\",groups:undefined}"},
	{js: "/((a)|b)*?c/.exec(\"abc\")", want: "[\"abc\",\"b\",undefined]{index:0,input:\"abc\",groups:undefined}"},
	{js: "/(a+?)(b*)/.exec(\"aabb\")", want: "[\"a\",\"a\",\"\"]{index:0,input:\"aabb\",groups:undefined}"},
	{js: "/a{2,4}?/.exec(\"aaaaa\")", want: "[\"aa\"]{index:0,input:\"aaaaa\",groups:undefined}"},
	{js: "/(?:ab){2,3}?c/.exec(\"abababc\")", want: "[\"abababc\"]{index:0,input:\"abababc\",groups:undefined}"},
	// --- counts above RE2's 1000 run as counters; quantified lookarounds ---
	{js: "/a{1001}/.test(\"a\".repeat(1001))", want: "true"},
	{js: "/a{1001}/.test(\"a\".repeat(1000))", want: "false"},
	{js: "/^(?:ab){1500}$/.test(\"ab\".repeat(1500))", want: "true"},
	{js: "/^(?:ab){1500}$/.test(\"ab\".repeat(1499))", want: "false"},
	{js: "/^(a){2000,}$/.exec(\"a\".repeat(2001))[1]", want: "\"a\""},
	{js: "/^(?:a{1000}){3}b/.test(\"a\".repeat(3000) + \"b\")", want: "true"},
	{js: "/(?=a)*a/.exec(\"a\")", want: "[\"a\"]{index:0,input:\"a\",groups:undefined}"},
	{js: "/(?=(a))?a/.exec(\"a\")", want: "[\"a\",undefined]{index:0,input:\"a\",groups:undefined}"},
	{js: "/(?=(a)){2}a/.exec(\"a\")", want: "[\"a\",\"a\"]{index:0,input:\"a\",groups:undefined}"},
	{js: "/(?!a)+b/.exec(\"b\")", want: "[\"b\"]{index:0,input:\"b\",groups:undefined}"},
	// --- m-mode line terminators, dotAll ---
	{js: "/^a$/m.exec(\"b\\ra\")", want: "[\"a\"]{index:2,input:\"b\\ra\",groups:undefined}"},
	{js: "/^a$/m.exec(\"b\u2028a\u2029c\")", want: "[\"a\"]{index:2,input:\"b\u2028a\u2029c\",groups:undefined}"},
	{js: "/a$/m.exec(\"a\\rb\")", want: "[\"a\"]{index:0,input:\"a\\rb\",groups:undefined}"},
	{js: "\"a\\rb\\nc\u2028d\".replace(/^/gm, \"#\")", want: "\"#a\\r#b\\n#c\u2028#d\""},
	{js: "\"a\\rb\\nc\u2029d\".replace(/$/gm, \"#\")", want: "\"a#\\rb#\\nc#\u2029d#\""},
	{js: "/(a)\\1/s.exec(\"a\\naa\")", want: "[\"aa\",\"a\"]{index:2,input:\"a\\naa\",groups:undefined}"},
	{js: "/.\\1/s.exec(\"\\n\\n\")", want: "null"},
	{js: "/(.)\\1/s.exec(\"x\\n\\n\")", want: "[\"\\n\\n\",\"\\n\"]{index:1,input:\"x\\n\\n\",groups:undefined}"},
	{js: "/(.)\\1/.exec(\"x\\n\\n\")", want: "null"},
	// --- sticky and global ---
	{js: "{ var r = /(a)\\1/y; r.lastIndex = 1; var m1 = r.exec(\"baa\"); r.lastIndex = 0; return [m1 && m1.index, r.exec(\"baa\"), r.lastIndex]; }", want: "[1,null,0]"},
	{js: "{ var r = /(a)\\1/g; var out = []; var m; while ((m = r.exec(\"aabaaaa\")) !== null) out.push(m.index, r.lastIndex); return out; }", want: "[0,2,3,5,5,7]"},
	{js: "\"aabaaaa\".replace(/(a)\\1/g, \"[$1]\")", want: "\"[a]b[a][a]\""},
	{js: "\"aabaaaa\".match(/(a)\\1/g)", want: "[\"aa\",\"aa\",\"aa\"]"},
	// --- split with lookarounds ---
	{js: "\"a1b2c3\".split(/(?<=\\d)/)", want: "[\"a1\",\"b2\",\"c3\"]"},
	{js: "\"a1b2c3\".split(/(?=\\d)/)", want: "[\"a\",\"1b\",\"2c\",\"3\"]"},
	{js: "\"abcabc\".split(/(?<=(b))/)", want: "[\"ab\",\"b\",\"cab\",\"b\",\"c\"]"},
	// --- hasIndices ---
	{js: "/(?<y>a)(?<=\\k<y>)/d.exec(\"xa\")", want: "[\"a\",\"a\"]{index:1,input:\"xa\",groups:{y:\"a\"},indices:[[1,2],[1,2]]{groups:{y:[1,2]}}}"},
	{js: "/(a)(?=(b))/d.exec(\"ab\")", want: "[\"a\",\"a\",\"b\"]{index:0,input:\"ab\",groups:undefined,indices:[[0,1],[0,1],[1,2]]{groups:undefined}}"},
	{js: "/(?<a>.)(?<b>\\k<a>)/d.exec(\"xyy\")", want: "[\"yy\",\"y\",\"y\"]{index:1,input:\"xyy\",groups:{a:\"y\",b:\"y\"},indices:[[1,3],[1,2],[2,3]]{groups:{a:[1,2],b:[2,3]}}}"},
	// --- lastIndex inside a surrogate pair (u) ---
	// The match starts at the pair (inputIndex) but the result starts at
	// lastIndex; the empty match's end is clamped to it (V8: [0,0]).
	{js: "{ var r = /(?:)/gu; r.lastIndex = 1; var m = r.exec(\"\\u{1F600}\"); return [m.index, r.lastIndex]; }", want: "[1,1]"},
	{js: "/\\udc00/gu.exec(\"\\u{10000}\")", want: "null"},
	// Spec: index and match start at lastIndex (V8 reports the pair's start).
	{js: "{ var r = /./gu; r.lastIndex = 1; var m = r.exec(\"\\u{1F600}x\"); return [m[0], m.index, r.lastIndex]; }", want: "[\"\\ude00\",1,2]"},
	{js: "{ var r = /x/gu; r.lastIndex = 1; var m = r.exec(\"\\u{1F600}x\"); return [m[0], m.index, r.lastIndex]; }", want: "[\"x\",2,3]"},
	{js: "{ var r = /x/yu; r.lastIndex = 1; var m = r.exec(\"\\u{1F600}x\"); return [m, r.lastIndex]; }", want: "[null,0]"},
	// --- surrogates: pairs are one character with u, lone surrogates match ---
	{js: "/\\ud83d/.exec(\"\\u{1F600}\")", want: "[\"\\ud83d\"]{index:0,input:\"\U0001f600\",groups:undefined}"},
	{js: "/\\ud83d/u.exec(\"\\u{1F600}\")", want: "null"},
	{js: "/\\ude00/u.exec(\"\\u{1F600}\\ude00\")", want: "[\"\\ude00\"]{index:2,input:\"\U0001f600\\ude00\",groups:undefined}"},
	{js: "/^.$/u.test(\"\\u{1F600}\")", want: "true"},
	{js: "/^.$/.test(\"\\u{1F600}\")", want: "false"},
	{js: "/^(.)\\1$/u.test(\"\\u{1F600}\\u{1F600}\")", want: "true"},
	{js: "/(\\ud83d)\\1/u.exec(\"\\ud83d\U0001f600\")", want: "null"},
	{js: "/(\\ude00)\\1/u.exec(\"\\ude00\U0001f600\")", want: "null"},
	{js: "/(?<=\\ude00)x/u.exec(\"\U0001f600x\")", want: "null"},
	{js: "/(?<=\\ude00)x/.exec(\"\U0001f600x\")", want: "[\"x\"]{index:2,input:\"\U0001f600x\",groups:undefined}"},
	{js: "/\\u{F0000}/u.exec(\"\\ud800\")", want: "null"},
	{js: "/\\u{F0000}/u.exec(\"a\\u{F0000}\")", want: "[\"\U000f0000\"]{index:1,input:\"a\U000f0000\",groups:undefined}"},
	{js: "/[^a]/u.exec(\"\\u{F0000}\")", want: "[\"\U000f0000\"]{index:0,input:\"\U000f0000\",groups:undefined}"},
	{js: "/^[\\u{F0000}-\\u{F07FF}]$/u.test(\"\U00100000\")", want: "false"},
	{js: "/\\u{F0000}/u.test(\"\\u{F0000}\")", want: "true"},
	{js: "/[\\ud800]/u.test(\"\\u{F0000}\")", want: "false"},
	// --- \b and \B with u and i: U+017F and U+212A are word characters ---
	{js: "/\\bk/iu.exec(\"\u212ak\")", want: "[\"\u212a\"]{index:0,input:\"\u212ak\",groups:undefined}"},
	{js: "/\\B\u212a/iu.exec(\"a\u212a\")", want: "[\"\u212a\"]{index:1,input:\"a\u212a\",groups:undefined}"},
	{js: "/k\\b/iu.exec(\"k\u212a\")", want: "[\"\u212a\"]{index:1,input:\"k\u212a\",groups:undefined}"},
	{js: "/\\w/iu.test(\"\u212a\")", want: "true"},
	// --- more RepeatMatcher, lookbehind and backreference corners ---
	{js: "/^(a+)+$/.test(\"aaaa\")", want: "true"},
	{js: "/(a)|(b)/.exec(\"b\")", want: "[\"b\",undefined,\"b\"]{index:0,input:\"b\",groups:undefined}"},
	{js: "/(k)\\1/i.exec(\"k\u212a\")", want: "null"},
	{js: "/(k)\\1/iu.exec(\"k\u212a\")", want: "[\"k\u212a\",\"k\"]{index:0,input:\"k\u212a\",groups:undefined}"},
	{js: "/(a?)??b/.exec(\"ab\")", want: "[\"ab\",\"a\"]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/(?:(?=(\\w))\\w)+/.exec(\"ab\")", want: "[\"ab\",\"b\"]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/(?:(?=(\\w))\\w|-)+/.exec(\"a-\")", want: "[\"a-\",undefined]{index:0,input:\"a-\",groups:undefined}"},
	{js: "/(ab){1001}/.test(\"ab\".repeat(1001))", want: "true"},
	{js: "/((a)|b){1001}/.exec(\"a\" + \"b\".repeat(1000)).slice(1)", want: "[\"b\",undefined]"},
	{js: "/(?:a?){2,}b/.exec(\"ab\")", want: "[\"ab\"]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/(?:a|b?){3}c/.exec(\"abc\")", want: "[\"abc\"]{index:0,input:\"abc\",groups:undefined}"},
	{js: "/(?:a|b?)+c/.exec(\"abc\")", want: "[\"abc\"]{index:0,input:\"abc\",groups:undefined}"},
	{js: "/(?:()|a)+b/.exec(\"aab\")", want: "[\"aab\",undefined]{index:0,input:\"aab\",groups:undefined}"},
	{js: "\"ab cd\".replace(/\\b(?=\\w)/g, \"|\")", want: "\"|ab |cd\""},
	{js: "\"foo.bar.baz\".replace(/(?<=\\.)\\w+/g, (m) => m.toUpperCase())", want: "\"foo.BAR.BAZ\""},
	{js: "\"aaa\".replace(/a*?/g, \"-\")", want: "\"-a-a-a-\""},
	{js: "\"aaa\".replace(/(?:)/gu, \"-\")", want: "\"-a-a-a-\""},
	{js: "\"\\u{1F600}\\u{1F600}\".replace(/(?:)/gu, \"-\")", want: "\"-\U0001f600-\U0001f600-\""},
	{js: "\"\\u{1F600}\\u{1F600}\".replace(/(?:)/g, \"-\").length", want: "9"},
	{js: "\"\\u{1F600}x\".split(/(?:)/u)", want: "[\"\U0001f600\",\"x\"]"},
	{js: "\"\\u{1F600}x\".split(/(?=x)/u)", want: "[\"\U0001f600\",\"x\"]"},
	{js: "/(?<!\\ud83d)\\ude00/u.exec(\"\\u{1F600}\\ude00\").index", want: "2"},
	{js: "/(?<!\\ud83d)\\ude00/.exec(\"\\u{1F600}\\ude00\").index", want: "2"},
	{js: "/(?=.)\\u{1F600}/u.exec(\"\\u{1F600}\").index", want: "0"},
	{js: "/[\\u{1F600}]{2}/u.exec(\"\\u{1F600}\\u{1F600}\").index", want: "0"},
	{js: "/\\u{1F600}{2}/u.exec(\"\\u{1F600}\\u{1F600}\").index", want: "0"},
	{js: "/(?<=\\u{1F600}{2})x/u.exec(\"\\u{1F600}\\u{1F600}x\").index", want: "4"},
	{js: "/(?<=a{1001})b/.exec(\"a\".repeat(1001) + \"b\").index", want: "1001"},
	{js: "/(?<=(a)+)b/.exec(\"aab\")", want: "[\"b\",\"a\"]{index:2,input:\"aab\",groups:undefined}"},
	{js: "/(?<=(a)(?:b|(c)))d/.exec(\"acd\")", want: "[\"d\",\"a\",\"c\"]{index:2,input:\"acd\",groups:undefined}"},
	{js: "/(?<=\\b)a/.exec(\"a\")", want: "[\"a\"]{index:0,input:\"a\",groups:undefined}"},
	{js: "/(?<=^a*)b/.exec(\"aab\")", want: "[\"b\"]{index:2,input:\"aab\",groups:undefined}"},
	{js: "/(?<=^(a*?))b/.exec(\"aab\")", want: "[\"b\",\"aa\"]{index:2,input:\"aab\",groups:undefined}"},
	{js: "\"bab\".replace(/(?<!^)b/g, \"X\")", want: "\"baX\""},
	{js: "/^(?:(a)|\\1b)+$/.exec(\"ab\")", want: "[\"ab\",undefined]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/(a)?(?:b\\1)?c/.exec(\"bc\")", want: "[\"bc\",undefined]{index:0,input:\"bc\",groups:undefined}"},
	{js: "/(?:(a)|b)\\1/.exec(\"aab\")", want: "[\"aa\",\"a\"]{index:0,input:\"aab\",groups:undefined}"},
	{js: "/(?<a>x)|(?<b>y)\\k<a>/.exec(\"y\")", want: "[\"y\",undefined,\"y\"]{index:0,input:\"y\",groups:{a:undefined,b:\"y\"}}"},
	// --- UTF-16 subjects of backtracking-only patterns (no working copy) ---
	{js: "\"\u00e9x\u00e9x\".replace(/(?<=\u00e9)x/g, \"-\")", want: "\"\u00e9-\u00e9-\""},
	{js: "\"\u00e91\u00e92\".split(/(?<=\u00e9)/)", want: "[\"\u00e9\",\"1\u00e9\",\"2\"]"},
	{js: "\"\u00e91\u00e92\".match(/(?<=\u00e9)\\d/g)", want: "[\"1\",\"2\"]"},
	{js: "{ var r = /(?<=\u00e9)x/y; r.lastIndex = 1; var m = r.exec(\"\u00e9x\u00e9x\"); r.lastIndex = 2; return [m.index, r.lastIndex, r.exec(\"\u00e9x\u00e9x\"), r.lastIndex]; }", want: "[1,2,null,0]"},
	{js: "{ var r = /(\u00e9)\\1/g; var out = []; var m; while ((m = r.exec(\"\u00e9\u00e9a\u00e9\u00e9\")) !== null) out.push(m.index, r.lastIndex); return out; }", want: "[0,2,3,5]"},
	{js: "/(?<=a)\\u{1F600}/u.exec(\"a\\u{1F600}\").index", want: "1"},
}

// compileForTest compiles pattern with flags in a fresh realm.
func compileForTest(t *testing.T, pattern, flags string) (*Realm, *compiledRegExp) {
	t.Helper()
	f, ok := parseRegExpFlags(flags)
	require.True(t, ok, "flags %q", flags)
	r := NewRealm()
	c, err := r.compileRegExp(FromGoString(pattern), f)
	require.NoError(t, err, "compile /%s/%s", pattern, flags)
	return r, c
}

// Engine selection (reExact): RE2 keeps every pattern it runs with
// JavaScript's semantics, the VM takes the rest.
func TestRegExpEngineSelection(t *testing.T) {
	exact := []string{
		`(\d+)?`, `(?:x(\d))+`, `(a|b)*c`, `(ab)+`, `^image(\[\d*\])?$`, `(?:(a)+b)*`,
		`(a+)+$`, `a{1000}`, `(?:a|b?){3}`, `^\s*(\w+)\s*=\s*(.*?)\s*$`, `\b\w+\b`,
	}
	for _, p := range exact {
		_, c := compileForTest(t, p, "")
		assert.NotNil(t, c.re, "/%s/ should run on RE2", p)
		assert.Nil(t, c.bt.Load(), "/%s/ should not build a backtracking program", p)
	}
	inexact := []string{
		`(?:(a)|b)*`, `(a*)*`, `(?:(a)?b)*`, `(?:a?)+`, `(a|)+`, `(?:(?=(\w))\w)+`,
		`a(?=b)`, `(?<=a)b`, `(?!a)`, `(?<!a)b`, `(a)\1`, `(?<n>a)\k<n>`, `a{1001}`, `(?:ab){0,1001}`,
	}
	for _, p := range inexact {
		_, c := compileForTest(t, p, "")
		assert.Nil(t, c.re, "/%s/ should run on the backtracking VM", p)
		assert.NotNil(t, c.bt.Load(), "/%s/ should have a backtracking program", p)
	}
}

// Subject-dependent fallbacks: an RE2 pattern runs a subject on the VM when
// the subject has a character for which RE2 differs from JavaScript. The
// native matcher, which reads those characters itself, runs both subjects;
// RE2 runs a subject past its cap, which the test stands for by dropping it.
func TestRegExpBacktrackSubjectFallback(t *testing.T) {
	cases := []struct {
		pattern, flags string
		plain, special string
	}{
		{`^b`, "m", "a\nb", "a\rb"},
		{`a$`, "m", "a\nb", "a b"},
		{`a$`, "m", "a\nb", "a b"},
		{`\bk`, "iu", "a k", "Kk"},
		{`k\B`, "iu", "k a", "kſ"},
		{`[^a]`, "u", "\U0001f600", "\U000f0000"},
	}
	for _, c := range cases {
		r, cr := compileForTest(t, c.pattern, c.flags)
		require.NotNil(t, cr.re, "/%s/%s", c.pattern, c.flags)
		require.NotNil(t, cr.fastPaths().nat, "/%s/%s", c.pattern, c.flags)
		var sub reSubject
		for _, s := range []string{c.plain, c.special} {
			r.initRegExpSubject(&sub, FromGoString(s), cr)
			assert.True(t, sub.nat, "/%s/%s on %q runs natively", c.pattern, c.flags, s)
		}
		cr, _ = natOracles(cr) // compiled regexps are shared: a copy without nat
		r.initRegExpSubject(&sub, FromGoString(c.plain), cr)
		assert.False(t, cr.useBT(&sub), "/%s/%s on %q stays on RE2", c.pattern, c.flags, c.plain)
		r.initRegExpSubject(&sub, FromGoString(c.special), cr)
		assert.True(t, cr.useBT(&sub), "/%s/%s on %q runs on the VM", c.pattern, c.flags, c.special)
	}
	// Without m, i+u or u those characters change nothing.
	r, cr := compileForTest(t, `^b`, "")
	cr, _ = natOracles(cr)
	var sub reSubject
	r.initRegExpSubject(&sub, FromGoString("a\rb"), cr)
	assert.False(t, cr.useBT(&sub))
}

// A catastrophic pattern on the VM is stopped by the interrupt, which the VM
// polls every btCheckInterval steps.
func TestRegExpBacktrackInterrupt(t *testing.T) {
	r, c := compileForTest(t, `(a+)+(?=$)`, "")
	require.Nil(t, c.re)
	subject := FromGoString(strings.Repeat("a", 30) + "b")
	p, err := c.btProgram()
	require.NoError(t, err)
	r.Interrupt("stop")
	in := newBTInput(subject, false)
	_, err = p.exec(r, &in, 0, false)
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "stop", ie.Value)

	rx, err := r.NewRegExp(FromGoString(`(a+)+(?=$)`), AtomEmpty)
	require.NoError(t, err)
	_, err = r.RegExpExec(rx, subject)
	require.ErrorAs(t, err, &ie)

	r.ClearInterrupt()
	m, err := r.RegExpExec(rx, FromGoString("aab"))
	require.NoError(t, err)
	assert.True(t, m.IsNull())
}

// The backtrack stack is bounded: a pattern that needs more entries raises a
// RangeError instead of growing without limit.
func TestRegExpBacktrackStackLimit(t *testing.T) {
	got := auditEvalJS(t, `{
		try { /(?:ab|ba)*(?=c)/.exec("ab".repeat(400000)); return "no error"; }
		catch (e) { return e.name + ": " + e.message; }
	}`)
	assert.Equal(t, `"RangeError: `+btStackMessage+`"`, got)
	// Below the limit the same pattern runs.
	assert.Equal(t, `true`, auditEvalJS(t, `/^(?:ab|ba)*(?=c)/.test("ab".repeat(1000) + "c")`))
}

// Backtracking programs join the process-wide pattern cache: every realm
// shares one compiled program.
func TestRegExpBacktrackProgramShared(t *testing.T) {
	defer func(limit int32) { regexpProgramsLimit = limit }(regexpProgramsLimit)
	regexpProgramsLimit = regexpProgramCount.Load() + 16
	pattern := `(?<=shared-bt-)\w+(\1)`
	_, c1 := compileForTest(t, pattern, "g")
	_, c2 := compileForTest(t, pattern, "g")
	require.Nil(t, c1.re)
	assert.Same(t, c1, c2)
	p1, err := c1.btProgram()
	require.NoError(t, err)
	p2, err := c2.btProgram()
	require.NoError(t, err)
	assert.Same(t, p1, p2)
}

// A lazily rebuilt program (an entry whose program was dropped) compiles
// from the cache key, including a wide pattern with a lone surrogate.
func TestRegExpBacktrackProgramFromKey(t *testing.T) {
	r := NewRealm()
	pat := FromUTF16([]uint16{'(', '?', '<', '=', 0xD800, ')', 'x'})
	c, err := r.compileRegExp(pat, regexpFlags{})
	require.NoError(t, err)
	require.Nil(t, c.re)
	fresh := &compiledRegExp{flags: c.flags, key: c.key}
	p, err := fresh.btProgram()
	require.NoError(t, err)
	in := newBTInput(FromUTF16([]uint16{'a', 0xD800, 'x'}), false)
	m, err := p.exec(r, &in, 0, false)
	require.NoError(t, err)
	assert.Equal(t, []int{2, 3}, m)
}

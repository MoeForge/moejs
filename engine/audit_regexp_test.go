package engine

// Audit of the RegExp implementation (engine/builtin_regexp.go,
// regexp_translate.go, regexp_simple.go, regexp_unsafe.go) plus the
// String.prototype glue in builtin_string.go. REVIEW ONLY: no production
// (non-_test.go) code is modified.
//
// TestAuditRegExpConformance is a large behaviour table cross-checked against
// node v26 on the same source text; entries whose `actual` field is set are
// cases where moejs diverges from node — the field records what moejs actually
// does so the table stays green, and each is tagged either "doc" (an accepted
// and documented difference) or "FINDING" (a real bug
// which has its own dedicated Test below that asserts the spec value and is
// wrapped in t.Skip so the build stays green; remove the Skip to reproduce).

import (
	"errors"
	resyntax "regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/MoeForge/moejs/compiler"
	"github.com/MoeForge/moejs/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditShowJS renders a JavaScript value deterministically for comparison.
const auditShowJS = `
function show(v) {
  if (v === undefined) return "undefined";
  if (v === null) return "null";
  if (typeof v === "string") return JSON.stringify(v);
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  if (Array.isArray(v)) {
    var parts = [];
    for (var i = 0; i < v.length; i++) parts.push(i in v ? show(v[i]) : "<hole>");
    var extra = [];
    if (v.index !== undefined) extra.push("index:" + show(v.index));
    if (v.input !== undefined) extra.push("input:" + show(v.input));
    if ("groups" in v) extra.push("groups:" + show(v.groups));
    if ("indices" in v) extra.push("indices:" + show(v.indices));
    return "[" + parts.join(",") + "]" + (extra.length ? "{" + extra.join(",") + "}" : "");
  }
  if (typeof v === "object") {
    var keys = Object.keys(v);
    return "{" + keys.map(function (k) { return k + ":" + show(v[k]); }).join(",") + "}";
  }
  return String(v);
}
`

// auditEvalJS evaluates js in a fresh realm and returns show(result). js is an
// expression, or a "{ ...; return x; }" block. A thrown JS error is returned
// as "throws Name: message".
func auditEvalJS(t *testing.T, js string) string {
	t.Helper()
	body := "return (" + js + ");"
	if strings.HasPrefix(js, "{") {
		body = js[1 : len(js)-1]
	}
	src := auditShowJS + "\nexport function f() { return show((function(){ " + body + " })()); }"
	m, err := syntax.ParseModule("audit.js", src, syntax.Options{})
	require.NoError(t, err, "parse %s", js)
	code, err := compiler.CompileModule(m)
	require.NoError(t, err, "compile %s", js)
	r := NewRealm()
	env, err := r.EvaluateModule(code)
	require.NoError(t, err, "evaluate %s", js)
	fn, ok := env.GetBindingValue("f")
	require.True(t, ok)
	v, err := r.Call(fn, Undefined(), nil)
	if err != nil {
		var exc *Exception
		if errors.As(err, &exc) {
			return "throws " + errorDisplayString(exc.Value)
		}
		return "throws <go> " + err.Error()
	}
	s, err := r.ToString(v)
	require.NoError(t, err)
	return s.GoString()
}

// auditJSCase is one behaviour probe. want is node/spec; actual (when set) is
// what moejs does instead.
type auditJSCase struct {
	js     string
	want   string
	actual string // "" == matches want
}

func (c auditJSCase) expected() string {
	if c.actual != "" {
		return c.actual
	}
	return c.want
}

func TestAuditRegExpConformance(t *testing.T) {
	for _, c := range auditConformanceCases {
		t.Run(c.js, func(t *testing.T) {
			assert.Equal(t, c.expected(), auditEvalJS(t, c.js), "%s", c.js)
		})
	}
}

var auditConformanceCases = []auditJSCase{
	// --- single-class fast path vs RE2, semantics ---
	{js: "\"a  b\\t\\nc\".replace(/\\s+/g, \"-\")", want: "\"a-b-c\""},
	{js: "\"a  b\\t\\nc\".replace(/(?:\\s)+/g, \"-\")", want: "\"a-b-c\""},
	{js: "\"a\u00a0b\u2028c\ufeffd\u3000e\u1680f\u2000g\u200ah\u202fi\u205fj\".replace(/\\s+/g, \"-\")", want: "\"a-b-c-d-e-f-g-h-i-j\""},
	{js: "\"a\u200bb\u180ec\".replace(/\\s+/g, \"-\")", want: "\"a\u200bb\u180ec\""},
	{js: "\"a  b\".replace(/\\S+/g, \"-\")", want: "\"-  -\""},
	{js: "\"a1b22c\".replace(/\\d+/g, \"-\")", want: "\"a-b-c\""},
	{js: "\"a1b22c\".replace(/\\D+/g, \"-\")", want: "\"-1-22-\""},
	{js: "\"a_b-c d\".replace(/\\w+/g, \"-\")", want: "\"--- -\""},
	{js: "\"a_b-c d\".replace(/\\W+/g, \"-\")", want: "\"a_b-c-d\""},
	{js: "\"\u0663x\".replace(/\\d/g, \"-\")", want: "\"\u0663x\""},
	{js: "\"\u00e9x\".replace(/\\w/g, \"-\")", want: "\"\u00e9-\""},
	{js: "\"a\\nb\\rc\u2028d\u2029e\".replace(/./g, \"-\")", want: "\"-\\n-\\r-\u2028-\u2029-\""},
	{js: "\"a\\nb\\rc\u2028d\u2029e\".replace(/./gs, \"-\")", want: "\"---------\""},
	{js: "/[^a]/u.exec(\"\U0001f600\")[0].length", want: "2"},
	{js: "/[^a]/.exec(\"\U0001f600\")[0].length", want: "1"},
	{js: "\"x\U0001f600y\".replace(/[^a]+/gu, \"-\")", want: "\"-\""},
	{js: "\"x\U0001f600y\".replace(/[^a]+/g, \"-\")", want: "\"-\""},
	{js: "\"x\U0001f600y\".replace(/[^a]/g, \"-\")", want: "\"----\""},
	{js: "\"a-b\".replace(/[ -]/g, \"_\")", want: "\"a_b\""},
	{js: "\"1-x2\".replace(/[\\d-x]+/g, \"_\")", want: "\"_\""},
	{js: "\"a-b\".replace(/[-a]+/g, \"_\")", want: "\"_b\""},
	{js: "\"a-b\".replace(/[a-]+/g, \"_\")", want: "\"_b\""},
	{js: "\"a\\bb\".replace(/[\\b]/g, \"_\")", want: "\"a_b\""},
	{js: "\"a]b\".replace(/[\\]]/g, \"_\")", want: "\"a_b\""},
	{js: "\"a\\nb\".replace(/[^]/g, \"_\")", want: "\"___\""},
	{js: "\"ab\".replace(/[]/g, \"_\")", want: "\"ab\""},
	{js: "\"ABC\".replace(/[a-z]+/i, \"_\")", want: "\"_\""},
	{js: "\"a b  c\".replace(/[^\\s]+/g, \"_\")", want: "\"_ _  _\""},
	{js: "\"aaa\".replace(/a+/g, \"-\")", want: "\"-\""},
	{js: "\"aaa\".replace(/a*/g, \"-\")", want: "\"--\""},
	{js: "\"aaa\".replace(/a?/g, \"-\")", want: "\"----\""},
	{js: "\"aaaaaa\".replace(/a{2,}/g, \"-\")", want: "\"-\""},
	{js: "\"aaaaaaa\".replace(/a{2,5}/g, \"-\")", want: "\"--\""},
	{js: "\"aaa\".replace(/a{0}/g, \"-\")", want: "\"-a-a-a-\""},
	{js: "\"aaa\".replace(/a+?/g, \"-\")", want: "\"---\""},
	{js: "\"aaa\".replace(/[a]+?/g, \"-\")", want: "\"---\""},
	{js: "\"  a  \".replace(/^\\s+/, \"\")", want: "\"a  \""},
	{js: "\"  a  \".replace(/\\s+$/, \"\")", want: "\"  a\""},
	{js: "/^\\s*$/.test(\"   \")", want: "true"},
	{js: "/^\\s*$/.test(\" a \")", want: "false"},
	{js: "\"abc\".replace(/x*/g, \"-\")", want: "\"-a-b-c-\""},
	{js: "\"aaa\".replace(/a*?/g, \"-\")", want: "\"-a-a-a-\""},
	{js: "\"\U0001f600\".replace(/(?:)/gu, \"-\")", want: "\"-\U0001f600-\""},
	{js: "\"\U0001f600\".replace(/(?:)/g, \"-\")", want: "\"-\\ud83d-\\ude00-\""},
	{js: "\"a b\".split(/\\s*/)", want: "[\"a\",\"b\"]"},
	{js: "\"abc\".split(/(?:)/)", want: "[\"a\",\"b\",\"c\"]"},
	{js: "\"\".split(/a/)", want: "[\"\"]"},
	{js: "\"\".split(/(?:)/)", want: "[]"},
	{js: "\"a1b2c3\".split(/\\d/)", want: "[\"a\",\"b\",\"c\",\"\"]"},
	{js: "\"a1b2c3\".split(/(?:\\d)/)", want: "[\"a\",\"b\",\"c\",\"\"]"},
	{js: "\"a b\".match(/\\s+/g)", want: "[\" \"]"},
	{js: "\"a b\".match(/(?:\\s)+/g)", want: "[\" \"]"},
	{js: "\"ab\".match(/\\s+/g)", want: "null"},
	{js: "\"ab\".match(/(?:\\s)+/g)", want: "null"},
	{js: "{ var r=/\\s+/g; r.lastIndex=1; var m=r.exec(\"a b c\"); return [m && m[0], m && m.index, r.lastIndex]; }", want: "[\" \",1,2]"},
	{js: "{ var r=/\\s+/g; var s=\"a b c\"; var o=[]; var m; while((m=r.exec(s))) o.push(m.index+\":\"+r.lastIndex); return o; }", want: "[\"1:2\",\"3:4\"]"},
	{js: "{ var r=/\\s+/y; r.lastIndex=1; return [r.test(\"a b\"), r.lastIndex, r.test(\"a b\"), r.lastIndex]; }", want: "[true,2,false,0]"},
	{js: "{ var r=/\\s+/y; return [r.test(\"a b\"), r.lastIndex]; }", want: "[false,0]"},
	{js: "{ var r=/\\s+/g; r.lastIndex=3; var x=\"a b c\".replace(r, \"-\"); return [x, r.lastIndex]; }", want: "[\"a-b-c\",0]"},
	{js: "{ var r=/\\s+/; r.lastIndex=3; var x=\"a b c\".replace(r, \"-\"); return [x, r.lastIndex]; }", want: "[\"a-b c\",3]"},
	{js: "{ var r=/\\s+/g; r.lastIndex=3; var x=\"a b c\".match(r); return [x, r.lastIndex]; }", want: "[[\" \",\" \"],0]"},
	{js: "{ var r=/\\s+/g; r.lastIndex=3; var x=\"a b c\".split(r); return [x, r.lastIndex]; }", want: "[[\"a\",\"b\",\"c\"],3]"},
	// FINDING (global+sticky): moejs ignores y when g is also set; see TestAuditRegExpGlobalSticky.
	{js: "{ var r=/\\s+/gy; return [\"a b c\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"a b c\",0]"},
	{js: "{ var r=/\\s+/gy; return [\" a b\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"-a b\",0]"},
	{js: "{ var r=/a/gy; return [\"aXa\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"-Xa\",0]"},
	{js: "{ var r=/a/gy; return [\"aXa\".match(r), r.lastIndex]; }", want: "[[\"a\"],0]"},
	{js: "{ var r=/a/gy; return [\"aaXa\".match(r), r.lastIndex]; }", want: "[[\"a\",\"a\"],0]"},
	{js: "{ var r=/a/gy; return [\"aaXa\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"--Xa\",0]"},
	{js: "{ var r=/a/y; return [\"aXa\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"-Xa\",1]"},
	{js: "\"a  b\".replace(/\\s+/g, function(m, off, s){ return \"<\"+m.length+\"@\"+off+\"/\"+s.length+\">\"; })", want: "\"a<2@1/4>b\""},
	{js: "\"a  b\".replace(/(?:\\s)+/g, function(m, off, s){ return \"<\"+m.length+\"@\"+off+\"/\"+s.length+\">\"; })", want: "\"a<2@1/4>b\""},
	{js: "\"a  b\".replace(/\\s+/, function(m, off, s, extra){ return \"[\"+m+\"|\"+off+\"|\"+s+\"|\"+(typeof extra)+\"]\"; })", want: "\"a[  |1|a  b|undefined]b\""},

	// --- UTF-16 subjects on RE2 ---
	{js: "\"\u00e9\U0001f600x\".replace(/x/, function(m, off){ return String(off); })", want: "\"\u00e9\U0001f6003\""},
	{js: "\"a\u00e9\".split(/\u00e9/)", want: "[\"a\",\"\"]"},
	{js: "/\u00e9/i.test(\"\u00c9\")", want: "true"},
	{js: "\"\u65e5\u672c\u8a9e\".replace(/\u672c/, \"-\")", want: "\"\u65e5-\u8a9e\""},
	{js: "{ var r=/\u672c/y; r.lastIndex=1; return [r.test(\"\u65e5\u672c\u8a9e\"), r.lastIndex]; }", want: "[true,2]"},
	{js: "{ var r=/\u672c/y; r.lastIndex=0; return [r.test(\"\u65e5\u672c\u8a9e\"), r.lastIndex]; }", want: "[false,0]"},
	{js: "{ var r=/x/g; var s=\"\u00e9\U0001f600x\u00e9x\"; var o=[]; var m; while((m=r.exec(s))) o.push(m.index+\":\"+r.lastIndex); return o; }", want: "[\"3:4\",\"5:6\"]"},
	{js: "{ var r=/x/gu; var s=\"\u00e9\U0001f600x\u00e9x\"; var o=[]; var m; while((m=r.exec(s))) o.push(m.index+\":\"+r.lastIndex); return o; }", want: "[\"3:4\",\"5:6\"]"},
	{js: "{ var r=/./gu; var s=\"a\u00e9\U0001f600\u4e2db\"; var o=[]; var m; while((m=r.exec(s))) o.push(m.index+\"-\"+r.lastIndex); return o; }", want: "[\"0-1\",\"1-2\",\"2-4\",\"4-5\",\"5-6\"]"},
	{js: "{ var r=/./g; var s=\"a\u00e9\U0001f600\u4e2db\"; var o=[]; var m; while((m=r.exec(s))) o.push(m.index+\"-\"+r.lastIndex); return o; }", want: "[\"0-1\",\"1-2\",\"2-3\",\"3-4\",\"4-5\",\"5-6\"]"},
	{js: "\"a\u00e9\U0001f600\u4e2db\".replace(/(\u4e2d)/, function(m, p1, off){ return \"[\"+off+\"]\"; })", want: "\"a\u00e9\U0001f600[4]b\""},
	{js: "\"a\u00e9\U0001f600\u4e2db\".replace(/\U0001f600/u, function(m, off){ return \"[\"+off+\"]\"; })", want: "\"a\u00e9[2]\u4e2db\""},
	{js: "\"a\u00e9\U0001f600\u4e2db\".replace(/\U0001f600/, function(m, off){ return \"[\"+off+\"]\"; })", want: "\"a\u00e9[2]\u4e2db\""},

	// --- translation exactness ---
	{js: "/\\bfoo\\b/.test(\"a foo b\")", want: "true"},
	{js: "/\\bfoo\\b/.test(\"afoob\")", want: "false"},
	{js: "/\\Bfoo/.test(\"afoo\")", want: "true"},
	{js: "/\\b/i.test(\"\u212a\")", want: "false"},
	{js: "/\\bx/i.test(\"\u017fx\")", want: "true"},
	// u+i \b sees ſ as a word char (JS canonicalises ſ->s); RE2's \b is ASCII, so such subjects run on the backtracking VM.
	{js: "/\\b\u017f/iu.test(\"\u017f\")", want: "true"},
	{js: "/\\b\u017f/i.test(\"\u017f\")", want: "false"},
	{js: "/a$/.test(\"a\\n\")", want: "false"},
	{js: "/a$/m.test(\"a\\nb\")", want: "true"},
	{js: "/^b/m.test(\"a\\nb\")", want: "true"},
	// m-flag line terminators include \r U+2028 U+2029; RE2 knows only \n, so such subjects run on the backtracking VM.
	{js: "/^b/m.test(\"a\\rb\")", want: "true"},
	{js: "/./.test(\"\\r\")", want: "false"},
	{js: "/./.test(\"\u2028\")", want: "false"},
	{js: "/./s.test(\"\\n\")", want: "true"},
	{js: "/./.test(\"\\n\")", want: "false"},
	{js: "/\\d/u.test(\"\u0663\")", want: "false"},
	{js: "/\\w/iu.test(\"\u017f\")", want: "true"},
	{js: "/\\S/.test(\" \")", want: "false"},
	{js: "/\\S/.test(\"\u00a0\")", want: "false"},
	{js: "/\\W/.test(\"_\")", want: "false"},
	{js: "/\\D/.test(\"0\")", want: "false"},
	{js: "/\\p{L}/u.test(\"\u00e9\")", want: "true"},
	{js: "/\\p{L}/u.test(\"1\")", want: "false"},
	{js: "/\\p{Script=Han}/u.test(\"\u4e2d\")", want: "true"},
	{js: "/\\p{Script=Han}/u.test(\"a\")", want: "false"},
	// Script aliases (\p{sc=Latn}) are accepted; see TestAuditRegExpScriptAlias.
	{js: "/\\p{sc=Latn}/u.test(\"a\")", want: "true"},
	{js: "/\\p{Script=Latin}/u.test(\"a\")", want: "true"},
	{js: "/\\p{Cased_Letter}/u.test(\"a\")", want: "true"},
	{js: "/\\p{LC}/u.test(\"a\")", want: "true"},
	{js: "/\\p{Lu}/iu.test(\"a\")", want: "true"},
	{js: "/\\p{Lu}/u.test(\"a\")", want: "false"},
	{js: "/\\p{Letter}/u.test(\"a\")", want: "true"},
	{js: "/\\p{L}/.test(\"p{L}\")", want: "true"},
	{js: "/\\cJ/.test(\"\\n\")", want: "true"},
	{js: "/\\x41/.test(\"A\")", want: "true"},
	{js: "/A/.test(\"A\")", want: "true"},
	{js: "/\\u{1F600}/u.test(\"\U0001f600\")", want: "true"},
	{js: "/\\u{1F600}/.test(\"u{1F600}\")", want: "true"},
	{js: "/\\u{2}/.test(\"uu\")", want: "true"},
	{js: "/\\0/.test(\"\\0\")", want: "true"},
	{js: "/\\101/.test(\"A\")", want: "true"},
	{js: "/\\a/.test(\"a\")", want: "true"},
	{js: "/[\\-]/.test(\"-\")", want: "true"},
	{js: "/a{/.test(\"a{\")", want: "true"},
	{js: "/a{2/.test(\"a{2\")", want: "true"},
	{js: "/a{2}/.test(\"aa\")", want: "true"},
	{js: "/a{2}/.test(\"a\")", want: "false"},
	{js: "/a{,2}/.test(\"a{,2}\")", want: "true"},
	{js: "/(a)|b/.exec(\"b\")", want: "[\"b\",undefined]{index:0,input:\"b\",groups:undefined}"},
	{js: "/(a)|b/.exec(\"a\")", want: "[\"a\",\"a\"]{index:0,input:\"a\",groups:undefined}"},
	{js: "/(a)|b/.exec(\"b\").groups", want: "undefined"},
	{js: "/(?<y>\\d{4})/.exec(\"2024\").groups.y", want: "\"2024\""},
	{js: "Object.getPrototypeOf(/(?<y>\\d{4})/.exec(\"2024\").groups)", want: "null"},
	{js: "/(?<y>\\d{4})-(?<m>\\d\\d)/d.exec(\"2024-05\")", want: "[\"2024-05\",\"2024\",\"05\"]{index:0,input:\"2024-05\",groups:{y:\"2024\",m:\"05\"},indices:[[0,7],[0,4],[5,7]]{groups:{y:[0,4],m:[5,7]}}}"},
	{js: "/(a)(b)?/d.exec(\"a\")", want: "[\"a\",\"a\",undefined]{index:0,input:\"a\",groups:undefined,indices:[[0,1],[0,1],undefined]{groups:undefined}}"},
	{js: "{ var r=/a/g; r.lastIndex=5; var m=r.exec(\"aaa\"); return [m, r.lastIndex]; }", want: "[null,0]"},
	{js: "{ var r=/a/g; r.lastIndex=\"2\"; var m=r.exec(\"aaa\"); return [m.index, r.lastIndex]; }", want: "[2,3]"},
	{js: "{ var r=/a/g; r.lastIndex=1.9; var m=r.exec(\"aaa\"); return [m.index, r.lastIndex]; }", want: "[1,2]"},
	{js: "{ var r=/a/g; r.lastIndex=-1; var m=r.exec(\"aaa\"); return [m.index, r.lastIndex]; }", want: "[0,1]"},
	{js: "{ var r=/a/g; Object.freeze(r); try { r.exec(\"a\"); return \"no throw\"; } catch(e) { return e.name; } }", want: "\"TypeError\""},
	{js: "{ var r=/a/y; Object.freeze(r); try { r.exec(\"a\"); return \"no throw\"; } catch(e) { return e.name; } }", want: "\"TypeError\""},
	{js: "{ var r=/a/; Object.freeze(r); try { return r.exec(\"a\")[0]; } catch(e) { return e.name; } }", want: "\"a\""},
	{js: "{ var r=/a/g; Object.freeze(r); try { r.exec(\"b\"); return \"no throw\"; } catch(e) { return e.name; } }", want: "\"TypeError\""},
	{js: "{ var r=/a/; r.lastIndex={valueOf:function(){ throw new Error(\"boom\"); }}; try { return r.exec(\"a\")[0]; } catch(e) { return e.message; } }", want: "\"boom\""},
	{js: "{ var r=/a/; r.lastIndex={valueOf:function(){ throw new Error(\"boom\"); }}; try { return \"a\".replace(r, \"b\"); } catch(e) { return e.message; } }", want: "\"boom\""},
	// .test coerces lastIndex for non-global patterns; see TestAuditRegExpTestLastIndexCoercion.
	{js: "{ var r=/a/; r.lastIndex={valueOf:function(){ throw new Error(\"boom\"); }}; try { return r.test(\"a\"); } catch(e) { return e.message; } }", want: "\"boom\""},
	{js: "{ var r=/[a]/; r.lastIndex={valueOf:function(){ throw new Error(\"boom\"); }}; try { return r.test(\"a\"); } catch(e) { return e.message; } }", want: "\"boom\""},
	{js: "new RegExp(\"/\").source", want: "\"\\\\/\""},
	{js: "new RegExp(\"\").source", want: "\"(?:)\""},
	{js: "/\\n/.source", want: "\"\\\\n\""},
	{js: "new RegExp(\"\\n\").source", want: "\"\\\\n\""},
	{js: "new RegExp(\"a\\\\/b\").source", want: "\"a\\\\/b\""},
	{js: "new RegExp(\"[/]\").source", want: "\"[/]\""},
	{js: "/a/dgimsuy.flags", want: "\"dgimsuy\""},
	{js: "new RegExp(\"a\", \"yusmigd\").flags", want: "\"dgimsuy\""},
	{js: "/a\\/b/gi.toString()", want: "\"/a\\\\/b/gi\""},
	{js: "new RegExp(/a/g, \"i\").flags", want: "\"i\""},
	{js: "new RegExp(/a/g, \"i\").source", want: "\"a\""},
	{js: "new RegExp(/a/g).flags", want: "\"g\""},
	{js: "{ var r=/a/; return RegExp(r) === r; }", want: "true"},
	{js: "{ var r=/a/; return new RegExp(r) === r; }", want: "false"},
	{js: "{ var r=/a/; return RegExp(r, \"g\") === r; }", want: "false"},
	{js: "{ try { new RegExp(\"a\", \"gg\"); } catch (e) { return e.name + \": \" + e.message; } }", want: "\"SyntaxError: Invalid flags supplied to RegExp constructor 'gg'\""},
	{js: "{ try { new RegExp(\"[\"); } catch (e) { return e.name + \": \" + e.message; } }", want: "\"SyntaxError: Invalid regular expression: /[/: Unterminated character class\""},
	{js: "new RegExp(\"a{1000}\").test(\"a\")", want: "false"},
	{js: "/(a|a)*b/.test(\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\")", want: "false"},
	{js: "/[\\s\\S]{1,1000}x/.test(\"ab\")", want: "false"},
	// Repeat counts above RE2's 1000 run on the backtracking VM as counters.
	{js: "{ try { new RegExp(\"a{1001}\"); } catch (e) { return e.name; } }", want: "undefined"},
	{js: "{ try { new RegExp(\"(a{1000}){1000}\"); return \"ok\"; } catch (e) { return e.name; } }", want: "\"ok\""},
	{js: "{ try { new RegExp(\"((a{100}){100}){100}\"); return \"ok\"; } catch (e) { return e.name; } }", want: "\"ok\""},
	// Backreferences and lookarounds run on the backtracking VM.
	{js: "{ try { new RegExp(\"(?<n>a)\\\\k<n>\"); } catch (e) { return e.name + \": \" + e.message; } }", want: "undefined"},
	{js: "{ try { new RegExp(\"(?=a)\"); } catch (e) { return e.name; } }", want: "undefined"},
	{js: "{ try { new RegExp(\"(?<=a)\"); } catch (e) { return e.name; } }", want: "undefined"},
	{js: "{ try { new RegExp(\"(a)\\\\1\"); } catch (e) { return e.name; } }", want: "undefined"},
	{js: "{ try { new RegExp(\"a(?=b)*\"); } catch (e) { return e.name; } }", want: "undefined"},
	// Quantified-group captures reset per iteration and empty iterations
	// fail (RepeatMatcher): such patterns are inexact for RE2 and run on the
	// backtracking VM; see TestAuditRegExpQuantifiedCaptureReset.
	{js: "/(?:(a)|b)*/.exec(\"ab\")", want: "[\"ab\",undefined]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/(a*)*/.exec(\"b\")", want: "[\"\",undefined]{index:0,input:\"b\",groups:undefined}"},
	{js: "/(z)((a+)?(b+)?(c))*/.exec(\"zaacbbbcac\")", want: "[\"zaacbbbcac\",\"z\",\"ac\",\"a\",undefined,\"c\"]{index:0,input:\"zaacbbbcac\",groups:undefined}"},
	{js: "/(?:(a)|(b))+/.exec(\"ab\")", want: "[\"ab\",undefined,\"b\"]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/(?:(a)|(b))+/.exec(\"ba\")", want: "[\"ba\",\"a\",undefined]{index:0,input:\"ba\",groups:undefined}"},

	// --- String glue ---
	{js: "\"abc\".replace(/b/, \"[$&]\")", want: "\"a[b]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$1]\")", want: "\"a[b]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$10]\")", want: "\"a[b0]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$01]\")", want: "\"a[b]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$00]\")", want: "\"a[$00]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$0]\")", want: "\"a[$0]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$2]\")", want: "\"a[$2]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$<x>]\")", want: "\"a[$<x>]c\""},
	{js: "\"abc\".replace(/(?<x>b)/, \"[$<x>]\")", want: "\"a[b]c\""},
	{js: "\"abc\".replace(/(?<x>b)/, \"[$<y>]\")", want: "\"a[]c\""},
	{js: "\"abc\".replace(/(?<x>b)/, \"[$<x]\")", want: "\"a[$<x]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$']\")", want: "\"a[c]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$`]\")", want: "\"a[a]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$$]\")", want: "\"a[$]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$]\")", want: "\"a[$]c\""},
	{js: "\"abc\".replace(/(b)/, \"[$x]\")", want: "\"a[$x]c\""},
	{js: "\"abc\".replace(/b/g, \"[$&]\")", want: "\"a[b]c\""},
	{js: "\"abc\".replace(/(b)/g, \"[$1$']\")", want: "\"a[bc]c\""},
	{js: "\"abcbd\".replace(/(b)(x)?/g, \"[$1$2]\")", want: "\"a[b]c[b]d\""},
	{js: "\"abcbd\".replace(/(b)(x)?/g, \"[$2]\")", want: "\"a[]c[]d\""},
	{js: "\"abcbd\".replace(/(?<n>b)(x)?/g, \"[$<n>]\")", want: "\"a[b]c[b]d\""},
	{js: "\"abc\".replace(/(b)(c)?/, function(m, p1, p2, off, str, extra){ return \"[\"+m+\"|\"+p1+\"|\"+p2+\"|\"+off+\"|\"+str+\"|\"+(typeof extra)+\"]\"; })", want: "\"a[bc|b|c|1|abc|undefined]\""},
	{js: "\"abc\".replace(/(?<n>b)(c)?/, function(m, p1, p2, off, str, groups){ return \"[\"+m+\"|\"+p1+\"|\"+p2+\"|\"+off+\"|\"+str+\"|\"+JSON.stringify(groups)+\"]\"; })", want: "\"a[bc|b|c|1|abc|{\\\"n\\\":\\\"b\\\"}]\""},
	{js: "\"a\".replace(\"a\", \"$&$&\")", want: "\"aa\""},
	{js: "\"a\".replace(\"a\", \"$'\")", want: "\"\""},
	// doc: message text differs from V8 but it is a TypeError as required.
	{js: "{ try { \"a\".replaceAll(/a/, \"b\"); } catch (e) { return e.name + \": \" + e.message; } }", want: "\"TypeError: String.prototype.replaceAll called with a non-global RegExp argument\"", actual: "\"TypeError: replaceAll must be called with a global RegExp\""},
	{js: "\"aaa\".replaceAll(/a/g, \"b\")", want: "\"bbb\""},
	{js: "\"aaa\".replaceAll(\"a\", \"b\")", want: "\"bbb\""},
	{js: "\"a1b2\".match(/\\d/g)", want: "[\"1\",\"2\"]"},
	{js: "\"ab\".match(/\\d/g)", want: "null"},
	{js: "\"a1b2\".match(/\\d/)", want: "[\"1\"]{index:1,input:\"a1b2\",groups:undefined}"},
	{js: "\"abc\".match()", want: "[\"\"]{index:0,input:\"abc\",groups:undefined}"},
	{js: "\"abc\".match(undefined)", want: "[\"\"]{index:0,input:\"abc\",groups:undefined}"},
	{js: "\"abc\".match(/(?:)/g)", want: "[\"\",\"\",\"\",\"\"]"},
	{js: "\"abc\".split(/(b)/)", want: "[\"a\",\"b\",\"c\"]"},
	{js: "\"a,b,c\".split(/,/, 2)", want: "[\"a\",\"b\"]"},
	{js: "\"a,b,c\".split(/b/, 0)", want: "[]"},
	{js: "\"axb\".split(/(x)?/)", want: "[\"a\",\"x\",\"b\"]"},
	{js: "\"ab\".split(/(x)?/)", want: "[\"a\",undefined,\"b\"]"},
	{js: "\"ab\".split(/(?:)/u)", want: "[\"a\",\"b\"]"},
	{js: "\"\U0001f600\".split(/(?:)/u)", want: "[\"\U0001f600\"]"},
	{js: "\"\U0001f600\".split(/(?:)/)", want: "[\"\\ud83d\",\"\\ude00\"]"},
	{js: "\"abc\".split(/b*/)", want: "[\"a\",\"c\"]"},
	{js: "\"abc\".split(/b*?/)", want: "[\"a\",\"b\",\"c\"]"},
	{js: "\"baaac\".replace(/a*/g, \"-\")", want: "\"-b--c-\""},
	{js: "\"baaac\".replace(/(?:a)*/g, \"-\")", want: "\"-b--c-\""},
	{js: "\"A<B>bold</B>and<CODE>coded</CODE>\".split(/<(\\/)?([^<>]+)>/)", want: "[\"A\",undefined,\"B\",\"bold\",\"/\",\"B\",\"and\",undefined,\"CODE\",\"coded\",\"/\",\"CODE\",\"\"]"},
	{js: "\"aXbXc\".split(/x/i)", want: "[\"a\",\"b\",\"c\"]"},
	{js: "\"ab\".split(/a/y)", want: "[\"\",\"b\"]"},
	{js: "\"abab\".replace(/(a)(b)/g, \"$2$1\")", want: "\"baba\""},

	// --- fail-loudly / absent features: documented gaps ---
	{js: "typeof \"a\".search", want: "\"function\""},
	{js: "{ try { return \"a\".matchAll(/a/g); } catch (e) { return e.name + \": \" + e.message; } }", want: "{}"},
	{js: "typeof RegExp.prototype.compile", want: "\"function\""},
	{js: "typeof RegExp.$1", want: "\"string\""},
	{js: "{ try { return typeof Symbol.replace; } catch (e) { return e.name; } }", want: "\"symbol\""},
	{js: "{ try { new RegExp(\"a\", \"v\"); } catch (e) { return e.name; } }", want: "undefined"},

	// --- RE2-only syntax must NOT leak ---
	{js: "{ try { new RegExp(\"(?i)a\"); } catch (e) { return e.name + \": \" + e.message; } }", want: "\"SyntaxError: Invalid regular expression: /(?i)a/: Invalid group\""},
	{js: "{ try { new RegExp(\"(?P<n>a)\"); } catch (e) { return e.name + \": \" + e.message; } }", want: "\"SyntaxError: Invalid regular expression: /(?P<n>a)/: Invalid group\""},
	{js: "{ try { new RegExp(\"(?U)a\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "/\\pL/.test(\"pL\")", want: "true"},
	{js: "/\\pL/.test(\"L\")", want: "false"},
	{js: "/\\Qa\\E/.test(\"QaE\")", want: "true"},
	{js: "/\\Aa\\z/.test(\"Aaz\")", want: "true"},
	{js: "/[[:alpha:]]/.test(\":\")", want: "false"},
	{js: "/[[:alpha:]]/.test(\":]\")", want: "true"},
	{js: "/[[:alpha:]]/.test(\"a]\")", want: "true"},
	{js: "/[[:alpha:]]/.test(\"b]\")", want: "false"},
	{js: "/[[:alpha:]]/.exec(\"x[]\")", want: "[\"[]\"]{index:1,input:\"x[]\",groups:undefined}"},
	{js: "{ try { new RegExp(\"\\\\pL\", \"u\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "{ try { new RegExp(\"\\\\Qa\\\\E\", \"u\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "{ try { new RegExp(\"[[:alpha:]]\", \"u\"); return \"ok\"; } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "{ try { new RegExp(\"a++\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "{ try { new RegExp(\"a{2}{3}\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "{ try { new RegExp(\"{2}\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "/}/.test(\"}\")", want: "true"},
	{js: "/]/.test(\"]\")", want: "true"},
	{js: "{ try { new RegExp(\"]\", \"u\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "/\\k/.test(\"k\")", want: "true"},
	{js: "{ try { new RegExp(\"\\\\k\", \"u\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "/[\\k]/.test(\"k\")", want: "true"},
	{js: "{ try { new RegExp(\"(?<a>b)[\\\\k]\"); } catch (e) { return e.name + \": \" + e.message; } }", want: "\"SyntaxError: Invalid regular expression: /(?<a>b)[\\\\k]/: Invalid escape\""},
	{js: "{ try { new RegExp(\"[\\\\k](?<a>b)\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "/(a)\\2/.test(\"a\\x02\")", want: "true"},
	{js: "/\\18/.test(\"\\x018\")", want: "true"},
	{js: "/[\\1]/.test(\"\\x01\")", want: "true"},
	{js: "/\\c1/.test(\"\\\\c1\")", want: "true"},
	{js: "/[\\c1]/.test(\"\\x11\")", want: "true"},
	{js: "/\\c/.test(\"\\\\c\")", want: "true"},
	{js: "/\\x4/.test(\"x4\")", want: "true"},
	{js: "{ try { new RegExp(\"\\\\x4\", \"u\"); } catch (e) { return e.name; } }", want: "\"SyntaxError\""},
	{js: "/\\u004/.test(\"u004\")", want: "true"},
	{js: "/[\\u{41}]/.test(\"u\")", want: "true"},
	{js: "/[\\u{41}]/u.test(\"A\")", want: "true"},

	// --- i-flag folding: Canonicalize is toUppercase without u, simple case folding with u ---
	{js: "\"\u017f\".replace(/\\w/i, \"-\")", want: "\"\u017f\""},
	{js: "\"\u017f\".replace(/[\\w]/i, \"-\")", want: "\"\u017f\""},
	{js: "\"K\".replace(/[a-z]+/i, \"_\")", want: "\"_\""}, // ASCII K matches case-insensitively (sanity)
	{js: "\"\u212a\".replace(/[a-z]/i, \"-\")", want: "\"\u212a\""},
	{js: "\"\u212a\".replace(/k/i, \"-\")", want: "\"\u212a\""},
	{js: "\"\u212a\".replace(/k/iu, \"-\")", want: "\"-\""},
}

// ---------------------------------------------------------------------------
// Findings (each Skipped; remove the Skip to reproduce)
// ---------------------------------------------------------------------------

// FINDING (high): when both g and y flags are set, String.prototype.replace and
// match ignore the sticky constraint and behave as pure-global, producing more
// matches than the spec allows (builtin_regexp.go regexpReplace/regexpMatch use
// findAll, which is not anchored at lastIndex). split correctly stays sticky.
func TestAuditRegExpGlobalSticky(t *testing.T) {
	cases := []auditJSCase{
		{js: "{ var r=/\\s+/gy; return [\"a b c\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"a b c\",0]"},
		{js: "{ var r=/a/gy; return [\"aaXa\".replace(r, function(m, i) { return i; }), r.lastIndex]; }", want: "[\"01Xa\",0]"},
		{js: "{ var r=/a|/gy; return [\"aaXa\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"---X--\",0]"},
		{js: "{ var r=/a|/gy; return [\"aaXa\".match(r), r.lastIndex]; }", want: "[[\"a\",\"a\",\"\",\"a\",\"\"],0]"},
		{js: "{ var r=/\\u00e9/gy; return [\"\\u00e9\\u00e9x\\u00e9\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"--x\u00e9\",0]"},
		{js: "{ var r=/(a)(b)?/gy; return [\"aab\".replace(r, \"[$1$2]\"), r.lastIndex]; }", want: "[\"[a][ab]\",0]"},
		{js: "{ var r=/a/gy; return \"aXa\".split(r); }", want: "[\"\",\"X\",\"\"]"},
		{js: "{ var r=/\\s+/gy; return [\" a b\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"-a b\",0]"},
		{js: "{ var r=/a/gy; return [\"aXa\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"-Xa\",0]"},
		{js: "{ var r=/a/gy; return [\"aXa\".match(r), r.lastIndex]; }", want: "[[\"a\"],0]"},
		{js: "{ var r=/a/gy; return [\"aaXa\".match(r), r.lastIndex]; }", want: "[[\"a\",\"a\"],0]"},
		{js: "{ var r=/a/gy; return [\"aaXa\".replace(r, \"-\"), r.lastIndex]; }", want: "[\"--Xa\",0]"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, auditEvalJS(t, c.js), "%s", c.js)
	}
}

// FINDING (high): the single-class fast path (regexp_simple.go) accepts, in u
// mode, a non-negated class whose range covers the surrogate area (0xD800..
// 0xDFFF) and then scans the subject per code unit, so a high/low surrogate
// half of an astral code point matches even though JavaScript in u mode treats
// the astral character as one code point outside the class. compileSimpleClass
// rejects only *negated* classes in u mode; it should also reject any class
// whose ranges include surrogate units.
func TestAuditRegExpSimpleClassUModeSurrogate(t *testing.T) {
	r := NewRealm()
	// pattern /[\u0080-\uffff]+/ built as literal backslash-u escapes
	pat := FromUTF16([]uint16{'[', '\\', 'u', '0', '0', '8', '0', '-', '\\', 'u', 'f', 'f', 'f', 'f', ']', '+'})
	subj := FromUTF16([]uint16{'a', 0xD83D, 0xDE00, 'b'}) // "a" + U+1F600 + "b"
	rx, err := r.NewRegExp(pat, FromGoString("gu"))
	require.NoError(t, err)
	d := rx.RegExpData()
	require.Nil(t, d.c.simple, "a u-mode class reaching into the surrogate area must take the RE2 path")
	res, err := regexpReplace(r, rx, d, subj, StringValue(FromGoString("-")))
	require.NoError(t, err)
	assert.Equal(t, "a\U0001f600b", res.AsString().GoString(), "the astral code point is one character outside the class")
	// Without u the class is over code units, both halves match and the
	// fast path applies.
	rx, err = r.NewRegExp(pat, FromGoString("g"))
	require.NoError(t, err)
	d = rx.RegExpData()
	require.NotNil(t, d.c.simple)
	res, err = regexpReplace(r, rx, d, subj, StringValue(FromGoString("-")))
	require.NoError(t, err)
	assert.Equal(t, "a-b", res.AsString().GoString())

	// V8 results for u and non-u classes over subjects with an astral code
	// point (E) and a lone high surrogate (H); the u-mode classes below the
	// surrogate area stay on the fast path and must agree.
	const defs = `const E = "\ud83d\ude00", H = "\ud800", e = "\u00e9"; `
	for _, c := range []struct{ js, want string }{
		{`("a" + E + "b").replace(/[\u0080-\uffff]+/gu, "-")`, `"a` + "\U0001f600" + `b"`},
		{`("a" + E + e + H + "b").replace(/[\u0080-\uffff]+/gu, "-")`, `"a` + "\U0001f600" + `-b"`},
		{`("a" + E + e + H + "b").replace(/[\u0080-\uffff]+/g, "-")`, `"a-b"`},
		{`("a" + E + e + "b").replace(/[a-z\u00e9]+/gu, "-")`, `"-` + "\U0001f600" + `-"`},
		{`("a" + E + e + "b").match(/[a-z\u00e9]+/gu)`, `["a","` + "\u00e9" + `b"]`},
		{`(E + E).replace(/[\ud83d]/gu, "-")`, `"` + "\U0001f600\U0001f600" + `"`},
		{`("a" + E + "b").match(/[\u0080-\uffff]+/gu)`, `null`},
		{`("x" + E).replace(/[\ud800-\udfff]+/gu, "-")`, `"x` + "\U0001f600" + `"`},
		{`("x" + E).replace(/[\ud800-\udfff]+/g, "-")`, `"x-"`},
		{`("a" + E + H + "b").replace(/[^a-z]+/g, "-")`, `"a-b"`},
		{`("a" + E + e + "b").replace(/[^a-z]+/gu, "-")`, `"a-b"`},
		{`("a" + E + "b").split(/[\u0080-\uffff]/u)`, `["a` + "\U0001f600" + `b"]`},
	} {
		assert.Equal(t, c.want, auditEvalJS(t, "{ "+defs+"return "+c.js+"; }"), c.js)
	}
	for _, p := range []struct {
		src, flags string
		fast       bool
	}{{`[\u0080-\uffff]+`, "gu", false}, {`[\u0080-\uffff]+`, "g", true}, {`[a-z\u00e9]+`, "gu", true}, {`[\ud800-\udfff]+`, "gu", false}, {`[\ud800-\udfff]+`, "g", false}, {`[\u0080-\ud7ff\ue000-\uffff]+`, "gu", true}, {`[^a-z]+`, "gu", false}, {`\s+`, "gu", true}} {
		rx, err := r.NewRegExp(FromGoString(p.src), FromGoString(p.flags))
		require.NoError(t, err)
		assert.Equal(t, p.fast, rx.RegExpData().c.simple != nil, "/%s/%s fast path", p.src, p.flags)
	}
}

// FINDING (high): simpleClass.find is O(n^2) for a bounded max (any class with
// no quantifier -> max==1, or {n}/{n,m}). It extends the whole contiguous run
// of matching units before truncating to max, so consecutive calls rescan the
// same run. /[a]/g or /\d/g or /\s/g on a long run of matching chars becomes
// quadratic (a linear /[a]+/g is unaffected). This is an algorithmic-complexity
// DoS in the single-class fast path.
func TestAuditRegExpSimpleClassQuadratic(t *testing.T) {
	r := NewRealm()
	const n = 200_000
	big := FromGoString(strings.Repeat("a", n))
	for _, src := range []string{"[a]", "[a]{2}", "\\w{1,3}"} {
		rx, err := r.NewRegExp(FromGoString(src), FromGoString("g"))
		require.NoError(t, err)
		d := rx.RegExpData()
		require.NotNil(t, d.c.simple)
		t0 := time.Now()
		res, err := regexpReplace(r, rx, d, big, StringValue(FromGoString("")))
		el := time.Since(t0)
		require.NoError(t, err)
		require.Equal(t, 0, res.AsString().Len())
		// The scan is linear (well under 10 ms for 200k units); the old
		// rescan of the whole run took about 20 s at this size, so the bound
		// is two orders of magnitude away from either behaviour.
		assert.Less(t, el, 2*time.Second, "/%s/g replace over %d chars took %v (quadratic)", src, n, el)
	}
	// The bounded extension yields the spec's matches: greedy up to max,
	// then the search resumes at the match end.
	for _, c := range []struct{ js, want string }{
		{`"aaaaa".replace(/[a]{2}/g, "X")`, `"XXa"`},
		{`"12345678".match(/\d{2,3}/g)`, `["123","456","78"]`},
		{`"aaaaaaa".match(/[a]{3}/g)`, `["aaa","aaa"]`},
		{`"xaaaaay".match(/[a]{2,3}/g)`, `["aaa","aa"]`},
		{`"aab aaab a".match(/[a]{2}/g)`, `["aa","aa"]`},
		{`"a".repeat(10).replace(/[a]/g, "b")`, `"bbbbbbbbbb"`},
		{`"aaaa".split(/[a]/)`, `["","","","",""]`},
		{`"aaa".replace(/[a]{4}/g, "X")`, `"aaa"`},
		{`"aaaa aaa".match(/[a]{3,}/g)`, `["aaaa","aaa"]`},
	} {
		assert.Equal(t, c.want, auditEvalJS(t, c.js), c.js)
	}
}

// FINDING (high/critical): compiledRegExp.variant() uses regexp.MustCompile on
// a program derived from the user pattern. A pattern whose base program compiles
// but whose search variant ((?s:.)/\A prefix) exceeds RE2's program-size limit
// causes MustCompile to PANIC with a plain string, which runtime.catch does NOT
// recover (it re-panics non-Exception/non-Interrupt values) -> host crash. The
// per-quantifier cap of 1000 does not prevent this: quantifiers can be chained.
// Trigger: a sticky regexp forces the anchored() variant on the first exec.
// Such a pattern runs on the backtracking VM, which has no program-size
// limit.
func TestAuditRegExpVariantCompilePanic(t *testing.T) {
	// RE2 enforces its limits in the parser on the unexpanded tree, so the
	// largest body that still parses is found by binary search without ever
	// building a program (3,355,443 instructions on go1.26).
	pattern := func(n int) string {
		return strings.Repeat("a{1000}", n/1000) + "a{" + strconv.Itoa(n%1000) + "}"
	}
	parses := func(p string) bool { _, err := resyntax.Parse(p, resyntax.Perl); return err == nil }
	limit := sort.Search(1<<23, func(n int) bool { return !parses(pattern(n)) }) - 1
	require.Greater(t, limit, 1000, "RE2 size limit not found")
	body := pattern(limit)
	tr, err := translateRegExp(utf16.Encode([]rune(body)), regexpFlags{sticky: true})
	require.NoError(t, err)
	require.True(t, parses(tr.body), "the base program is within RE2's limit")
	require.False(t, parses(variantOpenLargest+tr.body+`)`), "the search variant is over RE2's limit: the audit case")

	r := NewRealm()
	var rec any
	rx, err := func() (v *Object, err error) {
		defer func() { rec = recover() }()
		return r.NewRegExp(FromGoString(body), FromGoString("y"))
	}()
	require.Nil(t, rec, "construction panicked: %v", rec)
	require.NoError(t, err)
	require.Nil(t, rx.RegExpData().c.re, "a pattern whose search variant cannot compile runs on the backtracking VM")
	for _, c := range []struct {
		subject string
		match   bool
	}{{"a", false}, {strings.Repeat("a", limit), true}} {
		m, err := r.RegExpExec(rx, FromGoString(c.subject))
		require.NoError(t, err)
		require.Equal(t, c.match, !m.IsNull(), "sticky exec on %d a's", len(c.subject))
	}

	// The same shape a few units smaller runs on RE2, and every search
	// variant (anchored, context, context+anchored) compiles lazily and
	// matches.
	small := strings.Repeat("a{1000}", 3) + "a{5}"
	rx, err = r.NewRegExp(FromGoString(small), FromGoString("y"))
	require.NoError(t, err)
	require.NotNil(t, rx.RegExpData().c.re)
	subject := FromGoString("b" + strings.Repeat("a", 3005))
	for _, lastIndex := range []int{0, 1} {
		require.NoError(t, r.setRegExpLastIndex(rx, lastIndex))
		m, err := r.RegExpExec(rx, subject)
		require.NoError(t, err)
		require.Equal(t, lastIndex == 1, !m.IsNull(), "sticky exec at lastIndex %d", lastIndex)
	}
	got := auditEvalJS(t, `{ const s = "b" + "a".repeat(3005); return [s.replace(/`+small+`/g, "X"), s.match(/`+small+`/).index, /^`+small+`$/y.test(s.slice(1))]; }`)
	require.Equal(t, `["bX",1,true]`, got)
}

// FINDING (low/medium): RegExp.prototype.test on a non-global, non-sticky
// regexp takes a fast path that never reads lastIndex, so it skips the spec's
// ToLength(Get(R,"lastIndex")) coercion (RegExpBuiltinExec step). A side-
// effecting/throwing lastIndex getter is therefore not observed by test()
// (exec and replace do observe it).
func TestAuditRegExpTestLastIndexCoercion(t *testing.T) {
	for _, js := range []string{
		"{ var r=/a/; r.lastIndex={valueOf:function(){ throw new Error(\"boom\"); }}; try { return r.test(\"a\"); } catch(e) { return e.message; } }",
		"{ var r=/[a]/; r.lastIndex={valueOf:function(){ throw new Error(\"boom\"); }}; try { return r.test(\"a\"); } catch(e) { return e.message; } }",
	} {
		assert.Equal(t, "\"boom\"", auditEvalJS(t, js), "%s", js)
	}
	// The coercion is observable but the value is ignored and never written.
	for _, c := range []auditJSCase{
		{js: "{ var n=0, r=/a/; r.lastIndex={valueOf:function(){ n++; return 5; }}; var ok=r.test(\"a\"); return [ok, n, typeof r.lastIndex]; }", want: "[true,1,\"object\"]"},
		{js: "{ var r=/[a]/; r.lastIndex=7; return [r.test(\"a\"), r.lastIndex]; }", want: "[true,7]"},
	} {
		assert.Equal(t, c.want, auditEvalJS(t, c.js), "%s", c.js)
	}
}

// FINDING (medium): script property escapes only accept Go's script names
// (which match the canonical Unicode long names); the ISO-15924 aliases that
// ECMAScript also accepts (Latn, Grek, Cyrl, ...) are rejected as "Invalid
// property name" (regexp_translate.go unicodeProperty checks only
// unicode.Scripts[value]). \p{Script=Latin} works; \p{sc=Latn} throws.
func TestAuditRegExpScriptAlias(t *testing.T) {
	for _, js := range []string{
		"/\\p{sc=Latn}/u.test(\"a\")",
		"/\\p{Script=Latn}/u.test(\"a\")",
		"/\\p{Script=Grek}/u.test(\"\u03b1\")",
		"/\\p{sc=Cyrl}/u.test(\"\u0434\")",
		"/\\p{sc=Hani}/u.test(\"\u4e2d\")",
		"/\\P{sc=Latn}/u.test(\"\u03b1\")",
		"/\\p{sc=Qaai}/u.test(\"\u0301\")",
		"/\\p{sc=Zyyy}/u.test(\"1\")",
		"/[\\p{sc=Latn}\\p{sc=Grek}]+/u.test(\"a\u03b1\")",
	} {
		assert.Equal(t, "true", auditEvalJS(t, js), "%s", js)
	}
	for _, js := range []string{
		"/\\p{sc=Latn}/u.test(\"\u03b1\")",
		"/\\p{sc=Grek}/u.test(\"a\")",
	} {
		assert.Equal(t, "false", auditEvalJS(t, js), "%s", js)
	}
	// Unknown values are still SyntaxErrors, as are bare short names.
	for _, js := range []string{
		"{ try { new RegExp(\"\\\\p{sc=Xxxx}\", \"u\"); return \"ok\"; } catch (e) { return e.name; } }",
		"{ try { new RegExp(\"\\\\p{Latn}\", \"u\"); return \"ok\"; } catch (e) { return e.name; } }",
		"{ try { new RegExp(\"\\\\p{sc=latn}\", \"u\"); return \"ok\"; } catch (e) { return e.name; } }",
	} {
		assert.Equal(t, "\"SyntaxError\"", auditEvalJS(t, js), "%s", js)
	}
}

// FINDING (medium): captures inside a quantified group are not reset to
// undefined on iterations that do not take that branch (RE2 semantics), while
// ECMAScript clears them. This produces stale, non-undefined capture values.
// Inherent to RE2.
func TestAuditRegExpQuantifiedCaptureReset(t *testing.T) {
	// Such patterns are inexact for RE2 (reExact) and run on the
	// backtracking VM.
	cases := []auditJSCase{
		{js: "/(?:(a)|b)*/.exec(\"ab\")", want: "[\"ab\",undefined]{index:0,input:\"ab\",groups:undefined}"},
		{js: "/(a*)*/.exec(\"b\")", want: "[\"\",undefined]{index:0,input:\"b\",groups:undefined}"},
		{js: "/(?:(a)|(b))+/.exec(\"ab\")", want: "[\"ab\",undefined,\"b\"]{index:0,input:\"ab\",groups:undefined}"},
		{js: "/(?:(a)|(b))+/.exec(\"ba\")", want: "[\"ba\",\"a\",undefined]{index:0,input:\"ba\",groups:undefined}"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, auditEvalJS(t, c.js), "%s", c.js)
	}
}

// FINDING (low): escapeRegExpPattern (RegExp.prototype.source) special-cases a
// backslash by emitting it plus the *raw* following character, so a backslash
// immediately followed by a line terminator emits the raw terminator instead of
// escaping it, breaking the round-trip guarantee that "/"+source+"/"+flags
// parses back. new RegExp("\\\n").source should be `\n` (backslash n).
func TestAuditRegExpSourceEscapeBackslashLineTerminator(t *testing.T) {
	r := NewRealm()
	pat := FromUTF16([]uint16{'\\', '\n'}) // backslash + LF
	rx, err := r.NewRegExp(pat, AtomEmpty)
	require.NoError(t, err)
	src := escapeRegExpPattern(rx.RegExpData().Source())
	assert.Equal(t, `\n`, src.GoString(), "source must escape the line terminator, got %q", src.GoString())
	for _, c := range []struct {
		units []uint16
		want  string
	}{
		{[]uint16{'\\', '\r'}, `\r`},
		{[]uint16{'\\', 0x2028}, `\u2028`},
		{[]uint16{'a', '\\', '\n', 'b'}, `a\nb`},
		{[]uint16{'\\', '/'}, `\/`},                      // an escaped slash stays as written
		{[]uint16{'\\', '\\', '\n'}, `\\\n`},             // an escaped backslash, then a bare terminator
		{[]uint16{'\\'}, `\`},                            // a trailing backslash is copied
		{[]uint16{'a', 0x2028, 0x2029}, `a\u2028\u2029`}, // bare U+2028/2029 were emitted raw before
	} {
		got := escapeRegExpPattern(FromUTF16(c.units)).GoString()
		assert.Equal(t, c.want, got, "%v", c.units)
	}
	// The escaped source round-trips through the RegExp constructor.
	assert.Equal(t, "true", auditEvalJS(t, `{ const r = new RegExp("\\\n"); const r2 = new RegExp(r.source); return r2.test("\n") && r.source === r2.source; }`))
	assert.Equal(t, "true", auditEvalJS(t, `{ const r = new RegExp("\u2028"); return r.source === "\\u2028" && new RegExp(r.source).test("\u2028"); }`))
}

// FINDING (medium): a pending interrupt set before a global replace/match on a
// large subject is not observed: for a pattern that cannot match the empty
// string, findAll delegates to Go's FindAllSubmatchIndex in one uninterruptible
// call, and the literal-template ASCII replace (regexpReplaceFast) is a single
// ReplaceAll call. split, simpleReplace and empty-matchable findAll do check.
func TestAuditRegExpInterruptIgnored(t *testing.T) {
	big := FromGoString(strings.Repeat("a", 2_000_000))
	bigU := FromGoString(strings.Repeat("a\u00e9", 1_000_000))
	check := func(name, pattern string, subject *String, run func(r *Realm, rx *Object, d *RegExpData, s *String) error) {
		r := NewRealm()
		rx, err := r.NewRegExp(FromGoString(pattern), FromGoString("g"))
		require.NoError(t, err)
		d := rx.RegExpData()
		r.Interrupt("stop")
		err = run(r, rx, d, subject)
		var ie *InterruptedError
		assert.True(t, errors.As(err, &ie), "%s /%s/g: expected InterruptedError, got %v", name, pattern, err)
	}
	replace := func(r *Realm, rx *Object, d *RegExpData, s *String) error {
		_, err := regexpReplace(r, rx, d, s, StringValue(FromGoString("b")))
		return err
	}
	replaceFn := func(r *Realm, rx *Object, d *RegExpData, s *String) error {
		fn, _ := r.FromGo(NativeFunc(func(*Realm, Value, []Value) (Value, error) { return StringValue(AtomEmpty), nil }))
		_, err := regexpReplace(r, rx, d, s, fn)
		return err
	}
	match := func(r *Realm, rx *Object, d *RegExpData, s *String) error {
		_, err := regexpMatch(r, rx, d, s)
		return err
	}
	// Literal, left-context (\b) and empty-matchable patterns, ASCII and
	// UTF-16 subjects, template and functional replacers.
	for _, pattern := range []string{"a", "\\ba", "a|"} {
		check("replace", pattern, big, replace)
		check("replace", pattern, bigU, replace)
		check("replace(fn)", pattern, big, replaceFn)
		check("match", pattern, big, match)
		check("match", pattern, bigU, match)
	}
}

// ---------------------------------------------------------------------------
// Non-finding verifications (these pass)
// ---------------------------------------------------------------------------

// The process-wide compiled-program cache is shared across realms and read/
// written concurrently. This exercises 16 goroutines in 16 realms compiling a
// mix of shared and distinct patterns (and running searches that lazily compile
// the anchored/context variants) so `go test -race` can check the sync.Map and
// the atomic variant pointers. Expected to pass (no data race, no error).
func TestAuditRegExpSharedCacheRace(t *testing.T) {
	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := NewRealm()
			for i := 0; i < 200; i++ {
				// shared across goroutines: forces concurrent Load of the same entry
				shared := "shared" + string(rune('a'+i%8)) + `\d+`
				// distinct per goroutine+iteration: forces concurrent Store
				distinct := "g" + string(rune('A'+g)) + "n" + strings.Repeat("x", i%5) + `[0-9]+`
				for _, p := range []string{shared, distinct} {
					rx, err := r.NewRegExp(FromGoString(p), FromGoString("gy"))
					if err != nil {
						errs[g] = err
						return
					}
					d := rx.RegExpData()
					// exec from a non-zero lastIndex triggers lazy variant compiles.
					_ = r.setRegExpLastIndex(rx, 1)
					var sub reSubject
					r.initRegExpSubject(&sub, FromGoString("ab123 cd456"), d.c)
					if _, err := r.regexpBuiltinExec(rx, d, FromGoString("ab123 cd456"), &sub); err != nil {
						errs[g] = err
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	for g, err := range errs {
		require.NoErrorf(t, err, "goroutine %d", g)
	}
}

// The process-wide cache is bounded by regexpProgramsLimit and stops inserting
// once full (rather than growing without limit). Insert many distinct patterns
// and confirm the counter never exceeds the limit. (Entries are permanent — no
// eviction — which is a separate, documented note in the report.)
func TestAuditRegExpSharedCacheBounded(t *testing.T) {
	r := NewRealm()
	for i := 0; i < int(regexpProgramsLimit)+500; i++ {
		p := "audituniq" + auditItoa(i) + `x`
		if _, err := r.NewRegExp(FromGoString(p), AtomEmpty); err != nil {
			t.Fatalf("compile %q: %v", p, err)
		}
	}
	assert.LessOrEqual(t, regexpProgramCount.Load(), regexpProgramsLimit,
		"process-wide program count must stay within the bound")
}

// A function replacer that throws mid-way leaves consistent state: the
// exception propagates, no partial result is observable, and lastIndex is 0
// (set at the start of the global replace).
func TestAuditRegExpThrowingReplacerConsistency(t *testing.T) {
	js := `{
		var calls = 0;
		var r = /a/g;
		var err = null;
		try {
			"aaaa".replace(r, function(m, off){ calls++; if (calls === 3) throw new Error("stop"); return "-"; });
		} catch (e) { err = e.message; }
		return [err, calls, r.lastIndex];
	}`
	assert.Equal(t, "[\"stop\",3,0]", auditEvalJS(t, js))
}

// auditItoa is a tiny local int->string to avoid importing strconv only for the
// bounded-cache test's unique keys.
func auditItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

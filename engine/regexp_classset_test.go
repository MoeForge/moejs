package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The v flag (internal/regexpsyntax/classset.go). Expected values are node's (V8) unless a
// comment says the spec differs.
func TestRegExpUnicodeSets(t *testing.T) {
	for _, c := range regexpVFlagCases {
		t.Run(c.js, func(t *testing.T) {
			assert.Equal(t, c.expected(), auditEvalJS(t, c.js), "%s", c.js)
		})
	}
}

var regexpVFlagCases = []auditJSCase{
	{js: "/[\\p{L}--[a-z]]/v.test(\"a\")", want: "false"},
	{js: "/[\\p{L}--[a-z]]/v.test(\"A\")", want: "true"},
	{js: "/[\\p{L}&&\\p{Script=Greek}]+/v.exec(\"abc\u03b1\u03b2\u03b3\")", want: "[\"\u03b1\u03b2\u03b3\"]{index:3,input:\"abc\u03b1\u03b2\u03b3\",groups:undefined}"},
	{js: "/[[a-z]--[aeiou]]+/v.exec(\"aebcdi\")", want: "[\"bcd\"]{index:2,input:\"aebcdi\",groups:undefined}"},
	{js: "/[\\w--\\d]+/v.exec(\"12ab_3\")", want: "[\"ab_\"]{index:2,input:\"12ab_3\",groups:undefined}"},
	{js: "/[\\d--[0-4]]+/v.exec(\"0123456789\")", want: "[\"56789\"]{index:5,input:\"0123456789\",groups:undefined}"},
	{js: "/[[[a-c]--b]x]+/v.exec(\"acxb\")", want: "[\"acx\"]{index:0,input:\"acxb\",groups:undefined}"},
	{js: "/[[^a]&&[a-c]]+/v.exec(\"abcb\")", want: "[\"bcb\"]{index:1,input:\"abcb\",groups:undefined}"},
	{js: "/[^[^a]]/v.exec(\"ba\")", want: "[\"a\"]{index:1,input:\"ba\",groups:undefined}"},
	{js: "/[^\\d]/v.exec(\"12a\")", want: "[\"a\"]{index:2,input:\"12a\",groups:undefined}"},
	{js: "/[a&&b&&c]/v.exec(\"abc\")", want: "null"},
	{js: "/[[a-z]&&[a-c]&&[b-d]]+/v.exec(\"abcd\")", want: "[\"bc\"]{index:1,input:\"abcd\",groups:undefined}"},
	{js: "/[[a-z]--[a-c]--[x-z]]+/v.exec(\"abdwxz\")", want: "[\"dw\"]{index:2,input:\"abdwxz\",groups:undefined}"},
	{js: "/[\\q{abc|ab|a}]/v.exec(\"abcd\")", want: "[\"abc\"]{index:0,input:\"abcd\",groups:undefined}"},
	{js: "/[\\q{abc|ab|a}x]+/v.exec(\"xabcaab\")", want: "[\"xabcaab\"]{index:0,input:\"xabcaab\",groups:undefined}"},
	{js: "/^[\\q{abc|ab}]$/v.test(\"ab\")", want: "true"},
	{js: "/[\\q{}]/v.exec(\"x\")", want: "[\"\"]{index:0,input:\"x\",groups:undefined}"},
	{js: "/[\\q{}a]/v.exec(\"b\")", want: "[\"\"]{index:0,input:\"b\",groups:undefined}"},
	{js: "/[\\q{ab}\\q{cd}]/v.exec(\"xcd\")", want: "[\"cd\"]{index:1,input:\"xcd\",groups:undefined}"},
	{js: "/[\\q{ab|ab}ab]/v.exec(\"ab\")", want: "[\"ab\"]{index:0,input:\"ab\",groups:undefined}"},
	{js: "/[\\q{a\\|b}]/v.exec(\"a|b\")", want: "[\"a|b\"]{index:0,input:\"a|b\",groups:undefined}"},
	{js: "/^[\\q{ab|a}]+$/v.test(\"aab\")", want: "true"},
	{js: "/^[\\q{ab|a}]{2}b$/v.test(\"aab\")", want: "true"},
	{js: "/^(?:[\\q{abc|ab}])c$/v.test(\"abc\")", want: "true"},
	{js: "/[\\q{}]*/v.exec(\"a\")", want: "[\"\"]{index:0,input:\"a\",groups:undefined}"},
	{js: "/[\\q{|a}]+/v.exec(\"aa\")", want: "[\"aa\"]{index:0,input:\"aa\",groups:undefined}"},
	{js: "/(?<=[\\q{ab|a}])c/v.exec(\"abc\").index", want: "2"},
	{js: "/(?<=[\\q{ab|b}])c/v.exec(\"abc\").index", want: "2"},
	{js: "/(?<=^[\\q{ab|a}])c/v.exec(\"abc\")", want: "[\"c\"]{index:2,input:\"abc\",groups:undefined}"},
	{js: "/(?<![\\q{xb}])c/v.exec(\"abcxbc\").index", want: "2"},
	{js: "\"ab-abc-a\".replace(/[\\q{abc|ab}]/gv, \"X\")", want: "\"X-X-a\""},
	{js: "/\\p{RGI_Emoji}/v.exec(\"x\U0001f468\u200d\U0001f469\u200d\U0001f467y\")[0].length", want: "8"},
	{js: "/^\\p{RGI_Emoji}$/v.test(\"\U0001f1ef\U0001f1f5\")", want: "true"},
	{js: "/^\\p{RGI_Emoji_Flag_Sequence}+$/v.test(\"\U0001f1ef\U0001f1f5\U0001f1fa\U0001f1f8\")", want: "true"},
	{js: "/^\\p{Emoji_Keycap_Sequence}$/v.test(\"1\ufe0f\u20e3\")", want: "true"},
	{js: "/^\\p{RGI_Emoji_Modifier_Sequence}$/v.test(\"\U0001f44d\U0001f3fd\")", want: "true"},
	{js: "/^\\p{RGI_Emoji_Tag_Sequence}$/v.test(\"\U0001f3f4\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f\")", want: "true"},
	{js: "/^\\p{RGI_Emoji_ZWJ_Sequence}$/v.test(\"\U0001f468\u200d\U0001f469\u200d\U0001f467\")", want: "true"},
	{js: "/^[\\p{RGI_Emoji}--\\q{\U0001f1ef\U0001f1f5}]$/v.test(\"\U0001f1ef\U0001f1f5\")", want: "false"},
	{js: "/^[\\p{RGI_Emoji}--\\q{\U0001f1ef\U0001f1f5}]$/v.test(\"\U0001f1fa\U0001f1f8\")", want: "true"},
	{js: "/[\\p{RGI_Emoji_Flag_Sequence}&&\\q{\U0001f1ef\U0001f1f5|x}]/v.exec(\"x\U0001f1ef\U0001f1f5\")", want: "[\"\U0001f1ef\U0001f1f5\"]{index:1,input:\"x\U0001f1ef\U0001f1f5\",groups:undefined}"},
	{js: "/\\p{Basic_Emoji}/v.exec(\"a\U0001f600\")", want: "[\"\U0001f600\"]{index:1,input:\"a\U0001f600\",groups:undefined}"},
	{js: "/[\\p{RGI_Emoji}&&\\p{ASCII}]+/v.exec(\"a#1\")", want: "null"},
	{js: "\"\U0001f44d\U0001f3fdx\U0001f600\".match(/\\p{RGI_Emoji}/gv)", want: "[\"\U0001f44d\U0001f3fd\",\"\U0001f600\"]"},
	{js: "/^\\p{RGI_Emoji}$/v.test(\"\U0001f44d\")", want: "true"},
	{js: "/[\\q{AbC}]/vi.exec(\"xaBc\")", want: "[\"aBc\"]{index:1,input:\"xaBc\",groups:undefined}"},
	{js: "/[\\q{ab}]/vi.exec(\"AB\")", want: "[\"AB\"]{index:0,input:\"AB\",groups:undefined}"},
	{js: "/\\P{Ll}/vi.test(\"a\")", want: "false"},
	{js: "/\\P{Ll}/vi.test(\"A\")", want: "false"},
	{js: "/\\P{Ll}/ui.test(\"a\")", want: "true"},
	{js: "/\\P{Ll}/ui.test(\"A\")", want: "true"},
	{js: "/[\\P{Ll}]/vi.test(\"A\")", want: "false"},
	{js: "/[^\\P{Ll}]/vi.test(\"A\")", want: "true"},
	{js: "/[^\\P{Ll}]/ui.test(\"A\")", want: "false"},
	{js: "/\\p{Lu}/vi.test(\"a\")", want: "true"},
	{js: "/[\\p{Lu}--[A-Z]]/vi.test(\"a\")", want: "false"},
	{js: "/[\\p{Lu}--[A-Z]]/v.test(\"\u00c0\")", want: "true"},
	{js: "/[\\p{Lu}--[A-Z]]/vi.test(\"\u00e0\")", want: "true"},
	{js: "/[\\p{Lu}--[a-z]]/vi.test(\"B\")", want: "false"},
	{js: "/[\\W]/vi.test(\"S\")", want: "false"},
	{js: "/\\W/vi.test(\"\u017f\")", want: "false"},
	{js: "/\\W/ui.test(\"\u017f\")", want: "false"},
	{js: "/[^\\W]/vi.test(\"\u017f\")", want: "true"},
	{js: "/[\\w&&\\W]/vi.exec(\"a\")", want: "null"},
	{js: "/[[a-z]&&[^k]]/vi.test(\"K\")", want: "false"},
	{js: "/[[a-z]&&[^k]]/vi.test(\"\u212a\")", want: "false"},
	{js: "/[[a-z]--k]/vi.test(\"K\")", want: "false"},
	{js: "/[[a-z]--k]/vi.test(\"\u212a\")", want: "false"},
	// Spec: scf(U+1E9E) is U+00DF, so the class matches (V8 misses the one-character \q case).
	{js: "/[\\q{\u00df}]/vi.test(\"\u1e9e\")", want: "true"},
	{js: "/\u00df/vi.test(\"\u1e9e\")", want: "true"},
	{js: "/[^\\q{a}]/v.test(\"a\")", want: "false"},
	// Spec: \q{a} is folded and complemented among the fold's fixed points,
	// so "A" (scf "a") is excluded like with [^a] (V8 skips the fold).
	{js: "/[^\\q{a}]/vi.test(\"A\")", want: "false"},
	{js: "/[^a]/vi.test(\"A\")", want: "false"},
	{js: "/[^a]/ui.test(\"A\")", want: "false"},
	{js: "/\\D/vi.test(\"a\")", want: "true"},
	{js: "/[^\\S]/vi.exec(\" a\")", want: "[\" \"]{index:0,input:\" a\",groups:undefined}"},
	{js: "/[\\&\\-\\!]+/v.exec(\"a&-!\")", want: "[\"&-!\"]{index:1,input:\"a&-!\",groups:undefined}"},
	{js: "/[&]/v.test(\"&\")", want: "true"},
	{js: "/[a&b]+/v.exec(\"a&b\")", want: "[\"a&b\"]{index:0,input:\"a&b\",groups:undefined}"},
	{js: "/[\\b]/v.test(\"\\b\")", want: "true"},
	{js: "/[\\u{1F600}-\\u{1F602}]/v.test(\"\U0001f601\")", want: "true"},
	{js: "/[\\uD83D\\uDE00]/v.test(\"\U0001f600\")", want: "true"},
	{js: "/[\\uD83D\\uDE00]/v.test(\"\\uD83D\")", want: "false"},
	{js: "/./v.exec(\"\U0001f600\")[0].length", want: "2"},
	{js: "/[^a]/v.exec(\"\U0001f600\")[0].length", want: "2"},
	{js: "/[\\q{\\uD83D}]/v.exec(\"\U0001f600\")", want: "null"},
	{js: "/[\\q{\\uD83D}]/v.exec(\"\\uD83Dx\")", want: "[\"\\ud83d\"]{index:0,input:\"\\ud83dx\",groups:undefined}"},
	{js: "/\\p{Lu}/v.exec(\"aB\")", want: "[\"B\"]{index:1,input:\"aB\",groups:undefined}"},
	{js: "/[\\p{Lu}\\p{Nd}]+/v.exec(\"aB1c\")", want: "[\"B1\"]{index:1,input:\"aB1c\",groups:undefined}"},
	{js: "/[^\\p{Lu}]/v.exec(\"AbC\")", want: "[\"b\"]{index:1,input:\"AbC\",groups:undefined}"},
	{js: "/[\\s--\\n]+/v.exec(\"\\n \\t\\n\")", want: "[\" \\t\"]{index:1,input:\"\\n \\t\\n\",groups:undefined}"},
	{js: "/a/v.flags", want: "\"v\""},
	{js: "/a/v.unicode", want: "false"},
	{js: "/a/v.unicodeSets", want: "true"},
	{js: "/a/u.unicodeSets", want: "false"},
	{js: "/a/.unicodeSets", want: "false"},
	{js: "new RegExp(\"a\", \"vgi\").flags", want: "\"giv\""},
	{js: "/a/dgimsvy.flags", want: "\"dgimsvy\""},
	{js: "/a/v.toString()", want: "\"/a/v\""},
	{js: "String(new RegExp(\"[a&&b]\", \"v\"))", want: "\"/[a&&b]/v\""},
	{js: "RegExp.prototype.unicodeSets", want: "undefined"},
	{js: "Object.getOwnPropertyDescriptor(RegExp.prototype, \"unicodeSets\").get.call(/a/v)", want: "true"},
	{js: "Object.getOwnPropertyDescriptor(RegExp.prototype, \"unicodeSets\").get.name", want: "\"get unicodeSets\""},
	{js: "Object.getOwnPropertyDescriptor(RegExp.prototype, \"unicode\").get.call(/a/v)", want: "false"},
	{js: "\"\U0001f600x\".split(/(?:)/v)", want: "[\"\U0001f600\",\"x\"]"},
	{js: "\"\U0001f600\U0001f600\".replace(/(?:)/gv, \"-\")", want: "\"-\U0001f600-\U0001f600-\""},
	{js: "\"\U0001f600\U0001f600\".match(/(?:)/gv).length", want: "3"},
	{js: "\"a\U0001f600b\".match(/./gv).length", want: "3"},
	// Spec: index and match start at lastIndex inside the pair (V8: [0,0]).
	{js: "{ var r = /(?:)/gv; r.lastIndex = 1; var m = r.exec(\"\U0001f600\"); return [m.index, r.lastIndex]; }", want: "[1,1]"},
	{js: "new RegExp(\"[\\\\q{ab}]\", \"gv\").exec(\"xab\").index", want: "1"},
	{js: "/[\\p{ASCII}--\\p{L}--\\p{N}]+/v.exec(\"ab!?#1\")", want: "[\"!?#\"]{index:2,input:\"ab!?#1\",groups:undefined}"},
	{js: "/[[\\p{L}--[a-z]]&&\\p{ASCII}]+/v.exec(\"abCD\u00e9\")", want: "[\"CD\"]{index:2,input:\"abCD\u00e9\",groups:undefined}"},
}

// TestRegExpUnicodeSetsSyntax checks the early errors of the v flag's class
// syntax against node: reserved and syntax characters, mixed operators,
// ranges in set operations and MayContainStrings under negation.
func TestRegExpUnicodeSetsSyntax(t *testing.T) {
	invalid := []string{
		`[a-z&&b]`, `[a&&b--c]`, `[a--b&&c]`, `[ab--c]`, `[a&&&b]`, `[&&a]`,
		`[a&&]`, `[a--]`, `[(]`, `[)]`, `[{]`, `[}]`, `[/]`, `[|]`, `[-]`, `[a-]`,
		`[-a]`, `[!!]`, `[a!!b]`, `[a##b]`, `[a^^]`, `[^\p{RGI_Emoji}]`,
		`[^\q{ab}]`, `[^\q{}]`, `[^\q{a|}]`, `[^[\p{RGI_Emoji}]]`, `[^[a\q{ab}]]`,
		`[^[\p{RGI_Emoji}--\q{ab}]]`, `\P{RGI_Emoji}`, `[\P{RGI_Emoji}]`, `[z-a]`,
		`[\d-a]`, `[a-\d]`, `[a-[b]]`, `\q{a}`, `[\q{a]`, `[\q{a}`, `[\q{(}]`,
		`[\q{\d}]`, `[\k]`, `[\1]`, `[\c1]`, `[\z]`, `[`, `[[a]`, `[a]]`, `[a&&[b]`,
		`\p{Basic_Emoji=Yes}`, `[\p{RGI_Emoji}&&\q{a}--b]`, `[a-b--c]`, `[\q]`,
		`[\q{a}-b]`, `[a-z--[aeiou]]`,
	}
	for _, p := range invalid {
		_, err := parseRegExp(FromGoString(p).UTF16(), regexpFlags{unicode: true, unicodeSets: true})
		assert.Error(t, err, "%s", p)
	}
	valid := []string{
		`[^\q{a}]`, `[^[\q{a|b}]]`, `[^[\p{RGI_Emoji}&&\p{ASCII}]]`,
		`[^\p{RGI_Emoji}&&\p{ASCII}]`, `[\p{RGI_Emoji}&&\q{ab}]`, `[&]`, `[a&b]`,
		`[!]`, `[\&&\&]`, `[\-]`, `[\q{}]`, `[]`, `[^]`, `[a-z\d]`, `[[a-z][0-9]]`,
		`[^[^\p{RGI_Emoji}&&\p{ASCII}]]`, `[\b]`, `[\q{a\|b}]`, `[\w--\d]`,
		`[[a]--[b]--[c]]`, `[a&&b&&c]`, `[\q{abc|ab|a}]`, `\p{RGI_Emoji}`, `[\^^]`,
		`[^^]`, `[$]`, `[a$]`, `[.]`, `[\p{Lu}]`, `[\P{Lu}--a]`, `\P{Lu}`,
		`[\q{\u{1F600}}]`, `[^[\q{\u{1F600}}]]`, `[\u{1F600}-\u{1F602}]`,
	}
	for _, p := range valid {
		_, err := parseRegExp(FromGoString(p).UTF16(), regexpFlags{unicode: true, unicodeSets: true})
		assert.NoError(t, err, "%s", p)
	}
	for _, f := range []string{"uv", "vu", "vv", "gvu"} {
		_, ok := parseRegExpFlags(f)
		assert.False(t, ok, f)
	}
	fl, ok := parseRegExpFlags("yvd")
	assert.True(t, ok)
	assert.Equal(t, "dvy", fl.String())
	assert.True(t, fl.unicode, "v implies code-point mode")
}

// v-mode classes lower to alternations of characters, so they stay on RE2.
func TestRegExpUnicodeSetsSelection(t *testing.T) {
	for _, c := range []struct{ pattern, flags string }{
		{`[\p{L}--[a-z]]`, "v"}, {`[\q{abc|ab|a}]x`, "v"}, {`^\p{RGI_Emoji}$`, "v"},
		{`[\w&&\P{ASCII}]+`, "vi"}, {`[^\q{a}]`, "vi"}, {`[\q{ß}]`, "vi"},
	} {
		_, cr := compileForTest(t, c.pattern, c.flags)
		assert.True(t, cr.exact, "/%s/%s should run on RE2", c.pattern, c.flags)
	}
	deep := strings.Repeat("[", 3000) + strings.Repeat("]", 3000)
	_, err := parseRegExp(FromGoString(deep).UTF16(), regexpFlags{unicode: true, unicodeSets: true})
	assert.EqualError(t, err, "Regular expression too deeply nested")
}

package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Pattern modifiers `(?ims-ims:...)` and duplicate named groups (ES2025).
// Expected values are node's (V8), which agrees with the spec on these rows.
func TestRegExpModifiersAndDuplicateNames(t *testing.T) {
	for _, c := range regexpStretchCases {
		t.Run(c.js, func(t *testing.T) {
			assert.Equal(t, c.expected(), auditEvalJS(t, c.js), "%s", c.js)
		})
	}
}

var regexpStretchCases = []auditJSCase{
	{js: "/(?i:a)b/.test(\"Ab\")", want: "true"},
	{js: "/(?i:a)b/.test(\"AB\")", want: "false"},
	{js: "/a(?-i:b)/i.test(\"AB\")", want: "false"},
	{js: "/a(?-i:b)/i.test(\"Ab\")", want: "true"},
	{js: "/(?m:^b)/.test(\"a\\nb\")", want: "true"},
	{js: "/^b/.test(\"a\\nb\")", want: "false"},
	{js: "/(?-m:^b)/m.test(\"a\\nb\")", want: "false"},
	{js: "/(?s:.)/.test(\"\\n\")", want: "true"},
	{js: "/(?-s:.)/s.test(\"\\n\")", want: "false"},
	{js: "/(?i-s:a.)/s.test(\"A\\n\")", want: "false"},
	{js: "/(?i:[a-c]+)/.exec(\"xABCd\")[0]", want: "\"ABC\""},
	{js: "/(?i:\\w)/u.test(\"\\u017f\")", want: "true"},
	{js: "/(?i:\\b)/u.test(\"\\u017f\")", want: "true"},
	{js: "/(?i:(a))\\1/.test(\"AA\")", want: "true"},
	{js: "/(?i:(a))\\1/.test(\"aA\")", want: "false"},
	{js: "/(a)(?i:\\1)/.test(\"aA\")", want: "true"},
	{js: "/(?i:a(?-i:b)c)/.test(\"AbC\")", want: "true"},
	{js: "/(?i:a(?-i:b)c)/.test(\"ABC\")", want: "false"},
	{js: "/(?i-:a)/.test(\"A\")", want: "true"},
	{js: "/(?ims:^a.$)/.test(\"x\\nA\\n\")", want: "true"},
	{js: "/(?i:\\p{Lu})/u.test(\"a\")", want: "true"},
	{js: "/(?i:[\\q{AB}])/v.test(\"ab\")", want: "true"},
	{js: "/x(?i:K)/u.test(\"x\\u212a\")", want: "true"},
	{js: "/(?i:k)/.test(\"\\u212a\")", want: "false"},
	{js: "/(?i:k)/u.test(\"\\u212a\")", want: "true"},
	{js: "/(?<a>x)|(?<a>y)/.exec(\"y\").groups.a", want: "\"y\""},
	{js: "/(?<a>x)|(?<a>y)/.exec(\"x\").groups.a", want: "\"x\""},
	{js: "JSON.stringify(Object.keys(/(?<b>z)|(?<a>x)|(?<b>y)/.exec(\"y\").groups))", want: "\"[\\\"b\\\",\\\"a\\\"]\""},
	{js: "JSON.stringify(/(?<b>z)|(?<a>x)|(?<b>y)/.exec(\"y\").groups)", want: "\"{\\\"b\\\":\\\"y\\\"}\""},
	{js: "/(?:(?<a>x)|(?<a>y))\\k<a>/.test(\"yy\")", want: "true"},
	{js: "/(?:(?<a>x)|(?<a>y))\\k<a>/.test(\"xy\")", want: "false"},
	{js: "/(?:(?<a>x)|(?<a>y))\\k<a>/.test(\"xx\")", want: "true"},
	{js: "\"xy\".replace(/(?<a>x)|(?<a>y)/g, \"[$<a>]\")", want: "\"[x][y]\""},
	{js: "\"xy\".replace(/(?<a>x)|(?<a>y)/g, (...args) => \"(\" + args[args.length-1].a + \")\")", want: "\"(x)(y)\""},
	{js: "JSON.stringify(/(?<a>x)|(?<a>y)/d.exec(\"y\").indices.groups)", want: "\"{\\\"a\\\":[0,1]}\""},
	{js: "{ var re = /(?<a>x)|(?<a>y)/g, out = [], m; while ((m = re.exec(\"xyx\"))) out.push(m.groups.a); return JSON.stringify(out); }", want: "\"[\\\"x\\\",\\\"y\\\",\\\"x\\\"]\""},
	{js: "/(?:(?<a>x)|(?<a>y))+/.exec(\"xy\").groups.a", want: "\"y\""},
	{js: "/((?<a>x)|(?<a>y))(?:(?<b>1)|(?<b>2))/.exec(\"y2\").groups.b", want: "\"2\""},
	{js: "/(?<a>x)|(?:zz|(?<a>y))/.exec(\"y\").groups.a", want: "\"y\""},
	{js: "/(?<a>.)(?:(?<b>b)|(?<b>c))/.exec(\"ac\").groups.b", want: "\"c\""},
	{js: "JSON.stringify(/(?<a>x)|(?<a>y)/.exec(\"z\"))", want: "\"null\""},
	{js: "\"y\".replace(/(?<a>x)|(?<a>y)/, \"$<a>$<a>\")", want: "\"yy\""},
	{js: "/(?<a>a)|(?<a>b)/.source", want: "\"(?<a>a)|(?<a>b)\""},
}

// TestRegExpModifiersAndDuplicateNamesSyntax checks the early errors against
// node's messages ("" for a valid pattern): repeated and conflicting
// modifier flags, an empty `(?-:`, and a name repeated where both groups
// might participate (MightBothParticipate).
func TestRegExpModifiersAndDuplicateNamesSyntax(t *testing.T) {
	for _, c := range []struct{ pattern, err string }{
		{`(?ii:a)`, "Repeated flag in flag group"},
		{`(?-:a)`, "Invalid flag group"},
		{`(?i-i:a)`, "Repeated flag in flag group"},
		{`(?x:a)`, "Invalid group"},
		{`(?i`, "Invalid group"},
		{`(?i)`, "Invalid group"},
		{`(?ms-i`, "Invalid group"},
		{`(?-ii:a)`, "Repeated flag in flag group"},
		{`(?i-m-s:a)`, "Multiple dashes in flag group"},
		{`(?I:a)`, "Invalid group"},
		{`(?<a>x)(?<a>y)`, "Duplicate capture group name"},
		{`(?<a>x)|((?<a>y)(?<a>z))`, "Duplicate capture group name"},
		{`(?<a>(?<a>x)|y)`, "Duplicate capture group name"},
		{`(?:(?<a>x)|y)(?<a>z)`, "Duplicate capture group name"},
		{`(?<a>x)(?:(?<a>y)|z)`, "Duplicate capture group name"},
		{`(?i-:a)`, ""},
		{`(?-i:a)`, ""},
		{`(?ims:a)`, ""},
		{`(?ims-:a)`, ""},
		{`(?-ims:a)`, ""},
		{`(?i-ms:a)`, ""},
		{`(?<a>x)|(?<a>y)|(?<a>z)`, ""},
		{`(?:(?<a>x)|(?<a>y))(?:(?<b>x)|(?<b>y))`, ""},
		{`((?<a>x)|(?<a>y))|(?<a>z)`, ""},
	} {
		_, err := parseRegExp(FromGoString(c.pattern).UTF16(), regexpFlags{})
		if c.err == "" {
			assert.NoError(t, err, "%s", c.pattern)
		} else {
			assert.EqualError(t, err, c.err, "%s", c.pattern)
		}
	}
}

// Modified groups keep a pattern exact, so it stays on RE2; a duplicate name
// without a backreference does too.
func TestRegExpModifiersSelection(t *testing.T) {
	for _, c := range []struct{ pattern, flags string }{
		{`(?i:a)b`, ""}, {`a(?-i:b)`, "i"}, {`(?m:^b)`, ""}, {`(?s:.)x`, ""}, {`(?<a>x)|(?<a>y)`, ""},
	} {
		_, cr := compileForTest(t, c.pattern, c.flags)
		assert.True(t, cr.exact, "/%s/%s should run on RE2", c.pattern, c.flags)
	}
}

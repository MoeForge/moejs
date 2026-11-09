package engine

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegExpTranslationTable(t *testing.T) {
	cases := []struct {
		pattern string
		flags   string
		want    string // RE2 body, or "" when wantErr is set
		wantErr string
	}{
		{"abc", "", "abc", ""},
		{"a.b", "", `a[^\x{a}\x{d}\x{2028}\x{2029}]b`, ""},
		{"a.b", "s", `a[\x00-\x{10ffff}]b`, ""},
		{`.`, "u", `[^\x{a}\x{d}\x{2028}\x{2029}]`, ""},
		{`\d+\s*\w?`, "", `[0-9]+[\x{9}-\x{d}\x{20}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*[0-9A-Z_a-z]?`, ""},
		{`\D\S\W`, "", `[^0-9][^\x{9}-\x{d}\x{20}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}][^0-9A-Z_a-z]`, ""},
		// i: the Canonicalize closure is explicit, RE2 never folds.
		{`\w`, "i", `[0-9A-Z_a-z]`, ""},
		{`\w`, "iu", `[0-9A-Z_a-z\x{17f}\x{212a}]`, ""},
		{`\W`, "iu", `[^0-9A-Z_a-z\x{17f}\x{212a}]`, ""},
		{`k`, "i", `[Kk]`, ""},
		{`k`, "iu", `[Kk\x{212a}]`, ""},
		{`[^a-z]`, "i", `[^A-Za-z]`, ""},
		{`é`, "i", `[\x{c9}\x{e9}]`, ""},
		{`µ`, "i", `[\x{b5}\x{39c}\x{3bc}]`, ""},
		{`^a$`, "", `\Aa\z`, ""},
		{`^a$`, "m", `(?m:^)a(?m:$)`, ""},
		{`\bx\B`, "", `\bx\B`, ""},
		{`(a)(?:b)(?<name>c)`, "", `(a)b(c)`, ""},
		{`a|b|`, "", `a|b|`, ""},
		{`a*b+c?d*?e+?f??`, "", `a*b+c?d*?e+?f??`, ""},
		{`a{2}b{2,}c{2,4}d{2,4}?`, "", `a{2}b{2,}c{2,4}d{2,4}?`, ""},
		{`(ab)+(?:cd)*(?:e|f)?(?:(?:g)*)*`, "", `(ab)+(?:cd)*(?:e|f)?(?:g*)*`, ""},
		{`x(?:a|b)(c|d)`, "", `x(?:a|b)(c|d)`, ""},
		{`a{,2}`, "", `a\x{7b}\x{2c}2\x{7d}`, ""},
		{`a{`, "", `a\x{7b}`, ""},
		{`[abc]`, "", `[a-c]`, ""},
		{`[abd]`, "", `[abd]`, ""},
		{`[^a-z\d]`, "", `[^0-9a-z]`, ""},
		{`[\s\S]`, "", `[\x00-\x{10ffff}]`, ""},
		{`[]`, "", `[^\x00-\x{10ffff}]`, ""},
		{`[^]`, "", `[\x00-\x{10ffff}]`, ""},
		{`[\b]`, "", `\x{8}`, ""},
		{`[a-]`, "", `[\x{2d}a]`, ""},
		{`[\w-z]`, "", `[\x{2d}0-9A-Z_a-z]`, ""},
		// Without u every code unit is a character, so the BMP covers every
		// rune a working copy can hold.
		{`[\u0000-￿]`, "", `[\x00-\x{10ffff}]`, ""},
		{`[\uD800-\uDBFF]`, "", `[\x{f0000}-\x{f03ff}]`, ""},
		{`[^\uD800]`, "u", `[^\x{f0000}]`, ""},
		{`😀`, "", `\x{f003d}\x{f0600}`, ""},
		{`😀`, "u", `\x{1f600}`, ""},
		{`\uD83D`, "u", `\x{f003d}`, ""},
		{`\u{1F600}`, "u", `\x{1f600}`, ""},
		{`\u{1F600}`, "", `u\x{7b}1F600\x{7d}`, ""},
		{`\x41B\cA\t\n\r\v\f\0`, "", `AB\x{1}\x{9}\x{a}\x{d}\x{b}\x{c}\x{0}`, ""},
		{`\.\*\+\?\(\)\[\]\{\}\|\/\\\^\$\-`, "", `\x{2e}\x{2a}\x{2b}\x{3f}\x{28}\x{29}\x{5b}\x{5d}\x{7b}\x{7d}\x{7c}\x{2f}\x{5c}\x{5e}\x{24}\x{2d}`, ""},
		{`\q`, "", `q`, ""},
		{`\q`, "u", "", "Invalid escape"},
		{`\c`, "", `\x{5c}c`, ""},
		{`\101`, "", `A`, ""},
		{`\8`, "", `8`, ""},
		{`\1`, "u", "", "Invalid escape"},
		{`(a)\1`, "", "", "regular expression feature backreferences is not supported yet"},
		{`(?<n>a)\k<n>`, "", "", "regular expression feature named backreferences is not supported yet"},
		{`(?<n>a)\k<m>`, "", "", "Invalid named capture referenced"},
		{`\k<n>`, "u", "", "Invalid named capture referenced"},
		{`(?<n>a)\k`, "", "", "Invalid named reference"},
		{`\k`, "", `k`, ""},
		{`a(?=b)`, "", "", "regular expression feature lookahead is not supported yet"},
		{`a(?!b)`, "", "", "regular expression feature lookahead is not supported yet"},
		{`(?<=a)b`, "", "", "regular expression feature lookbehind is not supported yet"},
		{`(?<!a)b`, "", "", "regular expression feature lookbehind is not supported yet"},
		{`(?=a)*`, "u", "", "Nothing to repeat"},
		{`(?<=a)*`, "", "", "Nothing to repeat"},
		// Properties are explicit ranges from the Unicode tables.
		{`\p{Zs}`, "u", `[\x{20}\x{a0}\x{1680}\x{2000}-\x{200a}\x{202f}\x{205f}\x{3000}]`, ""},
		{`\p{gc=Space_Separator}`, "u", `[\x{20}\x{a0}\x{1680}\x{2000}-\x{200a}\x{202f}\x{205f}\x{3000}]`, ""},
		{`[\p{Zl}\d]`, "u", `[0-9\x{2028}]`, ""},
		{`\P{ASCII}`, "u", `[^\x{0}-\x{7f}]`, ""},
		{`\p{Any}`, "u", `[\x00-\x{10ffff}]`, ""},
		{`\P{Any}`, "u", `[^\x00-\x{10ffff}]`, ""},
		{`\p{sc=Hrkt}`, "u", `[^\x00-\x{10ffff}]`, ""},
		{`\p{L}`, "", `p\x{7b}L\x{7d}`, ""},
		{`\p{Nope}`, "u", "", "Invalid property name"},
		{`\p{Latin}`, "u", "", "Invalid property name"},
		{`\p{sc=Lu}`, "u", "", "Invalid property name"},
		{`\p{Lu=Lu}`, "u", "", "Invalid property name"},
		{`\p{ Lu}`, "u", "", "Invalid property name"},
		{`\p{Lu`, "u", "", "Invalid property name"},
		{`\p`, "u", "", "Invalid property name"},
		{`a{1001}`, "", "", "regular expression feature repeat count above 1000 is not supported yet"},
		{`(`, "", "", "Unterminated group"},
		{`)`, "", "", "Unmatched ')'"},
		{`a)`, "", "", "Unmatched ')'"},
		{`[a`, "", "", "Unterminated character class"},
		{`*a`, "", "", "Nothing to repeat"},
		{`a**`, "", "", "Nothing to repeat"},
		{`a{2}{3}`, "", "", "Nothing to repeat"},
		{`[z-a]`, "", "", "Range out of order in character class"},
		{`a{3,2}`, "", "", "numbers out of order in {} quantifier"},
		{`(?<n>a)(?<n>b)`, "", "", "Duplicate capture group name"},
		{`(?<1>a)`, "", "", "Invalid capture group name"},
		{`(?x)`, "", "", "Invalid group"},
		{`\`, "", "", `\ at end of pattern`},
		{`{`, "u", "", "Lone quantifier brackets"},
		{`}`, "u", "", "Lone quantifier brackets"},
		{`a{`, "u", "", "Incomplete quantifier"},
		{`[\d-z]`, "u", "", "Invalid character class"},
		{`你好`, "", `\x{4f60}\x{597d}`, ""},
		{`😀`, "", `\x{f003d}\x{f0600}`, ""},
		{`😀`, "u", `\x{1f600}`, ""},
		{`(?<$_ünïcödé>x)`, "", `(x)`, ""},
		{`(?<ab>x)\k<ab>`, "", "", "named backreferences"},
		{`(?<\u{1d4d0}>x)`, "", `(x)`, ""},
		{`(?<𝓐>x)`, "", `(x)`, ""},
		{"(?<a‌>x)", "", `(x)`, ""},
		{"(?<‌>x)", "", "", "Invalid capture group name"},
		{`(?<a-b>x)`, "", "", "Invalid capture group name"},
		{`(?<>x)`, "", "", "Invalid capture group name"},
	}
	for _, c := range cases {
		t.Run(c.pattern+"/"+c.flags, func(t *testing.T) {
			fl, ok := parseRegExpFlags(c.flags)
			require.True(t, ok)
			tr, err := translateRegExp(FromGoString(c.pattern).UTF16(), fl)
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, tr.body)
		})
	}
	tr, err := translateRegExp(FromGoString(`(a)(?<x>b)(?:c)(d)`).UTF16(), regexpFlags{})
	require.NoError(t, err)
	assert.Equal(t, []string{"", "", "x", ""}, tr.names)
	assert.True(t, tr.hasNames)
	for p, want := range map[string]bool{"a*": true, "a+": false, "": true, "a|": true, "(a?)(b*)": true, "(a?)b": false, "^": true, `\b`: true, "a{0,3}": true, "a{1}": false} {
		tr, err := translateRegExp(FromGoString(p).UTF16(), regexpFlags{})
		require.NoError(t, err, p)
		assert.Equal(t, want, tr.minLen0, "minLen0 of %q", p)
	}
}

func TestRegExpFlagsAndGetters(t *testing.T) {
	r := NewRealm()
	rx := newRegExp(t, r, "a/b\n", "gimsuyd")
	for name, want := range map[string]string{
		"source": `a\/b\n`, "flags": "dgimsuy", "global": "true", "ignoreCase": "true", "multiline": "true",
		"dotAll": "true", "unicode": "true", "sticky": "true", "hasIndices": "true", "lastIndex": "0",
	} {
		assert.Equal(t, want, propString(t, r, rx, name), name)
	}
	assert.Equal(t, `/a\/b\n/dgimsuy`, callMethod(t, r, rx, "toString").AsString().GoString())
	plain := newRegExp(t, r, "", "")
	assert.Equal(t, "(?:)", propString(t, r, plain, "source"))
	assert.Equal(t, "false", propString(t, r, plain, "global"))
	assert.Equal(t, "/(?:)/", callMethod(t, r, plain, "toString").AsString().GoString())
	// Prototype receivers.
	proto := ObjectValue(r.RegExpPrototype)
	assert.Equal(t, "(?:)", propString(t, r, proto, "source"))
	assert.Equal(t, "", propString(t, r, proto, "flags"))
	assert.Equal(t, "undefined", propString(t, r, proto, "global"))
	_, err := callMethodErr(r, proto, "exec", str("a"))
	assertErrorKind(t, err, KindTypeError, "requires that 'this' be a RegExp")
	// Invalid flags and patterns.
	for _, f := range []string{"gg", "x", "uv", "G"} {
		_, err := r.Construct(ObjectValue(r.RegExpCtor), []Value{str("a"), str(f)}, nil)
		assertErrorKind(t, err, KindSyntaxError, "Invalid flags supplied to RegExp constructor '"+f+"'")
	}
	_, err = r.Construct(ObjectValue(r.RegExpCtor), []Value{str("a(")}, nil)
	assertErrorKind(t, err, KindSyntaxError, "Invalid regular expression: /a(/: Unterminated group")
	backref, err := r.Construct(ObjectValue(r.RegExpCtor), []Value{str(`(a)\1`)}, nil)
	require.NoError(t, err, "backreferences run on the backtracking VM")
	m, err := r.RegExpExec(backref.AsObject(), FromGoString("baa"))
	require.NoError(t, err)
	assert.Equal(t, "1", propString(t, r, m, "index"))
	// Constructor forms: RegExp(re) returns re; new RegExp(re, flags) copies the source.
	same, err := r.Call(ObjectValue(r.RegExpCtor), Undefined(), []Value{rx})
	require.NoError(t, err)
	assert.Same(t, rx.AsObject(), same.AsObject())
	copied, err := r.Construct(ObjectValue(r.RegExpCtor), []Value{rx, str("g")}, nil)
	require.NoError(t, err)
	assert.NotSame(t, rx.AsObject(), copied.AsObject())
	assert.Equal(t, "g", propString(t, r, copied, "flags"))
	assert.Equal(t, `a\/b\n`, propString(t, r, copied, "source"))
	und, err := r.Construct(ObjectValue(r.RegExpCtor), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "/(?:)/", callMethod(t, r, und, "toString").AsString().GoString())
	assert.Same(t, r.RegExpPrototype, und.AsObject().Proto())
	assert.Equal(t, "RegExp", und.AsObject().ClassName())
	ots, _ := r.ObjectPrototype.GetProp(r, StringKey(AtomToString))
	tag, err := r.Call(ots, rx, nil)
	require.NoError(t, err)
	assert.Equal(t, "[object RegExp]", tag.AsString().GoString())
	// lastIndex is an own writable, non-enumerable, non-configurable property.
	d, ok := rx.AsObject().GetOwnProperty(StringKey(AtomLastIndex))
	require.True(t, ok)
	assert.True(t, d.Writable())
	assert.False(t, d.Enumerable())
	assert.False(t, d.Configurable())
	assert.Equal(t, []string{"lastIndex"}, keyNames(rx.AsObject().OwnPropertyKeys()))
}

func TestRegExpExecTable(t *testing.T) {
	r := NewRealm()
	cases := []struct {
		name     string
		pattern  string
		flags    string
		input    string
		lastIdx  int
		want     []any // nil for null
		index    string
		lastNext string
	}{
		{"simple", `b+`, "", "abbbc", 0, []any{"bbb"}, "1", "0"},
		{"captures", `(a)(x)?(b)`, "", "zab", 0, []any{"ab", "a", nil, "b"}, "1", "0"},
		{"no match", `x`, "", "abc", 0, nil, "", "0"},
		{"non-global ignores lastIndex", `a`, "", "aa", 1, []any{"a"}, "0", "1"},
		{"global from lastIndex", `a`, "g", "aXa", 1, []any{"a"}, "2", "3"},
		{"global past end resets", `a`, "g", "aa", 3, nil, "", "0"},
		{"global no match resets", `z`, "g", "aa", 0, nil, "", "0"},
		{"sticky hit", `a`, "y", "ba", 1, []any{"a"}, "1", "2"},
		{"sticky miss", `a`, "y", "ba", 0, nil, "", "0"},
		{"sticky anchored", `^a`, "y", "ba", 1, nil, "", "0"},
		{"sticky anchored multiline", `^a`, "my", "b\na", 2, []any{"a"}, "2", "3"},
		{"caret at lastIndex not start", `^a`, "g", "ba", 1, nil, "", "0"},
		{"word boundary context", `\bb`, "g", "ab b", 1, []any{"b"}, "3", "4"},
		{"non-boundary context", `\Bb`, "g", "ab b", 1, []any{"b"}, "1", "2"},
		{"dollar", `a$`, "", "ba", 0, []any{"a"}, "1", "0"},
		{"multiline dollar", `a$`, "m", "a\nb", 0, []any{"a"}, "0", "0"},
		{"case insensitive", `HELLO`, "i", "say hello", 0, []any{"hello"}, "4", "0"},
		{"case insensitive unicode", `é`, "i", "CAFÉ", 0, []any{"É"}, "3", "0"},
		{"unicode subject", `世界`, "", "你好世界", 0, []any{"世界"}, "2", "0"},
		{"unicode subject index", `\d+`, "g", "你1好22", 2, []any{"22"}, "3", "5"},
		{"emoji dot u", `^.$`, "u", "😀", 0, []any{"😀"}, "0", "0"},
		{"emoji dot no u", `^.$`, "", "😀", 0, nil, "", "0"},
		{"emoji two dots no u", `^..$`, "", "😀", 0, []any{"😀"}, "0", "0"},
		{"lone surrogate dot", `^.$`, "", "\xed\xa0\xbd", 0, []any{"\ufffd"}, "0", "0"},
		{"dot excludes 2028", `a.b`, "", "a\u2028b", 0, nil, "", "0"},
		{"dotAll includes 2028", `a.b`, "s", "a\u2028b", 0, []any{"a\u2028b"}, "0", "0"},
		{"space class nbsp", `\s`, "", "a\u00a0", 0, []any{"\u00a0"}, "1", "0"},
		{"space class excludes zwsp", `\s`, "", "\u200b", 0, nil, "", "0"},
		{"digit ascii only", `\d`, "u", "٣", 0, nil, "", "0"},
		{"word ascii only", `\w`, "u", "é", 0, nil, "", "0"},
		{"word ignoreCase kelvin", `\w`, "i", "K", 0, nil, "", "0"},
		{"class range surrogate", `[\uD800-\uDFFF]`, "", "a😀", 0, []any{"\ufffd"}, "1", "0"},
		{"class negated surrogate", `[^a]`, "", "a😀", 0, []any{"\ufffd"}, "1", "0"},
		{"class negated surrogate u", `[^a]`, "u", "a😀", 0, []any{"😀"}, "1", "0"},
		{"lazy", `a+?`, "", "aaa", 0, []any{"a"}, "0", "0"},
		{"alternation order", `a|ab`, "", "ab", 0, []any{"a"}, "0", "0"},
		{"empty pattern", ``, "g", "ab", 1, []any{""}, "1", "1"},
		{"escaped slash", `a\/b`, "", "a/b", 0, []any{"a/b"}, "0", "0"},
		{"property escape", `\p{Lu}+`, "u", "abcDEF", 0, []any{"DEF"}, "3", "0"},
		{"octal", `\101`, "", "A", 0, []any{"A"}, "0", "0"},
		{"control", `\cJ`, "", "a\nb", 0, []any{"\n"}, "1", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rx := newRegExp(t, r, c.pattern, c.flags)
			require.NoError(t, rx.AsObject().SetProp(r, StringKey(AtomLastIndex), IntValue(c.lastIdx)))
			var input Value
			if c.name == "lone surrogate dot" {
				input = utf16Str(0xD83D)
			} else {
				input = str(c.input)
			}
			got := callMethod(t, r, rx, "exec", input)
			if c.want == nil {
				assert.True(t, got.IsNull(), "got %v", got)
			} else {
				require.True(t, got.IsObject(), "got %v", got)
				assert.Equal(t, c.want, jsArrayB(r, got))
				assert.Equal(t, c.index, propString(t, r, got, "index"))
				assert.True(t, StrictEquals(input, mustProp(t, r, got, "input")))
			}
			assert.Equal(t, c.lastNext, propString(t, r, rx, "lastIndex"))
			// test() agrees with exec().
			require.NoError(t, rx.AsObject().SetProp(r, StringKey(AtomLastIndex), IntValue(c.lastIdx)))
			assert.Equal(t, c.want != nil, callMethod(t, r, rx, "test", input).AsBool())
		})
	}
}

func mustProp(t *testing.T, r *Realm, o Value, name string) Value {
	t.Helper()
	v, err := r.GetV(o, r.KeyFromGoString(name))
	require.NoError(t, err)
	return v
}

func TestRegExpExecResultShape(t *testing.T) {
	r := NewRealm()
	rx := newRegExp(t, r, `(?<year>\d{4})-(?<month>\d{2})(-(?<day>\d{2}))?`, "d")
	got := callMethod(t, r, rx, "exec", str("on 2024-09 ok"))
	require.True(t, got.IsObject())
	res := got.AsObject()
	assert.Equal(t, []string{"0", "1", "2", "3", "4", "length", "index", "input", "groups", "indices"}, keyNames(res.OwnPropertyKeys()))
	assert.Equal(t, []any{"2024-09", "2024", "09", nil, nil}, jsArrayB(r, got))
	groups := mustProp(t, r, got, "groups")
	require.True(t, groups.IsObject())
	assert.Nil(t, groups.AsObject().Proto(), "groups has a null prototype")
	assert.Equal(t, map[string]any{"year": "2024", "month": "09", "day": nil}, r.ToGo(groups))
	assert.Equal(t, []string{"year", "month", "day"}, keyNames(groups.AsObject().OwnPropertyKeys()))
	indices := mustProp(t, r, got, "indices")
	assert.Equal(t, []any{[]any{int64(3), int64(10)}, []any{int64(3), int64(7)}, []any{int64(8), int64(10)}, nil, nil}, jsArrayB(r, indices))
	ig := mustProp(t, r, indices, "groups")
	assert.Equal(t, map[string]any{"year": []any{int64(3), int64(7)}, "month": []any{int64(8), int64(10)}, "day": nil}, r.ToGo(ig))
	// Without names, groups is undefined; without d there is no indices key.
	got = callMethod(t, r, newRegExp(t, r, `(a)`, ""), "exec", str("a"))
	assert.True(t, mustProp(t, r, got, "groups").IsUndefined())
	assert.False(t, got.AsObject().HasOwnProperty(StringKey(AtomIndices)))
	// Result arrays are ordinary mutable arrays.
	require.True(t, got.AsObject().Push(r, IntValue(1)))
	assert.Equal(t, uint32(3), got.AsObject().ArrayLength())
	// Two exec results share one shape.
	a := callMethod(t, r, newRegExp(t, r, `a`, ""), "exec", str("a"))
	b := callMethod(t, r, newRegExp(t, r, `b`, ""), "exec", str("b"))
	assert.Same(t, a.AsObject().Shape(), b.AsObject().Shape())
	// Two RegExp instances share one shape.
	assert.Same(t, newRegExp(t, r, "x", "").AsObject().Shape(), newRegExp(t, r, "y", "g").AsObject().Shape())
}

func TestRegExpLastIndexCoercion(t *testing.T) {
	r := NewRealm()
	rx := newRegExp(t, r, `a`, "g")
	require.NoError(t, rx.AsObject().SetProp(r, StringKey(AtomLastIndex), str("1")))
	got := callMethod(t, r, rx, "exec", str("aa"))
	assert.Equal(t, "1", propString(t, r, got, "index"))
	require.NoError(t, rx.AsObject().SetProp(r, StringKey(AtomLastIndex), IntValue(-5)))
	got = callMethod(t, r, rx, "exec", str("aa"))
	assert.Equal(t, "0", propString(t, r, got, "index"))
	require.NoError(t, rx.AsObject().SetProp(r, StringKey(AtomLastIndex), NumberValue(1e300)))
	got = callMethod(t, r, rx, "exec", str("aa"))
	assert.True(t, got.IsNull())
	assert.Equal(t, "0", propString(t, r, rx, "lastIndex"))
	// Frozen lastIndex on a global regexp throws when it must be written.
	rx.AsObject().Freeze(r)
	_, err := callMethodErr(r, rx, "exec", str("aa"))
	assertErrorKind(t, err, KindTypeError, "read only")
	// Non-global regexp never writes lastIndex.
	ng := newRegExp(t, r, `a`, "")
	ng.AsObject().Freeze(r)
	assert.True(t, callMethod(t, r, ng, "test", str("a")).AsBool())
}

func TestRegExpGlobalMatchLoop(t *testing.T) {
	r := NewRealm()
	rx := newRegExp(t, r, `\d+`, "g")
	var found []string
	for {
		m := callMethod(t, r, rx, "exec", str("a1b22c333"))
		if m.IsNull() {
			break
		}
		found = append(found, jsArrayB(r, m)[0].(string))
	}
	assert.Equal(t, []string{"1", "22", "333"}, found)
	assert.Equal(t, "0", propString(t, r, rx, "lastIndex"))
	// Unicode subject positions are code units.
	rx = newRegExp(t, r, `o`, "g")
	callMethod(t, r, rx, "exec", str("😀o😀o"))
	assert.Equal(t, "3", propString(t, r, rx, "lastIndex"))
	m := callMethod(t, r, rx, "exec", str("😀o😀o"))
	assert.Equal(t, "5", propString(t, r, m, "index"))
}

func TestRegExpCache(t *testing.T) {
	r := NewRealm()
	a := newRegExp(t, r, `\s+`, "g").AsObject().RegExpData()
	b := newRegExp(t, r, `\s+`, "g").AsObject().RegExpData()
	c := newRegExp(t, r, `\s+`, "gi").AsObject().RegExpData()
	assert.Same(t, a.c, b.c, "same (pattern, flags) shares the compiled program")
	assert.NotSame(t, a.c, c.c)
	assert.Equal(t, 2, len(r.regexps.cache))
	for i := range regexpCacheLimit + 5 {
		newRegExp(t, r, "x"+string(rune('a'+i%26))+NumberToGoString(float64(i)), "")
	}
	assert.LessOrEqual(t, len(r.regexps.cache), regexpCacheLimit, "cache is bounded")
	// Realms keep independent front caches but share the immutable compiled
	// program process-wide (regexpPrograms); shared realms too.
	r2 := newShared()
	d := newRegExp(t, r2, `\s+`, "g").AsObject().RegExpData()
	assert.Same(t, a.c, d.c)
	assert.NotContains(t, r2.regexps.cache, regexpCacheKey{pattern: "xa0"})
	assert.Nil(t, r.host)
	// Concurrent realms compiling and matching the same pattern (with the
	// lazily built anchored/context variants) agree on results (run with -race).
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			rr := newShared()
			rx := newRegExp(t, rr, `(\d+)-(\d+)`, "g")
			v, err := rr.RegExpExec(rx.AsObject(), FromGoString("a 12-34 b 5-6"))
			if err != nil || !v.IsObject() {
				t.Error("first exec failed")
				return
			}
			v, err = rr.RegExpExec(rx.AsObject(), FromGoString("a 12-34 b 5-6"))
			if err != nil || !v.IsObject() {
				t.Error("second exec failed")
				return
			}
			if got, _ := v.AsObject().GetIndex(rr, 0); got.AsString().GoString() != "5-6" {
				t.Errorf("second match %s", got.AsString().GoString())
			}
		})
	}
	wg.Wait()
}

func TestRegExpDirectEntryPoints(t *testing.T) {
	r := NewRealm()
	rx, err := r.NewRegExp(FromGoString(`(\w+)@`), FromGoString("g"))
	require.NoError(t, err)
	res, err := r.RegExpExec(rx, FromGoString("mail bob@x"))
	require.NoError(t, err)
	assert.Equal(t, []any{"bob@", "bob"}, jsArrayB(r, res))
	assert.Equal(t, "9", propString(t, r, ObjectValue(rx), "lastIndex"))
	res, err = r.RegExpExec(rx, FromGoString("mail bob@x"))
	require.NoError(t, err)
	assert.True(t, res.IsNull())
	_, err = r.NewRegExp(FromGoString("("), FromGoString(""))
	assertErrorKind(t, err, KindSyntaxError, "Unterminated group")
	_, err = r.RegExpExec(r.NewObject(), FromGoString("x"))
	assertErrorKind(t, err, KindTypeError, "requires a RegExp")
	assert.Equal(t, `(\w+)@`, rx.RegExpData().Source().GoString())
	assert.Equal(t, "g", rx.RegExpData().Flags().GoString())
}

func TestRegExpInterrupt(t *testing.T) {
	r := NewRealm()
	rx := newRegExp(t, r, ``, "g")
	r.Interrupt("stop")
	_, err := callMethodErr(r, str(string(make([]byte, 5000))), "replace", rx, str("x"))
	var ie *InterruptedError
	require.ErrorAs(t, err, &ie)
	r.ClearInterrupt()
}

// Patterns differing only in lone surrogates compile separately: the cache
// key keeps the code units.
func TestRegExpCacheKeyKeepsSurrogates(t *testing.T) {
	got := auditEvalJS(t, `[new RegExp("\ud800").test("\ud800"), new RegExp("\ud801").test("\ud800"), new RegExp("�").test("\ud800"), new RegExp("�").test("�")].join()`)
	assert.Equal(t, `"true,false,false,true"`, got)
	for _, p := range []string{"a+", "ÿ", "￿ÿ", "é\U0001F600"} {
		s := FromGoString(p)
		assert.Equal(t, s.UTF16(), regexpKey(s, regexpFlags{}).units(), p)
	}
	// Every realm that evaluates a regexp allocates a map of these keys.
	if is64 { // a 64-bit layout
		assert.Equal(t, uintptr(24), unsafe.Sizeof(regexpCacheKey{}))
	}
}

// The RegExp.prototype accessors, lastIndex and EscapeRegExpPattern. The
// expected values are node's (V8) unless a comment says the spec differs.
func TestRegExpAccessors(t *testing.T) {
	for _, c := range regexpAccessorCases {
		t.Run(c.js, func(t *testing.T) {
			assert.Equal(t, c.expected(), auditEvalJS(t, c.js), "%s", c.js)
		})
	}
}

// EscapeRegExpPattern round trip: "/" + source + "/" + flags parses back to
// a literal with the same source.
func TestRegExpSourceRoundTrip(t *testing.T) {
	for _, c := range []struct{ pattern, flags string }{
		{"a/b\n[/]", ""}, {"/", "u"}, {"[\\]/]/", ""}, {"\\/\u2028", ""}, {"\\\n", ""},
		{"a/b[\\/]", "v"}, {"[[a]\\/]/", "v"}, {"\r\n", "v"}, {"[\n]", "v"},
	} {
		lit, err := json.Marshal(c.pattern)
		require.NoError(t, err)
		src := auditEvalJS(t, "new RegExp("+string(lit)+", \""+c.flags+"\").source")
		var source string
		require.NoError(t, json.Unmarshal([]byte(src), &source), src)
		assert.False(t, strings.ContainsAny(source, "\n\r\u2028\u2029"), "raw line terminator in %q", source)
		assert.Equal(t, src, auditEvalJS(t, "/"+source+"/"+c.flags+".source"), "/%s/%s", source, c.flags)
	}
}

var regexpAccessorCases = []auditJSCase{
	{js: "{ var re = /a/g; Object.defineProperty(re, \"global\", {value: false}); return re.flags; }", want: "\"\""},
	{js: "{ var re = /a/; Object.defineProperty(re, \"sticky\", {get: function () { return 1; }}); return [re.flags, re.sticky, re.test(\"ba\")]; }", want: "[\"y\",1,true]"},
	// Spec order (22.2.6.4): unicodeSets before sticky; V8 reads sticky first.
	{js: "{ var log = []; var o = {}; [\"hasIndices\", \"global\", \"ignoreCase\", \"multiline\", \"dotAll\", \"unicode\", \"unicodeSets\", \"sticky\"].forEach(function (k) { Object.defineProperty(o, k, {get: function () { log.push(k); return k !== \"global\"; }}); }); var f = Object.getOwnPropertyDescriptor(RegExp.prototype, \"flags\").get.call(o); return f + \" \" + log.join(); }", want: "\"dimsuvy hasIndices,global,ignoreCase,multiline,dotAll,unicode,unicodeSets,sticky\""},
	{js: "{ try { Object.getOwnPropertyDescriptor(RegExp.prototype, \"flags\").get.call(1); } catch (e) { return e.name; } }", want: "\"TypeError\""},
	{js: "Object.getOwnPropertyDescriptor(RegExp.prototype, \"flags\").get.call(RegExp.prototype)", want: "\"\""},
	{js: "Object.getOwnPropertyDescriptor(RegExp.prototype, \"flags\").get.call({})", want: "\"\""},
	{js: "Object.getOwnPropertyDescriptor(RegExp.prototype, \"flags\").get.call({global: 1, sticky: \"\", unicode: [], dotAll: 0})", want: "\"gu\""},
	{js: "{ var d = Object.getOwnPropertyDescriptor(RegExp.prototype, \"flags\"); return [typeof d.get, d.set, d.enumerable, d.configurable, d.get.name, d.get.length]; }", want: "[\"function\",undefined,false,true,\"get flags\",0]"},
	{js: "[\"source\", \"flags\", \"global\", \"ignoreCase\", \"multiline\", \"dotAll\", \"unicode\", \"unicodeSets\", \"sticky\", \"hasIndices\"].map(function (k) { var d = Object.getOwnPropertyDescriptor(RegExp.prototype, k); return d.get.name + \":\" + (d.set === undefined) + d.enumerable + d.configurable; }).join()", want: "\"get source:truefalsetrue,get flags:truefalsetrue,get global:truefalsetrue,get ignoreCase:truefalsetrue,get multiline:truefalsetrue,get dotAll:truefalsetrue,get unicode:truefalsetrue,get unicodeSets:truefalsetrue,get sticky:truefalsetrue,get hasIndices:truefalsetrue\""},
	{js: "[\"global\", \"ignoreCase\", \"multiline\", \"dotAll\", \"unicode\", \"unicodeSets\", \"sticky\", \"hasIndices\"].map(function (k) { return Object.getOwnPropertyDescriptor(RegExp.prototype, k).get.call(/a/dgimsvy); }).join()", want: "\"true,true,true,true,false,true,true,true\""},
	{js: "[\"global\", \"ignoreCase\", \"multiline\", \"dotAll\", \"unicode\", \"unicodeSets\", \"sticky\", \"hasIndices\"].map(function (k) { return Object.getOwnPropertyDescriptor(RegExp.prototype, k).get.call(RegExp.prototype); }).join()", want: "\",,,,,,,\""},
	{js: "{ try { Object.getOwnPropertyDescriptor(RegExp.prototype, \"global\").get.call({}); } catch (e) { return e.name; } }", want: "\"TypeError\""},
	{js: "{ try { Object.getOwnPropertyDescriptor(RegExp.prototype, \"unicodeSets\").get.call(\"a\"); } catch (e) { return e.name; } }", want: "\"TypeError\""},
	{js: "{ try { Object.getOwnPropertyDescriptor(RegExp.prototype, \"source\").get.call({}); } catch (e) { return e.name; } }", want: "\"TypeError\""},
	{js: "{ try { Object.getOwnPropertyDescriptor(RegExp.prototype, \"source\").get.call(undefined); } catch (e) { return e.name; } }", want: "\"TypeError\""},
	{js: "Object.getOwnPropertyDescriptor(RegExp.prototype, \"source\").get.call(RegExp.prototype)", want: "\"(?:)\""},
	{js: "[/a/u.unicode, /a/u.unicodeSets, /a/v.unicode, /a/v.unicodeSets, /a/.unicodeSets, /a/v.flags, /a/dv.flags]", want: "[true,false,false,true,false,\"v\",\"dv\"]"},
	{js: "Object.getOwnPropertyNames(/a/g)", want: "[\"lastIndex\"]"},
	{js: "Object.getOwnPropertyDescriptor(/a/g, \"lastIndex\")", want: "{value:0,writable:true,enumerable:false,configurable:false}"},
	{js: "{ var re = /a/g; re.lastIndex = 5; return [re.hasOwnProperty(\"lastIndex\"), re.lastIndex, Object.keys(re).length]; }", want: "[true,5,0]"},
	{js: "[Object.hasOwn(/a/, \"source\"), Object.hasOwn(/a/, \"flags\"), Object.hasOwn(/a/, \"global\"), Object.hasOwn(/a/, \"unicodeSets\")]", want: "[false,false,false,false]"},
	{js: "[new RegExp(\"/\").source, new RegExp(\"[/]\").source, new RegExp(\"a/b[/]c/\").source, new RegExp(\"\\\\/\").source]", want: "[\"\\\\/\",\"[/]\",\"a\\\\/b[/]c\\\\/\",\"\\\\/\"]"},
	{js: "[new RegExp(\"\\n\").source, new RegExp(\"\\r\").source, new RegExp(\"\\u2028\\u2029\").source, new RegExp(\"\\\\\\n\").source]", want: "[\"\\\\n\",\"\\\\r\",\"\\\\u2028\\\\u2029\",\"\\\\n\"]"},
	{js: "[new RegExp(\"\").source, RegExp(\"(?:)\").source, new RegExp(\"[\\\\]/]\").source, new RegExp(\"\\\\[/\").source]", want: "[\"(?:)\",\"(?:)\",\"[\\\\]/]\",\"\\\\[\\\\/\"]"},
	{js: "[String(new RegExp(\"a/b\", \"gv\")), String(new RegExp(\"[\\\\q{a|bc}]\", \"v\")), new RegExp(\"[\\\\/]\", \"v\").source, new RegExp(\"[[a]\\\\/]\", \"v\").source]", want: "[\"/a\\\\/b/gv\",\"/[\\\\q{a|bc}]/v\",\"[\\\\/]\",\"[[a]\\\\/]\"]"},
	{js: "RegExp.prototype.toString.call({source: \"x\", flags: \"yz\"})", want: "\"/x/yz\""},
	{js: "{ var log = []; var o = {get source() { log.push(\"source\"); return \"s\"; }, get flags() { log.push(\"flags\"); return \"f\"; }}; return RegExp.prototype.toString.call(o) + \" \" + log.join(); }", want: "\"/s/f source,flags\""},
	{js: "{ var re = /a/gi; Object.defineProperty(re, \"flags\", {value: \"X\"}); return [String(re), re.global]; }", want: "[\"/a/X\",true]"},
	{js: "{ var re = /a/gi; Object.defineProperty(re, \"source\", {value: \"Y\"}); return [String(re), re.test(\"A\")]; }", want: "[\"/Y/gi\",true]"},
	// @@replace reads the flags through Get (ES2025).
	{js: "{ var re = /a/g; Object.defineProperty(re, \"global\", {value: false}); return [String(re), \"aa\".replace(re, \"b\")]; }", want: "[\"/a/\",\"ba\"]"},
	{js: "new RegExp(/a/gi).flags", want: "\"gi\""},
	{js: "new RegExp(/a/gi, \"m\").flags", want: "\"m\""},
	{js: "{ var re = /a/gi; Object.defineProperty(re, \"flags\", {value: \"y\"}); return new RegExp(re).flags; }", want: "\"gi\""},
}
